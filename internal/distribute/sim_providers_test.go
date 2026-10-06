package distribute

import (
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
)

// Two providers write at once, one queue each: a GitHub and a GitLab
// instance, each with a target, in one hub. Their pull requests open and
// update in the same runs, each on its own platform, the report sums the
// writes per provider, and a run after writes nothing; the Linux job runs it
// under the race detector.
func TestTwoProviders(t *testing.T) {
	t.Parallel()
	gh := newSimWorld(t, fake.GitHub, "api")
	gl := newSimWorld(t, fake.GitLab, "web")
	const hubYML = "version: 1\nid: acme-eng\nproviders:\n" +
		"  - id: gh\n    type: github\n    writer: acme-write[bot]\n" +
		"  - id: corp\n    type: gitlab\n    url: https://gitlab.example.com\n    writer: acme-write[bot]\n"
	const targetsYML = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\n    provider: gh\n  - org: acme\n    provider: corp\n"
	deps := func() Deps {
		d := gh.deps(ModeDistribute)
		hub, _, err := config.ParseHub([]byte(hubYML))
		if err != nil {
			t.Fatal(err)
		}
		targets, _, err := config.ParseTargets([]byte(targetsYML))
		if err != nil {
			t.Fatal(err)
		}
		rps, err := hub.ResolveProviders(func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		d.Hub, d.Targets = hub, targets
		d.Providers = []Provider{{Config: rps[0], Writer: gh.p.Writer(gh.writer)}, {Config: rps[1], Writer: gl.p.Writer(gl.writer)}}
		d.Write.CanWorkflows = map[string]bool{"gh": true, "corp": true}
		return d
	}
	run := func(what string, want report.Outcome) {
		t.Helper()
		gh.p.ResetCalls()
		gl.p.ResetCalls()
		rep, err := Run(t.Context(), deps(), ModeDistribute)
		if err != nil {
			t.Fatal(err)
		}
		checkReport(t, rep)
		if len(rep.Targets) != 2 {
			t.Fatalf("%s: %s", what, outcomes(rep))
		}
		for _, tg := range rep.Targets {
			if tg.Outcome != want {
				t.Errorf("%s: %s:%s is %s:%s (%q)", what, tg.Provider, tg.Path, tg.Outcome, tg.Reason, tg.Warnings)
			}
		}
		if len(gh.p.Writes()) == 0 || len(gl.p.Writes()) == 0 || rep.Cost["gh"] == 0 || rep.Cost["corp"] == 0 {
			t.Errorf("%s: writes github %q, gitlab %q, cost %v", what, gh.p.Writes(), gl.p.Writes(), rep.Cost)
		}
		for _, w := range []*simWorld{gh, gl} {
			if v := w.p.Violations(); len(v) > 0 {
				t.Errorf("%s: forbidden transitions: %q", what, v)
			}
		}
		gh.p.ResetCalls()
		gl.p.ResetCalls()
		if _, err := Run(t.Context(), deps(), ModeDistribute); err != nil {
			t.Fatal(err)
		}
		if len(gh.p.Writes()) > 0 || len(gl.p.Writes()) > 0 {
			t.Errorf("%s: a second run wrote %q, %q", what, gh.p.Writes(), gl.p.Writes())
		}
	}
	run("the first run", report.OutcomeOpened)
	gh.ship("AGENTS.md", 2)
	gh.hubChanged()
	run("a pack change", report.OutcomeUpdated)
	if pr := gl.p.PR(gl.target("web").repo.ID, 1); pr.HeadSHA != gl.p.Branch(gl.target("web").repo.ID, "touchmark/acme-eng") || pr.HeadSHA == "" {
		t.Errorf("the GitLab merge request: head %s", pr.HeadSHA)
	}
}
