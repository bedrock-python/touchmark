package apply

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
)

// caseInsensitive reports whether the filesystem holding dir ignores case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := dir + string(os.PathSeparator) + "case-probe"
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(dir + string(os.PathSeparator) + "CASE-PROBE")
	return err == nil
}

// TestObserveSpelling: Observe tells how the target spells a path. On a
// case-insensitive filesystem the lookup finds the other spelling, which
// ActualPath names; on a case-sensitive one the requested path is absent
// and CaseTwin names the other spelling.
func TestObserveSpelling(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("Docs/Guide.md", "guide\n")
	tr.write("README.md", "readme\n")
	obs := tr.observe("Docs/Guide.md", "docs/guide.md", "readme.md", "docs/other.md", "Docs")
	if o := obs["Docs/Guide.md"]; o.Kind != decide.Regular || o.ActualPath != "" {
		t.Errorf("exact spelling: %+v", o)
	}
	if o := obs["docs/other.md"]; o.Kind != decide.Absent || o.CaseTwin != "" {
		t.Errorf("a new file in a case variant of a directory: %+v", o)
	}
	if caseInsensitive(t, tr.dir) {
		for p, want := range map[string]string{"docs/guide.md": "Docs/Guide.md", "readme.md": "README.md"} {
			if o := obs[p]; o.Kind != decide.Regular || o.ActualPath != want {
				t.Errorf("%s: %+v, want regular spelled %s", p, o, want)
			}
		}
		return
	}
	for p, want := range map[string]string{"docs/guide.md": "Docs/Guide.md", "readme.md": "README.md"} {
		if o := obs[p]; o.Kind != decide.Absent || o.CaseTwin != want {
			t.Errorf("%s: %+v, want absent next to %s", p, o, want)
		}
	}
}

