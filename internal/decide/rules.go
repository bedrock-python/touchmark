package decide

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/provenance"
)

const (
	modeFile = "100644"
	modeExec = "100755"

	detailIgnored = "listed under ignore"
	detailOptIn   = "the target's opt-in file"
)

// decider holds what Decide derives once from its Input.
type decider struct {
	in Input
	// min is the effective MinEvidenceSize.
	min int64
	// expanded holds the selected packs and all their aliases.
	expanded map[string]bool
	// currentName maps an alias to the current pack name it belongs to.
	currentName map[string]string
	// variants maps a case-folded path to the manifest paths that fold to it.
	variants map[string][]string
	// desiredByFold maps a case-folded path to the desired paths that fold to it.
	desiredByFold map[string][]string
	// desiredPaths holds the desired paths, sorted.
	desiredPaths []string
	// ignore matches the ignore patterns, ignoring case; adopt the --adopt
	// globs.
	ignore, adopt *pathx.Matcher
	// optIn is the folded opt-in file name, "" for none.
	optIn string
}

func newDecider(in Input) *decider {
	d := &decider{
		in:            in,
		min:           provenance.MinEvidenceSize,
		expanded:      map[string]bool{},
		currentName:   map[string]string{},
		variants:      map[string][]string{},
		desiredByFold: map[string][]string{},
		desiredPaths:  slices.Sorted(maps.Keys(in.Desired)),
		ignore:        pathx.NewMatcher(in.Ignore, true),
		adopt:         pathx.NewMatcher(in.Adopt, false),
	}
	if in.MinEvidenceSize > 0 {
		d.min = in.MinEvidenceSize
	}
	if in.OptInFile != "" {
		d.optIn = pathx.Fold(in.OptInFile)
	}
	for _, pack := range in.Aliases.Expand(in.Selected) {
		d.expanded[pack] = true
	}
	// Sorted keys make the choice deterministic if an old name is claimed twice.
	for _, name := range slices.Sorted(maps.Keys(in.Aliases)) {
		for _, old := range in.Aliases[name] {
			if _, ok := d.currentName[old]; !ok {
				d.currentName[old] = name
			}
		}
	}
	if in.Manifest != nil {
		for _, p := range slices.Sorted(maps.Keys(in.Manifest.Paths)) {
			f := pathx.Fold(p)
			d.variants[f] = append(d.variants[f], p)
		}
	}
	for _, p := range d.desiredPaths {
		f := pathx.Fold(p)
		d.desiredByFold[f] = append(d.desiredByFold[f], p)
	}
	return d
}

// observe returns the observation at p; a missing one is Absent.
func (d *decider) observe(p string) Observation {
	if o, ok := d.in.Observed[p]; ok {
		return o
	}
	return Observation{Kind: Absent}
}

// entryOf returns the target's spelling of the entry observed at p, or ""
// when nothing is there.
func (d *decider) entryOf(p string) string {
	o := d.observe(p)
	switch {
	case o.Kind == Absent:
		return ""
	case o.ActualPath != "":
		return o.ActualPath
	}
	return p
}

// ignored reports whether p is listed under ignore or is the opt-in file,
// ignoring case, and why.
func (d *decider) ignored(p string) (string, bool) {
	switch {
	case d.optIn != "" && pathx.Fold(p) == d.optIn:
		return detailOptIn, true
	case d.ignore.Match(p):
		return detailIgnored, true
	}
	return "", false
}

// history returns what the manifest records at p and at its case variants,
// by pack.
func (d *decider) history(p string) map[string][]provenance.Version {
	if d.in.Manifest == nil {
		return nil
	}
	paths := d.variants[pathx.Fold(p)]
	if len(paths) == 1 {
		return d.in.Manifest.Paths[paths[0]]
	}
	out := map[string][]provenance.Version{}
	for _, q := range paths {
		for pack, versions := range d.in.Manifest.Paths[q] {
			out[pack] = append(out[pack], versions...)
		}
	}
	return out
}

// evidence returns the ids of the versions at p large enough to prove
// ownership: over the selected packs and their aliases (allowed) when
// selected is true, over every other pack (any) otherwise.
func (d *decider) evidence(p string, selected bool) map[string]bool {
	out := map[string]bool{}
	for pack, versions := range d.history(p) {
		if d.expanded[pack] != selected {
			continue
		}
		for _, v := range versions {
			if v.Size >= d.min {
				out[v.OID] = true
			}
		}
	}
	return out
}

// shippedBySelected reports whether a selected pack or one of its aliases
// ever shipped anything at p, whatever the size.
func (d *decider) shippedBySelected(p string) bool {
	for pack, versions := range d.history(p) {
		if d.expanded[pack] && len(versions) > 0 {
			return true
		}
	}
	return false
}

