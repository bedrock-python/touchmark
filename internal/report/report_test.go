package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/apply"
	"github.com/bedrock-python/touchmark/internal/decide"
)

// applied is an apply report with one entry per outcome.
func applied() *Sync {
	entries := []Entry{
		{Path: "AGENTS.md", State: "missing", Action: "create", Pack: "base"},
		{Path: "docs/guide.md", State: "outdated", Action: "update", Pack: "base"},
		{Path: "old.md", State: "retired", Action: "delete", Pack: "base", Detail: "no longer shipped by pack base"},
		{Path: "my notes.md", State: "local", Action: "keep", Pack: "base"},
		{Path: "README.md", State: "current", Action: "keep", Pack: "base"},
	}
	results := []Result{
		NewResult(apply.Result{Entry: decide.Entry{Path: "old.md", Action: decide.Delete, State: decide.Retired}, Done: true}),
		NewResult(apply.Result{Entry: decide.Entry{Path: "AGENTS.md", Action: decide.Create, State: decide.Missing}, Err: errors.New("create AGENTS.md: disk full")}),
		NewResult(apply.Result{Entry: decide.Entry{Path: "docs/guide.md", Action: decide.Update, State: decide.Outdated}, Skipped: "changed since observed: the content differs"}),
	}
	return &Sync{
		Command:   "apply",
		Hub:       Hub{Dir: "/src/hub", ID: "acme-eng", Commit: "0123456789abcdef0123456789abcdef01234567"},
		Target:    Target{Root: "/src/svc", Ref: "gh:acme/svc", OptInFile: ".engineering-assets.yml", OptedIn: true},
		Selection: &Selection{Packs: []string{"base"}, Complete: true, Sources: map[string][]string{"base": {"defaults", "targets.yml"}}},
		Entries:   entries,
		Results:   results,
		Summary:   Summarize(entries, results),
		Warnings:  []string{"hub.yml: no version, assuming 1"},
	}
}

func TestWriteTextApply(t *testing.T) {
	var buf bytes.Buffer
	if err := applied().WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := `touchmark apply · hub acme-eng @ 0123456 · target gh:acme/svc
packs: base (defaults + targets.yml)
warning: hub.yml: no version, assuming 1

  failed   create  missing   AGENTS.md      base  create AGENTS.md: disk full
  skipped  update  outdated  docs/guide.md  base  changed since observed: the content differs
  done     delete  retired   old.md         base  no longer shipped by pack base
           keep    local     my notes.md    base  run touchmark apply --adopt 'my notes.md' to take it back, or add it to ignore

current 1 · missing 1 · outdated 1 · local 1 · retired 1
applied 1 of 3 changes · skipped 1 · failed 1
`
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSummarize(t *testing.T) {
	s := applied().Summary
	want := Summary{Missing: 1, Current: 1, Outdated: 1, Local: 1, Retired: 1, Changes: 3, Done: 1, Skipped: 1, Failed: 1}
	if s != want {
		t.Errorf("Summarize = %+v, want %+v", s, want)
	}
}

// Results is omitted for status and always present for apply; the other
// fields are always present.
func TestJSONFields(t *testing.T) {
	status := &Sync{Command: "status"}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, status); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["results"]; ok {
		t.Error("status report has results")
	}
	for _, k := range []string{"command", "dry_run", "hub", "target", "selection", "entries", "summary", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status report lacks %q", k)
		}
	}
	status.Results = []Result{}
	buf.Reset()
	if err := WriteJSON(&buf, status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"results": []`) {
		t.Errorf("apply report without changes lacks an empty results list:\n%s", buf.String())
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"AGENTS.md":          "AGENTS.md",
		".github/x_y-z+1.md": ".github/x_y-z+1.md",
		"my notes.md":        "'my notes.md'",
		"it's.md":            `'it'\''s.md'`,
		"$HOME.md":           "'$HOME.md'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
