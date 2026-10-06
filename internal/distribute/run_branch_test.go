package distribute

import (
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestRunBranchSafety classifies sync branches people changed: touchmark
// rewrites a branch only when nothing of anyone else's is in it. Each case
// runs with our pull request open (paused: one edit shows the block) and
// without one (the branch is left alone).
func TestRunBranchSafety(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	// ours adds a target whose sync branch holds our commit of the base
	// pack, with our written pull request when open; it returns the target
	// and the pull request.
	ours := func(path string, open bool) (platform.Repo, int64) {
		r := w.optedIn(path, nil)
		w.syncCommit(r, branch, "", baseFiles...)
		var n int64
		if open {
			n = w.ownOn(r, branch, keyOf(baseFiles...), baseFiles...)
			w.written(r, n)
		}
		return r, n
	}
	// helper opens a person's pull request from head to base, lets act
	// change the branches through it, and closes it again.
	helper := func(r platform.Repo, head, base string, act func(n int64) (string, error)) {
		t.Helper()
		n := w.pr(r, platform.PR{Head: head, Base: base, Author: w.person, Title: "people's work"})
		if _, err := act(n); err != nil {
			t.Fatalf("%s: %v", r.Path, err)
		}
		if w.p.PR(r.ID, n).State == platform.Open {
			w.p.SetPRState(r.ID, n, platform.Closed, &w.person, time.Time{})
		}
		w.ok()
	}
	type result struct {
		outcome report.Outcome
		reason  string
		writes  int
		detail  string
	}
	expect := map[string]result{}
	for _, open := range []bool{true, false} {
		suffix := "-closed"
		if open {
			suffix = "-open"
		}
		edited := result{report.OutcomeBlocked, "edited", 0, ""}
		if open {
			edited.writes = 1 // the paused block
		}

		// A person's commit on top of ours.
		r, _ := ours("acme/on-top"+suffix, open)
		w.push(r, branch, w.person, "notes.md", "mine\n")
		expect[r.Path] = result{edited.outcome, edited.reason, edited.writes, "was added after touchmark's commit"}

		// Update branch: a clean merge of the default branch after it moved.
		r, _ = ours("acme/base-merge"+suffix, open)
		w.push(r, "main", w.person, "README2.md", "another readme\n")
		helper(r, branch, "main", func(n int64) (string, error) { return w.p.UpdateBranch(r.ID, n, w.person, time.Time{}) })
		if open {
			expect[r.Path] = result{report.OutcomeUnchanged, "", 0, ""}
		} else {
			expect[r.Path] = result{report.OutcomeOpened, "", 4, ""}
		}

		// A feature branch merged into the sync branch.
		r, _ = ours("acme/feature-merge"+suffix, open)
		w.push(r, "feature", w.person, "notes.md", "a feature\n")
		helper(r, "feature", branch, func(n int64) (string, error) {
			return w.p.MergePR(r.ID, n, fake.MergeCommit, w.person, time.Time{})
		})
		expect[r.Path] = result{edited.outcome, edited.reason, edited.writes, "brings commits that are not on the default branch"}

		// Our commit cherry-picked onto someone else's commit (a rebase onto
		// their branch keeps our message and pairs).
		r, _ = ours("acme/cherry-pick"+suffix, open)
		w.push(r, "foreign", w.person, "notes.md", "someone else's\n")
		helper(r, branch, "foreign", func(n int64) (string, error) { return w.p.RebaseBranch(r.ID, n, w.person, time.Time{}) })
		expect[r.Path] = result{edited.outcome, edited.reason, edited.writes, "sits on commits that are not on the default branch"}
	}
	// Another hub's commit on the branch: someone else's branch.
	other := w.optedIn("acme/other-hub", nil)
	w.commitOn(other, branch, "", otherFP, baseFiles...)
	expect[other.Path] = result{report.OutcomeBlocked, "branch-taken", 0, "is a touchmark commit of this hub"}
	// A commit under a previous fingerprint of this hub is ours.
	moved := w.optedIn("acme/moved", nil)
	w.commitOn(moved, branch, "", prevFP, baseFiles...)
	w.written(moved, w.ownOn(moved, branch, keyOf(baseFiles...), baseFiles...))
	expect[moved.Path] = result{report.OutcomeUnchanged, "", 0, ""}

	rep := w.both(nil)
	for path, r := range expect {
		tg := targetOf(t, rep, "gh:"+path)
		if tg.Outcome != r.outcome || tg.Reason != r.reason || tg.Writes != r.writes {
			t.Errorf("%s: %s:%s with %d writes, want %s:%s with %d (warnings %q)", path, tg.Outcome, tg.Reason, tg.Writes,
				r.outcome, r.reason, r.writes, tg.Warnings)
		}
		if r.detail != "" && !hasWarningLike(tg, "branch "+branch+": ", r.detail) {
			t.Errorf("%s: warnings %q lack %q", path, tg.Warnings, r.detail)
		}
	}
}
