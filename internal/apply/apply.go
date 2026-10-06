// Package apply observes a target working tree and executes a decide.Plan on
// it.
//
// All filesystem access goes through an os.Root opened on the target root,
// and every ancestor of a path is checked with Lstat: touchmark never writes
// or deletes through a symlink, even one that stays inside the target.
package apply

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
)

// Target is a target working tree.
type Target struct {
	// Root is the absolute path of the target root (the directory holding the
	// opt-in file; normally the git work tree root).
	Root string
	// Git runs git in Root; nil when Root is not inside a git work tree, in
	// which case only raw identities are computed. When set, Root must be the
	// work tree root: `git hash-object --stdin-paths` resolves paths from
	// there, so Observe and Execute fail otherwise.
	Git *gitx.Git
}

// Observe returns an observation for every path. Paths that fail
// pathx.Validate are InvalidPath. For regular files it computes the raw blob
// id and, when Git is set, the filtered id through `git hash-object
// --stdin-paths` (one run for all regular files, unless one of them cannot be
// hashed: that path becomes InvalidPath with git's reason); the filtered id
// is added to OIDs when it differs. A file whose raw bytes are the blob the
// index holds is known by that id alone, the committed blob that plan and
// distribute see, so that a file committed with CRLF does not match a
// pack's LF version through its filtered id. Mode is "100755"/"100644" from
// the owner's executable bit (as git reads it), or "" on Windows outside
// git.
//
// Case: ActualPath is set when the target spells an existing entry with other
// case than p (a case-insensitive filesystem found it), and CaseTwin, for an
// absent p, names an existing entry whose path differs only by case (a
// case-sensitive filesystem). Both come from reading the directories on the
// way. An entry whose spelling cannot be told is InvalidPath.
//
// An ancestor directory that holds a .git entry (a nested repository or a
// checked-out submodule) makes a path UnsafeParent, as does a symlink or a
// non-directory. When Git is set and some path has an existing ancestor or
// is a regular file, the index is read once: an ancestor recorded as a
// submodule makes a path UnsafeParent, a regular file the index records as
// a symlink or submodule is NotRegular, and where git ignores the
// executable bit on disk (Windows, core.fileMode=false) Mode comes from the
// index (100644 for an untracked file, as git would add it).
//
// A path the filesystem refuses to inspect (a name the OS rejects, a
// permission error, a file replaced while being read) is observed as
// InvalidPath with the reason in Detail, so nothing is written there. Failing
// to open the root, to run git, or a cancelled ctx is an error.
func Observe(ctx context.Context, t Target, paths []string) (map[string]decide.Observation, error) {
	root, err := os.OpenRoot(t.Root)
	if err != nil {
		return nil, fmt.Errorf("open target: %w", err)
	}
	defer root.Close()
	ob := &observer{root: root, names: newLister(root)}
	out := make(map[string]decide.Observation, len(paths))
	for _, p := range paths {
		if _, seen := out[p]; seen {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[p] = ob.observe(p)
	}
	regular := regularPaths(paths, out)
	if t.Git == nil || (len(regular) == 0 && !ob.reached) {
		return out, nil
	}
	if err := checkWorkTreeRoot(ctx, t); err != nil {
		return nil, err
	}
	idx, err := readIndex(ctx, t.Git)
	if err != nil {
		return nil, err
	}
	applyIndex(out, idx)
	if err := addFiltered(ctx, t, out, regularPaths(paths, out)); err != nil {
		return nil, err
	}
	useStaged(out, idx)
	return out, nil
}

// BlobSource opens the content of a hub blob by id.
type BlobSource interface {
	Open(oid string) (io.ReadCloser, error)
}

// Result is what happened to one plan entry.
type Result struct {
	Entry decide.Entry
	// Done is true when the action was carried out (always true for Keep).
	Done bool
	// Skipped explains why a planned action was not carried out because the
	// file changed between Observe and Execute, or an AfterDeletes obstacle
	// remained. Skipped is not an error.
	Skipped string
	Err     error
}

// Execute carries out plan and returns one Result per entry, in plan order.
// Deletes run first, then the other entries in plan order (a plan from
// decide.Decide is already ordered so).
//
// Before touching a path it re-checks the state the plan was based on
// (TOCTOU): a delete requires the file to still hash to Entry.From; an update
// requires the file to still hash to Entry.From; an adopt requires a regular
// file, and the Entry.From content when set (decide.Decide leaves it empty:
// --adopt overwrites whatever local content is there); a chmod requires a
// regular file, with the Entry.From content when set; a create requires the
// path to be absent, with no entry beside it whose name differs only by case
// (for AfterDeletes entries this is checked after the deletes ran). "Hash
// to" means the raw bytes or, when Git is set, the content after the
// target's git filters. Parents must be real directories of the target (or
// absent, for writes); a symlink or a nested repository anywhere on the way
// skips the entry.
//
// Writes are atomic: a temp file in the same directory, written, verified
// against Entry.To, chmod'ed, then moved into place; the state is checked
// again right before the move. A create hard-links the temp file into place,
// which fails rather than replace a file that appeared meanwhile (a
// filesystem without hard links falls back to a rename); an update or adopt
// renames over the file, clearing a Windows read-only attribute when that
// is what refuses it. Where git ignores the executable bit on disk
// (Windows, core.fileMode=false), a 100755 file is also recorded as such in
// the index, which adds it there. Deletes remove parents that became
// empty, up to the root. A failure on one path does not stop the others; a
// cancelled ctx stops before the next entry and the entries not run get
// ctx.Err().
func Execute(ctx context.Context, t Target, plan decide.Plan, blobs BlobSource) []Result {
	results := make([]Result, len(plan.Entries))
	for i, e := range plan.Entries {
		results[i] = Result{Entry: e, Done: e.Action == decide.Keep}
	}
	root, err := os.OpenRoot(t.Root)
	if err != nil {
		failPending(results, fmt.Errorf("open target: %w", err))
		return results
	}
	defer root.Close()
	x := &executor{ctx: ctx, t: t, root: root, blobs: blobs}
	for _, deletes := range []bool{true, false} {
		for i := range results {
			r := &results[i]
			if r.Done || (r.Entry.Action == decide.Delete) != deletes {
				continue
			}
			if err := ctx.Err(); err != nil {
				r.Err = err
				continue
			}
			x.run(r)
		}
	}
	return results
}

// failPending sets err on every result that is not done.
func failPending(results []Result, err error) {
	for i := range results {
		if !results[i].Done {
			results[i].Err = err
		}
	}
}
