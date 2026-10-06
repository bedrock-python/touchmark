// Package hubch is the hub channel: what touchmark learns
// about the hub itself from its CI environment, and the hub CI's own token
// talking to the hub repository only.
//
// It answers: which CI runs us, the hub's fingerprint (host and immutable
// repository id), its visibility, the ref and event of this run, and the
// commit at the tip of the hub's default branch (the head guard: a run
// from an old commit writes nothing).
package hubch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
)

// CI names the CI system.
type CI string

const (
	GitHubActions  CI = "github-actions"
	GitLabCI       CI = "gitlab-ci"
	GiteaActions   CI = "gitea-actions"
	ForgejoActions CI = "forgejo-actions"
	Local          CI = "local"
)

// GitLabUser is the user of the job token in git's Basic header; register
// the token with it (redact.Registry.Add(token, GitLabUser)) so that the
// header's base64 form is masked too.
const GitLabUser = "gitlab-ci-token"

// gitTimeout bounds `git ls-remote`, as long as any git fetch or push of
// a run may take.
const gitTimeout = 3 * time.Minute

// Context is what the environment tells about the hub and this run. Fields
// the environment does not provide are empty.
type Context struct {
	CI CI
	// ServerURL is the hub's platform, e.g. "https://github.com".
	ServerURL string
	// APIURL is the REST base for the hub's platform.
	APIURL string
	// Host is the lowercased host of ServerURL (with port if any).
	Host string
	// RepoID is the hub repository's immutable id; RepoPath its path.
	RepoID   string
	RepoPath string
	// DefaultBranch is the hub's default branch.
	DefaultBranch string
	// RefName is the branch or tag this run builds; RefIsBranch tells which.
	RefName     string
	RefIsBranch bool
	// Event is the triggering event: push, pull_request, schedule,
	// workflow_dispatch, merge_request_event, web, …
	Event string
	// RefProtected is GitLab's CI_COMMIT_REF_PROTECTED.
	RefProtected bool
	// Environment is GitLab's CI_ENVIRONMENT_NAME.
	Environment string
	// Visibility is "public", "internal" or "private"; "" when unknown.
	Visibility string
	// RepositoryURL is the hub's git URL without credentials (GitLab
	// CI_REPOSITORY_URL with the userinfo removed).
	RepositoryURL string
	// TLSCAFile is GitLab's CI_SERVER_TLS_CA_FILE: the CA bundle of a
	// self-managed instance with a private CA, which the channel's git
	// trusts since it never reads the runner's git config.
	TLSCAFile string
}

