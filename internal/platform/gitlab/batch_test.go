package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

const optIn = ".engineering-assets.yml"

// gqlNode is a project node of batchFilesQuery's answer, as GitLab sends it
// (size a string: BigInt).
func gqlNode(id int, sha string, blobs ...map[string]any) map[string]any {
	var tree any
	if sha != "" {
		tree = map[string]any{"lastCommit": map[string]any{"sha": sha}}
	}
	if blobs == nil {
		blobs = []map[string]any{}
	}
	return map[string]any{"id": projectGID(strconv.Itoa(id)), "repository": map[string]any{
		"tree": tree, "blobs": map[string]any{"nodes": blobs},
	}}
}

func gqlBlobOf(path, mode, content string) map[string]any {
	return map[string]any{"path": path, "mode": mode, "oid": blobID([]byte(content), 40),
		"size": strconv.Itoa(len(content)), "storedExternally": false, "rawTextBlob": content}
}

// serveGraphQL answers POST /api/graphql with the nodes of the projects
// the request asks for, by global id.
func serveGraphQL(fx *fixture, nodes map[string]map[string]any) {
	fx.handle(http.MethodPost, "/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req gqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, msg("bad request"))
			return
		}
		var out []any
		ids, _ := req.Variables["ids"].([]any)
		for _, id := range ids {
			if n, ok := nodes[fmt.Sprint(id)]; ok {
				out = append(out, n)
			}
		}
		if out == nil {
			out = []any{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"projects": map[string]any{"nodes": out}}})
	})
}

// unreadable declares the files API of project id as GitLab answers a
// project it cannot find: ReadFile's ClassUnknown.
func unreadable(fx *fixture, id int) {
	fx.json(http.MethodGet, fmt.Sprintf("/projects/%d/repository/files/%s", id, escape(optIn)), http.StatusNotFound, msg("404 Project Not Found"))
}

func TestReadFiles(t *testing.T) {
	fx := newFixture(t)
	const sha = "c0ffee0000000000000000000000000000000001"
	text := "version: 1\npacks: [base]\n"
	nodes := map[string]map[string]any{}
	add := func(id int, n map[string]any) { nodes[projectGID(strconv.Itoa(id))] = n }
	add(1, gqlNode(1, sha, gqlBlobOf(optIn, "100644", text)))
	add(2, gqlNode(2, sha))
	add(3, gqlNode(3, sha, gqlBlobOf(optIn, "40000", "tree")))
	add(4, gqlNode(4, sha, gqlBlobOf(optIn, "120000", "target")))
	add(5, gqlNode(5, sha, gqlBlobOf(optIn, "100755", strings.Repeat("x", 300))))
	binary := gqlBlobOf(optIn, "100644", "\x00\x01")
	binary["rawTextBlob"] = nil
	add(6, gqlNode(6, sha, binary))
	add(7, gqlNode(7, ""))
	lfs := gqlBlobOf(optIn, "100644", "version https://git-lfs.github.com/spec/v1\n")
	lfs["storedExternally"] = true
	add(9, gqlNode(9, sha, lfs))
	serveGraphQL(fx, nodes)
	for _, id := range []int{6, 7, 8, 9} {
		unreadable(fx, id)
	}
	var repos []platform.Repo
	for id := 1; id <= 9; id++ {
		repos = append(repos, platform.Repo{Host: fx.provider().Host, ID: strconv.Itoa(id), Path: fmt.Sprintf("acme/r%d", id)})
	}
	repos = append(repos, platform.Repo{Host: fx.provider().Host, ID: "acme/byname", Path: "acme/byname"})
	fx.json(http.MethodGet, "/projects/acme%2Fbyname/repository/files/"+escape(optIn), http.StatusNotFound, msg("404 Project Not Found"))

	files, err := fx.reader.ReadFiles(t.Context(), repos, optIn, 256)
	var fe platform.FileErrors
	if !errors.As(err, &fe) || len(fe) != len(repos) {
		t.Fatalf("ReadFiles: %v", err)
	}
	if f := files[0]; fe[0] != nil || string(f.Content) != text || f.Mode != "100644" || f.OID != blobID([]byte(text), 40) {
		t.Errorf("a regular file: %+v, %v", f, fe[0])
	}
	if !errors.Is(fe[1], platform.ErrNotFound) {
		t.Errorf("a missing file: %v", fe[1])
	}
	for _, i := range []int{2, 3} {
		if !errors.Is(fe[i], platform.ErrNotRegular) {
			t.Errorf("r%d (a directory, a symlink): %v", i+1, fe[i])
		}
	}
	if !errors.Is(fe[4], platform.ErrTooLarge) {
		t.Errorf("a file over the limit: %v", fe[4])
	}
	// Binary content, an empty project, a project the answer lacks, LFS and
	// a project named by path are ReadFile's to judge: never ErrNotFound
	// from the batch.
	for _, i := range []int{5, 6, 7, 8, 9} {
		if fe[i] == nil || errors.Is(fe[i], platform.ErrNotFound) || platform.ClassOf(fe[i]) == platform.ClassNotFound {
			t.Errorf("%s: %v", repos[i].Path, fe[i])
		}
	}
	posts := fx.requests(http.MethodPost, "/api/graphql")
	if len(posts) != 1 {
		t.Fatalf("%d GraphQL requests, want 1", len(posts))
	}
	if posts[0].Auth != "Bearer "+fx.token || posts[0].Token != "" {
		t.Errorf("GraphQL credentials: Authorization %q, PRIVATE-TOKEN %q", posts[0].Auth, posts[0].Token)
	}
	var req gqlRequest
	if err := json.Unmarshal([]byte(posts[0].Body), &req); err != nil || req.Query != batchFilesQuery ||
		fmt.Sprint(req.Variables["paths"]) != "["+optIn+"]" || fmt.Sprint(req.Variables["first"]) != "9" {
		t.Errorf("the request: %v %+v", err, req)
	}
	for _, i := range []int{0, 1, 2, 3, 4} {
		if n := len(fx.requests(http.MethodGet, fmt.Sprintf("/projects/%s/repository/files/%s", repos[i].ID, escape(optIn)))); n != 0 {
			t.Errorf("%s: %d file reads, want none", repos[i].Path, n)
		}
	}
	if w := fx.writes(); len(w) != 1 {
		t.Errorf("writes %+v: only the GraphQL POST", w)
	}
}

