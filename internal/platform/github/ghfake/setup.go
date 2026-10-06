package ghfake

import (
	"context"
	"crypto/rsa"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Setup methods arrange the fake's state the way people and admins do on
// GitHub: through the web interface, not through the API under test.
// Their writes are never judged by Violations. They return an error
// wrapping nothing in particular when misused (an unknown account, a
// taken name, …).

// validName checks a login or repository name the fake accepts.
func validName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !nameByte(name[i]) {
			return false
		}
	}
	return true
}

// nameByte reports whether c may appear in a login or repository name.
func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// addAccount creates an account. Called with mu held.
func (s *Server) addAccount(login, typ string, plan Plan) (*account, error) {
	if !validName(strings.TrimSuffix(login, "[bot]")) {
		return nil, setupErr("invalid login %q", login)
	}
	if s.byLogin[strings.ToLower(login)] != nil {
		return nil, setupErr("login %q is taken", login)
	}
	if plan == "" {
		plan = PlanFree
	}
	a := &account{id: s.id(), login: login, typ: typ, name: login, plan: plan, created: s.now(), noreplyDomain: s.noreplyDomain()}
	s.accounts[a.id] = a
	s.byLogin[strings.ToLower(login)] = a
	return a, nil
}

// noreplyDomain is the domain of noreply commit addresses: github.com's,
// or users.noreply.<host> on GHES (assumed: GHES documents the noreply
// address without its form; the sandbox or a GHES administrator confirms
// it).
func (s *Server) noreplyDomain() string {
	if s.flavor != GHES {
		return "users.noreply.github.com"
	}
	u, err := url.Parse(s.http.URL)
	if err != nil {
		return "users.noreply.localhost"
	}
	return "users.noreply." + u.Hostname()
}

// AddUser creates a user on the free plan.
func (s *Server) AddUser(login string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.addAccount(login, TypeUser, PlanFree)
	return a.snapshot(), err
}

// AddOrg creates an organization on plan (PlanFree when empty).
func (s *Server) AddOrg(login string, plan Plan) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.addAccount(login, TypeOrganization, plan)
	return a.snapshot(), err
}

// Account returns an account by login (case-insensitive).
func (s *Server) Account(login string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(login)]
	return a.snapshot(), a != nil
}

// RenameAccount changes a user's or an organization's login. As on
// GitHub, the old login is free at once (the API does not redirect it),
// and so are the old paths of the account's repositories in the API (404:
// docs, "API requests that use the old organization's name will return a
// 404 error"), while git still reaches them there (docs: pushing to the
// old remote URL keeps working).
func (s *Server) RenameAccount(login, newLogin string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(login)]
	switch {
	case a == nil || a.typ == TypeBot:
		return Account{}, setupErr("RenameAccount: unknown account %q", login)
	case !validName(newLogin):
		return Account{}, setupErr("RenameAccount: invalid login %q", newLogin)
	case s.byLogin[strings.ToLower(newLogin)] != nil:
		return Account{}, setupErr("RenameAccount: login %q is taken", newLogin)
	}
	for _, id := range sortedIDs(s.repos) {
		if r := s.repos[id]; r.owner == a && !r.deleted {
			s.redirects[strings.ToLower(r.path())] = r.id
			s.apiGone[strings.ToLower(r.path())] = true
			delete(s.byPath, strings.ToLower(r.path()))
		}
	}
	delete(s.byLogin, strings.ToLower(login))
	a.login = newLogin
	s.byLogin[strings.ToLower(newLogin)] = a
	for _, id := range sortedIDs(s.repos) {
		if r := s.repos[id]; r.owner == a && !r.deleted {
			s.byPath[strings.ToLower(r.path())] = r
			delete(s.redirects, strings.ToLower(r.path()))
			delete(s.apiGone, strings.ToLower(r.path()))
		}
	}
	return a.snapshot(), nil
}

// AddEmail adds a commit email to an account (SSH-signed commits verify
// only when the committer email is the key owner's).
func (s *Server) AddEmail(login, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(login)]
	if a == nil {
		return setupErr("AddEmail: unknown account %q", login)
	}
	a.emails = append(a.emails, email)
	return nil
}

