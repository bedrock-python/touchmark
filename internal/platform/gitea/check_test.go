package gitea

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// findingOf returns the finding of check for repo ("" for the identity).
func findingOf(t *testing.T, fs []platform.Finding, repo, check string) platform.Finding {
	t.Helper()
	for _, f := range fs {
		if f.Repo == repo && f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding %s of %q in %+v", check, repo, fs)
	return platform.Finding{}
}

func wantFinding(t *testing.T, f platform.Finding, status platform.FindingStatus, detail string) {
	t.Helper()
	if f.Status != status || !strings.Contains(f.Detail, detail) {
		t.Errorf("%s %s: %s %q, want %s with %q", f.Repo, f.Check, f.Status, f.Detail, status, detail)
	}
}

func TestCheckIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		scopes []string
		want   platform.FindingStatus
		detail string
	}{
		{"writer's scopes", http.StatusOK, []string{"write:repository", "write:issue", "read:organization", "read:user"}, platform.FindingOK, "write:repository"},
		{"missing issue", http.StatusOK, []string{"write:repository", "read:organization", "read:user"}, platform.FindingFail, "lacks write:issue"},
		{"all", http.StatusOK, []string{"all"}, platform.FindingWarn, "all, more than the writer needs"},
		{"admin", http.StatusOK, []string{"write:repository", "write:issue", "write:admin", "read:user", "read:organization"}, platform.FindingWarn, "write:admin"},
		{"no read", http.StatusOK, []string{"write:repository", "write:issue"}, platform.FindingWarn, "lacks read:organization, read:user"},
		{"old server", http.StatusNotFound, nil, platform.FindingUnknown, "Gitea 1.27"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, "gitea")
			fx.json(http.MethodGet, "/user", http.StatusOK, fx.user(5, "touchmark-writer"))
			if tc.status == http.StatusOK {
				fx.json(http.MethodGet, "/token", http.StatusOK, map[string]any{"id": 3, "name": "writer", "scopes": tc.scopes, "token_last_eight": "0badc0de"})
			} else {
				fx.json(http.MethodGet, "/token", tc.status, fx.apiMsg("not found"))
			}
			fs, err := fx.writer.Check(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantFinding(t, findingOf(t, fs, "", "scopes"), tc.want, tc.detail)
			wantFinding(t, findingOf(t, fs, "", "token-expiry"), platform.FindingOK, "no expiry date")
			wantFinding(t, findingOf(t, fs, "", "2fa"), platform.FindingUnknown, "do not show")
			if calls := fx.requests(http.MethodGet, "/token"); len(calls) != 1 || calls[0].Auth != "token "+fx.token {
				t.Errorf("GET /token: %+v", calls)
			}
		})
	}
}

func TestCheckRepo(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/user", http.StatusOK, fx.user(5, "touchmark-writer"))
	perms := func(admin, push bool) repoOpt {
		return withField("permissions", map[string]any{"admin": admin, "push": push, "pull": true})
	}
	fx.json(http.MethodGet, "/repositories/7", http.StatusOK, fx.repo(7, "acme/api", perms(false, true)))
	fx.json(http.MethodGet, "/repositories/8", http.StatusOK, fx.repo(8, "acme/ro", perms(false, false)))
	fx.json(http.MethodGet, "/repositories/9", http.StatusOK, fx.repo(9, "acme/own", perms(true, true)))
	fx.json(http.MethodGet, "/repositories/10", http.StatusNotFound, fx.apiMsg("not found"))
	branch := func(repo, name string, protected, canPush bool) {
		fx.json(http.MethodGet, "/repos/"+repo+"/branches/"+name, http.StatusOK, map[string]any{
			"name": name, "commit": map[string]any{"id": strings.Repeat("a", 40)}, "protected": protected,
			"required_approvals": 0, "enable_status_check": false, "status_check_contexts": []string{},
			"user_can_push": canPush, "user_can_merge": canPush, "effective_branch_protection_name": "touchmark/*",
		})
	}
	missing := func(repo, name string) {
		fx.json(http.MethodGet, "/repos/"+repo+"/branches/"+name, http.StatusNotFound, fx.apiMsg("branch does not exist"))
	}
	branch("acme/api", "touchmark/acme-eng", true, true)
	missing("acme/api", "chore/sync")
	missing("acme/ro", "touchmark/acme-eng")
	missing("acme/ro", "chore/sync")
	branch("acme/own", "touchmark/acme-eng", false, true)
	missing("acme/own", "chore/sync")
	repos := []platform.Repo{
		{Host: fx.provider("gitea").Host, ID: "7", Path: "acme/api"},
		{Host: fx.provider("gitea").Host, ID: "8", Path: "acme/ro"},
		{Host: fx.provider("gitea").Host, ID: "9", Path: "acme/own"},
		{Host: fx.provider("gitea").Host, ID: "10", Path: "acme/gone"},
	}
	fs, err := fx.writer.Check(t.Context(), repos, []string{"touchmark/acme-eng", "chore/sync"})
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, findingOf(t, fs, "acme/api", "access"), platform.FindingOK, "may push")
	wantFinding(t, findingOf(t, fs, "acme/api", "rules"), platform.FindingWarn, "branch touchmark/acme-eng is protected")
	wantFinding(t, findingOf(t, fs, "acme/ro", "access"), platform.FindingFail, "may not push")
	wantFinding(t, findingOf(t, fs, "acme/ro", "rules"), platform.FindingUnknown, "no sync branch exists yet")
	wantFinding(t, findingOf(t, fs, "acme/own", "access"), platform.FindingWarn, "administers")
	wantFinding(t, findingOf(t, fs, "acme/own", "rules"), platform.FindingOK, "no protection rule")
	wantFinding(t, findingOf(t, fs, "acme/gone", "access"), platform.FindingFail, "does not see")

	// A protected branch the writer may not push to blocks the target.
	branch("acme/api", "touchmark/acme-eng", true, false)
	fs, err = fx.writer.Check(t.Context(), repos[:1], []string{"touchmark/acme-eng", "chore/sync"})
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, findingOf(t, fs, "acme/api", "rules"), platform.FindingFail, "blocked:rules:protected-branch")

	// A refused credential ends the call.
	fx.json(http.MethodGet, "/repositories/7", http.StatusUnauthorized, fx.apiMsg("token is required"))
	if _, err := fx.writer.Check(t.Context(), repos[:1], nil); platform.ClassOf(err) != platform.ClassAuth {
		t.Errorf("Check with a refused credential: %v", err)
	}
	for _, c := range fx.requests("", "") {
		if c.Method != http.MethodGet {
			t.Errorf("Check wrote: %s %s", c.Method, c.Path)
		}
	}
}

func TestCheckSigningKey(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOr0writerKeyForTheTestsOnly0000000000000000"
	fx := newFixture(t, "forgejo")
	fx.settings(50)
	fx.json(http.MethodGet, "/user", http.StatusOK, fx.user(5, "touchmark-writer"))
	fx.pages("/user/keys", []any{
		map[string]any{"id": 1, "key": "ssh-ed25519 AAAAotherkey laptop", "title": "laptop", "key_type": "user", "fingerprint": "SHA256:x"},
		map[string]any{"id": 2, "key": key + " touchmark", "title": "signing", "key_type": "user", "fingerprint": "SHA256:y"},
	})
	f, err := fx.writer.CheckSigningKey(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, f, platform.FindingUnknown, `touchmark-writer's ("signing")`)
	f, err = fx.writer.CheckSigningKey(t.Context(), "ssh-ed25519 AAAAnotregistered")
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, f, platform.FindingFail, "not among touchmark-writer's SSH keys")
}
