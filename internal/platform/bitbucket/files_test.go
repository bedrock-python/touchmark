package bitbucket

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// headCommit is the head of the default branch of acme/api in these tests.
const headCommit = "be4cdd0b1f9566847c274d0d05b9549881e0ae4a"

// optIn is the opt-in file the tests read.
const optIn = ".engineering-assets.yml"

// apiRepoFixture is the repository the file tests read.
var apiRepoFixture = platform.Repo{ID: repoUUID, Path: "acme/api", DefaultBranch: "main", ObjectFormat: "sha1"}

// branch declares GET …/refs/branches/{name} of acme/api with its head.
func (s *apiServer) branch(name, hash string) {
	s.json("/repositories/acme/api/refs/branches/"+name, http.StatusOK, map[string]any{
		"name": name, "type": "branch", "default_merge_strategy": "merge_commit",
		"merge_strategies": []string{"merge_commit", "squash", "fast_forward"},
		"target": map[string]any{"hash": hash, "type": "commit", "date": "2026-09-30T10:00:00+00:00",
			"message": "chore: sync", "parents": []any{}},
		"links": map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/api/branch/" + name}},
	})
}

// srcLinks are the links of an entry of the source API.
func srcLinks(commit, path string) map[string]any {
	self := "https://api.bitbucket.org/2.0/repositories/acme/api/src/" + commit + "/" + path
	return map[string]any{
		"self": map[string]any{"href": self},
		"meta": map[string]any{"href": self + "?format=meta"},
	}
}

// file declares the metadata and the raw content of path at commit of
// acme/api: attributes as the API lists them, content as the raw read
// returns it (the metadata's size is its length unless size is set).
func (s *apiServer) file(commit, path string, attrs any, content string, size ...int) {
	n := len(content)
	if len(size) > 0 {
		n = size[0]
	}
	s.handle(http.MethodGet, "/repositories/acme/api/src/"+commit+"/"+path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "meta" {
			writeJSON(w, http.StatusOK, map[string]any{
				"path": path, "type": "commit_file", "attributes": attrs, "escaped_path": path, "size": n,
				"mimetype": "text/plain", "has_malware_warning": false, "links": srcLinks(commit, path),
				"commit": map[string]any{"hash": commit, "type": "commit"},
			})
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", "attachment")
		_, _ = w.Write([]byte(content))
	})
}

// dir declares the metadata of a directory at commit of acme/api.
func (s *apiServer) dir(commit, path string) {
	s.json("/repositories/acme/api/src/"+commit+"/"+path, http.StatusOK, map[string]any{
		"path": path, "type": "commit_directory", "links": srcLinks(commit, path+"/"),
		"commit": map[string]any{"hash": commit, "type": "commit"},
	})
}

// missing declares a path that commit of acme/api lacks.
func (s *apiServer) missing(commit, path string) {
	s.json("/repositories/acme/api/src/"+commit+"/"+path, http.StatusNotFound, errorBody(noFileMessage+path))
}

