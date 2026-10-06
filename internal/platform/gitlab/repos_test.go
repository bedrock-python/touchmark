package gitlab

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestRepo(t *testing.T) {
	fx := newFixture(t)
	p := project(7, "acme/api", with("topics", []string{"Python", "engineering"}), with("visibility", "internal"))
	// GitLab finds paths ignoring case, and old paths of renamed projects,
	// and answers with the project itself (lookup_project follows
	// redirects).
	for _, path := range []string{"/projects/acme%2Fapi", "/projects/ACME%2FAPI", "/projects/acme%2Fold-api"} {
		fx.json(http.MethodGet, path, http.StatusOK, p)
	}
	fx.json(http.MethodGet, "/projects/acme%2Fmissing", http.StatusNotFound, msg("404 Project Not Found"))
	for _, path := range []string{"acme/api", "ACME/API", "acme/old-api"} {
		r, err := fx.reader.Repo(t.Context(), path)
		if err != nil {
			t.Fatalf("Repo(%s): %v", path, err)
		}
		want := platform.Repo{Host: fx.provider().Host, ID: "7", Path: "acme/api", DefaultBranch: "main",
			WebURL: "https://gitlab.example.com/acme/api", Visibility: "internal", ObjectFormat: "sha1",
			Topics: []string{"Python", "engineering"}}
		if r.Host != want.Host || r.ID != want.ID || r.Path != want.Path || r.DefaultBranch != want.DefaultBranch ||
			r.WebURL != want.WebURL || r.Visibility != want.Visibility || r.ObjectFormat != want.ObjectFormat ||
			!slices.Equal(r.Topics, want.Topics) || r.Archived || r.Empty || r.Fork || r.Mirror || r.Disabled ||
			r.PendingDelete || r.PRsDisabled {
			t.Errorf("Repo(%s) = %+v", path, r)
		}
	}
	_, err := fx.reader.Repo(t.Context(), "acme/missing")
	wantClass(t, "Repo of a missing project", err, platform.ClassNotFound, platform.ErrNotFound)
	for _, bad := range []string{"acme", "", "acme//api", "acme/../x", "a/b?c"} {
		_, err := fx.reader.Repo(t.Context(), bad)
		wantClass(t, "Repo("+bad+")", err, platform.ClassNotFound, platform.ErrNotFound)
	}
}

func TestRepoFlags(t *testing.T) {
	fx := newFixture(t)
	for _, tc := range []struct {
		name string
		opts []projectOpt
		want func(platform.Repo) bool
	}{
		{"archived", []projectOpt{with("archived", true)}, func(r platform.Repo) bool { return r.Archived }},
		{"empty", []projectOpt{with("empty_repo", true), with("default_branch", nil)},
			func(r platform.Repo) bool { return r.Empty && r.DefaultBranch == "" }},
		{"fork", []projectOpt{with("forked_from_project", map[string]any{"id": 1, "path_with_namespace": "up/api"})},
			func(r platform.Repo) bool { return r.Fork }},
		{"mirror", []projectOpt{with("mirror", true)}, func(r platform.Repo) bool { return r.Mirror }},
		{"pending delete", []projectOpt{with("marked_for_deletion_at", "2026-09-20"), with("marked_for_deletion_on", "2026-09-20")},
			func(r platform.Repo) bool { return r.PendingDelete }},
		{"MRs disabled", []projectOpt{with("merge_requests_access_level", "disabled"), with("merge_requests_enabled", false)},
			func(r platform.Repo) bool { return r.PRsDisabled }},
		{"MRs disabled, old field only", []projectOpt{with("merge_requests_access_level", nil), with("merge_requests_enabled", false)},
			func(r platform.Repo) bool { return r.PRsDisabled }},
		{"code disabled", []projectOpt{with("repository_access_level", "disabled"), with("default_branch", nil)},
			func(r platform.Repo) bool { return r.Disabled }},
		{"sha256", []projectOpt{with("repository_object_format", "sha256")}, func(r platform.Repo) bool { return r.ObjectFormat == "sha256" }},
		{"no object format (older GitLab)", []projectOpt{with("repository_object_format", nil)},
			func(r platform.Repo) bool { return r.ObjectFormat == "sha1" }},
		{"public", []projectOpt{with("visibility", "public")}, func(r platform.Repo) bool { return r.Visibility == "public" }},
		{"topics in tag_list only", []projectOpt{with("topics", nil), with("tag_list", []string{"legacy"})},
			func(r platform.Repo) bool { return slices.Equal(r.Topics, []string{"legacy"}) }},
	} {
		fx.json(http.MethodGet, "/projects/acme%2Fx", http.StatusOK, project(8, "acme/x", tc.opts...))
		r, err := fx.reader.Repo(t.Context(), "acme/x")
		if err != nil || !tc.want(r) {
			t.Errorf("%s: %+v, %v", tc.name, r, err)
		}
	}
	// A project without a default branch that is not empty lacks what the
	// driver depends on.
	fx.json(http.MethodGet, "/projects/acme%2Fx", http.StatusOK, project(8, "acme/x", with("default_branch", nil)))
	_, err := fx.reader.Repo(t.Context(), "acme/x")
	wantClass(t, "a project without a default branch", err, platform.ClassUnknown, nil)
}

