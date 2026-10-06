package prbody

import (
	"fmt"
	"strings"
)

// wordJoiner is the invisible character Inert puts after an "@" that could
// mention someone: no platform takes "@⁠name" for a mention, and it shows
// as "@name".
const wordJoiner = '⁠'

// Inert returns text, a description touchmark writes back with what people
// wrote in it (a close, an ack, a consumed recreate), so that none of its
// lines acts when touchmark writes it:
//   - line breaks become "\n" (Markdown and GitLab read a lone "\r" as one
//     too, which a check that splits at "\n" would miss);
//   - a line that starts with "/" after blanks and invisible format
//     characters gets a backslash before the "/": GitLab would run it as a
//     quick action of touchmark's, through the API too; Markdown shows the
//     escaped "/" as it was;
//   - every "@" that could mention someone (as the renderer's check reads
//     it) gets a word joiner after it: touchmark mentions no one, not even
//     with a line that an edit of the body brought out of an HTML comment.
//
// Everything else keeps its bytes, so Inert of a body touchmark rendered is
// that body, and CheckText accepts what Inert returns.
func Inert(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if startsWithSlash(line) {
			j := strings.IndexByte(line, '/')
			line = line[:j] + `\` + line[j:]
		}
		lines[i] = inertMentions(line)
	}
	return strings.Join(lines, "\n")
}

// inertMentions puts a word joiner after every "@" of line that could start
// a mention (mentionAt), one at a time: the whole line is read again after
// each, as mentionAt reads it from its start. A character reference to "@"
// becomes the character itself first, so the joiner follows what renders
// as "@".
func inertMentions(line string) string {
	for {
		at := mentionAt(line)
		if at < 0 {
			return line
		}
		if line[at] == '&' {
			end := at + strings.IndexByte(line[at:], ';') + 1
			line = line[:at] + "@" + line[end:]
		}
		line = line[:at+1] + string(wordJoiner) + line[at+1:]
	}
}

// CheckText checks text touchmark is about to write to a platform (a body or
// a comment), whatever made it: it holds no carriage return, no line that
// starts with "/" after blanks (GitLab would run it as a quick action of
// touchmark's) and no "@" that could mention someone. Errors wrap ErrUnsafe
// and name the line. Render's output passes it, and so does Inert's.
func CheckText(text string) error {
	if strings.IndexByte(text, '\r') >= 0 {
		return fmt.Errorf("%w: a carriage return, which Markdown reads as a line break", ErrUnsafe)
	}
	for i, line := range strings.Split(text, "\n") {
		if startsWithSlash(line) {
			return fmt.Errorf("%w: line %d starts with %q, which GitLab runs as a quick action", ErrUnsafe, i+1, "/")
		}
		if at := mentionAt(line); at >= 0 {
			return fmt.Errorf("%w: line %d: the %q at byte %d could mention someone", ErrUnsafe, i+1, "@", at+1)
		}
	}
	return nil
}
