package report

import (
	"fmt"
	"strconv"
	"strings"
)

// What a plan or a dry run says about the one-off operations: the plan of
// a hub pull request shows what each entry of .touchmark/operations.yml
// would do, in its Operations section.

// Kinds of Operation: the keys of operations.yml.
const (
	OpRecreate       = "recreate"
	OpForgetDeclines = "forget_declines"
	OpAllowMassClose = "allow_mass_close"
	OpAdoptUnmarked  = "adopt_unmarked"
)

// Effects of an Operation.
const (
	// EffectApplies: the entry changes what the run does.
	EffectApplies = "applies"
	// EffectNone: the entry is in force but changes nothing in this run (a
	// moved head, a decline already revoked, closes the guard allows
	// anyway); it can usually be removed.
	EffectNone = "none"
	// EffectExpired: the entry's date has passed; it does nothing and can
	// be removed.
	EffectExpired = "expired"
	// EffectUnknown: the run cannot tell (its target was not inspected:
	// skipped, failed, deferred, out of a plan's scope, or not public in a
	// public hub's report).
	EffectUnknown = "unknown"
)

// Operation is one entry of .touchmark/operations.yml, or one operation
// flag of a local run, and what it does in this run.
type Operation struct {
	// Kind is OpRecreate, OpForgetDeclines, OpAllowMassClose or
	// OpAdoptUnmarked.
	Kind string `json:"kind"`
	// Flag is set for an operation given as a flag of a local run
	// (--recreate, …) rather than read from operations.yml.
	Flag bool `json:"flag,omitempty"`
	// Target is the entry's target as operations.yml writes it (recreate,
	// forget_declines); "" for a target the report does not name (not
	// public, in a public hub's report). Provider is the provider id of the
	// target it names, when the run found it.
	Target   string `json:"target,omitempty"`
	Provider string `json:"provider,omitempty"`
	// PR is the pull request the entry names (forget_declines) or the one
	// whose branch it rebuilds (recreate, when the run found it).
	PR int64 `json:"pr,omitempty"`
	// Head is a recreate entry's branch head.
	Head string `json:"head,omitempty"`
	// Max and Until are the dated entries' (allow_mass_close,
	// adopt_unmarked): Until is YYYY-MM-DD, inclusive, UTC.
	Max   int    `json:"max,omitempty"`
	Until string `json:"until,omitempty"`
	// Effect is EffectApplies, EffectNone, EffectExpired or EffectUnknown;
	// Detail says what the entry does, or why it does nothing, in one line.
	Effect string `json:"effect"`
	Detail string `json:"detail"`
}

// operationRows are the rows of the Operations section: the kind (with
// "(flag)" for a local run's flag), what the entry names, and its detail.
func (d *Delivery) operationRows() [][]string {
	rows := make([][]string, 0, len(d.Operations))
	for _, op := range d.Operations {
		kind := op.Kind
		if op.Flag {
			kind += " (flag)"
		}
		rows = append(rows, []string{kind, d.operationSubject(op), op.Detail})
	}
	return rows
}

// operationSubject is what an entry names: its target and pull request,
// or its bounds.
func (d *Delivery) operationSubject(op Operation) string {
	var parts []string
	switch {
	case op.Target != "":
		parts = append(parts, op.Target)
	case op.Kind == OpRecreate || op.Kind == OpForgetDeclines:
		parts = append(parts, "a target not named here")
	}
	if op.PR > 0 && op.Target != "" {
		parts = append(parts, d.prSign(op.Provider)+strconv.FormatInt(op.PR, 10))
	}
	if op.Max > 0 {
		parts = append(parts, "max "+strconv.Itoa(op.Max))
	}
	if op.Until != "" {
		parts = append(parts, "until "+op.Until)
	}
	return strings.Join(parts, " ")
}

// writeTextOperations writes the Operations section of the text output:
// one line per entry, in the order of operations.yml (recreate,
// forget_declines, allow_mass_close, adopt_unmarked), then the flags.
func (d *Delivery) writeTextOperations(p *printer) {
	if len(d.Operations) == 0 {
		return
	}
	p.linef("Operations")
	for _, line := range alignRows(d.operationRows(), "  ") {
		p.linef("%s", line)
	}
	p.linef("")
}

// mdOperations writes the Operations section of the Markdown output.
func (d *Delivery) mdOperations(m *mdWriter) {
	if len(d.Operations) == 0 {
		return
	}
	m.line("")
	m.line("#### Operations")
	m.line("")
	m.line("| Operation | Entry | Effect |")
	m.line("|---|---|---|")
	for _, row := range d.operationRows() {
		// mdText escapes a pipe; a code span needs it escaped for the table.
		m.line(fmt.Sprintf("| %s | %s | %s |", mdText(row[0]), strings.ReplaceAll(mdCode(row[1]), "|", `\|`), mdText(row[2])))
	}
}
