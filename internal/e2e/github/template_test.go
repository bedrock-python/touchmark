package githube2e

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestTemplateWorkflow is a dry run of the hub template's GitHub workflow,
// .github/workflows/engineering-assets.yml of the template's working tree
// in TOUCHMARK_E2E_TEMPLATE, against the fake as a GitHub Enterprise
// Server: no GitHub, no act. It plays the runner's part for the events
// the workflow takes, job by job, as GitHub runs them:
//
//   - which jobs run: each job's if, evaluated with the event's github
//     context, the secrets and variables the job sees (the repository's,
//     and its environment's when it names one), and needs (a job whose
//     needs did not succeed is skipped);
//   - each job's GITHUB_TOKEN: a token of an App installed on the hub
//     alone, with the job's permissions (none outside them);
//   - environment: touchmark-distribute admits the default branch only,
//     as its deployment branch policy says;
//   - the steps: actions/checkout clones the hub (a pull request's
//     merge commit, as refs/pull/<n>/merge), run steps run in bash with
//     GITHUB_OUTPUT, and the touchmark Action runs this working tree's
//     scripts/action/run.sh with the step's inputs and env, as the Action
//     at a release would, with a docker on PATH that runs touchmark, built
//     from this tree, with the environment and as the user run.sh gives
//     the container; actions/upload-artifact checks that its files exist.
//
// The hub is the template with id, writer and targets set (README "On
// GitHub", step 5); the reader's App id and key are the repository's
// variable and secret, the writer's those of the environment. The runs:
// the pull request that describes the hub (check and plan, which keeps its
// report in a comment), its merge (probe and distribute, which opens the
// pull requests), the two schedules, Run workflow; Dependabot's pull
// request and one from a fork, which get no Actions secrets (Dependabot's
// read-only token may be raised by permissions, a fork's may not): plan
// runs offline and passes without --strict; and then a write key leaked
// into a repository secret: the plan job's probe step fails, and
// distribute refuses.
//
// It runs on Linux (the Action needs a Linux runner) with bash and a git
// that distribute supports: in the Docker run of the suite, with
// TOUCHMARK_E2E_TEMPLATE set; docs/project/e2e.md shows the command.
func TestTemplateWorkflow(t *testing.T) {
	dir := os.Getenv("TOUCHMARK_E2E_TEMPLATE")
	if dir == "" {
		t.Skip("TOUCHMARK_E2E_TEMPLATE is not set: it names the hub template's working tree to dry-run")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the touchmark Action runs on Linux runners only")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	needDistributeGit(t)
	for _, name := range ciVars {
		t.Setenv(name, "")
	}
	tpl := readTemplateDir(t, dir)
	var wf workflow
	if err := yaml.Unmarshal(tpl[".github/workflows/engineering-assets.yml"], &wf); err != nil {
		t.Fatalf("parse the workflow: %v", err)
	}
	r := newWorkflowRunner(t, bash, tpl, &wf)

	// The pull request that describes the hub.
	pr := r.openDescribePR()
	run := r.run("pull_request", map[string]any{"number": pr.Number})
	r.wantJobs("the pull request", run, map[string]string{"check": "success", "plan": "success",
		"probe": "skipped", "distribute": "skipped", "doctor": "skipped"})
	r.wantOutcomes("plan", run.reports["plan"], "opened")
	comments := r.w.srv.Comments(r.hub, pr.Number)
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "alpha") || comments[0].Author.Login != r.actionsBot {
		t.Errorf("the plan comments: %+v, want one by %s that names alpha", comments, r.actionsBot)
	}

	// The merge.
	if _, err := r.w.srv.MergePR(r.hub, pr.Number, ghfake.MergeCommit, person); err != nil {
		t.Fatal(err)
	}
	run = r.run("push", nil)
	r.wantJobs("the merge", run, map[string]string{"check": "skipped", "plan": "skipped",
		"probe": "success", "distribute": "success", "doctor": "skipped"})
	r.wantOutcomes("distribute", run.reports["distribute"], "opened")
	for _, name := range []string{"alpha", "beta"} {
		prs := r.w.srv.PRs(org + "/" + name)
		if len(prs) != 1 || prs[0].Author.Login != r.w.writeApp.Bot.Login || prs[0].Head != "touchmark/"+r.id {
			t.Errorf("%s: pull requests %+v, want one by %s on touchmark/%s", name, prs, r.w.writeApp.Bot.Login, r.id)
		}
	}

	// The schedules and Run workflow.
	run = r.run("schedule", map[string]any{"schedule": "47 4 * * 1"})
	r.wantJobs("the doctor schedule", run, map[string]string{"check": "skipped", "plan": "skipped",
		"probe": "success", "distribute": "skipped", "doctor": "success"})
	run = r.run("schedule", map[string]any{"schedule": "23 3 * * *"})
	r.wantJobs("the daily schedule", run, map[string]string{"check": "skipped", "plan": "skipped",
		"probe": "success", "distribute": "success", "doctor": "skipped"})
	r.wantOutcomes("the daily distribute", run.reports["distribute"], "unchanged")
	run = r.run("workflow_dispatch", nil)
	r.wantJobs("Run workflow", run, map[string]string{"check": "skipped", "plan": "skipped",
		"probe": "success", "distribute": "success", "doctor": "success"})

	// Dependabot's pull request, which bumps the Action, and a pull request
	// from a fork: no secrets, so plan reads no target and passes without
	// --strict, with its warning.
	wfText := r.tpl[".github/workflows/engineering-assets.yml"]
	// The bump moves the version comment of every pin, whichever release the
	// template names; a template without one would make an empty change.
	bumped := actionPinRe.ReplaceAllStringFunc(string(wfText), func(pin string) string {
		m := actionPinRe.FindStringSubmatch(pin)
		patch, _ := strconv.Atoi(m[4])
		return m[1] + m[2] + "." + m[3] + "." + strconv.Itoa(patch+1)
	})
	if bumped == string(wfText) {
		t.Fatal("the template's workflow pins the Action with no # vX.Y.Z comment: the bump would change nothing")
	}
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"Dependabot's pull request", map[string]any{"actor": "dependabot[bot]"}},
		{"a pull request from a fork", map[string]any{"fork": "bob/engineering-assets"}},
	} {
		bump := r.openPR("bump-"+strconv.Itoa(r.n), "chore: bump touchmark", map[string]string{
			".github/workflows/engineering-assets.yml": bumped})
		tc.extra["number"] = bump.Number
		run = r.run("pull_request", tc.extra)
		r.wantJobs(tc.name, run, map[string]string{"check": "success", "plan": "success",
			"probe": "skipped", "distribute": "skipped", "doctor": "skipped"})
		rep := run.reports["plan"]
		switch {
		case rep == nil:
			t.Errorf("%s: no plan report", tc.name)
		case rep.Strict || len(rep.Targets) != 0 ||
			!slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.HasPrefix(w, "offline plan:") }):
			t.Errorf("%s: plan strict %v, targets %+v, warnings %q: want an offline plan without --strict", tc.name, rep.Strict, rep.Targets, rep.Warnings)
		}
	}

	// A write key leaks into a repository secret.
	r.repoSecrets["TOUCHMARK_WRITE_APP_KEY"] = string(r.w.writeKey)
	check(t, r.w.srv.SetSecret(r.hub, "", "TOUCHMARK_WRITE_APP_KEY", false))
	pr2 := r.openPR("leak", "a change", map[string]string{"packs/agents/.agents/prompts/review.md": "a reviewed change of the review prompt, long enough to be a pack file of its own\n"})
	run = r.run("pull_request", map[string]any{"number": pr2.Number})
	r.wantJobs("a pull request with a leaked write key", run, map[string]string{"check": "success", "plan": "failure",
		"probe": "skipped", "distribute": "skipped", "doctor": "skipped"})
	if !strings.Contains(run.logs["plan"], "a write key is visible outside the environment") {
		t.Errorf("the plan job's log does not show the probe's error:\n%s", r.w.reg.Replace(run.logs["plan"]))
	}
	run = r.run("workflow_dispatch", nil)
	r.wantJobs("Run workflow with a leaked write key", run, map[string]string{"check": "skipped", "plan": "skipped",
		"probe": "success", "distribute": "failure", "doctor": "failure"})
	if !strings.Contains(run.logs["distribute"], "TOUCHMARK_KEY_EXPOSED=true") {
		t.Errorf("distribute's log does not name the probe's answer:\n%s", r.w.reg.Replace(run.logs["distribute"]))
	}
	r.w.violations()
	r.noSecrets()
}

