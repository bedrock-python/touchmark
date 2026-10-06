package decide

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The target of the DecideTarget tests: its sync branch and aliases, their
// heads, the default branch and its head, and the run's clock.
const (
	ttSync   = ownBranch
	ttAlias  = ownAlias
	ttAlias2 = "touchmark/older-id"
	ttMain   = "main"
)

var (
	ttB   = btSHA(0xb0) // B
	ttOld = btSHA(0xb1) // an older head of the default branch
	ttH   = btSHA(0x1)  // head of the sync branch
	ttHA  = btSHA(0x2)  // head of the alias
	ttHA2 = btSHA(0x3)  // head of the second alias
	ttNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
)

// ttBrokenMarker is a marker line of ours that does not parse.
const ttBrokenMarker = "<!-- touchmark:v1 hub=acme-eng fp=0123456789abcdef stream=sync key=sha256:x data=AAAA -->"

// ttD is D: one pair of each kind, none under WorkflowsDir.
func ttD() []Pair { return mixedPairs() }

// ttWD is D with a new workflow.
func ttWD() []Pair {
	return append(mixedPairs(), Pair{Path: ".github/workflows/ci.yml", From: ZeroOID, Mode: "100644", To: oidA})
}

// ttRewritable is a rewritable branch whose head is our commit on B,
// bringing c.
func ttRewritable(name, head string, c []Pair) Branch {
	return Branch{Name: name, Head: head, State: BranchRewritable, Hc: head, C: c, CKey: Key(StreamSync, c), E: ttB, HcIsHead: true, HcParent: ttB}
}

// ttBranch is a branch in state s whose head is head.
func ttBranch(name, head string, s BranchState) Branch {
	return Branch{Name: name, Head: head, State: s}
}

// ttOwn is an own PR on head with a marker of ours.
func ttOwn(n int64, head string, state platform.PRState) OwnPR {
	return OwnPR{
		PR: platform.PR{
			Number: n, State: state, Head: head, Base: ttMain, RepoID: "100", HeadRepoID: "100", BaseExists: true,
			Author: platform.Account{ID: writerID, Login: "acme-assets-write[bot]", Kind: platform.KindBot},
		},
		Marker: marker.Marker{Hub: "acme-eng", Stream: StreamSync, Data: marker.Data{V: marker.Version, Stream: StreamSync, Hub: "acme-eng", FP: ownFP}},
		Alias:  head != ttSync,
	}
}

// ttForeign is someone else's open PR on head, from the same repository.
func ttForeign(n int64, head string) platform.PR {
	return platform.PR{Number: n, State: platform.Open, Head: head, Base: ttMain, RepoID: "100", HeadRepoID: "100", BaseExists: true,
		Author: platform.Account{ID: "31337", Login: "jdoe", Kind: platform.KindUser}}
}

// ttInvalid is an open PR of our author on head whose body is body: a
// MarkerInvalid PR.
func ttInvalid(n int64, head, body string) platform.PR {
	pr := ttOwn(n, head, platform.Open).PR
	pr.Body = body
	return pr
}

// ttInput is a target without PRs, whose sync branch does not exist and
// which should receive ttD.
func ttInput() TargetInput {
	d := ttD()
	return TargetInput{Stream: StreamSync, D: d, Key: Key(StreamSync, d), B: ttB, DefaultBranch: ttMain, Branch: Branch{Name: ttSync}, Now: ttNow}
}

// Steps, spelled briefly.
func ttPush(branch, expect string, wf bool) Step {
	return Step{Kind: StepPush, Branch: branch, Expect: expect, NeedWorkflows: wf}
}
func ttRecreateBranch(branch, expect string) Step {
	return Step{Kind: StepRecreateBranch, Branch: branch, Expect: expect}
}
func ttRecreateBranchWf(branch, expect string) Step {
	return Step{Kind: StepRecreateBranch, Branch: branch, Expect: expect, NeedWorkflows: true}
}
func ttDelete(branch, expect string) Step {
	return Step{Kind: StepDeleteBranch, Branch: branch, Expect: expect}
}
func ttCreate() Step { return Step{Kind: StepCreatePR, Branch: ttSync} }

// ttEdit is the edit of a PR whose branch did not move: the base stays.
func ttEdit(n int64, branch string) Step { return Step{Kind: StepEditPR, PR: n, Branch: branch} }

// ttEditContent is ttEdit that rewrites the marker's content fields.
func ttEditContent(n int64, branch string) Step {
	return Step{Kind: StepEditPR, PR: n, Branch: branch, Content: true}
}

// ttEditPushed is the edit after a push: content fields and the base.
func ttEditPushed(n int64, branch string) Step {
	return Step{Kind: StepEditPR, PR: n, Branch: branch, Content: true, Base: ttMain}
}
func ttClose(n int64, branch, reason string) Step {
	return Step{Kind: StepClosePR, PR: n, Branch: branch, Reason: reason}
}
func ttClosedComment(n int64, branch string) Step {
	return Step{Kind: StepComment, PR: n, Branch: branch, Reason: OutcomeClosed}
}
func ttConsume(n int64, branch, expect string) Step {
	return Step{Kind: StepConsumeRecreate, PR: n, Branch: branch, Expect: expect}
}
func ttRevoke(n int64) Step { return Step{Kind: StepRevoke, PR: n} }
func ttAck(n int64) []Step {
	return []Step{{Kind: StepAck, PR: n}, {Kind: StepComment, PR: n, Reason: OutcomeDeclined}}
}

// ttWant is the decision with Body derived from the steps, as DecideTarget
// documents it.
func ttWant(outcome, reason string, pr int64, branch string, blocks Blocks, steps ...Step) TargetDecision {
	d := TargetDecision{Outcome: outcome, Reason: reason, PR: pr, Branch: branch, Steps: steps, Blocks: blocks}
	d.Body = slices.ContainsFunc(steps, func(s Step) bool { return s.Kind == StepCreatePR || s.Kind == StepEditPR })
	return d
}

// ttFormat prints a decision one step per line, for failure messages.
func ttFormat(d TargetDecision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:%s pr=%d branch=%q body=%v blocks=%+v", d.Outcome, d.Reason, d.PR, d.Branch, d.Body, d.Blocks)
	for _, s := range d.Steps {
		fmt.Fprintf(&b, "\n  %s branch=%q expect=%q pr=%d reason=%q wf=%v content=%v base=%q",
			s.Kind, s.Branch, s.Expect, s.PR, s.Reason, s.NeedWorkflows, s.Content, s.Base)
	}
	return b.String()
}

// ttCase is one DecideTarget case: edit turns ttInput into its input.
type ttCase struct {
	name string
	edit func(*TargetInput)
	want TargetDecision
}

// ttRun runs the cases, checking the invariants of every decision too.
func ttRun(t *testing.T, cases []ttCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := ttInput()
			c.edit(&in)
			got := DecideTarget(in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("DecideTarget =\n%s\nwant\n%s", ttFormat(got), ttFormat(c.want))
			}
			if err := ttCheck(in, got); err != nil {
				t.Fatalf("invariant: %v\n%s", err, ttFormat(got))
			}
		})
	}
}

// ttWithOwn adds an open own PR n on the sync branch, whose branch brings c
// and whose marker names c's key.
func ttWithOwn(n int64, c []Pair) func(*TargetInput) {
	return func(in *TargetInput) {
		o := ttOwn(n, ttSync, platform.Open)
		o.Marker.Key = Key(StreamSync, c)
		in.Own = append(in.Own, o)
		in.Branch = ttRewritable(ttSync, ttH, c)
	}
}

// ttEdits chains edits of the input.
func ttEdits(fs ...func(*TargetInput)) func(*TargetInput) {
	return func(in *TargetInput) {
		for _, f := range fs {
			f(in)
		}
	}
}

// Edits of the input.
var (
	ttNoD = func(in *TargetInput) { in.D, in.Key = nil, "" }
	ttWfD = func(in *TargetInput) {
		in.D = ttWD()
		in.Key = Key(StreamSync, in.D)
	}
	ttNoWorkflowPerm   = func(in *TargetInput) { in.PlatformWorkflowPerm, in.CanWorkflows = true, false }
	ttWithWorkflowPerm = func(in *TargetInput) { in.PlatformWorkflowPerm, in.CanWorkflows = true, true }
	ttDiffer           = func(in *TargetInput) { in.WorkflowsDiffer = true }
	ttNoForce          = func(branches ...string) func(*TargetInput) {
		return func(in *TargetInput) { in.NoForcePush = branches }
	}
	ttNoDelete = func(branches ...string) func(*TargetInput) {
		return func(in *TargetInput) { in.NoDelete = branches }
	}
	ttTicked        = func(in *TargetInput) { in.RecreateTicked = true }
	ttAdoptUnmarked = func(in *TargetInput) { in.Ops.AdoptUnmarked = true }
	ttSyncState     = func(s BranchState) func(*TargetInput) {
		return func(in *TargetInput) { in.Branch = ttBranch(ttSync, ttH, s) }
	}
	ttRecreateHeads = func(heads ...string) func(*TargetInput) {
		return func(in *TargetInput) { in.Ops.RecreateHeads = heads }
	}
	ttForeignOn = func(n int64, head string) func(*TargetInput) {
		return func(in *TargetInput) { in.ForeignOpen = append(in.ForeignOpen, ttForeign(n, head)) }
	}
	ttRecreateFor = func(head string) func(*TargetInput) {
		return func(in *TargetInput) { in.Own[0].Marker.Data.RecreateFor = &head }
	}
	ttBaseOfOwn = func(base string) func(*TargetInput) {
		return func(in *TargetInput) { in.Own[0].PR.Base = base }
	}
	ttMarkerKey = func(key string) func(*TargetInput) {
		return func(in *TargetInput) { in.Own[0].Marker.Key = key }
	}
	ttAliasBranch = func(name string, b Branch) func(*TargetInput) {
		return func(in *TargetInput) {
			if in.Aliases == nil {
				in.Aliases = map[string]Branch{}
			}
			in.Aliases[name] = b
		}
	}
	ttInvalidOn = func(n int64, head, body string) func(*TargetInput) {
		return func(in *TargetInput) { in.MarkerInvalid = append(in.MarkerInvalid, ttInvalid(n, head, body)) }
	}
)

