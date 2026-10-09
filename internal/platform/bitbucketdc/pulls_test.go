package bitbucketdc

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Accounts of the pull request fixtures.
var (
	writerUser  = user(writerID, "touchmark.writer", "touchmark.writer", "NORMAL")
	personUser  = user(personID, "Jane.Doe", "jane.doe", "NORMAL")
	serviceUser = user(serviceID, "project_1_bot", "project_1_bot", "SERVICE")
	writerAcct  = platform.Account{ID: strconv.Itoa(writerID), Login: "touchmark.writer", Kind: platform.KindUser}
)

// activity is an activity of a pull request.
func activity(id int64, action string, at int64, u map[string]any) map[string]any {
	m := map[string]any{"id": id, "createdDate": at, "action": action}
	if u != nil {
		m["user"] = u
	}
	return m
}

// prRoute is the route of pull request n of ACME/api, and more.
func prRoute(n int64, more string) string {
	return repoRoute("ACME", "api") + "/pull-requests/" + strconv.FormatInt(n, 10) + more
}

func TestPRs(t *testing.T) {
	f := newFixture(t)
	fork := repoRef(forkID, "ACME", "fork")
	f.pages(repoRoute("ACME", "api")+"/pull-requests", 2, []any{
		pr(prSpec{id: 9, source: syncBranch, author: writerUser, body: "body\n\n[touchmark]: # \"touchmark:v1 x\"", version: 3}),
		pr(prSpec{id: 8, source: syncBranch, author: personUser, state: "DECLINED"}), // not ours, closed
		pr(prSpec{id: 7, source: syncBranch, author: personUser, draft: true}),       // anyone's, open
		pr(prSpec{id: 6, source: syncBranch, author: writerUser, state: "DECLINED", target: "release"}),
		pr(prSpec{id: 5, source: syncBranch, author: writerUser, state: "MERGED"}),
		pr(prSpec{id: 4, source: "feature", author: writerUser}),                                            // another branch
		pr(prSpec{id: 3, source: syncBranch, author: writerUser, targetRepo: repoRef(201, "OTHER", "api")}), // into another repository
		pr(prSpec{id: 2, source: syncBranch, author: writerUser, state: "DECLINED", target: "gone"}),
	})
	f.pages(prRoute(6, "/activities"), 500, []any{
		activity(60, "COMMENTED", 1788500000000, personUser),
		activity(61, "DECLINED", 1788600000000, personUser),
		activity(62, "REOPENED", 1788400000000, writerUser),
		activity(63, "DECLINED", 1788300000000, writerUser),
		activity(64, "OPENED", 1788200000000, writerUser),
	})
	f.pages(prRoute(2, "/activities"), 500, []any{activity(20, "DECLINED", 1788600000000, nil)})
	f.pages(repoRoute("ACME", "api")+"/branches", 1000, []any{branch("release", mainSHA, false), branch("release-2", mainSHA, false)})
	_ = fork

	got, err := f.reader.PRs(t.Context(), f.apiRepoOf(), []string{syncBranch}, []platform.Account{writerAcct})
	if err != nil {
		t.Fatal(err)
	}
	var nums []int64
	for _, p := range got {
		nums = append(nums, p.Number)
	}
	if fmt.Sprint(nums) != "[9 7 6 5 2]" {
		t.Fatalf("PRs = %v, want [9 7 6 5 2]", nums)
	}
	open := got[0]
	switch {
	case open.State != platform.Open || open.HeadSHA != headSHA || open.Head != syncBranch || open.Base != "main" || !open.BaseExists:
		t.Errorf("open PR = %+v", open)
	case open.RepoID != "101" || open.HeadRepoID != "101" || open.Author.ID != "12" || open.Author.Kind != platform.KindUser:
		t.Errorf("open PR ids = %+v", open)
	case open.URL != f.base()+"/projects/ACME/repos/api/pull-requests/9" || open.Body != "body\n\n[touchmark]: # \"touchmark:v1 x\"":
		t.Errorf("open PR url %q, body %q", open.URL, open.Body)
	case !open.CreatedAt.Equal(time.UnixMilli(1788163200009)) || !open.ClosedAt.IsZero() || open.ClosedBy != nil:
		t.Errorf("open PR times %v %v", open.CreatedAt, open.ClosedAt)
	case open.Labels == nil:
		t.Error("labels are nil")
	}
	if !got[1].Draft || got[1].Body != "" {
		t.Errorf("a draft without a description = %+v", got[1])
	}
	declined := got[2]
	switch {
	case declined.State != platform.Closed || declined.HeadSHA != "" || declined.Base != "release" || !declined.BaseExists:
		t.Errorf("declined PR = %+v", declined)
	case declined.ClosedBy == nil || declined.ClosedBy.ID != "13" || declined.ClosedBy.Kind != platform.KindUser:
		t.Errorf("the latest decline is Jane's, got %+v", declined.ClosedBy)
	case !declined.ClosedAt.Equal(time.UnixMilli(1788422400006)):
		t.Errorf("closed at %v", declined.ClosedAt)
	}
	if got[3].State != platform.Merged || got[3].ClosedBy != nil {
		t.Errorf("merged PR = %+v", got[3])
	}
	system := got[4]
	if system.BaseExists || system.ClosedBy == nil || system.ClosedBy.ID != systemID || system.ClosedBy.Kind != platform.KindBot {
		t.Errorf("a decline without a user = %+v, closed by %+v", system, system.ClosedBy)
	}
	q := f.requests(http.MethodGet, repoRoute("ACME", "api")+"/pull-requests")[0].Query
	if q.Get("state") != "ALL" || q.Get("direction") != "OUTGOING" || q.Get("at") != "refs/heads/"+syncBranch || q.Get("order") != "NEWEST" {
		t.Errorf("the listing asked %v", q)
	}
	if n := len(f.requests(http.MethodGet, prRoute(8, "/activities"))); n != 0 {
		t.Error("the closer of someone else's pull request was read")
	}
	if n := len(f.requests(http.MethodGet, repoRoute("ACME", "api")+"/branches")); n != 2 {
		t.Errorf("%d branch lookups, want one per base (release, gone)", n)
	}
}

