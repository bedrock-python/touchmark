package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Git mode: the fake keeps every repository as a real bare git repository
// served by `git http-backend` (net/http/cgi) on a local test server, so
// gitx can fetch from and push to it through Remote, and PRs follow their
// branches like on real platforms.
//
// In git mode:
//   - SetFile, SetSymlink, SetGitlink and RemoveFile commit to the default
//     branch (author and committer "fake", dated by the platform clock);
//     an unchanged entry makes no commit. Head is the default branch's
//     commit, and a repository is Empty while that branch has no commit.
//   - ReadFile and Snapshots read the bare repository at "" (the default
//     branch), a branch name, "refs/heads/<branch>" or a full commit id; an
//     unknown ref is ClassNotFound.
//   - Reader.Remote, Writer.Remote and TargetWriter.Remote return
//     http://127.0.0.1:<port>/<repository path>.git. Reader and Writer send
//     the account's token (SetToken; no Header without one), in a read-only
//     form on GitHub-like flavors, where Apps read with installation tokens
//     and write through Target. A TargetWriter sends its
//     own per-target token, which the server binds to the repository and
//     the permissions Target minted it with (see target.Remote), and
//     refuses after Close. People push with their account token.
//   - A pull request's HeadSHA follows its head branch while it is open and
//     keeps its last value once closed or merged. CreatePR needs both
//     branches, EditPR an existing new base; reopening needs the head
//     branch.
//   - Pushes through the server count as touchmark's: people act through
//     PushFiles, MergePR, UpdateBranch, RebaseBranch and the setup
//     methods. Each push is logged as "Push <repo> <branches moved…>"
//     (Writes include it) and takes faults injected for "Push"; each fetch
//     or ls-remote is logged as "Fetch <repo>" and takes faults for
//     "Fetch". A fault fails the request with the Status of a
//     *platform.Error (500 otherwise); FailNextApplied lets a push move
//     the branches first, as when the response is lost.
//
// After every ref change, whoever made it, pull requests follow: a deleted
// head branch closes an open PR (every flavor); GitHub closes an open PR
// whose head equals its base's tip; Gitea and Forgejo mark an open PR
// merged once its head is reachable from its base (autodetect_manual_merge);
// a PR whose base branch is deleted loses BaseExists. GitLab runs the quick
// actions of bodies and comments written through the API instead of
// refusing them (see Violations).
//
// Not modeled, because not verified live yet:
// whether GitHub and GitLab mark an open PR merged when its head reaches
// its base through another merge (GitHub's merge detection, GitLab's
// "manually merged"). The fake keeps such a PR open there; a test that
// depends on either answer must not trust the fake.
//
// Pushes through the server meet the platforms' refusals: every repository
// has a pre-receive hook that, for an identity without the Workflows
// permission on a flavor with Caps.WorkflowPerm, rejects a push creating or
// changing a file under .github/workflows with GitHub's message.

// GitServer is a running git server for the fake.
type GitServer struct {
	// URL is the server's base URL (http://127.0.0.1:port).
	URL string

	p    *Platform
	srv  *http.Server
	log  *log.Logger
	once sync.Once
	err  error
}

// ServeGit switches the platform to git mode: repositories live under dir
// as bare repositories (existing and future ones), served at
// <URL>/<repo path>.git with uploadpack.allowFilter,
// uploadpack.allowAnySHA1InWant and http.receivepack enabled. Requests must
// carry "Authorization: Basic base64(<login>:<token>)" of an account with a
// token (SetToken, or its read-only form) or of a per-target token
// (TargetWriter.Remote); pushes also need write access (GrantWrite), and a
// per-target token only reaches its repository with its permissions (see
// target.Remote). Close stops the server.
//
// dir must be empty or absent. The files of existing repositories become
// one commit on their default branch. Call ServeGit before taking heads or
// snapshots: they change from the memory mode's synthetic ids to real
// commit ids. The platform stays in git mode after Close.
func (p *Platform) ServeGit(dir string) (*GitServer, error) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.err != nil:
		return nil, fmt.Errorf("fake: ServeGit: broken fixture: %w", p.err)
	case p.git != nil:
		return nil, errors.New("fake: ServeGit: the platform is in git mode already")
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	g, err := newGitMode(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("fake: ServeGit: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fake: ServeGit: %w", err)
	}
	s := &GitServer{URL: "http://" + ln.Addr().String(), p: p, log: log.New(io.Discard, "", 0)}
	g.server = s
	p.git = g
	ids := make([]string, 0, len(p.repos))
	for id := range p.repos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := p.convertRepo(ctx, p.repos[id]); err != nil {
			// Half the repositories are converted: the fixture is broken.
			_ = ln.Close()
			p.setupf("ServeGit: %v", err)
			return nil, fmt.Errorf("fake: ServeGit: %w", err)
		}
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: time.Minute, ErrorLog: s.log}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Close stops the git server. It waits for requests in flight (at most a
// few seconds) and may be called more than once.
func (s *GitServer) Close() error {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(ctx); err != nil {
			s.err = errors.Join(err, s.srv.Close())
		}
	})
	return s.err
}

