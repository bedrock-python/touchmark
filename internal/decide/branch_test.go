package decide

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/provenance"
)

// btSHA returns a distinct commit id for n.
func btSHA(n int) string { return fmt.Sprintf("%040x", n) }

// btTrailers returns the trailers touchmark writes for pairs in stream,
// under the hub fingerprint fp.
func btTrailers(fp, stream string, pairs []Pair) Trailers {
	return Trailers{HubID: "acme-eng", Fingerprint: fp, Stream: stream, Content: Key(stream, pairs), HubCommit: btSHA(0xabc)}
}

// btMessage returns a commit message with the trailer block of t.
func btMessage(t Trailers) string {
	return "chore: sync engineering assets\n\n" + FormatTrailers(t)
}

// btOurs is a touchmark commit on parents that brings pairs in stream.
func btOurs(sha string, parents []string, pairs []Pair, fp, stream string) HistoryCommit {
	return HistoryCommit{SHA: sha, Parents: parents, Message: btMessage(btTrailers(fp, stream, pairs)), Pairs: pairs}
}

// btPlain is a person's commit on parent.
func btPlain(sha, parent string) HistoryCommit {
	return HistoryCommit{SHA: sha, Parents: []string{parent}, Message: "fix: tweak the agents file\n"}
}

// btMerge is a merge commit of p1 and p2 with what the caller learned.
func btMerge(sha, p1, p2 string, clean bool, ancestor Tri) HistoryCommit {
	return HistoryCommit{SHA: sha, Parents: []string{p1, p2}, Message: "Merge branch 'main' into touchmark/acme-eng\n", CleanMerge: clean, BaseAncestor: ancestor}
}

func TestFormatTrailers(t *testing.T) {
	tr := Trailers{
		HubID:       "acme-eng",
		Fingerprint: "github.com/712345678",
		Stream:      StreamSync,
		Content:     "sha256:6b1f000000000000000000000000000000000000000000000000000000000000",
		HubCommit:   "3f2c1ab900000000000000000000000000000000",
	}
	want := "Touchmark-Hub: acme-eng@github.com/712345678\n" +
		"Touchmark-Stream: sync\n" +
		"Touchmark-Content: sha256:6b1f000000000000000000000000000000000000000000000000000000000000\n" +
		"Touchmark-Hub-Commit: 3f2c1ab900000000000000000000000000000000"
	if got := FormatTrailers(tr); got != want {
		t.Fatalf("FormatTrailers =\n%s\nwant\n%s", got, want)
	}
	if got, ok := ParseTrailers("chore: sync engineering assets\n\n" + want + "\n"); !ok || got != tr {
		t.Errorf("ParseTrailers(FormatTrailers) = %+v, %v; want %+v", got, ok, tr)
	}
}

