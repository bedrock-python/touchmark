package githube2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestDistribute is the life of one hub on GitHub through the command line
// in process (cli.Main), as a maintainer runs it, against the fake as a
// GitHub Enterprise Server with web commit signing (the drivers see the
// fake's host, which is not github.com): the reader and the writer are
// GitHub Apps whose ids and keys are generated at run time and passed in
// TOUCHMARK_GH_READ_APP_ID/KEY and TOUCHMARK_GH_WRITE_APP_ID/KEY. The
// writer's installation starts without the Workflows permission.
//
//  1. plan with the reader: pull requests would open in the opted-in
//     targets; nothing is written.
//  2. distribute with the writer: pull requests by touchmark-write[bot],
//     drafts as hub.yml asks, with the body, the marker, the label and the
//     writer's commit. epsilon, a private repository of a plan without
//     drafts, gets a ready pull request instead; eta, whose default branch
//     requires signed commits, gets a commit GitHub made and signed through
//     the API (three writes: the stage ref, the commit, the ref update; no
//     stage ref left); theta, whose pack changes a workflow, is blocked
//     before any write; kappa restricts the creation of touchmark/**
//     branches, which the push meets (blocked:rules:ruleset, nothing
//     written). A person opens a pull request from a fork of delta on the
//     sync branch's name, and adds a workflow to mu's default branch.
//  3. A person closes alpha's pull request (a decline, the closer read from
//     GraphQL's ClosedEvent), the writer's bot closes beta's (a self-close:
//     the content is proposed again).
//  4. The extra pack changes: delta's pull request is updated in place by a
//     push, eta's by an API commit (its pull request stays open); iota,
//     whose sync branch refuses force pushes, cannot be rebuilt
//     (blocked:rules:non-fast-forward, nothing written); mu's rebuild on
//     its new base moves the branch across the person's workflow, which
//     the installation may not do (blocked:permission:workflows).
//  5. The owner grants Workflows: theta opens, mu is rebuilt.
//  6. beta leaves targets.yml: the sweep closes its pull request as
//     target-dropped.
//
// After every distribute a second one writes nothing, the fork's pull
// request is untouched, every per-target token was minted for one
// repository and revoked, and the fake's journal is empty: no token used
// on another repository, no write with a revoked token, no branch of an
// open pull request moved onto its base or deleted. No token appears in any
// output.
func TestDistribute(t *testing.T) {
	needDistributeGit(t)
	s := newScenario(t, true)

	// 1. plan: what distribute would do; nothing is written. The reader
	// cannot know the writer's permissions: theta looks deliverable. It
	// reads the opt-in files in GraphQL batches, not one
	// contents request per target.
	sent := len(s.w.srv.Requests())
	rep := s.plan()
	for _, r := range s.w.srv.Requests()[sent:] {
		if r.Route == "GET /repos/{owner}/{repo}/contents/{path...}" {
			t.Errorf("plan read a file through the contents API: %+v", r)
		}
	}
	s.want("plan", rep, map[string]string{
		"alpha": "opened:", "beta": "opened:", "gamma": "skipped:not-opted-in", "delta": "opened:", "epsilon": "opened:",
		"eta": "opened:", "theta": "opened:", "iota": "opened:", "kappa": "opened:", "mu": "opened:",
	})
	if n := s.countPRs(); n != 0 {
		t.Fatalf("plan wrote: %d pull requests exist", n)
	}

	// 2. The first distribute.
	rep = s.settle("the first run", map[string]string{
		"alpha": "opened: #1", "beta": "opened: #1", "gamma": "skipped:not-opted-in", "delta": "opened: #1",
		"epsilon": "opened: #1", "eta": "opened: #1", "theta": "blocked:permission:workflows", "iota": "opened: #1",
		"kappa": "blocked:rules:ruleset", "mu": "opened: #1",
	})
	s.wantOps("the first run", rep, map[string][]string{
		"alpha": {"push", "create-pr #1"}, "beta": {"push", "create-pr #1"}, "delta": {"push", "create-pr #1"},
		"epsilon": {"push", "create-pr #1"}, "iota": {"push", "create-pr #1"}, "mu": {"push", "create-pr #1"},
		"eta": {"push", "api-commit", "update-refs", "create-pr #1"},
	})
	for _, name := range []string{"alpha", "beta", "delta", "epsilon", "eta", "iota", "mu"} {
		s.checkOpened(name, rep)
	}
	for name, draft := range map[string]bool{"alpha": true, "epsilon": false} {
		if pr := s.pr(name, 1); pr.Draft != draft {
			t.Errorf("%s #1: draft %v, want %v (epsilon's repository refuses drafts)", name, pr.Draft, draft)
		}
	}
	s.checkSigned("eta", s.pr("eta", 1).HeadSHA)
	s.noHiddenRefs("eta")
	for _, name := range []string{"theta", "kappa"} {
		if head := s.w.srv.Branch(s.repos[name], s.branch); head != "" || len(s.w.srv.PRs(s.repos[name])) != 0 {
			t.Errorf("%s is blocked, yet the sync branch is at %q and it has %d pull requests", name, head, len(s.w.srv.PRs(s.repos[name])))
		}
	}
	s.forkPR("delta")
	try(s.w.srv.Commit(s.repos["mu"], ghfake.CommitSpec{Author: person, Message: "CI of the team",
		Files: []ghfake.File{{Path: ".github/workflows/team.yml", Content: []byte(teamWorkflow)}}})).of(t)

	// 3. The person declines alpha; the writer's bot closes beta by hand.
	check(t, s.w.srv.SetPRState(s.repos["alpha"], 1, "closed", person))
	check(t, s.w.srv.SetPRState(s.repos["beta"], 1, "closed", s.w.writeApp.Bot.Login))
	rep = s.settle("alpha declined, beta closed by the writer", map[string]string{
		"alpha": "declined: #1", "beta": "opened: #2", "delta": "unchanged: #1", "epsilon": "unchanged: #1", "eta": "unchanged: #1",
		"theta": "blocked:permission:workflows", "iota": "unchanged: #1", "mu": "unchanged: #1",
	})
	s.wantOps("alpha declined, beta closed by the writer", rep, map[string][]string{
		"alpha": {"edit-pr #1", "comment #1"}, "beta": {"create-pr #2"},
	})
	s.checkDeclined("alpha", 1)
	if !s.graphQL("nodes") {
		t.Errorf("no GraphQL request read the closers (nodes)")
	}

	// 4. The extra pack changes.
	before := map[string]ghfake.PR{}
	for _, name := range extraTargets {
		before[name] = s.pr(name, 1)
	}
	s.packs["packs/extra/docs/extra.md"] = text("extra v2")
	s.commitHub("extra v2")
	rep = s.settle("the extra pack changed", map[string]string{
		"alpha": "declined: #1", "beta": "unchanged: #2", "delta": "updated:content #1", "eta": "updated:content #1",
		"iota": "blocked:rules:non-fast-forward #1", "mu": "blocked:permission:workflows #1",
	})
	s.wantOps("the extra pack changed", rep, map[string][]string{
		"delta": {"push", "edit-pr #1"}, "eta": {"push", "api-commit", "update-refs", "edit-pr #1"}, "mu": {"edit-pr #1"},
	})
	for _, name := range []string{"delta", "eta"} {
		after := s.pr(name, 1)
		switch {
		case after.State != "open" || after.HeadSHA == before[name].HeadSHA:
			t.Errorf("%s #1 after the pack change: %s at %s, want open at a new commit (was %s)", name, after.State, after.HeadSHA, before[name].HeadSHA)
		case s.file(name, s.branch, "docs/extra.md") != text("extra v2"):
			t.Errorf("%s's sync branch does not carry extra v2", name)
		case after.Body == before[name].Body:
			t.Errorf("%s #1 after the pack change: the body is not rewritten", name)
		}
		s.checkCommit(name, after.HeadSHA, name == "eta")
	}
	s.checkSigned("eta", s.pr("eta", 1).HeadSHA)
	s.noHiddenRefs("eta")
	for _, name := range []string{"iota", "mu"} {
		if after := s.pr(name, 1); after.State != "open" || after.HeadSHA != before[name].HeadSHA {
			t.Errorf("%s #1 is blocked, yet it is %s at %s (was %s)", name, after.State, after.HeadSHA, before[name].HeadSHA)
		}
	}
	if body := s.pr("mu", 1).Body; !strings.Contains(body, "Update branch") {
		t.Errorf("mu #1 is blocked on Workflows, but the body does not ask for Update branch:\n%s", body)
	}

	// 5. The owner grants the installation Workflows: theta opens, mu is
	// rebuilt on its new base.
	check(t, s.w.srv.SetInstallationPermissions(s.w.writeInst.ID, ghfake.Permissions{
		"contents": ghfake.Write, "pull_requests": ghfake.Write, "workflows": ghfake.Write, "metadata": ghfake.Read,
	}))
	rep = s.settle("Workflows granted", map[string]string{
		"theta": "opened: #1", "mu": "updated:content #1", "delta": "unchanged: #1", "iota": "blocked:rules:non-fast-forward #1",
	})
	s.wantOps("Workflows granted", rep, map[string][]string{"theta": {"push", "create-pr #1"}, "mu": {"push", "edit-pr #1"}})
	if got := s.file("theta", s.branch, ".github/workflows/lint.yml"); got != s.packs["packs/ci/.github/workflows/lint.yml"] {
		t.Errorf("theta's sync branch carries the workflow %q", got)
	}
	if got := s.file("mu", s.branch, ".github/workflows/team.yml"); got != teamWorkflow {
		t.Errorf("mu's rebuilt sync branch lacks the person's workflow: %q", got)
	}

	// 6. beta leaves targets.yml: the sweep closes its pull request.
	s.dropBeta = true
	s.commitHub("beta leaves the hub")
	rep = s.settle("beta dropped", map[string]string{"beta": "closed:target-dropped #2", "delta": "unchanged: #1"})
	s.wantOps("beta dropped", rep, map[string][]string{"beta": {"edit-pr #2", "comment #2"}})
	if pr := s.pr("beta", 2); pr.State != "closed" || pr.Merged {
		t.Errorf("beta #2 after the sweep: %s, merged %v; want closed", pr.State, pr.Merged)
	}
	if !rep.Sweep.Ran || !rep.Sweep.Complete {
		t.Errorf("the sweep: ran %v, complete %v", rep.Sweep.Ran, rep.Sweep.Complete)
	}
	s.scan()
}

