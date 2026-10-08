package conformance

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// commitBranch is the branch testCommit moves through the API, and
// commitStage its stage ref.
const (
	commitBranch = "touchmark/conformance-commit"
	commitStage  = gitx.HiddenRefPrefix + "0123456789abcdef/stage"
)

// testCommit checks platform.Committer, for the drivers
// whose per-target writer is one, against the platform: the core pushes
// its commit of the tree to the stage ref, and Commit makes the platform's
// own commit of that tree on the parent and moves the branch to it with
// compare-and-swap. A stale lease (Expect the branch left) and a lease on
// no branch where one exists are ClassConflict and move nothing; a commit
// that goes through leaves the branch at the returned commit, whose tree
// is the one asked for and whose only parent is Parent, deletes the stage
// ref, and keeps the pull request open on the branch open (the branch
// never passes through the base). Signatures are the platform's to give:
// an unsigned answer (ErrUnsigned) is accepted, with the branch where it
// was. Fixtures whose remotes are not served over HTTP skip, as do writers
// that are no Committer.
func testCommit(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "commit", Files: readme})
	tw := target(t, fx, repo)
	committer, ok := tw.(platform.Committer)
	if !ok {
		t.Skip("the per-target writer makes no API commits")
	}
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
	build := func(path, content string) gitx.Built {
		t.Helper()
		oid := gitx.RawOID([]byte(content))
		who := gitx.Person{Name: writer.Login, Email: writer.Email}
		if who.Email == "" {
			who.Email = "conformance@example.com"
		}
		built, err := local.BuildCommit(ctx, gitx.CommitSpec{
			Parent:  base,
			Changes: []gitx.Change{{Path: path, Mode: "100644", OID: oid}},
			Blobs: map[string]gitx.Blob{oid: {OID: oid, Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte(content))), nil
			}}},
			Author: who, Committer: who, When: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Message: "chore: conformance " + path + "\n",
		})
		must(t, "BuildCommit", err)
		return built
	}
	push := func(spec gitx.PushSpec) {
		t.Helper()
		res, err := local.Push(ctx, spec)
		must(t, "Push", err)
		if res.Status != gitx.PushOK {
			t.Fatalf("push %+v: %+v", spec, res)
		}
	}
	head := func(ref string) string {
		t.Helper()
		refs, err := local.RemoteRefs(ctx, ref)
		must(t, "RemoteRefs", err)
		return refs[ref]
	}

	// The branch exists with a first commit, and the writer's pull request
	// is open on it.
	first := build("conformance/first.md", "first\n")
	push(gitx.PushSpec{Branch: commitBranch, Commit: first.Commit})
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: commitBranch, Base: repo.DefaultBranch, Title: "conformance: commit", Body: markerBody})
	must(t, "CreatePR", err)

	// The commit to make: its objects on the platform through the stage ref.
	next := build("conformance/next.md", "next\n")
	push(gitx.PushSpec{Ref: commitStage, Commit: next.Commit})
	req := platform.CommitRequest{
		Branch:  commitBranch,
		Parent:  base,
		Tree:    next.Tree,
		Changes: []platform.Change{{Path: "conformance/next.md", Mode: "100644", OID: gitx.RawOID([]byte("next\n"))}},
		Blob:    func(string) ([]byte, error) { return []byte("next\n"), nil },
		Message: "chore: conformance next\n\nTouchmark-Conformance: yes\n",
		Stage:   commitStage,
	}

	unsigned := func(err error) bool {
		return platform.ClassOf(err) == platform.ClassUnsupported && ruleOfError(err) == "cannot-sign"
	}
	for _, tc := range []struct {
		name, expect string
	}{
		{"a stale lease", base},
		{"a lease on no branch", ""},
	} {
		stale := req
		stale.Expect = tc.expect
		_, err := committer.Commit(ctx, repo, stale)
		if got := head("refs/heads/" + commitBranch); got != first.Commit {
			t.Errorf("%s: the branch moved to %s", tc.name, got)
		}
		switch {
		case unsigned(err):
			// Unsigned before the lease was tried: nothing moved, and the
			// platform signs no API commit to go on with.
			t.Skipf("the platform does not sign API commits here: %v", err)
		case platform.ClassOf(err) != platform.ClassConflict:
			t.Errorf("%s: %v (class %v), want a conflict", tc.name, err, platform.ClassOf(err))
		}
	}
	if head(commitStage) == "" {
		// A driver may drop the stage ref after a refusal: stage again.
		push(gitx.PushSpec{Ref: commitStage, Commit: next.Commit, Expect: ""})
	}

	req.Expect = first.Commit
	got, err := committer.Commit(ctx, repo, req)
	switch {
	case unsigned(err):
		if h := head("refs/heads/" + commitBranch); h != first.Commit {
			t.Errorf("an unsigned commit moved the branch to %s", h)
		}
		t.Skipf("the platform does not sign API commits here: %v", err)
	case err != nil:
		t.Fatalf("Commit: %v", err)
	}
	now := head("refs/heads/" + commitBranch)
	if now == "" || !strings.EqualFold(now, got.SHA) || !got.CAS || !strings.EqualFold(got.Tree, req.Tree) {
		t.Errorf("Commit = %+v, the branch is at %s", got, now)
	}
	if _, ok, err := local.FetchBranch(ctx, commitBranch, 2); err != nil || !ok {
		t.Fatalf("FetchBranch(%s) = %v, %v", commitBranch, ok, err)
	}
	c, err := local.Commit(ctx, now)
	must(t, "Commit (git)", err)
	if len(c.Parents) != 1 || c.Parents[0] != base {
		t.Errorf("the platform's commit has parents %v, want %s", c.Parents, base)
	}
	diff, err := local.DiffTree(ctx, next.Commit, now)
	must(t, "DiffTree", err)
	if len(diff) > 0 {
		t.Errorf("the platform's commit differs from the tree asked for: %+v", diff)
	}
	if s := head(commitStage); s != "" {
		t.Errorf("the stage ref is left at %s", s)
	}
	prs, err := fx.Reader().PRs(ctx, repo, []string{commitBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	if open, ok := find(prs, pr.Number); !ok || open.State != platform.Open {
		t.Errorf("#%d after the API commit: %+v (found %v), want open", pr.Number, open, ok)
	}
}

// testPreflight checks platform.Preflighter, for the drivers whose writer
// is one: the rules of the default branch and of a sync branch that does
// not exist yet (GitHub's rules/branches answers for any name) are known to
// the writer, with no error, and none forbids force pushes on a repository
// without rules.
func testPreflight(t *testing.T, fx Fixture) {
	pf, ok := fx.Writer().(platform.Preflighter)
	if !ok {
		t.Skip("the writer reads no rules upfront")
	}
	repo := fx.CreateRepo(t, RepoSpec{Name: "preflight", Files: readme})
	rules, err := pf.Preflight(t.Context(), repo, []string{repo.DefaultBranch, SyncBranch})
	switch {
	case err != nil:
		t.Fatalf("Preflight: %v", err)
	case !rules.Known:
		t.Errorf("Preflight = %+v: the writer cannot read the rules of a repository it may write to", rules)
	case len(rules.NoForcePush) > 0 || rules.SignedCommits:
		t.Errorf("Preflight = %+v on a repository without rules", rules)
	}
}

// testPushGuard checks platform.PushGuard, for the drivers whose writer is
// one: on a repository without protection, neither the default branch nor
// a sync branch (one that does not exist yet, one that does) is listed,
// and the call writes nothing.
func testPushGuard(t *testing.T, fx Fixture) {
	g, ok := fx.Writer().(platform.PushGuard)
	if !ok {
		t.Skip("the writer tells no protected branches upfront")
	}
	repo := fx.CreateRepo(t, RepoSpec{Name: "pushguard", Files: readme})
	fx.CreateBranch(t, repo, OtherBranch)
	got, err := g.NoPush(t.Context(), repo, []string{repo.DefaultBranch, SyncBranch, OtherBranch, ""})
	if err != nil || len(got) > 0 {
		t.Errorf("NoPush = %+v, %v on a repository without protection", got, err)
	}
}

// ruleOfError returns the Rule of err's *platform.Error.
func ruleOfError(err error) string {
	var pe *platform.Error
	if errors.As(err, &pe) {
		return pe.Rule
	}
	return ""
}
