package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

// testRepo is a throwaway repository with a work tree.
type testRepo struct {
	t    *testing.T
	dir  string
	g    *Git
	tick int
}

// newTestRepo creates a repository on branch master with core.autocrlf set
// explicitly (the machine's system config may set it).
func newTestRepo(t *testing.T, autocrlf bool, initArgs ...string) *testRepo {
	t.Helper()
	dir := t.TempDir()
	r := &testRepo{t: t, dir: dir, g: &Git{Dir: dir, Env: testEnv(t)}}
	r.git(append([]string{"init", "-q", "-b", "master"}, initArgs...)...)
	r.git("config", "core.autocrlf", fmt.Sprint(autocrlf))
	return r
}

// git runs git in the repository and fails the test on error.
func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	return r.gitIn(nil, args...)
}

// gitIn runs git with stdin and fails the test on error.
func (r *testRepo) gitIn(stdin []byte, args ...string) string {
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

// tryGit runs git and returns its error instead of failing the test.
func (r *testRepo) tryGit(args ...string) error {
	_, err := r.g.Run(r.t.Context(), nil, args...)
	return err
}

// write writes content to path in the work tree and stages it.
func (r *testRepo) write(path, content string) {
	r.t.Helper()
	r.writeFile(path, []byte(content))
	r.git("add", "--", path)
}

// writeFile writes content to path in the work tree without staging it.
func (r *testRepo) writeFile(path string, content []byte) {
	r.t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// remove deletes path from the index and the work tree.
func (r *testRepo) remove(path string) {
	r.t.Helper()
	r.git("rm", "-q", "--", path)
}

// hashObject writes content as a blob and returns its id.
func (r *testRepo) hashObject(content []byte) string {
	r.t.Helper()
	return r.gitIn(content, "hash-object", "-w", "--stdin")
}

// setEntry puts an index entry with any mode, without touching the work
// tree. For mode 160000 content is the commit id; otherwise it is written as
// a blob.
func (r *testRepo) setEntry(mode, content, path string) string {
	r.t.Helper()
	oid := content
	if mode != "160000" {
		oid = r.hashObject([]byte(content))
	}
	r.git("update-index", "--add", "--cacheinfo", mode+","+oid+","+path)
	return oid
}

// commit commits the index with a distinct, deterministic date and returns
// the new HEAD.
func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.commitIndex(msg)
	return r.git("rev-parse", "HEAD")
}

// commitIndex commits the index with a distinct, deterministic date.
func (r *testRepo) commitIndex(msg string) {
	r.t.Helper()
	r.tick++
	date := fmt.Sprintf("%d +0000", 1767225600+60*r.tick)
	g := *r.g
	g.Env = append(slices.Clone(r.g.Env), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if _, err := g.Run(r.t.Context(), nil, "commit", "-q", "--allow-empty", "--no-verify", "-m", msg); err != nil {
		r.t.Fatalf("commit %q: %v", msg, err)
	}
}

// startMerge merges branch without committing and without rename detection,
// and returns the paths left in conflict.
func (r *testRepo) startMerge(branch string, extra ...string) []string {
	r.t.Helper()
	args := append([]string{"-c", "merge.renames=false", "-c", "diff.renames=false",
		"merge", "-q", "--no-ff", "--no-commit"}, extra...)
	err := r.tryGit(append(args, branch)...)
	conflicts := r.git("diff", "--name-only", "-z", "--diff-filter=U")
	var paths []string
	for p := range strings.SplitSeq(conflicts, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	var gitErr *Error
	if err != nil && (!errors.As(err, &gitErr) || len(paths) == 0) {
		r.t.Fatalf("merge %s: %v", branch, err)
	}
	return paths
}

// allTreeBlobs is the reference for History: every regular blob in the tree
// of any commit listed by `rev-list --all`, read with plain `ls-tree -r` and
// parsed independently of the code under test.
func (r *testRepo) allTreeBlobs() map[BlobVersion]bool {
	r.t.Helper()
	all := map[BlobVersion]bool{}
	for _, c := range strings.Fields(r.git("rev-list", "--all")) {
		out := r.git("ls-tree", "-r", "-z", "--full-tree", c)
		for rec := range strings.SplitSeq(out, "\x00") {
			if rec == "" {
				continue
			}
			meta, path, _ := strings.Cut(rec, "\t")
			f := strings.Fields(meta)
			if f[0] == "100644" || f[0] == "100755" {
				all[BlobVersion{Path: path, OID: f[2]}] = true
			}
		}
	}
	return all
}

// treeBlobs is allTreeBlobs at or under prefix.
func (r *testRepo) treeBlobs(prefix string) map[BlobVersion]bool {
	r.t.Helper()
	return underPrefix(r.allTreeBlobs(), prefix)
}

// underPrefix keeps the versions at or under prefix ("" keeps all).
func underPrefix(all map[BlobVersion]bool, prefix string) map[BlobVersion]bool {
	out := map[BlobVersion]bool{}
	for v := range all {
		if prefix == "" || v.Path == prefix || strings.HasPrefix(v.Path, prefix+"/") {
			out[v] = true
		}
	}
	return out
}

// history collects History into a set.
func (r *testRepo) history(rev, prefix string) map[BlobVersion]bool {
	r.t.Helper()
	got := map[BlobVersion]bool{}
	err := r.g.History(r.t.Context(), rev, prefix, func(v BlobVersion) error {
		got[v] = true
		return nil
	})
	if err != nil {
		r.t.Fatalf("History(%q, %q): %v", rev, prefix, err)
	}
	return got
}

// diffSets reports the difference between two History sets.
func diffSets(t *testing.T, got, want map[BlobVersion]bool) {
	t.Helper()
	for v := range want {
		if !got[v] {
			t.Errorf("missing %s %s", v.Path, v.OID)
		}
	}
	for v := range got {
		if !want[v] {
			t.Errorf("unexpected %s %s", v.Path, v.OID)
		}
	}
}
