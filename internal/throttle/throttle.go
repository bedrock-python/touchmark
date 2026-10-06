// Package throttle paces the calls touchmark makes to a platform, one Gate
// per provider and write identity: how many
// targets are inspected at once (Limits.Inspections, which the core's pools
// apply), a semaphore for git fetches, a read budget, and the write queue's
// limits (writes per minute and per hour, comments per minute, a minimum
// interval between two writes). The minute and hour limits are sliding
// windows: no minute and no hour ever holds more writes than its limit. A
// rate limit pauses the whole provider; three in a row of reads, or of
// writes, put it out of budget for the rest of the run. The Gate also keeps
// the provider's breaker: credentials refused for several targets in a row,
// or (GitLab) the plain 403s of an IP ban.
//
// Drivers never retry or pace. They meet the throttle only through the
// Meter a context carries: internal/httpx asks it before every API request
// (Request), gitx before every push (Write), so that the budget counts HTTP
// writes, not operations (one operation may send several requests). The
// core puts the provider's Gate on the contexts of its reads, and a Block on
// those of one target's writes (the writes of a target go as one block).
//
// Waits never loop on the clock: each call reserves its time under the
// Gate's lock and then sleeps until then, and the Gate's clock never reads
// earlier than the end of its last wait. So a fake clock whose Sleep
// returns at once, or one that advances on Sleep, drives every test
// deterministically.
//
// The package depends on the standard library only; the core maps platform
// errors to the Gate's events.
package throttle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Kind is what a write does.
type Kind uint8

const (
	// API is a write through the platform's API: a POST, PATCH, PUT or
	// DELETE, a GraphQL mutation.
	API Kind = iota
	// Comment is a comment on a pull request; it spends the comment budget
	// too.
	Comment
	// Push is a git push (a branch moved, created or deleted).
	Push
)

// Limits are the pacing of one provider. A zero field is no limit.
type Limits struct {
	// Inspections is how many targets are inspected at once; GitReads how
	// many git fetches run at once.
	Inspections int
	GitReads    int
	// ReadsPerMinute is the read budget: API reads in any minute.
	ReadsPerMinute int
	// WritesPerMinute and WritesPerHour bound the writes in any minute and
	// in any hour; CommentsPerMinute the comments in any minute.
	WritesPerMinute   int
	WritesPerHour     int
	CommentsPerMinute int
	// MinInterval is the least time between the starts of two writes.
	MinInterval time.Duration
}

// Clock is the time of a Gate. Now reads it; Sleep waits d, or until ctx
// ends (its error). The zero Clock is the real one.
type Clock struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Options configure a Gate.
type Options struct {
	// Name is the provider's id, for messages.
	Name   string
	Limits Limits
	Clock  Clock
	// Deadline is the run's deadline: no wait of the Gate ends after it
	// (zero for none).
	Deadline time.Time
	// MaxPause bounds one pause: a rate limit that asks for a longer one
	// puts the provider out of budget for the run (DefaultMaxPause when
	// zero).
	MaxPause time.Duration
	// WatchForbidden turns on the breaker for plain 403s (GitLab bans an
	// IP with a 403 to every request).
	WatchForbidden bool
}

// Defaults of the Gate.
const (
	// FirstPause is the pause after a rate limit that names no wait; each
	// further limit in a row doubles it.
	FirstPause = time.Minute
	// Strikes is how many rate limits in a row put a provider out of
	// budget for the rest of the run.
	Strikes = 3
	// DefaultMaxPause bounds one pause.
	DefaultMaxPause = 15 * time.Minute
	// AuthStreak is how many targets in a row with a refused credential put
	// a provider down; BanStreak how many in a row refused with a plain 403
	// when the Gate watches for bans (more than AuthStreak: a 403 may be one
	// target's refusal).
	AuthStreak = 3
	BanStreak  = 5
)

// Reasons of an Error, the outcomes' deferred reasons (see
// docs/reference/output.md).
const (
	ReasonRateLimit    = "rate-limit"
	ReasonDeadline     = "deadline"
	ReasonProviderDown = "provider-down"
)

