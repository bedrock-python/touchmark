package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of repository listings.
const (
	// maxRepoPages bounds a namespace listing: 40 000 repositories. A
	// larger one resolves incompletely.
	maxRepoPages = 400
	// lookups is how many repositories Resolve inspects at once (object
	// format, emptiness).
	lookups = 4
)

// apiRepo is a repository as the API reports it (the full repository of
// GET /repos/{owner}/{repo} and the minimal one of listings).
type apiRepo struct {
	ID              int64           `json:"id"`
	NodeID          string          `json:"node_id"`
	FullName        string          `json:"full_name"`
	DefaultBranch   string          `json:"default_branch"`
	HTMLURL         string          `json:"html_url"`
	Private         bool            `json:"private"`
	Visibility      string          `json:"visibility"`
	Archived        bool            `json:"archived"`
	Disabled        bool            `json:"disabled"`
	Fork            bool            `json:"fork"`
	MirrorURL       *string         `json:"mirror_url"`
	Size            int64           `json:"size"`
	HasPullRequests *bool           `json:"has_pull_requests"`
	Topics          []string        `json:"topics"`
	Permissions     *apiPermissions `json:"permissions"`
}

// apiPermissions are the caller's role on a repository.
type apiPermissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Pull     bool `json:"pull"`
}

// canPush reports whether the role may push and write pull requests.
func (p *apiPermissions) canPush() bool { return p != nil && (p.Push || p.Maintain || p.Admin) }

// check reports what the driver depends on that a repository lacks.
func (r *apiRepo) check(op string) error {
	if r.ID <= 0 || r.FullName == "" {
		return shapeError(op, "a repository without id or full_name")
	}
	if _, _, ok := splitRepoPath(r.FullName); !ok {
		return shapeError(op, "repository full_name %q is not owner/name", r.FullName)
	}
	return nil
}

// facts are what a repository's listing does not tell: its object format
// and whether it has commits.
type facts struct {
	format string
	empty  bool
}

// toRepo converts an API repository of the provider's host.
//
// Visibility is the API's (public, private, internal), else private or
// public from "private". A repository with a mirror_url is a mirror, one
// whose has_pull_requests is false has pull requests turned off (the field
// is in the REST descriptions of github.com, GHE.com and GHES 3.19; absent,
// they are on). pull_request_creation_policy "collaborators_only" is not a
// skip: whether an installed App counts as a collaborator is unverified.
func (c *client) toRepo(r *apiRepo, f facts) platform.Repo {
	visibility := strings.ToLower(r.Visibility)
	switch visibility {
	case "public", "private", "internal":
	default:
		visibility = "public"
		if r.Private {
			visibility = "private"
		}
	}
	format := f.format
	if format == "" {
		format = "sha1"
	}
	return platform.Repo{
		Host:          c.host,
		ID:            strconv.FormatInt(r.ID, 10),
		Path:          r.FullName,
		DefaultBranch: r.DefaultBranch,
		WebURL:        r.HTMLURL,
		Visibility:    visibility,
		ObjectFormat:  format,
		Archived:      r.Archived,
		Disabled:      r.Disabled,
		Empty:         f.empty,
		Mirror:        r.MirrorURL != nil && *r.MirrorURL != "",
		Fork:          r.Fork,
		PRsDisabled:   r.HasPullRequests != nil && !*r.HasPullRequests,
		Topics:        slices.Clone(r.Topics),
	}
}

// Repo returns one repository by path; paths compare case-insensitively
// (GitHub's are), and the old path of a renamed or transferred repository
// still leads to it (GitHub redirects, call follows), under its canonical
// path. With an App, the repository's owner must have the App installed;
// when the owner named has none, GET /repos/{owner}/{repo}/installation
// finds the installation of a repository transferred to an owner that has
// one. An owner's rename leaves no API redirect (docs: API requests with
// the old organization name answer 404): the target is then missing, and
// the sweep leaves pull requests alone in repositories of its name
// (decide.Sweep).
func (d *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	const op = "get repository"
	owner, name, ok := splitRepoPath(path)
	if !ok {
		return platform.Repo{}, notFound(op, "repository %q: GitHub paths are owner/name", path)
	}
	a, err := d.c.ownerAuth(ctx, owner)
	if err != nil && d.c.kind == credApp && platform.ClassOf(err) == platform.ClassNotFound {
		// The owner has no installation: the repository may have moved to
		// one that has (a transfer keeps a redirect from the old path).
		var tok string
		if tok, err = d.c.app.repoToken(ctx, op, owner, name); err == nil {
			a = d.c.staticAuth("Bearer " + tok)
		}
	}
	if err != nil {
		return platform.Repo{}, err
	}
	var r apiRepo
	if _, err := d.c.get(ctx, op, d.c.repoURL(owner, name), nil, a, &r); err != nil {
		return platform.Repo{}, err
	}
	if err := r.check(op); err != nil {
		return platform.Repo{}, err
	}
	f, err := d.c.facts(ctx, op, a, &r)
	if err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(&r, f), nil
}

