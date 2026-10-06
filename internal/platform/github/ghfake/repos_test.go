package ghfake

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRepoObject(t *testing.T) {
	w := newWorld(t, Options{})
	w.repo(RepoSpec{Owner: "acme", Name: "tools", Topics: []string{"engineering", "go"}, Archived: true,
		PRsDisabled: true, Template: true, Files: []File{{Path: "x", Content: []byte("x")}}})
	w.repo(RepoSpec{Owner: "acme", Name: "empty"})
	tok := w.token(nil, nil)
	got := w.call("GET", "/repos/ACME/Tools", tok, nil).obj(t)
	for k, want := range map[string]any{
		"full_name": "acme/tools", "archived": true, "has_pull_requests": false, "is_template": true,
		"visibility": "public", "private": false, "default_branch": "main", "fork": false, "disabled": false,
		"pull_request_creation_policy": "all", "clone_url": w.s.CloneURL("acme/tools"),
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if field(got, "topics", 1) != "go" || field(got, "owner", "type") != "Organization" || field(got, "permissions", "push") != true {
		t.Errorf("repository: %v", got)
	}
	if got := w.call("GET", "/repos/acme/empty", tok, nil).obj(t); got["size"] != float64(0) {
		t.Errorf("empty repository size %v", got["size"])
	}
	if got := w.call("GET", "/repos/acme/tools/topics", tok, nil).obj(t); field(got, "names", 0) != "engineering" {
		t.Errorf("topics: %v", got)
	}
	wantStatus(t, "by id", w.call("GET", "/repositories/"+itoa(w.api.ID), "", nil), 200)
}

func TestVisibility(t *testing.T) {
	w := newWorld(t, Options{})
	w.repo(RepoSpec{Owner: "acme", Name: "secret", Visibility: "private"})
	r := w.call("GET", "/repos/acme/secret", "", nil)
	wantStatus(t, "anonymous private", r, 404)
	if got := r.obj(t); got["message"] != "Not Found" || got["status"] != "404" || got["documentation_url"] == nil {
		t.Errorf("404 body: %v", got)
	}
	wantStatus(t, "anonymous public", w.call("GET", "/repos/acme/api", "", nil), 200)
	wantStatus(t, "installation private", w.call("GET", "/repos/acme/secret", w.token(nil, nil), nil), 200)
}

func TestRenames(t *testing.T) {
	w := newWorld(t, Options{})
	must[Repo](t)(w.s.RenameRepo("acme/api", "service"))
	r := w.call("GET", "/repos/acme/api/pulls?state=all", "", nil)
	wantStatus(t, "old path", r, http.StatusMovedPermanently)
	loc := w.s.URL() + "/repositories/" + itoa(w.api.ID) + "/pulls?state=all"
	if r.header.Get("Location") != loc || r.obj(t)["url"] != loc || r.obj(t)["message"] != "Moved Permanently" {
		t.Errorf("redirect: %s %v", r.header.Get("Location"), r.obj(t))
	}
	wantStatus(t, "follow", w.call("GET", strings.TrimPrefix(loc, w.s.URL()), "", nil), 200)
	r = w.call("POST", "/repos/acme/api/labels", w.token(nil, nil), map[string]any{"name": "x"})
	wantStatus(t, "old path, POST", r, http.StatusTemporaryRedirect)
	// GHES layout: the same under /api/v3.
	r = doRequest(t, w.s, "GET", w.s.EnterpriseAPIURL()+"/repos/acme/api", "", nil)
	wantStatus(t, "old path under /api/v3", r, http.StatusMovedPermanently)
	if want := w.s.EnterpriseAPIURL() + "/repositories/" + itoa(w.api.ID); r.header.Get("Location") != want {
		t.Errorf("Location %q, want %q", r.header.Get("Location"), want)
	}
	// A renamed account: the old login is gone, and so are the old paths of
	// its repositories in the API (docs); git still reaches them there.
	must[Account](t)(w.s.RenameAccount("acme", "acme-corp"))
	wantStatus(t, "old login", w.call("GET", "/users/acme", "", nil), 404)
	wantStatus(t, "new login", w.call("GET", "/users/acme-corp", "", nil), 200)
	wantStatus(t, "old owner path", w.call("GET", "/repos/acme/service", "", nil), http.StatusNotFound)
	if got := w.call("GET", "/repositories/"+itoa(w.api.ID), "", nil).obj(t); got["full_name"] != "acme-corp/service" {
		t.Errorf("renamed: %v", got["full_name"])
	}
}

func TestUsers(t *testing.T) {
	w := newWorld(t, Options{})
	got := w.call("GET", "/users/hub-writer%5Bbot%5D", "", nil).obj(t)
	if got["type"] != "Bot" || got["login"] != "hub-writer[bot]" || got["id"] != float64(w.app.Bot.ID) {
		t.Errorf("bot: %v", got)
	}
	if !strings.HasSuffix(got["html_url"].(string), "/apps/hub-writer") {
		t.Errorf("bot html_url %v", got["html_url"])
	}
	if w.app.Bot.Email != itoa(w.app.Bot.ID)+"+hub-writer[bot]@users.noreply.github.com" {
		t.Errorf("bot email %q", w.app.Bot.Email)
	}
	if got := w.call("GET", "/users/acme", "", nil).obj(t); got["type"] != "Organization" {
		t.Errorf("org as user: %v", got)
	}
	wantStatus(t, "org", w.call("GET", "/orgs/acme", "", nil), 200)
	wantStatus(t, "user as org", w.call("GET", "/orgs/alice", "", nil), 404)
}

func TestOrgRepos(t *testing.T) {
	w := newWorld(t, Options{})
	for _, n := range []string{"b", "c", "d"} {
		w.repo(RepoSpec{Owner: "acme", Name: n})
	}
	w.repo(RepoSpec{Owner: "acme", Name: "hidden", Visibility: "private"})
	must[Repo](t)(w.s.Fork("acme/api", "alice"))
	r := w.call("GET", "/orgs/acme/repos?per_page=2&page=2", "", nil)
	wantStatus(t, "page 2", r, 200)
	names := func(r reply) []string {
		var out []string
		for _, item := range r.list(t) {
			out = append(out, field(item, "name").(string))
		}
		return out
	}
	// Created, newest first; the private one is invisible.
	if got := names(r); strings.Join(got, ",") != "b,api" {
		t.Errorf("page 2: %v", got)
	}
	link := r.header.Get("Link")
	base := w.s.URL() + "/organizations/"
	if !strings.HasPrefix(link, "<"+base) || !strings.Contains(link, `page=1>; rel="prev"`) ||
		!strings.Contains(link, `rel="first"`) || strings.Contains(link, `rel="next"`) {
		t.Errorf("Link %q", link)
	}
	if got := names(w.call("GET", "/orgs/acme/repos?sort=full_name", w.token(nil, nil), nil)); strings.Join(got, ",") != "api,b,c,d,hidden" {
		t.Errorf("by name with a token: %v", got)
	}
	if got := names(w.call("GET", "/users/alice/repos?type=forks", "", nil)); strings.Join(got, ",") != "api" {
		t.Errorf("forks: %v", got)
	}
	fork := w.call("GET", "/repos/alice/api", "", nil).obj(t)
	if fork["fork"] != true || field(fork, "parent", "full_name") != "acme/api" || field(fork, "source", "full_name") != "acme/api" {
		t.Errorf("fork: %v", fork)
	}
}

func TestHashAlgorithm(t *testing.T) {
	w := newWorld(t, Options{})
	w.repo(RepoSpec{Owner: "acme", Name: "new", ObjectFormat: "sha256"})
	if got := w.call("GET", "/repos/acme/api/hash-algorithm", "", nil).obj(t); got["hash_algorithm"] != "sha1" {
		t.Errorf("sha1: %v", got)
	}
	if got := w.call("GET", "/repos/acme/new/hash-algorithm", "", nil).obj(t); got["hash_algorithm"] != "sha256" {
		t.Errorf("sha256: %v", got)
	}
	g := newWorld(t, Options{Flavor: GHES})
	wantStatus(t, "GHES", g.call("GET", "/api/v3/repos/acme/api/hash-algorithm", "", nil), 404)
}

func TestAPIVersions(t *testing.T) {
	w := newWorld(t, Options{})
	r := w.call("GET", "/repos/acme/api", "", nil, "X-GitHub-Api-Version", "2026-03-10")
	wantStatus(t, "2026-03-10", r, 200)
	if r.header.Get("X-Github-Api-Version-Selected") != "2026-03-10" {
		t.Errorf("selected %q", r.header.Get("X-Github-Api-Version-Selected"))
	}
	wantStatus(t, "unknown", w.call("GET", "/repos/acme/api", "", nil, "X-GitHub-Api-Version", "2020-01-01"), 400)
	g := newWorld(t, Options{Flavor: GHES})
	wantStatus(t, "GHES 2026", g.call("GET", "/api/v3/repos/acme/api", "", nil, "X-GitHub-Api-Version", "2026-03-10"), 400)
}

func TestPrimaryLimit(t *testing.T) {
	w := newWorld(t, Options{Limits: &Limits{InstallationPerHour: 3, AnonymousPerHour: 1}})
	tok := w.token(nil, nil)
	for i := range 3 {
		r := w.call("GET", "/repos/acme/api", tok, nil)
		wantStatus(t, "request", r, 200)
		if got := r.header.Get("X-Ratelimit-Remaining"); got != itoa(int64(2-i)) {
			t.Errorf("remaining %q after %d requests", got, i+1)
		}
		if r.header.Get("X-Ratelimit-Resource") != "core" || r.header.Get("X-Ratelimit-Limit") != "3" {
			t.Errorf("headers %v", r.header)
		}
	}
	r := w.call("GET", "/repos/acme/api", tok, nil)
	wantStatus(t, "over the limit", r, 403)
	if msg := r.obj(t)["message"]; msg != "API rate limit exceeded for installation ID "+itoa(w.inst.ID)+"." {
		t.Errorf("message %q", msg)
	}
	if r.header.Get("X-Ratelimit-Remaining") != "0" {
		t.Errorf("remaining %q", r.header.Get("X-Ratelimit-Remaining"))
	}
	// GET /rate_limit costs nothing.
	rl := w.call("GET", "/rate_limit", tok, nil)
	wantStatus(t, "rate_limit", rl, 200)
	if field(rl.obj(t), "resources", "core", "remaining") != float64(0) {
		t.Errorf("rate_limit: %s", rl.body)
	}
	// The window resets after an hour; anonymous callers have their own.
	w.clock.Advance(time.Hour)
	wantStatus(t, "next hour", w.call("GET", "/repos/acme/api", w.token(nil, nil), nil), 200)
	wantStatus(t, "anonymous", w.call("GET", "/repos/acme/api", "", nil), 200)
	wantStatus(t, "anonymous again", w.call("GET", "/repos/acme/api", "", nil), 403)
}

func TestSecondaryLimit(t *testing.T) {
	w := newWorld(t, Options{Limits: &Limits{RESTPointsPerMinute: 11}})
	tok := w.token(nil, nil)
	wantStatus(t, "POST", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "a"}), 201)
	wantStatus(t, "POST", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "b"}), 201)
	wantStatus(t, "GET", w.call("GET", "/repos/acme/api", tok, nil), 200)
	r := w.call("GET", "/repos/acme/api", tok, nil)
	wantStatus(t, "over 11 points", r, 403)
	if r.header.Get("Retry-After") != "60" || !strings.HasPrefix(r.obj(t)["message"].(string), "You have exceeded a secondary rate limit") {
		t.Errorf("secondary: %v %s", r.header, r.body)
	}
	w.clock.Advance(time.Minute)
	wantStatus(t, "a minute later", w.call("GET", "/repos/acme/api", tok, nil), 200)
}

