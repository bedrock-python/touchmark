package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FirstParentLog returns up to max commits of the first-parent chain from
// rev, newest first, and whether the chain reached a shallow boundary (a
// commit whose parents are not present) within max.
//
// The chain is git's view of the local history (git rev-list
// --first-parent --max-count=<max+1>): a shallow commit ends it. shallow is
// set when the chain ended within max commits at a commit whose object
// names a parent, that is at a shallow boundary rather than at a root.
// When the chain goes on beyond max commits, shallow is false. Commits are
// read from their objects (git cat-file --batch), so messages may hold any
// bytes; a commit object larger than MaxCommitObject fails the read with
// ErrTooLarge. max must be positive.
func (t *TargetRepo) FirstParentLog(ctx context.Context, rev string, max int) (commits []CommitInfo, shallow bool, err error) {
	if err := checkObjectName(rev); err != nil {
		return nil, false, err
	}
	if max < 1 {
		return nil, false, fmt.Errorf("git rev-list: max %d is not positive", max)
	}
	out, err := t.run(ctx, cmdOpts{}, "rev-list", "--first-parent", "--max-count="+strconv.Itoa(max+1), "--end-of-options", rev)
	if err != nil {
		return nil, false, err
	}
	var ids []string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		if !isOID(line) {
			return nil, false, fmt.Errorf("git rev-list %s: unexpected output %q", rev, abbrev(line))
		}
		ids = append(ids, line)
	}
	more := len(ids) > max
	if more {
		ids = ids[:max]
	}
	commits, err = t.commits(ctx, ids)
	if err != nil {
		return nil, false, err
	}
	shallow = !more && len(commits[len(commits)-1].Parents) > 0
	return commits, shallow, nil
}

// Commit returns one commit's info. rev may be any revision naming a
// commit (a tag is peeled); a revision that names nothing, or a commit
// that is not present, wraps ErrNotFound, and a commit object larger than
// MaxCommitObject ErrTooLarge.
func (t *TargetRepo) Commit(ctx context.Context, rev string) (CommitInfo, error) {
	if err := checkObjectName(rev); err != nil {
		return CommitInfo{}, err
	}
	name := rev + "^{commit}"
	objs, err := t.catFile(ctx, []string{name}, MaxCommitObject)
	if err != nil {
		return CommitInfo{}, err
	}
	if objs[0].missing {
		return CommitInfo{}, fmt.Errorf("commit %s: %w", abbrev(rev), ErrNotFound)
	}
	return commitInfo(objs[0])
}

