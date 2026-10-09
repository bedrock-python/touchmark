package bitbuckete2e

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
)

// The file's shape, as far as the test reads it
// (support.atlassian.com/bitbucket-cloud/docs/bitbucket-pipelines-configuration-reference/).
type pipelinesFile struct {
	Image     any       `yaml:"image"`
	Clone     cloneOpts `yaml:"clone"`
	Pipelines struct {
		Default      []item            `yaml:"default"`
		Branches     map[string][]item `yaml:"branches"`
		Tags         map[string][]item `yaml:"tags"`
		PullRequests map[string][]item `yaml:"pull-requests"`
		Custom       map[string][]item `yaml:"custom"`
	} `yaml:"pipelines"`
}

type cloneOpts struct {
	Depth   any   `yaml:"depth"`
	Enabled *bool `yaml:"enabled"`
}

type item struct {
	Step     *step `yaml:"step"`
	Parallel any   `yaml:"parallel"`
	Stage    any   `yaml:"stage"`
}

type step struct {
	Name       string     `yaml:"name"`
	Image      any        `yaml:"image"`
	Deployment string     `yaml:"deployment"`
	Trigger    string     `yaml:"trigger"`
	Clone      *cloneOpts `yaml:"clone"`
	MaxTime    int        `yaml:"max-time"`
	Script     []string   `yaml:"script"`
	Artifacts  []string   `yaml:"artifacts"`
}

// pipeline is one pipeline of the file: its steps, and what starts it.
type pipeline struct {
	name  string
	steps []step
	// env is what Bitbucket sets in every step of a run of it.
	env map[string]string
}

const (
	hubUUID  = "{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}"
	readTok  = "reader-api-token-0123456789"
	hubTok   = "hub-access-token-0123456789"
	writeTok = "writer-api-token-0123456789"
)

var (
	imageRe   = regexp.MustCompile(`^ghcr\.io/bedrock-python/touchmark:[^@\s]+@sha256:[0-9a-f]{64}$`)
	commandRe = regexp.MustCompile(`^touchmark (probe|check|plan|distribute|doctor)\b`)
)

