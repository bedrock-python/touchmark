package gitea

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Branch names of the tests, as the conformance suite uses them.
const (
	syncBranch  = "touchmark/conformance"
	aliasBranch = "chore/sync-engineering-assets"
)

// prWorld is one repository with pull requests of every kind the driver
// must tell apart, served as Gitea or Forgejo serves them.
type prWorld struct {
	*fixture
	repo                 map[string]any
	target               platform.Repo
	writer, other, ghost map[string]any
	prs                  []map[string]any
	timelines            map[int64][]any
}

func newPRWorld(t *testing.T, typ string) *prWorld {
	fx := newFixture(t, typ)
	fx.settings(50)
	w := &prWorld{fixture: fx, repo: fx.repo(7, "acme/api"), writer: fx.user(3, "tm-writer"), other: fx.user(4, "jdoe"),
		ghost: map[string]any{"id": -1, "login": "Ghost"}, timelines: map[int64][]any{}}
	w.target = platform.Repo{Host: fx.provider(typ).Host, ID: "7", Path: "acme/api", DefaultBranch: "main"}
	labels := []map[string]any{fx.label(1, "engineering-assets")}
	gone := ""
	w.prs = []map[string]any{
		// #1 merged by other; its head branch deleted since.
		fx.pr(w.repo, prSpec{number: 1, author: w.writer, head: syncBranch, headRef: "refs/pull/1/head", state: "merged", mergedBy: w.other, title: "merged"}),
		// #2 declined: closed by other, reopened and closed again by the writer.
		fx.pr(w.repo, prSpec{number: 2, author: w.writer, head: syncBranch, state: "closed", closedAt: "2026-09-12T09:30:00Z", title: "declined"}),
		// #3 closed by the writer on the alias.
		fx.pr(w.repo, prSpec{number: 3, author: w.writer, head: aliasBranch, state: "closed", title: "self-closed"}),
		// #4 someone else's, closed: not listed.
		fx.pr(w.repo, prSpec{number: 4, author: w.other, head: syncBranch, state: "closed", title: "foreign closed"}),
		// #5 someone else's on another branch: not listed.
		fx.pr(w.repo, prSpec{number: 5, author: w.other, head: "feature/other", title: "foreign elsewhere"}),
		// #6 the writer's on another branch: not listed.
		fx.pr(w.repo, prSpec{number: 6, author: w.writer, head: "feature/own", title: "own elsewhere"}),
		// #7 the writer's open one, with a label.
		fx.pr(w.repo, prSpec{number: 7, author: w.writer, head: syncBranch, labels: labels, title: "open", body: "body\n<!-- marker -->"}),
		// #8 someone else's open one on the alias.
		fx.pr(w.repo, prSpec{number: 8, author: w.other, head: aliasBranch, title: "foreign open"}),
		// #9 an open one from a fork, with the sync branch's name.
		fx.pr(w.repo, prSpec{number: 9, author: w.other, head: syncBranch, headRepoID: 9, title: "fork"}),
		// #10 an AGit pull request by a user named like the branch prefix.
		fx.pr(w.repo, prSpec{number: 10, author: fx.user(5, "touchmark"), head: "", title: "agit"}),
		// #11 the writer's, closed when its base branch was deleted (Forgejo).
		fx.pr(w.repo, prSpec{number: 11, author: w.writer, head: syncBranch, base: "release", baseSHA: &gone, state: "closed", title: "base gone"}),
		// #12 the writer's, closed by a user deleted since.
		fx.pr(w.repo, prSpec{number: 12, author: w.writer, head: aliasBranch, state: "closed", title: "ghost"}),
		// #13 an AGit pull request as Forgejo 16 shows it: user/topic as the
		// label, flow 1; its label is the sync branch's name.
		fx.pr(w.repo, prSpec{number: 13, author: fx.user(5, "touchmark"), head: "touchmark/conformance", headRef: "refs/pull/13/head",
			flow: ptr(1), title: "agit 16"}),
	}
	w.timelines[2] = []any{
		fx.event(1, "pull_push", w.writer, "2026-09-11T08:00:00Z"),
		fx.event(2, "close", w.other, "2026-09-11T09:00:00Z"),
		fx.event(3, "reopen", w.writer, "2026-09-12T09:00:00Z"),
		fx.event(4, "close", w.writer, "2026-09-12T09:30:00Z"),
		fx.event(5, "comment", w.writer, "2026-09-12T09:30:01Z"),
	}
	w.timelines[3] = []any{fx.event(6, "close", w.writer, "2026-09-10T12:00:00Z")}
	w.timelines[11] = []any{fx.event(7, "close", fx.user(1, "tm-admin"), "2026-09-10T12:00:00Z")}
	w.timelines[12] = []any{fx.event(8, "close", w.ghost, "2026-09-10T12:00:00Z")}
	w.serve()
	return w
}

