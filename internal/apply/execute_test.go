package apply

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
)

func create(p, to, mode string) decide.Entry {
	return decide.Entry{Path: p, State: decide.Missing, Action: decide.Create, To: to, Mode: mode}
}

func update(p, from, to, mode string) decide.Entry {
	return decide.Entry{Path: p, State: decide.Outdated, Action: decide.Update, From: from, To: to, Mode: mode}
}

func del(p, from string) decide.Entry {
	return decide.Entry{Path: p, State: decide.Retired, Action: decide.Delete, From: from}
}

func TestExecuteCreateNested(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	blobs := memBlobs{}
	res := tr.execute(blobs,
		create("a/b/c.md", blobs.add("nested\n"), modeFile),
		create("top.md", blobs.add("top\n"), modeFile),
		create("a/sibling.md", blobs.add("sibling\n"), modeFile),
	)
	wantDone(t, res)
	for p, want := range map[string]string{"a/b/c.md": "nested\n", "top.md": "top\n", "a/sibling.md": "sibling\n"} {
		if got := tr.read(p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
}

func TestExecuteUpdate(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("docs/a.md", "v1\n")
	blobs := memBlobs{}
	wantDone(t, tr.execute(blobs, update("docs/a.md", oid("v1\n"), blobs.add("v2\n"), modeFile)))
	if got := tr.read("docs/a.md"); got != "v2\n" {
		t.Errorf("docs/a.md = %q, want v2", got)
	}
}

// TestExecuteThroughFilters: a CRLF checkout of an LF blob still holds the
// planned From, which only the filtered id shows.
func TestExecuteThroughFilters(t *testing.T) {
	t.Parallel()
	lf := "line one\nline two\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	tr := newRepo(t, true)
	tr.write("update.md", crlf)
	tr.write("delete.md", crlf)
	blobs := memBlobs{}
	wantDone(t, tr.execute(blobs,
		del("delete.md", oid(lf)),
		update("update.md", oid(lf), blobs.add("new\n"), modeFile),
	))
	if got := tr.read("update.md"); got != "new\n" {
		t.Errorf("update.md = %q, want new", got)
	}
	if tr.lstat("delete.md") != nil {
		t.Error("delete.md still exists")
	}

	// Without git only raw ids count, so the same plan is skipped.
	plain := newTree(t)
	plain.write("update.md", crlf)
	wantSkipped(t, plain.execute(blobs, update("update.md", oid(lf), oid("new\n"), modeFile))[0], "content differs")
}

func TestExecuteDeleteRemovesEmptyParents(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("a/b/c/x.md", "x\n")
	tr.write("a/b/c/y.md", "y\n")
	tr.write("a/keep.md", "keep\n")
	tr.write("top.md", "top\n")
	tr.write("only/child.md", "child\n")
	wantDone(t, tr.execute(nil,
		del("a/b/c/x.md", oid("x\n")),
		del("a/b/c/y.md", oid("y\n")),
		del("only/child.md", oid("child\n")),
		del("top.md", oid("top\n")),
	))
	for _, p := range []string{"a/b/c/x.md", "a/b/c", "a/b", "only", "top.md"} {
		if tr.lstat(p) != nil {
			t.Errorf("%s still exists", p)
		}
	}
	if got := tr.read("a/keep.md"); got != "keep\n" {
		t.Errorf("a/keep.md = %q", got)
	}
	if tr.lstat("") == nil {
		t.Error("the root was removed")
	}
}

// TestExecuteChangedSinceObserved: every action re-checks what Observe saw
// and leaves a path that changed in the meantime untouched.
func TestExecuteChangedSinceObserved(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("delete.md", "v1\n")
	tr.write("update.md", "v1\n")
	tr.write("gone.md", "v1\n")
	obs := tr.observe("delete.md", "update.md", "gone.md", "create.md")
	if obs["create.md"].Kind != decide.Absent {
		t.Fatalf("create.md: %+v", obs["create.md"])
	}
	blobs := memBlobs{}
	v2 := blobs.add("v2\n")
	plan := []decide.Entry{
		del("delete.md", obs["delete.md"].OIDs[0]),
		update("update.md", obs["update.md"].OIDs[0], v2, modeFile),
		update("gone.md", obs["gone.md"].OIDs[0], v2, modeFile),
		create("create.md", v2, modeFile),
	}

	tr.write("delete.md", "edited\n")
	tr.write("update.md", "edited\n")
	tr.remove("gone.md")
	tr.write("create.md", "written meanwhile\n")

	res := tr.execute(blobs, plan...)
	wantSkipped(t, res[0], "changed since observed: the content differs")
	wantSkipped(t, res[1], "changed since observed: the content differs")
	wantSkipped(t, res[2], "changed since observed: the file is gone")
	wantSkipped(t, res[3], "changed since observed: the path is a file")
	for p, want := range map[string]string{"delete.md": "edited\n", "update.md": "edited\n", "create.md": "written meanwhile\n"} {
		if got := tr.read(p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if tr.lstat("gone.md") != nil {
		t.Error("gone.md was recreated")
	}
}

// TestExecuteChangedDuringWrite: a change that lands while the temp file is
// being written is caught by the check right before the rename.
func TestExecuteChangedDuringWrite(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("update.md", "v1\n")
	blobs := memBlobs{}
	v2 := blobs.add("v2\n")
	src := blobFunc(func(id string) (io.ReadCloser, error) {
		tr.write("update.md", "edited meanwhile\n")
		tr.write("create.md", "written meanwhile\n")
		return blobs.Open(id)
	})
	res := tr.execute(src, update("update.md", oid("v1\n"), v2, modeFile), create("create.md", v2, modeFile))
	wantSkipped(t, res[0], "the content differs")
	wantSkipped(t, res[1], "the path is a file")
	if got := tr.read("update.md"); got != "edited meanwhile\n" {
		t.Errorf("update.md = %q", got)
	}
	if got := tr.read("create.md"); got != "written meanwhile\n" {
		t.Errorf("create.md = %q", got)
	}
}

// TestExecuteSymlinkedParentAfterObserve: a directory replaced by a symlink
// after Observe must not be written through.
func TestExecuteSymlinkedParentAfterObserve(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("d/existing.md", "v1\n")
	obs := tr.observe("d/new.md", "d/existing.md", "e/new.md")
	if obs["d/new.md"].Kind != decide.Absent || obs["e/new.md"].Kind != decide.Absent {
		t.Fatalf("observations: %+v", obs)
	}
	tr.remove("d")
	tr.write("real/existing.md", "v1\n")
	tr.symlink("real", "d")
	tr.symlink("real", "e")

	blobs := memBlobs{}
	v2 := blobs.add("v2\n")
	res := tr.execute(blobs,
		create("d/new.md", v2, modeFile),
		update("d/existing.md", oid("v1\n"), v2, modeFile),
		del("d/existing.md", oid("v1\n")),
		create("e/new.md", v2, modeFile),
	)
	wantSkipped(t, res[0], "parent d is a symlink")
	wantSkipped(t, res[1], "parent d is a symlink")
	wantSkipped(t, res[2], "parent d is a symlink")
	wantSkipped(t, res[3], "parent e is a symlink")
	if tr.lstat("real/new.md") != nil {
		t.Error("real/new.md was written through the symlink")
	}
	if got := tr.read("real/existing.md"); got != "v1\n" {
		t.Errorf("real/existing.md = %q, want v1", got)
	}
}

// TestExecuteSymlinkSwappedIn: a file replaced by a symlink to identical
// content after Observe is neither followed nor replaced.
func TestExecuteSymlinkSwappedIn(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("a.md", "v1\n")
	tr.write("real.md", "v1\n")
	tr.observe("a.md")
	tr.remove("a.md")
	tr.symlink("real.md", "a.md")

	blobs := memBlobs{}
	v2 := blobs.add("v2\n")
	res := tr.execute(blobs,
		del("a.md", oid("v1\n")),
		update("a.md", oid("v1\n"), v2, modeFile),
		decide.Entry{Path: "a.md", State: decide.Local, Action: decide.Adopt, To: v2, Mode: modeFile},
		decide.Entry{Path: "a.md", State: decide.Current, Action: decide.Chmod, From: oid("v1\n"), Mode: modeExec},
	)
	for _, r := range res {
		wantSkipped(t, r, "now a symlink")
	}
	if fi := tr.lstat("a.md"); fi == nil || fi.Mode().Type()&fs.ModeSymlink == 0 {
		t.Error("a.md is no longer the symlink")
	}
	if got := tr.read("real.md"); got != "v1\n" {
		t.Errorf("real.md = %q, want v1", got)
	}
}

func TestExecuteAdopt(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("local.md", "our own\n")
	blobs := memBlobs{}
	v2 := blobs.add("pack version\n")
	adopt := func(p string) decide.Entry {
		return decide.Entry{Path: p, State: decide.Local, Action: decide.Adopt, To: v2, Mode: modeFile}
	}
	res := tr.execute(blobs, adopt("local.md"), adopt("gone.md"))
	wantDone(t, res[:1])
	wantSkipped(t, res[1], "the file is gone")
	if got := tr.read("local.md"); got != "pack version\n" {
		t.Errorf("local.md = %q", got)
	}
	if tr.lstat("gone.md") != nil {
		t.Error("adopt created gone.md")
	}
}

// TestExecuteAtomicWrite: when the blob cannot be written in full, the
// existing file keeps its content and no temp file is left.
func TestExecuteAtomicWrite(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	v2 := oid("v2\n")
	for name, tc := range map[string]struct {
		blobs BlobSource
		err   string
	}{
		"open fails": {
			blobFunc(func(string) (io.ReadCloser, error) { return nil, boom }), "boom",
		},
		"read fails midway": {
			blobFunc(func(string) (io.ReadCloser, error) {
				return io.NopCloser(io.MultiReader(strings.NewReader("v"), iotest.ErrReader(boom))), nil
			}), "boom",
		},
		"wrong content": {
			blobFunc(func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("something else\n")), nil
			}), "hashes to",
		},
		"close fails": {
			blobFunc(func(string) (io.ReadCloser, error) {
				return failingCloser{strings.NewReader("v2\n"), boom}, nil
			}), "boom",
		},
		"no blob source": {nil, "no blob source"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := newTree(t)
			tr.write("dir/a.md", "v1\n")
			res := tr.execute(tc.blobs,
				update("dir/a.md", oid("v1\n"), v2, modeFile),
				create("dir/new.md", v2, modeFile),
				create("fresh/new.md", v2, modeFile),
			)
			for _, r := range res {
				wantErr(t, r, tc.err)
			}
			if got := tr.read("dir/a.md"); got != "v1\n" {
				t.Errorf("dir/a.md = %q, want v1", got)
			}
			if tr.lstat("dir/new.md") != nil || tr.lstat("fresh") != nil {
				t.Error("a failed create left a file or the directory it created")
			}
			if tr.lstat("dir") == nil {
				t.Error("a failed create removed a directory it did not create")
			}
		})
	}
}

type failingCloser struct {
	io.Reader
	err error
}

func (f failingCloser) Close() error { return f.err }

func TestExecuteModes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the executable bit is not recorded on Windows")
	}
	tr := newTree(t)
	tr.writeMode("was-exec.sh", "v1\n", 0o755)
	tr.writeMode("chmod.sh", "run\n", 0o644)
	tr.writeMode("private.sh", "run\n", 0o600)
	tr.writeMode("unchmod.sh", "run\n", 0o755)
	blobs := memBlobs{}
	script := blobs.add("#!/bin/sh\n")
	chmod := func(p, mode string) decide.Entry {
		return decide.Entry{Path: p, State: decide.Current, Action: decide.Chmod, From: oid("run\n"), Mode: mode}
	}
	wantDone(t, tr.execute(blobs,
		create("bin/run.sh", script, modeExec),
		create("plain.txt", script, modeFile),
		update("was-exec.sh", oid("v1\n"), script, modeFile),
		chmod("chmod.sh", modeExec),
		chmod("private.sh", modeExec),
		chmod("unchmod.sh", modeFile),
	))
	for p, want := range map[string]fs.FileMode{
		"bin/run.sh":  0o755,
		"plain.txt":   0o644,
		"was-exec.sh": 0o644,
		"chmod.sh":    0o755,
		"private.sh":  0o700,
		"unchmod.sh":  0o644,
	} {
		if got := tr.lstat(p).Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", p, got, want)
		}
	}
	obs := tr.observe("bin/run.sh", "chmod.sh", "private.sh")
	for p, o := range obs {
		if o.Mode != modeExec {
			t.Errorf("%s observed as %q after the write, want %s", p, o.Mode, modeExec)
		}
	}
	wantSkipped(t, tr.execute(nil, chmod("plain.txt", modeExec))[0], "content differs")
}

