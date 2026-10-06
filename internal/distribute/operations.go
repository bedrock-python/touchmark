package distribute

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// opShort is how many hex digits of a commit the Operations show, as the
// heads of operations.yml are usually quoted.
const opShort = 7

// reportOperations sets the report's Operations for a plan or a dry run:
// what each one-off operation does in this run (the plan of a hub pull
// request shows what each entry of operations.yml would do, in its
// Operations section). The entries of operations.yml come first, in the
// file's order (recreate, forget_declines, allow_mass_close,
// adopt_unmarked), then those of a local run's flags (Write.LocalOps). It
// runs after phase E, when every decision is final.
//
//   - recreate: the target the entry names (the matching of decide.OpsFor)
//     and, among its classified sync branches, the one at the entry's head.
//     applies when the decision rebuilds that branch (a push or a rebuild
//     leases on the head) and the entry is why: the branch holds others'
//     commits, or its pull request lacks a valid marker; none when no sync
//     branch is at the head (it moved, or the branch is gone: the entry is
//     spent), when touchmark's commit is on top anyway, or when the
//     decision does not rebuild it (the detail names the outcome).
//   - forget_declines: applies when the decision revokes the pull request's
//     decline (StepRevoke); none when it is no decline in force (already
//     revoked, open, no pull request of touchmark's) or the target writes
//     nothing in this run.
//   - allow_mass_close: expired past its date; else applies when the run
//     closes more pull requests than the guard allows without the entry and
//     no more than its max; none when the closes fit anyway or exceed even
//     the max; unknown when the guard did not judge the run (a plan that
//     processes no target, or a limited plan that could not count them).
//   - adopt_unmarked: expired past its date; else applies when a decision
//     maintains an open pull request without any touchmark comment on a
//     branch alias (multi-gitter's); none when none does.
//
// A target-bound entry whose target was not inspected (skipped, failed,
// deferred before inspection, out of a limited plan's scope) is unknown,
// and so is one whose target the resolve did not find while a provider's
// resolve is incomplete; one whose target no provider found otherwise is
// none. A target a public hub's report does not name (hidden) stays
// unnamed: the entry's target is left out and its effect is unknown.
func (r *run) reportOperations(kept []*target) {
	var out []report.Operation
	for _, src := range []struct {
		ops  *config.Operations
		flag bool
	}{{r.d.Write.Operations, false}, {r.d.Write.LocalOps, true}} {
		ops := src.ops
		if ops == nil {
			continue
		}
		for _, e := range ops.Recreate {
			op := report.Operation{Kind: report.OpRecreate, Flag: src.flag, Target: e.Target, Head: strings.ToLower(e.Head)}
			one := &config.Operations{Version: 1, Recreate: []config.RecreateOp{e}}
			r.opRecreate(&op, r.opTarget(kept, one))
			out = append(out, op)
		}
		for _, e := range ops.ForgetDeclines {
			op := report.Operation{Kind: report.OpForgetDeclines, Flag: src.flag, Target: e.Target, PR: e.PR}
			one := &config.Operations{Version: 1, ForgetDeclines: []config.ForgetOp{e}}
			r.opForget(&op, r.opTarget(kept, one))
			out = append(out, op)
		}
		if e := ops.AllowMassClose; e != nil {
			op := report.Operation{Kind: report.OpAllowMassClose, Flag: src.flag, Max: e.Max, Until: e.Until}
			r.opMassClose(&op, e)
			out = append(out, op)
		}
		if e := ops.AdoptUnmarked; e != nil {
			op := report.Operation{Kind: report.OpAdoptUnmarked, Flag: src.flag, Until: e.Until}
			r.opAdopt(&op, e, kept)
			out = append(out, op)
		}
	}
	r.rep.Operations = out
}

// opTarget returns the first target of kept that the single entry of one
// names (decide.OpsFor, by every provider that found the target and every
// path it goes by); nil when none does.
func (r *run) opTarget(kept []*target, one *config.Operations) *target {
	for _, t := range kept {
		for _, id := range t.providers {
			for _, path := range t.paths() {
				o := decide.OpsFor(one, r.hub, r.targets, id, path, r.now)
				if len(o.RecreateHeads) > 0 || len(o.Forget) > 0 {
					return t
				}
			}
		}
	}
	return nil
}

