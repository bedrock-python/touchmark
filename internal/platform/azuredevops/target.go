package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The security namespace of Git repositories and its permission bits
// (https://learn.microsoft.com/en-us/azure/devops/organizations/security/namespace-reference,
// "Git Repositories": ID 2e9eb7ed-3c0a-47d4-87c1-0ffdd275fd87, token
// repoV2/<project id>/<repository id>; the bits are the namespace's
// actions, as GET _apis/securitynamespaces lists them: GenericContribute 4,
// CreateBranch 16, PullRequestContribute 16384).
const (
	gitNamespace          = "2e9eb7ed-3c0a-47d4-87c1-0ffdd275fd87"
	permContribute        = 4
	permCreateBranch      = 16
	permPullRequestContib = 16384
)

// maxDescription is the most characters a description holds (Pull
// Requests - Update), counted as .NET counts them, in UTF-16 code units
// (assumed).
const maxDescription = 4000

// Target checks that the writer may write to r and returns a TargetWriter
// bound to it, before the target's writes. The repository is read as the
// writer, by its id (a rename does not matter): one the writer cannot see,
// or one that answers with another id, is ClassNotFound.
//
// The permissions come from Permissions - Has Permissions on the Git
// Repositories namespace, evaluated for the caller: Contents needs
// GenericContribute and CreateBranch (the sync branch is new the first
// time; whoever creates a branch may force-push it), PRs
// PullRequestContribute. One that is missing is ClassPermission with Rule
// "contents" or "pull-requests". Whether a PAT with the Code scopes alone
// may call that API is not documented: when it refuses (401 or 403 once the
// credential itself was accepted), the check is skipped and a push or a
// pull request meets a missing permission instead (TF401027 →
// blocked:permission:push; a 403 → ClassPermission). Azure DevOps has no
// permission of its own for CI files, so Workflows needs nothing more.
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
	checks := []struct {
		want bool
		bits int
		rule string
		what string
	}{
		{need.Contents, permContribute, "contents", "contribute (push)"},
		{need.Contents, permCreateBranch, "contents", "create branches"},
		{need.PRs, permPullRequestContib, "pull-requests", "contribute to pull requests"},
	}
	for _, ch := range checks {
		if !ch.want {
			continue
		}
		ok, known, err := w.c.hasPermission(ctx, op, repo, ch.bits)
		if err != nil {
			return nil, err
		}
		if !known {
			// The API is not open to this token: the writes find out.
			break
		}
		if !ok {
			return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: ch.rule,
				Err: fmt.Errorf("the writer may not %s in %s", ch.what, repo.path())}
		}
	}
	pr := w.c.toRepo(repo)
	rem, err := w.Remote(ctx, pr)
	if err != nil {
		return nil, err
	}
	return &target{c: w.c, repo: pr, remote: rem.URL}, nil
}

// hasPermission evaluates bits of the Git Repositories namespace on repo
// for the caller: GET {org}/_apis/permissions/{namespace}/{bits}?tokens=
// repoV2/<project>/<repository>. known is false when the API refuses the
// token (401 or 403): the credential was accepted by connectionData just
// before, so the refusal is the token's scope. A rate limit, a transient
// failure or the end of ctx is err.
func (c *client) hasPermission(ctx context.Context, op string, repo *apiRepo, bits int) (ok, known bool, err error) {
	var answer list[bool]
	token := "repoV2/" + strings.ToLower(repo.Project.ID) + "/" + strings.ToLower(repo.ID)
	q := url.Values{"tokens": {token}, "alwaysAllowAdministrators": {"true"}}
	_, err = c.get(ctx, op, c.apis("permissions", gitNamespace, strconv.Itoa(bits)), q, &answer)
	switch {
	case err == nil:
	case platform.ClassOf(err) == platform.ClassAuth && statusOf(err) == http.StatusUnauthorized,
		platform.ClassOf(err) == platform.ClassPermission:
		return false, false, nil
	case stops(err) || platform.ClassOf(err) == platform.ClassTransient:
		return false, false, err
	default:
		return false, false, nil
	}
	if len(answer.Value) != 1 {
		return false, false, shapeError(op, "Has Permissions answered %d values for one token", len(answer.Value))
	}
	return answer.Value[0], true, nil
}

