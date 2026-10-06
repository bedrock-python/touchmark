//go:build e2e

package gitlabe2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// glAPI is GitLab's REST API as one account. The fixtures and the
// assertions use it directly, never the driver under test.
type glAPI struct {
	env *liveEnv
	as  account
}

func (e *liveEnv) api(as account) glAPI { return glAPI{env: e, as: as} }

// authOf sends the token of a as a Bearer token, which GitLab takes for
// personal, group and project access tokens as it takes PRIVATE-TOKEN
// (lib/gitlab/auth/auth_finders.rb).
func (e *liveEnv) authOf(a account) *httpx.Auth {
	token := a.Token
	return &httpx.Auth{Hosts: []string{e.Host}, Header: func(context.Context) (string, error) {
		return "Bearer " + token, nil
	}}
}

// endpoint returns the URL of path: under /api/v4 unless it starts with
// "/api/" or "/-/" itself.
func (a glAPI) endpoint(path string) string {
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/-/") {
		return a.env.URL + path
	}
	return a.env.URL + "/api/v4" + path
}

// do sends a request with a JSON body (nil for none) and returns the
// response whatever its status; a transport error fails the test.
func (a glAPI) do(t testing.TB, method, path string, in any) *httpx.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := a.env.HTTP.JSON(ctx, method, a.endpoint(path), a.env.authOf(a.as), in, nil)
	var se *httpx.StatusError
	if err != nil && !errors.As(err, &se) {
		t.Fatalf("%s %s as %s: %v", method, path, a.as.Login, err)
	}
	return resp
}

// ok sends a request, fails the test unless the answer is 2xx and decodes
// it into out (nil drops it).
func (a glAPI) ok(t testing.TB, method, path string, in, out any) {
	t.Helper()
	resp := a.do(t, method, path, in)
	if resp.Status/100 != 2 {
		t.Fatalf("%s %s as %s: HTTP %d: %s", method, path, a.as.Login, resp.Status, a.env.snippet(resp.Body))
	}
	decode(t, method+" "+path, resp, out)
}

// get is ok for a GET.
func (a glAPI) get(t testing.TB, path string, out any) {
	t.Helper()
	a.ok(t, http.MethodGet, path, nil, out)
}

// all reads every page of a listing as a: per_page 100, until a short
// page.
func all[T any](t testing.TB, a glAPI, path string) []T {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var out []T
	for page := 1; page <= 100; page++ {
		var items []T
		a.get(t, fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), &items)
		out = append(out, items...)
		if len(items) < 100 {
			return out
		}
	}
	t.Fatalf("GET %s: more than 100 pages", path)
	return nil
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

// pid is a project path or id as the API takes it in a URL: the id, or the
// URL-encoded full path.
func pid(p string) string {
	if _, err := strconv.ParseInt(p, 10, 64); err == nil {
		return p
	}
	return url.PathEscape(p)
}

// The JSON GitLab answers with, only the fields the tests read.

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Email    string `json:"email"`
	Bot      bool   `json:"bot"`
	IsAdmin  bool   `json:"is_admin"`
}

type apiNamespace struct {
	ID       int64  `json:"id"`
	FullPath string `json:"full_path"`
	Kind     string `json:"kind"`
}

type apiProject struct {
	ID                        int64        `json:"id"`
	Name                      string       `json:"name"`
	Path                      string       `json:"path"`
	PathWithNamespace         string       `json:"path_with_namespace"`
	DefaultBranch             string       `json:"default_branch"`
	WebURL                    string       `json:"web_url"`
	HTTPURLToRepo             string       `json:"http_url_to_repo"`
	Visibility                string       `json:"visibility"`
	Archived                  bool         `json:"archived"`
	EmptyRepo                 bool         `json:"empty_repo"`
	Mirror                    bool         `json:"mirror"`
	ForkedFrom                *apiProject  `json:"forked_from_project"`
	ImportStatus              string       `json:"import_status"`
	MarkedForDeletionOn       *string      `json:"marked_for_deletion_on"`
	MergeRequestsAccessLevel  string       `json:"merge_requests_access_level"`
	RepositoryAccessLevel     string       `json:"repository_access_level"`
	Topics                    []string     `json:"topics"`
	RepositoryObjectFormat    string       `json:"repository_object_format"`
	Namespace                 apiNamespace `json:"namespace"`
	AutocloseReferencedIssues bool         `json:"autoclose_referenced_issues"`
}

