package bitbucket

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The fixtures below follow the OpenAPI description of the REST API 2.0
// (https://api.bitbucket.org/swagger.json) and answers of public
// repositories read anonymously (atlassian/atlaskit-mk-2, 2026-10-08):
// every field the driver reads, and a few it ignores, as the API sends
// them.

// apiServer is an httptest server that answers the routes a test declares
// and records every request. An undeclared route fails the test. Routes
// are paths under /2.0 as sent, escaped ("/users/%7B…%7D").
type apiServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []apiCall
}

// apiCall is one request the server received.
type apiCall struct {
	Method, Path string // Path escaped, under /2.0
	Query        url.Values
	Auth         string // Authorization
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	// The read driver never writes.
	t.Cleanup(func() {
		for _, c := range s.writes() {
			t.Errorf("the reader wrote: %s %s", c.Method, c.Path)
		}
	})
	return s
}

// base is the server's web URL; the API is under base + "/2.0".
func (s *apiServer) base() string { return s.srv.URL }

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	p := r.URL.EscapedPath()
	s.mu.Lock()
	s.calls = append(s.calls, apiCall{Method: r.Method, Path: p, Query: r.URL.Query(), Auth: r.Header.Get("Authorization")})
	h := s.routes[r.Method+" "+p]
	s.mu.Unlock()
	if h == nil {
		s.t.Errorf("unexpected request %s %s?%s", r.Method, p, r.URL.RawQuery)
		writeJSON(w, http.StatusNotFound, errorBody("Resource not found"))
		return
	}
	h(w, r)
}

// apiPath returns path under /2.0.
func apiPath(path string) string { return "/2.0" + path }

// handle declares a route: method and an escaped path under /2.0.
func (s *apiServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+apiPath(path)] = h
}

// json declares a GET route that answers status with v as JSON.
func (s *apiServer) json(path string, status int, v any) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// pages declares a GET route that answers items per a page, with the
// page's next link (absolute, the query kept, page=n+1) on every page but
// the last, as the API paginates.
func (s *apiServer) pages(path string, per int, items []any) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, per, items)
	})
}

// servePage writes the page of items r asks for.
func servePage(w http.ResponseWriter, r *http.Request, per int, items []any) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("page"))
	if n <= 0 {
		n = 1
	}
	from := min((n-1)*per, len(items))
	to := min(from+per, len(items))
	body := map[string]any{"values": items[from:to], "pagelen": per, "page": n, "size": len(items)}
	if to < len(items) {
		q.Set("page", strconv.Itoa(n+1))
		body["next"] = fmt.Sprintf("http://%s%s?%s", r.Host, r.URL.EscapedPath(), q.Encode())
	}
	writeJSON(w, http.StatusOK, body)
}

// requests returns the recorded calls of method to path under /2.0 (""
// matches all).
func (s *apiServer) requests(method, path string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []apiCall
	for _, c := range s.calls {
		if (method == "" || c.Method == method) && (path == "" || c.Path == apiPath(path)) {
			out = append(out, c)
		}
	}
	return out
}

// writes returns the recorded requests that are not GETs.
func (s *apiServer) writes() []apiCall {
	var out []apiCall
	for _, c := range s.requests("", "") {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// reset forgets the recorded calls.
func (s *apiServer) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is Bitbucket's error body.
func errorBody(msg string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"message": msg}}
}

// Messages of 404s, as the API sends them anonymously.
const (
	noRepoMessage = "You may not have access to this repository or it no longer exists in this workspace. " +
		"If you think this repository exists and you have access, make sure you are authenticated."
	noFileMessage = "No such file or directory: "
)

// testToken returns a token made at run time, as long as a real one.
func testToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "ATATT3x-" + hex.EncodeToString(b)
}

// provider returns the resolved Bitbucket provider at the server.
func (s *apiServer) provider() config.ResolvedProvider {
	u, _ := url.Parse(s.base())
	return config.ResolvedProvider{
		Provider:  config.Provider{ID: "bb", Type: "bitbucket", URL: s.base(), APIURL: s.base() + "/2.0"},
		Host:      strings.ToLower(u.Host),
		APIURL:    s.base() + "/2.0",
		EnvPrefix: "TOUCHMARK_BB_",
	}
}

// fixture is a reader with a token over one test server.
type fixture struct {
	*apiServer
	token  string
	reader *reader
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := newAPIServer(t)
	tok := testToken(t)
	r, err := NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: tok}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{apiServer: s, token: tok, reader: r.(*reader)}
}

// Fixed ids.
const (
	repoUUID   = "{6380b4e9-6ac5-4dd4-a8e0-65f09cabe4c8}"
	forkUUID   = "{0d3c2b1a-9f8e-4d7c-b6a5-948372615041}"
	botUUID    = "{583f7ec5-ed93-49a9-b449-cfc4556cd7f8}"
	personUUID = "{bd1ac3ae-5a4e-416a-beab-37f06dd4cc93}"
	otherUUID  = "{b2457ec9-dd07-49b5-817e-9c9168905a3d}"
	syncBranch = "touchmark/acme-eng"
)

// uuidPath is a uuid as a path segment: braces escaped.
func uuidPath(uuid string) string { return url.PathEscape(uuid) }

