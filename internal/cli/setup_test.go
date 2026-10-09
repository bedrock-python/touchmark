package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
	"github.com/bedrock-python/touchmark/internal/setup"
)

// setupHub makes a hub checkout with hub.yml (none when empty) and the
// origin remote (none when empty).
func setupHub(t *testing.T, origin, hubYML string) *repo {
	t.Helper()
	h := newHub(t)
	if hubYML != "" {
		h.write(config.HubFile, hubYML)
	}
	h.write("README.md", "# hub\n")
	h.commit("the hub")
	if origin != "" {
		h.git("remote", "add", "origin", origin)
	}
	return h
}

// TestSetupRefusals: setup runs locally only, for GitHub and GitLab only,
// for an isolated write key only, with the maintainer's token from the
// environment; the flags of the other platform are refused.
func TestSetupRefusals(t *testing.T) {
	h := setupHub(t, "https://gitlab.example.com/platform/hub.git", "version: 1\nid: acme-eng\n")
	none := setupHub(t, "https://github.com/octo-org/hub.git", "version: 1\nid: acme-eng\nsecurity:\n  write_isolation: none\n  reason: Gitea\n")
	gh := setupHub(t, "https://github.com/octo-org/hub.git", "version: 1\nid: acme-eng\n")
	token := map[string]string{hubTokenEnv: "glpat-" + strings.Repeat("x", 20)}
	for _, tc := range []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"no-platform", nil, []string{"setup"}, "missing the platform"},
		{"unknown", nil, []string{"setup", "sourcehut", "--hub", h.dir}, `unknown platform "sourcehut"`},
		{"azure-devops", nil, []string{"setup", "azure-devops", "--hub", h.dir}, "setup does not set up azure-devops yet"},
		{"bitbucket", nil, []string{"setup", "bitbucket", "--hub", h.dir}, "setup does not set up bitbucket yet"},
		{"gitea", nil, []string{"setup", "gitea", "--hub", h.dir}, "every branch the secrets"},
		{"forgejo", nil, []string{"setup", "forgejo"}, "setup does not set up forgejo"},
		{"flags-first", nil, []string{"setup", "--hub", h.dir, "gitea", "--dry-run"}, "setup does not set up gitea"},
		{"two-platforms", nil, []string{"setup", "github", "gitlab"}, `unexpected argument "gitlab"`},
		{"no-group", token, []string{"setup", "gitlab", "--hub", h.dir}, "needs --group"},
		{"github-flags", token, []string{"setup", "github", "--hub", gh.dir, "--group", "acme"}, "--group: not for setup github"},
		{"gitlab-flags", token, []string{"setup", "gitlab", "--hub", h.dir, "--group", "acme", "--key-dir", "x"}, "--key-dir: not for setup gitlab"},
		{"ci", map[string]string{"CI": "true", hubTokenEnv: "x"}, []string{"setup", "github", "--hub", gh.dir}, "a maintainer's local run"},
		{"gitlab-ci", map[string]string{"GITLAB_CI": "true"}, []string{"setup", "gitlab", "--hub", h.dir, "--group", "acme"}, "a maintainer's local run"},
		{"isolation-none", token, []string{"setup", "github", "--hub", none.dir}, "security.write_isolation: none"},
		{"self-managed", token, []string{"setup", "gitlab", "--hub", h.dir, "--group", "acme"}, "pass --url"},
		{"no-token", nil, []string{"setup", "github", "--hub", gh.dir}, "set " + hubTokenEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runWith(t, tc.vars, tc.args...)
			if res.code != exitUsage || !strings.Contains(res.stderr, tc.want) {
				t.Errorf("exit %d, want 2 with %q\nstderr:\n%s", res.code, tc.want, res.stderr)
			}
			for _, v := range tc.vars {
				if len(v) > 8 && strings.Contains(res.stderr+res.stdout, v) {
					t.Error("the output shows the token")
				}
			}
		})
	}
}

// ghSetupWorld is a fake GitHub whose hub octo-org/hub alice administers,
// with a checkout whose origin is the fake.
type ghSetupWorld struct {
	f     *ghfake.Server
	h     *repo
	token string
}

