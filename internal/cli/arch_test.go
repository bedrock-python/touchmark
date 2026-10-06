package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The architecture that keeps plan read-only: plan holds no write
// credential, so nothing plan runs may reach the write drivers or read a
// write credential. The test walks the package's call graph from runPlan
// and fails on a reference to what only distribute may touch.
//
// The graph is over-approximated, by name: a call of f reaches every
// function named f, a call of x.m every method named m, and a function or
// method used as a value (a method value, a callback) counts as called. A
// selector that names a struct field of the package and is not called is a
// field read, not a method.

// forbiddenForPlan are the identifiers of the package that lead to a write
// identity, and forbiddenSelectors the qualified ones of other packages.
var (
	forbiddenForPlan   = []string{"distributeDrivers", "distributeDriver", "readWriteSecrets", "distributeRun", "runDistribute"}
	forbiddenSelectors = []string{"auth.Write", "auth.SigningKey"}
)

// callGraph is the package's functions and methods by name, what it knows
// about names, and the files' imports.
type callGraph struct {
	fset    *token.FileSet
	funcs   map[string][]*ast.FuncDecl // functions, by name
	methods map[string][]*ast.FuncDecl // methods, by name
	fields  map[string]bool            // names of struct fields
	imports map[string]bool            // local names of imported packages
}

// parseGraph reads the package's non-test files in dir.
func parseGraph(t *testing.T, dir string) *callGraph {
	t.Helper()
	g := &callGraph{fset: token.NewFileSet(), funcs: map[string][]*ast.FuncDecl{}, methods: map[string][]*ast.FuncDecl{},
		fields: map[string]bool{}, imports: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(g.fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			local := path.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			g.imports[local] = true
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Body == nil {
					return false
				}
				if x.Recv != nil {
					g.methods[x.Name.Name] = append(g.methods[x.Name.Name], x)
				} else {
					g.funcs[x.Name.Name] = append(g.funcs[x.Name.Name], x)
				}
			case *ast.StructType:
				for _, field := range x.Fields.List {
					for _, id := range field.Names {
						g.fields[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return g
}

// refs returns the functions and methods body refers to, and reports every
// forbidden reference through bad.
func (g *callGraph) refs(body *ast.BlockStmt, bad func(pos token.Pos, what string)) []*ast.FuncDecl {
	called := map[ast.Node]bool{}
	sels := map[*ast.Ident]bool{} // the names after a ".", which are no plain identifiers
	var out []*ast.FuncDecl
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			called[x.Fun] = true
		case *ast.SelectorExpr:
			sels[x.Sel] = true
			if pkg, ok := x.X.(*ast.Ident); ok && g.imports[pkg.Name] {
				if slices.Contains(forbiddenSelectors, pkg.Name+"."+x.Sel.Name) {
					bad(x.Pos(), pkg.Name+"."+x.Sel.Name)
				}
				return false // another package's name
			}
			if called[x] || !g.fields[x.Sel.Name] {
				out = append(out, g.methods[x.Sel.Name]...)
			}
		case *ast.Ident:
			if sels[x] {
				return true
			}
			if slices.Contains(forbiddenForPlan, x.Name) {
				bad(x.Pos(), x.Name)
			}
			out = append(out, g.funcs[x.Name]...)
		}
		return true
	})
	return out
}

func TestPlanReachesNoWriter(t *testing.T) {
	g := parseGraph(t, ".")
	if len(g.funcs["runPlan"]) != 1 || len(g.methods["distributeRun"]) == 0 {
		t.Fatal("the package no longer has runPlan and distributeRun: update the test")
	}
	reached := g.reach(t, "runPlan")
	// The walk must see the preparation plan and distribute share, or it
	// proves nothing.
	for _, name := range []string{"sharedDeps", "hubChannel", "readCredentials", "caFile", "isAncestor"} {
		if !reached[name] {
			t.Errorf("runPlan no longer reaches %s: update the test", name)
		}
	}
}

// TestMigrateReachesNoWriter: migrate checks accounts with the read
// credential only, under the rules plan follows.
func TestMigrateReachesNoWriter(t *testing.T) {
	g := parseGraph(t, ".")
	if len(g.funcs["runMigrate"]) != 1 {
		t.Fatal("the package no longer has runMigrate: update the test")
	}
	reached := g.reach(t, "runMigrate")
	for _, name := range []string{"checkAccounts", "reader", "discover"} {
		if !reached[name] {
			t.Errorf("runMigrate no longer reaches %s: update the test", name)
		}
	}
}

// reach walks the call graph from the function root, fails the test on
// every forbidden reference, and returns the names of what it reached.
func (g *callGraph) reach(t *testing.T, root string) map[string]bool {
	t.Helper()
	reached := map[*ast.FuncDecl]bool{}
	queue := slices.Clone(g.funcs[root])
	for len(queue) > 0 {
		fd := queue[0]
		queue = queue[1:]
		if reached[fd] {
			continue
		}
		reached[fd] = true
		queue = append(queue, g.refs(fd.Body, func(pos token.Pos, what string) {
			t.Errorf("%s: %s, which %s reaches, refers to %s", g.fset.Position(pos), fd.Name.Name, root, what)
		})...)
	}
	names := map[string]bool{}
	for fd := range reached {
		names[fd.Name.Name] = true
	}
	return names
}
