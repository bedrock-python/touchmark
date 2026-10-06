package prbody

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CheckIntro reports whether intro, the content of pr.intro_file, may open
// every body; Render applies the same rules. The intro is Markdown the hub
// writes, the same in every target, so it is held to rules simple enough to
// check without a Markdown parser:
//   - valid UTF-8 without control characters other than tabs and line
//     breaks (CRLF and CR count as line breaks);
//   - no line that starts with "/" after leading blanks: GitLab would run
//     it as a quick action, from every target;
//   - no "@" (nor a character reference to one) except right after an ASCII
//     letter, digit or "_", as in an e-mail address, and not even in code:
//     a mention in hundreds of pull requests is a notification storm;
//   - no "<!-- touchmark:" comment, the form of markers and controls: a
//     ticked control copied into the intro would act in every target;
//   - every code fence and HTML comment closed, every CommonMark HTML
//     block that only its own end condition ends (a line starting with
//     <pre, <script, <style or <textarea, <!--, <?, <! and a letter, or
//     <![CDATA[) ended within the intro, and the HTML elements that can
//     hide or swallow what follows (blockquote, code, details, div, pre,
//     script, style, summary, table, textarea) closed after they are
//     opened, as often as they are opened, so that the sections after the
//     intro, the ⚠ section first, render as touchmark wrote them.
//
// Errors wrap ErrIntro, and ErrUnsafe for "/" and "@"; they name the line.
func CheckIntro(intro string) error {
	_, err := prepareIntro(intro)
	return err
}

// prepareIntro checks intro (CheckIntro) and returns it as Render writes it:
// line breaks as "\n", without leading blank lines and trailing blanks.
func prepareIntro(intro string) (string, error) {
	s := strings.ReplaceAll(intro, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrIntro)
	}
	var open *fenceLine
	var html *htmlBlock
	for i, line := range strings.Split(s, "\n") {
		n := i + 1
		if j := strings.IndexFunc(line, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }); j >= 0 {
			r, _ := utf8.DecodeRuneInString(line[j:])
			return "", fmt.Errorf("%w: line %d: control character %U", ErrIntro, n, r)
		}
		if startsWithSlash(line) {
			return "", fmt.Errorf("%w: %w: line %d starts with %q, which GitLab runs as a quick action", ErrIntro, ErrUnsafe, n, "/")
		}
		if at := bareAt(line); at >= 0 {
			return "", fmt.Errorf("%w: %w: line %d: the %q at byte %d could mention someone; touchmark never mentions anyone (keep %q only inside a word, as in an e-mail address)", ErrIntro, ErrUnsafe, n, "@", at+1, "@")
		}
		switch {
		case open != nil:
			if open.closedBy(line) {
				open = nil
			}
		case html != nil:
			if html.endsOn(line) {
				html = nil
			}
		default:
			if f, ok := openFence(line); ok {
				f.line = n
				open = &f
			} else if b, ok := openHTMLBlock(line); ok && !b.endsOn(line) {
				b.line = n
				html = &b
			}
		}
	}
	if open != nil {
		return "", fmt.Errorf("%w: the code fence of line %d is never closed", ErrIntro, open.line)
	}
	if html != nil {
		return "", fmt.Errorf("%w: the %s HTML block of line %d never ends (no %s after it): the sections after the intro would render inside it",
			ErrIntro, html.start, html.line, html.what())
	}
	// Across lines too: the blanks after "<!--" may include line breaks.
	if loc := reservedRe.FindStringIndex(s); loc != nil {
		return "", fmt.Errorf("%w: line %d: %q comments are reserved for touchmark's marker and controls", ErrIntro, strings.Count(s[:loc[0]], "\n")+1, "<!-- touchmark:")
	}
	if n, ok := unclosedComment(s); ok {
		return "", fmt.Errorf("%w: the HTML comment of line %d is never closed", ErrIntro, n)
	}
	if msg, ok := unbalancedTag(s); ok {
		return "", fmt.Errorf("%w: %s", ErrIntro, msg)
	}
	return trimBlankLines(s), nil
}

