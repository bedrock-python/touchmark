package snapshot

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Object ids of the tests.
const (
	blobA  = "8f3c1a0b2e7d4a619b0c1d2e3f405162738495a6"
	blobB  = "77ab0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6071"
	blobC  = "5e7a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c"
	commit = "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d"
)

func regular(mode, oid string) decide.Observation {
	return decide.Observation{Kind: decide.Regular, OIDs: []string{oid}, Mode: mode}
}

func notRegular(detail string) decide.Observation {
	return decide.Observation{Kind: decide.NotRegular, Detail: detail}
}

func dir() decide.Observation {
	return decide.Observation{Kind: decide.NotRegular, IsDir: true, Detail: "a directory"}
}

func absent(twin string) decide.Observation {
	return decide.Observation{Kind: decide.Absent, CaseTwin: twin}
}

func blocked(blocker string, isFile bool, what string) decide.Observation {
	return decide.Observation{Kind: decide.UnsafeParent, Blocker: blocker, BlockerIsFile: isFile, Detail: "parent " + blocker + " is " + what}
}

func invalid(p string) decide.Observation {
	return decide.Observation{Kind: decide.InvalidPath, Detail: pathx.Validate(p).Error()}
}

func TestObserve(t *testing.T) {
	tree := &Tree{Commit: commit, Entries: map[string]Entry{
		"README.md":      {Mode: "100644", OID: blobA},
		"bin/run":        {Mode: "100755", OID: blobB},
		"link":           {Mode: "120000", OID: blobC},
		"vendor/lib":     {Mode: "160000", OID: commit},
		"docs/a.md":      {Mode: "100644", OID: blobA},
		"docs/deep/b.md": {Mode: "100644", OID: blobB},
		"Guide.md":       {Mode: "100644", OID: blobC},
		"A.md":           {Mode: "100644", OID: blobA},
		"a.MD":           {Mode: "100644", OID: blobB},
		"Tools":          {Mode: "100755", OID: blobA},
		"Sub":            {Mode: "160000", OID: commit},
	}}
	cases := []struct {
		path string
		want decide.Observation
	}{
		// Entries.
		{"README.md", regular("100644", blobA)},
		{"bin/run", regular("100755", blobB)},
		{"link", notRegular("a symlink")},
		{"vendor/lib", notRegular("a submodule")},
		// Directories implied by entries.
		{"docs", dir()},
		{"docs/deep", dir()},
		{"vendor", dir()},
		// Absent paths.
		{"missing.md", absent("")},
		{"docs/missing.md", absent("")},
		{"docs/deep/c/d.md", absent("")},
		// Unsafe parents: a file, an executable, a symlink, a submodule,
		// however deep beneath them.
		{"README.md/x", blocked("README.md", true, "a file")},
		{"bin/run/x/y", blocked("bin/run", true, "a file")},
		{"link/x", blocked("link", false, "a symlink")},
		{"vendor/lib/x", blocked("vendor/lib", false, "a submodule")},
		{"vendor/lib/nested/deep/x.md", blocked("vendor/lib", false, "a submodule")},
		// Case twins: an entry, a directory, the smallest of several.
		{"readme.md", absent("README.md")},
		{"guide.md", absent("Guide.md")},
		{"GUIDE.MD", absent("Guide.md")},
		{"DOCS", absent("docs")},
		{"Docs/A.md", absent("docs/a.md")},
		{"docs/A.md", absent("docs/a.md")},
		{"docs/DEEP/B.MD", absent("docs/deep/b.md")},
		{"a.md", absent("A.md")},
		{"A.MD", absent("A.md")},
		{"sub", absent("Sub")},
		// A directory spelled with other case merges on checkout: no twin.
		{"Docs/new.md", absent("")},
		// A file (or submodule) where the path needs a directory.
		{"tools/x.sh", absent("Tools")},
		{"TOOLS/x/y", absent("Tools")},
		{"readme.md/x", absent("README.md")},
		{"sub/x", absent("Sub")},
		// Invalid paths.
		{"", invalid("")},
		{"../x", invalid("../x")},
		{"a//b", invalid("a//b")},
		{"/abs", invalid("/abs")},
		{"docs/", invalid("docs/")},
		{`a\b`, invalid(`a\b`)},
		{".git/config", invalid(".git/config")},
		{"x/.GIT/hooks", invalid("x/.GIT/hooks")},
		{".gitmodules", invalid(".gitmodules")},
		{"con.txt", invalid("con.txt")},
		{"a:b", invalid("a:b")},
		{"tab\there", invalid("tab\there")},
	}
	var paths []string
	for _, c := range cases {
		paths = append(paths, c.path)
	}
	got := tree.Observe(paths)
	for _, c := range cases {
		if !reflect.DeepEqual(got[c.path], c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.path, got[c.path], c.want)
		}
	}
	if len(got) != len(cases) {
		t.Errorf("Observe returned %d observations for %d paths", len(got), len(cases))
	}
}

