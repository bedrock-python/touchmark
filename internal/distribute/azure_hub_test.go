package distribute

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/report"
)

// azDeploy is the deployment job of a hub on Azure Pipelines: its default
// branch (read by the hub channel), in the environment touchmark-distribute.
var azDeploy = hubch.Context{CI: hubch.AzurePipelines, Host: "dev.azure.com", RepoID: "0b7e5a2c-9d4f-4e1b-8a3c-6f5d2e1c0b9a",
	DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "push", Environment: "touchmark-distribute"}

// azChecked is a hub that states the Branch control check of its variable
// group.
func azChecked() *config.Hub {
	return hubWithIsolation("platform", "Azure: a Branch control check admits refs/heads/main only to touchmark-distribute")
}

// The guards of distribute and doctor on Azure Pipelines: a hub in Azure
// Repos, the default branch, no pull request, the environment
// touchmark-distribute, and under platform the hub's statement of the
// check.
func TestDistributeGuardAzure(t *testing.T) {
	with := func(edit func(*hubch.Context)) hubch.Context {
		c := azDeploy
		edit(&c)
		return c
	}
	for _, tc := range []struct {
		name string
		c    hubch.Context
		hub  *config.Hub
		want []string
	}{
		{"checked", azDeploy, azChecked(), nil},
		{"none", azDeploy, hubWithIsolation("none", "no checks yet"), nil},
		{"external", azDeploy, hubWithIsolation("external", ""), nil},
		{"platform without a statement", azDeploy, hubWithIsolation("platform", ""), []string{"Branch control check", "security.reason"}},
		{"the default isolation", azDeploy, &config.Hub{ID: "acme-eng"}, []string{"Branch control check"}},
		{"no environment", with(func(c *hubch.Context) { c.Environment = "" }), azChecked(), []string{"deployment job to the environment touchmark-distribute"}},
		{"another environment", with(func(c *hubch.Context) { c.Environment = "production" }), azChecked(), []string{"ENVIRONMENT_NAME"}},
		{"pull request", with(func(c *hubch.Context) { c.Event, c.RefName, c.RefIsBranch = "pull_request", "feature", false }), azChecked(),
			[]string{"does not run for the pull_request event", "feature, which is not a branch"}},
		{"other branch", with(func(c *hubch.Context) { c.RefName = "feature" }), azChecked(), []string{"default branch main, not on feature"}},
		{"tag", with(func(c *hubch.Context) { c.RefName, c.RefIsBranch = "v1", false }), azChecked(), []string{"v1, which is not a branch"}},
		{"default branch unread", with(func(c *hubch.Context) { c.DefaultBranch = "" }), azChecked(),
			[]string{"Azure Pipelines names none", "SYSTEM_ACCESSTOKEN"}},
		// A build of a repository outside Azure Repos has no hub.
		{"no hub in Azure Repos", hubch.Context{CI: hubch.AzurePipelines}, azChecked(), []string{"only for a hub in Azure Repos"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := DistributeGuard(t.Context(), GuardInput{Context: tc.c, Hub: tc.hub, Getenv: guardEnv{}.get, GitVersion: gitx.DeliveryMinVersion})
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
		})
	}
	if err := DoctorGuard(with(func(c *hubch.Context) { c.Environment = "" }), guardEnv{}.get); err == nil || !strings.Contains(err.Error(), "doctor needs a deployment job") {
		t.Errorf("doctor without the environment: %v", err)
	}
	if err := DoctorGuard(azDeploy, guardEnv{}.get); err != nil {
		t.Errorf("doctor in the environment: %v", err)
	}
}

