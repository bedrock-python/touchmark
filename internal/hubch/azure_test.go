package hubch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/httpx"
)

// The hub repository on Azure DevOps, as Build.Repository.ID gives its id,
// and as the fingerprint carries it.
const (
	azRepo    = "0B7E5A2C-9D4F-4E1B-8A3C-6F5D2E1C0B9A"
	azID      = "0b7e5a2c-9d4f-4e1b-8a3c-6f5d2e1c0b9a"
	azProject = "7d1e3c5a-2b4f-4a6e-9c8d-1f0e2d3c4b5a"
	azSelf    = "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"
	azTok     = "az-job-access-token-0123456789"
)

// azureEnv is a step of Azure Pipelines building the hub (the predefined
// variables of
// https://learn.microsoft.com/en-us/azure/devops/pipelines/build/variables),
// with extra variables over it ("" removes one).
func azureEnv(extra map[string]string) func(string) string {
	vars := map[string]string{
		"TF_BUILD":                  "True",
		"SYSTEM_COLLECTIONURI":      "https://dev.azure.com/acme/",
		"SYSTEM_TEAMPROJECT":        "Platform",
		"SYSTEM_TEAMPROJECTID":      azProject,
		"BUILD_REPOSITORY_PROVIDER": "TfsGit",
		"BUILD_REPOSITORY_ID":       azRepo,
		"BUILD_REPOSITORY_NAME":     "engineering-assets",
		"BUILD_REPOSITORY_URI":      "https://acme@dev.azure.com/acme/Platform/_git/engineering-assets",
	}
	for k, v := range extra {
		if v == "" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	return envOf(vars)
}

func TestDetectAzure(t *testing.T) {
	base := Context{CI: AzurePipelines, ServerURL: "https://dev.azure.com/acme", Host: "dev.azure.com", APIURL: "https://dev.azure.com/acme",
		RepoID: azID, RepoPath: "Platform/engineering-assets", RepositoryURL: "https://dev.azure.com/acme/Platform/_git/engineering-assets"}
	with := func(f func(*Context)) Context {
		c := base
		f(&c)
		return c
	}
	main := map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "IndividualCI"}
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  Context
	}{
		{"a push", main, with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "push", "main", true })},
		{"a batched push", map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "BatchedCI"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "push", "main", true })},
		{"a schedule", map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "Schedule"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "schedule", "main", true })},
		{"a manual run of a branch with a slash", map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/team/x", "BUILD_REASON": "Manual"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "manual", "team/x", true })},
		{"the deployment job", map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "IndividualCI", "ENVIRONMENT_NAME": "touchmark-distribute"},
			with(func(c *Context) {
				c.Event, c.RefName, c.RefIsBranch, c.Environment = "push", "main", true, "touchmark-distribute"
			})},
		{"a pull request", map[string]string{"BUILD_SOURCEBRANCH": "refs/pull/41/merge", "BUILD_REASON": "PullRequest",
			"SYSTEM_PULLREQUEST_SOURCEBRANCH": "refs/heads/feature", "SYSTEM_PULLREQUEST_PULLREQUESTID": "41"},
			with(func(c *Context) { c.Event, c.RefName = "pull_request", "feature" })},
		// A merge ref is a pull request's build whatever the reason says.
		{"a merge ref run by hand", map[string]string{"BUILD_SOURCEBRANCH": "refs/pull/41/merge", "BUILD_REASON": "Manual"},
			with(func(c *Context) { c.Event = "pull_request" })},
		{"a pull request from main", map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "PullRequest",
			"SYSTEM_PULLREQUEST_SOURCEBRANCH": "refs/heads/main"},
			with(func(c *Context) { c.Event, c.RefName = "pull_request", "main" })},
		{"a tag", map[string]string{"BUILD_SOURCEBRANCH": "refs/tags/v1", "BUILD_REASON": "IndividualCI"},
			with(func(c *Context) { c.Event, c.RefName = "push", "v1" })},
		{"another pipeline's completion", map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "ResourceTrigger"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "resourcetrigger", "main", true })},
		{"the older organization URL", map[string]string{"SYSTEM_COLLECTIONURI": "https://acme.visualstudio.com/", "BUILD_SOURCEBRANCH": "refs/heads/main",
			"BUILD_REASON": "Manual"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "manual", "main", true })},
		{"the collection URL in its other variable", map[string]string{"SYSTEM_COLLECTIONURI": "",
			"SYSTEM_TEAMFOUNDATIONCOLLECTIONURI": "https://dev.azure.com/acme/", "BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "Manual"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "manual", "main", true })},
		{"Azure DevOps Server", map[string]string{"SYSTEM_COLLECTIONURI": "https://tfs.acme.example/DefaultCollection/", "BUILD_SOURCEBRANCH": "refs/heads/main",
			"BUILD_REASON": "Manual"},
			with(func(c *Context) {
				c.ServerURL, c.APIURL, c.Host, c.Event, c.RefName, c.RefIsBranch = "", "", "", "manual", "main", true
			})},
		{"a garbled repository id", map[string]string{"BUILD_REPOSITORY_ID": "0b7e5a2c", "BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "Manual"},
			with(func(c *Context) { c.RepoID, c.Event, c.RefName, c.RefIsBranch = "", "manual", "main", true })},
		// A pipeline of Azure building a GitHub repository has no hub here.
		{"a GitHub repository", map[string]string{"BUILD_REPOSITORY_PROVIDER": "GitHub", "BUILD_SOURCEBRANCH": "refs/heads/main"},
			Context{CI: AzurePipelines}},
	} {
		got := Detect(azureEnv(tc.extra), nil)
		if got != tc.want {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.name, got, tc.want)
		}
	}
	c := Detect(azureEnv(main), nil)
	if fp := c.Fingerprint(); fp != "dev.azure.com/"+azID {
		t.Errorf("fingerprint %q", fp)
	}
	if c := Detect(envOf(map[string]string{"GITLAB_CI": "true", "TF_BUILD": "True"}), nil); c.CI != GitLabCI {
		t.Errorf("GitLab with a stray variable: %s", c.CI)
	}
	if tok := Token(c, azureEnv(map[string]string{AzureTokenVar: " " + azTok + " ", "GITHUB_TOKEN": "x"})); tok != azTok {
		t.Errorf("token %q", tok)
	}
}

