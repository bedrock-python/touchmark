// Package fake is an in-memory code hosting platform for tests: of the
// delivery core (plan, distribute) and of the platform contract itself,
// through package conformance.
//
// A Platform holds accounts, repositories with the tree of their default
// branch, pull requests with their comments, and labels. Reader and Writer
// return the platform.Reader and platform.Writer of one account; Snapshots
// returns a snapshot.Source over the same trees. Tests arrange state with
// the setup methods (AddAccount, AddRepo, SetFile, AddPR, SetPRState, …),
// inject faults with FailNext, Fail and Inject, and assert on PR, Comments,
// the call log and, with WithRequestLog, the requests and their times.
//
// The fake follows the documented contracts of package platform and the
// capabilities of its flavor: title-prefix drafts on
// GitLab, Gitea and Forgejo; bodies and comments that would run GitLab
// quick actions are refused (git mode runs them and records a violation);
// closers are not reported on Gitea and Forgejo. Branch rulesets, the
// Preflighter of GitHub's driver (WithPreflight) and its API commits
// (WithAPICommits, platform.Committer) are modeled in rules.go.
//
// In memory mode, the default, the fake models the default branch of each
// repository only: there is no git transport, and other branches exist
// only as names on pull requests. ServeGit switches it to git mode: every
// repository becomes a bare git repository served over HTTP, branches are
// real, pull requests follow them, people push and merge through the human
// actions (PushFiles, MergePR, UpdateBranch, RebaseBranch), and Violations
// reports the transitions touchmark must never make (see git.go).
//
// Setup methods do not return errors. The first misuse (an unknown
// repository, a path git cannot hold, a taken path or number, …) is kept:
// Err returns it, and every later Reader, Writer and snapshot call fails
// with it, so a broken fixture cannot pass unnoticed.
//
// A Platform is safe for concurrent use.
package fake

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// Flavor is the platform the fake imitates.
type Flavor string

// Flavors: the platforms touchmark supports.
const (
	GitHub  Flavor = "github"
	GitLab  Flavor = "gitlab"
	Gitea   Flavor = "gitea"
	Forgejo Flavor = "forgejo"
)

// Tree entry modes.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
	ModeGitlink    = "160000"
)

// Epoch is where the default clock starts.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// CapsFor returns the capabilities the fake reports for f, with the default
// limits of its platform. Every flavor keeps the marker in the body and has
// no API commit (WithAPICommits adds one). A flavor other than the four known
// ones gets GitHub's capabilities under its own name.
func CapsFor(f Flavor) platform.Caps {
	c := platform.Caps{
		Flavor:      string(f),
		Version:     "fake",
		MaxBody:     58000,
		Draft:       platform.DraftNative,
		CloserKnown: true,
		Marker:      platform.MarkerInBody,
	}
	switch f {
	case GitLab:
		c.MaxBody = 200000
		c.Draft = platform.DraftTitlePrefix
		c.DraftPrefix = "Draft: "
		c.QuickActions = true
		c.RuntimeOnly = []string{"push_rules"}
		c.Limits = platform.Limits{Reads: 8, GitReads: 4, ReadsPerMinute: 600, CommentsPerMinute: 50, MinInterval: 250 * time.Millisecond}
	case Gitea, Forgejo:
		c.Draft = platform.DraftTitlePrefix
		c.DraftPrefix = "WIP: "
		c.LabelsByID = true
		c.CloserKnown = false
		c.Limits = platform.Limits{Reads: 4, GitReads: 2, MinInterval: 250 * time.Millisecond}
	default:
		c.WorkflowPerm = true
		c.Limits = platform.Limits{Reads: 8, GitReads: 4, WritesPerMinute: 60, WritesPerHour: 450, MinInterval: time.Second}
	}
	return c
}

// Option configures a Platform.
type Option func(*Platform)

// WithFlavor sets the flavor and its capabilities (CapsFor). The default is
// GitHub.
func WithFlavor(f Flavor) Option {
	return func(p *Platform) { p.caps = CapsFor(f) }
}

// WithClock sets the clock that stamps pull requests, closes and comments.
// now is called with the Platform locked and must not call it. The default
// clock starts at Epoch and advances one second per reading, so a
// sequential test sees the same times on every run.
func WithClock(now func() time.Time) Option {
	return func(p *Platform) { p.clock = now }
}

// Comment is a comment on a pull request.
type Comment struct {
	Author    platform.Account
	Body      string
	CreatedAt time.Time
}

