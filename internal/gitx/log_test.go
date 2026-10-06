package gitx

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFirstParentLog(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	base := s.chain("main", "", 2)
	feature := s.chain("feature", base[1], 2)
	// Messages may hold anything but NUL (transfer.fsckObjects refuses a
	// commit with one): blank lines, CR, invalid UTF-8, lines that look like
	// headers.
	odd := "subject\n\nbody\r\n\n\ntree " + strings.Repeat("0", 40) + "\nparent x\n\xff\xfe\n"
	c1 := s.commit("main", []string{base[1]}, []file{{path: "m.txt", content: "m\n"}}, odd)
	merge := s.commit("main", []string{c1, feature[1]}, []file{{path: "a.txt", content: "feature 2\n"}}, "merge feature\n")
	head := s.commit("main", []string{merge}, []file{{path: "z.txt", content: "z\n"}}, "last")

	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 50); err != nil {
		t.Fatal(err)
	}
	log, shallow, err := tr.FirstParentLog(t.Context(), "refs/touchmark/remote/main", 10)
	if err != nil || shallow {
		t.Fatalf("FirstParentLog() = %v, %v, %v", shas(log), shallow, err)
	}
	want := []string{head, merge, c1, base[1], base[0]}
	var got []string
	for _, c := range log {
		got = append(got, c.SHA)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chain = %v, want %v (the feature commits are not on it)", got, want)
	}
	if log[2].Message != odd {
		t.Errorf("message = %q, want %q", log[2].Message, odd)
	}
	if log[0].Message != "last" { // as stored: no newline added

		t.Errorf("message = %q", log[0].Message)
	}
	if !slices.Equal(log[1].Parents, []string{c1, feature[1]}) {
		t.Errorf("merge parents = %v", log[1].Parents)
	}
	if len(log[4].Parents) != 0 || !slices.Equal(log[3].Parents, []string{base[0]}) {
		t.Errorf("parents = %v, %v", log[3].Parents, log[4].Parents)
	}
	if want := time.Unix(1767225600+60*7, 0); !log[0].Time.Equal(want) {
		t.Errorf("time = %v, want %v", log[0].Time, want)
	}

	// max cuts the chain; a chain going on is not shallow.
	log, shallow, err = tr.FirstParentLog(t.Context(), head, 2)
	if err != nil || len(log) != 2 || shallow {
		t.Errorf("FirstParentLog(max 2) = %v, %v, %v", shas(log), shallow, err)
	}
	// Exactly the whole chain: not shallow either (the root is reached).
	log, shallow, err = tr.FirstParentLog(t.Context(), head, 5)
	if err != nil || len(log) != 5 || shallow {
		t.Errorf("FirstParentLog(max 5) = %v, %v, %v", shas(log), shallow, err)
	}

	// A shallow boundary exactly at max.
	sh := newTarget(t, s.dir, Auth{})
	if _, _, err := sh.FetchBranch(t.Context(), "main", 3); err != nil {
		t.Fatal(err)
	}
	for _, max := range []int{3, 4, 20} {
		log, shallow, err = sh.FirstParentLog(t.Context(), head, max)
		if err != nil || len(log) != 3 || !shallow || log[2].SHA != c1 {
			t.Errorf("FirstParentLog(depth 3, max %d) = %v, %v, %v", max, shas(log), shallow, err)
		}
	}
	log, shallow, err = sh.FirstParentLog(t.Context(), head, 2)
	if err != nil || len(log) != 2 || shallow {
		t.Errorf("FirstParentLog(depth 3, max 2) = %v, %v, %v", shas(log), shallow, err)
	}

	for _, bad := range []string{"", "-n", "no-such-ref", "a\nb"} {
		if _, _, err := tr.FirstParentLog(t.Context(), bad, 5); err == nil {
			t.Errorf("FirstParentLog(%q) succeeded", bad)
		}
	}
	if _, _, err := tr.FirstParentLog(t.Context(), head, 0); err == nil {
		t.Error("FirstParentLog(max 0) succeeded")
	}
}

func TestCommit(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	root := s.commitAt("main", nil, []file{{path: "a", content: "a"}}, "root\n", "1767225600 +0300")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []string{root, "refs/touchmark/remote/main", root[:12]} {
		c, err := tr.Commit(t.Context(), rev)
		if err != nil || c.SHA != root || c.Message != "root\n" || len(c.Parents) != 0 {
			t.Errorf("Commit(%s) = %+v, %v", rev, c, err)
			continue
		}
		if _, off := c.Time.Zone(); off != 3*3600 || c.Time.Unix() != 1767225600 {
			t.Errorf("Commit(%s).Time = %v", rev, c.Time)
		}
	}
	if _, err := tr.Commit(t.Context(), missingOID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Commit(missing) = %v, want ErrNotFound", err)
	}
	if _, err := tr.Commit(t.Context(), "refs/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Commit(refs/nope) = %v, want ErrNotFound", err)
	}
	tree := s.git("rev-parse", root+"^{tree}")
	if _, err := tr.Commit(t.Context(), tree); err == nil {
		t.Error("Commit(tree) succeeded")
	}
}

