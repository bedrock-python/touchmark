package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// attrsFixture serves a tree whose attributes give filters, encodings and
// line endings, and returns a target that fetched it without blobs.
func attrsFixture(t *testing.T) (*served, *TargetRepo, string) {
	t.Helper()
	s := newServed(t, t.TempDir(), "repo.git")
	head := s.commit("main", nil, []file{
		{path: ".gitattributes", content: "*.bin filter=lfs diff=lfs merge=lfs -text\n*.txt text eol=crlf\n[attr]generated -diff linguist-generated\n*.gen generated\n"},
		{path: "docs/.gitattributes", content: "*.md working-tree-encoding=UTF-16LE text\n*.txt -text\n"},
		{path: "docs/deep/.gitattributes", mode: "120000", content: "../.gitattributes"},
		{path: "other/.gitattributes", content: "* filter=other\n"},
		{path: "a.bin", content: "bin"},
		{path: "docs/x.md", content: "x\n"},
	}, "attributes\n")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	return s, tr, head
}

func TestAttrs(t *testing.T) {
	t.Parallel()
	requireGit(t, gitCheckAttrSource, "check-attr --source")
	s, tr, head := attrsFixture(t)
	root, docs, deep := s.blob(head, ".gitattributes"), s.blob(head, "docs/.gitattributes"), s.blob(head, "docs/deep/.gitattributes")
	paths := []string{"a.bin", "docs/x.md", "b.txt", "docs/b.txt", "new/unknown.go", "x.gen", ".gitattributes"}
	attrs := []string{"filter", "working-tree-encoding", "text", "eol", "linguist-generated"}

	if lazyFetchOff(t) {
		// The attribute files are not fetched yet: git would read them as
		// empty, so Attrs refuses.
		_, err := tr.Attrs(t.Context(), head, paths, attrs...)
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), ".gitattributes") {
			t.Fatalf("Attrs(before FetchBlobs) = %v, want ErrNotFound naming the file", err)
		}
		// Only the files above the paths matter: other/.gitattributes is
		// still missing after this.
		if err := tr.FetchBlobs(t.Context(), []string{root, docs}); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Attrs(t.Context(), head, []string{"docs/deep/y.md"}, "text"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Attrs(docs/deep, symlinked .gitattributes missing) = %v", err)
		}
	}
	if err := tr.FetchBlobs(t.Context(), []string{root, docs, deep}); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{head, s.git("rev-parse", head+"^{tree}"), "refs/touchmark/remote/main"} {
		got, err := tr.Attrs(t.Context(), tree, paths, attrs...)
		if err != nil {
			t.Fatalf("Attrs(%s) = %v", tree, err)
		}
		u := "unspecified"
		want := map[string]map[string]string{
			"a.bin":          {"filter": "lfs", "working-tree-encoding": u, "text": "unset", "eol": u, "linguist-generated": u},
			"docs/x.md":      {"filter": u, "working-tree-encoding": "UTF-16LE", "text": "set", "eol": u, "linguist-generated": u},
			"b.txt":          {"filter": u, "working-tree-encoding": u, "text": "set", "eol": "crlf", "linguist-generated": u},
			"docs/b.txt":     {"filter": u, "working-tree-encoding": u, "text": "unset", "eol": "crlf", "linguist-generated": u},
			"new/unknown.go": {"filter": u, "working-tree-encoding": u, "text": u, "eol": u, "linguist-generated": u},
			"x.gen":          {"filter": u, "working-tree-encoding": u, "text": u, "eol": u, "linguist-generated": "set"},
			".gitattributes": {"filter": u, "working-tree-encoding": u, "text": u, "eol": u, "linguist-generated": u},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Attrs(%s) =\n%v\nwant\n%v", tree, got, want)
		}
	}
	if got, err := tr.Attrs(t.Context(), head, nil, "filter"); err != nil || len(got) != 0 {
		t.Errorf("Attrs(no paths) = %v, %v", got, err)
	}
	for name, call := range map[string]func() error{
		"no attrs":   func() error { _, err := tr.Attrs(t.Context(), head, paths); return err },
		"bad attr":   func() error { _, err := tr.Attrs(t.Context(), head, paths, "-text"); return err },
		"attr space": func() error { _, err := tr.Attrs(t.Context(), head, paths, "a b"); return err },
		"bad path":   func() error { _, err := tr.Attrs(t.Context(), head, []string{"../x"}, "text"); return err },
		"abs path":   func() error { _, err := tr.Attrs(t.Context(), head, []string{"/x"}, "text"); return err },
		"bad tree":   func() error { _, err := tr.Attrs(t.Context(), "--all", paths, "text"); return err },
		"no tree":    func() error { _, err := tr.Attrs(t.Context(), missingOID, paths, "text"); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("Attrs(%s) succeeded", name)
		}
	}
}

