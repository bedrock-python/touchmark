package fake_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// Tokens of the git-mode test accounts.
const (
	readerToken = "reader-token-3f9a"
	writerToken = "writer-token-81c2"
	otherToken  = "other-token-5d07"
)

// gitEnv is env in git mode: a served platform whose three accounts have
// tokens. Its cleanup stops the server and fails the test on violations
// left unread.
type gitEnv struct {
	*env
	srv *fake.GitServer
}

func newGitEnv(t *testing.T, opts ...fake.Option) *gitEnv {
	t.Helper()
	e := newEnv(t, opts...)
	srv, err := e.p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatalf("ServeGit: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if v := e.p.Violations(); len(v) > 0 {
			t.Errorf("unexpected violations: %q", v)
		}
	})
	e.p.SetToken(e.reader, readerToken)
	e.p.SetToken(e.writer, writerToken)
	e.p.SetToken(e.other, otherToken)
	e.ok()
	return &gitEnv{env: e, srv: srv}
}

// push is a person's push of files ("path", "content" pairs) to branch.
func (e *gitEnv) push(r platform.Repo, branch string, by platform.Account, files ...string) string {
	e.t.Helper()
	m := map[string][]byte{}
	for i := 0; i+1 < len(files); i += 2 {
		m[files[i]] = []byte(files[i+1])
	}
	head, err := e.p.PushFiles(r.ID, branch, m, by, time.Time{})
	if err != nil {
		e.t.Fatalf("PushFiles(%s, %s): %v", r.Path, branch, err)
	}
	return head
}

// violations returns the violations and checks they have the kinds want,
// in order.
func (e *gitEnv) violations(want ...string) []string {
	e.t.Helper()
	got := e.p.Violations()
	var kinds []string
	for _, v := range got {
		kind, _, _ := strings.Cut(v, " ")
		kinds = append(kinds, kind)
	}
	if !slices.Equal(kinds, want) {
		e.t.Errorf("violations %q, want kinds %v", got, want)
	}
	return got
}

// isolatedEnv keeps git away from the machine's configuration, makes its
// commits deterministic and keeps it from starting background maintenance,
// which would hold files of a temporary directory open on Windows.
func isolatedEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, []byte("[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	date := "1767225600 +0000"
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + global,
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_AUTHOR_NAME=touchmark test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=touchmark test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE=" + date,
		"GIT_CEILING_DIRECTORIES=" + os.TempDir(),
	}
}

