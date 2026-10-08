package fake

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// Writer returns the platform.Writer of account as. It reads like Reader(as)
// and writes where Grant or GrantWrite allowed it.
func (p *Platform) Writer(as platform.Account) platform.Writer {
	w := writer{reader{p: p, as: as}}
	switch {
	case p.preflight && p.pushGuard:
		return &preflightGuardWriter{preflightWriter{w}}
	case p.preflight:
		return &preflightWriter{w}
	case p.pushGuard:
		return &guardWriter{w}
	}
	return &w
}

type writer struct{ reader }

// Target mints a per-target writer when the account holds every permission
// in need on r (Workflows only counts on flavors with Caps.WorkflowPerm);
// otherwise it fails with ClassPermission and Rule "contents",
// "pull-requests" or "workflows". The writer can do only what need allowed:
// pull requests, comments and labels need PRs, and in git mode the token of
// its Remote is bound to r and need the way GitHub binds an installation
// token to repository_ids and permissions (see target.Remote).
func (w *writer) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	op := ops["Target"]
	return do(ctx, w.p, &w.as, "Target", []string{r.Path}, func() (platform.TargetWriter, error) {
		p := w.p
		s, err := p.repoOf(op, r)
		if err != nil {
			return nil, err
		}
		if err := p.allowed(op, s, w.as, need); err != nil {
			return nil, err
		}
		p.tokens++
		token := "fake-token-" + w.as.ID + "-" + s.repo.ID + "-" + strconv.FormatInt(p.tokens, 10)
		tw := &target{p: p, as: w.as, repoID: s.repo.ID, perms: need, token: token}
		p.targetTokens[token] = tw
		if p.apiCommits {
			return apiTarget{tw}, nil
		}
		return tw, nil
	})
}

// allowed checks that account holds need on s.
func (p *Platform) allowed(op string, s *repoState, account platform.Account, need platform.Perms) error {
	have := s.grants[account.ID]
	var missing string
	switch {
	case need.Contents && !have.Contents:
		missing = "contents"
	case need.PRs && !have.PRs:
		missing = "pull-requests"
	case need.Workflows && p.caps.WorkflowPerm && !have.Workflows:
		missing = "workflows"
	default:
		return nil
	}
	return denied(op, missing, "%s may not write %s of %s", account.Login, missing, s.repo.Path)
}

// target is a platform.TargetWriter.
type target struct {
	p      *Platform
	as     platform.Account
	repoID string
	perms  platform.Perms
	token  string
	closed bool // guarded by p.mu
}

// Remote returns the repository URL (the git server's in git mode) with a
// header of the writer's own per-target token ("Basic " +
// base64("x-access-token:<token>")); after Close the header fails with
// ClassAuth.
//
// In git mode the server holds the token to what Target minted it for, on
// every flavor (stricter than a GitLab or Gitea token, which is not
// narrowed; delivery must use each target's own writer anyway): only the
// target's repository (404 elsewhere); a push only when need had Contents
// and the account still holds it (403 otherwise, GitHub's "Permission to …
// denied"); on flavors with Caps.WorkflowPerm, no created or changed file
// under .github/workflows unless need had Workflows and the account still
// holds it (the push is rejected with GitHub's "refusing to allow a GitHub
// App to create or update workflow"); nothing after Close (401).
func (t *target) Remote() platform.Remote {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	var url string
	if s := t.p.repos[t.repoID]; s != nil {
		url = t.p.remoteURL(s)
	}
	return platform.Remote{URL: url, Header: t.header}
}

func (t *target) header(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if t.closed {
		return "", revoked("git")
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(targetLogin+":"+t.token)), nil
}

// targetLogin is the user name of per-target tokens, as on GitHub.
const targetLogin = "x-access-token"

// write runs a write method: start as the account, then the checks every
// write shares (a live token, permission for pull requests, a repository
// that is not archived), then body.
func write[T any](ctx context.Context, t *target, method string, args []string, body func(s *repoState) (T, error)) (T, error) {
	op := ops[method]
	return do(ctx, t.p, &t.as, method, args, func() (T, error) {
		var zero T
		p := t.p
		s := p.repos[t.repoID]
		switch {
		case t.closed:
			return zero, revoked(op)
		case s == nil:
			return zero, notFound(op, "repository id %s", t.repoID)
		case !t.perms.PRs:
			return zero, denied(op, "pull-requests", "the token of %s was minted without pull requests", s.repo.Path)
		}
		if err := p.allowed(op, s, t.as, platform.Perms{PRs: true}); err != nil {
			return zero, err
		}
		if s.repo.Archived {
			return zero, denied(op, "archived", "%s is archived", s.repo.Path)
		}
		return body(s)
	})
}

