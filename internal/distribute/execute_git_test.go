package distribute

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// exHTTPErr is a fault of the fake's git server or API with an HTTP status.
func exHTTPErr(class platform.Class, status int) error {
	return &platform.Error{Op: "test", Class: class, Status: status, Err: errors.New(http.StatusText(status))}
}

// denyPushes makes the target's repository refuse every push with message,
// as a pre-receive hook of the platform would.
func (w *exWorld) denyPushes(tg *target, message string) {
	w.t.Helper()
	hook := "#!/bin/sh\necho '" + message + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(w.p.GitDir(tg.repo.ID), "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// Every status of a push, each on a target of its own: what goes on, what
// is retried after a reconciling read, and what ends the target.
func TestExecutePushStatuses(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	cases := []struct {
		name    string
		arrange func(tg *target)
		outcome report.Outcome
		reason  string
		sleeps  []time.Duration
		pushed  bool
	}{
		{name: "ok", outcome: report.OutcomeOpened, pushed: true},
		{name: "transient", arrange: func(*target) { w.p.FailNext("Push", exHTTPErr(platform.ClassTransient, http.StatusServiceUnavailable)) },
			outcome: report.OutcomeOpened, sleeps: []time.Duration{time.Second}, pushed: true},
		{name: "applied", arrange: func(*target) {
			w.p.FailNextApplied("Push", exHTTPErr(platform.ClassTransient, http.StatusBadGateway))
		}, outcome: report.OutcomeOpened, pushed: true},
		{name: "gave-up", arrange: func(*target) {
			for range 3 {
				w.p.FailNext("Push", exHTTPErr(platform.ClassTransient, http.StatusServiceUnavailable))
			}
		}, outcome: report.OutcomeFailed, reason: "transient", sleeps: []time.Duration{time.Second, 2 * time.Second}},
		{name: "bad-request", arrange: func(*target) { w.p.FailNext("Push", exHTTPErr(platform.ClassInvalid, http.StatusBadRequest)) },
			outcome: report.OutcomeFailed, reason: "git"},
		{name: "unauthorized", arrange: func(*target) { w.p.FailNext("Push", exHTTPErr(platform.ClassAuth, http.StatusUnauthorized)) },
			outcome: report.OutcomeFailed, reason: "auth"},
		{name: "forbidden", arrange: func(tg *target) {
			// The credential is fine, the identity lost its write access
			// after the token was minted.
			x := w.wrap()
			x.target = func(tw platform.TargetWriter) platform.TargetWriter {
				w.p.Grant(tg.repo.ID, w.writer, platform.Perms{PRs: true})
				return tw
			}
		}, outcome: report.OutcomeBlocked, reason: "permission:push"},
		{name: "ruleset", arrange: func(tg *target) { w.denyPushes(tg, "GH013: Repository rule violations found for refs/heads/"+exBranch) },
			outcome: report.OutcomeBlocked, reason: "rules:ruleset"},
		{name: "unsigned", arrange: func(tg *target) { w.denyPushes(tg, "GitLab: Commit must be signed with a GPG key") },
			outcome: report.OutcomeBlocked, reason: "cannot-sign"},
		// A push refused for its rate pauses the provider, then is reconciled
		// and made again.
		{name: "rate-limited", arrange: func(*target) { w.p.FailNext("Push", exHTTPErr(platform.ClassRateLimited, http.StatusTooManyRequests)) },
			outcome: report.OutcomeOpened, sleeps: []time.Duration{throttle.FirstPause}, pushed: true},
		// Three in a row put the provider out of budget.
		{name: "rate-limited-out", arrange: func(*target) {
			for range throttle.Strikes {
				w.p.FailNext("Push", exHTTPErr(platform.ClassRateLimited, http.StatusTooManyRequests))
			}
		}, outcome: report.OutcomeDeferred, reason: "rate-limit", sleeps: []time.Duration{throttle.FirstPause, 2 * throttle.FirstPause}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w.t = t
			w.prov.gate = w.gate(w.prov.cfg.ID)
			tg := w.target("acme/"+tc.name, exOptIn, "version: 1\n")
			wk := w.work(tg, nil)
			if tc.arrange != nil {
				tc.arrange(tg)
			}
			w.slept()
			w.execute(wk)
			w.prov.reader, w.prov.writer = w.p.Writer(w.writer), w.p.Writer(w.writer)
			var pr int64
			if tc.outcome == report.OutcomeOpened {
				pr = 1
			}
			exWant(t, tg, tc.outcome, tc.reason, pr)
			if got := w.slept(); !slices.Equal(got, tc.sleeps) {
				t.Errorf("waits %v, want %v", got, tc.sleeps)
			}
			if pushed := w.p.Branch(tg.repo.ID, exBranch) == wk.Built.Commit; pushed != tc.pushed {
				t.Errorf("pushed %v, want %v", pushed, tc.pushed)
			}
			if pushes := slices.Index(w.opKinds(tg), "push"); (pushes >= 0) != tc.pushed {
				t.Errorf("ops %v", w.opKinds(tg))
			}
		})
	}
	// After rate limits that put the provider out of budget the rest of its
	// queue waits.
	w.t = t
	w.prov.gate = w.gate(w.prov.cfg.ID)
	first, second := w.target("acme/limited-1", exOptIn, "version: 1\n"), w.target("acme/limited-2", exOptIn, "version: 1\n")
	works := []*Work{w.work(first, nil), w.work(second, nil)}
	for range throttle.Strikes {
		w.p.FailNext("Push", exHTTPErr(platform.ClassRateLimited, http.StatusTooManyRequests))
	}
	w.execute(works...)
	exWant(t, first, report.OutcomeDeferred, "rate-limit", 0)
	exWant(t, second, report.OutcomeDeferred, "rate-limit", 0)
	if w.p.Branch(second.repo.ID, exBranch) != "" {
		t.Error("the queue went on after a rate limit")
	}
}

// A push the platform refuses for want of the Workflows permission, and a
// Workflows permission the identity lacks at preflight.
func TestExecuteWorkflows(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	w.files[".github/workflows/lint.yml"] = exWorkflow

	// The decision asked for no Workflows permission: the platform refuses.
	tg := w.target("acme/refused", exOptIn, "version: 1\n")
	wk := w.work(tg, nil)
	for i := range wk.Decision.Steps {
		wk.Decision.Steps[i].NeedWorkflows = false
	}
	wk.NeedPerms.Workflows = false
	w.execute(wk)
	exWant(t, tg, report.OutcomeBlocked, "permission:workflows", 0)
	if w.p.Branch(tg.repo.ID, exBranch) != "" {
		t.Error("the refused push moved the branch")
	}

	// The identity may not change workflows: preflight stops the target.
	tg = w.target("acme/no-workflows", exOptIn, "version: 1\n")
	w.p.Grant(tg.repo.ID, w.writer, platform.Perms{Contents: true, PRs: true})
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeBlocked, "permission:workflows", 0)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("writes %q", writes)
	}

	// With the permission the push goes through.
	tg = w.target("acme/workflows", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeOpened, "", 1)
	if !strings.Contains(w.pr(tg, 1).Body, "### ⚠ Sensitive paths") {
		t.Errorf("body:\n%s", w.pr(tg, 1).Body)
	}
}