// Error is a call the throttle refused, before anything was sent: the
// provider is out of budget or down for the run, or the wait for a pause or
// for the budget would end past the run's deadline or the block's bound.
type Error struct {
	// Provider is the provider's id.
	Provider string
	// Reason is ReasonRateLimit (a pause, or the provider out of budget),
	// ReasonDeadline (the budget leaves no room in time) or
	// ReasonProviderDown (the breaker is open).
	Reason string
	// Wait is how long the call would have waited; zero when it was refused
	// outright.
	Wait time.Duration
	msg  string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.msg
}

// Refused returns the *Error in err's tree, if any: the call was refused
// by the throttle and nothing was sent.
func Refused(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}

// Call is what a call does, for the streaks of rate limits: a platform
// may limit its writes (GitHub's secondary limits on content creation)
// while its reads go through, so each kind has a streak of its own.
type Call uint8

const (
	ReadCall Call = iota
	WriteCall
)

// Ticket marks when a call started, and what it does, for Limited and
// Passed: a rate limit of a call that started before the provider's latest
// pause belongs to that pause, and is no new strike. A ticket of TicketFor
// follows the call's requests: the call counts as started when its latest
// request went out.
type Ticket struct {
	gen  uint64
	call Call
	// sends is the record of the call's context, and seq how many requests
	// it held when the call started (TicketFor).
	sends *sends
	seq   uint64
}

// Stats are what a Gate did in a run.
type Stats struct {
	// Writes are the writes metered; Reads the API reads.
	Writes, Reads int
	// Limits are the rate limits reported; Pauses the pauses they started
	// and Paused their total length.
	Limits, Pauses int
	Paused         time.Duration
	// Waited is the time calls waited for the budget (not for pauses).
	Waited time.Duration
}

// Gate is the throttle of one provider and write identity. It is safe for
// concurrent use.
type Gate struct {
	name           string
	limits         Limits
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
	deadline       time.Time
	maxPause       time.Duration
	watchForbidden bool

	// git is the semaphore of git fetches; nil for no limit.
	git chan struct{}

	mu       sync.Mutex
	reads    window
	minute   window
	hour     window
	comments window
	// last is when the latest write was scheduled (the minimum interval);
	// paused when the latest pause ends.
	last, paused time.Time
	// gen counts the pauses; strikes the rate limits in a row, of reads
	// and of writes.
	gen     uint64
	strikes [2]int
	// virtual is the latest time a wait of the Gate ended at: the Gate's
	// clock never reads earlier (see clock).
	virtual time.Time
	// out says why the provider takes no more calls in this run ("" while
	// it does), with the reason of its deferrals.
	out, outReason string
	// authFails and forbidden are the breaker's streaks.
	authFails, forbidden int
	stats                Stats
}

