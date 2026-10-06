package distribute

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The recheck of phase F against what inspection saw: pull requests
// inspection listed do not move a target, and what the decision rests on (a
// pull request's state, its marker, its ticked controls) moves it when it
// changed.

// atRecheck runs act once, right before the recheck of phase F reads the
// branches of a target of g again (the first git ls-remote in its
// repositories), and returns the function that stops watching.
func (g *gitWorld) atRecheck(act func()) func() {
	var once sync.Once
	dir := filepathSlash(g.src.Dir)
	return gitx.TraceCommands(func(args, env []string) {
		if !slices.Contains(args, "ls-remote") {
			return
		}
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GIT_DIR="); ok && strings.HasPrefix(filepathSlash(v), dir) {
				once.Do(act)
				return
			}
		}
	})
}

// distribute runs distribute over g, and then again: the second run must
// write nothing (I7).
func (g *gitWorld) distribute(edit func(*Deps)) *report.Delivery {
	g.t.Helper()
	d := g.deps(ModeDistribute)
	if edit != nil {
		edit(&d)
	}
	rep := g.run(d, ModeDistribute)
	again := g.run(d, ModeDistribute)
	if writes := g.p.Writes(); len(writes) > 0 {
		g.t.Errorf("distribute right after wrote %q (%s)", writes, outcomes(again))
	}
	return rep
}

// Pull requests inspection listed never move a target at the recheck:
// multi-gitter's merge request a recreate-free adopt_unmarked takes over as
// a duplicate, and someone else's pull request from the alias the duplicate
// lived on. They once counted as opened after inspection, and
// the target ended failed:race on every run.
func TestRunRecheckKnownPRs(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	// Ours on the sync branch, multi-gitter's unmarked one on the alias.
	proto := w.optedIn("acme/migrated", nil)
	w.syncCommit(proto, branch, "", baseFiles...)
	nKept := w.openOwn(proto)
	w.written(proto, nKept)
	w.push(proto, alias, w.known, "AGENTS.md", agentsV1)
	nProto := w.pr(proto, platform.PR{Head: alias, Author: w.known, Title: "chore: sync engineering assets", Body: "Synced by multi-gitter."})
	// Ours on both branches, and a person's from the alias to develop.
	both := w.optedIn("acme/both", nil)
	w.push(both, "develop", w.person, "README.md", "develop\n")
	w.syncCommit(both, branch, "", baseFiles...)
	nBoth := w.openOwn(both)
	w.written(both, nBoth)
	w.syncCommit(both, alias, "", baseFiles...)
	nDup := w.ownOn(both, alias, keyOf(baseFiles...), baseFiles...)
	nTheirs := w.pr(both, platform.PR{Head: alias, Base: "develop", Author: w.person, Title: "our work on the alias"})
	ops := &config.Operations{Version: 1, AdoptUnmarked: &config.UntilOp{Until: "2099-12-31"}}

	rep := w.distribute(func(d *Deps) { d.Write.Operations = ops })
	want(t, rep, "gh:acme/migrated", report.OutcomeUnchanged, "", nKept)
	want(t, rep, "gh:acme/both", report.OutcomeUnchanged, "", nBoth)
	if pr := w.p.PR(proto.ID, nProto); pr.State != platform.Closed {
		t.Errorf("multi-gitter's duplicate #%d is %s", nProto, pr.State)
	}
	if pr := w.p.PR(both.ID, nDup); pr.State != platform.Closed {
		t.Errorf("our duplicate #%d is %s", nDup, pr.State)
	}
	if pr := w.p.PR(both.ID, nTheirs); pr.State != platform.Open || w.p.Branch(both.ID, alias) == "" {
		t.Errorf("someone else's #%d is %s, alias at %q", nTheirs, pr.State, w.p.Branch(both.ID, alias))
	}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeFailed {
			t.Errorf("%s failed:%s: %q", tg.Path, tg.Reason, tg.Warnings)
		}
	}
}