// The target moves after inspection: the recheck sees it, the target is
// inspected again once and the new decision carried out; without a
// re-inspection, or on a second move, it is failed:race.
func TestExecuteRecheckMoves(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/api", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	w.files["AGENTS.md"] = exAgentsV2

	// A person pushes after inspection: the run cannot inspect again.
	wk := w.work(tg, nil)
	w.push(tg, exBranch, "notes.md", "my notes\n")
	w.p.ResetCalls()
	w.execute(wk)
	exWant(t, tg, report.OutcomeFailed, "race", 1)
	if writes := w.p.Writes(); len(writes) > 0 || !exHasWarn(tg, "refs/heads/"+exBranch+" is at") {
		t.Errorf("writes %q, warnings %q", writes, tg.res.Warnings)
	}

	// With a re-inspection: the branch holds someone's commit now, so the
	// pull request is paused, with the recreate control in its body.
	reinspected := 0
	w.ex.reinspect = func(_ context.Context, old *Work) *Work {
		reinspected++
		return w.work(old.t, nil)
	}
	wk = w.work(tg, func(s *exState) {})
	head := w.push(tg, exBranch, "notes.md", "more notes\n")
	w.execute(wk)
	exWant(t, tg, report.OutcomeBlocked, decide.ReasonEdited, 1)
	pr := w.pr(tg, 1)
	if reinspected != 1 || pr.HeadSHA != head || !strings.Contains(pr.Body, prbody.ControlLine(prbody.ControlRecreate)) ||
		!exHasWarn(tg, "it was inspected again") {
		t.Errorf("reinspected %d, head %s (want %s), warnings %q, body:\n%s", reinspected, pr.HeadSHA, head, tg.res.Warnings, pr.Body)
	}

	// Moving again after the re-inspection is a race.
	w.ex.reinspect = func(_ context.Context, old *Work) *Work {
		nw := w.work(old.t, nil)
		w.push(tg, exBranch, "notes.md", "even more notes\n")
		return nw
	}
	w.push(tg, exBranch, "notes.md", "a change after inspection\n")
	wk = w.work(tg, nil)
	w.push(tg, exBranch, "notes.md", "and another\n")
	w.execute(wk)
	exWant(t, tg, report.OutcomeFailed, "race", 1)
	if !exHasWarn(tg, "changed again after touchmark inspected it anew") {
		t.Errorf("warnings %q", tg.res.Warnings)
	}
}

