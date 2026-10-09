package azuredevops

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Ids of the trees and the commit of the file tests.
const (
	headCommit = "af56d96fdbd7c26e9fc94336b6f50dcc6ceff484"
	rootTree   = "6f5bf80f955183fe9b0c8640961303620245c8a4"
	ciTree     = "d1d5c2d49045d52bba6419652d6ecb2cd560dc29"
)

// fileWorld declares a repository at headCommit on branch main: a root
// with .engineering-assets.yml, run.sh (executable), link (a symlink),
// sub (a submodule), big.txt and the folder ci with build.yml; any other
// folder is missing. The items route answers for the folders "/" and
// "/ci" at main or headCommit, a missing branch with
// GitUnresolvableToCommitException.
func fileWorld(t *testing.T, s *apiServer) map[string]string {
	t.Helper()
	files := map[string]string{
		".engineering-assets.yml": "packs: [agents]\n",
		"run.sh":                  "#!/bin/sh\necho hi\n",
		"build.yml":               "on: push\n",
		"big.txt":                 "0123456789",
	}
	entry := func(name, mode, typ, oid string, size int) map[string]any {
		return map[string]any{"objectId": oid, "relativePath": name, "mode": mode, "gitObjectType": typ, "size": size,
			"url": "https://dev.azure.com/" + org + "/_apis/git/repositories/" + repoID + "/blobs/" + oid}
	}
	blob := func(name string) map[string]any {
		mode := modeFile
		if name == "run.sh" {
			mode = modeExecutable
		}
		return entry(name, mode, "blob", gitBlobID(files[name]), len(files[name]))
	}
	s.json(repoPath("trees", rootTree), http.StatusOK, map[string]any{"objectId": rootTree, "size": 5, "treeEntries": []any{
		blob(".engineering-assets.yml"), blob("run.sh"), blob("big.txt"),
		entry("link", modeSymlink, "blob", gitBlobID("target"), 6),
		entry("sub", modeSubmodule, "commit", "3aae318f1661c50c34effbbf6882119ed161f2d6", 0),
		entry("ci", "40000", "tree", ciTree, 1),
	}})
	s.json(repoPath("trees", ciTree), http.StatusOK, map[string]any{"objectId": ciTree, "treeEntries": []any{blob("build.yml")}})
	for name, content := range files {
		content := content
		s.handle(http.MethodGet, repoPath("blobs", gitBlobID(content)), func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("$format") != "octetstream" {
				t.Errorf("a blob without $format=octetstream")
			}
			w.Header().Set("Content-Type", "application/octet-stream; api-version=7.1")
			_, _ = w.Write([]byte(content))
		})
		_ = name
	}
	s.handle(http.MethodGet, repoPath("items"), func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		version, kind := q.Get("versionDescriptor.version"), q.Get("versionDescriptor.versionType")
		if !(version == "main" && kind == "branch" || version == headCommit && kind == "commit") {
			writeJSON(w, http.StatusNotFound, errorBody(keyUnresolvable, "TF401175: The version descriptor <Branch: "+version+"> could not be resolved to a version in the repository"))
			return
		}
		item := map[string]any{"gitObjectType": "tree", "commitId": headCommit, "path": q.Get("path"), "isFolder": true}
		switch q.Get("path") {
		case "/":
			item["objectId"] = rootTree
		case "/ci":
			item["objectId"] = ciTree
		case "/run.sh":
			item = map[string]any{"objectId": gitBlobID(files["run.sh"]), "gitObjectType": "blob", "commitId": headCommit, "path": "/run.sh"}
		case "/link":
			item = map[string]any{"objectId": gitBlobID("target"), "gitObjectType": "blob", "commitId": headCommit, "path": "/link", "isSymLink": true}
		default:
			writeJSON(w, http.StatusNotFound, errorBody(keyItemNotFound, "TF401174: The item '"+q.Get("path")+"' could not be found in the repository"))
			return
		}
		writeJSON(w, http.StatusOK, item)
	})
	return files
}

