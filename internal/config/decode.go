package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// maxConfigSize bounds every config file touchmark reads. The opt-in file
// comes from targets, so it is untrusted input.
const maxConfigSize = 64 << 10

// Limits on the YAML tree, checked before the document is decoded. Aliases
// can expand a small file into a large tree, so both are bounded.
const (
	maxAliases = 100
	maxNodes   = 100_000
)

// document describes a decoded config file.
type document struct {
	// empty is true for an empty, whitespace-only or comment-only file.
	empty bool
	// keys holds the path of every mapping key present in the file, such as
	// "version", "limits.max_new_prs_per_run" or "targets[2].forks". A key
	// with a null value is present.
	keys map[string]bool
}

// has reports whether the key at path is present in the file.
func (d document) has(path string) bool { return d.keys[path] }

// decodeStrict decodes one YAML document into v, rejecting unknown keys,
// multiple documents and oversized input, and reports which keys are
// present. An empty or comment-only document leaves v unchanged.
//
// Scalars must have the type of the field they land in: a number is not a
// string, "yes" is not a boolean and 1.5 is not an integer. Null is accepted
// for mappings, lists and optional values, where it means "absent", and
// rejected for strings, numbers, booleans and list items. These are the
// rules of the JSON Schemas in schemas/, which yaml.v3 alone does not
// enforce.
func decodeStrict(name string, data []byte, v any) (document, error) {
	doc := document{keys: map[string]bool{}}
	if len(data) > maxConfigSize {
		return doc, fmt.Errorf("%s: larger than %d bytes", name, maxConfigSize)
	}
	// YAML rejects some whitespace-only input, such as a line with a tab.
	if len(bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))) == 0 {
		doc.empty = true
		return doc, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			doc.empty = true
			return doc, nil
		}
		return doc, fmt.Errorf("%s: %s", name, yamlError(err))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return doc, fmt.Errorf("%s: more than one YAML document", name)
	}
	w := walker{name: name, keys: doc.keys}
	if err := w.walk(&root, reflect.TypeOf(v).Elem(), ""); err != nil {
		return doc, err
	}
	dec = yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return doc, fmt.Errorf("%s: %s", name, yamlError(err))
	}
	return doc, nil
}

// walker checks a YAML node tree against the Go type it will be decoded
// into and records the keys it sees.
type walker struct {
	name    string
	keys    map[string]bool
	nodes   int
	aliases int
}

func (w *walker) errorf(n *yaml.Node, path, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if path != "" {
		msg = path + ": " + msg
	}
	return fmt.Errorf("%s:%d: %s", w.name, n.Line, msg)
}

func (w *walker) walk(n *yaml.Node, t reflect.Type, path string) error {
	w.nodes++
	if w.nodes > maxNodes {
		return w.errorf(n, path, "more than %d YAML nodes", maxNodes)
	}
	if n.Kind == yaml.AliasNode {
		n = w.deref(n)
	}
	if w.aliases > maxAliases {
		return w.errorf(n, path, "more than %d YAML aliases", maxAliases)
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return w.walk(n.Content[0], t, path)
	}
	if isNull(n) {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Struct, reflect.Interface:
			return nil
		}
		return w.errorf(n, path, "expected %s, got null", describeType(t))
	}
	switch t.Kind() {
	case reflect.Pointer:
		return w.walk(n, t.Elem(), path)
	case reflect.Interface:
		return nil
	case reflect.Struct:
		return w.walkStruct(n, t, path)
	case reflect.Map:
		return w.walkMap(n, t, path)
	case reflect.Slice:
		return w.walkSlice(n, t, path)
	}
	if n.Kind == yaml.ScalarNode && scalarFits(n, t) {
		return nil
	}
	return w.errorf(n, path, "expected %s, got %s", describeType(t), describeNode(n))
}

func (w *walker) walkStruct(n *yaml.Node, t reflect.Type, path string) error {
	if n.Kind != yaml.MappingNode {
		return w.errorf(n, path, "expected a mapping, got %s", describeNode(n))
	}
	fields := yamlFields(t)
	return w.eachPair(n, t, path, func(key string, val *yaml.Node, keyNode *yaml.Node) error {
		ft, ok := fields[key]
		if !ok {
			return w.errorf(keyNode, path, "unknown key %q", key)
		}
		return w.walk(val, ft, joinPath(path, key))
	})
}

