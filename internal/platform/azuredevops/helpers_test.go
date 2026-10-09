package azuredevops

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The fixtures below follow the REST reference of Azure DevOps Services 7.1
// (https://learn.microsoft.com/en-us/rest/api/azure/devops/?view=azure-devops-rest-7.1)
// and anonymous answers of public projects (dnceng-public/public,
// azure-sdk/public, 2026-10-09): every field the driver reads, and a few it
// ignores, as the API sends them.

// org is the organization of the tests: the server answers under /acme.
const org = "acme"

// Ids of the tests.
const (
	projectID = "cbb18261-c48f-4abb-8651-8cdcb5474649"
	repoID    = "2459d599-fdb2-4d28-9810-daeec061cf90"
	otherRepo = "382d78c1-5459-4030-925f-949b4a10dfff"
	forkRepo  = "5febef5a-833d-4e14-b9c0-14cb638f91e6"
	botID     = "56b0f042-b05e-86d9-b0f7-c5ddb1e76385"
	userID    = "08c37c0d-dc6c-6635-9662-1fa8dad90878"
)

// apiServer is an httptest server that answers the routes a test declares
// and records every request. An undeclared route fails the test, and so
// does any write unless the test is the writer's (allowWrites). Routes are
// escaped paths, from /acme on. Every request must carry api-version=7.1
// and X-TFS-FedAuthRedirect: Suppress.
type apiServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []apiCall
	// allowWrites lets the writer's tests write (their declared routes).
	allowWrites bool
}

// apiCall is one request the server received.
type apiCall struct {
	Method, Path string // Path escaped
	Query        url.Values
	Auth         string // Authorization
	ContentType  string
	Body         []byte
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() {
		if s.allowWrites {
			return
		}
		for _, c := range s.writes() {
			t.Errorf("the reader wrote: %s %s", c.Method, c.Path)
		}
	})
	return s
}

// url is the provider's url (and api_url): the server's /acme.
func (s *apiServer) url() string { return s.srv.URL + "/" + org }

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	p := r.URL.EscapedPath()
	s.mu.Lock()
	s.calls = append(s.calls, apiCall{Method: r.Method, Path: p, Query: r.URL.Query(), Auth: r.Header.Get("Authorization"),
		ContentType: r.Header.Get("Content-Type"), Body: body})
	h := s.routes[r.Method+" "+p]
	s.mu.Unlock()
	if r.URL.Query().Get("api-version") != "7.1" {
		s.t.Errorf("%s %s without api-version=7.1", r.Method, p)
	}
	if r.Header.Get("X-TFS-FedAuthRedirect") != "Suppress" {
		s.t.Errorf("%s %s without X-TFS-FedAuthRedirect: Suppress", r.Method, p)
	}
	if h == nil {
		s.t.Errorf("unexpected request %s %s?%s", r.Method, p, r.URL.RawQuery)
		writeJSON(w, http.StatusNotFound, errorBody("NotFoundException", "not found"))
		return
	}
	h(w, r)
}

// handle declares a route: method and an escaped path from /acme on
// ("/acme/_apis/git/repositories/…").
func (s *apiServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// json declares a GET route that answers status with v as JSON.
func (s *apiServer) json(path string, status int, v any) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// requests returns the recorded calls of method to path ("" matches all).
func (s *apiServer) requests(method, path string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8; api-version=7.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is Azure DevOps' error body.
func errorBody(typeKey, msg string) map[string]any {
	return map[string]any{"$id": "1", "innerException": nil, "message": msg,
		"typeName": "Microsoft.TeamFoundation.Git.Server." + typeKey + ", Microsoft.TeamFoundation.SourceControl.WebServer",
		"typeKey":  typeKey, "errorCode": 0, "eventId": 3000}
}

// collection is a collection answer.
func collection(items ...any) map[string]any {
	if items == nil {
		items = []any{}
	}
	return map[string]any{"count": len(items), "value": items}
}

// testToken returns a token made at run time, as long as a real PAT.
func testToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 26)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)[:52]
}

// provider returns the resolved Azure DevOps provider at the server.
func (s *apiServer) provider() config.ResolvedProvider {
	u, _ := url.Parse(s.srv.URL)
	return config.ResolvedProvider{
		Provider:  config.Provider{ID: "ado", Type: "azure-devops", URL: s.url(), APIURL: s.url()},
		Host:      strings.ToLower(u.Host),
		APIURL:    s.url(),
		EnvPrefix: "TOUCHMARK_ADO_",
	}
}

// httpClient is the HTTP client of the tests.
func httpClient() *httpx.Client { return httpx.New(httpx.Options{}) }

