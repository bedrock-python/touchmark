package github

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Branches of the tests.
const (
	syncBranch  = "touchmark/acme"
	aliasBranch = "chore/sync-engineering-assets"
)

// gqlActorOf is an actor as GraphQL names it: a bot's login without
// "[bot]" (recorded on github.com).
func gqlActorOf(typename, login string, id int64) map[string]any {
	return map[string]any{"__typename": typename, "login": strings.TrimSuffix(login, "[bot]"), "databaseId": id}
}

// gqlPRNode is a pull request as gqlPRFields reads it.
func gqlPRNode(number, repoID, headRepoID int64, head, base string, author map[string]any, state string, baseExists bool) map[string]any {
	var baseRef any
	if baseExists {
		baseRef = map[string]any{"name": base}
	}
	var headRepo any
	if headRepoID != 0 {
		headRepo = map[string]any{"databaseId": headRepoID}
	}
	return map[string]any{
		"id": "PR_" + itoa(number), "databaseId": number + 9000, "number": number,
		"url": "https://github.com/acme/api/pull/" + itoa(number), "state": state, "isDraft": false,
		"title": "sync", "body": "body of #" + itoa(number), "createdAt": "2026-09-05T08:00:00Z",
		"closedAt": nil, "mergedAt": nil, "headRefName": head, "headRefOid": strings.Repeat("f", 40),
		"baseRefName": base, "baseRef": baseRef, "headRepository": headRepo,
		"repository": map[string]any{"databaseId": repoID, "nameWithOwner": "acme/api"},
		"author":     author, "labels": map[string]any{"nodes": []any{map[string]any{"name": "engineering-assets"}}},
	}
}

// closersRoute answers queryClosers from closers by node id.
func (s *apiServer) closersRoute(t *testing.T, closers map[string]map[string]any) {
	s.graphql("timelineItems(itemTypes: [CLOSED_EVENT]", func(w http.ResponseWriter, req gqlCall) {
		var nodes []any
		for _, id := range req.Variables["ids"].([]any) {
			c, ok := closers[id.(string)]
			if !ok {
				t.Errorf("closer of %v asked", id)
				nodes = append(nodes, nil)
				continue
			}
			nodes = append(nodes, c)
		}
		gqlData(w, map[string]any{"nodes": nodes})
	})
}

// closedBy is a closers node of pull request id ("PR_<number>") of
// acme/api: merged by, else closed by actor.
func closedBy(id string, mergedBy, actor map[string]any) map[string]any {
	var events []any
	if actor != nil {
		events = append(events, map[string]any{"createdAt": "2026-09-10T12:00:00Z", "actor": actor})
	}
	return map[string]any{"number": atoi(strings.TrimPrefix(id, "PR_")), "repository": map[string]any{"databaseId": 101},
		"mergedBy": mergedBy, "timelineItems": map[string]any{"nodes": events}}
}

