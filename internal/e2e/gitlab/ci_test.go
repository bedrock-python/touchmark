//go:build e2e

package gitlabe2e

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform/conformance"
)

// TestCI runs the CI experiments behind the isolation probe and threat T5
// (a harmful target, docs/project/threat-model.md) on the
// runner the harness registered (shell executor, touchmark at RunnerBin).
func TestCI(t *testing.T) {
	e := needLive(t)
	if e.RunnerBin == "" {
		t.Skip(envPrefix + "RUNNER_BIN is not set: the harness started no runner")
	}
	fx := newGroup(t, e, "ci")
	t.Run("probe", func(t *testing.T) { ciProbe(t, fx) })
	t.Run("job-token", func(t *testing.T) { ciJobToken(t, fx) })
}

// The write-token variables of the probe experiment: X protected, Y not,
// both scoped to the environment touchmark-distribute; Z neither protected
// nor scoped, the key a hub must never keep so.
const (
	varProtected = "TOUCHMARK_X_WRITE_TOKEN"
	varOpen      = "TOUCHMARK_Y_WRITE_TOKEN"
	varPlain     = "TOUCHMARK_Z_WRITE_TOKEN"
)

// ciProbe: touchmark probe in a merge request pipeline, in a job with
// environment {name: touchmark-distribute, action: prepare}, exits 2 when
// it sees a write key; the experiment answers whether such a job gets the
// variables scoped to that environment, and checks that the probe catches
// an unprotected scoped variable.
//
// A hub project gets three variables (see varProtected) and a pipeline
// that runs the probe in two jobs, one with the environment (action
// prepare) and one without, on merge request pipelines and on the default
// branch. The default branch is protected (GitLab protects it by default);
// the person's merge request comes from an unprotected branch of the hub.
func ciProbe(t *testing.T, fx *fixture) {
	e := fx.env
	hub := fx.createProject(t, fx.nsID, "hub", nil)
	e.waitAccess(t, hub.ID, e.Person)
	values := map[string]string{}
	for _, v := range []struct {
		key, scope string
		protected  bool
	}{
		{varProtected, "touchmark-distribute", true},
		{varOpen, "touchmark-distribute", false},
		{varPlain, "*", false},
	} {
		value := "tm" + randHex(t, 16)
		values[v.key] = value
		e.Redact.Add(value)
		e.api(e.Root).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/variables", hub.ID), map[string]any{
			"key": v.key, "value": value, "protected": v.protected, "masked": true, "environment_scope": v.scope,
		}, nil)
	}
	job := fmt.Sprintf("  script:\n    - '%s probe'\n", e.RunnerBin)
	ci := "workflow:\n  rules:\n    - if: $CI_PIPELINE_SOURCE == \"merge_request_event\"\n    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH\n" +
		"probe-environment:\n  environment:\n    name: touchmark-distribute\n    action: prepare\n" + job +
		"probe-plain:\n" + job
	main := e.commitFiles(t, e.Root, hub.ID, "main", "", []conformance.File{
		{Path: "README.md", Content: []byte("# hub\n")}, {Path: ".gitlab-ci.yml", Content: []byte(ci)},
	}, "the hub's pipeline")
	mainJobs := e.pipelineJobs(t, hub.ID, func() (int64, bool) { return e.pipelineOf(t, hub.ID, "main", main) })
	// Read after the pipeline: GitLab protects the default branch of a new
	// project when the first push creates it, and the API may not show it
	// at once.
	var protected apiBranch
	e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/repository/branches/main", hub.ID), &protected)

	repo := fx.repo(t, hub.ID)
	mr := fx.openMR(t, repo, e.Person, mrOptions{head: "feature/probe", base: "main", title: "a change to the hub", body: "x"})
	mrJobs := e.pipelineJobs(t, hub.ID, func() (int64, bool) { return e.mrPipeline(t, hub.ID, mr.IID) })

	seen := func(jobs map[string]ciJob, name string) []string {
		var out []string
		for _, key := range []string{varProtected, varOpen, varPlain} {
			if strings.Contains(jobs[name].Trace, key) {
				out = append(out, key)
			}
		}
		return out
	}
	for _, jobs := range []map[string]ciJob{mainJobs, mrJobs} {
		for name, j := range jobs {
			for key, value := range values {
				if strings.Contains(j.Trace, value) {
					t.Errorf("the log of %s shows the value of %s", name, key)
				}
			}
		}
	}
	finding(t, "probe-default-branch", "main protected %v; its pipeline: probe-environment %s, sees %v; probe-plain %s, sees %v",
		protected.Protected, mainJobs["probe-environment"].Status, seen(mainJobs, "probe-environment"),
		mainJobs["probe-plain"].Status, seen(mainJobs, "probe-plain"))
	finding(t, "probe-mr-pipeline", "a merge request pipeline from an unprotected branch: probe-environment (environment touchmark-distribute, action prepare) %s, sees %v; probe-plain %s, sees %v",
		mrJobs["probe-environment"].Status, seen(mrJobs, "probe-environment"), mrJobs["probe-plain"].Status, seen(mrJobs, "probe-plain"))

	// What the template's setup depends on: the protected, scoped key reaches
	// the environment job of the protected default branch and no job of a
	// merge request pipeline; the probe catches the unprotected scoped one
	// there.
	if got := seen(mainJobs, "probe-environment"); !slices.Contains(got, varProtected) {
		t.Errorf("the environment job on the protected default branch sees %v, want %s among them", got, varProtected)
	}
	if got := seen(mrJobs, "probe-environment"); slices.Contains(got, varProtected) {
		t.Errorf("the environment job of a merge request pipeline sees the protected %s", varProtected)
	}
	if j := mrJobs["probe-environment"]; j.Status != "failed" || !slices.Contains(seen(mrJobs, "probe-environment"), varOpen) {
		t.Errorf("the probe in the environment job of a merge request pipeline: %s, sees %v; want failed, naming %s", j.Status, seen(mrJobs, "probe-environment"), varOpen)
	}
	if got := seen(mrJobs, "probe-plain"); slices.Contains(got, varOpen) || slices.Contains(got, varProtected) {
		t.Errorf("a job without an environment sees the scoped %v", got)
	}
	// The worst key of all, unprotected and unscoped (Z), reaches every job
	// of a merge request pipeline: the probe must catch it with an
	// environment and without one (a probe job without an environment, the
	// fallback if prepare jobs ever stop getting scoped variables, rests on
	// this). Nothing asserted it before this check.
	if j := mrJobs["probe-plain"]; j.Status != "failed" || !slices.Contains(seen(mrJobs, "probe-plain"), varPlain) {
		t.Errorf("the probe without an environment in a merge request pipeline: %s, sees %v; want failed, naming %s", j.Status, seen(mrJobs, "probe-plain"), varPlain)
	}
	if got := seen(mrJobs, "probe-environment"); !slices.Contains(got, varPlain) {
		t.Errorf("the probe in the environment job of a merge request pipeline sees %v; want %s among them", got, varPlain)
	}
}

