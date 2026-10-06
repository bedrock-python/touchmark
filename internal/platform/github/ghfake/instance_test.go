package ghfake

import (
	"strings"
	"testing"
)

// TestMeta: github.com's /meta names no version; GHES names it there and in
// a header of every REST answer.
func TestMeta(t *testing.T) {
	d := newWorld(t, Options{})
	m := d.call("GET", "/meta", "", nil)
	if _, ok := m.obj(t)["installed_version"]; ok || m.header.Get("X-GitHub-Enterprise-Version") != "" {
		t.Errorf("github.com /meta: %s, header %q", m.body, m.header.Get("X-GitHub-Enterprise-Version"))
	}
	g := newWorld(t, Options{Flavor: GHES})
	m = g.call("GET", "/api/v3/meta", "", nil)
	if got := m.obj(t)["installed_version"]; got != defaultGHESVersion {
		t.Errorf("GHES /meta installed_version %v", got)
	}
	for _, path := range []string{"/api/v3/meta", "/api/v3/repos/acme/api", "/api/v3/repos/acme/missing"} {
		if r := g.call("GET", path, g.token(nil, nil), nil); r.header.Get("X-GitHub-Enterprise-Version") != defaultGHESVersion {
			t.Errorf("GHES %s: X-GitHub-Enterprise-Version %q", path, r.header.Get("X-GitHub-Enterprise-Version"))
		}
	}
	if a, _ := g.s.Account("alice"); !strings.HasSuffix(a.Email, "@users.noreply.127.0.0.1") {
		t.Errorf("GHES noreply address %q", a.Email)
	}
	if a, _ := d.s.Account("alice"); !strings.HasSuffix(a.Email, "+alice@users.noreply.github.com") {
		t.Errorf("github.com noreply address %q", a.Email)
	}
}

// TestHEADRef: HEAD names the default branch in git/trees, contents and
// commits, not in git/commits.
func TestHEADRef(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token(nil, nil)
	head := w.s.Branch("acme/api", "main")
	for path, want := range map[string]int{
		"/repos/acme/api/git/trees/HEAD":              200,
		"/repos/acme/api/contents/README.md?ref=HEAD": 200,
		"/repos/acme/api/commits/HEAD":                200,
		"/repos/acme/api/git/commits/HEAD":            404,
	} {
		if r := w.call("GET", path, tok, nil); r.status != want {
			t.Errorf("GET %s: %d, want %d: %s", path, r.status, want, r.body)
		}
	}
	if sha := field(w.call("GET", "/repos/acme/api/commits/HEAD", tok, nil).obj(t), "sha"); sha != head {
		t.Errorf("commits/HEAD is %v, main %s", sha, head)
	}
}

// TestCommitFileAndBaseRef: Commit.file(path:) gives the entry of any type
// and NOT_FOUND on the field for a missing path or one through a symlink;
// PullRequest.baseRef is null once the base branch is gone.
func TestCommitFileAndBaseRef(t *testing.T) {
	w := newWorld(t, Options{})
	w.commit("acme/api", CommitSpec{Author: "alice", Files: []File{
		{Path: "docs/a.md", Content: []byte("a\n")},
		{Path: "link", Mode: ModeSymlink, Content: []byte("docs")},
		{Path: "lib", Mode: ModeGitlink, Content: []byte("0123456789abcdef0123456789abcdef01234567")},
	}})
	tok := w.token(nil, nil)
	q := `{ repository(owner: "acme", name: "api") { object(expression: "HEAD") { ... on Commit {
  f: file(path: "docs/a.md") { mode type } l: file(path: "link") { mode type } s: file(path: "lib") { mode type object { __typename } }
  m: file(path: "no/such") { mode } t: file(path: "link/a.md") { mode } } } } }`
	g := w.graphql(tok, q, nil)
	for alias, mode := range map[string]float64{"f": 33188, "l": 40960, "s": 57344} {
		if got := field(g, "data", "repository", "object", alias, "mode"); got != mode {
			t.Errorf("file %s: mode %v, want %v", alias, got, mode)
		}
	}
	errs, _ := g["errors"].([]any)
	if len(errs) != 2 || field(g, "errors", 0, "type") != "NOT_FOUND" || field(g, "errors", 0, "path", 2) != "m" ||
		field(g, "errors", 0, "message") != "Could not resolve file for path 'no/such'." || field(g, "errors", 1, "path", 2) != "t" {
		t.Errorf("missing paths: %v", g["errors"])
	}
	w.branch("acme/api", "release")
	w.branch("acme/api", "feature")
	pr := w.openPR("acme/api", PRSpec{Head: "feature", Base: "release", Title: "t", Author: "alice"})
	check(t, w.s.SetPRState("acme/api", pr.Number, "closed", "alice"))
	pq := `{ repository(owner: "acme", name: "api") { pullRequests(first: 5, states: [CLOSED]) { nodes { baseRefName baseRef { name } } } } }`
	if got := field(w.graphql(tok, pq, nil), "data", "repository", "pullRequests", "nodes", 0, "baseRef", "name"); got != "release" {
		t.Errorf("baseRef before the deletion: %v", got)
	}
	check(t, w.s.SetBranch("acme/api", "release", "", "alice"))
	after := w.graphql(tok, pq, nil)
	if got := field(after, "data", "repository", "pullRequests", "nodes", 0); field(got, "baseRef") != nil || field(got, "baseRefName") != "release" {
		t.Errorf("baseRef after the deletion: %v", got)
	}
}

// TestValidate: the exported validator accepts documents of the subset
// and refuses others with GitHub's messages.
func TestValidate(t *testing.T) {
	ok := `query($o: String!, $n: String!) { repository(owner: $o, name: $n) { pullRequests(first: 1) { nodes { baseRef { name } author { login ... on Mannequin { databaseId } } } } } }`
	for _, f := range []Flavor{DotCom, GHES} {
		if errs := Validate(f, ok); len(errs) > 0 {
			t.Errorf("%s: %q", f, errs)
		}
	}
	if errs := Validate(GHES, `{ repository(owner: "o", name: "n") { hasPullRequestsEnabled } }`); len(errs) != 1 {
		t.Errorf("GHES 3.19 lacks hasPullRequestsEnabled: %q", errs)
	}
	if errs := Validate(DotCom, `query($unused: String) { viewer { login } }`); len(errs) != 1 {
		t.Errorf("an unused variable: %q", errs)
	}
	if errs := Validate(DotCom, `{ viewer { `); len(errs) != 1 {
		t.Errorf("a syntax error: %q", errs)
	}
}
