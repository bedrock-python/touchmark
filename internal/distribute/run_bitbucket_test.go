package distribute

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The delivery scenarios on Bitbucket Cloud's flavor of the fake: no
// labels, the marker as a Markdown reference definition, no tick boxes in
// descriptions, and declined pull requests that no one can edit or reopen
// (Caps.ClosedImmutable). Every run goes through Run's full phase C in git
// mode.

// bbWorld is a git world of the Bitbucket flavor.
func bbWorld(t *testing.T) *gitWorld {
	t.Helper()
	g := newGitWorld(t, fake.WithFlavor(fake.Bitbucket))
	if c := g.p.Caps(); !c.ClosedImmutable || !c.NoLabels || c.BodyControls() {
		t.Fatalf("fixture: caps %+v", c)
	}
	return g
}

// bbForget is a deps edit that adds the operations.yml entry forgetting
// pull request n of acme/api (nil ops when n is 0).
func bbForget(n int64) func(*Deps) {
	return func(d *Deps) {
		if n > 0 {
			d.Write.Operations = &config.Operations{Version: 1, ForgetDeclines: []config.ForgetOp{{Target: "acme/api", PR: n}}}
		}
	}
}

// bbCheckBody checks what touchmark writes into a description on
// Bitbucket: the marker as a reference definition on the last line, no HTML
// comment and no tick box anywhere, the forget_declines hint in the
// footnote, no labels; it returns the marker.
func (g *gitWorld) bbCheckBody(pr platform.PR) marker.Marker {
	g.t.Helper()
	last := pr.Body[strings.LastIndexByte(pr.Body, '\n')+1:]
	if !strings.HasPrefix(last, `[touchmark]: # "touchmark:v1 `) || strings.Contains(pr.Body, "<!--") || strings.Contains(pr.Body, "- [ ]") {
		g.t.Errorf("#%d: the body is not Bitbucket's:\n%s", pr.Number, pr.Body)
	}
	if pr.State == platform.Open && !strings.Contains(pr.Body, "add a `forget_declines` entry with the number of this pull request") {
		g.t.Errorf("#%d: the footnote does not name forget_declines:\n%s", pr.Number, pr.Body)
	}
	if len(pr.Labels) != 0 {
		g.t.Errorf("#%d: labels %q", pr.Number, pr.Labels)
	}
	m, status := marker.Find(pr.Body, []string{hubFP})
	if status != marker.Found {
		g.t.Fatalf("#%d: marker %s", pr.Number, status)
	}
	if len(m.Data.LabelsSet) != 0 {
		g.t.Errorf("#%d: the marker records labels %q", pr.Number, m.Data.LabelsSet)
	}
	return m
}

// bbNoLabelCalls fails the test when the run asked the fake for labels.
func (g *gitWorld) bbNoLabelCalls() {
	g.t.Helper()
	for _, c := range g.p.Calls() {
		if strings.Contains(c, "Label") {
			g.t.Errorf("a label call on Bitbucket: %q", c)
		}
	}
}

// bbStep runs a plan and a dry run (they must agree and write nothing),
// then distribute and distribute again (which must write nothing), and
// checks the target acme/api of distribute's report: outcome, reason, pull
// request (the new one's number for opened, which the plan does not know)
// and how many writes the fake saw. The report counts the requests: one
// more for a close, which Bitbucket's driver sends as an edit and a
// decline. It returns the plan's report.
func (g *gitWorld) bbStep(what string, edit func(*Deps), outcome report.Outcome, reason string, pr int64, writes int) *report.Delivery {
	g.t.Helper()
	plan := g.both(edit)
	planned := pr
	if outcome == report.OutcomeOpened {
		planned = 0
	}
	want(g.t, plan, "gh:acme/api", outcome, reason, planned)
	d := g.deps(ModeDistribute)
	if edit != nil {
		edit(&d)
	}
	rep := g.run(d, ModeDistribute)
	g.bbNoLabelCalls()
	tg := want(g.t, rep, "gh:acme/api", outcome, reason, pr)
	requests := writes
	if outcome == report.OutcomeClosed {
		requests++
	}
	if got := len(g.p.Writes()); got != writes || tg.Writes != requests {
		g.t.Errorf("%s: %d writes (report %d), want %d (%d): %q", what, got, tg.Writes, writes, requests, g.p.Writes())
	}
	again := g.run(d, ModeDistribute)
	if w := g.p.Writes(); len(w) > 0 {
		g.t.Errorf("%s: distribute right after wrote %q (%s)", what, w, outcomes(again))
	}
	for _, tg := range again.Targets {
		if tg.Outcome == report.OutcomeFailed {
			g.t.Errorf("%s: the second run failed %s: %s %q", what, tg.Path, tg.Reason, tg.Warnings)
		}
	}
	return plan
}

