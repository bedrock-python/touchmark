package bitbucketdc

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestRepo(t *testing.T) {
	f := newFixture(t)
	f.json(repoRoute("ACME", "api"), http.StatusOK, repo(repoID, "ACME", "api"))
	f.defBranch("ACME", "api", "main", mainSHA)
	f.json(repoRoute("ACME", "fork"), http.StatusOK, repo(forkID, "ACME", "fork", with("public", true),
		with("origin", repoRef(repoID, "ACME", "api"))))
	f.defBranch("ACME", "fork", "develop", mainSHA)
	// An empty repository: 204 from …/branches/default, its configured
	// default branch from …/default-branch.
	f.json(repoRoute("ACME", "empty"), http.StatusOK, repo(103, "ACME", "empty"))
	f.json(repoRoute("ACME", "empty")+"/branches/default", http.StatusNoContent, nil)
	f.json(repoRoute("ACME", "empty")+"/default-branch", http.StatusOK, map[string]any{"id": "refs/heads/master", "displayId": "master", "type": "BRANCH"})
	// Branches exist, the configured default branch does not: skipped as
	// empty.
	f.json(repoRoute("ACME", "odd"), http.StatusOK, repo(104, "ACME", "odd", with("archived", true), with("state", "OFFLINE")))
	f.json(repoRoute("ACME", "odd")+"/branches/default", http.StatusNotFound, errorBody("com.atlassian.bitbucket.repository.NoSuchBranchException", "The default branch does not exist."))
	f.json(repoRoute("ACME", "odd")+"/default-branch", http.StatusOK, map[string]any{"id": "refs/heads/trunk", "displayId": "trunk", "type": "BRANCH"})
	f.json(repoRoute("ACME", "gone"), http.StatusNotFound, errorBody(noRepoException, noRepoMessage))

	got, err := f.reader.Repo(t.Context(), "ACME/api")
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Repo{Host: f.provider().Host, ID: "101", Path: "ACME/api", DefaultBranch: "main",
		WebURL: f.base() + "/projects/ACME/repos/api", Visibility: "private", ObjectFormat: "sha1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Repo = %+v\nwant %+v", got, want)
	}
	fork, err := f.reader.Repo(t.Context(), "ACME/fork")
	if err != nil || !fork.Fork || fork.Visibility != "public" || fork.DefaultBranch != "develop" || fork.Path != "ACME/fork" {
		t.Errorf("fork = %+v, %v", fork, err)
	}
	empty, err := f.reader.Repo(t.Context(), "ACME/empty")
	if err != nil || !empty.Empty || empty.DefaultBranch != "master" {
		t.Errorf("an empty repository = %+v, %v", empty, err)
	}
	odd, err := f.reader.Repo(t.Context(), "ACME/odd")
	if err != nil || !odd.Empty || odd.DefaultBranch != "trunk" || !odd.Archived || !odd.Disabled {
		t.Errorf("a repository whose default branch is missing = %+v, %v", odd, err)
	}
	_, err = f.reader.Repo(t.Context(), "ACME/gone")
	wantClass(t, "a missing repository", err, platform.ClassNotFound, platform.ErrNotFound)
	if err != nil && !strings.Contains(err.Error(), noRepoMessage) {
		t.Errorf("the error %q lacks the API's message", err)
	}
	f.reset()
	for _, p := range []string{"ACME", "ACME/a/b", "/api", "ACME/..", ""} {
		_, err := f.reader.Repo(t.Context(), p)
		wantClass(t, "Repo("+p+")", err, platform.ClassNotFound, platform.ErrNotFound)
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("paths that name no repository sent %d requests", len(calls))
	}
}

func TestRepoShape(t *testing.T) {
	f := newFixture(t)
	f.json(repoRoute("ACME", "api"), http.StatusOK, repo(0, "ACME", "api"))
	_, err := f.reader.Repo(t.Context(), "ACME/api")
	wantClass(t, "a repository without id", err, platform.ClassUnknown, nil)

	f.json(repoRoute("ACME", "api"), http.StatusOK, repo(repoID, "ACME", "api"))
	f.json(repoRoute("ACME", "api")+"/branches/default", http.StatusOK, branch("main", "", true))
	_, err = f.reader.Repo(t.Context(), "ACME/api")
	wantClass(t, "a default branch without a commit", err, platform.ClassUnknown, nil)
}

