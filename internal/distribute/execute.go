package distribute

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/report"
)

// execute runs phase F: the works' steps, one write queue per provider,
// targets in the order close → update → open (sweep closes with the closes),
// each target's writes as one block. For every target:
//
//  1. deadline and ctx: past Write.Deadline → deferred:deadline, a
//     cancelled ctx → deferred:interrupted; nothing started is left half
//     done without a report line.
//  2. recheck (right before the first write): RemoteRefs of the branch and
//     Reader.PRs again; if the branch head or the own/foreign open PRs moved
//     since inspection, inspect the target again once (the commit is
//     rebuilt) and use the new decision; a second move → failed:race.
//  3. preflight: Writer.Target(repo, Work.NeedPerms); a permission error →
//     blocked:permission:<what>. Close the TargetWriter after the target.
//  4. steps in order (decide.StepKind):
//     - StepConsumeRecreate: EditPR body = prbody.Untick(recreate) with the
//     marker's recreate_for = Step.Expect;
//     - StepPush: Repo.WithAuth(target writer's Remote header), Push(Branch,
//     Built.Commit, Step.Expect); OK and UpToDate go on; Stale → re-inspect
//     once as in 2 (conflict); Policy → blocked:rules:<rule>; Workflows →
//     blocked:permission:workflows; Unsigned → blocked:cannot-sign;
//     Permission → blocked:permission:push; RateLimited →
//     deferred:rate-limit; Auth → failed:auth; Error → failed:transient
//     when PushResult.Transient(), else failed:git — an outcome that is
//     unknown (timeout) is reconciled with RemoteRefs before anything
//     else;
//     - a StepPush whose commit must be signed through the platform
//     (Work.viaAPI): the API commit of apicommit.go — the commit pushed to
//     the stage ref refs/touchmark/<fp16>/stage, then
//     platform.Committer.Commit with the lease as Expect, which moves the
//     branch to the platform's signed commit of the same tree; an unsigned
//     answer blocks the target (blocked:cannot-sign) and every later one of
//     the provider that needs a signature, a stale lease re-inspects, and
//     the stage ref is deleted whatever happened;
//     - StepRecreateBranch: Push delete with lease Step.Expect, then Push
//     of Built.Commit with lease "" (or its API commit, with Expect "");
//     - StepDeleteBranch: Push delete with lease Step.Expect;
//     - StepCreatePR: body from prbody.Render (Work.Body, Decision.Blocks,
//     the recreate control only when paused, the marker line
//     marker.EncodeFrame of Work.Marker with TitleSet, Body hash and
//     LabelsSet set, in the frame of Caps.Marker), title pr.title, labels
//     pr.labels (none where Caps.NoLabels), draft pr.draft; ErrExists →
//     if the returned PR is ours, re-inspect once, else
//     blocked:branch-in-use;
//     - StepEditPR: decide.PlanPREdit(open PR, marker, DesiredPR{title,
//     rendered body without marker, labels, Step.Base}); write only when it
//     changes something or Step.Content is set (the marker must then carry
//     the new key and changes); report updated:body or updated:title when
//     that is all that changed;
//     - StepClosePR: one EditPR with the body's marker closed = {touchmark,
//     Step.Reason} and state closed;
//     - StepComment: prbody.ClosedComment, DeclinedComment or
//     AutoDeclinedComment by Step.Reason;
//     - StepAck: EditPR of the declined PR: marker ack and optin, body with
//     prbody.AddControl(repropose);
//     - StepRevoke: EditPR of the declined PR: marker revoked;
//     - StepRefreshMarker: EditPR of an open own PR, the body as last read
//     with the marker's optin set to the opt-in file's hash (nothing when
//     it is current already).
//     Before writing any text (body, comment, commit message, branch
//     name), Write.Redact.Contains → failed:secret-exposure, nothing
//     written.
//  5. verify: read the PR back up to 3 times (1, 2, 4 s): open and its head
//     is Built.Commit; otherwise the warning "eventual".
//
// Every mutation is appended to the report's Ops (time, account, target,
// kind, PR, before and after SHA). Errors map to outcomes as in Plan;
// a target's failure never stops the others.
//
// How the rules above are read:
//   - A work runs only when its decision has steps and phase E left its
//     report line alone: a line the gate turned into deferred (rollout-limit)
//     or blocked:mass-close, and a failed, skipped or hidden target, write
//     nothing. A sweep close without steps closes its PR with a comment;
//     a branch deletion needs the target's repository, which a sweep close
//     has not, and is skipped with a warning.
//   - Within a queue, closes come first, then updates (every other work
//     with writes: memory upkeep, blocks, edits), then new pull requests;
//     within each, targets.yml order (first matching entry) and sweep closes
//     after the targets' closes.
//   - Every text of a target (bodies, titles, labels, comments, branch
//     names, the commit message) is rendered and checked before its first
//     write, so a target that would leak a secret, or whose body cannot be
//     rendered, writes nothing at all (failed:secret-exposure,
//     failed:internal).
//   - The recheck reads the default branch when the decision pushes or opens
//     a pull request, the branches the steps push to or delete (their lease)
//     and the classified sync branches of the steps' pull requests. It
//     compares the pull requests listed now with every one inspection listed
//     on the sync branches (Work.listed, whoever opened them): the target
//     moved when one it did not list is open from the target repository on
//     a branch the steps touch, or by one of our authors on a sync branch;
//     when one that matters (own, written to, or from the target repository
//     on a branch the steps touch) changed state or is no longer listed; and
//     when one the steps write to lost or changed its marker, or had its
//     recreate or repropose control ticked or unticked. The marker a write
//     keeps is the listed body's, never inspection's. The first move
//     re-inspects the target (run.reinspect) and passes the new work through
//     phase E's limits again (regate); the new work is checked again, and a
//     second move, or a conflict of a write after the re-inspection, is
//     failed:race.
//   - A consumed recreate needs its control still ticked in the body it
//     edits; a body touchmark keeps (close, ack, revoke, consume) has its
//     human text made inert (prbody.Inert), and every text sent is checked
//     for lines starting with "/" and mentions (prbody.CheckText).
//   - preflight also refuses a push that needs a signature touchmark cannot
//     make (blocked:cannot-sign): no signing key and no API commit, an API
//     commit of the provider came back unsigned earlier in the run, or the
//     per-target writer is no platform.Committer.
//   - A write that failed with a transient error is tried at most 3 times,
//     1 s, 2 s and 4 s apart (with jitter), and a write whose outcome is
//     unknown first reads the platform back: a push reads the branch, a new
//     pull request the open ones on its branch, an edit the pull request. A
//     comment is never retried: a lost comment is better than two. A write
//     the platform is not ready for (platform.ErrNotYet: it applied nothing)
//     is sent again after growing pauses, for 30 s at most.
//   - The provider's throttle.Gate paces the queue: a target's writes start
//     as a block once the minute and hour limits leave room for its estimate
//     (a wait past Write.Deadline defers it: deferred:deadline), and every
//     HTTP write and push of the block waits for the minimum interval and
//     the limits. A rate-limited write pauses the provider (Retry-After,
//     else a minute doubled per limit in a row), is reconciled and made
//     again after the pause; three rate limits in a row, or a pause longer
//     than the Gate allows, put the provider out of budget, and the target
//     and the rest of the queue are deferred:rate-limit. Three targets in a
//     row that fail with auth errors defer the rest of the queue
//     (deferred:provider-down). The queue shares the Gate with phases B to D
//     (circuit.go): a provider phase C found out of budget or down writes
//     nothing.
//   - A comment and a branch deletion are secondary: their failures are
//     target warnings, except an auth error (failed:auth) and the end of the
//     run. A rate limit on one is a warning too (the main write went
//     through, so the target keeps its outcome), and it pauses the provider
//     like any other rate limit.
//   - When the run is cancelled, the write in flight gets 30 s, the target
//     stops before its next write (deferred:interrupted, with what was done
//     in a warning) and no target starts; a target's block of writes has 10
//     minutes from its start (then failed:transient), and never lasts past
//     Write.Deadline by more than the grace (then deferred:deadline).
//   - Where closed pull requests are immutable, a push refused for good
//     that ends the target blocked is followed by the edits that record
//     the current opt-in state in the open own pull requests the writes
//     after it would have edited, when their marker's optin is stale
//     (fallbacks, recordOptIn): secondary writes, rendered and checked with
//     the others before the first write.
//   - An edit step phase C found idle (Work.idle) is skipped; every other
//     edit renders the body as phase C did (Work.humanBody), so that the
//     run writes what its plan showed.
//   - On success the report line takes the decision's outcome, reason and
//     pull request; unchanged with an edit that wrote becomes updated:title
//     when only the title changed, else updated:body. Writes counts the HTTP
//     writes and pushes of the mutations made, as the block metered them (a
//     GitHub pull request is up to three POSTs), and Cost[provider] is
//     corrected from the estimate of phase E to them.
//   - Each target's line goes to Write.Stream once its last work is done
//     (stream.go); ops join the report's Ops sorted by time, then provider,
//     then order of writing.
func (r *run) execute(ctx context.Context, works []*Work) {
	newExecutor(r).run(ctx, works)
}

