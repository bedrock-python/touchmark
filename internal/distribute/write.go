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
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// preflight gets the per-target writer the writes need (execute's step 3):
// Writer.Target with the permissions of the steps and Work.NeedPerms, and
// for pushes the target's repository with the writer's credential. A work
// without writes needs no writer. A push that must be signed and cannot be
// ends the target blocked:cannot-sign before anything is written: no key and
// no API commit (phase C blocks those already), an API commit of the
// provider came back unsigned earlier in the run, or the per-target writer
// makes no API commits (platform.Committer). ok is false when the target
// ended.
func (x *targetExec) preflight(ctx context.Context, acts []writeAct) bool {
	if len(acts) == 0 {
		return true
	}
	need, pushes, api := x.w.NeedPerms, false, false
	for _, a := range acts {
		if a.kind == actPush {
			need.Contents = true
			pushes = pushes || a.commit != ""
			api = api || a.api
		} else {
			need.PRs = true
		}
	}
	for _, s := range workSteps(x.w) {
		need.Workflows = need.Workflows || s.NeedWorkflows
	}
	prov := x.t.prov
	if pushes && (x.w.needSig || prov.cfg.Sign == "always") && prov.signer == nil {
		switch why := prov.unsignedAPI(); {
		case !api:
			x.end(report.OutcomeBlocked, "cannot-sign", fmt.Sprintf("the push must be signed, and touchmark has no signing key for provider %s (%sSIGNING_KEY)",
				prov.cfg.ID, cmp.Or(prov.cfg.EnvPrefix, "TOUCHMARK_")))
			return false
		case why != "":
			x.end(report.OutcomeBlocked, "cannot-sign", "the push must be signed, and "+why)
			return false
		}
	}
	if x.tw != nil && covers(x.perms, need) {
		return true
	}
	x.closeWriter()
	if prov.writer == nil {
		x.end(report.OutcomeFailed, "internal", fmt.Sprintf("provider %s has no write identity", prov.cfg.ID))
		return false
	}
	var tw platform.TargetWriter
	err := x.retryRead(ctx, func() error {
		var err error
		tw, err = prov.writer.Target(ctx, x.t.repo, need)
		return err
	})
	if err != nil {
		if platform.ClassOf(err) == platform.ClassPermission && ruleOf(err) != "archived" {
			x.end(report.OutcomeBlocked, "permission:"+cmp.Or(ruleOf(err), "write"), "get the write credential for the target: "+err.Error())
			return false
		}
		x.fail("get the write credential for the target", err)
		return false
	}
	x.tw, x.perms = tw, need
	if _, ok := tw.(platform.Committer); api && !ok {
		x.end(report.OutcomeBlocked, "cannot-sign", noAPICommit(prov))
		return false
	}
	if !slices.ContainsFunc(acts, func(a writeAct) bool { return a.kind == actPush }) {
		return true
	}
	rem := tw.Remote()
	switch {
	case x.w.Repo == nil:
		x.end(report.OutcomeFailed, "internal", "a push without the target's repository")
		return false
	case rem.URL != "" && rem.URL != x.w.Repo.Remote:
		x.end(report.OutcomeFailed, "internal", "the write remote of the target is not the remote it was read from")
		return false
	}
	x.push = x.w.Repo.WithAuth(gitx.Auth{Header: rem.Header})
	return true
}

// covers reports whether a writer minted with have may do need.
func covers(have, need platform.Perms) bool {
	return (have.Contents || !need.Contents) && (have.PRs || !need.PRs) && (have.Workflows || !need.Workflows)
}

