package distribute

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// fleetFaults answers the calls of one provider of the fleet as a busy
// platform does (fake.Injector). Whether a call fails is a function of the
// seed, the run, the call and how often the run made that call before, so
// a run meets the same faults whatever the order of other providers' calls.
// Faults come after the meter let the call's requests through: a refused
// call was sent, and spent budget.
//
// Rates, in basis points of calls: reads (the reader's calls and
// snapshots) are rate limited with Retry-After 30, without headers 15,
// by GitHub's secondary limits (403) 15; fail with a server error 100 or a
// timeout 50; and 30 go through with headers that say the budget is spent
// until a reset 5 to 30 s away. Writes (pull requests, comments, labels)
// are rate limited with Retry-After 50, without 30, by secondary limits 50;
// fail with a server error before taking effect 100, and lose their answer
// to a timeout after taking effect 50. Minting a target's credential fails
// with a server error 100.
//
// Storms, per run: in run 1 the GitHub listing of pull requests of one
// target fails three times (failed:transient), and GitLab answers
// three writes in a row with 429 (the provider is out of budget for the
// rest of the run); in run 3 GitHub refuses the credentials of three
// targets in a row (the rest of the run is deferred:provider-down).
type fleetFaults struct {
	sp *fleetProv
	// failPRs is the path whose listings of pull requests fail in run 1.
	failPRs string

	mu  sync.Mutex
	run int
	off bool
	// seen counts the calls of the run by method and arguments, count by
	// method.
	seen  map[string]int
	count map[string]int
	// limits are the rate limits the run answered (a pause the provider
	// must honour); injected counts the faults by kind.
	limits   []fleetLimit
	injected map[string]int
}

// fleetLimit is a rate limit a provider answered: no request may reach it
// before at+pause. idx is how many requests its log held when it answered
// (fake.LoggedRequests): every request from idx on came after the answer.
type fleetLimit struct {
	at    time.Time
	pause time.Duration
	what  string
	idx   int
}

func newFleetFaults(sp *fleetProv) *fleetFaults {
	return &fleetFaults{sp: sp, seen: map[string]int{}, count: map[string]int{}, injected: map[string]int{}}
}

// begin starts run n: the counts start over; off turns the faults off.
func (f *fleetFaults) begin(n int, off bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run, f.off = n, off
	f.seen, f.count, f.injected = map[string]int{}, map[string]int{}, map[string]int{}
	f.limits = nil
}

// report returns the run's rate limits and fault counts.
func (f *fleetFaults) report() ([]fleetLimit, map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleetLimit(nil), f.limits...), f.injected
}

// fleetWrites are the calls that write.
var fleetWrites = map[string]bool{"CreatePR": true, "EditPR": true, "Comment": true, "EnsureLabels": true}

// fleetRoll is a number in [0, 10000) for one call of a run.
func fleetRoll(parts ...string) int {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return int(h.Sum64() % 10000)
}

