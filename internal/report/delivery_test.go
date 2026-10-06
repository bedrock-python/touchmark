package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/bedrock-python/touchmark/schemas"
)

const (
	sampleCommit = "3f2a1c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b"
	sampleKey    = "sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58"
	otherKey     = "sha256:0f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a4938"
)

// sampleDelivery is a plan over two providers with a target of most
// outcomes, in no particular order.
func sampleDelivery() *Delivery {
	d := NewDelivery("plan", "0.2.0")
	d.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/712345678", Commit: sampleCommit, PR: 41}
	d.Providers = []ProviderInfo{
		{ID: "gh", Type: "github", Host: "github.com", Reader: "acme-read[bot]", Writer: "acme-write[bot]", WriteCheck: "not checked", ResolveComplete: true},
		{ID: "corp", Type: "gitlab", Host: "gitlab.example.com", Reader: "tm-reader", Writer: "tm-writer", WriteCheck: "not checked", ResolveComplete: false, Missing: []string{"corp:platform/gone"}},
	}
	changes := ChangeCounts{Create: 2, Update: 1}
	d.Targets = []DeliveryTarget{
		{Provider: "gh", Host: "github.com", RepoID: "11", Path: "acme/sdk", Outcome: OutcomeOpened, Key: sampleKey, Packs: []string{"base"}, Changes: changes, Writes: 3},
		{Provider: "gh", Host: "github.com", RepoID: "12", Path: "acme/billing", Outcome: OutcomeOpened, Key: sampleKey, Packs: []string{"base"}, Changes: changes, Writes: 3,
			Orphaned: []string{"docs/a.md", "docs/b.md", "docs/c.md", "docs/d.md"}},
		{Provider: "gh", Host: "github.com", RepoID: "13", Path: "acme/api", Outcome: OutcomeUpdated, Reason: "content",
			PR: &PRRef{Number: 87, URL: "https://github.com/acme/api/pull/87", State: "open"}, Key: otherKey, Packs: []string{"base"}, Changes: changes, Writes: 2},
		{Provider: "gh", Host: "github.com", RepoID: "14", Path: "Acme/Zeta", Outcome: OutcomeUnchanged,
			PR: &PRRef{Number: 3, URL: "https://github.com/Acme/Zeta/pull/3", State: "open"}, Key: sampleKey, Packs: []string{"base"}, Changes: changes},
		{Provider: "gh", Host: "github.com", RepoID: "15", Path: "acme/old", Outcome: OutcomeClosed, Reason: "no-diff",
			PR: &PRRef{Number: 12, URL: "https://github.com/acme/old/pull/12", State: "open"}, Packs: []string{"base"}, Writes: 3},
		{Provider: "gh", Host: "github.com", Outcome: OutcomeSkipped, Reason: ReasonPrivate},
		{Provider: "gh", Host: "github.com", RepoID: "16", Path: "acme/web", Outcome: OutcomeSkipped, Reason: "not-opted-in"},
		{Provider: "gh", Host: "github.com", RepoID: "17", Path: "acme/archive", Outcome: OutcomeSkipped, Reason: "archived"},
		{Provider: "gh", Host: "github.com", RepoID: "18", Path: "acme/cli", Outcome: OutcomeBlocked, Reason: "branch-in-use",
			PR: &PRRef{Number: 31, URL: "https://github.com/acme/cli/pull/31", State: "open"}, Key: sampleKey, Packs: []string{"base"}, Changes: changes},
		{Provider: "gh", Host: "github.com", RepoID: "19", Path: "acme/web2", Outcome: OutcomeDeferred, Reason: "rollout-limit", Key: sampleKey, Packs: []string{"base"}, Changes: changes},
		{Provider: "gh", Host: "github.com", RepoID: "20", Path: "acme/broken", Outcome: OutcomeFailed, Reason: "auth",
			Warnings: []string{"read the opt-in file: auth (HTTP 401): bad credentials"}},
		{Provider: "corp", Host: "gitlab.example.com", RepoID: "301", Path: "platform/api", Outcome: OutcomeUpdated, Reason: "content",
			PR: &PRRef{Number: 14, URL: "https://gitlab.example.com/platform/api/-/merge_requests/14", State: "open"}, Key: sampleKey, Packs: []string{"base"}, Changes: changes, Writes: 2},
	}
	d.Warnings = []string{"hub head not checked: local run"}
	d.Cost = map[string]int{"gh": 11, "corp": 2}
	d.Summarize()
	return d
}

