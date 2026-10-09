package report

import (
	"cmp"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DeliverySchema is the schema id of delivery reports (schemas/report.schema.json).
const DeliverySchema = "report/v1"

// Outcome of one target (see docs/reference/output.md). In plan the same
// values mean "would".
type Outcome string

const (
	OutcomeOpened    Outcome = "opened"
	OutcomeUpdated   Outcome = "updated"
	OutcomeUnchanged Outcome = "unchanged"
	OutcomeClosed    Outcome = "closed"
	OutcomeDeclined  Outcome = "declined"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeBlocked   Outcome = "blocked"
	OutcomeDeferred  Outcome = "deferred"
	OutcomeFailed    Outcome = "failed"
)

// Outcomes lists every outcome in report order.
var Outcomes = []Outcome{OutcomeOpened, OutcomeUpdated, OutcomeUnchanged, OutcomeClosed, OutcomeDeclined, OutcomeSkipped, OutcomeBlocked, OutcomeDeferred, OutcomeFailed}

// Reasons allowed per outcome. Declined carries the PR
// number in PR, not a reason. A blocked rules or permission reason is
// "rules:<rule>" or "permission:<what>".
var Reasons = map[Outcome][]string{
	OutcomeOpened:    nil,
	OutcomeUpdated:   {"content", "rebase", "recreate", "body", "title", "base-renamed"},
	OutcomeUnchanged: nil,
	OutcomeClosed:    {"no-diff", "opted-out", "target-dropped", "duplicate"},
	OutcomeDeclined:  nil,
	OutcomeSkipped:   {"not-opted-in", "opted-out", "archived", "disabled", "empty", "mirror", "pending-deletion", "prs-disabled", "sha256", "unsafe-opt-in", "private-in-public-hub", "superseded"},
	OutcomeBlocked:   {"edited", "branch-taken", "branch-in-use", "opt-in-invalid", "marker-invalid", "rules:*", "permission:*", "cannot-sign", "archived", "mass-close"},
	OutcomeDeferred:  {"rate-limit", "deadline", "rollout-limit", "provider-down", "interrupted", "cooldown"},
	OutcomeFailed:    {"transient", "auth", "access", "git", "integrity", "race", "secret-exposure", "internal"},
}

// Why a target counts as opted in without an opt-in file
// (DeliveryTarget.AssumedBy).
const (
	// AssumedByHub: an entry of targets.yml that selects it has opt_in:
	// assumed.
	AssumedByHub = "targets.yml"
	// AssumedByFlag: plan --assume-opt-in, for the report only.
	AssumedByFlag = "--assume-opt-in"
)

// ReasonPrivate is the skip reason of a non-public target of a public hub.
// Such a target is only counted: its report entry has no path and no
// repository id, and no output names it: a public hub's CI logs are
// public.
const ReasonPrivate = "private-in-public-hub"

// RunOutcome is the outcome of a whole run.
type RunOutcome string

const (
	Completed  RunOutcome = "completed"
	Superseded RunOutcome = "superseded" // the hub moved on; nothing was written
)

// Delivery is the report of plan or distribute (report/v1).
type Delivery struct {
	Schema  string `json:"schema"`
	Command string `json:"command"` // plan | distribute
	// DryRun is set for distribute --dry-run: nothing was written, and the
	// outcomes and writes are what distribute would do, as in a plan.
	DryRun    bool              `json:"dry_run,omitempty"`
	Engine    string            `json:"engine"`
	Hub       DeliveryHub       `json:"hub"`
	Outcome   RunOutcome        `json:"outcome"`
	Strict    bool              `json:"strict"`
	Scope     *Scope            `json:"scope,omitempty"`         // the part of the fleet a plan processed (plan.go)
	Assumed   bool              `json:"assume_opt_in,omitempty"` // plan --assume-opt-in (plan.go)
	Providers []ProviderInfo    `json:"providers"`
	Targets   []DeliveryTarget  `json:"targets"`
	Ops       []Op              `json:"ops"`
	Warnings  []string          `json:"warnings"`
	Summary   map[Outcome]int   `json:"summary"`
	Sweep     SweepInfo         `json:"sweep"`
	Cost      map[string]int    `json:"cost"`               // provider id → estimated or actual writes
	Estimate  *Estimate         `json:"estimate,omitempty"` // a plan's or dry run's time and runs (plan.go)
	Paths     []PathChange      `json:"paths,omitempty"`    // what a plan's pushes change, by path (plan.go)
	Notes     map[string]string `json:"notes,omitempty"`

	// Operations are what each one-off operation does in a plan or a dry
	// run (operations.go).
	Operations []Operation `json:"operations,omitempty"`
}

// DeliveryHub identifies the hub run.
type DeliveryHub struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Commit      string `json:"commit"`
	PR          int64  `json:"pr,omitempty"` // plan on a hub PR
}

