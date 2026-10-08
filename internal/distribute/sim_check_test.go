package distribute

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The checks of the adversarial tests: the invariants of the package doc
// that must hold after every run, computed from the platform's state and a
// model of the hub, independently of decide where the rules are simple
// enough (D, the declines a person made), and from the call log of the fake.

// simChecker remembers, between runs of one world, what the invariants
// need: the declines people made, the commits people pushed to sync
// branches, and the pull requests the test opened itself.
type simChecker struct {
	w *simWorld
	// failf reports a broken invariant; the property test adds the
	// scenario's event log to it.
	failf func(format string, args ...any)
	// states are the pull requests' states at the last observation.
	states map[string]map[int64]platform.PRState
	// declines are the declines people made, by target and pull request.
	declines map[string]map[int64]*simDecline
	// tracked are the commits people pushed to sync branches, by target:
	// no run of touchmark may make them unreachable (I2). released are
	// those a recreate lets touchmark drop.
	tracked  map[string]map[string]bool
	released map[string]map[string]bool
	// foreign are the pull requests the test opened as someone else's (a
	// person's, a fork's, another hub's): touchmark never writes to them
	// (I3).
	foreign map[string]map[int64]bool
	// optIns are the hashes of the opt-in files at the last observation.
	optIns map[string]string
	// revoked are the pull requests a ticked repropose or a
	// forget_declines entry revoked, by target: never declines again.
	revoked map[string]map[int64]bool
}

// simDecline is a pull request of touchmark's a person closed without
// merging, while the test counts it in force: from the close until the
// team changes its opt-in file, reopens it, or revokes it.
type simDecline struct {
	changes []marker.Change
	optIn   string
	void    bool
}

func newSimChecker(w *simWorld, failf func(format string, args ...any)) *simChecker {
	return &simChecker{
		w: w, failf: failf,
		states:   map[string]map[int64]platform.PRState{},
		declines: map[string]map[int64]*simDecline{},
		tracked:  map[string]map[string]bool{},
		released: map[string]map[string]bool{},
		foreign:  map[string]map[int64]bool{},
		optIns:   map[string]string{},
		revoked:  map[string]map[int64]bool{},
	}
}

// addForeign records a pull request the test opened as someone else's.
func (c *simChecker) addForeign(tg *simTarget, n int64) {
	if c.foreign[tg.name] == nil {
		c.foreign[tg.name] = map[int64]bool{}
	}
	c.foreign[tg.name][n] = true
}

// revoke voids the decline of pull request n for good (a ticked
// repropose, a forget_declines entry), whether it was observed yet or not.
func (c *simChecker) revoke(tg *simTarget, n int64) {
	if c.revoked[tg.name] == nil {
		c.revoked[tg.name] = map[int64]bool{}
	}
	c.revoked[tg.name][n] = true
	if d := c.declines[tg.name][n]; d != nil {
		d.void = true
	}
}

// optInHash is the hash of tg's opt-in file as memory compares it
// (config.OptIn.Hash); "" when absent or invalid.
func (c *simChecker) optInHash(tg *simTarget) string {
	content := c.w.optIn(tg)
	if content == "" {
		return ""
	}
	o, _, err := config.ParseOptIn([]byte(content))
	if err != nil {
		return ""
	}
	return o.Hash()
}

// observe records what people did since the last observation: pull
// requests they closed (declines), reopened or merged, opt-in files they
// changed, and the commits of theirs on sync branches. It runs after
// people act and before touchmark runs.
func (c *simChecker) observe() {
	w := c.w
	for _, tg := range w.targets {
		hash := c.optInHash(tg)
		if old, seen := c.optIns[tg.name]; seen && old != hash {
			for _, d := range c.declines[tg.name] {
				d.void = true
			}
		}
		c.optIns[tg.name] = hash
		states := c.states[tg.name]
		if states == nil {
			states = map[int64]platform.PRState{}
			c.states[tg.name] = states
		}
		for _, pr := range w.prs(tg) {
			old, known := states[pr.Number]
			states[pr.Number] = pr.State
			if known && old == pr.State {
				continue
			}
			switch pr.State {
			case platform.Open, platform.Merged:
				delete(c.declines[tg.name], pr.Number)
			case platform.Closed:
				c.closed(tg, pr, hash)
			}
		}
		c.track(tg)
	}
}

