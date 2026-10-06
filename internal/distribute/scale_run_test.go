package distribute

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// first returns the first target of kind on provider id.
func (w *fleetWorld) first(kind fleetKind, id string) *fleetTarget {
	w.t.Helper()
	for _, st := range w.fleet {
		if st.kind == kind && st.prov.id == id {
			return st
		}
	}
	w.t.Fatalf("no %s target on %s", kind, id)
	return nil
}

// kinds counts the fleet's repositories by kind.
func (w *fleetWorld) kinds() map[fleetKind]int {
	out := map[fleetKind]int{}
	for _, st := range w.fleet {
		out[st.kind]++
	}
	return out
}

// fleetRun is what one run of the fleet did.
type fleetRun struct {
	// writes are the write requests all platforms received; settled is set
	// when no target ended deferred or failed.
	writes  int
	settled bool
	// digest sums the run up: outcomes, requests and times per provider.
	digest string
}

// runOnce runs distribute over the fleet as run n, an hour after the last
// one ended (the next scheduled run), with the platforms' faults unless
// quiet, and checks it (check). It fails the test on an error of the run.
func (w *fleetWorld) runOnce(n int, quiet bool) fleetRun {
	t := w.t
	t.Helper()
	start := w.now()
	if n > 1 {
		start = start.Add(time.Hour)
	}
	w.sync(start)
	deadline := start.Add(fleetRunLength)
	ownOpen := w.invariants(fmt.Sprintf("before run %d", n))
	for _, sp := range w.provs {
		sp.faults.begin(n, quiet)
		sp.p.ResetRequests()
		sp.p.ResetCalls()
	}
	rep, err := Run(t.Context(), w.deps(ModeDistribute, deadline), ModeDistribute)
	if err != nil {
		t.Fatalf("run %d: %v", n, err)
	}
	for _, sp := range w.provs {
		if err := sp.p.Err(); err != nil {
			t.Fatalf("run %d: setup: %v", n, err)
		}
	}
	checkReport(t, rep)
	res := w.check(n, rep, start, deadline, ownOpen)
	w.invariants(fmt.Sprintf("after run %d", n))
	return res
}

// fleetAt is a target's place in targets.yml order: its provider's entry,
// then its path (the fake lists repositories by path).
type fleetAt struct {
	prov int
	path string
}

func (a fleetAt) before(b fleetAt) bool {
	return a.prov < b.prov || (a.prov == b.prov && a.path < b.path)
}