// ProviderInfo is one provider line of the report.
type ProviderInfo struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Host            string `json:"host"`
	Reader          string `json:"reader,omitempty"` // login
	Writer          string `json:"writer,omitempty"` // login from hub.yml or Self()
	WriteCheck      string `json:"write_check"`      // "not checked", "Developer 41/42", …
	ResolveComplete bool   `json:"resolve_complete"`
	// Missing are the targets.yml repositories ("<provider>:<path>") the
	// platform does not know (the warning target-missing). They do not make
	// the resolve incomplete, but plan --strict exits 3 for them.
	Missing []string `json:"missing,omitempty"`
	Error   string   `json:"error,omitempty"` // provider unavailable
}

// DeliveryTarget is one target line.
type DeliveryTarget struct {
	Provider string  `json:"provider"`
	Host     string  `json:"host"`
	RepoID   string  `json:"repo_id"`
	Path     string  `json:"path"`
	Outcome  Outcome `json:"outcome"`
	Reason   string  `json:"reason,omitempty"`
	PR       *PRRef  `json:"pr,omitempty"`
	// Key is the content key of D, the hash of its (path, from, mode, to)
	// changes, "" when D is empty.
	Key   string   `json:"key,omitempty"`
	Packs []string `json:"packs,omitempty"`
	// Assumed is set when the target was processed without an opt-in file,
	// as if it had an empty one; AssumedBy says why: AssumedByHub (an entry
	// of targets.yml with opt_in: assumed subscribes it) or AssumedByFlag
	// (plan --assume-opt-in took an empty opt-in file for a missing or
	// unsafe one).
	Assumed   bool         `json:"opt_in_assumed,omitempty"`
	AssumedBy string       `json:"opt_in_assumed_by,omitempty"`
	Changes   ChangeCounts `json:"changes"`
	Orphaned  []string     `json:"orphaned,omitempty"`
	Warnings  []string     `json:"warnings,omitempty"`
	// Writes is the estimated (plan) or actual (distribute) number of writes
	// for this target: HTTP writes of the API and git pushes, as the
	// throttle counts them, for the mutations that went
	// through.
	Writes int `json:"writes"`
}

