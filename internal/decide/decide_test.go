package decide

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/provenance"
)

const (
	big  = 100 // at least provenance.MinEvidenceSize
	tiny = 10  // below provenance.MinEvidenceSize
)

// hist is one version a pack shipped at a path.
type hist struct {
	path, pack, oid string
	size            int64
}

func manifestOf(hs ...hist) *provenance.Manifest {
	m := &provenance.Manifest{Version: provenance.ManifestVersion}
	for _, h := range hs {
		m.Add(h.path, h.pack, provenance.Version{OID: h.oid, Size: h.size})
	}
	return m
}

func reg(oids ...string) Observation { return Observation{Kind: Regular, OIDs: oids} }

func regMode(mode string, oids ...string) Observation {
	return Observation{Kind: Regular, OIDs: oids, Mode: mode}
}

func dir() Observation { return Observation{Kind: NotRegular, IsDir: true} }

func fileBlocker(blocker string) Observation {
	return Observation{Kind: UnsafeParent, Blocker: blocker, BlockerIsFile: true}
}

// entryAt returns the entry for path, if any.
func entryAt(plan Plan, path string) (Entry, bool) {
	for _, e := range plan.Entries {
		if e.Path == path {
			return e, true
		}
	}
	return Entry{}, false
}

// noDetail drops the human explanation, which tests check separately.
func noDetail(e Entry) Entry {
	e.Detail = ""
	return e
}

func pathsOf(plan Plan) []string {
	var out []string
	for _, e := range plan.Entries {
		out = append(out, e.Path)
	}
	return out
}