func TestParseTrailers(t *testing.T) {
	d := mixedPairs()
	ours := btTrailers(ownFP, StreamSync, d)
	block := FormatTrailers(ours)
	const title = "chore: sync engineering assets\n\n"
	withHub := func(v string) string {
		return title + "Touchmark-Hub: " + v + "\nTouchmark-Stream: sync"
	}
	hubOnly := func(id, fp string) Trailers { return Trailers{HubID: id, Fingerprint: fp, Stream: StreamSync} }
	cases := []struct {
		name    string
		message string
		want    Trailers
		ok      bool
	}{
		{"ours", title + block, ours, true},
		{"ours with a final newline", title + block + "\n", ours, true},
		{"ours after a body", "chore: sync\n\nThe body explains.\nOver two lines.\n\n" + block + "\n", ours, true},
		{"CRLF", strings.ReplaceAll(title+block, "\n", "\r\n") + "\r\n", ours, true},
		{"trailing blank lines", title + block + "\n\n \n\t\n", ours, true},
		{"trailing comments", title + block + "\n# Please enter the commit message\n#\n", ours, true},
		{"comment inside the block", title + "Touchmark-Hub: acme-eng@github.com/712345678\n# note\nTouchmark-Stream: sync\n", hubOnly("acme-eng", ownFP), true},
		{"other trailers around", title + "Signed-off-by: A U Thor <a@example.com>\n" + block + "\nCo-authored-by: B <b@example.com>\n", ours, true},
		{"keys in another case", title + strings.ToLower(block), Trailers{
			HubID: "acme-eng", Fingerprint: ownFP, Stream: "sync", Content: ours.Content, HubCommit: ours.HubCommit,
		}, true},
		{"space before the colon", title + "Touchmark-Hub : acme-eng@github.com/712345678\nTouchmark-Stream\t: sync", hubOnly("acme-eng", ownFP), true},
		{"no space after the colon", title + "Touchmark-Hub:acme-eng@github.com/712345678\nTouchmark-Stream:sync", hubOnly("acme-eng", ownFP), true},
		{"values trimmed", title + "Touchmark-Hub:   acme-eng@github.com/712345678   \nTouchmark-Stream: sync \t", hubOnly("acme-eng", ownFP), true},
		{"folded value", title + "Touchmark-Hub: acme-eng@github.com/712345678\nTouchmark-Stream: sync\nTouchmark-Content: sha256:ab\n  cd\n\tef", Trailers{
			HubID: "acme-eng", Fingerprint: ownFP, Stream: "sync", Content: "sha256:ab cd ef",
		}, true},
		{"port in the fingerprint", withHub("old-id@gitlab.example.com:8443/1234"), hubOnly("old-id", "gitlab.example.com:8443/1234"), true},
		{"stream absent", title + "Touchmark-Hub: acme-eng@github.com/712345678", Trailers{HubID: "acme-eng", Fingerprint: ownFP}, true},
		{"leading blank lines", "\n \r\n\t\n" + title + block, ours, true},
		{"a --- line is not a divider", "chore: sync\n\nbody\n---\nmore body\n\n" + block, ours, true},
		{"signed-off lets a free line in", title + "Signed-off-by: A <a@example.com>\nA free line\n" + block, ours, true},
		{"cherry-pick note lets a free line in", title + "(cherry picked from commit 0123456789abcdef)\nA free line\n" + block, ours, true},
		{"old conflicts block after the trailers", title + block + "\n\nConflicts:\n\tAGENTS.md\n", ours, true},

		{"empty message", "", Trailers{}, false},
		{"no trailers", "chore: sync engineering assets\n\nJust a body.\n", Trailers{}, false},
		{"title only", "Touchmark-Hub: acme-eng@github.com/712345678\nTouchmark-Stream: sync\n", Trailers{}, false},
		// git starts a commit's message after its leading blank lines, so
		// the block below them is the title.
		{"title only after blank lines", "\n\t\n" + block, Trailers{}, false},
		{"not the last paragraph", title + block + "\n\nA closing paragraph.\n", Trailers{}, false},
		{"free line in the block", title + block + "\nA free line\n", Trailers{}, false},
		{"less than one in four", title + "Signed-off-by: A <a@example.com>\nfree 1\nfree 2\nfree 3\nfree 4\nfree 5\nfree 6\nfree 7\nTouchmark-Hub: acme-eng@github.com/712345678", Trailers{}, false},
		{"below a scissors line", title + "body\n\n# ------------------------ >8 ------------------------\n" + block, Trailers{}, false},
		// git 2.47 cuts only at "…>8…\n"; a CRLF scissors line is a comment.
		{"a CRLF scissors line is a comment", strings.ReplaceAll(title+"# ------------------------ >8 ------------------------\n"+block, "\n", "\r\n"), ours, true},
		{"hub absent", title + "Touchmark-Stream: sync\nTouchmark-Content: " + ours.Content, Trailers{}, false},
		{"hub twice", title + block + "\nTouchmark-Hub: acme-eng@github.com/712345678", Trailers{}, false},
		{"hub twice, other case", title + block + "\ntouchmark-hub: other@github.com/1", Trailers{}, false},
		{"stream twice", title + block + "\nTouchmark-Stream: adopt", Trailers{}, false},
		{"content twice, same value", title + block + "\nTouchmark-Content: " + ours.Content, Trailers{}, false},
		{"hub commit twice", title + block + "\nTouchmark-Hub-Commit: " + btSHA(1), Trailers{}, false},
		{"hub without @", withHub("github.com/712345678"), Trailers{}, false},
		{"hub without id", withHub("@github.com/712345678"), Trailers{}, false},
		{"hub without fingerprint", withHub("acme-eng@"), Trailers{}, false},
		{"hub id in capitals", withHub("Acme@github.com/712345678"), Trailers{}, false},
		{"hub id with a double hyphen", withHub("acme--eng@github.com/712345678"), Trailers{}, false},
		{"fingerprint without id", withHub("acme-eng@github.com"), Trailers{}, false},
		{"fingerprint with a letter in the id", withHub("acme-eng@github.com/7123x"), Trailers{}, false},
		{"fingerprint with a long port", withHub("acme-eng@gitlab.example.com:123456/1"), Trailers{}, false},
		{"fingerprint with a path", withHub("acme-eng@github.com/acme/712345678"), Trailers{}, false},
		{"two @", withHub("acme-eng@x@github.com/712345678"), Trailers{}, false},
		{"hub value folded", title + "Touchmark-Hub: acme-eng@github.com/712345678\n  more", Trailers{}, false},
		{"hub value with a space", withHub("acme-eng @github.com/712345678"), Trailers{}, false},
		{"indented trailer line", title + "  Touchmark-Hub: acme-eng@github.com/712345678", Trailers{}, false},
		{"trailer in a comment", title + "Touchmark-Stream: sync\n# Touchmark-Hub: acme-eng@github.com/712345678", Trailers{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseTrailers(c.message)
			if ok != c.ok || got != c.want {
				t.Fatalf("ParseTrailers = %+v, %v; want %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

// TestParseTrailersRoundTrip: whatever valid values and commit message
// touchmark writes, and whatever trailers people add to the block, the
// trailers read back as written.
func TestParseTrailersRoundTrip(t *testing.T) {
	ids := []string{"acme-eng", "a", "x1-y2-z3", "old-id"}
	fps := []string{"github.com/712345678", "gitlab.example.com:8443/1234", "git.example.org/1", "GHE.example.com/99"}
	titles := []string{
		"chore: sync engineering assets",
		"chore: sync engineering assets\n\nThe hub ships these files.",
		"chore: sync\n\n---\nA rule and a body.\n\nKey: value in the body",
		"chore: sync\n\nRefs: #12",
	}
	extras := []string{"", "\nSigned-off-by: A U Thor <a@example.com>", "\nCo-authored-by: B <b@example.com>\nReviewed-by: C <c@example.com>"}
	r := rand.New(rand.NewPCG(7, 11))
	for i := range 500 {
		pairs := mixedPairs()[:1+r.IntN(4)]
		stream := []string{StreamSync, StreamAdopt}[r.IntN(2)]
		want := Trailers{
			HubID:       ids[r.IntN(len(ids))],
			Fingerprint: fps[r.IntN(len(fps))],
			Stream:      stream,
			Content:     Key(stream, pairs),
			HubCommit:   btSHA(r.IntN(1 << 20)),
		}
		msg := titles[r.IntN(len(titles))] + "\n\n"
		if r.IntN(3) == 0 {
			msg += "Signed-off-by: A U Thor <a@example.com>\n"
		}
		msg += FormatTrailers(want) + extras[r.IntN(len(extras))]
		if r.IntN(2) == 0 {
			msg += "\n"
		}
		if r.IntN(4) == 0 {
			msg = strings.ReplaceAll(msg, "\n", "\r\n")
		}
		got, ok := ParseTrailers(msg)
		if !ok || got != want {
			t.Fatalf("case %d: ParseTrailers = %+v, %v; want %+v\nmessage: %q", i, got, ok, want, msg)
		}
	}
}

// FuzzParseTrailers: whatever a commit message holds, ParseTrailers does
// not panic, accepts only a valid hub value, and what it accepts is
// written back by FormatTrailers and read again unchanged.
func FuzzParseTrailers(f *testing.F) {
	block := FormatTrailers(btTrailers(ownFP, StreamSync, mixedPairs()))
	for _, m := range append(btTrailerCorpus(rand.New(rand.NewPCG(3, 4)), 50),
		"chore: sync\n\n"+block, "chore: sync\r\n\r\n"+strings.ReplaceAll(block, "\n", "\r\n"),
		"t\n\nTouchmark-Hub: a@b.c:1/2\n  x", "t\n\nConflicts:\n\tp", "# ------------------------ >8 ------------------------\n",
	) {
		f.Add(m)
	}
	f.Fuzz(func(t *testing.T, message string) {
		tr, ok := ParseTrailers(message)
		if !ok {
			if tr != (Trailers{}) {
				t.Fatalf("not ok with trailers %+v", tr)
			}
			return
		}
		if !trailerHubIDRe.MatchString(tr.HubID) || !trailerFingerprintRe.MatchString(tr.Fingerprint) {
			t.Fatalf("accepted hub %q@%q", tr.HubID, tr.Fingerprint)
		}
		if again, ok := ParseTrailers("chore: sync\n\n" + FormatTrailers(tr)); !ok || again != tr {
			t.Fatalf("round trip: %+v, %v; want %+v", again, ok, tr)
		}
	})
}

// btTrailerFragments are the lines random messages are made of: trailers of
// ours and others', continuations, comments, dividers, scissors and
// conflicts blocks, and near-trailers.
var btTrailerFragments = []string{
	"", " ", "\t", "\f", "title line", "Some body text.", "Another line of text",
	"Touchmark-Hub: acme-eng@github.com/712345678",
	"touchmark-HUB: old-id@gitlab.example.com:8443/1234",
	"Touchmark-Hub : acme-eng@github.com/712345678",
	"Touchmark-Hub:acme-eng@github.com/1",
	"Touchmark-Hub: bad value",
	"Touchmark-Stream: sync",
	"Touchmark-Stream:   adopt   ",
	"Touchmark-Content: sha256:6b1f000000000000000000000000000000000000000000000000000000000000",
	"Touchmark-Hub-Commit: 3f2c1ab900000000000000000000000000000000",
	"Signed-off-by: A U Thor <author@example.com>",
	"(cherry picked from commit 0123456789abcdef0123456789abcdef01234567)",
	"Co-authored-by: B <b@example.com>",
	"Key-Only:",
	"Key:value-without-space",
	"-dash-start: value",
	"Two words: not a trailer",
	": no token",
	"  continued value",
	"\tcontinued with a tab",
	"# a comment",
	"#",
	"---",
	"--- ",
	"# ------------------------ >8 ------------------------",
	"Conflicts:",
	"\tconflicted/path.txt",
	"Key\v: vertical tab",
}

// btTrailerCorpus returns n random messages built from the fragments, some
// with a title paragraph, some with CRLF line ends. A CRLF message has no
// scissors line: git builds disagree on whether "# --- >8 ---\r\n" cuts
// the message (Git for Windows 2.33 does, git 2.47 does not), and
// ParseTrailers follows 2.47.
func btTrailerCorpus(r *rand.Rand, n int) []string {
	out := make([]string, 0, n)
	for range n {
		sep := "\n"
		if r.IntN(4) == 0 {
			sep = "\r\n"
		}
		lines := make([]string, 1+r.IntN(9))
		for i := range lines {
			lines[i] = btTrailerFragments[r.IntN(len(btTrailerFragments))]
			if sep == "\r\n" && lines[i]+"\n" == trailerScissors {
				lines[i] = "# a comment"
			}
		}
		if r.IntN(3) > 0 {
			lines = append([]string{"chore: title", ""}, lines...)
		}
		msg := strings.Join(lines, sep)
		if r.IntN(2) == 0 {
			msg += sep
		}
		out = append(out, msg)
	}
	return out
}

// btGit runs git with the machine's and the user's configuration out of the
// way, stdin as its input, and returns its standard output.
func btGit(t *testing.T, stdin []byte, args ...string) []byte {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+global,
		"HOME="+home,
		"XDG_CONFIG_HOME="+home,
		"GIT_CEILING_DIRECTORIES="+os.TempDir(),
	)
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = home, env
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// TestTrailerBlockMatchesGit compares the trailer block ParseTrailers reads
// with the one git reads over table and random messages: the same
// trailers, keys and unfolded values, in the same order. git reads them as
// commit messages: one fast-import writes a commit per message, one log
// prints %(trailers:only,unfold), git's `interpret-trailers --parse
// --no-divider` in log form.
func TestTrailerBlockMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	d := mixedPairs()
	block := FormatTrailers(btTrailers(ownFP, StreamSync, d))
	messages := []string{
		"", "chore: sync\n\n" + block, "chore: sync\n\n" + block + "\n",
		strings.ReplaceAll("chore: sync\n\n"+block+"\n", "\n", "\r\n"),
		"chore: sync\n\nbody\n---\nmore\n\n" + block,
		"chore: sync\n\n" + block + "\n\nConflicts:\n\tAGENTS.md\n",
		"chore: sync\n\nbody\n\n# ------------------------ >8 ------------------------\n" + block,
		"# ------------------------ >8 ------------------------\n" + block,
		"#a\n\n" + block, "\n\n" + block, "chore\n\n" + block + "\n#\n\n#\n",
	}
	n := 2000
	if testing.Short() {
		n = 200
	}
	messages = append(messages, btTrailerCorpus(rand.New(rand.NewPCG(1, 2)), n)...)
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	btGit(t, nil, "init", "-q", "--bare", gitDir)
	// One commit per message, chained on one branch: a ref per message
	// would cost a file each.
	var stream bytes.Buffer
	for i, m := range messages {
		fmt.Fprintf(&stream, "commit refs/heads/m\ncommitter T <t@example.com> %d +0000\ndata %d\n%s\n", 1000000000+i, len(m), m)
	}
	btGit(t, stream.Bytes(), "--git-dir="+gitDir, "fast-import", "--quiet")
	out := string(btGit(t, nil, "--git-dir="+gitDir, "log", "--reverse", "--format=%(trailers:only,unfold)%x00", "refs/heads/m"))
	chunks := strings.Split(out, "\x00")
	if len(chunks) != len(messages)+1 {
		t.Fatalf("git printed %d messages, want %d", len(chunks)-1, len(messages))
	}
	got := make([][]trailerKV, len(messages))
	for i, chunk := range chunks[:len(messages)] {
		if i > 0 {
			chunk = strings.TrimPrefix(chunk, "\n") // the newline log puts after each commit
		}
		for line := range strings.Lines(chunk) {
			key, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), ": ")
			got[i] = append(got[i], trailerKV{key: key, value: value})
		}
	}
	for i, m := range messages {
		if mine := trailerBlock(m); !slices.Equal(mine, got[i]) {
			t.Errorf("message %d %q:\n got %q\ngit %q", i, m, mine, got[i])
		}
	}
	// Guard against a corpus that stops reaching trailers.
	blocks, ours := 0, 0
	for i, m := range messages {
		if len(got[i]) > 0 {
			blocks++
		}
		if _, ok := ParseTrailers(m); ok {
			ours++
		}
	}
	if blocks < len(messages)/10 || ours < len(messages)/50 {
		t.Errorf("of %d messages, %d have trailers and %d parse as touchmark's: the corpus is too thin", len(messages), blocks, ours)
	}
}

func TestClassifyBranch(t *testing.T) {
	fps := []string{ownFP, ownPrevFP}
	d := mixedPairs()
	sorted := slices.Clone(d)
	slices.SortFunc(sorted, branchComparePairs)
	key := Key(StreamSync, d)
	base, old := btSHA(1), btSHA(2) // B, and an older base
	hc := btOurs(btSHA(10), []string{base}, d, ownFP, StreamSync)
	mergeOld := btSHA(3)
	history := func(commits ...HistoryCommit) BranchHistory {
		h := BranchHistory{Name: ownBranch, Exists: true, Base: base, Commits: commits}
		if len(commits) > 0 {
			h.Head = commits[0].SHA
		}
		return h
	}
	// onto returns c with its first parent and the proof that it is on the
	// default branch.
	onto := func(c HistoryCommit, parent string, proof Tri) HistoryCommit {
		c.Parents = append([]string{parent}, c.Parents[1:]...)
		c.ParentBaseAncestor = proof
		return c
	}
	// someone is a person's commit F on B, below a cherry-picked Hc.
	someone := btSHA(50)
	// rewritable is the classification of a branch whose head is head and
	// whose E is e, with hc as Hc.
	rewritable := func(head, e string, isHead bool) Branch {
		return Branch{Name: ownBranch, Head: head, State: BranchRewritable, Hc: hc.SHA, C: sorted, CKey: key, E: e, HcIsHead: isHead, HcParent: base}
	}
	edited := func(head string, isHead bool) Branch {
		return Branch{Name: ownBranch, Head: head, State: BranchEdited, Hc: hc.SHA, C: sorted, CKey: key, HcIsHead: isHead, HcParent: base}
	}
	foreign := func(head string) Branch { return Branch{Name: ownBranch, Head: head, State: BranchForeign} }
	// plains returns n commits of people, newest first, the oldest on
	// parent, followed by rest.
	plains := func(n int, parent string, rest ...HistoryCommit) []HistoryCommit {
		var out []HistoryCommit
		for i := range n {
			p := btSHA(101 + i)
			if i == n-1 {
				p = parent
			}
			out = append(out, btPlain(btSHA(100+i), p))
		}
		return append(out, rest...)
	}
	cases := []struct {
		name   string
		h      BranchHistory
		fps    []string // nil: ownFP and ownPrevFP
		stream string
		want   Branch
		detail string // a substring of Detail; "" means Detail is empty
	}{
		{"absent", BranchHistory{Name: ownBranch, Head: "stale"}, nil, StreamSync, Branch{Name: ownBranch}, ""},
		{"ours at the head", history(hc), nil, StreamSync, rewritable(hc.SHA, base, true), ""},
		{"clean base merges after ours", history(
			btMerge(btSHA(12), btSHA(11), btSHA(5), true, TriYes),
			btMerge(btSHA(11), hc.SHA, mergeOld, true, TriYes),
			hc,
		), nil, StreamSync, rewritable(btSHA(12), btSHA(5), false), ""},
		{"previous fingerprint", history(btOurs(hc.SHA, []string{base}, d, ownPrevFP, StreamSync)), nil, StreamSync, rewritable(hc.SHA, base, true), ""},
		{"another hub id, our fingerprint", history(HistoryCommit{
			SHA: hc.SHA, Parents: []string{base}, Pairs: d,
			Message: btMessage(Trailers{HubID: "old-id", Fingerprint: ownFP, Stream: StreamSync, Content: key, HubCommit: btSHA(7)}),
		}), nil, StreamSync, rewritable(hc.SHA, base, true), ""},
		{"adopt stream", history(btOurs(hc.SHA, []string{base}, d, ownFP, StreamAdopt)), nil, StreamAdopt, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchRewritable, Hc: hc.SHA, C: sorted, CKey: Key(StreamAdopt, d), E: base, HcIsHead: true, HcParent: base,
		}, ""},
		{"our root commit", history(btOurs(hc.SHA, nil, d, ownFP, StreamSync)), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchRewritable, Hc: hc.SHA, C: sorted, CKey: key, HcIsHead: true,
		}, ""},
		{"the newest of ours counts", history(
			onto(btOurs(btSHA(20), []string{hc.SHA}, d[:2], ownFP, StreamSync), hc.SHA, TriYes),
			hc,
		), nil, StreamSync, Branch{
			Name: ownBranch, Head: btSHA(20), State: BranchRewritable, Hc: btSHA(20), C: []Pair{d[1], d[0]}, CKey: Key(StreamSync, d[:2]),
			E: hc.SHA, HcIsHead: true, HcParent: hc.SHA,
		}, ""},
		{"ours at the depth limit", history(plains(MaxHistory-1, hc.SHA, hc)...), nil, StreamSync, edited(btSHA(100), false), "commit " + btSHA(100 + MaxHistory - 2)[:12]},
		// The base moved on since touchmark's commit: its parent is an older
		// B, proven to be on the default branch.
		{"ours on an older base, proven", history(onto(hc, old, TriYes)), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchRewritable, Hc: hc.SHA, C: sorted, CKey: key, E: old, HcIsHead: true, HcParent: old,
		}, ""},
		{"ours on the base, proof not needed", history(onto(hc, base, TriNo)), nil, StreamSync, rewritable(hc.SHA, base, true), ""},

		{"no commit of ours", history(plains(3, base)...), nil, StreamSync, foreign(btSHA(100)), "none of the last 3 commits"},
		{"another hub", history(btOurs(hc.SHA, []string{base}, d, ownOther, StreamSync)), nil, StreamSync, foreign(hc.SHA), "none of the last 1"},
		{"another stream", history(btOurs(hc.SHA, []string{base}, d, ownFP, StreamAdopt)), nil, StreamSync, foreign(hc.SHA), "none of the last 1"},
		{"no fingerprints", history(hc), []string{}, StreamSync, foreign(hc.SHA), "none of the last 1"},
		{"ours beyond the depth limit", history(plains(MaxHistory, hc.SHA, hc)...), nil, StreamSync, foreign(btSHA(100)), fmt.Sprintf("none of the last %d", MaxHistory)},
		{"truncated before ours", func() BranchHistory { h := history(plains(2, base)...); h.Truncated = true; return h }(), nil, StreamSync, foreign(btSHA(100)), "ends before"},
		{"trailers that do not parse", history(HistoryCommit{
			SHA: hc.SHA, Parents: []string{base}, Pairs: d,
			Message: btMessage(btTrailers(ownFP, StreamSync, d)) + "\nTouchmark-Hub: acme-eng@github.com/712345678",
		}), nil, StreamSync, foreign(hc.SHA), "none of the last 1"},
		{"trailers not in the last paragraph", history(HistoryCommit{
			SHA: hc.SHA, Parents: []string{base}, Pairs: d,
			Message: btMessage(btTrailers(ownFP, StreamSync, d)) + "\n\nA note added later.",
		}), nil, StreamSync, foreign(hc.SHA), "none of the last 1"},
		{"empty history", BranchHistory{Name: ownBranch, Exists: true, Head: hc.SHA}, nil, StreamSync, foreign(hc.SHA), "none of the last 0"},

		{"our commit changed", history(HistoryCommit{
			SHA: hc.SHA, Parents: []string{base}, Pairs: d[:3], Message: hc.Message,
		}), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchEdited, Hc: hc.SHA, C: []Pair{d[1], d[2], d[0]}, CKey: key, HcIsHead: true, HcParent: base,
		}, "was changed"},
		{"a commit after ours", history(btPlain(btSHA(11), hc.SHA), hc), nil, StreamSync, edited(btSHA(11), false), "commit " + btSHA(11)[:12] + " was added"},
		{"a merge that is not clean", history(btMerge(btSHA(11), hc.SHA, mergeOld, false, TriYes), hc), nil, StreamSync, edited(btSHA(11), false), "beyond its parents"},
		{"a merge of another branch", history(btMerge(btSHA(11), hc.SHA, mergeOld, true, TriNo), hc), nil, StreamSync, edited(btSHA(11), false), "not on the default branch"},
		{"a merge not proven to be of the base", history(btMerge(btSHA(11), hc.SHA, mergeOld, true, TriUnknown), hc), nil, StreamSync, edited(btSHA(11), false), "could not be proven"},
		{"an octopus merge", history(HistoryCommit{
			SHA: btSHA(11), Parents: []string{hc.SHA, mergeOld, old}, CleanMerge: true, BaseAncestor: TriYes,
		}, hc), nil, StreamSync, edited(btSHA(11), false), "3 parents"},
		{"the oldest offender is named", history(
			btPlain(btSHA(12), btSHA(11)),
			btMerge(btSHA(11), hc.SHA, mergeOld, false, TriYes),
			hc,
		), nil, StreamSync, edited(btSHA(12), false), "merge " + btSHA(11)[:12]},
		{"a commit of ours in another stream after ours", history(
			btOurs(btSHA(11), []string{hc.SHA}, d, ownFP, StreamAdopt),
			hc,
		), nil, StreamSync, edited(btSHA(11), false), "was added"},
		{"truncated after ours", func() BranchHistory { h := history(hc); h.Truncated = true; return h }(), nil, StreamSync, edited(hc.SHA, true), "truncated"},
		{"history not from the head", func() BranchHistory { h := history(hc); h.Head = btSHA(99); return h }(), nil, StreamSync, edited(btSHA(99), true), "does not start at the branch head"},
		{"a commit missing from the chain", history(
			btMerge(btSHA(12), btSHA(11), btSHA(5), true, TriYes),
			hc,
		), nil, StreamSync, edited(btSHA(12), false), "not the branch's first-parent chain"},
		{"our commit lists a path twice", history(HistoryCommit{
			SHA: hc.SHA, Parents: []string{base}, Message: btMessage(btTrailers(ownFP, StreamSync, d)), Pairs: append(slices.Clone(d), d[0]),
		}), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchEdited, Hc: hc.SHA, C: []Pair{d[1], d[2], d[0], d[0], d[3]}, CKey: key, HcIsHead: true, HcParent: base,
		}, "lists a path twice"},

		// Commits below Hc (invariant I2, see package distribute; threat T8 of
		// docs/project/threat-model.md): a person resets the branch to B,
		// commits F and cherry-picks touchmark's commit onto it (or reorders
		// them with rebase -i). Hc' keeps message and diff, so its key
		// matches; a rewrite from B would drop F.
		{"ours cherry-picked onto someone's commit", history(onto(hc, someone, TriNo)), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchEdited, Hc: hc.SHA, C: sorted, CKey: key, HcIsHead: true, HcParent: someone,
		}, "sits on commits that are not on the default branch"},
		{"ours on a parent not proven to be on the base", history(onto(hc, someone, TriUnknown)), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchEdited, Hc: hc.SHA, C: sorted, CKey: key, HcIsHead: true, HcParent: someone,
		}, "could not be proven to sit on the default branch"},
		{"ours on an older base, no base given", func() BranchHistory { h := history(onto(hc, old, TriUnknown)); h.Base = ""; return h }(), nil, StreamSync, Branch{
			Name: ownBranch, Head: hc.SHA, State: BranchEdited, Hc: hc.SHA, C: sorted, CKey: key, HcIsHead: true, HcParent: old,
		}, "could not be proven"},
		{"the parent of ours checked before the merges after it", history(
			btMerge(btSHA(11), hc.SHA, mergeOld, true, TriYes),
			onto(hc, someone, TriNo),
		), nil, StreamSync, Branch{
			Name: ownBranch, Head: btSHA(11), State: BranchEdited, Hc: hc.SHA, C: sorted, CKey: key, HcParent: someone,
		}, "sits on commits"},
		{"a merge that carries our trailers", history(HistoryCommit{
			SHA: hc.SHA, Parents: []string{base, someone}, Message: hc.Message, Pairs: d, ParentBaseAncestor: TriYes,
		}), nil, StreamSync, edited(hc.SHA, true), "is a merge of 2 parents"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := fmt.Sprintf("%#v", c.h)
			fingerprints := c.fps
			if fingerprints == nil {
				fingerprints = fps
			}
			got := ClassifyBranch(c.h, fingerprints, c.stream)
			if after := fmt.Sprintf("%#v", c.h); after != before {
				t.Fatalf("ClassifyBranch modified its input")
			}
			detail := got.Detail
			got.Detail = ""
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ClassifyBranch =\n%+v\nwant\n%+v", got, c.want)
			}
			switch {
			case c.detail == "" && detail != "":
				t.Errorf("Detail = %q, want none", detail)
			case !strings.Contains(detail, c.detail):
				t.Errorf("Detail = %q, want it to contain %q", detail, c.detail)
			}
		})
	}
}

