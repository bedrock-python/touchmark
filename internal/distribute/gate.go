package distribute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// gate runs phase E over the targets of the run and the sweep closes: the
// mass-close guard over every close, then max_new_prs_per_run, then (in
// ModeDryRun) the permission preflight of every work that would write,
// then the write estimates.
func (r *run) gate(ctx context.Context, kept []*target, sweeps []*Work) {
	r.massClose(kept, sweeps)
	r.rollout(kept)
	if r.mode == ModeDryRun {
		r.preflightAll(ctx, kept, sweeps)
	}
	r.duplicates(kept)
	r.estimate(kept, sweeps)
}

// duplicates names on each target's line the duplicates its work closes
// (closed:duplicate): the line reports the pull request the work keeps.
func (r *run) duplicates(kept []*target) {
	for _, t := range kept {
		if t.work == nil {
			continue
		}
		for _, s := range t.work.Decision.Steps {
			if s.Kind == decide.StepClosePR && s.Reason == decide.ReasonDuplicate {
				r.targetWarning(t, fmt.Sprintf("closed:duplicate: #%d from %s duplicates #%d", s.PR, s.Branch, t.work.Decision.PR))
			}
		}
	}
}

// closes reports whether steps close a pull request.
func closes(steps []decide.Step) bool {
	return slices.ContainsFunc(steps, func(s decide.Step) bool { return s.Kind == decide.StepClosePR })
}

// stop replaces w's decision with one that writes nothing, with outcome
// and reason, and reports them on its target.
func (w *Work) stop(outcome report.Outcome, reason string) {
	w.Decision = decide.TargetDecision{Outcome: string(outcome), Reason: reason, PR: w.Decision.PR, Branch: w.Decision.Branch}
	w.NeedPerms = platform.Perms{}
	w.t.res.Outcome, w.t.res.Reason = outcome, reason
}

// massClose applies the mass-close guard: when the run would close more of
// touchmark's pull requests, for every reason, targets and sweep alike, than
// decide.MassCloseAllowed lets it (max(5, max_close_fraction × own open pull
// requests), or allow_mass_close of the operations), nothing is closed:
// every work with a close becomes blocked:mass-close without writes, and a
// run warning says why.
//
// A limited plan whose sweep did not list a provider with targets it
// leaves out cannot count their open pull requests: its count is a part of
// the whole, which would make the guard refuse closes that distribute
// allows. It does not block then; a run warning says that the guard was
// not judged (partialOpen).
func (r *run) massClose(kept []*target, sweeps []*Work) {
	works := r.works(kept, sweeps)
	n := 0
	open := map[string]bool{}
	for _, w := range works {
		for _, s := range w.Decision.Steps {
			if s.Kind == decide.StepClosePR {
				n++
			}
		}
		for _, o := range w.Own {
			if o.PR.State == platform.Open {
				open[sweepKey(w.t.host, w.t.repo.ID, o.PR.Number)] = true
			}
		}
	}
	// A limited plan counts the open pull requests of the targets it leaves
	// out too, as its sweep listed them (scope.go).
	for key := range r.sweptOpen {
		open[key] = true
	}
	ok, limit := decide.MassCloseAllowed(decide.MassCloseInput{
		Closes:      n,
		OwnOpen:     len(open),
		MaxFraction: r.hub.Limits.MaxCloseFraction,
		Allow:       r.massCloseOp(),
		Now:         r.now,
	})
	unjudged := false
	if partial := r.partialOpen(kept); !ok && len(partial) > 0 {
		r.warnf("the mass-close guard is not judged in this plan: it would close %d of the %d open pull requests of touchmark it counts, more than %d, "+
			"but the open pull requests of provider %s in the targets it leaves out were not listed (its sweep is off); "+
			"distribute, and plan --all, count them all", n, len(open), limit, strings.Join(partial, ", "))
		ok, unjudged = true, true
	}
	r.mu.Lock()
	r.gated = gateCounts{closes: n, closeLimit: limit, closesRefused: !ok, ownOpen: len(open), judged: !unjudged}
	r.mu.Unlock()
	if ok {
		return
	}
	r.warnf("blocked:mass-close: this run would close %d of touchmark's %d open pull requests, more than %d; nothing is closed. "+
		"If that is intended, allow it with allow_mass_close in %s", n, len(open), limit, config.OperationsFile)
	for _, w := range works {
		if closes(w.Decision.Steps) {
			w.stop(report.OutcomeBlocked, "mass-close")
		}
	}
}

