package throttle

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// epoch is where the tests' clocks start.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// clock moves only when a Gate sleeps on it: Sleep advances it by the wait
// and returns at once, so pacing takes no real time and every wait shows.
type clock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newClock() *clock { return &clock{now: epoch} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// waits returns the waits so far and forgets them.
func (c *clock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.slept
	c.slept = nil
	return out
}

func (c *clock) gate(o Options) *Gate {
	o.Clock = Clock{Now: c.Now, Sleep: c.Sleep}
	if o.Name == "" {
		o.Name = "gh"
	}
	return New(o)
}

// maxIn returns the most events of times in any window of span.
func maxIn(times []time.Time, span time.Duration) int {
	most := 0
	for i := range times {
		n := 0
		for j := i; j < len(times) && times[j].Sub(times[i]) < span; j++ {
			n++
		}
		most = max(most, n)
	}
	return most
}

// The sliding windows hold any window of their span to their limit, for
// any number of events asked at once, and let events through at once while
// there is room.
func TestWindow(t *testing.T) {
	var w window
	w.limit(3, time.Minute)
	at := epoch
	var got []time.Time
	for range 10 {
		at = w.earliest(at, 1)
		w.add(at)
		got = append(got, at)
	}
	want := []time.Time{epoch, epoch, epoch, epoch.Add(time.Minute), epoch.Add(time.Minute), epoch.Add(time.Minute),
		epoch.Add(2 * time.Minute), epoch.Add(2 * time.Minute), epoch.Add(2 * time.Minute), epoch.Add(3 * time.Minute)}
	if !slices.Equal(got, want) {
		t.Errorf("events at %v, want %v", got, want)
	}
	// Room for k events: the k-th oldest of the last n must have left.
	if e := w.earliest(epoch, 2); !e.Equal(epoch.Add(3 * time.Minute)) {
		t.Errorf("room for 2 at %v", e)
	}
	if e := w.earliest(epoch, 3); !e.Equal(epoch.Add(4 * time.Minute)) {
		t.Errorf("room for 3 at %v", e)
	}
	if e := w.earliest(epoch, 99); !e.Equal(epoch.Add(4 * time.Minute)) {
		t.Errorf("room for more than the limit at %v, want as for the limit", e)
	}
	// Lowering the limit keeps the newest events.
	w.limit(1, time.Minute)
	if len(w.log) != 1 || !w.log[0].Equal(epoch.Add(3*time.Minute)) {
		t.Errorf("log %v", w.log)
	}
	// Without a limit nothing waits, and the log keeps the events of the
	// last span only.
	w.limit(0, time.Minute)
	if e := w.earliest(epoch, 5); !e.Equal(epoch) {
		t.Errorf("unlimited: room at %v", e)
	}
	for i := range 5 {
		w.add(epoch.Add(4*time.Minute + time.Duration(i)*20*time.Second))
	}
	if len(w.log) != 3 || !w.log[0].Equal(epoch.Add(4*time.Minute+40*time.Second)) {
		t.Errorf("unlimited: log %v, want the last minute's", w.log)
	}
}

// A limit set after calls were made counts them (the platform's limits
// come with its Probe): three reads without a limit, then a limit of three
// a minute; the next read waits for the minute of the first three to end.
func TestGateLimitsLater(t *testing.T) {
	c := newClock()
	g := c.gate(Options{})
	for range 3 {
		if err := g.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		c.advance(10 * time.Second)
	}
	g.SetLimits(Limits{ReadsPerMinute: 3, WritesPerMinute: 1})
	if err := g.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{30 * time.Second}) {
		t.Errorf("the fourth read waited %v, want until a minute after the first", got)
	}
	// The same for writes made before the limit.
	c2 := newClock()
	g2 := c2.gate(Options{})
	if err := g2.Write(t.Context(), API); err != nil {
		t.Fatal(err)
	}
	g2.SetLimits(Limits{WritesPerMinute: 1})
	if err := g2.Write(t.Context(), API); err != nil {
		t.Fatal(err)
	}
	if got := c2.waits(); !slices.Equal(got, []time.Duration{time.Minute}) {
		t.Errorf("the second write waited %v", got)
	}
}

