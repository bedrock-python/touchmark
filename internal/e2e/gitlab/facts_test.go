//go:build e2e

package gitlabe2e

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
)

// TestFacts checks through the API and git alone, without the driver, what
// touchmark assumes about GitLab, and what a driver must know besides. Each
// subtest states the claim it checks, fails where GitLab contradicts what
// touchmark depends on and prints what it saw as a FINDING.
func TestFacts(t *testing.T) {
	e := needLive(t)
	finding(t, "version", "GitLab %s (%s); the reader and the writer are %ss", e.Version, e.Image, e.Accounts)
	fx := newGroup(t, e, "facts")
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *fixture)
	}{
		{"duplicate", factDuplicate},
		{"draft", factDraft},
		{"labels", factLabels},
		{"quick-actions", factQuickActions},
		{"closer", factCloser},
		{"source-branch-deleted", factSourceBranchDeleted},
		{"force-push", factForcePush},
		{"push-then-create", factPushThenCreate},
		{"branch-delete-closer", factBranchDeleteCloser},
		{"api-scope-git", factAPIScopeGit},
		{"body-size", factBodySize},
		{"object-format", factObjectFormat},
		{"git-basic", factGitBasic},
		{"members-all", factMembersAll},
		{"rate-limit-headers", factRateLimitHeaders},
		{"revoked-bot", factRevokedBot},
		{"account-type", factAccountType},
		{"fork-visibility", factForkVisibility},
		{"token-self", factTokenSelf},
		{"push-rule", factPushRule},
		{"pagination", factPagination},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, fx) })
	}
}

// readme is the one file most projects of the facts hold.
var readme = []conformance.File{{Path: "README.md", Content: []byte("# facts\n")}}

// factProject creates a project with a README in the facts group.
func factProject(t *testing.T, fx *fixture, name string) platform.Repo {
	t.Helper()
	p := fx.createProject(t, fx.nsID, name, readme)
	fx.env.waitAccess(t, p.ID, fx.env.Reader, fx.env.Writer, fx.env.Person)
	return fx.repo(t, p.ID)
}

// open opens a merge request from head to main as author, with a commit of
// its own on head.
func open(t *testing.T, fx *fixture, repo platform.Repo, author account, head, title, body string) apiMR {
	t.Helper()
	return fx.openMR(t, repo, author, mrOptions{head: head, base: repo.DefaultBranch, title: title, body: body})
}

// factDuplicate: one open merge request per source and target branch; a
// second gets 409 "Another open merge request already exists for this
// source branch: !N".
func factDuplicate(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "duplicate")
	id := projectID(t, repo)
	first := open(t, fx, repo, fx.env.Writer, "touchmark/dup", "first", "first")
	resp := fx.env.api(fx.env.Writer).do(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", id), map[string]any{
		"source_branch": "touchmark/dup", "target_branch": "main", "title": "second",
	})
	if resp.Status != http.StatusConflict {
		t.Errorf("a second open MR on the same branches: HTTP %d, want 409: %s", resp.Status, fx.env.snippet(resp.Body))
	}
	finding(t, "duplicate-mr", "a second MR from touchmark/dup to main while !%d is open: HTTP %d %s",
		first.IID, resp.Status, fx.env.snippet(resp.Body))
	// Another target branch is another pair.
	fx.CreateBranch(t, repo, "release")
	resp = fx.env.api(fx.env.Writer).do(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", id), map[string]any{
		"source_branch": "touchmark/dup", "target_branch": "release", "title": "to release",
	})
	finding(t, "duplicate-mr-other-target", "the same source branch to another target branch while !%d is open: HTTP %d",
		first.IID, resp.Status)
}

// factDraft: the title prefix "Draft: " makes a draft, and so does a
// commit message starting with Draft:.
func factDraft(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "draft")
	id := projectID(t, repo)
	var got []string
	for _, title := range []string{"Draft: prefix", "[Draft] brackets", "(Draft) parens", "WIP: old prefix", "draft: lower case"} {
		head := "draft/" + randHex(t, 3)
		mr := open(t, fx, repo, fx.env.Writer, head, title, "x")
		got = append(got, fmt.Sprintf("%q→draft %v title %q", title, mr.Draft, mr.Title))
		if title == "Draft: prefix" && !mr.Draft {
			t.Errorf("an MR titled %q is not a draft", title)
		}
	}
	finding(t, "draft-titles", "%s", strings.Join(got, "; "))

	mr := open(t, fx, repo, fx.env.Writer, "draft/commit", "ready", "x")
	fx.env.commitFiles(t, fx.env.Person, id, "draft/commit", "", []conformance.File{{Path: "wip.md", Content: []byte("wip\n")}}, "Draft: not done yet")
	after := fx.env.waitMR(t, id, mr.IID, time.Minute, func(m apiMR) bool { return m.Draft })
	finding(t, "draft-commit", "a commit with the message \"Draft: ...\" pushed to the source branch of a ready MR: draft %v, title %q",
		after.Draft, after.Title)
}

