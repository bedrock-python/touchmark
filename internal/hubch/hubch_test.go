package hubch

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
)

// fakeGitEnv makes the test binary act as git: it records its arguments and
// environment as JSON in the file the variable names and prints one
// ls-remote line. Its name must not start with TOUCHMARK_: gitx keeps
// touchmark's variables from every git process.
const fakeGitEnv = "HUBCH_TEST_FAKE_GIT"

const fakeHead = "0123456789abcdef0123456789abcdef01234567"

// TestMain isolates git from the machine's config and proxies, and serves
// as the fake git.
func TestMain(m *testing.M) {
	if out := os.Getenv(fakeGitEnv); out != "" {
		data, _ := json.Marshal(map[string][]string{"args": os.Args[1:], "env": os.Environ()})
		if err := os.WriteFile(out, data, 0o644); err != nil {
			os.Exit(3)
		}
		fmt.Println(fakeHead + "\trefs/heads/main")
		os.Exit(0)
	}
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	home, err := os.MkdirTemp("", "touchmark-hubch-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(home)
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   global,
		"HOME":                home,
		"XDG_CONFIG_HOME":     home,
		"GIT_AUTHOR_NAME":     "touchmark test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "touchmark test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_AUTHOR_DATE":     "1767225600 +0000",
		"GIT_COMMITTER_DATE":  "1767225600 +0000",
		"NO_PROXY":            "127.0.0.1,localhost",
		"no_proxy":            "127.0.0.1,localhost",
	} {
		os.Setenv(k, v)
	}
	for _, k := range []string{"GIT_ASKPASS", "SSH_ASKPASS", "GIT_CONFIG_COUNT", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		os.Unsetenv(k)
	}
	return m.Run()
}

