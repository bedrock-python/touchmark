package fake_test

import (
	"errors"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

func TestFailNext(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	rd := e.p.Reader(e.reader)
	first := &platform.Error{Op: "read file", Class: platform.ClassTransient, Status: 502}
	second := &platform.Error{Op: "read file", Class: platform.ClassRateLimited, Status: 429}
	e.p.FailNext("ReadFile", first)
	e.p.FailNext("ReadFile", second)
	e.p.FailNext("ReadFile", nil) // ignored
	_, err1 := rd.ReadFile(ctx, r, "", "README.md", 10)
	_, err2 := rd.ReadFile(ctx, r, "", "README.md", 10)
	_, err3 := rd.ReadFile(ctx, r, "", "README.md", 10)
	if !errors.Is(err1, first) || !errors.Is(err2, second) || err3 != nil {
		t.Errorf("queued faults: %v, %v, %v", err1, err2, err3)
	}
	// A fault of another method does not fire.
	e.p.FailNext("PRs", first)
	if _, err := rd.ReadFile(ctx, r, "", "README.md", 10); err != nil {
		t.Errorf("a PRs fault fired on ReadFile: %v", err)
	}
	if _, err := rd.PRs(ctx, r, nil, nil); !errors.Is(err, first) {
		t.Errorf("PRs fault: %v", err)
	}

	// "*" hits the next call of any method, after the method's own queue.
	e.p.FailNext("*", second)
	e.p.FailNext("Self", first)
	_, errSelf := rd.Self(ctx)
	_, errProbe := rd.Probe(ctx)
	_, errAfter := rd.Probe(ctx)
	if !errors.Is(errSelf, first) || !errors.Is(errProbe, second) || errAfter != nil {
		t.Errorf("wildcard: %v, %v, %v", errSelf, errProbe, errAfter)
	}
}

func TestFailSticky(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	rd := e.p.Reader(e.reader)
	down := &platform.Error{Class: platform.ClassAuth, Status: 401}
	e.p.Fail("*", down)
	for range 3 {
		if _, err := rd.Repo(ctx, r.Path); !errors.Is(err, down) {
			t.Errorf("sticky fault: %v", err)
		}
	}
	if _, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, ""); !errors.Is(err, down) {
		t.Errorf("sticky fault on Snapshot: %v", err)
	}
	// A queued fault fires first.
	once := errors.New("once")
	e.p.FailNext("Repo", once)
	if _, err := rd.Repo(ctx, r.Path); !errors.Is(err, once) {
		t.Errorf("queued before sticky: %v", err)
	}
	e.p.Fail("*", nil)
	if _, err := rd.Repo(ctx, r.Path); err != nil {
		t.Errorf("after clearing: %v", err)
	}
	e.p.Fail("PRs", down)
	_, errPRs := rd.PRs(ctx, r, nil, nil)
	_, errRepo := rd.Repo(ctx, r.Path)
	if !errors.Is(errPRs, down) || errRepo != nil {
		t.Errorf("sticky per method: %v, %v", errPRs, errRepo)
	}
}

// TestFailNextApplied: the write happens, the answer is lost; the core must
// reconcile by reading.
func TestFailNextApplied(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	lost := &platform.Error{Op: "create pull request", Class: platform.ClassTransient, Status: 504}
	e.p.FailNextApplied("CreatePR", lost)
	np := platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Labels: []string{"engineering-assets"}}
	pr, err := tw.CreatePR(ctx, np)
	if !errors.Is(err, lost) || pr.Number != 0 {
		t.Fatalf("lost response = %+v, %v", pr, err)
	}
	prs, err := e.p.Reader(e.reader).PRs(ctx, r, []string{"touchmark/hub"}, []platform.Account{e.writer})
	if err != nil || len(prs) != 1 {
		t.Fatalf("the PR was not created: %v, %v", numbers(prs), err)
	}
	dup, err := tw.CreatePR(ctx, np)
	if !errors.Is(err, platform.ErrExists) || dup.Number != prs[0].Number {
		t.Errorf("retry = %+v, %v; want ErrExists with #%d", dup, err, prs[0].Number)
	}

	e.p.FailNextApplied("EditPR", lost)
	if _, err := tw.EditPR(ctx, dup.Number, platform.PREdit{Body: ptr("edited")}); !errors.Is(err, lost) {
		t.Errorf("EditPR lost: %v", err)
	}
	if got := e.p.PR(r.ID, dup.Number).Body; got != "edited" {
		t.Errorf("the edit did not happen: %q", got)
	}
}

