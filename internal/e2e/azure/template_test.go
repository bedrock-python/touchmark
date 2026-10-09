package azuree2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
)

// The files' shape, as far as the test reads it
// (learn.microsoft.com/en-us/azure/devops/pipelines/yaml-schema/).
type pipelineFile struct {
	Trigger    any        `yaml:"trigger"`
	Schedules  []schedule `yaml:"schedules"`
	Parameters []struct {
		Name    string   `yaml:"name"`
		Default string   `yaml:"default"`
		Values  []string `yaml:"values"`
	} `yaml:"parameters"`
	Variables any     `yaml:"variables"`
	Pool      any     `yaml:"pool"`
	Container any     `yaml:"container"`
	Resources any     `yaml:"resources"`
	Stages    []stage `yaml:"stages"`
}

type schedule struct {
	Cron        string `yaml:"cron"`
	DisplayName string `yaml:"displayName"`
	Branches    struct {
		Include []string `yaml:"include"`
	} `yaml:"branches"`
	Always bool `yaml:"always"`
}

type stage struct {
	Stage     string `yaml:"stage"`
	DependsOn any    `yaml:"dependsOn"`
	Condition string `yaml:"condition"`
	Variables any    `yaml:"variables"`
	Jobs      []job  `yaml:"jobs"`
}

type job struct {
	Job         string           `yaml:"job"`
	Deployment  string           `yaml:"deployment"`
	Environment any              `yaml:"environment"`
	Variables   []map[string]any `yaml:"variables"`
	Container   any              `yaml:"container"`
	Pool        any              `yaml:"pool"`
	Timeout     int              `yaml:"timeoutInMinutes"`
	Steps       []map[string]any `yaml:"steps"`
	Strategy    struct {
		RunOnce struct {
			Deploy struct {
				Steps []map[string]any `yaml:"steps"`
			} `yaml:"deploy"`
		} `yaml:"runOnce"`
	} `yaml:"strategy"`
}

// stepTemplate is .azure-pipelines/touchmark.yml.
type stepTemplate struct {
	Parameters []struct {
		Name string `yaml:"name"`
	} `yaml:"parameters"`
	Steps []map[string]any `yaml:"steps"`
}

// call is one use of the step template: its parameters.
type call struct {
	name, args    string
	write, report bool
}

const (
	stepFile  = ".azure-pipelines/touchmark.yml"
	repoID    = "0b7e5a2c-9d4f-4e1b-8a3c-6f5d2e1c0b9a"
	readTok   = "reader-pat-0123456789"
	writeTok  = "writer-pat-0123456789"
	accessTok = "job-access-token-0123456789"
)

var (
	imageRe    = regexp.MustCompile(`ghcr\.io/bedrock-python/touchmark:[^@\s]+@sha256:[0-9a-f]{64}`)
	commandRe  = regexp.MustCompile(`^(probe|check|plan|distribute|doctor)\b`)
	deadlineRe = regexp.MustCompile(`--deadline (\d+)m\b`)
)

