package distribute

// Run extends the snapshot-only plan with branch history, memory, the
// commit, the sweep, the mass-close guard and, in ModeDistribute, phase F
// (execute.go).

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// Mode is what a run does.
type Mode uint8

const (
	// ModePlan reads with the read identity and writes nothing (phases
	// A–E, G). Provider.Writer is ignored.
	ModePlan Mode = iota
	// ModeDryRun is distribute --dry-run: the write identity and every
	// check (preflight included), no write. Its report equals the plan's
	// for the same state (TestPropertyDistribute checks it).
	ModeDryRun
	// ModeDistribute runs every phase, F included.
	ModeDistribute
)

// Repos is a snapshot source that also hands out each target's isolated git
// repository for history, commits and pushes; snapshot.GitSource is one.
// ModeDryRun and ModeDistribute need Deps.Snapshots to implement Repos, and
// a plan without it is the snapshot-only plan (tests use the fake platform
// in git mode with a snapshot.GitSource).
type Repos interface {
	snapshot.Source
	Repo(ctx context.Context, repo platform.Repo, remote platform.Remote) (*gitx.TargetRepo, error)
	// Release frees the target's repository once its work is done (its
	// directory is removed); releasing one that has none does nothing.
	Release(repo platform.Repo) error
}

// WriteDeps are what a run needs beyond a plan.
type WriteDeps struct {
	// Operations is .touchmark/operations.yml of the default branch (nil
	// when absent). LocalOps are the same operations given as flags in a
	// local run (the CLI refuses them in CI); both apply.
	Operations *config.Operations
	LocalOps   *config.Operations
	// HubBlobs opens a committed hub blob by id (the pack contents). With
	// Repos it is required, in a plan too: phase C builds every target's
	// commit in its private repository.
	HubBlobs func(oid string) (io.ReadCloser, error)
	// HubCommitTime is the committer date of HubCommit; commits are dated
	// max(HubCommitTime, B's date).
	HubCommitTime time.Time
	// Signers are SSH signing keys by provider id
	// (TOUCHMARK_<ID>_SIGNING_KEY), nil entries for none.
	Signers map[string]*sshsig.Signer
	// Redact holds every secret: text holding one is never written to a
	// platform (failed:secret-exposure) or printed; the lines of Stream are
	// masked with it.
	Redact *redact.Registry
	// Stream receives one JSON line per finished target
	// (touchmark-report.jsonl, written as the run goes); nil for none.
	Stream io.Writer
	// Deadline stops starting new targets (deferred:deadline); zero for
	// none. Phase C starts no inspection past three quarters of the time
	// left at the start (the rest is phase F's), and a target's writes never
	// last past Deadline by more than the write grace.
	Deadline time.Time
	// Intro is the content of pr.intro_file ("" for none); it must pass
	// prbody.CheckIntro.
	Intro string
	// HubURL is the hub's web URL for PR bodies ("" hides links, as
	// pr.link_hub decides per target visibility).
	HubURL string
	// CanWorkflows tells, per provider id, whether its write identity may
	// get the Workflows permission (GitHub App with Workflows RW): the
	// assumption for a target whose writer does not say (Rules.
	// WorkflowsKnown of platform.Preflighter overrides it).
	CanWorkflows map[string]bool
	// HubIsAncestor reports whether the hub commit ancestor is an ancestor of
	// commit in the hub's history (true for the same commit). The per-target
	// guard I8 asks it about the Touchmark-Hub-Commit trailer of each sync
	// branch it reads: a trailer that names a descendant of HubCommit is a
	// newer run's, and the target is skipped:superseded. A commit the hub's
	// clone does not have is no descendant (false, nil): an unknown hub commit
	// does not block. nil leaves the per-target guard out.
	HubIsAncestor func(ctx context.Context, ancestor, commit string) (bool, error)
}

