package gitlab

import (
	"net/http"
	"slices"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// protectedLevel is a push access level of a protected branch: a role.
func protectedLevel(n int) map[string]any {
	return map[string]any{"id": n, "access_level": n, "access_level_description": "x", "deploy_key_id": nil, "user_id": nil, "group_id": nil}
}

// TestVerdictOf: GitLab applies the most permissive of the protected
// branches that match a branch, to pushes and to force pushes.
func TestVerdictOf(t *testing.T) {
	const selfID, dev = 7, 30
	role := func(n int) apiProtectedLevel { return apiProtectedLevel{AccessLevel: n} }
	userID, groupID := int64(selfID), int64(99)
	exact := apiProtectedBranch{Name: "touchmark/acme-eng", Push: []apiProtectedLevel{role(30)}}
	maintainers := apiProtectedBranch{Name: "touchmark/*", Push: []apiProtectedLevel{role(40)}, AllowForcePush: true}
	noOne := apiProtectedBranch{Name: "*", Push: []apiProtectedLevel{role(0)}}
	group := apiProtectedBranch{Name: "touchmark/*", Push: []apiProtectedLevel{{GroupID: &groupID}}}
	user := apiProtectedBranch{Name: "touchmark/*", Push: []apiProtectedLevel{{UserID: &userID}}}
	for _, tc := range []struct {
		name  string
		rules []apiProtectedBranch
		push  platform.FindingStatus
		force bool
		n     int
	}{
		{"none", nil, platform.FindingOK, false, 0},
		{"another branch", []apiProtectedBranch{{Name: "main", Push: []apiProtectedLevel{role(0)}}}, platform.FindingOK, false, 0},
		{"Maintainers only", []apiProtectedBranch{maintainers}, platform.FindingFail, true, 1},
		{"Developers, no force", []apiProtectedBranch{exact}, platform.FindingOK, false, 1},
		{"exact allows, wildcard denies", []apiProtectedBranch{exact, maintainers}, platform.FindingOK, true, 2},
		{"wildcard allows, no one on another", []apiProtectedBranch{noOne, exact}, platform.FindingOK, false, 2},
		{"both deny", []apiProtectedBranch{noOne, maintainers}, platform.FindingFail, true, 2},
		{"a group", []apiProtectedBranch{group}, platform.FindingUnknown, false, 1},
		{"a group and a denial", []apiProtectedBranch{noOne, group}, platform.FindingUnknown, false, 2},
		{"the writer by id", []apiProtectedBranch{noOne, user}, platform.FindingOK, false, 2},
	} {
		v := verdictOf(tc.rules, "touchmark/acme-eng", selfID, dev)
		if v.push != tc.push || v.force != tc.force || len(v.rules) != tc.n {
			t.Errorf("%s: %+v, want push %s, force %v, %d rules", tc.name, v, tc.push, tc.force, tc.n)
		}
	}
}

func TestNoPush(t *testing.T) {
	newRepo := func(fx *fixture) platform.Repo {
		return platform.Repo{Host: fx.provider().Host, ID: "11", Path: "acme/api", DefaultBranch: "main"}
	}
	setup := func(t *testing.T, rules []any) *fixture {
		fx := newFixture(t)
		fx.json(http.MethodGet, "/user", http.StatusOK, self(7, "service_account_writer", true))
		fx.json(http.MethodGet, "/projects/11", http.StatusOK, project(11, "acme/api",
			with("permissions", map[string]any{"project_access": nil, "group_access": map[string]any{"access_level": 30, "notification_level": 3}})))
		fx.pages("/projects/11/protected_branches", rules)
		return fx
	}
	branches := []string{"main", "touchmark/acme-eng", "", "touchmark/old", "touchmark/acme-eng"}

	t.Run("blocked", func(t *testing.T) {
		fx := setup(t, []any{
			map[string]any{"id": 1, "name": "main", "allow_force_push": false, "push_access_levels": []any{protectedLevel(0)}},
			map[string]any{"id": 2, "name": "touchmark/*", "allow_force_push": false, "push_access_levels": []any{protectedLevel(40)}},
			map[string]any{"id": 3, "name": "touchmark/old", "allow_force_push": false, "push_access_levels": []any{protectedLevel(30)}},
		})
		got, err := fx.writer.NoPush(t.Context(), newRepo(fx), branches)
		if err != nil {
			t.Fatal(err)
		}
		want := []platform.Protected{{Branch: "touchmark/acme-eng", Rule: "touchmark/*"}}
		if !slices.Equal(got, want) {
			t.Errorf("NoPush = %+v, want %+v", got, want)
		}
		if w := fx.writes(); len(w) > 0 {
			t.Errorf("NoPush wrote: %+v", w)
		}
	})
	t.Run("no rule matches", func(t *testing.T) {
		fx := setup(t, []any{
			map[string]any{"id": 1, "name": "main", "allow_force_push": false, "push_access_levels": []any{protectedLevel(0)}},
		})
		got, err := fx.writer.NoPush(t.Context(), newRepo(fx), branches)
		if err != nil || len(got) > 0 {
			t.Errorf("NoPush = %+v, %v", got, err)
		}
		// One request: the project and the account are not read.
		if n := len(fx.requests("", "")); n != 1 {
			t.Errorf("%d requests, want 1: %+v", n, fx.requests("", ""))
		}
	})
	t.Run("only the default branch", func(t *testing.T) {
		fx := setup(t, nil)
		got, err := fx.writer.NoPush(t.Context(), newRepo(fx), []string{"main", ""})
		if err != nil || len(got) > 0 || len(fx.requests("", "")) > 0 {
			t.Errorf("NoPush = %+v, %v, requests %+v", got, err, fx.requests("", ""))
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		fx := setup(t, nil)
		fx.json(http.MethodGet, "/projects/11/protected_branches", http.StatusForbidden, msg("403 Forbidden"))
		got, err := fx.writer.NoPush(t.Context(), newRepo(fx), branches)
		if err != nil || len(got) > 0 {
			t.Errorf("a 403 listing: %+v, %v", got, err)
		}
	})
	t.Run("a refused credential", func(t *testing.T) {
		fx := setup(t, nil)
		fx.json(http.MethodGet, "/projects/11/protected_branches", http.StatusUnauthorized, msg("401 Unauthorized"))
		if _, err := fx.writer.NoPush(t.Context(), newRepo(fx), branches); err == nil || platform.ClassOf(err) != platform.ClassAuth {
			t.Errorf("a refused credential: %v", err)
		}
	})
}
