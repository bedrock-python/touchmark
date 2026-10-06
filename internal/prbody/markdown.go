package prbody

import (
	"regexp"
	"strings"
	"unicode"
)

// Code returns s as an inline code span that renders literally on GitHub,
// GitLab, Gitea and Forgejo: control characters removed, the fence longer
// than any backtick run in s, padded with spaces when s starts or ends with
// a backtick.
//
// "Control characters" are the C0 and C1 controls (line breaks and tabs
// included), the line and paragraph separators, and the bidirectional
// formatting characters, which reorder text on screen and could make one
// path look like another. Invalid UTF-8 becomes U+FFFD. The span is also
// padded when s both starts and ends with a space (and is not all spaces),
// because CommonMark strips one space from each end of such content. The
// empty string gives a span of one space, the closest to nothing a code
// span can show.
//
// Inside a code span nothing is markup: no mention, reference, emphasis,
// entity or HTML. In a table cell use cellCode, which also escapes "|".
func Code(s string) string {
	s = strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return "` `"
	}
	fence := strings.Repeat("`", longestRun(s, '`')+1)
	if s[0] == '`' || s[len(s)-1] == '`' || (s[0] == ' ' && s[len(s)-1] == ' ' && strings.Trim(s, " ") != "") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// cellCode is Code for a GFM table cell: every "|" is escaped as "\|".
// Tables split cells at unescaped pipes before they parse code spans, and
// then drop the backslash of "\|" (inside code spans too), so the span
// shows the text as it is (GFM spec, example 200; goldmark does the same).
func cellCode(s string) string {
	return strings.ReplaceAll(Code(s), "|", `\|`)
}

// isControl reports whether Code drops r.
func isControl(r rune) bool {
	return unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || isBidi(r)
}

// isBidi reports whether r is a bidirectional formatting character.
func isBidi(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// longestRun returns the length of the longest run of c in s.
func longestRun(s string, c byte) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// runLen returns the length of the run of s[i] starting at i.
func runLen(s string, i int) int {
	j := i
	for j < len(s) && s[j] == s[i] {
		j++
	}
	return j - i
}

// startsWithSlash reports whether line starts with "/" after leading
// whitespace and invisible format characters: GitLab would run it as a
// quick action. (GitLab itself wants the "/" in the first column; the
// assertion is stricter on purpose.)
func startsWithSlash(line string) bool {
	t := strings.TrimLeftFunc(line, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) })
	return strings.HasPrefix(t, "/")
}

// atEntityRe matches the character references that decode to "@".
var atEntityRe = regexp.MustCompile(`^&(?:#0*64|#[xX]0*40|commat);`)

// mentionAt returns the byte offset of the first "@" in line that could
// start a mention, or -1. It reads one line of Markdown that touchmark
// generated:
//   - an "@" inside a code span (CommonMark rules, within the line) is safe;
//   - an "@" right after an ASCII letter, digit or "_" is safe: GitHub
//     wants a non-word character before a mention, GitLab no word character,
//     Gitea a blank or a bracket;
//   - a backslash-escaped "@" and a character reference that decodes to
//     "@" count as "@";
//   - after a "<" that may open an HTML tag or an autolink, code spans are
//     no longer recognized, since those take precedence over code spans;
//   - an "@" (escaped or not) followed by a word joiner (U+2060) is safe:
//     no platform takes it for the start of a mention (Inert writes it).
func mentionAt(line string) int {
	spans := true
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line) && isASCIIPunct(line[i+1]):
			if line[i+1] == '@' && !joined(line, i+1) {
				return i + 1
			}
			i += 2
		case c == '`' && spans:
			n := runLen(line, i)
			if end := closingRun(line, i+n, n); end >= 0 {
				i = end + n
				continue
			}
			i += n
		case c == '<':
			if i+1 < len(line) && isTagStart(line[i+1]) {
				spans = false
			}
			i++
		case c == '@' && joined(line, i):
			i++
		case c == '@' || (c == '&' && atEntityRe.MatchString(line[i:])):
			if i == 0 || !isWordByte(line[i-1]) {
				return i
			}
			i++
		default:
			i++
		}
	}
	return -1
}

// joined reports whether the "@" at line[at] is followed by a word joiner.
func joined(line string, at int) bool {
	return strings.HasPrefix(line[at+1:], string(wordJoiner))
}

// closingRun returns where the first run of exactly n backticks at or after
// from starts, or -1.
func closingRun(line string, from, n int) int {
	for j := from; j < len(line); {
		if line[j] != '`' {
			j++
			continue
		}
		m := runLen(line, j)
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

// isTagStart reports whether c after "<" may start an HTML tag, comment,
// declaration or autolink.
func isTagStart(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '/' || c == '!' || c == '?'
}

// isWordByte reports whether c is an ASCII letter, digit or "_".
func isWordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// isASCIIPunct reports whether c is ASCII punctuation, which a backslash
// escapes in CommonMark.
func isASCIIPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}
