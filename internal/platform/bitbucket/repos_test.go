package bitbucket

import (
	"encoding/base64"
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
	f.json("/repositories/acme/api", http.StatusOK, repo(repoUUID, "acme/api"))
	f.json("/repositories/acme/fork", http.StatusOK, repo(forkUUID, "acme/fork", with("is_private", false),
		with("parent", repoRef(repoUUID, "acme/api")), with("mainbranch", map[string]any{"name": "develop", "type": "branch"})))
	f.json("/repositories/acme/empty", http.StatusOK, repo("{11111111-2222-4333-8444-555555555555}", "acme/empty", with("mainbranch", nil)))
	f.json("/repositories/acme/gone", http.StatusNotFound, errorBody(noRepoMessage))

	got, err := f.reader.Repo(t.Context(), "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Repo{Host: f.provider().Host, ID: repoUUID, Path: "acme/api", DefaultBranch: "main",
		WebURL: "https://bitbucket.org/acme/api", Visibility: "private", ObjectFormat: "sha1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Repo = %+v\nwant %+v", got, want)
	}
	fork, err := f.reader.Repo(t.Context(), "acme/fork")
	if err != nil || !fork.Fork || fork.Visibility != "public" || fork.DefaultBranch != "develop" || fork.Empty {
		t.Errorf("fork = %+v, %v", fork, err)
	}
	empty, err := f.reader.Repo(t.Context(), "acme/empty")
	if err != nil || !empty.Empty || empty.DefaultBranch != "" {
		t.Errorf("a repository without a main branch = %+v, %v; want empty", empty, err)
	}
	_, err = f.reader.Repo(t.Context(), "acme/gone")
	wantClass(t, "a missing repository", err, platform.ClassNotFound, platform.ErrNotFound)
	if err != nil && !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("the error %q lacks the API's message", err)
	}
	f.reset()
	for _, p := range []string{"acme", "acme/a/b", "/api", "acme/..", ""} {
		_, err := f.reader.Repo(t.Context(), p)
		wantClass(t, "Repo("+p+")", err, platform.ClassNotFound, platform.ErrNotFound)
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("paths that name no repository sent %d requests", len(calls))
	}
}

func TestRepoShape(t *testing.T) {
	f := newFixture(t)
	f.json("/repositories/acme/api", http.StatusOK, repo("", "acme/api"))
	_, err := f.reader.Repo(t.Context(), "acme/api")
	wantClass(t, "a repository without uuid", err, platform.ClassUnknown, nil)
}

// workspaceRepos are the repositories of workspace acme, out of order, with
// a fork.
func workspaceRepos() []any {
	return []any{
		repo("{00000000-0000-4000-8000-000000000003}", "acme/web"),
		repo("{00000000-0000-4000-8000-000000000001}", "acme/API"),
		repo(forkUUID, "acme/api-fork", with("parent", repoRef(repoUUID, "other/api"))),
		repo("{00000000-0000-4000-8000-000000000002}", "acme/billing"),
		repo("{00000000-0000-4000-8000-000000000004}", "acme/zeta"),
	}
}

func TestResolve(t *testing.T) {
	f := newFixture(t)
	f.pages("/repositories/acme", 2, workspaceRepos())
	got, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Subgroups: true})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range got.Repos {
		paths = append(paths, r.Path)
	}
	if !got.Complete || strings.Join(paths, " ") != "acme/API acme/billing acme/web acme/zeta" {
		t.Errorf("Resolve = %v, complete %v; want the four repositories that are no fork, sorted ignoring case", paths, got.Complete)
	}
	calls := f.requests(http.MethodGet, "/repositories/acme")
	if len(calls) != 3 || calls[0].Query.Get("pagelen") != "100" || calls[2].Query.Get("page") != "3" {
		t.Errorf("calls %+v; want three pages, the next links followed", calls)
	}
	withForks, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Forks: true})
	if err != nil || len(withForks.Repos) != 5 {
		t.Errorf("with forks: %d repositories, %v; want five", len(withForks.Repos), err)
	}
}

