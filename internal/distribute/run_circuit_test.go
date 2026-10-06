package distribute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// What a provider's failures tell the run: transient ones are tried again, a
// rate limit or a credential refused for three targets in a row stops the
// provider for the rest of the run, and a failure to fetch the history the
// proofs need is no proof that the branch was edited.

var (
	errTransient = &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	errLimited   = &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests, Err: errors.New("slow down")}
)

// countCalls counts the calls of the fake's log that start with prefix.
func countCalls(p interface{ Calls() []string }, prefix string) int {
	n := 0
	for _, c := range p.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// A read that fails transiently once is tried again and goes through; one
// that keeps failing fails its target after three attempts. One 502 of any
// read once failed the target (exit 1).
func TestRunRetriesReads(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/x", nil)
	w.p.FailNext("ReadFile", errTransient)
	w.p.FailNext("Probe", errTransient)
	rep := w.plan(w.deps())
	want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 0)
	if n := countCalls(w.p, "ReadFile"); n != 2 {
		t.Errorf("%d reads of the opt-in file, want 2", n)
	}
	w.p.ResetCalls()
	transientTimes(w.p, "ReadFile", errTransient, readAttempts)
	rep = w.plan(w.deps())
	want(t, rep, "gh:acme/x", report.OutcomeFailed, "transient", 0)
	if n := countCalls(w.p, "ReadFile"); n != readAttempts {
		t.Errorf("%d reads of the opt-in file, want %d", n, readAttempts)
	}
}

// Rate limits in a row stop the provider (one pauses it, three in a row put
// it out of budget), and so do auth failures of three targets in a row: its
// remaining targets are deferred without another call. Phase C once kept
// calling a provider that limited the rate or refused the credential for
// every target.
func TestRunCircuit(t *testing.T) {
	t.Run("rate-limit", func(t *testing.T) {
		w := newWorld(t)
		for i := range 5 {
			w.optedIn(fmt.Sprintf("acme/r%d", i), nil)
		}
		transientTimes(w.p, "PRs", errLimited, throttle.Strikes)
		d := w.deps()
		d.Concurrency = 1
		rep := w.plan(d)
		for i := range 5 {
			tg := want(t, rep, fmt.Sprintf("gh:acme/r%d", i), report.OutcomeDeferred, "rate-limit", 0)
			if !hasWarningLike(tg, "provider gh limited the rate 3 times in a row") {
				t.Errorf("r%d: warnings %q", i, tg.Warnings)
			}
		}
		if n := countCalls(w.p, "ReadFile"); n != 1 {
			t.Errorf("%d reads after the rate limit, want the first target's only", n)
		}
	})
	t.Run("one rate limit pauses", func(t *testing.T) {
		w := newWorld(t)
		for i := range 3 {
			w.optedIn(fmt.Sprintf("acme/p%d", i), nil)
		}
		w.p.FailNext("PRs", errLimited)
		clock := newFakeClock(fake.Epoch)
		d := w.deps()
		d.Concurrency = 1
		d.Now, d.sleep = clock.Now, clock.Sleep
		rep := w.plan(d)
		for i := range 3 {
			want(t, rep, fmt.Sprintf("gh:acme/p%d", i), report.OutcomeOpened, "", 0)
		}
		if n := countCalls(w.p, "PRs"); n != 4 {
			t.Errorf("%d listings, want the limited one again and one per target", n)
		}
		if got := clock.Slept(); !slices.Contains(got, throttle.FirstPause) {
			t.Errorf("waits %v, want the pause of a rate limit without headers (%v)", got, throttle.FirstPause)
		}
		if !hasWarning(rep.Warnings, "provider gh paused once for rate limits (1m0s in all)") {
			t.Errorf("warnings %q", rep.Warnings)
		}
	})
	t.Run("auth", func(t *testing.T) {
		w := newWorld(t)
		for i := range 5 {
			w.optedIn(fmt.Sprintf("acme/a%d", i), nil)
		}
		w.p.Fail("ReadFile", errAuth)
		d := w.deps()
		d.Concurrency = 1
		rep := w.plan(d)
		for i := range 5 {
			outcome, reason := report.OutcomeFailed, "auth"
			if i >= authStreak {
				outcome, reason = report.OutcomeDeferred, "provider-down"
			}
			want(t, rep, fmt.Sprintf("gh:acme/a%d", i), outcome, reason, 0)
		}
		if n := countCalls(w.p, "ReadFile"); n != authStreak {
			t.Errorf("%d refused reads, want %d", n, authStreak)
		}
	})
	// GitLab answers 403 to every request of an IP it banned; banStreak
	// targets in a row refused so put a gitlab provider down, so that the ban
	// is not extended. A 403 with a rule, and a provider of another type,
	// never trip it. Every target was once tried during a ban.
	t.Run("gitlab ban", func(t *testing.T) {
		errForbidden := &platform.Error{Op: "test", Class: platform.ClassPermission, Status: http.StatusForbidden, Err: errors.New("403 Forbidden")}
		errRule := &platform.Error{Op: "test", Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "archived", Err: errors.New("archived")}
		for _, tc := range []struct {
			name, typ string
			err       error
			tripped   bool
		}{
			{"gitlab, plain 403", "gitlab", errForbidden, true},
			{"gitlab, 403 with a rule", "gitlab", errRule, false},
			{"github, plain 403", "github", errForbidden, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := newWorld(t)
				const n = banStreak + 2
				for i := range n {
					w.optedIn(fmt.Sprintf("acme/b%d", i), nil)
				}
				w.p.Fail("ReadFile", tc.err)
				d := w.deps()
				d.Providers[0].Config.Type = tc.typ
				d.Concurrency = 1
				rep := w.plan(d)
				for i := range n {
					outcome, reason := report.OutcomeFailed, "access"
					if tc.tripped && i >= banStreak {
						outcome, reason = report.OutcomeDeferred, "provider-down"
					}
					tg := want(t, rep, fmt.Sprintf("gh:acme/b%d", i), outcome, reason, 0)
					if reason == "provider-down" && !hasWarningLike(tg, "answered 403 Forbidden for 5 targets in a row") {
						t.Errorf("b%d: warnings %q", i, tg.Warnings)
					}
				}
				wantReads := n
				if tc.tripped {
					wantReads = banStreak
				}
				if got := countCalls(w.p, "ReadFile"); got != wantReads {
					t.Errorf("%d reads, want %d", got, wantReads)
				}
			})
		}
	})
	t.Run("a success ends the streak", func(t *testing.T) {
		w := newWorld(t)
		for i := range 5 {
			w.optedIn(fmt.Sprintf("acme/s%d", i), nil)
		}
		for range 2 {
			w.p.FailNext("ReadFile", errAuth)
		}
		d := w.deps()
		d.Concurrency = 1
		rep := w.plan(d)
		for i := range 5 {
			if i < 2 {
				want(t, rep, fmt.Sprintf("gh:acme/s%d", i), report.OutcomeFailed, "auth", 0)
				continue
			}
			want(t, rep, fmt.Sprintf("gh:acme/s%d", i), report.OutcomeOpened, "", 0)
		}
	})
}

