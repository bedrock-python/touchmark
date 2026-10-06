package distribute

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// recheck reads the branches and pull requests the work depends on again,
// right before its first write (execute's step 2), and returns why the
// target moved since inspection ("" when it did not). ok is false when the
// target ended: a read failed, or a sweep close has nothing left to close.
func (x *targetExec) recheck(ctx context.Context) (moved string, ok bool) {
	if x.w.Repo != nil {
		want := x.expectedRefs()
		if len(want) > 0 {
			refs := keysInOrder(want)
			var got map[string]string
			err := x.retryRead(ctx, func() error {
				var err error
				got, err = x.w.Repo.RemoteRefs(ctx, refs...)
				return gitFailure(err)
			})
			if err != nil {
				x.fail("read the target's branches again", err)
				return "", false
			}
			for _, ref := range refs {
				if got[ref] != want[ref] {
					return fmt.Sprintf("%s is at %s, not %s", ref, labelHead(got[ref]), labelHead(want[ref])), true
				}
			}
		}
	}
	var fresh []platform.PR
	err := x.retryRead(ctx, func() error {
		var err error
		fresh, err = x.t.prov.reader.PRs(ctx, x.t.repo, x.r.branches, x.t.prov.authors)
		return err
	})
	if err != nil {
		x.fail("list the target's pull requests again", err)
		return "", false
	}
	x.index(fresh)
	if x.w.Sweep {
		return "", x.sweepStillOpen(fresh)
	}
	return x.prsMoved(fresh), true
}

// expectedRefs returns the branch heads the work depends on, by full ref
// name ("" for a branch that must not exist): the lease of each branch a
// step pushes to or deletes, the classified head of the sync branch of
// every other step, and B when the work pushes or opens a pull request.
// The first step that names a branch sets its head.
func (x *targetExec) expectedRefs() map[string]string {
	w := x.w
	want := map[string]string{}
	set := func(branch, head string) {
		if ref := "refs/heads/" + branch; branch != "" {
			if _, done := want[ref]; !done {
				want[ref] = head
			}
		}
	}
	base := false
	for _, s := range workSteps(w) {
		switch s.Kind {
		case decide.StepPush, decide.StepRecreateBranch:
			set(s.Branch, s.Expect)
			base = true
		case decide.StepDeleteBranch:
			set(s.Branch, s.Expect)
		case decide.StepCreatePR:
			base = true
			fallthrough
		default:
			if head, ok := x.classified(s.Branch); ok {
				set(s.Branch, head)
			}
		}
	}
	if base && w.DefaultBranch != "" && w.B != "" {
		set(w.DefaultBranch, w.B)
	}
	return want
}

// classified returns the head inspection found for the sync branch name
// ("" when absent), and whether it classified that branch.
func (x *targetExec) classified(name string) (string, bool) {
	w := x.w
	if name == "" {
		return "", false
	}
	if name == w.Branch.Name {
		return w.Branch.Head, true
	}
	for key, b := range w.Aliases {
		if cmp.Or(b.Name, key) == name {
			return b.Head, true
		}
	}
	return "", false
}

// touched returns the branches the work's steps write to: pushes, deletes,
// a new pull request's head, and the heads of the pull requests it edits.
func (x *targetExec) touched() map[string]bool {
	out := map[string]bool{}
	for _, s := range workSteps(x.w) {
		if s.Branch != "" {
			out[s.Branch] = true
		}
	}
	return out
}

// prSeen is what inspection saw of one pull request on the sync branches:
// what the decision rests on, and what the recheck compares.
type prSeen struct {
	state platform.PRState
	head  string
	// local is set when its head branch lives in the target repository.
	local bool
	// key is the content key of its marker when the body holds a valid
	// marker of this hub ("" when not): the pull request is touchmark's by
	// its marker.
	key string
	// recreate and repropose are its ticked controls, the only ones that
	// act (I11).
	recreate, repropose bool
}

// seenOf is what pr shows now in repository repo, for a hub with
// fingerprints fps.
func seenOf(pr platform.PR, repo platform.Repo, fps []string) prSeen {
	s := prSeen{
		state:     pr.State,
		head:      pr.Head,
		local:     fromTargetRepo(pr, repo),
		recreate:  prbody.Ticked(pr.Body, prbody.ControlRecreate),
		repropose: prbody.Ticked(pr.Body, prbody.ControlRepropose),
	}
	if m, status := marker.Find(pr.Body, fps); status == marker.Found {
		s.key = m.Key
	}
	return s
}

// seenAll is seenOf of every pull request of list, by number.
func seenAll(list []platform.PR, repo platform.Repo, fps []string) map[int64]prSeen {
	out := make(map[int64]prSeen, len(list))
	for _, pr := range list {
		out[pr.Number] = seenOf(pr, repo, fps)
	}
	return out
}

// writesTo returns the pull requests the work's steps write to.
func (x *targetExec) writesTo() map[int64]bool {
	out := map[int64]bool{}
	for _, s := range workSteps(x.w) {
		if s.PR > 0 {
			out[s.PR] = true
		}
	}
	return out
}

