package gitlab

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxProjectPages bounds a namespace listing: 20 000 projects of 100 per
// page. A larger one resolves incompletely.
const maxProjectPages = 200

// apiProject is a project as GET /projects/:id and the project listings
// report it (https://docs.gitlab.com/api/projects/#get-a-single-project).
// Fields of EE (mirror) or of newer versions (repository_object_format,
// marked_for_deletion_on) are simply absent elsewhere.
type apiProject struct {
	ID                int64           `json:"id"`
	PathWithNamespace string          `json:"path_with_namespace"`
	DefaultBranch     *string         `json:"default_branch"`
	WebURL            string          `json:"web_url"`
	HTTPURLToRepo     string          `json:"http_url_to_repo"`
	Visibility        string          `json:"visibility"`
	Archived          bool            `json:"archived"`
	EmptyRepo         bool            `json:"empty_repo"`
	Mirror            bool            `json:"mirror"`
	ForkedFrom        json.RawMessage `json:"forked_from_project"`
	Topics            []string        `json:"topics"`
	TagList           []string        `json:"tag_list"`
	// MergeRequestsEnabled is the older flag; MergeRequestsAccess
	// ("disabled", "private", "enabled") the newer one.
	MergeRequestsEnabled *bool  `json:"merge_requests_enabled"`
	MergeRequestsAccess  string `json:"merge_requests_access_level"`
	RepositoryAccess     string `json:"repository_access_level"`
	MarkedForDeletionAt  string `json:"marked_for_deletion_at"`
	MarkedForDeletionOn  string `json:"marked_for_deletion_on"`
	ObjectFormat         string `json:"repository_object_format"`
	Permissions          *struct {
		ProjectAccess *apiAccess `json:"project_access"`
		GroupAccess   *apiAccess `json:"group_access"`
	} `json:"permissions"`
}

// apiAccess is a member's access level: 10 Guest, 15 Planner, 20 Reporter,
// 30 Developer, 40 Maintainer, 50 Owner.
type apiAccess struct {
	AccessLevel int `json:"access_level"`
}

// Access levels the driver compares with.
const accessDeveloper = 30

// checkID reports a project without the id and path every use needs.
func (p *apiProject) checkID(op string) error {
	if p.ID <= 0 || !checkFullPath(p.PathWithNamespace, 2) {
		return shapeError(op, "a project without id or path_with_namespace")
	}
	return nil
}

// check reports what the driver depends on that a project lacks, when one
// project is at stake (Repo, Target, the projects of a sweep).
func (p *apiProject) check(op string) error {
	if err := p.checkID(op); err != nil {
		return err
	}
	if !p.EmptyRepo && (p.DefaultBranch == nil || *p.DefaultBranch == "") {
		// A project without a repository (its code feature off, or never
		// created) has no default branch either: it is Disabled, and the
		// core skips it.
		if p.RepositoryAccess != "disabled" {
			return shapeError(op, "project %s has no default_branch", p.PathWithNamespace)
		}
	}
	return nil
}

// fork reports whether the project is a fork. forked_from_project is shown
// only when the caller may read the upstream project (API::Entities::Project):
// a fork of a project the reader cannot see looks like any project.
func (p *apiProject) fork() bool {
	s := strings.TrimSpace(string(p.ForkedFrom))
	return s != "" && s != "null"
}

// topics returns the project's topics (tag_list before GitLab 14).
func (p *apiProject) topics() []string {
	if p.Topics != nil {
		return p.Topics
	}
	return p.TagList
}

// toRepo converts an API project of the provider's host.
func (c *client) toRepo(p *apiProject) platform.Repo {
	format := strings.ToLower(p.ObjectFormat)
	if format == "" {
		format = "sha1"
	}
	branch := ""
	if p.DefaultBranch != nil {
		branch = *p.DefaultBranch
	}
	visibility := p.Visibility
	switch visibility {
	case "public", "internal", "private":
	default:
		// Unknown or absent: the safe side for a public hub.
		visibility = "private"
	}
	prsOff := p.MergeRequestsAccess == "disabled" || p.MergeRequestsAccess == "" && p.MergeRequestsEnabled != nil && !*p.MergeRequestsEnabled
	topics := slices.Clone(p.topics())
	if topics == nil {
		topics = []string{}
	}
	return platform.Repo{
		Host:          c.host,
		ID:            strconv.FormatInt(p.ID, 10),
		Path:          p.PathWithNamespace,
		DefaultBranch: branch,
		WebURL:        p.WebURL,
		Visibility:    visibility,
		ObjectFormat:  format,
		Archived:      p.Archived,
		// A project whose code the caller may not read has no visible
		// default branch (BasicProjectDetails shows it to :read_code only):
		// for touchmark its repository is disabled, and the core skips it.
		Disabled:      p.RepositoryAccess == "disabled" || !p.EmptyRepo && branch == "",
		Empty:         p.EmptyRepo,
		Mirror:        p.Mirror,
		Fork:          p.fork(),
		PendingDelete: p.MarkedForDeletionAt != "" || p.MarkedForDeletionOn != "",
		PRsDisabled:   prsOff,
		Topics:        topics,
	}
}

