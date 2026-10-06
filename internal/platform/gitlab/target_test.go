package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// writerWorld is project 7 with the writer (id 3) as Developer and a
// server that keeps merge requests and applies PUTs as GitLab does: the
// state event first (a reopen of a merged MR changes nothing and is no
// error; a reopen of one whose branch is gone opens it, as CE 17.11 and
// 18.11 do, or is refused with 422 as on 19.4 when refuseReopen is set),
// then title, description, target branch and add_labels.
type writerWorld struct {
	*fixture
	repo         platform.Repo
	mrs          map[int64]map[string]any
	branches     map[string]bool
	labels       []string
	refuseReopen bool
}

func newWriterWorld(t *testing.T, access map[string]any) *writerWorld {
	fx := newFixture(t)
	w := &writerWorld{fixture: fx,
		repo:     platform.Repo{Host: fx.provider().Host, ID: "7", Path: "acme/api", DefaultBranch: "main"},
		mrs:      map[int64]map[string]any{},
		branches: map[string]bool{"main": true, syncBranch: true, "develop": true},
		labels:   []string{"engineering-assets", "group-label"},
	}
	fx.json(http.MethodGet, "/user", http.StatusOK, self(3, "service_account_group_1_abc", true))
	if access == nil {
		access = map[string]any{"project_access": map[string]any{"access_level": 30, "notification_level": 3}, "group_access": nil}
	}
	fx.json(http.MethodGet, "/projects/7", http.StatusOK, project(7, "acme/api", with("permissions", access)))
	fx.handle(http.MethodGet, "/projects/7/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var out []any
		for _, iid := range w.iids() {
			m := w.mrs[iid]
			if q.Has("source_branch") && m["source_branch"] != q.Get("source_branch") ||
				q.Get("state") != "" && q.Get("state") != "all" && m["state"] != q.Get("state") {
				continue
			}
			out = append(out, m)
		}
		servePage(rw, r, out, offset)
	})
	fx.handle(http.MethodPost, "/projects/7/merge_requests", w.create)
	for _, b := range []string{"main", syncBranch, "develop", "gone", "release"} {
		fx.handle(http.MethodGet, "/projects/7/repository/branches/"+escape(b), func(rw http.ResponseWriter, _ *http.Request) {
			if w.branches[b] {
				writeJSON(rw, http.StatusOK, map[string]any{"name": b, "protected": false, "developers_can_push": false})
				return
			}
			writeJSON(rw, http.StatusNotFound, msg("404 Branch Not Found"))
		})
	}
	return w
}

func (w *writerWorld) iids() []int64 {
	var out []int64
	for iid := range w.mrs {
		out = append(out, iid)
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

// add stores an MR and declares its routes.
func (w *writerWorld) add(m map[string]any) {
	iid := m["iid"].(int64)
	w.mrs[iid] = m
	p := "/projects/7/merge_requests/" + strconv.FormatInt(iid, 10)
	w.handle(http.MethodGet, p, func(rw http.ResponseWriter, _ *http.Request) { writeJSON(rw, http.StatusOK, w.mrs[iid]) })
	w.handle(http.MethodPut, p, func(rw http.ResponseWriter, r *http.Request) { w.put(rw, r, iid) })
	w.handle(http.MethodPost, p+"/notes", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, http.StatusCreated, map[string]any{"id": 900, "body": decodeBody(r)["body"], "author": basic(3, "sa")})
	})
}

