package hubch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

const maintainerToken = "maintainer-token-0123456789"

// keyServer answers the routes of routes (by path) and records the paths
// and credential headers it saw; an unknown path is 404.
type keyServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	auth  []string
}

func newKeyServer(t *testing.T, header, value string, routes map[string]string) *keyServer {
	t.Helper()
	s := &keyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.auth = append(s.auth, r.Header.Get(header))
		s.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(header) != value {
			http.Error(rw, `{"message": "401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, ok := routes[r.URL.Path]
		switch {
		case !ok:
			http.Error(rw, `{"message": "404 Not Found"}`, http.StatusNotFound)
		case strings.HasPrefix(body, "status "):
			var code int
			if _, err := fmt.Sscanf(body, "status %d", &code); err != nil {
				t.Errorf("route %s: %v", r.URL.Path, err)
			}
			http.Error(rw, `{"message": "refused"}`, code)
		default:
			rw.Header().Set("Content-Type", "application/json")
			fmt.Fprint(rw, body)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func secretNames(ks KeyStore) []string {
	var out []string
	for _, s := range ks.Secrets {
		where := s.Where
		switch {
		case s.Environment != "":
			where += ":" + s.Environment
		case s.Group != "":
			where += ":" + s.Group
		}
		if ks.Platform == "gitlab" {
			where += fmt.Sprintf(" protected=%v masked=%v scope=%s", s.Protected, s.Masked, s.Scope)
		}
		out = append(out, s.Name+" "+where)
	}
	slices.Sort(out)
	return out
}

func TestReadKeyStoreGitHub(t *testing.T) {
	const repo = "/api/v3/repos/acme/engineering-assets"
	s := newKeyServer(t, "Authorization", "Bearer "+maintainerToken, map[string]string{
		"/api/v3/repositories/712345678":       `{"full_name": "acme/engineering-assets", "default_branch": "main", "visibility": "internal", "private": true, "owner": {"login": "acme", "type": "Organization"}}`,
		repo + "/actions/secrets":              `{"total_count": 1, "secrets": [{"name": "TOUCHMARK_WRITE_APP_KEY", "created_at": "2026-09-01T00:00:00Z"}]}`,
		repo + "/actions/organization-secrets": "status 403",
		repo + "/dependabot/secrets":           `{"total_count": 0, "secrets": []}`,
		repo + "/environments":                 `{"total_count": 2, "environments": [{"name": "touchmark-distribute"}, {"name": "pages"}]}`,
		repo + "/environments/touchmark-distribute": `{"name": "touchmark-distribute",
			"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}`,
		repo + "/environments/touchmark-distribute/deployment-branch-policies": `{"total_count": 1, "branch_policies": [{"name": "main", "type": "branch"}]}`,
		repo + "/environments/touchmark-distribute/secrets":                    `{"total_count": 1, "secrets": [{"name": "TOUCHMARK_WRITE_APP_KEY"}]}`,
		repo + "/environments/pages":                                           `{"name": "pages", "deployment_branch_policy": null}`,
		repo + "/environments/pages/secrets":                                   `{"total_count": 0, "secrets": []}`,
	})
	ks, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "github", APIURL: s.URL + "/api/v3", RepoID: "712345678", Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	if ks.RepoPath != "acme/engineering-assets" || ks.DefaultBranch != "main" || ks.Visibility != "internal" {
		t.Errorf("store %+v", ks)
	}
	want := []string{"TOUCHMARK_WRITE_APP_KEY environment:touchmark-distribute", "TOUCHMARK_WRITE_APP_KEY repository"}
	if got := secretNames(ks); !slices.Equal(got, want) {
		t.Errorf("secrets %q, want %q", got, want)
	}
	if len(ks.Environments) != 2 || len(ks.Environments[0].Policies) != 1 || !ks.Environments[1].AllRefs {
		t.Errorf("environments %+v", ks.Environments)
	}
	if len(ks.Unread) != 1 || !strings.Contains(ks.Unread[0], "organization secrets shared with the repository: HTTP 403") {
		t.Errorf("unread %q", ks.Unread)
	}
	// A refused token ends the reading.
	_, err = ReadKeyStore(t.Context(), KeyStoreInput{Platform: "github", APIURL: s.URL + "/api/v3", RepoID: "712345678", Token: "wrong-token-0123456789"})
	if err == nil || strings.Contains(err.Error(), "wrong-token") {
		t.Errorf("a refused token: %v", err)
	}
}

func TestReadKeyStoreGitLab(t *testing.T) {
	s := newKeyServer(t, "Private-Token", maintainerToken, map[string]string{
		"/api/v4/projects/1234": `{"path_with_namespace": "acme/platform/engineering-assets", "default_branch": "main", "visibility": "private",
			"namespace": {"kind": "group", "full_path": "acme/platform"}, "ci_pipeline_variables_minimum_override_role": "no_one_allowed"}`,
		"/api/v4/projects/1234/variables": `[{"key": "TOUCHMARK_CORP_WRITE_TOKEN", "value": "never-kept-value", "protected": true, "masked": true,
			"hidden": true, "environment_scope": "touchmark-distribute", "variable_type": "env_var"}]`,
		"/api/v4/groups/acme/variables":          `[{"key": "TOUCHMARK_WRITE_TOKEN", "value": "never-kept-value", "protected": false, "masked": false, "environment_scope": "*"}]`,
		"/api/v4/groups/acme/platform/variables": "status 403",
		"/api/v4/projects/1234/protected_branches": `[{"id": 1, "name": "main", "push_access_levels": [{"access_level": 0}],
			"merge_access_levels": [{"access_level": 40}], "allow_force_push": false},
			{"id": 2, "name": "release/*", "push_access_levels": [{"access_level": 30}, {"access_level": 30, "user_id": 7}], "merge_access_levels": []}]`,
		"/api/v4/projects/1234/protected_tags": `[{"name": "v*", "create_access_levels": [{"access_level": 40}]}]`,
	})
	ks, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "gitlab", APIURL: s.URL + "/api/v4", RepoID: "1234", Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"TOUCHMARK_CORP_WRITE_TOKEN project protected=true masked=true scope=touchmark-distribute",
		"TOUCHMARK_WRITE_TOKEN group:acme protected=false masked=false scope=*",
	}
	if got := secretNames(ks); !slices.Equal(got, want) {
		t.Errorf("secrets %q, want %q", got, want)
	}
	if ks.PipelineVariables != "no_one_allowed" || len(ks.ProtectedBranches) != 2 || !ks.ProtectedBranches[1].Others ||
		!slices.Equal(ks.ProtectedBranches[0].Levels, []int{0, 40}) || len(ks.ProtectedTags) != 1 {
		t.Errorf("store %+v", ks)
	}
	if len(ks.Unread) != 1 || !strings.Contains(ks.Unread[0], "group acme/platform: HTTP 403") {
		t.Errorf("unread %q", ks.Unread)
	}
	if strings.Contains(fmt.Sprintf("%+v", ks), "never-kept-value") {
		t.Error("a variable's value was kept")
	}
	if !slices.Contains(s.paths, "/api/v4/groups/acme/platform/variables") {
		t.Errorf("paths %q", s.paths)
	}
}

