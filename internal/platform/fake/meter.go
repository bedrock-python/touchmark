package fake

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// The fake asks the internal/throttle Meter of each call's context for its
// requests, as a driver's HTTP client does: every call of
// the Reader is one read, and every write method as many writes as its
// flavor's driver sends HTTP requests that write. A request the Meter
// refuses is not made: the call fails with that refusal before the fake
// logs it or changes anything. Snapshots (git), Target (a minted token) and
// Close write nothing the budget counts.
//
// With WithRequestLog the fake also keeps every request the meter let
// through (Requests), stamped with the platform's clock: what a platform
// would have received, refused ones included (an injected fault fails a
// call after its requests went out). A test measures the throttle against
// it: a request made without a Meter in its context is logged too.

// unmetered are the calls that are no API read: the writes (metered by
// metered) and those the budget does not count.
var unmetered = map[string]bool{
	"CreatePR": true, "EditPR": true, "Comment": true, "EnsureLabels": true, "Commit": true,
	"Target": true, "Snapshot": true,
}

// RequestKind is what an API request does, as the budget counts it.
type RequestKind uint8

// Kinds of requests.
const (
	// RequestRead is an API read.
	RequestRead RequestKind = iota
	// RequestWrite is an API write other than a comment.
	RequestWrite
	// RequestComment is a comment on a pull request.
	RequestComment
)

// String names the kind.
func (k RequestKind) String() string {
	switch k {
	case RequestRead:
		return "read"
	case RequestWrite:
		return "write"
	case RequestComment:
		return "comment"
	}
	return fmt.Sprintf("RequestKind(%d)", k)
}

// Request is one API request the platform received (WithRequestLog).
type Request struct {
	// At is the platform's clock when the request arrived (WithClock).
	At time.Time
	// Method is the call that made it ("ReadFile", "CreatePR", …).
	Method string
	Kind   RequestKind
}

// WithRequestLog keeps every API request the platform receives, with the
// time of the platform's clock (Requests). The clock is read once per
// request, so set one with WithClock that a test controls: the default
// clock advances a second at every reading.
func WithRequestLog() Option {
	return func(p *Platform) { p.logRequests = true }
}

// Requests returns the API requests the platform received, in the order
// they arrived (WithRequestLog; empty without it).
func (p *Platform) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// loggedKey carries, in the context an Injector gets, how many requests
// the log held when the call's fault was decided (LoggedRequests).
type loggedKey struct{}

// LoggedRequests returns, in the context an Injector gets, how many
// requests the platform's log held when the injector was asked: the call's
// own requests included, those made after its answer not (WithRequestLog).
// A test checks by it what reached the platform after a rate limit it
// answered, whatever the clock read: a request sent at once carries the
// limit's own time.
func LoggedRequests(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(loggedKey{}).(int)
	return n, ok
}

// ResetRequests empties the log of requests.
func (p *Platform) ResetRequests() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = nil
}

// logRequest adds a request of method to the log (WithRequestLog).
func (p *Platform) logRequest(method string, k RequestKind) {
	if !p.logRequests {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, Request{At: p.now(), Method: method, Kind: k})
}

// meterRead asks the Meter of ctx for the read of method.
func (p *Platform) meterRead(ctx context.Context, method string) error {
	if unmetered[method] {
		return nil
	}
	if err := throttle.Read(ctx); err != nil {
		return fmt.Errorf("%s: %w", ops[method], err)
	}
	p.logRequest(method, RequestRead)
	return nil
}

// meterWrites asks the Meter of ctx for n writes of kind k by method.
func (p *Platform) meterWrites(ctx context.Context, method string, k throttle.Kind, n int) error {
	kind := RequestWrite
	if k == throttle.Comment {
		kind = RequestComment
	}
	for range n {
		if err := throttle.Write(ctx, k); err != nil {
			return fmt.Errorf("%s: %w", ops[method], err)
		}
		p.logRequest(method, kind)
	}
	return nil
}

// missingLabels counts the names the repository of t has no label for.
func (t *target) missingLabels(names []string) int {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	s := t.p.repos[t.repoID]
	if s == nil {
		return 0
	}
	n := 0
	for _, name := range dedupe(names) {
		if _, ok := s.labels[name]; !ok {
			n++
		}
	}
	return n
}

// createWrites is how many writes the flavor's driver sends to open np,
// before the pull request is known to exist (first) and after (then): the
// pull request; on GitHub then each missing label and the call that puts
// the labels on it; on Gitea and Forgejo first each missing label (the
// pull request takes their ids); on GitLab nothing more (labels by name).
func (t *target) createWrites(np platform.NewPR) (first, then int) {
	switch Flavor(t.p.caps.Flavor) {
	case GitLab:
		return 1, 0
	case Gitea, Forgejo:
		return 1 + t.missingLabels(np.Labels), 0
	}
	if len(np.Labels) == 0 {
		return 1, 0
	}
	return 1, 1 + t.missingLabels(np.Labels)
}

// editWrites is how many writes the flavor's driver sends for e, before
// the edit is known to apply (first) and after (then): the edit, on Gitea
// and Forgejo after a state or base change in a request of its own, on
// Bitbucket followed by the decline of a close; then labels added in a call
// of their own but on GitLab, after each missing one is created.
func (t *target) editWrites(e platform.PREdit) (first, then int) {
	first = 1
	flavor := Flavor(t.p.caps.Flavor)
	if (flavor == Gitea || flavor == Forgejo) && (e.State != nil || e.Base != nil) && (e.Title != nil || e.Body != nil) {
		first++
	}
	if flavor == Bitbucket && e.State != nil && (e.Title != nil || e.Body != nil || e.Base != nil) {
		first++
	}
	if len(e.AddLabels) > 0 && flavor != GitLab {
		then = 1 + t.missingLabels(e.AddLabels)
	}
	return first, then
}
