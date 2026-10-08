package bitbucket

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
	// maxPRPages bounds a listing of one repository's pull requests. PRs
	// has no way to say it saw only part: past the bound it fails.
	maxPRPages = 200
	// maxSweepPages bounds the open pull requests of one author in one
	// workspace.
	maxSweepPages = 200
	// maxWorkspacePages bounds the workspaces of the identity.
	maxWorkspacePages = 50
)

// Pull request states (the state enum of the OpenAPI description). A
// declined pull request cannot be reopened; a superseded one was replaced
// by another and is closed too.
const (
	stateOpen       = "OPEN"
	stateMerged     = "MERGED"
	stateDeclined   = "DECLINED"
	stateSuperseded = "SUPERSEDED"
)

// allStates are every state, which a listing with q= must name itself: the
// state parameter is ignored next to q.
var allStates = []string{stateOpen, stateMerged, stateDeclined, stateSuperseded}

// apiPR is a pull request as the API reports it, in lists and alone.
// summary.raw is the description as typed; description is the same text
// (sent by the API, not in the OpenAPI description).
type apiPR struct {
	ID          int64         `json:"id"`
	Title       string        `json:"title"`
	State       string        `json:"state"`
	Draft       bool          `json:"draft"`
	Summary     *apiText      `json:"summary"`
	Description *string       `json:"description"`
	Author      *apiAccount   `json:"author"`
	ClosedBy    *apiAccount   `json:"closed_by"`
	Source      apiPREndpoint `json:"source"`
	Destination apiPREndpoint `json:"destination"`
	CreatedOn   *time.Time    `json:"created_on"`
	UpdatedOn   *time.Time    `json:"updated_on"`
	Links       apiLinks      `json:"links"`
	// Reviewers are sent by GET …/pullrequests/{id} only (nil when absent,
	// as in listings); EditPR sends them back.
	Reviewers []apiAccount `json:"reviewers"`
}

// apiText is rendered markup: raw is what was typed.
type apiText struct {
	Raw *string `json:"raw"`
}

// apiPREndpoint is the source or destination of a pull request: its branch,
// its commit (a short hash) and its repository (gone for the source of a
// deleted fork).
type apiPREndpoint struct {
	Branch     *apiBranch  `json:"branch"`
	Commit     *apiCommit  `json:"commit"`
	Repository *apiRepoRef `json:"repository"`
}

// apiWorkspaceAccess is an entry of GET /2.0/user/workspaces.
type apiWorkspaceAccess struct {
	Workspace *struct {
		Slug string `json:"slug"`
	} `json:"workspace"`
}

// check reports what the driver depends on that a pull request lacks.
func (p *apiPR) check(op string) error {
	switch {
	case p.ID <= 0:
		return shapeError(op, "a pull request without an id")
	case !slices.Contains(allStates, p.State):
		return shapeError(op, "pull request #%d has state %q", p.ID, p.State)
	case p.Destination.Repository == nil || !uuidRe.MatchString(p.Destination.Repository.UUID):
		return shapeError(op, "pull request #%d lacks its destination repository", p.ID)
	case p.Destination.Branch == nil || p.Destination.Branch.Name == "":
		return shapeError(op, "pull request #%d lacks its destination branch", p.ID)
	case p.CreatedOn == nil:
		return shapeError(op, "pull request #%d has no created_on", p.ID)
	}
	return nil
}

// head returns the source branch of p, "" when the API names none.
func (p *apiPR) head() string {
	if p.Source.Branch == nil {
		return ""
	}
	return p.Source.Branch.Name
}

// authorID returns the uuid of p's author, "" when there is none (a
// deleted account).
func (p *apiPR) authorID() string {
	if p.Author == nil {
		return ""
	}
	return p.Author.UUID
}

// body returns the description and whether the API sent it.
func (p *apiPR) body() (string, bool) {
	switch {
	case p.Summary != nil && p.Summary.Raw != nil:
		return *p.Summary.Raw, true
	case p.Description != nil:
		return *p.Description, true
	}
	return "", false
}