// commits reads the commits ids, in order.
func (t *TargetRepo) commits(ctx context.Context, ids []string) ([]CommitInfo, error) {
	objs, err := t.catFile(ctx, ids, MaxCommitObject)
	if err != nil {
		return nil, err
	}
	out := make([]CommitInfo, 0, len(objs))
	for i, o := range objs {
		if o.missing {
			return nil, fmt.Errorf("commit %s: %w", ids[i], ErrNotFound)
		}
		c, err := commitInfo(o)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// commitInfo parses a commit object.
func commitInfo(o batchObject) (CommitInfo, error) {
	if o.typ != "commit" {
		return CommitInfo{}, fmt.Errorf("object %s is a %s, not a commit", o.oid, o.typ)
	}
	c, err := parseCommit(o.content)
	if err != nil {
		return CommitInfo{}, fmt.Errorf("commit %s: %w", o.oid, err)
	}
	c.SHA = o.oid
	return c, nil
}

// parseCommit parses a raw commit object: the header lines up to the first
// empty line ("tree", "parent" lines, "author", "committer", and others,
// possibly with continuation lines starting with a space), then the
// message. It needs a tree line and well-formed parent ids; a malformed
// committer line only leaves Time zero.
func parseCommit(raw []byte) (CommitInfo, error) {
	header, message, found := bytes.Cut(raw, []byte("\n\n"))
	if !found {
		// No message: the header ends at the end, maybe with its newline.
		header = bytes.TrimSuffix(raw, []byte("\n"))
	}
	var c CommitInfo
	tree := false
	for i, line := range strings.Split(string(header), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch {
		case i == 0:
			if key != "tree" || !isOID(value) {
				return CommitInfo{}, fmt.Errorf("unexpected first header line %q", abbrev(line))
			}
			tree = true
		case key == "parent":
			if !isOID(value) {
				return CommitInfo{}, fmt.Errorf("bad parent line %q", abbrev(line))
			}
			c.Parents = append(c.Parents, value)
		case key == "committer" && c.Time.IsZero():
			c.Time = identTime(value)
		}
	}
	if !tree {
		return CommitInfo{}, errors.New("no tree line")
	}
	c.Message = string(message)
	return c, nil
}

// identTime parses the date of an ident "Name <email> <unix> <+hhmm>", in
// the ident's zone; zero when it does not parse.
func identTime(ident string) time.Time {
	gt := strings.LastIndexByte(ident, '>')
	if gt < 0 {
		return time.Time{}
	}
	f := strings.Fields(ident[gt+1:])
	if len(f) != 2 {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(f[0], 10, 64)
	tz := f[1]
	if err != nil || len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return time.Time{}
	}
	hh, err1 := strconv.Atoi(tz[1:3])
	mm, err2 := strconv.Atoi(tz[3:5])
	if err1 != nil || err2 != nil || mm >= 60 {
		return time.Time{}
	}
	offset := hh*3600 + mm*60
	if tz[0] == '-' {
		offset = -offset
	}
	return time.Unix(sec, 0).In(time.FixedZone("", offset))
}

// DiffTree compares two trees or commits (from "" means the empty tree),
// with --no-renames and without textconv or external diff; paths are
// '/'-separated and sorted.
//
// It reads trees only (git diff-tree -r -z --raw), so the blobs need not
// be present. Submodules are compared by their commit ids.
func (t *TargetRepo) DiffTree(ctx context.Context, from, to string) ([]DiffEntry, error) {
	if from == "" {
		from = emptyTreeSHA1
	}
	if err := checkObjectName(from); err != nil {
		return nil, err
	}
	if err := checkObjectName(to); err != nil {
		return nil, err
	}
	out, err := t.run(ctx, cmdOpts{}, "diff-tree", "-r", "-z", "--raw", "--no-renames",
		"--no-ext-diff", "--no-textconv", "--no-commit-id", "--end-of-options", from, to)
	if err != nil {
		return nil, err
	}
	return parseDiffTree(out)
}

// parseDiffTree parses NUL-separated raw diff records
// ":<old mode> <new mode> <old oid> <new oid> <status>" NUL <path> NUL.
// Renames and copies are refused: DiffTree asks for none.
func parseDiffTree(out []byte) ([]DiffEntry, error) {
	var entries []DiffEntry
	toks := strings.Split(string(out), "\x00")
	for i := 0; i < len(toks); i++ {
		h := toks[i]
		if h == "" && i == len(toks)-1 {
			break
		}
		f := strings.Split(strings.TrimPrefix(h, ":"), " ")
		if !strings.HasPrefix(h, ":") || len(f) != 5 || !isOID(f[2]) || !isOID(f[3]) ||
			!isMode(f[0]) || !isMode(f[1]) || f[4] == "" || f[4][0] == 'R' || f[4][0] == 'C' {
			return nil, fmt.Errorf("git diff-tree: unexpected record %q", abbrev(h))
		}
		if i+1 >= len(toks) || toks[i+1] == "" {
			return nil, fmt.Errorf("git diff-tree: record %q has no path", abbrev(h))
		}
		i++
		entries = append(entries, DiffEntry{Path: toks[i], OldMode: f[0], NewMode: f[1], OldOID: f[2], NewOID: f[3]})
	}
	slices.SortFunc(entries, func(a, b DiffEntry) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}

// isMode reports whether s is six octal digits, as raw diffs print modes.
func isMode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// IsCleanMerge reports whether merge's tree equals, path by path, the tree
// of one of its first two parents: no path differs from both. It fails for
// a commit with fewer than two parents.
//
// The parents are those the commit object names; their trees must be
// present (they are, for commits a depth fetch brought, boundary included).
func (t *TargetRepo) IsCleanMerge(ctx context.Context, merge string) (bool, error) {
	c, err := t.Commit(ctx, merge)
	if err != nil {
		return false, err
	}
	if len(c.Parents) < 2 {
		return false, fmt.Errorf("commit %s has %d parent(s): not a merge", c.SHA, len(c.Parents))
	}
	first, err := t.DiffTree(ctx, c.Parents[0], c.SHA)
	if err != nil {
		return false, err
	}
	second, err := t.DiffTree(ctx, c.Parents[1], c.SHA)
	if err != nil {
		return false, err
	}
	changed := make(map[string]bool, len(first))
	for _, e := range first {
		changed[e.Path] = true
	}
	for _, e := range second {
		if changed[e.Path] {
			return false, nil
		}
	}
	return true, nil
}

// IsAncestor reports whether a is an ancestor of b (merge-base
// --is-ancestor) in the local history; a missing commit is an error the
// caller treats as "unknown".
//
// A commit is its own ancestor. The local history ends at shallow
// boundaries, so false may only mean that the history is too short: deepen
// it first (DeepenSince).
func (t *TargetRepo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	for _, rev := range []string{a, b} {
		if err := checkObjectName(rev); err != nil {
			return false, err
		}
	}
	_, err := t.run(ctx, cmdOpts{}, "merge-base", "--is-ancestor", a, b)
	var e *Error
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &e) && e.Code == 1:
		return false, nil
	}
	return false, err
}

// Tree lists every entry of rev recursively (ls-tree -r -z --full-tree).
//
// Blobs need not be present: the listing has no sizes (--long would read
// every blob), so Size is -1 for every entry. Trees themselves are not
// listed, only blobs, symlinks and submodules.
func (t *TargetRepo) Tree(ctx context.Context, rev string) ([]TreeEntry, error) {
	if err := checkObjectName(rev); err != nil {
		return nil, err
	}
	out, err := t.run(ctx, cmdOpts{}, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", rev)
	if err != nil {
		return nil, err
	}
	return parseShortTree(out)
}

// parseShortTree parses NUL-terminated `ls-tree` records without sizes:
// "<mode> SP <type> SP <oid> TAB <path>".
func parseShortTree(out []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Split(meta, " ")
		if !ok || path == "" || len(f) != 3 || !isOID(f[2]) || f[0] == "" || f[1] == "" {
			return nil, fmt.Errorf("git ls-tree: unexpected record %q", abbrev(rec))
		}
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Size: -1, Path: path})
	}
	return entries, nil
}

// checkObjectName rejects revisions git would read as options, and names
// that cannot travel on one line of cat-file's stdin.
func checkObjectName(rev string) error {
	if err := checkRev(rev); err != nil {
		return err
	}
	if strings.ContainsAny(rev, "\n\r\x00") {
		return fmt.Errorf("invalid git revision %q", abbrev(rev))
	}
	return nil
}
