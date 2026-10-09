package config

import (
	"fmt"
	"slices"
	"strings"
)

// Sources of a pack in Selection.Sources, besides the opt-in file name.
const (
	sourceDefaults = "defaults"
	sourceTargets  = TargetsFile
	sourceExplicit = "--packs"
	sourceRequires = "requires "
)

// resolver matches targets.yml references against a target.
type resolver struct {
	hub     *Hub
	targets *Targets
}

// provider resolves the provider of a reference: its own, then
// defaults.provider, then the hub's only provider. "" means unknown.
func (r resolver) provider(own string) string {
	switch {
	case own != "":
		return own
	case r.targets.Defaults.Provider != "":
		return r.targets.Defaults.Provider
	case len(r.hub.Providers) == 1:
		return r.hub.Providers[0].ID
	}
	return ""
}

// sameProvider reports whether two resolved providers can be equal: they
// are, or one of them is unknown.
func sameProvider(a, b string) bool { return a == "" || b == "" || a == b }

// matchRepo reports whether ref, with the entry-level provider override,
// names target. Paths compare case-insensitively.
func (r resolver) matchRepo(ref Ref, override string, target Ref) bool {
	if !strings.EqualFold(ref.Path, target.Path) {
		return false
	}
	return sameProvider(r.provider(firstNonEmpty(override, ref.Provider)), r.provider(target.Provider))
}

// matchPattern reports whether the exclude pattern pat (parsePattern)
// covers target: its path matches (glob.go) and the providers are
// compatible, as in matchRepo.
func (r resolver) matchPattern(pat Ref, target Ref) bool {
	if !matchPath(pat.Path, target.Path) {
		return false
	}
	return sameProvider(r.provider(pat.Provider), r.provider(target.Provider))
}

// mayContain reports whether the org or group entry e, whose namespace is
// ns, could select target: the target lies in the namespace (directly, or
// in a nested one when subgroups are included) on a compatible provider,
// and its path passes the entry's match patterns. On Azure DevOps the
// namespace is the provider's organization, which repository paths
// (project/repository) do not start with: any target of the provider may
// lie in it.
func (r resolver) mayContain(e *Entry, ns Ref, target Ref) bool {
	provider := r.provider(firstNonEmpty(e.Provider, ns.Provider))
	if !sameProvider(provider, r.provider(target.Provider)) {
		return false
	}
	if len(e.Match) > 0 && !matchAnyPath(e.Match, target.Path) {
		return false
	}
	if r.providerType(provider) == "azure-devops" {
		return true
	}
	prefix := strings.ToLower(ns.Path) + "/"
	path := strings.ToLower(target.Path)
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return includeSubgroups(e) || !strings.Contains(path[len(prefix):], "/")
}

