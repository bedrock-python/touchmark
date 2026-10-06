package githube2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
)

// shapeSpec is one recorded github.com answer of testdata/shapes.json.
type shapeSpec struct {
	Request string `json:"request"`
	// Status is the HTTP status GitHub answered (200 when 0).
	Status int `json:"status"`
	// Shape is the answer reduced to JSON types ("string", "number",
	// "boolean", "null", "a|b"); objects list the fields that must be
	// there, a one-element array the shape of every element.
	Shape any `json:"shape"`
	// Values pins fields by dotted path (array indices are numbers).
	Values map[string]any `json:"values"`
	// Absent lists fields GitHub leaves out.
	Absent []string `json:"absent"`
	// Prefix, Suffix and NoSuffix constrain string fields.
	Prefix   map[string]string `json:"prefix"`
	Suffix   map[string]string `json:"suffix"`
	NoSuffix map[string]string `json:"nosuffix"`
	// Link is the order of the rels of a Link header.
	Link []string `json:"link"`
}

// TestShapes asks the fake what testdata/shapes.json recorded from
// github.com and compares the answers: every recorded field with its type,
// the pinned values, the fields GitHub leaves out, the statuses and the
// Link header. The driver's own fixtures (internal/platform/github) follow
// the same recordings; a difference here is a difference between the fake
// and GitHub.
func TestShapes(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/shapes.json")
	check(t, err)
	var file struct {
		Shapes map[string]shapeSpec `json:"shapes"`
	}
	check(t, json.Unmarshal(data, &file))
	sw := newShapeWorld(t)
	answers := sw.answers()
	for _, name := range sortedKeys(file.Shapes) {
		spec := file.Shapes[name]
		t.Run(name, func(t *testing.T) {
			got, ok := answers[name]
			if !ok {
				t.Fatalf("no request of the fake answers %q (%s)", name, spec.Request)
			}
			want := spec.Status
			if want == 0 {
				want = http.StatusOK
			}
			if got.status != want {
				t.Errorf("HTTP %d, GitHub %d", got.status, want)
			}
			if spec.Shape != nil {
				matchShape(t, "", spec.Shape, got.body)
			}
			for p, want := range spec.Values {
				v, ok := lookup(got.body, p)
				if !ok || !sameJSON(v, want) {
					t.Errorf("%s = %v (present %v), GitHub %v", p, v, ok, want)
				}
			}
			for _, p := range spec.Absent {
				if v, ok := lookup(got.body, p); ok {
					t.Errorf("%s = %v, GitHub leaves it out", p, v)
				}
			}
			for p, want := range spec.Prefix {
				if s, _ := lookupString(got.body, p); !strings.HasPrefix(s, want) {
					t.Errorf("%s = %q, GitHub's starts with %q", p, s, want)
				}
			}
			for p, want := range spec.Suffix {
				if s, _ := lookupString(got.body, p); !strings.HasSuffix(s, want) {
					t.Errorf("%s = %q, GitHub's ends with %q", p, s, want)
				}
			}
			for p, want := range spec.NoSuffix {
				if s, _ := lookupString(got.body, p); strings.HasSuffix(s, want) {
					t.Errorf("%s = %q, GitHub's does not end with %q", p, s, want)
				}
			}
			if spec.Link != nil {
				checkLink(t, got.header.Get("Link"), spec.Link)
			}
		})
	}
	sw.w.violations()
}

// answer is what the fake answered one request.
type answer struct {
	status int
	header http.Header
	body   any
}

// shapeWorld is a fake prepared with one of each thing the shapes show.
type shapeWorld struct {
	t    *testing.T
	w    *world
	repo string // acme/shapes
	// signed is a commit GitHub signed.
	signed string
}

