package apply

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
)

// testEnv isolates git from the machine's config (which may set
// core.autocrlf) and makes commits deterministic.
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

// tree is a throwaway target working tree.
type tree struct {
	t   *testing.T
	dir string
	git *gitx.Git // nil when the tree is not a git work tree
}

// newTree returns an empty directory that is not a git work tree. Every
// test that uses it also checks that no temp file is left behind.
func newTree(t *testing.T) *tree {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { assertNoTemp(t, dir) })
	return &tree{t: t, dir: dir}
}

// newRepo returns an empty git work tree with core.autocrlf set explicitly.
func newRepo(t *testing.T, autocrlf bool) *tree {
	t.Helper()
	tr := newTree(t)
	tr.git = &gitx.Git{Dir: tr.dir, Env: testEnv(t)}
	tr.run("init", "-q", "-b", "master")
	tr.run("config", "core.autocrlf", fmt.Sprint(autocrlf))
	return tr
}

// target returns the Target for the tree.
func (tr *tree) target() Target { return Target{Root: tr.dir, Git: tr.git} }

// run runs git in the tree and fails the test on error.
func (tr *tree) run(args ...string) string {
	tr.t.Helper()
	out, err := tr.git.Run(tr.t.Context(), nil, args...)
	if err != nil {
		tr.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// abs returns the OS path of the repository path p.
func (tr *tree) abs(p string) string { return filepath.Join(tr.dir, filepath.FromSlash(p)) }

// write writes content at p, creating parents, with mode 0o644.
func (tr *tree) write(p, content string) { tr.writeMode(p, content, 0o644) }

// writeMode writes content at p with perm.
func (tr *tree) writeMode(p, content string, perm fs.FileMode) {
	tr.t.Helper()
	full := tr.abs(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), perm); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Chmod(full, perm); err != nil {
		tr.t.Fatal(err)
	}
}

// mkdir creates the directory p and its parents.
func (tr *tree) mkdir(p string) {
	tr.t.Helper()
	if err := os.MkdirAll(tr.abs(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
}

// remove deletes p and everything beneath it.
func (tr *tree) remove(p string) {
	tr.t.Helper()
	if err := os.RemoveAll(tr.abs(p)); err != nil {
		tr.t.Fatal(err)
	}
}

// symlink creates a symlink at p pointing to oldname, and skips the test
// when the system does not allow it (Windows without developer mode).
func (tr *tree) symlink(oldname, p string) {
	tr.t.Helper()
	if err := os.Symlink(filepath.FromSlash(oldname), tr.abs(p)); err != nil {
		tr.t.Skipf("symlinks are not available: %v", err)
	}
}

// read returns the content at p and fails the test when it is not there.
func (tr *tree) read(p string) string {
	tr.t.Helper()
	b, err := os.ReadFile(tr.abs(p))
	if err != nil {
		tr.t.Fatal(err)
	}
	return string(b)
}

// lstat returns what is at p, or nil when nothing is.
func (tr *tree) lstat(p string) fs.FileInfo {
	tr.t.Helper()
	fi, err := os.Lstat(tr.abs(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		tr.t.Fatal(err)
	}
	return fi
}

// observe runs Observe and fails the test on error.
func (tr *tree) observe(paths ...string) map[string]decide.Observation {
	tr.t.Helper()
	obs, err := Observe(tr.t.Context(), tr.target(), paths)
	if err != nil {
		tr.t.Fatalf("Observe: %v", err)
	}
	return obs
}

// execute runs Execute on entries.
func (tr *tree) execute(blobs BlobSource, entries ...decide.Entry) []Result {
	tr.t.Helper()
	return Execute(tr.t.Context(), tr.target(), decide.Plan{Entries: entries}, blobs)
}

// assertNoTemp fails the test if a touchmark temp file is left under dir.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".touchmark-") {
			t.Errorf("temp file left behind: %s", p)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("walk %s: %v", dir, err)
	}
}

// oid is the raw blob id of content.
func oid(content string) string { return gitx.RawOID([]byte(content)) }

// memBlobs is an in-memory BlobSource keyed by raw blob id.
type memBlobs map[string][]byte

// add stores content and returns its id.
func (m memBlobs) add(content string) string {
	id := oid(content)
	m[id] = []byte(content)
	return id
}

func (m memBlobs) Open(id string) (io.ReadCloser, error) {
	b, ok := m[id]
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", id, gitx.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// blobFunc adapts a function to BlobSource.
type blobFunc func(oid string) (io.ReadCloser, error)

func (f blobFunc) Open(oid string) (io.ReadCloser, error) { return f(oid) }

// wantDone fails the test unless every result is Done without error.
func wantDone(t *testing.T, results []Result) {
	t.Helper()
	for _, r := range results {
		if !r.Done || r.Err != nil || r.Skipped != "" {
			t.Errorf("%s %s: done=%v skipped=%q err=%v; want done", r.Entry.Action, r.Entry.Path, r.Done, r.Skipped, r.Err)
		}
	}
}

// wantSkipped fails the test unless r was skipped with a reason containing
// substr.
func wantSkipped(t *testing.T, r Result, substr string) {
	t.Helper()
	if r.Done || r.Err != nil || !strings.Contains(r.Skipped, substr) {
		t.Errorf("%s %s: done=%v skipped=%q err=%v; want skipped with %q",
			r.Entry.Action, r.Entry.Path, r.Done, r.Skipped, r.Err, substr)
	}
}

// wantErr fails the test unless r failed with an error containing substr.
func wantErr(t *testing.T, r Result, substr string) {
	t.Helper()
	if r.Done || r.Skipped != "" || r.Err == nil || !strings.Contains(r.Err.Error(), substr) {
		t.Errorf("%s %s: done=%v skipped=%q err=%v; want error with %q",
			r.Entry.Action, r.Entry.Path, r.Done, r.Skipped, r.Err, substr)
	}
}
