package bitbucketdc

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// apiRepoOf is the platform.Repo of ACME/api.
func (f *fixture) apiRepoOf() platform.Repo {
	return platform.Repo{Host: f.provider().Host, ID: "101", Path: "ACME/api", DefaultBranch: "main"}
}

// file declares path at any commit: its type, its size and its raw content.
func (f *fixture) file(path, typ string, content []byte) {
	route := repoRoute("ACME", "api")
	f.handle(http.MethodGet, route+"/browse/"+path, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("type") == "true":
			writeJSON(w, http.StatusOK, map[string]any{"type": typ})
		case r.URL.Query().Get("size") == "true":
			writeJSON(w, http.StatusOK, map[string]any{"size": len(content)})
		default:
			t := f.t
			t.Errorf("browse without type or size: %s", r.URL.RawQuery)
		}
	})
	f.handle(http.MethodGet, route+"/raw/"+path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
		_, _ = w.Write(content)
	})
}

func TestReadFile(t *testing.T) {
	f := newFixture(t)
	content := []byte("version: 1\nenabled: true\n")
	f.defBranch("ACME", "api", "main", mainSHA)
	f.file(".engineering-assets.yml", "FILE", content)
	got, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// git hash-object of the content.
	if got.OID != blobID(content) || got.Mode != "100644" || string(got.Content) != string(content) || got.Path != ".engineering-assets.yml" {
		t.Errorf("ReadFile = %+v", got)
	}
	if blobID([]byte("hello\n")) != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Error("blobID is not git's")
	}
	for _, c := range f.requests(http.MethodGet, "") {
		if strings.Contains(c.Path, "/browse/") || strings.Contains(c.Path, "/raw/") {
			if c.Query.Get("at") != mainSHA {
				t.Errorf("%s at %q, want the default branch's head", c.Path, c.Query.Get("at"))
			}
		}
	}

	// A branch and a commit.
	f.reset()
	f.pages(repoRoute("ACME", "api")+"/branches", 1000, []any{
		branch(syncBranch, headSHA, false), branch(syncBranch+"-old", mainSHA, false),
	})
	if _, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), syncBranch, ".engineering-assets.yml", 1<<20); err != nil {
		t.Fatal(err)
	}
	if c := f.requests(http.MethodGet, repoRoute("ACME", "api")+"/raw/.engineering-assets.yml"); len(c) != 1 || c[0].Query.Get("at") != headSHA {
		t.Errorf("the branch's file was read at %v", c)
	}
	if q := f.requests(http.MethodGet, repoRoute("ACME", "api")+"/branches")[0].Query; q.Get("filterText") != syncBranch || q.Get("boostMatches") != "true" {
		t.Errorf("the branch query %v", q)
	}
	f.reset()
	if _, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), strings.ToUpper(headSHA), ".engineering-assets.yml", 1<<20); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests(http.MethodGet, repoRoute("ACME", "api")+"/branches")); n != 0 {
		t.Error("a commit id was looked up as a branch")
	}
}

func TestReadFileKinds(t *testing.T) {
	f := newFixture(t)
	f.defBranch("ACME", "api", "main", mainSHA)
	f.file("docs", "DIRECTORY", nil)
	f.file("vendor/lib", "SUBMODULE", nil)
	f.file("big.bin", "FILE", make([]byte, 2048))
	f.file("odd", "LINK", nil)
	for _, tc := range []struct {
		path     string
		sentinel error
	}{{"docs", platform.ErrNotRegular}, {"vendor/lib", platform.ErrNotRegular}, {"big.bin", platform.ErrTooLarge}} {
		_, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", tc.path, 1024)
		if err == nil || !errors.Is(err, tc.sentinel) {
			t.Errorf("%s: %v, want %v", tc.path, err, tc.sentinel)
		}
	}
	if n := len(f.requests(http.MethodGet, repoRoute("ACME", "api")+"/raw/big.bin")); n != 0 {
		t.Error("a file over the limit was downloaded")
	}
	_, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", "odd", 1024)
	wantClass(t, "an unknown node type", err, platform.ClassUnknown, nil)
	for _, p := range []string{"", "/abs", "a//b", "a/../b", "a\x00b"} {
		_, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", p, 1024)
		wantClass(t, "path "+p, err, platform.ClassInvalid, nil)
	}
}

