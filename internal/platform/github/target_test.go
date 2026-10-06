package github

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// apiRepoTarget is the target repository of the writer tests.
var apiRepoTarget = platform.Repo{Host: "github.com", ID: "101", Path: "acme/api", DefaultBranch: "main"}

// repoInstallation declares GET /repos/acme/api/installation with perms.
func (f *fixture) repoInstallation(perms map[string]string) {
	f.handle(http.MethodGet, "/repos/acme/api/installation", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, installation(instAcme, "acme", "Organization", perms))
	}))
}

// TestTargetApp: a token for the repository only, with the permissions
// asked for, workflows only when needed; Close revokes it, and the target
// is dead afterwards.
func TestTargetApp(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.repoInstallation(nil)
	for _, tc := range []struct {
		need platform.Perms
		want map[string]string
	}{
		{platform.Perms{Contents: true, PRs: true}, map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}},
		{platform.Perms{PRs: true}, map[string]string{"contents": "read", "pull_requests": "write", "metadata": "read"}},
		{platform.Perms{Contents: true, PRs: true, Workflows: true},
			map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read", "workflows": "write"}},
	} {
		tw, err := f.writer.Target(t.Context(), apiRepoTarget, tc.need)
		if err != nil {
			t.Fatal(err)
		}
		h, err := tw.Remote().Header(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		tok := strings.TrimPrefix(string(raw), "x-access-token:")
		m, ok := f.tokenOf("Bearer " + tok)
		if !ok || !slices.Equal(m.repos, []int64{101}) || !mapsEqual(m.perms, tc.want) {
			t.Errorf("need %+v: token for %v with %v", tc.need, m.repos, m.perms)
		}
		if tw.Remote().URL != "https://github.com/acme/api.git" || !f.reg.Contains(tok) {
			t.Errorf("remote %q, registered %v", tw.Remote().URL, f.reg.Contains(tok))
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if revoked := f.revokedTokens(); len(revoked) == 0 || revoked[len(revoked)-1] != tok {
			t.Error("Close did not revoke the token")
		}
		if err := tw.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
		_, err = tw.Remote().Header(t.Context())
		wantClass(t, "git after Close", err, platform.ClassAuth, nil)
		_, err = tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "x"})
		wantClass(t, "CreatePR after Close", err, platform.ClassAuth, nil)
		if _, ok := tw.(platform.Committer); !ok {
			t.Error("an App's target is no Committer")
		}
	}
}

// TestTargetAppRefused: a repository outside the installations is
// ClassNotFound; a permission the installation lacks is ClassPermission
// with its rule, before anything is minted.
func TestTargetAppRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.json(http.MethodGet, "/repos/acme/api/installation", http.StatusNotFound, notFoundBody)
	_, err := f.writer.Target(t.Context(), apiRepoTarget, platform.Perms{Contents: true})
	wantClass(t, "not installed", err, platform.ClassNotFound, platform.ErrNotFound)

	for _, tc := range []struct {
		perms map[string]string
		need  platform.Perms
		rule  string
	}{
		{map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}, platform.Perms{Contents: true, Workflows: true}, "workflows"},
		{map[string]string{"contents": "read", "pull_requests": "write", "metadata": "read"}, platform.Perms{Contents: true}, "contents"},
		{map[string]string{"contents": "write", "pull_requests": "read", "metadata": "read"}, platform.Perms{PRs: true}, "pull-requests"},
	} {
		f.repoInstallation(tc.perms)
		_, err := f.writer.Target(t.Context(), apiRepoTarget, tc.need)
		wantClass(t, tc.rule, err, platform.ClassPermission, nil)
		if ruleOfErr(err) != tc.rule {
			t.Errorf("rule %q, want %q", ruleOfErr(err), tc.rule)
		}
	}
	if n := f.mintedCount(instAcme); n != 0 {
		t.Errorf("%d tokens minted for refused targets", n)
	}
	_, err = f.writer.Target(t.Context(), platform.Repo{Host: "github.com", Path: "acme/api"}, platform.Perms{})
	wantClass(t, "no id", err, platform.ClassInvalid, nil)
}

