// Package report is the result model of the local commands (check, status,
// apply) and renders it as text or JSON.
//
// JSON field names are snake_case and stable: every field is always present,
// except "results", which only apply prints.
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/bedrock-python/touchmark/internal/apply"
	"github.com/bedrock-python/touchmark/internal/decide"
)

// Outcomes of an apply result.
const (
	Done    = "done"
	Skipped = "skipped"
	Failed  = "failed"
)

// Hub identifies the hub a report was made from.
type Hub struct {
	Dir string `json:"dir"`
	// ID is hub.yml's id; "" for a legacy hub without hub.yml.
	ID     string `json:"id"`
	Commit string `json:"commit"`
}

// Target identifies the target working tree.
type Target struct {
	Root string `json:"root"`
	// Ref is the target in targets.yml ("path" or "provider:path"), or ""
	// when it could not be determined.
	Ref       string `json:"ref"`
	OptInFile string `json:"opt_in_file"`
	OptedIn   bool   `json:"opted_in"`
}

// Selection is the resolved pack list.
type Selection struct {
	Packs      []string            `json:"packs"`
	Complete   bool                `json:"complete"`
	Unresolved []string            `json:"unresolved"`
	Sources    map[string][]string `json:"sources"`
}

// Entry is one decision of the plan.
type Entry struct {
	Path         string `json:"path"`
	State        string `json:"state"`
	Action       string `json:"action"`
	Pack         string `json:"pack"`
	From         string `json:"from"`
	To           string `json:"to"`
	Mode         string `json:"mode"`
	Detail       string `json:"detail"`
	AfterDeletes bool   `json:"after_deletes"`
}

// NewEntry converts a plan entry.
func NewEntry(e decide.Entry) Entry {
	return Entry{
		Path:         e.Path,
		State:        string(e.State),
		Action:       string(e.Action),
		Pack:         e.Pack,
		From:         e.From,
		To:           e.To,
		Mode:         e.Mode,
		Detail:       e.Detail,
		AfterDeletes: e.AfterDeletes,
	}
}

// Result is what apply did with one entry whose action is not keep.
type Result struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	State   string `json:"state"`
	Outcome string `json:"outcome"` // done | skipped | failed
	// Detail is the reason for skipped and the error for failed.
	Detail string `json:"detail"`
}

// NewResult converts an apply result.
func NewResult(r apply.Result) Result {
	out := Result{Path: r.Entry.Path, Action: string(r.Entry.Action), State: string(r.Entry.State)}
	switch {
	case r.Err != nil:
		out.Outcome, out.Detail = Failed, r.Err.Error()
	case r.Skipped != "":
		out.Outcome, out.Detail = Skipped, r.Skipped
	default:
		out.Outcome = Done
	}
	return out
}

// Summary counts entries per state, the changes (entries whose action is
// not keep) and, for apply, the outcomes.
type Summary struct {
	Missing      int `json:"missing"`
	Current      int `json:"current"`
	Outdated     int `json:"outdated"`
	Local        int `json:"local"`
	Ignored      int `json:"ignored"`
	Retired      int `json:"retired"`
	RetiredLocal int `json:"retired_local"`
	Unsafe       int `json:"unsafe"`
	Orphaned     int `json:"orphaned"`
	Changes      int `json:"changes"`
	Done         int `json:"done"`
	Skipped      int `json:"skipped"`
	Failed       int `json:"failed"`
}

// stateCounts returns the counts in display order, paired with state names.
func (s Summary) stateCounts() []stateCount {
	return []stateCount{
		{decide.Current, s.Current},
		{decide.Missing, s.Missing},
		{decide.Outdated, s.Outdated},
		{decide.Local, s.Local},
		{decide.Ignored, s.Ignored},
		{decide.Retired, s.Retired},
		{decide.RetiredLocal, s.RetiredLocal},
		{decide.Unsafe, s.Unsafe},
		{decide.Orphaned, s.Orphaned},
	}
}

type stateCount struct {
	state decide.State
	n     int
}

// Summarize counts the entries of a plan and the outcomes of results.
func Summarize(entries []Entry, results []Result) Summary {
	var s Summary
	counters := map[string]*int{
		string(decide.Missing):      &s.Missing,
		string(decide.Current):      &s.Current,
		string(decide.Outdated):     &s.Outdated,
		string(decide.Local):        &s.Local,
		string(decide.Ignored):      &s.Ignored,
		string(decide.Retired):      &s.Retired,
		string(decide.RetiredLocal): &s.RetiredLocal,
		string(decide.Unsafe):       &s.Unsafe,
		string(decide.Orphaned):     &s.Orphaned,
	}
	for _, e := range entries {
		if n, ok := counters[e.State]; ok {
			*n++
		}
		if e.Action != string(decide.Keep) {
			s.Changes++
		}
	}
	for _, r := range results {
		switch r.Outcome {
		case Done:
			s.Done++
		case Skipped:
			s.Skipped++
		case Failed:
			s.Failed++
		}
	}
	return s
}

// Sync is the report of status and apply.
type Sync struct {
	Command string `json:"command"` // "status" or "apply"
	DryRun  bool   `json:"dry_run"`
	Hub     Hub    `json:"hub"`
	Target  Target `json:"target"`
	// Selection is nil when the target has not opted in.
	Selection *Selection `json:"selection"`
	// Entries is the whole plan, current entries included, in plan order.
	Entries []Entry `json:"entries"`
	// Results is set by apply (not --dry-run) only: one per entry whose
	// action is not keep, in execution order.
	Results  []Result `json:"results,omitzero"`
	Summary  Summary  `json:"summary"`
	Warnings []string `json:"warnings"`
}

// Check is the report of check.
type Check struct {
	Command  string   `json:"command"` // "check"
	Hub      Hub      `json:"hub"`
	Packs    []string `json:"packs"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// WriteJSON writes v as JSON indented by two spaces, followed by a newline.
// '<', '>' and '&' are not escaped.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
