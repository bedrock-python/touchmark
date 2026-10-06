package github

import (
	"encoding/base64"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestRepo: the flags of a repository, its object format and emptiness,
// on github.com.
func TestRepo(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(101, "acme/api",
		withField("visibility", "internal"), withField("private", true), withField("archived", true),
		withField("has_pull_requests", false), withField("topics", []string{"Python", "sync"}),
		withField("mirror_url", "https://example.com/upstream.git")))
	f.json(http.MethodGet, "/repos/acme/api/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha256"})
	r, err := f.reader.Repo(t.Context(), "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Repo{Host: "github.com", ID: "101", Path: "acme/api", DefaultBranch: "main", WebURL: "https://github.com/acme/api",
		Visibility: "internal", ObjectFormat: "sha256", Archived: true, Mirror: true, PRsDisabled: true, Topics: []string{"Python", "sync"}}
	if !reposEqual(r, want) {
		t.Errorf("Repo = %+v\nwant %+v", r, want)
	}
	for _, c := range f.requests(http.MethodGet, "/repos/acme/api") {
		if m, ok := f.tokenOf(c.Auth); !ok || m.installation != instAcme {
			t.Errorf("read with %v", m)
		}
	}
	// The object format is asked once per repository and run.
	if _, err := f.reader.Repo(t.Context(), "acme/api"); err != nil || len(f.requests(http.MethodGet, "/repos/acme/api/hash-algorithm")) != 1 {
		t.Errorf("asked the object format again: %v", err)
	}

	// Size 0: the default branch tells an empty repository.
	f.json(http.MethodGet, "/repos/acme/empty", http.StatusOK, repo(102, "acme/empty", withField("size", 0)))
	f.json(http.MethodGet, "/repos/acme/empty/branches/main", http.StatusNotFound, ghError("Branch not found"))
	f.json(http.MethodGet, "/repos/acme/empty/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	f.json(http.MethodGet, "/repos/acme/fresh", http.StatusOK, repo(103, "acme/fresh", withField("size", 0)))
	f.json(http.MethodGet, "/repos/acme/fresh/branches/main", http.StatusOK, map[string]any{"name": "main"})
	f.json(http.MethodGet, "/repos/acme/fresh/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	for name, empty := range map[string]bool{"acme/empty": true, "acme/fresh": false} {
		r, err := f.reader.Repo(t.Context(), name)
		if err != nil || r.Empty != empty || r.ObjectFormat != "sha1" || r.Visibility != "public" {
			t.Errorf("%s: %+v, %v", name, r, err)
		}
	}

	f.json(http.MethodGet, "/repos/acme/gone", http.StatusNotFound, notFoundBody)
	_, err = f.reader.Repo(t.Context(), "acme/gone")
	wantClass(t, "missing", err, platform.ClassNotFound, platform.ErrNotFound)
	for _, p := range []string{"acme", "acme/a/b", "", "../x"} {
		_, err := f.reader.Repo(t.Context(), p)
		wantClass(t, "path "+p, err, platform.ClassNotFound, platform.ErrNotFound)
	}
	f.json(http.MethodGet, "/orgs/nobody/installation", http.StatusNotFound, notFoundBody)
	f.json(http.MethodGet, "/users/nobody/installation", http.StatusNotFound, notFoundBody)
	f.json(http.MethodGet, "/repos/nobody/x/installation", http.StatusNotFound, notFoundBody)
	_, err = f.reader.Repo(t.Context(), "nobody/x")
	wantClass(t, "an owner without the App", err, platform.ClassNotFound, platform.ErrNotFound)

	// A repository transferred from an owner without the App to acme: its
	// installation is found through the old path, which redirects, and it
	// is read with acme's token under its canonical path; the old owner is
	// not taken for acme.
	f.json(http.MethodGet, "/orgs/octo-org/installation", http.StatusNotFound, notFoundBody)
	f.json(http.MethodGet, "/users/octo-org/installation", http.StatusNotFound, notFoundBody)
	f.handle(http.MethodGet, "/repos/octo-org/moved/installation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+r.Host+"/api/v3/repositories/104/installation")
		writeJSON(w, http.StatusMovedPermanently, map[string]any{"message": "Moved Permanently"})
	})
	f.handle(http.MethodGet, "/repositories/104/installation", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, installation(instAcme, "acme", "Organization", nil))
	}))
	f.handle(http.MethodGet, "/repos/octo-org/moved", func(w http.ResponseWriter, r *http.Request) {
		if m, ok := f.tokenOf(r.Header.Get("Authorization")); !ok || m.installation != instAcme {
			t.Errorf("the transferred repository was read with %+v", m)
		}
		w.Header().Set("Location", "http://"+r.Host+"/api/v3/repositories/104")
		writeJSON(w, http.StatusMovedPermanently, map[string]any{"message": "Moved Permanently"})
	})
	f.json(http.MethodGet, "/repositories/104", http.StatusOK, repo(104, "acme/moved"))
	f.json(http.MethodGet, "/repos/acme/moved/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	r, err = f.reader.Repo(t.Context(), "octo-org/moved")
	if err != nil || r.Path != "acme/moved" || r.ID != "104" {
		t.Errorf("a transferred repository: %+v, %v", r, err)
	}
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "octo-org"})
	wantClass(t, "the old owner after the transfer", err, platform.ClassNotFound, platform.ErrNotFound)
}

