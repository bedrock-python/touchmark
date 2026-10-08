package marker

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// refDef returns line, a marker in the comment frame, as a reference
// definition, as frame writes it.
func refDef(line string) string { return frame(line, FrameRefDef) }

// escapeAll puts a backslash before every ASCII punctuation character of
// s: the most an editor can add (Bitbucket adds some, "\[", "\_", "\{").
func escapeAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isASCIIPunct(s[i]) {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestEncodeFrameRefDef(t *testing.T) {
	for _, v := range goldenVectors() {
		t.Run(v.name, func(t *testing.T) {
			comment := mustEncode(t, v.m)
			line, err := EncodeFrame(v.m, FrameRefDef)
			if err != nil {
				t.Fatal(err)
			}
			payload := strings.TrimSuffix(strings.TrimPrefix(comment, "<!-- "), " -->")
			if want := `[touchmark]: # "` + payload + `"`; line != want {
				t.Fatalf("EncodeFrame = %.120q…, want %.120q…", line, want)
			}
			if strings.ContainsAny(payload, "\"\r\n") {
				t.Fatal("the payload holds a quote or a line break, which would end the title")
			}
			got, err := Parse(line)
			if err != nil {
				t.Fatalf("Parse(refdef): %v", err)
			}
			equalMarkers(t, got, written(v.m))
			if again, _ := EncodeFrame(v.m, FrameComment); again != comment {
				t.Error("EncodeFrame(FrameComment) differs from Encode")
			}
		})
	}
}

// TestEncodeFrameGolden pins the reference definition frame around the
// payload of a golden vector.
func TestEncodeFrameGolden(t *testing.T) {
	v := goldenVectors()[0]
	line, err := EncodeFrame(v.m, FrameRefDef)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		golden(t, "encode/"+v.name+".refdef", line+"\n")
		return
	}
	pinned := strings.TrimSuffix(readGolden(t, "encode/"+v.name+".refdef"), "\n")
	if !strings.HasPrefix(pinned, `[touchmark]: # "touchmark:v1 hub=`) || !strings.HasSuffix(pinned, `"`) {
		t.Fatalf("golden line %.60q… is not a reference definition", pinned)
	}
	got, err := Parse(pinned)
	if err != nil {
		t.Fatalf("Parse(golden line): %v", err)
	}
	equalMarkers(t, got, written(v.m))
	comment := strings.TrimSuffix(readGolden(t, "encode/"+v.name+".marker"), "\n")
	if refDef(comment) != pinned {
		t.Error("the golden reference definition does not frame the golden comment's payload")
	}
}

func TestParseRefDef(t *testing.T) {
	m := sampleMarker()
	comment := mustEncode(t, m)
	line := refDef(comment)
	want := written(m)
	accepted := []struct{ name, line string }{
		{"as written", line},
		{"brackets escaped", strings.Replace(strings.Replace(line, "[", `\[`, 1), "]", `\]`, 1)},
		{"every punctuation escaped", escapeAll(line)},
		{"hash and quotes escaped", strings.NewReplacer("#", `\#`, `"`, `\"`).Replace(line)},
	}
	for _, c := range accepted {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.line)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			equalMarkers(t, got, want)
		})
	}
	refused := []struct{ name, line string }{
		{"no closing quote", strings.TrimSuffix(line, `"`)},
		{"no closing quote, comment end", strings.TrimSuffix(line, `"`) + commentEnd},
		{"no closing quote, comment end escaped", escapeAll(strings.TrimSuffix(line, `"`) + commentEnd)},
		{"trailing blank", line + " "},
		{"CR", line + "\r"},
		{"single quotes", strings.Replace(strings.Replace(line, `# "`, `# '`, 1), `"`, `'`, 1)},
		{"another label", strings.Replace(line, "[touchmark]", "[touchmark2]", 1)},
		{"another destination", strings.Replace(line, `]: # "`, `]: #x "`, 1)},
		{"indented", " " + line},
		{"escaped backslash before the label", `\\` + line},
		{"letter escaped", strings.Replace(line, "hub=", `h\ub=`, 1)},
		{"comment inside", strings.Replace(line, `"touchmark:v1 `, `"<!-- touchmark:v1 `, 1)},
		{"too long", line[:len(line)-1] + strings.Repeat(`\=`, MaxLine) + `"`},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			if got, err := Parse(c.line); err == nil {
				t.Fatalf("Parse accepted %.80q…: %s", c.line, describeMarker(got))
			}
		})
	}
}