// SetToken sets the git and API token of an account; Reader.Remote and
// TargetWriter.Remote then return the server URL of the repository with a
// Header that sends it.
//
// An empty token removes the account's token. A token belongs to one
// account: giving it to a second one is a setup error, which never repeats
// the token.
func (p *Platform) SetToken(account platform.Account, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accounts[account.ID] == nil {
		p.setupf("SetToken: unknown account %q", account.ID)
		return
	}
	if token == "" {
		delete(p.gitTokens, account.ID)
		return
	}
	for id, other := range p.gitTokens {
		if other == token && id != account.ID {
			p.setupf("SetToken(%s): the token belongs to account %s", account.Login, id)
			return
		}
	}
	p.gitTokens[account.ID] = token
}

// SetKnownAuthors declares whose pull requests count as writer's own when
// writer pushes through the git server (known_authors in hub.yml):
// moving or deleting the branch of such an open PR is no violation. It
// replaces the previous list; no authors clears it.
func (p *Platform) SetKnownAuthors(writer platform.Account, authors ...platform.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range append([]platform.Account{writer}, authors...) {
		if p.accounts[a.ID] == nil {
			p.setupf("SetKnownAuthors: unknown account %q", a.ID)
			return
		}
	}
	ids := map[string]bool{}
	for _, a := range authors {
		ids[a.ID] = true
	}
	p.known[writer.ID] = ids
}

// Branch returns the commit at the tip of a branch of the repository in
// git mode, "" when the branch does not exist (or in memory mode).
func (p *Platform) Branch(repoID, name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.repos[repoID]; s != nil {
		return s.refs[name]
	}
	return ""
}

// GitDir returns the bare repository of the repository in git mode, for
// tests that inspect it with git; "" in memory mode.
func (p *Platform) GitDir(repoID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.repos[repoID]; s != nil {
		return s.dir
	}
	return ""
}

// MergeHow is how a person merges a PR.
type MergeHow uint8

const (
	MergeCommit MergeHow = iota + 1
	MergeSquash
	MergeRebase
)

// Errors of the human actions.
var (
	// ErrMergeConflict: the merge or rebase a human action needs has
	// conflicts.
	ErrMergeConflict = errors.New("fake: merge conflict")
	// ErrGitTooOld: a human action needs a content merge (a path changed on
	// both sides) and the local git is too old for it: a merge needs git
	// 2.38 (git merge-tree --write-tree), a rebase git 2.40 (with
	// --merge-base). Tests skip on it.
	ErrGitTooOld = errors.New("fake: git is too old for a content merge")
)