func newShapeWorld(t *testing.T) *shapeWorld {
	t.Helper()
	w := newWorld(t, worldOptions{flavor: ghfake.DotCom})
	srv := w.srv
	sw := &shapeWorld{t: t, w: w, repo: org + "/shapes"}
	try(srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: "shapes", Topics: []string{"shapes"}, Files: []ghfake.File{
		{Path: "Makefile", Content: []byte("all:\n\ttrue\n")},
		{Path: "VERSION-GEN", Mode: ghfake.ModeExecutable, Content: []byte("#!/bin/sh\necho 1\n")},
		{Path: "docs/notes.md", Content: []byte("notes\n")},
		{Path: "NOTES", Mode: ghfake.ModeSymlink, Content: []byte("docs/notes.md")},
		{Path: "lib", Mode: ghfake.ModeGitlink, Content: []byte("0123456789abcdef0123456789abcdef01234567")},
		{Path: ".gitmodules", Content: []byte("[submodule \"lib\"]\n\tpath = lib\n\turl = https://example.com/lib.git\n")},
	}})).of(t)
	check(t, srv.Grant(sw.repo, person, "write"))
	try(srv.AddRuleset(sw.repo, ghfake.Ruleset{Name: "main", Include: []string{"~DEFAULT_BRANCH"},
		Rules: []ghfake.Rule{{Type: ghfake.RuleNonFastForward}}})).of(t)
	sw.signed = try(srv.Commit(sw.repo, ghfake.CommitSpec{Author: person, Signed: true,
		Files: []ghfake.File{{Path: "CHANGES.md", Content: []byte("changes\n")}}, Message: "Update CHANGES.md"})).of(t)
	// A pull request from alice's fork, labeled and closed by her; one of
	// the writer's bot that the bot closed; one more for the pages.
	fork := try(srv.Fork(sw.repo, person)).of(t)
	try(srv.Commit(fork.FullName, ghfake.CommitSpec{Branch: "feature", Author: person,
		Files: []ghfake.File{{Path: "feature.md", Content: []byte("feature\n")}}})).of(t)
	forkPR := try(srv.OpenPR(sw.repo, ghfake.PRSpec{Head: "feature", HeadRepo: fork.FullName, Title: "a feature", Body: "body",
		Labels: []string{"enhancement"}, Author: person})).of(t)
	check(t, srv.SetPRState(sw.repo, forkPR.Number, "closed", person))
	bot := w.writeApp.Bot.Login
	try(srv.Commit(sw.repo, ghfake.CommitSpec{Branch: "touchmark/hub", Author: bot,
		Files: []ghfake.File{{Path: "AGENTS.md", Content: []byte("agents\n")}}})).of(t)
	botPR := try(srv.OpenPR(sw.repo, ghfake.PRSpec{Head: "touchmark/hub", Title: "sync", Body: "sync", Author: bot})).of(t)
	check(t, srv.SetPRState(sw.repo, botPR.Number, "closed", bot))
	try(srv.Commit(sw.repo, ghfake.CommitSpec{Branch: "docs", Author: person,
		Files: []ghfake.File{{Path: "more.md", Content: []byte("more\n")}}})).of(t)
	try(srv.OpenPR(sw.repo, ghfake.PRSpec{Head: "docs", Title: "docs", Body: "docs", Author: person})).of(t)
	return sw
}

// rest sends a REST request as alice (a classic personal access token).
func (sw *shapeWorld) rest(method, path string) answer {
	sw.t.Helper()
	req, err := http.NewRequest(method, sw.w.srv.APIURL()+path, nil)
	check(sw.t, err)
	req.Header.Set("Authorization", "Bearer "+sw.w.personToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return sw.do(req)
}

// graphql sends a GraphQL query as alice.
func (sw *shapeWorld) graphql(query string) answer {
	sw.t.Helper()
	body, err := json.Marshal(map[string]any{"query": query})
	check(sw.t, err)
	req, err := http.NewRequest(http.MethodPost, sw.w.srv.GraphQLURL(), bytes.NewReader(body))
	check(sw.t, err)
	req.Header.Set("Authorization", "Bearer "+sw.w.personToken)
	req.Header.Set("Content-Type", "application/json")
	return sw.do(req)
}

func (sw *shapeWorld) do(req *http.Request) answer {
	sw.t.Helper()
	resp, err := sw.w.srv.Client().Do(req)
	check(sw.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	check(sw.t, err)
	var v any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &v); err != nil {
			sw.t.Fatalf("%s %s: HTTP %d, not JSON: %s", req.Method, req.URL.Path, resp.StatusCode, data)
		}
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: v}
}