// TestDistributeUnsigned: on a GitHub Enterprise Server without web commit
// signing (its default), the API commit eta's signature rule asks for comes
// back unsigned. touchmark never moves the branch to it nor opens a pull
// request (blocked:cannot-sign) and deletes its stage ref; the other
// targets are delivered as usual. The next run tries again, with the same
// two writes (the stage push and the API commit): runs keep no state, and
// the platform may have started signing.
func TestDistributeUnsigned(t *testing.T) {
	needDistributeGit(t)
	s := newScenario(t, false)
	rep := s.distribute()
	s.want("an unsigned API commit", rep, map[string]string{
		"alpha": "opened: #1", "delta": "opened: #1", "eta": "blocked:cannot-sign", "iota": "opened: #1",
	})
	ops := s.opsOf(rep, "eta")
	if !slices.Contains(ops, "api-commit") || slices.Contains(ops, "update-refs") || slices.Contains(ops, "create-pr #1") {
		t.Errorf("eta: the writes %q, want the stage push and the API commit, and neither a branch update nor a pull request", ops)
	}
	if head := s.w.srv.Branch(s.repos["eta"], s.branch); head != "" || len(s.w.srv.PRs(s.repos["eta"])) != 0 {
		t.Errorf("eta: the sync branch is at %q with %d pull requests after an unsigned API commit", head, len(s.w.srv.PRs(s.repos["eta"])))
	}
	s.noHiddenRefs("eta")
	s.w.violations()
	s.checkTokens()
	again := s.distribute()
	s.want("the next run", again, map[string]string{"alpha": "unchanged: #1", "eta": "blocked:cannot-sign"})
	if ops := s.opsOf(again, "eta"); !slices.Equal(ops, []string{"push", "api-commit"}) {
		t.Errorf("eta on the next run: the writes %q, want the stage push and the API commit again", ops)
	}
	s.noHiddenRefs("eta")
	s.scan()
}

