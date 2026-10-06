package gitx

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchBranchDepth(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	ids := s.chain("main", "", 5)
	head := ids[4]
	tr := newTarget(t, s.dir, Auth{})

	sha, ok, err := tr.FetchBranch(t.Context(), "main", 2)
	if err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch(main, 2) = %s, %v, %v; want %s", sha, ok, err, head)
	}
	if got, err := tr.resolve(t.Context(), "refs/touchmark/remote/main"); err != nil || got != head {
		t.Errorf("refs/touchmark/remote/main = %s, %v", got, err)
	}
	log, shallow, err := tr.FirstParentLog(t.Context(), sha, 10)
	if err != nil || len(log) != 2 || !shallow || log[0].SHA != ids[4] || log[1].SHA != ids[3] {
		t.Fatalf("FirstParentLog(depth 2) = %v, %v, %v", shas(log), shallow, err)
	}
	// Only commits and trees came: the blobs stay on the server.
	if lazyFetchOff(t) {
		blob := s.blob(head, "a.txt")
		if missing, err := tr.missing(t.Context(), []string{blob}); err != nil || len(missing) != 1 {
			t.Errorf("missing(%s) = %v, %v; want the blob missing", blob, missing, err)
		}
	}

	sha, ok, err = tr.FetchBranch(t.Context(), "main", 10)
	if err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch(main, 10) = %s, %v, %v", sha, ok, err)
	}
	log, shallow, err = tr.FirstParentLog(t.Context(), sha, 10)
	if err != nil || len(log) != 5 || shallow {
		t.Fatalf("FirstParentLog(full) = %v, %v, %v; want 5 commits to the root", shas(log), shallow, err)
	}

	// A missing branch is no error.
	if sha, ok, err := tr.FetchBranch(t.Context(), "gone", 1); err != nil || ok || sha != "" {
		t.Errorf("FetchBranch(gone) = %q, %v, %v", sha, ok, err)
	}
	for _, bad := range []string{"", "-x", "a..b", "a b", "refs/heads/main", "x.lock", "a:b", "@", "a/", ".a", "x@{1}"} {
		if _, _, err := tr.FetchBranch(t.Context(), bad, 1); err == nil {
			t.Errorf("FetchBranch(%q) succeeded", bad)
		}
	}
	if _, _, err := tr.FetchBranch(t.Context(), "main", 0); err == nil {
		t.Error("FetchBranch(depth 0) succeeded")
	}
}

func TestFetchBranchRewritten(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	first := s.chain("sync", "", 2)
	tr := newTarget(t, s.dir, Auth{})
	if sha, _, err := tr.FetchBranch(t.Context(), "sync", 20); err != nil || sha != first[1] {
		t.Fatalf("FetchBranch() = %s, %v", sha, err)
	}
	// Someone force-pushes the branch: the local ref follows.
	other := s.commit("sync", nil, []file{{path: "b.txt", content: "b\n"}}, "rewritten\n")
	if sha, ok, err := tr.FetchBranch(t.Context(), "sync", 20); err != nil || !ok || sha != other {
		t.Errorf("FetchBranch(after force push) = %s, %v, %v; want %s", sha, ok, err, other)
	}
}

func TestRemoteRefs(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	main := s.chain("main", "", 1)[0]
	feature := s.chain("feature", main, 1)[0]
	// A branch whose name ends like another ref: ls-remote's tail match
	// would report it for "refs/heads/main".
	tail := s.commit("x/refs/heads/main", []string{main}, []file{{path: "t.txt", content: "t\n"}}, "tail\n")
	tr := newTarget(t, s.dir, Auth{})

	got, err := tr.RemoteRefs(t.Context(), "refs/heads/main", "refs/heads/feature", "refs/heads/missing")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"refs/heads/main": main, "refs/heads/feature": feature}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("RemoteRefs() = %v, want %v", got, want)
	}
	all, err := tr.RemoteRefs(t.Context())
	if err != nil || len(all) != 3 || all["refs/heads/x/refs/heads/main"] != tail {
		t.Errorf("RemoteRefs(all) = %v, %v", all, err)
	}
	for _, bad := range []string{"main", "refs/heads/a..b", "refs/heads/-x/../y", "refs/heads/a b"} {
		if _, err := tr.RemoteRefs(t.Context(), bad); err == nil {
			t.Errorf("RemoteRefs(%q) succeeded", bad)
		}
	}
}

