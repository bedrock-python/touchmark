package cli

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// In a hub pull request the sensitive paths are the union of the default
// branch's sensitive_paths and the pull request's own (threat T2 of
// docs/project/threat-model.md): a pull request that drops a pattern while
// it changes a file under it still gets the file marked SENSITIVE in its
// plan, and a pattern it adds counts at once. A run of the default branch
// reads its own list only.
func TestPlanSensitivePathsUnion(t *testing.T) {
	needDistributeGit(t)
	withSensitive := func(patterns string) string {
		return distHubYML + "sensitive_paths: [" + patterns + "]\n"
	}
	cases := []struct {
		name string
		// base is hub.yml on the default branch, branch on the pull
		// request's; the pull request also changes docs/guide.md and adds
		// docs/new.md to the pack base.
		base, branch string
		// sensitive are the changed paths the plan marks SENSITIVE.
		sensitive map[string]bool
	}{
		{
			name: "pull request drops the pattern", base: withSensitive(`"docs/guide.md"`), branch: distHubYML,
			sensitive: map[string]bool{"docs/guide.md": true, "docs/new.md": false},
		},
		{
			name: "pull request adds a pattern", base: distHubYML, branch: withSensitive(`"docs/new.md"`),
			sensitive: map[string]bool{"docs/guide.md": false, "docs/new.md": true},
		},
		{
			name: "both", base: withSensitive(`"docs/guide.md"`), branch: withSensitive(`"docs/new.md"`),
			sensitive: map[string]bool{"docs/guide.md": true, "docs/new.md": true},
		},
		{
			name: "neither", base: distHubYML, branch: distHubYML,
			sensitive: map[string]bool{"docs/guide.md": false, "docs/new.md": false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := distHub(t)
			h.write("hub.yml", tc.base)
			h.commit("the default branch's sensitive paths")
			h.git("checkout", "-q", "-b", "feature")
			h.write("hub.yml", tc.branch)
			h.write("packs/base/docs/guide.md", text("base guide v2"))
			h.write("packs/base/docs/new.md", text("a new page"))
			h.commit("the pull request")
			w := newDistWorld(t)
			w.installPlan()
			vars := sensitivePlanEnv(t)
			rep := decodeDelivery(t, planRun(t, h, vars, exitOK, "--format", "json").stdout)
			checkSensitive(t, rep, tc.sensitive)
			// The text output marks them too.
			out := planRun(t, h, vars, exitOK).stdout
			for p, want := range tc.sensitive {
				marked := false
				for _, line := range strings.Split(out, "\n") {
					if f := strings.Fields(line); len(f) >= 3 && f[1] == p && f[2] == "SENSITIVE" {
						marked = true
					}
				}
				if marked != want {
					t.Errorf("text output marks %s %v, want %v:\n%s", p, marked, want, out)
				}
			}
		})
	}

	// A run of the default branch itself (a push) reads its own list:
	// there is nothing else to trust.
	t.Run("default branch", func(t *testing.T) {
		h := distHub(t)
		h.write("hub.yml", withSensitive(`"docs/new.md"`))
		h.write("packs/base/docs/new.md", text("a new page"))
		h.commit("the default branch")
		w := newDistWorld(t)
		w.installPlan()
		vars := sensitivePlanEnv(t)
		vars["GITHUB_EVENT_NAME"], vars["GITHUB_REF"], vars["GITHUB_REF_NAME"] = "push", "refs/heads/master", "master"
		rep := decodeDelivery(t, planRun(t, h, vars, exitOK, "--format", "json").stdout)
		checkSensitive(t, rep, map[string]bool{"docs/new.md": true})
	})
}

// sensitivePlanEnv is the job of a hub pull request (#41) whose provider is
// the distribute world's: its read token, and a hub API on a closed
// loopback port, so that the hub channel fails at once and the scope comes
// from the clone's default branch.
func sensitivePlanEnv(t *testing.T) map[string]string {
	t.Helper()
	vars := actionsEnv(t)
	delete(vars, "TOUCHMARK_CORP_READ_TOKEN")
	vars["TOUCHMARK_GH_READ_TOKEN"] = distReadToken
	vars["GITHUB_API_URL"] = "http://127.0.0.1:1"
	return vars
}

// checkSensitive checks that the plan's paths hold every path of want,
// marked sensitive as want says, and no other sensitive path.
func checkSensitive(t *testing.T, rep report.Delivery, want map[string]bool) {
	t.Helper()
	got := map[string]bool{}
	for _, c := range rep.Paths {
		got[c.Path] = got[c.Path] || c.Sensitive
		if _, ok := want[c.Path]; !ok && c.Sensitive {
			t.Errorf("%s %s is marked sensitive", c.Action, c.Path)
		}
	}
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("the plan's paths lack %s: %+v", p, rep.Paths)
		} else if g != w {
			t.Errorf("%s sensitive %v, want %v", p, g, w)
		}
	}
}