// Platform is one fake platform instance (one host).
type Platform struct {
	host string

	// gitMu serializes every change of a git ref in git mode: pushes
	// through the server and the commits of setup methods and human
	// actions. It is taken before mu, never while holding it.
	gitMu sync.Mutex

	mu         sync.Mutex
	caps       platform.Caps
	clock      func() time.Time
	ticks      int64
	lastID     int64
	tokens     int64
	accounts   map[string]*platform.Account // by ID
	byLogin    map[string]*platform.Account // by lowercased login
	repos      map[string]*repoState        // by ID
	byPath     map[string]*repoState        // by lowercased path
	blobs      map[string][]byte            // by OID
	queued     map[string][]fault           // by method or "*"
	sticky     map[string]error             // by method or "*"
	inject     Injector                     // after queued and sticky faults
	incomplete bool
	calls      []call
	err        error // the first setup error

	// logRequests turns on the log of API requests (WithRequestLog).
	logRequests bool
	requests    []Request

	// Rules and API commits (rules.go): preflight and apiCommits are set
	// by options only; workflowsHidden and apiUnsigned by their setters.
	preflight       bool
	apiCommits      bool
	workflowsHidden bool
	apiUnsigned     bool

	// Checks of doctor (checker.go): what Check reports about the
	// identity (nil: the default), and the signing keys by account ID.
	identity    []platform.Finding
	signingKeys map[string][]string

	// Git mode (git.go).
	git          *gitMode                   // nil in memory mode
	gitTokens    map[string]string          // account ID → token (SetToken)
	targetTokens map[string]*target         // per-target token → its writer
	known        map[string]map[string]bool // pusher ID → author IDs it owns
	violations   []string
	pending      []pendingPush
}

// repoState is one repository with its default branch tree and PRs.
type repoState struct {
	repo    platform.Repo
	entries map[string]snapshot.Entry
	prs     map[int64]*prState
	lastPR  int64
	labels  map[string]string // name → id
	lastLbl int64
	grants  map[string]platform.Perms // account ID → perms
	// rulesets are the repository's branch rulesets; rulesHidden hides them
	// from Preflight (rules.go).
	rulesets    []Ruleset
	rulesHidden bool
	// In git mode: the bare repository and its branches (name → commit).
	dir  string
	refs map[string]string
}

type prState struct {
	pr       platform.PR
	comments []Comment
}

// fault is an injected error. An applied fault lets the call take effect
// first.
type fault struct {
	err     error
	applied bool
}

type call struct {
	method string
	args   []string
}

// New returns an empty platform for host (e.g. "github.com"), GitHub
// flavored unless an option says otherwise.
func New(host string, opts ...Option) *Platform {
	p := &Platform{
		host:         strings.ToLower(host),
		caps:         CapsFor(GitHub),
		accounts:     map[string]*platform.Account{},
		byLogin:      map[string]*platform.Account{},
		repos:        map[string]*repoState{},
		byPath:       map[string]*repoState{},
		blobs:        map[string][]byte{},
		queued:       map[string][]fault{},
		sticky:       map[string]error{},
		gitTokens:    map[string]string{},
		targetTokens: map[string]*target{},
		known:        map[string]map[string]bool{},
	}
	for _, o := range opts {
		o(p)
	}
	if p.apiCommits {
		p.caps.Commit.API, p.caps.Commit.SignedByPlatform, p.caps.Commit.CAS = true, true, true
	}
	if host == "" {
		p.setupf("New: empty host")
	}
	return p
}

// Host returns the platform's host, lowercased.
func (p *Platform) Host() string { return p.host }

// Caps returns the capabilities Probe reports.
func (p *Platform) Caps() platform.Caps {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneCaps(p.caps)
}

// SetCaps replaces the capabilities, e.g. to turn CloserKnown on for a
// Gitea test. The flavor-dependent behaviour follows the new value.
func (p *Platform) SetCaps(c platform.Caps) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.caps = cloneCaps(c)
}

// Err returns the first setup error, or nil.
func (p *Platform) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// setupf records a setup error unless one is recorded already. Called with
// p.mu held (or before p is shared).
func (p *Platform) setupf(format string, args ...any) {
	if p.err == nil {
		p.err = fmt.Errorf("fake: "+format, args...)
	}
}

// Accounts.

// AddAccount returns the account with login, adding it with kind when no
// account has that login (compared case-insensitively). Ids are numeric
// strings that depend only on the order of AddAccount and AddRepo calls.
// The email is "<id>+<login>@users.noreply.<host>".
func (p *Platform) AddAccount(login string, kind platform.AccountKind) platform.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	if login == "" {
		p.setupf("AddAccount: empty login")
		return platform.Account{}
	}
	if a := p.accountByLogin(login); a != nil {
		return *a
	}
	id := p.newID()
	a := &platform.Account{ID: id, Login: login, Email: id + "+" + login + "@users.noreply." + p.host, Kind: kind}
	p.accounts[id] = a
	p.byLogin[strings.ToLower(login)] = a
	return *a
}