// A lease that fails at the push: the conflict re-inspects the target.
func TestExecuteStaleLease(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/api", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	w.files["AGENTS.md"] = exAgentsV2
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"push", "edit-pr"}) {
		t.Fatalf("steps %v", got)
	}
	// The person pushes right after the recheck read the pull requests.
	x := w.wrap()
	x.onPRs = func() { w.push(tg, exBranch, "notes.md", "my notes\n") }
	w.execute(wk)
	exWant(t, tg, report.OutcomeFailed, "race", 1)
	if !exHasWarn(tg, "the lease on") {
		t.Errorf("warnings %q", tg.res.Warnings)
	}
}

// CreatePR meets an open pull request from the sync branch: someone
// else's blocks the target; touchmark's own is re-inspected.
func TestExecuteCreateExists(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	x := w.wrap()
	var other int64
	tg := w.target("acme/foreign", exOptIn, "version: 1\n")
	x.target = func(tw platform.TargetWriter) platform.TargetWriter {
		return &exTargetWriter{TargetWriter: tw, before: func(method string) {
			if method == "CreatePR" {
				other = w.addPR(tg, platform.PR{Head: exBranch, Author: w.person, Title: "my work on the sync branch"})
			}
		}}
	}
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeBlocked, decide.ReasonBranchInUse, other)

	own := w.target("acme/own", exOptIn, "version: 1\n")
	wk := w.work(own, nil)
	var mine int64
	x.target = func(tw platform.TargetWriter) platform.TargetWriter {
		return &exTargetWriter{TargetWriter: tw, before: func(method string) {
			if method == "CreatePR" && mine == 0 {
				mine = w.addPR(own, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(wk.D, nil)})
			}
		}}
	}
	w.ex.reinspect = func(_ context.Context, old *Work) *Work { return w.work(old.t, nil) }
	w.execute(wk)
	exWant(t, own, report.OutcomeUpdated, "body", mine)
	if prs := w.p.PRList(own.repo.ID); len(prs) != 1 {
		t.Errorf("pull requests %+v", prs)
	}
	if m := w.marker(w.pr(own, mine).Body); m.Data.Body != decide.BodyHash(w.pr(own, mine).Body) {
		t.Errorf("marker %+v", m.Data)
	}
}

// A ticked recreate control: consumed first (unticked, recreate_for in the
// marker), then the branch is rebuilt on B and the body rewritten.
func TestExecuteConsumeRecreate(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/api", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	w.push(tg, exBranch, "notes.md", "my notes\n")
	w.execute(w.work(tg, nil)) // paused: the recreate control appears
	exWant(t, tg, report.OutcomeBlocked, decide.ReasonEdited, 1)
	w.p.UpdatePR(tg.repo.ID, 1, func(pr *platform.PR) {
		pr.Body = strings.Replace(pr.Body, "- [ ] <!-- touchmark:recreate -->", "- [x] <!-- touchmark:recreate -->", 1)
	})
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"consume-recreate", "push", "edit-pr"}) {
		t.Fatalf("steps %v", got)
	}
	var bodies []string
	x := w.wrap()
	x.target = func(tw platform.TargetWriter) platform.TargetWriter {
		return &exTargetWriter{TargetWriter: tw, answer: func(pr platform.PR) platform.PR {
			bodies = append(bodies, pr.Body)
			return pr
		}}
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeUpdated, decide.ReasonRecreate, 1)
	if len(bodies) != 2 {
		t.Fatalf("%d edits", len(bodies))
	}
	consumed := w.marker(bodies[0])
	if prbody.Ticked(bodies[0], prbody.ControlRecreate) || consumed.Data.RecreateFor == nil || *consumed.Data.RecreateFor != wk.Branch.Head {
		t.Errorf("the consumed body: recreate_for %v, body:\n%s", consumed.Data.RecreateFor, bodies[0])
	}
	pr := w.pr(tg, 1)
	if m := w.marker(pr.Body); m.Data.RecreateFor != nil || m.Key != wk.Key || pr.HeadSHA != wk.Built.Commit {
		t.Errorf("after the rebuild: head %s, marker %+v", pr.HeadSHA, m.Data)
	}
	if got := w.opKinds(tg); !slices.Equal(got[len(got)-3:], []string{"edit-pr", "push", "edit-pr"}) {
		t.Errorf("ops %v", got)
	}
}

