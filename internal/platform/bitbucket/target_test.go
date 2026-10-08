package bitbucket

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// writerFixture is a writer with a token over one test server that takes
// the writes its routes declare. GET /user and /user/emails answer for the
// bot account.
type writerFixture struct {
	*apiServer
	token  string
	writer *writer
}

// botEmail is the bot account's primary confirmed address.
const botEmail = "touchmark-bot@acme.example"

// Paths of acme/api's pull requests and of the writer's permissions in acme.
const (
	prsPath   = "/repositories/acme/api/pullrequests"
	permsPath = "/user/workspaces/acme/permissions/repositories"
)

func newWriterFixture(t *testing.T) *writerFixture {
	t.Helper()
	s := newAPIServer(t)
	s.allowWrites = true
	tok := testToken(t)
	w, err := NewWriter(s.provider(), auth.Credential{Kind: auth.Token, Token: tok}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	s.json("/user", http.StatusOK, account(botUUID, "touchmark bot"))
	s.pages("/user/emails", 10, []any{
		map[string]any{"type": "email", "email": "old@acme.example", "is_primary": false, "is_confirmed": true},
		map[string]any{"type": "email", "email": botEmail, "is_primary": true, "is_confirmed": true},
	})
	return &writerFixture{apiServer: s, token: tok, writer: w.(*writer)}
}

// repoRoute declares GET /repositories/acme/{uuid} of acme/api.
func (f *writerFixture) repoRoute() {
	f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusOK, repo(repoUUID, "acme/api"))
}

// permRoute declares the writer's permission on acme/api ("" for none:
// the listing does not name it).
func (f *writerFixture) permRoute(perm string) {
	var items []any
	if perm != "" {
		items = append(items, map[string]any{"type": "repository_permission", "permission": perm,
			"user": account(botUUID, "touchmark bot"), "repository": repoRef(repoUUID, "acme/api")})
	}
	f.pages(permsPath, 10, items)
}

// target returns the TargetWriter of acme/api, the writer having write
// permission, with the recorded calls forgotten.
func (f *writerFixture) target(t *testing.T) *target {
	t.Helper()
	f.repoRoute()
	f.permRoute(permWrite)
	tw, err := f.writer.Target(t.Context(), apiRepoFixture, platform.Perms{Contents: true, PRs: true})
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
		out = append(out, c.Method+" "+strings.TrimPrefix(c.Path, "/2.0"))
	}
	return out
}

func TestWriterSelfEmail(t *testing.T) {
	f := newWriterFixture(t)
	a, err := f.writer.Self(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a != (platform.Account{ID: botUUID, Login: botUUID, Email: botEmail, Kind: platform.KindUser}) {
		t.Errorf("Self = %+v; want the primary confirmed address", a)
	}
	for name, tc := range map[string]struct {
		status int
		items  []any
		want   string
		class  platform.Class
	}{
		"unconfirmed primary": {http.StatusOK, []any{map[string]any{"email": "x@acme.example", "is_primary": true, "is_confirmed": false}}, "", 0},
		"not an address":      {http.StatusOK, []any{map[string]any{"email": "a <b>@c", "is_primary": true, "is_confirmed": true}}, "", 0},
		"forbidden":           {http.StatusForbidden, nil, "", 0},
		"rate limited":        {http.StatusTooManyRequests, nil, "", platform.ClassRateLimited},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			if tc.status == http.StatusOK {
				f.pages("/user/emails", 10, tc.items)
			} else {
				f.json("/user/emails", tc.status, errorBody("no"))
			}
			a, err := f.writer.Self(t.Context())
			if tc.class != 0 {
				wantClass(t, "Self", err, tc.class, nil)
				return
			}
			if err != nil || a.Email != tc.want || a.ID != botUUID {
				t.Errorf("Self = %+v, %v; want email %q", a, err, tc.want)
			}
		})
	}
}

