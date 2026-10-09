package marker

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// seedLines returns marker lines for the fuzz corpora: the golden vectors,
// a gzip bomb and damaged variants.
func seedLines(t testing.TB) []string {
	var lines []string
	for _, v := range goldenVectors() {
		line := mustEncode(t, v.m)
		lines = append(lines, line, line[:len(line)/2], strings.Replace(line, "v1", "v2", 1), line+"\r")
		rd := refDef(line)
		lines = append(lines, rd, escapeAll(rd), rd[:len(rd)/2], rd+"\r", strings.TrimSuffix(rd, refDefClose)+commentEnd)
	}
	lines = append(lines,
		lineWithData(gz(t, bytes.Repeat([]byte{' '}, 8<<20))),
		lineWithJSON(t, sampleJSON(t)+" "),
		lineWithJSON(t, "{}"),
		"<!-- touchmark:v1 -->",
		"<!-- touchmark:v1 hub= fp= stream= key= data= -->",
		`[touchmark]: # "touchmark:v1 hub= fp= stream= key= data="`,
		`\[touchmark\]: \# "touchmark:v1\"`,
		`[touchmark]: # "touchmark:v1 hub= fp= stream= key= data= -->`,
	)
	return lines
}

// FuzzParse checks that Parse never panics on any line, and that whatever
// it accepts encodes back to a line that parses to the same marker (changes
// may be dropped when the re-encoded payload is larger than the limits of
// the encoder, which are stricter than those of the parser).
func FuzzParse(f *testing.F) {
	for _, line := range seedLines(f) {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		m, err := Parse(line)
		if err != nil {
			return
		}
		if !strings.HasPrefix(line, commentPrefix) && !strings.HasSuffix(unescape(line), refDefClose) {
			t.Fatalf("accepted a reference definition without its closing quote: %.80q", line)
		}
		if m.FP16 != FP16(m.Data.FP) || m.Hub != m.Data.Hub || m.Stream != m.Data.Stream {
			t.Fatalf("header does not match the data: %s", describeMarker(m))
		}
		again, err := Encode(m)
		if err != nil {
			if !errors.Is(err, errTooLarge) {
				t.Fatalf("Encode of a parsed marker: %v", err)
			}
			return
		}
		back, err := Parse(again)
		if err != nil {
			t.Fatalf("Parse(Encode(Parse(line))): %v", err)
		}
		if !reflect.DeepEqual(back, m) && !reflect.DeepEqual(back, dropped(m)) {
			t.Fatalf("round trip differs\n got %s\nwant %s", describeMarker(back), describeMarker(m))
		}
		if twice := mustEncode(t, back); twice != again {
			t.Fatal("Encode is not stable across a round trip")
		}
	})
}

// FuzzEncode builds markers from fuzzed fields: whatever Encode accepts must
// parse back to the marker it wrote.
func FuzzEncode(f *testing.F) {
	f.Add("acme-eng", ourFP, "sync", "agents", "AGENTS.md", "77ab", "100644", "8f3c", "chore: sync", false, 3)
	f.Add("a", "h:1/2", "adopt", "", "x", "", "", "ff", "", true, 0)
	f.Add("hub", "github.com/1", "sync", "p", "", "", "100755", "0", "t", true, 6000)
	f.Fuzz(func(t *testing.T, hub, fp, stream, pack, path, from, mode, to, title string, ack bool, n int) {
		m := Marker{Key: key1, Data: Data{
			V: Version, Stream: stream, Hub: hub, FP: fp, Engine: "0.2.0",
			Packs: []string{pack}, TitleSet: title, Ack: ack, ChangesComplete: true,
			LabelsSet: []string{title},
		}}
		n = min(max(n, 0), MaxChanges+1)
		for i := range n {
			m.Data.Changes = append(m.Data.Changes, Change{Path: path + strings.Repeat("/x", i%3), From: from, Mode: mode, To: to})
		}
		if n%2 == 1 {
			m.Data.Closed = &Closed{By: "touchmark", Reason: title}
			m.Data.RecreateFor = &pack
		}
		line, err := Encode(m)
		if err != nil {
			return
		}
		if len(line) > MaxLine {
			t.Fatalf("line of %d bytes", len(line))
		}
		rd, err := EncodeFrame(m, FrameRefDef)
		switch {
		case err != nil && !errors.Is(err, errTooLarge):
			t.Fatalf("EncodeFrame(FrameRefDef) of an encodable marker: %v", err)
		case err != nil:
		case len(rd) > MaxLine:
			t.Fatalf("reference definition of %d bytes", len(rd))
		default:
			back, err := Parse(rd)
			if err != nil {
				t.Fatalf("Parse(EncodeFrame(m, FrameRefDef)): %v", err)
			}
			if !reflect.DeepEqual(back, written(m)) && !reflect.DeepEqual(back, dropped(m)) {
				t.Fatalf("reference definition round trip differs\n got %s", describeMarker(back))
			}
		}
		got, err := Parse(line)
		if err != nil {
			t.Fatalf("Parse(Encode(m)): %v\nline %.300q", err, line)
		}
		if !reflect.DeepEqual(got, written(m)) && !reflect.DeepEqual(got, dropped(m)) {
			t.Fatalf("round trip differs\n got %s\nwant %s", describeMarker(got), describeMarker(written(m)))
		}
	})
}

