package distribute

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// olderHub is a hub commit older than hubCommit, the Touchmark-Hub-Commit
// of the commits syncCommit makes.
const olderHub = "0123456789abcdef0123456789abcdef01234567"

// Guard I8 on a target: a sync branch whose touchmark commit names a hub
// commit newer than the run's belongs to a newer run, and the target is
// skipped:superseded without a write; a hub commit the hub does not know, or
// an older one, blocks nothing. An older run once rolled the newer content
// back.
func TestRunTargetSuperseded(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", old...)
	n := w.ownOn(api, branch, keyOf(old...), old...)
	var asked atomic.Int32
	newer := func(_ context.Context, a, c string) (bool, error) {
		asked.Add(1)
		return a == olderHub && c == hubCommit, nil
	}
	older := func(d *Deps) { d.HubCommit, d.Write.HubIsAncestor = olderHub, newer }
	rep := w.both(older)
	tg := want(t, rep, "gh:acme/api", report.OutcomeSkipped, "superseded", 0)
	if tg.Writes != 0 || !hasWarningLike(tg, "superseded: branch touchmark/acme-eng carries touchmark's commit for hub commit "+short(hubCommit)) {
		t.Errorf("writes %d, warnings %q", tg.Writes, tg.Warnings)
	}
	w.run(func() Deps { d := w.deps(ModeDistribute); older(&d); return d }(), ModeDistribute)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("a superseded run wrote %q", writes)
	}
	if asked.Load() == 0 {
		t.Error("the hub's history was never asked")
	}
	// The run's own commit, and a hub commit it does not know, block
	// nothing: the pull request is updated as usual.
	rep = w.both(func(d *Deps) {
		d.Write.HubIsAncestor = func(context.Context, string, string) (bool, error) { return false, nil }
	})
	want(t, rep, "gh:acme/api", report.OutcomeUpdated, "content", n)
	// An error of the hub's history fails the target: it may be stale.
	rep = w.both(func(d *Deps) {
		d.HubCommit = olderHub
		d.Write.HubIsAncestor = func(context.Context, string, string) (bool, error) {
			return false, errors.New("git merge-base: broken")
		}
	})
	want(t, rep, "gh:acme/api", report.OutcomeFailed, "internal", 0)
}

// Guard I8 fails closed for what writes: a dry run or distribute in CI whose
// hub channel fails, or that has none, stops with ErrHeadUnchecked before
// reading a target; a plan and a local run only warn. Every channel error
// was once a warning, and distribute wrote.
func TestRunHeadUnchecked(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/x", nil)
	ci := hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "schedule", Visibility: "private"}
	failing := channel{err: errors.New("hub channel: read the tip of branch \"main\": 503")}
	for _, tc := range []struct {
		name string
		mode Mode
		ch   hubch.Channel
		ctx  hubch.Context
		stop bool
	}{
		{"dry run, channel fails", ModeDryRun, failing, ci, true},
		{"distribute, channel fails", ModeDistribute, failing, ci, true},
		{"distribute, no channel", ModeDistribute, nil, ci, true},
		{"plan, channel fails", ModePlan, failing, ci, false},
		{"local distribute, no channel", ModeDistribute, nil, hubch.Context{CI: hubch.Local}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := runDeps(w, tc.mode)
			d.HubContext, d.Channel = tc.ctx, tc.ch
			w.p.ResetCalls()
			rep, err := Run(t.Context(), d, tc.mode)
			switch {
			case tc.stop && (!errors.Is(err, ErrHeadUnchecked) || rep != nil):
				t.Fatalf("Run = %v, %v; want ErrHeadUnchecked", rep, err)
			case tc.stop:
				for _, c := range w.p.Calls() {
					if strings.HasPrefix(c, "ReadFile") || strings.HasPrefix(c, "Target") {
						t.Errorf("a stopped run called %q", c)
					}
				}
			case err != nil:
				// The local run reaches the git source runDeps never serves;
				// the guard itself let it through.
				if errors.Is(err, ErrHeadUnchecked) {
					t.Fatalf("Run: %v", err)
				}
			case !hasWarning(rep.Warnings, "hub head not checked"):
				t.Errorf("warnings %q", rep.Warnings)
			}
		})
	}
}

// Phase C keeps a quarter of the time to --deadline for phase F: once
// inspections pass three quarters of it, no target is inspected any more,
// and the targets inspected are written. Once, a phase C that
// outlasted the deadline left no time to write anything, run after run.
func TestRunDeadlineReserve(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil)
	start := time.Now()
	clock := &slowInspections{Writer: w.p.Writer(w.writer), start: start}
	d := w.deps(ModeDistribute)
	d.Concurrency = 1
	d.Now = clock.now
	d.Write.Deadline = start.Add(100 * time.Second)
	d.Providers[0].Writer = clock
	rep := w.run(d, ModeDistribute)
	want(t, rep, "gh:acme/a", report.OutcomeOpened, "", 1)
	want(t, rep, "gh:acme/b", report.OutcomeDeferred, "deadline", 0)
	if !hasWarning(rep.Warnings, "deferred:deadline: the run's deadline (--deadline) left 1 of 2 targets undone") {
		t.Errorf("warnings %q", rep.Warnings)
	}
}

// slowInspections is a writer whose inspections take long on the run's
// clock (now): the first target's listing of pull requests ends 80 s after
// the start, and a second target's opt-in file is read past 100 s.
type slowInspections struct {
	platform.Writer
	start           time.Time
	offset          atomic.Int64
	listed, readSeq atomic.Int32
}

func (s *slowInspections) now() time.Time { return s.start.Add(time.Duration(s.offset.Load())) }

func (s *slowInspections) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	if s.listed.Add(1) == 1 {
		s.offset.Store(int64(80 * time.Second))
	}
	return s.Writer.PRs(ctx, r, heads, authors)
}

func (s *slowInspections) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	if s.readSeq.Add(1) == 2 {
		s.offset.Store(int64(101 * time.Second))
	}
	return s.Writer.ReadFile(ctx, r, ref, path, max)
}
