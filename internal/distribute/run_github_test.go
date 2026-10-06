package distribute

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// GitHub-specific delivery on the fake: the rules of branches read before
// any write (platform.Preflighter), signatures through the platform's API
// commit and the stage ref (platform.Committer), and the Workflows
// permission as the writer reports it.

// newAPIWorld is a git world whose platform reads rules upfront and makes
// API commits, as GitHub's driver with a GitHub App does.
func newAPIWorld(t *testing.T, opts ...fake.Option) *gitWorld {
	t.Helper()
	return newGitWorld(t, append([]fake.Option{fake.WithPreflight(), fake.WithAPICommits()}, opts...)...)
}

// signedEverywhere requires signed commits on every branch of r.
func (g *gitWorld) signedEverywhere(r platform.Repo) {
	g.t.Helper()
	g.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"~ALL"}, RequiredSignatures: true})
	g.ok()
}

// headCommit returns the raw commit object at the tip of branch in r.
func (g *gitWorld) headCommit(r platform.Repo, branch string) string {
	g.t.Helper()
	head := g.p.Branch(r.ID, branch)
	if head == "" {
		g.t.Fatalf("%s has no branch %s", r.Path, branch)
	}
	out, err := gitx.New(g.p.GitDir(r.ID)).Run(g.t.Context(), nil, "cat-file", "commit", head)
	if err != nil {
		g.t.Fatal(err)
	}
	return string(out)
}

// noHiddenRefs fails the test when a ref outside refs/heads is left in r:
// the stage ref never survives a run.
func (g *gitWorld) noHiddenRefs(r platform.Repo) {
	g.t.Helper()
	if refs := g.p.HiddenRefs(r.ID); len(refs) > 0 {
		g.t.Errorf("%s: refs left behind: %q", r.Path, refs)
	}
}

// opsOf returns the kinds of the ops of the report on ref, in order.
func opsOf(rep *report.Delivery, ref string) []string {
	var out []string
	for _, op := range rep.Ops {
		if op.Target == ref {
			out = append(out, op.Kind)
		}
	}
	return out
}

// writesOn returns the fake's write entries on the repository at path.
func (g *gitWorld) writesOn(path string) []string {
	var out []string
	for _, w := range g.p.Writes() {
		if f := strings.Fields(w); len(f) > 1 && f[1] == path {
			out = append(out, w)
		}
	}
	return out
}

// TestRunRequiredSignaturesAPICommit: a ruleset that requires signed
// commits, read before any write, makes a GitHub App commit through the API:
// the commit touchmark built goes to the stage ref, the platform makes and
// signs its own commit of the same tree and moves the branch to it, and the
// stage ref is gone. The plan and the dry run count its three writes; the
// second run writes nothing. A target without the rule pushes with git.
func TestRunRequiredSignaturesAPICommit(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	x := w.optedIn("acme/x", nil)
	w.signedEverywhere(x)
	y := w.optedIn("acme/y", nil)
	plan := w.both(nil)
	tx := want(t, plan, "gh:acme/x", report.OutcomeOpened, "", 0)
	ty := want(t, plan, "gh:acme/y", report.OutcomeOpened, "", 0)
	if tx.Writes-ty.Writes != writesAPICommit-1 {
		t.Errorf("estimated writes %d with the API commit, %d without: the stage ref and the refs count too", tx.Writes, ty.Writes)
	}

	estimate := tx.Writes
	rep := w.distribute(nil)
	tx = want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
	want(t, rep, "gh:acme/y", report.OutcomeOpened, "", 1)
	if got := opsOf(rep, "gh:acme/x"); len(got) < 4 || !slices.Equal(got[:4], []string{"push", "api-commit", "update-refs", "create-pr"}) {
		t.Errorf("ops of x %v", got)
	}
	// The HTTP writes the throttle metered (the stage ref, the commit, the
	// refs; the pull request, its label and the call that puts it on), as the
	// plan estimated them.
	if tx.Writes != estimate {
		t.Errorf("x: %d writes, %d estimated", tx.Writes, estimate)
	}
	if got := opsOf(rep, "gh:acme/y"); slices.Contains(got, "api-commit") {
		t.Errorf("ops of y %v: no rule asks y for a signature", got)
	}
	c := w.headCommit(x, branch)
	if !strings.Contains(c, "\ngpgsig ") || !strings.Contains(c, "author acme-write[bot] <") || !strings.Contains(c, "committer GitHub <noreply@github.com>") ||
		!strings.Contains(c, "Touchmark-Content: "+tx.Key) {
		t.Errorf("the head of x is not the platform's signed commit of touchmark's content:\n%s", c)
	}
	if pr := w.p.PR(x.ID, 1); pr.HeadSHA != w.p.Branch(x.ID, branch) {
		t.Errorf("#1 of x shows %s, the branch is at %s", pr.HeadSHA, w.p.Branch(x.ID, branch))
	}
	w.noHiddenRefs(x)
	w.noHiddenRefs(y)
}

