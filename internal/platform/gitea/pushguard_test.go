package gitea

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestNoPush(t *testing.T) {
	fx := newFixture(t, "gitea")
	branch := func(name string, status int, protected, canPush bool) {
		if status != http.StatusOK {
			fx.json(http.MethodGet, "/repos/acme/api/branches/"+name, status, fx.apiMsg("no"))
			return
		}
		fx.json(http.MethodGet, "/repos/acme/api/branches/"+name, http.StatusOK, map[string]any{
			"name": name, "commit": map[string]any{"id": strings.Repeat("a", 40)}, "protected": protected,
			"required_approvals": 0, "enable_status_check": false, "status_check_contexts": []string{},
			"user_can_push": canPush, "user_can_merge": canPush, "effective_branch_protection_name": "touchmark/*",
		})
	}
	branch("touchmark/acme-eng", http.StatusOK, true, false)
	branch("touchmark/pushable", http.StatusOK, true, true)
	branch("touchmark/free", http.StatusOK, false, true)
	branch("touchmark/new", http.StatusNotFound, false, false)
	branch("touchmark/broken", http.StatusInternalServerError, false, false)
	repo := platform.Repo{Host: fx.provider("gitea").Host, ID: "7", Path: "acme/api", DefaultBranch: "main"}

	got, err := fx.writer.NoPush(t.Context(), repo, []string{"main", "touchmark/acme-eng", "touchmark/pushable", "touchmark/free",
		"touchmark/new", "touchmark/broken", "", "touchmark/acme-eng"})
	if err != nil {
		t.Fatal(err)
	}
	want := []platform.Protected{{Branch: "touchmark/acme-eng", Rule: "touchmark/*"}}
	if !slices.Equal(got, want) {
		t.Errorf("NoPush = %+v, want %+v", got, want)
	}
	if n := len(fx.requests(http.MethodGet, "/repos/acme/api/branches/main")); n != 0 {
		t.Errorf("NoPush read the default branch %d times", n)
	}
	if n := len(fx.requests(http.MethodGet, "/repos/acme/api/branches/touchmark/acme-eng")); n != 1 {
		t.Errorf("NoPush read touchmark/acme-eng %d times, want 1", n)
	}

	// A refused credential ends the call.
	branch("touchmark/acme-eng", http.StatusUnauthorized, false, false)
	if _, err := fx.writer.NoPush(t.Context(), repo, []string{"touchmark/acme-eng"}); platform.ClassOf(err) != platform.ClassAuth {
		t.Errorf("NoPush with a refused credential: %v", err)
	}
	for _, c := range fx.requests("", "") {
		if c.Method != http.MethodGet {
			t.Errorf("NoPush wrote: %s %s", c.Method, c.Path)
		}
	}
}
