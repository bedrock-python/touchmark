package bitbucketdc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of listings.
const (
	// maxRepoPages bounds a project's or a label's repositories: 20 000 at
	// 1 000 a page. A larger listing resolves incompletely.
	maxRepoPages = 20
	// maxBranchPages bounds the branches read looking for one by name.
	maxBranchPages = 5
)

// apiRepo is a repository as the API reports it (RestRepository, and
// RestLabelable, its form in a label's listing).
type apiRepo struct {
	ID       int64       `json:"id"`
	Slug     string      `json:"slug"`
	Name     string      `json:"name"`
	State    string      `json:"state"`
	Public   bool        `json:"public"`
	Archived bool        `json:"archived"`
	Project  *apiProject `json:"project"`
	Origin   *apiRepoRef `json:"origin"`
	// LabelableType is REPOSITORY in a label's listing, absent elsewhere.
	LabelableType string `json:"labelableType"`
}

// apiProject is a repository's project: its key, and type NORMAL or
// PERSONAL (a user's own, whose key is ~<slug>).
type apiProject struct {
	Key    string `json:"key"`
	Public bool   `json:"public"`
	Type   string `json:"type"`
}

// apiRepoRef is the short repository of a fork's origin or of a pull
// request's ref.
type apiRepoRef struct {
	ID      int64       `json:"id"`
	Slug    string      `json:"slug"`
	Project *apiProject `json:"project"`
}

// path returns the key/slug of a repository reference, "" when it lacks
// either.
func (r *apiRepoRef) path() string {
	if r == nil || r.Project == nil || r.Project.Key == "" || r.Slug == "" {
		return ""
	}
	return r.Project.Key + "/" + r.Slug
}

// apiBranch is a branch (RestBranch): its full ref id, its name, its head
// commit and whether it is the default branch.
type apiBranch struct {
	ID           string `json:"id"`
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
	IsDefault    bool   `json:"isDefault"`
}

// check reports what the driver depends on that a repository lacks.
func (r *apiRepo) check(op string) error {
	switch {
	case r.ID <= 0 || r.Slug == "":
		return shapeError(op, "a repository without id or slug")
	case r.Project == nil || r.Project.Key == "":
		return shapeError(op, "repository %s has no project key", r.Slug)
	}
	return nil
}

// path returns the repository's <project key>/<slug>.
func (r *apiRepo) path() string { return r.Project.Key + "/" + r.Slug }

// toRepo converts an API repository; the default branch and whether the
// repository is empty come from defaultBranch: one whose default branch
// does not exist is skipped as empty, though other branches may. A
// repository that is not AVAILABLE (still initialising, failed or offline)
// is disabled; an archived one is read-only. The web URL is under the
// provider's url.
func (c *client) toRepo(r *apiRepo, def defaultBranch) platform.Repo {
	visibility := "private"
	if r.Public || r.Project.Public {
		visibility = "public"
	}
	return platform.Repo{
		Host:          c.host,
		ID:            strconv.FormatInt(r.ID, 10),
		Path:          r.path(),
		DefaultBranch: def.name,
		WebURL:        c.repoWeb(r.Project.Key, r.Slug),
		Visibility:    visibility,
		ObjectFormat:  "sha1",
		Archived:      r.Archived,
		Disabled:      r.State != "" && r.State != "AVAILABLE",
		Empty:         def.empty || def.missing,
		Fork:          r.Origin != nil,
	}
}

// repoWeb is the web page of repository key/slug.
func (c *client) repoWeb(key, slug string) string {
	return c.web + "/projects/" + url.PathEscape(key) + "/repos/" + url.PathEscape(slug)
}

// defaultBranch is what the driver knows of a repository's default branch:
// its name ("" when Bitbucket names none), its head commit, whether the
// repository has no branch at all (empty), and whether the default branch
// does not exist while other branches do (missing; head is "").
type defaultBranch struct {
	name, head     string
	empty, missing bool
}

// defaultBranchOf reads the default branch of key/slug: GET
// …/branches/default (deprecated, still in the reference), which answers
// the branch with its head, 204 for an empty repository, and 404 when the
// repository or its configured default branch does not exist; then GET
// …/default-branch for the configured name, whose 404 says the same. When
// both answer 404, the repository itself is read: gone, it is
// ClassNotFound; there, its default branch is missing.
func (c *client) defaultBranchOf(ctx context.Context, op, key, slug string) (defaultBranch, error) {
	var b apiBranch
	resp, err := c.get(ctx, op, c.repoURL(key, slug, "branches", "default"), nil, &b)
	switch {
	case err == nil && resp.Status == http.StatusNoContent:
		def, derr := c.configuredDefault(ctx, op, key, slug)
		if derr != nil && platform.ClassOf(derr) == platform.ClassNotFound {
			derr = nil
		}
		def.empty = true
		return def, derr
	case err == nil:
		name := strings.TrimPrefix(b.ID, "refs/heads/")
		if name == "" || name == b.ID || !isHexOID(b.LatestCommit) {
			return defaultBranch{}, shapeError(op, "the default branch of %s/%s: id %q, commit %q", key, slug, b.ID, b.LatestCommit)
		}
		return defaultBranch{name: name, head: strings.ToLower(b.LatestCommit)}, nil
	case platform.ClassOf(err) != platform.ClassNotFound:
		return defaultBranch{}, err
	}
	def, err := c.configuredDefault(ctx, op, key, slug)
	if err != nil && platform.ClassOf(err) == platform.ClassNotFound {
		if _, rerr := c.getRepo(ctx, op, key, slug); rerr != nil {
			return defaultBranch{}, rerr
		}
		err = nil
	}
	def.missing = true
	return def, err
}

