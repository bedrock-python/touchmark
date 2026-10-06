package decide

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Blob ids of the key vectors.
const (
	oidA = "8f3c1a0b2e7d4a619b0c1d2e3f405162738495a6"
	oidB = "77ab0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6071"
	oidC = "5e7a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c"
	oidD = "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d"
	oidE = "9a01bc23de45f6789a01bc23de45f6789a01bc23"
)

// keyLine is one hashed pair: path NUL from NUL mode NUL to LF.
func keyLine(path, from, mode, to string) string {
	return path + "\x00" + from + "\x00" + mode + "\x00" + to + "\n"
}

// hashed returns "sha256:" + hex(sha256(b)).
func hashed(b string) string {
	sum := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// mixedPairs has one pair per kind of change, in no particular order.
func mixedPairs() []Pair {
	return []Pair{
		{Path: "prompts/review.md", From: ZeroOID, Mode: "100644", To: oidC}, // create
		{Path: "AGENTS.md", From: oidB, Mode: "100644", To: oidA},            // update
		{Path: "prompts/old.md", From: oidE, Mode: ModeDelete, To: ZeroOID},  // delete
		{Path: "scripts/check.sh", From: oidD, Mode: "100755", To: oidD},     // chmod
	}
}

// TestKeyVectors pins the content key. Each vector spells
// out the bytes that are hashed, and the expected keys were computed from
// those bytes with an independent implementation (Python hashlib), so both
// the construction and the digest are fixed.
func TestKeyVectors(t *testing.T) {
	const domain = "touchmark-content/v1\n"
	cases := []struct {
		name   string
		stream string
		pairs  []Pair
		bytes  string
		want   string
	}{
		{
			name:   "empty sync",
			stream: StreamSync,
			bytes:  "touchmark-content/v1\nsync\n",
			want:   "sha256:1d5b7781d049554ae4c204681dc74d7875497184d8208542ca6acc4653f2b071",
		},
		{
			name:   "empty adopt",
			stream: StreamAdopt,
			bytes:  "touchmark-content/v1\nadopt\n",
			want:   "sha256:736568e23e8657c8a2edb7ba04e59fcdc58830048b10fea134ec43f16c064554",
		},
		{
			name:   "one create",
			stream: StreamSync,
			pairs:  []Pair{{Path: "AGENTS.md", From: ZeroOID, Mode: "100644", To: oidA}},
			bytes: "touchmark-content/v1\n" + "sync\n" +
				"AGENTS.md" + "\x00" + "0000000000000000000000000000000000000000" + "\x00" + "100644" + "\x00" + "8f3c1a0b2e7d4a619b0c1d2e3f405162738495a6" + "\n",
			want: "sha256:ca7b18586815a4e324228aca12c9117b2add31d9d553364eaf675cbffc0c23e4",
		},
		{
			name:   "create, update, delete and chmod",
			stream: StreamSync,
			pairs:  mixedPairs(),
			bytes: domain + "sync\n" +
				keyLine("AGENTS.md", oidB, "100644", oidA) +
				keyLine("prompts/old.md", oidE, "000000", ZeroOID) +
				keyLine("prompts/review.md", ZeroOID, "100644", oidC) +
				keyLine("scripts/check.sh", oidD, "100755", oidD),
			want: "sha256:692b98bad8e4804318ec4e629ddd593ac5d0655f9b712b10fea826cd25dd6958",
		},
		{
			name:   "same pairs in the adopt stream",
			stream: StreamAdopt,
			pairs:  mixedPairs(),
			bytes: domain + "adopt\n" +
				keyLine("AGENTS.md", oidB, "100644", oidA) +
				keyLine("prompts/old.md", oidE, "000000", ZeroOID) +
				keyLine("prompts/review.md", ZeroOID, "100644", oidC) +
				keyLine("scripts/check.sh", oidD, "100755", oidD),
			want: "sha256:612cae8f42cae486cbfc4e6c793446dc525ff1310d0c6a88d82834aecb3c6b41",
		},
		{
			// Byte order, not locale order: '-' (0x2d) < '.' (0x2e) < '/'
			// (0x2f) < 'B' (0x42) < 'a' (0x61).
			name:   "path byte order",
			stream: StreamSync,
			pairs: []Pair{
				{Path: "a/b", From: ZeroOID, Mode: "100644", To: oidA},
				{Path: "a.md", From: ZeroOID, Mode: "100644", To: oidA},
				{Path: "B.md", From: ZeroOID, Mode: "100644", To: oidA},
				{Path: "a-b", From: ZeroOID, Mode: "100644", To: oidA},
			},
			bytes: domain + "sync\n" +
				keyLine("B.md", ZeroOID, "100644", oidA) +
				keyLine("a-b", ZeroOID, "100644", oidA) +
				keyLine("a.md", ZeroOID, "100644", oidA) +
				keyLine("a/b", ZeroOID, "100644", oidA),
			want: "sha256:226907180c0781160e90e3574ca2f13d38ad55f64eb734d3151aa35f3aace17a",
		},
		{
			name:   "rollback to a version declined before (from matters)",
			stream: StreamSync,
			pairs:  []Pair{{Path: "AGENTS.md", From: oidC, Mode: "100644", To: oidA}},
			bytes:  domain + "sync\n" + keyLine("AGENTS.md", oidC, "100644", oidA),
			want:   "sha256:d1776dc3d5cca9919a8ceb2771a6d8eac9ab68ffd4c6408a7c2fdfb5291af6cf",
		},
		{
			name:   "UTF-8 path",
			stream: StreamSync,
			pairs:  []Pair{{Path: "docs/Guide \xc3\xbc.md", From: ZeroOID, Mode: "100644", To: oidA}},
			bytes:  domain + "sync\n" + keyLine("docs/Guide \xc3\xbc.md", ZeroOID, "100644", oidA),
			want:   "sha256:c914a4f5a9d6ed96e2ac6afd6b7ad6e1c64712b4b68750c66ca5c8a45578aaa0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hashed(c.bytes); got != c.want {
				t.Fatalf("the spelled-out bytes hash to %s, want %s", got, c.want)
			}
			if got := Key(c.stream, c.pairs); got != c.want {
				t.Errorf("Key = %s, want %s", got, c.want)
			}
		})
	}
}

