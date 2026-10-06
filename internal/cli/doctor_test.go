package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The doctor tests replace the write drivers, so none of them runs in
// parallel. Their platform is the fake in memory mode: doctor reads no git.

const (
	doctorHubToken = "doctor-hub-token-0123456789"
	// doctorSecretRepo is the name of a private target that no output of a
	// public hub may hold.
	doctorSecretRepo = "acme/secret-roadmap"
)

// doctorWorld is a fake GitHub with the hub's writer and targets.
type doctorWorld struct {
	t      *testing.T
	p      *fake.Platform
	writer platform.Account
	person platform.Account
	repos  map[string]platform.Repo
}

// newDoctorWorld serves acme/api (opted in, writable), acme/ro (opted in,
// read only), acme/web (not opted in) and the private doctorSecretRepo
// (opted in, read only).
func newDoctorWorld(t *testing.T) *doctorWorld {
	t.Helper()
	p := fake.New("localhost")
	w := &doctorWorld{t: t, p: p, writer: p.AddAccount("acme-write[bot]", platform.KindBot), person: p.AddAccount("jdoe", platform.KindUser),
		repos: map[string]platform.Repo{}}
	add := func(path, visibility string, files ...string) platform.Repo {
		r := p.AddRepo(platform.Repo{Path: path, Visibility: visibility})
		for i := 0; i+1 < len(files); i += 2 {
			p.SetFile(r.ID, files[i], []byte(files[i+1]), "")
		}
		w.repos[path] = r
		return r
	}
	api := add("acme/api", "public", planOptIn, "version: 1\n")
	p.GrantWrite(api.ID, w.writer)
	add("acme/ro", "public", planOptIn, "version: 1\n")
	add("acme/web", "public", "README.md", "not opted in\n")
	add(doctorSecretRepo, "private", planOptIn, "version: 1\n")
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	drivers := distributeDrivers
	t.Cleanup(func() { distributeDrivers = drivers })
	distributeDrivers = map[string]distributeDriver{"github": func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
		if client == nil || rp.Host != p.Host() || c.Token != distWriteToken {
			return nil, fmt.Errorf("unexpected provider %s or credential", rp.Host)
		}
		return p.Writer(w.writer), nil
	}}
	return w
}

// runDoctorCmd runs doctor against the hub and fails the test unless it
// exits with code.
func runDoctorCmd(t *testing.T, h *repo, vars map[string]string, code int, extra ...string) result {
	t.Helper()
	args := append([]string{"doctor", "--hub", h.dir}, extra...)
	res := runWith(t, vars, args...)
	if res.code != code {
		t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, code, res.stdout, res.stderr)
	}
	return res
}

func decodeDoctor(t *testing.T, out string) report.Doctor {
	t.Helper()
	var doc report.Doctor
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode doctor report: %v\n%s", err, out)
	}
	return doc
}

// checkIn returns check name of cs.
func checkIn(t *testing.T, where string, cs []report.DoctorCheck, name string) report.DoctorCheck {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s: no check %s in %+v", where, name, cs)
	return report.DoctorCheck{}
}

