package fake

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// refChange is one branch of a repository moving.
type refChange struct {
	branch   string
	old, new string // "" when the branch is absent
}

// diffRefs returns the branches that differ between before and after, by
// name.
func diffRefs(before, after map[string]string) []refChange {
	var out []refChange
	for name, id := range after {
		if before[name] != id {
			out = append(out, refChange{branch: name, old: before[name], new: id})
		}
	}
	for name, id := range before {
		if _, ok := after[name]; !ok {
			out = append(out, refChange{branch: name, old: id})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].branch < out[j].branch })
	return out
}

// pendingPush is a push to the branch of a closed pull request: a
// violation unless a new pull request from that branch follows.
type pendingPush struct {
	repoID, branch, text string
}

// prRef is a pull request with its repository.
type prRef struct {
	repo *repoState
	ps   *prState
}

// String names the pull request as "<repo path>#<number>".
func (r prRef) String() string {
	return r.repo.repo.Path + "#" + strconv.FormatInt(r.ps.pr.Number, 10)
}

// prsFrom lists the pull requests whose head is branch of the repository
// with id headRepo, by repository id and number. Called with mu held.
func (p *Platform) prsFrom(headRepo, branch string) []prRef {
	ids := make([]string, 0, len(p.repos))
	for id := range p.repos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []prRef
	for _, id := range ids {
		s := p.repos[id]
		for _, n := range sortedNumbers(s) {
			if pr := s.prs[n].pr; pr.HeadRepoID == headRepo && pr.Head == branch {
				out = append(out, prRef{repo: s, ps: s.prs[n]})
			}
		}
	}
	return out
}

// gitNewPR checks a new pull request against the branches in git mode:
// both exist, and GitHub refuses a head its base already contains ("No
// commits between"). Called with mu held.
func (p *Platform) gitNewPR(ctx context.Context, op string, s *repoState, np platform.NewPR) error {
	head, base := s.refs[np.Head], s.refs[np.Base]
	switch {
	case head == "":
		return invalid(op, "the head branch %q does not exist in %s", np.Head, s.repo.Path)
	case base == "":
		return invalid(op, "the base branch %q does not exist in %s", np.Base, s.repo.Path)
	}
	switch Flavor(p.caps.Flavor) {
	case GitLab, Gitea, Forgejo:
		return nil
	}
	in, err := p.repoGit(s).isAncestor(ctx, head, base)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	if in {
		return invalid(op, "no commits between %s and %s", np.Base, np.Head)
	}
	return nil
}

// headTip returns the tip of pr's head branch in git mode, "" when the
// branch or its repository does not exist. Called with mu held.
func (p *Platform) headTip(pr platform.PR) string {
	if s := p.repos[pr.HeadRepoID]; s != nil {
		return s.refs[pr.Head]
	}
	return ""
}

