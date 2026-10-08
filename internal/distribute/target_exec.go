package distribute

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// targetExec carries out the work of one target (execute's steps 1–5).
type targetExec struct {
	q   *provQueue
	ex  *executor
	r   *run
	w   *Work
	t   *target
	res *report.DeliveryTarget
	// runCtx is the run's context: once it is done, no write starts.
	runCtx context.Context

	// tw is the per-target writer and perms what it was minted with; push
	// is the target's repository with tw's credential.
	tw    platform.TargetWriter
	perms platform.Perms
	push  *gitx.TargetRepo
	// reinspected is set once the target was inspected again.
	reinspected bool
	// attempted is set once the target reached its first platform call.
	attempted bool

	// block is the target's block of writes on its provider's Gate, which
	// meters every call of the target (nil before it starts); bound is when
	// the block ends: no call starts after it.
	block *throttle.Block
	bound time.Time

	// writes counts the mutations made and ops journals them; metered
	// counts the HTTP writes and pushes their calls made (the block's, up to
	// the last mutation), and mark is the block's count when the write in
	// progress started.
	writes, metered, mark int
	ops                   []report.Op
	// done describes the mutations made, for a target stopped half way.
	done []string

	// prs holds the latest known state of the target's pull requests by
	// number (inspection, recheck, write responses), and marks their
	// markers.
	prs   map[int64]platform.PR
	marks map[int64]marker.Marker
	// created is the pull request CreatePR opened (0 for none), pushed the
	// commit a push left at the head of a branch ("" for none) and pushedTo
	// that branch.
	created  int64
	pushed   string
	pushedTo string
	// edited is "title" or "body" when an edit of the open pull request
	// wrote, for unchanged decisions (updated:title, updated:body).
	edited string
	// fallback are the edits that record the opt-in state should a push be
	// refused (fallbacks, recordOptIn).
	fallback []writeAct
}

func newTargetExec(q *provQueue, w *Work) *targetExec {
	return &targetExec{q: q, ex: q.ex, r: q.ex.r, w: w, t: w.t, res: &w.t.res}
}

// run carries out the work; it always leaves a report line.
func (x *targetExec) run(ctx context.Context) {
	x.runCtx = ctx
	defer x.closeWriter()
	defer func() {
		if v := recover(); v != nil {
			x.end(report.OutcomeFailed, "internal", fmt.Sprintf("internal error: %v", v))
		}
		if x.attempted {
			x.q.settle(x.res)
		}
	}()
	if !x.runnable(x.w) {
		return
	}
	if reason, why := x.q.deferral(); reason != "" {
		x.end(report.OutcomeDeferred, reason, why)
		return
	}
	if reason := x.ex.stoppedAt(ctx, x.now()); reason != "" {
		x.end(report.OutcomeDeferred, reason, "the run stopped before touchmark wrote to this target; the next run does it")
		return
	}
	// The block of writes starts once the provider's budget has room for it,
	// and before the deadline: its time runs from then, and the per-target
	// credential is minted after that wait, right before the writes.
	block, err := x.t.prov.throttle().Block(ctx, writesOf(x.w, x.t.prov.createsLabels(x.r.hub)))
	if err != nil {
		outcome, reason := x.outcomeOf(err)
		x.end(outcome, reason, "wait for the provider's budget: "+err.Error())
		return
	}
	gctx, cancel := x.ex.graceful(ctx)
	defer cancel()
	tctx, cancelTarget := context.WithTimeout(gctx, x.ex.timeout)
	defer cancelTarget()
	now := x.now()
	x.bound = now.Add(x.ex.timeout)
	if d := x.r.d.Write.Deadline; !d.IsZero() {
		// No write starts past the deadline; the one in flight then has the
		// grace a cancelled run gives it, so a target started just before the
		// deadline cannot outlive the CI job by its whole block of writes
		// The time left is read on the run's clock.
		var cancelDeadline context.CancelFunc
		tctx, cancelDeadline = context.WithTimeout(tctx, d.Sub(now)+x.ex.grace)
		defer cancelDeadline()
		if end := d.Add(x.ex.grace); end.Before(x.bound) {
			x.bound = end
		}
	}
	block.Bound(x.bound)
	x.block = block
	x.loop(throttle.With(tctx, block))
}