func TestIsLine(t *testing.T) {
	comment := mustEncode(t, sampleMarker())
	line := refDef(comment)
	cases := []struct {
		line string
		want bool
	}{
		{comment, true},
		{line, true},
		{escapeAll(line), true},
		{line + "\r", true},
		{strings.TrimSuffix(line, `"`), true},
		{strings.TrimSuffix(line, `"`) + commentEnd, true},
		{`[touchmark]: # "touchmark:v9 x"`, true},
		{"<!-- touchmark:recreate -->", true},
		{"- [ ] <!-- touchmark:recreate --> Rebuild this branch", false},
		{`[touchmark]: # "something else"`, false},
		{`[touchmark]: https://example.com "touchmark:v1"`, false},
		{"[touchmark]", false},
		{" " + line, false},
		{"see " + line, false},
		{"", false},
		{`\`, false},
		{"[", false},
	}
	for _, c := range cases {
		if got := IsLine(c.line); got != c.want {
			t.Errorf("IsLine(%.60q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestUnescape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{`\[a\]`, "[a]"},
		{`a\_b\*c\#d\(e\)f`, "a_b*c#d(e)f"},
		{`\\`, `\`},
		{`\\\[`, `\[`},
		{`\a\1\ \é`, `\a\1\ \é`},
		{`trailing\`, `trailing\`},
		{`\{\}\~\!\/\:\@\` + "`", "{}~!/:@`"},
	}
	for _, c := range cases {
		if got := unescape(c.in); got != c.want {
			t.Errorf("unescape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for c := 0; c < 128; c++ {
		want := strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", rune(c))
		if got := isASCIIPunct(byte(c)); got != want {
			t.Errorf("isASCIIPunct(%q) = %v, want %v", rune(c), got, want)
		}
	}
}

func TestFindRefDef(t *testing.T) {
	l := newFindLines(t)
	fps := []string{ourFP, prevFP}
	ours, ours2 := parsed(t, l.ours), parsed(t, l.ours2)
	rd := refDef(l.ours)
	cases := []struct {
		name   string
		body   string
		want   Status
		marker Marker
	}{
		{"ours last", "text\n\n" + rd, Found, ours},
		{"CRLF", "text\r\n\r\n" + rd + "\r\n", Found, ours},
		{"escaped by a web edit, CRLF", "text\\_with\\_escapes\r\n\r\n" + escapeAll(rd) + " \r\n", Found, ours},
		{"comment after refdef", rd + "\n" + l.ours2, Found, ours2},
		{"refdef after comment", l.ours2 + "\n" + rd, Found, ours},
		{"foreign refdef", "text\n" + refDef(l.foreign), Foreign, Marker{}},
		{"v2 refdef", refDef(l.v2), Foreign, Marker{}},
		{"broken refdef", "text\n" + strings.TrimSuffix(rd, `"`), Invalid, Marker{}},
		{"tampered refdef", refDef(l.tampered), Invalid, Marker{}},
		{"indented refdef", "  " + rd, None, Marker{}},
		{"refdef in a quote", "> " + rd, None, Marker{}},
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

func TestStripRefDef(t *testing.T) {
	l := newFindLines(t)
	rd := refDef(l.ours)
	cases := []struct{ name, body, want string }{
		{"refdef last", "text\n\n" + rd, "text"},
		{"CRLF", "a\r\nb\r\n\r\n" + rd + "\r\n", "a\r\nb"},
		{"escaped", "a\\_b\n\n" + escapeAll(rd), "a\\_b"},
		{"both frames", "a\n" + l.ours + "\n" + rd + "\nb", "a\nb"},
		{"another reference stays", "a\n\n[docs]: https://example.com \"Docs\"\n" + rd, "a\n\n[docs]: https://example.com \"Docs\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Strip(c.body); got != c.want {
				t.Errorf("Strip = %q, want %q", got, c.want)
			}
		})
	}
}

// TestEncodeFrameMeasuresTheFramedLine: a marker whose comment fits
// MaxLine but whose reference definition, 8 bytes longer, does not, loses
// its changes in the reference definition frame only.
func TestEncodeFrameMeasuresTheFramedLine(t *testing.T) {
	extra := len(refDefOpen) + len(refDefClose) - len(commentOpen) - len(commentEnd)
	r := rand.New(rand.NewPCG(5, 6))
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	random := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = letters[r.IntN(len(letters))]
		}
		return string(b)
	}
	base := sampleMarker()
	base.Data.Changes = []Change{{Path: random(40), Mode: "100644", To: fmt.Sprintf("%016x", r.Uint64())}}
	fixed := []string{random(MaxString), random(MaxString), random(MaxString)}
	tail := random(MaxString)
	for n := 1000; n <= MaxString; n++ {
		m := base
		m.Data.Packs = append(append([]string{}, fixed...), tail[:n])
		comment, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		if len(comment) <= MaxLine-extra || len(comment) > MaxLine {
			continue
		}
		if got := parsed(t, comment); len(got.Data.Changes) != 1 {
			t.Fatalf("the comment lost its changes at %d bytes", len(comment))
		}
		line, err := EncodeFrame(m, FrameRefDef)
		if err != nil {
			t.Fatal(err)
		}
		if len(line) > MaxLine {
			t.Fatalf("a reference definition of %d bytes, more than %d", len(line), MaxLine)
		}
		equalMarkers(t, parsed(t, line), dropped(m))
		return
	}
	t.Fatal("fixture: no pack length put the comment within the last bytes of MaxLine")
}
