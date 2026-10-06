package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The distribute tests replace the package's driver hooks, so none of them
// runs in parallel. Their platform is the fake in git mode on a loopback
// host, which the hub names with an http URL (a local instance).

const (
	distFP          = "github.com/712345678"
	distBranch      = "touchmark/acme-eng"
	distWriteToken  = "dist-write-token-0123456789"
	distPersonToken = "dist-person-token-0123456789"
	distHubYML      = "version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    url: http://localhost\n    writer: acme-write[bot]\n"
	distTargetsYML  = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\n"
)

// needDistributeGit skips a test that runs distribute past its guards on a
// git older than distribute supports.
func needDistributeGit(t *testing.T) {
	t.Helper()
	v, err := gitx.New("").Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := gitx.DeliveryMinVersion; slices.Compare(v[:], want[:]) < 0 {
		t.Skipf("git %d.%d.%d is older than %d.%d, which distribute needs; the Docker run of the suite covers this test", v[0], v[1], v[2], want[0], want[1])
	}
}

// distHub commits a hub with one provider on the fake's host and the pack
// base (AGENTS.md, docs/guide.md).
func distHub(t *testing.T) *repo {
	t.Helper()
	h := newHub(t)
	h.write("hub.yml", distHubYML)
	h.write("targets.yml", distTargetsYML)
	h.write("packs/base/AGENTS.md", planAgentsV2)
	h.write("packs/base/docs/guide.md", planGuide)
	h.commit("the base pack")
	return h
}

// distWorld is the fake platform of the distribute tests, in git mode.
type distWorld struct {
	t              *testing.T
	p              *fake.Platform
	writer, person platform.Account
	// repos are the repositories by path.
	repos map[string]platform.Repo
	// built counts the drivers distribute built.
	built int
}

// newDistWorld serves a fake GitHub on the host localhost with three
// targets: acme/billing and acme/api opted in, acme/web not.
func newDistWorld(t *testing.T) *distWorld {
	t.Helper()
	p := fake.New("localhost")
	w := &distWorld{t: t, p: p, writer: p.AddAccount("acme-write[bot]", platform.KindBot), person: p.AddAccount("jdoe", platform.KindUser),
		repos: map[string]platform.Repo{}}
	srv, err := p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the git server: %v", err)
		}
		if v := p.Violations(); len(v) > 0 {
			t.Errorf("forbidden transitions: %q", v)
		}
	})
	p.SetToken(w.writer, distWriteToken)
	p.SetToken(w.person, distPersonToken)
	for _, path := range []string{"acme/billing", "acme/api"} {
		w.repo(path, planOptIn, "version: 1\n", "README.md", "# "+path+"\n")
	}
	w.repo("acme/web", "README.md", "not opted in\n")
	return w
}

func (w *distWorld) repo(path string, files ...string) platform.Repo {
	w.t.Helper()
	r := w.p.AddRepo(platform.Repo{Path: path})
	for i := 0; i+1 < len(files); i += 2 {
		w.p.SetFile(r.ID, files[i], []byte(files[i+1]), "")
	}
	w.p.GrantWrite(r.ID, w.writer)
	if err := w.p.Err(); err != nil {
		w.t.Fatal(err)
	}
	got, _ := w.p.RepoByID(r.ID)
	w.repos[path] = got
	return got
}

// install replaces the write drivers with the world's platform until the
// test ends: a writer acts as the account of its token.
func (w *distWorld) install() {
	w.t.Helper()
	drivers := distributeDrivers
	w.t.Cleanup(func() { distributeDrivers = drivers })
	driver := func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
		w.built++
		if client == nil {
			return nil, errors.New("no HTTP client")
		}
		if rp.Host != w.p.Host() || c.Token != distWriteToken {
			return nil, fmt.Errorf("unexpected provider %s or credential", rp.Host)
		}
		return w.p.Writer(w.writer), nil
	}
	distributeDrivers = map[string]distributeDriver{"github": driver}
}

// distEnv is a maintainer's shell with the write token.
func distEnv() map[string]string {
	return map[string]string{"TOUCHMARK_GH_WRITE_TOKEN": distWriteToken}
}

// runDistributeCmd runs distribute against the hub and fails the test unless it
// exits with code.
func runDistributeCmd(t *testing.T, h *repo, vars map[string]string, code int, extra ...string) result {
	t.Helper()
	args := append([]string{"distribute", "--hub", h.dir}, extra...)
	res := runWith(t, vars, args...)
	if res.code != code {
		t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, code, res.stdout, res.stderr)
	}
	return res
}

