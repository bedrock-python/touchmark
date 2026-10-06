package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// BuildCommit writes the blobs, builds the tree in a temporary index
// (read-tree Parent, update-index --cacheinfo / --force-remove,
// write-tree --missing-ok), writes the commit object with hash-object -t
// commit -w, and checks that diff-tree Parent..tree lists exactly Changes.
// Same input gives the same commit id.
//
// In detail:
//   - Parent must be a full id of a commit present locally; every Change
//     path must pass pathx.Validate, appear once, and have Mode "100644",
//     "100755" or "" (delete); a written OID must be in Blobs.
//   - Each distinct blob a change writes is streamed to
//     `hash-object -w --no-filters --stdin` and must hash to its OID
//     (ErrIntegrity otherwise). Blobs no change uses are not written.
//   - The index lives in a temporary directory inside Dir, removed
//     afterwards. Changes go in through `update-index -z --index-info`, the
//     stdin form of --cacheinfo and --force-remove (mode 0 removes), so
//     paths never meet argv: deletions first, then writes. Blobs of Parent
//     need not be present (write-tree --missing-ok).
//   - The commit object is "tree", "parent", "author" and "committer"
//     ("Name <email> <unix> +0000", When in UTC), an optional "gpgsig"
//     header, a blank line and Message, completed with a final newline.
//     Sign signs exactly the object without the gpgsig header, which is
//     what git signs and verifies for gpg.format=ssh; the signature's lines
//     become the header's continuation lines. Message must not be empty or
//     hold NUL.
//   - `diff-tree -r --no-renames Parent <commit>` must list exactly the
//     changed paths, each with the requested mode and blob (a deletion as
//     mode 000000); anything else wraps ErrIntegrity. An update-index
//     that replaced a file by a directory or the reverse, or a change that
//     changes nothing, is caught here.
func (t *TargetRepo) BuildCommit(ctx context.Context, spec CommitSpec) (Built, error) {
	if err := checkSpec(spec); err != nil {
		return Built{}, fmt.Errorf("build commit: %w", err)
	}
	if err := t.checkCommit(ctx, spec.Parent); err != nil {
		return Built{}, fmt.Errorf("build commit: parent: %w", err)
	}
	if err := t.writeBlobs(ctx, spec); err != nil {
		return Built{}, fmt.Errorf("build commit: %w", err)
	}
	tree, err := t.buildTree(ctx, spec.Parent, spec.Changes)
	if err != nil {
		return Built{}, fmt.Errorf("build commit: %w", err)
	}
	object, err := commitObject(tree, spec)
	if err != nil {
		return Built{}, fmt.Errorf("build commit: %w", err)
	}
	out, err := t.run(ctx, cmdOpts{stdin: bytes.NewReader(object)}, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return Built{}, fmt.Errorf("build commit: write the commit: %w", err)
	}
	commit := trimEOL(out)
	if !isOID(commit) {
		return Built{}, fmt.Errorf("build commit: git hash-object: unexpected output %q", abbrev(commit))
	}
	if err := t.checkI1(ctx, spec.Parent, commit, spec.Changes); err != nil {
		return Built{}, fmt.Errorf("build commit %s: %w", commit, err)
	}
	return Built{Commit: commit, Tree: tree}, nil
}

// checkSpec validates spec without running git.
func checkSpec(spec CommitSpec) error {
	if !isOID(spec.Parent) {
		return fmt.Errorf("parent %q is not a full object id", abbrev(spec.Parent))
	}
	seen := make(map[string]bool, len(spec.Changes))
	for _, c := range spec.Changes {
		if err := pathx.Validate(c.Path); err != nil {
			return err
		}
		if seen[c.Path] {
			return fmt.Errorf("path %q changes twice", c.Path)
		}
		seen[c.Path] = true
		switch c.Mode {
		case "":
		case "100644", "100755":
			if !isOID(c.OID) {
				return fmt.Errorf("%s: blob %q is not a full object id", c.Path, abbrev(c.OID))
			}
			if b, ok := spec.Blobs[c.OID]; !ok || b.Open == nil {
				return fmt.Errorf("%s: no content for blob %s", c.Path, c.OID)
			}
		default:
			return fmt.Errorf("%s: mode %q is not 100644, 100755 or a deletion", c.Path, abbrev(c.Mode))
		}
	}
	if len(spec.Changes) == 0 {
		return errors.New("no changes: the commit would equal its parent")
	}
	if err := checkPerson("author", spec.Author); err != nil {
		return err
	}
	if err := checkPerson("committer", spec.Committer); err != nil {
		return err
	}
	if spec.When.IsZero() || spec.When.Unix() < 0 {
		return fmt.Errorf("date %v is not after 1970", spec.When)
	}
	if strings.TrimSpace(spec.Message) == "" || strings.ContainsRune(spec.Message, 0) {
		return errors.New("the message is empty or holds NUL")
	}
	return nil
}

