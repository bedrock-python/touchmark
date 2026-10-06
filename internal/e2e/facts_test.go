//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
)

// TestFacts checks through the API and git alone, without the driver, what
// touchmark assumes about Gitea and Forgejo, and what a driver must know
// besides. Each subtest states the claim it checks, fails where the platform
// contradicts what touchmark depends on and prints what it saw as a FINDING
// (a contradiction touchmark copes with, such as labels by name refused on
// create, is only printed).
func TestFacts(t *testing.T) {
	e := needLive(t)
	v := forgeVersion(t, e)
	finding(t, "version", "%s", v)
	fx := newOrg(t, e, "facts")
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *fixture, version)
	}{
		{"flavor", factFlavor},
		{"pull-list", factPullList},
		{"closer", factCloser},
		{"draft", factDraft},
		{"labels", factLabels},
		{"duplicate", factDuplicate},
		{"body", factBody},
		{"sha256", factSHA256},
		{"branch-deletion", factBranchDeletion},
		{"manual-merge", factManualMerge},
		{"sweep", factSweep},
		{"git-basic", factGitBasic},
		{"signed-commits", factSignedCommits},
		{"rate-limit-headers", factRateLimitHeaders},
		{"sudo", factSudo},
		{"token-introspection", factToken},
		{"pagination", factPagination},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, fx, v) })
	}
}

// version is what the forge says it is.
type version struct {
	Forgejo bool
	// Raw is /api/v1/version; ForgejoRaw /api/forgejo/v1/version.
	Raw, ForgejoRaw string
	// ForgejoStatus is the status of /api/forgejo/v1/version.
	ForgejoStatus int
	// Major and Minor are the product's: Gitea's from /api/v1/version,
	// Forgejo's from /api/forgejo/v1/version.
	Major, Minor int
}

func (v version) String() string {
	if v.Forgejo {
		return fmt.Sprintf("Forgejo %s (/api/v1/version says %q)", v.ForgejoRaw, v.Raw)
	}
	return fmt.Sprintf("Gitea %s (/api/forgejo/v1/version: HTTP %d)", v.Raw, v.ForgejoStatus)
}