// opsOf returns the writes of rep to target name, as wantOps reads them.
func (s *scenario) opsOf(rep report.Delivery, name string) []string {
	var out []string
	for _, op := range rep.Ops {
		if op.Target != "gh:"+s.repos[name] {
			continue
		}
		w := op.Kind
		if op.PR != 0 {
			w += fmt.Sprintf(" #%d", op.PR)
		}
		out = append(out, w)
	}
	return out
}

// teamWorkflow is the workflow a person adds to mu's default branch.
const teamWorkflow = "name: team\non: [push]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo test\n"

// scenario is one hub and its targets on the fake.
type scenario struct {
	t *testing.T
	w *world
	// id is the hub's id, branch its sync branch, fp its fingerprint.
	id, branch, fp string
	topic          string
	// repos are the targets' paths by short name.
	repos map[string]string
	hub   string // the hub's directory
	packs map[string]string
	// dropBeta leaves beta's entry out of targets.yml.
	dropBeta bool
	// outputs are every output of the command line, for the token scan.
	outputs []string
	dir     string
	// fork is the person's pull request from a fork of delta, as opened.
	fork *ghfake.PR
}

// The targets: alpha, gamma, epsilon and kappa carry the hub's topic
// (gamma is not opted in; epsilon is private in an organization whose plan
// refuses drafts there; kappa restricts the creation of touchmark/**
// branches); beta, delta, eta, iota and mu (the extra pack) and theta (the
// ci pack, a workflow) are named in targets.yml (eta requires signed
// commits on its default branch, iota refuses force pushes to
// touchmark/**); noise is opted in but never selected.
var scenarioRepos = []struct {
	name     string
	topic    bool
	optedIn  bool
	noDrafts bool
}{
	{"alpha", true, true, false}, {"beta", false, true, false}, {"gamma", true, false, false}, {"delta", false, true, false},
	{"epsilon", true, true, true}, {"eta", false, true, false}, {"theta", false, true, false}, {"iota", false, true, false},
	{"kappa", true, true, false}, {"mu", false, true, false}, {"noise", false, true, false},
}

// extraTargets get the extra pack.
var extraTargets = []string{"delta", "eta", "iota", "mu"}

