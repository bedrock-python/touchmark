package gitx

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// buildFixture serves a base commit and returns a target that fetched it
// without blobs, with the base's id.
func buildFixture(t *testing.T) (*served, *TargetRepo, string) {
	t.Helper()
	s := newServed(t, t.TempDir(), "repo.git")
	b := s.commit("main", nil, []file{
		{path: "keep.txt", content: "keep\n"},
		{path: "mod.txt", content: "old\n"},
		{path: "gone.txt", content: "gone\n"},
		{path: "run.sh", content: "echo\n"},
		{path: "tools/a.sh", content: "a\n"},
		{path: "link", mode: "120000", content: "keep.txt"},
	}, "base\n")
	tr := newTarget(t, s.dir, Auth{})
	if sha, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil || sha != b {
		t.Fatalf("FetchBranch() = %s, %v", sha, err)
	}
	return s, tr, b
}

// hubBlobs returns Blobs for contents, keyed by id.
func hubBlobs(contents ...string) map[string]Blob {
	blobs := map[string]Blob{}
	for _, c := range contents {
		id := RawOID([]byte(c))
		blobs[id] = Blob{OID: id, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(c)), nil }}
	}
	return blobs
}

var (
	bot  = Person{Name: "acme-assets[bot]", Email: "123+acme-assets[bot]@users.noreply.github.com"}
	when = time.Date(2026, 9, 25, 10, 30, 0, 0, time.FixedZone("MSK", 3*3600))
	msg  = "chore: sync engineering assets\n\nTouchmark-Hub: acme-eng@github.com/712345678\nTouchmark-Stream: sync"
)

// syncSpec is a commit with every kind of change on base b.
func syncSpec(s *served, b string) CommitSpec {
	run := s.blob(b, "run.sh")
	return CommitSpec{
		Parent: b,
		Changes: []Change{
			{Path: "mod.txt", Mode: "100644", OID: RawOID([]byte("new\n"))},
			{Path: "gone.txt"},
			{Path: "run.sh", Mode: "100755", OID: run},
			{Path: "tools/a.sh"},
			{Path: "tools", Mode: "100644", OID: RawOID([]byte("tools is a file now\n"))},
			{Path: "prompts/review.md", Mode: "100644", OID: RawOID([]byte("review\n"))},
		},
		Blobs:     hubBlobs("new\n", "echo\n", "tools is a file now\n", "review\n", "unused\n"),
		Author:    bot,
		Committer: bot,
		When:      when,
		Message:   msg,
	}
}

