package distribute

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The property test: random sequences of what hubs and people do (pack
// changes and reverts, a hub id change, merges, closes by people and bots,
// reopenings, pushes to sync branches, Update branch with merge and rebase,
// cherry-picks of touchmark's commit onto people's work, base moves, local
// edits, opt-in changes, someone else's and forks' pull requests, ticked
// controls, operations.yml entries, dropped and archived targets, time
// passing), on every flavor of the fake in git mode. A first distribute
// opens the pull requests; then each round of events is followed by a plan,
// a dry run and distribute (and distribute again right after it when it
// wrote). After every distribute:
//   - I1: every pushed commit sits on B alone and changes exactly D, which
//     the test computes from its model of the hub (and every report's key
//     is the key of that D);
//   - I2: no commit a person pushed to a sync branch becomes unreachable,
//     unless a recreate let touchmark drop it;
//   - I3: touchmark writes only to its own pull requests and pushes only to
//     the hub's sync branches, never to one carrying someone else's open
//     pull request;
//   - I4: no new pull request is covered by the declines people made that
//     are still in force;
//   - I5: the fake saw no forbidden transition;
//   - I7: distribute right after writes nothing;
//   - plan = distribute --dry-run, and distribute does what the dry run
//     said;
//   - the marker of the open pull request a run maintained names what its
//     branch carries.
//
// In one round of simFaultEvery a random write of the distribute goes wrong
// as in the crash matrix (sim_crash_test.go): that run gets the checks that
// hold whatever happened (I1–I5), and the next round must converge under
// all of them.
//
// Every run is real git against the fake's git server, so a scenario costs
// seconds of CPU: by default 100 scenarios of one or two rounds each run,
// and the default run asserts that they still reach the outcomes of
// simCovered (sim_sequences_test.go drives the rarer paths on purpose).
// TOUCHMARK_PROPERTY_SCENARIOS sets the number of scenarios (CI's nightly
// property job runs 10 000, in four shards of 2 500),
// TOUCHMARK_PROPERTY_ROUNDS the most rounds of events a scenario has (2),
// and TOUCHMARK_PROPERTY_SEED the first seed: scenario i runs with seed
// first+i, so
// `go test -run 'TestPropertyDistribute/seed=17$'` replays one (with the
// same TOUCHMARK_PROPERTY_ROUNDS).
func TestPropertyDistribute(t *testing.T) {
	needDeliveryGit(t)
	simHeavy(t)
	n := simEnvInt(t, "TOUCHMARK_PROPERTY_SCENARIOS", 100)
	rounds := max(1, simEnvInt(t, "TOUCHMARK_PROPERTY_ROUNDS", 2))
	first := simEnvInt(t, "TOUCHMARK_PROPERTY_SEED", 1)
	cov := newSimCoverage()
	// The parent's cleanup runs once every scenario is done.
	t.Cleanup(func() { cov.check(t, n, first == 1 && rounds == 2) })
	for i := range n {
		seed := uint64(first + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			runSimScenario(t, seed, rounds, cov)
		})
	}
}

// simEnvInt reads a positive number from the environment, def when unset.
func simEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		t.Fatalf("%s=%q is not a number", name, v)
	}
	return n
}

// simMaxEvents is the most events a round of a scenario has.
const simMaxEvents = 4

// simScenario is one scenario: its world, its checks, its random source
// and the events so far.
type simScenario struct {
	*simWorld
	c       *simChecker
	rng     *rand.Rand
	log     []string
	renames int
	// editDeps changes the deps of every run of the scenario (nil for
	// none).
	editDeps func(*Deps)
	// faulted is set while the last round's distribute met a fault.
	faulted bool
	// noFaults turns the faulted rounds off (the fixed sequences).
	noFaults bool
	// reached are the outcomes the scenario's runs reached
	// ("outcome:reason"), for the coverage of the property test (nil for
	// none).
	reached map[string]bool
}

// cover records the outcomes of rep's targets.
func (s *simScenario) cover(rep *report.Delivery) {
	if s.reached == nil {
		return
	}
	for _, tg := range rep.Targets {
		s.reached[string(tg.Outcome)+":"+tg.Reason] = true
	}
}

