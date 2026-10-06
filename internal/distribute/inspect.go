package distribute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// inspectAll runs phase C over list: each provider's targets in the order
// of list, through a pool of the provider's own (eachByProvider). Every
// target writes only its own result, so the output order does not depend
// on the schedule.
func (r *run) inspectAll(ctx context.Context, list []*target) {
	r.prepareBatches(list)
	r.eachByProvider(list, func(i int) { r.inspectOne(ctx, list[i]) })
}

// eachByProvider runs do for every index of list: the targets of each
// provider in the order of list, through a pool of workers of that
// provider's own, at most Deps.Concurrency of them and at most as many as
// its limits let it read at once (throttle.Limits.Inspections). Providers
// share no pool (every provider has its own semaphores), so a provider that
// waits out a pause or its read budget holds up its own targets only. A
// concurrency of 1 runs the whole list in order.
func (r *run) eachByProvider(list []*target, do func(i int)) {
	n := r.d.Concurrency
	if n <= 0 {
		n = defaultConcurrency
	}
	if n <= 1 {
		for i := range list {
			do(i)
		}
		return
	}
	byProv := map[*provider][]int{}
	var provs []*provider
	for i, t := range list {
		if byProv[t.prov] == nil {
			provs = append(provs, t.prov)
		}
		byProv[t.prov] = append(byProv[t.prov], i)
	}
	var wg sync.WaitGroup
	for _, p := range provs {
		idx := byProv[p]
		next := make(chan int)
		workers := min(n, len(idx))
		if k := p.throttle().Limits().Inspections; k > 0 {
			workers = min(workers, k)
		}
		for range workers {
			wg.Go(func() {
				for i := range next {
					do(i)
				}
			})
		}
		wg.Go(func() {
			defer close(next)
			for _, i := range idx {
				next <- i
			}
		})
	}
	wg.Wait()
}

// inspectOne inspects t in phase C and records that it finished.
func (r *run) inspectOne(ctx context.Context, t *target) {
	t.work = r.inspectSafe(ctx, t, r.inspectBy)
	r.inspected(t)
}

// inspected records that t's inspection finished: its rank in the
// completion order, and, unless its work goes on to phase F, the release of
// its repository. The work's snapshot tree, which phase F never reads, is
// dropped: a work waits for phase F with the whole fleet's.
func (r *run) inspected(t *target) {
	r.mu.Lock()
	r.finishedAt++
	t.order = r.finishedAt
	r.mu.Unlock()
	if t.work != nil {
		t.work.Tree = nil
	}
	if r.mode != ModeDistribute || t.work == nil || !t.work.writes() {
		r.release(t)
	}
}

// inspectSafe inspects t unless by has passed (zero for no limit), and
// turns a panic, a bug, into failed:internal for that target, so the other
// targets and the report survive it.
func (r *run) inspectSafe(ctx context.Context, t *target, by time.Time) (w *Work) {
	defer func() {
		if v := recover(); v != nil {
			t.res.Outcome, t.res.Reason = report.OutcomeFailed, "internal"
			t.res.Warnings = append(t.res.Warnings, fmt.Sprintf("internal error: %v", v))
			w = nil
		}
	}()
	return r.inspect(ctx, t, by)
}

