package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of pull request listings.
const (
	// maxPRPages bounds the REST listing of one head of one repository.
	// PRs has no way to say it saw only part: past the bound it fails.
	maxPRPages = 50
	// maxGQLPages bounds the GraphQL pages of open pull requests from one
	// head.
	maxGQLPages = 20
	// closersPerQuery is how many pull requests one closers query names
	// (nodes(ids:) takes at most 100).
	closersPerQuery = 100
	// maxInstallationRepoPages bounds GET /installation/repositories of one
	// installation: 10 000 repositories.
	maxInstallationRepoPages = 100
)

// apiPR is a pull request as the REST API reports it.
type apiPR struct {
	Number   int64      `json:"number"`
	NodeID   string     `json:"node_id"`
	HTMLURL  string     `json:"html_url"`
	State    string     `json:"state"`
	Draft    bool       `json:"draft"`
	Title    string     `json:"title"`
	Body     *string    `json:"body"`
	User     *apiUser   `json:"user"`
	Labels   []apiLabel `json:"labels"`
	Head     *apiRef    `json:"head"`
	Base     *apiRef    `json:"base"`
	Created  *time.Time `json:"created_at"`
	Closed   *time.Time `json:"closed_at"`
	MergedAt *time.Time `json:"merged_at"`
}

// apiRef is the head or base of a pull request: repo is null for the head
// of a pull request whose fork was deleted.
type apiRef struct {
	Ref   string   `json:"ref"`
	SHA   string   `json:"sha"`
	Label string   `json:"label"`
	Repo  *apiRepo `json:"repo"`
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
	case p.Base.Repo == nil || p.Base.Repo.ID <= 0 || p.Base.Ref == "":
		return shapeError(op, "pull request #%d lacks its base repository or branch", p.Number)
	case p.Created == nil:
		return shapeError(op, "pull request #%d has no created_at", p.Number)
	}
	return nil
}

