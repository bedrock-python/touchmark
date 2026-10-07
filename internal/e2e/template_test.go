//go:build e2e

package e2e

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
)

// TestTemplate runs the hub template's .gitea workflows (gitea.sh
// --template DIR) the way its README sets a hub up on Gitea: every job in
// the touchmark image under test, as the image's user, on Gitea's runner.
//
//  1. the hub: the template's files in one commit on main, as a migration
//     makes it (the commit skips CI), in the organisation of hubs; the one
//     change is the image of the jobs, the template's pin (a release's
//     digest, or the TODO(release) placeholder before the first release)
//     replaced with the image under test in the same NAME:TAG@sha256 form;
//     the reader's and the writer's tokens as the Actions secrets
//     TOUCHMARK_READ_TOKEN and TOUCHMARK_WRITE_TOKEN (README "On Gitea or
//     Forgejo", steps 1 and 2);
//  2. a pull request that describes the hub (step 5: id, writer, and
//     security.write_isolation none with a reason, which Gitea needs):
//     check and plan run and pass, distribute is skipped, and plan keeps
//     its report in a comment of the pull request through the job token;
//  3. the merge: on the push to main distribute alone runs, and opens a
//     pull request by the writer in each of the two opted-in targets;
//  4. Run workflow of engineering-assets-doctor.yml: doctor passes; Run
//     workflow of engineering-assets.yml: distribute finds nothing to do.
//
// No job log holds a token.
func TestTemplate(t *testing.T) {
	e := needLive(t)
	if e.Template == "" {
		t.Skip("TOUCHMARK_E2E_TEMPLATE is not set: gitea.sh --template DIR runs the hub template's workflows")
	}
	tpl := readTemplate(t, e.Template)
	workflow, doctorWorkflow := ".gitea/workflows/engineering-assets.yml", ".gitea/workflows/engineering-assets-doctor.yml"

	// The targets.
	fx := newOrg(t, e, "tmpl")
	targets := map[string]string{"billing": "version: 1\n", "sdk": "version: 1\npacks: [claude]\n"}
	for _, name := range []string{"billing", "sdk"} {
		fx.createRepo(t, fx.org, name, []conformance.File{{Path: config.DefaultOptIn, Content: []byte(targets[name])}})
		fx.admin().ok(t, http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s/%s", fx.writeTeam, fx.org, name), nil, nil)
	}

	// 1. The hub.
	hub := e.HubOrg + "/engineering-assets-" + randHex(t, 3)
	fx.admin().ok(t, http.MethodPost, "/orgs/"+e.HubOrg+"/repos", map[string]any{
		"name": strings.TrimPrefix(hub, e.HubOrg+"/"), "private": true, "auto_init": false, "default_branch": "main",
	}, nil)
	replace := map[string]string{}
	for _, f := range []string{workflow, doctorWorkflow} {
		replace[f] = pinImages(t, f, string(tpl[f]), e.TouchmarkImage)
	}
	e.pushFiles(t, e.Admin, hub, "main", templateFiles(tpl, replace), "Initial commit\n\n[skip ci]")
	for name, a := range map[string]account{"TOUCHMARK_READ_TOKEN": e.Reader, "TOUCHMARK_WRITE_TOKEN": e.Writer} {
		fx.admin().ok(t, http.MethodPut, "/repos/"+hub+"/actions/secrets/"+name, map[string]any{"data": a.Token}, nil)
	}

	// 2. The pull request that describes the hub.
	id := "acme-" + randHex(t, 2)
	targetsYML := fmt.Sprintf("version: 1\ndefaults:\n  packs: [agents]\ntargets:\n  - repo: %s/billing\n    packs: [python-service]\n  - repo: %s/sdk\n    packs: [python-service]\n", fx.org, fx.org)
	e.pushBranch(t, e.Person, hub, "main", "describe")
	e.pushFiles(t, e.Person, hub, "describe", []conformance.File{
		{Path: config.HubFile, Content: []byte(describeHub(t, string(tpl[config.HubFile]), id, e.Writer.Login))},
		{Path: config.TargetsFile, Content: []byte(targetsYML)},
	}, "Describe the hub")
	var pr apiPR
	e.api(e.Person).ok(t, http.MethodPost, "/repos/"+hub+"/pulls", map[string]any{"head": "describe", "base": "main", "title": "Describe the hub"}, &pr)
	prRun := e.waitRun(t, hub, "pull_request", "", 15*time.Minute)
	wantRunJobs(t, e, hub, "the pull request's run", prRun, map[string]string{"check": "success", "plan": "success", "distribute": "skipped"})
	// A pull request from the hub's own branch gets the secrets: its plan
	// runs with --strict (one from a fork would not).
	for _, j := range prRun.jobs {
		if j.Name != "plan" {
			continue
		}
		resp := e.api(e.Admin).do(t, http.MethodGet, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", hub, j.ID), nil)
		if !strings.Contains(string(resp.Body), "touchmark plan --hub . --comment --strict") {
			t.Errorf("the plan job of a pull request from the hub's own branch ran without --strict:\n%s", e.Redact.Replace(string(resp.Body)))
		}
	}
	var comments []apiComment
	e.api(e.Admin).get(t, fmt.Sprintf("/repos/%s/issues/%d/comments", hub, pr.Number), &comments)
	var planComment string
	for _, c := range comments {
		if strings.Contains(c.Body, "touchmark") {
			planComment = c.Body
		}
	}
	finding(t, "template-pr-run", "check, plan, distribute: %s; plan's comment by %d comments: %q", prRun.summary, len(comments), firstLine(planComment))
	if planComment == "" {
		t.Errorf("plan --comment left no comment on #%d", pr.Number)
	}
	for _, name := range []string{"billing", "sdk"} {
		if !strings.Contains(planComment, fx.org+"/"+name) {
			t.Errorf("the plan comment does not name %s/%s", fx.org, name)
		}
	}

	// 3. The merge, and distribute on main.
	e.merge(t, e.Person, hub, pr.Number)
	var main apiBranch
	e.api(e.Admin).get(t, "/repos/"+hub+"/branches/main", &main)
	pushRun := e.waitRun(t, hub, "push", main.Commit.ID, 15*time.Minute)
	wantRunJobs(t, e, hub, "the push's run", pushRun, map[string]string{"check": "skipped", "plan": "skipped", "distribute": "success"})
	finding(t, "template-push-run", "check, plan, distribute: %s", pushRun.summary)
	for _, name := range []string{"billing", "sdk"} {
		prs := fx.pulls(t, fx.org+"/"+name, "open")
		if len(prs) != 1 || prs[0].Head.Ref != "touchmark/"+id || prs[0].User.Login != e.Writer.Login {
			var got []string
			for _, p := range prs {
				got = append(got, fmt.Sprintf("#%d %s by %s", p.Number, p.Head.Ref, p.User.Login))
			}
			t.Errorf("%s/%s: open pull requests %v, want one on touchmark/%s by %s", fx.org, name, got, id, e.Writer.Login)
		}
	}

	// 4. Run workflow.
	for _, f := range []string{"engineering-assets-doctor.yml", "engineering-assets.yml"} {
		e.api(e.Person).ok(t, http.MethodPost, "/repos/"+hub+"/actions/workflows/"+f+"/dispatches", map[string]any{"ref": "main"}, nil)
		run := e.waitRun(t, hub, "workflow_dispatch", "", 15*time.Minute, f)
		want := map[string]string{"doctor": "success"}
		if f == "engineering-assets.yml" {
			want = map[string]string{"check": "skipped", "plan": "skipped", "distribute": "success"}
		}
		wantRunJobs(t, e, hub, "Run workflow of "+f, run, want)
		finding(t, "template-dispatch-"+strings.TrimSuffix(f, ".yml"), "%s", run.summary)
	}
	for _, name := range []string{"billing", "sdk"} {
		if n := len(fx.pulls(t, fx.org+"/"+name, "all")); n != 1 {
			t.Errorf("%s/%s has %d pull requests after the second distribute, want 1", fx.org, name, n)
		}
	}
}

// actionsRun is one run of a workflow and its jobs.
type actionsRun struct {
	ID      int64
	Path    string
	jobs    []actionsJob
	summary string
}

type actionsJob struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// waitRun waits for the newest run of the hub's workflows for event (and
// commit sha, and the workflow file, when given) to complete, and returns
// it with its jobs.
func (e *liveEnv) waitRun(t *testing.T, hub, event, sha string, d time.Duration, workflow ...string) actionsRun {
	t.Helper()
	deadline := time.Now().Add(d)
	q := url.Values{"event": {event}}
	if sha != "" {
		q.Set("head_sha", sha)
	}
	for {
		var runs struct {
			Runs []struct {
				ID         int64  `json:"id"`
				Path       string `json:"path"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				Event      string `json:"event"`
				HeadSHA    string `json:"head_sha"`
			} `json:"workflow_runs"`
		}
		e.api(e.Admin).get(t, "/repos/"+hub+"/actions/runs?"+q.Encode(), &runs)
		for _, r := range runs.Runs {
			if r.Event != event || (sha != "" && r.HeadSHA != sha) || (len(workflow) > 0 && !strings.HasPrefix(r.Path, workflow[0])) {
				continue
			}
			if r.Status != "completed" {
				break
			}
			run := actionsRun{ID: r.ID, Path: r.Path}
			var jobs struct {
				Jobs []actionsJob `json:"jobs"`
			}
			e.api(e.Admin).get(t, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", hub, r.ID), &jobs)
			run.jobs = jobs.Jobs
			var parts []string
			for _, j := range jobs.Jobs {
				parts = append(parts, j.Name+" "+j.Conclusion)
			}
			sort.Strings(parts)
			run.summary = fmt.Sprintf("run %d (%s, %s): %s", r.ID, r.Path, r.Conclusion, strings.Join(parts, ", "))
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("no completed %s run of %s after %s: %+v", event, hub, d, runs.Runs)
		}
		time.Sleep(3 * time.Second)
	}
}

// wantRunJobs checks the conclusion of every job of a run, and that no job
// log holds a token.
func wantRunJobs(t *testing.T, e *liveEnv, hub, what string, run actionsRun, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, j := range run.jobs {
		got[j.Name] = j.Conclusion
		resp := e.api(e.Admin).do(t, http.MethodGet, fmt.Sprintf("/repos/%s/actions/jobs/%d/logs", hub, j.ID), nil)
		log := string(resp.Body)
		for _, a := range []account{e.Admin, e.Reader, e.Writer, e.Person} {
			if strings.Contains(log, a.Token) {
				t.Errorf("%s: the log of %s holds the token of %s", what, j.Name, a.Login)
			}
		}
		if want[j.Name] != "" && want[j.Name] != j.Conclusion {
			t.Errorf("%s: job %s %s, want %s\n%s", what, j.Name, j.Conclusion, want[j.Name], e.Redact.Replace(log))
		}
	}
	var names, wantNames []string
	for n := range got {
		names = append(names, n)
	}
	for n := range want {
		wantNames = append(wantNames, n)
	}
	sort.Strings(names)
	sort.Strings(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Errorf("%s ran %v, want %v (%s)", what, names, wantNames, run.summary)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
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

// templateImage is a job's touchmark image in the template: a release's,
// pinned by digest, or before the first release the TODO(release)
// placeholder, whose digest is all zeros.
var templateImage = regexp.MustCompile(`(?m)^(\s*image: )ghcr\.io/bedrock-python/touchmark:[^@\s]+@sha256:[0-9a-f]{64}$`)

// pinImages replaces every touchmark image of a workflow with image, as a
// release's update does; the workflow must have one per job.
func pinImages(t *testing.T, name, workflow, image string) string {
	t.Helper()
	if len(templateImage.FindAllString(workflow, -1)) == 0 {
		t.Fatalf("%s has no touchmark image (image: ghcr.io/bedrock-python/touchmark:<tag>@sha256:<digest>)", name)
	}
	return templateImage.ReplaceAllString(workflow, "${1}"+image)
}

// describeHub sets id and writer in the template's hub.yml, as README step
// 5 asks, and the write isolation Gitea Actions needs: none, with the
// template's own reason.
func describeHub(t *testing.T, hubYML, id, writer string) string {
	t.Helper()
	for _, r := range []struct {
		re  *regexp.Regexp
		new string
	}{
		{regexp.MustCompile(`(?m)^id: change-me$`), "id: " + id},
		{regexp.MustCompile(`(?m)^# writer: \S+$`), "writer: " + writer},
		{regexp.MustCompile(`(?m)^  write_isolation: platform$`), "  write_isolation: none"},
		{regexp.MustCompile(`(?m)^  # reason: (".*")$`), "  reason: $1"},
	} {
		if n := len(r.re.FindAllString(hubYML, -1)); n != 1 {
			t.Fatalf("the template's hub.yml has %d lines matching %s, want 1", n, r.re)
		}
		hubYML = r.re.ReplaceAllString(hubYML, r.new)
	}
	return hubYML
}