func TestKeyOrderIndependent(t *testing.T) {
	pairs := mixedPairs()
	want := Key(StreamSync, pairs)
	r := rand.New(rand.NewPCG(7, 11))
	for range 20 {
		shuffled := slices.Clone(pairs)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		before := slices.Clone(shuffled)
		if got := Key(StreamSync, shuffled); got != want {
			t.Fatalf("Key(%v) = %s, want %s", shuffled, got, want)
		}
		if !slices.Equal(shuffled, before) {
			t.Fatal("Key reordered its input")
		}
	}
}

func TestKeyDistinguishes(t *testing.T) {
	base := []Pair{{Path: "AGENTS.md", From: ZeroOID, Mode: "100644", To: oidA}}
	edit := func(f func(*Pair)) []Pair {
		p := base[0]
		f(&p)
		return []Pair{p}
	}
	variants := map[string][]Pair{
		// "missing → v1" and "v2 → v1" are different pairs: a hub rollback
		// to a version declined before is delivered after another merged.
		"from":          edit(func(p *Pair) { p.From = oidC }),
		"mode":          edit(func(p *Pair) { p.Mode = "100755" }),
		"to":            edit(func(p *Pair) { p.To = oidB }),
		"path":          edit(func(p *Pair) { p.Path = "agents.md" }),
		"one more pair": append(slices.Clone(base), Pair{Path: "b", From: ZeroOID, Mode: "100644", To: oidA}),
		"no pair":       nil,
	}
	want := Key(StreamSync, base)
	seen := map[string]string{want: "base"}
	for name, pairs := range variants {
		got := Key(StreamSync, pairs)
		if other, ok := seen[got]; ok {
			t.Errorf("%s has the key of %s", name, other)
		}
		seen[got] = name
	}
	if Key(StreamSync, base) == Key(StreamAdopt, base) {
		t.Error("streams share a key")
	}
}

func TestKeyDuplicatePathPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Key did not panic on a duplicate path")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "duplicate path") {
			t.Errorf("panic = %v", r)
		}
	}()
	Key(StreamSync, []Pair{
		{Path: "a", From: ZeroOID, Mode: "100644", To: oidA},
		{Path: "b", From: ZeroOID, Mode: "100644", To: oidA},
		{Path: "a", From: oidA, Mode: ModeDelete, To: ZeroOID},
	})
}

func TestShort(t *testing.T) {
	cases := []struct {
		oid  string
		n    int
		want string
	}{
		{oidA, 16, "8f3c1a0b2e7d4a61"},
		{oidA, 7, "8f3c1a0"},
		{oidA, 40, oidA},
		{oidA, 64, oidA},
		{"abc", 16, "abc"},
		{ZeroOID, 16, ""},
		{strings.Repeat("0", 64), 16, ""},
		{"", 16, ""},
		{"0000000000000000a", 16, "0000000000000000"},
		{oidA, 0, ""},
		{oidA, -1, ""},
	}
	for _, c := range cases {
		if got := Short(c.oid, c.n); got != c.want {
			t.Errorf("Short(%q, %d) = %q, want %q", c.oid, c.n, got, c.want)
		}
	}
}

