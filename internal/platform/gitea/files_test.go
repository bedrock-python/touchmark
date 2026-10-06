package gitea

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// gitBlobID is git's SHA-1 blob id of content, computed apart from the
// driver's blobID.
func gitBlobID(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// fileFixture is a repository tree for the contents and trees APIs.
type fileFixture struct {
	*fixture
	repo   platform.Repo
	commit string
}

// file answers the contents API for path with a regular file.
func (f *fileFixture) file(path string, content []byte) {
	f.json(http.MethodGet, "/repos/acme/api/contents/"+path, http.StatusOK, map[string]any{
		"name": path[strings.LastIndex(path, "/")+1:], "path": path, "sha": gitBlobID(content),
		"last_commit_sha": f.commit, "last_author_date": "2026-09-01T10:00:00Z", "type": "file",
		"size": len(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content),
		"target": nil, "url": f.base() + "/api/v1/repos/acme/api/contents/" + path, "html_url": "",
		"git_url": "", "download_url": "", "submodule_git_url": nil,
		"_links": map[string]any{"self": "", "git": "", "html": ""},
	})
}

// tree answers the trees API for sha with entries (path, mode, type, sha),
// perPage entries a page.
func (f *fileFixture) tree(sha string, perPage int, entries ...[4]string) {
	f.handle(http.MethodGet, "/repos/acme/api/git/trees/"+sha, func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 {
			page = 1
		}
		var list []any
		for _, e := range entries {
			list = append(list, map[string]any{"path": e[0], "mode": e[1], "type": e[2], "size": 10, "sha": e[3],
				"url": f.base() + "/api/v1/repos/acme/api/git/blobs/" + e[3]})
		}
		from := min((page-1)*perPage, len(list))
		to := min(from+perPage, len(list))
		writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "url": "", "tree": list[from:to],
			"truncated": to < len(list), "page": page, "total_count": len(list)})
	})
}

func newFileFixture(t *testing.T, typ string) *fileFixture {
	fx := newFixture(t, typ)
	return &fileFixture{fixture: fx, repo: platform.Repo{Host: fx.provider(typ).Host, ID: "7", Path: "acme/api", DefaultBranch: "main"},
		commit: strings.Repeat("c0", 20)}
}

func TestReadFile(t *testing.T) {
	f := newFileFixture(t, "gitea")
	readme := []byte("# conformance\n\nline two ⚠\n")
	script := []byte("#!/bin/sh\necho conformance\n")
	guide := []byte("guide\n")
	f.file("README.md", readme)
	f.file("scripts/run.sh", script)
	f.file("docs/guide/intro.md", guide)
	scripts, docs, guideTree := strings.Repeat("51", 20), strings.Repeat("d0", 20), strings.Repeat("9d", 20)
	f.tree(f.commit, treePageSize,
		[4]string{"README.md", "100644", "blob", gitBlobID(readme)},
		[4]string{"docs", "040000", "tree", docs},
		[4]string{"scripts", "040000", "tree", scripts})
	f.tree(scripts, treePageSize, [4]string{"run.sh", "100755", "blob", gitBlobID(script)})
	f.tree(docs, treePageSize, [4]string{"a.md", "100644", "blob", strings.Repeat("aa", 20)}, [4]string{"guide", "040000", "tree", guideTree})
	f.tree(guideTree, treePageSize, [4]string{"intro.md", "100644", "blob", gitBlobID(guide)})

	for _, tc := range []struct {
		path, ref, mode string
		content         []byte
	}{
		{"README.md", "", "100644", readme},
		{"scripts/run.sh", "", "100755", script},
		{"docs/guide/intro.md", "", "100644", guide},
		{"README.md", f.commit, "100644", readme},
		{"README.md", "main", "100644", readme},
	} {
		got, err := f.reader.ReadFile(t.Context(), f.repo, tc.ref, tc.path, 64<<10)
		switch {
		case err != nil:
			t.Errorf("ReadFile(%s at %q): %v", tc.path, tc.ref, err)
		case got.Path != tc.path || got.Mode != tc.mode || !bytes.Equal(got.Content, tc.content) || got.OID != gitBlobID(tc.content):
			t.Errorf("ReadFile(%s at %q) = %+v", tc.path, tc.ref, got)
		}
	}
	calls := f.requests(http.MethodGet, "/repos/acme/api/contents/README.md")
	if len(calls) != 3 || calls[0].Query.Has("ref") || calls[1].Query.Get("ref") != f.commit || calls[2].Query.Get("ref") != "main" {
		t.Errorf("contents requests %+v", calls)
	}
	if _, err := f.reader.ReadFile(t.Context(), f.repo, "", "README.md", int64(len(readme))); err != nil {
		t.Errorf("ReadFile at exactly the limit: %v", err)
	}
	_, err := f.reader.ReadFile(t.Context(), f.repo, "", "README.md", int64(len(readme))-1)
	if !errors.Is(err, platform.ErrTooLarge) {
		t.Errorf("ReadFile over the limit: %v", err)
	}
}

