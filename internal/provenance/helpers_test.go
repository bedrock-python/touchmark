package provenance

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

// testEnv isolates git from the machine's config and makes commits
// deterministic.
func testEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
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
		// Plain directories under the temp dir must not resolve to a
		// repository around it (a dotfiles repository in the profile).
		"GIT_CEILING_DIRECTORIES=" + os.TempDir(),
	}
}

// hubRepo is a throwaway hub repository with a work tree.
type hubRepo struct {
	t    *testing.T
	dir  string
	g    *gitx.Git
	tick int
}

// newHubRepo creates a repository on branch master with core.autocrlf set
// explicitly (the machine's system config may set it) and core.protectNTFS
// off, so tests can commit paths Windows cannot check out.
func newHubRepo(t *testing.T, autocrlf bool, initArgs ...string) *hubRepo {
	t.Helper()
	dir := t.TempDir()
	r := &hubRepo{t: t, dir: dir, g: &gitx.Git{Dir: dir, Env: testEnv(t)}}
	r.git(append([]string{"init", "-q", "-b", "master"}, initArgs...)...)
	r.git("config", "core.autocrlf", fmt.Sprint(autocrlf))
	r.git("config", "core.protectNTFS", "false")
	return r
}

// hub returns the Hub for the repository.
func (r *hubRepo) hub() *Hub { return &Hub{Dir: r.dir, Git: r.g} }

// git runs git in the repository and fails the test on error.
func (r *hubRepo) git(args ...string) string {
	r.t.Helper()
	return r.gitIn(nil, args...)
}

// gitIn runs git with stdin and fails the test on error.
func (r *hubRepo) gitIn(stdin []byte, args ...string) string {
	r.t.Helper()
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	out, err := r.g.Run(r.t.Context(), in, args...)
	if err != nil {
		r.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// write writes content to path in the work tree and stages it.
func (r *hubRepo) write(path, content string) {
	r.t.Helper()
	r.writeFile(path, content)
	r.git("add", "--", path)
}

// writeFile writes content to path in the work tree without staging it.
func (r *hubRepo) writeFile(path, content string) {
	r.t.Helper()
	writeTestFile(r.t, filepath.Join(r.dir, filepath.FromSlash(path)), content)
}

// setEntry puts an index entry with any mode, without touching the work
// tree. For mode 160000 content is the commit id; otherwise it is written as
// a blob.
func (r *hubRepo) setEntry(mode, content, path string) {
	r.t.Helper()
	oid := content
	if mode != "160000" {
		oid = r.gitIn([]byte(content), "hash-object", "-w", "--stdin")
	}
	r.git("update-index", "--add", "--cacheinfo", mode+","+oid+","+path)
}

// commit commits the index with a distinct, increasing date and returns the
// new HEAD.
func (r *hubRepo) commit(msg string) string {
	r.t.Helper()
	r.tick++
	date := fmt.Sprintf("%d +0000", 1767225600+60*r.tick)
	g := *r.g
	g.Env = append(slices.Clone(r.g.Env), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if _, err := g.Run(r.t.Context(), nil, "commit", "-q", "--allow-empty", "--no-verify", "-m", msg); err != nil {
		r.t.Fatalf("commit %q: %v", msg, err)
	}
	return r.git("rev-parse", "HEAD")
}

// writeTestFile creates path with its parent directories.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fileURL returns a file:// URL for a local directory, which makes clone
// honour --depth.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	return "file://" + p
}

// blob returns the sha1 blob id of content.
func blob(content string) string { return gitx.RawOID([]byte(content)) }

// ver returns the Version of content.
func ver(content string) Version {
	return Version{OID: blob(content), Size: int64(len(content))}
}

// text returns distinct content of at least MinEvidenceSize bytes.
func text(name string) string {
	return name + ": " + strings.Repeat("shared engineering asset line\n", 3)
}
