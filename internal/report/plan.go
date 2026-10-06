package report

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// What a plan adds to the delivery report: the scope of a
// plan in a hub pull request, --assume-opt-in, and the estimate of the
// writes and their time per provider.

// Scope modes of a plan.
const (
	// ScopeAll processes every target: --all, a run that builds no hub pull
	// request, or a pull request that changes hub.yml, targets.yml,
	// operations.yml or another file every target depends on.
	ScopeAll = "all"
	// ScopePacks processes the targets whose final pack list holds a pack
	// the pull request changes.
	ScopePacks = "packs"
	// ScopeHub processes no target: the pull request changes nothing a
	// target receives.
	ScopeHub = "hub"
)

// Scope is what part of the fleet a plan processed in full. The resolve
// and the opt-in files cover every target; the targets outside the scope
// are only counted, never listed: they are neither closed, nor dropped,
// nor changed by the pull request.
type Scope struct {
	// Mode is ScopeAll, ScopePacks or ScopeHub.
	Mode string `json:"mode"`
	// Reason says what decided the mode: "--all", the files that make every
	// target depend on the pull request, or why the scope could not be
	// computed.
	Reason string `json:"reason,omitempty"`
	// Packs are the packs the pull request changes (ScopePacks), with the
	// current name of a pack whose former name changed.
	Packs []string `json:"packs,omitempty"`
	// Base is the commit the hub commit was compared with: its merge base
	// with the hub's default branch.
	Base string `json:"base,omitempty"`
	// Processed is how many of the Total targets the resolve found the plan
	// processed.
	Processed int `json:"processed"`
	Total     int `json:"total"`
}

// Estimate is what a plan or a dry run expects distribute's writes to
// take, per provider: the writes and the time the provider's write limits
// give them, and how many runs the rollout needs under
// limits.max_new_prs_per_run.
type Estimate struct {
	// NewPRs is how many pull requests the rollout opens, in this run and
	// the next ones (the targets deferred:rollout-limit); MaxNewPRs is
	// limits.max_new_prs_per_run.
	NewPRs    int `json:"new_prs"`
	MaxNewPRs int `json:"max_new_prs_per_run"`
	// Runs is how many runs distribute needs to open them all: 0 when
	// nothing is written, or when MaxNewPRs is 0 and some would open.
	Runs int `json:"runs"`
	// Providers are the estimates by provider id.
	Providers map[string]ProviderEstimate `json:"providers"`
}

// ProviderEstimate is one provider's writes (HTTP writes of the API and git
// pushes, as the throttle counts them) and the time its limits give them.
type ProviderEstimate struct {
	// Writes are this run's writes by the git path (every push one write:
	// no signature needed, or the provider's signing key makes it), and
	// Seconds the time the provider's write limits take for them.
	Writes  int   `json:"writes"`
	Seconds int64 `json:"seconds"`
	// APIWrites and APISeconds are the same when every push that needs or
	// may need a signature goes through the platform's API commit (three
	// writes each); absent when that changes nothing.
	APIWrites  int   `json:"api_writes,omitempty"`
	APISeconds int64 `json:"api_seconds,omitempty"`
	// TotalWrites and TotalSeconds are the writes of every run the rollout
	// needs, by the git path; absent when it fits one run.
	TotalWrites  int   `json:"total_writes,omitempty"`
	TotalSeconds int64 `json:"total_seconds,omitempty"`
}

// planLines are the lines the text output shows after the providers: a
// plan's scope and --assume-opt-in, and how many targets targets.yml
// subscribes without an opt-in file (opt_in: assumed).
func (d *Delivery) planLines() []string {
	var out []string
	if s := d.Scope; s != nil {
		out = append(out, "scope: "+scopeText(s))
	}
	if d.Assumed {
		out = append(out, "--assume-opt-in: "+d.assumeText())
	}
	if text := d.subscribedText(); text != "" {
		out = append(out, "opt_in: assumed: "+text)
	}
	return out
}