// TestRepoGHESHashAlgorithm: GHES 3.19 has no /hash-algorithm: its 404
// means SHA-1, and is not asked again in the run.
func TestRepoGHESHashAlgorithm(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken})
	for _, name := range []string{"acme/a", "acme/b"} {
		f.json(http.MethodGet, "/repos/"+name, http.StatusOK, repo(int64(200+len(name)+strings.Index(name, "b")), name))
		f.json(http.MethodGet, "/repos/"+name+"/hash-algorithm", http.StatusNotFound, notFoundBody)
	}
	for _, name := range []string{"acme/a", "acme/b"} {
		r, err := f.reader.Repo(t.Context(), name)
		if err != nil || r.ObjectFormat != "sha1" {
			t.Errorf("%s: %+v, %v", name, r, err)
		}
	}
	if n := len(f.requests(http.MethodGet, "/repos/acme/b/hash-algorithm")); n != 0 {
		t.Errorf("asked GHES again: %d", n)
	}
}

// TestResolve: an organization's repositories (type all), topics all
// present ignoring case, forks only when asked, sorted ignoring case; a
// user's when the name is no organization.
func TestResolve(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	var items []any
	for i, name := range []string{"Zeta", "alpha", "Beta", "fork", "untagged"} {
		opts := []repoOpt{withField("topics", []string{"sync", "python"})}
		switch name {
		case "fork":
			opts = append(opts, withField("fork", true))
		case "untagged":
			opts = []repoOpt{withField("topics", []string{"python"})}
		}
		items = append(items, repo(int64(300+i), "acme/"+name, opts...))
	}
	for i := range 150 {
		items = append(items, repo(int64(1000+i), "acme/other-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), withField("topics", []string{})))
	}
	f.pages("/orgs/acme/repos", items)
	for _, name := range []string{"Zeta", "alpha", "Beta", "fork", "untagged"} {
		f.json(http.MethodGet, "/repos/acme/"+name+"/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	}
	res, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"SYNC", "Python"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Repos); !slices.Equal(got, []string{"acme/alpha", "acme/Beta", "acme/Zeta"}) || !res.Complete {
		t.Errorf("Resolve = %v, complete %v", got, res.Complete)
	}
	for _, c := range f.requests(http.MethodGet, "/orgs/acme/repos") {
		if c.Query.Get("type") != "all" || c.Query.Get("per_page") != "100" {
			t.Errorf("query %v", c.Query)
		}
	}
	res, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"sync"}, Forks: true})
	if got := paths(res.Repos); err != nil || !slices.Equal(got, []string{"acme/alpha", "acme/Beta", "acme/fork", "acme/Zeta"}) {
		t.Errorf("with forks = %v, %v", got, err)
	}

	// A user whom the token does not act as: GET /users/{u}/repos lists
	// public repositories only, so the listing is incomplete, and says why.
	f.json(http.MethodGet, "/user", http.StatusOK, user(3002, "bob", "User"))
	f.json(http.MethodGet, "/orgs/alice/repos", http.StatusNotFound, notFoundBody)
	f.pages("/users/alice/repos", []any{repo(401, "alice/dotfiles")})
	f.json(http.MethodGet, "/repos/alice/dotfiles/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	res, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "alice"})
	if got := paths(res.Repos); err != nil || !slices.Equal(got, []string{"alice/dotfiles"}) || res.Complete ||
		!strings.Contains(res.Incomplete, "public repositories only") || !strings.Contains(res.Incomplete, "acts as bob") {
		t.Errorf("user = %v, complete %v (%q), %v", got, res.Complete, res.Incomplete, err)
	}
	if c := f.requests(http.MethodGet, "/users/alice/repos"); len(c) != 1 || c[0].Query.Get("type") != "owner" {
		t.Errorf("user listing %+v", c)
	}

	// The token's own account: GET /user/repos?affiliation=owner, private
	// repositories included, complete.
	me := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	me.json(http.MethodGet, "/user", http.StatusOK, user(3001, "Alice", "User"))
	me.json(http.MethodGet, "/orgs/alice/repos", http.StatusNotFound, notFoundBody)
	me.pages("/user/repos", []any{repo(401, "alice/dotfiles"), repo(402, "alice/private", withField("private", true), withField("visibility", "private"))})
	for _, name := range []string{"dotfiles", "private"} {
		me.json(http.MethodGet, "/repos/alice/"+name+"/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	}
	res, err = me.reader.Resolve(t.Context(), platform.Selector{Namespace: "alice"})
	if got := paths(res.Repos); err != nil || !slices.Equal(got, []string{"alice/dotfiles", "alice/private"}) || !res.Complete {
		t.Errorf("the token's own user = %v, complete %v, %v", got, res.Complete, err)
	}
	if c := me.requests(http.MethodGet, "/user/repos"); len(c) != 1 || c[0].Query.Get("affiliation") != "owner" || c[0].Query.Get("type") != "" {
		t.Errorf("own listing %+v", c)
	}

	// Anonymous: public repositories are all it can see; complete.
	an := newFixture(t, fixtureOpts{kind: credAnonymous, host: "github.com"})
	an.json(http.MethodGet, "/orgs/alice/repos", http.StatusNotFound, notFoundBody)
	an.pages("/users/alice/repos", []any{repo(401, "alice/dotfiles")})
	an.json(http.MethodGet, "/repos/alice/dotfiles/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	res, err = an.reader.Resolve(t.Context(), platform.Selector{Namespace: "alice"})
	if err != nil || len(res.Repos) != 1 || !res.Complete {
		t.Errorf("anonymous user = %+v, %v", res, err)
	}

	// A repository selector is Repo.
	f.json(http.MethodGet, "/repos/acme/alpha", http.StatusOK, repo(301, "acme/alpha"))
	res, err = f.reader.Resolve(t.Context(), platform.Selector{Repo: "acme/alpha", Namespace: "ignored"})
	if err != nil || len(res.Repos) != 1 || !res.Complete {
		t.Errorf("repo selector: %+v, %v", res, err)
	}
	_, err = f.reader.Resolve(t.Context(), platform.Selector{})
	wantClass(t, "no selector", err, platform.ClassInvalid, nil)
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme/sub", Subgroups: true})
	wantClass(t, "nested", err, platform.ClassNotFound, platform.ErrNotFound)
}

// TestResolveIncomplete: a later page that fails, or a repository whose
// facts cannot be read, makes the result incomplete; a first page that
// fails, a rate limit or a transient failure of a fact fails it.
func TestResolveIncomplete(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	f.handle(http.MethodGet, "/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusNotFound, notFoundBody)
			return
		}
		w.Header().Set("Link", `<http://`+r.Host+`/api/v3/organizations/1001/repos?page=2&per_page=100&type=all>; rel="next"`)
		writeJSON(w, http.StatusOK, []any{repo(301, "acme/a"), repo(302, "acme/b")})
	})
	f.json(http.MethodGet, "/repos/acme/a/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	f.json(http.MethodGet, "/repos/acme/b/hash-algorithm", http.StatusForbidden, ghError("Resource not accessible"))
	res, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	if err != nil || res.Complete || !slices.Equal(paths(res.Repos), []string{"acme/a"}) {
		t.Errorf("Resolve = %v, complete %v, %v", paths(res.Repos), res.Complete, err)
	}
	// The next page is asked with the query of the Link, on the path asked.
	if c := f.requests(http.MethodGet, "/orgs/acme/repos"); len(c) != 2 || c[1].Query.Get("page") != "2" {
		t.Errorf("pages %+v", c)
	}

	f.json(http.MethodGet, "/repos/acme/b/hash-algorithm", http.StatusBadGateway, ghError("Server Error"))
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	wantClass(t, "a transient fact", err, platform.ClassTransient, nil)

	f.json(http.MethodGet, "/orgs/acme/repos", http.StatusForbidden, map[string]any{"message": "API rate limit exceeded"})
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	wantClass(t, "rate limit", err, platform.ClassRateLimited, nil)
}

// TestResolveApp: an App lists a namespace with its installation's token;
// a namespace without the App is ClassNotFound.
func TestResolveApp(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.pages("/orgs/acme/repos", []any{repo(301, "acme/a")})
	f.json(http.MethodGet, "/repos/acme/a/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	res, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	if err != nil || len(res.Repos) != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	for _, c := range f.requests(http.MethodGet, "/orgs/acme/repos") {
		if m, ok := f.tokenOf(c.Auth); !ok || m.installation != instAcme {
			t.Errorf("listed with %v", m)
		}
	}
	f.json(http.MethodGet, "/orgs/octo-org/installation", http.StatusNotFound, notFoundBody)
	f.json(http.MethodGet, "/users/octo-org/installation", http.StatusNotFound, notFoundBody)
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "octo-org"})
	wantClass(t, "not installed", err, platform.ClassNotFound, platform.ErrNotFound)

	// A user's namespace: the repositories of the user's installation,
	// private ones included (GET /users/{u}/repos would list public ones
	// only), the user's alone.
	f.json(http.MethodGet, "/orgs/alice/repos", http.StatusNotFound, notFoundBody)
	f.handle(http.MethodGet, "/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if m, ok := f.tokenOf(r.Header.Get("Authorization")); !ok || m.installation != instAlice {
			t.Errorf("GET /installation/repositories with %+v", m)
		}
		items := []any{repo(401, "alice/dotfiles"), repo(402, "alice/private", withField("private", true), withField("visibility", "private")),
			repo(403, "acme/elsewhere")}
		servePage(w, r, items, func(page []any) any { return map[string]any{"total_count": len(items), "repositories": page} })
	})
	for _, name := range []string{"dotfiles", "private"} {
		f.json(http.MethodGet, "/repos/alice/"+name+"/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	}
	res, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "alice"})
	if got := paths(res.Repos); err != nil || !slices.Equal(got, []string{"alice/dotfiles", "alice/private"}) || !res.Complete {
		t.Errorf("a user's namespace = %v, complete %v, %v", got, res.Complete, err)
	}
	if c := f.requests(http.MethodGet, "/users/alice/repos"); len(c) != 0 {
		t.Errorf("GET /users/alice/repos: %+v", c)
	}
}

