package marker

import "strings"

// Detach splits body, as the core writes it, for a platform that keeps the
// marker outside the description (platform.MarkerInProperties, Azure
// DevOps): desc is the description, Strip(body): body without any marker
// line and without trailing whitespace; line is the last marker line of
// body (IsLine, without its trailing spaces, tabs and carriage return), ""
// when it has none. Attach(Detach(body)) has the human part and the marker
// Find reads in body: Strip and Find give the same answers for both.
func Detach(body string) (desc, line string) {
	return Strip(body), lastLine(body)
}

// Attach is the body the core reads from a platform that keeps the marker
// apart: the description without its marker lines (Strip: a person may
// paste one into a description; only the stored line counts), then a blank
// line and line, the stored marker line. Either part alone when the other
// is empty.
func Attach(desc, line string) string {
	desc = Strip(desc)
	switch {
	case line == "":
		return desc
	case desc == "":
		return line
	}
	return desc + "\n\n" + line
}

// lastLine returns the last marker line of body (IsLine), without trailing
// spaces, tabs and carriage returns; "" when there is none.
func lastLine(body string) string {
	for end := len(body); ; {
		start := strings.LastIndexByte(body[:end], '\n') + 1
		if line := strings.TrimRight(body[start:end], " \t\r"); IsLine(line) {
			return line
		}
		if start == 0 {
			return ""
		}
		end = start - 1
	}
}