// Limits of phase F.
const (
	// writeGrace is how long the write in flight may go on once the run is
	// cancelled (SIGINT, SIGTERM).
	writeGrace = 30 * time.Second
	// writesTimeout bounds the block of writes of one target.
	writesTimeout = 10 * time.Minute
	// writeAttempts is how often a write that failed transiently is tried.
	writeAttempts = 3
)

// retryDelays are the waits before the second and third attempt of a
// write, and verifyDelays those before each read-back of a pull request.
var (
	retryDelays  = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	verifyDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
)

// executor runs phase F for one run.
type executor struct {
	r *run
	// reinspect inspects a target that moved again: run.reinspect (tests
	// replace it). execute calls it at most once per target, when the
	// target moved after inspection (the recheck saw another branch head or
	// other open pull requests, a push met a stale lease, an edit
	// conflicted, or CreatePR found an own pull request open already). It
	// inspects w's target afresh, as phase C did (the commit rebuilt on the
	// current B), resets the target's report line to the new verdict, and
	// returns the new work of the same target; nil when the inspection ended
	// the target itself (its report line says why). A sweep close comes back
	// as it is.
	reinspect func(ctx context.Context, w *Work) *Work
	// sleep waits d or until ctx is done; tests make it instant.
	sleep func(ctx context.Context, d time.Duration) error
	// jitter spreads a retry delay; tests make it the identity.
	jitter func(d time.Duration) time.Duration
	// now is the clock of the journal and the deadline.
	now func() time.Time
	// grace and timeout are writeGrace and writesTimeout (tests shorten
	// them).
	grace, timeout time.Duration
}

