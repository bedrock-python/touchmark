package bitbucketdc

import (
	"bytes"
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

// The fixtures below follow the OpenAPI descriptions of Bitbucket Data
// Center 8.19 to 10.5 (developer.atlassian.com/server/bitbucket/rest/) and
// the examples of its REST documentation: every field the driver reads,
// and a few it ignores, as the reference shapes them. None was seen on a
// live instance.

// contextPath is the instance's context path: Bitbucket is often served
// under one (https://example.com/bitbucket).
const contextPath = "/bitbucket"

// apiPrefix is the REST base under the server's root.
const apiPrefix = contextPath + "/rest/api/latest"

// apiServer is an httptest server that answers the routes a test declares
// and records every request. An undeclared route fails the test, and so
// does any write unless the test is the writer's (allowWrites), whose
// writes are declared routes too. Routes are paths under the REST base as
// sent, escaped. An answer to the server's token carries X-AUSERNAME, as
// Bitbucket's answers to an authenticated user do.
type apiServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []apiCall
	// token is the credential the server knows, username its user's name.
	token, username string
	// allowWrites lets the writer's tests write (their declared routes).
	allowWrites bool
}

// apiCall is one request the server received.
type apiCall struct {
	Method, Path string // Path escaped, under the REST base
	Query        url.Values
	Auth         string // Authorization
	Body         []byte
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, routes: map[string]http.HandlerFunc{}, token: testToken(t), username: "touchmark.reader"}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	// The read driver never writes.
	t.Cleanup(func() {
		if s.allowWrites {
			return
		}
		for _, c := range s.writes() {
			t.Errorf("the reader wrote: %s %s", c.Method, c.Path)
		}
	})
	return s
}

// base is the instance's base URL, context path included.
func (s *apiServer) base() string { return s.srv.URL + contextPath }

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	p, ok := strings.CutPrefix(r.URL.EscapedPath(), apiPrefix)
	authz := r.Header.Get("Authorization")
	s.mu.Lock()
	s.calls = append(s.calls, apiCall{Method: r.Method, Path: p, Query: r.URL.Query(), Auth: authz, Body: body})
	h := s.routes[r.Method+" "+p]
	s.mu.Unlock()
	if authz == "Bearer "+s.token && s.username != "" {
		w.Header().Set("X-AUSERNAME", s.username)
	}
	if !ok || h == nil {
		s.t.Errorf("unexpected request %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		writeJSON(w, http.StatusNotFound, errorBody("com.atlassian.bitbucket.NoSuchResourceException", "Resource not found"))
		return
	}
	h(w, r)
}

// handle declares a route: method and an escaped path under the REST base.
func (s *apiServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// json declares a GET route that answers status with v as JSON (no body
// when v is nil).
func (s *apiServer) json(path string, status int, v any) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// pages declares a GET route that answers items, at most per a page
// whatever the limit asked (as page.max.* caps it), with start, limit,
// isLastPage and nextPageStart as the REST intro shows them.
func (s *apiServer) pages(path string, per int, items []any) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, per, items)
	})
}