// TestTemplatePipelines checks the security properties of the template's
// bitbucket-pipelines.yml:
//
//   - every step runs the touchmark image, pinned by digest, with the hub's
//     whole history; no step sets an image of its own;
//   - the pipelines and their touchmark commands: a pull request runs
//     probe, check, plan --strict --comment; the push pipeline of main and
//     master, and the custom pipeline distribute, run probe then
//     distribute; the custom pipeline doctor runs probe then doctor; there
//     is no default or tag pipeline, which would run on any branch or tag;
//   - only distribute and doctor run with deployment: touchmark-distribute,
//     and every pipeline starts with the probe, without it;
//   - no script names a TOUCHMARK_ variable, and the custom pipelines'
//     scripts name none at all: the website asks for variables a custom
//     pipeline's script references, and a pipeline variable overrides a
//     deployment variable;
//   - played with the variables Bitbucket gives each step (the repository's
//     to every step, the deployment's to its steps only): the probe passes
//     and touchmark's guards admit distribute and doctor on the default
//     branch of a hub that states its Premium restriction, and refuse them
//     on another branch, in a pull request, and under platform without the
//     statement; with the write key leaked into a repository variable,
//     every pipeline stops at its probe, before any step that holds it.
func TestTemplatePipelines(t *testing.T) {
	dir := os.Getenv("TOUCHMARK_E2E_TEMPLATE")
	if dir == "" {
		t.Skip("TOUCHMARK_E2E_TEMPLATE is not set: it names the hub template's working tree")
	}
	data, err := os.ReadFile(filepath.Join(dir, "bitbucket-pipelines.yml"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("the template has no bitbucket-pipelines.yml")
	}
	if err != nil {
		t.Fatal(err)
	}
	var f pipelinesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatalf("bitbucket-pipelines.yml: %v", err)
	}
	if s, _ := f.Image.(string); !imageRe.MatchString(s) {
		t.Errorf("image %v: want touchmark pinned by digest", f.Image)
	}
	if d, _ := f.Clone.Depth.(string); d != "full" {
		t.Errorf("clone depth %v: touchmark reads the hub's whole history", f.Clone.Depth)
	}
	if len(f.Pipelines.Default) > 0 || len(f.Pipelines.Tags) > 0 {
		t.Error("a default or tag pipeline runs on any branch or tag")
	}

	pr := map[string]string{"BITBUCKET_BRANCH": "feature", "BITBUCKET_PR_ID": "41", "BITBUCKET_PR_DESTINATION_BRANCH": "main"}
	main := map[string]string{"BITBUCKET_BRANCH": "main"}
	var all []pipeline
	add := func(name string, items []item, env map[string]string) {
		var steps []step
		for i, it := range items {
			if it.Step == nil || it.Parallel != nil || it.Stage != nil {
				t.Errorf("%s: item %d is not a plain step", name, i)
				continue
			}
			steps = append(steps, *it.Step)
		}
		all = append(all, pipeline{name: name, steps: steps, env: env})
	}
	if len(f.Pipelines.PullRequests) != 1 || f.Pipelines.PullRequests["**"] == nil {
		t.Fatalf("pull-requests %v: want one pipeline for every branch, \"**\"", keys(f.Pipelines.PullRequests))
	}
	add("pull-requests **", f.Pipelines.PullRequests["**"], pr)
	var branchPattern string
	for pattern, items := range f.Pipelines.Branches {
		branchPattern = pattern
		add("branches "+pattern, items, main)
	}
	if len(f.Pipelines.Branches) != 1 || !matchesBoth(branchPattern) {
		t.Errorf("branches %v: want one pipeline for main and master", keys(f.Pipelines.Branches))
	}
	for name, items := range f.Pipelines.Custom {
		add("custom "+name, items, main)
	}
	if got := keys(f.Pipelines.Custom); !slices.Equal(got, []string{"distribute", "doctor"}) {
		t.Errorf("custom pipelines %q, want distribute and doctor", got)
	}

	want := map[string][]string{
		"pull-requests **":          {"probe", "check", "plan"},
		"branches " + branchPattern: {"probe", "distribute"},
		"custom distribute":         {"probe", "distribute"},
		"custom doctor":             {"probe", "doctor"},
	}
	for _, p := range all {
		var cmds []string
		for _, s := range p.steps {
			cmd := checkStep(t, p, s)
			cmds = append(cmds, cmd)
		}
		if !slices.Equal(cmds, want[p.name]) {
			t.Errorf("%s runs %q, want %q", p.name, cmds, want[p.name])
		}
	}
	if t.Failed() {
		return
	}

	premium := &config.Hub{ID: "acme-eng", Security: config.Security{WriteIsolation: "platform",
		Reason: "Bitbucket Premium: only main may deploy to touchmark-distribute"}}
	repoVars := map[string]string{"TOUCHMARK_READ_TOKEN": readTok, "TOUCHMARK_PIPELINES_TOKEN": hubTok}
	deployVars := map[string]string{"TOUCHMARK_WRITE_TOKEN": writeTok}

	// As configured: every step runs; the probe passes; the deployment
	// steps pass touchmark's guards on the default branch.
	for _, p := range all {
		ran := play(t, p, repoVars, deployVars, premium, "")
		if len(ran) != len(p.steps) {
			t.Errorf("%s: ran %q of %d steps", p.name, ran, len(p.steps))
		}
	}
	// The custom pipelines run by hand on another branch: refused.
	for _, p := range all {
		if strings.HasPrefix(p.name, "custom ") {
			q := p
			q.env = map[string]string{"BITBUCKET_BRANCH": "feature"}
			if ran := play(t, q, repoVars, deployVars, premium, "default branch main, not on feature"); len(ran) == len(p.steps) {
				t.Errorf("%s on another branch ran every step", p.name)
			}
		}
	}
	// Free or Standard, the template's hub.yml: platform without a
	// statement is refused.
	plain := &config.Hub{ID: "acme-eng", Security: config.Security{WriteIsolation: "platform"}}
	for _, p := range all {
		if p.name == "custom doctor" || strings.HasPrefix(p.name, "branches ") {
			play(t, p, repoVars, deployVars, plain, "only Bitbucket Premium offers")
		}
	}
	// The write key leaked into a repository variable: every pipeline stops
	// at its probe.
	leaked := map[string]string{"TOUCHMARK_WRITE_TOKEN": writeTok}
	for k, v := range repoVars {
		leaked[k] = v
	}
	for _, p := range all {
		if ran := play(t, p, leaked, deployVars, premium, ""); len(ran) != 0 {
			t.Errorf("%s with a leaked write key ran %q", p.name, ran)
		}
	}
}