func TestDecideTargetMemoryUpkeep(t *testing.T) {
	upkeep := func(in *TargetInput) {
		in.Memory.ToRevoke = []int64{3, 4}
		in.Memory.ToAck = []int64{5, 6}
	}
	steps := func(rest ...Step) []Step {
		s := []Step{ttRevoke(3), ttRevoke(4)}
		s = append(s, ttAck(5)...)
		s = append(s, ttAck(6)...)
		return append(s, rest...)
	}
	ttRun(t, []ttCase{
		{"alone", ttEdits(upkeep, ttNoD), ttWant(OutcomeUnchanged, "", 0, "", Blocks{}, steps()...)},
		{"before a new PR", upkeep, ttWant(OutcomeOpened, "", 0, ttSync, Blocks{}, steps(ttPush(ttSync, "", false), ttCreate())...)},
		{"before a close", ttEdits(ttWithOwn(12, ttD()), ttNoD, upkeep), ttWant(OutcomeClosed, ReasonNoDiff, 12, ttSync, Blocks{},
			steps(ttClose(12, ttSync, ReasonNoDiff), ttClosedComment(12, ttSync), ttDelete(ttSync, ttH))...)},
		{"with a broken marker", ttEdits(ttWithOwn(12, ttD()), upkeep, func(in *TargetInput) {
			in.MarkerInvalid = []platform.PR{ttForeign(9, ttSync), ttForeign(10, ttAlias)}
		}), ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttSync, Blocks{}, steps()...)},
		{"while declined", ttEdits(upkeep, func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 5, Key: in.Key}}
		}), ttWant(OutcomeDeclined, "", 5, "", Blocks{}, steps()...)},
		// The third auto-close in a row counts as a decline, and its comment
		// advises about the stale bot.
		{"an escalated auto-close", ttEdits(ttNoD, func(in *TargetInput) {
			in.Memory.ToAck = []int64{19, 7}
			in.Memory.Auto = []AutoClose{{PR: 19, Key: "sha256:x"}, {PR: 15, Key: "sha256:x"}}
		}), ttWant(OutcomeUnchanged, "", 0, "", Blocks{},
			Step{Kind: StepAck, PR: 19}, Step{Kind: StepComment, PR: 19, Reason: CommentAutoDeclined}, ttAck(7)[0], ttAck(7)[1])},
	})
}

func TestDecideTargetMarkerInvalid(t *testing.T) {
	d := ttD()
	invalid := func(in *TargetInput) { in.MarkerInvalid = []platform.PR{ttForeign(9, ttAlias)} }
	legacy := ttBranch(ttAlias, ttHA, BranchForeign)
	legacy.LegacyRewritable = true
	ttRun(t, []ttCase{
		{"no other PR", invalid, ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttAlias, Blocks{})},
		{"with an own open PR", ttEdits(ttWithOwn(12, d[:2]), invalid), ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttAlias, Blocks{})},
		{"with nothing to deliver", ttEdits(ttWithOwn(12, d), ttNoD, invalid), ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttAlias, Blocks{})},
		{"with a duplicate", ttEdits(ttWithOwn(12, d), invalid, func(in *TargetInput) {
			in.Own = append(in.Own, ttOwn(8, ttAlias, platform.Open))
			in.Aliases = map[string]Branch{ttAlias: ttRewritable(ttAlias, ttHA, d)}
		}), ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttAlias, Blocks{})},

		// A recreate entry for the branch's head lifts it.
		// The branch is rebuilt and the edit writes a fresh marker.
		{"recreate adopts it on an edited branch", ttEdits(ttSyncState(BranchEdited), ttInvalidOn(9, ttSync, "no marker"), ttRecreateHeads(ttH)),
			ttWant(OutcomeUpdated, ReasonRecreate, 9, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(9, ttSync))},
		{"recreate adopts it on a rewritable branch", ttEdits(ttInvalidOn(9, ttSync, ttBrokenMarker), ttRecreateHeads(ttH), func(in *TargetInput) {
			in.Branch = ttRewritable(ttSync, ttH, in.D)
		}), ttWant(OutcomeUpdated, ReasonRecreate, 9, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(9, ttSync))},
		{"recreate adopts it on an alias", ttEdits(ttAliasBranch(ttAlias, ttBranch(ttAlias, ttHA, BranchForeign)), ttInvalidOn(9, ttAlias, ""), ttRecreateHeads(ttHA)),
			ttWant(OutcomeUpdated, ReasonRecreate, 9, ttAlias, Blocks{}, ttPush(ttAlias, ttHA, false), ttEditPushed(9, ttAlias))},
		{"recreate ignores a tick in a broken body", ttEdits(ttSyncState(BranchEdited), ttInvalidOn(9, ttSync, "x"), ttRecreateHeads(ttH), ttTicked),
			ttWant(OutcomeUpdated, ReasonRecreate, 9, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(9, ttSync))},
		{"recreate of another head", ttEdits(ttSyncState(BranchEdited), ttInvalidOn(9, ttSync, ""), ttRecreateHeads(ttOld)),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttSync, Blocks{})},
		{"recreate with nothing to deliver", ttEdits(ttSyncState(BranchEdited), ttInvalidOn(9, ttSync, ""), ttRecreateHeads(ttH), ttNoD),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 9, ttSync, Blocks{})},
		{"recreate, someone else's PR on the branch", ttEdits(ttSyncState(BranchEdited), ttInvalidOn(9, ttSync, ""), ttRecreateHeads(ttH), ttForeignOn(31, ttSync)),
			ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{})},
		{"recreate of an edited branch, workflows refused: no marker to write", ttEdits(ttSyncState(BranchForeign), ttInvalidOn(9, ttSync, ""),
			ttRecreateHeads(ttH), ttDiffer, ttNoWorkflowPerm),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 9, ttSync, Blocks{})},
		{"recreate of an edited branch, workflows refused: C describes it", ttEdits(ttInvalidOn(9, ttSync, ""), ttRecreateHeads(ttH), ttDiffer, ttNoWorkflowPerm,
			func(in *TargetInput) {
				in.Branch = ttRewritable(ttSync, ttH, d[:2])
				in.Branch.State = BranchEdited
			}),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 9, ttSync, Blocks{Paused: true, UpdateBranchNeeded: true}, ttEditContent(9, ttSync))},
		{"one adopted, another not", ttEdits(ttSyncState(BranchEdited), ttInvalidOn(9, ttSync, ""), ttInvalidOn(8, ttAlias, ""), ttRecreateHeads(ttH)),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 8, ttAlias, Blocks{})},

		// adopt_unmarked takes multi-gitter's merge requests on an alias,
		// without a marker, whose branch the one-off rule lets touchmark
		// rewrite.
		{"adopt_unmarked takes multi-gitter's MR", ttEdits(ttAdoptUnmarked, ttAliasBranch(ttAlias, legacy), ttInvalidOn(40, ttAlias, "Sync from multi-gitter.")),
			ttWant(OutcomeUpdated, ReasonContent, 40, ttAlias, Blocks{}, ttPush(ttAlias, ttHA, false), ttEditPushed(40, ttAlias))},
		{"adopt_unmarked after a crash: our commit is there, the marker is not", ttEdits(ttAdoptUnmarked, ttInvalidOn(40, ttAlias, "Sync."),
			func(in *TargetInput) { ttAliasBranch(ttAlias, ttRewritable(ttAlias, ttHA, in.D))(in) }),
			ttWant(OutcomeUnchanged, "", 40, ttAlias, Blocks{}, ttEditContent(40, ttAlias))},
		{"adopt_unmarked, the branch gone", ttEdits(ttAdoptUnmarked, ttInvalidOn(40, ttAlias, "")),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 40, ttAlias, Blocks{})},
		{"adopt_unmarked, nothing to deliver", ttEdits(ttAdoptUnmarked, ttAliasBranch(ttAlias, legacy), ttInvalidOn(40, ttAlias, ""), ttNoD),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 40, ttAlias, Blocks{})},
		{"adopt_unmarked, a broken marker", ttEdits(ttAdoptUnmarked, ttAliasBranch(ttAlias, legacy), ttInvalidOn(40, ttAlias, "x\n"+ttBrokenMarker)),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 40, ttAlias, Blocks{})},
		{"adopt_unmarked, not on an alias", ttEdits(ttAdoptUnmarked, ttInvalidOn(40, ttSync, ""), func(in *TargetInput) {
			in.Branch = ttBranch(ttSync, ttH, BranchForeign)
			in.Branch.LegacyRewritable = true
		}), ttWant(OutcomeBlocked, ReasonMarkerInvalid, 40, ttSync, Blocks{})},
		{"adopt_unmarked, the branch has content of its own", ttEdits(ttAdoptUnmarked, ttAliasBranch(ttAlias, ttBranch(ttAlias, ttHA, BranchForeign)),
			ttInvalidOn(40, ttAlias, "")), ttWant(OutcomeBlocked, ReasonMarkerInvalid, 40, ttAlias, Blocks{})},
		{"a legacy branch without adopt_unmarked", ttEdits(ttAliasBranch(ttAlias, legacy), ttInvalidOn(40, ttAlias, "")),
			ttWant(OutcomeBlocked, ReasonMarkerInvalid, 40, ttAlias, Blocks{})},
		{"adopt_unmarked beside an own PR: a duplicate", ttEdits(ttWithOwn(52, ttD()), ttAdoptUnmarked, ttAliasBranch(ttAlias, legacy), ttInvalidOn(40, ttAlias, "")),
			ttWant(OutcomeUnchanged, "", 52, ttSync, Blocks{},
				Step{Kind: StepClosePR, PR: 40, Branch: ttAlias, Reason: ReasonDuplicate, Content: true}, ttClosedComment(40, ttAlias),
				ttDelete(ttAlias, ttHA), ttEdit(52, ttSync))},
	})
}