// newScenario sets the targets up on a new fake GHES, with web commit
// signing on or off.
func newScenario(t *testing.T, signing bool) *scenario {
	t.Helper()
	w := newWorld(t, worldOptions{flavor: ghfake.GHES, webCommitSigning: &signing, writerPerms: ghfake.Permissions{
		"contents": ghfake.Write, "pull_requests": ghfake.Write, "metadata": ghfake.Read,
	}})
	s := &scenario{
		t: t, w: w,
		id:    "e2e-hub",
		topic: "touchmark-e2e",
		fp:    w.host + "/424242",
		repos: map[string]string{},
		packs: map[string]string{
			"packs/base/AGENTS.md":                text("base AGENTS.md v1"),
			"packs/base/docs/guide.md":            text("base guide v1"),
			"packs/extra/docs/extra.md":           text("extra v1"),
			"packs/ci/.github/workflows/lint.yml": "name: lint\non: [pull_request]\njobs:\n  lint:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo lint\n",
		},
		dir: t.TempDir(),
	}
	s.branch = "touchmark/" + s.id
	for _, r := range scenarioRepos {
		spec := ghfake.RepoSpec{Owner: org, Name: r.name, Files: []ghfake.File{{Path: "README.md", Content: []byte("# " + r.name + "\n")}}}
		if r.optedIn {
			spec.Files = append(spec.Files, ghfake.File{Path: config.DefaultOptIn, Content: []byte("version: 1\n")})
		}
		if r.topic {
			spec.Topics = []string{s.topic}
		}
		if r.noDrafts {
			spec.Visibility, spec.NoDrafts = "private", true
		}
		repo := try(w.srv.CreateRepo(spec)).of(t)
		check(t, w.srv.Grant(repo.FullName, person, "write"))
		s.repos[r.name] = repo.FullName
	}
	try(w.srv.AddRuleset(s.repos["eta"], ghfake.Ruleset{Name: "signed", Include: []string{"~DEFAULT_BRANCH"},
		Rules: []ghfake.Rule{{Type: ghfake.RuleRequiredSignatures}}})).of(t)
	try(w.srv.AddRuleset(s.repos["iota"], ghfake.Ruleset{Name: "no force", Include: []string{"refs/heads/touchmark/**"},
		Rules: []ghfake.Rule{{Type: ghfake.RuleNonFastForward}}})).of(t)
	try(w.srv.AddRuleset(s.repos["kappa"], ghfake.Ruleset{Name: "no touchmark branches", Include: []string{"refs/heads/touchmark/**"},
		Rules: []ghfake.Rule{{Type: ghfake.RuleCreation}}})).of(t)
	s.hub = t.TempDir()
	s.git(s.hub, "init", "-q", "-b", "master")
	s.commitHub("the hub")
	t.Cleanup(func() {
		s.w.violations()
		s.checkTokens()
	})
	return s
}

// needDistributeGit skips on a git older than distribute supports: the
// Docker run of the suite (git 2.47) covers the test.
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

// git runs git in dir with an isolated configuration.
func (s *scenario) git(dir string, args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=hub", "GIT_AUTHOR_EMAIL=hub@example.com", "GIT_COMMITTER_NAME=hub", "GIT_COMMITTER_EMAIL=hub@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitHub writes the hub's configuration and packs and commits them.
func (s *scenario) commitHub(msg string) {
	s.t.Helper()
	hub := fmt.Sprintf("version: 1\nid: %s\nproviders:\n  - id: gh\n    type: github\n    url: %s\n    writer: %s\npr:\n  title: \"chore: sync engineering assets\"\n  draft: true\n",
		s.id, s.w.srv.URL(), s.w.writeApp.Bot.Login)
	targets := fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: %s\n    topics: [%s]\n  - repo: %s\n    packs: [ci]\n",
		org, s.topic, s.repos["theta"])
	for _, name := range extraTargets {
		targets += fmt.Sprintf("  - repo: %s\n    packs: [extra]\n", s.repos[name])
	}
	if !s.dropBeta {
		targets += fmt.Sprintf("  - repo: %s\n", s.repos["beta"])
	}
	files := map[string]string{config.HubFile: hub, config.TargetsFile: targets}
	for p, content := range s.packs {
		files[p] = content
	}
	for p, content := range files {
		full := filepath.Join(s.hub, filepath.FromSlash(p))
		check(s.t, os.MkdirAll(filepath.Dir(full), 0o755))
		check(s.t, os.WriteFile(full, []byte(content), 0o644))
	}
	s.git(s.hub, "add", "-A")
	s.git(s.hub, "commit", "-q", "-m", msg)
}

// hubHead returns the hub's HEAD commit.
func (s *scenario) hubHead() string { return s.git(s.hub, "rev-parse", "HEAD") }

// text returns distinct content of a pack file whose first line is label,
// at least 64 bytes long: check refuses a smaller pack file.
func text(label string) string {
	return label + "\nshared engineering asset, kept in sync by touchmark\nmore shared content\n"
}

// ciVars are the variables that would make the command line think it runs
// in CI; the scenario is a maintainer's local run.
var ciVars = []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "GITEA_ACTIONS", "FORGEJO_ACTIONS"}

// run runs the command line in process with vars in the environment and
// fails the test unless it exits with code.
func (s *scenario) run(code int, vars map[string]string, args ...string) string {
	s.t.Helper()
	for _, name := range ciVars {
		s.t.Setenv(name, "")
	}
	for k, v := range vars {
		s.t.Setenv(k, v)
	}
	var stdout, stderr bytes.Buffer
	got := cli.Main(context.Background(), args, &stdout, &stderr)
	s.outputs = append(s.outputs, stdout.String(), stderr.String())
	if got != code {
		s.t.Fatalf("touchmark %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, code,
			s.w.reg.Replace(stdout.String()), s.w.reg.Replace(stderr.String()))
	}
	return stdout.String()
}