// count is the writes the target made: the HTTP writes and pushes its block
// metered for the mutations that went through (a GitHub pull request is up
// to three POSTs; a write the platform refused counts in the provider's
// budget, not here), never fewer than the mutations journaled.
func (x *targetExec) count() int {
	return max(x.writes, x.metered)
}

// loop runs the steps of execute from the recheck on, once more after a
// re-inspection.
func (x *targetExec) loop(ctx context.Context) {
	for {
		if x.interrupted() {
			return
		}
		x.attempted = true
		moved, ok := x.recheck(ctx)
		if !ok {
			return
		}
		if moved != "" {
			if !x.again(ctx, moved) {
				return
			}
			continue
		}
		acts, ok := x.prepare(ctx)
		if !ok {
			return
		}
		if !x.preflight(ctx, acts) {
			return
		}
		conflict, ok := x.perform(ctx, acts)
		if !ok {
			return
		}
		if conflict != "" {
			if !x.again(ctx, conflict) {
				return
			}
			continue
		}
		x.verify(ctx)
		x.succeed()
		return
	}
}

// runnable reports whether w has writes to make: steps, and a report line
// phase E left alone. A target a public hub does not name has none, but
// for a sweep close of its pull request.
func (x *targetExec) runnable(w *Work) bool {
	if w == nil || w.t == nil || (w.t.hidden && !w.Sweep) {
		return false
	}
	res := &w.t.res
	switch {
	case res.Outcome == report.OutcomeFailed, res.Outcome == report.OutcomeSkipped:
		return false
	case res.Outcome == report.OutcomeBlocked && res.Reason == "mass-close":
		return false
	case res.Outcome == report.OutcomeDeferred && w.Decision.Outcome != decide.OutcomeDeferred:
		return false
	}
	return len(workSteps(w)) > 0
}

// workSteps returns the steps of w: its decision's, or for a sweep close
// without steps the close and its comment.
func workSteps(w *Work) []decide.Step {
	if len(w.Decision.Steps) > 0 || !w.Sweep || w.SweepPR == nil {
		return w.Decision.Steps
	}
	reason := cmp.Or(w.Decision.Reason, w.t.res.Reason, decide.ReasonTargetDropped)
	pr := w.SweepPR.PR
	return []decide.Step{
		{Kind: decide.StepClosePR, PR: pr.Number, Branch: pr.Head, Reason: reason},
		{Kind: decide.StepComment, PR: pr.Number, Branch: pr.Head, Reason: decide.OutcomeClosed},
	}
}

// again re-inspects the target after it moved (why), once: it reports
// whether there is a new work to carry out. A work the re-inspection hands
// back unchanged (a sweep close) is tried once more as it is.
func (x *targetExec) again(ctx context.Context, why string) bool {
	if x.reinspected {
		x.end(report.OutcomeFailed, "race", "the target changed again after touchmark inspected it anew ("+why+"); the next run decides")
		return false
	}
	x.reinspected = true
	x.closeWriter()
	nw := x.ex.reinspect(ctx, x.w)
	switch {
	case nw == nil:
		if x.res.Outcome == report.OutcomeFailed && x.res.Reason == "race" {
			x.warn("the target changed after touchmark inspected it (" + why + "); the next run decides")
		}
		x.progress()
		return false
	case nw.t != x.t:
		x.end(report.OutcomeFailed, "internal", "the re-inspection returned the work of another target")
		return false
	case nw != x.w:
		x.warn("the target changed after touchmark inspected it (" + why + "); it was inspected again")
	}
	x.w = nw
	x.prs, x.marks = nil, nil
	if !x.runnable(nw) {
		x.progress()
		return false
	}
	return true
}

// interrupted reports whether the run ended, and then ends the target:
// deferred, with what was done.
func (x *targetExec) interrupted() bool {
	reason := x.ex.stoppedAt(x.runCtx, x.now())
	if reason == "" {
		return false
	}
	why := "the run stopped before touchmark wrote to this target; the next run does it"
	if x.writes > 0 {
		why = "the run stopped after touchmark " + strings.Join(x.done, ", ") + "; the next run completes the target"
	}
	x.end(report.OutcomeDeferred, reason, why)
	return true
}

// progress adds a warning with what was done, when anything was.
func (x *targetExec) progress() {
	if x.writes > 0 {
		x.warn("touchmark " + strings.Join(x.done, ", ") + " before it stopped")
	}
}

// end sets the report line's outcome and adds why as a warning.
func (x *targetExec) end(outcome report.Outcome, reason, why string) {
	x.res.Outcome, x.res.Reason = outcome, reason
	if why != "" {
		x.warn(why)
	}
}