func TestResolveGroup(t *testing.T) {
	fx := newFixture(t)
	list := []any{
		project(3, "acme/Zeta", with("topics", []string{"python", "engineering"})),
		project(1, "acme/sub/beta", with("topics", []string{"Python"})),
		project(2, "acme/alpha", with("topics", []string{"PYTHON", "Engineering"}), with("archived", true)),
		project(4, "acme/fork", with("topics", []string{"python", "engineering"}),
			with("forked_from_project", map[string]any{"id": 99})),
		project(5, "acme/empty", with("empty_repo", true), with("default_branch", nil), with("topics", []string{"python", "engineering"})),
		// Its repository is for members only, and the reader is none: no
		// default_branch. It once failed the whole listing.
		project(6, "acme/members-code", with("default_branch", nil), with("topics", []string{"python", "engineering"}),
			with("repository_access_level", "private")),
	}
	fx.pages("/groups/acme/projects", list)
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Subgroups: true, Topics: []string{"python", "engineering"}})
	if err != nil || !res.Complete {
		t.Fatalf("Resolve = %v, %v", res, err)
	}
	var got []string
	for _, r := range res.Repos {
		got = append(got, r.Path)
	}
	if want := []string{"acme/alpha", "acme/empty", "acme/members-code", "acme/Zeta"}; !slices.Equal(got, want) {
		t.Errorf("Resolve = %v, want %v (sorted ignoring case, no fork, all topics ignoring case)", got, want)
	}
	for _, r := range res.Repos {
		if r.Disabled != (r.Path == "acme/members-code") {
			t.Errorf("%s: Disabled %v", r.Path, r.Disabled)
		}
	}
	q := fx.requests(http.MethodGet, "/groups/acme/projects")[0].Query
	if q.Get("include_subgroups") != "true" || q.Get("with_shared") != "false" || q.Has("topic") || q.Has("archived") || q.Get("per_page") != "100" {
		t.Errorf("query %v", q)
	}

	res, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Forks: true})
	if err != nil || len(res.Repos) != 6 {
		t.Errorf("Resolve with forks = %d repos, %v", len(res.Repos), err)
	}
	fx.reset()
	if _, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"}); err != nil {
		t.Fatal(err)
	}
	if q := fx.requests(http.MethodGet, "/groups/acme/projects")[0].Query; q.Has("include_subgroups") {
		t.Errorf("without Subgroups: query %v", q)
	}
}