// A transport failure of the fetch that proves a branch sits on the
// default branch is an error of the target, tried three times, never a proof
// that the branch was edited: the pull request gets no "paused" block. One
// 502 of that fetch once paused every open pull request whose base had
// moved, and the next run unpaused it.
func TestRunDeepenTransport(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", baseFiles...)
	n := w.openOwn(api)
	w.written(api, n)
	w.push(api, "main", w.person, "src/app.txt", "the team's app\n")
	// The steady state: Hc's parent is not B, so the run proves it.
	rep := w.run(w.deps(ModeDistribute), ModeDistribute)
	want(t, rep, "gh:acme/api", report.OutcomeUnchanged, "", n)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Fatalf("the steady state wrote %q", writes)
	}
	for _, tc := range []struct {
		faults  int
		outcome report.Outcome
		reason  string
	}{
		{1, report.OutcomeUnchanged, ""},
		{readAttempts, report.OutcomeFailed, "transient"},
	} {
		stop := w.onShallowSince(func() { transientTimes(w.p, "Fetch", errTransient, tc.faults) })
		rep := w.run(w.deps(ModeDistribute), ModeDistribute)
		stop()
		pr := 0
		if tc.outcome == report.OutcomeUnchanged {
			pr = int(n)
		}
		tg := want(t, rep, "gh:acme/api", tc.outcome, tc.reason, int64(pr))
		if writes := w.p.Writes(); len(writes) > 0 {
			t.Errorf("%d faults: wrote %q (%q)", tc.faults, writes, tg.Warnings)
		}
		if body := w.p.PR(api.ID, n).Body; strings.Contains(body, "touchmark paused") {
			t.Errorf("%d faults: the pull request was paused:\n%s", tc.faults, body)
		}
	}
}

// onShallowSince runs arm once, right before the first fetch of the
// default branch's history (--shallow-since) in a repository of g's run,
// and returns the function that stops watching.
func (g *gitWorld) onShallowSince(arm func()) func() {
	var once sync.Once
	dir := filepathSlash(g.src.Dir)
	return gitx.TraceCommands(func(args, env []string) {
		if !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--shallow-since=") }) {
			return
		}
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GIT_DIR="); ok && strings.HasPrefix(filepathSlash(v), dir) {
				once.Do(arm)
				return
			}
		}
	})
}

// A provider's sweep waits while another provider of the run on the same
// host is unavailable: that provider's targets are not resolved, and their
// pull requests may be in this one's listing (its known_authors hold the
// other's writer). The sweep once closed them as
// target-dropped.
func TestRunSweepSameHost(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML = "version: 1\nid: acme-eng\nproviders:\n" +
		"  - id: gha\n    type: github\n    writer: acme-write[bot]\n" +
		"  - id: ghb\n    type: github\n    writer: acme-old[bot]\n    known_authors: [\"acme-write[bot]\"]\n"
	w.targetsYML = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\n    provider: gha\n"
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", baseFiles...)
	n := w.openOwn(api)
	w.p.GrantWrite(api.ID, w.known)
	w.ok()
	d := w.deps(ModeDryRun)
	hub, _, err := config.ParseHub([]byte(w.hubYML))
	if err != nil {
		t.Fatal(err)
	}
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	down := &probeFails{Writer: w.p.Writer(w.writer)}
	d.Providers = []Provider{{Config: rps[0], Writer: down}, {Config: rps[1], Writer: w.p.Writer(w.known)}}
	rep := w.run(d, ModeDryRun)
	if len(rep.Providers) != 2 || rep.Providers[0].Error == "" {
		t.Fatalf("providers %+v", rep.Providers)
	}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeClosed {
			t.Errorf("the sweep closed %s #%v while provider gha was down", tg.Path, tg.PR)
		}
	}
	if !strings.Contains(rep.Sweep.Reason, "off for provider ghb: provider gha on the same host is unavailable") {
		t.Errorf("sweep %+v", rep.Sweep)
	}
	if pr := w.p.PR(api.ID, n); pr.State != platform.Open {
		t.Errorf("#%d is %s", n, pr.State)
	}
}

// probeFails is a writer whose Probe always fails with a server error.
type probeFails struct{ platform.Writer }

func (probeFails) Probe(context.Context) (platform.Caps, error) { return platform.Caps{}, errTransient }
