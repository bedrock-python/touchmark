package gitea

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// targetWorld is a writer with a TargetWriter on acme/api (id 7).
type targetWorld struct {
	*prWorld
	tw platform.TargetWriter
}

func newTargetWorld(t *testing.T, typ string) *targetWorld {
	t.Helper()
	w := newPRWorld(t, typ)
	if typ == "gitea" {
		w.asGitea()
	} else {
		w.asForgejo("16.0.5")
	}
	w.json(http.MethodGet, "/user", http.StatusOK, w.writer)
	repo := w.apiServer.repo(7, "acme/api", withField("permissions", map[string]any{"admin": false, "push": true, "pull": true}))
	w.json(http.MethodGet, "/repositories/7", http.StatusOK, repo)
	tw, err := w.fixture.writer.Target(t.Context(), w.target, platform.Perms{Contents: true, PRs: true, Workflows: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tw.Close() })
	return &targetWorld{prWorld: w, tw: tw}
}

// labels declares the repository and organization labels and records the
// labels created. The listings send X-Total-Count and no Link, as the
// servers' label listings do; nil org labels answer 404, as for a user's
// repository.
func (w *targetWorld) labels(repo, org []map[string]any) *[]string {
	var created []string
	next := int64(100)
	w.pagesWith("/repos/acme/api/labels", toAny(repo), totalOnly)
	if org == nil {
		w.json(http.MethodGet, "/orgs/acme/labels", http.StatusNotFound, w.apiMsg("GetOrgByName"))
	} else {
		w.pagesWith("/orgs/acme/labels", toAny(org), totalOnly)
	}
	w.handle(http.MethodPost, "/repos/acme/api/labels", func(rw http.ResponseWriter, r *http.Request) {
		var in createLabel
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Color != "#ededed" {
			w.t.Errorf("create label %+v, %v", in, err)
		}
		created = append(created, in.Name)
		next++
		writeJSON(rw, http.StatusCreated, w.label(next, in.Name))
	})
	return &created
}

