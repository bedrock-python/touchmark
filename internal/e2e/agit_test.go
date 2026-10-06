//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
)

// TestAGitPR: a pull request opened through AGit (a push to refs/for/<base>
// with the push option topic) has no head branch,
// even when its topic is named like a sync branch. The driver never lists
// it as a pull request from a branch: otherwise anyone who may push for
// review could make touchmark see someone else's open pull request on its
// sync branch (branch-in-use) and stop it.
func TestAGitPR(t *testing.T) {
	e := needLive(t)
	fx := newOrg(t, e, "agit")
	repo := fx.CreateRepo(t, conformance.RepoSpec{Name: "agit", Files: []conformance.File{{Path: "README.md", Content: []byte("# agit\n")}}})
	const topic = "touchmark/agit"
	remote := e.remote(repo.Path)
	auth := e.gitAuth(e.Person)
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q")
	gitCmd(t, dir, nil, auth, "fetch", "-q", remote, "refs/heads/main")
	gitCmd(t, dir, nil, nil, "read-tree", "FETCH_HEAD")
	blob := gitCmd(t, dir, []byte("agit\n"), nil, "hash-object", "-w", "--stdin")
	gitCmd(t, dir, nil, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+",agit.md")
	tree := gitCmd(t, dir, nil, nil, "write-tree")
	who := []string{
		"GIT_AUTHOR_NAME=" + e.Person.Login, "GIT_AUTHOR_EMAIL=" + e.Person.Login + "@example.com",
		"GIT_COMMITTER_NAME=" + e.Person.Login, "GIT_COMMITTER_EMAIL=" + e.Person.Login + "@example.com",
	}
	commit := gitCmd(t, dir, nil, who, "commit-tree", tree, "-p", "FETCH_HEAD", "-m", "agit")
	gitCmd(t, dir, nil, auth, "push", "-q", "-o", "topic="+topic, "-o", "title=an AGit pull request",
		remote, commit+":refs/for/main")

	var raw []map[string]any
	fx.admin().get(t, "/repos/"+repo.Path+"/pulls?state=open&limit=50", &raw)
	if len(raw) != 1 {
		t.Fatalf("%d open pull requests after the AGit push, want 1", len(raw))
	}
	data, err := json.Marshal(raw[0])
	if err != nil {
		t.Fatal(err)
	}
	var pr struct {
		apiPR
		Flow *int `json:"flow"`
	}
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	flow := "absent"
	if pr.Flow != nil {
		flow = strconv.Itoa(*pr.Flow)
	}
	finding(t, "agit", "an AGit pull request with the topic %s: head label %q, ref %q, repo_id %d (the target's %d), flow %s",
		topic, pr.Head.Label, pr.Head.Ref, pr.Head.RepoID, pr.Base.RepoID, flow)

	var person apiUser
	fx.admin().get(t, "/users/"+e.Person.Login, &person)
	author := platform.Account{ID: strconv.FormatInt(person.ID, 10), Login: person.Login}
	r, _ := newDrivers(t, e)
	heads := []string{topic, e.Person.Login + "/" + topic}
	if pr.Head.Label != "" {
		heads = append(heads, pr.Head.Label)
	}
	for _, authors := range [][]platform.Account{nil, {author}} {
		got, err := r.PRs(context.Background(), repo, heads, authors)
		if err != nil {
			t.Fatalf("PRs: %v", err)
		}
		if slices.ContainsFunc(got, func(p platform.PR) bool { return p.Number == pr.Number }) {
			t.Errorf("PRs(%q, %d authors) lists the AGit pull request #%d as one from a branch", heads, len(authors), pr.Number)
		}
	}
	resp := fx.admin().do(t, http.MethodGet, "/repos/"+repo.Path+"/branches/"+topic, nil)
	if resp.Status != http.StatusNotFound {
		t.Errorf("the AGit push made a branch %s: HTTP %d", topic, resp.Status)
	}
}