// servePage writes the page of items r asks for.
func servePage(w http.ResponseWriter, r *http.Request, per int, items []any) {
	q := r.URL.Query()
	start, _ := strconv.Atoi(q.Get("start"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 25
	}
	limit = min(limit, per)
	from := min(start, len(items))
	to := min(from+limit, len(items))
	body := map[string]any{"values": items[from:to], "size": to - from, "limit": limit, "start": start,
		"isLastPage": to >= len(items), "filter": nil}
	if to < len(items) {
		body["nextPageStart"] = to
	}
	writeJSON(w, http.StatusOK, body)
}

// requests returns the recorded calls of method to path under the REST
// base ("" matches all).
func (s *apiServer) requests(method, path string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []apiCall
	for _, c := range s.calls {
		if (method == "" || c.Method == method) && (path == "" || c.Path == path) {
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
	if v == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is Bitbucket's error body (REST intro, "Errors").
func errorBody(exception, msg string) map[string]any {
	return map[string]any{"errors": []any{map[string]any{"context": nil, "message": msg, "exceptionName": exception}}}
}

// Exceptions and messages of the fixtures' errors.
const (
	noRepoException = "com.atlassian.bitbucket.repository.NoSuchRepositoryException"
	noRepoMessage   = "Repository API/missing does not exist."
	noPathException = "com.atlassian.bitbucket.content.NoSuchPathException"
	authorisation   = "com.atlassian.bitbucket.AuthorisationException"
)

// testToken returns a token made at run time, as long as a real one.
func testToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 22)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "BBDC-" + hex.EncodeToString(b)
}

// provider returns the resolved Bitbucket Data Center provider at the
// server.
func (s *apiServer) provider() config.ResolvedProvider {
	u, _ := url.Parse(s.base())
	return config.ResolvedProvider{
		Provider:  config.Provider{ID: "bbdc", Type: "bitbucket-datacenter", URL: s.base()},
		Host:      strings.ToLower(u.Host),
		APIURL:    s.base() + "/rest/api/latest",
		EnvPrefix: "TOUCHMARK_BBDC_",
	}
}

// fixture is a reader with the server's token.
type fixture struct {
	*apiServer
	reader *reader
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := newAPIServer(t)
	r, err := NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: s.token}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{apiServer: s, reader: r.(*reader)}
}

// Fixed ids and names.
const (
	repoID     = 101
	forkID     = 102
	readerID   = 11
	writerID   = 12
	personID   = 13
	serviceID  = 14
	syncBranch = "touchmark/acme-eng"
	headSHA    = "8d51122def5632836d1cb1026e879069e10a1e13"
	mainSHA    = "d2391895ff0b38a0c28c4f1b3e0f1a4b5c6d7e8f"
)

// user is a user as GET /users/{slug} and pull requests send it.
func user(id int64, name, slug, typ string) map[string]any {
	return map[string]any{"name": name, "emailAddress": name + "@example.com", "active": true,
		"displayName": strings.ToUpper(name[:1]) + name[1:], "id": id, "slug": slug, "type": typ,
		"links": map[string]any{"self": []any{map[string]any{"href": "https://bitbucket.example.com/users/" + slug}}}}
}

// project is a repository's project.
func project(key string) map[string]any {
	return map[string]any{"key": key, "id": 1, "name": "Acme " + key, "public": false, "type": "NORMAL",
		"links": map[string]any{"self": []any{map[string]any{"href": "https://bitbucket.example.com/projects/" + key}}}}
}

// repoOpt changes a repository fixture.
type repoOpt func(map[string]any)

func with(k string, v any) repoOpt { return func(m map[string]any) { m[k] = v } }

// repo is a repository as GET /projects/{key}/repos/{slug} sends it.
func repo(id int64, key, slug string, opts ...repoOpt) map[string]any {
	m := map[string]any{
		"slug": slug, "id": id, "name": slug, "hierarchyId": "e3c939f9ef4a7fae272e", "scmId": "git",
		"state": "AVAILABLE", "statusMessage": "Available", "forkable": true, "public": false, "archived": false,
		"project": project(key),
		"links": map[string]any{
			"clone": []any{
				map[string]any{"href": "https://bitbucket.example.com/scm/" + strings.ToLower(key) + "/" + slug + ".git", "name": "http"},
				map[string]any{"href": "ssh://git@bitbucket.example.com:7999/" + strings.ToLower(key) + "/" + slug + ".git", "name": "ssh"},
			},
			"self": []any{map[string]any{"href": "https://bitbucket.example.com/projects/" + key + "/repos/" + slug + "/browse"}},
		},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// repoRef is the repository of a pull request's ref.
func repoRef(id int64, key, slug string) map[string]any {
	return map[string]any{"id": id, "slug": slug, "name": slug, "project": project(key), "public": false, "scmId": "git", "state": "AVAILABLE"}
}

// branch is a branch as GET …/branches sends it.
func branch(name, commit string, def bool) map[string]any {
	return map[string]any{"id": "refs/heads/" + name, "displayId": name, "type": "BRANCH",
		"latestCommit": commit, "latestChangeset": commit, "isDefault": def}
}

// defBranch declares the default branch of key/slug: GET
// …/branches/default answering the branch.
func (s *apiServer) defBranch(key, slug, name, commit string) {
	s.json(repoRoute(key, slug)+"/branches/default", http.StatusOK, branch(name, commit, true))
}

// repoRoute is the route of a repository.
func repoRoute(key, slug string) string { return "/projects/" + key + "/repos/" + slug }

// prSpec describes a pull request fixture.
type prSpec struct {
	id         int64
	state      string // OPEN (default), MERGED, DECLINED
	author     map[string]any
	source     string
	sourceRepo map[string]any // default the target repository
	target     string         // default "main"
	targetRepo map[string]any // default acme/api
	commit     string         // the source commit, headSHA by default
	title      string
	body       string // "" leaves the description out, as the API does
	draft      bool
	version    int64
}

// pr is a pull request as the pull request APIs send it.
func pr(p prSpec) map[string]any {
	state := cmpStr(p.state, "OPEN")
	dest := p.targetRepo
	if dest == nil {
		dest = repoRef(repoID, "ACME", "api")
	}
	src := p.sourceRepo
	if src == nil {
		src = dest
	}
	target := cmpStr(p.target, "main")
	m := map[string]any{
		"id": p.id, "version": p.version, "title": cmpStr(p.title, "sync"), "state": state,
		"open": state == "OPEN", "closed": state != "OPEN", "draft": p.draft, "locked": false,
		"createdDate": int64(1788163200000) + p.id, "updatedDate": int64(1788336000000) + p.id,
		"fromRef": map[string]any{"id": "refs/heads/" + p.source, "displayId": p.source, "type": "BRANCH",
			"latestCommit": cmpStr(p.commit, headSHA), "repository": src},
		"toRef": map[string]any{"id": "refs/heads/" + target, "displayId": target, "type": "BRANCH",
			"latestCommit": mainSHA, "repository": dest},
		"author":       map[string]any{"user": p.author, "role": "AUTHOR", "approved": false, "status": "UNAPPROVED"},
		"reviewers":    []any{},
		"participants": []any{},
		"links":        map[string]any{"self": []any{map[string]any{"href": fmt.Sprintf("https://bitbucket.example.com/projects/ACME/repos/api/pull-requests/%d", p.id)}}},
	}
	if state != "OPEN" {
		m["closedDate"] = int64(1788422400000) + p.id
	}
	if p.body != "" {
		m["description"] = p.body
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
