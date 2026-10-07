package cli

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// assumedTargets subscribes acme/svc by name and the libraries of acme by a
// pattern, without an opt-in file.
const assumedTargets = `version: 1
defaults:
  packs: [base]
targets:
  - repo: acme/svc
    opt_in: assumed
  - org: acme
    match: [acme/lib-*]
    opt_in: assumed
`

// A target without the opt-in file that a repo: entry with opt_in: assumed
// names is opted in locally, with the packs of targets.yml; one only an org
// entry may subscribe stays not opted in, with a warning, since a local run
// cannot resolve the org.
func TestAssumedOptIn(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("targets.yml", assumedTargets)
	h.commit("subscribe acme/svc")
	tg := newRepo(t, false)
	s := newScenario(t, "assumed", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", tg.tree())

	lib := newScenario(t, "assumed", h, newRepo(t, false), "--repo", "acme/lib-x")
	rep := lib.report("status")
	if rep.Target.OptedIn || rep.Target.OptIn != report.OptInNone || len(rep.Warnings) != 1 ||
		!strings.Contains(rep.Warnings[0], "org: acme with opt_in: assumed cannot be resolved here") {
		t.Errorf("acme/lib-x: %+v, warnings %q", rep.Target, rep.Warnings)
	}
	web := newScenario(t, "assumed", h, newRepo(t, false), "--repo", "acme/web")
	if rep := web.report("status"); rep.Target.OptedIn || len(rep.Warnings) != 0 {
		t.Errorf("acme/web: %+v, warnings %q", rep.Target, rep.Warnings)
	}
	unknown := newScenario(t, "assumed", h, newPlainDir(t))
	if rep := unknown.report("status"); rep.Target.OptedIn || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "pass --repo") {
		t.Errorf("a target without a name: %+v, warnings %q", rep.Target, rep.Warnings)
	}

	// --assume-opt-in settles what the org entry leaves open: the library
	// counts as opted in, with the packs of targets.yml and no warning, and
	// apply writes them. So does a target without a name.
	libY := newRepo(t, false)
	flagged := newScenario(t, "assumed", h, libY, "--repo", "acme/lib-y", "--assume-opt-in")
	flagged.golden("status-flag.txt", flagged.text("status"))
	if rep := flagged.report("status"); !rep.Target.OptedIn || rep.Target.OptIn != report.OptInFlag || len(rep.Warnings) != 0 {
		t.Errorf("acme/lib-y with --assume-opt-in: %+v, warnings %q", rep.Target, rep.Warnings)
	}
	flagged.json("apply")
	if tree := libY.tree(); !strings.Contains(tree, "AGENTS.md") {
		t.Errorf("apply --assume-opt-in wrote no packs:\n%s", tree)
	}
	unnamed := newScenario(t, "assumed", h, newPlainDir(t), "--assume-opt-in")
	if rep := unnamed.report("status"); !rep.Target.OptedIn || rep.Target.OptIn != report.OptInFlag || len(rep.Warnings) != 0 {
		t.Errorf("a target without a name, with --assume-opt-in: %+v, warnings %q", rep.Target, rep.Warnings)
	}
	// A repo: entry that subscribes the target still says so.
	if rep := newScenario(t, "assumed", h, newRepo(t, false), "--repo", "acme/svc", "--assume-opt-in").report("status"); rep.Target.OptIn != report.OptInAssumed {
		t.Errorf("acme/svc with --assume-opt-in: opt_in %q, want %q", rep.Target.OptIn, report.OptInAssumed)
	}
}