// inspect runs the steps of phase C for one target, from a fresh report
// line, and returns its work: nil when the target's outcome needs none (its
// report line says why), and always nil for the snapshot-only plan. Past by
// (phase C: before Write.Deadline, keeping time for phase F; a re-inspection
// of phase F: Write.Deadline) the target is deferred:deadline without a
// call, and so is a target whose provider's circuit is open.
func (r *run) inspect(ctx context.Context, t *target, by time.Time) *Work {
	// The provider's Gate paces the reads; a re-inspection of phase F keeps
	// the meter of its target's block.
	ctx = throttle.Default(ctx, t.prov.throttle())
	t.res = report.DeliveryTarget{Provider: t.prov.cfg.ID, Host: t.host, Warnings: slices.Clone(t.warnings)}
	res := &t.res
	if t.hidden {
		res.Outcome, res.Reason = report.OutcomeSkipped, report.ReasonPrivate
		res.Warnings = nil
		r.leaveByHub(t, nil)
		return nil
	}
	res.RepoID, res.Path = t.repo.ID, t.repo.Path
	if reason := skipReason(t.repo); reason != "" {
		res.Outcome, res.Reason = report.OutcomeSkipped, reason
		r.leaveByHub(t, nil)
		return nil
	}
	if err := ctx.Err(); err != nil {
		r.fail(ctx, t, "inspect", err, "internal")
		return nil
	}
	if !by.IsZero() && !r.timeOf(t.prov).Before(by) {
		res.Outcome, res.Reason = report.OutcomeDeferred, "deadline"
		res.Warnings = append(res.Warnings, "the run's deadline left no time to inspect the target")
		return nil
	}
	if r.deferredByCircuit(t) {
		return nil
	}
	optIn, sel, tree, ok := r.optInAndTree(ctx, t)
	if !ok {
		return nil
	}
	res.Packs = sel.Packs
	if r.repos == nil {
		r.inspectSnapshotOnly(ctx, t, optIn, sel, tree)
		return nil
	}
	return r.inspectFull(ctx, t, optIn, sel, tree)
}