func TestBuildCommit(t *testing.T) {
	t.Parallel()
	s, tr, b := buildFixture(t)
	spec := syncSpec(s, b)
	built, err := tr.BuildCommit(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := tr.Tree(t.Context(), built.Commit)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Path] = e.Mode + " " + e.OID
	}
	want := map[string]string{
		"keep.txt":          "100644 " + s.blob(b, "keep.txt"),
		"link":              "120000 " + s.blob(b, "link"),
		"mod.txt":           "100644 " + RawOID([]byte("new\n")),
		"run.sh":            "100755 " + s.blob(b, "run.sh"),
		"tools":             "100644 " + RawOID([]byte("tools is a file now\n")),
		"prompts/review.md": "100644 " + RawOID([]byte("review\n")),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tree =\n%v\nwant\n%v", got, want)
	}
	if tree, err := tr.resolve(t.Context(), built.Commit+"^{tree}"); err != nil || tree != built.Tree {
		t.Errorf("Built.Tree = %s, the commit's tree is %s (%v)", built.Tree, tree, err)
	}
	raw, err := tr.catFile(t.Context(), []string{built.Commit}, MaxCommitObject)
	if err != nil {
		t.Fatal(err)
	}
	// When is written in UTC: 10:30 MSK is 07:30Z.
	unix := strconv.FormatInt(time.Date(2026, 9, 25, 7, 30, 0, 0, time.UTC).Unix(), 10)
	wantRaw := "tree " + built.Tree + "\nparent " + b + "\n" +
		"author acme-assets[bot] <123+acme-assets[bot]@users.noreply.github.com> " + unix + " +0000\n" +
		"committer acme-assets[bot] <123+acme-assets[bot]@users.noreply.github.com> " + unix + " +0000\n\n" + msg + "\n"
	if string(raw[0].content) != wantRaw {
		t.Errorf("commit object =\n%s\nwant\n%s", raw[0].content, wantRaw)
	}
	// The blob no change uses was not written.
	if lazyFetchOff(t) {
		if missing, err := tr.missing(t.Context(), []string{RawOID([]byte("unused\n"))}); err != nil || len(missing) != 1 {
			t.Errorf("unused blob written: %v, %v", missing, err)
		}
	}

	// Same input, same id: again, and in another repository.
	again, err := tr.BuildCommit(t.Context(), spec)
	if err != nil || again != built {
		t.Errorf("BuildCommit(again) = %+v, %v; want %+v", again, err, built)
	}
	other := newTarget(t, s.dir, Auth{})
	if _, _, err := other.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	if elsewhere, err := other.BuildCommit(t.Context(), spec); err != nil || elsewhere != built {
		t.Errorf("BuildCommit(other repository) = %+v, %v; want %+v", elsewhere, err, built)
	}

	// The commit reaches the server whole.
	res, err := tr.Push(t.Context(), PushSpec{Branch: "touchmark/acme-eng", Commit: built.Commit})
	if err != nil || res.Status != PushOK {
		t.Fatalf("Push() = %+v, %v", res, err)
	}
	s.git("fsck", "--strict", "--no-dangling")
	if got := s.git("rev-parse", "refs/heads/touchmark/acme-eng^{tree}"); got != built.Tree {
		t.Errorf("served tree = %s, want %s", got, built.Tree)
	}
	// No temporary index is left behind.
	if left, _ := filepath.Glob(filepath.Join(tr.Dir, "touchmark-index-*")); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestBuildCommitIntegrity(t *testing.T) {
	t.Parallel()
	s, tr, b := buildFixture(t)
	base := func(changes ...Change) CommitSpec {
		return CommitSpec{Parent: b, Changes: changes, Blobs: hubBlobs("keep\n", "x\n"),
			Author: bot, Committer: bot, When: when, Message: msg}
	}
	x := RawOID([]byte("x\n"))
	for name, spec := range map[string]CommitSpec{
		// Writing what the base holds changes nothing.
		"no-op": base(Change{Path: "keep.txt", Mode: "100644", OID: s.blob(b, "keep.txt")}),
		// A file where a directory is, without deleting the directory's
		// files: update-index replaces them, which the diff-tree check
		// catches.
		"file over dir": base(Change{Path: "tools", Mode: "100644", OID: x}),
		// A directory where a file is, likewise.
		"dir over file": base(Change{Path: "mod.txt/x", Mode: "100644", OID: x}),
		// Deleting a path the base does not have.
		"absent delete": base(Change{Path: "nope.txt"}),
	} {
		if _, err := tr.BuildCommit(t.Context(), spec); !errors.Is(err, ErrIntegrity) {
			t.Errorf("%s: BuildCommit() = %v, want ErrIntegrity", name, err)
		}
	}
	// A blob whose content does not hash to its id.
	lie := base(Change{Path: "new.txt", Mode: "100644", OID: x})
	lie.Blobs = map[string]Blob{x: {OID: x, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("y\n")), nil }}}
	if _, err := tr.BuildCommit(t.Context(), lie); !errors.Is(err, ErrIntegrity) {
		t.Errorf("BuildCommit(lying blob) = %v, want ErrIntegrity", err)
	}
}