// New returns the Gate of one provider.
func New(o Options) *Gate {
	g := &Gate{
		name:           o.Name,
		limits:         o.Limits,
		now:            o.Clock.Now,
		sleep:          o.Clock.Sleep,
		deadline:       o.Deadline,
		maxPause:       o.MaxPause,
		watchForbidden: o.WatchForbidden,
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.sleep == nil {
		g.sleep = sleep
	}
	if g.maxPause <= 0 {
		g.maxPause = DefaultMaxPause
	}
	g.setLimits(o.Limits)
	return g
}

// SetLimits replaces the Gate's limits: the platform's become known with
// its Probe, after the first calls. It must be called before any git slot
// is taken (Git); the events counted so far stay.
func (g *Gate) SetLimits(l Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setLimits(l)
}

// setLimits sets the limits. Called with g.mu held or before g is shared.
func (g *Gate) setLimits(l Limits) {
	g.limits = l
	g.reads.limit(l.ReadsPerMinute, time.Minute)
	g.minute.limit(l.WritesPerMinute, time.Minute)
	g.hour.limit(l.WritesPerHour, time.Hour)
	g.comments.limit(l.CommentsPerMinute, time.Minute)
	g.git = nil
	if l.GitReads > 0 {
		g.git = make(chan struct{}, l.GitReads)
	}
}

// Limits returns the Gate's limits. Limits.Inspections is for the core to
// apply: it runs that many inspections of the provider at once.
func (g *Gate) Limits() Limits {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limits
}

// Stats returns what the Gate did so far.
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Git takes one of the provider's git slots (Limits.GitReads fetches at
// once) and returns its release; it fails only when ctx ends.
func (g *Gate) Git(ctx context.Context) (release func(), err error) {
	g.mu.Lock()
	sem := g.git
	g.mu.Unlock()
	return acquire(ctx, sem)
}

// acquire takes a slot of sem (none needed when nil).
func acquire(ctx context.Context, sem chan struct{}) (func(), error) {
	if sem == nil {
		return func() {}, ctx.Err()
	}
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

// Ticket returns the ticket of a call of kind c that starts now.
func (g *Gate) Ticket(c Call) Ticket {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Ticket{gen: g.gen, call: c}
}

// TicketFor returns the ticket of a call of kind c that starts now and
// sends its requests under ctx. When ctx carries a Meter (With), the ticket
// follows them: a request that waited in the Meter for a pause or for the
// budget went out after the pauses that began meanwhile, and its rate limit
// is a new strike, not one of theirs.
func (g *Gate) TicketFor(ctx context.Context, c Call) Ticket {
	s := sendsOf(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	t := Ticket{gen: g.gen, call: c, sends: s}
	if s != nil {
		t.seq = s.current()
	}
	return t
}

// genOf returns the generation t's call started at: that of its latest
// request, when TicketFor saw one go out. Called with g.mu held.
func (g *Gate) genOf(t Ticket) uint64 {
	if t.sends != nil {
		if gen, ok := t.sends.since(g, t.seq); ok {
			return gen
		}
	}
	return t.gen
}

// clock reads the Gate's clock: never earlier than the end of a wait the
// Gate made. A real clock always is later by then; a test's clock whose
// Sleep returns at once is not, and without this every call after a pause
// would wait for it again. Called with g.mu held.
func (g *Gate) clock() time.Time {
	return later(g.now(), g.virtual)
}

// sleepUntil waits from now until at, and then keeps at as the earliest
// time the Gate's clock reads (clock).
func (g *Gate) sleepUntil(ctx context.Context, now, at time.Time) error {
	if err := g.wait(ctx, at.Sub(now)); err != nil {
		return err
	}
	g.mu.Lock()
	g.virtual = later(g.virtual, at)
	g.mu.Unlock()
	return nil
}

// Wait waits out a pause of the provider. It refuses (an *Error) when the
// provider is out of budget or down, or when the pause ends after bound
// (zero: the run's deadline).
func (g *Gate) Wait(ctx context.Context, bound time.Time) error {
	g.mu.Lock()
	if err := g.refusal(); err != nil {
		g.mu.Unlock()
		return err
	}
	now := g.clock()
	at := later(now, g.paused)
	if err := g.past(at, now, g.limitFor(bound), at.After(now)); err != nil {
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()
	return g.sleepUntil(ctx, now, at)
}

// Limited records that the call of ticket t was rate limited, with the
// wait the platform asked for (retryAfter, zero when it named none). A limit
// of a call that started before the latest pause only extends that pause;
// any other is a strike and pauses the provider: retryAfter, else
// FirstPause doubled for each strike before it. The Strikes-th strike in a
// row, or a pause longer than MaxPause, puts the provider out of budget for
// the run; a pause past the run's deadline refuses the waits for it
// (ReasonDeadline).
func (g *Gate) Limited(t Ticket, retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.Limits++
	now := g.clock()
	if g.genOf(t) < g.gen {
		if retryAfter > 0 {
			g.pause(now, retryAfter, false)
		}
		return
	}
	g.gen++
	g.strikes[t.call]++
	n := g.strikes[t.call]
	if n >= Strikes {
		g.setOut(ReasonRateLimit, fmt.Sprintf("provider %s limited the rate %d times in a row; the rest waits for the next run", g.name, n))
		return
	}
	d := retryAfter
	if d <= 0 {
		d = FirstPause << (n - 1)
	}
	g.pause(now, d, true)
}

// Pause pauses the provider for d without a strike: the platform said it
// has no budget left (a remaining count of 0) without refusing a call.
func (g *Gate) Pause(d time.Duration) {
	if d <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pause(g.clock(), d, true)
}

// pause pauses the provider until now+d, unless it is paused longer
// already; counted says whether it is a new pause for the stats. Called
// with g.mu held.
func (g *Gate) pause(now time.Time, d time.Duration, counted bool) {
	if d > g.maxPause {
		g.setOut(ReasonRateLimit, fmt.Sprintf("provider %s asks to wait %s, longer than touchmark pauses (%s); the rest waits for the next run",
			g.name, d, g.maxPause))
		return
	}
	until := now.Add(d)
	if !until.After(g.paused) {
		return
	}
	if counted {
		g.stats.Pauses++
	}
	g.stats.Paused += until.Sub(later(now, g.paused))
	g.paused = until
}

// Passed records that the call of ticket t went through: a call that
// started after the latest pause ends the streak of rate limits of its
// kind.
func (g *Gate) Passed(t Ticket) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.genOf(t) == g.gen {
		g.strikes[t.call] = 0
	}
}

// setOut puts the provider out for the rest of the run, once. Called with
// g.mu held.
func (g *Gate) setOut(reason, why string) {
	if g.out == "" {
		g.out, g.outReason = why, reason
	}
}

// Deferral returns why the provider takes no more calls in this run, with
// the deferred reason of its targets ("" when it takes them): out of
// budget ("rate-limit"), or its breaker open ("provider-down").
func (g *Gate) Deferral() (reason, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deferral()
}

func (g *Gate) deferral() (reason, why string) {
	switch {
	case g.out != "":
		return g.outReason, g.out
	case g.authFails >= AuthStreak:
		return ReasonProviderDown, fmt.Sprintf("provider %s refused the credential for %d targets in a row; the rest waits for the next run", g.name, g.authFails)
	case g.watchForbidden && g.forbidden >= BanStreak:
		return ReasonProviderDown, fmt.Sprintf("provider %s answered 403 Forbidden for %d targets in a row, as to an IP it banned; "+
			"the rest waits for the next run, so that the ban is not extended", g.name, g.forbidden)
	}
	return "", ""
}

// refusal is the *Error of a call to a provider that takes no more calls.
// Called with g.mu held.
func (g *Gate) refusal() error {
	reason, why := g.deferral()
	if reason == "" {
		return nil
	}
	return &Error{Provider: g.name, Reason: reason, msg: why}
}

// Streaks of the breaker: the core reports how each target ended.

// AuthFailed records a target whose credential was refused: the streak of
// refused credentials grows.
func (g *Gate) AuthFailed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authFails++
}

// Forbidden records how a target failed for the breaker on bans: plain
// when it was refused with a 403 that names no rule (as a ban answers every
// request), which extends the streak of refusals; any other failure is
// that target's and ends the streak.
func (g *Gate) Forbidden(plain bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if plain {
		g.forbidden++
	} else {
		g.forbidden = 0
	}
}

// Healthy records that the credential worked: both streaks end.
func (g *Gate) Healthy() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authFails, g.forbidden = 0, 0
}

// Read is one API read (the Meter of the provider's reads): it waits out a
// pause and for the read budget, and refuses as Wait does.
func (g *Gate) Read(ctx context.Context) error {
	return g.read(ctx, time.Time{})
}

// Write is one write outside a Block: paced like a write of a block with no
// bound but the run's deadline.
func (g *Gate) Write(ctx context.Context, k Kind) error {
	_, err := g.write(ctx, k, time.Time{})
	return err
}

// read reserves the time of one read and waits until then. A pause that
// began while it waited, or the provider put out meanwhile, takes the
// reservation back: the read reserves again, or is refused (letOut).
func (g *Gate) read(ctx context.Context, bound time.Time) error {
	for {
		g.mu.Lock()
		if err := g.refusal(); err != nil {
			g.mu.Unlock()
			return err
		}
		now := g.clock()
		at := later(now, g.paused)
		budget := g.reads.earliest(at, 1)
		if err := g.past(budget, now, g.limitFor(bound), at.After(now) && !budget.After(at)); err != nil {
			g.mu.Unlock()
			return err
		}
		g.reads.add(budget)
		g.stats.Reads++
		g.stats.Waited += budget.Sub(at)
		g.mu.Unlock()
		if err := g.sleepUntil(ctx, now, budget); err != nil {
			return err
		}
		if g.letOut(ctx, func() {
			g.reads.remove(budget)
			g.stats.Reads--
		}) {
			return nil
		}
	}
}

// write reserves the time of one write of kind k and waits until then; it
// reports whether the write was reserved (and so counted). As for read, a
// pause that began while it waited makes it reserve again.
func (g *Gate) write(ctx context.Context, k Kind, bound time.Time) (bool, error) {
	for {
		g.mu.Lock()
		if err := g.refusal(); err != nil {
			g.mu.Unlock()
			return false, err
		}
		now := g.clock()
		at := later(now, g.paused)
		budget := at
		if g.limits.MinInterval > 0 && !g.last.IsZero() {
			budget = later(budget, g.last.Add(g.limits.MinInterval))
		}
		budget = g.minute.earliest(budget, 1)
		budget = g.hour.earliest(budget, 1)
		if k == Comment {
			budget = g.comments.earliest(budget, 1)
		}
		if err := g.past(budget, now, g.limitFor(bound), at.After(now) && !budget.After(at)); err != nil {
			g.mu.Unlock()
			return false, err
		}
		last := g.last
		g.last = budget
		g.minute.add(budget)
		g.hour.add(budget)
		if k == Comment {
			g.comments.add(budget)
		}
		g.stats.Writes++
		g.stats.Waited += budget.Sub(at)
		g.mu.Unlock()
		if err := g.sleepUntil(ctx, now, budget); err != nil {
			return true, err
		}
		if g.letOut(ctx, func() {
			if g.last.Equal(budget) {
				g.last = last
			}
			g.minute.remove(budget)
			g.hour.remove(budget)
			if k == Comment {
				g.comments.remove(budget)
			}
			g.stats.Writes--
		}) {
			return true, nil
		}
	}
}

// letOut is the last look of a call that waited until its reserved time:
// it goes out unless a pause began meanwhile and is in force, or the
// provider takes no more calls; then undo takes its reservation back, and
// the call reserves again (or is refused). A call that goes out is noted in
// the record of ctx (TicketFor) with the generation it went out at.
func (g *Gate) letOut(ctx context.Context, undo func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refusal() != nil || g.paused.After(g.clock()) {
		undo()
		return false
	}
	if s := sendsOf(ctx); s != nil {
		s.record(g, g.gen)
	}
	return true
}

// limitFor returns the time no call may start after: bound, else the run's
// deadline (zero for none).
func (g *Gate) limitFor(bound time.Time) time.Time {
	if !bound.IsZero() {
		return bound
	}
	return g.deadline
}

// past refuses a call that would have to wait (the clock reads now) until
// at, after limit (zero: none): ReasonRateLimit when a pause keeps it
// (paused), ReasonDeadline when the budget does. A call that need not wait
// is never refused here: the throttle bounds its waits, and the caller its
// time (the deadline of phase F, the context of a block). Called with g.mu
// held.
func (g *Gate) past(at, now, limit time.Time, paused bool) error {
	if limit.IsZero() || !at.After(limit) || !at.After(now) {
		return nil
	}
	// The run's deadline is binding when limit is not before it (a block's
	// bound may be the deadline and the grace of the write in flight): the
	// run ran out of time, whatever it waited for. A pause past a bound
	// before the deadline outlasts the time a target's writes have.
	byDeadline := !g.deadline.IsZero() && !limit.Before(g.deadline)
	reason, cause, before := ReasonDeadline, "its budget", "the run's deadline"
	switch {
	case paused && byDeadline:
		cause = "a pause for a rate limit"
	case paused:
		reason, cause, before = ReasonRateLimit, "a pause for a rate limit", "the end of the target's writes"
	case !byDeadline:
		before = "the end of the target's writes"
	}
	wait := max(at.Sub(now), 0)
	return &Error{Provider: g.name, Reason: reason, Wait: wait,
		msg: fmt.Sprintf("provider %s: %s leaves no room before %s (a wait of %s)", g.name, cause, before, wait.Round(time.Second))}
}

// wait sleeps d on the Gate's clock.
func (g *Gate) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	return g.sleep(ctx, d)
}