// Random bursts never put more events in a window than its limit.
func TestWindowProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 200 {
		n := 1 + rng.IntN(20)
		span := time.Duration(1+rng.IntN(120)) * time.Second
		var w window
		w.limit(n, span)
		at := epoch
		var times []time.Time
		for range 300 {
			at = at.Add(time.Duration(rng.IntN(3000)) * time.Millisecond)
			if k := 1 + rng.IntN(n); rng.IntN(4) == 0 {
				// A block asks room for k and then takes k in a row.
				start := w.earliest(at, k)
				for range k {
					e := w.earliest(start, 1)
					if e.After(start) {
						t.Fatalf("round %d: a block with room for %d waited again for its %d-th event", round, k, len(times))
					}
					w.add(e)
					times = append(times, e)
				}
				at = start
				continue
			}
			e := w.earliest(at, 1)
			w.add(e)
			times = append(times, e)
		}
		if got := maxIn(times, span); got > n {
			t.Fatalf("round %d: %d events in %v, the limit is %d", round, got, span, n)
		}
	}
}

// Writes wait for the minimum interval and for the minute, hour and
// comment limits; the budget they respect holds over a long run.
func TestGateWrites(t *testing.T) {
	c := newClock()
	g := c.gate(Options{Limits: Limits{WritesPerMinute: 60, WritesPerHour: 450, CommentsPerMinute: 2, MinInterval: time.Second}})
	ctx := t.Context()
	var times []time.Time
	for i := range 1000 {
		k := API
		if i%10 == 9 {
			k = Comment
		}
		if err := g.Write(ctx, k); err != nil {
			t.Fatal(err)
		}
		times = append(times, c.Now())
	}
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < time.Second {
			t.Fatalf("writes %d and %d are %v apart", i-1, i, d)
		}
	}
	if got := maxIn(times, time.Minute); got > 60 {
		t.Errorf("%d writes in a minute", got)
	}
	if got := maxIn(times, time.Hour); got != 450 {
		t.Errorf("at most %d writes in an hour, want the budget of 450 used", got)
	}
	// 1000 writes at 450 an hour take a little over two hours.
	if d := times[len(times)-1].Sub(epoch); d < 2*time.Hour || d > 2*time.Hour+10*time.Minute {
		t.Errorf("1000 writes took %v", d)
	}
	if st := g.Stats(); st.Writes != 1000 || st.Waited <= 0 {
		t.Errorf("stats %+v", st)
	}
	// Comments have their own limit.
	c2 := newClock()
	g2 := c2.gate(Options{Limits: Limits{CommentsPerMinute: 2}})
	for range 3 {
		if err := g2.Write(ctx, Comment); err != nil {
			t.Fatal(err)
		}
	}
	if got := c2.waits(); !slices.Equal(got, []time.Duration{time.Minute}) {
		t.Errorf("the third comment of a minute waited %v", got)
	}
	if err := g2.Write(ctx, API); err != nil || len(c2.waits()) != 0 {
		t.Errorf("a write waited for the comment limit: %v", err)
	}
}