// check checks run n's report and the requests the platforms received
// against the hub's limits and the fleet's kinds, and returns what the run
// did.
func (w *fleetWorld) check(n int, rep *report.Delivery, start, deadline time.Time, ownOpen int) fleetRun {
	t := w.t
	t.Helper()
	res := fleetRun{settled: true}
	ends := map[string]time.Time{}
	var digest []string
	for i, sp := range w.provs {
		reqs := sp.p.Requests()
		limits, injected := sp.faults.report()
		l := limitsOf(sp.p.Caps().Limits, w.rps[i].Limits, 1)
		checkFleetBudget(t, n, sp.id, reqs, l, deadline)
		checkFleetPauses(t, n, sp.id, reqs, limits)
		writes, reads := 0, 0
		for _, r := range reqs {
			if r.Kind == fake.RequestRead {
				reads++
			} else {
				writes++
			}
		}
		res.writes += writes
		ends[sp.id] = sp.clock.Now()
		if c := rep.Cost[sp.id]; c > writes {
			t.Errorf("run %d: %s counts %d writes, the platform received %d", n, sp.id, c, writes)
		}
		t.Logf("run %d: %s: %d reads, %d writes (%d counted), faults %v, done at +%v", n, sp.id, reads, writes, rep.Cost[sp.id],
			injected, sp.clock.Now().Sub(start).Round(time.Second))
		digest = append(digest, fmt.Sprintf("%s:%d/%d/%v", sp.id, reads, writes, sp.clock.Now().Sub(start)))
	}

	counts := map[string]int{}
	opened, closed, attempted := 0, 0, 0
	var allowed, rolled []fleetAt
	provIndex := map[string]int{"gh": 0, "gl": 1}
	// out are the providers that went out of budget in the run, as their
	// named targets' warnings say: only that, or a pause that outlasts a
	// target's time, explains a target deferred:rate-limit (a single rate
	// limit pauses and retries).
	out := map[string]bool{}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeDeferred && tg.Reason == "rate-limit" && slices.ContainsFunc(tg.Warnings, outOfBudget) {
			out[tg.Provider] = true
		}
	}
	for _, tg := range rep.Targets {
		key := string(tg.Outcome) + ":" + tg.Reason
		counts[key]++
		switch tg.Outcome {
		case report.OutcomeDeferred, report.OutcomeFailed:
			res.settled = false
		case report.OutcomeOpened:
			opened++
		case report.OutcomeClosed:
			closed++
		}
		switch key {
		case "deferred:rate-limit":
			if !rateLimitExplained(tg.Warnings) && (tg.Path != "" || !out[tg.Provider]) {
				t.Errorf("run %d: %s:%s is deferred:rate-limit, and neither is %s out of budget nor did a pause outlast the target's time (warnings %q)",
					n, tg.Provider, tg.Path, tg.Provider, tg.Warnings)
			}
		case "failed:auth":
			if n != 3 || tg.Provider != "gh" {
				t.Errorf("run %d: %s:%s is failed:auth without a storm of refused credentials (warnings %q)", n, tg.Provider, tg.Path, tg.Warnings)
			}
		case "deferred:provider-down":
			if n != 3 || tg.Provider != "gh" {
				t.Errorf("run %d: %s:%s is deferred:provider-down without a storm of refused credentials", n, tg.Provider, tg.Path)
			}
		case "deferred:deadline":
			if end := ends[tg.Provider]; end.Before(deadline.Add(-61 * time.Minute)) {
				t.Errorf("run %d: %s:%s is deferred:deadline, and %s stopped at +%v", n, tg.Provider, tg.Path, tg.Provider, end.Sub(start))
			}
		case "blocked:mass-close":
			t.Errorf("run %d: %s:%s is blocked:mass-close (warnings %q)", n, tg.Provider, tg.Path, rep.Warnings)
		}
		if tg.Path == "" {
			switch key {
			case "skipped:private-in-public-hub", "closed:target-dropped", "deferred:deadline", "deferred:rate-limit",
				"deferred:provider-down", "failed:transient", "failed:auth":
			default:
				t.Errorf("run %d: an unnamed target is %s", n, key)
			}
			continue
		}
		st := w.byRef[tg.Provider+":"+tg.Path]
		if st == nil {
			t.Errorf("run %d: the report names %s:%s, no repository of the fleet", n, tg.Provider, tg.Path)
			continue
		}
		if !fleetAllowed(st.kind, key) {
			t.Errorf("run %d: %s (%s) is %s (warnings %q)", n, st.ref(), st.kind, key, tg.Warnings)
		}
		if st.kind == fleetFresh {
			at := fleetAt{provIndex[tg.Provider], strings.ToLower(tg.Path)}
			switch key {
			case "deferred:rollout-limit":
				rolled = append(rolled, at)
			case "unchanged:":
			default:
				attempted++
				allowed = append(allowed, at)
			}
		}
	}
	// The rollout limit: at most max_new_prs_per_run new pull requests, the
	// first ones in targets.yml order, the rest deferred.
	creates := 0
	for _, op := range rep.Ops {
		if op.Kind == "create-pr" {
			creates++
		}
	}
	if opened > fleetMaxNew || creates > fleetMaxNew {
		t.Errorf("run %d: %d opened, %d pull requests created, the limit is %d", n, opened, creates, fleetMaxNew)
	}
	if len(rolled) > 0 {
		if attempted < fleetMaxNew {
			t.Errorf("run %d: %d targets deferred:rollout-limit while only %d new pull requests were let through", n, len(rolled), attempted)
		}
		last, first := allowed[0], rolled[0]
		for _, a := range allowed {
			if last.before(a) {
				last = a
			}
		}
		for _, r := range rolled {
			if r.before(first) {
				first = r
			}
		}
		if first.before(last) {
			t.Errorf("run %d: %v is deferred:rollout-limit before %v, which went", n, first, last)
		}
	}
	// The mass-close guard (I10).
	if ok, limit := decide.MassCloseAllowed(decide.MassCloseInput{Closes: closed, OwnOpen: ownOpen, MaxFraction: 0.5}); !ok {
		t.Errorf("run %d: %d closes of %d open pull requests, more than %d", n, closed, ownOpen, limit)
	}
	if want := w.kinds()[fleetPrivate]; counts["skipped:private-in-public-hub"] != want {
		t.Errorf("run %d: %d targets skipped:private-in-public-hub, the fleet has %d", n, counts["skipped:private-in-public-hub"], want)
	}
	if d := counts["deferred:deadline"]; d > 0 && !hasWarning(rep.Warnings, fmt.Sprintf("left %d of", d)) {
		t.Errorf("run %d: %d targets deferred:deadline, warnings %q", n, d, rep.Warnings)
	}
	var line []string
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		line = append(line, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	t.Logf("run %d: %s; exit %d", n, strings.Join(line, " "), rep.ExitCode())
	res.digest = strings.Join(append(line, digest...), " ")
	return res
}

