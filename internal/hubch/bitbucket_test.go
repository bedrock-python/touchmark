package hubch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/httpx"
)

// bbUUID is the hub repository's UUID as Bitbucket writes it, and bbID as
// the fingerprint carries it.
const (
	bbUUID = "{3F2A8D4E-1B6C-4F0A-9E7D-5C2B1A0F9E8D}"
	bbID   = "3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d"
	bbTok  = "bb-hub-access-token-0123456789"
)

// pipelinesEnv is a step of Bitbucket Pipelines (the default variables of
// https://support.atlassian.com/bitbucket-cloud/docs/variables-and-secrets/),
// with extra variables over it.
func pipelinesEnv(extra map[string]string) func(string) string {
	vars := map[string]string{
		"CI":                        "true",
		"BITBUCKET_BUILD_NUMBER":    "17",
		"BITBUCKET_REPO_UUID":       bbUUID,
		"BITBUCKET_REPO_FULL_NAME":  "acme/engineering-assets",
		"BITBUCKET_WORKSPACE":       "acme",
		"BITBUCKET_REPO_SLUG":       "engineering-assets",
		"BITBUCKET_COMMIT":          "3f2c0000000000000000000000000000000000aa",
		"BITBUCKET_GIT_HTTP_ORIGIN": "http://bitbucket.org/acme/engineering-assets",
		"BITBUCKET_REPO_IS_PRIVATE": "true",
	}
	for k, v := range extra {
		if v == "" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	return envOf(vars)
}

func TestDetectBitbucket(t *testing.T) {
	base := Context{CI: BitbucketPipelines, ServerURL: "https://bitbucket.org", Host: "bitbucket.org", APIURL: "https://api.bitbucket.org/2.0",
		RepoID: bbID, RepoPath: "acme/engineering-assets", Visibility: "private", RepositoryURL: "http://bitbucket.org/acme/engineering-assets"}
	with := func(f func(*Context)) Context {
		c := base
		f(&c)
		return c
	}
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  Context
	}{
		{"push to a branch", map[string]string{"BITBUCKET_BRANCH": "main"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch = "push", "main", true })},
		{"the deployment step", map[string]string{"BITBUCKET_BRANCH": "main", "BITBUCKET_DEPLOYMENT_ENVIRONMENT": "touchmark-distribute"},
			with(func(c *Context) {
				c.Event, c.RefName, c.RefIsBranch, c.Environment = "push", "main", true, "touchmark-distribute"
			})},
		{"a pull request", map[string]string{"BITBUCKET_BRANCH": "feature", "BITBUCKET_PR_ID": "41", "BITBUCKET_PR_DESTINATION_BRANCH": "main"},
			with(func(c *Context) { c.Event, c.RefName = "pull_request", "feature" })},
		// A pull request's pipeline is never a branch run, even of the
		// default branch, whatever its id says.
		{"a pull request from main", map[string]string{"BITBUCKET_BRANCH": "main", "BITBUCKET_PR_ID": "x"},
			with(func(c *Context) { c.Event, c.RefName = "pull_request", "main" })},
		{"a tag", map[string]string{"BITBUCKET_TAG": "v1"},
			with(func(c *Context) { c.Event, c.RefName = "push", "v1" })},
		{"a public hub", map[string]string{"BITBUCKET_BRANCH": "main", "BITBUCKET_REPO_IS_PRIVATE": "false"},
			with(func(c *Context) { c.Event, c.RefName, c.RefIsBranch, c.Visibility = "push", "main", true, "public" })},
		{"no visibility, a garbled UUID", map[string]string{"BITBUCKET_BRANCH": "main", "BITBUCKET_REPO_IS_PRIVATE": "", "BITBUCKET_REPO_UUID": "{3f2a}"},
			with(func(c *Context) {
				c.Event, c.RefName, c.RefIsBranch, c.Visibility, c.RepoID = "push", "main", true, "", ""
			})},
	} {
		got := Detect(pipelinesEnv(tc.extra), nil)
		if got != tc.want {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.name, got, tc.want)
		}
	}
	c := Detect(pipelinesEnv(map[string]string{"BITBUCKET_BRANCH": "main"}), nil)
	if fp := c.Fingerprint(); fp != "bitbucket.org/"+bbID {
		t.Errorf("fingerprint %q", fp)
	}
	// GitLab's and Actions' flags win: a Bitbucket variable in their jobs
	// does not make them Bitbucket.
	if c := Detect(envOf(map[string]string{"GITLAB_CI": "true", "BITBUCKET_BUILD_NUMBER": "1"}), nil); c.CI != GitLabCI {
		t.Errorf("GitLab with a stray variable: %s", c.CI)
	}
	if tok := Token(c, pipelinesEnv(map[string]string{BitbucketTokenVar: " " + bbTok + " ", "GITHUB_TOKEN": "x"})); tok != bbTok {
		t.Errorf("token %q", tok)
	}
}