// Detect reads the CI environment.
//
//   - GitHub Actions (GITHUB_ACTIONS=true, not Gitea/Forgejo):
//     GITHUB_SERVER_URL, GITHUB_API_URL, GITHUB_REPOSITORY_ID,
//     GITHUB_REPOSITORY, GITHUB_REF_NAME, GITHUB_REF_TYPE,
//     GITHUB_EVENT_NAME, and from the JSON at GITHUB_EVENT_PATH
//     repository.visibility and repository.default_branch.
//   - Gitea Actions (GITEA_ACTIONS=true) and Forgejo Actions
//     (FORGEJO_ACTIONS=true): the same GITHUB_* variables; repository id
//     from the event payload's repository.id when the variable is absent.
//   - GitLab CI (GITLAB_CI=true): CI_SERVER_URL, CI_API_V4_URL,
//     CI_PROJECT_ID, CI_PROJECT_PATH, CI_DEFAULT_BRANCH,
//     CI_COMMIT_REF_NAME, CI_COMMIT_BRANCH (set only for branches),
//     CI_PIPELINE_SOURCE, CI_COMMIT_REF_PROTECTED, CI_ENVIRONMENT_NAME,
//     CI_PROJECT_VISIBILITY, CI_REPOSITORY_URL, CI_SERVER_TLS_CA_FILE.
//   - Otherwise Local with everything empty.
//
// readFile reads the event payload; errors reading or parsing it leave the
// payload fields empty. Detect never returns secrets.
//
// Details:
//   - FORGEJO_ACTIONS wins over GITEA_ACTIONS, which wins over
//     GITHUB_ACTIONS (both runners also set GITHUB_ACTIONS); a flag counts
//     when it is "true" in any case.
//   - Values are trimmed. URLs lose userinfo, query and trailing slashes;
//     a URL that is not absolute http(s) reads as absent. Host drops the
//     scheme's default port. Ids that are not positive decimal integers
//     read as absent, so a garbled id never makes a fingerprint.
//   - Without GITHUB_API_URL, APIURL is derived: https://api.github.com
//     for github.com, https://api.<host> for *.ghe.com, <server>/api/v3
//     for GitHub Enterprise Server, <server>/api/v1 for Gitea and Forgejo;
//     without CI_API_V4_URL, <server>/api/v4.
//   - RefIsBranch needs GITHUB_REF_TYPE=branch and, when GITHUB_REF is
//     set, GITHUB_REF=refs/heads/<GITHUB_REF_NAME>: a pull request run
//     (refs/pull/<n>/merge) is not a branch run. Without GITHUB_REF_TYPE
//     (older runners) both come from GITHUB_REF. On GitLab it needs
//     CI_COMMIT_BRANCH equal to CI_COMMIT_REF_NAME.
//   - Without repository.visibility in the payload (Gitea, Forgejo),
//     visibility comes from repository.private, repository.internal and
//     repository.owner.visibility ("limited" is internal, "private"
//     private); without repository.private it is unknown.
//   - Without CI_REPOSITORY_URL, RepositoryURL is <server>/<path>.git.
func Detect(getenv func(string) string, readFile func(string) ([]byte, error)) Context {
	if getenv == nil {
		return Context{CI: Local}
	}
	env := func(name string) string { return strings.TrimSpace(getenv(name)) }
	switch {
	case isTrue(env("FORGEJO_ACTIONS")):
		return detectActions(ForgejoActions, env, readFile)
	case isTrue(env("GITEA_ACTIONS")):
		return detectActions(GiteaActions, env, readFile)
	case isTrue(env("GITHUB_ACTIONS")):
		return detectActions(GitHubActions, env, readFile)
	case isTrue(env("GITLAB_CI")):
		return detectGitLab(env)
	}
	return Context{CI: Local}
}

func detectActions(ci CI, env func(string) string, readFile func(string) ([]byte, error)) Context {
	c := Context{CI: ci}
	c.ServerURL = cleanURL(env("GITHUB_SERVER_URL"))
	c.Host = hostOf(c.ServerURL)
	c.APIURL = cleanURL(env("GITHUB_API_URL"))
	if c.APIURL == "" {
		c.APIURL = defaultAPIURL(ci, c.ServerURL, c.Host)
	}
	c.RepoID = repoID(env("GITHUB_REPOSITORY_ID"))
	c.RepoPath = env("GITHUB_REPOSITORY")
	c.RefName, c.RefIsBranch = actionsRef(env("GITHUB_REF"), env("GITHUB_REF_NAME"), env("GITHUB_REF_TYPE"))
	c.Event = env("GITHUB_EVENT_NAME")

	p := readPayload(env("GITHUB_EVENT_PATH"), readFile)
	c.Visibility = p.visibility
	c.DefaultBranch = p.defaultBranch
	if c.RepoID == "" && ci != GitHubActions {
		c.RepoID = p.id
	}
	return c
}

func detectGitLab(env func(string) string) Context {
	c := Context{CI: GitLabCI}
	c.ServerURL = cleanURL(env("CI_SERVER_URL"))
	c.Host = hostOf(c.ServerURL)
	c.APIURL = cleanURL(env("CI_API_V4_URL"))
	if c.APIURL == "" && c.ServerURL != "" {
		c.APIURL = c.ServerURL + "/api/v4"
	}
	c.RepoID = repoID(env("CI_PROJECT_ID"))
	c.RepoPath = env("CI_PROJECT_PATH")
	c.DefaultBranch = env("CI_DEFAULT_BRANCH")
	c.RefName = env("CI_COMMIT_REF_NAME")
	if b := env("CI_COMMIT_BRANCH"); b != "" && b == c.RefName {
		c.RefIsBranch = true
	}
	c.Event = env("CI_PIPELINE_SOURCE")
	c.RefProtected = isTrue(env("CI_COMMIT_REF_PROTECTED"))
	c.Environment = env("CI_ENVIRONMENT_NAME")
	c.Visibility = visibility(env("CI_PROJECT_VISIBILITY"))
	c.RepositoryURL = cleanURL(env("CI_REPOSITORY_URL"))
	if c.RepositoryURL == "" && c.ServerURL != "" && c.RepoPath != "" {
		c.RepositoryURL = c.ServerURL + "/" + c.RepoPath + ".git"
	}
	c.TLSCAFile = env("CI_SERVER_TLS_CA_FILE")
	return c
}

