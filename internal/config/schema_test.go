package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/schemas"
)

// fixtureKinds maps each testdata directory, named like its schema, to its
// parser.
var fixtureKinds = []struct {
	name  string
	parse func([]byte) ([]Warning, error)
}{
	{"hub", func(b []byte) ([]Warning, error) { _, w, err := ParseHub(b); return w, err }},
	{"targets", func(b []byte) ([]Warning, error) { _, w, err := ParseTargets(b); return w, err }},
	{"opt-in", func(b []byte) ([]Warning, error) { _, w, err := ParseOptIn(b); return w, err }},
	{"operations", func(b []byte) ([]Warning, error) { _, w, err := ParseOperations(b); return w, err }},
}

// semanticOnly lists the fixtures the schemas accept and the parser
// rejects, with the rule that JSON Schema cannot express. Every other
// fixture validates against its schema exactly when the parser accepts it.
//
// Rules checked only in Go, beyond these fixtures:
//   - strings that look like tokens or private keys (TestParseRejectsSecrets);
//   - provider ids are unique;
//   - an entry's provider agrees with the provider prefix of its selector;
//   - branch names follow git check-ref-format (the schema checks characters);
//   - opt_in_file, pr.intro_file and ca_file pass pathx.Validate (the schema
//     checks slashes, '\' and ':');
//   - ignore and sensitive_paths patterns are not empty after trimming "./"
//     (the schema requires a character other than space, '/' and '\');
//   - commit.message holds no line equal to git's scissors line;
//   - operations.yml dates exist in their month (the schema checks the
//     form, the fixtures below the calendar); duplicate operations are
//     warnings, not errors;
//   - YAML-level rules, which the harness applies to both sides: syntax, one
//     document, duplicate keys, the size limit, the alias and node limits.
var semanticOnly = map[string]string{
	"hub/invalid/provider-duplicate-id.yml":    "provider ids are unique",
	"hub/invalid/branch-dotdot.yml":            "git check-ref-format",
	"hub/invalid/branch-lock.yml":              "git check-ref-format",
	"hub/invalid/opt-in-file-in-git.yml":       "pathx.Validate",
	"hub/invalid/intro-file-dotdot.yml":        "pathx.Validate",
	"hub/invalid/sensitive-path-dot-slash.yml": "pattern empty after trimming ./",
	"hub/invalid/commit-scissors.yml":          "git's scissors line, a whole line after git's cleanup",
	"targets/invalid/provider-conflict.yml":    "provider agrees with the ref prefix",
	"opt-in/invalid/ignore-dot-slash.yml":      "pattern empty after trimming ./",
	"operations/invalid/until-feb-30.yml":      "the day exists in its month",
	"operations/invalid/until-not-leap.yml":    "the day exists in its month",
}

func TestSchemasCompile(t *testing.T) {
	for _, name := range schemas.Names() {
		compileSchema(t, name)
	}
}

// TestFixtures runs every file under testdata/<kind>/{valid,invalid}
// through the parser and the schema. Header comments state expectations:
// "# error: <text>" must appear in the parse error, "# warning: <text>" in
// some warning; a valid fixture without warning lines must parse silently.
func TestFixtures(t *testing.T) {
	seen := map[string]bool{}
	for _, kind := range fixtureKinds {
		sch := compileSchema(t, kind.name)
		for _, dir := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join("testdata", kind.name, dir, "*.yml"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no fixtures in testdata/%s/%s: %v", kind.name, dir, err)
			}
			for _, file := range files {
				name := kind.name + "/" + dir + "/" + filepath.Base(file)
				seen[name] = true
				t.Run(name, func(t *testing.T) {
					data, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					parsed := checkParse(t, kind.parse, data, dir == "valid")
					accepted := schemaAccepts(sch, data)
					if reason, ok := semanticOnly[name]; ok {
						if !accepted || parsed {
							t.Errorf("listed as semantic-only (%s), but schema accepts=%v, parser accepts=%v", reason, accepted, parsed)
						}
						return
					}
					if accepted != parsed {
						t.Errorf("schema accepts=%v, parser accepts=%v", accepted, parsed)
					}
				})
			}
		}
	}
	for name := range semanticOnly {
		if !seen[name] {
			t.Errorf("semanticOnly names a missing fixture %s", name)
		}
	}
}