// serve declares the pull request routes: the list with its filters
// (poster by login, state, Forgejo 16's head), single pull requests and
// timelines (by since, in creation order, with the servers' headers: no
// Link, and an X-Total-Count that counts the page).
func (w *prWorld) serve() {
	w.handle(http.MethodGet, "/repos/acme/api/pulls", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var out []any
		for _, p := range w.prs {
			state := p["state"].(string)
			user := p["user"].(map[string]any)
			head := p["head"].(map[string]any)
			switch {
			case q.Get("state") == "open" && state != "open", q.Get("state") == "closed" && state != "closed":
				continue
			case q.Has("poster") && !equalFold(q.Get("poster"), user["login"].(string)):
				continue
			case q.Has("head") && head["label"] != q.Get("head"):
				continue
			}
			out = append(out, p)
		}
		if q.Has("poster") && q.Get("poster") == "renamed" {
			writeJSON(rw, http.StatusBadRequest, w.apiMsg("user does not exist [uid: 0, name: renamed]"))
			return
		}
		slices.Reverse(out) // newest first, as the servers sort
		servePage(rw, r, out, linkAndTotal)
	})
	w.json(http.MethodGet, "/repos/acme/api/branches/main", http.StatusOK, map[string]any{"name": "main",
		"commit": map[string]any{"id": strings.Repeat("b", 40)}, "protected": false, "user_can_push": false})
	w.json(http.MethodGet, "/repos/acme/api/branches/release", http.StatusNotFound, w.apiMsg("branch does not exist [name: release]"))
	for _, p := range w.prs {
		n := p["number"].(int64)
		w.json(http.MethodGet, "/repos/acme/api/pulls/"+strconv.FormatInt(n, 10), http.StatusOK, p)
		events := w.timelines[n]
		w.handle(http.MethodGet, "/repos/acme/api/issues/"+strconv.FormatInt(n, 10)+"/timeline", func(rw http.ResponseWriter, r *http.Request) {
			since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
			var out []any
			for _, e := range events {
				at, _ := time.Parse(time.RFC3339, e.(map[string]any)["updated_at"].(string))
				if err != nil || !at.Before(since) {
					out = append(out, e)
				}
			}
			servePage(rw, r, out, pageCount)
		})
	}
}