// repoArg is the repository path of the call log.
func (t *target) repoArg() string {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if s := t.p.repos[t.repoID]; s != nil {
		return s.repo.Path
	}
	return t.repoID
}

// CreatePR opens a pull request from np.Head in the repository itself.
// Head, Base and Title are required, Head differs from Base; the body must
// fit Caps.MaxBody and, with Caps.QuickActions, have no line that starts
// with "/". A draft on a title-prefix flavor gets Caps.DraftPrefix before
// its title. Missing labels are created (a CreateLabel entry each).
//
// In git mode both branches must exist, GitHub refuses a head without
// commits its base lacks, HeadSHA is the head's tip, and GitLab runs the
// body's quick actions instead of refusing them.
func (t *target) CreatePR(ctx context.Context, np platform.NewPR) (platform.PR, error) {
	first, then := t.createWrites(np)
	if err := t.p.meterWrites(ctx, "CreatePR", throttle.API, first); err != nil {
		return platform.PR{}, err
	}
	pr, err := t.createPR(ctx, np)
	if err == nil {
		// The calls that follow the pull request's own; a refusal of the
		// meter only leaves them uncounted: the pull request exists.
		_ = t.p.meterWrites(ctx, "CreatePR", throttle.API, then)
	}
	return pr, err
}

// createPR is CreatePR without the meter.
func (t *target) createPR(ctx context.Context, np platform.NewPR) (platform.PR, error) {
	op := ops["CreatePR"]
	return write(ctx, t, "CreatePR", []string{t.repoArg()}, func(s *repoState) (platform.PR, error) {
		p := t.p
		switch {
		case np.Head == "" || np.Base == "":
			return platform.PR{}, invalid(op, "head and base are required")
		case np.Head == np.Base:
			return platform.PR{}, invalid(op, "head and base are both %q", np.Head)
		case strings.TrimSpace(np.Title) == "":
			return platform.PR{}, invalid(op, "the title is empty")
		}
		if err := p.checkText(op, np.Body); err != nil {
			return platform.PR{}, err
		}
		if err := t.p.checkLabels(op, np.Labels); err != nil {
			return platform.PR{}, err
		}
		if s.repo.PRsDisabled {
			return platform.PR{}, &platform.Error{Op: op, Class: platform.ClassPolicy, Status: http.StatusForbidden,
				Rule: "prs-disabled", Err: fmt.Errorf("pull requests are disabled in %s", s.repo.Path)}
		}
		for _, n := range sortedNumbers(s) {
			pr := s.prs[n].pr
			if pr.State == platform.Open && pr.Head == np.Head && pr.HeadRepoID == s.repo.ID {
				status := http.StatusUnprocessableEntity
				if p.caps.Flavor == string(GitLab) {
					status = http.StatusConflict
				}
				return p.prView(pr), &platform.Error{Op: op, Class: platform.ClassConflict, Status: status,
					Err: fmt.Errorf("#%d is open from %s: %w", pr.Number, np.Head, platform.ErrExists)}
			}
		}
		if p.git != nil {
			if err := p.gitNewPR(ctx, op, s, np); err != nil {
				return platform.PR{}, err
			}
		}
		labels := dedupe(np.Labels)
		for _, l := range labels {
			p.label(s, l, true)
		}
		s.lastPR++
		pr := platform.PR{
			Number:     s.lastPR,
			URL:        p.prURL(s, s.lastPR),
			State:      platform.Open,
			Draft:      np.Draft,
			Head:       np.Head,
			Base:       np.Base,
			RepoID:     s.repo.ID,
			HeadRepoID: s.repo.ID,
			BaseExists: true,
			Title:      p.draftTitle(np.Title, np.Draft),
			Body:       np.Body,
			Labels:     labels,
			Author:     t.as,
			CreatedAt:  p.now(),
			HeadSHA:    s.refs[np.Head],
		}
		ps := &prState{pr: pr}
		s.prs[pr.Number] = ps
		if p.git != nil {
			p.opened(s.repo.ID, np.Head)
			if p.caps.QuickActions {
				p.runQuickActions(s, ps, np.Body, t.as, "the description")
			}
		}
		return p.prView(ps.pr), nil
	})
}

