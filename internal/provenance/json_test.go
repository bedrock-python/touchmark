package provenance

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var (
	oidA   = strings.Repeat("a", 40)
	oidB   = strings.Repeat("b", 40)
	oidD   = strings.Repeat("d", 40)
	commit = strings.Repeat("c", 40)
)

func TestManifestWriteJSON(t *testing.T) {
	t.Parallel()
	m := &Manifest{
		Version:   ManifestVersion,
		HubCommit: commit,
		Paths: map[string]map[string][]Version{
			// Versions keep their order; keys come out sorted; no HTML escaping.
			"docs/a&b.md":   {"agents": {{OID: oidB, Size: 70}, {OID: oidA, Size: 64}}},
			".editorconfig": {"core": {{OID: oidD, Size: 80}}, "base": {{OID: oidD, Size: 80}}},
		},
	}
	want := `{
  "version": 1,
  "hub_commit": "` + commit + `",
  "paths": {
    ".editorconfig": {
      "base": [
        {
          "oid": "` + oidD + `",
          "size": 80
        }
      ],
      "core": [
        {
          "oid": "` + oidD + `",
          "size": 80
        }
      ]
    },
    "docs/a&b.md": {
      "agents": [
        {
          "oid": "` + oidB + `",
          "size": 70
        },
        {
          "oid": "` + oidA + `",
          "size": 64
        }
      ]
    }
  }
}
`
	var buf bytes.Buffer
	if err := m.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != want {
		t.Errorf("WriteJSON() =\n%s\nwant\n%s", buf.String(), want)
	}

	compact, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var wantCompact bytes.Buffer
	if err := json.Compact(&wantCompact, []byte(want)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compact, wantCompact.Bytes()) {
		t.Errorf("MarshalJSON() = %s\nwant %s", compact, wantCompact.Bytes())
	}

	// json.Marshal escapes HTML characters but reads back the same.
	escaped, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{buf.Bytes(), compact, escaped} {
		back, err := ReadManifestJSON(data)
		if err != nil {
			t.Fatalf("ReadManifestJSON(%s): %v", data, err)
		}
		if !reflect.DeepEqual(back, m) {
			t.Errorf("ReadManifestJSON(%s) = %+v", data, back)
		}
	}
}

