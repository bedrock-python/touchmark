package github

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// rule is a rule of GET /rules/branches/{branch} (the shape recorded on
// github.com: type, ruleset source and id, parameters).
func rule(typ string) map[string]any {
	return map[string]any{"type": typ, "ruleset_source_type": "Organization", "ruleset_source": "acme", "ruleset_id": 42}
}

// TestPreflight: required signatures on any branch, force pushes and
// deletions blocked per branch, the sync branch asked for before it
// exists; the writer adds its installation's Workflows permission.
func TestPreflight(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.pages("/repos/acme/api/rules/branches/main", []any{rule("pull_request"), rule("required_signatures"), rule("non_fast_forward")})
	f.pages("/repos/acme/api/rules/branches/"+syncBranch, []any{rule("non_fast_forward"), rule("creation"), rule("deletion")})
	branches := []string{"main", syncBranch, "main"}
	got, err := f.reader.Preflight(t.Context(), apiRepoTarget, branches)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Known || !got.SignedCommits || !slices.Equal(got.NoForcePush, []string{"main", syncBranch}) ||
		!slices.Equal(got.NoDelete, []string{syncBranch}) || got.WorkflowsKnown {
		t.Errorf("reader Preflight = %+v", got)
	}
	if c := f.requests(http.MethodGet, "/repos/acme/api/rules/branches/"+syncBranch); len(c) != 1 || c[0].RawPath != "/api/v3/repos/acme/api/rules/branches/touchmark%2Facme" {
		t.Errorf("the sync branch was asked as %+v", c)
	}

	f.repoInstallation(nil)
	f.json(http.MethodGet, "/repos/acme/api/rulesets/42", http.StatusOK, ruleset(42, "never"))
	got, err = f.writer.Preflight(t.Context(), apiRepoTarget, branches)
	if err != nil || !got.WorkflowsKnown || !got.Workflows || !got.SignedCommits || !slices.Equal(got.NoForcePush, []string{"main", syncBranch}) {
		t.Errorf("writer with Workflows: %+v, %v", got, err)
	}

	// The permissions are the installation's, read once for the run.
	g := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	g.pages("/repos/acme/api/rules/branches/main", []any{rule("non_fast_forward")})
	g.pages("/repos/acme/api/rules/branches/"+syncBranch, []any{})
	noWorkflows := map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
	g.handle(http.MethodGet, "/orgs/acme/installation", g.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, installation(instAcme, "acme", "Organization", noWorkflows))
	}))
	g.repoInstallation(noWorkflows)
	for range 2 {
		got, err = g.writer.Preflight(t.Context(), apiRepoTarget, branches)
		if err != nil || !got.WorkflowsKnown || got.Workflows || got.SignedCommits {
			t.Errorf("writer without Workflows: %+v, %v", got, err)
		}
	}
	if n := len(g.requests(http.MethodGet, "/orgs/acme/installation")) + len(g.requests(http.MethodGet, "/repos/acme/api/installation")); n != 1 {
		t.Errorf("%d reads of the installation, want 1", n)
	}

	// Rules the identity cannot read: unknown, not an error.
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		f.json(http.MethodGet, "/repos/acme/api/rules/branches/main", status, ghError("Resource not accessible by integration"))
		got, err = f.reader.Preflight(t.Context(), apiRepoTarget, branches)
		if err != nil || got.Known || got.SignedCommits || len(got.NoForcePush) != 0 {
			t.Errorf("HTTP %d: %+v, %v", status, got, err)
		}
	}
	f.json(http.MethodGet, "/repos/acme/api/rules/branches/main", http.StatusForbidden, map[string]any{"message": "You have exceeded a secondary rate limit."})
	_, err = f.reader.Preflight(t.Context(), apiRepoTarget, branches)
	wantClass(t, "rate limit", err, platform.ClassRateLimited, nil)

	// The writer's installation cannot be read for another reason than a
	// 404: the call fails (the core retries), it does not guess.
	h := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	h.pages("/repos/acme/api/rules/branches/main", []any{})
	h.handle(http.MethodGet, "/orgs/acme/installation", h.asApp(func(w http.ResponseWriter, _ *http.Request) {
		inst := installation(instAcme, "acme", "Organization", nil)
		delete(inst, "permissions") // the owner's lookup names none
		writeJSON(w, http.StatusOK, inst)
	}))
	h.handle(http.MethodGet, "/repos/acme/api/installation", h.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadGateway, ghError("Server Error"))
	}))
	_, err = h.writer.Preflight(t.Context(), apiRepoTarget, []string{"main"})
	wantClass(t, "the installation unreadable", err, platform.ClassTransient, nil)
}

