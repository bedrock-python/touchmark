package prbody

import (
	"strings"

	"github.com/bedrock-python/touchmark/internal/marker"
)

// controlLabels are the labels of the controls, after their comment.
var controlLabels = map[string]string{
	ControlRecreate:  "Rebuild this branch (drops commits added by others)",
	ControlRepropose: "Propose this content again",
}

// ControlLine returns the unticked line of control as Render writes it,
// "- [ ] <!-- touchmark:<control> --> <label>", which Ticked reads; "" for
// a name other than ControlRecreate and ControlRepropose.
func ControlLine(control string) string {
	label, ok := controlLabels[control]
	if !ok {
		return ""
	}
	return controlLine(control, label)
}

// AddControl returns body with the unticked line of control added after its
// human part, before its marker lines (an ack adds the repropose control to
// a closed PR's body). A body that already has a line of control, ticked or
// not, and an unknown control leave body as it is.
//
// Marker lines are the lines marker.IsLine accepts, in either frame (as for
// marker.Strip); they keep their order, after a blank line. The human part
// loses its trailing whitespace, as marker.Strip leaves it.
func AddControl(body, control string) string {
	line := ControlLine(control)
	if line == "" || hasControl(body, control) {
		return body
	}
	human, markers := splitMarkers(body)
	return joinParagraphs(human, line, markers)
}

// ReplaceMarker returns body with every marker line dropped and line, the
// new marker, as its last line after a blank line: how an edit of an
// existing body (an ack, a consumed recreate, a close) writes the marker
// without rendering the body again.
func ReplaceMarker(body, line string) string {
	return joinParagraphs(marker.Strip(body), line)
}

// hasControl reports whether body has a line of control, ticked or not.
func hasControl(body, control string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if c, ok := parseControl(line); ok && c.name == control {
			return true
		}
	}
	return false
}

// splitMarkers returns the human part of body (marker.Strip) and its marker
// lines, one per line, without trailing blanks.
func splitMarkers(body string) (human, markers string) {
	var lines []string
	for line := range strings.SplitSeq(body, "\n") {
		if marker.IsLine(line) {
			lines = append(lines, strings.TrimRight(line, " \t\r"))
		}
	}
	return marker.Strip(body), strings.Join(lines, "\n")
}

// joinParagraphs joins the non-empty parts with blank lines.
func joinParagraphs(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	return b.String()
}

// Ticked reports whether control is ticked ("- [x] <!-- touchmark:<name> -->",
// any case of x) in body. Unticked or absent controls report false.
//
// A control line is a list item ("-", "*" or "+") whose checkbox is followed
// by the control's comment. Blanks (spaces and tabs) may come before the
// list marker, around the checkbox, inside its brackets and inside the
// comment; the line may end in "\r", and anything may follow the comment.
// A checkbox holding anything but blanks and one x does not count, nor does
// a line without the comment right after the checkbox, or with another
// control's name: only touchmark's own tagged lines act, and the rest of a
// body is data.
func Ticked(body, control string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if c, ok := parseControl(line); ok && c.ticked && c.name == control {
			return true
		}
	}
	return false
}

// Untick returns body with control unticked.
//
// The checkbox of every ticked line of control becomes "[ ]"; every other
// byte of body stays as it is.
func Untick(body, control string) string {
	var b strings.Builder
	for start := 0; ; {
		end := strings.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		line := body[start:end]
		if c, ok := parseControl(line); ok && c.ticked && c.name == control {
			b.WriteString(line[:c.box[0]])
			b.WriteByte(' ')
			b.WriteString(line[c.box[1]:])
		} else {
			b.WriteString(line)
		}
		if end == len(body) {
			return b.String()
		}
		b.WriteByte('\n')
		start = end + 1
	}
}

// control is one parsed control line.
type control struct {
	name   string
	ticked bool
	// box is the byte range between the checkbox's brackets.
	box [2]int
}

// parseControl reads a control line (see Ticked).
func parseControl(line string) (control, bool) {
	i := skipBlanks(line, 0)
	if i == len(line) || strings.IndexByte("-*+", line[i]) < 0 {
		return control{}, false
	}
	i = skipBlanks(line, i+1)
	if i == len(line) || line[i] != '[' {
		return control{}, false
	}
	open := i
	closing := strings.IndexByte(line[open:], ']')
	if closing < 0 {
		return control{}, false
	}
	closing += open
	var c control
	switch strings.Trim(line[open+1:closing], " \t") {
	case "":
	case "x", "X":
		c.ticked = true
	default:
		return control{}, false
	}
	c.box = [2]int{open + 1, closing}
	rest, ok := strings.CutPrefix(line[skipBlanks(line, closing+1):], "<!--")
	if !ok {
		return control{}, false
	}
	rest, ok = strings.CutPrefix(strings.TrimLeft(rest, " \t"), "touchmark:")
	if !ok {
		return control{}, false
	}
	n := 0
	for n < len(rest) && isWordByte(rest[n]) {
		n++
	}
	if n == 0 || !strings.HasPrefix(strings.TrimLeft(rest[n:], " \t"), "-->") {
		return control{}, false
	}
	c.name = rest[:n]
	return c, true
}

// skipBlanks returns the index of the first byte at or after i that is not
// a space or a tab.
func skipBlanks(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}