func envOf(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func filesOf(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if s, ok := files[path]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

const (
	githubPush = `{"ref": "refs/heads/main", "after": "3f2c",
		"repository": {"id": 712345678, "node_id": "R_kgDO", "name": "engineering-assets",
			"full_name": "acme/engineering-assets", "private": false, "visibility": "public",
			"default_branch": "main", "owner": {"login": "acme", "id": 1, "type": "Organization"}},
		"sender": {"login": "jdoe"}}`
	githubInternalPR = `{"action": "opened", "number": 41,
		"repository": {"id": 712345678, "private": true, "visibility": "internal", "default_branch": "trunk"},
		"pull_request": {"head": {"ref": "main", "repo": {"id": 999}}}}`
	giteaPush = `{"ref": "refs/heads/main",
		"repository": {"id": 42, "owner": {"id": 3, "login": "acme", "visibility": "public"},
			"name": "hub", "full_name": "acme/hub", "private": false, "internal": false,
			"default_branch": "main"}}`
	forgejoLimited = `{"repository": {"id": "77", "private": false,
		"owner": {"login": "acme", "visibility": "limited"}, "default_branch": "main"}}`
)

func TestDetect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		env   map[string]string
		files map[string]string
		want  Context
	}{
		{
			name: "local",
			env:  map[string]string{"CI": "true", "GITHUB_TOKEN": "ghp_x"},
			want: Context{CI: Local},
		},
		{
			name: "github push",
			env: map[string]string{
				"GITHUB_ACTIONS":       "true",
				"GITHUB_SERVER_URL":    "https://github.com",
				"GITHUB_API_URL":       "https://api.github.com",
				"GITHUB_REPOSITORY_ID": "712345678",
				"GITHUB_REPOSITORY":    "acme/engineering-assets",
				"GITHUB_REF":           "refs/heads/main",
				"GITHUB_REF_NAME":      "main",
				"GITHUB_REF_TYPE":      "branch",
				"GITHUB_EVENT_NAME":    "push",
				"GITHUB_EVENT_PATH":    "/runner/event.json",
			},
			files: map[string]string{"/runner/event.json": githubPush},
			want: Context{
				CI: GitHubActions, ServerURL: "https://github.com", APIURL: "https://api.github.com",
				Host: "github.com", RepoID: "712345678", RepoPath: "acme/engineering-assets",
				DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "push",
				Visibility: "public",
			},
		},
		{
			name: "github pull request on GHES",
			env: map[string]string{
				"GITHUB_ACTIONS":       "true",
				"GITHUB_SERVER_URL":    "https://GHES.Example.com:443/",
				"GITHUB_REPOSITORY_ID": "712345678",
				"GITHUB_REPOSITORY":    "acme/hub",
				"GITHUB_REF":           "refs/pull/41/merge",
				"GITHUB_REF_NAME":      "41/merge",
				"GITHUB_REF_TYPE":      "branch",
				"GITHUB_EVENT_NAME":    "pull_request",
				"GITHUB_EVENT_PATH":    "/runner/event.json",
			},
			files: map[string]string{"/runner/event.json": githubInternalPR},
			want: Context{
				CI: GitHubActions, ServerURL: "https://GHES.Example.com:443", APIURL: "https://GHES.Example.com:443/api/v3",
				Host: "ghes.example.com", RepoID: "712345678", RepoPath: "acme/hub",
				DefaultBranch: "trunk", RefName: "41/merge", Event: "pull_request", Visibility: "internal",
			},
		},
		{
			name: "github without the payload, id or ref type",
			env: map[string]string{
				"GITHUB_ACTIONS":       "TRUE",
				"GITHUB_SERVER_URL":    "https://acme.ghe.com",
				"GITHUB_REPOSITORY_ID": "0712",
				"GITHUB_REPOSITORY":    "acme/hub",
				"GITHUB_REF":           "refs/heads/release/v1",
				"GITHUB_EVENT_NAME":    "workflow_dispatch",
				"GITHUB_EVENT_PATH":    "/missing.json",
			},
			want: Context{
				CI: GitHubActions, ServerURL: "https://acme.ghe.com", APIURL: "https://api.acme.ghe.com",
				Host: "acme.ghe.com", RepoPath: "acme/hub", RefName: "release/v1", RefIsBranch: true,
				Event: "workflow_dispatch",
			},
		},
		{
			name: "github tag",
			env: map[string]string{
				"GITHUB_ACTIONS":    "true",
				"GITHUB_SERVER_URL": "https://github.com",
				"GITHUB_REF":        "refs/tags/v1.0.0",
				"GITHUB_REF_NAME":   "v1.0.0",
				"GITHUB_REF_TYPE":   "tag",
			},
			want: Context{
				CI: GitHubActions, ServerURL: "https://github.com", APIURL: "https://api.github.com",
				Host: "github.com", RefName: "v1.0.0",
			},
		},
		{
			name: "gitea without GITHUB_REPOSITORY_ID",
			env: map[string]string{
				"GITHUB_ACTIONS":    "true",
				"GITEA_ACTIONS":     "true",
				"GITHUB_SERVER_URL": "https://gitea.example.com:3000/",
				"GITHUB_REPOSITORY": "acme/hub",
				"GITHUB_REF":        "refs/heads/main",
				"GITHUB_REF_NAME":   "main",
				"GITHUB_REF_TYPE":   "branch",
				"GITHUB_EVENT_NAME": "push",
				"GITHUB_EVENT_PATH": "event.json",
			},
			files: map[string]string{"event.json": giteaPush},
			want: Context{
				CI: GiteaActions, ServerURL: "https://gitea.example.com:3000", APIURL: "https://gitea.example.com:3000/api/v1",
				Host: "gitea.example.com:3000", RepoID: "42", RepoPath: "acme/hub", DefaultBranch: "main",
				RefName: "main", RefIsBranch: true, Event: "push", Visibility: "public",
			},
		},
		{
			name: "forgejo with a limited owner",
			env: map[string]string{
				"GITHUB_ACTIONS":       "true",
				"GITEA_ACTIONS":        "true",
				"FORGEJO_ACTIONS":      "true",
				"GITHUB_SERVER_URL":    "https://codeberg.org",
				"GITHUB_API_URL":       "https://codeberg.org/api/v1",
				"GITHUB_REPOSITORY_ID": "not-a-number",
				"GITHUB_REPOSITORY":    "acme/hub",
				"GITHUB_EVENT_PATH":    "event.json",
			},
			files: map[string]string{"event.json": forgejoLimited},
			want: Context{
				CI: ForgejoActions, ServerURL: "https://codeberg.org", APIURL: "https://codeberg.org/api/v1",
				Host: "codeberg.org", RepoID: "77", RepoPath: "acme/hub", DefaultBranch: "main",
				Visibility: "internal",
			},
		},
		{
			name: "gitlab branch pipeline",
			env: map[string]string{
				"GITLAB_CI":               "true",
				"CI_SERVER_URL":           "https://GitLab.example.com",
				"CI_API_V4_URL":           "https://gitlab.example.com/api/v4",
				"CI_PROJECT_ID":           "1234",
				"CI_PROJECT_PATH":         "platform/engineering-assets",
				"CI_DEFAULT_BRANCH":       "main",
				"CI_COMMIT_REF_NAME":      "main",
				"CI_COMMIT_BRANCH":        "main",
				"CI_PIPELINE_SOURCE":      "schedule",
				"CI_COMMIT_REF_PROTECTED": "true",
				"CI_ENVIRONMENT_NAME":     "touchmark-distribute",
				"CI_PROJECT_VISIBILITY":   "Private",
				"CI_REPOSITORY_URL":       "https://gitlab-ci-token:glcbt-64_secretJobToken@gitlab.example.com/platform/engineering-assets.git",
				"CI_JOB_TOKEN":            "glcbt-64_secretJobToken",
			},
			want: Context{
				CI: GitLabCI, ServerURL: "https://GitLab.example.com", APIURL: "https://gitlab.example.com/api/v4",
				Host: "gitlab.example.com", RepoID: "1234", RepoPath: "platform/engineering-assets",
				DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "schedule",
				RefProtected: true, Environment: "touchmark-distribute", Visibility: "private",
				RepositoryURL: "https://gitlab.example.com/platform/engineering-assets.git",
			},
		},
		// Scheduled runs whose payload says nothing about the repository
		// (Gitea stored a null payload before go-gitea/gitea#38446): the
		// visibility and default branch stay unknown, and plan treats the
		// hub as public (distribute.Plan).
		{
			name: "gitea schedule with a null payload",
			env: map[string]string{
				"GITEA_ACTIONS":        "true",
				"GITHUB_ACTIONS":       "true",
				"GITHUB_SERVER_URL":    "https://gitea.example.com",
				"GITHUB_REPOSITORY_ID": "42",
				"GITHUB_REPOSITORY":    "acme/hub",
				"GITHUB_REF":           "refs/heads/main",
				"GITHUB_REF_NAME":      "main",
				"GITHUB_REF_TYPE":      "branch",
				"GITHUB_EVENT_NAME":    "schedule",
				"GITHUB_EVENT_PATH":    "event.json",
			},
			files: map[string]string{"event.json": "null"},
			want: Context{
				CI: GiteaActions, ServerURL: "https://gitea.example.com", APIURL: "https://gitea.example.com/api/v1",
				Host: "gitea.example.com", RepoID: "42", RepoPath: "acme/hub", RefName: "main", RefIsBranch: true,
				Event: "schedule",
			},
		},
		{
			name: "forgejo schedule with an empty payload",
			env: map[string]string{
				"FORGEJO_ACTIONS":   "true",
				"GITHUB_SERVER_URL": "https://codeberg.org",
				"GITHUB_REPOSITORY": "acme/hub",
				"GITHUB_EVENT_NAME": "schedule",
				"GITHUB_EVENT_PATH": "event.json",
			},
			files: map[string]string{"event.json": "{}"},
			want: Context{
				CI: ForgejoActions, ServerURL: "https://codeberg.org", APIURL: "https://codeberg.org/api/v1",
				Host: "codeberg.org", RepoPath: "acme/hub", Event: "schedule",
			},
		},
		{
			name: "github schedule payload without a repository",
			env: map[string]string{
				"GITHUB_ACTIONS":       "true",
				"GITHUB_SERVER_URL":    "https://github.com",
				"GITHUB_REPOSITORY_ID": "712345678",
				"GITHUB_REF_TYPE":      "branch",
				"GITHUB_REF_NAME":      "main",
				"GITHUB_EVENT_NAME":    "schedule",
				"GITHUB_EVENT_PATH":    "event.json",
			},
			files: map[string]string{"event.json": `{"schedule": "0 3 * * *"}`},
			// GITHUB_REF_TYPE=branch without GITHUB_REF: GITHUB_REF_NAME is the
			// branch.
			want: Context{
				CI: GitHubActions, ServerURL: "https://github.com", APIURL: "https://api.github.com",
				Host: "github.com", RepoID: "712345678", RefName: "main", RefIsBranch: true, Event: "schedule",
			},
		},
		{
			// GITHUB_REPOSITORY_ID wins over the payload's id on Gitea.
			name: "gitea with both repository ids",
			env: map[string]string{
				"GITEA_ACTIONS":        "true",
				"GITHUB_SERVER_URL":    "https://gitea.example.com",
				"GITHUB_REPOSITORY_ID": "7",
				"GITHUB_EVENT_PATH":    "event.json",
			},
			files: map[string]string{"event.json": giteaPush},
			want: Context{
				CI: GiteaActions, ServerURL: "https://gitea.example.com", APIURL: "https://gitea.example.com/api/v1",
				Host: "gitea.example.com", RepoID: "7", DefaultBranch: "main", Visibility: "public",
			},
		},
		{
			name: "gitlab with a private CA",
			env: map[string]string{
				"GITLAB_CI":             "true",
				"CI_SERVER_URL":         "https://gitlab.example.com",
				"CI_PROJECT_ID":         "1234",
				"CI_PROJECT_PATH":       "g/hub",
				"CI_SERVER_TLS_CA_FILE": " /builds/g/hub.tmp/CI_SERVER_TLS_CA_FILE ",
			},
			want: Context{
				CI: GitLabCI, ServerURL: "https://gitlab.example.com", APIURL: "https://gitlab.example.com/api/v4",
				Host: "gitlab.example.com", RepoID: "1234", RepoPath: "g/hub",
				RepositoryURL: "https://gitlab.example.com/g/hub.git", TLSCAFile: "/builds/g/hub.tmp/CI_SERVER_TLS_CA_FILE",
			},
		},
		{
			name: "gitlab merge request pipeline on a subpath",
			env: map[string]string{
				"GITLAB_CI":               "true",
				"CI_SERVER_URL":           "https://example.com:8443/gitlab",
				"CI_PROJECT_ID":           "7",
				"CI_PROJECT_PATH":         "g/hub",
				"CI_DEFAULT_BRANCH":       "main",
				"CI_COMMIT_REF_NAME":      "feature",
				"CI_PIPELINE_SOURCE":      "merge_request_event",
				"CI_COMMIT_REF_PROTECTED": "false",
				"CI_PROJECT_VISIBILITY":   "secret",
				"CI_REPOSITORY_URL":       "not a url",
			},
			want: Context{
				CI: GitLabCI, ServerURL: "https://example.com:8443/gitlab", APIURL: "https://example.com:8443/gitlab/api/v4",
				Host: "example.com:8443", RepoID: "7", RepoPath: "g/hub", DefaultBranch: "main",
				RefName: "feature", Event: "merge_request_event",
				RepositoryURL: "https://example.com:8443/gitlab/g/hub.git",
			},
		},
	}
	for _, tt := range tests {
		got := Detect(envOf(tt.env), filesOf(tt.files))
		if got != tt.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tt.name, got, tt.want)
		}
	}
}

