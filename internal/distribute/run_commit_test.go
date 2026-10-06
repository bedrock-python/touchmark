package distribute

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// Contents of the packs of the commit tests.
var (
	crlfText = "line one of a text file\r\nline two of a text file\r\n"
	runDoc   = version("Tools/run.md", 1)
)

// TestRunUnsafePaths runs the unsafe checks on B and on the new tree: a path
// the target's .gitattributes filters or re-encodes, one git would store as
// another blob (renormalize; text=auto keeps the hub's CRLF), and one whose
// directory clashes by case with the target's is left out of D, with a
// warning, and the commit is built again.
func TestRunUnsafePaths(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.pack("AGENTS.md", agentsV2, "docs/guide.md", guideV1, "notes/crlf.txt", crlfText, "Tools/run.md", runDoc)
	all := []string{"AGENTS.md", agentsV2, "docs/guide.md", guideV1, "notes/crlf.txt", crlfText, "Tools/run.md", runDoc}
	without := func(path string) []string {
		var out []string
		for i := 0; i+1 < len(all); i += 2 {
			if all[i] != path {
				out = append(out, all[i], all[i+1])
			}
		}
		return out
	}
	w.optedIn("acme/lfs", nil, ".gitattributes", "docs/** filter=lfs diff=lfs merge=lfs -text\n")
	w.optedIn("acme/text", nil, ".gitattributes", "*.txt text\n")
	w.optedIn("acme/auto", nil, ".gitattributes", "*.txt text=auto\n")
	w.optedIn("acme/case", nil, "tools/x.sh", "echo x\n")
	w.optedIn("acme/encoding", nil, ".gitattributes", "Tools/*.md working-tree-encoding=UTF-16LE\n")

	rep := w.both(nil)
	for path, c := range map[string]struct {
		unsafe string // "" for none
		why    string
	}{
		"acme/lfs":      {"docs/guide.md", "the default branch's .gitattributes gives it filter=lfs"},
		"acme/text":     {"notes/crlf.txt", "(renormalize)"},
		"acme/auto":     {"", ""},
		"acme/case":     {"Tools/run.md", `"Tools" differs only by case from "tools"`},
		"acme/encoding": {"Tools/run.md", "working-tree-encoding=UTF-16LE"},
	} {
		tg := want(t, rep, "gh:"+path, report.OutcomeOpened, "", 0)
		wantKey := keyOf(all...)
		if c.unsafe != "" {
			wantKey = keyOf(without(c.unsafe)...)
			if !hasWarningLike(tg, "unsafe: "+c.unsafe+": ", c.why) {
				t.Errorf("%s: warnings %q lack the unsafe %s (%s)", path, tg.Warnings, c.unsafe, c.why)
			}
		} else if hasWarningLike(tg, "unsafe:") {
			t.Errorf("%s: warnings %q", path, tg.Warnings)
		}
		if tg.Key != wantKey {
			t.Errorf("%s: key %s, want %s", path, tg.Key, wantKey)
		}
	}
}

// TestRunUnsafeFixpoint: a .gitattributes the pack ships makes other paths
// of D unsafe in the new tree; they leave D, and the commit of the second
// round (the .gitattributes alone) passes.
func TestRunUnsafeFixpoint(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	attrs := "*.md filter=lfs diff=lfs merge=lfs -text\n"
	w.pack(".gitattributes", attrs, "AGENTS.md", agentsV2, "docs/guide.md", guideV1)
	w.optedIn("acme/x", nil)
	rep := w.both(nil)
	tg := want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 0)
	if tg.Key != keyOf(".gitattributes", attrs) {
		t.Errorf("key %s, want the .gitattributes alone", tg.Key)
	}
	for _, p := range []string{"AGENTS.md", "docs/guide.md"} {
		if !hasWarningLike(tg, "unsafe: "+p+": ", "the new tree's .gitattributes gives it filter=lfs") {
			t.Errorf("warnings %q lack %s", tg.Warnings, p)
		}
	}
	if tg.Changes != (report.ChangeCounts{Create: 1}) {
		t.Errorf("changes %+v", tg.Changes)
	}
	w2, _ := w.inspectOne(w.deps(ModePlan), ModePlan, "acme/x")
	if len(w2.D) != 1 || w2.Built.Commit == "" {
		t.Errorf("D %v, commit %q", w2.D, w2.Built.Commit)
	}
}