// answers asks the fake every request of the shapes, by shape name.
func (sw *shapeWorld) answers() map[string]answer {
	t := sw.t
	repo := "/repos/" + sw.repo
	out := map[string]answer{}
	out["repo"] = sw.rest(http.MethodGet, repo)
	list := sw.rest(http.MethodGet, "/orgs/"+org+"/repos?type=all&per_page=100")
	out["repo_list_item"] = pick(t, list, func(v map[string]any) bool { return v["full_name"] == sw.repo })
	pulls := sw.rest(http.MethodGet, repo+"/pulls?state=closed&per_page=100")
	out["pull"] = pick(t, pulls, func(v map[string]any) bool {
		head, _ := v["head"].(map[string]any)
		r, _ := head["repo"].(map[string]any)
		return r["fork"] == true
	})
	tree := sw.rest(http.MethodGet, repo+"/git/trees/HEAD")
	byMode := map[string]any{}
	if m, ok := tree.body.(map[string]any); ok {
		byMode["sha"], byMode["truncated"] = m["sha"], m["truncated"]
		entries, _ := m["tree"].([]any)
		for kind, mode := range map[string]string{"blob": "100644", "exec": "100755", "link": "120000", "tree": "040000", "submodule": "160000"} {
			for _, e := range entries {
				if em, _ := e.(map[string]any); em["mode"] == mode {
					byMode[kind] = em
					break
				}
			}
		}
	}
	out["tree"] = answer{status: tree.status, header: tree.header, body: byMode}
	blobSHA, _ := lookupString(byMode, "blob.sha")
	out["blob"] = sw.rest(http.MethodGet, repo+"/git/blobs/"+blobSHA)
	out["contents_file"] = sw.rest(http.MethodGet, repo+"/contents/Makefile")
	out["contents_symlink"] = sw.rest(http.MethodGet, repo+"/contents/NOTES")
	out["contents_submodule"] = sw.rest(http.MethodGet, repo+"/contents/lib")
	rules := sw.rest(http.MethodGet, repo+"/rules/branches/main")
	out["rules_branch"] = pick(t, rules, func(v map[string]any) bool { return v["type"] == ghfake.RuleNonFastForward })
	out["rules_missing_branch"] = sw.rest(http.MethodGet, "/repos/"+sw.plainRepo()+"/rules/branches/touchmark-no-such-branch")
	out["hash_algorithm"] = sw.rest(http.MethodGet, repo+"/hash-algorithm")
	out["bot"] = sw.rest(http.MethodGet, "/users/"+url.PathEscape(sw.w.writeApp.Bot.Login))
	out["user"] = sw.rest(http.MethodGet, "/users/"+person)
	out["label"] = pick(t, sw.rest(http.MethodGet, repo+"/labels"), func(map[string]any) bool { return true })
	out["not_found"] = sw.rest(http.MethodGet, "/repos/"+org+"/touchmark-no-such-repo")
	out["git_commit"] = sw.rest(http.MethodGet, repo+"/git/commits/"+sw.signed)
	out["meta"] = sw.rest(http.MethodGet, "/meta")
	out["pulls_link"] = sw.rest(http.MethodGet, repo+"/pulls?state=all&per_page=1&page=2")
	out["graphql_not_found"] = sw.graphql(fmt.Sprintf(`query { a: repository(owner: %q, name: "shapes") { databaseId } b: repository(owner: %q, name: "touchmark-no-such-repo") { databaseId } }`, org, org))
	out["graphql_file_not_found"] = sw.graphql(fmt.Sprintf(`query { repository(owner: %q, name: "shapes") { object(expression: "HEAD") { ... on Commit { e: file(path: "no/such/path") { mode } } } } }`, org))
	out["graphql_invalid"] = sw.graphql(fmt.Sprintf(`query { repository(owner: %q, name: "shapes") { noSuchField } }`, org))
	out["graphql_unused_variable"] = sw.graphql(fmt.Sprintf(`query($unused: String) { repository(owner: %q, name: "shapes") { databaseId } }`, org))
	modes := sw.graphql(fmt.Sprintf(`query { repository(owner: %q, name: "shapes") { object(expression: "HEAD") { ... on Commit {
  link: file(path: "NOTES") { mode type size object { __typename } }
  submodule: file(path: "lib") { mode type size object { __typename } }
  exec: file(path: "VERSION-GEN") { mode type size object { __typename } }
  dir: file(path: "docs") { mode type size object { __typename } } } } } }`, org))
	modesObj, _ := lookup(modes.body, "data.repository.object")
	out["graphql_file_modes"] = answer{status: modes.status, header: modes.header, body: modesObj}
	closed := sw.graphql(fmt.Sprintf(`query { repository(owner: %q, name: "shapes") { pullRequests(states: [CLOSED], first: 10) { nodes { author { login }
  timelineItems(itemTypes: [CLOSED_EVENT], last: 1) { nodes { __typename ... on ClosedEvent { createdAt actor { __typename login ... on User { databaseId } ... on Bot { databaseId } ... on Mannequin { databaseId } } } } } } } } }`, org))
	var event any
	if nodes, ok := lookup(closed.body, "data.repository.pullRequests.nodes"); ok {
		for _, n := range nodes.([]any) {
			if ev, ok := lookup(n, "timelineItems.nodes.0"); ok {
				if typ, _ := lookupString(ev, "actor.__typename"); typ == "Bot" {
					event = ev
				}
			}
		}
	}
	out["graphql_closed_event"] = answer{status: closed.status, header: closed.header, body: event}
	out["git_ref"] = sw.rest(http.MethodGet, repo+"/git/ref/heads/touchmark/hub")
	out["git_ref_missing"] = sw.rest(http.MethodGet, repo+"/git/ref/heads/touch")
	open := sw.graphql(fmt.Sprintf(`query { repository(owner: %q, name: "shapes") { pullRequests(states: [OPEN], first: 1) { nodes {
  id databaseId number url state isDraft title body createdAt closedAt mergedAt headRefName headRefOid baseRefName baseRef { name }
  headRepository { databaseId } repository { databaseId nameWithOwner }
  author { __typename login ... on User { databaseId } ... on Bot { databaseId } ... on Mannequin { databaseId } }
  labels(first: 20) { nodes { name } } } } } }`, org))
	node, _ := lookup(open.body, "data.repository.pullRequests.nodes.0")
	out["graphql_pull_request"] = answer{status: open.status, header: open.header, body: node}
	return out
}