// targetRepo reads r as the identity, by its id. A repository that is gone
// or answers with another id is ClassNotFound.
func (c *client) targetRepo(ctx context.Context, op string, r platform.Repo) (*apiRepo, error) {
	if !guidRe.MatchString(r.ID) {
		return nil, invalid(op, "repository %s has no id", r.Path)
	}
	var repo apiRepo
	if _, err := c.get(ctx, op, c.repoAPI(r.ID), nil, &repo); err != nil {
		return nil, err
	}
	if err := repo.check(op); err != nil {
		return nil, err
	}
	if !strings.EqualFold(repo.ID, r.ID) {
		return nil, notFound(op, "%s is another repository (%s) now, not %s", r.Path, repo.ID, r.ID)
	}
	return &repo, nil
}

// target is a platform.TargetWriter. The writer's token is not narrowed:
// Close retires the TargetWriter, and later calls through it fail with
// ClassAuth, as after a revoked per-target token elsewhere.
type target struct {
	c      *client
	repo   platform.Repo
	remote string

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

// apiLabelName names a label to add (WebApiCreateTagRequestData).
type apiLabelName struct {
	Name string `json:"name"`
}

// createPR is the body of POST …/pullrequests.
type createPR struct {
	SourceRefName string         `json:"sourceRefName"`
	TargetRefName string         `json:"targetRefName"`
	Title         string         `json:"title"`
	Description   string         `json:"description"`
	IsDraft       bool           `json:"isDraft"`
	Labels        []apiLabelName `json:"labels,omitempty"`
}

// updatePR is the body of PATCH …/pullrequests/{id}: only what changes
// (Pull Requests - Update takes status, title, description and, where
// retargeting is on, targetRefName).
type updatePR struct {
	Title         *string `json:"title,omitempty"`
	Description   *string `json:"description,omitempty"`
	TargetRefName *string `json:"targetRefName,omitempty"`
	Status        *string `json:"status,omitempty"`
}

// patchOp is one operation of a JSON patch (application/json-patch+json).
type patchOp struct {
	Op    string  `json:"op"`
	Path  string  `json:"path"`
	Value *string `json:"value,omitempty"`
}

// checkDescription refuses a description Azure DevOps would refuse: more
// than maxDescription characters (UTF-16 code units, assumed).
func checkDescription(op, desc string) error {
	if n := len(utf16.Encode([]rune(desc))); n > maxDescription {
		return invalid(op, "a description of %d characters, more than Azure DevOps' %d", n, maxDescription)
	}
	return nil
}

// CreatePR opens a pull request from np.Head in the repository itself to
// np.Base: a draft when np.Draft (Azure DevOps' own flag), with np.Labels
// by name. np.Body is split (marker.Detach): the description is the body
// without its marker line, and the marker line goes into the pull request's
// property touchmark.marker right after the pull request is made (Pull
// Request Properties - Update). Labels the answer lacks are added one by
// one (whether labels in the create request are applied is to be confirmed
// live).
//
// An active pull request from the same head in the repository itself, to
// whatever base, is returned with ErrExists before anything is written;
// when the POST is refused as a duplicate (409, or 400 with
// GitPullRequestExistsException or TF401179) and such a pull request is
// active then, it is returned with ErrExists too. A failure after the POST
// (the property, a label) fails the call with the pull request open: the
// core reconciles by listing the head's pull requests, and an own pull
// request whose property was never written has no marker (the next run
// blocks it as marker-invalid until an adopt entry takes it over).
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
	desc, line := marker.Detach(np.Body)
	if err := checkDescription(op, desc); err != nil {
		return platform.PR{}, err
	}
	open, err := t.activeFrom(ctx, op, np.Head, np.Base)
	if err != nil {
		return platform.PR{}, err
	}
	if open != nil {
		return t.exists(ctx, op, open, np.Head, 0)
	}
	body := createPR{
		SourceRefName: "refs/heads/" + np.Head,
		TargetRefName: "refs/heads/" + np.Base,
		Title:         np.Title,
		Description:   desc,
		IsDraft:       np.Draft,
	}
	for _, l := range uniqueLabels(np.Labels) {
		body.Labels = append(body.Labels, apiLabelName{Name: l})
	}
	var p apiPR
	_, err = t.c.call(ctx, op, http.MethodPost, t.c.repoAPI(t.repo.ID, "pullrequests"), nil, "", body, &p)
	if err != nil {
		if platform.ClassOf(err) == platform.ClassConflict || platform.ClassOf(err) == platform.ClassInvalid && typeKeyOf(err) == keyPRExists {
			return t.existing(ctx, op, np, err)
		}
		return platform.PR{}, err
	}
	if err := p.check(op); err != nil {
		return platform.PR{}, err
	}
	if line != "" {
		if err := t.setMarker(ctx, op, p.ID, line); err != nil {
			return platform.PR{}, err
		}
	}
	if err := t.addLabels(ctx, op, &p, np.Labels); err != nil {
		return platform.PR{}, err
	}
	return t.finish(ctx, op, &p, line, true)
}