// newTestReader returns a reader with token ("" reads anonymously).
func newTestReader(t *testing.T, s *apiServer, token string) *reader {
	t.Helper()
	cred := auth.Credential{}
	if token != "" {
		cred = auth.Credential{Kind: auth.Token, Token: token}
	}
	r, err := NewReader(s.provider(), cred, httpClient())
	if err != nil {
		t.Fatal(err)
	}
	return r.(*reader)
}

// newTestWriter returns a writer with token; its routes may write.
func newTestWriter(t *testing.T, s *apiServer, token string) *writer {
	t.Helper()
	s.allowWrites = true
	w, err := NewWriter(s.provider(), auth.Credential{Kind: auth.Token, Token: token}, httpClient())
	if err != nil {
		t.Fatal(err)
	}
	return w.(*writer)
}

// Paths of the routes.
func apisPath(segs ...string) string { return "/" + org + "/_apis/" + strings.Join(segs, "/") }
func repoPath(segs ...string) string {
	return apisPath(append([]string{"git", "repositories", repoID}, segs...)...)
}

// repoJSON is a repository as the API answers it; defaultBranch "" leaves
// it out (an empty repository).
func repoJSON(id, project, name, defaultBranch string) map[string]any {
	r := map[string]any{
		"id":   id,
		"name": name,
		"url":  "https://dev.azure.com/" + org + "/" + projectID + "/_apis/git/repositories/" + id,
		"project": map[string]any{"id": projectID, "name": project, "state": "wellFormed", "visibility": "private",
			"revision": 20, "lastUpdateTime": "2023-05-16T15:27:53.67Z"},
		"size":            12337,
		"remoteUrl":       "https://" + org + "@dev.azure.com/" + org + "/" + project + "/_git/" + name,
		"sshUrl":          "git@ssh.dev.azure.com:v3/" + org + "/" + project + "/" + name,
		"webUrl":          "https://dev.azure.com/" + org + "/" + project + "/_git/" + name,
		"isDisabled":      false,
		"isInMaintenance": false,
	}
	if defaultBranch != "" {
		r["defaultBranch"] = "refs/heads/" + defaultBranch
	}
	return r
}

// identityRef is an identity as pull requests name it; subject is its
// descriptor ("svc.…", "aad.…").
func identityRef(id, name, subject string) map[string]any {
	return map[string]any{"displayName": name, "id": id, "uniqueName": nil, "descriptor": subject,
		"url": "https://spsprodcus4.vssps.visualstudio.com/A10a5dd58/_apis/Identities/" + id}
}

// testRepo is the platform.Repo of repoJSON(repoID, "Billing", "api", "main").
func testRepo() platform.Repo {
	return platform.Repo{Host: "x", ID: repoID, Path: "Billing/api", DefaultBranch: "main", ObjectFormat: "sha1"}
}

// prJSON is a pull request as the API answers it.
func prJSON(id int64, status, head, base string, author map[string]any, desc string) map[string]any {
	p := map[string]any{
		"repository":            repoJSON(repoID, "Billing", "api", "main"),
		"pullRequestId":         id,
		"codeReviewId":          id,
		"status":                status,
		"createdBy":             author,
		"creationDate":          "2026-02-18T04:35:01.9643061Z",
		"title":                 "chore: sync engineering assets",
		"description":           desc,
		"sourceRefName":         "refs/heads/" + head,
		"targetRefName":         "refs/heads/" + base,
		"mergeStatus":           "succeeded",
		"isDraft":               false,
		"lastMergeSourceCommit": map[string]any{"commitId": "3aae318f1661c50c34effbbf6882119ed161f2d6"},
		"reviewers":             []any{},
		"url":                   "https://dev.azure.com/" + org + "/" + projectID + "/_apis/git/repositories/" + repoID + "/pullRequests/" + fmt.Sprint(id),
	}
	if status != statusActive {
		p["closedDate"] = "2026-02-18T05:35:05.1682215Z"
	}
	return p
}

// propsJSON is a pull request's properties with the marker line ("" for
// none).
func propsJSON(line string) map[string]any {
	v := map[string]any{
		"Microsoft.Git.PullRequest.SourceRefName": map[string]any{"$type": "System.String", "$value": "refs/heads/touchmark/acme-eng"},
	}
	if line != "" {
		v[markerProperty] = map[string]any{"$type": "System.String", "$value": line}
	}
	return map[string]any{"count": len(v), "value": v}
}

// gitBlobID is git's id of a blob with content.
func gitBlobID(content string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// classOf asserts the class of err.
func wantClass(t *testing.T, what string, err error, want platform.Class) {
	t.Helper()
	if got := platform.ClassOf(err); got != want {
		t.Errorf("%s: class %v (%v), want %v", what, got, err, want)
	}
}
