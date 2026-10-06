package fake_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// The gits a content merge of a human action needs: merges, and the
// commits a rebase replays.
var (
	contentMerge  = [3]int{2, 38, 0}
	contentReplay = [3]int{2, 40, 0}
)

// commitLine formats a commit of a bare repository: "<parents>|<author>
// <email> <date>|<committer>|<subject>".
func commitLine(t *testing.T, dir, rev string) string {
	t.Helper()
	return gitIn(t, dir, "log", "-1", "--format=%P|%an <%ae> %at|%cn|%s", rev)
}

func TestGitPushFiles(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n", "old.md", "old\n")
	dir := e.p.GitDir(r.ID)
	main := e.p.Head(r.ID)
	when := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	h1, err := e.p.PushFiles(r.ID, "feature", map[string][]byte{"a.md": []byte("a\n"), "old.md": nil, "empty.md": {}}, e.other, when)
	must(t, err)
	want := fmt.Sprintf("%s|jdoe <%s> %d|jdoe|Update a.md and 2 more", main, e.other.Email, when.Unix())
	if got := commitLine(t, dir, h1); got != want {
		t.Errorf("commit %q, want %q", got, want)
	}
	tree, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "feature")
	must(t, err)
	if got := sortedKeys(tree.Entries); !slices.Equal(got, []string{"README.md", "a.md", "empty.md"}) {
		t.Errorf("feature holds %v", got)
	}
	if e.p.Branch(r.ID, "feature") != h1 || e.p.Head(r.ID) != main {
		t.Error("PushFiles moved the wrong branch")
	}
	h2, err := e.p.PushFiles(r.ID, "feature", map[string][]byte{"b.md": []byte("b\n")}, e.other, time.Time{})
	must(t, err)
	if got := gitIn(t, dir, "rev-parse", h2+"^"); got != h1 {
		t.Errorf("the second push's parent is %s, want %s", got, h1)
	}
	// On the default branch the tree the API serves follows.
	_, err = e.p.PushFiles(r.ID, "main", map[string][]byte{"README.md": []byte("new\n")}, e.other, time.Time{})
	must(t, err)
	if f, err := e.p.Reader(e.reader).ReadFile(ctx, r, "", "README.md", 100); err != nil || string(f.Content) != "new\n" {
		t.Errorf("README.md after a push to main: %q, %v", f.Content, err)
	}
	e.p.SetFile(r.ID, "README.md/x", []byte("x"), "")
	wantSetupErr(t, e.p, "lies under")

	// An empty repository gets a root commit.
	q := newGitEnv(t)
	empty := q.repo("acme/empty")
	root, err := q.p.PushFiles(empty.ID, "main", map[string][]byte{"a.md": []byte("a\n")}, q.other, time.Time{})
	must(t, err)
	if got := commitLine(t, q.p.GitDir(empty.ID), root); !strings.HasPrefix(got, "|") {
		t.Errorf("root commit %q has parents", got)
	}
	if got, _ := q.p.RepoByID(empty.ID); got.Empty || q.p.Head(empty.ID) != root {
		t.Errorf("after the first push: empty %v, head %q", got.Empty, q.p.Head(empty.ID))
	}

	for name, fn := range map[string]func() error{
		"bad branch": func() error {
			_, err := q.p.PushFiles(empty.ID, "a..b", nil, q.other, time.Time{})
			return err
		},
		"bad path": func() error {
			_, err := q.p.PushFiles(empty.ID, "x", map[string][]byte{"../a": nil}, q.other, time.Time{})
			return err
		},
		"unknown account": func() error {
			_, err := q.p.PushFiles(empty.ID, "x", nil, platform.Account{ID: "404"}, time.Time{})
			return err
		},
		"unknown repository": func() error {
			_, err := q.p.PushFiles("404", "x", nil, q.other, time.Time{})
			return err
		},
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// stackedPR pushes two commits to branch as the person and opens a PR
// from it, titled title; it returns the PR's number.
func stackedPR(e *gitEnv, r platform.Repo, branch, title string) int64 {
	e.t.Helper()
	e.push(r, branch, e.other, branch+"/1.md", "1\n")
	e.push(r, branch, e.other, branch+"/2.md", "2\n")
	return e.pr(r, platform.PR{Head: branch, Author: e.other, Title: title})
}

func TestGitMergePR(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	dir := e.p.GitDir(r.ID)
	when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// A merge commit.
	n := stackedPR(e, r, "merge-me", "Merge me")
	base, head := e.p.Head(r.ID), e.p.Branch(r.ID, "merge-me")
	tip, err := e.p.MergePR(r.ID, n, fake.MergeCommit, e.reader, when)
	must(t, err)
	want := fmt.Sprintf("%s %s|%s <%s> %d|%s|Merge pull request #%d from merge-me", base, head,
		e.reader.Login, e.reader.Email, when.Unix(), e.reader.Login, n)
	if got := commitLine(t, dir, tip); got != want {
		t.Errorf("merge commit %q, want %q", got, want)
	}
	if gitIn(t, dir, "rev-parse", tip+"^{tree}") != gitIn(t, dir, "rev-parse", head+"^{tree}") {
		t.Error("the merge commit does not have the head's tree")
	}
	pr := e.p.PR(r.ID, n)
	if pr.State != platform.Merged || pr.ClosedBy == nil || pr.ClosedBy.ID != e.reader.ID || !pr.ClosedAt.Equal(when) || pr.HeadSHA != head {
		t.Errorf("merged PR: %+v", pr)
	}
	if e.p.Head(r.ID) != tip {
		t.Error("the base did not move")
	}
	if f, err := e.p.Reader(e.reader).ReadFile(ctx, r, "", "merge-me/2.md", 10); err != nil || string(f.Content) != "2\n" {
		t.Errorf("a merged file: %q, %v", f.Content, err)
	}
	if _, err := e.p.MergePR(r.ID, n, fake.MergeCommit, e.reader, when); err == nil {
		t.Error("a merged PR merged again")
	}

	// A squash.
	n = stackedPR(e, r, "squash-me", "Squash me")
	base, head = e.p.Head(r.ID), e.p.Branch(r.ID, "squash-me")
	tip, err = e.p.MergePR(r.ID, n, fake.MergeSquash, e.reader, time.Time{})
	must(t, err)
	if got := commitLine(t, dir, tip); !strings.HasPrefix(got, base+"|") || !strings.HasSuffix(got, fmt.Sprintf("|Squash me (#%d)", n)) {
		t.Errorf("squash commit %q", got)
	}
	if gitIn(t, dir, "rev-parse", tip+"^{tree}") != gitIn(t, dir, "rev-parse", head+"^{tree}") {
		t.Error("the squash commit does not have the head's tree")
	}

	// A rebase keeps authors and messages, with the merger as committer.
	n = stackedPR(e, r, "rebase-me", "Rebase me")
	base, head = e.p.Head(r.ID), e.p.Branch(r.ID, "rebase-me")
	tip, err = e.p.MergePR(r.ID, n, fake.MergeRebase, e.reader, time.Time{})
	must(t, err)
	got := strings.Split(gitIn(t, dir, "log", "--format=%an|%cn|%s", base+".."+tip), "\n")
	if !slices.Equal(got, []string{"jdoe|" + e.reader.Login + "|Update rebase-me/2.md", "jdoe|" + e.reader.Login + "|Update rebase-me/1.md"}) {
		t.Errorf("rebased commits %q", got)
	}
	if gitIn(t, dir, "rev-parse", tip+"^{tree}") != gitIn(t, dir, "rev-parse", head+"^{tree}") || tip == head {
		t.Error("the rebase lost the head's tree or kept its commits")
	}

	// Squash and rebase need a head based on the base's tip.
	n = stackedPR(e, r, "stale", "Stale")
	e.push(r, "main", e.other, "moved.md", "m\n")
	for _, how := range []fake.MergeHow{fake.MergeSquash, fake.MergeRebase} {
		if _, err := e.p.MergePR(r.ID, n, how, e.reader, time.Time{}); err == nil {
			t.Errorf("merge %d of a stale head succeeded", how)
		}
	}
	// A merge commit merges path by path on any git.
	tip, err = e.p.MergePR(r.ID, n, fake.MergeCommit, e.reader, time.Time{})
	must(t, err)
	tree, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, tip)
	must(t, err)
	for _, path := range []string{"moved.md", "stale/1.md", "stale/2.md"} {
		if _, ok := tree.Entries[path]; !ok {
			t.Errorf("the merge lost %s", path)
		}
	}

	// Refusals.
	fork := e.p.AddRepo(platform.Repo{Path: "jdoe/api", Fork: true})
	e.push(fork, "f", e.other, "f.md", "f\n")
	fromFork := e.pr(r, platform.PR{Head: "f", HeadRepoID: fork.ID, Author: e.other})
	noBranch := e.pr(r, platform.PR{Head: "nowhere", Author: e.other})
	for name, tc := range map[string]struct {
		n   int64
		how fake.MergeHow
	}{
		"missing":    {999, fake.MergeCommit},
		"from fork":  {fromFork, fake.MergeCommit},
		"no branch":  {noBranch, fake.MergeCommit},
		"bad method": {stackedPR(e, r, "odd", "Odd"), fake.MergeHow(9)},
	} {
		if _, err := e.p.MergePR(r.ID, tc.n, tc.how, e.reader, time.Time{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestGitContentMerge: a path changed on both sides needs git merge-tree;
// the result is a content merge or a conflict.
func TestGitContentMerge(t *testing.T) {
	t.Parallel()
	e := newGitEnv(t)
	lines := "1\n2\n3\n4\n5\n6\n7\n8\n"
	r := e.repo("acme/api", "file.txt", lines)
	e.push(r, "feature", e.other, "file.txt", "1 feature\n2\n3\n4\n5\n6\n7\n8\n")
	e.push(r, "conflict", e.other, "file.txt", "1 conflict\n2\n3\n4\n5\n6\n7\n8\n")
	clean := e.pr(r, platform.PR{Head: "feature", Author: e.other})
	conflict := e.pr(r, platform.PR{Head: "conflict", Author: e.other})
	e.push(r, "main", e.other, "file.txt", "1\n2\n3\n4\n5\n6\n7\n8 main\n")

	v := gitVersion(t)
	merges, replays := slices.Compare(v[:], contentMerge[:]) >= 0, slices.Compare(v[:], contentReplay[:]) >= 0
	_, err := e.p.UpdateBranch(r.ID, clean, e.other, time.Time{})
	if !merges {
		wantErrIs(t, "a content merge on an old git", err, fake.ErrGitTooOld)
		_, err = e.p.MergePR(r.ID, conflict, fake.MergeCommit, e.other, time.Time{})
		wantErrIs(t, "a conflict on an old git", err, fake.ErrGitTooOld)
		_, err = e.p.RebaseBranch(r.ID, conflict, e.other, time.Time{})
		wantErrIs(t, "a rebase on an old git", err, fake.ErrGitTooOld)
		t.Skipf("git %d.%d.%d merges no contents", v[0], v[1], v[2])
	}
	must(t, err)
	f, err := e.p.Reader(e.reader).ReadFile(t.Context(), r, "feature", "file.txt", 100)
	if err != nil || string(f.Content) != "1 feature\n2\n3\n4\n5\n6\n7\n8 main\n" {
		t.Errorf("the content merge gave %q, %v", f.Content, err)
	}
	_, err = e.p.RebaseBranch(r.ID, conflict, e.other, time.Time{})
	if !replays {
		wantErrIs(t, "a rebase on git before 2.40", err, fake.ErrGitTooOld)
		t.Skipf("git %d.%d.%d replays no content merges", v[0], v[1], v[2])
	}
	must(t, err)
	e.push(r, "main", e.other, "file.txt", "1 main\n2\n3\n4\n5\n6\n7\n8 main\n")
	_, err = e.p.MergePR(r.ID, conflict, fake.MergeCommit, e.other, time.Time{})
	wantErrIs(t, "a conflict", err, fake.ErrMergeConflict)
	_, err = e.p.RebaseBranch(r.ID, conflict, e.other, time.Time{})
	wantErrIs(t, "a conflicting rebase", err, fake.ErrMergeConflict)
	if e.p.PR(r.ID, conflict).State != platform.Open {
		t.Error("a failed merge changed the PR")
	}
}

func TestGitUpdateBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	dir := e.p.GitDir(r.ID)
	n := stackedPR(e, r, "feature", "Feature")
	_, err := e.p.UpdateBranch(r.ID, n, e.other, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "up to date") {
		t.Errorf("UpdateBranch of an up-to-date head: %v", err)
	}
	head := e.p.Branch(r.ID, "feature")
	base := e.push(r, "main", e.other, "m.md", "m\n")
	tip, err := e.p.UpdateBranch(r.ID, n, e.writer, time.Time{})
	must(t, err)
	if got := commitLine(t, dir, tip); !strings.HasPrefix(got, head+" "+base+"|") || !strings.HasSuffix(got, "|Merge branch 'main' into feature") {
		t.Errorf("update commit %q", got)
	}
	if pr := e.p.PR(r.ID, n); pr.HeadSHA != tip || pr.State != platform.Open {
		t.Errorf("after Update branch: %+v", pr)
	}
	tree, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "feature")
	must(t, err)
	for _, path := range []string{"m.md", "feature/1.md", "feature/2.md"} {
		if _, ok := tree.Entries[path]; !ok {
			t.Errorf("the update lost %s", path)
		}
	}
	e.violations()
}

func TestGitRebaseBranch(t *testing.T) {
	t.Parallel()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	dir := e.p.GitDir(r.ID)
	n := stackedPR(e, r, "feature", "Feature")
	if _, err := e.p.RebaseBranch(r.ID, n, e.other, time.Time{}); err == nil {
		t.Error("RebaseBranch of an up-to-date head succeeded")
	}
	e.push(r, "main", e.other, "m.md", "m\n")
	// A merge of the base in the branch is dropped, as git rebase does.
	if _, err := e.p.UpdateBranch(r.ID, n, e.other, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.push(r, "feature", e.other, "feature/3.md", "3\n")
	base := e.push(r, "main", e.other, "n.md", "n\n")
	tip, err := e.p.RebaseBranch(r.ID, n, e.writer, time.Time{})
	must(t, err)
	got := strings.Split(gitIn(t, dir, "log", "--format=%P|%an|%cn|%s", base+".."+tip), "\n")
	if len(got) != 3 || !strings.HasPrefix(got[2], base+"|") {
		t.Fatalf("rebased history %q, want 3 commits on %s", got, base)
	}
	for i, subject := range []string{"Update feature/3.md", "Update feature/2.md", "Update feature/1.md"} {
		if parts := strings.Split(got[i], "|"); parts[1] != "jdoe" || parts[2] != e.writer.Login || parts[3] != subject {
			t.Errorf("commit %d: %q, want jdoe's %q committed by %s", i, got[i], subject, e.writer.Login)
		}
	}
	if e.p.PR(r.ID, n).HeadSHA != tip {
		t.Error("the PR does not follow the rebase")
	}
	// A commit the base already has becomes empty and is dropped.
	m := stackedPR(e, r, "dup", "Dup")
	e.push(r, "main", e.other, "dup/1.md", "1\n")
	tip, err = e.p.RebaseBranch(r.ID, m, e.other, time.Time{})
	must(t, err)
	if got := gitIn(t, dir, "rev-list", "--count", e.p.Head(r.ID)+".."+tip); got != "1" {
		t.Errorf("%s commits after the rebase, want 1", got)
	}
	e.violations()
}

// TestGitQuickActions: in git mode GitLab runs quick actions instead of
// refusing them, and the fake records each.
func TestGitQuickActions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t, fake.WithFlavor(fake.GitLab))
	r := e.repo("acme/api", "README.md", "hello\n")
	tw := e.target(r)
	e.push(r, "touchmark/hub", e.writer, "a.md", "a\n")
	e.push(r, "touchmark/two", e.writer, "b.md", "b\n")
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync",
		Body: "text\n  /label ~bug ~\"ci\"\nmore"})
	must(t, err)
	if !slices.Contains(pr.Labels, "bug") || !slices.Contains(pr.Labels, "ci") || pr.State != platform.Open {
		t.Errorf("after /label: %+v", pr)
	}
	e.violations("quick-action")
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{Body: ptr("/close")})
	must(t, err)
	if got := e.p.PR(r.ID, pr.Number); got.State != platform.Closed || got.ClosedBy == nil || got.ClosedBy.ID != e.writer.ID {
		t.Errorf("after /close: %s by %v", got.State, got.ClosedBy)
	}
	e.violations("quick-action")
	two, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/two", Base: "main", Title: "two"})
	must(t, err)
	must(t, tw.Comment(ctx, two.Number, "please\n/merge\n/approve"))
	if got := e.p.PR(r.ID, two.Number); got.State != platform.Merged {
		t.Errorf("after /merge: %s", got.State)
	}
	e.violations("quick-action", "quick-action")

	// GitHub has no quick actions.
	g := newGitEnv(t)
	gr := g.repo("acme/api", "README.md", "hello\n")
	g.push(gr, "touchmark/hub", g.writer, "a.md", "a\n")
	gpr, err := g.target(gr).CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Body: "/close"})
	if err != nil || gpr.State != platform.Open {
		t.Errorf("GitHub: %+v, %v", gpr, err)
	}
}

