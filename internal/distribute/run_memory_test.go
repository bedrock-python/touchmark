package distribute

import (
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// optInHash is the hash of the opt-in file content (config.OptIn.Hash).
func optInHash(t *testing.T, content string) string {
	t.Helper()
	o, _, err := config.ParseOptIn([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return o.Hash()
}

// closedOwn adds our pull request on the sync branch carrying the base
// pack, closed by `by` at `at`, with its marker data edited by edit; body
// adds text before the marker. It returns its number.
func (g *gitWorld) closedOwn(r platform.Repo, by platform.Account, at time.Time, body string, edit func(*marker.Data)) int64 {
	g.t.Helper()
	n := g.pr(r, platform.PR{Head: branch, Author: g.writer, Title: "chore: sync engineering assets",
		Body: body + markerBody(g.t, keyOf(baseFiles...), hubFP, changesOf(baseFiles...), edit)})
	g.p.SetPRState(r.ID, n, platform.Closed, &by, at)
	g.ok()
	return n
}

// TestRunMemory: what closed pull requests say: a decline in force, acked or
// not; a decline lapsed by the team's choice; the cooldown after a bot
// closed one, and its escalation; revocations.
func TestRunMemory(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	hash := optInHash(t, "version: 1\n")
	acked := func(d *marker.Data) { d.Ack, d.OptIn = true, hash }

	// A decline first seen: ack and comment.
	fresh := w.optedIn("acme/fresh", nil)
	nFresh := w.closedOwn(fresh, w.person, t0, "", nil)
	// A decline acked already: nothing to write.
	old := w.optedIn("acme/acked", nil)
	nOld := w.closedOwn(old, w.person, t0, "", acked)
	// The team edited the opt-in file since the ack: the decline lapsed.
	edited := w.repo("acme/opt-in-edited", nil, optInName, "version: 1\nignore: [notes/**]\n")
	w.closedOwn(edited, w.person, t0, "", acked)
	// The team made a path of the decline its own: lapsed; D is the rest.
	local := w.optedIn("acme/local-path", nil, "AGENTS.md", "our own agents file, written here\n")
	w.closedOwn(local, w.person, t0, "", acked)
	// A bot closed it: a cooldown of 30 days, then proposed again.
	botClosed := w.optedIn("acme/bot-closed", nil)
	nBot := w.closedOwn(botClosed, w.bot, t0, "", nil)
	// A bot closed the same content three times in a row: a decline.
	thrice := w.optedIn("acme/bot-thrice", nil)
	var nThird int64
	for i := range 3 {
		nThird = w.closedOwn(thrice, w.bot, t0.Add(time.Duration(i)*time.Hour), "", nil)
	}
	// People asked for the content again: revoked, then a new one.
	repropose := w.optedIn("acme/repropose", nil)
	w.closedOwn(repropose, w.person, t0,
		"- [x] <!-- touchmark:"+prbody.ControlRepropose+" --> Propose this content again\n\n", acked)
	// operations.yml forgets the decline.
	forget := w.optedIn("acme/forget", nil)
	nForget := w.closedOwn(forget, w.person, t0, "", acked)
	// A merge is no decline.
	merged := w.optedIn("acme/merged", nil)
	nMerged := w.pr(merged, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync",
		Body: markerBody(t, keyOf(baseFiles...), hubFP, changesOf(baseFiles...), nil)})
	w.p.SetPRState(merged.ID, nMerged, platform.Merged, &w.person, t0)

	ops := &config.Operations{Version: 1, ForgetDeclines: []config.ForgetOp{{Target: "acme/forget", PR: nForget}}}
	at := func(now time.Time) func(*Deps) {
		return func(d *Deps) {
			d.Now = func() time.Time { return now }
			d.Write.Operations = ops
		}
	}
	type result struct {
		outcome report.Outcome
		reason  string
		pr      int64
		writes  int
	}
	check := func(rep *report.Delivery, want map[string]result) {
		t.Helper()
		for path, r := range want {
			tg := targetOf(t, rep, "gh:"+path)
			var n int64
			if tg.PR != nil {
				n = tg.PR.Number
			}
			if tg.Outcome != r.outcome || tg.Reason != r.reason || n != r.pr || tg.Writes != r.writes {
				t.Errorf("%s: %s:%s #%d with %d writes, want %s:%s #%d with %d (warnings %q)", path, tg.Outcome, tg.Reason, n, tg.Writes,
					r.outcome, r.reason, r.pr, r.writes, tg.Warnings)
			}
		}
	}

	// Ten days after the closes. A new pull request of a target with a
	// closed one of ours creates no label: 3 writes.
	rep := w.both(at(t0.Add(10 * 24 * time.Hour)))
	check(rep, map[string]result{
		"acme/fresh":         {report.OutcomeDeclined, "", nFresh, 2},
		"acme/acked":         {report.OutcomeDeclined, "", nOld, 0},
		"acme/opt-in-edited": {report.OutcomeOpened, "", 0, 3},
		"acme/local-path":    {report.OutcomeOpened, "", 0, 3},
		"acme/bot-closed":    {report.OutcomeDeferred, "cooldown", nBot, 0},
		"acme/bot-thrice":    {report.OutcomeDeclined, "", nThird, 2},
		"acme/repropose":     {report.OutcomeOpened, "", 0, 4},
		"acme/forget":        {report.OutcomeOpened, "", 0, 4},
		"acme/merged":        {report.OutcomeOpened, "", 0, 3},
	})
	if k := targetOf(t, rep, "gh:acme/local-path").Key; k != keyOf("docs/guide.md", guideV1) {
		t.Errorf("local-path: key %s, want the guide's alone", k)
	}
	// Past the cooldown the bot-closed content is proposed again.
	rep = w.both(at(t0.Add(31 * 24 * time.Hour)))
	check(rep, map[string]result{
		"acme/bot-closed": {report.OutcomeOpened, "", 0, 3},
		"acme/acked":      {report.OutcomeDeclined, "", nOld, 0},
	})
}
