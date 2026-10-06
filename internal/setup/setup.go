// Package setup prepares a hub's platform for touchmark: `touchmark setup
// github` and `touchmark setup gitlab` (see docs/guide/setup.md). It runs
// locally, with a maintainer's token of the hub, and sets up
// only what keeps the write key isolated (security.write_isolation platform
// or external):
//
//   - GitLab, fully through the API: the reader and the writer (service
//     accounts where the instance has them, else group access tokens) and
//     their roles on the targets' group, the hub's protected, masked and
//     hidden variables (the write key scoped to the environment
//     touchmark-distribute), the environment, the protected default
//     branch that no one pushes to, "Minimum role to use pipeline
//     variables" no_one_allowed, protected variables kept out of merge
//     request pipelines, and the two pipeline schedules (gitlab.go);
//   - GitHub: the reader and writer Apps through the App manifest flow,
//     whose one browser step is unavoidable (manifest.go), the environment
//     touchmark-distribute that only the default branch may use, the App
//     ids as Actions variables, the hub's ruleset where the token may
//     create it; the private keys go to files of a private directory and
//     the `gh secret set` commands that store them are printed: encrypting
//     a secret needs a libsodium sealed box, which touchmark does not
//     implement (github.go).
//
// Every step reads first and writes only what differs, so a second run
// writes nothing; --dry-run only reads. A token setup mints goes straight
// into the hub's variable: it is registered with the run's redaction before
// anything else happens to it, and never printed. Each run ends with the
// checks of `touchmark doctor --hub-token` on where the hub keeps its keys.
//
// Gitea and Forgejo are not offered: their Actions give every branch the
// secrets, so no setup can isolate the write key there.
package setup

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/report"
)

// Schema is the schema id of setup reports (schemas/setup.schema.json).
const Schema = "setup/v1"

// Status grades one step of setup.
type Status string

// Step statuses, in report order.
const (
	// StatusOK: the step found what it wants; nothing written.
	StatusOK Status = "ok"
	// StatusDone: the step changed the platform in this run.
	StatusDone Status = "done"
	// StatusWould: --dry-run; the step would change the platform.
	StatusWould Status = "would"
	// StatusWarn: as wanted, with a caveat to read.
	StatusWarn Status = "warn"
	// StatusManual: left to the person, with instructions (and commands).
	StatusManual Status = "manual"
	// StatusUnknown: could not be read.
	StatusUnknown Status = "unknown"
	// StatusFail: the step failed, or found what setup must not fix.
	StatusFail Status = "fail"
)

// Statuses lists every status in report order.
var Statuses = []Status{StatusOK, StatusDone, StatusWould, StatusWarn, StatusManual, StatusUnknown, StatusFail}

// The environment that alone may use the write key.
const Environment = "touchmark-distribute"