// AddSigningKey registers an "ssh-ed25519 AAAA…" public key as a signing
// key of an account.
func (s *Server) AddSigningKey(login, authorizedKey string) error {
	blob, err := parseAuthorizedKey(authorizedKey)
	if err != nil {
		return setupErr("AddSigningKey: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(login)]
	if a == nil {
		return setupErr("AddSigningKey: unknown account %q", login)
	}
	a.signingKeys = append(a.signingKeys, blob)
	return nil
}

// AppSpec describes a GitHub App to register.
type AppSpec struct {
	Slug  string // lowercase; the bot is "<slug>[bot]"
	Name  string // Slug when empty
	Owner string // the user or organization that owns the App
	// PublicKey verifies the App's JWTs.
	PublicKey *rsa.PublicKey
	// Permissions are what the App asks installations for.
	Permissions Permissions
}

// App is a registered GitHub App.
type App struct {
	ID       int64
	ClientID string
	Slug     string
	Name     string
	// Bot is the App's bot user, "<slug>[bot]", the author of what the
	// App's installation tokens write.
	Bot Account
}

// RegisterApp registers a GitHub App and creates its bot user.
func (s *Server) RegisterApp(spec AppSpec) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.byLogin[strings.ToLower(spec.Owner)]
	switch {
	case owner == nil || owner.typ == TypeBot:
		return App{}, setupErr("RegisterApp: unknown owner %q", spec.Owner)
	case spec.PublicKey == nil:
		return App{}, setupErr("RegisterApp: no public key")
	case !validName(spec.Slug) || strings.ToLower(spec.Slug) != spec.Slug:
		return App{}, setupErr("RegisterApp: invalid slug %q", spec.Slug)
	}
	for _, a := range s.apps {
		if a.slug == spec.Slug {
			return App{}, setupErr("RegisterApp: slug %q is taken", spec.Slug)
		}
	}
	bot, err := s.addAccount(spec.Slug+"[bot]", TypeBot, PlanFree)
	if err != nil {
		return App{}, err
	}
	name := spec.Name
	if name == "" {
		name = spec.Slug
	}
	perms := spec.Permissions.clone()
	perms["metadata"] = maxLevel(perms["metadata"], Read)
	a := &app{id: s.id(), clientID: "Iv23li" + randomAlnum(14), slug: spec.Slug, name: name, owner: owner,
		key: spec.PublicKey, perms: perms, bot: bot}
	bot.app = a
	s.apps[a.id] = a
	return a.snapshot(), nil
}

// snapshot returns the exported view of a.
func (a *app) snapshot() App {
	return App{ID: a.id, ClientID: a.clientID, Slug: a.slug, Name: a.name, Bot: a.bot.snapshot()}
}

// InstallSpec describes an installation.
type InstallSpec struct {
	App     string // slug
	Account string // the user or organization it is installed on
	// Permissions granted; the App's when nil. They cannot exceed the
	// App's.
	Permissions Permissions
	// Repos are the selected repositories by name; nil means all of the
	// account's, now and later.
	Repos []string
}

// Installation is an installed App.
type Installation struct {
	ID      int64
	AppID   int64
	Account Account
}

// Install installs an App on an account.
func (s *Server) Install(spec InstallSpec) (Installation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.appBySlug(spec.App)
	acct := s.byLogin[strings.ToLower(spec.Account)]
	switch {
	case a == nil:
		return Installation{}, setupErr("Install: unknown App %q", spec.App)
	case acct == nil || acct.typ == TypeBot:
		return Installation{}, setupErr("Install: unknown account %q", spec.Account)
	}
	for _, in := range s.installs {
		if in.app == a && in.account == acct {
			return Installation{}, setupErr("Install: %s is installed on %s already", a.slug, acct.login)
		}
	}
	perms := a.perms.clone()
	if spec.Permissions != nil {
		perms = spec.Permissions.clone()
		for k, v := range perms {
			if levelRank(v) > levelRank(a.perms[k]) {
				return Installation{}, setupErr("Install: %s=%s exceeds what the App asks for", k, v)
			}
		}
	}
	perms["metadata"] = maxLevel(perms["metadata"], Read)
	in := &installation{id: s.id(), app: a, account: acct, perms: perms, selection: "all", created: s.now()}
	if spec.Repos != nil {
		ids, err := s.repoIDs(acct, spec.Repos)
		if err != nil {
			return Installation{}, err
		}
		in.selection, in.repos = "selected", ids
	}
	s.installs[in.id] = in
	return Installation{ID: in.id, AppID: a.id, Account: acct.snapshot()}, nil
}

// repoIDs maps repository names of owner to ids. Called with mu held.
func (s *Server) repoIDs(owner *account, names []string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for _, name := range names {
		r := s.byPath[strings.ToLower(owner.login+"/"+name)]
		if r == nil {
			return nil, setupErr("unknown repository %s/%s", owner.login, name)
		}
		ids[r.id] = true
	}
	return ids, nil
}

// appBySlug finds an App. Called with mu held.
func (s *Server) appBySlug(slug string) *app {
	for _, a := range s.apps {
		if a.slug == slug {
			return a
		}
	}
	return nil
}

// SetInstallationRepos changes the repositories of an installation: nil
// for all of the account's.
func (s *Server) SetInstallationRepos(id int64, repos []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.installs[id]
	if in == nil {
		return setupErr("SetInstallationRepos: unknown installation %d", id)
	}
	if repos == nil {
		in.selection, in.repos = "all", nil
		return nil
	}
	ids, err := s.repoIDs(in.account, repos)
	if err != nil {
		return err
	}
	in.selection, in.repos = "selected", ids
	return nil
}

// SetInstallationPermissions changes what an installation grants, as when
// its owner accepts or declines new permissions. Tokens minted before keep
// theirs.
func (s *Server) SetInstallationPermissions(id int64, perms Permissions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.installs[id]
	if in == nil {
		return setupErr("SetInstallationPermissions: unknown installation %d", id)
	}
	p := perms.clone()
	p["metadata"] = maxLevel(p["metadata"], Read)
	in.perms = p
	return nil
}

// SuspendInstallation suspends or unsuspends an installation: its tokens
// stop working and no new ones are minted while it is suspended.
func (s *Server) SuspendInstallation(id int64, suspended bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.installs[id]
	if in == nil {
		return setupErr("SuspendInstallation: unknown installation %d", id)
	}
	in.suspended = suspended
	return nil
}

// InstallationToken mints an installation token without the API, for
// tests and fixtures: narrowed to repos by name (nil: all of the
// installation's) and perms (nil: all of the installation's).
func (s *Server) InstallationToken(id int64, repos []string, perms Permissions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.installs[id]
	if in == nil {
		return "", setupErr("InstallationToken: unknown installation %d", id)
	}
	var ids map[int64]bool
	if repos != nil {
		var err error
		if ids, err = s.repoIDs(in.account, repos); err != nil {
			return "", err
		}
	}
	return s.mintToken(in, ids, perms.cloneOrNil()).value, nil
}

// cloneOrNil copies p, keeping nil.
func (p Permissions) cloneOrNil() Permissions {
	if p == nil {
		return nil
	}
	return p.clone()
}

// PATSpec describes a personal access token.
type PATSpec struct {
	// FineGrained makes a fine-grained token ("github_pat_…") with
	// Permissions on Repos; otherwise a classic one ("ghp_…") with Scopes
	// ("repo", "public_repo", "workflow").
	FineGrained bool
	Scopes      []string
	Permissions Permissions
	// Repos are "owner/name" paths; nil means all the user's.
	Repos []string
	// Expires is when it stops working; zero is never.
	Expires time.Time
}

// AddPAT creates a personal access token of a user.
func (s *Server) AddPAT(login string, spec PATSpec) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byLogin[strings.ToLower(login)]
	if u == nil || u.typ != TypeUser {
		return "", setupErr("AddPAT: unknown user %q", login)
	}
	t := &token{kind: tokenClassicPAT, user: u, scopes: slices.Clone(spec.Scopes), issued: s.now(), expires: spec.Expires}
	t.value = "ghp_" + randomAlnum(36)
	if spec.FineGrained {
		t.kind, t.value, t.perms = tokenFineGrainedPAT, "github_pat_"+randomAlnum(22)+"_"+randomAlnum(59), spec.Permissions.clone()
		if spec.Repos != nil {
			t.repos = map[int64]bool{}
			for _, p := range spec.Repos {
				r := s.byPath[strings.ToLower(p)]
				if r == nil {
					return "", setupErr("AddPAT: unknown repository %q", p)
				}
				t.repos[r.id] = true
			}
		}
	}
	s.tokens[t.value] = t
	return t.value, nil
}