func TestNewDelivery(t *testing.T) {
	d := NewDelivery("plan", "0.2.0")
	if d.Schema != DeliverySchema || d.Command != "plan" || d.Engine != "0.2.0" || d.Outcome != Completed {
		t.Errorf("NewDelivery = %+v", d)
	}
	if d.Providers == nil || d.Targets == nil || d.Ops == nil || d.Warnings == nil || d.Cost == nil || d.Notes == nil {
		t.Errorf("NewDelivery has nil lists or maps: %+v", d)
	}
	for _, o := range Outcomes {
		if n, ok := d.Summary[o]; !ok || n != 0 {
			t.Errorf("Summary[%s] = %d, %v; want 0, true", o, n, ok)
		}
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, d); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema", "command", "engine", "hub", "outcome", "strict", "providers", "targets", "ops", "warnings", "summary", "sweep", "cost"} {
		if _, ok := m[k]; !ok {
			t.Errorf("an empty report lacks %q", k)
		}
	}
	if _, ok := m["notes"]; ok {
		t.Error("an empty report has notes")
	}
}

func TestDeliverySummarize(t *testing.T) {
	d := sampleDelivery()
	want := map[Outcome]int{
		OutcomeOpened: 2, OutcomeUpdated: 2, OutcomeUnchanged: 1, OutcomeClosed: 1, OutcomeDeclined: 0,
		OutcomeSkipped: 3, OutcomeBlocked: 1, OutcomeDeferred: 1, OutcomeFailed: 1,
	}
	if !reflect.DeepEqual(d.Summary, want) {
		t.Errorf("Summary = %v, want %v", d.Summary, want)
	}
	var order []string
	for _, tg := range d.Targets {
		order = append(order, tg.Provider+":"+tg.Path)
	}
	wantOrder := []string{
		"corp:platform/api",
		"gh:", "gh:acme/api", "gh:acme/archive", "gh:acme/billing", "gh:acme/broken", "gh:acme/cli",
		"gh:acme/old", "gh:acme/sdk", "gh:acme/web", "gh:acme/web2", "gh:Acme/Zeta",
	}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("order =\n%q\nwant\n%q", order, wantOrder)
	}
	// Summarize again is a no-op.
	before := slices.Clone(d.Targets)
	d.Summarize()
	if !reflect.DeepEqual(before, d.Targets) {
		t.Error("a second Summarize reordered the targets")
	}
}