// TestRunBitbucketDecline: the life of a decline on Bitbucket. A new pull
// request records the opt-in state in its marker. A person declines it:
// the target is declined, nothing is written to the declined pull request
// (no ack, no comment) and nothing fails, run after run. A change of the
// opt-in file lifts the decline and the content comes again in a new pull
// request; an opt-in change while a pull request is open is recorded in its
// marker, so that declining it then still holds. A forget_declines entry
// brings declined content back while the entry is present, and an open
// pull request no longer needs it.
func TestRunBitbucketDecline(t *testing.T) {
	t.Parallel()
	g := bbWorld(t)
	api := g.optedIn("acme/api", nil)
	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	g.bbStep("the first run", nil, report.OutcomeOpened, "", 1, 2)
	pr := g.p.PR(api.ID, 1)
	if m := g.bbCheckBody(pr); m.Data.OptIn != optInHash(t, "version: 1\n") || m.Data.Ack {
		t.Errorf("#1: optin %q ack %v, want the opt-in file's hash, no ack", m.Data.OptIn, m.Data.Ack)
	}

	// A person declines it: declined, with no write at all.
	g.p.SetPRState(api.ID, 1, platform.Closed, &g.person, t0)
	g.ok()
	g.bbStep("declined", nil, report.OutcomeDeclined, "", 1, 0)
	if cs := g.p.Comments(api.ID, 1); len(cs) != 0 {
		t.Errorf("comments on the declined pull request: %+v", cs)
	}
	if got := g.p.PR(api.ID, 1); got.Body != pr.Body {
		t.Errorf("the declined pull request changed:\n%s", got.Body)
	}
	// A comment in the opt-in file parses the same: still declined.
	g.push(api, "main", g.person, optInName, "version: 1\n# reviewed by the platform team\n")
	g.bbStep("a comment in the opt-in file", nil, report.OutcomeDeclined, "", 1, 0)

	// The team changes the opt-in file: the decline lapses, a new pull
	// request carries the content.
	optIn2 := "version: 1\nignore: [notes/**]\n"
	g.push(api, "main", g.person, optInName, optIn2)
	g.bbStep("the opt-in file changed", nil, report.OutcomeOpened, "", 2, 2)
	pr2 := g.p.PR(api.ID, 2)
	if m := g.bbCheckBody(pr2); pr2.State != platform.Open || m.Data.OptIn != optInHash(t, optIn2) {
		t.Errorf("#2: %s, optin %q", pr2.State, m.Data.OptIn)
	}

	// The opt-in file changes again while #2 is open, the content does not:
	// the marker records the new state (one edit), so that a decline of #2
	// now holds under it.
	optIn3 := "version: 1\nignore: [notes/**, drafts/**]\n"
	g.push(api, "main", g.person, optInName, optIn3)
	g.bbStep("the opt-in file changed under the open pull request", nil, report.OutcomeUpdated, "body", 2, 1)
	if m := g.bbCheckBody(g.p.PR(api.ID, 2)); m.Data.OptIn != optInHash(t, optIn3) {
		t.Errorf("#2: optin %q after the opt-in change", m.Data.OptIn)
	}
	g.p.SetPRState(api.ID, 2, platform.Closed, &g.person, t0.Add(time.Hour))
	g.ok()
	g.bbStep("#2 declined", nil, report.OutcomeDeclined, "", 2, 0)

	// A forget_declines entry: proposed again while it is present; the plan
	// says it applies, and why it must stay.
	plan := g.bbStep("forget #2", bbForget(2), report.OutcomeOpened, "", 3, 2)
	i := slices.IndexFunc(plan.Operations, func(op report.Operation) bool { return op.Kind == report.OpForgetDeclines })
	if i < 0 || plan.Operations[i].Effect != report.EffectApplies || !strings.Contains(plan.Operations[i].Detail, "while the entry is present") {
		t.Errorf("operations %+v", plan.Operations)
	}
	if pr3 := g.p.PR(api.ID, 3); strings.Contains(pr3.Body, "Previously declined") {
		t.Errorf("#3 names the forgotten decline:\n%s", pr3.Body)
	}
	g.bbCheckBody(g.p.PR(api.ID, 3))
	// #3 is open and stronger than memory: without the entry it stays, and
	// only its description now names #2 as previously declined (one edit).
	g.bbStep("#3 open, the entry removed", nil, report.OutcomeUpdated, "body", 3, 1)
	if pr3 := g.p.PR(api.ID, 3); pr3.State != platform.Open || !strings.Contains(pr3.Body, "Some of these changes were in #2") {
		t.Errorf("#3: %s\n%s", pr3.State, pr3.Body)
	}
	for _, n := range []int64{1, 2} {
		if cs := g.p.Comments(api.ID, n); len(cs) != 0 {
			t.Errorf("comments on #%d: %+v", n, cs)
		}
	}
}