func newGHSetupWorld(t *testing.T, hubYML string) *ghSetupWorld {
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
	_, err = f.AddOrg("octo-org", ghfake.PlanTeam)
	must(err)
	_, err = f.AddUser("alice")
	must(err)
	_, err = f.CreateRepo(ghfake.RepoSpec{Owner: "octo-org", Name: "hub", Files: []ghfake.File{{Path: "README.md", Content: []byte("# hub\n")}}})
	must(err)
	must(f.Grant("octo-org/hub", "alice", "admin"))
	token, err := f.AddPAT("alice", ghfake.PATSpec{Scopes: []string{"repo"}})
	must(err)
	return &ghSetupWorld{f: f, h: setupHub(t, f.CloneURL("octo-org/hub"), hubYML), token: token}
}

// finished arranges what a first run and the person's gh commands leave:
// the environment limited to main with the writer's id and key, the
// reader's id and key in the repository, the ruleset.
func (w *ghSetupWorld) finished(t *testing.T) {
	t.Helper()
	if err := w.f.SetEnvironment("octo-org/hub", setup.Environment, ghfake.EnvironmentSpec{Policies: []string{"main"},
		Variables: map[string]string{"TOUCHMARK_WRITE_APP_ID": "2002"}, Secrets: []string{"TOUCHMARK_WRITE_APP_KEY"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.f.SetSecret("octo-org/hub", "", "TOUCHMARK_READ_APP_KEY", false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.AddRuleset("octo-org/hub", ghfake.Ruleset{Name: setup.RulesetName, Include: []string{"~DEFAULT_BRANCH"}}); err != nil {
		t.Fatal(err)
	}
}

// TestSetupGitHub runs setup github through the command line against the
// fake: a hub where everything is in place passes every step, writes
// nothing and exits 0, with a report that passes its schema and shows no
// token; a dry run of a fresh hub writes nothing and starts no browser
// step.
func TestSetupGitHub(t *testing.T) {
	w := newGHSetupWorld(t, "")
	w.finished(t)
	if err := w.f.SetVariable("octo-org/hub", "", "TOUCHMARK_READ_APP_ID", "2001"); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{hubTokenEnv: w.token}
	res := runWith(t, vars, "setup", "github", "--hub", w.h.dir, "--url", w.f.URL(), "--format", "json")
	if res.code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	validateSchema(t, "setup", []byte(res.stdout))
	var rep setup.Report
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Steps {
		if s.Status != setup.StatusOK {
			t.Errorf("step %s: %s %q", s.Name, s.Status, s.Detail)
		}
	}
	if rep.Platform != "github" || rep.Hub != "octo-org/hub" || rep.Isolation != "platform" {
		t.Errorf("report %+v", rep)
	}
	if n := fakeWrites(w.f); n != 0 {
		t.Errorf("wrote %d times", n)
	}
	if strings.Contains(res.stdout+res.stderr, w.token) || !strings.Contains(res.stderr, "with the maintainer's token from "+hubTokenEnv) {
		t.Errorf("stderr:\n%s", res.stderr)
	}

	fresh := newGHSetupWorld(t, "version: 1\nid: change-me\n")
	res = runWith(t, map[string]string{hubTokenEnv: fresh.token}, "setup", "github", "--hub", fresh.h.dir, "--url", fresh.f.URL(), "--dry-run")
	if res.code != exitOK || !strings.Contains(res.stdout, "would create the App octo-org-assets-read") || strings.Contains(res.stderr, "open http") {
		t.Errorf("dry run: exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if n := fakeWrites(fresh.f); n != 0 {
		t.Errorf("the dry run wrote %d times", n)
	}
}

// fakeWrites counts the fake's requests that change something.
func fakeWrites(f *ghfake.Server) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			n++
		}
	}
	return n
}

// TestSetupTokenStaysHome: a hub.yml whose provider sends the API to
// another host is refused before the token is sent anywhere.
func TestSetupTokenStaysHome(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	evil := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		http.Error(rw, "no", http.StatusUnauthorized)
	}))
	defer evil.Close()
	w := newGHSetupWorld(t, "")
	hubYML := fmt.Sprintf("version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    url: %s\n    api_url: %s/api/v3\n", w.f.URL(), strings.Replace(evil.URL, "127.0.0.1", "localhost", 1))
	w.h.write(config.HubFile, hubYML)
	w.h.commit("api elsewhere")
	res := runWith(t, map[string]string{hubTokenEnv: w.token}, "setup", "github", "--hub", w.h.dir)
	if res.code != exitUsage || !strings.Contains(res.stderr, "the maintainer's token goes to the hub's own API only") {
		t.Errorf("exit %d\n%s", res.code, res.stderr)
	}
	if len(hits) != 0 {
		t.Errorf("the other host got %q", hits)
	}
}

