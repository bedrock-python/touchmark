package prbody

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"go.yaml.in/yaml/v3"
)

const trailers = "Touchmark-Hub: acme-eng@github.com/712345678\n" +
	"Touchmark-Stream: sync\n" +
	"Touchmark-Content: sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58\n" +
	"Touchmark-Hub-Commit: " + hubCommit

func TestCommitMessage(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"chore: sync engineering assets", "chore: sync engineering assets\n\n" + trailers + "\n"},
		{"chore: sync\n\nLonger text.  \n\n\n\nMore.\n\n", "chore: sync\n\nLonger text.\n\nMore.\n\n" + trailers + "\n"},
		{"\n\n  chore: sync\r\n\r\nBody\r\n", "  chore: sync\n\nBody\n\n" + trailers + "\n"},
		{"chore: sync\twith a tab", "chore: sync\twith a tab\n\n" + trailers + "\n"},
		{"Drafted: fine", "Drafted: fine\n\n" + trailers + "\n"},
		{"fix: WIP: later in the line", "fix: WIP: later in the line\n\n" + trailers + "\n"},
	}
	for _, tc := range cases {
		got, err := CommitMessage(tc.msg, trailers)
		if err != nil || got != tc.want {
			t.Errorf("CommitMessage(%q) = %q, %v; want %q", tc.msg, got, err, tc.want)
		}
	}
	// Trailers are trimmed and their line breaks normalized.
	if got, err := CommitMessage("m", "\n"+strings.ReplaceAll(trailers, "\n", "\r\n")+"\r\n"); err != nil || got != "m\n\n"+trailers+"\n" {
		t.Errorf("CRLF trailers: %q, %v", got, err)
	}
	bad := []struct{ msg, trailers, why string }{
		{"Draft: sync", trailers, "draft"},
		{"draft:sync", trailers, "draft"},
		{"DRAFT: sync", trailers, "draft"},
		{"[Draft] sync", trailers, "draft"},
		{"(draft) sync", trailers, "draft"},
		{"WIP: sync", trailers, "draft"},
		{"wip:sync", trailers, "draft"},
		{"[WIP] sync", trailers, "draft"},
		{"  \n\n Draft: sync", trailers, "draft"},
		{string(rune(0xa0)) + "Draft: sync", trailers, "draft"},
		{"", trailers, "blank"},
		{" \n\t\n", trailers, "blank"},
		{"chore\x00", trailers, "control"},
		{"chore\x1b[1m", trailers, "control"},
		{string([]byte{'a', 0xff}), trailers, "UTF-8"},
		{"chore", "", "no trailers"},
		{"chore", "Touchmark-Hub: a\n\nTouchmark-Stream: sync", "not a trailer"},
		{"chore", "not a trailer", "not a trailer"},
		{"chore", "Key: value\x00", "not a trailer"},
		// Git drops a message from its scissors line on, trailers included.
		{"chore: sync\n\n" + Scissors + "\nsee the hub", trailers, "scissors"},
		{"chore: sync\n" + Scissors + "   \r\n", trailers, "scissors"},
		{Scissors + "\nchore", trailers, "scissors"},
	}
	for _, tc := range bad {
		_, err := CommitMessage(tc.msg, tc.trailers)
		if err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("CommitMessage(%q, %q) = %v, want an error about %s", tc.msg, tc.trailers, err, tc.why)
		}
	}
	// A scissors-like line that is not git's cuts nothing.
	for _, msg := range []string{"chore\n\n# ----- >8 -----", "chore\n\n#" + Scissors[1:] + "-", "chore\n\n  " + Scissors} {
		if _, err := CommitMessage(msg, trailers); err != nil {
			t.Errorf("CommitMessage(%q) = %v", msg, err)
		}
	}
}