// TestPRs: the owner-prefixed head filter in every state, open pull
// requests from forks through GraphQL, closers from ClosedEvent and
// mergedBy, the base's existence from the branch API, newest first.
func TestPRs(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	base := repo(101, "acme/api")
	writer := platform.Account{ID: "5001", Login: "touchmark-write[bot]", Kind: platform.KindBot}
	f.handle(http.MethodGet, "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "all" || !strings.HasPrefix(q.Get("head"), "acme:") {
			t.Errorf("pulls query %v: want state all and an owner-prefixed head", q)
		}
		var items []any
		switch q.Get("head") {
		case "acme:" + syncBranch:
			items = []any{
				pr(base, prSpec{number: 10, author: botUser, head: syncBranch, state: "merged"}),
				pr(base, prSpec{number: 9, author: botUser, head: syncBranch, state: "closed"}),
				pr(base, prSpec{number: 8, author: botUser, head: syncBranch, state: "closed"}),
				pr(base, prSpec{number: 7, author: alice, head: syncBranch, state: "closed"}),
				pr(base, prSpec{number: 6, author: alice, head: syncBranch}),
				pr(base, prSpec{number: 5, author: botUser, head: syncBranch, base: "release", draft: true, labels: []string{"engineering-assets"}}),
				// A filter the server did not honor: never kept.
				pr(base, prSpec{number: 3, author: botUser, head: "feature/x"}),
			}
		case "acme:" + aliasBranch:
			items = []any{pr(base, prSpec{number: 4, author: botUser, head: aliasBranch, state: "closed", body: ptr("marker")})}
		}
		servePage(w, r, items, nil)
	})
	f.graphql("pullRequests(headRefName: $head, states: [OPEN], first: 50, after: $after)", func(w http.ResponseWriter, req gqlCall) {
		if req.Variables["owner"] != "acme" || req.Variables["name"] != "api" {
			t.Errorf("variables %v", req.Variables)
		}
		var nodes []any
		if req.Variables["head"] == syncBranch {
			nodes = []any{
				gqlPRNode(6, 101, 101, syncBranch, "main", gqlActorOf("User", "alice", 3001), "OPEN", true),
				gqlPRNode(11, 101, 999, syncBranch, "main", gqlActorOf("User", "bob", 3002), "OPEN", true),
			}
		}
		gqlData(w, map[string]any{"repository": map[string]any{"pullRequests": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nodes}}})
	})
	f.closersRoute(t, map[string]map[string]any{
		"PR_10": closedBy("PR_10", gqlActorOf("User", "alice", 3001), gqlActorOf("User", "alice", 3001)),
		"PR_9":  closedBy("PR_9", nil, gqlActorOf("User", "alice", 3001)),
		"PR_8":  closedBy("PR_8", nil, gqlActorOf("Bot", "acme-janitor[bot]", 6001)),
		"PR_4":  closedBy("PR_4", nil, gqlActorOf("Bot", "touchmark-write[bot]", 5001)),
	})
	f.json(http.MethodGet, "/repos/acme/api/branches/main", http.StatusOK, map[string]any{"name": "main"})
	f.json(http.MethodGet, "/repos/acme/api/branches/release", http.StatusNotFound, ghError("Branch not found"))

	r := platform.Repo{Host: "github.com", ID: "101", Path: "acme/api", DefaultBranch: "main"}
	got, err := f.reader.PRs(t.Context(), r, []string{syncBranch, aliasBranch, syncBranch, ""}, []platform.Account{writer})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{11, 10, 9, 8, 6, 5, 4}; !slices.Equal(numbers(got), want) {
		t.Fatalf("PRs = %v, want %v", numbers(got), want)
	}
	byNumber := map[int64]platform.PR{}
	for _, p := range got {
		byNumber[p.Number] = p
	}
	check := func(n int64, state platform.PRState, by string, kind platform.AccountKind) {
		t.Helper()
		p := byNumber[n]
		switch {
		case p.State != state:
			t.Errorf("#%d: state %s, want %s", n, p.State, state)
		case by == "" && p.ClosedBy != nil:
			t.Errorf("#%d: ClosedBy %+v", n, p.ClosedBy)
		case by != "" && (p.ClosedBy == nil || p.ClosedBy.ID != by || p.ClosedBy.Kind != kind):
			t.Errorf("#%d: ClosedBy %+v, want %s (%d)", n, p.ClosedBy, by, kind)
		}
	}
	check(10, platform.Merged, "3001", platform.KindUser)
	check(9, platform.Closed, "3001", platform.KindUser)
	check(8, platform.Closed, "6001", platform.KindBot)
	check(4, platform.Closed, "5001", platform.KindBot)
	check(6, platform.Open, "", 0)
	if p := byNumber[8]; p.ClosedBy.Login != "acme-janitor[bot]" {
		t.Errorf("a bot closer's login %q, want the REST form", p.ClosedBy.Login)
	}
	if p := byNumber[11]; p.HeadRepoID != "999" || p.RepoID != "101" || p.Author.ID != "3002" || !p.BaseExists {
		t.Errorf("fork PR %+v", p)
	}
	if p := byNumber[5]; p.BaseExists || p.Base != "release" || !p.Draft || !slices.Equal(p.Labels, []string{"engineering-assets"}) ||
		p.HeadRepoID != "101" || p.Author.Kind != platform.KindBot || !p.ClosedAt.IsZero() {
		t.Errorf("#5 %+v", p)
	}
	if p := byNumber[4]; p.Body != "marker" || p.Head != aliasBranch || p.ClosedAt.IsZero() {
		t.Errorf("#4 %+v", p)
	}
	if n := len(f.requests(http.MethodGet, "/repos/acme/api/branches/main")); n != 1 {
		t.Errorf("asked for the base main %d times", n)
	}
	// Closers are asked once per close in the run.
	f.reset()
	if _, err := f.reader.PRs(t.Context(), r, []string{syncBranch, aliasBranch}, []platform.Account{writer}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.gqlCalls() {
		if strings.Contains(c.Body, "CLOSED_EVENT") {
			t.Error("closers asked again")
		}
	}

	// Without authors: open ones by anyone.
	got, err = f.reader.PRs(t.Context(), r, []string{syncBranch}, nil)
	if err != nil || !slices.Equal(numbers(got), []int64{11, 6, 5}) {
		t.Errorf("without authors: %v, %v", numbers(got), err)
	}
	if got, err := f.reader.PRs(t.Context(), r, nil, []platform.Account{writer}); err != nil || len(got) != 0 {
		t.Errorf("without heads: %v, %v", got, err)
	}
}