// toPR converts an API pull request; BaseExists and HeadSHA are left to the
// caller (fill). Declined and superseded pull requests are closed, merged
// ones merged; closed_by names who did it. The API has no time of closing:
// ClosedAt is updated_on, which a later change of a closed pull request
// (a comment) moves on, so it is never earlier than the close.
func (c *client) toPR(p *apiPR) platform.PR {
	body, _ := p.body()
	pr := platform.PR{
		Number:    p.ID,
		URL:       p.Links.HTML.Href,
		State:     platform.Open,
		Draft:     p.Draft,
		Head:      p.head(),
		Base:      p.Destination.Branch.Name,
		RepoID:    p.Destination.Repository.UUID,
		Title:     p.Title,
		Body:      body,
		Labels:    []string{},
		Author:    toAccount(p.Author),
		CreatedAt: p.CreatedOn.UTC(),
	}
	if pr.URL == "" && p.Destination.Repository.FullName != "" {
		pr.URL = c.web + "/" + p.Destination.Repository.FullName + "/pull-requests/" + strconv.FormatInt(p.ID, 10)
	}
	if p.Source.Repository != nil && uuidRe.MatchString(p.Source.Repository.UUID) && pr.Head != "" {
		pr.HeadRepoID = p.Source.Repository.UUID
	}
	switch p.State {
	case stateMerged:
		pr.State = platform.Merged
	case stateDeclined, stateSuperseded:
		pr.State = platform.Closed
	}
	if pr.State != platform.Open {
		if by := toAccount(p.ClosedBy); by.ID != "" {
			pr.ClosedBy = &by
		}
		if p.UpdatedOn != nil {
			pr.ClosedAt = p.UpdatedOn.UTC()
		}
	}
	return pr
}