func TestPairsMapsActions(t *testing.T) {
	plan := Plan{Entries: []Entry{
		// Deletes come first in a plan; Pairs sorts by path.
		{Path: "old.md", State: Retired, Action: Delete, From: oidE},
		{Path: "z/deleted-with-fields", State: Retired, Action: Delete, From: oidE, Mode: "100644", To: oidA},
		{Path: "AGENTS.md", State: Outdated, Action: Update, From: oidB, To: oidA, Mode: "100644"},
		{Path: "B.md", State: Missing, Action: Create, To: oidC, Mode: "100755"},
		{Path: "bin/tool", State: Current, Action: Chmod, From: oidD, Mode: "100755"},
		{Path: "bin/tool2", State: Current, Action: Chmod, From: oidD, To: oidD, Mode: "100755"},
		{Path: "blocked/new.md", State: Missing, Action: Create, To: oidA, Mode: "100644", AfterDeletes: true},
		{Path: "current.md", State: Current, Action: Keep, From: oidA},
		{Path: "ignored.md", State: Ignored, Action: Keep},
		{Path: "local.md", State: Local, Action: Adopt, To: oidC, Mode: "100644"},
		{Path: "no-mode.md", State: Missing, Action: Create, To: oidA},
		{Path: "orphan.md", State: Orphaned, Action: Keep, From: oidB},
		{Path: "unknown", Action: "frobnicate", To: oidA},
		{Path: "unsafe", State: Unsafe, Action: Keep},
	}}
	want := []Pair{
		{Path: "AGENTS.md", From: oidB, Mode: "100644", To: oidA},
		{Path: "B.md", From: ZeroOID, Mode: "100755", To: oidC},
		{Path: "bin/tool", From: oidD, Mode: "100755", To: oidD},
		{Path: "bin/tool2", From: oidD, Mode: "100755", To: oidD},
		{Path: "blocked/new.md", From: ZeroOID, Mode: "100644", To: oidA},
		// Decide records no From for an adopt: see Pairs.
		{Path: "local.md", From: ZeroOID, Mode: "100644", To: oidC},
		{Path: "no-mode.md", From: ZeroOID, Mode: "100644", To: oidA},
		{Path: "old.md", From: oidE, Mode: ModeDelete, To: ZeroOID},
		{Path: "z/deleted-with-fields", From: oidE, Mode: ModeDelete, To: ZeroOID},
	}
	if got := Pairs(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("Pairs =\n%v\nwant\n%v", got, want)
	}
	if got := Pairs(Plan{}); got != nil {
		t.Errorf("Pairs of an empty plan = %v, want nil", got)
	}
	if got := Pairs(Plan{Entries: []Entry{{Path: "a", State: Current, Action: Keep}}}); got != nil {
		t.Errorf("Pairs of a plan without changes = %v, want nil", got)
	}
}

// TestPairsOfDecide runs Decide and checks D and its key: every action that
// changes the target gives exactly one pair, with From = the blob in the
// base.
func TestPairsOfDecide(t *testing.T) {
	in := Input{
		Manifest: manifestOf(
			hist{"AGENTS.md", "agents", "a1", big},
			hist{"AGENTS.md", "agents", "a2", big},
			hist{"old.md", "agents", "o1", big},
			hist{"blocker", "agents", "k1", big},
			hist{"tool.sh", "agents", "t1", big},
		),
		Selected: []string{"agents"},
		Desired: map[string]Desired{
			"AGENTS.md":   {Path: "AGENTS.md", Pack: "agents", OID: "a2", Size: big, Mode: "100644"},
			"new.md":      {Path: "new.md", Pack: "agents", OID: "n1", Size: big, Mode: "100644"},
			"tool.sh":     {Path: "tool.sh", Pack: "agents", OID: "t1", Size: big, Mode: "100755"},
			"local.md":    {Path: "local.md", Pack: "agents", OID: "l1", Size: big, Mode: "100644"},
			"blocker/x":   {Path: "blocker/x", Pack: "agents", OID: "x1", Size: big, Mode: "100644"},
			"current.txt": {Path: "current.txt", Pack: "agents", OID: "c1", Size: big, Mode: "100644"},
		},
		Observed: map[string]Observation{
			"AGENTS.md":   regMode("100644", "a1"),
			"tool.sh":     regMode("100644", "t1"),
			"local.md":    regMode("100644", "mine"),
			"old.md":      regMode("100644", "o1"),
			"blocker":     regMode("100644", "k1"),
			"blocker/x":   fileBlocker("blocker"),
			"current.txt": regMode("100644", "c1"),
		},
	}
	got := Pairs(Decide(in))
	want := []Pair{
		{Path: "AGENTS.md", From: "a1", Mode: "100644", To: "a2"},
		{Path: "blocker", From: "k1", Mode: ModeDelete, To: ZeroOID},
		{Path: "blocker/x", From: ZeroOID, Mode: "100644", To: "x1"},
		{Path: "new.md", From: ZeroOID, Mode: "100644", To: "n1"},
		{Path: "old.md", From: "o1", Mode: ModeDelete, To: ZeroOID},
		{Path: "tool.sh", From: "t1", Mode: "100755", To: "t1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Pairs(Decide) =\n%v\nwant\n%v", got, want)
	}
	if Key(StreamSync, got) != Key(StreamSync, want) {
		t.Error("key differs")
	}
}