// TestRunWorkflows: on GitHub a push that changes .github/workflows needs
// the Workflows permission: without it a new pull request is
// blocked:permission:workflows; a move over someone else's workflow change
// asks for Update branch in the open pull request's body.
func TestRunWorkflows(t *testing.T) {
	t.Parallel()
	t.Run("in D", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t)
		wf := "on: push\njobs: {}\n"
		w.pack("AGENTS.md", agentsV2, ".github/workflows/ci.yml", wf)
		w.optedIn("acme/x", nil)
		rep := w.both(nil)
		want(t, rep, "gh:acme/x", report.OutcomeBlocked, "permission:workflows", 0)
		can := func(d *Deps) { d.Write.CanWorkflows = map[string]bool{"gh": true} }
		rep = w.both(can)
		want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 0)
		d := w.deps(ModeDryRun)
		can(&d)
		wk, res := w.inspectOne(d, ModeDryRun, "acme/x")
		switch {
		case wk == nil:
			t.Errorf("no work: %s:%s", res.Outcome, res.Reason)
		case wk.NeedPerms != (platform.Perms{Contents: true, PRs: true, Workflows: true}):
			t.Errorf("perms %+v", wk.NeedPerms)
		}
	})
	t.Run("moved over", func(t *testing.T) {
		t.Parallel()
		w := newGitWorld(t)
		r := w.optedIn("acme/x", nil)
		old := []string{"AGENTS.md", agentsV1, "docs/guide.md", guideV1}
		w.syncCommit(r, branch, "", old...)
		w.written(r, w.ownOn(r, branch, keyOf(old...), old...))
		w.push(r, "main", w.person, ".github/workflows/theirs.yml", "on: push\n")
		rep := w.both(nil)
		tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "permission:workflows", 1)
		if tg.Writes != 1 {
			t.Errorf("%d writes, want 1 (the Update branch block)", tg.Writes)
		}
		wk, _ := w.inspectOne(w.deps(ModePlan), ModePlan, "acme/x")
		if !wk.Decision.Blocks.UpdateBranchNeeded {
			t.Errorf("blocks %+v", wk.Decision.Blocks)
		}
		rep = w.both(func(d *Deps) { d.Write.CanWorkflows = map[string]bool{"gh": true} })
		want(t, rep, "gh:acme/x", report.OutcomeUpdated, "content", 1)
	})
}

// TestRunSigning: sign: always without a way to sign blocks every push of
// a dry run before it (blocked:cannot-sign; a plan, which never holds the
// key, plans the push); with the provider's key the commit carries its SSH
// signature.
func TestRunSigning(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML = strings.Replace(defaultHubYML, "    writer: acme-write[bot]\n", "    writer: acme-write[bot]\n    sign: always\n", 1)
	w.optedIn("acme/x", nil)
	w.optedIn("acme/current", nil, baseFiles...)
	plan := w.run(w.deps(ModePlan), ModePlan)
	want(t, plan, "gh:acme/x", report.OutcomeOpened, "", 0)
	rep := w.run(w.deps(ModeDryRun), ModeDryRun)
	tg := want(t, rep, "gh:acme/x", report.OutcomeBlocked, "cannot-sign", 0)
	if tg.Writes != 0 || !hasWarningLike(tg, "sign: always", "SIGNING_KEY") {
		t.Errorf("writes %d, warnings %q", tg.Writes, tg.Warnings)
	}
	want(t, rep, "gh:acme/current", report.OutcomeUnchanged, "", 0)

	signer, err := sshsig.ParsePrivateKey(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	signed := func(d *Deps) { d.Write.Signers = map[string]*sshsig.Signer{"gh": signer} }
	rep = w.both(signed)
	want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 0)
	d := w.deps(ModeDryRun)
	signed(&d)
	wk, _ := w.inspectOne(d, ModeDryRun, "acme/x")
	c, err := wk.Repo.Git.Run(t.Context(), nil, "cat-file", "commit", wk.Built.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(c, []byte("\ngpgsig -----BEGIN SSH SIGNATURE-----\n")) {
		t.Errorf("the commit is not signed:\n%s", c)
	}
	if !bytes.Contains(c, []byte("author acme-write[bot] <"+w.writer.Email+">")) {
		t.Errorf("the commit's author:\n%s", c)
	}
}

// testKey returns an unencrypted OpenSSH ed25519 private key (PROTOCOL.key),
// built here so that no private key sits in the repository.
func testKey(t *testing.T) []byte {
	t.Helper()
	str := func(b, s []byte) []byte { return append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...) }
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x17}, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	pubBlob := str(str(nil, []byte("ssh-ed25519")), pub)
	sec := binary.BigEndian.AppendUint32(nil, 0x0badcafe)
	sec = binary.BigEndian.AppendUint32(sec, 0x0badcafe)
	sec = str(sec, []byte("ssh-ed25519"))
	sec = str(sec, pub)
	sec = str(sec, priv)
	sec = str(sec, []byte("touchmark test"))
	for i := byte(1); len(sec)%8 != 0; i++ {
		sec = append(sec, i)
	}
	b := []byte("openssh-key-v1\x00")
	b = str(b, []byte("none"))
	b = str(b, []byte("none"))
	b = str(b, nil)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = str(b, pubBlob)
	b = str(b, sec)
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b})
}