func TestTarget(t *testing.T) {
	for name, tc := range map[string]struct {
		perm  string
		need  platform.Perms
		class platform.Class
		rule  string
	}{
		"write":                 {permWrite, platform.Perms{Contents: true, PRs: true}, 0, ""},
		"admin":                 {permAdmin, platform.Perms{Contents: true, PRs: true, Workflows: true}, 0, ""},
		"read":                  {permRead, platform.Perms{Contents: true, PRs: true}, platform.ClassPermission, "contents"},
		"none":                  {"", platform.Perms{Contents: true}, platform.ClassPermission, "contents"},
		"read, pull requests":   {permRead, platform.Perms{PRs: true}, platform.ClassPermission, "pull-requests"},
		"read, nothing to need": {permRead, platform.Perms{}, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			f.repoRoute()
			f.permRoute(tc.perm)
			tw, err := f.writer.Target(t.Context(), apiRepoFixture, tc.need)
			if tc.class != 0 {
				wantClass(t, "Target", err, tc.class, nil)
				var pe *platform.Error
				if !asError(err, &pe) || pe.Rule != tc.rule || pe.Status != http.StatusForbidden {
					t.Errorf("Target = %v; want rule %q", err, tc.rule)
				}
				return
			}
			if err != nil || tw == nil {
				t.Fatalf("Target = %v, %v", tw, err)
			}
			calls := f.requests(http.MethodGet, permsPath)
			if len(calls) != 1 || calls[0].Query.Get("q") != `repository.uuid="`+repoUUID+`"` {
				t.Errorf("permission requests %+v; want one filtered by the uuid", calls)
			}
		})
	}
}

