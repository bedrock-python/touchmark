package azuredevops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of listings and reads.
const (
	// maxPRPages bounds a listing of one head's pull requests in one
	// repository. PRs has no way to say it saw only part: past the bound it
	// fails.
	maxPRPages = 100
	// maxSweepPages bounds the active pull requests of one author in one
	// project.
	maxSweepPages = 100
	// maxClosedReads bounds, per call and per status, the pull requests
	// that are not active whose marker property is read (one GET each),
	// newest first, and the abandoned ones whose closer is read (one GET
	// each, the same ones): memory looks at the 50 newest closed pull
	// requests of touchmark's (decide.MemoryConfig.Window, which hub.yml
	// does not change), and the core reads no completed one. An older one
	// reports neither a marker nor a closer: a closed pull request without
	// a marker is nothing to the core.
	maxClosedReads = 60
	// listedDescription is how long a description a listing may show in
	// full: listings cut descriptions to 400 characters (assumed from the
	// observed listing; the REST reference says nothing), so a longer one,
	// counted in bytes, which are never fewer, is read alone.
	listedDescription = 390
)

// Pull request statuses (PullRequestStatus).
const (
	statusActive    = "active"
	statusAbandoned = "abandoned"
	statusCompleted = "completed"
)

// apiPR is a pull request as the API reports it (GitPullRequest), in
// listings and alone.
type apiPR struct {
	ID            int64           `json:"pullRequestId"`
	Status        string          `json:"status"`
	IsDraft       bool            `json:"isDraft"`
	Title         string          `json:"title"`
	Description   *string         `json:"description"`
	SourceRefName string          `json:"sourceRefName"`
	TargetRefName string          `json:"targetRefName"`
	CreatedBy     *apiIdentityRef `json:"createdBy"`
	ClosedBy      *apiIdentityRef `json:"closedBy"`
	CreationDate  *time.Time      `json:"creationDate"`
	ClosedDate    *time.Time      `json:"closedDate"`
	Repository    *apiRepo        `json:"repository"`
	ForkSource    *struct {
		Repository *apiRepo `json:"repository"`
	} `json:"forkSource"`
	LastMergeSourceCommit *struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	Labels []apiLabel `json:"labels"`
}

// apiLabel is a pull request's label (WebApiTagDefinition).
type apiLabel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active *bool  `json:"active"`
}

// apiRef is a ref of the Refs API (GitRef).
type apiRef struct {
	Name     string `json:"name"`
	ObjectID string `json:"objectId"`
}

// check reports what the driver depends on that a pull request lacks.
func (p *apiPR) check(op string) error {
	switch {
	case p.ID <= 0:
		return shapeError(op, "a pull request without an id")
	case p.Status != statusActive && p.Status != statusAbandoned && p.Status != statusCompleted:
		return shapeError(op, "pull request %d has status %q", p.ID, p.Status)
	case p.Repository == nil || !guidRe.MatchString(p.Repository.ID):
		return shapeError(op, "pull request %d lacks its repository", p.ID)
	case !strings.HasPrefix(p.TargetRefName, "refs/heads/"):
		return shapeError(op, "pull request %d has target %q", p.ID, p.TargetRefName)
	case p.CreationDate == nil:
		return shapeError(op, "pull request %d has no creationDate", p.ID)
	}
	return nil
}

// head returns the source branch of p, "" when it is no branch.
func (p *apiPR) head() string {
	h, ok := strings.CutPrefix(p.SourceRefName, "refs/heads/")
	if !ok {
		return ""
	}
	return h
}

// authorID returns the id of p's author, lowercased, "" for none.
func (p *apiPR) authorID() string { return refAccount(p.CreatedBy).ID }

// headRepo returns the id of the repository p's head lives in: its fork's,
// or its own.
func (p *apiPR) headRepo() string {
	if p.ForkSource != nil {
		if p.ForkSource.Repository == nil || !guidRe.MatchString(p.ForkSource.Repository.ID) {
			return ""
		}
		return strings.ToLower(p.ForkSource.Repository.ID)
	}
	return strings.ToLower(p.Repository.ID)
}

// cut reports whether the description p shows may be cut by a listing.
func (p *apiPR) cut() bool { return p.Description != nil && len(*p.Description) >= listedDescription }

