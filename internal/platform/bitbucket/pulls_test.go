package bitbucket

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The accounts of the pull request tests: the writer (a bot account with an
// API token, type user) and a person.
var (
	botAccount    = platform.Account{ID: botUUID, Login: botUUID, Kind: platform.KindUser}
	personAccount = platform.Account{ID: personUUID, Login: personUUID, Kind: platform.KindUser}
)

// fullHash is the full id of the source commit of pull request id in pr's
// fixtures, whose short form pr sends.
func fullHash(short string) string { return short + strings.Repeat("0", 40-len(short)) }

// commitRoute declares GET …/commit/{short} of acme/api with its full id.
func (s *apiServer) commitRoute(short string) {
	s.json("/repositories/acme/api/commit/"+short, http.StatusOK, map[string]any{
		"hash": fullHash(short), "type": "commit", "date": "2026-09-30T10:00:00+00:00", "message": "chore: sync",
	})
}

func TestPRs(t *testing.T) {
	f := newFixture(t)
	bot, person := account(botUUID, "touchmark bot"), account(personUUID, "Wilson Mendes Neto")
	fork := repoRef(forkUUID, "someone/api")
	items := []any{
		pr(prSpec{id: 9, author: bot, source: syncBranch, body: "sync\n\n[touchmark]: # \"touchmark:v1 …\"", draft: true}),
		pr(prSpec{id: 8, author: person, source: syncBranch, sourceRepo: fork, body: "from a fork"}),
		pr(prSpec{id: 7, state: "DECLINED", author: bot, closedBy: person, source: syncBranch, body: "declined"}),
		pr(prSpec{id: 6, state: "MERGED", author: bot, closedBy: person, source: syncBranch, target: "release"}),
		pr(prSpec{id: 5, state: "SUPERSEDED", author: bot, closedBy: bot, source: syncBranch, target: "gone"}),
		pr(prSpec{id: 4, state: "DECLINED", author: person, closedBy: person, source: syncBranch}),
		pr(prSpec{id: 3, author: bot, source: "feature/other"}),
	}
	f.pages("/repositories/acme/api/pullrequests", 3, items)
	f.commitRoute("a6dcc7402009")
	f.json("/repositories/acme/api/refs/branches/release", http.StatusOK, map[string]any{"name": "release", "type": "branch",
		"target": map[string]any{"hash": headCommit, "type": "commit"}})
	f.json("/repositories/acme/api/refs/branches/gone", http.StatusNotFound, errorBody("Branch \"gone\" not found"))

	got, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch, ""}, []platform.Account{botAccount})
	if err != nil {
		t.Fatal(err)
	}
	var numbers []int64
	byNumber := map[int64]platform.PR{}
	for _, p := range got {
		numbers = append(numbers, p.Number)
		byNumber[p.Number] = p
	}
	if len(numbers) != 5 || numbers[0] != 9 || numbers[1] != 8 || numbers[2] != 7 || numbers[3] != 6 || numbers[4] != 5 {
		t.Fatalf("PRs = %v; want 9, 8, 7, 6, 5: the bot's in every state and anyone's open ones, newest first", numbers)
	}
	open := byNumber[9]
	if open.State != platform.Open || !open.Draft || open.Head != syncBranch || open.Base != "main" || !open.BaseExists ||
		open.RepoID != repoUUID || open.HeadRepoID != repoUUID || open.HeadSHA != fullHash("a6dcc7402009") ||
		open.Author != botAccount || open.ClosedBy != nil || !open.ClosedAt.IsZero() ||
		open.Body != "sync\n\n[touchmark]: # \"touchmark:v1 …\"" || open.URL != "https://bitbucket.org/acme/api/pull-requests/9" ||
		len(open.Labels) != 0 || open.Labels == nil || !open.CreatedAt.Equal(time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("open = %+v", open)
	}
	if fromFork := byNumber[8]; fromFork.HeadRepoID != forkUUID || fromFork.HeadSHA != "" || fromFork.State != platform.Open {
		t.Errorf("from a fork = %+v; want its own head repository and no head commit read", fromFork)
	}
	declined := byNumber[7]
	if declined.State != platform.Closed || declined.ClosedBy == nil || *declined.ClosedBy != personAccount ||
		!declined.ClosedAt.Equal(time.Date(2026, 9, 10, 12, 0, 0, 123456000, time.UTC)) || declined.HeadSHA != "" {
		t.Errorf("declined = %+v; want closed, by the person, at updated_on", declined)
	}
	if merged := byNumber[6]; merged.State != platform.Merged || !merged.BaseExists || merged.ClosedBy == nil {
		t.Errorf("merged = %+v", merged)
	}
	if superseded := byNumber[5]; superseded.State != platform.Closed || superseded.BaseExists || superseded.ClosedBy == nil ||
		*superseded.ClosedBy != botAccount {
		t.Errorf("superseded = %+v; want closed, its deleted base reported", superseded)
	}

	calls := f.requests(http.MethodGet, "/repositories/acme/api/pullrequests")
	if len(calls) != 3 {
		t.Fatalf("%d pages read; want 3", len(calls))
	}
	q := calls[0].Query
	wantQ := `source.branch.name IN ("touchmark/acme-eng") AND state IN ("OPEN", "MERGED", "DECLINED", "SUPERSEDED")`
	if q.Get("q") != wantQ || q.Get("sort") != "-id" || q.Get("pagelen") != "50" || q.Has("state") {
		t.Errorf("query %v; want q %s, sorted by descending id, the state in q", q, wantQ)
	}
	if n := len(f.requests(http.MethodGet, "/repositories/acme/api/refs/branches/main")); n != 0 {
		t.Errorf("the default branch was looked up %d times; it exists", n)
	}

	// A second listing reads the full id of the head from memory.
	f.reset()
	if _, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, []platform.Account{botAccount}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests(http.MethodGet, "/repositories/acme/api/commit/a6dcc7402009")); n != 0 {
		t.Errorf("the short id was expanded again (%d requests)", n)
	}
}

