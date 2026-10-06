package distribute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// The throttle in the run: the platform's limits with hub.yml's overrides,
// pacing and blocks of writes, the writes counted as HTTP writes, the
// deadline, and the wait for a write the platform is not ready for. These
// tests run on the fake in memory mode, on a fake clock.

// TestLimitsOf: providers[].limits override the platform's defaults field
// by field; the targets inspected at once never exceed the run's
// concurrency.
func TestLimitsOf(t *testing.T) {
	caps := platform.Limits{Reads: 8, GitReads: 4, ReadsPerMinute: 600, WritesPerMinute: 60, WritesPerHour: 450,
		CommentsPerMinute: 50, MinInterval: time.Second}
	got := limitsOf(caps, config.ProviderLimits{}, 0)
	want := throttle.Limits{Inspections: 8, GitReads: 4, ReadsPerMinute: 600, WritesPerMinute: 60, WritesPerHour: 450,
		CommentsPerMinute: 50, MinInterval: time.Second}
	if got != want {
		t.Errorf("defaults: %+v", got)
	}
	got = limitsOf(caps, config.ProviderLimits{Reads: 2, GitReads: 1, ReadsPerMinute: 100, WritesPerMinute: 30, WritesPerHour: 300,
		CommentsPerMinute: 5, MinInterval: "250ms"}, 4)
	want = throttle.Limits{Inspections: 2, GitReads: 1, ReadsPerMinute: 100, WritesPerMinute: 30, WritesPerHour: 300,
		CommentsPerMinute: 5, MinInterval: 250 * time.Millisecond}
	if got != want {
		t.Errorf("overrides: %+v", got)
	}
	if got := limitsOf(caps, config.ProviderLimits{}, 3); got.Inspections != 3 {
		t.Errorf("concurrency 3: %d inspections at once", got.Inspections)
	}
	if got := limitsOf(platform.Limits{}, config.ProviderLimits{}, 0); got.Inspections != defaultConcurrency {
		t.Errorf("no limits: %d inspections at once", got.Inspections)
	}
}

// timedWrites records when the fake's per-target writers wrote, on the
// world's fake clock: the clock moves only when the throttle sleeps, so the
// time right after a write is when the throttle let it through.
type timedWrites struct {
	mu    sync.Mutex
	clock *fakeClock
	times []time.Time
	by    []string
}

func (tw *timedWrites) wrap(inner platform.TargetWriter) platform.TargetWriter {
	return &timedTarget{TargetWriter: inner, log: tw}
}

func (tw *timedWrites) record(what string) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.times = append(tw.times, tw.clock.Now())
	tw.by = append(tw.by, what)
}

type timedTarget struct {
	platform.TargetWriter
	log *timedWrites
}

func (x *timedTarget) EditPR(ctx context.Context, n int64, e platform.PREdit) (platform.PR, error) {
	pr, err := x.TargetWriter.EditPR(ctx, n, e)
	if err == nil {
		x.log.record(fmt.Sprintf("edit #%d", n))
	}
	return pr, err
}

func (x *timedTarget) Comment(ctx context.Context, n int64, body string) error {
	err := x.TargetWriter.Comment(ctx, n, body)
	if err == nil {
		x.log.record(fmt.Sprintf("comment #%d", n))
	}
	return err
}

// declined adds n targets whose pull request a person closed: each work is
// an ack and a comment, two writes.
func (w *exWorld) declined(n int) []*Work {
	w.t.Helper()
	var works []*Work
	for i := range n {
		_, wk := w.declinedWork(fmt.Sprintf("acme/d-%04d", i))
		works = append(works, wk)
	}
	return works
}

// paced puts a Gate with limits on the world's provider, on clock, with
// the run's deadline (zero for none), and runs the executor on clock.
func (w *exWorld) paced(clock *fakeClock, l throttle.Limits, deadline time.Time) {
	w.prov.gate = throttle.New(throttle.Options{Name: w.prov.cfg.ID, Limits: l, Deadline: deadline,
		Clock: throttle.Clock{Now: clock.Now, Sleep: clock.Sleep}})
	w.r.d.Write.Deadline = deadline
	w.ex.now, w.ex.sleep = clock.Now, clock.Sleep
}

