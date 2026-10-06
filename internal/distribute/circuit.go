package distribute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// Reads, their retries and the provider's throttle.
// Every call of the run to a platform or a target's git server that reads
// (phases B, C and D, the recheck of phase F, the per-target write
// credential) goes through retry: a transient failure is tried again after
// a backoff, and a rate limit pauses the provider (throttle.Gate) and tries
// again once the pause is over, until the provider is out of budget. What a
// provider's failures say about the provider itself (a rate limit, a
// streak of refused credentials or of plain 403s) is kept in its Gate,
// which phases B to F share: a provider that limited the rate three times in
// a row, or refused the credential for three targets in a row, gets no more
// calls in this run (deferred:rate-limit, deferred:provider-down).

// readAttempts is how often a read that failed transiently is tried.
const readAttempts = 3

// limitedAttempts bounds the attempts of one call that the platform keeps
// rate limiting: the Gate puts the provider out of budget after
// throttle.Strikes limits in a row, which ends the call before this bound.
const limitedAttempts = throttle.Strikes + 1

// retry runs call, a read of provider p, until it goes through: again after
// 1 and 2 s (with jitter) when it fails transiently, three attempts in all;
// again after the pause the provider's Gate starts when it is rate limited.
// Any other error, a call the throttle refused, and the end of ctx end it at
// once; call classifies git failures itself (gitFailure). The error of the
// last attempt is returned, joined with the throttle's refusal when that
// ended it.
func (r *run) retry(ctx context.Context, p *provider, call func() error) error {
	return retryRead(ctx, p.throttle(), r.sleep, r.jitter, time.Time{}, call)
}

// gitRetry is retry for a git fetch of provider p: each attempt holds one of
// the provider's git slots (throttle.Limits.GitReads).
func (r *run) gitRetry(ctx context.Context, p *provider, call func() error) error {
	g := p.throttle()
	return r.retry(ctx, p, func() error {
		release, err := g.Git(ctx)
		if err != nil {
			return err
		}
		defer release()
		return call()
	})
}

// retryRead is the loop of retry, with the waits of sleep and jitter, and
// the pauses of g, none of which may end after bound (zero: the run's
// deadline).
func retryRead(ctx context.Context, g *throttle.Gate, sleep sleepFunc, jitter func(time.Duration) time.Duration, bound time.Time, call func() error) error {
	var err error
	transient, limited := 0, 0
	for {
		if werr := g.Wait(ctx, bound); werr != nil {
			return joinRefusal(werr, err)
		}
		tk := g.TicketFor(ctx, throttle.ReadCall)
		err = call()
		switch {
		case err == nil:
			g.Passed(tk)
			return nil
		case ctx.Err() != nil:
			return err
		}
		if _, refused := throttle.Refused(err); refused {
			return err
		}
		switch platform.ClassOf(err) {
		case platform.ClassRateLimited:
			g.Limited(tk, retryAfterOf(err))
			if limited++; limited >= limitedAttempts {
				return err
			}
		case platform.ClassTransient:
			if transient++; transient >= readAttempts {
				return err
			}
			if sleep(ctx, jitter(retryDelays[transient-1])) != nil {
				return err
			}
		default:
			return err
		}
	}
}

// joinRefusal returns the throttle's refusal of a call whose last attempt
// failed with last (nil when none was made), on one line: both stay in the
// error's tree.
func joinRefusal(refusal, last error) error {
	if last == nil {
		return refusal
	}
	return fmt.Errorf("%w; the last answer: %w", refusal, last)
}

// retryAfterOf returns the wait a rate-limited platform error asks for
// (from Retry-After or the reset headers), zero when it names none.
func retryAfterOf(err error) time.Duration {
	var pe *platform.Error
	for e := err; errors.As(e, &pe); e = pe.Err {
		if pe.RetryAfter > 0 {
			return pe.RetryAfter
		}
	}
	return 0
}

