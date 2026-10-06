package marker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// Fingerprints of the tests: ours, the one before the hub moved, and
// another hub's.
const (
	ourFP   = "github.com/712345678"
	prevFP  = "gitlab.example.com/1234"
	otherFP = "github.com/999"
)

// Ids of the tests.
const (
	key1       = "sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58"
	key2       = "sha256:0f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a4938"
	hubCommit  = "3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3"
	bodyCommit = "a91b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d"
	baseCommit = "9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f7081920"
	optInHash  = "sha256:5e7a0b1c2d3e4f5061728394a5b6c7d8e9f00112233445566778899aabbccdd"
	bodyHash   = "sha256:c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
)

// sampleData is a valid payload of our hub with every kind of change.
func sampleData() Data {
	return Data{
		V:             Version,
		Stream:        "sync",
		Hub:           "acme-eng",
		FP:            ourFP,
		HubRepo:       "github.com/acme/engineering-assets",
		DecidedAt:     hubCommit,
		ContentCommit: bodyCommit,
		Base:          baseCommit,
		OptIn:         optInHash,
		Engine:        "0.2.0",
		Packs:         []string{"agents", "claude"},
		Changes: []Change{
			{Path: "AGENTS.md", From: "77ab0c1d2e3f4a5b", Mode: "100644", To: "8f3c1a0b2e7d4a61"},
			{Path: "prompts/old.md", From: "9a01bc23de45f678", Mode: "", To: ""},
			{Path: "prompts/review.md", From: "", Mode: "100644", To: "5e7a2b3c4d5e6f70"},
			{Path: "scripts/check.sh", From: "0a1b2c3d4e5f6071", Mode: "100755", To: "0a1b2c3d4e5f6071"},
		},
		ChangesComplete: true,
		TitleSet:        "chore: sync engineering assets",
		Body:            bodyHash,
		LabelsSet:       []string{"engineering-assets"},
	}
}

// sampleMarker is a valid marker of our hub.
func sampleMarker() Marker { return Marker{Key: key1, Data: sampleData()} }

// markerOf is sampleMarker with another key and fingerprint.
func markerOf(key, fp string) Marker {
	m := sampleMarker()
	m.Key, m.Data.FP = key, fp
	return m
}

// mustEncode encodes m and fails the test on error.
func mustEncode(t testing.TB, m Marker) string {
	t.Helper()
	line, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return line
}

// written returns m as Encode writes it: header attributes filled from the
// data and empty slices as nil, which is what Parse returns.
func written(m Marker) Marker {
	m.Hub, m.Stream, m.FP16 = m.Data.Hub, m.Data.Stream, FP16(m.Data.FP)
	if len(m.Data.Packs) == 0 {
		m.Data.Packs = nil
	}
	if len(m.Data.Changes) == 0 {
		m.Data.Changes = nil
	}
	if len(m.Data.LabelsSet) == 0 {
		m.Data.LabelsSet = nil
	}
	return m
}

// dropped returns m without its changes, as Encode writes it when they do
// not fit.
func dropped(m Marker) Marker {
	m = written(m)
	m.Data.Changes, m.Data.ChangesComplete = nil, false
	return m
}

// equalMarkers fails the test unless got equals want.
func equalMarkers(t testing.TB, got, want Marker) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("marker differs\n got %s\nwant %s", describeMarker(got), describeMarker(want))
	}
}

// describeMarker renders m for failure messages.
func describeMarker(m Marker) string {
	js, _ := json.Marshal(m.Data)
	return "hub=" + m.Hub + " fp=" + m.FP16 + " stream=" + m.Stream + " key=" + m.Key + " data=" + string(js)
}

// gz compresses b the way Encode does.
func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := newGzipWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipWriters.Put(zw)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// header is the valid header of sampleMarker, up to "data=".
func header() string {
	return v1Prefix + "hub=acme-eng fp=" + FP16(ourFP) + " stream=sync key=" + key1 + " data="
}

// lineWithData returns the sample header with raw (gzip bytes) as data.
func lineWithData(raw []byte) string {
	return header() + base64.StdEncoding.EncodeToString(raw) + commentEnd
}

// lineWithJSON returns the sample header with js as the payload.
func lineWithJSON(t testing.TB, js string) string {
	t.Helper()
	return lineWithData(gz(t, []byte(js)))
}

// sampleJSON is the payload of sampleMarker as Encode writes it.
func sampleJSON(t testing.TB) string {
	t.Helper()
	js, err := json.Marshal(wire(sampleData()))
	if err != nil {
		t.Fatal(err)
	}
	return string(js)
}

// jsonWith returns sampleJSON after edit changed its decoded form.
func jsonWith(t testing.TB, edit func(map[string]any)) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(sampleJSON(t)), &obj); err != nil {
		t.Fatal(err)
	}
	edit(obj)
	js, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return string(js)
}

// payload returns the JSON inside the data attribute of line.
func payload(t testing.TB, line string) string {
	t.Helper()
	i := strings.LastIndex(line, " data=")
	if i < 0 || !strings.HasSuffix(line, commentEnd) {
		t.Fatalf("no data attribute in %.80q", line)
	}
	js, err := decodeData(line[i+len(" data=") : len(line)-len(commentEnd)])
	if err != nil {
		t.Fatal(err)
	}
	return string(js)
}

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
	if string(want) != got {
		t.Errorf("%s differs from the golden file (go test -update rewrites it)\n--- got\n%s--- want\n%s", path, got, want)
	}
}

// readGolden returns the content of testdata/<name>.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	return string(b)
}

// ptr returns a pointer to s.
func ptr(s string) *string { return &s }

// uEsc returns the JSON escape of the code point with the hex digits h, e.g.
// uEsc("0076") is the escape of "v".
func uEsc(h string) string { return "\x5cu" + h }
