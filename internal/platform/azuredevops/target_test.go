package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// targetWorld declares what Target reads: the writer's identity, the
// repository by its id, and Has Permissions answering allow for every bit
// (perms decides per bit; a missing bit is denied).
func targetWorld(t *testing.T, s *apiServer, perms map[int]bool) {
	t.Helper()
	s.json(apisPath("connectionData"), http.StatusOK, connJSON(botID, "Microsoft.IdentityModel.Claims.ClaimsIdentity;x\\bot@acme.example", "bot@acme.example"))
	s.json(repoPath(), http.StatusOK, repoJSON(repoID, "Billing", "api", "main"))
	for _, bits := range []int{permContribute, permCreateBranch, permPullRequestContib} {
		bits := bits
		s.handle(http.MethodGet, apisPath("permissions", gitNamespace, fmt.Sprint(bits)), func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("tokens"); got != "repoV2/"+projectID+"/"+repoID {
				t.Errorf("security token %q", got)
			}
			allow, ok := perms[bits]
			writeJSON(w, http.StatusOK, collection(ok && allow))
		})
	}
}

// allPerms allows everything the writer needs.
var allPerms = map[int]bool{permContribute: true, permCreateBranch: true, permPullRequestContib: true}

func newTarget(t *testing.T, s *apiServer) *target {
	t.Helper()
	w := newTestWriter(t, s, testToken(t))
	repo := testRepo()
	repo.Host = w.c.host
	tw, err := w.Target(context.Background(), repo, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	return tw.(*target)
}

func TestTargetPermissions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		perms map[int]bool
		need  platform.Perms
		rule  string
	}{
		{"all", allPerms, platform.Perms{Contents: true, PRs: true}, ""},
		{"no push", map[int]bool{permCreateBranch: true, permPullRequestContib: true}, platform.Perms{Contents: true, PRs: true}, "contents"},
		{"no branch", map[int]bool{permContribute: true, permPullRequestContib: true}, platform.Perms{Contents: true}, "contents"},
		{"no pull requests", map[int]bool{permContribute: true, permCreateBranch: true}, platform.Perms{Contents: true, PRs: true}, "pull-requests"},
		{"pull requests only", map[int]bool{permPullRequestContib: true}, platform.Perms{PRs: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAPIServer(t)
			targetWorld(t, s, tc.perms)
			w := newTestWriter(t, s, testToken(t))
			repo := testRepo()
			repo.Host = w.c.host
			_, err := w.Target(context.Background(), repo, tc.need)
			var pe *platform.Error
			switch {
			case tc.rule == "" && err != nil:
				t.Errorf("Target: %v", err)
			case tc.rule != "" && (!errors.As(err, &pe) || pe.Class != platform.ClassPermission || pe.Rule != tc.rule):
				t.Errorf("Target = %v, want ClassPermission rule %s", err, tc.rule)
			}
		})
	}
}

// TestTargetWithoutPermissionsAPI: a token the permissions API refuses
// gets its TargetWriter; a repository that is another one now is gone.
func TestTargetWithoutPermissionsAPI(t *testing.T) {
	s := newAPIServer(t)
	targetWorld(t, s, allPerms)
	for _, bits := range []int{permContribute, permCreateBranch, permPullRequestContib} {
		s.json(apisPath("permissions", gitNamespace, fmt.Sprint(bits)), http.StatusUnauthorized,
			errorBody("UnauthorizedRequestException", "TF400813: The user is not authorized to access this resource."))
	}
	w := newTestWriter(t, s, testToken(t))
	repo := testRepo()
	repo.Host = w.c.host
	if _, err := w.Target(context.Background(), repo, platform.Perms{Contents: true, PRs: true}); err != nil {
		t.Errorf("Target without the permissions API: %v", err)
	}
	if n := len(s.requests(http.MethodGet, apisPath("permissions", gitNamespace, fmt.Sprint(permContribute)))); n != 1 {
		t.Errorf("%d permission requests, want 1 (the rest skipped)", n)
	}
	s.json(repoPath(), http.StatusOK, repoJSON(otherRepo, "Billing", "api", "main"))
	_, err := w.Target(context.Background(), repo, platform.Perms{Contents: true})
	if !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("another repository now: %v", err)
	}
}

