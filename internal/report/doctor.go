package report

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// DoctorSchema is the schema id of doctor reports
// (schemas/doctor.schema.json).
const DoctorSchema = "doctor/v1"

// CheckStatus grades one check of doctor: a check the
// write identity cannot read is unknown, never ok.
type CheckStatus string

const (
	StatusOK      CheckStatus = "ok"
	StatusWarn    CheckStatus = "warn"
	StatusFail    CheckStatus = "fail"
	StatusUnknown CheckStatus = "unknown"
)

// Statuses lists every status in report order.
var Statuses = []CheckStatus{StatusOK, StatusWarn, StatusFail, StatusUnknown}

// DoctorChecks lists the checks doctor knows, in the order its outputs show
// them; other names follow, sorted.
//   - hub: write-isolation (security.write_isolation and what this job
//     can tell of it), environment (GitHub Actions: who may use the
//     environment touchmark-distribute), key-location (doctor
//     --hub-token: where the write key lives and who can read it),
//     pipeline-variables, protected-branches and protected-tags (GitLab,
//     --hub-token);
//   - provider: writer (the write credential acts as hub.yml's writer),
//     token-expiry, scopes (a token's scopes), 2fa, hub-hidden (the writer
//     cannot write to the hub), hub-guard (under security.writer_on_hub
//     guard: the writer reaches the hub but cannot get content onto its
//     default branch), signing (whether pushes
//     that need a signature will get one), signing-key (the key is the
//     writer's on the platform);
//   - target: opt-in (the file could not be read), access, permissions,
//     workflows, rules, signing, markers (markers with the hub's id and a
//     foreign fingerprint, the hub's markers by unknown authors).
var DoctorChecks = []string{
	"write-isolation", "environment", "key-location", "pipeline-variables", "protected-branches", "protected-tags",
	"writer", "token-expiry", "scopes", "2fa", "hub-hidden", "hub-guard", "signing", "signing-key",
	"opt-in", "access", "permissions", "workflows", "rules", "markers",
}

// Doctor is the report of doctor (doctor/v1): the checks of each
// provider's write identity, of each target, and of the hub.
type Doctor struct {
	Schema  string      `json:"schema"`
	Command string      `json:"command"` // doctor
	Engine  string      `json:"engine"`
	Hub     DeliveryHub `json:"hub"`
	Strict  bool        `json:"strict"`
	// HubToken is set when --hub-token read where the hub keeps its
	// secrets.
	HubToken bool `json:"hub_token"`
	// HubChecks are the checks of the hub itself: write isolation, and with
	// --hub-token where the write key lives.
	HubChecks []DoctorCheck    `json:"hub_checks"`
	Providers []DoctorProvider `json:"providers"`
	// Targets are the targets of targets.yml, sorted by provider and path.
	// A non-public target of a public hub has no path, no repository id and
	// no check details: it is only counted.
	Targets  []DoctorTarget      `json:"targets"`
	Warnings []string            `json:"warnings"`
	Summary  map[CheckStatus]int `json:"summary"`
}