// A branch moved across workflow changes of the base, with no own pull
// request open and no Workflows permission: deleted and pushed afresh
// (GitHub's Workflows permission is not needed then), then the new pull
// request.
func TestExecuteRecreateBranch(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/api", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	w.p.SetPRState(tg.repo.ID, 1, platform.Closed, &w.writer, time.Time{})
	old := w.p.Branch(tg.repo.ID, exBranch)
	w.files["AGENTS.md"] = exAgentsV2
	wk := w.work(tg, func(s *exState) { s.in.CanWorkflows, s.in.WorkflowsDiffer = false, true })
	if got := exStepKinds(wk); !slices.Equal(got, []string{"recreate-branch", "create-pr"}) {
		t.Fatalf("steps %v", got)
	}
	w.r.rep.Ops = nil
	w.execute(wk)
	exWant(t, tg, report.OutcomeOpened, "", 2)
	if got := w.opKinds(tg); !slices.Equal(got, []string{"delete-branch", "push", "create-pr"}) {
		t.Errorf("ops %v", got)
	}
	if ops := w.r.rep.Ops; ops[0].Before != old || ops[1].Before != "" || ops[1].After != wk.Built.Commit {
		t.Errorf("ops %+v", ops)
	}
	if w.p.Branch(tg.repo.ID, exBranch) != wk.Built.Commit {
		t.Error("the branch was not pushed afresh")
	}
}

// Reading the pull request back: a platform slow to show the new head is
// read again (1 s, 2 s, 4 s); one that never shows it gets the warning
// "eventual".
func TestExecuteVerify(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	x := w.wrap()
	stale := func(pr platform.PR) platform.PR { pr.HeadSHA = ""; return pr }
	tg := w.target("acme/slow", exOptIn, "version: 1\n")
	x.target = func(tw platform.TargetWriter) platform.TargetWriter {
		return &exTargetWriter{TargetWriter: tw, answer: stale, before: func(method string) {
			if method == "CreatePR" {
				w.p.FailNext("PRs", exHTTPErr(platform.ClassTransient, http.StatusBadGateway))
			}
		}}
	}
	w.slept()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeOpened, "", 1)
	if got := w.slept(); !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second}) || exHasWarn(tg, "eventual") {
		t.Errorf("waits %v, warnings %q", got, tg.res.Warnings)
	}

	tg = w.target("acme/never", exOptIn, "version: 1\n")
	wk := w.work(tg, nil)
	x.target = func(tw platform.TargetWriter) platform.TargetWriter {
		return &exTargetWriter{TargetWriter: tw, answer: stale}
	}
	x.prs = func(prs []platform.PR) []platform.PR {
		for i := range prs {
			prs[i] = stale(prs[i])
		}
		return prs
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeOpened, "", 1)
	if got := w.slept(); !slices.Equal(got, verifyDelays) || !exHasWarn(tg, "eventual: #1 does not show") {
		t.Errorf("waits %v, warnings %q", got, tg.res.Warnings)
	}
}

// An edit whose answer was lost is read back before it is tried again; a
// comment is never tried twice.
func TestExecuteLostAnswers(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	applied := w.target("acme/applied", exOptIn, "version: 1\n")
	retried := w.target("acme/retried", exOptIn, "version: 1\n")
	w.execute(w.work(applied, nil), w.work(retried, nil))
	w.files["AGENTS.md"] = exAgentsV2

	w.p.FailNextApplied("EditPR", exHTTPErr(platform.ClassTransient, http.StatusBadGateway))
	w.slept()
	w.p.ResetCalls()
	w.execute(w.work(applied, nil))
	exWant(t, applied, report.OutcomeUpdated, "content", 1)
	if got := w.slept(); len(got) > 0 || strings.Count(strings.Join(w.p.Writes(), "\n"), "EditPR") != 1 {
		t.Errorf("waits %v, writes %q", got, w.p.Writes())
	}

	w.p.FailNext("EditPR", exHTTPErr(platform.ClassTransient, http.StatusServiceUnavailable))
	w.p.ResetCalls()
	w.execute(w.work(retried, nil))
	exWant(t, retried, report.OutcomeUpdated, "content", 1)
	if got := w.slept(); !slices.Equal(got, []time.Duration{time.Second}) || strings.Count(strings.Join(w.p.Writes(), "\n"), "EditPR") != 2 {
		t.Errorf("waits %v, writes %q", got, w.p.Writes())
	}

	// The close's comment fails: a warning, and the branch is still
	// deleted.
	w.push(retried, "main", "AGENTS.md", exAgentsV2, "docs/guide.md", exGuide)
	w.p.FailNext("Comment", exHTTPErr(platform.ClassTransient, http.StatusBadGateway))
	w.execute(w.work(retried, nil))
	exWant(t, retried, report.OutcomeClosed, decide.ReasonNoDiff, 1)
	if !exHasWarn(retried, "the comment on #1 may not have been posted") || w.p.Branch(retried.repo.ID, exBranch) != "" ||
		len(w.p.Comments(retried.repo.ID, 1)) != 0 {
		t.Errorf("warnings %q, branch %q", retried.res.Warnings, w.p.Branch(retried.repo.ID, exBranch))
	}
}