// TestTemplateAzurePipelines checks the security properties of the
// template's azure-pipelines.yml and its step template:
//
//   - every touchmark step runs the touchmark image pinned by digest, from
//     the one step template, with docker as the agent's user; no job runs
//     a container of its own; every checkout reads the whole history;
//   - the stages: probe first, without the variable group; check and plan
//     --strict --comment for a pull request; distribute and doctor as
//     deployment jobs to the environment touchmark-distribute that link the
//     variable group touchmark-distribute, which no other stage or job
//     links; no variables at the pipeline's top;
//   - the step template maps the writer's token only for write: true, and
//     the pipeline asks for it only in the probe and the deployment jobs;
//   - played with the variables Azure Pipelines gives each step (the
//     pipeline's secrets to every step that maps them, the group's to the
//     jobs that link it) for a pull request, a push, both schedules and
//     both manual runs, evaluating the stages' conditions: the probe
//     passes, the expected stages run, and touchmark's guards admit
//     distribute and doctor on the default branch of a hub that states its
//     Branch control check, and refuse them under platform without the
//     statement; with the write key leaked into a pipeline variable, every
//     run stops at its probe, before any stage that holds it.
func TestTemplateAzurePipelines(t *testing.T) {
	dir := os.Getenv("TOUCHMARK_E2E_TEMPLATE")
	if dir == "" {
		t.Skip("TOUCHMARK_E2E_TEMPLATE is not set: it names the hub template's working tree")
	}
	data, err := os.ReadFile(filepath.Join(dir, "azure-pipelines.yml"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("the template has no azure-pipelines.yml")
	}
	if err != nil {
		t.Fatal(err)
	}
	var f pipelineFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatalf("azure-pipelines.yml: %v", err)
	}
	tmplData, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(stepFile)))
	if err != nil {
		t.Fatal(err)
	}
	var tmpl stepTemplate
	if err := yaml.Unmarshal(tmplData, &tmpl); err != nil {
		t.Fatalf("%s: %v", stepFile, err)
	}
	checkStepTemplate(t, tmpl)
	if f.Variables != nil || f.Container != nil || f.Resources != nil {
		t.Error("the pipeline sets variables, a container or resources at its top: every stage would get them")
	}
	if len(f.Parameters) != 1 || f.Parameters[0].Name != "run" || f.Parameters[0].Default != "distribute" ||
		!slices.Equal(f.Parameters[0].Values, []string{"distribute", "doctor"}) {
		t.Errorf("parameters %+v: want run, distribute or doctor", f.Parameters)
	}
	var crons []string
	for _, s := range f.Schedules {
		crons = append(crons, s.DisplayName)
		if !s.Always || !slices.Contains(s.Branches.Include, "main") || !slices.Contains(s.Branches.Include, "master") {
			t.Errorf("schedule %q: want always, on main and master", s.DisplayName)
		}
	}
	if !slices.Equal(crons, []string{"touchmark distribute", "touchmark doctor"}) {
		t.Errorf("schedules %q", crons)
	}

	stages := map[string]stage{}
	var order []string
	for _, s := range f.Stages {
		stages[s.Stage] = s
		order = append(order, s.Stage)
	}
	if !slices.Equal(order, []string{"probe", "check", "distribute", "doctor"}) {
		t.Fatalf("stages %q", order)
	}
	calls := map[string][]call{}
	for _, s := range f.Stages {
		if s.Variables != nil {
			t.Errorf("stage %s sets variables", s.Stage)
		}
		if s.Stage != "probe" && fmt.Sprint(s.DependsOn) != "probe" {
			t.Errorf("stage %s depends on %v, want probe", s.Stage, s.DependsOn)
		}
		for _, j := range s.Jobs {
			calls[s.Stage] = append(calls[s.Stage], checkJob(t, s, j)...)
		}
	}
	want := map[string][]string{"probe": {"probe"}, "check": {"check", "plan"}, "distribute": {"distribute"}, "doctor": {"doctor"}}
	for name, cs := range calls {
		var got []string
		for _, c := range cs {
			got = append(got, c.name)
		}
		if !slices.Equal(got, want[name]) {
			t.Errorf("stage %s runs %q, want %q", name, got, want[name])
		}
	}
	if t.Failed() {
		return
	}

	checked := &config.Hub{ID: "acme-eng", Security: config.Security{WriteIsolation: "platform",
		Reason: "a Branch control check admits refs/heads/main only to the variable group touchmark-distribute"}}
	pipelineVars := map[string]string{"TOUCHMARK_READ_TOKEN": readTok}
	groupVars := map[string]string{"TOUCHMARK_WRITE_TOKEN": writeTok}
	runs := []struct {
		name   string
		env    map[string]string
		run    string
		stages []string
	}{
		{"pull request", map[string]string{"BUILD_REASON": "PullRequest", "BUILD_SOURCEBRANCH": "refs/pull/41/merge",
			"SYSTEM_PULLREQUEST_SOURCEBRANCH": "refs/heads/feature", "SYSTEM_PULLREQUEST_TARGETBRANCH": "refs/heads/main",
			"SYSTEM_PULLREQUEST_PULLREQUESTID": "41"}, "distribute", []string{"probe", "check"}},
		{"push", map[string]string{"BUILD_REASON": "IndividualCI", "BUILD_SOURCEBRANCH": "refs/heads/main"}, "distribute", []string{"probe", "distribute"}},
		{"batched push", map[string]string{"BUILD_REASON": "BatchedCI", "BUILD_SOURCEBRANCH": "refs/heads/main"}, "distribute", []string{"probe", "distribute"}},
		{"daily schedule", map[string]string{"BUILD_REASON": "Schedule", "BUILD_SOURCEBRANCH": "refs/heads/main",
			"BUILD_CRONSCHEDULE_DISPLAYNAME": "touchmark distribute"}, "distribute", []string{"probe", "distribute"}},
		{"weekly schedule", map[string]string{"BUILD_REASON": "Schedule", "BUILD_SOURCEBRANCH": "refs/heads/main",
			"BUILD_CRONSCHEDULE_DISPLAYNAME": "touchmark doctor"}, "distribute", []string{"probe", "doctor"}},
		{"manual distribute", map[string]string{"BUILD_REASON": "Manual", "BUILD_SOURCEBRANCH": "refs/heads/main"}, "distribute", []string{"probe", "distribute"}},
		{"manual doctor", map[string]string{"BUILD_REASON": "Manual", "BUILD_SOURCEBRANCH": "refs/heads/main"}, "doctor", []string{"probe", "doctor"}},
		{"manual run of another branch", map[string]string{"BUILD_REASON": "Manual", "BUILD_SOURCEBRANCH": "refs/heads/feature"}, "distribute", []string{"probe"}},
		{"push of a tag", map[string]string{"BUILD_REASON": "IndividualCI", "BUILD_SOURCEBRANCH": "refs/tags/v1"}, "distribute", []string{"probe"}},
	}
	for _, r := range runs {
		// As configured: the stages run, and every step passes.
		ran := play(t, f, calls, r.env, r.run, pipelineVars, groupVars, checked, "")
		if !slices.Equal(ran, r.stages) {
			t.Errorf("%s: ran %q, want %q", r.name, ran, r.stages)
		}
		// The write key leaked into a pipeline variable: the probe stops it.
		leaked := map[string]string{"TOUCHMARK_READ_TOKEN": readTok, "TOUCHMARK_WRITE_TOKEN": writeTok}
		if ran := play(t, f, calls, r.env, r.run, leaked, groupVars, checked, ""); len(ran) != 0 {
			t.Errorf("%s with a leaked write key ran %q", r.name, ran)
		}
	}
	// The template's hub.yml: platform without the statement is refused.
	plain := &config.Hub{ID: "acme-eng", Security: config.Security{WriteIsolation: "platform"}}
	play(t, f, calls, runs[1].env, "distribute", pipelineVars, groupVars, plain, "Branch control check")
	play(t, f, calls, runs[6].env, "doctor", pipelineVars, groupVars, plain, "Branch control check")
}

