package snapshot

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// servedRepo is a bare repository for GitSource to fetch from.
type servedRepo struct {
	t   *testing.T
	dir string
	g   *gitx.Git
}

// newServedRepo creates a bare repository root/name that serves blobless
// fetches and pushes.
func newServedRepo(t *testing.T, root, name string) *servedRepo {
	t.Helper()
	dir := filepath.Join(root, name)
	g := &gitx.Git{Env: testEnv(t)}
	s := &servedRepo{t: t, dir: dir, g: &gitx.Git{Dir: dir, Env: g.Env}}
	if _, err := g.Run(t.Context(), nil, "init", "-q", "--bare", "-b", "main", dir); err != nil {
		t.Fatal(err)
	}
	s.run("", "config", "uploadpack.allowFilter", "true")
	s.run("", "config", "http.receivepack", "true")
	return s
}

func (s *servedRepo) run(stdin string, args ...string) string {
	s.t.Helper()
	out, err := s.g.Run(s.t.Context(), strings.NewReader(stdin), args...)
	if err != nil {
		s.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a root commit of entries on branch with fast-import and
// returns its id.
func (s *servedRepo) commit(branch string, entries []treeEntry) string {
	s.t.Helper()
	var in strings.Builder
	fmt.Fprintf(&in, "reset refs/heads/%s\ncommit refs/heads/%s\n", branch, branch)
	in.WriteString("committer touchmark test <test@example.com> 1767225600 +0000\ndata 7\nserved\n")
	for _, e := range entries {
		if e.oid != "" {
			fmt.Fprintf(&in, "M %s %s %s\n", e.mode, e.oid, e.path)
			continue
		}
		fmt.Fprintf(&in, "M %s inline %s\ndata %d\n%s\n", e.mode, e.path, len(e.content), e.content)
	}
	s.run(in.String(), "-c", "core.autocrlf=false", "fast-import", "--quiet", "--force")
	return s.run("", "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
}

// lsTree lists rev with git itself, as the reference.
func (s *servedRepo) lsTree(rev string) map[string]Entry {
	s.t.Helper()
	out := s.run("", "ls-tree", "-r", "-z", "--full-tree", rev)
	m := map[string]Entry{}
	for rec := range strings.SplitSeq(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		m[path] = Entry{Mode: f[0], OID: f[2]}
	}
	return m
}

// fixtureEntries holds every kind of entry.
var fixtureEntries = []treeEntry{
	{mode: "100644", path: "README.md", content: "readme\n"},
	{mode: "100755", path: "scripts/check.sh", content: "#!/bin/sh\n"},
	{mode: "120000", path: "link.md", content: "README.md"},
	{mode: "160000", path: "vendor/sub", oid: "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d"},
	{mode: "100644", path: ".engineering-assets.yml", content: "packs: [agents]\n"},
	{mode: "100644", path: "docs/deep/guide.md", content: "guide\n"},
}

func newSource(t *testing.T) *GitSource {
	t.Helper()
	return &GitSource{Dir: t.TempDir(), Isolation: gitx.Isolation{Home: t.TempDir(), AllowFile: true, AllowHTTP: true}}
}

func TestGitSourceSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServedRepo(t, root, "api.git")
	head := s.commit("main", fixtureEntries)
	other := s.commit("develop", fixtureEntries[:2])
	src := newSource(t)
	repo := platform.Repo{Host: "git.example.com", ID: "42", Path: "acme/api", DefaultBranch: "main"}
	remote := platform.Remote{URL: s.dir}
	want := &Tree{Commit: head, Entries: s.lsTree(head)}
	for _, ref := range []string{"", "main", "refs/heads/main", head} {
		got, err := src.Snapshot(t.Context(), repo, remote, ref)
		if err != nil {
			t.Fatalf("Snapshot(%q) = %v", ref, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Snapshot(%q) =\n%+v\nwant\n%+v", ref, got, want)
		}
	}
	if len(want.Entries) != len(fixtureEntries) || want.Entries["vendor/sub"].Mode != "160000" || want.Entries["link.md"].Mode != "120000" {
		t.Fatalf("fixture: %v", want.Entries)
	}
	if got, err := src.Snapshot(t.Context(), repo, remote, "develop"); err != nil || got.Commit != other || len(got.Entries) != 2 {
		t.Errorf("Snapshot(develop) = %+v, %v", got, err)
	}

	// No such branch, or a commit that was never fetched: ClassNotFound.
	for _, ref := range []string{"gone", strings.Repeat("0", 40)} {
		_, err := src.Snapshot(t.Context(), repo, remote, ref)
		if platform.ClassOf(err) != platform.ClassNotFound || !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("Snapshot(%q) = %v, want ClassNotFound", ref, err)
		}
	}
	// An empty repository is "empty" for distribute.
	empty := newServedRepo(t, root, "empty.git")
	_, err := src.Snapshot(t.Context(), platform.Repo{Host: "git.example.com", ID: "43", Path: "acme/empty", DefaultBranch: "main"},
		platform.Remote{URL: empty.dir}, "")
	if platform.ClassOf(err) != platform.ClassNotFound {
		t.Errorf("Snapshot(empty) = %v, want ClassNotFound", err)
	}
	if _, err := src.Snapshot(t.Context(), platform.Repo{Host: "git.example.com", ID: "44", Path: "acme/x"},
		platform.Remote{URL: s.dir}, ""); err == nil {
		t.Error("Snapshot without a default branch succeeded")
	}
}

// TestGitSourceObserve: a snapshot taken through git observes like the
// tree the other tests build.
func TestGitSourceObserve(t *testing.T) {
	t.Parallel()
	s := newServedRepo(t, t.TempDir(), "r.git")
	s.commit("main", fixtureEntries)
	src := newSource(t)
	got, err := src.Snapshot(t.Context(), platform.Repo{Host: "h", ID: "1", Path: "o/r", DefaultBranch: "main"}, platform.Remote{URL: s.dir}, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := gitTree(t, fixtureEntries)
	paths := []string{"README.md", "scripts/check.sh", "link.md", "vendor/sub/x", "docs", "readme.md", "new.md"}
	if g, w := got.Observe(paths), ref.Observe(paths); !reflect.DeepEqual(g, w) {
		t.Errorf("Observe() =\n%v\nwant\n%v", g, w)
	}
}

func TestGitSourceRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServedRepo(t, root, "api.git")
	s.commit("main", fixtureEntries[:1])
	src := newSource(t)
	remote := platform.Remote{URL: s.dir}
	a, err := src.Repo(t.Context(), platform.Repo{Host: "Git.Example.com", ID: "42", Path: "acme/api"}, remote)
	if err != nil {
		t.Fatal(err)
	}
	b, err := src.Repo(t.Context(), platform.Repo{Host: "git.example.com", ID: "42", Path: "acme/renamed"}, remote)
	if err != nil || b.Dir != a.Dir {
		t.Errorf("Repo(same host and id) = %v, %v; want %s", b, err, a.Dir)
	}
	c, err := src.Repo(t.Context(), platform.Repo{Host: "git.example.com", ID: "43", Path: "acme/api"}, remote)
	if err != nil || c.Dir == a.Dir || filepath.Dir(c.Dir) != filepath.Clean(src.Dir) {
		t.Errorf("Repo(other id) = %v, %v", c, err)
	}
	if _, err := src.Repo(t.Context(), platform.Repo{Host: "git.example.com", ID: "42", Path: "acme/api"}, platform.Remote{URL: filepath.Join(root, "other.git")}); err == nil {
		t.Error("Repo(changed URL) succeeded")
	}
	for _, r := range []platform.Repo{{ID: "1", Path: "x"}, {Host: "h", Path: "x"}} {
		if _, err := src.Repo(t.Context(), r, remote); err == nil {
			t.Errorf("Repo(%+v) succeeded", r)
		}
	}
	if _, err := (&GitSource{}).Repo(t.Context(), platform.Repo{Host: "h", ID: "1"}, remote); err == nil {
		t.Error("Repo without Dir succeeded")
	}
	// A refused remote does not stick: the next call tries again.
	bad := platform.Repo{Host: "h", ID: "9", Path: "x"}
	if _, err := src.Repo(t.Context(), bad, platform.Remote{URL: "ssh://h/x.git"}); err == nil {
		t.Error("Repo(ssh remote) succeeded")
	}
	if _, err := src.Repo(t.Context(), bad, remote); err != nil {
		t.Errorf("Repo after a refused remote = %v", err)
	}
	if got, want := repoDir("h\x00x"), repoDir("h\x00x"); got != want || len(got) != 24 || repoDir("h\x00y") == got {
		t.Errorf("repoDir = %q", got)
	}
}

// TestGitSourceHTTP: snapshots through a smart-HTTP server with the read
// credential; the repository Repo returns for the write remote pushes with
// the write credential.
func TestGitSourceHTTP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServedRepo(t, root, "acme/api.git")
	head := s.commit("main", fixtureEntries)
	read, write := basicHeader("reader", "tm-read-token-1a2b"), basicHeader("writer", "tm-write-token-3c4d")
	srv := newHTTPServer(t, root, read, write)
	url := srv.URL + "/acme/api.git"
	repo := platform.Repo{Host: "127.0.0.1", ID: "7", Path: "acme/api", DefaultBranch: "main"}
	header := func(v string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return v, nil }
	}
	src := newSource(t)
	tree, err := src.Snapshot(t.Context(), repo, platform.Remote{URL: url, Header: header(read)}, "")
	if err != nil || tree.Commit != head || !reflect.DeepEqual(tree.Entries, s.lsTree(head)) {
		t.Fatalf("Snapshot() = %+v, %v", tree, err)
	}
	w, err := src.Repo(t.Context(), repo, platform.Remote{URL: url, Header: header(write)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Push(t.Context(), gitx.PushSpec{Branch: "touchmark/acme", Commit: head})
	if err != nil || res.Status != gitx.PushOK {
		t.Fatalf("Push(write credential) = %+v, %v", res, err)
	}
	// The read credential may not push: the identity's permission (403),
	// not a broken credential.
	r, err := src.Repo(t.Context(), repo, platform.Remote{URL: url, Header: header(read)})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.Push(t.Context(), gitx.PushSpec{Branch: "touchmark/other", Commit: head}); err != nil || res.Status != gitx.PushPermission {
		t.Errorf("Push(read credential) = %+v, %v; want PushPermission", res, err)
	}
	// Refused fetches, each with the class distribute reports it by.
	for name, c := range map[string]struct {
		header string
		class  platform.Class
	}{
		"anonymous":    {"", platform.ClassAuth},                            // failed:auth
		"wrong token":  {basicHeader("reader", "nope"), platform.ClassAuth}, // failed:auth
		"no access":    {noAccess, platform.ClassPermission},                // failed:access
		"rate limited": {rateLimited, platform.ClassRateLimited},            // deferred:rate-limit
	} {
		rem := platform.Remote{URL: url}
		if c.header != "" {
			rem.Header = header(c.header)
		}
		_, err := src.Snapshot(t.Context(), repo, rem, "")
		if platform.ClassOf(err) != c.class {
			t.Errorf("%s: Snapshot() = %v, want %v", name, err, c.class)
		}
		if strings.Contains(fmt.Sprint(err), "tm-read-token") {
			t.Errorf("%s: the error leaks a credential: %v", name, err)
		}
	}
	// A server that is gone: failed:transient.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	other := platform.Repo{Host: "127.0.0.1", ID: "8", Path: "acme/gone", DefaultBranch: "main"}
	if _, err := src.Snapshot(t.Context(), other, platform.Remote{URL: gone.URL + "/acme/gone.git"}, ""); platform.ClassOf(err) != platform.ClassTransient {
		t.Errorf("Snapshot(server gone) = %v, want ClassTransient", err)
	}
}

// TestClassifyFetchErrors: every failure class gitx tells has the class
// distribute reports it by; the fetch's own three-minute bound is
// transient. The caller's own deadline is left as it is: the caller tells
// it by its context (deferred:deadline), whatever ClassOf reads in it.
func TestClassifyFetchErrors(t *testing.T) {
	t.Parallel()
	repo := platform.Repo{Path: "acme/api"}
	gitErr := func(stderr string) error { return &gitx.Error{Args: []string{"fetch"}, Code: 128, Stderr: stderr} }
	for _, c := range []struct {
		err  error
		want platform.Class // ClassUnknown: not classified by classify
	}{
		{gitErr("fatal: Authentication failed for 'https://x/r.git/'"), platform.ClassAuth},
		{gitErr("fatal: unable to access 'https://x/r.git/': The requested URL returned error: 403"), platform.ClassPermission},
		{gitErr("fatal: unable to access 'https://x/r.git/': The requested URL returned error: 429"), platform.ClassRateLimited},
		{gitErr("fatal: unable to access 'https://x/r.git/': The requested URL returned error: 502"), platform.ClassTransient},
		{fmt.Errorf("git fetch: %w after 3m0s", gitx.ErrNetworkTimeout), platform.ClassTransient},
		{fmt.Errorf("git fetch: %w", context.DeadlineExceeded), platform.ClassUnknown},
		{gitErr("fatal: bad object"), platform.ClassUnknown},
	} {
		err := classify(repo, c.err)
		var pe *platform.Error
		got := platform.ClassUnknown
		if errors.As(err, &pe) {
			got = pe.Class
		}
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("classify(%v) = %v (class %v), want class %v wrapping it", c.err, err, got, c.want)
		}
	}
}

// TestGitSourceRelease: a released target's repository is gone, and the
// next call for the target starts a new one.
func TestGitSourceRelease(t *testing.T) {
	t.Parallel()
	s := newServedRepo(t, t.TempDir(), "api.git")
	head := s.commit("main", fixtureEntries[:1])
	src := newSource(t)
	repo := platform.Repo{Host: "h", ID: "1", Path: "acme/api", DefaultBranch: "main"}
	remote := platform.Remote{URL: s.dir}
	first, err := src.Repo(t.Context(), repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Release(platform.Repo{Host: "H", ID: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the released repository %s is still there: %v", first.Dir, err)
	}
	if err := src.Release(repo); err != nil {
		t.Errorf("Release again: %v", err)
	}
	if err := src.Release(platform.Repo{Host: "h", ID: "2"}); err != nil {
		t.Errorf("Release of a target without a repository: %v", err)
	}
	// A new repository in the same place, for any remote.
	tree, err := src.Snapshot(t.Context(), repo, remote, "")
	if err != nil || tree.Commit != head {
		t.Fatalf("Snapshot after Release = %+v, %v", tree, err)
	}
	second, err := src.Repo(t.Context(), repo, remote)
	if err != nil || second.Dir != first.Dir {
		t.Errorf("Repo after Release = %v, %v", second, err)
	}
	// Releases racing with Repo calls for the target never make Repo fail
	// (a released repository is not used after its Release: that is the
	// caller's part), and leave the target usable.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				if err := src.Release(repo); err != nil {
					t.Errorf("Release beside Repo: %v", err)
				}
				return
			}
			if _, err := src.Repo(context.Background(), repo, remote); err != nil {
				t.Errorf("Repo beside Release: %v", err)
			}
		}()
	}
	wg.Wait()
	if tree, err := src.Snapshot(t.Context(), repo, remote, ""); err != nil || tree.Commit != head {
		t.Errorf("Snapshot after the races = %+v, %v", tree, err)
	}
}

