package distribute

import (
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/report"
)

// TestPlanScopeScale is TestPlanScopeUntouched at the size of TestScale: a
// plan of a hub pull request that changes one pack (python) over the fleet
// of TestScale, 5000 targets on two providers, against the plan of every
// target. The scoped plan processes exactly the targets whose packs hold
// python (the python topic of targets.yml, whatever their opt-in file says),
// each as the full plan does; it reports no other target, closes nothing in
// them (no target-dropped, no opted-out outside python), never snapshots
// them or lists their pull requests, keeps the mass-close guard quiet,
// writes nothing (no writer is called) and passes the schema. The resolve
// and the opt-in files still cover every target.
func TestPlanScopeScale(t *testing.T) {
	simHeavy(t)
	quota := fleetQuota(t)
	w := newFleet(t, quota)
	plan := func(scope *Scope) (*report.Delivery, map[string]int, [][]string) {
		t.Helper()
		start := w.now().Add(time.Hour)
		w.sync(start)
		var calls [][]string
		reads := map[string]int{}
		for _, sp := range w.provs {
			sp.faults.begin(0, true)
			sp.p.ResetRequests()
			sp.p.ResetCalls()
		}
		d := w.deps(ModePlan, time.Time{})
		d.Scope = scope
		rep, err := Plan(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		checkReport(t, rep)
		for _, sp := range w.provs {
			reads[sp.id] = len(sp.p.Requests())
			calls = append(calls, sp.p.Calls())
		}
		return rep, reads, calls
	}
	full, fullReads, _ := plan(nil)
	scoped, scopedReads, calls := plan(&Scope{Mode: report.ScopePacks, Packs: []string{"python"}})

	python, total := map[string]bool{}, 0
	for _, st := range w.fleet {
		if st.kind == fleetDropped || st.kind == fleetHidden {
			continue
		}
		total++
		if st.python {
			python[st.ref()] = true
		}
	}
	if s := scoped.Scope; s == nil || s.Mode != report.ScopePacks || s.Processed != len(python) || s.Total != total {
		t.Fatalf("scope %+v, want %d of %d targets", s, len(python), total)
	}
	fullBy := map[string]report.DeliveryTarget{}
	for _, tg := range full.Targets {
		fullBy[tg.Provider+":"+tg.Path] = tg
	}
	named, unnamed, rolledInFull := 0, 0, 0
	for _, tg := range scoped.Targets {
		ref := tg.Provider + ":" + tg.Path
		if tg.Path == "" {
			unnamed++
			if tg.Reason != report.ReasonPrivate {
				t.Errorf("the scoped plan has an unnamed %s:%s", tg.Outcome, tg.Reason)
			}
			continue
		}
		named++
		if !python[ref] {
			t.Errorf("the scoped plan reports %s (%s:%s), outside the python pack", ref, tg.Outcome, tg.Reason)
			continue
		}
		f := fullBy[ref]
		if f.Outcome == report.OutcomeDeferred && f.Reason == "rollout-limit" && tg.Outcome == report.OutcomeOpened {
			// The rollout limit counts the targets a plan processes: in the
			// full plan GitHub's new pull requests come first in targets.yml
			// order and take every place.
			rolledInFull++
			continue
		}
		if f.Outcome != tg.Outcome || f.Reason != tg.Reason || f.Writes != tg.Writes || f.Key != tg.Key {
			t.Errorf("%s: scoped %s:%s (%d writes, key %s), full %s:%s (%d writes, key %s)", ref, tg.Outcome, tg.Reason, tg.Writes,
				tg.Key, f.Outcome, f.Reason, f.Writes, f.Key)
		}
	}
	privatePython := 0
	for _, st := range w.fleet {
		if st.kind == fleetPrivate && st.python {
			privatePython++
		}
	}
	if named+unnamed != len(python) || unnamed != privatePython {
		t.Errorf("the scoped plan reports %d named and %d unnamed targets, want %d python targets (%d private)",
			named, unnamed, len(python), privatePython)
	}
	t.Logf("the scoped plan: %d of %d targets; %d of its new pull requests wait for the rollout limit in the full plan",
		len(python), total, rolledInFull)
	for _, c := range []struct {
		key  string
		want bool
	}{{"closed:target-dropped", false}, {"blocked:mass-close", false}} {
		if n := countOutcome(scoped, c.key); (n > 0) != c.want {
			t.Errorf("the scoped plan has %d %s", n, c.key)
		}
	}
	if n := countOutcome(full, "closed:target-dropped"); n == 0 {
		t.Errorf("the full plan closes no pull request of a dropped repository: the fleet tests nothing")
	}
	// Out of the scope, nothing beyond the resolve and the opt-in file.
	for i, sp := range w.provs {
		for _, c := range calls[i] {
			method, rest, _ := strings.Cut(c, " ")
			if method != "Snapshot" && method != "PRs" && method != "Remote" {
				continue
			}
			path, _, _ := strings.Cut(rest, " ")
			if !python[sp.id+":"+path] {
				t.Errorf("the scoped plan called %q, outside the python pack", c)
			}
		}
		if scopedReads[sp.id] >= fullReads[sp.id] {
			t.Errorf("%s: the scoped plan read %d times, the full plan %d", sp.id, scopedReads[sp.id], fullReads[sp.id])
		}
		t.Logf("%s: reads: full plan %d, scoped plan %d", sp.id, fullReads[sp.id], scopedReads[sp.id])
	}
	for id, c := range scoped.Cost {
		if c > full.Cost[id] {
			t.Errorf("%s: the scoped plan costs %d writes, the full one %d", id, c, full.Cost[id])
		}
	}
}

// countOutcome counts the targets of rep with key (outcome:reason).
func countOutcome(rep *report.Delivery, key string) int {
	n := 0
	for _, tg := range rep.Targets {
		if string(tg.Outcome)+":"+tg.Reason == key {
			n++
		}
	}
	return n
}
