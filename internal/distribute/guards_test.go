package distribute

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
)

// guardEnv is an environment of variables.
type guardEnv map[string]string

func (e guardEnv) get(name string) string { return e[name] }

// The CI contexts of the guard tests: a run of the hub's default branch on
// each CI, as hubch.Detect reads it.
var (
	ghMain = hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "schedule"}
	glMain = hubch.Context{CI: hubch.GitLabCI, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "schedule",
		RefProtected: true, Environment: "touchmark-distribute"}
	giteaMain = hubch.Context{CI: hubch.GiteaActions, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "push"}
)

func TestDistributeGuard(t *testing.T) {
	ok := gitx.DeliveryMinVersion
	hub := func(isolation string) *config.Hub {
		h := &config.Hub{ID: "acme-eng"}
		h.Security.WriteIsolation = isolation
		if isolation == "none" {
			h.Security.Reason = "the hub runs on Gitea"
		}
		return h
	}
	exposed := func(v string) guardEnv { return guardEnv{"TOUCHMARK_KEY_EXPOSED": v} }
	with := func(c hubch.Context, edit func(*hubch.Context)) hubch.Context {
		edit(&c)
		return c
	}
	cases := []struct {
		name string
		in   GuardInput
		env  guardEnv
		want []string // substrings of the error; none for no error
	}{
		{name: "local", in: GuardInput{Context: hubch.Context{CI: hubch.Local}, Hub: hub("")}},
		{name: "local with operation flags", in: GuardInput{Context: hubch.Context{CI: hubch.Local}, LocalOps: true}},
		{name: "old git", in: GuardInput{Context: hubch.Context{CI: hubch.Local}, GitVersion: [3]int{2, 44, 9}},
			want: []string{"git 2.44.9 is too old: distribute needs git 2.45.0 or newer"}},
		{name: "old git in a dry run", in: GuardInput{Context: hubch.Context{CI: hubch.Local}, GitVersion: [3]int{2, 33, 0}, DryRun: true},
			want: []string{"git 2.33.0 is too old"}},
		{name: "operation flags with CI set", in: GuardInput{Context: hubch.Context{CI: hubch.Local}, LocalOps: true},
			env: guardEnv{"CI": "true"}, want: []string{"operation flags", "work only in a local run", ".touchmark/operations.yml"}},
		{name: "another CI", in: GuardInput{Context: hubch.Context{CI: hubch.Local}}, env: guardEnv{"CI": "woodpecker"},
			want: []string{"cannot tell which ref this CI job builds"}},
		{name: "CI false", in: GuardInput{Context: hubch.Context{CI: hubch.Local}, LocalOps: true}, env: guardEnv{"CI": "false"}},

		{name: "github default branch", in: GuardInput{Context: ghMain, Hub: hub("")}, env: exposed("false")},
		{name: "github probe says exposed", in: GuardInput{Context: ghMain, Hub: hub("platform")}, env: exposed("TRUE"),
			want: []string{"TOUCHMARK_KEY_EXPOSED=true", "outside the environment touchmark-distribute"}},
		{name: "github without probe", in: GuardInput{Context: ghMain, Hub: hub("")},
			want: []string{"TOUCHMARK_KEY_EXPOSED is not set", "no probe job"}},
		{name: "github garbled probe", in: GuardInput{Context: ghMain, Hub: hub("")}, env: exposed("maybe"),
			want: []string{"must be true or false"}},
		{name: "github external", in: GuardInput{Context: ghMain, Hub: hub("external")}},
		{name: "github none", in: GuardInput{Context: ghMain, Hub: hub("none")}},
		{name: "github pull request", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) {
			c.Event, c.RefName, c.RefIsBranch = "pull_request", "41/merge", false
		}), Hub: hub("")}, env: exposed("false"),
			want: []string{"does not run for the pull_request event", "41/merge, which is not a branch"}},
		{name: "github pull_request_target", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) { c.Event = "pull_request_target" }), Hub: hub("")},
			env: exposed("false"), want: []string{"pull_request_target event"}},
		{name: "github other branch", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) { c.RefName, c.Event = "feature", "push" }), Hub: hub("")},
			env: exposed("false"), want: []string{"only on the hub's default branch main, not on feature"}},
		{name: "github tag", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) { c.RefName, c.RefIsBranch, c.Event = "v1.0", false, "push" }), Hub: hub("")},
			env: exposed("false"), want: []string{"this job builds v1.0, which is not a branch"}},
		{name: "github schedule names no default branch", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) { c.DefaultBranch = "" }), Hub: hub("")},
			env: exposed("false")},
		{name: "github push names no default branch", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) { c.DefaultBranch, c.Event = "", "push" }), Hub: hub("")},
			env: exposed("false"), want: []string{"the CI names no default branch of the hub"}},
		// A dry run holds the write credential and builds its drivers from
		// the ref's hub.yml: a pull request's job may not run one either.
		{name: "github dry run of a pull request", in: GuardInput{Context: with(ghMain, func(c *hubch.Context) {
			c.Event, c.RefName, c.RefIsBranch = "pull_request", "41/merge", false
		}), Hub: hub(""), DryRun: true}, env: exposed("false"), want: []string{"does not run for the pull_request event"}},
		{name: "github dry run keeps the probe", in: GuardInput{Context: ghMain, Hub: hub(""), DryRun: true}, env: exposed("true"),
			want: []string{"TOUCHMARK_KEY_EXPOSED=true"}},
		{name: "github dry run keeps the operation flags", in: GuardInput{Context: ghMain, Hub: hub(""), DryRun: true, LocalOps: true},
			env: exposed("false"), want: []string{"operation flags"}},

		{name: "gitlab default branch", in: GuardInput{Context: glMain, Hub: hub("")}},
		{name: "gitlab unprotected", in: GuardInput{Context: with(glMain, func(c *hubch.Context) { c.RefProtected = false }), Hub: hub("")},
			want: []string{"CI_COMMIT_REF_PROTECTED=true"}},
		{name: "gitlab without environment", in: GuardInput{Context: with(glMain, func(c *hubch.Context) { c.Environment = "production" }), Hub: hub("")},
			want: []string{"environment touchmark-distribute"}},
		{name: "gitlab merge request", in: GuardInput{Context: with(glMain, func(c *hubch.Context) {
			c.Event, c.RefName, c.RefProtected = "merge_request_event", "feature", false
		}), Hub: hub("")},
			want: []string{"merge_request_event", "not on feature", "CI_COMMIT_REF_PROTECTED"}},
		{name: "gitlab dry run of a merge request", in: GuardInput{Context: with(glMain, func(c *hubch.Context) {
			c.Event, c.RefName, c.RefProtected, c.Environment = "merge_request_event", "feature", false, ""
		}), Hub: hub(""), DryRun: true}, want: []string{"merge_request_event", "CI_COMMIT_REF_PROTECTED"}},

		{name: "gitea platform", in: GuardInput{Context: giteaMain, Hub: hub("")},
			want: []string{"platform cannot hold on Gitea and Forgejo Actions"}},
		{name: "forgejo platform", in: GuardInput{Context: with(giteaMain, func(c *hubch.Context) { c.CI = hubch.ForgejoActions }), Hub: hub("platform")},
			want: []string{"platform cannot hold"}},
		{name: "gitea external", in: GuardInput{Context: giteaMain, Hub: hub("external")}},
		{name: "gitea none", in: GuardInput{Context: giteaMain, Hub: hub("none")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if in.GitVersion == [3]int{} {
				in.GitVersion = ok
			}
			env := tc.env
			in.Getenv = env.get
			err := DistributeGuard(t.Context(), in)
			if len(tc.want) == 0 {
				if err != nil {
					t.Errorf("error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("no error, want %q", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			for _, v := range env {
				if len(v) > 8 && strings.Contains(err.Error(), v) {
					t.Errorf("error %q quotes a value", err)
				}
			}
		})
	}
	// Without a Getenv nothing is read.
	if err := DistributeGuard(t.Context(), GuardInput{Context: hubch.Context{CI: hubch.Local}, GitVersion: ok, LocalOps: true}); err != nil {
		t.Errorf("no environment: %v", err)
	}
}