func TestDetectPayloadErrors(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITEA_ACTIONS":     "true",
		"GITHUB_SERVER_URL": "https://gitea.example.com",
		"GITHUB_EVENT_PATH": "event.json",
	}
	for payload, want := range map[string][3]string{ // visibility, default branch, id
		`not json`:             {},
		`{"repository": "x"}`:  {},
		`{"repository": null}`: {},
		`{"repository": {"id": -5, "private": null}}`:                                      {},
		`{"repository": {"id": 1.5, "default_branch": 7, "private": true}}`:                {"private", "", ""},
		`{"repository": {"id": 99999999999999999999999, "visibility": "PUBLIC"}}`:          {"public", "", ""},
		`{"repository": {"id": 12, "private": false, "owner": {"visibility": "private"}}}`: {"private", "", "12"},
		`{"repository": {"id": 12, "private": false, "internal": true}}`:                   {"internal", "", "12"},
		`{"repository": {"id": 12, "visibility": "weird", "private": false}}`:              {"public", "", "12"},
	} {
		c := Detect(envOf(env), filesOf(map[string]string{"event.json": payload}))
		if got := [3]string{c.Visibility, c.DefaultBranch, c.RepoID}; got != want {
			t.Errorf("payload %s: visibility, branch, id = %q, want %q", payload, got, want)
		}
	}
	// A reader that fails and a nil reader leave the fields empty.
	failing := func(string) ([]byte, error) { return nil, errors.New("boom") }
	for _, read := range []func(string) ([]byte, error){failing, nil} {
		if c := Detect(envOf(env), read); c.Visibility != "" || c.RepoID != "" || c.CI != GiteaActions {
			t.Errorf("unreadable payload gave %+v", c)
		}
	}
	if c := Detect(nil, nil); c != (Context{CI: Local}) {
		t.Errorf("nil getenv gave %+v", c)
	}
}