// A dry run reads everything with the write identity and writes nothing;
// its report is the golden one.
func TestDistributeDryRun(t *testing.T) {
	needDistributeGit(t)
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	s := newScenario(t, "distribute-dry-run", h, nil)
	res := runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP, "--dry-run")
	s.golden("dry-run.txt", s.normalize(res.stdout))
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("a dry run wrote: %q", writes)
	}
	res = runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP, "--dry-run", "--format", "json")
	rep := decodeDelivery(t, res.stdout)
	if rep.Command != "distribute" || !rep.DryRun || len(rep.Ops) != 0 {
		t.Errorf("command %s, dry run %v, ops %+v", rep.Command, rep.DryRun, rep.Ops)
	}
	got := map[string]string{}
	for _, tg := range rep.Targets {
		got[tg.Path] = string(tg.Outcome) + ":" + tg.Reason
	}
	want := map[string]string{"acme/billing": "opened:", "acme/api": "opened:", "acme/web": "skipped:not-opted-in"}
	if !maps.Equal(got, want) {
		t.Errorf("outcomes %v, want %v", got, want)
	}
}

// distribute on the fake: it opens the pull requests, writes the report
// and the stream, and a second run writes nothing.
func TestDistributeOpensPullRequests(t *testing.T) {
	needDistributeGit(t)
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	dir := t.TempDir()
	reportFile, stream := filepath.Join(dir, "report.json"), filepath.Join(dir, "stream.jsonl")
	res := runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP, "--format", "json", "--report", reportFile, "--stream", stream)
	rep := decodeDelivery(t, res.stdout)
	for _, path := range []string{"acme/billing", "acme/api"} {
		id := w.repos[path].ID
		pr := w.p.PR(id, 1)
		m, status := marker.Find(pr.Body, []string{distFP})
		if pr.State != platform.Open || status != marker.Found || m.Data.Hub != "acme-eng" || pr.Head != distBranch ||
			pr.HeadSHA != w.p.Branch(id, distBranch) || pr.HeadSHA == "" {
			t.Errorf("%s #1: %s, marker %s, head %s", path, pr.State, status, pr.HeadSHA)
		}
	}
	var kinds []string
	for _, op := range rep.Ops {
		kinds = append(kinds, op.Target+" "+op.Kind)
		if op.Account != "acme-write[bot]" || op.Time.IsZero() {
			t.Errorf("op %+v", op)
		}
	}
	slices.Sort(kinds)
	if want := []string{"gh:acme/api create-pr", "gh:acme/api push", "gh:acme/billing create-pr", "gh:acme/billing push"}; !slices.Equal(kinds, want) {
		t.Errorf("ops %q, want %q", kinds, want)
	}
	// Four HTTP writes per target on GitHub: the push, the
	// pull request, the label created at its first use, and the call that
	// puts it on the pull request.
	if rep.Cost["gh"] != 8 || rep.Summary[report.OutcomeOpened] != 2 {
		t.Errorf("cost %v, summary %v", rep.Cost, rep.Summary)
	}
	data, err := os.ReadFile(reportFile)
	if err != nil {
		t.Fatal(err)
	}
	if fileRep := decodeDelivery(t, string(data)); fileRep.Summary[report.OutcomeOpened] != 2 || len(fileRep.Ops) != 4 {
		t.Errorf("--report: summary %v, %d ops", fileRep.Summary, len(fileRep.Ops))
	}
	lines := streamLines(t, stream)
	if len(lines) != 3 || lines["acme/billing"] != 2 || lines["acme/api"] != 2 || lines["acme/web"] != 0 {
		t.Errorf("stream: ops by target %v", lines)
	}
	for _, out := range []string{res.stdout, res.stderr, string(data)} {
		if strings.Contains(out, distWriteToken) {
			t.Error("the write token reached the output")
		}
	}

	// Again: nothing to write.
	w.p.ResetCalls()
	res = runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP, "--format", "json")
	rep = decodeDelivery(t, res.stdout)
	if writes := w.p.Writes(); len(writes) > 0 || rep.Summary[report.OutcomeUnchanged] != 2 || len(rep.Ops) != 0 {
		t.Errorf("a second run: writes %q, summary %v", writes, rep.Summary)
	}
}