func TestIsolationChecksAzure(t *testing.T) {
	status := func(h *config.Hub) (report.CheckStatus, string) {
		cs := IsolationChecks(IsolationInput{Context: azDeploy, Hub: h})
		if len(cs) != 1 {
			t.Fatalf("checks %+v", cs)
		}
		return cs[0].Status, cs[0].Detail
	}
	if s, d := status(hubWithIsolation("platform", "")); s != report.StatusFail || !strings.Contains(d, "Branch control check") {
		t.Errorf("platform without a statement: %s %s", s, d)
	}
	if s, d := status(azChecked()); s != report.StatusUnknown || !strings.Contains(d, "the hub states: Azure: a Branch control") {
		t.Errorf("checked: %s %s", s, d)
	}
	if s, _ := status(hubWithIsolation("none", "no checks yet")); s != report.StatusWarn {
		t.Errorf("none: %s", s)
	}
}

func TestKeyLocationAzure(t *testing.T) {
	mainOnly := hubch.BranchCheck{Allowed: []string{"refs/heads/main"}, Protection: true}
	store := func(groups ...hubch.VariableGroup) hubch.KeyStore {
		return hubch.KeyStore{Platform: "azure-devops", DefaultBranch: "main", VariableGroups: groups, Secrets: []hubch.Secret{
			{Name: "TOUCHMARK_READ_TOKEN", Where: "pipeline", Group: "hub", Masked: true},
			{Name: "TOUCHMARK_WRITE_TOKEN", Where: "variable group", Group: "touchmark-distribute", Masked: true},
		}}
	}
	check := func(ks hubch.KeyStore, hub *config.Hub, status report.CheckStatus, sub string) {
		t.Helper()
		lines := keyChecks(KeyLocationChecks(ks, nil, hub), "key-location")
		if !hasCheck(lines, status, sub) {
			t.Errorf("no %s check with %q in:\n%s", status, sub, strings.Join(lines, "\n"))
		}
	}
	group := func(checks ...hubch.BranchCheck) hubch.VariableGroup {
		return hubch.VariableGroup{ID: 5, Name: "touchmark-distribute", BranchChecks: checks}
	}

	ok := store(group(mainOnly))
	check(ok, azChecked(), report.StatusOK, "admits the default branch main only, and verifies its protection")
	if lines := keyChecks(KeyLocationChecks(ok, nil, azChecked()), "key-location"); len(lines) != 1 {
		t.Errorf("a well kept key: %d checks:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// One check that admits main alone is enough: a stage passes only when
	// every check does.
	check(store(group(hubch.BranchCheck{Allowed: []string{"refs/heads/*"}}, mainOnly)), azChecked(), report.StatusOK, "admits the default branch main only")
	check(store(group()), azChecked(), report.StatusFail, "has no Branch control check")
	check(store(group(hubch.BranchCheck{Allowed: []string{"refs/heads/main", "refs/heads/release/*"}, Protection: true})), azChecked(),
		report.StatusFail, "admit refs/heads/release/* besides the default branch main")
	check(store(group(hubch.BranchCheck{Allowed: []string{"refs/heads/main"}})), azChecked(), report.StatusWarn, "none verifies branch protection")
	// A bare branch name never matches the run's full ref: no run passes.
	check(store(group(hubch.BranchCheck{Allowed: []string{"main"}, Protection: true})), azChecked(), report.StatusWarn, "without refs/heads/")
	// A check whose branches touchmark could not read is not taken for one
	// that admits the default branch alone.
	check(store(group(hubch.BranchCheck{Protection: true})), azChecked(), report.StatusUnknown, "lists no branch that touchmark can read")
	check(store(group(hubch.BranchCheck{Allowed: []string{"refs/heads/main"}, Protection: true, AllowUnknown: true})), azChecked(),
		report.StatusWarn, "lets an unknown status pass")
	check(store(hubch.VariableGroup{Name: "touchmark-distribute", ChecksUnread: "the checks of variable group touchmark-distribute: HTTP 403"}), azChecked(),
		report.StatusUnknown, "its checks could not be read")
	open := group(mainOnly)
	open.AllPipelines = true
	check(store(open), azChecked(), report.StatusWarn, "open to every pipeline of the project")
	// Without Verify branch protection, another repository's pipeline on its
	// own main passes the check.
	openUnverified := group(hubch.BranchCheck{Allowed: []string{"refs/heads/main"}})
	openUnverified.AllPipelines = true
	check(store(openUnverified), azChecked(), report.StatusFail, "open to every pipeline of the project")
	unreadPerms := group(mainOnly)
	unreadPerms.PermissionsUnread = "the pipeline permissions of variable group touchmark-distribute: HTTP 403"
	check(store(unreadPerms), azChecked(), report.StatusUnknown, "which pipelines may use variable group touchmark-distribute could not be read")
	check(store(group()), hubWithIsolation("none", "no checks yet"), report.StatusWarn, "accepted the risk")

	// A write key among the pipeline's variables fails; one not secret
	// warns; one in another group warns.
	leaked := store(group(mainOnly))
	leaked.Secrets = append(leaked.Secrets,
		hubch.Secret{Name: "TOUCHMARK_GH_WRITE_TOKEN", Where: "pipeline", Group: "hub", Masked: true},
		hubch.Secret{Name: "TOUCHMARK_SIGNING_KEY", Where: "variable group", Group: "shared"})
	leaked.VariableGroups = append(leaked.VariableGroups, hubch.VariableGroup{ID: 6, Name: "shared", BranchChecks: []hubch.BranchCheck{mainOnly}})
	check(leaked, azChecked(), report.StatusFail, "variable TOUCHMARK_GH_WRITE_TOKEN of pipeline hub: every run of the pipeline")
	check(leaked, azChecked(), report.StatusWarn, "variable TOUCHMARK_SIGNING_KEY of variable group shared: distribute's stage links the variable group touchmark-distribute")
	check(leaked, azChecked(), report.StatusWarn, "TOUCHMARK_SIGNING_KEY of variable group shared is not secret")

	// No group touchmark-distribute fails; groups that could not be listed
	// are unknown.
	bare := hubch.KeyStore{Platform: "azure-devops", DefaultBranch: "main"}
	check(bare, azChecked(), report.StatusFail, "has no variable group touchmark-distribute")
	bare.VariableGroupsUnread = "the project's variable groups: HTTP 403"
	check(bare, azChecked(), report.StatusUnknown, "is not known")
}

func TestPipelineSettingsAzure(t *testing.T) {
	grade := func(st *hubch.PipelineSettings, unread string) report.DoctorCheck {
		t.Helper()
		ks := hubch.KeyStore{Platform: "azure-devops", DefaultBranch: "main", PipelineSettings: st, PipelineSettingsUnread: unread}
		for _, c := range KeyLocationChecks(ks, nil, azChecked()) {
			if c.Name == "pipeline-settings" {
				return c
			}
		}
		t.Fatal("no pipeline-settings check")
		return report.DoctorCheck{}
	}
	if c := grade(&hubch.PipelineSettings{SettableVarsLimited: true, JobScopeLimited: true, ReposProtected: true}, ""); c.Status != report.StatusOK {
		t.Errorf("all on: %+v", c)
	}
	if c := grade(&hubch.PipelineSettings{JobScopeLimited: true, ReposProtected: true}, ""); c.Status != report.StatusFail ||
		!strings.Contains(c.Detail, "BASH_ENV") {
		t.Errorf("queue-time variables: %+v", c)
	}
	if c := grade(&hubch.PipelineSettings{SettableVarsLimited: true}, ""); c.Status != report.StatusWarn ||
		!strings.Contains(c.Detail, "job authorization scope") || !strings.Contains(c.Detail, "Protect access to repositories") {
		t.Errorf("token scope: %+v", c)
	}
	if c := grade(nil, "the project's pipeline settings: HTTP 403"); c.Status != report.StatusUnknown {
		t.Errorf("unread: %+v", c)
	}
}