// facts finds a repository's object format and whether it is empty.
//
// A repository without commits reports size 0, but so does one whose size
// GitHub has not computed yet (hourly): for size 0 the default branch is
// asked for (GET /branches/{b}: 404 without commits). The object format is
// GET /hash-algorithm on github.com and GHE.com; GHES 3.19 lacks the
// endpoint (404): its repositories are SHA-1, and one 404 there spares the
// rest. A format once found is kept for the run.
func (c *client) facts(ctx context.Context, op string, a *httpx.Auth, r *apiRepo) (facts, error) {
	owner, name, _ := splitRepoPath(r.FullName)
	var f facts
	if r.Size == 0 {
		if r.DefaultBranch == "" {
			f.empty = true
		} else {
			_, err := c.get(ctx, op, c.repoURL(owner, name, "branches", r.DefaultBranch), nil, a, nil)
			switch {
			case err == nil:
			case platform.ClassOf(err) == platform.ClassNotFound, statusOf(err) == http.StatusConflict:
				f.empty = true
			default:
				return facts{}, err
			}
		}
	}
	c.mu.Lock()
	format, known := c.formats[r.ID]
	skip := c.flavor == flavorGHES && c.noHash
	c.mu.Unlock()
	switch {
	case known:
		f.format = format
		return f, nil
	case skip:
		f.format = "sha1"
		return f, nil
	}
	var h struct {
		HashAlgorithm string `json:"hash_algorithm"`
	}
	_, err := c.get(ctx, op, c.repoURL(owner, name, "hash-algorithm"), nil, a, &h)
	switch {
	case err == nil && (h.HashAlgorithm == "sha1" || h.HashAlgorithm == "sha256"):
		f.format = h.HashAlgorithm
	case err == nil:
		return facts{}, shapeError(op, "GET /repos/%s/hash-algorithm: %q", r.FullName, h.HashAlgorithm)
	case platform.ClassOf(err) == platform.ClassNotFound:
		f.format = "sha1"
		if c.flavor == flavorGHES {
			c.mu.Lock()
			c.noHash = true
			c.mu.Unlock()
		}
		return f, nil
	default:
		return facts{}, err
	}
	c.mu.Lock()
	c.formats[r.ID] = f.format
	c.mu.Unlock()
	return f, nil
}

// Resolve lists the repositories sel selects, sorted by path ignoring case.
//
// A namespace is an organization (GET /orgs/{org}/repos, type all) or else
// a user; GitHub has no nested namespaces, so Subgroups changes nothing.
// Topics compare ignoring case and must all be present (the listing
// carries them); forks only with sel.Forks; templates are ordinary
// repositories.
//
// GET /users/{user}/repos lists a user's public repositories only (docs:
// rest/repos/repos#list-repositories-for-a-user), so a user's namespace is
// listed where its private repositories show: with an App, GET
// /installation/repositories with the token of the user's installation,
// keeping the user's repositories; with a token that acts as the user
// (Self), GET /user/repos?affiliation=owner. Any other token gets the
// public listing, which is then incomplete (Resolved.Incomplete says why):
// private repositories it may write to are missing, and a sweep over it
// could close their pull requests. An anonymous reader sees public
// repositories only anyway: its listing is complete.
//
// With an App the listing goes through the installation of the namespace,
// so it holds the repositories the installation covers, and possibly the
// organization's public ones it does not (a community report, unverified):
// a writer that cannot write to one fails its Target with ClassNotFound,
// visible in the report. A namespace without the App installed is
// ClassNotFound.
//
// The listing is incomplete when it is capped or a later page fails; a
// failure of the first page, a rate limit, an auth error or a transient
// failure is returned instead, so the caller can retry or pause. A
// repository whose facts cannot be read for another reason is left out,
// and the result is incomplete.
func (d *reader) Resolve(ctx context.Context, sel platform.Selector) (platform.Resolved, error) {
	const op = "resolve"
	if sel.Repo != "" {
		r, err := d.Repo(ctx, sel.Repo)
		if err != nil {
			return platform.Resolved{}, err
		}
		return platform.Resolved{Repos: []platform.Repo{r}, Complete: true}, nil
	}
	switch {
	case sel.Namespace == "":
		return platform.Resolved{}, invalid(op, "the selector names no repository and no namespace")
	case strings.ContainsAny(sel.Namespace, "/\x00?#\\") || sel.Namespace == "." || sel.Namespace == "..":
		return platform.Resolved{}, notFound(op, "namespace %q: GitHub has no nested namespaces", sel.Namespace)
	}
	a, err := d.c.ownerAuth(ctx, sel.Namespace)
	if err != nil {
		return platform.Resolved{}, err
	}
	var found []apiRepo
	collect := func(r apiRepo) error {
		if err := r.check(op); err != nil {
			return err
		}
		found = append(found, r)
		return nil
	}
	why := ""
	complete, err := listAll(ctx, d.c, op, d.c.endpoint("orgs", sel.Namespace, "repos"), url.Values{"type": {"all"}}, a, maxRepoPages, collect)
	if err != nil && platform.ClassOf(err) == platform.ClassNotFound && !laterPage(err) {
		// Not an organization: a user's repositories.
		found = nil
		complete, why, err = d.userRepos(ctx, op, sel.Namespace, a, collect)
	}
	if err != nil {
		if !laterPage(err) || fatal(err) {
			return platform.Resolved{}, err
		}
		complete = false
	}
	var chosen []*apiRepo
	seen := map[int64]bool{}
	for i := range found {
		r := &found[i]
		if seen[r.ID] || r.Fork && !sel.Forks || !hasTopics(r.Topics, sel.Topics) {
			continue
		}
		seen[r.ID] = true
		chosen = append(chosen, r)
	}
	all, errs := d.c.allFacts(ctx, op, a, chosen)
	out := platform.Resolved{Repos: []platform.Repo{}, Complete: complete}
	if !complete {
		out.Incomplete = why
	}
	for i, r := range chosen {
		switch err := errs[i]; {
		case err == nil:
			out.Repos = append(out.Repos, d.c.toRepo(r, all[i]))
		case fatal(err):
			return platform.Resolved{}, err
		default:
			out.Complete = false
		}
	}
	sortRepos(out.Repos)
	return out, nil
}

