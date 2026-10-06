//go:build e2e

package gitlabe2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/bedrock-python/touchmark/schemas"
)

// validate fails the test unless data, a report the command line printed,
// passes the embedded schema name ("report", "doctor"): the reports pass
// the schema, the live ones too. redact masks secrets in
// the message.
func validate(t *testing.T, name string, data []byte, redact func(string) string) {
	t.Helper()
	raw, ok := schemas.Get(name)
	if !ok {
		t.Fatalf("no schema %s", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
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
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the %s report is no JSON: %v", name, err)
	}
	if err := sch.Validate(v); err != nil {
		detail := err.Error()
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			if out, err := json.Marshal(ve.BasicOutput()); err == nil {
				detail = string(out)
			}
		}
		t.Errorf("the %s schema refuses the report: %s\n%s", name, redact(detail), redact(string(data)))
	}
}