// RevokeToken revokes a token as its owner would.
func (s *Server) RevokeToken(value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.lookupToken(value)
	if t == nil {
		return setupErr("RevokeToken: unknown token")
	}
	t.revoked = true
	return nil
}

// TokenInfo describes a token for assertions.
type TokenInfo struct {
	// Value is the token itself, for tests that scan outputs for it.
	Value string
	// Kind is "installation", "classic" or "fine-grained".
	Kind           string
	InstallationID int64
	Login          string // the bot or the user
	// Repos are the ids the token is narrowed to, sorted; nil when it is
	// not narrowed.
	Repos       []int64
	Permissions Permissions
	ExpiresAt   time.Time
	Revoked     bool
}

// Token describes a token by value.
func (s *Server) Token(value string) (TokenInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.lookupToken(value)
	if t == nil {
		return TokenInfo{}, false
	}
	info := TokenInfo{Value: t.value, Kind: "classic", Login: t.user.login, Permissions: t.perms.cloneOrNil(), ExpiresAt: t.expires, Revoked: t.revoked}
	switch t.kind {
	case tokenInstallation:
		info.Kind, info.InstallationID = "installation", t.inst.id
	case tokenFineGrainedPAT:
		info.Kind = "fine-grained"
	}
	if t.repos != nil {
		info.Repos = sortedIDs(t.repos)
	}
	return info, true
}