func TestReadFileNotRegular(t *testing.T) {
	f := newFileFixture(t, "forgejo")
	entry := func(name, typ string) map[string]any {
		return map[string]any{"name": name, "path": name, "sha": strings.Repeat("ab", 20), "type": typ, "size": 0,
			"encoding": nil, "content": nil, "target": nil, "last_commit_sha": f.commit}
	}
	link := entry("link.md", "symlink")
	link["target"] = "target.md"
	link["size"] = 9
	f.json(http.MethodGet, "/repos/acme/api/contents/link.md", http.StatusOK, link)
	f.json(http.MethodGet, "/repos/acme/api/contents/vendor/lib", http.StatusOK, entry("lib", "submodule"))
	f.json(http.MethodGet, "/repos/acme/api/contents/docs", http.StatusOK, []any{entry("a.md", "file"), entry("guide", "dir")})
	for _, p := range []string{"link.md", "vendor/lib", "docs"} {
		_, err := f.reader.ReadFile(t.Context(), f.repo, "", p, 64<<10)
		if !errors.Is(err, platform.ErrNotRegular) || platform.ClassOf(err) == platform.ClassNotFound {
			t.Errorf("ReadFile(%s): %v, want ErrNotRegular", p, err)
		}
	}
}

func TestReadFileNotFound(t *testing.T) {
	f := newFileFixture(t, "gitea")
	f.json(http.MethodGet, "/repos/acme/api/contents/missing.md", http.StatusNotFound, f.apiMsg("GetContentsOrList"))
	// Through a symlinked directory: the server finds no such path.
	f.json(http.MethodGet, "/repos/acme/api/contents/dirlink/a.md", http.StatusNotFound, f.apiMsg("object does not exist"))
	// Forgejo lists nothing for a path of an empty repository.
	f.json(http.MethodGet, "/repos/acme/api/contents/empty.md", http.StatusOK, []any{})
	for _, p := range []string{"missing.md", "dirlink/a.md", "empty.md"} {
		_, err := f.reader.ReadFile(t.Context(), f.repo, "", p, 64<<10)
		wantClass(t, "ReadFile of "+p, err, platform.ClassNotFound, platform.ErrNotFound)
	}
}

func TestReadFileEscapesPaths(t *testing.T) {
	f := newFileFixture(t, "gitea")
	content := []byte("x\n")
	name := "dir with space/f #1?.md"
	f.file(name, content)
	sub := strings.Repeat("5b", 20)
	f.tree(f.commit, treePageSize, [4]string{"dir with space", "040000", "tree", sub})
	f.tree(sub, treePageSize, [4]string{"f #1?.md", "100644", "blob", gitBlobID(content)})
	got, err := f.reader.ReadFile(t.Context(), f.repo, "", name, 64)
	if err != nil || got.Path != name {
		t.Fatalf("ReadFile(%q) = %+v, %v", name, got, err)
	}
	calls := f.requests(http.MethodGet, "/repos/acme/api/contents/"+name)
	if len(calls) != 1 || calls[0].RawPath != "/api/v1/repos/acme/api/contents/dir%20with%20space/f%20%231%3F.md" {
		t.Errorf("request path %+v", calls)
	}
}

