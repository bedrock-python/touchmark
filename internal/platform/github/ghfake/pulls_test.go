package ghfake

import (
	"net/http"
	"strings"
	"testing"
)

func TestCreatePull(t *testing.T) {
	w := newWorld(t, Options{})
	w.branch("acme/api", "touchmark/hub")
	tok := w.token([]string{"api"}, nil)
	body := "text\n\n<!-- touchmark:v1 hub=x fp=0123456789abcdef -->"
	r := w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "sync", "head": "touchmark/hub", "base": "main", "body": body})
	wantStatus(t, "create", r, 201)
	pr := r.obj(t)
	if field(pr, "user", "login") != "hub-writer[bot]" || field(pr, "user", "type") != "Bot" || pr["body"] != body ||
		field(pr, "head", "label") != "acme:touchmark/hub" || field(pr, "head", "repo", "id") != float64(w.api.ID) ||
		pr["state"] != "open" || pr["draft"] != false || pr["merged"] != false {
		t.Errorf("created: %v", pr)
	}
	if u := w.s.Usage("installation/" + itoa(w.inst.ID)); u.ContentCreated != 1 {
		t.Errorf("usage %+v", u)
	}

	r = w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "again", "head": "touchmark/hub", "base": "main"})
	wantStatus(t, "duplicate", r, 422)
	if msg := field(r.obj(t), "errors", 0, "message"); msg != "A pull request already exists for acme:touchmark/hub." {
		t.Errorf("duplicate: %s", r.body)
	}
	w.commit("acme/api", CommitSpec{Branch: "same", Author: "alice", Files: []File{{Path: "README.md", Content: []byte("# api\n")}}})
	check(t, w.s.SetBranch("acme/api", "same", "main", "alice"))
	r = w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "empty", "head": "same", "base": "main"})
	wantStatus(t, "no commits", r, 422)
	if msg := field(r.obj(t), "errors", 0, "message"); msg != "No commits between main and same" {
		t.Errorf("no commits: %s", r.body)
	}
	for name, in := range map[string]map[string]any{
		"missing head": {"title": "x", "head": "nope", "base": "main"},
		"missing base": {"title": "x", "head": "touchmark/hub", "base": "nope"},
		"no title":     {"head": "touchmark/hub", "base": "main"},
		"long body":    {"title": "x", "head": "touchmark/hub", "base": "main", "body": strings.Repeat("é", maxBody+1)},
	} {
		wantStatus(t, name, w.call("POST", "/repos/acme/api/pulls", tok, in), 422)
	}
	w.noViolations()
}

func TestDrafts(t *testing.T) {
	w := newWorld(t, Options{})
	must[Account](t)(w.s.AddOrg("small", PlanFree))
	w.repo(RepoSpec{Owner: "small", Name: "pub", Files: []File{{Path: "a", Content: []byte("a")}}})
	w.repo(RepoSpec{Owner: "small", Name: "priv", Visibility: "private", Files: []File{{Path: "a", Content: []byte("a")}}})
	pat := must[string](t)(w.s.AddPAT("alice", PATSpec{Scopes: []string{"repo"}}))
	for _, name := range []string{"small/pub", "small/priv"} {
		check(t, w.s.Grant(name, "alice", "write"))
		w.branch(name, "feature")
	}
	draft := map[string]any{"title": "d", "head": "feature", "base": "main", "draft": true}
	r := w.call("POST", "/repos/small/pub/pulls", pat, draft)
	wantStatus(t, "public draft", r, 201)
	if r.obj(t)["draft"] != true {
		t.Errorf("draft: %s", r.body)
	}
	r = w.call("POST", "/repos/small/priv/pulls", pat, draft)
	wantStatus(t, "private draft on free", r, 422)
	if msg := field(r.obj(t), "errors", 0, "message"); msg != "Draft pull requests are not supported in this repository." {
		t.Errorf("draft refusal: %s", r.body)
	}
	delete(draft, "draft")
	wantStatus(t, "private ready", w.call("POST", "/repos/small/priv/pulls", pat, draft), 201)
}

