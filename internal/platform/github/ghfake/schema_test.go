package ghfake

import (
	"os"
	"slices"
	"testing"
)

// TestSchemaSubset checks the fake's schemas against GitHub's public ones
// when GHFAKE_SCHEMA names a downloaded copy of
// https://docs.github.com/public/fpt/schema.docs.graphql and
// GHFAKE_SCHEMA_GHES one of
// https://docs.github.com/public/ghes-3.19/schema.docs-enterprise.graphql:
// every type, field, argument, input field, enum value and union member
// of the subset exists there with the same type and default. Without them
// it only checks that the subsets parse.
func TestSchemaSubset(t *testing.T) {
	for _, tc := range []struct {
		flavor Flavor
		env    string
	}{{DotCom, "GHFAKE_SCHEMA"}, {GHES, "GHFAKE_SCHEMA_GHES"}} {
		t.Run(string(tc.flavor), func(t *testing.T) {
			sub, err := fakeSchema(tc.flavor)
			if err != nil {
				t.Fatal(err)
			}
			path := os.Getenv(tc.env)
			if path == "" {
				t.Skip(tc.env + " is not set")
			}
			compareSchema(t, sub, path)
		})
	}
}

// compareSchema checks sub against the schema file at path.
func compareSchema(t *testing.T, sub *gqlSchema, path string) {
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	full, err := parseSchema(string(src))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	defString := func(v *gqlValue) string {
		if v == nil {
			return ""
		}
		return v.String()
	}
	for name, ft := range sub.types {
		gt := full.types[name]
		if gt == nil {
			t.Errorf("type %s is not in GitHub's schema", name)
			continue
		}
		if gt.kind != ft.kind {
			t.Errorf("%s is a %s, GitHub's a %s", name, ft.kind, gt.kind)
			continue
		}
		for _, i := range ft.ifaces {
			if !slices.Contains(gt.ifaces, i) {
				t.Errorf("%s does not implement %s on GitHub", name, i)
			}
		}
		for _, m := range ft.members {
			if !slices.Contains(gt.members, m) {
				t.Errorf("%s has no member %s on GitHub", name, m)
			}
		}
		for v := range ft.values {
			if !gt.values[v] {
				t.Errorf("%s has no value %s on GitHub", name, v)
			}
		}
		for fname, f := range ft.fields {
			g := gt.fields[fname]
			if g == nil {
				t.Errorf("%s.%s is not on GitHub", name, fname)
				continue
			}
			if f.typ.String() != g.typ.String() {
				t.Errorf("%s.%s: %s, GitHub %s", name, fname, f.typ, g.typ)
			}
			for aname, a := range f.args {
				ga := g.args[aname]
				switch {
				case ga == nil:
					t.Errorf("%s.%s(%s) is not on GitHub", name, fname, aname)
				case a.typ.String() != ga.typ.String() || defString(a.def) != defString(ga.def):
					t.Errorf("%s.%s(%s: %s = %s), GitHub %s = %s", name, fname, aname, a.typ, defString(a.def), ga.typ, defString(ga.def))
				}
			}
			for aname, ga := range g.args {
				if f.args[aname] == nil && ga.typ.nonNull && ga.def == nil {
					t.Errorf("%s.%s lacks the required argument %s", name, fname, aname)
				}
			}
		}
		for iname, in := range ft.inputs {
			g := gt.inputs[iname]
			if g == nil || in.typ.String() != g.typ.String() || defString(in.def) != defString(g.def) {
				t.Errorf("input %s.%s differs from GitHub's", name, iname)
			}
		}
		for iname := range gt.inputs {
			if ft.inputs[iname] == nil {
				t.Errorf("input %s lacks %s", name, iname)
			}
		}
	}
}
