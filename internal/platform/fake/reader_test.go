package fake_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

func TestReadFileErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n", "docs/a.md", "a")
	rd := e.p.Reader(e.reader)
	ctx := t.Context()

	_, err := rd.ReadFile(ctx, r, "", "missing.md", 100)
	wantRule(t, "missing", err, platform.ClassNotFound, http.StatusNotFound, "")
	wantIs(t, "missing", err, platform.ErrNotFound)

	_, err = rd.ReadFile(ctx, r, "", "docs", 100)
	wantIs(t, "directory", err, platform.ErrNotRegular)
	_, err = rd.ReadFile(ctx, r, "", "README.md", 5)
	wantIs(t, "too large", err, platform.ErrTooLarge)
	if f, err := rd.ReadFile(ctx, r, "", "README.md", 6); err != nil || string(f.Content) != "hello\n" {
		t.Errorf("at the limit: %q, %v", f.Content, err)
	}

	// The refs that name the default branch head are served; others are not.
	for _, ref := range []string{"main", "refs/heads/main", e.p.Head(r.ID)} {
		if _, err := rd.ReadFile(ctx, r, ref, "README.md", 100); err != nil {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	_, err = rd.ReadFile(ctx, r, "touchmark/hub", "README.md", 100)
	wantClass(t, "another ref", err, platform.ClassUnsupported)

	for name, tc := range map[string]struct {
		repo platform.Repo
		path string
		max  int64
	}{
		"negative limit": {r, "README.md", -1},
		"empty path":     {r, "", 100},
		"absolute path":  {r, "/README.md", 100},
		"no id":          {platform.Repo{Path: r.Path, Host: r.Host}, "README.md", 100},
		"other host":     {platform.Repo{ID: r.ID, Path: r.Path, Host: "gitlab.com"}, "README.md", 100},
	} {
		_, err := rd.ReadFile(ctx, tc.repo, "", tc.path, tc.max)
		wantClass(t, name, err, platform.ClassInvalid)
	}
	// Host may be left out; an unknown id is not found.
	if _, err := rd.ReadFile(ctx, platform.Repo{ID: r.ID}, "", "README.md", 100); err != nil {
		t.Errorf("repository without host: %v", err)
	}
	_, err = rd.ReadFile(ctx, platform.Repo{ID: "404", Path: "acme/gone"}, "", "README.md", 100)
	wantIs(t, "unknown id", err, platform.ErrNotFound)

	// Content is a copy.
	f, _ := rd.ReadFile(ctx, r, "", "README.md", 100)
	f.Content[0] = 'X'
	if g, _ := rd.ReadFile(ctx, r, "", "README.md", 100); string(g.Content) != "hello\n" {
		t.Errorf("ReadFile content aliases the store: %q", g.Content)
	}
}

func TestResolveDetails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.p.AddRepo(platform.Repo{Path: "Acme/Zeta", Topics: []string{"Python"}})
	b := e.p.AddRepo(platform.Repo{Path: "acme/alpha", Topics: []string{"python"}, PendingDelete: true, Disabled: true, Mirror: true, PRsDisabled: true})
	e.p.AddRepo(platform.Repo{Path: "acmeother/x", Topics: []string{"python"}})
	e.ok()
	rd := e.p.Reader(e.reader)
	res, err := rd.Resolve(t.Context(), platform.Selector{Namespace: "ACME", Topics: []string{"PYTHON"}})
	if err != nil || !res.Complete {
		t.Fatalf("Resolve = %+v, %v", res, err)
	}
	if len(res.Repos) != 2 || res.Repos[0].ID != b.ID || res.Repos[1].ID != a.ID {
		t.Errorf("Resolve = %+v, want acme/alpha then Acme/Zeta (not acmeother/x)", res.Repos)
	}
	if !res.Repos[0].PendingDelete || !res.Repos[0].Disabled || !res.Repos[0].Mirror || !res.Repos[0].PRsDisabled || !res.Repos[0].Empty {
		t.Errorf("flags are not reported: %+v", res.Repos[0])
	}
	_, err = rd.Resolve(t.Context(), platform.Selector{})
	wantClass(t, "empty selector", err, platform.ClassInvalid)
	if res, err := rd.Resolve(t.Context(), platform.Selector{Namespace: "nobody"}); err != nil || len(res.Repos) != 0 || !res.Complete {
		t.Errorf("a namespace without repositories = %+v, %v", res, err)
	}

	e.p.SetIncompleteListings(true)
	res, _ = rd.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
	sw, _ := rd.OpenPRsBy(t.Context(), []platform.Account{e.writer}, []string{"x"})
	one, _ := rd.Resolve(t.Context(), platform.Selector{Repo: "acme/alpha"})
	if res.Complete || len(res.Repos) != 2 || sw.Complete || !one.Complete {
		t.Errorf("incomplete listings: resolve %v (%d), sweep %v, repo %v", res.Complete, len(res.Repos), sw.Complete, one.Complete)
	}
	e.p.SetIncompleteListings(false)
	if res, _ := rd.Resolve(t.Context(), platform.Selector{Namespace: "acme"}); !res.Complete {
		t.Error("listings stay incomplete")
	}
}

