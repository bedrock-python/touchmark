package provenance

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

// file returns the File a pack ships with content.
func file(pack, path, content, mode string) File {
	return File{Pack: pack, Path: path, OID: blob(content), Size: int64(len(content)), Mode: mode}
}

// current builds a Current from files.
func current(files ...File) Current {
	cur := Current{}
	for _, f := range files {
		if cur[f.Pack] == nil {
			cur[f.Pack] = map[string]File{}
		}
		cur[f.Pack][f.Path] = f
	}
	return cur
}

// wantProblem is an expected problem; msg is a substring of Message.
type wantProblem struct {
	pack, path, msg string
}

func checkProblems(t *testing.T, got []Problem, want []wantProblem) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		w := want[i]
		ok = got[i].Pack == w.pack && got[i].Path == w.path && strings.Contains(got[i].Message, w.msg)
	}
	if !ok {
		t.Errorf("problems:\n%s\nwant:\n%+v", formatProblems(got), want)
	}
}

func formatProblems(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("  " + p.String() + "\n")
	}
	return b.String()
}

func readCurrent(t *testing.T, h *Hub, src Source) (Current, []Problem) {
	t.Helper()
	cur, problems, err := ReadCurrent(t.Context(), h, src)
	if err != nil {
		t.Fatalf("ReadCurrent(%d): %v", src, err)
	}
	return cur, problems
}