// closed records a pull request that was just closed without merging when it
// is a decline: touchmark's own, not closed by touchmark (marker closed.by,
// or the writer), by a person, by no one the platform names, or by a bot
// where the platform does not tell closers. Declines the test cannot judge
// exactly are left out: those without the full list of changes, and those
// whose marker kept an ack from before a reopening (memory counts them from
// that ack). Where closed pull requests are immutable, the optin the marker
// held while open stands for the ack: a decline closed under another
// opt-in state is lapsed already, and left out.
func (c *simChecker) closed(tg *simTarget, pr platform.PR, hash string) {
	w := c.w
	if !w.isOwn(pr) || c.revoked[tg.name][pr.Number] {
		return
	}
	m, _ := marker.Find(pr.Body, []string{hubFP})
	d := m.Data
	switch {
	case d.Closed != nil, d.Revoked, d.Ack, !d.ChangesComplete, len(d.Changes) == 0:
		return
	case pr.ClosedBy != nil && pr.ClosedBy.ID == w.writer.ID:
		return
	case pr.ClosedBy != nil && pr.ClosedBy.Kind == platform.KindBot && w.p.Caps().CloserKnown:
		return
	case w.p.Caps().ClosedImmutable && d.OptIn != "" && d.OptIn != hash:
		return
	}
	if c.declines[tg.name] == nil {
		c.declines[tg.name] = map[int64]*simDecline{}
	}
	c.declines[tg.name][pr.Number] = &simDecline{changes: slices.Clone(d.Changes), optIn: hash}
}

// track keeps the commits people pushed to tg's sync branches: those still
// reachable, and those on the sync branches now (merges left out: an
// "Update branch" merge brings the default branch, which stays).
func (c *simChecker) track(tg *simTarget) {
	w := c.w
	reach, err := w.reachable(tg)
	if err != nil {
		c.failf("%v", err)
		return
	}
	kept := map[string]bool{}
	for sha := range c.tracked[tg.name] {
		if reach[sha] {
			kept[sha] = true
		}
	}
	for _, b := range w.branches() {
		if w.p.Branch(tg.repo.ID, b) == "" {
			continue
		}
		// The author is matched as a fixed string: the email holds a "+",
		// which git's basic regular expressions read as "one or more".
		out, err := w.git(tg, "rev-list", "--no-merges", "--fixed-strings", "--author="+w.person.Email, "refs/heads/"+b, "--not", "refs/heads/"+tg.repo.DefaultBranch)
		if err != nil {
			c.failf("list the commits of %s on %s: %v", tg.repo.Path, b, err)
			continue
		}
		for _, sha := range strings.Fields(out) {
			kept[sha] = true
		}
	}
	c.tracked[tg.name] = kept
}

// release lets touchmark drop the commits a recreate names: every commit
// reachable from the head of a sync branch whose open pull request has
// its recreate control ticked, or whose head an operations.yml recreate
// entry names (the control says it "drops commits added by others").
func (c *simChecker) release() {
	w := c.w
	var heads []string
	if w.ops != nil {
		for _, r := range w.ops.Recreate {
			heads = append(heads, r.Head)
		}
	}
	for _, tg := range w.targets {
		mine := slices.Clone(heads)
		for _, pr := range w.openOwn(tg) {
			if prbody.Ticked(pr.Body, prbody.ControlRecreate) {
				mine = append(mine, w.p.Branch(tg.repo.ID, pr.Head))
			}
		}
		for _, head := range mine {
			if head == "" {
				continue
			}
			out, err := w.git(tg, "rev-list", head)
			if err != nil {
				continue // not a commit of this repository
			}
			if c.released[tg.name] == nil {
				c.released[tg.name] = map[string]bool{}
			}
			for _, sha := range strings.Fields(out) {
				c.released[tg.name][sha] = true
			}
		}
	}
}

