package config

import (
	"reflect"
	"strings"
	"testing"
)

// selectHub ships agents, claude (requires agents), python-service
// (requires agents, formerly python), python-library (requires agents) and
// gitlab.
func selectHub(providers ...string) *Hub {
	h := &Hub{
		ID: "acme-eng",
		Packs: map[string]PackMeta{
			"claude":         {Requires: []string{"agents"}},
			"python-service": {Requires: []string{"agents"}, Formerly: []string{"python"}},
			"python-library": {Requires: []string{"agents"}},
		},
	}
	for _, id := range providers {
		h.Providers = append(h.Providers, Provider{ID: id, Type: "github"})
	}
	return h
}

func knownPacks(names ...string) map[string]bool {
	if len(names) == 0 {
		names = []string{"agents", "claude", "python-service", "python-library", "gitlab"}
	}
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func ref(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func boolPtr(b bool) *bool { return &b }

func TestSelect(t *testing.T) {
	tests := []struct {
		name       string
		hub        *Hub
		targets    *Targets
		optIn      *OptIn
		target     string
		known      map[string]bool
		want       []string
		sources    map[string][]string
		unresolved []string
		warning    string
		wantErr    string
	}{
		{
			name:    "defaults only",
			targets: &Targets{Defaults: Defaults{Packs: []string{"agents"}}},
			target:  "acme/billing",
			want:    []string{"agents"},
			sources: map[string][]string{"agents": {"defaults"}},
		},
		{
			name: "defaults, entries, opt-in in order with requires",
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"gitlab"}},
				Targets: []Entry{
					{Repo: "acme/billing", Packs: []string{"python-service"}},
					{Repo: "acme/other", Packs: []string{"python-library"}},
				},
			},
			optIn:  &OptIn{Packs: []string{"claude"}},
			target: "acme/billing",
			want:   []string{"gitlab", "agents", "python-service", "claude"},
			sources: map[string][]string{
				"gitlab":         {"defaults"},
				"python-service": {"targets.yml"},
				"agents":         {"requires python-service", "requires claude"},
				"claude":         {".engineering-assets.yml"},
			},
		},
		{
			name: "duplicates keep the first position",
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"agents", "gitlab"}},
				Targets: []Entry{
					{Repo: "acme/billing", Packs: []string{"claude", "agents"}},
					{Repo: "acme/billing", Packs: []string{"gitlab"}},
				},
			},
			optIn:  &OptIn{Packs: []string{"claude", "claude"}},
			target: "acme/billing",
			want:   []string{"agents", "gitlab", "claude"},
			sources: map[string][]string{
				"agents": {"defaults", "targets.yml", "requires claude"},
				"gitlab": {"defaults", "targets.yml"},
				"claude": {"targets.yml", ".engineering-assets.yml"},
			},
		},
		{
			name:    "a dependency listed later moves before its dependent",
			targets: &Targets{Defaults: Defaults{Packs: []string{"claude", "gitlab", "agents"}}},
			target:  "acme/billing",
			want:    []string{"agents", "claude", "gitlab"},
		},
		{
			name: "requires is transitive, depth-first in declared order",
			hub: &Hub{Packs: map[string]PackMeta{
				"a": {Requires: []string{"b", "c"}},
				"b": {Requires: []string{"d"}},
				"c": {Requires: []string{"d", "e"}},
			}},
			targets: &Targets{Defaults: Defaults{Packs: []string{"e", "a"}}},
			target:  "acme/billing",
			known:   knownPacks("a", "b", "c", "d", "e"),
			want:    []string{"e", "d", "b", "c", "a"},
		},
		{
			name: "requires cycle",
			hub: &Hub{Packs: map[string]PackMeta{
				"a": {Requires: []string{"b"}},
				"b": {Requires: []string{"c"}},
				"c": {Requires: []string{"a"}},
			}},
			targets: &Targets{Defaults: Defaults{Packs: []string{"a"}}},
			target:  "acme/billing",
			known:   knownPacks("a", "b", "c"),
			wantErr: "requires cycle: a -> b -> c -> a",
		},
		{
			name:    "a pack requiring itself",
			hub:     &Hub{Packs: map[string]PackMeta{"a": {Requires: []string{"a"}}}},
			optIn:   &OptIn{Packs: []string{"a"}},
			target:  "acme/billing",
			known:   knownPacks("a"),
			wantErr: "requires cycle: a -> a",
		},
		{
			name:    "unknown pack in the opt-in file",
			optIn:   &OptIn{Packs: []string{"rust"}},
			target:  "acme/billing",
			wantErr: `unknown pack "rust" (from .engineering-assets.yml)`,
		},
		{
			name:    "unknown pack in targets.yml",
			targets: &Targets{Targets: []Entry{{Repo: "acme/billing", Packs: []string{"rust"}}}},
			target:  "acme/billing",
			wantErr: `unknown pack "rust" (from targets.yml)`,
		},
		{
			name:    "unknown pack in requires",
			hub:     &Hub{Packs: map[string]PackMeta{"claude": {Requires: []string{"agentz"}}}},
			optIn:   &OptIn{Packs: []string{"claude"}},
			target:  "acme/billing",
			wantErr: `unknown pack "agentz" (from requires claude)`,
		},
		{
			name:    "an unknown pack of a non-matching entry does not matter",
			targets: &Targets{Targets: []Entry{{Repo: "acme/other", Packs: []string{"rust"}}}},
			target:  "acme/billing",
			want:    []string{},
		},
		{
			name:    "a former name selects the pack with a warning",
			optIn:   &OptIn{Packs: []string{"python"}},
			target:  "acme/billing",
			want:    []string{"agents", "python-service"},
			warning: `pack "python" was renamed to "python-service"`,
		},
		{
			name: "excluded target",
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"agents"}},
				Targets:  []Entry{{Repo: "acme/legacy", Packs: []string{"claude"}}},
				Exclude:  []string{"Acme/Legacy"},
			},
			optIn:   &OptIn{Packs: []string{"rust"}},
			target:  "acme/legacy",
			want:    []string{},
			sources: map[string][]string{},
			warning: "acme/legacy is excluded by exclude[0]",
		},
		{
			name: "exclude on another provider does not apply",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"agents"}},
				Exclude:  []string{"corp:acme/legacy"},
			},
			target: "gh:acme/legacy",
			want:   []string{"agents"},
		},
		{
			name: "org and group entries that may contain the target are unresolved",
			targets: &Targets{
				Defaults: Defaults{Packs: []string{"agents"}},
				Targets: []Entry{
					{Org: "acme", Topics: []string{"python"}, Packs: []string{"python-service"}},
					{Group: "ACME/platform", Packs: []string{"gitlab"}},
					{Org: "other", Packs: []string{"claude"}},
					{Org: "acme"},
				},
			},
			target:     "acme/platform/billing",
			want:       []string{"agents"},
			unresolved: []string{"org: acme", "group: ACME/platform"},
		},
		{
			name: "subgroups false reaches only direct children",
			targets: &Targets{Targets: []Entry{
				{Group: "acme", Subgroups: boolPtr(false), Packs: []string{"gitlab"}},
				{Group: "acme/platform", Subgroups: boolPtr(false), Packs: []string{"claude"}},
			}},
			target:     "acme/platform/billing",
			want:       []string{},
			unresolved: []string{"group: acme/platform"},
		},
		{
			name: "a namespace entry on another provider is not unresolved",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{Targets: []Entry{
				{Org: "acme", Provider: "corp", Packs: []string{"gitlab"}},
				{Org: "gh:acme", Packs: []string{"claude"}},
			}},
			target:     "gh:acme/billing",
			want:       []string{},
			unresolved: []string{"org: gh:acme"},
		},
		{
			name: "providers differ",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{Targets: []Entry{
				{Repo: "corp:acme/billing", Packs: []string{"gitlab"}},
				{Repo: "acme/billing", Provider: "corp", Packs: []string{"gitlab"}},
				{Repo: "gh:acme/billing", Packs: []string{"claude"}},
			}},
			target: "gh:acme/billing",
			want:   []string{"agents", "claude"},
		},
		{
			name: "an unknown provider on either side matches by path",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{Targets: []Entry{
				{Repo: "corp:acme/billing", Packs: []string{"gitlab"}},
				{Repo: "acme/billing", Packs: []string{"claude"}},
			}},
			target: "acme/billing",
			want:   []string{"gitlab", "agents", "claude"},
		},
		{
			name: "defaults.provider resolves both sides",
			hub:  selectHub("gh", "corp"),
			targets: &Targets{
				Defaults: Defaults{Provider: "corp"},
				Targets: []Entry{
					{Repo: "acme/billing", Packs: []string{"gitlab"}},
					{Repo: "gh:acme/billing", Packs: []string{"claude"}},
				},
			},
			target: "acme/billing",
			want:   []string{"gitlab"},
		},
		{
			name: "the only provider resolves both sides",
			hub:  selectHub("gh"),
			targets: &Targets{Targets: []Entry{
				{Repo: "gh:acme/billing", Packs: []string{"claude"}},
			}},
			target: "acme/billing",
			want:   []string{"agents", "claude"},
		},
		{
			name:    "paths compare case-insensitively",
			targets: &Targets{Targets: []Entry{{Repo: "Acme/Billing", Packs: []string{"gitlab"}}}},
			target:  "acme/billing",
			want:    []string{"gitlab"},
		},
		{
			name:    "legacy entries select without packs",
			targets: &Targets{Legacy: true, Targets: []Entry{{Repo: "acme/billing"}}},
			optIn:   &OptIn{Packs: []string{"claude"}, Legacy: true},
			target:  "acme/billing",
			want:    []string{"agents", "claude"},
		},
		{
			name:    "custom opt-in file name in sources",
			hub:     &Hub{OptInFile: ".github/assets.yml"},
			optIn:   &OptIn{Packs: []string{"gitlab"}},
			target:  "acme/billing",
			want:    []string{"gitlab"},
			sources: map[string][]string{"gitlab": {".github/assets.yml"}},
		},
		{
			name:   "nil hub, targets and opt-in",
			target: "acme/billing",
			want:   []string{},
		},
		{
			name:    "a malformed entry built in code",
			targets: &Targets{Targets: []Entry{{Packs: []string{"agents"}}}},
			target:  "acme/billing",
			wantErr: "targets[0]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := tt.hub
			if hub == nil && !strings.HasPrefix(tt.name, "nil") {
				hub = selectHub()
			}
			known := tt.known
			if known == nil {
				known = knownPacks()
			}
			sel, warns, err := Select(hub, tt.targets, tt.optIn, ref(t, tt.target), known)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sel.Packs, tt.want) {
				t.Errorf("packs = %q, want %q", sel.Packs, tt.want)
			}
			if tt.sources != nil && !reflect.DeepEqual(sel.Sources, tt.sources) {
				t.Errorf("sources = %v, want %v", sel.Sources, tt.sources)
			}
			for _, p := range sel.Packs {
				if len(sel.Sources[p]) == 0 {
					t.Errorf("pack %s has no source", p)
				}
			}
			if !reflect.DeepEqual(sel.Unresolved, tt.unresolved) || sel.Complete != (len(tt.unresolved) == 0) {
				t.Errorf("unresolved = %q, complete = %v; want %q", sel.Unresolved, sel.Complete, tt.unresolved)
			}
			switch {
			case tt.warning == "" && len(warns) > 0:
				t.Errorf("unexpected warnings %v", warns)
			case tt.warning != "" && !anyWarningContains(warns, tt.warning):
				t.Errorf("warnings %v lack %q", warns, tt.warning)
			}
		})
	}
}

