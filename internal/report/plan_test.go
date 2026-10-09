package report

import (
	"regexp"
	"strings"
	"testing"
)

// planned is the sample plan of a hub pull request limited to one pack,
// with --assume-opt-in and an estimate.
func planned() *Delivery {
	d := sampleDelivery()
	d.Scope = &Scope{Mode: ScopePacks, Packs: []string{"python"}, Base: sampleCommit, Processed: 12, Total: 57}
	d.Assumed = true
	d.Targets[0].Assumed = true
	d.Estimate = &Estimate{NewPRs: 250, MaxNewPRs: 100, Runs: 3, Providers: map[string]ProviderEstimate{
		"gh":   {Writes: 400, Seconds: 2700, APIWrites: 600, APISeconds: 4020, TotalWrites: 1000, TotalSeconds: 7400},
		"corp": {Writes: 2, Seconds: 1},
	}}
	d.Paths = []PathChange{
		{Action: "update", Path: ".claude/settings.json", Sensitive: true, Targets: 15},
		{Action: "add", Path: "prompts/review.md", Targets: 12},
		{Action: "add", Path: "docs/a|b.md", Targets: 1},
	}
	return d
}

// A plan's text and Markdown say its scope, --assume-opt-in, and per
// provider the writes and their time by both paths and over every run.
func TestPlanLines(t *testing.T) {
	d := planned()
	var text, md strings.Builder
	if err := d.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nscope: pack python changed → 12 of 57 targets (--all for every target)\n",
		"\n--assume-opt-in: for this report only, a target without an opt-in file counts as opted in (one whose file says enabled: false stays opted out); 1 target is planned as if it had an empty one\n",
		"\nThis hub pull request changes, across the targets it affects\n" +
			"  update  .claude/settings.json  SENSITIVE  15 targets\n" +
			"  add     prompts/review.md                 12 targets\n" +
			"  add     docs/a|b.md                       1 target\n\n",
		"\nCost  gh ≈ 400 writes (~45 min), ≈ 600 (~1 h 7 min) if commits must be signed through the API, ≈ 1000 (~2 h 3 min) over all runs · " +
			"corp ≈ 2 writes (<1 min) · 3 runs (250 new pull requests, max_new_prs_per_run 100)\n",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	for _, want := range []string{
		"\n**Scope:** pack python changed → 12 of 57 targets \\(\\-\\-all for every target\\)\n",
		"\n**\\-\\-assume\\-opt\\-in:** for this report only, a target without an opt\\-in file counts as opted in",
		"\n**Cost:** gh ≈ 400 writes \\(\\~45 min\\)",
		"<details><summary>This hub pull request changes, across the targets it affects (3)</summary>\n\n| Change | Path | Targets |\n|---|---|---:|\n" +
			"| update ⚠ sensitive | `.claude/settings.json` | 15 |\n| add | `prompts/review.md` | 12 |\n| add | `docs/a\\|b.md` | 1 |\n",
	} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, md.String())
		}
	}

	cases := []struct {
		scope Scope
		want  string
	}{
		{Scope{Mode: ScopePacks, Packs: []string{"agents", "claude"}, Processed: 57, Total: 412}, "packs agents, claude changed → 57 of 412 targets (--all for every target)"},
		{Scope{Mode: ScopeHub, Total: 412}, "only hub files changed, which no target receives → none of 412 targets planned (--all for every target)"},
		{Scope{Mode: ScopeAll, Reason: "hub.yml changed", Processed: 412, Total: 412}, "every target (412): hub.yml changed"},
		{Scope{Mode: ScopeAll, Processed: 1, Total: 1}, "every target (1)"},
	}
	for _, tc := range cases {
		if got := scopeText(&tc.scope); got != tc.want {
			t.Errorf("scopeText(%+v) = %q, want %q", tc.scope, got, tc.want)
		}
	}

	// One run, and a rollout that cannot open anything.
	d.Estimate = &Estimate{NewPRs: 3, MaxNewPRs: 100, Runs: 1, Providers: map[string]ProviderEstimate{"gh": {Writes: 12, Seconds: 11}}}
	if got := d.costLine(); got != "gh ≈ 12 writes (<1 min) · 1 run" {
		t.Errorf("one run: %q", got)
	}
	d.Estimate = &Estimate{NewPRs: 3, Providers: map[string]ProviderEstimate{"gh": {}}}
	if got := d.costLine(); got != "gh ≈ 0 writes (<1 min) · 3 new pull requests wait: max_new_prs_per_run is 0" {
		t.Errorf("max 0: %q", got)
	}
	// distribute reports what it wrote, without an estimate.
	d.Command = "distribute"
	d.Cost = map[string]int{"gh": 5}
	if got := d.costLine(); got != "gh 5 writes · corp 0 writes" {
		t.Errorf("distribute: %q", got)
	}
}