// account is a user as pull requests and GET /users/{uuid} send it.
func account(uuid, nickname string) map[string]any {
	return map[string]any{
		"display_name": nickname,
		"links": map[string]any{
			"self":   map[string]any{"href": "https://api.bitbucket.org/2.0/users/" + uuidPath(uuid)},
			"avatar": map[string]any{"href": "https://avatar-management--avatars.us-west-2.prod.public.atl-paas.net/initials/H-5.png"},
			"html":   map[string]any{"href": "https://bitbucket.org/" + uuidPath(uuid) + "/"},
		},
		"type": "user", "uuid": uuid, "account_id": "5a8383aa89ef572e36a586ac", "nickname": nickname,
	}
}

// appUser is an app user (an access token's bot).
func appUser(uuid, name string) map[string]any {
	return map[string]any{"display_name": name, "type": "app_user", "uuid": uuid,
		"account_id": "712020:1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", "kind": "access_token", "links": map[string]any{}}
}

// repoOpt changes a repository fixture.
type repoOpt func(map[string]any)

func with(k string, v any) repoOpt { return func(m map[string]any) { m[k] = v } }

// repo is a repository as GET /repositories/{workspace}/{slug} sends it.
func repo(uuid, full string, opts ...repoOpt) map[string]any {
	ws, slug, _ := strings.Cut(full, "/")
	m := map[string]any{
		"type": "repository", "full_name": full, "name": slug, "slug": slug, "description": "", "scm": "git",
		"website": "", "is_private": true, "fork_policy": "no_public_forks", "uuid": uuid,
		"owner":      map[string]any{"display_name": ws, "type": "team", "uuid": "{02b941e3-cfaa-40f9-9a58-cec53e20bdc3}", "username": ws},
		"workspace":  map[string]any{"type": "workspace", "uuid": "{02b941e3-cfaa-40f9-9a58-cec53e20bdc3}", "name": ws, "slug": ws},
		"project":    map[string]any{"type": "project", "key": "PROJ", "uuid": "{8b56daff-dbc7-4cae-a7a3-1228c526906b}", "name": "Project"},
		"created_on": "2026-09-01T10:00:00.000000+00:00", "updated_on": "2026-09-02T10:00:00.000000+00:00",
		"size": 191411, "language": "python", "mainbranch": map[string]any{"name": "main", "type": "branch"},
		"parent": nil, "enforced_signed_commits": nil, "has_issues": false, "has_wiki": false,
		"override_settings": map[string]any{"default_merge_strategy": false, "branching_model": false},
		"links": map[string]any{
			"self": map[string]any{"href": "https://api.bitbucket.org/2.0/repositories/" + full},
			"html": map[string]any{"href": "https://bitbucket.org/" + full},
		},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// repoRef is the short repository of a pull request's endpoint.
func repoRef(uuid, full string) map[string]any {
	return map[string]any{"type": "repository", "full_name": full, "name": full[strings.IndexByte(full, '/')+1:], "uuid": uuid,
		"links": map[string]any{"html": map[string]any{"href": "https://bitbucket.org/" + full}}}
}

// prSpec describes a pull request fixture.
type prSpec struct {
	id         int64
	state      string // OPEN (default), MERGED, DECLINED, SUPERSEDED
	author     map[string]any
	closedBy   map[string]any
	source     string
	sourceRepo map[string]any // default the destination repository
	target     string         // default "main"
	hash       string         // the source commit, 12 digits by default
	title      string
	body       string
	draft      bool
	noSummary  bool // a listing without summary and description
}

// pr is a pull request as the pull request APIs send it.
func pr(p prSpec) map[string]any {
	state := cmpStr(p.state, "OPEN")
	dest := repoRef(repoUUID, "acme/api")
	src := p.sourceRepo
	if src == nil {
		src = dest
	}
	hash := cmpStr(p.hash, fmt.Sprintf("%012x", 0xa6dcc7402000+p.id))
	m := map[string]any{
		"comment_count": 0, "task_count": 0, "type": "pullrequest", "id": p.id, "title": cmpStr(p.title, "sync"),
		"state": state, "draft": p.draft, "merge_commit": nil, "close_source_branch": false, "reason": "",
		"author": p.author, "closed_by": nil,
		"created_on": "2026-09-05T08:00:00.000000+00:00", "updated_on": "2026-09-10T12:00:00.123456+00:00",
		"destination": map[string]any{
			"branch":     map[string]any{"name": cmpStr(p.target, "main")},
			"commit":     map[string]any{"hash": "d2391895ff0b", "type": "commit"},
			"repository": dest,
		},
		"source": map[string]any{
			"branch":     map[string]any{"name": p.source, "links": map[string]any{}, "sync_strategies": []string{"merge_commit", "rebase"}},
			"commit":     map[string]any{"hash": hash, "type": "commit"},
			"repository": src,
		},
		"links": map[string]any{"html": map[string]any{"href": fmt.Sprintf("https://bitbucket.org/acme/api/pull-requests/%d", p.id)}},
	}
	if state != "OPEN" && p.closedBy != nil {
		m["closed_by"] = p.closedBy
	}
	if !p.noSummary {
		m["description"] = p.body
		m["summary"] = map[string]any{"type": "rendered", "raw": p.body, "markup": "markdown", "html": "<p>" + p.body + "</p>"}
	}
	return m
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// wantClass checks the class of err (and, with sentinel, that it wraps it).
func wantClass(t *testing.T, what string, err error, class platform.Class, sentinel error) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error, want class %v", what, class)
	case platform.ClassOf(err) != class:
		t.Errorf("%s: class %v of %v, want %v", what, platform.ClassOf(err), err, class)
	case sentinel != nil && !errors.Is(err, sentinel):
		t.Errorf("%s: %v does not wrap %v", what, err, sentinel)
	}
}
