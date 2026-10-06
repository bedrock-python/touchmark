//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// live is the forge of this run; nil when TOUCHMARK_E2E_URL is not set.
var live *liveEnv

// account is one account the harness seeded.
type account struct {
	Login string
	Token string
}

// liveEnv is the forge scripts/e2e/gitea.sh started and seeded.
type liveEnv struct {
	// URL is the forge's root without a trailing slash, e.g.
	// http://localhost:3000.
	URL string
	// Host is the provider host touchmark derives from URL ("localhost:3000").
	Host   string
	Flavor string // "gitea" or "forgejo"
	Image  string
	// Org is the organisation the harness created, with the reader in a
	// read team, the writer in a write team and the person an owner.
	Org string
	// SignInRequired is set when the forge runs with [service]
	// REQUIRE_SIGNIN_VIEW = true (gitea.sh --require-signin).
	SignInRequired bool
	// Template is the hub template's working tree (gitea.sh --template),
	// HubOrg the organisation of hubs, of which the person is an owner,
	// and TouchmarkImage the touchmark image under test, as
	// NAME:TAG@sha256:<digest> in a registry the runner's Docker daemon
	// pulls from; all "" without --template.
	Template, HubOrg, TouchmarkImage string
	// Admin prepares fixtures; touchmark never gets its token.
	Admin, Reader, Writer, Person account
	// Redact masks every token of the run.
	Redact *redact.Registry
	// HTTP is the client of the fixtures: the product's, with its rules
	// (credentials only to Host).
	HTTP *httpx.Client
}

// roleVars are the variable names of the accounts, by role.
var roleVars = []string{"ADMIN", "READER", "WRITER", "PERSON"}

// loadEnv reads the forge from the TOUCHMARK_E2E_* variables and removes
// the token variables from the process environment, so that no child
// process inherits them. It returns nil when TOUCHMARK_E2E_URL is unset.
func loadEnv(getenv func(string) string) (*liveEnv, error) {
	raw := strings.TrimRight(strings.TrimSpace(getenv("TOUCHMARK_E2E_URL")), "/")
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("TOUCHMARK_E2E_URL %q is not an http(s) URL", raw)
	}
	e := &liveEnv{
		URL:    raw,
		Host:   strings.ToLower(u.Host),
		Flavor: strings.TrimSpace(getenv("TOUCHMARK_E2E_FLAVOR")),
		Image:  strings.TrimSpace(getenv("TOUCHMARK_E2E_IMAGE")),
		Org:    strings.TrimSpace(getenv("TOUCHMARK_E2E_ORG")),
		Redact: redact.New(),
		// gitea.sh sets it to 0 or 1.
		SignInRequired: strings.TrimSpace(getenv("TOUCHMARK_E2E_REQUIRE_SIGNIN")) == "1",

		Template:       strings.TrimSpace(getenv("TOUCHMARK_E2E_TEMPLATE")),
		HubOrg:         strings.TrimSpace(getenv("TOUCHMARK_E2E_HUB_ORG")),
		TouchmarkImage: strings.TrimSpace(getenv("TOUCHMARK_E2E_TOUCHMARK_IMAGE")),
	}
	if e.Template != "" && (e.HubOrg == "" || e.TouchmarkImage == "") {
		return nil, fmt.Errorf("TOUCHMARK_E2E_TEMPLATE needs TOUCHMARK_E2E_HUB_ORG and TOUCHMARK_E2E_TOUCHMARK_IMAGE")
	}
	if e.Flavor != "gitea" && e.Flavor != "forgejo" {
		return nil, fmt.Errorf("TOUCHMARK_E2E_FLAVOR %q is not gitea or forgejo", e.Flavor)
	}
	if e.Org == "" {
		e.Org = "acme"
	}
	accounts := []*account{&e.Admin, &e.Reader, &e.Writer, &e.Person}
	var errs []error
	for i, role := range roleVars {
		login := strings.TrimSpace(getenv("TOUCHMARK_E2E_" + role + "_LOGIN"))
		token := strings.TrimSpace(getenv("TOUCHMARK_E2E_" + role + "_TOKEN"))
		if login == "" || token == "" {
			errs = append(errs, fmt.Errorf("TOUCHMARK_E2E_%s_LOGIN and TOUCHMARK_E2E_%s_TOKEN must be set", role, role))
			continue
		}
		*accounts[i] = account{Login: login, Token: token}
		// The Basic forms git sends: the account's login, or a fixed user
		// the forges ignore.
		e.Redact.Add(token, login, "x-access-token", "oauth2")
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	e.HTTP = httpx.New(httpx.Options{Redact: e.Redact, UserAgent: "touchmark-e2e"})
	return e, nil
}