func TestDetectNeverReturnsSecrets(t *testing.T) {
	t.Parallel()
	const secret = "glcbt-64_TopSecretJobToken"
	for _, repoURL := range []string{
		"https://gitlab-ci-token:" + secret + "@gitlab.example.com/g/hub.git",
		"https://gitlab-ci-token:" + secret + "@gitlab.example.com/g/hub.git?x=" + secret + "#" + secret,
		"https://gitlab-ci-token:" + secret + "%zz@gitlab.example.com/g/hub.git",
	} {
		c := Detect(envOf(map[string]string{
			"GITLAB_CI":         "true",
			"CI_SERVER_URL":     "https://user:" + secret + "@gitlab.example.com",
			"CI_REPOSITORY_URL": repoURL,
			"CI_JOB_TOKEN":      secret,
			"CI_PROJECT_PATH":   "g/hub",
		}), nil)
		if dump := fmt.Sprintf("%#v", c); strings.Contains(dump, secret) {
			t.Errorf("Detect returned the token: %s", dump)
		}
		if c.RepositoryURL != "https://gitlab.example.com/g/hub.git" {
			t.Errorf("RepositoryURL = %q", c.RepositoryURL)
		}
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		c    Context
		want string
	}{
		{Context{Host: "github.com", RepoID: "712345678"}, "github.com/712345678"},
		{Context{Host: "gitlab.example.com:8443", RepoID: "1234"}, "gitlab.example.com:8443/1234"},
		{Context{Host: "github.com"}, ""},
		{Context{RepoID: "1"}, ""},
		{Context{}, ""},
	}
	for _, tt := range tests {
		if got := tt.c.Fingerprint(); got != tt.want {
			t.Errorf("%+v.Fingerprint() = %q, want %q", tt.c, got, tt.want)
		}
	}
	c := Detect(envOf(map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY_ID": "712345678",
	}), nil)
	if got := c.Fingerprint(); got != "github.com/712345678" {
		t.Errorf("detected fingerprint %q", got)
	}
}

func TestToken(t *testing.T) {
	t.Parallel()
	env := envOf(map[string]string{
		"GITHUB_TOKEN": " ghs_actions ",
		"CI_JOB_TOKEN": "glcbt-job",
		"GITEA_TOKEN":  "gitea-token",
	})
	tests := []struct {
		ci   CI
		env  func(string) string
		want string
	}{
		{GitHubActions, env, "ghs_actions"},
		{GitLabCI, env, "glcbt-job"},
		{GiteaActions, env, "gitea-token"},
		{ForgejoActions, env, "gitea-token"},
		{ForgejoActions, envOf(map[string]string{"GITHUB_TOKEN": "fallback"}), "fallback"},
		{Local, env, ""},
		{GitHubActions, nil, ""},
	}
	for _, tt := range tests {
		if got := Token(Context{CI: tt.ci}, tt.env); got != tt.want {
			t.Errorf("Token(%s) = %q, want %q", tt.ci, got, tt.want)
		}
	}
}

func TestNewErrors(t *testing.T) {
	t.Parallel()
	gh := Context{CI: GitHubActions, APIURL: "https://api.github.com", RepoPath: "acme/hub", DefaultBranch: "main"}
	gl := Context{CI: GitLabCI, RepositoryURL: "https://gitlab.example.com/g/hub.git", DefaultBranch: "main"}
	with := func(c Context, f func(*Context)) Context { f(&c); return c }
	tests := []struct {
		name  string
		c     Context
		token string
	}{
		{"local", Context{CI: Local}, ""},
		{"zero", Context{}, ""},
		{"unknown CI", Context{CI: "jenkins"}, ""},
		{"no default branch", with(gh, func(c *Context) { c.DefaultBranch = "" }), "t"},
		{"bad default branch", with(gh, func(c *Context) { c.DefaultBranch = "-main" }), "t"},
		{"branch with ..", with(gh, func(c *Context) { c.DefaultBranch = "a..b" }), "t"},
		{"branch with a space", with(gh, func(c *Context) { c.DefaultBranch = "a b" }), "t"},
		{"no repository path", with(gh, func(c *Context) { c.RepoPath = "" }), "t"},
		{"nested repository path", with(gh, func(c *Context) { c.RepoPath = "a/b/c" }), "t"},
		{"dot-dot repository path", with(gh, func(c *Context) { c.RepoPath = "../b" }), "t"},
		{"no API URL", with(gh, func(c *Context) { c.APIURL = "" }), "t"},
		{"relative API URL", with(gh, func(c *Context) { c.APIURL = "/api/v3" }), "t"},
		{"API URL with credentials", with(gh, func(c *Context) { c.APIURL = "https://u:p@api.github.com" }), "t"},
		{"token with a newline", gh, "tok\nX-Evil: 1"},
		{"gitlab without a URL", with(gl, func(c *Context) { c.RepositoryURL = "" }), "t"},
		{"gitlab URL with credentials", with(gl, func(c *Context) { c.RepositoryURL = "https://u:p@gitlab.example.com/g/hub.git" }), ""},
		{"gitlab over ssh", with(gl, func(c *Context) { c.RepositoryURL = "ssh://git@gitlab.example.com/g/hub.git" }), ""},
		{"gitlab token over plain http", with(gl, func(c *Context) { c.RepositoryURL = "http://gitlab.example.com/g/hub.git" }), "t"},
		{"gitlab token to a file URL", with(gl, func(c *Context) { c.RepositoryURL = "file:///tmp/hub.git" }), "t"},
		{"gitlab token with a space", gl, "tok en"},
		{"gitlab bad branch", with(gl, func(c *Context) { c.DefaultBranch = "main.lock" }), "t"},
	}
	for _, tt := range tests {
		ch, err := New(tt.c, nil, tt.token)
		if err == nil || ch != nil {
			t.Errorf("%s: New = %v, %v; want an error", tt.name, ch, err)
			continue
		}
		if tt.token != "" && len(tt.token) > 1 && strings.Contains(err.Error(), tt.token) {
			t.Errorf("%s: error quotes the token: %v", tt.name, err)
		}
	}
}