// optInAndTree reads and parses the opt-in file, selects the packs and
// takes the snapshot. When the tree holds another opt-in blob than the
// API returned (the default branch moved in between), the opt-in file is
// read again at the tree's commit. ok is false when the target's outcome
// is decided, or a limited plan leaves the target out (scope.go): then no
// snapshot is taken.
//
// A target that targets.yml subscribes (an entry that selects it has
// opt_in: assumed) and that has no opt-in file is processed as if it had an
// empty one: the packs of defaults and of its entries, nothing ignored. An
// opt-in file that says enabled: false opts any target out:
// skipped:opted-out, and the sweep closes its pull requests. When the API
// found the opt-in file of a subscribed target and the tree does not hold
// it, the file is read again at the tree's commit too: its deletion returns
// the target to the subscription, and must not make it look not opted in,
// which the sweep would take for an opt-out.
//
// A plan with Deps.AssumeOptIn plans a target without an opt-in file, or
// with one that is not a regular file or is too large, as if it had an empty
// one (for the report only). Either way the report line says so (Assumed,
// AssumedBy). Only a file the API did not find and the tree holds is read
// again at the tree's commit: an unsafe one stays assumed.
func (r *run) optInAndTree(ctx context.Context, t *target) (optIn *config.OptIn, sel config.Selection, tree *snapshot.Tree, ok bool) {
	res := &t.res
	assume := r.mode == ModePlan && r.d.AssumeOptIn
	ref := ""
	for attempt := 0; ; attempt++ {
		file, err := r.readOptIn(ctx, t, ref)
		// assumed: the run takes an empty opt-in file; unsafe: because the
		// API found one that is not a regular file or is too large (plan
		// --assume-opt-in only).
		assumed, unsafe := false, false
		switch {
		case err == nil:
		case isNotFound(err) && ctx.Err() == nil && (t.assumed || assume):
			assumed = true
		case isNotFound(err) && ctx.Err() == nil:
			res.Outcome, res.Reason = report.OutcomeSkipped, reasonNotOptedIn
			r.leaveByHub(t, nil)
			return nil, sel, nil, false
		case errors.Is(err, platform.ErrNotRegular) || errors.Is(err, platform.ErrTooLarge):
			if !slices.Contains(res.Warnings, err.Error()) {
				res.Warnings = append(res.Warnings, err.Error())
			}
			if assume {
				assumed, unsafe = true, true
				break
			}
			res.Outcome, res.Reason = report.OutcomeSkipped, "unsafe-opt-in"
			r.leaveByHub(t, nil)
			return nil, sel, nil, false
		default:
			r.fail(ctx, t, "read "+r.optIn, err, "internal")
			return nil, sel, nil, false
		}
		o, parseWarns, err := &config.OptIn{Version: 1}, []config.Warning(nil), error(nil)
		if !assumed {
			o, parseWarns, err = config.ParseOptIn(file.Content)
		}
		if err != nil {
			res.Outcome, res.Reason = report.OutcomeBlocked, "opt-in-invalid"
			res.Warnings = append(res.Warnings, r.optIn+": "+strings.Join(flatten(err), "\n"))
			r.leaveByHub(t, nil)
			return nil, sel, nil, false
		}
		if o.Disabled() {
			res.Outcome, res.Reason = report.OutcomeSkipped, reasonOptedOut
			r.leaveByHub(t, nil)
			return nil, sel, nil, false
		}
		s, warns, err := config.SelectFor(r.hub, r.targets, o, t.entries, r.known)
		if err != nil {
			res.Outcome, res.Reason = report.OutcomeBlocked, "opt-in-invalid"
			res.Warnings = append(res.Warnings, err.Error())
			r.leaveByHub(t, o.Packs)
			return nil, sel, nil, false
		}
		if r.leaveBySelection(t, s.Packs) {
			return nil, sel, nil, false
		}
		warns = append(parseWarns, warns...)
		if tree == nil {
			if tree, ok = r.snapshot(ctx, t); !ok {
				return nil, sel, nil, false
			}
		}
		e, inTree := tree.Entries[r.optIn]
		regular := inTree && (e.Mode == "100644" || e.Mode == "100755")
		switch {
		case assumed && !unsafe && inTree && attempt == 0 && tree.Commit != "":
			// The file came in between: read it at the snapshot's commit.
			ref = tree.Commit
			continue
		case !regular && assume:
			assumed = true
			o = &config.OptIn{Version: 1}
			if s, warns, err = config.SelectFor(r.hub, r.targets, o, t.entries, r.known); err != nil {
				res.Outcome, res.Reason = report.OutcomeBlocked, "opt-in-invalid"
				res.Warnings = append(res.Warnings, err.Error())
				return nil, sel, nil, false
			}
		case !inTree && assumed:
			// Subscribed by targets.yml, and no opt-in file on either side.
		case !inTree && t.assumed && attempt == 0 && tree.Commit != "":
			// The file of a subscribed target went away in between: read it
			// again at the snapshot's commit, where the subscription holds.
			// (Without targets.yml's subscription, the snapshot's word is
			// enough: the target is not opted in.)
			ref = tree.Commit
			continue
		case !inTree && t.assumed:
			res.Outcome, res.Reason = report.OutcomeFailed, "race"
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s read through the API is not in the snapshot", r.optIn))
			return nil, sel, nil, false
		case !inTree:
			res.Outcome, res.Reason = report.OutcomeSkipped, reasonNotOptedIn
			return nil, sel, nil, false
		case !regular:
			res.Outcome, res.Reason = report.OutcomeSkipped, "unsafe-opt-in"
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s has mode %s in the default branch", r.optIn, e.Mode))
			return nil, sel, nil, false
		case assumed && unsafe:
			// The API found the file unsafe: the plan keeps the empty one.
		case assumed:
			res.Outcome, res.Reason = report.OutcomeFailed, "race"
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s is not in the default branch through the API, and is in the snapshot", r.optIn))
			return nil, sel, nil, false
		case file.OID != "" && !strings.EqualFold(e.OID, file.OID) && attempt == 0 && tree.Commit != "":
			ref = tree.Commit
			continue
		case file.OID != "" && !strings.EqualFold(e.OID, file.OID):
			res.Outcome, res.Reason = report.OutcomeFailed, "race"
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s read through the API is not the one in the snapshot", r.optIn))
			return nil, sel, nil, false
		}
		for _, w := range warns {
			res.Warnings = append(res.Warnings, w.String())
		}
		res.Assumed, res.AssumedBy = assumed, ""
		switch {
		case assumed && t.assumed && !unsafe && !inTree:
			res.AssumedBy = report.AssumedByHub
		case assumed:
			res.AssumedBy = report.AssumedByFlag
		}
		return o, s, tree, true
	}
}

// Skip reasons of the opt-in step.
const (
	// reasonNotOptedIn: the target has no opt-in file, and targets.yml does
	// not subscribe it.
	reasonNotOptedIn = "not-opted-in"
	// reasonOptedOut: the opt-in file says enabled: false.
	reasonOptedOut = "opted-out"
)

