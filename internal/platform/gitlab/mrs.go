package gitlab

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of merge request listings.
const (
	// maxMRPages bounds the listing of one source branch of one project.
	// PRs has no way to say it saw only part: past the bound it fails.
	maxMRPages = 50
	// maxSweepPages bounds one instance-wide listing of OpenPRsBy.
	maxSweepPages = 50
)

// apiMR is a merge request as the merge request APIs report it
// (https://docs.gitlab.com/api/merge_requests/). Users in it are short
// (UserBasic): no bot flag.
type apiMR struct {
	ID              int64      `json:"id"`
	IID             int64      `json:"iid"`
	ProjectID       int64      `json:"project_id"`
	Title           string     `json:"title"`
	Description     *string    `json:"description"`
	State           string     `json:"state"` // opened, closed, locked, merged
	CreatedAt       *time.Time `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
	MergedAt        *time.Time `json:"merged_at"`
	ClosedAt        *time.Time `json:"closed_at"`
	MergedBy        *apiUser   `json:"merged_by"`
	MergeUser       *apiUser   `json:"merge_user"`
	ClosedBy        *apiUser   `json:"closed_by"`
	TargetBranch    string     `json:"target_branch"`
	SourceBranch    string     `json:"source_branch"`
	SourceProjectID int64      `json:"source_project_id"`
	TargetProjectID int64      `json:"target_project_id"`
	Author          *apiUser   `json:"author"`
	Labels          []string   `json:"labels"`
	Draft           *bool      `json:"draft"`
	WorkInProgress  bool       `json:"work_in_progress"`
	SHA             *string    `json:"sha"`
	WebURL          string     `json:"web_url"`
}

// check reports what the driver depends on that a merge request lacks.
func (m *apiMR) check(op string) error {
	switch {
	case m.IID <= 0:
		return shapeError(op, "a merge request without an iid")
	case m.State != "opened" && m.State != "closed" && m.State != "merged" && m.State != "locked":
		return shapeError(op, "merge request !%d has state %q", m.IID, m.State)
	case m.Author == nil || m.Author.ID == 0:
		return shapeError(op, "merge request !%d has no author", m.IID)
	case m.TargetProjectID <= 0 || m.TargetBranch == "" || m.SourceBranch == "":
		return shapeError(op, "merge request !%d lacks its target project or branches", m.IID)
	case m.CreatedAt == nil:
		return shapeError(op, "merge request !%d has no created_at", m.IID)
	}
	return nil
}

// draft reports whether m is a draft: the draft field (GitLab ≥ 13.12),
// else work_in_progress.
func (m *apiMR) draft() bool {
	if m.Draft != nil {
		return *m.Draft
	}
	return m.WorkInProgress
}

// shortAccount converts a user of a merge request: its kind is what the
// username tells (the bot flag is not there).
func shortAccount(u *apiUser) platform.Account {
	if u == nil {
		return platform.Account{}
	}
	return platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Username, Kind: kindOf(u.Username, nil)}
}

// toPR converts an API merge request whose target branch exists when
// baseExists. The locked state (GitLab is merging it, or a merge got stuck
// there) is open: the merge request was not closed, and a closed one
// without a closer would be a decline. PRs never returns a
// locked merge request from the project itself (lockedError), and EditPR
// refuses to write to one.
func toPR(m *apiMR, baseExists bool) platform.PR {
	pr := platform.PR{
		Number:     m.IID,
		URL:        m.WebURL,
		State:      platform.Open,
		Draft:      m.draft(),
		Head:       m.SourceBranch,
		Base:       m.TargetBranch,
		RepoID:     strconv.FormatInt(m.TargetProjectID, 10),
		BaseExists: baseExists,
		Title:      m.Title,
		Labels:     []string{},
		Author:     shortAccount(m.Author),
		CreatedAt:  m.CreatedAt.UTC(),
	}
	if m.SHA != nil && isHexOID(*m.SHA) {
		pr.HeadSHA = strings.ToLower(*m.SHA)
	}
	if m.Description != nil {
		pr.Body = *m.Description
	}
	// The source project of an MR from a fork that was deleted since is
	// gone (source_project_id null): never touchmark's own.
	if m.SourceProjectID > 0 {
		pr.HeadRepoID = strconv.FormatInt(m.SourceProjectID, 10)
	}
	pr.Labels = append(pr.Labels, m.Labels...)
	var closedAt *time.Time
	switch m.State {
	case "merged":
		pr.State = platform.Merged
		by := cmp.Or(m.MergedBy, m.MergeUser)
		if by != nil && by.ID != 0 {
			a := shortAccount(by)
			pr.ClosedBy = &a
		}
		closedAt = cmp.Or(m.MergedAt, m.ClosedAt, m.UpdatedAt)
	case "closed":
		pr.State = platform.Closed
		if m.ClosedBy != nil && m.ClosedBy.ID != 0 {
			a := shortAccount(m.ClosedBy)
			pr.ClosedBy = &a
		}
		closedAt = cmp.Or(m.ClosedAt, m.UpdatedAt)
	}
	if closedAt != nil {
		pr.ClosedAt = closedAt.UTC()
	}
	return pr
}

// finishPR fills in the kind of pr's closer, which the short user of a
// merge request lacks: memory tells bots from people by it (a merge
// request a bot closed is no decline).
func (c *client) finishPR(ctx context.Context, op string, pr *platform.PR) error {
	if pr.ClosedBy == nil {
		return nil
	}
	return c.withKind(ctx, op, pr.ClosedBy)
}

// PRs returns, newest first, the merge requests of r from heads by authors
// in every state and the open ones by anyone. One listing
// per head (GET /projects/:id/merge_requests?source_branch=<head>&state=all)
// holds every author's in every state, so the authors are filtered here.
// MRs from forks are included with their own HeadRepoID. Listings are
// complete or the call fails.
func (d *reader) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	const op = "list pull requests"
	if !checkFullPath(r.Path, 2) && projectID(r) == r.Path {
		return nil, invalid(op, "%q is not a group/…/project path", r.Path)
	}
	id := projectID(r)
	wanted := headSet(heads)
	ids := accountIDs(authors)
	if len(wanted) == 0 {
		return []platform.PR{}, nil
	}
	found := map[int64]*apiMR{}
	for _, h := range sortedKeys(wanted) {
		err := d.c.listMRs(ctx, op, id, url.Values{"source_branch": {h}, "state": {"all"}}, func(m apiMR) error {
			if err := m.check(op); err != nil {
				return err
			}
			// The listing is of MRs into this project; one into another
			// (a proxy's mix-up) is not r's.
			if m.SourceBranch != h || id == r.ID && strconv.FormatInt(m.TargetProjectID, 10) != id {
				return nil
			}
			if m.State == "locked" && m.SourceProjectID == m.TargetProjectID {
				return lockedError(op, m.IID, h)
			}
			if m.State == "opened" || m.State == "locked" || ids[strconv.FormatInt(m.Author.ID, 10)] {
				found[m.IID] = &m
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	bases, err := d.c.branchesExist(ctx, op, id, r.DefaultBranch, found)
	if err != nil {
		return nil, err
	}
	out := make([]platform.PR, 0, len(found))
	for _, m := range found {
		pr := toPR(m, bases[m.TargetBranch])
		if err := d.c.finishPR(ctx, op, &pr); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	slices.SortFunc(out, func(a, b platform.PR) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

// lockedError is the error of PRs for a project with merge request iid
// from head locked: GitLab is merging it, so whether it ends merged or
// open is not known yet. The target waits (ClassTransient: the core tries
// the listing again and then fails the target for this run) instead of
// deciding on a state about to change. A merge stuck in the locked state
// keeps the target failing until someone unlocks it, which is what it
// needs anyway.
func lockedError(op string, iid int64, head string) error {
	return &platform.Error{Op: op, Class: platform.ClassTransient,
		Err: fmt.Errorf("merge request !%d from %s is locked: GitLab is merging it (or the merge is stuck)", iid, head)}
}

// listMRs reads every page of the merge requests of project id that q
// selects; a listing over maxMRPages pages fails.
func (c *client) listMRs(ctx context.Context, op, id string, q url.Values, each func(apiMR) error) error {
	complete, err := listAll(ctx, c, op, c.projectURL(id, "merge_requests"), q, maxMRPages, each)
	if err != nil {
		return err
	}
	if !complete {
		return unknown(op, fmt.Errorf("project %s has more merge requests from one branch than %d pages; touchmark does not act on a partial list", id, maxMRPages))
	}
	return nil
}

// branchesExist tells, for the target branch of every MR of mrs, whether
// it exists (GET /projects/:id/repository/branches/:branch; "404 Branch
// Not Found" is a missing branch). The default branch exists: GitLab does
// not delete it.
func (c *client) branchesExist(ctx context.Context, op, id, defaultBranch string, mrs map[int64]*apiMR) (map[string]bool, error) {
	exists := map[string]bool{}
	if defaultBranch != "" {
		exists[defaultBranch] = true
	}
	var names []string
	for _, m := range mrs {
		if _, known := exists[m.TargetBranch]; !known && !slices.Contains(names, m.TargetBranch) {
			names = append(names, m.TargetBranch)
		}
	}
	slices.Sort(names)
	for _, b := range names {
		ok, err := c.branchExists(ctx, op, id, b)
		if err != nil {
			return nil, err
		}
		exists[b] = ok
	}
	return exists, nil
}

// branchExists reports whether branch exists in project id.
func (c *client) branchExists(ctx context.Context, op, id, branch string) (bool, error) {
	_, err := c.get(ctx, op, c.projectURL(id, "repository", "branches", branch), nil, nil)
	if err == nil {
		return true, nil
	}
	if what, ok := missing(err); ok && what == "branch" {
		return false, nil
	}
	if platform.ClassOf(err) == platform.ClassNotFound {
		// The project is gone, or something else answered: not a
		// verdict about the branch.
		return false, unknown(op, err)
	}
	return false, err
}

// getMR reads merge request iid of project id.
func (c *client) getMR(ctx context.Context, op, id string, iid int64) (*apiMR, error) {
	var m apiMR
	if _, err := c.get(ctx, op, c.projectURL(id, "merge_requests", strconv.FormatInt(iid, 10)), nil, &m); err != nil {
		return nil, err
	}
	if err := m.check(op); err != nil {
		return nil, err
	}
	return &m, nil
}

// OpenPRsBy lists the open merge requests by authors from heads in every
// project this identity sees, for the stale sweep: GET
// /merge_requests?scope=all&state=opened&author_id=<id>&source_branch=<b>
// for each author and head. scope=all lists the merge requests of every
// project the caller may read (MergeRequestsFinder over the projects
// visible to it), not only those it is a member of.
//
// A listing that fails, on any page, with a rate limit, a refused
// credential, a transient failure or the end of ctx fails the call, so the
// core can pause or retry; any other failure, or a listing capped at
// maxSweepPages, makes the result incomplete. The project of a hit that
// cannot be read makes it incomplete too (one gone since is left out).
func (d *reader) OpenPRsBy(ctx context.Context, authors []platform.Account, heads []string) (platform.Swept, error) {
	const op = "list open pull requests"
	ids := accountIDs(authors)
	wanted := headSet(heads)
	out := platform.Swept{PRs: []platform.RepoPR{}, Complete: true}
	if len(ids) == 0 || len(wanted) == 0 {
		return out, nil
	}
	hits := map[string]*apiMR{}
	for _, author := range sortedKeys(ids) {
		if n, err := strconv.ParseInt(author, 10, 64); err != nil || n <= 0 {
			// Not a GitLab user id: nothing of it can be listed.
			out.Complete = false
			continue
		}
		for _, h := range sortedKeys(wanted) {
			q := url.Values{"scope": {"all"}, "state": {"opened"}, "author_id": {author}, "source_branch": {h}}
			complete, err := listAll(ctx, d.c, op, d.c.endpoint("merge_requests"), q, maxSweepPages, func(m apiMR) error {
				if m.check(op) != nil {
					out.Complete = false
					return nil
				}
				if m.State != "opened" || m.SourceBranch != h || !ids[strconv.FormatInt(m.Author.ID, 10)] {
					return nil
				}
				hits[strconv.FormatInt(m.TargetProjectID, 10)+"!"+strconv.FormatInt(m.IID, 10)] = &m
				return nil
			})
			switch {
			case err == nil:
				out.Complete = out.Complete && complete
			case fatal(err):
				return platform.Swept{}, err
			default:
				out.Complete = false
			}
		}
	}
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	projects := map[int64]*apiProject{}
	bases := map[string]bool{}
	for _, k := range keys {
		m := hits[k]
		p, ok := projects[m.TargetProjectID]
		if !ok {
			got, err := d.c.getProject(ctx, op, strconv.FormatInt(m.TargetProjectID, 10))
			switch {
			case err == nil:
				p = got
			case platform.ClassOf(err) == platform.ClassNotFound:
				p = nil // gone since the listing
			case stops(err):
				return platform.Swept{}, err
			default:
				out.Complete = false
				continue
			}
			projects[m.TargetProjectID] = p
		}
		if p == nil {
			continue
		}
		repo := d.c.toRepo(p)
		bk := repo.ID + "\x00" + m.TargetBranch
		exists, known := bases[bk]
		if !known {
			if m.TargetBranch == repo.DefaultBranch {
				exists = true
			} else {
				var err error
				exists, err = d.c.branchExists(ctx, op, repo.ID, m.TargetBranch)
				if err != nil {
					if stops(err) {
						return platform.Swept{}, err
					}
					out.Complete = false
					continue
				}
			}
			bases[bk] = exists
		}
		out.PRs = append(out.PRs, platform.RepoPR{Repo: repo, PR: toPR(m, exists)})
	}
	slices.SortFunc(out.PRs, func(a, b platform.RepoPR) int {
		if c := strings.Compare(strings.ToLower(a.Repo.Path), strings.ToLower(b.Repo.Path)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Repo.ID, b.Repo.ID); c != 0 {
			return c
		}
		return cmp.Compare(b.PR.Number, a.PR.Number)
	})
	return out, nil
}

// headSet returns the non-empty heads as a set.
func headSet(heads []string) map[string]bool {
	out := map[string]bool{}
	for _, h := range heads {
		if h != "" {
			out[h] = true
		}
	}
	return out
}

// accountIDs returns the set of the accounts' ids.
func accountIDs(accounts []platform.Account) map[string]bool {
	ids := map[string]bool{}
	for _, a := range accounts {
		if a.ID != "" {
			ids[a.ID] = true
		}
	}
	return ids
}

// sortedKeys returns the keys of m in order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