// readTemplateDir reads the template's working tree: every regular file but
// those under .git, by slash path.
func readTemplateDir(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("read the template: %v", err)
	}
	return files
}

// The workflow, as far as the dry run reads it.
type workflow struct {
	Jobs yaml.Node `yaml:"jobs"`
}

type wfJob struct {
	If          string            `yaml:"if"`
	Needs       yaml.Node         `yaml:"needs"`
	Environment yaml.Node         `yaml:"environment"`
	Permissions yaml.Node         `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []wfStep          `yaml:"steps"`
}

type wfStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

// namedJob is a job with its id, in file order.
type namedJob struct {
	id  string
	job wfJob
}

func (wf *workflow) jobs(t *testing.T) []namedJob {
	t.Helper()
	var out []namedJob
	for i := 0; i+1 < len(wf.Jobs.Content); i += 2 {
		var j wfJob
		if err := wf.Jobs.Content[i+1].Decode(&j); err != nil {
			t.Fatalf("job %s: %v", wf.Jobs.Content[i].Value, err)
		}
		out = append(out, namedJob{id: wf.Jobs.Content[i].Value, job: j})
	}
	return out
}

// strings of a node that is a string or a list of strings.
func nodeStrings(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	}
	return nil
}

// environmentName is a job's environment: a name or {name: …}.
func environmentName(n yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "name" {
				return n.Content[i+1].Value
			}
		}
	}
	return ""
}

// workflowRunner holds the fake GitHub, the hub on it and what a runner
// needs.
type workflowRunner struct {
	t      *testing.T
	w      *world
	bash   string
	tpl    map[string][]byte
	wf     *workflow
	hub    string
	id     string
	repoID int64
	// actionsInst is the hub's installation of an App that stands for
	// GitHub Actions: it mints each job's GITHUB_TOKEN. actionsBot is its
	// bot.
	actionsInst ghfake.Installation
	actionsBot  string
	// The secrets and variables of the repository and of the environment
	// touchmark-distribute, by name.
	repoSecrets, repoVars, envSecrets, envVars map[string]string
	// bin holds the fake docker; touchmark is the binary under test.
	bin, touchmark string
	// actionInputs are the defaults of action.yml's inputs.
	actionInputs map[string]string
	// outputs collects every log for the secret scan.
	outputs []string
	n       int
}

func newWorkflowRunner(t *testing.T, bash string, tpl map[string][]byte, wf *workflow) *workflowRunner {
	t.Helper()
	w := newWorld(t, worldOptions{flavor: ghfake.GHES, selected: true})
	r := &workflowRunner{t: t, w: w, bash: bash, tpl: tpl, wf: wf, hub: org + "/engineering-assets", id: "acme-assets",
		repoSecrets: map[string]string{}, repoVars: map[string]string{}, envSecrets: map[string]string{}, envVars: map[string]string{}}

	// The targets, with the writer installed on them only.
	for _, name := range []string{"alpha", "beta"} {
		try(w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: name, Visibility: "private", Files: []ghfake.File{
			{Path: "README.md", Content: []byte("# " + name + "\n")},
			{Path: config.DefaultOptIn, Content: []byte("version: 1\n")},
		}})).of(t)
		w.selectRepo(name)
	}
	// The hub: the template, as Use this template makes it.
	var files []ghfake.File
	for p, content := range tpl {
		files = append(files, ghfake.File{Path: p, Content: content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	repo := try(w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: "engineering-assets", Visibility: "private", Files: files})).of(t)
	r.repoID = repo.ID
	keys, err := appKeys()
	check(t, err)
	actions := try(w.srv.RegisterApp(ghfake.AppSpec{Slug: "github-actions", Owner: org, PublicKey: &keys[0].key.PublicKey,
		Permissions: ghfake.Permissions{"contents": ghfake.Write, "pull_requests": ghfake.Write, "issues": ghfake.Write,
			"actions": ghfake.Read, "metadata": ghfake.Read}})).of(t)
	r.actionsBot = actions.Bot.Login
	r.actionsInst = try(w.srv.Install(ghfake.InstallSpec{App: "github-actions", Account: org, Repos: []string{"engineering-assets"}})).of(t)

	// README "On GitHub", step 3: the reader's id and key in the
	// repository, the writer's in the environment, which admits the default
	// branch only.
	r.repoVars["TOUCHMARK_READ_APP_ID"] = strconv.FormatInt(w.readApp.ID, 10)
	r.repoSecrets["TOUCHMARK_READ_APP_KEY"] = string(w.readKey)
	r.envVars["TOUCHMARK_WRITE_APP_ID"] = strconv.FormatInt(w.writeApp.ID, 10)
	r.envSecrets["TOUCHMARK_WRITE_APP_KEY"] = string(w.writeKey)
	check(t, w.srv.SetSecret(r.hub, "", "TOUCHMARK_READ_APP_KEY", false))
	check(t, w.srv.SetVariable(r.hub, "", "TOUCHMARK_READ_APP_ID", r.repoVars["TOUCHMARK_READ_APP_ID"]))
	check(t, w.srv.SetEnvironment(r.hub, "touchmark-distribute", ghfake.EnvironmentSpec{
		Policies: []string{"branch:" + repo.DefaultBranch}, Secrets: []string{"TOUCHMARK_WRITE_APP_KEY"},
		Variables: map[string]string{"TOUCHMARK_WRITE_APP_ID": r.envVars["TOUCHMARK_WRITE_APP_ID"]},
	}))

	// touchmark from this working tree, and the docker the Action finds.
	tmp := t.TempDir()
	r.touchmark = filepath.Join(tmp, "touchmark")
	build := exec.Command("go", "build", "-o", r.touchmark, "./cmd/touchmark")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	r.bin = filepath.Join(tmp, "bin")
	check(t, os.MkdirAll(r.bin, 0o755))
	check(t, os.WriteFile(filepath.Join(r.bin, "docker"), []byte(fakeDocker), 0o755))
	check(t, os.WriteFile(filepath.Join(r.bin, "gh"), []byte(fakeGH), 0o755))
	r.actionInputs = actionDefaults(t)
	return r
}

// moduleRoot is the directory of go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// actionDefaults reads the defaults of action.yml's inputs.
func actionDefaults(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Inputs map[string]struct {
			Default string `yaml:"default"`
		} `yaml:"inputs"`
	}
	if err := yaml.Unmarshal(data, &a); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, in := range a.Inputs {
		out[name] = in.Default
	}
	return out
}

// fakeRelease is the release the Action's step runs in the dry run: its
// version, as release-please writes it into action.yml, and the digest its
// version tag resolves to.
const (
	fakeReleaseVersion = "0.1.0"
	fakeReleaseDigest  = "sha256:abababababababababababababababababababababababababababababababab"
)

// fakeDocker is the docker the Action's run.sh finds on PATH: imagetools
// inspect resolves the release's version tag to its digest, pull succeeds,
// image inspect gives the release's version label; run checks the image
// and the user, then runs touchmark ($FAKE_DOCKER_TOUCHMARK) in the working
// directory with only the environment run.sh names (--env NAME takes the
// value of this environment, --env NAME=VALUE the value).
const fakeDocker = `#!/usr/bin/env bash
set -euo pipefail
image=ghcr.io/bedrock-python/touchmark
case "$1 ${2-}" in
"buildx imagetools")
	[ "$3 $4" = "inspect $image:` + fakeReleaseVersion + `" ] || { echo "fake docker: unexpected buildx $*" >&2; exit 2; }
	printf '%s' ` + fakeReleaseDigest + `
	exit 0
	;;
