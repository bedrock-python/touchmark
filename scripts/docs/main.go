// Command docs writes the parts of the documentation site that come from the
// code, so that they cannot drift from it: the help of every command, the
// fields of every configuration file and report (from the JSON Schemas in
// schemas/), and the inputs of the GitHub Action (from action.yml).
//
// A generated block is the text between a line
//
//	<!-- generated: KIND [ARG] -->
//
// and the next line `<!-- end generated -->`, in any page under docs/. KIND
// is one of:
//
//	help [COMMAND]  the output of `touchmark [COMMAND] --help`
//	schema NAME     a table of the fields of `touchmark schema NAME`
//	action-inputs   a table of the inputs of action.yml
//
// Everything outside the blocks is written by hand. From the repository
// root:
//
//	go run ./scripts/docs          rewrite every block that is out of date
//	go run ./scripts/docs -check   write nothing; exit 1 when a block is out of date
//
// `make docs-build` runs the first; TestDocsAreCurrent runs the second in
// `go test ./...`, so a change to a flag, a schema or an input fails CI until
// the pages follow it.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/schemas"
)

func main() {
	root := flag.String("root", ".", "the repository root `DIR`")
	check := flag.Bool("check", false, "write nothing; exit 1 when a generated block is out of date")
	flag.Parse()
	changed, err := run(*root, !*check)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docs:", err)
		os.Exit(2)
	}
	for _, page := range changed {
		if *check {
			fmt.Fprintf(os.Stderr, "docs: %s is out of date: run go run ./scripts/docs\n", page)
		} else {
			fmt.Printf("docs: wrote %s\n", page)
		}
	}
	if *check && len(changed) > 0 {
		os.Exit(1)
	}
}

// The markers of a generated block.
var (
	beginRe = regexp.MustCompile(`^<!-- generated: ([a-z-]+)(?: ([a-z-]+))? -->$`)
	endLine = "<!-- end generated -->"
)

// run regenerates the blocks of every page under root/docs and returns the
// pages, relative to root, whose text changed; it writes them when write is
// set.
func run(root string, write bool) ([]string, error) {
	g := &generator{root: root}
	var changed []string
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".md" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// A checkout with core.autocrlf has CRLF in the working tree; the
		// blocks compare with LF, as git stores the pages.
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		out, err := g.rewrite(text)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if out == text {
			return nil
		}
		changed = append(changed, rel)
		if write {
			return os.WriteFile(p, []byte(out), 0o644)
		}
		return nil
	})
	return changed, err
}

// blocks lists the "KIND ARG" of every block under root/docs, for the test
// that every command and schema has one.
func blocks(root string) (map[string]bool, error) {
	found := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".md" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			if m := beginRe.FindStringSubmatch(line); m != nil {
				found[m[1]+" "+m[2]] = true
			}
		}
		return nil
	})
	return found, err
}

// generator renders the blocks.
type generator struct {
	root string
}

// rewrite replaces the body of every block in text with its current
// rendering.
func (g *generator) rewrite(text string) (string, error) {
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		b.WriteString(lines[i])
		m := beginRe.FindStringSubmatch(strings.TrimSuffix(lines[i], "\n"))
		if m == nil {
			continue
		}
		end := -1
		for j := i + 1; j < len(lines); j++ {
			line := strings.TrimSuffix(lines[j], "\n")
			if beginRe.MatchString(line) {
				return "", fmt.Errorf("line %d: a block starts inside the block of line %d", j+1, i+1)
			}
			if line == endLine {
				end = j
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("line %d: the block has no %s line", i+1, endLine)
		}
		body, err := g.block(m[1], m[2])
		if err != nil {
			return "", fmt.Errorf("line %d: %w", i+1, err)
		}
		// Blank lines around the body: a table or a fence right after an
		// HTML comment would belong to the comment's HTML block.
		b.WriteString("\n" + body + "\n")
		b.WriteString(lines[end])
		i = end
	}
	return b.String(), nil
}

// block renders one block.
func (g *generator) block(kind, arg string) (string, error) {
	switch kind {
	case "help":
		text, err := help(arg)
		if err != nil {
			return "", err
		}
		return "```text\n" + text + "```\n", nil
	case "schema":
		data, ok := schemas.Get(arg)
		if !ok {
			return "", fmt.Errorf("no schema %q: want one of %s", arg, strings.Join(schemas.Names(), ", "))
		}
		return schemaTable(data)
	case "action-inputs":
		if arg != "" {
			return "", errors.New("action-inputs takes no argument")
		}
		data, err := os.ReadFile(filepath.Join(g.root, "action.yml"))
		if err != nil {
			return "", err
		}
		return actionInputs(data)
	}
	return "", fmt.Errorf("unknown block kind %q: want help, schema or action-inputs", kind)
}

// help returns the output of `touchmark [command] --help`, without trailing
// blanks on its lines.
func help(command string) (string, error) {
	args := []string{"--help"}
	if command != "" {
		args = []string{command, "--help"}
	}
	var out, errOut bytes.Buffer
	if code := cli.Main(context.Background(), args, &out, &errOut); code != 0 || errOut.Len() > 0 {
		return "", fmt.Errorf("touchmark %s: exit %d: %s", strings.Join(args, " "), code, errOut.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// commandNames returns the commands that `touchmark --help` lists.
func commandNames(usage string) []string {
	var names []string
	in := false
	for _, line := range strings.Split(usage, "\n") {
		switch {
		case line == "Commands:":
			in = true
		case in && strings.HasPrefix(line, "  "):
			names = append(names, strings.Fields(line)[0])
		case in:
			return names
		}
	}
	return names
}

// actionInputs renders the inputs of action.yml as a table, in file order.
func actionInputs(data []byte) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("action.yml: %w", err)
	}
	if len(doc.Content) == 0 {
		return "", errors.New("action.yml is empty")
	}
	inputs := mapValue(doc.Content[0], "inputs")
	if inputs == nil || inputs.Kind != yaml.MappingNode {
		return "", errors.New("action.yml has no inputs")
	}
	var b strings.Builder
	b.WriteString("| Input | Default | Description |\n|---|---|---|\n")
	for i := 0; i+1 < len(inputs.Content); i += 2 {
		name, spec := inputs.Content[i].Value, inputs.Content[i+1]
		def := "—"
		if v := mapValue(spec, "required"); v != nil && v.Value == "true" {
			def = "required"
		} else if v := mapValue(spec, "default"); v != nil && v.Value != "" {
			def = "`" + v.Value + "`"
		}
		desc := ""
		if v := mapValue(spec, "description"); v != nil {
			desc = cell(v.Value)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", name, def, desc)
	}
	return b.String(), nil
}

// mapValue returns the value of key in the mapping n, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// cell makes text safe in a cell of a Markdown table: one line, and outside
// code spans no characters that Markdown or HTML would read as markup or as
// the end of the cell.
func cell(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	parts := strings.Split(text, "`")
	for i := 0; i < len(parts); i += 2 { // the even parts are outside code spans
		p := strings.ReplaceAll(parts[i], `\`, `\\`)
		p = strings.NewReplacer("*", `\*`, "<", "&lt;", ">", "&gt;", "|", `\|`).Replace(p)
		parts[i] = p
	}
	// A pipe inside a code span stays as it is: the site's Markdown does not
	// split a cell there, and would print an escaping backslash.
	return strings.Join(parts, "`")
}
