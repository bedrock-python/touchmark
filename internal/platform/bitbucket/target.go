package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Repository permissions of an account (the permission enum of the
// OpenAPI description's repository_permission).
const (
	permAdmin = "admin"
	permWrite = "write"
	permRead  = "read"
	permNone  = "none"
)

// maxPermPages bounds the listing of the writer's repository permissions in
// a workspace when the API does not filter it by repository.
const maxPermPages = 50

// apiRepoPermission is an entry of GET
// /2.0/user/workspaces/{workspace}/permissions/repositories.
type apiRepoPermission struct {
	Permission string      `json:"permission"`
	Repository *apiRepoRef `json:"repository"`
}

// permission returns the identity's permission on the repository with uuid
// in workspace ws: admin, write or read, and none when the listing does not
// name the repository (the API lists the repositories the account was
// given access to, not public ones it merely sees). The listing is
// filtered by the repository's uuid (q=repository.uuid="{…}"); when the API
// refuses the filter (400), the workspace's whole listing is read, at most
// maxPermPages pages. Entries are matched by uuid whatever the filter did.
func (c *client) permission(ctx context.Context, op, ws, uuid string) (string, error) {
	u := c.endpoint("user", "workspaces", ws, "permissions", "repositories")
	found := ""
	each := func(p apiRepoPermission) error {
		if p.Repository != nil && p.Repository.UUID == uuid && found == "" {
			found = strings.ToLower(p.Permission)
		}
		return nil
	}
	q := url.Values{"q": {`repository.uuid="` + uuid + `"`}, "pagelen": {strconv.Itoa(repoPageLen)}}
	complete, err := listAll(ctx, c, op, u, q, maxPermPages, each)
	if err != nil && platform.ClassOf(err) == platform.ClassInvalid && !laterPage(err) {
		complete, err = listAll(ctx, c, op, u, url.Values{"pagelen": {strconv.Itoa(repoPageLen)}}, maxPermPages, each)
	}
	switch {
	case err != nil:
		return "", err
	case found != "":
		return found, nil
	case !complete:
		return "", &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("the writer's permissions in %s span more than %d pages, and repository %s is not among those read", ws, maxPermPages, uuid)}
	}
	return permNone, nil
}

// mayWrite reports whether a permission allows pushes and pull requests.
func mayWrite(perm string) bool { return perm == permWrite || perm == permAdmin }

// Target checks that the writer may write to r and returns a TargetWriter
// bound to it, before the target's writes. The repository is read as the
// writer, by its uuid (a rename does not matter): one the writer cannot see
// is ClassNotFound. Its permission there must be write or admin (Bitbucket
// has one right for pushes and pull requests, and none of its own for CI
// files, so Workflows needs nothing more); otherwise the error is
// ClassPermission with Rule "contents" or "pull-requests".
func (w *writer) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	const op = "target"
	if _, err := w.c.selfAccount(ctx); err != nil {
		return nil, err
	}
	if _, err := w.Remote(ctx, r); err != nil {
		return nil, err
	}
	repo, err := w.c.targetRepo(ctx, op, r)
	if err != nil {
		return nil, err
	}
	ws, slug, _ := splitRepoPath(repo.FullName)
	perm, err := w.c.permission(ctx, op, ws, repo.UUID)
	if err != nil {
		return nil, err
	}
	switch {
	case need.Contents && !mayWrite(perm):
		return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "contents",
			Err: fmt.Errorf("the writer has %s permission on %s, not write: it may not push", perm, repo.FullName)}
	case need.PRs && !mayWrite(perm):
		return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "pull-requests",
			Err: fmt.Errorf("the writer has %s permission on %s, not write: it may not write pull requests", perm, repo.FullName)}
	}
	pr := w.c.toRepo(repo)
	rem, err := w.Remote(ctx, pr)
	if err != nil {
		return nil, err
	}
	return &target{c: w.c, ws: ws, slug: slug, repo: pr, remote: rem.URL}, nil
}

