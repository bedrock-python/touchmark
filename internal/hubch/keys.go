package hubch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
)

// Where the hub keeps its secrets, as a maintainer's token of the hub reads
// it: `touchmark doctor --hub-token`. The token goes to the
// hub's API host only, in a header; the values of secrets and variables are
// never kept (the APIs of GitHub and Gitea never return them; GitLab's and
// Bitbucket's variables carry those not masked or secured, and the decoding
// drops them).

// KeyStoreInput locates the hub for ReadKeyStore.
type KeyStoreInput struct {
	// Platform is "github", "gitlab", "gitea", "forgejo" or "bitbucket".
	Platform string
	// APIURL is the REST base of the hub's platform (…/api/v3, /api/v4,
	// /api/v1, https://api.github.com).
	APIURL string
	// RepoID is the hub repository's immutable id (the fingerprint's): a
	// number, or on Bitbucket the repository's UUID (with or without
	// braces).
	RepoID string
	// Token is the maintainer's token.
	Token string
	// Client sends the requests; the token goes to the host of APIURL only.
	Client *httpx.Client
}

// KeyStore is what ReadKeyStore found.
type KeyStore struct {
	Platform      string
	RepoPath      string
	DefaultBranch string
	Visibility    string
	// Secrets are the secrets and variables the hub's jobs may get, by
	// name, where they live and how they are kept. Values are never read.
	Secrets []Secret
	// Environments are the hub's deployment environments on GitHub, with
	// who may use them, and on Bitbucket (whose API does not show which
	// branches may deploy); EnvironmentsUnread says why they could not be
	// listed ("" when they were).
	Environments       []NamedEnvironment
	EnvironmentsUnread string
	// ProtectedBranches and ProtectedTags are GitLab's: the refs whose
	// pipelines get protected variables; ProtectedBranchesUnread and
	// ProtectedTagsUnread say why they could not be listed ("" when they
	// were), so that their checks are unknown, not ok.
	ProtectedBranches       []ProtectedRef
	ProtectedTags           []ProtectedRef
	ProtectedBranchesUnread string
	ProtectedTagsUnread     string
	// PipelineVariables is GitLab's minimum role to run a pipeline with
	// variables (ci_pipeline_variables_minimum_override_role); "" when
	// not shown.
	PipelineVariables string
	// Unread lists what could not be read, and why: the checks that need it
	// are unknown.
	Unread []string
}

// Secret is one secret (GitHub, Gitea, Forgejo), CI/CD variable (GitLab)
// or Pipelines variable (Bitbucket).
type Secret struct {
	Name string
	// Where is "repository", "organization" (shared with the hub),
	// "dependabot", "environment" (Environment names it; a deployment
	// variable on Bitbucket), "project" or "group" (Group names it, GitLab),
	// "workspace" (Bitbucket).
	Where       string
	Environment string
	Group       string
	// Protected, Masked and Scope are GitLab's: a protected variable
	// reaches pipelines of protected refs only; Scope is its environment
	// scope ("*" for every job). On Bitbucket Masked is a secured
	// variable: hidden in the logs and the API.
	Protected bool
	Masked    bool
	Scope     string
}

// NamedEnvironment is a deployment environment of the hub on GitHub.
type NamedEnvironment struct {
	Name string
	Environment
	// Err is why its policy could not be read.
	Err error
}

// ProtectedRef is a protected branch or tag of GitLab: the roles that may
// push or merge to it (branches) or create it (tags) — 0 No one, 30
// Developers, 40 Maintainers, 60 administrators — and whether a user or a
// group may besides (Others).
type ProtectedRef struct {
	Name   string
	Levels []int
	Others bool
}

// Bounds of the listings ReadKeyStore reads.
const (
	keyPages    = 10
	keyPageSize = 100
)