// perform makes the writes in order (execute's step 4). conflict is set when
// a write found the target moved (a stale lease, a conflicting edit, an own
// pull request open already): the caller re-inspects once. ok is false when
// the target ended.
func (x *targetExec) perform(ctx context.Context, acts []writeAct) (conflict string, ok bool) {
	for _, a := range acts {
		if x.interrupted() {
			return "", false
		}
		x.mark = x.block.Writes()
		switch {
		case a.kind == actPush && a.api && a.commit != "":
			conflict, ok = x.doCommit(ctx, a)
		case a.kind == actPush:
			conflict, ok = x.doPush(ctx, a)
		case a.kind == actCreate:
			conflict, ok = x.doCreate(ctx, a)
		case a.kind == actEdit:
			conflict, ok = x.doEdit(ctx, a)
		case a.kind == actComment:
			ok = x.doComment(ctx, a)
		default:
			x.end(report.OutcomeFailed, "internal", fmt.Sprintf("unknown write %d", a.kind))
			return "", false
		}
		if !ok && a.kind == actPush && !a.soft {
			x.recordOptIn(ctx)
		}
		if !ok || conflict != "" {
			return conflict, ok
		}
	}
	return "", true
}

// recordOptIn makes the fallback edits (targetExec.fallbacks) once a push
// was refused and the target ended blocked: the edits after the push will
// not happen, and the open pull request's marker must record the current
// opt-in state before a person declines it, on a platform whose declined
// pull requests no one can edit. They are secondary writes: a failure is a
// warning, and the outcome stays. Nothing once the run is over, or when
// the target ended otherwise (failed, deferred: the provider is out of
// budget or its credential refused).
func (x *targetExec) recordOptIn(ctx context.Context) {
	if x.res.Outcome != report.OutcomeBlocked {
		return
	}
	for _, a := range x.fallback {
		if x.ex.stoppedAt(x.runCtx, x.now()) != "" {
			return
		}
		x.mark = x.block.Writes()
		var pr platform.PR
		err := x.retryWrite(ctx, func() error {
			var err error
			pr, err = x.tw.EditPR(ctx, a.pr, a.edit)
			return err
		}, func() (bool, error) {
			got, ok, err := x.editApplied(ctx, a)
			if ok {
				pr = got
			}
			return ok, err
		})
		if err != nil {
			x.warn(fmt.Sprintf("the opt-in state was not recorded in #%d: %v", a.pr, err))
			continue
		}
		if pr.Number == a.pr {
			x.remember(pr)
		}
		x.op("edit-pr", a.pr, "", "", a.desc)
	}
}

// doPush pushes a commit to a branch, or deletes it, with a lease, and
// classifies a refusal. A push whose outcome is unknown (a transient
// failure) reads the branch before it is tried again, at most three attempts
// in all. A push the server refused for its rate pauses the provider: a
// secondary one (a branch deletion) is then left in place; any other is
// reconciled the same way once the pause is over and tried again, until the
// provider is out of budget.
func (x *targetExec) doPush(ctx context.Context, a writeAct) (string, bool) {
	spec := gitx.PushSpec{Branch: a.branch, Ref: a.ref, Commit: a.commit, Expect: a.expect}
	what := "push to " + a.branch
	switch {
	case a.ref != "" && a.commit == "":
		what = "delete ref " + a.ref
	case a.ref != "":
		what = "push to " + a.ref
	case a.commit == "":
		what = "delete branch " + a.branch
	}
	g := x.t.prov.throttle()
	transient, limited := 0, 0
	for {
		tk := g.TicketFor(ctx, throttle.WriteCall)
		res, err := x.push.Push(ctx, spec)
		if err != nil {
			return "", x.softFail(a, what, err)
		}
		switch res.Status {
		case gitx.PushOK:
			g.Passed(tk)
			x.pushDone(a)
			return "", true
		case gitx.PushUpToDate:
			g.Passed(tk)
			if a.commit != "" && a.ref == "" {
				x.pushed, x.pushedTo = a.commit, a.branch
			}
			return "", true
		case gitx.PushStale:
			g.Passed(tk)
			if a.soft {
				x.warn(fmt.Sprintf("%s is left in place: it moved since touchmark read it (%s)", a.label(), res.Message))
				return "", true
			}
			return fmt.Sprintf("%s moved: the lease on %s failed", a.name(), labelHead(a.expect)), true
		case gitx.PushRateLimited:
			g.Limited(tk, 0)
			limited++
			if a.soft {
				outcome, reason := pushOutcome(res)
				return "", x.pushRefused(a, what, outcome, reason, res.Message)
			}
		case gitx.PushError:
			if !res.Transient() {
				return "", x.pushRefused(a, what, report.OutcomeFailed, "git", res.Message)
			}
			transient++
		default:
			outcome, reason := pushOutcome(res)
			return "", x.pushRefused(a, what, outcome, reason, res.Message)
		}
		applied, moved, err := x.reconcilePush(ctx, a)
		switch {
		case err != nil:
			return "", x.softFail(a, what+": its outcome is unknown and the branch cannot be read", err)
		case applied:
			x.pushDone(a)
			return "", true
		case moved != "" && a.soft:
			x.warn(fmt.Sprintf("%s is left in place: %s", a.label(), moved))
			return "", true
		case moved != "":
			return moved, true
		}
		switch {
		case res.Status == gitx.PushError && transient >= writeAttempts:
			return "", x.pushRefused(a, what, report.OutcomeFailed, "transient", "gave up after 3 attempts: "+res.Message)
		case res.Status == gitx.PushError:
			if err := x.ex.sleep(ctx, x.ex.jitter(retryDelays[transient-1])); err != nil {
				x.fail(what, err)
				return "", false
			}
		case limited >= limitedAttempts:
			outcome, reason := pushOutcome(res)
			return "", x.pushRefused(a, what, outcome, reason, res.Message)
		default:
			// Rate limited: the provider's pause first.
			if err := g.Wait(ctx, x.bound); err != nil {
				return "", x.softFail(a, what+": "+cmp.Or(res.Message, "rate limited"), err)
			}
		}
	}
}

