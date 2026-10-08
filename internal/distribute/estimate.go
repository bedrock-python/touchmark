package distribute

import (
	"time"

	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// The estimate of a plan or a dry run: per provider, the writes of the run
// and the time the provider's write limits give them (the throttle's
// accounting: HTTP writes of the API and git pushes, with its limits and the
// platform's defaults), both by the git path and with every push that may
// need a signature through the platform's API commit, and how many runs the
// rollout needs under limits.max_new_prs_per_run.

// pathWrites are writes by the git path, and with the pushes that may need
// a signature through the platform's API commit.
type pathWrites struct{ git, api int }

// mayNeedSignature reports whether a push of w may go through the
// platform's API commit: the platform makes them (Caps.Commit.API), w
// pushes, and either the run already plans it (a signature is needed and
// no signing key makes it), or the rules that would require a signature
// are unknown and no signing key would meet them.
func mayNeedSignature(w *Work) bool {
	p := w.t.prov
	if !p.caps.Commit.API || !pushes(w.Decision) {
		return false
	}
	return w.viaAPI || (!w.needSig && !w.rules.Known && p.signer == nil)
}

// writesByPath estimates w's writes by both paths.
func (r *run) writesByPath(w *Work) pathWrites {
	labels := w.t.prov.createsLabels(r.hub)
	return pathWrites{git: writesWith(w, labels, false), api: writesWith(w, labels, mayNeedSignature(w))}
}

// estimateRollout sets the report's Estimate from the reported targets of a
// plan or a dry run: this run's writes per provider by both paths, the
// writes the targets deferred:rollout-limit add in the runs after it, the
// time each takes at the provider's limits, and the runs.
func (r *run) estimateRollout(reported []*target) {
	now := map[*provider]*pathWrites{}
	later := map[*provider]int{}
	for _, p := range r.provs {
		now[p] = &pathWrites{}
	}
	newPRs := 0
	for _, t := range reported {
		switch {
		case t.res.Outcome == report.OutcomeOpened,
			t.res.Outcome == report.OutcomeDeferred && t.res.Reason == "rollout-limit":
			newPRs++
		}
		pw := now[t.prov]
		if pw == nil {
			pw = &pathWrites{}
			now[t.prov] = pw
		}
		later[t.prov] += t.later.git
		if r.repos == nil {
			// The snapshot-only plan estimates by outcome, by the git path.
			pw.git += t.res.Writes
			pw.api += t.res.Writes
			continue
		}
		for _, w := range append([]*Work{t.work}, t.sweeps...) {
			if w != nil {
				n := r.writesByPath(w)
				pw.git += n.git
				pw.api += n.api
			}
		}
	}
	e := &report.Estimate{NewPRs: newPRs, MaxNewPRs: r.hub.Limits.MaxNewPRsPerRun, Providers: map[string]report.ProviderEstimate{}}
	writes := 0
	for p, pw := range now {
		writes += pw.git + later[p]
	}
	switch limit := e.MaxNewPRs; {
	case newPRs > 0 && limit <= 0:
	case newPRs > limit:
		e.Runs = (newPRs + limit - 1) / limit
	case writes > 0:
		e.Runs = 1
	}
	for p, pw := range now {
		limits := p.throttle().Limits()
		pe := report.ProviderEstimate{Writes: pw.git, Seconds: seconds(writeTime(pw.git, limits))}
		if pw.api != pw.git {
			pe.APIWrites, pe.APISeconds = pw.api, seconds(writeTime(pw.api, limits))
		}
		if total := pw.git + later[p]; e.Runs > 1 && total != pw.git {
			pe.TotalWrites, pe.TotalSeconds = total, seconds(writeTime(total, limits))
		}
		e.Providers[p.cfg.ID] = pe
	}
	r.rep.Estimate = e
}

// writeTime is how long n writes take at limits: the throttle's windows let
// WritesPerHour writes into any hour and WritesPerMinute into any minute,
// MinInterval apart; the first write goes at once. A zero limit does not
// bound.
func writeTime(n int, l throttle.Limits) time.Duration {
	if n <= 1 {
		return 0
	}
	var t time.Duration
	if h := l.WritesPerHour; h > 0 && n > h {
		full := (n - 1) / h
		t += time.Duration(full) * time.Hour
		n -= full * h
	}
	within := time.Duration(n-1) * l.MinInterval
	if m := l.WritesPerMinute; m > 0 {
		within = max(within, time.Duration((n-1)/m)*time.Minute)
	}
	return t + within
}

// seconds is d in whole seconds, rounded up.
func seconds(d time.Duration) int64 {
	return int64((d + time.Second - 1) / time.Second)
}
