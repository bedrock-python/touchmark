package fake_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// basicOf is the Authorization value of login and token.
func basicOf(login, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(login+":"+token))
}

// TestGitServe: ServeGit turns existing repositories into commits and
// future ones into bare repositories, once, in an empty directory.
func TestGitServe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n", "docs/a.md", "a\n")
	e.p.SetSymlink(r.ID, "link", "README.md")
	e.p.SetGitlink(r.ID, "vendor/lib", strings.Repeat("cd", 20))
	e.ok()
	memHead := e.p.Head(r.ID)
	srv, err := e.p.ServeGit(filepath.Join(t.TempDir(), "absent", "yet"))
	must(t, err)
	t.Cleanup(func() { must(t, srv.Close()) })
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Errorf("server URL %q", srv.URL)
	}

	head := e.p.Head(r.ID)
	if len(head) != 40 || head == memHead {
		t.Fatalf("head after ServeGit %q (memory mode %q)", head, memHead)
	}
	if got := gitIn(t, e.p.GitDir(r.ID), "log", "--format=%an <%ae>|%s", head); got != "fake <fake@github.com>|fake: files set before git mode" {
		t.Errorf("seed commit: %q", got)
	}
	tree, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "")
	must(t, err)
	want := map[string][2]string{
		"README.md":  {fake.ModeFile, gitx.RawOID([]byte("hello\n"))},
		"docs/a.md":  {fake.ModeFile, gitx.RawOID([]byte("a\n"))},
		"link":       {fake.ModeSymlink, gitx.RawOID([]byte("README.md"))},
		"vendor/lib": {fake.ModeGitlink, strings.Repeat("cd", 20)},
	}
	if tree.Commit != head || len(tree.Entries) != len(want) {
		t.Fatalf("snapshot %s with %v, want %s with %d entries", tree.Commit, sortedKeys(tree.Entries), head, len(want))
	}
	for path, w := range want {
		if got := tree.Entries[path]; got.Mode != w[0] || got.OID != w[1] {
			t.Errorf("%s: %+v, want %v", path, got, w)
		}
	}

	// Later repositories are bare repositories too, empty until a commit.
	n := e.repo("acme/new")
	if !n.Empty || e.p.Head(n.ID) != "" {
		t.Errorf("new repository: empty %v, head %q", n.Empty, e.p.Head(n.ID))
	}
	if got := gitIn(t, e.p.GitDir(n.ID), "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("%s is not a bare repository: %q", e.p.GitDir(n.ID), got)
	}

	if _, err := e.p.ServeGit(t.TempDir()); err == nil {
		t.Error("a second ServeGit succeeded")
	}
	busy := t.TempDir()
	if err := os.WriteFile(filepath.Join(busy, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newEnv(t).p.ServeGit(busy); err == nil {
		t.Error("ServeGit in a directory that is not empty succeeded")
	}
	broken := fake.New("github.com")
	broken.AddRepo(platform.Repo{Path: "x"})
	if _, err := broken.ServeGit(t.TempDir()); err == nil {
		t.Error("ServeGit on a broken fixture succeeded")
	}
	must(t, srv.Close())
	must(t, srv.Close())
}

// TestGitRemoteAndAuth: remotes carry no credentials, headers send the
// account's token, and the server refuses what the platform would.
func TestGitRemoteAndAuth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	rem, err := e.p.Reader(e.reader).Remote(ctx, r)
	must(t, err)
	url := e.srv.URL + "/acme/api.git"
	readerAuth, writerAuth := basicOf(e.reader.Login, readerToken), basicOf(e.writer.Login, writerToken)
	// On GitHub an App reads with a read-only token: the account's, in the
	// fake's read-only form.
	if want := basicOf(e.reader.Login, "ro."+readerToken); rem.URL != url || header(t, rem) != want {
		t.Errorf("reader remote %s with %q, want %s with %q", rem.URL, header(t, rem), url, want)
	}
	// A target writer sends its own per-target token.
	tw := e.target(r)
	trem := tw.Remote()
	h := header(t, trem)
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); trem.URL != url || err != nil ||
		!strings.HasPrefix(string(dec), "x-access-token:") || h == writerAuth {
		t.Errorf("target remote %s with %q (%q), want %s with a per-target token", trem.URL, h, dec, url)
	}

	// A blobless fetch, then a blob on demand.
	c := newClient(t, rem)
	if tip := c.mustFetch("main"); tip != e.p.Head(r.ID) {
		t.Errorf("fetched %s, head %s", tip, e.p.Head(r.ID))
	}
	if got := c.show("refs/remotes/origin/main", "README.md"); got != "hello" {
		t.Errorf("README.md = %q", got)
	}
	for name, cl := range map[string]*client{
		"anonymous":      newClient(t, platform.Remote{URL: url}),
		"wrong token":    newClient(t, platform.Remote{URL: url, Header: fixed(basicOf(e.reader.Login, "nope"))}),
		"missing repo":   newClient(t, platform.Remote{URL: e.srv.URL + "/acme/missing.git", Header: rem.Header}),
		"another's name": newClient(t, platform.Remote{URL: url, Header: fixed(basicOf(e.other.Login, readerToken))}),
	} {
		if _, err := cl.fetch("main"); err == nil {
			t.Errorf("%s: the fetch succeeded", name)
		}
	}

	for _, tc := range []struct {
		method, url, auth string
		status            int
	}{
		{http.MethodGet, url + "/info/refs?service=git-upload-pack", "", http.StatusUnauthorized},
		{http.MethodGet, url + "/info/refs?service=git-upload-pack", basicOf(e.reader.Login, writerToken), http.StatusUnauthorized},
		{http.MethodGet, url + "/info/refs?service=git-upload-pack", readerAuth, http.StatusOK},
		{http.MethodGet, e.srv.URL + "/ACME/Api.git/info/refs?service=git-upload-pack", readerAuth, http.StatusOK},
		{http.MethodGet, e.srv.URL + "/acme/missing.git/info/refs?service=git-upload-pack", readerAuth, http.StatusNotFound},
		{http.MethodGet, url + "/info/refs?service=git-receive-pack", readerAuth, http.StatusForbidden},
		{http.MethodGet, url + "/info/refs?service=git-receive-pack", writerAuth, http.StatusOK},
		{http.MethodGet, url + "/info/refs", readerAuth, http.StatusForbidden},
		{http.MethodGet, url + "/HEAD", readerAuth, http.StatusNotFound},
		{http.MethodPost, url + "/info/refs?service=git-upload-pack", readerAuth, http.StatusMethodNotAllowed},
		{http.MethodGet, url + "/git-upload-pack", readerAuth, http.StatusMethodNotAllowed},
	} {
		status, challenge := httpStatus(t, tc.method, tc.url, tc.auth)
		if status != tc.status {
			t.Errorf("%s %s: %d, want %d", tc.method, strings.TrimPrefix(tc.url, e.srv.URL), status, tc.status)
		}
		if status == http.StatusUnauthorized && !strings.HasPrefix(challenge, "Basic ") {
			t.Errorf("401 without a Basic challenge: %q", challenge)
		}
	}

	// The writer pushes; the reader may not; nobody pushes to an archive.
	w := newClient(t, trem)
	base := w.mustFetch("main")
	commit := w.commit(base, "add a.md", "a.md", "a\n")
	w.mustPush(commit, "touchmark/hub", "")
	if got := e.p.Branch(r.ID, "touchmark/hub"); got != commit {
		t.Errorf("branch after the push: %q, want %s", got, commit)
	}
	c.mustFetch("main")
	if err := c.push(c.commit(base, "reader", "r.md", "r\n"), "reader", ""); err == nil {
		t.Error("the reader pushed")
	}
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.Archived = true })
	e.ok()
	if err := w.push(commit, "after-archive", ""); err == nil {
		t.Error("a push to an archived repository succeeded")
	}
	if e.p.Branch(r.ID, "reader") != "" || e.p.Branch(r.ID, "after-archive") != "" {
		t.Error("a refused push moved a branch")
	}

	// No token, no header; after Close the header fails.
	e.p.SetToken(e.reader, "")
	if rem, err := e.p.Reader(e.reader).Remote(ctx, r); err != nil || rem.Header != nil {
		t.Errorf("remote without a token: %+v, %v", rem, err)
	}
	if _, err := rem.Header(ctx); platform.ClassOf(err) != platform.ClassAuth {
		t.Errorf("a header after the token was removed: %v", err)
	}
	must(t, tw.Close())
	_, err = trem.Header(ctx)
	wantClass(t, "header after Close", err, platform.ClassAuth)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.target(r).Remote().Header(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("header with a cancelled context: %v", err)
	}
}