// TestSetupGitLabRefusedToken: GitLab refuses the token: exit 2, and the
// token went to the instance's API only, in its header.
func TestSetupGitLabRefusedToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	const token = "glpat-refused-token-0123456789"
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+fmt.Sprint(r.Header.Get("Private-Token") == token))
		mu.Unlock()
		http.Error(rw, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer api.Close()
	hubYML := fmt.Sprintf("version: 1\nid: acme-eng\nproviders:\n  - id: gl\n    type: gitlab\n    url: %s\n", api.URL)
	h := setupHub(t, api.URL+"/platform/hub.git", hubYML)
	res := runWith(t, map[string]string{hubTokenEnv: token}, "setup", "gitlab", "--hub", h.dir, "--group", "acme/services")
	if res.code != exitUsage || !strings.Contains(res.stderr, "GitLab refuses the token") || strings.Contains(res.stderr, token) {
		t.Errorf("exit %d\n%s", res.code, res.stderr)
	}
	if len(seen) != 1 || seen[0] != "GET /api/v4/user true" {
		t.Errorf("requests %q", seen)
	}
}

// TestSetupProvider: which provider setup works on, and the names of its
// variables: the short ones with one provider, TOUCHMARK_<ID>_ with
// several.
func TestSetupProvider(t *testing.T) {
	two := "version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    url: https://github.com\n" +
		"  - id: corp\n    type: gitlab\n    url: https://gitlab.example.com\n    writer: tm-writer\n"
	for _, tc := range []struct {
		name, origin, hubYML, platform string
		opts                           setupOptions
		id, prefix, path, err          string
		short                          bool
	}{
		{name: "template-github", origin: "https://github.com/octo-org/hub.git", hubYML: "version: 1\nid: change-me\n", platform: "github",
			id: "github", path: "octo-org/hub", short: true},
		{name: "gitlab-com", origin: "git@gitlab.com:acme/hub.git", platform: "gitlab", id: "gitlab", path: "acme/hub", short: true},
		{name: "several", origin: "https://gitlab.example.com/platform/hub.git", hubYML: two, platform: "gitlab",
			id: "corp", prefix: "TOUCHMARK_CORP_", path: "platform/hub"},
		{name: "by-id", origin: "https://gitlab.example.com/platform/hub.git", hubYML: two, platform: "github", opts: setupOptions{provider: "corp"},
			err: "provider corp is gitlab, not github"},
		{name: "self-managed-url", origin: "https://git.acme.test/platform/hub.git", platform: "gitlab",
			opts: setupOptions{url: "https://git.acme.test"}, id: "gitlab", path: "platform/hub", short: true},
		{name: "origin-elsewhere", origin: "https://github.com/octo-org/hub.git", platform: "gitlab",
			opts: setupOptions{url: "https://git.acme.test"}, err: "pass --project"},
		{name: "project", origin: "https://github.com/octo-org/hub.git", platform: "gitlab",
			opts: setupOptions{url: "https://git.acme.test", project: "/platform/hub/"}, id: "gitlab", path: "platform/hub", short: true},
		{name: "missing-in-several", origin: "https://git.acme.test/x/hub.git", hubYML: two, platform: "gitlab",
			opts: setupOptions{url: "https://git.acme.test"}, err: "lists several providers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupHub(t, tc.origin, tc.hubYML)
			var cfg *config.Hub
			if tc.hubYML != "" {
				var err error
				if cfg, _, err = config.ParseHub([]byte(tc.hubYML)); err != nil {
					t.Fatal(err)
				}
			}
			hb, err := openHub(t.Context(), &env{getenv: func(string) string { return "" }}, h.dir, true)
			if err != nil {
				t.Fatal(err)
			}
			rp, path, err := hb.setupProvider(t.Context(), cfg, tc.platform, &tc.opts)
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("err %v, want %q", err, tc.err)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			if rp.ID != tc.id || rp.Short != tc.short || path != tc.path || (tc.prefix != "" && rp.EnvPrefix != tc.prefix) {
				t.Errorf("provider %s short %v prefix %s, path %s", rp.ID, rp.Short, rp.EnvPrefix, path)
			}
		})
	}
}