// plainRepo creates a repository without rulesets and returns its path.
func (sw *shapeWorld) plainRepo() string {
	sw.t.Helper()
	r := try(sw.w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: "plain",
		Files: []ghfake.File{{Path: "README.md", Content: []byte("plain\n")}}})).of(sw.t)
	return r.FullName
}

// pick returns the first element of a listing answer that keep accepts.
func pick(t *testing.T, a answer, keep func(map[string]any) bool) answer {
	t.Helper()
	list, ok := a.body.([]any)
	if !ok {
		t.Fatalf("HTTP %d: not a listing: %v", a.status, a.body)
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && keep(m) {
			return answer{status: a.status, header: a.header, body: m}
		}
	}
	t.Fatalf("HTTP %d: no element of %d fits", a.status, len(list))
	return answer{}
}

// jsonType names the JSON type of a decoded value.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// matchShape checks got against the recorded shape want at path.
func matchShape(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case string:
		if typ := jsonType(got); !slices.Contains(strings.Split(w, "|"), typ) {
			t.Errorf("%s: %s, GitHub %s", orRoot(path), typ, w)
		}
	case map[string]any:
		m, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: %s, GitHub an object", orRoot(path), jsonType(got))
			return
		}
		for k, sub := range w {
			v, ok := m[k]
			if !ok {
				t.Errorf("%s: missing, GitHub has it", join(path, k))
				continue
			}
			matchShape(t, join(path, k), sub, v)
		}
	case []any:
		list, ok := got.([]any)
		if !ok {
			t.Errorf("%s: %s, GitHub an array", orRoot(path), jsonType(got))
			return
		}
		if len(w) == 0 {
			if len(list) != 0 {
				t.Errorf("%s: %d elements, GitHub none", orRoot(path), len(list))
			}
			return
		}
		if len(list) == 0 {
			t.Errorf("%s: empty, GitHub's has elements; the fixture must make one", orRoot(path))
		}
		for i, item := range list {
			matchShape(t, join(path, strconv.Itoa(i)), w[0], item)
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func orRoot(path string) string {
	if path == "" {
		return "the answer"
	}
	return path
}

// lookup returns the value at a dotted path; numbers index arrays.
func lookup(v any, path string) (any, bool) {
	for _, key := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[key]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func lookupString(v any, path string) (string, bool) {
	x, ok := lookup(v, path)
	s, isString := x.(string)
	return s, ok && isString
}

// sameJSON compares decoded JSON values; numbers of the testdata decode as
// float64 like the answers.
func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// linkRe is one link of a Link header.
var linkRe = regexp.MustCompile(`<([^>]+)>; rel="([a-z]+)"`)

// checkLink checks the rels of a Link header, in order, and GitHub's URL
// form: /repositories/<id>/… with the request's query and page last.
func checkLink(t *testing.T, header string, rels []string) {
	t.Helper()
	var got []string
	for _, m := range linkRe.FindAllStringSubmatch(header, -1) {
		got = append(got, m[2])
		u, err := url.Parse(m[1])
		switch {
		case err != nil:
			t.Errorf("link %q: %v", m[1], err)
		case !regexp.MustCompile(`^/repositories/[0-9]+/pulls$`).MatchString(u.Path):
			t.Errorf("link %s: path %s, GitHub /repositories/<id>/pulls", m[2], u.Path)
		case !regexp.MustCompile(`^state=all&per_page=1&page=[0-9]+$`).MatchString(u.RawQuery):
			t.Errorf("link %s: query %s, GitHub keeps the request's order with page last", m[2], u.RawQuery)
		}
	}
	if !slices.Equal(got, rels) {
		t.Errorf("Link rels %v, GitHub %v (%s)", got, rels, header)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
