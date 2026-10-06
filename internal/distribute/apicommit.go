package distribute

import (
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
)

// stageCleanup bounds the deletion of a stage ref once the run's context
// is done: the stage ref never outlives a run.
const stageCleanup = 30 * time.Second

// stageRef is the hidden ref an API commit stages its objects on:
// refs/touchmark/<fp16>/stage, fp16 of the hub's fingerprint, so that two
// hubs never share one.
func (r *run) stageRef() string {
	return gitx.HiddenRefPrefix + marker.FP16(r.fps[0]) + "/stage"
}

// doCommit makes the commit of a push through the platform's API, for a
// signature (Work.viaAPI), and moves the branch to it:
//
//  1. the commit touchmark built is pushed to the stage ref with a lease
//     on nothing (a stage ref a crashed run left behind is replaced, with
//     a lease on what it holds), so that the platform holds every object of
//     its tree and no workflow runs;
//  2. platform.Committer.Commit with the tree, the parent B, the message,
//     the stage ref, and the lease of the push as Expect: the platform
//     makes and signs its own commit of the same tree and moves the branch
//     to it with compare-and-swap, deleting the stage ref;
//  3. the answer: a signed commit of the tree is the new head, which the
//     pull request and the verification then use; an unsigned one
//     (platform.ErrUnsigned, or a branch moved to a commit the platform
//     did not sign) marks the provider's API as not signing for the rest of
//     the run and blocks the target (blocked:cannot-sign); a stale lease
//     (ClassConflict) re-inspects the target as a failed push lease does;
//     another error ends the target by its class. A transient failure is
//     reconciled first (the branch read back: a commit of the same tree on
//     B with the same message is the commit made) and tried again as a push
//     is;
//  4. the stage ref is deleted with a lease on what it holds, whatever
//     happened, unless the platform deleted it already: it never survives
//     the target.
//
// The journal records the push to the stage ref, the API commit and the
// update of the refs (three writes), and a deletion of the stage ref the
// core makes itself.
func (x *targetExec) doCommit(ctx context.Context, a writeAct) (string, bool) {
	prov := x.t.prov
	if why := prov.unsignedAPI(); why != "" {
		x.end(report.OutcomeBlocked, "cannot-sign", "the push must be signed, and "+why)
		x.progress()
		return "", false
	}
	committer, ok := x.tw.(platform.Committer)
	if !ok {
		x.end(report.OutcomeBlocked, "cannot-sign", noAPICommit(prov))
		x.progress()
		return "", false
	}
	stage := x.r.stageRef()
	// Deferred first: a push to the stage ref whose answer was lost may
	// have created it.
	defer x.dropStage(ctx, stage, a.commit)
	if !x.stage(ctx, stage, a) {
		return "", false
	}
	c, err := x.w.Repo.Commit(ctx, a.commit)
	if err != nil {
		x.fail("read the commit to make through the API", gitFailure(err))
		return "", false
	}
	req := platform.CommitRequest{
		Branch:  a.branch,
		Expect:  a.expect,
		Parent:  x.w.B,
		Tree:    x.w.Built.Tree,
		Changes: commitChanges(x.w.D),
		Blob:    x.r.hubBlob,
		Message: c.Message,
		Stage:   stage,
	}
	what := "commit to " + a.branch + " through the API"
	var got platform.Commit
	err = x.retryWrite(ctx, func() error {
		var err error
		got, err = committer.Commit(ctx, x.t.repo, req)
		return err
	}, func() (bool, error) {
		sha, applied, err := x.commitApplied(ctx, a, c.Message)
		if applied {
			// The platform moves a branch only to a commit it signed.
			got = platform.Commit{SHA: sha, Tree: x.w.Built.Tree, Verified: true, CAS: true}
		}
		return applied, err
	})
	switch {
	case err == nil && !got.Verified:
		// The platform moved the branch to a commit it did not sign: the
		// contract says it never does, so the provider's API is not trusted
		// to sign for the rest of the run.
		x.commitOps(a, got.SHA)
		why := fmt.Sprintf("the API commits of provider %s are not signed: %s holds the unsigned %s", prov.cfg.ID, a.branch, short(got.SHA))
		prov.markUnsignedAPI(why)
		x.end(report.OutcomeBlocked, "cannot-sign", why)
		x.progress()
		return "", false
	case err == nil && got.Tree != "" && !strings.EqualFold(got.Tree, x.w.Built.Tree):
		x.commitOps(a, got.SHA)
		x.end(report.OutcomeFailed, "integrity", fmt.Sprintf("%s: the platform made commit %s of tree %s, not of the tree %s touchmark built",
			what, short(got.SHA), short(got.Tree), short(x.w.Built.Tree)))
		x.progress()
		return "", false
	case err == nil:
		sha := strings.ToLower(got.SHA)
		x.commitOps(a, sha)
		x.pushed, x.pushedTo = sha, a.branch
		return "", true
	case errors.Is(err, platform.ErrUnsigned) || (platform.ClassOf(err) == platform.ClassUnsupported && ruleOf(err) == "cannot-sign"):
		if got.SHA != "" {
			x.op("api-commit", 0, "", strings.ToLower(got.SHA), "made the unsigned commit "+short(got.SHA)+" through the API")
		}
		why := fmt.Sprintf("the API commits of provider %s are not signed (the platform may not sign them: GitHub Enterprise Server signs only with web commit signing on)", prov.cfg.ID)
		prov.markUnsignedAPI(why)
		x.end(report.OutcomeBlocked, "cannot-sign", what+": "+err.Error())
		x.progress()
		return "", false
	case platform.ClassOf(err) == platform.ClassConflict:
		return fmt.Sprintf("%s moved: the lease on %s failed (%v)", a.name(), labelHead(a.expect), err), true
	}
	x.fail(what, err)
	return "", false
}

