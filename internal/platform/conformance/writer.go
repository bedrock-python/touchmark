package conformance

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

func testTarget(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "target", Files: readme})
	tw, err := fx.Writer().Target(ctx, repo, platform.Perms{Contents: true, PRs: true})
	must(t, "Target", err)
	noCredentials(t, "TargetWriter.Remote", tw.Remote().URL)
	pr := openPR(t, fx, repo, tw)
	if err := tw.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close revokes the per-target identity: no write and no git header
	// with it afterwards.
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{Body: ptr("after close")})
	wantClass(t, "EditPR after Close", err, platform.ClassAuth)
	wantClass(t, "Comment after Close", tw.Comment(ctx, pr.Number, "after close"), platform.ClassAuth)
	if header := tw.Remote().Header; header != nil {
		_, err := header(ctx)
		wantClass(t, "Remote().Header after Close", err, platform.ClassAuth)
	}
	if got := listed(t, fx, repo, pr.Number); got.Body != markerBody {
		t.Errorf("a write after Close took effect: body %q", got.Body)
	}
}

// wantClass checks that err is an error of class.
func wantClass(t *testing.T, what string, err error, class platform.Class) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error, want %v", what, class)
	case platform.ClassOf(err) != class:
		t.Errorf("%s: class %v of %v, want %v", what, platform.ClassOf(err), err, class)
	}
}

func testTargetDenied(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "read-only", Files: readme, ReadOnly: true})
	tw, err := fx.Writer().Target(t.Context(), repo, platform.Perms{Contents: true, PRs: true})
	if err == nil {
		_ = tw.Close()
		t.Fatal("Target on a repository the writer may not write succeeded")
	}
	if c := platform.ClassOf(err); c != platform.ClassPermission && c != platform.ClassNotFound {
		t.Errorf("Target denied: class %v of %v, want permission or not-found", c, err)
	}
}

// newPR is the pull request the writer tests open.
func newPR(repo platform.Repo) platform.NewPR {
	return platform.NewPR{
		Head:   SyncBranch,
		Base:   repo.DefaultBranch,
		Title:  "chore: sync engineering assets",
		Body:   markerBody,
		Labels: []string{"engineering-assets"},
	}
}

// openPR opens newPR(repo) through the writer.
func openPR(t *testing.T, fx Fixture, repo platform.Repo, tw platform.TargetWriter) platform.PR {
	t.Helper()
	fx.CreateBranch(t, repo, SyncBranch)
	pr, err := tw.CreatePR(t.Context(), newPR(repo))
	must(t, "CreatePR", err)
	return pr
}

// listed returns pull request number of repo as PRs reports it to the
// reader.
func listed(t *testing.T, fx Fixture, repo platform.Repo, number int64) platform.PR {
	t.Helper()
	prs, err := fx.Reader().PRs(t.Context(), repo, []string{SyncBranch}, []platform.Account{fx.Account(RoleWriter)})
	must(t, "PRs", err)
	pr, ok := find(prs, number)
	if !ok {
		t.Fatalf("PRs %v: no #%d", numbers(prs), number)
	}
	return pr
}

func testCreatePR(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "create", Files: readme})
	tw := target(t, fx, repo)
	writer := fx.Account(RoleWriter)
	want := newPR(repo)
	pr := openPR(t, fx, repo, tw)

	for name, got := range map[string]platform.PR{"CreatePR": pr, "PRs": listed(t, fx, repo, pr.Number)} {
		switch {
		case got.Number <= 0 || got.State != platform.Open || got.Draft:
			t.Errorf("%s: #%d state %s draft %v, want an open ready PR", name, got.Number, got.State, got.Draft)
		case got.Head != want.Head || got.Base != want.Base || !got.BaseExists:
			t.Errorf("%s: %s → %s (exists %v), want %s → %s", name, got.Head, got.Base, got.BaseExists, want.Head, want.Base)
		case got.Title != want.Title || got.Body != want.Body:
			t.Errorf("%s: title %q body %q, want them verbatim", name, got.Title, got.Body)
		case !slices.Contains(got.Labels, "engineering-assets"):
			t.Errorf("%s: labels %v, want engineering-assets", name, got.Labels)
		case got.Author.ID != writer.ID:
			t.Errorf("%s: author %s, want the writer %s", name, got.Author.ID, writer.ID)
		case got.RepoID != repo.ID || got.HeadRepoID != repo.ID:
			t.Errorf("%s: repo %s head repo %s, want %s for both", name, got.RepoID, got.HeadRepoID, repo.ID)
		case got.CreatedAt.IsZero() || !got.ClosedAt.IsZero() || got.ClosedBy != nil:
			t.Errorf("%s: created %v closed %v by %v", name, got.CreatedAt, got.ClosedAt, got.ClosedBy)
		}
	}

	// An open PR from the same head comes back with ErrExists, whatever
	// base the new one names: GitHub, Gitea and Forgejo would open a second
	// PR from the head to another base.
	fx.CreateBranch(t, repo, "develop")
	for _, base := range []string{want.Base, "develop"} {
		np := want
		np.Base = base
		dup, err := tw.CreatePR(t.Context(), np)
		wantErr(t, "CreatePR from the same head to "+base, err, platform.ErrExists, platform.ClassConflict)
		if dup.Number != pr.Number || dup.RepoID != repo.ID || dup.HeadRepoID != repo.ID {
			t.Errorf("CreatePR from the same head to %s returned #%d (repo %s, head repo %s), want the open #%d of %s itself",
				base, dup.Number, dup.RepoID, dup.HeadRepoID, pr.Number, repo.ID)
		}
	}
	prs, err := fx.Reader().PRs(t.Context(), repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	if len(prs) != 1 {
		t.Errorf("PRs from the head: %v, want only #%d", numbers(prs), pr.Number)
	}
}

