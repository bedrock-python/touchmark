package bitbucketdc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// writerFixture is a writer with the server's token over one test server
// that takes the writes its routes declare. The token is the writer's.
type writerFixture struct {
	*apiServer
	writer *writer
}

// prsRoute is the route of ACME/api's pull requests.
var prsRoute = repoRoute("ACME", "api") + "/pull-requests"

func newWriterFixture(t *testing.T) *writerFixture {
	t.Helper()
	s := newAPIServer(t)
	s.allowWrites = true
	s.username = "touchmark.writer"
	w, err := NewWriter(s.provider(), auth.Credential{Kind: auth.Token, Token: s.token}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	s.json("/application-properties", http.StatusOK, properties("9.4.0"))
	s.pages("/users", 1000, []any{writerUser})
	s.json(repoRoute("ACME", "api"), http.StatusOK, repo(repoID, "ACME", "api"))
	s.defBranch("ACME", "api", "main", mainSHA)
	return &writerFixture{apiServer: s, writer: w.(*writer)}
}

// perms declares the repositories named api the writer has each permission
// on: ACME/api for the permissions given, another project's otherwise.
func (f *writerFixture) perms(on ...string) {
	f.handle(http.MethodGet, "/repos", func(w http.ResponseWriter, r *http.Request) {
		items := []any{repo(901, "ACMEX", "api")}
		for _, p := range on {
			if r.URL.Query().Get("permission") == p {
				items = append(items, repo(repoID, "ACME", "api"))
			}
		}
		servePage(w, r, 1000, items)
	})
}

// target returns the TargetWriter of ACME/api, the writer having write
// permission, with the recorded calls forgotten.
func (f *writerFixture) target(t *testing.T) *target {
	t.Helper()
	f.perms(permWrite)
	tw, err := f.writer.Target(t.Context(), platform.Repo{Host: f.provider().Host, ID: "101", Path: "ACME/api"}, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	f.reset()
	return tw.(*target)
}

// body decodes the JSON body of a recorded request.
func body(t *testing.T, c apiCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("%s %s: body %q: %v", c.Method, c.Path, c.Body, err)
	}
	return m
}

// writeLog returns the writes recorded, as "METHOD path".
func (s *apiServer) writeLog() []string {
	var out []string
	for _, c := range s.writes() {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func TestTarget(t *testing.T) {
	f := newWriterFixture(t)
	r := platform.Repo{Host: f.provider().Host, ID: "101", Path: "ACME/api"}
	f.perms(permWrite)
	tw, err := f.writer.Target(t.Context(), r, platform.Perms{Contents: true, PRs: true, Workflows: true})
	if err != nil {
		t.Fatal(err)
	}
	q := f.requests(http.MethodGet, "/repos")[0].Query
	if q.Get("projectkey") != "ACME" || q.Get("name") != "api" || q.Get("permission") != "REPO_WRITE" || q.Get("archived") != "ALL" {
		t.Errorf("the permission query %v", q)
	}
	rem := tw.Remote()
	if rem.URL != f.base()+"/scm/acme/api.git" {
		t.Errorf("remote %q", rem.URL)
	}
	if h, err := rem.Header(t.Context()); err != nil || h != "Bearer "+f.token {
		t.Errorf("the git header is wrong: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = rem.Header(t.Context())
	wantClass(t, "git credentials after Close", err, platform.ClassAuth, nil)
	_, err = tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "x"})
	wantClass(t, "CreatePR after Close", err, platform.ClassAuth, nil)

	// Read permission only.
	f.perms()
	_, err = f.writer.Target(t.Context(), r, platform.Perms{Contents: true, PRs: true})
	wantClass(t, "a reader", err, platform.ClassPermission, nil)
	var pe *platform.Error
	if errors.As(err, &pe) && pe.Rule != "contents" {
		t.Errorf("rule %q, want contents", pe.Rule)
	}
	// Pull requests alone need read permission only.
	if _, err := f.writer.Target(t.Context(), r, platform.Perms{PRs: true}); err != nil {
		t.Errorf("pull requests alone: %v", err)
	}

	// Another repository took the path.
	_, err = f.writer.Target(t.Context(), platform.Repo{Host: r.Host, ID: "55", Path: "ACME/api"}, platform.Perms{Contents: true})
	wantClass(t, "a replaced repository", err, platform.ClassNotFound, platform.ErrNotFound)
	// A repository the writer does not see.
	f.json(repoRoute("ACME", "secret"), http.StatusNotFound, errorBody(noRepoException, "Repository ACME/secret does not exist."))
	_, err = f.writer.Target(t.Context(), platform.Repo{Host: r.Host, ID: "7", Path: "ACME/secret"}, platform.Perms{Contents: true})
	wantClass(t, "a hidden repository", err, platform.ClassNotFound, nil)
}

func TestCreatePR(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	f.pages(prsRoute, 1000, nil)
	f.handle(http.MethodPost, prsRoute, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, pr(prSpec{id: 12, source: syncBranch, author: writerUser, body: "b", draft: true, version: 0}))
	})
	got, err := tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "Sync", Body: "b", Labels: []string{"x"}, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 12 || !got.Draft || got.HeadSHA != headSHA || !got.BaseExists || got.Body != "b" {
		t.Errorf("CreatePR = %+v", got)
	}
	posts := f.requests(http.MethodPost, prsRoute)
	if len(posts) != 1 {
		t.Fatalf("%d POSTs", len(posts))
	}
	b := body(t, posts[0])
	want := `map[description:b draft:true fromRef:map[id:refs/heads/touchmark/acme-eng repository:map[project:map[key:ACME] slug:api]] title:Sync toRef:map[id:refs/heads/main repository:map[project:map[key:ACME] slug:api]]]`
	if fmt.Sprint(b) != want {
		t.Errorf("POST body %v\nwant %s", b, want)
	}
	if _, ok := b["reviewers"]; ok {
		t.Error("reviewers were sent")
	}
}

func TestCreatePRExisting(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	// Open already, before anything is written: the one to the base first.
	f.pages(prsRoute, 1000, []any{
		pr(prSpec{id: 7, source: syncBranch, author: writerUser, target: "develop"}),
		pr(prSpec{id: 6, source: syncBranch, author: writerUser}),
		pr(prSpec{id: 5, source: syncBranch, author: writerUser, sourceRepo: repoRef(forkID, "ACME", "fork")}),
	})
	got, err := tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "Sync"})
	if !errors.Is(err, platform.ErrExists) || got.Number != 6 {
		t.Errorf("CreatePR = #%d, %v; want #6 with ErrExists", got.Number, err)
	}
	if n := len(f.writes()); n != 0 {
		t.Errorf("%d writes", n)
	}

	// Opened meanwhile: the POST is refused with 409.
	f = newWriterFixture(t)
	tw = f.target(t)
	listed := 0
	f.handle(http.MethodGet, prsRoute, func(w http.ResponseWriter, r *http.Request) {
		listed++
		var items []any
		if listed > 1 {
			items = append(items, pr(prSpec{id: 8, source: syncBranch, author: writerUser}))
		}
		servePage(w, r, 1000, items)
	})
	f.handle(http.MethodPost, prsRoute, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, errorBody("com.atlassian.bitbucket.pull.DuplicatePullRequestException", "Only one pull request may be open for a given source and target branch"))
	})
	got, err = tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "Sync"})
	if !errors.Is(err, platform.ErrExists) || got.Number != 8 {
		t.Errorf("a 409 = #%d, %v; want #8 with ErrExists", got.Number, err)
	}

	// Refused otherwise: up to date, so nothing to propose.
	f = newWriterFixture(t)
	tw = f.target(t)
	f.pages(prsRoute, 1000, nil)
	f.handle(http.MethodPost, prsRoute, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, errorBody("com.atlassian.bitbucket.pull.EmptyPullRequestException", "The pull request has no changes"))
	})
	_, err = tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "Sync"})
	wantClass(t, "a refused POST", err, platform.ClassConflict, nil)
	if errors.Is(err, platform.ErrExists) {
		t.Error("a refusal without an open pull request is ErrExists")
	}
}

