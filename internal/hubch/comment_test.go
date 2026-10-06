package hubch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/httpx"
)

// commentServer serves the comments of one pull request of acme/hub the way
// GitHub (the API at /api/v3, pages of per_page) or Gitea and Forgejo (at
// /api/v1, pages of limit) do.
type commentServer struct {
	*httptest.Server
	mu       sync.Mutex
	comments []comment
	next     int64
	// asLogin is who the token comments as; refuse answers every write with
	// that status.
	asLogin string
	refuse  int
	// log is every request: method, path and query.
	log  []string
	auth []string
}

type comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func newCommentServer(t *testing.T, api, page, asLogin string) *commentServer {
	t.Helper()
	s := &commentServer{next: 100, asLogin: asLogin}
	list := api + "/repos/acme/hub/issues/41/comments"
	edit := api + "/repos/acme/hub/issues/comments/"
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.log = append(s.log, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		var in struct {
			Body string `json:"body"`
		}
		if r.Method != http.MethodGet {
			if s.refuse != 0 {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, s.refuse)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Body == "" {
				http.Error(w, `{"message":"no body"}`, http.StatusUnprocessableEntity)
				return
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == list:
			size, _ := strconv.Atoi(r.URL.Query().Get(page))
			n, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if size <= 0 || n <= 0 {
				http.Error(w, `{"message":"bad page"}`, http.StatusBadRequest)
				return
			}
			lo, hi := min((n-1)*size, len(s.comments)), min(n*size, len(s.comments))
			_ = json.NewEncoder(w).Encode(s.comments[lo:hi])
		case r.Method == http.MethodPost && r.URL.Path == list:
			c := comment{ID: s.next, Body: in.Body}
			c.User.Login = s.asLogin
			s.next++
			s.comments = append(s.comments, c)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(c)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, edit):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, edit), 10, 64)
			for i := range s.comments {
				if s.comments[i].ID == id {
					s.comments[i].Body = in.Body
					_ = json.NewEncoder(w).Encode(s.comments[i])
					return
				}
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// add adds a comment by login.
func (s *commentServer) add(login, body string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := comment{ID: s.next, Body: body}
	c.User.Login = login
	s.next++
	s.comments = append(s.comments, c)
	return c.ID
}

// bodies returns the comments as "id login: body".
func (s *commentServer) bodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.comments {
		out = append(out, fmt.Sprintf("%d %s: %s", c.ID, c.User.Login, c.Body))
	}
	return out
}

// lastID is the id of the newest comment.
func (s *commentServer) lastID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next - 1
}

// takeLog returns the requests since the last call.
func (s *commentServer) takeLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.log
	s.log = nil
	return out
}

