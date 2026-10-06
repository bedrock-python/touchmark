package config

import (
	"strings"
	"testing"
)

// The workflows of a hub on GitHub Actions, as the template lays them out
// (see docs/concepts/security.md): the probe job tests the write keys, the
// distribute job runs in the environment with them.
const (
	probeJob = `name: distribute
on: {schedule: [{cron: "17 * * * *"}], workflow_dispatch: {}}
jobs:
  probe:
    runs-on: ubuntu-latest
    outputs: {exposed: "${{ steps.p.outputs.v }}"}
    steps:
      - id: p
        run: echo "v=$V" >> "$GITHUB_OUTPUT"
        env: {V: "${{ secrets.WRITER_APP_KEY != '' || secrets.CORP_WRITE_TOKEN != '' }}"}
`
	distributeJob = `  distribute:
    needs: probe
    environment: touchmark-distribute
    runs-on: ubuntu-latest
    env:
      TOUCHMARK_KEY_EXPOSED: "${{ needs.probe.outputs.exposed }}"
      TOUCHMARK_GH_WRITE_APP_ID: "${{ vars.WRITER_APP_ID }}"
      TOUCHMARK_GH_WRITE_APP_KEY: "${{ secrets.WRITER_APP_KEY }}"
    steps:
      - run: touchmark distribute
        env:
          TOUCHMARK_CORP_WRITE_TOKEN: ${{ secrets.CORP_WRITE_TOKEN }}
`
	// planJob is the template's plan job: its first step fails when a write
	// key is visible to the pull request.
	planJob = `  plan:
    runs-on: ubuntu-latest
    steps:
      - run: test "$EXPOSED" = false
        env: {EXPOSED: "${{ secrets.WRITER_APP_KEY != '' || secrets.CORP_WRITE_TOKEN != '' }}"}
      - run: touchmark plan
`
)

func TestCheckProbe(t *testing.T) {
	t.Parallel()
	hub := func(isolation string) *Hub {
		h := &Hub{}
		h.Security.WriteIsolation = isolation
		return h
	}
	for _, tc := range []struct {
		name      string
		isolation string
		files     map[string]string
		want      []string // parts of the errors, one entry per error
	}{
		{name: "every key probed", files: map[string]string{"distribute.yml": probeJob + distributeJob}},
		{name: "no workflows"},
		{name: "a key the probe misses", files: map[string]string{
			"distribute.yml": strings.Replace(probeJob, " || secrets.CORP_WRITE_TOKEN != ''", "", 1) + distributeJob},
			want: []string{"distribute.yml: TOUCHMARK_CORP_WRITE_TOKEN: secrets.CORP_WRITE_TOKEN carries a write key, and no probe tests it"}},
		{name: "a probe in another file", files: map[string]string{
			"probe.yml":      probeJob,
			"distribute.yml": "on: workflow_call\njobs:\n" + distributeJob}},
		{name: "a signing key", files: map[string]string{
			"distribute.yml": probeJob + distributeJob + "      - run: touchmark distribute\n        env: {TOUCHMARK_SIGNING_KEY: \"${{ secrets.SIGNING }}\"}\n"},
			want: []string{"secrets.SIGNING carries a write key"}},
		// Another job (the plan job's step) tests the key the probe job
		// misses: the probe that feeds TOUCHMARK_KEY_EXPOSED must test it.
		{name: "the probe job misses a key another job tests", files: map[string]string{
			"distribute.yml": strings.Replace(probeJob, " || secrets.CORP_WRITE_TOKEN != ''", "", 1) + distributeJob + planJob},
			want: []string{"distribute.yml: job probe: its probe expression does not test secrets.CORP_WRITE_TOKEN"}},
		{name: "a probe expression that misses a key", files: map[string]string{
			"distribute.yml": probeJob + distributeJob + strings.Replace(planJob, "secrets.CORP_WRITE_TOKEN", "secrets.TOUCHMARK_SIGNING_KEY", 1)},
			want: []string{"distribute.yml: job plan: its probe expression does not test secrets.CORP_WRITE_TOKEN"}},
		{name: "a probe job that tests nothing", files: map[string]string{
			"distribute.yml": strings.Replace(probeJob, "${{ secrets.WRITER_APP_KEY != '' || secrets.CORP_WRITE_TOKEN != '' }}", "${{ false }}", 1) + distributeJob + planJob},
			want: []string{"distribute.yml: job probe, whose output TOUCHMARK_KEY_EXPOSED reads, tests no write key"}},
		{name: "a constant TOUCHMARK_KEY_EXPOSED", files: map[string]string{
			"distribute.yml": probeJob + strings.Replace(distributeJob, `"${{ needs.probe.outputs.exposed }}"`, "false", 1)},
			want: []string{`distribute.yml: TOUCHMARK_KEY_EXPOSED is the constant "false"`}},
		{name: "a probe job that does not exist", files: map[string]string{
			"distribute.yml": probeJob + strings.Replace(distributeJob, "needs.probe.outputs", "needs.check.outputs", 1)},
			want: []string{"distribute.yml: TOUCHMARK_KEY_EXPOSED reads needs.check, and no workflow has a job check"}},
		{name: "external isolation", isolation: "external", files: map[string]string{"distribute.yml": distributeJob}},
		{name: "not YAML", files: map[string]string{"broken.yml": "jobs: [\n"}, want: []string{"broken.yml: yaml:"}},
	} {
		files := map[string][]byte{}
		for name, content := range tc.files {
			files[WorkflowsDir+"/"+name] = []byte(content)
		}
		errs := CheckProbe(hub(tc.isolation), files)
		if len(errs) != len(tc.want) {
			t.Errorf("%s: errors %v, want %d", tc.name, errs, len(tc.want))
			continue
		}
		for i, want := range tc.want {
			if !strings.Contains(errs[i].Error(), want) {
				t.Errorf("%s: error %q lacks %q", tc.name, errs[i], want)
			}
		}
	}
	big := map[string][]byte{WorkflowsDir + "/big.yml": make([]byte, MaxWorkflowSize+1)}
	if errs := CheckProbe(hub(""), big); len(errs) != 1 || !strings.Contains(errs[0].Error(), "larger than") {
		t.Errorf("a large file: %v", errs)
	}
}

