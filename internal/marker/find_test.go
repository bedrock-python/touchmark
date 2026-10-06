package marker

import (
	"strings"
	"testing"
)

// findLines are marker lines for the Find and Strip tests.
type findLines struct {
	ours, ours2, prev, foreign, v2, broken, brokenForeign, tampered string
}

func newFindLines(t *testing.T) findLines {
	t.Helper()
	ours := mustEncode(t, markerOf(key1, ourFP))
	foreign := mustEncode(t, markerOf(key1, otherFP))
	// A tampered marker: our fp attribute over another hub's data.
	tampered := strings.Replace(foreign, "fp="+FP16(otherFP), "fp="+FP16(ourFP), 1)
	return findLines{
		ours:          ours,
		ours2:         mustEncode(t, markerOf(key2, ourFP)),
		prev:          mustEncode(t, markerOf(key2, prevFP)),
		foreign:       foreign,
		v2:            strings.Replace(ours, "touchmark:v1 ", "touchmark:v2 ", 1),
		broken:        ours[:len(ours)-20] + commentEnd,
		brokenForeign: foreign[:len(foreign)-20] + commentEnd,
		tampered:      tampered,
	}
}

func TestFind(t *testing.T) {
	l := newFindLines(t)
	fps := []string{ourFP, prevFP}
	ours, ours2, prev := parsed(t, l.ours), parsed(t, l.ours2), parsed(t, l.prev)
	cases := []struct {
		name   string
		body   string
		want   Status
		marker Marker
	}{
		{"empty body", "", None, Marker{}},
		{"no marker", "## Engineering assets\n\nSome text.\n", None, Marker{}},
		{"ours last", "text\n\n" + l.ours, Found, ours},
		{"ours only", l.ours, Found, ours},
		{"ours with newline", "text\n" + l.ours + "\n", Found, ours},
		{"CRLF body", "text\r\n\r\n" + l.ours + "\r\n", Found, ours},
		{"trailing blanks", "text\n" + l.ours + " \t \r\n", Found, ours},
		{"text after ours", "text\n" + l.ours + "\n\nA note someone added below.", Found, ours},
		{"ours twice: the last", l.ours + "\n" + l.ours2, Found, ours2},
		{"ours twice: the last, reversed", l.ours2 + "\n" + l.ours, Found, ours},
		{"previous fingerprint", "text\n" + l.prev, Found, prev},
		{"previous then current", l.prev + "\n" + l.ours, Found, ours},
		{"foreign only", "text\n" + l.foreign, Foreign, Marker{}},
		{"foreign after ours", l.ours + "\n" + l.foreign, Found, ours},
		{"foreign before ours", l.foreign + "\n" + l.ours, Found, ours},
		{"v2 only", l.v2, Foreign, Marker{}},
		{"v2 after ours", l.ours + "\n" + l.v2, Found, ours},
		{"other touchmark comment", "<!-- touchmark:recreate --> Rebuild", Foreign, Marker{}},
		{"broken ours", "text\n" + l.broken, Invalid, Marker{}},
		{"broken after ours", l.ours + "\n" + l.broken, Invalid, Marker{}},
		{"ours after broken", l.broken + "\n" + l.ours, Found, ours},
		{"tampered fp", "text\n" + l.tampered, Invalid, Marker{}},
		{"tampered after ours", l.ours + "\n" + l.tampered, Invalid, Marker{}},
		{"broken foreign", l.brokenForeign, Foreign, Marker{}},
		{"broken foreign after ours", l.ours + "\n" + l.brokenForeign, Found, ours},
		{"garbled v1 line", "<!-- touchmark:v1 garbage", Foreign, Marker{}},
		{"ours with text before on the line", "see: " + l.ours, None, Marker{}},
		{"ours indented", "  " + l.ours, None, Marker{}},
		{"ours in a list item", "- [ ] " + l.ours, None, Marker{}},
		{"checkbox control", "- [ ] <!-- touchmark:recreate --> Rebuild this branch", None, Marker{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, status := Find(c.body, fps)
			if status != c.want {
				t.Fatalf("status = %v, want %v", status, c.want)
			}
			equalMarkers(t, m, c.marker)
		})
	}
}

func TestFindWithoutFingerprints(t *testing.T) {
	l := newFindLines(t)
	if _, status := Find(l.ours, nil); status != Foreign {
		t.Errorf("status = %v, want foreign: nothing is ours", status)
	}
	if _, status := Find(l.ours, []string{prevFP}); status != Foreign {
		t.Errorf("status = %v, want foreign for another fingerprint", status)
	}
}