// projectRepos are the repositories of project ACME the identity reads,
// out of order, with a fork and an archived one, and one of another
// project that a loose match lets through.
func projectRepos() []any {
	return []any{
		repo(3, "ACME", "web"),
		repo(1, "ACME", "API"),
		repo(forkID, "ACME", "api-fork", with("origin", repoRef(201, "OTHER", "api"))),
		repo(2, "ACME", "billing", with("archived", true)),
		repo(4, "ACME", "zeta"),
		repo(5, "ACMEX", "stray"),
	}
}

func TestResolve(t *testing.T) {
	f := newFixture(t)
	f.pages("/repos", 2, projectRepos())
	for _, slug := range []string{"web", "API", "api-fork", "billing", "zeta"} {
		f.defBranch("ACME", slug, "main", mainSHA)
	}
	got, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Subgroups: true})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range got.Repos {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, " ") != "ACME/API ACME/billing ACME/web ACME/zeta" || !got.Complete {
		t.Errorf("Resolve = %v, complete %v", paths, got.Complete)
	}
	if !got.Repos[1].Archived || got.Repos[0].DefaultBranch != "main" {
		t.Errorf("repositories %+v", got.Repos)
	}
	calls := f.requests(http.MethodGet, "/repos")
	if len(calls) != 3 {
		t.Fatalf("%d pages read, want 3", len(calls))
	}
	if q := calls[0].Query; q.Get("projectkey") != "acme" || q.Get("archived") != "ALL" || q.Get("limit") != "1000" || q.Has("start") {
		t.Errorf("the first page asked %v", q)
	}
	if q := calls[1].Query; q.Get("start") != "2" {
		t.Errorf("the second page asked %v", q)
	}
	if n := len(f.requests(http.MethodGet, repoRoute("ACME", "api-fork")+"/branches/default")); n != 0 {
		t.Error("the default branch of a skipped fork was read")
	}

	forks, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME", Forks: true})
	if err != nil || len(forks.Repos) != 5 {
		t.Errorf("Resolve with forks = %+v, %v", forks.Repos, err)
	}
}

func TestResolveLabels(t *testing.T) {
	f := newFixture(t)
	// Labels list repositories of every project (RestLabelable).
	labeled := func(items ...any) []any {
		for _, it := range items {
			it.(map[string]any)["labelableType"] = "REPOSITORY"
		}
		return items
	}
	f.pages("/labels/python/labeled", 1000, labeled(repo(3, "ACME", "web"), repo(1, "ACME", "api"), repo(9, "OTHER", "tool")))
	f.pages("/labels/service/labeled", 1000, labeled(repo(1, "ACME", "api"), repo(9, "OTHER", "tool")))
	f.json("/labels/none/labeled", http.StatusNotFound, errorBody("com.atlassian.bitbucket.label.NoSuchLabelException", "Label none does not exist."))
	f.json("/projects/ACME", http.StatusOK, project("ACME"))
	f.defBranch("ACME", "api", "main", mainSHA)
	f.defBranch("ACME", "web", "main", mainSHA)

	got, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME", Topics: []string{"python", "service"}})
	if err != nil || len(got.Repos) != 1 || got.Repos[0].Path != "ACME/api" || !got.Complete {
		t.Errorf("Resolve with two labels = %+v, %v", got, err)
	}
	if q := f.requests(http.MethodGet, "/labels/python/labeled")[0].Query; q.Get("type") != "REPOSITORY" {
		t.Errorf("the label query %v", q)
	}
	one, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME", Topics: []string{"python"}})
	if err != nil || len(one.Repos) != 2 {
		t.Errorf("Resolve with one label = %+v, %v", one, err)
	}
	none, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME", Topics: []string{"python", "none"}})
	if err != nil || len(none.Repos) != 0 || !none.Complete {
		t.Errorf("a label no repository has = %+v, %v", none, err)
	}
	if n := len(f.requests(http.MethodGet, "/repos")); n != 0 {
		t.Errorf("a selector with labels listed the project (%d requests)", n)
	}
}