func TestParseCommit(t *testing.T) {
	t.Parallel()
	tree := strings.Repeat("1", 40)
	p1, p2 := strings.Repeat("2", 40), strings.Repeat("3", 40)
	cases := []struct {
		raw     string
		want    CommitInfo
		wantErr bool
	}{
		{raw: "tree " + tree + "\nparent " + p1 + "\nparent " + p2 + "\nauthor A <a> 1 +0000\ncommitter C <c> 100 -0130\n\nmsg\n",
			want: CommitInfo{Parents: []string{p1, p2}, Message: "msg\n", Time: time.Unix(100, 0).In(time.FixedZone("", -5400))}},
		// A signed commit: the continuation lines are headers, not message.
		{raw: "tree " + tree + "\nauthor A <a> 1 +0000\ncommitter C <c> 2 +0000\ngpgsig -----BEGIN SSH SIGNATURE-----\n abc\n \n -----END SSH SIGNATURE-----\n\nsigned\n",
			want: CommitInfo{Message: "signed\n", Time: time.Unix(2, 0).In(time.FixedZone("", 0))}},
		// No message at all.
		{raw: "tree " + tree + "\ncommitter C <c> 2 +0000\n", want: CommitInfo{Time: time.Unix(2, 0).In(time.FixedZone("", 0))}},
		// A malformed committer line leaves Time zero.
		{raw: "tree " + tree + "\ncommitter C <c> soon\n\nm", want: CommitInfo{Message: "m"}},
		{raw: "tree " + tree + "\ncommitter C <c> 2 +00x0\n\nm", want: CommitInfo{Message: "m"}},
		{raw: "parent " + p1 + "\ntree " + tree + "\n\nm", wantErr: true},
		{raw: "tree xyz\n\nm", wantErr: true},
		{raw: "tree " + tree + "\nparent 123\n\nm", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseCommit([]byte(c.raw))
		if (err != nil) != c.wantErr {
			t.Errorf("parseCommit(%q) error = %v", c.raw, err)
			continue
		}
		if !c.wantErr && (!reflect.DeepEqual(got.Parents, c.want.Parents) || got.Message != c.want.Message || !got.Time.Equal(c.want.Time) ||
			got.Time.Location().String() != c.want.Time.Location().String()) {
			t.Errorf("parseCommit(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func TestDiffTree(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	gitlink := strings.Repeat("5", 40)
	one := s.commit("main", nil, []file{
		{path: "keep.txt", content: "keep\n"},
		{path: "mod.txt", content: "old\n"},
		{path: "gone.txt", content: "gone\n"},
		{path: "run.sh", content: "echo\n"},
		{path: "link", mode: "120000", content: "keep.txt"},
		{path: "dir/file", content: "f\n"},
	}, "one\n")
	two := s.commit("main", []string{one}, []file{
		{path: "mod.txt", content: "new\n"},
		{path: "gone.txt", del: true},
		{path: "run.sh", mode: "100755", content: "echo\n"},
		{path: "link", content: "now a file\n"},
		{path: "dir", del: true},
		{path: "dir", content: "a file where a directory was\n"},
		{path: "sub", mode: "160000", content: gitlink},
		{path: "b c/ü.txt", content: "unicode\n"},
	}, "two\n")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 5); err != nil {
		t.Fatal(err)
	}
	got, err := tr.DiffTree(t.Context(), one, two)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(rev, p string) string { return s.blob(rev, p) }
	z := zeroSHA1
	want := []DiffEntry{
		{"b c/ü.txt", "000000", "100644", z, blob(two, "b c/ü.txt")},
		{"dir", "000000", "100644", z, blob(two, "dir")},
		{"dir/file", "100644", "000000", blob(one, "dir/file"), z},
		{"gone.txt", "100644", "000000", blob(one, "gone.txt"), z},
		{"link", "120000", "100644", blob(one, "link"), blob(two, "link")},
		{"mod.txt", "100644", "100644", blob(one, "mod.txt"), blob(two, "mod.txt")},
		{"run.sh", "100644", "100755", blob(one, "run.sh"), blob(one, "run.sh")},
		{"sub", "000000", "160000", z, gitlink},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiffTree(one, two) =\n%v\nwant\n%v", got, want)
	}
	// From the empty tree: every entry is added; trees work as well.
	all, err := tr.DiffTree(t.Context(), "", s.git("rev-parse", one+"^{tree}"))
	if err != nil || len(all) != 6 {
		t.Errorf("DiffTree(empty, tree) = %v, %v", all, err)
	}
	for _, e := range all {
		if e.OldMode != "000000" || e.OldOID != z {
			t.Errorf("DiffTree(empty) entry %+v", e)
		}
	}
	if same, err := tr.DiffTree(t.Context(), two, two); err != nil || len(same) != 0 {
		t.Errorf("DiffTree(two, two) = %v, %v", same, err)
	}
	if _, err := tr.DiffTree(t.Context(), one, missingOID); err == nil {
		t.Error("DiffTree(missing) succeeded")
	}
}

func TestParseDiffTree(t *testing.T) {
	t.Parallel()
	a, b, z := oidA, oidB, oidZ
	out := ":100644 100755 " + a + " " + b + " M\x00b.txt\x00:000000 100644 " + z + " " + a + " A\x00a.txt\x00"
	got, err := parseDiffTree([]byte(out))
	want := []DiffEntry{{"a.txt", "000000", "100644", z, a}, {"b.txt", "100644", "100755", a, b}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parseDiffTree() = %v, %v", got, err)
	}
	for _, bad := range []string{
		":100644 100644 " + a + " " + b + " M\x00",                  // no path
		":100644 100644 " + a + " " + b + " R100\x00x\x00y\x00",     // a rename
		"100644 100644 " + a + " " + b + " M\x00x\x00",              // no colon
		":100644 100644 " + a + " " + b + "\x00x\x00",               // no status
		":100644 1006444 " + a + " " + b + " M\x00x\x00",            // bad mode
		":100644 100644 " + a + " xyz M\x00x\x00",                   // bad id
		":100644 100644 " + a + " " + b + " M\x00x\x00junk\x00",     // trailing junk
		":100644 100644 " + a + " " + b + " M\x00x\x00\x00\x00\x00", // empty records
	} {
		if got, err := parseDiffTree([]byte(bad)); err == nil {
			t.Errorf("parseDiffTree(%q) = %v", bad, got)
		}
	}
	if got, err := parseDiffTree(nil); err != nil || len(got) != 0 {
		t.Errorf("parseDiffTree(nil) = %v, %v", got, err)
	}
}

func TestIsCleanMerge(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	b0 := s.commit("main", nil, []file{{path: "base.txt", content: "b0\n"}, {path: "ours.txt", content: "base version\n"}}, "b0\n")
	hc := s.commit("sync", []string{b0}, []file{{path: "ours.txt", content: "hub version\n"}, {path: "new.txt", content: "new\n"}}, "touchmark\n")
	b1 := s.commit("main", []string{b0}, []file{{path: "base.txt", content: "b1\n"}, {path: "other.txt", content: "o\n"}}, "b1\n")
	// "Update branch": the result takes each path from one of the parents.
	clean := s.commit("sync", []string{hc, b1}, []file{{path: "base.txt", content: "b1\n"}, {path: "other.txt", content: "o\n"}}, "Merge main\n")
	// An evil merge adds a change of its own.
	evil := s.commit("evil", []string{hc, b1}, []file{{path: "base.txt", content: "b1\n"}, {path: "other.txt", content: "o\n"},
		{path: "extra.txt", content: "sneaked in\n"}}, "Merge main\n")
	// A conflict resolved by mixing both sides.
	b2 := s.commit("main2", []string{b0}, []file{{path: "ours.txt", content: "base changed it\n"}}, "b2\n")
	mixed := s.commit("mixed", []string{hc, b2}, []file{{path: "ours.txt", content: "hub version\nbase changed it\n"}}, "Merge main\n")
	// Taking the base's side of a conflict is clean: every path is a parent's.
	theirs := s.commit("theirs", []string{hc, b2}, []file{{path: "ours.txt", content: "base changed it\n"}}, "Merge main\n")
	tr := newTarget(t, s.dir, Auth{})
	for _, b := range []string{"sync", "evil", "mixed", "theirs"} {
		if _, _, err := tr.FetchBranch(t.Context(), b, 5); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		rev  string
		want bool
	}{
		"clean": {clean, true}, "evil": {evil, false}, "mixed": {mixed, false}, "theirs": {theirs, true},
	} {
		got, err := tr.IsCleanMerge(t.Context(), c.rev)
		if err != nil || got != c.want {
			t.Errorf("IsCleanMerge(%s) = %v, %v; want %v", name, got, err, c.want)
		}
	}
	if _, err := tr.IsCleanMerge(t.Context(), hc); err == nil || !strings.Contains(err.Error(), "not a merge") {
		t.Errorf("IsCleanMerge(non-merge) = %v", err)
	}
	if _, err := tr.IsCleanMerge(t.Context(), missingOID); err == nil {
		t.Error("IsCleanMerge(missing) succeeded")
	}
}

func TestIsAncestor(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	ids := s.chain("main", "", 3)
	side := s.chain("side", ids[0], 1)[0]
	tr := newTarget(t, s.dir, Auth{})
	for _, b := range []string{"main", "side"} {
		if _, _, err := tr.FetchBranch(t.Context(), b, 10); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{ids[0], ids[2], true}, {ids[2], ids[2], true}, {ids[2], ids[0], false}, {side, ids[2], false}, {ids[0], side, true},
	} {
		if got, err := tr.IsAncestor(t.Context(), c.a, c.b); err != nil || got != c.want {
			t.Errorf("IsAncestor(%s, %s) = %v, %v; want %v", c.a[:7], c.b[:7], got, err, c.want)
		}
	}
	if _, err := tr.IsAncestor(t.Context(), missingOID, ids[2]); err == nil {
		t.Error("IsAncestor(missing) succeeded")
	}
	if _, err := tr.IsAncestor(t.Context(), "--all", ids[2]); err == nil {
		t.Error("IsAncestor(option) succeeded")
	}
}

func TestTree(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	gitlink := strings.Repeat("7", 40)
	head := s.commit("main", nil, []file{
		{path: "README.md", content: "r\n"},
		{path: "bin/run", mode: "100755", content: "#!/bin/sh\n"},
		{path: "link", mode: "120000", content: "README.md"},
		{path: "vendor/sub", mode: "160000", content: gitlink},
		{path: "a b/c\td.txt", content: "odd name\n"},
	}, "one\n")
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	got, err := tr.Tree(t.Context(), head)
	if err != nil {
		t.Fatal(err)
	}
	want := []TreeEntry{
		{Mode: "100644", Type: "blob", OID: s.blob(head, "README.md"), Size: -1, Path: "README.md"},
		{Mode: "100644", Type: "blob", OID: s.blob(head, "a b/c\td.txt"), Size: -1, Path: "a b/c\td.txt"},
		{Mode: "100755", Type: "blob", OID: s.blob(head, "bin/run"), Size: -1, Path: "bin/run"},
		{Mode: "120000", Type: "blob", OID: s.blob(head, "link"), Size: -1, Path: "link"},
		{Mode: "160000", Type: "commit", OID: gitlink, Size: -1, Path: "vendor/sub"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tree() =\n%v\nwant\n%v", got, want)
	}
	if _, err := tr.Tree(t.Context(), missingOID); err == nil {
		t.Error("Tree(missing) succeeded")
	}
}

func TestParseShortTree(t *testing.T) {
	t.Parallel()
	got, err := parseShortTree([]byte("100644 blob " + oidA + "\tx y\x00160000 commit " + oidB + "\tsub\x00"))
	want := []TreeEntry{{"100644", "blob", oidA, -1, "x y"}, {"160000", "commit", oidB, -1, "sub"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parseShortTree() = %v, %v", got, err)
	}
	for _, bad := range []string{
		"100644 blob " + oidA + " 12\tx\x00", // --long form
		"100644 blob xyz\tx\x00",
		"100644 blob " + oidA + "\t\x00",
		"100644 blob " + oidA + "x\x00",
		" blob " + oidA + "\tx\x00",
	} {
		if got, err := parseShortTree([]byte(bad)); err == nil {
			t.Errorf("parseShortTree(%q) = %v", bad, got)
		}
	}
}

func TestIdentTime(t *testing.T) {
	t.Parallel()
	for ident, want := range map[string]string{
		"A <a> 1767225600 +0000":         "2026-01-01T00:00:00Z",
		"A B <a@b> 1767225600 -0930":     "2025-12-31T14:30:00-09:30",
		"A <a> 1767225600 +0000 extra":   "",
		"A <a>":                          "",
		"no email 1767225600 +0000":      "",
		"A <a> 1767225600 +0060":         "",
		"<> 0 +0000":                     "1970-01-01T00:00:00Z",
		"A <a> -5 +0000":                 "1969-12-31T23:59:55Z",
		fmt.Sprintf("A <a> %d +0000", 1): "1970-01-01T00:00:01Z",
	} {
		got := identTime(ident)
		s := ""
		if !got.IsZero() {
			s = got.Format(time.RFC3339)
		}
		if s != want {
			t.Errorf("identTime(%q) = %q, want %q", ident, s, want)
		}
	}
}