// RenameAccount changes the login of the account with id; its id stays, as
// on real platforms. The new login must be free.
func (p *Platform) RenameAccount(id, login string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.accounts[id]
	switch {
	case a == nil:
		p.setupf("RenameAccount(%q): no such account", id)
	case login == "":
		p.setupf("RenameAccount(%q): empty login", id)
	case p.accountByLogin(login) != nil && !strings.EqualFold(a.Login, login):
		p.setupf("RenameAccount(%q): login %q is taken", id, login)
	default:
		delete(p.byLogin, strings.ToLower(a.Login))
		a.Login = login
		a.Email = id + "+" + login + "@users.noreply." + p.host
		p.byLogin[strings.ToLower(login)] = a
	}
}

func (p *Platform) accountByLogin(login string) *platform.Account {
	return p.byLogin[strings.ToLower(login)]
}

// refresh returns the current form of a (a renamed login), or a itself
// when the platform does not know its id.
func (p *Platform) refresh(a platform.Account) platform.Account {
	if cur, ok := p.accounts[a.ID]; ok && a.ID != "" {
		return *cur
	}
	return a
}

func (p *Platform) newID() string {
	p.lastID++
	return strconv.FormatInt(1000+p.lastID, 10)
}

// Repositories.

// AddRepo adds r and returns it as the platform reports it. Host is the
// platform's; an empty ID gets the next numeric id (a given one must be
// free). Defaults: DefaultBranch "main", Visibility "public", ObjectFormat
// "sha1", WebURL "https://<host>/<path>". The path needs at least two
// segments and must be free (compared case-insensitively). Empty is
// reported while the repository has no files, or when r.Empty is set.
func (p *Platform) AddRepo(r platform.Repo) platform.Repo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := checkRepoPath(r.Path); err != nil {
		p.setupf("AddRepo: %v", err)
		return platform.Repo{}
	}
	if other := p.repoByPathLocked(r.Path); other != nil {
		p.setupf("AddRepo(%q): path is taken by repository %s", r.Path, other.repo.ID)
		return platform.Repo{}
	}
	if r.ID == "" {
		r.ID = p.newID()
	} else if p.repos[r.ID] != nil {
		p.setupf("AddRepo(%q): id %s is taken", r.Path, r.ID)
		return platform.Repo{}
	}
	r.Host = p.host
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	if r.Visibility == "" {
		r.Visibility = "public"
	}
	if r.ObjectFormat == "" {
		r.ObjectFormat = "sha1"
	}
	if r.WebURL == "" {
		r.WebURL = "https://" + p.host + "/" + r.Path
	}
	r.Topics = slices.Clone(r.Topics)
	s := &repoState{
		repo:    r,
		entries: map[string]snapshot.Entry{},
		prs:     map[int64]*prState{},
		labels:  map[string]string{},
		grants:  map[string]platform.Perms{},
	}
	p.repos[r.ID] = s
	p.byPath[strings.ToLower(r.Path)] = s
	if p.git != nil {
		ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
		if err := p.initRepo(ctx, s); err != nil {
			p.setupf("AddRepo(%q): %v", r.Path, err)
		}
	}
	return p.repoView(s)
}

// UpdateRepo applies fn to the repository with id: archive it, rename it,
// change its topics or visibility, … Host and ID cannot change; a new path
// must be valid and free. In git mode a new default branch takes over the
// old one's commits when it does not exist yet (the old branch is then
// deleted, and pull requests based on it lose BaseExists), and the bare
// repository's HEAD follows it.
func (p *Platform) UpdateRepo(id string, fn func(*platform.Repo)) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo("UpdateRepo", id)
	if s == nil {
		return
	}
	r := s.repo
	r.Topics = slices.Clone(r.Topics)
	fn(&r)
	r.ID, r.Host = s.repo.ID, s.repo.Host
	if err := checkRepoPath(r.Path); err != nil {
		p.setupf("UpdateRepo(%s): %v", id, err)
		return
	}
	if other := p.repoByPathLocked(r.Path); other != nil && other != s {
		p.setupf("UpdateRepo(%s): path %q is taken by repository %s", id, r.Path, other.repo.ID)
		return
	}
	r.Topics = slices.Clone(r.Topics)
	delete(p.byPath, strings.ToLower(s.repo.Path))
	p.byPath[strings.ToLower(r.Path)] = s
	old := s.repo.DefaultBranch
	s.repo = r
	if p.git != nil && r.DefaultBranch != old {
		if err := p.renameDefault(s, old); err != nil {
			p.setupf("UpdateRepo(%s): %v", id, err)
		}
	}
}

