package pathx

import (
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// path500 is exactly MaxLen bytes long and otherwise valid.
var path500 = strings.Repeat("abcdefghi/", 49) + "abcdefghij"

var validPaths = []string{
	"AGENTS.md",
	"CLAUDE.md",
	".claude/skills/x/SKILL.md",
	".github/workflows/ci.yml",
	"a/b.c/d",
	".github",
	".gitignore",
	".gitattributes",
	".git-blame-ignore-revs",
	".gitlab-ci.yml",
	".gitlab/ci/lint.yml",
	"x/.gitmodules", // only a top-level .gitmodules is refused
	"x/gitmod~1",
	"gitmod~5",
	".gitmodules.d/x",
	"git",
	"git~2",
	".git~1",
	"a..b",
	"..a",
	" leading-space",
	"a b/c d.md",
	"docs/[x].md",
	"console.txt",
	"com10",
	"COM0",
	"lpt",
	"auxiliary/x",
	"nul_device",
	"docs/привет.md",
	"日本語/ファイル.txt",
	"emoji-\U0001F642.md",
	path500,
}

var invalidPaths = []struct {
	p    string
	want string // substring of the error that names the rule
}{
	{"", "empty"},
	{"/", "slash"},
	{"/a", "slash"},
	{"a/", "slash"},
	{"a//b", "bad segment"},
	{".", "bad segment"},
	{"..", "bad segment"},
	{"./a", "bad segment"},
	{"a/./b", "bad segment"},
	{"../a", "bad segment"},
	{"a/..", "bad segment"},
	{"a/../b", "bad segment"},
	{`a\b`, "contains"},
	{`\a`, "contains"},
	{"C:x", "contains"},
	{"a:b", "contains"},
	{"file.txt::$DATA", "contains"},
	{"docs/what?.md", "Windows does not allow"},
	{"a|b.md", "Windows does not allow"},
	{"a<b>.md", "Windows does not allow"},
	{`say "hi".md`, "Windows does not allow"},
	{"glob*.md", "Windows does not allow"},
	{"a\x00b", "control"},
	{"a\tb", "control"},
	{"a\nb", "control"},
	{"a\x1b[31mb", "control"},
	{"a\x7fb", "control"},
	{"a\u0085b", "control"}, // C1 NEXT LINE
	{"a\u009bb", "control"}, // C1 CONTROL SEQUENCE INTRODUCER
	{"a:\x1b[2J", "control"},
	{"a\xffb", "UTF-8"},
	{"\xc0\xaf", "UTF-8"}, // overlong '/'
	{"a./b", "ends with"},
	{"a /b", "ends with"},
	{"a.", "ends with"},
	{"a ", "ends with"},
	{"docs/x.md.", "ends with"},
	{".git.", "ends with"},
	{"CON", "reserved"},
	{"con.txt", "reserved"},
	{"Con.tar.gz", "reserved"},
	{"con .txt", "reserved"},
	{"aux/b", "reserved"},
	{"x/nul", "reserved"},
	{"PRN.md", "reserved"},
	{"LPT1.log", "reserved"},
	{"com9", "reserved"},
	{"COM\u00b9", "reserved"},
	{"lpt\u00b3.txt", "reserved"},
	{"CONIN$", "reserved"},
	{"conout$.log", "reserved"},
	{".git", ".git"},
	{".GIT/config", ".git"},
	{".Git/hooks/pre-commit", ".git"},
	{"x/.git/y", ".git"},
	{"x/.git", ".git"},
	{"git~1/config", ".git"},
	{"GIT~1/HEAD", ".git"},
	{".g\u200cit/config", ".git"},
	{"\ufeff.git/config", ".git"},
	{".gi\u202et/x", ".git"},
	{".gitmodules", ".gitmodules"},
	{".GITMODULES", ".gitmodules"},
	{".gitmodu\u200dles", ".gitmodules"},
	{"gitmod~1", ".gitmodules"},
	{"GITMOD~4", ".gitmodules"},
	{path500 + "k", "longer"},
	{strings.Repeat("a", MaxLen+1), "longer"},
}

func TestValidateAccepts(t *testing.T) {
	if len(path500) != MaxLen {
		t.Fatalf("path500 has %d bytes, want %d", len(path500), MaxLen)
	}
	for _, p := range validPaths {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", p, err)
			continue
		}
		checkAccepted(t, p)
	}
}

