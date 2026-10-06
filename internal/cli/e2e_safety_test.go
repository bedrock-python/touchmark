package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/report"
)

// changes returns "action path" for every entry of rep that is not kept.
func changes(rep report.Sync) []string {
	var out []string
	for _, e := range rep.Entries {
		if e.Action != string(decide.Keep) {
			out = append(out, e.Action+" "+e.Path)
		}
	}
	return out
}

// tracked returns the paths the target's index holds after `git add -A`.
func (r *repo) tracked() []string {
	r.t.Helper()
	r.git("add", "-A")
	var out []string
	for _, p := range strings.Split(r.git("ls-files"), "\n") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Scenario 12b: a case-only rename that also changes the content. On a
// case-insensitive filesystem the target's file keeps its history and is
// updated in place; on a case-sensitive one the old spelling retires and the
// new one is created after it. Either way the target ends up tracking one
// path with the new version, never two spellings.
func TestCaseOnlyRename(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML)
	h.write("packs/base/Docs/Guide.md", text("base guide v1"))
	h.commit("Docs/Guide.md")
	h.rm("packs/base/Docs/Guide.md")
	h.write("packs/base/docs/guide.md", text("base guide v2"))
	h.commit("rename to docs/guide.md, v2")
	tg := newRepo(t, false)
	tg.write(optInFile, optInBase)
	tg.write("Docs/Guide.md", text("base guide v1"))
	tg.commit("synced before the rename")
	s := newScenario(t, "case-rename", h, tg, "--repo", "acme/svc")

	insensitive := caseInsensitive(t, tg.dir)
	want := []string{"delete Docs/Guide.md", "create docs/guide.md"}
	if insensitive {
		want = []string{"update docs/guide.md"}
	}
	if got := changes(s.report("status")); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("status plans %q, want %q", got, want)
	}
	s.report("apply")
	if got := changes(s.report("status")); len(got) != 0 {
		t.Errorf("status after apply plans %q", got)
	}
	files := tg.tracked()
	var docs []string
	for _, p := range files {
		if strings.EqualFold(filepath.Dir(p), "docs") {
			docs = append(docs, p)
		}
	}
	if len(docs) != 1 {
		t.Fatalf("the target tracks %q under docs, want one path", docs)
	}
	if got := tg.git("show", ":"+docs[0]); got+"\n" != text("base guide v2") {
		t.Errorf("%s = %q, want v2", docs[0], got)
	}
}

// A file the target owns under one spelling is never joined by a pack's
// file under another: on a case-sensitive filesystem the pack's path is
// unsafe, on a case-insensitive one it is the target's local file.
func TestCaseVariantOfTargetFile(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML)
	h.write("packs/base/readme.md", text("base readme"))
	h.commit("readme.md")
	tg := optedIn(t, optInBase)
	tg.put("README.md", text("the target's own README"))
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	rep := s.report("apply")
	want := decide.Unsafe
	if caseInsensitive(t, tg.dir) {
		want = decide.Local
	}
	if e := entry(t, rep, "readme.md"); e.State != string(want) || e.Action != string(decide.Keep) {
		t.Errorf("readme.md = %+v, want %s", e, want)
	}
	entries, err := os.ReadDir(tg.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "readme.md" {
			t.Error("apply created readme.md next to README.md")
		}
	}
	if got := tg.read("README.md"); got != text("the target's own README") {
		t.Errorf("README.md = %q", got)
	}
}