"pull "*) exit 0 ;;
"image inspect") echo v` + fakeReleaseVersion + `; exit 0 ;;
"run "*) shift ;;
*) echo "fake docker: unexpected command $*" >&2; exit 2 ;;
esac
envs=() workdir= user=
while [ $# -gt 0 ]; do
	case $1 in
	--rm | --init) shift ;;
	--user) user=$2; shift 2 ;;
	--workdir) workdir=$2; shift 2 ;;
	--cap-drop | --security-opt | --volume) shift 2 ;;
	--env)
		case $2 in
		*=*) envs+=("$2") ;;
		*) if [ -n "${!2+x}" ]; then envs+=("$2=${!2}"); fi ;;
		esac
		shift 2
		;;
	-*) echo "fake docker: unexpected option $1" >&2; exit 2 ;;
	*) break ;;
	esac
done
ref=$1
shift
[ "$ref" = "$image@` + fakeReleaseDigest + `" ] || { echo "fake docker: runs $ref, not the release's digest" >&2; exit 2; }
[ "$user" = "$(id -u):$(id -g)" ] || { echo "fake docker: --user $user" >&2; exit 2; }
cd "$workdir"
exec env -i PATH="$FAKE_DOCKER_PATH" "${envs[@]}" "$FAKE_DOCKER_TOUCHMARK" "$@"
`

// fakeGH is the gh the Action's run.sh finds on PATH: the attestation of
// the release's digest verifies, given a token.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
[ "$1 $2 $3" = "attestation verify oci://ghcr.io/bedrock-python/touchmark@` + fakeReleaseDigest + `" ] ||
	{ echo "fake gh: unexpected command $*" >&2; exit 2; }
[ -n "${GH_TOKEN:-}" ] || { echo "fake gh: no GH_TOKEN" >&2; exit 4; }
`

