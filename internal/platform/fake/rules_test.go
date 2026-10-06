package fake_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// TestPreflight: WithPreflight makes the reader and the writer
// Preflighters that report the rulesets of the branches asked, a branch
// that does not exist included, and for the writer its Workflows
// permission; HideRules and HideWorkflows make them unknown.
func TestPreflight(t *testing.T) {
	plain := newEnv(t)
	if _, ok := plain.p.Reader(plain.reader).(platform.Preflighter); ok {
		t.Error("the reader of a platform without WithPreflight is a Preflighter")
	}
	if _, ok := plain.p.Writer(plain.writer).(platform.Preflighter); ok {
		t.Error("the writer of a platform without WithPreflight is a Preflighter")
	}

	e := newEnv(t, fake.WithPreflight())
	r := e.repo("acme/x", "README.md", "x\n")
	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"~DEFAULT_BRANCH"}, RequiredSignatures: true})
	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"touchmark/*"}, NonFastForward: true, Deletion: true})
	e.ok()
	ctx := t.Context()
	read := func(id platform.Reader, branches ...string) platform.Rules {
		t.Helper()
		pf, ok := id.(platform.Preflighter)
		if !ok {
			t.Fatal("not a Preflighter")
		}
		rules, err := pf.Preflight(ctx, r, branches)
		if err != nil {
			t.Fatal(err)
		}
		return rules
	}
	check := func(what string, got, want platform.Rules) {
		t.Helper()
		if got.Known != want.Known || got.SignedCommits != want.SignedCommits || !slices.Equal(got.NoForcePush, want.NoForcePush) ||
			!slices.Equal(got.NoDelete, want.NoDelete) || got.WorkflowsKnown != want.WorkflowsKnown || got.Workflows != want.Workflows {
			t.Errorf("%s: %+v, want %+v", what, got, want)
		}
	}
	reader, writer := e.p.Reader(e.reader), e.p.Writer(e.writer)
	check("reader", read(reader, "main", "touchmark/acme"), platform.Rules{Known: true, SignedCommits: true, NoForcePush: []string{"touchmark/acme"},
		NoDelete: []string{"touchmark/acme"}})
	check("writer", read(writer, "main", "touchmark/acme"),
		platform.Rules{Known: true, SignedCommits: true, NoForcePush: []string{"touchmark/acme"}, NoDelete: []string{"touchmark/acme"},
			WorkflowsKnown: true, Workflows: true})
	check("other branches", read(reader, "develop"), platform.Rules{Known: true})
	e.p.Grant(r.ID, e.writer, platform.Perms{Contents: true, PRs: true})
	check("no Workflows", read(writer, "develop"), platform.Rules{Known: true, WorkflowsKnown: true})
	e.p.HideWorkflows(true)
	e.p.HideRules(r.ID, true)
	check("hidden", read(writer, "main", "touchmark/acme"), platform.Rules{})
	if !slices.Contains(e.p.Calls(), "Preflight acme/x main touchmark/acme") {
		t.Errorf("calls %q", e.p.Calls())
	}
	e.p.FailNext("Preflight", errors.New("boom"))
	if _, err := reader.(platform.Preflighter).Preflight(ctx, r, []string{"main"}); err == nil {
		t.Error("an injected fault did not fail Preflight")
	}
}

// signedCommit writes a commit with a signature header on top of parent,
// changing path to content, in the client's repository.
func (c *client) signedCommit(parent, path, content string) string {
	c.t.Helper()
	unsigned := c.commit(parent, "signed change", path, content)
	tree := c.run(nil, "rev-parse", unsigned+"^{tree}")
	obj := fmt.Sprintf("tree %s\nparent %s\nauthor touchmark test <test@example.com> 1767225600 +0000\n"+
		"committer touchmark test <test@example.com> 1767225600 +0000\n"+
		"gpgsig -----BEGIN SSH SIGNATURE-----\n placeholder\n -----END SSH SIGNATURE-----\n\nsigned change\n", tree, parent)
	return c.run(strings.NewReader(obj), "hash-object", "-t", "commit", "-w", "--stdin")
}

// pushRef pushes commit to the full ref name with a lease on expect.
func (c *client) pushRef(commit, ref, expect string) error {
	_, err := c.try(nil, "push", "--porcelain", "--atomic", "--no-verify", "--force-with-lease="+ref+":"+expect, "origin", commit+":"+ref)
	return err
}