// pushOutcome maps a refused push to an outcome.
func pushOutcome(res gitx.PushResult) (report.Outcome, string) {
	switch res.Status {
	case gitx.PushPolicy:
		return report.OutcomeBlocked, "rules:" + pushRule(res.Message)
	case gitx.PushWorkflows:
		return report.OutcomeBlocked, decide.ReasonPermissionWflw
	case gitx.PushUnsigned:
		return report.OutcomeBlocked, "cannot-sign"
	case gitx.PushPermission:
		return report.OutcomeBlocked, "permission:push"
	case gitx.PushRateLimited:
		return report.OutcomeDeferred, "rate-limit"
	case gitx.PushAuth:
		return report.OutcomeFailed, "auth"
	}
	return report.OutcomeFailed, "git"
}

// pushRule names the rule a push refused by policy met, from the server's
// message: GitHub's rule against force pushes ("Cannot force-push to this
// branch": non-fast-forward, as the preflight names it), its rulesets
// (GH013) and branch protection (GH006, "protected branch"), Bitbucket
// Cloud's branch restrictions ("Permission denied to update branch …",
// which comes with "pre-receive hook declined"), Azure Repos' branch
// policies (TF402455: "Pushes to this branch are not permitted; you must
// use a pull request to update this branch", rule "policy" as RFC-0003
// names it), a pre-receive hook, a deletion rule; "push" when the message
// names none.
func pushRule(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "tf402455"):
		return "policy"
	case strings.Contains(m, "cannot force-push"):
		return "non-fast-forward"
	case strings.Contains(m, "gh013"), strings.Contains(m, "rule violation"):
		return "ruleset"
	case strings.Contains(m, "gh006"), strings.Contains(m, "protected branch"),
		strings.Contains(m, "permission denied to update branch"):
		return "protected-branch"
	case strings.Contains(m, "delet"):
		return "deletion"
	case strings.Contains(m, "pre-receive"):
		return "pre-receive-hook"
	}
	return "push"
}

// pushRefused ends the target with a refused push; a secondary push (a
// branch deletion after a close) only warns, unless its refusal concerns
// the credential.
func (x *targetExec) pushRefused(a writeAct, what string, outcome report.Outcome, reason, msg string) bool {
	why := what + ": " + cmp.Or(msg, "refused")
	if a.soft && reason != "auth" {
		x.warn(fmt.Sprintf("%s is left in place: %s", a.label(), why))
		return true
	}
	x.end(outcome, reason, why)
	x.progress()
	return false
}