// TestRemote: the https URL under the web URL and a Basic header with
// x-access-token and the current token; none when anonymous.
func TestRemote(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	r := platform.Repo{Host: "github.com", ID: "101", Path: "acme/api"}
	rem, err := f.reader.Remote(t.Context(), r)
	if err != nil || rem.URL != "https://github.com/acme/api.git" || rem.Header == nil {
		t.Fatalf("Remote = %+v, %v", rem, err)
	}
	h, err := rem.Header(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	user, tok, _ := strings.Cut(string(raw), ":")
	if m, ok := f.tokenOf("Bearer " + tok); user != "x-access-token" || !ok || m.installation != instAcme {
		t.Errorf("header for %q with %v", user, m)
	}
	if !f.reg.Contains(h) {
		t.Error("the Basic header is not masked by the run's registry")
	}

	an := newFixture(t, fixtureOpts{kind: credAnonymous, host: "github.com"})
	rem, err = an.reader.Remote(t.Context(), r)
	if err != nil || rem.Header != nil {
		t.Errorf("anonymous Remote = %+v, %v", rem, err)
	}
	_, err = f.reader.Remote(t.Context(), platform.Repo{Host: "gitlab.example.com", Path: "acme/api"})
	wantClass(t, "another host", err, platform.ClassInvalid, nil)
}

func paths(repos []platform.Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Path
	}
	return out
}

func reposEqual(a, b platform.Repo) bool {
	return reflect.DeepEqual(a, b)
}