func TestDecideTargetOpenRewritable(t *testing.T) {
	d := ttD()
	// rebased is D after the base changed AGENTS.md: the same content, from
	// another blob.
	rebased := slices.Clone(d)
	rebased[1].From = oidE
	ttRun(t, []ttCase{
		{"unchanged", ttWithOwn(12, d), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"unchanged in another order", ttWithOwn(12, []Pair{d[3], d[1], d[0], d[2]}), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"behind the base: no push", ttEdits(ttWithOwn(12, d), func(in *TargetInput) {
			in.Branch.HcParent, in.Branch.E = ttOld, ttOld
		}), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"after base merges: no push", ttEdits(ttWithOwn(12, d), func(in *TargetInput) {
			in.Branch.Head, in.Branch.HcIsHead = btSHA(0x99), false
		}), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		// As in TestCrashMatrix: a run pushed D and lost its EditPR. The
		// branch brings D, the marker still names the older content: the
		// edit rewrites it, or a decline would remember the wrong content.
		{"the marker names older content", ttEdits(ttWithOwn(12, d), ttMarkerKey(Key(StreamSync, d[:2]))),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEditContent(12, ttSync))},
		{"a marker without a key", ttEdits(ttWithOwn(12, d), ttMarkerKey("")),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEditContent(12, ttSync))},
		{"content changed", ttWithOwn(12, d[:2]), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"the base changed our path: a rebase", ttEdits(ttWithOwn(12, d), func(in *TargetInput) {
			in.D, in.Key = rebased, Key(StreamSync, rebased)
		}), ttWant(OutcomeUpdated, ReasonRebase, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"base renamed", ttEdits(ttWithOwn(12, d), ttBaseOfOwn("master")), ttWant(OutcomeUpdated, ReasonBaseRenamed, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"base renamed and content changed", ttEdits(ttWithOwn(12, d[:2]), ttBaseOfOwn("master")), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"default branch unknown", ttEdits(ttWithOwn(12, d), ttBaseOfOwn("master"), func(in *TargetInput) { in.DefaultBranch = "" }),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"default branch unknown, content changed: the base stays", ttEdits(ttWithOwn(12, d[:2]), ttBaseOfOwn("master"), func(in *TargetInput) { in.DefaultBranch = "" }),
			ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), Step{Kind: StepEditPR, PR: 12, Branch: ttSync, Content: true})},
		{"nothing to deliver", ttEdits(ttWithOwn(12, d), ttNoD), ttWant(OutcomeClosed, ReasonNoDiff, 12, ttSync, Blocks{},
			ttClose(12, ttSync, ReasonNoDiff), ttClosedComment(12, ttSync), ttDelete(ttSync, ttH))},
		{"an open PR is stronger than memory", ttEdits(ttWithOwn(12, d[:2]), func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 7, Key: in.Key}}
			in.Memory.Auto = []AutoClose{{PR: 7, Key: in.Key}}
			in.CooldownDeclined, in.CooldownUntil = true, ttNow.Add(time.Hour)
		}), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"previously declined stays in the body", ttEdits(ttWithOwn(12, d), func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 5, Key: "sha256:other", Changes: ShortChanges(d[:1]), Complete: true}}
		}), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{PreviouslyDeclined: []int64{5}}, ttEdit(12, ttSync))},

		{"workflows: D needs them, granted", ttEdits(ttWithOwn(12, d), ttWfD, ttWithWorkflowPerm), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, true), ttEditPushed(12, ttSync))},
		{"workflows: D needs them, refused", ttEdits(ttWithOwn(12, d), ttWfD, ttNoWorkflowPerm), ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, Blocks{})},
		{"workflows: D needs them, refused, the base moved too", ttEdits(ttWithOwn(12, d), ttWfD, ttNoWorkflowPerm, ttDiffer),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, Blocks{})},
		{"workflows: the move needs them, granted", ttEdits(ttWithOwn(12, d[:2]), ttDiffer, ttWithWorkflowPerm), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, true), ttEditPushed(12, ttSync))},
		{"workflows: the move needs them, refused", ttEdits(ttWithOwn(12, d[:2]), ttDiffer, ttNoWorkflowPerm),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, Blocks{UpdateBranchNeeded: true}, ttEdit(12, ttSync))},
		// No rebuild, so the renamed base stays: a PR moved to main without
		// one would show master's commits.
		{"workflows: a renamed base needs them, refused", ttEdits(ttWithOwn(12, d), ttBaseOfOwn("master"), ttDiffer, ttNoWorkflowPerm),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, Blocks{UpdateBranchNeeded: true}, ttEdit(12, ttSync))},
		{"workflows: no push, no need", ttEdits(ttWithOwn(12, d), ttDiffer, ttNoWorkflowPerm), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"workflows: no such permission on the platform", ttEdits(ttWithOwn(12, d[:2]), ttWfD, ttDiffer), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, true), ttEditPushed(12, ttSync))},

		{"branch gone", ttEdits(ttWithOwn(12, d), func(in *TargetInput) { in.Branch = Branch{Name: ttSync} }),
			ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, "", false), ttEditPushed(12, ttSync))},
		{"branch gone, nothing to deliver", ttEdits(ttWithOwn(12, d), ttNoD, func(in *TargetInput) { in.Branch = Branch{Name: ttSync} }),
			ttWant(OutcomeClosed, ReasonNoDiff, 12, ttSync, Blocks{}, ttClose(12, ttSync, ReasonNoDiff), ttClosedComment(12, ttSync))},
		{"branch gone, workflows refused", ttEdits(ttWithOwn(12, d), ttWfD, ttNoWorkflowPerm, func(in *TargetInput) { in.Branch = Branch{Name: ttSync} }),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, Blocks{})},
		{"branch gone, creating it moves nothing", ttEdits(ttWithOwn(12, d), ttDiffer, ttNoWorkflowPerm, func(in *TargetInput) { in.Branch = Branch{Name: ttSync} }),
			ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, "", false), ttEditPushed(12, ttSync))},

		// A rule against force pushes: a rebuild under our open PR would force
		// the branch, and deleting it would close the PR.
		{"no force pushes: the rebuild is refused", ttEdits(ttWithOwn(12, d[:2]), ttNoForce(ttSync)),
			ttWant(OutcomeBlocked, ReasonRulesNoForce, 12, ttSync, Blocks{})},
		{"no force pushes: a renamed base is refused too", ttEdits(ttWithOwn(12, d), ttBaseOfOwn("master"), ttNoForce(ttSync)),
			ttWant(OutcomeBlocked, ReasonRulesNoForce, 12, ttSync, Blocks{})},
		{"no force pushes on another branch", ttEdits(ttWithOwn(12, d[:2]), ttNoForce(ttAlias)),
			ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"no force pushes: no push, no refusal", ttEdits(ttWithOwn(12, d), ttNoForce(ttSync)),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"no force pushes: the branch gone, created", ttEdits(ttWithOwn(12, d), ttNoForce(ttSync), func(in *TargetInput) { in.Branch = Branch{Name: ttSync} }),
			ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, "", false), ttEditPushed(12, ttSync))},
		{"no force pushes: nothing to deliver still closes", ttEdits(ttWithOwn(12, d), ttNoD, ttNoForce(ttSync)),
			ttWant(OutcomeClosed, ReasonNoDiff, 12, ttSync, Blocks{}, ttClose(12, ttSync, ReasonNoDiff), ttClosedComment(12, ttSync), ttDelete(ttSync, ttH))},
	})
}