// fixed is a header source of one value.
func fixed(value string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return value, nil }
}

// TestGitLargePush: a push bigger than git's http.postBuffer arrives
// chunked, which CGI cannot stream; the server reads it first.
func TestGitLargePush(t *testing.T) {
	t.Parallel()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	w := newClient(t, e.target(r).Remote())
	// Two MiB that do not compress.
	big := make([]byte, 2<<20)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range big {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		big[i] = byte(x)
	}
	c := w.commit(w.mustFetch("main"), "big", "big.bin", string(big))
	w.mustPush(c, "touchmark/big", "")
	f, err := e.p.Reader(e.reader).ReadFile(t.Context(), r, "touchmark/big", "big.bin", 4<<20)
	if err != nil || gitx.RawOID(f.Content) != gitx.RawOID(big) {
		t.Errorf("the big file came back with %d bytes, %v", len(f.Content), err)
	}
}

func TestGitSetToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.p.SetToken(e.reader, "t1")
	e.p.SetToken(e.reader, "t1") // again, same account
	e.ok()
	e.p.SetToken(e.writer, "t1")
	wantSetupErr(t, e.p, "belongs to")
	if strings.Contains(e.p.Err().Error(), "t1") {
		t.Errorf("the setup error repeats the token: %v", e.p.Err())
	}
	q := newEnv(t)
	q.p.SetToken(platform.Account{ID: "404"}, "x")
	wantSetupErr(t, q.p, "unknown account")
	k := newEnv(t)
	k.p.SetKnownAuthors(k.writer, platform.Account{ID: "404"})
	wantSetupErr(t, k.p, "unknown account")
}

