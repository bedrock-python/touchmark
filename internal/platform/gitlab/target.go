package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Target checks that the writer may write to r and returns a TargetWriter
// bound to it, before the target's writes. The project is read as the
// writer, by id (a rename does not matter): a project it cannot see is
// ClassNotFound. Its access level must be Developer or higher for contents
// and merge requests: the higher of permissions.project_access and
// group_access of the project, else, as those leave out inherited and
// shared memberships, GET /projects/:id/members/all/:user_id. Otherwise
// the error is ClassPermission with Rule "contents" or "pull-requests".
// GitLab has no separate right for CI files, so Workflows needs nothing
// more. An administrator account is refused as in Self.
func (w *writer) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	const op = "target"
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := w.c.remoteURL(op, r); err != nil {
		return nil, err
	}
	p, err := w.c.getProject(ctx, op, projectID(r))
	if err != nil {
		return nil, err
	}
	level := 0
	if p.Permissions != nil {
		if a := p.Permissions.ProjectAccess; a != nil {
			level = max(level, a.AccessLevel)
		}
		if a := p.Permissions.GroupAccess; a != nil {
			level = max(level, a.AccessLevel)
		}
	}
	if level < accessDeveloper && (need.Contents || need.PRs) {
		var m apiAccess
		_, err := w.c.get(ctx, op, w.c.projectURL(strconv.FormatInt(p.ID, 10), "members", "all", self.ID), nil, &m)
		switch {
		case err == nil:
			level = max(level, m.AccessLevel)
		case platform.ClassOf(err) == platform.ClassNotFound:
			// Not a member, directly or through a group.
		default:
			return nil, err
		}
	}
	switch {
	case need.Contents && level < accessDeveloper:
		return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "contents",
			Err: fmt.Errorf("the writer has access level %d on %s, not Developer (30) or higher: it may not push", level, p.PathWithNamespace)}
	case need.PRs && level < accessDeveloper:
		return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: "pull-requests",
			Err: fmt.Errorf("the writer has access level %d on %s, not Developer (30) or higher: it may not write merge requests", level, p.PathWithNamespace)}
	}
	repo := w.c.toRepo(p)
	remote, err := w.c.remoteURL(op, repo)
	if err != nil {
		return nil, err
	}
	return &target{c: w.c, repo: repo, id: repo.ID, remote: remote, prsDisabled: repo.PRsDisabled}, nil
}

