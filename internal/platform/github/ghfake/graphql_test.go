package ghfake

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGraphQLBatchRead(t *testing.T) {
	w := contentWorld(t)
	tok := w.token(nil, Permissions{"contents": Read, "metadata": Read, "pull_requests": Read})
	query := `query OptIn {
  r0: repository(owner: "acme", name: "files") {
    id databaseId nameWithOwner isEmpty isArchived visibility
    defaultBranchRef { name prefix target { oid ... on Commit { tree { oid } } } }
    file: object(expression: "HEAD:AGENTS.md") { __typename ... on Blob { oid byteSize isBinary isTruncated text } }
    link: object(expression: "HEAD:CLAUDE.md") { ...blob }
    sub: object(expression: "HEAD:vendor/lib") { __typename }
    dir: object(expression: "HEAD:docs") { __typename ... on Tree { entries { name path mode type size } } }
    missing: object(expression: "HEAD:nope.yml") { __typename }
  }
  r1: repository(owner: "acme", name: "nothing") { id }
}
fragment blob on Blob { oid byteSize text }`
	got := w.graphql(tok, query, nil)
	data := got["data"].(map[string]any)
	r0 := data["r0"].(map[string]any)
	if r0["nameWithOwner"] != "acme/files" || r0["isEmpty"] != false || r0["visibility"] != "PUBLIC" ||
		field(r0, "defaultBranchRef", "name") != "main" || field(r0, "defaultBranchRef", "prefix") != "refs/heads/" {
		t.Errorf("r0: %v", r0)
	}
	if field(r0, "file", "__typename") != "Blob" || field(r0, "file", "byteSize") != float64(120) ||
		!strings.HasPrefix(field(r0, "file", "text").(string), "agents line") {
		t.Errorf("file: %v", r0["file"])
	}
	// Observed: a symlink is the Blob of its target text, a submodule null.
	if field(r0, "link", "text") != "AGENTS.md" || field(r0, "link", "byteSize") != float64(9) {
		t.Errorf("link: %v", r0["link"])
	}
	if r0["sub"] != nil || r0["missing"] != nil || field(r0, "dir", "__typename") != "Tree" {
		t.Errorf("sub %v missing %v dir %v", r0["sub"], r0["missing"], r0["dir"])
	}
	if field(r0, "dir", "entries", 0, "mode") != float64(0o100644) {
		t.Errorf("entry mode: %v", field(r0, "dir", "entries", 0))
	}
	if data["r1"] != nil {
		t.Errorf("r1: %v", data["r1"])
	}
	errs := got["errors"].([]any)
	if len(errs) != 1 || field(errs, 0, "type") != "NOT_FOUND" || field(errs, 0, "path", 0) != "r1" ||
		field(errs, 0, "message") != "Could not resolve to a Repository with the name 'acme/nothing'." ||
		field(errs, 0, "locations", 0, "line") != float64(11) {
		t.Errorf("errors: %v", errs)
	}
	// Key order follows the selection.
	raw := w.call("POST", "/graphql", tok, map[string]any{"query": `{ b: repository(owner:"acme", name:"api") { name } a: rateLimit { cost } }`})
	if s := string(raw.body); strings.Index(s, `"b"`) > strings.Index(s, `"a"`) {
		t.Errorf("key order: %s", s)
	}
}