func decodeBody(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

func (w *writerWorld) create(rw http.ResponseWriter, r *http.Request) {
	b := decodeBody(r)
	for _, iid := range w.iids() {
		m := w.mrs[iid]
		if m["state"] == "opened" && m["source_branch"] == b["source_branch"] && m["target_branch"] == b["target_branch"] &&
			m["source_project_id"] == int64(7) {
			writeJSON(rw, http.StatusConflict, msg([]string{"Another open merge request already exists for this source branch: !" + strconv.FormatInt(iid, 10)}))
			return
		}
	}
	iid := int64(len(w.mrs) + 1)
	var labels []string
	if s, _ := b["labels"].(string); s != "" {
		labels = strings.Split(s, ",")
	}
	title := b["title"].(string)
	m := mr(mrSpec{iid: iid, author: basic(3, "service_account_group_1_abc"), source: b["source_branch"].(string),
		target: b["target_branch"].(string), title: title, description: b["description"].(string), labels: labels})
	m["draft"] = hasDraftPrefix(title)
	w.add(m)
	writeJSON(rw, http.StatusCreated, m)
}

func (w *writerWorld) put(rw http.ResponseWriter, r *http.Request, iid int64) {
	b := decodeBody(r)
	m := w.mrs[iid]
	switch b["state_event"] {
	case "close":
		if m["state"] == "opened" {
			m["state"], m["closed_by"], m["closed_at"] = "closed", basic(3, "service_account_group_1_abc"), "2026-09-20T10:00:00.000Z"
		}
	case "reopen":
		if m["state"] != "closed" {
			break
		}
		if w.refuseReopen && (!w.branches[m["source_branch"].(string)] || !w.branches[m["target_branch"].(string)]) {
			writeJSON(rw, http.StatusUnprocessableEntity, msg(map[string]any{"base": []string{"Cannot be reopened: the source branch does not exist"}}))
			return
		}
		m["state"], m["closed_by"], m["closed_at"] = "opened", nil, nil
	}
	if v, ok := b["title"].(string); ok {
		m["title"] = v
		m["draft"] = hasDraftPrefix(v)
	}
	if v, ok := b["description"].(string); ok {
		m["description"] = v
	}
	if v, ok := b["target_branch"].(string); ok {
		m["target_branch"] = v
	}
	if v, ok := b["add_labels"].(string); ok {
		labels := slices.Clone(m["labels"].([]string))
		for _, l := range strings.Split(v, ",") {
			if !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
		m["labels"] = labels
	}
	writeJSON(rw, http.StatusOK, m)
}

func (w *writerWorld) target(t *testing.T) platform.TargetWriter {
	t.Helper()
	tw, err := w.writer.Target(t.Context(), w.repo, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	return tw
}

func newPR() platform.NewPR {
	return platform.NewPR{Head: syncBranch, Base: "main", Title: "chore: sync engineering assets",
		Body: "Sync ⚠\n\n| a | b |\n\n<!-- touchmark:v1 hub=x -->", Labels: []string{"engineering-assets", "new-label"}}
}

func TestTargetAccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		access  map[string]any
		member  int // members/all answer, 0 for 404
		wantErr platform.Class
		rule    string
	}{
		{"project Developer", map[string]any{"project_access": map[string]any{"access_level": 30}, "group_access": nil}, -1, 0, ""},
		{"group Maintainer", map[string]any{"project_access": nil, "group_access": map[string]any{"access_level": 40}}, -1, 0, ""},
		{"inherited Developer", map[string]any{"project_access": nil, "group_access": nil}, 30, 0, ""},
		{"Reporter", map[string]any{"project_access": map[string]any{"access_level": 20}, "group_access": nil}, 20, platform.ClassPermission, "contents"},
		{"no member", map[string]any{"project_access": nil, "group_access": nil}, 0, platform.ClassPermission, "contents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWriterWorld(t, tc.access)
			switch {
			case tc.member > 0:
				w.json(http.MethodGet, "/projects/7/members/all/3", http.StatusOK, map[string]any{"id": 3, "username": "sa", "access_level": tc.member})
			case tc.member == 0:
				w.json(http.MethodGet, "/projects/7/members/all/3", http.StatusNotFound, msg("404 Not found"))
			}
			tw, err := w.writer.Target(t.Context(), w.repo, platform.Perms{Contents: true, PRs: true, Workflows: true})
			if tc.wantErr == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if tw.Remote().URL != w.base()+"/acme/api.git" {
					t.Errorf("remote %q", tw.Remote().URL)
				}
				return
			}
			wantClass(t, "Target", err, tc.wantErr, nil)
			var pe *platform.Error
			if !errors.As(err, &pe) || pe.Rule != tc.rule {
				t.Errorf("rule of %v, want %q", err, tc.rule)
			}
		})
	}
	w := newWriterWorld(t, nil)
	w.json(http.MethodGet, "/projects/8", http.StatusNotFound, msg("404 Project Not Found"))
	_, err := w.writer.Target(t.Context(), platform.Repo{Host: w.repo.Host, ID: "8", Path: "acme/hidden"}, platform.Perms{Contents: true})
	wantClass(t, "Target of a hidden project", err, platform.ClassNotFound, platform.ErrNotFound)
}

