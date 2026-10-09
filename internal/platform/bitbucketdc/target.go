package bitbucketdc

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

// maxPermPages bounds the listing that tells whether the writer may write
// to a repository: the repositories of its project with its name that the
// writer may write to.
const maxPermPages = 5

// Repository permissions (the permission filter of GET /repos).
const (
	permWrite = "REPO_WRITE"
	permAdmin = "REPO_ADMIN"
)

// hasPermission reports whether the identity has perm on repo: GET
// /repos?projectkey=<key>&name=<name>&permission=<perm>&archived=ALL lists
// the repositories of the project with that name on which the identity has
// perm, matched here by id. Bitbucket shows a user's permissions on a
// repository to its admins only; this listing is the identity's own view.
func (c *client) hasPermission(ctx context.Context, op string, repo *apiRepo, perm string) (bool, error) {
	q := url.Values{"projectkey": {repo.Project.Key}, "name": {repo.Name}, "permission": {perm}, "archived": {"ALL"}}
	found := false
	complete, err := listAll(ctx, c, op, c.endpoint("repos"), q, pageLimit, maxPermPages, func(r apiRepo) error {
		if r.ID == repo.ID {
			found = true
		}
		return nil
	})
	switch {
	case err != nil:
		return false, err
	case !found && !complete:
		return false, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("more repositories named %q than touchmark reads, and %s is not among them", repo.Name, repo.path())}
	}
	return found, nil
}

// Target checks that the writer may write to r and returns a TargetWriter
// bound to it, before the target's writes. The repository is read as the
// writer: one the writer cannot see is ClassNotFound, and so is one that
// answers with another id (r is gone, and another took its path). Pushes
// need write permission (REPO_WRITE); pull requests need only read
// permission, which seeing the repository implies, and CI files need
// nothing of their own. A writer without write permission gets
// ClassPermission with Rule "contents".
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
	if need.Contents {
		ok, err := w.c.hasPermission(ctx, op, repo, permWrite)
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "contents",
				Err: fmt.Errorf("the writer may not write to %s (no REPO_WRITE): it may not push", repo.path())}
		}
	}
	def, err := w.c.defaultBranchOf(ctx, op, repo.Project.Key, repo.Slug)
	if err != nil {
		return nil, err
	}
	pr := w.c.toRepo(repo, def)
	rem, err := w.Remote(ctx, pr)
	if err != nil {
		return nil, err
	}
	return &target{c: w.c, key: repo.Project.Key, slug: repo.Slug, repo: pr, remote: rem.URL}, nil
}

// targetRepo reads r as the identity, by its path. A repository that now
// answers with another id than r's is ClassNotFound: r is gone.
func (c *client) targetRepo(ctx context.Context, op string, r platform.Repo) (*apiRepo, error) {
	key, slug, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	repo, err := c.getRepo(ctx, op, key, slug)
	if err != nil {
		return nil, err
	}
	if r.ID != "" && strconv.FormatInt(repo.ID, 10) != r.ID {
		return nil, notFound(op, "%s is another repository (%d) now, not %s", r.Path, repo.ID, r.ID)
	}
	return repo, nil
}

// target is a platform.TargetWriter. The writer's token is not narrowed:
// Close retires the TargetWriter, and later calls through it fail with
// ClassAuth, as after a revoked per-target token elsewhere.
type target struct {
	c         *client
	key, slug string
	repo      platform.Repo
	remote    string

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

// Remote is the repository's URL with the writer's Bearer header, which
// fails with ClassAuth after Close.
func (t *target) Remote() platform.Remote {
	return platform.Remote{URL: t.remote, Header: func(ctx context.Context) (string, error) {
		if err := t.live("git credentials"); err != nil {
			return "", err
		}
		return t.c.header(ctx)
	}}
}

// Close retires the TargetWriter; closing twice is fine.
func (t *target) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

// refBody names a branch of the target repository in a pull request.
type refBody struct {
	ID         string   `json:"id"`
	Repository repoBody `json:"repository"`
}

// repoBody names a repository by its project's key and its slug.
type repoBody struct {
	Slug    string `json:"slug"`
	Project struct {
		Key string `json:"key"`
	} `json:"project"`
}

// ref returns the body of branch name of the target repository.
func (t *target) ref(name string) *refBody {
	r := &refBody{ID: "refs/heads/" + name}
	r.Repository.Slug = t.slug
	r.Repository.Project.Key = t.key
	return r
}

// reviewerBody names a reviewer by user name.
type reviewerBody struct {
	User struct {
		Name string `json:"name"`
	} `json:"user"`
}

// createPR is the body of POST …/pull-requests: from a branch of the
// repository itself, without reviewers (Bitbucket adds default reviewers
// to pull requests made in its web interface, not through REST).
type createPR struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Draft       bool     `json:"draft"`
	FromRef     *refBody `json:"fromRef"`
	ToRef       *refBody `json:"toRef"`
}

