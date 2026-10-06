package report

import (
	"strings"
	"testing"
)

// sampleOperations are one entry of every kind, as a plan reports them,
// with a GitLab target, a target a public hub's report
// does not name, and a local run's flag.
func sampleOperations() []Operation {
	return []Operation{
		{Kind: OpRecreate, Target: "gh:acme/infra", Provider: "gh", PR: 9, Head: sampleCommit, Effect: EffectApplies,
			Detail: "head 4b1d9e0 matches: rebuilds touchmark/acme-eng on the default branch and drops what others added (commit 1a2b3c4d5e6f was added after touchmark's commit)"},
		{Kind: OpRecreate, Target: "corp:platform/x", Provider: "corp", Head: sampleCommit, Effect: EffectNone,
			Detail: "no sync branch is at 4b1d9e0 (touchmark/acme-eng is at 77aa001): it does nothing; remove it"},
		{Kind: OpRecreate, Effect: EffectUnknown, Detail: "names a target that is not public, which a public hub's report does not name"},
		{Kind: OpForgetDeclines, Target: "corp:platform/y", Provider: "corp", PR: 44, Effect: EffectApplies,
			Detail: "revokes the decline of !44: its content may be proposed again"},
		{Kind: OpAllowMassClose, Max: 400, Until: "2026-10-01", Effect: EffectExpired, Detail: "its date has passed: it does nothing; remove it"},
		{Kind: OpAdoptUnmarked, Until: "2026-10-15", Effect: EffectApplies, Detail: "adopts 1 unmarked pull request on branch aliases: corp:platform/z !14"},
		{Kind: OpAllowMassClose, Flag: true, Max: 10, Until: "2099-12-31", Effect: EffectNone, Detail: "not needed: this run closes 0 of touchmark's 3 open pull requests, within the guard's limit of 5"},
	}
}

// The Operations section of the text and Markdown outputs: one line per
// entry, aligned, after the outcome groups and before the paths; the
// pull request sign of the provider; no name for a hidden target; the
// JSON passes the schema.
func TestWriteOperations(t *testing.T) {
	d := sampleDelivery()
	d.Paths = []PathChange{{Action: "update", Path: ".claude/settings.json", Sensitive: true, Targets: 15}}
	d.Operations = sampleOperations()
	if err := validate(compileReportSchema(t), d); err != nil {
		t.Fatalf("the schema refuses the operations: %v", err)
	}

	var b strings.Builder
	if err := d.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	want := strings.Join([]string{
		"Operations",
		"  recreate                 gh:acme/infra #9          head 4b1d9e0 matches: rebuilds touchmark/acme-eng on the default branch and drops what others added (commit 1a2b3c4d5e6f was added after touchmark's commit)",
		"  recreate                 corp:platform/x           no sync branch is at 4b1d9e0 (touchmark/acme-eng is at 77aa001): it does nothing; remove it",
		"  recreate                 a target not named here   names a target that is not public, which a public hub's report does not name",
		"  forget_declines          corp:platform/y !44       revokes the decline of !44: its content may be proposed again",
		"  allow_mass_close         max 400 until 2026-10-01  its date has passed: it does nothing; remove it",
		"  adopt_unmarked           until 2026-10-15          adopts 1 unmarked pull request on branch aliases: corp:platform/z !14",
		"  allow_mass_close (flag)  max 10 until 2099-12-31   not needed: this run closes 0 of touchmark's 3 open pull requests, within the guard's limit of 5",
		"",
		"This hub pull request changes, across the targets it affects",
	}, "\n")
	if !strings.Contains(out, want) {
		t.Errorf("text output lacks\n%s\ngot\n%s", want, out)
	}
	if strings.Index(out, "\nOperations\n") < strings.Index(out, "  open ") {
		t.Errorf("Operations come before the outcome groups:\n%s", out)
	}

	b.Reset()
	if err := d.WriteMarkdown(&b); err != nil {
		t.Fatal(err)
	}
	md := b.String()
	for _, line := range []string{
		"#### Operations\n\n| Operation | Entry | Effect |\n|---|---|---|\n",
		"| recreate | `gh:acme/infra #9` | head 4b1d9e0 matches",
		"| forget\\_declines | `corp:platform/y !44` | revokes the decline of \\!44",
		"| allow\\_mass\\_close \\(flag\\) | `max 10 until 2099-12-31` |",
	} {
		if !strings.Contains(md, line) {
			t.Errorf("Markdown lacks %q:\n%s", line, md)
		}
	}
	if strings.Index(md, "#### Operations") > strings.Index(md, "This hub pull request changes") {
		t.Errorf("Markdown lists the paths before the operations:\n%s", md)
	}

	// No operations, no section.
	d.Operations = nil
	b.Reset()
	if err := d.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "Operations") {
		t.Errorf("a report without operations has the section:\n%s", b.String())
	}
}

// The schema keeps operations to their kinds and effects.
func TestOperationsSchema(t *testing.T) {
	sch := compileReportSchema(t)
	for name, edit := range map[string]func(*Operation){
		"kind":   func(o *Operation) { o.Kind = "merge" },
		"effect": func(o *Operation) { o.Effect = "maybe" },
		"detail": func(o *Operation) { o.Detail = "" },
		"head":   func(o *Operation) { o.Head = "4b1d9e0" },
		"until":  func(o *Operation) { o.Until = "soon" },
	} {
		d := sampleDelivery()
		op := sampleOperations()[0]
		edit(&op)
		d.Operations = []Operation{op}
		if validate(sch, d) == nil {
			t.Errorf("%s: the schema takes %+v", name, op)
		}
	}
}