func newExecutor(r *run) *executor {
	ex := &executor{
		r:       r,
		sleep:   r.sleep,
		jitter:  r.jitter,
		now:     r.d.Now,
		grace:   writeGrace,
		timeout: writesTimeout,
	}
	if ex.sleep == nil {
		ex.sleep = sleepCtx
	}
	if ex.jitter == nil {
		ex.jitter = jitter
	}
	if ex.now == nil {
		ex.now = time.Now
	}
	ex.reinspect = r.reinspect
	return ex
}

// run executes works: one goroutine per provider queue.
func (ex *executor) run(ctx context.Context, works []*Work) {
	queues := ex.queues(works)
	var wg sync.WaitGroup
	for _, q := range queues {
		wg.Go(func() { q.run(ctx) })
	}
	wg.Wait()
	ex.journal(queues)
}

// queues groups works by provider, in the order of the run's providers,
// and orders each queue (queueOrder).
func (ex *executor) queues(works []*Work) []*provQueue {
	byProv := map[*provider]*provQueue{}
	var out []*provQueue
	for i, w := range works {
		if w == nil || w.t == nil || w.t.prov == nil {
			continue
		}
		q := byProv[w.t.prov]
		if q == nil {
			idx := slices.Index(ex.r.provs, w.t.prov)
			if idx < 0 {
				idx = len(ex.r.provs) + len(out)
			}
			q = &provQueue{ex: ex, prov: w.t.prov, index: idx, left: map[*target]int{}, writes: map[*target]int{}, est: map[*target]int{}}
			byProv[w.t.prov] = q
			out = append(out, q)
		}
		q.items = append(q.items, queuedWork{w: w, pos: i})
		if q.left[w.t] == 0 {
			q.est[w.t] = w.t.res.Writes
			q.targets = append(q.targets, w.t)
		}
		q.left[w.t]++
	}
	slices.SortStableFunc(out, func(a, b *provQueue) int { return cmp.Compare(a.index, b.index) })
	for _, q := range out {
		slices.SortStableFunc(q.items, queueOrder)
	}
	return out
}