// TestRulesetsOnPush: the git server holds pushes to the rulesets: a
// branch that requires signatures refuses a commit without a signature
// header and takes a signed one; one that refuses force pushes takes a
// fast-forward and a deletion but not a forced update; one that refuses
// deletions takes any update but not a deletion; an account on the
// bypass list and a hidden ref are not held; the refusals read as GitHub's
// GH013.
func TestRulesetsOnPush(t *testing.T) {
	e := newGitEnv(t)
	r := e.repo("acme/x", "README.md", "x\n")
	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"signed/*"}, RequiredSignatures: true})
	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"linear"}, NonFastForward: true})
	e.ok()
	c := newClient(t, e.target(r).Remote())
	base := c.mustFetch("main")

	unsigned := c.commit(base, "unsigned change", "a.txt", "a\n")
	err := c.push(unsigned, "signed/x", "")
	if err == nil || !strings.Contains(err.Error(), "GH013") || !strings.Contains(err.Error(), "Commits must have verified signatures.") {
		t.Errorf("unsigned push: %v", err)
	}
	signed := c.signedCommit(base, "a.txt", "a\n")
	c.mustPush(signed, "signed/x", "")
	if err := c.pushRef(unsigned, "refs/touchmark/0123456789abcdef/stage", ""); err != nil {
		t.Errorf("a hidden ref: %v", err)
	}

	first := c.commit(base, "first", "b.txt", "1\n")
	c.mustPush(first, "linear", "")
	next := c.commit(first, "next", "b.txt", "2\n")
	c.mustPush(next, "linear", first)
	other := c.commit(base, "other", "b.txt", "3\n")
	err = c.push(other, "linear", next)
	if err == nil || !strings.Contains(err.Error(), "Cannot force-push to this branch") {
		t.Errorf("forced push: %v", err)
	}
	if got := e.p.Branch(r.ID, "linear"); got != next {
		t.Errorf("the refused push moved the branch to %s", got)
	}
	c.mustPush("", "linear", next)

	// A rule against deletions refuses deleting the branch, not moving it.
	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"kept"}, Deletion: true})
	e.ok()
	c.mustPush(first, "kept", "")
	c.mustPush(other, "kept", first)
	err = c.push("", "kept", other)
	if err == nil || !strings.Contains(err.Error(), "GH013") || !strings.Contains(err.Error(), "Cannot delete this protected branch.") {
		t.Errorf("a deletion: %v", err)
	}
	if got := e.p.Branch(r.ID, "kept"); got != other {
		t.Errorf("the refused deletion left the branch at %q", got)
	}

	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"~ALL"}, NonFastForward: true, RequiredSignatures: true, Bypass: []platform.Account{e.writer}})
	e.ok()
	c.mustPush(other, "bypassed", "")
	c.mustPush(unsigned, "bypassed", other)
	if refs := e.p.HiddenRefs(r.ID); !slices.Equal(refs, []string{"refs/touchmark/0123456789abcdef/stage " + unsigned}) {
		t.Errorf("hidden refs %q", refs)
	}
}