// atLeast reports whether the product's version is major.minor or later.
func (v version) atLeast(major, minor int) bool {
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

// forgeVersion reads both version endpoints anonymously.
func forgeVersion(t *testing.T, e *liveEnv) version {
	t.Helper()
	read := func(path string) (int, string) {
		var out struct {
			Version string `json:"version"`
		}
		resp, err := e.HTTP.JSON(context.Background(), http.MethodGet, e.URL+path, nil, nil, &out)
		if resp == nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp.Status, out.Version
	}
	var v version
	_, v.Raw = read("/api/v1/version")
	v.ForgejoStatus, v.ForgejoRaw = read("/api/forgejo/v1/version")
	v.Forgejo = v.ForgejoStatus == http.StatusOK
	own := v.Raw
	if v.Forgejo {
		own = v.ForgejoRaw
	}
	parts := strings.SplitN(strings.SplitN(own, "+", 2)[0], ".", 3)
	if len(parts) >= 2 {
		v.Major, _ = strconv.Atoi(parts[0])
		v.Minor, _ = strconv.Atoi(parts[1])
	}
	return v
}

// factRepo creates a repository with a README in the facts organisation.
func factRepo(t *testing.T, fx *fixture, name string) platform.Repo {
	t.Helper()
	return fx.CreateRepo(t, conformance.RepoSpec{Name: name, Files: []conformance.File{{Path: "README.md", Content: []byte("# " + name + "\n")}}})
}

// open opens a pull request from head to main as author.
func open(t *testing.T, fx *fixture, repo platform.Repo, author account, head, title string) apiPR {
	t.Helper()
	return fx.openPR(t, repo, author, prOptions{head: head, base: repo.DefaultBranch, title: title, body: title})
}

// numbersOf returns the numbers of prs.
func numbersOf(prs []apiPR) []int64 {
	out := make([]int64, 0, len(prs))
	for _, pr := range prs {
		out = append(out, pr.Number)
	}
	return out
}

// factFlavor: Forgejo answers 200 on /api/forgejo/v1/version, and its
// /api/v1/version carries the frozen suffix +gitea-1.22.0, which the gates
// on Gitea's version must not read.
func factFlavor(t *testing.T, fx *fixture, v version) {
	if v.Forgejo != (fx.env.Flavor == "forgejo") {
		t.Errorf("/api/forgejo/v1/version answers %d on %s (%s)", v.ForgejoStatus, fx.env.Flavor, fx.env.Image)
	}
	if v.Forgejo && !strings.Contains(v.Raw, "+gitea-") {
		t.Errorf("Forgejo's /api/v1/version is %q, without the +gitea- suffix touchmark expects", v.Raw)
	}
	finding(t, "flavor", "/api/v1/version = %q; /api/forgejo/v1/version: HTTP %d %q", v.Raw, v.ForgejoStatus, v.ForgejoRaw)
}

// factPullList: Gitea has no head filter (pulls?state=all&poster=<login>
// lists the writer's pull requests, and the branch is read from the head on
// the client), and pulls/{base}/{head} answers one pull request without a
// defined order; Forgejo 16 applies head=, Forgejo 15 is like Gitea. Whether
// head= matches a fork's pull request is printed.
func factPullList(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "pull-list")
	first := open(t, fx, repo, e.Writer, "sync", "first")
	fx.SetPRState(t, repo, first.Number, platform.Closed, conformance.RoleWriter)
	second := open(t, fx, repo, e.Writer, "sync", "second")
	other := open(t, fx, repo, e.Person, "other", "other branch")
	fork := fx.openPR(t, repo, e.Person, prOptions{head: "sync", base: "main", title: "from a fork", body: "fork", fork: true})
	reader := e.api(e.Reader)

	var byHead []apiPR
	reader.get(t, "/repos/"+repo.Path+"/pulls?state=all&limit=50&head=sync", &byHead)
	got := numbersOf(byHead)
	filters := !slices.Contains(got, other.Number)
	want := v.Forgejo && v.Major >= 16
	if filters != want {
		t.Errorf("the head filter should apply on Forgejo ≥ 16 only; here (%s) it applies: %v (%v)", v, filters, got)
	}
	finding(t, "head-filter", "pulls?state=all&head=sync answers %v of #%d, #%d (sync), #%d (other), #%d (a fork's sync): the filter applies %v; it matches the fork's pull request %v",
		got, first.Number, second.Number, other.Number, fork.Number, filters, slices.Contains(got, fork.Number))

	var byPoster []apiPR
	reader.get(t, "/repos/"+repo.Path+"/pulls?state=all&limit=50&poster="+url.QueryEscape(e.Writer.Login), &byPoster)
	if got := numbersOf(byPoster); !slices.Equal(sorted64(got), []int64{first.Number, second.Number}) {
		t.Errorf("pulls?state=all&poster=%s answers %v, want the writer's #%d and #%d", e.Writer.Login, got, first.Number, second.Number)
	}

	resp := reader.do(t, http.MethodGet, "/repos/"+repo.Path+"/pulls/main/sync", nil)
	var one apiPR
	if resp.Status == http.StatusOK {
		decode(t, "pulls/main/sync", resp, &one)
	}
	finding(t, "pulls-base-head", "pulls/main/sync: HTTP %d, #%d (%s), while #%d is closed and #%d open on that head",
		resp.Status, one.Number, one.State, first.Number, second.Number)

	f := fx.pr(t, repo, fork.Number)
	finding(t, "fork-head", "a fork's pull request reports head label %q, ref %q, repo_id %d (the target's is %d)",
		f.Head.Label, f.Head.Ref, f.Head.RepoID, f.Base.RepoID)
	if f.Head.RepoID == f.Base.RepoID {
		t.Errorf("a fork's pull request reports the target as its head repository")
	}
}

// pr returns pull request n of repo as the admin sees it.
func (fx *fixture) pr(t testing.TB, repo platform.Repo, n int64) apiPR {
	t.Helper()
	var pr apiPR
	fx.admin().get(t, fmt.Sprintf("/repos/%s/pulls/%d", repo.Path, n), &pr)
	return pr
}