// snapshot takes the snapshot of the target's default branch.
func (r *run) snapshot(ctx context.Context, t *target) (*snapshot.Tree, bool) {
	var remote platform.Remote
	err := r.retry(ctx, t.prov, func() error {
		var err error
		remote, err = t.prov.reader.Remote(ctx, t.repo)
		return err
	})
	if err != nil {
		r.fail(ctx, t, "get the remote", err, "internal")
		return nil, false
	}
	t.remote = remote
	var tree *snapshot.Tree
	err = r.gitRetry(ctx, t.prov, func() error {
		var err error
		tree, err = r.d.Snapshots.Snapshot(ctx, t.repo, remote, "")
		return err
	})
	switch {
	case err == nil && tree != nil:
		return tree, true
	case err == nil:
		r.fail(ctx, t, "snapshot", errors.New("no tree"), "git")
	case isNotFound(err) && ctx.Err() == nil:
		t.res.Outcome, t.res.Reason = report.OutcomeSkipped, "empty"
	default:
		r.fail(ctx, t, "snapshot", err, "git")
	}
	return nil, false
}

// decideInput is the per-path input of a target (step 4): the selected packs
// over the snapshot tree. A path of unsafe (path → why) is observed as not a
// regular file, which makes it unsafe: the unsafe checks of settle take it
// out of D (commit.go).
func (r *run) decideInput(optIn *config.OptIn, sel config.Selection, tree *snapshot.Tree, unsafe map[string]string) decide.Input {
	in := decide.Input{
		Manifest:  r.d.Manifest,
		Selected:  sel.Packs,
		Aliases:   r.aliases,
		Desired:   decide.Layer(r.d.Current, sel.Packs),
		Ignore:    optIn.Ignore,
		OptInFile: r.optIn,
	}
	obs := tree.Observe(decide.Paths(in))
	for path, why := range unsafe {
		if _, ok := obs[path]; ok {
			obs[path] = decide.Observation{Kind: decide.NotRegular, Detail: why}
		}
	}
	in.Observed = obs
	return in
}

// reportPlan fills the target's report line from the per-path plan and D:
// the counts of changes, the orphaned paths and the key.
func (r *run) reportPlan(t *target, plan decide.Plan, pairs []decide.Pair) {
	res := &t.res
	res.Changes, res.Orphaned, res.Key = report.ChangeCounts{}, nil, ""
	for _, e := range plan.Entries {
		switch e.Action {
		case decide.Create:
			res.Changes.Create++
		case decide.Update, decide.Adopt:
			res.Changes.Update++
		case decide.Delete:
			res.Changes.Delete++
		case decide.Chmod:
			res.Changes.Chmod++
		}
		if e.State == decide.Orphaned {
			res.Orphaned = append(res.Orphaned, e.Path)
		}
	}
	slices.Sort(res.Orphaned)
	if len(pairs) > 0 {
		res.Key = decide.Key(decide.StreamSync, pairs)
	}
}

// identity tells t's own pull requests from others'.
func (r *run) identity(t *target) decide.Identity {
	return decide.Identity{Branches: r.branches, Authors: t.prov.ids, Fingerprints: r.fps}
}

// prSet is the pull requests on a target's sync branches, sorted as
// decide.DecideTarget reads them.
type prSet struct {
	// all are the pull requests Reader.PRs listed, newest first.
	all []platform.PR
	// own are touchmark's own pull requests in every state, newest first;
	// foreign the open pull requests of others from the target repository;
	// invalid the open pull requests of our authors whose marker is missing
	// or broken.
	own     []decide.OwnPR
	foreign []platform.PR
	invalid []platform.PR
}