func TestDecideTargetOpenPaused(t *testing.T) {
	d := ttD()
	own := ttWithOwn(12, d[:2])
	paused := Blocks{Paused: true}
	// edited is the sync branch with our commit bringing d[:2] and
	// someone's commits after it.
	edited := func(in *TargetInput) {
		in.Branch = ttRewritable(ttSync, ttH, d[:2])
		in.Branch.State, in.Branch.HcIsHead, in.Branch.E = BranchEdited, false, ""
	}
	ttRun(t, []ttCase{
		{"edited", ttEdits(own, ttSyncState(BranchEdited)), ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"foreign", ttEdits(own, ttSyncState(BranchForeign)), ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"edited, the marker names C", ttEdits(own, edited), ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEdit(12, ttSync))},
		// A run pushed, lost its EditPR, then someone pushed: the marker
		// must name what the paused PR carries, C.
		{"edited, the marker names older content", ttEdits(own, edited, ttMarkerKey(Key(StreamSync, d[:1]))),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEditContent(12, ttSync))},
		// A block writes the body, never the base: moving the base without a
		// rebuild would show the old base's commits.
		{"edited, the base renamed", ttEdits(own, ttSyncState(BranchEdited), ttBaseOfOwn("master")),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"edited, nothing to deliver: never closed", ttEdits(own, ttSyncState(BranchEdited), ttNoD),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, Blocks{NothingMore: true}, ttEdit(12, ttSync))},
		{"nothing to deliver wins over a tick", ttEdits(own, ttSyncState(BranchEdited), ttNoD, ttTicked),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, Blocks{NothingMore: true}, ttEdit(12, ttSync))},
		{"nothing to deliver wins over operations", ttEdits(own, ttSyncState(BranchEdited), ttNoD, ttRecreateHeads(ttH)),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, Blocks{NothingMore: true}, ttEdit(12, ttSync))},
		{"recreate ticked", ttEdits(own, ttSyncState(BranchEdited), ttTicked), ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{},
			ttConsume(12, ttSync, ttH), ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"recreate ticked on a foreign branch", ttEdits(own, ttSyncState(BranchForeign), ttTicked), ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{},
			ttConsume(12, ttSync, ttH), ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"recreate by operations", ttEdits(own, ttSyncState(BranchEdited), ttRecreateHeads(ttOld, ttH)), ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"recreate by operations, the base renamed", ttEdits(own, ttSyncState(BranchEdited), ttRecreateHeads(ttH), ttBaseOfOwn("master")),
			ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"recreate by operations for another head", ttEdits(own, ttSyncState(BranchEdited), ttRecreateHeads(ttOld)),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"a crash after consuming the control", ttEdits(own, ttSyncState(BranchEdited), ttRecreateFor(ttH)), ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{},
			ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"someone pushed after the control was consumed", ttEdits(own, ttSyncState(BranchEdited), ttRecreateFor(ttOld)),
			ttWant(OutcomeBlocked, ReasonEdited, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"ticked again after a crash", ttEdits(own, ttSyncState(BranchEdited), ttRecreateFor(ttH), ttTicked), ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{},
			ttConsume(12, ttSync, ttH), ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"recreate: D needs workflows, refused", ttEdits(own, ttSyncState(BranchEdited), ttTicked, ttWfD, ttNoWorkflowPerm),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"recreate: the move needs workflows, refused", ttEdits(own, ttSyncState(BranchEdited), ttTicked, ttDiffer, ttNoWorkflowPerm),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 12, ttSync, Blocks{Paused: true, UpdateBranchNeeded: true}, ttEdit(12, ttSync))},
		{"recreate: D needs workflows, granted", ttEdits(own, ttSyncState(BranchEdited), ttTicked, ttWfD, ttWithWorkflowPerm), ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{},
			ttConsume(12, ttSync, ttH), ttPush(ttSync, ttH, true), ttEditPushed(12, ttSync))},
		{"recreate: the move needs workflows, granted", ttEdits(own, ttSyncState(BranchEdited), ttRecreateHeads(ttH), ttDiffer, ttWithWorkflowPerm),
			ttWant(OutcomeUpdated, ReasonRecreate, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, true), ttEditPushed(12, ttSync))},
		{"recreate: force pushes refused", ttEdits(own, ttSyncState(BranchEdited), ttTicked, ttNoForce(ttSync)),
			ttWant(OutcomeBlocked, ReasonRulesNoForce, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"recreate by operations: force pushes refused", ttEdits(own, ttSyncState(BranchEdited), ttRecreateHeads(ttH), ttNoForce(ttSync)),
			ttWant(OutcomeBlocked, ReasonRulesNoForce, 12, ttSync, paused, ttEdit(12, ttSync))},
		{"recreate on an alias", ttEdits(func(in *TargetInput) {
			in.Own = []OwnPR{ttOwn(8, ttAlias, platform.Open)}
			in.Aliases = map[string]Branch{ttAlias: ttBranch(ttAlias, ttHA, BranchEdited)}
		}, ttRecreateHeads(ttHA)), ttWant(OutcomeUpdated, ReasonRecreate, 8, ttAlias, Blocks{}, ttPush(ttAlias, ttHA, false), ttEditPushed(8, ttAlias))},

		// While adopt_unmarked is active, a foreign branch the one-off rule
		// accepts is maintained like a rewritable one.
		{"a legacy-rewritable alias is rewritten", ttEdits(ttAdoptUnmarked, func(in *TargetInput) {
			in.Own = []OwnPR{ttOwn(8, ttAlias, platform.Open)}
			b := ttBranch(ttAlias, ttHA, BranchForeign)
			b.LegacyRewritable = true
			in.Aliases = map[string]Branch{ttAlias: b}
		}), ttWant(OutcomeUpdated, ReasonContent, 8, ttAlias, Blocks{}, ttPush(ttAlias, ttHA, false), ttEditPushed(8, ttAlias))},
		{"a legacy-rewritable alias without adopt_unmarked is paused", func(in *TargetInput) {
			in.Own = []OwnPR{ttOwn(8, ttAlias, platform.Open)}
			b := ttBranch(ttAlias, ttHA, BranchForeign)
			b.LegacyRewritable = true
			in.Aliases = map[string]Branch{ttAlias: b}
		}, ttWant(OutcomeBlocked, ReasonEdited, 8, ttAlias, paused, ttEdit(8, ttAlias))},
	})
}

func TestDecideTargetOpenBranchInUse(t *testing.T) {
	d := ttD()
	ttRun(t, []ttCase{
		{"someone else's PR on our branch", ttEdits(ttWithOwn(12, d[:2]), ttForeignOn(31, ttSync)),
			ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{})},
		{"never closed or deleted then", ttEdits(ttWithOwn(12, d), ttNoD, ttForeignOn(31, ttSync)),
			ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{})},
		{"recreate does not lift it", ttEdits(ttWithOwn(12, d), ttSyncState(BranchEdited), ttTicked, ttRecreateHeads(ttH), ttForeignOn(31, ttSync)),
			ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{})},
		{"the first of several", ttEdits(ttWithOwn(12, d), ttForeignOn(33, ttSync), ttForeignOn(31, ttSync)),
			ttWant(OutcomeBlocked, ReasonBranchInUse, 33, ttSync, Blocks{})},
		{"someone else's PR on another branch", ttEdits(ttWithOwn(12, d[:2]), ttForeignOn(31, ttAlias), ttForeignOn(32, "feature/x")),
			ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"our PR on an alias, someone else's on the sync branch", ttEdits(ttForeignOn(31, ttSync), func(in *TargetInput) {
			in.Own = []OwnPR{ttOwn(8, ttAlias, platform.Open)}
			in.Aliases = map[string]Branch{ttAlias: ttRewritable(ttAlias, ttHA, d[:2])}
		}), ttWant(OutcomeUpdated, ReasonContent, 8, ttAlias, Blocks{}, ttPush(ttAlias, ttHA, false), ttEditPushed(8, ttAlias))},
	})
}

func TestDecideTargetOpenAliasesAndDuplicates(t *testing.T) {
	d := ttD()
	alias := func(n int64, name, head string, b Branch) func(*TargetInput) {
		return func(in *TargetInput) {
			o := ttOwn(n, name, platform.Open)
			o.Marker.Key = b.CKey
			in.Own = append(in.Own, o)
			if in.Aliases == nil {
				in.Aliases = map[string]Branch{}
			}
			b.Name = ""
			if b.State != BranchAbsent {
				b.Head = head
			}
			in.Aliases[name] = b
		}
	}
	rw := func(c []Pair) Branch { return ttRewritable("", "", c) }
	ttRun(t, []ttCase{
		{"on an alias", alias(8, ttAlias, ttHA, rw(d)), ttWant(OutcomeUnchanged, "", 8, ttAlias, Blocks{}, ttEdit(8, ttAlias))},
		{"on an alias, content changed", alias(8, ttAlias, ttHA, rw(d[:2])), ttWant(OutcomeUpdated, ReasonContent, 8, ttAlias, Blocks{},
			ttPush(ttAlias, ttHA, false), ttEditPushed(8, ttAlias))},
		{"on an alias, nothing to deliver", ttEdits(alias(8, ttAlias, ttHA, rw(d)), ttNoD), ttWant(OutcomeClosed, ReasonNoDiff, 8, ttAlias, Blocks{},
			ttClose(8, ttAlias, ReasonNoDiff), ttClosedComment(8, ttAlias), ttDelete(ttAlias, ttHA))},
		{"on an alias nobody classified", func(in *TargetInput) { in.Own = []OwnPR{ttOwn(8, ttAlias, platform.Open)} },
			ttWant(OutcomeUpdated, ReasonContent, 8, ttAlias, Blocks{}, ttPush(ttAlias, "", false), ttEditPushed(8, ttAlias))},
		// A closed duplicate's rewritable branch goes.
		{"duplicate on an alias", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, rw(d))), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{},
			ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttDelete(ttAlias, ttHA), ttEdit(12, ttSync))},
		{"the sync branch's PR is kept, even older", ttEdits(alias(15, ttAlias, ttHA, rw(d)), ttWithOwn(12, d)), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{},
			ttClose(15, ttAlias, ReasonDuplicate), ttClosedComment(15, ttAlias), ttDelete(ttAlias, ttHA), ttEdit(12, ttSync))},
		{"else the newest", ttEdits(alias(15, ttAlias, ttHA, rw(d[:2])), alias(9, ttAlias2, ttHA2, rw(d))), ttWant(OutcomeUpdated, ReasonContent, 15, ttAlias, Blocks{},
			ttClose(9, ttAlias2, ReasonDuplicate), ttClosedComment(9, ttAlias2), ttDelete(ttAlias2, ttHA2), ttPush(ttAlias, ttHA, false), ttEditPushed(15, ttAlias))},
		{"a duplicate on an edited branch stays open", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, Branch{State: BranchEdited})),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"a duplicate on a foreign branch stays open", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, Branch{State: BranchForeign})),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttEdit(12, ttSync))},
		{"a duplicate on a missing branch", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, "", Branch{})), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{},
			ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttEdit(12, ttSync))},
		{"a duplicate on a branch someone else's PR uses: closed, the branch stays", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, rw(d)), ttForeignOn(31, ttAlias)),
			ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{}, ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttEdit(12, ttSync))},
		{"two duplicates on one branch: deleted once, after both", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, rw(d)), func(in *TargetInput) {
			o := ttOwn(6, ttAlias, platform.Open)
			o.Marker.Key = Key(StreamSync, d)
			in.Own = append(in.Own, o)
		}), ttWant(OutcomeUnchanged, "", 12, ttSync, Blocks{},
			ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttClose(6, ttAlias, ReasonDuplicate), ttClosedComment(6, ttAlias),
			ttDelete(ttAlias, ttHA), ttEdit(12, ttSync))},
		{"a second PR on the sync branch", ttEdits(ttWithOwn(14, d), func(in *TargetInput) {
			o := ttOwn(12, ttSync, platform.Open)
			o.PR.Base = "master"
			in.Own = append(in.Own, o)
		}), ttWant(OutcomeUnchanged, "", 14, ttSync, Blocks{}, ttClose(12, ttSync, ReasonDuplicate), ttClosedComment(12, ttSync), ttEdit(14, ttSync))},
		{"duplicates close before the branch goes", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, rw(d)), ttNoD), ttWant(OutcomeClosed, ReasonNoDiff, 12, ttSync, Blocks{},
			ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttDelete(ttAlias, ttHA),
			ttClose(12, ttSync, ReasonNoDiff), ttClosedComment(12, ttSync), ttDelete(ttSync, ttH))},
		{"duplicates close when the branch is in use", ttEdits(ttWithOwn(12, d), alias(8, ttAlias, ttHA, rw(d)), ttForeignOn(31, ttSync)),
			ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{}, ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttDelete(ttAlias, ttHA))},
		{"closed and merged PRs are not open", ttEdits(ttWithOwn(12, d[:2]), func(in *TargetInput) {
			in.Own = append([]OwnPR{ttOwn(20, ttSync, platform.Closed), ttOwn(18, ttAlias, platform.Merged)}, in.Own...)
		}), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{}, ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
		{"upkeep, duplicates, then the kept PR", ttEdits(ttWithOwn(12, d[:2]), alias(8, ttAlias, ttHA, rw(d)), func(in *TargetInput) {
			in.Memory.ToRevoke = []int64{3}
		}), ttWant(OutcomeUpdated, ReasonContent, 12, ttSync, Blocks{},
			ttRevoke(3), ttClose(8, ttAlias, ReasonDuplicate), ttClosedComment(8, ttAlias), ttDelete(ttAlias, ttHA), ttPush(ttSync, ttH, false), ttEditPushed(12, ttSync))},
	})
}