func TestListPulls(t *testing.T) {
	w := newWorld(t, Options{})
	w.branch("acme/api", "touchmark/hub")
	w.branch("acme/api", "feature")
	a := w.openPR("acme/api", PRSpec{Head: "touchmark/hub", Title: "a", Author: "alice"})
	b := w.openPR("acme/api", PRSpec{Head: "feature", Title: "b", Author: "bob"})
	check(t, w.s.SetPRState("acme/api", a.Number, "closed", "bob"))
	numbers := func(query string) []int64 {
		var out []int64
		for _, item := range w.call("GET", "/repos/acme/api/pulls"+query, "", nil).list(t) {
			out = append(out, int64(field(item, "number").(float64)))
		}
		return out
	}
	eq := func(what string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", what, got, want)
				return
			}
		}
	}
	eq("open", numbers(""), b.Number)
	eq("all", numbers("?state=all"), b.Number, a.Number)
	eq("head", numbers("?state=all&head=acme:touchmark/hub"), a.Number)
	eq("head case", numbers("?state=all&head=ACME:touchmark/hub"), a.Number)
	// Observed: a head without "owner:" is ignored.
	eq("bare head", numbers("?state=all&head=touchmark/hub"), b.Number, a.Number)
	eq("other owner", numbers("?state=all&head=alice:touchmark/hub"))
	eq("closed", numbers("?state=closed"), a.Number)
	eq("asc", numbers("?state=all&direction=asc"), a.Number, b.Number)
	// Deleting the head branch keeps the pull request findable.
	check(t, w.s.SetBranch("acme/api", "touchmark/hub", "", "alice"))
	eq("after the branch is gone", numbers("?state=all&head=acme:touchmark/hub"), a.Number)
	single := w.call("GET", "/repos/acme/api/pulls/"+itoa(a.Number), "", nil).obj(t)
	if single["merged"] != false || single["state"] != "closed" || single["closed_at"] == nil {
		t.Errorf("single: %v", single)
	}
	wantStatus(t, "missing", w.call("GET", "/repos/acme/api/pulls/999", "", nil), 404)
}

func TestUpdatePull(t *testing.T) {
	w := newWorld(t, Options{})
	w.branch("acme/api", "touchmark/hub")
	w.branch("acme/api", "release")
	tok := w.token([]string{"api"}, nil)
	pr := w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "t", "head": "touchmark/hub", "base": "main"}).obj(t)
	n := itoa(int64(pr["number"].(float64)))

	r := w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"body": "closed body", "state": "closed"})
	wantStatus(t, "close", r, 200)
	got, _ := w.s.GetPR("acme/api", int64(pr["number"].(float64)))
	if got.State != "closed" || got.Body != "closed body" || got.ClosedBy == nil || got.ClosedBy.Login != "hub-writer[bot]" {
		t.Errorf("after close: %+v", got)
	}
	// A refused edit changes nothing (assumed: GitHub validates first).
	r = w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"body": "new", "base": "release"})
	wantStatus(t, "base of a closed pull request", r, 422)
	if got, _ := w.s.GetPR("acme/api", got.Number); got.Body != "closed body" || got.Base != "main" {
		t.Errorf("refused edit applied: %+v", got)
	}
	wantStatus(t, "reopen", w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"state": "open"}), 200)
	wantStatus(t, "retarget", w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"base": "release"}), 200)
	got, _ = w.s.GetPR("acme/api", got.Number)
	types := []string{}
	for _, e := range got.Events {
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "ClosedEvent,ReopenedEvent,BaseRefChangedEvent" || got.Base != "release" {
		t.Errorf("events %v, base %s", types, got.Base)
	}
	wantStatus(t, "bad state", w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"state": "merged"}), 422)
	wantStatus(t, "empty title", w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"title": " "}), 422)
	w.noViolations()
}

func TestReopenRules(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token([]string{"api"}, nil)
	open := func(branch string) string {
		w.branch("acme/api", branch)
		pr := w.openPR("acme/api", PRSpec{Head: branch, Title: branch, Author: "alice"})
		check(t, w.s.SetPRState("acme/api", pr.Number, "closed", "alice"))
		return itoa(pr.Number)
	}
	reopen := func(n string) reply {
		return w.call("PATCH", "/repos/acme/api/pulls/"+n, tok, map[string]any{"state": "open"})
	}
	gone := open("gone")
	check(t, w.s.SetBranch("acme/api", "gone", "", "alice"))
	r := reopen(gone)
	wantStatus(t, "deleted head", r, 422)
	if msg := field(r.obj(t), "errors", 0, "message"); !strings.Contains(msg.(string), "branch has been deleted") {
		t.Errorf("deleted head: %s", r.body)
	}
	forced := open("forced")
	check(t, w.s.SetBranch("acme/api", "forced", "main", "alice"))
	w.commit("acme/api", CommitSpec{Branch: "forced", Author: "alice", Files: []File{{Path: "other", Content: []byte("o")}}})
	r = reopen(forced)
	wantStatus(t, "force-pushed head", r, 422)
	if msg := field(r.obj(t), "errors", 0, "message"); !strings.Contains(msg.(string), "force-pushed or recreated") {
		t.Errorf("force-pushed: %s", r.body)
	}
	grown := open("grown")
	w.commit("acme/api", CommitSpec{Branch: "grown", Author: "alice", Files: []File{{Path: "more", Content: []byte("m")}}})
	wantStatus(t, "head grew", reopen(grown), 200)
}

