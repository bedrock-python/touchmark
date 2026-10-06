//go:build e2e

package gitlabe2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// live is GitLab of this run; nil when TOUCHMARK_E2E_GITLAB_URL is not set.
var live *liveEnv

// account is one account the harness seeded.
type account struct {
	Login string
	Token string
	// ID is the user id, read from GitLab at start.
	ID int64
}

// Account kinds the harness seeds the reader and the writer as.
const (
	accountsService = "service-account"
	accountsGroup   = "group-access-token"
)

// liveEnv is GitLab as scripts/e2e/gitlab.sh started and seeded it.
type liveEnv struct {
	// URL is GitLab's external_url without a trailing slash,
	// http://localhost.
	URL string
	// Host is the provider host touchmark derives from URL ("localhost").
	Host  string
	Image string
	// Group is the seeded top-level group; the reader is a Reporter, the
	// writer a Developer and the person an Owner of it. Subgroup is its
	// subgroup ("acme/sub").
	Group, Subgroup string
	// Accounts tells how the reader and the writer were made:
	// accountsService (instance service accounts: the gitlab-ce images have
	// them from 19.x on; CE 18.11 answers 404 to GET /service_accounts, whose
	// API is EE code there) or
	// accountsGroup (group access tokens of Group).
	Accounts string
	// RunnerBin is the path of touchmark in the runner's jobs; "" when the
	// harness started no runner.
	RunnerBin string
	// Template is the hub template's working tree (gitlab.sh --template),
	// HubGroup the group whose image runner runs jobs in the image they
	// name, and TouchmarkImage the touchmark image under test, as
	// NAME:TAG@sha256:<digest> in a registry the runner's Docker daemon
	// pulls from; all "" without --template.
	Template, HubGroup, TouchmarkImage string
	// Root prepares fixtures; touchmark never gets its token.
	Root, Reader, Writer, Person account
	// Version is GitLab's version, "18.11.12" or so; Major and Minor its
	// numbers.
	Version      string
	Major, Minor int
	// Redact masks every token of the run.
	Redact *redact.Registry
	// HTTP is the client of the fixtures: the product's, with its rules
	// (credentials only to Host, plain http only to loopback).
	HTTP *httpx.Client
}

// roleVars are the variable names of the accounts, by role.
var roleVars = []string{"ROOT", "READER", "WRITER", "PERSON"}

const envPrefix = "TOUCHMARK_E2E_GITLAB_"

// loadEnv reads GitLab from the TOUCHMARK_E2E_GITLAB_* variables. It returns
// nil when TOUCHMARK_E2E_GITLAB_URL is unset.
func loadEnv(getenv func(string) string) (*liveEnv, error) {
	raw := strings.TrimRight(strings.TrimSpace(getenv(envPrefix+"URL")), "/")
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%sURL %q is not an http(s) URL", envPrefix, raw)
	}
	e := &liveEnv{
		URL:       raw,
		Host:      strings.ToLower(u.Host),
		Image:     strings.TrimSpace(getenv(envPrefix + "IMAGE")),
		Group:     strings.TrimSpace(getenv(envPrefix + "GROUP")),
		Subgroup:  strings.TrimSpace(getenv(envPrefix + "SUBGROUP")),
		Accounts:  strings.TrimSpace(getenv(envPrefix + "ACCOUNTS")),
		RunnerBin: strings.TrimSpace(getenv(envPrefix + "RUNNER_BIN")),
		Redact:    redact.New(),

		Template:       strings.TrimSpace(getenv(envPrefix + "TEMPLATE")),
		HubGroup:       strings.TrimSpace(getenv(envPrefix + "HUB_GROUP")),
		TouchmarkImage: strings.TrimSpace(getenv(envPrefix + "TOUCHMARK_IMAGE")),
	}
	if e.Template != "" && (e.HubGroup == "" || e.TouchmarkImage == "") {
		return nil, fmt.Errorf("%sTEMPLATE needs %sHUB_GROUP and %sTOUCHMARK_IMAGE", envPrefix, envPrefix, envPrefix)
	}
	if e.Group == "" {
		e.Group = "acme"
	}
	if e.Subgroup == "" {
		e.Subgroup = e.Group + "/sub"
	}
	if e.Accounts != accountsService && e.Accounts != accountsGroup {
		return nil, fmt.Errorf("%sACCOUNTS %q is not %s or %s", envPrefix, e.Accounts, accountsService, accountsGroup)
	}
	accounts := []*account{&e.Root, &e.Reader, &e.Writer, &e.Person}
	var errs []error
	for i, role := range roleVars {
		login := strings.TrimSpace(getenv(envPrefix + role + "_LOGIN"))
		token := strings.TrimSpace(getenv(envPrefix + role + "_TOKEN"))
		if login == "" || token == "" {
			errs = append(errs, fmt.Errorf("%s%s_LOGIN and %s%s_TOKEN must be set", envPrefix, role, envPrefix, role))
			continue
		}
		*accounts[i] = account{Login: login, Token: token}
		// The Basic forms git sends: GitLab's convention oauth2, a CI job's
		// gitlab-ci-token, the account's login.
		e.Redact.Add(token, "oauth2", "gitlab-ci-token", login)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	e.HTTP = httpx.New(httpx.Options{Redact: e.Redact, UserAgent: "touchmark-e2e"})
	return e, nil
}

