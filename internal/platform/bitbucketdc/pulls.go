package bitbucketdc

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

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of listings.
const (
	// maxPRPages bounds the pull requests of one branch of one repository.
	// PRs has no way to say it saw only part: past the bound it fails.
	maxPRPages = 10
	// maxActivityPages bounds the activities read to find who declined a
	// pull request: 2 000 at 500 a page. Past the bound the closer stays
	// unknown.
	maxActivityPages = 4
	// maxSweepPages bounds the open pull requests of one author.
	maxSweepPages = 20
)

// Pull request states (RestPullRequest.state). A declined pull request can
// be reopened; a merged one cannot.
const (
	stateOpen     = "OPEN"
	stateDeclined = "DECLINED"
	stateMerged   = "MERGED"
)

// systemID is the id ClosedBy carries for a decline whose activity names
// no user: Bitbucket's auto-decline of inactive pull requests is by "the
// Bitbucket system user", whom the reference does not describe. A user's
// id is a number, so it never meets one.
const systemID = "system"

// apiPR is a pull request as the API reports it (RestPullRequest), in
// lists and alone. A description that is empty is absent.
type apiPR struct {
	ID          int64           `json:"id"`
	Version     *int64          `json:"version"`
	Title       string          `json:"title"`
	Description *string         `json:"description"`
	State       string          `json:"state"`
	Draft       bool            `json:"draft"`
	FromRef     apiRef          `json:"fromRef"`
	ToRef       apiRef          `json:"toRef"`
	Author      *apiParticipant `json:"author"`
	// Reviewers are sent back by EditPR: whether an update without them
	// keeps them is not documented.
	Reviewers   []apiParticipant `json:"reviewers"`
	CreatedDate *int64           `json:"createdDate"`
	UpdatedDate *int64           `json:"updatedDate"`
	ClosedDate  *int64           `json:"closedDate"`
}

// apiRef is the source or target of a pull request (RestPullRequestRef):
// its full ref, its head commit and its repository.
type apiRef struct {
	ID           string      `json:"id"`
	DisplayID    string      `json:"displayId"`
	LatestCommit string      `json:"latestCommit"`
	Repository   *apiRepoRef `json:"repository"`
}

// apiParticipant is a pull request's author or reviewer.
type apiParticipant struct {
	User *apiUser `json:"user"`
}

// apiActivity is one activity of a pull request (RestPullRequestActivity).
type apiActivity struct {
	ID          int64    `json:"id"`
	CreatedDate int64    `json:"createdDate"`
	Action      string   `json:"action"`
	User        *apiUser `json:"user"`
}

// check reports what the driver depends on that a pull request lacks.
func (p *apiPR) check(op string) error {
	switch {
	case p.ID <= 0:
		return shapeError(op, "a pull request without an id")
	case p.State != stateOpen && p.State != stateDeclined && p.State != stateMerged:
		return shapeError(op, "pull request #%d has state %q", p.ID, p.State)
	case p.ToRef.Repository == nil || p.ToRef.Repository.ID <= 0 || p.ToRef.Repository.path() == "":
		return shapeError(op, "pull request #%d lacks its target repository", p.ID)
	case p.base() == "":
		return shapeError(op, "pull request #%d targets %q, no branch", p.ID, p.ToRef.ID)
	case p.CreatedDate == nil:
		return shapeError(op, "pull request #%d has no createdDate", p.ID)
	case p.Version == nil:
		return shapeError(op, "pull request #%d has no version", p.ID)
	}
	return nil
}

// branchOf returns the branch a full ref names, "" for another ref.
func branchOf(ref string) string {
	b, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return ""
	}
	return b
}

// head returns the source branch of p, "" when it is no branch (a tag).
func (p *apiPR) head() string { return branchOf(p.FromRef.ID) }

// base returns the target branch of p.
func (p *apiPR) base() string { return branchOf(p.ToRef.ID) }

// authorID returns the id of p's author, "" when there is none.
func (p *apiPR) authorID() string {
	if p.Author == nil || p.Author.User == nil || p.Author.User.ID <= 0 {
		return ""
	}
	return strconv.FormatInt(p.Author.User.ID, 10)
}

// repoID returns the id of the repository of ref, "" when it names none.
func (r apiRef) repoID() string {
	if r.Repository == nil || r.Repository.ID <= 0 {
		return ""
	}
	return strconv.FormatInt(r.Repository.ID, 10)
}