// outOfBudget reports whether a warning says the throttle put the target's
// provider out of budget for the run (throttle.Gate: three rate limits in
// a row, or a pause longer than it takes).
func outOfBudget(w string) bool {
	return strings.Contains(w, "times in a row; the rest waits for the next run") && strings.Contains(w, "limited the rate") ||
		strings.Contains(w, "longer than touchmark pauses")
}

// rateLimitExplained reports whether a target's warnings explain its
// deferred:rate-limit: its provider went out of budget, or a pause for a
// rate limit left no room in the time the target's writes have.
func rateLimitExplained(warnings []string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool {
		return outOfBudget(w) || strings.Contains(w, "a pause for a rate limit leaves no room")
	})
}

// fleetAllowed reports whether a target of kind may end a run with key
// (outcome:reason): what its kind leads to, or a deferral or failure the
// platforms' faults explain (check ties each to its run's faults).
func fleetAllowed(kind fleetKind, key string) bool {
	switch key {
	case "deferred:deadline", "deferred:rate-limit", "deferred:provider-down", "failed:transient", "failed:auth":
		return true
	}
	var ok []string
	switch kind {
	case fleetFresh:
		ok = []string{"opened:", "unchanged:", "deferred:rollout-limit"}
	case fleetCurrent:
		ok = []string{"updated:body", "unchanged:"}
	case fleetRetitle:
		ok = []string{"updated:title", "updated:body", "unchanged:"}
	case fleetDeclined:
		ok = []string{"declined:"}
	case fleetSettled:
		ok = []string{"closed:no-diff", "unchanged:"}
	case fleetOptedOut:
		ok = []string{"closed:opted-out", "skipped:not-opted-in"}
	case fleetDropped:
		ok = []string{"closed:target-dropped"}
	case fleetNotOpted:
		ok = []string{"skipped:not-opted-in"}
	case fleetArchived:
		ok = []string{"skipped:archived"}
	case fleetInvalid:
		ok = []string{"blocked:opt-in-invalid"}
	case fleetForeign:
		ok = []string{"blocked:branch-in-use"}
	}
	return slices.Contains(ok, key)
}

// checkFleetBudget checks the requests a provider received in run n against
// its limits, on its clock: writes (comments included) at least the minimum
// interval apart, never more in a minute or an hour than allowed, comments
// and reads within their budgets per minute, and no write after the
// deadline's grace.
func checkFleetBudget(t *testing.T, n int, id string, reqs []fake.Request, l throttle.Limits, deadline time.Time) {
	t.Helper()
	var writes, comments, reads []time.Time
	for _, r := range reqs {
		switch r.Kind {
		case fake.RequestRead:
			reads = append(reads, r.At)
		case fake.RequestComment:
			comments = append(comments, r.At)
			writes = append(writes, r.At)
		default:
			writes = append(writes, r.At)
		}
	}
	for i := 1; i < len(writes); i++ {
		if gap := writes[i].Sub(writes[i-1]); gap < l.MinInterval {
			t.Errorf("run %d: %s: writes %d and %d are %v apart, the least is %v", n, id, i-1, i, gap, l.MinInterval)
			break
		}
	}
	for _, c := range []struct {
		what  string
		times []time.Time
		span  time.Duration
		limit int
	}{
		{"writes", writes, time.Minute, l.WritesPerMinute},
		{"writes", writes, time.Hour, l.WritesPerHour},
		{"comments", comments, time.Minute, l.CommentsPerMinute},
		{"reads", reads, time.Minute, l.ReadsPerMinute},
	} {
		if c.limit <= 0 {
			continue
		}
		if got := mostInSpan(c.times, c.span); got > c.limit {
			t.Errorf("run %d: %s: %d %s in %v, the limit is %d", n, id, got, c.what, c.span, c.limit)
		}
	}
	if len(writes) > 0 && writes[len(writes)-1].After(deadline.Add(writeGrace)) {
		t.Errorf("run %d: %s: a write at %v, past the deadline %v and its grace", n, id, writes[len(writes)-1], deadline)
	}
}