// The writes of one target go as a block: a block starts once the minute
// leaves room for it, so it never waits in the middle, and every write keeps
// the minimum interval.
func TestThrottleBlocks(t *testing.T) {
	w := newExWorld(t, exConfig{})
	works := w.declined(4)
	clock := newFakeClock(exEpoch)
	w.paced(clock, throttle.Limits{WritesPerMinute: 3, MinInterval: time.Second}, time.Time{})
	log := &timedWrites{clock: clock}
	w.wrap().target = log.wrap
	w.execute(works...)
	for _, wk := range works {
		exWant(t, wk.t, report.OutcomeDeclined, "", wk.Decision.PR)
		if wk.t.res.Writes != 2 {
			t.Errorf("%s: %d writes", wk.t.repo.Path, wk.t.res.Writes)
		}
	}
	if len(log.times) != 8 {
		t.Fatalf("writes %q", log.by)
	}
	for i := 1; i < len(log.times); i++ {
		if gap := log.times[i].Sub(log.times[i-1]); gap < time.Second {
			t.Errorf("writes %d and %d are %v apart", i-1, i, gap)
		}
	}
	if got := maxWrites(log.times, time.Minute); got > 3 {
		t.Errorf("%d writes in a minute, the limit is 3", got)
	}
	for i := 0; i < len(log.times); i += 2 {
		if gap := log.times[i+1].Sub(log.times[i]); gap != time.Second {
			t.Errorf("the block of %s and %s waited %v in the middle", log.by[i], log.by[i+1], gap)
		}
	}
}

// maxWrites returns the most times in any window of span.
func maxWrites(times []time.Time, span time.Duration) int {
	sorted := slices.Clone(times)
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })
	most := 0
	for i := range sorted {
		n := 0
		for j := i; j < len(sorted) && sorted[j].Sub(sorted[i]) < span; j++ {
			n++
		}
		most = max(most, n)
	}
	return most
}

// The writes of a target count as the HTTP writes its block metered, not its
// operations: Gitea closes a pull request with two PATCHes (the state first,
// then the body), one op.
func TestThrottleCountsHTTPWrites(t *testing.T) {
	for _, tc := range []struct {
		flavor fake.Flavor
		writes int
	}{
		{fake.GitHub, 2},
		{fake.Gitea, 3},
	} {
		t.Run(string(tc.flavor), func(t *testing.T) {
			w := newExWorld(t, exConfig{flavor: tc.flavor})
			tg := w.target("acme/current", exOptIn, "version: 1\n", "AGENTS.md", exAgentsV1, "docs/guide.md", exGuide)
			n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
			wk := w.work(tg, nil)
			if wk.Decision.Outcome != decide.OutcomeClosed {
				t.Fatalf("decision %s:%s %v", wk.Decision.Outcome, wk.Decision.Reason, exStepKinds(wk))
			}
			w.execute(wk)
			exWant(t, tg, report.OutcomeClosed, decide.ReasonNoDiff, n)
			if got := w.opKinds(tg); !slices.Equal(got, []string{"edit-pr", "comment"}) || tg.res.Writes != tc.writes {
				t.Errorf("ops %v, %d writes, want %d", got, tg.res.Writes, tc.writes)
			}
		})
	}
}

// notYet is the error of a CreatePR the platform is not ready for, as
// GitLab's driver returns it when the push of the head branch is not
// registered yet.
var notYet = &platform.Error{Op: "create pull request", Class: platform.ClassTransient, Status: http.StatusBadRequest,
	RetryAfter: 500 * time.Millisecond, Err: fmt.Errorf("GitLab has not registered the push yet: %w", platform.ErrNotYet)}

// createOnly returns the work of a target whose sync branch holds touchmark's
// commit of D already and has no pull request: the work only opens one
// (a branch with our content and no PR gets only the PR).
func (w *exWorld) createOnly(path string) (*target, *Work) {
	w.t.Helper()
	tg := w.target(path, exOptIn, "version: 1\n")
	wk := w.work(tg, func(st *exState) {
		in := &st.in
		head := "4b1d9e0c4b1d9e0c4b1d9e0c4b1d9e0c4b1d9e0c"
		in.Branch = decide.Branch{Name: exBranch, Head: head, State: decide.BranchRewritable, Hc: head, C: in.D, CKey: in.Key,
			E: in.B, HcIsHead: true, HcParent: in.B}
	})
	if got := exStepKinds(wk); !slices.Equal(got, []string{"create-pr"}) {
		w.t.Fatalf("steps %v, want only the pull request", got)
	}
	return tg, wk
}

