package gitea

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestRepo(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/repos/ACME/API", http.StatusOK, fx.repo(7, "acme/api", withField("topics", []string{"conformance", "python"})))
	got, err := fx.reader.Repo(t.Context(), "ACME/API")
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Repo{
		Host: fx.provider("gitea").Host, ID: "7", Path: "acme/api", DefaultBranch: "main",
		WebURL: fx.base() + "/acme/api", Visibility: "public", ObjectFormat: "sha1", Topics: []string{"conformance", "python"},
	}
	if got.Host != want.Host || got.ID != want.ID || got.Path != want.Path || got.DefaultBranch != want.DefaultBranch ||
		got.WebURL != want.WebURL || got.Visibility != want.Visibility || got.ObjectFormat != want.ObjectFormat ||
		!slices.Equal(got.Topics, want.Topics) || got.Archived || got.Disabled || got.Empty || got.Mirror || got.Fork ||
		got.PendingDelete || got.PRsDisabled {
		t.Errorf("Repo = %+v, want %+v", got, want)
	}
}

func TestRepoFlags(t *testing.T) {
	fx := newFixture(t, "forgejo")
	limited := fx.org("secret-org")
	limited["visibility"] = "limited"
	for _, tc := range []struct {
		name  string
		repo  map[string]any
		check func(platform.Repo) bool
	}{
		{"private", fx.repo(1, "acme/a", withField("private", true)), func(r platform.Repo) bool { return r.Visibility == "private" }},
		{"internal", fx.repo(2, "acme/b", withField("internal", true)), func(r platform.Repo) bool { return r.Visibility == "internal" }},
		{"limited owner", fx.repo(3, "secret-org/c", withField("owner", limited)), func(r platform.Repo) bool { return r.Visibility == "internal" }},
		{"archived", fx.repo(4, "acme/d", withField("archived", true)), func(r platform.Repo) bool { return r.Archived }},
		{"empty", fx.repo(5, "acme/e", withField("empty", true)), func(r platform.Repo) bool { return r.Empty }},
		{"mirror", fx.repo(6, "acme/f", withField("mirror", true)), func(r platform.Repo) bool { return r.Mirror }},
		{"fork", fx.repo(7, "acme/g", withField("fork", true)), func(r platform.Repo) bool { return r.Fork }},
		{"no pull requests", fx.repo(8, "acme/h", withField("has_pull_requests", false)), func(r platform.Repo) bool { return r.PRsDisabled }},
		{"no code", fx.repo(9, "acme/i", withField("has_code", false)), func(r platform.Repo) bool { return r.Disabled }},
		{"sha256", fx.repo(10, "acme/j", withField("object_format_name", "sha256")), func(r platform.Repo) bool { return r.ObjectFormat == "sha256" }},
		{"no object format", fx.repo(11, "acme/k", withField("object_format_name", nil)), func(r platform.Repo) bool { return r.ObjectFormat == "sha1" }},
		// Forgejo has no has_code: the code unit is taken to be on.
		{"no has_code", func() map[string]any { m := fx.repo(12, "acme/l"); delete(m, "has_code"); return m }(), func(r platform.Repo) bool { return !r.Disabled }},
	} {
		path := tc.repo["full_name"].(string)
		fx.json(http.MethodGet, "/repos/"+path, http.StatusOK, tc.repo)
		got, err := fx.reader.Repo(t.Context(), path)
		if err != nil || !tc.check(got) {
			t.Errorf("%s: Repo = %+v, %v", tc.name, got, err)
		}
	}
}

func TestRepoErrors(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/repos/acme/missing", http.StatusNotFound, fx.apiMsg("not found"))
	fx.json(http.MethodGet, "/repos/acme/broken", http.StatusOK, map[string]any{"id": 0, "full_name": ""})
	fx.json(http.MethodGet, "/repos/acme/typed", http.StatusOK, map[string]any{"id": "seven"})
	_, err := fx.reader.Repo(t.Context(), "acme/missing")
	wantClass(t, "Repo of a missing repository", err, platform.ClassNotFound, platform.ErrNotFound)
	for _, p := range []string{"", "acme", "group/sub/project", "acme/"} {
		_, err := fx.reader.Repo(t.Context(), p)
		wantClass(t, fmt.Sprintf("Repo(%q)", p), err, platform.ClassNotFound, platform.ErrNotFound)
	}
	_, err = fx.reader.Repo(t.Context(), "acme/broken")
	wantClass(t, "Repo without an id", err, platform.ClassUnknown, nil)
	_, err = fx.reader.Repo(t.Context(), "acme/typed")
	wantClass(t, "Repo with a string id", err, platform.ClassUnknown, nil)
}