// subscribedText says how many targets without an opt-in file targets.yml
// subscribes (AssumedByHub); "" for none.
func (d *Delivery) subscribedText() string {
	n := 0
	for _, t := range d.Targets {
		if t.Assumed && t.AssumedBy == AssumedByHub {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%s without an opt-in file %s opted in by targets.yml; an opt-in file with enabled: false opts one out",
		plural(n, "target"), pluralVerb(n))
}

// scopeText describes a plan's scope in one line.
func scopeText(s *Scope) string {
	more := " (--all for every target)"
	switch s.Mode {
	case ScopePacks:
		return fmt.Sprintf("%s %s changed → %d of %s%s", pluralWord(len(s.Packs), "pack"), strings.Join(s.Packs, ", "),
			s.Processed, plural(s.Total, "target"), more)
	case ScopeHub:
		return fmt.Sprintf("only hub files changed, which no target receives → none of %s planned%s", plural(s.Total, "target"), more)
	}
	text := "every target (" + strconv.Itoa(s.Total) + ")"
	if s.Reason != "" {
		text += ": " + s.Reason
	}
	return text
}

// pluralWord is word, with an "s" unless n is 1.
func pluralWord(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// assumeText describes --assume-opt-in: for this report only, a target
// without an opt-in file (or with one that is not a regular file or is too
// large) counts as opted in; one whose file says enabled: false does not.
func (d *Delivery) assumeText() string {
	n := 0
	for _, t := range d.Targets {
		if t.Assumed && t.AssumedBy != AssumedByHub {
			n++
		}
	}
	return fmt.Sprintf("for this report only, a target without an opt-in file counts as opted in (one whose file says enabled: false stays opted out); %s %s planned as if %s had an empty one",
		plural(n, "target"), pluralVerb(n), pluralPronoun(n))
}

func pluralVerb(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func pluralPronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}

// estimateLine is the cost line of a report with an Estimate: per provider,
// in the order of Providers, the writes and their time by the git path,
// through the API when signatures may be needed, and over every run of a
// rollout that takes several; then the number of runs.
func (d *Delivery) estimateLine() string {
	e := d.Estimate
	ids := make([]string, 0, len(e.Providers))
	for _, p := range d.Providers {
		if _, ok := e.Providers[p.ID]; ok && !slices.Contains(ids, p.ID) {
			ids = append(ids, p.ID)
		}
	}
	var rest []string
	for id := range e.Providers {
		if !slices.Contains(ids, id) {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	ids = append(ids, rest...)
	parts := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		pe := e.Providers[id]
		s := fmt.Sprintf("%s ≈ %s (%s)", id, plural(pe.Writes, "write"), approxDuration(pe.Seconds))
		if pe.APIWrites > 0 {
			s += fmt.Sprintf(", ≈ %d (%s) if commits must be signed through the API", pe.APIWrites, approxDuration(pe.APISeconds))
		}
		if pe.TotalWrites > 0 {
			s += fmt.Sprintf(", ≈ %d (%s) over all runs", pe.TotalWrites, approxDuration(pe.TotalSeconds))
		}
		parts = append(parts, s)
	}
	switch {
	case e.Runs > 1:
		parts = append(parts, fmt.Sprintf("%d runs (%d new pull requests, max_new_prs_per_run %d)", e.Runs, e.NewPRs, e.MaxNewPRs))
	case e.Runs == 1:
		parts = append(parts, "1 run")
	case e.NewPRs > 0:
		parts = append(parts, fmt.Sprintf("%s wait: max_new_prs_per_run is 0", plural(e.NewPRs, "new pull request")))
	}
	return strings.Join(parts, " · ")
}

// approxDuration renders a time estimate: "<1 min", "~7 min", "~4 h 3 min".
func approxDuration(seconds int64) string {
	d := (time.Duration(seconds)*time.Second + 30*time.Second).Truncate(time.Minute)
	switch {
	case d < time.Minute:
		return "<1 min"
	case d < time.Hour:
		return fmt.Sprintf("~%d min", int(d/time.Minute))
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if m == 0 {
		return fmt.Sprintf("~%d h", h)
	}
	return fmt.Sprintf("~%d h %d min", h, m)
}

// mdPlanLines writes a plan's scope and --assume-opt-in, and the targets
// targets.yml subscribes, to the Markdown output.
func (d *Delivery) mdPlanLines(m *mdWriter) {
	if s := d.Scope; s != nil {
		m.line("")
		m.line("**Scope:** " + mdText(scopeText(s)))
	}
	if d.Assumed {
		m.line("")
		m.line("**" + mdText("--assume-opt-in") + ":** " + mdText(d.assumeText()))
	}
	if text := d.subscribedText(); text != "" {
		m.line("")
		m.line("**" + mdText("opt_in: assumed") + ":** " + mdText(text))
	}
}

// Comment limits (plan --comment keeps one comment in the hub pull
// request).
const (
	// MaxComment is the most a pull request comment holds on GitHub (65 536
	// characters); the other platforms take at least as much.
	MaxComment = 65536
	// commentCut ends a comment that did not fit.
	commentCut = "\n*This comment is cut to fit; the JSON report of the job lists every target.*\n"
)

// WriteComment renders the Markdown report as the body of plan's comment in
// a hub pull request: WriteMarkdown's content, with its first line (the
// title) kept, cut to fit limit bytes (MaxComment when 0) with the closing
// line appended after it. closing is Markdown the caller made safe, such as
// the comment's hidden marker; it counts against the limit.
func (d *Delivery) WriteComment(w io.Writer, limit int, closing string) error {
	if limit <= 0 {
		limit = MaxComment
	}
	m := &mdWriter{limit: limit - len(closing), note: commentCut}
	d.markdown(m)
	_, err := io.WriteString(w, m.finish()+closing)
	if err != nil {
		return fmt.Errorf("write comment: %w", err)
	}
	return nil
}

// PathChange is one path the pushes of a plan or a dry run change, across
// the targets: what a hub pull request changes, across the targets it
// affects.
type PathChange struct {
	// Action is "add", "update", "delete" or "chmod".
	Action string `json:"action"`
	Path   string `json:"path"`
	// Sensitive is set for a path of the sensitive patterns (the built-in
	// ones and hub.yml's sensitive_paths) or written executable.
	Sensitive bool `json:"sensitive,omitempty"`
	// Targets is how many targets the change goes to.
	Targets int `json:"targets"`
}

// maxPathLines is how many paths the text and Markdown outputs list.
const maxPathLines = 20

// pathsTitle heads the paths section.
func (d *Delivery) pathsTitle() string {
	if d.Hub.PR > 0 {
		return "This hub pull request changes, across the targets it affects"
	}
	return "Changes across the targets"
}

// writeTextPaths writes the paths section of the text output: one line per
// path, sensitive ones first, at most maxPathLines.
func (d *Delivery) writeTextPaths(p *printer) {
	if len(d.Paths) == 0 {
		return
	}
	p.linef("%s", d.pathsTitle())
	shown := d.Paths[:min(len(d.Paths), maxPathLines)]
	// Sensitive paths come first: the flag column is there when the first
	// one is.
	flagged := shown[0].Sensitive
	rows := make([][]string, 0, len(shown))
	for _, c := range shown {
		row := []string{c.Action, c.Path}
		switch {
		case c.Sensitive:
			row = append(row, "SENSITIVE")
		case flagged:
			row = append(row, "")
		}
		rows = append(rows, append(row, plural(c.Targets, "target")))
	}
	for _, line := range alignRows(rows, "  ") {
		p.linef("%s", line)
	}
	if n := len(d.Paths) - maxPathLines; n > 0 {
		p.linef("  and %s more", plural(n, "path"))
	}
	p.linef("")
}

// mdPaths writes the paths section of the Markdown output as a table.
func (d *Delivery) mdPaths(m *mdWriter) {
	if len(d.Paths) == 0 {
		return
	}
	m.line("")
	m.open(fmt.Sprintf("<details><summary>%s (%d)</summary>", mdText(d.pathsTitle()), len(d.Paths)))
	m.line("")
	m.line("| Change | Path | Targets |")
	m.line("|---|---|---:|")
	for _, c := range d.Paths[:min(len(d.Paths), maxPathLines)] {
		action := mdText(c.Action)
		if c.Sensitive {
			action += " ⚠ sensitive"
		}
		// A pipe ends a table cell even in a code span unless escaped.
		m.line(fmt.Sprintf("| %s | %s | %d |", action, strings.ReplaceAll(mdCode(c.Path), "|", `\|`), c.Targets))
	}
	if n := len(d.Paths) - maxPathLines; n > 0 {
		m.line("")
		m.line(fmt.Sprintf("and %s more; the JSON report lists them all", plural(n, "path")))
	}
	m.line("")
	m.close("</details>")
}
