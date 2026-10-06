// Package pathx validates and matches repository-relative paths.
//
// A repository-relative path uses '/' as the separator, has no leading or
// trailing slash, and no empty, "." or ".." segments. Every path touchmark
// writes into a target goes through Validate first.
package pathx

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLen is the longest path a pack may ship.
const MaxLen = 500

// ErrInvalid is wrapped by every error Validate returns.
var ErrInvalid = errors.New("invalid repository path")

// Validate reports whether p is a path touchmark may manage in a target.
//
// It rejects absolute paths, backslashes, empty, "." and ".." segments, ':'
// (NTFS alternate data streams and drive prefixes), the other characters
// Windows does not allow in names (< > " | ? *), invalid UTF-8, control
// characters, segments ending in a dot or a space (Windows strips them),
// Windows reserved device names, anything inside .git (in any case and in its
// NTFS and HFS+ aliases), a top-level .gitmodules (likewise), and paths longer
// than MaxLen. Error messages quote p, so they are safe to print.
func Validate(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty", ErrInvalid)
	}
	if len(p) > MaxLen {
		return fmt.Errorf("%w: longer than %d bytes: %.40q…", ErrInvalid, MaxLen, p)
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("%w: not valid UTF-8: %q", ErrInvalid, p)
	}
	if strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: control character: %q", ErrInvalid, p)
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("%w: leading or trailing slash: %q", ErrInvalid, p)
	}
	if strings.ContainsAny(p, "\\:") {
		return fmt.Errorf("%w: contains '\\' or ':': %q", ErrInvalid, p)
	}
	if i := strings.IndexAny(p, windowsForbidden); i >= 0 {
		return fmt.Errorf("%w: contains %q, which Windows does not allow in names: %q", ErrInvalid, p[i:i+1], p)
	}
	for i, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".", "..":
			return fmt.Errorf("%w: bad segment %q: %q", ErrInvalid, seg, p)
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return fmt.Errorf("%w: segment ends with a dot or space: %q", ErrInvalid, p)
		}
		if isWindowsReserved(seg) {
			return fmt.Errorf("%w: Windows reserved name %q: %q", ErrInvalid, seg, p)
		}
		if IsGitDirName(seg) {
			return fmt.Errorf("%w: inside .git: %q", ErrInvalid, p)
		}
		if i == 0 && isGitmodulesName(seg) {
			// Not forbidden as such, but never managed: it would turn into submodules.
			return fmt.Errorf("%w: .gitmodules is never managed: %q", ErrInvalid, p)
		}
	}
	return nil
}

// windowsForbidden holds the printable characters Windows rejects in file
// names besides '\' and ':', which Validate reports on their own.
const windowsForbidden = `<>"|?*`

// IsGitDirName reports whether a single path segment names the .git directory
// on some filesystem: ".git" in any case, the NTFS short name "git~1", and
// HFS+ spellings that differ only by ignorable code points.
func IsGitDirName(seg string) bool {
	s := foldSegment(seg)
	return s == ".git" || s == "git~1"
}

// isGitmodulesName reports whether seg names .gitmodules on some filesystem,
// by the same rules as IsGitDirName, with the NTFS short names "gitmod~1" to
// "gitmod~4" that git itself treats as aliases.
func isGitmodulesName(seg string) bool {
	s := foldSegment(seg)
	if s == ".gitmodules" {
		return true
	}
	return len(s) == len("gitmod~1") && strings.HasPrefix(s, "gitmod~") && s[7] >= '1' && s[7] <= '4'
}