// runSimScenario runs the scenario of seed: a flavor and one or two
// targets, a first distribute, then one to maxRounds rounds of events and
// runs. The first distribute opens a pull request in every target, the
// same in every scenario, so its plan and dry run are left out, and one
// scenario in four runs distribute again after it.
func runSimScenario(t *testing.T, seed uint64, maxRounds int, cov *simCoverage) {
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	flavors := []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea, fake.Forgejo}
	flavor := flavors[rng.IntN(len(flavors))]
	names := []string{"api"}
	if rng.IntN(5) == 0 {
		names = append(names, "web")
	}
	rounds := 1 + rng.IntN(maxRounds)
	// One GitHub scenario in three has a GitHub App writer that must sign:
	// its pushes go through the API commit. The seed decides it, not the
	// generator, so every scenario draws the same events as before.
	app := flavor == fake.GitHub && seed%3 == 1
	w := newSimWorldOf(t, flavor, app, names...)
	s := &simScenario{simWorld: w, rng: rng, reached: map[string]bool{}}
	s.c = newSimChecker(w, func(format string, args ...any) {
		t.Helper()
		t.Errorf("round %d: "+format, append([]any{s.round()}, args...)...)
	})
	defer func() {
		if cov != nil {
			cov.add(s.reached)
		}
		if t.Failed() {
			t.Logf("seed %d, flavor %s (App %v), targets %v; what happened:\n  %s", seed, flavor, app, names, strings.Join(s.log, "\n  "))
			return
		}
		faults := 0
		for _, l := range s.log {
			if strings.HasPrefix(l, "runs, ") {
				faults++
			}
		}
		t.Logf("%s (App %v), %d targets, %d rounds of runs, %d events, %d faults", flavor, app, len(names), s.round(), len(s.log)-s.round(), faults)
	}()
	s.first(seed%4 == 0)
	for range rounds {
		for range 1 + rng.IntN(simMaxEvents) {
			s.event()
		}
		s.cycle()
		if t.Failed() {
			return
		}
	}
	if s.faulted {
		s.settle(s.editDeps) // the last round met a fault: the next run converges
	}
}

// round is the number of the rounds of runs so far.
func (s *simScenario) round() int {
	n := 0
	for _, l := range s.log {
		if strings.HasPrefix(l, "runs") {
			n++
		}
	}
	return n
}

func (s *simScenario) logf(format string, args ...any) {
	s.log = append(s.log, fmt.Sprintf(format, args...))
}

// simFaultEvery is how rare a faulted round is: in one round of so many,
// a random write of the round's distribute goes wrong as in the crash
// matrix (a 401 before or after the platform applies it, a lost answer, a
// cancelled run). The checks of that run are those that hold whatever
// happened (I1–I5, but for the push to the branch of a merged pull request
// a stopped run leaves before its new pull request); the next round
// converges under the full checks.
const simFaultEvery = 6