// A maintainer's doctor: the matrix of the targets, the unknowns of a
// local run, and --strict.
func TestDoctorLocal(t *testing.T) {
	h := distHub(t)
	w := newDoctorWorld(t)
	res := runDoctorCmd(t, h, distEnv(), exitFailed, "--hub-fp", distFP, "--format", "json")
	validateSchema(t, "doctor", []byte(res.stdout))
	doc := decodeDoctor(t, res.stdout)
	if doc.Schema != report.DoctorSchema || doc.Hub.ID != "acme-eng" || doc.HubToken {
		t.Errorf("report %+v", doc)
	}
	checkByTarget := map[string]string{}
	for _, tg := range doc.Targets {
		if tg.Skipped != "" {
			checkByTarget[tg.Path] = "skipped:" + tg.Skipped
			continue
		}
		for _, c := range tg.Checks {
			if c.Name == "access" {
				checkByTarget[tg.Path] = string(c.Status)
			}
		}
	}
	want := map[string]string{"acme/api": "ok", "acme/ro": "fail", "acme/web": "skipped:not-opted-in", doctorSecretRepo: "fail"}
	for path, status := range want {
		if checkByTarget[path] != status {
			t.Errorf("%s: %q, want %q (all: %v)", path, checkByTarget[path], status, checkByTarget)
		}
	}
	wantCheck := func(cs []report.DoctorCheck, name string, status report.CheckStatus) {
		t.Helper()
		if c := checkIn(t, name, cs, name); c.Status != status {
			t.Errorf("%s: %s %q, want %s", name, c.Status, c.Detail, status)
		}
	}
	wantCheck(doc.HubChecks, "write-isolation", report.StatusUnknown)
	wantCheck(doc.Providers[0].Checks, "writer", report.StatusOK)
	// The hub is on github.com, the provider on another host.
	wantCheck(doc.Providers[0].Checks, "hub-hidden", report.StatusOK)
	if strings.Contains(res.stdout+res.stderr, distWriteToken) {
		t.Error("the write token reached the output")
	}
	if len(w.p.Writes()) > 0 {
		t.Errorf("doctor wrote: %q", w.p.Writes())
	}
	// Text: the matrix names the targets.
	res = runDoctorCmd(t, h, distEnv(), exitFailed, "--hub-fp", distFP)
	for _, s := range []string{"touchmark doctor · hub acme-eng", "gh:acme/api", "not-opted-in 1", "Summary  ok "} {
		if !strings.Contains(res.stdout, s) {
			t.Errorf("text output lacks %q:\n%s", s, res.stdout)
		}
	}
	if !regexp.MustCompile(`(?m)^  gh:acme/ro +fail `).MatchString(res.stdout) {
		t.Errorf("the matrix does not show acme/ro's access failing:\n%s", res.stdout)
	}
	// Every target writable: 0, and 3 with --strict for the unknowns.
	for _, path := range []string{"acme/ro", doctorSecretRepo} {
		w.p.GrantWrite(w.repos[path].ID, w.writer)
	}
	runDoctorCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP)
	runDoctorCmd(t, h, distEnv(), 3, "--hub-fp", distFP, "--strict")
}

