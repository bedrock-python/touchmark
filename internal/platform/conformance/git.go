package conformance

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// gitBranch is the branch testTargetGit pushes.
const gitBranch = "touchmark/conformance-git"

// testTargetGit checks the git credentials of the remotes, through gitx as
// distribute pushes: a TargetWriter minted with
// Contents pushes to its repository with its Remote; the reader's Remote
// does not push; with Caps.WorkflowPerm, a TargetWriter minted without
// Workflows does not create a file under .github/workflows (PushWorkflows)
// and one minted with it does. Fixtures whose remotes are not served over
// HTTP (the fake in memory mode) skip.
func testTargetGit(t *testing.T, fx Fixture) {
	ctx := t.Context()
	caps := probe(t, fx)
	repo := fx.CreateRepo(t, RepoSpec{Name: "git", Files: readme})
	tw := target(t, fx, repo)
	rem := tw.Remote()
	if !strings.HasPrefix(rem.URL, "http://") && !strings.HasPrefix(rem.URL, "https://") {
		t.Skipf("the remote %s is not served over HTTP", rem.URL)
	}
	local, err := gitx.InitTarget(ctx, filepath.Join(t.TempDir(), "target"), rem.URL, gitx.Auth{Header: rem.Header},
		gitx.Isolation{Home: t.TempDir(), AllowHTTP: strings.HasPrefix(rem.URL, "http://")})
	must(t, "InitTarget", err)
	base, ok, err := local.FetchBranch(ctx, repo.DefaultBranch, 1)
	if err != nil || !ok {
		t.Fatalf("FetchBranch(%s) = %v, %v", repo.DefaultBranch, ok, err)
	}
	writer := fx.Account(RoleWriter)
	build := func(path string) string {
		t.Helper()
		content := []byte("conformance\n")
		oid := gitx.RawOID(content)
		who := gitx.Person{Name: writer.Login, Email: writer.Email}
		if who.Email == "" {
			who.Email = "conformance@example.com"
		}
		built, err := local.BuildCommit(ctx, gitx.CommitSpec{
			Parent:  base,
			Changes: []gitx.Change{{Path: path, Mode: "100644", OID: oid}},
			Blobs: map[string]gitx.Blob{oid: {OID: oid, Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(content)), nil
			}}},
			Author: who, Committer: who, When: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Message: "chore: conformance\n",
		})
		must(t, "BuildCommit", err)
		return built.Commit
	}
	push := func(with platform.Remote, branch, commit string) gitx.PushResult {
		t.Helper()
		res, err := local.WithAuth(gitx.Auth{Header: with.Header}).Push(ctx, gitx.PushSpec{Branch: branch, Commit: commit})
		must(t, "Push", err)
		return res
	}
	head := func(branch string) string {
		t.Helper()
		refs, err := local.RemoteRefs(ctx, "refs/heads/"+branch)
		must(t, "RemoteRefs", err)
		return refs["refs/heads/"+branch]
	}

	plain := build("conformance/plain.md")
	if res := push(rem, gitBranch, plain); res.Status != gitx.PushOK || head(gitBranch) != plain {
		t.Errorf("a push with the target's remote: %+v, branch at %q", res, head(gitBranch))
	}
	if rrem, err := fx.Reader().Remote(ctx, repo); err != nil {
		t.Errorf("Reader.Remote: %v", err)
	} else if rrem.Header != nil {
		if res := push(rrem, gitBranch+"-reader", plain); res.Status == gitx.PushOK || head(gitBranch+"-reader") != "" {
			t.Errorf("a push with the reader's remote: %+v", res)
		}
	}
	if !caps.WorkflowPerm {
		return
	}
	workflow := build(".github/workflows/conformance.yml")
	if res := push(rem, gitBranch+"-wf", workflow); res.Status != gitx.PushWorkflows || head(gitBranch+"-wf") != "" {
		t.Errorf("a workflow without the Workflows permission: %+v, want %s", res, gitx.PushWorkflows)
	}
	wf, err := fx.Writer().Target(ctx, repo, platform.Perms{Contents: true, PRs: true, Workflows: true})
	must(t, "Target with Workflows", err)
	defer func() {
		if err := wf.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if res := push(wf.Remote(), gitBranch+"-wf", workflow); res.Status != gitx.PushOK || head(gitBranch+"-wf") != workflow {
		t.Errorf("a workflow with the Workflows permission: %+v", res)
	}
}
