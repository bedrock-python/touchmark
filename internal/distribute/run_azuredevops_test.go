package distribute

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The delivery scenarios on Azure DevOps' flavor of the fake: the marker
// lives in a pull request property, apart from a description of at most
// 4 000 bytes (Caps.MarkerInProperties); labels go by name; abandoned pull
// requests are never written to again (Caps.ClosedImmutable). Every run
// goes through Run's full phase C in git mode.

// azWorld is a git world of the Azure DevOps flavor.
func azWorld(t *testing.T) *gitWorld {
	t.Helper()
	g := newGitWorld(t, fake.WithFlavor(fake.AzureDevOps))
	if c := g.p.Caps(); !c.ClosedImmutable || c.NoLabels || !c.BodyControls() || c.Marker != platform.MarkerInProperties || c.MaxBody != 4000 {
		t.Fatalf("fixture: caps %+v", c)
	}
	return g
}

// azCheckBody checks what touchmark writes on Azure DevOps: a description
// of at most 4 000 bytes without a marker line, the marker in the comment
// frame kept apart (the body's last line as the driver reads it back), the
// forget_declines hint in the footnote of an open pull request, the hub's
// labels; it returns the marker.
func (g *gitWorld) azCheckBody(pr platform.PR) marker.Marker {
	g.t.Helper()
	desc, line := marker.Detach(pr.Body)
	if len(desc) > 4000 {
		g.t.Errorf("#%d: a description of %d bytes", pr.Number, len(desc))
	}
	if !strings.HasPrefix(line, "<!-- touchmark:v1 ") || marker.Attach(desc, line) != pr.Body {
		g.t.Errorf("#%d: the body does not split into a description and a marker line:\n%s", pr.Number, pr.Body)
	}
	if pr.State == platform.Open && !strings.Contains(pr.Body, "add a `forget_declines` entry with the number of this pull request") {
		g.t.Errorf("#%d: the footnote does not name forget_declines:\n%s", pr.Number, pr.Body)
	}
	if !slices.Equal(pr.Labels, []string{"engineering-assets"}) {
		g.t.Errorf("#%d: labels %q", pr.Number, pr.Labels)
	}
	m, status := marker.Find(pr.Body, []string{hubFP})
	if status != marker.Found {
		g.t.Fatalf("#%d: marker %s", pr.Number, status)
	}
	if !slices.Equal(m.Data.LabelsSet, []string{"engineering-assets"}) {
		g.t.Errorf("#%d: the marker records labels %q", pr.Number, m.Data.LabelsSet)
	}
	return m
}

