package ghfake

import (
	"strings"
	"sync"
	"testing"
)

func TestWebCommits(t *testing.T) {
	w := newWorld(t, Options{})
	signed := w.commit("acme/api", CommitSpec{Author: "alice", Signed: true, Files: []File{{Path: "w", Content: []byte("w")}}})
	plain := w.commit("acme/api", CommitSpec{Author: "alice", Files: []File{{Path: "p", Content: []byte("p")}}})
	for sha, want := range map[string]string{signed: "valid", plain: "unsigned"} {
		got := w.call("GET", "/repos/acme/api/git/commits/"+sha, "", nil).obj(t)
		if field(got, "verification", "reason") != want {
			t.Errorf("%s: %v", want, got["verification"])
		}
	}
	c := w.call("GET", "/repos/acme/api/commits/"+signed, "", nil).obj(t)
	if field(c, "author", "login") != "alice" || field(c, "committer") != nil || field(c, "commit", "tree", "sha") == nil {
		t.Errorf("commits API: %v", c)
	}
	cmp := w.call("GET", "/repos/acme/api/compare/"+signed+"..."+plain, "", nil).obj(t)
	if cmp["status"] != "ahead" || cmp["ahead_by"] != float64(1) || field(cmp, "merge_base_commit", "sha") != signed {
		t.Errorf("compare: %v", cmp)
	}
}

func TestGHESSchema(t *testing.T) {
	w := newWorld(t, Options{Flavor: GHES})
	tok := w.token(nil, nil)
	q := `{ repository(owner: "acme", name: "api") { hasPullRequestsEnabled } }`
	g := doRequest(t, w.s, "POST", w.s.URL()+"/api/graphql", tok, map[string]any{"query": q}).obj(t)
	if field(g, "errors", 0, "extensions", "code") != "undefinedField" {
		t.Errorf("GHES 3.19 has no hasPullRequestsEnabled: %v", g)
	}
	d := newWorld(t, Options{})
	if got := d.graphql(d.token(nil, nil), q, nil); field(got, "data", "repository", "hasPullRequestsEnabled") != true {
		t.Errorf("github.com: %v", got)
	}
}

func TestConcurrent(t *testing.T) {
	w := newWorld(t, Options{})
	for i := range 3 {
		w.branch("acme/api", "b"+itoa(int64(i)))
	}
	tok := w.token(nil, nil)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := range 3 {
		n := itoa(int64(i))
		wg.Add(3)
		go func() {
			defer wg.Done()
			r := doRequest(t, w.s, "POST", w.s.URL()+"/repos/acme/api/pulls", tok,
				map[string]any{"title": n, "head": "b" + n, "base": "main"})
			if r.status != 201 {
				errs <- "create " + n + ": " + string(r.body)
			}
		}()
		go func() {
			defer wg.Done()
			if r := doRequest(t, w.s, "GET", w.s.URL()+"/repos/acme/api/contents/README.md", tok, nil); r.status != 200 {
				errs <- "contents: " + string(r.body)
			}
		}()
		go func() {
			defer wg.Done()
			r := doRequest(t, w.s, "POST", w.s.URL()+"/graphql", tok,
				map[string]any{"query": `{ repository(owner: "acme", name: "api") { pullRequests(first: 10) { totalCount } } }`})
			if r.status != 200 || strings.Contains(string(r.body), `"errors"`) {
				errs <- "graphql: " + string(r.body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := len(w.s.PRs("acme/api")); n != 3 {
		t.Errorf("%d pull requests", n)
	}
	w.noViolations()
}