func TestBuildCommitInvalid(t *testing.T) {
	t.Parallel()
	s, tr, b := buildFixture(t)
	x := RawOID([]byte("x\n"))
	valid := func() CommitSpec {
		return CommitSpec{Parent: b, Changes: []Change{{Path: "new.txt", Mode: "100644", OID: x}},
			Blobs: hubBlobs("x\n"), Author: bot, Committer: bot, When: when, Message: msg}
	}
	if _, err := tr.BuildCommit(t.Context(), valid()); err != nil {
		t.Fatalf("BuildCommit(valid) = %v", err)
	}
	openErr := errors.New("hub blob unreadable")
	for name, mutate := range map[string]func(*CommitSpec){
		"short parent":   func(c *CommitSpec) { c.Parent = b[:12] },
		"missing parent": func(c *CommitSpec) { c.Parent = missingOID },
		"tree parent":    func(c *CommitSpec) { c.Parent = s.git("rev-parse", b+"^{tree}") },
		"no changes":     func(c *CommitSpec) { c.Changes = nil },
		"dot git":        func(c *CommitSpec) { c.Changes[0].Path = ".git/config" },
		"dot git case":   func(c *CommitSpec) { c.Changes[0].Path = ".GIT/hooks/x" },
		"dot dot":        func(c *CommitSpec) { c.Changes[0].Path = "a/../b" },
		"absolute":       func(c *CommitSpec) { c.Changes[0].Path = "/etc/passwd" },
		"gitmodules":     func(c *CommitSpec) { c.Changes[0].Path = ".gitmodules" },
		"backslash":      func(c *CommitSpec) { c.Changes[0].Path = `a\b` },
		"duplicate":      func(c *CommitSpec) { c.Changes = append(c.Changes, c.Changes[0]) },
		"symlink mode":   func(c *CommitSpec) { c.Changes[0].Mode = "120000" },
		"short oid":      func(c *CommitSpec) { c.Changes[0].OID = x[:7] },
		"no blob":        func(c *CommitSpec) { c.Blobs = nil },
		"nil open":       func(c *CommitSpec) { c.Blobs[x] = Blob{OID: x} },
		"open fails": func(c *CommitSpec) {
			c.Blobs[x] = Blob{OID: x, Open: func() (io.ReadCloser, error) { return nil, openErr }}
		},
		"name bracket":    func(c *CommitSpec) { c.Author.Name = "a <b>" },
		"name newline":    func(c *CommitSpec) { c.Committer.Name = "a\nb" },
		"name space":      func(c *CommitSpec) { c.Author.Name = " a" },
		"empty email":     func(c *CommitSpec) { c.Committer.Email = "" },
		"email bracket":   func(c *CommitSpec) { c.Author.Email = "a>b" },
		"empty message":   func(c *CommitSpec) { c.Message = " \n" },
		"nul message":     func(c *CommitSpec) { c.Message = "a\x00b" },
		"zero date":       func(c *CommitSpec) { c.When = time.Time{} },
		"old date":        func(c *CommitSpec) { c.When = time.Unix(-10, 0) },
		"sign fails":      func(c *CommitSpec) { c.Sign = func([]byte) (string, error) { return "", openErr } },
		"sign empty":      func(c *CommitSpec) { c.Sign = func([]byte) (string, error) { return "\n", nil } },
		"sign with CR":    func(c *CommitSpec) { c.Sign = func([]byte) (string, error) { return "a\r\nb", nil } },
		"change dir path": func(c *CommitSpec) { c.Changes[0].Path = "dir/" },
	} {
		spec := valid()
		spec.Blobs = hubBlobs("x\n")
		mutate(&spec)
		if _, err := tr.BuildCommit(t.Context(), spec); err == nil {
			t.Errorf("%s: BuildCommit succeeded", name)
		} else if name == "open fails" && !errors.Is(err, openErr) {
			t.Errorf("%s: BuildCommit() = %v, want the open error", name, err)
		}
	}
}

// baseWithNames serves a base commit on branch whose root holds a file
// under each of names, written with mktree (no path checks), and returns a
// target that fetched it and the commit.
func baseWithNames(t *testing.T, branch string, names ...string) (*TargetRepo, string) {
	t.Helper()
	s := newServed(t, t.TempDir(), "repo.git")
	blob := s.gitIn("x\n", "hash-object", "-w", "--stdin")
	var tree strings.Builder
	for _, n := range names {
		tree.WriteString("100644 blob " + blob + "\t" + n + "\n")
	}
	commit := s.gitIn("base\n", "commit-tree", s.gitIn(tree.String(), "mktree"))
	s.git("update-ref", "refs/heads/"+branch, commit)
	tr := newTarget(t, s.dir, Auth{})
	if sha, _, err := tr.FetchBranch(t.Context(), branch, 1); err != nil || sha != commit {
		t.Fatalf("FetchBranch() = %s, %v", sha, err)
	}
	return tr, commit
}