// gitBlob is git's id of a blob with content.
func gitBlob(content string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

func TestReadFile(t *testing.T) {
	f := newFixture(t)
	f.branch("main", headCommit)
	content := "version: 1\npacks: [agents]\n"
	f.file(headCommit, optIn, []string{}, content)
	got, err := f.reader.ReadFile(t.Context(), apiRepoFixture, "", optIn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := platform.File{Path: optIn, Mode: "100644", OID: gitBlob(content), Content: []byte(content)}
	if got.Path != want.Path || got.Mode != want.Mode || got.OID != want.OID || string(got.Content) != content {
		t.Errorf("ReadFile = %+v\nwant %+v", got, want)
	}
	calls := f.requests(http.MethodGet, "/repositories/acme/api/src/"+headCommit+"/"+optIn)
	if len(calls) != 2 || calls[0].Query.Get("format") != "meta" || calls[1].Query.Get("format") != "" {
		t.Errorf("calls %+v; want the metadata, then the raw content, at the branch's head commit", calls)
	}
}

func TestReadFileModes(t *testing.T) {
	f := newFixture(t)
	c := headCommit
	f.file(c, "bin/run.sh", []string{"executable"}, "#!/bin/sh\n")
	f.file(c, "string-attr", "executable", "x")
	f.file(c, "binary", []string{"binary"}, "\x00\x01")
	for path, mode := range map[string]string{"bin/run.sh": "100755", "string-attr": "100755", "binary": "100644"} {
		got, err := f.reader.ReadFile(t.Context(), apiRepoFixture, c, path, 100)
		if err != nil || got.Mode != mode {
			t.Errorf("%s: %+v, %v; want mode %s", path, got, err, mode)
		}
	}
	if calls := f.requests(http.MethodGet, "/repositories/acme/api/refs/branches/main"); len(calls) != 0 {
		t.Error("a full commit id was resolved through the branch API")
	}
}

func TestReadFileNotRegular(t *testing.T) {
	f := newFixture(t)
	c := headCommit
	f.file(c, "link", []string{"link"}, "target")
	f.file(c, "sub", []string{"subrepository"}, "0123456789abcdef0123456789abcdef01234567")
	f.file(c, "big.bin", []string{"lfs", "binary"}, "")
	f.dir(c, "docs")
	f.json("/repositories/acme/api/src/"+c+"/odd", http.StatusOK, map[string]any{"path": "odd", "type": "commit_symlink"})
	for _, path := range []string{"link", "sub", "big.bin", "docs", "odd"} {
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, c, path, 100)
		wantClass(t, path, err, platform.ClassUnknown, platform.ErrNotRegular)
	}
	if calls := f.requests("", ""); len(calls) != 5 {
		t.Errorf("%d requests; want the metadata only, never the content", len(calls))
	}
}

func TestReadFileLFSRedirect(t *testing.T) {
	f := newFixture(t)
	f.handle(http.MethodGet, "/repositories/acme/api/src/"+headCommit+"/model.bin", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "meta" {
			writeJSON(w, http.StatusOK, map[string]any{"path": "model.bin", "type": "commit_file", "attributes": []string{}, "size": 3})
			return
		}
		http.Redirect(w, r, "https://api.media.atlassian.com/file/0123/binary", http.StatusFound)
	})
	_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, "model.bin", 100)
	wantClass(t, "a redirect elsewhere", err, platform.ClassUnknown, platform.ErrNotRegular)
}

func TestReadFileTooLarge(t *testing.T) {
	f := newFixture(t)
	f.file(headCommit, optIn, []string{}, strings.Repeat("x", 11))
	_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 10)
	wantClass(t, "over the limit", err, platform.ClassUnknown, platform.ErrTooLarge)
	if calls := f.requests("", ""); len(calls) != 1 {
		t.Errorf("%d requests; want the metadata only", len(calls))
	}
	if _, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 11); err != nil {
		t.Errorf("at the limit: %v", err)
	}
}