func TestCommentsAndLabels(t *testing.T) {
	w := newWorld(t, Options{})
	w.branch("acme/api", "touchmark/hub")
	pr := w.openPR("acme/api", PRSpec{Head: "touchmark/hub", Title: "t", Author: "alice"})
	n := itoa(pr.Number)
	tok := w.token([]string{"api"}, Permissions{"pull_requests": Write})
	r := w.call("POST", "/repos/acme/api/issues/"+n+"/comments", tok, map[string]any{"body": "closing: no-diff"})
	wantStatus(t, "comment", r, 201)
	if field(r.obj(t), "user", "login") != "hub-writer[bot]" {
		t.Errorf("comment: %s", r.body)
	}
	if c := w.s.Comments("acme/api", pr.Number); len(c) != 1 || c[0].Body != "closing: no-diff" {
		t.Errorf("comments %+v", c)
	}
	wantStatus(t, "list comments", w.call("GET", "/repos/acme/api/issues/"+n+"/comments", tok, nil), 200)
	wantStatus(t, "comment on nothing", w.call("POST", "/repos/acme/api/issues/99/comments", tok, map[string]any{"body": "x"}), 404)

	r = w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "engineering-assets", "color": "0e8a16", "description": "sync"})
	wantStatus(t, "create label", r, 201)
	r = w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "Engineering-Assets"})
	wantStatus(t, "existing label", r, 422)
	if field(r.obj(t), "errors", 0, "code") != "already_exists" {
		t.Errorf("existing label: %s", r.body)
	}
	r = w.call("POST", "/repos/acme/api/issues/"+n+"/labels", tok, map[string]any{"labels": []string{"engineering-assets", "new-one"}})
	wantStatus(t, "add labels", r, 200)
	if l := r.list(t); len(l) != 2 || field(l, 1, "name") != "new-one" {
		t.Errorf("labels: %s", r.body)
	}
	r = w.call("POST", "/repos/acme/api/issues/"+n+"/labels", tok, []string{"third"})
	if l := r.list(t); len(l) != 3 {
		t.Errorf("labels from a bare list: %s", r.body)
	}
	if got := strings.Join(w.s.Labels("acme/api"), ","); got != "engineering-assets,new-one,third" {
		t.Errorf("repository labels %s", got)
	}
	wantStatus(t, "get label", w.call("GET", "/repos/acme/api/labels/ENGINEERING-ASSETS", "", nil), 200)
	if u := w.s.Usage("installation/" + itoa(w.inst.ID)); u.ContentCreated != 4 {
		t.Errorf("usage %+v", u)
	}
}

func TestMergePull(t *testing.T) {
	w := newWorld(t, Options{})
	check(t, w.s.Grant("acme/api", "alice", "write"))
	pat := must[string](t)(w.s.AddPAT("alice", PATSpec{Scopes: []string{"repo"}}))
	check(t, w.s.Human("alice"))
	for _, how := range []MergeMethod{MergeCommit, MergeSquash, MergeRebase} {
		branch := "f-" + string(how)
		head := w.branch("acme/api", branch)
		pr := w.openPR("acme/api", PRSpec{Head: branch, Title: branch, Author: "alice"})
		wantStatus(t, "stale sha", w.call("PUT", "/repos/acme/api/pulls/"+itoa(pr.Number)+"/merge", pat,
			map[string]any{"merge_method": how, "sha": strings.Repeat("0", 40)}), http.StatusConflict)
		r := w.call("PUT", "/repos/acme/api/pulls/"+itoa(pr.Number)+"/merge", pat, map[string]any{"merge_method": how, "sha": head})
		wantStatus(t, "merge "+string(how), r, 200)
		got, _ := w.s.GetPR("acme/api", pr.Number)
		if !got.Merged || got.State != "closed" || got.MergedBy == nil || got.MergedBy.Login != "alice" || got.MergeSHA != w.s.Branch("acme/api", "main") {
			t.Errorf("%s: %+v", how, got)
		}
		if n := len(got.Events); n < 2 || got.Events[n-2].Type != EventMerged || got.Events[n-1].Type != EventClosed {
			t.Errorf("%s events %+v", how, got.Events)
		}
		commit := w.call("GET", "/repos/acme/api/git/commits/"+got.MergeSHA, "", nil).obj(t)
		if field(commit, "verification", "verified") != true || field(commit, "committer", "name") != "GitHub" {
			t.Errorf("%s commit: %v", how, commit)
		}
		wantStatus(t, "merge twice", w.call("PUT", "/repos/acme/api/pulls/"+itoa(pr.Number)+"/merge", pat, nil), 405)
	}
	w.noViolations()
}
