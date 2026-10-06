//go:build e2e

package gitlabe2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestProtectedBranch: a target protects touchmark/* with a wildcard rule,
// which a writer with Developer cannot read beforehand (the protected
// branches API wants Maintainer), so the rule shows at push.
//
//   - alpha lets Maintainers alone push to touchmark/*: the first push of
//     the sync branch is refused, and the target is
//     blocked:rules:protected-branch with no write at all.
//   - beta lets Developers push but not force-push: the first run opens a
//     merge request; after a pack change touchmark rebuilds the branch on
//     the base, which needs a force push, and the target is
//     blocked:rules:protected-branch, the merge request as it was.
func TestProtectedBranch(t *testing.T) {
	e := needLive(t)
	s := newScenario(t, e)
	for name, rule := range map[string]map[string]any{
		"alpha": {"name": "touchmark/*", "push_access_level": levelMaintainer, "merge_access_level": levelMaintainer},
		"beta":  {"name": "touchmark/*", "push_access_level": levelDeveloper, "merge_access_level": levelDeveloper, "allow_force_push": false},
	} {
		e.api(e.Root).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/protected_branches", s.ids[name]), rule, nil)
	}
	// What the writer sees of the rule before it pushes.
	resp := e.api(e.Writer).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/protected_branches", s.ids["alpha"]), nil)
	finding(t, "protected-branches-writer", "the writer's (Developer) GET /projects/:id/protected_branches: HTTP %d", resp.Status)

	rep := s.distribute(0)
	s.want("the first run", rep, s.expect(map[string]string{
		"alpha": "blocked:rules:protected-branch", "beta": "opened: #1", "delta": "opened: #1",
	}))
	s.wantOps("the first run", rep, map[string][]string{
		"beta": {"push", "create-pr #1"}, "delta": {"push", "create-pr #1"},
	})
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
		if tg.Path == s.repos["beta"] {
			finding(t, "protected-branch-force", "beta, touchmark/* without force push, after a pack change: %s:%s %q", tg.Outcome, tg.Reason, tg.Warnings)
			if !strings.Contains(strings.Join(tg.Warnings, " ")+" "+tg.Reason, "protected") {
				t.Errorf("beta: %q %q does not name the protected branch", tg.Reason, tg.Warnings)
			}
		}
	}
	if after := s.mr("beta", 1); after.SHA != before.SHA || after.Description != before.Description || after.State != "opened" {
		t.Errorf("beta !1 changed although its push was refused: %s at %s (was %s), body changed %v",
			after.State, after.SHA, before.SHA, after.Description != before.Description)
	}
	s.scanPlatform()
}
