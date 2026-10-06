package marker

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestFP16(t *testing.T) {
	// sha256("github.com/712345678") starts with these 16 hex digits.
	if got, want := FP16(ourFP), "b7461799636b11ec"; got != want {
		t.Errorf("FP16(%q) = %q, want %q", ourFP, got, want)
	}
	if got := FP16(""); got != "e3b0c44298fc1c14" {
		t.Errorf("FP16(\"\") = %q, want the prefix of sha256 of nothing", got)
	}
	if FP16(ourFP) == FP16(prevFP) {
		t.Error("different fingerprints hash alike")
	}
}

// goldenVectors are the markers whose encoding is pinned under
// testdata/encode.
func goldenVectors() []struct {
	name string
	m    Marker
} {
	closedNoDiff := sampleMarker()
	closedNoDiff.Key = key2
	closedNoDiff.Data.HubRepo = "" // private hub, public target
	closedNoDiff.Data.Closed = &Closed{By: "touchmark", Reason: "no-diff"}

	adopt := sampleMarker()
	adopt.Data.Stream = "adopt"
	adopt.Data.FP = prevFP
	adopt.Data.Hub = "old-hub-2"
	adopt.Data.Changes = adopt.Data.Changes[:1]
	adopt.Data.TitleSet = `chore: sync "assets" <b>&</b> — ünïcödé`
	adopt.Data.LabelsSet = []string{"engineering-assets", "bot:touchmark"}
	adopt.Data.Ack = true
	adopt.Data.Revoked = true
	adopt.Data.RecreateFor = ptr(baseCommit)

	empty := Marker{Key: key2, Data: Data{V: Version, Stream: "sync", Hub: "acme-eng", FP: "gitlab.example.com:8443/42"}}

	return []struct {
		name string
		m    Marker
	}{
		{"sync-open", sampleMarker()},
		{"closed-no-diff", closedNoDiff},
		{"adopt-acked", adopt},
		{"empty", empty},
	}
}

// TestEncodeGolden pins what Encode writes: the JSON inside the marker and
// the header before its data attribute. The gzip bytes are not pinned: Go
// promises no stable deflate output across releases, and nothing depends on
// them (the body hash leaves the marker out). Old lines must keep parsing to
// the marker they encoded.
func TestEncodeGolden(t *testing.T) {
	for _, v := range goldenVectors() {
		t.Run(v.name, func(t *testing.T) {
			line := mustEncode(t, v.m)
			golden(t, "encode/"+v.name+".json", payload(t, line)+"\n")
			if *update {
				golden(t, "encode/"+v.name+".marker", line+"\n")
				return
			}
			pinned := strings.TrimSuffix(readGolden(t, "encode/"+v.name+".marker"), "\n")
			if got, want := headerOf(t, line), headerOf(t, pinned); got != want {
				t.Errorf("header %q, golden %q", got, want)
			}
			if got, want := payload(t, line), payload(t, pinned); got != want {
				t.Errorf("payload differs from the golden line's\n got %s\nwant %s", got, want)
			}
			got, err := Parse(pinned)
			if err != nil {
				t.Fatalf("Parse(golden line): %v", err)
			}
			equalMarkers(t, got, written(v.m))
		})
	}
}

// headerOf returns line up to and including " data=".
func headerOf(t *testing.T, line string) string {
	t.Helper()
	i := strings.LastIndex(line, " data=")
	if i < 0 {
		t.Fatalf("no data attribute in %.80q", line)
	}
	return line[:i+len(" data=")]
}

func TestEncodeFormat(t *testing.T) {
	line := mustEncode(t, sampleMarker())
	want := "<!-- touchmark:v1 hub=acme-eng fp=" + FP16(ourFP) + " stream=sync key=" + key1 + " data="
	if !strings.HasPrefix(line, want) || !strings.HasSuffix(line, " -->") {
		t.Fatalf("line = %.200q, want prefix %q and suffix \" -->\"", line, want)
	}
	if strings.ContainsAny(line, "\r\n") || strings.Count(line, "--") != 2 {
		t.Errorf("line must be one HTML comment with no other \"--\": %.200q", line)
	}
	if again := mustEncode(t, sampleMarker()); again != line {
		t.Error("Encode is not deterministic")
	}
	// The JSON follows the field order of Data.
	js := payload(t, line)
	if !strings.HasPrefix(js, `{"v":1,"stream":"sync","hub":"acme-eng","fp":"github.com/712345678","hub_repo":`) ||
		!strings.Contains(js, `"changes":[["AGENTS.md","77ab0c1d2e3f4a5b","100644","8f3c1a0b2e7d4a61"],["prompts/old.md","9a01bc23de45f678","",""],`) ||
		!strings.HasSuffix(js, `"ack":false,"revoked":false,"recreate_for":null}`) {
		t.Errorf("unexpected JSON: %s", js)
	}
}