func TestLayer(t *testing.T) {
	cur := provenance.Current{
		"base": {
			"ruff.toml": {Pack: "base", Path: "ruff.toml", OID: "b", Size: big, Mode: "100644"},
			"AGENTS.md": {Pack: "base", Path: "AGENTS.md", OID: "a", Size: big, Mode: "100644"},
		},
		"python": {
			"ruff.toml":  {Pack: "python", Path: "ruff.toml", OID: "p", Size: 70, Mode: "100755"},
			"pyproj.cfg": {Pack: "python", Path: "pyproj.cfg", OID: "c", Size: big, Mode: "100644"},
		},
		"unused": {"u.md": {Pack: "unused", Path: "u.md", OID: "u", Size: big, Mode: "100644"}},
	}
	got := Layer(cur, []string{"base", "absent", "python"})
	want := map[string]Desired{
		"AGENTS.md":  {Path: "AGENTS.md", Pack: "base", OID: "a", Size: big, Mode: "100644"},
		"ruff.toml":  {Path: "ruff.toml", Pack: "python", OID: "p", Size: 70, Mode: "100755"},
		"pyproj.cfg": {Path: "pyproj.cfg", Pack: "python", OID: "c", Size: big, Mode: "100644"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Layer = %#v\nwant %#v", got, want)
	}

	// Reversing the order makes base win the shared path.
	if got := Layer(cur, []string{"python", "base"})["ruff.toml"].Pack; got != "base" {
		t.Errorf("reversed order: ruff.toml from %q, want base", got)
	}
	if got := Layer(cur, nil); got == nil || len(got) != 0 {
		t.Errorf("empty selection: got %#v, want an empty map", got)
	}
}

func TestPaths(t *testing.T) {
	in := Input{
		Manifest: manifestOf(
			hist{"AGENTS.md", "agents", "a1", big},
			hist{"old/x.md", "agents", "x1", big},
			hist{"legacy.md", "gone", "l1", big},
		),
		Desired: map[string]Desired{
			"AGENTS.md": {Path: "AGENTS.md"},
			"Agents.md": {Path: "Agents.md"},
			"new.md":    {Path: "new.md"},
		},
	}
	want := []string{"AGENTS.md", "Agents.md", "legacy.md", "new.md", "old/x.md"}
	if got := Paths(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Paths = %q, want %q", got, want)
	}

	in.Manifest = nil
	want = []string{"AGENTS.md", "Agents.md", "new.md"}
	if got := Paths(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Paths without manifest = %q, want %q", got, want)
	}
	if got := Paths(Input{}); len(got) != 0 {
		t.Errorf("Paths(empty) = %q, want none", got)
	}
}

// desiredBase is a target that gets pack agents, which ships AGENTS.md now
// (version "cur") and shipped "v1" and the tiny "small" before.
func desiredBase() Input {
	return Input{
		Manifest: manifestOf(
			hist{"AGENTS.md", "agents", "v1", big},
			hist{"AGENTS.md", "agents", "small", tiny},
			hist{"AGENTS.md", "agents", "cur", big},
			hist{"AGENTS.md", "other", "o1", big},
		),
		Selected: []string{"agents"},
		Desired: map[string]Desired{
			"AGENTS.md": {Path: "AGENTS.md", Pack: "agents", OID: "cur", Size: big, Mode: "100644"},
		},
		Observed: map[string]Observation{"AGENTS.md": reg("cur")},
	}
}

func TestDecideDesired(t *testing.T) {
	const p = "AGENTS.md"
	keep := func(s State) Entry { return Entry{Path: p, State: s, Action: Keep, Pack: "agents"} }
	create := Entry{Path: p, State: Missing, Action: Create, Pack: "agents", To: "cur", Mode: "100644"}
	current := Entry{Path: p, State: Current, Action: Keep, Pack: "agents", From: "cur"}
	outdated := func(from string) Entry {
		return Entry{Path: p, State: Outdated, Action: Update, Pack: "agents", From: from, To: "cur", Mode: "100644"}
	}
	adopt := Entry{Path: p, State: Local, Action: Adopt, Pack: "agents", To: "cur", Mode: "100644"}

	tests := []struct {
		name string
		mod  func(in *Input)
		want Entry
	}{
		// Rule 1: ignore beats everything.
		{"1 ignored present local", func(in *Input) {
			in.Ignore = []string{p}
			in.Observed[p] = reg("mine")
		}, keep(Ignored)},
		{"1 ignored absent", func(in *Input) {
			in.Ignore = []string{p}
			delete(in.Observed, p)
		}, keep(Ignored)},
		{"1 ignored beats outdated", func(in *Input) {
			in.Ignore = []string{"*.md"}
			in.Observed[p] = reg("v1")
		}, keep(Ignored)},
		{"1 ignored beats invalid path", func(in *Input) {
			in.Ignore = []string{p}
			in.Observed[p] = Observation{Kind: InvalidPath}
		}, keep(Ignored)},
		{"1 ignored beats unsafe parent", func(in *Input) {
			in.Ignore = []string{"**"}
			in.Observed[p] = Observation{Kind: UnsafeParent, Blocker: "x"}
		}, keep(Ignored)},
		{"1 ignored beats adopt", func(in *Input) {
			in.Ignore = []string{p}
			in.Adopt = []string{"**"}
			in.Observed[p] = reg("mine")
		}, keep(Ignored)},
		{"1 ignored current", func(in *Input) {
			in.Ignore = []string{p}
		}, keep(Ignored)},

		// Rules 2 to 7: what is not a regular file.
		{"2 invalid path", func(in *Input) {
			in.Observed[p] = Observation{Kind: InvalidPath, Detail: "bad"}
		}, keep(Unsafe)},
		{"4 unsafe parent symlink", func(in *Input) {
			in.Observed[p] = Observation{Kind: UnsafeParent, Blocker: "x"}
		}, keep(Unsafe)},
		{"4 unsafe parent file not deleted", func(in *Input) {
			in.Observed[p] = fileBlocker("x")
		}, keep(Unsafe)},
		{"5 absent", func(in *Input) {
			in.Observed[p] = Observation{Kind: Absent}
		}, create},
		{"5 missing observation is absent", func(in *Input) {
			delete(in.Observed, p)
		}, create},
		{"5 adopt does not apply to missing", func(in *Input) {
			in.Adopt = []string{"**"}
			delete(in.Observed, p)
		}, create},
		{"7 directory with nothing retired beneath", func(in *Input) {
			in.Observed[p] = dir()
		}, keep(Unsafe)},
		{"7 symlink", func(in *Input) {
			in.Observed[p] = Observation{Kind: NotRegular, Detail: "symlink"}
		}, keep(Unsafe)},
		{"unknown kind is unsafe", func(in *Input) {
			in.Observed[p] = Observation{Kind: Kind(42)}
		}, keep(Unsafe)},

		// Rule 8: current.
		{"8 current", nil, current},
		{"8 current through the filtered id", func(in *Input) {
			in.Observed[p] = reg("raw-crlf", "cur")
		}, current},
		{"8 executable bit missing", func(in *Input) {
			d := in.Desired[p]
			d.Mode = "100755"
			in.Desired[p] = d
			in.Observed[p] = regMode("100644", "cur")
		}, Entry{Path: p, State: Current, Action: Chmod, Pack: "agents", From: "cur", Mode: "100755"}},
		{"8 executable bit unknown", func(in *Input) {
			d := in.Desired[p]
			d.Mode = "100755"
			in.Desired[p] = d
			in.Observed[p] = regMode("", "cur")
		}, current},
		{"8 executable bit present", func(in *Input) {
			d := in.Desired[p]
			d.Mode = "100755"
			in.Desired[p] = d
			in.Observed[p] = regMode("100755", "cur")
		}, current},
		{"8 extra executable bit is kept", func(in *Input) {
			in.Observed[p] = regMode("100755", "cur")
		}, current},
		{"8 tiny current version still equals", func(in *Input) {
			in.Desired[p] = Desired{Path: p, Pack: "agents", OID: "small", Size: tiny, Mode: "100644"}
			in.Observed[p] = reg("small")
		}, Entry{Path: p, State: Current, Action: Keep, Pack: "agents", From: "small"}},
		{"8 current version absent from the manifest", func(in *Input) {
			in.Desired[p] = Desired{Path: p, Pack: "agents", OID: "worktree", Size: big, Mode: "100644"}
			in.Observed[p] = reg("worktree")
		}, Entry{Path: p, State: Current, Action: Keep, Pack: "agents", From: "worktree"}},

		// Rule 9: outdated.
		{"9 outdated", func(in *Input) {
			in.Observed[p] = reg("v1")
		}, outdated("v1")},
		{"9 outdated through the filtered id", func(in *Input) {
			in.Observed[p] = reg("raw-crlf", "v1")
		}, outdated("v1")},
		{"9 adopt does not change outdated", func(in *Input) {
			in.Adopt = []string{"**"}
			in.Observed[p] = reg("v1")
		}, outdated("v1")},
		{"9 tiny history is no evidence", func(in *Input) {
			in.Observed[p] = reg("small")
		}, keep(Local)},
		{"9 MinEvidenceSize override", func(in *Input) {
			in.MinEvidenceSize = 5
			in.Observed[p] = reg("small")
		}, outdated("small")},
		{"9 MinEvidenceSize override raises the bar", func(in *Input) {
			in.MinEvidenceSize = big + 1
			in.Observed[p] = reg("v1")
		}, keep(Local)},
		{"9 unselected pack history is no evidence", func(in *Input) {
			in.Observed[p] = reg("o1")
		}, keep(Local)},

		// Rules 10 and 11: local.
		{"10 adopt", func(in *Input) {
			in.Adopt = []string{p}
			in.Observed[p] = reg("mine")
		}, adopt},
		{"10 adopt glob", func(in *Input) {
			in.Adopt = []string{"docs", "*.md"}
			in.Observed[p] = reg("mine")
		}, adopt},
		{"11 adopt glob does not match", func(in *Input) {
			in.Adopt = []string{"docs/**"}
			in.Observed[p] = reg("mine")
		}, keep(Local)},
		{"11 local", func(in *Input) {
			in.Observed[p] = reg("mine")
		}, keep(Local)},
		{"11 regular without ids", func(in *Input) {
			in.Observed[p] = reg()
		}, keep(Local)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := desiredBase()
			if tt.mod != nil {
				tt.mod(&in)
			}
			plan := Decide(in)
			if len(plan.Entries) != 1 {
				t.Fatalf("got %d entries, want 1: %+v", len(plan.Entries), plan.Entries)
			}
			got := plan.Entries[0]
			if noDetail(got) != tt.want {
				t.Errorf("got  %+v\nwant %+v", noDetail(got), tt.want)
			}
			if got.State == Unsafe && got.Detail == "" {
				t.Errorf("unsafe entry without detail: %+v", got)
			}
		})
	}
}

