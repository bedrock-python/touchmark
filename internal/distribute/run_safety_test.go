package distribute

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// What touchmark writes and prints stays within bounds whatever the
// targets hold: no quick action or mention from people's text, no name of
// a non-public target of a public hub, no claim about a branch touchmark
// cannot read, and the same signing rule in the dry run and in distribute.

// A description touchmark writes back (an ack, a close, a consumed
// recreate) runs no GitLab quick action and mentions no one, whatever
// people put in it: the line comes out escaped. Once, a line
// hidden in an HTML block ran as the writer once marker.Strip dropped the
// line that opened the block.
func TestRunKeptBodyIsInert(t *testing.T) {
	t.Parallel()
	w := newSimWorld(t, fake.GitLab, "api")
	tg := w.target("api")
	w.run(ModeDistribute, nil)
	w.p.UpdatePR(tg.repo.ID, 1, func(pr *platform.PR) {
		pr.Body = "<!-- touchmark:note -->\n/label ~smuggled\n</p>\ncc @victim\n\n" + pr.Body
	})
	w.p.SetPRState(tg.repo.ID, 1, platform.Closed, &w.person, time.Time{})
	w.ok()
	rep := w.run(ModeDistribute, nil)
	if got := rep.Targets[0]; got.Outcome != report.OutcomeDeclined {
		t.Fatalf("%s:%s %q", got.Outcome, got.Reason, got.Warnings)
	}
	if v := w.p.Violations(); len(v) > 0 {
		t.Errorf("the writer ran a quick action: %q", v)
	}
	pr := w.p.PR(tg.repo.ID, 1)
	if !strings.Contains(pr.Body, `\/label ~smuggled`) || !strings.Contains(pr.Body, "cc @\u2060victim") {
		t.Errorf("people's lines are not inert:\n%s", pr.Body)
	}
	for _, l := range pr.Labels {
		if l == "smuggled" {
			t.Errorf("labels %q", pr.Labels)
		}
	}
}

// A dry run in a public hub names no non-public target anywhere: not in
// the warnings of a sweep close whose preflight the platform refused with
// a message that names the repository. The preflight's warning once
// printed it.
func TestRunHiddenPreflight(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.ctx = hubch.Context{CI: hubch.Local, Visibility: "public"}
	secret := w.repo("other/secret-payroll", func(r *platform.Repo) { r.Visibility = "private" }, "README.md", "x\n")
	w.openOwn(secret)
	w.p.Grant(secret.ID, w.writer, platform.Perms{Contents: true})
	w.optedIn("acme/api", nil)
	var stream bytes.Buffer
	d := w.deps(ModeDryRun)
	d.Write.Stream = &stream
	rep := w.run(d, ModeDryRun)
	blocked := 0
	for _, tg := range rep.Targets {
		if tg.Path == "" && tg.Outcome == report.OutcomeBlocked && strings.HasPrefix(tg.Reason, "permission:") {
			blocked++
			if len(tg.Warnings) > 0 {
				t.Errorf("a hidden target has warnings: %q", tg.Warnings)
			}
		}
	}
	if blocked != 1 {
		t.Errorf("%d hidden targets blocked by the preflight, want 1: %s", blocked, outcomes(rep))
	}
	var text, md, js bytes.Buffer
	for _, err := range []error{rep.WriteText(&text), rep.WriteMarkdown(&md), report.WriteJSON(&js, rep)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, out := range map[string]string{"text": text.String(), "markdown": md.String(), "json": js.String(), "stream": stream.String()} {
		if strings.Contains(out, "secret-payroll") {
			t.Errorf("the %s names the private repository:\n%s", name, out)
		}
	}
	if !json.Valid(js.Bytes()) {
		t.Error("the JSON report does not parse")
	}
}

// A paused pull request whose branch holds no commit of touchmark's (its
// commit is gone: people rewrote the branch) makes no claim about what the
// branch holds: no table, and all of D as what a rebuild would bring.
// The body once said the branch held D and a rebuild would bring
// nothing new.
func TestRunPausedForeignBody(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	api := w.optedIn("acme/api", nil)
	w.push(api, branch, w.person, "notes.md", "the team's notes\n")
	n := w.ownOn(api, branch, keyOf("AGENTS.md", agentsV1), "AGENTS.md", agentsV1)
	wk, res := w.inspectOne(w.deps(ModePlan), ModePlan, "acme/api")
	if wk == nil || res.Outcome != report.OutcomeBlocked || res.Reason != "edited" || !wk.Decision.Blocks.Paused {
		t.Fatalf("%s:%s, work %+v", res.Outcome, res.Reason, wk)
	}
	if !wk.Body.BranchUnknown || len(wk.Body.Changes) != 0 || len(wk.Body.Pending) != len(wk.D) {
		t.Errorf("body input: unknown %v, %d rows, %d pending of %d", wk.Body.BranchUnknown, len(wk.Body.Changes), len(wk.Body.Pending), len(wk.D))
	}
	body, err := wk.humanBody()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "cannot tell which of its changes the branch holds now") || strings.Contains(body, "would bring nothing new") ||
		strings.Contains(body, "The changes above are what the branch holds now") {
		t.Errorf("#%d's body:\n%s", n, body)
	}
}

// sign: always is refused alike by the dry run and by distribute when the
// platform reports an API commit (Caps.Commit.API) that execute does not
// make yet: the dry run says what distribute does. The dry run once let
// the push through and distribute blocked it.
func TestRunSignAlwaysWithAPICommit(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML = strings.Replace(defaultHubYML, "    writer: acme-write[bot]\n", "    writer: acme-write[bot]\n    sign: always\n", 1)
	caps := w.p.Caps()
	caps.Commit.API = true
	w.p.SetCaps(caps)
	w.optedIn("acme/x", nil)
	dry := w.run(w.deps(ModeDryRun), ModeDryRun)
	want(t, dry, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("wrote %q", writes)
	}
}