// TestGitSetupCommits: setup methods commit to the default branch, and
// reads see any branch or commit, byte for byte.
func TestGitSetupCommits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api")
	rd := e.p.Reader(e.reader)
	_, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "")
	wantClass(t, "snapshot of an empty repository", err, platform.ClassNotFound)
	wantIs(t, "snapshot of an empty repository", err, platform.ErrNotFound)
	_, err = rd.ReadFile(ctx, r, "", "README.md", 100)
	wantIs(t, "ReadFile in an empty repository", err, platform.ErrNotFound)

	crlf := "one\r\ntwo\r\n"
	e.p.SetFile(r.ID, "README.md", []byte(crlf), "")
	e.p.SetFile(r.ID, "bin/run", []byte("#!/bin/sh\n"), fake.ModeExecutable)
	e.p.SetSymlink(r.ID, "link", "README.md")
	e.p.SetGitlink(r.ID, "vendor/lib", strings.Repeat("AB", 20))
	e.p.SetFile(r.ID, "weird name\t\"quoted\".md", []byte("w"), "")
	e.ok()
	h1 := e.p.Head(r.ID)
	if got := gitIn(t, e.p.GitDir(r.ID), "rev-list", "--count", h1); got != "5" {
		t.Errorf("%s commits, want one per setup call", got)
	}
	if got, _ := e.p.RepoByID(r.ID); got.Empty {
		t.Error("a repository with commits is Empty")
	}
	e.p.SetFile(r.ID, "README.md", []byte(crlf), fake.ModeFile)
	if e.p.Head(r.ID) != h1 {
		t.Error("an unchanged file made a commit")
	}
	for _, ref := range []string{"", "main", "refs/heads/main", h1, strings.ToUpper(h1)} {
		f, err := rd.ReadFile(ctx, r, ref, "README.md", 100)
		if err != nil || string(f.Content) != crlf || f.OID != gitx.RawOID([]byte(crlf)) || f.Mode != fake.ModeFile {
			t.Errorf("ReadFile at %q = %+v, %v", ref, f, err)
		}
	}
	if f, err := rd.ReadFile(ctx, r, "", "bin/run", 100); err != nil || f.Mode != fake.ModeExecutable {
		t.Errorf("executable = %+v, %v", f, err)
	}
	if f, err := rd.ReadFile(ctx, r, "", "weird name\t\"quoted\".md", 100); err != nil || string(f.Content) != "w" {
		t.Errorf("a path fast-import quotes = %+v, %v", f, err)
	}

	e.p.SetFile(r.ID, "README.md", []byte("changed\n"), "")
	e.p.RemoveFile(r.ID, "link")
	e.ok()
	if f, err := rd.ReadFile(ctx, r, h1, "README.md", 100); err != nil || string(f.Content) != crlf {
		t.Errorf("ReadFile at an old commit = %q, %v", f.Content, err)
	}
	if f, err := rd.ReadFile(ctx, r, "", "README.md", 100); err != nil || string(f.Content) != "changed\n" {
		t.Errorf("ReadFile at the head = %q, %v", f.Content, err)
	}
	for name, tc := range map[string]struct {
		ref, path string
		max       int64
		target    error
	}{
		"removed":          {"", "link", 100, platform.ErrNotFound},
		"a symlink":        {h1, "link", 100, platform.ErrNotRegular},
		"a submodule":      {"", "vendor/lib", 100, platform.ErrNotRegular},
		"a directory":      {"", "bin", 100, platform.ErrNotRegular},
		"under a file":     {"", "README.md/x", 100, platform.ErrNotFound},
		"too large":        {"", "bin/run", 3, platform.ErrTooLarge},
		"unknown branch":   {"no-such-branch", "README.md", 100, platform.ErrNotFound},
		"unknown commit":   {strings.Repeat("0", 40), "README.md", 100, platform.ErrNotFound},
		"a tree id as ref": {gitIn(t, e.p.GitDir(r.ID), "rev-parse", h1+"^{tree}"), "README.md", 100, platform.ErrNotFound},
	} {
		_, err := rd.ReadFile(ctx, r, tc.ref, tc.path, tc.max)
		wantIs(t, name, err, tc.target)
	}

	tree, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, h1)
	must(t, err)
	if tree.Commit != h1 || tree.Entries["link"].Mode != fake.ModeSymlink || tree.Entries["vendor/lib"].OID != strings.Repeat("ab", 20) {
		t.Errorf("snapshot at %s: %+v", h1, tree)
	}
	rem, err := rd.Remote(ctx, r)
	must(t, err)
	if tree, err := e.p.Snapshots().Snapshot(ctx, r, rem, "main"); err != nil || tree.Commit != e.p.Head(r.ID) {
		t.Errorf("snapshot with the remote: %v", err)
	}
	_, err = e.p.Snapshots().Snapshot(ctx, r, platform.Remote{URL: "fake://github.com/acme/api"}, "")
	wantClass(t, "the memory mode's remote", err, platform.ClassInvalid)
	_, err = e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "no-such-branch")
	wantClass(t, "an unknown ref", err, platform.ClassNotFound)

	// The same steps give the same commits on another platform.
	q := newGitEnv(t)
	qr := q.repo("acme/other")
	q.p.SetFile(qr.ID, "README.md", []byte(crlf), "")
	q.p.SetFile(qr.ID, "bin/run", []byte("#!/bin/sh\n"), fake.ModeExecutable)
	q.p.SetSymlink(qr.ID, "link", "README.md")
	q.p.SetGitlink(qr.ID, "vendor/lib", strings.Repeat("AB", 20))
	q.p.SetFile(qr.ID, "weird name\t\"quoted\".md", []byte("w"), "")
	q.ok()
	if q.p.Head(qr.ID) != h1 {
		t.Errorf("the same steps gave %s, then %s", h1, q.p.Head(qr.ID))
	}
}

