package ghfake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxBody is GitHub's limit on a pull request body, in characters.
const maxBody = 65536

// refChange is one ref of a repository moving; old or new is "" when the
// ref is absent.
type refChange struct {
	ref, old, new string
}

// branchOf returns the branch a ref names, ok false for other refs.
func branchOf(ref string) (string, bool) {
	return strings.CutPrefix(ref, "refs/heads/")
}

// prsFrom lists the pull requests, in every repository, whose head is
// branch of r, oldest first. Called with mu held.
func (s *Server) prsFrom(r *repo, branch string) []*pr {
	var out []*pr
	for _, id := range sortedIDs(s.repos) {
		for _, p := range s.repos[id].prs {
			if p.headRepo == r && p.headRef == branch {
				out = append(out, p)
			}
		}
	}
	return out
}

// sortedIDs returns the keys of m in order.
func sortedIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// refsMoved records that refs of r moved, by actor (nil for setup
// methods), and makes pull requests follow like GitHub:
//   - an open pull request whose head branch is deleted closes (documented);
//   - an open pull request whose head moves takes the new head, with a
//     HeadRefForcePushedEvent when the move is not a fast-forward; when the
//     new head adds nothing to its base (equal to or behind the base's tip)
//     it closes (observed for equal, not documented; behind is assumed);
//   - an open pull request whose base moves so that its head becomes
//     reachable from the base is merged ("indirect merges", documented);
//   - an open pull request whose base branch is deleted is retargeted to
//     the base of a merged pull request whose head that branch was
//     (documented), and closed otherwise (public reports).
//
// judged marks touchmark's writes (pushes and API writes through HTTP by
// an identity not marked Human), whose forbidden transitions are recorded
// first, against the state before them. Called with gitMu and mu held.
func (s *Server) refsMoved(ctx context.Context, r *repo, changes []refChange, actor *account, judged bool) error {
	if len(changes) == 0 {
		return nil
	}
	rg := s.repoGit(r)
	if judged && actor != nil && !actor.human {
		for _, c := range changes {
			s.judge(ctx, r, c, actor)
		}
	}
	r.pushed = s.now()
	var baseMoves []refChange
	for _, c := range changes {
		branch, ok := branchOf(c.ref)
		if !ok {
			continue
		}
		baseMoves = append(baseMoves, c)
		for _, p := range s.prsFrom(r, branch) {
			if !p.open {
				continue
			}
			if c.new == "" {
				s.addEvent(p, EventHeadRefDeleted, actor, "")
				s.finish(p, false, actor)
				continue
			}
			if c.old != "" {
				if ff, err := rg.isAncestor(ctx, c.old, c.new); err == nil && !ff {
					s.addEvent(p, EventHeadRefForced, actor, "")
				}
			}
			p.headSHA = c.new
			p.updated = s.now()
			if err := s.syncPullRef(ctx, p); err != nil {
				return err
			}
			base := s.tip(ctx, p.repo, p.baseRef)
			if base == "" {
				continue
			}
			if in, err := s.repoGit(p.repo).isAncestor(ctx, p.headSHA, base); err == nil && in {
				s.finish(p, false, actor)
			}
		}
	}
	for _, c := range baseMoves {
		branch, _ := branchOf(c.ref)
		for _, p := range r.prs {
			if !p.open || p.baseRef != branch {
				continue
			}
			if c.new == "" {
				if to := s.retarget(ctx, r, branch); to != "" {
					s.baseChanged(p, actor, to)
					p.baseSHA = s.tip(ctx, r, to)
					continue
				}
				s.finish(p, false, actor)
				continue
			}
			p.baseSHA = c.new
			if p.headSHA == "" {
				continue
			}
			if in, err := rg.isAncestor(ctx, p.headSHA, c.new); err == nil && in {
				p.mergeSHA = c.new
				s.finish(p, true, actor)
			}
		}
	}
	return nil
}