// partialOpen lists the providers whose open pull requests a limited plan
// cannot all count for the mass-close guard: it leaves out targets of
// theirs, and its sweep did not list them (sweptOpen holds none of theirs).
func (r *run) partialOpen(kept []*target) []string {
	if !r.limited() {
		return nil
	}
	var ids []string
	for _, p := range r.provs {
		if !r.swept[p] && slices.ContainsFunc(kept, func(t *target) bool { return t.prov == p && t.outOfScope }) {
			ids = append(ids, p.cfg.ID)
		}
	}
	return ids
}

// rollout defers the targets that would open a pull request beyond
// limits.max_new_prs_per_run, in targets.yml order (deferred:rollout-limit,
// without writes).
func (r *run) rollout(kept []*target) {
	opened := 0
	for _, t := range byTargetsOrder(kept) {
		if t.res.Outcome != report.OutcomeOpened {
			continue
		}
		if opened < r.hub.Limits.MaxNewPRsPerRun {
			opened++
			continue
		}
		t.res.Outcome, t.res.Reason = report.OutcomeDeferred, "rollout-limit"
		if t.work != nil {
			// A later run opens it: the estimate of the rollout counts its
			// writes (estimate.go).
			t.later = r.writesByPath(t.work)
			t.work.stop(report.OutcomeDeferred, "rollout-limit")
		}
	}
	r.mu.Lock()
	r.gated.opens = opened
	r.mu.Unlock()
}

// gateCounts are what phase E let the run do, for the works a
// re-inspection of phase F makes (regate): the new pull requests it lets
// open, and the closes it counted against their limit.
type gateCounts struct {
	opens         int
	closes        int
	closeLimit    int
	closesRefused bool
	// ownOpen is how many open pull requests of touchmark's the mass-close
	// guard counted, and judged is set once the guard judged the run's
	// closes (not in a plan that processes no target, nor in a limited plan
	// that could not count them all): the Operations of a plan read them
	// (operations.go).
	ownOpen int
	judged  bool
}

// opensPR reports whether w opens a pull request.
func opensPR(w *Work) bool {
	return slices.ContainsFunc(w.Decision.Steps, func(s decide.Step) bool { return s.Kind == decide.StepCreatePR })
}

// closeCount counts the pull requests w closes.
func closeCount(w *Work) int {
	n := 0
	for _, s := range w.Decision.Steps {
		if s.Kind == decide.StepClosePR {
			n++
		}
	}
	return n
}

// regate applies phase E to nw, the work a re-inspection of phase F made in
// place of old: what old counted is given back, then a new pull request
// beyond limits.max_new_prs_per_run defers nw (deferred:rollout-limit), and
// closes beyond the run's mass-close limit, or any close in a run whose
// closes the guard refused, block it (blocked:mass-close). A re-inspection
// never gets past the gate that the inspection before it met.
func (r *run) regate(old, nw *Work) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := &r.gated
	if old != nil {
		if opensPR(old) {
			g.opens--
		}
		g.closes -= closeCount(old)
	}
	if nw == nil {
		return
	}
	if opensPR(nw) {
		if g.opens >= r.hub.Limits.MaxNewPRsPerRun {
			nw.stop(report.OutcomeDeferred, "rollout-limit")
			return
		}
		g.opens++
	}
	if n := closeCount(nw); n > 0 {
		if g.closesRefused || g.closes+n > g.closeLimit {
			nw.stop(report.OutcomeBlocked, "mass-close")
			return
		}
		g.closes += n
	}
}

// preflightAll runs the permission preflight of distribute --dry-run for
// every work that would write, in targets.yml order and then the sweep's:
// Writer.Target with the work's permissions, closed at once. A permission
// the identity lacks blocks the work (blocked:permission:<what>); another
// error fails or defers it by its class. Nothing is written.
func (r *run) preflightAll(ctx context.Context, kept []*target, sweeps []*Work) {
	for _, w := range r.works(byTargetsOrder(kept), sweeps) {
		if w.writes() {
			r.preflight(ctx, w)
		}
	}
}