func TestValidateRejects(t *testing.T) {
	for _, tc := range invalidPaths {
		err := Validate(tc.p)
		if err == nil {
			t.Errorf("Validate(%q) = nil, want an error", tc.p)
			continue
		}
		checkRejected(t, tc.p, err)
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%q) = %q, want it to mention %q", tc.p, err, tc.want)
		}
	}
}

// checkAccepted asserts the invariants every accepted path must hold.
func checkAccepted(t *testing.T, p string) {
	t.Helper()
	if len(p) > MaxLen || !utf8.ValidString(p) {
		t.Fatalf("accepted %q: too long or not UTF-8", p)
	}
	if strings.ContainsAny(p, "\\:"+windowsForbidden) || strings.ContainsFunc(p, unicode.IsControl) {
		t.Fatalf("accepted %q: a character Windows forbids, or a control character", p)
	}
	if path.Clean(p) != p {
		t.Fatalf("accepted %q: not clean (path.Clean gives %q)", p, path.Clean(p))
	}
	for i, seg := range strings.Split(p, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			t.Fatalf("accepted %q: bad segment %q", p, seg)
		case IsGitDirName(seg):
			t.Fatalf("accepted %q: segment %q names .git", p, seg)
		case i == 0 && isGitmodulesName(seg):
			t.Fatalf("accepted %q: top-level .gitmodules", p)
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		t.Fatalf("accepted %q: filepath.IsLocal is false on this OS", p)
	}
}

// checkRejected asserts that err is a proper Validate error that is safe to
// print in a log.
func checkRejected(t *testing.T, p string, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(%q) = %v, want it to wrap ErrInvalid", p, err)
	}
	msg := err.Error()
	if !utf8.ValidString(msg) || strings.ContainsFunc(msg, unicode.IsControl) || strings.ContainsFunc(msg, isHFSIgnorable) {
		t.Fatalf("Validate(%q) error %q carries raw control, format or invalid bytes", p, msg)
	}
}

func FuzzValidate(f *testing.F) {
	for _, p := range validPaths {
		f.Add(p)
	}
	for _, tc := range invalidPaths {
		f.Add(tc.p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		err := Validate(p)
		if err != nil {
			checkRejected(t, p, err)
			return
		}
		checkAccepted(t, p)
	})
}