func TestResolveNestedAndUser(t *testing.T) {
	fx := newFixture(t)
	fx.pages("/groups/acme%2Fsub/projects", []any{project(1, "acme/sub/beta")})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme/sub"})
	if err != nil || len(res.Repos) != 1 || res.Repos[0].ID != "1" {
		t.Errorf("nested group: %+v, %v", res, err)
	}

	fx.json(http.MethodGet, "/groups/jdoe/projects", http.StatusNotFound, msg("404 Group Not Found"))
	fx.pages("/users/jdoe/projects", []any{project(9, "jdoe/tools")})
	res, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "jdoe"})
	if err != nil || len(res.Repos) != 1 || res.Repos[0].Path != "jdoe/tools" {
		t.Errorf("user namespace: %+v, %v", res, err)
	}

	fx.json(http.MethodGet, "/groups/nobody/projects", http.StatusNotFound, msg("404 Group Not Found"))
	fx.json(http.MethodGet, "/users/nobody/projects", http.StatusNotFound, msg("404 User Not Found"))
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "nobody"})
	wantClass(t, "a missing namespace", err, platform.ClassNotFound, platform.ErrNotFound)

	fx.json(http.MethodGet, "/groups/acme%2Fgone/projects", http.StatusNotFound, msg("404 Group Not Found"))
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme/gone"})
	wantClass(t, "a missing nested group", err, platform.ClassNotFound, platform.ErrNotFound)

	_, err = fx.reader.Resolve(t.Context(), platform.Selector{})
	wantClass(t, "an empty selector", err, platform.ClassInvalid, nil)
}

func TestResolveRepoSelector(t *testing.T) {
	fx := newFixture(t)
	fx.json(http.MethodGet, "/projects/up%2Ffork", http.StatusOK, project(4, "up/fork", with("forked_from_project", map[string]any{"id": 1})))
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Repo: "up/fork", Namespace: "elsewhere", Topics: []string{"nope"}})
	if err != nil || !res.Complete || len(res.Repos) != 1 || !res.Repos[0].Fork {
		t.Errorf("a repo selector: %+v, %v", res, err)
	}
}

func TestResolveIncomplete(t *testing.T) {
	fx := newFixture(t)
	var list []any
	for i := range 150 {
		list = append(list, project(int64(i+1), "acme/p"+strings.Repeat("x", i%3)+string(rune('a'+i%26))))
	}
	fx.handle(http.MethodGet, "/groups/acme/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusForbidden, msg("403 Forbidden"))
			return
		}
		servePage(w, r, list, offset)
	})
	res, err := fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	if err != nil || res.Complete {
		t.Errorf("a failed later page: complete %v, %v", res.Complete, err)
	}

	// A rate limit fails the listing, on any page.
	fx.handle(http.MethodGet, "/groups/acme/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("Retry-After", "10")
			writeJSON(w, http.StatusTooManyRequests, msg("Retry later"))
			return
		}
		servePage(w, r, list, offset)
	})
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	wantClass(t, "a rate limit on page 2", err, platform.ClassRateLimited, nil)

	// A failed first page fails the call.
	fx.json(http.MethodGet, "/groups/acme/projects", http.StatusForbidden, msg("403 Forbidden"))
	_, err = fx.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	wantClass(t, "a refused first page", err, platform.ClassPermission, nil)
}

func TestRemote(t *testing.T) {
	fx := newFixture(t)
	r := platform.Repo{Host: fx.provider().Host, ID: "7", Path: "acme/sub/api"}
	rem, err := fx.reader.Remote(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL != fx.base()+"/acme/sub/api.git" {
		t.Errorf("URL %q", rem.URL)
	}
	h, err := rem.Header(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+fx.token)); h != want {
		t.Error("the git header is not Basic oauth2:<token>")
	}
	if got := fx.reader.c.mask(h); strings.Contains(got, fx.token) || got == h {
		t.Error("the Basic form of the token is not masked")
	}
	_, err = fx.reader.Remote(t.Context(), platform.Repo{Host: "other.example", Path: "acme/api"})
	wantClass(t, "Remote of another host", err, platform.ClassInvalid, nil)

	anon, _ := NewReader(fx.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
	rem, err = anon.Remote(t.Context(), r)
	if err != nil || rem.Header != nil {
		t.Errorf("anonymous Remote: header %v, %v", rem.Header != nil, err)
	}
}