// configuredDefault reads the configured default branch's name: GET
// …/default-branch.
func (c *client) configuredDefault(ctx context.Context, op, key, slug string) (defaultBranch, error) {
	var ref struct {
		ID string `json:"id"`
	}
	if _, err := c.get(ctx, op, c.repoURL(key, slug, "default-branch"), nil, &ref); err != nil {
		return defaultBranch{}, err
	}
	name := strings.TrimPrefix(ref.ID, "refs/heads/")
	if name == ref.ID {
		return defaultBranch{}, shapeError(op, "the default branch of %s/%s: id %q", key, slug, ref.ID)
	}
	return defaultBranch{name: name}, nil
}

// branch returns the branch name of key/slug, with ok false when it does
// not exist: GET …/branches?filterText=, which matches parts of names, with
// exact and prefix matches first (boostMatches); the exact ref is kept.
func (c *client) branch(ctx context.Context, op, key, slug, name string) (apiBranch, bool, error) {
	var found *apiBranch
	q := url.Values{"filterText": {name}, "boostMatches": {"true"}, "details": {"false"}}
	complete, err := listAll(ctx, c, op, c.repoURL(key, slug, "branches"), q, pageLimit, maxBranchPages, func(b apiBranch) error {
		if found == nil && b.ID == "refs/heads/"+name {
			found = &b
		}
		return nil
	})
	switch {
	case err != nil:
		return apiBranch{}, false, err
	case found != nil:
		if !isHexOID(found.LatestCommit) {
			return apiBranch{}, false, shapeError(op, "branch %s has no head commit", name)
		}
		return *found, true, nil
	case !complete:
		return apiBranch{}, false, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: errors.New("more branches match " + name + " than touchmark reads, and the branch is not among them")}
	}
	return apiBranch{}, false, nil
}

// Repo returns one repository by its <project key>/<slug> path, with its
// default branch. The API takes keys and slugs ignoring case; the result
// carries the canonical path.
func (d *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	const op = "get repository"
	key, slug, ok := splitRepoPath(path)
	if !ok {
		return platform.Repo{}, notFound(op, "repository %q: Bitbucket Data Center paths are <project key>/<repository>", path)
	}
	r, err := d.c.getRepo(ctx, op, key, slug)
	if err != nil {
		return platform.Repo{}, err
	}
	def, err := d.c.defaultBranchOf(ctx, op, r.Project.Key, r.Slug)
	if err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(r, def), nil
}

// getRepo reads GET /projects/{key}/repos/{slug}.
func (c *client) getRepo(ctx context.Context, op, key, slug string) (*apiRepo, error) {
	var r apiRepo
	if _, err := c.get(ctx, op, c.repoURL(key, slug), nil, &r); err != nil {
		return nil, err
	}
	if err := r.check(op); err != nil {
		return nil, err
	}
	return &r, nil
}

