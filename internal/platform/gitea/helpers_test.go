package gitea

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The fixtures below are written by hand after the swagger of Gitea 1.26
// and 1.27 and Forgejo 15 and 16 (/swagger.v1.json): every field the
// driver reads, and a few it ignores, as the servers send them.

// apiServer is an httptest server that answers the routes a test declares
// and records every request. An undeclared route fails the test.
type apiServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // "METHOD /path" (API-relative, unescaped)
	calls  []apiCall
}

// apiCall is one request the server received.
type apiCall struct {
	Method, Path, RawPath string
	Query                 url.Values
	Auth                  string
	Body                  string
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// base is the server's web URL; the API is under base + "/api/v1".
func (s *apiServer) base() string { return s.srv.URL }

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.calls = append(s.calls, apiCall{Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(),
		Query: r.URL.Query(), Auth: r.Header.Get("Authorization"), Body: string(body)})
	h := s.routes[r.Method+" "+r.URL.Path]
	s.mu.Unlock()
	if h == nil {
		s.t.Errorf("unexpected request %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "no route in the test", "url": s.base() + "/api/swagger"})
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	h(w, r)
}

// handle declares a route: method and an API path such as
// "/repos/acme/api" (unescaped; "/api/v1" is added).
func (s *apiServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(path, "/api/") {
		path = "/api/v1" + path
	}
	s.routes[method+" "+path] = h
}

// json declares a route that answers status with v as JSON.
func (s *apiServer) json(method, path string, status int, v any) {
	s.handle(method, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// requests returns the recorded calls of method to path ("" matches all).
func (s *apiServer) requests(method, path string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path != "" && !strings.HasPrefix(path, "/api/") {
		path = "/api/v1" + path
	}
	var out []apiCall
	for _, c := range s.calls {
		if (method == "" || c.Method == method) && (path == "" || c.Path == path) {
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
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiMsg is Gitea's error body.
func (s *apiServer) apiMsg(msg string) map[string]any {
	return map[string]any{"message": msg, "url": s.base() + "/api/swagger", "errors": nil}
}

// pageHeaders are the pagination headers a listing of the servers sends
// (checked on Gitea 1.26 and 1.27 and Forgejo 15 and 16 by the live fact
// "pagination" of internal/e2e).
type pageHeaders uint8

const (
	// linkAndTotal is a Link header with rel="next" while pages follow, and
	// X-Total-Count the size of the whole listing: pull requests,
	// repositories, the issue search (SetLinkHeader and SetTotalCountHeader).
	linkAndTotal pageHeaders = iota
	// totalOnly is X-Total-Count the size of the whole listing and no Link:
	// the labels of a repository or an organization (ListLabels in
	// routers/api/v1/repo/label.go and routers/api/v1/org/label.go, Gitea
	// 1.27.3).
	totalOnly
	// pageCount is X-Total-Count the size of the page and no Link: the
	// timeline (ListIssueCommentsAndTimeline in
	// routers/api/v1/repo/issue_comment.go, Gitea 1.27.3, counts the events
	// it returns once code comments and cross-references the caller may not
	// see are dropped).
	pageCount
)

// pages answers a listing from items by page and limit, with the Link and
// X-Total-Count headers of pull requests and repositories.
func (s *apiServer) pages(path string, items []any) {
	s.pagesWith(path, items, linkAndTotal)
}

// pagesWith answers a listing from items by page and limit, with headers h.
func (s *apiServer) pagesWith(path string, items []any, h pageHeaders) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, items, h)
	})
}

// route returns the handler of a declared route, nil for none.
func (s *apiServer) route(method, path string) http.HandlerFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(path, "/api/") {
		path = "/api/v1" + path
	}
	return s.routes[method+" "+path]
}

// servePage writes the page of items that r asks for, with the headers h
// describes.
func servePage(w http.ResponseWriter, r *http.Request, items []any, h pageHeaders) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if limit <= 0 {
		limit = 30
	}
	if page <= 0 {
		page = 1
	}
	from := min((page-1)*limit, len(items))
	to := min(from+limit, len(items))
	last := (len(items) + limit - 1) / limit
	if h == linkAndTotal && last > 1 {
		var links []string
		u := *r.URL
		link := func(p int, rel string) {
			q := u.Query()
			q.Set("page", strconv.Itoa(p))
			u.RawQuery = q.Encode()
			links = append(links, fmt.Sprintf("<http://%s%s>; rel=%q", r.Host, u.RequestURI(), rel))
		}
		if page < last {
			link(page+1, "next")
			link(last, "last")
		}
		if page > 1 {
			link(1, "first")
			link(page-1, "prev")
		}
		w.Header().Set("Link", strings.Join(links, ","))
	}
	total := len(items)
	if h == pageCount {
		total = to - from
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, items[from:to])
}

// testToken returns a token made at run time, as long as a real one.
func testToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// provider returns the resolved provider of typ at the server.
func (s *apiServer) provider(typ string) config.ResolvedProvider {
	u, _ := url.Parse(s.base())
	return config.ResolvedProvider{
		Provider:  config.Provider{ID: "forge", Type: typ, URL: s.base()},
		Host:      strings.ToLower(u.Host),
		APIURL:    s.base() + "/api/v1",
		EnvPrefix: "TOUCHMARK_FORGE_",
	}
}

// fixture is a driver pair over one test server.
type fixture struct {
	*apiServer
	token  string
	reader *reader
	writer *writer
}

// newFixture returns a reader and a writer of flavor typ with one token,
// on a server that answers the version endpoints of Gitea 1.27.3 or
// Forgejo 16.0.5 (and anonymous requests too, as a default instance does):
// Repo, Resolve and OpenPRsBy ask them first.
func newFixture(t *testing.T, typ string) *fixture {
	t.Helper()
	s := newAPIServer(t)
	if typ == "forgejo" {
		s.asForgejo("16.0.5")
	} else {
		s.asGitea()
	}
	tok := testToken(t)
	cred := auth.Credential{Kind: auth.Token, Token: tok}
	client := httpx.New(httpx.Options{})
	r, err := NewReader(s.provider(typ), cred, client)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(s.provider(typ), cred, client)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{apiServer: s, token: tok, reader: r.(*reader), writer: w.(*writer)}
}

// settings declares GET /settings/api with a page size.
func (s *apiServer) settings(maxItems int) {
	s.json(http.MethodGet, "/settings/api", http.StatusOK, map[string]any{
		"max_response_items": maxItems, "default_paging_num": 30, "default_git_trees_per_page": 1000,
		"default_max_blob_size": 10485760, "default_max_response_size": 104857600,
	})
}

// asGitea declares the version endpoints of Gitea 1.27.3.
func (s *apiServer) asGitea() {
	s.json(http.MethodGet, "/api/forgejo/v1/version", http.StatusNotFound, s.apiMsg("not found"))
	s.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"version": "1.27.3"})
}