// attribute names the pack whose history at p satisfies holds: the first
// selected pack (by its own name or an alias), then the other packs
// alphabetically, an alias resolved to its current name. It returns "" when
// no pack qualifies.
func (d *decider) attribute(p string, holds func([]provenance.Version) bool) string {
	hist := d.history(p)
	has := func(pack string) bool { return holds(hist[pack]) }
	for _, s := range d.in.Selected {
		if has(s) || slices.ContainsFunc(d.in.Aliases[s], has) {
			return s
		}
	}
	// Every selected pack and alias was tried above, so what matches here is
	// another pack.
	for _, pack := range slices.Sorted(maps.Keys(hist)) {
		if has(pack) {
			return d.current(pack)
		}
	}
	return ""
}

// current resolves an alias to the current pack name.
func (d *decider) current(pack string) string {
	if name, ok := d.currentName[pack]; ok {
		return name
	}
	return pack
}

// holdsOID matches a history that contains oid.
func holdsOID(oid string) func([]provenance.Version) bool {
	return func(vs []provenance.Version) bool {
		return slices.ContainsFunc(vs, func(v provenance.Version) bool { return v.OID == oid })
	}
}

// holdsAny matches a non-empty history.
func holdsAny(vs []provenance.Version) bool { return len(vs) > 0 }

// match returns the first observed id that is in set.
func match(o Observation, set map[string]bool) (string, bool) {
	for _, id := range o.OIDs {
		if set[id] {
			return id, true
		}
	}
	return "", false
}