// newGate returns the throttle of provider cfg for run r: the limits of
// hub.yml and of the run's concurrency until Probe tells the platform's
// (limitsOf), the run's clock, and the run's deadline.
func (r *run) newGate(cfg config.ResolvedProvider) *throttle.Gate {
	clock, ok := r.d.clocks[cfg.ID]
	if !ok {
		clock = throttle.Clock{Now: r.clock, Sleep: r.sleep}
	}
	return throttle.New(throttle.Options{
		Name:     cfg.ID,
		Limits:   limitsOf(platform.Limits{}, cfg.Limits, r.d.Concurrency),
		Clock:    clock,
		Deadline: r.d.Write.Deadline,
		// GitLab bans an IP with a 403 to every request.
		WatchForbidden: cfg.Type == "gitlab",
	})
}

// timeOf reads the time of provider p: its own clock when a test gave it
// one (Deps.clocks), else the run's.
func (r *run) timeOf(p *provider) time.Time {
	if p != nil && p.clock != nil {
		return p.clock()
	}
	return r.clock()
}

// throttle returns the provider's Gate: one without limits, on the real
// clock, for a provider a test built without one.
func (p *provider) throttle() *throttle.Gate {
	p.gateOnce.Do(func() {
		if p.gate == nil {
			p.gate = throttle.New(throttle.Options{Name: p.cfg.ID, WatchForbidden: p.cfg.Type == "gitlab"})
		}
	})
	return p.gate
}

// deferral returns why provider p gets no more calls in this run ("" when
// it may be called), and a warning saying so.
func (p *provider) deferral() (reason, why string) {
	return p.throttle().Deferral()
}

// failed records what a failed call of a target tells about provider p: an
// auth error extends the streak of refused credentials, a plain 403 the
// streak of refusals (plainForbidden), and any other failure ends that
// one. Rate limits are the retries' to record (retry, retryWrite).
func (p *provider) failed(err error) {
	g := p.throttle()
	if platform.ClassOf(err) == platform.ClassAuth {
		g.AuthFailed()
	}
	g.Forbidden(plainForbidden(err))
}

// succeeded records that the credential worked: the streaks of auth
// failures and of refusals end.
func (p *provider) succeeded() { p.throttle().Healthy() }

// settle records how a target of phase F ended: an auth failure extends the
// streak, a deferral that says nothing about the credential (the provider
// down or out of budget already, the end of the run) keeps it, and so does
// a failure of access (perhaps a 403: the report does not tell it from a
// 404); anything else ends it.
func (p *provider) settle(res *report.DeliveryTarget) {
	g := p.throttle()
	switch {
	case res.Outcome == report.OutcomeFailed && res.Reason == "auth":
		g.AuthFailed()
	case res.Outcome == report.OutcomeDeferred:
	case res.Outcome == report.OutcomeFailed && res.Reason == "access":
	default:
		g.Healthy()
	}
}

// plainForbidden reports whether err is a refusal by permission that names
// no rule: a 403 of the API or of git that says nothing about what is
// missing, as a ban answers every request. A refusal the driver explains
// (a Rule: the role, archived, workflows) is one target's.
func plainForbidden(err error) bool {
	var pe *platform.Error
	return platform.ClassOf(err) == platform.ClassPermission && errors.As(err, &pe) && pe.Rule == ""
}

// deferredByCircuit ends t, whose provider gets no more calls, as deferred
// without a platform call and reports whether it did.
func (r *run) deferredByCircuit(t *target) bool {
	reason, why := t.prov.deferral()
	if reason == "" {
		return false
	}
	t.res.Outcome, t.res.Reason = report.OutcomeDeferred, reason
	r.targetWarning(t, why)
	return true
}

// sleepFunc waits d or until ctx is done (its error).
type sleepFunc func(ctx context.Context, d time.Duration) error

// Streaks of the breaker, as the Gate counts them.
const (
	authStreak = throttle.AuthStreak
	banStreak  = throttle.BanStreak
)