func TestTargetErrors(t *testing.T) {
	t.Run("hidden repository", func(t *testing.T) {
		f := newWriterFixture(t)
		f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusNotFound, errorBody(noRepoMessage))
		_, err := f.writer.Target(t.Context(), apiRepoFixture, platform.Perms{Contents: true})
		wantClass(t, "Target", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("another repository under the path", func(t *testing.T) {
		f := newWriterFixture(t)
		f.json("/repositories/acme/api", http.StatusOK, repo(otherUUID, "acme/api"))
		r := apiRepoFixture
		r.ID = "42"
		_, err := f.writer.Target(t.Context(), r, platform.Perms{Contents: true})
		wantClass(t, "Target", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("another host", func(t *testing.T) {
		f := newWriterFixture(t)
		r := apiRepoFixture
		r.Host = "bitbucket.example.com"
		_, err := f.writer.Target(t.Context(), r, platform.Perms{Contents: true})
		wantClass(t, "Target", err, platform.ClassInvalid, nil)
	})
	t.Run("refused token", func(t *testing.T) {
		s := newAPIServer(t)
		w, err := NewWriter(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
		if err != nil {
			t.Fatal(err)
		}
		s.json("/user", http.StatusUnauthorized, errorBody("Token is invalid, expired, or not supported for this endpoint."))
		_, err = w.Target(t.Context(), apiRepoFixture, platform.Perms{Contents: true})
		wantClass(t, "Target", err, platform.ClassAuth, nil)
	})
	t.Run("filter refused", func(t *testing.T) {
		f := newWriterFixture(t)
		f.repoRoute()
		var items []any
		for i := range 25 {
			items = append(items, map[string]any{"permission": permRead, "repository": repoRef("{00000000-0000-4000-8000-0000000000"+twoDigits(i)+"}", "acme/r")})
		}
		items = append(items, map[string]any{"permission": permWrite, "repository": repoRef(repoUUID, "acme/api")})
		f.handle(http.MethodGet, permsPath, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("q") != "" {
				writeJSON(w, http.StatusBadRequest, errorBody(`Field "repository.uuid" does not support filtering`))
				return
			}
			servePage(w, r, 10, items)
		})
		if _, err := f.writer.Target(t.Context(), apiRepoFixture, platform.Perms{Contents: true}); err != nil {
			t.Fatalf("Target: %v", err)
		}
		if n := len(f.requests(http.MethodGet, permsPath)); n != 4 {
			t.Errorf("%d permission requests; want the refused filter and three pages", n)
		}
	})
	t.Run("permission listing fails", func(t *testing.T) {
		f := newWriterFixture(t)
		f.repoRoute()
		f.json(permsPath, http.StatusForbidden, errorBody("The requesting user does not have access to the workspace."))
		_, err := f.writer.Target(t.Context(), apiRepoFixture, platform.Perms{Contents: true})
		wantClass(t, "Target", err, platform.ClassPermission, nil)
	})
}

// twoDigits formats i with two digits.
func twoDigits(i int) string { return string([]byte{'0' + byte(i/10), '0' + byte(i%10)}) }

// asError is errors.As for *platform.Error.
func asError(err error, pe **platform.Error) bool { return errors.As(err, pe) }

func TestTargetRemote(t *testing.T) {
	f := newWriterFixture(t)
	tg := f.target(t)
	rem := tg.Remote()
	if rem.URL != f.base()+"/acme/api.git" {
		t.Errorf("URL %q", rem.URL)
	}
	h, err := rem.Header(t.Context())
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-bitbucket-api-token-auth:"+f.token))
	if err != nil || h != want {
		t.Errorf("Header = %q, %v; want the API token's Basic credentials", h, err)
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tg.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	_, err = rem.Header(t.Context())
	wantClass(t, "Header after Close", err, platform.ClassAuth, nil)
	_, err = tg.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "x"})
	wantClass(t, "CreatePR after Close", err, platform.ClassAuth, nil)
	_, err = tg.EditPR(t.Context(), 1, platform.PREdit{})
	wantClass(t, "EditPR after Close", err, platform.ClassAuth, nil)
	wantClass(t, "Comment after Close", tg.Comment(t.Context(), 1, "x"), platform.ClassAuth, nil)
	_, err = tg.EnsureLabels(t.Context(), nil)
	wantClass(t, "EnsureLabels after Close", err, platform.ClassAuth, nil)
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("%d requests after Close", n)
	}
}

// listing declares GET of acme/api's pull requests (items) and POST, which
// answers created.
func (f *writerFixture) prRoutes(items []any, status int, created any) {
	f.pages(prsPath, 10, items)
	f.handle(http.MethodPost, prsPath, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, created) })
}

func TestCreatePR(t *testing.T) {
	for _, draft := range []bool{false, true} {
		f := newWriterFixture(t)
		tg := f.target(t)
		bot := account(botUUID, "touchmark bot")
		created := pr(prSpec{id: 12, author: bot, source: syncBranch, title: "chore: sync", body: "body\n\n[touchmark]: # \"x\"", draft: draft})
		fork := pr(prSpec{id: 3, author: account(personUUID, "someone"), source: syncBranch, sourceRepo: repoRef(forkUUID, "someone/api")})
		f.prRoutes([]any{fork}, http.StatusCreated, created)
		f.commitRoute("a6dcc740200c")
		got, err := tg.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "chore: sync",
			Body: "body\n\n[touchmark]: # \"x\"", Labels: []string{"engineering-assets"}, Draft: draft})
		if err != nil {
			t.Fatal(err)
		}
		if got.Number != 12 || got.State != platform.Open || got.Draft != draft || got.Head != syncBranch || got.Base != "main" ||
			!got.BaseExists || got.HeadSHA != fullHash("a6dcc740200c") || got.Author.ID != botUUID || len(got.Labels) != 0 ||
			got.Body != "body\n\n[touchmark]: # \"x\"" {
			t.Errorf("CreatePR = %+v", got)
		}
		posts := f.requests(http.MethodPost, prsPath)
		if len(posts) != 1 {
			t.Fatalf("%d POSTs", len(posts))
		}
		b := body(t, posts[0])
		want := map[string]any{
			"title": "chore: sync", "description": "body\n\n[touchmark]: # \"x\"",
			"source":              map[string]any{"branch": map[string]any{"name": syncBranch}},
			"destination":         map[string]any{"branch": map[string]any{"name": "main"}},
			"close_source_branch": false, "draft": draft,
		}
		if js, wantJS := mustJSON(t, b), mustJSON(t, want); js != wantJS {
			t.Errorf("POST body %s\nwant %s", js, wantJS)
		}
		lists := f.requests(http.MethodGet, prsPath)
		if len(lists) != 1 || lists[0].Query.Get("q") != `source.branch.name IN ("`+syncBranch+`") AND state IN ("OPEN")` {
			t.Errorf("listings %+v", lists)
		}
	}
}