// appVars are the variables of an App credential of the provider gh.
func appVars(role string, app ghfake.App, key []byte) map[string]string {
	return map[string]string{
		"TOUCHMARK_GH_" + role + "_APP_ID":  strconv.FormatInt(app.ID, 10),
		"TOUCHMARK_GH_" + role + "_APP_KEY": string(key),
	}
}

// plan runs plan with the reader App.
func (s *scenario) plan() report.Delivery {
	s.t.Helper()
	out := s.run(0, appVars("READ", s.w.readApp, s.w.readKey), "plan", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json")
	return s.decode(out)
}

// distribute runs distribute with the writer App, with a report file and a
// stream, both kept for the token scan; every write must be the bot's.
func (s *scenario) distribute() report.Delivery {
	s.t.Helper()
	reportFile, stream := filepath.Join(s.dir, "report.json"), filepath.Join(s.dir, "stream.jsonl")
	out := s.run(0, appVars("WRITE", s.w.writeApp, s.w.writeKey),
		"distribute", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json", "--report", reportFile, "--stream", stream)
	for _, name := range []string{reportFile, stream} {
		data, err := os.ReadFile(name)
		check(s.t, err)
		s.outputs = append(s.outputs, string(data))
	}
	rep := s.decode(out)
	for _, op := range rep.Ops {
		if op.Account != s.w.writeApp.Bot.Login {
			s.t.Errorf("op %s %s by %q, want the writer %s", op.Target, op.Kind, op.Account, s.w.writeApp.Bot.Login)
		}
	}
	return rep
}

func (s *scenario) decode(out string) report.Delivery {
	s.t.Helper()
	validate(s.t, "report", []byte(out), s.w.reg.Replace)
	var rep report.Delivery
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		s.t.Fatalf("decode the report: %v\n%s", err, s.w.reg.Replace(out))
	}
	return rep
}

// settle runs distribute, checks the outcomes of the targets in want
// ("outcome:reason #pr"), and then that a second distribute writes nothing,
// by its report and on the fake. It returns the first run's report.
func (s *scenario) settle(what string, want map[string]string) report.Delivery {
	s.t.Helper()
	rep := s.distribute()
	s.want(what, rep, want)
	s.w.violations()
	before := s.snapshot()
	again := s.distribute()
	if len(again.Ops) > 0 {
		var ops []string
		for _, op := range again.Ops {
			ops = append(ops, op.Target+" "+op.Kind)
		}
		s.t.Errorf("%s: a second distribute wrote %q (%s)", what, ops, render(again))
	}
	if after := s.snapshot(); !maps2Equal(before, after) {
		s.t.Errorf("%s: a second distribute changed the fake:\nbefore %v\nafter  %v", what, before, after)
	}
	s.w.violations()
	s.checkFork()
	s.checkTokens()
	return rep
}

// want checks the outcomes of the targets named in want, by short name;
// noise must never be a target, and no target may fail.
func (s *scenario) want(what string, rep report.Delivery, want map[string]string) {
	s.t.Helper()
	got := outcomes(rep)
	for name, w := range want {
		if g := got[s.repos[name]]; g != w {
			s.t.Errorf("%s: %s is %q, want %q (%s)", what, name, g, w, render(rep))
		}
	}
	if _, ok := got[s.repos["noise"]]; ok {
		s.t.Errorf("%s: noise is a target, but no entry of targets.yml selects it", what)
	}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeFailed {
			s.t.Errorf("%s: %s failed: %q", what, tg.Path, tg.Warnings)
		}
	}
}

// outcomes returns "outcome:reason #pr" by target path.
func outcomes(rep report.Delivery) map[string]string {
	out := map[string]string{}
	for _, tg := range rep.Targets {
		v := string(tg.Outcome) + ":" + tg.Reason
		if tg.PR != nil {
			v += fmt.Sprintf(" #%d", tg.PR.Number)
		}
		out[tg.Path] = v
	}
	return out
}

func render(rep report.Delivery) string {
	var parts []string
	for _, tg := range rep.Targets {
		parts = append(parts, fmt.Sprintf("%s %s:%s %q", tg.Path, tg.Outcome, tg.Reason, tg.Warnings))
	}
	return strings.Join(parts, ", ")
}

func maps2Equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// wantOps checks the writes of rep by target: exactly want[name] for the
// targets it names, in the order of the journal, and none for the others.
// A write reads "<kind>", with " #<n>" when it is to pull request n.
func (s *scenario) wantOps(what string, rep report.Delivery, want map[string][]string) {
	s.t.Helper()
	byTarget := map[string]string{}
	for name, p := range s.repos {
		byTarget["gh:"+p] = name
	}
	got := map[string][]string{}
	for _, op := range rep.Ops {
		name, ok := byTarget[op.Target]
		if !ok {
			name = op.Target
		}
		w := op.Kind
		if op.PR != 0 {
			w += fmt.Sprintf(" #%d", op.PR)
		}
		got[name] = append(got[name], w)
	}
	for name, ops := range got {
		if _, ok := want[name]; !ok {
			s.t.Errorf("%s: %s got the writes %q, want none", what, name, ops)
		}
	}
	for name, ops := range want {
		if !slices.Equal(got[name], ops) {
			s.t.Errorf("%s: %s got the writes %q, want %q", what, name, got[name], ops)
		}
	}
}