// EditPR applies every field of e in one step, after checking them all:
// a title keeps the draft prefix of a draft on a title-prefix flavor, a
// state is Open or Closed (a merged PR keeps its state; closing records the
// writer as the closer), a new base is taken to exist, labels are only
// added. Draft never changes.
//
// With Caps.ClosedImmutable (Bitbucket Cloud) a PR closed without merging
// is final, as Bitbucket's driver reports it: reopening it, or changing its
// title, body or base, is ClassUnsupported and changes nothing; Closed
// alone changes nothing. An open PR's new body is written before it closes,
// in the same call.
//
// In git mode a new base must exist and reopening needs the head branch
// (HeadSHA follows it again); after a new base or a reopening the flavor
// reacts as after a push, and GitLab runs the body's quick actions.
func (t *target) EditPR(ctx context.Context, number int64, e platform.PREdit) (platform.PR, error) {
	first, then := t.editWrites(e)
	if err := t.p.meterWrites(ctx, "EditPR", throttle.API, first); err != nil {
		return platform.PR{}, err
	}
	pr, err := t.editPR(ctx, number, e)
	if err == nil {
		_ = t.p.meterWrites(ctx, "EditPR", throttle.API, then)
	}
	return pr, err
}

// editPR is EditPR without the meter.
func (t *target) editPR(ctx context.Context, number int64, e platform.PREdit) (platform.PR, error) {
	op := ops["EditPR"]
	return write(ctx, t, "EditPR", []string{t.repoArg(), "#" + strconv.FormatInt(number, 10)}, func(s *repoState) (platform.PR, error) {
		p := t.p
		ps := s.prs[number]
		if ps == nil {
			return platform.PR{}, notFound(op, "%s: pull request #%d", s.repo.Path, number)
		}
		pr := ps.pr
		if e.Title != nil && strings.TrimSpace(*e.Title) == "" {
			return platform.PR{}, invalid(op, "the title is empty")
		}
		if e.Body != nil {
			if err := p.checkText(op, *e.Body); err != nil {
				return platform.PR{}, err
			}
		}
		if e.State != nil {
			switch {
			case *e.State != platform.Open && *e.State != platform.Closed:
				return platform.PR{}, invalid(op, "state %q: only open and closed can be set", *e.State)
			case pr.State == platform.Merged:
				return platform.PR{}, invalid(op, "#%d is merged", number)
			}
		}
		if e.Base != nil && (*e.Base == "" || *e.Base == pr.Head) {
			return platform.PR{}, invalid(op, "base %q is empty or the head", *e.Base)
		}
		if p.caps.ClosedImmutable && pr.State == platform.Closed {
			changes := e.Title != nil && p.draftTitle(*e.Title, pr.Draft) != pr.Title ||
				e.Body != nil && *e.Body != pr.Body || e.Base != nil && *e.Base != pr.Base
			switch {
			case e.State != nil && *e.State == platform.Open:
				return platform.PR{}, unsupported(op, "#%d: a declined pull request cannot be reopened", number)
			case changes:
				return platform.PR{}, unsupported(op, "#%d is declined: only open pull requests can be changed", number)
			}
		}
		if err := t.p.checkLabels(op, e.AddLabels); err != nil {
			return platform.PR{}, err
		}
		reopen := e.State != nil && *e.State == platform.Open && pr.State == platform.Closed
		if p.git != nil {
			if e.Base != nil && s.refs[*e.Base] == "" {
				return platform.PR{}, invalid(op, "the base branch %q does not exist in %s", *e.Base, s.repo.Path)
			}
			if reopen && p.headTip(pr) == "" {
				return platform.PR{}, invalid(op, "#%d cannot be reopened: its head branch %q is gone", number, pr.Head)
			}
		}
		if e.Title != nil {
			pr.Title = p.draftTitle(*e.Title, pr.Draft)
		}
		if e.Body != nil {
			pr.Body = *e.Body
		}
		if e.Base != nil {
			pr.Base, pr.BaseExists = *e.Base, true
		}
		if e.State != nil && *e.State != pr.State {
			if *e.State == platform.Closed {
				closer := t.as
				pr.State, pr.ClosedBy, pr.ClosedAt = platform.Closed, &closer, p.now()
			} else {
				pr.State, pr.ClosedBy, pr.ClosedAt = platform.Open, nil, time.Time{}
				if tip := p.headTip(pr); tip != "" {
					pr.HeadSHA = tip
				}
			}
		}
		pr.Labels = slices.Clone(pr.Labels)
		for _, l := range dedupe(e.AddLabels) {
			p.label(s, l, true)
			if !slices.Contains(pr.Labels, l) {
				pr.Labels = append(pr.Labels, l)
			}
		}
		ps.pr = pr
		if p.git != nil {
			if e.Base != nil || reopen {
				p.settle(ctx, prRef{repo: s, ps: ps}, &t.as)
			}
			if e.Body != nil && p.caps.QuickActions {
				p.runQuickActions(s, ps, *e.Body, t.as, "the description")
			}
		}
		return p.prView(ps.pr), nil
	})
}

