package github

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// treeFile is a file of a repository fixture: mode 100644 when empty;
// 120000 is a symlink (content its target), 160000 a submodule (content
// the commit id).
type treeFile struct {
	path, mode, content string
}

// gitTree declares the trees and blobs of repository fullName at refs
// (each answers with the root tree), as the git trees and blobs APIs send
// them (not recursive), and returns the blob ids by path.
func (s *apiServer) gitTree(fullName string, refs []string, files []treeFile) map[string]string {
	type dir struct {
		entries map[string]map[string]any
	}
	dirs := map[string]*dir{"": {entries: map[string]map[string]any{}}}
	blobs := map[string]string{}
	var ensure func(p string) *dir
	ensure = func(p string) *dir {
		if d, ok := dirs[p]; ok {
			return d
		}
		d := &dir{entries: map[string]map[string]any{}}
		dirs[p] = d
		parent, name := "", p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			parent, name = p[:i], p[i+1:]
		}
		ensure(parent).entries[name] = map[string]any{"path": name, "mode": "040000", "type": "tree"}
		return d
	}
	for _, f := range files {
		parent, name := "", f.path
		if i := strings.LastIndex(f.path, "/"); i >= 0 {
			parent, name = f.path[:i], f.path[i+1:]
		}
		mode := cmpStr(f.mode, "100644")
		e := map[string]any{"path": name, "mode": mode, "type": "blob"}
		switch mode {
		case "160000":
			e["type"], e["sha"] = "commit", f.content
		default:
			id := gitBlobID(f.content)
			e["sha"], e["size"] = id, len(f.content)
			blobs[f.path] = id
			s.json(http.MethodGet, "/repos/"+fullName+"/git/blobs/"+id, http.StatusOK, map[string]any{
				"sha": id, "node_id": "B_" + id[:8], "size": len(f.content), "url": "", "content": b64(f.content), "encoding": "base64"})
		}
		ensure(parent).entries[name] = e
	}
	// Tree ids: a hash of the directory's path and names (made up, stable).
	ids := map[string]string{}
	var paths []string
	for p := range dirs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sum := sha1.Sum([]byte(fullName + "\x00" + p))
		ids[p] = hex.EncodeToString(sum[:])
	}
	for _, p := range paths {
		d := dirs[p]
		var entries []any
		names := make([]string, 0, len(d.entries))
		for n := range d.entries {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			e := d.entries[n]
			if e["type"] == "tree" {
				child := n
				if p != "" {
					child = p + "/" + n
				}
				e["sha"] = ids[child]
			}
			entries = append(entries, e)
		}
		body := map[string]any{"sha": ids[p], "url": "", "tree": entries, "truncated": false}
		if p == "" {
			for _, ref := range refs {
				s.json(http.MethodGet, "/repos/"+fullName+"/git/trees/"+ref, http.StatusOK, body)
			}
		}
		s.json(http.MethodGet, "/repos/"+fullName+"/git/trees/"+ids[p], http.StatusOK, body)
	}
	return blobs
}

