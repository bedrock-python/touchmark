package provenance

import (
	"strings"
	"testing"
)

func TestCheckPacks(t *testing.T) {
	t.Parallel()
	// f is a valid file of the minimum size.
	f := func(pack, path string) File {
		return File{Pack: pack, Path: path, OID: blob(path), Size: MinEvidenceSize, Mode: modeFile}
	}
	sized := func(pack, path string, size int64) File {
		file := f(pack, path)
		file.Size = size
		return file
	}
	moded := func(pack, path, mode string) File {
		file := f(pack, path)
		file.Mode = mode
		return file
	}
	const optIn = ".engineering-assets.yml"
	long := strings.Repeat("a", 64)

	tests := []struct {
		name  string
		cur   Current
		optIn string
		want  []wantProblem
	}{
		{
			name:  "clean",
			cur:   current(f("agents", "AGENTS.md"), f("agents", "docs/a.md"), moded("core", "bin/run", modeExec), f(long, "x.md")),
			optIn: optIn,
		},
		{name: "empty", cur: Current{}, optIn: optIn},
		{name: "same path in two packs is an override", cur: current(f("a", "README.md"), f("b", "README.md"))},
		{name: "shared directory", cur: current(f("a", "docs/x.md"), f("b", "docs/y.md"))},
		{
			name: "too small",
			cur:  current(sized("a", "x.md", MinEvidenceSize-1), sized("a", "empty", 0)),
			want: []wantProblem{{"a", "empty", "0 bytes, below the 64-byte minimum"}, {"a", "x.md", "63 bytes"}},
		},
		{
			name: "invalid paths",
			cur:  current(f("a", "x:y"), f("a", "../up"), f("a", ".git/config"), f("a", "CON.txt")),
			want: []wantProblem{
				{"a", "../up", "bad segment"},
				{"a", ".git/config", "inside .git"},
				{"a", "CON.txt", "reserved"},
				{"a", "x:y", "contains"},
			},
		},
		{
			name: "gitmodules at the top",
			cur:  current(f("a", ".gitmodules"), f("b", ".GITMODULES"), f("a", "sub/.gitmodules")),
			want: []wantProblem{{"a", ".gitmodules", ".gitmodules is never managed"}, {"b", ".GITMODULES", ".gitmodules is never managed"}},
		},
		{
			name:  "opt-in file",
			cur:   current(f("a", optIn), f("a", "sub/"+optIn)),
			optIn: optIn,
			want:  []wantProblem{{"a", optIn, "opt-in file"}},
		},
		{
			name:  "opt-in file in another case",
			cur:   current(f("b", ".Engineering-Assets.YML")),
			optIn: optIn,
			want:  []wantProblem{{"b", ".Engineering-Assets.YML", "opt-in file"}},
		},
		{
			name:  "custom opt-in file",
			cur:   current(f("a", "config/opt-in.yml"), f("a", optIn)),
			optIn: "config/opt-in.yml",
			want:  []wantProblem{{"a", "config/opt-in.yml", "opt-in file"}},
		},
		{name: "opt-in check off", cur: current(f("a", optIn))},
		{
			name: "lfsconfig at the top",
			cur:  current(f("a", ".lfsconfig"), f("a", "sub/.lfsconfig")),
			want: []wantProblem{{"a", ".lfsconfig", "Git LFS"}},
		},
		{
			name: "lfsconfig in another case",
			cur:  current(f("b", ".LfsConfig")),
			want: []wantProblem{{"b", ".LfsConfig", "Git LFS"}},
		},
		{
			// Case variants are also a collision.
			name: "opt-in file twice",
			cur:  current(f("a", optIn), f("b", ".Engineering-Assets.YML")),
			want: []wantProblem{{"a", optIn, `differs only by case from file ".Engineering-Assets.YML" in pack "b"`}},
		},
		{
			name: "mode",
			cur:  current(moded("a", "link", "120000"), moded("a", "none", "")),
			want: []wantProblem{{"a", "link", `unsupported mode "120000"`}, {"a", "none", `unsupported mode ""`}},
		},
		{
			name: "several problems on one file",
			cur:  current(sized("a", ".lfsconfig", 3)),
			want: []wantProblem{{"a", ".lfsconfig", "3 bytes"}, {"a", ".lfsconfig", "Git LFS"}},
		},
		{
			name: "invalid pack names",
			cur:  current(f("Bad", "x.md"), f("a--b", "x.md"), f(long+"a", "x.md"), f("-a", "x.md")),
			want: []wantProblem{
				{"-a", "", "must be lowercase"},
				{"Bad", "", "must be lowercase"},
				{"a--b", "", "must be lowercase"},
				{long + "a", "", "longer than 64"},
			},
		},
		{
			name: "case collision in one pack",
			cur:  current(f("a", "README.md"), f("a", "readme.md")),
			want: []wantProblem{{"a", "readme.md", `file differs only by case from file "README.md" in the same pack`}},
		},
		{
			name: "case collision across packs",
			cur:  current(f("a", "readme.md"), f("b", "README.md"), f("c", "README.md")),
			want: []wantProblem{{"a", "readme.md", `file differs only by case from file "README.md" in pack "b"`}},
		},
		{
			name: "directory case collision",
			cur:  current(f("a", "Docs/x.md"), f("b", "docs/y.md"), f("b", "docs/z.md")),
			want: []wantProblem{{"b", "docs", `directory differs only by case from directory "Docs" in pack "a"`}},
		},
		{
			name: "directory case collision in one pack",
			cur:  current(f("a", "Docs/x.md"), f("a", "docs/y.md")),
			want: []wantProblem{{"a", "docs", `directory differs only by case from directory "Docs" in the same pack`}},
		},
		{
			name: "file and directory",
			cur:  current(f("a", "docs"), f("b", "docs/x.md")),
			want: []wantProblem{{"b", "docs", `directory collides with a file of the same name in pack "a"`}},
		},
		{
			name: "file and directory differing by case",
			cur:  current(f("a", "Docs"), f("b", "docs/x.md")),
			want: []wantProblem{{"b", "docs", `directory differs only by case from file "Docs" in pack "a"`}},
		},
		{
			name: "case folding beyond ASCII",
			cur:  current(f("a", "straße/ǅ.md"), f("b", "straße/ǆ.md")),
			want: []wantProblem{{"b", "straße/ǆ.md", `differs only by case from file "straße/ǅ.md"`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkProblems(t, CheckPacks(tt.cur, tt.optIn), tt.want)
		})
	}
}

func TestCheckPackName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "agents", "python-service", "a1-b2-c3", strings.Repeat("x", 64)} {
		if err := checkPackName(name); err != nil {
			t.Errorf("checkPackName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "A", "a_b", "a--b", "-a", "a-", "a b", "a.b", "é", strings.Repeat("x", 65), "a\n"} {
		if err := checkPackName(name); err == nil {
			t.Errorf("checkPackName(%q): want error", name)
		}
	}
}

func TestSplitHubPath(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in, pack, path string
		ok             bool
	}{
		{"packs/agents/AGENTS.md", "agents", "AGENTS.md", true},
		{"packs/agents/docs/a.md", "agents", "docs/a.md", true},
		{"packs/README.md", "", "", false},
		{"packs", "", "", false},
		{"packs//x", "", "", false},
		{"packs/a/", "", "", false},
		{"other/a/b", "", "", false},
		{"packsx/a/b", "", "", false},
	} {
		pack, path, ok := splitHubPath(tt.in)
		if pack != tt.pack || path != tt.path || ok != tt.ok {
			t.Errorf("splitHubPath(%q) = %q, %q, %v; want %q, %q, %v", tt.in, pack, path, ok, tt.pack, tt.path, tt.ok)
		}
	}
}
