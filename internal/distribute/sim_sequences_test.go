package distribute

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
)

// The random scenarios of the property test reach the risky paths rarely:
// the sequences here drive each of them on purpose, under the same checks
// (sim_check_test.go), and the property test asserts that its default
// scenarios still reach the outcomes they reached when they were written,
// so that a change of the generator cannot silently stop exercising them.

// simCoverage counts, over the scenarios of a run of the property test,
// how many reached each outcome ("outcome:reason").
type simCoverage struct {
	mu        sync.Mutex
	scenarios int
	outcomes  map[string]int
}

func newSimCoverage() *simCoverage { return &simCoverage{outcomes: map[string]int{}} }

// add records the outcomes one scenario reached.
func (c *simCoverage) add(reached map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scenarios++
	for o := range reached {
		c.outcomes[o]++
	}
}

// simCovered are the outcomes the default scenarios of the property test
// must keep reaching (seeds 1 to 100, two rounds at most).
var simCovered = []string{
	"opened:", "unchanged:", "updated:content", "updated:body", "declined:", "deferred:cooldown",
	"blocked:edited", "blocked:branch-in-use", "closed:no-diff", "closed:target-dropped", "closed:opted-out",
	"skipped:not-opted-in", "skipped:archived",
}

// check logs the coverage of the run and, for the default scenarios run
// whole (n of them, full), fails on an outcome of simCovered none reached.
func (c *simCoverage) check(t *testing.T, n int, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scenarios == 0 {
		return
	}
	var lines []string
	for _, o := range slices.Sorted(maps.Keys(c.outcomes)) {
		lines = append(lines, fmt.Sprintf("%s %d", o, c.outcomes[o]))
	}
	t.Logf("outcomes reached, by scenarios (of %d): %s", c.scenarios, strings.Join(lines, ", "))
	if !full || n != 100 || c.scenarios != n || t.Failed() {
		return
	}
	for _, o := range simCovered {
		if c.outcomes[o] == 0 {
			t.Errorf("no default scenario reached %s any more: the generator stopped exercising it", o)
		}
	}
}

// newSimSequence is a scenario of fixed events over a world of flavor with
// target api, its checks failing t.
func newSimSequence(t *testing.T, flavor fake.Flavor) (*simScenario, *simTarget) {
	t.Helper()
	w := newSimWorld(t, flavor, "api")
	s := &simScenario{simWorld: w, rng: rand.New(rand.NewPCG(1, 2)), noFaults: true}
	s.c = newSimChecker(w, func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
	})
	return s, w.target("api")
}

// lastOutcome is the outcome the last distribute of the sequence reported
// for tg (from the scenario's log).
func (s *simScenario) lastOutcome(tg *simTarget) string {
	line := s.log[len(s.log)-1]
	_, rest, _ := strings.Cut(line, ": ")
	for _, part := range strings.Split(rest, ", ") {
		if name, outcome, ok := strings.Cut(part, " "); ok && name == tg.repo.Path {
			outcome, _, _ = strings.Cut(outcome, " (")
			outcome, _, _ = strings.Cut(outcome, " #")
			return outcome
		}
	}
	return ""
}