func TestUnsafeDetailComesFromObservation(t *testing.T) {
	in := desiredBase()
	in.Observed["AGENTS.md"] = Observation{Kind: UnsafeParent, Blocker: ".claude", Detail: "parent .claude is a symlink"}
	e := Decide(in).Entries[0]
	if e.State != Unsafe || e.Detail != "parent .claude is a symlink" {
		t.Errorf("got %+v", e)
	}
}

// undesiredBase is a target that gets pack agents. AGENTS.md is current.
// agents shipped OLD.md before; pack gone, which the target no longer gets,
// shipped LEGACY.md.
func undesiredBase() Input {
	in := desiredBase()
	in.Manifest = manifestOf(
		hist{"AGENTS.md", "agents", "cur", big},
		hist{"OLD.md", "agents", "old1", big},
		hist{"OLD.md", "agents", "oldtiny", tiny},
		hist{"LEGACY.md", "gone", "gone1", big},
		hist{"LEGACY.md", "gone", "gonetiny", tiny},
	)
	return in
}

func TestDecideUndesired(t *testing.T) {
	tests := []struct {
		name string
		path string
		mod  func(in *Input)
		want *Entry // nil: no entry
	}{
		{"retired absent", "OLD.md", nil, nil},
		{"retired explicitly absent", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = Observation{Kind: Absent}
		}, nil},
		{"retired", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = reg("old1")
		}, &Entry{Path: "OLD.md", State: Retired, Action: Delete, Pack: "agents", From: "old1"}},
		{"retired through the filtered id", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = reg("raw-crlf", "old1")
		}, &Entry{Path: "OLD.md", State: Retired, Action: Delete, Pack: "agents", From: "old1"}},
		{"retired under ignore", "OLD.md", func(in *Input) {
			in.Ignore = []string{"OLD.md"}
			in.Observed["OLD.md"] = reg("old1")
		}, &Entry{Path: "OLD.md", State: Ignored, Action: Keep, Pack: "agents"}},
		{"retired under ignore, absent", "OLD.md", func(in *Input) {
			in.Ignore = []string{"OLD.md"}
		}, nil},
		{"retired local", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = reg("mine")
		}, &Entry{Path: "OLD.md", State: RetiredLocal, Action: Keep, Pack: "agents"}},
		{"retired tiny version is local", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = reg("oldtiny")
		}, &Entry{Path: "OLD.md", State: RetiredLocal, Action: Keep, Pack: "agents"}},
		{"retired not regular", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = Observation{Kind: NotRegular, Detail: "symlink"}
		}, &Entry{Path: "OLD.md", State: Unsafe, Action: Keep, Pack: "agents"}},
		{"retired unsafe parent", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = fileBlocker("x")
		}, &Entry{Path: "OLD.md", State: Unsafe, Action: Keep, Pack: "agents"}},
		{"retired invalid path", "OLD.md", func(in *Input) {
			in.Observed["OLD.md"] = Observation{Kind: InvalidPath}
		}, &Entry{Path: "OLD.md", State: Unsafe, Action: Keep, Pack: "agents"}},
		{"retired history of an unselected pack is no evidence", "OLD.md", func(in *Input) {
			in.Manifest.Add("OLD.md", "gone", provenance.Version{OID: "g", Size: big})
			in.Observed["OLD.md"] = reg("g")
		}, &Entry{Path: "OLD.md", State: RetiredLocal, Action: Keep, Pack: "agents"}},

		{"orphaned", "LEGACY.md", func(in *Input) {
			in.Observed["LEGACY.md"] = reg("gone1")
		}, &Entry{Path: "LEGACY.md", State: Orphaned, Action: Keep, Pack: "gone", From: "gone1"}},
		{"orphaned through the filtered id", "LEGACY.md", func(in *Input) {
			in.Observed["LEGACY.md"] = reg("raw", "gone1")
		}, &Entry{Path: "LEGACY.md", State: Orphaned, Action: Keep, Pack: "gone", From: "gone1"}},
		{"orphaned tiny version", "LEGACY.md", func(in *Input) {
			in.Observed["LEGACY.md"] = reg("gonetiny")
		}, nil},
		{"orphan candidate local", "LEGACY.md", func(in *Input) {
			in.Observed["LEGACY.md"] = reg("mine")
		}, nil},
		{"orphan candidate ignored", "LEGACY.md", func(in *Input) {
			in.Ignore = []string{"LEGACY.md"}
			in.Observed["LEGACY.md"] = reg("gone1")
		}, nil},
		{"orphan candidate absent", "LEGACY.md", nil, nil},
		{"orphan candidate not regular", "LEGACY.md", func(in *Input) {
			in.Observed["LEGACY.md"] = dir()
		}, nil},
		{"orphan candidate invalid", "LEGACY.md", func(in *Input) {
			in.Observed["LEGACY.md"] = Observation{Kind: InvalidPath}
		}, nil},
		{"orphaned under a renamed pack", "LEGACY.md", func(in *Input) {
			in.Aliases = provenance.Aliases{"renamed": {"gone"}}
			in.Observed["LEGACY.md"] = reg("gone1")
		}, &Entry{Path: "LEGACY.md", State: Orphaned, Action: Keep, Pack: "renamed", From: "gone1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := undesiredBase()
			if tt.mod != nil {
				tt.mod(&in)
			}
			plan := Decide(in)
			got, ok := entryAt(plan, tt.path)
			switch {
			case tt.want == nil && ok:
				t.Fatalf("got %+v, want no entry", got)
			case tt.want == nil:
			case !ok:
				t.Fatalf("no entry, want %+v", *tt.want)
			case noDetail(got) != *tt.want:
				t.Errorf("got  %+v\nwant %+v", noDetail(got), *tt.want)
			}
			if ok && (got.State == Unsafe || got.State == Orphaned || got.State == Retired) && got.Detail == "" {
				t.Errorf("%s entry without detail: %+v", got.State, got)
			}
			if a, ok := entryAt(plan, "AGENTS.md"); !ok || a.State != Current {
				t.Errorf("AGENTS.md: got %+v, want current", a)
			}
		})
	}
}

