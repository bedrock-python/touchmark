package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
)

// TestGraphQLContracts validates every fixed GraphQL document the driver
// sends against the schemas of github.com and GHES 3.19, as the fake
// holds them (ghfake's subset, which TestSchemaSubset checks field for
// field against GitHub's public schemas).
func TestGraphQLContracts(t *testing.T) {
	docs := map[string]string{
		"queryOpenPRs":       queryOpenPRs,
		"queryClosers":       queryClosers,
		"querySweep":         querySweep,
		"mutationUpdateRefs": mutationUpdateRefs,
		"batchFilesQuery(1)": batchFilesQuery(1),
		"batchFilesQuery(3)": batchFilesQuery(3),
	}
	for _, f := range []ghfake.Flavor{ghfake.DotCom, ghfake.GHES} {
		for name, doc := range docs {
			if errs := ghfake.Validate(f, doc); len(errs) > 0 {
				t.Errorf("%s on %s: %q", name, f, errs)
			}
		}
	}
	if errs := ghfake.Validate(ghfake.DotCom, `query { repository(owner: "o", name: "n") { noSuchField } }`); len(errs) == 0 {
		t.Error("the validator accepts a field the schema lacks")
	}
}

// restRoute is one REST route the driver calls: the method, the path
// template of github/rest-api-description, and the query parameters the
// driver may send. dotcomOnly routes are absent from GHES 3.19 (the driver
// handles their 404).
type restRoute struct {
	method, path string
	query        []string
	dotcomOnly   bool
}

// restRoutes are every REST route of the driver:
// the unit tests' server refuses a request outside them
// (checkRESTContract), and TestRESTContracts checks them against GitHub's
// REST descriptions.
var restRoutes = []restRoute{
	{method: "GET", path: "/app"},
	{method: "GET", path: "/app/installations", query: []string{"per_page", "page"}},
	{method: "POST", path: "/app/installations/{installation_id}/access_tokens"},
	{method: "GET", path: "/orgs/{org}/installation"},
	{method: "GET", path: "/users/{username}/installation"},
	{method: "GET", path: "/repos/{owner}/{repo}/installation"},
	{method: "DELETE", path: "/installation/token"},
	{method: "GET", path: "/installation/repositories", query: []string{"per_page", "page"}},
	{method: "GET", path: "/meta"},
	{method: "GET", path: "/user"},
	{method: "GET", path: "/users/{username}"},
	{method: "GET", path: "/user/repos", query: []string{"affiliation", "per_page", "page"}},
	{method: "GET", path: "/users/{username}/repos", query: []string{"type", "per_page", "page"}},
	{method: "GET", path: "/orgs/{org}/repos", query: []string{"type", "per_page", "page"}},
	{method: "GET", path: "/repos/{owner}/{repo}"},
	{method: "GET", path: "/repos/{owner}/{repo}/hash-algorithm", dotcomOnly: true},
	{method: "GET", path: "/repos/{owner}/{repo}/branches/{branch}"},
	{method: "GET", path: "/repos/{owner}/{repo}/git/trees/{tree_sha}"},
	{method: "GET", path: "/repos/{owner}/{repo}/git/blobs/{file_sha}"},
	{method: "POST", path: "/repos/{owner}/{repo}/git/commits"},
	{method: "GET", path: "/repos/{owner}/{repo}/git/ref/{ref}"},
	{method: "DELETE", path: "/repos/{owner}/{repo}/git/refs/{ref}"},
	{method: "GET", path: "/repos/{owner}/{repo}/labels/{name}"},
	{method: "POST", path: "/repos/{owner}/{repo}/labels"},
	{method: "POST", path: "/repos/{owner}/{repo}/issues/{issue_number}/labels"},
	{method: "POST", path: "/repos/{owner}/{repo}/issues/{issue_number}/comments"},
	{method: "GET", path: "/repos/{owner}/{repo}/pulls", query: []string{"state", "head", "base", "sort", "direction", "per_page", "page"}},
	{method: "POST", path: "/repos/{owner}/{repo}/pulls"},
	{method: "GET", path: "/repos/{owner}/{repo}/pulls/{pull_number}"},
	{method: "PATCH", path: "/repos/{owner}/{repo}/pulls/{pull_number}"},
	{method: "GET", path: "/repos/{owner}/{repo}/rules/branches/{branch}", query: []string{"per_page", "page"}},
	{method: "GET", path: "/repos/{owner}/{repo}/rulesets/{ruleset_id}"},
}

// checkRESTContract returns an error when a request of the driver (method,
// the API-relative path with its segments unescaped, the query) is not one
// of restRoutes. A path under /repositories/{id} (where GitHub redirects
// the old path of a renamed repository) counts as the same path under
// /repos/{owner}/{repo}.
func checkRESTContract(method, path string, query url.Values) error {
	if rest, ok := strings.CutPrefix(path, "/repositories/"); ok {
		_, tail, _ := strings.Cut(rest, "/")
		path = "/repos/o/r"
		if tail != "" {
			path += "/" + tail
		}
	}
	for _, r := range restRoutes {
		if r.method != method || !matchTemplate(r.path, path) {
			continue
		}
		for k := range query {
			if !slices.Contains(r.query, k) {
				return fmt.Errorf("REST contract: %s %s sends the query parameter %q, which restRoutes does not declare for %s", method, path, k, r.path)
			}
		}
		return nil
	}
	return fmt.Errorf("REST contract: %s %s is not in restRoutes (contracts_test.go)", method, path)
}