func TestSelectIsDeterministic(t *testing.T) {
	hub := &Hub{Packs: map[string]PackMeta{}}
	known := map[string]bool{}
	var all []string
	for _, c := range "abcdefghijklmnop" {
		n := string(c)
		known[n] = true
		all = append(all, n)
		if n != "a" {
			hub.Packs[n] = PackMeta{Requires: []string{string(c - 1)}}
		}
	}
	targets := &Targets{Defaults: Defaults{Packs: []string{"p", "h", "c"}}}
	first, _, err := Select(hub, targets, nil, Ref{Path: "acme/x"}, known)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Packs, all) {
		t.Errorf("packs = %v", first.Packs)
	}
	for i := 0; i < 20; i++ {
		again, _, _ := Select(hub, targets, nil, Ref{Path: "acme/x"}, known)
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, again, first)
		}
	}
}

func TestExplicit(t *testing.T) {
	hub := selectHub()
	sel, err := Explicit(hub, []string{"claude", "gitlab", "claude"}, knownPacks())
	if err != nil {
		t.Fatal(err)
	}
	want := Selection{
		Packs:    []string{"agents", "claude", "gitlab"},
		Complete: true,
		Sources: map[string][]string{
			"claude": {"--packs"},
			"gitlab": {"--packs"},
			"agents": {"requires claude"},
		},
	}
	if !reflect.DeepEqual(sel, want) {
		t.Errorf("Explicit = %+v, want %+v", sel, want)
	}
	if _, err := Explicit(hub, []string{"python"}, knownPacks()); err == nil || !strings.Contains(err.Error(), `renamed to "python-service"`) {
		t.Errorf("former name: %v", err)
	}
	if _, err := Explicit(hub, []string{"rust"}, knownPacks()); err == nil || !strings.Contains(err.Error(), `unknown pack "rust" (from --packs)`) {
		t.Errorf("unknown pack: %v", err)
	}
	cyclic := &Hub{Packs: map[string]PackMeta{"a": {Requires: []string{"b"}}, "b": {Requires: []string{"a"}}}}
	if _, err := Explicit(cyclic, []string{"b"}, knownPacks("a", "b")); err == nil || !strings.Contains(err.Error(), "b -> a -> b") {
		t.Errorf("cycle: %v", err)
	}
	sel, err = Explicit(nil, nil, nil)
	if err != nil || !sel.Complete || len(sel.Packs) != 0 {
		t.Errorf("empty = %+v, %v", sel, err)
	}
}

