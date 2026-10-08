package marker

import "strings"

// Frame is how a marker line wraps its payload, "touchmark:v1 hub=… fp=…
// stream=… key=… data=…". The payload is the same in every frame.
type Frame uint8

const (
	// FrameComment is an HTML comment, which Markdown renderers hide:
	//
	//	<!-- touchmark:v1 hub=… fp=… stream=… key=… data=… -->
	FrameComment Frame = iota
	// FrameRefDef is a Markdown link reference definition whose title is
	// the payload, which renderers hide too, for platforms that escape HTML
	// in descriptions (Bitbucket Cloud), where a comment would show as
	// text:
	//
	//	[touchmark]: # "touchmark:v1 hub=… fp=… stream=… key=… data=…"
	FrameRefDef
)

// Parts of the frames.
const (
	// commentOpen is what a comment frame puts before the payload.
	commentOpen = "<!-- "
	// refDefOpen is what a reference definition frame puts before the
	// payload, and refDefClose after it.
	refDefOpen  = `[touchmark]: # "`
	refDefClose = `"`
	// payloadPrefix starts the payload of any version.
	payloadPrefix = "touchmark:"
)

// frame returns line, a marker in the comment frame, in frame f.
func frame(line string, f Frame) string {
	if f != FrameRefDef {
		return line
	}
	return refDefOpen + line[len(commentOpen):len(line)-len(commentEnd)] + refDefClose
}

// IsLine reports whether line is a marker line of any version in either
// frame, as Find and Strip see one: it starts with "<!-- touchmark:", or,
// once the backslash escapes Bitbucket adds to an edited description are
// undone, with `[touchmark]: # "touchmark:`. Controls
// ("- [ ] <!-- touchmark:recreate -->") are not marker lines.
func IsLine(line string) bool {
	_, ok := asComment(line)
	return ok
}

// asComment returns line in the comment frame, which Parse and Find read:
// line itself when it starts with "<!-- touchmark:"; for a reference
// definition, `[touchmark]: # "<payload>"` becomes "<!-- <payload> -->"
// (its payload left open when the closing quote is missing, so that Parse
// refuses it). ok is false for any other line. Nothing is trimmed: the
// callers trim the trailing blanks and carriage return they accept.
//
// A reference definition is read after unescape: Bitbucket Cloud returns an
// edited description with backslashes before punctuation ("\[touchmark\]",
// "\_"), which Markdown drops when it renders.
func asComment(line string) (string, bool) {
	if strings.HasPrefix(line, commentPrefix) {
		return line, true
	}
	if !strings.HasPrefix(line, "[") && !strings.HasPrefix(line, `\[`) {
		return "", false
	}
	rest, ok := strings.CutPrefix(unescape(line), refDefOpen)
	if !ok || !strings.HasPrefix(rest, payloadPrefix) {
		return "", false
	}
	if payload, ok := strings.CutSuffix(rest, refDefClose); ok {
		return commentOpen + payload + commentEnd, true
	}
	return commentOpen + rest, true
}

// unescape undoes Markdown's backslash escapes in s: a backslash before
// an ASCII punctuation character is dropped (CommonMark, "Backslash
// escapes"); every other byte stays, a backslash before anything else
// included.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isASCIIPunct reports whether c is one of CommonMark's ASCII punctuation
// characters: !"#$%&'()*+,-./:;<=>?@[\]^_`{|}~.
func isASCIIPunct(c byte) bool {
	return '!' <= c && c <= '/' || ':' <= c && c <= '@' || '[' <= c && c <= '`' || '{' <= c && c <= '~'
}