// TestBuildCommitWindowsNames: a base tree may hold names Windows cannot
// (legal in Linux repositories); nothing is checked out, so BuildCommit
// works on every OS. A drive prefix ("a:b.txt") is the one name Git for
// Windows refuses whatever the settings: a clear error there.
func TestBuildCommitWindowsNames(t *testing.T) {
	t.Parallel()
	names := []string{"CON", "aux.c", "keep.txt", "nul.txt", "q?.txt", "trail.", "trail ", "x<y>.md"}
	tr, b := baseWithNames(t, "main", names...)
	spec := CommitSpec{Parent: b, Changes: []Change{{Path: "new.txt", Mode: "100644", OID: RawOID([]byte("new\n"))}},
		Blobs: hubBlobs("new\n"), Author: bot, Committer: bot, When: when, Message: msg}
	built, err := tr.BuildCommit(t.Context(), spec)
	if err != nil {
		t.Fatalf("BuildCommit on a base with Windows-reserved names: %v", err)
	}
	entries, err := tr.Tree(t.Context(), built.Commit)
	if err != nil || len(entries) != len(names)+1 {
		t.Errorf("the tree has %d entries (%v), want %d", len(entries), err, len(names)+1)
	}

	drive, b := baseWithNames(t, "main", "a:b.txt", "keep.txt")
	spec.Parent = b
	_, err = drive.BuildCommit(t.Context(), spec)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "drive prefix") {
			t.Errorf("BuildCommit on a base with a:b.txt = %v, want the drive-prefix error", err)
		}
	} else if err != nil {
		t.Errorf("BuildCommit on a base with a:b.txt = %v", err)
	}
}

// sshKey generates an ed25519 key with ssh-keygen and returns its signer
// and public key line, skipping without ssh-keygen.
func sshKey(t *testing.T) *sshsig.Signer {
	t.Helper()
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skipf("ssh-keygen not found: %v", err)
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-C", "touchmark test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	data, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := sshsig.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// TestBuildCommitSigned: the SSHSIG signature covers the commit object
// without its gpgsig header, as git signs and verifies it.
func TestBuildCommitSigned(t *testing.T) {
	t.Parallel()
	signer := sshKey(t)
	s, tr, b := buildFixture(t)
	spec := syncSpec(s, b)
	plain, err := tr.BuildCommit(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	spec.Sign = func(payload []byte) (string, error) {
		payloads = append(payloads, bytes.Clone(payload))
		return signer.Sign(sshsig.Namespace, payload)
	}
	signed, err := tr.BuildCommit(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Tree != plain.Tree || signed.Commit == plain.Commit {
		t.Errorf("signed = %+v, plain = %+v", signed, plain)
	}
	objs, err := tr.catFile(t.Context(), []string{plain.Commit, signed.Commit}, MaxCommitObject)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 || !bytes.Equal(payloads[0], objs[0].content) {
		t.Errorf("the signed payload is not the unsigned commit object:\n%s\nwant\n%s", payloads, objs[0].content)
	}
	// The signed object is the unsigned one with the header inserted
	// before the blank line.
	head, body, _ := bytes.Cut(objs[1].content, []byte("\n\n"))
	var kept []string
	sig := ""
	for _, line := range strings.Split(string(head), "\n") {
		switch {
		case strings.HasPrefix(line, "gpgsig "):
			sig = strings.TrimPrefix(line, "gpgsig ") + "\n"
		case strings.HasPrefix(line, " ") && sig != "":
			sig += line[1:] + "\n"
		default:
			kept = append(kept, line)
		}
	}
	if got := strings.Join(kept, "\n") + "\n\n" + string(body); got != string(objs[0].content) {
		t.Errorf("signed object without gpgsig =\n%s\nwant\n%s", got, objs[0].content)
	}
	if !strings.HasPrefix(sig, "-----BEGIN SSH SIGNATURE-----\n") || !strings.HasSuffix(sig, "-----END SSH SIGNATURE-----\n") {
		t.Errorf("gpgsig = %q", sig)
	}
	// Deterministic: ed25519 signs the same payload the same way.
	if again, err := tr.BuildCommit(t.Context(), spec); err != nil || again != signed {
		t.Errorf("BuildCommit(signed again) = %+v, %v; want %+v", again, err, signed)
	}

	requireGit(t, gitSSHSign, "gpg.format=ssh")
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, []byte(bot.Email+" "+signer.PublicKey()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tr.Git.Run(t.Context(), nil, "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+filepath.ToSlash(allowed),
		"verify-commit", "-v", signed.Commit)
	if err != nil {
		t.Fatalf("git verify-commit: %v\n%s", err, out)
	}
	// And an unsigned commit does not verify.
	if _, err := tr.Git.Run(t.Context(), nil, "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+filepath.ToSlash(allowed),
		"verify-commit", plain.Commit); err == nil {
		t.Error("git verify-commit accepted the unsigned commit")
	}
}