// btModel is a small repository for TestClassifyBranchModel: the default
// branch, the commits people and touchmark make, and a sync branch.
type btModel struct {
	r       *rand.Rand
	next    int
	commits map[string]*btCommit
	main    []string // the default branch, oldest first; B is the last
	tip     string   // the sync branch head
	pairs   []Pair   // what touchmark's commits bring
	// unknown makes the caller's proofs fail now and then (TriUnknown).
	unknown bool
}

// btCommit is one commit of the model.
type btCommit struct {
	sha     string
	parents []string
	ours    bool   // carries touchmark's trailers for pairs
	pairs   []Pair // what it brings relative to its first parent
	// foreign marks content only people wrote: their commits, an amended
	// commit of ours, a merge that is not clean.
	foreign bool
	clean   bool // a merge whose paths all equal one parent's version
}

func (m *btModel) add(c btCommit) string {
	m.next++
	c.sha = btSHA(0x1000 + m.next)
	m.commits[c.sha] = &c
	return c.sha
}

func (m *btModel) base() string { return m.main[len(m.main)-1] }

// onMain reports whether sha is on the default branch.
func (m *btModel) onMain(sha string) bool { return slices.Contains(m.main, sha) }

// someMain returns a commit of the default branch, the newest more often.
func (m *btModel) someMain() string {
	if m.r.IntN(2) == 0 {
		return m.base()
	}
	return m.main[m.r.IntN(len(m.main))]
}

