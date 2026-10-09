package hubch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
)

// A hub on Bitbucket Cloud, run by Bitbucket Pipelines. Sources:
//   - the default variables:
//     https://support.atlassian.com/bitbucket-cloud/docs/variables-and-secrets/
//   - pull request pipelines (not run for pull requests from forks):
//     https://support.atlassian.com/bitbucket-cloud/docs/pipeline-start-conditions/
//   - the REST API: https://developer.atlassian.com/cloud/bitbucket/rest/intro/
//     (a repository by UUID: /2.0/repositories/{}/{uuid}).

// BitbucketTokenVar names the hub's own API token on Bitbucket Pipelines.
// Pipelines gives a step no token for the API, so the hub keeps one in a
// secured repository variable: an access token of the hub repository with
// Repositories: Read (the default branch and its tip) and, for plan
// --comment, Pull requests: Write. It is the counterpart of GITHUB_TOKEN and
// CI_JOB_TOKEN, and every pipeline of the hub sees it; it holds no write key
// of any target.
const BitbucketTokenVar = "TOUCHMARK_PIPELINES_TOKEN"

// The places of Bitbucket Cloud, the only home of Bitbucket Pipelines.
const (
	bitbucketServerURL = "https://bitbucket.org"
	bitbucketHost      = "bitbucket.org"
	bitbucketAPIURL    = "https://api.bitbucket.org/2.0"
)

// bitbucketUUIDRe is a UUID in canonical form: lowercase, without braces.
var bitbucketUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// BitbucketRepoID returns a Bitbucket repository UUID in the form the hub
// fingerprint carries it: lowercase, without the braces Bitbucket writes
// around it (BITBUCKET_REPO_UUID is "{0f1e…}"), so that the fingerprint is
// bitbucket.org/0f1e…. Anything that is not a UUID, with or without one
// pair of braces, gives "".
func BitbucketRepoID(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = s[1 : len(s)-1]
	}
	s = strings.ToLower(s)
	if !bitbucketUUIDRe.MatchString(s) {
		return ""
	}
	return s
}

// detectBitbucket reads Bitbucket Pipelines' default variables:
//   - the hub is on Bitbucket Cloud (ServerURL https://bitbucket.org, API
//     https://api.bitbucket.org/2.0); RepoID is BITBUCKET_REPO_UUID in
//     canonical form (BitbucketRepoID), RepoPath BITBUCKET_REPO_FULL_NAME;
//   - a pull request pipeline (BITBUCKET_PR_ID set, whatever its value) is
//     Event pull_request, its RefName the source branch BITBUCKET_BRANCH,
//     and no branch run; a branch pipeline (BITBUCKET_BRANCH) is Event push
//     and a branch run; a tag pipeline (BITBUCKET_TAG) is Event push, not a
//     branch run. Pipelines does not tell a push from a custom pipeline run
//     by hand or on a schedule: both are push runs of their branch;
//   - Environment is BITBUCKET_DEPLOYMENT_ENVIRONMENT, set in a step with a
//     deployment;
//   - Visibility is private or public from BITBUCKET_REPO_IS_PRIVATE;
//   - DefaultBranch stays empty: no variable names it, and the hub channel
//     reads it (RepositoryReader).
func detectBitbucket(env func(string) string) Context {
	c := Context{CI: BitbucketPipelines, ServerURL: bitbucketServerURL, Host: bitbucketHost, APIURL: bitbucketAPIURL}
	c.RepoID = BitbucketRepoID(env("BITBUCKET_REPO_UUID"))
	c.RepoPath = env("BITBUCKET_REPO_FULL_NAME")
	branch, tag := env("BITBUCKET_BRANCH"), env("BITBUCKET_TAG")
	switch {
	case env("BITBUCKET_PR_ID") != "":
		c.Event, c.RefName = "pull_request", branch
	case branch != "":
		c.Event, c.RefName, c.RefIsBranch = "push", branch, true
	case tag != "":
		c.Event, c.RefName = "push", tag
	}
	c.Environment = env("BITBUCKET_DEPLOYMENT_ENVIRONMENT")
	switch strings.ToLower(env("BITBUCKET_REPO_IS_PRIVATE")) {
	case "true":
		c.Visibility = "private"
	case "false":
		c.Visibility = "public"
	}
	c.RepositoryURL = cleanURL(env("BITBUCKET_GIT_HTTP_ORIGIN"))
	return c
}