// Directories of more than one page are read until the entry shows.
func TestReadFilePagedTree(t *testing.T) {
	f := newFileFixture(t, "gitea")
	content := []byte("opt-in\n")
	f.file("z.yml", content)
	entries := [][4]string{}
	for i := range 5 {
		entries = append(entries, [4]string{fmt.Sprintf("f%d", i), "100644", "blob", strings.Repeat("e0", 20)})
	}
	entries = append(entries, [4]string{"z.yml", "100755", "blob", gitBlobID(content)})
	f.tree(f.commit, 2, entries...)
	got, err := f.reader.ReadFile(t.Context(), f.repo, "", "z.yml", 64)
	if err != nil || got.Mode != "100755" {
		t.Errorf("ReadFile = %+v, %v", got, err)
	}
	if n := len(f.requests(http.MethodGet, "/repos/acme/api/git/trees/"+f.commit)); n != 3 {
		t.Errorf("%d tree pages read, want 3", n)
	}
}

func TestReadFileRefusesBadAnswers(t *testing.T) {
	f := newFileFixture(t, "gitea")
	good := []byte("hello\n")
	answer := func(path string, change func(map[string]any)) {
		m := map[string]any{"name": path, "path": path, "sha": gitBlobID(good), "last_commit_sha": f.commit, "type": "file",
			"size": len(good), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(good)}
		change(m)
		f.json(http.MethodGet, "/repos/acme/api/contents/"+path, http.StatusOK, m)
	}
	answer("tampered", func(m map[string]any) { m["content"] = base64.StdEncoding.EncodeToString([]byte("hellO\n")) })
	// The API says 5 or 7 bytes of the 6 it sends: both within the limit.
	answer("short", func(m map[string]any) { m["size"] = len(good) - 1 })
	answer("long", func(m map[string]any) { m["size"] = len(good) + 1 })
	answer("oversized", func(m map[string]any) { m["size"] = 99 })
	answer("nocontent", func(m map[string]any) { m["content"] = nil; m["encoding"] = nil })
	answer("notbase64", func(m map[string]any) { m["content"] = "%%%" })
	answer("nosha", func(m map[string]any) { m["sha"] = "" })
	for _, tc := range []struct {
		path  string
		class platform.Class
		// cause is what err must wrap: many unrelated errors share
		// ClassUnknown, so the class alone proves little.
		cause error
	}{
		{"tampered", platform.ClassUnknown, errShape},
		{"short", platform.ClassUnknown, errShape},
		{"long", platform.ClassUnknown, errShape},
		{"oversized", platform.ClassUnknown, platform.ErrTooLarge},
		{"nocontent", platform.ClassUnsupported, nil},
		{"notbase64", platform.ClassUnknown, errShape},
		{"nosha", platform.ClassUnknown, errShape},
	} {
		_, err := f.reader.ReadFile(t.Context(), f.repo, "", tc.path, 64)
		wantClass(t, "ReadFile of "+tc.path, err, tc.class, tc.cause)
		if errors.Is(tc.cause, errShape) && errors.Is(err, platform.ErrTooLarge) {
			t.Errorf("ReadFile of %s: %v reads as too large", tc.path, err)
		}
	}
	// The tree of the file's last commit, which cannot change, holds
	// another blob than the contents API sent: the answers contradict each
	// other.
	f.file("moved", good)
	f.tree(f.commit, treePageSize, [4]string{"moved", "100644", "blob", strings.Repeat("0f", 20)})
	_, err := f.reader.ReadFile(t.Context(), f.repo, "", "moved", 64)
	wantClass(t, "ReadFile of a file its last commit holds otherwise", err, platform.ClassUnknown, errShape)
	// Without a last commit the tree is read at the ref, which moves: a
	// file changed between the two requests is a conflict.
	f.json(http.MethodGet, "/repos/acme/api/contents/changed", http.StatusOK, map[string]any{"type": "file", "sha": gitBlobID(good),
		"size": len(good), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(good)})
	f.tree("main", treePageSize, [4]string{"changed", "100644", "blob", strings.Repeat("0f", 20)})
	_, err = f.reader.ReadFile(t.Context(), f.repo, "", "changed", 64)
	wantClass(t, "ReadFile of a file that changed at the ref", err, platform.ClassConflict, nil)
	for _, p := range []string{"", "/abs", "a//b", "a/../b", "./a", "a\x00b"} {
		_, err := f.reader.ReadFile(t.Context(), f.repo, "", p, 64)
		wantClass(t, fmt.Sprintf("ReadFile(%q)", p), err, platform.ClassInvalid, nil)
	}
	_, err = f.reader.ReadFile(t.Context(), f.repo, "", "x", -1)
	wantClass(t, "ReadFile with a negative limit", err, platform.ClassInvalid, nil)
}