func TestTargetClose(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.add(mr(mrSpec{iid: 1, author: basic(3, "sa"), source: syncBranch}))
	tw := w.target(t)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	w.reset()
	_, err := tw.CreatePR(t.Context(), newPR())
	wantClass(t, "CreatePR after Close", err, platform.ClassAuth, nil)
	_, err = tw.EditPR(t.Context(), 1, platform.PREdit{Body: ptr("x")})
	wantClass(t, "EditPR after Close", err, platform.ClassAuth, nil)
	wantClass(t, "Comment after Close", tw.Comment(t.Context(), 1, "x"), platform.ClassAuth, nil)
	_, err = tw.EnsureLabels(t.Context(), []string{"x"})
	wantClass(t, "EnsureLabels after Close", err, platform.ClassAuth, nil)
	_, err = tw.Remote().Header(t.Context())
	wantClass(t, "the git header after Close", err, platform.ClassAuth, nil)
	if n := len(w.requests("", "")); n != 0 {
		t.Errorf("%d requests after Close", n)
	}
}

func TestCreatePR(t *testing.T) {
	w := newWriterWorld(t, nil)
	tw := w.target(t)
	pr, err := tw.CreatePR(t.Context(), newPR())
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 1 || pr.State != platform.Open || pr.Draft || pr.Head != syncBranch || pr.Base != "main" ||
		pr.Body != newPR().Body || !slices.Equal(pr.Labels, []string{"engineering-assets", "new-label"}) || pr.HeadRepoID != "7" {
		t.Errorf("CreatePR = %+v", pr)
	}
	posts := w.requests(http.MethodPost, "/projects/7/merge_requests")
	body := decode(t, posts[0].Body)
	if body["labels"] != "engineering-assets,new-label" || body["remove_source_branch"] != false || body["squash"] != false ||
		body["title"] != "chore: sync engineering assets" || body["description"] != newPR().Body || body["source_branch"] != syncBranch ||
		body["target_branch"] != "main" {
		t.Errorf("POST body %v", body)
	}
	if _, ok := body["target_project_id"]; ok {
		t.Error("the MR names a target project")
	}

	// The open MR from the head comes back with ErrExists, whatever the
	// base, before anything is written.
	w.reset()
	for _, base := range []string{"main", "develop"} {
		np := newPR()
		np.Base = base
		dup, err := tw.CreatePR(t.Context(), np)
		wantClass(t, "CreatePR again to "+base, err, platform.ClassConflict, platform.ErrExists)
		if dup.Number != 1 {
			t.Errorf("CreatePR again returned !%d", dup.Number)
		}
	}
	if n := len(w.writes()); n != 0 {
		t.Errorf("%d writes for a duplicate", n)
	}
}

func TestCreatePRDraftAndGuard(t *testing.T) {
	w := newWriterWorld(t, nil)
	tw := w.target(t)
	np := newPR()
	np.Draft = true
	pr, err := tw.CreatePR(t.Context(), np)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Draft || pr.Title != "Draft: chore: sync engineering assets" {
		t.Errorf("draft: %v %q", pr.Draft, pr.Title)
	}

	w.reset()
	for _, body := range []string{"text\n/merge", "  /close", "a\r/approve", "\u200b/label ~x", "x\u2028/assign @me"} {
		np := newPR()
		np.Body = body
		_, err := tw.CreatePR(t.Context(), np)
		wantClass(t, "CreatePR with a quick action", err, platform.ClassInvalid, nil)
	}
	for _, title := range []string{"Draft: x", "[draft] x", "(Draft) x"} {
		np := newPR()
		np.Title = title
		_, err := tw.CreatePR(t.Context(), np)
		wantClass(t, "a ready MR titled "+title, err, platform.ClassInvalid, nil)
	}
	for _, labels := range [][]string{{"a,b"}, {" "}} {
		np := newPR()
		np.Labels = labels
		_, err := tw.CreatePR(t.Context(), np)
		wantClass(t, "labels", err, platform.ClassInvalid, nil)
	}
	if n := len(w.requests("", "")); n != 0 {
		t.Errorf("%d requests for refused MRs", n)
	}
	// A path in a code span or a URL is no quick action.
	np = newPR()
	np.Head = "develop"
	np.Body = "see `/merge` and https://x/y\n  - /path in a list is not at the start: - comes first"
	if _, err := tw.CreatePR(t.Context(), np); err != nil {
		t.Errorf("a body without quick actions: %v", err)
	}
}

