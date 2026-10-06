package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Target narrows the writer to repository r and the permissions need.
//
// For a GitHub App it reads the installation of r (GET
// /repos/{owner}/{repo}/installation: a repository outside the App's
// installations is ClassNotFound), refuses what the installation lacks
// (ClassPermission, Rule "contents", "pull-requests" or "workflows") and
// mints a token for r only (repository_ids=[r.ID]) with metadata read,
// contents and pull requests write or read as need asks, and workflows
// write only with need.Workflows. Close revokes it (DELETE
// /installation/token).
//
// For a token it reads r (GET /repos/{owner}/{repo}) and refuses a role
// that cannot push (ClassPermission): whether the token may change
// workflow files shows only when a push is refused ("refusing to allow …
// to create or update workflow", Rule "workflows"). Close only retires
// the TargetWriter.
func (w *writer) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	const op = "target"
	remote, err := w.c.remoteURL(op, r)
	if err != nil {
		return nil, err
	}
	owner, name, _ := splitRepoPath(r.Path)
	id, err := repoID(op, r)
	if err != nil {
		return nil, err
	}
	t := &target{c: w.c, owner: owner, name: name, repoID: id, remote: remote, prsDisabled: r.PRsDisabled, need: need}
	switch w.c.kind {
	case credApp:
		inst, err := w.c.app.repoInstallation(ctx, op, owner, name)
		if err != nil {
			return nil, err
		}
		if err := missingPermission(op, inst.Permissions, need, r.Path); err != nil {
			return nil, err
		}
		t.instID = inst.ID
		if err := t.mint(ctx, op); err != nil {
			return nil, err
		}
		return &appTarget{target: t}, nil
	case credToken:
		var repo apiRepo
		a := w.c.staticAuth("Bearer " + w.c.token)
		if _, err := w.c.get(ctx, op, w.c.repoURL(owner, name), nil, a, &repo); err != nil {
			return nil, err
		}
		if err := repo.check(op); err != nil {
			return nil, err
		}
		if repo.ID != id {
			return nil, notFound(op, "%s is now repository %d, not %d", r.Path, repo.ID, id)
		}
		switch {
		case need.Contents && !repo.Permissions.canPush():
			return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "contents",
				Err: fmt.Errorf("the writer may not push to %s", repo.FullName)}
		case need.PRs && !repo.Permissions.canPush():
			return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "pull-requests",
				Err: fmt.Errorf("the writer may not write pull requests of %s", repo.FullName)}
		}
		t.owner, t.name, _ = splitRepoPath(repo.FullName)
		t.nodeID = repo.NodeID
		t.token = w.c.token
		return t, nil
	}
	return nil, &platform.Error{Op: op, Class: platform.ClassAuth, Err: errors.New("no credential: the writer is anonymous")}
}

// missingPermission returns the ClassPermission error for the first
// permission need asks for that the installation's perms lack, nil when
// none is missing.
func missingPermission(op string, perms map[string]string, need platform.Perms, path string) error {
	lack := func(perm, rule string) error {
		return &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: rule,
			Err: fmt.Errorf("the GitHub App's installation on %s has %s %q, not write", path, perm, perms[perm])}
	}
	switch {
	case need.Workflows && perms["workflows"] != "write":
		return lack("workflows", "workflows")
	case need.Contents && perms["contents"] != "write":
		return lack("contents", "contents")
	case need.PRs && perms["pull_requests"] != "write":
		return lack("pull_requests", "pull-requests")
	}
	return nil
}

// target is a platform.TargetWriter: a token for one repository, a
// per-target installation token of an App (instID set) or the writer's
// token as is.
type target struct {
	c           *client
	owner, name string
	repoID      int64
	remote      string
	prsDisabled bool
	need        platform.Perms
	instID      int64 // App only

	mu      sync.Mutex
	token   string
	expires time.Time // App only
	nodeID  string    // the repository's node id, read when needed
	closed  bool
	// stale are tokens renewed away whose revocation failed: Close tries
	// them again.
	stale []string
}