// factLabels: create takes labels by name and creates the missing ones; an
// update adds labels through add_labels only.
func factLabels(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "labels")
	id := projectID(t, repo)
	name := "touchmark-" + randHex(t, 3)
	mr := fx.openMR(t, repo, fx.env.Writer, mrOptions{head: "labels/one", base: "main", title: "labels", body: "x", labels: []string{name}})
	labels := all[apiLabel](t, fx.env.api(fx.env.Root), fmt.Sprintf("/projects/%d/labels", id))
	created := slices.ContainsFunc(labels, func(l apiLabel) bool { return l.Name == name })
	if !slices.Contains(mr.Labels, name) || !created {
		t.Errorf("an MR created by the writer (Developer) with the new label %s: labels %v, the project has it: %v", name, mr.Labels, created)
	}
	// A person adds a label; the writer adds another with add_labels.
	var edited apiMR
	fx.env.api(fx.env.Person).ok(t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d", id, mr.IID),
		map[string]any{"add_labels": "person-label"}, nil)
	fx.env.api(fx.env.Writer).ok(t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d", id, mr.IID),
		map[string]any{"add_labels": name + ",second-" + name}, &edited)
	if !slices.Contains(edited.Labels, "person-label") || !slices.Contains(edited.Labels, "second-"+name) {
		t.Errorf("add_labels: labels %v, want the person's label kept and the new one added", edited.Labels)
	}
	finding(t, "labels", "create by name makes the project label %s (%v); add_labels keeps a person's label: %v", name, created, edited.Labels)
}

// factQuickActions: quick actions in a description run on create and on
// update, through the API too: a person's line "/draft" runs; the same text
// inside code, an HTML comment and a table cell, as touchmark's bodies carry
// it, does not.
func factQuickActions(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "quick")
	id := projectID(t, repo)
	person := open(t, fx, repo, fx.env.Person, "quick/person", "quick actions by a person", "A person's description.\n\n/draft\n")
	after := fx.env.mr(t, id, person.IID)
	finding(t, "quick-action-create", "a person's description with a line \"/draft\" on create: draft %v, the stored description %q", after.Draft, after.Description)
	if !after.Draft {
		t.Errorf("a line /draft in a description on create did not run")
	}
	var updated apiMR
	fx.env.api(fx.env.Person).ok(t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d", id, person.IID),
		map[string]any{"description": "Updated.\n\n/ready\n"}, &updated)
	updated = fx.env.mr(t, id, person.IID)
	finding(t, "quick-action-update", "the same MR updated with a line \"/ready\": draft %v, description %q", updated.Draft, updated.Description)

	// Text shaped like touchmark's body: paths in code spans in a table, a
	// fenced block, the marker as an HTML comment.
	body := "Sync from hub `facts`.\n\n| change | file | pack |\n|---|---|---|\n| add | `/close` | base |\n\n" +
		"```\n/close\n/merge\n```\n\n> /close\n\n<!-- touchmark:v1 hub=facts\n/close\n-->"
	bot := open(t, fx, repo, fx.env.Writer, "quick/bot", "touchmark-shaped body", body)
	bot = fx.env.mr(t, id, bot.IID)
	if bot.State != "opened" || bot.Description != body {
		t.Errorf("a touchmark-shaped body with /close in code, a quote and a comment: %s, description kept %v", bot.State, bot.Description == body)
	}
	finding(t, "quick-action-escaped", "/close inside a code span, a fenced block, a quote and an HTML comment of a writer's description: state %s, description kept byte for byte %v",
		bot.State, bot.Description == body)
}

// factCloser: the field closed_by names who closed, for a person
// and for a bot, and merged_by for a merge.
func factCloser(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "closer")
	id := projectID(t, repo)
	byPerson := open(t, fx, repo, fx.env.Writer, "closer/person", "closed by a person", "x")
	fx.env.setState(t, fx.env.Person, id, byPerson.IID, "close")
	byBot := open(t, fx, repo, fx.env.Writer, "closer/bot", "closed by the writer", "x")
	fx.env.setState(t, fx.env.Writer, id, byBot.IID, "close")
	merged := open(t, fx, repo, fx.env.Writer, "closer/merged", "merged by a person", "x")
	fx.env.merge(t, fx.env.Person, id, merged.IID)
	who := func(u *apiUser) string {
		if u == nil {
			return "null"
		}
		return fmt.Sprintf("%s (%d, bot %v)", u.Username, u.ID, u.Bot)
	}
	for _, c := range []struct {
		n    int64
		want account
	}{{byPerson.IID, fx.env.Person}, {byBot.IID, fx.env.Writer}} {
		mr := fx.env.mr(t, id, c.n)
		if mr.ClosedBy == nil || mr.ClosedBy.ID != c.want.ID {
			t.Errorf("!%d closed by %s: closed_by %s", c.n, c.want.Login, who(mr.ClosedBy))
		}
		// The reader sees the same (the list, as the driver reads it).
		var listed []apiMR
		fx.env.api(fx.env.Reader).get(t, fmt.Sprintf("/projects/%d/merge_requests?iids[]=%d&state=all", id, c.n), &listed)
		if len(listed) != 1 || listed[0].ClosedBy == nil || listed[0].ClosedBy.ID != c.want.ID {
			t.Errorf("!%d in the reader's list: %+v", c.n, listed)
		}
	}
	m := fx.env.mr(t, id, merged.IID)
	finding(t, "closed-by", "closed_by: person %s, writer %s; a merge by the person: state %s, merged_by %s, merge_user %s, closed_by %s",
		who(fx.env.mr(t, id, byPerson.IID).ClosedBy), who(fx.env.mr(t, id, byBot.IID).ClosedBy), m.State, who(m.MergedBy), who(m.MergeUser), who(m.ClosedBy))
}