func TestPRsNoHeads(t *testing.T) {
	f := newFixture(t)
	got, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{""}, []platform.Account{botAccount})
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("PRs without heads = %v, %v; want an empty list", got, err)
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("%d requests without heads", len(calls))
	}
}

func TestPRsQuoting(t *testing.T) {
	f := newFixture(t)
	f.pages("/repositories/acme/api/pullrequests", 50, []any{
		pr(prSpec{id: 2, author: account(botUUID, "bot"), source: `odd"name`, hash: strings.Repeat("c", 40)}),
		pr(prSpec{id: 1, author: account(botUUID, "bot"), source: "other"}),
	})
	got, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{`odd"name`}, []platform.Account{botAccount})
	if err != nil || len(got) != 1 || got[0].Number != 2 || got[0].HeadSHA != strings.Repeat("c", 40) {
		t.Errorf("PRs = %+v, %v; want #2, its full head taken as it is", got, err)
	}
	q := f.requests(http.MethodGet, "/repositories/acme/api/pullrequests")[0].Query.Get("q")
	if q != `state IN ("OPEN", "MERGED", "DECLINED", "SUPERSEDED")` {
		t.Errorf("q %s; want no branch filter for a name BBQL cannot hold safely", q)
	}
}

func TestPRsBody(t *testing.T) {
	f := newFixture(t)
	f.pages("/repositories/acme/api/pullrequests", 50, []any{
		pr(prSpec{id: 4, state: "MERGED", author: account(botUUID, "bot"), source: syncBranch, noSummary: true}),
	})
	f.json("/repositories/acme/api/pullrequests/4", http.StatusOK,
		pr(prSpec{id: 4, state: "MERGED", author: account(botUUID, "bot"), source: syncBranch, body: "the marker"}))
	got, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, []platform.Account{botAccount})
	if err != nil || len(got) != 1 || got[0].Body != "the marker" {
		t.Errorf("PRs = %+v, %v; want the description of the pull request read alone", got, err)
	}
}

