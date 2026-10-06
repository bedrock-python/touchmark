package gitx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRawOID(t *testing.T) {
	t.Parallel()
	for content, want := range map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	} {
		if got := RawOID([]byte(content)); got != want {
			t.Errorf("RawOID(%q) = %s, want %s", content, got, want)
		}
		got, err := RawOIDReader(strings.NewReader(content), int64(len(content)))
		if err != nil || got != want {
			t.Errorf("RawOIDReader(%q) = %s, %v; want %s", content, got, err, want)
		}
		got, err = RawOIDReader(iotest.OneByteReader(strings.NewReader(content)), int64(len(content)))
		if err != nil || got != want {
			t.Errorf("RawOIDReader(one byte at a time %q) = %s, %v; want %s", content, got, err, want)
		}
	}
}

func TestRawOIDReaderSizeMismatch(t *testing.T) {
	t.Parallel()
	if _, err := RawOIDReader(strings.NewReader("abc"), 4); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short content: %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := RawOIDReader(strings.NewReader("abcd"), 3); err == nil {
		t.Error("long content: want error")
	}
	if _, err := RawOIDReader(strings.NewReader(""), -1); err == nil {
		t.Error("negative size: want error")
	}
	boom := errors.New("boom")
	if _, err := RawOIDReader(iotest.ErrReader(boom), 1); !errors.Is(err, boom) {
		t.Errorf("read error: %v, want boom", err)
	}
}

// TestHashPathsCRLF is the Windows checkout case: with core.autocrlf=true
// the file on disk has CRLF while the committed blob has LF. HashPaths must
// give the committed id; the raw bytes hash differently.
func TestHashPathsCRLF(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, true)
	lf := "line one\nline two\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	r.write("docs/guide.md", lf)
	r.write("keep.bin", "raw\r\nbytes\r\n")
	r.write(".gitattributes", "*.bin -text\n")
	r.write("dir with space/файл.txt", lf)
	r.commit("one")

	committed := r.git("rev-parse", "HEAD:docs/guide.md")
	if committed != RawOID([]byte(lf)) {
		t.Fatalf("committed blob %s is not the LF content", committed)
	}
	r.writeFile("docs/guide.md", []byte(crlf))
	r.writeFile("dir with space/файл.txt", []byte(crlf))

	paths := []string{"docs/guide.md", "keep.bin", "dir with space/файл.txt", "docs/guide.md"}
	ids, err := r.g.HashPaths(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{committed, RawOID([]byte("raw\r\nbytes\r\n")), committed, committed}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("HashPaths = %v\nwant %v", ids, want)
	}
	if raw := RawOID([]byte(crlf)); raw == committed {
		t.Errorf("RawOID of CRLF bytes %s equals the committed LF blob", raw)
	}
}

func TestHashPathsDoesNotWrite(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	content := "never stored anywhere else\n"
	r.writeFile("new.txt", []byte(content))
	ids, err := r.g.HashPaths(t.Context(), []string{"new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != RawOID([]byte(content)) {
		t.Fatalf("HashPaths = %v", ids)
	}
	if err := r.tryGit("cat-file", "-e", ids[0]); err == nil {
		t.Error("HashPaths wrote the object")
	}
}

func TestHashPathsEdgeCases(t *testing.T) {
	t.Parallel()
	// No paths: no git process at all.
	g := &Git{Bin: "touchmark-no-such-git-binary"}
	if ids, err := g.HashPaths(t.Context(), nil); err != nil || len(ids) != 0 {
		t.Errorf("HashPaths(nil) = %v, %v", ids, err)
	}

	r := newTestRepo(t, false)
	for _, bad := range []string{"", "/abs", "nul\x00byte", filepath.Join(r.dir, "x")} {
		if _, err := r.g.HashPaths(t.Context(), []string{bad}); err == nil {
			t.Errorf("HashPaths(%q): want error", bad)
		}
	}
	var gitErr *Error
	if _, err := r.g.HashPaths(t.Context(), []string{"missing.txt"}); !errors.As(err, &gitErr) {
		t.Errorf("HashPaths(missing) = %v, want *Error", err)
	}

	// Names that --stdin-paths would misread unless quoted. Windows cannot
	// create them; skip those there.
	for _, name := range []string{`"quoted".txt`, "back\\slash", "tab\there", "new\nline"} {
		content := []byte("content of " + name)
		if err := os.WriteFile(filepath.Join(r.dir, name), content, 0o644); err != nil {
			t.Logf("skip %q: %v", name, err)
			continue
		}
		ids, err := r.g.HashPaths(t.Context(), []string{name})
		if err != nil || len(ids) != 1 || ids[0] != RawOID(content) {
			t.Errorf("HashPaths(%q) = %v, %v; want %s", name, ids, err, RawOID(content))
		}
	}
}

func TestQuoteStdinPath(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain/path.txt":   "plain/path.txt",
		"with space":       "with space",
		`back\slash`:       `back\slash`,
		`"lead`:            `"\"lead"`,
		`"a\b"`:            `"\"a\\b\""`,
		"tab\there":        `"tab\011here"`,
		"new\nline":        `"new\012line"`,
		"trail\r":          `"trail\015"`,
		"юникод":           "юникод",
		"mid\"quote":       "mid\"quote",
		"del\x7f":          `"del\177"`,
		"\x01leading ctrl": `"\001leading ctrl"`,
	} {
		if got := quoteStdinPath(in); got != want {
			t.Errorf("quoteStdinPath(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestHashPathsMatchesAdd(t *testing.T) {
	t.Parallel()
	// For a spread of contents, HashPaths agrees with what `git add` stores.
	for _, autocrlf := range []bool{false, true} {
		r := newTestRepo(t, autocrlf)
		contents := map[string][]byte{
			"lf.txt":    []byte("a\nb\n"),
			"crlf.txt":  []byte("a\r\nb\r\n"),
			"mixed.txt": []byte("a\r\nb\n"),
			"bin.dat":   {0, '\r', '\n', 1},
			"empty":     {},
		}
		var paths []string
		for p, c := range contents {
			r.writeFile(p, c)
			paths = append(paths, p)
		}
		ids, err := r.g.HashPaths(t.Context(), paths)
		if err != nil {
			t.Fatal(err)
		}
		r.git("add", "-A")
		for i, p := range paths {
			staged := r.git("rev-parse", ":"+p)
			if ids[i] != staged {
				t.Errorf("autocrlf=%v %s: HashPaths %s, git add %s", autocrlf, p, ids[i], staged)
			}
			if !autocrlf && ids[i] != RawOID(contents[p]) {
				t.Errorf("autocrlf=false %s: HashPaths %s != RawOID", p, ids[i])
			}
		}
	}
}