// TestRunRequiredSignaturesKey: with the provider's signing key the commit
// is signed locally and pushed with git, rule or no rule; without a key and
// without API commits a push the rules require signed is
// blocked:cannot-sign before any write in a dry run and in distribute,
// while a plan, which never holds the key, plans it.
func TestRunRequiredSignaturesKey(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, fake.WithPreflight())
	x := w.optedIn("acme/x", nil)
	w.signedEverywhere(x)

	plan := w.run(w.deps(ModePlan), ModePlan)
	want(t, plan, "gh:acme/x", report.OutcomeOpened, "", 0)
	dry := w.run(w.deps(ModeDryRun), ModeDryRun)
	tg := want(t, dry, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
	if !hasWarningLike(tg, "a rule of the target's branches requires signed commits", "SIGNING_KEY") {
		t.Errorf("warnings %q", tg.Warnings)
	}
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("wrote %q", writes)
	}

	signer, err := sshsig.ParsePrivateKey(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	signed := func(d *Deps) { d.Write.Signers = map[string]*sshsig.Signer{"gh": signer} }
	rep = w.distribute(signed)
	want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
	if got := opsOf(rep, "gh:acme/x"); slices.Contains(got, "api-commit") {
		t.Errorf("ops %v", got)
	}
	if c := w.headCommit(x, branch); !strings.Contains(c, "\ngpgsig -----BEGIN SSH SIGNATURE-----") {
		t.Errorf("the head is not signed with the key:\n%s", c)
	}
}

// TestRunRequiredSignaturesUnknown: rules the identity cannot read
// (Rules.Known false, or no Preflighter at all) change nothing upfront: the
// push meets the rule at run time and the target is blocked:cannot-sign,
// with nothing left behind.
func TestRunRequiredSignaturesUnknown(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string][]fake.Option{"no preflight": nil, "hidden": {fake.WithPreflight(), fake.WithAPICommits()}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newGitWorld(t, opts...)
			x := w.optedIn("acme/x", nil)
			w.signedEverywhere(x)
			w.p.HideRules(x.ID, true)
			w.ok()
			dry := w.both(nil)
			want(t, dry, "gh:acme/x", report.OutcomeOpened, "", 0)
			rep := w.run(w.deps(ModeDistribute), ModeDistribute)
			tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
			if tg.Writes != 0 || w.p.Branch(x.ID, branch) != "" {
				t.Errorf("%d writes, branch at %q", tg.Writes, w.p.Branch(x.ID, branch))
			}
			if !hasWarningLike(tg, "Commits must have verified signatures") {
				t.Errorf("warnings %q", tg.Warnings)
			}
			w.noHiddenRefs(x)
		})
	}
}

// TestRunSignAlwaysAPICommit: sign: always without a key commits through the
// API where the platform makes signed API commits, and the dry run agrees
// with the plan.
func TestRunSignAlwaysAPICommit(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.hubYML = strings.Replace(defaultHubYML, "    writer: acme-write[bot]\n", "    writer: acme-write[bot]\n    sign: always\n", 1)
	x := w.optedIn("acme/x", nil)
	want(t, w.both(nil), "gh:acme/x", report.OutcomeOpened, "", 0)
	rep := w.distribute(nil)
	want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
	if got := opsOf(rep, "gh:acme/x"); !slices.Contains(got, "api-commit") {
		t.Errorf("ops %v", got)
	}
	if c := w.headCommit(x, branch); !strings.Contains(c, "committer GitHub <") {
		t.Errorf("head:\n%s", c)
	}
	w.noHiddenRefs(x)
}