// TestGitFaults: pushes and fetches take injected faults; an applied one
// moves the branch and loses the answer.
func TestGitFaults(t *testing.T) {
	t.Parallel()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	tw := e.target(r)
	w := newClient(t, tw.Remote())
	base := w.mustFetch("main")
	c1 := w.commit(base, "one", "a.md", "a\n")
	e.p.ResetCalls()
	e.p.FailNext("Push", &platform.Error{Class: platform.ClassTransient, Status: 503})
	if err := w.push(c1, "touchmark/hub", ""); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("a push with a fault: %v", err)
	}
	if e.p.Branch(r.ID, "touchmark/hub") != "" {
		t.Error("a failed push moved the branch")
	}
	e.p.FailNextApplied("Push", &platform.Error{Class: platform.ClassTransient, Status: 502})
	if err := w.push(c1, "touchmark/hub", ""); err == nil {
		t.Error("a push whose answer was lost succeeded")
	}
	if e.p.Branch(r.ID, "touchmark/hub") != c1 {
		t.Error("an applied push fault did not move the branch")
	}
	w.mustPush(c1, "touchmark/hub", c1) // the retry finds it done
	calls := e.p.Calls()
	for _, want := range []string{"Push acme/api", "Push acme/api touchmark/hub"} {
		if !slices.Contains(calls, want) {
			t.Errorf("Calls %q lack %q", calls, want)
		}
	}

	e.p.FailNext("Fetch", errors.New("boom"))
	if _, err := w.fetch("main"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a fetch with a fault: %v", err)
	}
	if _, err := w.fetch("main"); err != nil {
		t.Errorf("the next fetch: %v", err)
	}
	if !slices.Contains(e.p.Calls(), "Fetch acme/api") {
		t.Errorf("Calls %q lack the fetch", e.p.Calls())
	}
}