func TestCreatePRDraftsOff(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	f.pages(prsRoute, 1000, nil)
	f.handle(http.MethodPost, prsRoute, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["draft"] == true {
			writeJSON(w, http.StatusBadRequest, errorBody("", "Draft pull requests are disabled"))
			return
		}
		writeJSON(w, http.StatusCreated, pr(prSpec{id: 3, source: syncBranch, author: writerUser}))
	})
	got, err := tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "Sync", Draft: true})
	if err != nil || got.Draft || len(f.requests(http.MethodPost, prsRoute)) != 2 {
		t.Errorf("drafts off = %+v, %v", got, err)
	}
	for _, np := range []platform.NewPR{{Base: "main", Title: "x"}, {Head: "a", Base: "a", Title: "x"}, {Head: "a", Base: "main", Title: " "}} {
		_, err := tw.CreatePR(t.Context(), np)
		wantClass(t, fmt.Sprintf("CreatePR(%+v)", np), err, platform.ClassInvalid, nil)
	}
}

func TestEditPR(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	reviewer := user(personID, "Jane.Doe", "jane.doe", "NORMAL")
	cur := pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "old", version: 4})
	cur["reviewers"] = []any{map[string]any{"user": reviewer, "role": "REVIEWER", "approved": false, "status": "UNAPPROVED"}}
	f.json(prsRoute+"/9", http.StatusOK, cur)
	f.handle(http.MethodPut, prsRoute+"/9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "new", version: 5}))
	})
	f.handle(http.MethodPost, prsRoute+"/9/decline", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "new", version: 6, state: "DECLINED"}))
	})
	newBody, closed := "new", platform.Closed
	got, err := tw.EditPR(t.Context(), 9, platform.PREdit{Body: &newBody, State: &closed, AddLabels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != platform.Closed || got.Body != "new" || got.HeadSHA != "" {
		t.Errorf("EditPR = %+v", got)
	}
	if fmt.Sprint(f.writeLog()) != fmt.Sprintf("[PUT %s/9 POST %s/9/decline]", prsRoute, prsRoute) {
		t.Errorf("writes %v", f.writeLog())
	}
	put := body(t, f.requests(http.MethodPut, prsRoute+"/9")[0])
	if put["version"] != float64(4) || put["description"] != "new" || put["title"] != "sync" || put["draft"] != false || put["toRef"] != nil {
		t.Errorf("PUT body %v", put)
	}
	if fmt.Sprint(put["reviewers"]) != "[map[user:map[name:Jane.Doe]]]" {
		t.Errorf("reviewers sent back %v", put["reviewers"])
	}
	decline := f.requests(http.MethodPost, prsRoute+"/9/decline")[0]
	if decline.Query.Get("version") != "5" || body(t, decline)["version"] != float64(5) {
		t.Errorf("decline with %v %s, want the PUT's version 5", decline.Query, decline.Body)
	}

	// Nothing changes: no write.
	f.reset()
	f.json(prsRoute+"/9", http.StatusOK, pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "new", version: 5}))
	if _, err := tw.EditPR(t.Context(), 9, platform.PREdit{Body: &newBody}); err != nil || len(f.writes()) != 0 {
		t.Errorf("an edit that changes nothing: %v, writes %v", err, f.writeLog())
	}

	// A new base.
	base := "develop"
	f.pages(repoRoute("ACME", "api")+"/branches", 1000, []any{branch("develop", mainSHA, false)})
	if _, err := tw.EditPR(t.Context(), 9, platform.PREdit{Base: &base}); err != nil {
		t.Fatal(err)
	}
	put = body(t, f.requests(http.MethodPut, prsRoute+"/9")[0])
	if fmt.Sprint(put["toRef"]) != "map[id:refs/heads/develop repository:map[project:map[key:ACME] slug:api]]" {
		t.Errorf("toRef %v", put["toRef"])
	}
}