// TestTargetToken: a token's role must push; Close only retires the
// target, and a token has no API commits.
func TestTargetToken(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	f.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(101, "acme/api"))
	_, err := f.writer.Target(t.Context(), apiRepoTarget, platform.Perms{Contents: true})
	wantClass(t, "read-only role", err, platform.ClassPermission, nil)
	if ruleOfErr(err) != "contents" {
		t.Errorf("rule %q", ruleOfErr(err))
	}
	f.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(101, "acme/api",
		withField("permissions", map[string]any{"admin": false, "maintain": false, "push": true, "triage": true, "pull": true})))
	tw, err := f.writer.Target(t.Context(), apiRepoTarget, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tw.(platform.Committer); ok {
		t.Error("a token's target is a Committer")
	}
	if err := tw.Close(); err != nil || len(f.requests(http.MethodDelete, "")) != 0 {
		t.Errorf("Close of a token: %v", err)
	}
	f.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(102, "acme/api"))
	_, err = f.writer.Target(t.Context(), apiRepoTarget, platform.Perms{})
	wantClass(t, "replaced repository", err, platform.ClassNotFound, platform.ErrNotFound)
}

// TestTargetTokenRenewal: a per-target token is minted again 10 minutes
// before it expires, and the old one revoked.
func TestTargetTokenRenewal(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.repoInstallation(nil)
	tw, err := f.writer.Target(t.Context(), apiRepoTarget, platform.Perms{Contents: true})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := tw.Remote().Header(t.Context())
	f.clock.add(51 * time.Minute)
	second, err := tw.Remote().Header(t.Context())
	if err != nil || second == first {
		t.Fatalf("not renewed: %v", err)
	}
	if revoked := f.revokedTokens(); len(revoked) != 1 {
		t.Errorf("revoked %d", len(revoked))
	}
	_ = tw.Close()
}

// newTarget returns an App's target of acme/api with contents and pull
// requests.
func newTarget(t *testing.T, f *fixture) platform.TargetWriter {
	t.Helper()
	f.repoInstallation(nil)
	tw, err := f.writer.Target(t.Context(), apiRepoTarget, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tw.Close() })
	return tw
}

// labelRoutes declares a repository's labels: existing ones answer GET,
// others 404 and are created by POST.
func (f *fixture) labelRoutes(existing ...string) {
	for _, l := range existing {
		f.json(http.MethodGet, "/repos/acme/api/labels/"+l, http.StatusOK, map[string]any{"name": l, "color": "ededed"})
	}
	f.handle(http.MethodPost, "/repos/acme/api/labels", func(w http.ResponseWriter, r *http.Request) {
		var l createLabel
		_ = json.NewDecoder(r.Body).Decode(&l)
		f.json(http.MethodGet, "/repos/acme/api/labels/"+l.Name, http.StatusOK, map[string]any{"name": l.Name, "color": l.Color})
		writeJSON(w, http.StatusCreated, map[string]any{"name": l.Name, "color": l.Color})
	})
}