// existing returns the active pull request from np.Head in the repository
// itself with ErrExists, after refused, a refusal of CreatePR's POST as a
// duplicate; refused alone when none is active.
func (t *target) existing(ctx context.Context, op string, np platform.NewPR, refused error) (platform.PR, error) {
	match, err := t.activeFrom(ctx, op, np.Head, np.Base)
	if err != nil {
		return platform.PR{}, errors.Join(refused, err)
	}
	if match == nil {
		return platform.PR{}, refused
	}
	return t.exists(ctx, op, match, np.Head, statusOf(refused))
}

// activeFrom returns the active pull request from head in the repository
// itself, the one to base first; nil when none is active. One from a
// fork's branch of that name is not from head.
func (t *target) activeFrom(ctx context.Context, op, head, base string) (*apiPR, error) {
	var match *apiPR
	q := url.Values{"searchCriteria.sourceRefName": {"refs/heads/" + head}, "searchCriteria.status": {statusActive}}
	complete, err := listPages(ctx, t.c, op, t.c.repoAPI(t.repo.ID, "pullrequests"), q, maxPRPages, func(p apiPR) error {
		if err := p.check(op); err != nil {
			return err
		}
		if p.Status != statusActive || p.head() != head || p.ForkSource != nil || !strings.EqualFold(p.Repository.ID, t.repo.ID) {
			return nil
		}
		if match == nil || p.TargetRefName == "refs/heads/"+base && match.TargetRefName != "refs/heads/"+base {
			match = &p
		}
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case !complete && match == nil:
		return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%s has more active pull requests from %s than %d pages", t.repo.Path, head, maxPRPages)}
	}
	return match, nil
}

// exists returns p, the active pull request from head, with the ErrExists
// of CreatePR; status is the HTTP status that told it (0 when a listing
// did).
func (t *target) exists(ctx context.Context, op string, p *apiPR, head string, status int) (platform.PR, error) {
	line, err := t.c.markerOf(ctx, op, t.repo.ID, p.ID)
	if err != nil {
		return platform.PR{}, err
	}
	pr, err := t.finish(ctx, op, p, line, p.cut())
	if err != nil {
		return platform.PR{}, err
	}
	return pr, &platform.Error{Op: op, Class: platform.ClassConflict, Status: status,
		Err: fmt.Errorf("#%d is active from %s: %w", p.ID, head, platform.ErrExists)}
}

// finish converts a pull request the API answered with, as PRs reports it:
// read alone first when alone is set (a listing's cut description), with
// line, its marker, BaseExists and, for an active one from the repository
// itself, its head.
func (t *target) finish(ctx context.Context, op string, p *apiPR, line string, alone bool) (platform.PR, error) {
	if alone {
		full, err := t.c.getPR(ctx, op, t.repo.ID, p.ID)
		if err != nil {
			return platform.PR{}, err
		}
		*p = *full
	}
	pr := t.c.toPR(p, line)
	var err error
	if pr.BaseExists, err = t.c.baseExists(ctx, op, t.repo, pr.Base, map[string]bool{}); err != nil {
		return platform.PR{}, err
	}
	if pr.State == platform.Open && pr.Head != "" && pr.HeadRepoID == pr.RepoID {
		if pr.HeadSHA, err = t.c.branchHead(ctx, op, t.repo.ID, pr.Head); err != nil {
			return platform.PR{}, err
		}
	}
	return pr, nil
}

// setMarker writes line into pull request number's property
// touchmark.marker, or removes the property when line is "" (Pull Request
// Properties - Update: replace adds a missing property, remove of a
// missing one does nothing).
func (t *target) setMarker(ctx context.Context, op string, number int64, line string) error {
	patch := []patchOp{{Op: "replace", Path: "/" + markerProperty, Value: &line}}
	if line == "" {
		patch = []patchOp{{Op: "remove", Path: "/" + markerProperty}}
	}
	_, err := t.c.call(ctx, op, http.MethodPatch, t.c.repoAPI(t.repo.ID, "pullRequests", strconv.FormatInt(number, 10), "properties"),
		nil, "application/json-patch+json", patch, nil)
	return err
}

// addLabels adds to p, by name, each of names it lacks (Pull Request
// Labels - Create), and records them on p.
func (t *target) addLabels(ctx context.Context, op string, p *apiPR, names []string) error {
	have := map[string]bool{}
	for _, l := range p.Labels {
		if l.Active == nil || *l.Active {
			have[strings.ToLower(l.Name)] = true
		}
	}
	for _, name := range uniqueLabels(names) {
		if have[strings.ToLower(name)] {
			continue
		}
		var got apiLabel
		if _, err := t.c.call(ctx, op, http.MethodPost, t.c.repoAPI(t.repo.ID, "pullRequests", strconv.FormatInt(p.ID, 10), "labels"),
			nil, "", apiLabelName{Name: name}, &got); err != nil {
			return err
		}
		have[strings.ToLower(name)] = true
		p.Labels = append(p.Labels, apiLabel{ID: got.ID, Name: name})
	}
	return nil
}

// uniqueLabels returns the non-empty names, each once (ignoring case), in
// order.
func uniqueLabels(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, strings.TrimSpace(n))
		}
	}
	return out
}