// ReadKeyStore reads where the hub keeps its secrets through the platform's
// API with a maintainer's token:
//   - GitHub: the repository by id; the names of its Actions secrets, the
//     organization's secrets shared with it, its Dependabot secrets, and
//     each environment's secrets and deployment branch policies;
//   - GitLab: the project by id (its minimum role for pipeline variables);
//     its CI/CD variables and those of each group above it (protected,
//     masked, environment scope); its protected branches and tags;
//   - Gitea and Forgejo: the repository by id, the names of its Actions
//     secrets and of its organization's;
//   - Bitbucket: the repository by UUID; its Pipelines variables, its
//     workspace's, and each deployment environment's (secured or not).
//
// A listing the token may not read is noted in Unread, or in the field of
// its own (EnvironmentsUnread, ProtectedBranchesUnread,
// ProtectedTagsUnread); a refused token or a hub that does not exist is an
// error.
func ReadKeyStore(ctx context.Context, in KeyStoreInput) (KeyStore, error) {
	if in.Client == nil {
		in.Client = httpx.New(httpx.Options{})
	}
	switch {
	case in.Platform == "bitbucket":
		if in.RepoID = BitbucketRepoID(in.RepoID); in.RepoID == "" {
			return KeyStore{}, errors.New("hub keys: the hub's repository UUID is unknown")
		}
	case repoID(in.RepoID) == "":
		return KeyStore{}, errors.New("hub keys: the hub's repository id is unknown")
	}
	if err := checkToken(in.Token); err != nil {
		return KeyStore{}, err
	}
	api, err := url.Parse(strings.TrimRight(in.APIURL, "/"))
	if err != nil || api.Host == "" || api.User != nil || (api.Scheme != "https" && api.Scheme != "http") {
		return KeyStore{}, errors.New("hub keys: the API URL is not an absolute http(s) URL without credentials")
	}
	if api.Scheme == "http" && !isLoopback(strings.ToLower(api.Hostname())) {
		return KeyStore{}, errors.New("hub keys: the maintainer's token is sent over https only")
	}
	k := &keyReader{in: in, base: api.String()}
	switch in.Platform {
	case "github":
		k.auth = &httpx.Auth{Hosts: []string{api.Host}, Header: k.header("Bearer ")}
		return k.github(ctx)
	case "gitlab":
		k.auth = &httpx.Auth{Hosts: []string{api.Host}, Name: "Private-Token", Header: k.header("")}
		return k.gitlab(ctx)
	case "gitea", "forgejo":
		k.auth = &httpx.Auth{Hosts: []string{api.Host}, Header: k.header("token ")}
		return k.gitea(ctx)
	case "bitbucket":
		k.auth = &httpx.Auth{Hosts: []string{api.Host}, Header: k.header("Bearer ")}
		return k.bitbucket(ctx)
	}
	return KeyStore{}, fmt.Errorf("hub keys: unknown platform %q", in.Platform)
}

// keyReader reads one hub's key store.
type keyReader struct {
	in   KeyStoreInput
	base string
	auth *httpx.Auth
	ks   KeyStore
}

// header returns the credential header of scheme and the token.
func (k *keyReader) header(scheme string) func(context.Context) (string, error) {
	value := scheme + k.in.Token
	return func(context.Context) (string, error) { return value, nil }
}

// get reads u into out.
func (k *keyReader) get(ctx context.Context, u string, out any) error {
	var h http.Header
	if k.in.Platform == "github" {
		h = http.Header{"Accept": {"application/vnd.github+json"}, "X-Github-Api-Version": {"2022-11-28"}}
	}
	_, err := k.in.Client.JSONWith(ctx, http.MethodGet, u, k.auth, h, nil, out)
	return err
}

