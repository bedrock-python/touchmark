package cli

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

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// hubComments serves the hub's API to plan --comment: the tip of master,
// and the comments of pull request #41, which GITHUB_TOKEN writes as
// github-actions[bot].
type hubComments struct {
	*httptest.Server
	mu       sync.Mutex
	tip      string
	comments map[int64]string // id → body
	order    []int64
	// writes are the requests that wrote; refuse answers them with a
	// status instead; auth the Authorization headers seen.
	writes []string
	refuse int
	auth   map[string]bool
}

func newHubComments(t *testing.T, tip string) *hubComments {
	t.Helper()
	s := &hubComments{tip: tip, comments: map[int64]string{}, auth: map[string]bool{}}
	const repo = "/repos/acme/engineering-assets"
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auth[r.Header.Get("Authorization")] = true
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo+"/git/ref/heads/master":
			fmt.Fprintf(w, `{"ref": "refs/heads/master", "object": {"sha": %q, "type": "commit"}}`, s.tip)
			return
		case r.Method == http.MethodGet && r.URL.Path == repo+"/issues/41/comments":
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			var list []map[string]any
			for _, id := range s.order {
				list = append(list, map[string]any{"id": id, "body": s.comments[id], "user": map[string]string{"login": "github-actions[bot]"}})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		s.writes = append(s.writes, r.Method+" "+r.URL.Path)
		if s.refuse != 0 {
			http.Error(w, `{"message": "Resource not accessible by integration"}`, s.refuse)
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, repo+"/issues/comments/"), 10, 64); {
		case r.Method == http.MethodPost && r.URL.Path == repo+"/issues/41/comments":
			id := int64(1000 + len(s.order))
			s.comments[id] = in.Body
			s.order = append(s.order, id)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodPatch && s.comments[id] != "":
			s.comments[id] = in.Body
			_, _ = w.Write([]byte("{}"))
		default:
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// take returns the comments and the writes so far, and forgets the writes.
func (s *hubComments) take() (comments []string, writes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		comments = append(comments, s.comments[id])
	}
	writes, s.writes = s.writes, nil
	return comments, writes
}

// plan --comment keeps one comment in the hub pull request through the hub
// channel with GITHUB_TOKEN: created by the first run, edited when the
// report changes, left alone when it does not. It holds the Markdown report,
// which names no private target of a public hub and no token.
func TestPlanComment(t *testing.T) {
	h := planHub(t)
	planPR(t, h, func(h *repo) { h.write("packs/python/docs/python.md", planPythonV2) })
	w := newPlanWorld(t)
	w.install()
	srv := newHubComments(t, h.git("rev-parse", "master"))
	vars := actionsEnv(t)
	vars["GITHUB_API_URL"] = srv.URL

	res := planRun(t, h, vars, exitOK, "--comment", "--format", "json")
	rep := decodeDelivery(t, res.stdout)
	if len(rep.Warnings) != 0 {
		t.Errorf("warnings %q", rep.Warnings)
	}
	comments, writes := srv.take()
	if len(comments) != 1 || len(writes) != 1 || !strings.HasPrefix(writes[0], "POST ") {
		t.Fatalf("comments %q, writes %q", comments, writes)
	}
	body := comments[0]
	for _, want := range []string{"### touchmark plan\n", "**Scope:** pack python changed → 2 of 11 targets", "`gh:acme/sdk`",
		"\n<!-- touchmark plan: acme-eng -->\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment lacks %q:\n%s", want, body)
		}
	}
	for _, secret := range []string{"acme/secret", ghReadToken, corpToken, hubJobToken} {
		if strings.Contains(body, secret) {
			t.Errorf("the comment holds %q", secret)
		}
	}
	srv.mu.Lock()
	if !srv.auth["Bearer "+hubJobToken] || len(srv.auth) != 1 {
		t.Errorf("Authorization headers %v", srv.auth)
	}
	srv.mu.Unlock()

	// The same report writes nothing; a new one edits the comment.
	planRun(t, h, vars, exitOK, "--comment")
	if _, writes := srv.take(); len(writes) != 0 {
		t.Errorf("an unchanged report wrote %q", writes)
	}
	w.repo("acme/snake", func(r *platform.Repo) { r.Topics = []string{"python"} }, planOptIn, "version: 1\n")
	planRun(t, h, vars, exitOK, "--comment")
	comments, writes = srv.take()
	if len(comments) != 1 || len(writes) != 1 || writes[0] != "PATCH /repos/acme/engineering-assets/issues/comments/1000" ||
		!strings.Contains(comments[0], "`gh:acme/snake`") {
		t.Errorf("after a change: comments %q, writes %q", comments, writes)
	}

	// A token that may not comment (a pull request from a fork) leaves a
	// warning; the plan goes on.
	srv.mu.Lock()
	srv.refuse = http.StatusForbidden
	srv.mu.Unlock()
	w.repo("acme/snake2", func(r *platform.Repo) { r.Topics = []string{"python"} }, planOptIn, "version: 1\n")
	rep = decodeDelivery(t, planRun(t, h, vars, exitOK, "--comment", "--format", "json").stdout)
	if len(rep.Warnings) != 1 || !strings.HasPrefix(rep.Warnings[0], "plan --comment: not posted: hub channel: comment on #41") &&
		!strings.HasPrefix(rep.Warnings[0], "plan --comment: not posted: hub channel: update comment 1000 of #41") {
		t.Errorf("refused: warnings %q", rep.Warnings)
	}

	// Outside a hub pull request there is nothing to comment on.
	rep = decodeDelivery(t, planRun(t, h, localEnv(), exitOK, "--comment", "--hub-fp", planFP, "--format", "json").stdout)
	if !slices.Contains(rep.Warnings, "plan --comment: not posted: this run builds no hub pull request") {
		t.Errorf("local: warnings %q", rep.Warnings)
	}
}