func TestResolveFailures(t *testing.T) {
	// A later page fails: incomplete.
	f := newFixture(t)
	f.handle(http.MethodGet, "/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "" {
			writeJSON(w, http.StatusBadRequest, errorBody("", "bad page"))
			return
		}
		servePage(w, r, 1, projectRepos())
	})
	f.defBranch("ACME", "web", "main", mainSHA)
	got, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	if err != nil || got.Complete || len(got.Repos) != 1 {
		t.Errorf("a failed later page = %+v, %v", got, err)
	}

	// The first page fails: an error.
	f = newFixture(t)
	f.json("/repos", http.StatusUnauthorized, errorBody("com.atlassian.bitbucket.auth.AuthenticationException", "Authentication failed. Please check your credentials and try again."))
	f.username = ""
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	wantClass(t, "a refused credential", err, platform.ClassAuth, nil)

	// A default branch that fails for now fails the call; one that is
	// refused makes the listing incomplete; a repository gone is left out.
	f = newFixture(t)
	f.pages("/repos", 1000, []any{repo(1, "ACME", "a"), repo(2, "ACME", "b"), repo(3, "ACME", "c")})
	f.defBranch("ACME", "a", "main", mainSHA)
	f.json(repoRoute("ACME", "b")+"/branches/default", http.StatusUnauthorized, errorBody(authorisation, "You are not permitted to access this resource"))
	f.json(repoRoute("ACME", "c")+"/branches/default", http.StatusNotFound, errorBody(noRepoException, noRepoMessage))
	f.json(repoRoute("ACME", "c")+"/default-branch", http.StatusNotFound, errorBody(noRepoException, noRepoMessage))
	f.json(repoRoute("ACME", "c"), http.StatusNotFound, errorBody(noRepoException, noRepoMessage))
	got, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	if err != nil || got.Complete || len(got.Repos) != 1 || got.Repos[0].Path != "ACME/a" {
		t.Errorf("Resolve = %+v, %v", got, err)
	}
	f.json(repoRoute("ACME", "b")+"/branches/default", http.StatusServiceUnavailable, errorBody("", "Bitbucket is starting"))
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	wantClass(t, "a transient failure", err, platform.ClassTransient, nil)

	f.reset()
	for _, sel := range []platform.Selector{{}, {Namespace: "ACME/sub"}, {Namespace: ".."}} {
		_, err := f.reader.Resolve(t.Context(), sel)
		if err == nil {
			t.Errorf("Resolve(%+v) passed", sel)
		}
	}
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("invalid selectors sent %d requests", n)
	}
}

// TestResolveProject: a project key that lists no repository is checked:
// a project Bitbucket does not find, or one whose key changed, is
// ClassNotFound (the core then sweeps nothing); one the identity may not
// read lists nothing, completely.
func TestResolveProject(t *testing.T) {
	f := newFixture(t)
	f.pages("/repos", 1000, nil)
	f.json("/projects/ACME", http.StatusNotFound, errorBody("com.atlassian.bitbucket.project.NoSuchProjectException", "Project ACME does not exist."))
	_, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	wantClass(t, "a missing project", err, platform.ClassNotFound, platform.ErrNotFound)
	f.json("/projects/ACME", http.StatusOK, project("ACME2"))
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	wantClass(t, "a project whose key changed", err, platform.ClassNotFound, platform.ErrNotFound)
	f.json("/projects/ACME", http.StatusUnauthorized, errorBody(authorisation, "You are not permitted to access this resource"))
	got, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	if err != nil || !got.Complete || len(got.Repos) != 0 {
		t.Errorf("a project the identity may not read = %+v, %v", got, err)
	}
	f.json("/projects/ACME", http.StatusOK, project("acme"))
	if _, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"}); err != nil {
		t.Errorf("an empty project: %v", err)
	}
	// A project with repositories is not read.
	f.reset()
	f.pages("/repos", 1000, []any{repo(1, "ACME", "a")})
	f.defBranch("ACME", "a", "main", mainSHA)
	if _, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"}); err != nil || len(f.requests(http.MethodGet, "/projects/ACME")) != 0 {
		t.Errorf("a project with repositories: %v, %d project reads", err, len(f.requests(http.MethodGet, "/projects/ACME")))
	}
}