// Tokens returns every installation token minted for an installation, in
// minting order, for tests that check narrowing and revocation.
func (s *Server) Tokens(installationID int64) []TokenInfo {
	s.mu.Lock()
	var values []*token
	for _, t := range s.tokens {
		if t.kind == tokenInstallation && t.inst.id == installationID {
			values = append(values, t)
		}
	}
	s.mu.Unlock()
	sort.Slice(values, func(i, j int) bool { return values[i].issued.Before(values[j].issued) })
	out := make([]TokenInfo, 0, len(values))
	for _, t := range values {
		info, _ := s.Token(t.value)
		out = append(out, info)
	}
	return out
}

// File is a tree entry of a commit made by a setup method.
type File struct {
	Path string
	// Mode is ModeFile (also when empty), ModeExecutable, ModeSymlink
	// (Content is the target) or ModeGitlink (Content is the commit id).
	Mode    string
	Content []byte
}

// RepoSpec describes a repository to create.
type RepoSpec struct {
	Owner, Name string
	// Visibility is "public" (the default), "private" or "internal".
	Visibility string
	// DefaultBranch is "main" when empty.
	DefaultBranch string
	// Files are committed to the default branch by the owner; none leaves
	// the repository empty.
	Files    []File
	Topics   []string
	Archived bool
	Disabled bool
	Template bool
	// PRsDisabled turns pull requests off (has_pull_requests false).
	PRsDisabled bool
	// NoDrafts refuses draft pull requests (422), as private repositories
	// of owners on the free plan always do.
	NoDrafts bool
	// ObjectFormat is "sha1" (the default) or "sha256".
	ObjectFormat string
	// Mirror is the mirror_url of a mirror.
	Mirror string
}