// TestRunAPICommitUnsigned: the first API commit that comes back unsigned
// (GitHub Enterprise Server without web commit signing) blocks its target
// and marks the provider: every later target that needs a signature is
// blocked:cannot-sign before any write. The branch never moves and the stage
// ref is gone.
func TestRunAPICommitUnsigned(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.p.SetAPISigning(false)
	x := w.optedIn("acme/x", nil)
	y := w.optedIn("acme/y", nil)
	w.signedEverywhere(x)
	w.signedEverywhere(y)
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	tx := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
	ty := want(t, rep, "gh:acme/y", report.OutcomeBlocked, "cannot-sign", 0)
	if !hasWarningLike(ty, "API commits of provider gh are not signed") {
		t.Errorf("y's warnings %q", ty.Warnings)
	}
	if got := w.writesOn("acme/y"); len(got) > 0 {
		t.Errorf("y: wrote %q after the provider's API commit came back unsigned", got)
	}
	if got := opsOf(rep, "gh:acme/x"); !slices.Equal(got, []string{"push", "api-commit"}) || tx.Writes != 2 {
		t.Errorf("x: ops %v, %d writes", got, tx.Writes)
	}
	for _, r := range []platform.Repo{x, y} {
		if head := w.p.Branch(r.ID, branch); head != "" {
			t.Errorf("%s: the branch moved to %s", r.Path, head)
		}
		w.noHiddenRefs(r)
	}
}

// racyWriter is the writer of a world whose API commits meet a branch that
// someone moved right before them (before), or right after them, before
// touchmark reads the answer (after). Nil hooks do nothing.
type racyWriter struct {
	platform.Writer
	before, after func()
}

func (w racyWriter) Preflight(ctx context.Context, r platform.Repo, branches []string) (platform.Rules, error) {
	return w.Writer.(platform.Preflighter).Preflight(ctx, r, branches)
}

func (w racyWriter) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	tw, err := w.Writer.Target(ctx, r, need)
	if err != nil {
		return nil, err
	}
	return racyTarget{TargetWriter: tw, before: w.before, after: w.after}, nil
}

type racyTarget struct {
	platform.TargetWriter
	before, after func()
}

func (t racyTarget) Commit(ctx context.Context, r platform.Repo, req platform.CommitRequest) (platform.Commit, error) {
	if t.before != nil {
		t.before()
	}
	c, err := t.TargetWriter.(platform.Committer).Commit(ctx, r, req)
	if t.after != nil {
		t.after()
	}
	return c, err
}

// lyingWriter is the writer of a world whose per-target writers answer
// Commit otherwise than the platform did (mutate), or are no Committers at
// all (hide).
type lyingWriter struct {
	platform.Writer
	mutate func(platform.Commit) platform.Commit
	hide   bool
}

func (w lyingWriter) Preflight(ctx context.Context, r platform.Repo, branches []string) (platform.Rules, error) {
	return w.Writer.(platform.Preflighter).Preflight(ctx, r, branches)
}

func (w lyingWriter) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	tw, err := w.Writer.Target(ctx, r, need)
	if err != nil || w.hide {
		return plainTarget{TargetWriter: tw}, err
	}
	return lyingTarget{TargetWriter: tw, mutate: w.mutate}, nil
}

// plainTarget hides the Committer of the target writer it wraps.
type plainTarget struct{ platform.TargetWriter }

type lyingTarget struct {
	platform.TargetWriter
	mutate func(platform.Commit) platform.Commit
}

func (t lyingTarget) Commit(ctx context.Context, r platform.Repo, req platform.CommitRequest) (platform.Commit, error) {
	c, err := t.TargetWriter.(platform.Committer).Commit(ctx, r, req)
	if err != nil {
		return c, err
	}
	return t.mutate(c), nil
}