// inForce returns the declines of tg the test counts in force now: not void,
// and no path of theirs local or ignored. The test's set is never larger
// than memory's: it drops a decline on any change of the opt-in file since
// the close, where memory compares with the file at the time of the ack.
func (c *simChecker) inForce(tg *simTarget) map[int64]*simDecline {
	w := c.w
	model := w.model(tg)
	out := map[int64]*simDecline{}
	for n, d := range c.declines[tg.name] {
		if d.void || model == nil {
			continue
		}
		if slices.ContainsFunc(d.changes, func(ch marker.Change) bool { return model.lapses(ch.Path) }) {
			continue
		}
		out[n] = d
	}
	return out
}

// simModel is what the hub ships to one target now, over the target's
// default branch: the model the checks compute D with.
type simModel struct {
	tree     map[string]string // path → blob in B
	desired  map[string]string // path → blob to ship
	selected map[string]bool   // packs
	ignore   []string
	shipped  map[string]map[string]bool // path → every blob the hub shipped there
}

// model returns the model of tg; nil when the target is not opted in (no
// opt-in file, or one that does not parse).
func (w *simWorld) model(tg *simTarget) *simModel {
	content := w.optIn(tg)
	if content == "" {
		return nil
	}
	o, _, err := config.ParseOptIn([]byte(content))
	if err != nil {
		return nil
	}
	m := &simModel{tree: map[string]string{}, desired: map[string]string{}, selected: map[string]bool{simBase: true},
		ignore: o.Ignore, shipped: map[string]map[string]bool{}}
	for _, pack := range o.Packs {
		m.selected[pack] = true
	}
	for path, e := range w.tree(tg) {
		m.tree[path] = e.OID
	}
	for pack := range m.selected {
		for path, content := range w.files[pack] {
			m.desired[path] = oid(content)
		}
	}
	for path, byPack := range w.manifest.Paths {
		m.shipped[path] = map[string]bool{}
		for _, vs := range byPack {
			for _, v := range vs {
				m.shipped[path][v.OID] = true
			}
		}
	}
	return m
}

// ignored reports whether the opt-in file ignores path (the simulations
// write exact paths there).
func (m *simModel) ignored(path string) bool {
	return slices.ContainsFunc(m.ignore, func(p string) bool { return strings.EqualFold(p, path) })
}

// lapses reports whether a decline of path no longer holds: the path is
// ignored, or the target holds content there the hub never shipped (the
// team made it its own).
func (m *simModel) lapses(path string) bool {
	if m.ignored(path) {
		return true
	}
	b, ok := m.tree[path]
	return ok && !m.shipped[path][b] && b != m.desired[path]
}

// D computes D for the simple trees of the simulations: every desired path
// not ignored, created when absent, updated when it holds a version the hub
// shipped, left alone when it holds the version shipped now or the team's
// own content; every path a selected pack no longer ships deleted when it
// holds a version the hub shipped.
func (m *simModel) D() []decide.Pair {
	var out []decide.Pair
	for _, path := range slices.Sorted(maps.Keys(m.desired)) {
		want := m.desired[path]
		b, ok := m.tree[path]
		switch {
		case m.ignored(path):
		case !ok:
			out = append(out, decide.Pair{Path: path, From: decide.ZeroOID, Mode: "100644", To: want})
		case b == want:
		case m.shipped[path][b]:
			out = append(out, decide.Pair{Path: path, From: b, Mode: "100644", To: want})
		}
	}
	for _, path := range slices.Sorted(maps.Keys(m.shipped)) {
		b, ok := m.tree[path]
		_, desired := m.desired[path]
		if desired || !ok || !m.selected[simPacks[path]] || m.ignored(path) || !m.shipped[path][b] {
			continue
		}
		out = append(out, decide.Pair{Path: path, From: b, Mode: decide.ModeDelete, To: decide.ZeroOID})
	}
	return out
}