func TestDecideTargetNoOpenPR(t *testing.T) {
	d := ttD()
	rewritable := func(c []Pair) func(*TargetInput) {
		return func(in *TargetInput) { in.Branch = ttRewritable(ttSync, ttH, c) }
	}
	legacySync := func(in *TargetInput) {
		in.Branch = ttBranch(ttSync, ttH, BranchForeign)
		in.Branch.LegacyRewritable = true
	}
	opened := func(steps ...Step) TargetDecision { return ttWant(OutcomeOpened, "", 0, ttSync, Blocks{}, steps...) }
	ttRun(t, []ttCase{
		{"no branch", func(*TargetInput) {}, opened(ttPush(ttSync, "", false), ttCreate())},
		{"nothing to deliver", ttNoD, ttWant(OutcomeUnchanged, "", 0, "", Blocks{})},
		{"nothing to deliver, the old branch stays", ttEdits(ttNoD, rewritable(d)), ttWant(OutcomeUnchanged, "", 0, "", Blocks{})},
		{"after a merged PR", func(in *TargetInput) { in.Own = []OwnPR{ttOwn(3, ttSync, platform.Merged)} }, opened(ttPush(ttSync, "", false), ttCreate())},
		{"after a closed PR memory does not hold", func(in *TargetInput) { in.Own = []OwnPR{ttOwn(3, ttSync, platform.Closed)} },
			opened(ttPush(ttSync, "", false), ttCreate())},
		{"a run crashed between push and PR", rewritable(d), opened(ttCreate())},
		{"a run crashed, pairs in another order", rewritable([]Pair{d[2], d[0], d[3], d[1]}), opened(ttCreate())},
		{"our branch behind the base", ttEdits(rewritable(d), func(in *TargetInput) { in.Branch.HcParent, in.Branch.E = ttOld, ttOld }),
			opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"our branch with base merges", ttEdits(rewritable(d), func(in *TargetInput) { in.Branch.Head, in.Branch.HcIsHead = btSHA(0x99), false }),
			opened(ttPush(ttSync, btSHA(0x99), false), ttCreate())},
		{"our branch with other content", rewritable(d[:2]), opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"someone else's branch", ttSyncState(BranchForeign), ttWant(OutcomeBlocked, ReasonBranchTaken, 0, ttSync, Blocks{})},
		{"someone else's branch, multi-gitter's", ttEdits(ttAdoptUnmarked, legacySync), opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"the migration rule set on the input", ttEdits(ttAdoptUnmarked, ttSyncState(BranchForeign), func(in *TargetInput) { in.LegacyRewritable = true }),
			opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"multi-gitter's branch after adopt_unmarked expired", legacySync, ttWant(OutcomeBlocked, ReasonBranchTaken, 0, ttSync, Blocks{})},
		{"someone else's branch rebuilt by operations", ttEdits(ttSyncState(BranchForeign), ttRecreateHeads(ttH)), opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"operations name another head", ttEdits(ttSyncState(BranchForeign), ttRecreateHeads(ttOld)), ttWant(OutcomeBlocked, ReasonBranchTaken, 0, ttSync, Blocks{})},
		{"an edited branch", ttSyncState(BranchEdited), ttWant(OutcomeBlocked, ReasonEdited, 0, ttSync, Blocks{})},
		{"an edited branch rebuilt by operations", ttEdits(ttSyncState(BranchEdited), ttRecreateHeads(ttH)), opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"a tick needs an open PR", ttEdits(ttSyncState(BranchEdited), ttTicked), ttWant(OutcomeBlocked, ReasonEdited, 0, ttSync, Blocks{})},
		{"an unknown branch state", func(in *TargetInput) { in.Branch.State = 9 }, ttWant(OutcomeBlocked, ReasonEdited, 0, ttSync, Blocks{})},
		{"someone else's PR on the sync branch", ttForeignOn(31, ttSync), ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{})},
		{"someone else's PR on a crashed run's branch", ttEdits(rewritable(d), ttForeignOn(31, ttSync)), ttWant(OutcomeBlocked, ReasonBranchInUse, 31, ttSync, Blocks{})},
		{"someone else's PR on an alias", ttForeignOn(31, ttAlias), opened(ttPush(ttSync, "", false), ttCreate())},
		{"previously declined", func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 5, Key: "sha256:other", Changes: ShortChanges(d[:1]), Complete: true}}
		}, ttWant(OutcomeOpened, "", 0, ttSync, Blocks{PreviouslyDeclined: []int64{5}}, ttPush(ttSync, "", false), ttCreate())},
		{"reproposed", func(in *TargetInput) { in.Memory.ToRevoke = []int64{7} }, opened(ttRevoke(7), ttPush(ttSync, "", false), ttCreate())},

		{"workflows: new branch, granted", ttEdits(ttWfD, ttWithWorkflowPerm), opened(ttPush(ttSync, "", true), ttCreate())},
		{"workflows: new branch, refused", ttEdits(ttWfD, ttNoWorkflowPerm), ttWant(OutcomeBlocked, ReasonPermissionWflw, 0, ttSync, Blocks{})},
		{"workflows: new branch, no such permission", ttWfD, opened(ttPush(ttSync, "", true), ttCreate())},
		{"workflows: a new branch moves nothing", ttEdits(ttDiffer, ttNoWorkflowPerm), opened(ttPush(ttSync, "", false), ttCreate())},
		{"workflows: moving, D needs them, refused", ttEdits(rewritable(d[:2]), ttWfD, ttNoWorkflowPerm, ttDiffer),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 0, ttSync, Blocks{})},
		{"workflows: moving needs them, refused: a fresh branch", ttEdits(rewritable(d[:2]), ttDiffer, ttNoWorkflowPerm),
			opened(ttRecreateBranch(ttSync, ttH), ttCreate())},
		{"workflows: moving needs them, granted", ttEdits(rewritable(d[:2]), ttDiffer, ttWithWorkflowPerm), opened(ttPush(ttSync, ttH, true), ttCreate())},
		{"workflows: moving, no such permission", ttEdits(rewritable(d[:2]), ttDiffer), opened(ttPush(ttSync, ttH, true), ttCreate())},
		{"workflows: multi-gitter's branch, refused: a fresh branch", ttEdits(ttAdoptUnmarked, legacySync, ttDiffer, ttNoWorkflowPerm),
			opened(ttRecreateBranch(ttSync, ttH), ttCreate())},
		{"workflows: an edited branch rebuilt, refused: a fresh branch", ttEdits(ttSyncState(BranchEdited), ttRecreateHeads(ttH), ttDiffer, ttNoWorkflowPerm),
			opened(ttRecreateBranch(ttSync, ttH), ttCreate())},
		{"workflows: an edited branch rebuilt, D needs them, refused", ttEdits(ttSyncState(BranchEdited), ttRecreateHeads(ttH), ttWfD, ttNoWorkflowPerm),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 0, ttSync, Blocks{})},
		{"workflows: a crashed run needs no push", ttEdits(ttWfD, ttNoWorkflowPerm, ttDiffer, func(in *TargetInput) {
			in.Branch = ttRewritable(ttSync, ttH, in.D)
		}), opened(ttCreate())},

		// Without our open PR, a branch a rule keeps from force pushes is
		// deleted with a lease and created anew: neither forces it.
		{"no force pushes: moving is a fresh branch", ttEdits(rewritable(d[:2]), ttNoForce(ttSync)), opened(ttRecreateBranch(ttSync, ttH), ttCreate())},
		{"no force pushes: a fresh branch asks Workflows for D only", ttEdits(rewritable(d[:2]), ttWfD, ttDiffer, ttWithWorkflowPerm, ttNoForce(ttSync)),
			opened(ttRecreateBranchWf(ttSync, ttH), ttCreate())},
		{"no force pushes: a new branch", ttNoForce(ttSync), opened(ttPush(ttSync, "", false), ttCreate())},
		// GitHub's ruleset form pairs "Restrict deletions" with "Block
		// force pushes": then the fresh branch is refused too, before any
		// write, and a plan says so.
		{"no force pushes, no deletions: blocked", ttEdits(rewritable(d[:2]), ttNoForce(ttSync), ttNoDelete(ttSync)),
			ttWant(OutcomeBlocked, ReasonRulesNoForce, 0, ttSync, Blocks{})},
		{"no deletions alone: a force push", ttEdits(rewritable(d[:2]), ttNoDelete(ttSync)), opened(ttPush(ttSync, ttH, false), ttCreate())},
		{"no deletions, a new branch", ttNoDelete(ttSync), opened(ttPush(ttSync, "", false), ttCreate())},
		{"workflows: moving needs them, refused, no deletions: blocked", ttEdits(rewritable(d[:2]), ttDiffer, ttNoWorkflowPerm, ttNoDelete(ttSync)),
			ttWant(OutcomeBlocked, ReasonPermissionWflw, 0, ttSync, Blocks{})},
		{"no force pushes: an edited branch rebuilt by operations", ttEdits(ttSyncState(BranchEdited), ttRecreateHeads(ttH), ttNoForce(ttSync)),
			opened(ttRecreateBranch(ttSync, ttH), ttCreate())},
		{"no force pushes: a crashed run needs no push", ttEdits(ttNoForce(ttSync), func(in *TargetInput) {
			in.Branch = ttRewritable(ttSync, ttH, in.D)
		}), opened(ttCreate())},
	})
}

