package marker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// MarshalJSON encodes the change as ["path","from","mode","to"].
func (c Change) MarshalJSON() ([]byte, error) {
	return json.Marshal([4]string{c.Path, c.From, c.Mode, c.To})
}

// UnmarshalJSON decodes an array of exactly four strings; null, a shorter or
// longer array and null elements are errors.
func (c *Change) UnmarshalJSON(b []byte) error {
	var a []*string
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("a change must be an array of four strings: %w", err)
	}
	if len(a) != 4 || slices.Contains(a, nil) {
		return errors.New("a change must be an array of four strings")
	}
	*c = Change{Path: *a[0], From: *a[1], Mode: *a[2], To: *a[3]}
	return nil
}

// Keys a payload object may hold, from the json tags: encoding/json matches
// keys case-insensitively and keeps the last of repeated keys, so
// unmarshalData checks them itself first. A key is required (true) unless
// its tag says omitempty: Encode writes every other key, so a payload
// without one was not written by touchmark whole.
var (
	dataKeys   = jsonKeys(reflect.TypeFor[Data]())
	closedKeys = jsonKeys(reflect.TypeFor[Closed]())
)

// jsonKeys returns the JSON names of the fields of the struct type t, each
// mapped to whether it is required (its tag has no omitempty).
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := range t.NumField() {
		name, opts, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		keys[name] = opts != "omitempty"
	}
	return keys
}

// unmarshalData decodes the payload strictly: valid UTF-8, one JSON object
// with exactly the keys Encode writes (exact case, none repeated, none
// missing but hub_repo and closed, the same inside "closed"), nothing after
// it, and at most MaxChanges changes. Empty arrays become nil.
func unmarshalData(js []byte) (Data, error) {
	if !utf8.Valid(js) {
		return Data{}, errors.New("data is not valid UTF-8")
	}
	if err := checkKeys(js, dataKeys, map[string]map[string]bool{"closed": closedKeys}); err != nil {
		return Data{}, fmt.Errorf("data: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	var d Data
	if err := dec.Decode(&d); err != nil {
		return Data{}, fmt.Errorf("data: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Data{}, errors.New("data: content after the JSON object")
	}
	if len(d.Changes) > MaxChanges {
		return Data{}, fmt.Errorf("data: %d changes, more than %d", len(d.Changes), MaxChanges)
	}
	if len(d.Packs) == 0 {
		d.Packs = nil
	}
	if len(d.Changes) == 0 {
		d.Changes = nil
	}
	if len(d.LabelsSet) == 0 {
		d.LabelsSet = nil
	}
	return d, nil
}

// checkKeys fails unless js is a JSON object whose keys are all in allowed,
// spelled exactly, each at most once, with every key allowed maps to true
// present. A key listed in nested whose value is an object is checked the
// same way against nested[key]. Values are not checked otherwise: the
// decoder that follows does it.
func checkKeys(js []byte, allowed map[string]bool, nested map[string]map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(js))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		_, known := allowed[key]
		switch {
		case !known:
			return fmt.Errorf("unknown key %.40q", key)
		case seen[key]:
			return fmt.Errorf("repeated key %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
		value = bytes.TrimLeft(value, " \t\r\n")
		if sub, ok := nested[key]; ok && len(value) > 0 && value[0] == '{' {
			if err := checkKeys(value, sub, nil); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allowed)) {
		if allowed[key] && !seen[key] {
			return fmt.Errorf("missing key %q", key)
		}
	}
	return nil
}