// cycle runs a plan and a dry run of the state the events left (they must
// agree and write nothing), then distribute, whose run the checks judge,
// then distribute again, which must write nothing (I7). A distribute that
// wrote nothing left the state as it was, so the second run is skipped
// then: it would read the same inputs. In a faulted round the distribute
// meets a fault, and the second run is left to the next round.
func (s *simScenario) cycle() {
	t := s.t
	t.Helper()
	if s.apiUnsigned {
		defer func() {
			s.apiUnsigned = false
			s.p.SetAPISigning(true)
		}()
	}
	c := s.c
	c.observe()
	s.p.ResetCalls()
	plan := s.run(ModePlan, s.editDeps)
	dry := s.run(ModeDryRun, s.editDeps)
	if writes := s.p.Writes(); len(writes) > 0 {
		c.failf("plan and dry run wrote: %q", writes)
	}
	c.samePlan(plan, dry)
	s.cover(dry)
	c.checkKeys(dry)
	c.release()
	before := c.snapshot()
	s.p.ResetCalls()
	var f *simFaults
	if s.rng.IntN(simFaultEvery) == 0 && !s.noFaults {
		f = &simFaults{w: s.simWorld, k: s.rng.IntN(3)}
	}
	rep, fault := s.distribute(f)
	writes := s.p.Writes()
	s.faulted = fault != ""
	for _, tg := range s.targets {
		if refs := s.p.HiddenRefs(tg.repo.ID); len(refs) > 0 {
			c.failf("the run left %q in %s: the stage ref never survives a run", refs, tg.repo.Path)
		}
	}
	if s.faulted {
		s.logf("runs, %s: %s (%d writes)", fault, outcomes(rep), len(writes))
		c.checkFaulted(rep, writes, before)
		return
	}
	s.logf("runs: %s (%d writes)", outcomes(rep), len(writes))
	c.checkRun(rep, dry, writes, before)
	if len(writes) == 0 {
		return
	}
	c.observe()
	s.p.ResetCalls()
	again := s.run(ModeDistribute, s.editDeps)
	if writes := s.unsignedRetries(s.p.Writes()); len(writes) > 0 {
		c.failf("I7: distribute right after wrote %q (%s)", writes, outcomes(again))
	}
	if v := s.p.Violations(); len(v) > 0 {
		c.failf("I5: forbidden transitions in the second run: %q", v)
	}
	for _, tg := range again.Targets {
		if tg.Outcome == report.OutcomeFailed {
			c.failf("the second run failed %s: %s: %q", tg.Path, tg.Reason, tg.Warnings)
		}
	}
}

// unsignedRetries drops from writes, while the platform's API commits come
// back unsigned, what a run tries again whatever the run before it met:
// the push to the stage ref, the API commit and the stage ref's deletion
// (runs keep no state, and the platform may have started signing). Those
// pushes move no branch: the fake logs them without one.
func (s *simScenario) unsignedRetries(writes []string) []string {
	if !s.apiUnsigned {
		return writes
	}
	return slices.DeleteFunc(slices.Clone(writes), func(w string) bool {
		f := strings.Fields(w)
		return f[0] == "Commit" || (f[0] == "Push" && len(f) == 2)
	})
}

// distribute runs distribute; with f, a random kind of fault lands on its
// f.k-th write. fault says what went wrong, "" when nothing did (the run
// wrote less than that).
func (s *simScenario) distribute(f *simFaults) (rep *report.Delivery, fault string) {
	if f == nil {
		return s.run(ModeDistribute, s.editDeps), ""
	}
	kind := pick(s, simFaultKinds)
	f.applied, f.err = kind.applied, kind.err
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	if kind.interrupt {
		f.cancel = cancel
	}
	remove := gitx.TraceCommands(f.trace)
	defer remove()
	d := s.deps(ModeDistribute)
	if s.editDeps != nil {
		s.editDeps(&d)
	}
	d.Providers[0].Writer = &exWriter{Writer: s.p.Writer(s.writer), target: func(tw platform.TargetWriter) platform.TargetWriter {
		return exWrap(tw, f.api)
	}}
	rep, err := Run(ctx, d, ModeDistribute)
	if err != nil {
		s.t.Fatalf("Run: %v", err)
	}
	s.ok()
	checkReport(s.t, rep)
	if f.hit != "" {
		fault = fmt.Sprintf("%s on write %d (%s)", kind.name, f.k, f.hit)
	}
	return rep, fault
}

// settle runs distribute, which the checks judge as in cycle but for the
// dry run's prediction (the run after a crash or a mid-run event is the one
// that converges), and distribute again, which must write nothing (I7).
func (s *simScenario) settle(edit func(*Deps)) {
	c := s.c
	c.observe()
	c.release()
	before := c.snapshot()
	s.p.ResetCalls()
	rep := s.run(ModeDistribute, edit)
	writes := s.p.Writes()
	s.cover(rep)
	s.logf("runs: %s (%d writes)", outcomes(rep), len(writes))
	c.checkRun(rep, rep, writes, before)
	c.observe()
	s.p.ResetCalls()
	again := s.run(ModeDistribute, edit)
	if writes := s.p.Writes(); len(writes) > 0 {
		c.failf("I7: distribute right after wrote %q (%s)", writes, outcomes(again))
	}
	if v := s.p.Violations(); len(v) > 0 {
		c.failf("I5: forbidden transitions in the second run: %q", v)
	}
}