// TestRunBitbucketForgetWhilePresent: a forget_declines entry acts on
// every run while it is present, not once: the pull request it brought is
// declined in turn and holds, and when the entry goes, the decline it named
// holds again.
func TestRunBitbucketForgetWhilePresent(t *testing.T) {
	t.Parallel()
	g := bbWorld(t)
	api := g.optedIn("acme/api", nil)
	g.bbStep("the first run", nil, report.OutcomeOpened, "", 1, 2)
	g.p.SetPRState(api.ID, 1, platform.Closed, &g.person, time.Time{})
	g.ok()
	g.bbStep("forget #1", bbForget(1), report.OutcomeOpened, "", 2, 1)
	g.p.SetPRState(api.ID, 2, platform.Closed, &g.person, time.Time{})
	g.ok()
	g.bbStep("#2 declined, the entry still present", bbForget(1), report.OutcomeDeclined, "", 2, 0)
	g.bbStep("the entry removed", nil, report.OutcomeDeclined, "", 2, 0)
}

// TestRunBitbucketUnanchored: a declined pull request whose marker records
// no opt-in state (a marker touchmark did not write in full) holds whatever
// the opt-in file says, and the report says how to lift it; a
// forget_declines entry does.
func TestRunBitbucketUnanchored(t *testing.T) {
	t.Parallel()
	g := bbWorld(t)
	api := g.optedIn("acme/api", nil)
	n := g.closedOwn(api, g.person, time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), "", nil)
	if m, _ := marker.Find(g.p.PR(api.ID, n).Body, []string{hubFP}); m.Data.OptIn != "" {
		t.Fatalf("fixture: optin %q", m.Data.OptIn)
	}
	plan := g.bbStep("declined without optin", nil, report.OutcomeDeclined, "", n, 0)
	if tg := targetOf(t, plan, "gh:acme/api"); !hasWarningLike(tg, "the decline of #1 holds until a forget_declines entry", "records no state of the opt-in file") {
		t.Errorf("warnings %q", tg.Warnings)
	}
	g.push(api, "main", g.person, optInName, "version: 1\nignore: [notes/**]\n")
	g.bbStep("the opt-in file changed", nil, report.OutcomeDeclined, "", n, 0)
	g.bbStep("forgotten", bbForget(n), report.OutcomeOpened, "", 2, 2)
}