// ciJob is one job of a pipeline with its log.
type ciJob struct {
	ID     int64
	Status string
	Trace  string
}

// pipelineOf finds the pipeline of ref at sha in the project id.
func (e *liveEnv) pipelineOf(t testing.TB, id int64, ref, sha string) (int64, bool) {
	t.Helper()
	var ps []struct {
		ID  int64  `json:"id"`
		SHA string `json:"sha"`
	}
	e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/pipelines?ref=%s&order_by=id&sort=desc", id, ref), &ps)
	for _, p := range ps {
		if p.SHA == sha {
			return p.ID, true
		}
	}
	return 0, false
}

// mrPipeline finds the latest pipeline of merge request n of the project
// id.
func (e *liveEnv) mrPipeline(t testing.TB, id, n int64) (int64, bool) {
	t.Helper()
	var ps []struct {
		ID int64 `json:"id"`
	}
	e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/merge_requests/%d/pipelines", id, n), &ps)
	if len(ps) == 0 {
		return 0, false
	}
	return ps[0].ID, true
}

// pipelineJobs waits, at most five minutes, for the pipeline find finds
// and for its jobs to finish, and returns them by name with their logs.
func (e *liveEnv) pipelineJobs(t testing.TB, id int64, find func() (int64, bool)) map[string]ciJob {
	t.Helper()
	return e.pipelineJobsWithin(t, id, 5*time.Minute, find)
}

