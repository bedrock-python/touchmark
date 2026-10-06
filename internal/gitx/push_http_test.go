package gitx

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestHTTPPushUpdatesBaseFile: a commit that changes a file the base holds
// is pushed over smart HTTP from the blobless repository, to a new branch
// and over an existing one. git-remote-http always packs thin (it runs
// send-pack --thin whatever `git push --no-thin` says), and pack-objects
// then looks up the base's blob at the changed path as a delta base: a blob
// a blobless repository does not have, and with lazy fetching off it died
// ("could not fetch … from promisor remote"), so every update of a file a
// target already held failed (found by the property test of distribute).
func TestHTTPPushUpdatesBaseFile(t *testing.T) {
	t.Parallel()
	requireGit(t, DeliveryMinVersion, "lazy fetches off (GIT_NO_LAZY_FETCH)")
	root := t.TempDir()
	s := newServed(t, root, "acme/api.git")
	old := strings.Repeat("the shared guide, version 1 of the engineering asset\n", 4)
	b := s.commit("main", nil, []file{{path: "README.md", content: "readme\n"}, {path: "docs/guide.md", content: old}}, "base\n")
	srv := newGitServer(t, root, "")
	tr := newTarget(t, srv.URL+"/acme/api.git", Auth{})
	if head, ok, err := tr.FetchBranch(t.Context(), "main", 1); err != nil || !ok || head != b {
		t.Fatalf("FetchBranch(main) = %s, %v, %v", head, ok, err)
	}
	build := func(n int) Built {
		t.Helper()
		content := strings.Repeat("the shared guide, version "+string(rune('1'+n))+" of the engineering asset\n", 4)
		id := RawOID([]byte(content))
		who := Person{Name: "acme-write[bot]", Email: "1+acme-write[bot]@users.noreply.github.com"}
		built, err := tr.BuildCommit(t.Context(), CommitSpec{
			Parent:  b,
			Changes: []Change{{Path: "docs/guide.md", Mode: "100644", OID: id}},
			Blobs: map[string]Blob{id: {OID: id, Open: func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(content)), nil
			}}},
			Author: who, Committer: who, When: time.Unix(1767225600, 0), Message: "chore: sync\n",
		})
		if err != nil {
			t.Fatalf("BuildCommit: %v", err)
		}
		return built
	}
	first := build(1)
	res, err := tr.Push(t.Context(), PushSpec{Branch: "touchmark/acme", Commit: first.Commit})
	if err != nil || res.Status != PushOK {
		t.Fatalf("Push to a new branch = %+v, %v", res, err)
	}
	second := build(2)
	res, err = tr.Push(t.Context(), PushSpec{Branch: "touchmark/acme", Commit: second.Commit, Expect: first.Commit})
	if err != nil || res.Status != PushOK {
		t.Fatalf("Push over the branch = %+v, %v", res, err)
	}
	if got := s.git("rev-parse", "refs/heads/touchmark/acme"); got != second.Commit {
		t.Errorf("the branch is at %s, want %s", got, second.Commit)
	}
}