// retarget returns the base of a merged pull request of r whose head was
// branch, when that base still exists. Called with mu held.
func (s *Server) retarget(ctx context.Context, r *repo, branch string) string {
	for _, p := range slices.Backward(r.prs) {
		if p.merged && p.headRepo == r && p.headRef == branch && s.tip(ctx, r, p.baseRef) != "" {
			return p.baseRef
		}
	}
	return ""
}

// tip returns the tip of a branch of r, "" when absent or unreadable.
func (s *Server) tip(ctx context.Context, r *repo, branch string) string {
	id, err := s.repoGit(r).branch(ctx, branch)
	if err != nil {
		return ""
	}
	return id
}

// syncPullRef points refs/pull/<n>/head of the base repository at the
// pull request's head, fetching it from a fork. Called with mu held.
func (s *Server) syncPullRef(ctx context.Context, p *pr) error {
	ref := "refs/pull/" + strconv.FormatInt(p.number, 10) + "/head"
	rg := s.repoGit(p.repo)
	if p.headRepo != nil && p.headRepo != p.repo {
		return rg.fetchFrom(ctx, p.headRepo.dir, p.headSHA, ref)
	}
	_, err := rg.g.Run(ctx, nil, "update-ref", ref, p.headSHA)
	return err
}

// addEvent appends a timeline event. Called with mu held.
func (s *Server) addEvent(p *pr, typ string, actor *account, reason string) {
	p.events = append(p.events, &event{id: s.id(), typ: typ, actor: actor, at: s.now(), stateReason: reason})
}

// baseChanged moves a pull request to base to, with a BaseRefChangedEvent.
// Called with mu held.
func (s *Server) baseChanged(p *pr, actor *account, to string) {
	s.addEvent(p, EventBaseRefChanged, actor, "")
	e := p.events[len(p.events)-1]
	e.from, e.to = p.baseRef, to
	p.baseRef = to
}

// finish closes or merges an open pull request as actor: a merge records
// a MergedEvent and a ClosedEvent, as GitHub does (observed read-only
// 2026-09-29). Called with mu held.
func (s *Server) finish(p *pr, merged bool, actor *account) {
	now := s.now()
	p.open, p.closedAt, p.updated = false, now, now
	reason := "NOT_PLANNED"
	if merged {
		p.merged, p.mergedAt, p.mergedBy = true, now, actor
		s.addEvent(p, EventMerged, actor, "")
		reason = "COMPLETED"
	}
	s.addEvent(p, EventClosed, actor, reason)
}

// newPR is a pull request to open.
type newPR struct {
	head, base, title, body string
	headRepo                *repo
	draft                   bool
	author                  *account
}

// openPR validates and opens a pull request in r, or returns GitHub's
// refusal. Called with gitMu and mu held.
func (s *Server) openPR(ctx context.Context, r *repo, np newPR) (*pr, *response) {
	fail := func(resp response) (*pr, *response) { return nil, &resp }
	if r.prsDisabled {
		return fail(validation(customError("PullRequest", "", "Pull requests are disabled for this repository.")))
	}
	if strings.TrimSpace(np.title) == "" {
		return fail(validation(map[string]any{"resource": "PullRequest", "code": "missing_field", "field": "title"}))
	}
	if resp := bodyTooLong(np.body); resp != nil {
		return fail(*resp)
	}
	base := s.tip(ctx, r, np.base)
	if base == "" {
		return fail(validation(map[string]any{"resource": "PullRequest", "field": "base", "code": "invalid"}))
	}
	hr := np.headRepo
	if hr == nil {
		hr = r
	}
	head := s.tip(ctx, hr, np.head)
	if head == "" {
		return fail(validation(map[string]any{"resource": "PullRequest", "field": "head", "code": "invalid"}))
	}
	for _, p := range r.prs {
		if p.open && p.headRepo == hr && p.headRef == np.head && p.baseRef == np.base {
			return fail(validation(customError("PullRequest", "",
				"A pull request already exists for "+hr.owner.login+":"+np.head+".")))
		}
	}
	if np.draft && (r.noDrafts || (r.private() && r.owner.plan == PlanFree)) {
		return fail(validation(customError("PullRequest", "", "Draft pull requests are not supported in this repository.")))
	}
	number := r.nextNumber + 1
	p := &pr{id: s.id(), number: number, repo: r, headRepo: hr, headOwner: hr.owner.login, headRef: np.head,
		headSHA: head, baseRef: np.base, baseSHA: base, title: np.title, body: np.body, draft: np.draft,
		open: true, author: np.author, created: s.now(), updated: s.now()}
	if err := s.syncPullRef(ctx, p); err != nil {
		resp := apiError(http.StatusInternalServerError, "Server Error")
		return nil, &resp
	}
	if in, err := s.repoGit(r).isAncestor(ctx, head, base); err != nil || in {
		_, _ = s.repoGit(r).g.Run(ctx, nil, "update-ref", "-d", "refs/pull/"+strconv.FormatInt(number, 10)+"/head")
		return fail(validation(customError("PullRequest", "", "No commits between "+np.base+" and "+np.head)))
	}
	r.nextNumber = number
	r.prs = append(r.prs, p)
	s.opened(hr, np.head)
	return p, nil
}