// testCreatePRBesideFork: a pull request someone opened from a fork's
// branch of the same name is no duplicate: the writer's own opens (threat
// T9 in docs/project/threat-model.md), and a later CreatePR finds the
// writer's, not the fork's.
func testCreatePRBesideFork(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "beside-fork", Files: readme})
	fork := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "from a fork", Body: markerBody, Author: RoleOther, Fork: true})
	tw := target(t, fx, repo)
	pr := openPR(t, fx, repo, tw)
	if pr.Number == fork.Number || pr.HeadRepoID != repo.ID || pr.State != platform.Open {
		t.Errorf("CreatePR beside a fork's #%d: #%d head repo %s state %s, want a new open PR of %s",
			fork.Number, pr.Number, pr.HeadRepoID, pr.State, repo.ID)
	}
	dup, err := tw.CreatePR(t.Context(), newPR(repo))
	wantErr(t, "CreatePR again", err, platform.ErrExists, platform.ClassConflict)
	if dup.Number != pr.Number {
		t.Errorf("CreatePR again returned #%d, want the writer's #%d, not the fork's #%d", dup.Number, pr.Number, fork.Number)
	}
}

func testCreatePRAfterClose(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "reopen", Files: readme})
	tw := target(t, fx, repo)
	first := openPR(t, fx, repo, tw)
	_, err := tw.EditPR(t.Context(), first.Number, platform.PREdit{State: ptr(platform.Closed)})
	must(t, "close", err)
	second, err := tw.CreatePR(t.Context(), newPR(repo))
	must(t, "CreatePR after the close", err)
	if second.Number == first.Number || second.State != platform.Open {
		t.Errorf("CreatePR after the close: #%d %s, want a new open PR (first #%d)", second.Number, second.State, first.Number)
	}
}

func testCreatePRDraft(t *testing.T, fx Fixture) {
	caps := probe(t, fx)
	repo := fx.CreateRepo(t, RepoSpec{Name: "draft", Files: readme})
	tw := target(t, fx, repo)
	fx.CreateBranch(t, repo, SyncBranch)
	np := newPR(repo)
	np.Draft = true
	pr, err := tw.CreatePR(t.Context(), np)
	must(t, "CreatePR draft", err)
	if !pr.Draft {
		t.Log("the platform refused the draft; the PR is ready")
	} else if caps.Draft == platform.DraftTitlePrefix && !hasPrefixFold(pr.Title, strings.TrimSpace(caps.DraftPrefix)) {
		t.Errorf("draft title %q lacks the prefix %q", pr.Title, caps.DraftPrefix)
	}
	edited, err := tw.EditPR(t.Context(), pr.Number, platform.PREdit{Title: ptr("chore: renamed"), Body: ptr("renamed")})
	must(t, "EditPR of a draft", err)
	if edited.Draft != pr.Draft {
		t.Errorf("EditPR changed the draft state: %v → %v", pr.Draft, edited.Draft)
	}
	if got := listed(t, fx, repo, pr.Number); got.Draft != pr.Draft {
		t.Errorf("PRs: draft %v after EditPR, want %v", got.Draft, pr.Draft)
	}
}

func testCreatePRMaxBody(t *testing.T, fx Fixture) {
	caps := probe(t, fx)
	repo := fx.CreateRepo(t, RepoSpec{Name: "max-body", Files: readme})
	tw := target(t, fx, repo)
	fx.CreateBranch(t, repo, SyncBranch)
	np := newPR(repo)
	np.Body = bodyOf(caps.MaxBody)
	pr, err := tw.CreatePR(t.Context(), np)
	must(t, fmt.Sprintf("CreatePR with a body of Caps.MaxBody = %d bytes", caps.MaxBody), err)
	if got := listed(t, fx, repo, pr.Number); got.Body != np.Body {
		t.Errorf("a body of %d bytes came back with %d bytes", len(np.Body), len(got.Body))
	}
}