// azStep runs a plan and a dry run (they must agree and write nothing),
// then distribute and distribute again (which must write nothing), and
// checks the target acme/api of distribute's report: outcome, reason, pull
// request (the new one's number for opened, which the plan does not know)
// and how many writes the fake saw (the first pull request of a repository
// creates its label too). It returns the plan's report.
func (g *gitWorld) azStep(what string, edit func(*Deps), outcome report.Outcome, reason string, pr int64, writes int) *report.Delivery {
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
	want(g.t, rep, "gh:acme/api", outcome, reason, pr)
	if got := len(g.p.Writes()); got != writes {
		g.t.Errorf("%s: %d writes, want %d: %q", what, got, writes, g.p.Writes())
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

// TestRunAzureDevOpsDecline: the life of a decline on Azure DevOps. A new
// pull request carries labels and records the opt-in state in its marker,
// which lives apart from its description. A person abandons it: the target
// is declined, nothing is written to the abandoned pull request and nothing
// fails, run after run. A change of the opt-in file lifts the decline and
// the content comes again in a new pull request; a forget_declines entry
// brings declined content back while the entry is present.
func TestRunAzureDevOpsDecline(t *testing.T) {
	t.Parallel()
	g := azWorld(t)
	api := g.optedIn("acme/api", nil)
	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	g.azStep("the first run", nil, report.OutcomeOpened, "", 1, 3)
	pr := g.p.PR(api.ID, 1)
	if m := g.azCheckBody(pr); m.Data.OptIn != optInHash(t, "version: 1\n") || m.Data.Ack {
		t.Errorf("#1: optin %q ack %v, want the opt-in file's hash, no ack", m.Data.OptIn, m.Data.Ack)
	}

	// A person abandons it: declined, with no write at all.
	g.p.SetPRState(api.ID, 1, platform.Closed, &g.person, t0)
	g.ok()
	g.azStep("declined", nil, report.OutcomeDeclined, "", 1, 0)
	if cs := g.p.Comments(api.ID, 1); len(cs) != 0 {
		t.Errorf("comments on the abandoned pull request: %+v", cs)
	}
	if got := g.p.PR(api.ID, 1); got.Body != pr.Body {
		t.Errorf("the abandoned pull request changed:\n%s", got.Body)
	}

	// The team changes the opt-in file: the decline lapses, a new pull
	// request carries the content.
	optIn2 := "version: 1\nignore: [notes/**]\n"
	g.push(api, "main", g.person, optInName, optIn2)
	g.azStep("the opt-in file changed", nil, report.OutcomeOpened, "", 2, 2)
	pr2 := g.p.PR(api.ID, 2)
	if m := g.azCheckBody(pr2); pr2.State != platform.Open || m.Data.OptIn != optInHash(t, optIn2) {
		t.Errorf("#2: %s, optin %q", pr2.State, m.Data.OptIn)
	}

	// The opt-in file changes again while #2 is open: the marker records
	// the new state (one edit), the description stays as it was.
	optIn3 := "version: 1\nignore: [notes/**, drafts/**]\n"
	g.push(api, "main", g.person, optInName, optIn3)
	g.azStep("the opt-in file changed under the open pull request", nil, report.OutcomeUpdated, "body", 2, 1)
	after := g.p.PR(api.ID, 2)
	if m := g.azCheckBody(after); m.Data.OptIn != optInHash(t, optIn3) {
		t.Errorf("#2: optin %q after the opt-in change", m.Data.OptIn)
	}
	if a, b := marker.Strip(after.Body), marker.Strip(pr2.Body); a != b {
		t.Errorf("#2: the description changed with the marker:\n%s\nwas\n%s", a, b)
	}
	g.p.SetPRState(api.ID, 2, platform.Closed, &g.person, t0.Add(time.Hour))
	g.ok()
	g.azStep("#2 declined", nil, report.OutcomeDeclined, "", 2, 0)

	// A forget_declines entry: proposed again while it is present.
	plan := g.azStep("forget #2", bbForget(2), report.OutcomeOpened, "", 3, 2)
	i := slices.IndexFunc(plan.Operations, func(op report.Operation) bool { return op.Kind == report.OpForgetDeclines })
	if i < 0 || plan.Operations[i].Effect != report.EffectApplies || !strings.Contains(plan.Operations[i].Detail, "while the entry is present") {
		t.Errorf("operations %+v", plan.Operations)
	}
	g.azCheckBody(g.p.PR(api.ID, 3))
	g.azStep("#3 open, the entry removed", nil, report.OutcomeUpdated, "body", 3, 1)
	for _, n := range []int64{1, 2} {
		if cs := g.p.Comments(api.ID, n); len(cs) != 0 {
			t.Errorf("comments on #%d: %+v", n, cs)
		}
	}
}

// TestRunAzureDevOpsSelfClose: touchmark abandons its own pull request in
// one edit whose body carries the closed marker, which the driver stores
// before it abandons the pull request. The close is no decline: the content
// comes again when the hub changes it, and no write touches the abandoned
// pull request.
func TestRunAzureDevOpsSelfClose(t *testing.T) {
	t.Parallel()
	g := azWorld(t)
	api := g.optedIn("acme/api", nil)
	g.azStep("the first run", nil, report.OutcomeOpened, "", 1, 3)

	g.push(api, "main", g.person, baseFiles...)
	g.azStep("no-diff", nil, report.OutcomeClosed, "no-diff", 1, 3)
	pr := g.p.PR(api.ID, 1)
	m := g.azCheckBody(pr)
	if pr.State != platform.Closed || pr.ClosedBy == nil || pr.ClosedBy.ID != g.writer.ID || m.Data.Closed == nil ||
		m.Data.Closed.By != "touchmark" || m.Data.Closed.Reason != "no-diff" {
		t.Errorf("#1: %s by %v, closed %+v", pr.State, pr.ClosedBy, m.Data.Closed)
	}
	g.azStep("after the close", nil, report.OutcomeUnchanged, "", 0, 0)

	g.edit = bbHubChange(g)
	g.azStep("the hub changed", nil, report.OutcomeOpened, "", 2, 2)
	g.azCheckBody(g.p.PR(api.ID, 2))
}

// TestRunAzureDevOpsLongBody: a pull request whose changes would need a
// long description fits Azure DevOps' 4 000 characters: the lists are cut,
// the marker lives apart, and it is never cut.
func TestRunAzureDevOpsLongBody(t *testing.T) {
	t.Parallel()
	g := azWorld(t)
	var files []string
	for i := range 120 {
		files = append(files, "docs/handbook/chapter-with-a-long-name/section-"+strings.Repeat("x", 30)+"-"+string(rune('a'+i%26))+string(rune('a'+i/26))+".md", "text\n")
	}
	g.pack(files...)
	api := g.optedIn("acme/api", nil)
	g.azStep("the first run", nil, report.OutcomeOpened, "", 1, 3)
	pr := g.p.PR(api.ID, 1)
	m := g.azCheckBody(pr)
	if len(m.Data.Changes) != 120 || !m.Data.ChangesComplete {
		t.Errorf("the marker records %d changes (complete %v), want all 120", len(m.Data.Changes), m.Data.ChangesComplete)
	}
	if !strings.Contains(pr.Body, "more.") {
		t.Errorf("the description lists every change in 4 000 bytes?\n%s", pr.Body)
	}
}

// TestRunAzureDevOpsMarkerLost: the marker property of a new pull request
// cannot be stored. The driver abandons the pull request and the core
// opens another, or, when the abandon failed too and the pull request is
// active without its marker, the core's reconcile stores the marker
// through an edit. Either way the target is opened with a marker, and the
// next run writes nothing: no marker-invalid block.
func TestRunAzureDevOpsMarkerLost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		leaveOpen bool
		pr        int64
	}{
		{"abandoned", false, 2},
		{"left active", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := azWorld(t)
			api := g.optedIn("acme/api", nil)
			g.p.FailMarkerNext(exHTTPErr(platform.ClassTransient, http.StatusServiceUnavailable), tc.leaveOpen)
			g.ok()
			rep := g.run(g.deps(ModeDistribute), ModeDistribute)
			want(t, rep, "gh:acme/api", report.OutcomeOpened, "", tc.pr)
			pr := g.p.PR(api.ID, tc.pr)
			if pr.State != platform.Open {
				t.Fatalf("#%d is %s", tc.pr, pr.State)
			}
			g.azCheckBody(pr)
			if !tc.leaveOpen {
				first := g.p.PR(api.ID, 1)
				if _, line := marker.Detach(first.Body); first.State != platform.Closed || line != "" ||
					first.ClosedBy == nil || first.ClosedBy.ID != g.writer.ID {
					t.Errorf("#1: %s by %v, marker %q: want abandoned by the writer without a marker", first.State, first.ClosedBy, line)
				}
			} else if edits := slices.IndexFunc(g.p.Writes(), func(w string) bool { return strings.HasPrefix(w, "EditPR") }); edits < 0 {
				t.Errorf("no edit stored the marker: %q", g.p.Writes())
			}
			g.azStep("the next run", nil, report.OutcomeUnchanged, "", tc.pr, 0)
		})
	}
}