// TestCloserByFlavor: Gitea and Forgejo do not report who closed a PR
// unmerged; the merger is reported everywhere.
func TestCloserByFlavor(t *testing.T) {
	t.Parallel()
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea, fake.Forgejo} {
		e := newEnv(t, fake.WithFlavor(f))
		r := e.repo("acme/api", "README.md", "x")
		closed := e.pr(r, platform.PR{Head: "touchmark/hub", Author: e.writer})
		e.p.SetPRState(r.ID, closed, platform.Closed, &e.other, time.Time{})
		merged := e.pr(r, platform.PR{Head: "touchmark/hub", Author: e.writer})
		e.p.SetPRState(r.ID, merged, platform.Merged, &e.other, time.Time{})
		prs, err := e.p.Reader(e.reader).PRs(t.Context(), r, []string{"touchmark/hub"}, []platform.Account{e.writer})
		if err != nil || len(prs) != 2 {
			t.Fatalf("%s: PRs = %v, %v", f, numbers(prs), err)
		}
		known := fake.CapsFor(f).CloserKnown
		if got := prs[1].ClosedBy != nil; got != known {
			t.Errorf("%s: closer of a closed PR reported %v, want %v", f, got, known)
		}
		if prs[0].ClosedBy == nil || prs[0].ClosedBy.ID != e.other.ID {
			t.Errorf("%s: merger %v, want %s", f, prs[0].ClosedBy, e.other.ID)
		}
		if e.p.PR(r.ID, closed).ClosedBy == nil {
			t.Errorf("%s: PR() hides the closer", f)
		}
	}
}

func TestOpenPRsByOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.repo("acme/b", "README.md", "x")
	a := e.repo("acme/a", "README.md", "x")
	for range 2 {
		e.pr(b, platform.PR{Head: "touchmark/hub", Author: e.writer})
		e.pr(a, platform.PR{Head: "touchmark/hub", Author: e.writer})
	}
	e.pr(a, platform.PR{Head: "touchmark/hub", Author: e.writer, HeadRepoID: "fork-id"})
	sw, err := e.p.Reader(e.reader).OpenPRsBy(t.Context(), []platform.Account{e.writer}, []string{"touchmark/hub"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rp := range sw.PRs {
		got = append(got, rp.Repo.Path+"#"+string(rune('0'+rp.PR.Number)))
	}
	sameList(t, "OpenPRsBy", got, []string{"acme/a#3", "acme/a#2", "acme/a#1", "acme/b#2", "acme/b#1"})
	if sw, _ := e.p.Reader(e.reader).OpenPRsBy(t.Context(), nil, []string{"touchmark/hub"}); len(sw.PRs) != 0 {
		t.Errorf("OpenPRsBy without authors = %d PRs", len(sw.PRs))
	}
}

// TestUnknownAccount: a reader for an account the platform does not know
// is a bad credential on every call.
func TestUnknownAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	ctx := t.Context()
	for name, rd := range map[string]platform.Reader{
		"zero":  e.p.Reader(platform.Account{}),
		"other": e.p.Reader(platform.Account{ID: "31337", Login: "ghost"}),
	} {
		_, err1 := rd.Self(ctx)
		_, err2 := rd.Repo(ctx, r.Path)
		_, err3 := rd.PRs(ctx, r, []string{"h"}, nil)
		for i, err := range []error{err1, err2, err3} {
			wantRule(t, name+" call "+string(rune('1'+i)), err, platform.ClassAuth, http.StatusUnauthorized, "")
		}
	}
	_, err := e.p.Writer(platform.Account{ID: "31337"}).Target(ctx, r, platform.Perms{PRs: true})
	wantClass(t, "Target as a ghost", err, platform.ClassAuth)
}

func TestContextErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	e.p.ResetCalls()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.p.Reader(e.reader).Repo(cancelled, r.Path)
	wantIs(t, "cancelled", err, context.Canceled)
	wantClass(t, "cancelled", err, platform.ClassUnknown)
	_, err = tw.CreatePR(cancelled, platform.NewPR{Head: "h", Base: "main", Title: "t"})
	wantIs(t, "cancelled write", err, context.Canceled)
	_, err = e.p.Snapshots().Snapshot(cancelled, r, platform.Remote{}, "")
	wantIs(t, "cancelled snapshot", err, context.Canceled)

	expired, cancel2 := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel2()
	_, err = e.p.Reader(e.reader).Probe(expired)
	wantClass(t, "deadline", err, platform.ClassTransient)

	if calls := e.p.Calls(); len(calls) != 0 {
		t.Errorf("calls that never started were logged: %v", calls)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: %v", err)
	}
}
