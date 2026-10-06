package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// The plan tests replace the package's driver hooks, so none of them runs in
// parallel; each builds its hub and platforms from scratch.

const (
	planFP       = "github.com/712345678"
	planBranch   = "touchmark/acme-eng"
	ghReadToken  = "gh-read-token-0123456789"
	corpToken    = "corp-read-token-0123456789"
	hubJobToken  = "hub-job-token-0123456789"
	planOptIn    = ".engineering-assets.yml"
	planHubYML   = "version: 1\nid: acme-eng\nprevious_fingerprints: [gitlab.example.com/1234]\nproviders:\n  - id: gh\n    type: github\n    writer: acme-write[bot]\n    known_authors: [\"acme-old[bot]\"]\n  - id: corp\n    type: gitlab\n    url: https://gitlab.example.com\n    writer: tm-writer\n"
	planTargets  = "version: 1\ndefaults:\n  provider: gh\n  packs: [base]\ntargets:\n  - org: acme\n  - org: acme\n    topics: [python]\n    packs: [python]\n  - repo: corp:platform/api\nexclude:\n  - acme/legacy\n"
	planStaleKey = "sha256:0f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a4938"
)

// The pack files of the plan hub.
var (
	planAgentsV1 = text("base AGENTS.md v1")
	planAgentsV2 = text("base AGENTS.md v2")
	planGuide    = text("base guide v1")
	planPython   = text("python guidelines v1")
)

// planHub commits a hub with two providers and the packs base (AGENTS.md
// in two versions, docs/guide.md) and python (docs/python.md).
func planHub(t *testing.T) *repo {
	t.Helper()
	h := newHub(t)
	h.write("hub.yml", planHubYML)
	h.write("targets.yml", planTargets)
	h.write("packs/base/AGENTS.md", planAgentsV1)
	h.write("packs/base/docs/guide.md", planGuide)
	h.commit("base v1")
	h.write("packs/base/AGENTS.md", planAgentsV2)
	h.write("packs/python/docs/python.md", planPython)
	h.commit("base v2, python")
	return h
}

// planWorld is the fake platforms of the hub: GitHub with the acme
// organisation and a GitLab instance.
type planWorld struct {
	t        *testing.T
	gh, corp *fake.Platform
	// accounts by read token.
	tokens map[string]platform.Account
	writer platform.Account
	person platform.Account
	// built records every driver plan built, in order.
	built []builtDriver
}

// builtDriver is what a driver was built with.
type builtDriver struct {
	id, url, api, writer string
	cred                 auth.Credential
}

// newPlanWorld builds a target for every outcome plan reaches.
func newPlanWorld(t *testing.T) *planWorld {
	t.Helper()
	gh := fake.New("github.com")
	corp := fake.New("gitlab.example.com", fake.WithFlavor(fake.GitLab))
	w := &planWorld{t: t, gh: gh, corp: corp}
	w.tokens = map[string]platform.Account{
		ghReadToken: gh.AddAccount("acme-read[bot]", platform.KindBot),
		corpToken:   corp.AddAccount("tm-reader", platform.KindServiceAccount),
	}
	w.writer = gh.AddAccount("acme-write[bot]", platform.KindBot)
	gh.AddAccount("acme-old[bot]", platform.KindBot)
	w.person = gh.AddAccount("jdoe", platform.KindUser)
	corp.AddAccount("tm-writer", platform.KindServiceAccount)

	key := planKey()
	w.repo("acme/billing", nil, planOptIn, "version: 1\n")
	api := w.repo("acme/api", nil, planOptIn, "version: 1\n")
	w.pr(api, platform.PR{Head: planBranch, Author: w.writer, Title: "chore: sync engineering assets", Body: planBody(t, key)})
	sdk := w.repo("acme/sdk", func(r *platform.Repo) { r.Topics = []string{"python"} }, planOptIn, "version: 1\n")
	w.pr(sdk, platform.PR{Head: planBranch, Author: w.writer, Title: "chore: sync engineering assets", Body: planBody(t, planStaleKey)})
	old := w.repo("acme/old", nil, planOptIn, "version: 1\n", "AGENTS.md", planAgentsV2, "docs/guide.md", planGuide)
	w.pr(old, platform.PR{Head: planBranch, Author: w.writer, Title: "chore: sync engineering assets", Body: planBody(t, key)})
	w.repo("acme/web", nil, "README.md", "not opted in")
	w.repo("acme/archive", func(r *platform.Repo) { r.Archived = true }, planOptIn, "version: 1\n")
	cli := w.repo("acme/cli", nil, planOptIn, "version: 1\n")
	w.pr(cli, platform.PR{Head: planBranch, Author: w.person, Title: "my own work on the sync branch"})
	docs := w.repo("acme/docs", nil, planOptIn, "version: 1\n")
	n := w.pr(docs, platform.PR{Head: planBranch, Author: w.writer, Title: "chore: sync engineering assets", Body: planBody(t, key)})
	gh.SetPRState(docs.ID, n, platform.Closed, &w.person, fake.Epoch)
	w.repo("acme/secret", func(r *platform.Repo) { r.Visibility = "private" }, planOptIn, "version: 1\n")
	w.repo("acme/legacy", nil, planOptIn, "version: 1\n")
	w.repo("acme/tools", nil, planOptIn, "version: 1\n", "docs/python.md", planPython)
	r := corp.AddRepo(platform.Repo{Path: "platform/api"})
	corp.SetFile(r.ID, planOptIn, []byte("version: 1\npacks: [python]\n"), "")
	w.ok()
	return w
}