// openDescribePR pushes README "On GitHub" step 5 to a branch and opens
// the pull request.
func (r *workflowRunner) openDescribePR() ghfake.PR {
	r.t.Helper()
	hubYML := string(r.tpl[config.HubFile])
	for _, rep := range []struct{ re, new string }{
		{`(?m)^id: change-me$`, "id: " + r.id},
		{`(?m)^# writer: \S+$`, "writer: " + r.w.writeApp.Bot.Login},
	} {
		re := regexp.MustCompile(rep.re)
		if n := len(re.FindAllString(hubYML, -1)); n != 1 {
			r.t.Fatalf("the template's hub.yml has %d lines matching %s, want 1", n, re)
		}
		hubYML = re.ReplaceAllString(hubYML, rep.new)
	}
	targets := fmt.Sprintf("version: 1\ndefaults:\n  packs: [agents]\ntargets:\n  - repo: %s/alpha\n    packs: [python-library]\n  - repo: %s/beta\n", org, org)
	return r.openPR("describe", "Describe the hub", map[string]string{config.HubFile: hubYML, config.TargetsFile: targets})
}

// openPR commits files to a new branch as the person and opens a pull
// request from it.
func (r *workflowRunner) openPR(branch, title string, files map[string]string) ghfake.PR {
	r.t.Helper()
	var list []ghfake.File
	for p, c := range files {
		list = append(list, ghfake.File{Path: p, Content: []byte(c)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	try(r.w.srv.Commit(r.hub, ghfake.CommitSpec{Branch: branch, Files: list, Author: person, Message: title})).of(r.t)
	return try(r.w.srv.OpenPR(r.hub, ghfake.PRSpec{Head: branch, Title: title, Author: person})).of(r.t)
}

// wfRun is the result of one run of the workflow.
type wfRun struct {
	results map[string]string // job → success, failure, skipped
	logs    map[string]string
	// reports are the JSON reports the jobs left (touchmark-report.json).
	reports map[string]*report.Delivery
}

// run runs the workflow for an event: "pull_request" (payload number),
// "push" (the default branch's tip), "schedule" (payload schedule) or
// "workflow_dispatch". extra's actor names who started the run (the person
// by default; dependabot[bot] gets Dependabot's secrets, of which there are
// none); a pull request's fork names the repository its head is in, and
// such a run gets no secrets and a read-only GITHUB_TOKEN.
func (r *workflowRunner) run(event string, extra map[string]any) wfRun {
	r.t.Helper()
	repo, _ := r.w.srv.Repo(r.hub)
	def := repo.DefaultBranch
	payload := map[string]any{"repository": map[string]any{
		"id": r.repoID, "full_name": r.hub, "default_branch": def, "private": true, "visibility": "private",
	}}
	ctx := map[string]string{"event_name": event, "actor": person}
	if a, ok := extra["actor"].(string); ok {
		ctx["actor"] = a
	}
	var ref, sha string
	switch event {
	case "pull_request":
		n := extra["number"].(int64)
		pr, ok := r.w.srv.GetPR(r.hub, n)
		if !ok {
			r.t.Fatalf("no pull request #%d", n)
		}
		headRepo := r.hub
		if fork, ok := extra["fork"].(string); ok {
			headRepo, ctx["fork"] = fork, "true"
		}
		payload["pull_request"] = map[string]any{"number": n,
			"head": map[string]any{"sha": pr.HeadSHA, "ref": pr.Head, "repo": map[string]any{"full_name": headRepo}},
			"base": map[string]any{"sha": r.w.srv.Branch(r.hub, def), "ref": def, "repo": map[string]any{"full_name": r.hub}}}
		payload["number"] = n
		ref = fmt.Sprintf("refs/pull/%d/merge", n)
		ctx["ref_name"] = fmt.Sprintf("%d/merge", n)
		ctx["base_ref"], ctx["head_ref"] = def, pr.Head
		sha = pr.HeadSHA // the merge commit is made at checkout
		ctx["pr_head"], ctx["pr_base"] = pr.HeadSHA, r.w.srv.Branch(r.hub, def)
	default:
		ref, sha = "refs/heads/"+def, r.w.srv.Branch(r.hub, def)
		ctx["ref_name"] = def
		if s, ok := extra["schedule"]; ok {
			payload["schedule"] = s
		}
	}
	ctx["ref"], ctx["sha"] = ref, sha
	r.n++
	runDir := r.t.TempDir()
	eventPath := filepath.Join(runDir, "event.json")
	data, err := json.Marshal(payload)
	check(r.t, err)
	check(r.t, os.WriteFile(eventPath, data, 0o644))

	out := wfRun{results: map[string]string{}, logs: map[string]string{}, reports: map[string]*report.Delivery{}}
	jobOutputs := map[string]map[string]string{}
	for _, nj := range r.wf.jobs(r.t) {
		res, log, outputs, rep := r.runJob(nj, ctx, payload, eventPath, out.results, jobOutputs)
		out.results[nj.id], out.logs[nj.id] = res, log
		jobOutputs[nj.id] = outputs
		if rep != nil {
			out.reports[nj.id] = rep
		}
		r.outputs = append(r.outputs, log)
	}
	return out
}

// runJob runs one job of a run and returns its result, log, outputs and
// JSON report.
func (r *workflowRunner) runJob(nj namedJob, gh map[string]string, payload map[string]any, eventPath string,
	results map[string]string, jobOutputs map[string]map[string]string) (string, string, map[string]string, *report.Delivery) {
	t := r.t
	t.Helper()
	job := nj.job
	needs := nodeStrings(job.Needs)
	for _, n := range needs {
		if results[n] != "success" {
			return "skipped", "", nil, nil
		}
	}
	env := environmentName(job.Environment)
	secrets, vars := map[string]string{}, map[string]string{}
	// Dependabot's runs get Dependabot's secrets (none here) instead of the
	// Actions secrets, and a fork's get none; variables stay.
	noSecrets := gh["actor"] == "dependabot[bot]" || gh["fork"] == "true"
	if !noSecrets {
		for k, v := range r.repoSecrets {
			secrets[k] = v
		}
	}
	for k, v := range r.repoVars {
		vars[k] = v
	}
	if env != "" {
		if !noSecrets {
			for k, v := range r.envSecrets {
				secrets[k] = v
			}
		}
		for k, v := range r.envVars {
			vars[k] = v
		}
	}
	token := r.jobToken(job, gh["fork"] == "true")
	needsCtx := map[string]any{}
	for _, n := range needs {
		outs := map[string]any{}
		for k, v := range jobOutputs[n] {
			outs[k] = v
		}
		needsCtx[n] = map[string]any{"outputs": outs, "result": results[n]}
	}
	ectx := exprContext{
		"github": map[string]any{"event_name": gh["event_name"], "ref_name": gh["ref_name"], "ref": gh["ref"], "sha": gh["sha"],
			"token": token, "event": payload, "repository": r.hub, "actor": gh["actor"]},
		"secrets": toAny(secrets), "vars": toAny(vars), "needs": needsCtx, "steps": map[string]any{},
	}
	if job.If != "" && !truthy(r.eval(job.If, ectx)) {
		return "skipped", "", nil, nil
	}
	var log strings.Builder
	if env != "" {
		// The deployment branch policy of the environment.
		if gh["ref"] != "refs/heads/"+payload["repository"].(map[string]any)["default_branch"].(string) {
			return "failure", fmt.Sprintf("the environment %s does not admit %s", env, gh["ref"]), nil, nil
		}
	}
	ws := r.t.TempDir()
	summary := filepath.Join(r.t.TempDir(), "summary.md")
	check(t, os.WriteFile(summary, nil, 0o644))
	base := map[string]string{
		"CI":                      "true",
		"GITHUB_ACTIONS":          "true",
		"RUNNER_OS":               "Linux",
		"RUNNER_TEMP":             r.t.TempDir(),
		"HOME":                    r.t.TempDir(),
		"GITHUB_SERVER_URL":       r.w.srv.URL(),
		"GITHUB_API_URL":          r.w.srv.EnterpriseAPIURL(),
		"GITHUB_GRAPHQL_URL":      r.w.srv.URL() + "/api/graphql",
		"GITHUB_REPOSITORY":       r.hub,
		"GITHUB_REPOSITORY_ID":    strconv.FormatInt(r.repoID, 10),
		"GITHUB_REPOSITORY_OWNER": org,
		"GITHUB_EVENT_NAME":       gh["event_name"],
		"GITHUB_EVENT_PATH":       eventPath,
		"GITHUB_REF":              gh["ref"],
		"GITHUB_REF_NAME":         gh["ref_name"],
		"GITHUB_REF_TYPE":         "branch",
		"GITHUB_SHA":              gh["sha"],
		"GITHUB_ACTOR":            gh["actor"],
		"GITHUB_WORKSPACE":        ws,
		"GITHUB_JOB":              nj.id,
		"GITHUB_RUN_ID":           strconv.Itoa(1000 + r.n),
		"GITHUB_STEP_SUMMARY":     summary,
		"PATH":                    r.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_DOCKER_PATH":        os.Getenv("PATH"),
		"FAKE_DOCKER_TOUCHMARK":   r.touchmark,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CONFIG_GLOBAL":       os.DevNull,
	}
	if gh["event_name"] == "pull_request" {
		base["GITHUB_BASE_REF"], base["GITHUB_HEAD_REF"] = gh["base_ref"], gh["head_ref"]
	}
	steps := map[string]any{}
	ectx["steps"] = steps
	failed := false
	var rep *report.Delivery
	for i, step := range job.Steps {
		// A step runs after a failure only with if: always().
		run := !failed
		if c := strings.TrimSpace(step.If); c == "always()" {
			run = true
		} else if c != "" {
			run = !failed && truthy(r.eval(c, ectx))
		}
		if !run {
			continue
		}
		stepEnv := map[string]string{}
		for k, v := range step.Env {
			stepEnv[k] = r.interpolate(v, ectx)
		}
		name := step.Name
		if name == "" {
			name = step.Uses
		}
		fmt.Fprintf(&log, "== step %d %s\n", i+1, name)
		var stepOut string
		var stepOutputs map[string]string
		var err error
		switch {
		case strings.HasPrefix(step.Uses, "actions/checkout@"):
			err = r.checkout(ws, gh, step)
		case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
			pattern := fmt.Sprint(step.With["path"])
			matches, _ := filepath.Glob(filepath.Join(ws, pattern))
			stepOut = fmt.Sprintf("upload %s: %d files", pattern, len(matches))
			if len(matches) == 0 && fmt.Sprint(step.With["if-no-files-found"]) != "ignore" {
				err = fmt.Errorf("no files match %s", pattern)
			}
		case strings.HasPrefix(step.Uses, "bedrock-python/touchmark@"):
			stepOut, err = r.touchmarkStep(ws, base, stepEnv, step, ectx)
		case step.Uses != "":
			t.Fatalf("job %s: the dry run does not know %s", nj.id, step.Uses)
		default:
			stepOut, stepOutputs, err = r.runStep(ws, base, stepEnv, r.interpolate(step.Run, ectx))
		}
		log.WriteString(stepOut)
		if step.ID != "" {
			steps[step.ID] = map[string]any{"outputs": toAny(stepOutputs)}
		}
		if err != nil {
			fmt.Fprintf(&log, "== step %d failed: %v\n", i+1, err)
			failed = true
		}
	}
	if data, err := os.ReadFile(filepath.Join(ws, "touchmark-report.json")); err == nil {
		validate(t, "report", data, r.w.reg.Replace)
		rep = &report.Delivery{}
		if err := json.Unmarshal(data, rep); err != nil {
			t.Errorf("job %s: decode its report: %v", nj.id, err)
		}
	}
	outputs := map[string]string{}
	for k, v := range job.Outputs {
		outputs[k] = r.interpolate(v, ectx)
	}
	if failed {
		return "failure", log.String(), outputs, rep
	}
	return "success", log.String(), outputs, rep
}

// jobToken mints the job's GITHUB_TOKEN with the job's permissions (the
// workflow's default, permissions: {}, when the job names none), read-only
// for a run of a fork's pull request.
func (r *workflowRunner) jobToken(job wfJob, readOnly bool) string {
	r.t.Helper()
	perms := ghfake.Permissions{"metadata": ghfake.Read}
	n := job.Permissions
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := strings.ReplaceAll(n.Content[i].Value, "-", "_")
		switch n.Content[i+1].Value {
		case "read":
			perms[name] = ghfake.Read
		case "write":
			perms[name] = ghfake.Write
			if readOnly {
				perms[name] = ghfake.Read
			}
		}
	}
	tok := try(r.w.srv.InstallationToken(r.actionsInst.ID, []string{"engineering-assets"}, perms)).of(r.t)
	r.w.reg.Add(tok)
	return tok
}

// checkout clones the hub as actions/checkout does with fetch-depth 0: a
// pull request's merge commit (refs/pull/<n>/merge), else the run's
// commit; no credentials stay in the clone.
func (r *workflowRunner) checkout(ws string, gh map[string]string, step wfStep) error {
	if fmt.Sprint(step.With["fetch-depth"]) != "0" || fmt.Sprint(step.With["persist-credentials"]) != "false" {
		return fmt.Errorf("checkout without fetch-depth 0 and persist-credentials false: %v", step.With)
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=GitHub", "GIT_AUTHOR_EMAIL=noreply@github.com", "GIT_COMMITTER_NAME=GitHub", "GIT_COMMITTER_EMAIL=noreply@github.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	if err := git("clone", "-q", r.w.srv.GitDir(r.hub), "."); err != nil {
		return err
	}
	if gh["event_name"] == "pull_request" {
		if err := git("checkout", "-q", "--detach", gh["pr_base"]); err != nil {
			return err
		}
		return git("merge", "-q", "--no-ff", "--no-edit", "-m", "Merge "+gh["pr_head"]+" into "+gh["pr_base"], gh["pr_head"])
	}
	return git("checkout", "-q", "--detach", gh["sha"])
}

// runStep runs a run: step with bash -e, as GitHub's shell: bash does,
// and returns its output and GITHUB_OUTPUT.
func (r *workflowRunner) runStep(ws string, base, stepEnv map[string]string, script string) (string, map[string]string, error) {
	outFile := filepath.Join(r.t.TempDir(), "output")
	check(r.t, os.WriteFile(outFile, nil, 0o644))
	cmd := exec.Command(r.bash, "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
	cmd.Dir = ws
	cmd.Env = envList(base, stepEnv, map[string]string{"GITHUB_OUTPUT": outFile})
	out, err := cmd.CombinedOutput()
	outputs := map[string]string{}
	data, _ := os.ReadFile(outFile)
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			outputs[k] = v
		}
	}
	return string(out), outputs, err
}

// touchmarkStep runs the touchmark Action's step: this working tree's
// scripts/action/run.sh, as action.yml runs it, with the inputs as TM_*,
// the image and version of a release, and the job's token for gh.
func (r *workflowRunner) touchmarkStep(ws string, base, stepEnv map[string]string, step wfStep, ectx exprContext) (string, error) {
	inputs := map[string]string{}
	for k, v := range r.actionInputs {
		inputs[k] = v
	}
	for k, v := range step.With {
		if _, ok := r.actionInputs[k]; !ok {
			return "", fmt.Errorf("the Action has no input %s", k)
		}
		inputs[k] = r.interpolate(fmt.Sprint(v), ectx)
	}
	tm := map[string]string{
		"TM_IMAGE": "ghcr.io/bedrock-python/touchmark", "TM_VERSION": fakeReleaseVersion, "GH_TOKEN": "job-token-for-gh",
		"TM_COMMAND": inputs["command"], "TM_HUB": inputs["hub"], "TM_STRICT": inputs["strict"], "TM_ALL": inputs["all"],
		"TM_COMMENT": inputs["comment"], "TM_ASSUME_OPT_IN": inputs["assume-opt-in"], "TM_DRY_RUN": inputs["dry-run"],
		"TM_ONLY": inputs["only"], "TM_DEADLINE": inputs["deadline"], "TM_FORMAT": inputs["format"], "TM_REPORT": inputs["report"],
		"GITHUB_ACTION_PATH": moduleRoot(r.t),
	}
	cmd := exec.Command(r.bash, filepath.Join(moduleRoot(r.t), "scripts", "action", "run.sh"))
	cmd.Dir = ws
	cmd.Env = envList(base, stepEnv, tm)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// envList merges environments, later ones winning, into NAME=VALUE.
func envList(maps ...map[string]string) []string {
	merged := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func toAny(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// wantJobs checks the result of every job of a run.
func (r *workflowRunner) wantJobs(what string, run wfRun, want map[string]string) {
	r.t.Helper()
	var names, results []string
	for n := range run.results {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		results = append(results, n+" "+run.results[n])
	}
	r.t.Logf("%s: %s", what, strings.Join(results, ", "))
	for _, n := range names {
		if want[n] != run.results[n] {
			r.t.Errorf("%s: job %s %s, want %s\n%s", what, n, run.results[n], want[n], r.w.reg.Replace(run.logs[n]))
		}
	}
	if len(names) != len(want) {
		r.t.Errorf("%s: jobs %v, want %v", what, names, want)
	}
}

// wantOutcomes checks that both targets have outcome in a report.
func (r *workflowRunner) wantOutcomes(what string, rep *report.Delivery, outcome string) {
	r.t.Helper()
	if rep == nil {
		r.t.Errorf("%s: no report", what)
		return
	}
	got := map[string]string{}
	for _, tg := range rep.Targets {
		got[tg.Path] = string(tg.Outcome)
	}
	want := map[string]string{org + "/alpha": outcome, org + "/beta": outcome}
	if !maps(got, want) {
		r.t.Errorf("%s: targets %v, want %v", what, got, want)
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// noSecrets checks that no key or token reached a log, as the runner
// shows it: without the ::add-mask:: commands, which it consumes.
func (r *workflowRunner) noSecrets() {
	r.t.Helper()
	for i, o := range r.outputs {
		var shown []string
		for _, line := range strings.Split(o, "\n") {
			if !strings.HasPrefix(line, "::add-mask::") {
				shown = append(shown, line)
			}
		}
		if s := strings.Join(shown, "\n"); strings.Contains(s, "PRIVATE KEY") || strings.Contains(s, "ghs_") {
			r.t.Errorf("log %d holds a key or a token:\n%s", i, r.w.reg.Replace(s))
		}
	}
}

// The expressions of the workflow: ${{ … }} in strings, bare in if.

type exprContext map[string]any

// actionPinRe matches a pin of the touchmark Action with its version
// comment, as Dependabot moves it: the commit, then # vX.Y.Z.
var actionPinRe = regexp.MustCompile(`(bedrock-python/touchmark@[0-9a-f]{40} # v)([0-9]+)\.([0-9]+)\.([0-9]+)`)

var interpolation = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// interpolate replaces every ${{ expr }} of s with its value.
func (r *workflowRunner) interpolate(s string, ctx exprContext) string {
	return interpolation.ReplaceAllStringFunc(s, func(m string) string {
		return valueString(r.eval(interpolation.FindStringSubmatch(m)[1], ctx))
	})
}

// eval evaluates an expression of the subset the template uses: context
// lookups, string literals, true, false, ==, !=, &&, ||, !, parentheses and
// always(); strings compare without case, as GitHub's do.
func (r *workflowRunner) eval(expr string, ctx exprContext) any {
	r.t.Helper()
	expr = strings.TrimSpace(expr)
	if m := interpolation.FindStringSubmatch(expr); m != nil && m[0] == expr {
		expr = m[1]
	}
	p := &exprParser{toks: tokenize(r.t, expr), ctx: ctx, t: r.t, src: expr}
	v := p.or()
	if p.pos != len(p.toks) {
		r.t.Fatalf("expression %q: unexpected %q", expr, p.toks[p.pos])
	}
	return v
}

var tokenRE = regexp.MustCompile(`\s*(==|!=|&&|\|\||!|\(|\)|'(?:[^']|'')*'|[A-Za-z_][A-Za-z0-9_.\-]*)`)

func tokenize(t *testing.T, s string) []string {
	var out []string
	for rest := s; strings.TrimSpace(rest) != ""; {
		m := tokenRE.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			t.Fatalf("expression %q: cannot read %q", s, rest)
		}
		out = append(out, rest[m[2]:m[3]])
		rest = rest[m[1]:]
	}
	return out
}

type exprParser struct {
	toks []string
	pos  int
	ctx  exprContext
	t    *testing.T
	src  string
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) or() any {
	v := p.and()
	for p.peek() == "||" {
		p.pos++
		w := p.and()
		if !truthy(v) {
			v = w
		}
	}
	return v
}

func (p *exprParser) and() any {
	v := p.cmp()
	for p.peek() == "&&" {
		p.pos++
		w := p.cmp()
		if truthy(v) {
			v = w
		}
	}
	return v
}

func (p *exprParser) cmp() any {
	v := p.unary()
	for op := p.peek(); op == "==" || op == "!="; op = p.peek() {
		p.pos++
		w := p.unary()
		eq := strings.EqualFold(valueString(v), valueString(w))
		v = eq == (op == "==")
	}
	return v
}

func (p *exprParser) unary() any {
	switch tok := p.peek(); {
	case tok == "!":
		p.pos++
		return !truthy(p.unary())
	case tok == "(":
		p.pos++
		v := p.or()
		if p.peek() != ")" {
			p.t.Fatalf("expression %q: no )", p.src)
		}
		p.pos++
		return v
	case strings.HasPrefix(tok, "'"):
		p.pos++
		return strings.ReplaceAll(tok[1:len(tok)-1], "''", "'")
	case tok == "true" || tok == "false":
		p.pos++
		return tok == "true"
	case tok == "always" || tok == "success":
		p.pos++
		if p.peek() == "(" && p.pos+1 < len(p.toks) && p.toks[p.pos+1] == ")" {
			p.pos += 2
			return true
		}
		p.t.Fatalf("expression %q: %s without ()", p.src, tok)
	case tok != "":
		p.pos++
		return p.lookup(tok)
	}
	p.t.Fatalf("expression %q ends early", p.src)
	return nil
}

// lookup reads a.b.c from the contexts; a missing property is null, an
// unknown context fails the test.
func (p *exprParser) lookup(path string) any {
	parts := strings.Split(path, ".")
	v, ok := p.ctx[parts[0]]
	if !ok {
		p.t.Fatalf("expression %q: the dry run has no context %s", p.src, parts[0])
	}
	for _, part := range parts[1:] {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[part]
	}
	return v
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	}
	return true
}

func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}
