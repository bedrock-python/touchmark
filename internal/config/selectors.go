package config

import (
	"fmt"
	"slices"
	"strings"
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
	// Match are the glob patterns of an org or group entry: a repository
	// of the namespace is selected when its full path matches one of them
	// (Selects); none selects every repository.
	Match []string
	// Assumed is set when the entry's opt_in is assumed (its own, or
	// defaults.opt_in): the targets it selects count as opted in without an
	// opt-in file.
	Assumed bool
}

// Selects reports whether the repository at path, which the platform
// listed for an org or group selector, passes its match patterns: it
// matches one of them, ignoring case, or there are none. A repo selector
// selects its repository.
func (s Selector) Selects(path string) bool {
	return len(s.Match) == 0 || matchAnyPath(s.Match, path)
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
		s := Selector{Entry: i, Provider: r.provider(firstNonEmpty(e.Provider, ref.Provider)),
			Assumed: targets.effectiveOptIn(e) == OptInAssumed}
		if entryKind(e) == "repo" {
			s.Repo = ref.Path
		} else {
			s.Namespace = ref.Path
			s.Subgroups = includeSubgroups(e)
			s.Topics = slices.Clone(e.Topics)
			s.Forks = e.Forks
			s.Match = slices.Clone(e.Match)
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
// that covers the repository at path on provider; ok is false when none
// does. An entry is a pattern (glob.go) whose provider is resolved as in
// Names: an entry without glob characters names one repository, as Names
// matches it. Entries that do not parse, and URLs ResolveURLs did not
// rewrite, are skipped: Check reports them.
func Excluded(hub *Hub, targets *Targets, provider, path string) (index int, ok bool) {
	if targets == nil {
		return 0, false
	}
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	r := resolver{hub: hub, targets: targets}
	for i, ex := range targets.Exclude {
		pat, err := parsePattern(ex)
		if err == nil && r.matchPattern(pat, Ref{Provider: provider, Path: path}) {
			return i, true
		}
	}
	return 0, false
}

// ExcludedBeneath returns the indexes of the exclude entries of targets.yml
// that name one repository (no glob characters) at a path the repository at
// path on provider lies beneath, ignoring case, on a compatible provider
// (resolved as in Excluded). Such an entry excludes a repository at its
// own path only, never what lies beneath it: written for a namespace, as a
// browser shows group and project URLs alike, it wants "/**".
func ExcludedBeneath(hub *Hub, targets *Targets, provider, path string) []int {
	if targets == nil {
		return nil
	}
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	r := resolver{hub: hub, targets: targets}
	var out []int
	for i, ex := range targets.Exclude {
		pat, err := parsePattern(ex)
		if err != nil || isGlob(pat.Path) || len(path) <= len(pat.Path) || path[len(pat.Path)] != '/' {
			continue
		}
		if strings.EqualFold(path[:len(pat.Path)], pat.Path) && sameProvider(r.provider(pat.Provider), r.provider(provider)) {
			out = append(out, i)
		}
	}
	return out
}
