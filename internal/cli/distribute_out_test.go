package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/schemas"
)

// validateSchema fails the test unless data, a JSON document, passes the
// embedded schema called name ("report", "doctor").
func validateSchema(t *testing.T, name string, data []byte) {
	t.Helper()
	raw, ok := schemas.Get(name)
	if !ok {
		t.Fatalf("no schema %s", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	url := schemas.BaseURL + name + ".schema.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	if err := sch.Validate(v); err != nil {
		detail := err.Error()
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			if out, err := json.Marshal(ve.BasicOutput()); err == nil {
				detail = string(out)
			}
		}
		t.Errorf("the %s schema refuses the report: %s\n%s", name, detail, data)
	}
}

// exitCodeOf is the exit code a command's error stands for.
func exitCodeOf(err error) int {
	var ce *cliError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ce):
		return ce.code
	}
	return exitFailed
}

// outToken is a credential the run registered; a platform message quotes it.
const outToken = "out-write-token-0123456789"

// outReport is a distribute report with a failed and a blocked target, a
// target a public hub does not name, and a warning that quotes the token.
func outReport() *report.Delivery {
	rep := report.NewDelivery("distribute", "test")
	rep.Hub = report.DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/712345678", Commit: strings.Repeat("a", 40)}
	rep.Providers = []report.ProviderInfo{{ID: "gh", Type: "github", Host: "github.com", ResolveComplete: true}}
	rep.Targets = []report.DeliveryTarget{
		{Provider: "gh", Host: "github.com", RepoID: "1", Path: "acme/api", Outcome: report.OutcomeFailed, Reason: "auth",
			Warnings: []string{"create pull request: token " + outToken + " expired"}},
		{Provider: "gh", Host: "github.com", RepoID: "2", Path: "acme/web", Outcome: report.OutcomeBlocked, Reason: "edited"},
		{Provider: "gh", Host: "github.com", Outcome: report.OutcomeFailed, Reason: "transient"},
		{Provider: "gh", Host: "github.com", RepoID: "3", Path: "acme/docs", Outcome: report.OutcomeOpened},
	}
	rep.Summarize()
	return rep
}