// streamLines reads a report stream and returns the number of ops of each
// target's line.
func streamLines(t *testing.T, name string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var l struct {
			Target report.DeliveryTarget `json:"target"`
			Ops    []report.Op           `json:"ops"`
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			t.Fatalf("stream line %q: %v", line, err)
		}
		if _, dup := out[l.Target.Path]; dup {
			t.Errorf("%s streamed twice", l.Target.Path)
		}
		out[l.Target.Path] = len(l.Ops)
	}
	return out
}

// In GitHub Actions: the credentials are masked first, the guards pass for
// the default branch with the probe's answer, and the step summary, the
// annotations (on stderr: stdout holds the report) and the report files
// for the artifact are written.
func TestDistributeActions(t *testing.T) {
	needDistributeGit(t)
	work := t.TempDir()
	t.Chdir(work)
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	tip := h.git("rev-parse", "HEAD")
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if serveEnvironment(rw, r, hubEnvironment, hubEnvironmentPolicies) {
			return
		}
		if r.URL.Path != "/repos/acme/engineering-assets/git/ref/heads/master" {
			http.NotFound(rw, r)
			return
		}
		fmt.Fprintf(rw, `{"ref": "refs/heads/master", "object": {"sha": %q, "type": "commit"}}`, tip)
	}))
	defer api.Close()
	vars := actionsEnv(t)
	delete(vars, "TOUCHMARK_GH_READ_TOKEN")
	delete(vars, "TOUCHMARK_CORP_READ_TOKEN")
	vars["TOUCHMARK_GH_WRITE_TOKEN"] = distWriteToken
	vars["TOUCHMARK_KEY_EXPOSED"] = "false"
	vars["GITHUB_EVENT_NAME"] = "schedule"
	vars["GITHUB_REF"], vars["GITHUB_REF_NAME"] = "refs/heads/master", "master"
	vars["GITHUB_API_URL"] = api.URL
	summary := filepath.Join(t.TempDir(), "summary.md")
	vars["GITHUB_STEP_SUMMARY"] = summary
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	w.p.Fail("CreatePR", &platform.Error{Op: "create pull request", Class: platform.ClassAuth, Status: http.StatusUnauthorized,
		Err: errors.New("token " + distWriteToken + " expired")})
	res := runDistributeCmd(t, h, vars, exitFailed, "--stream", stream)
	if !strings.HasPrefix(res.stderr, "::add-mask::") || !strings.Contains(res.stderr, "::add-mask::"+distWriteToken+"\n") {
		t.Errorf("stderr:\n%s", res.stderr)
	}
	if strings.Contains(res.stdout, distWriteToken) || strings.Contains(res.stdout, "::error") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	// The ::add-mask:: commands carry the token by design; nothing else on
	// stderr may.
	var unmasked []string
	for line := range strings.SplitSeq(res.stderr, "\n") {
		if !strings.HasPrefix(line, "::add-mask::") && strings.Contains(line, distWriteToken) {
			unmasked = append(unmasked, line)
		}
	}
	if !strings.Contains(res.stderr, "::error title=touchmark::gh:acme/api failed:auth: ") || len(unmasked) > 0 {
		t.Errorf("stderr:\n%s", res.stderr)
	}
	for _, name := range []string{"touchmark-report.json", "touchmark-report.md"} {
		data, err := os.ReadFile(filepath.Join(work, name))
		if err != nil || !strings.Contains(string(data), "acme/api") || strings.Contains(string(data), distWriteToken) {
			t.Errorf("%s %v:\n%s", name, err, data)
		}
		if err == nil && strings.HasSuffix(name, ".json") {
			validateSchema(t, "report", data)
		}
	}
	data, err := os.ReadFile(summary)
	if err != nil || !strings.Contains(string(data), "### touchmark distribute") || strings.Contains(string(data), distWriteToken) {
		t.Errorf("step summary %v:\n%s", err, data)
	}
	if lines := streamLines(t, stream); len(lines) != 3 {
		t.Errorf("stream %v", lines)
	}
}