// PushFiles commits files (path → content; nil content deletes) on top of
// branch as author and moves the branch (creating it from the default
// branch when absent). It returns the new head. A person's push.
//
// Files get mode 100644. In an empty repository the commit has no parent.
// The commit is authored and committed by author at when (the platform
// clock when zero). Git mode only, like every human action; they return
// errors instead of recording setup errors.
func (p *Platform) PushFiles(repoID, branch string, files map[string][]byte, author platform.Account, when time.Time) (string, error) {
	const op = "PushFiles"
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s, who, err := p.human(op, repoID, author)
	if err != nil {
		return "", err
	}
	if err := checkBranchName(branch); err != nil {
		return "", fmt.Errorf("fake: %s: %w", op, err)
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		if err := checkTreePath(path); err != nil {
			return "", fmt.Errorf("fake: %s: %w", op, err)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	changes := make([]fileChange, len(paths))
	for i, path := range paths {
		changes[i] = fileChange{path: path}
		if content := files[path]; content != nil {
			changes[i].mode, changes[i].data = ModeFile, content
		}
	}
	old := s.refs[branch]
	parent := old
	if parent == "" {
		parent = s.refs[s.repo.DefaultBranch]
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	sign := p.person(who, when)
	msg := pushMessage(paths)
	head, err := p.repoGit(s).importCommit(ctx, branch, parent, changes, sign, sign, msg)
	if err != nil {
		return "", fmt.Errorf("fake: %s(%s, %s): %w", op, s.repo.Path, branch, err)
	}
	if err := p.refsMoved(ctx, s, []refChange{{branch: branch, old: old, new: head}}, &who, false, false); err != nil {
		return "", fmt.Errorf("fake: %s(%s, %s): %w", op, s.repo.Path, branch, err)
	}
	return head, nil
}

// MergePR merges an open PR the way people do and marks it merged with
// ClosedBy = by. The head must be based on the current default branch for
// MergeRebase and MergeSquash; MergeCommit uses `git merge-tree
// --write-tree` and fails on conflicts (git ≥ 2.38).
//
// In detail: the PR's base branch (not necessarily the default one) moves
// and the new tip is returned; the commits are by by at when (the platform
// clock when zero), and the PR keeps its head as HeadSHA. MergeCommit
// writes "Merge pull request #N from <head>" with parents [base, head]:
// when no path changed on both sides (a head based on the base, for one)
// the tree is merged path by path, on any git; otherwise git merge-tree
// merges contents, and an older git gives ErrGitTooOld, a conflict
// ErrMergeConflict. MergeSquash writes one commit "<title> (#N)" with the
// head's tree; MergeRebase copies the head's commits, which must not
// include merges, keeping authors and messages. A head its base already
// contains is an error. The head branch stays. PRs from forks cannot be
// merged.
func (p *Platform) MergePR(repoID string, number int64, how MergeHow, by platform.Account, when time.Time) (string, error) {
	const op = "MergePR"
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s, who, err := p.human(op, repoID, by)
	if err != nil {
		return "", err
	}
	ps, head, base, err := p.openPR(op, s, number)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	r := p.repoGit(s)
	sign := p.person(who, when)
	pr := ps.pr
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("fake: %s(%s#%d): %w", op, s.repo.Path, number, err)
	}
	merged, err := r.isAncestor(ctx, head, base)
	if err != nil {
		return fail(err)
	}
	if merged {
		return fail(fmt.Errorf("%s has nothing that %s lacks", pr.Head, pr.Base))
	}
	based, err := r.isAncestor(ctx, base, head)
	if err != nil {
		return fail(err)
	}
	var tip string
	switch how {
	case MergeCommit:
		tree, err := r.mergeInto(ctx, base, head, based)
		if err != nil {
			return fail(err)
		}
		msg := fmt.Sprintf("Merge pull request #%d from %s\n\n%s\n", number, pr.Head, pr.Title)
		tip, err = r.commitObject(ctx, tree, []string{base, head}, sign.String(), sign.String(), msg)
		if err != nil {
			return fail(err)
		}
	case MergeSquash, MergeRebase:
		if !based {
			return fail(fmt.Errorf("%s is not based on the tip of %s", pr.Head, pr.Base))
		}
		if how == MergeSquash {
			tree, err := r.treeID(ctx, head)
			if err != nil {
				return fail(err)
			}
			tip, err = r.commitObject(ctx, tree, []string{base}, sign.String(), sign.String(), fmt.Sprintf("%s (#%d)\n", pr.Title, number))
			if err != nil {
				return fail(err)
			}
			break
		}
		tip, err = r.replay(ctx, base, head, sign, false)
		if err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown merge method %d", how))
	}
	if err := r.updateRef(ctx, pr.Base, tip, base); err != nil {
		return fail(err)
	}
	ps.pr.HeadSHA = head
	p.finish(&ps.pr, platform.Merged, &who, sign.when)
	if err := p.refsMoved(ctx, s, []refChange{{branch: pr.Base, old: base, new: tip}}, &who, false, false); err != nil {
		return fail(err)
	}
	return tip, nil
}

// UpdateBranch merges the default branch into the PR's head (GitHub's
// "Update branch" button): a merge commit with the head as first parent.
//
// The PR's base is merged, "Merge branch '<base>' into <head>" by by at
// when, and the new head returned. A head that already contains the base
// is an error, as the button is not offered then. Trees follow the rules
// of MergeCommit.
func (p *Platform) UpdateBranch(repoID string, number int64, by platform.Account, when time.Time) (string, error) {
	const op = "UpdateBranch"
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s, who, err := p.human(op, repoID, by)
	if err != nil {
		return "", err
	}
	ps, head, base, err := p.openPR(op, s, number)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	r := p.repoGit(s)
	pr := ps.pr
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("fake: %s(%s#%d): %w", op, s.repo.Path, number, err)
	}
	upToDate, err := r.isAncestor(ctx, base, head)
	if err != nil {
		return fail(err)
	}
	if upToDate {
		return fail(fmt.Errorf("%s is up to date with %s", pr.Head, pr.Base))
	}
	behind, err := r.isAncestor(ctx, head, base)
	if err != nil {
		return fail(err)
	}
	tree, err := r.mergeInto(ctx, head, base, behind)
	if err != nil {
		return fail(err)
	}
	sign := p.person(who, when)
	msg := fmt.Sprintf("Merge branch '%s' into %s\n", pr.Base, pr.Head)
	tip, err := r.commitObject(ctx, tree, []string{head, base}, sign.String(), sign.String(), msg)
	if err != nil {
		return fail(err)
	}
	if err := r.updateRef(ctx, pr.Head, tip, head); err != nil {
		return fail(err)
	}
	if err := p.refsMoved(ctx, s, []refChange{{branch: pr.Head, old: head, new: tip}}, &who, false, false); err != nil {
		return fail(err)
	}
	return tip, nil
}

// RebaseBranch rebases the PR's head onto the default branch keeping
// messages (the "Update branch with rebase" button).
//
// The commits of the head that its base lacks, merges left out, are
// replayed onto the PR's base keeping authors and messages, with by as
// committer at when; a commit that becomes empty is dropped, as git rebase
// does. Each replay is path by path when the base did not change the
// commit's paths, and a content merge otherwise (git merge-tree
// --merge-base, git ≥ 2.40, else ErrGitTooOld; conflicts are
// ErrMergeConflict). A head that already contains the base is an error.
func (p *Platform) RebaseBranch(repoID string, number int64, by platform.Account, when time.Time) (string, error) {
	const op = "RebaseBranch"
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s, who, err := p.human(op, repoID, by)
	if err != nil {
		return "", err
	}
	ps, head, base, err := p.openPR(op, s, number)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	r := p.repoGit(s)
	pr := ps.pr
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("fake: %s(%s#%d): %w", op, s.repo.Path, number, err)
	}
	upToDate, err := r.isAncestor(ctx, base, head)
	if err != nil {
		return fail(err)
	}
	if upToDate {
		return fail(fmt.Errorf("%s is up to date with %s", pr.Head, pr.Base))
	}
	tip, err := r.replay(ctx, base, head, p.person(who, when), true)
	if err != nil {
		return fail(err)
	}
	if err := r.updateRef(ctx, pr.Head, tip, head); err != nil {
		return fail(err)
	}
	if err := p.refsMoved(ctx, s, []refChange{{branch: pr.Head, old: head, new: tip}}, &who, false, false); err != nil {
		return fail(err)
	}
	return tip, nil
}