// TestGitPRsFollowBranches: open PRs follow their head branch, closed ones
// keep their head, and PRs need real branches.
func TestGitPRsFollowBranches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	tw := e.target(r)
	np := platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"}
	_, err := tw.CreatePR(ctx, np)
	wantClass(t, "a missing head branch", err, platform.ClassInvalid)
	h1 := e.push(r, "touchmark/hub", e.writer, "a.md", "a\n")
	_, err = tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "release", Title: "sync"})
	wantClass(t, "a missing base branch", err, platform.ClassInvalid)
	pr, err := tw.CreatePR(ctx, np)
	must(t, err)
	if pr.HeadSHA != h1 {
		t.Errorf("new PR head %q, want %s", pr.HeadSHA, h1)
	}
	h2 := e.push(r, "touchmark/hub", e.other, "b.md", "b\n")
	prs, err := e.p.Reader(e.reader).PRs(ctx, r, []string{"touchmark/hub"}, nil)
	if err != nil || len(prs) != 1 || prs[0].HeadSHA != h2 {
		t.Errorf("after a push: %+v, %v", prs, err)
	}

	// A closed PR keeps its head; reopened, it follows again.
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Closed)})
	must(t, err)
	h3 := e.push(r, "touchmark/hub", e.other, "c.md", "c\n")
	if got := e.p.PR(r.ID, pr.Number).HeadSHA; got != h2 {
		t.Errorf("closed PR head %s, want %s", got, h2)
	}
	got, err := tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Open)})
	if err != nil || got.HeadSHA != h3 || got.State != platform.Open {
		t.Errorf("reopened: %+v, %v", got, err)
	}

	// A fork's PR follows the fork's branch.
	fork := e.p.AddRepo(platform.Repo{Path: "jdoe/api", Fork: true})
	f1 := e.push(fork, "touchmark/hub", e.other, "f.md", "f\n")
	n := e.pr(r, platform.PR{Head: "touchmark/hub", HeadRepoID: fork.ID, Author: e.other})
	if got := e.p.PR(r.ID, n).HeadSHA; got != f1 {
		t.Errorf("fork PR head %q, want %s", got, f1)
	}
	f2 := e.push(fork, "touchmark/hub", e.other, "g.md", "g\n")
	if e.p.PR(r.ID, n).HeadSHA != f2 || e.p.PR(r.ID, pr.Number).HeadSHA != h3 {
		t.Error("a push to the fork moved the wrong PRs")
	}
	if got := gitIn(t, e.p.GitDir(fork.ID), "rev-list", "--count", f2); got != "2" {
		t.Errorf("the fork's history has %s commits, want 2 from a root", got)
	}

	// A new base must exist; reopening needs the head branch.
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{Base: ptr("release")})
	wantClass(t, "a missing new base", err, platform.ClassInvalid)
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Closed)})
	must(t, err)
	e.p.DeleteBranch(r.ID, "touchmark/hub")
	e.ok()
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Open)})
	wantClass(t, "reopening without a head branch", err, platform.ClassInvalid)
}

