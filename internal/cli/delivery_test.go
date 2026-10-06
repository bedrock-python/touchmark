package cli

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// distReadToken is the read token of the distribute world's reader.
const distReadToken = "dist-read-token-0123456789"

// installPlan makes plan read the world's platform through a reader of its
// own, with plan's own snapshot source (a git repository per target, as
// distribute's), until the test ends.
func (w *distWorld) installPlan() {
	w.t.Helper()
	reader := w.p.AddAccount("acme-read[bot]", platform.KindBot)
	w.p.SetToken(reader, distReadToken)
	if err := w.p.Err(); err != nil {
		w.t.Fatal(err)
	}
	drivers, snapshots := planDrivers, planSnapshots
	w.t.Cleanup(func() { planDrivers, planSnapshots = drivers, snapshots })
	planSnapshots = nil
	planDrivers = map[string]planDriver{"github": func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
		if client == nil || c.Token != distReadToken || rp.Host != w.p.Host() {
			return nil, errors.New("unexpected provider or credential")
		}
		return w.p.Reader(reader), nil
	}}
}

// deliveryLines renders what plan and distribute --dry-run must agree on for
// each target: outcome, reason, pull request, key and
// writes.
func deliveryLines(rep report.Delivery) []string {
	var out []string
	for _, tg := range rep.Targets {
		pr := int64(0)
		if tg.PR != nil {
			pr = tg.PR.Number
		}
		out = append(out, fmt.Sprintf("%s:%s %s:%s #%d key=%s writes=%d", tg.Provider, tg.Path, tg.Outcome, tg.Reason, pr, tg.Key, tg.Writes))
	}
	return out
}