// TestSimSequences drives, under the checks of the property test, the
// sequences its random scenarios rarely reach: a pause and its rebuild, the
// escalation of a bot's closes, a merge of a branch other than the base, a
// branch someone took, and a marker people broke.
func TestSimSequences(t *testing.T) {
	needDeliveryGit(t)
	simHeavy(t)
	t.Run("pause, tick, rebuild", func(t *testing.T) {
		t.Parallel()
		s, tg := newSimSequence(t, fake.GitHub)
		s.first(false)
		if _, err := s.push(tg, "touchmark/acme-eng", map[string]string{"notes.md": s.edit("notes.md")}); err != nil {
			t.Fatal(err)
		}
		s.logf("a person pushes to the sync branch")
		s.cycle()
		if got := s.lastOutcome(tg); got != "blocked:edited" {
			t.Fatalf("after a person's push: %s (%s)", got, s.log[len(s.log)-1])
		}
		if !s.tick(tg, 1, prbody.ControlRecreate) {
			t.Fatal("no recreate control")
		}
		s.logf("a person ticks rebuild")
		s.cycle()
		if got := s.lastOutcome(tg); got != "updated:recreate" {
			t.Errorf("after the tick: %s (%s)", got, s.log[len(s.log)-1])
		}
		s.cycle()
		if got := s.lastOutcome(tg); got != "unchanged" {
			t.Errorf("after the rebuild: %s (%s)", got, s.log[len(s.log)-1])
		}
	})
	t.Run("bot closes three times", func(t *testing.T) {
		t.Parallel()
		s, tg := newSimSequence(t, fake.GitHub)
		s.first(false)
		for round, want := range []string{"deferred:cooldown", "deferred:cooldown", "declined"} {
			pr := s.openOwn(tg)
			if len(pr) != 1 {
				t.Fatalf("round %d: open pull requests %v", round, pr)
			}
			s.p.SetPRState(tg.repo.ID, pr[0].Number, platform.Closed, &s.bot, time.Time{})
			s.logf("the stale bot closes #%d", pr[0].Number)
			s.cycle()
			if got := s.lastOutcome(tg); got != want {
				t.Fatalf("round %d: %s, want %s (%s)", round, got, want, s.log[len(s.log)-1])
			}
			if want == "declined" {
				break
			}
			// The cooldown doubles on the second close: wait both out.
			s.clock.Advance(time.Duration(31*(round+1)) * 24 * time.Hour)
			s.logf("time passes")
			s.cycle()
			if got := s.lastOutcome(tg); got != "opened" {
				t.Fatalf("round %d, after the cooldown: %s (%s)", round, got, s.log[len(s.log)-1])
			}
		}
		pr := s.ownPRs(tg)
		m, _ := marker.Find(pr[len(pr)-1].Body, []string{hubFP})
		if !m.Data.Ack {
			t.Errorf("the third close is not remembered as a decline: %+v", m.Data)
		}
	})
	t.Run("merge of a branch other than the base", func(t *testing.T) {
		t.Parallel()
		s, tg := newSimSequence(t, fake.GitLab)
		s.first(false)
		mine, err := s.mergeInto(tg, 1)
		if err != nil {
			t.Fatal(err)
		}
		s.logf("a person merges their branch %s into the sync branch", short(mine))
		s.cycle()
		if got := s.lastOutcome(tg); got != "blocked:edited" {
			t.Errorf("after the merge: %s (%s)", got, s.log[len(s.log)-1])
		}
		s.ship("AGENTS.md", 2)
		s.hubChanged()
		s.logf("the hub ships AGENTS.md v2")
		s.cycle()
		if got := s.lastOutcome(tg); got != "blocked:edited" {
			t.Errorf("after a pack change: %s (%s)", got, s.log[len(s.log)-1])
		}
	})
	t.Run("a branch someone took", func(t *testing.T) {
		t.Parallel()
		s, tg := newSimSequence(t, fake.Gitea)
		if _, err := s.push(tg, "touchmark/acme-eng", map[string]string{"notes.md": s.edit("notes.md")}); err != nil {
			t.Fatal(err)
		}
		s.logf("a person pushes their own sync branch")
		s.cycle()
		if got := s.lastOutcome(tg); got != "blocked:branch-taken" {
			t.Errorf("a taken branch: %s (%s)", got, s.log[len(s.log)-1])
		}
	})
	// A GitHub App's world (API commits, rules read upfront), through the
	// events only its scenarios draw: a person's workflow on the base and
	// Workflows taken away block the rebuild until Update branch merges the
	// base in; a ruleset against force pushes blocks the rebuild under the
	// open pull request; a round of unsigned API commits blocks the push,
	// and the next round delivers.
	t.Run("a GitHub App's rules and permissions", func(t *testing.T) {
		t.Parallel()
		w := newSimWorldOf(t, fake.GitHub, true, "api")
		s := &simScenario{simWorld: w, rng: rand.New(rand.NewPCG(1, 2)), noFaults: true}
		s.c = newSimChecker(w, func(format string, args ...any) {
			t.Helper()
			t.Errorf(format, args...)
		})
		tg := w.target("api")
		step := func(what, want string, events ...func(*simTarget) string) {
			t.Helper()
			for _, e := range events {
				if got := e(tg); got == "" {
					t.Fatalf("%s: an event did not apply", what)
				} else {
					s.logf("%s", got)
				}
			}
			s.cycle()
			if got := s.lastOutcome(tg); got != want {
				t.Fatalf("%s: %s, want %s (%s)", what, got, want, strings.Join(s.log, "\n  "))
			}
		}
		s.first(false)
		step("Workflows taken, a person's workflow on the base, a pack change", "blocked:permission:workflows",
			s.toggleWorkflows, s.teamWorkflow, s.hubBump)
		step("Update branch merges the base in", "updated:content", s.updateBranch)
		step("Workflows granted", "unchanged", s.toggleWorkflows)
		step("force pushes refused, the base moves, a pack change", "blocked:rules:non-fast-forward",
			s.toggleNoForce, s.baseMove, s.hubBump)
		step("force pushes allowed again", "updated:content", s.toggleNoForce)
		s.cycle()
		step("unsigned API commits, a pack change", "blocked:cannot-sign", s.unsignedAPI, s.hubBump)
		step("signed again", "updated:content")
	})
	t.Run("a marker people broke", func(t *testing.T) {
		t.Parallel()
		s, tg := newSimSequence(t, fake.Forgejo)
		s.first(false)
		s.p.UpdatePR(tg.repo.ID, 1, func(pr *platform.PR) { pr.Body = "We rewrote the description." })
		s.logf("a person rewrites #1's description")
		s.cycle()
		if got := s.lastOutcome(tg); got != "blocked:marker-invalid" {
			t.Errorf("a broken marker: %s (%s)", got, s.log[len(s.log)-1])
		}
	})
}