// cmTrailers are valid trailers for the round-trip tests.
func cmTrailers(r *rand.Rand) decide.Trailers {
	ids := []string{"acme-eng", "a", "x1-y2"}
	fps := []string{"github.com/712345678", "gitlab.example.com:8443/1234"}
	return decide.Trailers{
		HubID:       ids[r.IntN(len(ids))],
		Fingerprint: fps[r.IntN(len(fps))],
		Stream:      []string{decide.StreamSync, decide.StreamAdopt}[r.IntN(2)],
		Content:     testKey,
		HubCommit:   hubCommit,
	}
}

// cmFragments are the lines random commit.message values are made of:
// comments, scissors lines (git's and near ones), conflicts blocks,
// trailer-like lines, dividers, blanks.
var cmFragments = []string{
	"chore: sync engineering assets", "Body text.", "", " ", "\t", "#", "# a comment", Scissors,
	"# ----- >8 -----", "Conflicts:", "\tAGENTS.md", "Signed-off-by: A <a@example.com>",
	"Touchmark-Hub: other@github.com/1", "Key: value", "---", "(cherry picked from commit 0123)",
	"  indented", "Draft: later in the text",
}

// TestCommitMessageTrailersRoundTrip: whatever message CommitMessage
// accepts, decide.ParseTrailers reads the trailers it appended back
// exactly (the trailers are how touchmark finds its commit on a sync
// branch), and git reads the same trailer block.
func TestCommitMessageTrailersRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 8))
	accepted := 0
	for i := range 3000 {
		lines := make([]string, 1+r.IntN(8))
		for j := range lines {
			lines[j] = cmFragments[r.IntN(len(cmFragments))]
		}
		sep := []string{"\n", "\r\n", "\r"}[r.IntN(3)]
		msg := strings.Join(lines, sep)
		tr := cmTrailers(r)
		got, err := CommitMessage(msg, decide.FormatTrailers(tr))
		if err != nil {
			continue
		}
		accepted++
		if back, ok := decide.ParseTrailers(got); !ok || back != tr {
			t.Fatalf("case %d: ParseTrailers(CommitMessage(%q)) = %+v, %v; want %+v\nmessage: %q", i, msg, back, ok, tr, got)
		}
	}
	if accepted < 1000 {
		t.Errorf("only %d of 3000 messages accepted: the corpus is too thin", accepted)
	}
}

// FuzzCommitMessage: any accepted message keeps the trailers readable.
func FuzzCommitMessage(f *testing.F) {
	for _, m := range []string{
		"chore: sync", "chore\n\n" + Scissors + "\nx", "chore\n#\n# x\n", "chore\n\nConflicts:\n\tx",
		"chore\r\n\r\nbody\r\n", "chore\n\nKey: value", "# only a comment",
	} {
		f.Add(m)
	}
	tr := decide.Trailers{HubID: "acme-eng", Fingerprint: hubFP, Stream: decide.StreamSync, Content: testKey, HubCommit: hubCommit}
	block := decide.FormatTrailers(tr)
	f.Fuzz(func(t *testing.T, msg string) {
		got, err := CommitMessage(msg, block)
		if err != nil {
			return
		}
		if back, ok := decide.ParseTrailers(got); !ok || back != tr {
			t.Fatalf("ParseTrailers(CommitMessage(%q)) = %+v, %v", msg, back, ok)
		}
	})
}

func TestAutoDeclinedComment(t *testing.T) {
	paths := []string{"AGENTS.md", "prompts/review.md"}
	got := AutoDeclinedComment(19, paths, optInName, []string{"engineering-assets", "sync", "engineering-assets"})
	golden(t, "comments/auto-declined.md", got)
	checkComment(t, got)
	if !strings.HasSuffix(got, DeclinedComment(19, paths, optInName)) {
		t.Error("the auto-declined comment does not end with the declined one")
	}
	if !strings.Contains(got, "labels `engineering-assets` and `sync`") {
		t.Errorf("no advice about the labels:\n%s", got)
	}
	none := AutoDeclinedComment(0, nil, "", nil)
	checkComment(t, none)
	if !strings.Contains(none, "exempt them in its settings") || strings.Contains(none, "label") {
		t.Errorf("without labels:\n%s", none)
	}
}