// Run runs a plan, a dry run or distribute. Plan(ctx, d) is
// Run(ctx, d, ModePlan).
//
// Phases A and B are Plan's (distribute.go). In ModeDryRun and
// ModeDistribute the provider's Writer reads too: its Self is the commit
// author and must be the writer hub.yml names (else an error wrapping
// ErrWriterMismatch, and nothing is inspected); Caps come from its Probe,
// and the signing key from Write.Signers.
//
// In every mode phase C adds to Plan's steps 1–4 (classify, opt-in, packs,
// snapshot and per-path plan), when Deps.Snapshots implements Repos:
//  5. PRs: Reader.PRs(repo, [branch]+aliases, authors) in every state;
//     decide.Identity sorts them into own PRs (decide.OwnPR, with markers),
//     foreign open PRs from the target repository, and marker-invalid open
//     ones (closed ones without a valid marker tell nothing); a PR that may
//     be ours while a Lookup failed fails or defers the target as in Plan.
//  6. history (only when a branch can move: D is not empty, or an own or
//     marker-invalid PR is open): Repos.Repo; FetchBranch of the sync
//     branch and of each alias that carries such an open PR (depth
//     decide.MaxHistory+1, after the base, which the snapshot fetched), each
//     read at once: FirstParentLog; the pairs of the newest commit with our
//     trailer (Hc) from DiffTree(HcParent, Hc); IsCleanMerge for merges
//     after Hc. Then base ancestry of Hc's first parent and of those
//     merges' second parents (DeepenSince back to a day before the oldest,
//     then IsAncestor; a proof that cannot be made is TriUnknown, with a
//     warning, but a transport failure of the fetch — a class other than
//     Unknown, or the run's end — fails the target like any read). Each
//     branch read whose Touchmark-Hub-Commit names a strict descendant of
//     HubCommit (Write.HubIsAncestor) was written by a newer run:
//     skipped:superseded, nothing written (I8 per target).
//     decide.ClassifyBranch per branch; the workflows difference between E
//     and B of the branch that would move (DiffTree, paths under
//     decide.WorkflowsDir; only where Caps.WorkflowPerm); with
//     adopt_unmarked, LegacyRewritable over DiffTree(merge-base, H) for a
//     foreign branch.
//  7. memory: decide.BuildMemory over own closed PRs with the opt-in hash
//     (config.OptIn.Hash), paths local, retired-local or ignored in the
//     per-path plan, operations forget_declines and ticked repropose
//     controls (prbody.Ticked on closed bodies, where descriptions carry
//     controls: Caps.BodyControls), and Caps.ClosedImmutable; Cooldown for
//     the key.
//  8. the commit, when D is not empty: BuildCommit of D on B with the hub's
//     blobs, the writer as author and committer (with the platform's
//     no-reply address when it gives no email), dated
//     max(Write.HubCommitTime, B's date), the message of
//     prbody.CommitMessage with decide.FormatTrailers, signed with the
//     provider's key; then the unsafe checks on B and on
//     the new tree: a parent file or a case clash among the paths D adds,
//     and when B or D holds a .gitattributes, FetchBlobs of B's, Attrs for
//     filter and working-tree-encoding, Renormalized where text, eol or
//     ident apply. Failing paths become unsafe (a target warning each), D is
//     recomputed and the commit rebuilt, at most three rounds (then
//     failed:internal). A commit that changes other paths than D is
//     failed:integrity.
//  9. decide.DecideTarget with everything above → Work.Decision, the
//     Workflows permission from Write.CanWorkflows. When the decision
//     pushes (or is blocked on the Workflows permission) and the
//     provider's identity is a platform.Preflighter, the rules of the
//     default branch and of the branches it pushes to are read (the
//     reader's in ModePlan, the writer's otherwise) and the target decided
//     again with them: the writer's Workflows permission, when it knows
//     it, replaces Write.CanWorkflows, and the branches a rule keeps from
//     force pushes are decide's NoForcePush (blocked:rules:non-fast-forward
//     under our open pull request, a branch created afresh without one).
//     Rules the identity cannot read (Rules.Known false) change nothing.
//     A decision that still pushes asks the writer, when it is a
//     platform.PushGuard (GitLab, Gitea and Forgejo), which branches it
//     pushes to a protection rule keeps it from: one is blocked
//     rules:protected-branch before any write (blockProtected), memory
//     upkeep aside. Then the signature (checkSigning): providers[].sign "always", or a
//     rule requiring signed commits, needs one; the signing key makes it,
//     else the platform's API commit (Caps.Commit.API: phase F commits
//     through the stage ref), else, outside ModePlan, which never holds
//     the key and plans as if it could sign, a push becomes
//     blocked:cannot-sign (memory upkeep stays). The report gets the
//     outcome, reason and PR; an unchanged target whose StepEditPR writes
//     (decide.PlanPREdit on the rendered description, or content to
//     rewrite) is updated:title or updated:body, and one that writes
//     nothing marks the step idle.
//
// D. sweep (skipped with Only, or for a provider that is unavailable, is
// planned without credentials, recognizes no own PR, or whose resolve or
// listing is incomplete, with Sweep.Reason): Reader.OpenPRsBy(authors,
// branches), decide.Identity, decide.Sweep → one sweep Work per close
// (target-dropped, with a report line of its own; opted-out for a resolved
// target without opt-in file, which takes over its line; blocked:archived
// in an archived repository). A failed listing marks Sweep.Failed (exit 1).
//
// E. gate: decide.MassCloseAllowed over every close of the run (targets and
// sweep) with operations allow_mass_close; when refused, every work that
// closes becomes blocked:mass-close (exit 1) and nothing is closed; then
// max_new_prs_per_run (deferred:rollout-limit); then, in ModeDryRun, the
// permission preflight (Writer.Target with Work.NeedPerms, closed at once:
// blocked:permission:<what>, and blocked:cannot-sign when the commit must
// go through the API and the per-target writer is no platform.Committer);
// then the write estimates from the steps (an API commit counts three). A
// plan or a dry run also gets the report's Estimate (estimate.go): per
// provider the writes by the git path and, where a signature may be
// needed, through the API commit, the time the provider's write limits give
// them, and the runs max_new_prs_per_run gives the rollout.
//
// After phase E a plan or a dry run reports what each one-off operation
// does (Operations, operations.go).
//
// A plan limited by Deps.Scope (scope.go) runs phases D and E over the
// targets it processes: the sweep keeps only their closes (opted-out), and
// the mass-close guard counts every open pull request of touchmark its
// listing found. A plan that processes no target (report.ScopeHub) resolves
// and stops: no target is inspected and nothing is swept.
//
// F. ModeDistribute only: execute (execute.go) with every work that writes
// (targets in targets.yml order, then the sweep's closes).
//
// G. the report: Command "plan" or "distribute" (a dry run too), the ops
// journal, each finished target also written to Write.Stream: one JSON
// line per target (writeStreamLine: its report.DeliveryTarget, masked with
// Write.Redact, with the ops made to it), flushed each time, in the order
// the targets finished. A target phase F carries out is streamed by
// execute once its last work is done; every other target by the run once
// its outcome is final (after phase E), in the order its inspection
// finished (the sweep's own targets last).
//
// A target's repository is released as soon as its work is done: at the
// end of phase C, or after phase F for a work that writes in
// ModeDistribute. In ModePlan and ModeDryRun nothing is written to any
// platform; branch history, commits and checks run in the private
// repositories only.
func Run(ctx context.Context, d Deps, mode Mode) (*report.Delivery, error) {
	r, err := newRun(d, mode)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", commandOf(mode), err)
	}
	stop, err := r.superseded(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", commandOf(mode), err)
	}
	if stop {
		r.rep.Summarize()
		return r.rep, nil
	}
	kept, err := r.resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", commandOf(mode), err)
	}
	if r.hubOnly() {
		// The hub pull request changes nothing a target receives: no
		// target is inspected, and nothing is swept (scope.go).
		for _, t := range kept {
			t.outOfScope = true
		}
	} else {
		r.inspectAll(ctx, kept)
	}
	var sweeps []*Work
	switch {
	case r.repos == nil:
		r.gateSnapshotOnly(planned(kept))
		if len(d.Only) > 0 {
			r.rep.Sweep.Reason = sweepOffOnly
		}
	case r.hubOnly():
	default:
		sweeps = r.sweepAll(ctx, kept)
		r.gate(ctx, kept, sweeps)
	}
	if mode != ModeDistribute {
		r.reportOperations(kept)
	}
	reported := r.reported(kept, sweeps)
	if mode == ModeDistribute {
		r.carryOut(ctx, reported, r.executable(kept, sweeps))
	} else {
		r.carryOut(ctx, reported, nil)
		r.estimateRollout(reported)
		r.reportPaths(reported)
	}
	r.reportScope(kept)
	r.deadlineWarning(reported)
	r.pauseWarnings()
	for _, t := range reported {
		if t.hidden {
			// Whatever a platform said about it may name it.
			t.res.Warnings = nil
		}
		r.rep.Targets = append(r.rep.Targets, t.res)
	}
	r.rep.Summarize()
	return r.rep, nil
}

