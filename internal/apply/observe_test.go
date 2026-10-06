package apply

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
)

func TestObserveKinds(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("README.md", "hello\n")
	tr.write("docs/guide.md", "guide\n")
	tr.mkdir("dir/sub")
	tr.write("file", "a file where a directory is expected\n")

	obs := tr.observe("README.md", "docs/guide.md", "missing.md", "gone/deep/x.md",
		"dir", "dir/sub", "file/x.md", "file/deeper/x.md", "../escape", ".git/config", "a\\b")

	wantMode := modeFile
	if runtime.GOOS == "windows" {
		wantMode = ""
	}
	regular := func(content string) decide.Observation {
		return decide.Observation{Kind: decide.Regular, OIDs: []string{oid(content)}, Mode: wantMode}
	}
	unsafeParent := decide.Observation{
		Kind: decide.UnsafeParent, Blocker: "file", BlockerIsFile: true, Detail: "parent file is a file",
	}
	want := map[string]decide.Observation{
		"README.md":        regular("hello\n"),
		"docs/guide.md":    regular("guide\n"),
		"missing.md":       {Kind: decide.Absent},
		"gone/deep/x.md":   {Kind: decide.Absent},
		"dir":              {Kind: decide.NotRegular, IsDir: true, Detail: "a directory"},
		"dir/sub":          {Kind: decide.NotRegular, IsDir: true, Detail: "a directory"},
		"file/x.md":        unsafeParent,
		"file/deeper/x.md": unsafeParent,
	}
	for p, w := range want {
		if got := obs[p]; !equalObservation(got, w) {
			t.Errorf("%s: got %+v\nwant %+v", p, got, w)
		}
	}
	for _, p := range []string{"../escape", ".git/config", "a\\b"} {
		if o := obs[p]; o.Kind != decide.InvalidPath || o.Detail == "" {
			t.Errorf("%s: got %+v, want InvalidPath with a detail", p, o)
		}
	}
	if len(obs) != len(want)+3 {
		t.Errorf("got %d observations, want %d", len(obs), len(want)+3)
	}
}

func equalObservation(a, b decide.Observation) bool {
	return a.Kind == b.Kind && slices.Equal(a.OIDs, b.OIDs) && a.Mode == b.Mode && a.IsDir == b.IsDir &&
		a.Blocker == b.Blocker && a.BlockerIsFile == b.BlockerIsFile && a.Detail == b.Detail
}

func TestObserveSymlinks(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("real/file.md", "content\n")
	tr.symlink("real/file.md", "link.md")
	tr.symlink("real", "linkdir")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.symlink(outside, "escape.md")

	obs := tr.observe("link.md", "linkdir", "linkdir/file.md", "linkdir/new/x.md", "escape.md")
	for _, p := range []string{"link.md", "linkdir", "escape.md"} {
		if o := obs[p]; o.Kind != decide.NotRegular || o.IsDir || o.Detail != "a symlink" {
			t.Errorf("%s: got %+v, want NotRegular symlink", p, o)
		}
	}
	for _, p := range []string{"linkdir/file.md", "linkdir/new/x.md"} {
		o := obs[p]
		if o.Kind != decide.UnsafeParent || o.Blocker != "linkdir" || o.BlockerIsFile || o.Detail != "parent linkdir is a symlink" {
			t.Errorf("%s: got %+v, want UnsafeParent linkdir (not a file)", p, o)
		}
	}
}

func TestObserveMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the executable bit is not recorded on Windows")
	}
	tr := newTree(t)
	tr.writeMode("run.sh", "#!/bin/sh\n", 0o755)
	tr.writeMode("owner-only.sh", "#!/bin/sh\n", 0o700)
	tr.writeMode("group-only.sh", "#!/bin/sh\n", 0o654)
	tr.writeMode("plain.txt", "text\n", 0o644)
	obs := tr.observe("run.sh", "owner-only.sh", "group-only.sh", "plain.txt")
	// Like git, only the owner's executable bit counts.
	for p, want := range map[string]string{
		"run.sh": modeExec, "owner-only.sh": modeExec, "group-only.sh": modeFile, "plain.txt": modeFile,
	} {
		if got := obs[p].Mode; got != want {
			t.Errorf("%s: mode %q, want %q", p, got, want)
		}
	}
}

// TestObserveCRLF is the Windows checkout case: with core.autocrlf=true the
// file on disk has CRLF while git stores LF. Both ids must be observed.
func TestObserveCRLF(t *testing.T) {
	t.Parallel()
	lf := "line one\nline two\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	tr := newRepo(t, true)
	tr.write("crlf.md", crlf)
	tr.write("lf.md", lf)
	tr.write("binary.bin", crlf)
	tr.write(".gitattributes", "*.bin -text\n")
	obs := tr.observe("crlf.md", "lf.md", "binary.bin", "crlf.md")
	for p, want := range map[string][]string{
		"crlf.md":    {oid(crlf), oid(lf)},
		"lf.md":      {oid(lf)},
		"binary.bin": {oid(crlf)},
	} {
		if got := obs[p].OIDs; !slices.Equal(got, want) {
			t.Errorf("%s: OIDs %v, want %v", p, got, want)
		}
	}

	plain := newRepo(t, false)
	plain.write("crlf.md", crlf)
	if got, want := plain.observe("crlf.md")["crlf.md"].OIDs, []string{oid(crlf)}; !slices.Equal(got, want) {
		t.Errorf("autocrlf=false: OIDs %v, want %v", got, want)
	}
}