// TestRunAzureDevOpsEditMarkerLost: the marker property of an edit that
// changes the description fails after the description went through. The
// marker still describes the previous body, so the core's reconcile sees
// the edit unapplied and makes it again: the description is not sent twice,
// the marker is stored, and the next run writes nothing.
func TestRunAzureDevOpsEditMarkerLost(t *testing.T) {
	t.Parallel()
	g := azWorld(t)
	api := g.optedIn("acme/api", nil)
	g.azStep("the first run", nil, report.OutcomeOpened, "", 1, 3)
	g.edit = bbHubChange(g)
	g.p.FailMarkerNext(exHTTPErr(platform.ClassTransient, http.StatusBadGateway), false)
	g.ok()
	rep := g.run(g.deps(ModeDistribute), ModeDistribute)
	tg := targetOf(t, rep, "gh:acme/api")
	if tg.Outcome == report.OutcomeFailed || tg.Outcome == report.OutcomeBlocked {
		t.Fatalf("%s:%s %q", tg.Outcome, tg.Reason, tg.Warnings)
	}
	pr := g.p.PR(api.ID, 1)
	m := g.azCheckBody(pr)
	if want := decide.BodyHash(pr.Body); m.Data.Body != want {
		t.Errorf("the marker records body %s, the description hashes %s", m.Data.Body, want)
	}
	g.azStep("the next run", nil, report.OutcomeUnchanged, "", 1, 0)
}