// azThread is a comment thread of pull request 41 on azServer.
type azThread struct {
	ID       int64       `json:"id"`
	Status   string      `json:"status,omitempty"`
	Comments []azComment `json:"comments"`
}

type azComment struct {
	ID          int64  `json:"id"`
	Content     string `json:"content,omitempty"`
	CommentType string `json:"commentType"`
	IsDeleted   bool   `json:"isDeleted,omitempty"`
	Author      struct {
		ID string `json:"id"`
	} `json:"author"`
}

// azServer is the REST API of Azure DevOps for one organization, acme,
// with the hub repository, its project, its default branch main, and pull
// request 41 with its threads. The job access token acts as azSelf.
type azServer struct {
	*httptest.Server
	mu sync.Mutex
	// noVisibility leaves the project's visibility out of the repository's
	// answer, as the reference's sample does.
	noVisibility bool
	signIn       bool
	threads      []azThread
	posted       []map[string]any
	patched      int
	requests     []string
}

func newAzServer(t *testing.T) *azServer {
	t.Helper()
	s := &azServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *azServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	if s.signIn {
		w.Header().Set("Location", "https://spsprodweu5.vssps.visualstudio.com/_signin")
		w.WriteHeader(http.StatusFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+azTok || r.Header.Get("X-Tfs-Fedauthredirect") != "Suppress" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	version := r.URL.Query().Get("api-version")
	repo := "/acme/_apis/git/repositories/" + azID
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == repo && version == "7.1":
		project := map[string]any{"id": azProject, "name": "Platform"}
		if !s.noVisibility {
			project["visibility"] = "private"
		}
		reply(map[string]any{"id": azID, "name": "engineering-assets", "defaultBranch": "refs/heads/main", "project": project})
	case r.Method == http.MethodGet && r.URL.Path == "/acme/_apis/projects/"+azProject && version == "7.1":
		reply(map[string]any{"id": azProject, "name": "Platform", "visibility": "public"})
	case r.Method == http.MethodGet && r.URL.Path == repo+"/refs" && version == "7.1":
		if r.URL.Query().Get("filter") != "heads/main" {
			reply(map[string]any{"value": []any{}})
			return
		}
		reply(map[string]any{"value": []any{
			map[string]any{"name": "refs/heads/main-old", "objectId": strings.Repeat("1", 40)},
			map[string]any{"name": "refs/heads/main", "objectId": strings.Repeat("A", 40)},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/acme/_apis/connectionData" && version == "7.1-preview.1":
		reply(map[string]any{"authenticatedUser": map[string]any{"id": azSelf}})
	case r.Method == http.MethodGet && r.URL.Path == repo+"/pullRequests/41/threads" && version == "7.1":
		reply(map[string]any{"value": s.threads})
	case r.Method == http.MethodPost && r.URL.Path == repo+"/pullRequests/41/threads" && version == "7.1":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.posted = append(s.posted, body)
		reply(map[string]any{"id": 99})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, repo+"/pullRequests/41/threads/") && version == "7.1":
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for i := range s.threads {
			for j := range s.threads[i].Comments {
				c := &s.threads[i].Comments[j]
				if r.URL.Path == repo+"/pullRequests/41/threads/"+itoa(s.threads[i].ID)+"/comments/"+itoa(c.ID) {
					if !strings.EqualFold(c.Author.ID, azSelf) {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					c.Content = body.Content
					s.patched++
					reply(c)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// azChannel is the channel of a step building the hub on s.
func azChannel(t *testing.T, s *azServer, token string) *azureChannel {
	t.Helper()
	c := Detect(azureEnv(map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/main", "BUILD_REASON": "IndividualCI"}), nil)
	c.APIURL = s.URL + "/acme"
	ch, err := New(c, httpx.New(httpx.Options{}), token)
	if err != nil {
		t.Fatal(err)
	}
	return ch.(*azureChannel)
}

func TestAzureChannelRepository(t *testing.T) {
	s := newAzServer(t)
	ch := azChannel(t, s, azTok)
	info, err := ch.Repository(t.Context())
	if err != nil || info != (RepoInfo{DefaultBranch: "main", Visibility: "private"}) {
		t.Fatalf("Repository: %+v, %v", info, err)
	}
	head, err := ch.Head(t.Context())
	if err != nil || head != strings.Repeat("a", 40) {
		t.Fatalf("Head: %q, %v", head, err)
	}
	if v, err := ch.Visibility(t.Context()); err != nil || v != "private" {
		t.Errorf("Visibility: %q, %v", v, err)
	}

	// Without the project's visibility in the answer, the project tells it.
	s2 := newAzServer(t)
	s2.noVisibility = true
	info, err = azChannel(t, s2, azTok).Repository(t.Context())
	if err != nil || info.Visibility != "public" {
		t.Errorf("Repository without visibility: %+v, %v", info, err)
	}

	// A refused token goes to the sign-in page: say so, and how to map it.
	s3 := newAzServer(t)
	s3.signIn = true
	_, err = azChannel(t, s3, azTok).Repository(t.Context())
	if err == nil || !strings.Contains(err.Error(), "sign-in page") || !strings.Contains(err.Error(), AzureTokenVar) {
		t.Errorf("sign-in: %v", err)
	}

	// Without a token the server refuses; nothing leaks.
	if _, err := azChannel(t, s, "").Repository(t.Context()); err == nil {
		t.Error("anonymous read succeeded")
	}
}

func TestAzureChannelComment(t *testing.T) {
	s := newAzServer(t)
	ch := azChannel(t, s, azTok)
	const marker = `[touchmark-plan]: # "touchmark plan: acme-eng"`
	body := "plan\n\n" + marker + "\n"

	// No comment yet: a closed thread with it.
	if got, err := ch.UpsertComment(t.Context(), 41, marker, body); err != nil || got != CommentCreated {
		t.Fatalf("first: %s, %v", got, err)
	}
	if len(s.posted) != 1 || s.posted[0]["status"] != "closed" {
		t.Fatalf("posted %v", s.posted)
	}
	comments, _ := s.posted[0]["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["content"] != body {
		t.Fatalf("posted comments %v", comments)
	}

	// Someone else's comment with the marker is left alone; touchmark's own
	// is edited, and left as it is when it says the same.
	other := azComment{ID: 1, Content: "copied " + marker, CommentType: "text"}
	other.Author.ID = "11111111-2222-3333-4444-555555555555"
	mine := azComment{ID: 2, Content: "old\n\n" + marker + "\n", CommentType: "text"}
	mine.Author.ID = strings.ToUpper(azSelf)
	deleted := azComment{ID: 3, CommentType: "text", IsDeleted: true}
	deleted.Author.ID = azSelf
	system := azComment{ID: 4, Content: marker, CommentType: "system"}
	system.Author.ID = azSelf
	s.threads = []azThread{{ID: 7, Comments: []azComment{other}}, {ID: 8, Comments: []azComment{deleted, system, mine}}}
	s.posted = nil
	if got, err := ch.UpsertComment(t.Context(), 41, marker, body); err != nil || got != CommentUpdated {
		t.Fatalf("update: %s, %v", got, err)
	}
	if s.patched != 1 || s.threads[1].Comments[2].Content != body || s.threads[0].Comments[0].Content != "copied "+marker || len(s.posted) != 0 {
		t.Fatalf("after update: patched %d, threads %+v, posted %v", s.patched, s.threads, s.posted)
	}
	if got, err := ch.UpsertComment(t.Context(), 41, marker, body); err != nil || got != CommentUnchanged {
		t.Fatalf("again: %s, %v", got, err)
	}

	if _, err := ch.UpsertComment(t.Context(), 0, marker, body); err == nil {
		t.Error("pull request 0")
	}
	if _, err := ch.UpsertComment(t.Context(), 41, marker, "no marker"); err == nil {
		t.Error("a body without its marker")
	}
}

func TestAzureChannelNeedsTheRepository(t *testing.T) {
	c := Detect(azureEnv(map[string]string{"BUILD_REPOSITORY_PROVIDER": "GitHub"}), nil)
	if _, err := New(c, nil, azTok); err == nil {
		t.Error("a channel without a repository")
	}
}