// pullRequests runs step 5: the pull requests on the sync branches in every
// state, sorted by decide.Identity. ok is false when the target's outcome is
// decided: the listing failed, or a pull request may be touchmark's but its
// authors could not be looked up (failed or deferred by the class of that
// error, as in the snapshot-only plan; a marker-invalid pull request decides
// first).
func (r *run) pullRequests(ctx context.Context, t *target) (prSet, bool) {
	list, ok := r.listPRs(ctx, t)
	if !ok {
		return prSet{}, false
	}
	id := r.identity(t)
	s := prSet{all: list}
	var unplaced []platform.PR
	t.noOwnPRs = true
	for _, pr := range list {
		if pr.Author.ID != "" && slices.Contains(t.prov.ids, pr.Author.ID) {
			t.noOwnPRs = false
		}
		if !slices.Contains(r.branches, pr.Head) {
			continue
		}
		m, status := id.Own(pr)
		open := pr.State == platform.Open
		switch {
		case status == decide.Ours:
			s.own = append(s.own, decide.OwnPR{PR: pr, Marker: m, Alias: pr.Head != r.branches[0]})
		case !open:
			// A closed pull request without a valid marker of ours tells
			// nothing: its content is unknown.
		case status == decide.OursMarkerInvalid:
			s.invalid = append(s.invalid, pr)
		case sameRepo(t, pr):
			s.foreign = append(s.foreign, pr)
			if r.couldBeOurs(t, pr) {
				unplaced = append(unplaced, pr)
			}
		default:
			r.forkWarning(t, pr)
		}
	}
	if len(unplaced) > 0 && len(s.invalid) == 0 {
		r.failUnplaced(ctx, t, unplaced[0])
		return prSet{}, false
	}
	return s, true
}

// listPRs lists the pull requests on t's sync branches in every state
// (Reader.PRs). A listing that went through proves the credential works:
// the provider's streak of auth failures ends. ok is false when the
// target's outcome is decided: the listing failed.
func (r *run) listPRs(ctx context.Context, t *target) ([]platform.PR, bool) {
	var list []platform.PR
	err := r.retry(ctx, t.prov, func() error {
		var err error
		list, err = t.prov.reader.PRs(ctx, t.repo, r.branches, t.prov.authors)
		return err
	})
	if err != nil {
		r.fail(ctx, t, "list pull requests", err, "internal")
		return nil, false
	}
	t.prov.succeeded()
	return list, true
}

// keptPR returns the open pull request decide.DecideTarget maintains: the
// newest on the sync branch, else the newest. Marker-invalid pull requests
// count, as operations.yml may let touchmark adopt them.
func keptPR(s prSet, sync string) (decide.OwnPR, bool) {
	var best decide.OwnPR
	found := false
	consider := func(o decide.OwnPR) {
		switch {
		case o.PR.State != platform.Open:
		case !found:
			best, found = o, true
		case (o.PR.Head == sync) != (best.PR.Head == sync):
			if o.PR.Head == sync {
				best = o
			}
		case o.PR.Number > best.PR.Number:
			best = o
		}
	}
	for _, o := range s.own {
		consider(o)
	}
	for _, pr := range s.invalid {
		consider(decide.OwnPR{PR: pr, Alias: pr.Head != sync})
	}
	return best, found
}

// targetGit stands in for the targets' git repositories in phase C where a
// test has no git (Deps.git): a fleet of thousands of targets on the fake
// in memory mode (scale_test.go). It does what steps 6 and 8 do with the
// target's repository, and no Work it makes has one: such a work must not
// push.
type targetGit interface {
	// settle is step 8 (run.settle): w's Plan, D, Key and Built.
	settle(ctx context.Context, r *run, w *Work, optIn *config.OptIn, sel config.Selection) error
	// readBranches is step 6 (run.readBranches).
	readBranches(ctx context.Context, r *run, w *Work, names []string, adopt bool, moving string) (branchReads, error)
}