// appTarget is the target of a GitHub App: it also commits through the API
// (platform.Committer).
type appTarget struct{ *target }

var (
	_ platform.TargetWriter = (*target)(nil)
	_ platform.TargetWriter = (*appTarget)(nil)
	_ platform.Committer    = (*appTarget)(nil)
)

// targetPermissions are what the per-target token of need asks for.
func targetPermissions(need platform.Perms) map[string]string {
	level := func(write bool) string {
		if write {
			return "write"
		}
		return "read"
	}
	p := map[string]string{"metadata": "read", "contents": level(need.Contents), "pull_requests": level(need.PRs)}
	if need.Workflows {
		p["workflows"] = "write"
	}
	return p
}

// mint mints the per-target token (with t.mu held or before t is shared).
func (t *target) mint(ctx context.Context, op string) error {
	m, err := t.c.app.mint(ctx, op, t.instID, mintRequest{RepositoryIDs: []int64{t.repoID}, Permissions: targetPermissions(t.need)})
	if err != nil {
		return err
	}
	t.token, t.expires = m.Token, m.ExpiresAt
	return nil
}

// credential returns the token of the target, minted again when an App's
// expires within tokenRenew (the old one is revoked; one whose revocation
// fails is kept for Close); ClassAuth after Close.
func (t *target) credential(ctx context.Context, op string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return "", closedError(op)
	}
	if t.instID == 0 || t.c.now().Before(t.expires.Add(-tokenRenew)) {
		return t.token, nil
	}
	old := t.token
	if err := t.mint(ctx, op); err != nil {
		return "", err
	}
	if err := t.c.revoke(ctx, old); err != nil {
		t.stale = append(t.stale, old)
	}
	return t.token, nil
}

// auth returns the target's credential for one request of op.
func (t *target) auth(ctx context.Context, op string) (*httpx.Auth, error) {
	tok, err := t.credential(ctx, op)
	if err != nil {
		return nil, err
	}
	return t.c.staticAuth("Bearer " + tok), nil
}

// Remote is the repository's URL with the target token's Basic header,
// which fails with ClassAuth after Close.
func (t *target) Remote() platform.Remote {
	return platform.Remote{URL: t.remote, Header: func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		tok, err := t.credential(ctx, "git credentials")
		if err != nil {
			return "", err
		}
		return basic(tok), nil
	}}
}

// Close revokes an App's per-target token and retires the TargetWriter:
// later calls through it fail with ClassAuth. Closing twice is fine; a
// token GitHub no longer takes is revoked already.
func (t *target) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	tokens, app := append([]string{t.token}, t.stale...), t.instID != 0
	t.stale = nil
	t.mu.Unlock()
	if !app {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpx.DefaultTimeout)
	defer cancel()
	var errs []error
	for _, tok := range tokens {
		errs = append(errs, t.c.revoke(ctx, tok))
	}
	return errors.Join(errs...)
}

// createPR is the body of POST /repos/{owner}/{repo}/pulls.
type createPR struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
	Draft bool   `json:"draft,omitempty"`
}

// editPR is the body of PATCH /repos/{owner}/{repo}/pulls/{number}: nil
// fields stay as they are.
type editPR struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	State *string `json:"state,omitempty"`
	Base  *string `json:"base,omitempty"`
}

