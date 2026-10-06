//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// forgeAPI is the forge's REST API as one account. The fixtures and the
// assertions use it directly, never the driver under test.
type forgeAPI struct {
	env *liveEnv
	as  account
}

func (e *liveEnv) api(as account) forgeAPI { return forgeAPI{env: e, as: as} }

// endpoint returns the URL of path: under /api/v1 unless it starts with
// /api/ itself.
func (a forgeAPI) endpoint(path string) string {
	if strings.HasPrefix(path, "/api/") {
		return a.env.URL + path
	}
	return a.env.URL + "/api/v1" + path
}

func (a forgeAPI) auth() *httpx.Auth {
	token := a.as.Token
	return &httpx.Auth{Hosts: []string{a.env.Host}, Header: func(context.Context) (string, error) {
		return "token " + token, nil
	}}
}

// do sends a request with a JSON body (nil for none) and returns the
// response whatever its status; a transport error fails the test.
func (a forgeAPI) do(t testing.TB, method, path string, in any) *httpx.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resp, err := a.env.HTTP.JSON(ctx, method, a.endpoint(path), a.auth(), in, nil)
	var se *httpx.StatusError
	if err != nil && !errors.As(err, &se) {
		t.Fatalf("%s %s as %s: %v", method, path, a.as.Login, err)
	}
	return resp
}

// ok sends a request, fails the test unless the answer is 2xx and decodes
// it into out (nil drops it).
func (a forgeAPI) ok(t testing.TB, method, path string, in, out any) {
	t.Helper()
	resp := a.do(t, method, path, in)
	if resp.Status/100 != 2 {
		t.Fatalf("%s %s as %s: HTTP %d: %s", method, path, a.as.Login, resp.Status, a.env.snippet(resp.Body))
	}
	decode(t, method+" "+path, resp, out)
}

// get is ok for a GET.
func (a forgeAPI) get(t testing.TB, path string, out any) {
	t.Helper()
	a.ok(t, http.MethodGet, path, nil, out)
}

// decode decodes a JSON answer into out, unless out is nil or the answer
// has no body.
func decode(t testing.TB, what string, resp *httpx.Response, out any) {
	t.Helper()
	if out == nil || resp.Status == http.StatusNoContent || len(resp.Body) == 0 {
		return
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		t.Fatalf("%s: decode the answer: %v", what, err)
	}
}

// snippet returns the start of a response body for messages, masked.
func (e *liveEnv) snippet(body []byte) string {
	s := string(body)
	if len(s) > 600 {
		s = s[:600] + "..."
	}
	return e.Redact.Replace(s)
}

// b64 is standard base64, as the contents API takes file content.
func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// The JSON the forges answer with, only the fields the tests read.

