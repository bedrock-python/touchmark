package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/provenance"
)

// testEnv isolates git from the machine's config (which may set
// core.autocrlf) and makes it deterministic.
func testEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	date := "1767225600 +0000"
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + global,
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_AUTHOR_NAME=touchmark test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=touchmark test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE=" + date,
		"GIT_CEILING_DIRECTORIES=" + os.TempDir(),
	}
}

// treeEntry is one entry of a tree built with git: content is written as
// a blob, or oid is used as is (a gitlink).
type treeEntry struct {
	mode, path, content, oid string
}

// gitTree builds a commit in a throwaway repository with git fast-import
// (so modes, symlinks and gitlinks are what git writes, on any OS, in one
// process) and returns its snapshot from `git ls-tree`, as GitSource does.
func gitTree(t *testing.T, entries []treeEntry) *Tree {
	t.Helper()
	g := &gitx.Git{Dir: t.TempDir(), Env: testEnv(t)}
	run := func(stdin string, args ...string) string {
		t.Helper()
		out, err := g.Run(t.Context(), strings.NewReader(stdin), args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q")
	var in strings.Builder
	in.WriteString("commit refs/heads/snapshot\n")
	in.WriteString("committer touchmark test <test@example.com> 1767225600 +0000\n")
	in.WriteString("data 6\ntarget\n")
	for _, e := range entries {
		if e.oid != "" {
			fmt.Fprintf(&in, "M %s %s %s\n", e.mode, e.oid, e.path)
			continue
		}
		fmt.Fprintf(&in, "M %s inline %s\ndata %d\n%s\n", e.mode, e.path, len(e.content), e.content)
	}
	run(in.String(), "-c", "core.autocrlf=false", "fast-import", "--quiet")
	commit := run("", "rev-parse", "--verify", "refs/heads/snapshot^{commit}")
	listed, err := g.LsTree(t.Context(), commit, "")
	if err != nil {
		t.Fatal(err)
	}
	tree := &Tree{Commit: commit, Entries: map[string]Entry{}}
	for _, e := range listed {
		tree.Entries[e.Path] = Entry{Mode: e.Mode, OID: e.OID}
	}
	return tree
}

// version is the content of a pack file: large enough to be evidence.
func version(name string, n int) string {
	return strings.Repeat(fmt.Sprintf("%s, version %d\n", name, n), 8)
}

// TestDecideOnSnapshot runs the delivery path of a target end to end without
// a checkout: a tree from git, Observe, Decide, D and its key.
func TestDecideOnSnapshot(t *testing.T) {
	oid := func(content string) string { return gitx.RawOID([]byte(content)) }
	const pack = "agents"
	// What the pack ships now, and what it shipped before.
	now := map[string]struct {
		content, mode string
	}{
		"AGENTS.md":          {version("agents", 2), "100644"},
		"ruff.toml":          {version("ruff", 2), "100644"},
		"README.md":          {version("readme", 1), "100644"},
		"new.md":             {version("new", 1), "100644"},
		"scripts/check.sh":   {version("check", 1), "100755"},
		"link.md":            {version("link", 1), "100644"},
		"vendor/sub/file.md": {version("sub", 1), "100644"},
		"guide.md":           {version("guide", 1), "100644"},
		"Notes.md":           {version("notes", 2), "100644"},
		"tools":              {version("tools", 1), "100644"},
		"blocker/x":          {version("blocker-x", 1), "100644"},
		"docs":               {version("docs", 1), "100644"},
	}
	before := map[string]string{
		"AGENTS.md":  version("agents", 1),
		"ruff.toml":  version("ruff", 1),
		"notes.md":   version("notes", 1),
		"old.md":     version("old", 1),
		"tools/a.sh": version("tools-a", 1),
		"blocker":    version("blocker", 1),
	}
	manifest := &provenance.Manifest{Version: provenance.ManifestVersion}
	current := provenance.Current{pack: {}}
	for p, content := range before {
		manifest.Add(p, pack, provenance.Version{OID: oid(content), Size: int64(len(content))})
	}
	for p, f := range now {
		v := provenance.Version{OID: oid(f.content), Size: int64(len(f.content))}
		manifest.Add(p, pack, v)
		current[pack][p] = provenance.File{Pack: pack, Path: p, OID: v.OID, Size: v.Size, Mode: f.mode}
	}

	// The target's base: every state a path can be in, and one path git
	// sees but no pack ever shipped.
	base := []treeEntry{
		{mode: "100644", path: "AGENTS.md", content: now["AGENTS.md"].content},         // current
		{mode: "100644", path: "ruff.toml", content: before["ruff.toml"]},              // outdated
		{mode: "100644", path: "README.md", content: "our own readme, not the pack's"}, // local
		{mode: "100644", path: "scripts/check.sh", content: now["scripts/check.sh"].content},
		{mode: "120000", path: "link.md", content: "README.md"},
		{mode: "160000", path: "vendor/sub", oid: "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d"},
		{mode: "100644", path: "Guide.md", content: "the target's own guide"},
		{mode: "100644", path: "notes.md", content: before["notes.md"]},
		{mode: "100644", path: "old.md", content: before["old.md"]},
		{mode: "100644", path: "tools/a.sh", content: before["tools/a.sh"]},
		{mode: "100644", path: "blocker", content: before["blocker"]},
		{mode: "100644", path: "docs/guide.md", content: "the target's own docs"},
		{mode: "100644", path: "unrelated.txt", content: "not managed"},
	}
	tree := gitTree(t, base)
	if got := tree.Entries["link.md"].Mode; got != "120000" {
		t.Fatalf("fixture: link.md has mode %q", got)
	}
	if got := tree.Entries["vendor/sub"].Mode; got != "160000" {
		t.Fatalf("fixture: vendor/sub has mode %q", got)
	}

	in := decide.Input{
		Manifest:  manifest,
		Selected:  []string{pack},
		Desired:   decide.Layer(current, []string{pack}),
		OptInFile: ".touchmark.yml",
	}
	in.Observed = tree.Observe(decide.Paths(in))
	plan := decide.Decide(in)

	type row struct {
		path         string
		state        decide.State
		action       decide.Action
		from, to     string
		mode         string
		afterDeletes bool
	}
	var got []row
	for _, e := range plan.Entries {
		got = append(got, row{e.Path, e.State, e.Action, e.From, e.To, e.Mode, e.AfterDeletes})
	}
	c := func(p string) string { return oid(now[p].content) }
	b := func(p string) string { return oid(before[p]) }
	want := []row{
		{"tools/a.sh", decide.Retired, decide.Delete, b("tools/a.sh"), "", "", false},
		{"blocker", decide.Retired, decide.Delete, b("blocker"), "", "", false},
		{"notes.md", decide.Retired, decide.Delete, b("notes.md"), "", "", false},
		{"old.md", decide.Retired, decide.Delete, b("old.md"), "", "", false},
		{"AGENTS.md", decide.Current, decide.Keep, c("AGENTS.md"), "", "", false},
		{"Notes.md", decide.Missing, decide.Create, "", c("Notes.md"), "100644", true},
		{"README.md", decide.Local, decide.Keep, "", "", "", false},
		{"blocker/x", decide.Missing, decide.Create, "", c("blocker/x"), "100644", true},
		{"docs", decide.Unsafe, decide.Keep, "", "", "", false},
		{"guide.md", decide.Unsafe, decide.Keep, "", "", "", false},
		{"link.md", decide.Unsafe, decide.Keep, "", "", "", false},
		{"new.md", decide.Missing, decide.Create, "", c("new.md"), "100644", false},
		{"ruff.toml", decide.Outdated, decide.Update, b("ruff.toml"), c("ruff.toml"), "100644", false},
		{"scripts/check.sh", decide.Current, decide.Chmod, c("scripts/check.sh"), "", "100755", false},
		{"tools", decide.Missing, decide.Create, "", c("tools"), "100644", true},
		{"vendor/sub/file.md", decide.Unsafe, decide.Keep, "", "", "", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan =\n%v\nwant\n%v", got, want)
	}
	details := map[string]string{
		"link.md":            "a symlink",
		"vendor/sub/file.md": "parent vendor/sub is a submodule",
		"guide.md":           "differs only by case from Guide.md",
		"docs":               "a directory",
	}
	for _, e := range plan.Entries {
		if want, ok := details[e.Path]; ok && !strings.HasPrefix(e.Detail, want) {
			t.Errorf("%s: detail %q, want it to start with %q", e.Path, e.Detail, want)
		}
	}

	zero := decide.ZeroOID
	wantPairs := []decide.Pair{
		{Path: "Notes.md", From: zero, Mode: "100644", To: c("Notes.md")},
		{Path: "blocker", From: b("blocker"), Mode: decide.ModeDelete, To: zero},
		{Path: "blocker/x", From: zero, Mode: "100644", To: c("blocker/x")},
		{Path: "new.md", From: zero, Mode: "100644", To: c("new.md")},
		{Path: "notes.md", From: b("notes.md"), Mode: decide.ModeDelete, To: zero},
		{Path: "old.md", From: b("old.md"), Mode: decide.ModeDelete, To: zero},
		{Path: "ruff.toml", From: b("ruff.toml"), Mode: "100644", To: c("ruff.toml")},
		{Path: "scripts/check.sh", From: c("scripts/check.sh"), Mode: "100755", To: c("scripts/check.sh")},
		{Path: "tools", From: zero, Mode: "100644", To: c("tools")},
		{Path: "tools/a.sh", From: b("tools/a.sh"), Mode: decide.ModeDelete, To: zero},
	}
	pairs := decide.Pairs(plan)
	if !reflect.DeepEqual(pairs, wantPairs) {
		t.Fatalf("D =\n%v\nwant\n%v", pairs, wantPairs)
	}
	// Every from is what the base holds at the path.
	for _, p := range pairs {
		if e, ok := tree.Entries[p.Path]; ok != (p.From != zero) || (ok && e.OID != p.From) {
			t.Errorf("%s: from %s, the tree holds %+v", p.Path, p.From, e)
		}
	}
	key := decide.Key(decide.StreamSync, pairs)

	// The same target a commit later, with unrelated changes: D and the key
	// stay the same.
	later := append([]treeEntry(nil), base[:len(base)-1]...)
	later = append(later,
		treeEntry{mode: "100644", path: "unrelated.txt", content: "changed by the target"},
		treeEntry{mode: "100644", path: "src/main.py", content: "print('hi')\n"},
	)
	laterTree := gitTree(t, later)
	if laterTree.Commit == tree.Commit {
		t.Fatal("fixture: the later commit is the same")
	}
	in.Observed = laterTree.Observe(decide.Paths(in))
	if got := decide.Key(decide.StreamSync, decide.Pairs(decide.Decide(in))); got != key {
		t.Errorf("an unrelated commit changed the key: %s, want %s", got, key)
	}
}
