//go:build e2e

package gitlabe2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/setup"
)

// TestTemplate runs the hub template's own .gitlab-ci.yml (gitlab.sh
// --template DIR) the way its README sets a hub up on GitLab, with every job
// in the touchmark image under test, run by a runner with the Docker
// executor as the image's own user:
//
//  1. the hub: the template's files in one commit, as GitLab's import of a
//     repository makes a project (no pipeline runs), in the group of hubs,
//     with the instance runners off; the one change is the image line, the
//     TODO(release) placeholder replaced with the image under test, in the
//     same NAME:TAG@sha256:<digest> form;
//  2. `touchmark setup gitlab` from a clone of the hub, with the person's
//     token (README "On GitLab", steps 2 to 4);
//  3. a merge request that describes the hub (README step 5: id, writer,
//     targets): its pipeline runs check, plan and probe, and all pass; the
//     plan's report, attached to the merge request, would open a merge
//     request in each of the two opted-in targets;
//  4. the merge: the default branch's pipeline runs distribute alone, which
//     opens those merge requests, by the writer, on touchmark/<id>;
//  5. GitLab's CI Lint of the template's file, as it ships: a static check
//     (valid, no warnings, the five jobs), and a simulated pipeline of the
//     default branch (distribute alone);
//  6. the schedules setup made, played as GitLab plays them: "touchmark
//     doctor" runs doctor alone, "touchmark distribute" runs distribute
//     alone, which finds nothing to change.
//
// No log and no output holds a token.
func TestTemplate(t *testing.T) {
	e := needLive(t)
	if e.Template == "" {
		t.Skip(envPrefix + "TEMPLATE is not set: gitlab.sh --template DIR runs the hub template's pipelines")
	}
	for _, v := range []string{"CI", "GITLAB_CI", "GITHUB_ACTIONS", "GITEA_ACTIONS", "FORGEJO_ACTIONS"} {
		t.Setenv(v, "")
	}
	tpl := readTemplate(t, e.Template)
	ciFile := string(tpl[".gitlab-ci.yml"])

	// The targets: two opted-in projects of a fresh subgroup; one adds a
	// pack in its opt-in file.
	fx := newGroup(t, e, "tmpl")
	billing := fx.createProject(t, fx.nsID, "billing", []conformance.File{{Path: config.DefaultOptIn, Content: []byte("version: 1\n")}})
	sdk := fx.createProject(t, fx.nsID, "sdk", []conformance.File{{Path: config.DefaultOptIn, Content: []byte("version: 1\npacks: [claude]\n")}})
	for _, p := range []apiProject{billing, sdk} {
		e.waitAccess(t, p.ID, e.Reader, e.Writer, e.Person)
	}

	// 1. The hub.
	name := "engineering-assets-" + randHex(t, 3)
	hub := e.createProject(t, map[string]any{
		"name": name, "path": name, "namespace_id": fx.groupID(t, e.HubGroup), "visibility": "private",
		"initialize_with_readme": false, "default_branch": "main", "shared_runners_enabled": false,
	})
	e.waitAccess(t, hub.ID, e.Person)
	imported := templateFiles(tpl, map[string]string{".gitlab-ci.yml": pinImage(t, ciFile, e.TouchmarkImage)})
	e.commitFiles(t, e.Person, hub.ID, "main", "", imported, "Initial commit\n\n[skip ci]")

	// 2. setup, from the maintainer's clone.
	dir := t.TempDir()
	gitCmd(t, dir, nil, e.gitAuth(e.Person), "clone", "-q", e.remote(hub.PathWithNamespace), ".")
	var outputs []string
	t.Setenv("TOUCHMARK_HUB_TOKEN", e.Person.Token)
	var stdout, stderr bytes.Buffer
	code := cli.Main(context.Background(), []string{"setup", "gitlab", "--hub", dir, "--group", fx.ns, "--url", e.URL, "--format", "json"}, &stdout, &stderr)
	outputs = append(outputs, stdout.String(), stderr.String())
	if code != 0 {
		t.Fatalf("touchmark setup gitlab: exit %d\nstdout:\n%s\nstderr:\n%s", code, e.Redact.Replace(stdout.String()), e.Redact.Replace(stderr.String()))
	}
	validate(t, "setup", stdout.Bytes(), e.Redact.Replace)
	var rep setup.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode the setup report: %v", err)
	}
	t.Setenv("TOUCHMARK_HUB_TOKEN", "")
	var steps []string
	for _, s := range rep.Steps {
		steps = append(steps, fmt.Sprintf("%s %s", s.Status, s.Name))
		if s.Status != setup.StatusOK && s.Status != setup.StatusDone {
			t.Errorf("setup: %s %s: %s", s.Status, s.Name, s.Detail)
		}
	}
	finding(t, "template-setup", "accounts %s, writer %s: %s", rep.Accounts, rep.Writer, strings.Join(steps, ", "))

	// 3. The merge request that describes the hub.
	id := "acme-" + randHex(t, 2)
	described := []conformance.File{
		{Path: config.HubFile, Content: []byte(describeHub(t, string(tpl[config.HubFile]), id, rep.Writer))},
		{Path: config.TargetsFile, Content: []byte(fmt.Sprintf(
			"version: 1\ndefaults:\n  packs: [agents]\ntargets:\n  - group: %s\n    packs: [python-service]\n", fx.ns))},
	}
	e.commitFiles(t, e.Person, hub.ID, "describe", "main", described, "Describe the hub")
	var mr apiMR
	e.api(e.Person).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", hub.ID), map[string]any{
		"source_branch": "describe", "target_branch": "main", "title": "Describe the hub", "target_project_id": hub.ID,
	}, &mr)
	// The first jobs pull the runner's helper image and the touchmark
	// image.
	mrJobs := e.pipelineJobsWithin(t, hub.ID, 15*time.Minute, func() (int64, bool) { return e.mrPipeline(t, hub.ID, mr.IID) })
	wantJobs(t, "the merge request pipeline", mrJobs, "check", "plan", "probe")
	planReport := jobReport(t, e, hub.ID, mrJobs["plan"], "touchmark-report.json")
	finding(t, "template-mr-pipeline", "check %s, plan %s, probe %s; plan: %s", mrJobs["check"].Status, mrJobs["plan"].Status,
		mrJobs["probe"].Status, outcomeLine(planReport))
	wantOutcomes(t, "plan", planReport, map[string]string{billing.PathWithNamespace: "opened", sdk.PathWithNamespace: "opened"})

	// 4. The merge, and distribute on the default branch.
	e.merge(t, e.Person, hub.ID, mr.IID)
	head, ok := e.branchHead(t, hub.ID, "main")
	if !ok {
		t.Fatal("the hub has no main")
	}
	mainJobs := e.pipelineJobsWithin(t, hub.ID, 15*time.Minute, func() (int64, bool) { return e.pipelineOf(t, hub.ID, "main", head) })
	wantJobs(t, "the default branch's pipeline", mainJobs, "distribute")
	distReport := jobReport(t, e, hub.ID, mainJobs["distribute"], "touchmark-report.json")
	finding(t, "template-distribute", "distribute %s: %s", mainJobs["distribute"].Status, outcomeLine(distReport))
	wantOutcomes(t, "distribute", distReport, map[string]string{billing.PathWithNamespace: "opened", sdk.PathWithNamespace: "opened"})
	for _, p := range []apiProject{billing, sdk} {
		mrs := e.mrs(t, p.ID, "opened")
		if len(mrs) != 1 || mrs[0].SourceBranch != "touchmark/"+id || mrs[0].Author.Username != rep.Writer {
			var got []string
			for _, m := range mrs {
				got = append(got, fmt.Sprintf("!%d %s by %s", m.IID, m.SourceBranch, m.Author.Username))
			}
			t.Errorf("%s: open merge requests %v, want one on touchmark/%s by %s", p.PathWithNamespace, got, id, rep.Writer)
			continue
		}
		if !e.fileExists(t, p.ID, "touchmark/"+id, "AGENTS.md") || !e.fileExists(t, p.ID, "touchmark/"+id, "docs/guidelines/database.md") {
			t.Errorf("%s: the sync branch lacks the packs' files", p.PathWithNamespace)
		}
	}
	if !e.fileExists(t, sdk.ID, "touchmark/"+id, "CLAUDE.md") || e.fileExists(t, billing.ID, "touchmark/"+id, "CLAUDE.md") {
		t.Error("CLAUDE.md, of the pack sdk's opt-in file adds, is not where it belongs")
	}

	// 5. CI Lint. Not before the merge: a simulated pipeline of the
	// default branch runs no job while its tip is the import commit, whose
	// message says [skip ci].
	ciLint(t, e, hub.ID, ciFile)

	// 6. The schedules.
	docJobs := e.playSchedule(t, hub.ID, "touchmark doctor")
	wantJobs(t, "the doctor schedule's pipeline", docJobs, "doctor")
	doctor := jobArtifact(t, e, hub.ID, docJobs["doctor"], "touchmark-doctor.json")
	var doc report.Doctor
	if err := json.Unmarshal(doctor, &doc); err != nil {
		t.Errorf("decode touchmark-doctor.json: %v", err)
	}
	finding(t, "template-doctor", "doctor %s, checks %v", docJobs["doctor"].Status, doc.Summary)
	if doc.Summary[report.StatusFail] > 0 {
		t.Errorf("doctor: %v", doc.Summary)
	}
	distJobs := e.playSchedule(t, hub.ID, "touchmark distribute")
	wantJobs(t, "the distribute schedule's pipeline", distJobs, "distribute")
	again := jobReport(t, e, hub.ID, distJobs["distribute"], "touchmark-report.json")
	finding(t, "template-distribute-again", "distribute %s: %s", distJobs["distribute"].Status, outcomeLine(again))
	wantOutcomes(t, "the second distribute", again, map[string]string{billing.PathWithNamespace: "unchanged", sdk.PathWithNamespace: "unchanged"})

	// No token in any job log or output.
	for _, jobs := range []map[string]ciJob{mrJobs, mainJobs, docJobs, distJobs} {
		for name, j := range jobs {
			outputs = append(outputs, j.Trace)
			if strings.Contains(j.Trace, "glpat-") {
				t.Errorf("the log of %s shows a token", name)
			}
		}
	}
	for i, o := range outputs {
		for _, a := range []account{e.Root, e.Reader, e.Writer, e.Person} {
			if strings.Contains(o, a.Token) {
				t.Errorf("output %d holds the token of %s", i, a.Login)
			}
		}
	}
}

