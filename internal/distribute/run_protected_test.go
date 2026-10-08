package distribute

import (
	"slices"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// protect makes branch patterns of r protected branches the writer may not
// push to.
func (g *gitWorld) protect(r string, patterns ...string) {
	g.t.Helper()
	g.p.AddRuleset(r, fake.Ruleset{Branches: patterns, NoPush: true})
	g.ok()
}

// TestRunProtectedBranch: a protected branch the writer may not push to
// (GitLab's, Gitea's), which the writer as a platform.PushGuard reports,
// blocks the target rules:protected-branch before any write, in a dry run
// and in distribute, with a warning naming the rule; a plan, whose reader
// is no PushGuard, plans the pull request. An open pull request stays as
// it is. Without the guard, or with the rules hidden, the push meets the
// protection and is refused with the same reason, nothing written.
func TestRunProtectedBranch(t *testing.T) {
	t.Parallel()
	t.Run("guard", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t, fake.WithPushGuard())
		x := w.optedIn("acme/x", nil)
		w.protect(x.ID, "touchmark/*")
		w.optedIn("acme/y", nil)

		plan := w.run(w.deps(ModePlan), ModePlan)
		want(t, plan, "gh:acme/x", report.OutcomeOpened, "", 0)
		for _, mode := range []Mode{ModeDryRun, ModeDistribute} {
			rep := w.run(w.deps(mode), mode)
			tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "rules:protected-branch", 0)
			if !hasWarningLike(tg, "branch "+branch+" is protected (touchmark/*)") {
				t.Errorf("%v: warnings %q", mode, tg.Warnings)
			}
			if mode == ModeDistribute {
				want(t, rep, "gh:acme/y", report.OutcomeOpened, "", 1)
			}
		}
		if writes := w.writesOn("acme/x"); len(writes) > 0 || w.p.Branch(x.ID, branch) != "" {
			t.Errorf("wrote to acme/x: %q, branch at %q", writes, w.p.Branch(x.ID, branch))
		}
	})

	t.Run("open pull request", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t, fake.WithPushGuard())
		x := w.optedIn("acme/x", nil)
		want(t, w.distribute(nil), "gh:acme/x", report.OutcomeOpened, "", 1)
		head := w.p.Branch(x.ID, branch)
		w.protect(x.ID, branch)
		w.pack(slices.Concat(baseFiles, []string{"docs/new.md", version("docs/new.md", 1)})...)

		dry := w.run(w.deps(ModeDryRun), ModeDryRun)
		want(t, dry, "gh:acme/x", report.OutcomeBlocked, "rules:protected-branch", 1)
		rep := w.run(w.deps(ModeDistribute), ModeDistribute)
		tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "rules:protected-branch", 1)
		if !hasWarningLike(tg, "branch "+branch+" is protected ("+branch+")") {
			t.Errorf("warnings %q", tg.Warnings)
		}
		if writes := w.writesOn("acme/x"); len(writes) > 0 || w.p.Branch(x.ID, branch) != head {
			t.Errorf("wrote to acme/x: %q, branch moved from %s to %s", writes, head, w.p.Branch(x.ID, branch))
		}
	})

	for name, opts := range map[string][]fake.Option{"no guard": nil, "hidden": {fake.WithPushGuard()}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newGitWorld(t, opts...)
			x := w.optedIn("acme/x", nil)
			w.protect(x.ID, "touchmark/*")
			w.p.HideRules(x.ID, true)
			w.ok()
			want(t, w.both(nil), "gh:acme/x", report.OutcomeOpened, "", 0)
			rep := w.run(w.deps(ModeDistribute), ModeDistribute)
			tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "rules:protected-branch", 0)
			if tg.Writes != 0 || w.p.Branch(x.ID, branch) != "" {
				t.Errorf("%d writes, branch at %q", tg.Writes, w.p.Branch(x.ID, branch))
			}
			if !hasWarningLike(tg, "not allowed to push code to protected branches") {
				t.Errorf("warnings %q", tg.Warnings)
			}
		})
	}
}