func (f *fleetFaults) inject(ctx context.Context, method string, args []string) (error, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return nil, false
	}
	call := method + " " + strings.Join(args, " ")
	f.seen[call]++
	f.count[method]++
	if fleetWrites[method] {
		f.count["write"]++
	}
	nth := f.seen[call]
	now := f.sp.clock.Now()
	github := f.sp.flavor == fake.GitHub

	// The storms.
	switch {
	case f.run == 1 && github && method == "PRs" && len(args) > 0 && args[0] == f.failPRs && nth <= readAttempts:
		return f.fault("storm:5xx", fleetServerError(method)), false
	case f.run == 1 && !github && fleetWrites[method] && f.count["write"] >= 40 && f.count["write"] < 43:
		return f.limited(ctx, now, "storm:429", method, 0, http.StatusTooManyRequests), false
	case f.run == 3 && github && method == "Target" && f.count["Target"] >= 5 && f.count["Target"] < 8:
		return f.fault("storm:401", &platform.Error{Op: method, Class: platform.ClassAuth, Status: http.StatusUnauthorized,
			Err: errors.New("Bad credentials")}), false
	}

	roll := fleetRoll(strconv.Itoa(fleetSeed), f.sp.id, strconv.Itoa(f.run), call, strconv.Itoa(nth))
	wait := time.Duration(10+roll%81) * time.Second
	switch {
	case method == "Target":
		if roll < 100 {
			return f.fault("5xx", fleetServerError(method)), false
		}
	case fleetWrites[method]:
		switch {
		case roll < 50:
			return f.limited(ctx, now, "429+retry-after", method, wait, http.StatusTooManyRequests), false
		case roll < 80:
			return f.limited(ctx, now, "429", method, 0, http.StatusTooManyRequests), false
		case roll < 130 && github:
			return f.limited(ctx, now, "secondary", method, fleetSecondaryWait(roll), http.StatusForbidden), false
		case roll < 230:
			return f.fault("5xx", fleetServerError(method)), false
		case roll < 280:
			return f.fault("lost", fleetTimeout(method)), true
		}
	default:
		switch {
		case roll < 30:
			return f.limited(ctx, now, "429+retry-after", method, wait, http.StatusTooManyRequests), false
		case roll < 45:
			return f.limited(ctx, now, "429", method, 0, http.StatusTooManyRequests), false
		case roll < 60 && github:
			return f.limited(ctx, now, "secondary", method, fleetSecondaryWait(roll), http.StatusForbidden), false
		case roll < 160:
			return f.fault("5xx", fleetServerError(method)), false
		case roll < 210:
			return f.fault("timeout", fleetTimeout(method)), false
		case roll < 240:
			f.spent(ctx, now, time.Duration(5+roll%26)*time.Second)
		}
	}
	return nil, false
}

// fault counts an injected fault of kind and returns err.
func (f *fleetFaults) fault(kind string, err error) error {
	f.injected[kind]++
	return err
}

// limited answers a call with a rate limit (status 429, or 403 for
// GitHub's secondary limits) asking to wait retryAfter (zero: no header),
// and records the pause it starts: Retry-After, else at least the
// throttle's first pause.
func (f *fleetFaults) limited(ctx context.Context, now time.Time, kind, method string, retryAfter time.Duration, status int) error {
	pause := retryAfter
	if pause <= 0 {
		pause = throttle.FirstPause
	}
	idx, _ := fake.LoggedRequests(ctx)
	f.limits = append(f.limits, fleetLimit{at: now, pause: pause, what: kind + " " + method, idx: idx})
	msg := "API rate limit exceeded"
	if status == http.StatusForbidden {
		msg = "You have exceeded a secondary rate limit"
	}
	return f.fault(kind, &platform.Error{Op: method, Class: platform.ClassRateLimited, Status: status, RetryAfter: retryAfter,
		Err: errors.New(msg)})
}

// spent lets a call through with an answer whose headers say the budget
// is spent until reset (GitHub's x-ratelimit-*, GitLab's RateLimit-*),
// and records the pause the throttle takes from them.
func (f *fleetFaults) spent(ctx context.Context, now time.Time, reset time.Duration) {
	prefix := "X-RateLimit-"
	if f.sp.flavor == fake.GitLab {
		prefix = "RateLimit-"
	}
	at := now.Add(reset).Unix()
	h := http.Header{}
	h.Set(prefix+"Remaining", "0")
	h.Set(prefix+"Reset", strconv.FormatInt(at, 10))
	throttle.Observe(ctx, h)
	idx, _ := fake.LoggedRequests(ctx)
	f.limits = append(f.limits, fleetLimit{at: now, pause: max(time.Unix(at, 0).Sub(now), time.Second), what: "spent", idx: idx})
	f.injected["spent"]++
}

// fleetSecondaryWait is the Retry-After of a secondary limit: a minute for
// half of them, none for the rest.
func fleetSecondaryWait(roll int) time.Duration {
	if roll%2 == 0 {
		return time.Minute
	}
	return 0
}

// fleetServerError is a 502 or 503 of method.
func fleetServerError(method string) error {
	return &platform.Error{Op: method, Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
}

// fleetTimeout is a request of method that timed out before its answer
// came (net/http's client timeout).
func fleetTimeout(method string) error {
	return &platform.Error{Op: method, Class: platform.ClassTransient,
		Err: fmt.Errorf("%s: net/http: request canceled (Client.Timeout exceeded while awaiting headers)", method)}
}