// status returns the HTTP status of a failed request, 0 for none.
func status(err error) int {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// fatal reports whether err ends ReadKeyStore: the token is refused, or the
// request could not be made at all.
func fatal(err error) bool {
	s := status(err)
	return s == 0 || s == http.StatusUnauthorized || s >= 500
}

// unread notes that what could not be read, unless err ends the reading.
func (k *keyReader) unread(what string, err error) error {
	why, err := unreadable(what, err)
	if err != nil {
		return err
	}
	k.ks.Unread = append(k.ks.Unread, why)
	return nil
}

// unreadable returns why what could not be read ("<what>: HTTP <status>"),
// or the error that ends the reading.
func unreadable(what string, err error) (string, error) {
	if fatal(err) {
		return "", fmt.Errorf("hub keys: read %s: %w", what, err)
	}
	return fmt.Sprintf("%s: HTTP %d", what, status(err)), nil
}

// segs joins escaped path segments under the API base.
func (k *keyReader) segs(parts ...string) string {
	var b strings.Builder
	b.WriteString(k.base)
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(p))
	}
	return b.String()
}

// github reads the key store of a hub on GitHub.
func (k *keyReader) github(ctx context.Context) (KeyStore, error) {
	var repo struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Visibility    string `json:"visibility"`
		Private       bool   `json:"private"`
		Owner         struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if err := k.get(ctx, k.segs("repositories", k.in.RepoID), &repo); err != nil {
		return KeyStore{}, fmt.Errorf("hub keys: read the hub repository %s: %w", k.in.RepoID, err)
	}
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || !validName(owner) || !validName(name) {
		return KeyStore{}, fmt.Errorf("hub keys: the hub repository's name %q is not owner/name", repo.FullName)
	}
	k.ks = KeyStore{Platform: "github", RepoPath: repo.FullName, DefaultBranch: repo.DefaultBranch, Visibility: visibility(repo.Visibility)}
	if k.ks.Visibility == "" && repo.Private {
		k.ks.Visibility = "private"
	}
	lists := []struct {
		what, where string
		u           string
	}{
		{"the repository's Actions secrets", "repository", k.segs("repos", owner, name, "actions", "secrets")},
		{"the organization secrets shared with the repository", "organization", k.segs("repos", owner, name, "actions", "organization-secrets")},
		{"the repository's Dependabot secrets", "dependabot", k.segs("repos", owner, name, "dependabot", "secrets")},
	}
	for _, l := range lists {
		names, err := k.githubSecrets(ctx, l.u)
		if err != nil {
			if err := k.unread(l.what, err); err != nil {
				return KeyStore{}, err
			}
			continue
		}
		for _, n := range names {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: n, Where: l.where})
		}
	}
	envs, err := k.githubEnvironments(ctx, owner, name)
	if err != nil {
		if err := k.unread("the repository's environments", err); err != nil {
			return KeyStore{}, err
		}
		k.ks.EnvironmentsUnread = k.ks.Unread[len(k.ks.Unread)-1]
	}
	ch := &restChannel{client: k.in.Client, auth: k.auth, repo: k.segs("repos", owner, name), github: true}
	for _, env := range envs {
		ne := NamedEnvironment{Name: env}
		ne.Environment, ne.Err = ch.Environment(ctx, env)
		k.ks.Environments = append(k.ks.Environments, ne)
		names, err := k.githubSecrets(ctx, k.segs("repos", owner, name, "environments", env, "secrets"))
		if err != nil {
			if err := k.unread("the secrets of environment "+env, err); err != nil {
				return KeyStore{}, err
			}
			continue
		}
		for _, n := range names {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: n, Where: "environment", Environment: env})
		}
	}
	return k.ks, nil
}

// githubSecrets lists the names of a GitHub secrets listing.
func (k *keyReader) githubSecrets(ctx context.Context, u string) ([]string, error) {
	var names []string
	for page := 1; page <= keyPages; page++ {
		var list struct {
			Total   int `json:"total_count"`
			Secrets []struct {
				Name string `json:"name"`
			} `json:"secrets"`
		}
		if err := k.get(ctx, fmt.Sprintf("%s?per_page=%d&page=%d", u, keyPageSize, page), &list); err != nil {
			return nil, err
		}
		for _, s := range list.Secrets {
			names = append(names, s.Name)
		}
		if len(list.Secrets) < keyPageSize || len(names) >= list.Total {
			return names, nil
		}
	}
	return names, errors.New("more secrets than touchmark reads")
}