// apiServer is an httptest API that serves one branch endpoint and records
// the Authorization headers it saw.
type apiServer struct {
	*httptest.Server
	auth atomic.Value // string
	hits atomic.Int64
}

func newAPIServer(t *testing.T, path, body string) *apiServer {
	t.Helper()
	s := &apiServer{}
	s.auth.Store("")
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.auth.Store(r.Header.Get("Authorization"))
		if r.URL.EscapedPath() != path || r.Method != http.MethodGet {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestGitHubChannel(t *testing.T) {
	t.Parallel()
	const sha = "3f2c1ab90000000000000000000000000000beef"
	srv := newAPIServer(t, "/api/v3/repos/acme/hub/git/ref/heads/release/v%231",
		`{"ref":"refs/heads/release/v#1","node_id":"x","object":{"sha":"`+strings.ToUpper(sha)+`","type":"commit","url":"u"}}`)
	c := Context{CI: GitHubActions, APIURL: srv.URL + "/api/v3/", RepoPath: "acme/hub", DefaultBranch: "release/v#1"}
	ch, err := New(c, httpx.New(httpx.Options{}), "ghs_hubtoken")
	if err != nil {
		t.Fatal(err)
	}
	head, err := ch.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Errorf("Head() = %q, want %q", head, sha)
	}
	if got := srv.auth.Load(); got != "Bearer ghs_hubtoken" {
		t.Errorf("Authorization = %q", got)
	}

	// Anonymous without a token; a nil client is a default one.
	anon, err := New(c, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anon.Head(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := srv.auth.Load(); got != "" {
		t.Errorf("anonymous Authorization = %q", got)
	}
}

func TestGiteaChannel(t *testing.T) {
	t.Parallel()
	const sha = "aaaabbbbccccddddeeeeffff0000111122223333"
	srv := newAPIServer(t, "/api/v1/repos/acme/hub/branches/main",
		`{"name":"main","commit":{"id":"`+sha+`","message":"x"},"protected":true}`)
	for _, ci := range []CI{GiteaActions, ForgejoActions} {
		c := Context{CI: ci, APIURL: srv.URL + "/api/v1", RepoPath: "acme/hub", DefaultBranch: "main"}
		ch, err := New(c, nil, "gitea-actions-token")
		if err != nil {
			t.Fatal(err)
		}
		head, err := ch.Head(t.Context())
		if err != nil || head != sha {
			t.Errorf("%s: Head() = %q, %v", ci, head, err)
		}
		if got := srv.auth.Load(); got != "token gitea-actions-token" {
			t.Errorf("%s: Authorization = %q", ci, got)
		}
	}
}

func TestRESTChannelBadAnswers(t *testing.T) {
	t.Parallel()
	const sha = "3f2c1ab90000000000000000000000000000beef"
	tests := []struct {
		name string
		ci   CI
		path string
		body string
	}{
		{"github: another ref", GitHubActions, "/repos/a/h/git/ref/heads/main", `{"ref":"refs/heads/other","object":{"sha":"` + sha + `","type":"commit"}}`},
		{"github: a tag object", GitHubActions, "/repos/a/h/git/ref/heads/main", `{"ref":"refs/heads/main","object":{"sha":"` + sha + `","type":"tag"}}`},
		{"github: short sha", GitHubActions, "/repos/a/h/git/ref/heads/main", `{"ref":"refs/heads/main","object":{"sha":"3f2c","type":"commit"}}`},
		{"github: not found", GitHubActions, "/nowhere", ``},
		{"gitea: another branch", GiteaActions, "/repos/a/h/branches/main", `{"name":"other","commit":{"id":"` + sha + `"}}`},
		{"gitea: no commit", GiteaActions, "/repos/a/h/branches/main", `{"name":"main"}`},
		{"gitea: not JSON", GiteaActions, "/repos/a/h/branches/main", `<html>`},
	}
	for _, tt := range tests {
		srv := newAPIServer(t, tt.path, tt.body)
		ch, err := New(Context{CI: tt.ci, APIURL: srv.URL, RepoPath: "a/h", DefaultBranch: "main"}, nil, "tok-12345678")
		if err != nil {
			t.Fatal(err)
		}
		if head, err := ch.Head(t.Context()); err == nil {
			t.Errorf("%s: Head() = %q, want an error", tt.name, head)
		} else if strings.Contains(err.Error(), "tok-12345678") {
			t.Errorf("%s: error leaks the token: %v", tt.name, err)
		}
	}
	// A 404 keeps the status error in the chain.
	srv := newAPIServer(t, "/nowhere", ``)
	ch, _ := New(Context{CI: GitHubActions, APIURL: srv.URL, RepoPath: "a/h", DefaultBranch: "main"}, nil, "")
	_, err := ch.Head(t.Context())
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("404: error %v, want a StatusError", err)
	}
}

func TestRESTChannelTokenGoesOnlyToTheAPIHost(t *testing.T) {
	t.Parallel()
	var stolen atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stolen.Add(1)
	}))
	t.Cleanup(other.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	t.Cleanup(api.Close)
	ch, err := New(Context{CI: GitHubActions, APIURL: api.URL, RepoPath: "a/h", DefaultBranch: "main"}, nil, "ghs_hubtoken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Head(t.Context()); err == nil {
		t.Error("Head() followed a redirect to another host")
	}
	if n := stolen.Load(); n != 0 {
		t.Errorf("the other host received %d requests", n)
	}
	if hosts := ch.(*restChannel).auth.Hosts; len(hosts) != 1 || hosts[0] != strings.TrimPrefix(api.URL, "http://") {
		t.Errorf("the token is bound to %q", hosts)
	}
}