// preflight checks that the write identity may write what w writes. A
// provider whose circuit is open defers the work without a call, as phase
// F would; a transient failure is tried three times. Warnings name no
// target a public hub hides (the platform's message may).
func (r *run) preflight(ctx context.Context, w *Work) {
	t := w.t
	if reason, why := t.prov.deferral(); reason != "" {
		w.stop(report.OutcomeDeferred, reason)
		r.targetWarning(t, why)
		return
	}
	ctx = throttle.Default(ctx, t.prov.throttle())
	var tw platform.TargetWriter
	err := r.retry(ctx, t.prov, func() error {
		var err error
		tw, err = t.prov.writer.Target(ctx, t.repo, w.NeedPerms)
		return err
	})
	if err == nil {
		_, commits := tw.(platform.Committer)
		if err := tw.Close(); err != nil {
			r.targetWarning(t, "preflight: close the per-target writer: "+err.Error())
		}
		if w.viaAPI && !commits {
			w.stop(report.OutcomeBlocked, "cannot-sign")
			r.targetWarning(t, noAPICommit(t.prov))
		}
		return
	}
	if platform.ClassOf(err) == platform.ClassPermission {
		w.stop(report.OutcomeBlocked, "permission:"+permissionRule(err))
		r.targetWarning(t, "preflight: "+err.Error())
		return
	}
	r.fail(ctx, t, "preflight", err, "internal")
	w.stop(t.res.Outcome, t.res.Reason)
}

// permissionRule names what a permission error says is missing: the Rule
// of its *platform.Error ("contents", "pull-requests", "workflows"), or
// "write" when it names none.
func permissionRule(err error) string {
	var pe *platform.Error
	if errors.As(err, &pe) && pe.Rule != "" {
		return pe.Rule
	}
	return "write"
}

// noAPICommit is the warning of a push that needs the platform's API
// commit when the per-target writer of provider p makes none.
func noAPICommit(p *provider) string {
	return fmt.Sprintf("the push must be signed, and the write identity of provider %s makes no API commits (no signing key either: %sSIGNING_KEY)",
		p.cfg.ID, p.cfg.EnvPrefix)
}

// Writes of steps.
const (
	// writesLabels is the extra call GitHub needs to put labels on a new
	// pull request.
	writesLabels = 1
	// writesRecreate is a branch deleted and pushed again.
	writesRecreate = 2
	// writesAPICommit is a commit through the stage ref: the push to the stage
	// ref, the API commit, and the update of the refs.
	writesAPICommit = 3
)

// writesOf estimates the writes of w's steps: one per step, but none for a
// StepEditPR field ownership skips (Work.idle), two for a
// StepRecreateBranch, three for the commit of a StepPush or
// StepRecreateBranch that goes through the platform's API (Work.viaAPI: the
// stage ref, the commit, the refs), and for a StepCreatePR one more on
// GitHub for its labels and one for the label a first pull request creates
// where the platform needs a call of its own for it (createsLabel: hub.yml
// sets labels on a platform that has them).
func writesOf(w *Work, createsLabel bool) int {
	return writesWith(w, createsLabel, w.viaAPI)
}

// writesWith is writesOf with the commits of the pushes through the
// platform's API (api) or by git.
func writesWith(w *Work, createsLabel, api bool) int {
	typ := w.t.prov.cfg.Type
	commit := 1
	if api {
		commit = writesAPICommit
	}
	n := 0
	for i, s := range w.Decision.Steps {
		switch {
		case s.Kind == decide.StepEditPR && w.idle[i]:
		case s.Kind == decide.StepPush:
			n += commit
		case s.Kind == decide.StepRecreateBranch:
			n += writesRecreate - 1 + commit
		case s.Kind == decide.StepCreatePR:
			n++
			if typ == "github" {
				n += writesLabels
			}
			if createsLabel && w.t.noOwnPRs && typ != "gitlab" {
				n += writesLabel
			}
		default:
			n++
		}
	}
	return n
}

// estimate sets every reported target's estimated writes, the sum over its
// works (writesOf), and sums them per provider into Cost.
func (r *run) estimate(kept []*target, sweeps []*Work) {
	reported := r.reported(kept, sweeps)
	for _, t := range reported {
		t.res.Writes = 0
	}
	for _, w := range r.works(kept, sweeps) {
		w.t.res.Writes += writesOf(w, w.t.prov.createsLabels(r.hub))
	}
	r.recount(reported)
}

// recount sums the writes of the reported targets per provider into Cost.
func (r *run) recount(reported []*target) {
	for id := range r.rep.Cost {
		r.rep.Cost[id] = 0
	}
	for _, t := range reported {
		r.rep.Cost[t.prov.cfg.ID] += t.res.Writes
	}
}