// htmlBlock is an open CommonMark HTML block of types 1 to 5: the ones a
// blank line does not end (CommonMark 0.31, section 4.6). Left open, such a
// block swallows everything after it as raw HTML.
type htmlBlock struct {
	start string   // what starts it, for messages: "<pre>", "<?", …
	ends  []string // any of them on a line ends the block
	fold  bool     // ends compare ignoring case
	line  int      // where it started
}

// htmlRawTags start type 1 blocks, which end at a closing tag of any of
// them.
var htmlRawTags = []string{"pre", "script", "style", "textarea"}

// openHTMLBlock reports whether line starts an HTML block of types 1 to 5:
// at most three spaces of indentation, then <pre, <script, <style or
// <textarea (any case) followed by a blank, ">" or the end of the line;
// "<!--"; "<?"; "<![CDATA["; or "<!" and an ASCII letter.
func openHTMLBlock(line string) (htmlBlock, bool) {
	rest, ok := cutIndent(line)
	if !ok || !strings.HasPrefix(rest, "<") {
		return htmlBlock{}, false
	}
	for _, t := range htmlRawTags {
		if len(rest) > len(t) && strings.EqualFold(rest[1:1+len(t)], t) {
			if tail := rest[1+len(t):]; tail == "" || strings.IndexByte(" \t>", tail[0]) >= 0 {
				ends := make([]string, len(htmlRawTags))
				for i, e := range htmlRawTags {
					ends[i] = "</" + e + ">"
				}
				return htmlBlock{start: "<" + t + ">", ends: ends, fold: true}, true
			}
		}
	}
	switch {
	case strings.HasPrefix(rest, "<!--"):
		return htmlBlock{start: "<!--", ends: []string{"-->"}}, true
	case strings.HasPrefix(rest, "<?"):
		return htmlBlock{start: "<?", ends: []string{"?>"}}, true
	case strings.HasPrefix(rest, "<![CDATA["):
		return htmlBlock{start: "<![CDATA[", ends: []string{"]]>"}}, true
	case len(rest) > 2 && rest[1] == '!' && ('a' <= rest[2]|0x20 && rest[2]|0x20 <= 'z'):
		return htmlBlock{start: "<!" + rest[2:3], ends: []string{">"}}, true
	}
	return htmlBlock{}, false
}

// endsOn reports whether line meets the block's end condition. The line
// that starts a block counts too: it may end it.
func (b htmlBlock) endsOn(line string) bool {
	if b.fold {
		line = strings.ToLower(line)
	}
	return slices.ContainsFunc(b.ends, func(end string) bool { return strings.Contains(line, end) })
}

// what names the end conditions for a message.
func (b htmlBlock) what() string {
	quoted := make([]string, len(b.ends))
	for i, e := range b.ends {
		quoted[i] = strconv.Quote(e)
	}
	return strings.Join(quoted, " or ")
}

// reservedRe matches the comments touchmark writes: markers and controls.
var reservedRe = regexp.MustCompile(`(?i)<!--\s*touchmark:`)

// bareAt returns the byte offset of the first "@" (or character reference
// to "@") in line that is not right after an ASCII letter, digit or "_", or
// -1. Code spans do not protect it: see CheckIntro.
func bareAt(line string) int {
	for i := 0; i < len(line); i++ {
		if c := line[i]; (c == '@' || (c == '&' && atEntityRe.MatchString(line[i:]))) && (i == 0 || !isWordByte(line[i-1])) {
			return i
		}
	}
	return -1
}

// fenceLine is an open fenced code block.
type fenceLine struct {
	char byte // '`' or '~'
	n    int  // fence length
	line int  // where it opened
}

