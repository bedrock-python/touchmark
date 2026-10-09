package hubch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
)

// A hub in Azure Repos (Azure DevOps Services), run by Azure Pipelines.
// Sources:
//   - the predefined variables:
//     https://learn.microsoft.com/en-us/azure/devops/pipelines/build/variables
//   - the job access token (System.AccessToken):
//     https://learn.microsoft.com/en-us/azure/devops/pipelines/process/access-tokens
//   - the REST API 7.1: https://learn.microsoft.com/en-us/rest/api/azure/devops/
//     (Git: Repositories, Refs, Pull Request Threads; Core: connectionData).

// AzureTokenVar names the job access token of Azure Pipelines in a step's
// environment. Azure Pipelines hands System.AccessToken to a script only
// when the step maps it (env: SYSTEM_ACCESSTOKEN: $(System.AccessToken));
// it acts as the pipeline's build service identity, and with it the hub
// channel reads the hub's default branch, its tip and visibility, and keeps
// plan's comment in a hub pull request. It is the counterpart of
// GITHUB_TOKEN and CI_JOB_TOKEN.
const AzureTokenVar = "SYSTEM_ACCESSTOKEN"

// The host of Azure DevOps Services, the only home touchmark supports for a
// hub on Azure Pipelines: every organization is https://dev.azure.com/<org>.
const azureHost = "dev.azure.com"

// AzureRepoID returns an Azure Repos repository id (a GUID) in the form
// the hub fingerprint carries it: lowercase, without braces, so that the
// fingerprint is dev.azure.com/0f1e…. Anything that is not a GUID gives "".
// Repository ids are GUIDs unique across organizations, so the fingerprint
// needs no organization.
func AzureRepoID(s string) string { return BitbucketRepoID(s) }

// detectAzure reads Azure Pipelines' predefined variables (TF_BUILD=True):
//   - only a build of an Azure Repos Git repository (Build.Repository.Provider
//     TfsGit) of Azure DevOps Services (System.CollectionUri on dev.azure.com
//     or <org>.visualstudio.com) has a hub: ServerURL and APIURL are
//     https://dev.azure.com/<org>, Host dev.azure.com, RepoID
//     Build.Repository.ID in canonical form (AzureRepoID), RepoPath
//     <System.TeamProject>/<Build.Repository.Name>. Anything else leaves
//     them empty, so no fingerprint, and distribute refuses;
//   - Build.Reason PullRequest (a build validation policy), or a
//     Build.SourceBranch under refs/pull/, is Event pull_request, with
//     RefName the pull request's source branch and no branch run;
//     IndividualCI and BatchedCI are push, Schedule schedule, Manual
//     manual, any other reason lowercased. A Build.SourceBranch under
//     refs/heads/ is a branch run of that branch, one under refs/tags/ is
//     not;
//   - Environment is Environment.Name, set in a deployment job;
//   - DefaultBranch and Visibility stay empty: no variable names them, and
//     the hub channel reads them (RepositoryReader).
func detectAzure(env func(string) string) Context {
	c := Context{CI: AzurePipelines}
	if !strings.EqualFold(env("BUILD_REPOSITORY_PROVIDER"), "TfsGit") {
		return c
	}
	collection := env("SYSTEM_COLLECTIONURI")
	if collection == "" {
		collection = env("SYSTEM_TEAMFOUNDATIONCOLLECTIONURI")
	}
	if c.ServerURL = config.AzureCollectionURL(collection); c.ServerURL != "" {
		c.APIURL, c.Host = c.ServerURL, azureHost
	}
	c.RepoID = AzureRepoID(env("BUILD_REPOSITORY_ID"))
	if project, name := env("SYSTEM_TEAMPROJECT"), env("BUILD_REPOSITORY_NAME"); project != "" && name != "" {
		c.RepoPath = project + "/" + name
	}
	source := env("BUILD_SOURCEBRANCH")
	reason := env("BUILD_REASON")
	switch {
	case strings.EqualFold(reason, "PullRequest") || strings.HasPrefix(source, "refs/pull/"):
		c.Event = "pull_request"
		c.RefName = strings.TrimPrefix(env("SYSTEM_PULLREQUEST_SOURCEBRANCH"), "refs/heads/")
	case strings.HasPrefix(source, "refs/heads/"):
		c.RefName, c.RefIsBranch = strings.TrimPrefix(source, "refs/heads/"), true
	case strings.HasPrefix(source, "refs/tags/"):
		c.RefName = strings.TrimPrefix(source, "refs/tags/")
	}
	if c.Event == "" {
		switch strings.ToLower(reason) {
		case "individualci", "batchedci":
			c.Event = "push"
		case "":
		default:
			c.Event = strings.ToLower(reason)
		}
	}
	c.Environment = env("ENVIRONMENT_NAME")
	c.RepositoryURL = cleanURL(env("BUILD_REPOSITORY_URI"))
	return c
}