func equalFold(a, b string) bool {
	return len(a) == len(b) && (a == b || lower(a) == lower(b))
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func numbersOf(prs []platform.PR) []int64 {
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
	for _, typ := range []string{"gitea", "forgejo15", "forgejo16"} {
		t.Run(typ, func(t *testing.T) {
			w := newPRWorld(t, map[string]string{"gitea": "gitea", "forgejo15": "forgejo", "forgejo16": "forgejo"}[typ])
			switch typ {
			case "gitea":
				w.asGitea()
			case "forgejo15":
				w.asForgejo("15.0.9")
			case "forgejo16":
				w.asForgejo("16.0.5")
			}
			writer := platform.Account{ID: "3", Login: "tm-writer"}
			other := platform.Account{ID: "4", Login: "jdoe"}
			heads := []string{syncBranch, aliasBranch}
			got, err := w.reader.PRs(t.Context(), w.target, heads, []platform.Account{writer})
			if err != nil {
				t.Fatal(err)
			}
			if want := []int64{12, 11, 9, 8, 7, 3, 2, 1}; !slices.Equal(numbersOf(got), want) {
				t.Fatalf("PRs = %v, want %v", numbersOf(got), want)
			}
			checkWorldPRs(t, got)

			got, err = w.reader.PRs(t.Context(), w.target, heads, []platform.Account{other, writer})
			if want := []int64{12, 11, 9, 8, 7, 4, 3, 2, 1}; err != nil || !slices.Equal(numbersOf(got), want) {
				t.Errorf("PRs of two authors = %v, %v; want %v", numbersOf(got), err, want)
			}
			got, err = w.reader.PRs(t.Context(), w.target, heads, nil)
			if want := []int64{9, 8, 7}; err != nil || !slices.Equal(numbersOf(got), want) {
				t.Errorf("PRs without authors = %v, %v; want %v", numbersOf(got), err, want)
			}
			got, err = w.reader.PRs(t.Context(), w.target, []string{syncBranch}, []platform.Account{writer})
			if want := []int64{11, 9, 7, 2, 1}; err != nil || !slices.Equal(numbersOf(got), want) {
				t.Errorf("PRs of one head = %v, %v; want %v", numbersOf(got), err, want)
			}
			if got, err := w.reader.PRs(t.Context(), w.target, nil, []platform.Account{writer}); err != nil || len(got) != 0 {
				t.Errorf("PRs without heads = %v, %v", numbersOf(got), err)
			}

			lists := w.requests(http.MethodGet, "/repos/acme/api/pulls")
			if typ == "forgejo16" {
				for _, q := range queries(lists) {
					if q != "head=chore%2Fsync-engineering-assets&state=all" && q != "head=touchmark%2Fconformance&state=all" {
						t.Errorf("Forgejo 16 listed with %q, want the head filter", q)
					}
				}
			} else if want := []string{"poster=jdoe&state=all", "poster=tm-writer&state=all", "state=open"}; !slices.Equal(queries(lists), want) {
				t.Errorf("listings %q, want %q", queries(lists), want)
			}
			// Each close's timeline is read once per driver, whatever the
			// number of listings.
			for _, n := range []int64{2, 3, 11, 12} {
				if c := len(w.requests(http.MethodGet, "/repos/acme/api/issues/"+strconv.FormatInt(n, 10)+"/timeline")); c != 1 {
					t.Errorf("the timeline of #%d was read %d times", n, c)
				}
			}
			if c := len(w.requests(http.MethodGet, "/repos/acme/api/issues/1/timeline")); c != 0 {
				t.Errorf("the timeline of the merged #1 was read %d times: merged_by names the merger", c)
			}
		})
	}
}

// checkWorldPRs checks the fields of the writer's listing of newPRWorld.
func checkWorldPRs(t *testing.T, got []platform.PR) {
	t.Helper()
	for _, tc := range []struct {
		n        int64
		head     string
		state    platform.PRState
		author   string
		closedBy string // "" for none
		headRepo string
		base     string
		exists   bool
	}{
		{1, syncBranch, platform.Merged, "3", "4", "7", "main", true},
		{2, syncBranch, platform.Closed, "3", "3", "7", "main", true},
		{3, aliasBranch, platform.Closed, "3", "3", "7", "main", true},
		{7, syncBranch, platform.Open, "3", "", "7", "main", true},
		{8, aliasBranch, platform.Open, "4", "", "7", "main", true},
		{9, syncBranch, platform.Open, "4", "", "9", "main", true},
		{11, syncBranch, platform.Closed, "3", "1", "7", "release", false},
		{12, aliasBranch, platform.Closed, "3", "-1", "7", "main", true},
	} {
		p := find(got, tc.n)
		switch {
		case p.Head != tc.head || p.State != tc.state || p.Author.ID != tc.author:
			t.Errorf("#%d: head %q state %s author %s", tc.n, p.Head, p.State, p.Author.ID)
		case p.RepoID != "7" || p.HeadRepoID != tc.headRepo:
			t.Errorf("#%d: repo %s head repo %s, want 7 and %s", tc.n, p.RepoID, p.HeadRepoID, tc.headRepo)
		case p.Base != tc.base || p.BaseExists != tc.exists:
			t.Errorf("#%d: base %q exists %v", tc.n, p.Base, p.BaseExists)
		case p.CreatedAt.IsZero() || p.CreatedAt.Location() != time.UTC:
			t.Errorf("#%d: created %v", tc.n, p.CreatedAt)
		case tc.state == platform.Open && (p.ClosedBy != nil || !p.ClosedAt.IsZero()):
			t.Errorf("#%d: open, closed by %v at %v", tc.n, p.ClosedBy, p.ClosedAt)
		case tc.state != platform.Open && p.ClosedAt.IsZero():
			t.Errorf("#%d: %s without ClosedAt", tc.n, p.State)
		case tc.closedBy == "" && p.ClosedBy != nil, tc.closedBy != "" && (p.ClosedBy == nil || p.ClosedBy.ID != tc.closedBy):
			t.Errorf("#%d: closed by %+v, want %q", tc.n, p.ClosedBy, tc.closedBy)
		case len(p.HeadSHA) != 40:
			t.Errorf("#%d: head sha %q", tc.n, p.HeadSHA)
		}
	}
	if p := find(got, 12); p.ClosedBy == nil || p.ClosedBy.Kind != platform.KindUnknown {
		t.Errorf("#12: closed by the ghost %+v, want KindUnknown", p.ClosedBy)
	}
	if p := find(got, 7); p.Body != "body\n<!-- marker -->" || !slices.Equal(p.Labels, []string{"engineering-assets"}) || p.Title != "open" ||
		p.URL == "" || p.Draft {
		t.Errorf("#7: %+v", p)
	}
	if p := find(got, 2); !p.ClosedAt.Equal(time.Date(2026, 9, 12, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("#2 closed at %v", p.ClosedAt)
	}
}

// Gitea's lists keep the commit of a deleted base branch (its branch table
// keeps deleted branches): the branch API decides. Forgejo's lists are
// trusted.
func TestPRsStaleBase(t *testing.T) {
	for _, typ := range []string{"gitea", "forgejo"} {
		w := newPRWorld(t, typ)
		if typ == "gitea" {
			w.asGitea()
		} else {
			w.asForgejo("15.0.9")
		}
		stale := strings.Repeat("5", 40)
		w.prs[10] = w.pr(w.repo, prSpec{number: 11, author: w.writer, head: syncBranch, base: "release", baseSHA: &stale,
			state: "closed", title: "base gone"})
		w.serve()
		got, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch}, []platform.Account{{ID: "3", Login: "tm-writer"}})
		if err != nil {
			t.Fatal(err)
		}
		p := find(got, 11)
		asked := len(w.requests(http.MethodGet, "/repos/acme/api/branches/release"))
		switch typ {
		case "gitea":
			if p.BaseExists || asked != 1 || len(w.requests(http.MethodGet, "/repos/acme/api/branches/main")) != 1 {
				t.Errorf("Gitea: #11 BaseExists %v, release asked %d times", p.BaseExists, asked)
			}
			if !find(got, 7).BaseExists {
				t.Error("Gitea: #7's base main exists")
			}
		case "forgejo":
			if !p.BaseExists || asked != 0 {
				t.Errorf("Forgejo: #11 BaseExists %v, release asked %d times", p.BaseExists, asked)
			}
		}
		if typ == "gitea" {
			// A branch API that fails fails PRs: a base is never guessed.
			w.json(http.MethodGet, "/repos/acme/api/branches/release", http.StatusBadGateway, w.apiMsg("bad gateway"))
			_, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch}, []platform.Account{{ID: "3", Login: "tm-writer"}})
			wantClass(t, "Gitea: PRs with a failing branch API", err, platform.ClassTransient, nil)
		}
	}
}