// simBefore is what the checks of a distribute run read before it, by
// target.
type simBefore struct {
	// all are the numbers of every pull request, own those of touchmark's.
	all, own map[string]map[int64]bool
	inForce  map[string]map[int64]*simDecline
	models   map[string]*simModel
	b        map[string]string // default branch heads
	// busy are the sync branches that carry someone else's open pull
	// request from the repository itself.
	busy map[string]map[string]bool
	// fields are the title and labels of touchmark's pull requests.
	fields map[string]map[int64]string
}

// snapshot reads what the checks of the next distribute run need.
func (c *simChecker) snapshot() simBefore {
	w := c.w
	s := simBefore{all: map[string]map[int64]bool{}, own: map[string]map[int64]bool{}, inForce: map[string]map[int64]*simDecline{},
		models: map[string]*simModel{}, b: map[string]string{}, busy: map[string]map[string]bool{}, fields: map[string]map[int64]string{}}
	for _, tg := range w.targets {
		s.all[tg.name], s.own[tg.name] = map[int64]bool{}, map[int64]bool{}
		s.busy[tg.name], s.fields[tg.name] = map[string]bool{}, map[int64]string{}
		for _, pr := range w.prs(tg) {
			s.all[tg.name][pr.Number] = true
			switch {
			case w.isOwn(pr):
				s.own[tg.name][pr.Number] = true
				s.fields[tg.name][pr.Number] = simFields(pr)
			case pr.State == platform.Open && pr.HeadRepoID == pr.RepoID:
				s.busy[tg.name][pr.Head] = true
			}
		}
		s.inForce[tg.name] = c.inForce(tg)
		s.models[tg.name] = w.model(tg)
		s.b[tg.name] = w.p.Head(tg.repo.ID)
	}
	return s
}

// byRef returns the target a report names "gh:<path>".
func (w *simWorld) byRef(ref string) *simTarget {
	for _, tg := range w.targets {
		if "gh:"+tg.repo.Path == ref {
			return tg
		}
	}
	return nil
}

// byPath returns the target at repository path.
func (w *simWorld) byPath(path string) *simTarget {
	for _, tg := range w.targets {
		if tg.repo.Path == path {
			return tg
		}
	}
	return nil
}

// checkRun checks a distribute run: rep is its report, dry the report of
// the dry run just before it, writes the fake's call log of its writes and
// before what the platform held before it.
func (c *simChecker) checkRun(rep, dry *report.Delivery, writes []string, before simBefore) {
	w := c.w
	if v := w.p.Violations(); len(v) > 0 {
		c.failf("I5: forbidden transitions: %q", v)
	}
	c.checkOutcomes(rep, dry)
	c.checkWrites(writes, before)
	lines := map[string]report.DeliveryTarget{}
	for _, res := range rep.Targets {
		lines[res.Provider+":"+res.Path] = res
	}
	created := map[string][]int64{}
	for _, op := range rep.Ops {
		tg := w.byRef(op.Target)
		if tg == nil {
			c.failf("an op on %s, which is no target of the test: %+v", op.Target, op)
			continue
		}
		switch op.Kind {
		case "push", "update-refs":
			// A push, or the move of a branch to the platform's commit of
			// the same content (an API commit).
			c.checkPush(tg, op, before)
		case "create-pr":
			created[tg.name] = append(created[tg.name], op.PR)
		}
	}
	for _, tg := range w.targets {
		for _, n := range created[tg.name] {
			c.checkNewPR(tg, n, before)
		}
		c.checkKept(tg)
		c.checkFields(tg, before)
		if res, ok := lines["gh:"+tg.repo.Path]; ok {
			c.checkMarkers(tg, res)
		}
	}
}