// foldSegment returns seg as NTFS and HFS+ compare it: lower case, without
// HFS+ ignorable code points and without trailing dots and spaces.
func foldSegment(seg string) string {
	s := strings.Map(func(r rune) rune {
		if isHFSIgnorable(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, seg)
	return strings.TrimRight(s, ". ")
}

// isHFSIgnorable lists the code points HFS+ ignores when comparing names
// (the same set git uses in is_hfs_dotgit).
func isHFSIgnorable(r rune) bool {
	switch r {
	case 0x200c, 0x200d, 0x200e, 0x200f,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f,
		0xfeff:
		return true
	}
	return false
}

// windowsReserved holds the device names Windows reserves in every
// directory, in lower case. Windows reads the superscript digits ¹, ² and ³
// as digits, so "COM¹" is a device too.
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"com¹": true, "com²": true, "com³": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	"lpt¹": true, "lpt²": true, "lpt³": true,
	"conin$": true, "conout$": true,
}

// isWindowsReserved reports whether seg is a reserved device name, with or
// without an extension.
func isWindowsReserved(seg string) bool {
	base := strings.ToLower(seg)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return windowsReserved[strings.TrimRight(base, " ")]
}

// Fold returns the case-folded form of p used to detect paths that collide
// on case-insensitive filesystems (NTFS, APFS by default). It maps through
// upper case first, so letters such as 'ı' and 'ſ' fold together with the
// 'I' and 'S' that NTFS equates them with.
func Fold(p string) string { return strings.ToLower(strings.ToUpper(p)) }

// Parents returns the ancestors of p from the top down: "a/b/c" gives
// ["a", "a/b"]. A top-level path has no parents.
func Parents(p string) []string {
	var out []string
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return out
}

// Under reports whether p equals dir or lies beneath it.
func Under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// NormalizePattern trims the forms people write in ignore lists: a leading
// "./", surrounding slashes, backslashes and spaces.
func NormalizePattern(pattern string) string {
	p := strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return strings.Trim(p, "/")
}

// HasGlob reports whether pattern contains glob metacharacters.
func HasGlob(pattern string) bool { return strings.ContainsAny(pattern, "*?[") }

// Match reports whether the repository path p matches pattern.
//
// Patterns match whole segments. "*", "?" and "[...]" work inside one
// segment as in path.Match. "**" matches zero or more segments. A pattern
// without glob characters also matches everything beneath it, so "docs"
// matches "docs/a.md". A malformed segment pattern matches only itself
// literally. Matching takes time proportional to the number of pattern
// segments times the number of path segments, so patterns from untrusted
// files are safe to use.
func Match(pattern, p string) bool {
	pat := NormalizePattern(pattern)
	if pat == "" {
		return false
	}
	if matchSegments(strings.Split(pat, "/"), strings.Split(p, "/")) {
		return true
	}
	if !HasGlob(pat) && strings.HasPrefix(p, pat+"/") {
		return true
	}
	return false
}

// MatchAny reports whether p matches any of patterns.
func MatchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if Match(pat, p) {
			return true
		}
	}
	return false
}

// Matcher matches paths against a list of patterns with the semantics of
// Match. Each pattern is normalized and split once, and duplicates are
// dropped, so matching many paths against a long list from an untrusted
// file costs no repeated parsing.
type Matcher struct {
	fold bool
	pats []compiled
}

// compiled is one normalized pattern.
type compiled struct {
	segs []string
	// subtree is set for a pattern without glob characters: it also matches
	// every path beneath it.
	subtree string
}

// NewMatcher prepares patterns. With foldCase set, patterns and paths
// compare case-insensitively (through Fold), as NTFS and APFS compare
// names: "Docs/**" then also matches "docs/a.md". Empty patterns are
// dropped.
func NewMatcher(patterns []string, foldCase bool) *Matcher {
	m := &Matcher{fold: foldCase}
	seen := map[string]bool{}
	for _, pattern := range patterns {
		if foldCase {
			pattern = Fold(pattern)
		}
		pat := NormalizePattern(pattern)
		if pat == "" || seen[pat] {
			continue
		}
		seen[pat] = true
		c := compiled{segs: strings.Split(pat, "/")}
		if !HasGlob(pat) {
			c.subtree = pat + "/"
		}
		m.pats = append(m.pats, c)
	}
	return m
}

// Match reports whether p matches any of the patterns. A nil Matcher
// matches nothing.
func (m *Matcher) Match(p string) bool {
	if m == nil || len(m.pats) == 0 {
		return false
	}
	if m.fold {
		p = Fold(p)
	}
	segs := strings.Split(p, "/")
	for _, c := range m.pats {
		if matchSegments(c.segs, segs) || (c.subtree != "" && strings.HasPrefix(p, c.subtree)) {
			return true
		}
	}
	return false
}

// matchSegments reports whether segs matches the segment patterns pat, where
// "**" stands for zero or more segments. It fills the table "pat[i:] matches
// segs[j:]" one pattern segment at a time, from the end, so a pattern with
// many "**" cannot make it backtrack exponentially.
func matchSegments(pat, segs []string) bool {
	n := len(segs)
	next := make([]bool, n+1) // row i+1: pat[i+1:] matches segs[j:]
	cur := make([]bool, n+1)  // row i: pat[i:] matches segs[j:]
	next[n] = true            // the empty pattern matches only the empty tail
	for i := len(pat) - 1; i >= 0; i-- {
		if pat[i] == "**" {
			cur[n] = next[n]
			for j := n - 1; j >= 0; j-- {
				cur[j] = next[j] || cur[j+1]
			}
		} else {
			cur[n] = false
			for j := n - 1; j >= 0; j-- {
				cur[j] = next[j+1] && matchSegment(pat[i], segs[j])
			}
		}
		cur, next = next, cur
	}
	return next[0]
}

// matchSegment matches one path segment against one segment pattern as
// path.Match does. A malformed pattern matches only itself.
func matchSegment(pat, seg string) bool {
	ok, err := path.Match(pat, seg)
	if err != nil {
		return pat == seg
	}
	return ok
}