// bodyOf returns an ASCII body of n bytes that ends in a marker-like line
// and has no line starting with "/".
func bodyOf(n int) string {
	tail := "\n<!-- touchmark:v1 hub=conformance -->"
	var b strings.Builder
	for b.Len() < n-len(tail) {
		line := "| update | `docs/file-" + strconv.Itoa(b.Len()) + ".md` | pack |\n"
		b.WriteString(line)
	}
	s := b.String()[:max(0, n-len(tail))] + tail
	return s[len(s)-n:]
}

func testEditPR(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "edit", Files: readme})
	tw := target(t, fx, repo)
	pr := openPR(t, fx, repo, tw)
	title, body := "chore: edited", "edited body\n\n<!-- touchmark:v1 hub=conformance edited -->"
	got, err := tw.EditPR(t.Context(), pr.Number, platform.PREdit{Title: &title, Body: &body, AddLabels: []string{"conformance-a"}})
	must(t, "EditPR", err)
	for name, p := range map[string]platform.PR{"EditPR": got, "PRs": listed(t, fx, repo, pr.Number)} {
		if p.Title != title || p.Body != body || p.State != platform.Open {
			t.Errorf("%s: title %q body %q state %s, want all edits applied", name, p.Title, p.Body, p.State)
		}
		if !slices.Contains(p.Labels, "engineering-assets") || !slices.Contains(p.Labels, "conformance-a") {
			t.Errorf("%s: labels %v, want engineering-assets and conformance-a", name, p.Labels)
		}
	}
	// Labels are only ever added.
	got, err = tw.EditPR(t.Context(), pr.Number, platform.PREdit{AddLabels: []string{"conformance-b"}})
	must(t, "EditPR labels", err)
	for _, l := range []string{"engineering-assets", "conformance-a", "conformance-b"} {
		if !slices.Contains(got.Labels, l) {
			t.Errorf("EditPR labels: %v lacks %s", got.Labels, l)
		}
	}
	if got.Title != title || got.Body != body {
		t.Errorf("EditPR of labels changed title %q or body %q", got.Title, got.Body)
	}
}

func testEditPRState(t *testing.T, fx Fixture) {
	caps := probe(t, fx)
	repo := fx.CreateRepo(t, RepoSpec{Name: "state", Files: readme})
	tw := target(t, fx, repo)
	writer := fx.Account(RoleWriter)
	pr := openPR(t, fx, repo, tw)

	// Body and state in one request, as touchmark closes its own PRs.
	body := "closed by touchmark\n\n<!-- touchmark:v1 hub=conformance closed -->"
	got, err := tw.EditPR(t.Context(), pr.Number, platform.PREdit{Body: &body, State: ptr(platform.Closed)})
	must(t, "EditPR close", err)
	for name, p := range map[string]platform.PR{"EditPR": got, "PRs": listed(t, fx, repo, pr.Number)} {
		if p.State != platform.Closed || p.Body != body || p.ClosedAt.IsZero() {
			t.Errorf("%s: state %s body %q closed at %v, want closed with the new body", name, p.State, p.Body, p.ClosedAt)
		}
		checkClosedBy(t, caps, p, writer)
	}

	got, err = tw.EditPR(t.Context(), pr.Number, platform.PREdit{State: ptr(platform.Open)})
	must(t, "EditPR reopen", err)
	for name, p := range map[string]platform.PR{"EditPR": got, "PRs": listed(t, fx, repo, pr.Number)} {
		if p.State != platform.Open || p.ClosedBy != nil || !p.ClosedAt.IsZero() || p.Body != body {
			t.Errorf("%s: state %s closed by %v at %v, want open again", name, p.State, p.ClosedBy, p.ClosedAt)
		}
	}
}

func testEditPRBase(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "base", Files: readme})
	tw := target(t, fx, repo)
	fx.CreateBranch(t, repo, "develop")
	pr := openPR(t, fx, repo, tw)
	got, err := tw.EditPR(t.Context(), pr.Number, platform.PREdit{Base: ptr("develop")})
	must(t, "EditPR base", err)
	for name, p := range map[string]platform.PR{"EditPR": got, "PRs": listed(t, fx, repo, pr.Number)} {
		if p.Base != "develop" || !p.BaseExists || p.State != platform.Open {
			t.Errorf("%s: base %q exists %v state %s, want develop", name, p.Base, p.BaseExists, p.State)
		}
	}
}

