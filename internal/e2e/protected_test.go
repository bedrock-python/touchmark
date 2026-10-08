//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// TestProtectedBranch: branch protection on the forge, against pushes by
// anyone but administrators. The writer reads it only for branches that
// exist (GET branches/{b}: protected, user_can_push).
//
//   - alpha protects touchmark/* after the first run opened its pull
//     request: after a pack change, a dry run and distribute block it
//     rules:protected-branch before any write, with a warning naming the
//     rule, the pull request as it was.
//   - beta protects touchmark/* before the first run: the sync branch does
//     not exist, so the protection shows at its first push, refused with
//     the same reason and nothing written.
func TestProtectedBranch(t *testing.T) {
	e := needLive(t)
	s := newScenario(t, e)
	protect := func(name string) {
		e.api(e.Admin).ok(t, http.MethodPost, "/repos/"+s.repos[name]+"/branch_protections", map[string]any{
			"rule_name": "touchmark/*", "enable_push": false,
		}, nil)
	}
	// noWrites fails the test when rep has an op on target name.
	noWrites := func(what string, rep report.Delivery, name string) {
		t.Helper()
		for _, op := range rep.Ops {
			if op.Target == "forge:"+s.repos[name] {
				t.Errorf("%s: %s got the write %s", what, name, op.Kind)
			}
		}
	}

	protect("beta")
	rep := s.distribute(0)
	s.want("the first run", rep, map[string]string{"alpha": "opened: #1", "beta": "blocked:rules:protected-branch"})
	noWrites("the first run", rep, "beta")
	if _, ok := s.branchHead("beta", s.branch); ok {
		t.Errorf("beta has %s although no one may push to it", s.branch)
	}
	for _, tg := range rep.Targets {
		if tg.Path == s.repos["beta"] {
			finding(t, "protected-branch-new", "beta, touchmark/* protected before the sync branch exists: %s:%s %q", tg.Outcome, tg.Reason, tg.Warnings)
		}
	}

	protect("alpha")
	before := s.pr("alpha", 1)
	s.packs["packs/base/AGENTS.md"] = text("base AGENTS.md v2")
	s.commitHub("base v2")
	dry := s.decode(s.run(0, map[string]string{"TOUCHMARK_FORGE_WRITE_TOKEN": s.e.Writer.Token},
		"distribute", "--dry-run", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json"))
	rep = s.distribute(0)
	for _, c := range []struct {
		what string
		rep  report.Delivery
	}{{"a dry run", dry}, {"the pack changed", rep}} {
		s.want(c.what, c.rep, map[string]string{"alpha": "blocked:rules:protected-branch #1"})
		noWrites(c.what, c.rep, "alpha")
		for _, tg := range c.rep.Targets {
			if tg.Path == s.repos["alpha"] && !strings.Contains(strings.Join(tg.Warnings, " "), "is protected (touchmark/*)") {
				t.Errorf("%s: alpha's warnings %q do not name the rule", c.what, tg.Warnings)
			}
		}
	}
	if after := s.pr("alpha", 1); after.Head.SHA != before.Head.SHA || after.Body != before.Body || after.State != "open" {
		t.Errorf("alpha #1 changed although its branch is protected: %s at %s (was %s), body changed %v",
			after.State, after.Head.SHA, before.Head.SHA, after.Body != before.Body)
	}
	s.scanPlatform()
}