// TestGitSourceConcurrent takes snapshots of several targets from many
// goroutines, some sharing a target (run it with -race).
func TestGitSourceConcurrent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const targets = 3
	heads := make([]string, targets)
	for i := range heads {
		heads[i] = newServedRepo(t, root, fmt.Sprintf("r%d.git", i)).commit("main", fixtureEntries[:i+1])
	}
	src := newSource(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4*targets)
	for n := 0; n < 4*targets; n++ {
		i := n % targets
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := platform.Repo{Host: "h", ID: fmt.Sprint(i), Path: fmt.Sprintf("o/r%d", i), DefaultBranch: "main"}
			tree, err := src.Snapshot(context.Background(), repo, platform.Remote{URL: filepath.Join(root, fmt.Sprintf("r%d.git", i))}, "")
			switch {
			case err != nil:
				errs <- err
			case tree.Commit != heads[i] || len(tree.Entries) != i+1:
				errs <- fmt.Errorf("target %d: snapshot %s with %d entries", i, tree.Commit, len(tree.Entries))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	dirs, err := os.ReadDir(src.Dir)
	if err != nil || len(dirs) != targets {
		t.Errorf("Dir holds %d repositories, want %d (%v)", len(dirs), targets, err)
	}
}

func basicHeader(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

// Credentials newHTTPServer refuses with a status of their own.
var (
	noAccess    = basicHeader("stranger", "tm-no-access-9x8y") // 403: valid, but not for this repository
	rateLimited = basicHeader("busy", "tm-limited-7w6v")       // 429
)

// newHTTPServer serves the bare repositories under root with `git
// http-backend`: fetches need read or write, pushes need write. Like the
// platforms, it answers 401 to an unknown or missing credential, 403 to a
// known one without the right (read pushing, noAccess), and 429 to
// rateLimited.
func newHTTPServer(t *testing.T, root, read, write string) *httptest.Server {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{
		Path: bin,
		Args: []string{"http-backend"},
		Dir:  root,
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + global, "HOME=" + home},
		Logger: log.New(io.Discard, "", 0),
		Stderr: io.Discard,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		push := strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		switch {
		case got == write, got == read && !push:
			backend.ServeHTTP(w, r)
		case got == rateLimited:
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case got == read, got == noAccess:
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			w.Header().Set("WWW-Authenticate", `Basic realm="touchmark test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