// TestPRsAnonymous: without GraphQL, open pull requests from forks come
// from the whole open listing, and closers are unknown.
func TestPRsAnonymous(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credAnonymous, host: "github.com"})
	base := repo(101, "acme/api")
	fork := repo(999, "bob/api", withField("fork", true))
	f.handle(http.MethodGet, "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var items []any
		switch {
		case q.Get("head") == "acme:"+syncBranch && q.Get("state") == "all":
			items = []any{pr(base, prSpec{number: 2, author: botUser, head: syncBranch, state: "closed"})}
		case q.Get("head") == "" && q.Get("state") == "open":
			items = []any{
				pr(base, prSpec{number: 3, author: bob, head: syncBranch, headRepo: fork}),
				pr(base, prSpec{number: 4, author: bob, head: "other", headRepo: fork}),
				pr(base, prSpec{number: 5, author: bob, head: syncBranch, deletedFork: true}),
			}
		default:
			t.Errorf("query %v", q)
		}
		servePage(w, r, items, nil)
	})
	f.json(http.MethodGet, "/repos/acme/api/branches/main", http.StatusOK, map[string]any{"name": "main"})
	r := platform.Repo{Host: "github.com", ID: "101", Path: "acme/api"}
	got, err := f.reader.PRs(t.Context(), r, []string{syncBranch}, []platform.Account{{ID: "5001"}})
	if err != nil || !slices.Equal(numbers(got), []int64{5, 3, 2}) {
		t.Fatalf("PRs = %v, %v", numbers(got), err)
	}
	for _, p := range got {
		if p.ClosedBy != nil {
			t.Errorf("#%d: a closer without GraphQL", p.Number)
		}
		if p.Number == 5 && p.HeadRepoID != "" {
			t.Errorf("a deleted fork's head repository %q", p.HeadRepoID)
		}
	}
	if n := len(f.gqlCalls()); n != 0 {
		t.Errorf("%d GraphQL requests", n)
	}
}

