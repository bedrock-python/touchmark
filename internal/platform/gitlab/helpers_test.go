package gitlab

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

// The fixtures below are written by hand after the REST documentation of
// GitLab 17.x to 19.x (https://docs.gitlab.com/api/) and the entities of
// lib/api/entities: every field the driver reads, and a few it ignores, as
// the server sends them.

// apiServer is an httptest server that answers the routes a test declares
// and records every request. An undeclared route fails the test. Routes
// are API paths as sent, escaped ("/projects/acme%2Fapi"), with "%2E"
// read as ".".
type apiServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []apiCall
}

// apiCall is one request the server received.
type apiCall struct {
	Method, Path string // Path escaped, "%2E" as "."
	Query        url.Values
	Token        string // PRIVATE-TOKEN
	Auth         string // Authorization
	Body         string
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// base is the server's web URL; the API is under base + "/api/v4".
func (s *apiServer) base() string { return s.srv.URL }

// routePath normalizes an escaped path for matching.
func routePath(p string) string {
	p = strings.ReplaceAll(p, "%2E", ".")
	p = strings.ReplaceAll(p, "%2e", ".")
	return strings.ReplaceAll(p, "%2f", "%2F")
}

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := routePath(r.URL.EscapedPath())
	s.mu.Lock()
	s.calls = append(s.calls, apiCall{Method: r.Method, Path: p, Query: r.URL.Query(),
		Token: r.Header.Get("Private-Token"), Auth: r.Header.Get("Authorization"), Body: string(body)})
	h := s.routes[r.Method+" "+p]
	s.mu.Unlock()
	if h == nil {
		s.t.Errorf("unexpected request %s %s?%s", r.Method, p, r.URL.RawQuery)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "404 Not Found"})
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	h(w, r)
}

// apiPath returns path under /api/v4 unless it is already there.
func apiPath(path string) string {
	if !strings.HasPrefix(path, "/api/") {
		path = "/api/v4" + path
	}
	return routePath(path)
}

// handle declares a route: method and an escaped API path such as
// "/projects/acme%2Fapi" ("/api/v4" is added).
func (s *apiServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+apiPath(path)] = h
}

// json declares a route that answers status with v as JSON.
func (s *apiServer) json(method, path string, status int, v any) {
	s.handle(method, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// requests returns the recorded calls of method to path ("" matches all).
func (s *apiServer) requests(method, path string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path != "" {
		path = apiPath(path)
	}
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// msg is GitLab's error body {"message": m}.
func msg(m any) map[string]any { return map[string]any{"message": m} }

// pageStyle is how a listing paginates.
type pageStyle uint8

const (
	// offset: Link (next, first, last) and X-Next-Page, X-Page,
	// X-Per-Page, X-Prev-Page, X-Total and X-Total-Pages
	// (Gitlab::Pagination::OffsetHeaderBuilder).
	offset pageStyle = iota
	// offsetLarge: as offset for a listing over 10 000 rows, whose X-Total,
	// X-Total-Pages and rel="last" GitLab leaves out.
	offsetLarge
	// keyset: only Link with rel="next" (page_token), as the tree and the
	// /projects listing do with pagination=keyset.
	keyset
	// bare: no pagination headers at all (a proxy dropped them).
	bare
)

// pages answers a listing from items with offset pagination.
func (s *apiServer) pages(path string, items []any) { s.pagesWith(path, items, offset) }

// pagesWith answers a listing from items by page and per_page, with the
// headers of style.
func (s *apiServer) pagesWith(path string, items []any, style pageStyle) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, items, style)
	})
}

// servePage writes the page of items that r asks for.
func servePage(w http.ResponseWriter, r *http.Request, items []any, style pageStyle) {
	q := r.URL.Query()
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 20
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if style == keyset {
		page, _ = strconv.Atoi(strings.TrimPrefix(q.Get("page_token"), "tok"))
	}
	if page <= 0 {
		page = 1
	}
	from := min((page-1)*per, len(items))
	to := min(from+per, len(items))
	last := max(1, (len(items)+per-1)/per)
	link := func(p int, rel string) string {
		lq := url.Values{}
		for k, v := range q {
			lq[k] = v
		}
		if style == keyset {
			lq.Del("page")
			lq.Set("page_token", "tok"+strconv.Itoa(p))
		} else {
			lq.Set("page", strconv.Itoa(p))
		}
		return fmt.Sprintf("<http://%s%s?%s>; rel=%q", r.Host, r.URL.EscapedPath(), lq.Encode(), rel)
	}
	var links []string
	if page < last {
		links = append(links, link(page+1, "next"))
	}
	switch style {
	case offset, offsetLarge:
		if page > 1 {
			links = append(links, link(page-1, "prev"))
		}
		links = append(links, link(1, "first"))
		if style == offset {
			links = append(links, link(last, "last"))
		}
		w.Header().Set("Link", strings.Join(links, ", "))
		next := ""
		if page < last {
			next = strconv.Itoa(page + 1)
		}
		w.Header().Set("X-Next-Page", next)
		w.Header().Set("X-Page", strconv.Itoa(page))
		w.Header().Set("X-Per-Page", strconv.Itoa(per))
		if style == offset {
			w.Header().Set("X-Total", strconv.Itoa(len(items)))
			w.Header().Set("X-Total-Pages", strconv.Itoa(last))
		}
	case keyset:
		if len(links) > 0 {
			w.Header().Set("Link", strings.Join(links, ", "))
		}
	}
	writeJSON(w, http.StatusOK, items[from:to])
}

