package distribute

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// The snapshot-only plan (inspectSnapshotOnly and gateSnapshotOnly below):
// a plan whose snapshot source hands out no target repositories (Repos)
// knows neither branch history nor memory, builds no commit and does not
// sweep. Plan's doc describes it (steps C5 and E without Repos); the CLI's
// test hooks still plan this way.

// Write estimates per target by outcome (git path), for the snapshot-only
// plan; Run with Repos counts the steps of each decision instead (gate.go).
const (
	writesOpenGitHub = 3 // push, pull request, labels
	writesOpen       = 2 // push, pull request with labels
	writesUpdate     = 2 // push, body
	writesClose      = 3 // body and state, comment, branch deletion
	// writesLabel is the label a first pull request in a repository
	// creates on GitHub, Gitea and Forgejo; GitLab creates it with the
	// merge request.
	writesLabel = 1
)

// inspectSnapshotOnly runs steps 4 (the per-path plan) and 5 of the
// snapshot-only plan for a target whose opt-in file, packs and snapshot are
// known.
func (r *run) inspectSnapshotOnly(ctx context.Context, t *target, optIn *config.OptIn, sel config.Selection, tree *snapshot.Tree) {
	plan := decide.Decide(r.decideInput(optIn, sel, tree, nil))
	pairs := decide.Pairs(plan)
	r.reportPlan(t, plan, pairs)
	prs, ok := r.listPRs(ctx, t)
	if !ok {
		return
	}
	r.decidePR(ctx, t, prs, len(pairs) > 0)
}

// ownPR is an open pull request of ours with its marker.
type ownPR struct {
	pr platform.PR
	m  marker.Marker
}

// decidePR sets the outcome from the pull requests on the sync branches.
func (r *run) decidePR(ctx context.Context, t *target, prs []platform.PR, changes bool) {
	res := &t.res
	id := r.identity(t)
	var own []ownPR
	var invalid, foreign, unplaced []platform.PR
	t.noOwnPRs = true
	for _, pr := range prs {
		if pr.Author.ID != "" && slices.Contains(t.prov.ids, pr.Author.ID) {
			t.noOwnPRs = false
		}
		if pr.State != platform.Open || !slices.Contains(r.branches, pr.Head) {
			continue
		}
		m, status := id.Own(pr)
		switch {
		case status == decide.Ours:
			own = append(own, ownPR{pr: pr, m: m})
		case status == decide.OursMarkerInvalid:
			invalid = append(invalid, pr)
		case sameRepo(t, pr):
			foreign = append(foreign, pr)
			if r.couldBeOurs(t, pr) {
				unplaced = append(unplaced, pr)
			}
		default:
			r.forkWarning(t, pr)
		}
	}
	if len(invalid) > 0 {
		res.Outcome, res.Reason, res.PR = report.OutcomeBlocked, "marker-invalid", prRef(invalid[0])
		return
	}
	if len(unplaced) > 0 {
		r.failUnplaced(ctx, t, unplaced[0])
		return
	}
	var mine *ownPR
	for i := range own {
		if mine == nil || (own[i].pr.Head == r.branches[0] && mine.pr.Head != r.branches[0]) {
			mine = &own[i]
		}
	}
	push := ""
	switch {
	case mine != nil:
		push = mine.pr.Head
	case changes:
		push = r.branches[0]
	}
	if push != "" {
		if i := slices.IndexFunc(foreign, func(pr platform.PR) bool { return pr.Head == push }); i >= 0 {
			res.Outcome, res.Reason, res.PR = report.OutcomeBlocked, "branch-in-use", prRef(foreign[i])
			return
		}
	}
	switch {
	case mine != nil && !changes:
		res.Outcome, res.Reason, res.PR = report.OutcomeClosed, "no-diff", prRef(mine.pr)
	case mine != nil && mine.m.Key != res.Key:
		res.Outcome, res.Reason, res.PR = report.OutcomeUpdated, "content", prRef(mine.pr)
	case mine != nil && t.repo.DefaultBranch != "" && mine.pr.Base != t.repo.DefaultBranch:
		res.Outcome, res.Reason, res.PR = report.OutcomeUpdated, "base-renamed", prRef(mine.pr)
	case mine != nil:
		res.Outcome, res.PR = report.OutcomeUnchanged, prRef(mine.pr)
	case !changes:
		res.Outcome = report.OutcomeUnchanged
	default:
		res.Outcome = report.OutcomeOpened
	}
}