// undesiredPaths returns the manifest paths that are not desired now, in
// order, without those that are the same entry as a desired path under
// another spelling, and with one path per entry the target holds.
func (d *decider) undesiredPaths() []string {
	if d.in.Manifest == nil {
		return nil
	}
	var out []string
	chosen := map[string]int{} // entry → index in out of the path deciding it
	for _, p := range slices.Sorted(maps.Keys(d.in.Manifest.Paths)) {
		if _, ok := d.in.Desired[p]; ok {
			continue
		}
		entry := d.entryOf(p)
		if entry == "" {
			continue // nothing there: no entry either way
		}
		if d.sameAsDesired(p, entry) {
			continue
		}
		if i, ok := chosen[entry]; ok {
			// Prefer the spelling the target uses.
			if p == entry {
				out[i] = p
			}
			continue
		}
		chosen[entry] = len(out)
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// sameAsDesired reports whether entry, observed at the undesired path p, is
// or may be the entry observed at a desired case variant of p. A variant
// that could not be inspected may be the same file, so it counts.
func (d *decider) sameAsDesired(p, entry string) bool {
	for _, q := range d.desiredByFold[pathx.Fold(p)] {
		if d.entryOf(q) == entry || d.observe(q).Kind == InvalidPath {
			return true
		}
	}
	return false
}

// undesired decides a path no selected pack ships now. ok is false when the
// path needs no entry.
func (d *decider) undesired(p string) (e Entry, ok bool) {
	o := d.observe(p)
	if d.shippedBySelected(p) {
		return d.retirement(p, o)
	}
	return d.orphan(p, o)
}

// retirement decides a path a selected pack (or alias) shipped at some point.
func (d *decider) retirement(p string, o Observation) (Entry, bool) {
	if o.Kind == Absent || d.holdsShipped(o, p) {
		return Entry{}, false
	}
	e := Entry{Path: p, Action: Keep}
	if why, ok := d.ignored(p); ok {
		e.State, e.Detail = Ignored, why
	} else if o.Kind != Regular {
		e.State, e.Detail = Unsafe, unsafeDetail(o)
	} else if id, ok := match(o, d.evidence(p, true)); ok {
		e.State, e.Action, e.From = Retired, Delete, id
		e.Pack = d.attribute(p, holdsOID(id))
		e.Detail = "no longer shipped by pack " + e.Pack
		return e, true
	} else {
		e.State, e.Detail = RetiredLocal, "no longer shipped; the content is the target's own"
	}
	e.Pack = d.attribute(p, holdsAny)
	return e, true
}

// holdsShipped reports whether what is in the way at the retired path p is
// what the packs ship now: a directory holding a desired path, or a desired
// file as its parent. There is nothing to report then.
func (d *decider) holdsShipped(o Observation, p string) bool {
	switch o.Kind {
	case NotRegular:
		return o.IsDir && hasBeneath(d.desiredPaths, p)
	case UnsafeParent:
		_, desired := d.in.Desired[o.Blocker]
		return o.BlockerIsFile && desired
	}
	return false
}

// orphan decides a path shipped only by packs the target does not get.
func (d *decider) orphan(p string, o Observation) (Entry, bool) {
	if _, ignored := d.ignored(p); o.Kind != Regular || ignored {
		return Entry{}, false
	}
	id, ok := match(o, d.evidence(p, false))
	if !ok {
		return Entry{}, false
	}
	pack := d.attribute(p, holdsOID(id))
	return Entry{
		Path:   p,
		State:  Orphaned,
		Action: Keep,
		Pack:   pack,
		From:   id,
		Detail: "pack " + pack + " is no longer selected",
	}, true
}

// desired decides a path a selected pack ships now. deleted holds the paths
// this plan deletes, and retired is the same set sorted.
func (d *decider) desired(p string, deleted map[string]bool, retired []string) Entry {
	want := d.in.Desired[p]
	o := d.observe(p)
	e := Entry{Path: p, Pack: want.Pack, Action: Keep}
	create := func(afterDeletes bool, detail string) Entry {
		e.State, e.Action, e.To, e.Mode = Missing, Create, want.OID, want.Mode
		e.AfterDeletes, e.Detail = afterDeletes, detail
		return e
	}
	unsafe := func() Entry {
		e.State, e.Detail = Unsafe, unsafeDetail(o)
		return e
	}
	if why, ok := d.ignored(p); ok {
		e.State, e.Detail = Ignored, why
		return e
	}
	switch o.Kind {
	case InvalidPath:
		return unsafe()
	case UnsafeParent:
		if o.BlockerIsFile && deleted[o.Blocker] {
			return create(true, "after deleting retired "+o.Blocker)
		}
		return unsafe()
	case Absent:
		switch {
		case o.CaseTwin == "":
			return create(false, "")
		case deleted[o.CaseTwin]:
			return create(true, "after deleting retired "+o.CaseTwin)
		}
		e.State = Unsafe
		e.Detail = "differs only by case from " + o.CaseTwin + ", which the target holds; the two would collide on case-insensitive filesystems"
		return e
	case NotRegular:
		if o.IsDir && hasBeneath(retired, p) {
			return create(true, "after deleting retired files beneath it")
		}
		return unsafe()
	case Regular:
		return d.regular(e, want, o)
	default:
		return unsafe()
	}
}

// regular applies rules 8 to 11 to a regular file at a desired path.
func (d *decider) regular(e Entry, want Desired, o Observation) Entry {
	if o.ActualPath != "" {
		e.Detail = "the target spells it " + o.ActualPath
	}
	if slices.Contains(o.OIDs, want.OID) {
		e.State, e.From = Current, want.OID
		if want.Mode == modeExec && o.Mode == modeFile {
			e.Action, e.Mode = Chmod, want.Mode
			e.Detail = joinDetail("executable bit missing", e.Detail)
		}
		return e
	}
	if id, ok := match(o, d.evidence(e.Path, true)); ok {
		e.State, e.Action, e.From, e.To, e.Mode = Outdated, Update, id, want.OID, want.Mode
		return e
	}
	e.State = Local
	if d.adopt.Match(e.Path) {
		e.Action, e.To, e.Mode = Adopt, want.OID, want.Mode
	}
	return e
}

// joinDetail joins two explanations, either of which may be empty.
func joinDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// hasBeneath reports whether sorted holds a path strictly beneath dir.
func hasBeneath(sorted []string, dir string) bool {
	prefix := dir + "/"
	i, _ := slices.BinarySearch(sorted, prefix)
	return i < len(sorted) && strings.HasPrefix(sorted[i], prefix)
}

// unsafeDetail explains why an observation is unsafe.
func unsafeDetail(o Observation) string {
	if o.Detail != "" {
		return o.Detail
	}
	switch o.Kind {
	case InvalidPath:
		return "invalid path"
	case UnsafeParent:
		if o.Blocker != "" {
			return "unsafe parent " + o.Blocker
		}
		return "unsafe parent"
	case NotRegular:
		if o.IsDir {
			return "a directory"
		}
		return "not a regular file"
	default:
		return "unknown observation"
	}
}

// sortEntries orders a plan: every Delete first, deepest path first and then
// by path, then everything else by path. Paths are unique within a plan, so
// the order is total.
func sortEntries(es []Entry) {
	slices.SortFunc(es, func(a, b Entry) int {
		ad, bd := a.Action == Delete, b.Action == Delete
		switch {
		case ad && !bd:
			return -1
		case bd && !ad:
			return 1
		case ad:
			if c := cmp.Compare(depth(b.Path), depth(a.Path)); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Path, b.Path)
	})
}

// depth is the number of separators in a repository path.
func depth(p string) int { return strings.Count(p, "/") }