func TestAliases(t *testing.T) {
	var nilHub *Hub
	if got := nilHub.Aliases(); len(got) != 0 {
		t.Errorf("nil hub aliases = %v", got)
	}
	h := selectHub()
	got := h.Aliases()
	if !reflect.DeepEqual(got, map[string][]string{"python-service": {"python"}}) {
		t.Errorf("Aliases = %v", got)
	}
	got["python-service"][0] = "changed"
	if h.Packs["python-service"].Formerly[0] != "python" {
		t.Error("Aliases shares memory with the hub")
	}
}

// TestExplicitRequiresFormerName: a former name in hub.yml's requires is the
// hub's business, not the user's, so --packs still works.
func TestExplicitRequiresFormerName(t *testing.T) {
	hub := &Hub{Packs: map[string]PackMeta{
		"agents": {Formerly: []string{"base"}},
		"claude": {Requires: []string{"base"}},
	}}
	sel, err := Explicit(hub, []string{"claude"}, knownPacks("agents", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"agents", "claude"}; !reflect.DeepEqual(sel.Packs, want) {
		t.Errorf("Explicit = %v, want %v", sel.Packs, want)
	}
	if _, err := Explicit(hub, []string{"base"}, knownPacks("agents", "claude")); err == nil || !strings.Contains(err.Error(), `renamed to "agents"`) {
		t.Errorf("former name in --packs: %v", err)
	}
}

// TestFormerlyOfMissingPack: a rename to a pack that does not exist is not
// honoured: the old name stays unknown instead of selecting a pack that
// ships nothing and inherits the old pack's history.
func TestFormerlyOfMissingPack(t *testing.T) {
	hub := &Hub{Packs: map[string]PackMeta{"agents": {Formerly: []string{"base"}}}}
	targets := &Targets{Defaults: Defaults{Packs: []string{"base"}}}
	known := knownPacks("claude")
	if _, _, err := Select(hub, targets, nil, ref(t, "acme/svc"), known); err == nil || !strings.Contains(err.Error(), `unknown pack "base"`) {
		t.Errorf("Select = %v, want unknown pack", err)
	}
	if _, err := Explicit(hub, []string{"base"}, known); err == nil || !strings.Contains(err.Error(), `unknown pack "base"`) {
		t.Errorf("Explicit = %v, want unknown pack", err)
	}
	_, errs := Check(hub, targets, known)
	if !anyErrorContains(errs, `defaults.packs[0]: unknown pack "base"`) {
		t.Errorf("Check errors = %v, want the unknown pack", errs)
	}
	if got := hub.KnownAliases(known); len(got) != 0 {
		t.Errorf("KnownAliases = %v, want none", got)
	}
}

func TestKnownAliases(t *testing.T) {
	var nilHub *Hub
	if got := nilHub.KnownAliases(nil); len(got) != 0 {
		t.Errorf("nil hub = %v", got)
	}
	hub := &Hub{Packs: map[string]PackMeta{
		"agents": {Formerly: []string{"base", "core", "shared"}},
		"claude": {Formerly: []string{"agents-old", "shared"}}, // shared is claimed twice
		"extra":  {Formerly: []string{"claude"}},               // claude is a current pack
		"meta":   {Formerly: []string{"ghost"}},                // meta has no directory
		"python": {Formerly: []string{"meta"}},                 // meta is in hub.yml
	}}
	got := hub.KnownAliases(knownPacks("agents", "claude", "extra", "python"))
	want := map[string][]string{"agents": {"base", "core"}, "claude": {"agents-old"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("KnownAliases = %v, want %v", got, want)
	}
}

func anyErrorContains(errs []error, s string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}
