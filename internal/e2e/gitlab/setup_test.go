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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/setup"
)

// TestSetup runs `touchmark setup gitlab` as a maintainer
// does: with the person's token (an Owner of the seeded group, whose fresh
// subgroup holds the targets, and of the group of the hub),
// against a hub created as GitLab creates projects (the default branch
// protected for Maintainers) that keeps a stale write key, a token made by
// hand, in an unprotected variable of every environment.
//
// The first run makes the reader and the writer (service accounts where
// the instance lets the person create them, else group access tokens) with
// their roles, the variables (the reader's masked and unprotected, the
// writer's protected, masked and hidden and scoped to touchmark-distribute;
// the stale key revoked and deleted), the environment, the default branch
// that No one pushes to, no pipeline variables, no protected variables in
// merge request pipelines and the two schedules. A second run and a dry run
// change nothing (no token rotated). doctor --hub-token grades the hub ok.
// With the runner, a merge request sets the writer in hub.yml and adds a
// pipeline; on the default branch its job without an environment plans
// with the reader's variable and does not see the write key (probe), and
// its job in the environment runs doctor with the writer's variable. No
// token reaches any output.
func TestSetup(t *testing.T) {
	e := needLive(t)
	for _, v := range []string{"CI", "GITLAB_CI", "GITHUB_ACTIONS", "GITEA_ACTIONS", "FORGEJO_ACTIONS"} {
		t.Setenv(v, "")
	}
	fx := newGroup(t, e, "setup")
	root := e.api(e.Root)
	optIn := []conformance.File{{Path: config.DefaultOptIn, Content: []byte("version: 1\n")}}
	target := fx.createProject(t, fx.nsID, "service", optIn)
	e.waitAccess(t, target.ID, e.Person)

	// An Owner of the hub's group: the check of where the hub keeps its keys
	// reads the group's variables, which GitLab shows to Owners only.
	hubGroup := fx.sideGroup(t, "hub", map[int64]int{e.Person.ID: levelOwner})
	id := "e2e-" + randHex(t, 3)
	hubFiles := func(writer, ci string) []conformance.File {
		hubYML := fmt.Sprintf("version: 1\nid: %s\nproviders:\n  - id: gl\n    type: gitlab\n    url: %s\n", id, e.URL)
		if writer != "" {
			hubYML += "    writer: " + writer + "\n"
		}
		files := []conformance.File{
			{Path: config.HubFile, Content: []byte(hubYML)},
			{Path: config.TargetsFile, Content: []byte(fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - group: %s\n", fx.ns))},
			{Path: "packs/base/AGENTS.md", Content: []byte(text("base AGENTS.md v1"))},
		}
		if ci != "" {
			files = append(files, conformance.File{Path: ".gitlab-ci.yml", Content: []byte(ci)})
		}
		return files
	}
	hub := fx.createProject(t, fx.groupID(t, hubGroup), "hub", hubFiles("", ""))
	e.waitAccess(t, hub.ID, e.Person)
	// The stale write key is a token made by hand (the person's, as root
	// mints it), not setup's own: setup must revoke it, not only delete
	// the copy, since every branch's jobs could read it.
	var handMade struct {
		Token string `json:"token"`
	}
	root.ok(t, http.MethodPost, fmt.Sprintf("/users/%d/personal_access_tokens", e.Person.ID), map[string]any{
		"name": "a writer made by hand", "scopes": []string{"api"}, "expires_at": time.Now().AddDate(0, 0, 3).Format("2006-01-02")}, &handMade)
	stale := handMade.Token
	if stale == "" {
		t.Fatal("root minted no token")
	}
	e.Redact.Add(stale)
	root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/variables", hub.ID), map[string]any{
		"key": "TOUCHMARK_WRITE_TOKEN", "value": stale, "protected": false, "masked": true, "environment_scope": "*"}, nil)

	// The maintainer's checkout: hub.yml and the origin remote are what
	// setup reads.
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q", "-b", "main")
	for _, f := range hubFiles("", "") {
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

	var outputs []string
	run := func(code int, cmd string, extra ...string) []byte {
		t.Helper()
		t.Setenv("TOUCHMARK_HUB_TOKEN", e.Person.Token)
		args := append([]string{cmd, "--hub", dir}, extra...)
		var stdout, stderr bytes.Buffer
		got := cli.Main(context.Background(), args, &stdout, &stderr)
		outputs = append(outputs, stdout.String(), stderr.String())
		if got != code {
			t.Fatalf("touchmark %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, code,
				e.Redact.Replace(stdout.String()), e.Redact.Replace(stderr.String()))
		}
		return stdout.Bytes()
	}
	setupRun := func(extra ...string) setup.Report {
		t.Helper()
		out := run(0, "setup", append([]string{"gitlab", "--group", fx.ns, "--format", "json"}, extra...)...)
		validate(t, "setup", out, e.Redact.Replace)
		var rep setup.Report
		if err := json.Unmarshal(out, &rep); err != nil {
			t.Fatalf("decode the setup report: %v", err)
		}
		return rep
	}
	stepLines := func(rep setup.Report) string {
		var lines []string
		for _, s := range rep.Steps {
			lines = append(lines, fmt.Sprintf("%s %s: %s", s.Status, s.Name, s.Detail))
		}
		return strings.Join(lines, "\n")
	}

	rep := setupRun()
	finding(t, "setup-first-run", "accounts %s, reader %s, writer %s:\n%s", rep.Accounts, rep.Reader, rep.Writer, stepLines(rep))
	for _, s := range rep.Steps {
		if s.Status == setup.StatusFail || s.Status == setup.StatusManual || s.Status == setup.StatusUnknown {
			t.Errorf("first run: %s %s: %s", s.Status, s.Name, s.Detail)
		}
		if s.Name == "writer-variable" && !strings.Contains(s.Detail, `revoked the token TOUCHMARK_WRITE_TOKEN with environment scope "*" held`) {
			t.Errorf("first run: writer-variable %q does not say it revoked the stale key", s.Detail)
		}
	}
	if resp := e.api(account{Login: "the stale key", Token: stale}).do(t, http.MethodGet, "/personal_access_tokens/self", nil); resp.Status != http.StatusUnauthorized {
		t.Errorf("the stale write key still works: GET /personal_access_tokens/self HTTP %d", resp.Status)
	}

	// What GitLab holds now, as root reads it.
	type variable struct {
		Key       string `json:"key"`
		Scope     string `json:"environment_scope"`
		Protected bool   `json:"protected"`
		Masked    bool   `json:"masked"`
		Hidden    *bool  `json:"hidden"`
	}
	vars := all[variable](t, root, fmt.Sprintf("/projects/%d/variables", hub.ID))
	find := func(key, scope string) *variable {
		for i := range vars {
			if vars[i].Key == key && vars[i].Scope == scope {
				return &vars[i]
			}
		}
		return nil
	}
	rv, wv := find("TOUCHMARK_READ_TOKEN", "*"), find("TOUCHMARK_WRITE_TOKEN", setup.Environment)
	switch {
	case rv == nil || rv.Protected || !rv.Masked:
		t.Errorf("TOUCHMARK_READ_TOKEN: %+v", rv)
	case wv == nil || !wv.Protected || !wv.Masked:
		t.Errorf("TOUCHMARK_WRITE_TOKEN: %+v", wv)
	case find("TOUCHMARK_WRITE_TOKEN", "*") != nil:
		t.Error("the stale write key of every environment is still there")
	}
	hidden := func(v *variable) string {
		if v == nil || v.Hidden == nil {
			return "not shown"
		}
		return fmt.Sprint(*v.Hidden)
	}
	finding(t, "setup-variables", "reader hidden %s, writer hidden %s", hidden(rv), hidden(wv))
	if wv != nil && wv.Hidden != nil && !*wv.Hidden {
		t.Error("the write key is not hidden")
	}
	var envs []struct {
		Name string `json:"name"`
	}
	root.get(t, fmt.Sprintf("/projects/%d/environments?name=%s", hub.ID, setup.Environment), &envs)
	if len(envs) != 1 {
		t.Errorf("environments %v", envs)
	}
	var pb struct {
		Push []struct {
			Level int `json:"access_level"`
		} `json:"push_access_levels"`
		Merge []struct {
			Level int `json:"access_level"`
		} `json:"merge_access_levels"`
		Force bool `json:"allow_force_push"`
	}
	root.get(t, fmt.Sprintf("/projects/%d/protected_branches/main", hub.ID), &pb)
	if len(pb.Push) != 1 || pb.Push[0].Level != 0 || len(pb.Merge) != 1 || pb.Merge[0].Level != levelMaintainer || pb.Force {
		t.Errorf("main's protection %+v", pb)
	}
	var settings map[string]any
	root.get(t, fmt.Sprintf("/projects/%d", hub.ID), &settings)
	if settings["ci_pipeline_variables_minimum_override_role"] != "no_one_allowed" {
		t.Errorf("pipeline variables: %v", settings["ci_pipeline_variables_minimum_override_role"])
	}
	if v, ok := settings["protect_merge_request_pipelines"]; ok && v != false {
		t.Errorf("protect_merge_request_pipelines: %v", v)
	}
	finding(t, "setup-settings", "protect_merge_request_pipelines %v, restrict_user_defined_variables %v",
		settings["protect_merge_request_pipelines"], settings["restrict_user_defined_variables"])
	var schedules []struct {
		Description string `json:"description"`
		Ref         string `json:"ref"`
		Active      bool   `json:"active"`
	}
	root.get(t, fmt.Sprintf("/projects/%d/pipeline_schedules", hub.ID), &schedules)
	if len(schedules) != 2 {
		t.Errorf("schedules %+v", schedules)
	}
	// The accounts: their roles in the group of targets, none on the hub.
	var reader, writer apiUser
	for _, a := range []struct {
		login string
		level int
		out   *apiUser
	}{{rep.Reader, levelReporter, &reader}, {rep.Writer, levelDeveloper, &writer}} {
		var users []apiUser
		root.get(t, "/users?username="+a.login, &users)
		if len(users) != 1 {
			t.Fatalf("user %s: %v", a.login, users)
		}
		*a.out = users[0]
		var m apiMember
		root.get(t, fmt.Sprintf("/groups/%d/members/all/%d", fx.nsID, users[0].ID), &m)
		if m.AccessLevel != a.level {
			t.Errorf("%s: level %d in %s, want %d", a.login, m.AccessLevel, fx.ns, a.level)
		}
		if resp := root.do(t, http.MethodGet, fmt.Sprintf("/projects/%d/members/all/%d", hub.ID, users[0].ID), nil); resp.Status != http.StatusNotFound {
			t.Errorf("%s reaches the hub: HTTP %d", a.login, resp.Status)
		}
	}
	tokens := func() string {
		t.Helper()
		var list []struct {
			ID     int64    `json:"id"`
			Name   string   `json:"name"`
			Active bool     `json:"active"`
			Scopes []string `json:"scopes"`
			Level  int      `json:"access_level"`
		}
		if rep.Accounts == setup.AccountsGroup {
			root.get(t, fmt.Sprintf("/groups/%d/access_tokens?state=active", fx.nsID), &list)
		} else {
			for _, u := range []apiUser{reader, writer} {
				var l []struct {
					ID     int64    `json:"id"`
					Name   string   `json:"name"`
					Active bool     `json:"active"`
					Scopes []string `json:"scopes"`
					Level  int      `json:"access_level"`
				}
				root.get(t, fmt.Sprintf("/personal_access_tokens?user_id=%d&state=active", u.ID), &l)
				list = append(list, l...)
			}
		}
		var out []string
		for _, tok := range list {
			out = append(out, fmt.Sprintf("%d %s %s %d", tok.ID, tok.Name, strings.Join(tok.Scopes, ","), tok.Level))
		}
		slices.Sort(out)
		return strings.Join(out, "; ")
	}
	before := tokens()
	finding(t, "setup-tokens", "%s", before)

	// A second run and a dry run change nothing.
	for _, extra := range [][]string{nil, {"--dry-run"}} {
		rep := setupRun(extra...)
		for _, s := range rep.Steps {
			if s.Status != setup.StatusOK {
				t.Errorf("run %v: %s %s: %s", extra, s.Status, s.Name, s.Detail)
			}
		}
	}
	if after := tokens(); after != before {
		t.Errorf("the tokens changed: %s, then %s", before, after)
	}

	// doctor --hub-token reads the hub as setup left it.
	out := run(0, "doctor", "--hub-token", "--hub-fp", fmt.Sprintf("%s/%d", e.Host, hub.ID), "--format", "json")
	var doc report.Doctor
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.HubChecks {
		if c.Status == report.StatusFail || c.Status == report.StatusWarn {
			t.Errorf("doctor --hub-token: %s %s: %s", c.Status, c.Name, c.Detail)
		}
	}

	if e.RunnerBin != "" {
		setupPipeline(t, fx, hub, rep, hubFiles)
	}
	for i, o := range outputs {
		for _, a := range []account{e.Root, e.Reader, e.Writer, e.Person} {
			if strings.Contains(o, a.Token) {
				t.Errorf("output %d holds the token of %s", i, a.Login)
			}
		}
		if strings.Contains(o, "glpat-") || strings.Contains(o, stale) {
			t.Errorf("output %d holds a token:\n%s", i, e.Redact.Replace(o))
		}
	}
}

// setupPipeline merges a merge request that names the writer in hub.yml
// and adds the hub's pipeline, and checks the jobs of the default branch:
// without an environment, probe passes (the write key is not there) and
// plan reads the targets with the reader's token; in the environment
// touchmark-distribute, doctor runs with the writer's token.
func setupPipeline(t *testing.T, fx *fixture, hub apiProject, rep setup.Report, files func(writer, ci string) []conformance.File) {
	e := fx.env
	bin := e.RunnerBin
	ci := "variables:\n  GIT_DEPTH: \"0\"\nworkflow:\n  rules:\n    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH\n" +
		fmt.Sprintf("reader:\n  script:\n    - '%s probe'\n    - '%s plan --hub .'\n", bin, bin) +
		fmt.Sprintf("writer:\n  environment:\n    name: %s\n    action: prepare\n  script:\n    - '%s doctor --hub .'\n", setup.Environment, bin)
	e.commitFiles(t, e.Person, hub.ID, "configure", "main", files(rep.Writer, ci), "name the writer, add the pipeline")
	repo := fx.repo(t, hub.ID)
	var mr apiMR
	e.api(e.Person).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", hub.ID), map[string]any{
		"source_branch": "configure", "target_branch": "main", "title": "configure the hub", "target_project_id": hub.ID,
	}, &mr)
	e.merge(t, e.Person, hub.ID, mr.IID)
	head, ok := e.branchHead(t, hub.ID, repo.DefaultBranch)
	if !ok {
		t.Fatal("the hub has no main")
	}
	jobs := e.pipelineJobs(t, hub.ID, func() (int64, bool) { return e.pipelineOf(t, hub.ID, "main", head) })
	var lines []string
	for _, name := range []string{"reader", "writer"} {
		j, ok := jobs[name]
		lines = append(lines, fmt.Sprintf("%s %s", name, j.Status))
		if !ok || j.Status != "success" {
			t.Errorf("job %s: %s\n%s", name, j.Status, e.Redact.Replace(j.Trace))
		}
		if strings.Contains(j.Trace, "glpat-") {
			t.Errorf("the log of %s shows a token", name)
		}
	}
	finding(t, "setup-pipeline", "%s", strings.Join(lines, ", "))
}