func TestResolveRepo(t *testing.T) {
	f := newFixture(t)
	f.json("/repositories/acme/api", http.StatusOK, repo(repoUUID, "acme/api", with("parent", repoRef(forkUUID, "other/api"))))
	got, err := f.reader.Resolve(t.Context(), platform.Selector{Repo: "acme/api", Topics: []string{"ignored"}})
	if err != nil || !got.Complete || len(got.Repos) != 1 || got.Repos[0].ID != repoUUID {
		t.Errorf("Resolve(repo) = %+v, %v; want the repository, a fork too", got, err)
	}
}

func TestResolveErrors(t *testing.T) {
	t.Run("topics", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme", Topics: []string{"python"}})
		wantClass(t, "topics", err, platform.ClassInvalid, nil)
		if err != nil && !strings.Contains(err.Error(), "no topics") {
			t.Errorf("error %q; want it to say Bitbucket has no topics", err)
		}
		if calls := f.requests("", ""); len(calls) != 0 {
			t.Errorf("%d requests for a selector with topics", len(calls))
		}
	})
	t.Run("no namespace", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.reader.Resolve(t.Context(), platform.Selector{})
		wantClass(t, "empty", err, platform.ClassInvalid, nil)
		_, err = f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme/sub"})
		wantClass(t, "nested", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("missing workspace", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/gone", http.StatusNotFound, errorBody("No workspace with identifier 'gone'."))
		_, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "gone"})
		wantClass(t, "a missing workspace", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("a later page fails", func(t *testing.T) {
		f := newFixture(t)
		f.handle(http.MethodGet, "/repositories/acme", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				writeJSON(w, http.StatusForbidden, errorBody("Forbidden"))
				return
			}
			servePage(w, r, 2, workspaceRepos())
		})
		got, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
		if err != nil || got.Complete || len(got.Repos) != 2 {
			t.Errorf("Resolve = %+v, %v; want the first page, incomplete", got, err)
		}
	})
	t.Run("a later page is rate limited", func(t *testing.T) {
		f := newFixture(t)
		f.handle(http.MethodGet, "/repositories/acme", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.Header().Set("X-RateLimit-Limit", "1000")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", "394")
				writeJSON(w, http.StatusTooManyRequests, errorBody("Rate limit for this resource has been exceeded"))
				return
			}
			servePage(w, r, 2, workspaceRepos())
		})
		_, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
		wantClass(t, "rate limited", err, platform.ClassRateLimited, nil)
	})
	t.Run("a next link elsewhere", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme", http.StatusOK, map[string]any{
			"values": workspaceRepos()[:1], "pagelen": 1, "next": "https://evil.example.com/2.0/repositories/acme?page=2",
		})
		_, err := f.reader.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
		wantClass(t, "a next link to another host", err, platform.ClassUnknown, nil)
		if len(f.requests("", "")) != 1 {
			t.Error("the driver followed a next link to another host")
		}
	})
}

func TestRemote(t *testing.T) {
	f := newFixture(t)
	r := platform.Repo{Host: f.provider().Host, ID: repoUUID, Path: "acme/api"}
	rem, err := f.reader.Remote(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL != f.base()+"/acme/api.git" || rem.Header == nil {
		t.Fatalf("Remote = %+v; want the web URL with .git and a header", rem)
	}
	h, err := rem.Header(t.Context())
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-bitbucket-api-token-auth:"+f.token))
	if err != nil || h != want {
		t.Errorf("header %q, %v; want git's Basic form of an API token", h, err)
	}
	if _, err := f.reader.Remote(t.Context(), platform.Repo{Host: "bitbucket.example.com", Path: "acme/api"}); err == nil {
		t.Error("a repository of another host got a remote")
	}
	if _, err := f.reader.Remote(t.Context(), platform.Repo{Path: "acme"}); err == nil {
		t.Error("a path that is no workspace/repository got a remote")
	}
	anon, err := NewReader(f.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if rem, err := anon.Remote(t.Context(), r); err != nil || rem.Header != nil {
		t.Errorf("anonymous Remote = %+v, %v; want no header", rem, err)
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("Remote sent %d requests", len(calls))
	}
}