func TestPRsErrors(t *testing.T) {
	t.Run("a missing repository", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/pullrequests", http.StatusNotFound, errorBody(noRepoMessage))
		_, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, nil)
		wantClass(t, "PRs", err, platform.ClassNotFound, nil)
	})
	t.Run("a later page fails", func(t *testing.T) {
		f := newFixture(t)
		items := []any{
			pr(prSpec{id: 2, author: account(botUUID, "bot"), source: syncBranch}),
			pr(prSpec{id: 1, author: account(botUUID, "bot"), source: syncBranch}),
		}
		f.handle(http.MethodGet, "/repositories/acme/api/pullrequests", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				writeJSON(w, http.StatusBadGateway, errorBody("Bad gateway"))
				return
			}
			servePage(w, r, 1, items)
		})
		_, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, nil)
		wantClass(t, "PRs", err, platform.ClassTransient, nil)
	})
	t.Run("an unknown state", func(t *testing.T) {
		f := newFixture(t)
		p := pr(prSpec{id: 1, author: account(botUUID, "bot"), source: syncBranch})
		p["state"] = "QUEUED"
		f.pages("/repositories/acme/api/pullrequests", 50, []any{p})
		_, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, nil)
		wantClass(t, "PRs", err, platform.ClassUnknown, nil)
	})
	t.Run("a head commit the API does not find", func(t *testing.T) {
		f := newFixture(t)
		f.pages("/repositories/acme/api/pullrequests", 50, []any{pr(prSpec{id: 1, author: account(botUUID, "bot"), source: syncBranch})})
		f.json("/repositories/acme/api/commit/a6dcc7402001", http.StatusNotFound, errorBody("Commit not found"))
		got, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, nil)
		if err != nil || len(got) != 1 || got[0].HeadSHA != "" {
			t.Errorf("PRs = %+v, %v; want the pull request with its head unknown", got, err)
		}
	})
	t.Run("the commit API limits the rate", func(t *testing.T) {
		f := newFixture(t)
		f.pages("/repositories/acme/api/pullrequests", 50, []any{pr(prSpec{id: 1, author: account(botUUID, "bot"), source: syncBranch})})
		f.handle(http.MethodGet, "/repositories/acme/api/commit/a6dcc7402001", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusTooManyRequests, errorBody("Rate limit for this resource has been exceeded"))
		})
		_, err := f.reader.PRs(t.Context(), apiRepoFixture, []string{syncBranch}, nil)
		wantClass(t, "PRs", err, platform.ClassRateLimited, nil)
	})
}

// sweepFixture declares two workspaces, acme and labs, where the bot has
// open pull requests.
func sweepFixture(t *testing.T) *fixture {
	f := newFixture(t)
	bot := account(botUUID, "touchmark bot")
	f.pages("/user/workspaces", 1, []any{
		map[string]any{"administrator": false, "type": "workspace_access",
			"workspace": map[string]any{"type": "workspace_base", "slug": "labs", "uuid": "{470c176d-3574-44ea-bb41-89e8638bcca4}"}},
		map[string]any{"administrator": false, "type": "workspace_access",
			"workspace": map[string]any{"type": "workspace_base", "slug": "acme", "uuid": "{02b941e3-cfaa-40f9-9a58-cec53e20bdc3}"}},
	})
	labsUUID := "{9c8b7a6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d}"
	labsPR := pr(prSpec{id: 3, author: bot, source: syncBranch, body: "labs", target: "develop"})
	labsPR["destination"].(map[string]any)["repository"] = repoRef(labsUUID, "labs/tool")
	labsPR["source"].(map[string]any)["repository"] = repoRef(labsUUID, "labs/tool")
	f.pages("/workspaces/acme/pullrequests/"+uuidPath(botUUID), 50, []any{
		pr(prSpec{id: 12, author: bot, source: syncBranch, body: "twelve"}),
		pr(prSpec{id: 11, author: bot, source: "feature/x"}),
		pr(prSpec{id: 10, author: bot, source: syncBranch, noSummary: true}),
	})
	f.pages("/workspaces/labs/pullrequests/"+uuidPath(botUUID), 50, []any{labsPR})
	f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusOK, repo(repoUUID, "acme/api"))
	f.json("/repositories/labs/"+uuidPath(labsUUID), http.StatusOK, repo(labsUUID, "labs/tool", with("is_private", false)))
	f.json("/repositories/acme/api/pullrequests/10", http.StatusOK, pr(prSpec{id: 10, author: bot, source: syncBranch, body: "ten"}))
	f.json("/repositories/labs/tool/refs/branches/develop", http.StatusNotFound, errorBody("Branch \"develop\" not found"))
	return f
}

func TestOpenPRsBy(t *testing.T) {
	f := sweepFixture(t)
	got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || len(got.PRs) != 3 {
		t.Fatalf("OpenPRsBy = %+v; want three pull requests, complete", got)
	}
	want := []struct {
		path   string
		number int64
		body   string
		base   bool
	}{{"acme/api", 12, "twelve", true}, {"acme/api", 10, "ten", true}, {"labs/tool", 3, "labs", false}}
	for i, w := range want {
		rp := got.PRs[i]
		if rp.Repo.Path != w.path || rp.PR.Number != w.number || rp.PR.Body != w.body || rp.PR.BaseExists != w.base ||
			rp.PR.State != platform.Open || rp.PR.RepoID != rp.Repo.ID || rp.PR.HeadRepoID != rp.Repo.ID || rp.Repo.DefaultBranch != "main" {
			t.Errorf("PRs[%d] = %s #%d %+v; want %+v", i, rp.Repo.Path, rp.PR.Number, rp.PR, w)
		}
	}
	if got.PRs[2].Repo.Visibility != "public" || got.PRs[0].Repo.Visibility != "private" {
		t.Errorf("visibility %s, %s", got.PRs[0].Repo.Visibility, got.PRs[2].Repo.Visibility)
	}
	if n := len(f.requests(http.MethodGet, "/repositories/acme/"+uuidPath(repoUUID))); n != 1 {
		t.Errorf("acme/api read %d times; want once", n)
	}
	calls := f.requests(http.MethodGet, "/workspaces/acme/pullrequests/"+uuidPath(botUUID))
	if len(calls) != 1 || calls[0].Query.Get("q") != `source.branch.name IN ("touchmark/acme-eng") AND state IN ("OPEN")` {
		t.Errorf("calls %+v; want the open pull requests from the sync branch", calls)
	}
}