// first runs the first distribute, which the checks judge like every
// other, and with again distribute again, which must write nothing (I7).
func (s *simScenario) first(again bool) {
	c := s.c
	c.observe()
	before := c.snapshot()
	s.p.ResetCalls()
	rep := s.run(ModeDistribute, s.editDeps)
	writes := s.p.Writes()
	s.cover(rep)
	s.logf("runs: %s (%d writes)", outcomes(rep), len(writes))
	for _, tg := range rep.Targets {
		if tg.Outcome != report.OutcomeOpened {
			c.failf("the first run: %s is %s:%s (%q)", tg.Path, tg.Outcome, tg.Reason, tg.Warnings)
		}
	}
	c.checkRun(rep, rep, writes, before)
	if !again {
		return
	}
	c.observe()
	s.p.ResetCalls()
	second := s.run(ModeDistribute, s.editDeps)
	if writes := s.p.Writes(); len(writes) > 0 {
		c.failf("I7: distribute right after the first wrote %q (%s)", writes, outcomes(second))
	}
}

// simEvent is one kind of event, with its weight among the others. do
// applies it to target tg and returns what it did, "" when it does not
// apply now.
type simEvent struct {
	name   string
	weight int
	do     func(s *simScenario, tg *simTarget) string
}

// simEvents are the events of the scenarios.
var simEvents = []simEvent{
	{"hub-bump", 6, (*simScenario).hubBump},
	{"hub-revert", 3, (*simScenario).hubRevert},
	{"hub-retire", 2, (*simScenario).hubRetire},
	{"hub-rename", 1, (*simScenario).hubRename},
	{"merge", 5, (*simScenario).merge},
	{"close-person", 5, func(s *simScenario, tg *simTarget) string { return s.closeOwn(tg, s.person) }},
	{"close-bot", 4, func(s *simScenario, tg *simTarget) string { return s.closeOwn(tg, s.bot) }},
	{"reopen", 3, (*simScenario).reopen},
	{"push-sync", 4, (*simScenario).pushSync},
	{"base-move", 3, (*simScenario).baseMove},
	{"update-branch", 3, (*simScenario).updateBranch},
	{"rebase-branch", 3, (*simScenario).rebaseBranch},
	{"cherry-pick", 2, (*simScenario).cherryPickEvent},
	{"merge-feature", 2, (*simScenario).mergeFeature},
	{"local-edit", 3, (*simScenario).localEdit},
	{"restore-file", 2, (*simScenario).restoreFile},
	{"opt-in", 4, (*simScenario).changeOptIn},
	{"opt-out", 1, (*simScenario).optOut},
	{"foreign-pr", 2, (*simScenario).foreignPREvent},
	{"other-hub-pr", 1, (*simScenario).otherHubPREvent},
	{"fork-pr", 1, (*simScenario).forkPREvent},
	{"close-foreign", 2, (*simScenario).closeForeign},
	{"delete-sync", 2, (*simScenario).deleteSync},
	{"tick-recreate", 4, (*simScenario).tickRecreate},
	{"tick-repropose", 3, (*simScenario).tickRepropose},
	{"ops-recreate", 2, (*simScenario).opsRecreate},
	{"ops-forget", 2, (*simScenario).opsForget},
	{"ops-clear", 1, func(s *simScenario, _ *simTarget) string { s.ops = nil; return "operations.yml emptied" }},
	{"drop", 1, (*simScenario).drop},
	{"retitle", 1, (*simScenario).retitle},
	{"unlabel", 1, (*simScenario).unlabel},
	{"time", 2, func(s *simScenario, _ *simTarget) string {
		s.clock.Advance(31 * 24 * time.Hour)
		return "31 days pass"
	}},
	{"archive", 1, (*simScenario).archive},
}