// checkStep checks one step and returns its touchmark command.
func checkStep(t *testing.T, p pipeline, s step) string {
	t.Helper()
	if s.Image != nil {
		t.Errorf("%s: step %s sets its own image", p.name, s.Name)
	}
	if s.Trigger != "" && s.Trigger != "automatic" {
		t.Errorf("%s: step %s has trigger %s", p.name, s.Name, s.Trigger)
	}
	var cmd string
	for _, line := range s.Script {
		if strings.Contains(line, "TOUCHMARK_") {
			t.Errorf("%s: step %s names a touchmark variable: %s", p.name, s.Name, line)
		}
		if strings.HasPrefix(p.name, "custom ") && strings.Contains(line, "$") {
			t.Errorf("%s: step %s references a variable, which Run pipeline would ask for: %s", p.name, s.Name, line)
		}
		if m := commandRe.FindStringSubmatch(line); m != nil {
			if cmd != "" {
				t.Errorf("%s: step %s runs two touchmark commands", p.name, s.Name)
			}
			cmd = m[1]
		}
	}
	switch {
	case cmd == "":
		t.Errorf("%s: step %s runs no touchmark command", p.name, s.Name)
	case cmd == "distribute" || cmd == "doctor":
		if s.Deployment != "touchmark-distribute" {
			t.Errorf("%s: %s runs with deployment %q, want touchmark-distribute", p.name, cmd, s.Deployment)
		}
	case s.Deployment != "":
		t.Errorf("%s: %s runs with deployment %s, which holds the write key", p.name, cmd, s.Deployment)
	}
	script := strings.Join(s.Script, "\n")
	if cmd == "plan" && (!strings.Contains(script, "--strict") || !strings.Contains(script, "--comment")) {
		t.Errorf("%s: plan without --strict --comment", p.name)
	}
	if cmd == "distribute" && (s.MaxTime < 330 || !strings.Contains(script, "--deadline")) {
		t.Errorf("%s: distribute needs a deadline under its max-time (%d minutes)", p.name, s.MaxTime)
	}
	if s.Name != cmd {
		t.Errorf("%s: step %q runs %s", p.name, s.Name, cmd)
	}
	if s.Clone != nil && s.Clone.Depth != nil {
		t.Errorf("%s: step %s clones a shallow history", p.name, s.Name)
	}
	return cmd
}

// play runs pipeline p as Bitbucket would with these variables, step by
// step, until one fails: the probe through distribute.Probe, distribute
// and doctor through touchmark's guards (with the default branch main, as
// the hub channel reads it), and the other steps only through the guard of
// plan (no write key in the step). It returns the steps that ran to the
// end. wantErr, when set, is part of the error that must stop it.
func play(t *testing.T, p pipeline, repoVars, deployVars map[string]string, hub *config.Hub, wantErr string) []string {
	t.Helper()
	var ran []string
	for _, s := range p.steps {
		env := map[string]string{
			"CI": "true", "BITBUCKET_BUILD_NUMBER": "17", "BITBUCKET_REPO_UUID": hubUUID,
			"BITBUCKET_REPO_FULL_NAME": "acme/engineering-assets", "BITBUCKET_REPO_IS_PRIVATE": "true",
		}
		for k, v := range p.env {
			env[k] = v
		}
		for k, v := range repoVars {
			env[k] = v
		}
		if s.Deployment != "" {
			env["BITBUCKET_DEPLOYMENT_ENVIRONMENT"] = s.Deployment
			for k, v := range deployVars {
				env[k] = v
			}
		}
		getenv := func(k string) string { return env[k] }
		environ := func() []string {
			var out []string
			for k, v := range env {
				out = append(out, k+"="+v)
			}
			return out
		}
		var err error
		switch s.Name {
		case "probe":
			if res := distribute.Probe(getenv, environ); len(res.Exposed) > 0 {
				err = errors.New("probe: " + strings.Join(res.Exposed, ", ") + " visible")
			}
		case "distribute", "doctor":
			hctx := hubch.Detect(getenv, nil)
			if hctx.CI != hubch.BitbucketPipelines || hctx.Fingerprint() != "bitbucket.org/3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d" {
				t.Fatalf("%s: the step's context %+v", p.name, hctx)
			}
			hctx.DefaultBranch = "main"
			if s.Name == "doctor" {
				err = distribute.DoctorGuard(hctx, getenv)
				if err == nil {
					err = isolationFails(hctx, hub)
				}
			} else {
				err = distribute.DistributeGuard(t.Context(), distribute.GuardInput{Context: hctx, Hub: hub, Getenv: getenv, GitVersion: gitx.DeliveryMinVersion})
			}
		default:
			if res := distribute.Probe(getenv, environ); len(res.Exposed) > 0 {
				err = errors.New(s.Name + " sees " + strings.Join(res.Exposed, ", "))
			}
		}
		if err != nil {
			if wantErr != "" && !strings.Contains(err.Error(), wantErr) {
				t.Errorf("%s: step %s: %v, want %q", p.name, s.Name, err, wantErr)
			}
			if wantErr == "" && !strings.HasPrefix(err.Error(), "probe: ") {
				t.Errorf("%s: step %s: %v", p.name, s.Name, err)
			}
			return ran
		}
		ran = append(ran, s.Name)
	}
	if wantErr != "" {
		t.Errorf("%s: every step ran, want %q", p.name, wantErr)
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

// matchesBoth reports whether a branches pattern of the file takes main
// and master: a name, or a {a,b} alternation of names.
func matchesBoth(pattern string) bool {
	alts := []string{pattern}
	if strings.HasPrefix(pattern, "{") && strings.HasSuffix(pattern, "}") {
		alts = strings.Split(pattern[1:len(pattern)-1], ",")
	}
	return slices.Contains(alts, "main") && slices.Contains(alts, "master")
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