// createWorld declares the routes of CreatePR on top of targetWorld: no
// active pull request until the POST, which answers #11; its properties
// and labels.
func createWorld(t *testing.T, s *apiServer) (created *map[string]any, props *[]patchOp) {
	t.Helper()
	targetWorld(t, s, allPerms)
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	var pr map[string]any
	var stored []patchOp
	line := ""
	s.handle(http.MethodGet, repoPath("pullrequests"), func(w http.ResponseWriter, _ *http.Request) {
		if pr == nil {
			writeJSON(w, http.StatusOK, collection())
			return
		}
		writeJSON(w, http.StatusOK, collection(pr))
	})
	s.handle(http.MethodPost, repoPath("pullrequests"), func(w http.ResponseWriter, r *http.Request) {
		var in createPR
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		pr = prJSON(11, statusActive, strings.TrimPrefix(in.SourceRefName, "refs/heads/"), strings.TrimPrefix(in.TargetRefName, "refs/heads/"), bot, in.Description)
		pr["isDraft"] = in.IsDraft
		pr["title"] = in.Title
		if len(in.Labels) > 0 {
			// Only the first label is applied by the create request.
			pr["labels"] = []any{map[string]any{"id": "l0", "name": in.Labels[0].Name, "active": true}}
		}
		writeJSON(w, http.StatusCreated, pr)
	})
	s.handle(http.MethodGet, repoPath("pullrequests", "11"), func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, pr) })
	s.handle(http.MethodPatch, repoPath("pullRequests", "11", "properties"), func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json-patch+json" {
			t.Errorf("properties sent as %q", r.Header.Get("Content-Type"))
		}
		var ops []patchOp
		if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, ops...)
		for _, op := range ops {
			if op.Op == "remove" {
				line = ""
			} else if op.Value != nil {
				line = *op.Value
			}
		}
		writeJSON(w, http.StatusOK, propsJSON(line))
	})
	s.handle(http.MethodGet, repoPath("pullRequests", "11", "properties"), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, propsJSON(line))
	})
	s.handle(http.MethodPost, repoPath("pullRequests", "11", "labels"), func(w http.ResponseWriter, r *http.Request) {
		var in apiLabelName
		_ = json.NewDecoder(r.Body).Decode(&in)
		labels, _ := pr["labels"].([]any)
		pr["labels"] = append(labels, map[string]any{"id": "l" + in.Name, "name": in.Name, "active": true})
		writeJSON(w, http.StatusOK, map[string]any{"id": "l" + in.Name, "name": in.Name, "active": true})
	})
	s.json(repoPath("refs"), http.StatusOK, collection(map[string]any{"name": "refs/heads/" + syncBranch, "objectId": headSHA}))
	return &pr, &stored
}