// deadlineWarning adds a run warning when Write.Deadline left targets
// undone: how many were deferred:deadline, so that a run whose inspections
// alone outlast the deadline says why nothing was written.
func (r *run) deadlineWarning(reported []*target) {
	n := 0
	for _, t := range reported {
		if t.res.Outcome == report.OutcomeDeferred && t.res.Reason == "deadline" {
			n++
		}
	}
	if n == 0 || r.d.Write.Deadline.IsZero() {
		return
	}
	r.warnf("deferred:deadline: the run's deadline (--deadline) left %d of %d targets undone; the next run goes on with them, "+
		"and a longer deadline or fewer targets per run lets it finish", n, len(reported))
}

// pauseWarnings adds a run warning for each provider that paused for rate
// limits, in hub.yml order: how often, and for how long in all. A provider
// out of budget says so on its targets' lines.
func (r *run) pauseWarnings() {
	for _, p := range r.provs {
		st := p.throttle().Stats()
		if st.Pauses == 0 {
			continue
		}
		times := "once"
		if st.Pauses > 1 {
			times = fmt.Sprintf("%d times", st.Pauses)
		}
		r.warnf("provider %s paused %s for rate limits (%s in all)", p.cfg.ID, times, st.Paused.Round(time.Second))
	}
}

// carryOut finishes the reported targets (finished) and runs phase F for
// todo: the targets without a work to carry out first, whose outcome phase
// E made final, in the order their inspection finished; then execute, which
// streams the targets it carries out; then their repositories are released
// and Cost is summed from the writes made.
func (r *run) carryOut(ctx context.Context, reported []*target, todo []*Work) {
	executed := map[*target]bool{}
	for _, w := range todo {
		executed[w.t] = true
	}
	for _, t := range byCompletion(reported) {
		if !executed[t] {
			r.finished(t, true)
		}
	}
	if len(todo) == 0 {
		return
	}
	r.execute(ctx, todo)
	r.recount(reported)
	for _, t := range byCompletion(reported) {
		if executed[t] {
			r.finished(t, false)
		}
	}
}