// A write the platform is not ready for (platform.ErrNotYet: GitLab before
// it registered a push) is sent again after growing pauses, for 30 s at
// most, without reading the platform back: it applied nothing (drivers never
// wait themselves). Any longer, the target fails transiently.
func TestThrottleNotYet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refuse  int
		outcome report.Outcome
		reason  string
		calls   int
		sleeps  []time.Duration
	}{
		{"registered after two refusals", 2, report.OutcomeOpened, "", 3,
			[]time.Duration{500 * time.Millisecond, time.Second}},
		{"never registered", 100, report.OutcomeFailed, "transient", 9,
			[]time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 3 * time.Second,
				5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newExWorld(t, exConfig{})
			tg, wk := w.createOnly("acme/new")
			transientTimes(w.p, "CreatePR", notYet, tc.refuse)
			w.slept()
			w.p.ResetCalls()
			w.execute(wk)
			pr := int64(0)
			if tc.outcome == report.OutcomeOpened {
				pr = 1
			}
			exWant(t, tg, tc.outcome, tc.reason, pr)
			// The waits of the refused creates come first (the read-back of
			// the new pull request follows them).
			if got := w.slept(); len(got) < len(tc.sleeps) || !slices.Equal(got[:len(tc.sleeps)], tc.sleeps) {
				t.Errorf("waits %v, want %v first", got, tc.sleeps)
			}
			if n := countCalls(w.p, "CreatePR"); n != tc.calls {
				t.Errorf("%d CreatePR calls, want %d", n, tc.calls)
			}
			calls := w.p.Calls()
			last := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "CreatePR") })
			for i := len(calls) - 1; i >= 0; i-- {
				if strings.HasPrefix(calls[i], "CreatePR") {
					last = i
					break
				}
			}
			listings := 0
			for _, c := range calls[:last] {
				if strings.HasPrefix(c, "PRs") {
					listings++
				}
			}
			if listings != 1 {
				t.Errorf("%d listings of pull requests before the last create, want the recheck's only: a refused create needs no reconcile (%q)", listings, calls)
			}
		})
	}
}

// The deadline bounds the throttle's waits: a block the budget cannot start
// before the deadline is deferred:deadline; a target rate limited with a
// pause past the deadline is deferred:rate-limit, and the rest of the queue
// deferred:deadline without a call.
func TestThrottleDeadline(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		w := newExWorld(t, exConfig{})
		works := w.declined(3)
		clock := newFakeClock(exEpoch)
		w.paced(clock, throttle.Limits{WritesPerHour: 3}, exEpoch.Add(30*time.Minute))
		w.execute(works...)
		exWant(t, works[0].t, report.OutcomeDeclined, "", works[0].Decision.PR)
		for _, wk := range works[1:] {
			exWant(t, wk.t, report.OutcomeDeferred, "deadline", wk.Decision.PR)
			if !exHasWarn(wk.t, "wait for the provider's budget: provider gh: its budget leaves no room before the run's deadline") {
				t.Errorf("%s: warnings %q", wk.t.repo.Path, wk.t.res.Warnings)
			}
			if wk.t.res.Writes != 0 {
				t.Errorf("%s: %d writes", wk.t.repo.Path, wk.t.res.Writes)
			}
		}
	})
	t.Run("pause", func(t *testing.T) {
		w := newExWorld(t, exConfig{})
		works := w.declined(2)
		clock := newFakeClock(exEpoch)
		w.paced(clock, throttle.Limits{}, exEpoch.Add(5*time.Minute))
		limited := &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests,
			RetryAfter: 10 * time.Minute, Err: errors.New("slow down")}
		w.p.FailNext("EditPR", limited)
		w.execute(works...)
		exWant(t, works[0].t, report.OutcomeDeferred, "rate-limit", works[0].Decision.PR)
		exWant(t, works[1].t, report.OutcomeDeferred, "deadline", works[1].Decision.PR)
		if !exHasWarn(works[1].t, "provider gh: a pause for a rate limit leaves no room before the run's deadline (a wait of 10m0s)") {
			t.Errorf("warnings %q", works[1].t.res.Warnings)
		}
		if n := countCalls(w.p, "EditPR"); n != 1 {
			t.Errorf("%d edits after the pause", n)
		}
	})
}