// A login the server does not know falls back to the whole listing.
func TestPRsUnknownPoster(t *testing.T) {
	w := newPRWorld(t, "gitea")
	w.asGitea()
	renamed := platform.Account{ID: "3", Login: "renamed"}
	got, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch, aliasBranch}, []platform.Account{renamed})
	if want := []int64{12, 11, 9, 8, 7, 3, 2, 1}; err != nil || !slices.Equal(numbersOf(got), want) {
		t.Errorf("PRs = %v, %v; want %v", numbersOf(got), err, want)
	}
	if want := []string{"poster=renamed&state=all", "state=all", "state=open"}; !slices.Equal(queries(w.requests(http.MethodGet, "/repos/acme/api/pulls")), want) {
		t.Errorf("listings %q, want %q", queries(w.requests(http.MethodGet, "/repos/acme/api/pulls")), want)
	}
}

func TestPRsPagesAndLimits(t *testing.T) {
	w := newPRWorld(t, "gitea")
	w.asGitea()
	w.settings(2)
	got, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch, aliasBranch}, []platform.Account{{ID: "3", Login: "tm-writer"}})
	if want := []int64{12, 11, 9, 8, 7, 3, 2, 1}; err != nil || !slices.Equal(numbersOf(got), want) {
		t.Errorf("PRs over pages of 2 = %v, %v", numbersOf(got), err)
	}
	// A listing without end fails: PRs cannot be partial.
	w.handle(http.MethodGet, "/repos/acme/api/pulls", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Link", `<http://`+r.Host+`/api/v1/repos/acme/api/pulls?page=9999>; rel="next"`)
		writeJSON(rw, http.StatusOK, []any{w.prs[6]})
	})
	_, err = w.reader.PRs(t.Context(), w.target, []string{syncBranch}, nil)
	wantClass(t, "an endless listing", err, platform.ClassUnknown, nil)
	w.json(http.MethodGet, "/repos/acme/api/pulls", http.StatusNotFound, w.apiMsg("not found"))
	_, err = w.reader.PRs(t.Context(), w.target, []string{syncBranch}, nil)
	wantClass(t, "PRs of a missing repository", err, platform.ClassNotFound, platform.ErrNotFound)
	w.json(http.MethodGet, "/repos/acme/api/pulls", http.StatusOK, []any{map[string]any{"number": 1, "state": "weird"}})
	_, err = w.reader.PRs(t.Context(), w.target, []string{syncBranch}, nil)
	wantClass(t, "a pull request of an unknown shape", err, platform.ClassUnknown, nil)
}