// The target's opt-in file is never touched, whatever the hub's history
// holds at its name or at a case variant of it, and --adopt '**' does not
// change that.
func TestOptInFileProtected(t *testing.T) {
	t.Parallel()
	consent := "version: 1\npacks: [base]\n# the hub once shipped exactly this file, by mistake\n"
	h := baseHub(t)
	h.write("packs/base/"+optInFile, consent)
	h.commit("ship the opt-in file")
	h.rm("packs/base/" + optInFile)
	h.write("packs/base/.Engineering-Assets.yml", consent)
	h.commit("ship it under another spelling")
	h.rm("packs/base/.Engineering-Assets.yml")
	h.commit("stop shipping it")
	tg := optedIn(t, consent)
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")

	rep := s.report("status")
	if e := entry(t, rep, optInFile); e.State != string(decide.Ignored) || e.Detail != "the target's opt-in file" {
		t.Errorf("%s = %+v, want ignored as the opt-in file", optInFile, e)
	}
	for _, args := range [][]string{{"apply"}, {"apply", "--adopt", "**"}} {
		rep := s.report(args[0], args[1:]...)
		for _, e := range rep.Entries {
			if strings.EqualFold(e.Path, optInFile) && e.Action != string(decide.Keep) {
				t.Errorf("%v plans %+v", args, e)
			}
		}
		if got := tg.read(optInFile); got != consent {
			t.Fatalf("%v changed the opt-in file to %q", args, got)
		}
	}

	// A pack that ships the opt-in file now, in any case, fails check, and
	// status and apply refuse the hub.
	for _, name := range []string{optInFile, ".ENGINEERING-assets.yml"} {
		h.write("packs/base/"+name, consent)
		h.commit("ship " + name)
		for _, command := range []string{"status", "apply"} {
			res := s.run(exitUsage, command)
			if !strings.Contains(res.stderr, "the opt-in file is never shipped") || !strings.Contains(res.stderr, "touchmark check") {
				t.Errorf("%s with %s shipped: stderr %q", command, name, res.stderr)
			}
		}
		h.rm("packs/base/" + name)
		h.commit("stop shipping " + name)
	}
	if got := tg.read(optInFile); got != consent {
		t.Errorf("the opt-in file changed to %q", got)
	}
}

// A hub may name another opt-in file, also in a subdirectory; it is read
// from there and protected like the default one.
func TestCustomOptInFile(t *testing.T) {
	t.Parallel()
	consent := "version: 1\npacks: [base]\n# opted in through a file in .github, kept by the target\n"
	h := newHub(t)
	h.write("hub.yml", hubYML+"opt_in_file: .github/assets.yml\n")
	h.write("packs/base/.github/assets.yml", consent)
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v1"))
	h.commit("base, with the opt-in file by mistake")
	h.rm("packs/base/.github/assets.yml")
	h.commit("stop shipping it")
	tg := newRepo(t, false)
	tg.put(".github/assets.yml", consent)
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	rep := s.report("apply", "--adopt", "**")
	if !rep.Target.OptedIn || rep.Target.OptInFile != ".github/assets.yml" {
		t.Errorf("target = %+v, want opted in through .github/assets.yml", rep.Target)
	}
	if e := entry(t, rep, ".github/assets.yml"); e.State != string(decide.Ignored) {
		t.Errorf(".github/assets.yml = %+v, want ignored", e)
	}
	if got := tg.read(".github/assets.yml"); got != consent {
		t.Errorf("the opt-in file changed to %q", got)
	}
	s.wantContent("AGENTS.md", text("base AGENTS.md v1"))
}

// status and apply refuse a hub that fails check: a formerly that names a
// current pack would otherwise turn that pack's files into retired ones,
// and a top-level .lfsconfig would reconfigure LFS in every target.
func TestHubMustPassCheck(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML+"packs:\n  claude:\n    formerly: [agents]\n")
	h.write("packs/agents/AGENTS.md", text("agents AGENTS.md v1"))
	h.write("packs/claude/CLAUDE.md", text("claude CLAUDE.md v1"))
	h.commit("packs")
	tg := optedIn(t, "version: 1\npacks: [claude]\n")
	tg.put("AGENTS.md", text("agents AGENTS.md v1"))
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	for _, command := range []string{"status", "apply"} {
		res := s.run(exitUsage, command)
		if !strings.Contains(res.stderr, `"agents" is a current pack`) {
			t.Errorf("%s: stderr %q, want the formerly error", command, res.stderr)
		}
	}
	s.wantContent("AGENTS.md", text("agents AGENTS.md v1"))

	h.write("hub.yml", hubYML)
	h.write("packs/claude/.lfsconfig", "[lfs]\n\turl = https://lfs.example.com/owned-by-someone-else\n")
	h.commit("lfsconfig")
	if res := s.run(exitUsage, "apply"); !strings.Contains(res.stderr, ".lfsconfig is never shipped") {
		t.Errorf("apply with .lfsconfig: stderr %q", res.stderr)
	}
	if tg.exists(".lfsconfig") || tg.exists("CLAUDE.md") {
		t.Error("apply wrote from a hub that fails check")
	}
}

