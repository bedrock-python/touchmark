// Package decide is the pure core of touchmark: given the manifest, the packs
// selected for a target, what they ship now and what the target holds, it
// returns the state of every relevant path and the action to take.
//
// It does no I/O. `status` prints its result, `apply` executes it, and
// `plan`/`distribute` reuse it on snapshots taken through git.
package decide

import (
	"maps"
	"slices"

	"github.com/bedrock-python/touchmark/internal/provenance"
)

// State of a path in a target (see docs/concepts/ownership.md).
type State string

const (
	Missing      State = "missing"       // shipped now, absent in the target
	Current      State = "current"       // equals the version shipped now
	Outdated     State = "outdated"      // equals an older version the selected packs shipped
	Local        State = "local"         // matches no version: the target owns it
	Ignored      State = "ignored"       // listed under ignore
	Retired      State = "retired"       // no longer shipped; content came from the hub
	RetiredLocal State = "retired-local" // no longer shipped; content is the target's own
	Unsafe       State = "unsafe"        // symlink, not a regular file, or unsafe parent
	Orphaned     State = "orphaned"      // shipped only by packs the target no longer gets; report only
)

// Action to take on a path.
type Action string

const (
	Keep   Action = "keep"
	Create Action = "create"
	Update Action = "update"
	Delete Action = "delete"
	Adopt  Action = "adopt" // overwrite a local file with the current version (--adopt)
	Chmod  Action = "chmod" // content is current, only the executable bit differs
)

// Desired is the file the target should hold at Path: the version of the
// last selected pack that ships it.
type Desired struct {
	Path string
	Pack string
	OID  string
	Size int64
	Mode string // "100644" or "100755"
}

// Kind of an observation.
type Kind int

const (
	Absent Kind = iota
	Regular
	NotRegular   // symlink, directory, device, …
	UnsafeParent // an existing ancestor is a symlink or not a directory
	InvalidPath  // the path cannot be addressed safely
)

// Observation is what the target holds at a path.
type Observation struct {
	Kind Kind
	// OIDs are the blob ids the content is known under. In a committed tree,
	// the entry's blob id. In a working tree, the id of the raw bytes, plus
	// the id after the target's clean filters when the target is a git work
	// tree and the two differ (CRLF checkouts); but a file whose raw bytes
	// are the blob the index holds is known by that id alone, the blob a
	// commit holds, so that both views agree on it.
	// Regular only.
	OIDs []string
	// Mode is "100755" or "100644" when the filesystem records the
	// executable bit, "" when it does not (Windows). Regular only.
	Mode string
	// IsDir is set for NotRegular when the path is a real directory.
	IsDir bool
	// Blocker is the ancestor that makes the path unsafe (UnsafeParent), and
	// BlockerIsFile tells whether it is a regular file (vs a symlink or other).
	Blocker       string
	BlockerIsFile bool
	// Detail explains NotRegular, UnsafeParent and InvalidPath.
	Detail string
	// ActualPath is set, for anything but Absent, when the target holds the
	// entry under a spelling that differs from the path only by case: a
	// case-insensitive filesystem (NTFS, APFS) found it under the requested
	// name. It is the path as the target spells it. Two paths whose
	// observations resolve to the same spelling are the same entry.
	ActualPath string
	// CaseTwin is set, for Absent only, to an existing entry of the target
	// whose path differs from the path only by case: a case-sensitive
	// filesystem holds both spellings as different entries, and git would
	// track two paths that collide on every case-insensitive checkout.
	CaseTwin string
}

// Input is everything Decide needs.
type Input struct {
	Manifest *provenance.Manifest
	// Selected are the packs the target gets, in layering order, already
	// expanded with requires.
	Selected []string
	// Aliases maps a pack to former names whose history counts as its own.
	Aliases provenance.Aliases
	// Desired is keyed by repository path (see Layer).
	Desired map[string]Desired
	// Observed must hold an entry for every path returned by Paths. A missing
	// entry is treated as Absent.
	Observed map[string]Observation
	// Ignore patterns from the opt-in file (pathx.Match semantics, ignoring
	// case: on NTFS and APFS a case variant is the same file).
	Ignore []string
	// OptInFile is the target's opt-in file, which is never written, adopted
	// or deleted, whatever the history says; compared ignoring case. "" for
	// none.
	OptInFile string
	// Adopt globs (pathx.Match semantics) from --adopt.
	Adopt []string
	// MinEvidenceSize overrides provenance.MinEvidenceSize when > 0 (tests).
	MinEvidenceSize int64
}

// Entry is the decision for one path.
type Entry struct {
	Path   string
	State  State
	Action Action
	// Pack is the desired pack (missing, current, outdated, local, ignored)
	// or the pack whose history the content matched (retired, orphaned).
	Pack string
	// From is the observed blob id that matched history ("" if none).
	From string
	// To is the blob id to write (create, update, adopt).
	To   string
	Mode string // mode to write (create, update, adopt, chmod)
	// Detail is a short human explanation (why unsafe, which pack is gone).
	Detail string
	// AfterDeletes marks a create whose obstacle (a retired file in the way,
	// or a directory emptied by retirements) disappears only once the deletes
	// of this plan have run. The executor must re-check before writing.
	AfterDeletes bool
}

// Plan is the ordered list of decisions: every Delete first (sorted by path,
// deepest first), then everything else sorted by path.
type Plan struct {
	Entries []Entry
}