// providerType returns the type of the provider with id in hub.yml, "" for
// one it does not list.
func (r resolver) providerType(id string) string {
	for _, p := range r.hub.Providers {
		if p.ID == id {
			return p.Type
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// renames maps every former pack name to the pack that now carries it. It
// honours only renames to a pack that exists (in known): a former name of a
// pack that is gone stays an unknown pack, so its files are never treated as
// the history of a pack that ships nothing. A name that is a current pack
// (in known or in hub.Packs), or that two packs claim, is left out too.
// Check reports all three.
func renames(hub *Hub, known map[string]bool) map[string]string {
	out := map[string]string{}
	claimed := map[string]int{}
	for _, name := range sortedKeys(hub.Packs) {
		for _, old := range hub.Packs[name].Formerly {
			claimed[old]++
			out[old] = name
		}
	}
	for old, n := range claimed {
		_, current := hub.Packs[old]
		if n > 1 || current || known[old] || !known[out[old]] {
			delete(out, old)
		}
	}
	return out
}

// selection accumulates packs in order and expands requires.
type selection struct {
	hub      *Hub
	known    map[string]bool
	renamed  map[string]string
	optIn    string // name of the opt-in file, for messages
	explicit bool   // --packs: a renamed pack is an error, not a warning
	order    []string
	sources  map[string][]string
	warns    []Warning
}

func newSelection(hub *Hub, known map[string]bool) *selection {
	return &selection{
		hub:     hub,
		known:   known,
		renamed: renames(hub, known),
		optIn:   hub.OptInName(),
		sources: map[string][]string{},
	}
}

// fileOf names the file a source refers to, for warnings.
func (s *selection) fileOf(source string) string {
	switch {
	case source == sourceDefaults || source == sourceTargets:
		return TargetsFile
	case strings.HasPrefix(source, sourceRequires):
		return HubFile
	case source == sourceExplicit:
		return sourceExplicit
	}
	return source
}

// canonical returns the current name of pack name, which source asked for.
// A former name is an error only when the user typed it (--packs); in
// hub.yml's requires it resolves with a warning, like everywhere else.
func (s *selection) canonical(name, source string) (string, error) {
	if s.known[name] {
		return name, nil
	}
	if cur, ok := s.renamed[name]; ok {
		if s.explicit && source == sourceExplicit {
			return "", fmt.Errorf("pack %q was renamed to %q", name, cur)
		}
		s.warns = append(s.warns, Warning{
			File:    s.fileOf(source),
			Message: fmt.Sprintf("pack %q was renamed to %q; using %q", name, cur, cur),
		})
		return cur, nil
	}
	return "", fmt.Errorf("unknown pack %q (from %s)", name, source)
}

func (s *selection) addSource(name, source string) {
	if !slices.Contains(s.sources[name], source) {
		s.sources[name] = append(s.sources[name], source)
	}
}

// add appends packs from source, keeping the first position of each.
func (s *selection) add(packs []string, source string) error {
	for _, name := range packs {
		cur, err := s.canonical(name, source)
		if err != nil {
			return err
		}
		if _, seen := s.sources[cur]; !seen {
			s.order = append(s.order, cur)
		}
		s.addSource(cur, source)
	}
	return nil
}

// expand returns the packs in order with requires inserted: each pack's
// dependencies, transitively and depth-first in declared order, go right
// before it unless they are already earlier.
func (s *selection) expand() ([]string, error) {
	out := make([]string, 0, len(s.order))
	const visiting, done = 1, 2
	state := map[string]int{}
	var stack []string
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			i := slices.Index(stack, name)
			return fmt.Errorf("%s: requires cycle: %s", HubFile, strings.Join(append(slices.Clone(stack[i:]), name), " -> "))
		}
		state[name] = visiting
		stack = append(stack, name)
		for _, dep := range s.hub.Packs[name].Requires {
			source := sourceRequires + name
			cur, err := s.canonical(dep, source)
			if err != nil {
				return err
			}
			s.addSource(cur, source)
			if err := visit(cur); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		out = append(out, name)
		return nil
	}
	for _, name := range s.order {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func selectPacks(hub *Hub, targets *Targets, optIn *OptIn, target Ref, known map[string]bool) (Selection, []Warning, error) {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	if targets == nil {
		targets = &Targets{}
	}
	r := resolver{hub: hub, targets: targets}
	for i, ex := range targets.Exclude {
		pat, err := parsePattern(ex)
		if err != nil {
			return Selection{}, nil, fmt.Errorf("%s: exclude[%d]: %w", TargetsFile, i, err)
		}
		if r.matchPattern(pat, target) {
			w := Warning{File: TargetsFile, Message: fmt.Sprintf("%s is excluded by exclude[%d] (%s)", target, i, ex)}
			return Selection{Packs: []string{}, Complete: true, Sources: map[string][]string{}}, []Warning{w}, nil
		}
	}
	s := newSelection(hub, known)
	if err := s.add(targets.Defaults.Packs, sourceDefaults); err != nil {
		return Selection{}, nil, err
	}
	var unresolved []string
	for i := range targets.Targets {
		e := &targets.Targets[i]
		ref, err := entryRef(e)
		if err != nil {
			return Selection{}, nil, fmt.Errorf("%s: targets[%d]: %w", TargetsFile, i, err)
		}
		switch kind := entryKind(e); {
		case kind == "repo":
			if r.matchRepo(ref, e.Provider, target) {
				if err := s.add(e.Packs, sourceTargets); err != nil {
					return Selection{}, nil, err
				}
			}
		case len(e.Packs) > 0 && r.mayContain(e, ref, target):
			unresolved = append(unresolved, kind+": "+selectorValue(e))
		}
	}
	if optIn != nil {
		if err := s.add(optIn.Packs, s.optIn); err != nil {
			return Selection{}, s.warns, err
		}
	}
	packs, err := s.expand()
	if err != nil {
		return Selection{}, s.warns, err
	}
	return Selection{
		Packs:      packs,
		Complete:   len(unresolved) == 0,
		Unresolved: unresolved,
		Sources:    s.sources,
	}, s.warns, nil
}

// Assumption is what targets.yml says, as a local run can tell, about a
// target that has no opt-in file (Assume).
type Assumption struct {
	// Assumed is set when a repo: entry that names the target has opt_in:
	// assumed (its own, or defaults.opt_in) and no exclude entry covers the
	// target: it counts as opted in, as if it had an empty opt-in file.
	Assumed bool
	// Unresolved lists the org and group entries with opt_in: assumed whose
	// namespace may hold the target, as "org: acme": only the platform's API
	// tells whether they select it.
	Unresolved []string
}

// Assume tells whether targets.yml subscribes target, which has no opt-in
// file, in local mode: a repo: entry that names it (as Select matches
// them) with opt_in: assumed makes it Assumed; an org or group entry with
// opt_in: assumed whose namespace and match patterns may hold it is listed
// in Unresolved, since a local run cannot resolve it. An excluded target is
// neither. Nil hub and targets are a legacy hub and an empty targets.yml.
func Assume(hub *Hub, targets *Targets, target Ref) (Assumption, error) {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	var out Assumption
	if targets == nil {
		return out, nil
	}
	r := resolver{hub: hub, targets: targets}
	for i, ex := range targets.Exclude {
		pat, err := parsePattern(ex)
		if err != nil {
			return out, fmt.Errorf("%s: exclude[%d]: %w", TargetsFile, i, err)
		}
		if r.matchPattern(pat, target) {
			return out, nil
		}
	}
	for i := range targets.Targets {
		e := &targets.Targets[i]
		if targets.effectiveOptIn(e) != OptInAssumed {
			continue
		}
		ref, err := entryRef(e)
		if err != nil {
			return Assumption{}, fmt.Errorf("%s: targets[%d]: %w", TargetsFile, i, err)
		}
		switch kind := entryKind(e); {
		case kind == "repo":
			out.Assumed = out.Assumed || r.matchRepo(ref, e.Provider, target)
		case r.mayContain(e, ref, target):
			out.Unresolved = append(out.Unresolved, kind+": "+selectorValue(e))
		}
	}
	if out.Assumed {
		out.Unresolved = nil
	}
	return out, nil
}

func selectorValue(e *Entry) string {
	return firstNonEmpty(e.Repo, firstNonEmpty(e.Org, e.Group))
}

func explicitPacks(hub *Hub, packs []string, known map[string]bool) (Selection, error) {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	s := newSelection(hub, known)
	s.explicit = true
	if err := s.add(packs, sourceExplicit); err != nil {
		return Selection{}, err
	}
	out, err := s.expand()
	if err != nil {
		return Selection{}, err
	}
	return Selection{Packs: out, Complete: true, Sources: s.sources}, nil
}

func (h *Hub) aliases() map[string][]string {
	out := map[string][]string{}
	if h == nil {
		return out
	}
	for name, meta := range h.Packs {
		if len(meta.Formerly) > 0 {
			out[name] = slices.Clone(meta.Formerly)
		}
	}
	return out
}

func (h *Hub) knownAliases(known map[string]bool) map[string][]string {
	out := map[string][]string{}
	if h == nil {
		return out
	}
	renamed := renames(h, known)
	for _, old := range sortedKeys(renamed) {
		name := renamed[old]
		out[name] = append(out[name], old)
	}
	return out
}