// TestCreatePR: the pull request from the bare head, a draft falling back
// to ready once, the labels created and put on it.
func TestCreatePR(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	tw := newTarget(t, f)
	base := repo(101, "acme/api")
	f.pages("/repos/acme/api/pulls", nil)
	f.labelRoutes("engineering-assets")
	f.json(http.MethodGet, "/repos/acme/api/labels/sync", http.StatusNotFound, notFoundBody)
	var posted []createPR
	f.handle(http.MethodPost, "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		var body createPR
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body)
		if body.Draft {
			writeJSON(w, http.StatusUnprocessableEntity, ghError("Validation Failed",
				map[string]any{"resource": "PullRequest", "code": "custom", "message": "Draft pull requests are not supported in this repository."}))
			return
		}
		writeJSON(w, http.StatusCreated, pr(base, prSpec{number: 12, author: botUser, head: body.Head, title: body.Title, body: &body.Body}))
	})
	f.json(http.MethodPost, "/repos/acme/api/issues/12/labels", http.StatusOK, []any{
		map[string]any{"name": "engineering-assets"}, map[string]any{"name": "sync"}})
	got, err := tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "chore: sync", Body: "body",
		Labels: []string{"engineering-assets", "sync"}, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 2 || !posted[0].Draft || posted[1].Draft || posted[1].Head != syncBranch || posted[1].Base != "main" || posted[1].Body != "body" {
		t.Errorf("posted %+v", posted)
	}
	if got.Number != 12 || got.Draft || !slices.Equal(got.Labels, []string{"engineering-assets", "sync"}) || got.Body != "body" {
		t.Errorf("CreatePR = %+v", got)
	}
	if c := f.requests(http.MethodPost, "/repos/acme/api/labels"); len(c) != 1 || !strings.Contains(c[0].Body, `"name":"sync"`) {
		t.Errorf("created labels %+v", c)
	}
	if c := f.requests(http.MethodPost, "/repos/acme/api/issues/12/labels"); len(c) != 1 || c[0].Body != `{"labels":["engineering-assets","sync"]}` {
		t.Errorf("added labels %+v", c)
	}
	for _, c := range f.requests(http.MethodGet, "/repos/acme/api/pulls") {
		if c.Query.Get("head") != "acme:"+syncBranch || c.Query.Get("state") != "open" {
			t.Errorf("open pull requests asked with %v", c.Query)
		}
	}
	// A repository with pull requests off, and wrong requests.
	off := platform.NewPR{Head: syncBranch, Base: "main", Title: "t"}
	for _, np := range []platform.NewPR{{Base: "main", Title: "t"}, {Head: "main", Base: "main", Title: "t"}, {Head: syncBranch, Base: "main"},
		{Head: syncBranch, Base: "main", Title: "t", Labels: []string{" "}}} {
		_, err := tw.CreatePR(t.Context(), np)
		wantClass(t, "invalid", err, platform.ClassInvalid, nil)
	}
	f.repoInstallation(nil)
	r := apiRepoTarget
	r.PRsDisabled = true
	tw2, err := f.writer.Target(t.Context(), r, platform.Perms{PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tw2.Close()
	_, err = tw2.CreatePR(t.Context(), off)
	wantClass(t, "pull requests off", err, platform.ClassPolicy, nil)
}

// TestCreatePRExists: an open pull request from the head, to any base, is
// returned with ErrExists before anything is written, and the 422 of one
// opened meanwhile gives the same; a fork's branch of that name is not
// the head.
func TestCreatePRExists(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	tw := newTarget(t, f)
	base := repo(101, "acme/api")
	fork := repo(999, "acme/api-fork", withField("fork", true))
	f.pages("/repos/acme/api/pulls", []any{
		pr(base, prSpec{number: 7, author: alice, head: syncBranch, headRepo: fork}),
		pr(base, prSpec{number: 5, author: botUser, head: syncBranch, base: "release"}),
	})
	got, err := tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "t"})
	if !errors.Is(err, platform.ErrExists) || got.Number != 5 || platform.ClassOf(err) != platform.ClassConflict {
		t.Errorf("CreatePR = #%d, %v", got.Number, err)
	}
	if n := len(f.requests(http.MethodPost, "/repos/acme/api/pulls")); n != 0 {
		t.Errorf("%d pull requests posted", n)
	}

	// Opened meanwhile: the list is empty first, then has it.
	calls := 0
	f.handle(http.MethodGet, "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		calls++
		var items []any
		if calls > 1 {
			items = []any{pr(base, prSpec{number: 8, author: botUser, head: syncBranch})}
		}
		servePage(w, r, items, nil)
	})
	f.json(http.MethodPost, "/repos/acme/api/pulls", http.StatusUnprocessableEntity, ghError("Validation Failed",
		map[string]any{"resource": "PullRequest", "code": "custom", "message": "A pull request already exists for acme:touchmark/acme."}))
	got, err = tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "t"})
	if !errors.Is(err, platform.ErrExists) || got.Number != 8 {
		t.Errorf("after a 422: #%d, %v", got.Number, err)
	}
}