// queuedWork is a work in its queue, with its position in the works execute got.
type queuedWork struct {
	w   *Work
	pos int
}

// Classes of works, in the order a queue runs them.
const (
	classClose = iota
	classUpdate
	classOpen
)

// workClass is the class of w: a work that opens a pull request, one that
// closes one (a sweep close, a no-diff close, a duplicate closed next to
// the kept pull request), or any other.
func workClass(w *Work) int {
	switch {
	case slices.ContainsFunc(w.Decision.Steps, func(s decide.Step) bool { return s.Kind == decide.StepCreatePR }):
		return classOpen
	case w.Sweep || w.Decision.Outcome == decide.OutcomeClosed || closes(w.Decision.Steps):
		return classClose
	}
	return classUpdate
}

// queueOrder orders a queue: by class, the targets' closes before the
// sweep's, then targets.yml order (first matching entry, then the
// platform's order within it), then the order execute got them.
func queueOrder(a, b queuedWork) int {
	sweep := func(w *Work) int {
		if w.Sweep {
			return 1
		}
		return 0
	}
	return cmp.Or(
		cmp.Compare(workClass(a.w), workClass(b.w)),
		cmp.Compare(sweep(a.w), sweep(b.w)),
		cmp.Compare(a.w.t.first[0], b.w.t.first[0]),
		cmp.Compare(a.w.t.first[1], b.w.t.first[1]),
		cmp.Compare(a.pos, b.pos),
	)
}

// journal adds the queues' ops to the report, sorted by time, then provider,
// then the order they were made, and corrects Cost from the estimates of
// phase E to the writes made.
func (ex *executor) journal(queues []*provQueue) {
	type entry struct {
		op       report.Op
		prov, at int
	}
	var all []entry
	for _, q := range queues {
		for i, op := range q.ops {
			all = append(all, entry{op: op, prov: q.index, at: i})
		}
		id := q.prov.cfg.ID
		for _, t := range q.targets {
			ex.r.rep.Cost[id] += q.writes[t] - q.est[t]
		}
		if ex.r.rep.Cost[id] < 0 {
			ex.r.rep.Cost[id] = 0
		}
	}
	slices.SortStableFunc(all, func(a, b entry) int {
		return cmp.Or(a.op.Time.Compare(b.op.Time), cmp.Compare(a.prov, b.prov), cmp.Compare(a.at, b.at))
	})
	for _, e := range all {
		ex.r.rep.Ops = append(ex.r.rep.Ops, e.op)
	}
}

// stoppedAt returns why no write may start at now: "interrupted" when the
// run was cancelled, "deadline" when its ctx expired or Write.Deadline
// passed; "" otherwise.
func (ex *executor) stoppedAt(ctx context.Context, now time.Time) string {
	switch err := ctx.Err(); {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "interrupted"
	}
	if d := ex.r.d.Write.Deadline; !d.IsZero() && !now.Before(d) {
		return "deadline"
	}
	return ""
}

// now reads the time of the target's writes: its provider's own clock when
// a test gave it one (Deps.clocks), else the executor's.
func (x *targetExec) now() time.Time {
	if c := x.t.prov.clock; c != nil {
		return c()
	}
	return x.ex.now()
}

// graceful returns a context for the writes of one target: it lives on once
// ctx is done, for ex.grace, so that the write in flight can finish. cancel
// releases it.
func (ex *executor) graceful(ctx context.Context) (context.Context, context.CancelFunc) {
	g, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var mu sync.Mutex
	var timer *time.Timer
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		timer = time.AfterFunc(ex.grace, cancel)
	})
	return g, func() {
		stop()
		mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
		cancel()
	}
}

// sleepCtx waits d, or until ctx is done (its error).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// jitter returns a random duration in [d/2, d]: retries of many runners do
// not hit a platform in step.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + rand.N(d-half+1)
}
