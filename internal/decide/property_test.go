package decide

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/provenance"
)

// genUniverse holds the small world random inputs are drawn from: nested
// paths, case variants and packs with aliases, so collisions are frequent.
var (
	genPaths = []string{
		"a", "a/b", "a/b/c", "A", "x.md", "X.md", "dir", "dir/y", "dir/z/w",
		"k", "docs", "docs/a.md", "docs-old/a.md",
	}
	genCurrentPacks = []string{"p1", "p2", "p3"}
	genAllPacks     = []string{"p1", "p2", "p3", "old", "gone"}
	genIgnore       = []string{"a", "x.md", "dir/**", "*.md", "k", "docs"}
	genAdopt        = []string{"**", "a/**", "k", "X.md", "docs/*"}
)

// genSize gives every blob id one size: o0 to o3 are too small to be evidence.
func genSize(oid string) int64 {
	var n int
	_, _ = fmt.Sscanf(oid, "o%d", &n)
	if n < 4 {
		return tiny
	}
	return big
}

func genOID(r *rand.Rand) string { return fmt.Sprintf("o%d", r.IntN(12)) }

func genMode(r *rand.Rand) string {
	return []string{"100644", "100755"}[r.IntN(2)]
}

func pick(r *rand.Rand, from []string, p float64) []string {
	var out []string
	for _, s := range from {
		if r.Float64() < p {
			out = append(out, s)
		}
	}
	return out
}

// genInput draws a random Input.
func genInput(r *rand.Rand) Input {
	m := &provenance.Manifest{Version: provenance.ManifestVersion}
	for _, p := range genPaths {
		for _, pack := range genAllPacks {
			if r.Float64() < 0.35 {
				for range 1 + r.IntN(3) {
					oid := genOID(r)
					m.Add(p, pack, provenance.Version{OID: oid, Size: genSize(oid)})
				}
			}
		}
	}

	cur := provenance.Current{}
	for _, pack := range genCurrentPacks {
		cur[pack] = map[string]provenance.File{}
		for _, p := range genPaths {
			if r.Float64() < 0.25 {
				oid := genOID(r)
				cur[pack][p] = provenance.File{Pack: pack, Path: p, OID: oid, Size: genSize(oid), Mode: genMode(r)}
				if r.Float64() < 0.9 { // --worktree may ship uncommitted versions
					m.Add(p, pack, provenance.Version{OID: oid, Size: genSize(oid)})
				}
			}
		}
	}

	selected := pick(r, genCurrentPacks, 0.6)
	r.Shuffle(len(selected), func(i, j int) { selected[i], selected[j] = selected[j], selected[i] })

	aliases := provenance.Aliases{}
	if r.IntN(2) == 0 {
		aliases["p1"] = []string{"old"}
	}
	if r.IntN(3) == 0 {
		aliases["p2"] = []string{"gone", "old"}
	}

	in := Input{
		Manifest: m,
		Selected: selected,
		Aliases:  aliases,
		Desired:  Layer(cur, selected),
		Ignore:   pick(r, genIgnore, 0.15),
		Adopt:    pick(r, genAdopt, 0.25),
	}
	if r.IntN(4) == 0 {
		in.OptInFile = []string{"k", "X.md", "a"}[r.IntN(3)]
	}
	in.Observed = genObservations(r, Paths(in))
	return in
}

// genObservations draws what a target holds at every path. On a
// case-insensitive target, case variants are one entry: they share an
// observation, and ActualPath names the spelling on disk. On a
// case-sensitive one, each spelling is its own entry, and an absent path
// names an existing variant as its CaseTwin.
func genObservations(r *rand.Rand, paths []string) map[string]Observation {
	groups := map[string][]string{}
	for _, p := range paths {
		groups[pathx.Fold(p)] = append(groups[pathx.Fold(p)], p)
	}
	insensitive := r.IntN(2) == 0
	out := map[string]Observation{}
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		group := groups[key]
		if insensitive {
			o, ok := genObservation(r, group[0])
			if !ok {
				continue
			}
			spelling := group[r.IntN(len(group))]
			for _, p := range group {
				op := o
				if p != spelling && (o.Kind == Regular || o.Kind == NotRegular) {
					op.ActualPath = spelling
				}
				out[p] = op
			}
			continue
		}
		for _, p := range group {
			if o, ok := genObservation(r, p); ok {
				out[p] = o
			}
		}
		for _, p := range group {
			if out[p].Kind != Absent {
				continue
			}
			for _, q := range group {
				if o, ok := out[q]; ok && q != p && o.Kind != Absent {
					out[p] = Observation{Kind: Absent, CaseTwin: q}
					break
				}
			}
		}
	}
	return out
}