// renameDefault moves the git side of s to its new default branch, which
// was old. Called with gitMu and mu held.
func (p *Platform) renameDefault(s *repoState, old string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	r := p.repoGit(s)
	branch := s.repo.DefaultBranch
	if err := checkBranchName(branch); err != nil {
		return err
	}
	if _, err := r.g.Run(ctx, nil, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return err
	}
	tip := s.refs[old]
	if tip == "" || s.refs[branch] != "" {
		return p.loadTree(ctx, s)
	}
	if err := r.updateRef(ctx, branch, tip, ""); err != nil {
		return err
	}
	if err := r.updateRef(ctx, old, "", tip); err != nil {
		return err
	}
	return p.refsMoved(ctx, s, []refChange{{branch: old, old: tip}, {branch: branch, new: tip}}, nil, false, false)
}

// RepoByID returns the repository with id as the API reports it.
func (p *Platform) RepoByID(id string) (platform.Repo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.repos[id]
	if s == nil {
		return platform.Repo{}, false
	}
	return p.repoView(s), true
}

// SetFile puts a regular file on the default branch of the repository: mode
// is "100644" (also when empty) or "100755", and the blob id is
// gitx.RawOID(content). path is a repository path: '/'-separated and
// relative, without empty, "." or ".." segments. It may replace an entry
// at path, but must not lie under a file or be the parent of other entries,
// since a git tree cannot hold both.
func (p *Platform) SetFile(repoID, path string, content []byte, mode string) {
	switch mode {
	case "":
		mode = ModeFile
	case ModeFile, ModeExecutable:
	default:
		p.mu.Lock()
		p.setupf("SetFile(%s, %q): mode %q is not a regular file mode", repoID, path, mode)
		p.mu.Unlock()
		return
	}
	p.putEntry("SetFile", repoID, path, mode, content)
}

// SetSymlink puts a symbolic link to target at path (mode 120000; the blob
// holds target). The platform never follows it.
func (p *Platform) SetSymlink(repoID, path, target string) {
	if target == "" {
		p.mu.Lock()
		p.setupf("SetSymlink(%s, %q): empty target", repoID, path)
		p.mu.Unlock()
		return
	}
	p.putEntry("SetSymlink", repoID, path, ModeSymlink, []byte(target))
}

// SetGitlink puts a submodule entry at path that points at commit (mode
// 160000, 40 or 64 hex digits).
func (p *Platform) SetGitlink(repoID, path, commit string) {
	if !isHexID(commit) {
		p.mu.Lock()
		p.setupf("SetGitlink(%s, %q): %q is not a commit id", repoID, path, commit)
		p.mu.Unlock()
		return
	}
	p.putEntry("SetGitlink", repoID, path, ModeGitlink, []byte(strings.ToLower(commit)))
}

// putEntry stores one tree entry. For a gitlink, data is the commit id.
// In git mode the entry is committed to the default branch.
func (p *Platform) putEntry(op, repoID, path, mode string, data []byte) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo(op, repoID)
	if s == nil {
		return
	}
	if err := checkTreePath(path); err != nil {
		p.setupf("%s(%s): %v", op, repoID, err)
		return
	}
	if err := s.conflict(path); err != nil {
		p.setupf("%s(%s): %v", op, repoID, err)
		return
	}
	var oid string
	if mode == ModeGitlink {
		oid = string(data)
	} else {
		oid = gitx.RawOID(data)
		if _, ok := p.blobs[oid]; !ok {
			p.blobs[oid] = slices.Clone(data)
		}
	}
	if p.git != nil {
		c := fileChange{path: path, mode: mode, data: data, oid: oid}
		if err := p.commitSetup(s, c, "fake: set "+path+"\n"); err != nil {
			p.setupf("%s(%s): %v", op, repoID, err)
		}
		return
	}
	s.entries[path] = snapshot.Entry{Mode: mode, OID: oid}
}

// RemoveFile removes the entry at path, which must exist.
func (p *Platform) RemoveFile(repoID, path string) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo("RemoveFile", repoID)
	if s == nil {
		return
	}
	if _, ok := s.entries[path]; !ok {
		p.setupf("RemoveFile(%s, %q): no such entry", repoID, path)
		return
	}
	if p.git != nil {
		if err := p.commitSetup(s, fileChange{path: path}, "fake: remove "+path+"\n"); err != nil {
			p.setupf("RemoveFile(%s, %q): %v", repoID, path, err)
		}
		return
	}
	delete(s.entries, path)
}

// Head returns the commit id at the tip of the default branch: a sha1 over
// the repository id and its tree, so the same state always gives the same
// id and any change of the tree gives another. It is "" for an empty
// repository. In git mode it is the real commit, "" while the default
// branch does not exist.
func (p *Platform) Head(repoID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.repos[repoID]; s != nil {
		if p.git != nil {
			return s.refs[s.repo.DefaultBranch]
		}
		return s.head()
	}
	return ""
}