func TestCallLog(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	e.p.ResetCalls()
	rd := e.p.Reader(e.reader)
	w := e.p.Writer(e.writer)
	_, _ = rd.Probe(ctx)
	_, _ = rd.Self(ctx)
	_, _ = rd.Lookup(ctx, "jdoe")
	_, _ = rd.Resolve(ctx, platform.Selector{Namespace: "acme"})
	_, _ = rd.Resolve(ctx, platform.Selector{Repo: "acme/api"})
	_, _ = rd.Repo(ctx, "acme/api")
	_, _ = rd.ReadFile(ctx, r, "", ".touchmark.yml", 10)
	_, _ = rd.Remote(ctx, r)
	_, _ = rd.PRs(ctx, r, []string{"touchmark/hub"}, nil)
	_, _ = rd.OpenPRsBy(ctx, nil, nil)
	_, _ = e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "")
	tw, _ := w.Target(ctx, r, platform.Perms{Contents: true, PRs: true})
	pr, _ := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Labels: []string{"engineering-assets"}})
	_, _ = tw.EditPR(ctx, pr.Number, platform.PREdit{AddLabels: []string{"engineering-assets", "new"}})
	_ = tw.Comment(ctx, pr.Number, "hello")
	_, _ = tw.EnsureLabels(ctx, []string{"engineering-assets"})
	_ = tw.Close()
	_ = tw.Comment(ctx, pr.Number, "after close")

	sameList(t, "Calls", e.p.Calls(), []string{
		"Probe",
		"Self",
		"Lookup jdoe",
		"Resolve acme",
		"Resolve acme/api",
		"Repo acme/api",
		"ReadFile acme/api .touchmark.yml",
		"Remote acme/api",
		"PRs acme/api",
		"OpenPRsBy",
		"Snapshot acme/api",
		"Target acme/api",
		"CreatePR acme/api",
		"CreateLabel acme/api engineering-assets",
		"EditPR acme/api #1",
		"CreateLabel acme/api new",
		"Comment acme/api #1",
		"EnsureLabels acme/api",
		"Close acme/api",
		"Comment acme/api #1",
	})
	sameList(t, "Writes", e.p.Writes(), []string{
		"CreatePR acme/api",
		"CreateLabel acme/api engineering-assets",
		"EditPR acme/api #1",
		"CreateLabel acme/api new",
		"Comment acme/api #1",
		"Comment acme/api #1",
	})
	e.p.ResetCalls()
	if len(e.p.Calls()) != 0 || len(e.p.Writes()) != 0 {
		t.Error("ResetCalls kept entries")
	}
}

func TestSnapshotSource(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x", "docs/a.md", "a")
	src := e.p.Snapshots()
	rem, err := e.p.Reader(e.reader).Remote(ctx, r)
	must(t, err)
	tree, err := src.Snapshot(ctx, r, rem, "")
	if err != nil || tree.Commit != e.p.Head(r.ID) || len(tree.Entries) != 2 {
		t.Fatalf("Snapshot = %+v, %v", tree, err)
	}
	// The tree is a copy.
	delete(tree.Entries, "README.md")
	if again, _ := src.Snapshot(ctx, r, rem, "main"); len(again.Entries) != 2 {
		t.Error("Snapshot aliases the store")
	}
	// Observations come from the tree as the snapshot package defines them.
	obs := tree.Observe([]string{"docs/a.md", "docs"})
	if obs["docs/a.md"].OIDs == nil || !obs["docs"].IsDir {
		t.Errorf("Observe = %+v", obs)
	}
	if _, err := src.Snapshot(ctx, r, e.target(r).Remote(), ""); err != nil {
		t.Errorf("Snapshot with the target's remote: %v", err)
	}
	_, err = src.Snapshot(ctx, r, platform.Remote{URL: "fake://github.com/acme/other"}, "")
	wantClass(t, "another repository's remote", err, platform.ClassInvalid)
	_, err = src.Snapshot(ctx, r, rem, "touchmark/hub")
	wantClass(t, "another ref", err, platform.ClassUnsupported)
	empty := e.repo("acme/empty")
	_, err = src.Snapshot(ctx, empty, platform.Remote{}, "")
	wantClass(t, "empty repository", err, platform.ClassNotFound)
	wantIs(t, "empty repository", err, platform.ErrNotFound)
	_, err = src.Snapshot(ctx, platform.Repo{ID: "404"}, platform.Remote{}, "")
	wantIs(t, "unknown repository", err, platform.ErrNotFound)
	e.p.FailNext("Snapshot", errors.New("fetch failed"))
	if _, err := src.Snapshot(ctx, r, rem, ""); err == nil || err.Error() != "fetch failed" {
		t.Errorf("Snapshot fault: %v", err)
	}
	if fake.ModeFile != "100644" || fake.ModeGitlink != "160000" {
		t.Error("mode constants")
	}
}
