package distribute

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/report"
)

// bbDeploy is the deployment step of a hub on Bitbucket Pipelines: its
// default branch (read by the hub channel), in the deployment
// touchmark-distribute.
var bbDeploy = hubch.Context{CI: hubch.BitbucketPipelines, Host: "bitbucket.org", RepoID: "3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d",
	DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "push", Environment: "touchmark-distribute"}

// premium is a hub that states the Premium deployment restriction.
func premium() *config.Hub {
	return hubWithIsolation("platform", "Bitbucket Premium: only main may deploy to touchmark-distribute")
}

// The guards of distribute and doctor on Bitbucket Pipelines: the default
// branch, no pull request, the deployment touchmark-distribute, and under
// platform the hub's statement of the restriction.
func TestDistributeGuardBitbucket(t *testing.T) {
	with := func(edit func(*hubch.Context)) hubch.Context {
		c := bbDeploy
		edit(&c)
		return c
	}
	for _, tc := range []struct {
		name string
		c    hubch.Context
		hub  *config.Hub
		want []string
	}{
		{"premium", bbDeploy, premium(), nil},
		{"none", bbDeploy, hubWithIsolation("none", "Bitbucket Standard"), nil},
		{"external", bbDeploy, hubWithIsolation("external", ""), nil},
		// The template's hub.yml says platform and nothing else: refused, so a
		// hub on Free or Standard is not taken for isolated.
		{"platform without a statement", bbDeploy, hubWithIsolation("platform", ""), []string{"only Bitbucket Premium offers", "security.reason"}},
		{"the default isolation", bbDeploy, &config.Hub{ID: "acme-eng"}, []string{"only Bitbucket Premium offers"}},
		{"no deployment", with(func(c *hubch.Context) { c.Environment = "" }), premium(), []string{"deployment: touchmark-distribute"}},
		{"another deployment", with(func(c *hubch.Context) { c.Environment = "production" }), premium(), []string{"BITBUCKET_DEPLOYMENT_ENVIRONMENT"}},
		{"the deployment's name in another case", with(func(c *hubch.Context) { c.Environment = "Touchmark-Distribute" }), premium(), nil},
		{"pull request", with(func(c *hubch.Context) { c.Event, c.RefName, c.RefIsBranch = "pull_request", "feature", false }), premium(),
			[]string{"does not run for the pull_request event", "feature, which is not a branch"}},
		{"other branch", with(func(c *hubch.Context) { c.RefName = "feature" }), premium(), []string{"default branch main, not on feature"}},
		{"tag", with(func(c *hubch.Context) { c.RefName, c.RefIsBranch = "v1", false }), premium(), []string{"v1, which is not a branch"}},
		{"default branch unread", with(func(c *hubch.Context) { c.DefaultBranch = "" }), premium(),
			[]string{"Bitbucket Pipelines names none", "TOUCHMARK_PIPELINES_TOKEN"}},
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
	// doctor holds the write key too: the same context guards.
	if err := DoctorGuard(with(func(c *hubch.Context) { c.Environment = "" }), guardEnv{}.get); err == nil || !strings.Contains(err.Error(), "doctor needs a step with deployment") {
		t.Errorf("doctor without the deployment: %v", err)
	}
	if err := DoctorGuard(bbDeploy, guardEnv{}.get); err != nil {
		t.Errorf("doctor in the deployment: %v", err)
	}
}

// The probe of a Bitbucket step sees what the step's variables give it: the
// repository variables (the read key, the hub's access token) pass, a write
// key among them fails, as on the other platforms.
func TestProbeBitbucket(t *testing.T) {
	step := guardEnv{"BITBUCKET_BUILD_NUMBER": "7", "TOUCHMARK_READ_TOKEN": "r-0123456789", "TOUCHMARK_PIPELINES_TOKEN": "h-0123456789"}
	environ := func(e guardEnv) func() []string {
		return func() []string {
			var out []string
			for k, v := range e {
				out = append(out, k+"="+v)
			}
			return out
		}
	}
	if res := Probe(step.get, environ(step)); len(res.Exposed) != 0 {
		t.Errorf("a step without the deployment: %q", res.Exposed)
	}
	step["TOUCHMARK_WRITE_TOKEN"] = "w-0123456789"
	if res := Probe(step.get, environ(step)); len(res.Exposed) != 1 || res.Exposed[0] != "TOUCHMARK_WRITE_TOKEN" {
		t.Errorf("a write key in a repository variable: %q", res.Exposed)
	}
}

func TestIsolationChecksBitbucket(t *testing.T) {
	status := func(h *config.Hub) (report.CheckStatus, string) {
		cs := IsolationChecks(IsolationInput{Context: bbDeploy, Hub: h})
		if len(cs) != 1 {
			t.Fatalf("checks %+v", cs)
		}
		return cs[0].Status, cs[0].Detail
	}
	if s, d := status(hubWithIsolation("platform", "")); s != report.StatusFail || !strings.Contains(d, "only Bitbucket Premium offers") {
		t.Errorf("platform without a statement: %s %s", s, d)
	}
	if s, d := status(premium()); s != report.StatusUnknown || !strings.Contains(d, "the hub states: Bitbucket Premium") {
		t.Errorf("premium: %s %s", s, d)
	}
	if s, _ := status(hubWithIsolation("none", "Standard plan")); s != report.StatusWarn {
		t.Errorf("none: %s", s)
	}
}

func TestKeyLocationBitbucket(t *testing.T) {
	envs := []hubch.NamedEnvironment{{Name: "touchmark-distribute"}, {Name: "pages"}}
	ks := hubch.KeyStore{Platform: "bitbucket", DefaultBranch: "main", Environments: envs, Secrets: []hubch.Secret{
		{Name: "TOUCHMARK_READ_TOKEN", Where: "repository", Masked: true},
		{Name: "TOUCHMARK_WRITE_TOKEN", Where: "repository", Masked: true},
		{Name: "TOUCHMARK_SIGNING_KEY", Where: "workspace", Masked: true},
		{Name: "TOUCHMARK_WRITE_TOKEN", Where: "environment", Environment: "touchmark-distribute", Masked: true},
		{Name: "TOUCHMARK_BB_WRITE_TOKEN", Where: "environment", Environment: "pages"},
	}}
	lines := keyChecks(KeyLocationChecks(ks, nil, premium()), "key-location")
	for _, want := range []struct {
		status report.CheckStatus
		sub    string
	}{
		{report.StatusFail, "repository variable TOUCHMARK_WRITE_TOKEN: the pipelines of every branch"},
		{report.StatusFail, "workspace variable TOUCHMARK_SIGNING_KEY"},
		{report.StatusUnknown, "deployment variable TOUCHMARK_WRITE_TOKEN of environment touchmark-distribute: Bitbucket's API does not show"},
		{report.StatusWarn, "deployment variable TOUCHMARK_BB_WRITE_TOKEN of environment pages: distribute's step deploys to touchmark-distribute"},
		{report.StatusWarn, "TOUCHMARK_BB_WRITE_TOKEN of environment pages is not secured"},
	} {
		if !hasCheck(lines, want.status, want.sub) {
			t.Errorf("no %s check with %q in:\n%s", want.status, want.sub, strings.Join(lines, "\n"))
		}
	}
	if len(lines) != 5 {
		t.Errorf("%d checks:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// Admins only: the step of anyone else pauses.
	ks.Environments[0].AdminOnly = true
	if lines := keyChecks(KeyLocationChecks(ks, nil, premium()), "key-location"); !hasCheck(lines, report.StatusOK, "only admins may deploy there") {
		t.Errorf("admin only:\n%s", strings.Join(lines, "\n"))
	}
	// none: the risk is the hub's.
	ks.Environments[0].AdminOnly = false
	if lines := keyChecks(KeyLocationChecks(ks, nil, hubWithIsolation("none", "Standard")), "key-location"); !hasCheck(lines, report.StatusWarn, "accepted the risk") {
		t.Errorf("none:\n%s", strings.Join(lines, "\n"))
	}
	// No environment touchmark-distribute fails; one that could not be
	// listed is unknown.
	bare := hubch.KeyStore{Platform: "bitbucket", DefaultBranch: "main"}
	if lines := keyChecks(KeyLocationChecks(bare, nil, premium()), "key-location"); !hasCheck(lines, report.StatusFail, "the hub has no environment touchmark-distribute") {
		t.Errorf("no environment:\n%s", strings.Join(lines, "\n"))
	}
	bare.EnvironmentsUnread = "the repository's deployment environments: HTTP 403"
	if lines := keyChecks(KeyLocationChecks(bare, nil, premium()), "key-location"); !hasCheck(lines, report.StatusUnknown, "is not known") {
		t.Errorf("unread environments:\n%s", strings.Join(lines, "\n"))
	}
}