func TestProbe(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"TOUCHMARK_GH_READ_TOKEN=read-only-token-value",
		"TOUCHMARK_GH_WRITE_TOKEN=write-token-value-0123",
		"TOUCHMARK_CORP_WRITE_APP_KEY=-----BEGIN RSA PRIVATE KEY-----",
		"touchmark_old_id_write_app_id=12345",
		"TOUCHMARK_SIGNING_KEY=-----BEGIN OPENSSH PRIVATE KEY-----",
		"TOUCHMARK_GL_WRITE_TOKEN=   ",
		"TOUCHMARK_WRITE_TOKENS_URL=https://vault.example",
		"MY_TOUCHMARK_GH_WRITE_TOKEN=elsewhere-0123456789",
		"TOUCHMARK_KEY_EXPOSED=false",
		"=C:=C:\\work",
	}
	getenv := func(name string) string {
		if name == "TOUCHMARK_WRITE_APP_ID" {
			return "67890"
		}
		return ""
	}
	res := Probe(getenv, func() []string { return environ })
	want := []string{
		"TOUCHMARK_CORP_WRITE_APP_KEY",
		"TOUCHMARK_GH_WRITE_TOKEN",
		"TOUCHMARK_SIGNING_KEY",
		"TOUCHMARK_WRITE_APP_ID",
		"TOUCHMARK_WRITE_TOKENS_URL",
		"touchmark_old_id_write_app_id",
	}
	if !slices.Equal(res.Exposed, want) {
		t.Errorf("Exposed %q, want %q", res.Exposed, want)
	}
	if res := Probe(nil, nil); len(res.Exposed) != 0 {
		t.Errorf("an empty environment: %q", res.Exposed)
	}
	if res := Probe(func(string) string { return "" }, func() []string { return []string{"PATH=/bin", "TOUCHMARK_GH_READ_TOKEN=x"} }); len(res.Exposed) != 0 {
		t.Errorf("read credentials only: %q", res.Exposed)
	}
}