// inspectFull runs steps 5–9 of phase C (Run's doc) for a target whose
// opt-in file, packs and snapshot are known, and returns its work; nil when
// the target's outcome needs none.
func (r *run) inspectFull(ctx context.Context, t *target, optIn *config.OptIn, sel config.Selection, tree *snapshot.Tree) *Work {
	w := &Work{t: t, Tree: tree, B: tree.Commit, DefaultBranch: t.repo.DefaultBranch, OptInHash: optIn.Hash()}
	if r.d.git == nil {
		err := r.retry(ctx, t.prov, func() error {
			var err error
			w.Repo, err = r.repos.Repo(ctx, t.repo, t.remote)
			return gitFailure(err)
		})
		if err != nil {
			r.fail(ctx, t, "open the target's repository", err, "git")
			return nil
		}
	}
	prs, ok := r.pullRequests(ctx, t)
	if !ok {
		return nil
	}
	w.Own = prs.own
	w.listed = seenAll(prs.all, t.repo, r.fps)
	if err := r.settleOf(ctx, w, optIn, sel); err != nil {
		r.failCommit(ctx, t, err)
		return nil
	}
	r.reportPlan(t, w.Plan, w.D)
	ops := r.targetOps(t)
	kept, hasKept := keptPR(prs, r.branches[0])
	moving := r.branches[0]
	if hasKept {
		moving = kept.PR.Head
	}
	reads, err := r.readBranchesOf(ctx, w, r.historyBranches(w, prs), ops.AdoptUnmarked, moving)
	if err != nil {
		r.fail(ctx, t, "read the sync branches", gitFailure(err), "git")
		return nil
	}
	switch why, err := r.supersededOn(ctx, reads); {
	case err != nil:
		r.fail(ctx, t, "compare the hub commits of the sync branches", err, "internal")
		return nil
	case why != "":
		t.res.Outcome, t.res.Reason = report.OutcomeSkipped, "superseded"
		t.res.Warnings = append(t.res.Warnings, why)
		return nil
	}
	w.Memory = r.memory(t, w, ops)
	w.Branch = reads.branches[r.branches[0]]
	w.Aliases = reads.aliases(r.branches[0])
	w.Decision = r.decideTarget(w, prs, reads, ops, kept, hasKept)
	if needsRules(w.Decision) {
		rules, err := r.readRules(ctx, w)
		if err != nil {
			r.fail(ctx, t, "read the rules of the target's branches", err, "internal")
			return nil
		}
		w.rules = rules
		if (rules.Known && len(rules.NoForcePush) > 0) || rules.WorkflowsKnown {
			w.Decision = r.decideTarget(w, prs, reads, ops, kept, hasKept)
		}
	}
	if hasKept && (len(kept.Marker.Key) > 0 || touches(w.Decision, kept.PR.Number)) {
		w.Open = &kept
	}
	r.checkSigning(w)
	w.NeedPerms = needPerms(w.Decision.Steps)
	r.fillBody(w, sel, reads)
	if err := r.reportDecision(w, prs, reads); err != nil {
		r.fail(ctx, t, "render the pull request's description", err, "internal")
		return nil
	}
	return w
}

// settleOf runs step 8 in the target's repository (settle), or through
// Deps.git.
func (r *run) settleOf(ctx context.Context, w *Work, optIn *config.OptIn, sel config.Selection) error {
	if g := r.d.git; g != nil {
		return g.settle(ctx, r, w, optIn, sel)
	}
	return r.settle(ctx, w, optIn, sel)
}

// readBranchesOf runs step 6 in the target's repository (readBranches), or
// through Deps.git.
func (r *run) readBranchesOf(ctx context.Context, w *Work, names []string, adopt bool, moving string) (branchReads, error) {
	if g := r.d.git; g != nil {
		return g.readBranches(ctx, r, w, names, adopt, moving)
	}
	return r.readBranches(ctx, w, names, adopt, moving)
}

// decideTarget runs step 9: decide.DecideTarget over what steps 4–8 found
// for w; kept is the open pull request it maintains (hasKept).
func (r *run) decideTarget(w *Work, prs prSet, reads branchReads, ops decide.TargetOps, kept decide.OwnPR, hasKept bool) decide.TargetDecision {
	p := w.t.prov
	until, declined := w.Memory.Cooldown(w.Key, r.now, r.cooldown)
	can := r.d.Write.CanWorkflows[p.cfg.ID]
	if w.rules.WorkflowsKnown {
		can = w.rules.Workflows
	}
	var noForce, noDelete []string
	if w.rules.Known {
		noForce, noDelete = w.rules.NoForcePush, w.rules.NoDelete
	}
	return decide.DecideTarget(decide.TargetInput{
		Stream:               decide.StreamSync,
		D:                    w.D,
		Key:                  w.Key,
		B:                    w.B,
		DefaultBranch:        w.DefaultBranch,
		Branch:               w.Branch,
		Aliases:              w.Aliases,
		Own:                  prs.own,
		ForeignOpen:          prs.foreign,
		MarkerInvalid:        prs.invalid,
		Memory:               w.Memory,
		CooldownUntil:        until,
		CooldownDeclined:     declined,
		Ops:                  ops,
		RecreateTicked:       hasKept && prbody.Ticked(kept.PR.Body, prbody.ControlRecreate),
		PlatformWorkflowPerm: p.caps.WorkflowPerm,
		CanWorkflows:         can,
		WorkflowsDiffer:      reads.differ,
		NoForcePush:          noForce,
		NoDelete:             noDelete,
		LegacyRewritable:     w.Branch.LegacyRewritable,
		Now:                  r.now,
	})
}