// checkStepTemplate checks .azure-pipelines/touchmark.yml: one bash step
// that runs the pinned image with docker as the agent's user, mapping the
// writer's token only under write, and the publish of the reports.
func checkStepTemplate(t *testing.T, tmpl stepTemplate) {
	t.Helper()
	var names []string
	for _, p := range tmpl.Parameters {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"name", "args", "write", "report"}) {
		t.Errorf("%s parameters %q", stepFile, names)
	}
	if len(tmpl.Steps) != 2 {
		t.Fatalf("%s: %d steps, want the touchmark step and the conditional publish", stepFile, len(tmpl.Steps))
	}
	script, _ := tmpl.Steps[0]["bash"].(string)
	if n := len(imageRe.FindAllString(script, -1)); n != 1 || strings.Count(script, "ghcr.io/") != 1 {
		t.Errorf("%s: want the touchmark image pinned by digest, once", stepFile)
	}
	for _, want := range []string{"docker run", `--user "$(id -u):$(id -g)"`, "--cap-drop ALL", "--security-opt no-new-privileges", "${{ parameters.args }}"} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the script lacks %q", stepFile, want)
		}
	}
	for _, bad := range []string{"docker.sock", "--privileged", "$(TOUCHMARK", "--env-file"} {
		if strings.Contains(script, bad) {
			t.Errorf("%s: the script has %q", stepFile, bad)
		}
	}
	env, _ := tmpl.Steps[0]["env"].(map[string]any)
	got := map[string]any{}
	for k, v := range env {
		got[k] = v
	}
	write, _ := got["${{ if parameters.write }}"].(map[string]any)
	delete(got, "${{ if parameters.write }}")
	if fmt.Sprint(got) != fmt.Sprint(map[string]any{"SYSTEM_ACCESSTOKEN": "$(System.AccessToken)", "TOUCHMARK_READ_TOKEN": "$(TOUCHMARK_READ_TOKEN)"}) ||
		fmt.Sprint(write) != fmt.Sprint(map[string]any{"TOUCHMARK_WRITE_TOKEN": "$(TOUCHMARK_WRITE_TOKEN)"}) {
		t.Errorf("%s: env %v", stepFile, env)
	}
	if _, ok := tmpl.Steps[1]["${{ if parameters.report }}"]; !ok {
		t.Errorf("%s: the second step is not the publish under report", stepFile)
	}
}