// unsetTokens removes the token variables from the process environment.
func unsetTokens() {
	for _, role := range roleVars {
		_ = os.Unsetenv("TOUCHMARK_E2E_" + role + "_TOKEN")
	}
}

// provider is the forge as a hub.yml provider with the id "forge", resolved
// as touchmark resolves it.
func (e *liveEnv) provider(t testing.TB) config.ResolvedProvider {
	t.Helper()
	h := &config.Hub{Version: 1, ID: "e2e", Providers: []config.Provider{{
		ID: "forge", Type: e.Flavor, URL: e.URL, Writer: e.Writer.Login,
	}}}
	rps, err := h.ResolveProviders(func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolve the provider: %v", err)
	}
	if len(rps) != 1 || rps[0].Host != e.Host {
		t.Fatalf("resolve the provider: %+v, want one on %s", rps, e.Host)
	}
	return rps[0]
}

// credential is the token of a as touchmark reads it from the environment.
func credential(a account) auth.Credential {
	return auth.Credential{Kind: auth.Token, Token: a.Token}
}

// needLive skips a test without a forge.
func needLive(t *testing.T) *liveEnv {
	t.Helper()
	if live == nil {
		t.Skip("TOUCHMARK_E2E_URL is not set: scripts/e2e/gitea.sh runs these tests against a forge in Docker")
	}
	return live
}

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	e, err := loadEnv(os.Getenv)
	unsetTokens()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	if e == nil && os.Getenv("TOUCHMARK_E2E_REQUIRED") != "" {
		// gitea.sh sets it: every test skipping must not pass for a run.
		fmt.Fprintln(os.Stderr, "e2e: TOUCHMARK_E2E_REQUIRED is set, but TOUCHMARK_E2E_URL is not: the forge did not reach the tests")
		return 2
	}
	live = e
	flag.Parse()
	defer watchdog(os.Stdout)()
	home, err := isolateGit()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 2
	}
	defer os.RemoveAll(home)
	code := m.Run()
	printFindings(os.Stdout)
	return code
}

// watchdog prints the findings so far shortly before go test's -timeout
// ends the run: its panic never returns to TestMain, which prints them
// otherwise. It returns the function that stops it.
func watchdog(w io.Writer) (stop func()) {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return func() {}
	}
	d, err := time.ParseDuration(f.Value.String())
	if err != nil || d <= 2*time.Minute {
		return func() {}
	}
	t := time.AfterFunc(d-time.Minute, func() {
		fmt.Fprintln(w, "\n=== e2e: the run ends by -timeout within a minute")
		printFindings(w)
	})
	return func() { t.Stop() }
}

// isolateGit makes the git the tests run (and the one the command line
// runs in process for the hub) ignore the machine's configuration, as the
// command line's own tests do, and gives commits an identity. It returns
// the temporary home to remove.
func isolateGit() (string, error) {
	home, err := os.MkdirTemp("", "touchmark-e2e-home-")
	if err != nil {
		return "", err
	}
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		os.RemoveAll(home)
		return "", err
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CONFIG_GLOBAL":       global,
		"HOME":                    home,
		"XDG_CONFIG_HOME":         home,
		"GIT_AUTHOR_NAME":         "touchmark e2e",
		"GIT_AUTHOR_EMAIL":        "e2e@example.com",
		"GIT_COMMITTER_NAME":      "touchmark e2e",
		"GIT_COMMITTER_EMAIL":     "e2e@example.com",
		"GIT_TERMINAL_PROMPT":     "0",
		"GIT_CEILING_DIRECTORIES": os.TempDir(),
	} {
		if err := os.Setenv(k, v); err != nil {
			os.RemoveAll(home)
			return "", err
		}
	}
	return home, nil
}

// findings are the platform facts the run observed, for the summary.
var findings struct {
	sync.Mutex
	lines []string
}

// finding records a fact about the platform: in the log of t, and in the
// summary TestMain prints after the tests.
func finding(t testing.TB, key, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if live != nil {
		msg = live.Redact.Replace(msg)
	}
	t.Logf("FINDING %s: %s", key, msg)
	findings.Lock()
	findings.lines = append(findings.lines, key+": "+msg)
	findings.Unlock()
}

func printFindings(w io.Writer) {
	findings.Lock()
	defer findings.Unlock()
	if len(findings.lines) == 0 || live == nil {
		return
	}
	fmt.Fprintf(w, "\n=== FINDINGS on %s (%s)\n", live.Image, live.Flavor)
	for _, line := range findings.lines {
		fmt.Fprintf(w, "    %s\n", line)
	}
}

// randHex returns n random bytes in hex.
func randHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