// asForgejo declares the version endpoints of Forgejo version v.
func (s *apiServer) asForgejo(v string) {
	s.json(http.MethodGet, "/api/forgejo/v1/version", http.StatusOK, map[string]any{"version": v + "+gitea-1.22.0"})
	s.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"version": v + "+gitea-1.22.0"})
}

// signInOnly makes the version endpoints answer as an instance with
// [service] REQUIRE_SIGNIN_VIEW = true does to a request without a
// credential (Gitea 1.26 and 1.27, Forgejo 15 and 16): 403 "Only signed in
// user is allowed to call APIs.", or anon when set.
func (s *apiServer) signInOnly(anon http.HandlerFunc) {
	for _, path := range []string{"/version", "/api/forgejo/v1/version"} {
		signedIn := s.route(http.MethodGet, path)
		if signedIn == nil {
			s.t.Fatalf("signInOnly: %s is not declared", path)
		}
		s.handle(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Header.Get("Authorization") != "":
				signedIn(w, r)
			case anon != nil:
				anon(w, r)
			default:
				writeJSON(w, http.StatusForbidden, map[string]any{"message": "Only signed in user is allowed to call APIs."})
			}
		})
	}
}

// user is a user as /users/{name} and nested objects send it.
func (s *apiServer) user(id int64, login string) map[string]any {
	return map[string]any{
		"id": id, "login": login, "login_name": "", "source_id": 0, "full_name": "",
		"email": fmt.Sprintf("%d+%s@noreply.localhost", id, login), "avatar_url": s.base() + "/avatars/0",
		"html_url": s.base() + "/" + login, "language": "", "is_admin": false,
		"last_login": "0001-01-01T00:00:00Z", "created": "2026-09-01T10:00:00Z", "restricted": false,
		"active": true, "prohibit_login": false, "location": "", "website": "", "description": "",
		"visibility": "public", "followers_count": 0, "following_count": 0, "starred_repos_count": 0,
		"username": login,
	}
}