func TestGraphQLValidation(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token(nil, nil)
	for _, tc := range []struct {
		name, query, code, msg string
		path                   []any
	}{
		{"undefined field", `query Q { repository(owner: "acme", name: "api") { bogusField } }`, "undefinedField",
			"Field 'bogusField' doesn't exist on type 'Repository'", []any{"query Q", "repository", "bogusField"}},
		{"anonymous", `{ repository(owner: "acme", name: "api") { bogusField } }`, "undefinedField",
			"Field 'bogusField' doesn't exist on type 'Repository'", []any{"query", "repository", "bogusField"}},
		{"argument", `{ repository(owner: "acme", name: "api", color: "x") { id } }`, "argumentNotAccepted",
			"Field 'repository' doesn't accept argument 'color'", nil},
		{"missing argument", `{ repository(owner: "acme") { id } }`, "missingRequiredArguments",
			"Field 'repository' is missing required arguments: name", nil},
		{"no selection", `{ repository(owner: "acme", name: "api") { defaultBranchRef } }`, "selectionMismatch",
			"Field 'defaultBranchRef' returns Ref but has no selections. Did you mean 'defaultBranchRef { ... }'?", nil},
		{"enum", `{ repository(owner: "acme", name: "api") { pullRequests(first: 1, states: [BOGUS]) { totalCount } } }`,
			"argumentLiteralsIncompatible", "", nil},
		{"fragment type", `{ repository(owner: "acme", name: "api") { ... on Nope { id } } }`, "undefinedType",
			"No such type Nope, so it can't be a fragment condition", nil},
		{"undeclared variable", `query V { repository(owner: $o, name: "api") { id } }`, "variableNotDefined",
			"Variable $o is used by V but not declared", nil},
		{"unknown mutation", `mutation { createCommitOnBranch(input: {}) { clientMutationId } }`, "undefinedField",
			"Field 'createCommitOnBranch' doesn't exist on type 'Mutation'", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := w.graphql(tok, tc.query, nil)
			if _, hasData := got["data"]; hasData {
				t.Errorf("validation error with data: %v", got)
			}
			e := field(got, "errors", 0)
			if field(e, "extensions", "code") != tc.code || (tc.msg != "" && field(e, "message") != tc.msg) {
				t.Errorf("error: %v", e)
			}
			if tc.path != nil {
				b1, _ := json.Marshal(field(e, "path"))
				b2, _ := json.Marshal(tc.path)
				if string(b1) != string(b2) {
					t.Errorf("path %s, want %s", b1, b2)
				}
			}
		})
	}
	t.Run("syntax", func(t *testing.T) {
		got := w.graphql(tok, `{ repository(owner: "acme" { id } }`, nil)
		if msg, _ := field(got, "errors", 0, "message").(string); !strings.HasPrefix(msg, "Parse error on") {
			t.Errorf("syntax: %v", got)
		}
	})
	t.Run("variables", func(t *testing.T) {
		q := `query R($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { name } }`
		got := w.graphql(tok, q, map[string]any{"owner": "acme"})
		if msg := field(got, "errors", 0, "message"); msg != "Variable $name of type String! was provided invalid value" {
			t.Errorf("missing variable: %v", got)
		}
		got = w.graphql(tok, q, map[string]any{"owner": "acme", "name": "api"})
		if field(got, "data", "repository", "name") != "api" {
			t.Errorf("variables: %v", got)
		}
	})
	t.Run("anonymous", func(t *testing.T) {
		r := w.call("POST", "/graphql", "", map[string]any{"query": "{ rateLimit { cost } }"})
		wantStatus(t, "anonymous", r, 401)
	})
	t.Run("viewer", func(t *testing.T) {
		got := w.graphql(tok, "{ viewer { login } }", nil)
		if field(got, "errors", 0, "type") != "FORBIDDEN" {
			t.Errorf("viewer with an installation token: %v", got)
		}
		pat := must[string](t)(w.s.AddPAT("alice", PATSpec{Scopes: []string{"repo"}}))
		if got := w.graphql(pat, "{ viewer { login } }", nil); field(got, "data", "viewer", "login") != "alice" {
			t.Errorf("viewer: %v", got)
		}
	})
}