// Block is the writes of one target, which go as one block. Its Meter
// paces them on the Gate, bounded by the block's bound, and counts them.
type Block struct {
	g     *Gate
	bound time.Time
	mu    sync.Mutex
	n     int
}

// Block starts the block of a target that plans to make writes writes. It
// waits out a pause and until the minute and hour limits leave room for
// the block's writes (at most each limit), so that a block rarely waits in
// the middle; it refuses (an *Error) when the provider is out of budget or
// down, or when that wait would end past the run's deadline: no block
// starts after it. The block's calls then have until its bound (Bound).
func (g *Gate) Block(ctx context.Context, writes int) (*Block, error) {
	g.mu.Lock()
	if err := g.refusal(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	now := g.clock()
	at := later(now, g.paused)
	budget := g.minute.earliest(at, writes)
	budget = g.hour.earliest(budget, writes)
	if err := g.past(budget, now, g.deadline, at.After(now) && !budget.After(at)); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	g.stats.Waited += budget.Sub(at)
	g.mu.Unlock()
	if err := g.sleepUntil(ctx, now, budget); err != nil {
		return nil, err
	}
	return &Block{g: g}, nil
}

// Bound sets the time no call of the block may wait past (zero: the run's
// deadline): the time the target's writes have, from when the block
// started (the core gives it min(10 min, the deadline + 30 s)). It must be
// set before the block's first call.
func (b *Block) Bound(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bound = t
}

// limit returns the block's bound.
func (b *Block) limit() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bound
}

