package distribute

import (
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestRunOperations: the ways out of a pause: a recreate entry of
// operations.yml for the branch's head, the recreate control ticked in our
// pull request, a recreate entry that adopts a pull request of ours whose
// marker is gone, and adopt_unmarked for multi-gitter's merge requests on an
// alias.
func TestRunOperations(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	// An edited branch without our pull request, which operations.yml
	// rebuilds.
	byOp := w.optedIn("acme/by-op", nil)
	w.syncCommit(byOp, branch, "", baseFiles...)
	opHead := w.push(byOp, branch, w.person, "notes.md", "mine\n")
	// An edited branch whose pull request people ask to rebuild.
	ticked := w.optedIn("acme/ticked", nil)
	w.syncCommit(ticked, branch, "", baseFiles...)
	nTicked := w.openOwn(ticked)
	w.written(ticked, nTicked)
	w.push(ticked, branch, w.person, "notes.md", "mine\n")
	w.p.UpdatePR(ticked.ID, nTicked, func(pr *platform.PR) {
		pr.Body = "- [x] <!-- touchmark:" + prbody.ControlRecreate + " --> Rebuild this branch\n\n" + pr.Body
	})
	// Our pull request whose marker someone erased: operations.yml names
	// its branch's head.
	erased := w.optedIn("acme/erased", nil)
	erasedHead := w.syncCommit(erased, branch, "", baseFiles...)
	nErased := w.pr(erased, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: "the marker is gone"})
	// multi-gitter's merge request on the alias: no marker, by its bot,
	// holding a version the hub shipped.
	proto := w.optedIn("acme/migrated", nil)
	w.push(proto, alias, w.known, "AGENTS.md", agentsV1)
	nProto := w.pr(proto, platform.PR{Head: alias, Author: w.known, Title: "chore: sync engineering assets", Body: "Synced by multi-gitter."})

	w.ok()
	ops := &config.Operations{
		Version:       1,
		Recreate:      []config.RecreateOp{{Target: "acme/by-op", Head: opHead}, {Target: "gh:acme/erased", Head: erasedHead}},
		AdoptUnmarked: &config.UntilOp{Until: "2099-12-31"},
	}
	rep := w.both(nil)
	want(t, rep, "gh:acme/by-op", report.OutcomeBlocked, "edited", 0)
	want(t, rep, "gh:acme/ticked", report.OutcomeUpdated, "recreate", nTicked)
	want(t, rep, "gh:acme/erased", report.OutcomeBlocked, "marker-invalid", nErased)
	want(t, rep, "gh:acme/migrated", report.OutcomeBlocked, "marker-invalid", nProto)
	if n := targetOf(t, rep, "gh:acme/ticked").Writes; n != 3 {
		t.Errorf("ticked: %d writes, want 3 (untick, push, edit)", n)
	}

	rep = w.both(func(d *Deps) { d.Write.Operations = ops })
	type result struct {
		outcome report.Outcome
		reason  string
		pr      int64
		writes  int
	}
	for path, r := range map[string]result{
		"acme/by-op":    {report.OutcomeOpened, "", 0, 4},
		"acme/ticked":   {report.OutcomeUpdated, "recreate", nTicked, 3},
		"acme/erased":   {report.OutcomeUpdated, "recreate", nErased, 2},
		"acme/migrated": {report.OutcomeUpdated, "content", nProto, 2},
	} {
		tg := want(t, rep, "gh:"+path, r.outcome, r.reason, r.pr)
		if tg.Writes != r.writes {
			t.Errorf("%s: %d writes, want %d", path, tg.Writes, r.writes)
		}
	}
	// The same entries as local flags (LocalOps) act the same.
	local := w.both(func(d *Deps) { d.Write.LocalOps = ops })
	for i := range rep.Targets {
		a, b := rep.Targets[i], local.Targets[i]
		if a.Path != b.Path || a.Outcome != b.Outcome || a.Reason != b.Reason || a.Writes != b.Writes {
			t.Errorf("LocalOps: %s %s:%s %d, operations.yml: %s %s:%s %d", b.Path, b.Outcome, b.Reason, b.Writes, a.Path, a.Outcome, a.Reason, a.Writes)
		}
	}
}

// TestRunAliases: our pull request on an alias is maintained on the alias, a
// second one of ours on the sync branch makes it a duplicate, and a renamed
// default branch rebuilds the branch on it.
func TestRunAliases(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
	// Our pull request on the alias carries an old version.
	onAlias := w.optedIn("acme/on-alias", nil)
	w.syncCommit(onAlias, alias, "", old...)
	nAlias := w.ownOn(onAlias, alias, keyOf(old...), old...)
	// Ours on both: the sync branch's is kept, the alias's closed.
	both := w.optedIn("acme/both", nil)
	w.syncCommit(both, branch, "", baseFiles...)
	nKept := w.openOwn(both)
	w.syncCommit(both, alias, "", baseFiles...)
	w.ownOn(both, alias, keyOf(baseFiles...), baseFiles...)
	w.written(both, nKept)
	// The pull request still targets the old default branch.
	renamed := w.optedIn("acme/renamed", nil)
	w.syncCommit(renamed, branch, "", baseFiles...)
	nRenamed := w.pr(renamed, platform.PR{Head: branch, Base: "master", Author: w.writer, Title: "chore: sync engineering assets",
		Body: markerBody(t, keyOf(baseFiles...), hubFP, changesOf(baseFiles...), nil)})

	rep := w.both(nil)
	tg := want(t, rep, "gh:acme/on-alias", report.OutcomeUpdated, "content", nAlias)
	if tg.Writes != 2 {
		t.Errorf("on-alias: %d writes, want 2", tg.Writes)
	}
	wk, _ := w.inspectOne(w.deps(ModePlan), ModePlan, "acme/on-alias")
	if wk == nil || wk.Decision.Branch != alias || len(wk.Decision.Steps) == 0 || wk.Decision.Steps[0].Branch != alias {
		t.Errorf("on-alias: work %+v", wk)
	}
	tg = want(t, rep, "gh:acme/both", report.OutcomeUnchanged, "", nKept)
	if tg.Writes != 3 {
		t.Errorf("both: %d writes, want 3 (close, comment, the alias deleted)", tg.Writes)
	}
	tg = want(t, rep, "gh:acme/renamed", report.OutcomeUpdated, "base-renamed", nRenamed)
	if tg.Writes != 2 {
		t.Errorf("renamed: %d writes, want 2 (push, edit)", tg.Writes)
	}
}