func TestCreatePR(t *testing.T) {
	s := newAPIServer(t)
	_, stored := createWorld(t, s)
	tw := newTarget(t, s)
	body := "## Sync\n\ntext\n\n" + testLine
	pr, err := tw.CreatePR(context.Background(), platform.NewPR{Head: syncBranch, Base: "main", Title: "chore: sync",
		Body: body, Labels: []string{"engineering-assets", "deps"}, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	posts := s.requests(http.MethodPost, repoPath("pullrequests"))
	var in createPR
	if len(posts) != 1 || json.Unmarshal(posts[0].Body, &in) != nil {
		t.Fatalf("POSTs %d", len(posts))
	}
	if in.Description != "## Sync\n\ntext" || !in.IsDraft || in.SourceRefName != "refs/heads/"+syncBranch || len(in.Labels) != 2 {
		t.Errorf("POST %+v: the description must not hold the marker", in)
	}
	if len(*stored) != 1 || (*stored)[0].Path != "/touchmark.marker" || *(*stored)[0].Value != testLine {
		t.Errorf("properties %+v", *stored)
	}
	if pr.Number != 11 || pr.Body != body || !pr.Draft || pr.HeadSHA != headSHA || len(pr.Labels) != 2 {
		t.Errorf("CreatePR = %+v", pr)
	}
	if n := len(s.requests(http.MethodPost, repoPath("pullRequests", "11", "labels"))); n != 1 {
		t.Errorf("%d labels added after the create, want 1", n)
	}

	// The same head again: the active pull request, with ErrExists.
	again, err := tw.CreatePR(context.Background(), platform.NewPR{Head: syncBranch, Base: "main", Title: "chore: sync", Body: body})
	if !errors.Is(err, platform.ErrExists) || again.Number != 11 || again.Body != body {
		t.Errorf("a second CreatePR = #%d, %v", again.Number, err)
	}

	// A description over 4 000 characters is refused before anything.
	_, err = tw.CreatePR(context.Background(), platform.NewPR{Head: "other", Base: "main", Title: "t", Body: strings.Repeat("ж", 4001) + "\n\n" + testLine})
	wantClass(t, "a long description", err, platform.ClassInvalid)
}

// TestCreatePRDuplicate: a POST refused as a duplicate returns the active
// pull request that appeared meanwhile.
func TestCreatePRDuplicate(t *testing.T) {
	s := newAPIServer(t)
	targetWorld(t, s, allPerms)
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	calls := 0
	s.handle(http.MethodGet, repoPath("pullrequests"), func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(w, http.StatusOK, collection())
			return
		}
		writeJSON(w, http.StatusOK, collection(prJSON(12, statusActive, syncBranch, "main", bot, "theirs")))
	})
	s.json(repoPath("pullRequests", "12", "properties"), http.StatusOK, propsJSON(testLine))
	s.json(repoPath("refs"), http.StatusOK, collection())
	s.handle(http.MethodPost, repoPath("pullrequests"), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, errorBody(keyPRExists, tfPRExists+": An active pull request for the source and target branch already exists."))
	})
	pr, err := newTarget(t, s).CreatePR(context.Background(), platform.NewPR{Head: syncBranch, Base: "main", Title: "t", Body: "x\n\n" + testLine})
	if !errors.Is(err, platform.ErrExists) || pr.Number != 12 {
		t.Errorf("CreatePR = #%d, %v", pr.Number, err)
	}
}

// editWorld declares an active #11 with a stored marker, and the PATCH of
// the pull request.
func editWorld(t *testing.T, s *apiServer, status string) (patches *[]updatePR) {
	t.Helper()
	patches, _ = editWorldPR(t, s, status)
	return patches
}

// editWorldPR is editWorld that also returns #11 as the routes answer it.
func editWorldPR(t *testing.T, s *apiServer, status string) (patches *[]updatePR, pr *map[string]any) {
	t.Helper()
	pr, _ = createWorld(t, s)
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	*pr = prJSON(11, status, syncBranch, "main", bot, "old text")
	var got []updatePR
	s.handle(http.MethodPatch, repoPath("pullrequests", "11"), func(w http.ResponseWriter, r *http.Request) {
		var in updatePR
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		got = append(got, in)
		if in.Description != nil {
			(*pr)["description"] = *in.Description
		}
		if in.Title != nil {
			(*pr)["title"] = *in.Title
		}
		if in.Status != nil {
			(*pr)["status"] = *in.Status
			(*pr)["closedDate"] = "2026-03-01T00:00:00Z"
		}
		writeJSON(w, http.StatusOK, *pr)
	})
	return &got, pr
}