// readTemplate reads the template's working tree: every regular file but
// those under .git, by slash path.
func readTemplate(t *testing.T, root string) map[string][]byte {
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
	for _, f := range []string{".gitlab-ci.yml", config.HubFile, config.TargetsFile} {
		if _, ok := files[f]; !ok {
			t.Fatalf("the template has no %s", f)
		}
	}
	return files
}

// templateFiles returns the template's files in path order, with the
// contents of replace in place of theirs.
func templateFiles(tpl map[string][]byte, replace map[string]string) []conformance.File {
	paths := make([]string, 0, len(tpl))
	for p := range tpl {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files := make([]conformance.File, 0, len(paths))
	for _, p := range paths {
		content := tpl[p]
		if r, ok := replace[p]; ok {
			content = []byte(r)
		}
		files = append(files, conformance.File{Path: p, Content: content})
	}
	return files
}

// placeholderImage is the template's image line before a release pins it.
var placeholderImage = regexp.MustCompile(`(?m)^(\s*name: )ghcr\.io/bedrock-python/touchmark:[^@\s]+@sha256:0{64}$`)

// pinImage replaces the one placeholder image of the template's
// .gitlab-ci.yml with image, as a release's update does.
func pinImage(t *testing.T, ci, image string) string {
	t.Helper()
	if n := len(placeholderImage.FindAllString(ci, -1)); n != 1 {
		t.Fatalf("the template's .gitlab-ci.yml has %d placeholder image lines (name: ghcr.io/bedrock-python/touchmark:<tag>@sha256:0…), want 1", n)
	}
	return placeholderImage.ReplaceAllString(ci, "${1}"+image)
}

// describeHub sets id and writer in the template's hub.yml, as README step
// 5 asks: the lines `id: change-me` and `# writer: …`.
func describeHub(t *testing.T, hubYML, id, writer string) string {
	t.Helper()
	idLine := regexp.MustCompile(`(?m)^id: change-me$`)
	writerLine := regexp.MustCompile(`(?m)^# writer: \S+$`)
	if len(idLine.FindAllString(hubYML, -1)) != 1 || len(writerLine.FindAllString(hubYML, -1)) != 1 {
		t.Fatalf("the template's hub.yml has no single `id: change-me` and `# writer: …` line")
	}
	out := idLine.ReplaceAllString(hubYML, "id: "+id)
	return writerLine.ReplaceAllString(out, "writer: "+writer)
}

// ciLint checks the template's .gitlab-ci.yml, as it ships, with GitLab's
// CI Lint of the hub project: valid without warnings, every job known, and
// a push to the default branch runs distribute alone.
func ciLint(t *testing.T, e *liveEnv, project int64, ci string) {
	t.Helper()
	type lintJob struct {
		Name  string `json:"name"`
		Stage string `json:"stage"`
	}
	var static, simulated struct {
		Valid    bool      `json:"valid"`
		Errors   []string  `json:"errors"`
		Warnings []string  `json:"warnings"`
		Jobs     []lintJob `json:"jobs"`
	}
	path := fmt.Sprintf("/projects/%d/ci/lint", project)
	e.api(e.Person).ok(t, http.MethodPost, path, map[string]any{"content": ci, "include_jobs": true}, &static)
	e.api(e.Person).ok(t, http.MethodPost, path, map[string]any{"content": ci, "include_jobs": true, "dry_run": true, "dry_run_ref": "main"}, &simulated)
	names := func(jobs []lintJob) []string {
		var out []string
		for _, j := range jobs {
			out = append(out, j.Stage+"/"+j.Name)
		}
		sort.Strings(out)
		return out
	}
	finding(t, "template-ci-lint", "static: valid %v, errors %v, warnings %v, jobs %v; a push to main: valid %v, errors %v, warnings %v, jobs %v",
		static.Valid, static.Errors, static.Warnings, names(static.Jobs), simulated.Valid, simulated.Errors, simulated.Warnings, names(simulated.Jobs))
	if !static.Valid || len(static.Errors) > 0 || len(static.Warnings) > 0 {
		t.Errorf("CI Lint: valid %v, errors %v, warnings %v", static.Valid, static.Errors, static.Warnings)
	}
	if got, want := names(static.Jobs), []string{"check/check", "check/plan", "check/probe", "deliver/distribute", "deliver/doctor"}; !slices.Equal(got, want) {
		t.Errorf("CI Lint jobs %v, want %v", got, want)
	}
	if !simulated.Valid || len(simulated.Errors) > 0 || !slices.Equal(names(simulated.Jobs), []string{"deliver/distribute"}) {
		t.Errorf("CI Lint of a push to main: valid %v, errors %v, jobs %v; want distribute alone", simulated.Valid, simulated.Errors, names(simulated.Jobs))
	}
}

// wantJobs checks that the pipeline ran exactly the jobs names and that each
// succeeded.
func wantJobs(t *testing.T, what string, jobs map[string]ciJob, names ...string) {
	t.Helper()
	var got []string
	for name := range jobs {
		got = append(got, name)
	}
	sort.Strings(got)
	if !slices.Equal(got, names) {
		t.Errorf("%s ran %v, want %v", what, got, names)
	}
	for _, name := range names {
		if j, ok := jobs[name]; ok && j.Status != "success" {
			t.Errorf("%s: job %s %s\n%s", what, name, j.Status, live.Redact.Replace(j.Trace))
		}
	}
}

// jobArtifact reads the file p of a job's artifacts.
func jobArtifact(t *testing.T, e *liveEnv, project int64, j ciJob, p string) []byte {
	t.Helper()
	resp := e.api(e.Person).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/jobs/%d/artifacts/%s", project, j.ID, p), nil)
	if resp.Status != http.StatusOK {
		t.Errorf("artifact %s of job %d: HTTP %d", p, j.ID, resp.Status)
		return nil
	}
	return resp.Body
}

// jobReport reads a job's JSON report from its artifacts and checks it
// against the report schema.
func jobReport(t *testing.T, e *liveEnv, project int64, j ciJob, p string) report.Delivery {
	t.Helper()
	var rep report.Delivery
	data := jobArtifact(t, e, project, j, p)
	if data == nil {
		return rep
	}
	validate(t, "report", data, e.Redact.Replace)
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Errorf("decode %s: %v", p, err)
	}
	return rep
}

