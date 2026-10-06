package gitlab

import (
	"encoding/base64"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// treeFile is a tree entry of a test repository.
type treeFile struct {
	mode    string // 100644, 100755, 120000, 160000
	content string // a symlink's target, a submodule's commit id
}

// repoModel serves a repository's files API and tree API as GitLab does:
// the files API returns blobs (a symlink's target, a submodule as an empty
// blob with the commit id: Gitlab::Git::Blob.tree_entry), "404 File Not
// Found" for a directory or a missing path, "404 Commit Not Found" for an
// unknown ref; the tree API lists one directory, and answers a path that
// is no directory with "404 Tree Not Found" (≥ 17.7) or [] (legacy).
type repoModel struct {
	fx     *fixture
	id     string
	commit string
	files  map[string]treeFile
	legacy bool
}

const headCommit = "c0ffee0000000000000000000000000000000001"

func newRepoModel(fx *fixture, files map[string]treeFile) *repoModel {
	m := &repoModel{fx: fx, id: "7", commit: headCommit, files: files}
	fx.handle(http.MethodGet, "/projects/7/repository/tree", m.tree)
	return m
}

// oid returns the id of the entry at p.
func (m *repoModel) oid(p string) string {
	f := m.files[p]
	if f.mode == modeSubmodule {
		return f.content
	}
	return blobID([]byte(f.content), 40)
}

// serveFile declares the files API route of p.
func (m *repoModel) serveFile(p string) {
	m.fx.handle(http.MethodGet, "/projects/7/repository/files/"+escape(p), func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		if ref != "HEAD" && ref != "main" && ref != m.commit {
			writeJSON(w, http.StatusNotFound, msg("404 Commit Not Found"))
			return
		}
		f, ok := m.files[p]
		if !ok {
			writeJSON(w, http.StatusNotFound, msg("404 File Not Found"))
			return
		}
		content, size := f.content, len(f.content)
		if f.mode == modeSubmodule {
			content, size = "", 0
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"file_name": path.Base(p), "file_path": p, "size": size, "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(content)), "content_sha256": "",
			"ref": ref, "blob_id": m.oid(p), "commit_id": m.commit, "last_commit_id": m.commit,
			"execute_filemode": f.mode == modeExecutable,
		})
	})
}