func toAny(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func TestTarget(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	rem := w.tw.Remote()
	if rem.URL != w.base()+"/acme/api.git" || rem.Header == nil {
		t.Errorf("Remote = %+v", rem)
	}
	if h, err := rem.Header(t.Context()); err != nil || !strings.HasPrefix(h, "Basic ") {
		t.Errorf("Remote header %q, %v", h, err)
	}
	// The same URL as the reader's remote: distribute pushes where it read.
	if rr, err := w.reader.Remote(t.Context(), w.target); err != nil || rr.URL != rem.URL {
		t.Errorf("the reader's remote %q, %v", rr.URL, err)
	}
	if err := w.tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.tw.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	w.reset()
	_, err := w.tw.EditPR(t.Context(), 7, platform.PREdit{Body: ptr("x")})
	wantClass(t, "EditPR after Close", err, platform.ClassAuth, nil)
	wantClass(t, "Comment after Close", w.tw.Comment(t.Context(), 7, "x"), platform.ClassAuth, nil)
	_, err = w.tw.CreatePR(t.Context(), platform.NewPR{Head: "a", Base: "main", Title: "t"})
	wantClass(t, "CreatePR after Close", err, platform.ClassAuth, nil)
	_, err = w.tw.EnsureLabels(t.Context(), []string{"x"})
	wantClass(t, "EnsureLabels after Close", err, platform.ClassAuth, nil)
	_, err = rem.Header(t.Context())
	wantClass(t, "Remote header after Close", err, platform.ClassAuth, nil)
	if n := len(w.requests("", "")); n != 0 {
		t.Errorf("%d requests after Close", n)
	}
}

func TestTargetDenied(t *testing.T) {
	w := newPRWorld(t, "gitea")
	w.json(http.MethodGet, "/user", http.StatusOK, w.writer)
	w.json(http.MethodGet, "/repositories/7", http.StatusOK, w.apiServer.repo(7, "acme/api"))
	w.json(http.MethodGet, "/repositories/8", http.StatusNotFound, w.apiMsg("not found"))
	for _, tc := range []struct {
		need platform.Perms
		rule string
	}{
		{platform.Perms{Contents: true, PRs: true}, "contents"},
		{platform.Perms{PRs: true}, "pull-requests"},
	} {
		_, err := w.fixture.writer.Target(t.Context(), w.target, tc.need)
		var pe *platform.Error
		if !errors.As(err, &pe) || pe.Class != platform.ClassPermission || pe.Rule != tc.rule {
			t.Errorf("Target(%+v) = %v, want permission %s", tc.need, err, tc.rule)
		}
	}
	// Nothing needed, nothing refused: a pull-only writer may still read.
	if tw, err := w.fixture.writer.Target(t.Context(), w.target, platform.Perms{}); err != nil {
		t.Errorf("Target without needs: %v", err)
	} else {
		_ = tw.Close()
	}
	_, err := w.fixture.writer.Target(t.Context(), platform.Repo{Host: w.target.Host, ID: "8", Path: "acme/hidden"}, platform.Perms{Contents: true})
	wantClass(t, "Target of a hidden repository", err, platform.ClassNotFound, platform.ErrNotFound)
	_, err = w.fixture.writer.Target(t.Context(), platform.Repo{Host: "other.example.com", ID: "7", Path: "acme/api"}, platform.Perms{Contents: true})
	wantClass(t, "Target on another host", err, platform.ClassInvalid, nil)
}

func TestCreatePR(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	created := w.labels([]map[string]any{w.label(5, "engineering-assets"), w.label(3, "engineering-assets"), w.label(6, "other")}, nil)
	var sent createPR
	w.handle(http.MethodPost, "/repos/acme/api/pulls", func(rw http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		writeJSON(rw, http.StatusCreated, w.pr(w.repo, prSpec{number: 13, author: w.writer, head: sent.Head, title: sent.Title,
			body: sent.Body, labels: []map[string]any{w.label(3, "engineering-assets"), w.label(101, "conformance")}}))
	})
	pr, err := w.tw.CreatePR(t.Context(), platform.NewPR{Head: freshBranch, Base: "main", Title: "chore: sync",
		Body: "body ⚠", Labels: []string{"engineering-assets", "conformance"}})
	if err != nil {
		t.Fatal(err)
	}
	// Labels by id: the lowest id of a duplicated name, a created one.
	if sent.Head != freshBranch || sent.Base != "main" || sent.Title != "chore: sync" || sent.Body != "body ⚠" ||
		!slices.Equal(sent.Labels, []int64{3, 101}) || !slices.Equal(*created, []string{"conformance"}) {
		t.Errorf("sent %+v, created labels %v", sent, *created)
	}
	if pr.Number != 13 || pr.State != platform.Open || pr.Head != freshBranch || pr.HeadRepoID != "7" || pr.RepoID != "7" ||
		pr.Author.ID != "3" || !slices.Equal(pr.Labels, []string{"engineering-assets", "conformance"}) || pr.Draft {
		t.Errorf("CreatePR = %+v", pr)
	}
	// The open pull requests were looked at first.
	if n := len(w.requests(http.MethodGet, "/repos/acme/api/pulls")); n == 0 {
		t.Error("CreatePR did not look for an open pull request from the head")
	}
}

// freshBranch is a head no pull request of prWorld is open from.
const freshBranch = "touchmark/fresh"

func TestCreatePRDraft(t *testing.T) {
	for _, recognized := range []bool{true, false} {
		w := newTargetWorld(t, "forgejo")
		w.labels(nil, nil)
		var titles []string
		w.handle(http.MethodPost, "/repos/acme/api/pulls", func(rw http.ResponseWriter, r *http.Request) {
			var in createPR
			_ = json.NewDecoder(r.Body).Decode(&in)
			titles = append(titles, in.Title)
			writeJSON(rw, http.StatusCreated, w.pr(w.repo, prSpec{number: 13, author: w.writer, head: in.Head, title: in.Title,
				draft: recognized && strings.HasPrefix(in.Title, "WIP:")}))
		})
		w.handle(http.MethodPatch, "/repos/acme/api/pulls/13", func(rw http.ResponseWriter, r *http.Request) {
			var in editPR
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Title == nil || in.Body != nil || in.State != nil {
				t.Errorf("the fallback PATCH %+v", in)
			}
			titles = append(titles, *in.Title)
			writeJSON(rw, http.StatusCreated, w.pr(w.repo, prSpec{number: 13, author: w.writer, head: syncBranch, title: *in.Title}))
		})
		pr, err := w.tw.CreatePR(t.Context(), platform.NewPR{Head: freshBranch, Base: "main", Title: "chore: sync", Draft: true})
		if err != nil {
			t.Fatal(err)
		}
		if recognized {
			if !pr.Draft || pr.Title != "WIP: chore: sync" || !slices.Equal(titles, []string{"WIP: chore: sync"}) {
				t.Errorf("a draft: %+v, titles %q", pr, titles)
			}
		} else if pr.Draft || pr.Title != "chore: sync" || !slices.Equal(titles, []string{"WIP: chore: sync", "chore: sync"}) {
			t.Errorf("a draft the instance does not take: %+v, titles %q", pr, titles)
		}
	}
}

// An open pull request from the head in the repository itself is returned
// with ErrExists before anything is written, whatever its base: the servers
// refuse (409) only a duplicate to the same base, and open a second pull
// request from the head to another base (seen on Forgejo 16.0.5). #9, a
// fork's open pull request from a branch of the same name, and #13, an
// AGit one, are not from the head.
func TestCreatePRExisting(t *testing.T) {
	for _, base := range []string{"main", "develop"} {
		w := newTargetWorld(t, "gitea")
		w.labels(nil, nil)
		pr, err := w.tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: base, Title: "t", Labels: []string{"x"}})
		wantClass(t, "CreatePR of an open head to "+base, err, platform.ClassConflict, platform.ErrExists)
		if pr.Number != 7 || pr.HeadRepoID != "7" {
			t.Errorf("CreatePR to %s returned #%d (head repo %s), want the open #7", base, pr.Number, pr.HeadRepoID)
		}
		if q := queries(w.requests(http.MethodGet, "/repos/acme/api/pulls")); !slices.Equal(q, []string{"state=open"}) {
			t.Errorf("the open pull request was looked up with %q", q)
		}
		if n := len(w.requests(http.MethodPost, "")); n != 0 {
			t.Errorf("CreatePR to %s wrote %d times", base, n)
		}
	}
}