// repo is a throwaway git repository for the ls-remote tests.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T, dir string, bare bool) *repo {
	t.Helper()
	r := &repo{t: t, dir: dir}
	args := []string{"init", "-q", "-b", "main"}
	if bare {
		args = append(args, "--bare")
	}
	r.git(append(args, dir)...)
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	g := &gitx.Git{Dir: r.dir}
	if args[0] == "init" {
		g.Dir = ""
	}
	out, err := g.Run(r.t.Context(), nil, args...)
	if err != nil {
		r.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// commit makes an empty commit on HEAD and returns its id.
func (r *repo) commit(msg string) string {
	r.t.Helper()
	tree := r.git("mktree")
	return r.git("commit-tree", tree, "-m", msg)
}

// fileURL returns a file:// URL for dir.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x on Windows
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func TestGitLabChannelLsRemote(t *testing.T) {
	t.Parallel()
	r := newRepo(t, filepath.Join(t.TempDir(), "hub.git"), true)
	main := r.commit("main")
	decoy := r.commit("decoy")
	r.git("update-ref", "refs/heads/main", main)
	// Refs that ls-remote's pattern refs/heads/main also matches.
	r.git("update-ref", "refs/heads/refs/heads/main", decoy)
	r.git("update-ref", "refs/remotes/origin/refs/heads/main", decoy)
	r.git("update-ref", "refs/heads/mainline", decoy)

	c := Context{CI: GitLabCI, RepositoryURL: fileURL(r.dir), DefaultBranch: "main"}
	ch, err := New(c, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	head, err := ch.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head != main {
		t.Errorf("Head() = %s, want %s", head, main)
	}

	c.DefaultBranch = "missing"
	ch, err = New(c, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Head(t.Context()); err == nil || !strings.Contains(err.Error(), `branch "missing" not found`) {
		t.Errorf("missing branch: %v", err)
	}

	c.RepositoryURL = fileURL(filepath.Join(t.TempDir(), "nothing.git"))
	c.DefaultBranch = "main"
	ch, _ = New(c, nil, "")
	if _, err := ch.Head(t.Context()); err == nil {
		t.Error("a missing repository gave no error")
	}
}

func TestGitLabCommandKeepsTheTokenOutOfArgv(t *testing.T) {
	t.Parallel()
	const token = "glcbt-64_ArgvCanaryToken"
	basic := base64.StdEncoding.EncodeToString([]byte("gitlab-ci-token:" + token))
	c := Context{CI: GitLabCI, RepositoryURL: "https://GitLab.example.com:8443/g/hub.git", DefaultBranch: "main"}
	ch, err := New(c, nil, token)
	if err != nil {
		t.Fatal(err)
	}
	args, env := ch.(*gitChannel).command("/tmp", "/tmp/home", "/tmp/home/gitconfig")
	for _, a := range args {
		if strings.Contains(a, token) || strings.Contains(a, basic) {
			t.Errorf("argv holds the token: %q", a)
		}
	}
	// Every safety flag of the hub channel's git, in order, and nothing else.
	wantArgs := []string{
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "http.followRedirects=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"ls-remote", "--refs", "https://GitLab.example.com:8443/g/hub.git", "refs/heads/main",
	}
	if !slices.Equal(args, wantArgs) {
		t.Errorf("args %q\nwant %q", args, wantArgs)
	}
	wantEnv := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_CEILING_DIRECTORIES=/tmp",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/tmp/home/gitconfig",
		"HOME=/tmp/home",
		"XDG_CONFIG_HOME=/tmp/home",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://GitLab.example.com:8443/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
	if !slices.Equal(env, wantEnv) {
		t.Errorf("env %q\nwant %q", env, wantEnv)
	}

	// Without a token nothing is configured.
	anon, _ := New(c, nil, "")
	_, env = anon.(*gitChannel).command("/tmp", "/tmp/home", "/tmp/home/gitconfig")
	if want := wantEnv[:7]; !slices.Equal(env, want) {
		t.Errorf("anonymous env %q\nwant %q", env, want)
	}

	// A self-managed instance's CA (CI_SERVER_TLS_CA_FILE) is trusted over
	// https only.
	c.TLSCAFile = "/builds/tmp/CI_SERVER_TLS_CA_FILE"
	withCA, _ := New(c, nil, token)
	args, _ = withCA.(*gitChannel).command("/tmp", "/tmp/home", "/tmp/home/gitconfig")
	if i := slices.Index(args, "http.sslCAInfo=/builds/tmp/CI_SERVER_TLS_CA_FILE"); i < 1 || args[i-1] != "-c" || i > slices.Index(args, "ls-remote") {
		t.Errorf("args %q do not trust the CA file", args)
	}
	c.RepositoryURL = "http://localhost:8080/g/hub.git"
	plain, _ := New(c, nil, "")
	if args, _ = plain.(*gitChannel).command("/tmp", "/tmp/home", "/tmp/home/gitconfig"); slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "http.sslCAInfo") }) {
		t.Errorf("http args %q name a CA file", args)
	}
}

