package setup

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// ghWorld is a fake GitHub with the organization octo-org (plan Team) and
// its hub octo-org/hub, which alice administers with a classic token.
type ghWorld struct {
	f     *ghfake.Server
	token string
	reg   *redact.Registry
	// told collects the URLs setup asked the person to open; browsers
	// tracks the browser goroutines, which the test waits for.
	mu       sync.Mutex
	told     []string
	browsers sync.WaitGroup
}

func newGHWorld(t *testing.T, plan ghfake.Plan, visibility string) *ghWorld {
	t.Helper()
	f, err := ghfake.New(ghfake.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.AddOrg("octo-org", plan)
	must(err)
	_, err = f.AddUser("alice")
	must(err)
	_, err = f.CreateRepo(ghfake.RepoSpec{Owner: "octo-org", Name: "hub", Visibility: visibility,
		Files: []ghfake.File{{Path: "README.md", Content: []byte("# hub\n")}}})
	must(err)
	must(f.Grant("octo-org/hub", "alice", "admin"))
	token, err := f.AddPAT("alice", ghfake.PATSpec{Scopes: []string{"repo"}})
	must(err)
	w := &ghWorld{f: f, token: token, reg: redact.New()}
	t.Cleanup(w.browsers.Wait)
	return w
}

// input is the input of a run; the browser step is driven by browse.
func (w *ghWorld) input(t *testing.T, dry bool) GitHubInput {
	w.reg.Add(w.token)
	return GitHubInput{
		WebURL: w.f.URL(), APIURL: w.f.APIURL(), Host: "github.com", Repo: "octo-org/hub", Token: w.token,
		Client: httpx.New(httpx.Options{Redact: w.reg}), Isolation: "platform",
		ReadIDVar: "TOUCHMARK_READ_APP_ID", ReadKeyVar: "TOUCHMARK_READ_APP_KEY",
		WriteIDVar: "TOUCHMARK_WRITE_APP_ID", WriteKeyVar: "TOUCHMARK_WRITE_APP_KEY",
		DryRun: dry, KeyDir: filepath.Join(t.TempDir(), "keys"), Timeout: time.Minute, Engine: "test", Redact: w.reg,
		Tell: func(u string) {
			w.mu.Lock()
			w.told = append(w.told, u)
			w.mu.Unlock()
			w.browsers.Add(1)
			go func() {
				defer w.browsers.Done()
				w.browse(t, u)
			}()
		},
	}
}

var (
	formAction    = regexp.MustCompile(`<form action="([^"]+)" method="post">`)
	formManifest  = regexp.MustCompile(`name="manifest" value="([^"]+)"`)
	browserClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
)

// browse plays the person's browser: for each App it opens the page,
// posts its form to GitHub's "new App" page (the fake confirms at once)
// and follows GitHub's redirect to setup's callback, until the callback
// offers no next App.
func (w *ghWorld) browse(t *testing.T, page string) {
	for {
		body, err := get(page)
		if err != nil {
			t.Errorf("browser: %v", err)
			return
		}
		a, m := formAction.FindStringSubmatch(body), formManifest.FindStringSubmatch(body)
		if a == nil || m == nil {
			t.Errorf("browser: no form on the page:\n%s", body)
			return
		}
		req, _ := http.NewRequest(http.MethodPost, html.UnescapeString(a[1]), strings.NewReader(url.Values{"manifest": {html.UnescapeString(m[1])}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+w.token)
		resp, err := browserClient.Do(req)
		if err != nil {
			t.Errorf("browser: %v", err)
			return
		}
		_ = resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusFound || loc == "" {
			t.Errorf("browser: GitHub answered %d without a redirect", resp.StatusCode)
			return
		}
		if body, err = get(loc); err != nil || !strings.Contains(body, "Created the") {
			t.Errorf("browser: the callback answered %v:\n%s", err, body)
			return
		}
		if !strings.Contains(body, "Continue with the next App") {
			return
		}
	}
}

// get reads a page.
func get(u string) (string, error) {
	resp, err := browserClient.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return string(data), fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	return string(data), err
}

// writes counts the fake's requests that change something.
func (w *ghWorld) writes() int {
	n := 0
	for _, r := range w.f.Requests() {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			n++
		}
	}
	return n
}

// runGH runs GitHub setup and checks that no secret shows in its report.
func (w *ghWorld) runGH(t *testing.T, in GitHubInput) *Report {
	t.Helper()
	rep, err := GitHub(context.Background(), in)
	if err != nil {
		t.Fatalf("GitHub: %v", err)
	}
	var text, js bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, "PRIVATE KEY") || w.reg.Contains(out) {
			t.Errorf("the report shows a secret:\n%s", out)
		}
	}
	return rep
}

