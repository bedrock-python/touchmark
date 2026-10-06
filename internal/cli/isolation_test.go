package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A hub that accepted the risk (security.write_isolation: none, with its
// reason) runs distribute without the probe's answer and
// without the environment, and every report says so with the reason: the
// JSON, the text and the step summary.
func TestDistributeIsolationNone(t *testing.T) {
	needDistributeGit(t)
	t.Chdir(t.TempDir())
	const reason = "the hub's CI runs on Gitea Actions, which cannot keep a secret from other branches"
	h := distHub(t)
	h.write("hub.yml", distHubYML+"security:\n  write_isolation: none\n  reason: "+reason+"\n")
	h.commit("accept the risk")
	w := newDistWorld(t)
	w.install()
	api := newHubAPI(t, h.git("rev-parse", "HEAD"))
	api.env.Store("404") // no environment: the check does not apply
	vars := scheduleEnv(t, api.URL)
	delete(vars, "TOUCHMARK_KEY_EXPOSED") // no probe job: none needs none
	summary := filepath.Join(t.TempDir(), "summary.md")
	vars["GITHUB_STEP_SUMMARY"] = summary
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	want := "write isolation is off (security.write_isolation: none): jobs of any branch of the hub may see the write key; reason: " + reason

	rep := decodeDelivery(t, runDistributeCmd(t, h, vars, exitOK, "--dry-run", "--format", "json", "--stream", stream).stdout)
	if !slices.Contains(rep.Warnings, want) {
		t.Errorf("warnings %q lack %q", rep.Warnings, want)
	}
	if rep.Summary["opened"] != 2 {
		t.Errorf("summary %v", rep.Summary)
	}
	res := runDistributeCmd(t, h, vars, exitOK, "--dry-run", "--stream", stream)
	if !strings.Contains(res.stdout, "Warnings\n") || !strings.Contains(res.stdout, want) {
		t.Errorf("the text report lacks the warning:\n%s", res.stdout)
	}
	md, err := os.ReadFile(summary)
	if err != nil || !strings.Contains(string(md), "write isolation is off") || !strings.Contains(string(md), "Gitea Actions") {
		t.Errorf("the step summary lacks the warning (%v):\n%s", err, md)
	}

	// Without the reason the hub fails check before anything runs.
	h.write("hub.yml", distHubYML+"security:\n  write_isolation: none\n")
	h.commit("drop the reason")
	api.tip.Store(h.git("rev-parse", "HEAD"))
	w.p.ResetCalls()
	res = runDistributeCmd(t, h, vars, exitUsage, "--dry-run", "--stream", stream)
	if !strings.Contains(res.stderr, "security.reason") || !strings.Contains(res.stderr, "required with write_isolation: none") {
		t.Errorf("stderr: %s", res.stderr)
	}
	if calls := w.p.Calls(); len(calls) > 0 {
		t.Errorf("a refused run called the platform: %q", calls)
	}
}