// failCommit records a failure of step 8 as the target's outcome:
// failed:integrity when the commit changed other paths than D (I1), the
// class of a git network failure, else failed:git (a missing object, a
// commit or blob over the limits, an unexpected answer of git).
func (r *run) failCommit(ctx context.Context, t *target, err error) {
	fallback := "git"
	switch {
	case errors.Is(err, gitx.ErrIntegrity):
		fallback = "integrity"
	case errors.Is(err, errNoFixpoint):
		fallback = "internal"
	}
	r.fail(ctx, t, "build the commit", gitFailure(err), fallback)
}

// historyBranches are the sync branches whose history step 6 reads: the
// sync branch, and each alias that carries an own or marker-invalid open
// pull request. None when nothing can move a branch: D is empty and no
// such pull request is open (DecideTarget then decides without them).
func (r *run) historyBranches(w *Work, s prSet) []string {
	open := map[string]bool{}
	for _, o := range s.own {
		if o.PR.State == platform.Open {
			open[o.PR.Head] = true
		}
	}
	for _, pr := range s.invalid {
		open[pr.Head] = true
	}
	if len(w.D) == 0 && len(open) == 0 {
		return nil
	}
	names := []string{r.branches[0]}
	for _, alias := range r.branches[1:] {
		if open[alias] {
			names = append(names, alias)
		}
	}
	return names
}

// touches reports whether a step of d writes to pull request n.
func touches(d decide.TargetDecision, n int64) bool {
	return slices.ContainsFunc(d.Steps, func(s decide.Step) bool { return s.PR == n })
}

// needsRules reports whether the platform rules can change decision d (the
// preflight): it pushes (a rule may forbid the force, need a signature or
// deny the Workflows permission), or it is blocked on the Workflows
// permission that the run's default denied and the identity may have after
// all. Any other decision stands whatever the rules say: a rule only ever
// takes a push away.
func needsRules(d decide.TargetDecision) bool {
	return pushes(d) || (d.Outcome == decide.OutcomeBlocked && d.Reason == decide.ReasonPermissionWflw)
}

// pushes reports whether d moves or creates a branch.
func pushes(d decide.TargetDecision) bool {
	return slices.ContainsFunc(d.Steps, func(s decide.Step) bool {
		return s.Kind == decide.StepPush || s.Kind == decide.StepRecreateBranch
	})
}

// memoryUpkeep reports whether s is memory upkeep: a revocation, an ack or
// the comment after an ack.
func memoryUpkeep(s decide.Step) bool {
	switch s.Kind {
	case decide.StepRevoke, decide.StepAck:
		return true
	case decide.StepComment:
		return s.Reason == decide.OutcomeDeclined || s.Reason == decide.CommentAutoDeclined
	}
	return false
}

// readRules reads the platform rules of the branches w's decision pushes to
// and of the default branch, for the preflight, through the provider's
// identity when it is a platform.Preflighter (the reader in a plan, the
// writer otherwise; GitHub's rules/branches and, for a writer, the
// installation's Workflows permission). Without one it returns zero Rules:
// nothing is known upfront, and pushes meet the rules at run time. A
// transient failure is tried three times.
func (r *run) readRules(ctx context.Context, w *Work) (platform.Rules, error) {
	pf, ok := w.t.prov.reader.(platform.Preflighter)
	if !ok {
		return platform.Rules{}, nil
	}
	branches := uniqueNonEmpty([]string{w.DefaultBranch, w.Decision.Branch})
	for _, s := range w.Decision.Steps {
		if s.Kind == decide.StepPush || s.Kind == decide.StepRecreateBranch {
			branches = uniqueNonEmpty(append(branches, s.Branch))
		}
	}
	var rules platform.Rules
	err := r.retry(ctx, w.t.prov, func() error {
		var err error
		rules, err = pf.Preflight(ctx, w.t.repo, branches)
		return err
	})
	if err != nil {
		return platform.Rules{}, err
	}
	if !rules.Known {
		rules.SignedCommits, rules.NoForcePush, rules.NoDelete = false, nil, nil
	}
	return rules, nil
}