func TestDecideTargetMemory(t *testing.T) {
	d := ttD()
	key := Key(StreamSync, d)
	ttRun(t, []ttCase{
		{"declined by key", func(in *TargetInput) { in.Memory.Declines = []Decline{{PR: 7, Key: key}} },
			ttWant(OutcomeDeclined, "", 7, "", Blocks{})},
		{"declined by the union of two", func(in *TargetInput) {
			in.Memory.Declines = []Decline{
				{PR: 9, Key: "sha256:nine", Changes: ShortChanges(d[:2]), Complete: true},
				{PR: 7, Key: "sha256:seven", Changes: ShortChanges(d[2:]), Complete: true},
			}
		}, ttWant(OutcomeDeclined, "", 9, "", Blocks{})},
		{"the newest covering decline", func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 12, Key: key}, {PR: 30, Key: "sha256:other", Changes: ShortChanges(d[:1]), Complete: true}, {PR: 7, Key: key}}
		}, ttWant(OutcomeDeclined, "", 12, "", Blocks{})},
		{"part of D declined: a new PR", func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 7, Key: "sha256:other", Changes: ShortChanges(d[:3]), Complete: true}}
		}, ttWant(OutcomeOpened, "", 0, ttSync, Blocks{PreviouslyDeclined: []int64{7}}, ttPush(ttSync, "", false), ttCreate())},
		{"third auto-close", func(in *TargetInput) {
			in.CooldownDeclined = true
			in.Memory.Auto = []AutoClose{{PR: 21, Key: "sha256:other"}, {PR: 19, Key: key}, {PR: 15, Key: key}}
		}, ttWant(OutcomeDeclined, "", 19, "", Blocks{})},
		{"third auto-close, not in the window", func(in *TargetInput) { in.CooldownDeclined = true }, ttWant(OutcomeDeclined, "", 0, "", Blocks{})},
		{"a decline names its PR before an auto-close", func(in *TargetInput) {
			in.CooldownDeclined = true
			in.Memory.Auto = []AutoClose{{PR: 19, Key: key}}
			in.Memory.Declines = []Decline{{PR: 7, Key: key}}
		}, ttWant(OutcomeDeclined, "", 7, "", Blocks{})},
		{"cooling down", func(in *TargetInput) {
			in.CooldownUntil = ttNow.Add(24 * time.Hour)
			in.Memory.Auto = []AutoClose{{PR: 21, Key: "sha256:other"}, {PR: 19, Key: key}}
		}, ttWant(OutcomeDeferred, ReasonCooldown, 19, "", Blocks{})},
		{"cooled down", func(in *TargetInput) {
			in.CooldownUntil = ttNow
			in.Memory.Auto = []AutoClose{{PR: 19, Key: key}}
		}, ttWant(OutcomeOpened, "", 0, ttSync, Blocks{}, ttPush(ttSync, "", false), ttCreate())},
		{"declined before cooling down and branch in use", ttEdits(ttForeignOn(31, ttSync), func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 7, Key: key}}
			in.CooldownUntil = ttNow.Add(time.Hour)
		}), ttWant(OutcomeDeclined, "", 7, "", Blocks{})},
		{"cooling down before branch in use", ttEdits(ttForeignOn(31, ttSync), func(in *TargetInput) { in.CooldownUntil = ttNow.Add(time.Hour) }),
			ttWant(OutcomeDeferred, ReasonCooldown, 0, "", Blocks{})},
		{"declined, the old branch stays", ttEdits(ttSyncState(BranchEdited), func(in *TargetInput) {
			in.Memory.Declines = []Decline{{PR: 7, Key: key}}
		}), ttWant(OutcomeDeclined, "", 7, "", Blocks{})},
	})
}

// TestDecideTargetDoesNotMutateInput: a decision with every kind of step
// leaves its input as it was.
func TestDecideTargetDoesNotMutateInput(t *testing.T) {
	d := ttD()
	in := ttInput()
	ttWithOwn(12, []Pair{d[3], d[2]})(&in)
	in.Own = append(in.Own, ttOwn(8, ttAlias, platform.Open))
	in.Aliases = map[string]Branch{ttAlias: ttRewritable(ttAlias, ttHA, []Pair{d[1], d[0]})}
	in.Memory = Memory{ToRevoke: []int64{3}, ToAck: []int64{4}, Declines: []Decline{{PR: 4, Key: "sha256:x", Changes: ShortChanges(d[:1]), Complete: true}}}
	in.MarkerInvalid = []platform.PR{ttInvalid(10, ttAlias2, "")}
	in.Aliases[ttAlias2] = Branch{Name: ttAlias2, Head: ttHA2, State: BranchForeign, LegacyRewritable: true}
	in.Ops.AdoptUnmarked = true
	before, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := DecideTarget(in)
	after, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("DecideTarget modified its input:\n%s\n%s", before, after)
	}
	if len(got.Steps) < 5 {
		t.Fatalf("fixture: want a decision with many steps, got\n%s", ttFormat(got))
	}
}

func TestStepKindString(t *testing.T) {
	want := map[StepKind]string{
		StepPush: "push", StepRecreateBranch: "recreate-branch", StepDeleteBranch: "delete-branch", StepCreatePR: "create-pr",
		StepEditPR: "edit-pr", StepClosePR: "close-pr", StepComment: "comment", StepAck: "ack", StepRevoke: "revoke",
		StepConsumeRecreate: "consume-recreate", 0: "unknown", 99: "unknown",
	}
	for k, w := range want {
		if got := k.String(); got != w {
			t.Errorf("%d.String() = %q, want %q", k, got, w)
		}
	}
}

// ttReasons are the outcome and reason pairs DecideTarget may return
// (report/v1).
var ttReasons = map[string][]string{
	OutcomeOpened:    {""},
	OutcomeUpdated:   {ReasonContent, ReasonRebase, ReasonRecreate, ReasonBaseRenamed},
	OutcomeUnchanged: {""},
	OutcomeClosed:    {ReasonNoDiff},
	OutcomeDeclined:  {""},
	OutcomeBlocked:   {ReasonMarkerInvalid, ReasonBranchInUse, ReasonEdited, ReasonBranchTaken, ReasonPermissionWflw, ReasonRulesNoForce},
	OutcomeDeferred:  {ReasonCooldown},
}

// ttAdoptable restates, independently of DecideTarget, when operations.yml
// lets touchmark take over a MarkerInvalid PR.
func ttAdoptable(in TargetInput, pr platform.PR, branch func(string) Branch) (ok, rebuild bool) {
	b := branch(pr.Head)
	if pr.State != platform.Open || len(in.D) == 0 {
		return false, false
	}
	if b.Head != "" && slices.Contains(in.Ops.RecreateHeads, b.Head) {
		return true, true
	}
	legacy := b.State == BranchForeign && in.Ops.AdoptUnmarked && (b.LegacyRewritable || (b.Name == in.Branch.Name && in.LegacyRewritable))
	return in.Ops.AdoptUnmarked && pr.Head != in.Branch.Name && !strings.Contains(pr.Body, "<!-- touchmark:") &&
		(b.State == BranchRewritable || legacy), false
}