// factCloser: the timeline names who closed a pull request, which is what
// lets the driver report CloserKnown. The reader must see who closed, the
// last close counts, a merge names its merger.
func factCloser(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "closer")
	byPerson := open(t, fx, repo, e.Writer, "by-person", "closed by the person")
	fx.SetPRState(t, repo, byPerson.Number, platform.Closed, conformance.RoleOther)
	byWriter := open(t, fx, repo, e.Writer, "by-writer", "closed by the writer")
	fx.SetPRState(t, repo, byWriter.Number, platform.Closed, conformance.RoleWriter)
	again := open(t, fx, repo, e.Writer, "again", "closed, reopened, closed again")
	fx.SetPRState(t, repo, again.Number, platform.Closed, conformance.RoleOther)
	fx.SetPRState(t, repo, again.Number, platform.Open, conformance.RoleOther)
	fx.SetPRState(t, repo, again.Number, platform.Closed, conformance.RoleWriter)
	merged := open(t, fx, repo, e.Person, "merged", "merged by the writer")
	fx.SetPRState(t, repo, merged.Number, platform.Merged, conformance.RoleWriter)

	for _, tc := range []struct {
		n  int64
		by account
	}{{byPerson.Number, e.Person}, {byWriter.Number, e.Writer}, {again.Number, e.Writer}} {
		ev, ok := lastEvent(t, e, e.Reader, repo.Path, tc.n, "close")
		switch {
		case !ok:
			t.Errorf("#%d: no close event in the timeline the reader reads", tc.n)
		case ev.User == nil || ev.User.Login != tc.by.Login:
			t.Errorf("#%d: the last close event names %v, want %s", tc.n, ev.User, tc.by.Login)
		}
	}
	pr := fx.pr(t, repo, merged.Number)
	if !pr.Merged || pr.MergedBy == nil || pr.MergedBy.Login != e.Writer.Login {
		t.Errorf("#%d: merged %v by %v, want merged by %s", merged.Number, pr.Merged, pr.MergedBy, e.Writer.Login)
	}
	var events []apiTimeline
	e.api(e.Reader).get(t, fmt.Sprintf("/repos/%s/issues/%d/timeline?limit=50", repo.Path, merged.Number), &events)
	var types []string
	for _, ev := range events {
		who := ""
		if ev.User != nil {
			who = ev.User.Login
		}
		types = append(types, ev.Type+"/"+who)
	}
	finding(t, "closer", "the timeline (read:issue) names the closer in the last event of type \"close\"; a merge shows %v and merged_by %s",
		types, e.Writer.Login)
}

// factDraft: a title starting with WIP: makes a draft (the instance can
// change the prefixes).
func factDraft(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "draft")
	var drafts []string
	for i, title := range []string{"WIP: a draft", "[WIP] a draft", "Draft: a draft", "wip: lower case", "chore: sync engineering assets"} {
		pr := open(t, fx, repo, e.Writer, "draft-"+strconv.Itoa(i), title)
		if pr.Draft {
			drafts = append(drafts, strconv.Quote(title))
		}
		if title == "WIP: a draft" && !pr.Draft {
			t.Errorf("a title starting with WIP: should make a draft; %q did not", title)
		}
		if title == "chore: sync engineering assets" && pr.Draft {
			t.Errorf("touchmark's default title makes a draft")
		}
	}
	finding(t, "draft", "titles that make a draft: %s", strings.Join(drafts, ", "))
}

// factLabels: create takes label ids only; unknown labels are dropped
// without an error.
func factLabels(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "labels")
	ids := fx.labelIDs(t, repo.Path, []string{"fact-label"})
	byID := fx.openPR(t, repo, e.Writer, prOptions{head: "by-id", base: "main", title: "labels by id", body: "x", labels: []string{"fact-label"}})
	if !slices.ContainsFunc(byID.Labels, func(l apiLabel) bool { return l.ID == ids[0] }) {
		t.Errorf("a pull request created with label id %d has labels %v", ids[0], byID.Labels)
	}
	fx.commit(t, e.Writer, repo.Path, "by-name")
	resp := e.api(e.Writer).do(t, http.MethodPost, "/repos/"+repo.Path+"/pulls", map[string]any{
		"head": "by-name", "base": "main", "title": "labels by name", "body": "x", "labels": []string{"fact-label"},
	})
	var byName apiPR
	if resp.Status/100 == 2 {
		decode(t, "create", resp, &byName)
	}
	fx.commit(t, e.Writer, repo.Path, "unknown-id")
	resp2 := e.api(e.Writer).do(t, http.MethodPost, "/repos/"+repo.Path+"/pulls", map[string]any{
		"head": "unknown-id", "base": "main", "title": "an unknown label id", "body": "x", "labels": []int64{999999},
	})
	var byUnknown apiPR
	if resp2.Status/100 == 2 {
		decode(t, "create", resp2, &byUnknown)
	}
	// Adding labels to an existing pull request by name (the issue labels
	// endpoint), as a driver may do after creating it.
	resp3 := e.api(e.Writer).do(t, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo.Path, byID.Number),
		map[string]any{"labels": []string{"fact-label", "no-such-label"}})
	var after []apiLabel
	if resp3.Status/100 == 2 {
		decode(t, "labels", resp3, &after)
	}
	finding(t, "labels", "create with label ids works; with label names: HTTP %d (labels %v); with an unknown id: HTTP %d (labels %v); "+
		"POST issues/{n}/labels with names, one unknown: HTTP %d, labels %v",
		resp.Status, byName.Labels, resp2.Status, byUnknown.Labels, resp3.Status, after)
}

