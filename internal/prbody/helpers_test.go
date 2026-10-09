package prbody

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// Values of the tests.
const (
	hubFP     = "github.com/712345678"
	hubCommit = "3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3"
	hubURL    = "https://github.com/acme/engineering-assets"
	testKey   = "sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58"
	optInName = ".engineering-assets.yml"
)

// flavors are the platforms of the golden bodies.
var flavors = []string{"github", "gitlab", "gitea", "bitbucket", "azure-devops"}

// capsOf returns the capabilities that matter to bodies, as the drivers
// report them (and fake.CapsFor does).
func capsOf(flavor string) platform.Caps {
	c := platform.Caps{Flavor: flavor, MaxBody: 58000}
	switch flavor {
	case "gitlab":
		c.MaxBody = 200000
		c.QuickActions = true
	case "bitbucket":
		c.MaxBody = 60000
		c.Marker = platform.MarkerInRefDef
		c.NoLabels = true
		c.ClosedImmutable = true
	case "azure-devops":
		c.MaxBody = 4000
		c.Marker = platform.MarkerInProperties
		c.ClosedImmutable = true
	}
	return c
}

// testMarker is a real marker line of the test hub, in the comment frame.
func testMarker(t testing.TB) string {
	t.Helper()
	return markerFor(t, "")
}

// markerFor is a real marker line of the test hub in the frame of
// flavor's Caps.Marker.
func markerFor(t testing.TB, flavor string) string {
	t.Helper()
	encode := encodedMarker
	if capsOf(flavor).Marker == platform.MarkerInRefDef {
		encode = encodedRefDef
	}
	line, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// testData is the marker of the test hub.
var testData = marker.Marker{Key: testKey, Data: marker.Data{
	V: marker.Version, Stream: "sync", Hub: "acme-eng", FP: hubFP,
	DecidedAt: hubCommit, ContentCommit: hubCommit, Engine: "0.2.0",
	Packs: []string{"agents"}, TitleSet: "chore: sync engineering assets",
	LabelsSet: []string{"engineering-assets"},
}}

// encodedMarker and encodedRefDef encode the marker once: gzip at the best
// level is slow enough to dominate the fuzzers.
var (
	encodedMarker = sync.OnceValues(func() (string, error) { return marker.Encode(testData) })
	encodedRefDef = sync.OnceValues(func() (string, error) { return marker.EncodeFrame(testData, marker.FrameRefDef) })
)

// golden compares got with testdata/<name>, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", filepath.FromSlash(name))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if decoded(string(want)) != decoded(got) {
		t.Errorf("%s differs from the golden file (go test -update rewrites it)\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// decoded returns body with the data of each marker line as its JSON: Go
// does not promise the same gzip bytes from one release to the next, so the
// golden files compare what a marker holds, not how it was compressed.
func decoded(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !marker.IsLine(line) {
			continue
		}
		m, err := marker.Parse(line)
		if err != nil {
			continue
		}
		js, err := json.Marshal(m.Data)
		if err != nil {
			continue
		}
		end := " -->"
		if !strings.HasPrefix(line, "<!-- ") {
			end = `"`
		}
		lines[i] = line[:strings.Index(line, " data=")] + " data=" + string(js) + end
	}
	return strings.Join(lines, "\n")
}

// span is one code span of a line: its byte range and the text it shows.
type span struct {
	start, end int
	text       string
}

// asciiPunct is what a backslash escapes in CommonMark.
const asciiPunct = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// codeSpans parses the code spans of one line of inline Markdown by the
// CommonMark 0.31 rules (section 6.1), written apart from the package's own
// scanner: a backtick string opens a span closed by the next backtick
// string of the same length; inside, backslashes are literal; one space is
// stripped from each end when both ends are spaces and the text is not all
// spaces. Outside spans a backslash escapes ASCII punctuation, so an
// escaped backtick opens nothing. (Raw HTML and autolinks, which would take
// precedence, are not modelled: the lines checked with it put code spans
// before any "<".)
func codeSpans(line string) []span {
	var out []span
	for i := 0; i < len(line); {
		switch {
		case line[i] == '\\' && i+1 < len(line) && strings.IndexByte(asciiPunct, line[i+1]) >= 0:
			i += 2
		case line[i] == '`':
			open := i
			for i < len(line) && line[i] == '`' {
				i++
			}
			n := i - open
			closeAt := -1
			for k := i; k < len(line); {
				if line[k] != '`' {
					k++
					continue
				}
				m := k
				for m < len(line) && line[m] == '`' {
					m++
				}
				if m-k == n {
					closeAt = k
					break
				}
				k = m
			}
			if closeAt < 0 {
				continue // a literal backtick string
			}
			text := line[i:closeAt]
			if len(text) >= 2 && text[0] == ' ' && text[len(text)-1] == ' ' && strings.Trim(text, " ") != "" {
				text = text[1 : len(text)-1]
			}
			out = append(out, span{open, closeAt + n, text})
			i = closeAt + n
		default:
			i++
		}
	}
	return out
}

// outsideCode returns line with its code spans removed.
func outsideCode(line string) string {
	var b strings.Builder
	last := 0
	for _, s := range codeSpans(line) {
		b.WriteString(line[last:s.start])
		last = s.end
	}
	b.WriteString(line[last:])
	return b.String()
}

// gfmCells splits a table row as cmark-gfm does: without the outer pipes,
// at every pipe not preceded by a backslash; then each cell loses the
// backslash of every "\|" and its surrounding blanks.
func gfmCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var cells []string
	start := 0
	for i := 0; i < len(row); i++ {
		if row[i] == '|' && (i == 0 || row[i-1] != '\\') {
			cells = append(cells, row[start:i])
			start = i + 1
		}
	}
	cells = append(cells, row[start:])
	for i, c := range cells {
		var b strings.Builder
		for r := 0; r < len(c); r++ {
			if c[r] == '\\' && r+1 < len(c) && c[r+1] == '|' {
				continue
			}
			b.WriteByte(c[r])
		}
		cells[i] = strings.TrimSpace(b.String())
	}
	return cells
}

