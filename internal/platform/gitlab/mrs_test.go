package gitlab

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Branch names of the tests, as the conformance suite uses them.
const (
	syncBranch  = "touchmark/conformance"
	aliasBranch = "chore/sync-engineering-assets"
)

// mrWorld is one project with merge requests of every kind the driver must
// tell apart.
type mrWorld struct {
	*fixture
	repo          platform.Repo
	writer, other platform.Account
	mrs           []map[string]any
}

func newMRWorld(t *testing.T) *mrWorld {
	fx := newFixture(t)
	w := &mrWorld{fixture: fx,
		repo:   platform.Repo{Host: fx.provider().Host, ID: "7", Path: "acme/api", DefaultBranch: "main"},
		writer: platform.Account{ID: "3", Login: "service_account_group_1_abc"},
		other:  platform.Account{ID: "4", Login: "jdoe"},
	}
	wr, ot := basic(3, "service_account_group_1_abc"), basic(4, "jdoe")
	bot := basic(21, "project_7_bot_0a1b2c")
	w.mrs = []map[string]any{
		mr(mrSpec{iid: 1, author: wr, source: syncBranch, state: "merged", mergedBy: ot, title: "merged"}),
		mr(mrSpec{iid: 2, author: wr, source: syncBranch, state: "closed", closedBy: ot, title: "declined", closedAt: "2026-09-12T09:30:00.000Z"}),
		mr(mrSpec{iid: 3, author: wr, source: aliasBranch, state: "closed", closedBy: wr, title: "self-closed"}),
		mr(mrSpec{iid: 4, author: ot, source: syncBranch, state: "closed", closedBy: ot, title: "foreign closed"}),
		mr(mrSpec{iid: 5, author: ot, source: "feature/other", title: "foreign elsewhere"}),
		mr(mrSpec{iid: 7, author: wr, source: syncBranch, labels: []string{"engineering-assets"}, title: "open",
			description: "body\n<!-- marker -->", draft: true}),
		mr(mrSpec{iid: 8, author: ot, source: aliasBranch, title: "foreign open"}),
		mr(mrSpec{iid: 9, author: ot, source: syncBranch, sourceProject: 99, title: "fork"}),
		mr(mrSpec{iid: 11, author: wr, source: syncBranch, target: "release", state: "closed", closedBy: basic(1, "tm-admin"), title: "base gone"}),
		mr(mrSpec{iid: 13, author: wr, source: aliasBranch, state: "closed", closedBy: bot, title: "closed by a bot"}),
		mr(mrSpec{iid: 14, author: wr, source: aliasBranch, state: "closed", title: "closed long ago"}),
	}
	w.handle(http.MethodGet, "/projects/7/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var out []any
		for _, m := range w.mrs {
			switch {
			case q.Has("source_branch") && m["source_branch"] != q.Get("source_branch"):
				continue
			case q.Get("state") != "" && q.Get("state") != "all" && m["state"] != q.Get("state"):
				continue
			}
			out = append(out, m)
		}
		slices.Reverse(out) // newest first
		servePage(rw, r, out, offset)
	})
	w.json(http.MethodGet, "/projects/7/repository/branches/release", http.StatusNotFound, msg("404 Branch Not Found"))
	w.json(http.MethodGet, "/users/4", http.StatusOK, user(4, "jdoe", false))
	w.json(http.MethodGet, "/users/3", http.StatusOK, user(3, "service_account_group_1_abc", true))
	w.json(http.MethodGet, "/users/1", http.StatusOK, user(1, "tm-admin", false))
	return w
}

func numbers(prs []platform.PR) []int64 {
	out := make([]int64, len(prs))
	for i, p := range prs {
		out[i] = p.Number
	}
	return out
}

func find(prs []platform.PR, n int64) platform.PR {
	for _, p := range prs {
		if p.Number == n {
			return p
		}
	}
	return platform.PR{}
}

