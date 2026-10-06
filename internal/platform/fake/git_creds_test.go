package fake_test

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// deliveryRepo is gitx's target repository for a remote, as distribute
// makes it.
func deliveryRepo(t *testing.T, rem platform.Remote) *gitx.TargetRepo {
	t.Helper()
	repo, err := gitx.InitTarget(t.Context(), filepath.Join(t.TempDir(), "target"), rem.URL, gitx.Auth{Header: rem.Header},
		gitx.Isolation{Home: t.TempDir(), AllowHTTP: true})
	must(t, err)
	return repo
}

// fetchHead fetches branch without blobs and returns its tip.
func fetchHead(t *testing.T, repo *gitx.TargetRepo, branch string) string {
	t.Helper()
	sha, ok, err := repo.FetchBranch(t.Context(), branch, 1)
	if err != nil || !ok {
		t.Fatalf("FetchBranch(%s) = %v, %v", branch, ok, err)
	}
	return sha
}

// buildOn builds a commit on parent that writes path with content.
func buildOn(t *testing.T, repo *gitx.TargetRepo, parent, path, content string) string {
	t.Helper()
	oid := gitx.RawOID([]byte(content))
	bot := gitx.Person{Name: "acme-write[bot]", Email: "bot@example.com"}
	built, err := repo.BuildCommit(t.Context(), gitx.CommitSpec{
		Parent:  parent,
		Changes: []gitx.Change{{Path: path, Mode: "100644", OID: oid}},
		Blobs: map[string]gitx.Blob{oid: {OID: oid, Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte(content))), nil
		}}},
		Author: bot, Committer: bot, When: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Message: "chore: sync\n",
	})
	must(t, err)
	return built.Commit
}

// pushWith pushes commit to branch of repo's remote with rem's credential.
func pushWith(t *testing.T, repo *gitx.TargetRepo, rem platform.Remote, branch, commit, expect string) gitx.PushResult {
	t.Helper()
	res, err := repo.WithAuth(gitx.Auth{Header: rem.Header}).Push(t.Context(), gitx.PushSpec{Branch: branch, Commit: commit, Expect: expect})
	if err != nil {
		t.Fatalf("Push(%s) = %v", branch, err)
	}
	return res
}