// TestGitNoCommitsBetween: GitHub refuses a PR whose head its base already
// contains; the others open it.
func TestGitNoCommitsBetween(t *testing.T) {
	t.Parallel()
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea} {
		e := newGitEnv(t, fake.WithFlavor(f))
		r := e.repo("acme/api", "README.md", "hello\n")
		tw := e.target(r)
		w := newClient(t, tw.Remote())
		w.mustPush(w.mustFetch("main"), "same", "")
		_, err := tw.CreatePR(t.Context(), platform.NewPR{Head: "same", Base: "main", Title: "nothing"})
		if f == fake.GitHub {
			wantClass(t, "GitHub", err, platform.ClassInvalid)
		} else if err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// TestGitBehaviors: what each flavor does when branches move.
func TestGitBehaviors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flavor fake.Flavor
		// toBase is the state of an open PR whose head is pushed to its
		// base's tip; contained that of an open PR whose head a person
		// merges into the base through another PR.
		toBase, contained platform.PRState
	}{
		{fake.GitHub, platform.Closed, platform.Open},
		{fake.GitLab, platform.Open, platform.Open},
		{fake.Gitea, platform.Merged, platform.Merged},
		{fake.Forgejo, platform.Merged, platform.Merged},
	} {
		t.Run(string(tc.flavor), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e := newGitEnv(t, fake.WithFlavor(tc.flavor))
			r := e.repo("acme/api", "README.md", "hello\n")
			tw := e.target(r)
			w := newClient(t, tw.Remote())
			base := w.mustFetch("main")
			c1 := w.commit(base, "sync", "a.md", "a\n")
			w.mustPush(c1, "touchmark/hub", "")
			pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"})
			must(t, err)

			// The head pushed onto its base: forbidden everywhere.
			w.mustPush(base, "touchmark/hub", c1)
			got := e.p.PR(r.ID, pr.Number)
			if got.State != tc.toBase || got.HeadSHA != base {
				t.Errorf("head at the base: %s at %s, want %s", got.State, got.HeadSHA, tc.toBase)
			}
			if tc.toBase != platform.Open && (got.ClosedBy == nil || got.ClosedBy.ID != e.writer.ID || got.ClosedAt.IsZero()) {
				t.Errorf("closed by %v at %v, want the pusher", got.ClosedBy, got.ClosedAt)
			}
			e.violations("head-to-base")

			// The head branch of an open PR deleted: closed on every flavor.
			c2 := w.commit(base, "other", "b.md", "b\n")
			w.mustPush(c2, "touchmark/other", "")
			pr2, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/other", Base: "main", Title: "other"})
			must(t, err)
			w.mustPush("", "touchmark/other", c2)
			if got := e.p.PR(r.ID, pr2.Number); got.State != platform.Closed || got.HeadSHA != c2 || got.ClosedBy == nil {
				t.Errorf("branch deleted: %s at %s by %v", got.State, got.HeadSHA, got.ClosedBy)
			}
			e.violations("deleted-open-branch")

			// A person merges a PR whose head contains another open PR's head.
			e.p.GrantWrite(r.ID, e.other)
			pc := newClient(t, platform.Remote{URL: w.remote.URL, Header: fixed(basicOf(e.other.Login, otherToken))})
			x := pc.commit(pc.mustFetch("main"), "x", "x.md", "x\n")
			y := pc.commit(x, "y", "y.md", "y\n")
			pc.mustPush(x, "lower", "")
			pc.mustPush(y, "upper", "")
			lower := e.pr(r, platform.PR{Head: "lower", Author: e.other})
			upper := e.pr(r, platform.PR{Head: "upper", Author: e.other, Title: "stacked"})
			if _, err := e.p.MergePR(r.ID, upper, fake.MergeCommit, e.other, time.Time{}); err != nil {
				t.Fatalf("MergePR: %v", err)
			}
			got = e.p.PR(r.ID, lower)
			if got.State != tc.contained {
				t.Errorf("a head merged through another PR: %s, want %s", got.State, tc.contained)
			}
			if tc.contained == platform.Merged && (got.ClosedBy == nil || got.ClosedBy.ID != e.other.ID) {
				t.Errorf("merged by %v, want the merger", got.ClosedBy)
			}
			// A person deletes a branch: its open PR closes, no violation.
			e.p.DeleteBranch(r.ID, "lower")
			e.ok()
			if got := e.p.PR(r.ID, lower); got.State == platform.Open {
				t.Error("the PR of a deleted branch is open")
			}
			e.violations()
		})
	}
}