// ours adds touchmark's commit on parent.
func (m *btModel) ours(parent string) string {
	return m.add(btCommit{parents: []string{parent}, ours: true, pairs: m.pairs})
}

// hc returns the newest commit of ours on the first-parent chain from tip.
func (m *btModel) hc() *btCommit {
	for sha := m.tip; sha != ""; {
		c := m.commits[sha]
		if c == nil {
			return nil
		}
		if c.ours {
			return c
		}
		if len(c.parents) == 0 {
			return nil
		}
		sha = c.parents[0]
	}
	return nil
}

// step applies one random event to the model.
func (m *btModel) step() {
	switch m.r.IntN(9) {
	case 0: // the default branch moves on
		m.main = append(m.main, m.add(btCommit{parents: []string{m.base()}}))
	case 1: // a person commits on the sync branch
		m.tip = m.add(btCommit{parents: []string{m.tip}, foreign: true})
	case 2: // Update branch: a clean merge of the default branch
		m.tip = m.add(btCommit{parents: []string{m.tip, m.someMain()}, clean: true})
	case 3: // a merge of someone's feature branch
		f := m.add(btCommit{parents: []string{m.someMain()}, foreign: true})
		m.tip = m.add(btCommit{parents: []string{m.tip, f}, clean: m.r.IntN(2) == 0})
	case 4: // a merge of the default branch with changes of its own
		m.tip = m.add(btCommit{parents: []string{m.tip, m.someMain()}, clean: false})
	case 5: // someone's commit with touchmark's cherry-picked onto it
		f := m.add(btCommit{parents: []string{m.someMain()}, foreign: true})
		m.tip = m.ours(f)
	case 6: // the rebase button: touchmark's commit replayed onto B
		m.tip = m.ours(m.base())
	case 7: // touchmark's commit amended: its trailer names other content
		if hc := m.hc(); hc != nil && hc.sha == m.tip {
			amended := *hc
			amended.pairs = slices.Clone(m.pairs)
			amended.pairs[0].To = btSHA(0xdead)
			amended.foreign = true
			m.tip = m.add(amended)
		}
	case 8: // touchmark rebuilds the branch on B
		m.tip = m.ours(m.base())
	}
}