// TestGitLabChannelIsolation: the runner's git config and GIT_* variables
// never reach the channel's git, so they cannot redirect the job token,
// turn off TLS verification or print the header. The
// fixture proves they would: plain git follows them.
func TestGitLabChannelIsolation(t *testing.T) {
	root := t.TempDir()
	hub := newRepo(t, filepath.Join(root, "hub.git"), true)
	want := hub.commit("the hub")
	hub.git("update-ref", "refs/heads/main", want)
	decoy := newRepo(t, filepath.Join(root, "decoy.git"), true)
	other := decoy.commit("a decoy")
	decoy.git("update-ref", "refs/heads/main", other)
	hubURL, decoyURL := fileURL(hub.dir), fileURL(decoy.dir)

	global := filepath.Join(root, "evil.gitconfig")
	evil := "[url \"" + decoyURL + "\"]\n\tinsteadOf = " + hubURL + "\n[http]\n\tsslVerify = false\n"
	if err := os.WriteFile(global, []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'url."+decoyURL+".insteadof'='"+hubURL+"'")
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	t.Setenv("GIT_TRACE", "1")

	// The fixture works: git that inherits the environment reads the decoy.
	out, err := (&gitx.Git{}).Run(t.Context(), nil, "ls-remote", hubURL, "refs/heads/main")
	if err != nil || !strings.HasPrefix(string(out), other) {
		t.Fatalf("plain git read %q, %v; want the decoy %s", out, err, other)
	}

	ch, err := New(Context{CI: GitLabCI, RepositoryURL: hubURL, DefaultBranch: "main"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	head, err := ch.Head(t.Context())
	if err != nil || head != want {
		t.Errorf("Head() = %q, %v; want the hub's %s", head, err, want)
	}
	// Tracing is not inherited either: a failing call's error (git's
	// stderr) holds no trace lines.
	missing, _ := New(Context{CI: GitLabCI, RepositoryURL: fileURL(filepath.Join(root, "missing.git")), DefaultBranch: "main"}, nil, "")
	if _, err := missing.Head(t.Context()); err == nil || strings.Contains(err.Error(), "trace:") {
		t.Errorf("Head() of a missing repository = %v; want an error without trace lines", err)
	}

	env := channelEnviron()
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") && !slices.Contains(keptGitVars, strings.ToUpper(name)) {
			t.Errorf("channelEnviron keeps %s", name)
		}
	}
	t.Setenv("GIT_SSL_CAINFO", filepath.Join(root, "ca.pem"))
	if !slices.Contains(channelEnviron(), "GIT_SSL_CAINFO="+filepath.Join(root, "ca.pem")) {
		t.Error("channelEnviron drops GIT_SSL_CAINFO")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestGitLabProcessArgvCanary runs the channel against a fake git and
// checks the process it starts: no token in its arguments, the header in
// its environment.
func TestGitLabProcessArgvCanary(t *testing.T) {
	const token = "glcbt-64_ProcessCanaryToken"
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv(fakeGitEnv, record)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ch, err := New(Context{CI: GitLabCI, RepositoryURL: "https://gitlab.example.com/g/hub.git", DefaultBranch: "main"}, nil, token)
	if err != nil {
		t.Fatal(err)
	}
	ch.(*gitChannel).bin = self
	head, err := ch.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head != fakeHead {
		t.Errorf("Head() = %q", head)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Args, Env []string }
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("gitlab-ci-token:" + token))
	if joined := strings.Join(got.Args, " "); strings.Contains(joined, token) || strings.Contains(joined, basic) {
		t.Errorf("git argv holds the token: %s", joined)
	}
	if !contains(got.Env, "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic) {
		t.Error("git did not get the header through its environment")
	}
	if !contains(got.Args, "ls-remote") || !contains(got.Args, "credential.helper=") {
		t.Errorf("git argv = %q", got.Args)
	}
}

// TestGitLabChannelOverHTTP runs the channel against git http-backend that
// accepts only the job token's Basic header: the header reaches the server
// through GIT_CONFIG_*, and a wrong token fails without a prompt.
func TestGitLabChannelOverHTTP(t *testing.T) {
	t.Parallel()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	r := newRepo(t, filepath.Join(root, "g", "hub.git"), true)
	main := r.commit("main")
	r.git("update-ref", "refs/heads/main", main)

	const token = "glcbt-64_HTTPBackendToken"
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("gitlab-ci-token:"+token))
	var authorized, refused atomic.Int64
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + os.Getenv("GIT_CONFIG_GLOBAL"),
			"HOME=" + os.Getenv("HOME"),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != want {
			refused.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="hub"`)
			http.Error(w, "HTTP Basic: Access denied", http.StatusUnauthorized)
			return
		}
		authorized.Add(1)
		backend.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	c := Context{CI: GitLabCI, RepositoryURL: srv.URL + "/g/hub.git", DefaultBranch: "main"}
	ch, err := New(c, nil, token)
	if err != nil {
		t.Fatal(err)
	}
	head, err := ch.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head != main || authorized.Load() == 0 {
		t.Errorf("Head() = %s after %d authorized requests, want %s", head, authorized.Load(), main)
	}

	wrong, err := New(c, nil, "glcbt-64_WrongToken")
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrong.Head(t.Context())
	if err == nil || refused.Load() == 0 {
		t.Fatalf("a wrong token: %v after %d refusals", err, refused.Load())
	}
	if strings.Contains(err.Error(), "WrongToken") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestParseLsRemote(t *testing.T) {
	t.Parallel()
	const a, b = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	out := b + "\trefs/heads/refs/heads/main\r\n" + a + "\trefs/heads/main\n"
	if got, err := parseLsRemote([]byte(out), "main"); err != nil || got != a {
		t.Errorf("parseLsRemote = %q, %v", got, err)
	}
	if _, err := parseLsRemote([]byte("zz\trefs/heads/main\n"), "main"); err == nil {
		t.Error("a bad object id was accepted")
	}
	if _, err := parseLsRemote(nil, "main"); err == nil {
		t.Error("empty output was accepted")
	}
}

// TestChannelScheduleFallback: GitHub's and Gitea's schedule event names no
// default branch (its payload has no repository); the channel then reads
// the tip of the branch the run builds, which a schedule always builds from
// the default branch. A run of no branch keeps the error.
func TestChannelScheduleFallback(t *testing.T) {
	t.Parallel()
	const sha = "3f2c1ab90000000000000000000000000000beef"
	srv := newAPIServer(t, "/repos/acme/hub/git/ref/heads/main",
		`{"ref":"refs/heads/main","object":{"sha":"`+sha+`","type":"commit"}}`)
	c := Context{CI: GitHubActions, APIURL: srv.URL, RepoPath: "acme/hub", RefName: "main", RefIsBranch: true, Event: "schedule"}
	ch, err := New(c, nil, "ghs_hubtoken")
	if err != nil {
		t.Fatal(err)
	}
	if head, err := ch.Head(t.Context()); err != nil || head != sha {
		t.Errorf("Head() = %q, %v", head, err)
	}
	c.RefIsBranch = false
	if _, err := New(c, nil, "ghs_hubtoken"); err == nil {
		t.Error("a run of no branch and no default branch got a channel")
	}
}

// TestChannelVisibility: the REST channel reads the hub's visibility for a
// run whose event payload names none.
func TestChannelVisibility(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]string{
		`{"full_name":"acme/hub","private":true,"visibility":"private"}`:            "private",
		`{"full_name":"acme/hub","private":false,"visibility":"public"}`:            "public",
		`{"full_name":"acme/hub","private":false,"visibility":"internal"}`:          "internal",
		`{"full_name":"acme/hub","private":false,"owner":{"visibility":"limited"}}`: "internal",
		`{"full_name":"acme/hub"}`: "",
	} {
		srv := newAPIServer(t, "/repos/acme/hub", body)
		ch, err := New(Context{CI: GitHubActions, APIURL: srv.URL, RepoPath: "acme/hub", DefaultBranch: "main"}, nil, "ghs_hubtoken")
		if err != nil {
			t.Fatal(err)
		}
		got, err := ch.(VisibilityReader).Visibility(t.Context())
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%s: Visibility() = %q, %v; want %q", body, got, err, want)
		}
		if a := srv.auth.Load(); a != "Bearer ghs_hubtoken" {
			t.Errorf("Authorization = %q", a)
		}
	}
}

// TestChannelEnvironment: the GitHub channel reads the environment
// touchmark-distribute and, when it has custom deployment policies, the
// policies, page by page (as observed on public repositories: a missing
// deployment_branch_policy is null, and the policies of an environment
// without custom ones answer 404).
func TestChannelEnvironment(t *testing.T) {
	t.Parallel()
	const base = "/repos/acme/hub/environments/touchmark-distribute"
	page := func(from, n, total int) string {
		var items []string
		for i := from; i < from+n; i++ {
			items = append(items, fmt.Sprintf(`{"id": %d, "node_id": "x", "name": "b%d", "type": "branch"}`, i, i))
		}
		return fmt.Sprintf(`{"total_count": %d, "branch_policies": [%s]}`, total, strings.Join(items, ","))
	}
	for _, tc := range []struct {
		name  string
		env   string // "" for 404
		pages []string
		want  Environment
		err   error
	}{
		{name: "any ref", env: `{"name": "touchmark-distribute", "deployment_branch_policy": null}`, want: Environment{AllRefs: true}},
		{name: "protected", env: `{"name": "touchmark-distribute", "deployment_branch_policy": {"protected_branches": true, "custom_branch_policies": false}}`,
			want: Environment{ProtectedBranches: true}},
		{name: "custom", env: `{"name": "touchmark-distribute", "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}`,
			pages: []string{`{"total_count": 2, "branch_policies": [{"name": "main", "type": "branch"}, {"name": "v*", "type": "tag"}]}`},
			want:  Environment{Policies: []EnvironmentPolicy{{"main", "branch"}, {"v*", "tag"}}}},
		{name: "paged", env: `{"name": "touchmark-distribute", "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}`,
			pages: []string{page(0, 100, 101), page(100, 1, 101)}},
		{name: "none", err: ErrNoEnvironment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var auth atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth.Store(r.Header.Get("Authorization"))
				switch {
				case r.URL.Path == base && tc.env != "":
					fmt.Fprint(w, tc.env)
				case r.URL.Path == base+"/deployment-branch-policies" && len(tc.pages) > 0:
					n, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if n < 1 || n > len(tc.pages) || r.URL.Query().Get("per_page") != "100" {
						http.Error(w, "bad page", http.StatusBadRequest)
						return
					}
					fmt.Fprint(w, tc.pages[n-1])
				default:
					http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			ch, err := New(Context{CI: GitHubActions, APIURL: srv.URL, RepoPath: "acme/hub", DefaultBranch: "main"}, nil, "ghs_hubtoken")
			if err != nil {
				t.Fatal(err)
			}
			got, err := ch.(EnvironmentReader).Environment(t.Context(), "touchmark-distribute")
			switch {
			case tc.err != nil:
				if !errors.Is(err, tc.err) {
					t.Errorf("err %v, want %v", err, tc.err)
				}
			case err != nil:
				t.Fatal(err)
			case tc.name == "paged":
				if len(got.Policies) != 101 || got.Policies[100].Name != "b100" {
					t.Errorf("%d policies", len(got.Policies))
				}
			case got.AllRefs != tc.want.AllRefs || got.ProtectedBranches != tc.want.ProtectedBranches || !slices.Equal(got.Policies, tc.want.Policies):
				t.Errorf("Environment() = %+v, want %+v", got, tc.want)
			}
			if a, _ := auth.Load().(string); a != "Bearer ghs_hubtoken" {
				t.Errorf("Authorization = %q", a)
			}
		})
	}
	gitea, err := New(Context{CI: GiteaActions, APIURL: "https://gitea.example.com/api/v1", RepoPath: "acme/hub", DefaultBranch: "main"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitea.(EnvironmentReader).Environment(t.Context(), "touchmark-distribute"); err == nil {
		t.Error("the Gitea channel read an environment")
	}
}
