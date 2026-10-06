package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		// Without glob characters: one repository, ignoring case, nothing
		// beneath it.
		{"acme/legacy", "acme/legacy", true},
		{"acme/legacy", "ACME/Legacy", true},
		{"acme/legacy", "acme/legacy/sub", false},
		{"acme/legacy", "acme/legacy-x", false},
		// * stays within one segment.
		{"acme/legacy-*", "acme/legacy-api", true},
		{"acme/legacy-*", "acme/legacy-", true},
		{"acme/legacy-*", "acme/legacy", false},
		{"acme/*", "acme/x/y", false},
		{"*/billing", "acme/billing", true},
		{"ACME/Legacy-*", "acme/LEGACY-web", true},
		// ? is one character.
		{"acme/?ld", "acme/old", true},
		{"acme/?ld", "acme/bold", false},
		{"acme/?ld", "acme/ld", false},
		// ** spans segments, none included.
		{"platform/legacy/**", "platform/legacy/api", true},
		{"platform/legacy/**", "platform/legacy", true},
		{"platform/legacy/**", "platform/legacy/a/b/c", true},
		{"platform/legacy/**", "platform/legacyx/api", false},
		{"platform/legacy/**", "platform/api", false},
		{"**/sandbox", "acme/sandbox", true},
		{"**/sandbox", "a/b/c/sandbox", true},
		{"**/sandbox", "acme/sandbox-x", false},
		{"acme/**/api", "acme/api", true},
		{"acme/**/api", "acme/x/y/api", true},
		{"acme/**/api", "acme/x/api/y", false},
		{"**", "any/thing/at/all", true},
		// ** inside a longer segment is * twice.
		{"acme/svc-**", "acme/svc-a", true},
		{"acme/svc-**", "acme/svc-a/b", false},
		// A dot is an ordinary character.
		{"acme/.*", "acme/.github", true},
		{"acme/*.go", "acme/x.go", true},
	} {
		if got := matchPath(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestGlobUnder(t *testing.T) {
	for _, tc := range []struct {
		pattern, ns string
		subgroups   bool
		want        bool
	}{
		{"acme/svc-*", "acme", true, true},
		{"ACME/svc-*", "acme", false, true},
		{"other/*", "acme", true, false},
		{"acme", "acme", true, false},
		{"acme/*", "acme/sub", true, false},
		{"acme/**", "acme/sub", true, true},
		{"acme/**", "acme", false, true},
		{"**/api", "platform", false, true},
		{"**/api", "platform/team", true, true},
		{"platform/*/api", "platform", false, false},
		{"platform/*/api", "platform", true, true},
		{"platform/team/**", "platform", false, true},
		{"*/x", "a/b", true, false},
		{"*/*/x", "a/b", true, true},
		{"a/**/b/**", "a", false, true},
		{"a/?", "a", false, true},
	} {
		if got := globUnder(tc.pattern, tc.ns, tc.subgroups); got != tc.want {
			t.Errorf("globUnder(%q, %q, subgroups %v) = %v, want %v", tc.pattern, tc.ns, tc.subgroups, got, tc.want)
		}
	}
}

// TestExcludedPatterns: exclude patterns resolve their provider as a plain
// entry does, and a plain entry still names one repository only.
func TestExcludedPatterns(t *testing.T) {
	hub := selectHub("gh", "corp")
	targets := &Targets{
		Defaults: Defaults{Provider: "gh"},
		Exclude:  []string{"acme/legacy", "corp:platform/legacy/**", "acme/sandbox-*", "**/archive-??"},
	}
	for _, tc := range []struct {
		provider, path string
		index          int
		ok             bool
	}{
		{"gh", "acme/legacy", 0, true},
		{"gh", "acme/legacy/x", 0, false},
		{"corp", "platform/legacy/api", 1, true},
		{"corp", "platform/legacy/team/api", 1, true},
		{"gh", "platform/legacy/api", 0, false}, // corp's pattern
		{"gh", "acme/sandbox-1", 2, true},
		{"corp", "acme/sandbox-1", 0, false}, // a bare pattern means defaults.provider
		{"gh", "a/b/archive-17", 3, true},
		{"gh", "acme/archive-7", 0, false},
	} {
		index, ok := Excluded(hub, targets, tc.provider, tc.path)
		if index != tc.index || ok != tc.ok {
			t.Errorf("Excluded(%q, %q) = %d, %v; want %d, %v", tc.provider, tc.path, index, ok, tc.index, tc.ok)
		}
	}
}

// TestSelectorsMatchAndOptIn: an org or group entry's match patterns and
// every entry's opt_in, its own or defaults.opt_in, reach the selectors.
func TestSelectorsMatchAndOptIn(t *testing.T) {
	targets, _, err := ParseTargets([]byte(`version: 1
defaults:
  opt_in: assumed
targets:
  - org: acme
    match: [acme/svc-*, acme/api]
  - repo: acme/billing
    opt_in: required
  - group: platform
    opt_in: assumed
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Selectors(selectHub("gh"), targets)
	if err != nil {
		t.Fatal(err)
	}
	want := []Selector{
		{Entry: 0, Provider: "gh", Namespace: "acme", Subgroups: true, Match: []string{"acme/svc-*", "acme/api"}, Assumed: true},
		{Entry: 1, Provider: "gh", Repo: "acme/billing"},
		{Entry: 2, Provider: "gh", Namespace: "platform", Subgroups: true, Assumed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Selectors =\n%+v\nwant\n%+v", got, want)
	}
	for path, want := range map[string]bool{"acme/svc-a": true, "ACME/API": true, "acme/apix": false, "acme/web": false} {
		if got := got[0].Selects(path); got != want {
			t.Errorf("Selects(%q) = %v, want %v", path, got, want)
		}
	}
	if !got[2].Selects("platform/anything") {
		t.Error("a selector without match does not select")
	}
	got[0].Match[0] = "changed"
	if targets.Targets[0].Match[0] != "acme/svc-*" {
		t.Error("Selectors shares the match patterns of targets.yml")
	}
}

// TestSelectExcludePatternAndMatch: local mode applies exclude patterns,
// and an org entry whose match leaves the target out cannot select it.
func TestSelectExcludePatternAndMatch(t *testing.T) {
	hub := selectHub("gh")
	targets := &Targets{
		Targets: []Entry{
			{Org: "acme", Match: []string{"acme/svc-*"}, Packs: []string{"python-service"}},
			{Org: "acme", Match: []string{"acme/lib-*"}, Packs: []string{"python-library"}},
		},
		Exclude: []string{"acme/svc-legacy*"},
	}
	sel, _, err := Select(hub, targets, &OptIn{}, ref(t, "acme/svc-billing"), knownPacks())
	if err != nil {
		t.Fatal(err)
	}
	if sel.Complete || !reflect.DeepEqual(sel.Unresolved, []string{"org: acme"}) {
		t.Errorf("svc-billing: %+v, want only the svc entry unresolved", sel)
	}
	sel, warns, err := Select(hub, targets, &OptIn{}, ref(t, "acme/svc-legacy-1"), knownPacks())
	if err != nil || !sel.Complete || len(sel.Packs) != 0 || len(warns) != 1 || !strings.Contains(warns[0].Message, "excluded by exclude[0] (acme/svc-legacy*)") {
		t.Errorf("svc-legacy-1: %+v, %v, %v", sel, warns, err)
	}
	sel, _, err = Select(hub, targets, &OptIn{}, ref(t, "acme/web"), knownPacks())
	if err != nil || !sel.Complete {
		t.Errorf("web: %+v, %v; want complete: no entry's match takes it", sel, err)
	}
}

// TestParseTargetsUnquotedPattern: a pattern that starts with * is a YAML
// alias unless quoted; the parse error says so.
func TestParseTargetsUnquotedPattern(t *testing.T) {
	for _, yml := range []string{
		"version: 1\nexclude:\n  - **/legacy\n",
		"version: 1\ntargets:\n  - org: acme\n    match: [*/svc-*]\n",
		"version: 1\nexclude:\n  - *-archive\n",
	} {
		_, _, err := ParseTargets([]byte(yml))
		if err == nil || !strings.Contains(err.Error(), `quote it, as in "**/legacy"`) {
			t.Errorf("%q: %v", yml, err)
		}
	}
	targets, _, err := ParseTargets([]byte("version: 1\nexclude:\n  - \"**/legacy\"\n  - '*-archive/x'\n"))
	if err != nil || !reflect.DeepEqual(targets.Exclude, []string{"**/legacy", "*-archive/x"}) {
		t.Errorf("quoted patterns: %+v, %v", targets, err)
	}
}

func TestCheckPatterns(t *testing.T) {
	hub := selectHub("gh", "corp")
	for _, tc := range []struct {
		name     string
		targets  *Targets
		errors   []string
		warnings []string
	}{
		{
			name: "an exclude pattern that covers no listed repository is fine",
			targets: &Targets{Defaults: Defaults{Provider: "gh"}, Targets: []Entry{{Org: "acme"}},
				Exclude: []string{"acme/legacy-*", "corp:platform/**"}},
		},
		{
			name: "an exclude pattern that covers a listed repository",
			targets: &Targets{Defaults: Defaults{Provider: "gh"},
				Targets: []Entry{{Repo: "acme/legacy-api"}, {Repo: "corp:acme/legacy-x"}, {Repo: "acme/billing"}},
				Exclude: []string{"acme/legacy-*"}},
			warnings: []string{"exclude[0]: acme/legacy-* also covers targets[0] (acme/legacy-api); exclude wins"},
		},
		{
			name:    "an exclude pattern with an unknown provider",
			targets: &Targets{Defaults: Defaults{Provider: "gh"}, Exclude: []string{"lab:platform/**"}},
			errors:  []string{`targets.yml: exclude[0]: unknown provider "lab"`},
		},
		{
			name:    "a malformed exclude pattern built in code",
			targets: &Targets{Defaults: Defaults{Provider: "gh"}, Exclude: []string{"acme/[x]"}},
			errors:  []string{`targets.yml: exclude[0]: "acme/[x]": character '['`},
		},
		{
			name: "match patterns that select nothing",
			targets: &Targets{Defaults: Defaults{Provider: "gh"}, Targets: []Entry{
				{Org: "acme", Match: []string{"acme/svc-*", "other/*"}},
				{Group: "corp:platform", Subgroups: boolPtr(false), Match: []string{"platform/*/api"}},
			}},
			warnings: []string{
				"targets[0].match[1]: other/* matches no repository under acme, so it selects nothing",
				"targets[1].match[0]: platform/*/api matches no repository directly under platform (subgroups: false)",
			},
		},
		{
			name: "deeper than a platform without nested namespaces allows",
			targets: &Targets{Defaults: Defaults{Provider: "gh"}, Targets: []Entry{
				{Org: "acme", Match: []string{"acme/a/b", "acme/svc-*", "acme/**/api", "other/x/y"}},
			}, Exclude: []string{"acme/legacy/tree/main", "acme/**/legacy", "acme/legacy-*", "corp:a/b/c/**"}},
			warnings: []string{
				"exclude[0]: acme/legacy/tree/main has 4 path segments, and a github repository path is owner/name, so it excludes nothing",
				"targets[0].match[0]: acme/a/b matches no repository directly under acme (a github namespace has no subgroups)",
				"targets[0].match[3]: other/x/y matches no repository under acme, so it selects nothing",
			},
		},
		{
			name: "opt_in: required under an assumed org",
			targets: &Targets{Defaults: Defaults{Provider: "gh", OptIn: OptInAssumed}, Targets: []Entry{
				{Org: "acme", Match: []string{"acme/svc-*"}},
				{Repo: "acme/svc-billing", OptIn: OptInRequired},
				{Repo: "acme/web", OptIn: OptInRequired},
				{Repo: "corp:acme/svc-x", OptIn: OptInRequired},
				{Group: "corp:platform", OptIn: OptInRequired},
				{Repo: "corp:platform/api", OptIn: OptInRequired},
			}},
			warnings: []string{"targets[1]: opt_in: required does not hold acme/svc-billing back: targets[0] (org: acme) has opt_in: assumed and may select it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warns, errs := Check(hub, tc.targets, knownPacks())
			if len(errs) != len(tc.errors) {
				t.Errorf("errors = %q, want %d", errs, len(tc.errors))
			}
			for i := 0; i < len(errs) && i < len(tc.errors); i++ {
				if !strings.Contains(errs[i].Error(), tc.errors[i]) {
					t.Errorf("error %d = %q, want %q", i, errs[i], tc.errors[i])
				}
			}
			if len(warns) != len(tc.warnings) {
				t.Errorf("warnings = %v, want %d", warns, len(tc.warnings))
			}
			for i := 0; i < len(warns) && i < len(tc.warnings); i++ {
				if !strings.Contains(warns[i].String(), tc.warnings[i]) {
					t.Errorf("warning %d = %q, want %q", i, warns[i], tc.warnings[i])
				}
			}
		})
	}
}