func TestEncodeFillsHeaderFromData(t *testing.T) {
	m := sampleMarker()
	m.Hub, m.Stream, m.FP16 = "someone-else", "adopt", "0000000000000000"
	got, err := Parse(mustEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if got.Hub != "acme-eng" || got.Stream != "sync" || got.FP16 != FP16(ourFP) {
		t.Errorf("header = %s %s %s, want it filled from the data", got.Hub, got.Stream, got.FP16)
	}
}

func TestEncodeWritesEmptyArrays(t *testing.T) {
	m := sampleMarker()
	m.Data.Packs, m.Data.Changes, m.Data.LabelsSet = nil, nil, []string{}
	js := payload(t, mustEncode(t, m))
	for _, want := range []string{`"packs":[]`, `"changes":[]`, `"labels_set":[]`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON lacks %s: %s", want, js)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	long := strings.Repeat("é", MaxString/2) // exactly MaxString bytes
	cases := map[string]func(*Marker){
		"sample":   func(*Marker) {},
		"adopt":    func(m *Marker) { m.Data.Stream = "adopt" },
		"previous": func(m *Marker) { m.Data.FP = prevFP },
		"port":     func(m *Marker) { m.Data.FP = "git.example.com:8443/7" },
		"punycode": func(m *Marker) { m.Data.FP = "xn--80ak6aa92e.example--x.com/1" },
		"closed": func(m *Marker) {
			m.Data.Closed = &Closed{By: "touchmark", Reason: "target-dropped"}
		},
		"empty-closed":   func(m *Marker) { m.Data.Closed = &Closed{} },
		"recreate":       func(m *Marker) { m.Data.RecreateFor = ptr(hubCommit) },
		"recreate-empty": func(m *Marker) { m.Data.RecreateFor = ptr("") },
		"flags":          func(m *Marker) { m.Data.Ack, m.Data.Revoked, m.Data.ChangesComplete = true, true, false },
		"control-characters": func(m *Marker) {
			m.Data.TitleSet = "a\x00b\nc\td" + string(rune(0x2028)) + "e" + string(rune(0x2029)) + "f<g>&h\"i\\j"
		},
		"max-strings": func(m *Marker) {
			m.Data.TitleSet, m.Data.Body, m.Data.Engine = long, long, long
			m.Data.Packs = []string{long}
			m.Data.Changes = []Change{{Path: long, Mode: "100644", To: "1"}}
		},
		"short-oids": func(m *Marker) {
			m.Data.Changes = []Change{{Path: "a", From: "a", Mode: "100755", To: "0123456789abcdef"}}
		},
		"deletion-only": func(m *Marker) {
			m.Data.Changes = []Change{{Path: "gone.md", From: "ff"}}
		},
		"no-slices": func(m *Marker) { m.Data.Packs, m.Data.Changes, m.Data.LabelsSet = nil, nil, nil },
		"one-letter-hub": func(m *Marker) {
			m.Data.Hub = "a"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			m := sampleMarker()
			edit(&m)
			got, err := Parse(mustEncode(t, m))
			if err != nil {
				t.Fatalf("Parse(Encode(m)): %v", err)
			}
			equalMarkers(t, got, written(m))
		})
	}
}

func TestEncodeRejects(t *testing.T) {
	tooLong := strings.Repeat("x", MaxString+1)
	cases := map[string]func(*Marker){
		"hub empty":            func(m *Marker) { m.Data.Hub = "" },
		"hub uppercase":        func(m *Marker) { m.Data.Hub = "Acme" },
		"hub double hyphen":    func(m *Marker) { m.Data.Hub = "acme--eng" },
		"hub edge hyphen":      func(m *Marker) { m.Data.Hub = "acme-" },
		"hub space":            func(m *Marker) { m.Data.Hub = "acme eng" },
		"stream empty":         func(m *Marker) { m.Data.Stream = "" },
		"stream unknown":       func(m *Marker) { m.Data.Stream = "other" },
		"stream case":          func(m *Marker) { m.Data.Stream = "Sync" },
		"fp empty":             func(m *Marker) { m.Data.FP = "" },
		"fp without id":        func(m *Marker) { m.Data.FP = "github.com" },
		"fp id not numeric":    func(m *Marker) { m.Data.FP = "github.com/acme" },
		"fp with path":         func(m *Marker) { m.Data.FP = "github.com/acme/712345678" },
		"fp with scheme":       func(m *Marker) { m.Data.FP = "https://github.com/1" },
		"key empty":            func(m *Marker) { m.Key = "" },
		"key other algorithm":  func(m *Marker) { m.Key = "sha1:" + strings.Repeat("a", 40) },
		"key uppercase":        func(m *Marker) { m.Key = "sha256:" + strings.Repeat("A", 64) },
		"key short":            func(m *Marker) { m.Key = "sha256:" + strings.Repeat("a", 63) },
		"key long":             func(m *Marker) { m.Key = "sha256:" + strings.Repeat("a", 65) },
		"version zero":         func(m *Marker) { m.Data.V = 0 },
		"version two":          func(m *Marker) { m.Data.V = 2 },
		"title too long":       func(m *Marker) { m.Data.TitleSet = tooLong },
		"hub_repo too long":    func(m *Marker) { m.Data.HubRepo = tooLong },
		"decided_at too long":  func(m *Marker) { m.Data.DecidedAt = tooLong },
		"commit too long":      func(m *Marker) { m.Data.ContentCommit = tooLong },
		"base too long":        func(m *Marker) { m.Data.Base = tooLong },
		"optin too long":       func(m *Marker) { m.Data.OptIn = tooLong },
		"engine too long":      func(m *Marker) { m.Data.Engine = tooLong },
		"body too long":        func(m *Marker) { m.Data.Body = tooLong },
		"pack too long":        func(m *Marker) { m.Data.Packs = []string{"a", tooLong} },
		"label too long":       func(m *Marker) { m.Data.LabelsSet = []string{tooLong} },
		"closed.by too long":   func(m *Marker) { m.Data.Closed = &Closed{By: tooLong} },
		"closed.reason long":   func(m *Marker) { m.Data.Closed = &Closed{Reason: tooLong} },
		"recreate_for long":    func(m *Marker) { m.Data.RecreateFor = &tooLong },
		"invalid UTF-8":        func(m *Marker) { m.Data.TitleSet = "a\xffb" },
		"invalid UTF-8 pack":   func(m *Marker) { m.Data.Packs = []string{"\xc3"} },
		"invalid UTF-8 path":   func(m *Marker) { m.Data.Changes[0].Path = "\xed\xa0\x80" },
		"change path empty":    func(m *Marker) { m.Data.Changes[0].Path = "" },
		"change path too long": func(m *Marker) { m.Data.Changes[0].Path = tooLong },
		"change from long":     func(m *Marker) { m.Data.Changes[0].From = "0123456789abcdef0" },
		"change from upper":    func(m *Marker) { m.Data.Changes[0].From = "77AB0C1D2E3F4A5B" },
		"change to not hex":    func(m *Marker) { m.Data.Changes[0].To = "xyz" },
		"change full oid":      func(m *Marker) { m.Data.Changes[0].To = hubCommit },
		"change zero mode":     func(m *Marker) { m.Data.Changes[1].Mode = "000000" },
		"change symlink mode":  func(m *Marker) { m.Data.Changes[0].Mode = "120000" },
		"deletion with to":     func(m *Marker) { m.Data.Changes[1].To = "abc" },
		"write without to":     func(m *Marker) { m.Data.Changes[0].To = "" },
		"nothing to nothing":   func(m *Marker) { m.Data.Changes[2].To, m.Data.Changes[2].Mode = "", "" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			m := sampleMarker()
			edit(&m)
			if line, err := Encode(m); err == nil {
				t.Fatalf("Encode accepted it: %.200q", line)
			} else if errors.Is(err, errTooLarge) {
				t.Fatalf("Encode failed on size, want a validation error: %v", err)
			}
		})
	}
}