// TestRunAPICommitAnswers: the core never trusts an API commit it cannot
// vouch for. A platform that moves the branch to a commit it reports
// unsigned blocks the target (blocked:cannot-sign) and every later
// target's signed push of the provider; one that reports another tree
// than touchmark built fails it (failed:integrity, I6); a per-target
// writer that makes no API commits where one is needed blocks it before
// any write.
func TestRunAPICommitAnswers(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, lw lyingWriter) (*gitWorld, *report.Delivery) {
		t.Helper()
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		w.signedEverywhere(x)
		y := w.optedIn("acme/y", nil)
		w.signedEverywhere(y)
		d := w.deps(ModeDistribute)
		lw.Writer = d.Providers[0].Writer
		d.Providers[0].Writer = lw
		return w, w.run(d, ModeDistribute)
	}
	t.Run("unsigned", func(t *testing.T) {
		t.Parallel()
		w, rep := run(t, lyingWriter{mutate: func(c platform.Commit) platform.Commit { c.Verified = false; return c }})
		tx := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
		if !hasWarningLike(tx, "are not signed") || !slices.Contains(opsOf(rep, "gh:acme/x"), "api-commit") {
			t.Errorf("x: ops %v, warnings %q", opsOf(rep, "gh:acme/x"), tx.Warnings)
		}
		want(t, rep, "gh:acme/y", report.OutcomeBlocked, "cannot-sign", 0)
		if got := w.writesOn("acme/y"); len(got) > 0 {
			t.Errorf("y: wrote %q after the provider's API commit came back unsigned", got)
		}
	})
	t.Run("another tree", func(t *testing.T) {
		t.Parallel()
		_, rep := run(t, lyingWriter{mutate: func(c platform.Commit) platform.Commit { c.Tree = strings.Repeat("e", 40); return c }})
		tx := want(t, rep, "gh:acme/x", report.OutcomeFailed, "integrity", 0)
		if !hasWarningLike(tx, "not of the tree") {
			t.Errorf("warnings %q", tx.Warnings)
		}
	})
	t.Run("no Committer", func(t *testing.T) {
		t.Parallel()
		w, rep := run(t, lyingWriter{hide: true})
		want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
		if got := w.writesOn("acme/x"); len(got) > 0 {
			t.Errorf("wrote %q", got)
		}
	})
}

// TestRunAPICommitStaleLease: an API commit whose branch moved since
// touchmark read it fails its compare-and-swap (ClassConflict): the stage
// ref is deleted and the target decided again, like a push whose lease
// failed — here a person's commit on the branch pauses it. A conflict the
// platform reports without a move is decided again too, and the second API
// commit goes through.
func TestRunAPICommitStaleLease(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*gitWorld, platform.Repo, int64) {
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
		w.syncCommit(x, branch, "", old...)
		n := w.ownOn(x, branch, keyOf(old...), old...)
		w.written(x, n)
		w.signedEverywhere(x)
		return w, x, n
	}
	t.Run("moved", func(t *testing.T) {
		t.Parallel()
		w, x, n := setup(t)
		var once sync.Once
		d := w.deps(ModeDistribute)
		d.Providers[0].Writer = racyWriter{Writer: d.Providers[0].Writer, before: func() {
			once.Do(func() { w.push(x, branch, w.person, "notes.md", "the team's notes\n") })
		}}
		rep := w.run(d, ModeDistribute)
		tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "edited", n)
		if got := opsOf(rep, "gh:acme/x"); len(got) < 2 || !slices.Equal(got[:2], []string{"push", "delete-ref"}) || slices.Contains(got, "api-commit") {
			t.Errorf("ops %v (warnings %q)", got, tg.Warnings)
		}
		w.noHiddenRefs(x)
	})
	t.Run("conflict", func(t *testing.T) {
		t.Parallel()
		w, x, n := setup(t)
		w.p.FailNext("Commit", &platform.Error{Op: "commit", Class: platform.ClassConflict, Err: errors.New("stale data")})
		rep := w.distribute(nil)
		tg := want(t, rep, "gh:acme/x", report.OutcomeUpdated, "content", n)
		if got := opsOf(rep, "gh:acme/x"); !slices.Equal(got, []string{"push", "delete-ref", "push", "api-commit", "update-refs", "edit-pr"}) {
			t.Errorf("ops %v", got)
		}
		if !hasWarningLike(tg, "inspected again") {
			t.Errorf("warnings %q", tg.Warnings)
		}
		w.noHiddenRefs(x)
	})
}