// checkPerson validates an ident: git's fsck refuses '<', '>' and line
// breaks, and git itself would strip leading and trailing spaces.
func checkPerson(role string, p Person) error {
	bad := func(s string) bool {
		return s == "" || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '<' || r == '>' })
	}
	switch {
	case bad(p.Name) || strings.TrimSpace(p.Name) != p.Name:
		return fmt.Errorf("invalid %s name %q", role, abbrev(p.Name))
	case bad(p.Email) || strings.TrimSpace(p.Email) != p.Email:
		return fmt.Errorf("invalid %s email %q", role, abbrev(p.Email))
	}
	return nil
}

// checkCommit fails unless id is a commit present locally.
func (t *TargetRepo) checkCommit(ctx context.Context, id string) error {
	out, err := t.run(ctx, cmdOpts{}, "cat-file", "-t", id)
	if err != nil {
		return err
	}
	if typ := trimEOL(out); typ != "commit" {
		return fmt.Errorf("object %s is a %s, not a commit", id, typ)
	}
	return nil
}

// writeBlobs stores each distinct blob the changes write, in path order,
// and checks its id.
func (t *TargetRepo) writeBlobs(ctx context.Context, spec CommitSpec) error {
	var ids []string
	for _, c := range sortedChanges(spec.Changes) {
		if c.Mode != "" && !slices.Contains(ids, c.OID) {
			ids = append(ids, c.OID)
		}
	}
	for _, id := range ids {
		if err := t.writeBlob(ctx, id, spec.Blobs[id]); err != nil {
			return err
		}
	}
	return nil
}

// writeBlob streams one blob into `hash-object -w --no-filters --stdin`.
func (t *TargetRepo) writeBlob(ctx context.Context, id string, b Blob) error {
	r, err := b.Open()
	if err != nil {
		return fmt.Errorf("open blob %s: %w", id, err)
	}
	out, err := t.run(ctx, cmdOpts{stdin: r}, "hash-object", "-w", "--no-filters", "--stdin")
	closeErr := r.Close()
	if err != nil {
		return fmt.Errorf("write blob %s: %w", id, err)
	}
	if closeErr != nil {
		return fmt.Errorf("read blob %s: %w", id, closeErr)
	}
	if got := trimEOL(out); got != id {
		return fmt.Errorf("blob %s: the content hashes to %s: %w", id, abbrev(got), ErrIntegrity)
	}
	return nil
}

// indexPathChecks turns off the checks git makes of the paths it puts in an
// index for a checkout on NTFS or HFS+: reserved device names ("aux.c",
// "CON"), trailing dots and spaces, characters Windows forbids, .git
// look-alikes. B's tree comes from the platform, which accepted them, and
// nothing here is ever checked out, so they only made BuildCommit fail on
// the OS whose checks apply (Git for Windows checks core.protectNTFS paths,
// macOS core.protectHFS ones). touchmark's own paths passed pathx.Validate.
var indexPathChecks = []string{"-c", "core.protectNTFS=false", "-c", "core.protectHFS=false"}