func (w *walker) walkMap(n *yaml.Node, t reflect.Type, path string) error {
	if n.Kind != yaml.MappingNode {
		return w.errorf(n, path, "expected a mapping, got %s", describeNode(n))
	}
	return w.eachPair(n, t, path, func(key string, val *yaml.Node, _ *yaml.Node) error {
		return w.walk(val, t.Elem(), joinPath(path, key))
	})
}

// eachPair calls fn for every key of the mapping n, following merge keys
// ("<<") into the mappings they merge. t is the type of n itself.
func (w *walker) eachPair(n *yaml.Node, t reflect.Type, path string, fn func(key string, val, keyNode *yaml.Node) error) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := w.deref(n.Content[i]), n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return w.errorf(k, path, "mapping keys must be scalars")
		}
		if k.ShortTag() == "!!merge" {
			if err := w.walkMerge(val, t, path); err != nil {
				return err
			}
			continue
		}
		w.keys[joinPath(path, k.Value)] = true
		if err := fn(k.Value, val, k); err != nil {
			return err
		}
	}
	return nil
}

// walkMerge checks the value of a merge key: a mapping or a list of
// mappings, each checked as if its keys were written in place.
func (w *walker) walkMerge(val *yaml.Node, t reflect.Type, path string) error {
	val = w.deref(val)
	if val.Kind == yaml.SequenceNode {
		for _, item := range val.Content {
			if err := w.walk(item, t, path); err != nil {
				return err
			}
		}
		return nil
	}
	return w.walk(val, t, path)
}

func (w *walker) walkSlice(n *yaml.Node, t reflect.Type, path string) error {
	if n.Kind != yaml.SequenceNode {
		return w.errorf(n, path, "expected a list, got %s", describeNode(n))
	}
	for i, item := range n.Content {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		// yaml.v3 drops null items from lists of structs; reject them all.
		if isNull(target(item)) {
			return w.errorf(item, itemPath, "expected %s, got null", describeType(t.Elem()))
		}
		if err := w.walk(item, t.Elem(), itemPath); err != nil {
			return err
		}
	}
	return nil
}

// deref follows an alias, counting it against the alias limit.
func (w *walker) deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		w.aliases++
		n = n.Alias
	}
	return n
}

// target returns the node an alias points to, or n itself.
func target(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// scalarFits reports whether the scalar n decodes into a value of type t
// without conversion.
func scalarFits(n *yaml.Node, t reflect.Type) bool {
	tag := n.ShortTag()
	switch t.Kind() {
	case reflect.String:
		// yaml.v3 decodes timestamps into strings as written.
		return tag == "!!str" || tag == "!!timestamp"
	case reflect.Bool:
		return tag == "!!bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if tag == "!!int" {
			return true
		}
		var f float64
		return tag == "!!float" && n.Decode(&f) == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
	case reflect.Float32, reflect.Float64:
		if tag == "!!int" {
			return true
		}
		var f float64
		return tag == "!!float" && n.Decode(&f) == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return false
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// yamlFields maps the YAML key of every decodable field of struct type t to
// the field's type, using yaml.v3's naming rules.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func describeType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice:
		return "a list"
	case reflect.Pointer:
		return describeType(t.Elem())
	}
	return "a mapping"
}

func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	}
	switch tag := n.ShortTag(); tag {
	case "!!str":
		return "a string"
	case "!!int":
		return "an integer (" + shortValue(n.Value) + ")"
	case "!!float":
		return "a number (" + shortValue(n.Value) + ")"
	case "!!bool":
		return "a boolean (" + shortValue(n.Value) + ")"
	case "!!null":
		return "null"
	default:
		return "a " + tag + " value"
	}
}

// shortValue returns the first 20 characters of a scalar from the file,
// with anything unprintable escaped.
func shortValue(s string) string {
	if utf8.RuneCountInString(s) > 20 {
		s = string([]rune(s)[:20]) + "…"
	}
	return escapeUnprintable(s)
}