func (s *repoState) head() string {
	if len(s.entries) == 0 {
		return ""
	}
	paths := make([]string, 0, len(s.entries))
	for path := range s.entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	h := sha1.New()
	fmt.Fprintf(h, "fake commit\x00%s\x00", s.repo.ID)
	for _, path := range paths {
		e := s.entries[path]
		fmt.Fprintf(h, "%s %s\t%s\x00", e.Mode, e.OID, path)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// conflict reports why path cannot join the tree: an ancestor of it is an
// entry, or it is the parent directory of an entry.
func (s *repoState) conflict(path string) error {
	for i := 0; i < len(path); i++ {
		if path[i] != '/' {
			continue
		}
		if e, ok := s.entries[path[:i]]; ok {
			return fmt.Errorf("%q lies under %q, a %s entry", path, path[:i], e.Mode)
		}
	}
	if child, ok := s.child(path); ok {
		return fmt.Errorf("%q is the parent directory of %q", path, child)
	}
	return nil
}

// child returns an entry under directory dir, if any.
func (s *repoState) child(dir string) (string, bool) {
	prefix := dir + "/"
	for path := range s.entries {
		if strings.HasPrefix(path, prefix) {
			return path, true
		}
	}
	return "", false
}

// Pull requests.

// AddPR stores pr in the repository as people or other tools create pull
// requests, and returns its number. A zero Number gets the next number of
// the repository (they start at 1); a given one must be free. Head and
// Author.ID are required. Defaults: RepoID and HeadRepoID are repoID (set
// HeadRepoID to another id for a PR from a fork), State is Open, Base is
// the default branch, CreatedAt and (closed or merged) ClosedAt come from
// the clock, URL is the flavor's web URL. BaseExists starts true whatever
// pr says (DeleteBranch and UpdatePR change it); an open PR has no
// ClosedBy or ClosedAt. Missing labels are created in the repository. The
// rest, Draft and Title included, is stored as given.
func (p *Platform) AddPR(repoID string, pr platform.PR) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo("AddPR", repoID)
	if s == nil {
		return 0
	}
	if pr.RepoID != "" && pr.RepoID != repoID {
		p.setupf("AddPR(%s): pr.RepoID is %s", repoID, pr.RepoID)
		return 0
	}
	switch {
	case pr.Number < 0:
		p.setupf("AddPR(%s): negative number %d", repoID, pr.Number)
		return 0
	case pr.Number == 0:
		pr.Number = s.lastPR + 1
	case s.prs[pr.Number] != nil:
		p.setupf("AddPR(%s): number %d is taken", repoID, pr.Number)
		return 0
	}
	pr.RepoID = repoID
	pr.BaseExists = true
	if pr.HeadRepoID == "" {
		pr.HeadRepoID = repoID
	}
	if err := p.normalizePR(s, &pr); err != nil {
		p.setupf("AddPR(%s): %v", repoID, err)
		return 0
	}
	s.prs[pr.Number] = &prState{pr: pr}
	s.lastPR = max(s.lastPR, pr.Number)
	return pr.Number
}

// SetPRState moves a pull request to state as a person or bot would:
// Closed and Merged record closedBy (nil: the platform does not say) and
// at (the clock when zero); Open reopens it and clears both.
func (p *Platform) SetPRState(repoID string, number int64, state platform.PRState, closedBy *platform.Account, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps := p.setupPR("SetPRState", repoID, number)
	if ps == nil {
		return
	}
	switch state {
	case platform.Open:
		ps.pr.State, ps.pr.ClosedBy, ps.pr.ClosedAt = platform.Open, nil, time.Time{}
		if tip := p.headTip(ps.pr); tip != "" {
			ps.pr.HeadSHA = tip
		}
	case platform.Closed, platform.Merged:
		if at.IsZero() {
			at = p.now()
		}
		ps.pr.State, ps.pr.ClosedAt, ps.pr.ClosedBy = state, at, nil
		if closedBy != nil {
			c := *closedBy
			ps.pr.ClosedBy = &c
		}
	default:
		p.setupf("SetPRState(%s, #%d): unknown state %q", repoID, number, state)
	}
}

// UpdatePR applies fn to a stored pull request, as a person editing it on
// the platform would: retitle it, edit the body, relabel it, mark it ready,
// move its head. Number and RepoID cannot change; the result is checked
// and completed like AddPR's, except that BaseExists and HeadRepoID are
// kept as fn leaves them: an empty HeadRepoID is a PR whose fork was
// deleted.
func (p *Platform) UpdatePR(repoID string, number int64, fn func(*platform.PR)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps := p.setupPR("UpdatePR", repoID, number)
	if ps == nil {
		return
	}
	pr := p.storedPR(ps.pr)
	fn(&pr)
	pr.Number, pr.RepoID = number, repoID
	if err := p.normalizePR(p.repos[repoID], &pr); err != nil {
		p.setupf("UpdatePR(%s, #%d): %v", repoID, number, err)
		return
	}
	ps.pr = pr
}

// DeleteBranch records that a branch of the repository was deleted: pull
// requests with that base lose BaseExists. The default branch cannot be
// deleted. In git mode an existing branch is deleted as a person would:
// open pull requests from it close too.
func (p *Platform) DeleteBranch(repoID, name string) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo("DeleteBranch", repoID)
	if s == nil {
		return
	}
	if name == "" || name == s.repo.DefaultBranch {
		p.setupf("DeleteBranch(%s, %q): not a deletable branch", repoID, name)
		return
	}
	if tip := s.refs[name]; tip != "" {
		ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
		err := p.repoGit(s).updateRef(ctx, name, "", tip)
		if err == nil {
			err = p.refsMoved(ctx, s, []refChange{{branch: name, old: tip}}, nil, false, false)
		}
		if err != nil {
			p.setupf("DeleteBranch(%s, %q): %v", repoID, name, err)
		}
		return
	}
	for _, ps := range s.prs {
		if ps.pr.Base == name {
			ps.pr.BaseExists = false
		}
	}
}