func TestOpenPRsByIncomplete(t *testing.T) {
	t.Run("an author a workspace does not know", func(t *testing.T) {
		f := sweepFixture(t)
		f.json("/workspaces/labs/pullrequests/"+uuidPath(botUUID), http.StatusNotFound, errorBody("No such user"))
		got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
		if err != nil || got.Complete || len(got.PRs) != 2 {
			t.Errorf("OpenPRsBy = %+v, %v; want acme's pull requests, incomplete", got, err)
		}
	})
	t.Run("a repository that cannot be read", func(t *testing.T) {
		f := sweepFixture(t)
		f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusForbidden, errorBody("Forbidden"))
		got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
		if err != nil || got.Complete || len(got.PRs) != 1 {
			t.Errorf("OpenPRsBy = %+v, %v; want labs' pull request, incomplete", got, err)
		}
	})
	t.Run("a repository gone since the listing", func(t *testing.T) {
		f := sweepFixture(t)
		f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusNotFound, errorBody(noRepoMessage))
		got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
		if err != nil || !got.Complete || len(got.PRs) != 1 {
			t.Errorf("OpenPRsBy = %+v, %v; want labs' pull request, complete", got, err)
		}
	})
	t.Run("an author id that is no uuid", func(t *testing.T) {
		f := sweepFixture(t)
		got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount, {ID: "5b57c56fdfe79e2c947cd85f"}}, []string{syncBranch})
		if err != nil || got.Complete || len(got.PRs) != 3 {
			t.Errorf("OpenPRsBy = %+v, %v; want the bot's, incomplete", got, err)
		}
	})
}

func TestOpenPRsByErrors(t *testing.T) {
	t.Run("rate limited", func(t *testing.T) {
		f := sweepFixture(t)
		f.handle(http.MethodGet, "/workspaces/labs/pullrequests/"+uuidPath(botUUID), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "120")
			writeJSON(w, http.StatusTooManyRequests, errorBody("Rate limit for this resource has been exceeded"))
		})
		_, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
		wantClass(t, "OpenPRsBy", err, platform.ClassRateLimited, nil)
		if d := retryAfterOf(err); d != 120*time.Second {
			t.Errorf("RetryAfter %v; want x-ratelimit-reset's 120 s", d)
		}
	})
	t.Run("no workspaces", func(t *testing.T) {
		f := newFixture(t)
		f.json("/user/workspaces", http.StatusForbidden, errorBody("Your credentials lack one or more required privilege scopes."))
		got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
		if err != nil || got.Complete || len(got.PRs) != 0 {
			t.Errorf("OpenPRsBy = %+v, %v; want nothing, incomplete", got, err)
		}
	})
	t.Run("anonymous", func(t *testing.T) {
		s := newAPIServer(t)
		r, err := NewReader(s.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.OpenPRsBy(t.Context(), []platform.Account{botAccount}, []string{syncBranch})
		wantClass(t, "anonymous", err, platform.ClassAuth, nil)
	})
	t.Run("nothing to look for", func(t *testing.T) {
		f := newFixture(t)
		got, err := f.reader.OpenPRsBy(t.Context(), nil, []string{syncBranch})
		if err != nil || !got.Complete || got.PRs == nil {
			t.Errorf("OpenPRsBy without authors = %+v, %v", got, err)
		}
		if calls := f.requests("", ""); len(calls) != 0 {
			t.Errorf("%d requests", len(calls))
		}
	})
}

// retryAfterOf returns the RetryAfter of err's *platform.Error.
func retryAfterOf(err error) time.Duration {
	var pe *platform.Error
	if errors.As(err, &pe) {
		return pe.RetryAfter
	}
	return 0
}