func TestDeepenSince(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	day := int64(86400)
	base := int64(1767225600)
	var ids []string
	parent := ""
	for i := 1; i <= 6; i++ {
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		parent = s.commitAt("main", parents, []file{{path: "a.txt", content: fmt.Sprintf("v%d\n", i)}},
			fmt.Sprintf("c%d\n", i), fmt.Sprintf("%d +0000", base+int64(i)*day))
		ids = append(ids, parent)
	}
	tr := newTarget(t, s.dir, Auth{})
	head, _, err := tr.FetchBranch(t.Context(), "main", 1)
	if err != nil || head != ids[5] {
		t.Fatalf("FetchBranch() = %s, %v", head, err)
	}
	if lazyFetchOff(t) {
		if ok, err := tr.IsAncestor(t.Context(), ids[2], head); err == nil {
			t.Errorf("IsAncestor(unfetched commit) = %v, want an error", ok)
		}
	}
	// Back to commit 3 (day 3), one hour of margin.
	since := time.Unix(base+3*day-3600, 0)
	if err := tr.DeepenSince(t.Context(), "main", since); err != nil {
		t.Fatal(err)
	}
	log, shallow, err := tr.FirstParentLog(t.Context(), head, 20)
	if err != nil || len(log) != 4 || !shallow || log[3].SHA != ids[2] {
		t.Fatalf("FirstParentLog(after DeepenSince) = %v, %v, %v; want commits 6..3, shallow", shas(log), shallow, err)
	}
	if ok, err := tr.IsAncestor(t.Context(), ids[2], head); err != nil || !ok {
		t.Errorf("IsAncestor(c3, head) = %v, %v", ok, err)
	}
	// No ref moved.
	if got, err := tr.resolve(t.Context(), "refs/touchmark/remote/main"); err != nil || got != head {
		t.Errorf("refs/touchmark/remote/main = %s, %v", got, err)
	}
	if err := tr.DeepenSince(t.Context(), "gone", since); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeepenSince(gone) = %v, want ErrNotFound", err)
	}
	if err := tr.DeepenSince(t.Context(), "main", time.Time{}); err == nil {
		t.Error("DeepenSince(zero time) succeeded")
	}
}

func TestFetchBlobs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.commit("main", nil, []file{
		{path: ".gitattributes", content: "*.bin -text\n"},
		{path: "docs/.gitattributes", content: "*.md text eol=lf\n"},
		{path: "big.bin", content: strings.Repeat("x", 5000)},
	}, "one\n")
	attrs := s.blob(head, ".gitattributes")
	docs := s.blob(head, "docs/.gitattributes")
	srv := newGitServer(t, root, "")
	for name, remote := range map[string]string{"file": s.dir, "http": srv.URL + "/repo.git"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := newTarget(t, remote, Auth{})
			if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
				t.Fatal(err)
			}
			var fetches atomic.Int32
			tr.iso.trace = func(args, _ []string) {
				if slices.Contains(args, "fetch") && slices.Contains(args, "--stdin") {
					fetches.Add(1)
				}
			}
			if err := tr.FetchBlobs(t.Context(), []string{attrs, docs, attrs}); err != nil {
				t.Fatal(err)
			}
			tr.iso.trace = nil
			// FetchBlobs fetched the blobs itself: an older git fetches them
			// lazily when FetchBlobs asks which are missing, and FetchBlobs'
			// own fetch is not exercised there.
			if lazyFetchOff(t) && fetches.Load() != 1 {
				t.Errorf("FetchBlobs ran %d fetches, want 1", fetches.Load())
			}
			for id, want := range map[string]string{attrs: "*.bin -text\n", docs: "*.md text eol=lf\n"} {
				if got, err := tr.ReadBlob(t.Context(), id); err != nil || string(got) != want {
					t.Errorf("ReadBlob(%s) = %q, %v", id, got, err)
				}
			}
			// Only the blobs asked for came.
			if lazyFetchOff(t) {
				big := s.blob(head, "big.bin")
				if missing, err := tr.missing(t.Context(), []string{big}); err != nil || len(missing) != 1 {
					t.Errorf("big.bin present after FetchBlobs: %v, %v", missing, err)
				}
			}
			// Present ids are not fetched again; nothing to do is no error.
			if err := tr.FetchBlobs(t.Context(), []string{attrs}); err != nil {
				t.Errorf("FetchBlobs(present) = %v", err)
			}
			if err := tr.FetchBlobs(t.Context(), nil); err != nil {
				t.Errorf("FetchBlobs(nil) = %v", err)
			}
			if err := tr.FetchBlobs(t.Context(), []string{"not-an-id"}); err == nil {
				t.Error("FetchBlobs(invalid id) succeeded")
			}
			if err := tr.FetchBlobs(t.Context(), []string{missingOID}); err == nil {
				t.Error("FetchBlobs(unknown id) succeeded")
			}
		})
	}
}

