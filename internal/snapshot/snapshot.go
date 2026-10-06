// Package snapshot describes a target's committed tree as delivery sees it:
// no checkout, only paths, modes and blob ids.
//
// In CI touchmark never checks a target out, so a file's identity is its
// committed blob id and line-ending settings of the runner never matter.
package snapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Entry is one tree entry.
type Entry struct {
	// Mode is "100644", "100755", "120000" (symlink) or "160000" (gitlink).
	Mode string
	OID  string
}

// Tree is the snapshot of one commit of a target.
type Tree struct {
	// Commit is the snapshot's commit id (B for the default branch).
	Commit string
	// Entries maps repository paths ('/' separated) to entries. Only
	// non-tree entries are listed; directories are implied by paths.
	Entries map[string]Entry
}

// Source takes snapshots of targets: GitSource (a blobless fetch through
// Remote), or the fake platform's in tests.
type Source interface {
	// Snapshot returns the tree of ref ("" = the default branch head).
	// An empty repository is an error the caller classifies as skipped.
	Snapshot(ctx context.Context, repo platform.Repo, remote platform.Remote, ref string) (*Tree, error)
}

// Git modes of tree entries.
const (
	modeFile    = "100644"
	modeExec    = "100755"
	modeSymlink = "120000"
	modeGitlink = "160000"
)

// Observe converts the tree into decide observations for paths, the way
// apply.Observe does for a working tree but with committed identities:
//   - a path that fails pathx.Validate is InvalidPath;
//   - a path with an ancestor that is an entry (a blob, symlink or gitlink)
//     is UnsafeParent with that Blocker (the topmost one);
//     BlockerIsFile for 100644/100755;
//   - 100644 and 100755 entries are Regular with OIDs = [OID] and Mode;
//   - 120000 and 160000 entries are NotRegular with a Detail;
//   - a path that is a proper prefix of other entries is NotRegular with
//     IsDir;
//   - an absent path is Absent, with CaseTwin set when another entry's path
//     equals it ignoring case (pathx.Fold), because git would track two
//     paths that collide on every case-insensitive checkout. A directory
//     implied by the entries counts as such a path, and so does, failing
//     that, an entry that equals one of the path's ancestors ignoring case
//     (a file where the path needs a directory). Of several, the smallest
//     path in byte order is named.
//
// ActualPath is never set: a tree is case-sensitive.
//
// A malformed tree cannot make Observe fail: an entry of mode 040000 (a
// tree, which Entries should not list) counts as a directory, an entry of
// an unknown mode is NotRegular, and an entry that also has entries beneath
// it is NotRegular without IsDir. A nil Tree is empty. Each distinct path
// is observed once.
func (t *Tree) Observe(paths []string) map[string]decide.Observation {
	var entries map[string]Entry
	if t != nil {
		entries = t.Entries
	}
	ix := &index{entries: entries}
	out := make(map[string]decide.Observation, len(paths))
	for _, p := range paths {
		if _, done := out[p]; !done {
			out[p] = ix.observe(p)
		}
	}
	return out
}

// index answers questions about the paths of a tree. Its maps are built on
// first use.
type index struct {
	entries map[string]Entry
	// dirs holds every directory the entries imply, and the paths of tree
	// entries.
	dirs map[string]bool
	// folds maps pathx.Fold of every entry and directory to their paths.
	folds map[string][]string
}

// observe returns the observation of one path.
func (ix *index) observe(p string) decide.Observation {
	if err := pathx.Validate(p); err != nil {
		return decide.Observation{Kind: decide.InvalidPath, Detail: err.Error()}
	}
	for _, a := range pathx.Parents(p) {
		if e, ok := ix.entries[a]; ok && !isTree(e.Mode) {
			return decide.Observation{
				Kind:          decide.UnsafeParent,
				Blocker:       a,
				BlockerIsFile: isFile(e.Mode),
				Detail:        "parent " + a + " is " + describe(e.Mode),
			}
		}
	}
	e, ok := ix.entries[p]
	switch {
	case ok && isTree(e.Mode):
		return directory()
	case ok && ix.isDir(p):
		return decide.Observation{Kind: decide.NotRegular, Detail: describe(e.Mode) + " with entries beneath it in the same tree"}
	case ok && isFile(e.Mode):
		return decide.Observation{Kind: decide.Regular, OIDs: []string{e.OID}, Mode: e.Mode}
	case ok:
		return decide.Observation{Kind: decide.NotRegular, Detail: describe(e.Mode)}
	case ix.isDir(p):
		return directory()
	}
	return decide.Observation{Kind: decide.Absent, CaseTwin: ix.twin(p)}
}

// directory is the observation of a directory.
func directory() decide.Observation {
	return decide.Observation{Kind: decide.NotRegular, IsDir: true, Detail: "a directory"}
}

// isDir reports whether p is a directory of the tree.
func (ix *index) isDir(p string) bool {
	ix.buildDirs()
	return ix.dirs[p]
}

// buildDirs fills dirs once.
func (ix *index) buildDirs() {
	if ix.dirs != nil {
		return
	}
	ix.dirs = map[string]bool{}
	for q, e := range ix.entries {
		if isTree(e.Mode) {
			ix.dirs[q] = true
		}
		// Deepest parent first: once one is known, so are its parents.
		for i := strings.LastIndexByte(q, '/'); i > 0; i = strings.LastIndexByte(q[:i], '/') {
			if ix.dirs[q[:i]] {
				break
			}
			ix.dirs[q[:i]] = true
		}
	}
}

// twin returns the path of the tree that collides with the absent path p on
// a case-insensitive checkout, "" when there is none: the smallest entry or
// directory equal to p ignoring case, else the smallest entry (not a
// directory) equal to an ancestor of p ignoring case, the topmost ancestor
// first.
func (ix *index) twin(p string) string {
	if ix.folds == nil {
		ix.buildDirs()
		ix.folds = map[string][]string{}
		add := func(q string) {
			f := pathx.Fold(q)
			ix.folds[f] = append(ix.folds[f], q)
		}
		for q := range ix.entries {
			add(q)
		}
		for d := range ix.dirs {
			if _, isEntry := ix.entries[d]; !isEntry {
				add(d)
			}
		}
	}
	if q := ix.variant(p, false); q != "" {
		return q
	}
	for _, a := range pathx.Parents(p) {
		if q := ix.variant(a, true); q != "" {
			return q
		}
	}
	return ""
}

// variant returns the smallest path other than p that equals p ignoring
// case: an entry or a directory, or with fileOnly an entry that is not a
// tree.
func (ix *index) variant(p string, fileOnly bool) string {
	best := ""
	for _, q := range ix.folds[pathx.Fold(p)] {
		if q == p || (best != "" && q >= best) {
			continue
		}
		if fileOnly {
			if e, ok := ix.entries[q]; !ok || isTree(e.Mode) {
				continue
			}
		}
		best = q
	}
	return best
}

// isFile reports whether mode is a regular file's.
func isFile(mode string) bool { return mode == modeFile || mode == modeExec }

// isTree reports whether mode is a tree's, as ls-tree ("040000") or a raw
// tree object ("40000") spells it.
func isTree(mode string) bool { return mode == "040000" || mode == "40000" }

// describe names what an entry of mode is, to follow "is" in messages.
func describe(mode string) string {
	switch mode {
	case modeFile, modeExec:
		return "a file"
	case modeSymlink:
		return "a symlink"
	case modeGitlink:
		return "a submodule"
	}
	return fmt.Sprintf("an entry of unknown mode %.16q", mode)
}