// TestCreatePRBesideFork: an MR from a fork's branch of the same name is
// no duplicate.
func TestCreatePRBesideFork(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.add(mr(mrSpec{iid: 1, author: basic(4, "jdoe"), source: syncBranch, sourceProject: 99}))
	tw := w.target(t)
	pr, err := tw.CreatePR(t.Context(), newPR())
	if err != nil || pr.Number != 2 {
		t.Errorf("CreatePR beside a fork = !%d, %v", pr.Number, err)
	}
}

// TestCreatePRRace: a duplicate opened between the check and the POST
// comes back from the 409 with ErrExists.
func TestCreatePRRace(t *testing.T) {
	w := newWriterWorld(t, nil)
	tw := w.target(t)
	raced := false
	w.handle(http.MethodGet, "/projects/7/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
		if !raced {
			// The pre-check sees nothing; then someone opens !5.
			raced = true
			servePage(rw, r, nil, offset)
			w.add(mr(mrSpec{iid: 5, author: basic(3, "sa"), source: syncBranch}))
			return
		}
		var out []any
		for _, iid := range w.iids() {
			out = append(out, w.mrs[iid])
		}
		servePage(rw, r, out, offset)
	})
	pr, err := tw.CreatePR(t.Context(), newPR())
	wantClass(t, "CreatePR in a race", err, platform.ClassConflict, platform.ErrExists)
	if pr.Number != 5 || statusOf(err) != http.StatusConflict {
		t.Errorf("CreatePR in a race = !%d, status %d", pr.Number, statusOf(err))
	}
	if n := len(w.requests(http.MethodGet, "/projects/7/merge_requests/5")); n != 1 {
		t.Errorf("!5 read %d times", n)
	}
}

// TestCreatePRUnregisteredPush: GitLab answers 400 "source_branch does not
// exist" for a branch pushed a moment ago while its branch cache lags the
// push. When the branches API (Gitaly) finds the branch, CreatePR says so
// at once, with ClassTransient, platform.ErrNotYet, the 400 and a
// RetryAfter, after one POST: the driver waits for nothing (the core sends
// it again), and a later call opens the merge request once
// GitLab registered the push. When the branch does not exist, the 400
// stands.
func TestCreatePRUnregisteredPush(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exists bool
		refuse int // 400s before GitLab takes the branch
		want   platform.Class
		calls  int // CreatePR calls until the answer stops being ErrNotYet
	}{
		{"registered after two refusals", true, 2, 0, 3},
		{"really missing", false, 100, platform.ClassInvalid, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWriterWorld(t, nil)
			w.branches[syncBranch] = tc.exists
			refused := 0
			w.handle(http.MethodPost, "/projects/7/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
				if refused < tc.refuse {
					refused++
					writeJSON(rw, http.StatusBadRequest, msg(map[string]any{"source_branch": []string{"does not exist"}}))
					return
				}
				w.create(rw, r)
			})
			tw := w.target(t)
			var pr platform.PR
			var err error
			calls := 0
			for calls < 10 {
				calls++
				pr, err = tw.CreatePR(t.Context(), newPR())
				if !errors.Is(err, platform.ErrNotYet) {
					break
				}
				wantClass(t, "CreatePR before GitLab registered the push", err, platform.ClassTransient, nil)
				var pe *platform.Error
				if !errors.As(err, &pe) || pe.RetryAfter != pushRetry || statusOf(err) != http.StatusBadRequest ||
					!strings.Contains(err.Error(), "source_branch does not exist") {
					t.Errorf("CreatePR: %v (status %d), want the 400 kept with a RetryAfter of %v", err, statusOf(err), pushRetry)
				}
			}
			if calls != tc.calls {
				t.Errorf("%d calls, want %d", calls, tc.calls)
			}
			if tc.want == 0 {
				if err != nil || pr.Number != 1 {
					t.Fatalf("CreatePR = !%d, %v", pr.Number, err)
				}
			} else {
				wantClass(t, "CreatePR", err, tc.want, nil)
				if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "source_branch does not exist") {
					t.Errorf("CreatePR: %v (status %d), want the 400 kept", err, statusOf(err))
				}
			}
			if n := len(w.requests(http.MethodPost, "/projects/7/merge_requests")); n != calls {
				t.Errorf("%d POSTs in %d calls: the driver sent the POST again itself", n, calls)
			}
			lookups := min(tc.refuse, calls)
			if n := len(w.requests(http.MethodGet, "/projects/7/repository/branches/"+escape(syncBranch))); n != lookups {
				t.Errorf("the branch was looked up %d times, want %d", n, lookups)
			}
		})
	}
}