// opInspected checks the target t of a target-bound entry: it fills op's
// provider, or the effect and detail when the run cannot tell what the
// entry does (t nil, hidden, or not inspected). It returns t's work when
// the entry can be judged.
func (r *run) opInspected(op *report.Operation, t *target) *Work {
	switch {
	case t == nil:
		op.Effect, op.Detail = report.EffectNone, "names no target of this run: it does nothing; remove it"
		if id := r.incompleteResolve(); id != "" {
			op.Effect, op.Detail = report.EffectUnknown, "names no target this run found, and provider "+id+" did not list all its repositories"
		}
		return nil
	case t.hidden:
		op.Target, op.PR, op.Head = "", 0, ""
		op.Effect, op.Detail = report.EffectUnknown, "names a target that is not public, which a public hub's report does not name"
		return nil
	}
	op.Provider = t.prov.cfg.ID
	switch {
	case t.outOfScope:
		op.Effect, op.Detail = report.EffectUnknown, "the target is out of this plan's scope (plan --all inspects it)"
	case t.work == nil && t.res.Outcome == "":
		op.Effect, op.Detail = report.EffectUnknown, "the target was not inspected"
	case t.work == nil:
		op.Effect, op.Detail = report.EffectUnknown, "the target was not inspected: "+outcomeText(t.res.Outcome, t.res.Reason)
	default:
		return t.work
	}
	return nil
}

// incompleteResolve returns the id of the first provider whose resolve is
// incomplete or that failed, "" when every provider listed everything.
func (r *run) incompleteResolve() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.rep.Providers {
		if !p.ResolveComplete || p.Error != "" {
			return p.ID
		}
	}
	return ""
}

// outcomeText is "outcome" or "outcome:reason".
func outcomeText(outcome report.Outcome, reason string) string {
	if reason == "" {
		return string(outcome)
	}
	return string(outcome) + ":" + reason
}

// opRecreate judges a recreate entry for its target t (reportOperations).
func (r *run) opRecreate(op *report.Operation, t *target) {
	w := r.opInspected(op, t)
	if w == nil {
		return
	}
	head := op.Head
	var b decide.Branch
	found := false
	for _, c := range append([]decide.Branch{w.Branch}, sortedBranches(w.Aliases)...) {
		if c.Name != "" && c.Head == head {
			b, found = c, true
			break
		}
	}
	short := decide.Short(head, opShort)
	if !found {
		op.Effect = report.EffectNone
		switch {
		case w.Branch.Name == "":
			op.Detail = "the target needs no change and has no open pull request of touchmark's: it does nothing; remove it"
		case w.Branch.State == decide.BranchAbsent:
			op.Detail = fmt.Sprintf("no sync branch is at %s (%s does not exist): it does nothing; remove it", short, w.Branch.Name)
		default:
			op.Detail = fmt.Sprintf("no sync branch is at %s (%s is at %s): it does nothing; remove it",
				short, w.Branch.Name, decide.Short(w.Branch.Head, opShort))
		}
		return
	}
	if n := w.Decision.PR; n > 0 {
		op.PR = n
	}
	rebuilds := slices.ContainsFunc(w.Decision.Steps, func(s decide.Step) bool {
		return (s.Kind == decide.StepPush || s.Kind == decide.StepRecreateBranch) && s.Branch == b.Name && s.Expect == head
	})
	// A pull request without a valid marker on the branch is one the entry
	// takes over: without the entry it blocks the target.
	adopted := w.Open != nil && w.Open.PR.Head == b.Name && w.Open.Marker.Key == ""
	switch {
	case rebuilds && (b.State != decide.BranchRewritable || adopted):
		op.Effect = report.EffectApplies
		op.Detail = fmt.Sprintf("head %s matches: rebuilds %s on the default branch", short, b.Name)
		if b.State != decide.BranchRewritable {
			op.Detail += " and drops what others added"
			if b.Detail != "" {
				op.Detail += " (" + b.Detail + ")"
			}
		}
		if adopted {
			op.Detail += fmt.Sprintf("; takes over %s, whose marker is missing or broken", r.prName(t, w.Open.PR.Number))
		}
	case b.State == decide.BranchRewritable:
		op.Effect, op.Detail = report.EffectNone, fmt.Sprintf("head %s matches, but %s holds touchmark's commit on top and moves as it would without the entry: remove it",
			short, b.Name)
	default:
		op.Effect, op.Detail = report.EffectNone, fmt.Sprintf("head %s matches, but this run does not rebuild %s: %s",
			short, b.Name, outcomeText(report.Outcome(w.Decision.Outcome), w.Decision.Reason))
	}
}

// sortedBranches returns the branches of m by name.
func sortedBranches(m map[string]decide.Branch) []decide.Branch {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]decide.Branch, 0, len(names))
	for _, name := range names {
		out = append(out, m[name])
	}
	return out
}

