package distribute

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// TestRunThrottled: distribute end to end on the fake in git mode, paced by
// hub.yml's limits for its provider, on a fake clock. A rate limit of a pull
// request (Retry-After 30 s) and one of a push (no headers: a minute) pause
// the provider and the writes are made again. The API requests the platform
// received keep the limits: no minute holds more writes than the limit, and
// two writes are never closer than the minimum interval (the pushes reach
// the git server, which logs no times: they show in the gaps between the
// pull requests, four writes of a target apart). Every target opens its pull
// request, the report counts the HTTP writes, and a second run writes
// nothing.
func TestRunThrottled(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(fake.Epoch)
	w := newGitWorld(t, fake.WithClock(clock.Now), fake.WithRequestLog())
	w.hubYML = defaultHubYML + "    limits: { writes_per_minute: 6, min_interval: 2s }\n"
	var repos []platform.Repo
	for i := range 6 {
		repos = append(repos, w.optedIn(fmt.Sprintf("acme/t%d", i), nil))
	}
	w.p.FailNext("CreatePR", &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests,
		RetryAfter: 30 * time.Second, Err: errors.New("slow down")})
	w.p.FailNext("Push", exHTTPErr(platform.ClassRateLimited, http.StatusTooManyRequests))
	d := w.deps(ModeDistribute)
	d.Now, d.sleep = clock.Now, clock.Sleep
	start := clock.Now()
	rep := w.run(d, ModeDistribute)
	var created []time.Time
	sum := 0
	for _, r := range repos {
		tg := want(t, rep, "gh:"+r.Path, report.OutcomeOpened, "", 1)
		// The push, the pull request, its label created at first use, and
		// the call that puts it on; the refused push and pull request count
		// for the target they were made again for.
		if tg.Writes < 4 {
			t.Errorf("%s: %d writes", r.Path, tg.Writes)
		}
		sum += tg.Writes
		created = append(created, w.p.PR(r.ID, 1).CreatedAt)
	}
	if want := 6*4 + 2; sum != want || rep.Cost["gh"] != sum {
		t.Errorf("the targets wrote %d, cost %d; want %d", sum, rep.Cost["gh"], want)
	}
	slices.SortFunc(created, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(created); i++ {
		// Four writes a target, two seconds apart at least.
		if gap := created[i].Sub(created[i-1]); gap < 3*2*time.Second {
			t.Errorf("pull requests %d and %d opened %v apart", i-1, i, gap)
		}
	}
	// 24 writes at 6 a minute take over three minutes, and the pauses add a
	// minute and a half.
	if took := clock.Now().Sub(start); took < 3*time.Minute+90*time.Second {
		t.Errorf("the run took %v", took)
	}
	if !hasWarning(rep.Warnings, "provider gh paused 2 times for rate limits (1m30s in all)") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	checkFleetBudget(t, 1, "gh", w.p.Requests(), throttle.Limits{WritesPerMinute: 6, MinInterval: 2 * time.Second}, start.Add(24*time.Hour))
	rep = w.run(d, ModeDistribute)
	for _, r := range repos {
		want(t, rep, "gh:"+r.Path, report.OutcomeUnchanged, "", 1)
	}
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("the second run wrote %q", writes)
	}
}