// openFence reports whether line opens a fenced code block (CommonMark: at
// most three spaces of indentation, then at least three backticks whose
// info string has no backtick, or at least three tildes). Containers
// (quotes, list items) are not followed: their fences close with them.
func openFence(line string) (fenceLine, bool) {
	rest, ok := cutIndent(line)
	if !ok || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return fenceLine{}, false
	}
	n := runLen(rest, 0)
	if n < 3 || (rest[0] == '`' && strings.IndexByte(rest[n:], '`') >= 0) {
		return fenceLine{}, false
	}
	return fenceLine{char: rest[0], n: n}, true
}

// closedBy reports whether line closes f: at most three spaces, at least as
// many fence characters, then only blanks.
func (f fenceLine) closedBy(line string) bool {
	rest, ok := cutIndent(line)
	if !ok || rest == "" || rest[0] != f.char {
		return false
	}
	n := runLen(rest, 0)
	return n >= f.n && strings.Trim(rest[n:], " \t") == ""
}

// cutIndent removes up to three leading spaces; ok is false when the line
// is indented further (by four spaces or a tab), which makes it code or
// continuation text rather than a fence.
func cutIndent(line string) (string, bool) {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i == 4 || (i < len(line) && line[i] == '\t') {
		return "", false
	}
	return line[i:], true
}

// unclosedComment reports the line of the first "<!--" with no "-->" after
// it. "<!-->" and "<!--->" are whole (empty) comments, as HTML parsers and
// CommonMark 0.31 read them: the "-->" of a comment may overlap its "<!--".
func unclosedComment(s string) (int, bool) {
	for i := 0; ; {
		j := strings.Index(s[i:], "<!--")
		if j < 0 {
			return 0, false
		}
		start := i + j
		body := s[start+len("<!--"):]
		switch {
		case strings.HasPrefix(body, ">"):
			i = start + len("<!-->")
			continue
		case strings.HasPrefix(body, "->"):
			i = start + len("<!--->")
			continue
		}
		k := strings.Index(body, "-->")
		if k < 0 {
			return strings.Count(s[:start], "\n") + 1, true
		}
		i = start + len("<!--") + k + len("-->")
	}
}

// guardedTags are the HTML elements an intro must close: left open, they
// would hide (details, comment-like script and style), swallow (pre,
// textarea, code) or restructure (blockquote, div, summary, table) the
// sections after the intro.
var guardedTags = []string{"blockquote", "code", "details", "div", "pre", "script", "style", "summary", "table", "textarea"}

// unbalancedTag reports, for the first guarded element (ignoring case)
// that is closed before it is opened or left open at the end, why. Tags
// count in order: "</details> … <details>" leaves one open even though
// both appear once.
func unbalancedTag(s string) (string, bool) {
	lower := strings.ToLower(s)
	for _, t := range guardedTags {
		depth := 0
		for i := 0; i < len(lower); i++ {
			if lower[i] != '<' {
				continue
			}
			switch {
			case isTag(lower[i+1:], "/"+t):
				if depth--; depth < 0 {
					return fmt.Sprintf("</%s> on line %d closes a <%s> that is not open", t, strings.Count(s[:i], "\n")+1, t), true
				}
			case isTag(lower[i+1:], t):
				depth++
			}
		}
		if depth != 0 {
			return fmt.Sprintf("<%s> is opened and never closed", t), true
		}
	}
	return "", false
}

// isTag reports whether s starts with name followed by a blank, "/", ">"
// or the end, so that "pre" does not match "prefix".
func isTag(s, name string) bool {
	rest, ok := strings.CutPrefix(s, name)
	return ok && (rest == "" || strings.IndexByte(" \t\n/>", rest[0]) >= 0)
}

// trimBlankLines drops leading blank lines and trailing whitespace.
func trimBlankLines(s string) string {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	for {
		line, rest, ok := strings.Cut(s, "\n")
		if !ok || strings.TrimSpace(line) != "" {
			return s
		}
		s = rest
	}
}