// bodyTooLong refuses a body over 65 536 characters.
func bodyTooLong(body string) *response {
	if utf8.RuneCountInString(body) <= maxBody {
		return nil
	}
	resp := validation(customError("Issue", "body", "body is too long (maximum is 65536 characters)"))
	return &resp
}

// prEdit is a change of a pull request; nil fields stay.
type prEdit struct {
	title, body, state, base *string
}

// editPR validates and applies an edit as actor, all or nothing. Called
// with gitMu and mu held.
func (s *Server) editPR(ctx context.Context, p *pr, e prEdit, actor *account) *response {
	fail := func(resp response) *response { return &resp }
	if e.title != nil && strings.TrimSpace(*e.title) == "" {
		return fail(validation(map[string]any{"resource": "PullRequest", "code": "missing_field", "field": "title"}))
	}
	if e.body != nil {
		if resp := bodyTooLong(*e.body); resp != nil {
			return resp
		}
	}
	reopen, closing := false, false
	if e.state != nil {
		switch *e.state {
		case "open":
			reopen = !p.open
		case "closed":
			closing = p.open
		default:
			return fail(validation(map[string]any{"resource": "PullRequest", "code": "invalid", "field": "state"}))
		}
	}
	if reopen {
		if resp := s.checkReopen(ctx, p); resp != nil {
			return resp
		}
	}
	baseChange := e.base != nil && *e.base != p.baseRef
	if baseChange {
		switch {
		case !p.open && !reopen:
			return fail(validation(customError("PullRequest", "base", "Cannot change the base branch of a closed pull request.")))
		case s.tip(ctx, p.repo, *e.base) == "":
			return fail(validation(map[string]any{"resource": "PullRequest", "field": "base", "code": "invalid"}))
		}
		if in, err := s.repoGit(p.repo).isAncestor(ctx, p.headSHA, s.tip(ctx, p.repo, *e.base)); err == nil && in {
			return fail(validation(customError("PullRequest", "base",
				"There are no new commits between base branch '"+*e.base+"' and head branch '"+p.headRef+"'")))
		}
	}
	if e.title != nil {
		p.title = *e.title
	}
	if e.body != nil {
		p.body = *e.body
	}
	if baseChange {
		s.baseChanged(p, actor, *e.base)
		p.baseSHA = s.tip(ctx, p.repo, p.baseRef)
	}
	switch {
	case closing:
		s.finish(p, false, actor)
	case reopen:
		p.open, p.closedAt = true, time.Time{}
		if hr := p.headRepo; hr != nil {
			p.headSHA = s.tip(ctx, hr, p.headRef)
			_ = s.syncPullRef(ctx, p)
		}
		s.addEvent(p, EventReopened, actor, "")
		s.opened(p.headRepo, p.headRef)
	}
	p.updated = s.now()
	return nil
}

