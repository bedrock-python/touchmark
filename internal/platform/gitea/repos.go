package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Bounds of repository listings.
const (
	// maxRepoPages bounds a namespace listing: 20 000 repositories at the
	// default page size. A larger one resolves incompletely.
	maxRepoPages = 400
	// maxTopicPages bounds the topics of one repository.
	maxTopicPages = 10
)

// apiRepo is a repository as the API reports it.
type apiRepo struct {
	ID              int64       `json:"id"`
	FullName        string      `json:"full_name"`
	Owner           *apiOwner   `json:"owner"`
	DefaultBranch   string      `json:"default_branch"`
	HTMLURL         string      `json:"html_url"`
	Private         bool        `json:"private"`
	Internal        bool        `json:"internal"`
	Empty           bool        `json:"empty"`
	Archived        bool        `json:"archived"`
	Mirror          bool        `json:"mirror"`
	Fork            bool        `json:"fork"`
	HasPullRequests *bool       `json:"has_pull_requests"`
	HasCode         *bool       `json:"has_code"` // Gitea ≥ 1.26
	ObjectFormat    string      `json:"object_format_name"`
	Topics          optStrings  `json:"topics"`
	Permissions     *apiPermits `json:"permissions"`
}

// apiOwner is the owner of a repository: a user or an organization.
type apiOwner struct {
	Login      string `json:"login"`
	Visibility string `json:"visibility"` // public, limited or private
}

// apiPermits are the caller's rights on a repository: its role, not what
// its token's scopes allow.
type apiPermits struct {
	Admin bool `json:"admin"`
	Push  bool `json:"push"`
	Pull  bool `json:"pull"`
}

// optStrings is a list that remembers whether the JSON had it at all.
type optStrings struct {
	set  bool
	list []string
}

func (o *optStrings) UnmarshalJSON(b []byte) error {
	o.set = true
	return json.Unmarshal(b, &o.list)
}

// apiTopics is GET /repos/{owner}/{repo}/topics.
type apiTopics struct {
	Topics []string `json:"topics"`
}

// check reports what the driver depends on that a repository lacks.
func (r *apiRepo) check(op string) error {
	if r.ID <= 0 || r.FullName == "" || r.DefaultBranch == "" && !r.Empty {
		return shapeError(op, "a repository without id, full_name or default_branch")
	}
	if _, _, ok := splitRepoPath(r.FullName); !ok {
		return shapeError(op, "repository full_name %q is not owner/name", r.FullName)
	}
	return nil
}

// toRepo converts an API repository of the provider's host, on an instance
// that requires signing in to see anything when signIn is set.
//
// Visibility is what someone not signed in sees: "public" only for a
// repository that is not private, not internal, of an owner whose
// visibility is public, on an instance that shows it without signing in.
// Otherwise it is "internal" (visible to signed-in users or members) or
// "private". The API reports a repository of an instance with
// REQUIRE_SIGNIN_VIEW as private false, internal false and its owner's
// visibility public, while anyone not signed in gets 403 for it (checked on
// Gitea 1.26 and 1.27 and Forgejo 15 and 16): a public hub must not name it
// in its logs.
func (c *client) toRepo(r *apiRepo, signIn bool) platform.Repo {
	visibility := "public"
	switch {
	case r.Private:
		visibility = "private"
	case r.Internal || signIn || r.Owner != nil && (r.Owner.Visibility == "limited" || r.Owner.Visibility == "private"):
		// Not private, but visible only to signed-in users or members.
		visibility = "internal"
	}
	format := strings.ToLower(r.ObjectFormat)
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
		// A repository whose code unit is off serves no code.
		Disabled:    r.HasCode != nil && !*r.HasCode,
		Empty:       r.Empty,
		Mirror:      r.Mirror,
		Fork:        r.Fork,
		PRsDisabled: r.HasPullRequests != nil && !*r.HasPullRequests,
		Topics:      slices.Clone(r.Topics.list),
	}
}

// Repo returns one repository by path; paths compare case-insensitively,
// and the old path of a renamed repository, or of one whose owner was
// renamed, still leads to it (the server redirects, call follows), under
// its canonical path.
func (d *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	const op = "get repository"
	owner, name, ok := splitRepoPath(path)
	if !ok {
		return platform.Repo{}, notFound(op, "repository %q: Gitea and Forgejo paths are owner/name", path)
	}
	inst, err := d.c.instance(ctx)
	if err != nil {
		return platform.Repo{}, err
	}
	var r apiRepo
	if _, err := d.c.get(ctx, op, d.c.endpoint("repos", owner, name), nil, &r); err != nil {
		return platform.Repo{}, err
	}
	if err := r.check(op); err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(&r, inst.signIn), nil
}