// An opt-in file with enabled: false opts the target out, whatever
// targets.yml says: status says so, and apply writes nothing.
func TestOptedOut(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("targets.yml", assumedTargets)
	h.commit("subscribe acme/svc")
	tg := optedIn(t, "version: 1\nenabled: false\npacks: [base]\n")
	s := newScenario(t, "opted-out", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	if rep := newScenario(t, "opted-out", h, tg, "--repo", "acme/svc", "--assume-opt-in").report("status"); rep.Target.OptedIn ||
		rep.Target.OptIn != report.OptInDisabled {
		t.Errorf("enabled: false with --assume-opt-in: %+v", rep.Target)
	}
	if tree := tg.tree(); strings.Contains(tree, "AGENTS.md") {
		t.Errorf("apply wrote into a target that opted out:\n%s", tree)
	}
}

// Web URLs in targets.yml resolve against the providers of hub.yml, so the
// local commands see <provider>:<path>; a URL no provider serves fails
// check, and so does a hub whose only provider is implicit when nothing
// tells its url.
func TestTargetURLs(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("hub.yml", `version: 1
id: acme-eng
providers:
  - id: gh
    type: github
  - id: corp
    type: gitlab
    url: https://gitlab.example.com
`)
	h.write("targets.yml", `version: 1
defaults:
  packs: [base]
targets:
  - repo: https://github.com/acme/svc
    opt_in: assumed
  - group: https://gitlab.example.com/platform/
    match: [platform/**/api]
exclude:
  - https://gitlab.example.com/platform/legacy/**
`)
	h.commit("targets by URL")
	if res := runWith(t, nil, "check", "--hub", h.dir); res.code != 0 || !strings.Contains(res.stdout, "ok") {
		t.Fatalf("check: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	tg := newRepo(t, false)
	res := runWith(t, nil, "status", "--hub", h.dir, "--dir", tg.dir, "--repo", "gh:acme/svc", "--format", "json")
	if rep := decodeSync(t, res.stdout); res.code != 0 || rep.Target.OptIn != report.OptInAssumed || !rep.Target.OptedIn {
		t.Errorf("status of gh:acme/svc: exit %d, %+v\n%s", res.code, rep.Target, res.stderr)
	}

	h.write("targets.yml", "version: 1\ntargets:\n  - repo: https://bitbucket.org/acme/svc\n")
	h.commit("a URL of no provider")
	res = runWith(t, nil, "check", "--hub", h.dir)
	if res.code != exitUsage || !strings.Contains(res.stdout, "targets.yml: targets[0].repo: https://bitbucket.org/acme/svc is not under the url of any provider") {
		t.Errorf("check of a URL of no provider: exit %d\n%s", res.code, res.stdout)
	}
	res = runWith(t, nil, "status", "--hub", h.dir, "--dir", tg.dir, "--repo", "gh:acme/svc")
	if res.code != exitUsage || !strings.Contains(res.stderr, "is not under the url of any provider") {
		t.Errorf("status with a URL of no provider: exit %d\n%s", res.code, res.stderr)
	}

	// The implicit provider: the CI names its url; nothing does locally
	// without an origin remote.
	h.write("hub.yml", hubYML)
	h.write("targets.yml", "version: 1\ntargets:\n  - repo: https://gitlab.example.com/platform/api\n")
	h.commit("the implicit provider")
	res = runWith(t, nil, "check", "--hub", h.dir)
	if res.code != exitUsage || !strings.Contains(res.stdout, "its web URLs are matched with the providers' urls, which are unknown") {
		t.Errorf("check without a provider url: exit %d\n%s", res.code, res.stdout)
	}
	// In CI they resolve, with a warning that a local run cannot: an origin
	// remote names a provider on public instances only.
	ci := map[string]string{"GITLAB_CI": "true", "CI_SERVER_URL": "https://gitlab.example.com"}
	res = runWith(t, ci, "check", "--hub", h.dir)
	if res.code != 0 || !strings.Contains(res.stdout, "list the instance under providers in hub.yml") {
		t.Errorf("check in GitLab CI: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	h.write("targets.yml", "version: 1\ntargets:\n  - repo: https://gitlab.com/platform/api\n")
	h.commit("a target on gitlab.com")
	ci["CI_SERVER_URL"] = "https://gitlab.com"
	res = runWith(t, ci, "check", "--hub", h.dir)
	if res.code != 0 || strings.Contains(res.stdout, "list the instance under providers") {
		t.Errorf("check in GitLab CI on gitlab.com: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
}
