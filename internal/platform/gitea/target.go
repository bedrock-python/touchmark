package gitea

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

// Target checks that the writer may write to r and returns a TargetWriter
// bound to it, before the target's writes. The repository is read as the
// writer, by id (a rename does not matter): a repository it cannot see is
// ClassNotFound; without push permission (its role; Gitea and Forgejo have
// no separate right for CI files, so Workflows needs nothing more) it is
// ClassPermission with Rule "contents" or "pull-requests". An administrator
// account is refused as in Self.
func (w *writer) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	const op = "target"
	if _, err := w.c.selfAccount(ctx); err != nil {
		return nil, err
	}
	remote, err := w.c.remoteURL(op, r)
	if err != nil {
		return nil, err
	}
	u := ""
	if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil && id > 0 {
		u = w.c.endpoint("repositories", r.ID)
	} else {
		owner, name, _ := repoPath(op, r)
		u = w.c.endpoint("repos", owner, name)
	}
	var repo apiRepo
	if _, err := w.c.get(ctx, op, u, nil, &repo); err != nil {
		return nil, err
	}
	if err := repo.check(op); err != nil {
		return nil, err
	}
	push := repo.Permissions != nil && (repo.Permissions.Push || repo.Permissions.Admin)
	switch {
	case need.Contents && !push:
		return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "contents",
			Err: fmt.Errorf("the writer may not push to %s", repo.FullName)}
	case need.PRs && !push:
		return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "pull-requests",
			Err: fmt.Errorf("the writer may not write pull requests of %s", repo.FullName)}
	}
	owner, name, _ := splitRepoPath(repo.FullName)
	return &target{c: w.c, owner: owner, name: name, repoID: repo.ID, remote: remote,
		prsDisabled: repo.HasPullRequests != nil && !*repo.HasPullRequests}, nil
}