func TestContentCreationLimit(t *testing.T) {
	w := newWorld(t, Options{Limits: &Limits{ContentPerMinute: 2, ContentPerHour: 3}})
	tok := w.token(nil, nil)
	for _, n := range []string{"a", "b"} {
		wantStatus(t, "label "+n, w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": n}), 201)
	}
	r := w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "c"})
	wantStatus(t, "third in a minute", r, 403)
	if r.obj(t)["message"] != contentMessage || r.header.Get("Retry-After") == "" {
		t.Errorf("content limit: %v %s", r.header, r.body)
	}
	w.clock.Advance(time.Minute)
	wantStatus(t, "next minute", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "c"}), 201)
	w.clock.Advance(time.Minute)
	r = w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "d"})
	wantStatus(t, "fourth in an hour", r, 403)
	if u := w.s.Usage("installation/" + itoa(w.inst.ID)); u.ContentCreated != 3 {
		t.Errorf("usage %+v", u)
	}
}

func TestGHESLimitsOff(t *testing.T) {
	w := newWorld(t, Options{Flavor: GHES})
	r := w.call("GET", "/api/v3/repos/acme/api", "", nil)
	wantStatus(t, "GHES", r, 200)
	if r.header.Get("X-Ratelimit-Limit") != "" {
		t.Errorf("GHES sends rate limit headers: %v", r.header)
	}
}