func TestEditPRStale(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	reads := 0
	f.handle(http.MethodGet, prsRoute+"/9", func(w http.ResponseWriter, _ *http.Request) {
		reads++
		writeJSON(w, http.StatusOK, pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "old", version: int64(reads)}))
	})
	puts := 0
	f.handle(http.MethodPut, prsRoute+"/9", func(w http.ResponseWriter, _ *http.Request) {
		puts++
		if puts == 1 {
			writeJSON(w, http.StatusConflict, errorBody("com.atlassian.bitbucket.pull.PullRequestOutOfDateException", "You are attempting to modify a pull request based on out-of-date information."))
			return
		}
		writeJSON(w, http.StatusOK, pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "new", version: 9}))
	})
	newBody := "new"
	if _, err := tw.EditPR(t.Context(), 9, platform.PREdit{Body: &newBody}); err != nil {
		t.Fatal(err)
	}
	if v := body(t, f.requests(http.MethodPut, prsRoute+"/9")[1])["version"]; v != float64(2) {
		t.Errorf("the second PUT names version %v, want the one read again", v)
	}
	// Stale twice: the conflict stands.
	puts = 0
	f.handle(http.MethodPut, prsRoute+"/9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, errorBody("com.atlassian.bitbucket.pull.PullRequestOutOfDateException", "out of date"))
	})
	_, err := tw.EditPR(t.Context(), 9, platform.PREdit{Body: &newBody})
	wantClass(t, "stale twice", err, platform.ClassConflict, nil)
}

