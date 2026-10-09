package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A hostile environment around touchmark (a CI variable, a custom pipeline
// run with variables) that gives git a core.fsmonitor command through
// GIT_CONFIG_COUNT does not run it: neither check and status reading the
// hub, nor status and apply in a target. The control first shows that
// plain git in the same environment runs the command.
func TestHostileGitConfigEnv(t *testing.T) {
	h := baseHub(t)
	tg := newRepo(t, false)
	tg.write(optInFile, optInBase)
	tg.commit("opt in")
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hostile := map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "core.fsmonitor",
		"GIT_CONFIG_VALUE_0": "echo ran >>'" + filepath.ToSlash(marker) + "'",
	}

	control := exec.Command("git", "-C", tg.dir, "status", "--porcelain")
	control.Env = os.Environ()
	for k, v := range hostile {
		control.Env = append(control.Env, k+"="+v)
	}
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("plain git did not run the hostile core.fsmonitor (%v): the test proves nothing", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	for k, v := range hostile {
		t.Setenv(k, v)
	}
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	s.run(exitOK, "check")
	s.report("status")
	s.report("apply")
	if got := changes(s.report("status")); len(got) != 0 {
		t.Errorf("status after apply plans %q", got)
	}
	s.wantContent("AGENTS.md", text("base AGENTS.md v1"))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("touchmark's git ran the core.fsmonitor command of the inherited GIT_CONFIG_COUNT")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