// CreatePR opens a pull request from np.Head in the repository itself
// (head is the bare branch name: the same repository), with its labels.
//
// An open pull request from the same head, to whatever base, is returned
// with ErrExists before anything is written: GitHub refuses (422 "A pull
// request already exists for <owner>:<branch>") only one to the same base.
// The 422 of a duplicate opened meanwhile gives the same. A draft that the
// repository refuses (422 "Draft pull requests are not supported in this
// repository": private repositories on GitHub Free) is opened ready, once.
// Labels are created first, then put on the new pull request (POST
// /issues/{n}/labels): if that fails, the pull request is returned with
// the error.
func (t *target) CreatePR(ctx context.Context, np platform.NewPR) (platform.PR, error) {
	const op = "create pull request"
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
	a, err := t.auth(ctx, op)
	if err != nil {
		return platform.PR{}, err
	}
	if open, err := t.openFrom(ctx, op, a, np); err != nil {
		return platform.PR{}, err
	} else if open != nil {
		return existsError(op, *open, np.Head, 0)
	}
	labels, err := t.c.ensureLabels(ctx, op, a, t.owner, t.name, np.Labels)
	if err != nil {
		return platform.PR{}, err
	}
	body := createPR{Title: np.Title, Head: np.Head, Base: np.Base, Body: np.Body, Draft: np.Draft}
	var p apiPR
	_, err = t.c.call(ctx, op, http.MethodPost, t.c.repoURL(t.owner, t.name, "pulls"), nil, a, body, &p)
	if body.Draft && statusOf(err) == http.StatusUnprocessableEntity && strings.Contains(messageOf(err), "draft pull requests are not supported") {
		body.Draft, p = false, apiPR{}
		_, err = t.c.call(ctx, op, http.MethodPost, t.c.repoURL(t.owner, t.name, "pulls"), nil, a, body, &p)
	}
	if statusOf(err) == http.StatusUnprocessableEntity && strings.Contains(messageOf(err), "a pull request already exists") {
		return t.existing(ctx, op, a, np, err)
	}
	if err != nil {
		return platform.PR{}, err
	}
	if err := p.check(op); err != nil {
		return platform.PR{}, err
	}
	pr := t.c.toPR(&p)
	if len(labels) > 0 {
		got, err := t.c.addLabels(ctx, op, a, t.owner, t.name, pr.Number, labels)
		if err != nil {
			return pr, err
		}
		pr.Labels = got
	}
	return pr, nil
}

// existing returns the open pull request from np.Head in the repository
// itself with ErrExists, after conflict, a 422 of CreatePR; conflict alone
// when none is open any more.
func (t *target) existing(ctx context.Context, op string, a *httpx.Auth, np platform.NewPR, conflict error) (platform.PR, error) {
	match, err := t.openFrom(ctx, op, a, np)
	if err != nil {
		return platform.PR{}, errors.Join(conflict, err)
	}
	if match == nil {
		return platform.PR{}, conflict
	}
	return existsError(op, *match, np.Head, http.StatusUnprocessableEntity)
}