// Only the contents API tells that a file is missing: the core takes a
// missing opt-in file for an opt-out and closes the target's pull requests.
// A tree of the file's last commit (which cannot change) that lacks it, or
// leads through a file, contradicts the contents API: ClassUnknown, never
// ErrNotFound. A tree at the ref (no last commit named) that lacks it means
// it went away in between: ErrNotFound. A symlink on the way is
// ErrNotRegular either way.
func TestReadFileTreeLacksPath(t *testing.T) {
	content := []byte("guide\n")
	paths := []string{"top.md", "docs/guide.md", "lib/x.md", "docs/deep/x.md"}
	for _, fixed := range []bool{true, false} {
		f := newFileFixture(t, "forgejo")
		root := f.commit
		for _, p := range append(slices.Clone(paths), "link/x.md") {
			if fixed {
				f.file(p, content)
				continue
			}
			root = "main"
			f.json(http.MethodGet, "/repos/acme/api/contents/"+p, http.StatusOK, map[string]any{"type": "file", "sha": gitBlobID(content),
				"size": len(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
		}
		docs := strings.Repeat("d0", 20)
		f.tree(root, treePageSize,
			[4]string{"README.md", "100644", "blob", strings.Repeat("aa", 20)},
			[4]string{"docs", "040000", "tree", docs},
			[4]string{"lib", "100644", "blob", strings.Repeat("11", 20)}, // a file, not a directory
			[4]string{"link", "120000", "blob", strings.Repeat("22", 20)})
		f.tree(docs, treePageSize, [4]string{"other.md", "100644", "blob", strings.Repeat("bb", 20)})
		for _, p := range paths {
			_, err := f.reader.ReadFile(t.Context(), f.repo, "", p, 64)
			if fixed {
				wantClass(t, "ReadFile of "+p+" its last commit lacks", err, platform.ClassUnknown, errShape)
				if errors.Is(err, platform.ErrNotFound) {
					t.Errorf("ReadFile of %s: %v reads as not found", p, err)
				}
			} else {
				wantClass(t, "ReadFile of "+p+" the ref lacks", err, platform.ClassNotFound, platform.ErrNotFound)
			}
		}
		_, err := f.reader.ReadFile(t.Context(), f.repo, "", "link/x.md", 64)
		if !errors.Is(err, platform.ErrNotRegular) {
			t.Errorf("ReadFile through a symlink (fixed %v): %v, want ErrNotRegular", fixed, err)
		}
	}
}

// A failed request of the trees API keeps its class and wait, on the
// first page of the root tree, on a later page and in the tree of a
// directory on the way: never ErrNotFound, which would close the pull
// requests of a target whose opt-in file is there. A 404 of the trees
// API, after the contents API found the file, is ClassUnknown too.
func TestReadFileTreeFailures(t *testing.T) {
	content := []byte("opt in\n")
	for _, fail := range []struct {
		name   string
		status int
		header map[string]string
		class  platform.Class
		wait   time.Duration
	}{
		{"502", http.StatusBadGateway, nil, platform.ClassTransient, 0},
		{"429", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, platform.ClassRateLimited, 30 * time.Second},
		{"401", http.StatusUnauthorized, nil, platform.ClassAuth, 0},
		{"404", http.StatusNotFound, nil, platform.ClassUnknown, 0},
	} {
		for _, where := range []string{"first page", "later page", "directory"} {
			t.Run(fail.name+"/"+where, func(t *testing.T) {
				f := newFileFixture(t, "gitea")
				path := "touchmark.yml"
				if where == "directory" {
					path = ".config/touchmark.yml"
				}
				f.file(path, content)
				dir := strings.Repeat("d1", 20)
				failing := func(w http.ResponseWriter) {
					for k, v := range fail.header {
						w.Header().Set(k, v)
					}
					writeJSON(w, fail.status, f.apiMsg("failed"))
				}
				f.handle(http.MethodGet, "/repos/acme/api/git/trees/"+f.commit, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case where == "first page", where == "later page" && r.URL.Query().Get("page") == "2":
						failing(w)
					case where == "later page":
						writeJSON(w, http.StatusOK, map[string]any{"sha": f.commit, "truncated": true, "page": 1, "total_count": 2,
							"tree": []any{map[string]any{"path": "a.md", "mode": "100644", "type": "blob", "sha": strings.Repeat("aa", 20)}}})
					default:
						writeJSON(w, http.StatusOK, map[string]any{"sha": f.commit, "truncated": false, "page": 1, "total_count": 1,
							"tree": []any{map[string]any{"path": ".config", "mode": "040000", "type": "tree", "sha": dir}}})
					}
				})
				f.handle(http.MethodGet, "/repos/acme/api/git/trees/"+dir, func(w http.ResponseWriter, _ *http.Request) { failing(w) })
				_, err := f.reader.ReadFile(t.Context(), f.repo, "", path, 64)
				wantClass(t, "ReadFile", err, fail.class, nil)
				if errors.Is(err, platform.ErrNotFound) {
					t.Errorf("ReadFile: %v reads as not found", err)
				}
				var pe *platform.Error
				if fail.wait > 0 && (!errors.As(err, &pe) || pe.RetryAfter != fail.wait) {
					t.Errorf("ReadFile: %v, want RetryAfter %v", err, fail.wait)
				}
			})
		}
	}
}

// Without last_commit_sha, the mode comes from the tree at the ref, or at
// the default branch.
func TestReadFileWithoutLastCommit(t *testing.T) {
	f := newFileFixture(t, "gitea")
	content := []byte("x\n")
	f.json(http.MethodGet, "/repos/acme/api/contents/a.yml", http.StatusOK, map[string]any{"type": "file", "sha": gitBlobID(content),
		"size": len(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
	f.tree("main", treePageSize, [4]string{"a.yml", "100644", "blob", gitBlobID(content)})
	f.tree("feature/x", treePageSize, [4]string{"a.yml", "100755", "blob", gitBlobID(content)})
	if got, err := f.reader.ReadFile(t.Context(), f.repo, "", "a.yml", 64); err != nil || got.Mode != "100644" {
		t.Errorf("at the default branch: %+v, %v", got, err)
	}
	if got, err := f.reader.ReadFile(t.Context(), f.repo, "feature/x", "a.yml", 64); err != nil || got.Mode != "100755" {
		t.Errorf("at feature/x: %+v, %v", got, err)
	}
	if calls := f.requests(http.MethodGet, "/repos/acme/api/git/trees/feature/x"); len(calls) != 1 || !strings.HasSuffix(calls[0].RawPath, "/trees/feature%2Fx") {
		t.Errorf("the tree of feature/x was asked as %+v", calls)
	}
}

func TestBlobID(t *testing.T) {
	// git hash-object of "hello\n" in both object formats.
	if got := blobID([]byte("hello\n"), 40); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("SHA-1 blob id %s", got)
	}
	if got := blobID([]byte("hello\n"), 64); got != "2cf8d83d9ee29543b34a87727421fdecb7e3f3a183d337639025de576db9ebb4" {
		t.Errorf("SHA-256 blob id %s", got)
	}
}