func TestRenormalized(t *testing.T) {
	t.Parallel()
	requireGit(t, gitAttrSource, "--attr-source")
	s := newServed(t, t.TempDir(), "repo.git")
	head := s.commit("main", nil, []file{
		{path: ".gitattributes", content: "* text=auto eol=crlf\n*.bin -text\n*.id ident\n*.norm text\n"},
		{path: "a.txt", content: "a\n"},
	}, "attributes\n")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	if lazyFetchOff(t) {
		if _, err := tr.Renormalized(t.Context(), head, "x.txt", []byte("x\n")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Renormalized(attributes missing) = %v, want ErrNotFound", err)
		}
	}
	if err := tr.FetchBlobs(t.Context(), []string{s.blob(head, ".gitattributes")}); err != nil {
		t.Fatal(err)
	}
	crlf, lf := []byte("one\r\ntwo\r\n"), []byte("one\ntwo\n")
	for _, c := range []struct {
		path    string
		content []byte
		want    string
	}{
		// text normalizes CRLF to LF: the target would rewrite the blob.
		{"y.norm", crlf, RawOID(lf)},
		// text=auto would too, for a file git adds anew; but the delivered
		// blob is in the index of every checkout, and for it auto keeps
		// CRLF (TestRenormalizedMatchesStatus).
		{"x.txt", crlf, RawOID(crlf)},
		// LF content is kept as is.
		{"x.txt", lf, RawOID(lf)},
		// -text keeps CRLF.
		{"x.bin", crlf, RawOID(crlf)},
		// ident collapses an expanded $Id$, text=auto or not.
		{"v.id", []byte("$Id: 0123456789abcdef $\n"), RawOID([]byte("$Id$\n"))},
		// Binary content is not text for text=auto.
		{"img.dat", []byte("\x00\x01\r\n"), RawOID([]byte("\x00\x01\r\n"))},
	} {
		got, err := tr.Renormalized(t.Context(), head, c.path, c.content)
		if err != nil || got != c.want {
			t.Errorf("Renormalized(%s, %q) = %s, %v; want %s", c.path, c.content, got, err, c.want)
		}
	}
	// Nothing was written.
	if lazyFetchOff(t) {
		if missing, err := tr.missing(t.Context(), []string{RawOID(lf)}); err != nil || len(missing) != 1 {
			t.Errorf("Renormalized wrote a blob: %v, %v", missing, err)
		}
	}
	if _, err := tr.Renormalized(t.Context(), head, "../x", lf); err == nil {
		t.Error("Renormalized(../x) succeeded")
	}
	if _, err := tr.Renormalized(t.Context(), "nope", "x.txt", lf); err == nil {
		t.Error("Renormalized(bad tree) succeeded")
	}
}

