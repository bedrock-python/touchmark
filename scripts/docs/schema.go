package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// node is a JSON value that keeps the order of an object's keys, which is
// the order the schemas list their fields in.
type node struct {
	members []member // an object
	items   []*node  // an array
	value   any      // a string, json.Number, bool or nil
	kind    byte     // 'o' object, 'a' array, 'v' anything else
}

type member struct {
	key   string
	value *node
}

func parseJSON(data []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return decodeNode(dec)
}

func decodeNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return &node{kind: 'v', value: tok}, nil
	}
	n := &node{kind: 'a'}
	if d == '{' {
		n.kind = 'o'
	}
	for dec.More() {
		if n.kind == 'o' {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeNode(dec)
			if err != nil {
				return nil, err
			}
			n.members = append(n.members, member{key.(string), v})
			continue
		}
		v, err := decodeNode(dec)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, v)
	}
	_, err = dec.Token() // the closing delimiter
	return n, err
}

// get returns the value of key in an object, or nil.
func (n *node) get(key string) *node {
	if n == nil || n.kind != 'o' {
		return nil
	}
	for _, m := range n.members {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

// str returns the string value of key, or "".
func (n *node) str(key string) string {
	if v := n.get(key); v != nil {
		if s, ok := v.value.(string); ok {
			return s
		}
	}
	return ""
}

// strs returns the value of key as strings: a string, or an array of them.
func (n *node) strs(key string) []string {
	v := n.get(key)
	switch {
	case v == nil:
		return nil
	case v.kind == 'a':
		var out []string
		for _, it := range v.items {
			if s, ok := it.value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := v.value.(string); ok {
		return []string{s}
	}
	return nil
}

// isTrue reports whether key is the boolean true.
func (n *node) isTrue(key string) bool {
	if v := n.get(key); v != nil {
		b, ok := v.value.(bool)
		return ok && b
	}
	return false
}

// number returns the numeric value of key.
func (n *node) number(key string) (float64, bool) {
	if v := n.get(key); v != nil {
		if num, ok := v.value.(json.Number); ok {
			f, err := num.Float64()
			return f, err == nil
		}
	}
	return 0, false
}

// literal renders a JSON scalar as the configuration would spell it.
func literal(n *node) string {
	switch v := n.value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	}
	return fmt.Sprint(n.value)
}

// schemaDoc renders one schema as a table of its fields.
type schemaDoc struct {
	root *node
	rows [][3]string
	err  error
}

// schemaTable renders the fields of a JSON Schema as a Markdown table with a
// row for every field, nested fields under dotted keys: `a.b`, `a[].b` for
// the objects of a list, `a.<name>.b` for the values of a map.
func schemaTable(data []byte) (string, error) {
	root, err := parseJSON(data)
	if err != nil {
		return "", err
	}
	d := &schemaDoc{root: root}
	d.object("", root)
	if d.err != nil {
		return "", d.err
	}
	if len(d.rows) == 0 {
		return "", errors.New("the schema has no fields")
	}
	var b strings.Builder
	b.WriteString("| Key | Type | Description |\n|---|---|---|\n")
	for _, r := range d.rows {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r[0], r[1], r[2])
	}
	return b.String(), nil
}

// view is a schema with its $ref followed.
type view struct {
	s          *node
	def        string // the $defs entry it names, "" when inline
	desc       string // its own description, else the definition's
	deprecated bool
}

func (d *schemaDoc) view(s *node) view {
	v := view{s: s, desc: s.str("description"), deprecated: s.isTrue("deprecated")}
	for seen := 0; v.s.str("$ref") != ""; seen++ {
		ref := v.s.str("$ref")
		name, ok := strings.CutPrefix(ref, "#/$defs/")
		target := d.root.get("$defs").get(name)
		if !ok || target == nil || seen > 16 {
			d.fail(fmt.Errorf("cannot follow $ref %q", ref))
			return v
		}
		if v.def == "" {
			v.def = name
		}
		v.s = target
		if v.desc == "" {
			v.desc = target.str("description")
		}
		v.deprecated = v.deprecated || target.isTrue("deprecated")
	}
	return v
}

func (d *schemaDoc) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

// field is one property of an object, merged across the alternatives of a
// oneOf.
type field struct {
	name     string
	s        *node
	required bool
	in       []string // the alternatives that have it, by label; nil when all do
	altDesc  string   // the description of the alternative that brought it
	label    bool     // it names its alternative: the key that alternative requires
}

// fields lists the properties of an object schema: its own, then those of
// its oneOf alternatives, merged by name. An alternative is labelled by the
// one key it requires (repo, org or group in targets.yml).
func (d *schemaDoc) fields(s *node) []field {
	var out []field
	required := s.strs("required")
	if props := s.get("properties"); props != nil {
		for _, m := range props.members {
			out = append(out, field{name: m.key, s: m.value, required: slices.Contains(required, m.key)})
		}
	}
	alts := s.get("oneOf")
	if alts == nil {
		return out
	}
	byName := map[string]int{}
	for i, f := range out {
		byName[f.name] = i
	}
	labels := 0
	for _, a := range alts.items {
		av := d.view(a)
		label := ""
		if req := av.s.strs("required"); len(req) == 1 {
			label = req[0]
			labels++
		}
		for _, f := range d.fields(av.s) {
			if i, ok := byName[f.name]; ok {
				out[i].in = append(out[i].in, label)
				continue
			}
			f.in = []string{label}
			f.altDesc = av.desc
			f.label = f.name == label
			if f.label {
				f.required = false
			}
			byName[f.name] = len(out)
			out = append(out, f)
		}
	}
	for i := range out {
		// A field all alternatives have needs no note, and neither does
		// any field when an alternative has no label to name.
		if len(out[i].in) == len(alts.items) || labels != len(alts.items) {
			out[i].in = nil
			out[i].label = false
		}
	}
	return out
}

// object adds the rows of an object schema's fields under prefix.
func (d *schemaDoc) object(prefix string, s *node) {
	fields := d.fields(s)
	var labels []string
	for _, f := range fields {
		if f.label {
			labels = append(labels, "`"+f.name+"`")
		}
	}
	for _, f := range fields {
		v := d.view(f.s)
		key := prefix + f.name
		typ := d.typeOf(v.s)
		if f.required {
			typ += ", required"
		}
		desc := v.desc
		if desc == "" {
			desc = f.altDesc
		}
		desc = cell(desc)
		if v.deprecated {
			desc = strings.TrimSpace("Deprecated. " + desc)
		}
		switch {
		case f.label:
			desc = strings.TrimSpace(desc + " An entry has exactly one of " + orList(labels, "and") + ".")
		case f.in != nil:
			var in []string
			for _, l := range f.in {
				in = append(in, "`"+l+"`")
			}
			desc = strings.TrimSpace(desc + " Only with " + orList(in, "or") + ".")
		}
		if desc == "" {
			desc = "—"
		}
		d.rows = append(d.rows, [3]string{key, typ, desc})
		d.nested(key, v.s)
	}
}

// nested adds the rows of what a field holds: an object's fields, the
// fields of a list's objects, the fields of a map's object values.
func (d *schemaDoc) nested(key string, s *node) {
	switch {
	case d.isObject(s):
		d.object(key+".", s)
	case slices.Contains(s.strs("type"), "array") && s.get("items") != nil:
		if items := d.view(s.get("items")).s; d.isObject(items) {
			d.object(key+"[].", items)
		}
	case d.isMap(s):
		if values := d.view(s.get("additionalProperties")).s; d.isObject(values) {
			d.object(key+".<"+d.placeholder(s)+">.", values)
		}
	}
}

// isObject reports whether s is an object with fields of its own or of its
// alternatives.
func (d *schemaDoc) isObject(s *node) bool {
	if s.get("properties") != nil {
		return true
	}
	if alts := s.get("oneOf"); alts != nil {
		for _, a := range alts.items {
			if d.isObject(d.view(a).s) {
				return true
			}
		}
	}
	return false
}

// isMap reports whether s is an object whose keys are names it does not
// list.
func (d *schemaDoc) isMap(s *node) bool {
	a := s.get("additionalProperties")
	return a != nil && a.kind == 'o' && s.get("properties") == nil
}

// placeholder names the keys of a map after their schema: a packName key is
// <pack>, a providerId key <provider>.
func (d *schemaDoc) placeholder(s *node) string {
	name := d.view(s.get("propertyNames")).def
	if s.get("propertyNames") == nil || name == "" {
		return "name"
	}
	lower := strings.ToLower(name)
	for _, suffix := range []string{"name", "id"} {
		if trimmed, ok := strings.CutSuffix(lower, suffix); ok && trimmed != "" {
			return trimmed
		}
	}
	return lower
}

// typeOf describes a resolved schema's type for the Type column.
func (d *schemaDoc) typeOf(s *node) string {
	if c := s.get("const"); c != nil {
		return "`" + literal(c) + "`"
	}
	if e := s.get("enum"); e != nil {
		var vals []string
		for _, it := range e.items {
			vals = append(vals, "`"+literal(it)+"`")
		}
		return "one of " + strings.Join(vals, ", ")
	}
	var types []string
	for _, t := range s.strs("type") {
		if t != "null" {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		if d.isObject(s) {
			return "object"
		}
		return "any"
	}
	if len(types) > 1 {
		return strings.Join(types, " or ")
	}
	switch types[0] {
	case "array":
		return d.listType(s)
	case "object":
		if d.isMap(s) {
			values := d.view(s.get("additionalProperties")).s
			of := "object"
			if !d.isObject(values) {
				of = d.typeOf(values)
			}
			return "map of " + d.placeholder(s) + " to " + of
		}
		return "object"
	case "integer", "number":
		return types[0] + bounds(s)
	case "string":
		return "string" + stringLimits(s)
	}
	return types[0]
}

// listType describes an array: "list of strings", "list of objects".
func (d *schemaDoc) listType(s *node) string {
	items := s.get("items")
	of := "values"
	if items != nil {
		iv := d.view(items).s
		switch t := d.typeOf(iv); {
		case d.isObject(iv):
			of = "objects"
		case strings.HasPrefix(t, "one of "):
			of = "values, each " + t
		case strings.HasPrefix(t, "string, "):
			of = "strings, each " + strings.TrimPrefix(t, "string, ")
		case strings.HasPrefix(t, "string"):
			of = "strings" + strings.TrimPrefix(t, "string")
		case strings.HasPrefix(t, "integer "):
			of = "integers, each " + strings.TrimPrefix(t, "integer ")
		case t == "integer":
			of = "integers"
		default:
			of = t
		}
	}
	out := "list of " + of
	if max, ok := s.number("maxItems"); ok {
		out += fmt.Sprintf(", at most %g", max)
	}
	return out
}

// bounds describes the limits of a number: " ≥ 0", " > 0, ≤ 1". A maximum
// that only guards the integer type is left out.
func bounds(s *node) string {
	var parts []string
	if v, ok := s.number("minimum"); ok {
		parts = append(parts, fmt.Sprintf("≥ %g", v))
	}
	if v, ok := s.number("exclusiveMinimum"); ok {
		parts = append(parts, fmt.Sprintf("> %g", v))
	}
	if v, ok := s.number("maximum"); ok && v <= 1e6 {
		parts = append(parts, fmt.Sprintf("≤ %g", v))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, ", ")
}

// stringLimits describes the length and format of a string.
func stringLimits(s *node) string {
	lo, hasLo := s.number("minLength")
	hi, hasHi := s.number("maxLength")
	out := ""
	switch {
	case hasLo && hasHi:
		out = fmt.Sprintf(", %g to %g characters", lo, hi)
	case hasHi:
		out = fmt.Sprintf(", up to %g characters", hi)
	case hasLo && lo == 1:
		out = ", not empty"
	case hasLo:
		out = fmt.Sprintf(", at least %g characters", lo)
	}
	if f := s.str("format"); f != "" {
		out += " (" + f + ")"
	}
	return out
}

// orList joins items as "a, b or c" (or "and").
func orList(items []string, conj string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
}