// A timeline that cannot be read fails PRs: a closer is never guessed.
func TestPRsTimelineFailure(t *testing.T) {
	w := newPRWorld(t, "gitea")
	w.asGitea()
	w.json(http.MethodGet, "/repos/acme/api/issues/2/timeline", http.StatusBadGateway, w.apiMsg("bad gateway"))
	_, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch}, []platform.Account{{ID: "3", Login: "tm-writer"}})
	wantClass(t, "PRs with a failing timeline", err, platform.ClassTransient, nil)
}

// The timeline's X-Total-Count counts the page, not the timeline, so the
// driver reads on to a short or empty page. Here the last close is on the
// second page: since filters by update time, the timeline is in creation
// order, and comments written before the close but edited after it come
// first.
func TestPRsTimelinePages(t *testing.T) {
	w := newPRWorld(t, "gitea")
	w.asGitea()
	w.settings(2)
	edited := func(id int64, created, updated string) map[string]any {
		e := w.event(id, "comment", w.other, created)
		e["updated_at"] = updated
		return e
	}
	// #2 was closed last by the writer at 09:30 (closed_at).
	w.timelines[2] = []any{
		w.event(1, "close", w.other, "2026-09-12T09:00:00Z"),
		edited(2, "2026-09-12T09:05:00Z", "2026-09-12T09:45:00Z"),
		w.event(3, "reopen", w.writer, "2026-09-12T09:10:00Z"),
		edited(4, "2026-09-12T09:20:00Z", "2026-09-12T09:50:00Z"),
		w.event(5, "close", w.writer, "2026-09-12T09:30:00Z"),
	}
	w.serve()
	got, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch}, []platform.Account{{ID: "3", Login: "tm-writer"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := find(got, 2); p.ClosedBy == nil || p.ClosedBy.ID != "3" {
		t.Errorf("#2 closed by %+v, want the writer (3), whose close is on the second page of the timeline", p.ClosedBy)
	}
	if n := len(w.requests(http.MethodGet, "/repos/acme/api/issues/2/timeline")); n != 2 {
		t.Errorf("the timeline of #2 took %d requests, want 2 (a full page, then a short one)", n)
	}
}

func TestPRsConcurrent(t *testing.T) {
	w := newPRWorld(t, "forgejo")
	w.asForgejo("15.0.9")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := w.reader.PRs(t.Context(), w.target, []string{syncBranch, aliasBranch}, []platform.Account{{ID: "3", Login: "tm-writer"}})
			if want := []int64{12, 11, 9, 8, 7, 3, 2, 1}; err != nil || !slices.Equal(numbersOf(got), want) {
				t.Errorf("concurrent PRs = %v, %v", numbersOf(got), err)
			}
		}()
	}
	wg.Wait()
}