// scaleFaults injects the answers of a busy platform into the writes of a
// world: rate limits with and without Retry-After, server errors before
// and after the write took effect (a lost answer, as a timeout), all of
// them after the request was sent (metered).
type scaleFaults struct {
	platform.Writer
	mu    sync.Mutex
	n     int
	off   bool
	clock *fakeClock
	// times are when writes reached the platform; limits when each rate
	// limit was answered, with the wait it asked for.
	times  []time.Time
	limits []scaleLimit
}

// scaleLimit is a rate limit the platform answered at at, asking to wait
// wait; idx is how many writes had reached it then, the limited one
// included: every write from idx on came after the answer.
type scaleLimit struct {
	at   time.Time
	wait time.Duration
	idx  int
}

// roll returns the next fault die, 0–99, deterministic.
func (f *scaleFaults) roll() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return 99
	}
	f.n++
	return (f.n*37 + f.n/7) % 100
}

func (f *scaleFaults) sent(ctx context.Context, k throttle.Kind) error {
	if err := throttle.Write(ctx, k); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.times = append(f.times, f.clock.Now())
	return nil
}

func (f *scaleFaults) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	tw, err := f.Writer.Target(ctx, r, need)
	if err != nil {
		return nil, err
	}
	return &scaleTarget{TargetWriter: tw, f: f}, nil
}

func (f *scaleFaults) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	if f.roll() < 2 {
		return nil, &platform.Error{Op: "list pull requests", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	}
	return f.Writer.PRs(ctx, r, heads, authors)
}

type scaleTarget struct {
	platform.TargetWriter
	f *scaleFaults
}

// fault fails a write of kind k before (it was sent, the platform refused
// it) or after it took effect, or returns nil to let it through.
func (x *scaleTarget) fault(ctx context.Context, k throttle.Kind, applied func() error) (done bool, err error) {
	f := x.f
	switch d := f.roll(); {
	case d < 3:
		if err := f.sent(ctx, k); err != nil {
			return true, err
		}
		wait := time.Duration(0)
		if d == 0 {
			wait = 30 * time.Second
		}
		f.mu.Lock()
		f.limits = append(f.limits, scaleLimit{at: f.clock.Now(), wait: wait, idx: len(f.times)})
		f.mu.Unlock()
		return true, &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests, RetryAfter: wait, Err: errors.New("slow down")}
	case d < 6:
		if err := f.sent(ctx, k); err != nil {
			return true, err
		}
		return true, &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	case d < 8:
		if err := applied(); err != nil {
			return true, err
		}
		return true, &platform.Error{Op: "test", Class: platform.ClassTransient, Err: fmt.Errorf("the answer was lost: %w", context.DeadlineExceeded)}
	}
	return false, nil
}

func (x *scaleTarget) EditPR(ctx context.Context, n int64, e platform.PREdit) (platform.PR, error) {
	var pr platform.PR
	edit := func() error {
		var err error
		pr, err = x.TargetWriter.EditPR(ctx, n, e)
		if err == nil {
			x.f.mu.Lock()
			x.f.times = append(x.f.times, x.f.clock.Now())
			x.f.mu.Unlock()
		}
		return err
	}
	if done, err := x.fault(ctx, throttle.API, edit); done {
		return platform.PR{}, err
	}
	return pr, edit()
}

func (x *scaleTarget) Comment(ctx context.Context, n int64, body string) error {
	comment := func() error {
		err := x.TargetWriter.Comment(ctx, n, body)
		if err == nil {
			x.f.mu.Lock()
			x.f.times = append(x.f.times, x.f.clock.Now())
			x.f.mu.Unlock()
		}
		return err
	}
	if done, err := x.fault(ctx, throttle.Comment, comment); done {
		return err
	}
	return comment()
}