// Read is one API read of the block.
func (b *Block) Read(ctx context.Context) error { return b.g.read(ctx, b.limit()) }

// Write is one write of the block.
func (b *Block) Write(ctx context.Context, k Kind) error {
	counted, err := b.g.write(ctx, k, b.limit())
	if counted && err == nil {
		b.mu.Lock()
		b.n++
		b.mu.Unlock()
	}
	return err
}

// Writes returns the writes the block made (reserved and not cancelled).
func (b *Block) Writes() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// window bounds events to n in any span: a sliding log of the times of the
// last n events, in the order they were added. Times are added in order
// (later ensures it), which is what makes the bound hold: when an event is
// added, at most n-1 events lie in the span before it.
//
// A window without a bound (n <= 0) still logs the events of its last
// span: a bound set later counts them. The platform's limits become known
// with its Probe, a call the provider made before them (the scale test
// found GitLab's Probe as the 601st read of a minute whose budget is 600).
type window struct {
	n    int
	span time.Duration
	log  []time.Time
}

// limit sets the window's bound; the events logged stay, as many as the
// new bound keeps.
func (w *window) limit(n int, span time.Duration) {
	w.n, w.span = n, span
	if n > 0 && len(w.log) > n {
		w.log = append([]time.Time(nil), w.log[len(w.log)-n:]...)
	}
}