// A close whose branch deletion the platform refuses keeps its outcome: the
// pull request is closed with its comment, and the branch left in place is
// a warning (a secondary write).
func TestExecuteDeletionRefused(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	tg := w.target("acme/kept", exOptIn, "version: 1\n")
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeOpened, "", 1)
	w.push(tg, "main", "AGENTS.md", exAgentsV1, "docs/guide.md", exGuide)
	head := w.p.Branch(tg.repo.ID, exBranch)
	w.denyPushes(tg, "GH013: Repository rule violations found for refs/heads/"+exBranch)
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeClosed, decide.ReasonNoDiff, 1)
	if pr := w.pr(tg, 1); pr.State != platform.Closed || len(w.p.Comments(tg.repo.ID, 1)) != 1 {
		t.Errorf("pull request %s, comments %d", pr.State, len(w.p.Comments(tg.repo.ID, 1)))
	}
	if got := w.p.Branch(tg.repo.ID, exBranch); got != head || !exHasWarn(tg, "branch "+exBranch+" is left in place") {
		t.Errorf("branch %q (was %q), warnings %q", got, head, tg.res.Warnings)
	}
}

// A secret in the commit message stops the target before its first write;
// a provider that signs every commit and has no key stops it too.
func TestExecuteRefusesBeforeWriting(t *testing.T) {
	w := newExWorld(t, exConfig{git: true, hubYML: exHubYML + "    sign: always\n"})
	tg := w.target("acme/unsigned", exOptIn, "version: 1\n")
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeBlocked, "cannot-sign", 0)
	if writes := w.p.Writes(); len(writes) > 0 || slices.ContainsFunc(w.p.Calls(), func(c string) bool { return strings.HasPrefix(c, "Target") }) {
		t.Errorf("calls %q", w.p.Calls())
	}

	w = newExWorld(t, exConfig{git: true})
	const secret = "ghp_0123456789abcdefghij"
	w.reg.Add(secret)
	w.hub.Commit.Message = "chore: sync engineering assets (" + secret + ")"
	tg = w.target("acme/leak", exOptIn, "version: 1\n")
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeFailed, "secret-exposure", 0)
	if writes := w.p.Writes(); len(writes) > 0 || !exHasWarn(tg, "the commit message holds a secret") {
		t.Errorf("writes %q, warnings %q", writes, tg.res.Warnings)
	}
}

// Every flavor gets bodies and comments it takes as they are: no GitLab
// quick action runs, no violation.
func TestExecuteFlavors(t *testing.T) {
	for _, flavor := range []fake.Flavor{fake.GitLab, fake.Gitea, fake.Forgejo} {
		t.Run(string(flavor), func(t *testing.T) {
			w := newExWorld(t, exConfig{git: true, flavor: flavor})
			tg := w.target("acme/api", exOptIn, "version: 1\n")
			w.execute(w.work(tg, nil))
			exWant(t, tg, report.OutcomeOpened, "", 1)
			w.push(tg, "main", "AGENTS.md", exAgentsV1, "docs/guide.md", exGuide)
			w.execute(w.work(tg, nil))
			exWant(t, tg, report.OutcomeClosed, decide.ReasonNoDiff, 1)
			if pr := w.pr(tg, 1); pr.State != platform.Closed || len(w.p.Comments(tg.repo.ID, 1)) != 1 {
				t.Errorf("pull request %s, comments %d", pr.State, len(w.p.Comments(tg.repo.ID, 1)))
			}
		})
	}
}