// countPRs counts the pull requests of every repository.
func (s *scenario) countPRs() int {
	n := 0
	for _, p := range s.repos {
		n += len(s.w.srv.PRs(p))
	}
	return n
}

// pr returns pull request n of target name.
func (s *scenario) pr(name string, n int64) ghfake.PR {
	s.t.Helper()
	pr, ok := s.w.srv.GetPR(s.repos[name], n)
	if !ok {
		s.t.Fatalf("%s has no pull request #%d", name, n)
	}
	return pr
}

// snapshot describes every pull request of the targets, their comments
// and the sync branches, to tell whether anything was written.
func (s *scenario) snapshot() map[string]string {
	out := map[string]string{}
	for name, p := range s.repos {
		for _, pr := range s.w.srv.PRs(p) {
			sum := sha256.Sum256([]byte(pr.Title + "\x00" + pr.Body))
			labels := slices.Clone(pr.Labels)
			slices.Sort(labels)
			out[fmt.Sprintf("%s#%d", name, pr.Number)] = fmt.Sprintf("%s merged %v draft %v head %s comments %d labels %v text %x",
				pr.State, pr.Merged, pr.Draft, pr.HeadSHA, len(s.w.srv.Comments(p, pr.Number)), labels, sum[:6])
		}
		if head := s.w.srv.Branch(p, s.branch); head != "" {
			out[name+" "+s.branch] = head
		}
	}
	return out
}

// api sends a REST request to the fake as the person and decodes a 2xx
// JSON answer into out.
func (s *scenario) api(method, path string, out any) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.w.srv.EnterpriseAPIURL()+path, nil)
	check(s.t, err)
	req.Header.Set("Authorization", "Bearer "+s.w.personToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.w.srv.Client().Do(req)
	check(s.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	check(s.t, err)
	if resp.StatusCode/100 != 2 {
		s.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, body)
	}
	if out != nil {
		check(s.t, json.Unmarshal(body, out))
	}
}

// file returns the content of path at ref in target name.
func (s *scenario) file(name, ref, p string) string {
	s.t.Helper()
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	s.api(http.MethodGet, "/repos/"+s.repos[name]+"/contents/"+p+"?ref="+url.QueryEscape(ref), &f)
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil || f.Encoding != "base64" {
		s.t.Fatalf("%s of %s at %s: encoding %q: %v", p, name, ref, f.Encoding, err)
	}
	return string(data)
}

// apiCommit is a Git Data commit.
type apiCommit struct {
	SHA    string `json:"sha"`
	Author struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
	Committer struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"committer"`
	Message string `json:"message"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Verification struct {
		Verified bool   `json:"verified"`
		Reason   string `json:"reason"`
	} `json:"verification"`
}

// commit returns commit sha of target name.
func (s *scenario) commit(name, sha string) apiCommit {
	s.t.Helper()
	var c apiCommit
	s.api(http.MethodGet, "/repos/"+s.repos[name]+"/git/commits/"+sha, &c)
	return c
}

// checkOpened checks target name's first pull request against the report
// and what touchmark promises: the sync branch of the target itself to the
// default branch, by the writer's bot, with the title, the label, a body
// that lists the files and ends in the hub's marker, and the pack files on
// the branch.
func (s *scenario) checkOpened(name string, rep report.Delivery) {
	s.t.Helper()
	pr := s.pr(name, 1)
	var tg report.DeliveryTarget
	for _, x := range rep.Targets {
		if x.Path == s.repos[name] {
			tg = x
		}
	}
	repo, _ := s.w.srv.Repo(s.repos[name])
	switch {
	case tg.PR == nil || tg.PR.Number != pr.Number:
		s.t.Errorf("%s: the report names %+v, the fake has #%d", name, tg.PR, pr.Number)
	case pr.State != "open":
		s.t.Errorf("%s #%d: %s, want open", name, pr.Number, pr.State)
	case pr.Head != s.branch || pr.HeadRepoID != repo.ID || pr.Base != "main":
		s.t.Errorf("%s #%d: %s (repository %d) → %s, want %s → main in the target itself", name, pr.Number, pr.Head, pr.HeadRepoID, pr.Base, s.branch)
	case pr.Author.ID != s.w.writeApp.Bot.ID:
		s.t.Errorf("%s #%d: opened by %s, want %s", name, pr.Number, pr.Author.Login, s.w.writeApp.Bot.Login)
	case pr.Title != "chore: sync engineering assets":
		s.t.Errorf("%s #%d: title %q", name, pr.Number, pr.Title)
	case !slices.Contains(pr.Labels, "engineering-assets"):
		s.t.Errorf("%s #%d: labels %v, want engineering-assets", name, pr.Number, pr.Labels)
	}
	m, status := marker.Find(pr.Body, []string{s.fp})
	wantPacks := []string{"base"}
	files := []string{"AGENTS.md", "docs/guide.md"}
	if slices.Contains(extraTargets, name) {
		wantPacks = []string{"base", "extra"}
		files = append(files, "docs/extra.md")
	}
	switch {
	case status != marker.Found:
		s.t.Errorf("%s #%d: marker %s in the body:\n%s", name, pr.Number, status, pr.Body)
	case m.Data.Hub != s.id || m.Data.FP != s.fp || m.Stream != "sync" || m.Data.ContentCommit != s.hubHead():
		s.t.Errorf("%s #%d: marker hub %s fp %s stream %s content %s", name, pr.Number, m.Data.Hub, m.Data.FP, m.Stream, m.Data.ContentCommit)
	case !slices.Equal(sorted(m.Data.Packs), wantPacks):
		s.t.Errorf("%s #%d: marker packs %v, want %v", name, pr.Number, m.Data.Packs, wantPacks)
	case !strings.HasPrefix(lastLine(pr.Body), "<!-- touchmark:v1 "):
		s.t.Errorf("%s #%d: the marker is not the last line of the body", name, pr.Number)
	}
	for _, f := range files {
		if !strings.Contains(pr.Body, "`"+f+"`") {
			s.t.Errorf("%s #%d: the body does not list %s:\n%s", name, pr.Number, f, pr.Body)
		}
		if got, want := s.file(name, s.branch, f), s.packs[packPath(f)]; got != want {
			s.t.Errorf("%s: %s on %s is %q, want %q", name, f, s.branch, got, want)
		}
	}
	s.checkCommit(name, pr.HeadSHA, name == "eta")
}