// targetRepo reads r as the identity: by its uuid when r has one (the path
// may be stale after a rename), else by its path. A repository that now
// answers with another uuid is ClassNotFound: r is gone.
func (c *client) targetRepo(ctx context.Context, op string, r platform.Repo) (*apiRepo, error) {
	ws, slug, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	if uuidRe.MatchString(r.ID) {
		slug = r.ID
	}
	repo, err := c.getRepo(ctx, op, ws, slug)
	if err != nil {
		return nil, err
	}
	if r.ID != "" && repo.UUID != r.ID {
		return nil, notFound(op, "%s is another repository (%s) now, not %s", r.Path, repo.UUID, r.ID)
	}
	return repo, nil
}

// target is a platform.TargetWriter. The writer's token is not narrowed:
// Close retires the TargetWriter, and later calls through it fail with
// ClassAuth, as after a revoked per-target token elsewhere.
type target struct {
	c        *client
	ws, slug string
	repo     platform.Repo
	remote   string

	mu     sync.Mutex
	closed bool
}

// live returns closedError(op) after Close.
func (t *target) live(op string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return closedError(op)
	}
	return nil
}

// Remote is the repository's URL with the writer's Basic header
// ("x-bitbucket-api-token-auth:<token>"), which fails with ClassAuth after
// Close.
func (t *target) Remote() platform.Remote {
	return platform.Remote{URL: t.remote, Header: func(ctx context.Context) (string, error) {
		if err := t.live("git credentials"); err != nil {
			return "", err
		}
		return t.c.gitHeader(ctx)
	}}
}

// Close retires the TargetWriter; closing twice is fine.
func (t *target) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

// apiBranchRef names a branch in a pull request's source or destination.
type apiBranchRef struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
}

// branchRef returns the endpoint of branch name.
func branchRef(name string) *apiBranchRef {
	var b apiBranchRef
	b.Branch.Name = name
	return &b
}

// apiAccountRef names an account by its uuid.
type apiAccountRef struct {
	UUID string `json:"uuid"`
}

// createPR is the body of POST …/pullrequests: from a branch of the
// repository itself, never closing the source branch on merge (people
// decide what happens to touchmark's branch; the next run deletes it).
type createPR struct {
	Title             string        `json:"title"`
	Description       string        `json:"description"`
	Source            *apiBranchRef `json:"source"`
	Destination       *apiBranchRef `json:"destination"`
	CloseSourceBranch bool          `json:"close_source_branch"`
	Draft             bool          `json:"draft"`
}

// editPR is the body of PUT …/pullrequests/{id}. Title, description and
// draft are always sent, as they are when unchanged; reviewers too when the
// pull request showed them, since a PUT without them is said to clear them
// (Renovate's experience); the destination only to change it.
type editPR struct {
	Title       string           `json:"title"`
	Description *string          `json:"description,omitempty"`
	Destination *apiBranchRef    `json:"destination,omitempty"`
	Reviewers   *[]apiAccountRef `json:"reviewers,omitempty"`
	Draft       bool             `json:"draft"`
}

// comment is the body of POST …/pullrequests/{id}/comments.
type comment struct {
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
}

