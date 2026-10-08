package distribute

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// bitbucketLike gives the fake what the core reads of Bitbucket Cloud's
// capabilities (the driver's Probe): no labels, the marker as a Markdown
// reference definition, native drafts, a known closer and no permission of
// its own for CI files. The fake refuses any label on such a platform.
func bitbucketLike(c *platform.Caps) {
	c.NoLabels = true
	c.Marker = platform.MarkerInRefDef
	c.Draft = platform.DraftNative
	c.CloserKnown = true
	c.WorkflowPerm = false
	c.LabelsByID = false
}

// lastLine returns the last line of body.
func lastLine(body string) string { return body[strings.LastIndexByte(body, '\n')+1:] }

// isRefDef reports whether line is a v1 marker in the reference
// definition frame.
func isRefDef(line string) bool {
	return strings.HasPrefix(line, `[touchmark]: # "touchmark:v1 `) && strings.HasSuffix(line, `"`)
}

// noLabelCalls fails the test when the fake saw a label call.
func noLabelCalls(t *testing.T, w *exWorld) {
	t.Helper()
	for _, c := range w.p.Calls() {
		if strings.Contains(c, "Label") {
			t.Errorf("a label call on a platform without labels: %q", c)
		}
	}
}

// On a platform without labels the core opens a pull request without
// labels and records none in its marker, which it writes as a reference
// definition; a run with the same inputs, and one after hub.yml gains a
// label, write nothing; a content change and a close keep the frame and
// still ask for no label.
func TestExecuteNoLabelsLifecycle(t *testing.T) {
	w := newExWorld(t, exConfig{git: true, caps: bitbucketLike})
	if len(w.hub.PR.Labels) == 0 {
		t.Fatal("fixture: hub.yml sets no labels")
	}
	tg := w.target("acme/api", exOptIn, "version: 1\n", "README.md", "hello\n")

	wk := w.work(tg, nil)
	w.execute(wk)
	exWant(t, tg, report.OutcomeOpened, "", 1)
	pr := w.pr(tg, 1)
	if len(pr.Labels) != 0 {
		t.Errorf("labels %q", pr.Labels)
	}
	if !isRefDef(lastLine(pr.Body)) || strings.Contains(pr.Body, "<!-- touchmark:v1") {
		t.Errorf("the marker is not a reference definition:\n%s", pr.Body)
	}
	m := w.marker(pr.Body)
	if len(m.Data.LabelsSet) != 0 || m.Data.Body != decide.BodyHash(pr.Body) || m.Key != wk.Key {
		t.Errorf("marker %+v", m.Data)
	}
	noLabelCalls(t, w)

	// The same inputs, then a new label in hub.yml: nothing to write.
	for _, labels := range [][]string{w.hub.PR.Labels, append(slices.Clone(w.hub.PR.Labels), "platform-team")} {
		w.hub.PR.Labels = labels
		w.p.ResetCalls()
		w.execute(w.work(tg, nil))
		exWant(t, tg, report.OutcomeUnchanged, "", 1)
		if writes := w.p.Writes(); len(writes) > 0 {
			t.Errorf("labels %q: a run wrote %q", labels, writes)
		}
	}

	// The hub changes AGENTS.md: the edit keeps the frame, without labels.
	w.files["AGENTS.md"] = exAgentsV2
	w.p.ResetCalls()
	wk = w.work(tg, nil)
	w.execute(wk)
	exWant(t, tg, report.OutcomeUpdated, "content", 1)
	pr = w.pr(tg, 1)
	if !isRefDef(lastLine(pr.Body)) || w.marker(pr.Body).Key != wk.Key || len(pr.Labels) != 0 {
		t.Errorf("after the update: labels %q, body:\n%s", pr.Labels, pr.Body)
	}
	noLabelCalls(t, w)

	// The target takes the hub's files: the close writes the closed marker
	// in the same frame.
	w.push(tg, "main", "AGENTS.md", exAgentsV2, "docs/guide.md", exGuide)
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeClosed, "no-diff", 1)
	pr = w.pr(tg, 1)
	if m := w.marker(pr.Body); pr.State != platform.Closed || !isRefDef(lastLine(pr.Body)) || m.Data.Closed == nil || m.Data.Closed.By != "touchmark" {
		t.Errorf("after the close: %s, body:\n%s", pr.State, pr.Body)
	}
}

// A sweep close on a platform without labels and with the marker as a
// reference definition: the marker written with the close is in that frame,
// and the pull request's marker in the comment frame (one written before)
// is still found and replaced.
func TestExecuteRefDefSweepClose(t *testing.T) {
	w := newExWorld(t, exConfig{caps: bitbucketLike})
	tg := w.target("acme/gone", exOptIn, "version: 1\n")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	pr := w.pr(tg, n)
	m := w.marker(pr.Body)
	tg.res.Outcome, tg.res.Reason, tg.res.PR = report.OutcomeClosed, decide.ReasonTargetDropped, prRef(pr)
	wk := &Work{t: tg, Sweep: true, SweepPR: &decide.OwnPR{PR: pr, Marker: m},
		Decision: decide.TargetDecision{Outcome: decide.OutcomeClosed, Reason: decide.ReasonTargetDropped, PR: n}}
	w.execute(wk)
	exWant(t, tg, report.OutcomeClosed, decide.ReasonTargetDropped, n)
	pr = w.pr(tg, n)
	if pr.State != platform.Closed || !isRefDef(lastLine(pr.Body)) || strings.Contains(pr.Body, "<!-- touchmark:v1") {
		t.Fatalf("after the close: %s, body:\n%s", pr.State, pr.Body)
	}
	if m := w.marker(pr.Body); m.Data.Closed == nil || m.Data.Closed.Reason != decide.ReasonTargetDropped {
		t.Errorf("marker %+v", m.Data)
	}
	if !strings.HasPrefix(pr.Body, "Engineering assets from the hub.\n\n") || strings.Count(pr.Body, "\n") != 2 {
		t.Errorf("body:\n%s", pr.Body)
	}
	noLabelCalls(t, w)
}

// The comment after a third auto-close does not advise exempting labels a
// platform without labels never set.
func TestExecuteNoLabelsAutoDeclinedComment(t *testing.T) {
	w := newExWorld(t, exConfig{caps: bitbucketLike})
	stale := w.p.AddAccount("stale[bot]", platform.KindBot)
	tg := w.target("acme/docs", exOptIn, "version: 1\n")
	var last int64
	for range 3 {
		body := w.ownBody(w.missing(), func(d *marker.Data) { d.LabelsSet = nil })
		last = w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: body})
		w.p.SetPRState(tg.repo.ID, last, platform.Closed, &stale, time.Time{})
	}
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"ack", "comment"}) {
		t.Fatalf("steps %v", got)
	}
	w.execute(wk)
	cs := w.p.Comments(tg.repo.ID, last)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "exempt them in its settings") || strings.Contains(cs[0].Body, "label") {
		t.Errorf("comments %+v", cs)
	}
	noLabelCalls(t, w)
}