// TestRunBitbucketSelfClose: touchmark closes its own pull request on
// Bitbucket in one edit that writes the closed marker before it declines
// it: the target took the hub's files (no-diff), or it left the targets
// (target-dropped). Neither close is a decline: the content comes again
// when the hub changes it, and no write touches the declined pull requests.
func TestRunBitbucketSelfClose(t *testing.T) {
	t.Parallel()
	g := bbWorld(t)
	api := g.optedIn("acme/api", nil)
	g.bbStep("the first run", nil, report.OutcomeOpened, "", 1, 2)

	// The team takes the hub's files: closed:no-diff (the edit, the comment,
	// the branch's deletion).
	g.push(api, "main", g.person, baseFiles...)
	g.bbStep("no-diff", nil, report.OutcomeClosed, "no-diff", 1, 3)
	pr := g.p.PR(api.ID, 1)
	m := g.bbCheckBody(pr)
	if pr.State != platform.Closed || pr.ClosedBy == nil || pr.ClosedBy.ID != g.writer.ID || m.Data.Closed == nil ||
		m.Data.Closed.By != "touchmark" || m.Data.Closed.Reason != "no-diff" {
		t.Errorf("#1: %s by %v, closed %+v", pr.State, pr.ClosedBy, m.Data.Closed)
	}
	g.bbStep("after the close", nil, report.OutcomeUnchanged, "", 0, 0)

	// The hub changes AGENTS.md: a new pull request, not held back by #1.
	agents3 := version("AGENTS.md", 3)
	man, cur := manifestAndCurrent()
	v := provenance.Version{OID: oid(agents3), Size: int64(len(agents3))}
	man.Add("AGENTS.md", "base", v)
	cur["base"]["AGENTS.md"] = provenance.File{Pack: "base", Path: "AGENTS.md", OID: v.OID, Size: v.Size, Mode: "100644"}
	g.blobs[v.OID] = []byte(agents3)
	g.edit = func(d *Deps) { d.Manifest, d.Current = man, cur }
	g.bbStep("the hub changed", nil, report.OutcomeOpened, "", 2, 2)
	if pr := g.p.PR(api.ID, 2); pr.State != platform.Open {
		t.Errorf("#2: %s", pr.State)
	}

	// The repository leaves the targets: the sweep closes #2 the same way.
	g.targetsYML = defaultTargetsYML + "  - acme/api\n"
	rep := g.run(g.deps(ModeDistribute), ModeDistribute)
	g.bbNoLabelCalls()
	want(t, rep, "gh:acme/api", report.OutcomeClosed, "target-dropped", 2)
	pr = g.p.PR(api.ID, 2)
	if m := g.bbCheckBody(pr); pr.State != platform.Closed || m.Data.Closed == nil || m.Data.Closed.Reason != "target-dropped" {
		t.Errorf("#2: %s, closed %+v", pr.State, m.Data.Closed)
	}
	again := g.run(g.deps(ModeDistribute), ModeDistribute)
	if w := g.p.Writes(); len(w) > 0 {
		t.Errorf("a run after the sweep wrote %q (%s)", w, outcomes(again))
	}
	// Back among the targets: proposed again, no decline in the way.
	g.targetsYML = defaultTargetsYML
	g.bbStep("back among the targets", nil, report.OutcomeOpened, "", 3, 1)
}

// TestRunOptInChangeElsewhere: on a platform whose closed pull requests can
// be edited, an opt-in change that leaves the content as it is writes
// nothing to the open pull request: its description stays byte for byte
// (the ack records the opt-in state when a decline is first seen).
func TestRunOptInChangeElsewhere(t *testing.T) {
	t.Parallel()
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			g := newGitWorld(t, fake.WithFlavor(f))
			api := g.optedIn("acme/api", nil)
			rep := g.distribute(nil)
			want(t, rep, "gh:acme/api", report.OutcomeOpened, "", 1)
			before := g.p.PR(api.ID, 1)
			if !strings.Contains(before.Body, "<!-- touchmark:v1") || strings.Contains(before.Body, "forget_declines") {
				t.Errorf("the body is not the comment-frame one:\n%s", before.Body)
			}
			g.push(api, "main", g.person, optInName, "version: 1\nignore: [notes/**]\n")
			rep = g.distribute(nil)
			want(t, rep, "gh:acme/api", report.OutcomeUnchanged, "", 1)
			if tg := targetOf(t, rep, "gh:acme/api"); tg.Writes != 0 {
				t.Errorf("%d writes", tg.Writes)
			}
			if after := g.p.PR(api.ID, 1); after.Body != before.Body {
				t.Errorf("the body changed:\n%s\nwas\n%s", after.Body, before.Body)
			}
		})
	}
}