// TestCommitMessageMatchesCheck checks that CommitMessage refuses exactly
// the draft prefixes and scissors lines `touchmark check` refuses in
// commit.message.
func TestCommitMessageMatchesCheck(t *testing.T) {
	for _, msg := range []string{
		"Draft: x", "draft:x", "[Draft] x", "[draft]x", "(Draft) x", "WIP: x", "wip:x", "[WIP] x", " Draft: x",
		"\tWIP: x", string(rune(0x3000)) + "[wip] x", string(rune(0xfeff)) + "Draft: x",
		"Draft x", "(WIP) x", "WIP x", "chore: Draft: x", "Drafts: x", "[Drafty] x", "xDraft: x", "chore: sync",
		"chore\n\n" + Scissors + "\nx", "chore\r" + Scissors + " \t", Scissors, "chore\r\n" + Scissors + "\r\nx",
		"chore\n  " + Scissors, "chore\n" + Scissors + "-", "chore\n# ----- >8 -----",
	} {
		hub := "version: 1\nid: acme-eng\ncommit:\n  message: " + strconv.QuoteToASCII(msg) + "\n"
		_, _, checkErr := config.ParseHub([]byte(hub))
		checkRefuses := checkErr != nil && (strings.Contains(checkErr.Error(), "Draft") || strings.Contains(checkErr.Error(), "scissors"))
		if checkErr != nil && !checkRefuses {
			t.Fatalf("hub.yml with %q: %v", msg, checkErr)
		}
		_, err := CommitMessage(msg, trailers)
		if refuses := err != nil; refuses != checkRefuses {
			t.Errorf("%q: CommitMessage refuses: %v (%v), check refuses: %v", msg, refuses, err, checkRefuses)
		}
	}
}

func TestClosedComment(t *testing.T) {
	for _, reason := range []string{"no-diff", "opted-out", "target-dropped", "duplicate"} {
		got := ClosedComment(reason)
		golden(t, "comments/closed-"+reason+".md", got)
		checkSafe(t, got, 1)
		if strings.Contains(got, "reason `") {
			t.Errorf("%s: explained as an unknown reason", reason)
		}
	}
	got := ClosedComment("@x\n/close`")
	if want := "touchmark closed this: reason `` @x/close` ``."; got != want {
		t.Errorf("unknown reason: %q, want %q", got, want)
	}
	checkSafe(t, got, 1)
}

// TestDecideNames checks that the close reasons and actions prbody spells
// are decide's.
func TestDecideNames(t *testing.T) {
	for _, reason := range []string{decide.ReasonNoDiff, decide.ReasonOptedOut, decide.ReasonTargetDropped, decide.ReasonDuplicate} {
		if strings.Contains(ClosedComment(reason), "reason `") {
			t.Errorf("close reason %q has no text of its own", reason)
		}
	}
	actions := map[string]decide.Action{ActionCreate: decide.Create, ActionUpdate: decide.Update, ActionDelete: decide.Delete, ActionChmod: decide.Chmod}
	for ours, theirs := range actions {
		if ours != string(theirs) {
			t.Errorf("action %q is %q in decide", ours, theirs)
		}
	}
}