func TestExitCode(t *testing.T) {
	target := func(o Outcome, reason string) DeliveryTarget {
		return DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: "1", Path: "acme/x", Outcome: o, Reason: reason}
	}
	ok := ProviderInfo{ID: "gh", Type: "github", Host: "github.com", ResolveComplete: true}
	cases := []struct {
		name    string
		command string
		strict  bool
		edit    func(*Delivery)
		want    int
	}{
		{"empty", "plan", false, func(*Delivery) {}, 0},
		{"empty strict", "plan", true, func(*Delivery) {}, 0},
		{"opened", "plan", true, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeOpened, "")} }, 0},
		{"failed", "plan", false, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeFailed, "auth")} }, 1},
		{"failed beats strict", "plan", true, func(d *Delivery) {
			d.Targets = []DeliveryTarget{target(OutcomeBlocked, "edited"), target(OutcomeFailed, "git")}
		}, 1},
		{"provider error", "distribute", false, func(d *Delivery) { d.Providers = []ProviderInfo{{ID: "gh", Error: "probe: auth"}} }, 1},
		{"sweep failed", "distribute", false, func(d *Delivery) { d.Sweep.Failed = true }, 1},
		{"mass close", "distribute", false, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeBlocked, "mass-close")} }, 1},
		{"blocked", "plan", false, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeBlocked, "branch-in-use")} }, 0},
		{"blocked strict", "plan", true, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeBlocked, "branch-in-use")} }, 3},
		{"deferred strict", "distribute", true, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeDeferred, "rollout-limit")} }, 3},
		{"skipped strict", "plan", true, func(d *Delivery) { d.Targets = []DeliveryTarget{target(OutcomeSkipped, "archived")} }, 0},
		{"sweep off strict", "distribute", true, func(d *Delivery) { d.Sweep.Reason = "--only" }, 3},
		{"sweep off", "distribute", false, func(d *Delivery) { d.Sweep.Reason = "--only" }, 0},
		{"incomplete plan strict", "plan", true, func(d *Delivery) { d.Providers = []ProviderInfo{{ID: "gh"}} }, 3},
		{"incomplete plan", "plan", false, func(d *Delivery) { d.Providers = []ProviderInfo{{ID: "gh"}} }, 0},
		{"incomplete distribute strict", "distribute", true, func(d *Delivery) { d.Providers = []ProviderInfo{{ID: "gh"}} }, 0},
		{"missing target strict", "plan", true, func(d *Delivery) {
			p := ok
			p.Missing = []string{"gh:acme/gone"}
			d.Providers = []ProviderInfo{p}
		}, 3},
		{"complete strict", "plan", true, func(d *Delivery) { d.Providers = []ProviderInfo{ok} }, 0},
		{"superseded strict", "plan", true, func(d *Delivery) {
			d.Outcome = Superseded
			d.Providers = []ProviderInfo{{ID: "gh"}}
		}, 0},
	}
	for _, tc := range cases {
		d := NewDelivery(tc.command, "dev")
		d.Strict = tc.strict
		tc.edit(d)
		if got := d.ExitCode(); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleDelivery().WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := `touchmark plan · hub acme-eng (github.com/712345678) @ 3f2a1c9 (PR #41) · touchmark 0.2.0
gh    github.com          read acme-read[bot]  write acme-write[bot]: not checked  resolve complete
corp  gitlab.example.com  read tm-reader       write tm-writer: not checked        resolve incomplete, 1 target missing

  open       2  gh:acme/billing, gh:acme/sdk
  update     2  corp:platform/api !14 (content), gh:acme/api #87 (content)
  unchanged  1  gh:Acme/Zeta #3
  close      1  gh:acme/old #12 (no-diff)
  skipped    3  gh:acme/archive (archived), gh:acme/web (not-opted-in); 1 private in public hub
  blocked    1  gh:acme/cli #31 (branch-in-use)
  deferred   1  gh:acme/web2 (rollout-limit)
  failed     1  gh:acme/broken (auth)

Warnings
  hub head not checked: local run
  gh:acme/billing: orphaned 4: docs/a.md, docs/b.md, docs/c.md and 1 more
  gh:acme/broken: read the opt-in file: auth (HTTP 401): bad credentials
Cost  gh ≈ 11 writes · corp ≈ 2 writes
`
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A group names at most three targets; the rest are counted.
func TestWriteTextMore(t *testing.T) {
	d := NewDelivery("distribute", "dev")
	d.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/1", Commit: sampleCommit}
	d.Providers = []ProviderInfo{{ID: "gh", Type: "github", Host: "github.com", Reader: "r", Writer: "w", WriteCheck: "ok", ResolveComplete: true}}
	for i := range 5 {
		d.Targets = append(d.Targets, DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: fmt.Sprint(i + 1),
			Path: fmt.Sprintf("acme/r%d", i), Outcome: OutcomeOpened, PR: &PRRef{Number: int64(i + 1)}, Writes: 3})
	}
	for range 2 {
		d.Targets = append(d.Targets, DeliveryTarget{Provider: "gh", Host: "github.com", Outcome: OutcomeSkipped, Reason: ReasonPrivate})
	}
	d.Cost["gh"] = 15
	d.Summarize()
	var buf bytes.Buffer
	if err := d.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := `touchmark distribute · hub acme-eng (github.com/1) @ 3f2a1c9 · touchmark dev
gh  github.com  read r  write w: ok  resolve complete

  opened   5  gh:acme/r0 #1, gh:acme/r1 #2, gh:acme/r2 #3 and 2 more
  skipped  2  2 private in public hub

Cost  gh 15 writes
`
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteTextSuperseded(t *testing.T) {
	d := NewDelivery("plan", "dev")
	d.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/1", Commit: sampleCommit}
	d.Outcome = Superseded
	d.Warnings = []string{"the tip of the hub's default branch is 0123456, not 3f2a1c9"}
	d.Summarize()
	var buf bytes.Buffer
	if err := d.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := `touchmark plan · hub acme-eng (github.com/1) @ 3f2a1c9 · touchmark dev
superseded: the hub's default branch has moved on; no target was inspected
Warnings
  the tip of the hub's default branch is 0123456, not 3f2a1c9
`
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	var md bytes.Buffer
	if err := d.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "**Superseded:**") || strings.Contains(md.String(), "| Outcome |") {
		t.Errorf("markdown of a superseded run:\n%s", md.String())
	}
}