// opForget judges a forget_declines entry for its target t
// (reportOperations).
func (r *run) opForget(op *report.Operation, t *target) {
	w := r.opInspected(op, t)
	if w == nil {
		return
	}
	n := op.PR
	revokes := slices.ContainsFunc(w.Decision.Steps, func(s decide.Step) bool { return s.Kind == decide.StepRevoke && s.PR == n })
	i := slices.IndexFunc(w.Own, func(o decide.OwnPR) bool { return o.PR.Number == n })
	op.Effect = report.EffectNone
	switch {
	case revokes:
		op.Effect, op.Detail = report.EffectApplies, fmt.Sprintf("revokes the decline of %s: its content may be proposed again", r.prName(t, n))
	case slices.Contains(w.Memory.ToRevoke, n):
		op.Detail = fmt.Sprintf("would revoke the decline of %s, but the target writes nothing in this run: %s",
			r.prName(t, n), outcomeText(report.Outcome(w.Decision.Outcome), w.Decision.Reason))
	case i < 0:
		op.Detail = fmt.Sprintf("%s is no pull request of touchmark's on the target's sync branches: it does nothing; remove it", r.prName(t, n))
	case w.Own[i].PR.State == platform.Open:
		op.Detail = fmt.Sprintf("%s is open: there is no decline to forget", r.prName(t, n))
	case w.Own[i].Marker.Data.Revoked:
		op.Detail = fmt.Sprintf("the decline of %s is revoked already: remove the entry", r.prName(t, n))
	default:
		op.Detail = fmt.Sprintf("%s holds no decline (merged, or closed by touchmark or one of its accounts): it does nothing; remove it", r.prName(t, n))
	}
}

// prName is "#n", or "!n" for a GitLab merge request.
func (r *run) prName(t *target, n int64) string {
	if t != nil && t.prov.cfg.Type == "gitlab" {
		return fmt.Sprintf("!%d", n)
	}
	return fmt.Sprintf("#%d", n)
}

// opMassClose judges an allow_mass_close entry (reportOperations).
func (r *run) opMassClose(op *report.Operation, e *config.MassCloseOp) {
	if !e.Active(r.now) {
		op.Effect, op.Detail = report.EffectExpired, "its date has passed: it does nothing; remove it"
		return
	}
	r.mu.Lock()
	g := r.gated
	r.mu.Unlock()
	if !g.judged {
		op.Effect, op.Detail = report.EffectUnknown, "the mass-close guard did not judge this run's closes"
		return
	}
	_, base := decide.MassCloseAllowed(decide.MassCloseInput{Closes: g.closes, OwnOpen: g.ownOpen, MaxFraction: r.hub.Limits.MaxCloseFraction, Now: r.now})
	n, open := g.closes, g.ownOpen
	switch {
	case n <= base:
		op.Effect, op.Detail = report.EffectNone, fmt.Sprintf("not needed: this run closes %d of touchmark's %d open pull requests, within the guard's limit of %d", n, open, base)
	case n <= e.Max:
		op.Effect, op.Detail = report.EffectApplies, fmt.Sprintf("lets this run close %d of touchmark's %d open pull requests, more than the guard's limit of %d", n, open, base)
	default:
		op.Effect, op.Detail = report.EffectNone, fmt.Sprintf("this run would close %d of touchmark's %d open pull requests, more than its max: "+
			"the guard stops every close (blocked:mass-close)", n, open)
	}
}

// opAdopt judges an adopt_unmarked entry (reportOperations): the targets
// whose decision maintains an open pull request without any touchmark
// comment on a branch alias.
func (r *run) opAdopt(op *report.Operation, e *config.UntilOp, kept []*target) {
	if !e.Active(r.now) {
		op.Effect, op.Detail = report.EffectExpired, "its date has passed: it does nothing; remove it"
		return
	}
	var adopted []string
	for _, t := range kept {
		w := t.work
		if w == nil || t.hidden || w.Open == nil || !w.Open.Alias || !touches(w.Decision, w.Open.PR.Number) {
			continue
		}
		if _, status := marker.Find(w.Open.PR.Body, nil); status != marker.None {
			continue
		}
		adopted = append(adopted, t.prov.cfg.ID+":"+t.repo.Path+" "+r.prName(t, w.Open.PR.Number))
	}
	switch {
	case len(adopted) > 0:
		list := strings.Join(adopted[:min(len(adopted), 3)], ", ")
		if n := len(adopted) - 3; n > 0 {
			list += fmt.Sprintf(" and %d more", n)
		}
		what := "pull request"
		if len(adopted) > 1 {
			what += "s"
		}
		op.Effect, op.Detail = report.EffectApplies, fmt.Sprintf("adopts %d unmarked %s on branch aliases: %s", len(adopted), what, list)
	case r.limited():
		op.Effect, op.Detail = report.EffectUnknown, "adopts nothing among the targets this plan inspects (plan --all inspects every target)"
	default:
		op.Effect, op.Detail = report.EffectNone, "no open pull request without a marker on a branch alias is adopted in this run"
	}
}