// factSourceBranchDeleted: deleting the source branch closes the MR,
// and who GitLab names as the closer.
func factSourceBranchDeleted(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "branch-deleted")
	id := projectID(t, repo)
	mr := open(t, fx, repo, fx.env.Writer, "touchmark/deleted", "deleted branch", "x")
	fx.env.api(fx.env.Person).ok(t, http.MethodDelete, fmt.Sprintf("/projects/%d/repository/branches/%s", id, url.PathEscape("touchmark/deleted")), nil, nil)
	start := time.Now()
	after := fx.env.waitMR(t, id, mr.IID, time.Minute, func(m apiMR) bool { return m.State != "opened" })
	closer := "null"
	if after.ClosedBy != nil {
		closer = after.ClosedBy.Username
	}
	if after.State != "closed" {
		t.Errorf("!%d after its source branch was deleted: %s, want closed", mr.IID, after.State)
	}
	closedAfter := time.Since(start).Round(100 * time.Millisecond)
	// The close service saves the state first and the closer (the MR's
	// metrics, latest_closed_by) after it: a read in between sees no
	// closer. Wait for the closer, at most 15 s.
	settled := fx.env.waitMR(t, id, mr.IID, 15*time.Second, func(m apiMR) bool { return m.ClosedBy != nil })
	eventual := "still null after 15 s"
	if settled.ClosedBy != nil {
		eventual = fmt.Sprintf("%s after %s", settled.ClosedBy.Username, time.Since(start).Round(100*time.Millisecond))
	}
	finding(t, "source-branch-deleted", "the person deletes the source branch of the writer's !%d: %s after %s, closed_by %s at that read, then %s; state events %s",
		mr.IID, after.State, closedAfter, closer, eventual, fx.env.stateEvents(t, id, mr.IID))
	resp := fx.env.api(fx.env.Writer).do(t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d", id, mr.IID), map[string]any{"state_event": "reopen"})
	var re apiMR
	decode(t, "reopen", resp, &re)
	time.Sleep(3 * time.Second)
	later := fx.env.mr(t, id, mr.IID)
	finding(t, "reopen-without-branch", "a reopen of !%d without its source branch: HTTP %d, the answer says %s; read again 3 s later: %s",
		mr.IID, resp.Status, re.State, later.State)
}

// factForcePush: a force push keeps the MR: a force push of the
// source branch keeps the MR open at the new head, also one that resets the
// branch to its target.
func factForcePush(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "force")
	id := projectID(t, repo)
	mr := open(t, fx, repo, fx.env.Writer, "touchmark/force", "force push", "x")
	dir := t.TempDir()
	auth := fx.env.gitAuth(fx.env.Writer)
	remote := fx.env.remote(repo.Path)
	gitCmd(t, dir, nil, nil, "init", "-q")
	gitCmd(t, dir, nil, auth, "fetch", "-q", remote, "refs/heads/main:refs/heads/main", "refs/heads/touchmark/force:refs/heads/old")
	tree := gitCmd(t, dir, nil, nil, "rev-parse", "old^{tree}")
	rewritten := gitCmd(t, dir, nil, identity(fx.env.Writer, fx.env.Writer.Login+"@example.com"), "commit-tree", tree, "-p", "main", "-m", "rewritten")
	gitCmd(t, dir, nil, auth, "push", "-q", "--force-with-lease=refs/heads/touchmark/force:"+mr.SHA, remote, rewritten+":refs/heads/touchmark/force")
	after := fx.env.waitMR(t, id, mr.IID, 30*time.Second, func(m apiMR) bool { return m.SHA == rewritten })
	if after.State != "opened" || after.SHA != rewritten {
		t.Errorf("!%d after a force push: %s at %s, want opened at %s", mr.IID, after.State, after.SHA, rewritten)
	}
	main := gitCmd(t, dir, nil, nil, "rev-parse", "main")
	gitCmd(t, dir, nil, auth, "push", "-q", "--force-with-lease=refs/heads/touchmark/force:"+rewritten, remote, main+":refs/heads/touchmark/force")
	reset := fx.env.waitMR(t, id, mr.IID, 30*time.Second, func(m apiMR) bool { return m.SHA == main })
	finding(t, "force-push", "a force push keeps !%d %s at the new head %v; reset to its target branch: %s at the base %v",
		mr.IID, after.State, after.SHA == rewritten, reset.State, reset.SHA == main)
}