func (w *planWorld) ok() {
	w.t.Helper()
	for _, p := range []*fake.Platform{w.gh, w.corp} {
		if err := p.Err(); err != nil {
			w.t.Fatalf("setup: %v", err)
		}
	}
}

// repo adds a GitHub repository with files ("path", "content" pairs).
func (w *planWorld) repo(path string, edit func(*platform.Repo), files ...string) platform.Repo {
	w.t.Helper()
	r := platform.Repo{Path: path}
	if edit != nil {
		edit(&r)
	}
	r = w.gh.AddRepo(r)
	for i := 0; i+1 < len(files); i += 2 {
		w.gh.SetFile(r.ID, files[i], []byte(files[i+1]), "")
	}
	w.ok()
	return r
}

func (w *planWorld) pr(r platform.Repo, pr platform.PR) int64 {
	w.t.Helper()
	n := w.gh.AddPR(r.ID, pr)
	w.ok()
	return n
}

// planKey is the content key of a target of acme that holds none of the
// files of base.
func planKey() string {
	return decide.Key(decide.StreamSync, []decide.Pair{
		{Path: "AGENTS.md", From: decide.ZeroOID, Mode: "100644", To: gitx.RawOID([]byte(planAgentsV2))},
		{Path: "docs/guide.md", From: decide.ZeroOID, Mode: "100644", To: gitx.RawOID([]byte(planGuide))},
	})
}

// planBody is a pull request body with the hub's marker over key.
func planBody(t *testing.T, key string) string {
	t.Helper()
	line, err := marker.Encode(marker.Marker{Key: key, Data: marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: planFP, Engine: "0.2.0",
		Packs: []string{"base"}, TitleSet: "chore: sync engineering assets",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return "Engineering assets from the hub.\n\n" + line
}

// install replaces the driver hooks with the world's platforms until the
// test ends: a reader acts as the account of its token. A host without a
// platform gets an empty one, where every call fails.
func (w *planWorld) install() {
	w.t.Helper()
	drivers, snapshots := planDrivers, planSnapshots
	w.t.Cleanup(func() { planDrivers, planSnapshots = drivers, snapshots })
	platforms := map[string]*fake.Platform{"github.com": w.gh, "gitlab.example.com": w.corp}
	driver := func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
		w.built = append(w.built, builtDriver{id: rp.ID, url: rp.URL, api: rp.APIURL, writer: rp.Writer, cred: c})
		if client == nil {
			return nil, errors.New("no HTTP client")
		}
		p := platforms[rp.Host]
		if p == nil {
			p = fake.New(rp.Host)
		}
		return p.Reader(w.tokens[c.Token]), nil
	}
	planDrivers = map[string]planDriver{"github": driver, "gitlab": driver}
	planSnapshots = hostSources{"github.com": w.gh.Snapshots(), "gitlab.example.com": w.corp.Snapshots()}
}

// hostSources takes snapshots on the platform of the repository's host.
type hostSources map[string]snapshot.Source

func (s hostSources) Snapshot(ctx context.Context, r platform.Repo, remote platform.Remote, ref string) (*snapshot.Tree, error) {
	src := s[r.Host]
	if src == nil {
		return nil, fmt.Errorf("no platform for %s", r.Host)
	}
	return src.Snapshot(ctx, r, remote, ref)
}

// actionsEnv is the environment of a GitHub Actions job for pull request
// #41 of a public hub, with the read tokens of both providers.
func actionsEnv(t *testing.T) map[string]string {
	t.Helper()
	event := filepath.Join(t.TempDir(), "event.json")
	payload := `{"repository": {"id": 712345678, "visibility": "public", "default_branch": "master"}}`
	if err := os.WriteFile(event, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"CI":                        "true",
		"GITHUB_ACTIONS":            "true",
		"GITHUB_SERVER_URL":         "https://github.com",
		"GITHUB_API_URL":            "https://api.github.com",
		"GITHUB_REPOSITORY_ID":      "712345678",
		"GITHUB_REPOSITORY":         "acme/engineering-assets",
		"GITHUB_EVENT_NAME":         "pull_request",
		"GITHUB_EVENT_PATH":         event,
		"GITHUB_REF":                "refs/pull/41/merge",
		"GITHUB_REF_NAME":           "41/merge",
		"GITHUB_REF_TYPE":           "branch",
		"GITHUB_TOKEN":              hubJobToken,
		"TOUCHMARK_GH_READ_TOKEN":   ghReadToken,
		"TOUCHMARK_CORP_READ_TOKEN": corpToken,
	}
}

// localEnv is a developer's shell with the read tokens.
func localEnv() map[string]string {
	return map[string]string{
		"TOUCHMARK_GH_READ_TOKEN":   ghReadToken,
		"TOUCHMARK_CORP_READ_TOKEN": corpToken,
	}
}

// runWith runs the command line in process with the environment vars, and
// nothing else, as the variables touchmark reads.
func runWith(t *testing.T, vars map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	environ := func() []string {
		var out []string
		for k, v := range vars {
			out = append(out, k+"="+v)
		}
		return out
	}
	e := &env{stdout: &stdout, stderr: &stderr, getenv: func(k string) string { return vars[k] }, environ: environ, getwd: os.Getwd}
	code := e.main(t.Context(), args)
	res := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	checkJSONReport(t, args, res)
	return res
}

// checkJSONReport validates the report a run of plan, distribute or doctor
// printed with --format json against its schema; a run that ended before
// its report (exit 2) prints none.
func checkJSONReport(t *testing.T, args []string, res result) {
	t.Helper()
	if len(args) == 0 || res.code == exitUsage || strings.TrimSpace(res.stdout) == "" {
		return
	}
	json := false
	for i, a := range args {
		if a == "--format=json" || (a == "--format" && i+1 < len(args) && args[i+1] == "json") {
			json = true
		}
	}
	if !json {
		return
	}
	switch args[0] {
	case "plan", "distribute":
		validateSchema(t, "report", []byte(res.stdout))
	case "doctor":
		validateSchema(t, "doctor", []byte(res.stdout))
	case "setup":
		validateSchema(t, "setup", []byte(res.stdout))
	}
}

// planRun runs plan against the hub and fails the test unless it exits
// with code.
func planRun(t *testing.T, h *repo, vars map[string]string, code int, extra ...string) result {
	t.Helper()
	inJob(t, vars)
	args := append([]string{"plan", "--hub", h.dir}, extra...)
	res := runWith(t, vars, args...)
	if res.code != code {
		t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, code, res.stdout, res.stderr)
	}
	return res
}

