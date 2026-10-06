package config

import (
	"reflect"
	"testing"
)

// TestTargetsAssumed: an entry's opt_in is its own, else defaults.opt_in,
// else required; one entry with assumed among those that select a target
// is enough, whatever the others say.
func TestTargetsAssumed(t *testing.T) {
	targets := &Targets{
		Defaults: Defaults{OptIn: OptInAssumed},
		Targets: []Entry{
			{Org: "acme"},
			{Repo: "acme/x", OptIn: OptInRequired},
			{Repo: "acme/y", OptIn: OptInAssumed},
		},
	}
	for _, tc := range []struct {
		entries []int
		want    bool
	}{
		{nil, false},
		{[]int{0}, true},
		{[]int{1}, false},
		{[]int{1, 0}, true},
		{[]int{2}, true},
		{[]int{1, 7, -1}, false},
	} {
		if got := targets.Assumed(tc.entries); got != tc.want {
			t.Errorf("Assumed(%v) = %v, want %v", tc.entries, got, tc.want)
		}
	}
	targets.Defaults.OptIn = ""
	if targets.Assumed([]int{0, 1}) || !targets.Assumed([]int{2}) {
		t.Error("without defaults.opt_in, only an entry's own assumed counts")
	}
	var none *Targets
	if none.Assumed([]int{0}) {
		t.Error("nil targets assume something")
	}
}

// TestAssume: what a local run can tell about a target without an opt-in
// file.
func TestAssume(t *testing.T) {
	hub := selectHub("gh", "corp")
	targets := &Targets{
		Defaults: Defaults{Provider: "gh"},
		Targets: []Entry{
			{Repo: "acme/billing", OptIn: OptInAssumed},
			{Repo: "acme/web"},
			{Org: "acme", OptIn: OptInAssumed, Match: []string{"acme/svc-*"}},
			{Group: "corp:platform", OptIn: OptInAssumed},
			{Org: "acme", Packs: []string{"claude"}},
			{Repo: "acme/legacy", OptIn: OptInAssumed},
			{Repo: "acme/old-1", OptIn: OptInAssumed},
		},
		Exclude: []string{"acme/legacy", "acme/old-*"},
	}
	for _, tc := range []struct {
		target string
		want   Assumption
	}{
		{"acme/billing", Assumption{Assumed: true}},
		{"gh:ACME/Billing", Assumption{Assumed: true}},
		{"corp:acme/billing", Assumption{}},
		{"acme/web", Assumption{}},
		{"acme/svc-api", Assumption{Unresolved: []string{"org: acme"}}},
		{"corp:platform/team/api", Assumption{Unresolved: []string{"group: corp:platform"}}},
		{"acme/legacy", Assumption{}},
		{"acme/old-1", Assumption{}},
		{"acme/svc-old-1", Assumption{Unresolved: []string{"org: acme"}}},
	} {
		got, err := Assume(hub, targets, ref(t, tc.target))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Assume(%s) = %+v, %v; want %+v", tc.target, got, err, tc.want)
		}
	}
	// An explicit entry decides: the org entries need not be resolved.
	targets.Targets = append(targets.Targets, Entry{Repo: "acme/svc-api", OptIn: OptInAssumed})
	if got, _ := Assume(hub, targets, ref(t, "acme/svc-api")); !reflect.DeepEqual(got, Assumption{Assumed: true}) {
		t.Errorf("explicit and org entry: %+v", got)
	}
	// defaults.opt_in applies to every entry.
	plain := &Targets{Defaults: Defaults{OptIn: OptInAssumed}, Targets: []Entry{{Repo: "acme/x"}}}
	if got, _ := Assume(hub, plain, ref(t, "acme/x")); !got.Assumed {
		t.Error("defaults.opt_in: assumed does not reach the repo entry")
	}
	if got, err := Assume(nil, nil, ref(t, "acme/x")); err != nil || got.Assumed || got.Unresolved != nil {
		t.Errorf("Assume without targets = %+v, %v", got, err)
	}
	if _, err := Assume(hub, &Targets{Exclude: []string{"acme/[x]"}}, ref(t, "acme/x")); err == nil {
		t.Error("a malformed exclude entry is no error")
	}
}

// TestOptInEnabled: enabled: false opts out; an absent or null enabled, or
// true, does not; enabled does not count in the hash, which lifts declines
// only for packs and ignore.
func TestOptInEnabled(t *testing.T) {
	parse := func(s string) *OptIn {
		t.Helper()
		o, _, err := ParseOptIn([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	for content, want := range map[string]bool{
		"":                               false,
		"version: 1\n":                   false,
		"version: 1\nenabled: true\n":    false,
		"version: 1\nenabled:\n":         false,
		"version: 1\nenabled: false\n":   true,
		"enabled: false\npacks: [x-y]\n": true,
	} {
		if got := parse(content).Disabled(); got != want {
			t.Errorf("%q: Disabled() = %v, want %v", content, got, want)
		}
	}
	var none *OptIn
	if none.Disabled() {
		t.Error("a nil opt-in is disabled")
	}
	if a, b := parse("version: 1\n").Hash(), parse("version: 1\nenabled: false\n").Hash(); a != b {
		t.Errorf("enabled changes the hash: %s, %s", a, b)
	}
}
