package cli

import (
	"strings"
	"testing"
)

// check reads the GitHub Actions workflows of the hub, from HEAD unless
// --worktree: a write key a workflow hands out that the probe does not
// test fails it with exit 2.
func TestCheckProbe(t *testing.T) {
	t.Parallel()
	const wf = ".github/workflows/distribute.yml"
	const probe = `on: {schedule: [{cron: "17 * * * *"}]}
jobs:
  probe:
    runs-on: ubuntu-latest
    steps:
      - id: p
        run: echo "v=$V" >> "$GITHUB_OUTPUT"
        env: {V: "${{ secrets.OTHER != '' }}"}
  distribute:
    needs: probe
    environment: touchmark-distribute
    runs-on: ubuntu-latest
    env:
      TOUCHMARK_KEY_EXPOSED: "${{ needs.probe.outputs.exposed }}"
      TOUCHMARK_WRITE_TOKEN: "${{ secrets.WRITER_TOKEN }}"
    steps: [{run: touchmark distribute}]
`
	h := baseHub(t)
	h.write(wf, probe)
	h.write(".github/workflows/notes.txt", "secrets.WRITER_TOKEN is not a workflow\n")
	h.commit("workflows")
	s := newScenario(t, "", h, nil)
	want := wf + ": TOUCHMARK_WRITE_TOKEN: secrets.WRITER_TOKEN carries a write key, and no probe tests it"
	if res := s.run(exitUsage, "check"); !strings.Contains(res.stdout, want) {
		t.Errorf("check: stdout %q, want %q", res.stdout, want)
	}
	h.put(wf, strings.Replace(probe, "secrets.OTHER != ''", "secrets.WRITER_TOKEN != ''", 1))
	if res := s.run(0, "check", "--worktree"); strings.Contains(res.stdout, "no probe tests it") {
		t.Errorf("check --worktree: stdout %q", res.stdout)
	}
	s.run(exitUsage, "check")
}
