package distribute

import (
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// A target's life on the fake in git mode: a new pull request, a second
// run that writes nothing (I7), a hub change that updates it, and a close
// once the target holds the hub's files.
func TestExecuteLifecycle(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/api", exOptIn, "version: 1\n", "README.md", "hello\n")

	// Open: push the commit of D on B, then the pull request.
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"push", "create-pr"}) {
		t.Fatalf("steps %v", got)
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeOpened, "", 1)
	if head := w.p.Branch(tg.repo.ID, exBranch); head != wk.Built.Commit {
		t.Errorf("the sync branch is at %s, want the built commit %s", head, wk.Built.Commit)
	}
	pr := w.pr(tg, 1)
	m := w.marker(pr.Body)
	if m.Key != wk.Key || m.Data.TitleSet != exTitle || !slices.Equal(m.Data.LabelsSet, []string{exLabel}) ||
		m.Data.Body != decide.BodyHash(pr.Body) || !m.Data.ChangesComplete || len(m.Data.Changes) != 2 ||
		m.Data.ContentCommit != exHubCommit || m.Data.Base != wk.B || m.Data.OptIn != wk.OptInHash || m.Data.Engine != "test" {
		t.Errorf("marker %+v, key %s", m.Data, m.Key)
	}
	if pr.Title != exTitle || !slices.Equal(pr.Labels, []string{exLabel}) || pr.Base != "main" || pr.HeadSHA != wk.Built.Commit {
		t.Errorf("pull request %+v", pr)
	}
	if !strings.Contains(pr.Body, "`AGENTS.md`") || !strings.Contains(pr.Body, "`acme-eng`") {
		t.Errorf("body:\n%s", pr.Body)
	}
	if got := w.opKinds(tg); !slices.Equal(got, []string{"push", "create-pr"}) {
		t.Errorf("ops %v", got)
	}
	// The HTTP writes: the push, then GitHub's pull request, its label created
	// at first use and the call that puts it on; Cost is the estimate (one per
	// step here) corrected to them.
	if tg.res.Writes != 4 || w.r.rep.Cost["gh"] != 2 {
		t.Errorf("writes %d, cost %v", tg.res.Writes, w.r.rep.Cost)
	}
	ops := w.r.rep.Ops
	if ops[0].Account != "acme-write[bot]" || ops[0].Target != "gh:acme/api" || ops[0].Before != "" || ops[0].After != wk.Built.Commit ||
		ops[1].PR != 1 || ops[1].After != wk.Built.Commit || !ops[0].Time.Before(ops[1].Time) {
		t.Errorf("ops %+v", ops)
	}
	if l, ok := w.streamed()["acme/api"]; !ok || l.Target.Outcome != report.OutcomeOpened || len(l.Ops) != 2 {
		t.Errorf("stream line %+v", l)
	}

	// The same inputs again: nothing to write, not even a token.
	w.p.ResetCalls()
	w.r.rep.Ops = nil
	wk = w.work(tg, nil)
	w.execute(wk)
	exWant(t, tg, report.OutcomeUnchanged, "", 1)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("a second run wrote: %q", writes)
	}
	if slices.ContainsFunc(w.p.Calls(), func(c string) bool { return strings.HasPrefix(c, "Target") }) {
		t.Errorf("a run without writes minted a token: %q", w.p.Calls())
	}

	// The hub changes AGENTS.md: the branch is rebuilt on B and the body
	// rewritten with the new key.
	w.files["AGENTS.md"] = exAgentsV2
	wk = w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"push", "edit-pr"}) {
		t.Fatalf("steps %v", got)
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeUpdated, "content", 1)
	pr = w.pr(tg, 1)
	if m := w.marker(pr.Body); m.Key != wk.Key || pr.HeadSHA != wk.Built.Commit || m.Data.Body != decide.BodyHash(pr.Body) {
		t.Errorf("after the update: head %s, marker key %s, want %s", pr.HeadSHA, m.Key, wk.Key)
	}

	// The target takes the hub's files on its default branch: the pull
	// request is closed (no-diff), with a comment, and its branch deleted.
	head := w.p.Branch(tg.repo.ID, exBranch)
	w.push(tg, "main", "AGENTS.md", exAgentsV2, "docs/guide.md", exGuide)
	wk = w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"close-pr", "comment", "delete-branch"}) {
		t.Fatalf("steps %v", got)
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeClosed, "no-diff", 1)
	pr = w.pr(tg, 1)
	if m := w.marker(pr.Body); pr.State != platform.Closed || m.Data.Closed == nil || m.Data.Closed.By != "touchmark" || m.Data.Closed.Reason != "no-diff" {
		t.Errorf("after the close: %s, marker %+v", pr.State, m.Data.Closed)
	}
	if cs := w.p.Comments(tg.repo.ID, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "nothing left to sync") {
		t.Errorf("comments %+v", cs)
	}
	if w.p.Branch(tg.repo.ID, exBranch) != "" {
		t.Error("the branch of the closed pull request is still there")
	}
	var del gitxOp
	for _, op := range w.r.rep.Ops {
		if op.Kind == "delete-branch" {
			del = gitxOp{op.Before, op.After}
		}
	}
	if del.before != head {
		t.Errorf("the deletion's lease %s, want %s", del.before, head)
	}
}