// TestAttributeFilesListedOnce: the renormalize check asks for every path
// of D, up to three rounds; the tree is listed once per tree id, not per
// call.
func TestAttributeFilesListedOnce(t *testing.T) {
	t.Parallel()
	requireGit(t, gitAttrSource, "--attr-source")
	s, tr, head := attrsFixture(t)
	if err := tr.FetchBlobs(t.Context(), []string{s.blob(head, ".gitattributes"), s.blob(head, "docs/.gitattributes"),
		s.blob(head, "docs/deep/.gitattributes"), s.blob(head, "other/.gitattributes")}); err != nil {
		t.Fatal(err)
	}
	var listings atomic.Int32
	tr.iso.trace = func(args, _ []string) {
		if slices.Contains(args, "ls-tree") {
			listings.Add(1)
		}
	}
	for _, p := range []string{"a.txt", "docs/b.md", "other/c", "docs/deep/d.md"} {
		if _, err := tr.Renormalized(t.Context(), head, p, []byte("x\r\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Attrs(t.Context(), head, []string{p}, "text"); err != nil {
			t.Fatal(err)
		}
	}
	if n := listings.Load(); n != 1 {
		t.Errorf("the tree was listed %d times, want once", n)
	}
}

// TestRenormalizedMatchesStatus delivers a file, checks the commit out,
// touches the file so that git reads it again, and asks git status: the
// file looks changed exactly when Renormalized said the target's
// attributes rewrite the blob (otherwise status would call the file
// local).
func TestRenormalizedMatchesStatus(t *testing.T) {
	t.Parallel()
	requireGit(t, gitAttrSource, "--attr-source")
	crlf, lf := "one\r\ntwo\r\n", "one\ntwo\n"
	for _, c := range []struct {
		name, attrs, content string
		rewritten            bool
	}{
		{"text=auto keeps CRLF", "* text=auto\n", crlf, false},
		{"text=auto eol=crlf keeps CRLF", "* text=auto eol=crlf\n", crlf, false},
		{"text=auto LF", "* text=auto\n", lf, false},
		{"text normalizes CRLF", "* text\n", crlf, true},
		{"text eol=crlf LF", "* text eol=crlf\n", lf, false},
		{"-text keeps CRLF", "* -text\n", crlf, false},
		{"ident collapses", "* ident\n", "$Id: 0123 $\n", true},
		{"no attributes", "# none\n", crlf, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newServed(t, t.TempDir(), "repo.git")
			b := s.commit("main", nil, []file{{path: ".gitattributes", content: c.attrs}}, "attributes\n")
			tr := newTarget(t, s.dir, Auth{})
			if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
				t.Fatal(err)
			}
			if err := tr.FetchBlobs(t.Context(), []string{s.blob(b, ".gitattributes")}); err != nil {
				t.Fatal(err)
			}
			own := RawOID([]byte(c.content))
			built, err := tr.BuildCommit(t.Context(), CommitSpec{Parent: b, Changes: []Change{{Path: "f.txt", Mode: "100644", OID: own}},
				Blobs: hubBlobs(c.content), Author: bot, Committer: bot, When: when, Message: msg})
			if err != nil {
				t.Fatal(err)
			}
			id, err := tr.Renormalized(t.Context(), built.Tree, "f.txt", []byte(c.content))
			if err != nil {
				t.Fatal(err)
			}
			if res, err := tr.Push(t.Context(), PushSpec{Branch: "sync", Commit: built.Commit}); err != nil || res.Status != PushOK {
				t.Fatalf("Push() = %+v, %v", res, err)
			}
			wt := filepath.Join(t.TempDir(), "wt")
			g := &Git{Env: testEnv(t)}
			if _, err := g.Run(t.Context(), nil, "-c", "core.autocrlf=false", "clone", "-q", "--branch", "sync", s.dir, wt); err != nil {
				t.Fatal(err)
			}
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(wt, "f.txt"), later, later); err != nil {
				t.Fatal(err)
			}
			out, err := (&Git{Dir: wt, Env: testEnv(t)}).Run(t.Context(), nil, "-c", "core.autocrlf=false", "status", "--porcelain", "--", "f.txt")
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.TrimSpace(string(out)) != ""
			if rewritten := id != own; rewritten != changed || rewritten != c.rewritten {
				t.Errorf("Renormalized says rewritten=%v (want %v), git status says changed=%v (%q)", rewritten, c.rewritten, changed, out)
			}
		})
	}
}

// TestAttrsPinnedSettings: `git init` writes core.ignorecase=true on
// Windows and macOS and leaves it out on Linux; the isolation pins it, so
// that attributes, and so the unsafe set, D and the content key, are the
// same on every runner, and a rerun writes nothing. Patterns match ignoring
// case: "*.MD" is an LFS filter for readme.md, the fail-closed answer.
func TestAttrsPinnedSettings(t *testing.T) {
	t.Parallel()
	requireGit(t, gitAttrSource, "--attr-source")
	s := newServed(t, t.TempDir(), "repo.git")
	head := s.commit("main", nil, []file{
		{path: ".gitattributes", content: "*.MD filter=lfs diff=lfs merge=lfs -text\n*.CMD text eol=crlf\n"},
		{path: "docs/readme.md", content: "x\n"},
	}, "attributes\n")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	if err := tr.FetchBlobs(t.Context(), []string{s.blob(head, ".gitattributes")}); err != nil {
		t.Fatal(err)
	}
	cmd := []byte("@echo off\r\n")
	for _, setting := range []string{"", "false", "true"} {
		if setting != "" {
			if _, err := tr.Git.Run(t.Context(), nil, "config", "core.ignorecase", setting); err != nil {
				t.Fatal(err)
			}
		}
		got, err := tr.Attrs(t.Context(), head, []string{"docs/readme.md", "setup.cmd"}, "filter", "text")
		if err != nil {
			t.Fatal(err)
		}
		if got["docs/readme.md"]["filter"] != "lfs" || got["setup.cmd"]["text"] != "set" {
			t.Errorf("core.ignorecase=%q in the repository: Attrs = %v, want the patterns to match ignoring case", setting, got)
		}
		id, err := tr.Renormalized(t.Context(), head, "setup.cmd", cmd)
		if err != nil || id != RawOID([]byte("@echo off\n")) {
			t.Errorf("core.ignorecase=%q in the repository: Renormalized = %s, %v; want the LF blob", setting, id, err)
		}
	}
}

func TestIsAttrName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"text": true, "working-tree-encoding": true, "linguist.generated": true, "a_b": true, "A1": true,
		"": false, "-text": false, "a b": false, "a=b": false, "ü": false, "a/b": false,
	} {
		if got := isAttrName(name); got != want {
			t.Errorf("isAttrName(%q) = %v", name, got)
		}
	}
}