// checkJob checks one job of stage s and returns its uses of the step
// template.
func checkJob(t *testing.T, s stage, j job) []call {
	t.Helper()
	name := j.Job + j.Deployment
	if j.Container != nil || j.Pool != nil {
		t.Errorf("%s/%s sets its own container or pool", s.Stage, name)
	}
	steps := j.Steps
	deploy := j.Deployment != ""
	if deploy {
		steps = j.Strategy.RunOnce.Deploy.Steps
	}
	var groups []string
	for _, v := range j.Variables {
		if g, ok := v["group"].(string); ok {
			groups = append(groups, g)
		} else {
			t.Errorf("%s/%s sets a variable: %v", s.Stage, name, v)
		}
	}
	var calls []call
	for _, st := range steps {
		switch {
		case st["checkout"] != nil:
			if st["checkout"] == "self" && fmt.Sprint(st["fetchDepth"]) != "0" {
				t.Errorf("%s/%s: a shallow checkout", s.Stage, name)
			}
		case st["template"] != nil:
			if st["template"] != stepFile {
				t.Errorf("%s/%s uses template %v", s.Stage, name, st["template"])
				continue
			}
			p, _ := st["parameters"].(map[string]any)
			c := call{name: fmt.Sprint(p["name"]), args: fmt.Sprint(p["args"]), write: p["write"] == true, report: p["report"] == true}
			calls = append(calls, c)
			m := commandRe.FindStringSubmatch(c.args)
			if m == nil || m[1] != c.name {
				t.Errorf("%s/%s: step %s runs %q", s.Stage, name, c.name, c.args)
			}
		case st["bash"] != nil:
			script := fmt.Sprint(st["bash"])
			if strings.Contains(script, "TOUCHMARK_") || strings.Contains(script, "touchmark ") || s.Stage != "check" {
				t.Errorf("%s/%s: a script step of its own: %s", s.Stage, name, script)
			}
		default:
			t.Errorf("%s/%s: an unexpected step %v", s.Stage, name, st)
		}
	}
	keyed := s.Stage == "distribute" || s.Stage == "doctor"
	for _, c := range calls {
		switch {
		case keyed:
			env, _ := j.Environment.(string)
			if !deploy || env != "touchmark-distribute" || !slices.Equal(groups, []string{"touchmark-distribute"}) {
				t.Errorf("%s/%s: want a deployment job to touchmark-distribute that links the group touchmark-distribute", s.Stage, name)
			}
			if !c.write || !c.report {
				t.Errorf("%s/%s: %s without write or report", s.Stage, name, c.name)
			}
		case len(groups) > 0 || deploy:
			t.Errorf("%s/%s: %s runs with the variable group or in a deployment job", s.Stage, name, c.name)
		case c.write && c.name != "probe":
			t.Errorf("%s/%s: %s maps the writer's token", s.Stage, name, c.name)
		}
		if c.name == "plan" && (!strings.Contains(c.args, "--strict") || !strings.Contains(c.args, "--comment")) {
			t.Errorf("plan without --strict --comment")
		}
		if c.name == "distribute" {
			m := deadlineRe.FindStringSubmatch(c.args)
			if d, _ := strconv.Atoi(func() string {
				if m == nil {
					return ""
				}
				return m[1]
			}()); m == nil || d+5 > j.Timeout {
				t.Errorf("distribute needs a deadline well under its timeout (%d minutes): %q", j.Timeout, c.args)
			}
		}
	}
	return calls
}