// factPushThenCreate: how soon after a git push the merge request API takes
// the new branch as a source branch. GitLab checks it against the
// project's cached branch names, which the push's background job refreshes
// (the driver's CreatePR takes a 400 "source_branch does not exist" for a
// branch the branches API finds as transient). The project's cache is
// warmed first, as a target's always is: a merge request of another
// branch reads it. Each round pushes a new branch as the writer and, at
// once, asks HEAD /repository/branches/:branch (the cached view), GET of
// the same (Gitaly) and POST /merge_requests until the merge request is
// opened, and records when each first saw the branch.
func factPushThenCreate(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "push-create")
	id := projectID(t, repo)
	open(t, fx, repo, fx.env.Writer, "warm/cache", "warm the branch cache", "x")
	w := fx.env.api(fx.env.Writer)
	var got []string
	var raced int
	for round := 0; round < 3; round++ {
		branch := fmt.Sprintf("touchmark/push-%d-%s", round, randHex(t, 3))
		fx.env.pushBranch(t, fx.env.Writer, repo.Path, "main", branch)
		pushed := time.Now()
		var head, get, created time.Duration = -1, -1, -1
		first := 0
		for created < 0 && time.Since(pushed) < time.Minute {
			if head < 0 && w.do(t, http.MethodHead, fmt.Sprintf("/projects/%d/repository/branches/%s", id, url.PathEscape(branch)), nil).Status/100 == 2 {
				head = time.Since(pushed)
			}
			if get < 0 && w.do(t, http.MethodGet, fmt.Sprintf("/projects/%d/repository/branches/%s", id, url.PathEscape(branch)), nil).Status == http.StatusOK {
				get = time.Since(pushed)
			}
			resp := w.do(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", id), map[string]any{
				"source_branch": branch, "target_branch": "main", "title": "push then create " + branch,
			})
			if first == 0 {
				first = resp.Status
				if resp.Status != http.StatusCreated {
					raced++
					got = append(got, fmt.Sprintf("round %d: the first POST answered %d %s", round, resp.Status, fx.env.snippet(resp.Body)))
				}
			}
			switch {
			case resp.Status == http.StatusCreated:
				created = time.Since(pushed)
			case resp.Status != http.StatusBadRequest:
				t.Fatalf("POST /merge_requests from %s: HTTP %d %s", branch, resp.Status, fx.env.snippet(resp.Body))
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
		if created < 0 {
			t.Errorf("round %d: no merge request from %s a minute after the push", round, branch)
		}
		got = append(got, fmt.Sprintf("round %d: HEAD branch after %s, GET branch after %s, merge request after %s",
			round, head.Round(10*time.Millisecond), get.Round(10*time.Millisecond), created.Round(10*time.Millisecond)))
	}
	finding(t, "push-then-create", "%d of 3 rounds refused the merge request right after the push; %s", raced, strings.Join(got, "; "))
}

// factBranchDeleteCloser: whom GitLab names as the closer of a merge
// request that a deleted source branch closed, as touchmark meets it: the
// writer pushes the sync branch with git and opens the merge request, the
// person deletes the branch, either at once (the push's background job may
// not have run yet) or 20 s later. Each round records closed_by as the
// merge request API reports it 15 s after the close, and the author of the
// system note "closed".
func factBranchDeleteCloser(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "delete-closer")
	id := projectID(t, repo)
	w := fx.env.api(fx.env.Writer)
	var got []string
	for round, settle := range []time.Duration{0, 0, 20 * time.Second, 20 * time.Second} {
		branch := fmt.Sprintf("touchmark/closer-%d-%s", round, randHex(t, 3))
		fx.env.pushFiles(t, fx.env.Writer, repo.Path, branch, []conformance.File{{Path: "sync.md", Content: []byte(branch + "\n")}}, "sync")
		var mr apiMR
		deadline := time.Now().Add(time.Minute)
		for {
			resp := w.do(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", id), map[string]any{
				"source_branch": branch, "target_branch": "main", "title": "closer " + branch,
			})
			if resp.Status == http.StatusCreated {
				decode(t, "mr", resp, &mr)
				break
			}
			if resp.Status != http.StatusBadRequest || time.Now().After(deadline) {
				t.Fatalf("POST /merge_requests from %s: HTTP %d %s", branch, resp.Status, fx.env.snippet(resp.Body))
			}
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(settle)
		fx.env.api(fx.env.Person).ok(t, http.MethodDelete, fmt.Sprintf("/projects/%d/repository/branches/%s", id, url.PathEscape(branch)), nil, nil)
		closed := fx.env.waitMR(t, id, mr.IID, time.Minute, func(m apiMR) bool { return m.State != "opened" })
		time.Sleep(15 * time.Second)
		final := fx.env.mr(t, id, mr.IID)
		by := "null"
		if final.ClosedBy != nil {
			by = final.ClosedBy.Username
		}
		got = append(got, fmt.Sprintf("round %d (deleted %s after the MR): %s, closed_by %s, state events %s",
			round, settle, closed.State, by, fx.env.stateEvents(t, id, mr.IID)))
		if final.ClosedBy != nil && final.ClosedBy.ID == fx.env.Writer.ID {
			finding(t, "branch-delete-closer-writer", "round %d: GitLab names the writer, not the person who deleted the branch, as the closer", round)
		}
	}
	finding(t, "branch-delete-closer", "%s", strings.Join(got, "; "))
}