// openFrom returns the open pull request from np.Head in the repository
// itself, the one to np.Base first; nil when none is open. A pull request
// from a fork's branch of that name is not from np.Head.
func (t *target) openFrom(ctx context.Context, op string, a *httpx.Auth, np platform.NewPR) (*platform.PR, error) {
	var match *platform.PR
	q := url.Values{"head": {t.owner + ":" + np.Head}, "state": {"open"}}
	err := t.c.listPulls(ctx, op, a, t.owner, t.name, q, func(p apiPR) error {
		if p.State != "open" || p.Head.Ref != np.Head || p.Head.Repo == nil || p.Head.Repo.ID != t.repoID {
			return nil
		}
		pr := t.c.toPR(&p)
		if match == nil || pr.Base == np.Base && match.Base != np.Base {
			match = &pr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return match, nil
}

// existsError returns pr, the open pull request from head, with the
// ErrExists of CreatePR; status is the HTTP status that told it (0 when a
// listing did).
func existsError(op string, pr platform.PR, head string, status int) (platform.PR, error) {
	return pr, &platform.Error{Op: op, Class: platform.ClassConflict, Status: status,
		Err: fmt.Errorf("#%d is open from %s: %w", pr.Number, head, platform.ErrExists)}
}

// EditPR changes a pull request so that a refusal changes nothing else: the
// state and the base go first, in a PATCH of their own (GitHub refuses a
// reopen without the head branch and a base that does not exist with 422,
// and whether it keeps a title and body sent with them is not documented);
// then e.AddLabels (never removing one), then title and body in a second
// PATCH. REST cannot change the draft state, and EditPR never does.
//
// A failure between the steps leaves the ones before applied: a closed pull
// request with its old body, which the next run takes for a close by the
// writer's own account.
func (t *target) EditPR(ctx context.Context, number int64, e platform.PREdit) (platform.PR, error) {
	const op = "edit pull request"
	a, err := t.auth(ctx, op)
	if err != nil {
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
		got, err := t.patch(ctx, op, a, number, patch)
		if err != nil {
			return platform.PR{}, baseError(err, e.Base)
		}
		p = got
	}
	if len(e.AddLabels) > 0 {
		names, err := t.c.ensureLabels(ctx, op, a, t.owner, t.name, e.AddLabels)
		if err != nil {
			return platform.PR{}, err
		}
		if _, err := t.c.addLabels(ctx, op, a, t.owner, t.name, number, names); err != nil {
			return platform.PR{}, err
		}
		p = nil
	}
	if e.Title != nil || e.Body != nil {
		got, err := t.patch(ctx, op, a, number, editPR{Title: e.Title, Body: e.Body})
		if err != nil {
			return platform.PR{}, err
		}
		p = got
	}
	if p == nil {
		var got apiPR
		if _, err := t.c.get(ctx, op, t.c.repoURL(t.owner, t.name, "pulls", itoa(number)), nil, a, &got); err != nil {
			return platform.PR{}, err
		}
		if err := got.check(op); err != nil {
			return platform.PR{}, err
		}
		p = &got
	}
	pr := t.c.toPR(p)
	prs := []platform.PR{pr}
	if err := t.c.checkBases(ctx, op, a, t.owner, t.name, prs); err != nil {
		return platform.PR{}, err
	}
	pr = prs[0]
	if err := t.c.fillCloser(ctx, op, a, &pr, p.NodeID); err != nil {
		return platform.PR{}, err
	}
	return pr, nil
}

// patch sends one PATCH of pull request number and returns the pull
// request it answers with.
func (t *target) patch(ctx context.Context, op string, a *httpx.Auth, number int64, patch editPR) (*apiPR, error) {
	var got apiPR
	if _, err := t.c.call(ctx, op, http.MethodPatch, t.c.repoURL(t.owner, t.name, "pulls", itoa(number)), nil, a, patch, &got); err != nil {
		return nil, err
	}
	if err := got.check(op); err != nil {
		return nil, err
	}
	return &got, nil
}

// baseError turns the 422 of a PATCH that names a base the repository
// lacks ("Proposed base branch '…' was not found") into ClassInvalid with
// what it means; other errors stay.
func baseError(err error, base *string) error {
	var pe *platform.Error
	if base == nil || !errors.As(err, &pe) || pe.Status != http.StatusUnprocessableEntity ||
		!strings.Contains(messageOf(pe.Err), "base") {
		return err
	}
	return &platform.Error{Op: pe.Op, Class: platform.ClassInvalid, Status: pe.Status,
		Err: fmt.Errorf("the base branch %q cannot be set: %w", *base, pe.Err)}
}

// comment is the body of POST /repos/{owner}/{repo}/issues/{n}/comments.
type comment struct {
	Body string `json:"body"`
}

// Comment adds a comment to pull request number (the issues API, which
// takes pull requests; Pull requests: write allows it).
func (t *target) Comment(ctx context.Context, number int64, body string) error {
	const op = "comment"
	a, err := t.auth(ctx, op)
	if err != nil {
		return err
	}
	switch {
	case number <= 0:
		return notFound(op, "pull request #%d", number)
	case strings.TrimSpace(body) == "":
		return invalid(op, "the comment is empty")
	}
	_, err = t.c.call(ctx, op, http.MethodPost, t.c.issueURL(t.owner, t.name, number, "comments"), nil, a, comment{Body: body}, nil)
	return err
}

// EnsureLabels creates the missing labels and returns the names of all as
// the repository spells them (GitHub puts labels on by name).
func (t *target) EnsureLabels(ctx context.Context, names []string) ([]string, error) {
	const op = "ensure labels"
	a, err := t.auth(ctx, op)
	if err != nil {
		return nil, err
	}
	return t.c.ensureLabels(ctx, op, a, t.owner, t.name, names)
}