// mostInSpan returns the most times in any window of span; times are in
// order.
func mostInSpan(times []time.Time, span time.Duration) int {
	most, lo := 0, 0
	for hi := range times {
		for times[hi].Sub(times[lo]) >= span {
			lo++
		}
		most = max(most, hi-lo+1)
	}
	return most
}

// checkFleetPauses checks that no request reached a provider while a rate
// limit it answered in run n paused it: the Retry-After it asked for, at
// least the throttle's first pause without one, or until the reset its
// headers named. The requests are in the order they arrived, on the
// provider's clock: the first one logged after the answer (fleetLimit.idx)
// must come at the pause's end or later. The position, not the time, says
// what came after: the fake clock moves only when something sleeps, so a
// request sent at once, without honouring the pause, carries the limit's
// own time. A check by time alone once let such requests
// through.
func checkFleetPauses(t *testing.T, n int, id string, reqs []fake.Request, limits []fleetLimit) {
	t.Helper()
	bad := 0
	for _, l := range limits {
		end := l.at.Add(l.pause)
		if i := l.idx; i < len(reqs) && reqs[i].At.Before(end) {
			if bad++; bad <= 3 {
				t.Errorf("run %d: %s: a %s request at %v (#%d), while the %s at %v (#%d) paused the provider until %v",
					n, id, reqs[i].Method, reqs[i].At, i, l.what, l.at, l.idx-1, end)
			}
		}
	}
}

// invariants checks what must hold between runs (the invariants of the
// package doc, as far as the fleet's model shows them) and returns how many
// pull requests of touchmark's are open: at most one per target (no
// duplicate opened); a person's pull request is never written to (I3); a
// declined target gets no new pull request (I4); no pull request's head is
// its base (I5); at most one comment on any pull request.
func (w *fleetWorld) invariants(when string) int {
	t := w.t
	t.Helper()
	open := 0
	for _, st := range w.fleet {
		p := st.prov.p
		own := 0
		prs := p.PRList(st.repo.ID)
		for _, pr := range prs {
			if pr.Head == pr.Base {
				t.Errorf("%s: %s #%d has its base as its head", when, st.ref(), pr.Number)
			}
			if cs := p.Comments(st.repo.ID, pr.Number); len(cs) > 1 {
				t.Errorf("%s: %s #%d has %d comments", when, st.ref(), pr.Number, len(cs))
			}
			if pr.Author.ID == st.prov.writer.ID && pr.State == platform.Open {
				own++
			}
		}
		open += own
		if own > 1 {
			t.Errorf("%s: %s has %d open pull requests of touchmark's", when, st.ref(), own)
		}
		switch st.kind {
		case fleetForeign:
			got := p.PR(st.repo.ID, st.foreign.Number)
			if got.Title != st.foreign.Title || got.Body != st.foreign.Body || got.State != platform.Open ||
				len(p.Comments(st.repo.ID, st.foreign.Number)) > 0 || len(prs) != 1 {
				t.Errorf("%s: %s: the person's pull request changed: %+v, %d pull requests", when, st.ref(), got, len(prs))
			}
		case fleetDeclined:
			if len(prs) != 1 {
				t.Errorf("%s: %s: %d pull requests after a decline", when, st.ref(), len(prs))
			}
		}
	}
	return open
}