// mustJSON returns v as canonical JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCreatePRExists(t *testing.T) {
	bot := account(botUUID, "touchmark bot")
	np := platform.NewPR{Head: syncBranch, Base: "main", Title: "chore: sync", Body: "b"}
	t.Run("open before", func(t *testing.T) {
		f := newWriterFixture(t)
		tg := f.target(t)
		f.pages(prsPath, 10, []any{
			pr(prSpec{id: 7, author: bot, source: syncBranch, target: "release"}),
			pr(prSpec{id: 5, author: bot, source: syncBranch}),
		})
		f.commitRoute("a6dcc7402005")
		got, err := tg.CreatePR(t.Context(), np)
		wantClass(t, "CreatePR", err, platform.ClassConflict, platform.ErrExists)
		if got.Number != 5 {
			t.Errorf("CreatePR returned #%d; want #5, the one to main", got.Number)
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("writes %+v", w)
		}
	})
	t.Run("opened meanwhile", func(t *testing.T) {
		f := newWriterFixture(t)
		tg := f.target(t)
		listed := 0
		f.handle(http.MethodGet, prsPath, func(w http.ResponseWriter, r *http.Request) {
			listed++
			var items []any
			if listed > 1 {
				items = append(items, pr(prSpec{id: 6, author: bot, source: syncBranch}))
			}
			servePage(w, r, 10, items)
		})
		f.handle(http.MethodPost, prsPath, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusBadRequest, errorBody("There are no changes to be pulled"))
		})
		f.commitRoute("a6dcc7402006")
		got, err := tg.CreatePR(t.Context(), np)
		wantClass(t, "CreatePR", err, platform.ClassConflict, platform.ErrExists)
		var pe *platform.Error
		if got.Number != 6 || !asError(err, &pe) || pe.Status != http.StatusBadRequest {
			t.Errorf("CreatePR = #%d, %v", got.Number, err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f := newWriterFixture(t)
		tg := f.target(t)
		f.prRoutes(nil, http.StatusBadRequest, errorBody("source branch not found: "+f.token))
		_, err := tg.CreatePR(t.Context(), np)
		wantClass(t, "CreatePR", err, platform.ClassInvalid, nil)
		if err != nil && strings.Contains(err.Error(), f.token) {
			t.Errorf("the error shows the token: %v", err)
		}
		if n := len(f.requests(http.MethodGet, prsPath)); n != 2 {
			t.Errorf("%d listings; want one before and one after the refusal", n)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		f := newWriterFixture(t)
		tg := f.target(t)
		for _, np := range []platform.NewPR{{Base: "main", Title: "t"}, {Head: "x", Base: "x", Title: "t"}, {Head: "x", Base: "main", Title: " "}} {
			_, err := tg.CreatePR(t.Context(), np)
			wantClass(t, "CreatePR", err, platform.ClassInvalid, nil)
		}
		if n := len(f.requests("", "")); n != 0 {
			t.Errorf("%d requests", n)
		}
	})
}

// editFixture is a TargetWriter over pull request #4 of acme/api, open
// (or in state) from the sync branch, with a reviewer, and the routes of
// its edits: PUT answers the body it was sent applied to the pull request,
// POST …/decline declines it.
type editFixture struct {
	*writerFixture
	tg *target
	// cur is the pull request as the server holds it.
	cur map[string]any
	// putStatus, when set, answers PUTs; putBody is its body.
	putStatus []int
	putBody   any
	// keepOpen makes the decline answer 200 without declining.
	keepOpen bool
}