// Resolve lists the repositories sel selects, sorted by path ignoring case.
// A namespace is a project, by its key: GET /repos?projectkey=<key>
// &archived=ALL, every repository of it the identity reads, archived ones
// included (GET /projects/{key}/repos would need read access to the
// project itself). Projects do not nest, so Subgroups changes nothing.
// Topics are repository labels, every one of them on a repository: GET
// /labels/{name}/labeled?type=REPOSITORY lists a label's repositories the
// identity sees, in every project; the first label's are kept in the
// project, the others' intersect them. Forks only with sel.Forks. Each
// repository's default branch is read (one request each); archived and
// unavailable repositories are included for the core to skip.
//
// The listing is incomplete when it is capped or a later page fails; a
// failure of the first page, a rate limit, an auth error or a transient
// failure is returned instead, so the caller can retry or pause.
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
	case strings.ContainsAny(sel.Namespace, "/\x00?#") || sel.Namespace == "." || sel.Namespace == "..":
		return platform.Resolved{}, notFound(op, "project %q: Bitbucket Data Center has no nested projects", sel.Namespace)
	}
	inProject := func(r apiRepo) bool { return strings.EqualFold(r.Project.Key, sel.Namespace) }
	var (
		found    []apiRepo
		complete bool
		err      error
	)
	if len(sel.Topics) == 0 {
		q := url.Values{"projectkey": {sel.Namespace}, "archived": {"ALL"}}
		found, complete, err = d.c.repos(ctx, op, d.c.endpoint("repos"), q, inProject)
	} else {
		found, complete, err = d.c.labeled(ctx, op, sel.Topics[0], inProject)
		for _, label := range sel.Topics[1:] {
			if err != nil || len(found) == 0 {
				break
			}
			var (
				more []apiRepo
				c    bool
			)
			more, c, err = d.c.labeled(ctx, op, label, inProject)
			complete = complete && c
			ids := map[int64]bool{}
			for _, r := range more {
				ids[r.ID] = true
			}
			found = slices.DeleteFunc(found, func(r apiRepo) bool { return !ids[r.ID] })
		}
	}
	if err != nil {
		return platform.Resolved{}, err
	}
	if len(found) == 0 {
		// The listings name no missing project: a key that is wrong, or
		// that the project changed, lists nothing.
		if err := d.c.checkProject(ctx, op, sel.Namespace); err != nil {
			return platform.Resolved{}, err
		}
	}
	out := platform.Resolved{Repos: []platform.Repo{}, Complete: complete}
	seen := map[int64]bool{}
	for i := range found {
		r := &found[i]
		if seen[r.ID] || r.Origin != nil && !sel.Forks {
			continue
		}
		seen[r.ID] = true
		def, err := d.c.defaultBranchOf(ctx, op, r.Project.Key, r.Slug)
		switch {
		case err == nil:
		case platform.ClassOf(err) == platform.ClassNotFound:
			continue // gone since the listing
		case fatal(err):
			return platform.Resolved{}, err
		default:
			out.Complete = false
			continue
		}
		out.Repos = append(out.Repos, d.c.toRepo(r, def))
	}
	sortRepos(out.Repos)
	return out, nil
}

// checkProject tells whether project key exists as the identity sees it:
// GET /projects/{key}. A project Bitbucket does not find is ClassNotFound,
// and so is one that answers with another key (its key changed, and the
// old one leads to it); a project the identity may not read (401 with a
// user, 403) is no error: its repositories may still be readable.
func (c *client) checkProject(ctx context.Context, op, key string) error {
	var p apiProject
	_, err := c.get(ctx, op, c.endpoint("projects", key), nil, &p)
	switch {
	case err == nil && !strings.EqualFold(p.Key, key):
		return notFound(op, "project %s is %s now", key, p.Key)
	case err == nil, platform.ClassOf(err) == platform.ClassPermission:
		return nil
	}
	return err
}

// repos lists the repositories at u with query that keep accepts. complete
// is false when the listing was capped or a later page failed; a failure
// of the first page, or one that stops the call or is transient, is
// returned.
func (c *client) repos(ctx context.Context, op, u string, query url.Values, keep func(apiRepo) bool) ([]apiRepo, bool, error) {
	var out []apiRepo
	complete, err := listAll(ctx, c, op, u, query, pageLimit, maxRepoPages, func(r apiRepo) error {
		if err := r.check(op); err != nil {
			return err
		}
		if keep(r) {
			out = append(out, r)
		}
		return nil
	})
	switch {
	case err != nil && (!laterPage(err) || fatal(err)):
		return nil, false, err
	case err != nil:
		complete = false
	}
	return out, complete, nil
}

// labeled lists the repositories with label that keep accepts: GET
// /labels/{name}/labeled?type=REPOSITORY. A label no repository has is 404
// (none).
func (c *client) labeled(ctx context.Context, op, label string, keep func(apiRepo) bool) ([]apiRepo, bool, error) {
	if label == "" || strings.ContainsAny(label, "/\x00?#") {
		return nil, true, nil
	}
	q := url.Values{"type": {"REPOSITORY"}}
	found, complete, err := c.repos(ctx, op, c.endpoint("labels", label, "labeled"), q, func(r apiRepo) bool {
		return (r.LabelableType == "" || r.LabelableType == "REPOSITORY") && keep(r)
	})
	if err != nil && platform.ClassOf(err) == platform.ClassNotFound && !laterPage(err) {
		return nil, true, nil
	}
	return found, complete, err
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

// Remote is the repository's git URL under the provider's url
// (<url>/scm/<key>/<slug>.git), never a host the API names, with the
// credential's Bearer header (none when anonymous).
func (d *reader) Remote(_ context.Context, r platform.Repo) (platform.Remote, error) {
	const op = "get remote"
	if r.Host != "" && !strings.EqualFold(r.Host, d.c.host) {
		return platform.Remote{}, invalid(op, "repository %s is on %s, not %s", r.Path, r.Host, d.c.host)
	}
	key, slug, err := repoPath(op, r)
	if err != nil {
		return platform.Remote{}, err
	}
	rem := platform.Remote{URL: d.c.cloneURL(key, slug)}
	if d.c.token != "" {
		rem.Header = d.c.header
	}
	return rem, nil
}

// cloneURL is the HTTP clone URL of key/slug: <url>/scm/<key>/<slug>.git,
// the key lowercased as Bitbucket writes it.
func (c *client) cloneURL(key, slug string) string {
	return c.web + "/scm/" + url.PathEscape(strings.ToLower(key)) + "/" + url.PathEscape(slug) + ".git"
}