// testEditPRRefused: an edit the platform refuses changes nothing (body and
// state go in one request): reopening a merged PR fails, and its title,
// body and labels stay as they were. A base that does not exist, when
// refused, is refused the same way (the fake without git branches accepts
// any base).
func testEditPRRefused(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "refused", Files: readme})
	tw := target(t, fx, repo)
	pr := openPR(t, fx, repo, tw)
	// unchanged checks that the PR still is as before, and has no label
	// label.
	unchanged := func(what string, before platform.PR, label string) {
		t.Helper()
		got := listed(t, fx, repo, pr.Number)
		if got.Title != before.Title || got.Body != before.Body || got.Base != before.Base || slices.Contains(got.Labels, label) {
			t.Errorf("%s: title %q body %q base %q labels %v, want them unchanged", what, got.Title, got.Body, got.Base, got.Labels)
		}
	}
	edit := func(n int) platform.PREdit {
		return platform.PREdit{Title: ptr("refused title " + itoa(int64(n))), Body: ptr("refused body " + itoa(int64(n))),
			AddLabels: []string{"conformance-refused-" + itoa(int64(n))}}
	}

	e := edit(1)
	e.Base = ptr("conformance-no-such-base")
	if _, err := tw.EditPR(t.Context(), pr.Number, e); err != nil {
		unchanged("EditPR to a missing base", pr, e.AddLabels[0])
	}

	before := listed(t, fx, repo, pr.Number)
	fx.SetPRState(t, repo, pr.Number, platform.Merged, RoleOther)
	e = edit(2)
	e.State = ptr(platform.Open)
	_, err := tw.EditPR(t.Context(), pr.Number, e)
	if err == nil {
		t.Fatal("EditPR reopened a merged PR")
	}
	unchanged("EditPR reopening a merged PR", before, e.AddLabels[0])
	if got := listed(t, fx, repo, pr.Number); got.State != platform.Merged {
		t.Errorf("after the refused reopen: state %s, want merged", got.State)
	}
}

func testEditPRMissing(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "missing", Files: readme})
	tw := target(t, fx, repo)
	const missing = 999999
	_, err := tw.EditPR(t.Context(), missing, platform.PREdit{Body: ptr("x")})
	wantErr(t, "EditPR of a missing PR", err, platform.ErrNotFound, platform.ClassNotFound)
	err = tw.Comment(t.Context(), missing, "x")
	wantErr(t, "Comment on a missing PR", err, platform.ErrNotFound, platform.ClassNotFound)
}

func testComment(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "comment", Files: readme})
	tw := target(t, fx, repo)
	pr := openPR(t, fx, repo, tw)
	body := "touchmark remembers that this content was declined ⚠\n\n```yaml\nignore:\n  - AGENTS.md\n```"
	must(t, "Comment", tw.Comment(t.Context(), pr.Number, body))
	got := fx.Comments(t, repo, pr.Number)
	if len(got) == 0 || got[len(got)-1] != body {
		t.Errorf("Comments = %q, want the last to be %q", got, body)
	}
}

func testEnsureLabels(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "labels", Files: readme})
	tw := target(t, fx, repo)
	first, err := tw.EnsureLabels(t.Context(), []string{"conformance-x", "conformance-y"})
	must(t, "EnsureLabels", err)
	if len(first) != 2 || first[0] == "" || first[1] == "" || first[0] == first[1] {
		t.Fatalf("EnsureLabels = %q, want two distinct ids", first)
	}
	again, err := tw.EnsureLabels(t.Context(), []string{"conformance-y", "conformance-x"})
	must(t, "EnsureLabels again", err)
	if !slices.Equal(again, []string{first[1], first[0]}) {
		t.Errorf("EnsureLabels again = %q, want the same ids %q in the new order", again, []string{first[1], first[0]})
	}
}

// concurrentRound is one round of testConcurrent.
func concurrentRound(ctx context.Context, r platform.Reader, repo platform.Repo, number int64, writer platform.Account) error {
	got, err := r.Repo(ctx, repo.Path)
	if err != nil {
		return fmt.Errorf("concurrent Repo: %w", err)
	}
	if got.ID != repo.ID {
		return fmt.Errorf("concurrent Repo: id %s, want %s", got.ID, repo.ID)
	}
	f, err := r.ReadFile(ctx, repo, "", readme[0].Path, 64<<10)
	if err != nil {
		return fmt.Errorf("concurrent ReadFile: %w", err)
	}
	if !bytes.Equal(f.Content, readme[0].Content) {
		return fmt.Errorf("concurrent ReadFile: content %q", f.Content)
	}
	prs, err := r.PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	if err != nil {
		return fmt.Errorf("concurrent PRs: %w", err)
	}
	if len(prs) != 1 || prs[0].Number != number || prs[0].Body != markerBody {
		return fmt.Errorf("concurrent PRs: %v, want #%d", numbers(prs), number)
	}
	if _, err := r.Self(ctx); err != nil {
		return fmt.Errorf("concurrent Self: %w", err)
	}
	return nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