// packPath returns the hub path of target file f.
func packPath(f string) string {
	if f == "docs/extra.md" {
		return "packs/extra/" + f
	}
	return "packs/base/" + f
}

// checkCommit checks the head commit of a pull request of touchmark's: one
// commit on the default branch's tip, with touchmark's trailers, authored
// by the writer's bot; committed by the bot when touchmark pushed it, by
// GitHub when GitHub made it through the API (api).
func (s *scenario) checkCommit(name, sha string, api bool) {
	s.t.Helper()
	c := s.commit(name, sha)
	main := s.w.srv.Branch(s.repos[name], "main")
	bot := s.w.writeApp.Bot.Login
	msg := c.Message
	switch {
	case c.Author.Name != bot || !strings.HasSuffix(c.Author.Email, "+"+bot+"@users.noreply."+hostName(s.w.host)):
		s.t.Errorf("%s %s: author %s <%s>, want the bot %s with its noreply address", name, sha, c.Author.Name, c.Author.Email, bot)
	case !api && (c.Committer.Name != c.Author.Name || c.Committer.Email != c.Author.Email):
		s.t.Errorf("%s %s: committer %s <%s>, author %s <%s>", name, sha, c.Committer.Name, c.Committer.Email, c.Author.Name, c.Author.Email)
	case api && c.Committer.Name != "GitHub":
		s.t.Errorf("%s %s: committer %s <%s>, want GitHub (an API commit)", name, sha, c.Committer.Name, c.Committer.Email)
	case len(c.Parents) != 1 || c.Parents[0].SHA != main:
		s.t.Errorf("%s %s: parents %v, want the tip of main %s", name, sha, c.Parents, main)
	case !strings.Contains(msg, "\nTouchmark-Hub: "+s.id+"@"+s.fp+"\n") || !strings.Contains(msg, "\nTouchmark-Content: sha256:") ||
		!strings.Contains(msg, "\nTouchmark-Hub-Commit: "+s.hubHead()):
		s.t.Errorf("%s %s: message without touchmark's trailers:\n%s", name, sha, msg)
	}
}

// noHiddenRefs checks that no ref under refs/touchmark/ is left in target
// name: the stage ref of an API commit is deleted with the branch update.
func (s *scenario) noHiddenRefs(name string) {
	s.t.Helper()
	if refs := s.git(s.w.srv.GitDir(s.repos[name]), "for-each-ref", "--format=%(refname)", "refs/touchmark/"); refs != "" {
		s.t.Errorf("%s keeps hidden refs: %s", name, refs)
	}
}