// TestGitHubSetup: a fresh hub gets its environment limited to main, the
// two Apps through the manifest flow (their private keys in files of the
// key directory, the gh commands printed), their ids in variables, and a
// ruleset; once the person stored the keys, a second run writes nothing.
func TestGitHubSetup(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanTeam, "private")
	in := w.input(t, false)
	rep := w.runGH(t, in)
	wantSteps(t, rep, map[string]Status{
		"hub": StatusOK, "environment": StatusDone, "deployment-branches": StatusDone, "reader-app": StatusDone, "writer-app": StatusDone,
		"reader-variable": StatusDone, "writer-variable": StatusDone, "reader-key": StatusManual, "writer-key": StatusManual, "ruleset": StatusDone,
	})
	if rep.ExitCode() != ExitPending {
		t.Errorf("exit %d", rep.ExitCode())
	}
	if len(w.told) != 1 || !strings.HasPrefix(w.told[0], "http://127.0.0.1:") {
		t.Errorf("told %q", w.told)
	}
	if rep.Reader != "octo-org-assets-read[bot]" || rep.Writer != "octo-org-assets-write[bot]" {
		t.Errorf("reader %s, writer %s", rep.Reader, rep.Writer)
	}
	for slug, want := range map[string]ghfake.Permissions{
		"octo-org-assets-read":  {"metadata": "read", "contents": "read", "pull_requests": "read"},
		"octo-org-assets-write": {"metadata": "read", "contents": "write", "pull_requests": "write", "workflows": "write"},
	} {
		if _, perms, ok := w.f.AppBySlug(slug); !ok || fmt.Sprint(perms) != fmt.Sprint(want) {
			t.Errorf("App %s: %v %v", slug, ok, perms)
		}
	}
	keyDir, _ := filepath.Abs(in.KeyDir)
	for _, label := range []string{"reader", "writer"} {
		p := filepath.Join(keyDir, label+".pem")
		data, err := os.ReadFile(p)
		if err != nil || !bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Errorf("%s: %v", p, err)
		}
		if st, err := os.Stat(p); err == nil && runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", p, st.Mode())
		}
	}
	rk, wk := step(t, rep, "reader-key"), step(t, rep, "writer-key")
	if len(rk.Commands) != 1 || rk.Commands[0] != "gh secret set TOUCHMARK_READ_APP_KEY --repo octo-org/hub < "+shellQuote(filepath.Join(keyDir, "reader.pem")) {
		t.Errorf("reader-key commands %q", rk.Commands)
	}
	if len(wk.Commands) != 1 || !strings.HasPrefix(wk.Commands[0], "gh secret set TOUCHMARK_WRITE_APP_KEY --repo octo-org/hub --env touchmark-distribute < ") {
		t.Errorf("writer-key commands %q", wk.Commands)
	}
	env, ok := w.f.Environment("octo-org/hub", Environment)
	if !ok || env.AllBranches || env.Protected || !slices.Equal(env.Policies, []string{"branch:main"}) {
		t.Errorf("environment %+v", env)
	}
	if env.Variables["TOUCHMARK_WRITE_APP_ID"] == "" {
		t.Errorf("environment variables %v", env.Variables)
	}
	if _, vars := w.f.RepoSecrets("octo-org/hub"); vars["TOUCHMARK_READ_APP_ID"] == "" || vars["TOUCHMARK_WRITE_APP_ID"] != "" {
		t.Errorf("repository variables %v", vars)
	}
	if got := w.f.Rulesets("octo-org/hub"); !slices.Equal(got, []string{RulesetName}) {
		t.Errorf("rulesets %q", got)
	}
	if !slices.ContainsFunc(rep.Next, func(n string) bool { return strings.Contains(n, "/apps/octo-org-assets-write/installations/new") }) ||
		!slices.ContainsFunc(rep.Next, func(n string) bool { return strings.Contains(n, "set the writer to octo-org-assets-write[bot]") }) {
		t.Errorf("next %q", rep.Next)
	}

	// The person runs the printed commands.
	if err := w.f.SetSecret("octo-org/hub", "", "TOUCHMARK_READ_APP_KEY", false); err != nil {
		t.Fatal(err)
	}
	if err := w.f.SetSecret("octo-org/hub", Environment, "TOUCHMARK_WRITE_APP_KEY", false); err != nil {
		t.Fatal(err)
	}
	before := w.writes()
	in = w.input(t, false)
	in.Writer = "octo-org-assets-write[bot]"
	rep = w.runGH(t, in)
	onlyOK(t, rep)
	if n := w.writes() - before; n != 0 || rep.ExitCode() != ExitOK || len(w.told) != 1 {
		t.Errorf("the second run wrote %d times, exit %d, told %q", n, rep.ExitCode(), w.told)
	}
	if s := step(t, rep, "check key-location"); !strings.Contains(s.Detail, "environment touchmark-distribute") {
		t.Errorf("check %q", s.Detail)
	}
	rep = w.runGH(t, w.input(t, true))
	onlyOK(t, rep)
	if n := w.writes() - before; n != 0 {
		t.Errorf("the dry run wrote %d times", n)
	}
}