// greedyParams are the path parameters that may hold '/' (a ref, a branch,
// a label name): as the last segment of a template, they take the rest of
// the path.
var greedyParams = []string{"{ref}", "{branch}", "{name}"}

// matchTemplate matches a path against a template whose {param} segments
// match one segment each, but a greedy last one (greedyParams).
func matchTemplate(template, path string) bool {
	ts := strings.Split(strings.Trim(template, "/"), "/")
	ps := strings.Split(strings.Trim(path, "/"), "/")
	for i, t := range ts {
		if i >= len(ps) || ps[i] == "" {
			return false
		}
		if strings.HasPrefix(t, "{") {
			if i == len(ts)-1 && slices.Contains(greedyParams, t) {
				return true
			}
			continue
		}
		if t != ps[i] {
			return false
		}
	}
	return len(ts) == len(ps)
}

// TestRESTContracts checks restRoutes against GitHub's REST descriptions
// (github/rest-api-description) when GHFAKE_REST_DESCRIPTION names a
// downloaded api.github.com.json and GHFAKE_REST_DESCRIPTION_GHES a
// ghes-3.19.json: every route exists with its method, and every query
// parameter the driver may send is declared there. Without them it checks
// the matcher only.
func TestRESTContracts(t *testing.T) {
	for _, tc := range []struct {
		template, path string
		want           bool
	}{
		{"/repos/{owner}/{repo}/git/ref/{ref}", "/repos/acme/api/git/ref/heads/touchmark/hub", true},
		{"/repos/{owner}/{repo}", "/repos/acme/api", true},
		{"/repos/{owner}/{repo}", "/repos/acme/api/pulls", false},
		{"/repos/{owner}/{repo}/pulls", "/repos/acme/api", false},
		{"/users/{username}", "/users/touchmark-write[bot]", true},
	} {
		if got := matchTemplate(tc.template, tc.path); got != tc.want {
			t.Errorf("matchTemplate(%s, %s) = %v", tc.template, tc.path, got)
		}
	}
	if err := checkRESTContract("GET", "/repositories/104/installation", nil); err != nil {
		t.Error(err)
	}
	if err := checkRESTContract("GET", "/repos/acme/api/pulls", url.Values{"q": {"x"}}); err == nil {
		t.Error("an undeclared query parameter passes")
	}
	for _, tc := range []struct {
		name, env string
		ghes      bool
	}{{"api.github.com", "GHFAKE_REST_DESCRIPTION", false}, {"ghes-3.19", "GHFAKE_REST_DESCRIPTION_GHES", true}} {
		t.Run(tc.name, func(t *testing.T) {
			file := os.Getenv(tc.env)
			if file == "" {
				t.Skip(tc.env + " is not set")
			}
			ops, err := restDescription(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range restRoutes {
				if tc.ghes && r.dotcomOnly {
					if _, ok := ops[r.method+" "+r.path]; ok {
						t.Errorf("%s %s exists on %s now: the driver may stop treating its 404 as SHA-1", r.method, r.path, tc.name)
					}
					continue
				}
				params, ok := ops[r.method+" "+r.path]
				if !ok {
					t.Errorf("%s %s is not in %s", r.method, r.path, tc.name)
					continue
				}
				for _, q := range r.query {
					if !slices.Contains(params, q) {
						t.Errorf("%s %s: the query parameter %q is not declared in %s (it has %q)", r.method, r.path, q, tc.name, params)
					}
				}
			}
		})
	}
}

// restDescription reads an OpenAPI description and returns, for each
// "METHOD /path", its query parameters.
func restDescription(file string) (map[string][]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	type param struct {
		Ref  string `json:"$ref"`
		Name string `json:"name"`
		In   string `json:"in"`
	}
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Parameters map[string]param `json:"parameters"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	resolve := func(p param) param {
		if p.Ref != "" {
			return doc.Components.Parameters[p.Ref[strings.LastIndexByte(p.Ref, '/')+1:]]
		}
		return p
	}
	out := map[string][]string{}
	for path, methods := range doc.Paths {
		var shared []param
		if raw, ok := methods["parameters"]; ok {
			_ = json.Unmarshal(raw, &shared)
		}
		for method, raw := range methods {
			if method == "parameters" {
				continue
			}
			var op struct {
				Parameters []param `json:"parameters"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, fmt.Errorf("%s: %s %s: %w", file, method, path, err)
			}
			var query []string
			for _, p := range append(slices.Clone(shared), op.Parameters...) {
				if p = resolve(p); p.In == "query" {
					query = append(query, p.Name)
				}
			}
			out[strings.ToUpper(method)+" "+path] = query
		}
	}
	return out, nil
}