// testToken returns a token made at run time, as long as a real one.
func testToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "test-" + hex.EncodeToString(b)
}

// provider returns the resolved GitLab provider at the server.
func (s *apiServer) provider() config.ResolvedProvider {
	u, _ := url.Parse(s.base())
	return config.ResolvedProvider{
		Provider:  config.Provider{ID: "corp", Type: "gitlab", URL: s.base()},
		Host:      strings.ToLower(u.Host),
		APIURL:    s.base() + "/api/v4",
		EnvPrefix: "TOUCHMARK_CORP_",
	}
}

// fixture is a driver pair over one test server.
type fixture struct {
	*apiServer
	token  string
	reader *reader
	writer *writer
}

// newFixture returns a reader and a writer with one token on a server that
// answers GET /metadata of GitLab 18.11.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusOK, map[string]any{
		"version": "18.11.2", "revision": "0123abcd", "enterprise": false,
		"kas": map[string]any{"enabled": false, "externalUrl": nil, "externalK8sProxyUrl": nil, "version": nil},
	})
	tok := testToken(t)
	cred := auth.Credential{Kind: auth.Token, Token: tok}
	client := httpx.New(httpx.Options{})
	r, err := NewReader(s.provider(), cred, client)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(s.provider(), cred, client)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{apiServer: s, token: tok, reader: r.(*reader), writer: w.(*writer)}
}

// user is a user as GET /users/:id sends it to a regular user.
func user(id int64, username string, bot bool) map[string]any {
	return map[string]any{
		"id": id, "username": username, "name": username, "state": "active", "locked": false,
		"avatar_url": "https://secure.gravatar.com/avatar/0", "web_url": "https://gitlab.example.com/" + username,
		"created_at": "2026-09-01T10:00:00.000Z", "bio": "", "location": "", "public_email": "", "linkedin": "",
		"twitter": "", "discord": "", "github": "", "website_url": "", "organization": "", "job_title": "",
		"pronouns": nil, "bot": bot, "work_information": nil, "followers": 0, "following": 0, "is_followed": false,
		"local_time": nil,
	}
}

// basic is a user as lists, merge requests and notes send it (UserBasic).
func basic(id int64, username string) map[string]any {
	return map[string]any{"id": id, "username": username, "name": username, "state": "active", "locked": false,
		"avatar_url": "https://secure.gravatar.com/avatar/0", "web_url": "https://gitlab.example.com/" + username}
}

// self is GET /user of a regular user (no is_admin: only administrators
// get it, API::Entities::UserWithAdmin).
func self(id int64, username string, bot bool) map[string]any {
	m := user(id, username, bot)
	m["email"] = username + "@noreply.gitlab.example.com"
	m["commit_email"] = m["email"]
	m["external"] = bot
	m["private_profile"] = false
	m["two_factor_enabled"] = false
	m["can_create_group"] = false
	m["can_create_project"] = false
	m["projects_limit"] = 0
	m["theme_id"] = 1
	m["color_scheme_id"] = 1
	m["identities"] = []any{}
	return m
}

// projectOpt changes a project fixture.
type projectOpt func(map[string]any)

func with(k string, v any) projectOpt { return func(m map[string]any) { m[k] = v } }