// play runs the pipeline as Azure Pipelines would for a run with env (the
// reason and branch) and the parameter run, stage by stage: a stage runs
// when its condition holds; its steps get the predefined variables, the
// job access token, the pipeline's variables pipelineVars and, in a job
// that links the group, groupVars, as far as the step template maps them.
// The probe goes through distribute.Probe, distribute and doctor through
// touchmark's guards (with the default branch main, as the hub channel
// reads it), check and plan through the probe's eyes (no write key). It
// returns the stages that ran to the end; wantErr, when set, is part of the
// error that must stop one.
func play(t *testing.T, f pipelineFile, calls map[string][]call, env map[string]string, run string,
	pipelineVars, groupVars map[string]string, hub *config.Hub, wantErr string) []string {
	t.Helper()
	var ran []string
	failed := false
	for _, s := range f.Stages {
		if s.Stage != "probe" {
			cond := strings.ReplaceAll(s.Condition, "${{ parameters.run }}", run)
			ok, err := evaluate(cond, env, !failed)
			if err != nil {
				t.Fatalf("stage %s: condition %q: %v", s.Stage, s.Condition, err)
			}
			if !ok {
				continue
			}
		}
		keyed := s.Stage == "distribute" || s.Stage == "doctor"
		for _, c := range calls[s.Stage] {
			step := map[string]string{
				"CI": "true", "TF_BUILD": "True", "SYSTEM_COLLECTIONURI": "https://dev.azure.com/acme/", "SYSTEM_TEAMPROJECT": "Platform",
				"BUILD_REPOSITORY_PROVIDER": "TfsGit", "BUILD_REPOSITORY_ID": repoID, "BUILD_REPOSITORY_NAME": "engineering-assets",
				hubch.AzureTokenVar: accessTok,
			}
			for k, v := range env {
				step[k] = v
			}
			scope := map[string]string{}
			for k, v := range pipelineVars {
				scope[k] = v
			}
			if keyed {
				step["ENVIRONMENT_NAME"] = "touchmark-distribute"
				for k, v := range groupVars {
					scope[k] = v
				}
			}
			// The step template maps the reader's token, and the writer's
			// under write; an undefined one is left out.
			mapped := []string{"TOUCHMARK_READ_TOKEN"}
			if c.write {
				mapped = append(mapped, "TOUCHMARK_WRITE_TOKEN")
			}
			for _, k := range mapped {
				if v, ok := scope[k]; ok {
					step[k] = v
				}
			}
			getenv := func(k string) string { return step[k] }
			environ := func() []string {
				var out []string
				for k, v := range step {
					out = append(out, k+"="+v)
				}
				return out
			}
			var err error
			switch c.name {
			case "distribute", "doctor":
				hctx := hubch.Detect(getenv, nil)
				if hctx.CI != hubch.AzurePipelines || hctx.Fingerprint() != "dev.azure.com/"+repoID {
					t.Fatalf("%s: the step's context %+v", s.Stage, hctx)
				}
				hctx.DefaultBranch = "main"
				if c.name == "doctor" {
					err = distribute.DoctorGuard(hctx, getenv)
					if err == nil {
						err = isolationFails(hctx, hub)
					}
				} else {
					err = distribute.DistributeGuard(t.Context(), distribute.GuardInput{Context: hctx, Hub: hub, Getenv: getenv, GitVersion: gitx.DeliveryMinVersion})
				}
			default:
				if res := distribute.Probe(getenv, environ); len(res.Exposed) > 0 {
					err = errors.New("probe: " + c.name + " sees " + strings.Join(res.Exposed, ", "))
				}
			}
			if err != nil {
				if wantErr != "" && !strings.Contains(err.Error(), wantErr) {
					t.Errorf("stage %s: step %s: %v, want %q", s.Stage, c.name, err, wantErr)
				}
				switch {
				case wantErr != "":
				case !strings.HasPrefix(err.Error(), "probe: "):
					t.Errorf("stage %s: step %s: %v", s.Stage, c.name, err)
				case c.name != "probe":
					t.Errorf("stage %s: step %s sees a write key the probe did not", s.Stage, c.name)
				}
				failed = true
				break
			}
		}
		if failed {
			if s.Stage == "probe" {
				return ran
			}
			continue
		}
		ran = append(ran, s.Stage)
	}
	if wantErr != "" && !failed {
		t.Errorf("every stage ran, want %q", wantErr)
	}
	return ran
}

// isolationFails is doctor's write-isolation check as an error when it
// fails: doctor reports it, and exits 1.
func isolationFails(hctx hubch.Context, hub *config.Hub) error {
	for _, c := range distribute.IsolationChecks(distribute.IsolationInput{Context: hctx, Hub: hub}) {
		if c.Status == "fail" {
			return errors.New(c.Detail)
		}
	}
	return nil
}