// TestGitRenameDefault: a new default branch takes over the old one's
// commits.
func TestGitRenameDefault(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t)
	r := e.repo("acme/api", "README.md", "hello\n")
	tip := e.p.Head(r.ID)
	n := e.pr(r, platform.PR{Head: "feature", Author: e.other})
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.DefaultBranch = "trunk" })
	e.ok()
	if e.p.Branch(r.ID, "trunk") != tip || e.p.Branch(r.ID, "main") != "" || e.p.Head(r.ID) != tip {
		t.Errorf("after the rename: trunk %q, main %q, head %q", e.p.Branch(r.ID, "trunk"), e.p.Branch(r.ID, "main"), e.p.Head(r.ID))
	}
	if e.p.PR(r.ID, n).BaseExists {
		t.Error("a PR based on the old default branch still has its base")
	}
	if got := gitIn(t, e.p.GitDir(r.ID), "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Errorf("HEAD is %s", got)
	}
	if f, err := e.p.Reader(e.reader).ReadFile(ctx, r, "", "README.md", 100); err != nil || string(f.Content) != "hello\n" {
		t.Errorf("ReadFile after the rename: %q, %v", f.Content, err)
	}
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.DefaultBranch = "bad name" })
	wantSetupErr(t, e.p, "not a valid branch name")
}