// earliest returns the earliest time, not before at nor before the last
// event, at which k more events fit (k is taken as n when larger); at for
// a window without limit.
func (w *window) earliest(at time.Time, k int) time.Time {
	if w.n <= 0 {
		return at
	}
	if len(w.log) > 0 {
		at = later(at, w.log[len(w.log)-1])
	}
	k = min(max(k, 1), w.n)
	keep := w.n - k
	if len(w.log) <= keep {
		return at
	}
	return later(at, w.log[len(w.log)-keep-1].Add(w.span))
}

// add records an event at t, which must not be before the last one. Only
// the events of the last span stay: no event before t-span can count
// against a bound, now or set later, since every later event comes at t or
// after; and so an event taken back (remove) leaves the log as if it had
// never been added.
func (w *window) add(t time.Time) {
	i := 0
	for i < len(w.log) && !w.log[i].After(t.Add(-w.span)) {
		i++
	}
	if i > 0 {
		w.log = append(w.log[:0], w.log[i:]...)
	}
	w.log = append(w.log, t)
}

// remove takes back an event at t: a reservation that was not used. The
// order of the log stays.
func (w *window) remove(t time.Time) {
	for i := len(w.log) - 1; i >= 0; i-- {
		if w.log[i].Equal(t) {
			w.log = append(w.log[:i], w.log[i+1:]...)
			return
		}
	}
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// sleep waits d, or until ctx is done (its error).
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
