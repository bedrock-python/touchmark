package distribute

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// TestRunOutcomes runs a plan and a dry run over targets with real
// branches: a target for every outcome phase C reaches, the two reports
// equal, nothing written.
func TestRunOutcomes(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)

	// Nothing there: a new branch and pull request.
	w.optedIn("acme/billing", nil)
	// Our commit and pull request, as touchmark wrote them.
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", baseFiles...)
	w.written(api, w.ownOn(api, branch, keyOf(baseFiles...), baseFiles...))
	// The same, but the description is not the one touchmark renders now.
	stale := w.optedIn("acme/stale-body", nil)
	w.syncCommit(stale, branch, "", baseFiles...)
	w.ownOn(stale, branch, keyOf(baseFiles...), baseFiles...)
	// The pull request carries base only; the target now gets python too.
	sdk := w.optedIn("acme/sdk", topics("python"))
	w.syncCommit(sdk, branch, "", baseFiles...)
	w.ownOn(sdk, branch, keyOf(baseFiles...), baseFiles...)
	// The default branch took the hub's files meanwhile.
	old := w.optedIn("acme/old", nil)
	w.syncCommit(old, branch, "", baseFiles...)
	w.ownOn(old, branch, keyOf(baseFiles...), baseFiles...)
	w.push(old, "main", w.person, baseFiles...)
	// Up to date without a pull request; outdated without one.
	w.optedIn("acme/current", nil, baseFiles...)
	w.optedIn("acme/outdated", nil, "AGENTS.md", agentsV1)
	// Someone else's pull request on the sync branch.
	cli := w.optedIn("acme/cli", nil)
	w.push(cli, branch, w.person, "notes.md", "mine\n")
	w.pr(cli, platform.PR{Head: branch, Author: w.person, Title: "my own work"})
	// Our author's pull request without a marker.
	bad := w.optedIn("acme/bad-marker", nil)
	w.syncCommit(bad, branch, "", baseFiles...)
	w.pr(bad, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: "no marker"})
	// A person closed our pull request: a decline, first seen.
	declined := w.optedIn("acme/declined", nil)
	w.syncCommit(declined, branch, "", baseFiles...)
	n := w.ownOn(declined, branch, keyOf(baseFiles...), baseFiles...)
	w.p.SetPRState(declined.ID, n, platform.Closed, &w.person, time.Time{})
	// The branch holds someone else's commit and no pull request.
	taken := w.optedIn("acme/taken", nil)
	w.push(taken, branch, w.person, "notes.md", "mine\n")
	// A file of a pack the target no longer gets.
	w.optedIn("acme/orphan", nil, "docs/python.md", pythonV1)
	// Classified before anything is read.
	w.repo("acme/web", nil, "README.md", "not opted in")
	w.optedIn("acme/archived", func(r *platform.Repo) { r.Archived = true })

	rep := w.both(nil)
	want(t, rep, "gh:acme/billing", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/api", report.OutcomeUnchanged, "", 1)
	want(t, rep, "gh:acme/stale-body", report.OutcomeUpdated, "body", 1)
	want(t, rep, "gh:acme/sdk", report.OutcomeUpdated, "content", 1)
	want(t, rep, "gh:acme/old", report.OutcomeClosed, "no-diff", 1)
	want(t, rep, "gh:acme/current", report.OutcomeUnchanged, "", 0)
	want(t, rep, "gh:acme/outdated", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/cli", report.OutcomeBlocked, "branch-in-use", 1)
	want(t, rep, "gh:acme/bad-marker", report.OutcomeBlocked, "marker-invalid", 1)
	dec := want(t, rep, "gh:acme/declined", report.OutcomeDeclined, "", 1)
	want(t, rep, "gh:acme/taken", report.OutcomeBlocked, "branch-taken", 0)
	orphan := want(t, rep, "gh:acme/orphan", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/web", report.OutcomeSkipped, "not-opted-in", 0)
	want(t, rep, "gh:acme/archived", report.OutcomeSkipped, "archived", 0)
	if len(rep.Targets) != 14 {
		t.Errorf("%d targets, want 14", len(rep.Targets))
	}
	if dec.PR == nil || dec.PR.State != "closed" {
		t.Errorf("declined: PR %+v", dec.PR)
	}
	if !slices.Equal(orphan.Orphaned, []string{"docs/python.md"}) {
		t.Errorf("orphaned %q", orphan.Orphaned)
	}
	if k := targetOf(t, rep, "gh:acme/billing").Key; k != keyOf(baseFiles...) {
		t.Errorf("billing: key %s", k)
	}
	if k := targetOf(t, rep, "gh:acme/old").Key; k != "" {
		t.Errorf("old: key %q, want none", k)
	}

	// Writes: a first pull request 4 on GitHub (push, pull request,
	// labels, the label), an edit 1, a push and an edit 2, a close 3
	// (close, comment, branch deletion), an ack 2 (ack, comment).
	writes := map[string]int{
		"gh:acme/billing": 4, "gh:acme/api": 0, "gh:acme/stale-body": 1, "gh:acme/sdk": 2, "gh:acme/old": 3,
		"gh:acme/current": 0, "gh:acme/outdated": 4, "gh:acme/cli": 0, "gh:acme/bad-marker": 0,
		"gh:acme/declined": 2, "gh:acme/taken": 0, "gh:acme/orphan": 4,
	}
	sum := 0
	for ref, n := range writes {
		if got := targetOf(t, rep, ref).Writes; got != n {
			t.Errorf("%s: %d writes, want %d", ref, got, n)
		}
		sum += n
	}
	if rep.Cost["gh"] != sum {
		t.Errorf("cost %v, want %d", rep.Cost, sum)
	}
	if rep.Sweep != (report.SweepInfo{Ran: true, Complete: true}) {
		t.Errorf("sweep %+v", rep.Sweep)
	}
	if code := rep.ExitCode(); code != 0 {
		t.Errorf("exit code %d", code)
	}
}

// TestRunUpdates: the reasons of updated besides content: the default branch
// changed one of our paths (rebase: the same content from another base), and
// hub.yml's pr.title changed while the title is still touchmark's (title:
// only the title is written).
func TestRunUpdates(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	rebase := w.optedIn("acme/rebase", nil)
	w.syncCommit(rebase, branch, "", baseFiles...)
	nRebase := w.openOwn(rebase)
	w.written(rebase, nRebase)
	w.push(rebase, "main", w.person, "AGENTS.md", agentsV1)
	title := w.optedIn("acme/title", nil)
	w.syncCommit(title, branch, "", baseFiles...)
	nTitle := w.openOwn(title)
	w.written(title, nTitle)
	// People retitled this one: their title stays.
	theirs := w.optedIn("acme/their-title", nil)
	w.syncCommit(theirs, branch, "", baseFiles...)
	nTheirs := w.openOwn(theirs)
	w.written(theirs, nTheirs)
	w.p.UpdatePR(theirs.ID, nTheirs, func(pr *platform.PR) { pr.Title = "Sync the shared files" })

	w.hubYML += "pr:\n  title: \"chore: sync the engineering assets\"\n"
	rep := w.both(nil)
	tg := want(t, rep, "gh:acme/rebase", report.OutcomeUpdated, "rebase", nRebase)
	if tg.Writes != 2 {
		t.Errorf("rebase: %d writes, want 2", tg.Writes)
	}
	tg = want(t, rep, "gh:acme/title", report.OutcomeUpdated, "title", nTitle)
	if tg.Writes != 1 {
		t.Errorf("title: %d writes, want 1", tg.Writes)
	}
	tg = want(t, rep, "gh:acme/their-title", report.OutcomeUnchanged, "", nTheirs)
	if tg.Writes != 0 {
		t.Errorf("their title: %d writes, want 0", tg.Writes)
	}
}

// TestRunFailures: a target's failure never stops the others, and its
// class decides the outcome: a git failure without a class is failed:git,
// a hub blob that does not hash to its id failed:integrity (I1), a rate
// limit deferred:rate-limit.
func TestRunFailures(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil)
	d := w.deps(ModePlan)
	d.Concurrency = 1
	w.p.FailNext("Fetch", &platform.Error{Op: "test", Class: platform.ClassInvalid, Status: http.StatusBadRequest, Err: errors.New("bad request")})
	rep := w.run(d, ModePlan)
	want(t, rep, "gh:acme/a", report.OutcomeFailed, "git", 0)
	want(t, rep, "gh:acme/b", report.OutcomeOpened, "", 0)
	if code := rep.ExitCode(); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}

	limited := &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests, Err: errors.New("slow down")}
	w.p.FailNext("Fetch", limited)
	rep = w.run(d, ModePlan)
	// One rate limit pauses the provider, and the fetch is made again.
	want(t, rep, "gh:acme/a", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/b", report.OutcomeOpened, "", 0)
	for range throttle.Strikes {
		w.p.FailNext("Fetch", limited)
	}
	rep = w.run(d, ModePlan)
	want(t, rep, "gh:acme/a", report.OutcomeDeferred, "rate-limit", 0)
	// Three in a row stop the provider: the next target waits too, without a
	// call.
	want(t, rep, "gh:acme/b", report.OutcomeDeferred, "rate-limit", 0)

	w.blobs[oid(guideV1)] = []byte("not the guide the manifest names\n")
	rep = w.run(w.deps(ModePlan), ModePlan)
	tg := want(t, rep, "gh:acme/a", report.OutcomeFailed, "integrity", 0)
	if !hasWarningLike(tg, "build the commit") {
		t.Errorf("warnings %q", tg.Warnings)
	}
}