// softFail ends the target with the error of a write; a secondary write
// only warns, unless the error concerns the credential or the run's end.
func (x *targetExec) softFail(a writeAct, what string, err error) bool {
	if a.soft && !x.hard(err) {
		x.warn(what + ": " + err.Error())
		return true
	}
	x.fail(what, err)
	return false
}

// hard reports whether err ends the target even for a secondary write: the
// credential was refused, or the run is over.
func (x *targetExec) hard(err error) bool {
	return platform.ClassOf(err) == platform.ClassAuth || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// reconcilePush reads the branch after a push whose outcome is unknown:
// applied when it is at the commit (absent for a deletion), not applied
// when it is still at the lease, moved (why) otherwise.
func (x *targetExec) reconcilePush(ctx context.Context, a writeAct) (applied bool, moved string, err error) {
	ref := a.name()
	var refs map[string]string
	err = x.retryRead(ctx, func() error {
		var err error
		refs, err = x.push.RemoteRefs(ctx, ref)
		return gitFailure(err)
	})
	if err != nil {
		return false, "", err
	}
	switch head := refs[ref]; head {
	case a.commit:
		return true, "", nil
	case a.expect:
		return false, "", nil
	default:
		return false, fmt.Sprintf("%s moved to %s while touchmark pushed", a.name(), labelHead(head)), nil
	}
}

// name is the full name of the ref a push moves: its hidden ref, else
// refs/heads/<branch>.
func (a writeAct) name() string {
	if a.ref != "" {
		return a.ref
	}
	return "refs/heads/" + a.branch
}

// label names the ref a push moves in warnings: "branch <b>" or "ref <hidden
// ref>".
func (a writeAct) label() string {
	if a.ref != "" {
		return "ref " + a.ref
	}
	return "branch " + a.branch
}

// pushDone records a push that moved or deleted a branch or a hidden ref.
func (x *targetExec) pushDone(a writeAct) {
	switch {
	case a.ref != "" && a.commit == "":
		x.op("delete-ref", 0, a.expect, "", a.desc)
	case a.commit == "":
		x.op("delete-branch", 0, a.expect, "", a.desc)
	case a.ref != "":
		x.op("push", 0, a.expect, a.commit, a.desc)
	default:
		x.op("push", 0, a.expect, a.commit, a.desc)
		x.pushed, x.pushedTo = a.commit, a.branch
	}
}

// doCreate opens the pull request: an open one from the branch already is
// re-inspected when it is touchmark's (conflict), and blocks the target when
// it is someone else's (branch-in-use).
func (x *targetExec) doCreate(ctx context.Context, a writeAct) (string, bool) {
	var pr platform.PR
	err := x.retryWrite(ctx, func() error {
		var err error
		pr, err = x.tw.CreatePR(ctx, a.newPR)
		return err
	}, func() (bool, error) {
		found, ok, err := x.openFrom(ctx, a.branch)
		if ok {
			pr = found
		}
		return ok, err
	})
	switch {
	case err == nil:
		x.created = pr.Number
		x.remember(pr)
		x.op("create-pr", pr.Number, "", pr.HeadSHA, fmt.Sprintf("opened #%d", pr.Number))
		return "", true
	case errors.Is(err, platform.ErrExists) && pr.Number > 0:
		x.remember(pr)
		id := decide.Identity{Branches: x.r.branches, Authors: x.t.prov.ids, Fingerprints: x.r.fps}
		if _, status := id.Own(pr); status != decide.NotOurs {
			return fmt.Sprintf("#%d is open from %s already", pr.Number, a.branch), true
		}
		x.end(report.OutcomeBlocked, decide.ReasonBranchInUse, fmt.Sprintf("#%d by %s is open from %s", pr.Number, cmp.Or(pr.Author.Login, "someone"), a.branch))
		x.setPR(pr)
		x.progress()
		return "", false
	}
	x.fail("open the pull request", err)
	return "", false
}

// openFrom returns the open pull request of touchmark's authors from
// branch in the target repository, for a CreatePR whose outcome is
// unknown.
func (x *targetExec) openFrom(ctx context.Context, branch string) (platform.PR, bool, error) {
	var prs []platform.PR
	err := x.retryRead(ctx, func() error {
		var err error
		prs, err = x.t.prov.reader.PRs(ctx, x.t.repo, []string{branch}, x.t.prov.authors)
		return err
	})
	if err != nil {
		return platform.PR{}, false, err
	}
	for _, pr := range prs {
		if pr.State == platform.Open && pr.Head == branch && fromTargetRepo(pr, x.t.repo) && slices.Contains(x.t.prov.ids, pr.Author.ID) {
			return pr, true, nil
		}
	}
	return platform.PR{}, false, nil
}

// doEdit edits a pull request in one request; a conflict re-inspects.
func (x *targetExec) doEdit(ctx context.Context, a writeAct) (string, bool) {
	var pr platform.PR
	err := x.retryWrite(ctx, func() error {
		var err error
		pr, err = x.tw.EditPR(ctx, a.pr, a.edit)
		return err
	}, func() (bool, error) {
		got, ok, err := x.editApplied(ctx, a)
		if ok {
			pr = got
		}
		return ok, err
	})
	switch {
	case err == nil:
		if pr.Number == a.pr {
			x.remember(pr)
		}
		x.op("edit-pr", a.pr, "", "", a.desc)
		if a.field != "" {
			x.edited = a.field
		}
		return "", true
	case platform.ClassOf(err) == platform.ClassConflict:
		return fmt.Sprintf("the edit of #%d conflicted: %v", a.pr, err), true
	}
	x.fail(fmt.Sprintf("edit #%d", a.pr), err)
	return "", false
}

// editApplied reads a pull request back after an edit whose outcome is
// unknown: applied when it shows the body (and state) the edit wrote.
func (x *targetExec) editApplied(ctx context.Context, a writeAct) (platform.PR, bool, error) {
	var prs []platform.PR
	err := x.retryRead(ctx, func() error {
		var err error
		prs, err = x.t.prov.reader.PRs(ctx, x.t.repo, x.r.branches, x.t.prov.authors)
		return err
	})
	if err != nil {
		return platform.PR{}, false, err
	}
	i := slices.IndexFunc(prs, func(pr platform.PR) bool { return pr.Number == a.pr })
	if i < 0 {
		return platform.PR{}, false, nil
	}
	pr := prs[i]
	same := a.edit.Body == nil || sameText(pr.Body, *a.edit.Body)
	same = same && (a.edit.State == nil || pr.State == *a.edit.State)
	return pr, same, nil
}

// sameText reports whether two bodies are the same text, whatever line
// endings and trailing blanks the platform stored.
func sameText(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), " \t\r\n")
	}
	return norm(a) == norm(b)
}

