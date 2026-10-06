package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// A public hub names no non-public target in any output of distribute, in
// GitHub Actions on a schedule, where the hub's visibility comes through the
// hub channel: stdout in every format, stderr with its annotations, the step
// summary, the report files for the artifact, --report and the stream. The
// private targets: one opted in (skipped:private-in-public-hub), one that
// left targets.yml with an open pull request of touchmark's (the sweep closes
// it, unnamed), both with names the platform's errors would quote; a public
// target fails, so that the annotations and warnings have something to say.
func TestDistributePublicHubNamesNoPrivateTarget(t *testing.T) {
	needDistributeGit(t)
	work := t.TempDir()
	t.Chdir(work)
	h := distHub(t)
	h.write("targets.yml", distTargetsYML+"exclude:\n  - acme/secret-dropped\n")
	h.commit("exclude a target")
	w := newDistWorld(t)
	w.install()
	secret := w.repo("acme/secret-roadmap", planOptIn, "version: 1\n")
	dropped := w.repo("acme/secret-dropped", "README.md", "left the hub\n")
	for _, id := range []string{secret.ID, dropped.ID} {
		w.p.UpdateRepo(id, func(r *platform.Repo) { r.Visibility = "private" })
	}
	line, err := marker.Encode(marker.Marker{Key: "sha256:" + strings.Repeat("0", 64), Data: marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: distFP, Engine: "test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	w.p.AddPR(dropped.ID, platform.PR{Head: distBranch, Author: w.writer, Title: "chore: sync engineering assets", Body: "Synced.\n\n" + line})
	w.p.FailNext("CreatePR", &platform.Error{Op: "create pull request", Class: platform.ClassInvalid, Status: http.StatusUnprocessableEntity,
		Err: errors.New("validation failed")})
	if err := w.p.Err(); err != nil {
		t.Fatal(err)
	}
	// The hub is public: the channel says so on a schedule.
	api := httptest.NewServer(publicHub(h.git("rev-parse", "HEAD")))
	defer api.Close()
	vars := scheduleEnv(t, api.URL)
	summary := filepath.Join(t.TempDir(), "summary.md")
	vars["GITHUB_STEP_SUMMARY"] = summary
	reportFile, stream := filepath.Join(t.TempDir(), "report.json"), filepath.Join(t.TempDir(), "stream.jsonl")
	var outputs []string
	for i, format := range []string{"text", "json", "markdown"} {
		code := exitFailed
		if i > 0 {
			code = exitOK // the fault was used up
		}
		res := runDistributeCmd(t, h, vars, code, "--format", format, "--report", reportFile, "--stream", stream)
		outputs = append(outputs, res.stdout, res.stderr)
		for _, name := range []string{summary, reportFile, stream, filepath.Join(work, "touchmark-report.json"), filepath.Join(work, "touchmark-report.md")} {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			outputs = append(outputs, name+":\n"+string(data))
		}
	}
	masks := regexp.MustCompile(`(?m)^::add-mask::.*$`)
	for _, out := range outputs {
		if strings.Contains(masks.ReplaceAllString(out, ""), "secret-") {
			t.Errorf("an output names a private target:\n%s", out)
		}
	}
	// The reports pass the schema, with the targets they do not name.
	for _, name := range []string{reportFile, filepath.Join(work, "touchmark-report.json")} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		validateSchema(t, "report", data)
	}
	if pr := w.p.PR(dropped.ID, 1); pr.State != platform.Closed {
		t.Errorf("the dropped private target's pull request is %s", pr.State)
	}
	if !strings.Contains(outputs[0], "private in public hub") {
		t.Errorf("the text report does not count the private targets:\n%s", outputs[0])
	}
}

// publicHub is the API of a public hub for its channel: the tip of master,
// the repository, the environment touchmark-distribute.
func publicHub(tip string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if serveEnvironment(rw, r, hubEnvironment, hubEnvironmentPolicies) {
			return
		}
		switch r.URL.Path {
		case "/repos/acme/engineering-assets/git/ref/heads/master":
			fmt.Fprintf(rw, `{"ref": "refs/heads/master", "object": {"sha": %q, "type": "commit"}}`, tip)
		case "/repos/acme/engineering-assets":
			fmt.Fprint(rw, `{"full_name": "acme/engineering-assets", "private": false, "visibility": "public"}`)
		default:
			http.NotFound(rw, r)
		}
	})
}