func TestExecuteKeepAndCancel(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("delete.md", "v1\n")
	blobs := memBlobs{}
	v2 := blobs.add("v2\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	keep := decide.Entry{Path: "keep.md", State: decide.Current, Action: decide.Keep}
	res := Execute(ctx, tr.target(), decide.Plan{Entries: []decide.Entry{
		del("delete.md", oid("v1\n")), keep, create("new.md", v2, modeFile),
	}}, blobs)
	if !res[1].Done || res[1].Err != nil {
		t.Errorf("keep: %+v, want done", res[1])
	}
	for _, r := range []Result{res[0], res[2]} {
		if r.Done || !errors.Is(r.Err, context.Canceled) {
			t.Errorf("%s %s: %+v, want context.Canceled", r.Entry.Action, r.Entry.Path, r)
		}
	}
	if tr.lstat("new.md") != nil || tr.lstat("delete.md") == nil {
		t.Error("a cancelled Execute touched the tree")
	}
}

// TestExecuteCancelStopsBeforeNextEntry cancels from inside the blob source:
// the running entry finishes, the next ones do not run.
func TestExecuteCancelStopsBeforeNextEntry(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	blobs := memBlobs{}
	v1 := blobs.add("v1\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	src := blobFunc(func(id string) (io.ReadCloser, error) {
		cancel()
		return blobs.Open(id)
	})
	res := Execute(ctx, tr.target(), decide.Plan{Entries: []decide.Entry{
		create("a.md", v1, modeFile), create("b.md", v1, modeFile),
	}}, src)
	wantDone(t, res[:1])
	if !errors.Is(res[1].Err, context.Canceled) || tr.lstat("b.md") != nil {
		t.Errorf("b.md: %+v, want context.Canceled and no file", res[1])
	}
}