func TestReadFileMissing(t *testing.T) {
	t.Run("a missing file", func(t *testing.T) {
		f := newFixture(t)
		f.branch("main", headCommit)
		f.missing(headCommit, optIn)
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, "", optIn, 100)
		wantClass(t, "a missing file", err, platform.ClassNotFound, platform.ErrNotFound)
		if calls := f.requests("", ""); len(calls) != 2 {
			t.Errorf("%d requests; want the branch and the metadata", len(calls))
		}
	})
	t.Run("a missing nested file", func(t *testing.T) {
		f := newFixture(t)
		f.missing(headCommit, "a/b/c.yml")
		f.dir(headCommit, "a")
		f.missing(headCommit, "a/b")
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, "a/b/c.yml", 100)
		wantClass(t, "a missing directory", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("through a symlink", func(t *testing.T) {
		f := newFixture(t)
		f.missing(headCommit, "conf/touchmark.yml")
		f.file(headCommit, "conf", []string{"link"}, "../elsewhere")
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, "conf/touchmark.yml", 100)
		wantClass(t, "through a symlink", err, platform.ClassUnknown, platform.ErrNotRegular)
	})
	t.Run("through a file", func(t *testing.T) {
		f := newFixture(t)
		f.missing(headCommit, "README.md/x")
		f.file(headCommit, "README.md", []string{}, "# api\n")
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, "README.md/x", 100)
		wantClass(t, "through a file", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("another 404 at a commit that exists", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/src/"+headCommit+"/"+optIn, http.StatusNotFound, errorBody("Not found"))
		f.json("/repositories/acme/api/commit/"+headCommit, http.StatusOK, map[string]any{"hash": headCommit, "type": "commit"})
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 100)
		wantClass(t, "a commit that exists", err, platform.ClassNotFound, platform.ErrNotFound)
	})
	t.Run("another 404 at a commit that is gone", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/src/"+headCommit+"/"+optIn, http.StatusNotFound, errorBody(noRepoMessage))
		f.json("/repositories/acme/api/commit/"+headCommit, http.StatusNotFound, errorBody(noRepoMessage))
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 100)
		wantClass(t, "a commit that is gone", err, platform.ClassUnknown, nil)
		if platform.ClassOf(err) == platform.ClassNotFound || isNotFound(err) {
			t.Errorf("%v: a missing commit must never read as a missing file", err)
		}
	})
	t.Run("a missing repository", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/refs/branches/main", http.StatusNotFound, errorBody(noRepoMessage))
		f.json("/repositories/acme/api", http.StatusNotFound, errorBody(noRepoMessage))
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, "", optIn, 100)
		wantClass(t, "a missing repository", err, platform.ClassUnknown, nil)
		if isNotFound(err) {
			t.Errorf("%v: a missing repository must never read as a missing file", err)
		}
	})
	t.Run("a missing ref", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/refs/branches/feature/x", http.StatusNotFound, errorBody("Branch \"feature/x\" not found"))
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, "feature/x", optIn, 100)
		wantClass(t, "a missing ref", err, platform.ClassUnknown, nil)
		if isNotFound(err) {
			t.Errorf("%v: a missing ref must never read as a missing file", err)
		}
	})
	t.Run("the default branch is gone", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/refs/branches/main", http.StatusNotFound, errorBody("Branch \"main\" not found"))
		f.json("/repositories/acme/api", http.StatusOK, repo(repoUUID, "acme/api"))
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, "", optIn, 100)
		wantClass(t, "a default branch without a head", err, platform.ClassUnknown, nil)
		if isNotFound(err) {
			t.Errorf("%v: must never read as a missing file", err)
		}
	})
	t.Run("the default branch was renamed", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/refs/branches/master", http.StatusNotFound, errorBody("Branch \"master\" not found"))
		f.json("/repositories/acme/api", http.StatusOK, repo(repoUUID, "acme/api"))
		f.branch("main", headCommit)
		f.file(headCommit, optIn, []string{}, "version: 1\n")
		r := apiRepoFixture
		r.DefaultBranch = "master"
		if _, err := f.reader.ReadFile(t.Context(), r, "", optIn, 100); err != nil {
			t.Errorf("ReadFile: %v; want the file at the current main branch", err)
		}
	})
	t.Run("an empty repository", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api", http.StatusOK, repo(repoUUID, "acme/api", with("mainbranch", nil)))
		r := apiRepoFixture
		r.DefaultBranch, r.Empty = "", true
		_, err := f.reader.ReadFile(t.Context(), r, "", optIn, 100)
		wantClass(t, "an empty repository", err, platform.ClassNotFound, platform.ErrNotFound)
	})
}

// isNotFound reports whether err says the file is missing, as the core
// asks.
func isNotFound(err error) bool {
	return platform.ClassOf(err) == platform.ClassNotFound
}

func TestReadFileInconsistent(t *testing.T) {
	t.Run("the content is gone", func(t *testing.T) {
		f := newFixture(t)
		f.handle(http.MethodGet, "/repositories/acme/api/src/"+headCommit+"/"+optIn, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") == "meta" {
				writeJSON(w, http.StatusOK, map[string]any{"path": optIn, "type": "commit_file", "attributes": []string{}, "size": 3})
				return
			}
			writeJSON(w, http.StatusNotFound, errorBody(noRepoMessage))
		})
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 100)
		wantClass(t, "content gone after the metadata", err, platform.ClassUnknown, nil)
		if isNotFound(err) {
			t.Errorf("%v: must never read as a missing file", err)
		}
	})
	t.Run("the size differs", func(t *testing.T) {
		f := newFixture(t)
		f.file(headCommit, optIn, []string{}, "abc", 4)
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 100)
		wantClass(t, "a size that differs", err, platform.ClassUnknown, nil)
	})
	t.Run("another path", func(t *testing.T) {
		f := newFixture(t)
		f.json("/repositories/acme/api/src/"+headCommit+"/"+optIn, http.StatusOK,
			map[string]any{"path": "elsewhere.yml", "type": "commit_file", "attributes": []string{}, "size": 3})
		_, err := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 100)
		wantClass(t, "metadata of another path", err, platform.ClassUnknown, nil)
	})
}

func TestReadFileInvalid(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		r    platform.Repo
		path string
		max  int64
	}{
		{platform.Repo{Path: "acme"}, optIn, 1},
		{apiRepoFixture, "", 1},
		{apiRepoFixture, "a//b", 1},
		{apiRepoFixture, "../x", 1},
		{apiRepoFixture, optIn, -1},
	} {
		_, err := f.reader.ReadFile(t.Context(), tc.r, "", tc.path, tc.max)
		wantClass(t, fmt.Sprintf("%s %q %d", tc.r.Path, tc.path, tc.max), err, platform.ClassInvalid, nil)
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("%d requests for invalid reads", len(calls))
	}
}