// tree answers GET /projects/7/repository/tree?path=&ref=.
func (m *repoModel) tree(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("ref") != m.commit && q.Get("ref") != "HEAD" && q.Get("ref") != "main" {
		writeJSON(w, http.StatusNotFound, msg("404 Tree Not Found"))
		return
	}
	dir := q.Get("path")
	var entries []any
	seen := map[string]bool{}
	for p := range m.files {
		rest, ok := p, dir == ""
		if !ok {
			rest, ok = strings.CutPrefix(p, dir+"/")
		}
		if !ok {
			continue
		}
		name, sub, deeper := strings.Cut(rest, "/")
		full := path.Join(dir, name)
		if seen[name] {
			continue
		}
		seen[name] = true
		if deeper {
			_ = sub
			entries = append(entries, map[string]any{"id": strings.Repeat("d", 40), "name": name, "type": "tree", "path": full, "mode": "040000"})
			continue
		}
		f := m.files[p]
		typ := "blob"
		if f.mode == modeSubmodule {
			typ = "commit"
		}
		entries = append(entries, map[string]any{"id": m.oid(p), "name": name, "type": typ, "path": full, "mode": f.mode})
	}
	slices.SortFunc(entries, func(a, b any) int {
		return strings.Compare(a.(map[string]any)["name"].(string), b.(map[string]any)["name"].(string))
	})
	if len(entries) == 0 {
		if m.legacy {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeJSON(w, http.StatusNotFound, msg("404 Tree Not Found"))
		return
	}
	servePage(w, r, entries, offset)
}

func filesWorld(t *testing.T) (*fixture, *repoModel, platform.Repo) {
	fx := newFixture(t)
	m := newRepoModel(fx, map[string]treeFile{
		"README.md":           {mode: modeFile, content: "# conformance\n\nline two ⚠\n"},
		"scripts/run.sh":      {mode: modeExecutable, content: "#!/bin/sh\necho conformance\n"},
		"docs/guide/intro.md": {mode: modeFile, content: "guide\n"},
		"link.md":             {mode: modeSymlink, content: "README.md"},
		"dirlink":             {mode: modeSymlink, content: "docs"},
		"vendor/lib":          {mode: modeSubmodule, content: "0123456789abcdef0123456789abcdef01234567"},
	})
	for _, p := range []string{"README.md", "scripts/run.sh", "docs/guide/intro.md", "link.md", "vendor/lib",
		"docs", "docs/guide", "missing.md", "docs/missing.md", "README.md/x", "dirlink/a.md", "nowhere/deep/x.md"} {
		m.serveFile(p)
	}
	return fx, m, platform.Repo{Host: fx.provider().Host, ID: "7", Path: "acme/api", DefaultBranch: "main"}
}

func TestReadFile(t *testing.T) {
	fx, m, repo := filesWorld(t)
	for _, tc := range []struct {
		ref, path, mode string
	}{
		{"", "README.md", modeFile},
		{"", "scripts/run.sh", modeExecutable},
		{"", "docs/guide/intro.md", modeFile},
		{"main", "README.md", modeFile},
		{headCommit, "scripts/run.sh", modeExecutable},
	} {
		fx.reset()
		f, err := fx.reader.ReadFile(t.Context(), repo, tc.ref, tc.path, 64<<10)
		if err != nil {
			t.Fatalf("ReadFile(%q, %s): %v", tc.ref, tc.path, err)
		}
		if f.Path != tc.path || f.Mode != tc.mode || string(f.Content) != m.files[tc.path].content || f.OID != m.oid(tc.path) {
			t.Errorf("ReadFile(%q, %s) = %s %s %q", tc.ref, tc.path, f.Mode, f.OID, f.Content)
		}
		calls := fx.requests(http.MethodGet, "/projects/7/repository/files/"+escape(tc.path))
		if len(calls) != 1 || calls[0].Query.Get("ref") != cmpStr(tc.ref, "HEAD") {
			t.Errorf("files API calls %v", calls)
		}
		// The tree is read at the commit the files API read, never at a
		// branch that may have moved.
		for _, c := range fx.requests(http.MethodGet, "/projects/7/repository/tree") {
			if c.Query.Get("ref") != headCommit {
				t.Errorf("tree read at %q", c.Query.Get("ref"))
			}
		}
	}

	for _, p := range []string{"link.md", "vendor/lib", "docs", "docs/guide", "dirlink/a.md"} {
		_, err := fx.reader.ReadFile(t.Context(), repo, "", p, 64<<10)
		if !errors.Is(err, platform.ErrNotRegular) {
			t.Errorf("ReadFile(%s): %v, want ErrNotRegular", p, err)
		}
	}
	for _, p := range []string{"missing.md", "docs/missing.md", "README.md/x", "nowhere/deep/x.md"} {
		_, err := fx.reader.ReadFile(t.Context(), repo, "", p, 64<<10)
		wantClass(t, "ReadFile("+p+")", err, platform.ClassNotFound, platform.ErrNotFound)
	}

	size := int64(len(m.files["README.md"].content))
	if _, err := fx.reader.ReadFile(t.Context(), repo, "", "README.md", size); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	_, err := fx.reader.ReadFile(t.Context(), repo, "", "README.md", size-1)
	if !errors.Is(err, platform.ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	for _, bad := range []string{"", "/abs", "a//b", "../x", "a/./b"} {
		_, err := fx.reader.ReadFile(t.Context(), repo, "", bad, 10)
		wantClass(t, "ReadFile("+bad+")", err, platform.ClassInvalid, nil)
	}
}

func TestReadFileLegacyTree(t *testing.T) {
	// Before 17.7 the tree API answers a path that is no directory with [].
	fx, m, repo := filesWorld(t)
	m.legacy = true
	for _, p := range []string{"missing.md", "README.md/x", "nowhere/deep/x.md"} {
		_, err := fx.reader.ReadFile(t.Context(), repo, "", p, 64<<10)
		wantClass(t, "legacy ReadFile("+p+")", err, platform.ClassNotFound, platform.ErrNotFound)
	}
	if _, err := fx.reader.ReadFile(t.Context(), repo, "", "dirlink/a.md", 64<<10); !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("legacy ReadFile through a symlink: %v", err)
	}
}

// TestReadFileNeverOptsOut: only a missing file is ErrNotFound; a missing
// project, ref or anything that is not GitLab's answer is not, since the
// core takes a missing opt-in file for an opt-out.
func TestReadFileNeverOptsOut(t *testing.T) {
	fx, m, repo := filesWorld(t)
	gone := platform.Repo{Host: repo.Host, ID: "8", Path: "acme/gone"}
	fx.json(http.MethodGet, "/projects/8/repository/files/README%2Emd", http.StatusNotFound, msg("404 Project Not Found"))
	_, err := fx.reader.ReadFile(t.Context(), gone, "", "README.md", 1024)
	if err == nil || errors.Is(err, platform.ErrNotFound) || platform.ClassOf(err) == platform.ClassNotFound {
		t.Errorf("a missing project: %v (class %v)", err, platform.ClassOf(err))
	}

	_, err = fx.reader.ReadFile(t.Context(), repo, strings.Repeat("e", 40), "README.md", 1024)
	if err == nil || platform.ClassOf(err) == platform.ClassNotFound {
		t.Errorf("a missing commit: %v (class %v)", err, platform.ClassOf(err))
	}

	proxy := platform.Repo{Host: repo.Host, ID: "9", Path: "acme/proxied"}
	fx.handle(http.MethodGet, "/projects/9/repository/files/README%2Emd", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>404 Not Found</html>"))
	})
	_, err = fx.reader.ReadFile(t.Context(), proxy, "", "README.md", 1024)
	if err == nil || platform.ClassOf(err) == platform.ClassNotFound {
		t.Errorf("a proxy's 404: %v (class %v)", err, platform.ClassOf(err))
	}

	// The files API finds it, the tree of that commit does not: unknown.
	m.serveFile("ghost.md")
	m.files["ghost.md"] = treeFile{mode: modeFile, content: "boo\n"}
	fx.handle(http.MethodGet, "/projects/7/repository/tree", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, []any{map[string]any{"id": m.oid("README.md"), "name": "README.md", "type": "blob", "path": "README.md", "mode": "100644"}}, offset)
	})
	_, err = fx.reader.ReadFile(t.Context(), repo, "", "ghost.md", 1024)
	wantClass(t, "a tree without the file", err, platform.ClassUnknown, nil)

	// A transient failure of the tree stays transient.
	fx.json(http.MethodGet, "/projects/7/repository/tree", http.StatusBadGateway, msg("502 Bad Gateway"))
	_, err = fx.reader.ReadFile(t.Context(), repo, "", "README.md", 1024)
	wantClass(t, "a failed tree", err, platform.ClassTransient, nil)
}

