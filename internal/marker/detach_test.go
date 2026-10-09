package marker

import "testing"

// TestDetachAttach: what a platform that keeps the marker apart stores, and
// the body the core reads back, keep the human part and the marker.
func TestDetachAttach(t *testing.T) {
	line := mustEncode(t, sampleMarker())
	other := mustEncode(t, markerOf(key2, ourFP))
	for _, tc := range []struct {
		name, body, desc, line string
	}{
		{"body and marker", "## Title\n\ntext\n\n" + line, "## Title\n\ntext", line},
		{"marker only", line, "", line},
		{"no marker", "## Title\n\ntext\n", "## Title\n\ntext", ""},
		{"crlf and blanks", "text\r\n\r\n" + line + " \r\n", "text", line},
		{"the last marker wins", "text\n" + other + "\n\n" + line, "text", line},
		{"controls stay", "- [ ] <!-- touchmark:recreate --> Rebuild\n\n" + line, "- [ ] <!-- touchmark:recreate --> Rebuild", line},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, got := Detach(tc.body)
			if desc != tc.desc || got != tc.line {
				t.Fatalf("Detach = %q, %.40q; want %q, %.40q", desc, got, tc.desc, tc.line)
			}
			back := Attach(desc, got)
			if Strip(back) != Strip(tc.body) {
				t.Errorf("Strip(Attach) = %q, want %q", Strip(back), Strip(tc.body))
			}
			m1, s1 := Find(back, []string{ourFP})
			m2, s2 := Find(tc.body, []string{ourFP})
			if s1 != s2 || m1.Key != m2.Key {
				t.Errorf("Find(Attach) = %v %s, Find(body) = %v %s", s1, m1.Key, s2, m2.Key)
			}
		})
	}
	// A marker pasted into the description never counts: only the stored
	// line does.
	if got := Attach("text\n\n"+other, line); got != "text\n\n"+line {
		t.Errorf("Attach with a marker in the description = %q", got)
	}
	if got := Attach("text\n\n"+other, ""); got != "text" {
		t.Errorf("Attach without a stored line = %q", got)
	}
	if got := Attach("", ""); got != "" {
		t.Errorf("Attach of nothing = %q", got)
	}
}