// TestRunAPICommitStage: the stage ref never survives a run. One a crashed
// run left behind is replaced with a lease on what it holds; an API commit
// the platform refuses leaves the branch alone, and touchmark deletes the
// stage ref itself; one whose answer was lost is read back and counts as
// made.
func TestRunAPICommitStage(t *testing.T) {
	t.Parallel()
	t.Run("left behind", func(t *testing.T) {
		t.Parallel()
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		w.signedEverywhere(x)
		stage := gitx.HiddenRefPrefix + marker.FP16(hubFP) + "/stage"
		if _, err := gitx.New(w.p.GitDir(x.ID)).Run(t.Context(), nil, "update-ref", stage, w.p.Head(x.ID)); err != nil {
			t.Fatal(err)
		}
		rep := w.distribute(nil)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
		w.noHiddenRefs(x)
	})
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		w.signedEverywhere(x)
		w.p.FailNext("Commit", &platform.Error{Op: "commit", Class: platform.ClassInvalid, Status: http.StatusUnprocessableEntity, Err: errors.New("tree not found")})
		rep := w.run(w.deps(ModeDistribute), ModeDistribute)
		want(t, rep, "gh:acme/x", report.OutcomeFailed, "internal", 0)
		if got := opsOf(rep, "gh:acme/x"); !slices.Equal(got, []string{"push", "delete-ref"}) {
			t.Errorf("ops %v", got)
		}
		if head := w.p.Branch(x.ID, branch); head != "" {
			t.Errorf("the branch moved to %s", head)
		}
		w.noHiddenRefs(x)
	})
	t.Run("answer lost", func(t *testing.T) {
		t.Parallel()
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		w.signedEverywhere(x)
		w.p.FailNextApplied("Commit", &platform.Error{Op: "commit", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")})
		rep := w.distribute(nil)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
		if got := opsOf(rep, "gh:acme/x"); len(got) < 3 || !slices.Equal(got[:3], []string{"push", "api-commit", "update-refs"}) {
			t.Errorf("ops %v", got)
		}
		if c := w.headCommit(x, branch); !strings.Contains(c, "\ngpgsig ") {
			t.Errorf("head:\n%s", c)
		}
		w.noHiddenRefs(x)
	})
	// A 502 before the platform applied the commit: the branch is still at
	// the lease, so the commit is tried again, and made once.
	t.Run("failed before", func(t *testing.T) {
		t.Parallel()
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		w.signedEverywhere(x)
		w.p.FailNext("Commit", &platform.Error{Op: "commit", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")})
		rep := w.run(w.deps(ModeDistribute), ModeDistribute)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
		ops := opsOf(rep, "gh:acme/x")
		if n := slices.Index(ops, "api-commit"); n < 0 || slices.Contains(ops[n+1:], "api-commit") {
			t.Errorf("ops %v: want one API commit", ops)
		}
		if calls := slices.DeleteFunc(w.p.Calls(), func(c string) bool { return !strings.HasPrefix(c, "Commit ") }); len(calls) != 2 {
			t.Errorf("Commit calls %q, want the failed one and its retry", calls)
		}
		w.noHiddenRefs(x)
		if again := w.distribute(nil); len(opsOf(again, "gh:acme/x")) != 0 {
			t.Errorf("the next run wrote %v", opsOf(again, "gh:acme/x"))
		}
	})
	// The answer is lost while a person pushes to the sync branch: the read
	// back finds a head that is neither the lease nor touchmark's commit.
	// The target is decided again as after a failed lease (here: the
	// person's commit pauses it), not failed as transient.
	t.Run("answer lost, branch moved", func(t *testing.T) {
		t.Parallel()
		w := newAPIWorld(t)
		x := w.optedIn("acme/x", nil)
		old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
		w.syncCommit(x, branch, "", old...)
		n := w.ownOn(x, branch, keyOf(old...), old...)
		w.written(x, n)
		w.signedEverywhere(x)
		w.p.FailNextApplied("Commit", &platform.Error{Op: "commit", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")})
		var once sync.Once
		d := w.deps(ModeDistribute)
		d.Providers[0].Writer = racyWriter{Writer: d.Providers[0].Writer, after: func() {
			once.Do(func() { w.push(x, branch, w.person, "notes.md", "the team's notes\n") })
		}}
		rep := w.run(d, ModeDistribute)
		tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "edited", n)
		if !hasWarningLike(tg, "inspected again") {
			t.Errorf("warnings %q", tg.Warnings)
		}
		if calls := slices.DeleteFunc(w.p.Calls(), func(c string) bool { return !strings.HasPrefix(c, "Commit ") }); len(calls) != 1 {
			t.Errorf("Commit calls %q: an unknown outcome over a moved branch is never tried again", calls)
		}
		w.noHiddenRefs(x)
	})
}

// TestRunWorkflowsPreflight: the writer's Workflows permission, as its
// Preflight reports it, replaces the run's assumption: a refusal it knows is
// decided upfront, so a branch moved over someone else's workflow change
// without our open pull request is created afresh from B; unknown, the
// assumption stands and the per-target token meets the refusal. Granted, the
// push carries the permission.
func TestRunWorkflowsPreflight(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, workflows bool) (*gitWorld, platform.Repo) {
		w := newGitWorld(t, fake.WithPreflight())
		x := w.optedIn("acme/x", nil)
		w.syncCommit(x, branch, "", baseFiles...)
		w.push(x, "main", w.person, ".github/workflows/theirs.yml", "on: push\n")
		w.p.Grant(x.ID, w.writer, platform.Perms{Contents: true, PRs: true, Workflows: workflows})
		w.ok()
		return w, x
	}
	assume := func(d *Deps) { d.Write.CanWorkflows = map[string]bool{"gh": true} }
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		w, x := setup(t, false)
		d := w.deps(ModeDryRun)
		assume(&d)
		wk, res := w.inspectOne(d, ModeDryRun, "acme/x")
		if wk == nil || !wk.rules.WorkflowsKnown || wk.rules.Workflows || res.Outcome != report.OutcomeOpened ||
			len(wk.Decision.Steps) == 0 || wk.Decision.Steps[0].Kind.String() != "recreate-branch" {
			t.Fatalf("%s:%s, work %+v", res.Outcome, res.Reason, wk)
		}
		rep := w.distribute(assume)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
		if got := opsOf(rep, "gh:acme/x"); len(got) < 3 || !slices.Equal(got[:3], []string{"delete-branch", "push", "create-pr"}) {
			t.Errorf("ops %v", got)
		}
		if pr := w.p.PR(x.ID, 1); pr.HeadSHA != w.p.Branch(x.ID, branch) {
			t.Errorf("#1 shows %s", pr.HeadSHA)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		w, _ := setup(t, false)
		w.p.HideWorkflows(true)
		d := w.deps(ModeDryRun)
		assume(&d)
		rep := w.run(d, ModeDryRun)
		want(t, rep, "gh:acme/x", report.OutcomeBlocked, "permission:workflows", 0)
		if !slices.ContainsFunc(w.p.Calls(), func(c string) bool { return c == "Target acme/x" }) {
			t.Errorf("the dry run's preflight asked no token: %q", w.p.Calls())
		}
	})
	// The recovery the Update branch block promises: a rebuild of our open
	// pull request across the person's workflow is blocked without Workflows,
	// and the body asks for Update branch; once the person merges the base
	// into the sync branch, the next run pushes on top of it without Workflows
	// (the branch already carries the workflow, so the push changes none) and
	// updates the pull request.
	t.Run("update branch", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t, fake.WithPreflight())
		x := w.optedIn("acme/x", nil)
		old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
		w.syncCommit(x, branch, "", old...)
		n := w.ownOn(x, branch, keyOf(old...), old...)
		w.written(x, n)
		w.push(x, "main", w.person, ".github/workflows/theirs.yml", "on: push\n")
		w.p.Grant(x.ID, w.writer, platform.Perms{Contents: true, PRs: true})
		w.ok()
		rep := w.distribute(nil)
		want(t, rep, "gh:acme/x", report.OutcomeBlocked, "permission:workflows", n)
		if body := w.p.PR(x.ID, n).Body; !strings.Contains(body, "Update branch") {
			t.Errorf("the body does not ask for Update branch:\n%s", body)
		}
		if _, err := w.p.UpdateBranch(x.ID, n, w.person, time.Time{}); err != nil {
			t.Fatal(err)
		}
		rep = w.distribute(nil)
		want(t, rep, "gh:acme/x", report.OutcomeUpdated, "content", n)
		if pr := w.p.PR(x.ID, n); pr.State != platform.Open || pr.HeadSHA != w.p.Branch(x.ID, branch) {
			t.Errorf("#%d after the update: %s at %s, the branch at %s", n, pr.State, pr.HeadSHA, w.p.Branch(x.ID, branch))
		}
		if v := w.p.Violations(); len(v) > 0 {
			t.Errorf("violations %q", v)
		}
	})
	t.Run("granted", func(t *testing.T) {
		t.Parallel()
		w, x := setup(t, true)
		d := w.deps(ModeDryRun)
		wk, res := w.inspectOne(d, ModeDryRun, "acme/x")
		if wk == nil || res.Outcome != report.OutcomeOpened || !wk.NeedPerms.Workflows || wk.Decision.Steps[0].Kind.String() != "push" {
			t.Fatalf("%s:%s, work %+v", res.Outcome, res.Reason, wk)
		}
		rep := w.distribute(nil)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
		if pr := w.p.PR(x.ID, 1); pr.HeadSHA != w.p.Branch(x.ID, branch) {
			t.Errorf("#1 shows %s", pr.HeadSHA)
		}
	})
}