// --packs works when hub.yml's requires still uses a pack's former name:
// that name is the hub's business, not the user's.
func TestPacksWithFormerNameInRequires(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML+"packs:\n  agents:\n    formerly: [base]\n  claude:\n    requires: [base]\n")
	h.write("packs/agents/AGENTS.md", text("agents AGENTS.md v1"))
	h.write("packs/claude/CLAUDE.md", text("claude CLAUDE.md v1"))
	h.write("targets.yml", "version: 1\ntargets:\n  - org: acme\n    packs: [claude]\n")
	h.commit("packs")
	tg := optedIn(t, "version: 1\n")
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	s.run(exitUsage, "apply")
	rep := s.report("apply", "--packs", "claude")
	if got := strings.Join(rep.Selection.Packs, ","); got != "agents,claude" {
		t.Errorf("packs = %s, want agents,claude", got)
	}
	s.wantContent("AGENTS.md", text("agents AGENTS.md v1"))
	if res := s.run(exitUsage, "apply", "--packs", "base"); !strings.Contains(res.stderr, `pack "base" was renamed to "agents"`) {
		t.Errorf("--packs base: stderr %q", res.stderr)
	}
}

// A formerly that points at a pack that is gone does not turn a reference to
// the old pack into a pack that ships nothing and deletes its files.
func TestFormerlyOfRemovedPack(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	h.write("hub.yml", hubYML)
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v1"))
	h.write("packs/extra/EXTRA.md", text("extra EXTRA.md v1"))
	h.write("targets.yml", "version: 1\ndefaults:\n  packs: [base]\n")
	h.commit("base")
	h.rm("packs/base")
	h.write("hub.yml", hubYML+"packs:\n  agents:\n    formerly: [base]\n")
	h.commit("drop base, half-renamed")
	tg := optedIn(t, "version: 1\n")
	tg.put("AGENTS.md", text("base AGENTS.md v1"))
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	for _, command := range []string{"status", "apply"} {
		res := s.run(exitUsage, command)
		if !strings.Contains(res.stderr, `unknown pack "base"`) {
			t.Errorf("%s: stderr %q, want an unknown pack", command, res.stderr)
		}
	}
	s.wantContent("AGENTS.md", text("base AGENTS.md v1"))
}

// A sha256 hub or target is refused: its ids never equal the sha1 ids of
// target files.
func TestSHA256Refused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := &repo{t: t, dir: dir, g: gitx.New(dir)}
	if _, err := h.g.Run(t.Context(), nil, "init", "-q", "-b", "master", "--object-format=sha256"); err != nil {
		t.Skipf("git cannot create a sha256 repository: %v", err)
	}
	h.git("config", "core.autocrlf", "false")
	h.write("hub.yml", hubYML)
	h.write("packs/base/AGENTS.md", text("base AGENTS.md v1"))
	h.commit("base")
	tg := optedIn(t, optInBase)
	tg.put("AGENTS.md", text("base AGENTS.md v1"))
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	for _, command := range []string{"check", "status", "apply"} {
		if res := s.run(exitUsage, command); !strings.Contains(res.stderr, "sha256 object ids") {
			t.Errorf("%s: stderr %q", command, res.stderr)
		}
	}

	// A sha256 target next to a sha1 hub.
	tdir := t.TempDir()
	target := &repo{t: t, dir: tdir, g: gitx.New(tdir)}
	target.git("init", "-q", "-b", "master", "--object-format=sha256")
	target.put(optInFile, optInBase)
	s = newScenario(t, "", baseHub(t), target, "--repo", "acme/svc")
	if res := s.run(exitUsage, "status"); !strings.Contains(res.stderr, "sha256 object ids") {
		t.Errorf("sha256 target: stderr %q", res.stderr)
	}
}

// A hub cloned with --filter would fetch every historical blob one by one.
func TestPartialCloneHub(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.git("config", "uploadpack.allowFilter", "true")
	clone := t.TempDir()
	h.git("clone", "-q", "--filter=blob:none", fileURL(h.dir), clone)
	tg := optedIn(t, optInBase)
	if res := run(t, "status", "--hub", clone, "--dir", tg.dir, "--repo", "acme/svc"); res.code != exitUsage || !strings.Contains(res.stderr, "partial clone") {
		t.Errorf("status: exit %d, stderr %q", res.code, res.stderr)
	}
	if res := run(t, "check", "--hub", clone); res.code != exitUsage || !strings.Contains(res.stdout, "partial clone") {
		t.Errorf("check: exit %d, stdout %q", res.code, res.stdout)
	}
}