func decodeDelivery(t *testing.T, out string) report.Delivery {
	t.Helper()
	var rep report.Delivery
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	return rep
}

// plan on the fake prints a plan. A hub pull request in GitHub Actions of a
// public hub, two providers, a target for every outcome plan reaches.
func TestPlanOnFake(t *testing.T) {
	h := planHub(t)
	// The pull request is the hub's second commit: master is at the first,
	// and both packs change, which every target gets.
	h.git("update-ref", "refs/remotes/origin/master", h.git("rev-parse", "HEAD~1"))
	w := newPlanWorld(t)
	w.install()
	vars := actionsEnv(t)
	s := newScenario(t, "plan-basic", h, nil)

	res := planRun(t, h, vars, exitOK)
	s.golden("plan.txt", s.normalize(res.stdout))
	masks := res.stderr

	res = planRun(t, h, vars, exitOK, "--format", "json")
	s.golden("plan.json", s.normalize(res.stdout))
	rep := decodeDelivery(t, res.stdout)

	res = planRun(t, h, vars, exitOK, "--format", "markdown")
	s.golden("plan.md", s.normalize(res.stdout))

	// The outcomes.
	got := map[string]string{}
	for _, tg := range rep.Targets {
		got[tg.Provider+":"+tg.Path] = string(tg.Outcome) + ":" + tg.Reason
	}
	want := map[string]string{
		"gh:acme/billing":   "opened:",
		"gh:acme/api":       "unchanged:",
		"gh:acme/sdk":       "updated:content",
		"gh:acme/old":       "closed:no-diff",
		"gh:acme/web":       "skipped:not-opted-in",
		"gh:acme/archive":   "skipped:archived",
		"gh:acme/cli":       "blocked:branch-in-use",
		"gh:acme/docs":      "opened:", // declined where the plan reads memory; this snapshot-only one does not
		"gh:":               "skipped:private-in-public-hub",
		"gh:acme/tools":     "opened:",
		"corp:platform/api": "opened:",
	}
	for ref, outcome := range want {
		if got[ref] != outcome {
			t.Errorf("%s: %q, want %q", ref, got[ref], outcome)
		}
	}
	if len(rep.Targets) != len(want) {
		t.Errorf("%d targets, want %d: %v", len(rep.Targets), len(want), got)
	}
	if rep.Hub.Fingerprint != planFP || rep.Hub.PR != 41 || rep.Command != "plan" {
		t.Errorf("hub %+v, command %s", rep.Hub, rep.Command)
	}
	// Nothing names the private target, and nothing was written.
	for _, out := range []string{res.stdout, s.normalize(res.stdout)} {
		if strings.Contains(out, "acme/secret") {
			t.Error("the output names a private target of a public hub")
		}
	}
	if writes := slices.Concat(w.gh.Writes(), w.corp.Writes()); len(writes) > 0 {
		t.Errorf("plan wrote: %q", writes)
	}
	// GitHub Actions masks every form of the tokens; the tokens never reach
	// stdout.
	for _, token := range []string{ghReadToken, corpToken, hubJobToken} {
		if !strings.Contains(masks, "::add-mask::"+token+"\n") {
			t.Errorf("stderr lacks the mask of %s:\n%s", token, masks)
		}
	}
	// Besides the masks, stderr holds the annotations of GitHub Actions for
	// the blocked and failed targets, the same as distribute's.
	var notes []string
	for _, line := range strings.Split(strings.TrimSpace(masks), "\n") {
		switch {
		case strings.HasPrefix(line, "::add-mask::"):
		case strings.HasPrefix(line, "::warning ") || strings.HasPrefix(line, "::error "):
			notes = append(notes, line)
		default:
			t.Errorf("stderr: %q", line)
		}
	}
	if want := []string{"::warning title=touchmark::gh:acme/cli blocked:branch-in-use"}; !slices.Equal(notes, want) {
		t.Errorf("annotations %q, want %q", notes, want)
	}
	// The job's report files, for its artifact, and the step summary when
	// the job has one.
	for _, name := range []string{"touchmark-report.json", "touchmark-report.md"} {
		data, err := os.ReadFile(name)
		switch {
		case err != nil:
			t.Errorf("the report file of the job: %v", err)
		case strings.Contains(string(data), "acme/secret") || strings.Contains(string(data), ghReadToken):
			t.Errorf("%s names the private target or holds a token", name)
		case name == "touchmark-report.json":
			validateSchema(t, "report", data)
		}
	}

	// --strict: a blocked target exits 3, with the same report.
	res = planRun(t, h, vars, exitStrict, "--strict", "--format", "json")
	if rep := decodeDelivery(t, res.stdout); !rep.Strict || rep.Summary[report.OutcomeBlocked] != 1 {
		t.Errorf("--strict report: strict %v, summary %v", rep.Strict, rep.Summary)
	}

	// The step summary of the job: the plan in Markdown, with its scope,
	// without the private target.
	summary := filepath.Join(t.TempDir(), "summary.md")
	vars["GITHUB_STEP_SUMMARY"] = summary
	planRun(t, h, vars, exitOK)
	data, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"### touchmark plan", "**Scope:**", "acme/billing"} {
		if !strings.Contains(string(data), s) {
			t.Errorf("the step summary lacks %q:\n%s", s, data)
		}
	}
	if strings.Contains(string(data), "acme/secret") {
		t.Error("the step summary names the private target")
	}
}