func TestReadCurrentCommittedAndWorkTree(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	agents, run, editor := text("agents"), text("run"), text("editor")
	r.write("packs/agents/AGENTS.md", agents)
	r.write("packs/agents/bin/run.sh", run)
	r.git("update-index", "--chmod=+x", "packs/agents/bin/run.sh")
	r.write("packs/core/.editorconfig", editor)
	r.write("README.md", "outside packs/\n")
	r.commit("c1")

	// Uncommitted: an unstaged edit, a staged edit and an untracked file.
	edited, staged, untracked := text("edited"), text("staged"), text("untracked")
	r.writeFile("packs/agents/AGENTS.md", edited)
	r.write("packs/core/.editorconfig", staged)
	r.writeFile("packs/core/new.md", untracked)
	if err := os.Chmod(filepath.Join(r.dir, "packs", "agents", "bin", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	cur, problems := readCurrent(t, r.hub(), Committed)
	want := current(
		file("agents", "AGENTS.md", agents, modeFile),
		file("agents", "bin/run.sh", run, modeExec),
		file("core", ".editorconfig", editor, modeFile),
	)
	if !reflect.DeepEqual(cur, want) || len(problems) != 0 {
		t.Errorf("ReadCurrent(Committed) = %+v, %v\nwant %+v", cur, problems, want)
	}

	// Where git ignores the executable bit (Windows), the index keeps it.
	cur, problems = readCurrent(t, r.hub(), WorkTree)
	want = current(
		file("agents", "AGENTS.md", edited, modeFile),
		file("agents", "bin/run.sh", run, modeExec),
		file("core", ".editorconfig", staged, modeFile),
		file("core", "new.md", untracked, modeFile),
	)
	if !reflect.DeepEqual(cur, want) || len(problems) != 0 {
		t.Errorf("ReadCurrent(WorkTree) = %+v, %v\nwant %+v", cur, problems, want)
	}

	// The blobs git would commit were written to a scratch store only.
	if _, err := r.g.Run(t.Context(), nil, "cat-file", "-e", blob(untracked)); err == nil {
		t.Error("ReadCurrent(WorkTree) wrote into the hub's object store")
	}

	// With core.fileMode=false the index decides on every OS.
	r.git("config", "core.fileMode", "false")
	r.git("update-index", "--chmod=-x", "packs/agents/bin/run.sh")
	cur, _ = readCurrent(t, r.hub(), WorkTree)
	if got := cur["agents"]["bin/run.sh"].Mode; got != modeFile {
		t.Errorf("core.fileMode=false: run.sh mode %s, want the index's %s", got, modeFile)
	}
}

// TestReadCurrentIgnoresCheckoutFilters: a CRLF checkout of the hub does not
// change what ships, and --worktree reads the files through the hub's clean
// filters, so an unedited CRLF checkout reads the same as the commit.
func TestReadCurrentIgnoresCheckoutFilters(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, true)
	lf := text("line endings")
	r.write(".gitattributes", "*.bin -text\n")
	r.write("packs/agents/AGENTS.md", lf)
	r.write("packs/agents/raw.bin", "raw\r\n"+text("bytes"))
	r.commit("c1")
	osPath := filepath.Join(r.dir, "packs", "agents", "AGENTS.md")
	if err := os.Remove(osPath); err != nil {
		t.Fatal(err)
	}
	r.git("checkout", "--", "packs/agents/AGENTS.md")
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	if data, err := os.ReadFile(osPath); err != nil || string(data) != crlf {
		t.Fatalf("checkout did not produce CRLF: %q, %v", data, err)
	}

	committed, _ := readCurrent(t, r.hub(), Committed)
	if got := committed["agents"]["AGENTS.md"].OID; got != blob(lf) {
		t.Errorf("Committed OID = %s, want the LF blob %s", got, blob(lf))
	}
	work, _ := readCurrent(t, r.hub(), WorkTree)
	if !reflect.DeepEqual(work, committed) {
		t.Errorf("WorkTree = %+v\nwant the committed %+v", work, committed)
	}

	// Outside git there are no filters: the raw bytes count.
	plain := t.TempDir()
	writeTestFile(t, filepath.Join(plain, "packs", "agents", "AGENTS.md"), crlf)
	cur, _ := readCurrent(t, &Hub{Dir: plain, Git: &gitx.Git{Dir: plain, Env: append(testEnv(t), "GIT_CEILING_DIRECTORIES="+filepath.Dir(plain))}}, WorkTree)
	if got := cur["agents"]["AGENTS.md"]; got.OID != blob(crlf) || got.Size != int64(len(crlf)) {
		t.Errorf("WorkTree outside git = %+v, want the raw CRLF bytes %s", got, blob(crlf))
	}
	m, _, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	if vs := m.Versions("AGENTS.md", "agents"); len(vs) != 1 || vs[0] != ver(lf) {
		t.Errorf("manifest versions = %+v, want the LF blob", vs)
	}
}

func TestReadCurrentCommittedProblems(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	agents, run := text("agents"), text("run")
	r.write("packs/agents/AGENTS.md", agents)
	root := r.commit("c0")
	r.write("packs/README.md", "directly under packs/\n")
	r.setEntry("120000", "AGENTS.md", "packs/agents/link")
	r.setEntry("160000", root, "packs/agents/sub")
	r.setEntry("100644", text("x"), "packs/Bad_Name/x.md")
	r.setEntry("100644", text("y"), "packs/Bad_Name/y.md")
	r.setEntry("100644", text("colon"), "packs/agents/a:b.md")
	r.setEntry("100644", text("git dir"), "packs/agents/deep/git~1/config")
	r.setEntry("100755", run, "packs/agents/run.sh")
	r.commit("c1")

	cur, problems := readCurrent(t, r.hub(), Committed)
	want := current(
		file("agents", "AGENTS.md", agents, modeFile),
		file("agents", "run.sh", run, modeExec),
	)
	if !reflect.DeepEqual(cur, want) {
		t.Errorf("ReadCurrent(Committed) = %+v\nwant %+v", cur, want)
	}
	checkProblems(t, problems, []wantProblem{
		{"", "", "packs/README.md: file outside any pack"},
		{"Bad_Name", "", `pack name "Bad_Name"`},
		{"agents", "a:b.md", "invalid repository path"},
		{"agents", "deep/git~1/config", "inside .git"},
		{"agents", "link", msgSymlink},
		{"agents", "sub", msgSubmodule},
	})
}

func TestReadCurrentPacksNotADirectory(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	r.write("packs", text("a file named packs"))
	r.commit("c1")
	for _, src := range []Source{Committed, WorkTree} {
		cur, problems := readCurrent(t, r.hub(), src)
		if len(cur) != 0 {
			t.Errorf("ReadCurrent(%d) = %+v, want no packs", src, cur)
		}
		checkProblems(t, problems, []wantProblem{{"", "", "packs: must be a directory of packs, not a file"}})
	}
}

func TestReadCurrentWithoutPacks(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	r.write("README.md", "no packs\n")
	r.commit("c1")
	for _, src := range []Source{Committed, WorkTree} {
		cur, problems := readCurrent(t, r.hub(), src)
		if cur == nil || len(cur) != 0 || len(problems) != 0 {
			t.Errorf("ReadCurrent(%d) = %#v, %v; want an empty Current", src, cur, problems)
		}
	}
}

func TestReadCurrentWorkTreeProblems(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := func(p, content string) { writeTestFile(t, filepath.Join(dir, filepath.FromSlash(p)), content) }
	agents, store := text("agents"), text("hidden files count")
	w("packs/agents/AGENTS.md", agents)
	w("packs/agents/.DS_Store", store)
	w("packs/README.md", "directly under packs/\n")
	w("packs/Bad_Name/x.md", text("x"))
	w("packs/Bad_Name/y.md", text("y"))
	w("packs/agents/nested/.git/config", "[core]\n")
	w("packs/agents/nested/.git/HEAD", "ref: refs/heads/master\n")
	if err := os.MkdirAll(filepath.Join(dir, "packs", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	// WorkTree does not need git.
	cur, problems := readCurrent(t, &Hub{Dir: dir}, WorkTree)
	want := current(
		file("agents", "AGENTS.md", agents, modeFile),
		file("agents", ".DS_Store", store, modeFile),
	)
	if !reflect.DeepEqual(cur, want) {
		t.Errorf("ReadCurrent(WorkTree) = %+v\nwant %+v", cur, want)
	}
	checkProblems(t, problems, []wantProblem{
		{"", "", "packs/README.md: file outside any pack"},
		{"Bad_Name", "", `pack name "Bad_Name"`},
		{"agents", "nested/.git", "inside .git"},
	})
}

func TestReadCurrentWorkTreeUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("names and modes Windows cannot hold")
	}
	t.Parallel()
	dir := t.TempDir()
	w := func(p, content string) { writeTestFile(t, filepath.Join(dir, filepath.FromSlash(p)), content) }
	run := text("run")
	w("packs/agents/run.sh", run)
	if err := os.Chmod(filepath.Join(dir, "packs", "agents", "run.sh"), 0o744); err != nil {
		t.Fatal(err)
	}
	w("packs/agents/back\\slash.md", text("backslash"))
	w("packs/agents/tab\there.md", text("tab"))

	cur, problems := readCurrent(t, &Hub{Dir: dir}, WorkTree)
	if want := current(file("agents", "run.sh", run, modeExec)); !reflect.DeepEqual(cur, want) {
		t.Errorf("ReadCurrent(WorkTree) = %+v\nwant %+v", cur, want)
	}
	checkProblems(t, problems, []wantProblem{
		{"agents", "back\\slash.md", `contains '\' or ':'`},
		{"agents", "tab\there.md", "control character"},
	})
}

func TestReadCurrentWorkTreeSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agents := text("agents")
	writeTestFile(t, filepath.Join(dir, "packs", "agents", "AGENTS.md"), agents)
	pack := filepath.Join(dir, "packs", "agents")
	if err := os.Symlink("AGENTS.md", filepath.Join(pack, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, link := range []string{filepath.Join(pack, "loop"), filepath.Join(dir, "packs", "alias")} {
		if err := os.Symlink(pack, link); err != nil {
			t.Fatal(err)
		}
	}

	cur, problems := readCurrent(t, &Hub{Dir: dir}, WorkTree)
	if want := current(file("agents", "AGENTS.md", agents, modeFile)); !reflect.DeepEqual(cur, want) {
		t.Errorf("ReadCurrent(WorkTree) = %+v\nwant %+v", cur, want)
	}
	checkProblems(t, problems, []wantProblem{
		{"", "", "packs/alias: symlink outside any pack"},
		{"agents", "link.md", msgSymlink},
		{"agents", "loop", msgSymlink},
	})

	// packs itself as a symlink is not followed.
	other := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "packs"), filepath.Join(other, "packs")); err != nil {
		t.Fatal(err)
	}
	cur, problems = readCurrent(t, &Hub{Dir: other}, WorkTree)
	if len(cur) != 0 {
		t.Errorf("ReadCurrent(packs symlink) = %+v, want no packs", cur)
	}
	checkProblems(t, problems, []wantProblem{{"", "", "packs: must be a directory of packs, not a symlink"}})
}