// RepoInfo is what the hub channel reads of the hub repository itself.
type RepoInfo struct {
	// DefaultBranch is the repository's main branch.
	DefaultBranch string
	// Visibility is "public" or "private".
	Visibility string
}

// RepositoryReader is a Channel that can read the hub repository's default
// branch and visibility: the channel of Bitbucket, whose CI variables name
// no default branch.
type RepositoryReader interface {
	Repository(ctx context.Context) (RepoInfo, error)
}

// bitbucketChannel reads the hub repository through Bitbucket Cloud's REST
// API, and keeps plan's comment in a hub pull request.
type bitbucketChannel struct {
	client *httpx.Client
	auth   *httpx.Auth // nil without a token
	repo   string      // {api}/repositories/{workspace}/{slug}
	uuid   string      // canonical, "" when the CI named none
	branch string      // the default branch, "" until read
	info   *RepoInfo   // the repository, once read
}

// newBitbucket returns the channel of a hub on Bitbucket: the repository
// c.RepoPath (workspace/slug) under c.APIURL, with token, when set, sent as
// a Bearer token to the API's host only. When c names its default branch,
// Head reads that branch; else the repository's main branch.
func newBitbucket(c Context, client *httpx.Client, token string) (Channel, error) {
	if c.DefaultBranch != "" {
		if err := checkBranch(c.DefaultBranch); err != nil {
			return nil, fmt.Errorf("hub channel: default branch: %w", err)
		}
	}
	ws, slug, ok := strings.Cut(c.RepoPath, "/")
	if !ok || !validName(ws) || !validName(slug) {
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
	ch := &bitbucketChannel{
		client: client,
		repo:   strings.TrimRight(api.String(), "/") + "/repositories/" + url.PathEscape(ws) + "/" + url.PathEscape(slug),
		uuid:   c.RepoID,
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

// Repository reads the hub repository (GET /repositories/{workspace}/{slug}):
// its main branch and is_private. The answer must name the repository the
// CI named (its uuid), and a main branch.
func (ch *bitbucketChannel) Repository(ctx context.Context) (RepoInfo, error) {
	if ch.info != nil {
		return *ch.info, nil
	}
	var repo struct {
		UUID       string `json:"uuid"`
		IsPrivate  *bool  `json:"is_private"`
		MainBranch *struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	if _, err := ch.client.JSON(ctx, http.MethodGet, ch.repo, ch.auth, nil, &repo); err != nil {
		return RepoInfo{}, fmt.Errorf("hub channel: read the hub repository: %w", err)
	}
	if ch.uuid != "" && BitbucketRepoID(repo.UUID) != ch.uuid {
		return RepoInfo{}, fmt.Errorf("hub channel: the answer names repository %q, not {%s}", repo.UUID, ch.uuid)
	}
	if repo.MainBranch == nil || checkBranch(repo.MainBranch.Name) != nil {
		return RepoInfo{}, errors.New("hub channel: the hub repository's answer names no main branch")
	}
	if repo.IsPrivate == nil {
		return RepoInfo{}, errors.New("hub channel: the hub repository's answer does not say whether it is private")
	}
	info := RepoInfo{DefaultBranch: repo.MainBranch.Name, Visibility: "public"}
	if *repo.IsPrivate {
		info.Visibility = "private"
	}
	ch.info = &info
	return info, nil
}

// Visibility reads the hub repository's visibility (VisibilityReader).
func (ch *bitbucketChannel) Visibility(ctx context.Context) (string, error) {
	info, err := ch.Repository(ctx)
	return info.Visibility, err
}

// Head reads the tip of the default branch: GET
// /repositories/{workspace}/{slug}/refs/branches/{branch}, whose target.hash
// is the full commit id.
func (ch *bitbucketChannel) Head(ctx context.Context) (string, error) {
	branch := ch.branch
	if branch == "" {
		info, err := ch.Repository(ctx)
		if err != nil {
			return "", err
		}
		branch = info.DefaultBranch
	}
	var ref struct {
		Name   string `json:"name"`
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	if _, err := ch.client.JSON(ctx, http.MethodGet, ch.repo+"/refs/branches/"+escapeSegments(branch), ch.auth, nil, &ref); err != nil {
		return "", fmt.Errorf("hub channel: read the tip of branch %q: %w", branch, err)
	}
	sha := strings.ToLower(ref.Target.Hash)
	if ref.Name != branch || !isOID(sha) {
		return "", fmt.Errorf("hub channel: unexpected answer for branch %q: name %q, commit %q", branch, ref.Name, ref.Target.Hash)
	}
	return sha, nil
}

// bitbucketCommentPage is the page size of the comment listing, Bitbucket's
// largest pagelen.
const bitbucketCommentPage = 100

// UpsertComment keeps plan's comment in hub pull request pr (Commenter).
// It lists the pull request's comments (GET
// …/pullrequests/{pr}/comments, oldest first, 100 a page) and takes those
// that are neither deleted nor inline and whose raw content holds marker.
// An access token cannot learn its own account (GET /2.0/user refuses it),
// so the comment is not told by its author: each candidate, newest first, is
// replaced (PUT …/comments/{id}), and a refusal (HTTP 403 or 404: another
// account's comment, which only its author may edit) moves on to the next.
// Only an accepted edit proves the comment touchmark's, so a candidate whose
// content is already body is replaced all the same (the edit changes
// nothing) and counts as unchanged once Bitbucket accepts it: a comment of
// someone else's that copies the body exactly does not stand in for
// touchmark's. Without one that takes the edit, it creates the comment
// (POST …/comments). A comment of anyone else that holds the marker is thus
// never changed.
func (ch *bitbucketChannel) UpsertComment(ctx context.Context, pr int64, marker, body string) (CommentResult, error) {
	if pr <= 0 {
		return "", fmt.Errorf("hub channel: no pull request to comment on (#%d)", pr)
	}
	if marker == "" || !strings.Contains(body, marker) {
		return "", errors.New("hub channel: the comment's body lacks its marker")
	}
	comments := ch.repo + "/pullrequests/" + strconv.FormatInt(pr, 10) + "/comments"
	type candidate struct {
		id  int64
		raw string
	}
	var found []candidate
	for page := 1; page <= maxCommentPages; page++ {
		var list struct {
			Next   string `json:"next"`
			Values []struct {
				ID      int64 `json:"id"`
				Deleted bool  `json:"deleted"`
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
				Inline *struct{} `json:"inline"`
			} `json:"values"`
		}
		u := fmt.Sprintf("%s?pagelen=%d&page=%d", comments, bitbucketCommentPage, page)
		if _, err := ch.client.JSON(ctx, http.MethodGet, u, ch.auth, nil, &list); err != nil {
			return "", fmt.Errorf("hub channel: list the comments of #%d: %w", pr, err)
		}
		for _, c := range list.Values {
			if c.ID > 0 && !c.Deleted && c.Inline == nil && strings.Contains(c.Content.Raw, marker) {
				found = append(found, candidate{c.ID, c.Content.Raw})
			}
		}
		if list.Next == "" || len(list.Values) == 0 {
			break
		}
	}
	payload := map[string]any{"content": map[string]string{"raw": body}}
	for i := len(found) - 1; i >= 0; i-- {
		c := found[i]
		edit := comments + "/" + strconv.FormatInt(c.id, 10)
		_, err := ch.client.JSON(ctx, http.MethodPut, edit, ch.auth, payload, nil)
		if err == nil {
			if c.raw == body {
				return CommentUnchanged, nil
			}
			return CommentUpdated, nil
		}
		if s := status(err); s == http.StatusForbidden || s == http.StatusNotFound {
			continue
		}
		return "", fmt.Errorf("hub channel: update comment %d of #%d: %w", c.id, pr, err)
	}
	if _, err := ch.client.JSON(ctx, http.MethodPost, comments, ch.auth, payload, nil); err != nil {
		return "", fmt.Errorf("hub channel: comment on #%d: %w", pr, err)
	}
	return CommentCreated, nil
}