// CreateRepo creates a repository.
func (s *Server) CreateRepo(spec RepoSpec) (Repo, error) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.byLogin[strings.ToLower(spec.Owner)]
	if owner == nil || owner.typ == TypeBot {
		return Repo{}, setupErr("CreateRepo: unknown owner %q", spec.Owner)
	}
	r, err := s.newRepo(owner, spec)
	if err != nil {
		return Repo{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if len(spec.Files) > 0 {
		changes := make([]fileChange, len(spec.Files))
		for i, f := range spec.Files {
			changes[i] = toChange(f)
		}
		if _, err := s.commitAs(ctx, r, r.defaultBranch, "", changes, owner, "Initial commit\n", false); err != nil {
			return Repo{}, err
		}
	}
	r.archived = spec.Archived
	return r.snapshot(), nil
}

// newRepo registers and initializes a repository. Called with gitMu and mu
// held.
func (s *Server) newRepo(owner *account, spec RepoSpec) (*repo, error) {
	if !validName(spec.Name) {
		return nil, setupErr("invalid repository name %q", spec.Name)
	}
	path := strings.ToLower(owner.login + "/" + spec.Name)
	if s.byPath[path] != nil {
		return nil, setupErr("repository %s/%s exists", owner.login, spec.Name)
	}
	vis := spec.Visibility
	switch vis {
	case "":
		vis = "public"
	case "public", "private", "internal":
	default:
		return nil, setupErr("invalid visibility %q", vis)
	}
	format := spec.ObjectFormat
	switch format {
	case "":
		format = "sha1"
	case "sha1", "sha256":
	default:
		return nil, setupErr("invalid object format %q", format)
	}
	branch := spec.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	r := &repo{id: s.id(), owner: owner, name: spec.Name, visibility: vis, defaultBranch: branch,
		disabled: spec.Disabled, template: spec.Template, prsDisabled: spec.PRsDisabled, prPolicy: "all",
		noDrafts: spec.NoDrafts, objectFormat: format, mirror: spec.Mirror, topics: slices.Clone(spec.Topics),
		collaborators: map[int64]string{}, created: s.now(), pushed: s.now()}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if err := s.initRepo(ctx, r); err != nil {
		return nil, err
	}
	s.repos[r.id] = r
	s.byPath[path] = r
	delete(s.redirects, path)
	return r, nil
}

// toChange converts a File.
func toChange(f File) fileChange {
	mode := f.Mode
	if mode == "" {
		mode = ModeFile
	}
	c := fileChange{path: f.Path, mode: mode, data: f.Content}
	if mode == ModeGitlink {
		c.oid, c.data = strings.TrimSpace(string(f.Content)), nil
	}
	return c
}

// commitAs commits changes on branch as a person (author and committer
// by), from base when the branch is new ("" for the default branch), and
// moves the branch; signed makes it a web-flow signed commit, as GitHub
// signs commits made in the web interface. Called with gitMu and mu held.
func (s *Server) commitAs(ctx context.Context, r *repo, branch, base string, changes []fileChange, by *account, msg string, signed bool) (string, error) {
	rg := s.repoGit(r)
	old, err := rg.branch(ctx, branch)
	if err != nil {
		return "", err
	}
	parent := old
	if parent == "" {
		if base == "" {
			base = r.defaultBranch
		}
		if id, ok, err := rg.resolve(ctx, base); err != nil {
			return "", err
		} else if ok {
			parent = id
		}
	}
	tree, err := rg.buildTree(ctx, parent, changes)
	if err != nil {
		return "", err
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	who := ident{name: by.login, email: by.noreply(), when: s.now()}
	committer := who
	if signed {
		committer = ident{name: webFlowName, email: webFlowEmail, when: who.when}
	}
	text := commitText(tree, parents, who, committer, "", msg)
	if signed {
		text = commitText(tree, parents, who, committer, s.webFlowSign(text), msg)
	}
	id, err := rg.writeObject(ctx, "commit", []byte(text))
	if err != nil {
		return "", err
	}
	ref := "refs/heads/" + branch
	if err := rg.updateRefs(ctx, []refTx{{ref: ref, old: old, new: id}}); err != nil {
		return "", err
	}
	return id, s.refsMoved(ctx, r, []refChange{{ref: ref, old: old, new: id}}, by, false)
}

// CommitSpec is a person's commit.
type CommitSpec struct {
	// Branch is created from From ("" for the default branch; a branch or
	// a commit id) when absent.
	Branch, From string
	Files        []File
	Delete       []string
	// Author is the login of the person; the owner when empty.
	Author  string
	Message string
	// Signed makes a commit GitHub signed (verification "valid"), as for
	// commits made in the web interface.
	Signed bool
}

// Commit makes a person's commit and returns its id.
func (s *Server) Commit(path string, spec CommitSpec) (string, error) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return "", setupErr("Commit: unknown repository %q", path)
	}
	by := r.owner
	if spec.Author != "" {
		by = s.byLogin[strings.ToLower(spec.Author)]
		if by == nil {
			return "", setupErr("Commit: unknown account %q", spec.Author)
		}
	}
	if spec.Branch == "" {
		spec.Branch = r.defaultBranch
	}
	var changes []fileChange
	for _, f := range spec.Files {
		changes = append(changes, toChange(f))
	}
	for _, p := range spec.Delete {
		changes = append(changes, fileChange{path: p})
	}
	msg := spec.Message
	if msg == "" {
		msg = "Update files\n"
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return s.commitAs(ctx, r, spec.Branch, spec.From, changes, by, msg, spec.Signed)
}

// SetBranch creates, moves (also by force) or, with an empty id, deletes a
// branch as a person.
func (s *Server) SetBranch(path, branch, id, by string) error {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return setupErr("SetBranch: unknown repository %q", path)
	}
	who := s.byLogin[strings.ToLower(by)]
	if who == nil {
		return setupErr("SetBranch: unknown account %q", by)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	rg := s.repoGit(r)
	old, err := rg.branch(ctx, branch)
	if err != nil {
		return err
	}
	if id != "" {
		resolved, ok, err := rg.resolve(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return setupErr("SetBranch: %s has no commit %q", path, id)
		}
		id = resolved
	}
	if old == id {
		return nil
	}
	ref := "refs/heads/" + branch
	if err := rg.updateRefs(ctx, []refTx{{ref: ref, old: old, new: id}}); err != nil {
		return err
	}
	return s.refsMoved(ctx, r, []refChange{{ref: ref, old: old, new: id}}, who, false)
}

// SetMaxTreeEntries changes the truncation threshold of recursive tree
// listings (Options.MaxTreeEntries).
func (s *Server) SetMaxTreeEntries(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.MaxTreeEntries = n
}

// Branch returns the tip of a branch, "" when it does not exist.
func (s *Server) Branch(path, branch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return s.tip(ctx, r, branch)
}

// GitDir returns the bare repository of a repository, for tests that
// inspect it with git.
func (s *Server) GitDir(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.byPath[strings.ToLower(path)]; r != nil {
		return r.dir
	}
	return ""
}

// Repo returns a repository by path (case-insensitive).
func (s *Server) Repo(path string) (Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return Repo{}, false
	}
	return r.snapshot(), true
}

