package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
)

const (
	hubYML    = "version: 1\nid: acme-eng\n"
	optInBase = "version: 1\npacks: [base]\n"
	optInFile = ".engineering-assets.yml"
)

// baseHub returns a hub whose pack base ships three files.
func baseHub(t *testing.T) *repo {
	t.Helper()
	h := newHub(t)
	h.write("hub.yml", hubYML)
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v1"))
	h.write("packs/base/.github/pull_request_template.md", text("base PR template v1"))
	h.write("packs/base/docs/guide.md", text("base guide v1"))
	h.commit("base v1")
	return h
}

// optedIn returns a target repository holding only the opt-in file.
func optedIn(t *testing.T, optIn string) *repo {
	t.Helper()
	tg := newRepo(t, false)
	tg.put(optInFile, optIn)
	return tg
}

// Scenario 1: an empty target opts in; apply creates every file and a second
// apply has nothing to do.
func TestFreshOptIn(t *testing.T) {
	t.Parallel()
	s := newScenario(t, "fresh-opt-in", baseHub(t), optedIn(t, optInBase))
	s.golden("status.json", s.json("status"))
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", s.target.tree())
	s.golden("apply-again.json", s.json("apply"))
	s.golden("status-after.txt", s.text("status"))
}

// Scenario 2: a target already in sync gets no changes.
func TestAlreadyInSync(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := optedIn(t, optInBase)
	for _, p := range []string{"AGENTS.md", ".github/pull_request_template.md", "docs/guide.md"} {
		tg.put(p, h.read("packs/base/"+p))
	}
	s := newScenario(t, "in-sync", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 3: a file the target edited is local and left alone, until
// --adopt takes it back.
func TestLocalEditAndAdopt(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := optedIn(t, optInBase)
	own := text("our own AGENTS.md")
	tg.put("AGENTS.md", own)
	s := newScenario(t, "local-edit", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent("AGENTS.md", own)
	s.golden("apply-adopt.json", s.json("apply", "--adopt", "AGENTS.md"))
	s.wantContent("AGENTS.md", text("base AGENTS.md v1"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 4: a target holding an older committed version is updated.
func TestOutdated(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v2"))
	h.commit("base v2")
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v3"))
	h.commit("base v3")
	tg := optedIn(t, optInBase)
	tg.put("AGENTS.md", text("base AGENTS.md v2"))
	s := newScenario(t, "outdated", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent("AGENTS.md", text("base AGENTS.md v3"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 5: nothing under an ignored subtree is touched; its paths are
// reported ignored.
func TestIgnoreSubtree(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/.claude/settings.json", text("base settings v1"))
	h.write("packs/base/.claude/skills/review/SKILL.md", text("base review skill v1"))
	h.commit("claude files")
	tg := optedIn(t, "version: 1\npacks: [base]\nignore: ['.claude/**']\n")
	own := text("our settings")
	tg.put(".claude/settings.json", own)
	s := newScenario(t, "ignore-subtree", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent(".claude/settings.json", own)
	if tg.exists(".claude/skills") {
		t.Error("apply wrote under the ignored .claude/")
	}
	s.golden("tree.txt", tg.tree())
}

// Scenario 6: when two packs ship a path, the later one wins; a target
// holding the earlier pack's version is updated to the later one.
func TestPackOverride(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/.claude/settings.json", text("base settings v1"))
	h.write("packs/python-service/.claude/settings.json", text("python-service settings v1"))
	h.write("packs/python-service/ruff.toml", text("# python-service ruff.toml v1"))
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - repo: acme/svc\n    packs: [python-service]\n")
	h.commit("python-service")
	tg := optedIn(t, "version: 1\n")
	tg.put(".claude/settings.json", text("base settings v1"))
	s := newScenario(t, "pack-override", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent(".claude/settings.json", text("python-service settings v1"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 7: a file a pack stopped shipping is deleted when the target
// holds a shipped version, with the directories it leaves empty; a modified
// copy is kept.
func TestRetire(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/tools/legacy/run.md", text("base legacy run v1"))
	h.write("packs/base/scripts/old.md", text("base old script v1"))
	h.write("packs/base/NOTES.md", text("base notes v1"))
	h.commit("more files")
	h.rm("packs/base/tools")
	h.rm("packs/base/scripts")
	h.rm("packs/base/NOTES.md")
	h.commit("retire")
	tg := optedIn(t, optInBase)
	tg.put("tools/legacy/run.md", text("base legacy run v1"))
	tg.put("tools/mine.md", "the target's own file\n")
	tg.put("scripts/old.md", text("base old script v1"))
	tg.put("NOTES.md", text("our notes"))
	s := newScenario(t, "retire", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	for _, p := range []string{"tools/legacy", "scripts"} {
		if tg.exists(p) {
			t.Errorf("%s was not removed", p)
		}
	}
	s.wantContent("NOTES.md", text("our notes"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 8: files a git target committed with LF and checked out as CRLF
// (core.autocrlf=true, on any OS) are current; an outdated CRLF copy is
// updated.
func TestCRLF(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := newRepo(t, false)
	tg.write(optInFile, optInBase)
	for _, p := range []string{"AGENTS.md", ".github/pull_request_template.md", "docs/guide.md"} {
		tg.write(p, h.read("packs/base/"+p))
	}
	tg.commit("sync")
	tg.git("config", "core.autocrlf", "true")
	for _, p := range []string{"AGENTS.md", ".github", "docs"} {
		if err := os.RemoveAll(tg.abs(p)); err != nil {
			t.Fatal(err)
		}
	}
	tg.git("checkout", "--", ".")
	if got := tg.read("AGENTS.md"); !strings.Contains(got, "\r\n") {
		t.Fatalf("AGENTS.md was not checked out with CRLF: %q", got)
	}
	s := newScenario(t, "crlf", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))

	h.write("packs/base/AGENTS.md", text("base AGENTS.md v2"))
	h.commit("base v2")
	s.golden("status-outdated.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent("AGENTS.md", text("base AGENTS.md v2"))
	s.golden("tree.txt", tg.tree())
	s.golden("status-after.txt", s.text("status"))
}

// Scenario 8b: files a git target committed with CRLF line endings are its
// own, as plan sees them, even where core.autocrlf=true (on any OS) would
// hash them like the pack's LF version: apply leaves them byte for byte,
// and --adopt takes one back.
func TestCRLFCommitted(t *testing.T) {
	t.Parallel()
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	h := baseHub(t)
	tg := newRepo(t, false)
	tg.write(optInFile, optInBase)
	tg.write("AGENTS.md", crlf(h.read("packs/base/AGENTS.md")))
	tg.write(".github/pull_request_template.md", h.read("packs/base/.github/pull_request_template.md"))
	tg.write("docs/guide.md", crlf(h.read("packs/base/docs/guide.md")))
	tg.commit("sync")
	tg.git("config", "core.autocrlf", "true")
	for _, p := range []string{"AGENTS.md", ".github", "docs"} {
		if err := os.RemoveAll(tg.abs(p)); err != nil {
			t.Fatal(err)
		}
	}
	tg.git("checkout", "--", ".")
	s := newScenario(t, "crlf-committed", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))

	h.write("packs/base/AGENTS.md", text("base AGENTS.md v2"))
	h.commit("base v2")
	s.golden("apply.json", s.json("apply"))
	s.wantContent("AGENTS.md", crlf(text("base AGENTS.md v1")))
	s.wantContent("docs/guide.md", crlf(text("base guide v1")))
	s.golden("apply-adopt.json", s.json("apply", "--adopt", "AGENTS.md"))
	s.wantContent("AGENTS.md", text("base AGENTS.md v2"))
	s.golden("tree.txt", tg.tree())
	s.golden("status-after.txt", s.text("status"))
}

// Scenario 9: requires delivers the required pack too, layered before the
// pack that requires it.
func TestRequires(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML+"packs:\n  claude:\n    requires: [agents]\n")
	h.write("packs/agents/AGENTS.md", text("agents AGENTS.md v1"))
	h.write("packs/agents/.agents/shared.md", text("agents shared v1"))
	h.write("packs/claude/CLAUDE.md", text("claude CLAUDE.md v1"))
	h.write("packs/claude/.agents/shared.md", text("claude shared v1"))
	h.commit("packs")
	tg := optedIn(t, "version: 1\npacks: [claude]\n")
	s := newScenario(t, "requires", h, tg, "--repo", "acme/svc")
	rep := s.report("status")
	if got := strings.Join(rep.Selection.Packs, ","); got != "agents,claude" {
		t.Errorf("packs = %s, want agents,claude", got)
	}
	s.golden("apply.json", s.json("apply"))
	s.wantContent(".agents/shared.md", text("claude shared v1"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 10: a renamed pack keeps its history through formerly: an old
// version is outdated and a path only the old name shipped is retired.
func TestFormerly(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML)
	h.write("packs/python-lib/pyproject.md", text("python-lib pyproject v1"))
	h.write("packs/python-lib/only-old.md", text("python-lib only-old v1"))
	h.commit("python-lib")
	h.git("mv", "packs/python-lib", "packs/python-library")
	h.rm("packs/python-library/only-old.md")
	h.write("packs/python-library/pyproject.md", text("python-library pyproject v2"))
	h.write("hub.yml", hubYML+"packs:\n  python-library:\n    formerly: [python-lib]\n")
	h.commit("rename python-lib to python-library")
	tg := optedIn(t, "version: 1\npacks: [python-lib]\n")
	tg.put("pyproject.md", text("python-lib pyproject v1"))
	tg.put("only-old.md", text("python-lib only-old v1"))
	s := newScenario(t, "formerly", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", tg.tree())
	s.golden("manifest.json", s.normalize(s.run(0, "manifest").stdout))
}

// Scenario 11: files of a pack the target no longer gets are reported
// orphaned and kept.
func TestOrphaned(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/extra/EXTRA.md", text("extra EXTRA.md v1"))
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base, extra]\n")
	h.commit("extra")
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base]\n")
	h.commit("drop extra from defaults")
	tg := optedIn(t, "version: 1\n")
	tg.put("EXTRA.md", text("extra EXTRA.md v1"))
	s := newScenario(t, "orphaned", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent("EXTRA.md", text("extra EXTRA.md v1"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 12a: a historical version under 64 bytes is not evidence: a
// target holding it keeps its file.
func TestTinyVersionIsNotEvidence(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML)
	h.write("packs/base/config.json", "{}\n")
	h.write("packs/base/empty/.gitkeep", "")
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v1"))
	h.commit("tiny files")
	h.write("packs/base/config.json", text(`{"shared": true}`))
	h.rm("packs/base/empty/.gitkeep")
	h.commit("grow config, drop .gitkeep")
	tg := optedIn(t, optInBase)
	tg.put("config.json", "{}\n")
	tg.put("empty/.gitkeep", "")
	s := newScenario(t, "tiny-version", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.wantContent("config.json", "{}\n")
	s.golden("tree.txt", tg.tree())
}

// caseInsensitive reports whether the filesystem holding dir ignores case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// Scenario 13: a file that became a directory and a directory that became a
// file converge in one apply.
func TestFileDirectoryConversions(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/handbook", text("base handbook as a file"))
	h.write("packs/base/tools/run.md", text("base tools/run.md"))
	h.commit("handbook file, tools directory")
	h.rm("packs/base/handbook")
	h.write("packs/base/handbook/index.md", text("base handbook/index.md"))
	h.rm("packs/base/tools")
	h.write("packs/base/tools", text("base tools as a file"))
	h.commit("swap file and directory")
	tg := optedIn(t, optInBase)
	tg.put("handbook", text("base handbook as a file"))
	tg.put("tools/run.md", text("base tools/run.md"))
	s := newScenario(t, "file-directory", h, tg, "--repo", "acme/svc")
	s.golden("status.json", s.json("status"))
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", tg.tree())
	s.golden("status-after.txt", s.text("status"))
}

// Scenario 14a: a shallow hub is refused with exit 2.
func TestShallowHub(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v2"))
	h.commit("base v2")
	clone := t.TempDir()
	h.git("clone", "-q", "--depth", "1", fileURL(h.dir), clone)
	tg := optedIn(t, optInBase)
	for _, args := range [][]string{
		{"status", "--hub", clone, "--dir", tg.dir, "--repo", "acme/svc"},
		{"apply", "--hub", clone, "--dir", tg.dir, "--repo", "acme/svc"},
		{"manifest", "--hub", clone},
	} {
		res := run(t, args...)
		if res.code != exitUsage || !strings.Contains(res.stderr, "shallow") {
			t.Errorf("%s: exit %d, stderr %q; want 2 and a shallow error", args[0], res.code, res.stderr)
		}
	}
	res := run(t, "check", "--hub", clone)
	if res.code != exitUsage || !strings.Contains(res.stdout, "shallow") {
		t.Errorf("check: exit %d, stdout %q; want 2 and a shallow error", res.code, res.stdout)
	}
	if tg.exists("AGENTS.md") {
		t.Error("apply wrote into the target from a shallow hub")
	}
}

// fileURL returns a file:// URL for a local directory, which makes clone
// honour --depth.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	return "file://" + p
}

// Scenario 14b: uncommitted pack edits are ignored unless --worktree.
func TestWorktree(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.put("packs/base/AGENTS.md", text("base AGENTS.md uncommitted"))
	h.put("packs/base/NEW.md", text("base NEW.md uncommitted"))
	tg := optedIn(t, optInBase)
	tg.put("AGENTS.md", text("base AGENTS.md v1"))
	s := newScenario(t, "worktree", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("status-worktree.txt", s.text("status", "--worktree"))
	s.golden("apply-worktree.json", s.json("apply", "--worktree"))
	s.wantContent("AGENTS.md", text("base AGENTS.md uncommitted"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 15: an org selector cannot be evaluated locally: status warns,
// apply refuses without --packs and writes nothing, and works with it.
func TestIncompleteSelection(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/extra/EXTRA.md", text("extra EXTRA.md v1"))
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\n    topics: [python]\n    packs: [extra]\n")
	h.commit("org selector")
	tg := optedIn(t, "version: 1\n")
	s := newScenario(t, "incomplete-selection", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("status.json", s.json("status"))
	before := tg.tree()
	res := s.run(exitUsage, "apply")
	if !strings.Contains(res.stderr, "selection incomplete") || res.stdout != "" {
		t.Errorf("apply: stdout %q, stderr %q; want only a selection error", res.stdout, res.stderr)
	}
	if after := tg.tree(); after != before {
		t.Errorf("apply wrote with an incomplete selection:\n%s", after)
	}
	s.golden("apply-packs.json", s.json("apply", "--packs", "base,extra"))
	s.golden("tree.txt", tg.tree())
}

// Scenario 16: the pre-v1 formats (a repos: list, an opt-in file
// without version) and a hub without hub.yml still work.
func TestLegacyFormats(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("targets.yml", "repos:\n  - acme/svc\n  - acme/other\n")
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v1"))
	h.write("packs/base/docs/guide.md", text("base guide v1"))
	h.commit("legacy hub")
	tg := optedIn(t, "packs: [base]\nignore:\n  - docs/**\n")
	s := newScenario(t, "legacy", h, tg, "--repo", "acme/svc")
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", tg.tree())
	s.golden("status.txt", s.text("status"))
	s.golden("check.txt", s.normalize(s.run(0, "check").stdout))
}

// Scenario 17: check reports every defect and exits 2; a clean hub passes.
func TestCheckReportsDefects(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", "version: 1\nid: change-me\npacks:\n  a:\n    requires: [b]\n  b:\n    requires: [a]\n")
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base, nope]\n")
	h.write("packs/a/A.md", text("pack a"))
	h.write("packs/b/B.md", text("pack b"))
	h.write("packs/base/tiny.txt", "tiny\n")
	h.write("packs/base/README.md", text("base README"))
	h.write("packs/base/"+optInFile, text("version: 1"))
	h.write("packs/extra/readme.md", text("extra readme"))
	h.setEntry("120000", "README.md", "packs/base/link")
	h.commit("defects")
	s := newScenario(t, "check-defects", h, nil)
	res := s.run(exitUsage, "check")
	s.golden("check.txt", s.normalize(res.stdout))
	res = s.run(exitUsage, "check", "--format", "json")
	s.golden("check.json", s.normalize(res.stdout))
}

func TestCheckCleanHub(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base]\n")
	h.commit("targets")
	s := newScenario(t, "check-clean", h, nil)
	s.golden("check.txt", s.normalize(s.run(0, "check").stdout))
}

// A target without the opt-in file is reported and left alone.
func TestNotOptedIn(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := newRepo(t, false)
	s := newScenario(t, "not-opted-in", h, tg, "--repo", "acme/svc")
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	if tree := tg.tree(); tree != "\n" {
		t.Errorf("apply wrote into a target that did not opt in:\n%s", tree)
	}
}

// A target outside git is compared by raw bytes.
func TestPlainDirectoryTarget(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := newPlainDir(t)
	tg.put(optInFile, optInBase)
	tg.put("AGENTS.md", text("base AGENTS.md v1"))
	s := newScenario(t, "plain-directory", h, tg)
	s.golden("status.txt", s.text("status"))
	s.golden("apply.json", s.json("apply"))
	s.golden("tree.txt", tg.tree())
}

// Without --repo the target is named after its origin remote, with the
// provider whose host matches.
func TestRefFromOrigin(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("hub.yml", hubYML+"platform: github\n")
	h.write("packs/python/pyproject.md", text("python pyproject v1"))
	h.write("targets.yml", "version: 1\ntargets:\n  - repo: acme/svc\n    packs: [python]\n")
	h.commit("github provider")
	tg := optedIn(t, optInBase)
	tg.git("remote", "add", "origin", "git@github.com:acme/svc.git")
	s := newScenario(t, "ref-from-origin", h, tg)
	rep := s.report("status")
	if rep.Target.Ref != "github:acme/svc" {
		t.Errorf("ref = %q, want github:acme/svc", rep.Target.Ref)
	}
	if got := strings.Join(rep.Selection.Packs, ","); got != "python,base" || !rep.Selection.Complete {
		t.Errorf("selection = %s (complete %v), want python,base", got, rep.Selection.Complete)
	}

	// Without a remote the targets.yml entries cannot be matched.
	tg.git("remote", "remove", "origin")
	rep = s.report("status")
	if rep.Target.Ref != "" || rep.Selection.Complete {
		t.Errorf("without origin: ref %q, complete %v; want no ref, incomplete", rep.Target.Ref, rep.Selection.Complete)
	}
	s.run(exitUsage, "apply")
}

// --packs, --adopt and the opt-in file are validated before anything is
// written.
func TestSelectionErrors(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := optedIn(t, "version: 1\npacks: [nope]\n")
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"status"}, `unknown pack "nope"`},
		{[]string{"apply", "--packs", "base,nope"}, `unknown pack "nope"`},
		{[]string{"apply", "--packs", "base,"}, "empty pack name"},
		{[]string{"apply", "--adopt", "/"}, "is empty"},
	} {
		res := s.run(exitUsage, tc.args[0], tc.args[1:]...)
		if !strings.Contains(res.stderr, tc.want) {
			t.Errorf("%v: stderr %q, want %q", tc.args, res.stderr, tc.want)
		}
	}
	tg.put(optInFile, "version: 1\npacks: base\n")
	if res := s.run(exitUsage, "status"); !strings.Contains(res.stderr, optInFile) {
		t.Errorf("invalid opt-in file: stderr %q", res.stderr)
	}
	if tg.exists("AGENTS.md") {
		t.Error("a failed command wrote into the target")
	}
}

// A symlinked opt-in file is refused.
func TestOptInSymlink(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := newRepo(t, false)
	tg.put("elsewhere.yml", optInBase)
	if err := os.Symlink("elsewhere.yml", tg.abs(optInFile)); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	if res := s.run(exitUsage, "status"); !strings.Contains(res.stderr, "not a regular file") {
		t.Errorf("stderr %q, want a not-a-regular-file error", res.stderr)
	}
}

// A symlink at a managed path is unsafe and left alone.
func TestUnsafeSymlink(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := optedIn(t, optInBase)
	tg.put("elsewhere.md", "target content\n")
	if err := os.Symlink("elsewhere.md", tg.abs("AGENTS.md")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	rep := s.report("apply")
	if e := entry(t, rep, "AGENTS.md"); e.State != string(decide.Unsafe) || e.Action != string(decide.Keep) {
		t.Errorf("AGENTS.md: %+v, want unsafe keep", e)
	}
	if fi, err := os.Lstat(tg.abs("AGENTS.md")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("AGENTS.md is no longer the symlink: %v", err)
	}
}

// A failed write makes apply exit 1 after reporting.
func TestApplyFailureExitsOne(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test user cannot write to")
	}
	h := baseHub(t)
	tg := optedIn(t, optInBase)
	tg.put("docs/other.md", "the target's own\n")
	if err := os.Chmod(tg.abs("docs"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tg.abs("docs"), 0o755) })
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	res := s.run(exitFailed, "apply", "--format", "json")
	rep := decodeSync(t, res.stdout)
	if rep.Summary.Failed != 1 || rep.Summary.Done != 2 {
		t.Errorf("summary %+v, want 1 failed and 2 done", rep.Summary)
	}
}

// --dry-run reports what apply would do and writes nothing.
func TestDryRun(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := optedIn(t, optInBase)
	s := newScenario(t, "dry-run", h, tg, "--repo", "acme/svc")
	s.golden("apply-dry-run.txt", s.text("apply", "--dry-run"))
	rep := s.report("apply", "--dry-run")
	if !rep.DryRun || rep.Results != nil || rep.Summary.Changes != 3 {
		t.Errorf("dry run report: dry_run %v, results %v, changes %d", rep.DryRun, rep.Results, rep.Summary.Changes)
	}
	if tg.exists("AGENTS.md") {
		t.Error("--dry-run wrote into the target")
	}
}

// A config file that fails to parse stops status and apply with exit 2 and
// check lists the problem. Configs are read from HEAD unless --worktree.
func TestBrokenConfig(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("hub.yml", hubYML+"bogus: 1\n")
	h.commit("typo")
	tg := optedIn(t, optInBase)
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	for _, command := range []string{"status", "apply"} {
		res := s.run(exitUsage, command)
		if !strings.Contains(res.stderr, `unknown key "bogus"`) || !strings.Contains(res.stderr, "touchmark check") {
			t.Errorf("%s: stderr %q, want the parse error and a hint to run check", command, res.stderr)
		}
	}
	if res := s.run(exitUsage, "check"); !strings.Contains(res.stdout, `unknown key "bogus"`) {
		t.Errorf("check: stdout %q, want the parse error", res.stdout)
	}
	h.put("hub.yml", hubYML)
	s.run(exitUsage, "status")
	s.run(0, "status", "--worktree")
	if tg.exists("AGENTS.md") {
		t.Error("a failed command wrote into the target")
	}
}