// A duplicate opened between the lookup and the POST (409) gives the same
// ErrExists; a 409 without an open pull request any more is the conflict
// alone.
func TestCreatePRExistingRace(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	w.labels(nil, nil)
	own := w.prs[6] // #7
	w.prs = slices.Delete(w.prs, 6, 7)
	w.handle(http.MethodPost, "/repos/acme/api/pulls", func(rw http.ResponseWriter, r *http.Request) {
		var in createPR
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Head == syncBranch {
			w.prs = append(w.prs, own) // someone was faster
		}
		writeJSON(rw, http.StatusConflict, w.apiMsg("pull request already exists for these targets [id: 107, issue_id: 107, head_repo_id: 7, base_repo_id: 7, head_branch: "+in.Head+", base_branch: main]"))
	})
	pr, err := w.tw.CreatePR(t.Context(), platform.NewPR{Head: syncBranch, Base: "main", Title: "t"})
	wantClass(t, "CreatePR racing a duplicate", err, platform.ClassConflict, platform.ErrExists)
	var pe *platform.Error
	if pr.Number != 7 || !errors.As(err, &pe) || pe.Status != http.StatusConflict {
		t.Errorf("CreatePR racing a duplicate = #%d, %v; want #7 with 409", pr.Number, err)
	}
	pr, err = w.tw.CreatePR(t.Context(), platform.NewPR{Head: "feature/gone", Base: "main", Title: "t"})
	wantClass(t, "CreatePR conflict without an open one", err, platform.ClassConflict, nil)
	if errors.Is(err, platform.ErrExists) || pr.Number != 0 {
		t.Errorf("CreatePR = #%d, %v; want no pull request and no ErrExists", pr.Number, err)
	}
}

