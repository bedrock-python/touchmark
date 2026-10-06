package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// planPythonV2 is the python pack's file as a hub pull request changes it.
var planPythonV2 = text("python guidelines v2")

// planPR turns the plan hub into a hub pull request: the default branch
// master (and origin/master, as a CI clone has it) stays at the hub's tip,
// and edit's changes are committed on the branch feature.
func planPR(t *testing.T, h *repo, edit func(h *repo)) {
	t.Helper()
	h.git("update-ref", "refs/remotes/origin/master", h.git("rev-parse", "HEAD"))
	h.git("checkout", "-q", "-b", "feature")
	edit(h)
	h.commit("the pull request")
}

// outcomes maps "<provider>:<path>" to "<outcome>:<reason>".
func outcomes(rep report.Delivery) map[string]string {
	got := map[string]string{}
	for _, tg := range rep.Targets {
		got[tg.Provider+":"+tg.Path] = string(tg.Outcome) + ":" + tg.Reason
	}
	return got
}

// snapshotted lists the repositories the platforms took snapshots of.
func (w *planWorld) snapshotted() []string {
	var out []string
	for _, c := range slices.Concat(w.gh.Calls(), w.corp.Calls()) {
		if repo, ok := strings.CutPrefix(c, "Snapshot "); ok {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

// A hub pull request that changes one pack plans the targets of that pack
// only: the others are neither snapshotted nor listed, so
// no untouched target shows up as closed, dropped or changed; the opt-in
// files of every target are read all the same.
func TestPlanScopeOnePack(t *testing.T) {
	h := planHub(t)
	planPR(t, h, func(h *repo) { h.write("packs/python/docs/python.md", planPythonV2) })
	w := newPlanWorld(t)
	w.install()
	vars := actionsEnv(t)
	s := newScenario(t, "plan-scope", h, nil)

	res := planRun(t, h, vars, exitOK)
	s.golden("plan.txt", s.normalize(res.stdout))
	res = planRun(t, h, vars, exitOK, "--format", "markdown")
	s.golden("plan.md", s.normalize(res.stdout))
	w.gh.ResetCalls()
	w.corp.ResetCalls()
	res = planRun(t, h, vars, exitOK, "--format", "json", "--strict")
	s.golden("plan.json", s.normalize(res.stdout))
	rep := decodeDelivery(t, res.stdout)

	want := map[string]string{"gh:acme/sdk": "updated:content", "corp:platform/api": "opened:"}
	if got := outcomes(rep); !mapsEqual(got, want) {
		t.Errorf("targets %v, want %v", got, want)
	}
	sc := rep.Scope
	if sc == nil || sc.Mode != report.ScopePacks || !slices.Equal(sc.Packs, []string{"python"}) || sc.Processed != 2 || sc.Total != 11 {
		t.Fatalf("scope %+v", sc)
	}
	if base := h.git("rev-parse", "master"); sc.Base != base {
		t.Errorf("scope base %s, want master %s", sc.Base, base)
	}
	if got := w.snapshotted(); !slices.Equal(got, []string{"acme/sdk", "platform/api"}) {
		t.Errorf("snapshots of %q, want only the python targets", got)
	}
	// Every opted-in target's opt-in file is read: the packs need it.
	for _, repo := range []string{"acme/api", "acme/old", "acme/tools", "acme/billing"} {
		if !slices.ContainsFunc(w.gh.Calls(), func(c string) bool { return strings.HasPrefix(c, "ReadFile "+repo+" ") }) {
			t.Errorf("the opt-in file of %s was not read", repo)
		}
	}
	if rep.Summary[report.OutcomeClosed] != 0 || rep.Summary[report.OutcomeBlocked] != 0 || rep.Summary[report.OutcomeSkipped] != 0 {
		t.Errorf("summary %v: an untouched target is reported", rep.Summary)
	}
	if e := rep.Estimate; e == nil || e.NewPRs != 1 || e.Runs != 1 {
		t.Errorf("estimate %+v", e)
	}
}

// mapsEqual compares two string maps.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// The scope's other modes: a pull request that changes only hub files no
// target receives plans none (and passes --strict); one that changes the
// configuration plans every target; --all plans every target of any pull
// request; a run that builds no pull request has no scope; and a scope that
// cannot be computed plans every target, with a warning. A pack file that a
// commit of the pull request adds and a later one takes back (on the branch,
// or on a side branch merged into it) puts its pack in the scope: the hub's
// history decides what targets get (a target's file that matches a version
// a pack once had is the pack's), so the net diff alone would hide what the
// merge does.
func TestPlanScopeModes(t *testing.T) {
	pack := func(h *repo) { h.write("packs/python/docs/python.md", planPythonV2) }
	const extra = "packs/python/.github/workflows/release.yml"
	cases := []struct {
		name string
		edit func(h *repo)
		args []string
		vars func(map[string]string)
		mode string // "" for no scope
		why  string
		// unknown is set when the default branch is not in the clone: the
		// providers are then read without credentials too, so the targets
		// are not compared.
		unknown bool
		// targets is how many targets the report holds when not every one
		// (11) or none (ScopeHub).
		targets int
	}{
		{name: "hub files only", edit: func(h *repo) { h.write("README.md", "# the hub\n") }, mode: report.ScopeHub},
		{name: "CI files", edit: func(h *repo) { h.write(".github/workflows/engineering-assets.yml", "on: push\n") }, mode: report.ScopeHub},
		{name: "targets.yml", edit: func(h *repo) { h.write("targets.yml", planTargets+"# reviewed\n") },
			mode: report.ScopeAll, why: "targets.yml changed"},
		{name: "operations.yml", edit: func(h *repo) { h.write(".touchmark/operations.yml", "version: 1\n") },
			mode: report.ScopeAll, why: ".touchmark/operations.yml changed"},
		{name: "--all", edit: pack, args: []string{"--all"}, mode: report.ScopeAll, why: "--all"},
		{name: "pack file added and taken back", edit: func(h *repo) {
			h.write(extra, text("release workflow"))
			h.commit("add a workflow")
			h.rm(extra)
		}, mode: report.ScopePacks, targets: 2},
		{name: "merged side branch", edit: func(h *repo) {
			h.git("checkout", "-q", "-b", "side")
			h.write(extra, text("release workflow"))
			h.commit("add a workflow")
			h.rm(extra)
			h.commit("take it back")
			h.git("checkout", "-q", "feature")
			h.write("README.md", "# the hub\n")
			h.commit("readme")
			h.git("merge", "-q", "--no-ff", "-m", "merge side", "side")
		}, mode: report.ScopePacks, targets: 2},
		{name: "not a pull request", edit: pack, vars: func(v map[string]string) {
			v["GITHUB_EVENT_NAME"] = "workflow_dispatch"
			v["GITHUB_REF"], v["GITHUB_REF_NAME"] = "refs/heads/feature", "feature"
		}},
		{name: "default branch not in the clone", edit: pack, vars: func(v map[string]string) {
			if err := os.WriteFile(v["GITHUB_EVENT_PATH"], []byte(`{"repository": {"id": 712345678, "visibility": "public", "default_branch": "trunk"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, mode: report.ScopeAll, why: "the pull request's scope is unknown: the default branch trunk is not in this clone", unknown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := planHub(t)
			planPR(t, h, tc.edit)
			w := newPlanWorld(t)
			w.install()
			vars := actionsEnv(t)
			if tc.vars != nil {
				tc.vars(vars)
			}
			args := []string{"--format", "json"}
			if tc.mode == report.ScopeHub {
				// No target is planned: nothing blocks --strict.
				args = append(args, "--strict")
			}
			rep := decodeDelivery(t, planRun(t, h, vars, exitOK, append(args, tc.args...)...).stdout)
			sc := rep.Scope
			switch {
			case tc.mode == "" && sc != nil:
				t.Fatalf("scope %+v, want none", sc)
			case tc.mode == "":
			case sc == nil || sc.Mode != tc.mode || !strings.HasPrefix(sc.Reason, tc.why):
				t.Fatalf("scope %+v, want %s (%q)", sc, tc.mode, tc.why)
			case tc.mode == report.ScopePacks && !slices.Equal(sc.Packs, []string{"python"}):
				t.Fatalf("scope %+v, want the python pack", sc)
			case tc.unknown:
				if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.HasPrefix(w, tc.why) }) {
					t.Errorf("warnings %q lack %q", rep.Warnings, tc.why)
				}
				return
			case sc.Total != 11:
				t.Errorf("scope %+v: want 11 targets in all", sc)
			}
			want := 11
			switch {
			case tc.targets > 0:
				want = tc.targets
			case tc.mode == report.ScopeHub:
				want = 0
			}
			if len(rep.Targets) != want {
				t.Errorf("%d targets, want %d: %v", len(rep.Targets), want, outcomes(rep))
			}
			if tc.mode == report.ScopeHub && len(w.snapshotted()) > 0 {
				t.Errorf("a hub-only plan took snapshots of %q", w.snapshotted())
			}
		})
	}
}