// safe reports whether rewriting the sync branch from B loses nothing:
// every commit reachable from its head that is not on the default branch
// is touchmark's own, unchanged, or a clean merge.
func (m *btModel) safe() bool {
	seen := map[string]bool{}
	var walk func(sha string) bool
	walk = func(sha string) bool {
		if seen[sha] || m.onMain(sha) || sha == "" {
			return true
		}
		seen[sha] = true
		c := m.commits[sha]
		if c.foreign || (len(c.parents) > 1 && !c.clean) {
			return false
		}
		// The default branch is linear: what is not on it is walked.
		for _, p := range c.parents {
			if !walk(p) {
				return false
			}
		}
		return true
	}
	return walk(m.tip)
}

// tri answers a proof the caller makes, failing it now and then.
func (m *btModel) tri(ok bool) Tri {
	if m.unknown && m.r.IntN(4) == 0 {
		return TriUnknown
	}
	if ok {
		return TriYes
	}
	return TriNo
}

// history is what the caller would read about the sync branch.
func (m *btModel) history() BranchHistory {
	h := BranchHistory{Name: ownBranch, Exists: true, Head: m.tip, Base: m.base()}
	for sha := m.tip; sha != "" && len(h.Commits) < MaxHistory; {
		c := m.commits[sha]
		hc := HistoryCommit{SHA: c.sha, Parents: slices.Clone(c.parents), Message: "fix: a person's change\n"}
		if c.ours {
			hc.Message = btMessage(btTrailers(ownFP, StreamSync, m.pairs))
			hc.Pairs = slices.Clone(c.pairs)
			if len(c.parents) > 0 {
				hc.ParentBaseAncestor = m.tri(m.onMain(c.parents[0]))
			}
		}
		if len(c.parents) > 1 {
			hc.CleanMerge = c.clean
			hc.BaseAncestor = m.tri(m.onMain(c.parents[1]))
		}
		h.Commits = append(h.Commits, hc)
		if c.ours || len(c.parents) == 0 {
			break
		}
		sha = c.parents[0]
	}
	return h
}