// A recreate withdrawn after inspection is not carried out: the recheck
// compares the controls the decision rests on, the target is inspected
// again and stays paused, and the person's commit stays (I2). The rebuild
// once went on with the tick inspection saw.
func TestRunRecheckRecreateWithdrawn(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", "AGENTS.md", agentsV1, "docs/guide.md", guideV1)
	n := w.ownOn(api, branch, keyOf("AGENTS.md", agentsV1, "docs/guide.md", guideV1), "AGENTS.md", agentsV1, "docs/guide.md", guideV1)
	mine := w.push(api, branch, w.person, "src/mine.txt", "mine\n")
	w.run(w.deps(ModeDistribute), ModeDistribute) // the paused block and its control
	pr := w.p.PR(api.ID, n)
	line := prbody.ControlLine(prbody.ControlRecreate)
	if !strings.Contains(pr.Body, line) {
		t.Fatalf("no recreate control in #%d:\n%s", n, pr.Body)
	}
	ticked := strings.Replace(pr.Body, line, strings.Replace(line, "- [ ]", "- [x]", 1), 1)
	w.p.UpdatePR(api.ID, n, func(pr *platform.PR) { pr.Body = ticked })
	stop := w.atRecheck(func() {
		w.p.UpdatePR(api.ID, n, func(pr *platform.PR) { pr.Body = prbody.Untick(pr.Body, prbody.ControlRecreate) })
	})
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	stop()
	tg := want(t, rep, "gh:acme/api", report.OutcomeBlocked, "edited", n)
	if !hasWarningLike(tg, "the rebuild control of #", "unticked") {
		t.Errorf("warnings %q", tg.Warnings)
	}
	if head := w.p.Branch(api.ID, branch); head != mine {
		t.Errorf("the branch moved to %s, from the person's %s", short(head), short(mine))
	}
	for _, c := range w.p.Writes() {
		if strings.HasPrefix(c, "Push") {
			t.Errorf("pushed after the tick was withdrawn: %q", c)
		}
	}
}

// A pull request whose marker people erased after inspection is not closed:
// the recheck finds the marker gone, and the target inspected again is
// blocked:marker-invalid (the PR and its marker are read again right before
// a close). The close once went on and wrote a marker back.
func TestRunRecheckMarkerErased(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	api := w.optedIn("acme/api", nil, baseFiles...)
	w.syncCommit(api, branch, "", "AGENTS.md", agentsV1)
	n := w.ownOn(api, branch, keyOf("AGENTS.md", agentsV1), "AGENTS.md", agentsV1)
	plan := w.run(w.deps(ModePlan), ModePlan)
	want(t, plan, "gh:acme/api", report.OutcomeClosed, "no-diff", n)
	stop := w.atRecheck(func() {
		w.p.UpdatePR(api.ID, n, func(pr *platform.PR) { pr.Body = "We take this over by hand." })
	})
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	stop()
	tg := want(t, rep, "gh:acme/api", report.OutcomeBlocked, "marker-invalid", n)
	if !hasWarningLike(tg, "no longer carries touchmark's marker") {
		t.Errorf("warnings %q", tg.Warnings)
	}
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("wrote to a pull request people took over: %q", writes)
	}
	if pr := w.p.PR(api.ID, n); pr.State != platform.Open || pr.Body != "We take this over by hand." {
		t.Errorf("#%d: %s, body %q", n, pr.State, pr.Body)
	}
}

// A re-inspection of phase F passes the gate of phase E again: a target
// whose pull request people merged before the recheck would open a new one,
// beyond max_new_prs_per_run. Phase F once opened it anyway.
func TestRunReinspectionRegates(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML += "limits:\n  max_new_prs_per_run: 0\n"
	old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", old...)
	n := w.ownOn(api, branch, keyOf(old...), old...)
	stop := w.atRecheck(func() {
		if _, err := w.p.MergePR(api.ID, n, fake.MergeSquash, w.person, time.Time{}); err != nil {
			t.Errorf("merge #%d: %v", n, err)
		}
	})
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	stop()
	want(t, rep, "gh:acme/api", report.OutcomeDeferred, "rollout-limit", 0)
	for _, c := range w.p.Writes() {
		if strings.HasPrefix(c, "CreatePR") || strings.HasPrefix(c, "Push") {
			t.Errorf("wrote past the rollout limit: %q", c)
		}
	}
}