func TestExecuteFailureDoesNotStopOthers(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	blobs := memBlobs{}
	res := tr.execute(blobs,
		create("a.md", oid("not in the source\n"), modeFile),
		create("b.md", blobs.add("b\n"), modeFile),
	)
	wantErr(t, res[0], "not found")
	if !errors.Is(res[0].Err, gitx.ErrNotFound) {
		t.Errorf("a.md: %v, want gitx.ErrNotFound", res[0].Err)
	}
	wantDone(t, res[1:])
}

func TestExecuteInvalidEntries(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("a.md", "a\n")
	blobs := memBlobs{}
	v := blobs.add("v\n")
	res := tr.execute(blobs,
		create("../escape.md", v, modeFile),
		create(".git/hooks/pre-commit", v, modeExec),
		decide.Entry{Path: "a.md", Action: decide.Update, To: v},
		decide.Entry{Path: "a.md", Action: decide.Delete},
		decide.Entry{Path: "b.md", Action: decide.Create},
		decide.Entry{Path: "a.md", Action: "rename"},
	)
	for _, r := range res {
		if r.Done || r.Err == nil {
			t.Errorf("%s %q: %+v, want an error", r.Entry.Action, r.Entry.Path, r)
		}
	}
	if got := tr.read("a.md"); got != "a\n" {
		t.Errorf("a.md = %q", got)
	}
}