// fail ends the target by what err says of the call what: its class, or
// the run's end (see outcomeOf); what was done is added.
func (x *targetExec) fail(what string, err error) {
	outcome, reason := x.outcomeOf(err)
	x.end(outcome, reason, what+": "+err.Error())
	x.progress()
}

// outcomeOf maps an error of the target's platform or git calls to an
// outcome: a call the throttle refused defers the target by the refusal's
// reason.
func (x *targetExec) outcomeOf(err error) (report.Outcome, string) {
	if refusal, ok := throttle.Refused(err); ok {
		return report.OutcomeDeferred, refusal.Reason
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The run's end (its ctx, or Write.Deadline, which bounds the
		// target's writes too) defers the target; the target's own budget
		// ran out otherwise.
		if reason := x.ex.stoppedAt(x.runCtx, x.now()); reason != "" {
			return report.OutcomeDeferred, reason
		}
		return report.OutcomeFailed, "transient"
	}
	switch platform.ClassOf(err) {
	case platform.ClassRateLimited:
		return report.OutcomeDeferred, "rate-limit"
	case platform.ClassAuth:
		return report.OutcomeFailed, "auth"
	case platform.ClassPermission:
		switch rule := ruleOf(err); rule {
		case "":
			return report.OutcomeFailed, "access"
		case "archived":
			return report.OutcomeBlocked, "archived"
		default:
			return report.OutcomeBlocked, "permission:" + rule
		}
	case platform.ClassPolicy:
		return report.OutcomeBlocked, "rules:" + cmp.Or(ruleOf(err), "policy")
	case platform.ClassNotFound:
		return report.OutcomeFailed, "access"
	case platform.ClassTransient:
		return report.OutcomeFailed, "transient"
	case platform.ClassConflict:
		return report.OutcomeFailed, "race"
	case platform.ClassUnknown:
		var ge *gitx.Error
		if errors.As(err, &ge) {
			return report.OutcomeFailed, "git"
		}
	}
	return report.OutcomeFailed, "internal"
}

// ruleOf returns the rule a platform error names, in a form a report
// reason can carry: letters, digits, '.', '_' and '-', at most 40 bytes.
func ruleOf(err error) string {
	var pe *platform.Error
	if !errors.As(err, &pe) {
		return ""
	}
	rule := strings.Map(func(r rune) rune {
		if r < 0x80 && (r == '.' || r == '_' || r == '-' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return r
		}
		return -1
	}, pe.Rule)
	if len(rule) > 40 {
		rule = rule[:40]
	}
	return rule
}

// warn adds a warning to the target's report line; none for a target a
// public hub does not name, as platform messages may name it.
func (x *targetExec) warn(why string) {
	if why != "" && !x.t.hidden && !slices.Contains(x.res.Warnings, why) {
		x.res.Warnings = append(x.res.Warnings, why)
	}
}

// succeed sets the report line of a work whose writes all went through.
func (x *targetExec) succeed() {
	dec := x.w.Decision
	if dec.Outcome != "" {
		x.res.Outcome, x.res.Reason = report.Outcome(dec.Outcome), dec.Reason
		if dec.Outcome == decide.OutcomeUnchanged && x.edited != "" {
			x.res.Outcome, x.res.Reason = report.OutcomeUpdated, x.edited
		}
	}
	n := x.created
	if n == 0 {
		n = dec.PR
	}
	if n == 0 && x.w.Sweep && x.w.SweepPR != nil {
		n = x.w.SweepPR.PR.Number
	}
	if pr, ok := x.prs[n]; ok && n > 0 {
		x.setPR(pr)
	} else if n > 0 && (x.res.PR == nil || x.res.PR.Number != n) {
		x.setPR(platform.PR{Number: n})
	}
}

// setPR names pr on the report line, unless the target is one a public hub
// does not name (the URL would).
func (x *targetExec) setPR(pr platform.PR) {
	if !x.t.hidden {
		x.res.PR = prRef(pr)
	}
}

// closeWriter closes the per-target writer, which revokes its token.
func (x *targetExec) closeWriter() {
	if x.tw == nil {
		return
	}
	if err := x.tw.Close(); err != nil {
		x.warn("release the target's write credential: " + err.Error())
	}
	x.tw, x.push, x.perms = nil, nil, platform.Perms{}
}