func TestCreatePRRefusals(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	w.labels(nil, nil)
	for _, np := range []platform.NewPR{
		{Head: "", Base: "main", Title: "t"},
		{Head: "a", Base: "a", Title: "t"},
		{Head: "a", Base: "main", Title: "  "},
		{Head: "a", Base: "main", Title: "t", Labels: []string{" "}},
	} {
		_, err := w.tw.CreatePR(t.Context(), np)
		wantClass(t, "CreatePR("+np.Head+", "+np.Title+")", err, platform.ClassInvalid, nil)
	}
	w.json(http.MethodPost, "/repos/acme/api/pulls", http.StatusLocked, w.apiMsg("repo is archived"))
	_, err := w.tw.CreatePR(t.Context(), platform.NewPR{Head: "a", Base: "main", Title: "t"})
	var pe *platform.Error
	if !errors.As(err, &pe) || pe.Class != platform.ClassPermission || pe.Rule != "archived" || pe.Status != http.StatusLocked {
		t.Errorf("CreatePR in an archived repository: %v", err)
	}

	// Pull requests turned off: refused before any request.
	w2 := newPRWorld(t, "gitea")
	w2.json(http.MethodGet, "/user", http.StatusOK, w2.writer)
	w2.json(http.MethodGet, "/repositories/7", http.StatusOK, w2.apiServer.repo(7, "acme/api", withField("has_pull_requests", false),
		withField("permissions", map[string]any{"admin": false, "push": true, "pull": true})))
	tw, err := w2.fixture.writer.Target(t.Context(), w2.target, platform.Perms{PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tw.CreatePR(t.Context(), platform.NewPR{Head: "a", Base: "main", Title: "t"})
	if !errors.As(err, &pe) || pe.Class != platform.ClassPolicy || pe.Rule != "prs-disabled" {
		t.Errorf("CreatePR with pull requests off: %v", err)
	}
}

func TestEditPR(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	w.labels([]map[string]any{w.label(1, "engineering-assets")}, []map[string]any{w.label(40, "org-wide")})
	var order []string
	var patches []map[string]any
	w.handle(http.MethodPost, "/repos/acme/api/issues/7/labels", func(rw http.ResponseWriter, r *http.Request) {
		var in addLabels
		_ = json.NewDecoder(r.Body).Decode(&in)
		order = append(order, "labels "+jsonOf(in.Labels))
		writeJSON(rw, http.StatusOK, []any{w.label(1, "engineering-assets"), w.label(40, "org-wide")})
	})
	w.handle(http.MethodPatch, "/repos/acme/api/pulls/7", func(rw http.ResponseWriter, r *http.Request) {
		patch := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		patches = append(patches, patch)
		order = append(order, "patch")
		p := w.pr(w.repo, prSpec{number: 7, author: w.writer, head: syncBranch, title: "t", body: "closed body",
			state: "closed", closedAt: "2026-09-20T10:00:00Z", labels: []map[string]any{w.label(1, "engineering-assets"), w.label(40, "org-wide")}})
		writeJSON(rw, http.StatusCreated, p)
	})
	w.timelines[7] = []any{w.event(9, "close", w.writer, "2026-09-20T10:00:00Z")}
	w.serve()
	body, state := "closed body", platform.Closed
	pr, err := w.tw.EditPR(t.Context(), 7, platform.PREdit{Body: &body, State: &state, AddLabels: []string{"org-wide"}})
	if err != nil {
		t.Fatal(err)
	}
	// What the server may refuse goes first, alone: the state; then the
	// labels (by id, the organization's); then the body.
	if !slices.Equal(order, []string{"patch", "labels [40]", "patch"}) {
		t.Errorf("requests %q", order)
	}
	if len(patches) != 2 || jsonOf(patches[0]) != `{"state":"closed"}` || jsonOf(patches[1]) != `{"body":"closed body"}` {
		t.Errorf("PATCH requests %v", patches)
	}
	if pr.State != platform.Closed || pr.Body != "closed body" || pr.ClosedBy == nil || pr.ClosedBy.ID != "3" ||
		!slices.Equal(pr.Labels, []string{"engineering-assets", "org-wide"}) {
		t.Errorf("EditPR = %+v (closed by %+v)", pr, pr.ClosedBy)
	}
}

// A refused state or base changes nothing: no body, title or label is
// written (a merged pull request or open dependencies answer 412, a
// missing base 404; the servers keep a title and body sent in the same
// PATCH).
func TestEditPRRefusalWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   platform.PREdit
		status int
		msg    string
		class  platform.Class
		rule   string
	}{
		{"merged", platform.PREdit{State: ptr(platform.Closed)}, http.StatusPreconditionFailed,
			"cannot change state of this pull request, it was already merged", platform.ClassConflict, ""},
		{"dependencies", platform.PREdit{State: ptr(platform.Closed)}, http.StatusPreconditionFailed,
			"cannot close this pull request because it still has open dependencies", platform.ClassPolicy, "dependencies"},
		{"dependencies (Gitea)", platform.PREdit{State: ptr(platform.Closed)}, http.StatusPreconditionFailed,
			"cannot close this issue or pull request because it still has open dependencies", platform.ClassPolicy, "dependencies"},
		{"missing base", platform.PREdit{Base: ptr("no-such-base")}, http.StatusNotFound,
			"new base 'no-such-base' not exist", platform.ClassInvalid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTargetWorld(t, "gitea")
			w.labels([]map[string]any{w.label(1, "engineering-assets")}, nil)
			var patches []map[string]any
			w.handle(http.MethodPatch, "/repos/acme/api/pulls/7", func(rw http.ResponseWriter, r *http.Request) {
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				patches = append(patches, in)
				writeJSON(rw, tc.status, w.apiMsg(tc.msg))
			})
			e := tc.edit
			e.Title, e.Body, e.AddLabels = ptr("new title"), ptr("closed by touchmark"), []string{"engineering-assets"}
			_, err := w.tw.EditPR(t.Context(), 7, e)
			wantClass(t, "EditPR", err, tc.class, nil)
			var pe *platform.Error
			if tc.rule != "" && (!errors.As(err, &pe) || pe.Rule != tc.rule) {
				t.Errorf("EditPR: %v, want rule %s", err, tc.rule)
			}
			if len(patches) != 1 || patches[0]["body"] != nil || patches[0]["title"] != nil {
				t.Errorf("PATCH requests %v, want one without title and body", patches)
			}
			if n := len(w.requests(http.MethodPost, "/repos/acme/api/issues/7/labels")); n != 0 {
				t.Errorf("labels added %d times", n)
			}
		})
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A label that cannot be added fails EditPR before its PATCH: the marker the
// PATCH writes records the labels set (labels_set), and a label recorded but
// never added would not be tried again.
func TestEditPRLabelFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  platform.Class
	}{
		{http.StatusForbidden, platform.ClassPermission},
		{http.StatusInternalServerError, platform.ClassTransient},
		{http.StatusTooManyRequests, platform.ClassRateLimited},
	} {
		w := newTargetWorld(t, "gitea")
		w.labels([]map[string]any{w.label(1, "engineering-assets")}, nil)
		w.json(http.MethodPost, "/repos/acme/api/issues/7/labels", tc.status, w.apiMsg("labels refused"))
		w.handle(http.MethodPatch, "/repos/acme/api/pulls/7", func(rw http.ResponseWriter, _ *http.Request) {
			writeJSON(rw, http.StatusCreated, w.pr(w.repo, prSpec{number: 7, author: w.writer, head: syncBranch}))
		})
		body := "a new body"
		_, err := w.tw.EditPR(t.Context(), 7, platform.PREdit{Body: &body, AddLabels: []string{"engineering-assets"}})
		wantClass(t, "EditPR when the label is refused with "+http.StatusText(tc.status), err, tc.class, nil)
		if n := len(w.requests(http.MethodPatch, "/repos/acme/api/pulls/7")); n != 0 {
			t.Errorf("%d: EditPR sent %d PATCH requests after the labels failed", tc.status, n)
		}
	}
}