// checkParse parses data, checks the header expectations and reports
// whether the parser accepted it.
func checkParse(t *testing.T, parse func([]byte) ([]Warning, error), data []byte, valid bool) bool {
	t.Helper()
	wantErrs, wantWarns := expectations(data)
	warns, err := parse(data)
	if valid {
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(wantWarns) == 0 && len(warns) > 0 {
			t.Errorf("unexpected warnings: %v", warns)
		}
		for _, w := range wantWarns {
			if !anyWarningContains(warns, w) {
				t.Errorf("no warning contains %q: %v", w, warns)
			}
		}
		return true
	}
	if err == nil {
		t.Fatal("parse succeeded, want an error")
	}
	if len(wantErrs) == 0 {
		t.Errorf("invalid fixture without an # error: line; the error is %q", err)
	}
	for _, e := range wantErrs {
		if !strings.Contains(err.Error(), e) {
			t.Errorf("error %q does not contain %q", err, e)
		}
	}
	return false
}

func expectations(data []byte) (errs, warns []string) {
	for _, line := range strings.Split(string(data), "\n") {
		if s, ok := strings.CutPrefix(line, "# error: "); ok {
			errs = append(errs, strings.TrimSpace(s))
		}
		if s, ok := strings.CutPrefix(line, "# warning: "); ok {
			warns = append(warns, strings.TrimSpace(s))
		}
	}
	return errs, warns
}

func anyWarningContains(warns []Warning, s string) bool {
	for _, w := range warns {
		if strings.Contains(w.String(), s) {
			return true
		}
	}
	return false
}

func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	data, ok := schemas.Get(name)
	if !ok {
		t.Fatalf("no schema %q", name)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	url := schemas.BaseURL + name + ".schema.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return sch
}

// schemaAccepts validates a YAML file against sch the way an editor does:
// the YAML becomes a JSON value, an empty document an empty object.
func schemaAccepts(sch *jsonschema.Schema, data []byte) bool {
	v, err := yamlInstance(data)
	if err != nil {
		return false
	}
	return sch.Validate(v) == nil
}

func yamlInstance(data []byte) (any, error) {
	if len(data) > maxConfigSize {
		return nil, errors.New("too large")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one document")
	}
	v, err := nodeToJSON(&root)
	if v == nil && err == nil {
		v = map[string]any{}
	}
	return v, err
}

// nodeToJSON converts a YAML node to the JSON value a YAML-aware editor
// validates: scalars by their resolved tag, dates and unknown tags as the
// text written, merge keys applied.
func nodeToJSON(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return nodeToJSON(n.Content[0])
	case yaml.AliasNode:
		return nodeToJSON(n.Alias)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := nodeToJSON(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		return mappingToJSON(n)
	}
	var v any
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool", "!!int", "!!float":
		err := n.Decode(&v)
		return v, err
	}
	return n.Value, nil
}