// Comment adds a comment by the writer. The body must not be blank, must
// fit Caps.MaxBody and, with Caps.QuickActions, have no line that starts
// with "/"; in git mode GitLab runs such lines as quick actions instead.
func (t *target) Comment(ctx context.Context, number int64, body string) error {
	op := ops["Comment"]
	if err := t.p.meterWrites(ctx, "Comment", throttle.Comment, 1); err != nil {
		return err
	}
	_, err := write(ctx, t, "Comment", []string{t.repoArg(), "#" + strconv.FormatInt(number, 10)}, func(s *repoState) (struct{}, error) {
		p := t.p
		ps := s.prs[number]
		if ps == nil {
			return struct{}{}, notFound(op, "%s: pull request #%d", s.repo.Path, number)
		}
		if strings.TrimSpace(body) == "" {
			return struct{}{}, invalid(op, "the comment is empty")
		}
		if err := p.checkText(op, body); err != nil {
			return struct{}{}, err
		}
		ps.comments = append(ps.comments, Comment{Author: t.as, Body: body, CreatedAt: p.now()})
		if p.git != nil && p.caps.QuickActions {
			p.runQuickActions(s, ps, body, t.as, "a comment")
		}
		return struct{}{}, nil
	})
	return err
}

// EnsureLabels returns one id per name, creating missing labels (a
// CreateLabel entry each): numeric ids on flavors with Caps.LabelsByID,
// the names elsewhere.
func (t *target) EnsureLabels(ctx context.Context, names []string) ([]string, error) {
	op := ops["EnsureLabels"]
	if err := t.p.meterWrites(ctx, "EnsureLabels", throttle.API, t.missingLabels(names)); err != nil {
		return nil, err
	}
	return write(ctx, t, "EnsureLabels", []string{t.repoArg()}, func(s *repoState) ([]string, error) {
		if err := t.p.checkLabels(op, names); err != nil {
			return nil, err
		}
		ids := make([]string, len(names))
		for i, name := range names {
			ids[i] = t.p.label(s, name, true)
		}
		return ids, nil
	})
}

// Close revokes the per-target token: later calls fail with ClassAuth.
// Closing twice is fine. Faults injected for "Close" apply.
func (t *target) Close() error {
	p := t.p
	p.mu.Lock()
	defer p.mu.Unlock()
	path := t.repoID
	if s := p.repos[t.repoID]; s != nil {
		path = s.repo.Path
	}
	p.logCall("Close", path)
	if f, ok := p.takeFault("Close"); ok {
		if f.applied {
			t.closed = true
		}
		return f.err
	}
	t.closed = true
	return nil
}

// draftTitle puts the draft prefix before title for a draft on a
// title-prefix flavor, unless it is there already.
func (p *Platform) draftTitle(title string, draft bool) string {
	prefix := p.caps.DraftPrefix
	if !draft || p.caps.Draft != platform.DraftTitlePrefix || prefix == "" || hasPrefixFold(title, strings.TrimSpace(prefix)) {
		return title
	}
	return prefix + title
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// checkText checks a body or comment against the flavor: at most
// Caps.MaxBody bytes and, with Caps.QuickActions, no line whose first
// non-blank character is "/" (in memory mode: git mode runs them).
func (p *Platform) checkText(op, text string) error {
	if limit := p.caps.MaxBody; limit > 0 && len(text) > limit {
		return invalid(op, "the text has %d bytes, more than %d", len(text), limit)
	}
	if p.caps.QuickActions && p.git == nil {
		for i, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimLeft(line, " \t\r"), "/") {
				return invalid(op, "line %d starts with \"/\" and would run as a quick action", i+1)
			}
		}
	}
	return nil
}

// checkLabels refuses blank label names, and any label on a flavor
// without labels (Caps.NoLabels), where the core must ask for none.
func (p *Platform) checkLabels(op string, names []string) error {
	if p.caps.NoLabels && len(names) > 0 {
		return invalid(op, "labels %q on a platform without labels", names)
	}
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return invalid(op, "blank label name")
		}
	}
	return nil
}

func sortedNumbers(s *repoState) []int64 {
	out := make([]int64, 0, len(s.prs))
	for n := range s.prs {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func revoked(op string) error {
	return &platform.Error{Op: op, Class: platform.ClassAuth, Status: http.StatusUnauthorized,
		Err: errors.New("the per-target token was revoked")}
}