// Violations returns the forbidden transitions the platform saw since the
// last call: a head moved to its base, the branch of an
// open PR deleted, a push to the branch of a closed PR, a branch carrying a
// foreign open PR moved. Tests assert it is empty. The platform also acts
// on them like the real one: GitHub closes a PR whose head equals its base,
// every flavor closes a PR whose branch is deleted, Gitea and Forgejo mark
// a PR merged when its head becomes reachable from the base
// (autodetect_manual_merge), and GitLab executes quick actions in bodies.
//
// Only touchmark's writes are judged: pushes through the git server and,
// for quick actions, bodies and comments written through the API. People's
// actions trigger the same reactions without violations. Always empty in
// memory mode. Each entry starts with its kind:
//   - "head-to-base <repo>#<n>: …" a push left an open PR's head equal to
//     or behind its base;
//   - "deleted-open-branch <repo>#<n>: …" a push deleted the head branch of
//     an open PR;
//   - "foreign-branch <repo>#<n>: …" a push moved or deleted the head
//     branch of an open PR by another author (see SetKnownAuthors);
//   - "closed-branch-push <repo>#<n>: …" a push moved the branch of a
//     closed or merged PR and no new PR from that branch followed before
//     this call;
//   - "default-branch <repo>: …" a push moved or deleted the default
//     branch;
//   - "quick-action <repo>#<n>: …" GitLab ran a quick action of a body or
//     comment written through the API (/close closes, /merge marks the PR
//     merged without moving branches, /label ~a ~b adds labels; other
//     commands are only recorded).
func (p *Platform) Violations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.violations)
	for _, pp := range p.pending {
		out = append(out, pp.text)
	}
	p.violations, p.pending = nil, nil
	return out
}