func TestResolveNamespace(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.settings(2)
	items := []any{
		fx.repo(4, "acme/Zeta", withField("topics", []string{"python", "conformance"})),
		fx.repo(1, "acme/alpha", withField("topics", []string{"conformance", "python"})),
		fx.repo(2, "acme/beta", withField("topics", []string{"conformance"})),
		fx.repo(3, "acme/forked", withField("fork", true), withField("topics", []string{"python"})),
		fx.repo(5, "acme/gamma", withField("archived", true), withField("topics", []string{"PYTHON"})),
	}
	fx.pages("/orgs/acme/repos", items)
	for _, tc := range []struct {
		name string
		sel  platform.Selector
		want []string
	}{
		{"all", platform.Selector{Namespace: "acme"}, []string{"acme/alpha", "acme/beta", "acme/gamma", "acme/Zeta"}},
		{"forks", platform.Selector{Namespace: "acme", Forks: true}, []string{"acme/alpha", "acme/beta", "acme/forked", "acme/gamma", "acme/Zeta"}},
		{"one topic", platform.Selector{Namespace: "acme", Topics: []string{"python"}}, []string{"acme/alpha", "acme/gamma", "acme/Zeta"}},
		{"all topics", platform.Selector{Namespace: "acme", Topics: []string{"Python", "conformance"}}, []string{"acme/alpha", "acme/Zeta"}},
		{"a missing topic", platform.Selector{Namespace: "acme", Topics: []string{"python", "nope"}}, nil},
		// No nested namespaces: Subgroups changes nothing.
		{"subgroups", platform.Selector{Namespace: "acme", Subgroups: true}, []string{"acme/alpha", "acme/beta", "acme/gamma", "acme/Zeta"}},
	} {
		res, err := fx.reader.Resolve(t.Context(), tc.sel)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []string
		for _, r := range res.Repos {
			got = append(got, r.Path)
		}
		if !slices.Equal(got, tc.want) || !res.Complete {
			t.Errorf("%s: Resolve = %v (complete %v), want %v", tc.name, got, res.Complete, tc.want)
		}
	}
	// Pages of 2 (the instance's MAX_RESPONSE_ITEMS): three requests a listing.
	for _, c := range fx.requests(http.MethodGet, "/orgs/acme/repos") {
		if c.Query.Get("limit") != "2" {
			t.Errorf("limit %q, want the instance's 2", c.Query.Get("limit"))
		}
	}
}

func TestResolveUserNamespace(t *testing.T) {
	fx := newFixture(t, "forgejo")
	fx.settings(50)
	fx.json(http.MethodGet, "/orgs/jdoe/repos", http.StatusNotFound, fx.apiMsg("GetOrgByName"))
	fx.pages("/users/jdoe/repos", []any{fx.repo(9, "jdoe/tools")})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "jdoe"})
	if err != nil || len(res.Repos) != 1 || res.Repos[0].Path != "jdoe/tools" || !res.Complete {
		t.Errorf("Resolve of a user = %+v, %v", res, err)
	}
	fx.json(http.MethodGet, "/orgs/nobody/repos", http.StatusNotFound, fx.apiMsg("GetOrgByName"))
	fx.json(http.MethodGet, "/users/nobody/repos", http.StatusNotFound, fx.apiMsg("user redirect does not exist [name: nobody]"))
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "nobody"})
	wantClass(t, "Resolve of a missing namespace", err, platform.ClassNotFound, platform.ErrNotFound)
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme/sub"})
	wantClass(t, "Resolve of a nested namespace", err, platform.ClassNotFound, platform.ErrNotFound)
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{})
	wantClass(t, "Resolve of nothing", err, platform.ClassInvalid, nil)
}

func TestResolveRepoSelector(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/repos/acme/forked", http.StatusOK, fx.repo(3, "acme/forked", withField("fork", true)))
	fx.json(http.MethodGet, "/repos/acme/gone", http.StatusNotFound, fx.apiMsg("not found"))
	// A Repo selector ignores the rest and never skips a fork.
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Repo: "acme/forked", Namespace: "elsewhere", Topics: []string{"x"}})
	if err != nil || len(res.Repos) != 1 || !res.Repos[0].Fork || !res.Complete {
		t.Errorf("Resolve repo = %+v, %v", res, err)
	}
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Repo: "acme/gone"})
	wantClass(t, "Resolve of a missing repository", err, platform.ClassNotFound, platform.ErrNotFound)
}