// A new title of a draft keeps its prefix; a ready pull request gets none.
func TestEditPRTitleKeepsDraft(t *testing.T) {
	for _, tc := range []struct {
		current string
		draft   bool
		want    string
	}{
		{"WIP: old", true, "WIP: new"},
		{"[WIP]  old", true, "[WIP]  new"},
		{"wip:old", true, "wip:new"},
		{"Draft: old", true, "WIP: new"}, // a prefix of the instance's own
		{"old", false, "new"},
	} {
		w := newTargetWorld(t, "forgejo")
		w.json(http.MethodGet, "/repos/acme/api/pulls/7", http.StatusOK,
			w.pr(w.repo, prSpec{number: 7, author: w.writer, head: syncBranch, title: tc.current, draft: tc.draft}))
		var sent editPR
		w.handle(http.MethodPatch, "/repos/acme/api/pulls/7", func(rw http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&sent)
			writeJSON(rw, http.StatusCreated, w.pr(w.repo, prSpec{number: 7, author: w.writer, head: syncBranch, title: *sent.Title, draft: tc.draft}))
		})
		pr, err := w.tw.EditPR(t.Context(), 7, platform.PREdit{Title: ptr("new")})
		if err != nil || sent.Title == nil || *sent.Title != tc.want || pr.Draft != tc.draft {
			t.Errorf("title %q → %v, %v; want %q", tc.current, sent.Title, err, tc.want)
		}
	}
}