// stateEvents lists the state events of merge request n of the project
// id (GET .../resource_state_events) as "state by user".
func (e *liveEnv) stateEvents(t testing.TB, id, n int64) string {
	t.Helper()
	var out []string
	for _, ev := range all[struct {
		State string   `json:"state"`
		User  *apiUser `json:"user"`
	}](t, e.api(e.Root), fmt.Sprintf("/projects/%d/merge_requests/%d/resource_state_events", id, n)) {
		who := "nobody"
		if ev.User != nil {
			who = ev.User.Username
		}
		out = append(out, ev.State+" by "+who)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// factAPIScopeGit: whether the scope api of a group or project token covers
// Git over HTTP; GitLab's documentation says it does not, hence the
// writer's write_repository. A group access token with the scope api alone,
// Developer, reads and pushes over git, or does not.
func factAPIScopeGit(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "api-scope")
	tok := fx.env.groupAccessToken(t, fx.nsID, "api-only", levelDeveloper, []string{"api"})
	fx.env.waitAccess(t, projectID(t, repo), tok.account)
	remote := fx.env.remote(repo.Path)
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q")
	auth := fx.env.gitAuth(tok.account)
	_, lsErr := gitTry(dir, auth, "ls-remote", remote, "refs/heads/main")
	gitCmd(t, dir, nil, fx.env.gitAuth(fx.env.Root), "fetch", "-q", remote, "refs/heads/main:refs/heads/main")
	_, pushErr := gitTry(dir, auth, "push", "-q", remote, "main:refs/heads/api-scope")
	finding(t, "api-scope-git", "a group access token with the scope api alone (Developer): git ls-remote %s, git push %s", okOr(lsErr), okOr(pushErr))
}

// factBodySize: the driver's budget for a description is 200 000 bytes,
// under GitLab's limit of 1 MiB: a description of 200 000 bytes and one
// just over 1 MiB.
func factBodySize(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "body-size")
	id := projectID(t, repo)
	var got []string
	for i, n := range []int{200000, 1 << 20, 1<<20 + 1} {
		head := fmt.Sprintf("body/%d", i)
		fx.commit(t, fx.env.Writer, id, "main", head)
		body := strings.Repeat("a", n-1) + "\n"
		resp := fx.env.api(fx.env.Writer).do(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", id), map[string]any{
			"source_branch": head, "target_branch": "main", "title": "body size", "description": body,
		})
		kept := ""
		if resp.Status == http.StatusCreated {
			var mr apiMR
			decode(t, "mr", resp, &mr)
			kept = fmt.Sprintf(", stored %d bytes", len(fx.env.mr(t, id, mr.IID).Description))
		} else {
			kept = ": " + fx.env.snippet(resp.Body)
		}
		got = append(got, fmt.Sprintf("%d bytes: HTTP %d%s", n, resp.Status, kept))
		if n == 200000 && resp.Status != http.StatusCreated {
			t.Errorf("a description of %d bytes (the driver's budget): HTTP %d", n, resp.Status)
		}
	}
	finding(t, "body-size", "%s", strings.Join(got, "; "))
}

// factObjectFormat: a project reports its object format in
// repository_object_format. CE creates sha256 projects only with the feature
// flag support_sha256_repositories; the fact records what the instance does.
func factObjectFormat(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "sha1")
	var p apiProject
	fx.env.api(fx.env.Reader).get(t, "/projects/"+repo.ID, &p)
	finding(t, "object-format", "a new project: repository_object_format %q as the reader reads it", p.RepositoryObjectFormat)
	if p.RepositoryObjectFormat != "sha1" {
		t.Errorf("repository_object_format %q, want sha1", p.RepositoryObjectFormat)
	}
	sha, how := fx.env.sha256Project(t, fx.nsID, "sha256-"+randHex(t, 3))
	finding(t, "object-format-sha256", "a project created with repository_object_format sha256 (%s): %q", how, sha.RepositoryObjectFormat)
}