// azureChannel reads the hub repository through the REST API of Azure
// DevOps Services with the job access token, and keeps plan's comment in a
// hub pull request.
type azureChannel struct {
	client *httpx.Client
	auth   *httpx.Auth // nil without a token
	api    string      // https://dev.azure.com/<org>
	repo   string      // {api}/_apis/git/repositories/{id}
	id     string      // the repository id, canonical
	branch string      // the default branch, "" until read
	info   *RepoInfo   // the repository, once read
}

// The REST API versions the channel asks for: 7.1, and its preview for
// connectionData, which has no other.
const (
	azureAPIVersion     = "7.1"
	azurePreviewVersion = "7.1-preview.1"
)

// newAzure returns the channel of a hub in Azure Repos: the repository
// c.RepoID of the organization at c.APIURL, with token, when set, sent as a
// Bearer token to the API's host only. When c names its default branch,
// Head reads that branch; else the repository's default branch.
func newAzure(c Context, client *httpx.Client, token string) (Channel, error) {
	if c.DefaultBranch != "" {
		if err := checkBranch(c.DefaultBranch); err != nil {
			return nil, fmt.Errorf("hub channel: default branch: %w", err)
		}
	}
	if AzureRepoID(c.RepoID) == "" {
		return nil, errors.New("hub channel: the hub's repository id is unknown: Azure Pipelines builds no Azure Repos Git repository here")
	}
	api, err := url.Parse(c.APIURL)
	if err != nil || api.Host == "" || api.User != nil || (api.Scheme != "https" && api.Scheme != "http") {
		return nil, errors.New("hub channel: the API URL is not an absolute http(s) URL without credentials")
	}
	api.RawQuery, api.ForceQuery, api.Fragment, api.RawFragment = "", false, "", ""
	if client == nil {
		client = httpx.New(httpx.Options{})
	}
	base := strings.TrimRight(api.String(), "/")
	ch := &azureChannel{
		client: client,
		api:    base,
		repo:   base + "/_apis/git/repositories/" + AzureRepoID(c.RepoID),
		id:     AzureRepoID(c.RepoID),
		branch: c.DefaultBranch,
	}
	if token != "" {
		if err := checkToken(token); err != nil {
			return nil, err
		}
		value := "Bearer " + token
		ch.auth = &httpx.Auth{Hosts: []string{api.Host}, Header: func(context.Context) (string, error) { return value, nil }}
	}
	return ch, nil
}

// azureHeaders are sent with every request: JSON, and a 401 instead of a
// redirect to the sign-in page for a refused token.
func azureHeaders() http.Header {
	return http.Header{"Accept": {"application/json"}, "X-Tfs-Fedauthredirect": {"Suppress"}}
}

// do sends one request of the channel with api-version and query.
func (ch *azureChannel) do(ctx context.Context, method, u string, query url.Values, in, out any) error {
	return ch.doVersion(ctx, azureAPIVersion, method, u, query, in, out)
}

// doVersion is do with another api-version.
func (ch *azureChannel) doVersion(ctx context.Context, version, method, u string, query url.Values, in, out any) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("api-version", version)
	resp, err := ch.client.JSONWith(ctx, method, u+"?"+q.Encode(), ch.auth, azureHeaders(), in, out)
	if resp != nil && (resp.Status == http.StatusNonAuthoritativeInfo || resp.Status/100 == 3) {
		return fmt.Errorf("HTTP %d: Azure DevOps sent the request to its sign-in page; the job access token is missing or refused "+
			"(map it into the step: env: %s: $(System.AccessToken))", resp.Status, AzureTokenVar)
	}
	return err
}