func TestWriteTextNoTargets(t *testing.T) {
	d := NewDelivery("plan", "dev")
	d.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/1", Commit: sampleCommit}
	d.Providers = []ProviderInfo{{ID: "gh", Type: "github", Host: "github.com", Error: "probe: auth (HTTP 401): bad credentials"}}
	d.Cost["gh"] = 0
	d.Summarize()
	var buf bytes.Buffer
	if err := d.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	want := `touchmark plan · hub acme-eng (github.com/1) @ 3f2a1c9 · touchmark dev
gh  github.com  read ?  write none: not checked  unavailable

  no targets

Warnings
  provider gh: probe: auth (HTTP 401): bad credentials
Cost  gh ≈ 0 writes
`
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteMarkdown(t *testing.T) {
	d := sampleDelivery()
	d.Strict = true
	var buf bytes.Buffer
	if err := d.WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	want := "### touchmark plan\n" +
		"\n" +
		"Hub `acme-eng` (`github.com/712345678`) at `3f2a1c9` (PR #41) · touchmark 0\\.2\\.0 · strict\n" +
		"\n" +
		"| Provider | Host | Read | Write | Resolve |\n" +
		"|---|---|---|---|---|\n" +
		"| `gh` | `github.com` | `acme-read[bot]` | `acme-write[bot]`: not checked | resolve complete |\n" +
		"| `corp` | `gitlab.example.com` | `tm-reader` | `tm-writer`: not checked | resolve incomplete, 1 target missing |\n" +
		"\n" +
		"| Outcome | Targets |\n" +
		"|---|---:|\n" +
		"| open | 2 |\n" +
		"| update | 2 |\n" +
		"| unchanged | 1 |\n" +
		"| close | 1 |\n" +
		"| skipped | 3 |\n" +
		"| blocked | 1 |\n" +
		"| deferred | 1 |\n" +
		"| failed | 1 |\n" +
		"\n" +
		"<details><summary>open (2)</summary>\n" +
		"\n" +
		"- `gh:acme/billing`\n" +
		"- `gh:acme/sdk`\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>update (2)</summary>\n" +
		"\n" +
		"- `corp:platform/api` [\\!14](https://gitlab.example.com/platform/api/-/merge_requests/14) (content)\n" +
		"- `gh:acme/api` [\\#87](https://github.com/acme/api/pull/87) (content)\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>unchanged (1)</summary>\n" +
		"\n" +
		"- `gh:Acme/Zeta` [\\#3](https://github.com/Acme/Zeta/pull/3)\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>close (1)</summary>\n" +
		"\n" +
		"- `gh:acme/old` [\\#12](https://github.com/acme/old/pull/12) (no\\-diff)\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>skipped (3)</summary>\n" +
		"\n" +
		"- `gh:acme/archive` (archived)\n" +
		"- `gh:acme/web` (not\\-opted\\-in)\n" +
		"- 1 private in public hub, not named\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>blocked (1)</summary>\n" +
		"\n" +
		"- `gh:acme/cli` [\\#31](https://github.com/acme/cli/pull/31) (branch\\-in\\-use)\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>deferred (1)</summary>\n" +
		"\n" +
		"- `gh:acme/web2` (rollout\\-limit)\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details><summary>failed (1)</summary>\n" +
		"\n" +
		"- `gh:acme/broken` (auth)\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"#### Warnings\n" +
		"\n" +
		"- hub head not checked\\: local run\n" +
		"- gh\\:acme/billing\\: orphaned 4\\: docs/a\\.md, docs/b\\.md, docs/c\\.md and 1 more\n" +
		"- gh\\:acme/broken\\: read the opt\\-in file\\: auth \\(HTTP 401\\)\\: bad credentials\n" +
		"\n" +
		"**Cost:** gh ≈ 11 writes · corp ≈ 2 writes\n"
	if got := buf.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A summary over 1 MiB is cut at a line, its open section closed, with a
// note at the end.
func TestWriteMarkdownCut(t *testing.T) {
	d := NewDelivery("plan", "dev")
	d.Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/1", Commit: sampleCommit}
	d.Providers = []ProviderInfo{{ID: "gh", Type: "github", Host: "github.com", ResolveComplete: true}}
	long := strings.Repeat("x", 200)
	for i := range 20000 {
		d.Targets = append(d.Targets, DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: fmt.Sprint(i + 1),
			Path: fmt.Sprintf("acme/%s-%05d", long, i), Outcome: OutcomeOpened, Writes: 3})
	}
	d.Summarize()
	var buf bytes.Buffer
	if err := d.WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if len(out) > maxMarkdown {
		t.Errorf("%d bytes, more than %d", len(out), maxMarkdown)
	}
	if !strings.HasSuffix(out, "</details>\n"+cutNote) {
		t.Errorf("the cut summary does not end with the closed section and the note: %q", out[max(0, len(out)-300):])
	}
	if strings.Count(out, "<details>") != strings.Count(out, "</details>") {
		t.Error("unbalanced <details>")
	}
	// A small report is not cut.
	var small bytes.Buffer
	if err := sampleDelivery().WriteMarkdown(&small); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(small.String(), "cut at 1 MiB") {
		t.Error("a small summary has the cut note")
	}
}

func TestMarkdownEscaping(t *testing.T) {
	for in, want := range map[string]string{
		"plain words":            "plain words",
		"<script>&":              "&lt;script&gt;&amp;",
		"*bold* _it_ [x](y) `c`": "\\*bold\\* \\_it\\_ \\[x\\]\\(y\\) \\`c\\`",
		"line\nbreak\r":          "line break ",
		"@jdoe | #1":             "\\@jdoe \\| \\#1",
	} {
		if got := mdText(in); got != want {
			t.Errorf("mdText(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"acme/x":                      "`acme/x`",
		"a`b":                         "``a`b``",
		"`edge":                       "`` `edge ``",
		"new\nline":                   "`new line`",
		"":                            "` `",
		"bidi" + string(rune(0x202e)): "`bidi `",
	} {
		if got := mdCode(in); got != want {
			t.Errorf("mdCode(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"https://github.com/acme/x/pull/1": "https://github.com/acme/x/pull/1",
		"http://localhost:3000/pulls/2":    "http://localhost:3000/pulls/2",
		"javascript:alert(1)":              "",
		"https://x/a b":                    "",
		"https://x/a)(b":                   "",
		"https://user@x/a":                 "",
		"ftp://x/a":                        "",
		"":                                 "",
	} {
		if got := linkURL(in); got != want {
			t.Errorf("linkURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// compileReportSchema compiles schemas/report.schema.json.
func compileReportSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	data, ok := schemas.Get("report")
	if !ok {
		t.Fatal("no report schema")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	url := schemas.BaseURL + "report.schema.json"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

// validate encodes d as the commands print it and validates it.
func validate(sch *jsonschema.Schema, d *Delivery) error {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, d); err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(&buf)
	if err != nil {
		return err
	}
	return sch.Validate(v)
}

func TestDeliverySchema(t *testing.T) {
	sch := compileReportSchema(t)
	valid := map[string]*Delivery{"sample": sampleDelivery(), "empty": NewDelivery("plan", "dev")}
	valid["empty"].Hub = DeliveryHub{ID: "acme-eng", Fingerprint: "github.com/1", Commit: sampleCommit}

	superseded := NewDelivery("plan", "dev")
	superseded.Hub = valid["empty"].Hub
	superseded.Outcome = Superseded
	superseded.Warnings = []string{"superseded"}
	valid["superseded"] = superseded

	distributed := sampleDelivery()
	distributed.Command = "distribute"
	distributed.Ops = []Op{
		{Time: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC), Account: "acme-write[bot]", Target: "gh:acme/sdk", Kind: "push", Before: "", After: sampleCommit},
		{Time: time.Date(2026, 9, 25, 10, 0, 1, 500, time.UTC), Account: "acme-write[bot]", Target: "gh:acme/sdk", Kind: "create-pr", PR: 5},
	}
	distributed.Sweep = SweepInfo{Ran: true, Complete: true}
	distributed.Notes = map[string]string{"only": "gh:acme/sdk"}
	valid["distribute"] = distributed

	// Every outcome with every reason it allows.
	every := NewDelivery("plan", "dev")
	every.Hub = valid["empty"].Hub
	n := 0
	add := func(o Outcome, reason string) {
		n++
		tg := DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: fmt.Sprint(n), Path: fmt.Sprintf("acme/r%d", n), Outcome: o, Reason: reason}
		if o == OutcomeDeclined {
			tg.PR = &PRRef{Number: 44, State: "closed"}
		}
		if reason == ReasonPrivate {
			tg.RepoID, tg.Path = "", ""
		}
		every.Targets = append(every.Targets, tg)
	}
	for _, o := range Outcomes {
		reasons := Reasons[o]
		if len(reasons) == 0 {
			add(o, "")
		}
		for _, r := range reasons {
			add(o, strings.Replace(r, "*", "workflows", 1))
		}
	}
	every.Summarize()
	valid["every outcome"] = every

	// The targets a public hub does not name: skipped, and those whose pull
	// request the sweep closes.
	hidden := NewDelivery("distribute", "dev")
	hidden.Hub = valid["empty"].Hub
	hidden.Targets = []DeliveryTarget{
		{Provider: "gh", Host: "github.com", Outcome: OutcomeSkipped, Reason: ReasonPrivate},
		{Provider: "gh", Host: "github.com", Outcome: OutcomeClosed, Reason: "target-dropped"},
		{Provider: "gh", Host: "github.com", Outcome: OutcomeBlocked, Reason: "archived"},
		{Provider: "gh", Host: "github.com", Outcome: OutcomeFailed, Reason: "transient"},
	}
	hidden.Summarize()
	valid["hidden targets"] = hidden

	for name, d := range valid {
		if err := validate(sch, d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// What the schema refuses.
	target := func() DeliveryTarget {
		return DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: "1", Path: "acme/x", Outcome: OutcomeOpened}
	}
	invalid := map[string]func(d *Delivery){
		"unknown reason": func(d *Delivery) {
			tg := target()
			tg.Outcome, tg.Reason = OutcomeSkipped, "bored"
			d.Targets = []DeliveryTarget{tg}
		},
		"opened with a reason": func(d *Delivery) {
			tg := target()
			tg.Reason = "content"
			d.Targets = []DeliveryTarget{tg}
		},
		"updated without a reason": func(d *Delivery) {
			tg := target()
			tg.Outcome = OutcomeUpdated
			d.Targets = []DeliveryTarget{tg}
		},
		"declined without a PR": func(d *Delivery) {
			tg := target()
			tg.Outcome = OutcomeDeclined
			d.Targets = []DeliveryTarget{tg}
		},
		"blocked rules without a rule": func(d *Delivery) {
			tg := target()
			tg.Outcome, tg.Reason = OutcomeBlocked, "rules:"
			d.Targets = []DeliveryTarget{tg}
		},
		"private target named": func(d *Delivery) {
			tg := target()
			tg.Outcome, tg.Reason = OutcomeSkipped, ReasonPrivate
			d.Targets = []DeliveryTarget{tg}
		},
		"private target with packs": func(d *Delivery) {
			d.Targets = []DeliveryTarget{{Provider: "gh", Host: "github.com", Outcome: OutcomeSkipped, Reason: ReasonPrivate, Packs: []string{"base"}}}
		},
		"target without a path": func(d *Delivery) {
			tg := target()
			tg.Path = ""
			d.Targets = []DeliveryTarget{tg}
		},
		"target without an id": func(d *Delivery) {
			tg := target()
			tg.RepoID = ""
			d.Targets = []DeliveryTarget{tg}
		},
		"hidden target with a pull request": func(d *Delivery) {
			d.Targets = []DeliveryTarget{{Provider: "gh", Host: "github.com", Outcome: OutcomeClosed, Reason: "target-dropped",
				PR: &PRRef{Number: 3, State: "closed"}}}
		},
		"hidden target with warnings": func(d *Delivery) {
			d.Targets = []DeliveryTarget{{Provider: "gh", Host: "github.com", Outcome: OutcomeFailed, Reason: "transient",
				Warnings: []string{"close pull request: acme/secret"}}}
		},
		"bad key": func(d *Delivery) {
			tg := target()
			tg.Key = "sha256:abc"
			d.Targets = []DeliveryTarget{tg}
		},
		"unknown outcome": func(d *Delivery) {
			tg := target()
			tg.Outcome = "sent"
			d.Targets = []DeliveryTarget{tg}
		},
		"summary without an outcome": func(d *Delivery) { delete(d.Summary, OutcomeFailed) },
		"bad commit":                 func(d *Delivery) { d.Hub.Commit = "HEAD" },
		"bad fingerprint":            func(d *Delivery) { d.Hub.Fingerprint = "github.com" },
		"bad command":                func(d *Delivery) { d.Command = "apply" },
		"bad provider id in cost":    func(d *Delivery) { d.Cost["Bad_ID"] = 1 },
		"bad op kind": func(d *Delivery) {
			d.Ops = []Op{{Time: time.Now(), Account: "a", Target: "gh:acme/x", Kind: "merge"}}
		},
	}
	for name, edit := range invalid {
		d := NewDelivery("plan", "dev")
		d.Hub = valid["empty"].Hub
		edit(d)
		if err := validate(sch, d); err == nil {
			t.Errorf("%s: the schema accepts it", name)
		}
	}
}

// TestPlanGoldensValidate validates the JSON reports the CLI's end-to-end
// tests of plan produce (internal/cli/testdata/golden/plan-*) against the
// schema, with the hub commit their placeholder stands for.
func TestPlanGoldensValidate(t *testing.T) {
	sch := compileReportSchema(t)
	files, err := filepath.Glob(filepath.Join("..", "cli", "testdata", "golden", "plan-*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no plan reports under ../cli/testdata/golden; update this test if they moved")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte("$COMMIT"), []byte(sampleCommit))
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := sch.Validate(v); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
}

// TestSchemaOpKinds keeps the op kinds of the schema identical to OpKinds,
// and every kind valid in a report.
func TestSchemaOpKinds(t *testing.T) {
	data, _ := schemas.Get("report")
	var doc struct {
		Defs struct {
			Op struct {
				Properties struct {
					Kind struct {
						Enum []string `json:"enum"`
					} `json:"kind"`
				} `json:"properties"`
			} `json:"op"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Defs.Op.Properties.Kind.Enum; !slices.Equal(got, OpKinds) {
		t.Errorf("schema op kinds %q, Go has %q", got, OpKinds)
	}
	sch := compileReportSchema(t)
	d := sampleDelivery()
	d.Command = "distribute"
	for _, kind := range OpKinds {
		d.Ops = append(d.Ops, Op{Time: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC), Account: "acme-write[bot]", Target: "gh:acme/sdk", Kind: kind})
	}
	if err := validate(sch, d); err != nil {
		t.Errorf("every op kind: %v", err)
	}
}

// TestSchemaReasons keeps the reasons of the schema identical to Reasons.
func TestSchemaReasons(t *testing.T) {
	data, _ := schemas.Get("report")
	var doc struct {
		Defs struct {
			Target struct {
				Properties struct {
					Outcome struct {
						Enum []string `json:"enum"`
					} `json:"outcome"`
				} `json:"properties"`
				AllOf []struct {
					If struct {
						Properties struct {
							Outcome *struct {
								Const string `json:"const"`
							} `json:"outcome"`
						} `json:"properties"`
					} `json:"if"`
					Then struct {
						Required   []string `json:"required"`
						Properties struct {
							Reason struct {
								Enum  []string `json:"enum"`
								AnyOf []struct {
									Enum    []string `json:"enum"`
									Pattern string   `json:"pattern"`
								} `json:"anyOf"`
							} `json:"reason"`
						} `json:"properties"`
					} `json:"then"`
				} `json:"allOf"`
			} `json:"target"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, o := range Outcomes {
		outcomes = append(outcomes, string(o))
	}
	if got := doc.Defs.Target.Properties.Outcome.Enum; !slices.Equal(got, outcomes) {
		t.Errorf("schema outcomes %q, Go has %q", got, outcomes)
	}
	seen := map[Outcome]bool{}
	for _, rule := range doc.Defs.Target.AllOf {
		if rule.If.Properties.Outcome == nil {
			continue
		}
		o := Outcome(rule.If.Properties.Outcome.Const)
		seen[o] = true
		reason := rule.Then.Properties.Reason
		got := slices.Clone(reason.Enum)
		for _, alt := range reason.AnyOf {
			got = append(got, alt.Enum...)
			if p, ok := strings.CutSuffix(alt.Pattern, ":[^ ]+$"); ok {
				got = append(got, strings.TrimPrefix(p, "^")+":*")
			}
		}
		want := Reasons[o]
		slices.Sort(got)
		want = slices.Sorted(slices.Values(want))
		if !slices.Equal(got, want) {
			t.Errorf("%s: schema reasons %q, Go has %q", o, got, want)
		}
		if needs := len(Reasons[o]) > 0; needs != slices.Contains(rule.Then.Required, "reason") {
			t.Errorf("%s: the schema requires a reason: %v, Go lists reasons: %v", o, !needs, needs)
		}
	}
	for _, o := range Outcomes {
		if !seen[o] {
			t.Errorf("the schema has no rule for outcome %s", o)
		}
	}
}

// TestHiddenSweepTarget: a non-public repository of a public hub that the
// sweep closes a pull request in has no path; its warnings, which may quote
// a platform message naming it, are printed nowhere, and it is only
// counted.
func TestHiddenSweepTarget(t *testing.T) {
	d := NewDelivery("distribute", "0.2.0")
	d.Providers = []ProviderInfo{{ID: "gh", Type: "github", Host: "github.com", ResolveComplete: true}}
	d.Targets = []DeliveryTarget{
		{Provider: "gh", Host: "github.com", Outcome: OutcomeClosed, Reason: "target-dropped",
			Warnings: []string{"preflight: rule pull-requests: acme-write[bot] may not write pull-requests of other/secret-payroll"}},
		{Provider: "gh", Host: "github.com", RepoID: "7", Path: "acme/api", Outcome: OutcomeClosed, Reason: "no-diff", Warnings: []string{"named"}},
	}
	d.Summarize()
	var text, md strings.Builder
	if err := d.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"text": text.String(), "markdown": md.String()} {
		if strings.Contains(out, "secret-payroll") || strings.Contains(out, "gh::") {
			t.Errorf("%s names the hidden target:\n%s", name, out)
		}
		if !strings.Contains(out, "1 private in public hub") || !strings.Contains(out, "named") {
			t.Errorf("%s:\n%s", name, out)
		}
	}
}
