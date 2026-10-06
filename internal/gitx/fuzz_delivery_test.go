package gitx

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Fuzz targets for the parsers of target-controlled git output, ls-tree
// -z among them. Anyone who may push to a target shapes these
// outputs: the parsers must never panic, and what they accept keeps its
// promises.

func FuzzParseShortTree(f *testing.F) {
	oid := strings.Repeat("a", 40)
	f.Add("100644 blob " + oid + "\tAGENTS.md\x00160000 commit " + oid + "\tvendor/lib\x00")
	f.Add("120000 blob " + oid + "\ta\tb\x00")
	f.Add("100644 blob " + oid + "\t\x00")
	f.Add("x")
	f.Fuzz(func(t *testing.T, out string) {
		entries, err := parseShortTree([]byte(out))
		if err != nil {
			return
		}
		for _, e := range entries {
			if !isOID(e.OID) || e.Path == "" || e.Mode == "" || e.Type == "" || e.Size != -1 {
				t.Fatalf("parsed %+v from %q", e, out)
			}
		}
	})
}

func FuzzParseDiffTree(f *testing.F) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f.Add(":100644 100644 " + a + " " + b + " M\x00AGENTS.md\x00:000000 100755 " + zeroSHA1 + " " + b + " A\x00run.sh\x00")
	f.Add(":100644 000000 " + a + " " + zeroSHA1 + " D\x00gone\x00")
	f.Add(":100644 100644 " + a + " " + b + " R100\x00x\x00y\x00")
	f.Add(":1 2 3 4 5\x00")
	f.Fuzz(func(t *testing.T, out string) {
		entries, err := parseDiffTree([]byte(out))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.Path == "" || !isOID(e.OldOID) || !isOID(e.NewOID) || !isMode(e.OldMode) || !isMode(e.NewMode) {
				t.Fatalf("parsed %+v from %q", e, out)
			}
		}
		if !slices.IsSortedFunc(entries, func(x, y DiffEntry) int { return strings.Compare(x.Path, y.Path) }) {
			t.Fatalf("entries not sorted: %+v", entries)
		}
	})
}

func FuzzParseCommit(f *testing.F) {
	tree, parent := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f.Add("tree " + tree + "\nparent " + parent + "\nauthor A <a@x> 1 +0000\ncommitter C <c@x> 2 +0100\n\nmessage\n")
	f.Add("tree " + tree + "\ngpgsig -----BEGIN SSH SIGNATURE-----\n x\n -----END SSH SIGNATURE-----\n\n")
	f.Add("tree " + tree + "\ncommitter C <c@x> 99999999999999999999 +9999")
	f.Add("parent " + parent + "\n\n")
	f.Fuzz(func(t *testing.T, raw string) {
		c, err := parseCommit([]byte(raw))
		if err != nil {
			return
		}
		if !strings.HasPrefix(raw, "tree ") {
			t.Fatalf("accepted a commit without a first tree line: %q", raw)
		}
		for _, p := range c.Parents {
			if !isOID(p) {
				t.Fatalf("parent %q from %q", p, raw)
			}
		}
	})
}

func FuzzParseBatch(f *testing.F) {
	oid := strings.Repeat("c", 40)
	f.Add(oid+" blob 3\nabc\n", oid)
	f.Add("HEAD missing\n", "HEAD")
	f.Add(oid+" commit 999\nshort\n", oid)
	f.Add(oid+" blob 0\n\nextra", oid)
	f.Fuzz(func(t *testing.T, out, name string) {
		names := []string{name, name}
		objs, err := parseBatch([]byte(out), names)
		if err != nil {
			return
		}
		if len(objs) != len(names) {
			t.Fatalf("%d objects for %d names", len(objs), len(names))
		}
		// The input is consumed exactly, object by object: a missing line,
		// or a header naming the object's size, its content and a newline.
		rest := out
		for i, o := range objs {
			if o.missing {
				line := names[i] + " missing\n"
				if !strings.HasPrefix(rest, line) {
					line = names[i] + " ambiguous\n"
				}
				if !strings.HasPrefix(rest, line) {
					t.Fatalf("object %d missing without its line in %q", i, out)
				}
				rest = rest[len(line):]
				continue
			}
			header, after, _ := strings.Cut(rest, "\n")
			f := strings.Split(header, " ")
			size, err := strconv.Atoi(f[len(f)-1])
			if !isOID(o.oid) || len(f) != 3 || f[0] != o.oid || f[1] != o.typ || err != nil || size != len(o.content) ||
				!strings.HasPrefix(after, string(o.content)+"\n") {
				t.Fatalf("object %d %+v does not match %q", i, o, out)
			}
			rest = after[len(o.content)+1:]
		}
		if rest != "" {
			t.Fatalf("%q of %q left over", rest, out)
		}
	})
}

func FuzzParseBatchCheck(f *testing.F) {
	oid := strings.Repeat("d", 40)
	f.Add(oid+" blob 12\nx missing\n", "a", "x")
	f.Add(oid+" tree -1\n", "a", "b")
	f.Fuzz(func(t *testing.T, out, a, b string) {
		heads, err := parseBatchCheck([]byte(out), []string{a, b})
		if err != nil {
			return
		}
		if len(heads) != 2 {
			t.Fatalf("%d headers for 2 names", len(heads))
		}
		for _, h := range heads {
			if !h.missing && (!isOID(h.oid) || h.size < 0) {
				t.Fatalf("parsed %+v from %q", h, out)
			}
		}
	})
}