// githubEnvironments lists the names of the repository's environments.
func (k *keyReader) githubEnvironments(ctx context.Context, owner, name string) ([]string, error) {
	var names []string
	for page := 1; page <= keyPages; page++ {
		var list struct {
			Total        int `json:"total_count"`
			Environments []struct {
				Name string `json:"name"`
			} `json:"environments"`
		}
		u := fmt.Sprintf("%s?per_page=%d&page=%d", k.segs("repos", owner, name, "environments"), keyPageSize, page)
		if err := k.get(ctx, u, &list); err != nil {
			return nil, err
		}
		for _, e := range list.Environments {
			if validName(e.Name) {
				names = append(names, e.Name)
			}
		}
		if len(list.Environments) < keyPageSize || len(names) >= list.Total {
			return names, nil
		}
	}
	return names, nil
}

// gitlabVariable is a CI/CD variable of GitLab; its value is not decoded.
type gitlabVariable struct {
	Key       string `json:"key"`
	Protected bool   `json:"protected"`
	Masked    bool   `json:"masked"`
	Scope     string `json:"environment_scope"`
}

// gitlabLevel is one access level of a protected branch or tag.
type gitlabLevel struct {
	AccessLevel int    `json:"access_level"`
	UserID      *int64 `json:"user_id"`
	GroupID     *int64 `json:"group_id"`
	DeployKeyID *int64 `json:"deploy_key_id"`
}

// gitlab reads the key store of a hub on GitLab.
func (k *keyReader) gitlab(ctx context.Context) (KeyStore, error) {
	var project struct {
		Path          string `json:"path_with_namespace"`
		DefaultBranch string `json:"default_branch"`
		Visibility    string `json:"visibility"`
		Namespace     struct {
			Kind     string `json:"kind"`
			FullPath string `json:"full_path"`
		} `json:"namespace"`
		PipelineVariables *string `json:"ci_pipeline_variables_minimum_override_role"`
	}
	id := k.in.RepoID
	if err := k.get(ctx, k.segs("projects", id), &project); err != nil {
		return KeyStore{}, fmt.Errorf("hub keys: read the hub project %s: %w", id, err)
	}
	k.ks = KeyStore{Platform: "gitlab", RepoPath: project.Path, DefaultBranch: project.DefaultBranch, Visibility: visibility(project.Visibility)}
	if project.PipelineVariables != nil {
		k.ks.PipelineVariables = *project.PipelineVariables
	} else {
		k.ks.Unread = append(k.ks.Unread, "the minimum role for pipeline variables: not shown to this token")
	}
	vars, err := gitlabList[gitlabVariable](ctx, k, k.segs("projects", id, "variables"))
	if err != nil {
		if err := k.unread("the project's CI/CD variables", err); err != nil {
			return KeyStore{}, err
		}
	}
	for _, v := range vars {
		k.ks.Secrets = append(k.ks.Secrets, Secret{Name: v.Key, Where: "project", Protected: v.Protected, Masked: v.Masked, Scope: v.Scope})
	}
	if project.Namespace.Kind == "group" && project.Namespace.FullPath != "" {
		parts := strings.Split(project.Namespace.FullPath, "/")
		for i := range parts {
			group := strings.Join(parts[:i+1], "/")
			vars, err := gitlabList[gitlabVariable](ctx, k, k.segs("groups", group, "variables"))
			if err != nil {
				if err := k.unread("the CI/CD variables of group "+group, err); err != nil {
					return KeyStore{}, err
				}
				continue
			}
			for _, v := range vars {
				k.ks.Secrets = append(k.ks.Secrets, Secret{Name: v.Key, Where: "group", Group: group, Protected: v.Protected, Masked: v.Masked, Scope: v.Scope})
			}
		}
	}
	type protectedBranch struct {
		Name  string        `json:"name"`
		Push  []gitlabLevel `json:"push_access_levels"`
		Merge []gitlabLevel `json:"merge_access_levels"`
	}
	branches, err := gitlabList[protectedBranch](ctx, k, k.segs("projects", id, "protected_branches"))
	if err != nil {
		if k.ks.ProtectedBranchesUnread, err = unreadable("the protected branches", err); err != nil {
			return KeyStore{}, err
		}
	}
	for _, b := range branches {
		k.ks.ProtectedBranches = append(k.ks.ProtectedBranches, protectedRef(b.Name, append(b.Push, b.Merge...)))
	}
	type protectedTag struct {
		Name   string        `json:"name"`
		Create []gitlabLevel `json:"create_access_levels"`
	}
	tags, err := gitlabList[protectedTag](ctx, k, k.segs("projects", id, "protected_tags"))
	if err != nil {
		if k.ks.ProtectedTagsUnread, err = unreadable("the protected tags", err); err != nil {
			return KeyStore{}, err
		}
	}
	for _, t := range tags {
		k.ks.ProtectedTags = append(k.ks.ProtectedTags, protectedRef(t.Name, t.Create))
	}
	return k.ks, nil
}

