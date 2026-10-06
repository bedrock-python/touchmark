package distribute

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// A pack that brings a Gitea or Forgejo target its first .gitea/workflows
// turns off the target's .github/workflows there: the pull request's body
// says so, and plan warns about it on the target's line too.
// A target without .github/workflows, one with .gitea/workflows already,
// and a GitHub target get no warning.
func TestPlanWarnsGiteaWorkflows(t *testing.T) {
	t.Parallel()
	workflow := version(".gitea/workflows/lint.yml", 1)
	for _, tc := range []struct {
		name, typ string
		files     []string
		warns     bool
	}{
		{"gitea with github workflows", "gitea", []string{".github/workflows/ci.yml", "on: push\n"}, true},
		{"forgejo with github workflows", "forgejo", []string{".github/workflows/ci.yml", "on: push\n"}, true},
		{"gitea without workflows", "gitea", nil, false},
		{"gitea with its own workflows", "gitea", []string{".github/workflows/ci.yml", "on: push\n", ".gitea/workflows/ci.yml", "on: push\n"}, false},
		{"github", "github", []string{".github/workflows/ci.yml", "on: push\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flavor := map[string]fake.Flavor{"gitea": fake.Gitea, "forgejo": fake.Forgejo, "github": fake.GitHub}[tc.typ]
			w := newGitWorld(t, fake.WithFlavor(flavor))
			if tc.typ != "github" {
				w.hubYML = strings.Replace(defaultHubYML, "    type: github\n", "    type: "+tc.typ+"\n    url: https://github.com\n", 1)
			}
			w.pack("AGENTS.md", agentsV2, ".gitea/workflows/lint.yml", workflow)
			w.optedIn("acme/x", nil, tc.files...)
			rep := w.both(nil)
			tg := want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 0)
			if got := hasWarning(tg.Warnings, giteaWorkflowsWarning); got != tc.warns {
				t.Errorf("warns %v, want %v: %q", got, tc.warns, tg.Warnings)
			}
		})
	}
}
