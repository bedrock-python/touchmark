package config

import (
	"fmt"
	"slices"
)

// Selector is one targets.yml entry as delivery resolves it through a
// platform: one repository, or the repositories of an
// organisation or group.
type Selector struct {
	// Entry is the index of the entry in Targets.Targets.
	Entry int
	// Provider is the id of the entry's provider: its provider field, else
	// the prefix of its repo, org or group, else defaults.provider, else the
	// hub's only provider in hub.yml. It is "" when none of them decides: a
	// hub without providers, whose only provider is implicit (the CI's).
	Provider string
	// Repo is the repository path of a repo entry, without its prefix, as
	// written.
	Repo string
	// Namespace is the organisation or group of an org or group entry,
	// without its prefix. Subgroups tells whether nested namespaces are
	// included: always, unless the entry says subgroups: false.
	Namespace string
	Subgroups bool
	// Topics must all be present on a repository; Forks includes forks.
	Topics []string
	Forks  bool
}

// Selectors returns the entries of targets.yml as selectors, in file order.
// An entry without a repo, org or group, or whose selector does not parse,
// is an error; ParseTargets and Check reject such files first. Nil hub and
// targets are a legacy hub and an empty targets.yml.
func Selectors(hub *Hub, targets *Targets) ([]Selector, error) {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	if targets == nil {
		return nil, nil
	}
	r := resolver{hub: hub, targets: targets}
	out := make([]Selector, 0, len(targets.Targets))
	for i := range targets.Targets {
		e := &targets.Targets[i]
		if e.Repo == "" && e.Org == "" && e.Group == "" {
			return nil, fmt.Errorf("%s: targets[%d]: needs one of repo, org or group", TargetsFile, i)
		}
		ref, err := entryRef(e)
		if err != nil {
			return nil, fmt.Errorf("%s: targets[%d]: %w", TargetsFile, i, err)
		}
		s := Selector{Entry: i, Provider: r.provider(firstNonEmpty(e.Provider, ref.Provider))}
		if entryKind(e) == "repo" {
			s.Repo = ref.Path
		} else {
			s.Namespace = ref.Path
			s.Subgroups = includeSubgroups(e)
			s.Topics = slices.Clone(e.Topics)
			s.Forks = e.Forks
		}
		out = append(out, s)
	}
	return out, nil
}

// Names reports whether ref names the repository at path on the provider
// with id provider, the way Select matches repo entries and exclude
// entries: the paths are equal ignoring case, and the providers are equal
// unless either stays unknown. ref's provider is its prefix, else
// defaults.provider, else the hub's only provider; the provider "" is
// unknown unless defaults.provider or a single provider in hub.yml decides.
// Nil hub and targets are a legacy hub and an empty targets.yml.
func Names(hub *Hub, targets *Targets, ref Ref, provider, path string) bool {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	if targets == nil {
		targets = &Targets{}
	}
	r := resolver{hub: hub, targets: targets}
	return r.matchRepo(ref, "", Ref{Provider: provider, Path: path})
}

// Excluded returns the index of the first exclude entry of targets.yml
// that names the repository at path on provider (see Names); ok is false
// when none does. Entries that do not parse are skipped: Check reports
// them.
func Excluded(hub *Hub, targets *Targets, provider, path string) (index int, ok bool) {
	if targets == nil {
		return 0, false
	}
	for i, ex := range targets.Exclude {
		ref, err := ParseRef(ex)
		if err == nil && Names(hub, targets, ref, provider, path) {
			return i, true
		}
	}
	return 0, false
}
