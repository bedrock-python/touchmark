package ghfake

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

func TestForeignBranch(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	w.branch("acme/api", "touchmark/hub")
	w.openPR("acme/api", PRSpec{Head: "touchmark/hub", Title: "by a person", Author: "alice"})
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	mustGit(t, env, dir, "fetch", "-q", "origin", "touchmark/hub")
	mustGit(t, env, dir, "reset", "-q", "--hard", "FETCH_HEAD")
	commitFile(t, env, dir, "x", "1\n", "one")
	push(t, env, dir, "origin", "HEAD:refs/heads/touchmark/hub")
	if v := w.s.Violations(); len(v) != 1 || !strings.HasPrefix(v[0], "foreign-branch acme/api#1: hub-writer[bot] moved") {
		t.Errorf("violations %q", v)
	}
	check(t, w.s.SetKnownAuthors("hub-writer[bot]", "alice"))
	commitFile(t, env, dir, "x", "2\n", "two")
	push(t, env, dir, "origin", "HEAD:refs/heads/touchmark/hub")
	w.noViolations()
	// A person's token is not judged once marked Human.
	check(t, w.s.Grant("acme/api", "bob", "write"))
	check(t, w.s.Human("bob"))
	pat := must[string](t)(w.s.AddPAT("bob", PATSpec{Scopes: []string{"repo"}}))
	bdir, benv := clone(t, w.s, "acme/api", pat)
	commitFile(t, benv, bdir, "y", "y\n", "direct")
	if res := push(t, benv, bdir, "origin", "HEAD:main"); res.Status != gitx.PushOK {
		t.Fatalf("person's push: %+v", res)
	}
	w.noViolations()
	// The same push by touchmark is a violation.
	commitFile(t, env, dir, "z", "z\n", "z")
	push(t, env, dir, "--force", "origin", "HEAD:main")
	if v := w.s.Violations(); len(v) == 0 || !strings.HasPrefix(v[0], "default-branch acme/api") {
		t.Errorf("violations %q", v)
	}
}

func TestBaseDeleted(t *testing.T) {
	w := newWorld(t, Options{})
	w.branch("acme/api", "release")
	w.branch("acme/api", "feature")
	onRelease := w.openPR("acme/api", PRSpec{Head: "feature", Base: "release", Title: "f", Author: "alice"})
	check(t, w.s.SetBranch("acme/api", "release", "", "bob"))
	got, _ := w.s.GetPR("acme/api", onRelease.Number)
	if got.State != "closed" || got.Base != "release" || got.ClosedBy == nil || got.ClosedBy.Login != "bob" {
		t.Errorf("base deleted: %+v", got)
	}
	// A stacked pull request moves to the base of the merged one whose
	// branch is deleted (documented retargeting).
	w.branch("acme/api", "step1")
	w.commit("acme/api", CommitSpec{Branch: "step2", From: "step1", Author: "alice", Files: []File{{Path: "s2", Content: []byte("2")}}})
	one := w.openPR("acme/api", PRSpec{Head: "step1", Title: "1", Author: "alice"})
	two := w.openPR("acme/api", PRSpec{Head: "step2", Base: "step1", Title: "2", Author: "alice"})
	must[string](t)(w.s.MergePR("acme/api", one.Number, MergeCommit, "bob"))
	check(t, w.s.SetBranch("acme/api", "step1", "", "bob"))
	got, _ = w.s.GetPR("acme/api", two.Number)
	if got.State != "open" || got.Base != "main" || got.Events[len(got.Events)-1].Type != EventBaseRefChanged {
		t.Errorf("retargeted: %+v", got)
	}
	w.noViolations()
}

func TestGitFaults(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	c := commitFile(t, env, dir, "x", "x\n", "x")
	w.s.Fail("GIT push", Fault{Status: 502})
	if res := push(t, env, dir, "origin", "HEAD:refs/heads/a"); res.Status == gitx.PushOK {
		t.Errorf("faulted push: %+v", res)
	}
	if w.s.Branch("acme/api", "a") != "" {
		t.Error("a failed push moved the branch")
	}
	// An applied fault: the branch moves, the answer is lost.
	w.s.Fail("GIT push", Fault{Status: 500, Applied: true})
	if res := push(t, env, dir, "origin", "HEAD:refs/heads/b"); res.Status == gitx.PushOK {
		t.Errorf("applied fault: %+v", res)
	}
	if w.s.Branch("acme/api", "b") != c {
		t.Error("an applied fault did not move the branch")
	}
	w.s.Fail("GIT fetch", Fault{Status: 503})
	if _, _, err := git(t, env, dir, "fetch", "-q", "origin"); err == nil {
		t.Error("faulted fetch worked")
	}
	mustGit(t, env, dir, "fetch", "-q", "origin")
	var routes []string
	for _, r := range w.s.Requests() {
		if strings.HasPrefix(r.Route, "GIT ") {
			routes = append(routes, r.Route+" "+itoa(int64(r.Status)))
		}
	}
	joined := strings.Join(routes, "\n")
	for _, want := range []string{"GIT push acme/api 502", "GIT push acme/api refs/heads/b 500", "GIT fetch acme/api 503", "GIT fetch acme/api 200"} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests lack %q:\n%s", want, joined)
		}
	}
	w.noViolations()
}

func TestEnterpriseLayout(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{Flavor: GHES})
	tok := w.token([]string{"api"}, nil)
	if len(tok) != 40 {
		t.Errorf("GHES token of %d characters", len(tok))
	}
	r := doRequest(t, w.s, "GET", w.s.EnterpriseAPIURL()+"/repos/acme/api", tok, nil)
	wantStatus(t, "REST under /api/v3", r, 200)
	if url := r.obj(t)["url"].(string); !strings.HasPrefix(url, w.s.EnterpriseAPIURL()+"/repos/") {
		t.Errorf("url %q", url)
	}
	g := doRequest(t, w.s, "POST", w.s.URL()+"/api/graphql", tok, map[string]any{"query": `{ repository(owner: "acme", name: "api") { name } }`})
	if field(g.obj(t), "data", "repository", "name") != "api" {
		t.Errorf("GraphQL at /api/graphql: %s", g.body)
	}
	clone(t, w.s, "acme/api", tok)
}