// genObservation draws what a target holds at p; ok is false for a missing
// observation, which Decide treats as Absent.
func genObservation(r *rand.Rand, p string) (Observation, bool) {
	switch n := r.IntN(100); {
	case n < 5:
		return Observation{}, false
	case n < 25:
		return Observation{Kind: Absent}, true
	case n < 70:
		o := Observation{Kind: Regular, Mode: []string{"", "100644", "100755"}[r.IntN(3)]}
		for range 1 + r.IntN(2) {
			if r.IntN(4) == 0 {
				o.OIDs = append(o.OIDs, fmt.Sprintf("local%d", r.IntN(2)))
			} else {
				o.OIDs = append(o.OIDs, genOID(r))
			}
		}
		return o, true
	case n < 82:
		return Observation{Kind: NotRegular, IsDir: r.IntN(2) == 0}, true
	case n < 95:
		blocker := genPaths[r.IntN(len(genPaths))]
		if parents := pathx.Parents(p); len(parents) > 0 && r.IntN(4) > 0 {
			blocker = parents[r.IntN(len(parents))]
		}
		return Observation{Kind: UnsafeParent, Blocker: blocker, BlockerIsFile: r.IntN(2) == 0}, true
	default:
		return Observation{Kind: InvalidPath}, true
	}
}

// refAllowed recomputes allowed(p) independently of the implementation:
// every version the selected packs shipped at p or at a case variant of p.
func refAllowed(in Input, p string) map[string]bool {
	out := map[string]bool{}
	for q := range in.Manifest.Paths {
		if pathx.Fold(q) != pathx.Fold(p) {
			continue
		}
		for _, pack := range in.Aliases.Expand(in.Selected) {
			for _, v := range in.Manifest.Versions(q, pack) {
				if v.Size >= provenance.MinEvidenceSize {
					out[v.OID] = true
				}
			}
		}
	}
	return out
}

// refEntry is the target's spelling of what is observed at p, "" for
// nothing.
func refEntry(in Input, p string) string {
	o := in.Observed[p]
	switch {
	case o.Kind == Absent:
		return ""
	case o.ActualPath != "":
		return o.ActualPath
	}
	return p
}

// refIgnored reports whether p is ignored: an ignore pattern or the opt-in
// file, ignoring case.
func refIgnored(in Input, p string) bool {
	if in.OptInFile != "" && strings.EqualFold(in.OptInFile, p) {
		return true
	}
	return slices.ContainsFunc(in.Ignore, func(pat string) bool { return pathx.Match(strings.ToLower(pat), strings.ToLower(p)) })
}

func TestDecideProperties(t *testing.T) {
	runs := 1500
	if testing.Short() {
		runs = 300
	}
	seen := map[string]int{}
	for seed := range uint64(runs) {
		r := rand.New(rand.NewPCG(seed, 0x70c4))
		in := genInput(r)
		plan := Decide(in)
		if err := checkPlan(in, plan); err != nil {
			t.Fatalf("seed %d: %v\ninput: %+v\nplan: %+v", seed, err, in, plan.Entries)
		}
		for range 20 {
			if again := Decide(in); !reflect.DeepEqual(again, plan) {
				t.Fatalf("seed %d: Decide is not deterministic:\n%+v\n%+v", seed, plan.Entries, again.Entries)
			}
		}
		for _, e := range plan.Entries {
			seen[string(e.State)+"/"+string(e.Action)]++
			if e.AfterDeletes {
				seen["after-deletes"]++
			}
			if in.Observed[e.Path].CaseTwin != "" {
				seen["case-twin/"+string(e.Action)]++
			}
			if in.Observed[e.Path].ActualPath != "" && e.Action == Update {
				seen["other-spelling/update"]++
			}
		}
	}
	// Guard against a generator that stops reaching the interesting rules.
	for _, want := range []string{
		"missing/create", "current/keep", "current/chmod", "outdated/update",
		"local/keep", "local/adopt", "ignored/keep", "retired/delete",
		"retired-local/keep", "unsafe/keep", "orphaned/keep", "after-deletes",
		"case-twin/create", "case-twin/keep", "other-spelling/update",
	} {
		if seen[want] == 0 {
			t.Errorf("no %s decision in %d random plans", want, runs)
		}
	}
}

// checkPlan verifies the safety and ordering invariants of a plan.
func checkPlan(in Input, plan Plan) error {
	if err := checkOrder(plan); err != nil {
		return err
	}
	deletes := map[string]bool{}
	deletedEntries := map[string]bool{}
	for _, e := range plan.Entries {
		if e.Action == Delete {
			deletes[e.Path] = true
			entry := refEntry(in, e.Path)
			if deletedEntries[entry] {
				return fmt.Errorf("two deletes of the entry %s", entry)
			}
			deletedEntries[entry] = true
		}
	}
	// desiredEntries holds what the target holds at desired paths.
	desiredEntries := map[string]bool{}
	for p := range in.Desired {
		if _, ok := entryAt(plan, p); !ok {
			return fmt.Errorf("desired %s has no entry", p)
		}
		if entry := refEntry(in, p); entry != "" {
			desiredEntries[entry] = true
		}
	}
	for _, e := range plan.Entries {
		if err := checkEntry(in, e, deletes, desiredEntries); err != nil {
			return fmt.Errorf("%s: %w (%+v)", e.Path, err, e)
		}
	}
	return nil
}