func TestReadKeyStoreGitea(t *testing.T) {
	s := newKeyServer(t, "Authorization", "token "+maintainerToken, map[string]string{
		"/api/v1/repositories/42":                               `{"full_name": "acme/engineering-assets", "default_branch": "main", "private": false, "owner": {"login": "acme"}}`,
		"/api/v1/repos/acme/engineering-assets/actions/secrets": `[{"name": "TOUCHMARK_WRITE_TOKEN", "created": "2026-09-01T00:00:00Z"}]`,
	})
	ks, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "gitea", APIURL: s.URL + "/api/v1", RepoID: "42", Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	if got := secretNames(ks); !slices.Equal(got, []string{"TOUCHMARK_WRITE_TOKEN repository"}) {
		t.Errorf("secrets %q", got)
	}
	if len(ks.Unread) != 1 || !strings.Contains(ks.Unread[0], "organization's Actions secrets: HTTP 404") {
		t.Errorf("unread %q", ks.Unread)
	}
}

func TestReadKeyStoreRefuses(t *testing.T) {
	for name, in := range map[string]KeyStoreInput{
		"http":     {Platform: "github", APIURL: "http://github.example.com/api/v3", RepoID: "1", Token: maintainerToken},
		"userinfo": {Platform: "github", APIURL: "https://user:pw@github.example.com/api/v3", RepoID: "1", Token: maintainerToken},
		"no id":    {Platform: "github", APIURL: "https://github.example.com/api/v3", Token: maintainerToken},
		"token":    {Platform: "github", APIURL: "https://github.example.com/api/v3", RepoID: "1", Token: "has space"},
		"platform": {Platform: "bitbucket", APIURL: "https://bitbucket.example.com", RepoID: "1", Token: maintainerToken},
	} {
		if _, err := ReadKeyStore(t.Context(), in); err == nil || strings.Contains(err.Error(), maintainerToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Listings the token may not read are noted where their checks look: the
// protected branches and tags of GitLab, the environments of GitHub.
func TestReadKeyStoreUnreadListings(t *testing.T) {
	gl := newKeyServer(t, "Private-Token", maintainerToken, map[string]string{
		"/api/v4/projects/1234": `{"path_with_namespace": "acme/engineering-assets", "default_branch": "main", "visibility": "private",
			"namespace": {"kind": "user", "full_path": "acme"}, "ci_pipeline_variables_minimum_override_role": "no_one_allowed"}`,
		"/api/v4/projects/1234/variables":          `[]`,
		"/api/v4/projects/1234/protected_branches": "status 403",
		"/api/v4/projects/1234/protected_tags":     "status 403",
	})
	ks, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "gitlab", APIURL: gl.URL + "/api/v4", RepoID: "1234", Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ks.ProtectedBranchesUnread, "HTTP 403") || !strings.Contains(ks.ProtectedTagsUnread, "HTTP 403") {
		t.Errorf("unread: branches %q, tags %q", ks.ProtectedBranchesUnread, ks.ProtectedTagsUnread)
	}
	const repo = "/api/v3/repos/acme/engineering-assets"
	gh := newKeyServer(t, "Authorization", "Bearer "+maintainerToken, map[string]string{
		"/api/v3/repositories/712345678":       `{"full_name": "acme/engineering-assets", "default_branch": "main", "visibility": "private", "private": true}`,
		repo + "/actions/secrets":              `{"total_count": 0, "secrets": []}`,
		repo + "/actions/organization-secrets": `{"total_count": 0, "secrets": []}`,
		repo + "/dependabot/secrets":           `{"total_count": 0, "secrets": []}`,
		repo + "/environments":                 "status 403",
	})
	ks, err = ReadKeyStore(t.Context(), KeyStoreInput{Platform: "github", APIURL: gh.URL + "/api/v3", RepoID: "712345678", Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ks.EnvironmentsUnread, "HTTP 403") {
		t.Errorf("environments unread %q", ks.EnvironmentsUnread)
	}
}