// simFields is the title and the labels of a pull request.
func simFields(pr platform.PR) string {
	labels := slices.Clone(pr.Labels)
	slices.Sort(labels)
	return fmt.Sprintf("title %q, labels %q", pr.Title, labels)
}

// checkFields checks that a run changed neither the title nor the labels of
// a pull request of touchmark's that existed before it: after the creation
// they are people's, and the simulations never change pr.title or pr.labels
// in hub.yml, so touchmark has nothing to add.
func (c *simChecker) checkFields(tg *simTarget, before simBefore) {
	w := c.w
	for n, was := range before.fields[tg.name] {
		if now := simFields(w.p.PR(tg.repo.ID, n)); now != was {
			c.failf("the run changed #%d of %s: %s, then %s", n, tg.repo.Path, was, now)
		}
	}
}

// checkFaulted checks a run a fault stopped or disturbed: what holds
// whatever happened. No forbidden transition (but for a push to the branch
// of a merged or closed pull request whose new pull request the stopped run
// never opened: the next run opens it), writes only to touchmark's own
// (I3), pushed commits of D on B (I1), new pull requests on the sync branch
// and not covered by the declines in force (I4), no commit of a person
// lost (I2). Markers may lag behind a branch until the next run.
func (c *simChecker) checkFaulted(rep *report.Delivery, writes []string, before simBefore) {
	w := c.w
	for _, v := range w.p.Violations() {
		if !strings.HasPrefix(v, "closed-branch-push ") {
			c.failf("I5: forbidden transition in a faulted run: %q", v)
		}
	}
	c.checkWrites(writes, before)
	for _, op := range rep.Ops {
		tg := w.byRef(op.Target)
		switch {
		case tg == nil:
			c.failf("an op on %s, which is no target of the test: %+v", op.Target, op)
		case op.Kind == "push" || op.Kind == "update-refs":
			c.checkPush(tg, op, before)
		case op.Kind == "create-pr":
			c.checkNewPR(tg, op.PR, before)
		}
	}
	for _, tg := range w.targets {
		c.checkKept(tg)
		c.checkFields(tg, before)
	}
}

// checkOutcomes checks the outcomes of a run: nothing failed (the
// simulations inject no fault), and every target ended as the dry run
// said it would.
func (c *simChecker) checkOutcomes(rep, dry *report.Delivery) {
	want := map[string]report.DeliveryTarget{}
	for _, tg := range dry.Targets {
		want[tg.Provider+":"+tg.Path] = tg
	}
	for _, tg := range rep.Targets {
		ref := tg.Provider + ":" + tg.Path
		if tg.Outcome == report.OutcomeFailed {
			c.failf("%s failed:%s: %q", ref, tg.Reason, tg.Warnings)
		}
		d, ok := want[ref]
		switch {
		case !ok:
			c.failf("%s is in the report of distribute but not in the dry run's", ref)
		case c.w.apiUnsigned && tg.Outcome == report.OutcomeBlocked && tg.Reason == "cannot-sign":
			// A dry run makes no API commit, so it cannot know that the
			// platform leaves them unsigned.
		case d.Outcome != tg.Outcome || d.Reason != tg.Reason:
			c.failf("%s: the dry run said %s:%s, distribute did %s:%s (warnings %q)", ref, d.Outcome, d.Reason, tg.Outcome, tg.Reason, tg.Warnings)
		case d.PR != nil && (tg.PR == nil || tg.PR.Number != d.PR.Number):
			c.failf("%s: the dry run named #%d, distribute %+v", ref, d.PR.Number, tg.PR)
		}
	}
	if len(rep.Targets) != len(dry.Targets) {
		c.failf("distribute reported %d targets, the dry run %d", len(rep.Targets), len(dry.Targets))
	}
}