// TestCaseOnlyRenameConverges runs a case-only rename with changed content
// through Observe, Decide and Execute: whatever the filesystem, the target
// ends up with one file holding the new version, and a second plan is empty.
func TestCaseOnlyRenameConverges(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("docs/Guide.md", "guide v1\n")
	h := newHub()
	h.shipped("docs/Guide.md", "guide v1\n")
	h.ships("docs/guide.md", "guide v2\n")
	h.apply(t, tr)
	entries, err := os.ReadDir(tr.abs("docs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("docs holds %d entries, want one file", len(entries))
	}
	if got := tr.read("docs/" + entries[0].Name()); got != "guide v2\n" {
		t.Errorf("%s = %q, want v2", entries[0].Name(), got)
	}
}

// TestCreateBesideCaseTwin: a create never adds a second spelling next to
// an existing one, even when the plan did not know about it.
func TestCreateBesideCaseTwin(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("docs/README.md", "the target's own\n")
	blobs := memBlobs{}
	res := tr.execute(blobs, create("docs/readme.md", blobs.add("pack readme\n"), modeFile))
	if caseInsensitive(t, tr.dir) {
		wantSkipped(t, res[0], "the path is a file")
	} else {
		wantSkipped(t, res[0], "docs/README.md differs only by case")
	}
	if got := tr.read("docs/README.md"); got != "the target's own\n" {
		t.Errorf("docs/README.md = %q", got)
	}
}

// TestCreateDoesNotReplace: a file that appears between the last check and
// the move is not replaced.
func TestCreateDoesNotReplace(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	root, err := os.OpenRoot(tr.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	x := &executor{ctx: t.Context(), t: tr.target(), root: root}
	tr.write(".touchmark-test.tmp", "ours\n")
	tr.write("a.md", "appeared meanwhile\n")
	err = x.place(create("a.md", oid("ours\n"), modeFile), ".touchmark-test.tmp")
	var s skip
	if !errors.As(err, &s) || !strings.Contains(err.Error(), "appeared") {
		t.Errorf("place = %v, want a skip", err)
	}
	if got := tr.read("a.md"); got != "appeared meanwhile\n" {
		t.Errorf("a.md = %q, want it untouched", got)
	}
	tr.remove(".touchmark-test.tmp")
}

// TestUpdateReadOnly: a read-only file (attrib +r on Windows) is updated.
func TestUpdateReadOnly(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.writeMode("ro.md", "v1\n", 0o444)
	blobs := memBlobs{}
	wantDone(t, tr.execute(blobs, update("ro.md", oid("v1\n"), blobs.add("v2\n"), modeFile)))
	if got := tr.read("ro.md"); got != "v2\n" {
		t.Errorf("ro.md = %q, want v2", got)
	}
}

// TestNestedRepositoryParent: files under a nested repository or a
// submodule belong to that repository; touchmark neither writes nor deletes
// there.
func TestNestedRepositoryParent(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, false)
	tr.write("nested/.git", "gitdir: ../.git/modules/nested\n")
	tr.write("nested/old.md", "old\n")
	tr.write("tracked.md", "tracked\n")
	tr.run("add", "tracked.md")
	tr.run("commit", "-q", "-m", "one")
	head := tr.run("rev-parse", "HEAD")
	tr.run("update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	tr.mkdir("sub") // an uninitialized submodule: an empty directory

	obs := tr.observe("nested/new.md", "nested/old.md", "sub/new.md", "sub/deep/x.md")
	for p, blocker := range map[string]string{
		"nested/new.md": "nested", "nested/old.md": "nested", "sub/new.md": "sub", "sub/deep/x.md": "sub",
	} {
		o := obs[p]
		if o.Kind != decide.UnsafeParent || o.Blocker != blocker || o.BlockerIsFile {
			t.Errorf("%s: %+v, want unsafe below %s", p, o, blocker)
		}
	}

	blobs := memBlobs{}
	res := tr.execute(blobs,
		del("nested/old.md", oid("old\n")),
		create("nested/new.md", blobs.add("new\n"), modeFile),
	)
	wantSkipped(t, res[0], "nested repository")
	wantSkipped(t, res[1], "nested repository")
	if tr.lstat("nested/new.md") != nil || tr.lstat("nested/old.md") == nil {
		t.Error("Execute wrote into the nested repository")
	}
}

// TestIndexCorrectsObservations: where git ignores the executable bit on
// disk, the index says what it is; and a file the index tracks as a symlink
// is not a regular file.
func TestIndexCorrectsObservations(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, false)
	tr.run("config", "core.fileMode", "false")
	tr.write("run.sh", "run\n")
	tr.write("plain.sh", "plain\n")
	tr.write("link", "run.sh")
	tr.run("add", "run.sh", "plain.sh")
	tr.run("update-index", "--chmod=+x", "run.sh")
	tr.run("update-index", "--add", "--cacheinfo", "120000,"+oid("run.sh")+",link")
	tr.writeMode("untracked.sh", "new\n", 0o755)

	obs := tr.observe("run.sh", "plain.sh", "untracked.sh", "link")
	for p, want := range map[string]string{"run.sh": modeExec, "plain.sh": modeFile, "untracked.sh": modeFile} {
		if got := obs[p].Mode; got != want {
			t.Errorf("%s: mode %q, want the index's %q", p, got, want)
		}
	}
	if o := obs["link"]; o.Kind != decide.NotRegular {
		t.Errorf("link: %+v, want not regular", o)
	}
}

// TestExecutableBitRecordedInIndex: where git ignores the executable bit on
// disk, a 100755 create or chmod records it in the index, so a commit
// keeps it; status then sees the file current.
func TestExecutableBitRecordedInIndex(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, false)
	tr.run("config", "core.fileMode", "false")
	tr.write("tracked.sh", "run\n")
	tr.run("add", "tracked.sh")
	tr.run("commit", "-q", "-m", "one")
	blobs := memBlobs{}
	script := blobs.add("#!/bin/sh\n")
	wantDone(t, tr.execute(blobs,
		create("bin/new.sh", script, modeExec),
		create("bin/plain.txt", script, modeFile),
		decide.Entry{Path: "tracked.sh", State: decide.Current, Action: decide.Chmod, From: oid("run\n"), Mode: modeExec},
	))
	staged := tr.run("ls-files", "-s", "--", "bin/new.sh", "tracked.sh", "bin/plain.txt")
	for _, want := range []string{"100755 " + script + " 0\tbin/new.sh", "100755 " + oid("run\n") + " 0\ttracked.sh"} {
		if !strings.Contains(staged, want) {
			t.Errorf("index:\n%s\nwant a line %q", staged, want)
		}
	}
	if strings.Contains(staged, "bin/plain.txt") {
		t.Errorf("a 100644 create was added to the index:\n%s", staged)
	}
	obs := tr.observe("bin/new.sh", "tracked.sh")
	for p, o := range obs {
		if o.Mode != modeExec {
			t.Errorf("%s observed as %q, want %s", p, o.Mode, modeExec)
		}
	}
}

// TestObserveUnhashableFile: a file git cannot hash is unsafe; the others
// are still observed through git.
func TestObserveUnhashableFile(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, true)
	tr.run("config", "filter.boom.clean", "false")
	tr.run("config", "filter.boom.required", "true")
	tr.write(".gitattributes", "bad.txt filter=boom\n")
	tr.write("bad.txt", "bad\n")
	tr.write("good.txt", "good\r\n")
	obs := tr.observe("bad.txt", "good.txt")
	if o := obs["bad.txt"]; o.Kind != decide.InvalidPath || !strings.Contains(o.Detail, "cannot hash through git") {
		t.Errorf("bad.txt: %+v, want unsafe", o)
	}
	if o := obs["good.txt"]; o.Kind != decide.Regular || len(o.OIDs) != 2 || o.OIDs[1] != oid("good\n") {
		t.Errorf("good.txt: %+v, want both ids", o)
	}
}