// TestCheckEnvironment: the environment touchmark-distribute may let only
// the hub's default branch use it; the check fails closed: what cannot be
// read or told is a refusal too, never a pass.
func TestCheckEnvironment(t *testing.T) {
	t.Parallel()
	custom := func(ps ...hubch.EnvironmentPolicy) hubch.Environment { return hubch.Environment{Policies: ps} }
	branch := func(name string) hubch.EnvironmentPolicy { return hubch.EnvironmentPolicy{Name: name, Type: "branch"} }
	for _, tc := range []struct {
		name             string
		env              hubch.Environment
		readErr          error
		def              string
		warning, refusal string
	}{
		{name: "the default branch only", env: custom(branch("main")), def: "main"},
		{name: "no branch at all", env: custom(), def: "main"},
		{name: "no environment", readErr: fmt.Errorf("read: %w", hubch.ErrNoEnvironment), def: "main", refusal: "has no environment touchmark-distribute"},
		{name: "unreadable", readErr: errors.New("403 Forbidden"), def: "main", refusal: "could not be read (403 Forbidden)"},
		{name: "a server error", readErr: errors.New("502 Bad Gateway"), def: "main", refusal: "actions: read permission"},
		{name: "any ref", env: hubch.Environment{AllRefs: true}, def: "main", refusal: "lets any branch and tag use it"},
		{name: "protected branches", env: hubch.Environment{ProtectedBranches: true}, def: "main", refusal: "lets every protected branch use it"},
		{name: "a pattern", env: custom(branch("ma*")), def: "main", refusal: "lets branch ma* use it"},
		{name: "a tag", env: custom(branch("main"), hubch.EnvironmentPolicy{Name: "main", Type: "tag"}), def: "main", refusal: "tag main"},
		{name: "unknown default branch", env: custom(branch("main")), refusal: "an unknown ref"},
	} {
		warning, err := CheckEnvironment(tc.env, tc.readErr, tc.def)
		switch {
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.refusal)
		case tc.refusal == "" && err != nil:
			t.Errorf("%s: err %v", tc.name, err)
		case (tc.warning == "") != (warning == "") || !strings.Contains(warning, tc.warning):
			t.Errorf("%s: warning %q, want %q", tc.name, warning, tc.warning)
		}
	}
	for _, tc := range []struct {
		ci   hubch.CI
		mode string
		want bool
	}{
		{hubch.GitHubActions, "", true}, {hubch.GitHubActions, "platform", true}, {hubch.GitHubActions, "external", false},
		{hubch.GitLabCI, "", false}, {hubch.Local, "", false},
	} {
		cfg := &config.Hub{}
		cfg.Security.WriteIsolation = tc.mode
		if got := EnvironmentGuardApplies(hubch.Context{CI: tc.ci}, cfg); got != tc.want {
			t.Errorf("%s/%q: %v", tc.ci, tc.mode, got)
		}
	}
}