// TestGitHubRepairs: an environment any protected branch may use, with a
// wait timer and an extra policy, is limited to main and keeps its timer;
// a write key in a repository secret fails the check.
func TestGitHubRepairs(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanTeam, "public")
	timer := 5
	if err := w.f.SetEnvironment("octo-org/hub", Environment, ghfake.EnvironmentSpec{Protected: true, WaitTimer: &timer,
		Variables: map[string]string{"TOUCHMARK_WRITE_APP_ID": "77"}, Secrets: []string{"TOUCHMARK_WRITE_APP_KEY"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.f.SetSecret("octo-org/hub", "", "TOUCHMARK_WRITE_APP_KEY", false); err != nil {
		t.Fatal(err)
	}
	in := w.input(t, false)
	rep := w.runGH(t, in)
	wantSteps(t, rep, map[string]Status{"environment": StatusDone, "deployment-branches": StatusDone, "writer-app": StatusOK, "writer-key": StatusOK})
	env, _ := w.f.Environment("octo-org/hub", Environment)
	if env.Protected || !slices.Equal(env.Policies, []string{"branch:main"}) || env.WaitTimer == nil || *env.WaitTimer != 5 {
		t.Errorf("environment %+v", env)
	}
	found := false
	for _, s := range rep.Steps {
		if s.Name == "check key-location" && s.Status == StatusFail && strings.Contains(s.Detail, "repository secret TOUCHMARK_WRITE_APP_KEY") {
			found = true
		}
	}
	if !found || rep.ExitCode() != ExitFailed {
		t.Errorf("no failed check of the repository secret, exit %d: %q", rep.ExitCode(), steps(rep))
	}

	// An extra policy is deleted.
	if err := w.f.SetEnvironment("octo-org/hub", Environment, ghfake.EnvironmentSpec{Policies: []string{"main", "release/*"},
		Variables: map[string]string{"TOUCHMARK_WRITE_APP_ID": "77"}}); err != nil {
		t.Fatal(err)
	}
	rep = w.runGH(t, in)
	if s := step(t, rep, "deployment-branches"); s.Status != StatusDone || !strings.Contains(s.Detail, `deleted the branch policy "release/*"`) {
		t.Errorf("deployment-branches %s %q", s.Status, s.Detail)
	}
	if env, _ := w.f.Environment("octo-org/hub", Environment); !slices.Equal(env.Policies, []string{"branch:main"}) {
		t.Errorf("policies %q", env.Policies)
	}
}

// TestGitHubFreePrivate: a private hub on GitHub Free cannot keep a key to
// one branch: the environment fails with the way out, the ruleset is left
// to the person.
func TestGitHubFreePrivate(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanFree, "private")
	in := w.input(t, true)
	rep := w.runGH(t, in)
	if rep.ExitCode() != ExitOK {
		t.Errorf("dry run exit %d", rep.ExitCode())
	}
	in.DryRun = false
	rep = w.runGH(t, in)
	if s := step(t, rep, "environment"); s.Status != StatusFail || !strings.Contains(s.Detail, "GitHub Free") {
		t.Errorf("environment %s %q", s.Status, s.Detail)
	}
	if s := step(t, rep, "ruleset"); s.Status != StatusManual {
		t.Errorf("ruleset %s %q", s.Status, s.Detail)
	}
	if s := step(t, rep, "writer-app"); s.Status != StatusFail {
		t.Errorf("writer-app %s %q", s.Status, s.Detail)
	}
	if rep.ExitCode() != ExitFailed {
		t.Errorf("exit %d", rep.ExitCode())
	}
}

// TestGitHubPreconditions: a token that does not administer the hub, an
// unknown hub and isolation none are refused before any write.
func TestGitHubPreconditions(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanTeam, "public")
	if _, err := w.f.AddUser("bob"); err != nil {
		t.Fatal(err)
	}
	if err := w.f.Grant("octo-org/hub", "bob", "write"); err != nil {
		t.Fatal(err)
	}
	bob, err := w.f.AddPAT("bob", ghfake.PATSpec{Scopes: []string{"repo"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(in *GitHubInput)
		want string
	}{
		{"not-admin", func(in *GitHubInput) { in.Token = bob }, "does not administer"},
		{"unknown", func(in *GitHubInput) { in.Repo = "octo-org/nope" }, "not visible"},
		{"none", func(in *GitHubInput) { in.Isolation = "none" }, "isolated write key only"},
		{"bad-repo", func(in *GitHubInput) { in.Repo = "hub" }, "not owner/name"},
	} {
		in := w.input(t, false)
		tc.edit(&in)
		before := w.writes()
		_, err := GitHub(context.Background(), in)
		if !IsPrecondition(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a precondition with %q", tc.name, err, tc.want)
		}
		if n := w.writes() - before; n != 0 {
			t.Errorf("%s: wrote %d times", tc.name, n)
		}
	}
}

// TestGitHubDryRunFresh: a dry run writes nothing and starts no flow.
func TestGitHubDryRunFresh(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanTeam, "public")
	rep := w.runGH(t, w.input(t, true))
	wantSteps(t, rep, map[string]Status{
		"environment": StatusWould, "deployment-branches": StatusWould, "reader-app": StatusWould, "writer-app": StatusWould,
		"reader-variable": StatusWould, "writer-variable": StatusWould, "reader-key": StatusWould, "writer-key": StatusWould, "ruleset": StatusWould,
	})
	if n := w.writes(); n != 0 || len(w.told) != 0 || rep.ExitCode() != ExitOK {
		t.Errorf("wrote %d times, told %q, exit %d", n, w.told, rep.ExitCode())
	}
}

// TestGitHubExternal: under external isolation the writer's key stays in
// its file for the person's store; no gh command stores it in the hub.
func TestGitHubExternal(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanTeam, "public")
	in := w.input(t, false)
	in.Isolation = "external"
	rep := w.runGH(t, in)
	s := step(t, rep, "writer-key")
	if s.Status != StatusManual || len(s.Commands) != 0 || !strings.Contains(s.Detail, "secrets store") {
		t.Errorf("writer-key %s %q %q", s.Status, s.Detail, s.Commands)
	}
	if !slices.ContainsFunc(rep.Next, func(n string) bool { return strings.Contains(n, "repo:octo-org/hub:environment:touchmark-distribute") }) {
		t.Errorf("next %q", rep.Next)
	}
}