func TestBitbucketRepoID(t *testing.T) {
	for in, want := range map[string]string{
		bbUUID:               bbID,
		bbID:                 bbID,
		" " + bbUUID + " ":   bbID,
		"{" + bbID:           "",
		"{{" + bbID + "}}":   "",
		"712345678":          "",
		"":                   "",
		"3f2a8d4e1b6c4f0a9e": "",
	} {
		if got := BitbucketRepoID(in); got != want {
			t.Errorf("BitbucketRepoID(%q) = %q, want %q", in, got, want)
		}
	}
}

// bbServer is Bitbucket Cloud's API for one hub, acme/engineering-assets,
// with its pull request 41 and the comments on it, as
// https://api.bitbucket.org/swagger.json describes them. The hub's access
// token comments as account "{bot}"; a comment of another account refuses
// its edit with 403.
type bbServer struct {
	*httptest.Server
	mu       sync.Mutex
	private  bool
	comments []bbComment
	next     int64
	log      []string
	auth     []string
}

type bbComment struct {
	ID      int64 `json:"id"`
	Deleted bool  `json:"deleted"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	Inline *struct {
		Path string `json:"path"`
	} `json:"inline,omitempty"`
	User struct {
		UUID string `json:"uuid"`
	} `json:"user"`
}

func newBBServer(t *testing.T) *bbServer {
	t.Helper()
	s := &bbServer{next: 500, private: true}
	const repo = "/2.0/repositories/acme/engineering-assets"
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.log = append(s.log, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		authed := r.Header.Get("Authorization") == "Bearer "+bbTok
		if !authed && (s.private || r.Method != http.MethodGet) {
			// Bitbucket answers a private repository's anonymous reader as
			// if it did not exist.
			http.Error(w, `{"type": "error", "error": {"message": "Repository not found"}}`, http.StatusNotFound)
			return
		}
		var in struct {
			Content struct {
				Raw string `json:"raw"`
			} `json:"content"`
		}
		if r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Content.Raw == "" {
				http.Error(w, `{"type": "error"}`, http.StatusBadRequest)
				return
			}
		}
		comments := repo + "/pullrequests/41/comments"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo:
			fmt.Fprintf(w, `{"type": "repository", "uuid": %q, "full_name": "acme/engineering-assets", "is_private": %v, "mainbranch": {"type": "branch", "name": "trunk"}}`, bbUUID, s.private)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/refs/branches/trunk":
			fmt.Fprint(w, `{"type": "branch", "name": "trunk", "target": {"type": "commit", "hash": "0123456789ABCDEF0123456789abcdef01234567"}}`)
		case r.Method == http.MethodGet && r.URL.Path == comments:
			size, _ := strconv.Atoi(r.URL.Query().Get("pagelen"))
			n, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if size <= 0 || size > 100 || n <= 0 {
				http.Error(w, `{"type": "error"}`, http.StatusBadRequest)
				return
			}
			lo, hi := min((n-1)*size, len(s.comments)), min(n*size, len(s.comments))
			page := map[string]any{"pagelen": size, "page": n, "values": s.comments[lo:hi]}
			if hi < len(s.comments) {
				page["next"] = fmt.Sprintf("https://api.bitbucket.org%s?pagelen=%d&page=%d", comments, size, n+1)
			}
			_ = json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodPost && r.URL.Path == comments:
			c := bbComment{ID: s.next}
			c.Content.Raw, c.User.UUID = in.Content.Raw, "{bot}"
			s.next++
			s.comments = append(s.comments, c)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(c)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, comments+"/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, comments+"/"), 10, 64)
			for i := range s.comments {
				if s.comments[i].ID != id {
					continue
				}
				if s.comments[i].User.UUID != "{bot}" {
					http.Error(w, `{"type": "error", "error": {"message": "You are not allowed to edit this comment"}}`, http.StatusForbidden)
					return
				}
				s.comments[i].Content.Raw = in.Content.Raw
				_ = json.NewEncoder(w).Encode(s.comments[i])
				return
			}
			http.Error(w, `{"type": "error"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"type": "error"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *bbServer) add(author, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := bbComment{ID: s.next}
	c.Content.Raw, c.User.UUID = raw, author
	s.next++
	s.comments = append(s.comments, c)
}

func (s *bbServer) raws() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.comments {
		out = append(out, c.User.UUID+": "+c.Content.Raw)
	}
	return out
}