// sameRepo reports whether pr's head branch lives in the target repository
// itself (not in a fork).
func sameRepo(t *target, pr platform.PR) bool {
	return pr.HeadRepoID == pr.RepoID || pr.HeadRepoID == t.repo.ID
}

// forkWarning warns about a pull request from another repository that
// carries this hub's marker: it is never touchmark's.
func (r *run) forkWarning(t *target, pr platform.PR) {
	if _, st := marker.Find(pr.Body, r.fps); st == marker.Found {
		t.res.Warnings = append(t.res.Warnings, fmt.Sprintf("#%d from another repository carries this hub's marker; it is not touchmark's pull request", pr.Number))
	}
}

// failUnplaced fails or defers t because pr might be touchmark's but its
// authors could not be looked up (step B).
func (r *run) failUnplaced(ctx context.Context, t *target, pr platform.PR) {
	// The error is phase B's, told again: it goes to the circuit once, then.
	r.record(ctx, t, fmt.Sprintf("tell whether #%d by %s is touchmark's pull request (looking up its authors failed)", pr.Number, pr.Author.Login),
		t.prov.lookupErr, "internal")
	t.res.PR = prRef(pr)
}

// couldBeOurs reports whether pr, not recognized as ours, might be ours
// after all: a Lookup of the provider's authors failed, pr's author is not
// one that resolved, and pr carries no marker of another hub.
func (r *run) couldBeOurs(t *target, pr platform.PR) bool {
	if t.prov.lookupErr == nil || (pr.Author.ID != "" && slices.Contains(t.prov.ids, pr.Author.ID)) {
		return false
	}
	_, st := marker.Find(pr.Body, r.fps)
	return st != marker.Foreign
}

// gateSnapshotOnly runs phase E of the snapshot-only plan: the rollout
// limit in targets.yml order, then the write estimates by outcome.
func (r *run) gateSnapshotOnly(list []*target) {
	opened := 0
	for _, t := range byTargetsOrder(list) {
		if t.res.Outcome != report.OutcomeOpened {
			continue
		}
		if opened >= r.hub.Limits.MaxNewPRsPerRun {
			w := estimate(t.prov.cfg.Type, report.OutcomeOpened, len(r.hub.PR.Labels) > 0 && t.noOwnPRs)
			t.later = pathWrites{git: w, api: w}
			t.res.Outcome, t.res.Reason = report.OutcomeDeferred, "rollout-limit"
			continue
		}
		opened++
	}
	createsLabel := len(r.hub.PR.Labels) > 0
	for _, t := range list {
		t.res.Writes = estimate(t.prov.cfg.Type, t.res.Outcome, createsLabel && t.noOwnPRs)
		r.rep.Cost[t.prov.cfg.ID] += t.res.Writes
	}
}

// byTargetsOrder returns list sorted in targets.yml order: the first entry
// that found each target, then its position in that entry's listing.
func byTargetsOrder(list []*target) []*target {
	ordered := slices.Clone(list)
	slices.SortStableFunc(ordered, func(a, b *target) int {
		return cmp.Or(cmp.Compare(a.first[0], b.first[0]), cmp.Compare(a.first[1], b.first[1]))
	})
	return ordered
}

// estimate is the number of writes an outcome takes on a provider of
// providerType (git path). newLabel adds the label a first pull request
// creates where the platform needs a call of its own for it.
func estimate(providerType string, o report.Outcome, newLabel bool) int {
	switch o {
	case report.OutcomeOpened:
		n := writesOpen
		if providerType == "github" {
			n = writesOpenGitHub
		}
		if newLabel && providerType != "gitlab" {
			n += writesLabel
		}
		return n
	case report.OutcomeUpdated:
		return writesUpdate
	case report.OutcomeClosed:
		return writesClose
	}
	return 0
}
