package distribute

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The limits of phase F's writes: a cancelled run gives the write in flight
// its grace and no more, and a target's block of writes has its own budget.

// blockingEdits is a per-target writer whose edits wait until their
// context ends; started is closed when the first one starts.
type blockingEdits struct {
	platform.TargetWriter
	once    sync.Once
	started chan struct{}
	// ended is when the first edit's context ended.
	ended atomic.Int64
}

func (b *blockingEdits) EditPR(ctx context.Context, _ int64, _ platform.PREdit) (platform.PR, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	b.ended.CompareAndSwap(0, time.Now().UnixNano())
	return platform.PR{}, ctx.Err()
}

// declinedWork adds a target whose pull request a person closed, and
// returns it with its work: an ack, whose edit is the first write.
func (w *exWorld) declinedWork(path string) (*target, *Work) {
	w.t.Helper()
	tg := w.target(path, exOptIn, "version: 1\n")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})
	return tg, w.work(tg, nil)
}

func TestExecuteWriteLimits(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		w := newExWorld(t, exConfig{})
		tg, wk := w.declinedWork("acme/slow")
		blocked := &blockingEdits{started: make(chan struct{})}
		w.wrap().target = func(tw platform.TargetWriter) platform.TargetWriter { blocked.TargetWriter = tw; return blocked }
		w.ex.timeout, w.ex.grace = 50*time.Millisecond, time.Hour
		w.execute(wk)
		exWant(t, tg, report.OutcomeFailed, "transient", tg.res.PR.Number)
	})
	t.Run("grace", func(t *testing.T) {
		w := newExWorld(t, exConfig{})
		tg, wk := w.declinedWork("acme/stopped")
		blocked := &blockingEdits{started: make(chan struct{})}
		w.wrap().target = func(tw platform.TargetWriter) platform.TargetWriter { blocked.TargetWriter = tw; return blocked }
		w.ex.timeout, w.ex.grace = time.Hour, 50*time.Millisecond
		ctx, cancel := context.WithCancel(w.ctx)
		defer cancel()
		var cancelled atomic.Int64
		go func() {
			<-blocked.started
			cancelled.Store(time.Now().UnixNano())
			cancel() // SIGTERM while the edit is in flight
		}()
		w.ex.run(ctx, []*Work{wk})
		// The edit in flight lived its grace after the run was cancelled, and
		// not the target's whole budget.
		if d := time.Duration(blocked.ended.Load() - cancelled.Load()); d < 40*time.Millisecond || d > 20*time.Second {
			t.Errorf("the write in flight had %v after the run was cancelled, want its grace of 50ms", d)
		}
		exWant(t, tg, report.OutcomeDeferred, "interrupted", tg.res.PR.Number)
		if calls := w.p.Calls(); strings.Contains(strings.Join(calls, "\n"), "Comment") {
			t.Errorf("a write started after the run was cancelled: %q", calls)
		}
	})
}

// Every per-target write credential phase F mints is revoked after the
// target (threat T3 of docs/project/threat-model.md): each "Target <repo>"
// of the call log is followed by "Close <repo>".
func TestExecuteRevokesTargetWriters(t *testing.T) {
	w := newExWorld(t, exConfig{})
	var works []*Work
	for _, path := range []string{"acme/one", "acme/two"} {
		_, wk := w.declinedWork(path)
		works = append(works, wk)
	}
	w.p.ResetCalls()
	w.execute(works...)
	checkRevoked(t, w.p.Calls())
	if open := w.p.OpenTargetWriters(); len(open) > 0 {
		t.Errorf("per-target writers left open for %q", open)
	}
}

// checkRevoked checks a call log: every per-target writer minted ("Target
// <repo>") is closed ("Close <repo>") later.
func checkRevoked(t *testing.T, calls []string) {
	t.Helper()
	open := map[string]int{}
	minted := 0
	for _, c := range calls {
		switch f := strings.Fields(c); {
		case len(f) == 2 && f[0] == "Target":
			open[f[1]]++
			minted++
		case len(f) == 2 && f[0] == "Close":
			open[f[1]]--
		}
	}
	if minted == 0 {
		t.Error("no per-target writer was minted")
	}
	for repo, n := range open {
		if n != 0 {
			t.Errorf("%d per-target writers of %s were not closed (%q)", n, repo, calls)
		}
	}
}
