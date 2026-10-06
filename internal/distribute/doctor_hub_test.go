package distribute

import (
	"errors"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/report"
)

func hubWithIsolation(mode, reason string) *config.Hub {
	return &config.Hub{ID: "acme-eng", Security: config.Security{WriteIsolation: mode, Reason: reason}}
}

func TestIsolationChecks(t *testing.T) {
	actions := hubch.Context{CI: hubch.GitHubActions, RefName: "main", RefIsBranch: true, Event: "schedule"}
	onlyMain := hubch.Environment{Policies: []hubch.EnvironmentPolicy{{Name: "main", Type: "branch"}}}
	for _, tc := range []struct {
		name string
		in   IsolationInput
		want map[string]report.CheckStatus
	}{
		{"external", IsolationInput{Context: actions, Hub: hubWithIsolation("external", "")},
			map[string]report.CheckStatus{"write-isolation": report.StatusOK}},
		{"none", IsolationInput{Context: actions, Hub: hubWithIsolation("none", "a Gitea hub")},
			map[string]report.CheckStatus{"write-isolation": report.StatusWarn}},
		{"github isolated", IsolationInput{Context: actions, Getenv: env{"TOUCHMARK_KEY_EXPOSED": "false"}.get, EnvironmentRead: true, Environment: onlyMain},
			map[string]report.CheckStatus{"write-isolation": report.StatusOK, "environment": report.StatusOK}},
		{"github exposed", IsolationInput{Context: actions, Getenv: env{"TOUCHMARK_KEY_EXPOSED": "true"}.get, EnvironmentRead: true, Environment: hubch.Environment{AllRefs: true}},
			map[string]report.CheckStatus{"write-isolation": report.StatusFail, "environment": report.StatusFail}},
		{"github no probe, environment unreadable", IsolationInput{Context: actions, EnvironmentRead: true, EnvironmentErr: errors.New("HTTP 403")},
			map[string]report.CheckStatus{"write-isolation": report.StatusUnknown, "environment": report.StatusUnknown}},
		{"github no environment", IsolationInput{Context: actions, Getenv: env{"TOUCHMARK_KEY_EXPOSED": "false"}.get, EnvironmentRead: true,
			EnvironmentErr: hubch.ErrNoEnvironment}, map[string]report.CheckStatus{"write-isolation": report.StatusOK, "environment": report.StatusFail}},
		{"gitlab", IsolationInput{Context: hubch.Context{CI: hubch.GitLabCI}},
			map[string]report.CheckStatus{"write-isolation": report.StatusUnknown}},
		{"gitea", IsolationInput{Context: hubch.Context{CI: hubch.GiteaActions}},
			map[string]report.CheckStatus{"write-isolation": report.StatusFail}},
		{"local", IsolationInput{Context: hubch.Context{CI: hubch.Local}},
			map[string]report.CheckStatus{"write-isolation": report.StatusUnknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]report.CheckStatus{}
			for _, c := range IsolationChecks(tc.in) {
				got[c.Name] = c.Status
				if c.Detail == "" {
					t.Errorf("%s has no detail", c.Name)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("checks %v, want %v", got, tc.want)
			}
			for name, status := range tc.want {
				if got[name] != status {
					t.Errorf("%s: %s, want %s", name, got[name], status)
				}
			}
		})
	}
}

// env is a test environment of variables.
type env map[string]string

func (e env) get(k string) string { return e[k] }

// keyChecks returns the checks of name as "status: detail" lines.
func keyChecks(cs []report.DoctorCheck, name string) []string {
	var out []string
	for _, c := range cs {
		if c.Name == name {
			out = append(out, string(c.Status)+": "+c.Detail)
		}
	}
	return out
}