// doComment posts a comment. It is never retried (a lost comment is better
// than two), and its failure is a warning unless the credential was refused
// or the run is over.
func (x *targetExec) doComment(ctx context.Context, a writeAct) bool {
	g := x.t.prov.throttle()
	tk := g.TicketFor(ctx, throttle.WriteCall)
	err := x.tw.Comment(throttle.AsComment(ctx), a.pr, a.comment)
	if err == nil {
		g.Passed(tk)
		x.op("comment", a.pr, "", "", a.desc)
		return true
	}
	if platform.ClassOf(err) == platform.ClassRateLimited {
		// The comment is lost; the provider pauses.
		g.Limited(tk, retryAfterOf(err))
	}
	if platform.ClassOf(err) == platform.ClassTransient && !x.hard(err) {
		x.warn(fmt.Sprintf("the comment on #%d may not have been posted: %v", a.pr, err))
		return true
	}
	return x.softFail(a, fmt.Sprintf("comment on #%d", a.pr), err)
}

// retryWrite runs write, and after a transient failure reads the platform
// back (reconcile): a write that was applied is done, one that was not is
// tried again, 1 and 2 s later, and one whose outcome stays unknown is not
// tried again. A rate-limited write pauses the provider, is reconciled the
// same way once the pause is over, and tried again until the provider is out
// of budget. A write the platform is not ready for (platform.ErrNotYet: it
// applied nothing) is sent again after the pause it names, without a
// reconcile, for notYetWait at most (drivers never wait themselves). A
// reconcile that finds the target moved by someone else (its error of class
// conflict) returns that error, so that the caller decides again as for a
// failed lease, instead of the failure, which would end the target. A call
// the throttle refused ends it at once.
func (x *targetExec) retryWrite(ctx context.Context, write func() error, reconcile func() (bool, error)) error {
	g := x.t.prov.throttle()
	transient, limited, polls := 0, 0, 0
	var waited time.Duration
	for {
		tk := g.TicketFor(ctx, throttle.WriteCall)
		err := write()
		if err == nil {
			g.Passed(tk)
			return nil
		}
		if _, refused := throttle.Refused(err); refused || ctx.Err() != nil {
			return err
		}
		class := platform.ClassOf(err)
		switch {
		case errors.Is(err, platform.ErrNotYet):
			d := max(notYetPolls[min(polls, len(notYetPolls)-1)], retryAfterOf(err))
			if waited+d > notYetWait || x.now().Add(d).After(x.bound) {
				return err
			}
			polls++
			waited += d
			if serr := x.ex.sleep(ctx, d); serr != nil {
				return serr
			}
			continue
		case class == platform.ClassRateLimited:
			g.Limited(tk, retryAfterOf(err))
			limited++
		case class == platform.ClassTransient:
			transient++
		default:
			return err
		}
		applied, rerr := reconcile()
		switch {
		case rerr != nil && platform.ClassOf(rerr) == platform.ClassConflict:
			return rerr
		case rerr != nil:
			if refusal, refused := throttle.Refused(rerr); refused {
				// The limit put the provider out of budget, or its pause
				// outlasts the target's time: the report says so, and the
				// target stays the rate limit's (deferred:rate-limit).
				return fmt.Errorf("%w; then the reconciling read: %s", err, refusal.Error())
			}
			return err
		case applied:
			return nil
		}
		switch {
		case class == platform.ClassTransient && transient >= writeAttempts:
			return err
		case class == platform.ClassTransient:
			if serr := x.ex.sleep(ctx, x.ex.jitter(retryDelays[transient-1])); serr != nil {
				return serr
			}
		case limited >= limitedAttempts:
			return err
		default:
			// Rate limited: the provider's pause first.
			if werr := g.Wait(ctx, x.bound); werr != nil {
				return joinRefusal(werr, err)
			}
		}
	}
}

// notYetWait bounds how long one write the platform is not ready for
// (platform.ErrNotYet) is sent again, and notYetPolls are the pauses
// between its attempts, the last one repeated; a longer RetryAfter of the
// error wins. GitLab registered a push 1.5 to 10 s after it under load
// (measured on CE 19.4).
const notYetWait = 30 * time.Second

var notYetPolls = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second}

// remember records the state of a pull request a write returned.
func (x *targetExec) remember(pr platform.PR) {
	if x.prs == nil {
		x.prs = map[int64]platform.PR{}
	}
	x.prs[pr.Number] = pr
}

// op journals one mutation and counts it as a write, with the HTTP writes
// its calls made since the write in progress started or the last mutation
// (count).
func (x *targetExec) op(kind string, pr int64, before, after, desc string) {
	if n := x.block.Writes(); n > x.mark {
		x.metered += n - x.mark
		x.mark = n
	}
	x.ops = append(x.ops, report.Op{
		Time:    x.now().UTC(),
		Account: cmp.Or(x.t.prov.self.Login, x.t.prov.cfg.Writer),
		Target:  refOf(x.t),
		Kind:    kind,
		PR:      pr,
		Before:  before,
		After:   after,
	})
	x.writes++
	x.done = append(x.done, desc)
}
