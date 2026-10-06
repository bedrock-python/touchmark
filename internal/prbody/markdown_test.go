package prbody

import (
	"strings"
	"testing"
)

func TestCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "` `"},
		{"a", "`a`"},
		{"AGENTS.md", "`AGENTS.md`"},
		{"a`b", "``a`b``"},
		{"a``b`", "``` a``b` ```"},
		{"`", "`` ` ``"},
		{"``", "``` `` ```"},
		{"`a`", "`` `a` ``"},
		{" a ", "`  a  `"},
		{" a", "` a`"},
		{"a ", "`a `"},
		{" ", "` `"},
		{"   ", "`   `"},
		{"a\nb", "`ab`"},
		{"a\r\n\tb\x00c\x7f\xc2\x85", "`abc`"},
		{"\x1b[31mred", "`[31mred`"},
		{"a" + string(rune(0x202e)) + "b" + string(rune(0x2066)) + "c", "`abc`"},
		{"a" + string(rune(0x2028)) + "b" + string(rune(0x2029)), "`ab`"},
		{"\n", "` `"},
		{string([]byte{'a', 0xff, 'b'}), "`a" + string(rune(0xfffd)) + "b`"},
		{"@user", "`@user`"},
		{"<b>&amp;</b>", "`<b>&amp;</b>`"},
		{`a\`, "`a\\`"},
	}
	for _, tc := range cases {
		got := Code(tc.in)
		if got != tc.want {
			t.Errorf("Code(%q) = %q, want %q", tc.in, got, tc.want)
		}
		s := codeSpans(got)
		if len(s) != 1 || s[0].start != 0 || s[0].end != len(got) || s[0].text != shown(tc.in) {
			t.Errorf("Code(%q) = %q parses as %+v, want one span showing %q", tc.in, got, s, shown(tc.in))
		}
	}
}

func TestCellCode(t *testing.T) {
	for _, s := range []string{"a|b", `a\|b`, "|", "`|`", `\`, "a||b", "x|"} {
		row := "| create | " + cellCode(s) + " | `p` |"
		cells := gfmCells(row)
		if len(cells) != 3 {
			t.Errorf("%q: row %q has %d cells", s, row, len(cells))
			continue
		}
		sp := codeSpans(cells[1])
		if len(sp) != 1 || sp[0].start != 0 || sp[0].end != len(cells[1]) || sp[0].text != shown(s) {
			t.Errorf("%q: cell %q parses as %+v", s, cells[1], sp)
		}
	}
}

func TestSensitive(t *testing.T) {
	yes := []struct{ path, mode string }{
		{".github/workflows/ci.yml", "100644"},
		{".github/workflows/sub/dir/x.yaml", ""},
		{".github/actions/setup/action.yml", "100644"},
		{".github/dependabot.yml", "100644"},
		{".gitlab-ci.yml", "100644"},
		{".gitlab/ci/build.yml", "100644"},
		{".gitlab/build.yml", "100644"},
		{".gitea/workflows/ci.yml", "100644"},
		{".forgejo/workflows/ci.yml", "100644"},
		{".claude/settings.json", "100644"},
		{".claude/settings.local.json", "100644"},
		{".claude/hooks/pre.sh", "100644"},
		{".claude/agents/reviewer.md", "100644"},
		{".claude/skills/commit/SKILL.md", "100644"},
		{".claude/skills/commit/scripts/run.py", "100644"},
		{".claude/commands/x.md", "100644"},
		{".agents/skills/review/SKILL.md", "100644"},
		{".github/skills/review/SKILL.md", "100644"},
		{".mcp.json", "100644"},
		{"CODEOWNERS", "100644"},
		{".github/CODEOWNERS", "100644"},
		{"docs/CODEOWNERS", "100644"},
		{".gitattributes", "100644"},
		{"renovate.json", "100644"},
		{"renovate.json5", "100644"},
		{".pre-commit-config.yaml", "100644"},
		{".devcontainer/devcontainer.json", "100644"},
		{".vscode/tasks.json", "100644"},
		{"scripts/anything", "100755"},
		{"deploy/prod/values.yaml", "100644"},       // extra
		{".GitHub/Workflows/ci.yml", "100644"},      // case variant
		{".github/workflows/removed.yml", "000000"}, // a deletion
	}
	for _, tc := range yes {
		if !Sensitive(tc.path, tc.mode, []string{"deploy/**"}) {
			t.Errorf("Sensitive(%q, %q) = false", tc.path, tc.mode)
		}
	}
	no := []struct{ path, mode string }{
		{"AGENTS.md", "100644"},
		{".github/ISSUE_TEMPLATE/bug.md", "100644"},
		{".gitlab/issue_templates/bug.md", "100644"},
		{".claude/settings", "100644"},
		{".claude/skills.md", "100644"},
		{".agents/project.md", "100644"},
		{".agents/prompts/review.md", "100644"},
		{"CODEOWNERS.md", "100644"},
		{"docs/.gitattributes.md", "100644"},
		{"deploy/prod/values.yaml", "100644"}, // no extra patterns here
		{"scripts/x.sh", "100644"},
		{"scripts/x.sh", ""},
	}
	for _, tc := range no {
		if Sensitive(tc.path, tc.mode, nil) {
			t.Errorf("Sensitive(%q, %q) = true", tc.path, tc.mode)
		}
	}
	// The builtin list is not modified by extra patterns.
	n := len(BuiltinSensitive)
	for range 3 {
		Sensitive("a", "", []string{"x/**", "y/**"})
	}
	if len(BuiltinSensitive) != n || strings.Contains(strings.Join(BuiltinSensitive, " "), "x/**") {
		t.Error("BuiltinSensitive changed")
	}
}

func TestMentionAt(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"no at", -1},
		{"@a", 0},
		{"x @a", 2},
		{"mail@example.com", -1},
		{"x_@a", -1},
		{"`@a`", -1},
		{"`@a` @b", 5},
		{"``@a` x``", -1},
		{"``@a`", 2},
		{"\\`@a`", 2},
		{"\\@a", 1},
		{"&#64;a", 0},
		{"&#0064;a", 0},
		{"&#X40;a", 0},
		{"&commat;a", 0},
		{"x&#64;a", -1},
		{"&#65;", -1},
		{"<b>`@a`</b>", 4},
		{"`<b>` `@a`", -1},
		{"a < b `@a`", -1},
		{"| `a\\|@b` |", -1},
		{"é@a", 2},
	}
	for _, tc := range cases {
		if got := mentionAt(tc.line); got != tc.want {
			t.Errorf("mentionAt(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func TestStartsWithSlash(t *testing.T) {
	for _, line := range []string{"/close", " /close", "\t/x", string(rune(0x3000)) + "/x", string(rune(0x200b)) + "/x", string(rune(0xfeff)) + " /x", "\v/x"} {
		if !startsWithSlash(line) {
			t.Errorf("startsWithSlash(%q) = false", line)
		}
	}
	for _, line := range []string{"", "a/b", "- /x", "`/x`", "\\/x", "> /x", "|/x"} {
		if startsWithSlash(line) {
			t.Errorf("startsWithSlash(%q) = true", line)
		}
	}
}