func checkOrder(plan Plan) error {
	seen := map[string]bool{}
	inDeletes := true
	for i, e := range plan.Entries {
		if seen[e.Path] {
			return fmt.Errorf("duplicate entry for %s", e.Path)
		}
		seen[e.Path] = true
		if e.Action == Delete && !inDeletes {
			return fmt.Errorf("delete of %s after a non-delete", e.Path)
		}
		inDeletes = e.Action == Delete
		if i == 0 {
			continue
		}
		prev := plan.Entries[i-1]
		if prev.Action == Delete && e.Action == Delete {
			pd, ed := strings.Count(prev.Path, "/"), strings.Count(e.Path, "/")
			if pd < ed || (pd == ed && prev.Path > e.Path) {
				return fmt.Errorf("deletes out of order: %s before %s", prev.Path, e.Path)
			}
		}
		if prev.Action != Delete && e.Action != Delete && prev.Path > e.Path {
			return fmt.Errorf("entries out of order: %s before %s", prev.Path, e.Path)
		}
	}
	return nil
}

func checkEntry(in Input, e Entry, deletes, desiredEntries map[string]bool) error {
	o, ok := in.Observed[e.Path]
	if !ok {
		o = Observation{Kind: Absent}
	}
	want, desired := in.Desired[e.Path]
	ignored := refIgnored(in, e.Path)
	allowed := refAllowed(in, e.Path)

	if ignored && (e.Action != Keep || e.State != Ignored) {
		return fmt.Errorf("ignored path is %s/%s", e.State, e.Action)
	}
	if !desired && desiredEntries[refEntry(in, e.Path)] {
		return fmt.Errorf("the entry of a desired path has an entry under another spelling")
	}
	switch e.Action {
	case Delete:
		if desired {
			return fmt.Errorf("deletes a desired path")
		}
		if o.Kind != Regular || !slices.Contains(o.OIDs, e.From) || !allowed[e.From] {
			return fmt.Errorf("deletes without evidence")
		}
	case Update:
		if !desired || o.Kind != Regular || !slices.Contains(o.OIDs, e.From) || !allowed[e.From] {
			return fmt.Errorf("updates without evidence")
		}
		if slices.Contains(o.OIDs, want.OID) || e.To != want.OID {
			return fmt.Errorf("updates a current file or to the wrong version")
		}
	case Adopt:
		if !desired || e.State != Local || o.Kind != Regular || !pathx.MatchAny(in.Adopt, e.Path) || ignored {
			return fmt.Errorf("adopts outside --adopt or a non-local file")
		}
		if slices.ContainsFunc(o.OIDs, func(id string) bool { return allowed[id] || id == want.OID }) {
			return fmt.Errorf("adopts a file the hub owns")
		}
	case Chmod:
		if !desired || o.Kind != Regular || !slices.Contains(o.OIDs, want.OID) || o.Mode != "100644" || want.Mode != "100755" {
			return fmt.Errorf("chmod without a current file lacking the executable bit")
		}
	case Create:
		if !desired || e.To != want.OID {
			return fmt.Errorf("creates an undesired path or the wrong version")
		}
		if err := checkCreate(e, o, deletes); err != nil {
			return err
		}
	case Keep:
	default:
		return fmt.Errorf("unknown action")
	}
	return nil
}

// checkCreate verifies that a create writes to an absent path, or to one whose
// obstacle this plan deletes.
func checkCreate(e Entry, o Observation, deletes map[string]bool) error {
	switch {
	case o.Kind == Absent && o.CaseTwin == "" && !e.AfterDeletes:
		return nil
	case o.Kind == Absent && o.CaseTwin != "":
		if !e.AfterDeletes || !deletes[o.CaseTwin] {
			return fmt.Errorf("creates a second spelling next to %s", o.CaseTwin)
		}
		return nil
	case o.Kind == UnsafeParent && e.AfterDeletes:
		if !o.BlockerIsFile || !deletes[o.Blocker] {
			return fmt.Errorf("creates behind a blocker the plan does not delete")
		}
		return nil
	case o.Kind == NotRegular && e.AfterDeletes:
		for p := range deletes {
			if strings.HasPrefix(p, e.Path+"/") {
				return nil
			}
		}
		return fmt.Errorf("creates over a directory with nothing deleted beneath")
	default:
		return fmt.Errorf("creates over a %v observation", o.Kind)
	}
}

// TestPathsProperties checks that Paths covers every desired and manifest
// path exactly once, sorted.
func TestPathsProperties(t *testing.T) {
	for seed := range uint64(500) {
		r := rand.New(rand.NewPCG(seed, 0x9a7b))
		in := genInput(r)
		got := Paths(in)
		if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
			t.Fatalf("seed %d: not sorted and unique: %q", seed, got)
		}
		for p := range in.Desired {
			if !slices.Contains(got, p) {
				t.Fatalf("seed %d: desired %s missing", seed, p)
			}
		}
		for p := range in.Manifest.Paths {
			if !slices.Contains(got, p) {
				t.Fatalf("seed %d: manifest path %s missing", seed, p)
			}
		}
	}
}