// TestOpenPRsBy: every installation's repositories, batched GraphQL per
// head over 50 repositories, the authors' open pull requests from the
// heads; a repository with more is read to the end.
func TestOpenPRsBy(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	writer := platform.Account{ID: "5001"}
	var acmeRepos []any
	for i := range 60 {
		acmeRepos = append(acmeRepos, repo(int64(700+i), "acme/r"+itoa(int64(i))))
	}
	f.handle(http.MethodGet, "/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		m, ok := f.tokenOf(r.Header.Get("Authorization"))
		if !ok {
			t.Error("GET /installation/repositories without an installation token")
		}
		items := acmeRepos
		if m.installation == instAlice {
			items = []any{repo(800, "alice/dots")}
		}
		servePage(w, r, items, func(page []any) any {
			return map[string]any{"total_count": len(items), "repository_selection": "selected", "repositories": page}
		})
	})
	nodeRepo := func(id string) int64 { return int64(atoi(strings.TrimPrefix(id, "R_"))) }
	f.graphql("nodes(ids: $ids) {\n    ... on Repository", func(w http.ResponseWriter, req gqlCall) {
		ids := req.Variables["ids"].([]any)
		if len(ids) > 50 {
			t.Errorf("%d repositories in one request", len(ids))
		}
		head := req.Variables["head"].(string)
		var nodes []any
		for _, raw := range ids {
			id := raw.(string)
			rid := nodeRepo(id)
			var prs []any
			more := false
			switch {
			case rid == 700 && head == syncBranch:
				prs = []any{gqlPRNode(3, rid, rid, syncBranch, "main", gqlActorOf("Bot", "touchmark-write[bot]", 5001), "OPEN", true),
					gqlPRNode(2, rid, rid, syncBranch, "main", gqlActorOf("User", "alice", 3001), "OPEN", true)}
			case rid == 759 && head == aliasBranch:
				more = true
				prs = []any{gqlPRNode(9, rid, rid, aliasBranch, "main", gqlActorOf("User", "alice", 3001), "OPEN", true)}
			case rid == 800 && head == syncBranch:
				prs = []any{gqlPRNode(1, rid, 12345, syncBranch, "main", gqlActorOf("Bot", "touchmark-write[bot]", 5001), "OPEN", true)}
			}
			nodes = append(nodes, map[string]any{"id": id, "databaseId": rid, "pullRequests": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": more, "endCursor": nil}, "nodes": prs}})
		}
		gqlData(w, map[string]any{"nodes": nodes})
	})
	f.graphql("pullRequests(headRefName: $head, states: [OPEN], first: 50, after: $after)", func(w http.ResponseWriter, req gqlCall) {
		if req.Variables["name"] != "r59" || req.Variables["head"] != aliasBranch {
			t.Errorf("read to the end: %v", req.Variables)
		}
		gqlData(w, map[string]any{"repository": map[string]any{"pullRequests": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
			"nodes": []any{
				gqlPRNode(9, 759, 759, aliasBranch, "main", gqlActorOf("User", "alice", 3001), "OPEN", true),
				gqlPRNode(8, 759, 759, aliasBranch, "main", gqlActorOf("Bot", "touchmark-write[bot]", 5001), "OPEN", true),
			}}}})
	})
	sw, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch, aliasBranch})
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, rp := range sw.PRs {
		refs = append(refs, rp.Repo.Path+"#"+itoa(rp.PR.Number))
		if rp.PR.State != platform.Open || rp.PR.Author.ID != "5001" || rp.PR.RepoID != rp.Repo.ID {
			t.Errorf("%+v", rp)
		}
	}
	if want := []string{"acme/r0#3", "acme/r59#8", "alice/dots#1"}; !sw.Complete || !slices.Equal(refs, want) {
		t.Errorf("OpenPRsBy = %v (complete %v), want %v", refs, sw.Complete, want)
	}
	if n := len(f.gqlCalls()); n != 7 {
		t.Errorf("%d GraphQL requests, want 2 heads × (2 acme batches + 1 alice) and one read to the end", n)
	}

	// A rate limit fails the sweep.
	f.graphql("nodes(ids: $ids) {\n    ... on Repository", func(w http.ResponseWriter, _ gqlCall) {
		gqlData(w, nil, gqlErr("RATE_LIMITED", "API rate limit exceeded"))
	})
	_, err = f.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch})
	wantClass(t, "rate limit", err, platform.ClassRateLimited, nil)
	// Another failure of a batch makes it incomplete.
	f.graphql("nodes(ids: $ids) {\n    ... on Repository", func(w http.ResponseWriter, _ gqlCall) {
		gqlData(w, nil, gqlErr("MAX_NODE_LIMIT_EXCEEDED", "too many nodes"))
	})
	sw, err = f.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch})
	if err != nil || sw.Complete {
		t.Errorf("a failed batch: complete %v, %v", sw.Complete, err)
	}
}