// normalizePR checks pr and fills its defaults (see AddPR).
func (p *Platform) normalizePR(s *repoState, pr *platform.PR) error {
	if pr.Head == "" {
		return errors.New("pull request without head")
	}
	if pr.Author.ID == "" {
		return errors.New("pull request without author id")
	}
	switch pr.State {
	case "":
		pr.State = platform.Open
	case platform.Open, platform.Closed, platform.Merged:
	default:
		return fmt.Errorf("unknown state %q", pr.State)
	}
	if pr.Base == "" {
		pr.Base = s.repo.DefaultBranch
	}
	if pr.CreatedAt.IsZero() {
		pr.CreatedAt = p.now()
	}
	if pr.State == platform.Open {
		pr.ClosedBy, pr.ClosedAt = nil, time.Time{}
	} else if pr.ClosedAt.IsZero() {
		pr.ClosedAt = p.now()
	}
	if pr.ClosedBy != nil {
		c := *pr.ClosedBy
		pr.ClosedBy = &c
	}
	if pr.URL == "" {
		pr.URL = p.prURL(s, pr.Number)
	}
	// In git mode an open PR follows its head branch, and a closed one
	// without a head commit takes the branch's.
	if tip := p.headTip(*pr); tip != "" && (pr.State == platform.Open || pr.HeadSHA == "") {
		pr.HeadSHA = tip
	}
	for _, l := range pr.Labels {
		if strings.TrimSpace(l) == "" {
			return errors.New("blank label")
		}
	}
	pr.Labels = dedupe(pr.Labels)
	for _, l := range pr.Labels {
		p.label(s, l, false)
	}
	return nil
}

// PR returns a stored pull request as the platform holds it, ClosedBy
// included whatever the flavor reports; the zero PR if there is none.
func (p *Platform) PR(repoID string, number int64) platform.PR {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.repos[repoID]; s != nil {
		if ps := s.prs[number]; ps != nil {
			return p.storedPR(ps.pr)
		}
	}
	return platform.PR{}
}

// PRList returns every pull request of the repository as PR does, by
// number.
func (p *Platform) PRList(repoID string) []platform.PR {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.repos[repoID]
	if s == nil {
		return nil
	}
	out := make([]platform.PR, 0, len(s.prs))
	for _, ps := range s.prs {
		out = append(out, p.storedPR(ps.pr))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// Comments returns the comments on a pull request, oldest first.
func (p *Platform) Comments(repoID string, number int64) []Comment {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.repos[repoID]
	if s == nil || s.prs[number] == nil {
		return nil
	}
	out := slices.Clone(s.prs[number].comments)
	for i := range out {
		out[i].Author = p.refresh(out[i].Author)
	}
	return out
}

// Labels returns the names of the repository's labels, sorted.
func (p *Platform) Labels(repoID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.repos[repoID]
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.labels))
	for name := range s.labels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Access.

// GrantWrite gives account write access to the repository: contents, pull
// requests and workflows.
func (p *Platform) GrantWrite(repoID string, account platform.Account) {
	p.Grant(repoID, account, platform.Perms{Contents: true, PRs: true, Workflows: true})
}

// Grant sets exactly the permissions account has on the repository; the
// zero Perms revokes them. Every account can read every repository.
func (p *Platform) Grant(repoID string, account platform.Account, perms platform.Perms) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo("Grant", repoID)
	if s == nil {
		return
	}
	if p.accounts[account.ID] == nil {
		p.setupf("Grant(%s): unknown account %q", repoID, account.ID)
		return
	}
	if perms == (platform.Perms{}) {
		delete(s.grants, account.ID)
		return
	}
	s.grants[account.ID] = perms
}