// TestGitConcurrentUse drives the server from many goroutines; run with
// -race.
func TestGitConcurrentUse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newGitEnv(t, fake.WithFlavor(fake.Gitea))
	var repos []platform.Repo
	for i := range 3 {
		repos = append(repos, e.repo(fmt.Sprintf("acme/r%d", i), "README.md", "x\n"))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for _, r := range repos {
		// Clients and commits are made here: only the test's goroutine may
		// fail the test.
		tw := e.target(r)
		w := newClient(t, tw.Remote())
		tip := w.mustFetch("main")
		var commits []string
		for i := range 3 {
			commits = append(commits, w.commit(tip, fmt.Sprintf("sync %d", i), "sync.md", fmt.Sprint(i)))
		}
		wg.Add(3)
		go func() {
			defer wg.Done()
			prev := ""
			for i, c := range commits {
				if err := w.push(c, "touchmark/hub", prev); err != nil {
					errs <- err
					return
				}
				prev = c
				if i == 0 {
					if _, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"}); err != nil {
						errs <- err
					}
				}
			}
		}()
		go func() {
			defer wg.Done()
			rd := e.p.Reader(e.reader)
			for range 5 {
				if _, err := rd.PRs(ctx, r, []string{"touchmark/hub"}, nil); err != nil {
					errs <- err
				}
				if _, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, ""); err != nil {
					errs <- err
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := range 2 {
				if _, err := e.p.PushFiles(r.ID, "main", map[string][]byte{fmt.Sprintf("p%d.md", i): []byte("p")}, e.other, time.Time{}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, r := range repos {
		prs := e.p.PRList(r.ID)
		if len(prs) != 1 || prs[0].HeadSHA != e.p.Branch(r.ID, "touchmark/hub") || prs[0].State != platform.Open {
			t.Errorf("%s: %+v", r.Path, prs)
		}
	}
}

// TestGitMemoryMode: without ServeGit nothing of the git mode is there.
func TestGitMemoryMode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	n := e.pr(r, platform.PR{Head: "h", Author: e.other})
	for name, fn := range map[string]func() error{
		"PushFiles": func() error {
			_, err := e.p.PushFiles(r.ID, "h", nil, e.other, time.Time{})
			return err
		},
		"MergePR": func() error {
			_, err := e.p.MergePR(r.ID, n, fake.MergeCommit, e.other, time.Time{})
			return err
		},
		"UpdateBranch": func() error {
			_, err := e.p.UpdateBranch(r.ID, n, e.other, time.Time{})
			return err
		},
		"RebaseBranch": func() error {
			_, err := e.p.RebaseBranch(r.ID, n, e.other, time.Time{})
			return err
		},
	} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "needs git mode") {
			t.Errorf("%s in memory mode: %v", name, err)
		}
	}
	e.p.SetToken(e.reader, "t")
	e.ok()
	rem, err := e.p.Reader(e.reader).Remote(t.Context(), r)
	if err != nil || rem.URL != "fake://github.com/acme/api" || rem.Header != nil {
		t.Errorf("memory-mode remote with a token: %+v, %v", rem, err)
	}
	if e.p.Branch(r.ID, "main") != "" || e.p.GitDir(r.ID) != "" || len(e.p.Violations()) != 0 {
		t.Error("git-mode state in memory mode")
	}
}