// TestReadBlobNeverFetches: a blob that is not present is ErrNotFound,
// never a lazy fetch (GIT_NO_LAZY_FETCH, git 2.45).
func TestReadBlobNeverFetches(t *testing.T) {
	t.Parallel()
	requireGit(t, DeliveryMinVersion, "GIT_NO_LAZY_FETCH")
	s := newServed(t, t.TempDir(), "repo.git")
	head := s.commit("main", nil, []file{{path: "a.txt", content: "a\n"}, {path: "dir/b.txt", content: "b\n"}}, "one\n")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	blob := s.blob(head, "a.txt")
	for i := 0; i < 2; i++ {
		if _, err := tr.ReadBlob(t.Context(), blob); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ReadBlob(missing) = %v, want ErrNotFound", err)
		}
	}
	tree := s.git("rev-parse", head+"^{tree}")
	if _, err := tr.ReadBlob(t.Context(), tree); err == nil || !strings.Contains(err.Error(), "tree") {
		t.Errorf("ReadBlob(tree) = %v", err)
	}
	if _, err := tr.ReadBlob(t.Context(), "HEAD"); err == nil {
		t.Error("ReadBlob(HEAD) succeeded")
	}
}

// TestObjectLimits: a commit or blob a target's writer made huge is refused
// by its size, before anything of it is read into memory.
func TestObjectLimits(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	big := strings.Repeat("x", MaxReadBlob+1)
	head := s.commit("main", nil, []file{{path: "big.bin", content: big}, {path: "small.txt", content: "s\n"}}, "base\n")
	huge := s.commit("main", []string{head}, []file{{path: "a.txt", content: "a\n"}}, "huge\n\n"+strings.Repeat("y", MaxCommitObject))
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.FirstParentLog(t.Context(), huge, 5); !errors.Is(err, ErrTooLarge) {
		t.Errorf("FirstParentLog(huge commit) = %v, want ErrTooLarge", err)
	}
	if _, err := tr.Commit(t.Context(), "refs/touchmark/remote/main"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Commit(huge) = %v, want ErrTooLarge", err)
	}
	if c, err := tr.Commit(t.Context(), head); err != nil || c.SHA != head {
		t.Errorf("Commit(head) = %+v, %v", c, err)
	}
	blob, small := s.blob(head, "big.bin"), s.blob(head, "small.txt")
	if err := tr.FetchBlobs(t.Context(), []string{blob, small}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.ReadBlob(t.Context(), blob); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadBlob(%d bytes) = %v, want ErrTooLarge", len(big), err)
	}
	if got, err := tr.ReadBlob(t.Context(), small); err != nil || string(got) != "s\n" {
		t.Errorf("ReadBlob(small) = %q, %v", got, err)
	}
}

func shas(log []CommitInfo) []string {
	var out []string
	for _, c := range log {
		out = append(out, c.SHA[:8])
	}
	return out
}
