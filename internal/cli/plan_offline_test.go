package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// TestPlanOffline: a hub pull request without any read secret, as
// Dependabot's and a fork's get, is planned offline: no
// provider is reached, the report shows the hub's side and warns, and the
// exit code is 0, or 3 with --strict. A read variable without its key
// (READ_APP_ID, a variable, not a secret) does not make a credential. Off a
// pull request, and in one where another provider has its secret, a missing
// read credential stays an error.
func TestPlanOffline(t *testing.T) {
	prepare := func(t *testing.T) (*repo, *planWorld) {
		h := planHub(t)
		h.git("checkout", "-q", "-b", "feature")
		h.write("packs/python/docs/python.md", text("python guidelines v2"))
		h.commit("the pull request")
		w := newPlanWorld(t)
		w.install()
		return h, w
	}
	noRead := func(t *testing.T) map[string]string {
		vars := actionsEnv(t)
		delete(vars, "TOUCHMARK_GH_READ_TOKEN")
		delete(vars, "TOUCHMARK_CORP_READ_TOKEN")
		return vars
	}
	offline := func(t *testing.T, w *planWorld, rep report.Delivery) {
		t.Helper()
		if len(w.built) != 0 {
			t.Errorf("drivers built: %+v, want none", w.built)
		}
		if !slices.ContainsFunc(rep.Warnings, func(s string) bool { return strings.HasPrefix(s, "offline plan:") }) {
			t.Errorf("warnings %q lack the offline plan's", rep.Warnings)
		}
		if len(rep.Targets) != 0 {
			t.Errorf("targets %+v, want none", rep.Targets)
		}
		if len(rep.Providers) != 2 {
			t.Fatalf("providers %+v, want gh and corp", rep.Providers)
		}
		for _, p := range rep.Providers {
			if p.Error != "" || p.ResolveComplete || p.Reader != "" {
				t.Errorf("provider %+v: want no error, no reader and an incomplete resolve", p)
			}
		}
		if rep.Scope == nil || !slices.Equal(rep.Scope.Packs, []string{"python"}) {
			t.Errorf("scope %+v, want the pack python", rep.Scope)
		}
	}

	t.Run("dependabot", func(t *testing.T) {
		h, w := prepare(t)
		vars := noRead(t)
		vars["GITHUB_ACTOR"] = "dependabot[bot]"
		vars["TOUCHMARK_GH_READ_APP_ID"] = "1234" // a variable, not a secret
		res := planRun(t, h, vars, exitOK, "--format", "json")
		offline(t, w, decodeDelivery(t, res.stdout))
		if strings.Contains(res.stderr, "no read credential: set") {
			t.Errorf("stderr: %s", res.stderr)
		}
	})
	t.Run("strict", func(t *testing.T) {
		h, w := prepare(t)
		res := planRun(t, h, noRead(t), exitStrict, "--format", "json", "--strict")
		offline(t, w, decodeDelivery(t, res.stdout))
	})
	t.Run("text", func(t *testing.T) {
		h, _ := prepare(t)
		res := planRun(t, h, noRead(t), exitOK)
		for _, want := range []string{"offline plan: this hub pull request has no read credential", "provider gh: not checked without credentials"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
			}
		}
	})
	t.Run("not a pull request", func(t *testing.T) {
		h, _ := prepare(t)
		h.git("checkout", "-q", "master")
		vars := noRead(t)
		vars["GITHUB_EVENT_NAME"] = "schedule"
		vars["GITHUB_REF"] = "refs/heads/master"
		vars["GITHUB_REF_NAME"] = "master"
		res := planRun(t, h, vars, exitUsage)
		if !strings.Contains(res.stderr, "no read credential") {
			t.Errorf("stderr: %s", res.stderr)
		}
	})
	t.Run("one provider has its secret", func(t *testing.T) {
		h, _ := prepare(t)
		vars := noRead(t)
		vars["TOUCHMARK_GH_READ_TOKEN"] = ghReadToken
		res := planRun(t, h, vars, exitUsage)
		if !strings.Contains(res.stderr, "provider corp: no read credential") {
			t.Errorf("stderr: %s", res.stderr)
		}
	})
}

// TestPlanGitVersion: plan reads the targets with the git distribute needs,
// and refuses an older one with exit 2 before it reads anything; an offline
// plan reads no target and runs on the git of the local commands.
func TestPlanGitVersion(t *testing.T) {
	old := gitVersion
	t.Cleanup(func() { gitVersion = old })
	gitVersion = func(context.Context) ([3]int, error) { return [3]int{2, 44, 9}, nil }
	prepare := func(t *testing.T) (*repo, *planWorld) {
		h := planHub(t)
		h.git("checkout", "-q", "-b", "feature")
		h.write("packs/python/docs/python.md", text("python guidelines v2"))
		h.commit("the pull request")
		w := newPlanWorld(t)
		w.install()
		planSnapshots = nil // plan's own git source
		return h, w
	}
	t.Run("online", func(t *testing.T) {
		h, w := prepare(t)
		res := planRun(t, h, actionsEnv(t), exitUsage)
		if want := "git 2.44.9 is too old: plan needs git 2.45.0 or newer"; !strings.Contains(res.stderr, want) {
			t.Errorf("stderr lacks %q: %s", want, res.stderr)
		}
		if len(w.built) != 0 {
			t.Errorf("drivers built before the refusal: %+v", w.built)
		}
	})
	t.Run("offline", func(t *testing.T) {
		h, _ := prepare(t)
		vars := actionsEnv(t)
		delete(vars, "TOUCHMARK_GH_READ_TOKEN")
		delete(vars, "TOUCHMARK_CORP_READ_TOKEN")
		res := planRun(t, h, vars, exitOK, "--format", "json")
		if rep := decodeDelivery(t, res.stdout); len(rep.Targets) != 0 {
			t.Errorf("targets %+v, want none", rep.Targets)
		}
	})
	t.Run("distribute", func(t *testing.T) {
		h, _ := prepare(t)
		h.git("checkout", "-q", "master")
		res := runWith(t, localEnv(), "distribute", "--hub", h.dir, "--hub-fp", "github.com/712345678", "--dry-run")
		if want := "git 2.44.9 is too old: distribute needs git 2.45.0 or newer"; res.code != exitUsage || !strings.Contains(res.stderr, want) {
			t.Errorf("exit %d, want %d and %q: %s", res.code, exitUsage, want, res.stderr)
		}
	})
}