// CreatePR opens a pull request from np.Head in the repository itself to
// np.Base, a draft when np.Draft (Bitbucket's own flag). Bitbucket has no
// labels: np.Labels are ignored (Caps.NoLabels).
//
// An open pull request from the same head in the repository itself, to
// whatever base, is returned with ErrExists before anything is written.
// When the POST is refused (400 or 409) and such a pull request is open
// then (one opened meanwhile), it is returned with ErrExists too; otherwise
// the refusal stands. A POST whose answer was lost is the core's to
// reconcile: it lists the pull requests of the head before it tries again.
func (t *target) CreatePR(ctx context.Context, np platform.NewPR) (platform.PR, error) {
	const op = "create pull request"
	if err := t.live(op); err != nil {
		return platform.PR{}, err
	}
	switch {
	case np.Head == "" || np.Base == "":
		return platform.PR{}, invalid(op, "head and base are required")
	case np.Head == np.Base:
		return platform.PR{}, invalid(op, "head and base are both %q", np.Head)
	case strings.TrimSpace(np.Title) == "":
		return platform.PR{}, invalid(op, "the title is empty")
	}
	open, err := t.openFrom(ctx, op, np)
	if err != nil {
		return platform.PR{}, err
	}
	if open != nil {
		return t.exists(ctx, op, open, np.Head, 0)
	}
	body := createPR{Title: np.Title, Description: np.Body, Source: branchRef(np.Head), Destination: branchRef(np.Base), Draft: np.Draft}
	var p apiPR
	_, err = t.c.call(ctx, op, http.MethodPost, t.prsURL(), nil, body, &p)
	if err != nil {
		if c := platform.ClassOf(err); c == platform.ClassInvalid || c == platform.ClassConflict {
			return t.existing(ctx, op, np, err)
		}
		return platform.PR{}, err
	}
	if err := p.check(op); err != nil {
		return platform.PR{}, err
	}
	return t.finish(ctx, op, &p)
}

// existing returns the open pull request from np.Head in the repository
// itself with ErrExists, after refused, a 400 or 409 of CreatePR's POST;
// refused alone when none is open.
func (t *target) existing(ctx context.Context, op string, np platform.NewPR, refused error) (platform.PR, error) {
	match, err := t.openFrom(ctx, op, np)
	if err != nil {
		return platform.PR{}, errors.Join(refused, err)
	}
	if match == nil {
		return platform.PR{}, refused
	}
	return t.exists(ctx, op, match, np.Head, statusOf(refused))
}

// openFrom returns the open pull request from np.Head in the repository
// itself (source and destination), the one to np.Base first; nil when none
// is open. One from a fork's branch of that name is not from np.Head.
func (t *target) openFrom(ctx context.Context, op string, np platform.NewPR) (*apiPR, error) {
	var match *apiPR
	q := url.Values{
		"q":       {prQuery([]string{np.Head}, []string{stateOpen})},
		"sort":    {"-id"},
		"pagelen": {strconv.Itoa(prPageLen)},
	}
	complete, err := listAll(ctx, t.c, op, t.prsURL(), q, maxPRPages, func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		if !t.ownOpen(&p, np.Head) {
			return nil
		}
		if match == nil || p.Destination.Branch.Name == np.Base && match.Destination.Branch.Name != np.Base {
			match = &p
		}
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case !complete && match == nil:
		return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%s has more open pull requests from %s than %d pages", t.repo.Path, np.Head, maxPRPages)}
	}
	return match, nil
}

// ownOpen reports whether p is an open pull request from head in the
// repository itself.
func (t *target) ownOpen(p *apiPR, head string) bool {
	return p.State == stateOpen && p.head() == head &&
		p.Source.Repository != nil && p.Source.Repository.UUID == t.repo.ID &&
		p.Destination.Repository.UUID == t.repo.ID
}

// exists returns p, the open pull request from head, with the ErrExists of
// CreatePR; status is the HTTP status that told it (0 when a listing did).
func (t *target) exists(ctx context.Context, op string, p *apiPR, head string, status int) (platform.PR, error) {
	pr, err := t.finish(ctx, op, p)
	if err != nil {
		return platform.PR{}, err
	}
	return pr, &platform.Error{Op: op, Class: platform.ClassConflict, Status: status,
		Err: fmt.Errorf("#%d is open from %s: %w", p.ID, head, platform.ErrExists)}
}

