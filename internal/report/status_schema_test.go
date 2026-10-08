package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/schemas"
)

// compileSchema compiles the embedded schema called name.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	data, ok := schemas.Get(name)
	if !ok {
		t.Fatalf("no %s schema", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	url := schemas.BaseURL + name + ".schema.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

// validateJSON encodes v as the commands print it and validates it.
func validateJSON(t *testing.T, sch *jsonschema.Schema, v any) error {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, v); err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return sch.Validate(doc)
}

func TestStatusSchema(t *testing.T) {
	sch := compileSchema(t, "status")
	// applied() as the CLI prints it: status and apply never print a null
	// list.
	ap := applied()
	ap.Schema, ap.Target.OptIn, ap.Selection.Unresolved = SyncSchema, OptInFile, []string{}
	ap.Entries[0].To, ap.Entries[0].Mode = "da9823e8fe7223ced65fd1d888c4086c5663d588", "100644"
	if err := validateJSON(t, sch, ap); err != nil {
		t.Errorf("apply: %v", err)
	}
	none := &Sync{Schema: SyncSchema, Command: "status", Hub: Hub{Dir: "/src/hub"},
		Target: Target{Root: "/src/web", OptIn: OptInNone}, Entries: []Entry{}, Warnings: []string{}}
	if err := validateJSON(t, sch, none); err != nil {
		t.Errorf("a target not opted in: %v", err)
	}

	bad := map[string]func(s *Sync){
		"no schema":         func(s *Sync) { s.Schema = "" },
		"another schema":    func(s *Sync) { s.Schema = "status/v2" },
		"unknown state":     func(s *Sync) { s.Entries[0].State = "stale" },
		"unknown action":    func(s *Sync) { s.Entries[0].Action = "move" },
		"unknown opt_in":    func(s *Sync) { s.Target.OptIn = "maybe" },
		"a short blob id":   func(s *Sync) { s.Entries[0].To = "da9823e" },
		"an unknown mode":   func(s *Sync) { s.Entries[0].Mode = "120000" },
		"an unknown result": func(s *Sync) { s.Results[0].Outcome = "partial" },
	}
	for name, mutate := range bad {
		s := applied()
		s.Schema, s.Target.OptIn, s.Selection.Unresolved = SyncSchema, OptInFile, []string{}
		mutate(s)
		if err := validateJSON(t, sch, s); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}

// TestStatusSchemaEnums keeps the states, actions and opt_in values of the
// schema identical to Go's.
func TestStatusSchemaEnums(t *testing.T) {
	data, _ := schemas.Get("status")
	var doc struct {
		Properties struct {
			Target struct {
				Properties struct {
					OptIn struct {
						Enum []string `json:"enum"`
					} `json:"opt_in"`
				} `json:"properties"`
			} `json:"target"`
		} `json:"properties"`
		Defs struct {
			State struct {
				Enum []string `json:"enum"`
			} `json:"state"`
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, c := range (Summary{}).stateCounts() {
		states = append(states, string(c.state))
	}
	actions := []string{string(decide.Keep), string(decide.Create), string(decide.Update), string(decide.Delete), string(decide.Adopt), string(decide.Chmod)}
	optIns := []string{OptInFile, OptInAssumed, OptInFlag, OptInDisabled, OptInNone}
	for _, c := range []struct {
		name         string
		schema, inGo []string
	}{
		{"states", doc.Defs.State.Enum, states},
		{"actions", doc.Defs.Action.Enum, actions},
		{"opt_in", doc.Properties.Target.Properties.OptIn.Enum, optIns},
	} {
		if !sameSet(c.schema, c.inGo) {
			t.Errorf("%s: schema %q, Go %q", c.name, c.schema, c.inGo)
		}
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func TestCheckSchema(t *testing.T) {
	sch := compileSchema(t, "check")
	c := &Check{Schema: CheckSchema, Command: "check", Hub: Hub{Dir: "/src/hub", ID: "acme-eng", Commit: "0123456789abcdef0123456789abcdef01234567"},
		Packs: []string{"base"}, Errors: []string{}, Warnings: []string{}}
	if err := validateJSON(t, sch, c); err != nil {
		t.Error(err)
	}
	c.Schema = ""
	if err := validateJSON(t, sch, c); err == nil {
		t.Error("a check report without schema: valid")
	}
}

// TestLocalGoldensValidate validates the JSON reports of status, apply and
// check that the CLI's end-to-end tests produce
// (internal/cli/testdata/golden) against their schemas, with the hub commit
// their placeholder stands for.
func TestLocalGoldensValidate(t *testing.T) {
	schemaOf := map[string]*jsonschema.Schema{"status": compileSchema(t, "status"), "apply": compileSchema(t, "status"), "check": compileSchema(t, "check")}
	files, err := filepath.Glob(filepath.Join("..", "cli", "testdata", "golden", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(data, &head) != nil {
			continue
		}
		sch := schemaOf[head.Command]
		if sch == nil {
			continue
		}
		seen[head.Command]++
		data = bytes.ReplaceAll(data, []byte("$COMMIT"), []byte("0123456789abcdef0123456789abcdef01234567"))
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := sch.Validate(v); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
	for _, cmd := range []string{"status", "apply", "check"} {
		if seen[cmd] == 0 {
			t.Errorf("no %s report under ../cli/testdata/golden; update this test if they moved", cmd)
		}
	}
}