// repoForSetup finds a repository by path. Called with mu held.
func (s *Server) repoForSetup(op, path string) (*repo, error) {
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return nil, setupErr("%s: unknown repository %q", op, path)
	}
	return r, nil
}

// Fork forks a repository into owner's account with all its branches.
func (s *Server) Fork(path, owner string) (Repo, error) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.repoForSetup("Fork", path)
	if err != nil {
		return Repo{}, err
	}
	acct := s.byLogin[strings.ToLower(owner)]
	if acct == nil || acct.typ == TypeBot {
		return Repo{}, setupErr("Fork: unknown owner %q", owner)
	}
	r, err := s.newRepo(acct, RepoSpec{Name: src.name, Visibility: src.visibility, DefaultBranch: src.defaultBranch,
		ObjectFormat: src.objectFormat})
	if err != nil {
		return Repo{}, err
	}
	r.parent = src
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if _, err := s.repoGit(r).g.Run(ctx, nil, "fetch", "-q", "--no-tags", src.dir, "+refs/heads/*:refs/heads/*"); err != nil {
		return Repo{}, err
	}
	return r.snapshot(), nil
}

// RenameRepo renames a repository in its account. The old path keeps
// answering with redirects (REST 301 for GET, 307 otherwise).
func (s *Server) RenameRepo(path, newName string) (Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("RenameRepo", path)
	if err != nil {
		return Repo{}, err
	}
	return s.move(r, r.owner, newName)
}

// TransferRepo moves a repository to another account, keeping its name.
func (s *Server) TransferRepo(path, newOwner string) (Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("TransferRepo", path)
	if err != nil {
		return Repo{}, err
	}
	acct := s.byLogin[strings.ToLower(newOwner)]
	if acct == nil || acct.typ == TypeBot {
		return Repo{}, setupErr("TransferRepo: unknown owner %q", newOwner)
	}
	return s.move(r, acct, r.name)
}

// move renames or transfers r. Called with mu held.
func (s *Server) move(r *repo, owner *account, name string) (Repo, error) {
	if !validName(name) {
		return Repo{}, setupErr("invalid repository name %q", name)
	}
	to := strings.ToLower(owner.login + "/" + name)
	if other := s.byPath[to]; other != nil && other != r {
		return Repo{}, setupErr("repository %s/%s exists", owner.login, name)
	}
	from := strings.ToLower(r.path())
	delete(s.byPath, from)
	s.redirects[from] = r.id
	delete(s.apiGone, from)
	r.owner, r.name = owner, name
	s.byPath[to] = r
	delete(s.redirects, to)
	delete(s.apiGone, to)
	for _, in := range s.installs {
		if in.repos[r.id] && in.account != owner {
			delete(in.repos, r.id)
		}
	}
	return r.snapshot(), nil
}

// DeleteRepo deletes a repository. Pull requests from it keep their head
// label, with no head repository.
func (s *Server) DeleteRepo(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("DeleteRepo", path)
	if err != nil {
		return err
	}
	r.deleted = true
	delete(s.byPath, strings.ToLower(r.path()))
	for _, other := range s.repos {
		for _, p := range other.prs {
			if p.headRepo == r {
				p.headRepo = nil
				if p.open && other != r {
					s.finish(p, false, nil)
				}
			}
		}
	}
	return nil
}

// SetArchived archives or unarchives a repository.
func (s *Server) SetArchived(path string, archived bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("SetArchived", path)
	if err != nil {
		return err
	}
	r.archived = archived
	return nil
}

// SetTopics replaces the topics of a repository.
func (s *Server) SetTopics(path string, topics ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("SetTopics", path)
	if err != nil {
		return err
	}
	r.topics = slices.Clone(topics)
	return nil
}

// SetVisibility changes a repository's visibility.
func (s *Server) SetVisibility(path, visibility string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("SetVisibility", path)
	if err != nil {
		return err
	}
	switch visibility {
	case "public", "private", "internal":
	default:
		return setupErr("SetVisibility: invalid visibility %q", visibility)
	}
	r.visibility = visibility
	return nil
}