// millis converts epoch milliseconds to a time in UTC.
func millis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// toPR converts an API pull request; BaseExists, HeadSHA and ClosedBy are
// left to the caller. Declined pull requests are closed, merged ones
// merged; ClosedAt is closedDate (updatedDate when absent).
func (c *client) toPR(p *apiPR) platform.PR {
	body := ""
	if p.Description != nil {
		body = *p.Description
	}
	var author platform.Account
	if p.Author != nil {
		author = toAccount(p.Author.User)
	}
	pr := platform.PR{
		Number:    p.ID,
		URL:       c.repoWeb(p.ToRef.Repository.Project.Key, p.ToRef.Repository.Slug) + "/pull-requests/" + strconv.FormatInt(p.ID, 10),
		State:     platform.Open,
		Draft:     p.Draft,
		Head:      p.head(),
		Base:      p.base(),
		RepoID:    p.ToRef.repoID(),
		Title:     p.Title,
		Body:      body,
		Labels:    []string{},
		Author:    author,
		CreatedAt: millis(*p.CreatedDate),
	}
	if pr.Head != "" {
		pr.HeadRepoID = p.FromRef.repoID()
	}
	switch p.State {
	case stateMerged:
		pr.State = platform.Merged
	case stateDeclined:
		pr.State = platform.Closed
	}
	if pr.State != platform.Open {
		switch {
		case p.ClosedDate != nil:
			pr.ClosedAt = millis(*p.ClosedDate)
		case p.UpdatedDate != nil:
			pr.ClosedAt = millis(*p.UpdatedDate)
		}
	}
	return pr
}