// TestReadFilesChunks: 120 projects take three requests, in order; a nested
// path the answer lacks goes to ReadFile.
func TestReadFilesChunks(t *testing.T) {
	fx := newFixture(t)
	nodes := map[string]map[string]any{}
	var repos []platform.Repo
	for id := 1; id <= 120; id++ {
		nodes[projectGID(strconv.Itoa(id))] = gqlNode(id, "c0ffee0000000000000000000000000000000001")
		repos = append(repos, platform.Repo{Host: fx.provider().Host, ID: strconv.Itoa(id), Path: fmt.Sprintf("acme/r%d", id)})
	}
	serveGraphQL(fx, nodes)
	_, err := fx.reader.ReadFiles(t.Context(), repos, optIn, 256)
	var fe platform.FileErrors
	if !errors.As(err, &fe) {
		t.Fatalf("ReadFiles: %v", err)
	}
	for i, e := range fe {
		if !errors.Is(e, platform.ErrNotFound) {
			t.Errorf("r%d: %v", i+1, e)
		}
	}
	posts := fx.requests(http.MethodPost, "/api/graphql")
	if len(posts) != 3 {
		t.Fatalf("%d requests, want 3", len(posts))
	}
	for k, want := range []string{"50", "50", "20"} {
		var req gqlRequest
		_ = json.Unmarshal([]byte(posts[k].Body), &req)
		if got := fmt.Sprint(req.Variables["first"]); got != want {
			t.Errorf("request %d: first %s, want %s", k, got, want)
		}
	}

	fx.reset()
	unreadable2 := func(id int, p string) {
		fx.json(http.MethodGet, fmt.Sprintf("/projects/%d/repository/files/%s", id, escape(p)), http.StatusNotFound, msg("404 Project Not Found"))
	}
	unreadable2(1, "config/"+optIn)
	_, err = fx.reader.ReadFiles(t.Context(), repos[:1], "config/"+optIn, 256)
	if !errors.As(err, &fe) || errors.Is(fe[0], platform.ErrNotFound) {
		t.Errorf("a nested path the answer lacks: %v", err)
	}
}

// TestReadFilesFailures: a rate limit or a refused credential fails the
// call; an answer with errors reads its projects file by file, and from
// then on the client skips GraphQL.
func TestReadFilesFailures(t *testing.T) {
	repo := func(fx *fixture) []platform.Repo {
		return []platform.Repo{{Host: fx.provider().Host, ID: "1", Path: "acme/r1"}}
	}
	for _, tc := range []struct {
		status int
		class  platform.Class
	}{{http.StatusUnauthorized, platform.ClassAuth}, {http.StatusTooManyRequests, platform.ClassRateLimited}} {
		fx := newFixture(t)
		fx.json(http.MethodPost, "/api/graphql", tc.status, msg("no"))
		if _, err := fx.reader.ReadFiles(t.Context(), repo(fx), optIn, 256); platform.ClassOf(err) != tc.class {
			t.Errorf("HTTP %d: %v, want %v", tc.status, err, tc.class)
		}
	}

	fx := newFixture(t)
	fx.json(http.MethodPost, "/api/graphql", http.StatusOK, map[string]any{
		"data": nil, "errors": []any{map[string]any{"message": "Field 'blobs' doesn't exist on type 'Repository'"}},
	})
	unreadable(fx, 1)
	for run := range 2 {
		_, err := fx.reader.ReadFiles(t.Context(), repo(fx), optIn, 256)
		var fe platform.FileErrors
		if !errors.As(err, &fe) || errors.Is(fe[0], platform.ErrNotFound) {
			t.Errorf("run %d: %v", run, err)
		}
	}
	if n := len(fx.requests(http.MethodPost, "/api/graphql")); n != 1 {
		t.Errorf("%d GraphQL requests, want 1: the second call reads file by file", n)
	}
	if n := len(fx.requests(http.MethodGet, "/projects/1/repository/files/"+escape(optIn))); n != 2 {
		t.Errorf("%d file reads, want 2", n)
	}
}