// exitStrict is the exit code of plan --strict with something blocked.
const exitStrict = 3

// A run outside CI needs --hub-fp; with it, the plan names every target,
// since the hub's visibility is unknown.
func TestPlanLocal(t *testing.T) {
	h := planHub(t)
	w := newPlanWorld(t)
	w.install()
	res := planRun(t, h, localEnv(), exitUsage)
	if !strings.Contains(res.stderr, "the hub's fingerprint is unknown") || !strings.Contains(res.stderr, "--hub-fp HOST/ID") {
		t.Errorf("stderr: %s", res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("stdout: %s", res.stdout)
	}

	s := newScenario(t, "plan-local", h, nil)
	res = planRun(t, h, localEnv(), exitOK, "--hub-fp", planFP)
	s.golden("plan.txt", s.normalize(res.stdout))

	res = planRun(t, h, localEnv(), exitOK, "--hub-fp", "GitHub.com/712345678", "--format", "json")
	rep := decodeDelivery(t, res.stdout)
	if rep.Hub.Fingerprint != planFP || rep.Hub.PR != 0 {
		t.Errorf("hub %+v", rep.Hub)
	}
	if !slices.Contains(rep.Warnings, "hub head not checked: a local run cannot read the tip of the hub's default branch") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	if res.stderr != "" {
		t.Errorf("stderr: %s", res.stderr)
	}
	named := false
	for _, tg := range rep.Targets {
		named = named || tg.Path == "acme/secret"
	}
	if !named {
		t.Error("a local run hides a private target")
	}

	// --only restricts the targets; the sweep is off.
	res = planRun(t, h, localEnv(), exitOK, "--hub-fp", planFP, "--only", "gh:acme/api,corp:platform/api", "--only", "acme/cli", "--format", "json")
	rep = decodeDelivery(t, res.stdout)
	var refs []string
	for _, tg := range rep.Targets {
		refs = append(refs, tg.Provider+":"+tg.Path)
	}
	if !slices.Equal(refs, []string{"corp:platform/api", "gh:acme/api", "gh:acme/cli"}) || rep.Sweep.Reason == "" {
		t.Errorf("targets %q, sweep %+v", refs, rep.Sweep)
	}
}

// The head guard on a run of the hub's default branch: when the hub
// channel says the branch has moved on, plan is superseded and reads no
// target.
func TestPlanSuperseded(t *testing.T) {
	h := planHub(t)
	w := newPlanWorld(t)
	w.install()
	var mu sync.Mutex
	tip, sent := strings.Repeat("a", 40), ""
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sent = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/acme/engineering-assets/git/ref/heads/master" {
			http.NotFound(rw, r)
			return
		}
		fmt.Fprintf(rw, `{"ref": "refs/heads/master", "object": {"sha": %q, "type": "commit"}}`, tip)
	}))
	defer srv.Close()
	vars := actionsEnv(t)
	vars["GITHUB_EVENT_NAME"] = "push"
	vars["GITHUB_REF"] = "refs/heads/master"
	vars["GITHUB_REF_NAME"] = "master"
	vars["GITHUB_API_URL"] = srv.URL

	res := planRun(t, h, vars, exitOK, "--strict")
	if !strings.Contains(res.stdout, "superseded: the hub's default branch has moved on") ||
		!strings.Contains(res.stdout, "the tip of the hub's default branch is aaaaaaaaaaaa") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	mu.Lock()
	if sent != "Bearer "+hubJobToken {
		t.Errorf("the hub channel sent %q", sent)
	}
	tip = h.git("rev-parse", "HEAD")
	mu.Unlock()
	if calls := slices.Concat(w.gh.Calls(), w.corp.Calls()); len(calls) > 0 {
		t.Errorf("a superseded plan called the platforms: %q", calls)
	}

	// At the tip, the plan runs: a blocked target makes --strict exit 3.
	res = planRun(t, h, vars, exitStrict, "--strict", "--format", "json")
	rep := decodeDelivery(t, res.stdout)
	if rep.Outcome != report.Completed || len(rep.Targets) == 0 || len(rep.Warnings) != 0 {
		t.Errorf("outcome %s, %d targets, warnings %q", rep.Outcome, len(rep.Targets), rep.Warnings)
	}

	// A channel that fails leaves a warning.
	vars["GITHUB_REPOSITORY"] = "not a path"
	rep = decodeDelivery(t, planRun(t, h, vars, exitOK, "--format", "json").stdout)
	if len(rep.Warnings) != 1 || !strings.HasPrefix(rep.Warnings[0], "hub head not checked: hub channel: invalid repository path") {
		t.Errorf("warnings %q", rep.Warnings)
	}
}