// finish converts a pull request the API answered with, as PRs reports
// it: its description read alone when the answer left it out, BaseExists,
// and the full id of its head for an open pull request from the
// repository itself.
func (t *target) finish(ctx context.Context, op string, p *apiPR) (platform.PR, error) {
	if err := t.c.fillBody(ctx, op, t.ws, t.slug, p); err != nil {
		return platform.PR{}, err
	}
	pr := t.c.toPR(p)
	var err error
	if pr.BaseExists, err = t.c.baseExists(ctx, op, t.repo, t.ws, t.slug, pr.Base, map[string]bool{}); err != nil {
		return platform.PR{}, err
	}
	if pr.State == platform.Open && pr.HeadRepoID != "" && pr.HeadRepoID == pr.RepoID && p.Source.Commit != nil {
		if pr.HeadSHA, err = t.c.fullCommit(ctx, op, t.ws, t.slug, pr.RepoID, p.Source.Commit.Hash); err != nil {
			return platform.PR{}, err
		}
	}
	return pr, nil
}

// EditPR changes a pull request. It reads the pull request first: only an
// open one can be changed on Bitbucket.
//
//   - Open: title, body and base go in one PUT (the current title, body,
//     draft flag and reviewers sent back with them, so that nothing else
//     changes), skipped when it would change nothing; then, for State
//     Closed, the pull request is declined (POST …/decline). The PUT comes
//     first: once declined, a pull request can never be changed again, and
//     the body touchmark closes its own pull requests with carries the
//     marker's "closed". A failure between the two leaves an open pull
//     request with the new body, which the next run decides on again.
//   - Declined or superseded: State Closed alone changes nothing; State
//     Open is ClassUnsupported (Bitbucket cannot reopen a declined pull
//     request), and so is any change of title, body or base.
//   - Merged: any change is ClassConflict, as a pull request merged since
//     the core read it.
//
// State Merged is ClassUnsupported: touchmark merges nothing. Labels are
// ignored (Caps.NoLabels); the draft flag never changes. A PUT refused
// because of the reviewers sent back (400 naming reviewers: one left the
// workspace or was deactivated) is sent once more without them.
func (t *target) EditPR(ctx context.Context, number int64, e platform.PREdit) (platform.PR, error) {
	const op = "edit pull request"
	if err := t.live(op); err != nil {
		return platform.PR{}, err
	}
	switch {
	case number <= 0:
		return platform.PR{}, notFound(op, "pull request #%d", number)
	case e.Title != nil && strings.TrimSpace(*e.Title) == "":
		return platform.PR{}, invalid(op, "the title is empty")
	case e.State != nil && *e.State == platform.Merged:
		return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassUnsupported, Err: errors.New("touchmark merges no pull request")}
	case e.State != nil && *e.State != platform.Open && *e.State != platform.Closed:
		return platform.PR{}, invalid(op, "state %q: only open and closed can be set", *e.State)
	case e.Base != nil && *e.Base == "":
		return platform.PR{}, invalid(op, "the base is empty")
	}
	cur, err := t.getPR(ctx, op, number)
	if err != nil {
		return platform.PR{}, err
	}
	body, _ := cur.body()
	changes := e.Title != nil && *e.Title != cur.Title || e.Body != nil && *e.Body != body ||
		e.Base != nil && *e.Base != cur.Destination.Branch.Name
	switch cur.State {
	case stateMerged:
		if changes || e.State != nil {
			return platform.PR{}, conflict(op, "#%d is merged: it can no longer be changed", number)
		}
		return t.finish(ctx, op, cur)
	case stateDeclined, stateSuperseded:
		switch {
		case e.State != nil && *e.State == platform.Open:
			return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassUnsupported,
				Err: fmt.Errorf("#%d: Bitbucket cannot reopen a declined pull request", number)}
		case changes:
			return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassUnsupported,
				Err: fmt.Errorf("#%d is %s: Bitbucket changes open pull requests only", number, strings.ToLower(cur.State))}
		}
		return t.finish(ctx, op, cur)
	}
	if changes {
		put := editPR{Title: cur.Title, Draft: cur.Draft}
		if e.Title != nil {
			put.Title = *e.Title
		}
		switch {
		case e.Body != nil:
			put.Description = e.Body
		case cur.Summary != nil && cur.Summary.Raw != nil || cur.Description != nil:
			put.Description = &body
		}
		if e.Base != nil && *e.Base != cur.Destination.Branch.Name {
			put.Destination = branchRef(*e.Base)
		}
		if cur.Reviewers != nil {
			refs := make([]apiAccountRef, 0, len(cur.Reviewers))
			for _, r := range cur.Reviewers {
				if r.UUID != "" {
					refs = append(refs, apiAccountRef{UUID: r.UUID})
				}
			}
			put.Reviewers = &refs
		}
		got, err := t.put(ctx, op, number, put)
		if err != nil && put.Reviewers != nil && len(*put.Reviewers) > 0 && reviewersRefused(err) {
			put.Reviewers = nil
			got, err = t.put(ctx, op, number, put)
		}
		if err != nil {
			return platform.PR{}, err
		}
		cur = got
	}
	if e.State != nil && *e.State == platform.Closed {
		// The answer of a decline is not described: the pull request is
		// read again.
		if _, err := t.c.call(ctx, op, http.MethodPost, t.prURL(number, "decline"), nil, nil, nil); err != nil {
			return platform.PR{}, err
		}
		got, err := t.getPR(ctx, op, number)
		if err != nil {
			return platform.PR{}, err
		}
		if got.State == stateOpen {
			return platform.PR{}, conflict(op, "Bitbucket did not decline #%d (it is %s)", number, got.State)
		}
		cur = got
	}
	return t.finish(ctx, op, cur)
}