// hasCheck reports whether some line of lines starts with status and holds
// sub.
func hasCheck(lines []string, status report.CheckStatus, sub string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, string(status)+": ") && strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestKeyLocationGitHub(t *testing.T) {
	ks := hubch.KeyStore{
		Platform: "github", RepoPath: "acme/engineering-assets", DefaultBranch: "main",
		Secrets: []hubch.Secret{
			{Name: "GH_WRITER_KEY", Where: "repository"},
			{Name: "TOUCHMARK_GH_WRITE_APP_KEY", Where: "environment", Environment: "touchmark-distribute"},
			{Name: "TOUCHMARK_CORP_WRITE_TOKEN", Where: "environment", Environment: "release"},
			{Name: "TOUCHMARK_GH_SIGNING_KEY", Where: "organization"},
			{Name: "NPM_TOKEN", Where: "repository"},
			{Name: "TOUCHMARK_GH_WRITE_TOKEN", Where: "dependabot"},
		},
		Environments: []hubch.NamedEnvironment{
			{Name: "touchmark-distribute", Environment: hubch.Environment{Policies: []hubch.EnvironmentPolicy{{Name: "main", Type: "branch"}}}},
			{Name: "release", Environment: hubch.Environment{AllRefs: true}},
		},
		Unread: []string{"the organization secrets shared with the repository: HTTP 403"},
	}
	lines := keyChecks(KeyLocationChecks(ks, []string{"GH_WRITER_KEY"}, hubWithIsolation("platform", "")), "key-location")
	for _, want := range []struct {
		status report.CheckStatus
		sub    string
	}{
		{report.StatusFail, "repository secret GH_WRITER_KEY"},
		{report.StatusOK, "secret TOUCHMARK_GH_WRITE_APP_KEY is in environment touchmark-distribute"},
		{report.StatusFail, "secret TOUCHMARK_CORP_WRITE_TOKEN of environment release: the environment release lets any branch"},
		{report.StatusFail, "organization secret TOUCHMARK_GH_SIGNING_KEY"},
		{report.StatusFail, "Dependabot secret TOUCHMARK_GH_WRITE_TOKEN"},
		{report.StatusUnknown, "HTTP 403"},
	} {
		if !hasCheck(lines, want.status, want.sub) {
			t.Errorf("no %s check with %q in:\n%s", want.status, want.sub, strings.Join(lines, "\n"))
		}
	}
	for _, l := range lines {
		if strings.Contains(l, "NPM_TOKEN") {
			t.Errorf("a secret without a write key is graded: %s", l)
		}
	}
	// No environment touchmark-distribute, and no key found.
	lines = keyChecks(KeyLocationChecks(hubch.KeyStore{Platform: "github", DefaultBranch: "main"}, nil, nil), "key-location")
	if !hasCheck(lines, report.StatusFail, "no environment touchmark-distribute") || !hasCheck(lines, report.StatusUnknown, "no secret or variable named like a write key") {
		t.Errorf("empty store:\n%s", strings.Join(lines, "\n"))
	}
}

