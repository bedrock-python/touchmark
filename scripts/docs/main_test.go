package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/schemas"
)

// root is the repository root, from this package's directory.
var root = filepath.Join("..", "..")

// TestDocsAreCurrent fails when a generated block of the documentation
// differs from what the code says now: a flag, a schema field or an input
// of the Action changed without the pages.
func TestDocsAreCurrent(t *testing.T) {
	changed, err := run(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range changed {
		t.Errorf("%s is out of date: run go run ./scripts/docs", page)
	}
}

// TestDocsCoverEverything fails when a command, a schema or the Action's
// inputs have no generated block anywhere under docs/.
func TestDocsCoverEverything(t *testing.T) {
	found, err := blocks(root)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := help("")
	if err != nil {
		t.Fatal(err)
	}
	commands := commandNames(usage)
	if len(commands) < 10 {
		t.Fatalf("touchmark --help lists %d commands: %q", len(commands), commands)
	}
	want := []string{"help ", "action-inputs "}
	for _, c := range commands {
		want = append(want, "help "+c)
	}
	for _, s := range schemas.Names() {
		want = append(want, "schema "+s)
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("no page under docs/ has the block <!-- generated: %s -->", strings.TrimSpace(w))
		}
	}
}

func TestRewrite(t *testing.T) {
	g := &generator{root: root}
	in := "# Probe\n\n<!-- generated: help probe -->\nstale\n<!-- end generated -->\n\nAfter.\n"
	out, err := g.rewrite(in)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Probe\n\n<!-- generated: help probe -->\n\n```text\nUsage: touchmark probe\n\n" +
		"Exit 2 when this job sees a write credential or signing key (the isolation probe).\n```\n\n" +
		"<!-- end generated -->\n\nAfter.\n"
	if out != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", out, want)
	}
	again, err := g.rewrite(out)
	if err != nil || again != out {
		t.Errorf("a second rewrite changed the page (err %v):\n%s", err, again)
	}

	for _, bad := range []string{
		"<!-- generated: help probe -->\nno end\n",
		"<!-- generated: help probe -->\n<!-- generated: help check -->\n<!-- end generated -->\n",
		"<!-- generated: nonsense -->\n<!-- end generated -->\n",
		"<!-- generated: schema nonsense -->\n<!-- end generated -->\n",
	} {
		if _, err := g.rewrite(bad); err == nil {
			t.Errorf("rewrite(%q) succeeded", bad)
		}
	}
}