// simAppEvents are the events of a GitHub App's world only (simWorld.app): a
// ruleset against force pushes on a target's sync branch toggled, the
// installation's Workflows permission toggled and a person's workflow on a
// default branch, and a round in which the platform's API commits come back
// unsigned. They are drawn only there, so the other scenarios draw what they
// drew before.
var simAppEvents = []simEvent{
	{"no-force", 4, (*simScenario).toggleNoForce},
	{"workflows", 4, (*simScenario).toggleWorkflows},
	{"team-workflow", 4, (*simScenario).teamWorkflow},
	{"unsigned-api", 3, (*simScenario).unsignedAPI},
}

// event applies one random event that applies now (at most 20 tries).
func (s *simScenario) event() {
	events := simEvents
	if s.app {
		events = append(slices.Clone(simEvents), simAppEvents...)
	}
	total := 0
	for _, e := range events {
		total += e.weight
	}
	for range 20 {
		k := s.rng.IntN(total)
		var e simEvent
		for _, e = range events {
			if k < e.weight {
				break
			}
			k -= e.weight
		}
		tg := s.targets[s.rng.IntN(len(s.targets))]
		if what := e.do(s, tg); what != "" {
			s.logf("%s %s: %s", e.name, tg.name, what)
			s.ok()
			return
		}
	}
}

// pick returns a random element of list.
func pick[T any](s *simScenario, list []T) T { return list[s.rng.IntN(len(list))] }