// TestEditPR: the state and base go first, alone; labels are added; title
// and body follow in a PATCH of their own; a refused state writes nothing
// else.
func TestEditPR(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	tw := newTarget(t, f)
	base := repo(101, "acme/api")
	f.labelRoutes("engineering-assets")
	var patches []string
	state := "open"
	f.handle(http.MethodPatch, "/repos/acme/api/pulls/5", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		patches = append(patches, string(raw))
		if s, ok := body["state"].(string); ok {
			state = s
		}
		writeJSON(w, http.StatusOK, pr(base, prSpec{number: 5, author: botUser, head: syncBranch, state: state, body: ptr("new")}))
	})
	f.json(http.MethodPost, "/repos/acme/api/issues/5/labels", http.StatusOK, []any{map[string]any{"name": "engineering-assets"}})
	f.json(http.MethodGet, "/repos/acme/api/branches/main", http.StatusOK, map[string]any{"name": "main"})
	f.closersRoute(t, map[string]map[string]any{"PR_5": closedBy("PR_5", nil, gqlActorOf("Bot", "touchmark-write[bot]", 5001))})
	closed := platform.Closed
	got, err := tw.EditPR(t.Context(), 5, platform.PREdit{Title: ptr("t"), Body: ptr("new"), State: &closed, AddLabels: []string{"engineering-assets"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"state":"closed"}`, `{"body":"new","title":"t"}`}; !slices.Equal(patches, want) {
		t.Errorf("patches %q, want %q", patches, want)
	}
	if got.State != platform.Closed || got.ClosedBy == nil || got.ClosedBy.ID != "5001" || !got.BaseExists {
		t.Errorf("EditPR = %+v", got)
	}
	var order []string
	for _, c := range f.requests("", "") {
		if c.Method == http.MethodPatch || c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/labels") {
			order = append(order, c.Method+" "+c.Path)
		}
	}
	if want := []string{"PATCH /repos/acme/api/pulls/5", "POST /repos/acme/api/issues/5/labels", "PATCH /repos/acme/api/pulls/5"}; !slices.Equal(order, want) {
		t.Errorf("order %q", order)
	}

	// A refused reopen writes nothing else.
	patches = nil
	f.json(http.MethodPatch, "/repos/acme/api/pulls/6", http.StatusUnprocessableEntity, ghError("Validation Failed",
		map[string]any{"resource": "PullRequest", "code": "custom", "message": "state cannot be changed. The touchmark/acme branch has been deleted."}))
	open := platform.Open
	_, err = tw.EditPR(t.Context(), 6, platform.PREdit{Body: ptr("x"), State: &open})
	wantClass(t, "refused reopen", err, platform.ClassInvalid, nil)
	if n := len(f.requests(http.MethodPatch, "/repos/acme/api/pulls/6")); n != 1 {
		t.Errorf("%d PATCHes after a refusal", n)
	}
	// A base that does not exist.
	f.json(http.MethodPatch, "/repos/acme/api/pulls/7", http.StatusUnprocessableEntity, ghError("Validation Failed",
		map[string]any{"resource": "PullRequest", "field": "base", "code": "invalid", "message": "Proposed base branch 'gone' was not found"}))
	_, err = tw.EditPR(t.Context(), 7, platform.PREdit{Base: ptr("gone")})
	wantClass(t, "missing base", err, platform.ClassInvalid, nil)

	merged := platform.Merged
	for _, e := range []platform.PREdit{{State: &merged}, {Title: ptr(" ")}, {Base: ptr("")}, {AddLabels: []string{""}}} {
		_, err := tw.EditPR(t.Context(), 5, e)
		wantClass(t, "invalid edit", err, platform.ClassInvalid, nil)
	}
	_, err = tw.EditPR(t.Context(), 0, platform.PREdit{})
	wantClass(t, "no number", err, platform.ClassNotFound, platform.ErrNotFound)
}

// TestCommentAndLabels: a comment through the issues API; labels created
// when missing, a label created meanwhile (422 already_exists) is fine.
func TestCommentAndLabels(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	tw := newTarget(t, f)
	f.json(http.MethodPost, "/repos/acme/api/issues/5/comments", http.StatusCreated, map[string]any{"id": 1, "body": "hi"})
	if err := tw.Comment(t.Context(), 5, "hi"); err != nil {
		t.Fatal(err)
	}
	if c := f.requests(http.MethodPost, "/repos/acme/api/issues/5/comments"); len(c) != 1 || c[0].Body != `{"body":"hi"}` {
		t.Errorf("comment %+v", c)
	}
	f.json(http.MethodPost, "/repos/acme/api/issues/6/comments", http.StatusNotFound, notFoundBody)
	wantClass(t, "missing", tw.Comment(t.Context(), 6, "hi"), platform.ClassNotFound, platform.ErrNotFound)
	wantClass(t, "empty", tw.Comment(t.Context(), 5, " "), platform.ClassInvalid, nil)

	f.json(http.MethodGet, "/repos/acme/api/labels/Engineering-Assets", http.StatusOK, map[string]any{"name": "engineering-assets"})
	gets := 0
	f.handle(http.MethodGet, "/repos/acme/api/labels/raced", func(w http.ResponseWriter, _ *http.Request) {
		gets++
		if gets == 1 {
			writeJSON(w, http.StatusNotFound, notFoundBody)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": "raced"})
	})
	f.json(http.MethodPost, "/repos/acme/api/labels", http.StatusUnprocessableEntity, ghError("Validation Failed",
		map[string]any{"resource": "Label", "field": "name", "code": "already_exists"}))
	names, err := tw.EnsureLabels(t.Context(), []string{"Engineering-Assets", "raced"})
	if err != nil || !slices.Equal(names, []string{"engineering-assets", "raced"}) {
		t.Errorf("EnsureLabels = %v, %v", names, err)
	}
}