// Reads spend the read budget; without limits nothing waits.
func TestGateReads(t *testing.T) {
	c := newClock()
	g := c.gate(Options{Limits: Limits{ReadsPerMinute: 2}})
	for range 5 {
		if err := g.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{time.Minute, time.Minute}) {
		t.Errorf("waits %v", got)
	}
	free := c.gate(Options{})
	for range 100 {
		if err := free.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := free.Write(t.Context(), API); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.waits(); len(got) > 0 {
		t.Errorf("a gate without limits waited %v", got)
	}
}

// A rate limit pauses the provider: for Retry-After, else FirstPause
// doubled per strike; three strikes in a row put it out of budget, a call
// that goes through after a pause ends the streak, and a limit of a call
// that started before the latest pause is no new strike.
func TestGateLimited(t *testing.T) {
	ctx := t.Context()
	c := newClock()
	g := c.gate(Options{})
	tk := g.Ticket(ReadCall)
	g.Limited(tk, 0)
	// Two more calls that started before the pause were limited too: the
	// same pause, which a shorter Retry-After does not change.
	g.Limited(tk, 0)
	g.Limited(tk, 30*time.Second)
	if err := g.Wait(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	tk = g.Ticket(ReadCall)
	g.Limited(tk, 0) // a second strike: twice as long
	if err := g.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{time.Minute, 2 * time.Minute}) {
		t.Errorf("pauses %v", got)
	}
	g.Passed(g.Ticket(ReadCall)) // ends the streak
	tk = g.Ticket(ReadCall)
	g.Limited(tk, 45*time.Second) // Retry-After wins
	if err := g.Write(ctx, API); err != nil {
		t.Fatal(err)
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{45 * time.Second}) {
		t.Errorf("pause %v, want Retry-After", got)
	}
	for range Strikes - 1 {
		g.Limited(g.Ticket(ReadCall), 0)
	}
	if reason, why := g.Deferral(); reason != ReasonRateLimit || why != "provider gh limited the rate 3 times in a row; the rest waits for the next run" {
		t.Errorf("deferral %q, %q", reason, why)
	}
	for _, call := range []func() error{
		func() error { return g.Read(ctx) },
		func() error { return g.Write(ctx, API) },
		func() error { return g.Wait(ctx, time.Time{}) },
		func() error { _, err := g.Block(ctx, 1); return err },
	} {
		err := call()
		if e, ok := Refused(err); !ok || e.Reason != ReasonRateLimit || e.Provider != "gh" {
			t.Errorf("a call to a provider out of budget: %v", err)
		}
	}
	if st := g.Stats(); st.Limits != 7 || st.Pauses != 4 {
		t.Errorf("stats %+v", st)
	}
}

// A pause longer than MaxPause, or past the run's deadline, puts the
// provider out of budget at once.
func TestGateLongPause(t *testing.T) {
	c := newClock()
	g := c.gate(Options{MaxPause: 10 * time.Minute})
	g.Limited(g.Ticket(ReadCall), 11*time.Minute)
	if reason, why := g.Deferral(); reason != ReasonRateLimit || why != "provider gh asks to wait 11m0s, longer than touchmark pauses (10m0s); the rest waits for the next run" {
		t.Errorf("deferral %q, %q", reason, why)
	}
	// A pause past the deadline: the run is out of time.
	g = c.gate(Options{Deadline: epoch.Add(30 * time.Second)})
	g.Limited(g.Ticket(ReadCall), 0)
	if reason, _ := g.Deferral(); reason != "" {
		t.Errorf("deferral %q", reason)
	}
	err := g.Wait(t.Context(), time.Time{})
	if e, ok := Refused(err); !ok || e.Reason != ReasonDeadline || e.Error() != "provider gh: a pause for a rate limit leaves no room before the run's deadline (a wait of 1m0s)" {
		t.Errorf("a pause past the deadline: %v", err)
	}
	// Pause without a strike: the platform said it has nothing left.
	g = c.gate(Options{})
	g.Pause(30 * time.Second)
	if err := g.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{30 * time.Second}) {
		t.Errorf("waits %v", got)
	}
}

// No wait ends past the run's deadline or a block's bound: the call is
// refused instead, deferred:deadline when the budget keeps it and
// deferred:rate-limit when a pause does. A call that need not wait is never
// refused for the time.
func TestGateBounds(t *testing.T) {
	ctx := t.Context()
	c := newClock()
	g := c.gate(Options{Limits: Limits{WritesPerMinute: 1}, Deadline: epoch.Add(30 * time.Second)})
	if err := g.Write(ctx, API); err != nil {
		t.Fatal(err)
	}
	err := g.Write(ctx, API)
	if e, ok := Refused(err); !ok || e.Reason != ReasonDeadline || e.Wait != time.Minute {
		t.Errorf("a write past the deadline: %v", err)
	}
	// A block starts only before the deadline.
	b, err := g.Block(ctx, 1)
	if e, ok := Refused(err); !ok || e.Reason != ReasonDeadline {
		t.Errorf("a block that would start past the deadline: %v, %v", b, err)
	}
	c.advance(40 * time.Second)
	if err := g.Read(ctx); err != nil {
		t.Errorf("a read that need not wait, past the deadline: %v", err)
	}
	// A pause past a bound before the deadline: longer than a target's
	// writes may take.
	c2 := newClock()
	g2 := c2.gate(Options{Deadline: epoch.Add(time.Hour)})
	g2.Limited(g2.Ticket(ReadCall), 5*time.Minute)
	err = g2.Wait(ctx, epoch.Add(time.Minute))
	if e, ok := Refused(err); !ok || e.Reason != ReasonRateLimit || e.Wait != 5*time.Minute ||
		e.Error() != "provider gh: a pause for a rate limit leaves no room before the end of the target's writes (a wait of 5m0s)" {
		t.Errorf("a pause past the bound: %v", err)
	}
	if err := g2.Wait(ctx, time.Time{}); err != nil || !slices.Equal(c2.waits(), []time.Duration{5 * time.Minute}) {
		t.Errorf("a pause without a bound: %v", err)
	}
}