// doctorActionsEnv is a scheduled GitHub Actions job of the default branch
// of a hub whose visibility the event does not tell (a schedule event's
// payload has no repository), with the write token and the probe's answer,
// against the hub API api.
func doctorActionsEnv(t *testing.T, api string) map[string]string {
	t.Helper()
	event := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(event, []byte(`{"schedule": "0 5 * * 1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"CI":                       "true",
		"GITHUB_ACTIONS":           "true",
		"GITHUB_SERVER_URL":        "https://github.com",
		"GITHUB_API_URL":           api,
		"GITHUB_REPOSITORY_ID":     "712345678",
		"GITHUB_REPOSITORY":        "acme/engineering-assets",
		"GITHUB_EVENT_NAME":        "schedule",
		"GITHUB_EVENT_PATH":        event,
		"GITHUB_REF":               "refs/heads/master",
		"GITHUB_REF_NAME":          "master",
		"GITHUB_REF_TYPE":          "branch",
		"GITHUB_TOKEN":             hubJobToken,
		"TOUCHMARK_GH_WRITE_TOKEN": distWriteToken,
		"TOUCHMARK_KEY_EXPOSED":    "false",
		"GITHUB_STEP_SUMMARY":      filepath.Join(t.TempDir(), "summary.md"),
	}
}

// doctorHubAPI serves the hub channel of a public hub acme/engineering-assets
// (its tip, the repository with its visibility, the environment
// touchmark-distribute) and counts the requests.
func doctorHubAPI(t *testing.T, visibility string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if serveEnvironment(rw, r, hubEnvironment, hubEnvironmentPolicies) {
			return
		}
		switch r.URL.Path {
		case "/repos/acme/engineering-assets":
			fmt.Fprintf(rw, `{"full_name": "acme/engineering-assets", "private": %v, "visibility": %q}`, visibility != "public", visibility)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A scheduled doctor in GitHub Actions of a public hub: the visibility
// comes through the hub channel, and no output names the private target:
// stdout, stderr (annotations included), the step summary and the report
// files.
func TestDoctorActionsPublicHub(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	h := distHub(t)
	newDoctorWorld(t)
	api := doctorHubAPI(t, "public")
	vars := doctorActionsEnv(t, api.URL)
	var outputs []string
	for _, format := range []string{"text", "json", "markdown"} {
		res := runDoctorCmd(t, h, vars, exitFailed, "--format", format)
		outputs = append(outputs, "stdout "+format+":\n"+res.stdout, "stderr "+format+":\n"+res.stderr)
		if !strings.Contains(res.stderr, "::error title=touchmark::gh:acme/ro access: ") {
			t.Errorf("no annotation for acme/ro:\n%s", res.stderr)
		}
	}
	for _, name := range []string{vars["GITHUB_STEP_SUMMARY"], filepath.Join(work, "touchmark-doctor.json"), filepath.Join(work, "touchmark-doctor.md")} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, name+":\n"+string(data))
	}
	for _, out := range outputs {
		// The runner reads the masks from ::add-mask:: commands: those
		// carry the tokens, and nothing else may.
		out = regexp.MustCompile(`(?m)^::add-mask::.*$`).ReplaceAllString(out, "")
		if strings.Contains(out, "secret-roadmap") || strings.Contains(out, distWriteToken) || strings.Contains(out, hubJobToken) {
			t.Errorf("an output names the private target or holds a token:\n%s", out)
		}
	}
	doc := decodeDoctor(t, strings.SplitN(outputs[2], "\n", 2)[1])
	validateSchema(t, "doctor", []byte(strings.SplitN(outputs[2], "\n", 2)[1]))
	if data, err := os.ReadFile(filepath.Join(work, "touchmark-doctor.json")); err == nil {
		validateSchema(t, "doctor", data)
	}
	hidden := 0
	for _, tg := range doc.Targets {
		if tg.Path == "" {
			hidden++
		}
	}
	if hidden != 1 {
		t.Errorf("%d targets not named, want 1: %+v", hidden, doc.Targets)
	}
	for _, name := range []string{"write-isolation", "environment"} {
		if c := checkIn(t, "hub", doc.HubChecks, name); c.Status != report.StatusOK {
			t.Errorf("%s: %s %q", name, c.Status, c.Detail)
		}
	}
	// The hub's path comes from CI: the writer does not see it.
	if c := checkIn(t, "gh", doc.Providers[0].Checks, "hub-hidden"); c.Status != report.StatusOK {
		t.Errorf("hub-hidden: %s %q", c.Status, c.Detail)
	}
	summary, _ := os.ReadFile(vars["GITHUB_STEP_SUMMARY"])
	if !strings.Contains(string(summary), "### touchmark doctor") || !strings.Contains(string(summary), "1 target private in a public hub, not named") {
		t.Errorf("step summary:\n%s", summary)
	}

	// A private hub names every target.
	api = doctorHubAPI(t, "private")
	vars["GITHUB_API_URL"] = api.URL
	res := runDoctorCmd(t, h, vars, exitFailed)
	if !strings.Contains(res.stdout, "secret-roadmap") {
		t.Errorf("a private hub's doctor does not name its private target:\n%s", res.stdout)
	}
}

// doctor refuses to run where it must not hold the write key, and
// --hub-token outside a maintainer's shell.
func TestDoctorRefusals(t *testing.T) {
	h := distHub(t)
	newDoctorWorld(t)
	api := doctorHubAPI(t, "public")
	vars := doctorActionsEnv(t, api.URL)
	vars["GITHUB_EVENT_NAME"], vars["GITHUB_REF"], vars["GITHUB_REF_NAME"] = "pull_request", "refs/pull/4/merge", "4/merge"
	res := runDoctorCmd(t, h, vars, exitUsage)
	if !strings.Contains(res.stderr, "doctor does not run for the pull_request event") {
		t.Errorf("stderr: %s", res.stderr)
	}
	vars = doctorActionsEnv(t, api.URL)
	vars[hubTokenEnv] = doctorHubToken
	res = runDoctorCmd(t, h, vars, exitUsage, "--hub-token")
	if !strings.Contains(res.stderr, "--hub-token is for a maintainer's local run") || strings.Contains(res.stderr, doctorHubToken) {
		t.Errorf("stderr: %s", res.stderr)
	}
	res = runDoctorCmd(t, h, distEnv(), exitUsage, "--hub-fp", distFP, "--hub-token")
	if !strings.Contains(res.stderr, "set "+hubTokenEnv) {
		t.Errorf("stderr: %s", res.stderr)
	}
	res = runDoctorCmd(t, h, map[string]string{}, exitUsage, "--hub-fp", distFP)
	if !strings.Contains(res.stderr, "no write credential") {
		t.Errorf("stderr: %s", res.stderr)
	}
}

// doctor --hub-token on GitHub: the secrets' names by where they live,
// graded; the maintainer's token reaches the hub's API only, and no
// output.
func TestDoctorHubTokenGitHub(t *testing.T) {
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+doctorHubToken {
			http.Error(rw, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		seen = append(seen, r.URL.Path)
		if serveEnvironment(rw, r, hubEnvironment, hubEnvironmentPolicies) {
			return
		}
		const repo = "/api/v3/repos/acme/engineering-assets"
		switch r.URL.Path {
		case "/api/v3/repositories/712345678":
			fmt.Fprint(rw, `{"full_name": "acme/engineering-assets", "default_branch": "master", "visibility": "public", "private": false, "owner": {"login": "acme", "type": "Organization"}}`)
		case repo + "/actions/secrets":
			fmt.Fprint(rw, `{"total_count": 2, "secrets": [{"name": "TOUCHMARK_GH_WRITE_TOKEN"}, {"name": "NPM_TOKEN"}]}`)
		case repo + "/actions/organization-secrets":
			http.Error(rw, `{"message": "Must have admin rights"}`, http.StatusForbidden)
		case repo + "/dependabot/secrets":
			fmt.Fprint(rw, `{"total_count": 0, "secrets": []}`)
		case repo + "/environments":
			fmt.Fprint(rw, `{"total_count": 1, "environments": [{"name": "touchmark-distribute"}]}`)
		case repo + "/environments/touchmark-distribute":
			fmt.Fprint(rw, hubEnvironment)
		case repo + "/environments/touchmark-distribute/deployment-branch-policies":
			fmt.Fprint(rw, hubEnvironmentPolicies)
		case repo + "/environments/touchmark-distribute/secrets":
			fmt.Fprint(rw, `{"total_count": 1, "secrets": [{"name": "TOUCHMARK_GH_WRITE_APP_KEY"}]}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	defer api.Close()
	h := newHub(t)
	// The API is on the hub's host (localhost, the fingerprint's), on the
	// test server's port.
	hubAPI := strings.Replace(api.URL, "127.0.0.1", "localhost", 1) + "/api/v3"
	h.write("hub.yml", "version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    url: http://localhost\n    api_url: "+hubAPI+"\n    writer: acme-write[bot]\n")
	h.write("targets.yml", distTargetsYML)
	h.write("packs/base/AGENTS.md", planAgentsV2)
	h.commit("the hub")
	newDoctorWorld(t)
	vars := distEnv()
	vars[hubTokenEnv] = doctorHubToken
	res := runDoctorCmd(t, h, vars, exitFailed, "--hub-fp", "localhost/712345678", "--hub-token", "--format", "json")
	if strings.Contains(res.stdout+res.stderr, doctorHubToken) {
		t.Error("the maintainer's token reached the output")
	}
	validateSchema(t, "doctor", []byte(res.stdout))
	doc := decodeDoctor(t, res.stdout)
	if !doc.HubToken {
		t.Error("hub_token is not set")
	}
	got := map[string]report.CheckStatus{}
	for _, c := range doc.HubChecks {
		if c.Name == "key-location" {
			got[c.Detail] = c.Status
		}
	}
	find := func(sub string, status report.CheckStatus) {
		t.Helper()
		for d, s := range got {
			if strings.Contains(d, sub) {
				if s != status {
					t.Errorf("%q: %s, want %s", d, s, status)
				}
				return
			}
		}
		t.Errorf("no key-location check with %q in %v", sub, got)
	}
	find("repository secret TOUCHMARK_GH_WRITE_TOKEN", report.StatusFail)
	find("secret TOUCHMARK_GH_WRITE_APP_KEY is in environment touchmark-distribute", report.StatusOK)
	find("organization secrets shared with the repository: HTTP 403", report.StatusUnknown)
	for d := range got {
		if strings.Contains(d, "NPM_TOKEN") {
			t.Errorf("a secret that holds no write key is graded: %s", d)
		}
	}
	if len(seen) == 0 {
		t.Error("the hub's API was not asked")
	}

	// A token the hub refuses: the check is unknown, and the rest of the
	// report stands.
	vars = distEnv()
	vars[hubTokenEnv] = "refused-hub-token-0123456789"
	res = runDoctorCmd(t, h, vars, exitFailed, "--hub-fp", "localhost/712345678", "--hub-token", "--format", "json")
	doc = decodeDoctor(t, res.stdout)
	var keyChecks []report.DoctorCheck
	for _, c := range doc.HubChecks {
		if c.Name == "key-location" {
			keyChecks = append(keyChecks, c)
		}
	}
	if len(keyChecks) != 1 || keyChecks[0].Status != report.StatusUnknown || !strings.Contains(keyChecks[0].Detail, "could not be read") ||
		strings.Contains(res.stdout+res.stderr, "refused-hub-token") || len(doc.Targets) == 0 {
		t.Errorf("key-location %+v, %d targets\n%s", keyChecks, len(doc.Targets), res.stderr)
	}

	// Without the write key (it lives in the CI only): the hub's checks
	// alone, and a warning that the writer and target checks are the CI's.
	// --hub-token once refused to run without the write key.
	res = runDoctorCmd(t, h, map[string]string{hubTokenEnv: doctorHubToken}, exitFailed, "--hub-fp", "localhost/712345678", "--hub-token", "--format", "json")
	validateSchema(t, "doctor", []byte(res.stdout))
	doc = decodeDoctor(t, res.stdout)
	if len(doc.Providers) != 0 || len(doc.Targets) != 0 || !doc.HubToken ||
		!slices.ContainsFunc(doc.Warnings, func(w string) bool {
			return strings.Contains(w, "checked the hub only: provider gh has no write credential")
		}) {
		t.Errorf("hub only: providers %+v, %d targets, warnings %q", doc.Providers, len(doc.Targets), doc.Warnings)
	}
	if !slices.ContainsFunc(doc.HubChecks, func(c report.DoctorCheck) bool {
		return c.Status == report.StatusFail && strings.Contains(c.Detail, "repository secret TOUCHMARK_GH_WRITE_TOKEN")
	}) {
		t.Errorf("hub only: hub checks %+v", doc.HubChecks)
	}
	if !strings.Contains(res.stderr, "on localhost:") {
		t.Errorf("the host the token goes to is not printed:\n%s", res.stderr)
	}

	// A checkout whose hub.yml sends the API elsewhere than the hub's host
	// never gets the token. A branch's api_url once received
	// the maintainer's token.
	seen = nil
	h.write("hub.yml", "version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    url: http://localhost\n    api_url: "+api.URL+"/api/v3\n    writer: acme-write[bot]\n")
	h.commit("the API elsewhere")
	res = runDoctorCmd(t, h, map[string]string{hubTokenEnv: doctorHubToken}, exitUsage, "--hub-fp", "localhost/712345678", "--hub-token")
	if !strings.Contains(res.stderr, "not to the hub's host localhost") || len(seen) > 0 {
		t.Errorf("a foreign API: requests %q\n%s", seen, res.stderr)
	}
}

// The step summary appends within what is left of the step's 1 MiB, and
// writes nothing without room.
func TestWriteSummaryBudget(t *testing.T) {
	name := filepath.Join(t.TempDir(), "summary.md")
	full := strings.Repeat("x", report.MaxSummary-100)
	if err := os.WriteFile(name, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	var limit int
	err := writeSummary(redact.New(), name, func(_ io.Writer, n int) error {
		limit = n
		return nil
	})
	if err != nil || limit != 100 {
		t.Errorf("limit %d, %v", limit, err)
	}
	rep := report.NewDelivery("distribute", "test")
	for i := range 60000 {
		rep.Targets = append(rep.Targets, report.DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: fmt.Sprint(i), Path: fmt.Sprintf("acme/r%05d", i), Outcome: report.OutcomeOpened})
	}
	rep.Summarize()
	if err := os.WriteFile(name, []byte("## earlier step output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(redact.New(), name, rep.WriteSummary); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(name)
	if len(data) > report.MaxSummary || !strings.HasPrefix(string(data), "## earlier step output\n") || !strings.Contains(string(data), "cut to fit") {
		t.Errorf("summary of %d bytes, starts %q, ends %q", len(data), string(data[:40]), string(data[len(data)-120:]))
	}
}

// doctor --hub-token trusts a ca_file only as the default branch's hub.yml
// does (origin/HEAD): a checkout under review must not make the client
// trust its own CA for the maintainer's token.
func TestDoctorHubTokenCAFile(t *testing.T) {
	h := newHub(t)
	ca := testCA(t)
	withCA := "version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    url: http://localhost\n    writer: acme-write[bot]\n    ca_file: certs/hub.pem\n"
	h.write("hub.yml", withCA)
	h.write("certs/hub.pem", ca)
	h.write("targets.yml", distTargetsYML)
	h.write("packs/base/AGENTS.md", planAgentsV2)
	h.commit("the hub")
	vars := map[string]string{hubTokenEnv: doctorHubToken}
	res := runDoctorCmd(t, h, vars, exitUsage, "--hub-fp", "localhost/712345678", "--hub-token")
	if !strings.Contains(res.stderr, "does not know the default branch") {
		t.Errorf("no origin/HEAD:\n%s", res.stderr)
	}
	// The default branch trusts no CA: this checkout's is refused.
	h.git("update-ref", "refs/remotes/origin/master", h.git("rev-parse", "HEAD"))
	h.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	h.write("hub.yml", strings.Replace(withCA, "    ca_file: certs/hub.pem\n", "", 1))
	h.commit("no CA on the default branch")
	h.git("update-ref", "refs/remotes/origin/master", h.git("rev-parse", "HEAD"))
	h.write("hub.yml", withCA)
	h.commit("a CA under review")
	res = runDoctorCmd(t, h, vars, exitUsage, "--hub-fp", "localhost/712345678", "--hub-token")
	if !strings.Contains(res.stderr, "differs from the default branch's (origin/HEAD)") {
		t.Errorf("a CA under review:\n%s", res.stderr)
	}
}