// azLongDescription has a person fill the description of open pull request
// n of r up to 4 000 bytes but one, ending with a mention: making it inert
// would push it over Azure DevOps' limit.
func (g *gitWorld) azLongDescription(r platform.Repo, n int64) string {
	g.t.Helper()
	var desc string
	g.p.UpdatePR(r.ID, n, func(pr *platform.PR) {
		head := marker.Strip(pr.Body) + "\n\n"
		tail := "\ncc @bob"
		desc = head + strings.Repeat("x", 3999-len(head)-len(tail)) + tail
		pr.Body = desc
	})
	g.ok()
	if len(desc) != 3999 {
		g.t.Fatalf("fixture: a description of %d bytes", len(desc))
	}
	return desc
}

// TestRunAzureDevOpsLongPeopleText: a person appends "cc @bob" to a
// description of nearly 4 000 bytes. Touchmark's writes that keep the
// description (a close, an opt-in refresh) store the marker and leave the
// description byte for byte: none fails for a description touchmark would
// have lengthened, and none mentions anyone, since nothing of people's
// text is sent.
func TestRunAzureDevOpsLongPeopleText(t *testing.T) {
	t.Parallel()

	t.Run("close", func(t *testing.T) {
		t.Parallel()
		g := azWorld(t)
		api := g.optedIn("acme/api", nil)
		g.azStep("the first run", nil, report.OutcomeOpened, "", 1, 3)
		desc := g.azLongDescription(api, 1)
		g.push(api, "main", g.person, baseFiles...)
		g.azStep("no-diff", nil, report.OutcomeClosed, "no-diff", 1, 3)
		pr := g.p.PR(api.ID, 1)
		m, _ := marker.Find(pr.Body, []string{hubFP})
		if pr.State != platform.Closed || m.Data.Closed == nil || m.Data.Closed.By != "touchmark" {
			t.Errorf("#1: %s, closed %+v", pr.State, m.Data.Closed)
		}
		if got, _ := marker.Detach(pr.Body); got != desc {
			t.Errorf("the description changed:\n%q\nwas\n%q", got, desc)
		}
	})

	t.Run("opt-in refresh", func(t *testing.T) {
		t.Parallel()
		g := azWorld(t)
		api := g.optedIn("acme/api", nil)
		g.azStep("the first run", nil, report.OutcomeOpened, "", 1, 3)
		foreign := g.p.AddPR(api.ID, platform.PR{Head: branch, Author: g.person, Title: "wip", Body: "My changes."})
		g.ok()
		g.azStep("someone else's pull request on the sync branch", nil, report.OutcomeBlocked, "branch-in-use", foreign, 0)
		desc := g.azLongDescription(api, 1)
		before := g.p.PR(api.ID, 1)
		optIn2 := "version: 1\nignore: [notes/**]\n"
		g.push(api, "main", g.person, optInName, optIn2)
		g.azStep("the opt-in file changed while blocked", nil, report.OutcomeBlocked, "branch-in-use", foreign, 1)
		after := g.p.PR(api.ID, 1)
		m, status := marker.Find(after.Body, []string{hubFP})
		if status != marker.Found || m.Data.OptIn != optInHash(t, optIn2) {
			t.Errorf("#1: marker %s, optin %q", status, m.Data.OptIn)
		}
		if got, _ := marker.Detach(after.Body); got != desc || after.Title != before.Title {
			t.Errorf("the description changed:\n%q\nwas\n%q", got, desc)
		}
	})
}