func TestPRsCloser(t *testing.T) {
	f := newFixture(t)
	f.pages(repoRoute("ACME", "api")+"/pull-requests", 1000, []any{
		pr(prSpec{id: 3, source: syncBranch, author: writerUser, state: "DECLINED"}),
	})
	for _, tc := range []struct {
		name  string
		items []any
		want  *platform.Account
	}{
		{"a service user", []any{activity(1, "DECLINED", 5, serviceUser)}, &platform.Account{ID: "14", Login: "project_1_bot", Kind: platform.KindBot}},
		{"no decline", []any{activity(1, "OPENED", 5, writerUser)}, nil},
		{"many pages", func() []any {
			var items []any
			for i := range 2001 {
				items = append(items, activity(int64(i), "COMMENTED", int64(i), personUser))
			}
			return append(items, activity(5000, "DECLINED", 9999, personUser))
		}(), nil},
	} {
		f.pages(prRoute(3, "/activities"), 500, tc.items)
		got, err := f.reader.PRs(t.Context(), f.apiRepoOf(), []string{syncBranch}, []platform.Account{writerAcct})
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v, %v", tc.name, got, err)
		}
		if fmt.Sprint(got[0].ClosedBy) != fmt.Sprint(tc.want) {
			t.Errorf("%s: closed by %+v, want %+v", tc.name, got[0].ClosedBy, tc.want)
		}
	}
	f.json(prRoute(3, "/activities"), http.StatusServiceUnavailable, errorBody("", "busy"))
	_, err := f.reader.PRs(t.Context(), f.apiRepoOf(), []string{syncBranch}, []platform.Account{writerAcct})
	wantClass(t, "activities that fail", err, platform.ClassTransient, nil)
}