func TestDeclinedComment(t *testing.T) {
	paths := []string{"prompts/review.md", "AGENTS.md", "docs/[draft].md", "AGENTS.md", "@team/notes.md", `quote"back\slash.md`, " lead.md"}
	got := DeclinedComment(12, paths, optInName)
	golden(t, "comments/declined.md", got)
	checkComment(t, got)
	patterns := yamlIgnore(t, got)
	want := []string{" lead.md", "@team/notes.md", "AGENTS.md", "docs/[draft].md", "prompts/review.md", `quote"back\slash.md`}
	if len(patterns) != len(want) {
		t.Fatalf("patterns %q", patterns)
	}
	for i, p := range want {
		// A quote and a backslash make a path invalid: it is still quoted
		// safely, but no pattern can name it (see ignorePattern).
		if pathx.Validate(p) == nil && !pathx.NewMatcher([]string{patterns[i]}, true).Match(p) {
			t.Errorf("pattern %q does not match %q", patterns[i], p)
		}
	}
	// A bracket is a class: the plain path would not match itself.
	if pathx.Match("docs/[draft].md", "docs/[draft].md") || !pathx.Match(ignorePattern("docs/[draft].md"), "docs/[draft].md") {
		t.Error("ignorePattern does not escape the class")
	}
	// The opt-in parser takes the block as it is.
	optIn := "version: 1\n" + yamlBlock(t, got)
	o, _, err := config.ParseOptIn([]byte(optIn))
	if err != nil || len(o.Ignore) != len(want) {
		t.Errorf("ParseOptIn: %v, %d patterns", err, len(o.Ignore))
	}

	none := DeclinedComment(0, nil, "")
	golden(t, "comments/declined-no-paths.md", none)
	checkComment(t, none)
	if strings.Contains(none, "```") || strings.Contains(none, "forget_declines") || !strings.Contains(none, "the opt-in file") {
		t.Errorf("without paths, PR or file name:\n%s", none)
	}
}

func TestDeclinedCommentCap(t *testing.T) {
	var paths []string
	for i := range 150 {
		paths = append(paths, fmt.Sprintf("docs/page-%03d.md", i))
	}
	got := DeclinedComment(3, paths, optInName)
	if n := len(yamlIgnore(t, got)); n != maxRows {
		t.Errorf("%d patterns, want %d", n, maxRows)
	}
	if !strings.Contains(got, "  # …and 50 more\n") {
		t.Errorf("no count of the rest:\n%s", got)
	}
	long := strings.Repeat("x", 4000)
	paths = paths[:0]
	for i := range 50 {
		paths = append(paths, fmt.Sprintf("%s-%02d", long, i))
	}
	got = DeclinedComment(3, paths, optInName)
	if len(got) > maxYAML+2048 {
		t.Errorf("a comment of %d bytes", len(got))
	}
	if n := len(yamlIgnore(t, got)); n == 0 || n == 50 {
		t.Errorf("%d patterns of 4 KiB", n)
	}
	checkComment(t, got)
}

// checkComment checks a comment: no line starts with "/", and "@" appears
// only in code spans or the YAML block.
func checkComment(t *testing.T, s string) {
	t.Helper()
	fenced := false
	for i, line := range strings.Split(s, "\n") {
		if startsSlash(line) {
			t.Errorf("line %d starts with a slash: %q", i+1, line)
		}
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			if line != "ignore:" && !strings.HasPrefix(line, "  - \"") && !strings.HasPrefix(line, "  # ") {
				t.Errorf("line %d of the YAML block: %q", i+1, line)
			}
			continue
		}
		if rest := outsideCode(line); strings.ContainsRune(rest, '@') {
			t.Errorf("line %d has an @ outside code: %q", i+1, line)
		}
	}
	if fenced {
		t.Error("the YAML block is not closed")
	}
}

// yamlBlock returns the YAML inside the fenced block of a comment.
func yamlBlock(t *testing.T, s string) string {
	t.Helper()
	_, rest, ok := strings.Cut(s, "```yaml\n")
	if !ok {
		t.Fatalf("no YAML block in\n%s", s)
	}
	block, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatalf("an open YAML block in\n%s", s)
	}
	return block
}

// yamlIgnore parses the YAML block of a comment.
func yamlIgnore(t *testing.T, s string) []string {
	t.Helper()
	var doc struct {
		Ignore []string `yaml:"ignore"`
	}
	if err := yaml.Unmarshal([]byte(yamlBlock(t, s)), &doc); err != nil {
		t.Fatalf("the YAML block does not parse: %v\n%s", err, s)
	}
	return doc.Ignore
}
