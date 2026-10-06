package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// BlobVersion is one post-image blob seen in history.
type BlobVersion struct {
	Path string
	OID  string
}

// History reports every blob that appeared at a path under prefix in any
// commit reachable from rev, calling fn once per (path, oid) occurrence
// (duplicates allowed; the caller deduplicates).
//
// It runs a single
//
//	git log --full-history -m --root --raw --no-renames --no-abbrev -z
//	    --format=%x00%H <rev> -- <prefix>
//
// and takes the post-image side of every raw entry that is not a deletion.
// Together with -m (diffs of merges against each parent) and --root this
// yields every blob of every tree, including evil merges. Mode 160000
// (submodules) and 120000 (symlinks) entries are skipped.
//
// The pathspec is ":(top)<prefix>" without globbing, so prefix is a literal
// path from the repository root even when Dir is a subdirectory. Config that
// would change which merge diffs log prints or how it names paths
// (log.diffMerges, log.follow, diff.relative) is pinned. The output is
// streamed; an error from fn stops git and is returned as is.
func (g *Git) History(ctx context.Context, rev, prefix string, fn func(BlobVersion) error) error {
	if err := checkRev(rev); err != nil {
		return err
	}
	prefix = cleanPrefix(prefix)
	args := historyArgs(rev, prefix)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.command(runCtx, args)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git log: %w", err)
	}
	stderr := newTailBuffer(stderrLimit)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git log: start: %w", err)
	}

	perr := parseHistory(bufio.NewReaderSize(stdout, 64<<10), prefix, fn)
	if perr != nil {
		cancel() // git's exit status no longer matters
		_ = cmd.Wait()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("git log: %w", ctxErr)
		}
		return perr
	}
	if err := cmd.Wait(); err != nil {
		return commandError(ctx, args, err, stderr)
	}
	return nil
}

// historyArgs returns the git arguments History runs.
func historyArgs(rev, prefix string) []string {
	args := []string{
		"-c", "log.diffMerges=separate",
		"-c", "log.follow=false",
		"-c", "diff.relative=false",
		"--noglob-pathspecs",
		"log", "--full-history", "-m", "--root", "--raw", "--no-renames", "--no-abbrev", "-z",
		"--format=%x00%H", "--no-color", "--no-ext-diff", "--no-textconv",
		rev, "--",
	}
	if prefix != "" {
		args = append(args, ":(top)"+prefix)
	}
	return args
}

// parseHistory reads the -z raw output of History's git log from r and calls
// fn for every post-image regular blob under prefix.
//
// The stream is a sequence of NUL-terminated tokens: commit headers
// ("<oid>", possibly surrounded by newlines), raw entry headers
// (":<old mode> <new mode> <old oid> <new oid> <status>", possibly after a
// newline) each followed by one path token (two for renames and copies), and
// empty tokens between commits.
func parseHistory(r *bufio.Reader, prefix string, fn func(BlobVersion) error) error {
	p := historyParser{prefix: prefix, fn: fn}
	for {
		tok, err := r.ReadString(0)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("git log: read output: %w", err)
		}
		eof := err != nil
		if eof && p.pending > 0 {
			break // a path without its terminating NUL is truncated
		}
		tok = strings.TrimSuffix(tok, "\x00")
		if tok != "" || p.pending > 0 {
			if perr := p.token(tok); perr != nil {
				return perr
			}
		}
		if eof {
			break
		}
	}
	if p.pending > 0 {
		return fmt.Errorf("git log: output ends inside the raw entry of commit %s", p.commit)
	}
	return nil
}

// historyParser is the state of parseHistory between tokens.
type historyParser struct {
	prefix string
	fn     func(BlobVersion) error
	commit string   // commit being read, for error messages
	entry  rawEntry // last raw entry header
	// pending is the number of path tokens entry still expects.
	pending int
}

// rawEntry is the part of a raw diff header History needs.
type rawEntry struct {
	newMode string
	newOID  string
	status  byte
}

func (p *historyParser) token(tok string) error {
	if p.pending > 0 {
		return p.path(tok)
	}
	for {
		tok = strings.TrimLeft(tok, "\n")
		if tok == "" {
			return nil
		}
		if tok[0] == ':' {
			return p.header(tok)
		}
		line, rest, _ := strings.Cut(tok, "\n")
		if !isOID(line) {
			return fmt.Errorf("git log: unexpected output %q after commit %s", abbrev(tok), p.commit)
		}
		p.commit = line
		tok = rest
	}
}

// header parses ":<old mode> <new mode> <old oid> <new oid> <status>".
func (p *historyParser) header(h string) error {
	f := strings.Split(h[1:], " ")
	if strings.HasPrefix(h, "::") || len(f) != 5 || !isOID(f[2]) || !isOID(f[3]) || f[4] == "" {
		return fmt.Errorf("git log: unexpected raw entry %q in commit %s", abbrev(h), p.commit)
	}
	p.entry = rawEntry{newMode: f[1], newOID: f[3], status: f[4][0]}
	p.pending = 1
	if p.entry.status == 'R' || p.entry.status == 'C' {
		p.pending = 2 // source path, then destination path
	}
	return nil
}

// path consumes one path token of the current raw entry and reports the
// post-image once the entry is complete.
func (p *historyParser) path(path string) error {
	if path == "" {
		return fmt.Errorf("git log: empty path in commit %s", p.commit)
	}
	p.pending--
	if p.pending > 0 {
		return nil
	}
	e := p.entry
	if e.status == 'D' || !isRegularMode(e.newMode) {
		return nil
	}
	if p.prefix != "" && !under(path, p.prefix) {
		return nil
	}
	return p.fn(BlobVersion{Path: path, OID: e.newOID})
}

// isRegularMode reports whether a tree entry mode is a regular file.
// 100664 is a legacy mode old git could write.
func isRegularMode(mode string) bool {
	switch mode {
	case "100644", "100755", "100664":
		return true
	}
	return false
}

// abbrev shortens s for error messages.
func abbrev(s string) string {
	const max = 80
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
