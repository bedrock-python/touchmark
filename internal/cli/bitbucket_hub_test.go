package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// bbHubToken is the hub's access token on Bitbucket Pipelines.
const bbHubToken = "bb-hub-access-token-0123456789"

// bbPipelines is a step of the hub's pipelines on Bitbucket.
func bbPipelines(extra map[string]string) map[string]string {
	vars := map[string]string{
		"CI":                        "true",
		"BITBUCKET_BUILD_NUMBER":    "12",
		"BITBUCKET_REPO_UUID":       "{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}",
		"BITBUCKET_REPO_FULL_NAME":  "acme/engineering-assets",
		"BITBUCKET_REPO_IS_PRIVATE": "true",
		hubch.BitbucketTokenVar:     bbHubToken,
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// bbHubAPI serves the hub repository's answers to the hub channel and
// records the comments written on pull request 41.
type bbHubAPI struct {
	*httptest.Server
	mu     sync.Mutex
	writes []string // "METHOD path: raw"
	auth   map[string]bool
}

func newBBHubAPI(t *testing.T) *bbHubAPI {
	t.Helper()
	s := &bbHubAPI{auth: map[string]bool{}}
	const repo = "/2.0/repositories/acme/engineering-assets"
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auth[r.Header.Get("Authorization")] = true
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+bbHubToken {
			http.Error(w, `{"type": "error"}`, http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo:
			fmt.Fprint(w, `{"uuid": "{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}", "full_name": "acme/engineering-assets", "is_private": true, "mainbranch": {"name": "master"}}`)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/pullrequests/41/comments":
			fmt.Fprint(w, `{"pagelen": 100, "values": []}`)
		case r.Method == http.MethodPost && r.URL.Path == repo+"/pullrequests/41/comments":
			var in struct {
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			s.writes = append(s.writes, r.Method+" "+r.URL.Path+": "+in.Content.Raw)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": 9}`)
		default:
			http.Error(w, `{"type": "error"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// detectAt is hubch.Detect of vars with the API of srv.
func detectAt(vars map[string]string, srv *httptest.Server) hubch.Context {
	c := hubch.Detect(func(k string) string { return vars[k] }, nil)
	c.APIURL = srv.URL + "/2.0"
	return c
}

// On Bitbucket, Pipelines names no default branch: the hub channel reads it
// with the hub's access token before any guard.
func TestHubRepositoryBitbucket(t *testing.T) {
	srv := newBBHubAPI(t)
	vars := bbPipelines(map[string]string{"BITBUCKET_BRANCH": "master"})
	hctx, warnings := hubRepository(t.Context(), detectAt(vars, srv.Server), func(k string) string { return vars[k] })
	if len(warnings) != 0 || hctx.DefaultBranch != "master" || hctx.Visibility != "private" {
		t.Errorf("context %+v, warnings %q", hctx, warnings)
	}
	// Without the token a private hub is not read: a warning, no default
	// branch, and so distribute's guard refuses.
	delete(vars, hubch.BitbucketTokenVar)
	hctx, warnings = hubRepository(t.Context(), detectAt(vars, srv.Server), func(k string) string { return vars[k] })
	if hctx.DefaultBranch != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "a private hub needs TOUCHMARK_PIPELINES_TOKEN") {
		t.Errorf("without a token: %+v, %q", hctx, warnings)
	}
	// Other CIs are left as they are.
	gh := hubch.Context{CI: hubch.GitHubActions, DefaultBranch: ""}
	if got, w := hubRepository(t.Context(), gh, func(string) string { return "" }); got != gh || w != nil {
		t.Errorf("GitHub: %+v, %q", got, w)
	}
}

// plan --comment on a Bitbucket hub pull request: one comment through the
// hub channel with the hub's access token, its marker a Markdown reference
// definition and its body without HTML, which Bitbucket would show as text.
func TestPlanCommentBitbucket(t *testing.T) {
	srv := newBBHubAPI(t)
	vars := bbPipelines(map[string]string{"BITBUCKET_BRANCH": "feature", "BITBUCKET_PR_ID": "41", "BITBUCKET_PR_DESTINATION_BRANCH": "master"})
	getenv := func(k string) string { return vars[k] }
	hctx := detectAt(vars, srv.Server)
	if pr := hubPR(hctx, getenv); pr != 41 {
		t.Fatalf("hubPR = %d", pr)
	}
	reg := redact.New()
	reg.Add(bbHubToken)
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
	if !srv.auth["Bearer "+bbHubToken] || len(srv.auth) != 1 {
		t.Errorf("Authorization headers %v", srv.auth)
	}
	// Every other CI keeps its HTML comment, byte for byte.
	for _, ci := range []hubch.CI{hubch.GitHubActions, hubch.GiteaActions, hubch.ForgejoActions} {
		if got := planCommentMarkerOf(ci, "acme-eng"); got != "<!-- touchmark plan: acme-eng -->" {
			t.Errorf("%s: marker %q", ci, got)
		}
	}
}

func TestHubAPIHostBitbucket(t *testing.T) {
	for _, tc := range []struct{ typ, host, want string }{
		{"bitbucket", "bitbucket.org", "api.bitbucket.org"},
		{"bitbucket", "Bitbucket.org:443", "api.bitbucket.org"},
		{"github", "bitbucket.org", "bitbucket.org"},
	} {
		if got := hubAPIHost(tc.typ, tc.host); got != tc.want {
			t.Errorf("hubAPIHost(%s, %s) = %s, want %s", tc.typ, tc.host, got, tc.want)
		}
	}
}

// --hub-fp takes a Bitbucket repository UUID as BITBUCKET_REPO_UUID gives
// it, and keeps it in the form the CI reports.
func TestHubFPBitbucket(t *testing.T) {
	var f fingerprintFlag
	if err := f.Set("bitbucket.org/{3F2A8D4E-1B6C-4F0A-9E7D-5C2B1A0F9E8D}"); err != nil || string(f) != "bitbucket.org/3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d" {
		t.Errorf("--hub-fp: %q, %v", f, err)
	}
	for _, bad := range []string{"bitbucket.org/{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d", "bitbucket.org/3f2a8d4e", "bitbucket.org/{}"} {
		if err := f.Set(bad); err == nil {
			t.Errorf("--hub-fp %s: no error", bad)
		}
	}
	vars := bbPipelines(map[string]string{"BITBUCKET_BRANCH": "master"})
	hctx := hubch.Detect(func(k string) string { return vars[k] }, nil)
	fp, warnings, err := planFingerprint(hctx, "bitbucket.org/3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d")
	if err != nil || len(warnings) != 0 || fp != "bitbucket.org/3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d" {
		t.Errorf("planFingerprint: %q, %q, %v", fp, warnings, err)
	}
}