// factDuplicate: a pull request from a head to a base that already have an
// open one is refused with 409, which CreatePR turns into ErrExists with the
// open one; one to another base, or after the open one is closed, is created.
func factDuplicate(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "duplicate")
	first := open(t, fx, repo, e.Writer, "dup", "first")
	create := func(base, title string) (int, int64, string) {
		t.Helper()
		resp := e.api(e.Writer).do(t, http.MethodPost, "/repos/"+repo.Path+"/pulls", map[string]any{
			"head": "dup", "base": base, "title": title, "body": title,
		})
		var pr apiPR
		if resp.Status/100 == 2 {
			decode(t, "create", resp, &pr)
		}
		return resp.Status, pr.Number, strings.TrimSpace(e.snippet(resp.Body))
	}
	dup, _, msg := create("main", "the same head and base")
	if dup != http.StatusConflict {
		t.Errorf("a second pull request from dup to main while #%d is open: HTTP %d, want 409: %s", first.Number, dup, msg)
	}
	fx.commit(t, e.Person, repo.Path, "release")
	other, otherN, otherMsg := create("release", "the same head, another base")
	fx.SetPRState(t, repo, first.Number, platform.Closed, conformance.RoleWriter)
	after, afterN, afterMsg := create("main", "after the close")
	if after != http.StatusCreated {
		t.Errorf("a pull request from dup to main after #%d was closed: HTTP %d, want 201: %s", first.Number, after, afterMsg)
	}
	if other/100 == 2 {
		otherMsg = fmt.Sprintf("#%d", otherN)
	}
	finding(t, "duplicate", "POST pulls from dup to main while #%d is open: HTTP %d %s; from dup to another base: HTTP %d %s; "+
		"from dup to main once #%d is closed: HTTP %d (#%d)", first.Number, dup, msg, other, otherMsg, first.Number, after, afterN)
}

// factBody: the API sets no limit on a body (the driver's budget is
// 58 000 bytes all the same): bodies up to 1 MiB are kept byte for byte.
func factBody(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "body")
	for _, size := range []int{65_536, 262_144, 1 << 20} {
		body := strings.Repeat("0123456789abcdef\n", size/17) + strings.Repeat("x", size%17)
		fx.commit(t, e.Writer, repo.Path, "body-"+strconv.Itoa(size))
		resp := e.api(e.Writer).do(t, http.MethodPost, "/repos/"+repo.Path+"/pulls", map[string]any{
			"head": "body-" + strconv.Itoa(size), "base": "main", "title": "a body of " + strconv.Itoa(size), "body": body,
		})
		if resp.Status/100 != 2 {
			finding(t, "body", "a body of %d bytes: HTTP %d", size, resp.Status)
			if size <= 65_536 {
				t.Errorf("a body of %d bytes is refused: HTTP %d", size, resp.Status)
			}
			continue
		}
		var pr apiPR
		decode(t, "create", resp, &pr)
		got := fx.pr(t, repo, pr.Number)
		if got.Body != body {
			t.Errorf("a body of %d bytes came back with %d bytes", size, len(got.Body))
		}
		finding(t, "body", "a body of %d bytes is kept byte for byte", size)
	}
}

// factSHA256: a repository reports its object format in
// object_format_name.
func factSHA256(t *testing.T, fx *fixture, v version) {
	resp := fx.admin().do(t, http.MethodPost, "/orgs/"+fx.org+"/repos", map[string]any{
		"name": "sha256", "private": true, "auto_init": true, "default_branch": "main", "object_format_name": "sha256",
	})
	if resp.Status/100 != 2 {
		finding(t, "sha256", "creating a sha256 repository: HTTP %d: %s", resp.Status, fx.env.snippet(resp.Body))
		t.Skip("the forge does not create sha256 repositories")
	}
	var r apiRepo
	fx.admin().get(t, "/repos/"+fx.org+"/sha256", &r)
	var b apiBranch
	fx.admin().get(t, "/repos/"+fx.org+"/sha256/branches/main", &b)
	if r.ObjectFormatName != "sha256" || len(b.Commit.ID) != 64 {
		t.Errorf("a sha256 repository reports object_format_name %q and a head of %d hex digits", r.ObjectFormatName, len(b.Commit.ID))
	}
	finding(t, "sha256", "object_format_name %q, head %d hex digits", r.ObjectFormatName, len(b.Commit.ID))
}