func TestGraphQLPullRequests(t *testing.T) {
	w := newWorld(t, Options{})
	fork := must[Repo](t)(w.s.Fork("acme/api", "bob"))
	w.branch("acme/api", "touchmark/hub")
	w.commit(fork.FullName, CommitSpec{Branch: "touchmark/hub", Author: "bob", Files: []File{{Path: "f", Content: []byte("f")}}})
	tok := w.token([]string{"api"}, nil)
	own := w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "own", "head": "touchmark/hub", "base": "main", "body": "b"}).obj(t)
	ownN := int64(own["number"].(float64))
	forkPR := w.openPR("acme/api", PRSpec{HeadRepo: "bob/api", Head: "touchmark/hub", Title: "fork", Author: "bob"})
	check(t, w.s.SetPRState("acme/api", ownN, "closed", "alice"))
	w.branch("acme/api", "done")
	merged := w.openPR("acme/api", PRSpec{Head: "done", Title: "done", Author: "alice"})
	must[string](t)(w.s.MergePR("acme/api", merged.Number, MergeCommit, "bob"))

	q := `query PRs($o: String!, $n: String!, $h: String!) {
  repository(owner: $o, name: $n) {
    pullRequests(headRefName: $h, states: [OPEN, CLOSED, MERGED], first: 10, orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes {
        number state merged isDraft headRefName headRefOid baseRefName body isCrossRepository
        headRepository { databaseId nameWithOwner }
        author { __typename login ... on Bot { databaseId } ... on User { databaseId } }
        timelineItems(itemTypes: [CLOSED_EVENT], last: 1) {
          nodes { __typename ... on ClosedEvent { stateReason actor { __typename login ... on User { databaseId } } } }
        }
      }
    }
  }
}`
	got := w.graphql(tok, q, map[string]any{"o": "acme", "n": "api", "h": "touchmark/hub"})
	conn := field(got, "data", "repository", "pullRequests")
	if field(conn, "totalCount") != float64(2) || field(conn, "pageInfo", "hasNextPage") != false {
		t.Fatalf("connection: %v", got)
	}
	first, second := field(conn, "nodes", 0), field(conn, "nodes", 1)
	if field(first, "number") != float64(forkPR.Number) || field(first, "isCrossRepository") != true ||
		field(first, "headRepository", "nameWithOwner") != "bob/api" || field(first, "state") != "OPEN" {
		t.Errorf("fork pull request: %v", first)
	}
	// Observed: a Bot's GraphQL login has no "[bot]".
	if field(second, "author", "__typename") != "Bot" || field(second, "author", "login") != "hub-writer" ||
		field(second, "author", "databaseId") != float64(w.app.Bot.ID) || field(second, "state") != "CLOSED" {
		t.Errorf("own pull request: %v", second)
	}
	closer := field(second, "timelineItems", "nodes", 0)
	if field(closer, "__typename") != "ClosedEvent" || field(closer, "actor", "login") != "alice" ||
		field(closer, "actor", "__typename") != "User" || field(closer, "stateReason") != "NOT_PLANNED" {
		t.Errorf("closer: %v", closer)
	}

	got = w.graphql(tok, `{ repository(owner: "acme", name: "api") { pullRequests(headRefName: "done", first: 5) {
  nodes { state merged mergedBy { login } timelineItems(first: 10, itemTypes: [MERGED_EVENT, CLOSED_EVENT]) { totalCount filteredCount nodes { __typename } } } } } }`, nil)
	m := field(got, "data", "repository", "pullRequests", "nodes", 0)
	if field(m, "state") != "MERGED" || field(m, "mergedBy", "login") != "bob" ||
		field(m, "timelineItems", "nodes", 0, "__typename") != "MergedEvent" ||
		field(m, "timelineItems", "nodes", 1, "__typename") != "ClosedEvent" {
		t.Errorf("merged: %v", m)
	}

	t.Run("pagination", func(t *testing.T) {
		got := w.graphql(tok, `{ repository(owner: "acme", name: "api") { pullRequests(states: [OPEN]) { totalCount } } }`, nil)
		if field(got, "errors", 0, "type") != "MISSING_PAGINATION_BOUNDARIES" || field(got, "data", "repository", "pullRequests") != nil {
			t.Errorf("no first: %v", got)
		}
		got = w.graphql(tok, `{ repository(owner: "acme", name: "api") { pullRequests(first: 101) { totalCount } } }`, nil)
		if field(got, "errors", 0, "type") != "EXCESSIVE_PAGINATION" {
			t.Errorf("first 101: %v", got)
		}
		page := `query P($after: String) { repository(owner: "acme", name: "api") {
  pullRequests(first: 2, after: $after) { pageInfo { hasNextPage endCursor } nodes { number } } } }`
		got = w.graphql(tok, page, nil)
		info := field(got, "data", "repository", "pullRequests", "pageInfo")
		if field(info, "hasNextPage") != true || field(got, "data", "repository", "pullRequests", "nodes", 1, "number") != float64(2) {
			t.Fatalf("page 1: %v", got)
		}
		got = w.graphql(tok, page, map[string]any{"after": field(info, "endCursor")})
		if field(got, "data", "repository", "pullRequests", "pageInfo", "hasNextPage") != false ||
			field(got, "data", "repository", "pullRequests", "nodes", 0, "number") != float64(3) {
			t.Errorf("page 2: %v", got)
		}
	})
	w.noViolations()
}

