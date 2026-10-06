package distribute

import (
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// People act while distribute runs: an event lands after phase C inspected
// the target, right before the recheck reads its branches and pull requests
// again; or later, once the per-target writer is minted or right before the
// push, where only the lease of the push can still catch a moved branch. The
// run must lose no commit of a person (I2), write to nothing that is not
// touchmark's (I3), make no forbidden transition (I5) and fail at most with
// failed:race; the runs after it converge and a run after that writes
// nothing (I7).

// simMoment is when a mid-run event lands.
type simMoment string

const (
	beforeRecheck simMoment = "before-recheck"
	beforeWrite   simMoment = "before-write"
	beforePush    simMoment = "before-push"
)

// simMidEvent is something people do during a run; do returns false when
// it does not apply to the state.
type simMidEvent struct {
	name string
	// moves tells that the event moves the sync branch: only those can land
	// after the recheck (the lease of the push catches them; a pull request
	// closed or opened between the recheck and the push is a race no client
	// can close).
	moves bool
	do    func(s *simScenario, tg *simTarget) bool
}

var simMidEvents = []simMidEvent{
	{name: "push-sync", moves: true, do: func(s *simScenario, tg *simTarget) bool {
		_, err := s.push(tg, s.branches()[0], map[string]string{"notes.md": s.edit("notes.md")})
		return err == nil
	}},
	{name: "update-branch", moves: true, do: func(s *simScenario, tg *simTarget) bool {
		pr, ok := s.openOwnPR(tg)
		if !ok || s.baseMove(tg) == "" {
			return false
		}
		_, err := s.p.UpdateBranch(tg.repo.ID, pr.Number, s.person, time.Time{})
		return err == nil
	}},
	{name: "base-move", do: func(s *simScenario, tg *simTarget) bool { return s.baseMove(tg) != "" }},
	{name: "local-edit", do: func(s *simScenario, tg *simTarget) bool {
		_, err := s.push(tg, tg.repo.DefaultBranch, map[string]string{"AGENTS.md": s.edit("AGENTS.md")})
		return err == nil
	}},
	{name: "merge", do: func(s *simScenario, tg *simTarget) bool {
		pr, ok := s.openOwnPR(tg)
		if !ok {
			return false
		}
		_, err := s.p.MergePR(tg.repo.ID, pr.Number, fake.MergeCommit, s.person, time.Time{})
		return err == nil
	}},
	{name: "close", do: func(s *simScenario, tg *simTarget) bool {
		pr, ok := s.openOwnPR(tg)
		if ok {
			s.p.SetPRState(tg.repo.ID, pr.Number, platform.Closed, &s.person, time.Time{})
		}
		return ok
	}},
	{name: "foreign-pr", do: func(s *simScenario, tg *simTarget) bool {
		_, err := s.foreignPR(tg)
		return err == nil
	}},
}

// simMidStates are the states the run starts from: what it would write.
var simMidStates = []struct {
	name  string
	setup func(w *simWorld)
}{
	{"open", func(*simWorld) {}},
	{"update", func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.ship("AGENTS.md", 2)
		w.hubChanged()
	}},
	{"close", func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.mustDo(w.push(w.target("api"), "main", maps.Clone(w.files[simBase])))
	}},
}

func TestMidRunMoves(t *testing.T) {
	needDeliveryGit(t)
	simHeavy(t)
	for _, st := range simMidStates {
		for _, ev := range simMidEvents {
			for _, at := range []simMoment{beforeRecheck, beforeWrite, beforePush} {
				if at != beforeRecheck && !ev.moves {
					continue
				}
				t.Run(st.name+"/"+ev.name+"/"+string(at), func(t *testing.T) {
					t.Parallel()
					midRun(t, st.setup, ev, at)
				})
			}
		}
	}
}

// midRun starts from the state setup makes, lands ev at moment at of the
// next distribute, and checks the run and the runs after it.
func midRun(t *testing.T, setup func(*simWorld), ev simMidEvent, at simMoment) {
	w := newSimWorld(t, fake.GitHub, "api")
	tg := w.target("api")
	setup(w)
	s := &simScenario{simWorld: w, rng: rand.New(rand.NewPCG(1, 2))}
	s.c = newSimChecker(w, func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
	})
	s.c.observe()
	// A ticked rebuild or an operations.yml recreate lets the run drop
	// people's commits (I2): the checks must know it before the run.
	s.c.release()
	before := s.c.snapshot()

	// fire runs on the run's goroutine: it reports problems, never stops
	// the test.
	var once sync.Once
	fired, applied := false, false
	fire := func() {
		once.Do(func() {
			fired = true
			applied = ev.do(s, tg)
			if err := w.p.Err(); err != nil {
				t.Errorf("setup: %v", err)
			}
			s.c.track(tg) // what people pushed must survive the run
			for _, pr := range w.prs(tg) {
				if pr.Author.ID != w.writer.ID {
					s.c.addForeign(tg, pr.Number)
				}
			}
		})
	}
	src := filepathSlash(w.src.Dir)
	remove := gitx.TraceCommands(func(args, env []string) {
		cmd := ""
		for _, a := range args {
			if a == "ls-remote" || a == "push" {
				cmd = a
			}
		}
		mine := slices.ContainsFunc(env, func(kv string) bool {
			v, ok := strings.CutPrefix(kv, "GIT_DIR=")
			return ok && strings.HasPrefix(filepathSlash(v), src)
		})
		switch {
		case !mine:
		case at == beforeRecheck && cmd == "ls-remote", at == beforePush && cmd == "push":
			fire()
		}
	})
	defer remove()
	w.p.ResetCalls()
	rep := w.run(ModeDistribute, func(d *Deps) {
		if at != beforeWrite {
			return
		}
		d.Providers[0].Writer = &exWriter{Writer: w.p.Writer(w.writer), target: func(tw platform.TargetWriter) platform.TargetWriter {
			fire()
			return tw
		}}
	})
	remove()
	writes := w.p.Writes()
	switch {
	case !fired:
		t.Skipf("the run never reached %s: %s", at, outcomes(rep))
	case !applied:
		t.Skipf("%s does not apply to the state", ev.name)
	}
	t.Logf("%s at %s: %s, writes %q", ev.name, at, outcomes(rep), writes)
	if v := w.p.Violations(); len(v) > 0 {
		t.Errorf("I5: forbidden transitions: %q", v)
	}
	for _, res := range rep.Targets {
		if res.Outcome == report.OutcomeFailed && res.Reason != "race" {
			t.Errorf("%s failed:%s: %q", res.Path, res.Reason, res.Warnings)
		}
	}
	s.c.checkKept(tg)
	s.c.checkWrites(writes, before)
	after := s.c.snapshot()
	for _, op := range rep.Ops {
		if op.Kind == "push" {
			s.c.checkPush(tg, op, after)
		}
	}
	// The next run converges, and the one after it writes nothing.
	s.settle(nil)
	if t.Failed() {
		t.Logf("what happened: %s", strings.Join(s.log, "\n  "))
	}
}