// checkWrites checks I3 on the fake's call log of one run: touchmark
// writes only to its own pull requests (those it had before the run, or
// opened in it) and pushes only to the hub's sync branches, never to one
// that carried someone else's open pull request.
func (c *simChecker) checkWrites(writes []string, before simBefore) {
	w := c.w
	for _, call := range writes {
		f := strings.Fields(call)
		if len(f) < 2 {
			continue
		}
		tg := w.byPath(f[1])
		if tg == nil {
			c.failf("I3: %q writes to a repository that is no target", call)
			continue
		}
		switch f[0] {
		case "EditPR", "Comment":
			n, err := strconv.ParseInt(strings.TrimPrefix(f[len(f)-1], "#"), 10, 64)
			if err != nil {
				c.failf("unreadable call %q", call)
				continue
			}
			// A pull request is touchmark's when it was before the run, or
			// when the run opened it.
			pr := w.p.PR(tg.repo.ID, n)
			mine := before.own[tg.name][n] || (!before.all[tg.name][n] && w.isOwn(pr))
			if c.foreign[tg.name][n] || !mine {
				c.failf("I3: %q writes to #%d of %s, which is not touchmark's", call, n, tg.repo.Path)
			}
		case "Push", "UpdateRefs":
			for _, b := range f[2:] {
				switch {
				case !slices.Contains(w.branches(), b):
					c.failf("I3: %q moves %s, which is no sync branch", call, b)
				case before.busy[tg.name][b]:
					c.failf("I3: %q moves %s, which carried someone else's open pull request", call, b)
				}
			}
		}
	}
}

// checkPush checks I1 on one push of a run: the commit sits on B alone and
// changes exactly D, the model's pairs, and its trailer names their key.
func (c *simChecker) checkPush(tg *simTarget, op report.Op, before simBefore) {
	w := c.w
	commit := op.After
	parents, msg, err := w.commitOf(tg, commit)
	if err != nil {
		c.failf("I1: read pushed commit %s of %s: %v", commit, tg.repo.Path, err)
		return
	}
	b := before.b[tg.name]
	if len(parents) != 1 || parents[0] != b {
		c.failf("I1: pushed commit %s of %s has parents %v, not B %s", short(commit), tg.repo.Path, parents, short(b))
		return
	}
	got, err := w.diff(tg, parents[0], commit)
	if err != nil {
		c.failf("I1: diff of %s: %v", short(commit), err)
		return
	}
	m := before.models[tg.name]
	if m == nil {
		c.failf("I1: %s pushed %s while it is not opted in", tg.repo.Path, short(commit))
		return
	}
	want := m.D()
	if !decide.PairsEqual(got, want) {
		c.failf("I1: pushed commit %s of %s changes %v, but D is %v", short(commit), tg.repo.Path, got, want)
	}
	tr, ok := decide.ParseTrailers(msg)
	if !ok || tr.Fingerprint != hubFP || len(want) == 0 || tr.Content != decide.Key(decide.StreamSync, want) {
		c.failf("I1: pushed commit %s of %s has trailers %+v (ok %v), want the key of D", short(commit), tg.repo.Path, tr, ok)
	}
}