// TestCreatePRBranchLookupFails: after the 400 of an unregistered push, a
// branch lookup that is rate limited, refused or fails transiently returns
// that failure, so the core pauses the provider or retries; one that fails
// for good leaves the 400. Every lookup failure once became the 400's
// ClassInvalid.
func TestCreatePRBranchLookupFails(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   platform.Class
	}{
		{http.StatusTooManyRequests, platform.ClassRateLimited},
		{http.StatusUnauthorized, platform.ClassAuth},
		{http.StatusBadGateway, platform.ClassTransient},
		{http.StatusForbidden, platform.ClassInvalid},
	} {
		w := newWriterWorld(t, nil)
		w.json(http.MethodPost, "/projects/7/merge_requests", http.StatusBadRequest, msg(map[string]any{"source_branch": []string{"does not exist"}}))
		tw := w.target(t)
		w.json(http.MethodGet, "/projects/7/repository/branches/"+escape(syncBranch), tc.status, msg("no"))
		_, err := tw.CreatePR(t.Context(), newPR())
		wantClass(t, fmt.Sprintf("a branch lookup with %d", tc.status), err, tc.want, nil)
		if err == nil || !strings.Contains(err.Error(), "source_branch does not exist") {
			t.Errorf("%d: %v does not keep the 400", tc.status, err)
		}
		if n := len(w.requests(http.MethodPost, "/projects/7/merge_requests")); n != 1 {
			t.Errorf("%d: %d POSTs, want 1", tc.status, n)
		}
	}
}

// TestCreatePRConflictPaths: how a 409 of CreatePR is resolved when !N is
// not the open MR from the head (closed meanwhile, gone, unreadable), when
// no open MR is found, and when the open one targets a branch that is gone.
func TestCreatePRConflictPaths(t *testing.T) {
	conflict409 := func(w *writerWorld, n int) {
		w.json(http.MethodPost, "/projects/7/merge_requests", http.StatusConflict,
			msg([]string{"Another open merge request already exists for this source branch: !" + strconv.Itoa(n)}))
	}
	// listAfter serves the open-MR listing empty for the pre-check and the
	// open MRs of w from the second listing on.
	listAfter := func(w *writerWorld) {
		n := 0
		w.handle(http.MethodGet, "/projects/7/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
			n++
			var out []any
			if n > 1 {
				for _, iid := range w.iids() {
					if w.mrs[iid]["state"] == "opened" {
						out = append(out, w.mrs[iid])
					}
				}
			}
			servePage(rw, r, out, offset)
		})
	}
	t.Run("!N closed meanwhile, another open", func(t *testing.T) {
		w := newWriterWorld(t, nil)
		tw := w.target(t)
		w.add(mr(mrSpec{iid: 5, author: basic(3, "sa"), source: syncBranch, state: "closed"}))
		w.add(mr(mrSpec{iid: 6, author: basic(3, "sa"), source: syncBranch}))
		listAfter(w)
		conflict409(w, 5)
		pr, err := tw.CreatePR(t.Context(), newPR())
		wantClass(t, "CreatePR", err, platform.ClassConflict, platform.ErrExists)
		if pr.Number != 6 || statusOf(err) != http.StatusConflict {
			t.Errorf("CreatePR = !%d, status %d; want !6 from the listing", pr.Number, statusOf(err))
		}
	})
	t.Run("!N gone, none open", func(t *testing.T) {
		w := newWriterWorld(t, nil)
		tw := w.target(t)
		w.json(http.MethodGet, "/projects/7/merge_requests/5", http.StatusNotFound, msg("404 Not found"))
		listAfter(w)
		conflict409(w, 5)
		pr, err := tw.CreatePR(t.Context(), newPR())
		wantClass(t, "CreatePR", err, platform.ClassConflict, nil)
		if errors.Is(err, platform.ErrExists) || pr.Number != 0 {
			t.Errorf("CreatePR = !%d, %v; want the 409 alone", pr.Number, err)
		}
	})
	t.Run("!N unreadable", func(t *testing.T) {
		w := newWriterWorld(t, nil)
		tw := w.target(t)
		w.json(http.MethodGet, "/projects/7/merge_requests/5", http.StatusInternalServerError, msg("500 Internal Server Error"))
		conflict409(w, 5)
		_, err := tw.CreatePR(t.Context(), newPR())
		if err == nil || errors.Is(err, platform.ErrExists) || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "!5") {
			t.Errorf("CreatePR: %v; want the 409 joined with the failed read", err)
		}
	})
	t.Run("open to a base that is gone", func(t *testing.T) {
		w := newWriterWorld(t, nil)
		tw := w.target(t)
		w.add(mr(mrSpec{iid: 7, author: basic(3, "sa"), source: syncBranch, target: "release"}))
		pr, err := tw.CreatePR(t.Context(), newPR())
		wantClass(t, "CreatePR", err, platform.ClassConflict, platform.ErrExists)
		if pr.Number != 7 || pr.BaseExists || pr.Base != "release" {
			t.Errorf("CreatePR = %+v; want !7 on release, BaseExists false", pr)
		}
		w.branches["release"] = true
		if pr, _ := tw.CreatePR(t.Context(), newPR()); !pr.BaseExists {
			t.Errorf("CreatePR = %+v; want BaseExists", pr)
		}
	})
}