// human opens a human action on the repository with id repoID: git mode, a
// sound fixture and a known account (returned with its current login).
// Called with gitMu and mu held.
func (p *Platform) human(op, repoID string, who platform.Account) (*repoState, platform.Account, error) {
	switch {
	case p.err != nil:
		return nil, platform.Account{}, fmt.Errorf("fake: %s: broken fixture: %w", op, p.err)
	case p.git == nil:
		return nil, platform.Account{}, fmt.Errorf("fake: %s needs git mode (ServeGit)", op)
	}
	s := p.repos[repoID]
	if s == nil {
		return nil, platform.Account{}, fmt.Errorf("fake: %s: no repository with id %q", op, repoID)
	}
	a := p.accounts[who.ID]
	if a == nil || who.ID == "" {
		return nil, platform.Account{}, fmt.Errorf("fake: %s: unknown account %q", op, who.ID)
	}
	return s, *a, nil
}

// openPR returns an open pull request of s whose head branch lives in s,
// with the tips of its head and base.
func (p *Platform) openPR(op string, s *repoState, number int64) (ps *prState, head, base string, err error) {
	ps = s.prs[number]
	switch {
	case ps == nil:
		return nil, "", "", fmt.Errorf("fake: %s: %s has no pull request #%d", op, s.repo.Path, number)
	case ps.pr.State != platform.Open:
		return nil, "", "", fmt.Errorf("fake: %s: %s#%d is %s", op, s.repo.Path, number, ps.pr.State)
	case ps.pr.HeadRepoID != s.repo.ID:
		return nil, "", "", fmt.Errorf("fake: %s: %s#%d comes from another repository", op, s.repo.Path, number)
	}
	head, base = s.refs[ps.pr.Head], s.refs[ps.pr.Base]
	switch {
	case head == "":
		return nil, "", "", fmt.Errorf("fake: %s: the head branch %q of %s#%d does not exist", op, ps.pr.Head, s.repo.Path, number)
	case base == "":
		return nil, "", "", fmt.Errorf("fake: %s: the base branch %q of %s#%d does not exist", op, ps.pr.Base, s.repo.Path, number)
	}
	return ps, head, base, nil
}

// person is the signature of an account at when, the platform clock when
// zero. Called with mu held.
func (p *Platform) person(a platform.Account, when time.Time) ident {
	if when.IsZero() {
		when = p.now()
	}
	return ident{name: a.Login, email: a.Email, when: when}
}

// pushMessage is the message of a person's push of paths.
func pushMessage(paths []string) string {
	switch len(paths) {
	case 0:
		return "Empty push\n"
	case 1:
		return "Update " + paths[0] + "\n"
	}
	return fmt.Sprintf("Update %s and %d more\n", paths[0], len(paths)-1)
}

// checkBranchName accepts what git accepts as refs/heads/<name>
// (git check-ref-format), without asking git.
func checkBranchName(name string) error {
	bad := name == "" || name == "@" || strings.HasPrefix(name, "-") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{")
	for _, seg := range strings.Split(name, "/") {
		bad = bad || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock")
	}
	for i := 0; i < len(name) && !bad; i++ {
		c := name[i]
		bad = c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0
	}
	if bad {
		return fmt.Errorf("%q is not a valid branch name", name)
	}
	return nil
}