// shipped returns the paths the hub ships now, sorted.
func (s *simScenario) shipped() []string {
	var out []string
	for _, files := range s.files {
		for path := range files {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

func (s *simScenario) toggleNoForce(tg *simTarget) string {
	s.noForce[tg.name] = !s.noForce[tg.name]
	s.setRules(tg)
	if s.noForce[tg.name] {
		return "a ruleset refuses force pushes to touchmark/*"
	}
	return "the ruleset against force pushes to touchmark/* is gone"
}

func (s *simScenario) toggleWorkflows(*simTarget) string {
	s.workflowsOff = !s.workflowsOff
	for _, tg := range s.targets {
		s.setRules(tg)
	}
	if s.workflowsOff {
		return "the owner takes the Workflows permission from the writer's installation"
	}
	return "the owner grants the writer's installation Workflows"
}

func (s *simScenario) teamWorkflow(tg *simTarget) string {
	path := fmt.Sprintf(".github/workflows/team-%d.yml", s.edits+1)
	if _, err := s.push(tg, tg.repo.DefaultBranch, map[string]string{path: s.edit(path)}); err != nil {
		return ""
	}
	return "a person adds " + path + " to the default branch"
}

// unsignedAPI makes the platform's API commits unsigned until the end of
// the round's runs (cycle turns signing back on).
func (s *simScenario) unsignedAPI(*simTarget) string {
	if s.apiUnsigned {
		return ""
	}
	s.apiUnsigned = true
	s.p.SetAPISigning(false)
	return "the platform's API commits come back unsigned for a round"
}

func (s *simScenario) hubBump(*simTarget) string {
	path := pick(s, s.shipped())
	n := s.versions[path] + 1
	s.ship(path, n)
	s.hubChanged()
	return fmt.Sprintf("the hub ships %s v%d", path, n)
}

func (s *simScenario) hubRevert(*simTarget) string {
	var old []string
	for _, path := range s.shipped() {
		if s.versions[path] >= 2 {
			old = append(old, path)
		}
	}
	if len(old) == 0 {
		return ""
	}
	path := pick(s, old)
	n := 1 + s.rng.IntN(s.versions[path])
	if s.files[simPacks[path]][path] == simContent(path, n) {
		return ""
	}
	s.ship(path, n)
	s.hubChanged()
	return fmt.Sprintf("the hub goes back to %s v%d", path, n)
}

func (s *simScenario) hubRetire(*simTarget) string {
	const path = "docs/old.md"
	defer s.hubChanged()
	if _, ok := s.files[simBase][path]; ok {
		s.retire(path)
		return "the hub stops shipping " + path
	}
	n := s.versions[path] + 1
	s.ship(path, n)
	return fmt.Sprintf("the hub ships %s v%d again", path, n)
}

func (s *simScenario) hubRename(*simTarget) string {
	if s.renames >= 2 {
		return ""
	}
	s.renames++
	s.renameHub(fmt.Sprintf("acme-eng-%d", s.renames+1))
	return "the hub's id is now " + s.hubID
}

// openOwnPR returns a random open pull request of touchmark's in tg.
func (s *simScenario) openOwnPR(tg *simTarget) (platform.PR, bool) {
	open := s.openOwn(tg)
	if len(open) == 0 {
		return platform.PR{}, false
	}
	return pick(s, open), true
}

func (s *simScenario) merge(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok {
		return ""
	}
	how := pick(s, []fake.MergeHow{fake.MergeCommit, fake.MergeSquash, fake.MergeRebase})
	if _, err := s.p.MergePR(tg.repo.ID, pr.Number, how, s.person, time.Time{}); err != nil {
		how = fake.MergeCommit
		if _, err := s.p.MergePR(tg.repo.ID, pr.Number, how, s.person, time.Time{}); err != nil {
			return ""
		}
	}
	return fmt.Sprintf("#%d merged (%d)", pr.Number, how)
}

func (s *simScenario) closeOwn(tg *simTarget, by platform.Account) string {
	pr, ok := s.openOwnPR(tg)
	if !ok {
		return ""
	}
	s.p.SetPRState(tg.repo.ID, pr.Number, platform.Closed, &by, time.Time{})
	return fmt.Sprintf("#%d closed by %s", pr.Number, by.Login)
}

func (s *simScenario) reopen(tg *simTarget) string {
	var closed []platform.PR
	for _, pr := range s.ownPRs(tg) {
		if pr.State != platform.Closed || s.p.Branch(tg.repo.ID, pr.Head) == "" {
			continue
		}
		busy := slices.ContainsFunc(s.prs(tg), func(o platform.PR) bool {
			return o.State == platform.Open && o.Head == pr.Head && o.HeadRepoID == tg.repo.ID
		})
		if !busy {
			closed = append(closed, pr)
		}
	}
	if len(closed) == 0 {
		return ""
	}
	pr := pick(s, closed)
	s.p.SetPRState(tg.repo.ID, pr.Number, platform.Open, nil, time.Time{})
	return fmt.Sprintf("#%d reopened", pr.Number)
}

// syncBranch returns a random sync branch of tg that exists.
func (s *simScenario) syncBranch(tg *simTarget) (string, bool) {
	var have []string
	for _, b := range s.branches() {
		if s.p.Branch(tg.repo.ID, b) != "" {
			have = append(have, b)
		}
	}
	if len(have) == 0 {
		return "", false
	}
	return pick(s, have), true
}

func (s *simScenario) pushSync(tg *simTarget) string {
	b, ok := s.syncBranch(tg)
	if !ok {
		return ""
	}
	path := fmt.Sprintf("notes/%d.md", s.edits+1)
	if s.rng.IntN(2) == 0 {
		path = pick(s, s.shipped())
	}
	head, err := s.push(tg, b, map[string]string{path: s.edit(path)})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("a person pushes %s to %s (%s)", path, b, short(head))
}

func (s *simScenario) baseMove(tg *simTarget) string {
	path := fmt.Sprintf("src/%d.txt", s.edits+1)
	if _, err := s.push(tg, tg.repo.DefaultBranch, map[string]string{path: s.edit(path)}); err != nil {
		return ""
	}
	return "a person pushes " + path + " to the default branch"
}

func (s *simScenario) updateBranch(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok || s.baseMove(tg) == "" {
		return ""
	}
	if _, err := s.p.UpdateBranch(tg.repo.ID, pr.Number, s.person, time.Time{}); err != nil {
		return "the base moves (Update branch failed: " + err.Error() + ")"
	}
	return fmt.Sprintf("the base moves, and Update branch merges it into #%d", pr.Number)
}

func (s *simScenario) rebaseBranch(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok || s.baseMove(tg) == "" {
		return ""
	}
	if _, err := s.p.RebaseBranch(tg.repo.ID, pr.Number, s.person, time.Time{}); err != nil {
		return "the base moves (Update branch with rebase failed: " + err.Error() + ")"
	}
	return fmt.Sprintf("the base moves, and Update branch rebases #%d onto it", pr.Number)
}

func (s *simScenario) cherryPickEvent(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok {
		return ""
	}
	mine, err := s.cherryPick(tg, pr.Number)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("a person puts touchmark's commit of #%d onto their commit %s", pr.Number, short(mine))
}

func (s *simScenario) mergeFeature(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok {
		return ""
	}
	mine, err := s.mergeInto(tg, pr.Number)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("a person merges their branch (%s) into #%d's branch", short(mine), pr.Number)
}

func (s *simScenario) localEdit(tg *simTarget) string {
	path := pick(s, s.shipped())
	if _, err := s.push(tg, tg.repo.DefaultBranch, map[string]string{path: s.edit(path)}); err != nil {
		return ""
	}
	return "the team makes " + path + " its own"
}

func (s *simScenario) restoreFile(tg *simTarget) string {
	tree := s.tree(tg)
	var have []string
	for path := range simPacks {
		if _, ok := tree[path]; ok {
			have = append(have, path)
		}
	}
	if len(have) == 0 {
		return ""
	}
	slices.Sort(have)
	path := pick(s, have)
	content, what := "", "deletes "+path
	if c, ok := s.files[simPacks[path]][path]; ok && s.rng.IntN(2) == 0 {
		content, what = c, "copies the hub's "+path
	}
	if _, err := s.push(tg, tg.repo.DefaultBranch, map[string]string{path: content}); err != nil {
		return ""
	}
	return "the team " + what + " on the default branch"
}

func (s *simScenario) changeOptIn(tg *simTarget) string {
	old := s.optIn(tg)
	if old == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("version: 1\n")
	if s.rng.IntN(3) == 0 {
		fmt.Fprintf(&b, "# note %d\n", s.edits)
	}
	if s.rng.IntN(3) == 0 {
		b.WriteString("packs: [extra]\n")
	}
	var ignore []string
	for _, path := range []string{"AGENTS.md", "docs/guide.md", "docs/old.md", "docs/extra.md"} {
		if s.rng.IntN(4) == 0 {
			ignore = append(ignore, path)
		}
	}
	if len(ignore) > 0 {
		fmt.Fprintf(&b, "ignore: [%s]\n", strings.Join(ignore, ", "))
	}
	s.edits++
	if b.String() == old {
		return ""
	}
	if err := s.setOptIn(tg, b.String()); err != nil {
		return ""
	}
	return fmt.Sprintf("the opt-in file becomes %q", b.String())
}

func (s *simScenario) optOut(tg *simTarget) string {
	if s.optIn(tg) == "" {
		if s.setOptIn(tg, "version: 1\n") != nil {
			return ""
		}
		return "the team opts in again"
	}
	if s.setOptIn(tg, "") != nil {
		return ""
	}
	return "the team deletes its opt-in file"
}

// foreignPREvent opens a person's pull request from the sync branch, also
// while touchmark's own is open there (to another base, say).
func (s *simScenario) foreignPREvent(tg *simTarget) string {
	n, err := s.foreignPR(tg)
	if err != nil {
		return ""
	}
	s.c.addForeign(tg, n)
	return fmt.Sprintf("a person opens #%d from the sync branch", n)
}

// otherHubPREvent opens a pull request of another hub that shares the id
// and the writer from the sync branch, also while touchmark's own is open
// there.
func (s *simScenario) otherHubPREvent(tg *simTarget) string {
	n, err := s.otherHubPR(tg)
	if err != nil {
		return ""
	}
	s.c.addForeign(tg, n)
	return fmt.Sprintf("another hub with the same id opens #%d from the sync branch", n)
}

func (s *simScenario) forkPREvent(tg *simTarget) string {
	n := s.forkPR(tg)
	s.c.addForeign(tg, n)
	return fmt.Sprintf("a fork opens #%d with the sync branch's name and a copy of the marker", n)
}

func (s *simScenario) closeForeign(tg *simTarget) string {
	var open []int64
	for n := range s.c.foreign[tg.name] {
		if s.p.PR(tg.repo.ID, n).State == platform.Open {
			open = append(open, n)
		}
	}
	if len(open) == 0 {
		return ""
	}
	slices.Sort(open)
	n := pick(s, open)
	s.p.SetPRState(tg.repo.ID, n, platform.Closed, &s.person, time.Time{})
	return fmt.Sprintf("someone else's #%d closed", n)
}

func (s *simScenario) deleteSync(tg *simTarget) string {
	b, ok := s.syncBranch(tg)
	if !ok {
		return ""
	}
	s.p.DeleteBranch(tg.repo.ID, b)
	return "a person deletes " + b
}

func (s *simScenario) tickRecreate(tg *simTarget) string {
	for _, pr := range s.openOwn(tg) {
		if s.tick(tg, pr.Number, prbody.ControlRecreate) {
			return fmt.Sprintf("a person ticks rebuild in #%d", pr.Number)
		}
	}
	return ""
}

func (s *simScenario) tickRepropose(tg *simTarget) string {
	for _, pr := range s.ownPRs(tg) {
		if pr.State == platform.Closed && s.tick(tg, pr.Number, prbody.ControlRepropose) {
			s.c.revoke(tg, pr.Number)
			return fmt.Sprintf("a person ticks propose again in #%d", pr.Number)
		}
	}
	return ""
}

func (s *simScenario) opsRecreate(tg *simTarget) string {
	b, ok := s.syncBranch(tg)
	if !ok {
		return ""
	}
	head := s.p.Branch(tg.repo.ID, b)
	if s.ops == nil {
		s.ops = &config.Operations{Version: 1}
	}
	s.ops.Recreate = append(s.ops.Recreate, config.RecreateOp{Target: "gh:" + tg.repo.Path, Head: head})
	return fmt.Sprintf("operations.yml recreates %s at %s", b, short(head))
}

func (s *simScenario) opsForget(tg *simTarget) string {
	var closed []int64
	for _, pr := range s.ownPRs(tg) {
		if pr.State == platform.Closed {
			closed = append(closed, pr.Number)
		}
	}
	if len(closed) == 0 {
		return ""
	}
	n := pick(s, closed)
	if s.ops == nil {
		s.ops = &config.Operations{Version: 1}
	}
	s.ops.ForgetDeclines = append(s.ops.ForgetDeclines, config.ForgetOp{Target: "gh:" + tg.repo.Path, PR: n})
	s.c.revoke(tg, n)
	return fmt.Sprintf("operations.yml forgets #%d", n)
}

func (s *simScenario) drop(tg *simTarget) string {
	if s.excluded[tg.name] {
		delete(s.excluded, tg.name)
		return "targets.yml takes it back"
	}
	s.excluded[tg.name] = true
	return "targets.yml excludes it"
}

func (s *simScenario) retitle(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok {
		return ""
	}
	s.p.UpdatePR(tg.repo.ID, pr.Number, func(pr *platform.PR) { pr.Title = "Sync, with the team's own title" })
	return fmt.Sprintf("a person retitles #%d", pr.Number)
}

func (s *simScenario) unlabel(tg *simTarget) string {
	pr, ok := s.openOwnPR(tg)
	if !ok || len(pr.Labels) == 0 {
		return ""
	}
	s.p.UpdatePR(tg.repo.ID, pr.Number, func(pr *platform.PR) { pr.Labels = nil })
	return fmt.Sprintf("a person takes the labels off #%d", pr.Number)
}

func (s *simScenario) archive(tg *simTarget) string {
	var archived bool
	s.p.UpdateRepo(tg.repo.ID, func(r *platform.Repo) {
		r.Archived = !r.Archived
		archived = r.Archived
	})
	if archived {
		return "the repository is archived"
	}
	return "the repository is unarchived"
}