// sha256Project creates a project of the sha256 object format in the group
// with the id namespace, with a README, turning on the feature flag
// support_sha256_repositories when the first attempt makes a sha1 project.
// It returns the project and how it was made.
func (e *liveEnv) sha256Project(t testing.TB, namespace int64, name string) (apiProject, string) {
	t.Helper()
	create := func(n string) apiProject {
		p := e.createProject(t, map[string]any{
			"name": n, "path": n, "namespace_id": namespace, "visibility": "private",
			"initialize_with_readme": true, "default_branch": "main", "repository_object_format": "sha256",
		})
		e.api(e.Root).get(t, fmt.Sprintf("/projects/%d", p.ID), &p)
		return p
	}
	p := create(name)
	if p.RepositoryObjectFormat == "sha256" {
		return p, "by default"
	}
	resp := e.api(e.Root).do(t, http.MethodPost, "/features/support_sha256_repositories", map[string]any{"value": true})
	how := fmt.Sprintf("the first attempt made %q; POST /features/support_sha256_repositories: HTTP %d", p.RepositoryObjectFormat, resp.Status)
	return create(name + "-flag"), how
}

// factGitBasic: git over HTTP takes a token as Basic credentials only
// (oauth2:<token>): which user names GitLab takes with a token, and what a
// push with the reader's token gets.
func factGitBasic(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "git-basic")
	remote := fx.env.remote(repo.Path)
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q")
	// The commit every push below sends, fetched with root's token.
	gitCmd(t, dir, nil, fx.env.gitAuth(fx.env.Root), "fetch", "-q", remote, "refs/heads/main:refs/heads/main")
	var got []string
	for _, c := range []struct {
		what string
		env  []string
	}{
		{"oauth2:<writer token>", fx.env.basicAuth("oauth2", fx.env.Writer.Token)},
		{"<writer login>:<writer token>", fx.env.basicAuth(fx.env.Writer.Login, fx.env.Writer.Token)},
		{"someone-else:<writer token>", fx.env.basicAuth(fx.env.Person.Login, fx.env.Writer.Token)},
		{"x-access-token:<writer token>", fx.env.basicAuth("x-access-token", fx.env.Writer.Token)},
		{"oauth2:<reader token>", fx.env.basicAuth("oauth2", fx.env.Reader.Token)},
		{"Bearer <writer token>", []string{
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + fx.env.URL + "/.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Bearer " + fx.env.Writer.Token,
		}},
	} {
		_, lsErr := gitTry(dir, c.env, "ls-remote", remote, "refs/heads/main")
		branch := "basic/" + randHex(t, 3)
		_, pushErr := gitTry(dir, c.env, "push", "-q", remote, fx.Head(t, repo)+":refs/heads/"+branch)
		got = append(got, fmt.Sprintf("%s: ls-remote %s, push %s", c.what, okOr(lsErr), okOr(pushErr)))
		if c.what == "oauth2:<writer token>" && (lsErr != nil || pushErr != nil) {
			t.Errorf("git with %s: ls-remote %v, push %v", c.what, lsErr, pushErr)
		}
		if c.what == "oauth2:<reader token>" && pushErr == nil {
			t.Errorf("the reader's token (read_repository, Reporter) pushed")
		}
	}
	finding(t, "git-basic", "%s", strings.Join(got, "; "))
}

// okOr is "ok", or the first line of err's message.
func okOr(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return "refused (" + msg + ")"
}