// TestRunNoForcePush: a rule against force pushes on the sync branch: the
// rebuild under our open pull request is blocked:rules:non-fast-forward in
// the plan, the dry run and distribute, with nothing written; without our
// open pull request the branch is deleted and created afresh, which forces
// nothing. Rules the identity cannot read meet the refusal at run time, with
// the same reason.
func TestRunNoForcePush(t *testing.T) {
	t.Parallel()
	noForce := fake.Ruleset{Branches: []string{"touchmark/*"}, NonFastForward: true}
	t.Run("open pull request", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t, fake.WithPreflight())
		x := w.optedIn("acme/x", nil)
		old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
		w.syncCommit(x, branch, "", old...)
		n := w.ownOn(x, branch, keyOf(old...), old...)
		w.written(x, n)
		w.p.AddRuleset(x.ID, noForce)
		w.ok()
		want(t, w.both(nil), "gh:acme/x", report.OutcomeBlocked, "rules:non-fast-forward", n)
		rep := w.run(w.deps(ModeDistribute), ModeDistribute)
		want(t, rep, "gh:acme/x", report.OutcomeBlocked, "rules:non-fast-forward", n)
		if writes := w.p.Writes(); len(writes) > 0 {
			t.Errorf("wrote %q", writes)
		}
	})
	t.Run("no pull request", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t, fake.WithPreflight())
		x := w.optedIn("acme/x", nil)
		w.syncCommit(x, branch, "", baseFiles...)
		w.push(x, "main", w.person, "README.md", "moved on\n")
		w.p.AddRuleset(x.ID, noForce)
		w.ok()
		want(t, w.both(nil), "gh:acme/x", report.OutcomeOpened, "", 0)
		rep := w.distribute(nil)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 1)
		if got := opsOf(rep, "gh:acme/x"); len(got) < 3 || !slices.Equal(got[:3], []string{"delete-branch", "push", "create-pr"}) {
			t.Errorf("ops %v", got)
		}
	})
	// GitHub's ruleset form selects "Restrict deletions" with "Block force
	// pushes": the fresh branch is refused too, and the plan, the dry run
	// and distribute say so before any write. Without the preflight, the
	// force push meets the rule at run time, with nothing written.
	t.Run("no pull request, no deletions", func(t *testing.T) {
		t.Parallel()
		for name, opts := range map[string][]fake.Option{"preflight": {fake.WithPreflight()}, "at run time": nil} {
			w := newGitWorld(t, opts...)
			x := w.optedIn("acme/x", nil)
			w.syncCommit(x, branch, "", baseFiles...)
			w.push(x, "main", w.person, "README.md", "moved on\n")
			w.p.AddRuleset(x.ID, fake.Ruleset{Branches: []string{"touchmark/*"}, NonFastForward: true, Deletion: true})
			w.ok()
			head := w.p.Branch(x.ID, branch)
			if opts != nil {
				want(t, w.both(nil), "gh:acme/x", report.OutcomeBlocked, "rules:non-fast-forward", 0)
			}
			rep := w.run(w.deps(ModeDistribute), ModeDistribute)
			tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "rules:non-fast-forward", 0)
			if tg.Writes != 0 || w.p.Branch(x.ID, branch) != head {
				t.Errorf("%s: %d writes, the branch at %s (was %s)", name, tg.Writes, w.p.Branch(x.ID, branch), head)
			}
			if opts == nil && !hasWarningLike(tg, "Cannot force-push to this branch") {
				t.Errorf("%s: warnings %q", name, tg.Warnings)
			}
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t)
		x := w.optedIn("acme/x", nil)
		old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
		w.syncCommit(x, branch, "", old...)
		n := w.ownOn(x, branch, keyOf(old...), old...)
		w.written(x, n)
		w.p.AddRuleset(x.ID, noForce)
		w.ok()
		want(t, w.both(nil), "gh:acme/x", report.OutcomeUpdated, "content", n)
		rep := w.run(w.deps(ModeDistribute), ModeDistribute)
		tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "rules:non-fast-forward", n)
		if tg.Writes != 0 || !hasWarningLike(tg, "Cannot force-push to this branch") {
			t.Errorf("%d writes, warnings %q", tg.Writes, tg.Warnings)
		}
	})
}