// factBranchDeletion: deleting the head branch of an open pull request
// closes it; and what happens to a pull request whose base is deleted,
// which memory treats as an automatic close, not a decline.
func factBranchDeletion(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "deletion")
	doomed := open(t, fx, repo, e.Writer, "doomed", "its head is deleted")
	fx.commit(t, e.Person, repo.Path, "release")
	based := fx.openPR(t, repo, e.Writer, prOptions{head: "feature", base: "release", title: "its base is deleted", body: "x"})

	fx.admin().ok(t, http.MethodDelete, "/repos/"+repo.Path+"/branches/doomed", nil, nil)
	pr := waitPR(t, fx, repo, doomed.Number, func(pr apiPR) bool { return pr.State != "open" })
	if pr.State != "closed" || pr.Merged {
		t.Errorf("#%d after its head branch was deleted: state %s, merged %v; want closed", doomed.Number, pr.State, pr.Merged)
	}
	ev, _ := lastEvent(t, e, e.Reader, repo.Path, doomed.Number, "close")
	closer := "nobody"
	if ev.User != nil {
		closer = ev.User.Login
	}
	finding(t, "head-deleted", "deleting the head branch closes #%d (closer in the timeline: %s); its head reads label %q, ref %q, sha %.12s",
		doomed.Number, closer, pr.Head.Label, pr.Head.Ref, pr.Head.SHA)

	fx.admin().ok(t, http.MethodDelete, "/repos/"+repo.Path+"/branches/release", nil, nil)
	pr = waitPR(t, fx, repo, based.Number, func(pr apiPR) bool { return pr.State != "open" || pr.Base.Ref != "release" })
	switch {
	case pr.State == "open" && pr.Base.Ref == "release":
		t.Errorf("#%d stays open on the deleted base release", based.Number)
	case pr.State == "open":
		finding(t, "base-deleted", "deleting the base branch release moves the open #%d onto %s", based.Number, pr.Base.Ref)
	default:
		finding(t, "base-deleted", "deleting the base branch release closes #%d (base still %q)", based.Number, pr.Base.Ref)
	}
}