// unsetTokens removes the token variables from the process environment, so
// that no child process inherits them.
func unsetTokens() {
	for _, role := range roleVars {
		_ = os.Unsetenv(envPrefix + role + "_TOKEN")
	}
}

// identify reads the ids of the accounts and GitLab's version as root.
func (e *liveEnv) identify() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, a := range []*account{&e.Root, &e.Reader, &e.Writer, &e.Person} {
		var u apiUser
		if _, err := e.HTTP.JSON(ctx, http.MethodGet, e.URL+"/api/v4/user", e.authOf(*a), nil, &u); err != nil {
			return fmt.Errorf("GET /user as %s: %w", a.Login, err)
		}
		if u.Username != a.Login {
			return fmt.Errorf("the token of %s acts as %s", a.Login, u.Username)
		}
		a.ID = u.ID
	}
	var v struct {
		Version string `json:"version"`
	}
	if _, err := e.HTTP.JSON(ctx, http.MethodGet, e.URL+"/api/v4/version", e.authOf(e.Root), nil, &v); err != nil {
		return fmt.Errorf("GET /version: %w", err)
	}
	e.Version = v.Version
	parts := strings.SplitN(strings.SplitN(v.Version, "-", 2)[0], ".", 3)
	if len(parts) >= 2 {
		e.Major, _ = strconv.Atoi(parts[0])
		e.Minor, _ = strconv.Atoi(parts[1])
	}
	return nil
}

// provider is GitLab as a hub.yml provider with the id "gl", resolved as
// touchmark resolves it.
func (e *liveEnv) provider(t testing.TB) config.ResolvedProvider {
	t.Helper()
	h := &config.Hub{Version: 1, ID: "e2e", Providers: []config.Provider{{
		ID: "gl", Type: "gitlab", URL: e.URL, Writer: e.Writer.Login,
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

// needLive skips a test without GitLab.
func needLive(t *testing.T) *liveEnv {
	t.Helper()
	if live == nil {
		t.Skip(envPrefix + "URL is not set: scripts/e2e/gitlab.sh runs these tests against GitLab in Docker")
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
	if e == nil && os.Getenv(envPrefix+"REQUIRED") != "" {
		// gitlab.sh sets it: every test skipping must not pass for a run.
		fmt.Fprintln(os.Stderr, "e2e: "+envPrefix+"REQUIRED is set, but "+envPrefix+"URL is not: GitLab did not reach the tests")
		return 2
	}
	if e != nil {
		if err := e.identify(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", e.Redact.Replace(err.Error()))
			return 2
		}
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
// runs in process for the hub) ignore the machine's configuration and gives
// commits an identity. It returns the temporary home to remove.
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

// finding records a fact about GitLab: in the log of t, and in the summary
// TestMain prints after the tests.
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
	fmt.Fprintf(w, "\n=== FINDINGS on %s (GitLab %s, reader and writer: %s)\n", live.Image, live.Version, live.Accounts)
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