// ruleset is GET /repos/{owner}/{repo}/rulesets/{id} (trimmed to what the
// driver reads and a few fields around it).
func ruleset(id int64, canBypass string) map[string]any {
	return map[string]any{"id": id, "name": "branch rules", "target": "branch", "source_type": "Organization", "source": "acme",
		"enforcement": "active", "current_user_can_bypass": canBypass}
}

// ruleIn is a rule of GET /rules/branches/{branch} from ruleset id.
func ruleIn(typ string, id int64) map[string]any {
	r := rule(typ)
	r["ruleset_id"] = id
	return r
}

// TestPreflightBypass: rules/branches lists every active rule whoever may
// bypass it; the writer drops the rules of the sync branch whose ruleset
// it may bypass (current_user_can_bypass always or exempt), reading each
// ruleset once, and keeps those it may not (pull_requests_only, never, a
// ruleset it cannot read) and every rule of the default branch, which the
// person who merges meets. The reader cannot tell the writer's bypass and
// keeps them all.
func TestPreflightBypass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		canBypass string
		dropped   bool
	}{
		{"always", http.StatusOK, "always", true},
		{"exempt", http.StatusOK, "exempt", true},
		{"pull requests only", http.StatusOK, "pull_requests_only", false},
		{"never", http.StatusOK, "never", false},
		{"unreadable", http.StatusNotFound, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
			f.pages("/repos/acme/api/rules/branches/main", []any{ruleIn("required_signatures", 7), ruleIn("non_fast_forward", 7)})
			f.pages("/repos/acme/api/rules/branches/"+syncBranch, []any{ruleIn("non_fast_forward", 7), ruleIn("required_signatures", 7),
				ruleIn("non_fast_forward", 8)})
			f.repoInstallation(nil)
			if tc.status == http.StatusOK {
				f.json(http.MethodGet, "/repos/acme/api/rulesets/7", http.StatusOK, ruleset(7, tc.canBypass))
			} else {
				f.json(http.MethodGet, "/repos/acme/api/rulesets/7", tc.status, notFoundBody)
			}
			f.json(http.MethodGet, "/repos/acme/api/rulesets/8", http.StatusOK, ruleset(8, "never"))

			// Ruleset 7 on the sync branch only: ruleset 8 still forbids force
			// pushes there, and the default branch keeps both of 7's rules.
			got, err := f.writer.Preflight(t.Context(), apiRepoTarget, []string{"main", syncBranch})
			if err != nil || !got.Known || !got.SignedCommits || !slices.Equal(got.NoForcePush, []string{"main", syncBranch}) {
				t.Errorf("with ruleset 8: %+v, %v", got, err)
			}
			if c := f.requests(http.MethodGet, "/repos/acme/api/rulesets/7"); len(c) != 1 {
				t.Errorf("ruleset 7 read %d times, want once", len(c))
			}
			if c := f.requests(http.MethodGet, "/repos/acme/api/rulesets/7"); len(c) == 1 {
				if m, ok := f.tokenOf(c[0].Auth); !ok || m.installation != instAcme {
					t.Errorf("the ruleset was read with %+v", m)
				}
			}

			// Without ruleset 8, the sync branch's rules go with 7's bypass.
			f.pages("/repos/acme/api/rules/branches/"+syncBranch, []any{ruleIn("non_fast_forward", 7), ruleIn("required_signatures", 7)})
			got, err = f.writer.Preflight(t.Context(), apiRepoTarget, []string{syncBranch})
			wantForce := []string{syncBranch}
			if tc.dropped {
				wantForce = nil
			}
			if err != nil || got.SignedCommits == tc.dropped || !slices.Equal(got.NoForcePush, wantForce) {
				t.Errorf("sync branch only: %+v, %v; want the rules dropped %v", got, err, tc.dropped)
			}
			got, err = f.reader.Preflight(t.Context(), apiRepoTarget, []string{syncBranch})
			if err != nil || !got.SignedCommits || !slices.Equal(got.NoForcePush, []string{syncBranch}) {
				t.Errorf("reader: %+v, %v", got, err)
			}
		})
	}

	// A transient failure or a rate limit of the ruleset fails the call.
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.pages("/repos/acme/api/rules/branches/"+syncBranch, []any{ruleIn("non_fast_forward", 7)})
	f.repoInstallation(nil)
	f.json(http.MethodGet, "/repos/acme/api/rulesets/7", http.StatusBadGateway, ghError("Server Error"))
	_, err := f.writer.Preflight(t.Context(), apiRepoTarget, []string{syncBranch})
	wantClass(t, "a transient ruleset", err, platform.ClassTransient, nil)
}