// TestCheckProbeProviders: the probe must cover the write key of every
// provider of hub.yml. A key handed to an action's input
// is seen as one in an env mapping is; a provider whose key no workflow
// hands out where check can see it is an error, since the probe may miss
// it; short names count for a hub of one provider only.
func TestCheckProbeProviders(t *testing.T) {
	t.Parallel()
	hubOf := func(ids ...string) *Hub {
		h := &Hub{}
		for _, id := range ids {
			h.Providers = append(h.Providers, Provider{ID: id, Type: "github"})
		}
		return h
	}
	const probe = `jobs:
  probe:
    runs-on: ubuntu-latest
    steps:
      - id: p
        run: echo "v=$V" >> "$GITHUB_OUTPUT"
        env: {V: "${{ secrets.GH_KEY != '' }}"}
  distribute:
    needs: probe
    environment: touchmark-distribute
    runs-on: ubuntu-latest
    env:
      TOUCHMARK_KEY_EXPOSED: "${{ needs.probe.outputs.exposed }}"
      TOUCHMARK_GH_WRITE_APP_ID: "${{ vars.GH_APP_ID }}"
      TOUCHMARK_GH_WRITE_APP_KEY: "${{ secrets.GH_KEY }}"
    steps:
`
	const action = `      - uses: acme/touchmark-action@v1
        with:
          gl-write-token: ${{ secrets.GL_WRITE_TOKEN }}
          gl-signing-key: ${{ secrets.GL_SIGNING }}
`
	for _, tc := range []struct {
		name  string
		hub   *Hub
		files map[string]string
		want  []string
	}{
		{name: "every provider carried and probed", hub: hubOf("gh", "gl"), files: map[string]string{"d.yml": strings.Replace(probe,
			"secrets.GH_KEY != ''", "secrets.GH_KEY != '' || secrets.GL_WRITE_TOKEN != '' || secrets.GL_SIGNING != ''", 1) + action}},
		{name: "an action input the probe misses", hub: hubOf("gh", "gl"), files: map[string]string{"d.yml": probe + action},
			want: []string{"d.yml: with gl-write-token: secrets.GL_WRITE_TOKEN carries a write key, and no probe tests it",
				"d.yml: with gl-signing-key: secrets.GL_SIGNING carries a write key"}},
		{name: "a provider whose key is not seen", hub: hubOf("gh", "corp-eu"), files: map[string]string{"d.yml": probe + "      - run: touchmark distribute\n"},
			want: []string{"provider corp-eu: no workflow hands touchmark its write key", "TOUCHMARK_CORP_EU_WRITE_TOKEN", "corp-eu-write-token"}},
		{name: "short names with two providers", hub: hubOf("gh", "gl"), files: map[string]string{"d.yml": strings.Replace(probe,
			"TOUCHMARK_GH_WRITE_APP_KEY", "TOUCHMARK_WRITE_APP_KEY", 1)},
			want: []string{"provider gh: no workflow", "provider gl: no workflow"}},
		{name: "short names with one provider", hub: hubOf("gh"), files: map[string]string{"d.yml": strings.Replace(probe,
			"TOUCHMARK_GH_WRITE_APP_KEY", "TOUCHMARK_WRITE_APP_KEY", 1)}},
		{name: "a key from no secret", hub: hubOf("gh"), files: map[string]string{"d.yml": strings.Replace(probe,
			"${{ secrets.GH_KEY }}", "${{ steps.vault.outputs.key }}", 1)},
			want: []string{"provider gh: no workflow hands touchmark its write key from a secret"}},
		{name: "the implicit provider without a key", hub: hubOf(), files: map[string]string{"d.yml": "jobs:\n  d:\n    steps:\n      - run: touchmark distribute --format json\n"},
			want: []string{"no workflow hands touchmark a write key"}},
		{name: "no distribute workflow", hub: hubOf("gh", "gl"), files: map[string]string{"ci.yml": "jobs:\n  t:\n    steps:\n      - run: touchmark check\n        env: {DEPLOY_WRITE_TOKEN: \"${{ secrets.DEPLOY }}\"}\n"}},
	} {
		files := map[string][]byte{}
		for name, content := range tc.files {
			files[WorkflowsDir+"/"+name] = []byte(content)
		}
		errs := CheckProbe(tc.hub, files)
		var all []string
		for _, err := range errs {
			all = append(all, err.Error())
		}
		joined := strings.Join(all, "\n")
		if len(tc.want) == 0 && len(errs) > 0 {
			t.Errorf("%s: errors %q", tc.name, all)
		}
		for _, want := range tc.want {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: errors %q lack %q", tc.name, all, want)
			}
		}
	}
}