// works returns the works of the run: the targets' in the order of kept,
// then the sweep closes.
func (r *run) works(kept []*target, sweeps []*Work) []*Work {
	var out []*Work
	for _, t := range kept {
		if t.work != nil {
			out = append(out, t.work)
		}
	}
	return append(out, sweeps...)
}

// executable returns the works phase F carries out: every work that
// writes, the targets' in targets.yml order, then the sweep closes.
func (r *run) executable(kept []*target, sweeps []*Work) []*Work {
	var out []*Work
	for _, w := range r.works(byTargetsOrder(kept), sweeps) {
		if w.writes() {
			out = append(out, w)
		}
	}
	return out
}

// reported returns the targets with a line in the report: the targets of
// the run a limited plan processes (planned: every target, unless the plan
// is limited), then the repositories the sweep added.
func (r *run) reported(kept []*target, sweeps []*Work) []*target {
	out := planned(kept)
	for _, w := range sweeps {
		if w.t.dropped && !slices.Contains(out, w.t) {
			out = append(out, w.t)
		}
	}
	return out
}

// byCompletion returns list in the order its targets finished phase C
// (the sweep's own targets last, in their order).
func byCompletion(list []*target) []*target {
	out := slices.Clone(list)
	rank := func(t *target) int {
		if t.dropped {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b *target) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.order, b.order))
	})
	return out
}

// finished records that t's work is done, once: its repository is
// released and, with streamIt, its report line written to Write.Stream.
// The line of a target phase F carries out is execute's to write (stream.go:
// once its last work is done); every other target's is the run's, once its
// outcome is final after phase E. It is safe for concurrent use.
func (r *run) finished(t *target, streamIt bool) {
	r.mu.Lock()
	done := t.done
	t.done = true
	r.mu.Unlock()
	if done {
		return
	}
	r.release(t)
	if streamIt {
		r.stream(t)
	}
}

// release frees t's repository (Repos.Release); a failure is a warning.
func (r *run) release(t *target) {
	if r.repos == nil || t.dropped || t.hidden {
		return
	}
	if err := r.repos.Release(t.repo); err != nil {
		r.warnf("release the repository of %s:%s: %v", t.prov.cfg.ID, t.repo.Path, err)
	}
}

// stream writes t's report line to Write.Stream as one JSON line in the
// stream's format (writeStreamLine: the target, masked with Write.Redact,
// and no ops) and flushes it when the stream can be flushed. A write that
// fails is a run warning, and the run writes no more lines.
func (r *run) stream(t *target) {
	s := r.d.Write.Stream
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streamErr != nil {
		return
	}
	r.streamErr = writeStreamLine(s, r.d.Write.Redact, t.res, nil)
	if f, ok := s.(interface{ Flush() error }); ok && r.streamErr == nil {
		r.streamErr = f.Flush()
	}
	if r.streamErr != nil {
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%v; the stream misses the targets after it", r.streamErr))
	}
}