// Grant gives a user a role on a repository ("read", "write" or "admin");
// "" removes it.
func (s *Server) Grant(path, login, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("Grant", path)
	if err != nil {
		return err
	}
	u := s.byLogin[strings.ToLower(login)]
	if u == nil {
		return setupErr("Grant: unknown account %q", login)
	}
	if role == "" {
		delete(r.collaborators, u.id)
		return nil
	}
	if levelRank(role) == 0 {
		return setupErr("Grant: invalid role %q", role)
	}
	r.collaborators[u.id] = role
	return nil
}

// PRSpec is a person's pull request.
type PRSpec struct {
	// Head is a branch of HeadRepo ("" for the repository itself: a fork's
	// "owner/name" otherwise); Base is "" for the default branch.
	Head, Base string
	HeadRepo   string
	Title      string
	Body       string
	Draft      bool
	Labels     []string
	Author     string // login
}

// PR is a pull request as the fake knows it.
type PR struct {
	ID, Number    int64
	State         string // "open" or "closed"
	Merged, Draft bool
	Head          string
	HeadSHA       string
	HeadRepoID    int64 // 0 once the head repository is deleted
	Base          string
	BaseSHA       string
	MergeSHA      string
	Title, Body   string
	Labels        []string
	Author        Account
	ClosedBy      *Account // the actor of the last close, nil when none is named
	MergedBy      *Account
	CreatedAt     time.Time
	ClosedAt      time.Time
	MergedAt      time.Time
	Events        []Event
}

// Event is a timeline event.
type Event struct {
	Type        string // EventClosed, EventMerged, …
	Actor       *Account
	At          time.Time
	StateReason string
}

// Comment is an issue comment on a pull request.
type Comment struct {
	ID        int64
	Author    Account
	Body      string
	CreatedAt time.Time
}

// snapshot returns the exported view of p.
func (p *pr) snapshot() PR {
	out := PR{ID: p.id, Number: p.number, State: "open", Merged: p.merged, Draft: p.draft, Head: p.headRef,
		HeadSHA: p.headSHA, Base: p.baseRef, BaseSHA: p.baseSHA, MergeSHA: p.mergeSHA, Title: p.title, Body: p.body,
		Author: p.author.snapshot(), CreatedAt: p.created, ClosedAt: p.closedAt, MergedAt: p.mergedAt}
	if !p.open {
		out.State = "closed"
	}
	if p.headRepo != nil {
		out.HeadRepoID = p.headRepo.id
	}
	for _, l := range p.labels {
		out.Labels = append(out.Labels, l.name)
	}
	if !p.open {
		if c := p.closer(); c != nil {
			snap := c.snapshot()
			out.ClosedBy = &snap
		}
	}
	if p.mergedBy != nil {
		snap := p.mergedBy.snapshot()
		out.MergedBy = &snap
	}
	for _, e := range p.events {
		ev := Event{Type: e.typ, At: e.at, StateReason: e.stateReason}
		if e.actor != nil {
			snap := e.actor.snapshot()
			ev.Actor = &snap
		}
		out.Events = append(out.Events, ev)
	}
	return out
}

