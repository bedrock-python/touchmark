package apply

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

func readBlob(t *testing.T, src BlobSource, id string) (string, error) {
	t.Helper()
	rc, err := src.Open(id)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func TestGitBlobs(t *testing.T) {
	t.Parallel()
	tr := newRepo(t, true)
	content := "line one\r\nline two\n"
	out, err := tr.git.Run(t.Context(), strings.NewReader(content), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(out))
	b, err := tr.git.OpenBlobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	src := GitBlobs{B: b}

	if got, err := readBlob(t, src, id); err != nil || got != content {
		t.Errorf("Open(%s) = %q, %v; want %q", id, got, err, content)
	}
	missing := strings.Repeat("0", 40)
	if _, err := readBlob(t, src, missing); !errors.Is(err, gitx.ErrNotFound) {
		t.Errorf("Open(missing) = %v, want gitx.ErrNotFound", err)
	}
	if _, err := readBlob(t, GitBlobs{}, id); err == nil {
		t.Error("Open without a reader: want error")
	}
}

func TestDirBlobs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(rel, content string) string {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return full
	}
	write(filepath.Join("packs", "base", "a.md"), "relative\n")
	abs := write("absolute.md", "absolute\n")
	if err := os.Mkdir(filepath.Join(dir, "a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := DirBlobs{Dir: dir, Paths: map[string]string{
		oid("relative\n"): filepath.Join("packs", "base", "a.md"),
		oid("absolute\n"): abs,
		oid("dir"):        "a-dir",
		oid("gone"):       "gone.md",
	}}
	for id, want := range map[string]string{oid("relative\n"): "relative\n", oid("absolute\n"): "absolute\n"} {
		if got, err := readBlob(t, src, id); err != nil || got != want {
			t.Errorf("Open(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
	if _, err := readBlob(t, src, oid("unknown")); !errors.Is(err, gitx.ErrNotFound) {
		t.Errorf("Open(unknown) = %v, want gitx.ErrNotFound", err)
	}
	if _, err := readBlob(t, src, oid("dir")); err == nil {
		t.Error("Open(directory): want error")
	}
	if _, err := readBlob(t, src, oid("gone")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open(gone) = %v, want os.ErrNotExist", err)
	}
}