// checkSigning decides how a push of w is signed. A signature is needed with
// providers[].sign always, and under sign: auto when a rule of the default
// or the sync branch requires signed commits (Rules.SignedCommits from the
// preflight). A needed signature is the provider's key when it has one (the
// commit is built signed, and git pushes it); else the platform's API commit
// through the stage ref where Caps.Commit.API offers one and no API commit
// of the provider came back unsigned in this run (phase F then commits
// through the per-target writer's platform.Committer); else the decision is
// blocked:cannot-sign before any push, with a warning that says why. Memory
// upkeep, which pushes nothing, stays.
//
// A plan never has the key (write secrets stay out of its job): it plans as
// if distribute could sign, through the API where the platform has one,
// whose writes the estimates count; a dry run tells.
func (r *run) checkSigning(w *Work) {
	p := w.t.prov
	w.needSig, w.viaAPI = false, false
	if !pushes(w.Decision) {
		return
	}
	var why string
	switch {
	case p.cfg.Sign == "always":
		why = fmt.Sprintf("provider %s signs every commit (sign: always)", p.cfg.ID)
	case w.rules.Known && w.rules.SignedCommits:
		why = "a rule of the target's branches requires signed commits"
	default:
		return
	}
	w.needSig = true
	switch {
	case r.mode == ModePlan:
		w.viaAPI = p.caps.Commit.API
		return
	case p.signer != nil:
		return
	case p.caps.Commit.API && p.unsignedAPI() == "":
		w.viaAPI = true
		return
	}
	how := "the platform makes no signed commits through its API"
	if u := p.unsignedAPI(); u != "" {
		how = u
	}
	d := w.Decision
	var upkeep []decide.Step
	for _, s := range d.Steps {
		if memoryUpkeep(s) {
			upkeep = append(upkeep, s)
		}
	}
	w.Decision = decide.TargetDecision{Outcome: decide.OutcomeBlocked, Reason: "cannot-sign", PR: d.PR, Branch: d.Branch, Steps: upkeep}
	w.needSig, w.viaAPI = false, false
	w.t.res.Warnings = append(w.t.res.Warnings, fmt.Sprintf("%s, and touchmark cannot sign for provider %s: it has no signing key (%sSIGNING_KEY), and %s",
		why, p.cfg.ID, p.cfg.EnvPrefix, how))
}

// needPerms is the permissions of the per-target write token for steps:
// Contents for pushes and deletions, PRs for writes to pull requests,
// Workflows for a push that needs it.
func needPerms(steps []decide.Step) platform.Perms {
	var p platform.Perms
	for _, s := range steps {
		switch s.Kind {
		case decide.StepPush, decide.StepRecreateBranch, decide.StepDeleteBranch:
			p.Contents = true
		default:
			p.PRs = true
		}
		if s.NeedWorkflows {
			p.Workflows = true
		}
	}
	return p
}

// gitFailure gives an error of a target's git a platform class when it has
// one (gitx.ClassifyFailure): a refused credential is ClassAuth, a refused
// identity ClassPermission, HTTP 429 ClassRateLimited, a network failure
// ClassTransient. Anything else is returned as it is (failed:git).
func gitFailure(err error) error {
	class := platform.ClassUnknown
	switch gitx.ClassifyFailure(err) {
	case gitx.FailureAuth:
		class = platform.ClassAuth
	case gitx.FailurePermission:
		class = platform.ClassPermission
	case gitx.FailureRateLimited:
		class = platform.ClassRateLimited
	case gitx.FailureTransient:
		class = platform.ClassTransient
	}
	if class == platform.ClassUnknown || platform.ClassOf(err) != platform.ClassUnknown {
		return err
	}
	return &platform.Error{Op: "git", Class: class, Err: err}
}