// gitVersion returns the local git's version.
func gitVersion(t *testing.T) [3]int {
	t.Helper()
	v, err := (&gitx.Git{Env: isolatedEnv(t)}).Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// needGit skips the test when git is older than min.
func needGit(t *testing.T, min [3]int, why string) {
	t.Helper()
	if v := gitVersion(t); slices.Compare(v[:], min[:]) < 0 {
		t.Skipf("git %d.%d.%d is older than %d.%d, which %s needs", v[0], v[1], v[2], min[0], min[1], why)
	}
}

// client is a bare repository that talks to the git server the way gitx
// does: blobless fetches, commits built without a work tree, pushes with a
// lease, the credential as an extra header in the environment.
type client struct {
	t      *testing.T
	g      *gitx.Git
	remote platform.Remote
	tick   int
}

func newClient(t *testing.T, remote platform.Remote) *client {
	t.Helper()
	c := &client{t: t, g: &gitx.Git{Dir: t.TempDir(), Env: isolatedEnv(t)}, remote: remote}
	c.run(nil, "init", "-q", "--bare")
	c.run(nil, "remote", "add", "origin", remote.URL)
	return c
}

// try runs git with the remote's credential and returns its output.
func (c *client) try(stdin io.Reader, args ...string) (string, error) {
	g := *c.g
	if c.remote.Header != nil {
		h, err := c.remote.Header(c.t.Context())
		if err != nil {
			return "", err
		}
		g.Env = append(slices.Clone(g.Env), "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: "+h)
	}
	out, err := g.Run(c.t.Context(), stdin, args...)
	return strings.TrimSpace(string(out)), err
}

// run is try that fails the test on error.
func (c *client) run(stdin io.Reader, args ...string) string {
	c.t.Helper()
	out, err := c.try(stdin, args...)
	if err != nil {
		c.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// fetch fetches branch without blobs and returns its tip.
func (c *client) fetch(branch string) (string, error) {
	_, err := c.try(nil, "fetch", "-q", "--filter=blob:none", "--no-tags", "origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	if err != nil {
		return "", err
	}
	return c.try(nil, "rev-parse", "--verify", "refs/remotes/origin/"+branch)
}

// mustFetch is fetch that fails the test on error.
func (c *client) mustFetch(branch string) string {
	c.t.Helper()
	tip, err := c.fetch(branch)
	if err != nil {
		c.t.Fatalf("fetch %s: %v", branch, err)
	}
	return tip
}

// deleted marks a path to delete in commit.
const deleted = "\x00deleted"

// commit writes a commit on top of parent ("" for a root commit) with
// changes ("path", "content" pairs; content deleted removes the path) and
// returns it. Each commit of a client gets a later date.
func (c *client) commit(parent, msg string, changes ...string) string {
	c.t.Helper()
	c.tick++
	var b strings.Builder
	fmt.Fprintf(&b, "commit refs/heads/scratch\nmark :1\ncommitter touchmark test <test@example.com> %d +0000\n", 1767225600+60*c.tick)
	fmt.Fprintf(&b, "data %d\n%s\n", len(msg), msg)
	if parent != "" {
		fmt.Fprintf(&b, "from %s\n", parent)
	}
	for i := 0; i+1 < len(changes); i += 2 {
		if changes[i+1] == deleted {
			fmt.Fprintf(&b, "D %s\n", changes[i])
			continue
		}
		fmt.Fprintf(&b, "M 100644 inline %s\ndata %d\n%s\n", changes[i], len(changes[i+1]), changes[i+1])
	}
	b.WriteString("get-mark :1\n")
	return c.run(strings.NewReader(b.String()), "fast-import", "--quiet", "--force")
}

// push moves branch to commit ("" deletes it) with a lease on expect (""
// when the branch must not exist).
func (c *client) push(commit, branch, expect string) error {
	src := commit + ":refs/heads/" + branch
	if commit == "" {
		src = ":refs/heads/" + branch
	}
	_, err := c.try(nil, "push", "--porcelain", "--atomic", "--no-verify",
		"--force-with-lease=refs/heads/"+branch+":"+expect, "origin", src)
	return err
}

// mustPush is push that fails the test on error.
func (c *client) mustPush(commit, branch, expect string) {
	c.t.Helper()
	if err := c.push(commit, branch, expect); err != nil {
		c.t.Fatalf("push %s to %s: %v", commit, branch, err)
	}
}

// show returns the content of path at rev, fetching the blob when needed.
func (c *client) show(rev, path string) string {
	c.t.Helper()
	return c.run(nil, "cat-file", "-p", rev+":"+path)
}

// httpStatus requests url with method and an optional Authorization value
// and returns the status and the WWW-Authenticate header.
func httpStatus(t *testing.T, method, url, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

// header returns the value of a remote's header, failing the test on error.
func header(t *testing.T, rem platform.Remote) string {
	t.Helper()
	if rem.Header == nil {
		t.Fatalf("remote %s has no header", rem.URL)
	}
	h, err := rem.Header(t.Context())
	if err != nil {
		t.Fatalf("header of %s: %v", rem.URL, err)
	}
	return h
}

// gitIn runs git in a bare repository of the platform and returns its
// output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := (&gitx.Git{Dir: dir, Env: isolatedEnv(t)}).Run(t.Context(), nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// wantErrIs checks errors.Is for a human action.
func wantErrIs(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: %v, want %v", what, err, target)
	}
}