// TestThrottleScale is phase F alone at the size of TestScale (which runs
// the whole of distribute): 5000 targets on the fake with rate limits,
// server errors and lost answers. Every run keeps the platform's budget (60
// writes a minute, 450 an hour, a second apart) and every pause a rate limit
// asks for, ends by its deadline of 5 h 30 min, and the runs that follow
// finish the fleet: each declined pull request is acknowledged once, with at
// most one comment, and nothing else is written.
func TestThrottleScale(t *testing.T) {
	n := 5000
	if testing.Short() {
		n = 500
	}
	w := newExWorld(t, exConfig{})
	targets := make([]*target, n)
	prs := make([]int64, n)
	for i := range n {
		tg := w.target(fmt.Sprintf("acme/s-%04d", i), exOptIn, "version: 1\n")
		prs[i] = w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
		w.p.SetPRState(tg.repo.ID, prs[i], platform.Closed, &w.person, time.Time{})
		targets[i] = tg
	}
	clock := newFakeClock(exEpoch)
	faults := &scaleFaults{Writer: w.p.Writer(w.writer), clock: clock}
	w.prov.reader, w.prov.writer = faults, faults
	limits := limitsOf(w.p.Caps().Limits, config.ProviderLimits{}, 8)
	const runLength = 5*time.Hour + 30*time.Minute
	for run := 1; ; run++ {
		if run > 12 {
			t.Fatalf("the fleet is not done after %d runs", run-1)
		}
		faults.mu.Lock()
		faults.off, faults.times, faults.limits = true, nil, nil
		faults.mu.Unlock()
		var works []*Work
		for _, tg := range targets {
			if wk := w.work(tg, nil); wk.writes() {
				works = append(works, wk)
			}
		}
		if len(works) == 0 {
			t.Logf("done after %d runs", run-1)
			break
		}
		faults.mu.Lock()
		faults.off = false
		faults.mu.Unlock()
		start := clock.Now()
		deadline := start.Add(runLength)
		w.paced(clock, limits, deadline)
		w.execute(works...)
		counts := map[string]int{}
		for _, wk := range works {
			res := wk.t.res
			counts[string(res.Outcome)+":"+res.Reason]++
			switch key := string(res.Outcome) + ":" + res.Reason; key {
			case "declined:", "deferred:deadline", "failed:transient":
			case "deferred:rate-limit":
				// Only the provider out of budget, or a pause past the
				// target's time, defers it: one rate limit pauses and
				// retries.
				if !rateLimitExplained(res.Warnings) {
					t.Errorf("run %d: %s ended %s, unexplained (warnings %q)", run, wk.t.repo.Path, key, res.Warnings)
				}
			default:
				t.Errorf("run %d: %s ended %s (warnings %q)", run, wk.t.repo.Path, key, res.Warnings)
			}
		}
		t.Logf("run %d: %d works, %v, %d writes, %d rate limits, ended at +%v", run, len(works), counts,
			len(faults.times), len(faults.limits), clock.Now().Sub(start))
		checkBudget(t, run, faults.times, limits, deadline)
		checkPauses(t, run, faults.times, faults.limits)
		// The next run comes an hour later, as a schedule would start it.
		clock.Advance(time.Hour)
	}
	for i, tg := range targets {
		pr := w.p.PR(tg.repo.ID, prs[i])
		if m := w.marker(pr.Body); !m.Data.Ack || pr.State != platform.Closed {
			t.Errorf("%s #%d: %s, marker %+v", tg.repo.Path, prs[i], pr.State, m.Data)
		}
		if cs := w.p.Comments(tg.repo.ID, prs[i]); len(cs) > 1 {
			t.Errorf("%s #%d: %d comments", tg.repo.Path, prs[i], len(cs))
		}
		if len(w.p.PRList(tg.repo.ID)) != 1 {
			t.Errorf("%s: %d pull requests", tg.repo.Path, len(w.p.PRList(tg.repo.ID)))
		}
	}
}

// checkBudget checks the writes of one run against the limits: a minimum
// interval apart, the minute and hour limits in every window, and none
// after the deadline's grace.
func checkBudget(t *testing.T, run int, times []time.Time, l throttle.Limits, deadline time.Time) {
	t.Helper()
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < l.MinInterval {
			t.Errorf("run %d: writes %d and %d are %v apart", run, i-1, i, gap)
			break
		}
	}
	if got := maxWrites(times, time.Minute); got > l.WritesPerMinute {
		t.Errorf("run %d: %d writes in a minute, the limit is %d", run, got, l.WritesPerMinute)
	}
	if got := maxWrites(times, time.Hour); got > l.WritesPerHour {
		t.Errorf("run %d: %d writes in an hour, the limit is %d", run, got, l.WritesPerHour)
	}
	if len(times) > 0 && times[len(times)-1].After(deadline.Add(writeGrace)) {
		t.Errorf("run %d: a write at %v, after the deadline %v", run, times[len(times)-1], deadline)
	}
}

