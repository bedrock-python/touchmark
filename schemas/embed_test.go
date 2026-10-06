package schemas

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestGet(t *testing.T) {
	for _, name := range Names() {
		data, ok := Get(name)
		if !ok || len(data) == 0 {
			t.Fatalf("Get(%q) = %d bytes, %v", name, len(data), ok)
		}
		var doc struct {
			Schema string `json:"$schema"`
			ID     string `json:"$id"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := BaseURL + name + ".schema.json"; doc.ID != want {
			t.Errorf("%s: $id = %q, want %q", name, doc.ID, want)
		}
		if doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema = %q", name, doc.Schema)
		}
	}
	for _, name := range []string{"", "hub.schema", "embed", "../go", "HUB", "operations.schema"} {
		if _, ok := Get(name); ok {
			t.Errorf("Get(%q) succeeded", name)
		}
	}
}

// TestNames: the config and report schemas are registered once each, and
// Names returns a copy.
func TestNames(t *testing.T) {
	got := Names()
	for _, name := range []string{"hub", "targets", "opt-in", "operations", "report", "doctor"} {
		if !slices.Contains(got, name) {
			t.Errorf("Names() = %q lacks %q", got, name)
		}
	}
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != len(got) {
		t.Errorf("Names() = %q has duplicates", got)
	}
	got[0] = "changed"
	if Names()[0] == "changed" {
		t.Error("Names shares memory with the registry")
	}
}