// ttCheck verifies the promises of DecideTarget on one decision: memory
// upkeep first; no branch moved or deleted under someone else's open PR;
// no push without an open PR of ours on the branch or a PR opened from it;
// D never empty on a push; leases on the heads the input names; no close
// of a PR whose branch carries people's commits; steps in a legal order;
// Content and Base as documented on Step; Body and PreviouslyDeclined as
// documented; valid outcomes.
func ttCheck(in TargetInput, d TargetDecision) error {
	branch := func(name string) Branch {
		if name == in.Branch.Name {
			return in.Branch
		}
		if b, ok := in.Aliases[name]; ok {
			if b.Name == "" {
				b.Name = name
			}
			return b
		}
		return Branch{Name: name}
	}
	legacy := func(b Branch) bool {
		return b.State == BranchForeign && in.Ops.AdoptUnmarked && (b.LegacyRewritable || (b.Name == in.Branch.Name && in.LegacyRewritable))
	}
	foreign := func(name string) bool {
		return slices.ContainsFunc(in.ForeignOpen, func(pr platform.PR) bool { return pr.Head == name })
	}
	open := map[int64]OwnPR{}
	for _, o := range in.Own {
		if o.PR.State == platform.Open {
			open[o.PR.Number] = o
		}
	}

	if !slices.Contains(ttReasons[d.Outcome], d.Reason) {
		return fmt.Errorf("outcome %s:%s is not a report/v1 pair", d.Outcome, d.Reason)
	}
	var prefix []Step
	for _, n := range in.Memory.ToRevoke {
		prefix = append(prefix, ttRevoke(n))
	}
	for _, n := range in.Memory.ToAck {
		ack := ttAck(n)
		if slices.ContainsFunc(in.Memory.Auto, func(a AutoClose) bool { return a.PR == n }) {
			ack[1].Reason = CommentAutoDeclined
		}
		prefix = append(prefix, ack...)
	}
	if len(d.Steps) < len(prefix) || !slices.Equal(d.Steps[:len(prefix)], prefix) {
		return errors.New("memory upkeep does not come first, in order")
	}
	steps := d.Steps[len(prefix):]

	adopted := map[int64]bool{}
	allAdoptable := true
	for _, pr := range in.MarkerInvalid {
		if ok, _ := ttAdoptable(in, pr, branch); ok {
			adopted[pr.Number] = true
			continue
		}
		allAdoptable = false
	}
	switch {
	case d.Reason == ReasonMarkerInvalid:
		if len(steps) > 0 || allAdoptable {
			return errors.New("marker-invalid with other writes, or with every broken marker adoptable")
		}
	case !allAdoptable:
		return errors.New("a broken marker that operations.yml does not adopt must block every other write")
	default:
		for _, pr := range in.MarkerInvalid {
			open[pr.Number] = OwnPR{PR: pr, Alias: pr.Head != in.Branch.Name}
		}
	}

	closed := map[int64]int{} // PR → index of its StepClosePR
	pushed := map[string]int{}
	created := -1
	for i, s := range steps {
		b := branch(s.Branch)
		switch s.Kind {
		case StepRevoke, StepAck:
			return fmt.Errorf("step %d: %s outside memory upkeep", i, s.Kind)
		case StepPush, StepRecreateBranch, StepDeleteBranch:
			if s.PR != 0 {
				return fmt.Errorf("step %d: %s names PR %d", i, s.Kind, s.PR)
			}
			if foreign(s.Branch) {
				return fmt.Errorf("step %d: %s of %s, which carries someone else's open PR", i, s.Kind, s.Branch)
			}
			if want := b.Head; s.Expect != want || (s.Kind != StepPush && want == "") {
				return fmt.Errorf("step %d: %s of %s leases on %q, the head is %q", i, s.Kind, s.Branch, s.Expect, want)
			}
		}
		if (s.Content || s.Base != "") && s.Kind != StepEditPR && (s.Kind != StepClosePR || s.Base != "") {
			return fmt.Errorf("step %d: %s with Content or Base", i, s.Kind)
		}
		switch s.Kind {
		case StepPush, StepRecreateBranch:
			if len(in.D) == 0 {
				return fmt.Errorf("step %d: %s of an empty D would set the head to B (I5)", i, s.Kind)
			}
			ours := false
			for n, o := range open {
				if _, gone := closed[n]; o.PR.Head == s.Branch && !gone {
					ours = true
				}
			}
			opens := slices.ContainsFunc(steps, func(c Step) bool { return c.Kind == StepCreatePR && c.Branch == s.Branch })
			if !ours && !opens {
				return fmt.Errorf("step %d: %s of %s without an open PR of ours on it or a PR opened from it", i, s.Kind, s.Branch)
			}
			if s.NeedWorkflows && in.PlatformWorkflowPerm && !in.CanWorkflows {
				return fmt.Errorf("step %d: %s needs a Workflows permission the identity cannot get", i, s.Kind)
			}
			if TouchesWorkflows(in.D) && !s.NeedWorkflows {
				return fmt.Errorf("step %d: %s writes workflows without asking for the permission", i, s.Kind)
			}
			if s.Kind == StepRecreateBranch && s.NeedWorkflows != TouchesWorkflows(in.D) {
				return fmt.Errorf("step %d: a fresh branch needs Workflows only for D", i)
			}
			if s.Kind == StepPush && s.Expect != "" && slices.Contains(in.NoForcePush, s.Branch) {
				return fmt.Errorf("step %d: forces %s, on which a rule forbids force pushes", i, s.Branch)
			}
			if _, again := pushed[s.Branch]; again {
				return fmt.Errorf("step %d: %s pushed twice", i, s.Branch)
			}
			pushed[s.Branch] = i
		case StepDeleteBranch:
			if b.State != BranchRewritable && !legacy(b) {
				return fmt.Errorf("step %d: deletes %s, which is %s", i, s.Branch, b.State)
			}
			for n, o := range open {
				if at, ok := closed[n]; o.PR.Head == s.Branch && (!ok || at > i) {
					return fmt.Errorf("step %d: deletes %s under the open PR #%d", i, s.Branch, n)
				}
			}
		case StepClosePR:
			o, ok := open[s.PR]
			if !ok || s.Branch != o.PR.Head {
				return fmt.Errorf("step %d: closes #%d, not an open PR of ours on %s", i, s.PR, s.Branch)
			}
			if (b.State == BranchEdited || b.State == BranchForeign) && !legacy(b) {
				return fmt.Errorf("step %d: closes #%d on %s, which is %s", i, s.PR, s.Branch, b.State)
			}
			if s.Reason != ReasonNoDiff && s.Reason != ReasonDuplicate {
				return fmt.Errorf("step %d: closes for %q", i, s.Reason)
			}
			if adopted[s.PR] && !s.Content {
				return fmt.Errorf("step %d: closes the adopted #%d without writing its marker's content", i, s.PR)
			}
			if i+1 >= len(steps) || steps[i+1] != ttClosedComment(s.PR, s.Branch) {
				return fmt.Errorf("step %d: the close of #%d is not followed by its comment", i, s.PR)
			}
			if _, twice := closed[s.PR]; twice {
				return fmt.Errorf("step %d: closes #%d twice", i, s.PR)
			}
			closed[s.PR] = i
		case StepComment:
			if at, ok := closed[s.PR]; !ok || at != i-1 || s.Reason != OutcomeClosed {
				return fmt.Errorf("step %d: a comment on #%d that does not follow its close", i, s.PR)
			}
		case StepEditPR, StepConsumeRecreate:
			o, ok := open[s.PR]
			if _, gone := closed[s.PR]; !ok || gone || s.PR != d.PR || s.Branch != o.PR.Head {
				return fmt.Errorf("step %d: %s of #%d, not the kept open PR of ours", i, s.Kind, s.PR)
			}
			if s.Kind == StepEditPR {
				if i != len(steps)-1 {
					return fmt.Errorf("step %d: the edit of the PR is not the last step", i)
				}
				// Content and Base follow the push (Step): D after one, else C
				// whenever the marker does not name it.
				_, moved := pushed[s.Branch]
				stale := b.Hc != "" && b.CKey != "" && o.Marker.Key != b.CKey
				switch {
				case moved && (!s.Content || s.Base != in.DefaultBranch):
					return fmt.Errorf("step %d: the edit after a push has Content %v and base %q", i, s.Content, s.Base)
				case !moved && (s.Base != "" || s.Content != stale):
					return fmt.Errorf("step %d: an edit without a push has Content %v (stale marker %v) and base %q", i, s.Content, stale, s.Base)
				case adopted[s.PR] && !s.Content:
					return fmt.Errorf("step %d: an edit of the adopted #%d that does not write its marker's content", i, s.PR)
				}
				break
			}
			if adopted[s.PR] {
				return fmt.Errorf("step %d: consumes a tick in the body of the adopted #%d", i, s.PR)
			}
			if !in.RecreateTicked || s.Expect != b.Head || b.Head == "" {
				return fmt.Errorf("step %d: consumes a recreate with %q (ticked %v, head %q)", i, s.Expect, in.RecreateTicked, b.Head)
			}
			if i+1 >= len(steps) || steps[i+1].Kind != StepPush || steps[i+1].Branch != s.Branch || steps[i+1].Expect != s.Expect {
				return fmt.Errorf("step %d: the consumed recreate is not followed by the push", i)
			}
		case StepCreatePR:
			switch {
			case created >= 0:
				return errors.New("two PRs created")
			case len(open) > 0:
				return errors.New("a PR created next to an open PR of ours")
			case s.Branch != in.Branch.Name || s.PR != 0:
				return fmt.Errorf("a PR created from %s, not the sync branch", s.Branch)
			case i != len(steps)-1:
				return errors.New("the PR is not created last")
			case len(in.D) == 0:
				return errors.New("a PR created with nothing to deliver")
			}
			created = i
		default:
			return fmt.Errorf("step %d: unknown kind %d", i, s.Kind)
		}
	}

	if (created >= 0) != (d.Outcome == OutcomeOpened) {
		return errors.New("opened without creating a PR, or the reverse")
	}
	if created >= 0 {
		if declined, _ := in.Memory.IsDeclined(in.D, in.Key); declined || in.CooldownDeclined || in.CooldownUntil.After(in.Now) {
			return errors.New("a PR opened against memory (I4)")
		}
	}
	body := slices.ContainsFunc(d.Steps, func(s Step) bool { return s.Kind == StepCreatePR || s.Kind == StepEditPR })
	if d.Body != body {
		return fmt.Errorf("Body = %v with steps that render %v", d.Body, body)
	}
	if want := in.Memory.Overlap(in.D); !body && d.Blocks.PreviouslyDeclined != nil || body && !slices.Equal(d.Blocks.PreviouslyDeclined, want) {
		return fmt.Errorf("PreviouslyDeclined = %v, want %v", d.Blocks.PreviouslyDeclined, want)
	}
	if !body && (d.Blocks.Paused || d.Blocks.NothingMore || d.Blocks.UpdateBranchNeeded) {
		return fmt.Errorf("blocks %+v that no body shows", d.Blocks)
	}
	if d.Blocks.Paused && d.Blocks.NothingMore {
		return errors.New("paused and nothing more at once")
	}
	switch d.Outcome {
	case OutcomeDeclined, OutcomeDeferred:
		if len(steps) > 0 || len(open) > 0 {
			return fmt.Errorf("%s writes or has an open PR of ours", d.Outcome)
		}
	case OutcomeUnchanged:
		if len(open) == 0 && (len(steps) > 0 || len(in.D) > 0) {
			return errors.New("unchanged without an open PR must mean nothing to deliver and no write")
		}
	}
	return nil
}