// factMembersAll: a Reporter reads the writer's members/all entry: the
// reader sees the writer's inherited role in a target, as plan --strict
// checks it.
func factMembersAll(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "members")
	var m apiMember
	resp := fx.env.api(fx.env.Reader).do(t, http.MethodGet, fmt.Sprintf("/projects/%s/members/all/%d", repo.ID, fx.env.Writer.ID), nil)
	decode(t, "members/all", resp, &m)
	if resp.Status != http.StatusOK || m.AccessLevel != levelDeveloper {
		t.Errorf("the reader's GET members/all/<writer>: HTTP %d, access level %d, want 200 and %d", resp.Status, m.AccessLevel, levelDeveloper)
	}
	var p struct {
		Permissions struct {
			ProjectAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	fx.env.api(fx.env.Writer).get(t, "/projects/"+repo.ID, &p)
	project, group := 0, 0
	if p.Permissions.ProjectAccess != nil {
		project = p.Permissions.ProjectAccess.AccessLevel
	}
	if p.Permissions.GroupAccess != nil {
		group = p.Permissions.GroupAccess.AccessLevel
	}
	finding(t, "members-all", "the reader reads the writer's role in a project of a subgroup through members/all: HTTP %d, level %d; the writer's own permissions: project_access %d, group_access %d",
		resp.Status, m.AccessLevel, project, group)
}

// factRateLimitHeaders: what a default self-managed instance sends about
// rate limits (RateLimit-* headers pause the provider's queue).
func factRateLimitHeaders(t *testing.T, fx *fixture) {
	resp := fx.env.api(fx.env.Reader).do(t, http.MethodGet, "/projects?membership=true&per_page=1", nil)
	var names []string
	for k := range resp.Header {
		if l := strings.ToLower(k); strings.Contains(l, "ratelimit") || strings.Contains(l, "retry-after") {
			names = append(names, k+"="+resp.Header.Get(k))
		}
	}
	slices.Sort(names)
	finding(t, "rate-limit-headers", "GET /projects on a default instance: HTTP %d, rate-limit headers %v", resp.Status, names)
}

// factRevokedBot: whether the bot of a revoked token can still be found
// through the API, which migrate relies on. The bot of a group access
// token opens a merge request, as a multi-gitter hub's did; then the token
// is revoked, and for
// 90 s the fact watches what GitLab does with the bot (a revoke may delete
// the bot user from a background job, DeleteUserWorker, and move its
// records to the Ghost User) and with the author of its merge request.
func factRevokedBot(t *testing.T, fx *fixture) {
	group := fx.sideGroup(t, "bot", nil)
	gid := fx.groupID(t, group)
	proto := fx.env.groupAccessToken(t, gid, "multi-gitter", levelDeveloper, []string{"api", "write_repository"})
	p := fx.createProject(t, gid, "revoked", readme)
	fx.env.waitAccess(t, p.ID, proto.account)
	fx.env.commitFiles(t, proto.account, p.ID, legacyBranch, "main", []conformance.File{{Path: "AGENTS.md", Content: []byte("x\n")}}, "chore: sync")
	var mr apiMR
	fx.env.api(proto.account).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", p.ID), map[string]any{
		"source_branch": legacyBranch, "target_branch": "main", "title": "multi-gitter",
	}, &mr)
	fx.env.revokeGroupToken(t, gid, proto)
	revoked := time.Now()
	var seen []string
	last := ""
	for time.Since(revoked) < 90*time.Second {
		resp := fx.env.api(fx.env.Reader).do(t, http.MethodGet, fmt.Sprintf("/users/%d", proto.ID), nil)
		var u apiUser
		decode(t, "user", resp, &u)
		var byName []apiUser
		fx.env.api(fx.env.Reader).get(t, "/users?username="+url.QueryEscape(proto.Login), &byName)
		now := fx.env.mr(t, p.ID, mr.IID)
		state := fmt.Sprintf("GET /users/:id HTTP %d state %q; by username %d; the MR's author %s (%d)",
			resp.Status, u.State, len(byName), now.Author.Username, now.Author.ID)
		if state != last {
			seen = append(seen, fmt.Sprintf("after %s: %s", time.Since(revoked).Round(time.Second), state))
			last = state
		}
		time.Sleep(3 * time.Second)
	}
	finding(t, "revoked-bot", "the bot %s (id %d) of a group access token opened !%d, then the token was revoked: %s",
		proto.Login, proto.ID, mr.IID, strings.Join(seen, "; "))
}

// factAccountType: where the API tells a bot from a person (memory tells
// bots from people by the account's kind). The users API
// answers with a "bot" field in some views only; the fact records which
// view shows what for the writer and the person, as the reader reads them,
// and what user_type an administrator sees.
func factAccountType(t *testing.T, fx *fixture) {
	type view struct {
		Bot      *bool   `json:"bot"`
		UserType *string `json:"user_type"`
	}
	show := func(v view) string {
		out := "bot " + "absent"
		if v.Bot != nil {
			out = fmt.Sprintf("bot %v", *v.Bot)
		}
		if v.UserType != nil {
			out += ", user_type " + *v.UserType
		}
		return out
	}
	var got []string
	for _, a := range []account{fx.env.Writer, fx.env.Person} {
		var byID, byRoot view
		var byName []view
		fx.env.api(fx.env.Reader).get(t, fmt.Sprintf("/users/%d", a.ID), &byID)
		fx.env.api(fx.env.Reader).get(t, "/users?username="+url.QueryEscape(a.Login), &byName)
		fx.env.api(fx.env.Root).get(t, fmt.Sprintf("/users/%d", a.ID), &byRoot)
		name := "absent"
		if len(byName) == 1 {
			name = show(byName[0])
		}
		got = append(got, fmt.Sprintf("%s: the reader's GET /users/:id %s; GET /users?username= %s; root's GET /users/:id %s",
			a.Login, show(byID), name, show(byRoot)))
	}
	var self view
	fx.env.api(fx.env.Writer).get(t, "/user", &self)
	got = append(got, "the writer's own GET /user "+show(self))
	finding(t, "account-type", "%s", strings.Join(got, "; "))
}