// gitxOp is the before and after of a journaled push.
type gitxOp struct{ before, after string }

// exStepKinds returns the names of a work's steps.
func exStepKinds(wk *Work) []string {
	var out []string
	for _, s := range wk.Decision.Steps {
		out = append(out, s.Kind.String())
	}
	return out
}

// An unchanged pull request whose title hub.yml changed, while the title is
// still the one touchmark set: one edit, reported as updated:title.
func TestExecuteTitleOnly(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/api", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	w.hub.PR.Title = "chore: engineering assets"
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeUpdated, "title", 1)
	pr := w.pr(tg, 1)
	if m := w.marker(pr.Body); pr.Title != "chore: engineering assets" || m.Data.TitleSet != "chore: engineering assets" {
		t.Errorf("title %q, title_set %q", pr.Title, m.Data.TitleSet)
	}
	if writes := w.p.Writes(); !slices.Equal(writes, []string{"EditPR acme/api #1"}) {
		t.Errorf("writes %q", writes)
	}
	// A person's title stays theirs.
	w.p.UpdatePR(tg.repo.ID, 1, func(pr *platform.PR) { pr.Title = "Sync assets (please review)" })
	w.hub.PR.Title = "chore: sync"
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeUnchanged, "", 1)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("writes %q", writes)
	}
}

// The same with drafts on a platform that marks them with a title prefix
// (GitLab's "Draft: "): touchmark's own draft gets the new pr.title and
// stays a draft. Once, the prefixed title never equalled the
// marker's plain title_set, so no draft ever got a new pr.title.
func TestExecuteTitleOnlyDraft(t *testing.T) {
	w := newExWorld(t, exConfig{git: true, flavor: fake.GitLab})
	w.hub.PR.Draft = true
	tg := w.target("acme/api", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	if pr := w.pr(tg, 1); !pr.Draft || pr.Title != "Draft: "+w.hub.PR.Title {
		t.Fatalf("the first run opened %q, draft %v", pr.Title, pr.Draft)
	}
	w.hub.PR.Title = "chore: engineering assets"
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeUpdated, "title", 1)
	pr := w.pr(tg, 1)
	if m := w.marker(pr.Body); pr.Title != "Draft: chore: engineering assets" || !pr.Draft || m.Data.TitleSet != "chore: engineering assets" {
		t.Errorf("title %q, draft %v, title_set %q", pr.Title, pr.Draft, m.Data.TitleSet)
	}
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeUnchanged, "", 1)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("a second run wrote %q", writes)
	}
}

// keep the imports used while the file grows.
var (
	_ = gitx.PushOK
	_ = fake.GitHub
	_ = prbody.ControlRecreate
)