func TestSchemaTable(t *testing.T) {
	schema := `{
  "type": "object",
  "required": ["id"],
  "properties": {
    "id": { "description": "The hub's id.", "type": "string", "minLength": 3, "maxLength": 40 },
    "entries": { "type": ["array", "null"], "items": { "$ref": "#/$defs/entry" } },
    "packs": {
      "type": "object",
      "propertyNames": { "$ref": "#/$defs/packName" },
      "additionalProperties": { "type": "object", "properties": { "requires": { "type": "array", "items": { "type": "string" } } } }
    },
    "glob": { "description": "a | b, ** and <id>; ` + "`a|b`" + ` stays code", "type": "string" }
  },
  "$defs": {
    "packName": { "type": "string" },
    "entry": { "oneOf": [
      { "type": "object", "required": ["repo"], "properties": { "repo": { "type": "string" }, "packs": { "type": "array", "items": { "type": "string" } } } },
      { "type": "object", "required": ["org"], "properties": { "org": { "type": "string" }, "topics": { "description": "Topics.", "type": "array", "items": { "type": "string" } }, "packs": { "type": "array", "items": { "type": "string" } } } }
    ] }
  }
}`
	got, err := schemaTable([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{
		"| `id` | string, 3 to 40 characters, required | The hub's id. |",
		"| `entries` | list of objects | — |",
		"| `entries[].repo` | string | An entry has exactly one of `repo` and `org`. |",
		"| `entries[].packs` | list of strings | — |",
		"| `entries[].topics` | list of strings | Topics. Only with `org`. |",
		"| `packs` | map of pack to object | — |",
		"| `packs.<pack>.requires` | list of strings | — |",
		"| `glob` | string | a \\| b, \\*\\* and &lt;id&gt;; `a|b` stays code |",
	} {
		if !strings.Contains(got, row+"\n") {
			t.Errorf("the table lacks the row\n%s\nin\n%s", row, got)
		}
	}
}

// internalRe matches pointers to the maintainers' unpublished design
// records in text: a design document's number in any case (published RFCs,
// written as "RFC 3339", pass) or section sign, a milestone name, the name
// of the internal hub touchmark grew out of, and Cyrillic, the language of
// those records. The section sign is written as an escape, so that this
// file passes its own check.
var internalRe = regexp.MustCompile(`(?i:rfc)(?:-[0-9]{4}|[_ ]?0[0-9]{3})|\x{00A7}|\bM[0-9](?:\.[0-9]+)?\b|(?i:[p]rototype)|\p{Cyrillic}`)

// identRe matches the same pointers in a Go identifier, where a milestone
// sits inside camel case (fooM20, TestBarM2) and a design document's
// number has no dash. time.RFC3339 passes.
var identRe = regexp.MustCompile(`(?i:rfc)_?0[0-9]{3}|(?:^|[a-z0-9_])M[0-9]+(?:$|[A-Z_])|(?i:[p]rototype)|\p{Cyrillic}`)

// pathRe matches the same pointers in a file's path, in any case: a design
// document's number, a milestone as a name or a part of one (m2/,
// m2.1-plan.yml), the internal hub's name, the section sign and Cyrillic.
var pathRe = regexp.MustCompile(`(?i)rfc[-_ ]?0[0-9]{3}|(?:^|[/_.-])m[0-9]+(?:\.[0-9]+)?(?:$|[/_.-])|[p]rototype|\x{00A7}|\p{Cyrillic}`)

// base64Re matches the base64 runs that can spell a milestone name by
// chance, which internalRefs drops before it matches: the gzip payload of
// a marker and the checksums of go.sum.
var base64Re = regexp.MustCompile(`data=H4sI[A-Za-z0-9+/=]*|h1:[A-Za-z0-9+/=]+`)

// cyrillicRe matches the one kind of internalRe's matches that the string
// literals of tests may hold: Unicode fixtures, such as a path or a secret
// in Cyrillic letters.
var cyrillicRe = regexp.MustCompile(`\p{Cyrillic}`)

// notScanned lists the files whose text TestNoInternalReferences does not
// read, by path pattern (path.Match), each with the reason.
var notScanned = map[string]string{
	"CHANGELOG.md":      "release-please writes it from commit subjects, which cannot change once pushed",
	"docs/changelog.md": "make docs copies CHANGELOG.md there; git ignores the copy",
}

// notWalked lists the directories that the walk without git skips: git's
// own, and those that builds and tools write and git ignores.
var notWalked = []string{".git", ".cache", ".venv", ".claude/worktrees", "bin", "dist", "site"}

// notWalkedFiles lists, by base name (path.Match), the files that the walk
// without git skips: those that tests, coverage and local setups write and
// .gitignore ignores. A Go file is never skipped.
var notWalkedFiles = []string{"*.out", "coverage.*", "*.coverprofile", "profile.cov", "go.work", "go.work.sum", ".env", "*.test", "*.exe"}

// internalRef is a match of internalRe: the line it is on, counting from 0,
// the line's text and the match.
type internalRef struct {
	line        int
	text, match string
}

// internalRefs returns the first match of internalRe on every line of
// text, base64 runs (base64Re) left out; with fixture set, it lets Cyrillic
// through.
func internalRefs(text string, fixture bool) []internalRef {
	var refs []internalRef
	for i, line := range strings.Split(text, "\n") {
		for _, m := range internalRe.FindAllString(base64Re.ReplaceAllString(line, ""), -1) {
			if fixture && cyrillicRe.MatchString(m) {
				continue
			}
			refs = append(refs, internalRef{i, strings.TrimSpace(line), m})
			break
		}
	}
	return refs
}

// moduleFiles returns the module's files, relative to root and with
// slashes: those git tracks or would add, or, where git cannot list them,
// every file outside notWalked and notWalkedFiles.
func moduleFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err == nil {
		return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	}
	t.Logf("git ls-files: %v; walking the tree instead", err)
	var files []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && (slices.Contains(notWalked, rel) || d.Name() == "__pycache__"):
			return filepath.SkipDir
		case !d.IsDir() && !ignoredFile(rel):
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// ignoredFile reports whether notWalkedFiles names the file rel.
func ignoredFile(rel string) bool {
	if strings.HasSuffix(rel, ".go") {
		return false
	}
	return slices.ContainsFunc(notWalkedFiles, func(pattern string) bool {
		ok, _ := path.Match(pattern, path.Base(rel))
		return ok
	})
}

// skipped returns the reason notScanned gives for the file rel, or "".
func skipped(rel string) string {
	for pattern, reason := range notScanned {
		if ok, _ := path.Match(pattern, rel); ok {
			return reason
		}
	}
	return ""
}

// TestNoInternalReferences fails when text a reader meets points at
// something they cannot open (internalRe): the help of every command, the
// path of every file of the module that git tracks or would add (pathRe),
// and the text of those files but notScanned. Of a Go file it reads the
// comments and the string literals, where errors, help and generated files
// come from, and the identifiers (identRe), which go test -v and stack
// traces print; the literals of tests may hold Cyrillic, as Unicode
// fixtures. A file with a NUL byte is binary and its text skipped.
func TestNoInternalReferences(t *testing.T) {
	report := func(where string, line int, ref internalRef) {
		t.Helper()
		t.Errorf("%s:%d: %q in %q", where, line, ref.match, ref.text)
	}
	usage, err := help("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append([]string{""}, commandNames(usage)...) {
		text, err := help(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range internalRefs(text, false) {
			report(strings.Join(strings.Fields("touchmark "+c+" --help"), " "), ref.line+1, ref)
		}
	}

	var files, goFiles, pages int
	for _, rel := range moduleFiles(t) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted from the working tree
		}
		if err != nil {
			t.Fatal(err)
		}
		if m := pathRe.FindString(rel); m != "" {
			t.Errorf("%s: %q in the path", rel, m)
		}
		if skipped(rel) != "" || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		files++
		if strings.HasPrefix(rel, "docs/") && strings.HasSuffix(rel, ".md") {
			pages++
		}
		if !strings.HasSuffix(rel, ".go") {
			for _, ref := range internalRefs(string(data), false) {
				report(rel, ref.line+1, ref)
			}
			continue
		}
		goFiles++
		test := strings.HasSuffix(rel, "_test.go")
		fset := token.NewFileSet()
		var s scanner.Scanner
		s.Init(fset.AddFile(rel, -1, len(data)), data, func(pos token.Position, msg string) {
			t.Errorf("%s: %s", pos, msg)
		}, scanner.ScanComments)
		for {
			pos, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			p := fset.Position(pos)
			switch tok {
			case token.IDENT:
				if m := identRe.FindString(lit); m != "" {
					report(rel, p.Line, internalRef{text: lit, match: m})
				}
			case token.COMMENT:
				for _, ref := range internalRefs(lit, false) {
					report(rel, p.Line+ref.line, ref)
				}
			case token.STRING, token.CHAR:
				v, err := strconv.Unquote(lit)
				if err != nil {
					t.Errorf("%s: %v", p, err)
					continue
				}
				for _, ref := range internalRefs(v, test) {
					line := p.Line // an escaped newline starts no line of the source
					if lit[0] == '`' {
						line += ref.line
					}
					report(rel, line, ref)
				}
			}
		}
	}
	if files < 500 || goFiles < 300 || pages < 20 {
		t.Fatalf("scanned %d files, %d of them Go, %d pages under docs/", files, goFiles, pages)
	}
}

func TestInternalRefs(t *testing.T) {
	// The cases are spelled so that this file passes TestNoInternalReferences:
	// the underscores go, and the section sign is made at run time.
	spell := strings.NewReplacer("_", "", "SECT", string(rune(0xa7))).Replace
	text := spell("plain\nsee RFC_-0002 for why\nM_2.6 and M_1\nthe proto_type, SECT 4\n" +
		"the rfc_-0001 file, a PROTO_TYPE\nRFC 3339, RFC3339, data=H4sIAM_2.6+x/=, h1:M_3w=\n" +
		"файл.txt\nM2x M10x version M\n")
	refs := func(fixture bool) []string {
		var got []string
		for _, ref := range internalRefs(text, fixture) {
			got = append(got, fmt.Sprintf("%d %s", ref.line, ref.match))
		}
		return got
	}
	want := []string{spell("1 RFC_-0002"), spell("2 M_2.6"), spell("3 proto_type"), spell("4 rfc_-0001"), "6 ф"}
	if got := refs(false); !slices.Equal(got, want) {
		t.Errorf("internalRefs = %q, want %q", got, want)
	}
	if got := refs(true); !slices.Equal(got, want[:4]) {
		t.Errorf("internalRefs of a fixture = %q, want %q", got, want[:4])
	}
	for ident, want := range map[string]bool{
		spell("gateM_20"): true, spell("TestPlanM_2"): true, spell("TestM_2Plan"): true,
		spell("planM_2_1"): true, spell("M_1"): true, spell("proto_typeConfig"): true,
		spell("TestMigrateProto_type"): true, "rfc" + "_0001": true, "RFC" + "0002Example": true,
		"inspectSnapshotOnly": false, "RFC3339": false, "RFC1123Z": false, "M": false,
		"MaxMemory": false, "sha256": false, "x509": false,
	} {
		if got := identRe.MatchString(ident); got != want {
			t.Errorf("identRe.MatchString(%q) = %v, want %v", ident, got, want)
		}
	}
	for rel, want := range map[string]bool{
		spell("internal/config/testdata/rfc_-0001-example.yml"): true,
		spell("testdata/golden/migrate/proto_type.txt"):         true,
		spell("testdata/m_2/plan.yml"):                          true,
		spell("docs/plan-M_2.1.md"):                             true,
		"internal/marker/testdata/encode/empty.marker":          false,
		"internal/cli/testdata/golden/migrate/multi-gitter.txt": false,
		"docs/assets/arm64.png":                                 false,
	} {
		if got := pathRe.MatchString(rel); got != want {
			t.Errorf("pathRe.MatchString(%q) = %v, want %v", rel, got, want)
		}
	}
	for rel, want := range map[string]bool{
		"CHANGELOG.md": true, "docs/changelog.md": true, "go.sum": false,
		"internal/marker/testdata/encode/empty.marker": false, "README.md": false,
	} {
		if got := skipped(rel) != ""; got != want {
			t.Errorf("skipped(%q) = %v, want %v", rel, got, want)
		}
	}
	for rel, want := range map[string]bool{
		"coverage.out": true, "internal/apply/apply.test": true, ".env": true,
		"coverage.go": false, "docs/env.md": false, "go.mod": false,
	} {
		if got := ignoredFile(rel); got != want {
			t.Errorf("ignoredFile(%q) = %v, want %v", rel, got, want)
		}
	}
}