// TestClassifyBranchModel builds sync branches from random events (people's
// commits, Update branch, merges of other branches, cherry-picks and
// reorders onto someone's commit, the rebase button, amends, rebuilds) and
// checks invariant I2 (see package distribute): a branch ClassifyBranch
// calls rewritable loses nothing when rewritten from B. With every proof the
// caller makes answered, the reverse holds too: a branch that loses nothing
// is rewritable.
func TestClassifyBranchModel(t *testing.T) {
	runs := 5000
	if testing.Short() {
		runs = 1000
	}
	fps := []string{ownFP}
	seen := map[string]int{}
	for seed := range uint64(runs) {
		r := rand.New(rand.NewPCG(seed, 0xb7))
		m := &btModel{r: r, commits: map[string]*btCommit{}, pairs: mixedPairs()[:1+r.IntN(4)], unknown: r.IntN(3) == 0}
		m.main = []string{m.add(btCommit{})}
		for range r.IntN(3) {
			m.main = append(m.main, m.add(btCommit{parents: []string{m.base()}}))
		}
		m.tip = m.ours(m.someMain())
		for range r.IntN(5) {
			m.step()
		}
		h := m.history()
		got := ClassifyBranch(h, fps, StreamSync)
		safe := m.safe()
		if got.State == BranchRewritable && !safe {
			t.Fatalf("seed %d: rewritable, but a rewrite from B loses someone's commits\nhistory: %+v", seed, h)
		}
		if !m.unknown && safe && got.State != BranchRewritable {
			t.Fatalf("seed %d: loses nothing, yet %s (%s)\nhistory: %+v", seed, got.State, got.Detail, h)
		}
		key := got.State.String()
		if !safe {
			key += " unsafe"
		}
		seen[key]++
		if got.State == BranchEdited && strings.Contains(got.Detail, "sits on commits") {
			seen["below"]++
		}
	}
	for _, want := range []string{"rewritable", "edited unsafe", "edited", "below"} {
		if seen[want] == 0 {
			t.Errorf("no %q among %d branches: %v", want, runs, seen)
		}
	}
}

