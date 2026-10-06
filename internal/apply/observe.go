package apply

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// observer inspects the paths of one target.
type observer struct {
	root  *os.Root
	names *lister
	// reached is set once a path has an existing ancestor directory: the
	// index is then needed to tell whether one of them is a submodule.
	reached bool
}

// observe inspects one repository path without following any symlink.
func (ob *observer) observe(p string) decide.Observation {
	if err := pathx.Validate(p); err != nil {
		return decide.Observation{Kind: decide.InvalidPath, Detail: err.Error()}
	}
	missing, b, err := inspectParents(ob.root, p)
	if err != nil {
		return uninspectable(err)
	}
	if parents := pathx.Parents(p); len(parents) > 0 && missing != parents[0] {
		ob.reached = true
	}
	switch {
	case missing != "":
		return ob.absent(p)
	case b != nil:
		return decide.Observation{
			Kind:          decide.UnsafeParent,
			Blocker:       b.path,
			BlockerIsFile: b.isFile(),
			Detail:        b.detail(),
		}
	}
	fi, exists, err := lstat(ob.root, p)
	switch {
	case err != nil:
		return uninspectable(err)
	case !exists:
		return ob.absent(p)
	}
	var o decide.Observation
	switch kindOf(fi) {
	case kindRegular:
		o = observeFile(ob.root, p, fi)
	case kindDir:
		o = decide.Observation{Kind: decide.NotRegular, IsDir: true, Detail: "a directory"}
	default:
		o = decide.Observation{Kind: decide.NotRegular, Detail: describe(fi)}
	}
	if o.Kind == decide.InvalidPath {
		return o
	}
	return ob.spell(p, o)
}

// absent is the observation of a path that does not exist, with the entry
// that differs from it only by case, if the target holds one.
func (ob *observer) absent(p string) decide.Observation {
	twin, err := ob.names.twin(p)
	if err != nil {
		return uninspectable(err)
	}
	return decide.Observation{Kind: decide.Absent, CaseTwin: twin}
}

// spell records in o how the target spells the entry found at p.
func (ob *observer) spell(p string, o decide.Observation) decide.Observation {
	actual, ok, err := ob.names.spelling(p)
	switch {
	case err != nil:
		return uninspectable(err)
	case !ok:
		return decide.Observation{Kind: decide.InvalidPath, Detail: "cannot tell how the target spells this path"}
	case actual != p:
		o.ActualPath = actual
	}
	return o
}

// observeFile hashes the raw bytes of the regular file p that Lstat
// described as lst.
func observeFile(root *os.Root, p string, lst fs.FileInfo) decide.Observation {
	oid, fi, err := rawOID(root, p, lst)
	if err != nil {
		return uninspectable(err)
	}
	return decide.Observation{Kind: decide.Regular, OIDs: []string{oid}, Mode: modeOf(fi)}
}

// uninspectable is the observation for a path the filesystem refused to
// inspect: unsafe, so nothing is written or deleted there, with the reason.
func uninspectable(err error) decide.Observation {
	detail := "cannot inspect: " + err.Error()
	if errors.Is(err, errChanged) {
		detail = "changed while being inspected"
	}
	return decide.Observation{Kind: decide.InvalidPath, Detail: detail}
}

// applyIndex corrects the observations with what the target's index says:
// a submodule on the way makes a path unsafe, a path git tracks as a
// symlink or submodule is not a regular file whatever the filesystem shows
// (Windows checks symlinks out as plain files without core.symlinks), and
// where git ignores the executable bit on disk the index's mode counts.
func applyIndex(out map[string]decide.Observation, idx *index) {
	for p, o := range out {
		if o.Kind == decide.InvalidPath {
			continue
		}
		if sub := idx.submoduleAbove(p, o); sub != "" {
			out[p] = decide.Observation{Kind: decide.UnsafeParent, Blocker: sub, Detail: "parent " + sub + " is a submodule"}
			continue
		}
		if o.Kind != decide.Regular {
			continue
		}
		switch idx.modes[p] {
		case modeSymlink:
			out[p] = decide.Observation{Kind: decide.NotRegular, Detail: "a symlink in git"}
			continue
		case modeGitlink:
			out[p] = decide.Observation{Kind: decide.NotRegular, Detail: "a submodule in git"}
			continue
		}
		if idx.trustModes {
			o.Mode = modeFile
			if idx.modes[p] == modeExec {
				o.Mode = modeExec
			}
			out[p] = o
		}
	}
}

// addFiltered adds to each regular observation the id git would store for
// the file, through `git hash-object --stdin-paths` for all of them. A file
// git cannot hash (a path too long for the platform, a failing clean filter)
// becomes InvalidPath with git's reason, and the others are still hashed.
func addFiltered(ctx context.Context, t Target, out map[string]decide.Observation, regular []string) error {
	if t.Git == nil || len(regular) == 0 {
		return nil
	}
	ids, errs, err := t.Git.HashPathsEach(ctx, regular)
	if err != nil {
		return fmt.Errorf("hash target files through git: %w", err)
	}
	for i, p := range regular {
		if errs[i] != nil {
			out[p] = decide.Observation{Kind: decide.InvalidPath, Detail: "cannot hash through git: " + gitReason(errs[i])}
			continue
		}
		o := out[p]
		if !slices.Contains(o.OIDs, ids[i]) {
			o.OIDs = append(o.OIDs, ids[i])
			out[p] = o
		}
	}
	return nil
}

// useStaged leaves a regular file whose bytes on disk are the blob the
// index holds with that blob's id alone: the blob a commit holds is what
// other clones see, what a pull request changes, and what plan and
// distribute compare. A file committed with CRLF and checked out as is
// (core.autocrlf=true) is such a file; its filtered id, converted to LF, is
// not what the repository holds, and must not make it equal to a pack's LF
// version. A file that matches the index only through its filters (an LF
// blob checked out with CRLF, a file stored through a clean filter such as
// Git LFS) keeps both ids, so that its bytes on disk still count, as do
// those of a modified or untracked file. On a case-insensitive filesystem
// the index is looked up under the spelling the target uses.
func useStaged(out map[string]decide.Observation, idx *index) {
	for p, o := range out {
		if o.Kind != decide.Regular || len(o.OIDs) < 2 {
			continue
		}
		tracked := p
		if o.ActualPath != "" {
			tracked = o.ActualPath
		}
		if id, ok := idx.staged[tracked]; ok && o.OIDs[0] == id {
			o.OIDs = []string{id}
			out[p] = o
		}
	}
}

// gitReason returns the last line of a git error, where git states why.
func gitReason(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.LastIndex(msg, "\n"); i >= 0 {
		msg = msg[i+1:]
	}
	return msg
}

// regularPaths returns the paths of out observed as regular files, in the
// order of paths.
func regularPaths(paths []string, out map[string]decide.Observation) []string {
	var regular []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !seen[p] && out[p].Kind == decide.Regular {
			regular = append(regular, p)
		}
		seen[p] = true
	}
	return regular
}