func TestReadCurrentErrors(t *testing.T) {
	t.Parallel()
	if _, _, err := ReadCurrent(t.Context(), nil, Committed); err == nil {
		t.Error("ReadCurrent(nil): want error")
	}
	r := newHubRepo(t, false)
	if _, _, err := ReadCurrent(t.Context(), r.hub(), Committed); err == nil {
		t.Error("ReadCurrent(Committed, no commits): want error")
	}
	if _, _, err := ReadCurrent(t.Context(), r.hub(), Source(42)); err == nil {
		t.Error("ReadCurrent(unknown source): want error")
	}
}

func TestProblemString(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		p    Problem
		want string
	}{
		{Problem{Message: "packs/README.md: file outside any pack"}, "packs/README.md: file outside any pack"},
		{Problem{Pack: "agents", Message: "m"}, "packs/agents: m"},
		{Problem{Pack: "agents", Path: "docs/a b.md", Message: "m"}, "packs/agents/docs/a b.md: m"},
		{Problem{Pack: "agents", Path: "a\tb", Message: "m"}, `"packs/agents/a\tb": m`},
		{Problem{Pack: "Bad\x1b[31m", Message: "m"}, `"packs/Bad\x1b[31m": m`},
	} {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("%+v.String() = %q, want %q", tt.p, got, tt.want)
		}
	}
}