func TestFindHc(t *testing.T) {
	d := mixedPairs()
	fps := []string{ownFP, ownPrevFP}
	ours := btOurs(btSHA(10), []string{btSHA(1)}, d, ownFP, StreamSync)
	prev := btOurs(btSHA(11), []string{btSHA(10)}, d[:1], ownPrevFP, StreamSync)
	adopt := btOurs(btSHA(12), []string{btSHA(11)}, d, ownFP, StreamAdopt)
	other := btOurs(btSHA(13), []string{btSHA(12)}, d, ownOther, StreamSync)
	plain := btPlain(btSHA(14), btSHA(13))
	deep := make([]HistoryCommit, MaxHistory)
	for i := range deep {
		deep[i] = btPlain(btSHA(200+i), btSHA(201+i))
	}
	cases := []struct {
		name    string
		commits []HistoryCommit
		stream  string
		want    int
		tr      Trailers
	}{
		{"none", nil, StreamSync, -1, Trailers{}},
		{"at the head", []HistoryCommit{ours}, StreamSync, 0, btTrailers(ownFP, StreamSync, d)},
		{"the newest of ours", []HistoryCommit{plain, prev, ours}, StreamSync, 1, btTrailers(ownPrevFP, StreamSync, d[:1])},
		{"past other hubs and streams", []HistoryCommit{other, adopt, ours}, StreamSync, 2, btTrailers(ownFP, StreamSync, d)},
		{"the adopt stream", []HistoryCommit{other, adopt, ours}, StreamAdopt, 1, btTrailers(ownFP, StreamAdopt, d)},
		{"the last commit within reach", append(slices.Clone(deep[:MaxHistory-1]), ours), StreamSync, MaxHistory - 1, btTrailers(ownFP, StreamSync, d)},
		{"out of reach", append(slices.Clone(deep), ours), StreamSync, -1, Trailers{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, tr := FindHc(c.commits, fps, c.stream)
			if got != c.want || tr != c.tr {
				t.Fatalf("FindHc = %d, %+v; want %d, %+v", got, tr, c.want, c.tr)
			}
		})
	}
}