// sweepWorld serves the issue search over two repositories.
func sweepWorld(t *testing.T, typ string) (*prWorld, map[string]any) {
	w := newPRWorld(t, typ)
	b := w.repo2()
	onB := w.pr(b, prSpec{number: 1, author: w.writer, head: aliasBranch, title: "b"})
	w.json(http.MethodGet, "/repos/acme/b/pulls/1", http.StatusOK, onB)
	issue := func(repo map[string]any, p map[string]any) map[string]any {
		return map[string]any{"id": p["id"], "url": "", "html_url": p["html_url"], "number": p["number"], "user": p["user"],
			"original_author": "", "original_author_id": 0, "title": p["title"], "body": "", "ref": "", "assets": []any{},
			"labels": []any{}, "milestone": nil, "assignee": nil, "assignees": nil, "state": p["state"], "is_locked": false,
			"comments": 0, "created_at": p["created_at"], "updated_at": p["updated_at"], "closed_at": nil, "due_date": nil,
			"pull_request": map[string]any{"merged": false, "merged_at": nil, "draft": false, "html_url": p["html_url"]},
			"repository":   map[string]any{"id": repo["id"], "name": repo["name"], "owner": "acme", "full_name": repo["full_name"]},
			"pin_order":    0}
	}
	var all []any
	for _, p := range w.prs {
		if p["state"] == "open" {
			all = append(all, issue(w.repo, p))
		}
	}
	all = append(all, issue(b, onB))
	w.handle(http.MethodGet, "/repos/issues/search", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("type") != "pulls" || q.Get("state") != "open" {
			t.Errorf("search %q", r.URL.RawQuery)
		}
		var out []any
		for _, it := range all {
			login := it.(map[string]any)["user"].(map[string]any)["login"].(string)
			switch {
			case typ == "gitea" && q.Has("created_by") && q.Get("created_by") == "gone":
				writeJSON(rw, http.StatusNotFound, w.apiMsg("user does not exist [uid: 0, name: gone]"))
				return
			case typ == "gitea" && q.Has("created_by") && !equalFold(q.Get("created_by"), login):
				continue
			case q.Get("created") == "true" && login != "tm-writer": // the token is the writer's
				continue
			}
			out = append(out, it)
		}
		servePage(rw, r, out, linkAndTotal)
	})
	return w, onB
}

// repo2 is a second repository, acme/b.
func (w *prWorld) repo2() map[string]any { return w.apiServer.repo(8, "acme/b") }