func TestPRsFailures(t *testing.T) {
	f := newFixture(t)
	var many []any
	for i := range maxPRPages*1000 + 1 {
		many = append(many, pr(prSpec{id: int64(i + 1), source: syncBranch, author: personUser, state: "MERGED"}))
	}
	f.pages(repoRoute("ACME", "api")+"/pull-requests", 1000, many)
	_, err := f.reader.PRs(t.Context(), f.apiRepoOf(), []string{syncBranch}, nil)
	wantClass(t, "a listing past its bound", err, platform.ClassUnknown, nil)

	f.pages(repoRoute("ACME", "api")+"/pull-requests", 1000, []any{map[string]any{"id": 1, "state": "OPEN"}})
	_, err = f.reader.PRs(t.Context(), f.apiRepoOf(), []string{syncBranch}, nil)
	wantClass(t, "a pull request without refs", err, platform.ClassUnknown, nil)

	f.reset()
	if got, err := f.reader.PRs(t.Context(), f.apiRepoOf(), nil, nil); err != nil || len(got) != 0 {
		t.Errorf("no heads = %v, %v", got, err)
	}
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("no heads sent %d requests", n)
	}
}

func TestOpenPRsBy(t *testing.T) {
	f := newFixture(t)
	f.json("/users/touchmark.writer", http.StatusOK, writerUser)
	writer, err := f.reader.Lookup(t.Context(), "touchmark.writer")
	if err != nil {
		t.Fatal(err)
	}
	f.pages("/dashboard/pull-requests", 1, []any{
		pr(prSpec{id: 9, source: syncBranch, author: writerUser}),
		pr(prSpec{id: 4, source: "feature", author: writerUser}),
		pr(prSpec{id: 3, source: syncBranch, author: writerUser, targetRepo: repoRef(201, "OTHER", "tool")}),
		pr(prSpec{id: 2, source: syncBranch, author: writerUser, targetRepo: repoRef(301, "GONE", "x")}),
		pr(prSpec{id: 1, source: syncBranch, author: personUser}),
	})
	f.json(repoRoute("ACME", "api"), http.StatusOK, repo(repoID, "ACME", "api"))
	f.defBranch("ACME", "api", "main", mainSHA)
	f.json(repoRoute("OTHER", "tool"), http.StatusOK, repo(201, "OTHER", "tool"))
	f.defBranch("OTHER", "tool", "main", mainSHA)
	f.json(repoRoute("GONE", "x"), http.StatusNotFound, errorBody(noRepoException, "gone"))

	got, err := f.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || len(got.PRs) != 2 {
		t.Fatalf("OpenPRsBy = %+v", got)
	}
	if got.PRs[0].Repo.Path != "ACME/api" || got.PRs[0].PR.Number != 9 || got.PRs[1].Repo.Path != "OTHER/tool" || !got.PRs[0].PR.BaseExists {
		t.Errorf("swept %+v", got.PRs)
	}
	q := f.requests(http.MethodGet, "/dashboard/pull-requests")[0].Query
	if q.Get("user") != "touchmark.writer" || q.Get("role") != "AUTHOR" || q.Get("state") != "OPEN" {
		t.Errorf("the dashboard query %v", q)
	}

	// An author whose name the reader never learnt.
	got, err = f.reader.OpenPRsBy(t.Context(), []platform.Account{writer, {ID: "77", Login: "ghost"}}, []string{syncBranch})
	if err != nil || got.Complete {
		t.Errorf("an unknown author = %+v, %v", got, err)
	}

	// A dashboard that refuses another user's name.
	f.json("/dashboard/pull-requests", http.StatusUnauthorized, errorBody(authorisation, "You are not permitted to access this resource"))
	got, err = f.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch})
	if err != nil || got.Complete {
		t.Errorf("a refused dashboard = %+v, %v", got, err)
	}
	f.json("/dashboard/pull-requests", http.StatusTooManyRequests, nil)
	_, err = f.reader.OpenPRsBy(t.Context(), []platform.Account{writer}, []string{syncBranch})
	wantClass(t, "a rate limit", err, platform.ClassRateLimited, nil)
}