// actionsRef returns the ref name and whether it is a branch, from
// GITHUB_REF, GITHUB_REF_NAME and GITHUB_REF_TYPE.
func actionsRef(ref, name, typ string) (string, bool) {
	switch typ {
	case "branch":
		if ref == "" {
			return name, name != ""
		}
		b, ok := strings.CutPrefix(ref, "refs/heads/")
		return name, ok && b == name
	case "":
		if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok && (name == "" || name == b) {
			return b, true
		}
		if t, ok := strings.CutPrefix(ref, "refs/tags/"); ok && name == "" {
			return t, false
		}
	}
	return name, false
}

// payload is what Detect takes from an Actions event payload.
type payload struct {
	visibility    string
	defaultBranch string
	id            string
}

// readPayload reads the repository fields of the event payload at path.
// Each field is read on its own: one of an unexpected type leaves only that
// field empty.
func readPayload(path string, readFile func(string) ([]byte, error)) payload {
	var p payload
	if path == "" || readFile == nil {
		return p
	}
	data, err := readFile(path)
	if err != nil {
		return p
	}
	var event struct {
		Repository map[string]json.RawMessage `json:"repository"`
	}
	if json.Unmarshal(data, &event) != nil || event.Repository == nil {
		return p
	}
	repo := event.Repository
	p.defaultBranch = strings.TrimSpace(jsonString(repo["default_branch"]))
	p.id = jsonID(repo["id"])
	p.visibility = payloadVisibility(repo)
	return p
}

func payloadVisibility(repo map[string]json.RawMessage) string {
	if v := visibility(jsonString(repo["visibility"])); v != "" {
		return v
	}
	var private *bool
	if json.Unmarshal(repo["private"], &private) != nil || private == nil {
		return ""
	}
	if *private {
		return "private"
	}
	var internal bool
	if json.Unmarshal(repo["internal"], &internal) == nil && internal {
		return "internal"
	}
	var owner struct {
		Visibility string `json:"visibility"`
	}
	if json.Unmarshal(repo["owner"], &owner) == nil {
		switch strings.ToLower(owner.Visibility) {
		case "limited":
			return "internal"
		case "private":
			return "private"
		}
	}
	return "public"
}

func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// jsonID reads an id given as a JSON number or string.
func jsonID(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, `"`) && json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return repoID(s)
}

// repoID returns s when it is a positive decimal integer (a repository id
// on every supported platform), else "".
func repoID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 20 || s[0] == '0' || strings.Trim(s, "0123456789") != "" {
		return ""
	}
	return s
}

func visibility(v string) string {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "public", "internal", "private":
		return v
	}
	return ""
}

func isTrue(v string) bool { return strings.EqualFold(v, "true") }

// cleanURL returns raw without userinfo, query, fragment and trailing
// slashes, or "" when it is not an absolute http(s) URL with a host.
func cleanURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return ""
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return strings.TrimRight(u.String(), "/")
}

