package config

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		name     string
		hub      *Hub
		targets  *Targets
		known    map[string]bool
		errors   []string // substrings, one error each, in order
		warnings []string // substrings, one warning each, in order
	}{
		{
			name: "clean",
			hub:  selectHub("gh"),
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"agents"}},
				Targets: []Entry{
					{Repo: "gh:acme/billing", Packs: []string{"python-service"}},
					{Org: "acme", Packs: []string{"claude"}},
				},
				Exclude: []string{"acme/legacy"},
			},
		},
		{
			name: "nil hub and targets",
		},
		{
			name:   "placeholder id",
			hub:    &Hub{ID: PlaceholderID},
			errors: []string{`hub.yml: id: still the template placeholder "change-me"`},
		},
		{
			name: "legacy hub without id",
			hub:  &Hub{Legacy: true},
		},
		{
			name: "unknown packs in targets.yml",
			hub:  selectHub(),
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"agents", "rust"}},
				Targets:  []Entry{{Repo: "acme/billing", Packs: []string{"go"}}},
			},
			errors: []string{
				`targets.yml: defaults.packs[1]: unknown pack "rust"`,
				`targets.yml: targets[0].packs[0]: unknown pack "go"`,
			},
		},
		{
			name:     "former names in targets.yml",
			hub:      selectHub(),
			targets:  &Targets{Defaults: Defaults{Packs: []string{"python"}}},
			warnings: []string{`targets.yml: defaults.packs[0]: pack "python" was renamed to "python-service"`},
		},
		{
			name: "requires",
			hub: &Hub{Packs: map[string]PackMeta{
				"claude": {Requires: []string{"agentz", "base"}},
				"agents": {Formerly: []string{"base"}},
			}},
			known:    knownPacks("agents", "claude"),
			errors:   []string{`hub.yml: packs.claude.requires[0]: unknown pack "agentz"`},
			warnings: []string{`hub.yml: packs.claude.requires[1]: pack "base" was renamed to "agents"`},
		},
		{
			name: "requires cycles, each reported once",
			hub: &Hub{Packs: map[string]PackMeta{
				"a": {Requires: []string{"b"}},
				"b": {Requires: []string{"a"}},
				"c": {Requires: []string{"c"}},
				"d": {Requires: []string{"a"}},
				"e": {Requires: []string{"old-f"}},
				"f": {Requires: []string{"e"}, Formerly: []string{"old-f"}},
			}},
			known: knownPacks("a", "b", "c", "d", "e", "f"),
			errors: []string{
				"hub.yml: requires cycle: a -> b -> a",
				"hub.yml: requires cycle: c -> c",
				"hub.yml: requires cycle: e -> f -> e",
			},
			warnings: []string{`packs.e.requires[0]: pack "old-f" was renamed to "f"`},
		},
		{
			name: "formerly collisions",
			hub: &Hub{Packs: map[string]PackMeta{
				"agents": {Formerly: []string{"base", "claude"}},
				"claude": {Formerly: []string{"claude"}},
				"core":   {Formerly: []string{"base"}},
				"docs":   {Formerly: []string{"gitlab"}},
			}},
			known: knownPacks("agents", "claude", "core", "docs", "gitlab"),
			errors: []string{
				`hub.yml: packs.agents.formerly: "claude" is a current pack`,
				`hub.yml: packs.claude.formerly: "claude" is a current pack`,
				`hub.yml: packs.core.formerly: "base" is also a former name of "agents"`,
				`hub.yml: packs.docs.formerly: "gitlab" is a current pack`,
			},
		},
		{
			name:     "metadata for a pack that does not exist",
			hub:      &Hub{Packs: map[string]PackMeta{"rust": {Description: "gone"}}},
			known:    knownPacks("agents"),
			warnings: []string{"hub.yml: packs.rust: no such pack under packs/"},
		},
		{
			name: "unknown providers",
			hub:  selectHub("gh"),
			targets: &Targets{
				Defaults: Defaults{Provider: "gl"},
				Targets: []Entry{
					{Repo: "corp:acme/billing"},
					{Org: "acme", Provider: "ghe"},
				},
				Exclude: []string{"old:acme/x"},
			},
			errors: []string{
				`targets.yml: defaults.provider: unknown provider "gl" (hub.yml defines gh)`,
				`targets.yml: targets[0].repo: unknown provider "corp"`,
				`targets.yml: targets[1].provider: unknown provider "ghe"`,
				`targets.yml: exclude[0]: unknown provider "old"`,
			},
		},
		{
			name:    "a provider prefix without providers",
			hub:     selectHub(),
			targets: &Targets{Targets: []Entry{{Group: "gl:acme"}}},
			errors:  []string{`targets.yml: targets[0].group: unknown provider "gl" (hub.yml defines none)`},
		},
		{
			name: "several providers need an explicit one",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{Targets: []Entry{
				{Repo: "acme/billing"},
				{Repo: "gh:acme/payments"},
				{Org: "acme", Provider: "corp"},
			}},
			errors: []string{"targets.yml: targets[0]: no provider, and hub.yml defines gh, corp"},
		},
		{
			name: "defaults.provider covers every entry",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{
				Defaults: Defaults{Provider: "gh"},
				Targets:  []Entry{{Repo: "acme/billing"}},
			},
		},
		{
			name: "malformed references built in code",
			hub:  selectHub(),
			targets: &Targets{
				Targets: []Entry{{Repo: "billing"}},
				Exclude: []string{"acme"},
			},
			errors: []string{
				`targets.yml: targets[0]: "billing"`,
				`targets.yml: exclude[0]: "acme"`,
			},
		},
		{
			name: "listed and excluded",
			hub:  selectHub(),
			targets: &Targets{
				Targets: []Entry{{Repo: "acme/billing"}, {Repo: "acme/legacy"}},
				Exclude: []string{"Acme/Legacy"},
			},
			warnings: []string{"targets.yml: exclude[0]: Acme/Legacy is also listed as targets[1]; exclude wins"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			known := tt.known
			if known == nil {
				known = knownPacks()
			}
			warns, errs := Check(tt.hub, tt.targets, known)
			if len(errs) != len(tt.errors) {
				t.Errorf("errors = %q, want %d", errs, len(tt.errors))
			}
			for i := 0; i < len(errs) && i < len(tt.errors); i++ {
				if !strings.Contains(errs[i].Error(), tt.errors[i]) {
					t.Errorf("error %d = %q, want %q", i, errs[i], tt.errors[i])
				}
			}
			if len(warns) != len(tt.warnings) {
				t.Errorf("warnings = %v, want %d", warns, len(tt.warnings))
			}
			for i := 0; i < len(warns) && i < len(tt.warnings); i++ {
				if !strings.Contains(warns[i].String(), tt.warnings[i]) {
					t.Errorf("warning %d = %q, want %q", i, warns[i], tt.warnings[i])
				}
			}
		})
	}
}

// TestCheckFixtures runs Check on parsed fixtures that belong together.
func TestCheckFixtures(t *testing.T) {
	hub := mustParseHub(t, readFixture(t, "hub/valid/full.yml"))
	targets, _, err := ParseTargets([]byte(`
version: 1
defaults: {provider: gh, packs: [agents]}
targets:
  - repo: corp:platform/billing
    packs: [python]
  - org: gh:acme
    packs: [claude]
`))
	if err != nil {
		t.Fatal(err)
	}
	warns, errs := Check(hub, targets, knownPacks())
	if len(errs) > 0 {
		t.Errorf("errors: %v", errs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Message, `"python" was renamed to "python-service"`) {
		t.Errorf("warnings: %v", warns)
	}

	placeholder := mustParseHub(t, readFixture(t, "hub/valid/placeholder-id.yml"))
	if _, errs := Check(placeholder, nil, knownPacks()); len(errs) != 1 {
		t.Errorf("placeholder id: %v", errs)
	}
}