// Every refusal exits 2 before anything is read from a platform.
func TestPlanRefusals(t *testing.T) {
	h := planHub(t)
	fp := []string{"--hub-fp", planFP}
	// A git plan supports, whatever the machine has: the refusals below
	// come before any target is read, and TestPlanGitVersion has git's.
	gv := gitVersion
	t.Cleanup(func() { gitVersion = gv })
	gitVersion = func(context.Context) ([3]int, error) { return gitx.DeliveryMinVersion, nil }

	// A provider type without a driver in the build.
	drivers := planDrivers
	t.Cleanup(func() { planDrivers = drivers })
	planDrivers = map[string]planDriver{}
	res := planRun(t, h, localEnv(), exitUsage, fp...)
	if !strings.Contains(res.stderr, "provider gh: no github driver in this build") {
		t.Errorf("stderr: %s", res.stderr)
	}
	planDrivers = drivers
	res = runWith(t, nil, "plan", "--help")
	for _, flag := range []string{"--hub DIR", "--only REF", "--hub-fp HOST/ID", "--strict", "--format FORMAT", "text, json or markdown"} {
		if res.code != exitOK || !strings.Contains(res.stdout, flag) {
			t.Errorf("plan --help: exit %d, lacks %q:\n%s", res.code, flag, res.stdout)
		}
	}

	w := newPlanWorld(t)
	w.install()
	cases := []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"no credential", map[string]string{"TOUCHMARK_GH_READ_TOKEN": ghReadToken}, fp,
			"provider corp: no read credential: set TOUCHMARK_CORP_READ_TOKEN, or TOUCHMARK_CORP_READ_APP_ID and TOUCHMARK_CORP_READ_APP_KEY"},
		{"bad credential", map[string]string{"TOUCHMARK_GH_READ_TOKEN": "has space", "TOUCHMARK_CORP_READ_TOKEN": corpToken}, fp,
			"provider gh: TOUCHMARK_GH_READ_TOKEN holds a space"},
		{"write credential in CI", func() map[string]string {
			v := actionsEnv(t)
			v["TOUCHMARK_CORP_WRITE_TOKEN"] = "corp-write-token-0123456789"
			return v
		}(), nil, "TOUCHMARK_CORP_WRITE_TOKEN is set: plan reads with the read credential only"},
		{"short write credential in CI", func() map[string]string {
			v := actionsEnv(t)
			v["TOUCHMARK_WRITE_APP_ID"] = "12345"
			return v
		}(), nil, "TOUCHMARK_WRITE_APP_ID is set"},
		// The guard does not trust the provider list: a stale or renamed
		// provider's write credential, or the implicit provider's, counts.
		{"undeclared provider's write credential in CI", func() map[string]string {
			v := actionsEnv(t)
			v["TOUCHMARK_GITHUB_WRITE_TOKEN"] = "gh-write-token-0123456789"
			return v
		}(), nil, "TOUCHMARK_GITHUB_WRITE_TOKEN is set: plan reads with the read credential only"},
		{"signing key in CI", func() map[string]string {
			v := actionsEnv(t)
			v["TOUCHMARK_GH_SIGNING_KEY"] = "-----BEGIN OPENSSH PRIVATE KEY-----"
			v["TOUCHMARK_OLD_ID_WRITE_APP_KEY"] = "-----BEGIN RSA PRIVATE KEY-----"
			return v
		}(), nil, "TOUCHMARK_GH_SIGNING_KEY, TOUCHMARK_OLD_ID_WRITE_APP_KEY are set"},
		{"write credential under another CI", func() map[string]string {
			v := localEnv()
			v["CI"] = "woodpecker"
			v["TOUCHMARK_CORP_WRITE_TOKEN"] = "corp-write-token-0123456789"
			return v
		}(), fp, "TOUCHMARK_CORP_WRITE_TOKEN is set"},
		{"bad format", localEnv(), append(fp, "--format", "yaml"), `--format: "yaml" is not text, json or markdown`},
		{"bad fingerprint", localEnv(), []string{"--hub-fp", "github.com"}, `"github.com" is not HOST/ID`},
		{"bad only", localEnv(), append(fp, "--only", "acme"), "invalid value \"acme\" for flag -only"},
		{"extra argument", localEnv(), append(fp, "acme/api"), `unexpected argument "acme/api"`},
	}
	for _, tc := range cases {
		res := planRun(t, h, tc.vars, exitUsage, tc.args...)
		if !strings.Contains(res.stderr, tc.want) || res.stdout != "" {
			t.Errorf("%s: stdout %q, stderr %q; want %q", tc.name, res.stdout, res.stderr, tc.want)
		}
	}
	if calls := slices.Concat(w.gh.Calls(), w.corp.Calls()); len(calls) > 0 {
		t.Errorf("a refused plan called the platform: %q", calls)
	}

	// A hub that fails check, or has no id.
	bad := planHub(t)
	bad.write("targets.yml", "version: 1\ntargets:\n  - repo: acme/x\n    packs: [nope]\n")
	bad.commit("unknown pack")
	res = planRun(t, bad, localEnv(), exitUsage, fp...)
	if !strings.Contains(res.stderr, "the hub fails touchmark check") || !strings.Contains(res.stderr, `unknown pack "nope"`) {
		t.Errorf("stderr: %s", res.stderr)
	}
	legacy := newHub(t)
	legacy.write("packs/base/AGENTS.md", planAgentsV1)
	legacy.commit("a legacy hub")
	res = planRun(t, legacy, localEnv(), exitUsage, fp...)
	if !strings.Contains(res.stderr, "hub.yml has no id") {
		t.Errorf("stderr: %s", res.stderr)
	}
}