// shown is the text a code span of s shows: s without the characters Code
// drops, invalid bytes as U+FFFD, and one space for nothing. It is written
// apart from Code's own filter.
func shown(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case unicode.Is(unicode.Cc, r),
			r == 0x2028, r == 0x2029, r == 0x061c, r == 0x200e, r == 0x200f,
			0x202a <= r && r <= 0x202e, 0x2066 <= r && r <= 0x2069:
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return " "
	}
	return b.String()
}

// startsSlash reports whether line starts with "/" after blanks and format
// characters, written apart from startsWithSlash.
func startsSlash(line string) bool {
	for _, r := range line {
		if unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		return r == '/'
	}
	return false
}

// checkSafe fails when a line of text starts with "/" or, from line first
// on, has an "@" or "&#" outside code spans.
func checkSafe(t *testing.T, text string, first int) {
	t.Helper()
	for i, line := range strings.Split(text, "\n") {
		if startsSlash(line) {
			t.Errorf("line %d starts with a slash: %q", i+1, line)
		}
		if i+1 < first {
			continue
		}
		if rest := outsideCode(line); strings.ContainsRune(rest, '@') || strings.Contains(rest, "&#") {
			t.Errorf("line %d has an @ outside code: %q", i+1, line)
		}
	}
}

// section returns the part of body from the line heading to the next blank
// line that starts a heading, a details element, a control or the
// footnote. The heading must be a whole line after the first one: text in
// code spans never is.
func section(body, heading string) string {
	i := strings.Index(body, "\n"+heading+"\n")
	if i < 0 {
		return ""
	}
	i++
	rest := body[i+len(heading):]
	end := len(rest)
	for _, next := range []string{"\n\n### ", "\n\n<details>", "\n\n---\n", "\n\n- [ ] <!--"} {
		if j := strings.Index(rest, next); j >= 0 && j < end {
			end = j
		}
	}
	return body[i : i+len(heading)+end]
}