func TestEditPRClosed(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	newBody, open, closed, merged := "new", platform.Open, platform.Closed, platform.Merged
	f.json(prsRoute+"/4", http.StatusOK, pr(prSpec{id: 4, source: syncBranch, author: writerUser, state: "DECLINED", body: "old"}))
	if got, err := tw.EditPR(t.Context(), 4, platform.PREdit{State: &closed}); err != nil || got.State != platform.Closed {
		t.Errorf("closing a declined pull request = %+v, %v", got, err)
	}
	_, err := tw.EditPR(t.Context(), 4, platform.PREdit{State: &open})
	wantClass(t, "reopening", err, platform.ClassUnsupported, nil)
	_, err = tw.EditPR(t.Context(), 4, platform.PREdit{Body: &newBody})
	wantClass(t, "editing a declined pull request", err, platform.ClassUnsupported, nil)
	f.json(prsRoute+"/3", http.StatusOK, pr(prSpec{id: 3, source: syncBranch, author: writerUser, state: "MERGED", body: "old"}))
	_, err = tw.EditPR(t.Context(), 3, platform.PREdit{Body: &newBody})
	wantClass(t, "editing a merged pull request", err, platform.ClassConflict, nil)
	_, err = tw.EditPR(t.Context(), 3, platform.PREdit{State: &merged})
	wantClass(t, "merging", err, platform.ClassUnsupported, nil)
	if n := len(f.writes()); n != 0 {
		t.Errorf("%d writes to closed pull requests", n)
	}
}

func TestComment(t *testing.T) {
	f := newWriterFixture(t)
	tw := f.target(t)
	f.handle(http.MethodPost, prsRoute+"/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": 1, "version": 0, "text": "hello"})
	})
	if err := tw.Comment(t.Context(), 9, "hello"); err != nil {
		t.Fatal(err)
	}
	if b := body(t, f.requests(http.MethodPost, prsRoute+"/9/comments")[0]); fmt.Sprint(b) != "map[text:hello]" {
		t.Errorf("comment body %v", b)
	}
	wantClass(t, "an empty comment", tw.Comment(t.Context(), 9, " "), platform.ClassInvalid, nil)
	if ids, err := tw.EnsureLabels(t.Context(), []string{"x"}); err != nil || ids != nil {
		t.Errorf("EnsureLabels = %v, %v", ids, err)
	}
}

func TestCheck(t *testing.T) {
	f := newWriterFixture(t)
	r := platform.Repo{Host: f.provider().Host, ID: "101", Path: "ACME/api"}
	ids, err := f.writer.Check(t.Context(), nil, nil)
	if err != nil || len(ids) != 3 {
		t.Fatalf("identity checks = %+v, %v", ids, err)
	}
	for _, c := range ids {
		if c.Status != platform.FindingUnknown {
			t.Errorf("%s is %s, want unknown", c.Check, c.Status)
		}
	}
	for _, tc := range []struct {
		perms []string
		want  platform.FindingStatus
	}{
		{[]string{permWrite}, platform.FindingOK},
		{[]string{permWrite, permAdmin}, platform.FindingWarn},
		{nil, platform.FindingFail},
	} {
		f.perms(tc.perms...)
		got, err := f.writer.Check(t.Context(), []platform.Repo{r}, nil)
		if err != nil || len(got) != 2 || got[0].Check != "access" || got[0].Status != tc.want || got[1].Check != "rules" || got[1].Status != platform.FindingUnknown {
			t.Errorf("perms %v: %+v, %v", tc.perms, got, err)
		}
	}
	f.json(repoRoute("ACME", "gone"), http.StatusNotFound, errorBody(noRepoException, "gone"))
	got, err := f.writer.Check(t.Context(), []platform.Repo{{Host: r.Host, Path: "ACME/gone"}}, nil)
	if err != nil || len(got) != 1 || got[0].Status != platform.FindingFail {
		t.Errorf("a repository the writer does not see = %+v, %v", got, err)
	}
	if !strings.Contains(got[0].Detail, "does not see") {
		t.Errorf("detail %q", got[0].Detail)
	}
	// A repository hidden behind 401 (with the user's name): fail too.
	f.json(repoRoute("ACME", "hidden"), http.StatusUnauthorized, errorBody(authorisation, "You are not permitted to access this resource"))
	got, err = f.writer.Check(t.Context(), []platform.Repo{{Host: r.Host, Path: "ACME/hidden"}}, nil)
	if err != nil || len(got) != 1 || got[0].Status != platform.FindingFail {
		t.Errorf("a repository refused with 401 = %+v, %v", got, err)
	}
}