func TestIsGitDirName(t *testing.T) {
	tests := []struct {
		seg  string
		want bool
	}{
		{".git", true},
		{".GIT", true},
		{".Git", true},
		{"git~1", true},
		{"GIT~1", true},
		{".git.", true},
		{".git ", true},
		{".git. . ", true},
		{".g\u200cit", true},
		{"\u200e.git", true},
		{".git\ufeff", true},
		{".\u206agit", true},
		{".github", false},
		{".gitignore", false},
		{".gitmodules", false},
		{"git", false},
		{".git~1", false},
		{"git~2", false},
		{"x.git", false},
		{".gitx", false},
		{"..git", false},
		{".gi t", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := IsGitDirName(tc.seg); got != tc.want {
			t.Errorf("IsGitDirName(%q) = %v, want %v", tc.seg, got, tc.want)
		}
	}
}

func TestIsGitmodulesName(t *testing.T) {
	tests := []struct {
		seg  string
		want bool
	}{
		{".gitmodules", true},
		{".GitModules", true},
		{".gitmodules.", true},
		{".gitmodules ", true},
		{".gitmodu\u200cles", true},
		{"gitmod~1", true},
		{"GITMOD~4", true},
		{"gitmod~2.", true},
		{"gitmod~0", false},
		{"gitmod~5", false},
		{"gitmod~", false},
		{"gitmod~12", false},
		{"gitmodules", false},
		{".gitmodule", false},
		{".gitmodules.bak", false},
	}
	for _, tc := range tests {
		if got := isGitmodulesName(tc.seg); got != tc.want {
			t.Errorf("isGitmodulesName(%q) = %v, want %v", tc.seg, got, tc.want)
		}
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, p string
		want       bool
	}{
		// Exact paths, case-sensitive.
		{"AGENTS.md", "AGENTS.md", true},
		{"AGENTS.md", "agents.md", false},
		{"AGENTS.md", "docs/AGENTS.md", false},
		// A pattern without glob characters covers its subtree.
		{"docs", "docs", true},
		{"docs", "docs/a.md", true},
		{"docs", "docs/x/y.md", true},
		{"docs", "docs2/a.md", false},
		{"docs", "a/docs/b.md", false},
		{"docs/a", "docs/a.md", false},
		{".github", ".github/workflows/ci.yml", true},
		// '*' stays within one segment and disables the subtree rule.
		{"*.md", "README.md", true},
		{"*.md", "docs/a.md", false},
		{"docs/*", "docs/a.md", true},
		{"docs/*", "docs/x/a.md", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/a.txt", false},
		{"*", "a", true},
		{"*", "a/b", false},
		// '**' matches zero or more segments.
		{"**/x", "x", true},
		{"**/x", "a/b/x", true},
		{"**/x", "a/x/b", false},
		{"a/**", "a", true},
		{"a/**", "a/b/c", true},
		{"a/**", "b/a", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"**", "anything/at/all", true},
		{"**/*.md", "a.md", true},
		{"**/*.md", "docs/a/b.md", true},
		{"**/*.md", "docs/a/b.txt", false},
		{"**/**/x", "x", true},
		{".claude/**/SKILL.md", ".claude/skills/x/SKILL.md", true},
		// '?' and character classes.
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "a/c", false},
		{"[ab].md", "a.md", true},
		{"[ab].md", "c.md", false},
		{"[^ab].md", "c.md", true},
		{"[a-c]x", "bx", true},
		// A malformed segment pattern matches only itself.
		{"[", "[", true},
		{"[", "a", false},
		{"docs/[", "docs/[", true},
		{"a[", "a[/b", false},
		// Normalization of hand-written patterns.
		{"./docs/", "docs/a.md", true},
		{"././docs", "docs/a.md", true},
		{"\\docs\\a.md", "docs/a.md", true},
		{" docs ", "docs/a.md", true},
		{"/AGENTS.md", "AGENTS.md", true},
		// An empty pattern matches nothing.
		{"", "a", false},
		{"   ", "a", false},
		{"./", "a", false},
		{"/", "a", false},
	}
	for _, tc := range tests {
		if got := Match(tc.pattern, tc.p); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.p, got, tc.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	if MatchAny(nil, "a") {
		t.Error("MatchAny(nil) = true")
	}
	if !MatchAny([]string{"x", "docs"}, "docs/a.md") {
		t.Error("MatchAny misses the second pattern")
	}
	if MatchAny([]string{"x", "*.md"}, "docs/a.md") {
		t.Error("MatchAny matches no pattern but returns true")
	}
}

func TestMatcher(t *testing.T) {
	var nilMatcher *Matcher
	if nilMatcher.Match("a") || NewMatcher(nil, true).Match("a") || NewMatcher([]string{"", "/", "./"}, false).Match("a") {
		t.Error("an empty matcher matches")
	}
	patterns := []string{"docs", "*.md", "a/**/b", "./x/", "docs"}
	exact := NewMatcher(patterns, false)
	for _, p := range []string{"docs/a.md", "README.md", "a/b", "a/x/y/b", "x/z", "Docs/a", "readme.MD", "q"} {
		if got, want := exact.Match(p), MatchAny(patterns, p); got != want {
			t.Errorf("Match(%q) = %v, MatchAny says %v", p, got, want)
		}
	}
	if len(exact.pats) != 4 {
		t.Errorf("duplicates kept: %d patterns", len(exact.pats))
	}

	folded := NewMatcher([]string{"Docs/**", ".Engineering-Assets.yml", "[A-C]*.TXT"}, true)
	for p, want := range map[string]bool{
		"docs/guide.md":           true,
		"DOCS/Guide.md":           true,
		".engineering-assets.yml": true,
		".ENGINEERING-ASSETS.YML": true,
		"b-notes.txt":             true,
		"d-notes.txt":             false,
		"other.md":                false,
	} {
		if got := folded.Match(p); got != want {
			t.Errorf("folded Match(%q) = %v, want %v", p, got, want)
		}
	}
}

func FuzzMatcher(f *testing.F) {
	f.Add("docs", "docs/a.md")
	f.Add("**/X", "a/b/x")
	f.Add("[", "[")
	f.Fuzz(func(t *testing.T, pattern, p string) {
		if got, want := NewMatcher([]string{pattern}, false).Match(p), Match(pattern, p); got != want {
			t.Fatalf("Matcher(%q).Match(%q) = %v, Match says %v", pattern, p, got, want)
		}
		if got, want := NewMatcher([]string{pattern}, true).Match(p), Match(Fold(pattern), Fold(p)); got != want {
			t.Fatalf("folded Matcher(%q).Match(%q) = %v, want %v", pattern, p, got, want)
		}
	})
}

// TestMatchManyDoubleStars guards against exponential backtracking on
// patterns that come from untrusted target files.
func TestMatchManyDoubleStars(t *testing.T) {
	segs := slices.Repeat([]string{"a"}, 200)
	miss := strings.Join(segs, "/")
	hit := miss + "/z"
	patterns := []string{
		strings.Repeat("**/", 64) + "z",
		strings.Repeat("**/a/", 40) + "z",
		strings.Repeat("**/*/", 40) + "z",
	}
	done := make(chan error, 1)
	go func() {
		for _, pat := range patterns {
			if Match(pat, miss) || !Match(pat, hit) {
				done <- errors.New("wrong result for " + pat[:12] + "…")
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Match backtracks on repeated **")
	}
}

// TestMatchSegmentsExhaustive compares matchSegments with a plain
// backtracking reference on every small pattern and path.
func TestMatchSegmentsExhaustive(t *testing.T) {
	pats := sequences([]string{"a", "b", "*", "**"}, 4)
	paths := sequences([]string{"a", "b"}, 4)
	for _, pat := range pats {
		for _, segs := range paths {
			if got, want := matchSegments(pat, segs), matchSegmentsRef(pat, segs); got != want {
				t.Fatalf("matchSegments(%q, %q) = %v, want %v", pat, segs, got, want)
			}
		}
	}
}

func FuzzMatch(f *testing.F) {
	f.Add("docs", "docs/a.md")
	f.Add("**/x", "a/b/x")
	f.Add("a/**/b", "a/x/y/b")
	f.Add("[", "[")
	f.Add("./docs/*.md", "docs/a.md")
	f.Fuzz(func(t *testing.T, pattern, p string) {
		got := Match(pattern, p) // must not panic
		pat := strings.Split(NormalizePattern(pattern), "/")
		segs := strings.Split(p, "/")
		if len(pat) <= 8 && len(segs) <= 8 && NormalizePattern(pattern) != "" {
			want := matchSegmentsRef(pat, segs) ||
				!HasGlob(pattern) && strings.HasPrefix(p, NormalizePattern(pattern)+"/")
			if got != want {
				t.Fatalf("Match(%q, %q) = %v, reference says %v", pattern, p, got, want)
			}
		}
		if Validate(p) == nil {
			if !Match("**", p) {
				t.Fatalf("Match(\"**\", %q) = false", p)
			}
			// Patterns are trimmed, so only paths that survive trimming work
			// as literal patterns: " a" cannot be addressed exactly.
			literal := !HasGlob(p) && NormalizePattern(p) == p
			if literal && (!Match(p, p) || !Match(p, p+"/x") || Match(p, p+"x")) {
				t.Fatalf("literal pattern %q does not match exactly itself and its subtree", p)
			}
		}
	})
}

// matchSegmentsRef is the obvious backtracking matcher, kept as a reference
// for small inputs.
func matchSegmentsRef(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegmentsRef(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	return len(segs) > 0 && matchSegment(pat[0], segs[0]) && matchSegmentsRef(pat[1:], segs[1:])
}

// sequences returns every sequence of 1 to n elements drawn from alphabet.
func sequences(alphabet []string, n int) [][]string {
	var out [][]string
	level := [][]string{nil}
	for range n {
		var next [][]string
		for _, s := range level {
			for _, a := range alphabet {
				next = append(next, append(slices.Clone(s), a))
			}
		}
		out = append(out, next...)
		level = next
	}
	return out
}

func TestNormalizePattern(t *testing.T) {
	tests := []struct{ in, want string }{
		{"docs", "docs"},
		{"./docs/", "docs"},
		{"././docs", "docs"},
		{"/docs/", "docs"},
		{"\\docs\\a.md", "docs/a.md"},
		{"  docs/a.md  ", "docs/a.md"},
		{"**/x", "**/x"},
		{"./", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := NormalizePattern(tc.in); got != tc.want {
			t.Errorf("NormalizePattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParents(t *testing.T) {
	tests := []struct {
		p    string
		want []string
	}{
		{"a", nil},
		{"a/b", []string{"a"}},
		{"a/b/c", []string{"a", "a/b"}},
		{".claude/skills/x/SKILL.md", []string{".claude", ".claude/skills", ".claude/skills/x"}},
	}
	for _, tc := range tests {
		if got := Parents(tc.p); !slices.Equal(got, tc.want) {
			t.Errorf("Parents(%q) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestUnder(t *testing.T) {
	tests := []struct {
		p, dir string
		want   bool
	}{
		{"a", "a", true},
		{"a/b", "a", true},
		{"a/b/c", "a/b", true},
		{"ab", "a", false},
		{"a/bc", "a/b", false},
		{"a", "a/b", false},
		{"b/a", "a", false},
	}
	for _, tc := range tests {
		if got := Under(tc.p, tc.dir); got != tc.want {
			t.Errorf("Under(%q, %q) = %v, want %v", tc.p, tc.dir, got, tc.want)
		}
	}
}

func TestFold(t *testing.T) {
	same := [][2]string{
		{"README.md", "readme.MD"},
		{".GitHub/Workflows/CI.yml", ".github/workflows/ci.yml"},
		{"Привет.md", "привет.md"},
		{"\u0131", "I"}, // dotless i: NTFS upcases it to I
		{"\u017f", "s"}, // long s: NTFS upcases it to S
	}
	for _, tc := range same {
		if Fold(tc[0]) != Fold(tc[1]) {
			t.Errorf("Fold(%q) = %q, Fold(%q) = %q, want equal", tc[0], Fold(tc[0]), tc[1], Fold(tc[1]))
		}
	}
	if Fold("a.md") == Fold("b.md") {
		t.Error("Fold merges different names")
	}
	if got := Fold("AGENTS.md"); got != "agents.md" {
		t.Errorf("Fold(%q) = %q, want %q", "AGENTS.md", got, "agents.md")
	}
}

func TestHasGlob(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"docs", false},
		{"docs/a.md", false},
		{"a]", false},
		{"*", true},
		{"**", true},
		{"a?b", true},
		{"[ab]", true},
		{"docs/*.md", true},
	}
	for _, tc := range tests {
		if got := HasGlob(tc.pattern); got != tc.want {
			t.Errorf("HasGlob(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}