// TestReadFile covers regular files, modes, refs, missing paths and every
// kind of entry that is not a regular file.
func TestReadFile(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	readme, script, guide := "# api\n\nline two ⚠\n", "#!/bin/sh\necho hi\n", "guide\n"
	head := strings.Repeat("c", 40)
	f.gitTree("acme/api", []string{"HEAD", head, "main"}, []treeFile{
		{path: "README.md", content: readme},
		{path: "scripts/run.sh", mode: "100755", content: script},
		{path: "docs/guide/intro.md", content: guide},
		{path: "target.md", content: "secret of the target\n"},
		{path: "link.md", mode: "120000", content: "target.md"},
		{path: "dirlink", mode: "120000", content: "docs"},
		{path: "vendor/lib", mode: "160000", content: strings.Repeat("d", 40)},
	})
	r := platform.Repo{Host: "github.com", ID: "101", Path: "acme/api", DefaultBranch: "main"}
	for _, tc := range []struct {
		ref, path, mode, content string
	}{
		{"", "README.md", "100644", readme},
		{"", "scripts/run.sh", "100755", script},
		{"", "docs/guide/intro.md", "100644", guide},
		{"main", "README.md", "100644", readme},
		{head, "scripts/run.sh", "100755", script},
	} {
		got, err := f.reader.ReadFile(t.Context(), r, tc.ref, tc.path, 64<<10)
		if err != nil {
			t.Errorf("ReadFile %s at %q: %v", tc.path, tc.ref, err)
			continue
		}
		if got.Path != tc.path || got.Mode != tc.mode || string(got.Content) != tc.content || got.OID != gitBlobID(tc.content) {
			t.Errorf("ReadFile %s at %q = %+v", tc.path, tc.ref, got)
		}
	}
	for _, p := range []string{"docs", "docs/guide", "link.md", "vendor/lib", "dirlink/guide/intro.md"} {
		_, err := f.reader.ReadFile(t.Context(), r, "", p, 64<<10)
		if !errors.Is(err, platform.ErrNotRegular) {
			t.Errorf("ReadFile %s: %v, want ErrNotRegular", p, err)
		}
	}
	for _, p := range []string{"missing.md", "docs/missing.md", "README.md/x", "missing/x.md"} {
		_, err := f.reader.ReadFile(t.Context(), r, "", p, 64<<10)
		wantClass(t, "missing "+p, err, platform.ClassNotFound, platform.ErrNotFound)
	}
	// At the limit, over it: the entry's size says so before the blob.
	if _, err := f.reader.ReadFile(t.Context(), r, "", "README.md", int64(len(readme))); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	f.reset()
	_, err := f.reader.ReadFile(t.Context(), r, "", "README.md", int64(len(readme))-1)
	if !errors.Is(err, platform.ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	for _, c := range f.requests("", "") {
		if strings.Contains(c.Path, "/git/blobs/") {
			t.Errorf("a blob over the limit was read: %s", c.Path)
		}
	}
	for _, p := range []string{"", "/x", "a//b", "a/../b", "a\x00b"} {
		_, err := f.reader.ReadFile(t.Context(), r, "", p, 64<<10)
		wantClass(t, "path "+p, err, platform.ClassInvalid, nil)
	}
}

// TestReadFileMissingVersusGone: a repository without commits (409) and a
// missing repository or ref are ErrNotFound; a subtree or blob the tree
// named that answers 404, or content that is not the blob, is unknown:
// never a missing opt-in file.
func TestReadFileMissingVersusGone(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	r := platform.Repo{Host: "github.com", ID: "101", Path: "acme/api"}
	f.json(http.MethodGet, "/repos/acme/api/git/trees/HEAD", http.StatusConflict, ghError("Git Repository is empty."))
	_, err := f.reader.ReadFile(t.Context(), r, "", "README.md", 1<<10)
	wantClass(t, "empty repository", err, platform.ClassNotFound, platform.ErrNotFound)

	f.json(http.MethodGet, "/repos/acme/api/git/trees/HEAD", http.StatusNotFound, notFoundBody)
	_, err = f.reader.ReadFile(t.Context(), r, "", "README.md", 1<<10)
	wantClass(t, "missing repository", err, platform.ClassNotFound, platform.ErrNotFound)

	blobs := f.gitTree("acme/api", []string{"HEAD"}, []treeFile{{path: "docs/a.md", content: "a\n"}, {path: "b.md", content: "b\n"}})
	sum := sha1.Sum([]byte("acme/api\x00docs")) // the id gitTree gives docs
	docs := hex.EncodeToString(sum[:])
	f.json(http.MethodGet, "/repos/acme/api/git/trees/"+docs, http.StatusNotFound, notFoundBody)
	_, err = f.reader.ReadFile(t.Context(), r, "", "docs/a.md", 1<<10)
	wantClass(t, "subtree gone", err, platform.ClassUnknown, nil)
	if errors.Is(err, platform.ErrNotFound) {
		t.Error("a subtree 404 is ErrNotFound")
	}

	f.json(http.MethodGet, "/repos/acme/api/git/blobs/"+blobs["b.md"], http.StatusNotFound, notFoundBody)
	_, err = f.reader.ReadFile(t.Context(), r, "", "b.md", 1<<10)
	wantClass(t, "blob gone", err, platform.ClassUnknown, nil)

	f.json(http.MethodGet, "/repos/acme/api/git/blobs/"+blobs["b.md"], http.StatusOK, map[string]any{
		"sha": blobs["b.md"], "size": 2, "content": b64("c\n"), "encoding": "base64"})
	_, err = f.reader.ReadFile(t.Context(), r, "", "b.md", 1<<10)
	wantClass(t, "content of another blob", err, platform.ClassUnknown, nil)
}

// TestReadFiles: one GraphQL request per 50 repositories of an owner, a
// File or ReadFile's error per repository, text checked against its blob
// id, binary content read through the blob API.
func TestReadFiles(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	const optIn = ".github/touchmark.yml"
	text := "version: 1\npacks: [agents]\n"
	binary := "\x00\x01\x02 not text"
	commit := strings.Repeat("e", 40)
	var repos []platform.Repo
	for i := range 53 {
		repos = append(repos, platform.Repo{Host: "github.com", ID: itoa(int64(500 + i)), Path: "acme/r" + itoa(int64(i))})
	}
	repos = append(repos, platform.Repo{Host: "github.com", ID: "600", Path: "alice/dots"})
	// r0 regular, r1 executable, r2 symlink, r3 missing file, r4 missing
	// repository, r5 empty, r6 binary (read through REST), r7 too large, r8
	// text that is not the blob's (read through REST).
	f.gitTree("acme/r6", []string{commit}, []treeFile{{path: optIn, content: binary}})
	f.gitTree("acme/r8", []string{commit}, []treeFile{{path: optIn, content: text}})
	entry := func(mode int, content string, blob map[string]any) map[string]any {
		return map[string]any{"mode": mode, "type": "blob", "oid": gitBlobID(content), "size": len(content), "object": blob}
	}
	textBlob := func(content string) map[string]any {
		return map[string]any{"byteSize": len(content), "isBinary": false, "isTruncated": false, "oid": gitBlobID(content), "text": content}
	}
	f.graphql("r0: repository(owner: $o0, name: $n0)", func(w http.ResponseWriter, req gqlCall) {
		if req.Variables["path"] != optIn {
			t.Errorf("path %v", req.Variables["path"])
		}
		data := map[string]any{}
		var errs []map[string]any
		for j := 0; ; j++ {
			o, ok := req.Variables["o"+itoa(int64(j))].(string)
			if !ok {
				break
			}
			alias := "r" + itoa(int64(j))
			n := req.Variables["n"+itoa(int64(j))].(string)
			id := int64(500)
			if o == "acme" {
				id += int64(atoi(strings.TrimPrefix(n, "r")))
			} else {
				id = 600
			}
			withFile := func(file any) map[string]any {
				return map[string]any{"databaseId": id, "object": map[string]any{"oid": commit, "file": file}}
			}
			switch n {
			case "r1":
				data[alias] = withFile(entry(33261, text, textBlob(text)))
			case "r2":
				data[alias] = withFile(map[string]any{"mode": 40960, "type": "blob", "oid": gitBlobID("x"), "size": 1, "object": textBlob("x")})
			case "r3":
				data[alias] = withFile(nil)
				errs = append(errs, gqlErr("NOT_FOUND", "Could not resolve file for path '"+optIn+"'.", alias, "object", "file"))
			case "r4":
				data[alias] = nil
				errs = append(errs, gqlErr("NOT_FOUND", "Could not resolve to a Repository with the name 'acme/r4'.", alias))
			case "r5":
				data[alias] = map[string]any{"databaseId": id, "object": nil}
			case "r6":
				data[alias] = withFile(entry(33188, binary, map[string]any{"byteSize": len(binary), "isBinary": true, "isTruncated": false,
					"oid": gitBlobID(binary), "text": nil}))
			case "r7":
				big := strings.Repeat("x", 70<<10)
				data[alias] = withFile(entry(33188, big, textBlob(big)))
			case "r8":
				data[alias] = withFile(entry(33188, text, map[string]any{"byteSize": len(text), "isBinary": false, "isTruncated": false,
					"oid": gitBlobID(text), "text": "version: 1\npacks: [agents]\r\n"}))
			default:
				data[alias] = withFile(entry(33188, text, textBlob(text)))
			}
		}
		gqlData(w, data, errs...)
	})
	files, err := f.reader.ReadFiles(t.Context(), repos, optIn, 64<<10)
	var fe platform.FileErrors
	if !errors.As(err, &fe) || len(files) != len(repos) || len(fe) != len(repos) {
		t.Fatalf("ReadFiles: %d files, %v", len(files), err)
	}
	for i, r := range repos {
		name := strings.Split(r.Path, "/")[1]
		switch name {
		case "r2":
			if !errors.Is(fe[i], platform.ErrNotRegular) {
				t.Errorf("%s: %v", name, fe[i])
			}
		case "r3", "r4", "r5":
			wantClass(t, name, fe[i], platform.ClassNotFound, platform.ErrNotFound)
		case "r7":
			if !errors.Is(fe[i], platform.ErrTooLarge) {
				t.Errorf("%s: %v", name, fe[i])
			}
		default:
			want, mode := text, "100644"
			switch name {
			case "r1":
				mode = "100755"
			case "r6":
				want = binary
			}
			if fe[i] != nil || files[i].Path != optIn || files[i].Mode != mode || !bytes.Equal(files[i].Content, []byte(want)) || files[i].OID != gitBlobID(want) {
				t.Errorf("%s: %+v, %v", name, files[i], fe[i])
			}
		}
	}
	calls := f.gqlCalls()
	if len(calls) != 3 {
		t.Fatalf("%d GraphQL requests, want 2 for acme (50 + 3) and 1 for alice", len(calls))
	}
	for _, c := range calls[:2] {
		if m, ok := f.tokenOf(c.Auth); !ok || m.installation != instAcme {
			t.Errorf("acme batch with %v", m)
		}
	}
	if m, ok := f.tokenOf(calls[2].Auth); !ok || m.installation != instAlice {
		t.Errorf("alice batch with %v", m)
	}
	// The query holds no data: owners, names and the path are variables.
	for _, c := range calls {
		if strings.Contains(c.Body, `repository(owner: \"acme\"`) || strings.Contains(c.Body, `file(path: \".github`) {
			t.Errorf("data in the query text: %s", c.Body[:200])
		}
	}
	for _, name := range []string{"acme/r6", "acme/r8"} {
		if n := len(f.requests(http.MethodGet, "/repos/"+name+"/git/trees/"+commit)); n != 1 {
			t.Errorf("%s: read through REST %d times at the commit seen", name, n)
		}
	}
}

// TestReadFilesFailures: a rate limit fails the call; any other failure of
// a whole request is the failure of its repositories.
func TestReadFilesFailures(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	repos := []platform.Repo{{Host: "github.com", ID: "1", Path: "acme/a"}, {Host: "gitlab.example.com", ID: "2", Path: "acme/b"}}
	f.graphql("r0: repository", func(w http.ResponseWriter, _ gqlCall) {
		gqlData(w, nil, gqlErr("RATE_LIMITED", "API rate limit exceeded"))
	})
	_, err := f.reader.ReadFiles(t.Context(), repos, "x.yml", 1<<10)
	wantClass(t, "rate limit", err, platform.ClassRateLimited, nil)
	var fe platform.FileErrors
	if errors.As(err, &fe) {
		t.Error("a rate limit is not per repository")
	}
	f.graphql("r0: repository", func(w http.ResponseWriter, _ gqlCall) {
		gqlData(w, nil, gqlErr("FORBIDDEN", "Resource not accessible by integration"))
	})
	_, err = f.reader.ReadFiles(t.Context(), repos, "x.yml", 1<<10)
	if !errors.As(err, &fe) || platform.ClassOf(fe[0]) != platform.ClassPermission || platform.ClassOf(fe[1]) != platform.ClassInvalid {
		t.Errorf("per repository: %v", err)
	}
}

// TestReadFilesAnonymous: without GraphQL, file by file.
func TestReadFilesAnonymous(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credAnonymous, host: "github.com"})
	f.gitTree("acme/a", []string{"HEAD"}, []treeFile{{path: "x.yml", content: "a: 1\n"}})
	f.json(http.MethodGet, "/repos/acme/b/git/trees/HEAD", http.StatusNotFound, notFoundBody)
	files, err := f.reader.ReadFiles(t.Context(), []platform.Repo{{ID: "1", Path: "acme/a"}, {ID: "2", Path: "acme/b"}}, "x.yml", 1<<10)
	var fe platform.FileErrors
	if !errors.As(err, &fe) || string(files[0].Content) != "a: 1\n" || fe[0] != nil || !errors.Is(fe[1], platform.ErrNotFound) {
		t.Errorf("files %+v, %v", files, err)
	}
	if n := len(f.gqlCalls()); n != 0 {
		t.Errorf("%d GraphQL requests without a credential", n)
	}
	for _, c := range f.requests("", "") {
		if c.Auth != "" {
			t.Errorf("%s with a credential", c.Path)
		}
	}
}

// atoi is strconv.Atoi without the error, for fixtures.
func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
