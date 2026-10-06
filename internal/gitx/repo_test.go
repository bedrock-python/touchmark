package gitx

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestIndexEntries(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("a.txt", "a\n")
	r.write("dir/b.sh", "b\n")
	r.git("update-index", "--chmod=+x", "dir/b.sh")
	link := r.setEntry("120000", "a.txt", "link")
	r.commit("one")
	sub := r.git("rev-parse", "HEAD")
	r.setEntry("160000", sub, "sub")

	got, err := r.g.IndexEntries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []IndexEntry{
		{Mode: "100644", OID: RawOID([]byte("a\n")), Path: "a.txt"},
		{Mode: "100755", OID: RawOID([]byte("b\n")), Path: "dir/b.sh"},
		{Mode: "120000", OID: link, Path: "link"},
		{Mode: "160000", OID: sub, Path: "sub"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("IndexEntries = %+v\nwant %+v", got, want)
	}

	// Relative to Dir, not to the top level.
	sg := &Git{Dir: filepath.Join(r.dir, "dir"), Env: r.g.Env}
	got, err = sg.IndexEntries(t.Context())
	if err != nil || len(got) != 1 || got[0].Path != "b.sh" {
		t.Errorf("IndexEntries in dir = %+v, %v", got, err)
	}
}

func TestParseIndex(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	got, err := parseIndex([]byte("100644 " + oid + " 2\tx y\x00"))
	if err != nil || len(got) != 1 || got[0] != (IndexEntry{Mode: "100644", OID: oid, Stage: 2, Path: "x y"}) {
		t.Errorf("parseIndex = %+v, %v", got, err)
	}
	for _, bad := range []string{
		"100644 " + oid + "\tx\x00",
		"100644 " + oid + " 4\tx\x00",
		"100644 xyz 0\tx\x00",
		"100644 " + oid + " 0\t\x00",
		"100644 " + oid + " 0 x\x00",
	} {
		if _, err := parseIndex([]byte(bad)); err == nil {
			t.Errorf("parseIndex(%q): want error", bad)
		}
	}
}

func TestConfigBool(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.git("config", "touchmark.yes", "on")
	r.git("config", "touchmark.no", "0")
	for key, want := range map[string]bool{"touchmark.yes": true, "touchmark.no": false} {
		if got, err := r.g.ConfigBool(t.Context(), key, !want); err != nil || got != want {
			t.Errorf("ConfigBool(%s) = %v, %v; want %v", key, got, err, want)
		}
	}
	for _, def := range []bool{false, true} {
		if got, err := r.g.ConfigBool(t.Context(), "touchmark.unset", def); err != nil || got != def {
			t.Errorf("ConfigBool(unset, %v) = %v, %v", def, got, err)
		}
	}
	r.git("config", "touchmark.bad", "maybe")
	if _, err := r.g.ConfigBool(t.Context(), "touchmark.bad", false); err == nil {
		t.Error("ConfigBool(maybe): want error")
	}
}

func TestInsideGitDir(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	bare := newTestRepo(t, false, "--bare")
	plain := t.TempDir()
	env := append(r.g.Env[:len(r.g.Env):len(r.g.Env)], "GIT_CEILING_DIRECTORIES="+filepath.Dir(plain))
	for dir, want := range map[string]bool{
		r.dir:                                 false,
		filepath.Join(r.dir, ".git"):          true,
		filepath.Join(r.dir, ".git", "hooks"): true,
		bare.dir:                              true,
		plain:                                 false,
	} {
		g := &Git{Dir: dir, Env: env}
		if got, err := g.InsideGitDir(t.Context()); err != nil || got != want {
			t.Errorf("InsideGitDir(%s) = %v, %v; want %v", dir, got, err, want)
		}
	}
}

func TestIsPartialClone(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("a.txt", "a\n")
	r.commit("one")
	if got, err := r.g.IsPartialClone(t.Context()); err != nil || got {
		t.Errorf("IsPartialClone(full) = %v, %v", got, err)
	}
	r.git("config", "remote.origin.promisor", "true")
	if got, err := r.g.IsPartialClone(t.Context()); err != nil || !got {
		t.Errorf("IsPartialClone(promisor remote) = %v, %v", got, err)
	}
	r.git("config", "remote.origin.promisor", "false")
	if got, err := r.g.IsPartialClone(t.Context()); err != nil || got {
		t.Errorf("IsPartialClone(promisor=false) = %v, %v", got, err)
	}

	// A real blobless clone.
	r.git("config", "uploadpack.allowFilter", "true")
	clone := t.TempDir()
	r.git("clone", "-q", "--filter=blob:none", "--no-checkout", fileURL(r.dir), clone)
	g := &Git{Dir: clone, Env: r.g.Env}
	if got, err := g.IsPartialClone(t.Context()); err != nil || !got {
		t.Errorf("IsPartialClone(blobless clone) = %v, %v", got, err)
	}
}

func TestSetExecutable(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.git("config", "core.fileMode", "false")
	r.write("tracked.sh", "tracked\n")
	r.commit("one")
	r.writeFile("new.sh", []byte("new\n"))
	for _, p := range []string{"tracked.sh", "new.sh"} {
		if err := r.g.SetExecutable(t.Context(), p); err != nil {
			t.Fatalf("SetExecutable(%s): %v", p, err)
		}
	}
	entries, err := r.g.IndexEntries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Mode != "100755" {
			t.Errorf("%s: mode %s after SetExecutable", e.Path, e.Mode)
		}
	}
	if len(entries) != 2 {
		t.Errorf("index = %+v, want both files", entries)
	}
	var gitErr *Error
	if err := r.g.SetExecutable(t.Context(), "missing.sh"); !errors.As(err, &gitErr) {
		t.Errorf("SetExecutable(missing) = %v, want *Error", err)
	}
	if err := r.g.SetExecutable(t.Context(), "/abs"); err == nil {
		t.Error("SetExecutable(/abs): want error")
	}
}