// checkPauses checks that no write reached the platform during the pause a
// rate limit asked for: its Retry-After, else at least a minute. The writes
// after the answer are those from its idx on (by position: the fake clock
// moves only when something sleeps, so a write sent at once carries the
// limit's own time).
func checkPauses(t *testing.T, run int, times []time.Time, limits []scaleLimit) {
	t.Helper()
	for _, l := range limits {
		pause := l.wait
		if pause == 0 {
			pause = throttle.FirstPause
		}
		for _, at := range times[min(l.idx, len(times)):] {
			if at.Before(l.at.Add(pause)) {
				t.Errorf("run %d: a write at %v, during the pause of %v from %v", run, at, pause, l.at)
				return
			}
		}
	}
}

// Each provider's inspections run at most as many at once as its limits
// let it read (providers[].limits.reads), whatever the run's concurrency.
func TestInspectPerProvider(t *testing.T) {
	w := newWorld(t)
	w.hubYML = defaultHubYML + "    limits: { reads: 2 }\n"
	for i := range 12 {
		w.optedIn(fmt.Sprintf("acme/c%02d", i), nil)
	}
	d := w.deps()
	d.Concurrency = 8
	gauge := &inFlight{Reader: d.Providers[0].Reader, both: make(chan struct{})}
	d.Providers[0].Reader = gauge
	rep := w.plan(d)
	for i := range 12 {
		want(t, rep, fmt.Sprintf("gh:acme/c%02d", i), report.OutcomeOpened, "", 0)
	}
	if gauge.most != 2 {
		t.Errorf("at most %d opt-in files read at once, want 2", gauge.most)
	}
}

// inFlight counts the ReadFile calls in flight; the first two meet before
// either returns, so that two at once is seen for certain.
type inFlight struct {
	platform.Reader
	mu         sync.Mutex
	now, most  int
	calls      int
	both       chan struct{}
	bothClosed bool
}

func (g *inFlight) ReadFile(ctx context.Context, r platform.Repo, ref, path string, limit int64) (platform.File, error) {
	g.mu.Lock()
	g.now++
	g.calls++
	g.most = max(g.most, g.now)
	if g.now == 2 && !g.bothClosed {
		close(g.both)
		g.bothClosed = true
	}
	first := g.calls == 1
	g.mu.Unlock()
	if first {
		select {
		case <-g.both:
		case <-time.After(10 * time.Second):
		}
	}
	defer func() {
		g.mu.Lock()
		g.now--
		g.mu.Unlock()
	}()
	return g.Reader.ReadFile(ctx, r, ref, path, limit)
}

// A pause of one provider stops its own inspections only: GitLab's first
// opt-in read is rate limited, and its inspections wait out the pause on a
// clock that does not move until the test lets it; GitHub's inspections
// meanwhile read every opt-in file and finish. The run's pool of
// inspections was once shared, and GitLab's sleeping workers held every
// slot of it.
func TestInspectPauseOneProvider(t *testing.T) {
	w := newFleet(t, [2]int{40, 40})
	for _, sp := range w.provs {
		sp.faults.begin(1, true)
	}
	gh, gl := w.provs[0], w.provs[1]
	gl.p.FailNext("ReadFile", &platform.Error{Op: "read file", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests,
		RetryAfter: time.Minute, Err: errors.New("slow down")})
	release := make(chan struct{})
	d := w.deps(ModePlan, time.Time{})
	d.Concurrency = 8
	d.clocks["gl"] = throttle.Clock{Now: gl.clock.Now, Sleep: func(ctx context.Context, dur time.Duration) error {
		select {
		case <-release:
			return gl.clock.Sleep(ctx, dur)
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	reads := 0
	for _, st := range w.fleet {
		switch st.kind {
		case fleetArchived, fleetPrivate, fleetDropped, fleetHidden:
		default:
			if st.prov == gh {
				reads++
			}
		}
	}
	type result struct {
		rep *report.Delivery
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := Plan(t.Context(), d)
		done <- result{rep, err}
	}()
	for wait := time.Now().Add(30 * time.Second); countCalls(gh.p, "ReadFile") < reads; {
		if time.Now().After(wait) {
			close(release)
			<-done
			t.Fatalf("GitHub read %d of %d opt-in files while GitLab paused", countCalls(gh.p, "ReadFile"), reads)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	for _, tg := range res.rep.Targets {
		if tg.Outcome == report.OutcomeDeferred || tg.Outcome == report.OutcomeFailed {
			t.Errorf("%s:%s is %s:%s (warnings %q)", tg.Provider, tg.Path, tg.Outcome, tg.Reason, tg.Warnings)
		}
	}
}