// Every refusal exits 2 before anything is written.
func TestDistributeRefusals(t *testing.T) {
	h := distHub(t)
	res := runDistributeCmd(t, h, distEnv(), exitUsage, "--hub-fp", distFP, "--worktree")
	if !strings.Contains(res.stderr, "distribute ships only committed packs") {
		t.Errorf("stderr: %s", res.stderr)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--recreate", "acme/api"}, `"acme/api" is not TARGET@HEAD`},
		{[]string{"--recreate", "acme/api@1234"}, "is not a full commit id"},
		{[]string{"--forget-declines", "acme/api#x"}, "is not a pull request number"},
		{[]string{"--allow-mass-close", "0"}, "is not a positive number"},
		{[]string{"--deadline", "soon"}, "is not a duration"},
		{[]string{"--format", "yaml"}, `"yaml" is not text, json or markdown`},
	} {
		res := runDistributeCmd(t, h, distEnv(), exitUsage, append([]string{"--hub-fp", distFP}, tc.args...)...)
		if !strings.Contains(res.stderr, tc.want) {
			t.Errorf("%v: stderr %s", tc.args, res.stderr)
		}
	}

	// The operation flags are refused in CI, whatever else the guards say.
	vars := actionsEnv(t)
	vars["TOUCHMARK_KEY_EXPOSED"] = "false"
	for _, flag := range [][]string{
		{"--recreate", "gh:acme/api@" + strings.Repeat("a", 40)},
		{"--forget-declines", "gh:acme/api#44"},
		{"--allow-mass-close", "400"},
		{"--adopt-unmarked"},
		{"--allow-stale"},
	} {
		res := runDistributeCmd(t, h, vars, exitUsage, flag...)
		if !strings.Contains(res.stderr, "work only in a local run") || res.stdout != "" {
			t.Errorf("%v: stdout %q, stderr %q", flag, res.stdout, res.stderr)
		}
	}
	// A pull request's job may not write, even without operation flags.
	res = runDistributeCmd(t, h, vars, exitUsage)
	if !strings.Contains(res.stderr, "does not run for the pull_request event") {
		t.Errorf("stderr: %s", res.stderr)
	}
	// A write key visible outside the environment.
	vars["TOUCHMARK_KEY_EXPOSED"] = "true"
	vars["GITHUB_EVENT_NAME"], vars["GITHUB_REF"], vars["GITHUB_REF_NAME"] = "schedule", "refs/heads/master", "master"
	res = runDistributeCmd(t, h, vars, exitUsage)
	if !strings.Contains(res.stderr, "TOUCHMARK_KEY_EXPOSED=true") {
		t.Errorf("stderr: %s", res.stderr)
	}
}

// Past the guards: a provider without a write credential, and a local run
// whose HEAD is not the tip of origin's default branch.
func TestDistributeLocalRefusals(t *testing.T) {
	needDistributeGit(t)
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	res := runDistributeCmd(t, h, map[string]string{}, exitUsage, "--hub-fp", distFP)
	if !strings.Contains(res.stderr, "provider gh: no write credential: set TOUCHMARK_GH_WRITE_TOKEN") {
		t.Errorf("stderr: %s", res.stderr)
	}
	res = runDistributeCmd(t, h, map[string]string{"TOUCHMARK_GH_WRITE_TOKEN": "has space"}, exitUsage, "--hub-fp", distFP)
	if !strings.Contains(res.stderr, "TOUCHMARK_GH_WRITE_TOKEN holds a space") {
		t.Errorf("stderr: %s", res.stderr)
	}
	res = runDistributeCmd(t, h, map[string]string{"TOUCHMARK_GH_WRITE_TOKEN": distWriteToken, "TOUCHMARK_GH_SIGNING_KEY": "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----"},
		exitUsage, "--hub-fp", distFP)
	if !strings.Contains(res.stderr, "TOUCHMARK_GH_SIGNING_KEY") {
		t.Errorf("stderr: %s", res.stderr)
	}

	// origin's default branch is behind HEAD: refused unless --allow-stale.
	old := h.git("rev-parse", "HEAD")
	h.git("update-ref", "refs/remotes/origin/master", old)
	h.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	h.write("packs/base/AGENTS.md", planAgentsV1)
	h.commit("an unreviewed change")
	res = runDistributeCmd(t, h, distEnv(), exitUsage, "--hub-fp", distFP, "--dry-run")
	if !strings.Contains(res.stderr, "is not the tip of origin/master") {
		t.Errorf("stderr: %s", res.stderr)
	}
	if w.built > 0 {
		t.Errorf("a refused run built %d drivers", w.built)
	}
	runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP, "--dry-run", "--allow-stale")
}

