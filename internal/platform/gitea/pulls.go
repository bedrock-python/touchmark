package gitea

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of pull request listings.
const (
	// maxPRPages bounds a listing of one repository's pull requests. PRs
	// has no way to say it saw only part: past the bound it fails.
	maxPRPages = 200
	// maxSearchPages bounds the instance-wide search of OpenPRsBy.
	maxSearchPages = 200
	// maxTimelinePages bounds the timeline read after a close.
	maxTimelinePages = 20
)

// Forgejo's pull request flows: 1 is AGit, a pull request without a head
// branch in any repository.
const flowAGit = 1

// apiPR is a pull request as the API reports it.
type apiPR struct {
	Number   int64      `json:"number"`
	HTMLURL  string     `json:"html_url"`
	State    string     `json:"state"`
	Merged   bool       `json:"merged"`
	Draft    bool       `json:"draft"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	User     *apiUser   `json:"user"`
	Labels   []apiLabel `json:"labels"`
	Head     *apiBranch `json:"head"`
	Base     *apiBranch `json:"base"`
	MergedBy *apiUser   `json:"merged_by"`
	Created  *time.Time `json:"created_at"`
	Closed   *time.Time `json:"closed_at"`
	MergedAt *time.Time `json:"merged_at"`
	Flow     *int       `json:"flow"` // Forgejo only
}

// apiBranch is the head or base of a pull request. label is the branch
// name; ref is the branch too, or refs/pull/<n>/head once it is deleted.
// sha is empty when the branch is gone.
type apiBranch struct {
	Label  string   `json:"label"`
	Ref    string   `json:"ref"`
	SHA    string   `json:"sha"`
	RepoID int64    `json:"repo_id"`
	Repo   *apiRepo `json:"repo"`
}

// apiIssue is an issue or pull request of the issue search.
type apiIssue struct {
	Number      int64          `json:"number"`
	User        *apiUser       `json:"user"`
	Repository  *apiRepoMeta   `json:"repository"`
	PullRequest *apiPullMarker `json:"pull_request"`
}

// apiRepoMeta is the short repository of an issue.
type apiRepoMeta struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// apiPullMarker is set on issues that are pull requests.
type apiPullMarker struct {
	Merged bool `json:"merged"`
}

// apiEvent is an entry of an issue's timeline.
type apiEvent struct {
	Type string   `json:"type"`
	User *apiUser `json:"user"`
}

// check reports what the driver depends on that a pull request lacks.
func (p *apiPR) check(op string) error {
	switch {
	case p.Number <= 0:
		return shapeError(op, "a pull request without a number")
	case p.State != "open" && p.State != "closed":
		return shapeError(op, "pull request #%d has state %q", p.Number, p.State)
	case p.User == nil || p.Head == nil || p.Base == nil:
		return shapeError(op, "pull request #%d lacks user, head or base", p.Number)
	case p.Base.RepoID <= 0 || p.Base.Label == "" && p.Base.Ref == "":
		return shapeError(op, "pull request #%d lacks its base repository or branch", p.Number)
	case p.Created == nil:
		return shapeError(op, "pull request #%d has no created_at", p.Number)
	}
	return nil
}

// agit reports whether p is an AGit pull request: no head branch (Gitea
// and Forgejo 15 leave the label empty; Forgejo 16 names it user/topic and
// sets flow 1).
func (p *apiPR) agit() bool { return p.Head.Label == "" || p.Flow != nil && *p.Flow == flowAGit }

// head returns the head branch of p, "" for an AGit pull request.
func (p *apiPR) head() string {
	if p.agit() {
		return ""
	}
	return p.Head.Label
}

// base returns the base branch of p.
func (p *apiPR) base() string { return cmp.Or(p.Base.Label, p.Base.Ref) }

// toPR converts an API pull request; ClosedBy is set from merged_by only
// (closer adds the closer of a pull request closed without a merge).
func toPR(p *apiPR) platform.PR {
	pr := platform.PR{
		Number:  p.Number,
		URL:     p.HTMLURL,
		State:   platform.Open,
		Draft:   p.Draft,
		Head:    p.head(),
		HeadSHA: strings.ToLower(p.Head.SHA),
		Base:    p.base(),
		RepoID:  strconv.FormatInt(p.Base.RepoID, 10),
		// The base branch is gone when the server finds no commit for it.
		BaseExists: p.Base.SHA != "",
		Title:      p.Title,
		Body:       p.Body,
		Labels:     []string{},
		Author:     toAccount(p.User),
		CreatedAt:  p.Created.UTC(),
	}
	// A head repository that is gone (-1) or an AGit pull request has none:
	// never touchmark's own.
	if p.Head.RepoID > 0 && !p.agit() {
		pr.HeadRepoID = strconv.FormatInt(p.Head.RepoID, 10)
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	switch {
	case p.Merged:
		pr.State = platform.Merged
		if p.MergedBy != nil {
			by := toAccount(p.MergedBy)
			pr.ClosedBy = &by
		}
	case p.State == "closed":
		pr.State = platform.Closed
	}
	if pr.State != platform.Open {
		switch {
		case p.Closed != nil:
			pr.ClosedAt = p.Closed.UTC()
		case p.MergedAt != nil:
			pr.ClosedAt = p.MergedAt.UTC()
		}
	}
	return pr
}

// closerKey identifies one close of one pull request: a reopened and
// closed again pull request has a new closed_at.
type closerKey struct {
	repo   string
	number int64
	at     int64
}

// fillCloser fills in ClosedBy of a pull request closed without a merge (or
// merged without merged_by) from its timeline: the user of the last close
// (or merge) event since closed_at. Closers are remembered per close, so
// the repeated listings of a run read each timeline once. An error leaves
// pr unchanged: a closer that cannot be read is no reason to guess.
func (c *client) fillCloser(ctx context.Context, op, owner, name string, pr *platform.PR) error {
	if pr.State == platform.Open || pr.ClosedBy != nil || pr.ClosedAt.IsZero() {
		return nil
	}
	key := closerKey{repo: pr.RepoID, number: pr.Number, at: pr.ClosedAt.Unix()}
	c.mu.Lock()
	by, ok := c.closers[key]
	c.mu.Unlock()
	if ok {
		pr.ClosedBy = cloneAccount(by)
		return nil
	}
	want := "close"
	if pr.State == platform.Merged {
		want = "merge_pull"
	}
	// The close event is created with closed_at (issue_update.go of both
	// platforms); since filters on the events' update time, one second
	// earlier keeps a strict comparison safe.
	q := url.Values{"since": {pr.ClosedAt.Add(-time.Second).UTC().Format(time.RFC3339)}}
	var last *apiUser
	u := c.endpoint("repos", owner, name, "issues", strconv.FormatInt(pr.Number, 10), "timeline")
	// X-Total-Count of the timeline counts the page, not the timeline.
	_, err := listAll(ctx, c, op, u, q, listOpts{maxPages: maxTimelinePages}, func(e apiEvent) error {
		if e.Type == want && e.User != nil {
			last = e.User
		}
		return nil
	})
	if err != nil {
		return err
	}
	if last != nil {
		a := toAccount(last)
		by = &a
	}
	c.mu.Lock()
	c.closers[key] = by
	c.mu.Unlock()
	pr.ClosedBy = cloneAccount(by)
	return nil
}

func cloneAccount(a *platform.Account) *platform.Account {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

// PRs returns, newest first, the pull requests of r from heads by authors
// in every state and the open ones by anyone. A pull
// request's head is its head branch in whatever repository: pull requests
// from forks are included with their own HeadRepoID; AGit ones, which have
// no head branch, never are.
//
// Forgejo 16 lists by head branch. Elsewhere there is no head filter (the
// server ignores head=): the authors' pull requests are listed by poster,
// every open one in full, and the heads filtered here. Listings are complete or
// the call fails.
func (d *reader) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	const op = "list pull requests"
	owner, name, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, h := range heads {
		if h != "" {
			wanted[h] = true
		}
	}
	ids := accountIDs(authors)
	if len(wanted) == 0 {
		return []platform.PR{}, nil
	}
	found := map[int64]*apiPR{}
	collect := func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		if wanted[p.head()] && (p.State == "open" || ids[strconv.FormatInt(p.User.ID, 10)]) {
			found[p.Number] = &p
		}
		return nil
	}
	list := func(q url.Values) error { return d.c.listPulls(ctx, op, owner, name, q, collect) }
	inst, instErr := d.c.instance(ctx)
	if instErr == nil && inst.headFilter() {
		for _, h := range sortedKeys(wanted) {
			if err := list(url.Values{"state": {"all"}, "head": {h}}); err != nil {
				return nil, err
			}
		}
	} else {
		if err := d.byAuthors(ctx, op, owner, name, authors, list); err != nil {
			return nil, err
		}
		if err := list(url.Values{"state": {"open"}}); err != nil {
			return nil, err
		}
	}
	if instErr != nil || inst.flavor != "forgejo" {
		if err := d.c.checkBases(ctx, op, owner, name, found); err != nil {
			return nil, err
		}
	}
	out := make([]platform.PR, 0, len(found))
	for _, p := range found {
		pr := toPR(p)
		if err := d.c.fillCloser(ctx, op, owner, name, &pr); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	slices.SortFunc(out, func(a, b platform.PR) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

// checkBases asks the branch API whether the bases of prs still exist.
// Gitea's pull request lists take a base's commit from its branch table,
// where a deleted branch stays (ToAPIPullRequests in
// services/convert/pull.go, 1.26 and 1.27): a listed base.sha does not show
// the deletion that the single pull request's does. A base the branch API
// does not know gets an empty sha, as the server reports a missing base.
func (c *client) checkBases(ctx context.Context, op, owner, name string, prs map[int64]*apiPR) error {
	gone := map[string]bool{}
	numbers := make([]int64, 0, len(prs))
	for n := range prs {
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	for _, n := range numbers {
		p := prs[n]
		base := p.base()
		if p.Base.SHA == "" {
			continue
		}
		if _, known := gone[base]; !known {
			_, err := c.get(ctx, op, c.endpoint("repos", owner, name, "branches", base), nil, nil)
			switch {
			case err == nil:
				gone[base] = false
			case platform.ClassOf(err) == platform.ClassNotFound:
				gone[base] = true
			default:
				return err
			}
		}
		if gone[base] {
			p.Base.SHA = ""
		}
	}
	return nil
}

// byAuthors lists the pull requests of each author in every state by the
// poster filter (Gitea ≥ 1.23, Forgejo 15). A login the server does not
// know (400) falls back to the whole listing, which covers every author.
func (d *reader) byAuthors(ctx context.Context, op, owner, name string, authors []platform.Account, list func(url.Values) error) error {
	logins := map[string]bool{}
	for _, a := range authors {
		if a.ID == "" {
			continue
		}
		if a.Login == "" {
			return list(url.Values{"state": {"all"}})
		}
		logins[strings.ToLower(a.Login)] = true
	}
	for _, login := range sortedKeys(logins) {
		err := list(url.Values{"state": {"all"}, "poster": {login}})
		switch {
		case err == nil:
		case isStatus(err, http.StatusBadRequest) && !laterPage(err):
			return list(url.Values{"state": {"all"}})
		default:
			return err
		}
	}
	return nil
}

// listPulls reads every page of the pull requests of owner/name that q
// selects; a listing over maxPRPages pages fails.
func (c *client) listPulls(ctx context.Context, op, owner, name string, q url.Values, each func(apiPR) error) error {
	complete, err := listAll(ctx, c, op, c.endpoint("repos", owner, name, "pulls"), q,
		listOpts{maxPages: maxPRPages, trustTotal: true}, each)
	if err != nil {
		return err
	}
	if !complete {
		return &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%s/%s has more pull requests than %d pages; touchmark does not act on a partial list", owner, name, maxPRPages)}
	}
	return nil
}

// OpenPRsBy lists the open pull requests by authors from heads in every
// repository this identity sees, for the stale sweep.
//
// The issue search finds them: by created_by on Gitea (≥ 1.26); on
// Forgejo, which has no such filter, by created=true when every author is
// the credential's own account, else the whole search filtered here. Each
// hit is then read as a pull request (for its head branch and repository).
//
// A search that fails, on any page, with a rate limit, a refused
// credential, a transient failure or the end of ctx fails the call, so the
// core can pause or retry; any other failure of a search (an author's login
// the server does not know, a 403) makes the result incomplete, as does a
// search capped at maxSearchPages. A pull request that cannot be read makes
// the result incomplete, unless for a rate limit, the credential or the end
// of ctx, which fail the call; one gone since the search is left out.
func (d *reader) OpenPRsBy(ctx context.Context, authors []platform.Account, heads []string) (platform.Swept, error) {
	const op = "list open pull requests"
	ids := accountIDs(authors)
	wanted := map[string]bool{}
	for _, h := range heads {
		if h != "" {
			wanted[h] = true
		}
	}
	out := platform.Swept{PRs: []platform.RepoPR{}, Complete: true}
	if len(ids) == 0 || len(wanted) == 0 {
		return out, nil
	}
	inst, err := d.c.instance(ctx)
	if err != nil {
		if fatal(err) {
			return platform.Swept{}, err
		}
		// An instance that could not be told: searched as Forgejo is (the
		// whole search, filtered here), its repositories never public.
		inst = instance{signIn: true}
	}
	queries, err := d.sweepQueries(ctx, inst, authors, ids)
	if err != nil {
		return platform.Swept{}, err
	}
	type hit struct {
		repo   apiRepoMeta
		number int64
	}
	hits := map[string]hit{}
	for _, q := range queries {
		q.Set("type", "pulls")
		q.Set("state", "open")
		complete, err := listAll(ctx, d.c, op, d.c.endpoint("repos", "issues", "search"), q,
			listOpts{maxPages: maxSearchPages, trustTotal: true}, func(is apiIssue) error {
				if is.PullRequest == nil || is.User == nil || is.Repository == nil || is.Number <= 0 {
					return nil
				}
				if !ids[strconv.FormatInt(is.User.ID, 10)] {
					return nil
				}
				key := strconv.FormatInt(is.Repository.ID, 10) + "#" + strconv.FormatInt(is.Number, 10)
				hits[key] = hit{repo: *is.Repository, number: is.Number}
				return nil
			})
		switch {
		case err == nil:
			out.Complete = out.Complete && complete
		case fatal(err):
			return platform.Swept{}, err
		default:
			// An unknown created_by (404 on Gitea), a failed later page.
			out.Complete = false
		}
	}
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		h := hits[k]
		owner, name, ok := splitRepoPath(h.repo.FullName)
		if !ok {
			out.Complete = false
			continue
		}
		// The single pull request, unlike a list, also tells a deleted base.
		p, err := d.c.getPR(ctx, op, owner, name, h.number)
		if err == nil && (p.Base.Repo == nil || p.Base.Repo.check(op) != nil) {
			err = shapeError(op, "pull request #%d of %s lacks its base repository", h.number, h.repo.FullName)
		}
		switch {
		case err == nil:
		case platform.ClassOf(err) == platform.ClassNotFound:
			continue // gone since the search
		case stops(err):
			return platform.Swept{}, err
		default:
			out.Complete = false
			continue
		}
		if p.State != "open" || !wanted[p.head()] || !ids[strconv.FormatInt(p.User.ID, 10)] {
			continue
		}
		out.PRs = append(out.PRs, platform.RepoPR{Repo: d.c.toRepo(p.Base.Repo, inst.signIn), PR: toPR(p)})
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

// sweepQueries returns the issue searches that cover the open pull
// requests of the authors on inst: one per author by created_by on Gitea;
// on Forgejo created=true when the authors are the credential's own
// account, else one search of everything (filtered by the caller).
func (d *reader) sweepQueries(ctx context.Context, inst instance, authors []platform.Account, ids map[string]bool) ([]url.Values, error) {
	if inst.flavor == "gitea" {
		var qs []url.Values
		logins := map[string]bool{}
		for _, a := range authors {
			if a.ID == "" {
				continue
			}
			if a.Login == "" {
				return []url.Values{{}}, nil
			}
			logins[strings.ToLower(a.Login)] = true
		}
		for _, l := range sortedKeys(logins) {
			qs = append(qs, url.Values{"created_by": {l}})
		}
		return qs, nil
	}
	if d.c.token != "" && len(ids) == 1 {
		self, err := d.c.selfAccount(ctx)
		if err != nil {
			return nil, err
		}
		if ids[self.ID] {
			return []url.Values{{"created": {"true"}}}, nil
		}
	}
	return []url.Values{{}}, nil
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

// getPR reads one pull request of owner/name.
func (c *client) getPR(ctx context.Context, op, owner, name string, number int64) (*apiPR, error) {
	var p apiPR
	if _, err := c.get(ctx, op, c.endpoint("repos", owner, name, "pulls", strconv.FormatInt(number, 10)), nil, &p); err != nil {
		return nil, err
	}
	if err := p.check(op); err != nil {
		return nil, err
	}
	return &p, nil
}