// editPR is the body of PUT …/pull-requests/{id}: the version read, the
// title, description and draft flag as they are or change, the reviewers
// sent back as they are (an update without them is not documented), the
// target branch only to change it.
type editPR struct {
	Version     int64          `json:"version"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Draft       bool           `json:"draft"`
	Reviewers   []reviewerBody `json:"reviewers"`
	ToRef       *refBody       `json:"toRef,omitempty"`
}

// CreatePR opens a pull request from np.Head in the repository itself to
// np.Base, a draft when np.Draft (Bitbucket's own flag; when the instance
// refuses drafts, it is opened ready). Bitbucket Data Center has no labels
// on pull requests: np.Labels are ignored (Caps.NoLabels).
//
// An open pull request from the same head in the repository itself, to
// whatever base, is returned with ErrExists before anything is written.
// When the POST is refused with 409 (Bitbucket keeps one open pull request
// per pair of branches) and such a pull request is open then, it is
// returned with ErrExists too; otherwise the refusal stands. A POST whose
// answer was lost is the core's to reconcile: it lists the pull requests
// of the head before it tries again.
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
	body := createPR{Title: np.Title, Description: np.Body, Draft: np.Draft, FromRef: t.ref(np.Head), ToRef: t.ref(np.Base)}
	var p apiPR
	_, err = t.c.call(ctx, op, http.MethodPost, t.prsURL(), nil, body, &p)
	if err != nil && np.Draft && draftRefused(err) {
		body.Draft = false
		_, err = t.c.call(ctx, op, http.MethodPost, t.prsURL(), nil, body, &p)
	}
	if err != nil {
		if platform.ClassOf(err) == platform.ClassConflict {
			return t.existing(ctx, op, np, err)
		}
		return platform.PR{}, err
	}
	if err := p.check(op); err != nil {
		return platform.PR{}, err
	}
	return t.finish(ctx, op, &p)
}

// draftRefused reports whether err, a refused POST, is about the draft
// flag: an instance with drafts turned off (feature.pull.request.drafts).
func draftRefused(err error) bool {
	c := platform.ClassOf(err)
	return (c == platform.ClassInvalid || c == platform.ClassConflict || c == platform.ClassUnsupported) &&
		strings.Contains(strings.ToLower(err.Error()), "draft")
}

// existing returns the open pull request from np.Head in the repository
// itself with ErrExists, after refused, a 409 of CreatePR's POST; refused
// alone when none is open.
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
// itself (source and target), the one to np.Base first; nil when none is
// open.
func (t *target) openFrom(ctx context.Context, op string, np platform.NewPR) (*apiPR, error) {
	var match *apiPR
	q := url.Values{"state": {stateOpen}, "direction": {"OUTGOING"}, "at": {"refs/heads/" + np.Head},
		"withAttributes": {"false"}, "withProperties": {"false"}}
	complete, err := listAll(ctx, t.c, op, t.prsURL(), q, pageLimit, maxPRPages, func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		if !t.ownOpen(&p, np.Head) {
			return nil
		}
		if match == nil || p.base() == np.Base && match.base() != np.Base {
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
	return p.State == stateOpen && p.head() == head && p.FromRef.repoID() == t.repo.ID && p.ToRef.repoID() == t.repo.ID
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
// it: BaseExists, and the head of an open pull request. A closed one
// carries no ClosedBy: the core reads closers through PRs.
func (t *target) finish(ctx context.Context, op string, p *apiPR) (platform.PR, error) {
	pr := t.c.toPR(p)
	var err error
	if pr.BaseExists, err = t.c.baseExists(ctx, op, t.repo, t.key, t.slug, pr.Base, map[string]bool{}); err != nil {
		return platform.PR{}, err
	}
	if pr.State == platform.Open && isHexOID(p.FromRef.LatestCommit) {
		pr.HeadSHA = strings.ToLower(p.FromRef.LatestCommit)
	}
	return pr, nil
}

// EditPR changes a pull request. It reads the pull request first, for its
// version, which every change must name.
//
//   - Open: title, body and base go in one PUT (with the version, the
//     current title, body, draft flag and reviewers sent back, so that
//     nothing else changes), skipped when it would change nothing; then,
//     for State Closed, the pull request is declined (POST …/decline with
//     the version the PUT answered). The PUT comes first: touchmark writes
//     no declined pull request, and the body it closes its own pull
//     requests with carries the marker's "closed". A failure between the
//     two leaves an open pull request with the new body, which the next
//     run decides on again. A 409 (the pull request changed since it was
//     read) reads it again and tries once more.
//   - Declined: State Closed alone changes nothing; State Open, and any
//     change of title, body or base, are ClassUnsupported: touchmark
//     writes no declined pull request (Caps.ClosedImmutable), though
//     Bitbucket can reopen one.
//   - Merged: any change is ClassConflict, as a pull request merged since
//     the core read it.
//
// State Merged is ClassUnsupported: touchmark merges nothing. Labels are
// ignored (Caps.NoLabels); the draft flag never changes.
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
	var cur *apiPR
	for attempt := 0; ; attempt++ {
		var err error
		if cur, err = t.getPR(ctx, op, number); err != nil {
			return platform.PR{}, err
		}
		done, err := t.edit(ctx, op, cur, e)
		switch {
		case err == nil:
			cur = done
		case attempt == 0 && platform.ClassOf(err) == platform.ClassConflict && statusOf(err) == http.StatusConflict:
			continue // changed since it was read
		default:
			return platform.PR{}, err
		}
		break
	}
	return t.finish(ctx, op, cur)
}

// edit applies e to cur, the pull request as just read (see EditPR), and
// returns the pull request as it then is.
func (t *target) edit(ctx context.Context, op string, cur *apiPR, e platform.PREdit) (*apiPR, error) {
	body := ""
	if cur.Description != nil {
		body = *cur.Description
	}
	changes := e.Title != nil && *e.Title != cur.Title || e.Body != nil && *e.Body != body ||
		e.Base != nil && *e.Base != cur.base()
	switch cur.State {
	case stateMerged:
		if changes || e.State != nil {
			return nil, conflict(op, "#%d is merged: it can no longer be changed", cur.ID)
		}
		return cur, nil
	case stateDeclined:
		switch {
		case e.State != nil && *e.State == platform.Open:
			return nil, &platform.Error{Op: op, Class: platform.ClassUnsupported,
				Err: fmt.Errorf("#%d is declined: touchmark reopens no declined pull request on Bitbucket Data Center", cur.ID)}
		case changes:
			return nil, &platform.Error{Op: op, Class: platform.ClassUnsupported,
				Err: fmt.Errorf("#%d is declined: touchmark changes no declined pull request on Bitbucket Data Center", cur.ID)}
		}
		return cur, nil
	}
	if changes {
		put := editPR{Version: *cur.Version, Title: cur.Title, Description: body, Draft: cur.Draft, Reviewers: []reviewerBody{}}
		if e.Title != nil {
			put.Title = *e.Title
		}
		if e.Body != nil {
			put.Description = *e.Body
		}
		if e.Base != nil && *e.Base != cur.base() {
			put.ToRef = t.ref(*e.Base)
		}
		for _, r := range cur.Reviewers {
			if r.User != nil && r.User.Name != "" {
				var rb reviewerBody
				rb.User.Name = r.User.Name
				put.Reviewers = append(put.Reviewers, rb)
			}
		}
		var got apiPR
		if _, err := t.c.call(ctx, op, http.MethodPut, t.prURL(cur.ID), nil, put, &got); err != nil {
			return nil, err
		}
		if err := got.check(op); err != nil {
			return nil, err
		}
		cur = &got
	}
	if e.State != nil && *e.State == platform.Closed {
		version := strconv.FormatInt(*cur.Version, 10)
		var got apiPR
		if _, err := t.c.call(ctx, op, http.MethodPost, t.prURL(cur.ID, "decline"), url.Values{"version": {version}},
			map[string]any{"version": *cur.Version}, &got); err != nil {
			return nil, err
		}
		if err := got.check(op); err != nil {
			return nil, err
		}
		if got.State == stateOpen {
			return nil, conflict(op, "Bitbucket did not decline #%d (it is %s)", cur.ID, got.State)
		}
		cur = &got
	}
	return cur, nil
}

// getPR reads pull request number alone.
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

// Comment adds a comment to pull request number (POST …/comments with the
// text as Markdown). Bitbucket takes comments on declined and merged pull
// requests too, but not in an archived repository.
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
	_, err := t.c.call(ctx, op, http.MethodPost, t.prURL(number, "comments"), nil, map[string]string{"text": body}, nil)
	return err
}

// EnsureLabels does nothing: Bitbucket Data Center pull requests have no
// labels (Caps.NoLabels), and the core asks for none.
func (t *target) EnsureLabels(context.Context, []string) ([]string, error) {
	if err := t.live("ensure labels"); err != nil {
		return nil, err
	}
	return nil, nil
}

func (t *target) prsURL() string { return t.c.repoURL(t.key, t.slug, "pull-requests") }

func (t *target) prURL(number int64, more ...string) string {
	return t.c.repoURL(t.key, t.slug, append([]string{"pull-requests", strconv.FormatInt(number, 10)}, more...)...)
}

// conflict is a ClassConflict error of op.
func conflict(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassConflict, Err: fmt.Errorf(format, args...)}
}