func TestEditPR(t *testing.T) {
	s := newAPIServer(t)
	patches := editWorld(t, s, statusActive)
	tw := newTarget(t, s)
	newLine := strings.Replace(testLine, "key=sha256:6b1f", "key=sha256:0000", 1)
	body := "new text\n\n" + newLine
	closed := platform.Closed
	pr, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &body, State: &closed, AddLabels: []string{"engineering-assets"}})
	if err != nil {
		t.Fatal(err)
	}
	// The property first, then the labels, then one PATCH.
	var order []string
	for _, c := range s.writes() {
		order = append(order, c.Method+" "+c.Path[strings.LastIndex(c.Path, "/")+1:])
	}
	if strings.Join(order, ", ") != "PATCH properties, POST labels, PATCH 11" {
		t.Errorf("writes %v", order)
	}
	if len(*patches) != 1 || (*patches)[0].Description == nil || *(*patches)[0].Description != "new text" ||
		(*patches)[0].Status == nil || *(*patches)[0].Status != "abandoned" || (*patches)[0].Title != nil {
		t.Errorf("PATCH %+v", *patches)
	}
	if pr.State != platform.Closed || pr.Body != body {
		t.Errorf("EditPR = %+v", pr)
	}
	if _, line := marker.Detach(pr.Body); line != newLine {
		t.Errorf("the stored marker %q", line)
	}

	// Abandoned now: never reactivated, never changed; closing again is
	// nothing.
	s.reset()
	open := platform.Open
	_, err = tw.EditPR(context.Background(), 11, platform.PREdit{State: &open})
	wantClass(t, "reopen", err, platform.ClassUnsupported)
	other := "other\n\n" + newLine
	_, err = tw.EditPR(context.Background(), 11, platform.PREdit{Body: &other})
	wantClass(t, "edit an abandoned one", err, platform.ClassUnsupported)
	if _, err := tw.EditPR(context.Background(), 11, platform.PREdit{State: &closed, Body: &body}); err != nil {
		t.Errorf("closing an abandoned one with its own body: %v", err)
	}
	if w := s.writes(); len(w) != 0 {
		t.Errorf("writes to an abandoned pull request: %v", w)
	}
}

// TestEditPRUnchanged: an edit that changes nothing writes nothing, and a
// title alone is one PATCH without description.
func TestEditPRUnchanged(t *testing.T) {
	s := newAPIServer(t)
	patches := editWorld(t, s, statusActive)
	tw := newTarget(t, s)
	same := "old text\n\n" + testLine
	// The marker alone goes to the property, without a PATCH of the pull
	// request.
	if _, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &same}); err != nil || len(*patches) != 0 {
		t.Fatalf("setup: %v, %+v", err, *patches)
	}
	s.reset()
	if _, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &same}); err != nil {
		t.Fatal(err)
	}
	if w := s.writes(); len(w) != 0 {
		t.Errorf("writes for no change: %+v", w)
	}
	title := "chore: new title"
	if _, err := tw.EditPR(context.Background(), 11, platform.PREdit{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if len(*patches) != 1 || (*patches)[0].Title == nil || (*patches)[0].Description != nil || (*patches)[0].Status != nil {
		t.Errorf("PATCH %+v", *patches)
	}
}

func TestEditPRCompleted(t *testing.T) {
	s := newAPIServer(t)
	editWorld(t, s, statusCompleted)
	tw := newTarget(t, s)
	body := "x\n\n" + testLine
	_, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &body})
	wantClass(t, "a completed pull request", err, platform.ClassConflict)
	merged := platform.Merged
	_, err = tw.EditPR(context.Background(), 11, platform.PREdit{State: &merged})
	wantClass(t, "merging", err, platform.ClassUnsupported)
}

func TestCommentAndLabels(t *testing.T) {
	s := newAPIServer(t)
	targetWorld(t, s, allPerms)
	s.handle(http.MethodPost, repoPath("pullRequests", "11", "threads"), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 1})
	})
	tw := newTarget(t, s)
	if err := tw.Comment(context.Background(), 11, "hello"); err != nil {
		t.Fatal(err)
	}
	var thread struct {
		Comments []struct {
			Content     string `json:"content"`
			CommentType string `json:"commentType"`
		} `json:"comments"`
		Status string `json:"status"`
	}
	posts := s.requests(http.MethodPost, repoPath("pullRequests", "11", "threads"))
	if len(posts) != 1 || json.Unmarshal(posts[0].Body, &thread) != nil || len(thread.Comments) != 1 ||
		thread.Comments[0].Content != "hello" || thread.Comments[0].CommentType != "text" || thread.Status != "closed" {
		t.Errorf("thread %s", posts[0].Body)
	}
	names, err := tw.EnsureLabels(context.Background(), []string{"a", "A", "b", ""})
	if err != nil || strings.Join(names, ",") != "a,b" {
		t.Errorf("EnsureLabels = %v, %v", names, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	wantClass(t, "after Close", tw.Comment(context.Background(), 11, "x"), platform.ClassAuth)
	if _, err := tw.Remote().Header(context.Background()); platform.ClassOf(err) != platform.ClassAuth {
		t.Errorf("git credentials after Close: %v", err)
	}
}