// PRRef names a pull request.
type PRRef struct {
	Number int64  `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// ChangeCounts counts D by action.
type ChangeCounts struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Delete int `json:"delete"`
	Chmod  int `json:"chmod"`
}

// Op is one mutation, for the audit journal (distribute only).
type Op struct {
	Time    time.Time `json:"time"`
	Account string    `json:"account"`
	Target  string    `json:"target"`
	Kind    string    `json:"kind"` // one of OpKinds
	PR      int64     `json:"pr,omitempty"`
	Before  string    `json:"before,omitempty"`
	After   string    `json:"after,omitempty"`
}

// OpKinds are the mutations the journal records, every one of them: a git
// push; a pull request created or edited (body, state, title, base,
// labels); a comment; a branch deleted; a label created for a first pull
// request; and the steps of an API commit through the stage ref: the
// commit object, the refs moved in one call, and a stage
// ref deleted on its own.
var OpKinds = []string{"push", "create-pr", "edit-pr", "comment", "delete-branch", "create-label", "api-commit", "update-refs", "delete-ref"}

// SweepInfo reports the stale-PR sweep.
type SweepInfo struct {
	Ran      bool   `json:"ran"`
	Complete bool   `json:"complete"`
	Failed   bool   `json:"failed"`
	Reason   string `json:"reason,omitempty"` // why it did not run
}

// Commands of delivery reports.
const (
	commandPlan = "plan"
)

// Exit codes of delivery commands (see docs/reference/exit-codes.md).
const (
	exitOK     = 0
	exitFailed = 1
	exitStrict = 3
)

// NewDelivery returns an empty report for command: schema report/v1,
// outcome completed, every outcome counted zero, and empty (not nil)
// lists and maps, so the JSON always has every field.
func NewDelivery(command, engine string) *Delivery {
	d := &Delivery{
		Schema:    DeliverySchema,
		Command:   command,
		Engine:    engine,
		Outcome:   Completed,
		Providers: []ProviderInfo{},
		Targets:   []DeliveryTarget{},
		Ops:       []Op{},
		Warnings:  []string{},
		Summary:   make(map[Outcome]int, len(Outcomes)),
		Cost:      map[string]int{},
		Notes:     map[string]string{},
	}
	for _, o := range Outcomes {
		d.Summary[o] = 0
	}
	return d
}

// Summarize recomputes Summary from Targets (every outcome present, zero
// included) and sorts Targets by (provider, path). Paths compare ignoring
// case first; the sort is stable, so targets without a path (private in a
// public hub) keep their order.
func (d *Delivery) Summarize() {
	sum := make(map[Outcome]int, len(Outcomes))
	for _, o := range Outcomes {
		sum[o] = 0
	}
	for _, t := range d.Targets {
		sum[t.Outcome]++
	}
	d.Summary = sum
	slices.SortStableFunc(d.Targets, func(a, b DeliveryTarget) int {
		return cmp.Or(
			cmp.Compare(a.Provider, b.Provider),
			cmp.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)),
			cmp.Compare(a.Path, b.Path),
		)
	})
}

// ExitCode returns the exit code:
//   - 1 when any target failed, a provider reported Error, the sweep failed,
//     or a target is blocked with reason "mass-close";
//   - otherwise 0 for a superseded run;
//   - otherwise, with Strict, 3 when any target is blocked or deferred, the
//     sweep did not run while a sweep was expected (Sweep.Reason set), or —
//     for plan — some provider's resolve was incomplete or a repository
//     named in targets.yml was missing (ProviderInfo.Missing);
//   - otherwise 0.
//
// Code 2 (configuration and guards) is decided before a report exists.
func (d *Delivery) ExitCode() int {
	for _, t := range d.Targets {
		if t.Outcome == OutcomeFailed || (t.Outcome == OutcomeBlocked && t.Reason == "mass-close") {
			return exitFailed
		}
	}
	for _, p := range d.Providers {
		if p.Error != "" {
			return exitFailed
		}
	}
	if d.Sweep.Failed {
		return exitFailed
	}
	if d.Outcome == Superseded || !d.Strict {
		return exitOK
	}
	for _, t := range d.Targets {
		if t.Outcome == OutcomeBlocked || t.Outcome == OutcomeDeferred {
			return exitStrict
		}
	}
	if d.Sweep.Reason != "" {
		return exitStrict
	}
	if d.Command == commandPlan {
		for _, p := range d.Providers {
			if !p.ResolveComplete || len(p.Missing) > 0 {
				return exitStrict
			}
		}
	}
	return exitOK
}

// examples is how many targets a text group line names.
const examples = 3

// maxMarkdown bounds WriteMarkdown's output: the size limit of a GitHub
// Actions step summary.
const maxMarkdown = 1 << 20

// WriteText renders the report for a terminal or CI log:
// a header line with the command, hub id, fingerprint and short commit and
// engine version; one line per provider; one line per non-empty outcome
// group with the count and up to 3 example targets ("<provider>:<path>",
// with "#<pr>" ("!<pr>" on GitLab) and "(reason)" when known) followed by
// "and N more"; a plan's or a dry run's Operations (what each one-off
// operation does) and paths; a "Warnings" block (provider errors, run
// warnings, and per target its orphaned files and warnings); and a "Cost"
// line with the writes per provider. Targets skipped because they are
// private in a public hub are only counted, never named. A superseded run
// prints the header and its warnings only.
func (d *Delivery) WriteText(w io.Writer) error {
	p := &printer{w: w}
	p.linef("%s", d.header())
	if d.Outcome == Superseded {
		p.linef("superseded: the hub's default branch has moved on; no target was inspected")
		d.writeTextWarnings(p)
		return p.err
	}
	for _, line := range alignRows(d.providerRows(), "") {
		p.linef("%s", line)
	}
	for _, line := range d.planLines() {
		p.linef("%s", line)
	}
	p.linef("")
	groups := d.groups()
	if len(groups) == 0 {
		p.linef("  no targets")
	}
	width := 0
	for _, g := range groups {
		width = max(width, len(strconv.Itoa(len(g.targets))))
	}
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, []string{g.label, fmt.Sprintf("%*d", width, len(g.targets)), d.groupExamples(g)})
	}
	for _, line := range alignRows(rows, "  ") {
		p.linef("%s", line)
	}
	p.linef("")
	d.writeTextOperations(p)
	d.writeTextPaths(p)
	d.writeTextWarnings(p)
	if line := d.costLine(); line != "" {
		p.linef("Cost  %s", line)
	}
	return p.err
}

// would reports whether the report says what distribute would do: a plan's
// or a dry run's.
func (d *Delivery) would() bool { return d.Command == commandPlan || d.DryRun }

// title is the command as the output names it: "plan", "distribute" or
// "distribute --dry-run".
func (d *Delivery) title() string {
	if d.DryRun {
		return d.Command + " --dry-run"
	}
	return d.Command
}

// header is the first line of the text output.
func (d *Delivery) header() string {
	var b strings.Builder
	b.WriteString("touchmark " + d.title())
	if d.Hub.ID != "" || d.Hub.Fingerprint != "" || d.Hub.Commit != "" {
		b.WriteString(" · hub")
		if d.Hub.ID != "" {
			b.WriteString(" " + d.Hub.ID)
		}
		if d.Hub.Fingerprint != "" {
			b.WriteString(" (" + d.Hub.Fingerprint + ")")
		}
		if c := shortOID(d.Hub.Commit); c != "" {
			b.WriteString(" @ " + c)
		}
		if d.Hub.PR > 0 {
			fmt.Fprintf(&b, " (PR #%d)", d.Hub.PR)
		}
	}
	if d.Engine != "" {
		b.WriteString(" · touchmark " + d.Engine)
	}
	return b.String()
}

// providerRows are the provider lines: id, host, reader, writer and the
// state of the resolve.
func (d *Delivery) providerRows() [][]string {
	rows := make([][]string, 0, len(d.Providers))
	for _, p := range d.Providers {
		rows = append(rows, []string{
			p.ID,
			p.Host,
			"read " + orUnknown(p.Reader, "?"),
			"write " + orUnknown(p.Writer, "none") + ": " + orUnknown(p.WriteCheck, "not checked"),
			providerState(p),
		})
	}
	return rows
}

// providerState tells whether the provider resolved every target.
func providerState(p ProviderInfo) string {
	var s string
	switch {
	case p.Error != "":
		return "unavailable"
	case p.ResolveComplete:
		s = "resolve complete"
	default:
		s = "resolve incomplete"
	}
	if n := len(p.Missing); n > 0 {
		s += ", " + plural(n, "target") + " missing"
	}
	return s
}

// group is the targets with one outcome, in report order.
type group struct {
	outcome Outcome
	label   string
	targets []DeliveryTarget
}

// groups returns the non-empty outcome groups in the order of Outcomes;
// outcomes outside Outcomes follow, sorted.
func (d *Delivery) groups() []group {
	by := map[Outcome][]DeliveryTarget{}
	var extra []Outcome
	for _, t := range d.Targets {
		if _, seen := by[t.Outcome]; !seen && !slices.Contains(Outcomes, t.Outcome) {
			extra = append(extra, t.Outcome)
		}
		by[t.Outcome] = append(by[t.Outcome], t)
	}
	slices.Sort(extra)
	var out []group
	for _, o := range slices.Concat(Outcomes, extra) {
		if ts := by[o]; len(ts) > 0 {
			out = append(out, group{outcome: o, label: d.label(o), targets: ts})
		}
	}
	return out
}

// label names an outcome group: plan and a dry run use verbs for what
// distribute would do.
func (d *Delivery) label(o Outcome) string {
	if d.would() {
		switch o {
		case OutcomeOpened:
			return "open"
		case OutcomeUpdated:
			return "update"
		case OutcomeClosed:
			return "close"
		}
	}
	if o == "" {
		return "(none)"
	}
	return string(o)
}

// groupExamples names up to 3 targets of g, then how many more there are
// and how many are private in a public hub.
func (d *Delivery) groupExamples(g group) string {
	var named []string
	hidden := 0
	for _, t := range g.targets {
		if isHidden(t) {
			hidden++
			continue
		}
		if len(named) < examples {
			named = append(named, d.targetLabel(t))
		}
	}
	shown := len(named)
	s := strings.Join(named, ", ")
	if more := len(g.targets) - hidden - shown; more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	if hidden > 0 {
		if s != "" {
			s += "; "
		}
		s += fmt.Sprintf("%d private in public hub", hidden)
	}
	return s
}

// targetLabel is "<provider>:<path>", with the PR and the reason when
// known.
func (d *Delivery) targetLabel(t DeliveryTarget) string {
	s := targetRef(t)
	if t.PR != nil && t.PR.Number > 0 {
		s += " " + d.prSign(t.Provider) + strconv.FormatInt(t.PR.Number, 10)
	}
	if t.Reason != "" {
		s += " (" + t.Reason + ")"
	}
	return s
}

// prSign is how the platform of provider numbers pull requests: "!" for
// GitLab merge requests, "#" elsewhere.
func (d *Delivery) prSign(provider string) string {
	for _, p := range d.Providers {
		if p.ID == provider && p.Type == "gitlab" {
			return "!"
		}
	}
	return "#"
}

// targetRef is "<provider>:<path>".
func targetRef(t DeliveryTarget) string { return t.Provider + ":" + t.Path }

// isHidden reports whether t is a non-public target of a public hub, which
// is never named: a target without a path (skipped:private-in-public-hub,
// or a non-public repository the sweep closes a pull request in). Its
// warnings are never printed either: platform messages may name it.
func isHidden(t DeliveryTarget) bool {
	return t.Path == "" || (t.Outcome == OutcomeSkipped && t.Reason == ReasonPrivate)
}

// warningLines returns the warnings in output order: provider errors, run
// warnings, then per named target its orphaned files and warnings.
func (d *Delivery) warningLines() []string {
	var out []string
	for _, p := range d.Providers {
		if p.Error != "" {
			out = append(out, "provider "+p.ID+": "+p.Error)
		}
	}
	out = append(out, d.Warnings...)
	for _, t := range d.Targets {
		if isHidden(t) {
			continue
		}
		if n := len(t.Orphaned); n > 0 {
			out = append(out, fmt.Sprintf("%s: orphaned %d: %s", targetRef(t), n, listSome(t.Orphaned, examples)))
		}
		for _, w := range t.Warnings {
			out = append(out, targetRef(t)+": "+w)
		}
	}
	return out
}

func (d *Delivery) writeTextWarnings(p *printer) {
	lines := d.warningLines()
	if len(lines) == 0 {
		return
	}
	p.linef("Warnings")
	for _, l := range lines {
		p.linef("  %s", strings.ReplaceAll(l, "\n", "\n    "))
	}
}

// costLine lists the writes per provider, in the order of Providers, then
// any other provider of Cost by id; "" when there is nothing to list. Plan
// writes are estimates; with an Estimate, the line is its (estimateLine).
func (d *Delivery) costLine() string {
	if d.Estimate != nil && d.would() {
		return d.estimateLine()
	}
	var ids []string
	for _, p := range d.Providers {
		if !slices.Contains(ids, p.ID) {
			ids = append(ids, p.ID)
		}
	}
	var rest []string
	for id := range d.Cost {
		if !slices.Contains(ids, id) {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	ids = append(ids, rest...)
	approx := ""
	if d.would() {
		approx = "≈ "
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+" "+approx+plural(d.Cost[id], "write"))
	}
	return strings.Join(parts, " · ")
}

// listSome joins up to n items and says how many more there are.
func listSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// alignRows renders rows with every column but the last padded to its
// widest cell, prefixed with indent, without trailing spaces.
func alignRows(rows [][]string, indent string) []string {
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var b strings.Builder
		b.WriteString(indent)
		for i, cell := range row {
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// shortOID returns the first hex digits of a commit id that text output
// shows.
func shortOID(oid string) string {
	if len(oid) > shortCommit {
		return oid[:shortCommit]
	}
	return oid
}

func orUnknown(s, unknown string) string {
	if s == "" {
		return unknown
	}
	return s
}

// WriteMarkdown renders the same content for $GITHUB_STEP_SUMMARY and the
// GitLab artifact: the header, a provider table, a counts table and one
// collapsible section per non-empty group listing every target, the
// warnings and the cost, at most 1 MiB (the rest is cut with a note).
// Targets private in a public hub are only counted. Text from targets and
// platforms is escaped or put in code spans, so it cannot add markup.
func (d *Delivery) WriteMarkdown(w io.Writer) error {
	m := &mdWriter{limit: maxMarkdown}
	d.markdown(m)
	_, err := io.WriteString(w, m.finish())
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// markdown writes the Markdown report to m (WriteMarkdown, WriteComment).
func (d *Delivery) markdown(m *mdWriter) {
	m.line("### touchmark " + d.title())
	m.line("")
	m.line(d.mdHeader())
	if d.Outcome == Superseded {
		m.line("")
		m.line("**Superseded:** the hub's default branch has moved on; no target was inspected.")
	} else {
		d.mdProviders(m)
		d.mdPlanLines(m)
		d.mdGroups(m)
		d.mdOperations(m)
		d.mdPaths(m)
	}
	d.mdWarnings(m)
	if line := d.costLine(); line != "" {
		m.line("")
		m.line("**Cost:** " + mdText(line))
	}
}

// mdHeader is the hub line of the Markdown output.
func (d *Delivery) mdHeader() string {
	var parts []string
	hub := "Hub"
	if d.Hub.ID != "" {
		hub += " " + mdCode(d.Hub.ID)
	}
	if d.Hub.Fingerprint != "" {
		hub += " (" + mdCode(d.Hub.Fingerprint) + ")"
	}
	if c := shortOID(d.Hub.Commit); c != "" {
		hub += " at " + mdCode(c)
	}
	if d.Hub.PR > 0 {
		hub += fmt.Sprintf(" (PR #%d)", d.Hub.PR)
	}
	parts = append(parts, hub)
	if d.Engine != "" {
		parts = append(parts, "touchmark "+mdText(d.Engine))
	}
	if d.Strict {
		parts = append(parts, "strict")
	}
	return strings.Join(parts, " · ")
}

func (d *Delivery) mdProviders(m *mdWriter) {
	if len(d.Providers) == 0 {
		return
	}
	m.line("")
	m.line("| Provider | Host | Read | Write | Resolve |")
	m.line("|---|---|---|---|---|")
	for _, p := range d.Providers {
		m.line("| " + strings.Join([]string{
			mdCode(p.ID),
			mdCode(p.Host),
			mdCodeOr(p.Reader, "?"),
			mdCodeOr(p.Writer, "none") + ": " + mdText(orUnknown(p.WriteCheck, "not checked")),
			mdText(providerState(p)),
		}, " | ") + " |")
	}
}

func (d *Delivery) mdGroups(m *mdWriter) {
	groups := d.groups()
	m.line("")
	if len(groups) == 0 {
		m.line("No targets.")
		return
	}
	m.line("| Outcome | Targets |")
	m.line("|---|---:|")
	for _, g := range groups {
		m.line(fmt.Sprintf("| %s | %d |", mdText(g.label), len(g.targets)))
	}
	for _, g := range groups {
		m.line("")
		m.details(fmt.Sprintf("%s (%d)", mdText(g.label), len(g.targets)))
		m.line("")
		hidden := 0
		for _, t := range g.targets {
			if isHidden(t) {
				hidden++
				continue
			}
			m.line("- " + d.mdTarget(t))
		}
		if hidden > 0 {
			m.line(fmt.Sprintf("- %d private in public hub, not named", hidden))
		}
		m.line("")
		m.endDetails()
	}
}

// mdTarget is one target of a group: its name, PR (a link when the URL is
// a plain http(s) URL) and reason.
func (d *Delivery) mdTarget(t DeliveryTarget) string {
	s := mdCode(targetRef(t))
	if t.PR != nil && t.PR.Number > 0 {
		pr := d.prSign(t.Provider) + strconv.FormatInt(t.PR.Number, 10)
		if u := linkURL(t.PR.URL); u != "" {
			pr = "[" + mdText(pr) + "](" + u + ")"
		} else {
			pr = mdText(pr)
		}
		s += " " + pr
	}
	if t.Reason != "" {
		s += " (" + mdText(t.Reason) + ")"
	}
	return s
}

func (d *Delivery) mdWarnings(m *mdWriter) {
	lines := d.warningLines()
	if len(lines) == 0 {
		return
	}
	m.line("")
	m.line("#### Warnings")
	m.line("")
	for _, l := range lines {
		m.line("- " + mdText(l))
	}
}

// mdWriter accumulates Markdown lines up to a byte limit. Past the limit it
// drops the rest, closes an open <details> and ends with a note.
type mdWriter struct {
	limit int
	// note ends a cut report; cutNote when empty.
	note string
	b    strings.Builder
	// openTag is set while a <details> section is open.
	openTag bool
	cut     bool
	// plain writes Markdown without HTML, for a platform that shows HTML as
	// text (Bitbucket escapes it): a section is a bold title, not
	// <details>.
	plain bool
}

// cutNote ends a cut report.
const cutNote = "\n*This summary is cut at 1 MiB; the JSON report lists every target.*\n"

// closeDetails closes a <details> section cut in the middle.
const closeDetails = "\n</details>\n"

// cutText is the note that ends a cut report.
func (m *mdWriter) cutText() string {
	if m.note != "" {
		return m.note
	}
	return cutNote
}

// line appends one line unless the report would exceed its limit with the
// closing tag and the note.
func (m *mdWriter) line(s string) {
	if m.cut {
		return
	}
	if m.b.Len()+len(s)+1+len(closeDetails)+len(m.cutText()) > m.limit {
		m.cut = true
		return
	}
	m.b.WriteString(s)
	m.b.WriteByte('\n')
}

func (m *mdWriter) open(s string) {
	m.line(s)
	if !m.cut {
		m.openTag = true
	}
}

func (m *mdWriter) close(s string) {
	m.line(s)
	if !m.cut {
		m.openTag = false
	}
}

// details opens a collapsible section titled summary (Markdown text): a
// <details> block, or with plain a bold line.
func (m *mdWriter) details(summary string) {
	if m.plain {
		m.line("**" + summary + "**")
		return
	}
	m.open("<details><summary>" + summary + "</summary>")
}

// endDetails closes the section details opened.
func (m *mdWriter) endDetails() {
	if !m.plain {
		m.close("</details>")
	}
}

// finish returns the Markdown, cut with a note when it did not fit.
func (m *mdWriter) finish() string {
	if !m.cut {
		return m.b.String()
	}
	if m.openTag {
		m.b.WriteString(closeDetails)
	}
	m.b.WriteString(m.cutText())
	return m.b.String()
}

// mdCode puts s in a code span: control characters become spaces, and the
// fence is longer than any run of backticks in s.
func mdCode(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidiControl(r) {
			return ' '
		}
		return r
	}, s)
	if s == "" {
		return "` `"
	}
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// mdCodeOr is mdCode of s, or the plain placeholder when s is empty.
func mdCodeOr(s, placeholder string) string {
	if s == "" {
		return mdText(placeholder)
	}
	return mdCode(s)
}

// mdText escapes s for Markdown text: HTML special characters become
// entities, Markdown punctuation is backslash-escaped, and control
// characters (newlines included) become spaces.
func mdText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case unicode.IsControl(r) || isBidiControl(r):
			b.WriteByte(' ')
		case strings.ContainsRune("\\`*_{}[]()#+-.!|~:@", r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// linkURL returns u when it is an absolute http(s) URL that is safe inside
// a Markdown link: no spaces, parentheses, angle brackets or quotes; "" otherwise.
func linkURL(u string) string {
	if u == "" || strings.ContainsFunc(u, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune(`()<>"'\`+"`", r) || r > unicode.MaxASCII
	}) {
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return u
}

// isBidiControl reports whether r reorders text when rendered.
func isBidiControl(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