// target is a platform.TargetWriter. The writer's token is not narrowed:
// Close retires the TargetWriter, and later calls through it fail with
// ClassAuth, as after a revoked per-target token elsewhere.
type target struct {
	c           *client
	repo        platform.Repo
	id          string // the numeric project id
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

// Remote is the project's URL with the writer's Basic header, which fails
// with ClassAuth after Close.
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

// createMR is the body of POST /projects/:id/merge_requests
// (https://docs.gitlab.com/api/merge_requests/#create-mr).
type createMR struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	Labels             string `json:"labels,omitempty"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
	Squash             bool   `json:"squash"`
}

// editMR is the body of PUT /projects/:id/merge_requests/:iid
// (https://docs.gitlab.com/api/merge_requests/#update-mr): nil fields stay
// as they are. add_labels adds and never removes (labels would replace).
type editMR struct {
	Title        *string `json:"title,omitempty"`
	Description  *string `json:"description,omitempty"`
	StateEvent   string  `json:"state_event,omitempty"`
	TargetBranch *string `json:"target_branch,omitempty"`
	AddLabels    string  `json:"add_labels,omitempty"`
}

// note is the body of POST /projects/:id/merge_requests/:iid/notes.
type note struct {
	Body string `json:"body"`
}

// CreatePR opens a merge request from np.Head in the project itself, with
// its labels by name (GitLab creates the missing ones as project labels).
// A draft gets the "Draft: " title prefix. The body must not run a quick
// action (checkText).
//
// An open MR from the same head in the project itself, to whatever target
// branch, is returned with ErrExists before anything is written: GitLab
// refuses (409) only a second one to the same target. The 409 of a
// duplicate opened meanwhile ("Another open merge request already exists
// for this source branch: !N") gives the same: !N is read and returned.
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
			Err: fmt.Errorf("merge requests are disabled in %s", t.repo.Path)}
	}
	if err := checkText(op, "the description", np.Body); err != nil {
		return platform.PR{}, err
	}
	labels, err := labelList(op, np.Labels)
	if err != nil {
		return platform.PR{}, err
	}
	title := np.Title
	switch {
	case np.Draft && !hasDraftPrefix(title):
		title = draftPrefix + title
	case !np.Draft && hasDraftPrefix(title):
		return platform.PR{}, invalid(op, "the title %q starts with a draft prefix: GitLab would open a draft", np.Title)
	}
	open, err := t.openFrom(ctx, op, np)
	if err != nil {
		return platform.PR{}, err
	}
	if open != nil {
		return t.exists(ctx, op, open, np.Head, 0)
	}
	body := createMR{SourceBranch: np.Head, TargetBranch: np.Base, Title: title, Description: np.Body, Labels: labels}
	m, err := t.post(ctx, op, body)
	if isStatus(err, http.StatusBadRequest) && sourceBranchMissing(err) {
		return platform.PR{}, t.unregistered(ctx, op, body.SourceBranch, err)
	}
	if isStatus(err, http.StatusConflict) {
		return t.existing(ctx, op, np, err)
	}
	if err != nil {
		return platform.PR{}, err
	}
	if err := m.check(op); err != nil {
		return platform.PR{}, err
	}
	return toPR(m, true), nil
}

// sourceBranchMissing reports whether err, a 400 of CreatePR, says the
// source branch does not exist ({"message": {"source_branch": ["does not
// exist"]}}, flattened by parseMessage).
func sourceBranchMissing(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "source_branch does not exist") || strings.Contains(msg, "source branch does not exist")
}

// post sends POST /projects/:id/merge_requests once.
func (t *target) post(ctx context.Context, op string, body createMR) (*apiMR, error) {
	var m apiMR
	if _, err := t.c.call(ctx, op, http.MethodPost, t.c.projectURL(t.id, "merge_requests"), nil, body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// pushRetry is the wait CreatePR asks for when GitLab has not registered
// the push of its source branch yet (unregistered): the core sends the
// POST again after it, and after growing waits, for a bounded time.
const pushRetry = 500 * time.Millisecond

// unregistered handles createErr, a 400 "source_branch does not exist" of
// CreatePR's POST, which follows touchmark's push of branch.
//
// GitLab checks the source branch of a new merge request against the
// project's cached branch names (MergeRequest#validate_branch_existence,
// Repository#branch_exists?, a Redis set), and the push's background job
// (PostReceive, Git::BranchPushService) refreshes that cache after the push
// was accepted. Until then a branch that was pushed is "source_branch does
// not exist" to the merge request API, while the branches API, which asks
// Gitaly, finds it. Measured live on CE 19.4.1 under load (TestFacts,
// push-then-create): every POST right after a push was refused, the merge
// request opened 1.5 to 10 s later.
//
// So the branch is looked up in the branches API: when it does not exist,
// or the lookup fails for good, createErr stands; a lookup that was rate
// limited, refused the credential, failed transiently or ran out of ctx
// returns that failure (joined with createErr), so the core pauses or
// retries instead of failing the target. When it does exist, the error is
// ClassTransient with platform.ErrNotYet and a RetryAfter of pushRetry: the
// refused POST created nothing, and the core sends it again after a pause
// (a duplicate would be a 409, which CreatePR resolves). The driver waits
// for nothing itself, as drivers never retry or pace.
func (t *target) unregistered(ctx context.Context, op, branch string, createErr error) error {
	ok, err := t.c.branchExists(ctx, op, t.id, branch)
	switch {
	case err != nil && fatal(err):
		// A rate limit, a refused credential, a transient failure or the
		// end of ctx: the core pauses or retries on that, not on the 400.
		return errors.Join(err, createErr)
	case err != nil || !ok:
		return createErr
	}
	return &platform.Error{Op: op, Class: platform.ClassTransient, Status: statusOf(createErr), RetryAfter: pushRetry,
		Err: fmt.Errorf("GitLab has not registered the push of %s yet (the branch exists, its branch cache does not show it): %w: %w",
			branch, platform.ErrNotYet, createErr)}
}

// duplicateRef finds "!N" in the message of a 409 of CreatePR.
var duplicateRef = regexp.MustCompile(`already exists for this source branch: !(\d+)`)

// existing returns the open MR from np.Head in the project itself (!N of
// conflict's message, else one found by listing) with ErrExists, after
// conflict, a 409 of CreatePR; conflict alone when none is open any more.
func (t *target) existing(ctx context.Context, op string, np platform.NewPR, conflictErr error) (platform.PR, error) {
	if sm := duplicateRef.FindStringSubmatch(conflictErr.Error()); sm != nil {
		if iid, err := strconv.ParseInt(sm[1], 10, 64); err == nil {
			m, err := t.c.getMR(ctx, op, t.id, iid)
			switch {
			case err == nil && t.ownOpen(m, np.Head):
				return t.exists(ctx, op, m, np.Head, http.StatusConflict)
			case err != nil && platform.ClassOf(err) != platform.ClassNotFound:
				return platform.PR{}, errors.Join(conflictErr, err)
			}
		}
	}
	match, err := t.openFrom(ctx, op, np)
	if err != nil {
		return platform.PR{}, errors.Join(conflictErr, err)
	}
	if match == nil {
		return platform.PR{}, conflictErr
	}
	return t.exists(ctx, op, match, np.Head, http.StatusConflict)
}

// ownOpen reports whether m is an open MR from head in the project itself.
func (t *target) ownOpen(m *apiMR, head string) bool {
	return m.State == "opened" && m.SourceBranch == head &&
		strconv.FormatInt(m.SourceProjectID, 10) == t.id && strconv.FormatInt(m.TargetProjectID, 10) == t.id
}

// openFrom returns the open MR from np.Head in the project itself, the one
// to np.Base first; nil when none is open. An MR from a fork's branch of
// that name is not from np.Head.
func (t *target) openFrom(ctx context.Context, op string, np platform.NewPR) (*apiMR, error) {
	var match *apiMR
	err := t.c.listMRs(ctx, op, t.id, url.Values{"source_branch": {np.Head}, "state": {"opened"}}, func(m apiMR) error {
		if err := m.check(op); err != nil {
			return err
		}
		if !t.ownOpen(&m, np.Head) {
			return nil
		}
		if match == nil || m.TargetBranch == np.Base && match.TargetBranch != np.Base {
			match = &m
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return match, nil
}

// exists returns m, the open MR from head, with the ErrExists of CreatePR;
// status is the HTTP status that told it (0 when a listing did).
func (t *target) exists(ctx context.Context, op string, m *apiMR, head string, status int) (platform.PR, error) {
	base := true
	if m.TargetBranch != t.repo.DefaultBranch {
		ok, err := t.c.branchExists(ctx, op, t.id, m.TargetBranch)
		if err != nil {
			return platform.PR{}, err
		}
		base = ok
	}
	return toPR(m, base), &platform.Error{Op: op, Class: platform.ClassConflict, Status: status,
		Err: fmt.Errorf("!%d is open from %s: %w", m.IID, head, platform.ErrExists)}
}

// EditPR changes a merge request so that a refusal changes nothing.
//
// GitLab runs a PUT's state_event before it assigns title and description,
// and a close or reopen it cannot do is no error: the rest of the PUT
// applies all the same (see the package doc). So EditPR reads the MR
// first and refuses (ClassConflict) a state change a merged MR cannot take
// and any edit of a locked MR (a merge in progress), and (ClassInvalid) a
// base branch that does not exist: GitLab checks it on create only. A
// reopen goes first, in a PUT of its
// own, and must take effect before anything else is sent; then title,
// description, base, labels (add_labels, never removing one) and a close
// go in one PUT, so touchmark closes its own MRs with the new body in one
// request. A new title keeps the draft prefix of a draft,
// and a title with a draft prefix is refused for a ready MR, so the draft
// state never changes. The description must not run a quick action.
func (t *target) EditPR(ctx context.Context, number int64, e platform.PREdit) (platform.PR, error) {
	const op = "edit pull request"
	if err := t.live(op); err != nil {
		return platform.PR{}, err
	}
	switch {
	case number <= 0:
		return platform.PR{}, notFound(op, "merge request !%d", number)
	case e.Title != nil && strings.TrimSpace(*e.Title) == "":
		return platform.PR{}, invalid(op, "the title is empty")
	case e.State != nil && *e.State != platform.Open && *e.State != platform.Closed:
		return platform.PR{}, invalid(op, "state %q: only open and closed can be set", *e.State)
	case e.Base != nil && *e.Base == "":
		return platform.PR{}, invalid(op, "the base is empty")
	}
	if e.Body != nil {
		if err := checkText(op, "the description", *e.Body); err != nil {
			return platform.PR{}, err
		}
	}
	labels, err := labelList(op, e.AddLabels)
	if err != nil {
		return platform.PR{}, err
	}
	cur, err := t.c.getMR(ctx, op, t.id, number)
	if err != nil {
		return platform.PR{}, err
	}
	if cur.State == "locked" {
		// GitLab is merging it: nothing is written into a merge.
		return platform.PR{}, conflict(op, "!%d is locked: GitLab is merging it", number)
	}
	reopen, closing := false, false
	if e.State != nil {
		switch {
		case *e.State == platform.Open && cur.State == "opened", *e.State == platform.Closed && cur.State == "closed":
		case cur.State == "merged":
			return platform.PR{}, conflict(op, "!%d is %s: it cannot be %s", number, cur.State, map[platform.PRState]string{platform.Open: "reopened", platform.Closed: "closed"}[*e.State])
		case *e.State == platform.Open:
			reopen = true
		default:
			closing = true
		}
	}
	put := editMR{Description: e.Body, AddLabels: labels}
	if e.Base != nil && *e.Base != cur.TargetBranch {
		ok, err := t.c.branchExists(ctx, op, t.id, *e.Base)
		if err != nil {
			return platform.PR{}, err
		}
		if !ok {
			return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassInvalid, Status: http.StatusNotFound,
				Err: fmt.Errorf("the base branch %q does not exist", *e.Base)}
		}
		put.TargetBranch = e.Base
	}
	if e.Title != nil {
		title := *e.Title
		switch {
		case cur.draft():
			title = keepDraftPrefix(cur.Title, title)
		case hasDraftPrefix(title):
			return platform.PR{}, invalid(op, "the title %q starts with a draft prefix: GitLab would make !%d a draft", *e.Title, number)
		}
		put.Title = &title
	}
	if reopen {
		// GitLab 17 and 18 reopen an MR whose source branch is gone, 19
		// refuses with 422 (see the package doc): an MR without its
		// branches is not reopened on any version.
		for _, b := range []string{cur.SourceBranch, cur.TargetBranch} {
			if b == t.repo.DefaultBranch {
				continue
			}
			ok, err := t.c.branchExists(ctx, op, t.id, b)
			if err != nil {
				return platform.PR{}, err
			}
			if !ok {
				return platform.PR{}, conflict(op, "!%d cannot be reopened: its branch %q is gone", number, b)
			}
		}
		got, err := t.put(ctx, op, number, editMR{StateEvent: "reopen"})
		if isStatus(err, http.StatusUnprocessableEntity) {
			// Refused in the meantime (a branch deleted, another open MR on
			// the same branches): nothing was changed.
			return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassConflict, Status: http.StatusUnprocessableEntity,
				Err: fmt.Errorf("GitLab did not reopen !%d: %w", number, err)}
		}
		if err != nil {
			return platform.PR{}, err
		}
		if got.State != "opened" {
			// Refused without an error: a missing source or target branch,
			// or another open MR on the same branches.
			return platform.PR{}, conflict(op, "GitLab did not reopen !%d (it is %s): its branches are gone or another MR is open on them", number, got.State)
		}
		cur = got
	}
	if closing {
		put.StateEvent = "close"
	}
	if put != (editMR{}) {
		got, err := t.put(ctx, op, number, put)
		if err != nil {
			return platform.PR{}, err
		}
		switch {
		case closing && got.State != "closed":
			return platform.PR{}, conflict(op, "GitLab did not close !%d (it is %s)", number, got.State)
		case put.TargetBranch != nil && got.TargetBranch != *put.TargetBranch:
			return platform.PR{}, conflict(op, "GitLab did not change the base of !%d to %q", number, *put.TargetBranch)
		}
		cur = got
	}
	base := true
	if cur.TargetBranch != t.repo.DefaultBranch && (put.TargetBranch == nil || *put.TargetBranch != cur.TargetBranch) {
		if base, err = t.c.branchExists(ctx, op, t.id, cur.TargetBranch); err != nil {
			return platform.PR{}, err
		}
	}
	pr := toPR(cur, base)
	if err := t.c.finishPR(ctx, op, &pr); err != nil {
		return platform.PR{}, err
	}
	return pr, nil
}

// put sends one PUT of merge request number and returns the MR it answers
// with.
func (t *target) put(ctx context.Context, op string, number int64, body editMR) (*apiMR, error) {
	var got apiMR
	if _, err := t.c.call(ctx, op, http.MethodPut, t.c.projectURL(t.id, "merge_requests", strconv.FormatInt(number, 10)), nil, body, &got); err != nil {
		return nil, err
	}
	if err := got.check(op); err != nil {
		return nil, err
	}
	return &got, nil
}

// Comment adds a note to merge request number. The body must not run a
// quick action (checkText).
func (t *target) Comment(ctx context.Context, number int64, body string) error {
	const op = "comment"
	if err := t.live(op); err != nil {
		return err
	}
	switch {
	case number <= 0:
		return notFound(op, "merge request !%d", number)
	case strings.TrimSpace(body) == "":
		return invalid(op, "the comment is empty")
	}
	if err := checkText(op, "the comment", body); err != nil {
		return err
	}
	_, err := t.c.call(ctx, op, http.MethodPost, t.c.projectURL(t.id, "merge_requests", strconv.FormatInt(number, 10), "notes"), nil, note{Body: body}, nil)
	return err
}

// checkText refuses text with a line whose first character, after blanks
// and invisible format characters, is "/": GitLab runs such a line of a
// description or a note as a quick action of the writer's, through the API
// too, on create and on update (IssuableBaseService#handle_quick_actions;
// https://docs.gitlab.com/user/project/quick_actions/). The core never
// sends one (prbody escapes them); this guard makes sure no hub text does
// anything in a target. Every line break GitLab or Markdown may take for
// one splits lines here.
func checkText(op, what, text string) error {
	line, start := 1, 0
	for i, r := range text {
		if isLineBreak(r) {
			if slashLine(text[start:i]) {
				return quickActionError(op, what, line)
			}
			line++
			start = i + len(string(r))
		}
	}
	if slashLine(text[start:]) {
		return quickActionError(op, what, line)
	}
	return nil
}

func quickActionError(op, what string, line int) error {
	return &platform.Error{Op: op, Class: platform.ClassInvalid, Rule: "quick-action",
		Err: fmt.Errorf("line %d of %s starts with \"/\" and would run as a GitLab quick action; touchmark does not send it", line, what)}
}

// isLineBreak reports whether r ends a line.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// slashLine reports whether line starts with "/" after blanks and
// invisible format characters.
func slashLine(line string) bool {
	t := strings.TrimLeftFunc(line, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) })
	return strings.HasPrefix(t, "/")
}

// Draft prefixes GitLab recognizes at the start of a title, ignoring case
// (MergeRequest::DRAFT_REGEX, https://docs.gitlab.com/user/project/merge_requests/drafts/).
var draftPrefixes = []string{"Draft:", "[Draft]", "(Draft)"}

// hasDraftPrefix reports whether title starts with a draft prefix.
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
// title: the one it has, or draftPrefix when it has none GitLab knows.
func keepDraftPrefix(current, title string) string {
	if hasDraftPrefix(title) {
		return title
	}
	if p := draftPrefixOf(current); p != "" {
		return p + title
	}
	return draftPrefix + title
}