// waitPR re-reads pull request n until done holds, for at most 30 s: the
// forges adjust pull requests after a push or a deletion in the background.
func waitPR(t *testing.T, fx *fixture, repo platform.Repo, n int64, done func(apiPR) bool) apiPR {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		pr := fx.pr(t, repo, n)
		if done(pr) || time.Now().After(deadline) {
			return pr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// factManualMerge: with autodetect_manual_merge on, a pull request whose head
// reaches the base is recorded as merged, by the one who pushed the base.
func factManualMerge(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "manual-merge")
	fx.admin().ok(t, http.MethodPatch, "/repos/"+repo.Path, map[string]any{"autodetect_manual_merge": true}, nil)
	pr := open(t, fx, repo, e.Writer, "manual", "merged by a push to main")
	e.pushBranch(t, e.Person, repo.Path, "manual", "main")
	got := waitPR(t, fx, repo, pr.Number, func(pr apiPR) bool { return pr.State != "open" })
	if !got.Merged {
		t.Errorf("#%d after main reached its head, with autodetect_manual_merge: state %s, merged %v; want merged",
			pr.Number, got.State, got.Merged)
	}
	by := "nobody"
	if got.MergedBy != nil {
		by = got.MergedBy.Login
	}
	finding(t, "manual-merge", "with autodetect_manual_merge, %s's push that makes main reach the head of #%d (opened by %s) records it as merged %v by %s",
		e.Person.Login, pr.Number, e.Writer.Login, got.Merged, by)
}

// factSweep: on Gitea 1.26 and later, GET
// /repos/issues/search?type=pulls&state=open&created_by=<login> lists one
// author's open pull requests; Forgejo has no created_by (it ignores the
// filter), and created=true lists the requesting account's own.
// A filter applies when the answer holds the writer's pull request and no
// open pull request of anyone else, although the person has some.
func factSweep(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "sweep")
	pr := open(t, fx, repo, e.Writer, "sweep", "for the sweep")
	open(t, fx, repo, e.Person, "sweep-person", "by the person")
	type issue struct {
		Number     int64   `json:"number"`
		User       apiUser `json:"user"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	// search returns whether the answer holds the writer's pull request and
	// whether it holds only the writer's, retrying for 15 s: the forge's
	// issue indexer may lag behind.
	search := func(as account, query string) (found, only bool, status int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			resp := e.api(as).do(t, http.MethodGet, "/repos/issues/search?type=pulls&state=open&limit=50&"+query, nil)
			if resp.Status != http.StatusOK {
				return false, false, resp.Status
			}
			var answer []issue
			decode(t, "search", resp, &answer)
			found = slices.ContainsFunc(answer, func(i issue) bool { return i.Number == pr.Number && i.Repository.FullName == repo.Path })
			only = !slices.ContainsFunc(answer, func(i issue) bool { return i.User.Login != e.Writer.Login })
			if found || time.Now().After(deadline) {
				return found, only, resp.Status
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	byFound, byOnly, byStatus := search(e.Reader, "created_by="+url.QueryEscape(e.Writer.Login))
	createdFound, createdOnly, createdStatus := search(e.Writer, "created=true")
	if !v.Forgejo && v.atLeast(1, 26) && (!byFound || !byOnly) {
		t.Errorf("created_by should list the writer's open pull requests, and only them, to the reader on Gitea ≥ 1.26: found %v, only the writer's %v (HTTP %d)",
			byFound, byOnly, byStatus)
	}
	if !createdFound || !createdOnly {
		t.Errorf("created=true should list the writer's own open pull requests, and only them: found %v, only the writer's %v (HTTP %d)",
			createdFound, createdOnly, createdStatus)
	}
	finding(t, "sweep", "issues/search as the reader with created_by=<writer>: HTTP %d, finds its pull request %v, the filter applies %v; "+
		"as the writer with created=true: HTTP %d, finds it %v, the filter applies %v",
		byStatus, byFound, byOnly, createdStatus, createdFound, createdOnly)
}

// factGitBasic: git over HTTP takes an access token as the password of
// Basic credentials, whatever the user name (the token reaches git only
// as "Authorization: Basic …" in http.extraHeader). The
// driver sends the user x-access-token, the harness the bot's login; the
// token alone decides who reads and pushes. On Gitea the reader and the
// writer are users of type bot.
func factGitBasic(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "git-basic")
	remote := e.remote(repo.Path)
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q")
	gitCmd(t, dir, nil, e.gitAuth(e.Writer), "fetch", "-q", remote, "refs/heads/main")
	tip := gitCmd(t, dir, nil, nil, "rev-parse", "FETCH_HEAD")
	read := func(auth []string) error {
		_, err := gitTry(dir, auth, "ls-remote", remote, "refs/heads/main")
		return err
	}
	push := func(auth []string, branch string) error {
		_, err := gitTry(dir, auth, "push", "-q", remote, tip+":refs/heads/"+branch)
		return err
	}
	wrong := strings.Repeat("0", len(e.Writer.Token))
	var seen []string
	for _, tc := range []struct {
		what string
		err  error
		ok   bool
	}{
		{"read with the writer's login and token", read(e.gitAuth(e.Writer)), true},
		{"read with x-access-token and the writer's token", read(e.basicAuth("x-access-token", e.Writer.Token)), true},
		{"read with the writer's login and a wrong token", read(e.basicAuth(e.Writer.Login, wrong)), false},
		{"read without credentials", read(nil), false},
		{"push with x-access-token and the writer's token", push(e.basicAuth("x-access-token", e.Writer.Token), "basic-x-access-token"), true},
		{"push with the reader's login and the writer's token", push(e.basicAuth(e.Reader.Login, e.Writer.Token), "basic-writer-token"), true},
		{"push with the writer's login and the reader's token", push(e.basicAuth(e.Writer.Login, e.Reader.Token), "basic-reader-token"), false},
	} {
		if (tc.err == nil) != tc.ok {
			t.Errorf("%s: %v, want success %v", tc.what, tc.err, tc.ok)
		}
		outcome := "works"
		if tc.err != nil {
			outcome = "refused (" + lastLine(tc.err.Error()) + ")"
		}
		seen = append(seen, tc.what+": "+outcome)
	}
	kind := "users"
	if e.Flavor == "gitea" {
		kind = "users of type bot"
	}
	finding(t, "git-basic", "the reader and the writer are %s; %s", kind, strings.Join(seen, "; "))
}

// factSignedCommits: whether the writer can see that a branch protection
// requires signed commits, and what a push of an unsigned commit into a
// branch such a rule protects answers: gitx takes "protected from
// unverified commit" for blocked:cannot-sign.
func factSignedCommits(t *testing.T, fx *fixture, v version) {
	e := fx.env
	repo := factRepo(t, fx, "signed")
	const pattern = "touchmark/*"
	// A branch the rule protects, created before the rule.
	fx.commit(t, e.Person, repo.Path, "touchmark/existing")
	fx.admin().ok(t, http.MethodPost, "/repos/"+repo.Path+"/branch_protections", map[string]any{
		"rule_name": pattern, "enable_push": true, "require_signed_commits": true,
	}, nil)
	writer := e.api(e.Writer)
	rules := writer.do(t, http.MethodGet, "/repos/"+repo.Path+"/branch_protections", nil)
	rule := writer.do(t, http.MethodGet, "/repos/"+repo.Path+"/branch_protections/"+url.PathEscape(pattern), nil)
	var branch map[string]any
	writer.get(t, "/repos/"+repo.Path+"/branches/"+url.PathEscape("touchmark/existing"), &branch)
	var keys []string
	for k := range branch {
		if k != "commit" && k != "name" {
			keys = append(keys, fmt.Sprintf("%s=%v", k, branch[k]))
		}
	}
	slices.Sort(keys)

	// An unsigned commit on top of main, pushed by the writer to a new branch
	// under the rule.
	remote := e.remote(repo.Path)
	auth := e.gitAuth(e.Writer)
	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q")
	gitCmd(t, dir, nil, auth, "fetch", "-q", remote, "refs/heads/main")
	who := []string{
		"GIT_AUTHOR_NAME=" + e.Writer.Login, "GIT_AUTHOR_EMAIL=" + e.Writer.Login + "@example.com",
		"GIT_COMMITTER_NAME=" + e.Writer.Login, "GIT_COMMITTER_EMAIL=" + e.Writer.Login + "@example.com",
	}
	commit := gitCmd(t, dir, nil, who, "commit-tree", "FETCH_HEAD^{tree}", "-p", "FETCH_HEAD", "-m", "unsigned")
	_, pushErr := gitTry(dir, auth, "push", remote, commit+":refs/heads/touchmark/unsigned")
	switch {
	case pushErr == nil:
		t.Errorf("an unsigned commit was pushed to a branch whose rule requires signed commits")
	case strings.Contains(pushErr.Error(), "protected from unverified commit"):
	case e.Flavor == "gitea" && strings.Contains(pushErr.Error(), "no message for end users"):
		// Gitea fails its own check (seen on 1.26.4 and 1.27.3):
		// verifyCommits returns the unverified commit wrapped, the
		// pre-receive hook no longer recognizes it and answers 500
		// (routers/private/hook_verification.go, hook_pre_receive.go; the
		// forge logs "Unable to check commits …: Unverified commit: <sha>").
		// The push is refused all the same; gitx sees a policy refusal, not
		// an unsigned one.
	default:
		t.Errorf("the refusal of an unsigned commit does not say %q, which gitx looks for: %v", "protected from unverified commit", pushErr)
	}
	refusal := "none"
	if pushErr != nil {
		var lines []string
		for line := range strings.SplitSeq(pushErr.Error(), "\n") {
			if l := strings.TrimSpace(line); strings.Contains(l, "unverified") || strings.Contains(l, "declined") || strings.Contains(l, "end users") {
				lines = append(lines, l)
			}
		}
		refusal = strings.Join(lines, " | ")
	}
	finding(t, "signed-commits", "with a rule %q requiring signed commits, the writer reads GET branch_protections: HTTP %d, "+
		"GET branch_protections/{rule}: HTTP %d, GET branches/{b} of a protected branch: %v; an unsigned push is refused: %s",
		pattern, rules.Status, rule.Status, keys, refusal)
}

// factRateLimitHeaders: an instance has no rate limits of its own. The
// answers of a default instance carry no rate-limit header, so the driver's
// reading of them (RateLimit, Retry-After, X-RateLimit-*) has only its unit
// tests: Codeberg's limits, announced in RateLimit headers, are not seen by
// this run.
func factRateLimitHeaders(t *testing.T, fx *fixture, v version) {
	e := fx.env
	paths := []string{"/version", "/user", "/repos/search?limit=1", "/repos/issues/search?type=pulls&limit=1"}
	var seen []string
	for _, p := range paths {
		resp := e.api(e.Reader).do(t, http.MethodGet, p, nil)
		for name, values := range resp.Header {
			if l := strings.ToLower(name); strings.Contains(l, "ratelimit") || l == "retry-after" {
				seen = append(seen, p+" "+name+": "+strings.Join(values, ", "))
			}
		}
	}
	slices.Sort(seen)
	finding(t, "rate-limit-headers", "rate-limit headers in the answers to %d reads: %v", len(paths), seen)
}

// factSudo: touchmark refuses an admin token, since through Sudo it acts
// as anyone.
func factSudo(t *testing.T, fx *fixture, v version) {
	e := fx.env
	var u apiUser
	e.api(e.Admin).get(t, "/user?sudo="+url.QueryEscape(e.Person.Login), &u)
	if u.Login != e.Person.Login {
		t.Errorf("an admin token with sudo=%s acts as %s", e.Person.Login, u.Login)
	}
	finding(t, "sudo", "an admin token with ?sudo=%s acts as %s", e.Person.Login, u.Login)
}

// factToken: GET /api/v1/token describes the token on Gitea 1.27 and
// later, for doctor's check of its scopes. Only the
// scopes of the answer are printed: it also carries the token's last eight
// characters.
func factToken(t *testing.T, fx *fixture, v version) {
	e := fx.env
	resp := e.api(e.Writer).do(t, http.MethodGet, "/token", nil)
	if !v.Forgejo && v.atLeast(1, 27) && resp.Status != http.StatusOK {
		t.Errorf("GET /api/v1/token should answer on Gitea ≥ 1.27; HTTP %d", resp.Status)
	}
	var info struct {
		Scopes []string `json:"scopes"`
	}
	if resp.Status == http.StatusOK {
		decode(t, "token", resp, &info)
	}
	finding(t, "token-introspection", "GET /api/v1/token with the writer's token: HTTP %d, scopes %v", resp.Status, info.Scopes)
}

// factPagination: how the listings the driver reads tell where they end
// (listAll in internal/platform/gitea/client.go): a Link header with
// rel="next", and an X-Total-Count that counts the whole listing or only
// the page. The driver ends a listing by X-Total-Count only where it counts
// the listing (trustTotal), and the fixtures of the driver's unit tests
// (pageHeaders) send what this sees. Each listing is read as the reader,
// in an organisation of its own, with more items than a page of one.
func factPagination(t *testing.T, fx *fixture, v version) {
	e := fx.env
	org := newOrg(t, e, "pages")
	repo := factRepo(t, org, "pages")
	factRepo(t, org, "pages-2")
	org.labelIDs(t, repo.Path, []string{"page-a", "page-b", "page-c"})
	for _, name := range []string{"org-a", "org-b"} {
		org.admin().ok(t, http.MethodPost, "/orgs/"+org.org+"/labels", map[string]any{"name": name, "color": "#ededed"}, nil)
	}
	var first apiPR
	for i := range 3 {
		pr := open(t, org, repo, e.Writer, fmt.Sprintf("page-%d", i), "a page")
		if i == 0 {
			first = pr
		}
	}
	for i := range 3 {
		e.api(e.Person).ok(t, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo.Path, first.Number),
			map[string]any{"body": fmt.Sprintf("comment %d", i)}, nil)
	}
	reader := e.api(e.Reader)
	with := func(path, query string) string {
		if strings.Contains(path, "?") {
			return path + "&" + query
		}
		return path + "?" + query
	}
	var seen []string
	for _, l := range []struct {
		name, path string
		// trusted: the driver ends this listing by X-Total-Count.
		trusted bool
	}{
		{"pulls", "/repos/" + repo.Path + "/pulls?state=all", true},
		{"org repos", "/orgs/" + org.org + "/repos", true},
		{"repo labels", "/repos/" + repo.Path + "/labels", true},
		{"org labels", "/orgs/" + org.org + "/labels", true},
		{"issue search", "/repos/issues/search?type=pulls&state=open&owner=" + org.org, true},
		{"timeline", fmt.Sprintf("/repos/%s/issues/%d/timeline", repo.Path, first.Number), false},
	} {
		// The size of the listing; the issue search may lag behind (its
		// indexer works from a queue).
		var all []json.RawMessage
		deadline := time.Now().Add(15 * time.Second)
		for {
			reader.get(t, with(l.path, "limit=50"), &all)
			if len(all) >= 2 || time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if len(all) < 2 {
			t.Errorf("%s: %d items, want at least 2 to tell a total from a page", l.name, len(all))
			continue
		}
		resp := reader.do(t, http.MethodGet, with(l.path, "limit=1&page=1"), nil)
		if resp.Status != http.StatusOK {
			t.Errorf("%s: a page of one: HTTP %d", l.name, resp.Status)
			continue
		}
		next := false
		for _, link := range resp.Header.Values("Link") {
			next = next || strings.Contains(link, `rel="next"`)
		}
		total := resp.Header.Get("X-Total-Count")
		kind := "absent"
		switch total {
		case strconv.Itoa(len(all)):
			kind = "the listing"
		case "1":
			kind = "the page"
		case "":
		default:
			kind = "neither"
		}
		if l.trusted && kind != "the listing" {
			t.Errorf("%s: X-Total-Count is %q for %d items (it counts %s): the driver, which ends this listing by it, would stop early",
				l.name, total, len(all), kind)
		}
		seen = append(seen, fmt.Sprintf("%s: Link next %v, X-Total-Count counts %s", l.name, next, kind))
	}
	finding(t, "pagination", "a page of one of %d listings: %s", len(seen), strings.Join(seen, "; "))
}

func sorted64(ns []int64) []int64 {
	out := slices.Clone(ns)
	slices.Sort(out)
	return out
}