// TestGitHubUserHub: a hub of a personal account creates the Apps on the
// person's own "new App" page.
func TestGitHubUserHub(t *testing.T) {
	w := newGHWorld(t, ghfake.PlanTeam, "public")
	if _, err := w.f.CreateRepo(ghfake.RepoSpec{Owner: "alice", Name: "hub", Files: []ghfake.File{{Path: "README.md", Content: []byte("# hub\n")}}}); err != nil {
		t.Fatal(err)
	}
	in := w.input(t, false)
	in.Repo = "alice/hub"
	rep := w.runGH(t, in)
	if s := step(t, rep, "reader-app"); s.Status != StatusDone || !strings.Contains(s.Detail, "alice-assets-read") {
		t.Errorf("reader-app %s %q", s.Status, s.Detail)
	}
}

// TestManifestFlowPage: the page answers its own address and GET only,
// with no-store and a CSP that posts to GitHub only; a callback with
// another state or no code is refused; a listen address that is not a
// loopback one is refused.
func TestManifestFlowPage(t *testing.T) {
	if err := checkLoopback("0.0.0.0:0"); err == nil {
		t.Error("0.0.0.0 accepted")
	}
	if err := checkLoopback("[::1]:0"); err != nil {
		t.Error(err)
	}
	f := &manifestFlow{addr: "127.0.0.1:5555", origin: "https://github.com", codes: make(chan string, 1)}
	f.current = &flowRequest{label: "reader", state: "s3cret-state", newApp: "https://github.com/settings/apps/new?state=s3cret-state",
		manifest: appManifest{Name: "acme-assets-read", Permissions: readerPermissions}, result: make(chan string, 1)}
	serve := func(method, target, host string) *http.Response {
		req, _ := http.NewRequest(method, "http://"+host+target, nil)
		req.Host = host
		rec := &recorder{header: http.Header{}}
		f.ServeHTTP(rec, req)
		return &http.Response{StatusCode: rec.status, Header: rec.header, Body: io.NopCloser(&rec.body)}
	}
	if r := serve("GET", "/", "evil.example:5555"); r.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("another host: %d", r.StatusCode)
	}
	if r := serve("POST", "/", f.addr); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", r.StatusCode)
	}
	r := serve("GET", "/", f.addr)
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" || !strings.Contains(r.Header.Get("Content-Security-Policy"), "form-action https://github.com") ||
		r.Header.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(string(body), "contents</code>: read") {
		t.Errorf("page %d %v:\n%s", r.StatusCode, r.Header, body)
	}
	if r := serve("GET", "/callback?code=abc&state=other", f.addr); r.StatusCode != http.StatusBadRequest {
		t.Errorf("another state: %d", r.StatusCode)
	}
	if r := serve("GET", "/callback?code=a%20b&state=s3cret-state", f.addr); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad code: %d", r.StatusCode)
	}
	if r := serve("GET", "/elsewhere", f.addr); r.StatusCode != http.StatusNotFound {
		t.Errorf("another path: %d", r.StatusCode)
	}
	// The code is handed over once; a second redirect for the same App is
	// refused.
	f.current.result <- "<p>Created.</p>"
	if r := serve("GET", "/callback?code=abc&state=s3cret-state", f.addr); r.StatusCode != http.StatusOK || <-f.codes != "abc" {
		t.Errorf("the callback: %d", r.StatusCode)
	}
	if r := serve("GET", "/callback?code=abd&state=s3cret-state", f.addr); r.StatusCode != http.StatusConflict {
		t.Errorf("a second code: %d", r.StatusCode)
	}
}

// recorder is a minimal http.ResponseWriter.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(s int) {
	if r.status == 0 {
		r.status = s
	}
}
func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.body.Write(p)
}

// TestShellQuote: paths with spaces and quotes survive a POSIX shell.
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/tmp/keys/reader.pem": "/tmp/keys/reader.pem",
		`C:\Users\A B\r.pem`:   `'C:\Users\A B\r.pem'`,
		"it's":                 `'it'\''s'`,
		"":                     "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
