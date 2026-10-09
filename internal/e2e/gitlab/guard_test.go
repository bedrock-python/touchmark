//go:build e2e

package gitlabe2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestWriterOnHub runs security.writer_on_hub guard on GitLab: the hub is a
// project of a subgroup of the seeded group, which the writer is a
// Developer of, so the writer reaches the hub. The hub's default branch is
// protected (push No one, merge Maintainers), merges wait for a pipeline
// that succeeded and not a skipped one, and the CI file is read from the
// default branch (ci_config_path <file>@<hub>:main). Its merge request
// pipelines run `touchmark plan` on the runner the harness registered.
//
//   - doctor, with the writer's token, grades hub-guard ok; once the CI
//     file is read from the source branch again, it fails;
//   - the person's merge request: plan passes;
//   - the writer pushes to the merge request's branch, with a CI file of
//     its own that would drop plan: the pipeline still runs the default
//     branch's CI file, and plan exits 2 naming the writer's push;
//   - a merge request the writer opens: plan exits 2 naming the writer.
func TestWriterOnHub(t *testing.T) {
	e := needLive(t)
	if e.RunnerBin == "" {
		t.Skip(envPrefix + "RUNNER_BIN is not set: the harness started no runner")
	}
	t.Setenv("CI", "")
	fx := newGroup(t, e, "guard")
	id := "e2e-" + randHex(t, 3)
	root := e.api(e.Root)
	hubYML := fmt.Sprintf("version: 1\nid: %s\nproviders:\n  - id: gl\n    type: gitlab\n    url: %s\n    writer: %s\n"+
		"security:\n  writer_on_hub: guard\n", id, e.URL, e.Writer.Login)
	ci := "workflow:\n  rules:\n    - if: $CI_PIPELINE_SOURCE == \"merge_request_event\"\nvariables:\n  GIT_DEPTH: \"0\"\n" +
		"plan:\n  script:\n" +
		"    - git fetch --no-tags origin \"+refs/heads/$CI_DEFAULT_BRANCH:refs/remotes/origin/$CI_DEFAULT_BRANCH\"\n" +
		fmt.Sprintf("    - '%s plan --hub .'\n", e.RunnerBin)
	files := []conformance.File{
		{Path: config.HubFile, Content: []byte(hubYML)},
		{Path: config.TargetsFile, Content: []byte(fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - group: %s\n", fx.ns))},
		{Path: "packs/base/AGENTS.md", Content: []byte(text("base AGENTS.md v1"))},
		{Path: ".gitlab-ci.yml", Content: []byte(ci)},
	}
	hub := fx.createProject(t, fx.nsID, "hub", files)
	e.waitAccess(t, hub.ID, e.Writer, e.Reader, e.Person)
	pinned := ".gitlab-ci.yml@" + hub.PathWithNamespace + ":main"

	// The guard: main protected, the merge checks, the CI file of main.
	root.do(t, http.MethodDelete, fmt.Sprintf("/projects/%d/protected_branches/main", hub.ID), nil)
	root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/protected_branches", hub.ID),
		map[string]any{"name": "main", "push_access_level": 0, "merge_access_level": levelMaintainer, "allow_force_push": false}, nil)
	root.ok(t, http.MethodPut, fmt.Sprintf("/projects/%d", hub.ID), map[string]any{
		"only_allow_merge_if_pipeline_succeeds": true, "allow_merge_on_skipped_pipeline": false, "ci_config_path": pinned,
	}, nil)
	root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/variables", hub.ID),
		map[string]any{"key": "TOUCHMARK_GL_READ_TOKEN", "value": e.Reader.Token, "masked": true, "protected": false}, nil)

	// doctor, as a maintainer runs it, from a clone of the hub.
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q", "-b", "main")
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, nil, nil, "add", "-A")
	gitCmd(t, dir, nil, nil, "commit", "-q", "-m", "the hub")
	gitCmd(t, dir, nil, nil, "remote", "add", "origin", e.remote(hub.PathWithNamespace))
	fp := fmt.Sprintf("%s/%d", e.Host, hub.ID)
	hubGuard := func(code int) report.DoctorCheck {
		t.Helper()
		t.Setenv("TOUCHMARK_GL_WRITE_TOKEN", e.Writer.Token)
		args := []string{"doctor", "--hub", dir, "--hub-fp", fp, "--format", "json"}
		var stdout, stderr bytes.Buffer
		if got := cli.Main(context.Background(), args, &stdout, &stderr); got != code {
			t.Fatalf("doctor: exit %d, want %d\n%s\n%s", got, code, e.Redact.Replace(stdout.String()), e.Redact.Replace(stderr.String()))
		}
		validate(t, "doctor", stdout.Bytes(), e.Redact.Replace)
		var doc report.Doctor
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Providers[0].Checks {
			if c.Name == "hub-hidden" {
				t.Errorf("hub-hidden under guard: %+v", c)
			}
			if c.Name == "hub-guard" {
				return c
			}
		}
		t.Fatalf("no hub-guard in %+v", doc.Providers[0].Checks)
		return report.DoctorCheck{}
	}
	if c := hubGuard(0); c.Status != report.StatusOK {
		t.Errorf("hub-guard %s: %s", c.Status, c.Detail)
	}

	repo := fx.repo(t, hub.ID)
	// planJob is the plan job of the pipeline of merge request mr at its
	// head now.
	planJob := func(mr apiMR) ciJob {
		t.Helper()
		sha := e.mr(t, hub.ID, mr.IID).SHA
		ref := fmt.Sprintf("refs/merge-requests/%d/head", mr.IID)
		jobs := e.pipelineJobs(t, hub.ID, func() (int64, bool) { return e.pipelineOf(t, hub.ID, ref, sha) })
		job, ok := jobs["plan"]
		if !ok {
			t.Fatalf("the merge request pipeline ran %v, not the default branch's plan", keys(jobs))
		}
		return job
	}

	// The person's merge request: plan passes.
	mr := fx.openMR(t, repo, e.Person, mrOptions{head: "feature", base: "main", title: "a change"})
	if job := planJob(mr); job.Status != "success" {
		t.Fatalf("plan of the person's merge request: %s\n%s", job.Status, e.Redact.Replace(job.Trace))
	}

	// The writer pushes a CI file of its own to the branch: the pipeline
	// still runs main's, whose plan refuses the push.
	before := e.mr(t, hub.ID, mr.IID).SHA
	e.commitFiles(t, e.Writer, hub.ID, "feature", "", []conformance.File{
		{Path: ".gitlab-ci.yml", Content: []byte("ok:\n  script:\n    - 'true'\n")},
		{Path: "packs/base/AGENTS.md", Content: []byte(text("the writer's AGENTS.md"))},
	}, "the writer's change")
	e.waitMR(t, hub.ID, mr.IID, 2*time.Minute, func(m apiMR) bool { return m.SHA != before })
	job := planJob(mr)
	if job.Status != "failed" || !strings.Contains(job.Trace, "pushed to feature") {
		t.Errorf("plan after the writer's push: %s\n%s", job.Status, e.Redact.Replace(job.Trace))
	}

	// A merge request the writer opens.
	wmr := fx.openMR(t, repo, e.Writer, mrOptions{head: "writer-change", base: "main", title: "the writer's"})
	if job := planJob(wmr); job.Status != "failed" || !strings.Contains(job.Trace, "was opened by") {
		t.Errorf("plan of the writer's merge request: %s\n%s", job.Status, e.Redact.Replace(job.Trace))
	}

	// The CI file of the source branch again: hub-guard fails.
	root.ok(t, http.MethodPut, fmt.Sprintf("/projects/%d", hub.ID), map[string]any{"ci_config_path": ""}, nil)
	if c := hubGuard(1); c.Status != report.StatusFail || !strings.Contains(c.Detail, pinned) {
		t.Errorf("hub-guard without the pin %s: %s", c.Status, c.Detail)
	}
}

// keys returns the names of jobs.
func keys(jobs map[string]ciJob) []string {
	var out []string
	for k := range jobs {
		out = append(out, k)
	}
	return out
}