// Counts returns the number of entries per state.
func (p Plan) Counts() map[State]int {
	out := map[State]int{}
	for _, e := range p.Entries {
		out[e.State]++
	}
	return out
}

// Changes returns the entries whose action is not Keep.
func (p Plan) Changes() []Entry {
	var out []Entry
	for _, e := range p.Entries {
		if e.Action != Keep {
			out = append(out, e)
		}
	}
	return out
}

// Layer resolves what the target should hold: for every path shipped by a
// selected pack, the version of the LAST selected pack that ships it.
// Packs absent from cur are ignored (the caller validates selection).
func Layer(cur provenance.Current, selected []string) map[string]Desired {
	out := map[string]Desired{}
	for _, pack := range selected {
		for p, f := range cur[pack] {
			out[p] = Desired{Path: p, Pack: pack, OID: f.OID, Size: f.Size, Mode: f.Mode}
		}
	}
	return out
}

// Paths returns every repository path Decide needs an observation for: all
// desired paths and every path in the manifest (tombstone and orphan
// candidates), sorted.
func Paths(in Input) []string {
	set := map[string]bool{}
	for p := range in.Desired {
		set[p] = true
	}
	if in.Manifest != nil {
		for p := range in.Manifest.Paths {
			set[p] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// Decide applies the rules below to every path from Paths and returns the plan.
//
// Evidence. The history of p is what the manifest records at every path whose
// pathx.Fold equals that of p: on a case-insensitive filesystem a case
// variant is the same file, so a case-only rename in a pack keeps the file's
// history. allowed(p) is the set of OIDs in the history of p that the
// selected packs (with their aliases) shipped and whose size is at least
// MinEvidenceSize. any(p) is the same over packs that are NOT selected. An
// observation matches a set when any of its OIDs is in it. ignored(p) holds
// when p matches an Ignore pattern or is the OptInFile, both ignoring case.
//
// Desired paths (p in Desired, d = Desired[p]):
//  1. ignored(p)                              → Ignored, Keep (even if absent)
//  2. InvalidPath                             → Unsafe, Keep
//  3. UnsafeParent whose blocker is a regular file that this plan deletes
//     (retired)                               → Missing, Create, AfterDeletes
//  4. UnsafeParent otherwise                  → Unsafe, Keep
//  5. Absent with a CaseTwin that this plan deletes
//     (retired)                               → Missing, Create, AfterDeletes
//     Absent with any other CaseTwin          → Unsafe, Keep (creating p
//     would add a second spelling; the executor also refuses a create
//     while a case twin is there)
//     Absent otherwise                        → Missing, Create
//  6. NotRegular directory with at least one Retired entry beneath it in
//     this plan                               → Missing, Create, AfterDeletes
//     (Decide cannot see unrelated files in the directory; when the
//     executor finds the directory still there, it skips the create and
//     reports why, and the next run sees it as unsafe)
//  7. NotRegular otherwise                    → Unsafe, Keep
//  8. Regular and d.OID ∈ OIDs:
//     d.Mode == "100755", observed Mode == "100644" → Current, Chmod
//     otherwise                               → Current, Keep
//  9. Regular and matches allowed(p)          → Outdated, Update, From = matched
//  10. Regular, no match, pathx.MatchAny(Adopt, p) → Local, Adopt
//  11. Regular, no match                       → Local, Keep
//
// A Regular desired path whose ActualPath is set (the target spells it with
// other case) is decided the same way; the Detail names the spelling.
//
// Paths not desired now:
//   - If p and a desired q with the same Fold are the same entry (both
//     observed, resolving to the same ActualPath or path), or q is
//     InvalidPath (it may be), skip p: it is the managed file under another
//     spelling, and a case-only rename in a pack must never delete it. When
//     they are different entries (a case-sensitive filesystem holds both),
//     p is decided below, so the old spelling retires like any other path.
//   - Several such paths that are the same entry are decided once, under the
//     spelling the target uses (else the first in order).
//   - Shipped by a selected pack (or alias) at some point:
//     Absent                                   → no entry
//     ignored(p)                               → Ignored, Keep
//     NotRegular directory with a desired path beneath it, or UnsafeParent
//     whose blocker is desired (what the packs ship now) → no entry
//     InvalidPath, UnsafeParent, NotRegular    → Unsafe, Keep
//     Regular and matches allowed(p)           → Retired, Delete, From = matched
//     Regular otherwise                        → RetiredLocal, Keep
//   - Shipped only by other packs:
//     Regular, not ignored, matches any(p)     → Orphaned, Keep, Pack = that pack
//     otherwise                                → no entry
//
// For Retired and Orphaned, Pack names the first pack (in Selected order, then
// alphabetical) whose history holds the matched OID; an alias resolves to the
// current name it is an alias of.
func Decide(in Input) Plan {
	d := newDecider(in)
	var entries []Entry
	// Retirements come first: rules 3 and 6 depend on what this plan deletes.
	deleted := map[string]bool{}
	for _, p := range d.undesiredPaths() {
		e, ok := d.undesired(p)
		if !ok {
			continue
		}
		if e.Action == Delete {
			deleted[p] = true
		}
		entries = append(entries, e)
	}
	retired := slices.Sorted(maps.Keys(deleted))
	for _, p := range slices.Sorted(maps.Keys(in.Desired)) {
		entries = append(entries, d.desired(p, deleted, retired))
	}
	sortEntries(entries)
	return Plan{Entries: entries}
}