// TestGitHeadBehindBase: a push that leaves an open PR's head behind its
// base (an older base commit, such as E or Hc's parent pushed instead of
// the built commit) is the head-to-base violation too, not only a head
// equal to the base's tip; GitHub keeps such a PR open.
func TestGitHeadBehindBase(t *testing.T) {
	t.Parallel()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	tw := e.target(r)
	w := newClient(t, tw.Remote())
	base := w.mustFetch("main")
	c1 := w.commit(base, "one", "a.md", "1\n")
	w.mustPush(c1, "touchmark/hub", "")
	pr, err := tw.CreatePR(t.Context(), platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"})
	must(t, err)
	e.push(r, "main", e.other, "m.md", "m\n")
	e.violations()
	w.mustPush(base, "touchmark/hub", c1)
	if v := e.violations("head-to-base"); len(v) == 1 && !strings.Contains(v[0], "already contains") {
		t.Errorf("violation %q", v[0])
	}
	if got := e.p.PR(r.ID, pr.Number); got.State != platform.Open || got.HeadSHA != base {
		t.Errorf("the PR is %s at %s, want open at %s", got.State, got.HeadSHA, base)
	}
}

// TestGitEditPRSettles: Gitea marks a PR merged when an edit makes its head
// part of its base: a new base that contains the head, or a reopening after
// the head reached the base.
func TestGitEditPRSettles(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, f := range []fake.Flavor{fake.Gitea, fake.GitHub} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			e := newGitEnv(t, fake.WithFlavor(f))
			r := e.repo("acme/api", "README.md", "hello\n")
			tw := e.target(r)
			w := newClient(t, tw.Remote())
			base := w.mustFetch("main")
			c1 := w.commit(base, "one", "a.md", "1\n")
			w.mustPush(c1, "touchmark/hub", "")
			w.mustPush(c1, "release", "")
			pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"})
			must(t, err)
			got, err := tw.EditPR(ctx, pr.Number, platform.PREdit{Base: ptr("release")})
			must(t, err)
			// GitHub closes a PR whose head is its base's tip.
			want := platform.Closed
			if f == fake.Gitea {
				want = platform.Merged
			}
			if got.State != want || e.p.PR(r.ID, pr.Number).State != want {
				t.Errorf("after a new base that contains the head: %s, want %s", got.State, want)
			}

			// A closed PR whose head a person then merged into main.
			c2 := w.commit(base, "two", "b.md", "2\n")
			w.mustPush(c2, "touchmark/other", "")
			pr2, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/other", Base: "main", Title: "other"})
			must(t, err)
			_, err = tw.EditPR(ctx, pr2.Number, platform.PREdit{State: ptr(platform.Closed)})
			must(t, err)
			pc := newClient(t, platform.Remote{URL: w.remote.URL, Header: fixed(basicOf(e.other.Login, otherToken))})
			e.p.GrantWrite(r.ID, e.other)
			e.ok()
			pc.mustFetch("main")
			pc.mustFetch("touchmark/other")
			pc.mustPush(c2, "main", base)
			// Every push through the server is judged as touchmark's.
			e.violations("default-branch")
			got, err = tw.EditPR(ctx, pr2.Number, platform.PREdit{State: ptr(platform.Open)})
			must(t, err)
			if f == fake.Gitea && got.State != platform.Merged {
				t.Errorf("reopened after its head reached the base: %s, want merged", got.State)
			}
			if f == fake.GitHub && got.State != platform.Closed {
				// GitHub closes a PR whose head is its base's tip.
				t.Errorf("reopened with its head at the base's tip: %s, want closed", got.State)
			}
		})
	}
}

