package azuredevops

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// apiRepo is a repository as the API reports it (GitRepository).
type apiRepo struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Project       *apiProject `json:"project"`
	DefaultBranch string      `json:"defaultBranch"`
	WebURL        string      `json:"webUrl"`
	IsDisabled    bool        `json:"isDisabled"`
	IsFork        bool        `json:"isFork"`
}

// apiProject is a repository's project (TeamProjectReference).
type apiProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// check reports what the driver depends on that a repository lacks.
func (r *apiRepo) check(op string) error {
	switch {
	case !guidRe.MatchString(r.ID) || r.Name == "":
		return shapeError(op, "a repository without id or name")
	case r.Project == nil || !guidRe.MatchString(r.Project.ID) || r.Project.Name == "":
		return shapeError(op, "repository %s has no project", r.ID)
	case strings.Contains(r.Project.Name, "/") || strings.Contains(r.Name, "/"):
		return shapeError(op, "repository %s/%s: a name with a slash", r.Project.Name, r.Name)
	}
	return nil
}

// path is the repository's <project>/<repository>.
func (r *apiRepo) path() string { return r.Project.Name + "/" + r.Name }

// toRepo converts an API repository. Its visibility is its project's
// (public or private). A repository without a default branch is empty
// (nothing was pushed yet); a disabled one is reported as such (it shows
// no default branch either, seen anonymously), and the core skips it.
// Azure Repos has no archiving and always takes pull requests.
func (c *client) toRepo(r *apiRepo) platform.Repo {
	visibility := "private"
	if strings.EqualFold(r.Project.Visibility, "public") {
		visibility = "public"
	}
	web := r.WebURL
	if web == "" {
		web = c.web + "/" + url.PathEscape(r.Project.Name) + "/_git/" + url.PathEscape(r.Name)
	}
	return platform.Repo{
		Host:          c.host,
		ID:            strings.ToLower(r.ID),
		Path:          r.path(),
		DefaultBranch: strings.TrimPrefix(r.DefaultBranch, "refs/heads/"),
		WebURL:        web,
		Visibility:    visibility,
		ObjectFormat:  "sha1",
		Disabled:      r.IsDisabled,
		Fork:          r.IsFork,
		Empty:         r.DefaultBranch == "",
	}
}

// Repo returns one repository by its <project>/<repository> path: GET
// {org}/{project}/_apis/git/repositories/{name}. The API takes names
// ignoring case; the result carries the canonical path. A missing project
// or repository is ErrNotFound.
func (d *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	const op = "get repository"
	project, name, ok := splitRepoPath(path)
	if !ok {
		return platform.Repo{}, notFound(op, "repository %q: Azure DevOps paths are project/repository", path)
	}
	var r apiRepo
	if _, err := d.c.get(ctx, op, endpoint(d.c.api, project, "_apis", "git", "repositories", name), nil, &r); err != nil {
		return platform.Repo{}, err
	}
	if err := r.check(op); err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(&r), nil
}

// getRepo reads a repository by its id: GET
// {org}/_apis/git/repositories/{id}.
func (c *client) getRepo(ctx context.Context, op, id string) (*apiRepo, error) {
	var r apiRepo
	if _, err := c.get(ctx, op, c.repoAPI(id), nil, &r); err != nil {
		return nil, err
	}
	if err := r.check(op); err != nil {
		return nil, err
	}
	if !strings.EqualFold(r.ID, id) {
		return nil, shapeError(op, "repository %s answers as %s", id, r.ID)
	}
	return &r, nil
}

// listRepos reads every repository of the organization the identity sees,
// across its projects: GET {org}/_apis/git/repositories, which the API
// answers in one collection.
func (c *client) listRepos(ctx context.Context, op string) ([]apiRepo, error) {
	var all list[apiRepo]
	if _, err := c.get(ctx, op, c.apis("git", "repositories"), nil, &all); err != nil {
		return nil, err
	}
	for i := range all.Value {
		if err := all.Value[i].check(op); err != nil {
			return nil, err
		}
	}
	return all.Value, nil
}

// Resolve lists the repositories sel selects, sorted by path ignoring case.
// A namespace is the organization itself (its name, as the provider's url
// gives it, ignoring case): an org entry selects every repository of every
// project the identity sees, and match narrows it to projects
// (Billing/*). Azure DevOps has no topics, so a selector with topics is
// ClassInvalid (check refuses such an entry before), and no nested
// namespaces, so Subgroups changes nothing. Forks only with sel.Forks.
// Disabled repositories are included, for the core to skip and report.
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
	case !strings.EqualFold(sel.Namespace, d.c.org):
		return platform.Resolved{}, notFound(op, "namespace %q: an Azure DevOps provider is the organization %s, and an org entry names it "+
			"(select a project's repositories with match: <project>/*)", sel.Namespace, d.c.org)
	}
	found, err := d.c.listRepos(ctx, op)
	if err != nil {
		return platform.Resolved{}, err
	}
	out := platform.Resolved{Repos: []platform.Repo{}, Complete: true}
	seen := map[string]bool{}
	for i := range found {
		r := &found[i]
		id := strings.ToLower(r.ID)
		if seen[id] || r.IsFork && !sel.Forks {
			continue
		}
		seen[id] = true
		out.Repos = append(out.Repos, d.c.toRepo(r))
	}
	sortRepos(out.Repos)
	return out, nil
}

// errTopics is why a selector with topics is refused.
var errTopics = errors.New("repositories on Azure DevOps have no topics; select them with match (<project>/svc-*) or list them by repo")

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

// Remote is the repository's git URL under the provider's url,
// https://dev.azure.com/{org}/{project}/_git/{repository}, never a URL the
// API names, with the credential's Basic header (none when anonymous).
func (d *reader) Remote(_ context.Context, r platform.Repo) (platform.Remote, error) {
	const op = "get remote"
	if r.Host != "" && !strings.EqualFold(r.Host, d.c.host) {
		return platform.Remote{}, invalid(op, "repository %s is on %s, not %s", r.Path, r.Host, d.c.host)
	}
	project, name, ok := splitRepoPath(r.Path)
	if !ok {
		return platform.Remote{}, invalid(op, "%q is not a project/repository path", r.Path)
	}
	rem := platform.Remote{URL: d.c.web + "/" + url.PathEscape(project) + "/_git/" + url.PathEscape(name)}
	if d.c.token != "" {
		rem.Header = d.c.gitHeader
	}
	return rem, nil
}
