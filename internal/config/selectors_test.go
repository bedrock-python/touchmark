package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSelectors(t *testing.T) {
	targets, _, err := ParseTargets([]byte(`version: 1
defaults:
  provider: gh
targets:
  - repo: acme/billing
  - repo: corp:platform/api
  - org: acme
    topics: [python, service]
    forks: true
  - provider: corp
    group: platform
    subgroups: false
  - group: corp:tools/sub
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Selectors(selectHub("gh", "corp"), targets)
	if err != nil {
		t.Fatal(err)
	}
	want := []Selector{
		{Entry: 0, Provider: "gh", Repo: "acme/billing"},
		{Entry: 1, Provider: "corp", Repo: "platform/api"},
		{Entry: 2, Provider: "gh", Namespace: "acme", Subgroups: true, Topics: []string{"python", "service"}, Forks: true},
		{Entry: 3, Provider: "corp", Namespace: "platform"},
		{Entry: 4, Provider: "corp", Namespace: "tools/sub", Subgroups: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Selectors =\n%+v\nwant\n%+v", got, want)
	}
	// The topics are a copy.
	got[2].Topics[0] = "changed"
	if targets.Targets[2].Topics[0] != "python" {
		t.Error("Selectors shares the topics of targets.yml")
	}
}

// A hub whose only provider is implicit leaves the provider unknown; one
// provider in hub.yml decides.
func TestSelectorsProvider(t *testing.T) {
	targets := &Targets{Targets: []Entry{{Repo: "acme/x"}}}
	for _, tc := range []struct {
		hub  *Hub
		want string
	}{
		{nil, ""},
		{&Hub{}, ""},
		{selectHub("only"), "only"},
		{selectHub("a", "b"), ""},
	} {
		got, err := Selectors(tc.hub, targets)
		if err != nil || len(got) != 1 || got[0].Provider != tc.want {
			t.Errorf("hub %+v: %+v, %v; want provider %q", tc.hub, got, err, tc.want)
		}
	}
	if got, err := Selectors(nil, nil); got != nil || err != nil {
		t.Errorf("Selectors(nil, nil) = %v, %v", got, err)
	}
	for _, bad := range []Entry{{}, {Repo: "no-slash"}, {Org: "a//b"}} {
		_, err := Selectors(nil, &Targets{Targets: []Entry{bad}})
		if err == nil || !strings.Contains(err.Error(), "targets[0]") {
			t.Errorf("entry %+v: error %v, want one naming targets[0]", bad, err)
		}
	}
}

func TestNamesAndExcluded(t *testing.T) {
	hub := selectHub("gh", "corp")
	targets := &Targets{
		Defaults: Defaults{Provider: "gh"},
		Exclude:  []string{"not a ref", "acme/legacy", "corp:platform/sandbox"},
	}
	for _, tc := range []struct {
		provider, path string
		index          int
		ok             bool
	}{
		{"gh", "acme/legacy", 1, true},
		{"gh", "ACME/Legacy", 1, true},
		{"corp", "acme/legacy", 0, false}, // the bare path means defaults.provider
		{"", "acme/legacy", 1, true},      // an unknown provider matches by path
		{"corp", "platform/sandbox", 2, true},
		{"gh", "platform/sandbox", 0, false},
		{"gh", "acme/billing", 0, false},
	} {
		index, ok := Excluded(hub, targets, tc.provider, tc.path)
		if index != tc.index || ok != tc.ok {
			t.Errorf("Excluded(%q, %q) = %d, %v; want %d, %v", tc.provider, tc.path, index, ok, tc.index, tc.ok)
		}
	}
	// Without defaults.provider and with several providers, a bare path
	// names the repository on every provider.
	if !Names(hub, &Targets{}, Ref{Path: "acme/x"}, "corp", "acme/X") {
		t.Error("a bare path does not name the repository on corp")
	}
	if Names(hub, &Targets{}, Ref{Provider: "gh", Path: "acme/x"}, "corp", "acme/x") {
		t.Error("gh:acme/x names the repository on corp")
	}
	if _, ok := Excluded(nil, nil, "gh", "acme/x"); ok {
		t.Error("nil targets exclude something")
	}
}