// buildTree writes the tree of parent with changes applied, through a
// temporary index.
//
// One path check stays on Git for Windows whatever the settings: a path
// whose first component starts with a drive letter and a colon ("c:x")
// cannot enter an index there. A base holding one fails with an error that
// says so; the same target builds on other systems.
func (t *TargetRepo) buildTree(ctx context.Context, parent string, changes []Change) (string, error) {
	tmp, err := os.MkdirTemp(t.Dir, "touchmark-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	o := cmdOpts{env: []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}}
	if _, err := t.run(ctx, o, append(slices.Clone(indexPathChecks), "read-tree", parent)...); err != nil {
		var e *Error
		if runtime.GOOS == "windows" && errors.As(err, &e) && strings.Contains(e.Stderr, "invalid path") {
			return "", fmt.Errorf("read the base tree: it holds a path Git for Windows cannot put in an index, such as one that starts with a drive prefix (\"c:x\"); the commit builds on other systems: %w", err)
		}
		return "", err
	}
	var in bytes.Buffer
	sorted := sortedChanges(changes)
	for _, c := range sorted {
		if c.Mode == "" {
			in.WriteString("0 " + zeroSHA1 + "\t" + c.Path + "\x00")
		}
	}
	for _, c := range sorted {
		if c.Mode != "" {
			in.WriteString(c.Mode + " " + c.OID + "\t" + c.Path + "\x00")
		}
	}
	o.stdin = &in
	if _, err := t.run(ctx, o, append(slices.Clone(indexPathChecks), "update-index", "-z", "--index-info")...); err != nil {
		return "", err
	}
	o.stdin = nil
	out, err := t.run(ctx, o, "write-tree", "--missing-ok")
	if err != nil {
		return "", err
	}
	tree := trimEOL(out)
	if !isOID(tree) {
		return "", fmt.Errorf("git write-tree: unexpected output %q", abbrev(tree))
	}
	return tree, nil
}

// sortedChanges returns changes sorted by path bytes.
func sortedChanges(changes []Change) []Change {
	s := slices.Clone(changes)
	slices.SortFunc(s, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return s
}

// commitObject returns the raw commit object: the payload, and the gpgsig
// header when spec.Sign is set.
func commitObject(tree string, spec CommitSpec) ([]byte, error) {
	when := strconv.FormatInt(spec.When.Unix(), 10) + " +0000"
	var head bytes.Buffer
	head.WriteString("tree " + tree + "\n")
	head.WriteString("parent " + spec.Parent + "\n")
	head.WriteString("author " + spec.Author.Name + " <" + spec.Author.Email + "> " + when + "\n")
	head.WriteString("committer " + spec.Committer.Name + " <" + spec.Committer.Email + "> " + when + "\n")
	msg := spec.Message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	payload := slices.Concat(head.Bytes(), []byte("\n"+msg))
	if spec.Sign == nil {
		return payload, nil
	}
	sig, err := spec.Sign(payload)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	sig = strings.TrimSuffix(sig, "\n")
	if sig == "" || strings.ContainsAny(sig, "\r\x00") {
		return nil, errors.New("sign: the signature is empty or holds CR or NUL")
	}
	head.WriteString("gpgsig")
	for line := range strings.SplitSeq(sig, "\n") {
		head.WriteString(" " + line + "\n")
	}
	return slices.Concat(head.Bytes(), []byte("\n"+msg)), nil
}

// checkI1 compares diff-tree parent..commit with changes.
func (t *TargetRepo) checkI1(ctx context.Context, parent, commit string, changes []Change) error {
	diff, err := t.DiffTree(ctx, parent, commit)
	if err != nil {
		return err
	}
	want := make(map[string]Change, len(changes))
	for _, c := range changes {
		want[c.Path] = c
	}
	for _, e := range diff {
		c, ok := want[e.Path]
		switch {
		case !ok:
			return fmt.Errorf("the commit also changes %s: %w", e.Path, ErrIntegrity)
		case c.Mode == "" && (e.NewMode != "000000" || e.NewOID != zeroSHA1):
			return fmt.Errorf("%s is not deleted: %w", e.Path, ErrIntegrity)
		case c.Mode != "" && (e.NewMode != c.Mode || e.NewOID != c.OID):
			return fmt.Errorf("%s is %s %s, want %s %s: %w", e.Path, e.NewMode, e.NewOID, c.Mode, c.OID, ErrIntegrity)
		}
		delete(want, e.Path)
	}
	if len(want) > 0 {
		paths := make([]string, 0, len(want))
		for p := range want {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		return fmt.Errorf("the commit does not change %s: %w", paths[0], ErrIntegrity)
	}
	return nil
}
