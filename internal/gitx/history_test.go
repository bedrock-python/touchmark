package gitx

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oidC = "cccccccccccccccccccccccccccccccccccccccc"
	oidZ = "0000000000000000000000000000000000000000"
	c1   = "1111111111111111111111111111111111111111"
	c2   = "2222222222222222222222222222222222222222"
)

func parseString(t *testing.T, s, prefix string) ([]BlobVersion, error) {
	t.Helper()
	var got []BlobVersion
	err := parseHistory(bufio.NewReader(strings.NewReader(s)), prefix, func(v BlobVersion) error {
		got = append(got, v)
		return nil
	})
	return got, err
}

func TestParseHistory(t *testing.T) {
	t.Parallel()
	// The layout git 2.33 prints, plus variants the parser tolerates: the
	// header and first entry in one token, commits without entries, a rename
	// with two paths and a trailing commit without a terminator.
	s := "\x00" + c1 + "\x00\n" +
		":100644 100644 " + oidA + " " + oidB + " M\x00p/a\x00" +
		":000000 100755 " + oidZ + " " + oidC + " A\x00p/x y\x00" +
		":100644 000000 " + oidA + " " + oidZ + " D\x00p/gone\x00" +
		":000000 120000 " + oidZ + " " + oidA + " A\x00p/link\x00" +
		":000000 160000 " + oidZ + " " + oidB + " A\x00p/sub\x00" +
		":120000 100664 " + oidA + " " + oidC + " T\x00p/legacy\x00" +
		"\x00" + c2 + "\x00" +
		"\x00" + c2 + "\n:100644 100644 " + oidA + " " + oidB + " R100\x00p/old\x00p/new\x00" +
		":100644 100644 " + oidA + " " + oidA + " M\x00outside\x00" +
		":100644 100644 " + oidA + " " + oidC + " M\x00pp/near\x00" +
		"\x00" + c1
	got, err := parseString(t, s, "p")
	if err != nil {
		t.Fatal(err)
	}
	want := []BlobVersion{
		{"p/a", oidB},
		{"p/x y", oidC},
		{"p/legacy", oidC},
		{"p/new", oidB},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseHistory = %v\nwant %v", got, want)
	}

	all, err := parseString(t, s, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(want)+2 {
		t.Errorf("parseHistory without prefix = %v", all)
	}

	if got, err := parseString(t, "", ""); err != nil || len(got) != 0 {
		t.Errorf("parseHistory(empty) = %v, %v", got, err)
	}
}

func TestParseHistoryErrors(t *testing.T) {
	t.Parallel()
	entry := ":100644 100644 " + oidA + " " + oidB + " M\x00"
	for name, s := range map[string]string{
		"garbage":           "\x00not-a-commit\x00",
		"combined diff":     "\x00" + c1 + "\x00\n::100644 100644 100644 " + oidA + " " + oidA + " " + oidB + " MM\x00p/a\x00",
		"short header":      "\x00" + c1 + "\x00\n:100644 100644 " + oidA + " M\x00p/a\x00",
		"bad oid":           "\x00" + c1 + "\x00\n:100644 100644 " + oidA + " xyz M\x00p/a\x00",
		"truncated":         "\x00" + c1 + "\x00\n" + entry,
		"unterminated path": "\x00" + c1 + "\x00\n" + entry + "p/a",
		"empty path":        "\x00" + c1 + "\x00\n" + entry + "\x00",
		"rename one path":   "\x00" + c1 + "\x00\n:100644 100644 " + oidA + " " + oidB + " R090\x00p/a\x00",
	} {
		if _, err := parseString(t, s, ""); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestParseHistoryCallbackError(t *testing.T) {
	t.Parallel()
	stop := errors.New("stop")
	s := "\x00" + c1 + "\x00\n" +
		":000000 100644 " + oidZ + " " + oidA + " A\x00a\x00" +
		":000000 100644 " + oidZ + " " + oidB + " A\x00b\x00"
	calls := 0
	err := parseHistory(bufio.NewReader(strings.NewReader(s)), "", func(BlobVersion) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("err = %v after %d calls, want stop after 1", err, calls)
	}
}

// TestHistoryScenario covers linear edits, a deletion, a merged side
// branch, an evil merge, two root commits, symlinks, gitlinks and type
// changes.
func TestHistoryScenario(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("packs/base/a.txt", "a1\n")
	r.write("packs/base/b.txt", "b1\n")
	r.write("other/o.txt", "o1\n")
	r.write("packs.txt", "not under packs/\n")
	r.setEntry("120000", "a.txt", "packs/base/link")
	root := r.commit("root")
	r.git("reset", "-q", "--hard")

	r.setEntry("160000", root, "packs/base/sub")
	r.write("packs/base/a.txt", "a2\n")
	r.commit("edit a, add submodule")
	r.git("reset", "-q", "--hard")

	r.remove("packs/base/b.txt")
	r.commit("delete b")

	r.git("checkout", "-q", "-b", "side")
	r.write("packs/base/a.txt", "a-side\n")
	r.write("packs/base/c.txt", "c1\n")
	r.commit("side 1")
	r.write("packs/base/c.txt", "c2\n")
	r.commit("side 2")

	r.git("checkout", "-q", "master")
	r.write("other/o.txt", "o2\n")
	r.write("packs/extra/e.txt", "e1\n")
	r.commit("master edit")
	if conflicts := r.startMerge("side"); len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts %v", conflicts)
	}
	r.commit("merge side")

	// Evil merge: the merge result holds a version of a.txt that neither
	// parent has.
	r.git("checkout", "-q", "-b", "topic")
	r.write("packs/extra/e.txt", "e2\n")
	r.commit("topic")
	r.git("checkout", "-q", "master")
	r.write("packs/base/c.txt", "c3\n")
	r.commit("master c3")
	if conflicts := r.startMerge("topic"); len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts %v", conflicts)
	}
	r.write("packs/base/a.txt", "evil\n")
	evilMerge := r.commit("evil merge")
	evil := r.git("rev-parse", "HEAD:packs/base/a.txt")
	for _, parent := range []string{evilMerge + "^1", evilMerge + "^2"} {
		if strings.Contains(r.git("ls-tree", "-r", parent), evil) {
			t.Fatalf("scenario broken: %s has the evil blob", parent)
		}
	}

	// A file turns into a symlink and back into a file.
	r.write("packs/base/t.txt", "t1\n")
	r.commit("t file")
	r.setEntry("120000", "a.txt", "packs/base/t.txt")
	r.commit("t symlink")
	r.git("reset", "-q", "--hard")
	r.setEntry("100755", "t2\n", "packs/base/t.txt")
	r.commit("t file again")
	r.git("reset", "-q", "--hard")

	// A second root commit, merged with --allow-unrelated-histories.
	r.git("checkout", "-q", "--orphan", "island")
	r.git("rm", "-r", "-q", "-f", "--", ".")
	r.write("packs/island/z.txt", "z1\n")
	r.commit("island root")
	r.git("checkout", "-q", "master")
	if conflicts := r.startMerge("island", "--allow-unrelated-histories"); len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts %v", conflicts)
	}
	r.commit("merge island")
	if roots := strings.Fields(r.git("rev-list", "--max-parents=0", "HEAD")); len(roots) != 2 {
		t.Fatalf("want 2 root commits, got %v", roots)
	}

	all := r.allTreeBlobs()
	for _, prefix := range []string{"packs", "packs/", "packs/base", "", "other", "pack", "packs/island"} {
		diffSets(t, r.history("HEAD", prefix), underPrefix(all, cleanPrefix(prefix)))
	}

	got := r.history("HEAD", "packs")
	blob := func(content string) string { return RawOID([]byte(content)) }
	for _, v := range []BlobVersion{
		{"packs/base/a.txt", blob("a1\n")},
		{"packs/base/a.txt", blob("a-side\n")},
		{"packs/base/a.txt", evil},
		{"packs/base/b.txt", blob("b1\n")},
		{"packs/base/c.txt", blob("c1\n")},
		{"packs/base/t.txt", blob("t1\n")},
		{"packs/base/t.txt", blob("t2\n")},
		{"packs/island/z.txt", blob("z1\n")},
	} {
		if !got[v] {
			t.Errorf("History lacks %s %s", v.Path, v.OID)
		}
	}
	for v := range got {
		if strings.HasSuffix(v.Path, "/link") || strings.HasSuffix(v.Path, "/sub") || v.Path == "packs.txt" {
			t.Errorf("History reports %s", v.Path)
		}
	}
	if evil != blob("evil\n") {
		t.Errorf("evil blob %s != RawOID %s", evil, blob("evil\n"))
	}

	// Older revisions see only their own history.
	old := r.history(root, "packs")
	if len(old) != 2 {
		t.Errorf("History(root) = %v", old)
	}
}

