package distribute

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestPlanOperations: a plan and a dry run say what each one-off operation
// does (the Operations section): every entry of operations.yml in file
// order, then the local flags, each with its effect and why. A plan and a
// dry run of the same state agree.
func TestPlanOperations(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	// An edited branch without a pull request, which an entry rebuilds.
	byOp := w.optedIn("acme/by-op", nil)
	w.syncCommit(byOp, branch, "", baseFiles...)
	opHead := w.push(byOp, branch, w.person, "notes.md", "mine\n")
	// An edited branch whose entry names the head before the person's push.
	moved := w.optedIn("acme/moved", nil)
	staleHead := w.syncCommit(moved, branch, "", baseFiles...)
	w.push(moved, branch, w.person, "notes.md", "mine\n")
	// Our pull request whose marker someone erased: an entry names its head.
	erased := w.optedIn("acme/erased", nil)
	erasedHead := w.syncCommit(erased, branch, "", baseFiles...)
	nErased := w.pr(erased, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: "the marker is gone"})
	// multi-gitter's merge request on the alias, without a marker.
	proto := w.optedIn("acme/migrated", nil)
	w.push(proto, alias, w.known, "AGENTS.md", agentsV1)
	nProto := w.pr(proto, platform.PR{Head: alias, Author: w.known, Title: "chore: sync engineering assets", Body: "Synced by multi-gitter."})
	// A decline an entry forgets, and an entry for a pull request that is
	// none of ours.
	forget := w.optedIn("acme/forget", nil)
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	hash := optInHash(t, "version: 1\n")
	nForget := w.closedOwn(forget, w.person, t0, "", func(d *marker.Data) { d.Ack, d.OptIn = true, hash })
	// A repository that is not opted in.
	w.repo("acme/web", nil, "README.md", "not opted in\n")
	w.ok()

	ops := &config.Operations{
		Version: 1,
		Recreate: []config.RecreateOp{
			{Target: "acme/by-op", Head: opHead},
			{Target: "gh:acme/moved", Head: staleHead},
			{Target: "acme/erased", Head: erasedHead},
			{Target: "acme/web", Head: strings.Repeat("ab", 20)},
			{Target: "acme/nowhere", Head: strings.Repeat("cd", 20)},
		},
		ForgetDeclines: []config.ForgetOp{{Target: "acme/forget", PR: nForget}, {Target: "acme/forget", PR: 99}},
		AllowMassClose: &config.MassCloseOp{Max: 40, Until: "2099-12-31"},
		AdoptUnmarked:  &config.UntilOp{Until: "2099-12-31"},
	}
	flags := &config.Operations{Version: 1, AllowMassClose: &config.MassCloseOp{Max: 10, Until: "2020-01-01"}}
	edit := func(d *Deps) {
		d.Now = func() time.Time { return t0.Add(10 * 24 * time.Hour) }
		d.Write.Operations, d.Write.LocalOps = ops, flags
	}
	dp, dd := w.deps(ModePlan), w.deps(ModeDryRun)
	edit(&dp)
	edit(&dd)
	plan := w.run(dp, ModePlan)
	dry := w.run(dd, ModeDryRun)
	same(t, plan, dry)
	if a, b := opsJSON(t, plan.Operations), opsJSON(t, dry.Operations); a != b {
		t.Errorf("plan and dry run differ:\nplan %s\ndry  %s", a, b)
	}
	want(t, plan, "gh:acme/by-op", report.OutcomeOpened, "", 0)
	want(t, plan, "gh:acme/erased", report.OutcomeUpdated, "recreate", nErased)
	want(t, plan, "gh:acme/migrated", report.OutcomeUpdated, "content", nProto)
	want(t, plan, "gh:acme/forget", report.OutcomeOpened, "", 0)

	type opWant struct {
		kind, target string
		flag         bool
		pr           int64
		effect       string
		detail       []string
	}
	short := func(s string) string { return s[:7] }
	wants := []opWant{
		{kind: "recreate", target: "acme/by-op", effect: report.EffectApplies,
			detail: []string{"head " + short(opHead) + " matches: rebuilds touchmark/acme-eng on the default branch and drops what others added",
				"was added after touchmark's commit"}},
		{kind: "recreate", target: "gh:acme/moved", effect: report.EffectNone,
			detail: []string{"no sync branch is at " + short(staleHead) + " (touchmark/acme-eng is at ", "it does nothing; remove it"}},
		{kind: "recreate", target: "acme/erased", pr: nErased, effect: report.EffectApplies,
			detail: []string{"head " + short(erasedHead) + " matches: rebuilds touchmark/acme-eng on the default branch; takes over #" +
				fmt.Sprint(nErased) + ", whose marker is missing or broken"}},
		{kind: "recreate", target: "acme/web", effect: report.EffectUnknown, detail: []string{"the target was not inspected: skipped:not-opted-in"}},
		{kind: "recreate", target: "acme/nowhere", effect: report.EffectNone, detail: []string{"names no target of this run"}},
		{kind: "forget_declines", target: "acme/forget", pr: nForget, effect: report.EffectApplies,
			detail: []string{fmt.Sprintf("revokes the decline of #%d: its content may be proposed again", nForget)}},
		{kind: "forget_declines", target: "acme/forget", pr: 99, effect: report.EffectNone,
			detail: []string{"#99 is no pull request of touchmark's on the target's sync branches"}},
		{kind: "allow_mass_close", effect: report.EffectNone,
			detail: []string{"not needed: this run closes 0 of touchmark's ", "open pull requests, within the guard's limit of 5"}},
		{kind: "adopt_unmarked", effect: report.EffectApplies,
			detail: []string{fmt.Sprintf("adopts 1 unmarked pull request on branch aliases: gh:acme/migrated #%d", nProto)}},
		{kind: "allow_mass_close", flag: true, effect: report.EffectExpired, detail: []string{"its date has passed"}},
	}
	if len(plan.Operations) != len(wants) {
		t.Fatalf("operations %s, want %d", opsJSON(t, plan.Operations), len(wants))
	}
	for i, wo := range wants {
		op := plan.Operations[i]
		if op.Kind != wo.kind || op.Target != wo.target || op.Flag != wo.flag || op.PR != wo.pr || op.Effect != wo.effect {
			t.Errorf("operation %d: %+v, want %+v", i, op, wo)
		}
		for _, d := range wo.detail {
			if !strings.Contains(op.Detail, d) {
				t.Errorf("operation %d: detail %q lacks %q", i, op.Detail, d)
			}
		}
	}

	// The text output lists them after the outcome groups.
	var b strings.Builder
	if err := plan.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "\nOperations\n") {
		t.Errorf("text output lacks the Operations section:\n%s", out)
	}
	for _, row := range [][]string{
		{"recreate", "acme/by-op", "head"},
		{"forget_declines", "acme/forget", fmt.Sprintf("#%d", nForget), "revokes"},
		{"allow_mass_close", "max", "40", "until", "2099-12-31", "not"},
		{"adopt_unmarked", "until", "2099-12-31", "adopts"},
		{"allow_mass_close", "(flag)", "max", "10", "until", "2020-01-01", "its"},
	} {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= len(row) && slices.Equal(f[:len(row)], row) {
				found = true
			}
		}
		if !found {
			t.Errorf("text output lacks a line %q:\n%s", row, out)
		}
	}
	if strings.Index(out, "\nOperations\n") < strings.Index(out, "\n  open ") {
		t.Errorf("Operations come before the outcome groups:\n%s", out)
	}
}

func opsJSON(t *testing.T, ops []report.Operation) string {
	t.Helper()
	data, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