func TestRemote(t *testing.T) {
	f := newFixture(t)
	r := platform.Repo{Host: f.provider().Host, ID: "101", Path: "ACME/api"}
	rem, err := f.reader.Remote(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL != f.base()+"/scm/acme/api.git" || rem.Header == nil {
		t.Errorf("Remote = %+v", rem)
	}
	if h, err := rem.Header(t.Context()); err != nil || h != "Bearer "+f.token {
		t.Errorf("the git header is wrong: %v", err)
	}
	_, err = f.reader.Remote(t.Context(), platform.Repo{Host: "evil.example.com", Path: "ACME/api"})
	wantClass(t, "a repository of another host", err, platform.ClassInvalid, nil)

	anon, err := NewReader(f.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if rem, err := anon.Remote(t.Context(), r); err != nil || rem.Header != nil {
		t.Errorf("an anonymous remote = %+v, %v", rem, err)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	path := repoRoute("ACME", "api")
	for _, tc := range []struct {
		name     string
		status   int
		body     any
		username string
		class    platform.Class
	}{
		{"a refused credential", http.StatusUnauthorized, errorBody("com.atlassian.bitbucket.auth.IncorrectPasswordAuthenticationException", "Authentication failed."), "", platform.ClassAuth},
		{"a user without the permission", http.StatusUnauthorized, errorBody("", "The currently authenticated user has insufficient permissions"), "touchmark.reader", platform.ClassPermission},
		{"an AuthorisationException", http.StatusUnauthorized, errorBody(authorisation, "You are not permitted to access this resource"), "", platform.ClassPermission},
		{"a forbidden request", http.StatusForbidden, errorBody("", "Forbidden"), "touchmark.reader", platform.ClassPermission},
		{"a conflict", http.StatusConflict, errorBody("", "The specified version is out of date."), "touchmark.reader", platform.ClassConflict},
		{"a rate limit", http.StatusTooManyRequests, nil, "touchmark.reader", platform.ClassRateLimited},
		{"a server error", http.StatusInternalServerError, errorBody("", "boom"), "touchmark.reader", platform.ClassTransient},
	} {
		f.username = tc.username
		f.json(path, tc.status, tc.body)
		_, err := f.reader.c.getRepo(t.Context(), "get repository", "ACME", "api")
		wantClass(t, tc.name, err, tc.class, nil)
		if err != nil && strings.Contains(err.Error(), f.token) {
			t.Errorf("%s: the error shows the token", tc.name)
		}
	}
	f.username = "touchmark.reader"
	f.handle(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, nil)
	})
	_, err := f.reader.c.getRepo(t.Context(), "get repository", "ACME", "api")
	var pe *platform.Error
	if !errors.As(err, &pe) || pe.RetryAfter.Seconds() != 30 {
		t.Errorf("a rate limit with Retry-After = %v", err)
	}

	// A page that is not the last but names no next start.
	f.json("/repos", http.StatusOK, map[string]any{"values": []any{}, "isLastPage": false, "start": 0, "limit": 25, "size": 0})
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	wantClass(t, "a page without nextPageStart", err, platform.ClassUnknown, nil)
	f.json("/repos", http.StatusOK, map[string]any{"values": []any{}, "start": 0})
	_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "ACME"})
	wantClass(t, "a page without isLastPage", err, platform.ClassUnknown, nil)
}

func TestRedirects(t *testing.T) {
	f := newFixture(t)
	// A repository renamed: its old path redirects below the API.
	f.handle(http.MethodGet, repoRoute("ACME", "old"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", apiPrefix+repoRoute("ACME", "api"))
		w.WriteHeader(http.StatusMovedPermanently)
	})
	f.json(repoRoute("ACME", "api"), http.StatusOK, repo(repoID, "ACME", "api"))
	f.defBranch("ACME", "api", "main", mainSHA)
	if r, err := f.reader.Repo(t.Context(), "ACME/old"); err != nil || r.Path != "ACME/api" {
		t.Errorf("a redirected repository = %+v, %v", r, err)
	}
	// A redirect elsewhere is not followed.
	f.handle(http.MethodGet, repoRoute("ACME", "away"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://evil.example.com/rest/api/latest/projects/ACME/repos/api")
		w.WriteHeader(http.StatusFound)
	})
	_, err := f.reader.Repo(t.Context(), "ACME/away")
	if err == nil {
		t.Error("a redirect to another host was followed")
	}
}