// hostOf returns the lowercased host of an http(s) URL, with its port
// unless it is the scheme's default.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	port := u.Port()
	switch strings.ToLower(u.Scheme) {
	case "https":
		if port == "443" {
			port = ""
		}
	case "http":
		if port == "80" {
			port = ""
		}
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func defaultAPIURL(ci CI, server, host string) string {
	switch {
	case server == "":
		return ""
	case ci == GiteaActions || ci == ForgejoActions:
		return server + "/api/v1"
	case host == "github.com":
		return "https://api.github.com"
	case strings.HasSuffix(host, ".ghe.com"):
		return "https://api." + host
	}
	return server + "/api/v3"
}

// Fingerprint returns Host + "/" + RepoID, or "" when either is unknown.
func (c Context) Fingerprint() string {
	if c.Host == "" || c.RepoID == "" {
		return ""
	}
	return c.Host + "/" + c.RepoID
}

// Channel talks to the hub repository with the hub CI's own token.
type Channel interface {
	// Head returns the commit id at the tip of the hub's default branch.
	Head(ctx context.Context) (string, error)
}

// Token returns the hub CI's own token from the environment: GITHUB_TOKEN
// on GitHub Actions, CI_JOB_TOKEN on GitLab CI, GITEA_TOKEN or GITHUB_TOKEN
// on Gitea and Forgejo Actions; "" when absent or Local.
//
// The caller registers the token with its redact.Registry before use, on
// GitLab with the basic user GitLabUser.
func Token(c Context, getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	env := func(name string) string { return strings.TrimSpace(getenv(name)) }
	switch c.CI {
	case GitHubActions:
		return env("GITHUB_TOKEN")
	case GitLabCI:
		return env("CI_JOB_TOKEN")
	case GiteaActions, ForgejoActions:
		if t := env("GITEA_TOKEN"); t != "" {
			return t
		}
		return env("GITHUB_TOKEN")
	}
	return ""
}

// New returns the channel for c:
//   - GitHub, Gitea, Forgejo: REST over client — GitHub
//     GET {api}/repos/{path}/git/ref/heads/{branch}; Gitea and Forgejo
//     GET {api}/repos/{path}/branches/{branch} — with the token sent only to
//     the host of c.APIURL;
//   - GitLab: `git ls-remote` of c.RepositoryURL with the job token passed
//     as an http.extraHeader through GIT_CONFIG_COUNT/KEY/VALUE environment
//     variables (never in the URL or argv), because CI_JOB_TOKEN cannot
//     read branches through the API.
//
// It fails for Local or when the context lacks what the channel needs.
//
// The branch whose tip Head reads is c.DefaultBranch; when the CI names no
// default branch but the run builds a branch (GitHub's and Gitea's schedule
// event, whose payload has no repository, and which always builds the
// default branch), it is the branch the run builds, c.RefName: the head
// guard then still holds for the scheduled runs a template triggers.
//
// A nil client is a default httpx client. An empty token reads anonymously.
// The token goes over https only (plain http to localhost for tests); the
// GitLab channel also reads a file:// URL, without a token (tests). git
// runs outside any repository, without credential helpers, prompts or
// redirects, and isolated from the runner: no system or
// global config, an empty HOME, and none of the inherited GIT_* variables
// but GIT_SSL_CAINFO and GIT_SSL_CAPATH, so that no config, trace or
// GIT_SSL_NO_VERIFY of the machine can redirect the job token, print it or
// send it over unverified TLS. It trusts c.TLSCAFile when set
// (http.sslCAInfo).
func New(c Context, client *httpx.Client, token string) (Channel, error) {
	switch c.CI {
	case GitHubActions, GiteaActions, ForgejoActions:
		return newREST(c, client, token)
	case GitLabCI:
		return newGit(c, token)
	case Local, "":
		return nil, errors.New("hub channel: not running in a known CI")
	}
	return nil, fmt.Errorf("hub channel: unknown CI %q", c.CI)
}

// headBranch is the branch whose tip the channel of c reads (see New).
func headBranch(c Context) string {
	if c.DefaultBranch == "" && c.RefIsBranch {
		return c.RefName
	}
	return c.DefaultBranch
}

// restChannel reads the default branch through the REST API of GitHub,
// Gitea or Forgejo.
type restChannel struct {
	client *httpx.Client
	auth   *httpx.Auth // nil without a token
	url    string      // the branch endpoint
	repo   string      // the repository endpoint
	branch string
	github bool
	ci     CI
}

func newREST(c Context, client *httpx.Client, token string) (Channel, error) {
	c.DefaultBranch = headBranch(c)
	if err := checkBranch(c.DefaultBranch); err != nil {
		return nil, fmt.Errorf("hub channel: default branch: %w", err)
	}
	owner, name, ok := strings.Cut(c.RepoPath, "/")
	if !ok || !validName(owner) || !validName(name) {
		return nil, fmt.Errorf("hub channel: invalid repository path %q", c.RepoPath)
	}
	api, err := url.Parse(c.APIURL)
	if err != nil || api.Host == "" || api.User != nil || (api.Scheme != "https" && api.Scheme != "http") {
		return nil, errors.New("hub channel: the API URL is not an absolute http(s) URL without credentials")
	}
	api.RawQuery, api.ForceQuery, api.Fragment, api.RawFragment = "", false, "", ""
	if client == nil {
		client = httpx.New(httpx.Options{})
	}
	ch := &restChannel{client: client, branch: c.DefaultBranch, github: c.CI == GitHubActions, ci: c.CI}
	base := strings.TrimRight(api.String(), "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	ch.repo = base
	scheme := "token "
	if ch.github {
		ch.url = base + "/git/ref/heads/" + escapeSegments(c.DefaultBranch)
		scheme = "Bearer "
	} else {
		ch.url = base + "/branches/" + escapeSegments(c.DefaultBranch)
	}
	if token != "" {
		if err := checkToken(token); err != nil {
			return nil, err
		}
		value := scheme + token
		ch.auth = &httpx.Auth{
			Hosts:  []string{api.Host},
			Header: func(context.Context) (string, error) { return value, nil },
		}
	}
	return ch, nil
}

func (ch *restChannel) Head(ctx context.Context) (string, error) {
	if ch.github {
		var ref struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if _, err := ch.client.JSON(ctx, http.MethodGet, ch.url, ch.auth, nil, &ref); err != nil {
			return "", fmt.Errorf("hub channel: read the tip of branch %q: %w", ch.branch, err)
		}
		sha := strings.ToLower(ref.Object.SHA)
		if ref.Ref != "refs/heads/"+ch.branch || ref.Object.Type != "commit" || !isOID(sha) {
			return "", fmt.Errorf("hub channel: unexpected answer for branch %q: ref %q, %s %q",
				ch.branch, ref.Ref, ref.Object.Type, ref.Object.SHA)
		}
		return sha, nil
	}
	var branch struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := ch.client.JSON(ctx, http.MethodGet, ch.url, ch.auth, nil, &branch); err != nil {
		return "", fmt.Errorf("hub channel: read the tip of branch %q: %w", ch.branch, err)
	}
	sha := strings.ToLower(branch.Commit.ID)
	if branch.Name != ch.branch || !isOID(sha) {
		return "", fmt.Errorf("hub channel: unexpected answer for branch %q: name %q, commit %q",
			ch.branch, branch.Name, branch.Commit.ID)
	}
	return sha, nil
}

// Visibility reads the hub repository's visibility ("public", "internal"
// or "private") through the REST API (GET /repos/{owner}/{repo}), for a run
// whose event payload names none: GitHub's and Gitea's schedule event
// (the visibility comes from the payload, the CI or the hub channel). The
// answer is read as the payload's repository block is
// (visibility, else private, internal and the owner's visibility); an
// answer that says none of them is an error.
func (ch *restChannel) Visibility(ctx context.Context) (string, error) {
	var repo map[string]json.RawMessage
	if _, err := ch.client.JSON(ctx, http.MethodGet, ch.repo, ch.auth, nil, &repo); err != nil {
		return "", fmt.Errorf("hub channel: read the hub repository: %w", err)
	}
	v := payloadVisibility(repo)
	if v == "" {
		return "", errors.New("hub channel: the hub repository's answer names no visibility")
	}
	return v, nil
}

// VisibilityReader is a Channel that can also read the hub repository's
// visibility (the REST channel of GitHub, Gitea and Forgejo).
type VisibilityReader interface {
	Visibility(ctx context.Context) (string, error)
}

// Environment is what the hub channel reads of a deployment environment of
// the hub repository on GitHub: which refs may run jobs in it, and so see
// its secrets.
type Environment struct {
	// AllRefs is set when the environment has no deployment branch policy:
	// a job of any branch or tag may use it.
	AllRefs bool
	// ProtectedBranches is set when only branches with branch protection
	// may use it (every branch may when none is protected).
	ProtectedBranches bool
	// Policies are its custom deployment branch and tag policies, when it
	// has them: name patterns (fnmatch) with type "branch" or "tag".
	Policies []EnvironmentPolicy
}

// EnvironmentPolicy is one custom deployment branch or tag policy.
type EnvironmentPolicy struct {
	Name string
	Type string // "branch" or "tag"
}

// ErrNoEnvironment is wrapped by the error of EnvironmentReader.Environment
// when the hub repository has no environment of that name (HTTP 404).
var ErrNoEnvironment = errors.New("no such environment")

// EnvironmentReader is a Channel that can also read a deployment
// environment of the hub repository (the REST channel of GitHub).
type EnvironmentReader interface {
	Environment(ctx context.Context, name string) (Environment, error)
}

// maxPolicyPages bounds the deployment branch policies read: 1 000.
const maxPolicyPages = 10

// Environment reads the deployment environment name of the hub repository
// on GitHub: GET /repos/{owner}/{repo}/environments/{name}, and when its
// deployment_branch_policy has custom_branch_policies, GET
// …/deployment-branch-policies (100 a page). Both need the Actions read
// permission of the token (GITHUB_TOKEN: permissions actions: read);
// public repositories answer without a token. A 404 of the environment is
// ErrNoEnvironment; the channels of Gitea and Forgejo read none (an
// error).
func (ch *restChannel) Environment(ctx context.Context, name string) (Environment, error) {
	if !ch.github {
		return Environment{}, errors.New("hub channel: only GitHub has deployment environments")
	}
	if !validName(name) {
		return Environment{}, fmt.Errorf("hub channel: invalid environment name %q", name)
	}
	base := ch.repo + "/environments/" + url.PathEscape(name)
	var env struct {
		Name   string `json:"name"`
		Policy *struct {
			Protected bool `json:"protected_branches"`
			Custom    bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
	}
	if _, err := ch.client.JSON(ctx, http.MethodGet, base, ch.auth, nil, &env); err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return Environment{}, fmt.Errorf("hub channel: environment %s: %w", name, ErrNoEnvironment)
		}
		return Environment{}, fmt.Errorf("hub channel: read environment %s: %w", name, err)
	}
	if !strings.EqualFold(env.Name, name) {
		return Environment{}, fmt.Errorf("hub channel: the answer for environment %s names %q", name, env.Name)
	}
	switch {
	case env.Policy == nil:
		return Environment{AllRefs: true}, nil
	case !env.Policy.Custom:
		return Environment{ProtectedBranches: env.Policy.Protected}, nil
	}
	out := Environment{ProtectedBranches: env.Policy.Protected, Policies: []EnvironmentPolicy{}}
	for page := 1; page <= maxPolicyPages; page++ {
		var list struct {
			Total    int `json:"total_count"`
			Policies []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"branch_policies"`
		}
		u := fmt.Sprintf("%s/deployment-branch-policies?per_page=100&page=%d", base, page)
		if _, err := ch.client.JSON(ctx, http.MethodGet, u, ch.auth, nil, &list); err != nil {
			return Environment{}, fmt.Errorf("hub channel: read the deployment branch policies of %s: %w", name, err)
		}
		for _, p := range list.Policies {
			typ := p.Type
			if typ == "" {
				typ = "branch"
			}
			out.Policies = append(out.Policies, EnvironmentPolicy{Name: p.Name, Type: typ})
		}
		if len(list.Policies) < 100 || len(out.Policies) >= list.Total {
			return out, nil
		}
	}
	return Environment{}, fmt.Errorf("hub channel: environment %s has more deployment branch policies than touchmark reads", name)
}

// gitChannel reads the default branch with `git ls-remote` (GitLab).
type gitChannel struct {
	url    string // without credentials
	scheme string // "https", "http" or "file"
	branch string
	// scope is the URL prefix the header is bound to ("https://host/"), and
	// header the extra header ("Authorization: Basic …"); both empty
	// without a token.
	scope  string
	header string
	// caFile is the CA bundle git trusts for https, "" for the default.
	caFile string
	// bin is the git executable, "git" when empty (tests replace it).
	bin string
}

func newGit(c Context, token string) (Channel, error) {
	c.DefaultBranch = headBranch(c)
	if err := checkBranch(c.DefaultBranch); err != nil {
		return nil, fmt.Errorf("hub channel: default branch: %w", err)
	}
	if c.RepositoryURL == "" {
		return nil, errors.New("hub channel: no repository URL")
	}
	u, err := url.Parse(c.RepositoryURL)
	if err != nil {
		return nil, errors.New("hub channel: the repository URL does not parse")
	}
	if u.User != nil {
		return nil, errors.New("hub channel: the repository URL carries credentials")
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	ch := &gitChannel{url: u.String(), scheme: strings.ToLower(u.Scheme), branch: c.DefaultBranch}
	switch ch.scheme {
	case "https", "http":
		if u.Host == "" {
			return nil, errors.New("hub channel: the repository URL has no host")
		}
		if ch.scheme == "https" {
			ch.caFile = c.TLSCAFile
		}
		if token == "" {
			break
		}
		if ch.scheme == "http" && !isLoopback(strings.ToLower(u.Hostname())) {
			return nil, errors.New("hub channel: the job token is sent over https only")
		}
		if err := checkToken(token); err != nil {
			return nil, err
		}
		ch.scope = ch.scheme + "://" + u.Host + "/"
		ch.header = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(GitLabUser+":"+token))
	case "file":
		if token != "" {
			return nil, errors.New("hub channel: the job token is not sent to a file URL")
		}
	default:
		return nil, fmt.Errorf("hub channel: unsupported repository URL scheme %q", u.Scheme)
	}
	return ch, nil
}

func (ch *gitChannel) Head(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "touchmark-hub-")
	if err != nil {
		return "", fmt.Errorf("hub channel: %w", err)
	}
	defer os.RemoveAll(dir)
	global := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		return "", fmt.Errorf("hub channel: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	args, env := ch.command(filepath.Dir(dir), dir, global)
	g := &gitx.Git{Dir: dir, Bin: ch.bin, Env: env, Inherit: channelEnviron}
	out, err := g.Run(ctx, nil, args...)
	if err != nil {
		return "", fmt.Errorf("hub channel: read the tip of branch %q: %w", ch.branch, err)
	}
	return parseLsRemote(out, ch.branch)
}

// command returns the arguments and the extra environment of
// `git ls-remote`, run in an empty directory under ceiling with home as its
// HOME and global as its (empty) global config. The token is only in the
// environment, as an extra header bound to the repository's host.
func (ch *gitChannel) command(ceiling, home, global string) (args, env []string) {
	args = []string{
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "http.followRedirects=false",
		"-c", "protocol.allow=never",
		"-c", "protocol." + ch.scheme + ".allow=always",
	}
	if ch.caFile != "" {
		args = append(args, "-c", "http.sslCAInfo="+ch.caFile)
	}
	args = append(args, "ls-remote", "--refs", ch.url, "refs/heads/"+ch.branch)
	env = []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_CEILING_DIRECTORIES=" + ceiling,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + global,
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
	}
	if ch.header != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+ch.scope+".extraHeader",
			"GIT_CONFIG_VALUE_0="+ch.header,
		)
	}
	return args, env
}

// keptGitVars are the only GIT_* variables the channel's git inherits: a
// private CA bundle of the runner.
var keptGitVars = []string{"GIT_SSL_CAINFO", "GIT_SSL_CAPATH"}

// channelEnviron is the process environment without git's own variables
// (keptGitVars aside): config passed through GIT_CONFIG_PARAMETERS or
// GIT_CONFIG_COUNT, GIT_SSL_NO_VERIFY, GIT_TRACE* and GIT_CURL_VERBOSE
// (which would print the header), GIT_PROXY_COMMAND and the like. Proxy
// variables such as HTTPS_PROXY stay. Names compare case-insensitively, as
// on Windows.
func channelEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GIT_") && !slices.Contains(keptGitVars, upper) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// parseLsRemote returns the object id of refs/heads/<branch> in the output
// of `git ls-remote`. Only the exact ref counts: ls-remote patterns also
// match refs that merely end in it.
func parseLsRemote(out []byte, branch string) (string, error) {
	want := "refs/heads/" + branch
	for line := range strings.SplitSeq(string(out), "\n") {
		oid, ref, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "\t")
		if !ok || ref != want {
			continue
		}
		oid = strings.ToLower(oid)
		if !isOID(oid) {
			return "", fmt.Errorf("hub channel: git ls-remote: bad object id %q for %s", oid, want)
		}
		return oid, nil
	}
	return "", fmt.Errorf("hub channel: branch %q not found", branch)
}

// checkBranch rejects names git would not accept as a branch (a subset of
// git check-ref-format that keeps URLs and arguments safe).
func checkBranch(b string) error {
	if b == "" {
		return errors.New("empty branch name")
	}
	bad := b == "@" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{")
	for _, r := range b {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			bad = true
		}
	}
	for _, seg := range strings.Split(b, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("invalid branch name %q", b)
	}
	return nil
}

// validName reports whether s is an owner or repository name on GitHub,
// Gitea and Forgejo.
func validName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// checkToken rejects a token that cannot go into a header. The error never
// quotes it.
func checkToken(token string) error {
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= ' ' || c == 0x7f {
			return errors.New("hub channel: the token has whitespace or control characters")
		}
	}
	return nil
}

// escapeSegments escapes each '/'-separated segment of a branch name for a
// URL path.
func escapeSegments(b string) string {
	segs := strings.Split(b, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// isLoopback reports whether host may receive the token over plain http.
func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// isOID reports whether s is a full lowercase hex object id (sha1 or sha256).
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