func TestApproxDuration(t *testing.T) {
	for s, want := range map[int64]string{0: "<1 min", 29: "<1 min", 30: "~1 min", 419: "~7 min", 3599: "~1 h", 3600: "~1 h", 14599: "~4 h 3 min"} {
		if got := approxDuration(s); got != want {
			t.Errorf("approxDuration(%d) = %q, want %q", s, got, want)
		}
	}
}

// WriteComment renders the Markdown report within a comment's limit, with
// the closing marker after the cut.
func TestWriteComment(t *testing.T) {
	d := planned()
	const marker = "\n<!-- touchmark plan: acme-eng -->\n"
	var full strings.Builder
	if err := d.WriteComment(&full, 0, marker); err != nil {
		t.Fatal(err)
	}
	var md strings.Builder
	if err := d.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if full.String() != md.String()+marker {
		t.Errorf("an uncut comment is not the Markdown report and its marker:\n%s", full.String())
	}
	for i := range 3000 {
		d.Targets = append(d.Targets, DeliveryTarget{Provider: "gh", Host: "github.com", RepoID: "9", Path: "acme/many-" + strings.Repeat("x", i%40),
			Outcome: OutcomeOpened})
	}
	var cut strings.Builder
	if err := d.WriteComment(&cut, 20000, marker); err != nil {
		t.Fatal(err)
	}
	s := cut.String()
	if len(s) > 20000 || !strings.HasSuffix(s, commentCut+marker) || strings.Count(s, "<details>") != strings.Count(s, "</details>") {
		t.Errorf("cut comment of %d bytes:\n%s", len(s), s[max(0, len(s)-400):])
	}
}

// WritePlainComment renders the same report without HTML (Bitbucket shows
// HTML as text): every section a bold title, and the cut note and closing
// line as WriteComment has them.
func TestWritePlainComment(t *testing.T) {
	d := planned()
	const marker = "\n[touchmark-plan]: # \"touchmark plan: acme-eng\"\n"
	var html, plain strings.Builder
	if err := d.WriteComment(&html, 0, marker); err != nil {
		t.Fatal(err)
	}
	if err := d.WritePlainComment(&plain, 0, marker); err != nil {
		t.Fatal(err)
	}
	s := plain.String()
	if !strings.Contains(html.String(), "<details>") {
		t.Fatalf("the sample report has no section to compare:\n%s", html.String())
	}
	if strings.Contains(s, "<") || !strings.HasSuffix(s, marker) {
		t.Errorf("plain comment:\n%s", s)
	}
	for _, m := range regexp.MustCompile(`<details><summary>(.*)</summary>`).FindAllStringSubmatch(html.String(), -1) {
		if !strings.Contains(s, "\n**"+m[1]+"**\n") {
			t.Errorf("plain comment lacks the section title **%s**:\n%s", m[1], s)
		}
	}
	for i := range 3000 {
		d.Targets = append(d.Targets, DeliveryTarget{Provider: "bb", Host: "bitbucket.org", RepoID: "9", Path: "acme/many-" + strings.Repeat("x", i%40),
			Outcome: OutcomeOpened})
	}
	var cut strings.Builder
	if err := d.WritePlainComment(&cut, 20000, marker); err != nil {
		t.Fatal(err)
	}
	if s := cut.String(); len(s) > 20000 || !strings.HasSuffix(s, commentCut+marker) || strings.Contains(s, "<") {
		t.Errorf("cut plain comment of %d bytes:\n%s", len(s), s[max(0, len(s)-400):])
	}
}

// The schema accepts a plan's scope, estimate and --assume-opt-in, and
// refuses them malformed.
func TestPlanSchema(t *testing.T) {
	sch := compileReportSchema(t)
	for name, d := range map[string]*Delivery{"planned": planned(), "hub only": func() *Delivery {
		d := planned()
		d.Scope = &Scope{Mode: ScopeHub, Base: sampleCommit, Total: 57}
		return d
	}()} {
		if err := validate(sch, d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, edit := range map[string]func(d *Delivery){
		"unknown mode":        func(d *Delivery) { d.Scope.Mode = "some" },
		"packs without packs": func(d *Delivery) { d.Scope.Packs = nil },
		"hub with packs":      func(d *Delivery) { d.Scope.Mode = ScopeHub },
		"short base":          func(d *Delivery) { d.Scope.Base = "3f2a1c9" },
		"path of no target":   func(d *Delivery) { d.Paths[0].Targets = 0 },
		"path action":         func(d *Delivery) { d.Paths[0].Action = "create" },
		"negative seconds":    func(d *Delivery) { d.Estimate.Providers["gh"] = ProviderEstimate{Writes: 1, Seconds: -1} },
		"provider id":         func(d *Delivery) { d.Estimate.Providers["GH"] = ProviderEstimate{} },
		"providers null":      func(d *Delivery) { d.Estimate = &Estimate{Providers: nil} },
	} {
		d := planned()
		edit(d)
		if err := validate(sch, d); err == nil {
			t.Errorf("%s: the schema accepts it", name)
		}
	}
}