func TestReadFileMissing(t *testing.T) {
	route := repoRoute("ACME", "api")
	missing := func(f *fixture, exception string) {
		f.json(route+"/browse/.engineering-assets.yml", http.StatusNotFound, errorBody(exception, "The path \".engineering-assets.yml\" does not exist at revision \""+mainSHA+"\""))
	}

	// The path is missing at a commit that exists.
	f := newFixture(t)
	f.defBranch("ACME", "api", "main", mainSHA)
	missing(f, noPathException)
	_, err := f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "a missing path", err, platform.ClassNotFound, platform.ErrNotFound)
	if n := len(f.requests(http.MethodGet, route+"/commits/"+mainSHA)); n != 0 {
		t.Error("NoSuchPathException did not suffice")
	}

	// A 404 that names nothing: the commit tells.
	missing(f, "")
	f.json(route+"/commits/"+mainSHA, http.StatusOK, map[string]any{"id": mainSHA, "displayId": mainSHA[:11]})
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "a 404 without an exception, the commit found", err, platform.ClassNotFound, platform.ErrNotFound)
	f.json(route+"/commits/"+mainSHA, http.StatusNotFound, errorBody("", "gone"))
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "neither the file nor the commit", err, platform.ClassUnknown, nil)
	if err != nil && errors.Is(err, platform.ErrNotFound) {
		t.Error("an unknown failure wraps ErrNotFound")
	}

	// The repository is gone between two requests.
	missing(f, noRepoException)
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "a missing repository", err, platform.ClassUnknown, nil)

	// An empty repository has no file.
	f = newFixture(t)
	f.json(route+"/branches/default", http.StatusNoContent, nil)
	f.json(route+"/default-branch", http.StatusOK, map[string]any{"id": "refs/heads/main", "displayId": "main"})
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "an empty repository", err, platform.ClassNotFound, platform.ErrNotFound)

	// A missing repository, or a missing ref, is no missing file.
	f = newFixture(t)
	f.json(route+"/branches/default", http.StatusNotFound, errorBody(noRepoException, noRepoMessage))
	f.json(route+"/default-branch", http.StatusNotFound, errorBody(noRepoException, noRepoMessage))
	f.json(route, http.StatusNotFound, errorBody(noRepoException, noRepoMessage))
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "a missing repository", err, platform.ClassUnknown, nil)
	// The repository is there, its default branch is not: no missing file.
	f.json(route, http.StatusOK, repo(repoID, "ACME", "api"))
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "a missing default branch", err, platform.ClassUnknown, nil)
	f.pages(route+"/branches", 1000, []any{branch(syncBranch+"-old", mainSHA, false)})
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), syncBranch, ".engineering-assets.yml", 1024)
	wantClass(t, "a missing branch", err, platform.ClassUnknown, nil)

	// The type shows the file, the raw read does not find it.
	f = newFixture(t)
	f.defBranch("ACME", "api", "main", mainSHA)
	f.file(".engineering-assets.yml", "FILE", []byte("x"))
	f.json(route+"/raw/.engineering-assets.yml", http.StatusNotFound, errorBody(noPathException, "gone"))
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", ".engineering-assets.yml", 1024)
	wantClass(t, "a file the raw read misses", err, platform.ClassUnknown, nil)

	// The raw content is not the size the browse API gave.
	f = newFixture(t)
	f.defBranch("ACME", "api", "main", mainSHA)
	f.file("a.txt", "FILE", []byte("abc"))
	f.handle(http.MethodGet, route+"/raw/a.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("abcd")) })
	_, err = f.reader.ReadFile(t.Context(), f.apiRepoOf(), "", "a.txt", 1024)
	wantClass(t, "a size mismatch", err, platform.ClassUnknown, nil)
}
