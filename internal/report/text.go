package report

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/decide"
)

// shortCommit is how many hex digits of the hub commit text output shows.
const shortCommit = 7

// printer writes lines and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) linef(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format+"\n", args...)
	}
}

// table writes rows with every column but the last padded to its widest
// cell, indented by two spaces, without trailing spaces.
func (p *printer) table(rows [][]string) {
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	for _, row := range rows {
		var b strings.Builder
		b.WriteString("  ")
		for i, cell := range row {
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
			}
		}
		p.linef("%s", strings.TrimRight(b.String(), " "))
	}
}

// WriteText writes the report as the text output of status and apply.
func (s *Sync) WriteText(w io.Writer) error {
	p := &printer{w: w}
	p.linef("%s · hub %s · target %s", s.title(), hubLabel(s.Hub), targetLabel(s.Target))
	switch {
	case s.Target.OptIn == OptInDisabled:
		p.linef("opted out: %s says enabled: false", s.Target.OptInFile)
		s.writeWarnings(p)
		return p.err
	case !s.Target.OptedIn:
		p.linef("not opted in: %s not found in %s", s.Target.OptInFile, s.Target.Root)
		s.writeWarnings(p)
		return p.err
	case s.Target.OptIn == OptInAssumed:
		p.linef("opted in by targets.yml (opt_in: assumed): %s not found in %s", s.Target.OptInFile, s.Target.Root)
	case s.Target.OptIn == OptInFlag:
		p.linef("opted in by --assume-opt-in: %s not found in %s", s.Target.OptInFile, s.Target.Root)
	}
	if s.Selection != nil {
		p.linef("packs: %s", packsLine(s.Selection))
		if !s.Selection.Complete {
			p.linef("WARNING: selection incomplete: %s cannot be evaluated here, so packs may be missing; pass --packs to be exact",
				strings.Join(s.Selection.Unresolved, ", "))
		}
	}
	s.writeWarnings(p)
	if rows := s.rows(); len(rows) > 0 {
		p.linef("")
		p.table(rows)
		p.linef("")
	}
	p.linef("%s", statesLine(s.Summary))
	p.linef("%s", s.outcomeLine())
	return p.err
}

func (s *Sync) title() string {
	if s.DryRun {
		return "touchmark " + s.Command + " --dry-run"
	}
	return "touchmark " + s.Command
}

func (s *Sync) writeWarnings(p *printer) {
	for _, w := range s.Warnings {
		p.linef("warning: %s", w)
	}
}

// rows returns the table of every entry that is not current, or that has
// something to do. After apply, the first column is the outcome.
func (s *Sync) rows() [][]string {
	outcome := make(map[string]Result, len(s.Results))
	for _, r := range s.Results {
		outcome[r.Path] = r
	}
	var rows [][]string
	for _, e := range s.Entries {
		if e.State == string(decide.Current) && e.Action == string(decide.Keep) {
			continue
		}
		row := []string{e.Action, e.State, e.Path, e.Pack, entryDetail(e)}
		if s.Results != nil {
			r, ok := outcome[e.Path]
			switch {
			case !ok:
				row = append([]string{""}, row...)
			case r.Outcome == Done:
				row = append([]string{r.Outcome}, row...)
			default:
				row = append([]string{r.Outcome}, row[:4]...)
				row = append(row, r.Detail)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// entryDetail is the detail column: decide's detail, or a hint for a local
// file.
func entryDetail(e Entry) string {
	if e.State == string(decide.Local) && e.Action == string(decide.Keep) && e.Detail == "" {
		return "run touchmark apply --adopt " + shellQuote(e.Path) + " to take it back, or add it to ignore"
	}
	return e.Detail
}

// statesLine lists the non-zero state counts.
func statesLine(s Summary) string {
	var parts []string
	for _, c := range s.stateCounts() {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", c.state, c.n))
		}
	}
	if len(parts) == 0 {
		return "no managed paths"
	}
	return strings.Join(parts, " · ")
}

// outcomeLine is the last line: what is left to do, or what was done.
func (s *Sync) outcomeLine() string {
	n := s.Summary.Changes
	switch {
	case n == 0:
		return "in sync"
	case s.Results == nil && s.DryRun:
		return fmt.Sprintf("dry run: %s would be applied", plural(n, "change"))
	case s.Results == nil:
		return fmt.Sprintf("%s to apply: run touchmark apply", plural(n, "change"))
	}
	line := fmt.Sprintf("applied %d of %s", s.Summary.Done, plural(n, "change"))
	if s.Summary.Skipped > 0 {
		line += fmt.Sprintf(" · skipped %d", s.Summary.Skipped)
	}
	if s.Summary.Failed > 0 {
		line += fmt.Sprintf(" · failed %d", s.Summary.Failed)
	}
	return line
}

// packsLine lists the selected packs with where each came from.
func packsLine(sel *Selection) string {
	if len(sel.Packs) == 0 {
		return "none"
	}
	parts := make([]string, len(sel.Packs))
	for i, pack := range sel.Packs {
		parts[i] = pack
		if src := sel.Sources[pack]; len(src) > 0 {
			parts[i] += " (" + strings.Join(src, " + ") + ")"
		}
	}
	return strings.Join(parts, ", ")
}

// WriteText writes the report as the text output of check.
func (c *Check) WriteText(w io.Writer) error {
	p := &printer{w: w}
	p.linef("touchmark check · hub %s", hubLabel(c.Hub))
	if len(c.Packs) > 0 {
		p.linef("packs: %s", strings.Join(c.Packs, ", "))
	} else {
		p.linef("packs: none")
	}
	if len(c.Errors) > 0 {
		p.linef("errors:")
		for _, e := range c.Errors {
			p.linef("  %s", indentLines(e))
		}
	}
	if len(c.Warnings) > 0 {
		p.linef("warnings:")
		for _, w := range c.Warnings {
			p.linef("  %s", indentLines(w))
		}
	}
	switch {
	case len(c.Errors) > 0:
		p.linef("check failed: %s, %s", plural(len(c.Errors), "error"), plural(len(c.Warnings), "warning"))
	case len(c.Warnings) > 0:
		p.linef("ok, %s", plural(len(c.Warnings), "warning"))
	default:
		p.linef("ok")
	}
	return p.err
}

// indentLines indents the continuation lines of a multi-line message.
func indentLines(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

// hubLabel names the hub by id, or by directory for a legacy hub, with the
// short commit.
func hubLabel(h Hub) string {
	name := h.ID
	if name == "" {
		name = h.Dir
	}
	commit := h.Commit
	if len(commit) > shortCommit {
		commit = commit[:shortCommit]
	}
	if commit == "" {
		return name
	}
	return name + " @ " + commit
}

// targetLabel names the target by ref, or by its root.
func targetLabel(t Target) string {
	if t.Ref != "" {
		return t.Ref
	}
	return t.Root
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// shellQuote quotes a repository path for a POSIX shell when it holds
// characters other than letters, digits and "._-/+@,=".
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, unsafeInShell) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unsafeInShell reports whether r needs quoting in a POSIX shell word.
func unsafeInShell(r rune) bool {
	safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/+@,=", r)
	return !safe
}