func TestBranchStateString(t *testing.T) {
	for s, want := range map[BranchState]string{BranchAbsent: "absent", BranchRewritable: "rewritable", BranchForeign: "foreign", BranchEdited: "edited", 9: "unknown"} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", s, got, want)
		}
	}
}

func TestLegacyRewritable(t *testing.T) {
	m := manifestOf(
		hist{"AGENTS.md", "agents", oidA, big},
		hist{"AGENTS.md", "agents", oidB, tiny}, // any size counts
		hist{"prompts/old.md", "gone", oidE, big},
		hist{"scripts/check.sh", "python", oidD, big},
	)
	write := func(path, to string) Pair { return Pair{Path: path, From: oidC, Mode: "100644", To: to} }
	cases := []struct {
		name string
		diff []Pair
		m    *provenance.Manifest
		want bool
	}{
		{"empty diff", nil, m, true},
		{"empty diff, no manifest", nil, nil, true},
		{"writes a shipped version", []Pair{write("AGENTS.md", oidA)}, m, true},
		{"writes a small shipped version", []Pair{write("AGENTS.md", oidB)}, m, true},
		{"writes the version of a pack no longer selected", []Pair{{Path: "prompts/old.md", From: ZeroOID, Mode: "100644", To: oidE}}, m, true},
		{"deletes a shipped version", []Pair{{Path: "prompts/old.md", From: oidE, Mode: ModeDelete, To: ZeroOID}}, m, true},
		{"makes a shipped file executable", []Pair{{Path: "scripts/check.sh", From: oidD, Mode: "100755", To: oidD}}, m, true},
		{"all of several", []Pair{write("AGENTS.md", oidA), {Path: "prompts/old.md", From: oidE, Mode: ModeDelete, To: ZeroOID}}, m, true},

		{"writes content of its own", []Pair{write("AGENTS.md", oidC)}, m, false},
		{"one of several is its own", []Pair{write("AGENTS.md", oidA), write("README.md", oidC)}, m, false},
		{"a shipped blob at another path", []Pair{write("README.md", oidA)}, m, false},
		{"another spelling", []Pair{write("agents.md", oidA)}, m, false},
		{"deletes content of its own", []Pair{{Path: "AGENTS.md", From: oidC, Mode: ModeDelete, To: ZeroOID}}, m, false},
		{"a symlink to a shipped blob", []Pair{{Path: "AGENTS.md", From: ZeroOID, Mode: "120000", To: oidA}}, m, false},
		{"a submodule", []Pair{{Path: "AGENTS.md", From: ZeroOID, Mode: "160000", To: oidA}}, m, false},
		{"a malformed deletion", []Pair{{Path: "prompts/old.md", From: oidE, Mode: ModeDelete, To: oidE}}, m, false},
		{"a write of nothing", []Pair{write("AGENTS.md", ZeroOID)}, m, false},
		{"no manifest", []Pair{write("AGENTS.md", oidA)}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LegacyRewritable(c.diff, c.m); got != c.want {
				t.Fatalf("LegacyRewritable = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTouchesWorkflows(t *testing.T) {
	pair := func(path string) Pair { return Pair{Path: path, From: ZeroOID, Mode: "100644", To: oidA} }
	cases := []struct {
		name  string
		pairs []Pair
		want  bool
	}{
		{"none", nil, false},
		{"a workflow", []Pair{pair("AGENTS.md"), pair(".github/workflows/ci.yml")}, true},
		{"a nested file", []Pair{pair(".github/workflows/sub/x.yml")}, true},
		{"a deletion", []Pair{{Path: ".github/workflows/old.yml", From: oidB, Mode: ModeDelete, To: ZeroOID}}, true},
		{"a mode change", []Pair{{Path: ".github/workflows/run.sh", From: oidB, Mode: "100755", To: oidB}}, true},
		{"the directory itself", []Pair{pair(".github/workflows")}, true},
		{"another case", []Pair{pair(".GitHub/Workflows/ci.yml")}, true},
		{"a sibling directory", []Pair{pair(".github/workflows-old/ci.yml")}, false},
		{"actions", []Pair{pair(".github/actions/setup/action.yml")}, false},
		{"not at the root", []Pair{pair("docs/.github/workflows/ci.yml")}, false},
		{"gitea workflows", []Pair{pair(".gitea/workflows/ci.yml")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TouchesWorkflows(c.pairs); got != c.want {
				t.Fatalf("TouchesWorkflows = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPairsEqual(t *testing.T) {
	d := mixedPairs()
	reversed := slices.Clone(d)
	slices.Reverse(reversed)
	changed := slices.Clone(d)
	changed[1].To = oidC
	cases := []struct {
		name string
		a, b []Pair
		want bool
	}{
		{"both empty", nil, []Pair{}, true},
		{"same order", d, slices.Clone(d), true},
		{"any order", d, reversed, true},
		{"one missing", d, d[:3], false},
		{"one differs", d, changed, false},
		{"zero id is not empty", []Pair{{Path: "a", From: ZeroOID, Mode: "100644", To: oidA}}, []Pair{{Path: "a", Mode: "100644", To: oidA}}, false},
		{"a pair twice", []Pair{d[0], d[0], d[1]}, []Pair{d[0], d[1], d[1]}, false},
		{"twice in both", []Pair{d[0], d[1], d[0]}, []Pair{d[0], d[0], d[1]}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := slices.Clone(c.a), slices.Clone(c.b)
			if got := PairsEqual(c.a, c.b); got != c.want {
				t.Fatalf("PairsEqual = %v, want %v", got, c.want)
			}
			if got := PairsEqual(c.b, c.a); got != c.want {
				t.Fatalf("PairsEqual reversed = %v, want %v", got, c.want)
			}
			if !slices.Equal(a, c.a) || !slices.Equal(b, c.b) {
				t.Fatal("PairsEqual modified its input")
			}
		})
	}
}
