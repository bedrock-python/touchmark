package distribute

import (
	"cmp"
	"slices"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// Actions of report.PathChange, in the order the report lists them after
// the sensitive paths: what removes or rewrites a file first.
var pathActions = []string{"delete", "update", "chmod", "add"}

// pathAction names what pair p does to its path.
func pathAction(p decide.Pair) string {
	switch {
	case p.Mode == decide.ModeDelete:
		return "delete"
	case p.From == decide.ZeroOID:
		return "add"
	case p.From == p.To:
		return "chmod"
	}
	return "update"
}

// reportPaths sets the report's Paths from the reported targets of a plan or
// a dry run (what a hub pull request changes, across the targets it
// affects): for every target whose decision pushes, each pair of its D
// counted by path and action, sensitive paths (prbody.Sensitive with
// hub.yml's sensitive_paths) first, then by action and path.
func (r *run) reportPaths(reported []*target) {
	type key struct{ action, path string }
	counts := map[key]int{}
	sensitive := map[key]bool{}
	for _, t := range reported {
		w := t.work
		if w == nil || !pushes(w.Decision) {
			continue
		}
		for _, p := range w.D {
			k := key{pathAction(p), p.Path}
			counts[k]++
			if prbody.Sensitive(p.Path, p.Mode, r.hub.SensitivePaths) {
				sensitive[k] = true
			}
		}
	}
	if len(counts) == 0 {
		return
	}
	out := make([]report.PathChange, 0, len(counts))
	for k, n := range counts {
		out = append(out, report.PathChange{Action: k.action, Path: k.path, Sensitive: sensitive[k], Targets: n})
	}
	rank := func(b bool) int {
		if b {
			return 0
		}
		return 1
	}
	slices.SortFunc(out, func(a, b report.PathChange) int {
		return cmp.Or(cmp.Compare(rank(a.Sensitive), rank(b.Sensitive)),
			cmp.Compare(slices.Index(pathActions, a.Action), slices.Index(pathActions, b.Action)),
			cmp.Compare(a.Path, b.Path))
	})
	r.rep.Paths = out
}