func TestCreatePRDisabled(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.json(http.MethodGet, "/projects/7", http.StatusOK, project(7, "acme/api", with("merge_requests_access_level", "disabled"),
		with("permissions", map[string]any{"project_access": map[string]any{"access_level": 30}})))
	tw := w.target(t)
	_, err := tw.CreatePR(t.Context(), newPR())
	wantClass(t, "CreatePR with MRs disabled", err, platform.ClassPolicy, nil)
}

func TestEditPR(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.add(mr(mrSpec{iid: 1, author: basic(3, "sa"), source: syncBranch, labels: []string{"engineering-assets", "human"}, draft: true, title: "sync"}))
	tw := w.target(t)
	w.reset()
	body := "closed by touchmark\n\n<!-- touchmark:v1 closed -->"
	pr, err := tw.EditPR(t.Context(), 1, platform.PREdit{Title: ptr("chore: new"), Body: &body, State: ptr(platform.Closed),
		Base: ptr("develop"), AddLabels: []string{"extra"}})
	if err != nil {
		t.Fatal(err)
	}
	puts := w.requests(http.MethodPut, "/projects/7/merge_requests/1")
	if len(puts) != 1 {
		t.Fatalf("%d PUTs, want one", len(puts))
	}
	got := decode(t, puts[0].Body)
	if got["title"] != "Draft: chore: new" || got["description"] != body || got["state_event"] != "close" ||
		got["target_branch"] != "develop" || got["add_labels"] != "extra" {
		t.Errorf("PUT %v", got)
	}
	if _, ok := got["labels"]; ok {
		t.Error("the PUT replaces labels")
	}
	if pr.State != platform.Closed || pr.Body != body || pr.Base != "develop" || !pr.Draft || pr.Title != "Draft: chore: new" ||
		!slices.Equal(pr.Labels, []string{"engineering-assets", "human", "extra"}) || pr.ClosedBy == nil || pr.ClosedBy.ID != "3" ||
		pr.ClosedBy.Kind != platform.KindServiceAccount || !pr.BaseExists {
		t.Errorf("EditPR = %+v", pr)
	}

	// Reopen: a PUT of its own first, then the rest.
	w.reset()
	pr, err = tw.EditPR(t.Context(), 1, platform.PREdit{State: ptr(platform.Open), Body: ptr("reopened")})
	if err != nil || pr.State != platform.Open || pr.Body != "reopened" || pr.ClosedBy != nil {
		t.Fatalf("reopen = %+v, %v", pr, err)
	}
	puts = w.requests(http.MethodPut, "/projects/7/merge_requests/1")
	if len(puts) != 2 || decode(t, puts[0].Body)["state_event"] != "reopen" || len(decode(t, puts[0].Body)) != 1 {
		t.Errorf("reopen PUTs %v", puts)
	}

	// Nothing to change: no write.
	w.reset()
	if _, err := tw.EditPR(t.Context(), 1, platform.PREdit{State: ptr(platform.Open)}); err != nil {
		t.Fatal(err)
	}
	if n := len(w.writes()); n != 0 {
		t.Errorf("%d writes for no change", n)
	}
}

