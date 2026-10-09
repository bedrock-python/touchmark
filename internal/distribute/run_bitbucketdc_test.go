package distribute

import (
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The delivery scenarios on Bitbucket Data Center's flavor of the fake:
// Bitbucket Cloud's (no labels, the marker as a Markdown reference
// definition, no tick boxes, touchmark writes no declined pull request)
// with a shorter description, and declined pull requests that a person may
// reopen. Every run goes through Run's full phase C in git mode.

// dcWorld is a git world of the Bitbucket Data Center flavor, whose hub
// lists the reader among automation_accounts, as the guide asks (else a
// plan warns, and a dry run, under the writer, does not).
func dcWorld(t *testing.T) *gitWorld {
	t.Helper()
	g := newGitWorld(t, fake.WithFlavor(fake.BitbucketDataCenter))
	if c := g.p.Caps(); !c.ClosedImmutable || !c.NoLabels || c.BodyControls() || c.MaxBody != 30000 || !c.ReaderCloses {
		t.Fatalf("fixture: caps %+v", c)
	}
	g.hubYML = dcHubYML
	return g
}

// dcHubYML is the default hub.yml with the reader among
// automation_accounts.
var dcHubYML = strings.Replace(defaultHubYML, "    known_authors:", "    automation_accounts: [\"acme-read[bot]\"]\n    known_authors:", 1)

// TestRunBitbucketDataCenterDecline: a person declines touchmark's pull
// request: declined, with no write to it, run after run. A person reopens
// it: it is touchmark's open pull request again, and nothing needs
// writing. A bot declines it: an automatic close, which holds the content
// back for the cooldown only.
func TestRunBitbucketDataCenterDecline(t *testing.T) {
	t.Parallel()
	g := dcWorld(t)
	api := g.optedIn("acme/api", nil)
	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	g.bbStep("the first run", nil, report.OutcomeOpened, "", 1, 2)
	pr := g.bbCheckBody(g.p.PR(api.ID, 1))
	if pr.Data.OptIn == "" {
		t.Error("#1 records no opt-in state")
	}

	g.p.SetPRState(api.ID, 1, platform.Closed, &g.person, t0)
	g.ok()
	g.bbStep("declined", nil, report.OutcomeDeclined, "", 1, 0)

	// A person reopens it.
	g.p.SetPRState(api.ID, 1, platform.Open, nil, time.Time{})
	g.ok()
	g.bbStep("reopened", nil, report.OutcomeUnchanged, "", 1, 0)
	if got := g.p.PR(api.ID, 1); got.State != platform.Open {
		t.Errorf("#1 is %s after the reopening", got.State)
	}

	// A bot declines it (as Bitbucket's system user does after weeks
	// without activity): not the team's decision.
	g.p.SetPRState(api.ID, 1, platform.Closed, &g.bot, t0.Add(time.Hour))
	g.ok()
	plan := g.both(nil)
	if tg := targetOf(t, plan, "gh:acme/api"); tg.Outcome == report.OutcomeDeclined {
		t.Errorf("a bot's decline is the team's: %s %s", tg.Outcome, tg.Reason)
	}
}

// TestPlanReaderCloses: on a platform where the read credential can close
// pull requests, a plan warns until its account is in automation_accounts.
func TestPlanReaderCloses(t *testing.T) {
	const warning = "the read credential's account acme-read[bot] can decline pull requests on bitbucket-datacenter"
	w := newWorld(t, fake.WithFlavor(fake.BitbucketDataCenter))
	w.optedIn("acme/api", nil)
	if rep := w.plan(w.deps()); !hasWarning(rep.Warnings, warning) {
		t.Errorf("warnings %q", rep.Warnings)
	}
	w.hubYML = dcHubYML
	if rep := w.plan(w.deps()); hasWarning(rep.Warnings, "can decline pull requests") {
		t.Errorf("warnings %q with the reader among automation_accounts", rep.Warnings)
	}
	gh := newWorld(t)
	gh.optedIn("acme/api", nil)
	if rep := gh.plan(gh.deps()); hasWarning(rep.Warnings, "can decline pull requests") {
		t.Errorf("warnings %q on GitHub", rep.Warnings)
	}
}

// TestRunBitbucketDataCenterSelfClose: touchmark closes its own pull
// request in one edit that writes the closed marker, then declines it.
func TestRunBitbucketDataCenterSelfClose(t *testing.T) {
	t.Parallel()
	g := dcWorld(t)
	api := g.optedIn("acme/api", nil)
	g.bbStep("the first run", nil, report.OutcomeOpened, "", 1, 2)
	g.push(api, "main", g.person, baseFiles...)
	g.bbStep("no-diff", nil, report.OutcomeClosed, "no-diff", 1, 3)
	pr := g.p.PR(api.ID, 1)
	m := g.bbCheckBody(pr)
	if pr.State != platform.Closed || m.Data.Closed == nil || m.Data.Closed.By != "touchmark" {
		t.Errorf("#1: %s, closed %+v", pr.State, m.Data.Closed)
	}
	g.bbStep("after the close", nil, report.OutcomeUnchanged, "", 0, 0)
}