// DoctorCheck is one check and its status.
type DoctorCheck struct {
	Name   string      `json:"check"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

// DoctorProvider is one provider: its write identity and the checks of it.
type DoctorProvider struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Host string `json:"host"`
	// Writer is the login hub.yml names; Self the one the write credential
	// acts as ("" when it could not be read).
	Writer string `json:"writer,omitempty"`
	Self   string `json:"self,omitempty"`
	// ResolveComplete is false when not every target could be listed.
	ResolveComplete bool `json:"resolve_complete"`
	// Missing are the targets.yml repositories the platform does not know.
	Missing []string      `json:"missing,omitempty"`
	Error   string        `json:"error,omitempty"` // the provider could not be checked
	Checks  []DoctorCheck `json:"checks"`
}

// DoctorTarget is one target and its checks.
type DoctorTarget struct {
	Provider string `json:"provider"`
	Host     string `json:"host"`
	RepoID   string `json:"repo_id"`
	Path     string `json:"path"`
	// Skipped says why the target was not checked: a skip reason of
	// delivery (archived, not-opted-in, …), or deferred:<reason> when the
	// run could not check it (the provider took no more calls; such a target
	// counts as unknown); "" when it was.
	Skipped string        `json:"skipped,omitempty"`
	Checks  []DoctorCheck `json:"checks"`
}

// NewDoctor returns an empty doctor report: every list empty (not nil) and
// every status counted zero.
func NewDoctor(engine string) *Doctor {
	d := &Doctor{
		Schema:    DoctorSchema,
		Command:   "doctor",
		Engine:    engine,
		HubChecks: []DoctorCheck{},
		Providers: []DoctorProvider{},
		Targets:   []DoctorTarget{},
		Warnings:  []string{},
	}
	d.Summarize()
	return d
}

// hidden reports whether t is a target the report does not name.
func (t DoctorTarget) hidden() bool { return t.Path == "" }

// Summarize counts the checks by status (every status present), gives
// every list a non-nil value, and sorts the targets by provider and path
// (targets without a path keep their order at the end of their provider).
func (d *Doctor) Summarize() {
	sum := make(map[CheckStatus]int, len(Statuses))
	for _, s := range Statuses {
		sum[s] = 0
	}
	count := func(cs []DoctorCheck) {
		for _, c := range cs {
			sum[c.Status]++
		}
	}
	if d.HubChecks == nil {
		d.HubChecks = []DoctorCheck{}
	}
	if d.Warnings == nil {
		d.Warnings = []string{}
	}
	if d.Providers == nil {
		d.Providers = []DoctorProvider{}
	}
	if d.Targets == nil {
		d.Targets = []DoctorTarget{}
	}
	count(d.HubChecks)
	for i := range d.Providers {
		if d.Providers[i].Checks == nil {
			d.Providers[i].Checks = []DoctorCheck{}
		}
		count(d.Providers[i].Checks)
	}
	for i := range d.Targets {
		if d.Targets[i].Checks == nil {
			d.Targets[i].Checks = []DoctorCheck{}
		}
		count(d.Targets[i].Checks)
	}
	d.Summary = sum
	slices.SortStableFunc(d.Targets, func(a, b DoctorTarget) int {
		return cmp.Or(
			cmp.Compare(a.Provider, b.Provider),
			cmp.Compare(boolRank(a.hidden()), boolRank(b.hidden())),
			cmp.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path)),
			cmp.Compare(a.Path, b.Path),
		)
	})
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ExitCode returns doctor's exit code: 1 when a check
// failed or a provider could not be checked; otherwise, with Strict, 3 when
// a check warns or is unknown, a target was deferred unchecked, or a
// provider's targets could not all be listed; otherwise 0.
func (d *Doctor) ExitCode() int {
	all := d.allChecks()
	for _, c := range all {
		if c.Status == StatusFail {
			return exitFailed
		}
	}
	for _, p := range d.Providers {
		if p.Error != "" {
			return exitFailed
		}
	}
	if !d.Strict {
		return exitOK
	}
	for _, c := range all {
		if c.Status == StatusWarn || c.Status == StatusUnknown {
			return exitStrict
		}
	}
	for _, p := range d.Providers {
		if !p.ResolveComplete || len(p.Missing) > 0 {
			return exitStrict
		}
	}
	for _, t := range d.Targets {
		if strings.HasPrefix(t.Skipped, "deferred:") {
			return exitStrict
		}
	}
	return exitOK
}

func (d *Doctor) allChecks() []DoctorCheck {
	all := slices.Clone(d.HubChecks)
	for _, p := range d.Providers {
		all = append(all, p.Checks...)
	}
	for _, t := range d.Targets {
		all = append(all, t.Checks...)
	}
	return all
}

// checkOrder sorts check names as DoctorChecks lists them, others after.
func checkOrder(a, b string) int {
	ia, ib := slices.Index(DoctorChecks, a), slices.Index(DoctorChecks, b)
	switch {
	case ia >= 0 && ib >= 0:
		return cmp.Compare(ia, ib)
	case ia >= 0:
		return -1
	case ib >= 0:
		return 1
	}
	return cmp.Compare(a, b)
}

// columns are the check names of the named targets that were checked, in
// check order.
func (d *Doctor) columns() []string {
	var cols []string
	for _, t := range d.Targets {
		if t.hidden() || t.Skipped != "" {
			continue
		}
		for _, c := range t.Checks {
			if !slices.Contains(cols, c.Name) {
				cols = append(cols, c.Name)
			}
		}
	}
	slices.SortFunc(cols, checkOrder)
	return cols
}

// cell is the status of check name in cs: the worst of them when a check
// repeats, "-" when absent.
func cell(cs []DoctorCheck, name string) string {
	worst := ""
	rank := func(s CheckStatus) int {
		switch s {
		case StatusFail:
			return 3
		case StatusWarn:
			return 2
		case StatusUnknown:
			return 1
		}
		return 0
	}
	best := -1
	for _, c := range cs {
		if c.Name == name && rank(c.Status) > best {
			best, worst = rank(c.Status), string(c.Status)
		}
	}
	if worst == "" {
		return "-"
	}
	return worst
}

// doctorRef is "<provider>:<path>".
func doctorRef(t DoctorTarget) string { return t.Provider + ":" + t.Path }

// problem is a check that is not ok, with where it belongs.
type problem struct {
	where string
	check DoctorCheck
}

// problems returns the checks that are not ok of the hub, the providers
// and the named targets, in report order.
func (d *Doctor) problems() []problem {
	var out []problem
	add := func(where string, cs []DoctorCheck) {
		for _, c := range cs {
			if c.Status != StatusOK {
				out = append(out, problem{where, c})
			}
		}
	}
	add("hub", d.HubChecks)
	for _, p := range d.Providers {
		add("provider "+p.ID, p.Checks)
	}
	for _, t := range d.Targets {
		if !t.hidden() {
			add(doctorRef(t), t.Checks)
		}
	}
	return out
}

// hiddenLine counts the targets a public hub does not name and the
// statuses of their checks; "" when there are none.
func (d *Doctor) hiddenLine() string {
	n := 0
	counts := map[CheckStatus]int{}
	skipped := 0
	for _, t := range d.Targets {
		if !t.hidden() {
			continue
		}
		n++
		if t.Skipped != "" {
			skipped++
		}
		for _, c := range t.Checks {
			counts[c.Status]++
		}
	}
	if n == 0 {
		return ""
	}
	s := plural(n, "target") + " private in a public hub, not named"
	var parts []string
	for _, st := range Statuses {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, counts[st]))
		}
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("skipped %d", skipped))
	}
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, ", ")
	}
	return s
}

// targetsLine counts the named targets checked and skipped, by reason.
func (d *Doctor) targetsLine() string {
	checked := 0
	skipped := map[string]int{}
	for _, t := range d.Targets {
		switch {
		case t.hidden():
		case t.Skipped != "":
			skipped[t.Skipped]++
		default:
			checked++
		}
	}
	s := plural(checked, "target") + " checked"
	if len(skipped) > 0 {
		total := 0
		var reasons []string
		for _, r := range slices.Sorted(maps.Keys(skipped)) {
			total += skipped[r]
			reasons = append(reasons, fmt.Sprintf("%s %d", r, skipped[r]))
		}
		s += fmt.Sprintf(" · skipped %d (%s)", total, strings.Join(reasons, ", "))
	}
	return s
}

// summaryLine is "ok N · warn N · fail N · unknown N".
func (d *Doctor) summaryLine() string {
	parts := make([]string, 0, len(Statuses))
	for _, s := range Statuses {
		parts = append(parts, fmt.Sprintf("%s %d", s, d.Summary[s]))
	}
	return strings.Join(parts, " · ")
}

// header is the first line of the text output.
func (d *Doctor) header() string {
	var b strings.Builder
	b.WriteString("touchmark doctor")
	if d.HubToken {
		b.WriteString(" --hub-token")
	}
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
	}
	if d.Engine != "" {
		b.WriteString(" · touchmark " + d.Engine)
	}
	return b.String()
}

// providerTitle is "gh (github.com), writer acme-write[bot]".
func providerTitle(p DoctorProvider) string {
	s := p.ID + " (" + p.Host + ")"
	switch {
	case p.Self != "" && p.Self != p.Writer && p.Writer != "":
		s += ", writer " + p.Writer + ", credential of " + p.Self
	case p.Writer != "":
		s += ", writer " + p.Writer
	case p.Self != "":
		s += ", credential of " + p.Self
	}
	return s
}

// checkRows are the rows of a list of checks: name, status, detail.
func checkRows(cs []DoctorCheck) [][]string {
	rows := make([][]string, 0, len(cs))
	for _, c := range cs {
		rows = append(rows, []string{c.Name, string(c.Status), oneLineText(c.Detail)})
	}
	return rows
}

// oneLineText replaces line breaks and other control characters with
// spaces: a detail quotes platform messages, and a line of its own could
// pass for something else in a CI log.
func oneLineText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || isBidiControl(r) {
			return ' '
		}
		return r
	}, s)
}

// WriteText renders the report for a terminal or a CI log: the header;
// the hub's checks; per provider its checks; the targets as a matrix of
// target × check (named targets that were checked), how many were skipped
// and how many a public hub does not name; every check that is not ok
// with its detail; the warnings; and the counts by status.
func (d *Doctor) WriteText(w io.Writer) error {
	p := &printer{w: w}
	p.linef("%s", d.header())
	if len(d.HubChecks) > 0 {
		p.linef("Hub")
		for _, line := range alignRows(checkRows(d.HubChecks), "  ") {
			p.linef("%s", line)
		}
	}
	for _, pr := range d.Providers {
		p.linef("Provider %s", providerTitle(pr))
		if pr.Error != "" {
			p.linef("  unavailable: %s", oneLineText(pr.Error))
		}
		for _, line := range alignRows(checkRows(pr.Checks), "  ") {
			p.linef("%s", line)
		}
		if n := len(pr.Missing); n > 0 {
			p.linef("  missing: %s", listSome(pr.Missing, examples))
		}
		if !pr.ResolveComplete {
			p.linef("  resolve incomplete: not every target was listed")
		}
	}
	p.linef("Targets: %s", d.targetsLine())
	if cols := d.columns(); len(cols) > 0 {
		rows := [][]string{append([]string{"target"}, cols...)}
		for _, t := range d.Targets {
			if t.hidden() || t.Skipped != "" {
				continue
			}
			row := []string{doctorRef(t)}
			for _, c := range cols {
				row = append(row, cell(t.Checks, c))
			}
			rows = append(rows, row)
		}
		for _, line := range alignRows(rows, "  ") {
			p.linef("%s", line)
		}
	}
	if line := d.hiddenLine(); line != "" {
		p.linef("  %s", line)
	}
	if probs := d.problems(); len(probs) > 0 {
		p.linef("Problems")
		rows := make([][]string, 0, len(probs))
		for _, pb := range probs {
			rows = append(rows, []string{pb.where, pb.check.Name, string(pb.check.Status), oneLineText(pb.check.Detail)})
		}
		for _, line := range alignRows(rows, "  ") {
			p.linef("%s", line)
		}
	}
	if len(d.Warnings) > 0 {
		p.linef("Warnings")
		for _, wn := range d.Warnings {
			p.linef("  %s", oneLineText(wn))
		}
	}
	p.linef("Summary  %s", d.summaryLine())
	return p.err
}

// doctorCutNote ends a Markdown report cut at 1 MiB.
const doctorCutNote = "\n*This report is cut at 1 MiB; the JSON report lists every check.*\n"

// WriteMarkdown renders the same content as WriteText in Markdown, for a
// CI artifact or step summary, at most 1 MiB (the rest is cut with a
// note): the header, the counts by status, the problems, the hub's and
// each provider's checks, and the matrix of targets in a collapsible
// section. Text from targets and platforms is escaped or put in code
// spans.
func (d *Doctor) WriteMarkdown(w io.Writer) error {
	return d.writeMarkdown(w, &mdWriter{limit: maxMarkdown, note: doctorCutNote})
}

// writeMarkdown renders the Markdown report into m and writes it to w.
func (d *Doctor) writeMarkdown(w io.Writer, m *mdWriter) error {
	title := "### touchmark doctor"
	if d.HubToken {
		title += " --hub-token"
	}
	m.line(title)
	m.line("")
	m.line(d.mdHeader())
	m.line("")
	m.line("| Status | Checks |")
	m.line("|---|---:|")
	for _, s := range Statuses {
		m.line(fmt.Sprintf("| %s | %d |", s, d.Summary[s]))
	}
	if probs := d.problems(); len(probs) > 0 {
		m.line("")
		m.line("#### Problems")
		m.line("")
		m.line("| Where | Check | Status | Detail |")
		m.line("|---|---|---|---|")
		for _, pb := range probs {
			m.line("| " + strings.Join([]string{mdCode(pb.where), mdText(pb.check.Name), string(pb.check.Status), mdText(pb.check.Detail)}, " | ") + " |")
		}
	}
	if line := d.hiddenLine(); line != "" {
		m.line("")
		m.line(mdText(line) + ".")
	}
	if len(d.HubChecks) > 0 {
		m.line("")
		m.line("#### Hub")
		m.line("")
		mdChecks(m, d.HubChecks)
	}
	for _, p := range d.Providers {
		m.line("")
		m.line("#### Provider " + mdCode(p.ID) + " (" + mdCode(p.Host) + ")")
		m.line("")
		if p.Writer != "" || p.Self != "" {
			m.line("Writer " + mdCodeOr(p.Writer, "none") + ", credential of " + mdCodeOr(p.Self, "?") + ".")
			m.line("")
		}
		if p.Error != "" {
			m.line("**Unavailable:** " + mdText(p.Error))
			m.line("")
		}
		if len(p.Missing) > 0 {
			m.line("Missing: " + mdText(listSome(p.Missing, examples)) + ".")
			m.line("")
		}
		mdChecks(m, p.Checks)
	}
	m.line("")
	m.line("#### Targets")
	m.line("")
	m.line(mdText(d.targetsLine()) + ".")
	if cols := d.columns(); len(cols) > 0 {
		m.line("")
		m.open("<details><summary>Every target</summary>")
		m.line("")
		head := "| Target |"
		sep := "|---|"
		for _, c := range cols {
			head += " " + mdText(c) + " |"
			sep += "---|"
		}
		m.line(head)
		m.line(sep)
		for _, t := range d.Targets {
			if t.hidden() || t.Skipped != "" {
				continue
			}
			row := "| " + mdCode(doctorRef(t)) + " |"
			for _, c := range cols {
				row += " " + cell(t.Checks, c) + " |"
			}
			m.line(row)
		}
		m.line("")
		m.close("</details>")
	}
	if len(d.Warnings) > 0 {
		m.line("")
		m.line("#### Warnings")
		m.line("")
		for _, wn := range d.Warnings {
			m.line("- " + mdText(wn))
		}
	}
	if _, err := io.WriteString(w, m.finish()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// mdHeader is the hub line of the Markdown output.
func (d *Doctor) mdHeader() string {
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
	parts := []string{hub}
	if d.Engine != "" {
		parts = append(parts, "touchmark "+mdText(d.Engine))
	}
	if d.Strict {
		parts = append(parts, "strict")
	}
	return strings.Join(parts, " · ")
}

// mdChecks writes a table of checks.
func mdChecks(m *mdWriter, cs []DoctorCheck) {
	if len(cs) == 0 {
		m.line("No checks.")
		return
	}
	m.line("| Check | Status | Detail |")
	m.line("|---|---|---|")
	for _, c := range cs {
		m.line("| " + mdText(c.Name) + " | " + string(c.Status) + " | " + mdText(c.Detail) + " |")
	}
}