type apiUser struct {
	ID      int64  `json:"id"`
	Login   string `json:"login"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
}

type apiRepo struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	FullName         string   `json:"full_name"`
	Owner            apiUser  `json:"owner"`
	DefaultBranch    string   `json:"default_branch"`
	HTMLURL          string   `json:"html_url"`
	CloneURL         string   `json:"clone_url"`
	Private          bool     `json:"private"`
	Internal         bool     `json:"internal"`
	Empty            bool     `json:"empty"`
	Archived         bool     `json:"archived"`
	Fork             bool     `json:"fork"`
	Mirror           bool     `json:"mirror"`
	HasPullRequests  bool     `json:"has_pull_requests"`
	ObjectFormatName string   `json:"object_format_name"`
	Topics           []string `json:"topics"`
	Permissions      *struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
}

// platform returns the repository as package platform describes it, from
// the forge's own answer.
func (r apiRepo) platform(host string) platform.Repo {
	vis := "public"
	switch {
	case r.Private:
		vis = "private"
	case r.Internal:
		vis = "internal"
	}
	format := r.ObjectFormatName
	if format == "" {
		format = "sha1"
	}
	topics := r.Topics
	if topics == nil {
		topics = []string{}
	}
	return platform.Repo{
		Host:          host,
		ID:            strconv.FormatInt(r.ID, 10),
		Path:          r.FullName,
		DefaultBranch: r.DefaultBranch,
		WebURL:        r.HTMLURL,
		Visibility:    vis,
		ObjectFormat:  format,
		Archived:      r.Archived,
		Empty:         r.Empty,
		Mirror:        r.Mirror,
		Fork:          r.Fork,
		PRsDisabled:   !r.HasPullRequests,
		Topics:        topics,
	}
}

type apiLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiBranchInfo struct {
	Label  string   `json:"label"`
	Ref    string   `json:"ref"`
	SHA    string   `json:"sha"`
	RepoID int64    `json:"repo_id"`
	Repo   *apiRepo `json:"repo"`
}

type apiPR struct {
	Number    int64         `json:"number"`
	HTMLURL   string        `json:"html_url"`
	State     string        `json:"state"`
	Draft     bool          `json:"draft"`
	Merged    bool          `json:"merged"`
	Mergeable bool          `json:"mergeable"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	User      apiUser       `json:"user"`
	Labels    []apiLabel    `json:"labels"`
	Head      apiBranchInfo `json:"head"`
	Base      apiBranchInfo `json:"base"`
	Comments  int           `json:"comments"`
	Created   time.Time     `json:"created_at"`
	Updated   time.Time     `json:"updated_at"`
	Closed    *time.Time    `json:"closed_at"`
	MergedAt  *time.Time    `json:"merged_at"`
	MergedBy  *apiUser      `json:"merged_by"`
}

// platform returns the pull request as package platform describes it,
// from the forge's own answer. The head is the branch name (label), which
// Gitea keeps when the head branch is gone and ref becomes
// refs/pull/<n>/head (services/convert/pull.go). The closer is not in the
// answer: ClosedBy stays nil.
func (p apiPR) platform() platform.PR {
	state := platform.Open
	switch {
	case p.Merged:
		state = platform.Merged
	case p.State == "closed":
		state = platform.Closed
	}
	var labels []string
	for _, l := range p.Labels {
		labels = append(labels, l.Name)
	}
	pr := platform.PR{
		Number:     p.Number,
		URL:        p.HTMLURL,
		State:      state,
		Draft:      p.Draft,
		Head:       p.Head.Label,
		HeadSHA:    p.Head.SHA,
		Base:       p.Base.Ref,
		RepoID:     strconv.FormatInt(p.Base.RepoID, 10),
		HeadRepoID: strconv.FormatInt(p.Head.RepoID, 10),
		BaseExists: true,
		Title:      p.Title,
		Body:       p.Body,
		Labels:     labels,
		Author:     platform.Account{ID: strconv.FormatInt(p.User.ID, 10), Login: p.User.Login},
		CreatedAt:  p.Created,
	}
	if p.Closed != nil {
		pr.ClosedAt = *p.Closed
	}
	return pr
}

type apiComment struct {
	ID   int64   `json:"id"`
	Body string  `json:"body"`
	User apiUser `json:"user"`
}

// apiTimeline is one event of an issue's or pull request's timeline.
type apiTimeline struct {
	ID      int64     `json:"id"`
	Type    string    `json:"type"`
	Body    string    `json:"body"`
	User    *apiUser  `json:"user"`
	Created time.Time `json:"created_at"`
}

type apiBranch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

type apiCommit struct {
	SHA    string   `json:"sha"`
	Author *apiUser `json:"author"`
	// Committer is the platform account of the committer, when its email
	// belongs to one.
	Committer *apiUser `json:"committer"`
	Commit    struct {
		Author    apiPerson `json:"author"`
		Committer apiPerson `json:"committer"`
		Message   string    `json:"message"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

type apiPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type apiContent struct {
	Type     string `json:"type"`
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// text decodes the content of a file answer.
func (c apiContent) text(t testing.TB) string {
	t.Helper()
	if c.Encoding != "base64" {
		t.Fatalf("content encoding %q", c.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(c.Content)
	if err != nil {
		t.Fatalf("decode content: %v", err)
	}
	return string(data)
}