// A listing without topics gets them per repository when topics matter.
func TestResolveTopicsFallback(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.settings(50)
	noTopics := func(id int64, name string) map[string]any {
		m := fx.repo(id, name)
		delete(m, "topics")
		return m
	}
	fx.pages("/orgs/acme/repos", []any{noTopics(1, "acme/a"), noTopics(2, "acme/b"), fx.repo(3, "acme/c")})
	fx.json(http.MethodGet, "/repos/acme/a/topics", http.StatusOK, map[string]any{"topics": []string{"python"}})
	fx.json(http.MethodGet, "/repos/acme/b/topics", http.StatusOK, map[string]any{"topics": []string{"go"}})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"python"}})
	if err != nil || len(res.Repos) != 1 || res.Repos[0].Path != "acme/a" || !slices.Equal(res.Repos[0].Topics, []string{"python"}) {
		t.Errorf("Resolve = %+v, %v", res, err)
	}
	// acme/c lists its (empty) topics: no request for it.
	if n := len(fx.requests(http.MethodGet, "/repos/acme/c/topics")); n != 0 {
		t.Errorf("topics of acme/c asked %d times", n)
	}
	// Topics that cannot be read leave their repository out and make the
	// resolve incomplete; a refused credential or a rate limit fails it.
	fx.json(http.MethodGet, "/repos/acme/b/topics", http.StatusNotFound, fx.apiMsg("not found"))
	res, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"python"}})
	if err != nil || res.Complete || len(res.Repos) != 1 {
		t.Errorf("Resolve with topics that vanished = %+v, %v; want acme/a, incomplete", res, err)
	}
	fx.json(http.MethodGet, "/repos/acme/b/topics", http.StatusUnauthorized, fx.apiMsg("token is required"))
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"python"}})
	wantClass(t, "Resolve with topics refused to the credential", err, platform.ClassAuth, nil)
	fx.handle(http.MethodGet, "/repos/acme/b/topics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		writeJSON(w, http.StatusTooManyRequests, fx.apiMsg("slow down"))
	})
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"python"}})
	wantClass(t, "Resolve with rate-limited topics", err, platform.ClassRateLimited, nil)
}

func TestResolveIncomplete(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.settings(1)
	calls := 0
	fx.handle(http.MethodGet, "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusForbidden, fx.apiMsg("forbidden"))
			return
		}
		servePage(w, r, []any{fx.repo(1, "acme/a"), fx.repo(2, "acme/b")}, linkAndTotal)
	})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	if err != nil || res.Complete || len(res.Repos) != 1 {
		t.Errorf("a failed later page: %+v, %v; want acme/a, incomplete", res, err)
	}
	// A rate limit, a refused credential or a transient failure on a later
	// page fails the call with its class and wait: the core pauses the
	// provider, stops it or retries the listing.
	for _, tc := range []struct {
		status int
		class  platform.Class
		retry  time.Duration
	}{
		{http.StatusTooManyRequests, platform.ClassRateLimited, 7 * time.Second},
		{http.StatusUnauthorized, platform.ClassAuth, 0},
		{http.StatusBadGateway, platform.ClassTransient, 0},
		{http.StatusServiceUnavailable, platform.ClassTransient, 7 * time.Second},
	} {
		fx.handle(http.MethodGet, "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.Header().Set("Retry-After", "7")
				writeJSON(w, tc.status, fx.apiMsg("slow down"))
				return
			}
			servePage(w, r, []any{fx.repo(1, "acme/a"), fx.repo(2, "acme/b")}, linkAndTotal)
		})
		res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
		var pe *platform.Error
		switch {
		case err == nil:
			t.Errorf("status %d on page 2: %+v without an error", tc.status, res)
		case !errors.As(err, &pe) || platform.ClassOf(err) != tc.class || pe.Status != tc.status || pe.RetryAfter != tc.retry:
			t.Errorf("status %d on page 2: %v (class %v), want class %v, retry after %v", tc.status, err, platform.ClassOf(err), tc.class, tc.retry)
		case !strings.HasPrefix(err.Error(), "page 2: "):
			t.Errorf("status %d on page 2: %q does not name the page", tc.status, err)
		}
	}
	// A listing that never ends is capped.
	fx.handle(http.MethodGet, "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v1/orgs/acme/repos?page=999>; rel="next"`, r.Host))
		writeJSON(w, http.StatusOK, []any{fx.repo(1, "acme/a")})
	})
	fx.reset()
	res, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	if err != nil || res.Complete || len(res.Repos) != 1 {
		t.Errorf("an endless listing: %+v, %v; want one repository, incomplete", res, err)
	}
	if n := len(fx.requests(http.MethodGet, "/orgs/acme/repos")); n != maxRepoPages {
		t.Errorf("an endless listing took %d requests, want %d", n, maxRepoPages)
	}
}