// toPR converts an API pull request, its stored marker line appended to
// its description (marker.Attach); BaseExists and HeadSHA are left to the
// caller. Abandoned pull requests are closed, completed ones merged.
func (c *client) toPR(p *apiPR, markerLine string) platform.PR {
	desc := ""
	if p.Description != nil {
		desc = *p.Description
	}
	pr := platform.PR{
		Number:     p.ID,
		State:      platform.Open,
		Draft:      p.IsDraft,
		Head:       p.head(),
		Base:       strings.TrimPrefix(p.TargetRefName, "refs/heads/"),
		RepoID:     strings.ToLower(p.Repository.ID),
		HeadRepoID: p.headRepo(),
		Title:      p.Title,
		Body:       marker.Attach(desc, markerLine),
		Labels:     []string{},
		Author:     refAccount(p.CreatedBy),
		CreatedAt:  p.CreationDate.UTC(),
	}
	if pr.Head == "" {
		pr.HeadRepoID = ""
	}
	web := p.Repository.WebURL
	if web == "" && p.Repository.Project != nil && p.Repository.Project.Name != "" && p.Repository.Name != "" {
		web = c.web + "/" + url.PathEscape(p.Repository.Project.Name) + "/_git/" + url.PathEscape(p.Repository.Name)
	}
	if web != "" {
		pr.URL = web + "/pullrequest/" + strconv.FormatInt(p.ID, 10)
	}
	for _, l := range p.Labels {
		if l.Name != "" && (l.Active == nil || *l.Active) {
			pr.Labels = append(pr.Labels, l.Name)
		}
	}
	switch p.Status {
	case statusCompleted:
		pr.State = platform.Merged
	case statusAbandoned:
		pr.State = platform.Closed
	}
	if pr.State != platform.Open {
		if by := refAccount(p.ClosedBy); by.ID != "" {
			pr.ClosedBy = &by
		}
		if p.ClosedDate != nil {
			pr.ClosedAt = p.ClosedDate.UTC()
		}
	}
	return pr
}

