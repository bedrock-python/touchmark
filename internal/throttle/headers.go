package throttle

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A platform that answers a call also tells how much of its budget is
// left: GitHub in x-ratelimit-remaining and x-ratelimit-reset,
// GitLab in RateLimit-Remaining, RateLimit-Reset and RateLimit-ResetTime,
// Codeberg in the IETF draft's RateLimit ("…;r=0;t=30"). An answer that
// shows none left pauses the provider until the budget resets, without a
// strike: the next call would be refused. internal/httpx tells the Meter
// of a request's context the headers of every answer (Observe).

// unixCutoff tells a reset given as a Unix time from one given in seconds
// to wait: a number of seconds never reaches it.
const unixCutoff = 1_000_000_000

// Observe tells the Meter of ctx the headers of an answer (a Gate or a
// Block of one pauses its provider when they show no budget left); nothing
// without a Meter, or for requests marked Uncounted.
func Observe(ctx context.Context, h http.Header) {
	m := meterOf(ctx)
	if m == nil || h == nil || markOf(ctx) == markUncounted {
		return
	}
	if o, ok := m.(interface{ observe(http.Header) }); ok {
		o.observe(h)
	}
}

// observe pauses the provider until its budget resets when h shows none
// left.
func (g *Gate) observe(h http.Header) {
	g.mu.Lock()
	now := g.clock()
	g.mu.Unlock()
	if d, ok := exhausted(h, now); ok {
		g.Pause(d)
	}
}

func (b *Block) observe(h http.Header) { b.g.observe(h) }

// exhausted reports whether h shows no budget left, and how long until it
// resets (at least a second; ok is false when the reset is unknown).
func exhausted(h http.Header, now time.Time) (time.Duration, bool) {
	if r, ok := draftParam(h.Get("RateLimit"), "r"); ok && r == 0 {
		if t, ok := draftParam(h.Get("RateLimit"), "t"); ok {
			return atLeastSecond(time.Duration(t) * time.Second), true
		}
	}
	for _, prefix := range []string{"X-RateLimit-", "RateLimit-"} {
		if strings.TrimSpace(h.Get(prefix+"Remaining")) != "0" {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"Reset")), 10, 64); err == nil && n >= 0 {
			if n >= unixCutoff {
				return atLeastSecond(time.Unix(n, 0).Sub(now)), true
			}
			return atLeastSecond(time.Duration(n) * time.Second), true
		}
		if t, err := http.ParseTime(strings.TrimSpace(h.Get(prefix + "ResetTime"))); err == nil {
			return atLeastSecond(t.Sub(now)), true
		}
	}
	return 0, false
}

// atLeastSecond returns d, but a second at least: a reset in the past or
// in the same second still needs the platform's clock to turn.
func atLeastSecond(d time.Duration) time.Duration {
	return max(d, time.Second)
}

// draftParam returns the integer parameter name ("r", "t") of the IETF
// draft's RateLimit header: the first policy item that has it, such as
// `"default";r=0;t=30`.
func draftParam(v, name string) (int64, bool) {
	for item := range strings.SplitSeq(v, ",") {
		for p := range strings.SplitSeq(item, ";") {
			key, val, found := strings.Cut(strings.TrimSpace(p), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), name) {
				continue
			}
			n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(val), `"`), 10, 64)
			if err == nil && n >= 0 {
				return n, true
			}
		}
	}
	return 0, false
}