// hostName is host without its port.
func hostName(host string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// checkSigned checks that commit sha of target name carries GitHub's
// verified signature.
func (s *scenario) checkSigned(name, sha string) {
	s.t.Helper()
	if c := s.commit(name, sha); !c.Verification.Verified || c.Verification.Reason != "valid" {
		s.t.Errorf("%s %s: verified %v (%s), want a commit GitHub signed", name, sha, c.Verification.Verified, c.Verification.Reason)
	}
}

// checkDeclined checks what distribute writes when it first sees a decline:
// ack in the marker and one comment by the writer's bot.
func (s *scenario) checkDeclined(name string, n int64) {
	s.t.Helper()
	m, status := marker.Find(s.pr(name, n).Body, []string{s.fp})
	if status != marker.Found || !m.Data.Ack {
		s.t.Errorf("%s #%d: marker %s, ack %v; want an acknowledged decline", name, n, status, m.Data.Ack)
	}
	comments := s.w.srv.Comments(s.repos[name], n)
	if len(comments) != 1 || comments[0].Author.ID != s.w.writeApp.Bot.ID {
		s.t.Errorf("%s #%d: %d comments, want one by the writer's bot", name, n, len(comments))
	}
}

// forkPR opens, as the person, a pull request into target name from a fork
// on a branch named like the sync branch, with a copy of the body of
// touchmark's pull request and its marker (threat T9).
func (s *scenario) forkPR(name string) {
	s.t.Helper()
	fork := try(s.w.srv.Fork(s.repos[name], person)).of(s.t)
	try(s.w.srv.Commit(fork.FullName, ghfake.CommitSpec{Branch: s.branch, Author: person,
		Files: []ghfake.File{{Path: "mine.md", Content: []byte("mine\n")}}, Message: "my own sync"})).of(s.t)
	own := s.pr(name, 1)
	pr := try(s.w.srv.OpenPR(s.repos[name], ghfake.PRSpec{Head: s.branch, HeadRepo: fork.FullName, Title: own.Title,
		Body: own.Body, Author: person})).of(s.t)
	s.fork = &pr
}

// checkFork checks that the fork's pull request is as it was opened.
func (s *scenario) checkFork() {
	s.t.Helper()
	if s.fork == nil {
		return
	}
	now := s.pr("delta", s.fork.Number)
	if now.State != "open" || now.Body != s.fork.Body || now.Title != s.fork.Title || now.HeadSHA != s.fork.HeadSHA ||
		len(s.w.srv.Comments(s.repos["delta"], now.Number)) != 0 || len(now.Labels) != len(s.fork.Labels) {
		s.t.Errorf("the fork's pull request #%d was touched: %+v", now.Number, now)
	}
}

// checkTokens checks the writer's installation tokens: every token
// narrowed to repositories is narrowed to one and revoked once its run
// ended; none was used on another repository (the journal's token-scope);
// only theta's and mu's carry workflows write (theta's pack
// changes a workflow, mu's rebuild moves its branch across a person's
// workflow), as least privilege asks, and every reading token of the
// writer was revoked when its run closed the driver.
func (s *scenario) checkTokens() {
	s.t.Helper()
	workflowRepos := map[int64]string{}
	for _, name := range []string{"theta", "mu"} {
		if r, ok := s.w.srv.Repo(s.repos[name]); ok {
			workflowRepos[r.ID] = name
		}
	}
	narrowed := 0
	for _, tok := range s.w.srv.Tokens(s.w.writeInst.ID) {
		if tok.Repos == nil {
			if !tok.Revoked {
				s.t.Errorf("a reading token of the writer (permissions %v) outlived its run", tok.Permissions)
			}
			continue
		}
		narrowed++
		if len(tok.Repos) != 1 || !tok.Revoked {
			s.t.Errorf("a per-target token: repositories %v, revoked %v; want one repository, revoked", tok.Repos, tok.Revoked)
		}
		if tok.Permissions["workflows"] != "" && workflowRepos[tok.Repos[0]] == "" {
			s.t.Errorf("a per-target token of repository %d has workflows %q: only a target whose writes change workflows needs it",
				tok.Repos[0], tok.Permissions["workflows"])
		}
	}
	if narrowed == 0 && len(s.w.srv.PRs(s.repos["alpha"])) > 0 {
		s.t.Errorf("the writer wrote without a per-target token")
	}
}

// graphQL reports whether a GraphQL request with root field root was sent.
func (s *scenario) graphQL(root string) bool {
	for _, r := range s.w.srv.Requests() {
		if strings.HasPrefix(r.Route, "POST /graphql") && strings.Contains(r.Route, root) {
			return true
		}
	}
	return false
}

// scan checks that no secret reached any output of the command line, a
// pull request, a comment or a commit message of the targets.
func (s *scenario) scan() {
	s.t.Helper()
	// Every token the fake minted during the run is a secret too.
	for _, inst := range []int64{s.w.readInst.ID, s.w.writeInst.ID} {
		for _, tok := range s.w.srv.Tokens(inst) {
			s.w.reg.Add(tok.Value, "x-access-token")
		}
	}
	for i, out := range s.outputs {
		if s.w.reg.Contains(out) {
			s.t.Errorf("output %d of the command line holds a secret", i)
		}
	}
	for name, p := range s.repos {
		for _, pr := range s.w.srv.PRs(p) {
			if s.w.reg.Contains(pr.Title + "\n" + pr.Body) {
				s.t.Errorf("%s #%d holds a secret", name, pr.Number)
			}
			for _, c := range s.w.srv.Comments(p, pr.Number) {
				if s.w.reg.Contains(c.Body) {
					s.t.Errorf("a comment on %s #%d holds a secret", name, pr.Number)
				}
			}
			if pr.HeadSHA != "" {
				if c := s.commit(name, pr.HeadSHA); s.w.reg.Contains(c.Message) {
					s.t.Errorf("the head commit of %s #%d holds a secret", name, pr.Number)
				}
			}
		}
	}
}

func sorted(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return out
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