// TestFindDataOfAnotherHub simulates an FP16 collision: the fp attribute is
// ours but the data names another fingerprint. It must be Invalid, never
// Found.
func TestFindDataOfAnotherHub(t *testing.T) {
	l := newFindLines(t)
	oursFP16 := func(fp16 string) bool { return fp16 == FP16(ourFP) || fp16 == FP16(otherFP) }
	ours := func(fp string) bool { return fp == ourFP }
	if _, status := find("text\n"+l.foreign, oursFP16, ours); status != Invalid {
		t.Errorf("status = %v, want invalid", status)
	}
	if m, status := find(l.foreign+"\n"+l.ours, oursFP16, ours); status != Found || m.Data.FP != ourFP {
		t.Errorf("status = %v with fp %q, want ours found", status, m.Data.FP)
	}
}

func TestFindLargeBody(t *testing.T) {
	l := newFindLines(t)
	fps := []string{ourFP}
	filler := strings.Repeat("filler line of the description\n", (scanLimit/31)+10) // > 1 MiB
	if len(filler) <= scanLimit {
		t.Fatal("fixture: filler must exceed the scan limit")
	}
	cases := []struct {
		name string
		body string
		want Status
	}{
		{"marker beyond the last MiB", l.ours + "\n" + filler, None},
		{"marker in the last MiB", filler + l.ours, Found},
		{"marker at the window start", strings.Repeat("x", 100) + "\n" + l.ours + "\n" + strings.Repeat("y", scanLimit-len(l.ours)-1), Found},
		{"window cuts the marker line", strings.Repeat("x", 100) + "\n" + l.ours + "\n" + strings.Repeat("y", scanLimit-len(l.ours)/2), None},
		// Cut right before "<!--" of a line that does not start there: the
		// partial line must not pass for a marker line.
		{"window starts inside a line", strings.Repeat("x", 100) + "not a marker: " + l.ours + "\n" + strings.Repeat("y", scanLimit-len(l.ours)-1), None},
		{"window without a newline", strings.Repeat("z", scanLimit+10), None},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, status := Find(c.body, fps); status != c.want {
				t.Fatalf("status = %v, want %v", status, c.want)
			}
		})
	}
}

func TestTail(t *testing.T) {
	head := strings.Repeat("a", 10)
	cases := []struct{ name, body, want string }{
		{"short", "abc\ndef", "abc\ndef"},
		{"exact", strings.Repeat("b", scanLimit), strings.Repeat("b", scanLimit)},
		{"cut at a line start", head + "\n" + strings.Repeat("c", scanLimit), strings.Repeat("c", scanLimit)},
		{"cut inside a line", head + strings.Repeat("d", 5) + "\n" + strings.Repeat("e", scanLimit-3), strings.Repeat("e", scanLimit-3)},
		{"no line start", strings.Repeat("f", scanLimit+1), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tail(c.body); got != c.want {
				t.Errorf("tail = %d bytes %.20q…, want %d bytes %.20q…", len(got), got, len(c.want), c.want)
			}
		})
	}
}

func TestStrip(t *testing.T) {
	l := newFindLines(t)
	cases := []struct{ name, body, want string }{
		{"empty", "", ""},
		{"no marker", "text\n", "text"},
		{"marker last", "text\n\n" + l.ours, "text"},
		{"marker only", l.ours, ""},
		{"CRLF", "a\r\nb\r\n\r\n" + l.ours + "\r\n", "a\r\nb"},
		{"marker in the middle", "a\n" + l.ours + "\nb", "a\nb"},
		{"markers of every kind", l.foreign + "\na\n" + l.v2 + "\n" + l.broken + "\nb\n<!-- touchmark:recreate -->\n" + l.ours, "a\nb"},
		{"controls stay", "- [ ] <!-- touchmark:recreate --> Rebuild this branch\n" + l.ours, "- [ ] <!-- touchmark:recreate --> Rebuild this branch"},
		{"indented marker stays", "a\n  " + l.ours, "a\n  " + l.ours},
		{"trailing whitespace", "a  \n\n\t \n", "a"},
		{"leading whitespace stays", "\n\n  a", "\n\n  a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Strip(c.body); got != c.want {
				t.Errorf("Strip = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{None: "none", Found: "found", Invalid: "invalid", Foreign: "foreign", 9: "unknown"} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", s, got, want)
		}
	}
}

// parsed parses line and fails the test on error.
func parsed(t *testing.T, line string) Marker {
	t.Helper()
	m, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