// target is a platform.TargetWriter. The writer's token is not narrowed:
// Close retires the TargetWriter, and later calls through it fail with
// ClassAuth, as after a revoked per-target token elsewhere.
type target struct {
	c           *client
	owner, name string
	repoID      int64
	remote      string
	prsDisabled bool

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

// Remote is the repository's URL with the writer's Basic header, which
// fails with ClassAuth after Close.
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

// createPR is the body of POST /repos/{owner}/{repo}/pulls.
type createPR struct {
	Head   string  `json:"head"`
	Base   string  `json:"base"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Labels []int64 `json:"labels,omitempty"`
}

// editPR is the body of PATCH /repos/{owner}/{repo}/pulls/{index}: nil
// fields stay as they are.
type editPR struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	State *string `json:"state,omitempty"`
	Base  *string `json:"base,omitempty"`
}

// addLabels is the body of POST /repos/{owner}/{repo}/issues/{index}/labels.
type addLabels struct {
	Labels []int64 `json:"labels"`
}

// comment is the body of POST /repos/{owner}/{repo}/issues/{index}/comments.
type comment struct {
	Body string `json:"body"`
}

// CreatePR opens a pull request from np.Head in the repository itself,
// with its labels by id (created when missing). A draft gets the "WIP: "
// title prefix; when the instance does not take it for a draft (its
// WORK_IN_PROGRESS_PREFIXES lack it), the prefix is taken off again and the
// pull request is ready.
//
// An open pull request from the same head, to whatever base, is returned
// with ErrExists before anything is written: the servers refuse (409) only
// one to the same base, and open a second one from the head to another
// base (checked on Gitea 1.26 and 1.27 and Forgejo 15 and 16). The 409 of a
// duplicate opened meanwhile gives the same.
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
	case t.prsDisabled:
		return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassPolicy, Rule: "prs-disabled",
			Err: fmt.Errorf("pull requests are disabled in %s/%s", t.owner, t.name)}
	}
	for _, l := range np.Labels {
		if strings.TrimSpace(l) == "" {
			return platform.PR{}, invalid(op, "blank label name")
		}
	}
	open, err := t.openFrom(ctx, op, np)
	if err != nil {
		return platform.PR{}, err
	}
	if open != nil {
		return existsError(op, open, np.Head, 0)
	}
	labels, err := t.c.ensureLabels(ctx, op, t.owner, t.name, np.Labels)
	if err != nil {
		return platform.PR{}, err
	}
	title := np.Title
	if np.Draft && !hasDraftPrefix(title) {
		title = draftPrefix + title
	}
	var p apiPR
	_, err = t.c.call(ctx, op, http.MethodPost, t.c.endpoint("repos", t.owner, t.name, "pulls"), nil,
		createPR{Head: np.Head, Base: np.Base, Title: title, Body: np.Body, Labels: labels}, &p)
	if isStatus(err, http.StatusConflict) {
		return t.existing(ctx, op, np, err)
	}
	if err != nil {
		return platform.PR{}, err
	}
	if err := p.check(op); err != nil {
		return platform.PR{}, err
	}
	if np.Draft && !p.Draft && title != np.Title {
		// Best effort: a failure leaves a ready pull request with the prefix.
		var ready apiPR
		plain := np.Title
		_, err := t.c.call(ctx, op, http.MethodPatch, t.prURL(p.Number), nil, editPR{Title: &plain}, &ready)
		if err == nil && ready.check(op) == nil {
			p = ready
		}
	}
	return toPR(&p), nil
}

// existing returns the open pull request from np.Head in the repository
// itself (on np.Base first) with ErrExists, after conflict, a 409 of
// CreatePR; conflict alone when none is open any more.
func (t *target) existing(ctx context.Context, op string, np platform.NewPR, conflict error) (platform.PR, error) {
	match, err := t.openFrom(ctx, op, np)
	if err != nil {
		return platform.PR{}, errors.Join(conflict, err)
	}
	if match == nil {
		return platform.PR{}, conflict
	}
	return existsError(op, match, np.Head, http.StatusConflict)
}

// openFrom returns the open pull request from np.Head in the repository
// itself, the one to np.Base first; nil when none is open. A pull request
// from a fork's branch of that name, or an AGit one, is not from np.Head.
func (t *target) openFrom(ctx context.Context, op string, np platform.NewPR) (*apiPR, error) {
	var match *apiPR
	q := url.Values{"state": {"open"}}
	if inst, err := t.c.instance(ctx); err == nil && inst.headFilter() {
		q.Set("head", np.Head)
	}
	err := t.c.listPulls(ctx, op, t.owner, t.name, q, func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		if p.State != "open" || p.head() != np.Head || p.Head.RepoID != t.repoID {
			return nil
		}
		if match == nil || p.base() == np.Base && match.base() != np.Base {
			match = &p
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return match, nil
}

// existsError returns p, the open pull request from head, with the
// ErrExists of CreatePR; status is the HTTP status that told it (0 when a
// listing did).
func existsError(op string, p *apiPR, head string, status int) (platform.PR, error) {
	return toPR(p), &platform.Error{Op: op, Class: platform.ClassConflict, Status: status,
		Err: fmt.Errorf("#%d is open from %s: %w", p.Number, head, platform.ErrExists)}
}

// EditPR changes a pull request so that a refusal changes nothing. The
// servers apply a PATCH's title and body before its state and base, and
// keep them when they refuse the state (412: merged, or open dependencies)
// or the base (404: no such branch) (EditPullRequest in
// routers/api/v1/repo/pull.go; seen on Gitea 1.26 and 1.27 and Forgejo 15
// and 16). So the state and the base go first, in a PATCH of their own;
// then e.AddLabels (by id, never removing one), which the body's marker
// records; then title and body in a second PATCH. A new title keeps the
// draft prefix of a draft, so the draft state never changes.
//
// A failure between the steps leaves the ones before applied: a closed pull
// request with its old body, which the next run takes for a close by the
// writer's own account.
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
	case e.State != nil && *e.State != platform.Open && *e.State != platform.Closed:
		return platform.PR{}, invalid(op, "state %q: only open and closed can be set", *e.State)
	case e.Base != nil && *e.Base == "":
		return platform.PR{}, invalid(op, "the base is empty")
	}
	for _, l := range e.AddLabels {
		if strings.TrimSpace(l) == "" {
			return platform.PR{}, invalid(op, "blank label name")
		}
	}
	// p is the pull request as the last request left it; nil once a
	// request answered without it.
	var p *apiPR
	if e.State != nil || e.Base != nil {
		patch := editPR{Base: e.Base}
		if e.State != nil {
			s := string(*e.State)
			patch.State = &s
		}
		got, err := t.patch(ctx, op, number, patch)
		if err != nil {
			return platform.PR{}, baseError(err, e.Base)
		}
		p = got
	}
	if len(e.AddLabels) > 0 {
		ids, err := t.c.ensureLabels(ctx, op, t.owner, t.name, e.AddLabels)
		if err != nil {
			return platform.PR{}, err
		}
		if _, err := t.c.call(ctx, op, http.MethodPost, t.issueURL(number, "labels"), nil, addLabels{Labels: ids}, nil); err != nil {
			return platform.PR{}, err
		}
		p = nil
	}
	if e.Title != nil || e.Body != nil {
		patch := editPR{Body: e.Body}
		if e.Title != nil {
			cur := p
			if cur == nil {
				var err error
				if cur, err = t.c.getPR(ctx, op, t.owner, t.name, number); err != nil {
					return platform.PR{}, err
				}
			}
			title := *e.Title
			if cur.Draft {
				title = keepDraftPrefix(cur.Title, title)
			}
			patch.Title = &title
		}
		got, err := t.patch(ctx, op, number, patch)
		if err != nil {
			return platform.PR{}, err
		}
		p = got
	}
	if p == nil {
		got, err := t.c.getPR(ctx, op, t.owner, t.name, number)
		if err != nil {
			return platform.PR{}, err
		}
		p = got
	}
	pr := toPR(p)
	if err := t.c.fillCloser(ctx, op, t.owner, t.name, &pr); err != nil {
		return platform.PR{}, err
	}
	return pr, nil
}

// patch sends one PATCH of pull request number and returns the pull
// request it answers with.
func (t *target) patch(ctx context.Context, op string, number int64, patch editPR) (*apiPR, error) {
	var got apiPR
	if _, err := t.c.call(ctx, op, http.MethodPatch, t.prURL(number), nil, patch, &got); err != nil {
		return nil, err
	}
	if err := got.check(op); err != nil {
		return nil, err
	}
	return &got, nil
}

// baseError turns the 404 of a PATCH that names a base the repository
// lacks ("new base 'x' not exist") into ClassInvalid: the pull request is
// there, the request was wrong.
func baseError(err error, base *string) error {
	var pe *platform.Error
	if base == nil || !errors.As(err, &pe) || pe.Status != http.StatusNotFound ||
		!strings.Contains(strings.ToLower(pe.Error()), "new base") {
		return err
	}
	return &platform.Error{Op: pe.Op, Class: platform.ClassInvalid, Status: pe.Status,
		Err: fmt.Errorf("the base branch %q does not exist: %s", *base, strings.TrimSuffix(pe.Err.Error(), ": "+platform.ErrNotFound.Error()))}
}

// Comment adds a comment to pull request number. Gitea answers a comment
// on a missing issue with 500: the pull request is then looked up, and a
// missing one is ErrNotFound.
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
	_, err := t.c.call(ctx, op, http.MethodPost, t.issueURL(number, "comments"), nil, comment{Body: body}, nil)
	if err != nil && isStatus(err, http.StatusInternalServerError) {
		if _, lookup := t.c.getPR(ctx, op, t.owner, t.name, number); platform.ClassOf(lookup) == platform.ClassNotFound {
			return lookup
		}
	}
	return err
}

// EnsureLabels creates the missing labels and returns the ids of all.
func (t *target) EnsureLabels(ctx context.Context, names []string) ([]string, error) {
	const op = "ensure labels"
	if err := t.live(op); err != nil {
		return nil, err
	}
	ids, err := t.c.ensureLabels(ctx, op, t.owner, t.name, names)
	if err != nil {
		return nil, err
	}
	return labelStrings(ids), nil
}

func (t *target) prURL(number int64) string {
	return t.c.endpoint("repos", t.owner, t.name, "pulls", strconv.FormatInt(number, 10))
}

func (t *target) issueURL(number int64, what string) string {
	return t.c.endpoint("repos", t.owner, t.name, "issues", strconv.FormatInt(number, 10), what)
}

// Draft prefixes Gitea and Forgejo recognize by default
// (WORK_IN_PROGRESS_PREFIXES), compared ignoring case as they do.
var draftPrefixes = []string{"WIP:", "[WIP]"}

// hasDraftPrefix reports whether title starts with a default draft prefix.
func hasDraftPrefix(title string) bool { return draftPrefixOf(title) != "" }

// draftPrefixOf returns the draft prefix title starts with, as written in
// title and with the blanks after it; "" for none.
func draftPrefixOf(title string) string {
	for _, p := range draftPrefixes {
		if len(title) >= len(p) && strings.EqualFold(title[:len(p)], p) {
			rest := title[len(p):]
			return title[:len(p)+len(rest)-len(strings.TrimLeft(rest, " \t"))]
		}
	}
	return ""
}

// keepDraftPrefix returns title with the draft prefix of current, a draft's
// title: the one it has, or draftPrefix when the instance marked it a
// draft by a prefix touchmark does not know.
func keepDraftPrefix(current, title string) string {
	if hasDraftPrefix(title) {
		return title
	}
	if p := draftPrefixOf(current); p != "" {
		return p + title
	}
	return draftPrefix + title
}