// prsMoved compares the pull requests listed now with what inspection saw
// of them (Work.listed) and says how the target moved, "" when it did not:
//   - a pull request inspection did not list that is open from the target
//     repository on a branch the steps touch (it would block them: I3), or
//     on any sync branch by one of touchmark's authors (it may be
//     touchmark's own, or a duplicate);
//   - a pull request that matters (touchmark's own, adopted or written to,
//     or from the target repository on a branch the steps touch) that
//     changed state, or that was open and is no longer listed;
//   - a pull request the steps write to whose marker of this hub is gone or
//     changed (people took it over or edited it), or whose ticked controls
//     changed (a recreate or repropose withdrawn, or given: the decision
//     rests on them, I2).
//
// Pull requests inspection listed and that did not change do not move the
// target, whoever opened them.
func (x *targetExec) prsMoved(fresh []platform.PR) string {
	w := x.w
	own := map[int64]bool{}
	for _, o := range w.Own {
		own[o.PR.Number] = true
	}
	if w.Open != nil {
		own[w.Open.PR.Number] = true
	}
	touched, writes := x.touched(), x.writesTo()
	matters := func(n int64, s prSeen) bool { return own[n] || writes[n] || (touched[s.head] && s.local) }
	listed := map[int64]bool{}
	for _, pr := range fresh {
		listed[pr.Number] = true
		now := seenOf(pr, x.t.repo, x.r.fps)
		was, known := w.listed[pr.Number]
		switch {
		case !known && now.state == platform.Open && now.local && touched[now.head]:
			return fmt.Sprintf("#%d by %s was opened from %s", pr.Number, cmp.Or(pr.Author.Login, "someone"), pr.Head)
		case !known && now.state == platform.Open && now.local && slices.Contains(x.t.prov.ids, pr.Author.ID):
			return fmt.Sprintf("#%d by %s was opened from %s", pr.Number, cmp.Or(pr.Author.Login, "someone"), pr.Head)
		case !known || !matters(pr.Number, was):
		case now.state != was.state:
			return fmt.Sprintf("#%d is %s now, not %s", pr.Number, now.state, was.state)
		case !writes[pr.Number]:
		case was.key != "" && now.key == "":
			return fmt.Sprintf("#%d no longer carries touchmark's marker", pr.Number)
		case was.key != now.key:
			return fmt.Sprintf("the marker of #%d changed", pr.Number)
		case was.recreate != now.recreate:
			return fmt.Sprintf("the rebuild control of #%d was %s", pr.Number, tickedWord(now.recreate))
		case was.repropose != now.repropose:
			return fmt.Sprintf("the propose-again control of #%d was %s", pr.Number, tickedWord(now.repropose))
		}
	}
	for _, n := range keysInOrder(w.listed) {
		if was := w.listed[n]; was.state == platform.Open && !listed[n] && matters(n, was) {
			return fmt.Sprintf("#%d is no longer listed on the sync branches", n)
		}
	}
	return ""
}

// tickedWord says how a control changed.
func tickedWord(ticked bool) string {
	if ticked {
		return "ticked"
	}
	return "unticked"
}

// sweepStillOpen reports whether a sweep close still has its pull request to
// close: open, and touchmark's by decide.Identity. Otherwise the target ends
// unchanged, with a warning.
func (x *targetExec) sweepStillOpen(fresh []platform.PR) bool {
	sp := x.w.SweepPR
	if sp == nil {
		x.end(report.OutcomeFailed, "internal", "a sweep close without its pull request")
		return false
	}
	i := slices.IndexFunc(fresh, func(pr platform.PR) bool { return pr.Number == sp.PR.Number })
	if i < 0 {
		x.end(report.OutcomeUnchanged, "", fmt.Sprintf("#%d is no longer listed; touchmark closes nothing", sp.PR.Number))
		return false
	}
	pr := fresh[i]
	x.setPR(pr)
	if pr.State != platform.Open {
		x.end(report.OutcomeUnchanged, "", fmt.Sprintf("#%d was %s before touchmark closed it", pr.Number, pr.State))
		return false
	}
	id := decide.Identity{Branches: x.r.branches, Authors: x.t.prov.ids, Fingerprints: x.r.fps}
	if _, status := id.Own(pr); status != decide.Ours {
		x.end(report.OutcomeUnchanged, "", fmt.Sprintf("#%d no longer carries touchmark's marker; touchmark leaves it alone", sp.PR.Number))
		return false
	}
	return true
}

// index records what the work knows of its pull requests, the listing just
// read over what inspection saw: their state and body, and the marker of
// each own one. A listed pull request's marker is its listed body's (none
// when that body holds no valid marker of ours: a marker people erased is
// never taken from inspection); inspection's marker stays only for one the
// listing left out.
func (x *targetExec) index(fresh []platform.PR) {
	w := x.w
	x.prs, x.marks = map[int64]platform.PR{}, map[int64]marker.Marker{}
	add := func(o *decide.OwnPR) {
		if o != nil {
			x.prs[o.PR.Number], x.marks[o.PR.Number] = o.PR, o.Marker
		}
	}
	for i := range w.Own {
		add(&w.Own[i])
	}
	add(w.Open)
	add(w.SweepPR)
	for _, pr := range fresh {
		x.prs[pr.Number] = pr
		m, status := marker.Find(pr.Body, x.r.fps)
		if status != marker.Found {
			m = marker.Marker{}
		}
		x.marks[pr.Number] = m
	}
}

// retryRead runs read as retry does (circuit.go): again after 1 and 2 s
// when it fails transiently, again after the provider's pause when it is
// rate limited; no pause lasts past the block's bound.
func (x *targetExec) retryRead(ctx context.Context, read func() error) error {
	return retryRead(ctx, x.t.prov.throttle(), x.ex.sleep, x.ex.jitter, x.bound, read)
}

// fromTargetRepo reports whether pr's head branch lives in repo itself, not
// in a fork: only such a pull request uses the target's own sync branch.
func fromTargetRepo(pr platform.PR, repo platform.Repo) bool {
	return pr.HeadRepoID != "" && (pr.HeadRepoID == pr.RepoID || pr.HeadRepoID == repo.ID)
}

// labelHead is a branch head for messages: a short commit id, or "absent".
func labelHead(head string) string {
	if head == "" {
		return "absent"
	}
	return short(head)
}

// keysInOrder returns the keys of m in order.
func keysInOrder[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
