//go:build e2e

package gitlabe2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// TestProtectedBranch: targets protect touchmark/* with a wildcard rule,
// which the writer, a Developer, reads before it pushes (GET
// /projects/:id/protected_branches answers Developers), so a dry run
// predicts what a push meets.
//
//   - alpha lets Maintainers alone push to touchmark/*: a dry run and the
//     first run block it rules:protected-branch before any write, with a
//     warning naming the rule.
//   - beta lets Developers push but not force-push: the first run opens a
//     merge request; after a pack change touchmark rebuilds the branch on
//     the base, which needs a force push, and the push is refused:
//     blocked:rules:protected-branch, the merge request as it was.
//   - delta has two rules on the sync branch: touchmark/* for Maintainers
//     alone, and the branch's own name for Developers with force pushes.
//     GitLab applies the most permissive rule that matches a branch, to
//     pushes and force pushes, so delta is delivered as if unprotected.
func TestProtectedBranch(t *testing.T) {
	e := needLive(t)
	s := newScenario(t, e)
	for _, r := range []struct {
		name string
		rule map[string]any
	}{
		{"alpha", map[string]any{"name": "touchmark/*", "push_access_level": levelMaintainer, "merge_access_level": levelMaintainer}},
		{"beta", map[string]any{"name": "touchmark/*", "push_access_level": levelDeveloper, "merge_access_level": levelDeveloper, "allow_force_push": false}},
		{"delta", map[string]any{"name": "touchmark/*", "push_access_level": levelMaintainer, "merge_access_level": levelMaintainer}},
		{"delta", map[string]any{"name": s.branch, "push_access_level": levelDeveloper, "merge_access_level": levelDeveloper, "allow_force_push": true}},
	} {
		e.api(e.Root).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/protected_branches", s.ids[r.name]), r.rule, nil)
	}
	// What the writer sees of the rule before it pushes.
	resp := e.api(e.Writer).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/protected_branches", s.ids["alpha"]), nil)
	finding(t, "protected-branches-writer", "the writer's (Developer) GET /projects/:id/protected_branches: HTTP %d", resp.Status)

	alphaWarned := func(what string, rep report.Delivery) {
		t.Helper()
		for _, tg := range rep.Targets {
			if tg.Path == s.repos["alpha"] && !strings.Contains(strings.Join(tg.Warnings, " "), "is protected (touchmark/*)") {
				t.Errorf("%s: alpha's warnings %q do not name the rule", what, tg.Warnings)
			}
		}
	}
	dry := s.decode(s.run(0, map[string]string{s.envVar("WRITE_TOKEN"): s.e.Writer.Token},
		"distribute", "--dry-run", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json"))
	s.want("a dry run", dry, s.expect(map[string]string{
		"alpha": "blocked:rules:protected-branch", "beta": "opened:", "delta": "opened:",
	}))
	alphaWarned("a dry run", dry)

	rep := s.distribute(0)
	s.want("the first run", rep, s.expect(map[string]string{
		"alpha": "blocked:rules:protected-branch", "beta": "opened: #1", "delta": "opened: #1",
	}))
	s.wantOps("the first run", rep, map[string][]string{
		"beta": {"push", "create-pr #1"}, "delta": {"push", "create-pr #1"},
	})
	alphaWarned("the first run", rep)
	for _, tg := range rep.Targets {
		if tg.Path == s.repos["alpha"] {
			finding(t, "protected-branch-create", "alpha, touchmark/* pushed by Maintainers only: %s:%s %q", tg.Outcome, tg.Reason, tg.Warnings)
		}
	}
	if _, ok := e.branchHead(t, s.ids["alpha"], s.branch); ok {
		t.Errorf("alpha has %s although the rule allows Maintainers alone", s.branch)
	}

	before := s.mr("beta", 1)
	s.packs["packs/base/AGENTS.md"] = text("base AGENTS.md v2")
	s.commitHub("base v2")
	rep = s.distribute(0)
	s.want("the pack changed", rep, s.expect(map[string]string{
		"alpha": "blocked:rules:protected-branch", "beta": "blocked:rules:protected-branch #1", "delta": "updated:content #1",
	}))
	s.wantOps("the pack changed", rep, map[string][]string{"delta": {"push", "edit-pr #1"}})
	for _, tg := range rep.Targets {
		switch tg.Path {
		case s.repos["beta"]:
			finding(t, "protected-branch-force", "beta, touchmark/* without force push, after a pack change: %s:%s %q", tg.Outcome, tg.Reason, tg.Warnings)
			if !strings.Contains(strings.Join(tg.Warnings, " ")+" "+tg.Reason, "protected") {
				t.Errorf("beta: %q %q does not name the protected branch", tg.Reason, tg.Warnings)
			}
		case s.repos["delta"]:
			finding(t, "protected-branch-permissive", "delta, touchmark/* for Maintainers and %s for Developers with force pushes: %s:%s", s.branch, tg.Outcome, tg.Reason)
		}
	}
	if after := s.mr("beta", 1); after.SHA != before.SHA || after.Description != before.Description || after.State != "opened" {
		t.Errorf("beta !1 changed although its push was refused: %s at %s (was %s), body changed %v",
			after.State, after.SHA, before.SHA, after.Description != before.Description)
	}
	s.scanPlatform()
}