// project is a project as GET /projects/:id sends it to a Reporter.
func project(id int64, full string, opts ...projectOpt) map[string]any {
	ns, name, _ := cutLast(full)
	m := map[string]any{
		"id": id, "description": "", "name": name, "name_with_namespace": strings.ReplaceAll(full, "/", " / "),
		"path": name, "path_with_namespace": full, "created_at": "2026-09-01T10:00:00.000Z",
		"default_branch": "main", "tag_list": []string{}, "topics": []string{},
		"ssh_url_to_repo":  "git@gitlab.example.com:" + full + ".git",
		"http_url_to_repo": "https://gitlab.example.com/" + full + ".git",
		"web_url":          "https://gitlab.example.com/" + full, "readme_url": nil, "forks_count": 0, "avatar_url": nil,
		"star_count": 0, "last_activity_at": "2026-09-02T10:00:00.000Z", "visibility": "private",
		"namespace": map[string]any{"id": 9, "name": ns, "path": ns, "kind": "group", "full_path": ns,
			"parent_id": nil, "avatar_url": nil, "web_url": "https://gitlab.example.com/groups/" + ns},
		"container_registry_image_prefix": "", "_links": map[string]any{},
		"packages_enabled": true, "empty_repo": false, "archived": false, "resolve_outdated_diff_discussions": false,
		"repository_object_format": "sha1", "issues_enabled": true, "merge_requests_enabled": true,
		"wiki_enabled": true, "jobs_enabled": true, "snippets_enabled": true, "container_registry_enabled": true,
		"service_desk_enabled": false, "can_create_merge_request_in": true, "issues_access_level": "enabled",
		"repository_access_level": "enabled", "merge_requests_access_level": "enabled",
		"forking_access_level": "enabled", "wiki_access_level": "enabled", "builds_access_level": "enabled",
		"snippets_access_level": "enabled", "pages_access_level": "private", "analytics_access_level": "enabled",
		"container_registry_access_level": "enabled", "security_and_compliance_access_level": "private",
		"releases_access_level": "enabled", "environments_access_level": "enabled",
		"feature_flags_access_level": "enabled", "infrastructure_access_level": "enabled",
		"monitor_access_level": "enabled", "model_experiments_access_level": "enabled",
		"model_registry_access_level": "enabled", "emails_disabled": false, "emails_enabled": true,
		"shared_runners_enabled": true, "lfs_enabled": true, "creator_id": 1, "import_status": "none",
		"open_issues_count": 0, "ci_default_git_depth": 20, "public_jobs": true, "shared_with_groups": []any{},
		"only_allow_merge_if_pipeline_succeeds": false, "request_access_enabled": true,
		"merge_method": "merge", "squash_option": "default_off", "marked_for_deletion_at": nil,
		"marked_for_deletion_on": nil, "compliance_frameworks": []any{},
		"permissions": map[string]any{
			"project_access": nil,
			"group_access":   map[string]any{"access_level": 20, "notification_level": 3},
		},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// cutLast splits "a/b/c" into "a/b" and "c".
func cutLast(full string) (string, string, bool) {
	i := strings.LastIndexByte(full, '/')
	if i < 0 {
		return "", full, false
	}
	return full[:i], full[i+1:], true
}

// mrSpec describes a merge request fixture.
type mrSpec struct {
	iid           int64
	projectID     int64 // target project, default 7
	sourceProject int64 // default the target
	author        map[string]any
	source        string
	target        string // default "main"
	state         string // opened (default), closed, merged, locked
	closedBy      map[string]any
	mergedBy      map[string]any
	labels        []string
	title         string
	description   string
	draft         bool
	closedAt      string
}

// mr is a merge request as the merge request APIs send it.
func mr(p mrSpec) map[string]any {
	target := cmpStr(p.target, "main")
	pid := p.projectID
	if pid == 0 {
		pid = 7
	}
	src := p.sourceProject
	if src == 0 {
		src = pid
	}
	state := cmpStr(p.state, "opened")
	var closedAt, mergedAt, closedBy, mergedBy any
	switch state {
	case "closed":
		closedAt, closedBy = cmpStr(p.closedAt, "2026-09-10T12:00:00.000Z"), p.closedBy
		if p.closedBy == nil {
			closedBy = nil
		}
	case "merged":
		mergedAt = cmpStr(p.closedAt, "2026-09-10T12:00:00.000Z")
		if p.mergedBy != nil {
			mergedBy = p.mergedBy
		}
	}
	labels := p.labels
	if labels == nil {
		labels = []string{}
	}
	title := cmpStr(p.title, "mr")
	if p.draft {
		title = "Draft: " + title
	}
	return map[string]any{
		"id": 1000 + p.iid, "iid": p.iid, "project_id": pid, "title": title, "description": p.description,
		"state": state, "created_at": "2026-09-05T08:00:00.000Z", "updated_at": "2026-09-10T12:00:00.000Z",
		"merged_by": mergedBy, "merge_user": mergedBy, "merged_at": mergedAt, "merge_after": nil,
		"prepared_at": "2026-09-05T08:00:01.000Z", "closed_by": closedBy, "closed_at": closedAt,
		"target_branch": target, "source_branch": p.source, "user_notes_count": 0, "upvotes": 0, "downvotes": 0,
		"author": p.author, "assignees": []any{}, "assignee": nil, "reviewers": []any{},
		"source_project_id": src, "target_project_id": pid, "labels": labels, "draft": p.draft,
		"work_in_progress": p.draft, "milestone": nil, "merge_when_pipeline_succeeds": false,
		"merge_status": "can_be_merged", "detailed_merge_status": "mergeable",
		"sha": strings.Repeat("a", 39) + strconv.FormatInt(p.iid%10, 10), "merge_commit_sha": nil,
		"squash_commit_sha": nil, "discussion_locked": nil, "should_remove_source_branch": nil,
		"force_remove_source_branch": false, "prepared_at_": nil, "reference": "!" + strconv.FormatInt(p.iid, 10),
		"references": map[string]any{"short": "!" + strconv.FormatInt(p.iid, 10)},
		"web_url":    fmt.Sprintf("https://gitlab.example.com/acme/api/-/merge_requests/%d", p.iid),
		"time_stats": map[string]any{"time_estimate": 0, "total_time_spent": 0},
		"squash":     false, "squash_on_merge": false, "task_completion_status": map[string]any{"count": 0, "completed_count": 0},
		"has_conflicts": false, "blocking_discussions_resolved": true, "approvals_before_merge": nil,
	}
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

// decode unmarshals a recorded JSON body.
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return m
}