// Repository reads the hub repository (GET
// {org}/_apis/git/repositories/{id}): its default branch (refs/heads/…)
// and its project's visibility, which the answer's project may leave out
// (the reference's sample does): then the project itself (GET
// {org}/_apis/projects/{project id}). The answer must name the repository
// the CI named, and a default branch.
func (ch *azureChannel) Repository(ctx context.Context) (RepoInfo, error) {
	if ch.info != nil {
		return *ch.info, nil
	}
	var repo struct {
		ID            string `json:"id"`
		DefaultBranch string `json:"defaultBranch"`
		Project       struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"project"`
	}
	if err := ch.do(ctx, http.MethodGet, ch.repo, nil, nil, &repo); err != nil {
		return RepoInfo{}, fmt.Errorf("hub channel: read the hub repository: %w", err)
	}
	if AzureRepoID(repo.ID) != ch.id {
		return RepoInfo{}, fmt.Errorf("hub channel: the answer names repository %q, not %s", repo.ID, ch.id)
	}
	branch, ok := strings.CutPrefix(repo.DefaultBranch, "refs/heads/")
	if !ok || checkBranch(branch) != nil {
		return RepoInfo{}, errors.New("hub channel: the hub repository's answer names no default branch")
	}
	info := RepoInfo{DefaultBranch: branch}
	vis := repo.Project.Visibility
	if vis == "" {
		project := AzureRepoID(repo.Project.ID)
		if project == "" {
			return RepoInfo{}, errors.New("hub channel: the hub repository's answer names no project")
		}
		var p struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		}
		if err := ch.do(ctx, http.MethodGet, ch.api+"/_apis/projects/"+project, nil, nil, &p); err != nil {
			return RepoInfo{}, fmt.Errorf("hub channel: read the hub's project: %w", err)
		}
		if AzureRepoID(p.ID) != project {
			return RepoInfo{}, fmt.Errorf("hub channel: the answer names project %q, not %s", p.ID, project)
		}
		vis = p.Visibility
	}
	switch v := strings.ToLower(vis); v {
	case "private", "public":
		info.Visibility = v
	default:
		return RepoInfo{}, fmt.Errorf("hub channel: the hub project's visibility %q is neither private nor public", vis)
	}
	ch.info = &info
	return info, nil
}

// Visibility reads the hub's visibility, its project's (VisibilityReader).
func (ch *azureChannel) Visibility(ctx context.Context) (string, error) {
	info, err := ch.Repository(ctx)
	return info.Visibility, err
}

// Head reads the tip of the default branch: GET
// …/repositories/{id}/refs?filter=heads/{branch}, a prefix filter whose
// exact match refs/heads/{branch} carries the commit id.
func (ch *azureChannel) Head(ctx context.Context) (string, error) {
	branch := ch.branch
	if branch == "" {
		info, err := ch.Repository(ctx)
		if err != nil {
			return "", err
		}
		branch = info.DefaultBranch
	}
	var refs struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	if err := ch.do(ctx, http.MethodGet, ch.repo+"/refs", url.Values{"filter": {"heads/" + branch}}, nil, &refs); err != nil {
		return "", fmt.Errorf("hub channel: read the tip of branch %q: %w", branch, err)
	}
	for _, r := range refs.Value {
		if r.Name != "refs/heads/"+branch {
			continue
		}
		sha := strings.ToLower(r.ObjectID)
		if !isOID(sha) {
			return "", fmt.Errorf("hub channel: unexpected answer for branch %q: commit %q", branch, r.ObjectID)
		}
		return sha, nil
	}
	return "", fmt.Errorf("hub channel: branch %q not found", branch)
}

// self returns the id of the identity the token acts as (GET
// {org}/_apis/connectionData, authenticatedUser.id, as Microsoft's own API
// clients read it; the endpoint is not in the REST reference): the
// pipeline's build service.
func (ch *azureChannel) self(ctx context.Context) (string, error) {
	var data struct {
		AuthenticatedUser struct {
			ID string `json:"id"`
		} `json:"authenticatedUser"`
	}
	if err := ch.doVersion(ctx, azurePreviewVersion, http.MethodGet, ch.api+"/_apis/connectionData", nil, nil, &data); err != nil {
		return "", fmt.Errorf("hub channel: read the token's identity: %w", err)
	}
	id := strings.ToLower(strings.TrimSpace(data.AuthenticatedUser.ID))
	if id == "" || id == "00000000-0000-0000-0000-000000000000" {
		return "", errors.New("hub channel: the token's identity is anonymous: map the job access token into the step " +
			"(env: " + AzureTokenVar + ": $(System.AccessToken))")
	}
	return id, nil
}

// azureThreadClosed is the status of the thread touchmark creates: closed,
// so that a policy requiring comment resolution is not held by it.
const azureThreadClosed = "closed"

// UpsertComment keeps plan's comment in hub pull request pr (Commenter):
// it learns the token's identity (connectionData), lists the pull
// request's threads (GET …/pullRequests/{pr}/threads), and takes the first
// comment, not deleted, by that identity whose content holds marker; when
// its content differs it replaces it (PATCH
// …/threads/{thread}/comments/{comment}). Without one it creates a closed
// thread with the comment (POST …/threads), so that a policy requiring
// comment resolution does not wait on it. A comment of anyone else that
// holds the marker is never changed.
func (ch *azureChannel) UpsertComment(ctx context.Context, pr int64, marker, body string) (CommentResult, error) {
	if pr <= 0 {
		return "", fmt.Errorf("hub channel: no pull request to comment on (#%d)", pr)
	}
	if marker == "" || !strings.Contains(body, marker) {
		return "", errors.New("hub channel: the comment's body lacks its marker")
	}
	self, err := ch.self(ctx)
	if err != nil {
		return "", err
	}
	threads := ch.repo + "/pullRequests/" + strconv.FormatInt(pr, 10) + "/threads"
	var list struct {
		Value []struct {
			ID        int64 `json:"id"`
			IsDeleted bool  `json:"isDeleted"`
			Comments  []struct {
				ID          int64  `json:"id"`
				Content     string `json:"content"`
				IsDeleted   bool   `json:"isDeleted"`
				CommentType string `json:"commentType"`
				Author      struct {
					ID string `json:"id"`
				} `json:"author"`
			} `json:"comments"`
		} `json:"value"`
	}
	if err := ch.do(ctx, http.MethodGet, threads, nil, nil, &list); err != nil {
		return "", fmt.Errorf("hub channel: list the comment threads of #%d: %w", pr, err)
	}
	for _, t := range list.Value {
		if t.IsDeleted || t.ID <= 0 {
			continue
		}
		for _, c := range t.Comments {
			if c.IsDeleted || c.ID <= 0 || !strings.EqualFold(c.Author.ID, self) || !strings.Contains(c.Content, marker) ||
				(c.CommentType != "" && !strings.EqualFold(c.CommentType, "text")) {
				continue
			}
			if c.Content == body {
				return CommentUnchanged, nil
			}
			edit := threads + "/" + strconv.FormatInt(t.ID, 10) + "/comments/" + strconv.FormatInt(c.ID, 10)
			if err := ch.do(ctx, http.MethodPatch, edit, nil, map[string]string{"content": body}, nil); err != nil {
				return "", fmt.Errorf("hub channel: update comment %d of thread %d of #%d: %w", c.ID, t.ID, pr, err)
			}
			return CommentUpdated, nil
		}
	}
	thread := map[string]any{
		"comments": []map[string]any{{"parentCommentId": 0, "content": body, "commentType": "text"}},
		"status":   azureThreadClosed,
	}
	if err := ch.do(ctx, http.MethodPost, threads, nil, thread, nil); err != nil {
		return "", fmt.Errorf("hub channel: comment on #%d: %w", pr, err)
	}
	return CommentCreated, nil
}