// commitSetup commits one change of a setup method to the default branch
// of s and updates its tree. An unchanged entry makes no commit. Called
// with gitMu and mu held.
func (p *Platform) commitSetup(s *repoState, c fileChange, msg string) error {
	var entry snapshot.Entry
	if c.mode != "" {
		entry = snapshot.Entry{Mode: c.mode, OID: c.oid}
		if c.mode != ModeGitlink {
			entry.OID = objectID(s.repo.ObjectFormat, "blob", c.data)
		}
		if old, ok := s.entries[c.path]; ok && old == entry {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	branch := s.repo.DefaultBranch
	old := s.refs[branch]
	sign := p.setupSign()
	head, err := p.repoGit(s).importCommit(ctx, branch, old, []fileChange{c}, sign, sign, msg)
	if err != nil {
		return err
	}
	if c.mode == "" {
		delete(s.entries, c.path)
	} else {
		s.entries[c.path] = entry
	}
	return p.refsMoved(ctx, s, []refChange{{branch: branch, old: old, new: head}}, nil, false, true)
}

// refsMoved records that branches of s moved: the ref mirror, the tree of
// the default branch (read from git unless treeKnown), the pull requests
// that follow the branches and the flavor's reactions. by is who moved
// them, nil for setup methods. pushed marks a push through the server,
// whose forbidden transitions are recorded first, against the state before
// it. Called with gitMu and mu held.
func (p *Platform) refsMoved(ctx context.Context, s *repoState, changes []refChange, by *platform.Account, pushed, treeKnown bool) error {
	if pushed && by != nil {
		for _, c := range changes {
			p.judgePush(ctx, s, c, *by)
		}
	}
	defaultMoved := false
	for _, c := range changes {
		if c.new == "" {
			delete(s.refs, c.branch)
		} else {
			s.refs[c.branch] = c.new
		}
		defaultMoved = defaultMoved || c.branch == s.repo.DefaultBranch
	}
	if defaultMoved && !treeKnown {
		if err := p.loadTree(ctx, s); err != nil {
			return err
		}
	}
	var moved []prRef
	seen := map[*prState]bool{}
	add := func(r prRef) {
		if !seen[r.ps] {
			seen[r.ps] = true
			moved = append(moved, r)
		}
	}
	for _, c := range changes {
		for _, r := range p.prsFrom(s.repo.ID, c.branch) {
			pr := &r.ps.pr
			switch {
			case pr.State != platform.Open:
			case c.new == "":
				// Every platform closes a pull request whose branch is gone.
				p.finish(pr, platform.Closed, by, time.Time{})
			default:
				pr.HeadSHA = c.new
				add(r)
			}
		}
		for _, n := range sortedNumbers(s) {
			ps := s.prs[n]
			if ps.pr.Base != c.branch {
				continue
			}
			ps.pr.BaseExists = c.new != ""
			if c.new != "" && ps.pr.State == platform.Open {
				add(prRef{repo: s, ps: ps})
			}
		}
	}
	for _, r := range moved {
		p.settle(ctx, r, by)
	}
	return nil
}

// loadTree reads the tree of the default branch of s into its entries.
// Called with mu held.
func (p *Platform) loadTree(ctx context.Context, s *repoState) error {
	tip := s.refs[s.repo.DefaultBranch]
	if tip == "" {
		s.entries = map[string]snapshot.Entry{}
		return nil
	}
	entries, err := p.repoGit(s).entries(ctx, tip)
	if err != nil {
		return fmt.Errorf("read the tree of %s: %w", s.repo.Path, err)
	}
	s.entries = entries
	return nil
}

// settle applies the flavor's reaction to an open pull request whose head
// or base moved: GitHub closes it when its head is its base's tip, Gitea
// and Forgejo mark it merged once its head is reachable from its base.
// Called with mu held.
func (p *Platform) settle(ctx context.Context, r prRef, by *platform.Account) {
	pr := &r.ps.pr
	base := r.repo.refs[pr.Base]
	if pr.State != platform.Open || pr.HeadSHA == "" || base == "" {
		return
	}
	switch Flavor(p.caps.Flavor) {
	case GitLab:
	case Gitea, Forgejo:
		// A head from a fork is not in the base repository: no answer.
		if in, err := p.repoGit(r.repo).isAncestor(ctx, pr.HeadSHA, base); err == nil && in {
			p.finish(pr, platform.Merged, by, time.Time{})
		}
	default:
		if pr.HeadSHA == base {
			p.finish(pr, platform.Closed, by, time.Time{})
		}
	}
}

// finish closes or merges an open pull request at at (the clock when zero)
// as by, nil when the platform does not name anyone. Called with mu held.
func (p *Platform) finish(pr *platform.PR, state platform.PRState, by *platform.Account, at time.Time) {
	if at.IsZero() {
		at = p.now()
	}
	pr.State, pr.ClosedAt, pr.ClosedBy = state, at, nil
	if by != nil {
		c := p.refresh(*by)
		pr.ClosedBy = &c
	}
}

// judgePush records the forbidden transitions (see Violations) of one
// branch moved by a push of by, against the state before the push. Called
// with gitMu and mu held.
func (p *Platform) judgePush(ctx context.Context, s *repoState, c refChange, by platform.Account) {
	who := p.refresh(by).Login
	verb := "moved"
	if c.new == "" {
		verb = "deleted"
	}
	if c.branch == s.repo.DefaultBranch {
		p.violate("default-branch %s: %s %s %s, the default branch", s.repo.Path, who, verb, c.branch)
	}
	var open []prRef
	var last prRef
	for _, r := range p.prsFrom(s.repo.ID, c.branch) {
		if r.ps.pr.State == platform.Open {
			open = append(open, r)
		} else {
			last = r
		}
	}
	for _, r := range open {
		pr := r.ps.pr
		if c.new == "" {
			p.violate("deleted-open-branch %s: %s deleted %s, the branch of an open pull request", r, who, c.branch)
		}
		if !p.own(by, pr.Author) {
			p.violate("foreign-branch %s: %s %s %s, the branch of an open pull request by %s", r, who, verb, c.branch,
				p.refresh(pr.Author).Login)
		}
		base := r.repo.refs[pr.Base]
		if c.new == "" || base == "" {
			continue
		}
		if in, err := p.repoGit(r.repo).isAncestor(ctx, c.new, base); err == nil && in {
			p.violate("head-to-base %s: %s moved %s to %s, which %s already contains", r, who, c.branch, c.new, pr.Base)
		}
	}
	if c.new != "" && len(open) == 0 && last.ps != nil {
		p.addPending(s.repo.ID, c.branch, fmt.Sprintf(
			"closed-branch-push %s: %s pushed to %s, the branch of a %s pull request, and opened no new one",
			last, who, c.branch, last.ps.pr.State))
	}
}

// own reports whether a pull request by author counts as pusher's own.
// Called with mu held.
func (p *Platform) own(pusher, author platform.Account) bool {
	return author.ID == pusher.ID || p.known[pusher.ID][author.ID]
}

// violate records a violation. Called with mu held.
func (p *Platform) violate(format string, args ...any) {
	p.violations = append(p.violations, fmt.Sprintf(format, args...))
}

// addPending records a push to the branch of a closed pull request, once
// per branch. Called with mu held.
func (p *Platform) addPending(repoID, branch, text string) {
	for i, pp := range p.pending {
		if pp.repoID == repoID && pp.branch == branch {
			p.pending[i].text = text
			return
		}
	}
	p.pending = append(p.pending, pendingPush{repoID: repoID, branch: branch, text: text})
}

// opened clears the pending pushes a new pull request from branch
// justifies. Called with mu held.
func (p *Platform) opened(repoID, branch string) {
	p.pending = slices.DeleteFunc(p.pending, func(pp pendingPush) bool {
		return pp.repoID == repoID && pp.branch == branch
	})
}

// runQuickActions runs, as GitLab does, the quick actions of text that by
// wrote in where ("the description", "a comment") of a pull request of s:
// lines whose first non-blank character is "/". Each is recorded as a
// violation. Called with mu held.
func (p *Platform) runQuickActions(s *repoState, ps *prState, text string, by platform.Account, where string) {
	ref := prRef{repo: s, ps: ps}
	for _, line := range strings.Split(text, "\n") {
		cmd := strings.TrimSpace(line)
		if !strings.HasPrefix(cmd, "/") {
			continue
		}
		p.violate("quick-action %s: %s ran %q in %s", ref, by.Login, cmd, where)
		fields := strings.Fields(cmd)
		pr := &ps.pr
		switch fields[0] {
		case "/close":
			if pr.State == platform.Open {
				p.finish(pr, platform.Closed, &by, time.Time{})
			}
		case "/merge":
			if pr.State == platform.Open {
				p.finish(pr, platform.Merged, &by, time.Time{})
			}
		case "/label":
			for _, f := range fields[1:] {
				name := strings.Trim(strings.TrimPrefix(f, "~"), `"`)
				if name == "" {
					continue
				}
				p.label(s, name, true)
				if !slices.Contains(pr.Labels, name) {
					pr.Labels = append(slices.Clone(pr.Labels), name)
				}
			}
		}
	}
}