// FuzzFind checks that Find and Strip never panic, that a marker of ours
// appended as the last line always wins, and that Strip removes every
// marker line and nothing else.
func FuzzFind(f *testing.F) {
	for _, line := range seedLines(f) {
		f.Add("text\n" + line + "\n")
	}
	f.Add("a\r\n<!-- touchmark:v1 hub=acme-eng fp=" + FP16(ourFP) + " x -->\r\nb")
	f.Add("<!-- touchmark:\n<!-- touchmark:v1 \n<!-- touchmark:v1 fp=")
	fps := []string{ourFP, prevFP}
	last := mustEncode(f, markerOf(key2, prevFP))
	want, err := Parse(last)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body string) {
		m, status := Find(body, fps)
		switch status {
		case Found:
			if m.Data.FP != ourFP && m.Data.FP != prevFP {
				t.Fatalf("found a marker of %q", m.Data.FP)
			}
			if m.FP16 != FP16(m.Data.FP) {
				t.Fatal("found a marker whose header does not match its data")
			}
		case None, Invalid, Foreign:
			if !reflect.DeepEqual(m, Marker{}) {
				t.Fatalf("status %v with a marker", status)
			}
		default:
			t.Fatalf("status %v", status)
		}

		stripped := Strip(body)
		if Strip(stripped) != stripped {
			t.Fatal("Strip is not idempotent")
		}
		for line := range strings.SplitSeq(stripped, "\n") {
			if IsLine(line) {
				t.Fatalf("Strip left a marker line: %.80q", line)
			}
		}
		if _, status := Find(stripped, fps); status != None && len(stripped) <= scanLimit {
			t.Fatalf("Find after Strip = %v, want none", status)
		}

		if len(body) > scanLimit/2 {
			return
		}
		appended := body + "\n" + last
		if got, status := Find(appended, fps); status != Found || !reflect.DeepEqual(got, want) {
			t.Fatalf("appended marker: status %v", status)
		}
		if Strip(appended) != stripped {
			t.Fatalf("Strip(body + marker) = %q, want %q", Strip(appended), stripped)
		}
		appended = body + "\r\n" + escapeAll(refDef(last)) + " \r\n"
		if got, status := Find(appended, fps); status != Found || !reflect.DeepEqual(got, want) {
			t.Fatalf("appended reference definition: status %v", status)
		}
	})
}

// FuzzDetach: a body split for a platform that keeps the marker apart and
// joined again keeps its human part, and the line kept apart is a marker
// line or nothing.
func FuzzDetach(f *testing.F) {
	for _, line := range seedLines(f) {
		f.Add("text\r\n\r\n" + line + " \n")
	}
	f.Add("<!-- touchmark:\n\n- [ ] <!-- touchmark:recreate -->")
	f.Fuzz(func(t *testing.T, body string) {
		desc, line := Detach(body)
		if line != "" && !IsLine(line) {
			t.Fatalf("Detach kept %.80q apart, no marker line", line)
		}
		if Strip(desc) != desc {
			t.Fatal("the description keeps a marker line or trailing blanks")
		}
		if got := Strip(Attach(desc, line)); got != Strip(body) {
			t.Fatalf("Strip(Attach(Detach)) = %.80q, want %.80q", got, Strip(body))
		}
		if d2, l2 := Detach(Attach(desc, line)); d2 != desc || l2 != line {
			t.Fatal("Detach(Attach(Detach(body))) differs from Detach(body)")
		}
	})
}