// outcomeLine lists a report's targets as path=outcome.
func outcomeLine(rep report.Delivery) string {
	var out []string
	for _, tg := range rep.Targets {
		out = append(out, tg.Path+"="+string(tg.Outcome))
	}
	sort.Strings(out)
	return fmt.Sprintf("outcome %s, targets %v", rep.Outcome, out)
}

// wantOutcomes checks the outcome of every target of a report.
func wantOutcomes(t *testing.T, what string, rep report.Delivery, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, tg := range rep.Targets {
		got[tg.Path] = string(tg.Outcome)
	}
	for p, o := range want {
		if got[p] != o {
			t.Errorf("%s: %s is %q, want %q (%s)", what, p, got[p], o, outcomeLine(rep))
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: %s, want %d targets", what, outcomeLine(rep), len(want))
	}
}

// playSchedule runs the pipeline schedule described desc now, as its
// owner, and returns the jobs of the pipeline it starts.
func (e *liveEnv) playSchedule(t *testing.T, project int64, desc string) map[string]ciJob {
	t.Helper()
	var schedules []struct {
		ID          int64  `json:"id"`
		Description string `json:"description"`
	}
	e.api(e.Person).get(t, fmt.Sprintf("/projects/%d/pipeline_schedules", project), &schedules)
	var sid int64
	for _, s := range schedules {
		if s.Description == desc {
			sid = s.ID
		}
	}
	if sid == 0 {
		t.Fatalf("no pipeline schedule %q: %+v", desc, schedules)
	}
	latest := func() int64 {
		var ps []struct {
			ID int64 `json:"id"`
		}
		e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/pipelines?source=schedule&order_by=id&sort=desc&per_page=1", project), &ps)
		if len(ps) == 0 {
			return 0
		}
		return ps[0].ID
	}
	before := latest()
	e.api(e.Person).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/pipeline_schedules/%d/play", project, sid), nil, nil)
	return e.pipelineJobsWithin(t, project, 15*time.Minute, func() (int64, bool) {
		p := latest()
		return p, p > before
	})
}