func (s *bbServer) takeLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.log
	s.log = nil
	return log
}

func (s *bbServer) channel(t *testing.T, token string) Channel {
	t.Helper()
	c := Detect(pipelinesEnv(map[string]string{"BITBUCKET_PR_ID": "41", "BITBUCKET_BRANCH": "feature"}), nil)
	c.APIURL = s.URL + "/2.0"
	ch, err := New(c, httpx.New(httpx.Options{}), token)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestBitbucketChannel(t *testing.T) {
	s := newBBServer(t)
	ch := s.channel(t, bbTok)
	info, err := ch.(RepositoryReader).Repository(t.Context())
	if err != nil || info != (RepoInfo{DefaultBranch: "trunk", Visibility: "private"}) {
		t.Fatalf("repository %+v, %v", info, err)
	}
	head, err := ch.Head(t.Context())
	if err != nil || head != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("head %q, %v", head, err)
	}
	if v, err := ch.(VisibilityReader).Visibility(t.Context()); err != nil || v != "private" {
		t.Errorf("visibility %q, %v", v, err)
	}
	// The repository is read once; the token went with every request.
	if log := s.takeLog(); !slices.Equal(log, []string{
		"GET /2.0/repositories/acme/engineering-assets?",
		"GET /2.0/repositories/acme/engineering-assets/refs/branches/trunk?",
	}) {
		t.Errorf("requests %q", log)
	}
	for _, a := range s.auth {
		if a != "Bearer "+bbTok {
			t.Errorf("authorization %q", a)
		}
	}
	// Without a token a private hub cannot be read; a public one can.
	if _, err := s.channel(t, "").(RepositoryReader).Repository(t.Context()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("anonymous read of a private hub: %v", err)
	}
	s.private = false
	if info, err := s.channel(t, "").(RepositoryReader).Repository(t.Context()); err != nil || info.Visibility != "public" {
		t.Errorf("anonymous read of a public hub: %+v, %v", info, err)
	}
	// An answer for another repository is refused.
	c := Detect(pipelinesEnv(map[string]string{"BITBUCKET_BRANCH": "main", "BITBUCKET_REPO_UUID": "{0b7e5a2c-9d4f-4e1b-8a3c-6f5d2e1c0b9a}"}), nil)
	c.APIURL = s.URL + "/2.0"
	other, err := New(c, nil, bbTok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Head(t.Context()); err == nil || !strings.Contains(err.Error(), "names repository") {
		t.Errorf("another repository's answer: %v", err)
	}
}

func TestBitbucketChannelErrors(t *testing.T) {
	c := Detect(pipelinesEnv(map[string]string{"BITBUCKET_BRANCH": "main"}), nil)
	for name, mutate := range map[string]func(*Context){
		"no path":      func(c *Context) { c.RepoPath = "" },
		"bad path":     func(c *Context) { c.RepoPath = "acme/../x/y" },
		"bad API":      func(c *Context) { c.APIURL = "ftp://api.bitbucket.org" },
		"bad branch":   func(c *Context) { c.DefaultBranch = "-x" },
		"API with key": func(c *Context) { c.APIURL = "https://u:p@api.bitbucket.org/2.0" },
	} {
		cc := c
		mutate(&cc)
		if _, err := New(cc, nil, bbTok); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := New(c, nil, "a token"); err == nil || strings.Contains(err.Error(), "a token") {
		t.Errorf("a token with a space: %v", err)
	}
}

func TestBitbucketUpsertComment(t *testing.T) {
	const marker = `[touchmark-plan]: # "touchmark plan: acme-eng"`
	body := func(s string) string { return s + "\n\n" + marker + "\n" }
	s := newBBServer(t)
	ch := s.channel(t, bbTok).(Commenter)

	// Someone else's comments, one quoting the marker, an inline one and a
	// deleted one with it, come first.
	s.add("{alice}", "LGTM")
	s.add("{mallory}", body("planted"))
	s.add("{bot}", body("inline"))
	s.comments[len(s.comments)-1].Inline = &struct {
		Path string `json:"path"`
	}{"hub.yml"}
	s.add("{bot}", body("deleted"))
	s.comments[len(s.comments)-1].Deleted = true
	s.takeLog()

	// None of them is plan's own: mallory's refuses the edit, so a new one
	// is made.
	res, err := ch.UpsertComment(t.Context(), 41, marker, body("first"))
	if err != nil || res != CommentCreated {
		t.Fatalf("first: %s, %v", res, err)
	}
	if log := s.takeLog(); !slices.Equal(log, []string{
		"GET /2.0/repositories/acme/engineering-assets/pullrequests/41/comments?pagelen=100&page=1",
		"PUT /2.0/repositories/acme/engineering-assets/pullrequests/41/comments/501?",
		"POST /2.0/repositories/acme/engineering-assets/pullrequests/41/comments?",
	}) {
		t.Errorf("requests %q", log)
	}
	// The next run edits its own comment, newest first.
	if res, err := ch.UpsertComment(t.Context(), 41, marker, body("second")); err != nil || res != CommentUpdated {
		t.Errorf("second: %s, %v", res, err)
	}
	s.takeLog()
	if res, err := ch.UpsertComment(t.Context(), 41, marker, body("second")); err != nil || res != CommentUnchanged {
		t.Errorf("same: %s, %v", res, err)
	}
	// Unchanged only once the edit proves the comment its own.
	if log := s.takeLog(); len(log) != 2 || !strings.HasPrefix(log[1], "PUT ") || !strings.HasSuffix(log[1], "/comments/504?") {
		t.Errorf("requests %q", log)
	}
	want := []string{"{alice}: LGTM", "{mallory}: " + body("planted"), "{bot}: " + body("inline"), "{bot}: " + body("deleted"), "{bot}: " + body("second")}
	if got := s.raws(); !slices.Equal(got, want) {
		t.Errorf("comments\n got  %q\n want %q", got, want)
	}
	// Past a page of 100 comments it still finds its own.
	for i := range 150 {
		s.add("{alice}", fmt.Sprintf("note %d", i))
	}
	s.takeLog()
	if res, err := ch.UpsertComment(t.Context(), 41, marker, body("third")); err != nil || res != CommentUpdated {
		t.Errorf("third: %s, %v", res, err)
	}
	if log := s.takeLog(); len(log) != 3 || !strings.HasSuffix(log[1], "page=2") || !strings.HasPrefix(log[2], "PUT ") {
		t.Errorf("requests %q", log)
	}
	// Errors: no marker, no pull request, a refused token.
	if _, err := ch.UpsertComment(t.Context(), 41, marker, "no marker"); err == nil {
		t.Error("a body without its marker")
	}
	if _, err := ch.UpsertComment(t.Context(), 0, marker, body("x")); err == nil {
		t.Error("no pull request")
	}
	if _, err := s.channel(t, "").(Commenter).UpsertComment(t.Context(), 41, marker, body("x")); err == nil {
		t.Error("an anonymous comment")
	}
}

// Someone else's comment that holds exactly the body plan would write is
// not taken for plan's own: equal content proves nothing until an edit is
// accepted, so plan tries it, is refused, and comments itself.
func TestBitbucketUpsertCommentCopiedBody(t *testing.T) {
	const marker = `[touchmark-plan]: # "touchmark plan: acme-eng"`
	body := "the plan\n\n" + marker + "\n"
	s := newBBServer(t)
	ch := s.channel(t, bbTok).(Commenter)
	s.add("{mallory}", body)
	s.takeLog()

	res, err := ch.UpsertComment(t.Context(), 41, marker, body)
	if err != nil || res != CommentCreated {
		t.Fatalf("first: %s, %v", res, err)
	}
	if log := s.takeLog(); !slices.Equal(log, []string{
		"GET /2.0/repositories/acme/engineering-assets/pullrequests/41/comments?pagelen=100&page=1",
		"PUT /2.0/repositories/acme/engineering-assets/pullrequests/41/comments/500?",
		"POST /2.0/repositories/acme/engineering-assets/pullrequests/41/comments?",
	}) {
		t.Errorf("requests %q", log)
	}
	if got, want := s.raws(), []string{"{mallory}: " + body, "{bot}: " + body}; !slices.Equal(got, want) {
		t.Errorf("comments\n got  %q\n want %q", got, want)
	}
	// The next run with the same body finds its own: unchanged.
	if res, err := ch.UpsertComment(t.Context(), 41, marker, body); err != nil || res != CommentUnchanged {
		t.Errorf("same: %s, %v", res, err)
	}
	if got := s.raws(); len(got) != 2 {
		t.Errorf("comments %q", got)
	}
}

func TestReadKeyStoreBitbucket(t *testing.T) {
	const repo = "/2.0/repositories/acme/engineering-assets"
	const env = "{1c2d3e4f-0000-4000-8000-000000000001}"
	const pages = "{1c2d3e4f-0000-4000-8000-000000000002}"
	s := newKeyServer(t, "Authorization", "Bearer "+maintainerToken, map[string]string{
		"/2.0/repositories/{}/{" + bbID + "}": `{"uuid": "` + bbUUID + `", "full_name": "acme/engineering-assets", "is_private": true, "mainbranch": {"name": "main"}}`,
		repo + "/pipelines_config/variables": `{"pagelen": 100, "values": [{"key": "TOUCHMARK_READ_TOKEN", "secured": true, "value": ""},
			{"key": "TOUCHMARK_WRITE_TOKEN", "secured": false, "value": "leaked-value"}]}`,
		"/2.0/workspaces/acme/pipelines-config/variables": "status 403",
		repo + "/environments": `{"pagelen": 100, "values": [{"uuid": "` + env + `", "name": "touchmark-distribute", "restrictions": {"admin_only": true}},
			{"uuid": "` + pages + `", "name": "pages"}]}`,
		repo + "/deployments_config/environments/" + env + "/variables":   `{"values": [{"key": "TOUCHMARK_WRITE_TOKEN", "secured": true}]}`,
		repo + "/deployments_config/environments/" + pages + "/variables": `{"values": []}`,
	})
	ks, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "bitbucket", APIURL: s.URL + "/2.0", RepoID: bbUUID, Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	if ks.RepoPath != "acme/engineering-assets" || ks.DefaultBranch != "main" || ks.Visibility != "private" {
		t.Errorf("store %+v", ks)
	}
	want := []string{"TOUCHMARK_READ_TOKEN repository", "TOUCHMARK_WRITE_TOKEN environment:touchmark-distribute", "TOUCHMARK_WRITE_TOKEN repository"}
	if got := secretNames(ks); !slices.Equal(got, want) {
		t.Errorf("secrets %q, want %q", got, want)
	}
	for _, sec := range ks.Secrets {
		if sec.Masked != (sec.Name == "TOUCHMARK_READ_TOKEN" || sec.Where == "environment") {
			t.Errorf("secret %+v: secured is %v", sec, sec.Masked)
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", ks), "leaked-value") {
		t.Error("the store keeps a variable's value")
	}
	if len(ks.Environments) != 2 || !ks.Environments[0].AdminOnly || ks.Environments[1].AdminOnly {
		t.Errorf("environments %+v", ks.Environments)
	}
	if len(ks.Unread) != 1 || !strings.Contains(ks.Unread[0], "workspace's Pipelines variables: HTTP 403") {
		t.Errorf("unread %q", ks.Unread)
	}
	// Every request carried the token, to the API's host only.
	for _, a := range s.auth {
		if a != "Bearer "+maintainerToken {
			t.Errorf("authorization %q", a)
		}
	}
	// The fingerprint's form, without braces, reads the same hub.
	if _, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "bitbucket", APIURL: s.URL + "/2.0", RepoID: bbID, Token: maintainerToken}); err != nil {
		t.Errorf("the canonical id: %v", err)
	}
	if _, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "bitbucket", APIURL: s.URL + "/2.0", RepoID: "712345678", Token: maintainerToken}); err == nil {
		t.Error("a numeric id on Bitbucket")
	}
	if _, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "bitbucket", APIURL: s.URL + "/2.0", RepoID: bbID, Token: "wrong-token-0123456789"}); err == nil {
		t.Error("a refused token")
	}
}