// TestEditPRRefused: what GitLab would refuse silently, or apply only in
// part, is refused before anything is written.
func TestEditPRRefused(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.add(mr(mrSpec{iid: 1, author: basic(3, "sa"), source: syncBranch, state: "merged", mergedBy: basic(4, "jdoe")}))
	w.add(mr(mrSpec{iid: 2, author: basic(3, "sa"), source: "gone", state: "closed", closedBy: basic(4, "jdoe")}))
	w.add(mr(mrSpec{iid: 3, author: basic(3, "sa"), source: syncBranch}))
	w.branches["gone"] = false
	tw := w.target(t)
	w.reset()
	edit := platform.PREdit{Title: ptr("t"), Body: ptr("b"), AddLabels: []string{"l"}}

	e := edit
	e.State = ptr(platform.Open)
	_, err := tw.EditPR(t.Context(), 1, e)
	wantClass(t, "reopening a merged MR", err, platform.ClassConflict, nil)
	e.State = ptr(platform.Closed)
	_, err = tw.EditPR(t.Context(), 1, e)
	wantClass(t, "closing a merged MR", err, platform.ClassConflict, nil)
	e = edit
	e.Base = ptr("release")
	_, err = tw.EditPR(t.Context(), 3, e)
	wantClass(t, "a missing base", err, platform.ClassInvalid, nil)
	e = edit
	e.Body = ptr("ok\n/merge")
	_, err = tw.EditPR(t.Context(), 3, e)
	wantClass(t, "a quick action", err, platform.ClassInvalid, nil)
	_, err = tw.EditPR(t.Context(), 3, platform.PREdit{Title: ptr("Draft: now a draft")})
	wantClass(t, "a draft prefix for a ready MR", err, platform.ClassInvalid, nil)
	if n := len(w.writes()); n != 0 {
		t.Errorf("%d writes for refused edits", n)
	}

	// An MR whose source branch is gone is not reopened: CE 17.11 and
	// 18.11 would open it without a branch, 19.4 refuses with 422 (live,
	// TestFacts reopen-without-branch). Nothing is sent, on either.
	for _, refuse := range []bool{false, true} {
		w.refuseReopen = refuse
		e = edit
		e.State = ptr(platform.Open)
		_, err = tw.EditPR(t.Context(), 2, e)
		wantClass(t, "a reopen without the source branch", err, platform.ClassConflict, nil)
		if puts := w.requests(http.MethodPut, "/projects/7/merge_requests/2"); len(puts) != 0 {
			t.Errorf("PUTs %v", puts)
		}
		if w.mrs[2]["state"] != "closed" || w.mrs[2]["title"] == "t" || w.mrs[2]["description"] == "b" {
			t.Error("the refused reopen changed the MR")
		}
	}

	// A reopen GitLab refuses after the check (19.4's 422; a branch
	// deleted in the meantime): a conflict, and the rest is not sent.
	w.add(mr(mrSpec{iid: 4, author: basic(3, "sa"), source: syncBranch, state: "closed", closedBy: basic(4, "jdoe")}))
	w.json(http.MethodPut, "/projects/7/merge_requests/4", http.StatusUnprocessableEntity, msg(map[string]any{"base": []string{"Cannot be reopened"}}))
	_, err = tw.EditPR(t.Context(), 4, e)
	wantClass(t, "a reopen refused with 422", err, platform.ClassConflict, nil)
	if puts := w.requests(http.MethodPut, "/projects/7/merge_requests/4"); len(puts) != 1 || len(decode(t, puts[0].Body)) != 1 ||
		statusOf(err) != http.StatusUnprocessableEntity {
		t.Errorf("PUTs %v, status %d", puts, statusOf(err))
	}

	// A locked MR (GitLab is merging it) takes no edit at all, not even of
	// the body alone. A body edit once went through.
	w.reset()
	w.add(mr(mrSpec{iid: 5, author: basic(3, "sa"), source: syncBranch, state: "locked"}))
	for _, le := range []platform.PREdit{{Body: ptr("ack")}, {State: ptr(platform.Closed)}, {AddLabels: []string{"l"}}} {
		_, err = tw.EditPR(t.Context(), 5, le)
		wantClass(t, "an edit of a locked MR", err, platform.ClassConflict, nil)
	}
	if n := len(w.writes()); n != 0 {
		t.Errorf("%d writes to a locked MR", n)
	}

	w.json(http.MethodGet, "/projects/7/merge_requests/99", http.StatusNotFound, msg("404 Not found"))
	_, err = tw.EditPR(t.Context(), 99, platform.PREdit{Body: ptr("x")})
	wantClass(t, "EditPR of a missing MR", err, platform.ClassNotFound, platform.ErrNotFound)
}