func TestProbeCommand(t *testing.T) {
	res := runWith(t, map[string]string{"TOUCHMARK_GH_READ_TOKEN": "read-token-0123456789"}, "probe")
	if res.code != exitOK || !strings.Contains(res.stdout, "no write credential or signing key is visible") {
		t.Errorf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	secret := "write-token-0123456789"
	res = runWith(t, map[string]string{"TOUCHMARK_GH_WRITE_TOKEN": secret, "TOUCHMARK_SIGNING_KEY": "-----BEGIN OPENSSH PRIVATE KEY-----"}, "probe")
	if res.code != exitUsage || !strings.Contains(res.stderr, "TOUCHMARK_GH_WRITE_TOKEN, TOUCHMARK_SIGNING_KEY are visible in this job") ||
		strings.Contains(res.stderr, secret) || strings.Contains(res.stderr, "BEGIN") {
		t.Errorf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if res := runWith(t, nil, "probe", "extra"); res.code != exitUsage {
		t.Errorf("an extra argument: exit %d", res.code)
	}
}

// The default deadline comes from the CI: the job's timeout less 5 minutes
// on GitLab, 5h30m on GitHub Actions, none elsewhere; --deadline wins.
func TestDistributeDeadline(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	gitlab := hubch.Context{CI: hubch.GitLabCI}
	for _, tc := range []struct {
		name string
		flag string
		ctx  hubch.Context
		env  map[string]string
		want time.Duration // 0: no deadline
	}{
		{name: "local", ctx: hubch.Context{CI: hubch.Local}},
		{name: "github", ctx: hubch.Context{CI: hubch.GitHubActions}, want: 5*time.Hour + 30*time.Minute},
		{name: "gitlab", ctx: gitlab, env: map[string]string{"CI_JOB_TIMEOUT": "3600"}, want: 55 * time.Minute},
		{name: "gitlab short job", ctx: gitlab, env: map[string]string{"CI_JOB_TIMEOUT": "120"}, want: time.Minute},
		{name: "gitlab without timeout", ctx: gitlab},
		{name: "gitea", ctx: hubch.Context{CI: hubch.GiteaActions}},
		{name: "flag", flag: "50m", ctx: hubch.Context{CI: hubch.GitHubActions}, want: 50 * time.Minute},
		{name: "flag none", flag: "0", ctx: gitlab, env: map[string]string{"CI_JOB_TIMEOUT": "3600"}},
	} {
		d := &distOptions{}
		if tc.flag != "" {
			if err := d.deadline.Set(tc.flag); err != nil {
				t.Fatal(err)
			}
		}
		got := deadline(d, tc.ctx, func(k string) string { return tc.env[k] }, now)
		if (tc.want == 0 && !got.IsZero()) || (tc.want != 0 && !got.Equal(now.Add(tc.want))) {
			t.Errorf("%s: deadline %v, want now + %v", tc.name, got, tc.want)
		}
	}
}

// The operation flags become the operations of the day, for the run only.
func TestDistributeOperationFlags(t *testing.T) {
	head := strings.Repeat("A", 40)
	d := &distOptions{}
	fs, _ := distributeFlags(d)
	if err := fs.parse([]string{"--recreate", "gh:acme/api@" + head, "--forget-declines", "acme/docs#44", "--allow-mass-close", "400", "--adopt-unmarked"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC)
	ops := d.operations(now)
	if !d.localOps() || ops == nil || len(ops.Recreate) != 1 || ops.Recreate[0].Head != strings.ToLower(head) || ops.Recreate[0].Target != "gh:acme/api" ||
		len(ops.ForgetDeclines) != 1 || ops.ForgetDeclines[0].PR != 44 || ops.AllowMassClose.Max != 400 ||
		!ops.AllowMassClose.Active(now) || ops.AllowMassClose.Active(now.Add(2*time.Hour)) || !ops.AdoptUnmarked.Active(now) {
		t.Errorf("operations %+v", ops)
	}
	if _, errs := config.CheckOperations(ops, &config.Targets{}, &config.Hub{Providers: []config.Provider{{ID: "gh"}}}, now); len(errs) > 0 {
		t.Errorf("check: %v", errs)
	}
	d = &distOptions{}
	fs, _ = distributeFlags(d)
	if err := fs.parse([]string{"--allow-stale"}); err != nil {
		t.Fatal(err)
	}
	if !d.localOps() || d.operations(now) != nil {
		t.Errorf("--allow-stale: local %v, operations %+v", d.localOps(), d.operations(now))
	}
}