// fitsLine reports the size of the JSON and of the line for m with its
// changes, as Encode would first render them.
func fitsLine(t *testing.T, m Marker) (jsonSize, lineSize int) {
	t.Helper()
	m.Hub, m.Stream, m.FP16 = m.Data.Hub, m.Data.Stream, FP16(m.Data.FP)
	line, size, err := encodeLine(m, m.Data)
	if err != nil {
		t.Fatal(err)
	}
	return size, len(line)
}

func TestEncodeKeepsChangesThatFit(t *testing.T) {
	m := sampleMarker()
	m.Data.Changes = nil
	for i := range 200 {
		m.Data.Changes = append(m.Data.Changes, Change{Path: fmt.Sprintf("docs/page-%03d.md", i), Mode: "100644", To: "0123456789abcdef"})
	}
	got, err := Parse(mustEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	equalMarkers(t, got, written(m))
}

func TestEncodeDropsChangesOverMaxJSON(t *testing.T) {
	m := sampleMarker()
	m.Data.Changes = nil
	for i := range 800 {
		p := fmt.Sprintf("docs/section-%04d/%s.md", i, strings.Repeat("x", 40))
		m.Data.Changes = append(m.Data.Changes, Change{Path: p, From: "fedcba9876543210", Mode: "100644", To: "0123456789abcdef"})
	}
	jsonSize, lineSize := fitsLine(t, m)
	if jsonSize <= MaxJSON || lineSize > MaxLine {
		t.Fatalf("fixture: JSON %d bytes, line %d bytes; want only the JSON over its limit", jsonSize, lineSize)
	}
	line := mustEncode(t, m)
	got, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	equalMarkers(t, got, dropped(m))
	if got.Key != key1 {
		t.Error("dropping changes must keep the key")
	}
}

func TestEncodeDropsChangesOverMaxLine(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	hex := func() string { return fmt.Sprintf("%016x", r.Uint64()) }
	m := sampleMarker()
	m.Data.Changes = nil
	for range 600 {
		m.Data.Changes = append(m.Data.Changes, Change{Path: "p/" + hex(), From: hex(), Mode: "100644", To: hex()})
	}
	jsonSize, lineSize := fitsLine(t, m)
	if jsonSize > MaxJSON || lineSize <= MaxLine {
		t.Fatalf("fixture: JSON %d bytes, line %d bytes; want only the line over its limit", jsonSize, lineSize)
	}
	got, err := Parse(mustEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	equalMarkers(t, got, dropped(m))
}

func TestEncodeDropsChangesOverMaxChanges(t *testing.T) {
	m := sampleMarker()
	m.Data.Changes = make([]Change, MaxChanges+1)
	for i := range m.Data.Changes {
		m.Data.Changes[i] = Change{Path: fmt.Sprint(i), Mode: "100644", To: "1"}
	}
	got, err := Parse(mustEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	equalMarkers(t, got, dropped(m))
}

func TestEncodeTooLargeLine(t *testing.T) {
	// Random letters do not compress: 5 × 4096 of them overflow the line
	// with or without changes, while the JSON stays under MaxJSON.
	r := rand.New(rand.NewPCG(3, 4))
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	m := sampleMarker()
	m.Data.Packs = nil
	for range 5 {
		b := make([]byte, MaxString)
		for i := range b {
			b[i] = letters[r.IntN(len(letters))]
		}
		m.Data.Packs = append(m.Data.Packs, string(b))
	}
	if jsonSize, _ := fitsLine(t, m); jsonSize > MaxJSON {
		t.Fatalf("fixture: JSON %d bytes is over MaxJSON", jsonSize)
	}
	_, err := Encode(m)
	if !errors.Is(err, errTooLarge) || !strings.Contains(err.Error(), "line") {
		t.Fatalf("Encode = %v, want a line-size error", err)
	}
}

func TestEncodeTooLargeJSON(t *testing.T) {
	m := sampleMarker()
	m.Data.Packs = nil
	for range 13 {
		m.Data.Packs = append(m.Data.Packs, strings.Repeat("a", MaxString))
	}
	_, err := Encode(m)
	if !errors.Is(err, errTooLarge) || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("Encode = %v, want a JSON-size error", err)
	}
}

func TestChangeJSON(t *testing.T) {
	js, err := json.Marshal(Change{Path: "a<b>", From: "", Mode: "100644", To: "ff"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(js), `["a`+uEsc("003c")+`b`+uEsc("003e")+`","","100644","ff"]`; got != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
	var c Change
	if err := json.Unmarshal([]byte(`["p","a","","" ]`), &c); err != nil || c != (Change{Path: "p", From: "a"}) {
		t.Errorf("Unmarshal = %+v, %v", c, err)
	}
	for _, bad := range []string{`null`, `[]`, `["a","b","c"]`, `["a","b","c","d","e"]`, `["a",null,"c","d"]`, `["a","b","c",1]`, `{"path":"a"}`, `"a"`} {
		var c Change
		if err := json.Unmarshal([]byte(bad), &c); err == nil {
			t.Errorf("Unmarshal(%s) accepted it: %+v", bad, c)
		}
	}
}