// Faults and the call log.

// FailNext makes the next call of method fail with err, without effect.
// Faults queue: FailNext twice fails the next two calls. method is a method
// name of platform.Reader, platform.Writer or platform.TargetWriter
// ("Probe", "ReadFile", "Target", "CreatePR", "Close", …), "Snapshot" for
// the snapshot source, "Push" or "Fetch" for requests of the git server,
// or "*" for any call. A nil err is ignored.
func (p *Platform) FailNext(method string, err error) {
	p.queue(method, fault{err: err})
}

// FailNextApplied makes the next call of method take effect and then fail
// with err, as when the response is lost after the platform acted.
func (p *Platform) FailNextApplied(method string, err error) {
	p.queue(method, fault{err: err, applied: true})
}

func (p *Platform) queue(method string, f fault) {
	if f.err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queued[method] = append(p.queued[method], f)
}

// Fail makes every call of method (or "*") fail with err, without effect,
// until Fail(method, nil). Queued faults fire first.
func (p *Platform) Fail(method string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.sticky, method)
		return
	}
	p.sticky[method] = err
}

// Injector decides the fault of one call of the platform, as a busy
// platform answers it: the error the call fails with (nil for none), and
// whether the call takes effect first, as with FailNextApplied (an answer
// lost after the platform acted, a timeout). method and args are those of
// the call log (Calls). It is consulted after the faults of FailNext and
// Fail, once the meter let the call's requests through, so a call it fails
// counts as sent; it runs with the Platform locked and must not call it.
// ctx is the call's: an injector may tell the throttle of ctx what headers
// the answer carried (throttle.Observe). It is not consulted for the
// requests of the git server (git mode).
type Injector func(ctx context.Context, method string, args []string) (err error, applied bool)

// Inject sets the injector of faults (nil for none).
func (p *Platform) Inject(fn Injector) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inject = fn
}

// takeFault returns the fault the next call of method meets, if any.
func (p *Platform) takeFault(method string) (fault, bool) {
	for _, key := range []string{method, "*"} {
		if q := p.queued[key]; len(q) > 0 {
			p.queued[key] = q[1:]
			return q[0], true
		}
	}
	for _, key := range []string{method, "*"} {
		if err := p.sticky[key]; err != nil {
			return fault{err: err}, true
		}
	}
	return fault{}, false
}

// SetIncompleteListings makes Resolve with a namespace and OpenPRsBy
// report Complete false, as when paging stops early; their results stay
// whole.
func (p *Platform) SetIncompleteListings(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.incomplete = on
}

// writeMethods are the entries of the call log that write.
var writeMethods = map[string]bool{"CreatePR": true, "EditPR": true, "Comment": true, "CreateLabel": true, "Push": true,
	"Commit": true, "UpdateRefs": true}

// Calls returns the call log, one entry per platform call in the order
// they were made, failed calls included: the method name and its
// arguments, e.g. "Probe", "Lookup acme-bot", "Resolve acme",
// "ReadFile acme/api .touchmark.yml", "PRs acme/api", "Target acme/api",
// "CreatePR acme/api", "EditPR acme/api #3", "Comment acme/api #3",
// "EnsureLabels acme/api", "Close acme/api", "Snapshot acme/api". A label
// created by CreatePR, EditPR or EnsureLabels adds "CreateLabel <repo>
// <name>". Calls cancelled before they started are not logged. In git
// mode, "Fetch <repo>" is a fetch or ls-remote and "Push <repo>
// <branch…>" a push with the branches it moved.
func (p *Platform) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.render(func(string) bool { return true })
}

// Writes returns the entries of Calls that write or try to: CreatePR,
// EditPR, Comment, CreateLabel, Push, and the Commit and UpdateRefs of an
// API commit.
func (p *Platform) Writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.render(func(m string) bool { return writeMethods[m] })
}

// OpenTargetWriters returns the repositories (paths, sorted, one entry per
// writer) of the per-target writers Target minted and nobody closed: a run
// closes each one after its target, which revokes its token.
func (p *Platform) OpenTargetWriters() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{}
	for _, t := range p.targetTokens {
		if t.closed {
			continue
		}
		path := t.repoID
		if s := p.repos[t.repoID]; s != nil {
			path = s.repo.Path
		}
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// ResetCalls empties the call log.
func (p *Platform) ResetCalls() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = nil
}

