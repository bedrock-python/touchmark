package ghfake

import (
	"bytes"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests drive the fake with net/http and git only, never with a
// driver: they pin the behavior the package documentation promises.

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
	keyErr  error
)

// appKey returns an RSA key shared by the tests (generated once per run).
func appKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() { testKey, _, keyErr = GenerateAppKey() })
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	return testKey
}

// clock is a controllable clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// world is a fake with the usual cast: the organization acme on the team
// plan, the people alice and bob, the App hub-writer installed on acme
// with contents, pull requests and workflows, and a public repository
// acme/api with a README.
type world struct {
	t     *testing.T
	s     *Server
	clock *clock
	app   App
	inst  Installation
	api   Repo
}

// must returns v, failing the test on err: must(t)(f()).
func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newWorld starts a fake with the usual cast.
func newWorld(t *testing.T, opts Options) *world {
	t.Helper()
	c := &clock{now: time.Now().UTC().Truncate(time.Second)}
	if opts.Now == nil {
		opts.Now = c.Now
	}
	if opts.Dir == "" {
		opts.Dir = filepath.Join(t.TempDir(), "gh")
	}
	s, err := New(opts)
	check(t, err)
	t.Cleanup(func() { _ = s.Close() })
	w := &world{t: t, s: s, clock: c}
	must[Account](t)(s.AddOrg("acme", PlanTeam))
	must[Account](t)(s.AddUser("alice"))
	must[Account](t)(s.AddUser("bob"))
	w.app = must[App](t)(s.RegisterApp(AppSpec{Slug: "hub-writer", Owner: "acme", PublicKey: &appKey(t).PublicKey,
		Permissions: Permissions{"contents": Write, "pull_requests": Write, "workflows": Write, "metadata": Read}}))
	w.inst = must[Installation](t)(s.Install(InstallSpec{App: "hub-writer", Account: "acme"}))
	w.api = w.repo(RepoSpec{Owner: "acme", Name: "api", Files: []File{{Path: "README.md", Content: []byte("# api\n")}}})
	return w
}

// repo creates a repository.
func (w *world) repo(spec RepoSpec) Repo {
	w.t.Helper()
	return must[Repo](w.t)(w.s.CreateRepo(spec))
}

// commit makes a person's commit.
func (w *world) commit(path string, spec CommitSpec) string {
	w.t.Helper()
	return must[string](w.t)(w.s.Commit(path, spec))
}

// openPR opens a person's pull request.
func (w *world) openPR(path string, spec PRSpec) PR {
	w.t.Helper()
	return must[PR](w.t)(w.s.OpenPR(path, spec))
}

// branch creates branch with one commit on top of the default branch.
func (w *world) branch(path, name string) string {
	w.t.Helper()
	return w.commit(path, CommitSpec{Branch: name, Author: "alice",
		Files: []File{{Path: "branch-" + strings.ReplaceAll(name, "/", "-") + ".txt", Content: []byte(name + "\n")}}})
}

// jwt signs a valid JWT of the world's App.
func (w *world) jwt() string {
	now := w.clock.Now()
	return must[string](w.t)(SignJWT(appKey(w.t), w.app.ClientID, now.Add(-60*time.Second), now.Add(9*time.Minute)))
}

// token mints an installation token through the fake's Go API.
func (w *world) token(repos []string, perms Permissions) string {
	return must[string](w.t)(w.s.InstallationToken(w.inst.ID, repos, perms))
}

// reply is a decoded response.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// json decodes the body into a generic value.
func (r reply) json(t *testing.T) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return v
}

// obj decodes an object body.
func (r reply) obj(t *testing.T) map[string]any {
	t.Helper()
	m, ok := r.json(t).(map[string]any)
	if !ok {
		t.Fatalf("not an object: %s", r.body)
	}
	return m
}

// list decodes a list body.
func (r reply) list(t *testing.T) []any {
	t.Helper()
	l, ok := r.json(t).([]any)
	if !ok {
		t.Fatalf("not a list: %s", r.body)
	}
	return l
}

// call sends a REST request to path under the server URL with a bearer
// credential ("" for none) and a JSON body (nil for none).
func (w *world) call(method, path, cred string, body any, header ...string) reply {
	w.t.Helper()
	return doRequest(w.t, w.s, method, w.s.URL()+path, cred, body, header...)
}

// doRequest sends one request.
func doRequest(t *testing.T, s *Server, method, url, cred string, body any, header ...string) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}
}

// graphql sends a GraphQL request and decodes the response.
func (w *world) graphql(cred, query string, vars map[string]any) map[string]any {
	w.t.Helper()
	body := map[string]any{"query": query}
	if vars != nil {
		body["variables"] = vars
	}
	r := w.call("POST", "/graphql", cred, body)
	if r.status != http.StatusOK {
		w.t.Fatalf("graphql: HTTP %d %s", r.status, r.body)
	}
	return r.obj(w.t)
}

// wantStatus checks a status.
func wantStatus(t *testing.T, what string, r reply, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: HTTP %d, want %d: %s", what, r.status, status, r.body)
	}
}

// field walks a decoded JSON value by keys and indexes.
func field(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// noViolations fails on any violation.
func (w *world) noViolations() {
	w.t.Helper()
	if v := w.s.Violations(); len(v) > 0 {
		w.t.Fatalf("violations: %q", v)
	}
}

// Git helpers.

// gitEnvFor returns an isolated git environment with an Authorization
// header for the server (none without a token), passed through
// GIT_CONFIG_COUNT so it is in no argv.
func gitEnvFor(t *testing.T, s *Server, token string) []string {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + cfg, "HOME=" + home, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=alice", "GIT_AUTHOR_EMAIL=alice@example.invalid",
		"GIT_COMMITTER_NAME=alice", "GIT_COMMITTER_EMAIL=alice@example.invalid"}
	pairs := [][2]string{{"init.defaultBranch", "main"}, {"core.autocrlf", "false"}}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		pairs = append(pairs, [2]string{"http." + s.URL() + "/.extraHeader", "Authorization: Basic " + basic})
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(pairs)))
	for i, p := range pairs {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+p[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+p[1])
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") && !strings.HasPrefix(strings.ToUpper(kv), "HOME=") {
			env = append(env, kv)
		}
	}
	return env
}

// gitCmd returns a git command in dir with env (the process's when nil).
func gitCmd(dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd
}

// git runs git in dir and returns stdout and stderr.
func git(t *testing.T, env []string, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// mustGit runs git and fails the test on an error.
func mustGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	out, errOut, err := git(t, env, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errOut)
	}
	return strings.TrimSpace(out)
}

// clone clones a repository of the server with token ("" anonymous).
func clone(t *testing.T, s *Server, path, token string) (dir string, env []string) {
	t.Helper()
	env = gitEnvFor(t, s, token)
	dir = filepath.Join(t.TempDir(), "clone")
	mustGit(t, env, "", "clone", "-q", s.CloneURL(path), dir)
	return dir, env
}

// commitFile writes a file in a clone and commits it.
func commitFile(t *testing.T, env []string, dir, path, content, msg string) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, dir, "add", "--", path)
	mustGit(t, env, dir, "commit", "-q", "-m", msg)
	return mustGit(t, env, dir, "rev-parse", "HEAD")
}

// needGit skips a test without git.
func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
}

// itoa formats an int64.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