func TestReadFileEmptyRepo(t *testing.T) {
	fx := newFixture(t)
	repo := platform.Repo{Host: fx.provider().Host, ID: "7", Path: "acme/empty"}
	fx.json(http.MethodGet, "/projects/7/repository/files/README%2Emd", http.StatusNotFound, msg("404 Commit Not Found"))
	fx.json(http.MethodGet, "/projects/7", http.StatusOK, project(7, "acme/empty", with("empty_repo", true), with("default_branch", nil)))
	_, err := fx.reader.ReadFile(t.Context(), repo, "", "README.md", 1024)
	wantClass(t, "an empty repository", err, platform.ClassNotFound, platform.ErrNotFound)

	// A project with commits whose HEAD does not resolve is no opt-out.
	fx.json(http.MethodGet, "/projects/7", http.StatusOK, project(7, "acme/empty"))
	_, err = fx.reader.ReadFile(t.Context(), repo, "", "README.md", 1024)
	if platform.ClassOf(err) == platform.ClassNotFound {
		t.Errorf("a broken HEAD: %v", err)
	}
}

func TestReadFileBadContent(t *testing.T) {
	fx, m, repo := filesWorld(t)
	fx.handle(http.MethodGet, "/projects/7/repository/files/README%2Emd", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"file_path": "README.md", "size": 5, "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte("forgd")), "blob_id": m.oid("README.md"),
			"commit_id": headCommit, "last_commit_id": headCommit})
	})
	_, err := fx.reader.ReadFile(t.Context(), repo, "", "README.md", 1024)
	wantClass(t, "content that does not hash to the blob", err, platform.ClassUnknown, nil)
}
