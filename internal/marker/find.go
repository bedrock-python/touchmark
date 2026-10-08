package marker

import (
	"strings"
	"unicode"
)

// scanLimit is how much of the end of a body Find reads.
const scanLimit = 1 << 20

// Status is what Find saw in a body.
type Status uint8

const (
	// None: no marker line of any version at all (IsLine).
	None Status = iota
	// Found: a valid marker for one of the fingerprints.
	Found
	// Invalid: a v1 marker whose fp attribute is ours but that fails Parse,
	// or whose data belongs to another fingerprint (a tampered marker).
	Invalid
	// Foreign: markers exist, none is ours (another hub, another version).
	Foreign
)

// String returns the status name for messages and tests.
func (s Status) String() string {
	switch s {
	case None:
		return "none"
	case Found:
		return "found"
	case Invalid:
		return "invalid"
	case Foreign:
		return "foreign"
	}
	return "unknown"
}

// Find returns the LAST marker in body whose fingerprint is one of
// fingerprints (full forms), with Found when it is valid. Bodies larger than
// 1 MiB are scanned only in their last 1 MiB (from the first full line in
// it). Markers of unknown versions count as Foreign.
//
// A marker line starts with "<!-- touchmark:" at the start of a line, or is
// a reference definition `[touchmark]: # "touchmark:…"` there, read as the
// comment it frames (IsLine; Bitbucket's backslash escapes undone); it is
// compared without trailing spaces, tabs and carriage returns (platforms
// store bodies with CRLF). Scanning from the end, the first v1 marker line
// whose fp attribute is FP16 of one of fingerprints decides (the last marker
// with our fp counts): Found when it parses and its data names one of
// fingerprints in full, Invalid otherwise, even when a valid marker of ours
// precedes it. Lines above it are never parsed, so Find parses at most one
// line. Without such a line the status is Foreign when any touchmark comment
// was seen (another hub's marker, a broken line with a foreign fp, another
// version) and None otherwise.
func Find(body string, fingerprints []string) (Marker, Status) {
	fp16s := make(map[string]bool, len(fingerprints))
	full := make(map[string]bool, len(fingerprints))
	for _, fp := range fingerprints {
		fp16s[FP16(fp)] = true
		full[fp] = true
	}
	return find(body, func(fp16 string) bool { return fp16s[fp16] }, func(fp string) bool { return full[fp] })
}

// find is Find with the fingerprint tests as functions: oursFP16 tells an fp
// attribute of ours, ours a full fingerprint of ours (tests simulate FP16
// collisions through them).
func find(body string, oursFP16, ours func(string) bool) (Marker, Status) {
	body = tail(body)
	status := None
	for end := len(body); ; {
		start := strings.LastIndexByte(body[:end], '\n') + 1
		if line, ok := asComment(strings.TrimRight(body[start:end], " \t\r")); ok {
			status = Foreign
			if strings.HasPrefix(line, v1Prefix) && oursFP16(fpAttr(line)) {
				m, err := Parse(line)
				if err != nil || !ours(m.Data.FP) {
					return Marker{}, Invalid
				}
				return m, Found
			}
		}
		if start == 0 {
			return Marker{}, status
		}
		end = start - 1
	}
}

// tail returns the last scanLimit bytes of body, without the partial line
// they may start with.
func tail(body string) string {
	if len(body) <= scanLimit {
		return body
	}
	cut := len(body) - scanLimit
	if body[cut-1] != '\n' {
		i := strings.IndexByte(body[cut:], '\n')
		if i < 0 {
			return ""
		}
		cut += i + 1
	}
	return body[cut:]
}

// fpAttr returns the value of the first " fp=" attribute of a marker line,
// read up to the next space; "" when there is none. It works on lines Parse
// refuses, to tell a broken marker of ours from someone else's.
func fpAttr(line string) string {
	_, v, ok := strings.Cut(line, " fp=")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(v, ' '); i >= 0 {
		v = v[:i]
	}
	return v
}

// Strip returns body without any touchmark marker lines (any version, either
// frame), with trailing whitespace removed, for hashing the human part of a
// body. A marker line is one IsLine accepts, as for Find; other lines keep
// their bytes, carriage returns included.
func Strip(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	first := true
	for line := range strings.SplitSeq(body, "\n") {
		if IsLine(line) {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		first = false
	}
	return strings.TrimRightFunc(b.String(), unicode.IsSpace)
}