// factForkVisibility: what a fork tells about its source to someone who
// cannot read the source (forks are skipped unless a selector takes them;
// the driver tells them by forked_from_project).
func factForkVisibility(t *testing.T, fx *fixture) {
	hidden := fx.sideGroup(t, "hidden", nil)
	src := fx.createProject(t, fx.groupID(t, hidden), "source", readme)
	var fork apiProject
	fx.root().ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/fork", src.ID), map[string]any{
		"namespace_path": fx.ns, "name": "fork-of-hidden", "path": "fork-of-hidden",
	}, &fork)
	fork = fx.waitForked(t, fork.ID)
	fx.env.waitAccess(t, fork.ID, fx.env.Reader)
	var asReader, asRoot map[string]any
	fx.env.api(fx.env.Reader).get(t, fmt.Sprintf("/projects/%d", fork.ID), &asReader)
	fx.root().get(t, fmt.Sprintf("/projects/%d", fork.ID), &asRoot)
	_, readerHas := asReader["forked_from_project"]
	_, rootHas := asRoot["forked_from_project"]
	var forks []apiProject
	fx.env.api(fx.env.Reader).get(t, fmt.Sprintf("/groups/%d/projects?per_page=100", fx.nsID), &forks)
	listed := "absent"
	for _, p := range forks {
		if p.ID == fork.ID {
			listed = fmt.Sprintf("forked_from_project set %v", p.ForkedFrom != nil)
		}
	}
	finding(t, "fork-visibility", "a fork of a project the reader cannot read: the reader's GET /projects/:id has forked_from_project %v (root's: %v); in the group listing: %s",
		readerHas && asReader["forked_from_project"] != nil, rootHas && asRoot["forked_from_project"] != nil, listed)
}

// factTokenSelf: doctor reads a token's expiry from
// /personal_access_tokens/self, here with the reader's and the writer's
// tokens.
func factTokenSelf(t *testing.T, fx *fixture) {
	var got []string
	for _, a := range []account{fx.env.Reader, fx.env.Writer} {
		var tok struct {
			Scopes    []string `json:"scopes"`
			ExpiresAt string   `json:"expires_at"`
			Active    bool     `json:"active"`
		}
		resp := fx.env.api(a).do(t, http.MethodGet, "/personal_access_tokens/self", nil)
		decode(t, "token", resp, &tok)
		got = append(got, fmt.Sprintf("%s: HTTP %d scopes %v expires %s", a.Login, resp.Status, tok.Scopes, tok.ExpiresAt))
		if resp.Status != http.StatusOK || tok.ExpiresAt == "" {
			t.Errorf("GET /personal_access_tokens/self as %s: HTTP %d, expires %q", a.Login, resp.Status, tok.ExpiresAt)
		}
	}
	finding(t, "token-self", "%s", strings.Join(got, "; "))
}

// factPushRule: push rules show only at push time, since GET /push_rule
// needs Maintainer; here on CE.
func factPushRule(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "push-rule")
	w := fx.env.api(fx.env.Writer).do(t, http.MethodGet, "/projects/"+repo.ID+"/push_rule", nil)
	r := fx.env.api(fx.env.Root).do(t, http.MethodGet, "/projects/"+repo.ID+"/push_rule", nil)
	finding(t, "push-rule", "GET /projects/:id/push_rule on CE: the writer (Developer) HTTP %d, root HTTP %d", w.Status, r.Status)
}

// factPagination: the pagination headers of the listings the driver reads.
func factPagination(t *testing.T, fx *fixture) {
	repo := factProject(t, fx, "pages")
	id := projectID(t, repo)
	for i := 0; i < 3; i++ {
		open(t, fx, repo, fx.env.Writer, fmt.Sprintf("pages/%d", i), "page", "x")
	}
	var got []string
	for _, p := range []string{
		fmt.Sprintf("/projects/%d/merge_requests?state=all&per_page=2", id),
		fmt.Sprintf("/groups/%d/projects?include_subgroups=true&per_page=2", fx.nsID),
		fmt.Sprintf("/projects/%d/merge_requests/1/notes?per_page=2", id),
	} {
		resp := fx.env.api(fx.env.Reader).do(t, http.MethodGet, p, nil)
		link := "no"
		if resp.Header.Get("Link") != "" {
			link = "yes"
		}
		got = append(got, fmt.Sprintf("%s: HTTP %d X-Total %q X-Total-Pages %q X-Next-Page %q Link %s",
			strings.SplitN(p, "?", 2)[0], resp.Status, resp.Header.Get("X-Total"), resp.Header.Get("X-Total-Pages"), resp.Header.Get("X-Next-Page"), link))
	}
	finding(t, "pagination", "%s", strings.Join(got, "; "))
}