// plan and distribute --dry-run say the same through the command line: a
// decline, a paused branch and a sweep close, which only phase C's full
// inspection and the sweep see. Plan once ran the in-memory plan and
// reported new pull requests where distribute would do none.
func TestPlanEqualsDryRun(t *testing.T) {
	needDistributeGit(t)
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	w.installPlan()
	runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP)
	// A person declines billing's pull request, and pushes to api's branch.
	w.p.SetPRState(w.repos["acme/billing"].ID, 1, platform.Closed, &w.person, time.Time{})
	if _, err := w.p.PushFiles(w.repos["acme/api"].ID, distBranch, map[string][]byte{"notes.md": []byte("the team's notes\n")}, w.person, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// A repository outside targets.yml with a pull request of touchmark's.
	outside := w.repo("other/outside", "README.md", "not a target\n")
	line, err := marker.Encode(marker.Marker{Key: "sha256:" + strings.Repeat("0", 64), Data: marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: distFP, Engine: "test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	w.p.AddPR(outside.ID, platform.PR{Head: distBranch, Author: w.writer, Title: "chore: sync engineering assets", Body: "Synced.\n\n" + line})
	if err := w.p.Err(); err != nil {
		t.Fatal(err)
	}
	w.p.ResetCalls()

	plan := decodeDelivery(t, planRun(t, h, map[string]string{"TOUCHMARK_GH_READ_TOKEN": distReadToken}, exitOK, "--hub-fp", distFP, "--format", "json").stdout)
	dry := decodeDelivery(t, runDistributeCmd(t, h, distEnv(), exitOK, "--hub-fp", distFP, "--dry-run", "--format", "json").stdout)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("plan or the dry run wrote %q", writes)
	}
	if a, b := deliveryLines(plan), deliveryLines(dry); !slices.Equal(a, b) {
		t.Errorf("plan and the dry run differ:\nplan %q\ndry  %q", a, b)
	}
	if plan.Sweep != dry.Sweep || !maps.Equal(plan.Summary, dry.Summary) || !maps.Equal(plan.Cost, dry.Cost) {
		t.Errorf("plan: sweep %+v summary %v cost %v; dry run: sweep %+v summary %v cost %v",
			plan.Sweep, plan.Summary, plan.Cost, dry.Sweep, dry.Summary, dry.Cost)
	}
	got := map[string]string{}
	for _, tg := range plan.Targets {
		got[tg.Path] = string(tg.Outcome) + ":" + tg.Reason
	}
	for path, want := range map[string]string{"acme/billing": "declined:", "acme/api": "blocked:edited", "other/outside": "closed:target-dropped"} {
		if got[path] != want {
			t.Errorf("%s is %q, want %q (%v)", path, got[path], want, got)
		}
	}
}

// scheduleEnv is the environment of a scheduled GitHub Actions job of the
// hub's default branch: the event payload names no repository (GitHub's
// schedule event), so neither the default branch nor the visibility is in
// it. api is the hub's API.
func scheduleEnv(t *testing.T, api string) map[string]string {
	t.Helper()
	vars := actionsEnv(t)
	delete(vars, "TOUCHMARK_GH_READ_TOKEN")
	delete(vars, "TOUCHMARK_CORP_READ_TOKEN")
	if err := os.WriteFile(vars["GITHUB_EVENT_PATH"], []byte(`{"schedule": "17 * * * *"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	vars["TOUCHMARK_GH_WRITE_TOKEN"] = distWriteToken
	vars["TOUCHMARK_KEY_EXPOSED"] = "false"
	vars["GITHUB_EVENT_NAME"] = "schedule"
	vars["GITHUB_REF"], vars["GITHUB_REF_NAME"] = "refs/heads/master", "master"
	vars["GITHUB_API_URL"] = api
	return vars
}

// hubAPI serves the hub's API for the channel: the tip of master (tip), the
// repository's visibility, the environment touchmark-distribute (env, or
// hubEnvironment's answer when it holds ""), and a status for every
// request when fail is set.
type hubAPI struct {
	*httptest.Server
	tip      atomic.Value // string
	env      atomic.Value // string: the environment's JSON, "404" or "403"
	policies atomic.Value // string: its deployment branch policies' JSON
	fail     atomic.Int32 // an HTTP status, 0 for none
	heads    atomic.Int32 // requests for the tip
}

// The environment touchmark-distribute of the tests' hub as the template
// sets it up: the default branch, master, only.
const (
	hubEnvironment = `{"id": 1, "name": "touchmark-distribute", "protection_rules": [{"type": "branch_policy"}],
		"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}`
	hubEnvironmentPolicies = `{"total_count": 1, "branch_policies": [{"id": 3, "node_id": "x", "name": "master", "type": "branch"}]}`
)

// serveEnvironment answers the channel's requests for the environment
// touchmark-distribute of the hub acme/engineering-assets with env (the
// environment's JSON; "404" for none, "403" for a token without the
// Actions read permission) and policies (its deployment branch policies);
// it reports whether r was one.
func serveEnvironment(rw http.ResponseWriter, r *http.Request, env, policies string) bool {
	const base = "/repos/acme/engineering-assets/environments/touchmark-distribute"
	switch r.URL.Path {
	case base:
		switch env {
		case "404":
			http.Error(rw, `{"message": "Not Found", "status": "404"}`, http.StatusNotFound)
		case "403":
			http.Error(rw, `{"message": "Resource not accessible by integration", "status": "403"}`, http.StatusForbidden)
		default:
			fmt.Fprint(rw, env)
		}
		return true
	case base + "/deployment-branch-policies":
		fmt.Fprint(rw, policies)
		return true
	}
	return false
}

func newHubAPI(t *testing.T, tip string) *hubAPI {
	t.Helper()
	a := &hubAPI{}
	a.tip.Store(tip)
	a.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// The environment answers whatever fail says: the environment check
		// fails closed on its own (TestDistributeEnvironment), and fail
		// tests the channel's other reads.
		env, _ := a.env.Load().(string)
		policies, _ := a.policies.Load().(string)
		if serveEnvironment(rw, r, cmp.Or(env, hubEnvironment), cmp.Or(policies, hubEnvironmentPolicies)) {
			return
		}
		if status := a.fail.Load(); status != 0 {
			http.Error(rw, `{"message":"unavailable"}`, int(status))
			return
		}
		switch r.URL.Path {
		case "/repos/acme/engineering-assets/git/ref/heads/master":
			a.heads.Add(1)
			fmt.Fprintf(rw, `{"ref": "refs/heads/master", "object": {"sha": %q, "type": "commit"}}`, a.tip.Load())
		case "/repos/acme/engineering-assets":
			fmt.Fprint(rw, `{"full_name": "acme/engineering-assets", "private": true, "visibility": "private"}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

// The head guard holds for the scheduled runs the template triggers: the
// channel reads the tip of the branch the schedule builds, and a run whose
// HEAD is not it writes nothing. Once, as the schedule's payload names no
// default branch, the channel could not be built, and distribute wrote the
// old hub commit's content everywhere.
func TestDistributeScheduleChecksHead(t *testing.T) {
	needDistributeGit(t)
	// A CI run leaves its report files in the working directory.
	t.Chdir(t.TempDir())
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	api := newHubAPI(t, strings.Repeat("b", 40))
	vars := scheduleEnv(t, api.URL)
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	res := runDistributeCmd(t, h, vars, exitOK, "--format", "json", "--stream", stream)
	rep := decodeDelivery(t, res.stdout)
	if rep.Outcome != report.Superseded || api.heads.Load() == 0 {
		t.Errorf("outcome %s, %d reads of the tip", rep.Outcome, api.heads.Load())
	}
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("a superseded run wrote %q", writes)
	}
	// At the tip, the run goes on; the visibility the payload lacks comes
	// from the channel.
	api.tip.Store(h.git("rev-parse", "HEAD"))
	res = runDistributeCmd(t, h, vars, exitOK, "--dry-run", "--format", "json", "--stream", stream)
	rep = decodeDelivery(t, res.stdout)
	if rep.Outcome != report.Completed || rep.Summary[report.OutcomeOpened] != 2 {
		t.Errorf("outcome %s, summary %v", rep.Outcome, rep.Summary)
	}
	for _, warning := range rep.Warnings {
		if strings.Contains(warning, "visibility is unknown") {
			t.Errorf("warning %q", warning)
		}
	}
}

// On GitHub Actions under write_isolation platform, the hub channel checks
// that only the default branch may use the environment
// touchmark-distribute: no environment, one any ref may
// use, one that lets a tag or another branch use it, one that lets every
// protected branch use it and one the token cannot read stop distribute
// (exit 2) before anything is read: the check fails closed.
func TestDistributeEnvironment(t *testing.T) {
	needDistributeGit(t)
	// A CI run leaves its report files in the working directory.
	t.Chdir(t.TempDir())
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	api := newHubAPI(t, h.git("rev-parse", "HEAD"))
	vars := scheduleEnv(t, api.URL)
	custom := `{"name": "touchmark-distribute", "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}`
	for _, tc := range []struct {
		name, env, policies string
		warning, refusal    string
	}{
		{name: "the template's"},
		{name: "none", env: "404", refusal: "the hub has no environment touchmark-distribute"},
		{name: "any ref", env: `{"name": "touchmark-distribute", "deployment_branch_policy": null}`, refusal: "lets any branch and tag use it"},
		{name: "a tag", env: custom, policies: `{"total_count": 2, "branch_policies": [{"name": "master", "type": "branch"}, {"name": "v*", "type": "tag"}]}`,
			refusal: "lets tag v* use it besides the default branch master"},
		{name: "another branch", env: custom, policies: `{"total_count": 1, "branch_policies": [{"name": "release/*", "type": "branch"}]}`,
			refusal: "lets branch release/* use it"},
		{name: "protected branches", env: `{"name": "touchmark-distribute", "deployment_branch_policy": {"protected_branches": true, "custom_branch_policies": false}}`,
			refusal: "lets every protected branch use it"},
		{name: "unreadable", env: "403", refusal: "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api.env.Store(tc.env)
			api.policies.Store(tc.policies)
			if tc.refusal != "" {
				res := runDistributeCmd(t, h, vars, exitUsage, "--dry-run", "--stream", filepath.Join(t.TempDir(), "stream.jsonl"))
				if !strings.Contains(res.stderr, tc.refusal) {
					t.Errorf("stderr: %s", res.stderr)
				}
				if calls := w.p.Calls(); len(calls) > 0 {
					t.Errorf("a refused run called the platform: %q", calls)
				}
				return
			}
			res := runDistributeCmd(t, h, vars, exitOK, "--dry-run", "--format", "json", "--stream", filepath.Join(t.TempDir(), "stream.jsonl"))
			rep := decodeDelivery(t, res.stdout)
			found := slices.ContainsFunc(rep.Warnings, func(s string) bool { return strings.Contains(s, "touchmark-distribute") })
			switch {
			case tc.warning == "" && found:
				t.Errorf("warnings %q", rep.Warnings)
			case tc.warning != "" && !slices.ContainsFunc(rep.Warnings, func(s string) bool { return strings.Contains(s, tc.warning) }):
				t.Errorf("warnings %q lack %q", rep.Warnings, tc.warning)
			}
			w.p.ResetCalls()
		})
	}
}

// The head guard fails closed for distribute: a hub channel that fails
// stops the run before anything is read or written (exit 2); a plan warns.
// Every channel error was once a warning, and distribute wrote.
func TestDistributeChannelFails(t *testing.T) {
	needDistributeGit(t)
	// A CI run leaves its report files in the working directory.
	t.Chdir(t.TempDir())
	h := distHub(t)
	w := newDistWorld(t)
	w.install()
	w.installPlan()
	api := newHubAPI(t, "")
	api.fail.Store(http.StatusServiceUnavailable)
	vars := scheduleEnv(t, api.URL)
	res := runDistributeCmd(t, h, vars, exitUsage, "--stream", filepath.Join(t.TempDir(), "stream.jsonl"))
	if !strings.Contains(res.stderr, "the tip of the hub's default branch could not be read") || !strings.Contains(res.stderr, "nothing is written") {
		t.Errorf("stderr: %s", res.stderr)
	}
	if calls := w.p.Calls(); len(calls) > 0 {
		t.Errorf("a stopped run called the platform: %q", calls)
	}
	plan := map[string]string{}
	for k, v := range vars {
		if !strings.Contains(k, "WRITE") {
			plan[k] = v
		}
	}
	plan["TOUCHMARK_GH_READ_TOKEN"] = distReadToken
	rep := decodeDelivery(t, planRun(t, h, plan, exitOK, "--format", "json").stdout)
	if !slices.ContainsFunc(rep.Warnings, func(s string) bool { return strings.HasPrefix(s, "hub head not checked: hub channel:") }) {
		t.Errorf("warnings %q", rep.Warnings)
	}
}

// A failure while distribute prepares its run exits 2, leaves no
// temporary directory and never panics. Every preparation error once
// dereferenced a nil run.
func TestDistributePreparationErrors(t *testing.T) {
	needDistributeGit(t)
	h := distHub(t)
	w := newDistWorld(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	drivers := distributeDrivers
	t.Cleanup(func() { distributeDrivers = drivers })
	distributeDrivers = map[string]distributeDriver{"github": func(config.ResolvedProvider, auth.Credential, *httpx.Client) (platform.Writer, error) {
		return nil, errors.New("the credential has no installation")
	}}
	res := runDistributeCmd(t, h, distEnv(), exitUsage, "--hub-fp", distFP)
	if !strings.Contains(res.stderr, "provider gh: the credential has no installation") {
		t.Errorf("stderr: %s", res.stderr)
	}
	w.install()
	res = runDistributeCmd(t, h, distEnv(), exitUsage, "--hub-fp", distFP, "--stream", filepath.Join(tmp, "missing", "stream.jsonl"))
	if !strings.Contains(res.stderr, "--stream") {
		t.Errorf("stderr: %s", res.stderr)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "touchmark-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}
