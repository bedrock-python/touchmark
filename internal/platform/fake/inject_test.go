package fake_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// testClock is a clock the test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return ctx.Err()
}

// The request log keeps every API request the meter let through, stamped
// with the platform's clock: one read per reader call, the writes of each
// write method as its flavor's driver sends them, comments apart; snapshots
// and minted tokens are no requests. A request the meter refuses is not
// made, and is not logged.
func TestRequestLog(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clock := &testClock{now: fake.Epoch}
	e := newEnv(t, fake.WithClock(clock.Now), fake.WithRequestLog())
	r := e.repo("acme/api", "README.md", "x")
	if _, err := e.p.Reader(e.reader).ReadFile(ctx, r, "", "README.md", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, ""); err != nil {
		t.Fatal(err)
	}
	g := throttle.New(throttle.Options{Name: "gh", Limits: throttle.Limits{MinInterval: time.Second},
		Clock: throttle.Clock{Now: clock.Now, Sleep: clock.Sleep}})
	tw := e.target(r)
	wctx := throttle.With(ctx, g)
	pr, err := tw.CreatePR(wctx, platform.NewPR{Head: "topic", Base: "main", Title: "t", Body: "b", Labels: []string{"engineering-assets"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Comment(wctx, pr.Number, "hello"); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		at     time.Duration
		method string
		kind   fake.RequestKind
	}
	var got []entry
	for _, q := range e.p.Requests() {
		got = append(got, entry{q.At.Sub(fake.Epoch), q.Method, q.Kind})
	}
	// GitHub opens a pull request with labels in three writes: the pull
	// request, the missing label, the call that puts it on; a second apart.
	want := []entry{
		{0, "ReadFile", fake.RequestRead},
		{0, "CreatePR", fake.RequestWrite},
		{time.Second, "CreatePR", fake.RequestWrite},
		{2 * time.Second, "CreatePR", fake.RequestWrite},
		{3 * time.Second, "Comment", fake.RequestComment},
	}
	if !slices.Equal(got, want) {
		t.Errorf("requests %+v, want %+v", got, want)
	}
	// A refused request is not made.
	ctxDone, cancel := context.WithCancel(ctx)
	cancel()
	e.p.ResetRequests()
	if err := tw.Comment(throttle.With(ctxDone, g), pr.Number, "again"); err == nil {
		t.Error("a comment went through a cancelled meter")
	}
	if got := e.p.Requests(); len(got) != 0 {
		t.Errorf("refused requests logged: %+v", got)
	}
	// Without the option nothing is logged.
	plain := newEnv(t)
	pr2 := plain.repo("acme/web", "README.md", "x")
	if _, err := plain.p.Reader(plain.reader).Repo(ctx, pr2.Path); err != nil {
		t.Fatal(err)
	}
	if got := plain.p.Requests(); len(got) != 0 {
		t.Errorf("requests logged without WithRequestLog: %+v", got)
	}
}

// An injector fails the calls it chooses after the meter let them through,
// after the faults of FailNext and Fail; an applied fault lets the call
// take effect first.
func TestInject(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t, fake.WithRequestLog())
	r := e.repo("acme/api", "README.md", "x")
	n := e.pr(r, platform.PR{Head: "topic", Author: e.writer, Title: "t", Body: "b"})
	busy := &platform.Error{Op: "list pull requests", Class: platform.ClassRateLimited, Status: 429, RetryAfter: time.Minute}
	lost := &platform.Error{Op: "edit pull request", Class: platform.ClassTransient, Err: errors.New("timeout")}
	var seen []string
	e.p.Inject(func(_ context.Context, method string, args []string) (error, bool) {
		seen = append(seen, method+" "+strings.Join(args, " "))
		switch method {
		case "PRs":
			return busy, false
		case "EditPR":
			return lost, true
		}
		return nil, false
	})
	rd := e.p.Reader(e.reader)
	if _, err := rd.PRs(ctx, r, nil, nil); !errors.Is(err, busy) {
		t.Errorf("PRs: %v, want the injected limit", err)
	}
	if got := e.p.Requests(); len(got) != 1 || got[0].Method != "PRs" {
		t.Errorf("the call the injector failed was sent, one request: %+v", got)
	}
	title := "new title"
	if _, err := e.target(r).EditPR(ctx, n, platform.PREdit{Title: &title}); !errors.Is(err, lost) {
		t.Errorf("EditPR: %v, want the injected timeout", err)
	}
	if got := e.p.PR(r.ID, n).Title; got != title {
		t.Errorf("an applied fault left the title %q", got)
	}
	// FailNext comes first; the injector is asked when no other fault fires.
	first := errors.New("first")
	e.p.FailNext("PRs", first)
	if _, err := rd.PRs(ctx, r, nil, nil); !errors.Is(err, first) {
		t.Errorf("PRs: %v, want the queued fault", err)
	}
	if want := []string{"PRs acme/api", "Target acme/api", "EditPR acme/api #1"}; !slices.Equal(seen, want) {
		t.Errorf("the injector saw %q, want %q", seen, want)
	}
	e.p.Inject(nil)
	if _, err := rd.PRs(ctx, r, nil, nil); err != nil {
		t.Errorf("PRs without an injector: %v", err)
	}
}