// TestCheck: doctor's findings for an App and for a token.
func TestCheck(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.repoInstallation(map[string]string{"contents": "write", "pull_requests": "read", "metadata": "read"})
	f.pages("/repos/acme/api/rules/branches/main", []any{rule("required_signatures")})
	f.pages("/repos/acme/api/rules/branches/"+syncBranch, []any{rule("non_fast_forward")})
	f.json(http.MethodGet, "/repos/acme/api/rulesets/42", http.StatusOK, ruleset(42, "never"))
	f.json(http.MethodGet, "/repos/acme/gone/installation", http.StatusNotFound, notFoundBody)
	gone := platform.Repo{Host: "github.com", ID: "102", Path: "acme/gone"}
	got, err := f.writer.Check(t.Context(), []platform.Repo{apiRepoTarget, gone}, []string{"main", syncBranch})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]platform.FindingStatus{
		"acme/api access": platform.FindingOK, "acme/api permissions": platform.FindingFail,
		"acme/api workflows": platform.FindingWarn, "acme/api rules": platform.FindingWarn, "acme/gone access": platform.FindingFail,
	}
	if len(got) != len(want) {
		t.Errorf("findings %+v", got)
	}
	for _, fd := range got {
		if s, ok := want[fd.Repo+" "+fd.Check]; !ok || s != fd.Status || fd.Detail == "" {
			t.Errorf("finding %+v", fd)
		}
	}

	tf := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	tf.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(101, "acme/api",
		withField("permissions", map[string]any{"push": true, "pull": true})))
	tf.pages("/repos/acme/api/rules/branches/main", []any{rule("required_signatures")})
	got, err = tf.writer.Check(t.Context(), []platform.Repo{apiRepoTarget}, []string{"main"})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]platform.FindingStatus{}
	for _, fd := range got {
		statuses[fd.Check] = fd.Status
	}
	if statuses["access"] != platform.FindingOK || statuses["workflows"] != platform.FindingUnknown || statuses["rules"] != platform.FindingWarn {
		t.Errorf("token findings %+v", got)
	}
}

// An installation with more permissions than touchmark needs warns, and
// fails with those that reach beyond the targets' files: a leaked App key
// mints tokens with all of them (threat T3 in
// docs/project/threat-model.md).
func TestAppPermissions(t *testing.T) {
	for _, tc := range []struct {
		perms  map[string]string
		status platform.FindingStatus
		detail string
	}{
		{map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read", "workflows": "write"}, platform.FindingOK, "contents:write, pull_requests:write"},
		{map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read", "issues": "write"}, platform.FindingWarn, "more than touchmark needs: issues:write"},
		{map[string]string{"contents": "write", "pull_requests": "write", "actions": "read"}, platform.FindingWarn, "actions:read"},
		{map[string]string{"contents": "write", "pull_requests": "write", "administration": "write", "secrets": "read"}, platform.FindingFail,
			"more than touchmark needs: administration:write, secrets:read"},
		{map[string]string{"contents": "write", "pull_requests": "write", "actions": "write", "organization_administration": "read"}, platform.FindingFail, "actions:write"},
		{map[string]string{"contents": "read", "pull_requests": "write", "members": "read"}, platform.FindingFail, "lacks contents:write; the installation has more"},
	} {
		status, detail := appPermissions(tc.perms)
		if status != tc.status || !strings.Contains(detail, tc.detail) {
			t.Errorf("%v: %s %q, want %s with %q", tc.perms, status, detail, tc.status, tc.detail)
		}
	}
}

// TestCheckIdentity: with no repository, Check reports the identity's own
// checks without a request: an App mints its tokens and has no second
// factor; a token's are not read.
func TestCheckIdentity(t *testing.T) {
	for kind, want := range map[credKind]platform.FindingStatus{credApp: platform.FindingOK, credToken: platform.FindingUnknown} {
		f := newFixture(t, fixtureOpts{kind: kind, host: "github.com"})
		got, err := f.writer.Check(t.Context(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		checks := map[string]platform.FindingStatus{}
		for _, fd := range got {
			checks[fd.Check] = fd.Status
			if fd.Repo != "" || fd.Detail == "" {
				t.Errorf("finding %+v", fd)
			}
		}
		if checks["token-expiry"] != want || checks["2fa"] != want || len(checks) != 2 {
			t.Errorf("kind %v: findings %+v", kind, got)
		}
	}
}