func TestHistoryErrors(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("packs/p/a.txt", "a\n")
	r.write("packs/p/b.txt", "b\n")
	r.commit("one")

	stop := errors.New("stop")
	calls := 0
	err := r.g.History(t.Context(), "HEAD", "", func(BlobVersion) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("History with failing callback = %v after %d calls", err, calls)
	}

	noop := func(BlobVersion) error { return nil }
	var gitErr *Error
	if err := r.g.History(t.Context(), "no-such-rev", "", noop); !errors.As(err, &gitErr) {
		t.Errorf("History(no-such-rev) = %v, want *Error", err)
	}
	if err := r.g.History(t.Context(), "-p", "", noop); err == nil {
		t.Error("History(-p): want error")
	}
	empty := newTestRepo(t, false)
	if err := empty.g.History(t.Context(), "HEAD", "", noop); err == nil {
		t.Error("History on a repository without commits: want error")
	}
}

// TestHistoryLiteralPrefix checks that prefix is a literal directory path,
// not a glob and not a string prefix.
func TestHistoryLiteralPrefix(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("packs/a[b]/x.txt", "x\n")
	r.write("packs/ab/y.txt", "y\n")
	r.write("packs/abc/w.txt", "w\n")
	r.commit("one")
	all := r.allTreeBlobs()
	for _, prefix := range []string{"packs/a[b]", "packs/ab", "packs/a"} {
		got := r.history("HEAD", prefix)
		diffSets(t, got, underPrefix(all, prefix))
		if prefix != "packs/a" && len(got) != 1 {
			t.Errorf("History(%q) = %v, want one file", prefix, got)
		}
	}
}