// checkReopen refuses to reopen a merged pull request, one whose head
// branch is gone or was force-pushed or recreated since it closed, or one
// whose head adds nothing to its base (messages assumed). Called with mu
// held.
func (s *Server) checkReopen(ctx context.Context, p *pr) *response {
	fail := func(msg string) *response {
		resp := validation(customError("PullRequest", "state", msg))
		return &resp
	}
	if p.merged {
		return fail("state cannot be changed. The pull request has already been merged.")
	}
	if p.headRepo == nil {
		return fail("state cannot be changed. The repository that submitted this pull request has been deleted.")
	}
	head := s.tip(ctx, p.headRepo, p.headRef)
	if head == "" {
		return fail("state cannot be changed. The " + p.headRef + " branch has been deleted.")
	}
	if head != p.headSHA {
		if err := s.fetchHead(ctx, p, head); err != nil {
			return fail("state cannot be changed.")
		}
		if in, err := s.repoGit(p.repo).isAncestor(ctx, p.headSHA, head); err != nil || !in {
			return fail("state cannot be changed. The " + p.headRef + " branch was force-pushed or recreated.")
		}
	}
	base := s.tip(ctx, p.repo, p.baseRef)
	if base == "" {
		return fail("state cannot be changed. The " + p.baseRef + " branch has been deleted.")
	}
	if in, err := s.repoGit(p.repo).isAncestor(ctx, head, base); err == nil && in {
		return fail("state cannot be changed. There are no new commits on the " + p.headRef + " branch.")
	}
	return nil
}

// fetchHead makes commit id of a fork's head available in the base
// repository. Called with mu held.
func (s *Server) fetchHead(ctx context.Context, p *pr, id string) error {
	if p.headRepo == p.repo {
		return nil
	}
	return s.repoGit(p.repo).fetchFrom(ctx, p.headRepo.dir, id, "")
}

// judge records the forbidden transitions (see Violations) of one ref
// change by a touchmark identity, against the state before it. Called
// with gitMu and mu held.
func (s *Server) judge(ctx context.Context, r *repo, c refChange, by *account) {
	branch, ok := branchOf(c.ref)
	if !ok {
		return
	}
	verb := "moved"
	if c.new == "" {
		verb = "deleted"
	}
	if branch == r.defaultBranch {
		s.violate("default-branch %s: %s %s %s, the default branch", r.path(), by.login, verb, branch)
	}
	var open []*pr
	var last *pr
	for _, p := range s.prsFrom(r, branch) {
		if p.open {
			open = append(open, p)
		} else {
			last = p
		}
	}
	for _, p := range open {
		ref := p.repo.path() + "#" + strconv.FormatInt(p.number, 10)
		if c.new == "" {
			s.violate("deleted-open-branch %s: %s deleted %s, the branch of an open pull request", ref, by.login, branch)
		}
		if !s.own(by, p.author) {
			s.violate("foreign-branch %s: %s %s %s, the branch of an open pull request by %s", ref, by.login, verb,
				branch, p.author.login)
		}
		base := s.tip(ctx, p.repo, p.baseRef)
		if c.new == "" || base == "" {
			continue
		}
		prg := s.repoGit(p.repo)
		if p.repo != r {
			if err := prg.fetchFrom(ctx, r.dir, c.new, ""); err != nil {
				continue
			}
		}
		if in, err := prg.isAncestor(ctx, c.new, base); err == nil && in {
			s.violate("head-to-base %s: %s moved %s to %s, which %s already contains", ref, by.login, branch, c.new, p.baseRef)
		}
	}
	if c.new != "" && len(open) == 0 && last != nil {
		state := "closed"
		if last.merged {
			state = "merged"
		}
		s.addPending(r.id, branch, fmt.Sprintf(
			"closed-branch-push %s#%d: %s pushed to %s, the branch of a %s pull request, and opened no new one",
			last.repo.path(), last.number, by.login, branch, state))
	}
}

// own reports whether a pull request by author counts as pusher's own.
// Called with mu held.
func (s *Server) own(pusher, author *account) bool {
	return author == pusher || s.known[pusher.id][author.id]
}