// TestObserveCommittedCRLF: a file whose bytes on disk are the blob the
// index holds is known by that id alone, whatever its line endings; a file
// that matches the index only through its filters (an LF blob checked out
// with CRLF, a clean filter), and a modified or untracked file, keep both
// ids. The repository's core.autocrlf=true makes this the Windows checkout
// on every OS.
func TestObserveCommittedCRLF(t *testing.T) {
	t.Parallel()
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	committedCRLF, committedLF := "committed with CRLF\n", "committed with LF\n"
	edited, staged, untracked := "edited after the commit\n", "staged with CRLF\n", "never added\n"
	filtered := "stored through a clean filter\n"

	tr := newRepo(t, false)
	tr.run("config", "filter.up.clean", "tr a-z A-Z")
	tr.write(".gitattributes", "*.up filter=up\n")
	tr.write("crlf.md", crlf(committedCRLF))
	tr.write("lf.md", committedLF)
	tr.write("edited.md", crlf(committedCRLF))
	tr.write("filtered.up", filtered)
	tr.run("add", ".gitattributes", "crlf.md", "lf.md", "edited.md", "filtered.up")
	tr.run("commit", "-q", "-m", "one")
	tr.run("config", "core.autocrlf", "true")
	// A fresh checkout: the LF blob comes out with CRLF, the CRLF blob as is.
	tr.remove("crlf.md")
	tr.remove("lf.md")
	tr.run("checkout", "--", "crlf.md", "lf.md")
	if got := tr.read("lf.md"); got != crlf(committedLF) {
		t.Fatalf("lf.md was checked out as %q, want CRLF", got)
	}
	tr.write("edited.md", crlf(edited))
	tr.write("staged.md", crlf(staged))
	tr.run("-c", "core.autocrlf=false", "add", "staged.md")
	tr.write("untracked.md", crlf(untracked))

	obs := tr.observe("crlf.md", "lf.md", "edited.md", "staged.md", "untracked.md", "filtered.up")
	for p, want := range map[string][]string{
		"crlf.md":      {oid(crlf(committedCRLF))},
		"lf.md":        {oid(crlf(committedLF)), oid(committedLF)},
		"edited.md":    {oid(crlf(edited)), oid(edited)},
		"staged.md":    {oid(crlf(staged))},
		"untracked.md": {oid(crlf(untracked)), oid(untracked)},
		"filtered.up":  {oid(filtered), oid(strings.ToUpper(filtered))},
	} {
		if got := obs[p].OIDs; obs[p].Kind != decide.Regular || !slices.Equal(got, want) {
			t.Errorf("%s: %+v, want Regular with OIDs %v", p, obs[p], want)
		}
	}
	if caseInsensitive(t, tr.dir) {
		// The index is read under the spelling the target uses.
		o := tr.observe("CRLF.md")["CRLF.md"]
		if o.ActualPath != "crlf.md" || !slices.Equal(o.OIDs, []string{oid(crlf(committedCRLF))}) {
			t.Errorf("CRLF.md: %+v, want crlf.md with its committed id", o)
		}
	}
}

func TestObserveNoRegularFilesRunsNoGit(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.mkdir("dir")
	tr.git = &gitx.Git{Dir: tr.dir, Bin: filepath.Join(t.TempDir(), "no-such-git")}
	tr.observe("dir", "missing.md")
}

func TestObserveGitFailure(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("a.md", "a\n")
	tr.git = &gitx.Git{Dir: tr.dir, Bin: filepath.Join(t.TempDir(), "no-such-git")}
	if _, err := Observe(t.Context(), tr.target(), []string{"a.md"}); err == nil {
		t.Error("Observe with a broken git: want error")
	}
}

// TestObserveRootBelowWorkTree guards against hashing the wrong files: git
// resolves --stdin-paths from the work tree root, not from Root.
func TestObserveRootBelowWorkTree(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, false)
	tr.write("sub/a.md", "sub\n")
	tr.write("a.md", "top\n")
	sub := Target{Root: tr.abs("sub"), Git: &gitx.Git{Dir: tr.abs("sub"), Env: tr.git.Env}}
	_, err := Observe(t.Context(), sub, []string{"a.md"})
	if err == nil || !strings.Contains(err.Error(), "not the git work tree root") {
		t.Errorf("Observe below the work tree root: %v, want error", err)
	}
}

func TestObserveMissingRoot(t *testing.T) {
	t.Parallel()
	_, err := Observe(t.Context(), Target{Root: filepath.Join(t.TempDir(), "missing")}, []string{"a.md"})
	if err == nil {
		t.Error("Observe on a missing root: want error")
	}
}

func TestObserveCancelled(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Observe(ctx, tr.target(), []string{"a.md"}); err == nil {
		t.Error("Observe with a cancelled context: want error")
	}
}

// TestObserveNameRejectedByOS covers a name that is a valid repository path
// but that the filesystem cannot address (a segment longer than 255
// characters): it must be unsafe, not an error and not absent (a write
// would fail or land elsewhere).
func TestObserveNameRejectedByOS(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	name := strings.Repeat("a", 300) + ".md"
	o := tr.observe(name)[name]
	if o.Kind != decide.InvalidPath || !strings.HasPrefix(o.Detail, "cannot inspect: ") {
		t.Errorf("got %+v, want InvalidPath with the OS error", o)
	}
}