// TestHistoryIgnoresUserConfig pins config that would change git log output.
func TestHistoryIgnoresUserConfig(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("packs/p/a.txt", "a1\n")
	r.commit("one")
	r.git("checkout", "-q", "-b", "side")
	r.write("packs/p/b.txt", "b1\n")
	r.commit("side")
	r.git("checkout", "-q", "master")
	r.write("packs/p/c.txt", "c1\n")
	r.commit("master")
	r.startMerge("side")
	r.write("packs/p/a.txt", "evil\n")
	r.commit("evil merge")
	want := r.treeBlobs("packs")
	evil := BlobVersion{Path: "packs/p/a.txt", OID: RawOID([]byte("evil\n"))}
	if !want[evil] {
		t.Fatalf("the reference misses the evil merge blob %v", evil)
	}

	for _, kv := range [][2]string{
		{"log.follow", "true"},
		{"diff.relative", "true"},
		{"diff.renames", "copies"},
		{"core.abbrev", "7"},
		{"log.showRoot", "false"},
		{"diff.external", "false"},
	} {
		r.git("config", kv[0], kv[1])
	}
	sub := &Git{Dir: filepath.Join(r.dir, "packs", "p"), Env: r.g.Env}
	// Each value of log.diffMerges changes which merge diffs -m prints: off
	// and none drop them (and the evil blob with them), combined and
	// dense-combined print "::" entries.
	for _, mode := range []string{"off", "none", "first-parent", "combined", "dense-combined"} {
		r.git("config", "log.diffMerges", mode)
		for _, g := range []*Git{r.g, sub} {
			got := map[BlobVersion]bool{}
			err := g.History(t.Context(), "HEAD", "packs", func(v BlobVersion) error {
				got[v] = true
				return nil
			})
			if err != nil {
				t.Fatalf("log.diffMerges=%s in %s: %v", mode, g.Dir, err)
			}
			if !got[evil] {
				t.Errorf("log.diffMerges=%s in %s: the evil merge blob is missing", mode, g.Dir)
			}
			diffSets(t, got, want)
		}
	}
}

// TestHistoryRandom compares History with the reference on seeded random
// histories with branches, merges, conflicts and evil merges.
func TestHistoryRandom(t *testing.T) {
	t.Parallel()
	seeds := []uint64{1, 2, 3}
	if testing.Short() {
		seeds = seeds[:1]
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			r := newTestRepo(t, false)
			r.git("config", "core.fileMode", "false")
			genHistory(r, rand.New(rand.NewPCG(seed, 0x70c4)), 30)
			if n := len(strings.Fields(r.git("rev-list", "--all"))); n < 30 {
				t.Fatalf("generated only %d commits", n)
			}
			if merges := strings.Fields(r.git("rev-list", "--merges", "--all")); len(merges) == 0 {
				t.Fatal("generated no merges")
			}
			all := r.allTreeBlobs()
			for _, prefix := range []string{"packs", "packs/p1", ""} {
				diffSets(t, r.history("HEAD", prefix), underPrefix(all, prefix))
			}
		})
	}
}