// UpsertComment keeps one comment: it creates it, edits it when the body
// changes, leaves it when it does not, and never touches a comment of
// someone else that holds the marker, nor its own comments without it;
// the comment it keeps may be past the first page.
func TestUpsertComment(t *testing.T) {
	t.Parallel()
	const marker = "<!-- touchmark plan: acme-eng -->"
	cases := []struct {
		ci          CI
		api, page   string
		login, auth string
	}{
		{GitHubActions, "/api/v3", "per_page", "github-actions[bot]", "Bearer hub-token"},
		{GiteaActions, "/api/v1", "limit", "gitea-actions", "token hub-token"},
		{ForgejoActions, "/api/v1", "limit", "forgejo-actions", "token hub-token"},
	}
	for _, tc := range cases {
		t.Run(string(tc.ci), func(t *testing.T) {
			t.Parallel()
			srv := newCommentServer(t, tc.api, tc.page, tc.login)
			alice := srv.add("alice", "a copy of the marker: "+marker)
			srv.add(tc.login, "another comment of the CI")
			for range 120 {
				srv.add("bob", "busy thread")
			}
			ch, err := New(Context{CI: tc.ci, APIURL: srv.URL + tc.api, RepoPath: "acme/hub", DefaultBranch: "main"}, httpx.New(httpx.Options{}), "hub-token")
			if err != nil {
				t.Fatal(err)
			}
			c, ok := ch.(Commenter)
			if !ok {
				t.Fatalf("the %s channel is no Commenter", tc.ci)
			}
			res, err := c.UpsertComment(t.Context(), 41, marker, "plan v1\n"+marker)
			if err != nil || res != CommentCreated {
				t.Fatalf("first: %s, %v", res, err)
			}
			log := srv.takeLog()
			if last := log[len(log)-1]; !strings.HasPrefix(last, "POST "+tc.api+"/repos/acme/hub/issues/41/comments") {
				t.Errorf("first: requests %q", log)
			}
			id := srv.lastID()

			res, err = c.UpsertComment(t.Context(), 41, marker, "plan v2\n"+marker)
			if err != nil || res != CommentUpdated {
				t.Fatalf("second: %s, %v", res, err)
			}
			log = srv.takeLog()
			if last := log[len(log)-1]; last != fmt.Sprintf("PATCH %s/repos/acme/hub/issues/comments/%d?", tc.api, id) {
				t.Errorf("second: requests %q", log)
			}
			res, err = c.UpsertComment(t.Context(), 41, marker, "plan v2\n"+marker)
			if err != nil || res != CommentUnchanged {
				t.Fatalf("third: %s, %v", res, err)
			}
			for _, r := range srv.takeLog() {
				if !strings.HasPrefix(r, "GET ") {
					t.Errorf("an unchanged comment was written: %s", r)
				}
			}
			bodies := srv.bodies()
			if got := bodies[0]; got != fmt.Sprintf("%d alice: a copy of the marker: %s", alice, marker) {
				t.Errorf("someone else's comment changed: %s", got)
			}
			if got := bodies[len(bodies)-1]; got != fmt.Sprintf("%d %s: plan v2\n%s", id, tc.login, marker) {
				t.Errorf("the comment: %s", got)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			for _, a := range srv.auth {
				if a != tc.auth {
					t.Errorf("Authorization %q", a)
				}
			}
		})
	}
}

// A refused write is an error that names the pull request; a body without
// its marker, no pull request, and a CI without a known comment author are
// refused before any request.
func TestUpsertCommentErrors(t *testing.T) {
	t.Parallel()
	const marker = "<!-- touchmark plan: acme-eng -->"
	srv := newCommentServer(t, "/api/v3", "per_page", "github-actions[bot]")
	srv.mu.Lock()
	srv.refuse = http.StatusForbidden
	srv.mu.Unlock()
	ch, err := New(Context{CI: GitHubActions, APIURL: srv.URL + "/api/v3", RepoPath: "acme/hub", DefaultBranch: "main"}, nil, "hub-token")
	if err != nil {
		t.Fatal(err)
	}
	c := ch.(Commenter)
	if _, err := c.UpsertComment(t.Context(), 41, marker, "plan\n"+marker); err == nil || !strings.Contains(err.Error(), "comment on #41") ||
		!strings.Contains(err.Error(), "403") {
		t.Errorf("refused: %v", err)
	}
	srv.takeLog()
	for name, call := range map[string]func() error{
		"no marker in the body": func() error { _, err := c.UpsertComment(t.Context(), 41, marker, "plan"); return err },
		"empty marker":          func() error { _, err := c.UpsertComment(t.Context(), 41, "", "plan"); return err },
		"no pull request":       func() error { _, err := c.UpsertComment(t.Context(), 0, marker, "plan\n"+marker); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if log := srv.takeLog(); len(log) > 0 {
		t.Errorf("requests %q", log)
	}
	// GitLab's channel has none: CI_JOB_TOKEN cannot write notes.
	gl, err := New(Context{CI: GitLabCI, RepositoryURL: "https://gitlab.example.com/acme/hub.git", DefaultBranch: "main"}, nil, "job-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gl.(Commenter); ok {
		t.Error("the GitLab channel comments")
	}
}