// TestGitRepoIDsThatAreDevices: an id that names a Windows device ("con",
// "nul", "aux", "com1") gets a bare repository on every system.
func TestGitRepoIDsThatAreDevices(t *testing.T) {
	t.Parallel()
	e := newGitEnv(t)
	for _, id := range []string{"con", "nul", "aux", "com1", "lpt9", "prn"} {
		r := e.p.AddRepo(platform.Repo{ID: id, Path: "acme/" + id})
		e.p.SetFile(r.ID, "README.md", []byte(id+"\n"), "")
		e.ok()
		if base := filepath.Base(e.p.GitDir(id)); strings.EqualFold(base, id+".git") {
			t.Errorf("repository %s lives in %s", id, base)
		}
		if tree, err := e.p.Snapshots().Snapshot(t.Context(), r, platform.Remote{}, ""); err != nil || len(tree.Entries) != 1 {
			t.Errorf("snapshot of %s: %+v, %v", id, tree, err)
		}
	}
}

// TestGitViolations: the forbidden transitions (see Violations) and what
// excuses them.
func TestGitViolations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	tw := e.target(r)
	w := newClient(t, tw.Remote())
	base := w.mustFetch("main")
	c1 := w.commit(base, "one", "a.md", "1\n")
	w.mustPush(c1, "touchmark/hub", "")
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"})
	must(t, err)
	c2 := w.commit(base, "two", "a.md", "2\n")
	w.mustPush(c2, "touchmark/hub", c1)
	e.violations()

	// The branch of a closed PR: a push with no new PR after it.
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Closed)})
	must(t, err)
	c3 := w.commit(base, "three", "a.md", "3\n")
	w.mustPush(c3, "touchmark/hub", c2)
	v := e.violations("closed-branch-push")
	if len(v) == 1 && !strings.Contains(v[0], fmt.Sprintf("acme/api#%d", pr.Number)) {
		t.Errorf("the violation does not name #%d: %q", pr.Number, v[0])
	}
	c4 := w.commit(base, "four", "a.md", "4\n")
	w.mustPush(c4, "touchmark/hub", c3)
	_, err = tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"})
	must(t, err)
	e.violations()

	// Someone else's open PR on a branch.
	x := e.push(r, "feature", e.other, "f.md", "f\n")
	e.pr(r, platform.PR{Head: "feature", Author: e.other})
	w.mustFetch("feature")
	f1 := w.commit(x, "on feature", "g.md", "g\n")
	w.mustPush(f1, "feature", x)
	e.violations("foreign-branch")
	e.p.SetKnownAuthors(e.writer, e.other)
	e.ok()
	f2 := w.commit(f1, "known", "h.md", "h\n")
	w.mustPush(f2, "feature", f1)
	e.violations()
	e.p.SetKnownAuthors(e.writer)
	e.ok()
	w.mustPush(x, "feature", f2)
	e.violations("foreign-branch")
	w.mustPush("", "feature", x)
	e.violations("deleted-open-branch", "foreign-branch")

	// The default branch is never touchmark's.
	w.mustPush(w.commit(base, "direct", "m.md", "m\n"), "main", base)
	e.violations("default-branch")

	writes := e.p.Writes()
	if !slices.Contains(writes, "Push acme/api touchmark/hub") || !slices.Contains(writes, "Push acme/api main") {
		t.Errorf("Writes = %q, want the pushes", writes)
	}
}
