package gitlab

import (
	"net/http"
	"strings"
	"testing"
	"time"

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

// fixedNow sets the driver's clock for the test.
func fixedNow(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

func TestCheckIdentity(t *testing.T) {
	fixedNow(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name            string
		token           map[string]any
		user            map[string]any
		expiry, scopes  platform.FindingStatus
		expDetail, scop string
		twoFA           platform.FindingStatus
	}{
		{"service account", map[string]any{"scopes": []string{"api", "write_repository"}, "expires_at": "2027-03-01", "active": true, "revoked": false},
			self(7, "service_account_group_9_writer", true), platform.FindingOK, platform.FindingOK, "expires on 2027-03-01", "api, write_repository", platform.FindingOK},
		{"expiring person", map[string]any{"scopes": []string{"api", "sudo"}, "expires_at": "2026-10-10", "active": true, "revoked": false},
			self(7, "jdoe", false), platform.FindingWarn, platform.FindingWarn, "in 10 days", "sudo", platform.FindingWarn},
		{"expired", map[string]any{"scopes": []string{"write_repository"}, "expires_at": "2026-09-29", "active": false, "revoked": false},
			self(7, "group_9_bot_0123abcd", true), platform.FindingFail, platform.FindingFail, "revoked or inactive", "lacks api", platform.FindingOK},
		{"no expiry", map[string]any{"scopes": []string{"api"}, "expires_at": nil, "active": true, "revoked": false},
			self(7, "group_9_bot_0123abcd", true), platform.FindingOK, platform.FindingOK, "no expiry date", "api", platform.FindingOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.json(http.MethodGet, "/user", http.StatusOK, tc.user)
			fx.json(http.MethodGet, "/personal_access_tokens/self", http.StatusOK, tc.token)
			fs, err := fx.writer.Check(t.Context(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantFinding(t, findingOf(t, fs, "", "token-expiry"), tc.expiry, tc.expDetail)
			wantFinding(t, findingOf(t, fs, "", "scopes"), tc.scopes, tc.scop)
			wantFinding(t, findingOf(t, fs, "", "2fa"), tc.twoFA, "")
		})
	}
	t.Run("person with 2FA", func(t *testing.T) {
		fx := newFixture(t)
		u := self(7, "jdoe", false)
		u["two_factor_enabled"] = true
		fx.json(http.MethodGet, "/user", http.StatusOK, u)
		fx.json(http.MethodGet, "/personal_access_tokens/self", http.StatusNotFound, msg("404 Not Found"))
		fs, err := fx.writer.Check(t.Context(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantFinding(t, findingOf(t, fs, "", "2fa"), platform.FindingOK, "two-factor")
		wantFinding(t, findingOf(t, fs, "", "token-expiry"), platform.FindingUnknown, "404")
	})
}

func TestCheckProject(t *testing.T) {
	fx := newFixture(t)
	fx.json(http.MethodGet, "/user", http.StatusOK, self(7, "service_account_writer", true))
	access := func(level int) projectOpt {
		return with("permissions", map[string]any{"project_access": nil, "group_access": map[string]any{"access_level": level, "notification_level": 3}})
	}
	fx.json(http.MethodGet, "/projects/11", http.StatusOK, project(11, "acme/api", access(30)))
	fx.json(http.MethodGet, "/projects/12", http.StatusOK, project(12, "acme/ro", access(20)))
	fx.json(http.MethodGet, "/projects/12/members/all/7", http.StatusNotFound, msg("404 Not found"))
	fx.json(http.MethodGet, "/projects/13", http.StatusOK, project(13, "acme/own", access(40)))
	fx.json(http.MethodGet, "/projects/14", http.StatusNotFound, msg("404 Project Not Found"))
	level := func(n int) map[string]any {
		return map[string]any{"id": n, "access_level": n, "access_level_description": "x", "deploy_key_id": nil, "user_id": nil, "group_id": nil}
	}
	fx.pages("/projects/11/protected_branches", []any{
		map[string]any{"id": 1, "name": "main", "allow_force_push": false, "push_access_levels": []any{level(0)}},
		map[string]any{"id": 2, "name": "touchmark/*", "allow_force_push": false, "push_access_levels": []any{level(30)}},
	})
	fx.pages("/projects/12/protected_branches", []any{})
	fx.pages("/projects/13/protected_branches", []any{
		map[string]any{"id": 3, "name": "*/acme-eng", "allow_force_push": true, "push_access_levels": []any{level(40)}},
	})
	repos := []platform.Repo{
		{Host: fx.provider().Host, ID: "11", Path: "acme/api"},
		{Host: fx.provider().Host, ID: "12", Path: "acme/ro"},
		{Host: fx.provider().Host, ID: "13", Path: "acme/own"},
		{Host: fx.provider().Host, ID: "14", Path: "acme/gone"},
	}
	fs, err := fx.writer.Check(t.Context(), repos, []string{"touchmark/acme-eng"})
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, findingOf(t, fs, "acme/api", "access"), platform.FindingOK, "Developer")
	wantFinding(t, findingOf(t, fs, "acme/api", "rules"), platform.FindingWarn, "touchmark/* covers touchmark/acme-eng without force pushes")
	wantFinding(t, findingOf(t, fs, "acme/ro", "access"), platform.FindingFail, "access level 20")
	wantFinding(t, findingOf(t, fs, "acme/ro", "rules"), platform.FindingOK, "no protected branch covers")
	wantFinding(t, findingOf(t, fs, "acme/own", "access"), platform.FindingWarn, "access level 40")
	wantFinding(t, findingOf(t, fs, "acme/own", "rules"), platform.FindingOK, "no protected branch covers")
	wantFinding(t, findingOf(t, fs, "acme/gone", "access"), platform.FindingFail, "does not see")

	// Maintainers only: the Developer writer is blocked.
	fx.pages("/projects/11/protected_branches", []any{
		map[string]any{"id": 2, "name": "touchmark/*", "allow_force_push": true, "push_access_levels": []any{level(40)}},
	})
	fs, err = fx.writer.Check(t.Context(), repos[:1], []string{"touchmark/acme-eng"})
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, findingOf(t, fs, "acme/api", "rules"), platform.FindingFail, "blocked:rules:protected-branch")
	for _, c := range fx.requests("", "") {
		if c.Method != http.MethodGet {
			t.Errorf("Check wrote: %s %s", c.Method, c.Path)
		}
	}
}

func TestProtectedMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, branch string
		want            bool
	}{
		{"touchmark/*", "touchmark/acme-eng", true},
		{"touchmark/*", "touchmark", false},
		{"*", "touchmark/acme-eng", true},
		{"*-eng", "touchmark/acme-eng", true},
		{"main", "main", true},
		{"main", "mainline", false},
		{"touch.mark/*", "touchXmark/a", false},
	} {
		if got := protectedMatch(tc.pattern, tc.branch); got != tc.want {
			t.Errorf("protectedMatch(%q, %q) = %v", tc.pattern, tc.branch, got)
		}
	}
}

func TestCheckSigningKey(t *testing.T) {
	fixedNow(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGitLabWriterKeyForTheTestsOnly000000000000"
	fx := newFixture(t)
	fx.json(http.MethodGet, "/user", http.StatusOK, self(7, "service_account_writer", true))
	keys := func(usage, expires string) {
		var exp any
		if expires != "" {
			exp = expires
		}
		fx.pages("/user/keys", []any{
			map[string]any{"id": 1, "title": "auth", "key": "ssh-ed25519 AAAAother", "usage_type": "auth", "created_at": "2026-09-01T10:00:00.000Z", "expires_at": nil},
			map[string]any{"id": 2, "title": "touchmark", "key": key + " touchmark@ci", "usage_type": usage, "created_at": "2026-09-01T10:00:00.000Z", "expires_at": exp},
		})
	}
	for _, tc := range []struct {
		usage, expires string
		status         platform.FindingStatus
		detail         string
	}{
		{"auth_and_signing", "", platform.FindingOK, `"touchmark" is service_account_writer's signing key`},
		{"signing", "2026-10-05T00:00:00.000Z", platform.FindingWarn, "expires on 2026-10-05"},
		{"signing", "2026-09-01T00:00:00.000Z", platform.FindingFail, "expired"},
		{"auth", "", platform.FindingFail, "authenticate only"},
	} {
		keys(tc.usage, tc.expires)
		f, err := fx.writer.CheckSigningKey(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		wantFinding(t, f, tc.status, tc.detail)
	}
	f, err := fx.writer.CheckSigningKey(t.Context(), "ssh-ed25519 AAAAnotregistered")
	if err != nil {
		t.Fatal(err)
	}
	wantFinding(t, f, platform.FindingFail, "not among")
}