const editPath = prsPath + "/4"

func newEditFixture(t *testing.T, state string) *editFixture {
	t.Helper()
	wf := newWriterFixture(t)
	f := &editFixture{writerFixture: wf, tg: wf.target(t)}
	f.cur = pr(prSpec{id: 4, state: state, author: account(botUUID, "touchmark bot"), closedBy: account(botUUID, "touchmark bot"),
		source: syncBranch, title: "chore: sync", body: "old body\r\n\r\n[touchmark]: # \"old\""})
	f.cur["reviewers"] = []any{account(personUUID, "Reviewer")}
	f.handle(http.MethodGet, editPath, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, f.cur) })
	f.handle(http.MethodPut, editPath, func(w http.ResponseWriter, r *http.Request) {
		if len(f.putStatus) > 0 {
			status := f.putStatus[0]
			f.putStatus = f.putStatus[1:]
			if status != http.StatusOK {
				writeJSON(w, status, f.putBody)
				return
			}
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("PUT body: %v", err)
		}
		f.cur["title"] = in["title"]
		if d, ok := in["description"].(string); ok {
			f.cur["description"] = d
			f.cur["summary"] = map[string]any{"raw": d, "markup": "markdown", "html": ""}
		}
		if dst, ok := in["destination"].(map[string]any); ok {
			f.cur["destination"].(map[string]any)["branch"] = dst["branch"]
		}
		writeJSON(w, http.StatusOK, f.cur)
	})
	f.handle(http.MethodPost, editPath+"/decline", func(w http.ResponseWriter, _ *http.Request) {
		if !f.keepOpen {
			f.cur["state"] = "DECLINED"
			f.cur["closed_by"] = account(botUUID, "touchmark bot")
		}
		writeJSON(w, http.StatusOK, f.cur)
	})
	f.commitRoute("a6dcc7402004")
	f.branch("release", headCommit)
	return f
}

func ptr[T any](v T) *T { return &v }

func TestEditPRFields(t *testing.T) {
	f := newEditFixture(t, "OPEN")
	got, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Title: ptr("chore: new"), Body: ptr("new body"), Base: ptr("release"),
		AddLabels: []string{"engineering-assets"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "chore: new" || got.Body != "new body" || got.Base != "release" || !got.BaseExists || got.State != platform.Open ||
		got.HeadSHA != fullHash("a6dcc7402004") || len(got.Labels) != 0 {
		t.Errorf("EditPR = %+v", got)
	}
	if w := f.writeLog(); !slices.Equal(w, []string{"PUT " + editPath}) {
		t.Fatalf("writes %q", w)
	}
	b := body(t, f.requests(http.MethodPut, editPath)[0])
	want := map[string]any{
		"title": "chore: new", "description": "new body", "draft": false,
		"destination": map[string]any{"branch": map[string]any{"name": "release"}},
		"reviewers":   []any{map[string]any{"uuid": personUUID}},
	}
	if js, wantJS := mustJSON(t, b), mustJSON(t, want); js != wantJS {
		t.Errorf("PUT body %s\nwant %s", js, wantJS)
	}
}

func TestEditPRKeepsWhatItDoesNotChange(t *testing.T) {
	f := newEditFixture(t, "OPEN")
	f.cur["draft"] = true
	if _, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Body: ptr("new body")}); err != nil {
		t.Fatal(err)
	}
	b := body(t, f.requests(http.MethodPut, editPath)[0])
	if b["title"] != "chore: sync" || b["draft"] != true || b["destination"] != nil || b["description"] != "new body" {
		t.Errorf("PUT body %v; want the title and draft flag kept, no destination", b)
	}
	// Nothing to change: no PUT.
	f.reset()
	if _, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Title: ptr("chore: sync"), Body: ptr("new body"), Base: ptr("main")}); err != nil {
		t.Fatal(err)
	}
	if w := f.writeLog(); len(w) != 0 {
		t.Errorf("an edit that changes nothing wrote %q", w)
	}
}

