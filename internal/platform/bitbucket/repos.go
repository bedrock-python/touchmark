package bitbucket

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxRepoPages bounds a workspace listing: 20 000 repositories at 100 a
// page. A larger one resolves incompletely.
const maxRepoPages = 200

// apiRepo is a repository as the API reports it.
type apiRepo struct {
	UUID       string      `json:"uuid"`
	FullName   string      `json:"full_name"`
	IsPrivate  bool        `json:"is_private"`
	MainBranch *apiBranch  `json:"mainbranch"`
	Parent     *apiRepoRef `json:"parent"`
	Links      apiLinks    `json:"links"`
}

// apiRepoRef is the short repository of a fork's parent or of a pull
// request's source or destination.
type apiRepoRef struct {
	UUID     string `json:"uuid"`
	FullName string `json:"full_name"`
}

// apiBranch is a branch: a repository's main branch, a pull request's
// source or destination branch, or GET …/refs/branches/{name}, where target
// is its head commit.
type apiBranch struct {
	Name   string     `json:"name"`
	Target *apiCommit `json:"target"`
}

// apiCommit is a commit; hash is full in the commit and branch APIs and
// short (12 digits) in pull requests.
type apiCommit struct {
	Hash string `json:"hash"`
}

// apiLinks are the links of a resource that the driver reads.
type apiLinks struct {
	HTML struct {
		Href string `json:"href"`
	} `json:"html"`
}

// check reports what the driver depends on that a repository lacks.
func (r *apiRepo) check(op string) error {
	if !uuidRe.MatchString(r.UUID) || r.FullName == "" {
		return shapeError(op, "a repository without uuid or full_name")
	}
	if _, _, ok := splitRepoPath(r.FullName); !ok {
		return shapeError(op, "repository full_name %q is not workspace/slug", r.FullName)
	}
	return nil
}

// toRepo converts an API repository. A repository without a main branch is
// taken for empty (nothing was pushed yet; to be confirmed live). Bitbucket
// archives nothing and always has pull requests; a fork has a parent.
func (c *client) toRepo(r *apiRepo) platform.Repo {
	visibility := "public"
	if r.IsPrivate {
		visibility = "private"
	}
	web := r.Links.HTML.Href
	if web == "" {
		web = c.web + "/" + r.FullName
	}
	out := platform.Repo{
		Host:         c.host,
		ID:           r.UUID,
		Path:         r.FullName,
		WebURL:       web,
		Visibility:   visibility,
		ObjectFormat: "sha1",
		Fork:         r.Parent != nil,
		Empty:        r.MainBranch == nil || r.MainBranch.Name == "",
	}
	if r.MainBranch != nil {
		out.DefaultBranch = r.MainBranch.Name
	}
	return out
}

// Repo returns one repository by its workspace/slug path. The API takes
// paths ignoring case (slugs are lowercase); the result carries the
// canonical full_name.
func (d *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	const op = "get repository"
	ws, slug, ok := splitRepoPath(path)
	if !ok {
		return platform.Repo{}, notFound(op, "repository %q: Bitbucket paths are workspace/repository", path)
	}
	r, err := d.c.getRepo(ctx, op, ws, slug)
	if err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(r), nil
}

// getRepo reads GET /2.0/repositories/{workspace}/{slug}; slug may be the
// repository's uuid in braces.
func (c *client) getRepo(ctx context.Context, op, ws, slug string) (*apiRepo, error) {
	var r apiRepo
	if _, err := c.get(ctx, op, c.endpoint("repositories", ws, slug), nil, &r); err != nil {
		return nil, err
	}
	if err := r.check(op); err != nil {
		return nil, err
	}
	return &r, nil
}

// Resolve lists the repositories sel selects, sorted by path ignoring case.
// A namespace is a workspace (GET /2.0/repositories/{workspace}, every
// repository the identity sees there); Bitbucket has no nested namespaces,
// so Subgroups changes nothing, and no topics, so a selector with topics is
// ClassInvalid (check refuses such an entry before). Forks only with
// sel.Forks.
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
	case len(sel.Topics) > 0:
		return platform.Resolved{}, &platform.Error{Op: op, Class: platform.ClassInvalid, Err: errTopics}
	case strings.ContainsAny(sel.Namespace, "/\x00?#") || sel.Namespace == "." || sel.Namespace == "..":
		return platform.Resolved{}, notFound(op, "workspace %q: Bitbucket has no nested namespaces", sel.Namespace)
	}
	var found []apiRepo
	q := url.Values{"pagelen": {strconv.Itoa(repoPageLen)}}
	complete, err := listAll(ctx, d.c, op, d.c.endpoint("repositories", sel.Namespace), q, maxRepoPages, func(r apiRepo) error {
		if err := r.check(op); err != nil {
			return err
		}
		found = append(found, r)
		return nil
	})
	if err != nil {
		if !laterPage(err) || fatal(err) {
			return platform.Resolved{}, err
		}
		complete = false
	}
	out := platform.Resolved{Repos: []platform.Repo{}, Complete: complete}
	seen := map[string]bool{}
	for i := range found {
		r := &found[i]
		if seen[r.UUID] || r.Parent != nil && !sel.Forks {
			continue
		}
		seen[r.UUID] = true
		out.Repos = append(out.Repos, d.c.toRepo(r))
	}
	sortRepos(out.Repos)
	return out, nil
}

// errTopics is why a selector with topics is refused.
var errTopics = errors.New("repositories on Bitbucket have no topics; select them with match (workspace/svc-*) or list them by repo")

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

// Remote is the repository's git URL under the provider's web URL
// (https://bitbucket.org/{full_name}.git), never a host the API names, with
// the credential's Basic header (none when anonymous).
func (d *reader) Remote(_ context.Context, r platform.Repo) (platform.Remote, error) {
	const op = "get remote"
	if r.Host != "" && !strings.EqualFold(r.Host, d.c.host) {
		return platform.Remote{}, invalid(op, "repository %s is on %s, not %s", r.Path, r.Host, d.c.host)
	}
	ws, slug, err := repoPath(op, r)
	if err != nil {
		return platform.Remote{}, err
	}
	rem := platform.Remote{URL: d.c.web + "/" + url.PathEscape(ws) + "/" + url.PathEscape(slug) + ".git"}
	if d.c.token != "" {
		rem.Header = d.c.gitHeader
	}
	return rem, nil
}