// protectedRef summarizes the access levels of a protected ref.
func protectedRef(name string, levels []gitlabLevel) ProtectedRef {
	r := ProtectedRef{Name: name}
	for _, l := range levels {
		switch {
		case l.UserID != nil || l.GroupID != nil:
			r.Others = true
		case l.DeployKeyID != nil:
		default:
			r.Levels = append(r.Levels, l.AccessLevel)
		}
	}
	return r
}

// gitlabList reads a GitLab listing of T by page, until a short page.
func gitlabList[T any](ctx context.Context, k *keyReader, u string) ([]T, error) {
	var out []T
	for page := 1; page <= keyPages; page++ {
		var items []T
		if err := k.get(ctx, fmt.Sprintf("%s?per_page=%d&page=%d", u, keyPageSize, page), &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) < keyPageSize {
			return out, nil
		}
	}
	return out, errors.New("more items than touchmark reads")
}

// gitea reads the key store of a hub on Gitea or Forgejo.
func (k *keyReader) gitea(ctx context.Context) (KeyStore, error) {
	var repo struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := k.get(ctx, k.segs("repositories", k.in.RepoID), &repo); err != nil {
		return KeyStore{}, fmt.Errorf("hub keys: read the hub repository %s: %w", k.in.RepoID, err)
	}
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok || !validName(owner) || !validName(name) {
		return KeyStore{}, fmt.Errorf("hub keys: the hub repository's name %q is not owner/name", repo.FullName)
	}
	k.ks = KeyStore{Platform: k.in.Platform, RepoPath: repo.FullName, DefaultBranch: repo.DefaultBranch, Visibility: "public"}
	if repo.Private {
		k.ks.Visibility = "private"
	}
	type secret struct {
		Name string `json:"name"`
	}
	for _, l := range []struct{ what, where, u string }{
		{"the repository's Actions secrets", "repository", k.segs("repos", owner, name, "actions", "secrets")},
		{"the organization's Actions secrets", "organization", k.segs("orgs", owner, "actions", "secrets")},
	} {
		var all []secret
		for page := 1; page <= keyPages; page++ {
			var items []secret
			err := k.get(ctx, fmt.Sprintf("%s?limit=%d&page=%d", l.u, 50, page), &items)
			if err != nil {
				if err := k.unread(l.what, err); err != nil {
					return KeyStore{}, err
				}
				all = nil
				break
			}
			all = append(all, items...)
			if len(items) < 50 {
				break
			}
		}
		for _, s := range all {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: s.Name, Where: l.where})
		}
	}
	return k.ks, nil
}

// FormatLevels names GitLab access levels for messages: "No one",
// "Developers", "Maintainers", "level 25".
func FormatLevels(levels []int) string {
	var names []string
	for _, l := range levels {
		switch l {
		case 0:
			names = append(names, "No one")
		case 30:
			names = append(names, "Developers")
		case 40:
			names = append(names, "Maintainers")
		case 60:
			names = append(names, "administrators")
		default:
			names = append(names, "level "+strconv.Itoa(l))
		}
	}
	return strings.Join(names, ", ")
}
