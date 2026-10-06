package gitx

import (
	"fmt"
	"strings"
	"testing"
)

// A target serves objects git would refuse in a checkout: a tree with an
// entry .GIT (hasDotgit, the path of CVE-2014-9390 and its relatives) and
// a commit whose message holds a NUL byte. transfer.fsckObjects, which
// every command in a target repository runs with, makes the fetch fail,
// and nothing of the branch is kept (a malicious target; see
// docs/project/threat-model.md).
func TestFetchRefusesMalformedObjects(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	good := s.commit("main", nil, []file{{path: "ok.txt", content: "ok\n"}}, "base\n")
	blob := s.blob(good, "ok.txt")
	dotGit := s.gitIn(fmt.Sprintf("100644 blob %s\t.GIT\n100644 blob %s\tok.txt\n", blob, blob), "mktree")
	evilTree := s.git("commit-tree", "-p", good, "-m", "a tree with .GIT", dotGit)
	s.git("update-ref", "refs/heads/dotgit", evilTree)
	tree := s.git("rev-parse", good+"^{tree}")
	raw := fmt.Sprintf("tree %s\nparent %s\nauthor A <a@example.com> 1767225600 +0000\ncommitter A <a@example.com> 1767225600 +0000\n\nnul\x00inside\n", tree, good)
	nul := s.gitIn(raw, "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
	s.git("update-ref", "refs/heads/nul-byte", nul)

	for branch, why := range map[string]string{"dotgit": "hasDotgit", "nul-byte": "nulInCommit"} {
		tr := newTarget(t, s.dir, Auth{})
		if sha, ok, err := tr.FetchBranch(t.Context(), "main", 1); err != nil || !ok || sha != good {
			t.Fatalf("FetchBranch(main) = %s, %v, %v", sha, ok, err)
		}
		sha, ok, err := tr.FetchBranch(t.Context(), branch, 1)
		if err == nil {
			t.Errorf("FetchBranch(%s) = %s, %v: the malformed object was taken", branch, sha, ok)
			continue
		}
		if !strings.Contains(err.Error(), why) && !strings.Contains(err.Error(), "fsck") {
			t.Errorf("FetchBranch(%s): %v, want a refusal by fsck (%s)", branch, err, why)
		}
		for _, id := range []string{evilTree, nul} {
			if _, err := tr.Commit(t.Context(), id); err == nil {
				t.Errorf("%s: the target repository holds commit %s", branch, id)
			}
		}
	}
}