func TestGraphQLLimitsAndFaults(t *testing.T) {
	w := newWorld(t, Options{Limits: &Limits{GraphQLPointsPerHour: 2, GraphQLPointsPerMinute: 6}})
	tok := w.token(nil, nil)
	q := `{ rateLimit { cost limit remaining used } }`
	got := w.graphql(tok, q, nil)
	if field(got, "data", "rateLimit", "limit") != float64(2) || field(got, "data", "rateLimit", "used") != float64(1) {
		t.Errorf("rateLimit: %v", got)
	}
	w.graphql(tok, q, nil)
	r := w.call("POST", "/graphql", tok, map[string]any{"query": q})
	wantStatus(t, "primary", r, 200)
	if field(r.obj(t), "errors", 0, "type") != "RATE_LIMITED" || r.header.Get("X-Ratelimit-Remaining") != "0" ||
		r.header.Get("X-Ratelimit-Resource") != "graphql" {
		t.Errorf("primary: %v %s", r.header, r.body)
	}
	w.clock.Advance(time.Hour)
	tok = w.token(nil, nil)
	mut := `mutation { updateRefs(input: {repositoryId: "x", refUpdates: []}) { clientMutationId } }`
	w.graphql(tok, mut, nil)
	r = w.call("POST", "/graphql", tok, map[string]any{"query": mut})
	wantStatus(t, "secondary after two mutations", r, 403)
	if r.header.Get("Retry-After") == "" {
		t.Errorf("no Retry-After: %v", r.header)
	}

	f := newWorld(t, Options{})
	tok = f.token(nil, nil)
	f.s.Fail("GRAPHQL repository", Fault{GraphQL: "INTERNAL"})
	got = f.graphql(tok, `{ repository(owner: "acme", name: "api") { id } }`, nil)
	if field(got, "errors", 0, "type") != "INTERNAL" || got["data"] != nil {
		t.Errorf("fault: %v", got)
	}
	f.s.Fail("POST /graphql", Fault{Status: 502})
	wantStatus(t, "502", f.call("POST", "/graphql", tok, map[string]any{"query": q}), 502)
	if got := f.graphql(tok, q, nil); got["data"] == nil {
		t.Errorf("after faults: %v", got)
	}
	reqs := f.s.Requests()
	if last := reqs[len(reqs)-1]; last.Route != "POST /graphql rateLimit" {
		t.Errorf("route %q", last.Route)
	}
}

func TestGraphQLNodes(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token(nil, nil)
	got := w.graphql(tok, `query N($ids: [ID!]!) { nodes(ids: $ids) { __typename ... on Repository { name } ... on Bot { login } } }`,
		map[string]any{"ids": []string{w.api.NodeID, w.app.Bot.NodeID, "bm9wZQ=="}})
	if field(got, "data", "nodes", 0, "name") != "api" || field(got, "data", "nodes", 1, "login") != "hub-writer" ||
		field(got, "data", "nodes", 2) != nil || field(got, "errors", 0, "path", 1) != float64(2) {
		t.Errorf("nodes: %v", got)
	}
	got = w.graphql(tok, `{ user(login: "hub-writer[bot]") { id } organization(login: "acme") { login databaseId } }`, nil)
	if field(got, "data", "user") != nil || field(got, "data", "organization", "login") != "acme" {
		t.Errorf("user and organization: %v", got)
	}
}