// TestAPICommit: WithAPICommits makes the per-target writers Committers.
// Commit makes the platform's signed commit of the staged tree, authored
// by the writer and committed by the platform, moves the branch with
// compare-and-swap and deletes the stage ref; a stale lease is
// ClassConflict with the stage ref kept; an unsigned commit (SetAPISigning
// off) is ErrUnsigned with the branch where it was; the Workflows
// permission and the rule against force pushes hold it as they hold a
// push.
func TestAPICommit(t *testing.T) {
	plain := newGitEnv(t)
	if _, ok := plain.target(plain.repo("acme/plain", "README.md", "x\n")).(platform.Committer); ok {
		t.Error("a per-target writer without WithAPICommits is a Committer")
	}

	e := newGitEnv(t, fake.WithAPICommits())
	if c := e.p.Caps(); !c.Commit.API || !c.Commit.SignedByPlatform || !c.Commit.CAS {
		t.Errorf("caps %+v", c.Commit)
	}
	r := e.repo("acme/x", "README.md", "x\n")
	ctx := t.Context()
	tw := e.target(r)
	cm, ok := tw.(platform.Committer)
	if !ok {
		t.Fatal("the per-target writer is not a Committer")
	}
	c := newClient(t, tw.Remote())
	base := c.mustFetch("main")
	local := c.commit(base, "chore: sync engineering assets\n\nTouchmark-Stream: sync", "AGENTS.md", "agents\n")
	tree := c.run(nil, "rev-parse", local+"^{tree}")
	const stage = "refs/touchmark/0123456789abcdef/stage"
	stageIt := func() {
		t.Helper()
		if err := c.pushRef(local, stage, ""); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}
	req := platform.CommitRequest{Branch: "touchmark/acme", Parent: base, Tree: tree, Message: "chore: sync engineering assets\n", Stage: stage}

	stageIt()
	got, err := cm.Commit(ctx, r, req)
	if err != nil || !got.Verified || !got.CAS || got.Tree != tree || got.SHA == local {
		t.Fatalf("Commit: %+v, %v", got, err)
	}
	if head := e.p.Branch(r.ID, req.Branch); head != got.SHA {
		t.Errorf("the branch is at %s, not %s", head, got.SHA)
	}
	if refs := e.p.HiddenRefs(r.ID); len(refs) > 0 {
		t.Errorf("stage left: %q", refs)
	}
	obj := gitIn(t, e.p.GitDir(r.ID), "cat-file", "commit", got.SHA)
	for _, part := range []string{"tree " + tree, "parent " + base, "author acme-write[bot] <", "committer GitHub <noreply@github.com>", "\ngpgsig "} {
		if !strings.Contains(obj, part) {
			t.Errorf("the commit lacks %q:\n%s", part, obj)
		}
	}
	calls := e.p.Writes()
	if !slices.Contains(calls, "Commit acme/x touchmark/acme") || !slices.Contains(calls, "UpdateRefs acme/x touchmark/acme") {
		t.Errorf("writes %q", calls)
	}

	stageIt()
	stale := req
	stale.Expect = base
	_, err = cm.Commit(ctx, r, stale)
	wantClass(t, "stale lease", err, platform.ClassConflict)
	if head := e.p.Branch(r.ID, req.Branch); head != got.SHA {
		t.Errorf("a stale lease moved the branch to %s", head)
	}
	if refs := e.p.HiddenRefs(r.ID); len(refs) != 1 {
		t.Errorf("the stage ref should stay after a refused update: %q", refs)
	}

	e.p.SetAPISigning(false)
	again := req
	again.Expect = got.SHA
	unsigned, err := cm.Commit(ctx, r, again)
	var pe *platform.Error
	if !errors.Is(err, platform.ErrUnsigned) || !errors.As(err, &pe) || pe.Class != platform.ClassUnsupported || pe.Rule != "cannot-sign" ||
		unsigned.SHA == "" || unsigned.Verified {
		t.Errorf("unsigned: %+v, %v", unsigned, err)
	}
	if head := e.p.Branch(r.ID, req.Branch); head != got.SHA {
		t.Errorf("an unsigned commit moved the branch to %s", head)
	}
	if refs := e.p.HiddenRefs(r.ID); len(refs) > 0 {
		t.Errorf("stage left after an unsigned commit: %q", refs)
	}
	e.p.SetAPISigning(true)

	// Staged by a token that may change workflows; committed by one that
	// may not.
	all, err := e.p.Writer(e.writer).Target(ctx, r, platform.Perms{Contents: true, PRs: true, Workflows: true})
	if err != nil {
		t.Fatal(err)
	}
	cw := newClient(t, all.Remote())
	cw.mustFetch("main")
	wf := cw.commit(base, "workflow", ".github/workflows/ci.yml", "on: push\n")
	if err := cw.pushRef(wf, stage, ""); err != nil {
		t.Fatal(err)
	}
	wfTree := cw.run(nil, "rev-parse", wf+"^{tree}")
	if err := all.Close(); err != nil {
		t.Fatal(err)
	}
	wfReq := req
	wfReq.Branch, wfReq.Tree = "touchmark/wf", wfTree
	_, err = cm.Commit(ctx, r, wfReq)
	if !errors.As(err, &pe) || pe.Class != platform.ClassPermission || pe.Rule != "workflows" {
		t.Errorf("workflows: %v", err)
	}
	gitIn(t, e.p.GitDir(r.ID), "update-ref", "-d", stage)

	e.p.AddRuleset(r.ID, fake.Ruleset{Branches: []string{"touchmark/*"}, NonFastForward: true})
	e.ok()
	stageIt()
	forced := req
	forced.Expect, forced.Message = got.SHA, "chore: sync engineering assets again\n"
	_, err = cm.Commit(ctx, r, forced)
	if !errors.As(err, &pe) || pe.Class != platform.ClassPolicy || !strings.Contains(err.Error(), "Cannot force-push") {
		t.Errorf("forced: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = cm.Commit(ctx, r, req)
	wantClass(t, "closed writer", err, platform.ClassAuth)
	e.violations()
}

// TestAPICommitMemoryMode: API commits need git mode.
func TestAPICommitMemoryMode(t *testing.T) {
	e := newEnv(t, fake.WithAPICommits())
	r := e.repo("acme/x", "README.md", "x\n")
	cm := e.target(r).(platform.Committer)
	id := strings.Repeat("a", 40)
	_, err := cm.Commit(t.Context(), r, platform.CommitRequest{Branch: "b", Parent: id, Tree: id, Message: "m\n"})
	wantClass(t, "memory mode", err, platform.ClassUnsupported)
}