// GitLab's CI_JOB_TOKEN cannot write merge request notes: plan --comment
// says so and posts nothing.
func TestPlanCommentGitLab(t *testing.T) {
	rep := report.NewDelivery("plan", "dev")
	hctx := hubch.Context{CI: hubch.GitLabCI, Event: "merge_request_event"}
	got := postPlanComment(t.Context(), hctx, nil, 7, rep, redact.New())
	if !strings.Contains(got, "GitLab's CI_JOB_TOKEN cannot write merge request notes") {
		t.Errorf("warning %q", got)
	}
	hctx = hubch.Context{CI: hubch.GitHubActions, Event: "pull_request"}
	if got := postPlanComment(t.Context(), hctx, failedChannel{err: fmt.Errorf("hub channel: invalid repository path")}, 7, rep, redact.New()); got !=
		"plan --comment: not posted: hub channel: invalid repository path" {
		t.Errorf("failed channel: %q", got)
	}
}

// plan --assume-opt-in plans every target as opted in; the output says so
// in every format.
func TestPlanAssumeOptInOutput(t *testing.T) {
	h := planHub(t)
	w := newPlanWorld(t)
	w.install()
	s := newScenario(t, "plan-assume", h, nil)
	args := []string{"--assume-opt-in", "--hub-fp", planFP, "--only", "gh:acme/web,gh:acme/api"}
	res := planRun(t, h, localEnv(), exitOK, args...)
	s.golden("plan.txt", s.normalize(res.stdout))
	res = planRun(t, h, localEnv(), exitOK, append(args, "--format", "markdown")...)
	s.golden("plan.md", s.normalize(res.stdout))
	res = planRun(t, h, localEnv(), exitOK, append(args, "--format", "json")...)
	s.golden("plan.json", s.normalize(res.stdout))
	rep := decodeDelivery(t, res.stdout)
	if !rep.Assumed || len(rep.Targets) != 2 {
		t.Fatalf("assumed %v, %d targets", rep.Assumed, len(rep.Targets))
	}
	for _, tg := range rep.Targets {
		if want := tg.Path == "acme/web"; tg.Assumed != want || tg.Outcome == report.OutcomeSkipped {
			t.Errorf("%s: %s, assumed %v", tg.Path, tg.Outcome, tg.Assumed)
		}
	}
	res = runWith(t, nil, "plan", "--help")
	for _, flag := range []string{"--all", "--comment", "--assume-opt-in"} {
		if !strings.Contains(res.stdout, flag) {
			t.Errorf("plan --help lacks %s:\n%s", flag, res.stdout)
		}
	}
}