// PRs returns, newest first, the pull requests of r from heads by authors
// in every status and the active ones by anyone: per head, GET
// {org}/_apis/git/repositories/{id}/pullrequests?searchCriteria.
// sourceRefName=refs/heads/<head>&searchCriteria.status=all, pages of 100,
// filtered again here. Those from forks (forkSource) come with their own
// HeadRepoID. The listing is complete or the call fails.
//
// Each pull request kept costs reads of its own: its properties, for the
// marker (Pull Request Properties - List; one request each), for every
// active one and for the maxClosedReads newest abandoned and completed
// ones each; the pull request alone when the listing may have cut its
// description, and for an abandoned one, to learn its closer (listings
// leave closedBy out; the same maxClosedReads newest). An older pull
// request that is not active comes without its marker. BaseExists reads
// the Refs API for a base other than r's default branch (once per base);
// HeadSHA, for an active pull request from r itself, is its source
// branch's head (Refs API: lastMergeSourceCommit lags behind pushes until
// the merge is computed again), "" when the branch is gone.
func (d *reader) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	const op = "list pull requests"
	if !guidRe.MatchString(r.ID) {
		return nil, invalid(op, "repository %s has no id", r.Path)
	}
	wanted := setOf(heads)
	if len(wanted) == 0 {
		return []platform.PR{}, nil
	}
	ids := accountIDs(authors)
	var found []apiPR
	seen := map[int64]bool{}
	for _, h := range sortedKeys(wanted) {
		q := url.Values{
			"searchCriteria.sourceRefName": {"refs/heads/" + h},
			"searchCriteria.status":        {"all"},
		}
		complete, err := listPages(ctx, d.c, op, d.c.repoAPI(r.ID, "pullrequests"), q, maxPRPages, func(p apiPR) error {
			if err := p.check(op); err != nil {
				return err
			}
			if !seen[p.ID] && wanted[p.head()] && strings.EqualFold(p.Repository.ID, r.ID) && (p.Status == statusActive || ids[p.authorID()]) {
				seen[p.ID] = true
				found = append(found, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !complete {
			return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
				Err: fmt.Errorf("%s has more pull requests from %s than %d pages; touchmark does not act on a partial list", r.Path, h, maxPRPages)}
		}
	}
	slices.SortFunc(found, func(a, b apiPR) int { return cmp.Compare(b.ID, a.ID) })
	out := make([]platform.PR, 0, len(found))
	bases := map[string]bool{}
	closed := map[string]int{}
	for i := range found {
		p := &found[i]
		alone := p.cut()
		props := true
		if p.Status != statusActive {
			props = closed[p.Status] < maxClosedReads
			closed[p.Status]++
			if props && p.Status == statusAbandoned {
				alone = true
			}
		}
		pr, err := d.c.complete(ctx, op, r, p, alone, props, bases)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, nil
}

// complete reads what a listed pull request p of r lacks (the pull request
// alone when alone is set, its marker property when props is set, its
// base, its head) and converts it. known holds the bases found so far.
func (c *client) complete(ctx context.Context, op string, r platform.Repo, p *apiPR, alone, props bool, known map[string]bool) (platform.PR, error) {
	if alone {
		full, err := c.getPR(ctx, op, r.ID, p.ID)
		if err != nil {
			return platform.PR{}, err
		}
		*p = *full
	}
	line := ""
	if props {
		var err error
		if line, err = c.markerOf(ctx, op, r.ID, p.ID); err != nil {
			return platform.PR{}, err
		}
	}
	var err error
	pr := c.toPR(p, line)
	if pr.BaseExists, err = c.baseExists(ctx, op, r, pr.Base, known); err != nil {
		return platform.PR{}, err
	}
	if pr.State == platform.Open && pr.Head != "" && pr.HeadRepoID == pr.RepoID {
		if pr.HeadSHA, err = c.branchHead(ctx, op, r.ID, pr.Head); err != nil {
			return platform.PR{}, err
		}
	}
	return pr, nil
}

// getPR reads pull request number of the repository with id alone: GET
// {org}/_apis/git/repositories/{id}/pullrequests/{number}.
func (c *client) getPR(ctx context.Context, op, repoID string, number int64) (*apiPR, error) {
	var p apiPR
	if _, err := c.get(ctx, op, c.repoAPI(repoID, "pullrequests", strconv.FormatInt(number, 10)), nil, &p); err != nil {
		return nil, err
	}
	if err := p.check(op); err != nil {
		return nil, err
	}
	if p.ID != number {
		return nil, shapeError(op, "GET pull request %d answers %d", number, p.ID)
	}
	return &p, nil
}

// markerOf returns the marker line stored in pull request number's
// properties (touchmark.marker), "" when it has none: GET
// {org}/_apis/git/repositories/{id}/pullRequests/{number}/properties.
func (c *client) markerOf(ctx context.Context, op, repoID string, number int64) (string, error) {
	var props struct {
		Value map[string]apiProperty `json:"value"`
	}
	if _, err := c.get(ctx, op, c.repoAPI(repoID, "pullRequests", strconv.FormatInt(number, 10), "properties"), nil, &props); err != nil {
		return "", err
	}
	line := props.Value[markerProperty].text()
	if strings.ContainsAny(line, "\r\n") || !marker.IsLine(line) {
		// Not a marker line, or one with more in it: no marker the core
		// may read (it never wrote such a value).
		return "", nil
	}
	return line, nil
}

// baseExists reports whether branch, a pull request's base in r, exists:
// r's default branch does; another is looked up once per call (known holds
// the answers). A pull request whose base was deleted keeps its name.
func (c *client) baseExists(ctx context.Context, op string, r platform.Repo, branch string, known map[string]bool) (bool, error) {
	if branch == r.DefaultBranch {
		return true, nil
	}
	if v, ok := known[branch]; ok {
		return v, nil
	}
	head, err := c.branchHead(ctx, op, r.ID, branch)
	if err != nil {
		return false, err
	}
	known[branch] = head != ""
	return known[branch], nil
}

// branchHead returns the head commit of branch in the repository with id,
// "" when the branch does not exist: GET …/refs?filter=heads/<branch>,
// whose filter is a prefix, so the exact name is looked for in the answer.
func (c *client) branchHead(ctx context.Context, op, repoID, branch string) (string, error) {
	var refs list[apiRef]
	q := url.Values{"filter": {"heads/" + branch}}
	if _, err := c.get(ctx, op, c.repoAPI(repoID, "refs"), q, &refs); err != nil {
		return "", err
	}
	for _, ref := range refs.Value {
		if ref.Name == "refs/heads/"+branch {
			if !isHexOID(ref.ObjectID) {
				return "", shapeError(op, "branch %s has no head", branch)
			}
			return strings.ToLower(ref.ObjectID), nil
		}
	}
	return "", nil
}

// OpenPRsBy lists the active pull requests by authors from heads in every
// repository this identity sees, for the stale sweep: the organization's
// repositories are listed once (for their projects and for the report),
// then, per project and author, GET {org}/{project}/_apis/git/pullrequests?
// searchCriteria.creatorId=<id>&searchCriteria.status=active, filtered by
// head here. Each pull request found reads its properties (the marker) and,
// when its description may be cut, itself. HeadSHA stays "": the sweep
// closes pull requests and reads no heads.
//
// A failure with a rate limit, a refused credential, a transient failure or
// the end of ctx fails the call; any other failure of a project's listing
// makes the result incomplete, as does a listing capped at its bound. A
// pull request gone since the listing is left out; one that cannot be read
// otherwise makes the result incomplete.
func (d *reader) OpenPRsBy(ctx context.Context, authors []platform.Account, heads []string) (platform.Swept, error) {
	const op = "list open pull requests"
	ids := accountIDs(authors)
	wanted := setOf(heads)
	out := platform.Swept{PRs: []platform.RepoPR{}, Complete: true}
	if len(ids) == 0 || len(wanted) == 0 {
		return out, nil
	}
	if d.c.token == "" {
		return platform.Swept{}, &platform.Error{Op: op, Class: platform.ClassAuth, Err: errAnonymousSweep}
	}
	all, err := d.c.listRepos(ctx, op)
	if err != nil {
		return platform.Swept{}, err
	}
	repos := map[string]*apiRepo{}
	var projects []string
	seenProject := map[string]bool{}
	for i := range all {
		r := &all[i]
		repos[strings.ToLower(r.ID)] = r
		if p := strings.ToLower(r.Project.ID); !seenProject[p] {
			seenProject[p] = true
			projects = append(projects, p)
		}
	}
	slices.Sort(projects)
	type hit struct{ pr apiPR }
	hits := map[string]hit{}
	for _, project := range projects {
		for _, id := range sortedKeys(ids) {
			q := url.Values{"searchCriteria.creatorId": {id}, "searchCriteria.status": {statusActive}}
			complete, err := listPages(ctx, d.c, op, endpoint(d.c.api, project, "_apis", "git", "pullrequests"), q, maxSweepPages, func(p apiPR) error {
				if err := p.check(op); err != nil {
					return err
				}
				if p.Status == statusActive && wanted[p.head()] && ids[p.authorID()] {
					hits[strings.ToLower(p.Repository.ID)+"#"+strconv.FormatInt(p.ID, 10)] = hit{pr: p}
				}
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
	bases := map[string]map[string]bool{}
	for _, k := range keys {
		p := hits[k].pr
		id := strings.ToLower(p.Repository.ID)
		repo := repos[id]
		if repo == nil {
			// Created after the listing of repositories, or not visible as
			// a repository: read it.
			got, err := d.c.getRepo(ctx, op, id)
			switch {
			case err == nil:
				repo = got
			case platform.ClassOf(err) == platform.ClassNotFound:
				continue
			case stops(err):
				return platform.Swept{}, err
			default:
				out.Complete = false
				continue
			}
			repos[id] = repo
		}
		r := d.c.toRepo(repo)
		if bases[id] == nil {
			bases[id] = map[string]bool{}
		}
		pr, err := d.c.sweptPR(ctx, op, r, &p, bases[id])
		switch {
		case err == nil:
		case platform.ClassOf(err) == platform.ClassNotFound:
			continue // gone since the listing
		case stops(err):
			return platform.Swept{}, err
		default:
			out.Complete = false
			continue
		}
		out.PRs = append(out.PRs, platform.RepoPR{Repo: r, PR: pr})
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

// sweptPR completes a pull request the sweep found, without its head.
func (c *client) sweptPR(ctx context.Context, op string, r platform.Repo, p *apiPR, known map[string]bool) (platform.PR, error) {
	if p.cut() {
		full, err := c.getPR(ctx, op, r.ID, p.ID)
		if err != nil {
			return platform.PR{}, err
		}
		*p = *full
	}
	line, err := c.markerOf(ctx, op, r.ID, p.ID)
	if err != nil {
		return platform.PR{}, err
	}
	pr := c.toPR(p, line)
	pr.BaseExists, err = c.baseExists(ctx, op, r, pr.Base, known)
	return pr, err
}

// errAnonymousSweep is why an anonymous reader cannot sweep: it has no
// pull requests of its own.
var errAnonymousSweep = errors.New("no credential: the reader is anonymous, and only an identity has pull requests to sweep")

// accountIDs returns the set of the accounts' ids, lowercased.
func accountIDs(accounts []platform.Account) map[string]bool {
	ids := map[string]bool{}
	for _, a := range accounts {
		if guidRe.MatchString(a.ID) {
			ids[strings.ToLower(a.ID)] = true
		}
	}
	return ids
}

// setOf returns the set of the non-empty strings of list.
func setOf(list []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range list {
		if s != "" {
			out[s] = true
		}
	}
	return out
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