// TestGitTargetCredentials: on GitHub, each credential the fake hands out
// reaches only what the platform lets it reach, so a pipeline that pushes
// with the wrong one, skips Target or ignores NeedWorkflows fails in tests.
func TestGitTargetCredentials(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	a := e.repo("acme/a", "README.md", "a\n")
	b := e.repo("acme/b", "README.md", "b\n")
	full := e.target(a) // contents and pull requests, no workflows
	repo := deliveryRepo(t, full.Remote())
	base := fetchHead(t, repo, "main")
	plain := buildOn(t, repo, base, "docs/x.md", "x\n")
	workflow := buildOn(t, repo, base, ".github/workflows/ci.yml", "on: push\n")
	status := func(what string, got gitx.PushResult, want gitx.PushStatus) {
		t.Helper()
		if got.Status != want {
			t.Errorf("%s: %+v, want %s", what, got, want)
		}
	}

	status("the target's token", pushWith(t, repo, full.Remote(), "touchmark/hub", plain, ""), gitx.PushOK)

	// Minted without contents: refused like a GitHub token with contents
	// read-only.
	prsOnly, err := e.p.Writer(e.writer).Target(ctx, a, platform.Perms{PRs: true})
	must(t, err)
	status("a token without contents", pushWith(t, repo, prsOnly.Remote(), "touchmark/x", plain, ""), gitx.PushPermission)

	// The writer's own remote reads only: writes go through Target.
	wrem, err := e.p.Writer(e.writer).Remote(ctx, a)
	must(t, err)
	status("the writer's read remote", pushWith(t, repo, wrem, "touchmark/y", plain, ""), gitx.PushPermission)
	if _, _, err := deliveryRepo(t, wrem).FetchBranch(ctx, "main", 1); err != nil {
		t.Errorf("a fetch with the writer's read remote: %v", err)
	}

	// Workflows need the permission, for a change of D and for a move
	// across someone else's workflow change.
	status("a workflow without the permission", pushWith(t, repo, full.Remote(), "touchmark/wf", workflow, ""), gitx.PushWorkflows)
	e.push(a, "main", e.other, ".github/workflows/lint.yml", "on: pull_request\n")
	moved := buildOn(t, repo, fetchHead(t, repo, "main"), "docs/x.md", "x\n")
	status("a move across a workflow change", pushWith(t, repo, full.Remote(), "touchmark/hub", moved, plain), gitx.PushWorkflows)
	status("a fresh branch on the new base", pushWith(t, repo, full.Remote(), "touchmark/fresh", moved, ""), gitx.PushOK)
	wf, err := e.p.Writer(e.writer).Target(ctx, a, platform.Perms{Contents: true, PRs: true, Workflows: true})
	must(t, err)
	status("a workflow with the permission", pushWith(t, repo, wf.Remote(), "touchmark/wf", workflow, ""), gitx.PushOK)
	status("a move with the permission", pushWith(t, repo, wf.Remote(), "touchmark/hub", moved, plain), gitx.PushOK)
	if got := e.p.Branch(a.ID, "touchmark/hub"); got != moved {
		t.Errorf("touchmark/hub at %s, want %s", got, moved)
	}

	// A token reaches only its own repository.
	other := deliveryRepo(t, e.target(b).Remote())
	onB := buildOn(t, other, fetchHead(t, other, "main"), "docs/x.md", "x\n")
	if res := pushWith(t, other, full.Remote(), "touchmark/hub", onB, ""); res.Status == gitx.PushOK {
		t.Errorf("a push to acme/b with the token of acme/a: %+v", res)
	}
	if _, _, err := other.WithAuth(gitx.Auth{Header: full.Remote().Header}).FetchBranch(ctx, "main", 1); err == nil {
		t.Error("a fetch of acme/b with the token of acme/a succeeded")
	}
	if e.p.Branch(b.ID, "touchmark/hub") != "" {
		t.Error("a refused push moved a branch of acme/b")
	}

	// A permission withdrawn after the token was minted.
	later := e.target(a)
	e.p.Grant(a.ID, e.writer, platform.Perms{PRs: true})
	e.ok()
	status("a withdrawn grant", pushWith(t, repo, later.Remote(), "touchmark/z", plain, ""), gitx.PushPermission)

	// A closed writer's token is revoked on the server too.
	token := header(t, full.Remote())
	must(t, full.Close())
	if code, _ := httpStatus(t, http.MethodGet, e.srv.URL+"/acme/a.git/info/refs?service=git-upload-pack", token); code != http.StatusUnauthorized {
		t.Errorf("a revoked token: %d, want 401", code)
	}
}

// TestGitTargetCredentialsGitLab: GitLab has no Workflows permission, and
// the writer's personal access token may push where it has write access;
// the per-target token is still bound to its repository.
func TestGitTargetCredentialsGitLab(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t, fake.WithFlavor(fake.GitLab))
	a := e.repo("acme/a", "README.md", "a\n")
	tw := e.target(a)
	repo := deliveryRepo(t, tw.Remote())
	base := fetchHead(t, repo, "main")
	workflow := buildOn(t, repo, base, ".github/workflows/ci.yml", "on: push\n")
	if res := pushWith(t, repo, tw.Remote(), "touchmark/wf", workflow, ""); res.Status != gitx.PushOK {
		t.Errorf("a workflow on GitLab: %+v", res)
	}
	wrem, err := e.p.Writer(e.writer).Remote(ctx, a)
	must(t, err)
	if res := pushWith(t, repo, wrem, "touchmark/pat", workflow, ""); res.Status != gitx.PushOK {
		t.Errorf("the writer's personal access token: %+v", res)
	}
	rrem, err := e.p.Reader(e.reader).Remote(ctx, a)
	must(t, err)
	if res := pushWith(t, repo, rrem, "touchmark/reader", workflow, ""); res.Status != gitx.PushPermission {
		t.Errorf("the reader's token: %+v, want PushPermission", res)
	}
}
