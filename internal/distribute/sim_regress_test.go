package distribute

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// Regressions the adversarial tests found, each pinned on its own.

// A hub commit that changes nothing for a target (a pack it does not get,
// operations.yml, targets.yml) writes nothing to its open pull request:
// the description names the hub commit that decided the content, not the
// run's. A change of the content then names the new commit. Found by the
// lifecycle test: every hub commit rewrote every open pull request.
func TestHubCommitWithoutChangeWritesNothing(t *testing.T) {
	t.Parallel()
	w := newSimWorld(t, fake.GitHub, "api")
	tg := w.target("api")
	w.run(ModeDistribute, nil)
	first := w.hubCommit
	w.ship("docs/extra.md", 2)
	w.hubChanged()
	w.p.ResetCalls()
	rep := w.run(ModeDistribute, nil)
	if writes := w.p.Writes(); len(writes) > 0 || rep.Targets[0].Outcome != report.OutcomeUnchanged {
		t.Errorf("a hub commit that changes nothing for the target: %s, writes %q", outcomes(rep), writes)
	}
	body := w.p.PR(tg.repo.ID, 1).Body
	if !strings.Contains(body, first[:7]) || strings.Contains(body, w.hubCommit[:7]) {
		t.Errorf("the description names another hub commit than %s:\n%s", first[:7], body)
	}

	w.ship("AGENTS.md", 2)
	w.hubChanged()
	rep = w.run(ModeDistribute, nil)
	pr := w.p.PR(tg.repo.ID, 1)
	m, _ := marker.Find(pr.Body, []string{hubFP})
	if rep.Targets[0].Outcome != report.OutcomeUpdated || !strings.Contains(pr.Body, w.hubCommit[:7]) || m.Data.ContentCommit != w.hubCommit {
		t.Errorf("a content change: %s, content_commit %s, body:\n%s", outcomes(rep), m.Data.ContentCommit, pr.Body)
	}
}

// The I2 check of the adversarial tests sees the commits people push to a
// sync branch, and a commit that becomes unreachable fails it: a check that
// tracks nothing passes whatever a run drops. The author was once matched
// as a regular expression, whose "+" of the email matched no commit.
func TestSimTrackerSeesPeoplesCommits(t *testing.T) {
	t.Parallel()
	w := newSimWorld(t, fake.GitHub, "api")
	tg := w.target("api")
	w.run(ModeDistribute, nil)
	head, err := w.push(tg, "touchmark/acme-eng", map[string]string{"notes.md": simLocal("notes.md", 1)})
	if err != nil {
		t.Fatal(err)
	}
	var failures []string
	c := newSimChecker(w, func(format string, args ...any) { failures = append(failures, fmt.Sprintf(format, args...)) })
	c.observe()
	if !c.tracked["api"][head] {
		t.Fatalf("the checker does not track %s, the person's commit on the sync branch: %v", short(head), c.tracked["api"])
	}
	c.checkKept(tg)
	if len(failures) > 0 {
		t.Fatalf("a kept commit fails the check: %q", failures)
	}
	w.p.DeleteBranch(tg.repo.ID, "touchmark/acme-eng")
	w.ok()
	c.checkKept(tg)
	if len(failures) != 1 || !strings.Contains(failures[0], "I2: acme/api lost the commit "+short(head)) {
		t.Errorf("a lost commit: failures %q", failures)
	}
}

// A rebuild that stopped after its push, before the edit that ends it,
// leaves no recreate_for behind once the next run finishes it: kept, it
// would rebuild the branch again, without a tick, the day someone puts the
// old head back to restore the commits the rebuild dropped. Found by the
// crash matrix (recreate/2-EditPR/before/401).
func TestRecreateForClearedAfterCrash(t *testing.T) {
	t.Parallel()
	w := newSimWorld(t, fake.GitHub, "api")
	tg := w.target("api")
	w.run(ModeDistribute, nil)
	w.mustDo(w.push(tg, "touchmark/acme-eng", map[string]string{"notes.md": simLocal("notes.md", 1)}))
	w.run(ModeDistribute, nil)
	if !w.tick(tg, 1, prbody.ControlRecreate) {
		t.Fatal("no recreate control")
	}
	// The rebuild: the control consumed (write 0), the push (1), and the
	// edit that ends it (2), which fails.
	f := &simFaults{w: w, k: 2, err: simAuthErr}
	remove := gitx.TraceCommands(f.trace)
	rep := w.run(ModeDistribute, func(d *Deps) {
		d.Providers[0].Writer = &exWriter{Writer: w.p.Writer(w.writer), target: func(tw platform.TargetWriter) platform.TargetWriter {
			return exWrap(tw, f.api)
		}}
	})
	remove()
	if f.hit != "EditPR" || rep.Targets[0].Reason != "auth" {
		t.Fatalf("the failed run: %s (fault on %q)", outcomes(rep), f.hit)
	}
	rep = w.run(ModeDistribute, nil)
	m, _ := marker.Find(w.p.PR(tg.repo.ID, 1).Body, []string{hubFP})
	if m.Data.RecreateFor != nil {
		t.Errorf("after %s the marker keeps recreate_for %s", outcomes(rep), *m.Data.RecreateFor)
	}
}