// checkNewPR checks a pull request a run opened: on the hub's sync branch,
// with the head a commit of D on B, and not covered by the declines in
// force (I4).
func (c *simChecker) checkNewPR(tg *simTarget, n int64, before simBefore) {
	w := c.w
	pr := w.p.PR(tg.repo.ID, n)
	if pr.Head != w.branches()[0] || !w.isOwn(pr) {
		c.failf("opened #%d of %s on %s (own %v): new pull requests open on the sync branch", n, tg.repo.Path, pr.Head, w.isOwn(pr))
	}
	parents, _, err := w.commitOf(tg, pr.HeadSHA)
	if err != nil || len(parents) != 1 {
		c.failf("the head %s of new #%d of %s: parents %v, %v", short(pr.HeadSHA), n, tg.repo.Path, parents, err)
		return
	}
	pairs, err := w.diff(tg, parents[0], pr.HeadSHA)
	if err != nil {
		c.failf("diff of the head of #%d: %v", n, err)
		return
	}
	if m := before.models[tg.name]; m != nil && !decide.PairsEqual(pairs, m.D()) {
		c.failf("new #%d of %s carries %v, but D is %v", n, tg.repo.Path, pairs, m.D())
	}
	union := map[marker.Change]bool{}
	var by []int64
	for k, d := range before.inForce[tg.name] {
		for _, ch := range d.changes {
			union[ch] = true
		}
		by = append(by, k)
	}
	covered := len(pairs) > 0
	for _, ch := range decide.ShortChanges(pairs) {
		covered = covered && union[ch]
	}
	if covered {
		slices.Sort(by)
		c.failf("I4: opened #%d of %s with %v, which the declines in force %v cover", n, tg.repo.Path, pairs, by)
	}
}

// checkKept checks I2: every commit a person pushed to a sync branch of tg
// is still reachable from a branch, unless a recreate let touchmark drop
// it.
func (c *simChecker) checkKept(tg *simTarget) {
	w := c.w
	reach, err := w.reachable(tg)
	if err != nil {
		c.failf("%v", err)
		return
	}
	for _, sha := range slices.Sorted(maps.Keys(c.tracked[tg.name])) {
		if !reach[sha] && !c.released[tg.name][sha] {
			c.failf("I2: %s lost the commit %s a person pushed to a sync branch", tg.repo.Path, short(sha))
		}
	}
}

// checkMarkers checks that the marker of the open pull request a run
// maintained names what its branch carries: the key of the newest touchmark
// commit on it (memory remembers what the pull request really carried). res
// is the target's report line: a run that skipped the target, or was blocked
// or held off before writing to it, maintained nothing (a person may have
// reopened an old pull request whose branch a newer one took over; the run
// that maintains it writes its key).
func (c *simChecker) checkMarkers(tg *simTarget, res report.DeliveryTarget) {
	w := c.w
	switch {
	case res.PR == nil:
		return
	case res.Outcome == report.OutcomeOpened, res.Outcome == report.OutcomeUpdated, res.Outcome == report.OutcomeUnchanged:
	case res.Outcome == report.OutcomeBlocked && res.Reason == decide.ReasonEdited:
	default:
		return
	}
	for _, pr := range w.openOwn(tg) {
		head := w.p.Branch(tg.repo.ID, pr.Head)
		if pr.Number != res.PR.Number || head == "" {
			continue
		}
		key, ok := w.hcKey(tg, head)
		if !ok {
			continue
		}
		m, _ := marker.Find(pr.Body, []string{hubFP})
		if m.Key != key {
			c.failf("the marker of open #%d of %s names %s, its branch's commit carries %s", pr.Number, tg.repo.Path, m.Key, key)
		}
	}
}

// commitOf returns the parents and message of commit in tg's repository
// on the fake's side.
func (w *simWorld) commitOf(tg *simTarget, commit string) ([]string, string, error) {
	out, err := w.git(tg, "cat-file", "commit", commit)
	if err != nil {
		return nil, "", err
	}
	head, msg, _ := strings.Cut(out, "\n\n")
	var parents []string
	for _, line := range strings.Split(head, "\n") {
		if p, ok := strings.CutPrefix(line, "parent "); ok {
			parents = append(parents, p)
		}
	}
	return parents, msg, nil
}

// diff returns the pairs of `git diff-tree` from a to b in tg's repository.
func (w *simWorld) diff(tg *simTarget, a, b string) ([]decide.Pair, error) {
	out, err := w.git(tg, "diff-tree", "-r", "-z", "--no-renames", a, b)
	if err != nil {
		return nil, err
	}
	var pairs []decide.Pair
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			return nil, fmt.Errorf("diff-tree line %q", fields[i])
		}
		p := decide.Pair{Path: fields[i+1], From: meta[2], Mode: meta[1], To: meta[3]}
		if meta[0] == decide.ModeDelete {
			p.From = decide.ZeroOID
		}
		if meta[1] == decide.ModeDelete {
			p.Mode, p.To = decide.ModeDelete, decide.ZeroOID
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}