// OpenPR opens a pull request as a person.
func (s *Server) OpenPR(path string, spec PRSpec) (PR, error) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("OpenPR", path)
	if err != nil {
		return PR{}, err
	}
	author := s.byLogin[strings.ToLower(spec.Author)]
	if author == nil {
		return PR{}, setupErr("OpenPR: unknown author %q", spec.Author)
	}
	np := newPR{head: spec.Head, base: spec.Base, title: spec.Title, body: spec.Body, draft: spec.Draft, author: author}
	if np.base == "" {
		np.base = r.defaultBranch
	}
	if spec.HeadRepo != "" {
		if np.headRepo, err = s.repoForSetup("OpenPR", spec.HeadRepo); err != nil {
			return PR{}, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	p, refusal := s.openPR(ctx, r, np)
	if refusal != nil {
		return PR{}, setupErr("OpenPR: %s", describe(*refusal))
	}
	for _, name := range spec.Labels {
		s.labelPR(p, name)
	}
	return p.snapshot(), nil
}

// describe returns the messages of a REST refusal.
func describe(resp response) string {
	m, _ := resp.body.(map[string]any)
	msg, _ := m["message"].(string)
	if list, ok := m["errors"].([]any); ok {
		for _, e := range list {
			em, _ := e.(map[string]any)
			if t, ok := em["message"].(string); ok {
				msg += ": " + t
			} else {
				msg += ": " + strings.TrimSpace(strings.Join([]string{str(em["field"]), str(em["code"])}, " "))
			}
		}
	}
	return strconv.Itoa(resp.status) + " " + msg
}

// str formats a JSON value as a string.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// prForSetup finds a pull request. Called with mu held.
func (s *Server) prForSetup(op, path string, number int64) (*pr, error) {
	r, err := s.repoForSetup(op, path)
	if err != nil {
		return nil, err
	}
	for _, p := range r.prs {
		if p.number == number {
			return p, nil
		}
	}
	return nil, setupErr("%s: %s has no pull request #%d", op, path, number)
}

// SetPRState closes ("closed") or reopens ("open") a pull request as a
// person, with GitHub's checks.
func (s *Server) SetPRState(path string, number int64, state, by string) error {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.prForSetup("SetPRState", path, number)
	if err != nil {
		return err
	}
	who := s.byLogin[strings.ToLower(by)]
	if who == nil {
		return setupErr("SetPRState: unknown account %q", by)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if refusal := s.editPR(ctx, p, prEdit{state: &state}, who); refusal != nil {
		return setupErr("SetPRState: %s", describe(*refusal))
	}
	return nil
}

// PREdit is a person's edit of a pull request; nil fields stay.
type PREdit struct {
	Title, Body *string
	Draft       *bool
	AddLabels   []string
	// RemoveLabels removes labels from the pull request.
	RemoveLabels []string
}

// EditPR edits a pull request as a person: title, body, draft state and
// labels.
func (s *Server) EditPR(path string, number int64, by string, e PREdit) error {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.prForSetup("EditPR", path, number)
	if err != nil {
		return err
	}
	who := s.byLogin[strings.ToLower(by)]
	if who == nil {
		return setupErr("EditPR: unknown account %q", by)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if refusal := s.editPR(ctx, p, prEdit{title: e.Title, body: e.Body}, who); refusal != nil {
		return setupErr("EditPR: %s", describe(*refusal))
	}
	if e.Draft != nil {
		p.draft = *e.Draft
	}
	for _, name := range e.AddLabels {
		s.labelPR(p, name)
	}
	p.labels = slices.DeleteFunc(p.labels, func(l *label) bool {
		return slices.ContainsFunc(e.RemoveLabels, func(n string) bool { return strings.EqualFold(n, l.name) })
	})
	return nil
}

// CommentPR adds a person's comment to a pull request.
func (s *Server) CommentPR(path string, number int64, by, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.prForSetup("CommentPR", path, number)
	if err != nil {
		return err
	}
	who := s.byLogin[strings.ToLower(by)]
	if who == nil {
		return setupErr("CommentPR: unknown account %q", by)
	}
	p.comments = append(p.comments, &comment{id: s.id(), author: who, body: body, created: s.now()})
	return nil
}

// MergeMethod is how a pull request is merged.
type MergeMethod string

// Merge methods.
const (
	MergeCommit MergeMethod = "merge"
	MergeSquash MergeMethod = "squash"
	MergeRebase MergeMethod = "rebase"
)

// MergePR merges a pull request as a person and returns the new tip of its
// base (see the merge rules of the REST endpoint).
func (s *Server) MergePR(path string, number int64, how MergeMethod, by string) (string, error) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.prForSetup("MergePR", path, number)
	if err != nil {
		return "", err
	}
	who := s.byLogin[strings.ToLower(by)]
	if who == nil {
		return "", setupErr("MergePR: unknown account %q", by)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	sha, refusal := s.mergePR(ctx, p, how, who, "", false)
	if refusal != nil {
		return "", setupErr("MergePR: %s", describe(*refusal))
	}
	return sha, nil
}

// GetPR returns a pull request.
func (s *Server) GetPR(path string, number int64) (PR, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.prForSetup("GetPR", path, number)
	if err != nil {
		return PR{}, false
	}
	return p.snapshot(), true
}

// PRs returns the pull requests of a repository, oldest first.
func (s *Server) PRs(path string) []PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return nil
	}
	out := make([]PR, len(r.prs))
	for i, p := range r.prs {
		out[i] = p.snapshot()
	}
	return out
}

// Comments returns the comments of a pull request, oldest first.
func (s *Server) Comments(path string, number int64) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.prForSetup("Comments", path, number)
	if err != nil {
		return nil
	}
	out := make([]Comment, len(p.comments))
	for i, c := range p.comments {
		out[i] = Comment{ID: c.id, Author: c.author.snapshot(), Body: c.body, CreatedAt: c.created}
	}
	return out
}

// Labels returns the labels of a repository by name, in creation order.
func (s *Server) Labels(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return nil
	}
	out := make([]string, len(r.labels))
	for i, l := range r.labels {
		out[i] = l.name
	}
	return out
}
