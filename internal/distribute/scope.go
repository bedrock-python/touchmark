package distribute

import (
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The scope of a plan in a hub pull request: the resolve and the opt-in
// files cover every target, since the final pack lists need them, but only
// the targets of the packs the pull request changes are snapshotted and
// decided. The other targets stay targets of the run: the sweep never closes
// their pull requests as target-dropped or opted-out, and the mass-close
// guard counts their open pull requests; they are only counted in the
// report.

// Scope limits a plan to the targets a hub pull request touches (Deps.Scope;
// nil plans every target and reports no scope). Only ModePlan reads it:
// distribute always processes every target.
type Scope struct {
	// Mode is report.ScopeAll, report.ScopePacks or report.ScopeHub.
	Mode string
	// Packs are the pack directories the pull request changes (ScopePacks):
	// the names under packs/.
	Packs []string
	// Reason says what decided the mode, for the report.
	Reason string
	// Base is the commit the hub commit was compared with: their merge base
	// with the default branch ("" when unknown).
	Base string
}

// Hub paths every target depends on besides hub.yml and targets.yml: the
// one-off operations and anything else under .touchmark/, and the schemas.
const (
	touchmarkDir = ".touchmark/"
	schemasDir   = "schemas/"
	packsDir     = "packs/"
)

// maxScopeFiles is how many files a scope's reason names.
const maxScopeFiles = 3

// ScopeOf classifies the hub paths a pull request changes (git diff
// --name-only --no-renames between the merge base with the default branch
// and the hub commit, and the paths under packs/ any of its commits changes:
// a pack file's past versions decide targets too) for a plan:
//   - hub.yml, targets.yml, a path under .touchmark/ (operations.yml,
//     pr.intro_file's usual home) or schemas/, and a file hub.yml refers to
//     (pr.intro_file, a provider's ca_file): every target (ScopeAll, with
//     the files in Reason);
//   - else a path under packs/<p>/: the targets of the packs p
//     (ScopePacks);
//   - else (README, CI files, a file directly under packs/): no target
//     (ScopeHub).
//
// hub is the hub.yml of the hub commit (nil for none). Paths use forward
// slashes, as git prints them; they are compared as they are.
func ScopeOf(changed []string, hub *config.Hub) Scope {
	refs := map[string]bool{config.HubFile: true, config.TargetsFile: true}
	if hub != nil {
		if hub.PR.IntroFile != "" {
			refs[path.Clean(hub.PR.IntroFile)] = true
		}
		for _, p := range hub.Providers {
			if p.CAFile != "" {
				refs[path.Clean(p.CAFile)] = true
			}
		}
	}
	var all []string
	packs := map[string]bool{}
	for _, p := range changed {
		switch {
		case refs[p] || strings.HasPrefix(p, touchmarkDir) || strings.HasPrefix(p, schemasDir):
			all = append(all, p)
		case strings.HasPrefix(p, packsDir):
			if name, _, ok := strings.Cut(p[len(packsDir):], "/"); ok && name != "" {
				packs[name] = true
			}
		}
	}
	if len(all) > 0 {
		slices.Sort(all)
		all = slices.Compact(all)
		reason := strings.Join(all[:min(len(all), maxScopeFiles)], ", ")
		if n := len(all) - maxScopeFiles; n > 0 {
			reason += " and " + strconv.Itoa(n) + " more"
		}
		return Scope{Mode: report.ScopeAll, Reason: reason + " changed"}
	}
	if len(packs) > 0 {
		return Scope{Mode: report.ScopePacks, Packs: slices.Sorted(maps.Keys(packs))}
	}
	return Scope{Mode: report.ScopeHub}
}

// limited reports whether the run is a plan whose scope leaves targets
// out: ScopePacks or ScopeHub.
func (r *run) limited() bool {
	s := r.d.Scope
	return r.mode == ModePlan && s != nil && s.Mode != report.ScopeAll
}

// hubOnly reports whether the run is a plan that processes no target: the
// hub pull request changes nothing a target receives.
func (r *run) hubOnly() bool {
	return r.limited() && r.d.Scope.Mode == report.ScopeHub
}

// scopePacks returns the packs a limited plan processes the targets of:
// the packs the pull request changes, and the current name of each whose
// former name (formerly) is among them.
func (r *run) scopePacks() map[string]bool {
	out := map[string]bool{}
	if !r.limited() {
		return out
	}
	for _, p := range r.d.Scope.Packs {
		out[p] = true
	}
	for name, former := range r.aliases {
		if slices.ContainsFunc(former, func(f string) bool { return out[f] }) {
			out[name] = true
		}
	}
	return out
}

// touched reports whether a limited plan processes a target whose final
// pack list is packs: it holds a pack of the scope.
func (r *run) touched(packs []string) bool {
	return slices.ContainsFunc(packs, func(p string) bool { return r.affected[p] })
}

// leaveBySelection marks t out of the scope of a limited plan when its
// final pack list (the selection with requires, former names resolved)
// holds no pack of the scope, and reports whether it did: the target is
// then neither snapshotted nor decided.
func (r *run) leaveBySelection(t *target, packs []string) bool {
	if !r.limited() || r.touched(packs) {
		return false
	}
	t.outOfScope = true
	return true
}

// leaveByHub marks t out of the scope of a limited plan when its outcome
// was decided before its packs were known (skipped, or its opt-in file
// missing, unsafe or invalid) and the packs the hub gives it holds no pack
// of the scope: defaults.packs and the packs of its targets.yml entries,
// with requires, and named, the packs its opt-in file asks for (under
// their current names). A target whose hub selection fails stays in scope.
func (r *run) leaveByHub(t *target, named []string) {
	if !r.limited() {
		return
	}
	sel, _, err := config.SelectFor(r.hub, r.targets, nil, t.entries, r.known)
	if err != nil {
		return
	}
	packs := slices.Clone(sel.Packs)
	for _, p := range named {
		packs = append(packs, r.currentName(p))
	}
	if !r.touched(packs) {
		t.outOfScope = true
	}
}

// currentName returns the pack that carries p now: p, or the pack whose
// former name it is.
func (r *run) currentName(p string) string {
	for name, former := range r.aliases {
		if slices.Contains(former, p) {
			return name
		}
	}
	return p
}

// planned returns the targets of list a limited plan processes, in order:
// every target when the plan is not limited.
func planned(list []*target) []*target {
	out := make([]*target, 0, len(list))
	for _, t := range list {
		if !t.outOfScope {
			out = append(out, t)
		}
	}
	return out
}

// reportScope sets the report's scope for a plan with Deps.Scope: its
// mode, the packs, and how many of the resolved targets it processed.
func (r *run) reportScope(kept []*target) {
	s := r.d.Scope
	if r.mode != ModePlan || s == nil {
		return
	}
	packs := []string(nil)
	if s.Mode == report.ScopePacks {
		packs = slices.Sorted(maps.Keys(r.affected))
	}
	r.rep.Scope = &report.Scope{Mode: s.Mode, Reason: s.Reason, Packs: packs, Base: s.Base,
		Processed: len(planned(kept)), Total: len(kept)}
}

// sweepKey is how the mass-close guard names an open pull request: the
// repository's host and id and the number.
func sweepKey(host, repoID string, n int64) string {
	return strings.ToLower(host) + "/" + repoID + "#" + strconv.FormatInt(n, 10)
}

// noteOpen records, for the mass-close guard of a limited plan, the open
// pull requests of touchmark the sweep of a provider listed: the guard
// weighs the plan's closes against every open pull request of touchmark, the
// ones in targets the plan leaves out too.
func (r *run) noteOpen(cands []decide.SweepCandidate) {
	if !r.limited() {
		return
	}
	for _, c := range cands {
		if r.sweptOpen == nil {
			r.sweptOpen = map[string]bool{}
		}
		r.sweptOpen[sweepKey(c.Repo.Host, c.Repo.ID, c.PR.PR.Number)] = true
	}
}

// keepSweep reports whether a sweep close stays in a plan: always, unless
// the plan is limited, which keeps only the closes of the targets it
// processes (opted-out); a repository dropped from targets.yml is no target
// of it, and a pack pull request changes nothing there.
func (r *run) keepSweep(c decide.SweepClose, optedOut map[string]*target) bool {
	if !r.limited() {
		return true
	}
	repo := c.Candidate.Repo
	_, ok := optedOut[strings.ToLower(repo.Host)+"/"+repo.ID]
	return ok
}