// getProject reads project id (a numeric id or a full path).
func (c *client) getProject(ctx context.Context, op, id string) (*apiProject, error) {
	var p apiProject
	if _, err := c.get(ctx, op, c.projectURL(id), nil, &p); err != nil {
		return nil, err
	}
	if err := p.check(op); err != nil {
		return nil, err
	}
	return &p, nil
}

// Repo returns one project by path; paths compare case-insensitively, and
// the old path of a renamed or moved project still leads to it
// (find_by_full_path with follow_redirects, see the package doc), under its
// canonical path.
func (d *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	const op = "get repository"
	if !checkFullPath(path, 2) {
		return platform.Repo{}, notFound(op, "repository %q: GitLab paths are group/…/project", path)
	}
	p, err := d.c.getProject(ctx, op, path)
	if err != nil {
		return platform.Repo{}, err
	}
	return d.c.toRepo(p), nil
}

// Resolve lists the projects sel selects, sorted by path ignoring case.
//
// A namespace is a group, with its subgroups' projects when sel.Subgroups
// (GET /groups/:id/projects?include_subgroups=true), without projects
// shared into it from elsewhere (with_shared=false); or a user
// (GET /users/:username/projects). Archived projects are listed; forks
// only with sel.Forks. Topics compare ignoring case and must all be
// present; they are filtered here, not by the server, whose topic filter
// compares case-sensitively.
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
	case !checkFullPath(sel.Namespace, 1):
		return platform.Resolved{}, notFound(op, "namespace %q", sel.Namespace)
	}
	var found []apiProject
	// One project whose code the reader may not read (no default_branch:
	// a project of a subgroup the reader is not in, with its repository
	// for members only) must not fail the listing: it is kept, Disabled.
	collect := func(p apiProject) error {
		if err := p.checkID(op); err != nil {
			return err
		}
		found = append(found, p)
		return nil
	}
	q := url.Values{
		"with_shared": {"false"},
		"order_by":    {"id"},
		"sort":        {"asc"},
	}
	if sel.Subgroups {
		q.Set("include_subgroups", "true")
	}
	complete, err := listAll(ctx, d.c, op, d.c.endpoint("groups", sel.Namespace, "projects"), q, maxProjectPages, collect)
	if err != nil && platform.ClassOf(err) == platform.ClassNotFound && !laterPage(err) && !strings.Contains(sel.Namespace, "/") {
		// Not a group: a user's projects.
		found = nil
		uq := url.Values{"order_by": {"id"}, "sort": {"asc"}}
		complete, err = listAll(ctx, d.c, op, d.c.endpoint("users", sel.Namespace, "projects"), uq, maxProjectPages, collect)
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
		p := &found[i]
		if seen[p.ID] || p.fork() && !sel.Forks {
			continue
		}
		seen[p.ID] = true
		if !hasTopics(p.topics(), sel.Topics) {
			continue
		}
		out.Repos = append(out.Repos, d.c.toRepo(p))
	}
	sortRepos(out.Repos)
	return out, nil
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

// Remote is the project's git URL under the provider's web URL, never a
// host the API names (http_url_to_repo follows the instance's external_url,
// which may differ from the URL touchmark was given), with the credential's
// Basic header (none when anonymous).
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

// remoteURL returns <web>/<group>/…/<project>.git for r.
func (c *client) remoteURL(op string, r platform.Repo) (string, error) {
	if r.Host != "" && !strings.EqualFold(r.Host, c.host) {
		return "", invalid(op, "repository %s is on %s, not %s", r.Path, r.Host, c.host)
	}
	if !checkFullPath(r.Path, 2) {
		return "", invalid(op, "%q is not a group/…/project path", r.Path)
	}
	var b strings.Builder
	b.WriteString(c.web)
	for seg := range strings.SplitSeq(r.Path, "/") {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(seg))
	}
	b.WriteString(".git")
	return b.String(), nil
}

// projectID returns the id to address r by in the API: its numeric id,
// else its path.
func projectID(r platform.Repo) string {
	if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil && id > 0 {
		return r.ID
	}
	return r.Path
}