// Resolve lists the repositories sel selects, sorted by path ignoring case.
// A namespace is an organization or a user (Gitea and Forgejo have no
// nested namespaces, so Subgroups changes nothing); the old name of a
// renamed one still leads to it, its repositories under their canonical
// paths. Topics compare ignoring case and must all be present; forks only
// with sel.Forks.
//
// The listing is incomplete when it is capped or a later page fails; a
// failure of the first page, a rate limit, an auth error or a transient
// failure is returned instead, so the caller can retry or pause. So are
// the topics of a repository whose listing lacks them: a repository whose
// topics cannot be read is left out, and the result is incomplete.
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
		return platform.Resolved{}, notFound(op, "namespace %q: Gitea and Forgejo have no nested namespaces", sel.Namespace)
	}
	inst, err := d.c.instance(ctx)
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
	complete, err := listAll(ctx, d.c, op, d.c.endpoint("orgs", sel.Namespace, "repos"), nil,
		listOpts{maxPages: maxRepoPages, trustTotal: true}, collect)
	if err != nil && platform.ClassOf(err) == platform.ClassNotFound && !laterPage(err) {
		// Not an organization: a user's repositories.
		found = nil
		complete, err = listAll(ctx, d.c, op, d.c.endpoint("users", sel.Namespace, "repos"), nil,
			listOpts{maxPages: maxRepoPages, trustTotal: true}, collect)
	}
	if err != nil {
		if !laterPage(err) || fatal(err) {
			return platform.Resolved{}, err
		}
		complete = false
	}
	out := platform.Resolved{Repos: []platform.Repo{}, Complete: complete}
	seen := map[int64]bool{}
	for i := range found {
		r := &found[i]
		if seen[r.ID] || r.Fork && !sel.Forks {
			continue
		}
		seen[r.ID] = true
		if len(sel.Topics) > 0 && !r.Topics.set {
			topics, err := d.topics(ctx, r.FullName)
			if err != nil {
				if fatal(err) {
					return platform.Resolved{}, err
				}
				out.Complete = false
				continue
			}
			r.Topics = optStrings{set: true, list: topics}
		}
		if !hasTopics(r.Topics.list, sel.Topics) {
			continue
		}
		out.Repos = append(out.Repos, d.c.toRepo(r, inst.signIn))
	}
	sortRepos(out.Repos)
	return out, nil
}

// topics returns the topics of a repository whose listing lacked them.
func (d *reader) topics(ctx context.Context, fullName string) ([]string, error) {
	owner, name, _ := splitRepoPath(fullName)
	var out []string
	q := url.Values{}
	u := d.c.endpoint("repos", owner, name, "topics")
	size, _ := d.c.pages(ctx)
	q.Set("limit", strconv.Itoa(size))
	for page := 1; page <= maxTopicPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var t apiTopics
		if _, err := d.c.get(ctx, "read topics", u, q, &t); err != nil {
			return nil, err
		}
		out = append(out, t.Topics...)
		if len(t.Topics) < size {
			return out, nil
		}
	}
	return out, nil
}

// stops reports whether err ends a call at once, whatever it already
// found: the context ended, the platform limits the rate or refuses the
// credential.
func stops(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	c := platform.ClassOf(err)
	return c == platform.ClassRateLimited || c == platform.ClassAuth
}

// fatal reports whether err must fail a listing instead of making it
// incomplete: it stops the call, or the platform failed for now (the caller
// retries the whole listing).
func fatal(err error) bool { return stops(err) || platform.ClassOf(err) == platform.ClassTransient }

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
// host the API names, with the credential's Basic header (none when
// anonymous).
func (d *reader) Remote(_ context.Context, r platform.Repo) (platform.Remote, error) {
	u, err := d.c.remoteURL("get remote", r)
	if err != nil {
		return platform.Remote{}, err
	}
	rem := platform.Remote{URL: u}
	if d.c.token != "" {
		rem.Header = d.c.gitHeader
	}
	return rem, nil
}

// remoteURL returns <web>/<owner>/<name>.git for r.
func (c *client) remoteURL(op string, r platform.Repo) (string, error) {
	if r.Host != "" && !strings.EqualFold(r.Host, c.host) {
		return "", invalid(op, "repository %s is on %s, not %s", r.Path, r.Host, c.host)
	}
	owner, name, err := repoPath(op, r)
	if err != nil {
		return "", err
	}
	return c.web + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git", nil
}