func TestKeyLocationGitLab(t *testing.T) {
	ks := hubch.KeyStore{
		Platform: "gitlab", RepoPath: "acme/engineering-assets", DefaultBranch: "main", PipelineVariables: "developer",
		Secrets: []hubch.Secret{
			{Name: "TOUCHMARK_CORP_WRITE_TOKEN", Where: "project", Protected: true, Masked: true, Scope: "touchmark-distribute"},
			{Name: "TOUCHMARK_X_WRITE_TOKEN", Where: "project", Protected: false, Masked: true, Scope: "touchmark-distribute"},
			{Name: "TOUCHMARK_Y_WRITE_TOKEN", Where: "group", Group: "acme", Protected: true, Masked: false, Scope: "*"},
			{Name: "SONAR_TOKEN", Where: "project"},
		},
		ProtectedBranches: []hubch.ProtectedRef{
			{Name: "main", Levels: []int{0, 40}},
			{Name: "release/*", Levels: []int{30}},
			{Name: "hotfix", Levels: []int{40}},
		},
		ProtectedTags: []hubch.ProtectedRef{{Name: "v*", Levels: []int{30}}},
	}
	cs := KeyLocationChecks(ks, nil, hubWithIsolation("platform", ""))
	keys := keyChecks(cs, "key-location")
	for _, want := range []struct {
		status report.CheckStatus
		sub    string
	}{
		{report.StatusOK, "project variable TOUCHMARK_CORP_WRITE_TOKEN is protected and scoped"},
		{report.StatusFail, "project variable TOUCHMARK_X_WRITE_TOKEN is not protected"},
		{report.StatusWarn, `variable TOUCHMARK_Y_WRITE_TOKEN of group acme is protected with environment scope "*"`},
		{report.StatusWarn, "variable TOUCHMARK_Y_WRITE_TOKEN of group acme is not masked"},
	} {
		if !hasCheck(keys, want.status, want.sub) {
			t.Errorf("no %s check with %q in:\n%s", want.status, want.sub, strings.Join(keys, "\n"))
		}
	}
	if b := keyChecks(cs, "protected-branches"); len(b) != 1 || !hasCheck(b, report.StatusFail, "release/* lets Developers push") ||
		!strings.Contains(b[0], "protected branch hotfix") {
		t.Errorf("protected branches: %q", b)
	}
	if tags := keyChecks(cs, "protected-tags"); !hasCheck(tags, report.StatusFail, "v* may be created by Developers") {
		t.Errorf("protected tags: %q", tags)
	}
	if pv := keyChecks(cs, "pipeline-variables"); !hasCheck(pv, report.StatusWarn, "developer") {
		t.Errorf("pipeline variables: %q", pv)
	}
	ks.ProtectedBranches, ks.ProtectedTags, ks.PipelineVariables = ks.ProtectedBranches[:1], nil, "no_one_allowed"
	cs = KeyLocationChecks(ks, nil, nil)
	for _, name := range []string{"protected-branches", "protected-tags", "pipeline-variables"} {
		if got := keyChecks(cs, name); len(got) != 1 || !strings.HasPrefix(got[0], "ok: ") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestKeyLocationGitea(t *testing.T) {
	ks := hubch.KeyStore{Platform: "gitea", Secrets: []hubch.Secret{{Name: "TOUCHMARK_WRITE_TOKEN", Where: "repository"}}}
	if got := keyChecks(KeyLocationChecks(ks, nil, nil), "key-location"); !hasCheck(got, report.StatusFail, "every branch's jobs the secrets") {
		t.Errorf("platform: %q", got)
	}
	if got := keyChecks(KeyLocationChecks(ks, nil, hubWithIsolation("none", "one maintainer")), "key-location"); !hasCheck(got, report.StatusWarn, "accepted the risk") {
		t.Errorf("none: %q", got)
	}
}

func TestIsWriteKeyName(t *testing.T) {
	for name, want := range map[string]bool{
		"TOUCHMARK_WRITE_TOKEN":        true,
		"TOUCHMARK_GH_WRITE_APP_KEY":   true,
		"touchmark_corp_signing_key":   true,
		"GH_WRITE_APP_KEY":             true,
		"gh-write-token":               true,
		"TOUCHMARK_GH_WRITE_APP_ID":    false,
		"TOUCHMARK_GH_READ_TOKEN":      false,
		"NPM_TOKEN":                    false,
		"WRITER":                       false,
		"TOUCHMARK_GH_WRITE_TOKEN_OLD": false,
	} {
		if got := IsWriteKeyName(name, nil); got != want {
			t.Errorf("IsWriteKeyName(%q) = %v", name, got)
		}
	}
	if !IsWriteKeyName("deploy_key", []string{"DEPLOY_KEY"}) {
		t.Error("a secret the workflows hand touchmark is a write key")
	}
}

// A listing the token could not read makes its check unknown, never ok or
// fail: GitLab's protected branches and tags, GitHub's environments and an
// environment's deployment policy.
func TestKeyLocationUnread(t *testing.T) {
	ks := hubch.KeyStore{Platform: "gitlab", DefaultBranch: "main", PipelineVariables: "no_one_allowed",
		ProtectedBranchesUnread: "HTTP 403", ProtectedTagsUnread: "HTTP 403"}
	cs := KeyLocationChecks(ks, nil, nil)
	for _, name := range []string{"protected-branches", "protected-tags"} {
		if got := keyChecks(cs, name); len(got) != 1 || !hasCheck(got, report.StatusUnknown, "HTTP 403") {
			t.Errorf("%s: %q", name, got)
		}
	}
	gh := hubch.KeyStore{Platform: "github", DefaultBranch: "main", EnvironmentsUnread: "HTTP 403",
		Secrets:      []hubch.Secret{{Name: "TOUCHMARK_WRITE_APP_KEY", Where: "environment", Environment: "deploy"}},
		Environments: []hubch.NamedEnvironment{{Name: "deploy", Err: errors.New("HTTP 403")}}}
	lines := keyChecks(KeyLocationChecks(gh, nil, hubWithIsolation("platform", "")), "key-location")
	for _, l := range lines {
		if strings.HasPrefix(l, "fail: ") {
			t.Errorf("a check of what could not be read fails: %s", l)
		}
	}
	if !hasCheck(lines, report.StatusUnknown, "environment touchmark-distribute is not known") || !hasCheck(lines, report.StatusUnknown, "TOUCHMARK_WRITE_APP_KEY") {
		t.Errorf("lines:\n%s", strings.Join(lines, "\n"))
	}
}