var randPaths = []string{
	"packs/p1/a.txt",
	"packs/p1/dir/b.txt",
	"packs/p1/dir/deep/c.txt",
	"packs/p2/d e.txt",
	"packs/p2/юникод.md",
	"packs/p3/f.bin",
	"packs.txt",
	"other/x.txt",
}

var randContents = []string{
	"one\n", "two\n", "three\n", "line\r\nwith crlf\r\n", "\x00\x01binary\xff\n", "no newline",
}

// genHistory builds about steps commits on random branches, then merges
// every branch into master so that HEAD reaches all commits. It works on
// the work tree and stages with `git add -A` to keep the number of git
// processes low.
func genHistory(r *testRepo, rng *rand.Rand, steps int) {
	r.t.Helper()
	for _, p := range randPaths[:3] {
		r.writeFile(p, []byte(randContents[rng.IntN(len(randContents))]))
	}
	r.git("add", "-A")
	r.commitIndex("root")
	branches := []string{"master"}
	for i := range steps {
		switch op := rng.IntN(10); {
		case op < 5 || len(branches) < 2 && op >= 7:
			r.git("checkout", "-q", branches[rng.IntN(len(branches))])
			mutate(r, rng, i)
			r.commitIndex(fmt.Sprintf("edit %d", i))
		case op < 7:
			name := fmt.Sprintf("b%d", i)
			r.git("checkout", "-q", "-b", name, branches[rng.IntN(len(branches))])
			branches = append(branches, name)
			mutate(r, rng, i)
			r.commitIndex(fmt.Sprintf("branch %d", i))
		default:
			perm := rng.Perm(len(branches))
			r.git("checkout", "-q", branches[perm[0]])
			randomMerge(r, rng, branches[perm[1]], i)
		}
	}
	r.git("checkout", "-q", "master")
	for i, b := range branches[1:] {
		randomMerge(r, rng, b, steps+i)
	}
}

// mutate writes, rewrites, deletes or chmods one to three random paths and
// stages the result.
func mutate(r *testRepo, rng *rand.Rand, step int) {
	r.t.Helper()
	var chmod []string
	for range 1 + rng.IntN(3) {
		p := randPaths[rng.IntN(len(randPaths))]
		full := filepath.Join(r.dir, filepath.FromSlash(p))
		_, err := os.Stat(full)
		exists := err == nil
		switch op := rng.IntN(10); {
		case exists && op == 0:
			if err := os.Remove(full); err != nil {
				r.t.Fatal(err)
			}
		case exists && op == 1:
			chmod = append(chmod, p)
		case op < 4:
			r.writeFile(p, fmt.Appendf(nil, "unique %d %d\n", step, rng.IntN(1000)))
		default:
			r.writeFile(p, []byte(randContents[rng.IntN(len(randContents))]))
		}
	}
	r.git("add", "-A")
	var present []string
	for _, p := range chmod {
		if _, err := os.Stat(filepath.Join(r.dir, filepath.FromSlash(p))); err == nil {
			present = append(present, p)
		}
	}
	if len(present) > 0 {
		r.git(append([]string{"update-index", "--chmod=+x", "--"}, present...)...)
	}
}

// randomMerge merges from into the current branch, resolves conflicts with
// fresh content and sometimes adds an evil change.
func randomMerge(r *testRepo, rng *rand.Rand, from string, step int) {
	r.t.Helper()
	conflicts := r.startMerge(from)
	for _, p := range conflicts {
		r.writeFile(p, fmt.Appendf(nil, "resolved %d %s\n", step, p))
	}
	evil := rng.IntN(3) == 0
	if evil {
		p := randPaths[rng.IntN(len(randPaths))]
		r.writeFile(p, fmt.Appendf(nil, "evil %d\n", step))
	}
	if len(conflicts) > 0 || evil {
		r.git("add", "-A")
	}
	r.commitIndex(fmt.Sprintf("merge %s %d", from, step))
}