func TestManifestJSONEmpty(t *testing.T) {
	t.Parallel()
	for _, m := range []*Manifest{
		{Version: ManifestVersion, HubCommit: commit},
		{Version: ManifestVersion, HubCommit: commit, Paths: map[string]map[string][]Version{}},
	} {
		data, err := m.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"version":1,"hub_commit":"` + commit + `","paths":{}}`; string(data) != want {
			t.Errorf("MarshalJSON() = %s, want %s", data, want)
		}
	}
	var nilManifest *Manifest
	if data, err := nilManifest.MarshalJSON(); err != nil || string(data) != "null" {
		t.Errorf("nil MarshalJSON() = %s, %v", data, err)
	}
}

func TestManifestJSONRoundTripFromHub(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	r.write("packs/agents/AGENTS.md", text("v1"))
	r.setEntry("100644", text("docs"), "packs/agents/docs/<x> & y.md") // not a Windows file name
	r.write("packs/core/AGENTS.md", text("core"))
	r.commit("c1")
	r.write("packs/agents/AGENTS.md", text("v2"))
	r.commit("c2")
	m, _, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := m.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifestJSON(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Errorf("round trip =\n%s\nwant\n%s", dump(t, back), buf.String())
	}
	var again bytes.Buffer
	if err := back.WriteJSON(&again); err != nil {
		t.Fatal(err)
	}
	if again.String() != buf.String() {
		t.Errorf("second encoding differs:\n%s\nfirst:\n%s", again.String(), buf.String())
	}
}

// manifestDoc returns a manifest document with paths as the "paths" value.
func manifestDoc(paths string) string {
	return `{"version":1,"hub_commit":"` + commit + `","paths":` + paths + `}`
}

// oneVersion is a valid "paths" value.
var oneVersion = `{"a.md":{"agents":[{"oid":"` + oidA + `","size":64}]}}`

func TestReadManifestJSON(t *testing.T) {
	t.Parallel()
	sha256 := strings.Repeat("e", 64)
	valid := []struct {
		name, doc string
		paths     int
	}{
		{"one version", manifestDoc(oneVersion), 1},
		{"trailing space", manifestDoc(oneVersion) + " \n", 1},
		{"null paths", manifestDoc("null"), 0},
		{"no paths", `{"version":1,"hub_commit":"` + commit + `"}`, 0},
		{"sha256", `{"version":1,"hub_commit":"` + sha256 + `","paths":{"a":{"p":[{"oid":"` + sha256 + `","size":0}]}}}`, 1},
	}
	for _, tt := range valid {
		m, err := ReadManifestJSON([]byte(tt.doc))
		if err != nil {
			t.Errorf("%s: ReadManifestJSON() = %v", tt.name, err)
			continue
		}
		if m.Paths == nil || len(m.Paths) != tt.paths {
			t.Errorf("%s: Paths = %#v", tt.name, m.Paths)
		}
	}

	invalid := []struct{ name, doc string }{
		{"not JSON", "version: 1"},
		{"empty", ""},
		{"null", "null"},
		{"array", "[]"},
		{"no version", `{"hub_commit":"` + commit + `","paths":{}}`},
		{"version 2", `{"version":2,"hub_commit":"` + commit + `","paths":{}}`},
		{"version string", `{"version":"1","hub_commit":"` + commit + `","paths":{}}`},
		{"no hub commit", `{"version":1,"paths":{}}`},
		{"short hub commit", `{"version":1,"hub_commit":"abc","paths":{}}`},
		{"upper-case hub commit", `{"version":1,"hub_commit":"` + strings.ToUpper(commit) + `","paths":{}}`},
		{"unknown field", `{"version":1,"hub_commit":"` + commit + `","paths":{},"extra":1}`},
		{"trailing object", manifestDoc("{}") + "{}"},
		{"trailing garbage", manifestDoc("{}") + "x"},
		{"paths array", manifestDoc("[]")},
		{"invalid path", manifestDoc(`{"../a":{"agents":[{"oid":"` + oidA + `","size":64}]}}`)},
		{"path in .git", manifestDoc(`{".git/hooks/x":{"agents":[{"oid":"` + oidA + `","size":64}]}}`)},
		{"control character", manifestDoc(`{"a\u0007":{"agents":[{"oid":"` + oidA + `","size":64}]}}`)},
		{"no packs", manifestDoc(`{"a.md":{}}`)},
		{"null packs", manifestDoc(`{"a.md":null}`)},
		{"invalid pack name", manifestDoc(`{"a.md":{"Agents":[{"oid":"` + oidA + `","size":64}]}}`)},
		{"no versions", manifestDoc(`{"a.md":{"agents":[]}}`)},
		{"null versions", manifestDoc(`{"a.md":{"agents":null}}`)},
		{"bad oid", manifestDoc(`{"a.md":{"agents":[{"oid":"xyz","size":64}]}}`)},
		{"oid of another hash", manifestDoc(`{"a.md":{"agents":[{"oid":"` + sha256 + `","size":64}]}}`)},
		{"duplicate version", manifestDoc(`{"a.md":{"agents":[{"oid":"` + oidA + `","size":64},{"oid":"` + oidA + `","size":64}]}}`)},
		{"negative size", manifestDoc(`{"a.md":{"agents":[{"oid":"` + oidA + `","size":-1}]}}`)},
		{"fractional size", manifestDoc(`{"a.md":{"agents":[{"oid":"` + oidA + `","size":1.5}]}}`)},
		{"unknown version field", manifestDoc(`{"a.md":{"agents":[{"oid":"` + oidA + `","size":64,"mode":"100644"}]}}`)},
	}
	for _, tt := range invalid {
		if m, err := ReadManifestJSON([]byte(tt.doc)); err == nil {
			t.Errorf("%s: ReadManifestJSON() = %+v; want error", tt.name, m)
		}
	}
}

func FuzzReadManifestJSON(f *testing.F) {
	f.Add([]byte(manifestDoc(oneVersion)))
	f.Add([]byte(manifestDoc("{}")))
	f.Add([]byte(manifestDoc(`{"docs/<a>.md":{"agents":[{"oid":"` + oidA + `","size":64},{"oid":"` + oidB + `","size":0}],"core":[{"oid":"` + oidD + `","size":9}]}}`)))
	f.Add([]byte(`{"VERSION":1,"Hub_Commit":"` + commit + `","paths":null}`))
	f.Add([]byte("null"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ReadManifestJSON(data)
		if err != nil {
			return
		}
		enc, err := m.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON: %v", err)
		}
		back, err := ReadManifestJSON(enc)
		if err != nil {
			t.Fatalf("ReadManifestJSON(MarshalJSON(m)) = %v\n%s", err, enc)
		}
		if !reflect.DeepEqual(back, m) {
			t.Fatalf("round trip changed the manifest:\n%+v\n%+v", m, back)
		}
		again, err := back.MarshalJSON()
		if err != nil || !bytes.Equal(again, enc) {
			t.Fatalf("encoding is not stable: %s vs %s (%v)", again, enc, err)
		}
	})
}

func TestManifestAddAndVersions(t *testing.T) {
	t.Parallel()
	var m Manifest
	if got := m.Versions("a.md", "agents"); got != nil {
		t.Errorf("Versions on an empty manifest = %v", got)
	}
	var nilManifest *Manifest
	if got := nilManifest.Versions("a.md", "agents"); got != nil {
		t.Errorf("Versions on nil = %v", got)
	}
	m.Add("a.md", "agents", Version{OID: oidB, Size: 70})
	m.Add("a.md", "agents", Version{OID: oidA, Size: 64})
	m.Add("a.md", "agents", Version{OID: oidB, Size: 70})
	m.Add("a.md", "core", Version{OID: oidA, Size: 64})
	want := []Version{{OID: oidB, Size: 70}, {OID: oidA, Size: 64}}
	if got := m.Versions("a.md", "agents"); !reflect.DeepEqual(got, want) {
		t.Errorf("Versions(agents) = %v, want %v", got, want)
	}
	if got := m.Versions("a.md", "core"); len(got) != 1 {
		t.Errorf("Versions(core) = %v", got)
	}
	if got := m.Versions("b.md", "agents"); got != nil {
		t.Errorf("Versions(b.md) = %v", got)
	}
}

func TestAliasesExpand(t *testing.T) {
	t.Parallel()
	a := Aliases{"agents": {"agent-docs", "ai"}, "core": {"base"}}
	got := a.Expand([]string{"core", "agents", "core", "python"})
	want := []string{"core", "agents", "python", "base", "agent-docs", "ai"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand() = %v, want %v", got, want)
	}
	if got := Aliases(nil).Expand([]string{"a"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("nil Expand() = %v", got)
	}
}