// stage pushes the commit of a to the stage ref (doCommit's step 1). A
// stage ref already there (left by a run that stopped) is replaced once,
// with a lease on what it holds. ok is false when the target ended.
func (x *targetExec) stage(ctx context.Context, stage string, a writeAct) bool {
	st := writeAct{kind: actPush, branch: a.branch, ref: stage, commit: a.commit,
		desc: "staged " + short(a.commit) + " for " + a.branch + " on " + stage}
	for attempt := range 2 {
		moved, ok := x.doPush(ctx, st)
		if !ok || moved == "" {
			return ok
		}
		if attempt > 0 {
			break
		}
		var refs map[string]string
		err := x.retryRead(ctx, func() error {
			var err error
			refs, err = x.push.RemoteRefs(ctx, stage)
			return gitFailure(err)
		})
		if err != nil {
			x.fail("read the stage ref "+stage, err)
			return false
		}
		st.expect = refs[stage]
	}
	x.end(report.OutcomeFailed, "race", "the stage ref "+stage+" moved while touchmark staged its commit; the next run tries again")
	x.progress()
	return false
}

// dropStage deletes the stage ref when it is still there (doCommit's step
// 4), with a lease on what it holds; a failure is a warning. It runs on a
// context of its own once ctx is done, for at most stageCleanup: the stage
// ref must not outlive the run.
func (x *targetExec) dropStage(ctx context.Context, stage, commit string) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), stageCleanup)
		defer cancel()
	}
	var refs map[string]string
	err := x.retryRead(ctx, func() error {
		var err error
		refs, err = x.push.RemoteRefs(ctx, stage)
		return gitFailure(err)
	})
	if err != nil {
		x.warn(fmt.Sprintf("the stage ref %s may be left behind: %v", stage, err))
		return
	}
	head := refs[stage]
	if head == "" {
		return
	}
	del := writeAct{kind: actPush, branch: "", ref: stage, expect: head, soft: true, desc: "deleted " + stage}
	if head != commit {
		del.desc = "deleted " + stage + " (it held " + short(head) + ")"
	}
	if _, ok := x.doPush(ctx, del); !ok {
		x.warn("the stage ref " + stage + " is left behind; the next API commit of this hub replaces it")
	}
}

// commitApplied reads the branch back after an API commit whose outcome is
// unknown: applied when it now holds a commit of the tree touchmark built
// on B with message msg (sha is it), not applied when it is still at the
// lease; moved otherwise, which is an error of class conflict.
func (x *targetExec) commitApplied(ctx context.Context, a writeAct, msg string) (sha string, applied bool, err error) {
	var refs map[string]string
	err = x.retryRead(ctx, func() error {
		var err error
		refs, err = x.push.RemoteRefs(ctx, a.name())
		return gitFailure(err)
	})
	if err != nil {
		return "", false, err
	}
	head := refs[a.name()]
	if head == a.expect {
		return "", false, nil
	}
	moved := &platform.Error{Op: "commit", Class: platform.ClassConflict,
		Err: fmt.Errorf("%s moved to %s while touchmark committed through the API", a.name(), labelHead(head))}
	if head == "" {
		return "", false, moved
	}
	err = x.retryRead(ctx, func() error {
		_, _, err := x.push.FetchBranch(ctx, a.branch, 1)
		return gitFailure(err)
	})
	if err != nil {
		return "", false, err
	}
	c, err := x.w.Repo.Commit(ctx, head)
	if err != nil {
		return "", false, gitFailure(err)
	}
	diff, err := x.w.Repo.DiffTree(ctx, x.w.Built.Commit, head)
	if err != nil {
		return "", false, gitFailure(err)
	}
	if len(diff) > 0 || !slices.Equal(c.Parents, []string{x.w.B}) || c.Message != msg {
		return "", false, moved
	}
	return head, true, nil
}

// commitOps journals the writes of an API commit that moved a's branch to
// sha: the commit and the update of the refs.
func (x *targetExec) commitOps(a writeAct, sha string) {
	x.op("api-commit", 0, "", sha, "made "+short(sha)+" for "+a.branch+" through the API")
	x.op("update-refs", 0, a.expect, sha, "moved "+a.branch+" to "+short(sha))
}

// commitChanges are the paths of D as an API commit's changes: a deletion
// has no mode.
func commitChanges(d []decide.Pair) []platform.Change {
	out := make([]platform.Change, 0, len(d))
	for _, p := range d {
		c := platform.Change{Path: p.Path, Mode: p.Mode, OID: p.To}
		if strings.Trim(p.Mode, "0") == "" {
			c.Mode, c.OID = "", ""
		}
		out = append(out, c)
	}
	return out
}