func TestEditPRClose(t *testing.T) {
	f := newEditFixture(t, "OPEN")
	body := "closed by touchmark\n\n[touchmark]: # \"closed\""
	got, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Body: &body, State: ptr(platform.Closed)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != platform.Closed || got.Body != body || got.ClosedBy == nil || got.ClosedBy.ID != botUUID || got.HeadSHA != "" {
		t.Errorf("EditPR = %+v", got)
	}
	// The body first: a declined pull request can no longer be changed.
	if w := f.writeLog(); !slices.Equal(w, []string{"PUT " + editPath, "POST " + editPath + "/decline"}) {
		t.Errorf("writes %q; want the PUT, then the decline", w)
	}

	// A close alone declines.
	f = newEditFixture(t, "OPEN")
	if _, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{State: ptr(platform.Closed)}); err != nil {
		t.Fatal(err)
	}
	if w := f.writeLog(); !slices.Equal(w, []string{"POST " + editPath + "/decline"}) {
		t.Errorf("writes %q", w)
	}

	// A decline that leaves the pull request open is a conflict.
	f = newEditFixture(t, "OPEN")
	f.keepOpen = true
	_, err = f.tg.EditPR(t.Context(), 4, platform.PREdit{State: ptr(platform.Closed)})
	wantClass(t, "EditPR", err, platform.ClassConflict, nil)
}

func TestEditPRClosedPullRequests(t *testing.T) {
	for _, state := range []string{"DECLINED", "SUPERSEDED"} {
		t.Run(state, func(t *testing.T) {
			f := newEditFixture(t, state)
			_, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{State: ptr(platform.Open)})
			wantClass(t, "reopen", err, platform.ClassUnsupported, nil)
			if err == nil || !strings.Contains(err.Error(), "Bitbucket cannot reopen a declined pull request") {
				t.Errorf("reopen: %v", err)
			}
			for name, e := range map[string]platform.PREdit{
				"body":           {Body: ptr("acked")},
				"body and close": {Body: ptr("acked"), State: ptr(platform.Closed)},
				"title":          {Title: ptr("chore: new")},
				"base":           {Base: ptr("release")},
			} {
				_, err := f.tg.EditPR(t.Context(), 4, e)
				wantClass(t, name, err, platform.ClassUnsupported, nil)
			}
			got, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{State: ptr(platform.Closed), Body: ptr("old body\r\n\r\n[touchmark]: # \"old\"")})
			if err != nil || got.State != platform.Closed {
				t.Errorf("a close of a closed pull request: %+v, %v; want nothing to do", got, err)
			}
			if w := f.writeLog(); len(w) != 0 {
				t.Errorf("writes %q", w)
			}
		})
	}
	f := newEditFixture(t, "MERGED")
	for name, e := range map[string]platform.PREdit{
		"close": {State: ptr(platform.Closed)},
		"body":  {Body: ptr("x")},
	} {
		_, err := f.tg.EditPR(t.Context(), 4, e)
		wantClass(t, "merged: "+name, err, platform.ClassConflict, nil)
	}
	_, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{State: ptr(platform.Merged)})
	wantClass(t, "set merged", err, platform.ClassUnsupported, nil)
	if w := f.writeLog(); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}

func TestEditPRReviewersRefused(t *testing.T) {
	f := newEditFixture(t, "OPEN")
	f.putStatus = []int{http.StatusBadRequest, http.StatusOK}
	f.putBody = map[string]any{"type": "error", "error": map[string]any{"message": "Bad request",
		"fields": map[string]any{"reviewers": []string{"Malformed reviewers list"}}}}
	if _, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Body: ptr("new")}); err != nil {
		t.Fatal(err)
	}
	puts := f.requests(http.MethodPut, editPath)
	if len(puts) != 2 || body(t, puts[0])["reviewers"] == nil || body(t, puts[1])["reviewers"] != nil {
		t.Errorf("PUTs %d; want one with the reviewers, then one without", len(puts))
	}
	// Another refusal stands.
	f = newEditFixture(t, "OPEN")
	f.putStatus = []int{http.StatusBadRequest}
	f.putBody = errorBody("the destination branch does not exist")
	_, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Base: ptr("nope")})
	wantClass(t, "EditPR", err, platform.ClassInvalid, nil)
	if n := len(f.requests(http.MethodPut, editPath)); n != 1 {
		t.Errorf("%d PUTs", n)
	}
}