func TestComment(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.add(mr(mrSpec{iid: 1, author: basic(3, "sa"), source: syncBranch}))
	tw := w.target(t)
	body := "touchmark remembers ⚠\n\n```yaml\nignore:\n  - AGENTS.md\n```"
	if err := tw.Comment(t.Context(), 1, body); err != nil {
		t.Fatal(err)
	}
	posts := w.requests(http.MethodPost, "/projects/7/merge_requests/1/notes")
	if len(posts) != 1 || decode(t, posts[0].Body)["body"] != body {
		t.Errorf("notes %v", posts)
	}
	wantClass(t, "a comment with a quick action", tw.Comment(t.Context(), 1, "fine\n /unassign"), platform.ClassInvalid, nil)
	wantClass(t, "an empty comment", tw.Comment(t.Context(), 1, " "), platform.ClassInvalid, nil)
	w.json(http.MethodPost, "/projects/7/merge_requests/99/notes", http.StatusNotFound, msg("404 Not found"))
	wantClass(t, "a comment on a missing MR", tw.Comment(t.Context(), 99, "x"), platform.ClassNotFound, platform.ErrNotFound)
	w.json(http.MethodPost, "/projects/7/merge_requests/1/notes", http.StatusTooManyRequests, msg("Retry later"))
	wantClass(t, "a comment over the notes limit", tw.Comment(t.Context(), 1, "x"), platform.ClassRateLimited, nil)
	if n := len(w.requests(http.MethodPost, "/projects/7/merge_requests/1/notes")); n != 2 {
		t.Errorf("%d notes sent", n)
	}
}

func TestEnsureLabels(t *testing.T) {
	w := newWriterWorld(t, nil)
	w.pages("/projects/7/labels", []any{
		map[string]any{"id": 1, "name": "engineering-assets", "color": "#ededed", "is_project_label": true},
		map[string]any{"id": 2, "name": "group-label", "color": "#ededed", "is_project_label": false},
	})
	created := 0
	w.handle(http.MethodPost, "/projects/7/labels", func(rw http.ResponseWriter, r *http.Request) {
		created++
		if decodeBody(r)["name"] == "raced" {
			writeJSON(rw, http.StatusConflict, msg("Label already exists"))
			return
		}
		writeJSON(rw, http.StatusCreated, map[string]any{"id": 10 + created, "name": "x"})
	})
	tw := w.target(t)
	got, err := tw.EnsureLabels(t.Context(), []string{"new", "engineering-assets", "group-label", "raced"})
	if err != nil || !slices.Equal(got, []string{"new", "engineering-assets", "group-label", "raced"}) {
		t.Errorf("EnsureLabels = %v, %v", got, err)
	}
	if created != 2 {
		t.Errorf("%d labels created, want 2", created)
	}
	if q := w.requests(http.MethodGet, "/projects/7/labels")[0].Query; q.Get("include_ancestor_groups") != "true" {
		t.Errorf("labels query %v", q)
	}
	_, err = tw.EnsureLabels(t.Context(), []string{"a,b"})
	wantClass(t, "a label with a comma", err, platform.ClassInvalid, nil)
}

func TestCheckText(t *testing.T) {
	for text, ok := range map[string]bool{
		"":                         true,
		"plain":                    true,
		"a/b\nc / d":               true,
		"`/merge`":                 true,
		"\\/merge":                 true,
		"/merge":                   false,
		"x\n\t/close":              false,
		"x\r\n/close":              false,
		"x\r/close":                false,
		"x\u0085/close":            false,
		"x\u2029/close":            false,
		"\ufeff/close":             false,
		"x\n\u00a0\u2060/label ~y": false,
		"> /quoted":                true,
	} {
		err := checkText("op", "text", text)
		if (err == nil) != ok {
			t.Errorf("checkText(%q) = %v", text, err)
		}
	}
}