func TestOpenPRsByGitea(t *testing.T) {
	w, _ := sweepWorld(t, "gitea")
	w.asGitea()
	writer := platform.Account{ID: "3", Login: "tm-writer"}
	sw, err := w.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch, aliasBranch})
	if err != nil || !sw.Complete {
		t.Fatalf("OpenPRsBy = %+v, %v", sw, err)
	}
	var got []string
	for _, rp := range sw.PRs {
		got = append(got, rp.Repo.Path+"#"+strconv.FormatInt(rp.PR.Number, 10))
		if rp.PR.State != platform.Open || rp.PR.Author.ID != "3" || rp.PR.RepoID != rp.Repo.ID || rp.Repo.DefaultBranch != "main" {
			t.Errorf("%s: %+v", got[len(got)-1], rp)
		}
	}
	// Sorted by repository path, then newest first; the writer's #6 is on
	// another branch.
	if want := []string{"acme/api#7", "acme/b#1"}; !slices.Equal(got, want) {
		t.Errorf("OpenPRsBy = %v, want %v", got, want)
	}
	if want := []string{"created_by=tm-writer&state=open&type=pulls"}; !slices.Equal(queries(w.requests(http.MethodGet, "/repos/issues/search")), want) {
		t.Errorf("searches %q", queries(w.requests(http.MethodGet, "/repos/issues/search")))
	}
	// Two authors: a search each; an author the server does not know makes
	// the result incomplete.
	sw, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{writer, {ID: "4", Login: "jdoe"}}, []string{syncBranch, aliasBranch})
	got = nil
	for _, rp := range sw.PRs {
		got = append(got, rp.Repo.Path+"#"+strconv.FormatInt(rp.PR.Number, 10))
	}
	if want := []string{"acme/api#9", "acme/api#8", "acme/api#7", "acme/b#1"}; err != nil || !sw.Complete || !slices.Equal(got, want) {
		t.Errorf("OpenPRsBy of two authors = %v (complete %v), %v; want %v", got, sw.Complete, err, want)
	}
	sw, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{writer, {ID: "99", Login: "gone"}}, []string{syncBranch, aliasBranch})
	if err != nil || sw.Complete || len(sw.PRs) != 2 {
		t.Errorf("OpenPRsBy with an unknown login = %+v, %v; want two, incomplete", sw, err)
	}
}

func TestOpenPRsByForgejo(t *testing.T) {
	w, _ := sweepWorld(t, "forgejo")
	w.asForgejo("15.0.9")
	w.json(http.MethodGet, "/user", http.StatusOK, w.writer)
	writer := platform.Account{ID: "3", Login: "tm-writer"}
	// The writer's own sweep: created=true.
	sw, err := w.fixture.writer.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch, aliasBranch})
	if err != nil || !sw.Complete || len(sw.PRs) != 2 {
		t.Errorf("writer's OpenPRsBy = %+v, %v", sw, err)
	}
	if want := []string{"created=true&state=open&type=pulls"}; !slices.Equal(queries(w.requests(http.MethodGet, "/repos/issues/search")), want) {
		t.Errorf("searches %q, want %q", queries(w.requests(http.MethodGet, "/repos/issues/search")), want)
	}
	// Another author: Forgejo has no created_by, the whole search is
	// filtered here.
	w.reset()
	sw, err = w.fixture.writer.OpenPRsBy(t.Context(), []platform.Account{writer, {ID: "4", Login: "jdoe"}}, []string{syncBranch, aliasBranch})
	if err != nil || !sw.Complete || len(sw.PRs) != 4 {
		t.Errorf("OpenPRsBy of two authors = %+v, %v", sw, err)
	}
	if want := []string{"state=open&type=pulls"}; !slices.Equal(queries(w.requests(http.MethodGet, "/repos/issues/search")), want) {
		t.Errorf("searches %q, want %q", queries(w.requests(http.MethodGet, "/repos/issues/search")), want)
	}
}

func TestOpenPRsByFailures(t *testing.T) {
	w, _ := sweepWorld(t, "gitea")
	w.asGitea()
	writer := platform.Account{ID: "3", Login: "tm-writer"}
	heads := []string{syncBranch, aliasBranch}
	// A pull request gone since the search is left out; one that fails to
	// read makes the result incomplete; a rate limit fails the call.
	w.json(http.MethodGet, "/repos/acme/b/pulls/1", http.StatusNotFound, w.apiMsg("not found"))
	sw, err := w.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, heads)
	if err != nil || !sw.Complete || len(sw.PRs) != 1 {
		t.Errorf("a vanished pull request: %+v, %v", sw, err)
	}
	w.json(http.MethodGet, "/repos/acme/b/pulls/1", http.StatusInternalServerError, w.apiMsg(""))
	sw, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, heads)
	if err != nil || sw.Complete || len(sw.PRs) != 1 {
		t.Errorf("a pull request that fails: %+v, %v", sw, err)
	}
	w.handle(http.MethodGet, "/repos/acme/b/pulls/1", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Retry-After", "30")
		writeJSON(rw, http.StatusTooManyRequests, w.apiMsg("slow down"))
	})
	_, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, heads)
	wantClass(t, "a rate limit", err, platform.ClassRateLimited, nil)
	// A credential refused while reading a pull request fails the call too:
	// it is no reason to sweep less.
	w.json(http.MethodGet, "/repos/acme/b/pulls/1", http.StatusUnauthorized, w.apiMsg("token is required"))
	sw, err = w.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, heads)
	wantClass(t, "a refused credential", err, platform.ClassAuth, nil)
	if err == nil {
		t.Errorf("a refused credential: %+v without an error", sw)
	}
	if sw, err := w.reader.OpenPRsBy(t.Context(), nil, heads); err != nil || !sw.Complete || len(sw.PRs) != 0 {
		t.Errorf("no authors: %+v, %v", sw, err)
	}
}