func TestPRs(t *testing.T) {
	w := newMRWorld(t)
	heads := []string{syncBranch, aliasBranch}
	got, err := w.reader.PRs(t.Context(), w.repo, heads, []platform.Account{w.writer})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{14, 13, 11, 9, 8, 7, 3, 2, 1}; !slices.Equal(numbers(got), want) {
		t.Fatalf("PRs = %v, want %v", numbers(got), want)
	}
	for _, c := range w.requests(http.MethodGet, "/projects/7/merge_requests") {
		if c.Query.Get("state") != "all" || !slices.Contains(heads, c.Query.Get("source_branch")) {
			t.Errorf("listing query %v", c.Query)
		}
	}

	merged := find(got, 1)
	if merged.State != platform.Merged || merged.ClosedBy == nil || merged.ClosedBy.ID != "4" || merged.ClosedBy.Kind != platform.KindUser ||
		merged.ClosedAt.IsZero() || !merged.BaseExists || merged.Head != syncBranch || merged.Base != "main" {
		t.Errorf("merged: %+v, closed by %+v", merged, merged.ClosedBy)
	}
	declined := find(got, 2)
	if declined.State != platform.Closed || declined.ClosedBy == nil || declined.ClosedBy.ID != "4" ||
		!declined.ClosedAt.Equal(time.Date(2026, 9, 12, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("declined: %+v", declined)
	}
	if self := find(got, 3); self.ClosedBy == nil || self.ClosedBy.ID != "3" || self.ClosedBy.Kind != platform.KindServiceAccount {
		t.Errorf("self-closed: closed by %+v", self.ClosedBy)
	}
	open := find(got, 7)
	if open.State != platform.Open || !open.Draft || open.Body != "body\n<!-- marker -->" || !slices.Equal(open.Labels, []string{"engineering-assets"}) ||
		open.ClosedBy != nil || !open.ClosedAt.IsZero() || open.RepoID != "7" || open.HeadRepoID != "7" || open.Author.ID != "3" ||
		open.Title != "Draft: open" || open.URL == "" || !isHexOID(open.HeadSHA) || open.CreatedAt.IsZero() {
		t.Errorf("open: %+v", open)
	}
	if fork := find(got, 9); fork.HeadRepoID != "99" || fork.RepoID != "7" || fork.Head != syncBranch {
		t.Errorf("fork: head repo %s repo %s", fork.HeadRepoID, fork.RepoID)
	}
	if gone := find(got, 11); gone.BaseExists || gone.Base != "release" {
		t.Errorf("base gone: BaseExists %v base %s", gone.BaseExists, gone.Base)
	}
	if byBot := find(got, 13); byBot.ClosedBy == nil || byBot.ClosedBy.Kind != platform.KindBot {
		t.Errorf("closed by a bot: %+v", byBot.ClosedBy)
	}
	if old := find(got, 14); old.State != platform.Closed || old.ClosedBy != nil {
		t.Errorf("closed without closed_by: %+v", old.ClosedBy)
	}
	// Each closer is looked up once; the access token's bot not at all.
	for _, id := range []string{"1", "3", "4", "21"} {
		n := len(w.requests(http.MethodGet, "/users/"+id))
		if want := map[string]int{"1": 1, "3": 1, "4": 1, "21": 0}[id]; n != want {
			t.Errorf("GET /users/%s %d times, want %d", id, n, want)
		}
	}
	// The default branch is not asked for.
	if n := len(w.requests(http.MethodGet, "/projects/7/repository/branches/main")); n != 0 {
		t.Errorf("GET branches/main %d times", n)
	}

	got, err = w.reader.PRs(t.Context(), w.repo, heads, []platform.Account{w.writer, w.other})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{14, 13, 11, 9, 8, 7, 4, 3, 2, 1}; !slices.Equal(numbers(got), want) {
		t.Errorf("PRs of two authors = %v, want %v", numbers(got), want)
	}
	got, err = w.reader.PRs(t.Context(), w.repo, heads, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{9, 8, 7}; !slices.Equal(numbers(got), want) {
		t.Errorf("PRs without authors = %v, want %v", numbers(got), want)
	}
	got, err = w.reader.PRs(t.Context(), w.repo, nil, []platform.Account{w.writer})
	if err != nil || len(got) != 0 {
		t.Errorf("PRs without heads = %v, %v", numbers(got), err)
	}
}

// TestPRsLocked: a locked merge request (GitLab is merging it, or the
// merge is stuck) is open, never closed: a closed one without a closer
// would be a decline (decide.ClassifyClose), and the core would ack it on
// a merge in progress. One from a sync branch of the project itself makes
// the listing fail transiently, so the target waits; one from a fork is
// listed as open. Locked was once reported closed.
func TestPRsLocked(t *testing.T) {
	w := newMRWorld(t)
	wr := basic(3, "service_account_group_1_abc")
	w.mrs = append(w.mrs, mr(mrSpec{iid: 12, author: wr, source: aliasBranch, state: "locked", title: "merging"}))
	_, err := w.reader.PRs(t.Context(), w.repo, []string{syncBranch, aliasBranch}, []platform.Account{w.writer})
	wantClass(t, "a locked merge request of the project", err, platform.ClassTransient, nil)
	if err == nil || !strings.Contains(err.Error(), "!12") {
		t.Errorf("the error does not name !12: %v", err)
	}

	w = newMRWorld(t)
	w.mrs = append(w.mrs, mr(mrSpec{iid: 15, author: basic(4, "jdoe"), source: syncBranch, sourceProject: 99, state: "locked"}))
	got, err := w.reader.PRs(t.Context(), w.repo, []string{syncBranch}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if locked := find(got, 15); locked.State != platform.Open || locked.ClosedBy != nil || !locked.ClosedAt.IsZero() {
		t.Errorf("a locked merge request from a fork: %+v", locked)
	}

	// Whatever reaches toPR as locked is open, so memory never sees it.
	m := apiMR{IID: 3, State: "locked", Author: &apiUser{ID: 3, Username: "sa"}, TargetProjectID: 7, SourceProjectID: 7,
		TargetBranch: "main", SourceBranch: syncBranch, CreatedAt: ptr(time.Now())}
	if pr := toPR(&m, true); pr.State != platform.Open || pr.ClosedBy != nil {
		t.Errorf("toPR(locked) = %+v", pr)
	}
}

// TestMRShape: a merge request without what the driver depends on is an
// error of the API's shape, not a merge request.
func TestMRShape(t *testing.T) {
	good := func() apiMR {
		return apiMR{IID: 1, State: "opened", Author: &apiUser{ID: 3}, TargetProjectID: 7, TargetBranch: "main",
			SourceBranch: syncBranch, CreatedAt: ptr(time.Now())}
	}
	if m := good(); m.check("test") != nil {
		t.Fatalf("a whole merge request: %v", m.check("test"))
	}
	for name, edit := range map[string]func(*apiMR){
		"no iid":            func(m *apiMR) { m.IID = 0 },
		"unknown state":     func(m *apiMR) { m.State = "reopened" },
		"no author":         func(m *apiMR) { m.Author = nil },
		"author without id": func(m *apiMR) { m.Author = &apiUser{Username: "x"} },
		"no target project": func(m *apiMR) { m.TargetProjectID = 0 },
		"no source branch":  func(m *apiMR) { m.SourceBranch = "" },
		"no created_at":     func(m *apiMR) { m.CreatedAt = nil },
	} {
		m := good()
		edit(&m)
		err := m.check("test")
		if err == nil || !errors.Is(err, errShape) || platform.ClassOf(err) != platform.ClassUnknown {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPRsManyPages(t *testing.T) {
	w := newMRWorld(t)
	w.mrs = nil
	for i := int64(1); i <= 230; i++ {
		w.mrs = append(w.mrs, mr(mrSpec{iid: i, author: basic(4, "jdoe"), source: syncBranch, state: "closed"}))
	}
	w.mrs = append(w.mrs, mr(mrSpec{iid: 231, author: basic(3, "service_account_group_1_abc"), source: syncBranch, state: "closed"}))
	got, err := w.reader.PRs(t.Context(), w.repo, []string{syncBranch}, []platform.Account{w.writer})
	if err != nil || !slices.Equal(numbers(got), []int64{231}) {
		t.Errorf("PRs over three pages = %v, %v", numbers(got), err)
	}
}

func TestPRsErrors(t *testing.T) {
	w := newMRWorld(t)
	w.json(http.MethodGet, "/projects/7/repository/branches/release", http.StatusServiceUnavailable, msg("503"))
	_, err := w.reader.PRs(t.Context(), w.repo, []string{syncBranch}, []platform.Account{w.writer})
	wantClass(t, "a failed branch check", err, platform.ClassTransient, nil)

	w = newMRWorld(t)
	w.json(http.MethodGet, "/users/4", http.StatusTooManyRequests, msg("Retry later"))
	_, err = w.reader.PRs(t.Context(), w.repo, []string{syncBranch}, []platform.Account{w.writer})
	wantClass(t, "a rate-limited closer lookup", err, platform.ClassRateLimited, nil)

	w = newMRWorld(t)
	w.json(http.MethodGet, "/users/4", http.StatusNotFound, msg("404 User Not Found"))
	got, err := w.reader.PRs(t.Context(), w.repo, []string{syncBranch}, []platform.Account{w.writer})
	if err != nil || find(got, 2).ClosedBy == nil || find(got, 2).ClosedBy.Kind != platform.KindUnknown {
		t.Errorf("a closer that is gone: %v", err)
	}

	w = newMRWorld(t)
	w.json(http.MethodGet, "/projects/7/merge_requests", http.StatusNotFound, msg("404 Project Not Found"))
	_, err = w.reader.PRs(t.Context(), w.repo, []string{syncBranch}, nil)
	wantClass(t, "a missing project", err, platform.ClassNotFound, nil)
}

// sweepWorld serves GET /merge_requests over several projects.
func sweepWorld(t *testing.T) (*mrWorld, []map[string]any) {
	w := newMRWorld(t)
	wr, ot := basic(3, "service_account_group_1_abc"), basic(4, "jdoe")
	all := []map[string]any{
		mr(mrSpec{iid: 7, projectID: 7, author: wr, source: syncBranch}),
		mr(mrSpec{iid: 2, projectID: 10, author: wr, source: aliasBranch, target: "release"}),
		mr(mrSpec{iid: 3, projectID: 10, author: wr, source: "feature/own"}),
		mr(mrSpec{iid: 4, projectID: 10, author: ot, source: aliasBranch}),
		mr(mrSpec{iid: 5, projectID: 8, author: wr, source: syncBranch}), // project gone since
		mr(mrSpec{iid: 6, projectID: 10, author: wr, source: syncBranch, sourceProject: 55}),
	}
	w.handle(http.MethodGet, "/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("scope") != "all" || q.Get("state") != "opened" {
			t.Errorf("sweep query %v", q)
		}
		var out []any
		for _, m := range all {
			if strconv.FormatInt(m["author"].(map[string]any)["id"].(int64), 10) != q.Get("author_id") ||
				m["source_branch"] != q.Get("source_branch") {
				continue
			}
			out = append(out, m)
		}
		servePage(rw, r, out, offset)
	})
	w.json(http.MethodGet, "/projects/7", http.StatusOK, project(7, "acme/api"))
	w.json(http.MethodGet, "/projects/10", http.StatusOK, project(10, "acme/Web"))
	w.json(http.MethodGet, "/projects/8", http.StatusNotFound, msg("404 Project Not Found"))
	w.json(http.MethodGet, "/projects/10/repository/branches/release", http.StatusNotFound, msg("404 Branch Not Found"))
	return w, all
}

func TestOpenPRsBy(t *testing.T) {
	w, _ := sweepWorld(t)
	sw, err := w.reader.OpenPRsBy(t.Context(), []platform.Account{w.writer}, []string{syncBranch, aliasBranch})
	if err != nil || !sw.Complete {
		t.Fatalf("OpenPRsBy = %+v, %v", sw, err)
	}
	var got []string
	for _, rp := range sw.PRs {
		got = append(got, rp.Repo.Path+"!"+strconv.FormatInt(rp.PR.Number, 10))
		if rp.PR.RepoID != rp.Repo.ID || rp.PR.State != platform.Open || rp.PR.Author.ID != "3" {
			t.Errorf("%s!%d: %+v", rp.Repo.Path, rp.PR.Number, rp.PR)
		}
	}
	if want := []string{"acme/api!7", "acme/Web!6", "acme/Web!2"}; !slices.Equal(got, want) {
		t.Errorf("OpenPRsBy = %v, want %v", got, want)
	}
	for _, rp := range sw.PRs {
		switch rp.PR.Number {
		case 2:
			if rp.PR.BaseExists {
				t.Error("!2: BaseExists with its base gone")
			}
		case 6:
			if rp.PR.HeadRepoID != "55" {
				t.Errorf("!6 from a fork: head repo %s", rp.PR.HeadRepoID)
			}
		}
	}
	if n := len(w.requests(http.MethodGet, "/projects/10")); n != 1 {
		t.Errorf("project 10 read %d times", n)
	}

	sw, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{w.writer, w.other}, []string{syncBranch, aliasBranch})
	if err != nil || len(sw.PRs) != 4 {
		t.Errorf("two authors: %d PRs, %v", len(sw.PRs), err)
	}
	sw, err = w.reader.OpenPRsBy(t.Context(), nil, []string{syncBranch})
	if err != nil || !sw.Complete || len(sw.PRs) != 0 {
		t.Errorf("no authors: %+v, %v", sw, err)
	}
}

func TestOpenPRsByFailures(t *testing.T) {
	w, _ := sweepWorld(t)
	w.handle(http.MethodGet, "/merge_requests", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("source_branch") == aliasBranch {
			writeJSON(rw, http.StatusForbidden, msg("403 Forbidden"))
			return
		}
		servePage(rw, r, nil, offset)
	})
	sw, err := w.reader.OpenPRsBy(t.Context(), []platform.Account{w.writer}, []string{syncBranch, aliasBranch})
	if err != nil || sw.Complete {
		t.Errorf("a refused listing: complete %v, %v", sw.Complete, err)
	}

	for _, tc := range []struct {
		name   string
		status int
		class  platform.Class
	}{
		{"rate limit", http.StatusTooManyRequests, platform.ClassRateLimited},
		{"auth", http.StatusUnauthorized, platform.ClassAuth},
		{"transient", http.StatusBadGateway, platform.ClassTransient},
	} {
		w.json(http.MethodGet, "/merge_requests", tc.status, msg("no"))
		_, err := w.reader.OpenPRsBy(t.Context(), []platform.Account{w.writer}, []string{syncBranch})
		wantClass(t, "a sweep with a "+tc.name, err, tc.class, nil)
	}

	w, _ = sweepWorld(t)
	w.json(http.MethodGet, "/projects/10", http.StatusForbidden, msg("403 Forbidden"))
	sw, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{w.writer}, []string{syncBranch, aliasBranch})
	if err != nil || sw.Complete || len(sw.PRs) != 1 {
		t.Errorf("an unreadable project: %d PRs, complete %v, %v", len(sw.PRs), sw.Complete, err)
	}
}