func TestWritePaths(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, true)
	lf := "line one\nline two\n"
	r.writeFile("a.txt", []byte(strings.ReplaceAll(lf, "\n", "\r\n")))
	objects := t.TempDir()
	g := r.g.WithObjectDir(objects)
	ids, err := g.WritePaths(t.Context(), []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != RawOID([]byte(lf)) {
		t.Fatalf("WritePaths = %v, want the LF blob", ids)
	}
	b, err := g.OpenBlobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if data, err := b.Read(ids[0]); err != nil || string(data) != lf {
		t.Errorf("Read = %q, %v; want the cleaned content", data, err)
	}
	if err := r.tryGit("cat-file", "-e", ids[0]); err == nil {
		t.Error("WritePaths wrote into the repository's own object store")
	}
}

func TestHashPathsEach(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	for _, p := range []string{"a.txt", "c.txt", "e.txt"} {
		r.writeFile(p, []byte(p+"\n"))
	}
	paths := []string{"a.txt", "b-missing.txt", "c.txt", "d-missing.txt", "e.txt"}
	ids, errs, err := r.g.HashPathsEach(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		missing := strings.Contains(p, "missing")
		switch {
		case missing && (ids[i] != "" || errs[i] == nil):
			t.Errorf("%s: id %q, err %v; want an error", p, ids[i], errs[i])
		case !missing && (ids[i] != RawOID([]byte(p+"\n")) || errs[i] != nil):
			t.Errorf("%s: id %q, err %v; want its blob id", p, ids[i], errs[i])
		}
	}
	if ids, errs, err := r.g.HashPathsEach(t.Context(), nil); err != nil || len(ids) != 0 || len(errs) != 0 {
		t.Errorf("HashPathsEach(nil) = %v, %v, %v", ids, errs, err)
	}
	g := &Git{Dir: r.dir, Bin: "touchmark-no-such-git-binary"}
	if _, _, err := g.HashPathsEach(t.Context(), []string{"a.txt"}); err == nil {
		t.Error("HashPathsEach without git: want error")
	}
}