// pipelineJobsWithin is pipelineJobs with the time limit d.
func (e *liveEnv) pipelineJobsWithin(t testing.TB, id int64, d time.Duration, find func() (int64, bool)) map[string]ciJob {
	t.Helper()
	deadline := time.Now().Add(d)
	var pipeline int64
	for {
		if p, ok := find(); ok {
			pipeline = p
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pipeline in project %d after %s", id, d)
		}
		time.Sleep(2 * time.Second)
	}
	for {
		var p struct {
			Status string `json:"status"`
		}
		e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/pipelines/%d", id, pipeline), &p)
		switch p.Status {
		case "success", "failed", "canceled", "skipped":
			var jobs []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/pipelines/%d/jobs?per_page=100", id, pipeline), &jobs)
			out := map[string]ciJob{}
			for _, j := range jobs {
				resp := e.api(e.Root).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/jobs/%d/trace", id, j.ID), nil)
				out[j.Name] = ciJob{ID: j.ID, Status: j.Status, Trace: string(resp.Body)}
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline %d of project %d is %s after %s", pipeline, id, p.Status, d)
		}
		time.Sleep(2 * time.Second)
	}
}

// jobTokenLine matches what the job-token experiment's job prints: the
// job writes "JOB%s" with TOKEN, so that the command, which the log shows
// too, never matches. Runner 18 and later put a timestamp before every
// line of the log.
var jobTokenLine = regexp.MustCompile(`JOBTOKEN ([^\x1b\r\n]*)`)

// jobTokenReach is what one round of the job-token experiment found.
type jobTokenReach struct {
	// read are the API reads' statuses and git ls-remote's result, write
	// the git push's and the API write's.
	read, write string
	fields      map[string]string
}

// ciJobToken: the CI of a sync branch runs as the writer, and its job's
// CI_JOB_TOKEN acts with the writer's access to the other targets (threat
// T5); the experiment measures how far the job token allowlist cuts that.
//
// Target X's pipeline runs on a branch the writer pushes, as the writer
// does with a sync branch. Its job reaches target Y with CI_JOB_TOKEN:
// reads (the project, a file, the merge requests, git ls-remote) and
// writes (a git push of a new branch, a branch created through the API).
// The rounds: Y's job token settings as GitLab creates them; Y's inbound
// allowlist turned off, as on projects created before it became the
// default; the same with Y's "allow Git push requests" for job tokens on;
// and the allowlist on again with X in it. Only reads of a new project
// were once tried, while real targets are often older projects.
func ciJobToken(t *testing.T, fx *fixture) {
	e := fx.env
	y := fx.createProject(t, fx.nsID, "target-y", readme)
	x := fx.createProject(t, fx.nsID, "target-x", readme)
	e.waitAccess(t, x.ID, e.Reader, e.Writer, e.Person)
	e.waitAccess(t, y.ID, e.Reader, e.Writer, e.Person)
	var scope map[string]any
	resp := e.api(e.Root).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/job_token_scope", y.ID), nil)
	decode(t, "job_token_scope", resp, &scope)
	finding(t, "job-token-scope-default", "a new project's GET /job_token_scope: HTTP %d %v", resp.Status, scope)

	ci := fmt.Sprintf(`workflow:
  rules:
    - if: $CI_PIPELINE_SOURCE == "push" && $CI_COMMIT_BRANCH != $CI_DEFAULT_BRANCH
reach:
  variables:
    Y_ID: "%d"
    Y_PATH: "%s"
  script:
    - 'code() { curl -s -o /dev/null -w "%%{http_code}" -H "JOB-TOKEN: $CI_JOB_TOKEN" "$CI_API_V4_URL/$1"; }'
    - 'printf "JOB%%s user=%%s project=%%s file=%%s mrs=%%s\n" TOKEN "$GITLAB_USER_LOGIN" "$(code projects/$Y_ID)" "$(code "projects/$Y_ID/repository/files/README.md/raw?ref=main")" "$(code projects/$Y_ID/merge_requests)"'
    - 'H="Authorization: Basic $(printf "gitlab-ci-token:%%s" "$CI_JOB_TOKEN" | base64 -w0)"'
    - 'if git -c http.extraHeader="$H" ls-remote "$CI_SERVER_URL/$Y_PATH.git" main >/dev/null 2>&1; then r=ok; else r=refused; fi; printf "JOB%%s git=%%s\n" TOKEN "$r"'
    - 'tmp=$(mktemp -d) && git -C "$tmp" init -q && git -C "$tmp" -c user.name=job -c user.email=job@example.com commit -q --allow-empty -m jobtoken'
    - 'if git -C "$tmp" -c http.extraHeader="$H" push -q "$CI_SERVER_URL/$Y_PATH.git" "HEAD:refs/heads/jobtoken-$CI_PIPELINE_ID" >/dev/null 2>&1; then p=pushed; else p=refused; fi; w=$(curl -s -o /dev/null -w "%%{http_code}" -X POST -H "JOB-TOKEN: $CI_JOB_TOKEN" "$CI_API_V4_URL/projects/$Y_ID/repository/branches?branch=jobtoken-api-$CI_PIPELINE_ID&ref=main"); printf "JOB%%s push=%%s apiwrite=%%s\n" TOKEN "$p" "$w"'
`, y.ID, y.PathWithNamespace)
	e.commitFiles(t, e.Root, x.ID, "main", "", []conformance.File{{Path: ".gitlab-ci.yml", Content: []byte(ci)}}, "the target's pipeline")

	run := func(round string) jobTokenReach {
		t.Helper()
		branch := "touchmark/ci-" + round
		sha := e.commitFiles(t, e.Writer, x.ID, branch, "main", []conformance.File{{Path: round + ".md", Content: []byte(round + "\n")}}, "sync "+round)
		jobs := e.pipelineJobs(t, x.ID, func() (int64, bool) { return e.pipelineOf(t, x.ID, branch, sha) })
		j, ok := jobs["reach"]
		if !ok {
			t.Fatalf("round %s: no job reach in %v", round, jobs)
		}
		if e.Redact.Contains(j.Trace) {
			t.Errorf("round %s: the job's log shows a token", round)
		}
		var lines []string
		for _, m := range jobTokenLine.FindAllStringSubmatch(j.Trace, -1) {
			lines = append(lines, m[1])
		}
		if len(lines) != 3 {
			t.Fatalf("round %s: the job %s printed %q:\n%s", round, j.Status, lines, e.snippet([]byte(j.Trace)))
		}
		r := jobTokenReach{read: lines[0] + " " + lines[1], write: lines[2], fields: map[string]string{}}
		for _, f := range strings.Fields(strings.Join(lines, " ")) {
			if k, v, ok := strings.Cut(f, "="); ok {
				r.fields[k] = v
			}
		}
		return r
	}
	// blocked reports whether a round reached nothing of Y: no API read or
	// write answered 2xx, and git neither read nor pushed.
	blocked := func(r jobTokenReach) bool {
		for _, k := range []string{"project", "file", "mrs", "apiwrite"} {
			if strings.HasPrefix(r.fields[k], "2") {
				return false
			}
		}
		return r.fields["git"] == "refused" && r.fields["push"] == "refused"
	}
	before := run("default")
	finding(t, "job-token-default", "a pipeline of target X on a branch the writer pushed, against target Y with Y's default settings: %s; writes: %s", before.read, before.write)
	if before.fields["user"] != e.Writer.Login {
		t.Errorf("the pipeline of the writer's push runs as %q, want the writer %s", before.fields["user"], e.Writer.Login)
	}
	// What T5's mitigation rests on: with the allowlist on (the default of
	// a new project), X's job token reaches nothing of Y.
	if !blocked(before) {
		t.Errorf("with Y's default job token settings, X's job reached Y: %s; %s", before.read, before.write)
	}

	// Projects created before the allowlist became the default have it off.
	resp = e.api(e.Root).do(t, http.MethodPatch, fmt.Sprintf("/projects/%d/job_token_scope", y.ID), map[string]any{"enabled": false})
	finding(t, "job-token-scope-off", "PATCH /projects/Y/job_token_scope enabled=false (a legacy project's setting): HTTP %d %s", resp.Status, e.snippet(resp.Body))
	if resp.Status/100 != 2 {
		// The instance may enforce the allowlist for every project (the
		// administrator's setting enforce_ci_inbound_job_token_scope_enabled):
		// an instance upgraded with it off keeps older projects open.
		root := e.api(e.Root)
		set := root.do(t, http.MethodPut, "/application/settings", map[string]any{"enforce_ci_inbound_job_token_scope_enabled": false})
		finding(t, "job-token-enforced", "the instance enforces the allowlist; PUT /application/settings enforce_ci_inbound_job_token_scope_enabled=false: HTTP %d", set.Status)
		if set.Status/100 == 2 {
			t.Cleanup(func() {
				root.do(t, http.MethodPut, "/application/settings", map[string]any{"enforce_ci_inbound_job_token_scope_enabled": true})
			})
			resp = root.do(t, http.MethodPatch, fmt.Sprintf("/projects/%d/job_token_scope", y.ID), map[string]any{"enabled": false})
			finding(t, "job-token-scope-off-unenforced", "PATCH /projects/Y/job_token_scope enabled=false with the instance not enforcing: HTTP %d %s", resp.Status, e.snippet(resp.Body))
		}
	}
	if resp.Status/100 == 2 {
		legacy := run("legacy")
		finding(t, "job-token-legacy", "the same with Y's allowlist off: %s; writes: %s", legacy.read, legacy.write)
		resp = e.api(e.Root).do(t, http.MethodPut, fmt.Sprintf("/projects/%d", y.ID), map[string]any{"ci_push_repository_for_job_token_allowed": true})
		finding(t, "job-token-push-setting", "PUT /projects/Y ci_push_repository_for_job_token_allowed=true: HTTP %d", resp.Status)
		pushOn := run("legacy-push")
		finding(t, "job-token-legacy-push", "the same with Y's allowlist off and job token pushes allowed: %s; writes: %s", pushOn.read, pushOn.write)
		resp = e.api(e.Root).do(t, http.MethodPatch, fmt.Sprintf("/projects/%d/job_token_scope", y.ID), map[string]any{"enabled": true})
		if resp.Status/100 != 2 {
			t.Fatalf("PATCH /projects/Y/job_token_scope enabled=true: HTTP %d", resp.Status)
		}
	}
	resp = e.api(e.Root).do(t, http.MethodPost, fmt.Sprintf("/projects/%d/job_token_scope/allowlist", y.ID), map[string]any{"target_project_id": x.ID})
	finding(t, "job-token-allowlist-add", "POST /projects/Y/job_token_scope/allowlist with X: HTTP %d", resp.Status)
	after := run("allowed")
	finding(t, "job-token-allowed", "the same after Y lets X's job token in (job token pushes as the previous round left them): %s; writes: %s", after.read, after.write)
}