// Pages that permission filtering shortened do not end a listing: the Link
// header does.
func TestResolveShortPagesFollowLink(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.settings(3)
	fx.handle(http.MethodGet, "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v1/orgs/acme/repos?limit=3&page=2>; rel="next",<http://%s/api/v1/orgs/acme/repos?limit=3&page=2>; rel="last"`, r.Host, r.Host))
			w.Header().Set("X-Total-Count", "6")
			writeJSON(w, http.StatusOK, []any{fx.repo(1, "acme/a")}) // two hidden
		default:
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v1/orgs/acme/repos?limit=3&page=1>; rel="first"`, r.Host))
			w.Header().Set("X-Total-Count", "6")
			writeJSON(w, http.StatusOK, []any{fx.repo(2, "acme/b")})
		}
	})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	if err != nil || len(res.Repos) != 2 || !res.Complete {
		t.Errorf("Resolve = %+v, %v; want both, complete", res, err)
	}
}

func TestRemote(t *testing.T) {
	fx := newFixture(t, "gitea")
	repo := platform.Repo{Host: fx.provider("gitea").Host, ID: "7", Path: "acme/api"}
	rem, err := fx.reader.Remote(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL != fx.base()+"/acme/api.git" || strings.Contains(rem.URL, fx.token) {
		t.Errorf("Remote URL %q", rem.URL)
	}
	header, err := rem.Header(t.Context())
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fx.token))
	if err != nil || header != want {
		t.Errorf("Remote header = %q, %v", header, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rem.Header(canceled); err == nil {
		t.Error("the header of a canceled context")
	}
	if len(fx.requests("", "")) != 0 {
		t.Errorf("Remote made %d requests", len(fx.requests("", "")))
	}
	_, err = fx.reader.Remote(t.Context(), platform.Repo{Host: "elsewhere.example.com", ID: "7", Path: "acme/api"})
	wantClass(t, "Remote of another host", err, platform.ClassInvalid, nil)

	anon, err := NewReader(fx.provider("forgejo"), auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	rem, err = anon.Remote(t.Context(), repo)
	if err != nil || rem.Header != nil || rem.URL != fx.base()+"/acme/api.git" {
		t.Errorf("anonymous Remote = %+v, %v", rem, err)
	}
}

// redirect declares a route that redirects with status to location, as
// the servers answer the old path of a renamed repository (301), user or
// organization (307): a path on the same host, the query kept.
func (s *apiServer) redirect(method, path string, status int, location string) {
	s.handle(method, path, func(w http.ResponseWriter, r *http.Request) {
		loc := location
		if r.URL.RawQuery != "" && !strings.Contains(loc, "?") {
			loc += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(status)
	})
}

// The old path of a renamed repository leads to it under its canonical
// path, by Repo and by a Repo selector, with the same credential: Gitea
// and Forgejo answer it with 301 to the new path (checked on Gitea 1.26
// and 1.27 and Forgejo 15 and 16).
func TestRepoRenamed(t *testing.T) {
	for _, typ := range []string{"gitea", "forgejo"} {
		t.Run(typ, func(t *testing.T) {
			fx := newFixture(t, typ)
			fx.redirect(http.MethodGet, "/repos/acme/old-name", http.StatusMovedPermanently, "/api/v1/repos/acme/new-name")
			fx.json(http.MethodGet, "/repos/acme/new-name", http.StatusOK, fx.repo(7, "acme/new-name"))
			got, err := fx.reader.Repo(t.Context(), "acme/old-name")
			if err != nil || got.ID != "7" || got.Path != "acme/new-name" {
				t.Errorf("Repo of the old path = %+v, %v; want acme/new-name", got, err)
			}
			res, err := fx.reader.Resolve(t.Context(), platform.Selector{Repo: "acme/old-name"})
			if err != nil || len(res.Repos) != 1 || res.Repos[0].Path != "acme/new-name" || !res.Complete {
				t.Errorf("Resolve of the old path = %+v, %v", res, err)
			}
			for _, c := range fx.requests(http.MethodGet, "") {
				if strings.HasPrefix(c.Path, "/api/v1/repos/") && c.Auth != "token "+fx.token {
					t.Errorf("%s: Authorization %q, want the token", c.Path, c.Auth)
				}
			}
			if n := len(fx.requests(http.MethodGet, "/repos/acme/new-name")); n != 2 {
				t.Errorf("%d requests of the new path, want 2", n)
			}
		})
	}
}

// A renamed login leads to its account (307), whose id stays: the writer
// named by an old login in hub.yml is still the writer.
func TestLookupRenamed(t *testing.T) {
	fx := newFixture(t, "forgejo")
	fx.redirect(http.MethodGet, "/users/old-bot", http.StatusTemporaryRedirect, "/api/v1/users/new-bot")
	fx.json(http.MethodGet, "/users/new-bot", http.StatusOK, fx.user(3, "new-bot"))
	a, err := fx.reader.Lookup(t.Context(), "old-bot")
	if err != nil || a.ID != "3" || a.Login != "new-bot" {
		t.Errorf("Lookup of the old login = %+v, %v; want id 3, new-bot", a, err)
	}
}

// A renamed organization's old name lists its repositories (307 on every
// page, the query kept).
func TestResolveRenamedNamespace(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.settings(2)
	fx.redirect(http.MethodGet, "/orgs/old-org/repos", http.StatusTemporaryRedirect, "/api/v1/orgs/new-org/repos")
	fx.pages("/orgs/new-org/repos", []any{fx.repo(1, "new-org/a"), fx.repo(2, "new-org/b"), fx.repo(3, "new-org/c")})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "old-org"})
	var got []string
	for _, r := range res.Repos {
		got = append(got, r.Path)
	}
	if err != nil || !res.Complete || !slices.Equal(got, []string{"new-org/a", "new-org/b", "new-org/c"}) {
		t.Errorf("Resolve of the old name = %v (complete %v), %v", got, res.Complete, err)
	}
	pages := map[string]bool{}
	for _, c := range fx.requests(http.MethodGet, "/orgs/new-org/repos") {
		pages[c.Query.Get("page")] = true
		if c.Query.Get("limit") != "2" {
			t.Errorf("the redirect lost the query: %v", c.Query)
		}
	}
	if !pages["1"] || !pages["2"] {
		t.Errorf("pages read %v, want 1 and 2", pages)
	}
}

// Redirects that are not followed stay errors: one to another host (the
// credential never leaves the provider), one out of the API (a login
// page), one of a write (never repeated elsewhere), and a loop, after a
// few hops.
func TestRedirectsNotFollowed(t *testing.T) {
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		writeJSON(w, http.StatusOK, map[string]any{"id": 7, "full_name": "acme/api", "default_branch": "main"})
	}))
	t.Cleanup(other.Close)
	fx := newFixture(t, "gitea")
	fx.redirect(http.MethodGet, "/repos/acme/away", http.StatusMovedPermanently, other.URL+"/api/v1/repos/acme/api")
	fx.redirect(http.MethodGet, "/repos/acme/login", http.StatusFound, "/user/login")
	fx.redirect(http.MethodGet, "/repos/acme/userinfo", http.StatusMovedPermanently,
		"http://someone:secret@"+strings.TrimPrefix(fx.base(), "http://")+"/api/v1/repos/acme/api")
	fx.redirect(http.MethodGet, "/repos/acme/loop", http.StatusMovedPermanently, "/api/v1/repos/acme/loop")
	for _, p := range []string{"acme/away", "acme/login", "acme/userinfo", "acme/loop"} {
		_, err := fx.reader.Repo(t.Context(), p)
		wantClass(t, "Repo("+p+")", err, platform.ClassUnknown, nil)
		if err != nil && strings.Contains(err.Error(), fx.token) {
			t.Errorf("Repo(%s): the error holds the token", p)
		}
	}
	if len(elsewhere) != 0 {
		t.Errorf("requests to another host: %q", elsewhere)
	}
	if n := len(fx.requests(http.MethodGet, "/repos/acme/loop")); n != maxRedirects+1 {
		t.Errorf("the loop was requested %d times, want %d", n, maxRedirects+1)
	}
	// A write is not repeated at the new path.
	fx.redirect(http.MethodPost, "/repos/acme/old-name/issues/1/comments", http.StatusTemporaryRedirect, "/api/v1/repos/acme/new-name/issues/1/comments")
	_, err := fx.reader.c.call(t.Context(), "comment", http.MethodPost, fx.reader.c.endpoint("repos", "acme", "old-name", "issues", "1", "comments"),
		nil, map[string]string{"body": "x"}, nil)
	wantClass(t, "a redirected POST", err, platform.ClassUnknown, nil)
	if n := len(fx.requests(http.MethodPost, "/repos/acme/new-name/issues/1/comments")); n != 0 {
		t.Errorf("the POST was repeated at the new path %d times", n)
	}
}