func TestFaults(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token(nil, nil)
	w.s.Fail("POST /repos/{owner}/{repo}/labels", Fault{Status: 502, Times: 2})
	for range 2 {
		wantStatus(t, "faulted", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "a"}), 502)
	}
	wantStatus(t, "after the faults", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "a"}), 201)
	// An applied fault takes effect and fails anyway.
	w.s.Fail("POST /repos/{owner}/{repo}/labels", Fault{Status: 500, Applied: true})
	wantStatus(t, "applied", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "b"}), 500)
	wantStatus(t, "label exists", w.call("GET", "/repos/acme/api/labels/b", tok, nil), 200)
	// A route through /repositories/{id} takes the faults of its /repos form.
	w.s.Fail("GET /repos/{owner}/{repo}/labels", Fault{Status: 429, RetryAfter: 30 * time.Second})
	r := w.call("GET", "/repositories/"+itoa(w.api.ID)+"/labels", tok, nil)
	wantStatus(t, "alias", r, 429)
	if r.header.Get("Retry-After") != "30" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	var routes []string
	for _, req := range w.s.Requests() {
		routes = append(routes, req.Route)
	}
	if !strings.Contains(strings.Join(routes, "\n"), "GET /repos/{owner}/{repo}/labels/{name}") {
		t.Errorf("requests %q", routes)
	}
}
