package distribute

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/schemas"
)

// Every report a test of this package makes passes the schemas of schemas/:
// the helpers that run plan, dry runs and distribute check theirs
// (checkReport), and those that read the report stream check each line's
// target against the report schema's target (checkStreamSchema).

// compiled holds the compiled schemas by name and JSON pointer.
var compiled sync.Map

// schemaOf returns the schema name ("report", "doctor") compiled at
// pointer ("" for the document, else e.g. "/$defs/target").
func schemaOf(name, pointer string) (*jsonschema.Schema, error) {
	key := name + "#" + pointer
	if s, ok := compiled.Load(key); ok {
		return s.(*jsonschema.Schema), nil
	}
	raw, ok := schemas.Get(name)
	if !ok {
		return nil, errors.New("no schema " + name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	url := schemas.BaseURL + name + ".schema.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(url + "#" + pointer)
	if err != nil {
		return nil, err
	}
	compiled.Store(key, s)
	return s, nil
}

// validateJSON checks data against the schema name at pointer.
func validateJSON(name, pointer string, data []byte) error {
	s, err := schemaOf(name, pointer)
	if err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := s.Validate(v); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			if out, merr := json.Marshal(ve.BasicOutput()); merr == nil {
				return errors.New(string(out))
			}
		}
		return err
	}
	return nil
}

// checkReport fails the test unless rep, written as the CLI writes it
// (report.WriteJSON), passes schemas/report.schema.json.
func checkReport(t testing.TB, rep *report.Delivery) {
	t.Helper()
	if rep == nil {
		return
	}
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if err := validateJSON("report", "", buf.Bytes()); err != nil {
		t.Errorf("the report schema refuses the %s report: %v", rep.Command, err)
	}
}

// checkDoctorReport fails the test unless doc passes
// schemas/doctor.schema.json.
func checkDoctorReport(t testing.TB, doc *report.Doctor) {
	t.Helper()
	if doc == nil {
		return
	}
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, doc); err != nil {
		t.Fatal(err)
	}
	if err := validateJSON("doctor", "", buf.Bytes()); err != nil {
		t.Errorf("the doctor schema refuses the report: %v", err)
	}
}

// checkStreamSchema fails the test unless every line of a report stream
// holds a target the report schema's target accepts, and ops its op
// accepts.
func checkStreamSchema(t testing.TB, stream string) {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSuffix(stream, "\n"), "\n") {
		if line == "" {
			continue
		}
		var l struct {
			Target json.RawMessage   `json:"target"`
			Ops    []json.RawMessage `json:"ops"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Errorf("stream line %q: %v", line, err)
			continue
		}
		if err := validateJSON("report", "/$defs/target", l.Target); err != nil {
			t.Errorf("the report schema refuses the target of stream line %s: %v", line, err)
		}
		for _, op := range l.Ops {
			if err := validateJSON("report", "/$defs/op", op); err != nil {
				t.Errorf("the report schema refuses an op of stream line %s: %v", line, err)
			}
		}
	}
}