func TestReadFile(t *testing.T) {
	s := newAPIServer(t)
	files := fileWorld(t, s)
	r := newTestReader(t, s, testToken(t))
	ctx := context.Background()
	repo := testRepo()

	f, err := r.ReadFile(ctx, repo, "", ".engineering-assets.yml", 1<<20)
	if err != nil || string(f.Content) != files[".engineering-assets.yml"] || f.Mode != "100644" ||
		f.OID != gitBlobID(files[".engineering-assets.yml"]) || f.Path != ".engineering-assets.yml" {
		t.Errorf("ReadFile = %+v, %v", f, err)
	}
	if f, err := r.ReadFile(ctx, repo, headCommit, "run.sh", 1<<20); err != nil || f.Mode != "100755" {
		t.Errorf("an executable at a commit: %+v, %v", f, err)
	}
	if f, err := r.ReadFile(ctx, repo, "main", "ci/build.yml", 1<<20); err != nil || string(f.Content) != files["build.yml"] {
		t.Errorf("a nested file: %+v, %v", f, err)
	}
	for _, p := range []string{"link", "sub", "ci", "link/x"} {
		if _, err := r.ReadFile(ctx, repo, "", p, 1<<20); !errors.Is(err, platform.ErrNotRegular) {
			t.Errorf("%s: %v, want ErrNotRegular", p, err)
		}
	}
	for _, p := range []string{"missing.yml", "docs/missing.yml", "run.sh/x"} {
		if _, err := r.ReadFile(ctx, repo, "", p, 1<<20); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", p, err)
		}
	}
	if _, err := r.ReadFile(ctx, repo, "", "big.txt", 5); !errors.Is(err, platform.ErrTooLarge) {
		t.Errorf("a large file: %v", err)
	}
	// A missing branch is never a missing file.
	_, err = r.ReadFile(ctx, repo, "nope", ".engineering-assets.yml", 1<<20)
	wantClass(t, "a missing branch", err, platform.ClassUnknown)
	if errors.Is(err, platform.ErrNotFound) {
		t.Error("a missing branch is ErrNotFound")
	}
	for _, bad := range []string{"", "/x", "a//b", "../x"} {
		_, err := r.ReadFile(ctx, repo, "", bad, 1)
		wantClass(t, "path "+bad, err, platform.ClassInvalid)
	}
}

// TestReadFileDefaultBranch: the default branch r names is gone: the
// repository tells the current one; an empty repository has no file; a
// missing repository is never a missing file.
func TestReadFileDefaultBranch(t *testing.T) {
	s := newAPIServer(t)
	fileWorld(t, s)
	r := newTestReader(t, s, testToken(t))
	ctx := context.Background()
	s.json(repoPath(), http.StatusOK, repoJSON(repoID, "Billing", "api", "main"))
	old := testRepo()
	old.DefaultBranch = "master"
	if _, err := r.ReadFile(ctx, old, "", ".engineering-assets.yml", 1<<20); err != nil {
		t.Errorf("a renamed default branch: %v", err)
	}
	s.json(repoPath(), http.StatusOK, repoJSON(repoID, "Billing", "api", ""))
	empty := testRepo()
	empty.DefaultBranch = ""
	if _, err := r.ReadFile(ctx, empty, "", ".engineering-assets.yml", 1<<20); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("an empty repository: %v", err)
	}
	s.json(repoPath(), http.StatusNotFound, errorBody(keyRepoNotFound, "TF401019: gone"))
	_, err := r.ReadFile(ctx, empty, "", ".engineering-assets.yml", 1<<20)
	wantClass(t, "a missing repository", err, platform.ClassUnknown)
}

// TestReadFileBlobMismatch: bytes that are not the entry's blob are no
// file.
func TestReadFileBlobMismatch(t *testing.T) {
	s := newAPIServer(t)
	files := fileWorld(t, s)
	s.handle(http.MethodGet, repoPath("blobs", gitBlobID(files["build.yml"])), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("on: pull\n"))
	})
	_, err := newTestReader(t, s, testToken(t)).ReadFile(context.Background(), testRepo(), "", "ci/build.yml", 1<<20)
	wantClass(t, "mismatch", err, platform.ClassUnknown)
}