// TestOpenPRsBySuspended: a suspended installation (suspended_at, or a
// token refused as suspended) is left out with a note, and the sweep stays
// complete: the writer could close nothing there. Another refusal to mint
// makes it incomplete. An anonymous reader sweeps nothing; a token sweeps
// what GET /user/repos lists.
func TestOpenPRsBySuspended(t *testing.T) {
	emptyRepos := func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, nil, func(page []any) any { return map[string]any{"total_count": 0, "repositories": page} })
	}
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.json(http.MethodPost, "/app/installations/7002/access_tokens", http.StatusForbidden, ghError("This installation has been suspended"))
	f.handle(http.MethodGet, "/installation/repositories", emptyRepos)
	sw, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{{ID: "5001"}}, []string{syncBranch})
	if err != nil || !sw.Complete || len(sw.PRs) != 0 || len(sw.Notes) != 1 ||
		!strings.Contains(sw.Notes[0], "installation 7002 of the GitHub App on alice is suspended") {
		t.Errorf("refused as suspended: %+v, %v", sw, err)
	}

	// Listed as suspended: no token is asked for.
	s := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	suspended := installation(instAlice, "alice", "User", nil)
	suspended["suspended_at"] = "2026-09-20T10:00:00Z"
	s.handle(http.MethodGet, "/app/installations", s.asApp(func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, []any{installation(instAcme, "acme", "Organization", nil), suspended}, nil)
	}))
	s.handle(http.MethodGet, "/installation/repositories", emptyRepos)
	sw, err = s.reader.OpenPRsBy(t.Context(), []platform.Account{{ID: "5001"}}, []string{syncBranch})
	if err != nil || !sw.Complete || len(sw.Notes) != 1 || s.mintedCount(instAlice) != 0 {
		t.Errorf("listed as suspended: %+v, %v; %d tokens of alice's installation", sw, err, s.mintedCount(instAlice))
	}

	// Another refusal: incomplete.
	o := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	o.json(http.MethodPost, "/app/installations/7002/access_tokens", http.StatusUnprocessableEntity,
		ghError("The permissions requested are not granted to this installation."))
	o.handle(http.MethodGet, "/installation/repositories", emptyRepos)
	sw, err = o.reader.OpenPRsBy(t.Context(), []platform.Account{{ID: "5001"}}, []string{syncBranch})
	if err != nil || sw.Complete || len(sw.Notes) != 0 {
		t.Errorf("refused otherwise: %+v, %v", sw, err)
	}

	an := newFixture(t, fixtureOpts{kind: credAnonymous, host: "github.com"})
	sw, err = an.reader.OpenPRsBy(t.Context(), []platform.Account{{ID: "5001"}}, []string{syncBranch})
	if err != nil || sw.Complete {
		t.Errorf("anonymous: %+v, %v", sw, err)
	}

	tf := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	tf.pages("/user/repos", []any{repo(900, "acme/x")})
	tf.graphql("nodes(ids: $ids) {\n    ... on Repository", func(w http.ResponseWriter, req gqlCall) {
		gqlData(w, map[string]any{"nodes": []any{map[string]any{"id": "R_900", "databaseId": 900, "pullRequests": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false},
			"nodes":    []any{gqlPRNode(4, 900, 900, syncBranch, "main", gqlActorOf("User", "alice", 3001), "OPEN", true)}}}}})
	})
	sw, err = tf.reader.OpenPRsBy(t.Context(), []platform.Account{{ID: "3001"}}, []string{syncBranch})
	if err != nil || !sw.Complete || len(sw.PRs) != 1 || sw.PRs[0].Repo.Path != "acme/x" {
		t.Errorf("token: %+v, %v", sw, err)
	}
}

func numbers(prs []platform.PR) []int64 {
	out := make([]int64, len(prs))
	for i, p := range prs {
		out[i] = p.Number
	}
	return out
}