// ttGen draws a TargetInput from a small world, so that heads, branches,
// PRs and operations collide often.
func ttGen(r *rand.Rand) TargetInput {
	heads := []string{ttH, ttHA, ttHA2, btSHA(0x4)}
	head := func() string { return heads[r.IntN(len(heads))] }
	cands := ttWD()
	pairs := func() []Pair {
		var out []Pair
		for _, p := range cands {
			if r.IntN(2) == 0 {
				out = append(out, p)
			}
		}
		return out
	}
	in := TargetInput{Stream: StreamSync, B: ttB, DefaultBranch: ttMain, Now: ttNow}
	if r.IntN(6) > 0 {
		in.D = pairs()
		if len(in.D) > 0 {
			in.Key = Key(StreamSync, in.D)
		}
	}
	if len(in.D) > 0 && r.IntN(8) == 0 {
		// The base changed a path of D: only From differs from a branch that
		// brings in.D as it was.
		in.D = slices.Clone(in.D)
		in.D[0].From = oidE
		in.Key = Key(StreamSync, in.D)
	}
	if r.IntN(12) == 0 {
		in.DefaultBranch = ""
	}
	gen := func(name string) Branch {
		b := Branch{Name: name}
		switch r.IntN(7) {
		case 0:
		case 1, 2, 3:
			b.State, b.Head = BranchRewritable, head()
			b.C = pairs()
			if r.IntN(2) == 0 {
				b.C = slices.Clone(in.D)
				if len(b.C) > 0 && r.IntN(4) == 0 {
					b.C[0].From = oidB
				}
			}
			b.CKey = Key(StreamSync, b.C)
			b.HcIsHead = r.IntN(3) > 0
			b.Hc = b.Head
			if !b.HcIsHead {
				b.Hc = btSHA(0x77)
			}
			b.HcParent = []string{ttB, ttOld}[r.IntN(2)]
			b.E = b.HcParent
		case 4:
			b.State, b.Head = BranchForeign, head()
			b.LegacyRewritable = r.IntN(2) == 0
		case 5:
			b.State, b.Head = BranchEdited, head()
			if r.IntN(2) == 0 {
				b.C = pairs()
				b.CKey, b.Hc, b.HcParent = Key(StreamSync, b.C), btSHA(0x78), ttB
			}
		case 6:
			b.State, b.Head = BranchForeign, head()
		}
		return b
	}
	in.Branch = gen(ttSync)
	names := []string{ttSync, ttAlias, ttAlias2}
	for _, a := range names[1:] {
		if r.IntN(4) > 0 {
			if in.Aliases == nil {
				in.Aliases = map[string]Branch{}
			}
			in.Aliases[a] = gen(a)
		}
	}
	branchOf := func(name string) Branch {
		if name == ttSync {
			return in.Branch
		}
		return in.Aliases[name]
	}
	n := int64(50)
	for range r.IntN(4) {
		n -= int64(1 + r.IntN(4))
		o := ttOwn(n, names[r.IntN(len(names))], []platform.PRState{platform.Open, platform.Open, platform.Closed, platform.Merged}[r.IntN(4)])
		if r.IntN(5) == 0 {
			o.PR.Base = "master"
		}
		switch r.IntN(4) {
		case 0, 1:
			o.Marker.Key = branchOf(o.PR.Head).CKey
		case 2:
			o.Marker.Key = Key(StreamSync, cands[:1])
		}
		switch r.IntN(5) {
		case 0:
			h := head()
			o.Marker.Data.RecreateFor = &h
		case 1:
			h := branchOf(o.PR.Head).Head
			o.Marker.Data.RecreateFor = &h
		}
		in.Own = append(in.Own, o)
	}
	for i := range r.IntN(3) {
		in.ForeignOpen = append(in.ForeignOpen, ttForeign(int64(60+i), append(names, "feature/x")[r.IntN(4)]))
	}
	for i := range r.IntN(3) {
		if r.IntN(3) > 0 {
			continue
		}
		body := []string{"", "Sync from multi-gitter.", "x\n" + ttBrokenMarker}[r.IntN(3)]
		in.MarkerInvalid = append(in.MarkerInvalid, ttInvalid(int64(70+i), names[r.IntN(len(names))], body))
	}
	in.Ops.AdoptUnmarked = r.IntN(3) == 0
	if r.IntN(5) == 0 {
		in.Memory.ToRevoke = []int64{3}
	}
	if r.IntN(5) == 0 {
		in.Memory.ToAck = []int64{4, 5}[:1+r.IntN(2)]
	}
	if len(in.D) > 0 {
		switch r.IntN(6) {
		case 0:
			in.Memory.Declines = []Decline{{PR: 7, Key: in.Key}}
		case 1:
			in.Memory.Declines = []Decline{{PR: 6, Key: "sha256:other", Changes: ShortChanges(in.D[:1]), Complete: true}}
		}
	}
	switch r.IntN(5) {
	case 0:
		in.Memory.Auto = []AutoClose{{PR: 8, Key: in.Key}}
	case 1:
		in.Memory.Auto = []AutoClose{{PR: 4, Key: "sha256:auto"}}
	}
	in.CooldownDeclined = r.IntN(15) == 0
	switch r.IntN(8) {
	case 0:
		in.CooldownUntil = ttNow.Add(time.Hour)
	case 1:
		in.CooldownUntil = ttNow.Add(-time.Hour)
	}
	for _, h := range heads {
		if r.IntN(6) == 0 {
			in.Ops.RecreateHeads = append(in.Ops.RecreateHeads, h)
		}
	}
	in.RecreateTicked = r.IntN(4) == 0
	in.PlatformWorkflowPerm = r.IntN(2) == 0
	in.CanWorkflows = r.IntN(2) == 0
	in.WorkflowsDiffer = r.IntN(3) == 0
	in.LegacyRewritable = r.IntN(5) == 0
	if r.IntN(5) == 0 {
		names := []string{in.Branch.Name}
		for name := range in.Aliases {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if r.IntN(3) > 0 {
				in.NoForcePush = append(in.NoForcePush, name)
			}
		}
	}
	return in
}

// TestDecideTargetProperties checks the invariants of ttCheck, determinism
// and that the input is left alone, over random inputs, and that the
// generator keeps reaching every rule.
func TestDecideTargetProperties(t *testing.T) {
	runs := 20000
	if testing.Short() {
		runs = 4000
	}
	seen := map[string]int{}
	for seed := range uint64(runs) {
		r := rand.New(rand.NewPCG(seed, 0x7a4))
		in := ttGen(r)
		// Determinism and the input's integrity cost two encodings and a
		// second decision: one seed in eight pays for them.
		deep := seed%8 == 0
		var before []byte
		if deep {
			var err error
			if before, err = json.Marshal(in); err != nil {
				t.Fatal(err)
			}
		}
		d := DecideTarget(in)
		if err := ttCheck(in, d); err != nil {
			js, _ := json.Marshal(in)
			t.Fatalf("seed %d: %v\n%s\ninput: %s", seed, err, ttFormat(d), js)
		}
		if deep {
			if again := DecideTarget(in); !reflect.DeepEqual(again, d) {
				t.Fatalf("seed %d: DecideTarget is not deterministic:\n%s\n%s", seed, ttFormat(d), ttFormat(again))
			}
			if after, _ := json.Marshal(in); string(after) != string(before) {
				t.Fatalf("seed %d: DecideTarget modified its input", seed)
			}
		}
		seen[d.Outcome+":"+d.Reason]++
		for _, s := range d.Steps {
			seen[s.Kind.String()+":"+s.Reason]++
			if s.Kind == StepEditPR && s.Content && s.Base == "" {
				seen["content-refresh"]++
			}
		}
		if d.Blocks.Paused {
			seen["paused"]++
		}
		if d.Blocks.NothingMore {
			seen["nothing-more"]++
		}
		if d.Blocks.UpdateBranchNeeded {
			seen["update-branch-needed"]++
		}
		if len(d.Blocks.PreviouslyDeclined) > 0 {
			seen["previously-declined"]++
		}
		if d.Outcome == OutcomeOpened && len(d.Steps) > 0 && d.Steps[0].Kind == StepCreatePR {
			seen["converged"]++
		}
		if len(in.MarkerInvalid) > 0 && d.Reason != ReasonMarkerInvalid {
			seen["adopted"]++
		}
		open := 0
		for _, o := range in.Own {
			if o.PR.State == platform.Open {
				open++
			}
		}
		closes := 0
		for _, s := range d.Steps {
			if s.Kind == StepClosePR && s.Reason == ReasonDuplicate {
				closes++
			}
		}
		if len(in.MarkerInvalid) == 0 && open > 1 && closes < open-1 {
			seen["duplicate-left-open"]++
		}
	}
	for _, want := range []string{
		"opened:", "updated:content", "updated:rebase", "updated:recreate", "updated:base-renamed", "unchanged:", "closed:no-diff", "declined:",
		"blocked:marker-invalid", "blocked:branch-in-use", "blocked:edited", "blocked:branch-taken", "blocked:permission:workflows",
		"blocked:rules:non-fast-forward",
		"deferred:cooldown",
		"push:", "recreate-branch:", "delete-branch:", "create-pr:", "edit-pr:", "close-pr:no-diff", "close-pr:duplicate",
		"comment:closed", "comment:declined", "comment:auto-declined", "ack:", "revoke:", "consume-recreate:",
		"paused", "nothing-more", "update-branch-needed", "previously-declined", "converged", "duplicate-left-open",
		"content-refresh", "adopted",
	} {
		if seen[want] == 0 {
			t.Errorf("no %s in %d random decisions", want, runs)
		}
	}
}