func TestOrphanedDetailNamesPack(t *testing.T) {
	in := undesiredBase()
	in.Observed["LEGACY.md"] = reg("gone1")
	e, _ := entryAt(Decide(in), "LEGACY.md")
	if !strings.Contains(e.Detail, "gone") {
		t.Errorf("detail %q does not name pack gone", e.Detail)
	}
}

// caseRename is a pack that shipped docs/Guide.md (v1) and now ships
// docs/guide.md (v2), with the target's observations of both spellings.
func caseRename(guide, lower Observation) Input {
	return Input{
		Manifest: manifestOf(
			hist{"docs/Guide.md", "base", "v1", big},
			hist{"docs/guide.md", "base", "v2", big},
		),
		Selected: []string{"base"},
		Desired: map[string]Desired{
			"docs/guide.md": {Path: "docs/guide.md", Pack: "base", OID: "v2", Size: big, Mode: "100644"},
		},
		Observed: map[string]Observation{"docs/Guide.md": guide, "docs/guide.md": lower},
	}
}

// A case-only rename on a case-insensitive filesystem: both spellings are
// the file the target spells docs/Guide.md. It keeps its history, is
// updated in place and is never deleted.
func TestCaseOnlyRenameCaseInsensitive(t *testing.T) {
	for _, content := range []string{"v1", "v2", "mine"} {
		t.Run(content, func(t *testing.T) {
			lower := reg(content)
			lower.ActualPath = "docs/Guide.md"
			plan := Decide(caseRename(reg(content), lower))
			if e, ok := entryAt(plan, "docs/Guide.md"); ok {
				t.Errorf("the old spelling has an entry: %+v", e)
			}
			e, _ := entryAt(plan, "docs/guide.md")
			want := map[string]State{"v1": Outdated, "v2": Current, "mine": Local}[content]
			if e.State != want || !strings.Contains(e.Detail, "docs/Guide.md") {
				t.Errorf("docs/guide.md = %+v, want %s naming the spelling", e, want)
			}
			if content == "v1" && (e.Action != Update || e.From != "v1") {
				t.Errorf("docs/guide.md = %+v, want an update from v1", e)
			}
		})
	}
	// The observations may come either way round: the target spells the file
	// like the new name, and the old name resolves to it.
	guide := reg("v1")
	guide.ActualPath = "docs/guide.md"
	plan := Decide(caseRename(guide, reg("v1")))
	if len(plan.Entries) != 1 || plan.Entries[0].Path != "docs/guide.md" || plan.Entries[0].State != Outdated {
		t.Errorf("new spelling on disk: %+v", plan.Entries)
	}
}