// TestHashPathsLongPath: a path longer than MAX_PATH hashes on Windows,
// because HashPaths enables core.longpaths.
func TestHashPathsLongPath(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	p := strings.Repeat("d", 60) + "/" + strings.Repeat("e", 60) + "/" + strings.Repeat("f", 60) + "/" +
		strings.Repeat("g", 60) + "/" + strings.Repeat("h", 60) + ".md"
	full := filepath.Join(r.dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Skipf("cannot create a long path: %v", err)
	}
	if err := os.WriteFile(full, []byte("long\n"), 0o644); err != nil {
		t.Skipf("cannot create a long path: %v", err)
	}
	ids, err := r.g.HashPaths(t.Context(), []string{p})
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Fatalf("HashPaths(long path): %v", err)
		}
		t.Skipf("the filesystem refuses the long path: %v", err)
	}
	if ids[0] != RawOID([]byte("long\n")) {
		t.Errorf("HashPaths(long path) = %s", ids[0])
	}
}

// The parsers of git output take arbitrary bytes; they must not panic, and
// what they return must be consistent with their input.

func FuzzParseHistory(f *testing.F) {
	oid := strings.Repeat("a", 40)
	f.Add("packs", "\x00"+oid+"\n\n:000000 100644 "+strings.Repeat("0", 40)+" "+oid+" A\x00packs/p/x\x00")
	f.Add("", ":100644 100644 "+oid+" "+oid+" R100\x00a\x00b\x00")
	f.Add("packs", "::100644 100644 100644 "+oid+" "+oid+" "+oid+" MM\x00packs/x\x00")
	f.Fuzz(func(t *testing.T, prefix, out string) {
		prefix = cleanPrefix(prefix)
		_ = parseHistory(bufio.NewReader(strings.NewReader(out)), prefix, func(v BlobVersion) error {
			if !isOID(v.OID) || v.Path == "" {
				t.Fatalf("reported %+v", v)
			}
			if prefix != "" && !under(v.Path, prefix) {
				t.Fatalf("reported %q outside %q", v.Path, prefix)
			}
			return nil
		})
	})
}

func FuzzParseLsTree(f *testing.F) {
	oid := strings.Repeat("b", 40)
	f.Add("packs", "100644 blob "+oid+"      12\tpacks/a\x00160000 commit "+oid+"       -\tpacks/s\x00")
	f.Add("", "garbage")
	f.Fuzz(func(t *testing.T, prefix, out string) {
		entries, err := parseLsTree([]byte(out), prefix)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !isOID(e.OID) || e.Path == "" || e.Size < -1 {
				t.Fatalf("parsed %+v", e)
			}
			if prefix != "" && !under(e.Path, prefix) {
				t.Fatalf("kept %q outside %q", e.Path, prefix)
			}
		}
	})
}

func FuzzParseObjectHeader(f *testing.F) {
	oid := strings.Repeat("c", 40)
	f.Add(oid+" blob 12", oid)
	f.Add(oid+" missing", oid)
	f.Add("x y z", oid)
	f.Fuzz(func(t *testing.T, line, want string) {
		h, err := parseObjectHeader(line, want)
		if err != nil {
			return
		}
		if h.oid != want || h.size < 0 || (h.missing && h.typ != "") {
			t.Fatalf("parseObjectHeader(%q, %q) = %+v", line, want, h)
		}
	})
}

func FuzzParseIndex(f *testing.F) {
	f.Add("100644 " + strings.Repeat("d", 40) + " 0\tpath\x00")
	f.Fuzz(func(t *testing.T, out string) {
		entries, err := parseIndex([]byte(out))
		if err != nil {
			return
		}
		for _, e := range entries {
			if !isOID(e.OID) || e.Path == "" || e.Stage < 0 || e.Stage > 3 {
				t.Fatalf("parsed %+v", e)
			}
		}
	})
}