func TestEditPRErrors(t *testing.T) {
	f := newEditFixture(t, "OPEN")
	for name, tc := range map[string]struct {
		n     int64
		e     platform.PREdit
		class platform.Class
	}{
		"no number":   {0, platform.PREdit{}, platform.ClassNotFound},
		"blank title": {4, platform.PREdit{Title: ptr(" ")}, platform.ClassInvalid},
		"empty base":  {4, platform.PREdit{Base: ptr("")}, platform.ClassInvalid},
		"bad state":   {4, platform.PREdit{State: ptr(platform.PRState("draft"))}, platform.ClassInvalid},
	} {
		_, err := f.tg.EditPR(t.Context(), tc.n, tc.e)
		wantClass(t, name, err, tc.class, nil)
	}
	f.json(prsPath+"/9", http.StatusNotFound, errorBody("Pull request not found"))
	_, err := f.tg.EditPR(t.Context(), 9, platform.PREdit{Body: ptr("x")})
	wantClass(t, "missing", err, platform.ClassNotFound, platform.ErrNotFound)
	if w := f.writeLog(); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}

func TestComment(t *testing.T) {
	f := newWriterFixture(t)
	tg := f.target(t)
	f.handle(http.MethodPost, editPath+"/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": 1, "type": "pullrequest_comment", "content": map[string]any{"raw": "x"}})
	})
	f.handle(http.MethodPost, prsPath+"/9/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody("Pull request not found"))
	})
	if err := tg.Comment(t.Context(), 4, "touchmark closed this:\n\nno diff"); err != nil {
		t.Fatal(err)
	}
	c := f.requests(http.MethodPost, editPath+"/comments")
	if len(c) != 1 || mustJSON(t, body(t, c[0])) != `{"content":{"raw":"touchmark closed this:\n\nno diff"}}` {
		t.Errorf("comments %+v", c)
	}
	wantClass(t, "missing", tg.Comment(t.Context(), 9, "x"), platform.ClassNotFound, platform.ErrNotFound)
	wantClass(t, "empty", tg.Comment(t.Context(), 4, " \n"), platform.ClassInvalid, nil)
	wantClass(t, "no number", tg.Comment(t.Context(), 0, "x"), platform.ClassNotFound, nil)
}

func TestEnsureLabels(t *testing.T) {
	f := newWriterFixture(t)
	tg := f.target(t)
	ids, err := tg.EnsureLabels(t.Context(), []string{"engineering-assets"})
	if ids != nil || err != nil {
		t.Errorf("EnsureLabels = %v, %v; want nothing: Bitbucket has no labels", ids, err)
	}
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// TestWriterMasksToken: a write's error that echoes the token shows it
// masked, in both of its forms.
func TestWriterMasksToken(t *testing.T) {
	f := newEditFixture(t, "OPEN")
	basic := base64.StdEncoding.EncodeToString([]byte("x-bitbucket-api-token-auth:" + f.token))
	f.putStatus = []int{http.StatusConflict}
	f.putBody = errorBody("echo " + f.token + " and " + basic)
	_, err := f.tg.EditPR(t.Context(), 4, platform.PREdit{Body: ptr("x")})
	wantClass(t, "EditPR", err, platform.ClassConflict, nil)
	if err == nil || strings.Contains(err.Error(), f.token) || strings.Contains(err.Error(), basic) {
		t.Errorf("EditPR: %v", err)
	}
}