// In a hub pull request, hub.yml has not been reviewed: its providers and
// ca_file come from the default branch, and a provider it adds or moves is
// planned without a credential.
func TestPlanHubPullRequestProviders(t *testing.T) {
	type driverWant struct {
		url, writer string
		anonymous   bool
	}
	ca := testCA(t)
	withCA := strings.Replace(planHubYML, "    writer: tm-writer\n", "    writer: tm-writer\n    ca_file: certs/corp.pem\n", 1)
	cases := []struct {
		name string
		// base prepares the default branch; branch the pull request.
		base, branch func(h *repo)
		hubYML       string // on the pull request's branch; "" keeps the default branch's
		vars         func(map[string]string)
		drivers      map[string]driverWant
		warnings     []string
	}{
		{
			name:   "moved api",
			hubYML: strings.Replace(planHubYML, "url: https://gitlab.example.com", "url: https://collector.example", 1),
			drivers: map[string]driverWant{
				"gh":   {url: "https://github.com", writer: "acme-write[bot]"},
				"corp": {url: "https://collector.example", writer: "tm-writer", anonymous: true},
			},
			warnings: []string{"provider corp: this run's type, url, api_url or ca_file differs from the default branch's: planned without credentials"},
		},
		{
			name:   "new ca_file",
			branch: func(h *repo) { h.write("certs/corp.pem", ca) },
			hubYML: withCA,
			drivers: map[string]driverWant{
				"gh":   {url: "https://github.com", writer: "acme-write[bot]"},
				"corp": {url: "https://gitlab.example.com", writer: "tm-writer", anonymous: true},
			},
			warnings: []string{"provider corp: this run's type, url, api_url or ca_file differs"},
		},
		{
			// The CA bundle comes from the default branch too: the pull
			// request's (not even a certificate here) is never read.
			name: "changed ca_file content",
			base: func(h *repo) {
				h.write("hub.yml", withCA)
				h.write("certs/corp.pem", ca)
				h.commit("trust the corp CA")
			},
			branch: func(h *repo) { h.write("certs/corp.pem", "not a certificate\n") },
			drivers: map[string]driverWant{
				"gh":   {url: "https://github.com", writer: "acme-write[bot]"},
				"corp": {url: "https://gitlab.example.com", writer: "tm-writer"},
			},
		},
		{
			name:   "new provider without a credential",
			hubYML: planHubYML + "  - id: extra\n    type: gitlab\n    url: https://gitlab.extra.example\n",
			drivers: map[string]driverWant{
				"gh":    {url: "https://github.com", writer: "acme-write[bot]"},
				"corp":  {url: "https://gitlab.example.com", writer: "tm-writer"},
				"extra": {url: "https://gitlab.extra.example", anonymous: true},
			},
			warnings: []string{"provider extra is not in the default branch's hub.yml: planned without credentials until it is merged"},
		},
		{
			name:   "changed writer",
			hubYML: strings.Replace(planHubYML, "writer: acme-write[bot]", "writer: jdoe", 1),
			drivers: map[string]driverWant{
				"gh":   {url: "https://github.com", writer: "acme-write[bot]"},
				"corp": {url: "https://gitlab.example.com", writer: "tm-writer"},
			},
			warnings: []string{"provider gh: this run changes its settings; plan uses the default branch's until it is merged"},
		},
		{
			// A new hub's first pull request names the writer, which the
			// template's default branch leaves out: plan takes it, so that
			// it recognizes touchmark's pull requests and sweeps.
			name: "added writer",
			base: func(h *repo) {
				h.write("hub.yml", strings.Replace(planHubYML, "    writer: tm-writer\n", "", 1))
				h.commit("no writer for corp yet")
			},
			hubYML: planHubYML,
			drivers: map[string]driverWant{
				"gh":   {url: "https://github.com", writer: "acme-write[bot]"},
				"corp": {url: "https://gitlab.example.com", writer: "tm-writer"},
			},
			warnings: []string{"provider corp: the default branch's hub.yml names no writer: plan takes this run's, tm-writer"},
		},
		{
			name: "default branch not in the clone",
			vars: func(v map[string]string) {
				event := v["GITHUB_EVENT_PATH"]
				if err := os.WriteFile(event, []byte(`{"repository": {"id": 712345678, "visibility": "public", "default_branch": "trunk"}}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			drivers: map[string]driverWant{
				"gh":   {url: "https://github.com", writer: "acme-write[bot]", anonymous: true},
				"corp": {url: "https://gitlab.example.com", writer: "tm-writer", anonymous: true},
			},
			warnings: []string{"every provider is planned without credentials", "the default branch trunk is not in this clone"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := planHub(t)
			if tc.base != nil {
				tc.base(h)
			}
			h.git("checkout", "-q", "-b", "feature")
			if tc.branch != nil {
				tc.branch(h)
			}
			if tc.hubYML != "" {
				h.write("hub.yml", tc.hubYML)
			}
			if tc.branch != nil || tc.hubYML != "" {
				h.commit("the pull request")
			}
			w := newPlanWorld(t)
			w.install()
			vars := actionsEnv(t)
			if tc.vars != nil {
				tc.vars(vars)
			}
			res := planRun(t, h, vars, exitOK, "--format", "json")
			rep := decodeDelivery(t, res.stdout)
			got := map[string]driverWant{}
			for _, b := range w.built {
				got[b.id] = driverWant{url: b.url, writer: b.writer, anonymous: b.cred.Kind == 0}
				if b.cred.Kind == 0 && (b.cred.Token != "" || b.cred.AppKey != nil) {
					t.Errorf("%s: a credential without a kind: %+v", b.id, b.cred)
				}
			}
			if !reflect.DeepEqual(got, tc.drivers) {
				t.Errorf("drivers:\n got  %+v\n want %+v", got, tc.drivers)
			}
			for _, want := range tc.warnings {
				if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
					t.Errorf("warnings %q lack %q", rep.Warnings, want)
				}
			}
			for _, p := range rep.Providers {
				if want := tc.drivers[p.ID]; want.anonymous && (p.Error != "" || p.ResolveComplete) {
					t.Errorf("anonymous provider %+v: want no error and an incomplete resolve", p)
				}
			}
			if strings.Contains(res.stdout, corpToken) || strings.Contains(res.stdout, ghReadToken) {
				t.Error("a token reached the output")
			}
		})
	}
}

// testCA returns a PEM certificate, as a hub's ca_file holds.
func testCA(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// A run of the default branch reads its own hub.yml with the credentials.
func TestPlanDefaultBranchProviders(t *testing.T) {
	h := planHub(t)
	w := newPlanWorld(t)
	w.install()
	vars := actionsEnv(t)
	vars["GITHUB_EVENT_NAME"] = "schedule"
	vars["GITHUB_REF"] = "refs/heads/master"
	vars["GITHUB_REF_NAME"] = "master"
	delete(vars, "GITHUB_TOKEN") // no channel: the head is not checked
	planRun(t, h, vars, exitOK, "--format", "json")
	for _, b := range w.built {
		if b.cred.Kind == 0 {
			t.Errorf("%s: no credential on the default branch", b.id)
		}
	}
}

func TestUntrustedRun(t *testing.T) {
	for _, tc := range []struct {
		ctx  hubch.Context
		want bool
	}{
		{hubch.Context{CI: hubch.Local}, false},
		{hubch.Context{}, false},
		{hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "push"}, false},
		{hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "41/merge", Event: "pull_request"}, true},
		// A pull_request_target run builds the default branch, but for a
		// pull request: it counts as one.
		{hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "pull_request_target"}, true},
		{hubch.Context{CI: hubch.GitLabCI, DefaultBranch: "main", RefName: "feature", RefIsBranch: true, Event: "merge_request_event"}, true},
		{hubch.Context{CI: hubch.GitLabCI, DefaultBranch: "main", RefName: "feature", RefIsBranch: true, Event: "push"}, true},
		{hubch.Context{CI: hubch.ForgejoActions, DefaultBranch: "main", RefName: "v1.0", Event: "push"}, true},
	} {
		if got := untrustedRun(tc.ctx); got != tc.want {
			t.Errorf("untrustedRun(%+v) = %v, want %v", tc.ctx, got, tc.want)
		}
	}
}

// A read variable is removed from the process environment once plan read
// it, so no child process inherits it.
func TestPlanUnsetsReadVariables(t *testing.T) {
	h := planHub(t)
	w := newPlanWorld(t)
	w.install()
	for _, name := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "GITEA_ACTIONS", "FORGEJO_ACTIONS"} {
		t.Setenv(name, "")
	}
	t.Setenv("TOUCHMARK_GH_READ_TOKEN", ghReadToken)
	t.Setenv("TOUCHMARK_CORP_READ_TOKEN", corpToken)
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr, getenv: os.Getenv, environ: os.Environ, getwd: os.Getwd}
	if code := e.main(t.Context(), []string{"plan", "--hub", h.dir, "--hub-fp", planFP}); code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	for _, name := range []string{"TOUCHMARK_GH_READ_TOKEN", "TOUCHMARK_CORP_READ_TOKEN"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("%s is still in the environment", name)
		}
	}
	if len(w.built) != 2 || w.built[0].cred.Token != ghReadToken || w.built[1].cred.Token != corpToken {
		t.Errorf("drivers %+v", w.built)
	}
}

// The README's hub.yml names only the writer: locally, the provider comes
// from the hub's origin remote, with the short variable
// names.
func TestPlanShorthandFromOrigin(t *testing.T) {
	h := newHub(t)
	h.write("hub.yml", "version: 1\nid: acme-eng\nwriter: acme-write[bot]\n")
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\nexclude: [acme/legacy]\n")
	h.write("packs/base/AGENTS.md", planAgentsV2)
	h.write("packs/base/docs/guide.md", planGuide)
	h.commit("a hub on the shorthand")
	h.git("remote", "add", "origin", "https://github.com/acme/engineering-assets.git")
	w := newPlanWorld(t)
	w.install()
	res := planRun(t, h, map[string]string{"TOUCHMARK_READ_TOKEN": ghReadToken}, exitOK, "--hub-fp", planFP, "--format", "json")
	rep := decodeDelivery(t, res.stdout)
	if len(rep.Providers) != 1 || rep.Providers[0].ID != "github" || rep.Providers[0].Host != "github.com" || rep.Providers[0].Writer != "acme-write[bot]" {
		t.Errorf("providers %+v", rep.Providers)
	}
	if len(w.built) != 1 || w.built[0].cred.Token != ghReadToken {
		t.Errorf("drivers %+v", w.built)
	}
	opened := 0
	for _, tg := range rep.Targets {
		if tg.Provider != "github" {
			t.Errorf("target %s:%s", tg.Provider, tg.Path)
		}
		if tg.Outcome == report.OutcomeOpened {
			opened++
		}
	}
	if opened == 0 {
		t.Errorf("no target opened: %+v", rep.Targets)
	}

	// A self-managed origin cannot tell the type: hub.yml needs providers.
	h.git("remote", "set-url", "origin", "https://git.example.com/acme/engineering-assets.git")
	res = planRun(t, h, map[string]string{"TOUCHMARK_READ_TOKEN": ghReadToken}, exitUsage, "--hub-fp", planFP)
	if !strings.Contains(res.stderr, "add providers to hub.yml") || strings.Contains(res.stderr, "git.example.com") {
		t.Errorf("stderr: %s", res.stderr)
	}
}

// A platform error that quotes a credential is masked in every output.
func TestPlanMasksSecrets(t *testing.T) {
	h := planHub(t)
	w := newPlanWorld(t)
	w.install()
	leak := &platform.Error{Op: "read file", Class: platform.ClassAuth, Status: 401,
		Err: errors.New("token " + ghReadToken + " (base64 " + basic("x-access-token", ghReadToken) + ") is revoked")}
	w.gh.Fail("ReadFile", leak)
	// The Markdown escapes of the token's '-' must not let it through.
	escaped := strings.ReplaceAll(ghReadToken, "-", `\-`)
	for _, format := range []string{formatText, formatJSON, formatMarkdown} {
		res := planRun(t, h, localEnv(), exitFailed, "--hub-fp", planFP, "--format", format)
		for _, form := range []string{ghReadToken, escaped, basic("x-access-token", ghReadToken), "token-0123456789"} {
			if strings.Contains(res.stdout, form) || strings.Contains(res.stderr, form) {
				t.Errorf("%s: %q leaked:\n%s", format, form, res.stdout)
			}
		}
		if !strings.Contains(res.stdout, "token *** (base64 ***) is revoked") &&
			!strings.Contains(res.stdout, `token \*\*\* \(base64 \*\*\*\) is revoked`) {
			t.Errorf("%s: no masked failure in:\n%s", format, res.stdout)
		}
	}
}

// basic is the base64 of "user:secret", as in an HTTP Basic header.
func basic(user, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
}