// The issue search failing with a rate limit, a refused credential or a
// transient failure, on its first page or a later one, fails OpenPRsBy with
// that class (and the wait of a rate limit), never an incomplete result:
// the core pauses the provider on a rate limit and retries a transient
// failure, which an incomplete sweep would hide.
// What the search refuses otherwise (a 403, an unknown user's 404) only
// makes the result incomplete.
func TestOpenPRsBySearchErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		page   string // the page of the search that fails
		status int
		header map[string]string
		class  platform.Class
		retry  time.Duration
	}{
		{"rate limit", "1", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, platform.ClassRateLimited, 30 * time.Second},
		{"rate limit on page 2", "2", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, platform.ClassRateLimited, 30 * time.Second},
		{"a proxy's limit", "1", http.StatusForbidden, map[string]string{"RateLimit-Remaining": "0", "RateLimit-Reset": "9"}, platform.ClassRateLimited, 9 * time.Second},
		{"Codeberg's limit", "2", http.StatusTooManyRequests, map[string]string{"RateLimit": `"baseline";r=0;t=42`}, platform.ClassRateLimited, 42 * time.Second},
		{"credential", "1", http.StatusUnauthorized, nil, platform.ClassAuth, 0},
		{"credential on page 2", "2", http.StatusUnauthorized, nil, platform.ClassAuth, 0},
		{"transient", "1", http.StatusBadGateway, nil, platform.ClassTransient, 0},
		{"transient on page 2", "2", http.StatusServiceUnavailable, map[string]string{"Retry-After": "5"}, platform.ClassTransient, 5 * time.Second},
		{"forbidden", "1", http.StatusForbidden, nil, platform.ClassUnknown, 0},
		{"forbidden on page 2", "2", http.StatusForbidden, nil, platform.ClassUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := sweepWorld(t, "gitea")
			w.asGitea()
			w.settings(1) // the writer's two open pull requests take two pages
			search := w.route(http.MethodGet, "/repos/issues/search")
			w.handle(http.MethodGet, "/repos/issues/search", func(rw http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") != tc.page {
					search(rw, r)
					return
				}
				for k, v := range tc.header {
					rw.Header().Set(k, v)
				}
				writeJSON(rw, tc.status, w.apiMsg("refused"))
			})
			sw, err := w.reader.OpenPRsBy(t.Context(), []platform.Account{{ID: "3", Login: "tm-writer"}}, []string{syncBranch, aliasBranch})
			if tc.class == platform.ClassUnknown {
				if err != nil || sw.Complete {
					t.Errorf("OpenPRsBy = %+v, %v; want an incomplete result without an error", sw, err)
				}
				return
			}
			var pe *platform.Error
			switch {
			case err == nil:
				t.Fatalf("OpenPRsBy = %+v without an error, want class %v", sw, tc.class)
			case !errors.As(err, &pe):
				t.Fatalf("OpenPRsBy: %v is no *platform.Error", err)
			case platform.ClassOf(err) != tc.class || pe.Status != tc.status || pe.RetryAfter != tc.retry:
				t.Errorf("OpenPRsBy: %v (class %v, status %d, retry after %v), want class %v, status %d, retry after %v",
					err, platform.ClassOf(err), pe.Status, pe.RetryAfter, tc.class, tc.status, tc.retry)
			}
		})
	}
}
