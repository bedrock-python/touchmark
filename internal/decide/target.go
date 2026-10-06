package decide

import (
	"slices"
	"time"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Target outcomes and reasons (the strings of report/v1, see
// docs/reference/output.md; decide cannot import report, which imports
// decide).
const (
	OutcomeOpened    = "opened"
	OutcomeUpdated   = "updated"
	OutcomeUnchanged = "unchanged"
	OutcomeClosed    = "closed"
	OutcomeDeclined  = "declined"
	OutcomeBlocked   = "blocked"
	OutcomeDeferred  = "deferred"

	ReasonContent        = "content"
	ReasonRebase         = "rebase"
	ReasonRecreate       = "recreate"
	ReasonBaseRenamed    = "base-renamed"
	ReasonEdited         = "edited"
	ReasonBranchTaken    = "branch-taken"
	ReasonBranchInUse    = "branch-in-use"
	ReasonMarkerInvalid  = "marker-invalid"
	ReasonCooldown       = "cooldown"
	ReasonPermissionWflw = "permission:workflows"
	// ReasonRulesNoForce is the reason of a push a platform rule refuses
	// because it would force the branch (GitHub's non_fast_forward): a
	// report reason "rules:<rule>".
	ReasonRulesNoForce = "rules:non-fast-forward"
)

// CommentAutoDeclined is the Reason of the StepComment that follows the
// StepAck of an auto-close counted as a decline (the third in a row with
// one key): its comment also advises taking touchmark's pull requests out
// of the bot's reach (prbody.AutoDeclinedComment). Other declines' comments
// have Reason OutcomeDeclined.
const CommentAutoDeclined = "auto-declined"

// StepKind is one write DecideTarget asks for.
type StepKind uint8

const (
	// StepPush builds the commit of D on B and pushes it to Branch with a
	// lease on Expect ("" = the branch must not exist).
	StepPush StepKind = iota + 1
	// StepRecreateBranch deletes Branch with a lease on Expect, then pushes
	// the commit of D on B with a lease on "": a fresh branch from B carries
	// no workflow diff.
	StepRecreateBranch
	// StepDeleteBranch deletes Branch with a lease on Expect.
	StepDeleteBranch
	// StepCreatePR opens a PR from Branch to the default branch.
	StepCreatePR
	// StepEditPR writes the fields PlanPREdit plans for PR (body, title,
	// base, labels); the caller skips it when nothing changes and Content
	// is not set.
	StepEditPR
	// StepClosePR is one EditPR: the body with marker closed = {touchmark,
	// Reason} and state closed.
	StepClosePR
	// StepComment posts one comment on PR; Reason names its kind
	// ("closed", "declined", "auto-declined"): OutcomeClosed follows the
	// StepClosePR of the same PR, whose Reason says why; OutcomeDeclined and
	// CommentAutoDeclined follow a StepAck.
	StepComment
	// StepAck writes a first-seen decline's ack: marker ack and optin, and
	// the repropose control in the body.
	StepAck
	// StepRevoke writes revoked into a decline's marker.
	StepRevoke
	// StepConsumeRecreate unticks the recreate control and writes
	// recreate_for = Expect into the open PR's marker, before the push.
	StepConsumeRecreate
)

// String returns the step name for plans, messages and tests.
func (k StepKind) String() string {
	switch k {
	case StepPush:
		return "push"
	case StepRecreateBranch:
		return "recreate-branch"
	case StepDeleteBranch:
		return "delete-branch"
	case StepCreatePR:
		return "create-pr"
	case StepEditPR:
		return "edit-pr"
	case StepClosePR:
		return "close-pr"
	case StepComment:
		return "comment"
	case StepAck:
		return "ack"
	case StepRevoke:
		return "revoke"
	case StepConsumeRecreate:
		return "consume-recreate"
	}
	return "unknown"
}

// Step is one write, in order.
type Step struct {
	Kind   StepKind
	Branch string
	// Expect is the lease of pushes and deletes, and the recreate_for value
	// of StepConsumeRecreate.
	Expect string
	PR     int64
	Reason string
	// NeedWorkflows is set on pushes that create, change or carry changes
	// under WorkflowsDir (the per-target token must include Workflows).
	NeedWorkflows bool
	// Content, on a StepEditPR or StepClosePR, asks the caller to rewrite
	// the marker's content fields (key, changes, changes_complete, packs,
	// content_commit) and to write even when PlanPREdit reports no change.
	// They describe what the branch holds after the decision: D (key
	// TargetInput.Key, content_commit the hub commit of this run) when the
	// decision pushes it; otherwise the branch's C (key Branch.CKey,
	// content_commit the Touchmark-Hub-Commit trailer of Hc, which FindHc
	// returns with Hc's index: Trailers.HubCommit). It is set after every
	// push, and whenever the marker's key is not CKey (a run that pushed and
	// then lost its EditPR, as in TestCrashMatrix): memory must remember
	// what the PR really carried.
	//
	// The marker of a PR adopted from MarkerInvalid is empty or broken:
	// every write to it carries Content and the caller writes every field.
	// Its edits describe D or C as above (without either, no edit is
	// asked); a StepClosePR of it whose branch has no Hc describes D, since
	// a close by touchmark creates no memory.
	Content bool
	// Base, on a StepEditPR, is the base the edit sets, passed as
	// DesiredPR.Base: DefaultBranch when the decision rebuilt the branch on
	// B (a push of the branch comes first), "" otherwise. A base moved
	// without a rebuild would show the commits between the two branches.
	Base string
}

// Blocks are the situational blocks the PR body must show.
type Blocks struct {
	// Paused: "touchmark paused" with the recreate control; the body lists
	// what a rebuild would bring.
	Paused bool
	// UpdateBranchNeeded: "Press Update branch; touchmark continues on the
	// next run".
	UpdateBranchNeeded bool
	// NothingMore: "the hub proposes nothing more: close this PR or keep
	// your commits".
	NothingMore bool
	// PreviouslyDeclined lists declines overlapping D.
	PreviouslyDeclined []int64
}

// TargetInput is everything DecideTarget needs about one target and stream.
type TargetInput struct {
	Stream string
	// D and Key: desired pairs relative to B; Key is "" when D is empty.
	D   []Pair
	Key string
	// B is the default branch head; DefaultBranch its name.
	B             string
	DefaultBranch string
	// Branch is the classified sync branch; Aliases the classified alias
	// branches, by name (only those that carry an own open PR matter).
	Branch  Branch
	Aliases map[string]Branch
	// Own are own PRs of the stream in every state, newest first.
	Own []OwnPR
	// ForeignOpen are open PRs from the same repository on sync branches
	// that are not ours (a fork's PR never uses our branch and is not here).
	ForeignOpen []platform.PR
	// MarkerInvalid are open PRs of our author on our branches, same
	// repository, whose marker is missing or broken.
	MarkerInvalid []platform.PR
	Memory        Memory
	// CooldownUntil and CooldownDeclined come from Memory.Cooldown(Key).
	CooldownUntil    time.Time
	CooldownDeclined bool
	Ops              TargetOps
	// RecreateTicked is set when the recreate control is ticked in the body
	// of the own open PR.
	RecreateTicked bool
	// PlatformWorkflowPerm is Caps.WorkflowPerm; CanWorkflows tells whether
	// the write identity can get it; WorkflowsDiffer whether tree(E) and
	// tree(B) differ under WorkflowsDir for the branch that would move (a
	// Foreign or Edited branch has no E: tree(H) stands for it).
	PlatformWorkflowPerm bool
	CanWorkflows         bool
	WorkflowsDiffer      bool
	// NoForcePush lists the sync branches on which a platform rule forbids
	// force pushes (Preflighter; GitHub's non_fast_forward). Every push that
	// moves an existing sync branch rebuilds it on B, which forces it.
	NoForcePush []string
	// NoDelete lists the sync branches on which a platform rule forbids
	// deleting them (GitHub's deletion): the fallback that deletes a branch
	// and creates it again is refused there.
	NoDelete []string
	// LegacyRewritable is Branch.LegacyRewritable of the sync branch, for
	// callers that set it here: either one counts.
	LegacyRewritable bool
	Now              time.Time
}

// TargetDecision is what to do with one target.
type TargetDecision struct {
	Outcome string
	Reason  string
	// PR is the PR acted on or blocking (0 for a PR not created yet).
	PR int64
	// Branch is the branch written or shown.
	Branch string
	Steps  []Step
	Blocks Blocks
	// Body is set when the caller must render the PR body (a new PR, an
	// edit of the open PR, a recreate): exactly when Steps hold a
	// StepCreatePR or a StepEditPR.
	Body bool
}

// DecideTarget decides what one target gets: when to push, what GitHub's
// Workflows permission allows, what memory holds back, which PRs close, and
// what operations.yml asks. Rules, in order:
//
// Memory upkeep, always first: StepRevoke for each Memory.ToRevoke, then
// StepAck and StepComment for each Memory.ToAck (Reason
// CommentAutoDeclined for an auto-close in Memory.Auto, OutcomeDeclined
// otherwise).
//
//  1. An open PR in MarkerInvalid → blocked:marker-invalid (no other write),
//     unless operations.yml lets touchmark adopt it (see below): an adopted
//     PR joins the own open PRs with an empty marker.
//  2. An own open PR exists (the newest; on the sync branch or an alias,
//     its branch X). Own open PRs on other sync branches are closed as
//     duplicates (StepClosePR "duplicate" + StepComment); the one on the
//     sync branch itself is kept, else the newest.
//     a. A foreign open PR on X → blocked:branch-in-use (X is never moved or
//     deleted while someone else's PR uses it, even with our open PR).
//     b. X is Foreign (and not legacy-rewritable) or Edited:
//     - RecreateTicked, or X.Head is in Ops.RecreateHeads →
//     StepConsumeRecreate (only when ticked) + StepPush(expect H) +
//     StepEditPR → updated:recreate;
//     - the marker's recreate_for equals X.Head (a run crashed after
//     consuming the control) → StepPush(expect H) + StepEditPR →
//     updated:recreate;
//     - D empty → blocked:edited with Blocks.NothingMore (never closed);
//     - otherwise → blocked:edited with Blocks.Paused.
//     c. X is Rewritable (or legacy-rewritable, or absent):
//     - D empty → StepClosePR("no-diff") + StepComment("closed") +
//     StepDeleteBranch(expect H) → closed:no-diff;
//     - PairsEqual(C, D) and the PR's base is DefaultBranch → StepEditPR
//     only (the caller drops it when PlanPREdit changes nothing and
//     Content is not set) → unchanged;
//     - otherwise a push is needed (content changed, or the base was
//     renamed: rebuild on B and edit the base). need =
//     TouchesWorkflows(D) or WorkflowsDiffer. With PlatformWorkflowPerm,
//     need and !CanWorkflows: TouchesWorkflows(D) →
//     blocked:permission:workflows; else blocked:permission:workflows
//     with Blocks.UpdateBranchNeeded. X in NoForcePush (X exists) →
//     blocked:rules:non-fast-forward, no write. Else StepPush(expect H,
//     NeedWorkflows = need) + StepEditPR → updated:content;
//     updated:rebase when C and D differ only in From (the base changed
//     our paths); updated:base-renamed when only the base differs.
//  3. No own open PR:
//     a. D empty → unchanged, no write (the old branch is left alone).
//     b. Memory.IsDeclined(D, Key) or CooldownDeclined → declined, PR = the
//     newest covering decline, no push.
//     c. CooldownUntil after Now → deferred:cooldown.
//     d. A foreign open PR on the sync branch → blocked:branch-in-use.
//     e. The sync branch is:
//     - Absent → StepPush(expect "") + StepCreatePR → opened;
//     - Rewritable with HcIsHead, PairsEqual(C, D) and HcParent == B (a
//     run crashed between push and PR) → StepCreatePR only → opened;
//     - Rewritable otherwise → move it: with PlatformWorkflowPerm,
//     TouchesWorkflows(D) and !CanWorkflows → blocked:permission:workflows;
//     with PlatformWorkflowPerm, WorkflowsDiffer and !CanWorkflows, or the
//     branch in NoForcePush → StepRecreateBranch(expect H) +
//     StepCreatePR (a deletion and a creation force nothing), unless the
//     branch is in NoDelete: then blocked:rules:non-fast-forward (in
//     NoForcePush) or blocked:permission:workflows, no write; else
//     StepPush(expect H, NeedWorkflows) + StepCreatePR → opened;
//     - Foreign: legacy-rewritable → as Rewritable; H in
//     Ops.RecreateHeads → StepPush(expect H) + StepCreatePR → opened;
//     else blocked:branch-taken;
//     - Edited: H in Ops.RecreateHeads → StepPush(expect H) +
//     StepCreatePR → opened; else blocked:edited.
//     A new PR gets Blocks.PreviouslyDeclined = Memory.Overlap(D).
//
// DecideTarget never deletes or moves a branch that carries a foreign open
// PR, never pushes to the branch of a closed PR unless it opens a new PR,
// and never sets a branch head to B (GitHub closes a PR whose head equals
// its base, and Gitea and Forgejo with autodetect_manual_merge record a
// merge): D is never empty on a push.
//
// Operations adopt MarkerInvalid PRs. An open PR there is adopted, D not
// empty, when:
//   - operations.yml recreate names the head of its branch: the branch is
//     rebuilt whatever its state (StepPush with a lease on its head, then
//     StepEditPR → updated:recreate; RecreateTicked and recreate_for play
//     no part for it), and the edit writes a fresh marker;
//   - or adopt_unmarked is active (Ops.AdoptUnmarked), the PR is on an
//     alias, its body holds no touchmark comment at all (merge requests
//     multi-gitter opened, whose content is unknown), and its branch is
//     Rewritable or legacy-rewritable: it is maintained like an own PR.
//
// A branch is legacy-rewritable when it is Foreign, Ops.AdoptUnmarked is
// set and the caller marked it (Branch.LegacyRewritable, or
// TargetInput.LegacyRewritable for the sync branch). Such a branch counts
// as Rewritable everywhere: pushes lease on its head, a no-diff close
// deletes it, a duplicate on it is closed.
//
// Where the rules above leave room, DecideTarget reads them so:
//   - Memory upkeep runs whatever follows, blocked:marker-invalid included
//     (it writes to closed PRs only). Per decline, StepAck comes before its
//     StepComment: a run that stops between them loses the comment rather
//     than posting it twice.
//   - The kept PR is the newest open own PR on the sync branch, else the
//     newest open own PR (PR numbers order adopted PRs among Own). Its
//     branch X is found by the PR's head: Branch when that is Branch.Name,
//     else Aliases; a head found in neither is an absent branch named after
//     it.
//   - Every other open own PR is a duplicate, a second one on X included,
//     so that X never carries two. A duplicate whose branch is Edited or
//     Foreign (not legacy-rewritable) stays open: closing it would bury
//     people's commits, and an edited branch's PR is never closed. After
//     the closes, the branch of each closed duplicate other than X is
//     deleted with a lease on its head when it exists, is rewritable and
//     carries no foreign open PR.
//   - A block reaches the body only through a write, so a decision that
//     shows one on the open PR (Paused, NothingMore, UpdateBranchNeeded)
//     asks for StepEditPR (one write); the caller drops it once the body
//     shows the block. Body is set exactly when the steps hold
//     StepCreatePR or StepEditPR, and then PreviouslyDeclined is
//     Memory.Overlap(D) for an open PR as for a new one: the desired body
//     must not change from one run to the next, or a rerun on the same
//     inputs would write again. An adopted PR whose marker could not be
//     written (no push, and no Hc to describe) gets no edit and so no
//     block.
//   - unchanged with a StepEditPR is reported by the caller as
//     updated:body or updated:title when the edit writes (PlanPREdit
//     changed something, or Content is set), and as unchanged otherwise.
//   - In 2b, an empty D wins over a recreate (a rebuild of nothing would
//     set the head to B); the control stays ticked.
//   - The Workflows rule applies to every push. need is
//     TouchesWorkflows(D), plus WorkflowsDiffer when the push moves an
//     existing branch (for a Foreign or Edited branch E is not defined: the
//     caller compares tree(H) with tree(B)). With PlatformWorkflowPerm,
//     need and !CanWorkflows: a need from D is
//     blocked:permission:workflows; a need from the move alone is
//     blocked:permission:workflows with Blocks.UpdateBranchNeeded while our
//     open PR is on the branch (a recreate keeps Blocks.Paused as well),
//     and StepRecreateBranch + StepCreatePR when no PR of ours is open (a
//     Foreign or Edited branch that operations.yml rebuilds included).
//     StepRecreateBranch carries NeedWorkflows = TouchesWorkflows(D).
//   - A rule against force pushes (NoForcePush) applies to every push that
//     moves an existing branch, a recreate included, after the Workflows
//     rule: with our open PR on the branch it is
//     blocked:rules:non-fast-forward (the rebuild of an Edited or Foreign
//     branch keeps Blocks.Paused and its edit; otherwise nothing is
//     written: deleting the branch would close the PR); without one the
//     branch is deleted with a lease and created anew (StepRecreateBranch,
//     NeedWorkflows = TouchesWorkflows(D)).
//   - An absent X with our PR open (the platform kept the PR without its
//     branch) loses nothing: D empty closes the PR (closed:no-diff) without
//     deleting a branch; otherwise X is pushed with a lease on "" and the
//     PR edited (updated:content).
//   - The base counts as renamed only when DefaultBranch is known.
//   - declined names the first PR IsDeclined returns (it lists the newest
//     first), or, when only CooldownDeclined holds, the newest auto-close
//     with Key; deferred:cooldown names the newest auto-close with Key.
//   - PR is the kept PR, or the PR that blocks (branch-in-use: the foreign
//     PR; marker-invalid: the first MarkerInvalid PR not adopted); 0 when
//     there is none. Branch is the branch the decision writes or blocks
//     on, "" for unchanged, declined and deferred without an open PR.
//   - PR steps carry the PR and its head as Branch (memory upkeep: the PR
//     only); branch steps carry Branch and Expect, PR 0; StepCreatePR
//     carries the sync branch. Content and Base are as documented on Step.
//
// DecideTarget does not modify in and never panics.
func DecideTarget(in TargetInput) TargetDecision {
	t := &targetDecider{in: in}
	for _, n := range in.Memory.ToRevoke {
		t.add(Step{Kind: StepRevoke, PR: n})
	}
	for _, n := range in.Memory.ToAck {
		reason := OutcomeDeclined
		if slices.ContainsFunc(in.Memory.Auto, func(a AutoClose) bool { return a.PR == n }) {
			reason = CommentAutoDeclined
		}
		t.add(Step{Kind: StepAck, PR: n})
		t.add(Step{Kind: StepComment, PR: n, Reason: reason})
	}
	own, blocking, ok := t.adopt()
	if !ok {
		return t.finish(OutcomeBlocked, ReasonMarkerInvalid, blocking.Number, blocking.Head)
	}
	if kept, dups, ok := t.ownOpen(own); ok {
		return t.withOpenPR(kept, dups)
	}
	return t.withoutOpenPR()
}

// targetDecider builds one TargetDecision.
type targetDecider struct {
	in TargetInput
	d  TargetDecision
	// adopted holds the MarkerInvalid PRs taken over, by number; rebuild
	// marks those a recreate entry names.
	adopted, rebuild map[int64]bool
}

// add appends a step.
func (t *targetDecider) add(s Step) { t.d.Steps = append(t.d.Steps, s) }

// editPR asks for the edit of the open PR pr on branch x. pushed tells
// whether the decision pushed x; it sets Content and Base. It returns false,
// adding nothing, when the edit cannot write a marker: an adopted PR whose
// content is unknown.
func (t *targetDecider) editPR(pr OwnPR, x Branch, pushed bool) bool {
	content, ok := t.content(pr, x, pushed)
	if !ok {
		return false
	}
	s := Step{Kind: StepEditPR, PR: pr.PR.Number, Branch: x.Name, Content: content}
	if pushed {
		s.Base = t.in.DefaultBranch
	}
	t.add(s)
	return true
}

// content tells whether a write to the open PR pr on x must rewrite the
// marker's content fields (see Step.Content), and false for ok when no
// marker can be written: pr was adopted and x's content is unknown.
func (t *targetDecider) content(pr OwnPR, x Branch, pushed bool) (content, ok bool) {
	switch {
	case pushed:
		return true, true
	case x.Hc != "" && x.CKey != "":
		return pr.Marker.Key != x.CKey, true
	case t.adopted[pr.PR.Number]:
		return false, false
	}
	return false, true
}

// finish fills in the outcome and derives Body and PreviouslyDeclined.
func (t *targetDecider) finish(outcome, reason string, pr int64, branch string) TargetDecision {
	t.d.Outcome, t.d.Reason, t.d.PR, t.d.Branch = outcome, reason, pr, branch
	t.d.Body = slices.ContainsFunc(t.d.Steps, func(s Step) bool { return s.Kind == StepCreatePR || s.Kind == StepEditPR })
	if t.d.Body {
		t.d.Blocks.PreviouslyDeclined = t.in.Memory.Overlap(t.in.D)
	} else {
		t.d.Blocks = Blocks{}
	}
	return t.d
}

// adopt returns the own PRs with the MarkerInvalid PRs operations.yml lets
// touchmark take over (see DecideTarget), ordered newest first by number
// among Own. ok is false when a MarkerInvalid PR is not taken over: it is
// blocking, and blocks the target.
func (t *targetDecider) adopt() (own []OwnPR, blocking platform.PR, ok bool) {
	in := t.in
	own = in.Own
	for _, pr := range in.MarkerInvalid {
		x := t.branch(pr.Head)
		switch {
		case pr.State != platform.Open || len(in.D) == 0:
			return nil, pr, false
		case t.recreateHead(x.Head):
			t.markAdopted(pr.Number, true)
		case in.Ops.AdoptUnmarked && pr.Head != in.Branch.Name && targetUnmarked(pr.Body) &&
			(x.State == BranchRewritable || t.legacy(x)):
			t.markAdopted(pr.Number, false)
		default:
			return nil, pr, false
		}
		o := OwnPR{PR: pr, Alias: pr.Head != in.Branch.Name}
		at := slices.IndexFunc(own, func(p OwnPR) bool { return p.PR.Number < pr.Number })
		if at < 0 {
			at = len(own)
		}
		own = slices.Insert(slices.Clone(own), at, o)
	}
	return own, platform.PR{}, true
}

// markAdopted records an adopted PR; rebuild marks one a recreate entry
// names.
func (t *targetDecider) markAdopted(n int64, rebuild bool) {
	if t.adopted == nil {
		t.adopted, t.rebuild = map[int64]bool{}, map[int64]bool{}
	}
	t.adopted[n] = true
	t.rebuild[n] = rebuild
}

// targetUnmarked reports whether body holds no touchmark comment at all:
// neither a marker of any hub or version nor a broken one.
func targetUnmarked(body string) bool {
	_, status := marker.Find(body, nil)
	return status == marker.None
}

// ownOpen returns the kept open own PR among own and the other open own
// PRs, in the order of own; ok is false when no own PR is open.
func (t *targetDecider) ownOpen(own []OwnPR) (kept OwnPR, dups []OwnPR, ok bool) {
	at := -1
	for i, o := range own {
		if o.PR.State != platform.Open {
			continue
		}
		if at < 0 || (own[at].Alias && !o.Alias) {
			at = i
		}
	}
	if at < 0 {
		return OwnPR{}, nil, false
	}
	for i, o := range own {
		if i != at && o.PR.State == platform.Open {
			dups = append(dups, o)
		}
	}
	return own[at], dups, true
}

// branch returns the classification of the sync branch named head.
func (t *targetDecider) branch(head string) Branch {
	if head == t.in.Branch.Name {
		return t.in.Branch
	}
	b, ok := t.in.Aliases[head]
	if !ok {
		return Branch{Name: head}
	}
	if b.Name == "" {
		b.Name = head
	}
	return b
}

// legacy reports whether b is a Foreign branch the one-off migration rule
// lets touchmark rewrite while adopt_unmarked is active.
func (t *targetDecider) legacy(b Branch) bool {
	marked := b.LegacyRewritable || (b.Name == t.in.Branch.Name && t.in.LegacyRewritable)
	return b.State == BranchForeign && marked && t.in.Ops.AdoptUnmarked
}

// rewritable reports whether b may be rewritten or deleted: Rewritable, or
// legacy-rewritable.
func (t *targetDecider) rewritable(b Branch) bool {
	return b.State == BranchRewritable || t.legacy(b)
}

// foreignOn returns the first foreign open PR on branch.
func (t *targetDecider) foreignOn(branch string) (platform.PR, bool) {
	i := slices.IndexFunc(t.in.ForeignOpen, func(pr platform.PR) bool { return pr.Head == branch })
	if i < 0 {
		return platform.PR{}, false
	}
	return t.in.ForeignOpen[i], true
}

// recreateHead reports whether operations.yml asks to rebuild a branch
// whose head is head.
func (t *targetDecider) recreateHead(head string) bool {
	return head != "" && slices.Contains(t.in.Ops.RecreateHeads, head)
}

// blockWorkflows reports whether a push with need cannot get the Workflows
// permission it needs.
func (t *targetDecider) blockWorkflows(need bool) bool {
	return need && t.in.PlatformWorkflowPerm && !t.in.CanWorkflows
}

// noForce reports whether a rule forbids force pushes to the existing
// branch b: a push that moves it from its head would be refused.
func (t *targetDecider) noForce(b Branch) bool {
	return b.Head != "" && b.State != BranchAbsent && slices.Contains(t.in.NoForcePush, b.Name)
}

// withOpenPR applies rule 2: kept is the open own PR touchmark maintains.
func (t *targetDecider) withOpenPR(kept OwnPR, dups []OwnPR) TargetDecision {
	x := t.branch(kept.PR.Head)
	var closed []string // branches of the closed duplicates, in order
	for _, o := range dups {
		b := t.branch(o.PR.Head)
		if !t.rewritable(b) && b.State != BranchAbsent {
			continue
		}
		t.add(Step{Kind: StepClosePR, PR: o.PR.Number, Branch: o.PR.Head, Reason: ReasonDuplicate, Content: t.adopted[o.PR.Number]})
		t.add(Step{Kind: StepComment, PR: o.PR.Number, Branch: o.PR.Head, Reason: OutcomeClosed})
		if !slices.Contains(closed, b.Name) {
			closed = append(closed, b.Name)
		}
	}
	for _, name := range closed {
		b := t.branch(name)
		if _, busy := t.foreignOn(name); name == x.Name || busy || b.Head == "" || !t.rewritable(b) {
			continue
		}
		t.add(Step{Kind: StepDeleteBranch, Branch: name, Expect: b.Head})
	}
	if f, ok := t.foreignOn(x.Name); ok {
		return t.finish(OutcomeBlocked, ReasonBranchInUse, f.Number, x.Name)
	}
	if t.rebuild[kept.PR.Number] {
		return t.recreate(kept, x)
	}
	if (x.State == BranchForeign && !t.legacy(x)) || x.State == BranchEdited {
		return t.paused(kept, x)
	}
	return t.maintain(kept, x)
}

// paused applies rule 2b: X carries commits of others.
func (t *targetDecider) paused(kept OwnPR, x Branch) TargetDecision {
	in := t.in
	n := kept.PR.Number
	if len(in.D) == 0 {
		t.d.Blocks.NothingMore = true
		t.editPR(kept, x, false)
		return t.finish(OutcomeBlocked, ReasonEdited, n, x.Name)
	}
	rf := kept.Marker.Data.RecreateFor
	resume := rf != nil && *rf == x.Head
	if x.Head == "" || (!in.RecreateTicked && !t.recreateHead(x.Head) && !resume) {
		t.d.Blocks.Paused = true
		t.editPR(kept, x, false)
		return t.finish(OutcomeBlocked, ReasonEdited, n, x.Name)
	}
	return t.recreate(kept, x)
}

// recreate rebuilds X from B for the kept PR: a recreate asked through the
// control, operations.yml or a consumed control, or a MarkerInvalid PR a
// recreate entry adopts. D is not empty.
func (t *targetDecider) recreate(kept OwnPR, x Branch) TargetDecision {
	in := t.in
	n := kept.PR.Number
	wf := TouchesWorkflows(in.D)
	moves := x.State != BranchAbsent
	need := wf || (moves && in.WorkflowsDiffer)
	if t.blockWorkflows(need) {
		t.d.Blocks.Paused = x.State == BranchForeign || x.State == BranchEdited
		t.d.Blocks.UpdateBranchNeeded = !wf
		t.editPR(kept, x, false)
		return t.finish(OutcomeBlocked, ReasonPermissionWflw, n, x.Name)
	}
	if moves && t.noForce(x) {
		t.d.Blocks.Paused = x.State == BranchForeign || x.State == BranchEdited
		t.editPR(kept, x, false)
		return t.finish(OutcomeBlocked, ReasonRulesNoForce, n, x.Name)
	}
	if in.RecreateTicked && !t.adopted[n] {
		t.add(Step{Kind: StepConsumeRecreate, PR: n, Branch: x.Name, Expect: x.Head})
	}
	t.add(Step{Kind: StepPush, Branch: x.Name, Expect: x.Head, NeedWorkflows: need})
	t.editPR(kept, x, true)
	return t.finish(OutcomeUpdated, ReasonRecreate, n, x.Name)
}

// maintain applies rule 2c: X is rewritable (or absent: nothing to lose).
func (t *targetDecider) maintain(kept OwnPR, x Branch) TargetDecision {
	in := t.in
	n := kept.PR.Number
	moves := t.rewritable(x)
	if len(in.D) == 0 {
		content, _ := t.content(kept, x, false)
		t.add(Step{Kind: StepClosePR, PR: n, Branch: x.Name, Reason: ReasonNoDiff, Content: content})
		t.add(Step{Kind: StepComment, PR: n, Branch: x.Name, Reason: OutcomeClosed})
		if moves {
			t.add(Step{Kind: StepDeleteBranch, Branch: x.Name, Expect: x.Head})
		}
		return t.finish(OutcomeClosed, ReasonNoDiff, n, x.Name)
	}
	renamed := in.DefaultBranch != "" && kept.PR.Base != in.DefaultBranch
	same := x.State == BranchRewritable && PairsEqual(x.C, in.D)
	if same && !renamed {
		t.editPR(kept, x, false)
		return t.finish(OutcomeUnchanged, "", n, x.Name)
	}
	wf := TouchesWorkflows(in.D)
	need := wf || (moves && in.WorkflowsDiffer)
	if t.blockWorkflows(need) {
		if !wf {
			t.d.Blocks.UpdateBranchNeeded = true
			t.editPR(kept, x, false)
		}
		return t.finish(OutcomeBlocked, ReasonPermissionWflw, n, x.Name)
	}
	if moves && t.noForce(x) {
		return t.finish(OutcomeBlocked, ReasonRulesNoForce, n, x.Name)
	}
	expect := ""
	if moves {
		expect = x.Head
	}
	t.add(Step{Kind: StepPush, Branch: x.Name, Expect: expect, NeedWorkflows: need})
	t.editPR(kept, x, true)
	reason := ReasonContent
	switch {
	case same:
		reason = ReasonBaseRenamed
	case x.State == BranchRewritable && targetOnlyFrom(x.C, in.D):
		reason = ReasonRebase
	}
	return t.finish(OutcomeUpdated, reason, n, x.Name)
}

// targetOnlyFrom reports whether c and d hold the same paths with the same
// modes and blobs to write and differ only in From: the base changed our
// paths, and the push rebases the same content (updated:rebase).
func targetOnlyFrom(c, d []Pair) bool {
	if len(c) != len(d) || PairsEqual(c, d) {
		return false
	}
	strip := func(pairs []Pair) []Pair {
		out := slices.Clone(pairs)
		for i := range out {
			out[i].From = ""
		}
		return out
	}
	return PairsEqual(strip(c), strip(d))
}

// withoutOpenPR applies rule 3: no own PR is open.
func (t *targetDecider) withoutOpenPR() TargetDecision {
	in := t.in
	if len(in.D) == 0 {
		return t.finish(OutcomeUnchanged, "", 0, "")
	}
	if declined, prs := in.Memory.IsDeclined(in.D, in.Key); declined || in.CooldownDeclined {
		n := t.newestAuto()
		if declined && len(prs) > 0 {
			n = prs[0]
		}
		return t.finish(OutcomeDeclined, "", n, "")
	}
	if in.CooldownUntil.After(in.Now) {
		return t.finish(OutcomeDeferred, ReasonCooldown, t.newestAuto(), "")
	}
	b := in.Branch
	if f, ok := t.foreignOn(b.Name); ok {
		return t.finish(OutcomeBlocked, ReasonBranchInUse, f.Number, b.Name)
	}
	switch b.State {
	case BranchAbsent:
		return t.open(b, false)
	case BranchRewritable:
		if b.HcIsHead && b.HcParent == in.B && PairsEqual(b.C, in.D) {
			return t.createPR(b)
		}
		return t.open(b, true)
	case BranchForeign:
		if t.legacy(b) || t.recreateHead(b.Head) {
			return t.open(b, true)
		}
		return t.finish(OutcomeBlocked, ReasonBranchTaken, 0, b.Name)
	case BranchEdited:
		if t.recreateHead(b.Head) {
			return t.open(b, true)
		}
		return t.finish(OutcomeBlocked, ReasonEdited, 0, b.Name)
	}
	// An unknown state proves nothing: leave the branch alone.
	return t.finish(OutcomeBlocked, ReasonEdited, 0, b.Name)
}

// open builds the commit of D on the sync branch b and opens a PR from it:
// b is created (moves false) or moved from its head (moves true).
func (t *targetDecider) open(b Branch, moves bool) TargetDecision {
	in := t.in
	wf := TouchesWorkflows(in.D)
	if t.blockWorkflows(wf) {
		return t.finish(OutcomeBlocked, ReasonPermissionWflw, 0, b.Name)
	}
	switch {
	case !moves:
		t.add(Step{Kind: StepPush, Branch: b.Name, Expect: "", NeedWorkflows: wf})
	case t.blockWorkflows(in.WorkflowsDiffer), t.noForce(b):
		// A fresh branch from B carries no workflow diff,
		// and neither its deletion nor its creation forces it; a rule
		// against deleting the branch refuses that way too.
		if slices.Contains(in.NoDelete, b.Name) {
			if t.noForce(b) {
				return t.finish(OutcomeBlocked, ReasonRulesNoForce, 0, b.Name)
			}
			return t.finish(OutcomeBlocked, ReasonPermissionWflw, 0, b.Name)
		}
		t.add(Step{Kind: StepRecreateBranch, Branch: b.Name, Expect: b.Head, NeedWorkflows: wf})
	default:
		t.add(Step{Kind: StepPush, Branch: b.Name, Expect: b.Head, NeedWorkflows: wf || in.WorkflowsDiffer})
	}
	return t.createPR(b)
}

// createPR opens the PR from the sync branch b.
func (t *targetDecider) createPR(b Branch) TargetDecision {
	t.add(Step{Kind: StepCreatePR, Branch: b.Name})
	return t.finish(OutcomeOpened, "", 0, b.Name)
}

// newestAuto returns the newest auto-close with the key of D (Memory.Auto
// lists the newest first), or 0.
func (t *targetDecider) newestAuto() int64 {
	if t.in.Key == "" {
		return 0
	}
	i := slices.IndexFunc(t.in.Memory.Auto, func(a AutoClose) bool { return a.Key == t.in.Key })
	if i < 0 {
		return 0
	}
	return t.in.Memory.Auto[i].PR
}