// A block waits until the minute and hour limits have room for its
// writes, so that it does not stop in the middle; its writes count, and
// they are bounded by its bound.
func TestGateBlock(t *testing.T) {
	ctx := t.Context()
	c := newClock()
	g := c.gate(Options{Limits: Limits{WritesPerMinute: 4, WritesPerHour: 100, MinInterval: time.Second}})
	for range 3 {
		if err := g.Write(ctx, API); err != nil {
			t.Fatal(err)
		}
	}
	c.waits()
	// Three writes in the last minute leave room for one: a block of three
	// waits until two more fit.
	start := c.Now()
	b, err := g.Block(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if waited := c.Now().Sub(start); waited != time.Minute-time.Second {
		t.Errorf("the block waited %v", waited)
	}
	c.waits()
	m := Meter(b)
	for range 3 {
		if err := m.Write(ctx, API); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{time.Second, time.Second}) {
		t.Errorf("the block's writes waited %v: more than the minimum interval", got)
	}
	if b.Writes() != 3 {
		t.Errorf("%d writes counted", b.Writes())
	}
	// A block's writes stop at its bound, which may be past the run's
	// deadline (the grace of the write in flight).
	g.deadline = c.Now().Add(10 * time.Second)
	b, err = g.Block(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b.Bound(c.Now().Add(30 * time.Second))
	for i := 0; ; i++ {
		err := b.Write(ctx, API)
		if err != nil {
			if e, ok := Refused(err); !ok || e.Reason != ReasonDeadline || i == 0 {
				t.Errorf("write %d: %v", i, err)
			}
			break
		}
	}
	// A cancelled wait is no write of the block.
	c3 := newClock()
	g3 := c3.gate(Options{Limits: Limits{MinInterval: time.Minute}})
	b3, err := g3.Block(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := b3.Write(ctx, API); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := b3.Write(cancelled, API); !errors.Is(err, context.Canceled) || b3.Writes() != 1 {
		t.Errorf("a cancelled write: %v, %d writes", err, b3.Writes())
	}
	var nilBlock *Block
	if nilBlock.Writes() != 0 {
		t.Error("a nil block counts writes")
	}
}

// The breaker: refused credentials for AuthStreak targets in a row put the
// provider down, and so do plain 403s for BanStreak targets when the Gate
// watches for bans; a success ends both streaks, another failure ends the
// streak of 403s.
func TestGateBreaker(t *testing.T) {
	c := newClock()
	g := c.gate(Options{})
	for range AuthStreak - 1 {
		g.AuthFailed()
	}
	if reason, _ := g.Deferral(); reason != "" {
		t.Errorf("down after %d refusals", AuthStreak-1)
	}
	g.Healthy()
	for range AuthStreak {
		g.AuthFailed()
	}
	if reason, why := g.Deferral(); reason != ReasonProviderDown || why != "provider gh refused the credential for 3 targets in a row; the rest waits for the next run" {
		t.Errorf("deferral %q, %q", reason, why)
	}
	if _, err := g.Block(t.Context(), 1); err == nil {
		t.Error("a block of a provider down")
	} else if e, _ := Refused(err); e.Reason != ReasonProviderDown {
		t.Errorf("refusal %v", err)
	}
	for _, watch := range []bool{false, true} {
		g := c.gate(Options{WatchForbidden: watch})
		for range BanStreak - 1 {
			g.Forbidden(true)
		}
		g.Forbidden(false)
		for range BanStreak {
			g.Forbidden(true)
		}
		reason, why := g.Deferral()
		switch {
		case !watch && reason != "":
			t.Errorf("a gate that does not watch for bans: %q", why)
		case watch && (reason != ReasonProviderDown || why != "provider gh answered 403 Forbidden for 5 targets in a row, as to an IP it banned; "+
			"the rest waits for the next run, so that the ban is not extended"):
			t.Errorf("watching: %q, %q", reason, why)
		}
	}
}

// The git slots bound the fetches at once; SetLimits replaces them.
func TestGateGit(t *testing.T) {
	g := New(Options{Limits: Limits{GitReads: 2}})
	ctx := t.Context()
	r1, err := g.Git(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.Git(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := g.Git(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a third fetch at once: %v", err)
	}
	r1()
	r1() // twice is once
	r3, err := g.Git(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2()
	r3()
	g.SetLimits(Limits{})
	if _, err := g.Git(ctx); err != nil {
		t.Errorf("no limit: %v", err)
	}
	if l := g.Limits(); l.GitReads != 0 {
		t.Errorf("limits %+v", l)
	}
}

// meterLog records what a Meter was asked.
type meterLog struct {
	mu  sync.Mutex
	log []string
	err error
}

func (m *meterLog) Read(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = append(m.log, "read")
	return m.err
}

func (m *meterLog) Write(_ context.Context, k Kind) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = append(m.log, [...]string{"api", "comment", "push"}[k])
	return m.err
}

// The context's Meter is asked for each request by its method and marks:
// GET, HEAD and OPTIONS read, other methods write; AsRead makes a write a
// read (a GraphQL query), AsComment a comment, Uncounted nothing; a context
// without a Meter asks nothing.
func TestMeter(t *testing.T) {
	m := &meterLog{}
	ctx := With(t.Context(), m)
	for _, step := range []struct {
		ctx    context.Context
		method string
	}{
		{ctx, "GET"}, {ctx, ""}, {ctx, "HEAD"}, {ctx, "OPTIONS"}, {ctx, "POST"}, {ctx, "PATCH"}, {ctx, "DELETE"}, {ctx, "PUT"},
		{AsRead(ctx), "POST"}, {AsComment(ctx), "POST"}, {AsComment(ctx), "GET"}, {Uncounted(ctx), "POST"}, {Uncounted(ctx), "GET"},
		{t.Context(), "POST"},
	} {
		if err := Request(step.ctx, step.method); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(ctx, Push); err != nil {
		t.Fatal(err)
	}
	if err := Write(AsComment(ctx), Push); err != nil {
		t.Fatal(err)
	}
	want := []string{"read", "read", "read", "read", "api", "api", "api", "api", "read", "comment", "read", "push", "push"}
	if !slices.Equal(m.log, want) {
		t.Errorf("asked %q, want %q", m.log, want)
	}
	// Default keeps a Meter the context has.
	other := &meterLog{}
	if err := Read(Default(ctx, other)); err != nil || len(other.log) > 0 {
		t.Errorf("Default replaced the meter: %v %q", err, other.log)
	}
	if err := Read(Default(t.Context(), other)); err != nil || len(other.log) != 1 {
		t.Errorf("Default set no meter: %v %q", err, other.log)
	}
	// A refusal comes back as it is.
	m.err = &Error{Provider: "gh", Reason: ReasonDeadline, msg: "no"}
	if err := Request(ctx, "POST"); !errors.Is(err, m.err) {
		t.Errorf("refusal %v", err)
	}
}

// Many readers at once share the read budget: 200 reads at 10 a minute
// reach at least into the 20th minute, whatever the order they reserve in
// (later when the readers' waits on the shared clock overlap).
func TestGateConcurrentReads(t *testing.T) {
	c := newClock()
	g := c.gate(Options{Limits: Limits{ReadsPerMinute: 10}})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				if err := g.Read(t.Context()); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	g.mu.Lock()
	last := g.reads.log[len(g.reads.log)-1]
	g.mu.Unlock()
	if want := epoch.Add(19 * time.Minute); last.Before(want) {
		t.Errorf("the last read at %v, before %v", last, want)
	}
	if st := g.Stats(); st.Reads != 200 {
		t.Errorf("stats %+v", st)
	}
}

// Reads that go through end no streak of rate-limited writes (GitHub
// limits content creation while it answers reads), and a write that goes
// through ends it.
func TestGateStreaksByKind(t *testing.T) {
	ctx := t.Context()
	c := newClock()
	g := c.gate(Options{})
	for range Strikes - 1 {
		g.Limited(g.Ticket(WriteCall), 0)
		if err := g.Wait(ctx, time.Time{}); err != nil {
			t.Fatal(err)
		}
		g.Passed(g.Ticket(ReadCall))
	}
	g.Limited(g.Ticket(WriteCall), 0)
	if reason, _ := g.Deferral(); reason != ReasonRateLimit {
		t.Errorf("writes limited %d times in a row with reads between: deferral %q", Strikes, reason)
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{time.Minute, 2 * time.Minute}) {
		t.Errorf("pauses %v", got)
	}
	g = c.gate(Options{})
	g.Limited(g.Ticket(WriteCall), 0)
	if err := g.Wait(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	g.Passed(g.Ticket(WriteCall))
	g.Limited(g.Ticket(WriteCall), 0)
	if err := g.Wait(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := c.waits(); !slices.Equal(got, []time.Duration{time.Minute, time.Minute}) {
		t.Errorf("pauses %v: a write that went through ends the streak", got)
	}
}

// A clock whose Sleep returns at once and that does not move: the Gate's
// own clock still moves by its waits, so a pause is waited once.
func TestGateVirtualClock(t *testing.T) {
	var slept []time.Duration
	g := New(Options{Name: "gh", Limits: Limits{MinInterval: time.Second}, Clock: Clock{
		Now:   func() time.Time { return epoch },
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}})
	g.Limited(g.Ticket(ReadCall), 0)
	for range 3 {
		if err := g.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := g.Write(t.Context(), API); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(slept, []time.Duration{time.Minute, time.Second, time.Second}) {
		t.Errorf("waits %v", slept)
	}
}

// The headers of an answer that show no budget left pause the provider
// until the reset, without a strike: GitHub's x-ratelimit-*, GitLab's
// RateLimit-* (a Unix time, seconds, or an HTTP date), the IETF draft's
// RateLimit of Codeberg. Anything else changes nothing.
func TestObserve(t *testing.T) {
	for _, tc := range []struct {
		name  string
		h     map[string]string
		pause time.Duration
	}{
		{"github", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(epoch.Add(90*time.Second).Unix(), 10)}, 90 * time.Second},
		{"github, left", map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": strconv.FormatInt(epoch.Add(90*time.Second).Unix(), 10)}, 0},
		{"gitlab seconds", map[string]string{"RateLimit-Remaining": "0", "RateLimit-Reset": "20"}, 20 * time.Second},
		{"gitlab date", map[string]string{"RateLimit-Remaining": "0", "RateLimit-ResetTime": epoch.Add(time.Minute).Format(http.TimeFormat)}, time.Minute},
		{"reset past", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(epoch.Add(-time.Minute).Unix(), 10)}, time.Second},
		{"ietf draft", map[string]string{"RateLimit": `"default";r=0;t=30`}, 30 * time.Second},
		{"ietf draft, left", map[string]string{"RateLimit": `"default";r=5;t=30`}, 0},
		{"no reset", map[string]string{"X-RateLimit-Remaining": "0"}, 0},
		{"none", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			g := c.gate(Options{})
			h := http.Header{}
			for k, v := range tc.h {
				h.Set(k, v)
			}
			ctx := With(t.Context(), g)
			Observe(ctx, h)
			Observe(Uncounted(ctx), http.Header{"Ratelimit": {`r=0;t=3600`}})
			Observe(t.Context(), http.Header{"Ratelimit": {`r=0;t=3600`}})
			if err := g.Read(t.Context()); err != nil {
				t.Fatal(err)
			}
			var want []time.Duration
			if tc.pause > 0 {
				want = []time.Duration{tc.pause}
			}
			if got := c.waits(); !slices.Equal(got, want) {
				t.Errorf("waits %v, want %v", got, want)
			}
			if st := g.Stats(); st.Limits != 0 {
				t.Errorf("a strike: %+v", st)
			}
		})
	}
}

// manualClock moves only when the test advances it. Sleep blocks until the
// clock reaches the sleeper's wake-up time, and first tells the test when
// that is (asleep), so the test can act while the call waits.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []manualWaiter
	asleep  chan time.Time
}

type manualWaiter struct {
	at   time.Time
	wake chan struct{}
}

func newManualClock() *manualClock {
	return &manualClock{now: epoch, asleep: make(chan time.Time, 16)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	w := manualWaiter{at: c.now.Add(d), wake: make(chan struct{})}
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()
	c.asleep <- w.at
	select {
	case <-w.wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// advance moves the clock by d and wakes the sleepers it reached.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		close(w.wake)
	}
	c.waiters = kept
}

// A call that reserved its time before a pause began, and sleeps until
// then, does not go out while the pause is in force: it reserves again
// after the pause. Its rate limit is then a new strike, as the request went
// out after the pause (the ticket of the call is the one of its request),
// and its reservation is not spent twice.
func TestGateBookedBeforePause(t *testing.T) {
	for _, kind := range []string{"read", "write"} {
		t.Run(kind, func(t *testing.T) {
			c := newManualClock()
			g := New(Options{Name: "gl", Limits: Limits{ReadsPerMinute: 1, WritesPerMinute: 1},
				Clock: Clock{Now: c.Now, Sleep: c.Sleep}})
			ck := ReadCall
			if kind == "write" {
				ck = WriteCall
			}
			call := func(ctx context.Context) error {
				if kind == "read" {
					return Read(ctx)
				}
				return Write(ctx, API)
			}
			ctxA := With(t.Context(), g)
			tkA := g.TicketFor(ctxA, ck)
			if err := call(ctxA); err != nil {
				t.Fatal(err)
			}
			ctxB := With(t.Context(), g)
			tkB := g.TicketFor(ctxB, ck)
			done := make(chan error, 1)
			go func() { done <- call(ctxB) }()
			if at := <-c.asleep; !at.Equal(epoch.Add(time.Minute)) {
				t.Fatalf("B sleeps until %v", at)
			}
			c.advance(time.Second)
			g.Limited(tkA, 0) // paused until epoch+61s
			c.advance(59 * time.Second)
			select {
			case at := <-c.asleep:
				if !at.Equal(epoch.Add(61 * time.Second)) {
					t.Errorf("B reserved again until %v", at)
				}
			case err := <-done:
				t.Fatalf("B went out at %v, in the pause (%v)", c.Now(), err)
			}
			c.advance(time.Second)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			st := g.Stats()
			if (kind == "read" && st.Reads != 2) || (kind == "write" && st.Writes != 2) {
				t.Errorf("stats %+v: the reservation counted twice", st)
			}
			// B's rate limit: a second strike, twice as long a pause.
			g.Limited(tkB, 0)
			g.mu.Lock()
			paused, gen := g.paused, g.gen
			g.mu.Unlock()
			if want := epoch.Add(61*time.Second + 2*time.Minute); !paused.Equal(want) || gen != 2 {
				t.Errorf("paused until %v (gen %d), want %v", paused, gen, want)
			}
			// A call that went out before the pause and is limited in it
			// only belongs to that pause.
			g.Limited(tkA, 0)
			if g.Stats().Pauses != 2 {
				t.Errorf("stats %+v", g.Stats())
			}
		})
	}
}

// schedClock is a virtual-time clock for a known number of workers: Sleep
// blocks until the clock reaches the sleeper's wake-up time, and the clock
// moves to the earliest wake-up only once every live worker sleeps (or has
// finished). So a worker that logs a request as soon as its call was let
// out logs the time the Gate reserved for it, whatever the workers' order.
type schedClock struct {
	mu     sync.Mutex
	now    time.Time
	live   int
	asleep []*schedSleeper
}

type schedSleeper struct {
	at   time.Time
	wake chan struct{}
}

func newSchedClock(workers int) *schedClock { return &schedClock{now: epoch, live: workers} }

func (c *schedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *schedClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	s := &schedSleeper{at: c.now.Add(d), wake: make(chan struct{})}
	c.asleep = append(c.asleep, s)
	c.advance()
	c.mu.Unlock()
	select {
	case <-s.wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// done records that a worker finished.
func (c *schedClock) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live--
	c.advance()
}

// advance moves the clock to the earliest wake-up and wakes the sleepers it
// reached, once every live worker sleeps. Called with c.mu held.
func (c *schedClock) advance() {
	if len(c.asleep) == 0 || len(c.asleep) < c.live {
		return
	}
	next := c.asleep[0].at
	for _, s := range c.asleep {
		if s.at.Before(next) {
			next = s.at
		}
	}
	c.now = later(c.now, next)
	kept := c.asleep[:0]
	for _, s := range c.asleep {
		if s.at.After(c.now) {
			kept = append(kept, s)
			continue
		}
		close(s.wake)
	}
	c.asleep = kept
}

// sentRequest is a request a worker of TestGateConcurrentPaced sent: when,
// whether it wrote, and the Gate's generation when it was let out.
type sentRequest struct {
	at    time.Time
	write bool
	gen   uint64
}

// strike is a rate limit that started a pause: the generation it began, and
// the least the pause lasts (its Retry-After, else FirstPause).
type strike struct {
	gen uint64
	end time.Time
}

// Eight workers share a Gate's budget and pauses, as the inspections of one
// provider do at the run's default concurrency, on a clock that moves only
// when every worker waits. The platform answers some requests with a rate
// limit, with or without Retry-After, others go through. Whatever the order
// the workers run in: no minute holds more reads or writes than the limits,
// writes keep the minimum interval, and no request the Gate let out after a
// rate limit started a pause comes before that pause's end. The scale tests
// run at a concurrency of 1 only, so this one covers concurrent callers.
func TestGateConcurrentPaced(t *testing.T) {
	const workers, ops = 8, 150
	limits := Limits{ReadsPerMinute: 30, WritesPerMinute: 10, WritesPerHour: 200, MinInterval: time.Second}
	c := newSchedClock(workers)
	g := New(Options{Name: "gl", Limits: limits, Clock: Clock{Now: c.Now, Sleep: c.Sleep}, MaxPause: time.Hour})
	var (
		net     sync.Mutex
		sent    []sentRequest
		strikes []strike
		wg      sync.WaitGroup
	)
	for w := range workers {
		wg.Go(func() {
			defer c.done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			ctx := With(t.Context(), g)
			for range ops {
				write := rng.IntN(3) == 0
				call := ReadCall
				if write {
					call = WriteCall
				}
				tk := g.TicketFor(ctx, call)
				var err error
				if write {
					err = Write(ctx, API)
				} else {
					err = Read(ctx)
				}
				if _, refused := Refused(err); refused {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				// The platform answers at once: no time passes.
				net.Lock()
				g.mu.Lock()
				gen := sendsOf(ctx).gen
				before := g.gen
				g.mu.Unlock()
				at := c.Now()
				sent = append(sent, sentRequest{at: at, write: write, gen: gen})
				switch roll := rng.IntN(100); {
				case roll < 3:
					ra := time.Duration(0)
					if roll == 0 {
						ra = 20 * time.Second
					}
					g.Limited(tk, ra)
					g.mu.Lock()
					after := g.gen
					g.mu.Unlock()
					if after > before {
						strikes = append(strikes, strike{gen: after, end: at.Add(cmp.Or(ra, FirstPause))})
					}
				default:
					g.Passed(tk)
				}
				net.Unlock()
			}
		})
	}
	wg.Wait()
	if len(sent) < workers*ops/2 {
		t.Fatalf("%d requests sent of %d: the provider went out of budget early (%+v)", len(sent), workers*ops, g.Stats())
	}
	var reads, writes []time.Time
	for i, r := range sent {
		if i > 0 && r.at.Before(sent[i-1].at) {
			t.Fatalf("request %d at %v, before request %d at %v", i, r.at, i-1, sent[i-1].at)
		}
		if r.write {
			writes = append(writes, r.at)
		} else {
			reads = append(reads, r.at)
		}
	}
	if got := maxIn(reads, time.Minute); got > limits.ReadsPerMinute {
		t.Errorf("%d reads in a minute, the limit is %d", got, limits.ReadsPerMinute)
	}
	if got := maxIn(writes, time.Minute); got > limits.WritesPerMinute {
		t.Errorf("%d writes in a minute, the limit is %d", got, limits.WritesPerMinute)
	}
	if got := maxIn(writes, time.Hour); got > limits.WritesPerHour {
		t.Errorf("%d writes in an hour, the limit is %d", got, limits.WritesPerHour)
	}
	for i := 1; i < len(writes); i++ {
		if gap := writes[i].Sub(writes[i-1]); gap < limits.MinInterval {
			t.Errorf("writes %d and %d are %v apart", i-1, i, gap)
			break
		}
	}
	if len(strikes) == 0 {
		t.Fatal("no rate limit started a pause")
	}
	bad := 0
	for _, r := range sent {
		for _, s := range strikes {
			if s.gen <= r.gen && r.at.Before(s.end) {
				if bad++; bad <= 3 {
					t.Errorf("a request let out at generation %d at %v, in the pause of generation %d until %v", r.gen, r.at, s.gen, s.end)
				}
			}
		}
	}
	t.Logf("%d requests, %d pauses, %v", len(sent), len(strikes), g.Stats())
}