// hcKey returns the content key in the trailers of the newest touchmark
// commit of this hub on the first-parent chain from head (at most
// decide.MaxHistory commits).
func (w *simWorld) hcKey(tg *simTarget, head string) (string, bool) {
	out, err := w.git(tg, "log", "--first-parent", "-n", strconv.Itoa(decide.MaxHistory), "--format=%B%x00", head)
	if err != nil {
		return "", false
	}
	for _, msg := range strings.Split(out, "\x00") {
		msg = strings.TrimPrefix(msg, "\n")
		if tr, ok := decide.ParseTrailers(msg); ok && tr.Fingerprint == hubFP && tr.Stream == decide.StreamSync {
			return tr.Content, true
		}
	}
	return "", false
}

// samePlan checks that a plan and a dry run of the same state agree on every
// target (outcome, reason, pull request, key, estimated writes), on the
// sweep, the summary and the cost.
func (c *simChecker) samePlan(plan, dry *report.Delivery) {
	line := func(tg report.DeliveryTarget) string {
		pr := int64(0)
		if tg.PR != nil {
			pr = tg.PR.Number
		}
		return fmt.Sprintf("%s:%s %s:%s #%d key=%s writes=%d", tg.Provider, tg.Path, tg.Outcome, tg.Reason, pr, tg.Key, tg.Writes)
	}
	lines := func(rep *report.Delivery) []string {
		var out []string
		for _, tg := range rep.Targets {
			out = append(out, line(tg))
		}
		return out
	}
	if a, b := lines(plan), lines(dry); !slices.Equal(a, b) {
		c.failf("plan and dry run differ:\nplan %q\ndry  %q", a, b)
	}
	if plan.Sweep != dry.Sweep || !maps.Equal(plan.Summary, dry.Summary) || !maps.Equal(plan.Cost, dry.Cost) {
		c.failf("plan: sweep %+v summary %v cost %v; dry run: sweep %+v summary %v cost %v",
			plan.Sweep, plan.Summary, plan.Cost, dry.Sweep, dry.Summary, dry.Cost)
	}
}

// checkKeys checks that the report's key of every target touchmark
// inspected is the key of the model's D.
func (c *simChecker) checkKeys(rep *report.Delivery) {
	w := c.w
	for _, res := range rep.Targets {
		tg := w.byRef(res.Provider + ":" + res.Path)
		if tg == nil || res.Outcome == report.OutcomeSkipped || res.Outcome == report.OutcomeFailed ||
			(res.Outcome == report.OutcomeClosed && (res.Reason == decide.ReasonTargetDropped || res.Reason == decide.ReasonOptedOut)) ||
			(res.Outcome == report.OutcomeBlocked && (res.Reason == "archived" || res.Reason == "opt-in-invalid")) {
			continue
		}
		m := w.model(tg)
		if m == nil {
			continue
		}
		want := ""
		if d := m.D(); len(d) > 0 {
			want = decide.Key(decide.StreamSync, d)
		}
		if res.Key != want {
			c.failf("%s: the report's key %s is not the key %s of D %v (%s:%s)", res.Path, res.Key, want, m.D(), res.Outcome, res.Reason)
		}
	}
}

// outcomes renders a report's targets for messages.
func outcomes(rep *report.Delivery) string {
	var out []string
	for _, tg := range rep.Targets {
		s := fmt.Sprintf("%s %s", tg.Path, tg.Outcome)
		if tg.Reason != "" {
			s += ":" + tg.Reason
		}
		if tg.PR != nil {
			s += fmt.Sprintf(" #%d", tg.PR.Number)
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(a, b) })
	return strings.Join(out, ", ")
}