// --worktree on a hub checked out with CRLF (core.autocrlf=true, the Git for
// Windows default) reads what git would commit: an unedited checkout gives
// the same status as the commit, and apply --worktree changes nothing.
func TestWorktreeCRLFHub(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.git("config", "core.autocrlf", "true")
	if err := os.RemoveAll(h.abs("packs")); err != nil {
		t.Fatal(err)
	}
	h.git("checkout", "--", "packs")
	if !strings.Contains(h.read("packs/base/AGENTS.md"), "\r\n") {
		t.Fatal("the hub was not checked out with CRLF")
	}
	tg := optedIn(t, optInBase)
	for _, p := range []string{"AGENTS.md", ".github/pull_request_template.md", "docs/guide.md"} {
		tg.put(p, text(map[string]string{
			"AGENTS.md":                        "base AGENTS.md v1",
			".github/pull_request_template.md": "base PR template v1",
			"docs/guide.md":                    "base guide v1",
		}[p]))
	}
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	if committed, work := s.text("status"), s.text("status", "--worktree"); committed != work {
		t.Errorf("status --worktree differs from status:\n%s\nwant\n%s", work, committed)
	}
	if rep := s.report("apply", "--worktree"); rep.Summary.Changes != 0 {
		t.Errorf("apply --worktree plans %q", changes(rep))
	}

	// An uncommitted edit ships as git would commit it: with LF.
	h.put("packs/base/AGENTS.md", strings.ReplaceAll(text("base AGENTS.md edited"), "\n", "\r\n"))
	s.report("apply", "--worktree")
	s.wantContent("AGENTS.md", text("base AGENTS.md edited"))
}

// --dir inside a .git directory is not a target.
func TestTargetInsideGitDir(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	tg := newRepo(t, false)
	gitDir := filepath.Join(tg.dir, ".git")
	if err := os.WriteFile(filepath.Join(gitDir, optInFile), []byte(optInBase), 0o644); err != nil {
		t.Fatal(err)
	}
	res := run(t, "apply", "--hub", h.dir, "--dir", gitDir, "--repo", "acme/svc")
	if res.code != exitUsage || !strings.Contains(res.stderr, "inside a git directory") {
		t.Errorf("exit %d, stderr %q", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "AGENTS.md")); err == nil {
		t.Error("apply wrote into .git")
	}
}

// The executable bit reaches the target's commits where git ignores it on
// disk (Windows, core.fileMode=false): status sees a tracked copy without
// it, and apply records it in the index, for a new file too.
func TestExecutableBit(t *testing.T) {
	t.Parallel()
	h := baseHub(t)
	h.write("packs/base/scripts/run.sh", "#!/bin/sh\n# shared engineering asset, kept in sync by touchmark\n")
	h.git("update-index", "--chmod=+x", "packs/base/scripts/run.sh")
	h.write("packs/base/scripts/new.sh", "#!/bin/sh\n# another shared engineering script, kept in sync by touchmark\n")
	h.git("update-index", "--chmod=+x", "packs/base/scripts/new.sh")
	h.commit("scripts")
	tg := newRepo(t, false)
	tg.git("config", "core.fileMode", "false")
	tg.write(optInFile, optInBase)
	tg.write("scripts/run.sh", h.read("packs/base/scripts/run.sh"))
	tg.commit("run.sh without the executable bit")
	s := newScenario(t, "", h, tg, "--repo", "acme/svc")
	if e := entry(t, s.report("status"), "scripts/run.sh"); e.State != string(decide.Current) || e.Action != string(decide.Chmod) {
		t.Errorf("scripts/run.sh = %+v, want current/chmod", e)
	}
	s.report("apply")
	for _, p := range []string{"scripts/run.sh", "scripts/new.sh"} {
		if got := tg.git("ls-files", "-s", "--", p); !strings.HasPrefix(got, "100755 ") {
			t.Errorf("index entry of %s: %q, want 100755", p, got)
		}
	}
	if got := changes(s.report("status")); len(got) != 0 {
		t.Errorf("status after apply plans %q", got)
	}
}