// The outputs of a run follow the CI the hub context names: GitHub Actions
// gets the step summary, the annotations on stderr and the report files;
// GitLab CI the report files only; Gitea Actions the step summary and the
// files; Forgejo Actions the files (Forgejo shows no step summary); another
// CI the files; a local run none. Every output is masked, and none names
// the target the report does not name.
func TestPublishByCI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		vars     map[string]string
		ci       hubch.CI
		summary  bool
		annotate bool
		files    bool
	}{
		{name: "github", vars: map[string]string{"CI": "true", "GITHUB_ACTIONS": "true"}, ci: hubch.GitHubActions, summary: true, annotate: true, files: true},
		{name: "gitlab", vars: map[string]string{"CI": "true", "GITLAB_CI": "true"}, ci: hubch.GitLabCI, files: true},
		{name: "gitea", vars: map[string]string{"CI": "true", "GITHUB_ACTIONS": "true", "GITEA_ACTIONS": "true"}, ci: hubch.GiteaActions, summary: true, files: true},
		{name: "forgejo", vars: map[string]string{"CI": "true", "GITHUB_ACTIONS": "true", "FORGEJO_ACTIONS": "true"}, ci: hubch.ForgejoActions, files: true},
		{name: "other", vars: map[string]string{"CI": "woodpecker"}, ci: hubch.Local, files: true},
		{name: "local", vars: map[string]string{}, ci: hubch.Local},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			t.Chdir(work)
			summary := filepath.Join(t.TempDir(), "summary.md")
			// The runners of all three Actions set it; only some show it.
			if tc.vars["GITHUB_ACTIONS"] != "" {
				tc.vars["GITHUB_STEP_SUMMARY"] = summary
			}
			getenv := func(k string) string { return tc.vars[k] }
			hctx := hubch.Detect(getenv, os.ReadFile)
			if hctx.CI != tc.ci {
				t.Fatalf("detected %q, want %q", hctx.CI, tc.ci)
			}
			reg := redact.New()
			reg.Add(outToken)
			var stdout, stderr bytes.Buffer
			e := &env{stdout: &stdout, stderr: &stderr, getenv: getenv}
			reportFile := filepath.Join(t.TempDir(), "report.json")
			err := publishDelivery(e, hctx, reg, outReport(), formatText, reportFile)
			if code := exitCodeOf(err); code != exitFailed {
				t.Fatalf("exit %d (%v)", code, err)
			}
			outputs := map[string]string{"stdout": stdout.String(), "stderr": stderr.String()}
			read := func(name string) (string, bool) {
				data, err := os.ReadFile(name)
				if err != nil {
					return "", false
				}
				outputs[name] = string(data)
				return string(data), true
			}
			if data, ok := read(reportFile); !ok {
				t.Error("no --report file")
			} else {
				validateSchema(t, "report", []byte(data))
			}
			jsonFile, jsonOK := read(filepath.Join(work, "touchmark-report.json"))
			_, mdOK := read(filepath.Join(work, "touchmark-report.md"))
			if jsonOK != tc.files || mdOK != tc.files {
				t.Errorf("report files: json %v, md %v, want %v", jsonOK, mdOK, tc.files)
			}
			if jsonOK {
				validateSchema(t, "report", []byte(jsonFile))
			}
			got, summaryOK := read(summary)
			if summaryOK != tc.summary || (summaryOK && !strings.Contains(got, "### touchmark distribute")) {
				t.Errorf("step summary written %v, want %v:\n%s", summaryOK, tc.summary, got)
			}
			annotated := strings.Contains(stderr.String(), "::error title=touchmark::gh:acme/api failed:auth: create pull request: token *** expired\n")
			if annotated != tc.annotate || strings.Contains(stdout.String(), "::") {
				t.Errorf("annotations %v, want %v\nstdout:\n%s\nstderr:\n%s", annotated, tc.annotate, stdout.String(), stderr.String())
			}
			if tc.annotate && !strings.Contains(stderr.String(), "::warning title=touchmark::gh:acme/web blocked:edited\n") {
				t.Errorf("no warning for the blocked target:\n%s", stderr.String())
			}
			for name, out := range outputs {
				if strings.Contains(out, outToken) {
					t.Errorf("%s holds the token:\n%s", name, out)
				}
				if strings.Contains(out, "gh: failed") || strings.Contains(out, "gh: transient") {
					t.Errorf("%s names the hidden target:\n%s", name, out)
				}
			}
		})
	}
}

// At most ten failed and ten blocked targets are annotated; the targets a
// public hub does not name never are.
func TestAnnotationsBounded(t *testing.T) {
	rep := report.NewDelivery("distribute", "test")
	for i := range 15 {
		for _, o := range []report.Outcome{report.OutcomeFailed, report.OutcomeBlocked} {
			rep.Targets = append(rep.Targets, report.DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: fmt.Sprint(i),
				Path: fmt.Sprintf("acme/r%02d-%s", i, o), Outcome: o, Reason: "edited"})
		}
		rep.Targets = append(rep.Targets, report.DeliveryTarget{Provider: "gh", Host: "github.com", Outcome: report.OutcomeFailed, Reason: "git"})
	}
	got := annotations(rep)
	if n := strings.Count(got, "::error "); n != maxAnnotations {
		t.Errorf("%d errors", n)
	}
	if n := strings.Count(got, "::warning "); n != maxAnnotations {
		t.Errorf("%d warnings", n)
	}
	if strings.Contains(got, "gh: ") || strings.Contains(got, "::gh:failed") {
		t.Errorf("a hidden target is annotated:\n%s", got)
	}
	// A message cannot end the command or start another one.
	if s := annotation("error", "a\nb\r::warning::c%"); s != "::error title=touchmark::a%0Ab%0D::warning::c%25\n" {
		t.Errorf("annotation %q", s)
	}
}