// EditPR changes a pull request. It reads the pull request first.
//
//   - Active: the marker line of e.Body goes into the property first (when
//     it differs from the stored one), then the labels to add, then one
//     PATCH with what changes of title, description (e.Body without its
//     marker line), base and, for State Closed, status abandoned: the
//     closing body's marker is stored before the pull request is
//     abandoned, after which touchmark never writes to it again. A failure
//     between them leaves an active pull request with part of the edit,
//     which the next run decides on again.
//   - Abandoned: touchmark never reactivates one, nor changes it
//     (Caps.ClosedImmutable): State Open is ClassUnsupported, and so is any
//     change of title, body or base; State Closed alone changes nothing.
//   - Completed: any change is ClassConflict, as a pull request merged
//     since the core read it.
//
// State Merged is ClassUnsupported: touchmark completes no pull request.
// The draft flag never changes; labels are only added.
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
		return platform.PR{}, unsupported(op, "touchmark completes no pull request")
	case e.State != nil && *e.State != platform.Open && *e.State != platform.Closed:
		return platform.PR{}, invalid(op, "state %q: only open and closed can be set", *e.State)
	case e.Base != nil && *e.Base == "":
		return platform.PR{}, invalid(op, "the base is empty")
	}
	cur, err := t.c.getPR(ctx, op, t.repo.ID, number)
	if err != nil {
		return platform.PR{}, err
	}
	stored, err := t.c.markerOf(ctx, op, t.repo.ID, number)
	if err != nil {
		return platform.PR{}, err
	}
	curDesc := ""
	if cur.Description != nil {
		curDesc = *cur.Description
	}
	var desc, line string
	if e.Body != nil {
		desc, line = marker.Detach(*e.Body)
	}
	base := strings.TrimPrefix(cur.TargetRefName, "refs/heads/")
	newTitle := e.Title != nil && *e.Title != cur.Title
	newDesc := e.Body != nil && desc != curDesc
	newMarker := e.Body != nil && line != stored
	newBase := e.Base != nil && *e.Base != base
	changes := newTitle || newDesc || newMarker || newBase
	switch cur.Status {
	case statusCompleted:
		if changes || e.State != nil {
			return platform.PR{}, conflict(op, "#%d is completed: it can no longer be changed", number)
		}
		return t.finish(ctx, op, cur, stored, false)
	case statusAbandoned:
		switch {
		case e.State != nil && *e.State == platform.Open:
			return platform.PR{}, unsupported(op, "#%d is abandoned: touchmark never reactivates a pull request", number)
		case changes:
			return platform.PR{}, unsupported(op, "#%d is abandoned: touchmark changes active pull requests only", number)
		}
		return t.finish(ctx, op, cur, stored, false)
	}
	if newDesc {
		if err := checkDescription(op, desc); err != nil {
			return platform.PR{}, err
		}
	}
	if newMarker {
		if err := t.setMarker(ctx, op, number, line); err != nil {
			return platform.PR{}, err
		}
		stored = line
	}
	if err := t.addLabels(ctx, op, cur, e.AddLabels); err != nil {
		return platform.PR{}, err
	}
	var patch updatePR
	if newTitle {
		patch.Title = e.Title
	}
	if newDesc {
		patch.Description = &desc
	}
	if newBase {
		ref := "refs/heads/" + *e.Base
		patch.TargetRefName = &ref
	}
	if e.State != nil && *e.State == platform.Closed {
		s := statusAbandoned
		patch.Status = &s
	}
	if patch != (updatePR{}) {
		var got apiPR
		if _, err := t.c.call(ctx, op, http.MethodPatch, t.c.repoAPI(t.repo.ID, "pullrequests", strconv.FormatInt(number, 10)), nil, "", patch, &got); err != nil {
			return platform.PR{}, err
		}
		if err := got.check(op); err != nil {
			return platform.PR{}, err
		}
		if patch.Status != nil && got.Status == statusActive {
			return platform.PR{}, conflict(op, "Azure DevOps did not abandon #%d", number)
		}
		labels := cur.Labels
		cur = &got
		if len(cur.Labels) == 0 {
			cur.Labels = labels
		}
	}
	return t.finish(ctx, op, cur, stored, cur.cut())
}

// Comment adds a comment to pull request number: a new thread with one
// text comment, closed so that it never waits for a resolution (Pull
// Request Threads - Create; a closed thread does not block the policy that
// asks for resolved comments). Azure DevOps takes comments on abandoned and
// completed pull requests too.
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
	thread := map[string]any{
		"comments": []map[string]any{{"parentCommentId": 0, "content": body, "commentType": "text"}},
		"status":   "closed",
	}
	_, err := t.c.call(ctx, op, http.MethodPost, t.c.repoAPI(t.repo.ID, "pullRequests", strconv.FormatInt(number, 10), "threads"), nil, "", thread, nil)
	return err
}

// EnsureLabels returns the names as they are: Azure DevOps creates a label
// when a pull request first gets it by name (Pull Request Labels - Create),
// so there is nothing to create beforehand.
func (t *target) EnsureLabels(_ context.Context, names []string) ([]string, error) {
	if err := t.live("ensure labels"); err != nil {
		return nil, err
	}
	return slices.Clone(uniqueLabels(names)), nil
}
