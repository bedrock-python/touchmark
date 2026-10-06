package fake_test

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestGitDeliveryClient drives the fake with gitx's delivery client, as
// distribute will: ls-remote, blobless fetches, blobs by id, a commit built
// without a work tree, pushes with a lease, credentials in a header.
func TestGitDeliveryClient(t *testing.T) {
	t.Parallel()
	needGit(t, gitx.DeliveryMinVersion, "gitx's delivery client")
	ctx := t.Context()
	e := newGitEnv(t)
	attrs := []byte("*.md text eol=lf\n")
	r := e.repo("acme/api", "README.md", "hello\n", ".gitattributes", string(attrs))
	tw := e.target(r)
	rem := tw.Remote()
	repo, err := gitx.InitTarget(ctx, filepath.Join(t.TempDir(), "target"), rem.URL, gitx.Auth{Header: rem.Header},
		gitx.Isolation{Home: t.TempDir(), AllowHTTP: true})
	must(t, err)

	refs, err := repo.RemoteRefs(ctx, "refs/heads/main", "refs/heads/touchmark/hub")
	must(t, err)
	if len(refs) != 1 || refs["refs/heads/main"] != e.p.Head(r.ID) {
		t.Errorf("RemoteRefs = %v, want main at %s", refs, e.p.Head(r.ID))
	}
	base, ok, err := repo.FetchBranch(ctx, "main", 1)
	if err != nil || !ok || base != e.p.Head(r.ID) {
		t.Fatalf("FetchBranch(main) = %s, %v, %v", base, ok, err)
	}
	if _, ok, err := repo.FetchBranch(ctx, "touchmark/hub", 20); err != nil || ok {
		t.Errorf("FetchBranch of a missing branch = %v, %v", ok, err)
	}
	attrsOID := gitx.RawOID(attrs)
	must(t, repo.FetchBlobs(ctx, []string{attrsOID}))
	if got, err := repo.ReadBlob(ctx, attrsOID); err != nil || !bytes.Equal(got, attrs) {
		t.Errorf("ReadBlob = %q, %v", got, err)
	}

	build := func(parent, content string) string {
		t.Helper()
		oid := gitx.RawOID([]byte(content))
		writer := gitx.Person{Name: e.writer.Login, Email: e.writer.Email}
		built, err := repo.BuildCommit(ctx, gitx.CommitSpec{
			Parent:  parent,
			Changes: []gitx.Change{{Path: "AGENTS.md", Mode: "100644", OID: oid}},
			Blobs: map[string]gitx.Blob{oid: {OID: oid, Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte(content))), nil
			}}},
			Author: writer, Committer: writer,
			When:    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Message: "chore: sync engineering assets\n\nTouchmark-Hub: acme@github.com/1\n",
		})
		if err != nil {
			t.Fatalf("BuildCommit: %v", err)
		}
		return built.Commit
	}
	push := func(spec gitx.PushSpec, want gitx.PushStatus) {
		t.Helper()
		res, err := repo.Push(ctx, spec)
		if err != nil || res.Status != want {
			t.Errorf("Push %+v = %+v, %v; want status %d", spec, res, err, want)
		}
	}

	c1 := build(base, "# agents v1\n")
	push(gitx.PushSpec{Branch: "touchmark/hub", Commit: c1}, gitx.PushOK)
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "chore: sync engineering assets"})
	must(t, err)
	if pr.HeadSHA != c1 {
		t.Errorf("PR head %s, want %s", pr.HeadSHA, c1)
	}
	f, err := e.p.Reader(e.reader).ReadFile(ctx, r, "touchmark/hub", "AGENTS.md", 100)
	if err != nil || string(f.Content) != "# agents v1\n" {
		t.Errorf("the pushed file: %q, %v", f.Content, err)
	}
	push(gitx.PushSpec{Branch: "touchmark/hub", Commit: c1}, gitx.PushUpToDate)

	// A lease on a stale head is refused; the right one moves the branch.
	c2 := build(base, "# agents v2\n")
	push(gitx.PushSpec{Branch: "touchmark/hub", Commit: c2, Expect: base}, gitx.PushStale)
	push(gitx.PushSpec{Branch: "touchmark/hub", Commit: c2, Expect: c1}, gitx.PushOK)
	if got := e.p.PR(r.ID, pr.Number).HeadSHA; got != c2 {
		t.Errorf("PR head after the update %s, want %s", got, c2)
	}

	// The reader's credential cannot push (the identity's permission, not a
	// broken credential); a server failure is an error, a rate limit
	// deferred.
	rrem, err := e.p.Reader(e.reader).Remote(ctx, r)
	must(t, err)
	res, err := repo.WithAuth(gitx.Auth{Header: rrem.Header}).Push(ctx, gitx.PushSpec{Branch: "other", Commit: c2})
	if err != nil || res.Status != gitx.PushPermission {
		t.Errorf("a push with the reader's credential = %+v, %v; want PushPermission", res, err)
	}
	e.p.FailNext("Push", &platform.Error{Class: platform.ClassTransient, Status: 503})
	push(gitx.PushSpec{Branch: "other", Commit: c2}, gitx.PushError)
	e.p.FailNext("Push", &platform.Error{Class: platform.ClassRateLimited, Status: 429})
	push(gitx.PushSpec{Branch: "other", Commit: c2}, gitx.PushRateLimited)

	// Deleting the branch of an open PR closes it, and is forbidden.
	push(gitx.PushSpec{Branch: "touchmark/hub", Expect: c2}, gitx.PushOK)
	if got := e.p.PR(r.ID, pr.Number); got.State != platform.Closed {
		t.Errorf("after the deletion the PR is %s", got.State)
	}
	e.violations("deleted-open-branch")

	// After Close the credential is gone.
	must(t, tw.Close())
	if _, err := repo.Push(ctx, gitx.PushSpec{Branch: "late", Commit: c2}); platform.ClassOf(err) != platform.ClassAuth {
		t.Errorf("a push after Close: %v, want an auth error", err)
	}
	if e.p.Branch(r.ID, "late") != "" || e.p.Branch(r.ID, "other") != "" {
		t.Error("a refused push moved a branch")
	}
}