// repoOpt changes a repository fixture.
type repoOpt func(map[string]any)

func withField(k string, v any) repoOpt { return func(m map[string]any) { m[k] = v } }

// repo is a repository as /repos/{owner}/{repo} sends it, to the reader.
func (s *apiServer) repo(id int64, fullName string, opts ...repoOpt) map[string]any {
	owner, name, _ := strings.Cut(fullName, "/")
	m := map[string]any{
		"id": id, "owner": s.org(owner), "name": name, "full_name": fullName, "description": "",
		"empty": false, "private": false, "fork": false, "template": false, "parent": nil, "mirror": false,
		"size": 42, "language": "", "languages_url": s.base() + "/api/v1/repos/" + fullName + "/languages",
		"html_url": s.base() + "/" + fullName, "url": s.base() + "/api/v1/repos/" + fullName, "link": "",
		"ssh_url": "git@example.com:" + fullName + ".git", "clone_url": s.base() + "/" + fullName + ".git",
		"original_url": "", "website": "", "stars_count": 0, "forks_count": 0, "watchers_count": 1,
		"open_issues_count": 0, "open_pr_counter": 0, "release_counter": 0, "default_branch": "main",
		"archived": false, "created_at": "2026-09-01T10:00:00Z", "updated_at": "2026-09-02T10:00:00Z",
		"archived_at": "1970-01-01T00:00:00Z",
		"permissions": map[string]any{"admin": false, "push": false, "pull": true},
		"has_code":    true, "has_issues": true, "internal_tracker": map[string]any{
			"enable_time_tracker": true, "allow_only_contributors_to_track_time": true, "enable_issue_dependencies": true,
		},
		"has_wiki": true, "has_pull_requests": true, "has_projects": true, "has_releases": true, "has_packages": true,
		"has_actions": true, "ignore_whitespace_conflicts": false, "allow_merge_commits": true,
		"allow_rebase": true, "allow_rebase_explicit": true, "allow_squash_merge": true,
		"allow_fast_forward_only_merge": true, "allow_rebase_update": true, "default_delete_branch_after_merge": false,
		"default_merge_style": "merge", "default_allow_maintainer_edit": false, "avatar_url": "",
		"internal": false, "mirror_interval": "", "object_format_name": "sha1",
		"mirror_updated": "0001-01-01T00:00:00Z", "repo_transfer": nil, "topics": []string{}, "licenses": []string{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// org is an organization as a repository's owner.
func (s *apiServer) org(login string) map[string]any {
	m := s.user(1000, login)
	m["email"] = ""
	return m
}

// prSpec describes a pull request fixture.
type prSpec struct {
	number     int64
	author     map[string]any
	head       string // branch; "" for AGit
	headRef    string // defaults to head
	headRepoID int64  // defaults to the base repository
	base       string // defaults to "main"
	baseSHA    *string
	state      string // "open" (default), "closed", "merged"
	mergedBy   map[string]any
	labels     []map[string]any
	title      string
	body       string
	draft      bool
	flow       *int
	closedAt   string
	createdAt  string
}

// pr is a pull request as /repos/{owner}/{repo}/pulls sends it.
func (s *apiServer) pr(repo map[string]any, p prSpec) map[string]any {
	repoID := repo["id"].(int64)
	fullName := repo["full_name"].(string)
	head := map[string]any{"label": p.head, "ref": cmpStr(p.headRef, p.head), "sha": strings.Repeat("a", 39) + strconv.FormatInt(p.number%10, 10),
		"repo_id": repoID, "repo": repo}
	if p.head == "" {
		head["ref"] = fmt.Sprintf("refs/pull/%d/head", p.number)
	}
	if p.headRepoID != 0 {
		head["repo_id"] = p.headRepoID
		head["repo"] = nil
	}
	baseSHA := strings.Repeat("b", 40)
	if p.baseSHA != nil {
		baseSHA = *p.baseSHA
	}
	base := map[string]any{"label": cmpStr(p.base, "main"), "ref": cmpStr(p.base, "main"), "sha": baseSHA, "repo_id": repoID, "repo": repo}
	labels := []any{}
	for _, l := range p.labels {
		labels = append(labels, l)
	}
	state, merged := "open", false
	var closedAt, mergedAt, mergedBy any
	switch p.state {
	case "closed":
		state, closedAt = "closed", cmpStr(p.closedAt, "2026-09-10T12:00:00Z")
	case "merged":
		state, merged, closedAt, mergedAt = "closed", true, cmpStr(p.closedAt, "2026-09-10T12:00:00Z"), cmpStr(p.closedAt, "2026-09-10T12:00:00Z")
		mergedBy = p.mergedBy
	}
	m := map[string]any{
		"id": p.number + 100, "url": fmt.Sprintf("%s/%s/pulls/%d", s.base(), fullName, p.number), "number": p.number,
		"user": p.author, "title": cmpStr(p.title, "pr"), "body": p.body, "labels": labels, "milestone": nil,
		"assignee": nil, "assignees": nil, "requested_reviewers": []any{}, "requested_reviewers_teams": []any{},
		"state": state, "draft": p.draft, "is_locked": false, "comments": 0, "review_comments": 0,
		"additions": 1, "deletions": 0, "changed_files": 1,
		"html_url":  fmt.Sprintf("%s/%s/pulls/%d", s.base(), fullName, p.number),
		"diff_url":  fmt.Sprintf("%s/%s/pulls/%d.diff", s.base(), fullName, p.number),
		"patch_url": fmt.Sprintf("%s/%s/pulls/%d.patch", s.base(), fullName, p.number),
		"mergeable": true, "merged": merged, "merged_at": mergedAt, "merge_commit_sha": nil, "merged_by": mergedBy,
		"allow_maintainer_edit": false, "base": base, "head": head, "merge_base": strings.Repeat("c", 40),
		"due_date": nil, "created_at": cmpStr(p.createdAt, "2026-09-05T08:00:00Z"), "updated_at": "2026-09-10T12:00:00Z",
		"closed_at": closedAt, "pin_order": 0,
	}
	if p.flow != nil {
		m["flow"] = *p.flow
	}
	return m
}

// label is a label fixture.
func (s *apiServer) label(id int64, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "exclusive": false, "is_archived": false, "color": "ededed",
		"description": "", "url": fmt.Sprintf("%s/api/v1/repos/acme/api/labels/%d", s.base(), id)}
}

// event is a timeline entry.
func (s *apiServer) event(id int64, typ string, user map[string]any, at string) map[string]any {
	return map[string]any{"id": id, "type": typ, "html_url": "", "pull_request_url": "", "issue_url": "",
		"user": user, "body": "", "created_at": at, "updated_at": at, "old_project_id": 0, "project_id": 0,
		"old_title": "", "new_title": "", "old_ref": "", "new_ref": "", "ref_action": "", "ref_commit_sha": "",
		"review_id": 0, "label": nil, "assignee": nil, "assignee_team": nil, "removed_assignee": false,
		"resolve_doer": nil, "dependent_issue": nil, "milestone": nil, "old_milestone": nil,
		"tracked_time": nil, "ref_comment": nil, "ref_issue": nil}
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ptr[T any](v T) *T { return &v }

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

// queries returns the query strings of calls, sorted, for assertions.
func queries(calls []apiCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		q := url.Values{}
		for k, v := range c.Query {
			if k != "limit" && k != "page" {
				q[k] = v
			}
		}
		out[i] = q.Encode()
	}
	slices.Sort(out)
	return slices.Compact(out)
}