// platform returns the project as package platform describes it, from
// GitLab's own answer.
func (p apiProject) platform(host string) platform.Repo {
	format := p.RepositoryObjectFormat
	if format == "" {
		format = "sha1"
	}
	topics := p.Topics
	if topics == nil {
		topics = []string{}
	}
	return platform.Repo{
		Host:          host,
		ID:            strconv.FormatInt(p.ID, 10),
		Path:          p.PathWithNamespace,
		DefaultBranch: p.DefaultBranch,
		WebURL:        p.WebURL,
		Visibility:    p.Visibility,
		ObjectFormat:  format,
		Archived:      p.Archived,
		Empty:         p.EmptyRepo,
		Mirror:        p.Mirror,
		Fork:          p.ForkedFrom != nil,
		PendingDelete: p.MarkedForDeletionOn != nil,
		PRsDisabled:   p.MergeRequestsAccessLevel == "disabled",
		Topics:        topics,
	}
}

type apiMR struct {
	ID              int64      `json:"id"`
	IID             int64      `json:"iid"`
	ProjectID       int64      `json:"project_id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	State           string     `json:"state"`
	Draft           bool       `json:"draft"`
	SourceBranch    string     `json:"source_branch"`
	TargetBranch    string     `json:"target_branch"`
	SourceProjectID int64      `json:"source_project_id"`
	TargetProjectID int64      `json:"target_project_id"`
	Author          apiUser    `json:"author"`
	Labels          []string   `json:"labels"`
	SHA             string     `json:"sha"`
	WebURL          string     `json:"web_url"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ClosedAt        *time.Time `json:"closed_at"`
	ClosedBy        *apiUser   `json:"closed_by"`
	MergedBy        *apiUser   `json:"merged_by"`
	MergeUser       *apiUser   `json:"merge_user"`
	MergedAt        *time.Time `json:"merged_at"`
	UserNotesCount  int        `json:"user_notes_count"`
	MergeStatus     string     `json:"merge_status"`
	DetailedStatus  string     `json:"detailed_merge_status"`
}

// platform returns the merge request as package platform describes it,
// from GitLab's own answer.
func (m apiMR) platform() platform.PR {
	state := platform.Open
	switch m.State {
	case "merged":
		state = platform.Merged
	case "closed":
		state = platform.Closed
	}
	pr := platform.PR{
		Number:     m.IID,
		URL:        m.WebURL,
		State:      state,
		Draft:      m.Draft,
		Head:       m.SourceBranch,
		HeadSHA:    m.SHA,
		Base:       m.TargetBranch,
		RepoID:     strconv.FormatInt(m.TargetProjectID, 10),
		HeadRepoID: strconv.FormatInt(m.SourceProjectID, 10),
		BaseExists: true,
		Title:      m.Title,
		Body:       m.Description,
		Labels:     m.Labels,
		Author:     platform.Account{ID: strconv.FormatInt(m.Author.ID, 10), Login: m.Author.Username},
		CreatedAt:  m.CreatedAt,
	}
	if m.ClosedAt != nil {
		pr.ClosedAt = *m.ClosedAt
	}
	return pr
}

type apiNote struct {
	ID     int64   `json:"id"`
	Body   string  `json:"body"`
	System bool    `json:"system"`
	Author apiUser `json:"author"`
}

type apiBranch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Commit    struct {
		ID string `json:"id"`
	} `json:"commit"`
}

type apiCommit struct {
	ID             string   `json:"id"`
	ParentIDs      []string `json:"parent_ids"`
	Message        string   `json:"message"`
	AuthorName     string   `json:"author_name"`
	AuthorEmail    string   `json:"author_email"`
	CommitterName  string   `json:"committer_name"`
	CommitterEmail string   `json:"committer_email"`
}

type apiLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type apiMember struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	AccessLevel int    `json:"access_level"`
}

// Access levels.
const (
	levelReporter   = 20
	levelDeveloper  = 30
	levelMaintainer = 40
	levelOwner      = 50
)
