package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The hub on Azure DevOps: its repository and project, and the job access
// token of its pipeline, which acts as azBuildService.
const (
	azHubRepo      = "0b7e5a2c-9d4f-4e1b-8a3c-6f5d2e1c0b9a"
	azHubProject   = "7d1e3c5a-2b4f-4a6e-9c8d-1f0e2d3c4b5a"
	azBuildService = "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"
	azJobToken     = "az-job-access-token-0123456789"
)

// azPipelines is a step of the hub's pipeline on Azure Pipelines, with the
// job access token mapped into it.
func azPipelines(extra map[string]string) map[string]string {
	vars := map[string]string{
		"TF_BUILD":                  "True",
		"SYSTEM_COLLECTIONURI":      "https://dev.azure.com/acme/",
		"SYSTEM_TEAMPROJECT":        "Platform",
		"BUILD_REPOSITORY_PROVIDER": "TfsGit",
		"BUILD_REPOSITORY_ID":       azHubRepo,
		"BUILD_REPOSITORY_NAME":     "engineering-assets",
		hubch.AzureTokenVar:         azJobToken,
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// azHubAPI serves the hub repository's answers to the hub channel and
// records the comment threads created on pull request 41.
type azHubAPI struct {
	*httptest.Server
	mu     sync.Mutex
	writes []string // "METHOD path: content"
	auth   map[string]bool
}

func newAzHubAPI(t *testing.T) *azHubAPI {
	t.Helper()
	s := &azHubAPI{auth: map[string]bool{}}
	const repo = "/acme/_apis/git/repositories/" + azHubRepo
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auth[r.Header.Get("Authorization")] = true
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+azJobToken {
			http.Error(w, `{"message": "TF400813"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo:
			fmt.Fprint(w, `{"id": "`+azHubRepo+`", "name": "engineering-assets", "defaultBranch": "refs/heads/master",
				"project": {"id": "`+azHubProject+`", "name": "Platform", "visibility": "private"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/acme/_apis/connectionData":
			fmt.Fprint(w, `{"authenticatedUser": {"id": "`+azBuildService+`"}}`)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/pullRequests/41/threads":
			fmt.Fprint(w, `{"count": 0, "value": []}`)
		case r.Method == http.MethodPost && r.URL.Path == repo+"/pullRequests/41/threads":
			var in struct {
				Comments []struct {
					Content string `json:"content"`
				} `json:"comments"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			for _, c := range in.Comments {
				s.writes = append(s.writes, r.Method+" "+r.URL.Path+": "+c.Content)
			}
			fmt.Fprint(w, `{"id": 9}`)
		default:
			http.Error(w, `{"message": "not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// azDetectAt is hubch.Detect of vars with the API of srv.
func azDetectAt(vars map[string]string, srv *httptest.Server) hubch.Context {
	c := hubch.Detect(func(k string) string { return vars[k] }, nil)
	c.APIURL = srv.URL + "/acme"
	return c
}

// On Azure Pipelines no variable names the default branch: the hub channel
// reads it with the job access token before any guard.
func TestHubRepositoryAzure(t *testing.T) {
	srv := newAzHubAPI(t)
	vars := azPipelines(map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/master", "BUILD_REASON": "IndividualCI"})
	hctx, warnings := hubRepository(t.Context(), azDetectAt(vars, srv.Server), func(k string) string { return vars[k] })
	if len(warnings) != 0 || hctx.DefaultBranch != "master" || hctx.Visibility != "private" {
		t.Errorf("context %+v, warnings %q", hctx, warnings)
	}
	// A step that does not map the token: a warning that says how, no
	// default branch, and so distribute's guard refuses.
	delete(vars, hubch.AzureTokenVar)
	hctx, warnings = hubRepository(t.Context(), azDetectAt(vars, srv.Server), func(k string) string { return vars[k] })
	if hctx.DefaultBranch != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "env: SYSTEM_ACCESSTOKEN: $(System.AccessToken)") {
		t.Errorf("without a token: %+v, %q", hctx, warnings)
	}
}

// plan --comment on an Azure DevOps hub pull request: one closed thread
// through the hub channel with the job access token, plain Markdown with a
// reference definition as its marker.
func TestPlanCommentAzure(t *testing.T) {
	srv := newAzHubAPI(t)
	vars := azPipelines(map[string]string{"BUILD_SOURCEBRANCH": "refs/pull/41/merge", "BUILD_REASON": "PullRequest",
		"SYSTEM_PULLREQUEST_SOURCEBRANCH": "refs/heads/feature", "SYSTEM_PULLREQUEST_PULLREQUESTID": "41"})
	getenv := func(k string) string { return vars[k] }
	hctx := azDetectAt(vars, srv.Server)
	if pr := hubPR(hctx, getenv); pr != 41 {
		t.Fatalf("hubPR = %d", pr)
	}
	reg := redact.New()
	reg.Add(azJobToken)
	ch, err := hubch.New(hctx, httpx.New(httpx.Options{Redact: reg}), hubch.Token(hctx, getenv))
	if err != nil {
		t.Fatal(err)
	}
	rep := report.NewDelivery("plan", "dev")
	rep.Hub.ID = "acme-eng"
	rep.Paths = []report.PathChange{{Action: "add", Path: "AGENTS.md", Targets: 2}}
	if w := postPlanComment(t.Context(), hctx, ch, 41, rep, reg); w != "" {
		t.Fatalf("warning %q", w)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.writes) != 1 {
		t.Fatalf("writes %q", srv.writes)
	}
	body := srv.writes[0]
	if !strings.HasSuffix(body, "\n\n[touchmark-plan]: # \"touchmark plan: acme-eng\"\n") || strings.Contains(body, "<") {
		t.Errorf("comment:\n%s", body)
	}
	if !srv.auth["Bearer "+azJobToken] || len(srv.auth) != 1 {
		t.Errorf("Authorization headers %v", srv.auth)
	}
}

// --hub-fp takes an Azure Repos repository id in any case, and keeps it in
// the form the CI reports.
func TestHubFPAzure(t *testing.T) {
	var f fingerprintFlag
	if err := f.Set("dev.azure.com/" + strings.ToUpper(azHubRepo)); err != nil || string(f) != "dev.azure.com/"+azHubRepo {
		t.Errorf("--hub-fp: %q, %v", f, err)
	}
	vars := azPipelines(map[string]string{"BUILD_SOURCEBRANCH": "refs/heads/master"})
	hctx := hubch.Detect(func(k string) string { return vars[k] }, nil)
	fp, warnings, err := planFingerprint(hctx, "dev.azure.com/"+azHubRepo)
	if err != nil || len(warnings) != 0 || fp != "dev.azure.com/"+azHubRepo {
		t.Errorf("planFingerprint: %q, %q, %v", fp, warnings, err)
	}
	if got := config.CanonicalFingerprint("Dev.Azure.com:443/" + strings.ToUpper(azHubRepo)); got != "dev.azure.com/"+azHubRepo {
		t.Errorf("CanonicalFingerprint: %q", got)
	}
}

// commandGuard breaks every Azure Pipelines logging command, also one split
// across writes, and keeps everything else byte for byte.
func TestCommandGuard(t *testing.T) {
	var out bytes.Buffer
	g := newCommandGuard(&out)
	for _, chunk := range []string{"ok ##vso[task.setvariable variable=X]1\n", "path: ##", "vso[task.prependpath]/tmp", " #", "# end\n", "##v"} {
		if n, err := g.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if err := g.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "ok ##vso [task.setvariable variable=X]1\npath: ##vso [task.prependpath]/tmp ## end\n##v"
	if out.String() != want {
		t.Errorf("guarded %q\nwant    %q", out.String(), want)
	}
}

// Under Azure Pipelines everything touchmark prints goes through the guard;
// elsewhere it is left as it is.
func TestMainBreaksLoggingCommands(t *testing.T) {
	for _, tc := range []struct {
		tfBuild, want string
	}{{"True", "##vso [task.setvariable"}, {"", "##vso[task.setvariable"}} {
		var stdout, stderr bytes.Buffer
		e := &env{stdout: &stdout, stderr: &stderr, getenv: func(k string) string {
			if k == "TF_BUILD" {
				return tc.tfBuild
			}
			return ""
		}}
		if code := e.main(t.Context(), []string{"##vso[task.setvariable variable=X]1"}); code != exitUsage {
			t.Errorf("TF_BUILD=%q: exit %d", tc.tfBuild, code)
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("TF_BUILD=%q: stderr %q lacks %q", tc.tfBuild, stderr.String(), tc.want)
		}
	}
}