func mappingToJSON(n *yaml.Node) (map[string]any, error) {
	out := map[string]any{}
	var merged []map[string]any
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], n.Content[i+1]
		if k.ShortTag() == "!!merge" {
			m, err := mergeSources(val)
			if err != nil {
				return nil, err
			}
			merged = append(merged, m...)
			continue
		}
		key := k.Value
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		v, err := nodeToJSON(val)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	for _, m := range merged {
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

func mergeSources(val *yaml.Node) ([]map[string]any, error) {
	for val.Kind == yaml.AliasNode {
		val = val.Alias
	}
	nodes := []*yaml.Node{val}
	if val.Kind == yaml.SequenceNode {
		nodes = val.Content
	}
	var out []map[string]any
	for _, n := range nodes {
		for n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		if n.Kind != yaml.MappingNode {
			return nil, errors.New("merge of a non-mapping")
		}
		m, err := mappingToJSON(n)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// TestSchemaPatterns keeps the patterns and limits the schemas share with
// the Go validation identical.
func TestSchemaPatterns(t *testing.T) {
	want := map[string]map[string]any{
		"hub": {
			"/properties/id/pattern":                                             packNamePattern,
			"/properties/id/minLength":                                           minHubIDLen,
			"/properties/id/maxLength":                                           maxHubIDLen,
			"/$defs/packName/pattern":                                            packNamePattern,
			"/$defs/packName/maxLength":                                          maxPackNameLen,
			"/$defs/providerId/pattern":                                          providerIDPattern,
			"/$defs/url/pattern":                                                 urlPattern,
			"/$defs/account/pattern":                                             accountPattern,
			"/$defs/account/maxLength":                                           maxAccountLen,
			"/properties/previous_fingerprints/items/pattern":                    fingerprintPattern,
			"/properties/memory/properties/auto_close_cooldown/pattern":          cooldownPattern,
			"/properties/pr/properties/labels/items/pattern":                     labelPattern,
			"/properties/pr/properties/labels/items/maxLength":                   maxLabelLen,
			"/properties/pr/properties/title/pattern":                            nonBlankPattern,
			"/properties/commit/properties/message/pattern":                      nonBlankPattern,
			"/properties/commit/properties/message/not/pattern":                  draftPattern,
			"/properties/pr/properties/title/not/pattern":                        draftPattern,
			"/properties/security/then/properties/reason/pattern":                nonBlankPattern,
			"/properties/pr/properties/link_hub/enum":                            linkHubModes,
			"/properties/security/properties/write_isolation/enum":               isolationModes,
			"/properties/security/properties/private_targets_in_public_hub/enum": privateTargets,
			"/$defs/providerType/enum":                                           providerTypes,
			"/$defs/sign/enum":                                                   signModes,
			"/$defs/pattern/pattern":                                             patternPattern,
		},
		"targets": {
			"/$defs/packName/pattern":     packNamePattern,
			"/$defs/packName/maxLength":   maxPackNameLen,
			"/$defs/providerId/pattern":   providerIDPattern,
			"/$defs/ref/pattern":          refPattern,
			"/$defs/ref/maxLength":        maxRefLen,
			"/$defs/namespace/pattern":    namespacePattern,
			"/$defs/namespace/maxLength":  maxRefLen,
			"/$defs/topics/items/pattern": nonBlankPattern,
		},
		"opt-in": {
			"/properties/packs/items/pattern":   packNamePattern,
			"/properties/packs/items/maxLength": maxPackNameLen,
			"/properties/ignore/items/pattern":  patternPattern,
			"/properties/ignore/maxItems":       maxIgnorePatterns,
		},
		"operations": {
			"/$defs/target/pattern":                                   refPattern,
			"/$defs/target/maxLength":                                 maxRefLen,
			"/$defs/head/pattern":                                     headPattern,
			"/$defs/until/pattern":                                    datePattern,
			"/properties/allow_mass_close/properties/max/maximum":     maxMassClose,
			"/properties/allow_mass_close/properties/max/minimum":     1,
			"/properties/forget_declines/items/properties/pr/minimum": 1,
		},
	}
	for name, pointers := range want {
		data, _ := schemas.Get(name)
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for ptr, w := range pointers {
			got, ok := lookup(doc, ptr)
			if !ok {
				t.Errorf("%s: %s missing", name, ptr)
				continue
			}
			wj, _ := json.Marshal(w)
			gj, _ := json.Marshal(got)
			if !bytes.Equal(wj, gj) {
				t.Errorf("%s: %s = %s, Go has %s", name, ptr, gj, wj)
			}
		}
	}
}

func lookup(doc any, ptr string) (any, bool) {
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, false
		}
		if doc, ok = m[tok]; !ok {
			return nil, false
		}
	}
	return doc, true
}

// TestRefPatternsMatchParser checks that the schema's reference patterns
// accept exactly what ParseRef and namespace parsing accept.
func TestRefPatternsMatchParser(t *testing.T) {
	inputs := []string{
		"acme/billing", "Acme/Billing", "gh:acme/billing", "group/sub/project",
		"acme/.github", "acme/..x", "acme/...", "acme/a.b-c_d", "acme/billing.git",
		"acme", "gh:acme", "", ":acme/x", "GH:acme/x", "gh-2:acme/x", "9gh:acme/x",
		"acme/", "/acme/x", "acme//x", "acme/./x", "acme/../x", "acme/.", "acme/..",
		"acme/my repo", "acme/x:y", "a:b:c/d", "acme/über", "acme\\x", "a/b/c/d/e/f",
		strings.Repeat("a", 256) + "/" + strings.Repeat("b", 256),
		strings.Repeat("a", 255) + "/" + strings.Repeat("b", 256),
	}
	refRe := mustCompile(t, refPattern)
	nsRe := mustCompile(t, namespacePattern)
	for _, s := range inputs {
		_, err := ParseRef(s)
		if got, want := err == nil, refRe.MatchString(s) && len(s) <= maxRefLen; got != want {
			t.Errorf("ParseRef(%q) ok=%v, schema ok=%v (%v)", s, got, want, err)
		}
		_, err = parseRef(s, 1)
		if got, want := err == nil, nsRe.MatchString(s) && len(s) <= maxRefLen; got != want {
			t.Errorf("namespace %q ok=%v, schema ok=%v (%v)", s, got, want, err)
		}
	}
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return re
}
