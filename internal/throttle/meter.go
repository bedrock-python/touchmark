package throttle

import (
	"context"
	"net/http"
	"sync"
)

// Meter paces and counts the calls made under a context: a Gate for a
// provider's reads, a Block for one target's writes. Read and Write wait
// until the call may be sent and refuse it (an *Error, or the end of ctx)
// when it may not; a refused call must not be sent.
type Meter interface {
	Read(ctx context.Context) error
	Write(ctx context.Context, k Kind) error
}

type (
	meterKey struct{}
	markKey  struct{}
	sendsKey struct{}
)

// mark says what the requests under a context are, beyond their method.
type mark uint8

const (
	unmarked mark = iota
	// markRead: a request that only reads, whatever its method (a GraphQL
	// query is a POST).
	markRead
	// markUncounted: requests that spend no budget of the platform's
	// writes (a GitHub App's installation tokens, minted and revoked).
	markUncounted
	// markComment: the write is a comment.
	markComment
)

// With returns ctx with m as its Meter, and a record of the requests a
// Gate lets out under it (Gate.TicketFor).
func With(ctx context.Context, m Meter) context.Context {
	return context.WithValue(context.WithValue(ctx, meterKey{}, m), sendsKey{}, &sends{})
}

// Default returns ctx with m as its Meter, unless ctx carries one already:
// the reads a target's block makes on the way (a re-inspection) stay the
// block's.
func Default(ctx context.Context, m Meter) context.Context {
	if meterOf(ctx) != nil {
		return ctx
	}
	return With(ctx, m)
}

// meterOf returns the Meter of ctx, nil for none.
func meterOf(ctx context.Context) Meter {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(meterKey{}).(Meter)
	return m
}

// sends records the latest request a Gate let out under a context's Meter
// (With): a call's rate limit belongs to the pauses that began before its
// request went out, not before the call started, since the call may have
// waited in the Meter for a pause or for its budget.
type sends struct {
	mu  sync.Mutex
	seq uint64
	gen uint64
	g   *Gate
}

// sendsOf returns the record of ctx, nil for none.
func sendsOf(ctx context.Context) *sends {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sendsKey{}).(*sends)
	return s
}

// record notes a request g let out at its generation gen.
func (s *sends) record(g *Gate, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.gen, s.g = gen, g
}

// current returns how many requests were let out so far.
func (s *sends) current() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// since returns the generation of g when the latest request was let out,
// if it was g's and came after the seq-th.
func (s *sends) since(g *Gate, seq uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq <= seq || s.g != g {
		return 0, false
	}
	return s.gen, true
}

func markOf(ctx context.Context) mark {
	if ctx == nil {
		return unmarked
	}
	m, _ := ctx.Value(markKey{}).(mark)
	return m
}

// AsRead marks the requests under ctx as reads, whatever their method: a
// GraphQL query.
func AsRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, markKey{}, markRead)
}

// Uncounted marks the requests under ctx as spending no write budget: a
// GitHub App's installation tokens, whose minting and revocation spend the
// App's limit of minted tokens instead.
func Uncounted(ctx context.Context) context.Context {
	return context.WithValue(ctx, markKey{}, markUncounted)
}

// AsComment marks the writes under ctx as comments.
func AsComment(ctx context.Context) context.Context {
	return context.WithValue(ctx, markKey{}, markComment)
}

// Read asks the Meter of ctx for one API read; nil without a Meter, or for
// requests marked Uncounted.
func Read(ctx context.Context) error {
	m := meterOf(ctx)
	if m == nil || markOf(ctx) == markUncounted {
		return nil
	}
	return m.Read(ctx)
}

// Write asks the Meter of ctx for one write of kind k (a comment when ctx
// is marked AsComment); a read for requests marked AsRead; nil without a
// Meter, or for requests marked Uncounted.
func Write(ctx context.Context, k Kind) error {
	m := meterOf(ctx)
	if m == nil {
		return nil
	}
	switch markOf(ctx) {
	case markUncounted:
		return nil
	case markRead:
		return m.Read(ctx)
	case markComment:
		if k == API {
			k = Comment
		}
	}
	return m.Write(ctx, k)
}

// Request asks the Meter of ctx for one HTTP request with method: GET, HEAD
// and OPTIONS read, every other method writes through the API
// (internal/httpx calls it before sending).
func Request(ctx context.Context, method string) error {
	switch method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
		return Read(ctx)
	}
	return Write(ctx, API)
}