// reinspect runs phase C again for w's target, from a fresh report line:
// execute's recheck when the branch or the pull requests moved since the
// inspection, or after a push lost its lease. The snapshot, the pull
// requests, the history, the memory and the commit are read afresh, in the
// same repository, and the new work passes the gate of phase E again
// (regate: the rollout limit and the mass-close guard). It returns the new
// work, which is also the target's; nil when the target's outcome needs none
// (its report line says why). A sweep close is returned as it is.
func (r *run) reinspect(ctx context.Context, w *Work) *Work {
	if w == nil || w.Sweep {
		return w
	}
	t := w.t
	t.work = r.inspectSafe(ctx, t, r.d.Write.Deadline)
	r.regate(w, t.work)
	return t.work
}

// Work is one target after phase C (or one sweep close after phase D):
// everything execute needs to carry out its steps.
type Work struct {
	t *target

	// Repo is the target's private repository; nil for sweep closes. It is
	// released at the end of phase C in ModePlan and ModeDryRun, and after
	// phase F in ModeDistribute (Run, finished).
	Repo *gitx.TargetRepo
	// Tree is the snapshot of B; phase C drops it once the target is
	// inspected (phase F does not read it).
	Tree *snapshot.Tree
	// B is the default branch head; DefaultBranch its name.
	B             string
	DefaultBranch string
	// Plan is the per-path plan; D its pairs and Key their key.
	Plan decide.Plan
	D    []decide.Pair
	Key  string
	// Built is the commit of D on B (zero when D is empty).
	Built gitx.Built
	// Branch and Aliases are the classified sync branches (the aliases with
	// an own or marker-invalid open PR). Both are zero when step 6 was not
	// needed: D is empty and no such PR is open.
	Branch  decide.Branch
	Aliases map[string]decide.Branch
	// Own are the own PRs in every state, newest first; Open the own open
	// PR the decision works on (nil when none): the kept one, or a
	// marker-invalid one the decision adopts (with an empty marker). A
	// sweep close carries its PR here too.
	Own  []decide.OwnPR
	Open *decide.OwnPR
	// Memory is the target's memory; OptInHash the opt-in file's hash.
	Memory    decide.Memory
	OptInHash string
	// Decision is decide.DecideTarget's verdict (after the gate: a work the
	// gate or the preflight stops has no steps).
	Decision decide.TargetDecision
	// Body is the body input without Marker, blocks and controls, which
	// execute fills from Decision and the marker it writes (humanBody
	// renders the rest): the changes table of what the branch holds after
	// the decision (D when it pushes or equals the branch's C, else the
	// branch's C) with the hub commit of that content, while paused the
	// pending D − C, the local files, the sensitive patterns of hub.yml, the
	// hub's name and URL (pr.link_hub for the target's visibility), the
	// packs and the platform's Caps.
	Body prbody.Input
	// Marker is the marker data of the content D: Stream, Hub, FP, HubRepo
	// (empty where pr.link_hub hides the hub), DecidedAt, ContentCommit (the
	// run's hub commit), Base (B), OptIn, Engine, Packs, Changes (short),
	// ChangesComplete; its key is Key. A write that describes a branch's C
	// instead takes the branch's C and CKey and the Touchmark-Hub-Commit of
	// its Hc (decide.Step.Content). execute adds TitleSet, Body, LabelsSet
	// and the state fields as it writes. For a sweep close it is the PR's
	// marker data as found.
	Marker marker.Data
	// Sweep is set for a sweep close: SweepPR is the PR and Repo is nil.
	Sweep   bool
	SweepPR *decide.OwnPR
	// NeedPerms is the per-target write token's permissions (Contents for
	// pushes and deletes, PRs for PR writes, Workflows when a push needs it).
	NeedPerms platform.Perms

	// rules are the platform rules step 6 read for the branches the
	// decision pushes to and the default branch (platform.Preflighter;
	// zero when the provider reads none or the decision needs none).
	rules platform.Rules
	// needSig is set when a push of the work must be signed (providers[].sign
	// always, or a rule), and viaAPI when the signature is the platform's API
	// commit through the stage ref rather than the provider's key.
	needSig, viaAPI bool

	// idle marks the StepEditPR steps (by index in Decision.Steps) that
	// field ownership skips: they change nothing and rewrite no content.
	idle map[int]bool
	// listed is what inspection saw of every pull request Reader.PRs listed
	// on the sync branches (a sweep close: of its pull request), by number:
	// the recheck of phase F compares the fresh listing with it.
	listed map[int64]prSeen
}

// writes reports whether w has a step that writes: any but a StepEditPR
// that is idle.
func (w *Work) writes() bool {
	for i, s := range w.Decision.Steps {
		if s.Kind != decide.StepEditPR || !w.idle[i] {
			return true
		}
	}
	return false
}