func TestExecuteMissingRoot(t *testing.T) {
	t.Parallel()
	target := Target{Root: filepath.Join(t.TempDir(), "missing")}
	res := Execute(t.Context(), target, decide.Plan{Entries: []decide.Entry{
		{Path: "keep.md", Action: decide.Keep},
		create("a.md", oid("a\n"), modeFile),
	}}, memBlobs{})
	if !res[0].Done || res[1].Err == nil {
		t.Errorf("results %+v, want keep done and create failed", res)
	}
}

// TestExecuteRunsDeletesFirst: a hand-made plan that lists an AfterDeletes
// create before its delete still works, and results stay in plan order.
func TestExecuteRunsDeletesFirst(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write("docs", "old docs file\n")
	blobs := memBlobs{}
	c := create("docs/x.md", blobs.add("x\n"), modeFile)
	c.AfterDeletes = true
	res := tr.execute(blobs, c, del("docs", oid("old docs file\n")))
	wantDone(t, res)
	if res[0].Entry.Path != "docs/x.md" || res[1].Entry.Path != "docs" {
		t.Errorf("results out of plan order: %+v", res)
	}
	if got := tr.read("docs/x.md"); got != "x\n" {
		t.Errorf("docs/x.md = %q", got)
	}
}

func TestExecuteRootBelowWorkTree(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, false)
	tr.write("sub/a.md", "local\n")
	sub := Target{Root: tr.abs("sub"), Git: &gitx.Git{Dir: tr.abs("sub"), Env: tr.git.Env}}
	res := Execute(t.Context(), sub, decide.Plan{Entries: []decide.Entry{
		del("a.md", oid("v1\n")),
	}}, nil)
	wantErr(t, res[0], "not the git work tree root")
	if got := tr.read("sub/a.md"); got != "local\n" {
		t.Errorf("sub/a.md = %q", got)
	}
}