// reviewersRefused reports whether err, a refused PUT, is about its
// reviewers (a 400 whose message or fields name them).
func reviewersRefused(err error) bool {
	return statusOf(err) == http.StatusBadRequest && strings.Contains(strings.ToLower(err.Error()), "reviewer")
}

// getPR reads pull request number alone (with its reviewers).
func (t *target) getPR(ctx context.Context, op string, number int64) (*apiPR, error) {
	var p apiPR
	if _, err := t.c.get(ctx, op, t.prURL(number), nil, &p); err != nil {
		return nil, err
	}
	if err := p.check(op); err != nil {
		return nil, err
	}
	if p.ID != number {
		return nil, shapeError(op, "GET pull request #%d answers #%d", number, p.ID)
	}
	return &p, nil
}

// put sends one PUT of pull request number and returns the pull request it
// answers with.
func (t *target) put(ctx context.Context, op string, number int64, body editPR) (*apiPR, error) {
	var got apiPR
	if _, err := t.c.call(ctx, op, http.MethodPut, t.prURL(number), nil, body, &got); err != nil {
		return nil, err
	}
	if err := got.check(op); err != nil {
		return nil, err
	}
	return &got, nil
}

// Comment adds a comment to pull request number (POST …/comments with the
// text as raw Markdown). Bitbucket takes comments on declined and merged
// pull requests too.
func (t *target) Comment(ctx context.Context, number int64, body string) error {
	const op = "comment"
	if err := t.live(op); err != nil {
		return err
	}
	switch {
	case number <= 0:
		return notFound(op, "pull request #%d", number)
	case strings.TrimSpace(body) == "":
		return invalid(op, "the comment is empty")
	}
	var c comment
	c.Content.Raw = body
	_, err := t.c.call(ctx, op, http.MethodPost, t.prURL(number, "comments"), nil, c, nil)
	return err
}

// EnsureLabels does nothing: Bitbucket pull requests have no labels
// (Caps.NoLabels), and the core asks for none.
func (t *target) EnsureLabels(context.Context, []string) ([]string, error) {
	if err := t.live("ensure labels"); err != nil {
		return nil, err
	}
	return nil, nil
}

func (t *target) prsURL() string {
	return t.c.endpoint("repositories", t.ws, t.slug, "pullrequests")
}

func (t *target) prURL(number int64, more ...string) string {
	return t.c.endpoint(append([]string{"repositories", t.ws, t.slug, "pullrequests", strconv.FormatInt(number, 10)}, more...)...)
}

// conflict is a ClassConflict error of op.
func conflict(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassConflict, Err: fmt.Errorf(format, args...)}
}