// TestObserveTwinPrefersTheSamePath: a path that equals an entry ignoring
// case is named before an entry that collides with one of its ancestors.
func TestObserveTwinPrefersTheSamePath(t *testing.T) {
	tree := &Tree{Entries: map[string]Entry{
		"X":   {Mode: "100644", OID: blobA},
		"x/Y": {Mode: "100644", OID: blobB},
		"b":   {Mode: "100644", OID: blobA},
		"B/c": {Mode: "100644", OID: blobA},
	}}
	got := tree.Observe([]string{"x/y", "x/z", "b/C", "B/d"})
	want := map[string]decide.Observation{
		"x/y": absent("x/Y"),
		"x/z": absent("X"),
		"b/C": blocked("b", true, "a file"),
		"B/d": absent("b"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Observe =\n%+v\nwant\n%+v", got, want)
	}
}

// TestObserveMalformedTree: Entries built from a broken or unexpected
// listing never make Observe fail or treat something unknown as a file.
func TestObserveMalformedTree(t *testing.T) {
	tree := &Tree{Entries: map[string]Entry{
		"t":        {Mode: "040000", OID: blobA}, // a tree entry
		"t/f":      {Mode: "100644", OID: blobB},
		"empty":    {Mode: "040000", OID: blobA},
		"raw":      {Mode: "40000", OID: blobA},
		"weird":    {Mode: "100664", OID: blobC},
		"both":     {Mode: "100644", OID: blobA}, // a file with entries beneath it
		"both/x":   {Mode: "100644", OID: blobB},
		"longmode": {Mode: strings.Repeat("7", 100), OID: blobC},
	}}
	got := tree.Observe([]string{"t", "t/f", "empty", "empty/x", "raw", "weird", "weird/x", "both", "both/x", "longmode"})
	want := map[string]decide.Observation{
		"t":        dir(),
		"t/f":      regular("100644", blobB),
		"empty":    dir(),
		"empty/x":  absent(""),
		"raw":      dir(),
		"weird":    notRegular(`an entry of unknown mode "100664"`),
		"weird/x":  blocked("weird", false, `an entry of unknown mode "100664"`),
		"both":     notRegular("a file with entries beneath it in the same tree"),
		"both/x":   blocked("both", true, "a file"),
		"longmode": notRegular(`an entry of unknown mode "7777777777777777"`),
	}
	if !reflect.DeepEqual(got, want) {
		for p := range want {
			if !reflect.DeepEqual(got[p], want[p]) {
				t.Errorf("%q:\n got %+v\nwant %+v", p, got[p], want[p])
			}
		}
	}
}

func TestObserveEmptyAndNil(t *testing.T) {
	var nilTree *Tree
	for name, tree := range map[string]*Tree{"nil": nilTree, "no entries": {}, "empty map": {Entries: map[string]Entry{}}} {
		got := tree.Observe([]string{"a", "a/b", "../x", "a"})
		want := map[string]decide.Observation{"a": absent(""), "a/b": absent(""), "../x": invalid("../x")}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Observe = %+v", name, got)
		}
	}
	if got := (&Tree{}).Observe(nil); got == nil || len(got) != 0 {
		t.Errorf("Observe(nil) = %#v, want an empty map", got)
	}
}

// TestObserveMatchesReference compares Observe on random trees full of case
// variants, nesting and every mode with a direct reading of its contract.
func TestObserveMatchesReference(t *testing.T) {
	universe := []string{
		"a", "A", "a/b", "a/B", "A/b", "a/b/c", "A/B/C", "b", "b/c", "B/c", "c.md", "C.md",
		"d/e/f", "D/E", "d", "x/y/z/w", "X/Y", "x/y", "k", "K/l",
	}
	modes := []string{"100644", "100755", "120000", "160000"}
	r := rand.New(rand.NewPCG(5, 8))
	queries := append(slices.Clone(universe), "a/b/c/d", "A/x", "e", "D/e/F", "x/y/Z", "K/L", "k/l/m")
	for range 1000 {
		tree := &Tree{Entries: map[string]Entry{}}
		for _, p := range universe {
			if r.IntN(4) == 0 {
				tree.Entries[p] = Entry{Mode: modes[r.IntN(len(modes))], OID: fmt.Sprint(r.IntN(3))}
			}
		}
		got := tree.Observe(queries)
		for _, p := range queries {
			if want := reference(tree, p); !reflect.DeepEqual(got[p], want) {
				t.Fatalf("tree %v, path %q:\n got %+v\nwant %+v", tree.Entries, p, got[p], want)
			}
		}
	}
}

// reference observes p in tree by reading the contract of Observe
// literally, without indexes.
func reference(tree *Tree, p string) decide.Observation {
	if err := pathx.Validate(p); err != nil {
		return decide.Observation{Kind: decide.InvalidPath, Detail: err.Error()}
	}
	for _, a := range pathx.Parents(p) {
		if e, ok := tree.Entries[a]; ok {
			return decide.Observation{Kind: decide.UnsafeParent, Blocker: a, BlockerIsFile: isFile(e.Mode), Detail: "parent " + a + " is " + describe(e.Mode)}
		}
	}
	isDirectory := func(q string) bool {
		for e := range tree.Entries {
			if strings.HasPrefix(e, q+"/") {
				return true
			}
		}
		return false
	}
	e, ok := tree.Entries[p]
	switch {
	case ok && isDirectory(p):
		return notRegular(describe(e.Mode) + " with entries beneath it in the same tree")
	case ok && isFile(e.Mode):
		return regular(e.Mode, e.OID)
	case ok:
		return notRegular(describe(e.Mode))
	case isDirectory(p):
		return dir()
	}
	// Every path of the tree: entries and the directories they imply.
	var all []string
	for e := range tree.Entries {
		all = append(all, e)
		all = append(all, pathx.Parents(e)...)
	}
	slices.Sort(all)
	for _, q := range all {
		if q != p && pathx.Fold(q) == pathx.Fold(p) {
			return absent(q)
		}
	}
	for _, a := range pathx.Parents(p) {
		for _, q := range all {
			if _, isEntry := tree.Entries[q]; isEntry && q != a && pathx.Fold(q) == pathx.Fold(a) {
				return absent(q)
			}
		}
	}
	return absent("")
}
