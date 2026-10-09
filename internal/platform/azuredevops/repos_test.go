package azuredevops

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestRepo(t *testing.T) {
	s := newAPIServer(t)
	s.json("/"+org+"/Billing/_apis/git/repositories/API", http.StatusOK, repoJSON(repoID, "Billing", "api", "main"))
	s.json("/"+org+"/Billing/_apis/git/repositories/gone", http.StatusNotFound, errorBody(keyRepoNotFound, "TF401019: The Git repository with name or identifier gone does not exist"))
	s.json("/"+org+"/My%20Project/_apis/git/repositories/web", http.StatusOK, repoJSON(otherRepo, "My Project", "web", ""))
	r := newTestReader(t, s, testToken(t))
	got, err := r.Repo(context.Background(), "Billing/API")
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Repo{Host: r.c.host, ID: repoID, Path: "Billing/api", DefaultBranch: "main",
		WebURL: "https://dev.azure.com/acme/Billing/_git/api", Visibility: "private", ObjectFormat: "sha1"}
	if got.Host != want.Host || got.ID != want.ID || got.Path != want.Path || got.DefaultBranch != want.DefaultBranch ||
		got.WebURL != want.WebURL || got.Visibility != want.Visibility || got.Empty || got.Disabled || got.Fork {
		t.Errorf("Repo = %+v", got)
	}
	empty, err := r.Repo(context.Background(), "My Project/web")
	if err != nil || !empty.Empty || empty.DefaultBranch != "" || empty.Path != "My Project/web" {
		t.Errorf("an empty repository: %+v, %v", empty, err)
	}
	if _, err := r.Repo(context.Background(), "Billing/gone"); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("a missing repository: %v", err)
	}
	for _, bad := range []string{"api", "a/b/c", "Billing/"} {
		if _, err := r.Repo(context.Background(), bad); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("Repo(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestResolve(t *testing.T) {
	s := newAPIServer(t)
	fork := repoJSON(forkRepo, "Billing", "api-fork", "main")
	fork["isFork"] = true
	disabled := repoJSON(otherRepo, "Archive", "old", "")
	disabled["isDisabled"] = true
	public := repoJSON("11111111-2222-3333-4444-555555555555", "web", "Site", "main")
	public["project"].(map[string]any)["visibility"] = "public"
	s.json(apisPath("git", "repositories"), http.StatusOK, collection(repoJSON(repoID, "Billing", "api", "main"), fork, disabled, public))
	r := newTestReader(t, s, testToken(t))
	res, err := r.Resolve(context.Background(), platform.Selector{Namespace: "ACME"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, x := range res.Repos {
		paths = append(paths, x.Path)
	}
	if !res.Complete || len(paths) != 3 || paths[0] != "Archive/old" || paths[1] != "Billing/api" || paths[2] != "web/Site" {
		t.Fatalf("Resolve = %v (complete %v)", paths, res.Complete)
	}
	if !res.Repos[0].Disabled || res.Repos[2].Visibility != "public" {
		t.Errorf("disabled %v, visibility %q", res.Repos[0].Disabled, res.Repos[2].Visibility)
	}
	withForks, err := r.Resolve(context.Background(), platform.Selector{Namespace: org, Forks: true})
	if err != nil || len(withForks.Repos) != 4 {
		t.Errorf("with forks: %d, %v", len(withForks.Repos), err)
	}
	_, err = r.Resolve(context.Background(), platform.Selector{Namespace: org, Topics: []string{"python"}})
	wantClass(t, "topics", err, platform.ClassInvalid)
	_, err = r.Resolve(context.Background(), platform.Selector{Namespace: "Billing"})
	if !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("a project as namespace: %v", err)
	}
}

func TestRemote(t *testing.T) {
	s := newAPIServer(t)
	token := testToken(t)
	r := newTestReader(t, s, token)
	repo := testRepo()
	repo.Host = r.c.host
	repo.Path = "My Project/api"
	rem, err := r.Remote(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL != s.url()+"/My%20Project/_git/api" {
		t.Errorf("URL %q", rem.URL)
	}
	h, err := rem.Header(context.Background())
	if err != nil || h != "Basic "+base64.StdEncoding.EncodeToString([]byte("touchmark:"+token)) {
		t.Errorf("git header %q, %v", h, err)
	}
	anon, err := newTestReader(t, s, "").Remote(context.Background(), repo)
	if err != nil || anon.Header != nil {
		t.Errorf("anonymous remote: %+v, %v", anon, err)
	}
	repo.Host = "elsewhere.example"
	_, err = r.Remote(context.Background(), repo)
	wantClass(t, "another host", err, platform.ClassInvalid)
}