// On a case-sensitive filesystem the spellings are different files: the old
// one retires, and the new one is created only once it is gone, so git never
// tracks both.
func TestCaseOnlyRenameCaseSensitive(t *testing.T) {
	twin := Observation{Kind: Absent, CaseTwin: "docs/Guide.md"}
	for _, content := range []string{"v1", "v2"} {
		got := Decide(caseRename(reg(content), twin)).Entries
		for i := range got {
			got[i] = noDetail(got[i])
		}
		want := []Entry{
			{Path: "docs/Guide.md", State: Retired, Action: Delete, Pack: "base", From: content},
			{Path: "docs/guide.md", State: Missing, Action: Create, Pack: "base", To: "v2", Mode: "100644", AfterDeletes: true},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\ngot  %+v\nwant %+v", content, got, want)
		}
	}

	// The team edited the old spelling: it stays, and the new one would
	// collide with it.
	plan := Decide(caseRename(reg("mine"), twin))
	if len(plan.Changes()) != 0 {
		t.Errorf("local old spelling: changes %+v", plan.Changes())
	}
	if e, _ := entryAt(plan, "docs/guide.md"); e.State != Unsafe || !strings.Contains(e.Detail, "differs only by case from docs/Guide.md") {
		t.Errorf("docs/guide.md = %+v, want unsafe", e)
	}

	// Both spellings exist: the old one retires, the managed one is current.
	plan = Decide(caseRename(reg("v1"), reg("v2")))
	if e, _ := entryAt(plan, "docs/Guide.md"); e.Action != Delete {
		t.Errorf("docs/Guide.md = %+v, want a delete", e)
	}
	if e, _ := entryAt(plan, "docs/guide.md"); e.State != Current {
		t.Errorf("docs/guide.md = %+v, want current", e)
	}

	// A file the target has always owned under another spelling is never
	// joined by the pack's.
	in := Input{
		Selected: []string{"base"},
		Desired:  map[string]Desired{"readme.md": {Path: "readme.md", Pack: "base", OID: "r1", Size: big, Mode: "100644"}},
		Observed: map[string]Observation{"readme.md": {Kind: Absent, CaseTwin: "README.md"}},
	}
	if e, _ := entryAt(Decide(in), "readme.md"); e.State != Unsafe || e.Action != Keep {
		t.Errorf("readme.md next to README.md = %+v, want unsafe", e)
	}
}