// prQuery returns the BBQL filter of pull requests from heads in states:
// `source.branch.name IN ("a", "b") AND state IN ("OPEN")`. The escaping
// of BBQL strings is not documented: a head with a quote, a backslash or a
// control character drops the branch filter, and the caller, which
// filters every listing anyway, reads them all.
func prQuery(heads, states []string) string {
	quote := func(list []string) string {
		parts := make([]string, len(list))
		for i, s := range list {
			parts[i] = `"` + s + `"`
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	q := "state IN " + quote(states)
	for _, h := range heads {
		if strings.ContainsFunc(h, func(r rune) bool { return r == '"' || r == '\\' || r < 0x20 || r == 0x7f }) {
			return q
		}
	}
	return "source.branch.name IN " + quote(heads) + " AND " + q
}

// PRs returns, newest first, the pull requests of r from heads by authors
// in every state and the open ones by anyone: one listing of r's pull
// requests filtered by source branch and every state in q=, sorted by
// descending id, filtered again here. A pull request's head is its source
// branch in whatever repository: those from forks are included with their
// own HeadRepoID. The listing is complete or the call fails.
//
// BaseExists reads the branch API for a base other than r's default branch
// (once per base). HeadSHA is the full id of the source commit of an open
// pull request from r itself, from the commit API (the pull request shows
// 12 digits; a short id once expanded is remembered); "" for the others,
// which the core reads no head of.
func (d *reader) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	const op = "list pull requests"
	ws, slug, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	wanted := setOf(heads)
	if len(wanted) == 0 {
		return []platform.PR{}, nil
	}
	ids := accountIDs(authors)
	q := url.Values{
		"q":       {prQuery(sortedKeys(wanted), allStates)},
		"sort":    {"-id"},
		"pagelen": {strconv.Itoa(prPageLen)},
	}
	var found []apiPR
	seen := map[int64]bool{}
	complete, err := listAll(ctx, d.c, op, d.c.endpoint("repositories", ws, slug, "pullrequests"), q, maxPRPages, func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		if !seen[p.ID] && wanted[p.head()] && (p.State == stateOpen || ids[p.authorID()]) {
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
			Err: fmt.Errorf("%s has more pull requests than %d pages; touchmark does not act on a partial list", r.Path, maxPRPages)}
	}
	out := make([]platform.PR, 0, len(found))
	bases := map[string]bool{}
	for i := range found {
		p := &found[i]
		if err := d.c.fillBody(ctx, op, ws, slug, p); err != nil {
			return nil, err
		}
		pr := d.c.toPR(p)
		if pr.BaseExists, err = d.c.baseExists(ctx, op, r, ws, slug, pr.Base, bases); err != nil {
			return nil, err
		}
		if pr.State == platform.Open && pr.HeadRepoID != "" && pr.HeadRepoID == pr.RepoID && p.Source.Commit != nil {
			if pr.HeadSHA, err = d.c.fullCommit(ctx, op, ws, slug, pr.RepoID, p.Source.Commit.Hash); err != nil {
				return nil, err
			}
		}
		out = append(out, pr)
	}
	slices.SortFunc(out, func(a, b platform.PR) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

// fillBody reads a pull request alone when its listing left its
// description out (a listing is taken to carry summary; to be confirmed
// live), so that the marker in it is never missed.
func (c *client) fillBody(ctx context.Context, op, ws, slug string, p *apiPR) error {
	if _, ok := p.body(); ok {
		return nil
	}
	var full apiPR
	if _, err := c.get(ctx, op, c.endpoint("repositories", ws, slug, "pullrequests", strconv.FormatInt(p.ID, 10)), nil, &full); err != nil {
		return err
	}
	if err := full.check(op); err != nil {
		return err
	}
	if _, ok := full.body(); !ok || full.ID != p.ID {
		return shapeError(op, "pull request #%d has no description", p.ID)
	}
	*p = full
	return nil
}

// baseExists reports whether branch, a pull request's base in r, exists:
// r's default branch does; another is looked up once per call (known holds
// the answers). A pull request whose base was deleted stays with its
// branch's name.
func (c *client) baseExists(ctx context.Context, op string, r platform.Repo, ws, slug, branch string, known map[string]bool) (bool, error) {
	if branch == r.DefaultBranch {
		return true, nil
	}
	if v, ok := known[branch]; ok {
		return v, nil
	}
	_, err := c.get(ctx, op, c.endpoint(append([]string{"repositories", ws, slug, "refs", "branches"}, pathSegments(branch)...)...), nil, nil)
	switch {
	case err == nil:
		known[branch] = true
	case platform.ClassOf(err) == platform.ClassNotFound:
		known[branch] = false
	default:
		return false, err
	}
	return known[branch], nil
}

// fullCommit returns the full id of the commit whose id starts with short
// in ws/slug (the repository with uuid repo): GET …/commit/{short}. A
// commit the API does not find, or an answer that is no full id with that
// prefix, gives "" (the head stays unknown); a failure that stops the call
// or is transient is returned.
func (c *client) fullCommit(ctx context.Context, op, ws, slug, repo, short string) (string, error) {
	short = strings.ToLower(strings.TrimSpace(short))
	switch {
	case !isHex(short) || len(short) < 7:
		return "", nil
	case isHexOID(short):
		return short, nil
	}
	key := repo + " " + short
	c.mu.Lock()
	full, ok := c.commits[key]
	c.mu.Unlock()
	if ok {
		return full, nil
	}
	var cm apiCommit
	_, err := c.get(ctx, op, c.endpoint("repositories", ws, slug, "commit", short), nil, &cm)
	switch {
	case err != nil && fatal(err):
		return "", err
	case err != nil:
		return "", nil
	}
	full = strings.ToLower(cm.Hash)
	if !isHexOID(full) || !strings.HasPrefix(full, short) {
		return "", nil
	}
	c.mu.Lock()
	c.commits[key] = full
	c.mu.Unlock()
	return full, nil
}

// OpenPRsBy lists the open pull requests by authors from heads in every
// repository this identity sees, for the stale sweep: for each workspace of
// the identity (GET /2.0/user/workspaces) and each author, the open pull
// requests the author made there (GET
// /2.0/workspaces/{workspace}/pullrequests/{uuid}, filtered by source
// branch and state in q=), filtered again here; each repository is read
// once, for the report and the default branch. HeadSHA stays "": the sweep
// closes pull requests and reads no heads.
//
// A listing that fails, on any page, with a rate limit, a refused
// credential, a transient failure or the end of ctx fails the call, so the
// core can pause or retry; any other failure of a listing (an author the
// workspace does not know, a missing scope) makes the result incomplete, as
// does a listing capped at its bound. A repository or pull request gone
// since the listing is left out; one that cannot be read otherwise makes
// the result incomplete.
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
	workspaces, complete, err := d.c.workspaces(ctx, op)
	switch {
	case err != nil && fatal(err):
		return platform.Swept{}, err
	case err != nil:
		out.Complete = false
	}
	out.Complete = out.Complete && complete
	type hit struct {
		ws string
		pr apiPR
	}
	hits := map[string]hit{}
	q := url.Values{
		"q":       {prQuery(sortedKeys(wanted), []string{stateOpen})},
		"sort":    {"-id"},
		"pagelen": {strconv.Itoa(prPageLen)},
	}
	for _, ws := range workspaces {
		for _, id := range sortedKeys(ids) {
			if !uuidRe.MatchString(id) {
				out.Complete = false
				continue
			}
			complete, err := listAll(ctx, d.c, op, d.c.endpoint("workspaces", ws, "pullrequests", id), q, maxSweepPages, func(p apiPR) error {
				if err := p.check(op); err != nil {
					return err
				}
				if p.State == stateOpen && wanted[p.head()] && ids[p.authorID()] {
					hits[p.Destination.Repository.UUID+"#"+strconv.FormatInt(p.ID, 10)] = hit{ws: ws, pr: p}
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
	repos := map[string]*apiRepo{}
	bases := map[string]map[string]bool{}
	for _, k := range keys {
		h := hits[k]
		p := h.pr
		uuid := p.Destination.Repository.UUID
		ws := h.ws
		if w, _, ok := splitRepoPath(p.Destination.Repository.FullName); ok {
			ws = w
		}
		repo, ok := repos[uuid]
		if !ok {
			repo, err = d.c.getRepo(ctx, op, ws, uuid)
			if err == nil && repo.UUID != uuid {
				err = shapeError(op, "repository %s answers with uuid %s", uuid, repo.UUID)
			}
			switch {
			case err == nil:
			case platform.ClassOf(err) == platform.ClassNotFound:
				repo = nil
			case stops(err):
				return platform.Swept{}, err
			default:
				out.Complete = false
				repo = nil
			}
			repos[uuid] = repo
		}
		if repo == nil {
			continue
		}
		rws, rslug, _ := splitRepoPath(repo.FullName)
		r := d.c.toRepo(repo)
		if bases[uuid] == nil {
			bases[uuid] = map[string]bool{}
		}
		err := d.c.fillBody(ctx, op, rws, rslug, &p)
		var pr platform.PR
		if err == nil {
			pr = d.c.toPR(&p)
			pr.BaseExists, err = d.c.baseExists(ctx, op, r, rws, rslug, pr.Base, bases[uuid])
		}
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

// errAnonymousSweep is why an anonymous reader cannot sweep: it has no
// workspaces and no pull requests of its own.
var errAnonymousSweep = errors.New("no credential: the reader is anonymous, and only an account has workspaces to sweep")

// workspaces returns the slugs of the workspaces the identity reaches,
// sorted; complete is false when the listing was capped or a later page
// failed.
func (c *client) workspaces(ctx context.Context, op string) ([]string, bool, error) {
	set := map[string]bool{}
	q := url.Values{"pagelen": {strconv.Itoa(repoPageLen)}}
	complete, err := listAll(ctx, c, op, c.endpoint("user", "workspaces"), q, maxWorkspacePages, func(a apiWorkspaceAccess) error {
		if a.Workspace == nil || a.Workspace.Slug == "" || strings.ContainsAny(a.Workspace.Slug, "/\x00?#") {
			return shapeError(op, "a workspace without a slug")
		}
		set[a.Workspace.Slug] = true
		return nil
	})
	if err != nil && (!laterPage(err) || fatal(err)) {
		return nil, false, err
	}
	if err != nil {
		complete = false
	}
	return sortedKeys(set), complete, nil
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