// userRepos lists the repositories of user namespace user for Resolve,
// handing each to collect; why says why the listing is incomplete when it
// is only because the credential cannot see the user's private
// repositories (see Resolve).
func (d *reader) userRepos(ctx context.Context, op, user string, a *httpx.Auth, collect func(apiRepo) error) (complete bool, why string, err error) {
	c := d.c
	switch c.kind {
	case credApp:
		repos, complete, err := c.listRepos(ctx, op, a, c.endpoint("installation", "repositories"))
		if err != nil {
			return false, "", err
		}
		for _, r := range repos {
			owner, _, ok := splitRepoPath(r.FullName)
			if !ok || !strings.EqualFold(owner, user) {
				continue
			}
			if err := collect(r); err != nil {
				return false, "", err
			}
		}
		return complete, "", nil
	case credToken:
		self, err := c.selfAccount(ctx)
		switch {
		case err == nil && strings.EqualFold(self.Login, user):
			complete, err := listAll(ctx, c, op, c.endpoint("user", "repos"), url.Values{"affiliation": {"owner"}}, a, maxRepoPages, collect)
			return complete, "", err
		case err != nil && fatal(err):
			return false, "", err
		}
		who := "the token's account"
		if err == nil {
			who = self.Login
		}
		_, err = listAll(ctx, c, op, c.endpoint("users", user, "repos"), url.Values{"type": {"owner"}}, a, maxRepoPages, collect)
		return false, fmt.Sprintf("GET /users/%s/repos lists public repositories only, and the token acts as %s, not as %s: "+
			"private repositories of %s are missing (name them with repo: entries, or use a GitHub App installed on %s)", user, who, user, user, user), err
	}
	complete, err = listAll(ctx, c, op, c.endpoint("users", user, "repos"), url.Values{"type": {"owner"}}, a, maxRepoPages, collect)
	return complete, "", err
}

// allFacts reads the facts of repos, lookups at a time.
func (c *client) allFacts(ctx context.Context, op string, a *httpx.Auth, repos []*apiRepo) ([]facts, []error) {
	out := make([]facts, len(repos))
	errs := make([]error, len(repos))
	sem := make(chan struct{}, lookups)
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i], errs[i] = c.facts(ctx, op, a, r)
		}()
	}
	wg.Wait()
	return out, errs
}

// hasTopics reports whether have holds every topic of want, ignoring case.
func hasTopics(have, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}

// sortRepos orders repositories by path ignoring case, then path, then id.
func sortRepos(repos []platform.Repo) {
	slices.SortFunc(repos, func(a, b platform.Repo) int {
		if c := strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// Remote is the repository's git URL under the provider's web URL, never a
// host the API names, with the credential's Basic header ("x-access-token"
// and the token; for an App the reading token of the owner's installation,
// renewed when due); no header when anonymous.
func (d *reader) Remote(_ context.Context, r platform.Repo) (platform.Remote, error) {
	const op = "get remote"
	u, err := d.c.remoteURL(op, r)
	if err != nil {
		return platform.Remote{}, err
	}
	rem := platform.Remote{URL: u}
	if d.c.kind != credAnonymous {
		owner, _, _ := splitRepoPath(r.Path)
		rem.Header = func(ctx context.Context) (string, error) { return d.c.gitHeader(ctx, owner) }
	}
	return rem, nil
}

// remoteURL returns <web>/<owner>/<name>.git for r.
func (c *client) remoteURL(op string, r platform.Repo) (string, error) {
	if err := c.checkHost(op, r); err != nil {
		return "", err
	}
	owner, name, err := repoPath(op, r)
	if err != nil {
		return "", err
	}
	return c.web + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git", nil
}