// toPR converts a REST pull request; ClosedBy and BaseExists are the
// caller's to fill (closers, bases).
func (c *client) toPR(p *apiPR) platform.PR {
	pr := platform.PR{
		Number:     p.Number,
		URL:        p.HTMLURL,
		State:      platform.Open,
		Draft:      p.Draft,
		Head:       p.Head.Ref,
		HeadSHA:    strings.ToLower(p.Head.SHA),
		Base:       p.Base.Ref,
		RepoID:     strconv.FormatInt(p.Base.Repo.ID, 10),
		BaseExists: true,
		Title:      p.Title,
		Labels:     []string{},
		Author:     c.toAccount(p.User),
		CreatedAt:  p.Created.UTC(),
	}
	if p.Body != nil {
		pr.Body = *p.Body
	}
	if p.Head.Repo != nil && p.Head.Repo.ID > 0 {
		pr.HeadRepoID = strconv.FormatInt(p.Head.Repo.ID, 10)
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	switch {
	case p.State == "closed" && p.MergedAt != nil:
		pr.State = platform.Merged
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

// gqlPR is a pull request as gqlPRFields reads it.
type gqlPR struct {
	ID          string     `json:"id"`
	DatabaseID  int64      `json:"databaseId"`
	Number      int64      `json:"number"`
	URL         string     `json:"url"`
	State       string     `json:"state"` // OPEN, CLOSED, MERGED
	IsDraft     bool       `json:"isDraft"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	CreatedAt   *time.Time `json:"createdAt"`
	ClosedAt    *time.Time `json:"closedAt"`
	MergedAt    *time.Time `json:"mergedAt"`
	HeadRefName string     `json:"headRefName"`
	HeadRefOID  string     `json:"headRefOid"`
	BaseRefName string     `json:"baseRefName"`
	BaseRef     *struct {
		Name string `json:"name"`
	} `json:"baseRef"`
	HeadRepository *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"headRepository"`
	Repository *struct {
		DatabaseID    int64  `json:"databaseId"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author *gqlActor `json:"author"`
	Labels *struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// toPR converts a GraphQL pull request. The base exists when baseRef
// resolves; a deleted author (the ghost) is unknown.
func (p *gqlPR) toPR() (platform.PR, bool) {
	if p == nil || p.Number <= 0 || p.Repository == nil || p.Repository.DatabaseID <= 0 || p.CreatedAt == nil {
		return platform.PR{}, false
	}
	pr := platform.PR{
		Number:     p.Number,
		URL:        p.URL,
		State:      platform.Open,
		Draft:      p.IsDraft,
		Head:       p.HeadRefName,
		HeadSHA:    strings.ToLower(p.HeadRefOID),
		Base:       p.BaseRefName,
		RepoID:     strconv.FormatInt(p.Repository.DatabaseID, 10),
		BaseExists: p.BaseRef != nil,
		Title:      p.Title,
		Body:       p.Body,
		Labels:     []string{},
		Author:     p.Author.account(),
		CreatedAt:  p.CreatedAt.UTC(),
	}
	if p.HeadRepository != nil && p.HeadRepository.DatabaseID > 0 {
		pr.HeadRepoID = strconv.FormatInt(p.HeadRepository.DatabaseID, 10)
	}
	if p.Labels != nil {
		for _, l := range p.Labels.Nodes {
			pr.Labels = append(pr.Labels, l.Name)
		}
	}
	switch p.State {
	case "OPEN":
	case "MERGED":
		pr.State = platform.Merged
	case "CLOSED":
		pr.State = platform.Closed
	default:
		return platform.PR{}, false
	}
	if pr.State != platform.Open {
		switch {
		case p.ClosedAt != nil:
			pr.ClosedAt = p.ClosedAt.UTC()
		case p.MergedAt != nil:
			pr.ClosedAt = p.MergedAt.UTC()
		}
	}
	return pr, true
}

// PRs returns, newest first, the pull requests of r from heads by authors
// in every state and the open ones by anyone.
//
// Each head is listed with GET /pulls?head=<owner>:<head>&state=all: the
// owner prefix is required, a bare branch is ignored silently and every
// pull request of the repository comes back (checked on github.com). That
// finds the pull requests from the repository itself (and from any other
// repository of its owner). Open pull requests from forks with a branch of
// that name come from GraphQL pullRequests(headRefName:), which returns
// them with their head repository; an anonymous reader, without GraphQL,
// reads every open pull request instead and keeps those from heads.
//
// Merged is merged_at set; who closed or merged comes from GraphQL (the
// actor of the last ClosedEvent, mergedBy), one request for all closed
// pull requests of the call, remembered per close for the run; an
// anonymous reader leaves ClosedBy nil (Caps.CloserKnown is false then).
// BaseExists asks the branch API, once per base. Listings are complete or
// the call fails.
func (d *reader) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	const op = "list pull requests"
	if err := d.c.checkHost(op, r); err != nil {
		return nil, err
	}
	owner, name, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	wanted := uniqueHeads(heads)
	ids := accountIDs(authors)
	if len(wanted) == 0 {
		return []platform.PR{}, nil
	}
	a, err := d.c.ownerAuth(ctx, owner)
	if err != nil {
		return nil, err
	}
	found := map[int64]platform.PR{}
	nodes := map[int64]string{}
	keep := func(pr platform.PR, node string) {
		if !slices.Contains(wanted, pr.Head) || pr.State != platform.Open && !ids[pr.Author.ID] {
			return
		}
		if _, dup := found[pr.Number]; !dup {
			found[pr.Number], nodes[pr.Number] = pr, node
		}
	}
	for _, h := range wanted {
		q := url.Values{"head": {owner + ":" + h}, "state": {"all"}}
		err := d.c.listPulls(ctx, op, a, owner, name, q, func(p apiPR) error {
			// A head the filter did not honor is dropped: never all pull
			// requests of the repository.
			if p.Head.Ref == h {
				keep(d.c.toPR(&p), p.NodeID)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if d.c.kind == credAnonymous {
		err := d.c.listPulls(ctx, op, nil, owner, name, url.Values{"state": {"open"}}, func(p apiPR) error {
			keep(d.c.toPR(&p), p.NodeID)
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		for _, h := range wanted {
			prs, err := d.c.openFromHead(ctx, op, a, owner, name, h)
			if err != nil {
				return nil, err
			}
			for _, pr := range prs {
				keep(pr.pr, pr.node)
			}
		}
	}
	out := make([]platform.PR, 0, len(found))
	for _, pr := range found {
		out = append(out, pr)
	}
	slices.SortFunc(out, func(a, b platform.PR) int { return cmp.Compare(b.Number, a.Number) })
	if err := d.c.checkBases(ctx, op, a, owner, name, out); err != nil {
		return nil, err
	}
	if d.c.kind != credAnonymous {
		if err := d.c.fillClosers(ctx, op, a, out, nodes); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listPulls reads every page of the pull requests of owner/name that q
// selects; a listing over maxPRPages pages fails.
func (c *client) listPulls(ctx context.Context, op string, a *httpx.Auth, owner, name string, q url.Values, each func(apiPR) error) error {
	complete, err := listAll(ctx, c, op, c.repoURL(owner, name, "pulls"), q, a, maxPRPages, func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		return each(p)
	})
	if err != nil {
		return err
	}
	if !complete {
		return &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%s/%s has more pull requests than %d pages; touchmark does not act on a partial list", owner, name, maxPRPages)}
	}
	return nil
}

// nodePR is a pull request with its GraphQL node id.
type nodePR struct {
	pr   platform.PR
	node string
}

// openFromHead lists the open pull requests of owner/name from head in any
// repository (GraphQL; forks included).
func (c *client) openFromHead(ctx context.Context, op string, a *httpx.Auth, owner, name, head string) ([]nodePR, error) {
	var out []nodePR
	var after any
	for range maxGQLPages {
		var data struct {
			Repository *struct {
				PullRequests struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []*gqlPR `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		errs, err := c.graphql(ctx, op, a, queryOpenPRs, map[string]any{"owner": owner, "name": name, "head": head, "after": after}, &data)
		if err != nil {
			return nil, err
		}
		if e := errs.at("repository"); e != nil {
			return nil, c.fieldError(op, e)
		}
		if data.Repository == nil {
			return nil, notFound(op, "repository %s/%s", owner, name)
		}
		for _, n := range data.Repository.PullRequests.Nodes {
			pr, ok := n.toPR()
			if !ok {
				return nil, shapeError(op, "a pull request of %s/%s without number, repository, state or createdAt", owner, name)
			}
			out = append(out, nodePR{pr: pr, node: n.ID})
		}
		pi := data.Repository.PullRequests.PageInfo
		if !pi.HasNextPage || pi.EndCursor == "" {
			return out, nil
		}
		after = pi.EndCursor
	}
	return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
		Err: fmt.Errorf("%s/%s has more open pull requests from %s than touchmark reads", owner, name, head)}
}

// checkBases sets BaseExists of each pull request from the branch API
// (GET /branches/{base}: 404 when it is gone), once per base.
func (c *client) checkBases(ctx context.Context, op string, a *httpx.Auth, owner, name string, prs []platform.PR) error {
	exists := map[string]bool{}
	for i := range prs {
		base := prs[i].Base
		ok, known := exists[base]
		if !known {
			_, err := c.get(ctx, op, c.repoURL(owner, name, "branches", base), nil, a, nil)
			switch {
			case err == nil:
				ok = true
			case platform.ClassOf(err) == platform.ClassNotFound:
			default:
				return err
			}
			exists[base] = ok
		}
		prs[i].BaseExists = ok
	}
	return nil
}

// closer is who closed one close of a pull request (nil: nobody named).
type closer struct {
	by *platform.Account
}

// closerKey identifies one close of one pull request: a reopened and
// closed again pull request has a new closedAt.
func closerKey(pr *platform.PR) string {
	return pr.RepoID + "#" + strconv.FormatInt(pr.Number, 10) + "@" + strconv.FormatInt(pr.ClosedAt.Unix(), 10)
}

// gqlClosers is one node of queryClosers.
type gqlClosers struct {
	Number     int64 `json:"number"`
	Repository *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"repository"`
	MergedBy      *gqlActor `json:"mergedBy"`
	TimelineItems *struct {
		Nodes []*struct {
			Actor *gqlActor `json:"actor"`
		} `json:"nodes"`
	} `json:"timelineItems"`
}

// fillCloser is fillClosers for one pull request with node id node.
func (c *client) fillCloser(ctx context.Context, op string, a *httpx.Auth, pr *platform.PR, node string) error {
	prs := []platform.PR{*pr}
	if err := c.fillClosers(ctx, op, a, prs, map[int64]string{pr.Number: node}); err != nil {
		return err
	}
	*pr = prs[0]
	return nil
}

// fillClosers sets ClosedBy of the closed and merged pull requests of prs,
// all of one repository (nodes maps their numbers to node ids): mergedBy
// for a merge, else the actor of the last ClosedEvent (a deleted account
// is unknown, without an id: never touchmark's own). A close without a
// ClosedEvent leaves ClosedBy nil. The answers are matched by repository
// and number, not by node id (GitHub migrates node ids to a new format).
// Closers are remembered per close, so the repeated listings of a run ask
// once.
func (c *client) fillClosers(ctx context.Context, op string, a *httpx.Auth, prs []platform.PR, nodes map[int64]string) error {
	var ask []string
	index := map[int64]int{}
	for i := range prs {
		pr := &prs[i]
		node := nodes[pr.Number]
		if pr.State == platform.Open || pr.ClosedBy != nil || node == "" {
			continue
		}
		c.mu.Lock()
		known, ok := c.closers[closerKey(pr)]
		c.mu.Unlock()
		if ok {
			pr.ClosedBy = cloneAccount(known.by)
			continue
		}
		if _, dup := index[pr.Number]; !dup {
			ask = append(ask, node)
			index[pr.Number] = i
		}
	}
	for start := 0; start < len(ask); start += closersPerQuery {
		chunk := ask[start:min(start+closersPerQuery, len(ask))]
		var data struct {
			Nodes []*gqlClosers `json:"nodes"`
		}
		errs, err := c.graphql(ctx, op, a, queryClosers, map[string]any{"ids": chunk}, &data)
		if err != nil {
			return err
		}
		if len(errs) > 0 {
			return c.fieldError(op, &errs[0])
		}
		for _, n := range data.Nodes {
			if n == nil {
				continue
			}
			i, ok := index[n.Number]
			if !ok || n.Repository == nil || strconv.FormatInt(n.Repository.DatabaseID, 10) != prs[i].RepoID {
				continue
			}
			pr := &prs[i]
			var by *platform.Account
			switch {
			case pr.State == platform.Merged && n.MergedBy != nil:
				acc := n.MergedBy.account()
				by = &acc
			case n.TimelineItems != nil && len(n.TimelineItems.Nodes) > 0 && n.TimelineItems.Nodes[0] != nil:
				acc := n.TimelineItems.Nodes[0].Actor.account()
				by = &acc
			}
			pr.ClosedBy = cloneAccount(by)
			c.mu.Lock()
			c.closers[closerKey(pr)] = &closer{by: by}
			c.mu.Unlock()
		}
	}
	return nil
}

func cloneAccount(a *platform.Account) *platform.Account {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

// OpenPRsBy lists the open pull requests by authors from heads in every
// repository this identity sees, for the stale sweep.
//
// The repositories are those of every installation of an App (GET
// /app/installations, then GET /installation/repositories with each
// installation's token), or those a token sees (GET /user/repos). Their
// open pull requests from each head come from batched GraphQL,
// nodes(ids:) of 50 repositories per request, pullRequests(headRefName:,
// states: OPEN); a repository with more of them is read to the end. The
// search API is never used: it caps results, scans at most 4 000
// repositories and lags.
//
// A suspended installation (suspended_at, or a token refused as
// suspended) is left out with a note (Swept.Notes): it mints no token, so
// the writer can close nothing there, and the listing stays complete.
//
// The result is incomplete when a listing was capped or a later page
// failed, an installation's token could not be minted for another reason,
// or a batch failed otherwise than with a rate limit, a refused
// credential, a transient failure or the end of ctx, which fail the call.
// An anonymous reader sees no repository as its own: its sweep is
// incomplete and empty.
func (d *reader) OpenPRsBy(ctx context.Context, authors []platform.Account, heads []string) (platform.Swept, error) {
	const op = "list open pull requests"
	ids := accountIDs(authors)
	wanted := uniqueHeads(heads)
	out := platform.Swept{PRs: []platform.RepoPR{}, Complete: true}
	if len(ids) == 0 || len(wanted) == 0 {
		return out, nil
	}
	type scope struct {
		auth  *httpx.Auth
		repos []apiRepo
	}
	var scopes []scope
	switch d.c.kind {
	case credAnonymous:
		out.Complete = false
		return out, nil
	case credToken:
		a := d.c.staticAuth("Bearer " + d.c.token)
		repos, complete, err := d.c.listRepos(ctx, op, a, d.c.endpoint("user", "repos"))
		if err != nil {
			return platform.Swept{}, err
		}
		out.Complete = out.Complete && complete
		scopes = append(scopes, scope{auth: a, repos: repos})
	case credApp:
		insts, complete, err := d.c.app.installations(ctx, op)
		if err != nil {
			return platform.Swept{}, err
		}
		out.Complete = out.Complete && complete
		for _, inst := range insts {
			if inst.SuspendedAt != nil {
				out.Notes = append(out.Notes, suspendedNote(inst))
				continue
			}
			tok, err := d.c.app.readToken(ctx, inst.ID)
			switch {
			case err == nil:
			case stops(err) || platform.ClassOf(err) == platform.ClassTransient:
				return platform.Swept{}, err
			case suspendedError(err):
				out.Notes = append(out.Notes, suspendedNote(inst))
				continue
			default:
				out.Complete = false
				continue
			}
			a := d.c.staticAuth("Bearer " + tok)
			repos, complete, err := d.c.listRepos(ctx, op, a, d.c.endpoint("installation", "repositories"))
			if err != nil {
				return platform.Swept{}, err
			}
			out.Complete = out.Complete && complete
			scopes = append(scopes, scope{auth: a, repos: repos})
		}
	}
	seen := map[string]bool{}
	for _, s := range scopes {
		byID := map[int64]*apiRepo{}
		var nodeIDs []string
		for i := range s.repos {
			r := &s.repos[i]
			if r.NodeID == "" || r.check(op) != nil {
				out.Complete = false
				continue
			}
			if _, dup := byID[r.ID]; !dup {
				byID[r.ID] = r
				nodeIDs = append(nodeIDs, r.NodeID)
			}
		}
		for _, h := range wanted {
			for start := 0; start < len(nodeIDs); start += batchSize {
				chunk := nodeIDs[start:min(start+batchSize, len(nodeIDs))]
				prs, complete, err := d.c.sweepBatch(ctx, op, s.auth, chunk, h, byID)
				switch {
				case err == nil:
				case fatal(err):
					return platform.Swept{}, err
				default:
					out.Complete = false
					continue
				}
				out.Complete = out.Complete && complete
				for _, rp := range prs {
					key := rp.Repo.ID + "#" + strconv.FormatInt(rp.PR.Number, 10)
					if seen[key] || !ids[rp.PR.Author.ID] || rp.PR.Head != h {
						continue
					}
					seen[key] = true
					out.PRs = append(out.PRs, rp)
				}
			}
		}
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

// suspendedNote is the sweep's note about a suspended installation: it
// mints no token, so its pull requests can be neither read nor closed, and
// leaving them out does not make the listing incomplete.
func suspendedNote(inst apiInstallation) string {
	on := "an account"
	if inst.Account != nil && inst.Account.Login != "" {
		on = inst.Account.Login
	}
	return fmt.Sprintf("installation %d of the GitHub App on %s is suspended: the sweep leaves its repositories out", inst.ID, on)
}

// listRepos reads a listing of repositories: a JSON list (GET /user/repos)
// or {repositories: [...]} (GET /installation/repositories). complete is
// false when it was capped or a later page failed.
func (c *client) listRepos(ctx context.Context, op string, a *httpx.Auth, u string) ([]apiRepo, bool, error) {
	var out []apiRepo
	complete, err := listPages(ctx, c, op, u, nil, a, maxInstallationRepoPages, func(body []byte) (int, error) {
		var page []apiRepo
		if err := decodeRepos(body, &page); err != nil {
			return 0, shapeError(op, "a page of repositories that does not decode: %v", err)
		}
		out = append(out, page...)
		return len(page), nil
	})
	if err != nil {
		if !laterPage(err) || fatal(err) {
			return nil, false, err
		}
		complete = false
	}
	return out, complete, nil
}

// gqlSweepRepo is one node of querySweep.
type gqlSweepRepo struct {
	DatabaseID   int64 `json:"databaseId"`
	PullRequests *struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []*gqlPR `json:"nodes"`
	} `json:"pullRequests"`
}

// sweepBatch reads the open pull requests from head of the repositories
// with node ids ids, matched by database id (GitHub migrates node ids to a
// new format; the answer may use another than the question); a repository
// with more than a page of them is read to the end through openFromHead.
// complete is false when a repository could not be read.
func (c *client) sweepBatch(ctx context.Context, op string, a *httpx.Auth, ids []string, head string, byID map[int64]*apiRepo) ([]platform.RepoPR, bool, error) {
	var data struct {
		Nodes []*gqlSweepRepo `json:"nodes"`
	}
	errs, err := c.graphql(ctx, op, a, querySweep, map[string]any{"ids": ids, "head": head}, &data)
	if err != nil {
		return nil, false, err
	}
	complete := len(errs) == 0 && len(data.Nodes) == len(ids)
	var out []platform.RepoPR
	for _, n := range data.Nodes {
		if n == nil || n.PullRequests == nil {
			// A repository gone since it was listed, or not one.
			complete = false
			continue
		}
		r := byID[n.DatabaseID]
		if r == nil {
			complete = false
			continue
		}
		repo := c.toRepo(r, facts{})
		prs := make([]*gqlPR, 0, len(n.PullRequests.Nodes))
		prs = append(prs, n.PullRequests.Nodes...)
		if n.PullRequests.PageInfo.HasNextPage {
			owner, name, _ := splitRepoPath(r.FullName)
			all, err := c.openFromHead(ctx, op, a, owner, name, head)
			if err != nil {
				if fatal(err) {
					return nil, false, err
				}
				complete = false
				continue
			}
			for _, np := range all {
				out = append(out, platform.RepoPR{Repo: repo, PR: np.pr})
			}
			continue
		}
		for _, p := range prs {
			pr, ok := p.toPR()
			if !ok {
				complete = false
				continue
			}
			out = append(out, platform.RepoPR{Repo: repo, PR: pr})
		}
	}
	return out, complete, nil
}

// decodeRepos decodes a page of repositories: a list, or an object with
// the list in "repositories" (GET /installation/repositories).
func decodeRepos(body []byte, out *[]apiRepo) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(trimmed, out)
	}
	var page struct {
		Repositories []apiRepo `json:"repositories"`
	}
	if err := json.Unmarshal(trimmed, &page); err != nil {
		return err
	}
	*out = page.Repositories
	return nil
}

// uniqueHeads returns the non-empty heads, sorted, without repeats.
func uniqueHeads(heads []string) []string {
	var out []string
	for _, h := range heads {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	slices.Sort(out)
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
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