// Old spellings that are one entry on a case-insensitive filesystem are
// deleted once, under the spelling the target uses.
func TestCaseVariantsDeletedOnce(t *testing.T) {
	in := Input{
		Manifest: manifestOf(
			hist{"Docs/Old.md", "base", "o1", big},
			hist{"docs/old.md", "base", "o2", big},
		),
		Selected: []string{"base"},
		Observed: map[string]Observation{
			"Docs/Old.md": {Kind: Regular, OIDs: []string{"o2"}, ActualPath: "docs/old.md"},
			"docs/old.md": reg("o2"),
		},
	}
	want := []Entry{{Path: "docs/old.md", State: Retired, Action: Delete, Pack: "base", From: "o2"}}
	got := Decide(in).Entries
	for i := range got {
		got[i] = noDetail(got[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Ignore patterns and the opt-in file match whatever the case: on NTFS and
// APFS a case variant is the same file.
func TestIgnoreAndOptInIgnoreCase(t *testing.T) {
	in := Input{
		Manifest: manifestOf(
			hist{"Docs/guide.md", "base", "g1", big},
			hist{".Engineering-Assets.yml", "base", "e1", big},
			hist{"docs/new.md", "base", "n1", big},
		),
		Selected: []string{"base"},
		Desired: map[string]Desired{
			"docs/new.md":             {Path: "docs/new.md", Pack: "base", OID: "n2", Size: big, Mode: "100644"},
			".ENGINEERING-ASSETS.YML": {Path: ".ENGINEERING-ASSETS.YML", Pack: "base", OID: "e2", Size: big, Mode: "100644"},
		},
		Observed: map[string]Observation{
			"Docs/guide.md":           reg("g1"),
			".Engineering-Assets.yml": reg("e1"),
			"docs/new.md":             reg("n1"),
			".ENGINEERING-ASSETS.YML": reg("mine"),
		},
		Ignore:    []string{"DOCS/**"},
		OptInFile: ".engineering-assets.yml",
		Adopt:     []string{"**"},
	}
	plan := Decide(in)
	if len(plan.Changes()) != 0 {
		t.Errorf("changes %+v, want none", plan.Changes())
	}
	for _, p := range []string{"Docs/guide.md", ".Engineering-Assets.yml", "docs/new.md", ".ENGINEERING-ASSETS.YML"} {
		if e, _ := entryAt(plan, p); e.State != Ignored {
			t.Errorf("%s = %+v, want ignored", p, e)
		}
	}
	if e, _ := entryAt(plan, ".Engineering-Assets.yml"); e.Detail != detailOptIn {
		t.Errorf("opt-in file detail %q", e.Detail)
	}
}

// Two packs ship ruff.toml; the later one wins, and the history of the one it
// overrides still proves the file came from the hub.
func TestLaterPackWinsAndOverriddenHistoryCounts(t *testing.T) {
	cur := provenance.Current{
		"base":   {"ruff.toml": {Pack: "base", Path: "ruff.toml", OID: "b2", Size: big, Mode: "100644"}},
		"python": {"ruff.toml": {Pack: "python", Path: "ruff.toml", OID: "p2", Size: big, Mode: "100644"}},
	}
	selected := []string{"base", "python"}
	m := manifestOf(
		hist{"ruff.toml", "base", "b1", big},
		hist{"ruff.toml", "base", "b2", big},
		hist{"ruff.toml", "python", "p1", big},
		hist{"ruff.toml", "python", "p2", big},
	)
	for _, from := range []string{"b1", "b2", "p1"} {
		in := Input{
			Manifest: m,
			Selected: selected,
			Desired:  Layer(cur, selected),
			Observed: map[string]Observation{"ruff.toml": reg(from)},
		}
		want := []Entry{{Path: "ruff.toml", State: Outdated, Action: Update, Pack: "python", From: from, To: "p2", Mode: "100644"}}
		if got := Decide(in).Entries; !reflect.DeepEqual(got, want) {
			t.Errorf("from %s: got %+v, want %+v", from, got, want)
		}
	}
}

// Pack agents was called ai before (formerly: [ai]).
func TestFormerlyAlias(t *testing.T) {
	m := manifestOf(
		hist{"AGENTS.md", "ai", "a1", big},
		hist{"PROMPT.md", "ai", "pr1", big},
		hist{"AGENTS.md", "agents", "cur", big},
	)
	base := func() Input {
		return Input{
			Manifest: m,
			Selected: []string{"agents"},
			Desired: map[string]Desired{
				"AGENTS.md": {Path: "AGENTS.md", Pack: "agents", OID: "cur", Size: big, Mode: "100644"},
			},
			Observed: map[string]Observation{
				"AGENTS.md": reg("a1"),
				"PROMPT.md": reg("pr1"),
			},
		}
	}

	in := base()
	in.Aliases = provenance.Aliases{"agents": {"ai"}}
	want := []Entry{
		{Path: "PROMPT.md", State: Retired, Action: Delete, Pack: "agents", From: "pr1"},
		{Path: "AGENTS.md", State: Outdated, Action: Update, Pack: "agents", From: "a1", To: "cur", Mode: "100644"},
	}
	got := Decide(in).Entries
	for i := range got {
		got[i] = noDetail(got[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with alias:\ngot  %+v\nwant %+v", got, want)
	}

	// Without the alias the old history belongs to a pack the target does not get.
	want = []Entry{
		{Path: "AGENTS.md", State: Local, Action: Keep, Pack: "agents"},
		{Path: "PROMPT.md", State: Orphaned, Action: Keep, Pack: "ai", From: "pr1"},
	}
	got = Decide(base()).Entries
	for i := range got {
		got[i] = noDetail(got[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without alias:\ngot  %+v\nwant %+v", got, want)
	}
}

// A pack the target no longer gets leaves its files alone.
func TestDeselectedPackOrphansNeverDeletes(t *testing.T) {
	in := Input{
		Manifest: manifestOf(
			hist{"ruff.toml", "python", "r1", big},
			hist{".gitkeep", "python", "k", tiny},
			hist{"AGENTS.md", "agents", "cur", big},
		),
		Selected: []string{"agents"},
		Desired: map[string]Desired{
			"AGENTS.md": {Path: "AGENTS.md", Pack: "agents", OID: "cur", Size: big, Mode: "100644"},
		},
		Observed: map[string]Observation{
			"AGENTS.md": reg("cur"),
			"ruff.toml": reg("r1"),
			".gitkeep":  reg("k"),
		},
	}
	plan := Decide(in)
	if len(plan.Changes()) != 0 {
		t.Errorf("changes: %+v", plan.Changes())
	}
	e, ok := entryAt(plan, "ruff.toml")
	if !ok || e.State != Orphaned || e.Action != Keep || e.Pack != "python" || e.From != "r1" {
		t.Errorf("ruff.toml: got %+v", e)
	}
	if e, ok := entryAt(plan, ".gitkeep"); ok {
		t.Errorf(".gitkeep: tiny content must not be orphaned: %+v", e)
	}
}

func TestAttribution(t *testing.T) {
	tests := []struct {
		name     string
		selected []string
		aliases  provenance.Aliases
		hs       []hist
		want     Entry
	}{
		{"retired: first selected pack", []string{"a", "b"}, nil,
			[]hist{{"X.md", "b", "x", big}, {"X.md", "a", "x", big}},
			Entry{Path: "X.md", State: Retired, Action: Delete, Pack: "a", From: "x"}},
		{"retired: selection order, not name order", []string{"b", "a"}, nil,
			[]hist{{"X.md", "a", "x", big}, {"X.md", "b", "x", big}},
			Entry{Path: "X.md", State: Retired, Action: Delete, Pack: "b", From: "x"}},
		{"retired: only the pack holding the id", []string{"a", "b"}, nil,
			[]hist{{"X.md", "a", "y", big}, {"X.md", "b", "x", big}},
			Entry{Path: "X.md", State: Retired, Action: Delete, Pack: "b", From: "x"}},
		{"retired: alias resolves to its pack", []string{"a", "b"}, provenance.Aliases{"b": {"old"}},
			[]hist{{"X.md", "a", "y", big}, {"X.md", "old", "x", big}},
			Entry{Path: "X.md", State: Retired, Action: Delete, Pack: "b", From: "x"}},
		{"retired: alias of an earlier pack wins", []string{"a", "b"}, provenance.Aliases{"a": {"old"}},
			[]hist{{"X.md", "b", "x", big}, {"X.md", "old", "x", big}},
			Entry{Path: "X.md", State: Retired, Action: Delete, Pack: "a", From: "x"}},
		{"orphaned: alphabetical", []string{"a"}, nil,
			[]hist{{"X.md", "zeta", "x", big}, {"X.md", "alpha", "x", big}},
			Entry{Path: "X.md", State: Orphaned, Action: Keep, Pack: "alpha", From: "x"}},
		{"orphaned: only the pack holding the id", []string{"a"}, nil,
			[]hist{{"X.md", "alpha", "y", big}, {"X.md", "zeta", "x", big}},
			Entry{Path: "X.md", State: Orphaned, Action: Keep, Pack: "zeta", From: "x"}},
		{"orphaned: alias resolves to its current name", []string{"a"}, provenance.Aliases{"omega": {"alpha"}},
			[]hist{{"X.md", "zeta", "x", big}, {"X.md", "alpha", "x", big}},
			Entry{Path: "X.md", State: Orphaned, Action: Keep, Pack: "omega", From: "x"}},
		{"orphaned: old name claimed twice resolves alphabetically", []string{"a"},
			provenance.Aliases{"zz": {"alpha"}, "mm": {"alpha"}},
			[]hist{{"X.md", "alpha", "x", big}},
			Entry{Path: "X.md", State: Orphaned, Action: Keep, Pack: "mm", From: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Input{
				Manifest: manifestOf(tt.hs...),
				Selected: tt.selected,
				Aliases:  tt.aliases,
				Observed: map[string]Observation{"X.md": reg("x")},
			}
			for range 20 {
				plan := Decide(in)
				if len(plan.Entries) != 1 || noDetail(plan.Entries[0]) != tt.want {
					t.Fatalf("got %+v, want %+v", plan.Entries, tt.want)
				}
			}
		})
	}
}

// A pack used to ship the file docs and now ships docs/a.md.
func TestFileToDirectory(t *testing.T) {
	base := func() Input {
		return Input{
			Manifest: manifestOf(
				hist{"docs", "docs", "d1", big},
				hist{"docs/a.md", "docs", "a1", big},
			),
			Selected: []string{"docs"},
			Desired: map[string]Desired{
				"docs/a.md": {Path: "docs/a.md", Pack: "docs", OID: "a1", Size: big, Mode: "100644"},
			},
			Observed: map[string]Observation{
				"docs":      reg("d1"),
				"docs/a.md": fileBlocker("docs"),
			},
		}
	}

	want := []Entry{
		{Path: "docs", State: Retired, Action: Delete, Pack: "docs", From: "d1"},
		{Path: "docs/a.md", State: Missing, Action: Create, Pack: "docs", To: "a1", Mode: "100644", AfterDeletes: true},
	}
	got := Decide(base()).Entries
	for i := range got {
		got[i] = noDetail(got[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("retired blocker:\ngot  %+v\nwant %+v", got, want)
	}

	// The team made docs its own: nothing is deleted and docs/a.md is unsafe.
	in := base()
	in.Observed["docs"] = reg("mine")
	plan := Decide(in)
	if len(plan.Changes()) != 0 {
		t.Errorf("local blocker: changes %+v", plan.Changes())
	}
	if e, _ := entryAt(plan, "docs/a.md"); e.State != Unsafe {
		t.Errorf("local blocker: docs/a.md is %+v, want unsafe", e)
	}
	if e, _ := entryAt(plan, "docs"); e.State != RetiredLocal {
		t.Errorf("local blocker: docs is %+v, want retired-local", e)
	}

	// A blocker that is a symlink is never deleted through.
	in = base()
	in.Observed["docs"] = Observation{Kind: NotRegular, Detail: "symlink"}
	in.Observed["docs/a.md"] = Observation{Kind: UnsafeParent, Blocker: "docs", Detail: "parent docs is a symlink"}
	plan = Decide(in)
	if len(plan.Changes()) != 0 {
		t.Errorf("symlink blocker: changes %+v", plan.Changes())
	}
	if e, _ := entryAt(plan, "docs/a.md"); e.State != Unsafe || e.Detail != "parent docs is a symlink" {
		t.Errorf("symlink blocker: docs/a.md is %+v, want unsafe", e)
	}

	// The blocker is deleted, but it is not the file blocking this path.
	in = base()
	in.Observed["docs/a.md"] = Observation{Kind: UnsafeParent, Blocker: "docs", BlockerIsFile: false}
	if e, _ := entryAt(Decide(in), "docs/a.md"); e.State != Unsafe {
		t.Errorf("non-file blocker: docs/a.md is %+v, want unsafe", e)
	}
}

// Once a file↔directory swap converged, what stands at the old paths is what
// the packs ship now: nothing is reported there.
func TestConvergedSwapIsQuiet(t *testing.T) {
	in := Input{
		Manifest: manifestOf(
			hist{"handbook", "base", "h1", big},
			hist{"handbook/index.md", "base", "i1", big},
			hist{"tools/run.md", "base", "r1", big},
			hist{"tools", "base", "t1", big},
		),
		Selected: []string{"base"},
		Desired: map[string]Desired{
			"handbook/index.md": {Path: "handbook/index.md", Pack: "base", OID: "i1", Size: big, Mode: "100644"},
			"tools":             {Path: "tools", Pack: "base", OID: "t1", Size: big, Mode: "100644"},
		},
		Observed: map[string]Observation{
			"handbook":          dir(),
			"handbook/index.md": reg("i1"),
			"tools":             reg("t1"),
			"tools/run.md":      fileBlocker("tools"),
		},
	}
	plan := Decide(in)
	if got := pathsOf(plan); !reflect.DeepEqual(got, []string{"handbook/index.md", "tools"}) {
		t.Errorf("entries for %q, want only the desired paths", got)
	}
	// A blocker or directory that is not what the packs ship is still unsafe.
	in.Observed["tools/run.md"] = fileBlocker("other")
	delete(in.Desired, "handbook/index.md")
	plan = Decide(in)
	for _, p := range []string{"handbook", "tools/run.md"} {
		if e, _ := entryAt(plan, p); e.State != Unsafe {
			t.Errorf("%s = %+v, want unsafe", p, e)
		}
	}
}

// A pack used to ship docs/a.md and docs/sub/b.md and now ships the file docs.
func TestDirectoryToFile(t *testing.T) {
	base := func() Input {
		return Input{
			Manifest: manifestOf(
				hist{"docs/a.md", "docs", "a1", big},
				hist{"docs/sub/b.md", "docs", "b1", big},
				hist{"docs-old/c.md", "docs", "c1", big},
				hist{"docs", "docs", "d1", big},
			),
			Selected: []string{"docs"},
			Desired: map[string]Desired{
				"docs": {Path: "docs", Pack: "docs", OID: "d1", Size: big, Mode: "100644"},
			},
			Observed: map[string]Observation{
				"docs":          dir(),
				"docs/a.md":     reg("a1"),
				"docs/sub/b.md": reg("b1"),
			},
		}
	}

	want := []Entry{
		{Path: "docs/sub/b.md", State: Retired, Action: Delete, Pack: "docs", From: "b1"},
		{Path: "docs/a.md", State: Retired, Action: Delete, Pack: "docs", From: "a1"},
		{Path: "docs", State: Missing, Action: Create, Pack: "docs", To: "d1", Mode: "100644", AfterDeletes: true},
	}
	got := Decide(base()).Entries
	for i := range got {
		got[i] = noDetail(got[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("retired children:\ngot  %+v\nwant %+v", got, want)
	}

	// Nothing beneath docs is retired: a sibling with a common prefix does not count.
	in := base()
	in.Observed["docs/a.md"] = reg("mine")
	in.Observed["docs/sub/b.md"] = Observation{Kind: Absent}
	in.Observed["docs-old/c.md"] = reg("c1")
	plan := Decide(in)
	if e, _ := entryAt(plan, "docs"); e.State != Unsafe || e.Action != Keep {
		t.Errorf("local children: docs is %+v, want unsafe", e)
	}
	if e, _ := entryAt(plan, "docs-old/c.md"); e.Action != Delete {
		t.Errorf("docs-old/c.md is %+v, want delete", e)
	}
}

func TestPlanOrder(t *testing.T) {
	var hs []hist
	observed := map[string]Observation{}
	for _, p := range []string{"b", "a/b/c", "a/b", "z/y", "c/d/e", "a/z"} {
		hs = append(hs, hist{p, "p", "o-" + p, big})
		observed[p] = reg("o-" + p)
	}
	in := Input{
		Manifest: manifestOf(hs...),
		Selected: []string{"p"},
		Desired: map[string]Desired{
			"m":     {Path: "m", Pack: "p", OID: "m1", Size: big, Mode: "100644"},
			"a/new": {Path: "a/new", Pack: "p", OID: "n1", Size: big, Mode: "100644"},
			"0":     {Path: "0", Pack: "p", OID: "z1", Size: big, Mode: "100644"},
		},
		Observed: observed,
	}
	in.Observed["0"] = reg("z1")
	want := []string{"a/b/c", "c/d/e", "a/b", "a/z", "z/y", "b", "0", "a/new", "m"}
	for range 20 {
		if got := pathsOf(Decide(in)); !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %q, want %q", got, want)
		}
	}
}

func TestPlanCountsAndChanges(t *testing.T) {
	in := undesiredBase()
	in.Manifest.Add("NEW.md", "agents", provenance.Version{OID: "n1", Size: big})
	in.Manifest.Add("MINE.md", "agents", provenance.Version{OID: "m1", Size: big})
	in.Desired["NEW.md"] = Desired{Path: "NEW.md", Pack: "agents", OID: "n1", Size: big, Mode: "100644"}
	in.Desired["MINE.md"] = Desired{Path: "MINE.md", Pack: "agents", OID: "m1", Size: big, Mode: "100644"}
	in.Observed["MINE.md"] = reg("mine")
	in.Observed["OLD.md"] = reg("old1")
	in.Observed["LEGACY.md"] = reg("gone1")

	plan := Decide(in)
	wantCounts := map[State]int{Current: 1, Missing: 1, Local: 1, Retired: 1, Orphaned: 1}
	if got := plan.Counts(); !reflect.DeepEqual(got, wantCounts) {
		t.Errorf("Counts = %v, want %v", got, wantCounts)
	}
	var changed []string
	for _, e := range plan.Changes() {
		changed = append(changed, e.Path+":"+string(e.Action))
	}
	if want := []string{"OLD.md:delete", "NEW.md:create"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("Changes = %q, want %q", changed, want)
	}

	var empty Plan
	if len(empty.Counts()) != 0 || empty.Changes() != nil {
		t.Errorf("empty plan: counts %v, changes %v", empty.Counts(), empty.Changes())
	}
}

func TestDecideNilManifest(t *testing.T) {
	in := desiredBase()
	in.Manifest = nil
	in.Observed["AGENTS.md"] = reg("v1")
	want := []Entry{{Path: "AGENTS.md", State: Local, Action: Keep, Pack: "agents"}}
	if got := Decide(in).Entries; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := Decide(Input{}).Entries; len(got) != 0 {
		t.Errorf("empty input: got %+v", got)
	}
}

// Decide must not modify its input.
func TestDecideDoesNotMutateInput(t *testing.T) {
	in := undesiredBase()
	in.Observed["OLD.md"] = reg("old1")
	in.Observed["LEGACY.md"] = reg("gone1")
	in.Selected = []string{"agents"}
	before := cloneInput(in)
	Decide(in)
	if !reflect.DeepEqual(in, before) {
		t.Errorf("input changed")
	}
}

func cloneInput(in Input) Input {
	out := in
	m := &provenance.Manifest{Version: in.Manifest.Version, HubCommit: in.Manifest.HubCommit, Paths: map[string]map[string][]provenance.Version{}}
	for p, byPack := range in.Manifest.Paths {
		m.Paths[p] = map[string][]provenance.Version{}
		for pack, vs := range byPack {
			m.Paths[p][pack] = slices.Clone(vs)
		}
	}
	out.Manifest = m
	out.Selected = slices.Clone(in.Selected)
	out.Desired = map[string]Desired{}
	for k, v := range in.Desired {
		out.Desired[k] = v
	}
	out.Observed = map[string]Observation{}
	for k, v := range in.Observed {
		v.OIDs = slices.Clone(v.OIDs)
		out.Observed[k] = v
	}
	return out
}