// PRs returns, newest first, the pull requests into r from heads by authors
// in every state and the open ones by anyone: for each head, GET
// …/pull-requests?state=ALL&direction=OUTGOING&at=refs/heads/<head>, the
// pull requests from that branch of r, filtered again here (the reference
// does not spell out what at means with direction). Pull requests from a
// branch of the same name in a fork are not listed: the core never takes
// them for its own. The listing is complete or the call fails.
//
// BaseExists reads the branch API for a base other than r's default branch
// (once per base). HeadSHA is the source commit of an open pull request.
// ClosedBy of a declined pull request is who declined it, from its
// activities (see closer).
func (d *reader) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	const op = "list pull requests"
	key, slug, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	wanted := setOf(heads)
	if len(wanted) == 0 {
		return []platform.PR{}, nil
	}
	ids := accountIDs(authors)
	var found []apiPR
	seen := map[int64]bool{}
	for _, head := range sortedKeys(wanted) {
		q := url.Values{
			"state":          {"ALL"},
			"direction":      {"OUTGOING"},
			"at":             {"refs/heads/" + head},
			"order":          {"NEWEST"},
			"withAttributes": {"false"},
			"withProperties": {"false"},
		}
		complete, err := listAll(ctx, d.c, op, d.c.repoURL(key, slug, "pull-requests"), q, pageLimit, maxPRPages, func(p apiPR) error {
			if err := p.check(op); err != nil {
				return err
			}
			into := r.ID == "" || p.ToRef.repoID() == r.ID
			from := r.ID == "" || p.FromRef.repoID() == r.ID
			if !seen[p.ID] && into && from && p.head() == head && (p.State == stateOpen || ids[p.authorID()]) {
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
				Err: fmt.Errorf("%s has more pull requests from %s than %d pages; touchmark does not act on a partial list", r.Path, head, maxPRPages)}
		}
	}
	out := make([]platform.PR, 0, len(found))
	bases := map[string]bool{}
	for i := range found {
		p := &found[i]
		pr := d.c.toPR(p)
		if pr.BaseExists, err = d.c.baseExists(ctx, op, r, key, slug, pr.Base, bases); err != nil {
			return nil, err
		}
		if pr.State == platform.Open && isHexOID(p.FromRef.LatestCommit) {
			pr.HeadSHA = strings.ToLower(p.FromRef.LatestCommit)
		}
		if pr.State == platform.Closed {
			if pr.ClosedBy, err = d.c.closer(ctx, op, key, slug, p.ID); err != nil {
				return nil, err
			}
		}
		out = append(out, pr)
	}
	slices.SortFunc(out, func(a, b platform.PR) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

// closer returns who declined pull request id of key/slug: the user of its
// latest DECLINED activity (GET …/pull-requests/{id}/activities, read whole
// up to maxActivityPages: the reference gives no order), nil when none is
// found or the activities run past the bound. A person is a NORMAL user, a
// service user a bot (toAccount); an activity without a user is taken for
// Bitbucket's system user, a bot (systemID). A pull request gone since the
// listing has no closer.
func (c *client) closer(ctx context.Context, op, key, slug string, id int64) (*platform.Account, error) {
	var last *apiActivity
	complete, err := listAll(ctx, c, op, c.repoURL(key, slug, "pull-requests", strconv.FormatInt(id, 10), "activities"), nil, activityLimit, maxActivityPages, func(a apiActivity) error {
		if a.Action == stateDeclined && (last == nil || a.CreatedDate > last.CreatedDate || a.CreatedDate == last.CreatedDate && a.ID > last.ID) {
			last = &a
		}
		return nil
	})
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound && !laterPage(err):
		return nil, nil
	case err != nil:
		return nil, err
	case !complete || last == nil:
		return nil, nil
	case last.User == nil:
		return &platform.Account{ID: systemID, Login: systemID, Kind: platform.KindBot}, nil
	}
	a := toAccount(last.User)
	if a.ID == "" {
		return nil, nil
	}
	return &a, nil
}

// baseExists reports whether branch, a pull request's base in r, exists:
// r's default branch does; another is looked up once per call (known holds
// the answers). A pull request whose base was deleted keeps its ref.
func (c *client) baseExists(ctx context.Context, op string, r platform.Repo, key, slug, branch string, known map[string]bool) (bool, error) {
	if branch == r.DefaultBranch {
		return true, nil
	}
	if v, ok := known[branch]; ok {
		return v, nil
	}
	_, ok, err := c.branch(ctx, op, key, slug, branch)
	if err != nil {
		return false, err
	}
	known[branch] = ok
	return ok, nil
}

// OpenPRsBy lists the open pull requests by authors from heads in every
// repository this identity sees, for the stale sweep: for each author, GET
// /dashboard/pull-requests?state=OPEN&role=AUTHOR&user=<name>, the open
// pull requests the author made, filtered by source branch here. The
// dashboard takes a user's name, which Self and Lookup learn: an author
// neither resolved makes the result incomplete. Each repository is read
// once, for the report and the default branch. HeadSHA stays "": the sweep
// closes pull requests and reads no heads.
//
// A listing that fails, on any page, with a rate limit, a refused
// credential, a transient failure or the end of ctx fails the call, so the
// core can pause or retry; any other failure of a listing (a dashboard
// that refuses another user's name) makes the result incomplete, as does a
// listing capped at its bound. A repository gone since the listing is left
// out; one that cannot be read otherwise makes the result incomplete.
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
	hits := map[string]apiPR{}
	for _, id := range sortedKeys(ids) {
		name := d.c.nameOf(id)
		if name == "" {
			out.Complete = false
			continue
		}
		q := url.Values{"state": {stateOpen}, "role": {"AUTHOR"}, "user": {name}, "order": {"NEWEST"}}
		complete, err := listAll(ctx, d.c, op, d.c.endpoint("dashboard", "pull-requests"), q, pageLimit, maxSweepPages, func(p apiPR) error {
			if err := p.check(op); err != nil {
				return err
			}
			if p.State == stateOpen && wanted[p.head()] && p.authorID() == id {
				hits[p.ToRef.repoID()+"#"+strconv.FormatInt(p.ID, 10)] = p
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
	keys := make([]string, 0, len(hits))
	for k := range hits {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	type known struct {
		repo  *platform.Repo
		bases map[string]bool
	}
	repos := map[string]*known{}
	for _, k := range keys {
		p := hits[k]
		ref := p.ToRef.Repository
		rid := p.ToRef.repoID()
		kr, ok := repos[rid]
		if !ok {
			kr = &known{bases: map[string]bool{}}
			r, err := d.repoByRef(ctx, op, ref)
			switch {
			case err == nil:
				kr.repo = &r
			case platform.ClassOf(err) == platform.ClassNotFound:
			case stops(err):
				return platform.Swept{}, err
			default:
				out.Complete = false
			}
			repos[rid] = kr
		}
		if kr.repo == nil {
			continue
		}
		key, slug, _ := splitRepoPath(kr.repo.Path)
		pr := d.c.toPR(&p)
		var err error
		pr.BaseExists, err = d.c.baseExists(ctx, op, *kr.repo, key, slug, pr.Base, kr.bases)
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
		out.PRs = append(out.PRs, platform.RepoPR{Repo: *kr.repo, PR: pr})
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

// repoByRef reads the repository a pull request names, with its default
// branch; one that answers with another id is ClassUnknown.
func (d *reader) repoByRef(ctx context.Context, op string, ref *apiRepoRef) (platform.Repo, error) {
	r, err := d.c.getRepo(ctx, op, ref.Project.Key, ref.Slug)
	if err != nil {
		return platform.Repo{}, err
	}
	if r.ID != ref.ID {
		return platform.Repo{}, shapeError(op, "repository %s answers with id %d, not %d", ref.path(), r.ID, ref.ID)
	}
	def, err := d.c.defaultBranchOf(ctx, op, r.Project.Key, r.Slug)
	if err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(r, def), nil
}

// errAnonymousSweep is why an anonymous reader cannot sweep: only a user
// has a dashboard.
var errAnonymousSweep = errors.New("no credential: the reader is anonymous, and only a user has pull requests to sweep")

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
