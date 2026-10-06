package gitx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// IndexEntry is one line of `git ls-files -s -z`.
type IndexEntry struct {
	Mode  string // "100644", "100755", "120000", "160000"
	OID   string
	Stage int // 0, or 1 to 3 for a path in conflict
	Path  string
}

// IndexEntries lists the index of the work tree containing Dir, with paths
// relative to Dir.
func (g *Git) IndexEntries(ctx context.Context) ([]IndexEntry, error) {
	out, err := g.Run(ctx, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	return parseIndex(out)
}

// parseIndex parses NUL-terminated `ls-files -s` records
// "<mode> SP <oid> SP <stage> TAB <path>".
func parseIndex(out []byte) ([]IndexEntry, error) {
	var entries []IndexEntry
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Split(meta, " ")
		if !ok || path == "" || len(f) != 3 || !isOID(f[1]) || len(f[2]) != 1 || f[2][0] < '0' || f[2][0] > '3' {
			return nil, fmt.Errorf("git ls-files -s: unexpected record %q", abbrev(rec))
		}
		entries = append(entries, IndexEntry{Mode: f[0], OID: f[1], Stage: int(f[2][0] - '0'), Path: path})
	}
	return entries, nil
}

// ConfigBool returns the boolean config value key as git reads it in Dir,
// or def when it is not set.
func (g *Git) ConfigBool(ctx context.Context, key string, def bool) (bool, error) {
	out, err := g.Run(ctx, nil, "config", "--type=bool", "--get", key)
	var gerr *Error
	if errors.As(err, &gerr) && gerr.Code == 1 {
		return def, nil // not set
	}
	if err != nil {
		return false, err
	}
	switch s := trimEOL(out); s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("git config %s: unexpected output %q", key, s)
	}
}

// InsideGitDir reports whether Dir is inside a repository's git directory
// (.git or a bare repository), where there is no work tree to write to.
// Outside any repository it returns false.
func (g *Git) InsideGitDir(ctx context.Context) (bool, error) {
	out, err := g.Run(ctx, nil, "rev-parse", "--is-inside-git-dir", "--is-bare-repository")
	if err != nil {
		var e *Error
		if errors.As(err, &e) && strings.Contains(e.Stderr, "not a git repository (or any") {
			return false, nil
		}
		return false, err
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSuffix(line, "\r") == "true" {
			return true, nil
		}
	}
	return false, nil
}

// IsPartialClone reports whether the repository is a partial clone
// (`clone --filter`): objects missing locally are fetched on demand, one
// round trip at a time.
func (g *Git) IsPartialClone(ctx context.Context) (bool, error) {
	out, err := g.Run(ctx, nil, "config", "-z", "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor)$`)
	var gerr *Error
	if errors.As(err, &gerr) && gerr.Code == 1 {
		return false, nil // none of the keys is set
	}
	if err != nil {
		return false, err
	}
	for rec := range strings.SplitSeq(string(out), "\x00") {
		key, value, _ := strings.Cut(rec, "\n")
		switch {
		case key == "":
		case strings.EqualFold(key, "extensions.partialclone"):
			if value != "" {
				return true, nil
			}
		case isTrue(value):
			return true, nil
		}
	}
	return false, nil
}

// isTrue reports whether a raw config value is one of git's spellings of
// true. A key without a value is true.
func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "", "true", "yes", "on", "1":
		return true
	}
	return false
}

// SetExecutable records mode 100755 for the work tree file p (relative to
// Dir) in the index, adding the file when it is not tracked yet:
// `git update-index --add --chmod=+x -- p`. Where core.fileMode is false
// (Windows), the index is the only place git keeps the executable bit.
func (g *Git) SetExecutable(ctx context.Context, p string) error {
	if err := checkStdinPath(p); err != nil {
		return err
	}
	_, err := g.Run(ctx, nil, "-c", "core.longpaths=true", "-c", "core.safecrlf=false",
		"update-index", "--add", "--chmod=+x", "--", p)
	return err
}

// WithObjectDir returns a copy of g whose commands read and write objects
// in dir only (GIT_OBJECT_DIRECTORY), leaving the repository's own object
// store untouched: a scratch store for blobs that must not land in it.
func (g *Git) WithObjectDir(dir string) *Git {
	out := *g
	out.Env = append(slices.Clone(g.Env), "GIT_OBJECT_DIRECTORY="+dir)
	return &out
}