func (p *Platform) render(keep func(method string) bool) []string {
	out := []string{}
	for _, c := range p.calls {
		if keep(c.method) {
			out = append(out, strings.Join(append([]string{c.method}, c.args...), " "))
		}
	}
	return out
}

func (p *Platform) logCall(method string, args ...string) {
	p.calls = append(p.calls, call{method: method, args: args})
}

// Helpers. All run with p.mu held.

func (p *Platform) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	p.ticks++
	return Epoch.Add(time.Duration(p.ticks) * time.Second)
}

func (p *Platform) setupRepo(op, id string) *repoState {
	s := p.repos[id]
	if s == nil {
		p.setupf("%s: no repository with id %q", op, id)
	}
	return s
}

func (p *Platform) setupPR(op, repoID string, number int64) *prState {
	s := p.setupRepo(op, repoID)
	if s == nil {
		return nil
	}
	ps := s.prs[number]
	if ps == nil {
		p.setupf("%s(%s): no pull request #%d", op, repoID, number)
	}
	return ps
}

func (p *Platform) repoByPathLocked(path string) *repoState {
	return p.byPath[strings.ToLower(path)]
}

// repoView is a repository as the API reports it: empty without files, or
// in git mode without a commit on the default branch.
func (p *Platform) repoView(s *repoState) platform.Repo {
	r := s.repo
	r.Topics = slices.Clone(r.Topics)
	if p.git != nil {
		r.Empty = r.Empty || s.refs[r.DefaultBranch] == ""
	} else {
		r.Empty = r.Empty || len(s.entries) == 0
	}
	return r
}

// storedPR is a copy of pr with current account logins.
func (p *Platform) storedPR(pr platform.PR) platform.PR {
	pr.Labels = slices.Clone(pr.Labels)
	pr.Author = p.refresh(pr.Author)
	if pr.ClosedBy != nil {
		c := p.refresh(*pr.ClosedBy)
		pr.ClosedBy = &c
	}
	return pr
}

// prView is a pull request as the API reports it: without the closer of a
// PR closed unmerged when the flavor does not report closers.
func (p *Platform) prView(pr platform.PR) platform.PR {
	pr = p.storedPR(pr)
	if pr.State == platform.Closed && !p.caps.CloserKnown {
		pr.ClosedBy = nil
	}
	return pr
}

func (p *Platform) prURL(s *repoState, number int64) string {
	base := "https://" + p.host + "/" + s.repo.Path
	n := strconv.FormatInt(number, 10)
	switch Flavor(p.caps.Flavor) {
	case GitLab:
		return base + "/-/merge_requests/" + n
	case Gitea, Forgejo:
		return base + "/pulls/" + n
	}
	return base + "/pull/" + n
}

// label returns the id of label name in s, creating it when missing; log
// adds a CreateLabel entry for a new label. Ids are names unless the flavor
// labels by id.
func (p *Platform) label(s *repoState, name string, log bool) string {
	id, ok := s.labels[name]
	if !ok {
		s.lastLbl++
		id = strconv.FormatInt(s.lastLbl, 10)
		s.labels[name] = id
		if log {
			p.logCall("CreateLabel", s.repo.Path, name)
		}
	}
	if p.caps.LabelsByID {
		return id
	}
	return name
}

func cloneCaps(c platform.Caps) platform.Caps {
	c.RuntimeOnly = slices.Clone(c.RuntimeOnly)
	return c
}

// dedupe returns a copy of list without repeats, in first-seen order.
func dedupe(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// checkTreePath accepts what a git tree can hold as a path.
func checkTreePath(path string) error {
	if path == "" {
		return errors.New("empty path")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("path %q contains NUL", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q has an empty, \".\" or \"..\" segment", path)
		}
	}
	return nil
}

// checkRepoPath accepts "owner/name" and "group/sub/project".
func checkRepoPath(path string) error {
	if err := checkTreePath(path); err != nil {
		return fmt.Errorf("repository %w", err)
	}
	if !strings.Contains(path, "/") {
		return fmt.Errorf("repository path %q has no namespace", path)
	}
	return nil
}

func isHexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// Errors.

func notFound(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassNotFound, Status: http.StatusNotFound,
		Err: fmt.Errorf(format+": %w", append(args, platform.ErrNotFound)...)}
}

func invalid(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassInvalid, Status: http.StatusUnprocessableEntity,
		Err: fmt.Errorf(format, args...)}
}

func denied(op, rule, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassPermission, Status: http.StatusForbidden, Rule: rule,
		Err: fmt.Errorf(format, args...)}
}