func TestEditPRErrors(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	w.json(http.MethodPatch, "/repos/acme/api/pulls/404", http.StatusNotFound, w.apiMsg("not found"))
	w.json(http.MethodPatch, "/repos/acme/api/pulls/1", http.StatusPreconditionFailed, w.apiMsg("cannot change state of this pull request, it was already merged"))
	w.json(http.MethodPatch, "/repos/acme/api/pulls/7", http.StatusNotFound, w.apiMsg("new base 'nosuch' not exist"))
	_, err := w.tw.EditPR(t.Context(), 404, platform.PREdit{Body: ptr("x")})
	wantClass(t, "EditPR of a missing pull request", err, platform.ClassNotFound, platform.ErrNotFound)
	_, err = w.tw.EditPR(t.Context(), 1, platform.PREdit{State: ptr(platform.Open)})
	wantClass(t, "reopening a merged pull request", err, platform.ClassConflict, nil)
	_, err = w.tw.EditPR(t.Context(), 7, platform.PREdit{Base: ptr("nosuch")})
	wantClass(t, "a missing base", err, platform.ClassInvalid, nil)
	if errors.Is(err, platform.ErrNotFound) {
		t.Errorf("a missing base reads as a missing pull request: %v", err)
	}
	for _, e := range []platform.PREdit{{Title: ptr(" ")}, {State: ptr(platform.Merged)}, {Base: ptr("")}} {
		_, err := w.tw.EditPR(t.Context(), 7, e)
		wantClass(t, "a wrong edit", err, platform.ClassInvalid, nil)
	}
	// Nothing to change: the pull request as it is.
	pr, err := w.tw.EditPR(t.Context(), 8, platform.PREdit{})
	if err != nil || pr.Number != 8 {
		t.Errorf("an empty edit = %+v, %v", pr, err)
	}
}

func TestComment(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	var got string
	w.handle(http.MethodPost, "/repos/acme/api/issues/7/comments", func(rw http.ResponseWriter, r *http.Request) {
		var in comment
		_ = json.NewDecoder(r.Body).Decode(&in)
		got = in.Body
		writeJSON(rw, http.StatusCreated, map[string]any{"id": 25, "body": in.Body, "user": w.writer})
	})
	body := "touchmark remembers ⚠\n\n```yaml\nignore:\n  - AGENTS.md\n```"
	if err := w.tw.Comment(t.Context(), 7, body); err != nil || got != body {
		t.Errorf("Comment: %v, sent %q", err, got)
	}
	// Gitea answers a comment on a missing issue with 500.
	w.json(http.MethodPost, "/repos/acme/api/issues/999999/comments", http.StatusInternalServerError, map[string]any{"message": "", "url": ""})
	w.json(http.MethodGet, "/repos/acme/api/pulls/999999", http.StatusNotFound, w.apiMsg("not found"))
	err := w.tw.Comment(t.Context(), 999999, "x")
	wantClass(t, "Comment on a missing pull request", err, platform.ClassNotFound, platform.ErrNotFound)
	// A 500 on a pull request that exists stays transient.
	w.json(http.MethodPost, "/repos/acme/api/issues/8/comments", http.StatusInternalServerError, map[string]any{"message": "", "url": ""})
	wantClass(t, "Comment with a server error", w.tw.Comment(t.Context(), 8, "x"), platform.ClassTransient, nil)
	w.json(http.MethodPost, "/repos/acme/api/issues/2/comments", http.StatusLocked, w.apiMsg("issue is locked"))
	err = w.tw.Comment(t.Context(), 2, "x")
	var pe *platform.Error
	if !errors.As(err, &pe) || pe.Class != platform.ClassPolicy || pe.Rule != "locked" {
		t.Errorf("Comment on a locked conversation: %v", err)
	}
	wantClass(t, "a blank comment", w.tw.Comment(t.Context(), 7, " \n"), platform.ClassInvalid, nil)
}