// Step is one step of setup and what came of it.
type Step struct {
	Name   string `json:"step"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Commands are shell commands the person runs to finish the step
	// (GitHub's secrets), in order.
	Commands []string `json:"commands,omitempty"`
}

// Report is what setup did (setup/v1).
type Report struct {
	Schema   string `json:"schema"`
	Command  string `json:"command"` // setup
	Engine   string `json:"engine"`
	Platform string `json:"platform"` // github or gitlab
	// Host is the platform's host (with a non-default port).
	Host string `json:"host"`
	// Hub is the hub repository's path, HubID its id ("" when unknown).
	Hub   string `json:"hub"`
	HubID string `json:"hub_id,omitempty"`
	// Group is GitLab's group of targets; Org GitHub's owner of the Apps.
	Group string `json:"group,omitempty"`
	Org   string `json:"org,omitempty"`
	// Isolation is security.write_isolation: platform or external.
	Isolation string `json:"isolation"`
	DryRun    bool   `json:"dry_run"`
	// Accounts is what the reader and the writer are: "service-account" or
	// "group-access-token" (GitLab), "app" (GitHub); "" when unknown yet.
	Accounts string `json:"accounts,omitempty"`
	// Reader and Writer are the accounts' logins, "" when unknown (a dry
	// run of a group access token, an App not created yet).
	Reader string `json:"reader,omitempty"`
	Writer string `json:"writer,omitempty"`
	Steps  []Step `json:"steps"`
	// Next lists what the person does after this run.
	Next    []string       `json:"next"`
	Summary map[Status]int `json:"summary"`
}

// newReport starts a report.
func newReport(platform, engine string, dryRun bool) *Report {
	return &Report{Schema: Schema, Command: "setup", Engine: engine, Platform: platform, DryRun: dryRun,
		Steps: []Step{}, Next: []string{}, Summary: map[Status]int{}}
}

// add appends a step.
func (r *Report) add(name string, status Status, format string, args ...any) *Step {
	r.Steps = append(r.Steps, Step{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
	return &r.Steps[len(r.Steps)-1]
}

// next appends a line to Next once.
func (r *Report) next(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	for _, l := range r.Next {
		if l == line {
			return
		}
	}
	r.Next = append(r.Next, line)
}

// Summarize counts the steps per status, every status present.
func (r *Report) Summarize() {
	r.Summary = map[Status]int{}
	for _, s := range Statuses {
		r.Summary[s] = 0
	}
	for _, s := range r.Steps {
		r.Summary[s.Status]++
	}
	if r.Steps == nil {
		r.Steps = []Step{}
	}
	if r.Next == nil {
		r.Next = []string{}
	}
}

// Exit codes of a report.
const (
	ExitOK      = 0
	ExitFailed  = 1
	ExitPending = 3
)

// ExitCode is 1 when a step failed, 3 when one is left to the person or
// could not be read (run setup again once it is done), 0 otherwise.
func (r *Report) ExitCode() int {
	pending := false
	for _, s := range r.Steps {
		switch s.Status {
		case StatusFail:
			return ExitFailed
		case StatusManual, StatusUnknown:
			pending = true
		}
	}
	if pending {
		return ExitPending
	}
	return ExitOK
}

// WriteJSON writes the report as JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	r.Summarize()
	return report.WriteJSON(w, r)
}

// WriteText writes the report for people: a header, one line per step
// (commands indented below it), the next steps and the summary.
func (r *Report) WriteText(w io.Writer) error {
	r.Summarize()
	var b strings.Builder
	head := "touchmark setup " + r.Platform
	if r.Hub != "" {
		head += " · hub " + r.Hub
		if r.HubID != "" {
			head += " (" + r.Host + "/" + r.HubID + ")"
		}
	}
	switch {
	case r.Group != "":
		head += " · targets in group " + r.Group
	case r.Org != "":
		head += " · Apps of " + r.Org
	}
	head += " · isolation " + r.Isolation
	if r.DryRun {
		head += " · dry run"
	}
	b.WriteString(head + "\n")
	width := 0
	for _, s := range r.Steps {
		width = max(width, len(s.Name))
	}
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "  %-7s  %-*s  %s\n", s.Status, width, s.Name, oneLine(s.Detail))
		for _, c := range s.Commands {
			fmt.Fprintf(&b, "      %s\n", c)
		}
	}
	if len(r.Next) > 0 {
		b.WriteString("Next\n")
		for _, n := range r.Next {
			fmt.Fprintf(&b, "  - %s\n", oneLine(n))
		}
	}
	var counts []string
	for _, s := range Statuses {
		if n := r.Summary[s]; n > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", s, n))
		}
	}
	b.WriteString(strings.Join(counts, " · ") + "\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// oneLine joins the lines of s with "; " and strips control characters,
// so that a platform's message cannot forge lines of the report.
func oneLine(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, line))
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(line)
	}
	return b.String()
}

// PreconditionError is a refusal before anything was written: the token,
// the hub or the group is not what setup needs. The command line exits
// with code 2 on it.
type PreconditionError struct{ Err error }

func (e *PreconditionError) Error() string { return e.Err.Error() }
func (e *PreconditionError) Unwrap() error { return e.Err }

// precondition returns a PreconditionError.
func precondition(format string, args ...any) error {
	return &PreconditionError{Err: fmt.Errorf(format, args...)}
}

// IsPrecondition reports whether err is a PreconditionError.
func IsPrecondition(err error) bool {
	var pe *PreconditionError
	return errors.As(err, &pe)
}

// dateAfter returns the date days after now, as GitLab takes expiry dates
// (YYYY-MM-DD, UTC).
func dateAfter(now time.Time, days int) string {
	return now.UTC().AddDate(0, 0, days).Format("2006-01-02")
}
