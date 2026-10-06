package distribute

import (
	"fmt"
	"slices"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// pyFiles is what the base and python packs bring to a target that holds
// none of their files.
var pyFiles = append(slices.Clone(baseFiles), "docs/python.md", pythonV1)

// pythonScope is the scope of a hub pull request that changes the python
// pack.
func pythonScope() *Scope { return &Scope{Mode: report.ScopePacks, Packs: []string{"python"}} }

// settled adds an opted-in target (with topics) whose default branch took
// files after touchmark's pull request, still open, proposed them: a plan
// that processes it closes the pull request (no-diff).
func (g *gitWorld) settled(path string, topics []string, files ...string) (platform.Repo, int64) {
	g.t.Helper()
	r := g.optedIn(path, func(r *platform.Repo) { r.Topics = topics })
	g.syncCommit(r, branch, "", files...)
	n := g.ownOn(r, branch, keyOf(files...), files...)
	g.push(r, "main", g.person, files...)
	return r, n
}

// A plan of a hub pull request that changes one pack does not report the
// targets it leaves out as closed, dropped or otherwise changed. The sweep
// keeps the closes of the targets it processes only: a base target whose
// pull request a full plan would close (no-diff), one that opted out and a
// repository dropped from targets.yml all stay untouched, while the python
// targets are planned as in a full plan. distribute --dry-run ignores the
// scope.
func TestPlanScopeUntouched(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.optedIn("acme/py", topics("python"))
	_, nDone := w.settled("acme/py-done", []string{"python"}, pyFiles...)
	pyGone := w.repo("acme/py-gone", topics("python"), "README.md", "the opt-in file is gone\n")
	nPyGone := w.openOwn(pyGone)
	_, nWeb := w.settled("acme/web", nil, baseFiles...)
	gone := w.repo("acme/gone", nil, "README.md", "the opt-in file is gone\n")
	nGone := w.openOwn(gone)
	outside := w.repo("other/outside", nil, "README.md", "x\n")
	nOutside := w.openOwn(outside)

	// The full plan closes every one of them.
	full := w.both(nil)
	want(t, full, "gh:acme/py", report.OutcomeOpened, "", 0)
	want(t, full, "gh:acme/py-done", report.OutcomeClosed, "no-diff", nDone)
	want(t, full, "gh:acme/py-gone", report.OutcomeClosed, "opted-out", nPyGone)
	want(t, full, "gh:acme/web", report.OutcomeClosed, "no-diff", nWeb)
	want(t, full, "gh:acme/gone", report.OutcomeClosed, "opted-out", nGone)
	want(t, full, "gh:other/outside", report.OutcomeClosed, "target-dropped", nOutside)

	d := w.deps(ModePlan)
	d.Scope = pythonScope()
	rep := w.run(d, ModePlan)
	want(t, rep, "gh:acme/py", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/py-done", report.OutcomeClosed, "no-diff", nDone)
	want(t, rep, "gh:acme/py-gone", report.OutcomeClosed, "opted-out", nPyGone)
	for _, tg := range rep.Targets {
		switch tg.Path {
		case "acme/web", "acme/gone", "other/outside", "":
			t.Errorf("a target outside the scope is reported: %+v", tg)
		}
	}
	if len(rep.Targets) != 3 || rep.Summary[report.OutcomeClosed] != 2 {
		t.Errorf("%d targets, summary %v", len(rep.Targets), rep.Summary)
	}
	if rep.Sweep != (report.SweepInfo{Ran: true, Complete: true}) {
		t.Errorf("sweep %+v", rep.Sweep)
	}
	if s := rep.Scope; s == nil || s.Processed != 3 || s.Total != 5 {
		t.Errorf("scope %+v", s)
	}
	for _, id := range []string{"acme/web", "acme/gone"} {
		for _, c := range w.p.Calls() {
			if c == "Snapshot "+id || c == "Fetch "+id || c == "PRs "+id {
				t.Errorf("a target outside the scope was inspected: %s", c)
			}
		}
	}
	if full.Cost["gh"] <= rep.Cost["gh"] {
		t.Errorf("cost: full %d, scoped %d", full.Cost["gh"], rep.Cost["gh"])
	}

	// distribute --dry-run plans every target whatever the scope.
	dd := w.deps(ModeDryRun)
	dd.Scope = pythonScope()
	dry := w.run(dd, ModeDryRun)
	same(t, full, dry)
	if dry.Scope != nil {
		t.Errorf("a dry run reports a scope: %+v", dry.Scope)
	}
}

// The mass-close guard of a plan limited to one pack weighs its closes
// against every open pull request of touchmark, the ones in targets it
// leaves out too (its sweep lists them): six python closes of fourteen
// open pull requests pass max_close_fraction 0.5, as they do in a full
// plan, where the six alone would not; and with a lower fraction both
// plans block them.
func TestPlanScopeMassClose(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML = defaultHubYML + "limits:\n  max_close_fraction: 0.5\n"
	for i := range 6 {
		w.settled(fmt.Sprintf("acme/py%d", i), []string{"python"}, pyFiles...)
	}
	for i := range 8 {
		r := w.optedIn(fmt.Sprintf("acme/base%d", i), nil)
		w.syncCommit(r, branch, "", baseFiles...)
		w.written(r, w.openOwn(r))
	}
	check := func(name string, rep *report.Delivery, closed, blocked, targets int) {
		t.Helper()
		if rep.Summary[report.OutcomeClosed] != closed || rep.Summary[report.OutcomeBlocked] != blocked || len(rep.Targets) != targets {
			t.Errorf("%s: %d targets, summary %v; want %d closed, %d blocked", name, len(rep.Targets), rep.Summary, closed, blocked)
		}
	}
	check("full", w.both(nil), 6, 0, 14)
	d := w.deps(ModePlan)
	d.Scope = pythonScope()
	rep := w.run(d, ModePlan)
	check("scoped", rep, 6, 0, 6)
	if hasWarning(rep.Warnings, "blocked:mass-close") {
		t.Errorf("warnings %q", rep.Warnings)
	}

	w.hubYML = defaultHubYML + "limits:\n  max_close_fraction: 0.2\n"
	check("full, fraction 0.2", w.both(nil), 0, 6, 14)
	d = w.deps(ModePlan)
	d.Scope = pythonScope()
	rep = w.run(d, ModePlan)
	check("scoped, fraction 0.2", rep, 0, 6, 6)
	if !hasWarning(rep.Warnings, "blocked:mass-close: this run would close 6 of touchmark's 14 open pull requests, more than 5") {
		t.Errorf("warnings %q", rep.Warnings)
	}

	// Without the sweep's listing (it is off: the listings are incomplete),
	// the scoped plan counts only its own six open pull requests: the guard
	// is not judged, and a warning says so, instead of a false
	// blocked:mass-close (exit 1).
	w.hubYML = defaultHubYML + "limits:\n  max_close_fraction: 0.5\n"
	w.p.SetIncompleteListings(true)
	d = w.deps(ModePlan)
	d.Scope = pythonScope()
	rep = w.run(d, ModePlan)
	check("scoped, no sweep", rep, 6, 0, 6)
	if rep.Sweep.Ran || !hasWarning(rep.Warnings, "the mass-close guard is not judged in this plan: it would close 6 of the 6 open pull requests") {
		t.Errorf("sweep %+v, warnings %q", rep.Sweep, rep.Warnings)
	}
}

// plan --assume-opt-in plans a repository without an opt-in file as opted
// in: its pull request is not swept as opted-out.
func TestPlanAssumeOptInSweep(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	gone := w.repo("acme/gone", nil, "README.md", "the opt-in file is gone\n")
	w.syncCommit(gone, branch, "", baseFiles...)
	n := w.openOwn(gone)
	want(t, w.both(nil), "gh:acme/gone", report.OutcomeClosed, "opted-out", n)
	d := w.deps(ModePlan)
	d.AssumeOptIn = true
	rep := w.run(d, ModePlan)
	tg := want(t, rep, "gh:acme/gone", report.OutcomeUpdated, "body", n)
	if !tg.Assumed || !rep.Assumed {
		t.Errorf("assumed: target %v, report %v", tg.Assumed, rep.Assumed)
	}
}

// The estimate gives both numbers where a signature may be needed: with
// rules the reader cannot see, every push may have to go through the API
// commit (three writes); with sign: always the plan counts the API path, and
// the git path is the writes with a signing key.
func TestPlanEstimateSigned(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, fake.WithPreflight(), fake.WithAPICommits())
	a := w.optedIn("acme/a", nil)
	b := w.optedIn("acme/b", nil)
	w.optedIn("acme/c", nil)

	rep := w.run(w.deps(ModePlan), ModePlan)
	gh := rep.Estimate.Providers["gh"]
	// Three new pull requests: push, pull request, labels and the label.
	if gh.Writes != 12 || gh.APIWrites != 0 || rep.Cost["gh"] != 12 {
		t.Errorf("rules known: %+v, cost %d", gh, rep.Cost["gh"])
	}
	w.p.HideRules(a.ID, true)
	w.p.HideRules(b.ID, true)
	rep = w.run(w.deps(ModePlan), ModePlan)
	gh = rep.Estimate.Providers["gh"]
	if gh.Writes != 12 || gh.APIWrites != 16 || rep.Cost["gh"] != 12 {
		t.Errorf("rules hidden: %+v, cost %d", gh, rep.Cost["gh"])
	}
	w.hubYML = defaultHubYML + "    sign: always\n"
	rep = w.run(w.deps(ModePlan), ModePlan)
	gh = rep.Estimate.Providers["gh"]
	if gh.Writes != 12 || gh.APIWrites != 18 || rep.Cost["gh"] != 18 {
		t.Errorf("sign always: %+v, cost %d", gh, rep.Cost["gh"])
	}
}

// A plan lists what its pushes change by path across the targets: sensitive
// paths first (hub.yml's sensitive_paths), then by action and path; a target
// that pushes nothing adds nothing.
func TestPlanPaths(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML = defaultHubYML + "sensitive_paths: [docs/guide.md]\n"
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", topics("python"))
	w.optedIn("acme/c", nil, "AGENTS.md", agentsV1)
	w.optedIn("acme/done", nil, baseFiles...)
	rep := w.both(nil)
	want := []report.PathChange{
		{Action: "add", Path: "docs/guide.md", Sensitive: true, Targets: 3},
		{Action: "update", Path: "AGENTS.md", Targets: 1},
		{Action: "add", Path: "AGENTS.md", Targets: 2},
		{Action: "add", Path: "docs/python.md", Targets: 1},
	}
	if !slices.Equal(rep.Paths, want) {
		t.Errorf("paths %+v, want %+v", rep.Paths, want)
	}
}
