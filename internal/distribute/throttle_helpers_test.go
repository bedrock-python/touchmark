package distribute

import (
	"context"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/throttle"
)

// exhaust puts p's Gate out of budget, as throttle.Strikes rate limits in
// a row do.
func exhaust(p *provider) {
	g := p.throttle()
	for range throttle.Strikes {
		g.Limited(g.Ticket(throttle.ReadCall), 0)
	}
}

// fakeClock is a clock that moves only when something sleeps on it: Sleep
// advances it by the wait and returns at once, so a run paced by its Gates
// takes no real time and every wait is recorded.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

// Now reads the clock.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep advances the clock by d (unless ctx is done) and records it.
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.now = c.now.Add(d)
		c.slept = append(c.slept, d)
	}
	return nil
}

// Advance moves the clock by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Slept returns the waits recorded so far.
func (c *fakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}
