package apply

import (
	"context"
	"fmt"
	"runtime"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Index modes of entries that are not regular files.
const (
	modeSymlink = "120000"
	modeGitlink = "160000"
)

// index is what touchmark reads from a target's git index.
type index struct {
	// modes maps a tracked path to its mode: the stage 0 entry, or any stage
	// of a path in conflict.
	modes map[string]string
	// staged maps a path the index holds at stage 0 as a regular file
	// (100644 or 100755) to its blob id: the content committed, or staged
	// for the next commit.
	staged map[string]string
	// trustModes is set where git ignores the executable bit on disk
	// (Windows, core.fileMode=false): the index is the only record of it,
	// and an untracked file would be added as 100644.
	trustModes bool
}

// readIndex reads the index and core.fileMode of the work tree g runs in.
func readIndex(ctx context.Context, g *gitx.Git) (*index, error) {
	trust, err := ignoresExecBit(ctx, g)
	if err != nil {
		return nil, err
	}
	entries, err := g.IndexEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the target's index: %w", err)
	}
	idx := &index{modes: make(map[string]string, len(entries)), staged: map[string]string{}, trustModes: trust}
	for _, e := range entries {
		if _, seen := idx.modes[e.Path]; !seen || e.Stage == 0 {
			idx.modes[e.Path] = e.Mode
		}
		if e.Stage == 0 && (e.Mode == modeFile || e.Mode == modeExec) {
			idx.staged[e.Path] = e.OID
		}
	}
	return idx, nil
}

// ignoresExecBit reports whether git ignores the executable bit on disk in
// the work tree g runs in: always on Windows, else when core.fileMode is
// false (WSL on /mnt/c, exFAT, some network mounts).
func ignoresExecBit(ctx context.Context, g *gitx.Git) (bool, error) {
	if runtime.GOOS == "windows" {
		return true, nil
	}
	fileMode, err := g.ConfigBool(ctx, "core.fileMode", true)
	if err != nil {
		return false, fmt.Errorf("read core.fileMode of the target: %w", err)
	}
	return !fileMode, nil
}

// submoduleAbove returns the highest ancestor of p that the index records
// as a submodule, "" if none. An ancestor at or below the blocker o already
// names does not count: the blocker is higher up.
func (idx *index) submoduleAbove(p string, o decide.Observation) string {
	for _, dir := range pathx.Parents(p) {
		if o.Kind == decide.UnsafeParent && dir == o.Blocker {
			return ""
		}
		if idx.modes[dir] == modeGitlink {
			return dir
		}
	}
	return ""
}