func TestEnsureLabels(t *testing.T) {
	w := newTargetWorld(t, "gitea")
	w.settings(2)
	created := w.labels(
		[]map[string]any{w.label(9, "conformance-y"), w.label(2, "b"), w.label(3, "c"), w.label(8, "conformance-y")},
		[]map[string]any{w.label(50, "org"), w.label(51, "conformance-y")})
	ids, err := w.tw.EnsureLabels(t.Context(), []string{"conformance-x", "conformance-y", "org", "conformance-x"})
	if err != nil {
		t.Fatal(err)
	}
	// The repository's lowest id wins over the organization's; a missing
	// name is created once.
	if !slices.Equal(ids, []string{"101", "8", "50", "101"}) || !slices.Equal(*created, []string{"conformance-x"}) {
		t.Errorf("EnsureLabels = %v, created %v", ids, *created)
	}
	if ids, err := w.tw.EnsureLabels(t.Context(), nil); err != nil || len(ids) != 0 {
		t.Errorf("EnsureLabels(nil) = %v, %v", ids, err)
	}
	// A user's repository has no organization labels (404): created.
	w2 := newTargetWorld(t, "forgejo")
	created = w2.labels(nil, nil)
	if ids, err := w2.tw.EnsureLabels(t.Context(), []string{"engineering-assets"}); err != nil || !slices.Equal(ids, []string{"101"}) ||
		!slices.Equal(*created, []string{"engineering-assets"}) {
		t.Errorf("EnsureLabels in a user's repository = %v, %v, created %v", ids, err, *created)
	}
	w2.json(http.MethodPost, "/repos/acme/api/labels", http.StatusUnprocessableEntity, w2.apiMsg("invalid color"))
	_, err = w2.tw.EnsureLabels(t.Context(), []string{"new"})
	wantClass(t, "a label the server refuses", err, platform.ClassInvalid, nil)

	// Organization labels this token may not read (403): the repository's
	// label is created, and CreatePR goes on with it.
	w3 := newTargetWorld(t, "gitea")
	created = w3.labels(nil, []map[string]any{})
	w3.json(http.MethodGet, "/orgs/acme/labels", http.StatusForbidden, w3.apiMsg("user should be a member of the organization"))
	var sent createPR
	w3.handle(http.MethodPost, "/repos/acme/api/pulls", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		writeJSON(rw, http.StatusCreated, w3.pr(w3.repo, prSpec{number: 13, author: w3.writer, head: sent.Head,
			labels: []map[string]any{w3.label(101, "engineering-assets")}}))
	})
	pr, err := w3.tw.CreatePR(t.Context(), platform.NewPR{Head: freshBranch, Base: "main", Title: "t", Labels: []string{"engineering-assets"}})
	if err != nil || pr.Number != 13 || !slices.Equal(sent.Labels, []int64{101}) || !slices.Equal(*created, []string{"engineering-assets"}) {
		t.Errorf("CreatePR without organization labels = #%d, %v; sent labels %v, created %v", pr.Number, err, sent.Labels, *created)
	}
	// Any other failure of the organization's labels fails the call.
	w3.json(http.MethodGet, "/orgs/acme/labels", http.StatusBadGateway, w3.apiMsg("bad gateway"))
	_, err = w3.tw.EnsureLabels(t.Context(), []string{"missing"})
	wantClass(t, "organization labels that fail", err, platform.ClassTransient, nil)

	// A label listing that does not end within maxLabelPages fails: the
	// label may be on a page not read, and creating it would make a second
	// label of its name.
	w4 := newTargetWorld(t, "gitea")
	w4.settings(1)
	w4.handle(http.MethodGet, "/repos/acme/api/labels", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("X-Total-Count", "100000")
		writeJSON(rw, http.StatusOK, []any{w4.label(1, "other")})
	})
	_, err = w4.tw.EnsureLabels(t.Context(), []string{"engineering-assets"})
	wantClass(t, "an endless label listing", err, platform.ClassUnknown, errTooManyLabels)
	if n := len(w4.requests(http.MethodPost, "/repos/acme/api/labels")); n != 0 {
		t.Errorf("an endless label listing created %d labels", n)
	}
}