// checkFinal checks the fleet once it is done: each kind where its
// decisions lead it.
func (w *fleetWorld) checkFinal() {
	t := w.t
	t.Helper()
	fps := []string{hubFP}
	for _, st := range w.fleet {
		p := st.prov.p
		var mine []platform.PR
		for _, pr := range p.PRList(st.repo.ID) {
			if pr.Author.ID == st.prov.writer.ID {
				mine = append(mine, pr)
			}
		}
		fail := func(format string, args ...any) {
			t.Errorf("%s (%s): %s", st.ref(), st.kind, fmt.Sprintf(format, args...))
		}
		switch st.kind {
		case fleetNotOpted, fleetArchived, fleetInvalid, fleetPrivate, fleetForeign:
			if len(mine) != 0 {
				fail("%d pull requests of touchmark's", len(mine))
			}
			continue
		}
		if len(mine) != 1 {
			fail("%d pull requests of touchmark's", len(mine))
			continue
		}
		pr := mine[0]
		m, status := marker.Find(pr.Body, fps)
		if status != marker.Found {
			fail("#%d: no valid marker (%s)", pr.Number, status)
			continue
		}
		switch st.kind {
		case fleetFresh, fleetCurrent, fleetRetitle:
			if pr.State != platform.Open || m.Key != st.key || pr.Title != fleetTitle || m.Data.Body == "" ||
				!slices.Contains(pr.Labels, "engineering-assets") {
				fail("#%d: %s %q, labels %q, marker key %s body %q", pr.Number, pr.State, pr.Title, pr.Labels, m.Key, m.Data.Body)
			}
		case fleetDeclined:
			if pr.State != platform.Closed || !m.Data.Ack {
				fail("#%d: %s, ack %v", pr.Number, pr.State, m.Data.Ack)
			}
		case fleetSettled, fleetOptedOut, fleetDropped, fleetHidden:
			reason := map[fleetKind]string{fleetSettled: "no-diff", fleetOptedOut: "opted-out", fleetDropped: "target-dropped",
				fleetHidden: "target-dropped"}[st.kind]
			if pr.State != platform.Closed || m.Data.Closed == nil || m.Data.Closed.By != "touchmark" || m.Data.Closed.Reason != reason {
				fail("#%d: %s, closed %+v, want %s", pr.Number, pr.State, m.Data.Closed, reason)
			}
		}
	}
}

// TestScale runs distribute over 5000 targets (fleetQuota) on a GitHub and a
// GitLab whose platforms rate limit (with and without Retry-After, GitHub's
// secondary limits, headers that say the budget is spent), fail with server
// errors and time out, with storms that put a provider out of budget or down
// for a run. Every run keeps each provider's budget and minimum interval on
// its clock and every pause a rate limit started, defers what the rollout
// limit, the deadline (5 h 30 min) or a storm leaves with the right reason,
// and keeps the invariants the fleet's model shows; the runs that follow, an
// hour apart, finish the fleet within a bound (the runs the rollout limit
// needs, and fleetSpareRuns), and a run on the finished fleet writes
// nothing. Each report passes the schema. It is heavy, so it runs on Linux
// (the CI test job, and the nightly job with a larger fleet), elsewhere with
// TOUCHMARK_HEAVY_TESTS; -short runs a tenth of the fleet.
func TestScale(t *testing.T) {
	simHeavy(t)
	quota := fleetQuota(t)
	began := time.Now()
	w := newFleet(t, quota)
	w.provs[0].faults.failPRs = w.first(fleetCurrent, "gh").repo.Path
	t.Logf("the fleet: %v", w.kinds())
	done := 0
	limit := fleetSpareRuns + (w.kinds()[fleetFresh]+fleetMaxNew-1)/fleetMaxNew
	for n := 1; n <= limit; n++ {
		res := w.runOnce(n, false)
		if res.writes == 0 && res.settled {
			done = n
			break
		}
	}
	if done == 0 {
		t.Fatalf("the fleet is not done after %d runs", limit)
	}
	t.Logf("done: run %d wrote nothing; %v in all", done, time.Since(began).Round(time.Millisecond))
	w.checkFinal()
	// I7: the finished fleet stays finished, faults or not.
	if res := w.runOnce(done+1, false); res.writes != 0 || !res.settled {
		t.Errorf("a run on the finished fleet wrote %d times (settled %v)", res.writes, res.settled)
	}
}

// TestScaleDeterministic: the same seed makes the same runs. A small fleet
// runs twice; every run sums up the same: outcomes, requests and the time
// each provider took; and the fleet is done by the fourth run. It is light
// enough for every platform, where it stands in for TestScale.
func TestScaleDeterministic(t *testing.T) {
	t.Parallel()
	digests := func() []string {
		w := newFleet(t, [2]int{150, 100})
		w.provs[0].faults.failPRs = w.first(fleetCurrent, "gh").repo.Path
		var out []string
		var last fleetRun
		for n := 1; n <= 4; n++ {
			last = w.runOnce(n, false)
			out = append(out, last.digest)
		}
		if last.writes != 0 || !last.settled {
			t.Errorf("the fleet is not done after 4 runs: %s", last.digest)
		}
		w.checkFinal()
		return out
	}
	a, b := digests(), digests()
	if !slices.Equal(a, b) {
		t.Errorf("two runs of the same seed differ:\n%q\n%q", a, b)
	}
}
