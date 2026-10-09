package decide

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/provenance"
)

// Trailer keys of touchmark commits.
const (
	TrailerHub       = "Touchmark-Hub"        // "<hub id>@<fingerprint>"
	TrailerStream    = "Touchmark-Stream"     // "sync" or "adopt"
	TrailerContent   = "Touchmark-Content"    // Key of the pairs the commit brings
	TrailerHubCommit = "Touchmark-Hub-Commit" // hub commit that decided the content
)

// MaxHistory is how deep the first-parent chain of a sync branch is read
// when looking for touchmark's newest commit, Hc.
const MaxHistory = 20

// Trailers are the touchmark trailers of one commit.
type Trailers struct {
	HubID       string
	Fingerprint string
	Stream      string
	Content     string // "sha256:<64 hex>"
	HubCommit   string
}

// FormatTrailers returns the trailer block, one "Key: value" line per
// trailer in the order Hub, Stream, Content, Hub-Commit, without a trailing
// newline. The hub line is "Touchmark-Hub: <id>@<fingerprint>".
//
// Values are written as given: the caller passes validated ones (a hub id,
// a fingerprint, a stream, a content key and a commit id never hold spaces
// or line breaks), so that ParseTrailers reads back exactly t.
func FormatTrailers(t Trailers) string {
	return TrailerHub + ": " + t.HubID + "@" + t.Fingerprint + "\n" +
		TrailerStream + ": " + t.Stream + "\n" +
		TrailerContent + ": " + t.Content + "\n" +
		TrailerHubCommit + ": " + t.HubCommit
}

// ParseTrailers reads the touchmark trailers of a commit message: the
// "Key: value" lines of its last paragraph (git interpret-trailers rules;
// other trailers are ignored). ok is false when the Touchmark-Hub trailer is
// absent or malformed, or a touchmark key appears twice.
//
// The trailer block follows git (trailer.c) as it reads a commit's
// trailers (`git log --format=%(trailers:only,unfold)`, which is `git
// interpret-trailers --parse --no-divider` on the message without its
// leading blank lines):
//   - leading blank lines are skipped;
//   - lines starting with '#' are comments: they are never trailers, and
//     the message ends before a trailing run of them and of empty lines,
//     before old "Conflicts:" blocks and at a scissors line ("# ---…---
//     >8 ---…---" ended by a line feed, as git 2.47 reads it; Git for
//     Windows 2.33 also cuts at one ended by CRLF);
//   - the first paragraph is the title and never holds trailers, so a
//     message without a blank line has none;
//   - the block is the last paragraph when every line of it is a trailer
//     ("Token: value", the token of ASCII letters, digits and hyphens,
//     optionally followed by spaces before the colon) or a continuation
//     line (starting with a space or a tab, folded into the trailer above
//     it), or when at least a quarter of its lines are trailers and one
//     starts with "Signed-off-by: " or "(cherry picked from commit ";
//   - a "---" line does not end the message: commit messages are not
//     patches.
//
// Keys compare ignoring case, like git's %(trailers:key=…); tokens and
// values are trimmed of spaces, tabs and carriage returns, so CRLF messages
// read the same. The hub value must be "<id>@<fingerprint>": a hub id
// (lowercase letters and digits in words joined by single hyphens) and a
// fingerprint (host, optional port, numeric repository id). The other
// values are returned as written, even when empty or absent: the caller
// compares them. On !ok the zero Trailers is returned.
func ParseTrailers(message string) (t Trailers, ok bool) {
	var hub string
	values := [len(trailerKeys)]*string{&hub, &t.Stream, &t.Content, &t.HubCommit}
	var seen [len(trailerKeys)]bool
	for _, tr := range trailerBlock(message) {
		i := trailerIndex(tr.key)
		if i < 0 {
			continue
		}
		if seen[i] {
			return Trailers{}, false
		}
		seen[i] = true
		*values[i] = tr.value
	}
	if !seen[0] {
		return Trailers{}, false
	}
	id, fp, found := strings.Cut(hub, "@")
	if !found || !trailerHubIDRe.MatchString(id) || !trailerFingerprintRe.MatchString(fp) {
		return Trailers{}, false
	}
	t.HubID, t.Fingerprint = id, fp
	return t, true
}

// trailerKeys are the touchmark trailer keys, in the order of the fields
// ParseTrailers fills.
var trailerKeys = [...]string{TrailerHub, TrailerStream, TrailerContent, TrailerHubCommit}

var (
	// trailerHubIDRe is the hub id syntax (hub.yml id, marker hub).
	trailerHubIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// trailerFingerprintRe is the fingerprint syntax (hub.yml
	// previous_fingerprints, marker fp) in canonical form: host, optional port,
	// and a numeric id or a lowercase Bitbucket repository UUID without braces.
	trailerFingerprintRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?/([0-9]+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
)

// trailerGitPrefixes are the prefixes git itself writes; one of them lets
// a paragraph with other lines count as trailers (git_generated_prefixes).
var trailerGitPrefixes = [...]string{"Signed-off-by: ", "(cherry picked from commit "}

// trailerIndex returns the index of key in trailerKeys, ignoring case, or
// -1. Tokens are ASCII, so EqualFold compares them byte by byte.
func trailerIndex(key string) int {
	for i, k := range trailerKeys {
		if strings.EqualFold(key, k) {
			return i
		}
	}
	return -1
}

// trailerKV is one trailer of a block: its token and unfolded value.
type trailerKV struct{ key, value string }

// trailerBlock returns the trailers of message's trailer block (see
// ParseTrailers), in order, with continuation lines folded into their
// values. Lines of the block that are not trailers are skipped.
func trailerBlock(message string) []trailerKV {
	// Git starts a commit's message at its first line that is not blank
	// (pretty.c, skip_blank_lines).
	for message != "" {
		line, rest, _ := strings.Cut(message, "\n")
		if !trailerBlank(line) {
			break
		}
		message = rest
	}
	msg := message[:trailerMessageEnd(message)]
	lines := strings.Split(msg, "\n")
	if strings.HasSuffix(msg, "\n") {
		lines = lines[:len(lines)-1] // a final newline ends a line, it starts none
	}
	title := 0
	for title < len(lines) && (trailerComment(lines[title]) || !trailerBlank(lines[title])) {
		title++
	}
	start := trailerBlockStart(lines, title)
	if start < 0 {
		return nil
	}
	var out []trailerKV
	last := -1 // index in out of the trailer the previous line was, or -1
	for _, line := range lines[start:] {
		if last >= 0 && line != "" && trailerSpace(line[0]) {
			out[last].value += "\n" + line
			continue
		}
		last = -1
		if trailerComment(line) {
			continue
		}
		sep := trailerSeparator(line)
		if sep < 1 {
			continue
		}
		out = append(out, trailerKV{key: trailerTrim(line[:sep]), value: line[sep+1:]})
		last = len(out) - 1
	}
	for i := range out {
		out[i].value = trailerUnfold(out[i].value)
	}
	return out
}

// trailerScissors is the line below which `git commit -v` cuts a message.
const trailerScissors = "# ------------------------ >8 ------------------------\n"

// trailerMessageEnd returns the length of the part of message that may
// hold trailers (git's ignore_non_trailer): the message up to a scissors
// line, without its trailing run of empty and comment lines and of old
// "Conflicts:" blocks (a "Conflicts:" line and the tab-indented paths
// after it). A run that starts on the first line is not dropped, as in git.
func trailerMessageEnd(message string) int {
	cutoff := len(message)
	if strings.HasPrefix(message, trailerScissors) {
		cutoff = 0
	} else if i := strings.Index(message, "\n"+trailerScissors); i >= 0 {
		cutoff = i + 1
	}
	run, conflicts := 0, false // run is where the trailing run starts; 0 is none
	for bol := 0; bol < cutoff; {
		next := len(message)
		if i := strings.IndexByte(message[bol:], '\n'); i >= 0 {
			next = bol + i + 1
		}
		switch line := message[bol:]; {
		case line[0] == '#' || line[0] == '\n':
			if run == 0 {
				run = bol
			}
		case strings.HasPrefix(line, "Conflicts:\n"):
			conflicts = true
			if run == 0 {
				run = bol
			}
		case conflicts && line[0] == '\t':
		case run != 0:
			run, conflicts = 0, false
		}
		bol = next
	}
	if run != 0 {
		return run
	}
	return cutoff
}

// trailerBlockStart returns the index of the first line of the trailer
// block of lines, or -1 when there is none. Lines before title are the
// title paragraph. It is git's find_trailer_block_start, without
// configured trailer keys: trailing blank lines are skipped, comment lines
// neither count as trailers nor end the block.
func trailerBlockStart(lines []string, title int) int {
	onlySpaces := true
	recognized := false
	trailers, others, continuations := 0, 0, 0
	for l := len(lines) - 1; l >= title; l-- {
		line := lines[l]
		if trailerComment(line) {
			others += continuations
			continuations = 0
			continue
		}
		if trailerBlank(line) {
			if onlySpaces {
				continue
			}
			others += continuations
			if (recognized && trailers*3 >= others) || (trailers > 0 && others == 0) {
				return l + 1
			}
			return -1
		}
		onlySpaces = false
		if slices.ContainsFunc(trailerGitPrefixes[:], func(p string) bool { return strings.HasPrefix(line, p) }) {
			trailers++
			continuations = 0
			recognized = true
			continue
		}
		switch {
		case trailerSeparator(line) >= 1 && !trailerSpace(line[0]):
			trailers++
			continuations = 0
		case trailerSpace(line[0]):
			continuations++
		default:
			others += 1 + continuations
			continuations = 0
		}
	}
	return -1
}

// trailerSeparator returns the index of the ':' that ends a trailer token
// at the start of line, or -1. The token is ASCII letters, digits and
// hyphens, optionally followed by spaces or tabs (git's find_separator).
func trailerSeparator(line string) int {
	space := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ':':
			return i
		case !space && (c == '-' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'):
			continue
		case i > 0 && (c == ' ' || c == '\t'):
			space = true
			continue
		}
		return -1
	}
	return -1
}

// trailerUnfold joins the lines of a folded value with single spaces and
// trims it (git's unfold_value).
func trailerUnfold(v string) string {
	if !strings.Contains(v, "\n") {
		return trailerTrim(v)
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] != '\n' {
			b.WriteByte(v[i])
			continue
		}
		for i+1 < len(v) && trailerSpace(v[i+1]) {
			i++
		}
		b.WriteByte(' ')
	}
	return trailerTrim(b.String())
}

// trailerSpace reports whether c is white space as git's isspace sees it.
func trailerSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// trailerTrim trims git white space from both ends of s.
func trailerTrim(s string) string { return strings.Trim(s, " \t\n\r") }

// trailerBlank reports whether line holds white space only.
func trailerBlank(line string) bool { return trailerTrim(line) == "" }

// trailerComment reports whether line is a comment for git (core.commentChar
// is not read: touchmark never runs with the target's config).
func trailerComment(line string) bool { return strings.HasPrefix(line, "#") }

// Tri is a three-valued answer.
type Tri uint8

const (
	TriUnknown Tri = iota
	TriYes
	TriNo
)

// HistoryCommit is one commit of the first-parent chain of a sync branch,
// with what the caller learned about it through git.
type HistoryCommit struct {
	SHA     string
	Parents []string
	// Message is the full commit message (trailers are parsed from it).
	Message string
	// Pairs are the pairs the commit brings relative to its first parent,
	// From being the blob in that parent. The caller fills them only for
	// the newest commit whose Touchmark-Hub trailer is ours (Hc).
	Pairs []Pair
	// CleanMerge and BaseAncestor are filled for merge commits newer than
	// Hc: CleanMerge is set when every path of the result equals the
	// version of one of the parents (the "Update branch" button);
	// BaseAncestor tells whether the second parent is an ancestor of the
	// target's current default branch head (B).
	CleanMerge   bool
	BaseAncestor Tri
	// ParentBaseAncestor is filled for Hc: whether its first parent is an
	// ancestor of B. The proof is the one of BaseAncestor (IsAncestor after
	// DeepenSince back to the parent's date, or the platform's compare
	// API); a first parent equal to BranchHistory.Base needs none. Without
	// it the commits below Hc may be someone else's (their commit with Hc
	// cherry-picked on top, or a reorder by rebase -i), and a rewrite from
	// B would drop them.
	ParentBaseAncestor Tri
}

// BranchHistory is what the caller read about one sync branch.
type BranchHistory struct {
	Name   string
	Exists bool
	Head   string // H
	// Base is B, the default branch head the caller proved ancestry
	// against (ParentBaseAncestor, BaseAncestor). Hc's first parent equal
	// to Base is on the default branch without further proof; "" proves
	// nothing.
	Base string
	// Commits is the first-parent chain from Head, newest first, up to and
	// including the newest commit with one of our fingerprints in its
	// Touchmark-Hub trailer, or MaxHistory commits when there is none.
	Commits []HistoryCommit
	// Truncated is set when the chain ended at a shallow boundary before
	// reaching a commit with our trailer.
	Truncated bool
}

// BranchState is what touchmark may do with a sync branch.
type BranchState uint8

const (
	// BranchAbsent: the branch does not exist.
	BranchAbsent BranchState = iota
	// BranchRewritable: our commit Hc is found, it still brings exactly the
	// pairs its Touchmark-Content trailer names, it sits on the default
	// branch (its first parent is an ancestor of B), and every commit after
	// it is a clean merge of the base. Rewriting loses nothing.
	BranchRewritable
	// BranchForeign: no commit with our trailer within MaxHistory: the
	// branch is someone else's (another hub, or a person).
	BranchForeign
	// BranchEdited: our commit is there but someone changed the branch:
	// the pairs no longer match the trailer, Hc is a merge or is not proven
	// to sit on the default branch, a commit after Hc is not a clean merge
	// of the base, or the chain hit the shallow boundary.
	BranchEdited
)

// String returns the state name for messages and tests.
func (s BranchState) String() string {
	switch s {
	case BranchAbsent:
		return "absent"
	case BranchRewritable:
		return "rewritable"
	case BranchForeign:
		return "foreign"
	case BranchEdited:
		return "edited"
	}
	return "unknown"
}

// Branch is the classification of one sync branch.
type Branch struct {
	Name  string
	Head  string // H
	State BranchState
	// Hc is our newest commit on the first-parent chain; "" when none.
	Hc string
	// C are the pairs Hc brings; CKey its Touchmark-Content trailer.
	C    []Pair
	CKey string
	// E is the effective base of the branch: the first parent of Hc, or the
	// second parent of the newest clean base merge after Hc.
	E string
	// HcIsHead is set when Hc is the branch head (no commit after it).
	HcIsHead bool
	// HcParent is the first parent of Hc.
	HcParent string
	// Detail explains Foreign and Edited for people.
	Detail string
	// LegacyRewritable is set by the caller on a Foreign branch that the
	// one-off migration rule lets touchmark rewrite (a branch multi-gitter
	// pushed, see docs/guide/migrate.md): operations.yml adopt_unmarked is
	// active and LegacyRewritable holds for the branch's diff. DecideTarget
	// honours it only while TargetOps.AdoptUnmarked is set.
	LegacyRewritable bool
}

// ClassifyBranch decides whether touchmark may rewrite h, by content:
//  1. !h.Exists → BranchAbsent.
//  2. No commit in h.Commits whose Touchmark-Hub fingerprint is one of
//     fingerprints and whose stream equals stream → BranchForeign (also when
//     Truncated). The hub id before '@' plays no part.
//  3. Key(stream, Hc.Pairs) != Hc's Touchmark-Content → BranchEdited
//     ("our commit was changed"). A rebase by button keeps message and diff
//     and passes as long as the base did not touch our paths.
//  4. Hc is a merge, or Hc's first parent is neither h.Base nor proven an
//     ancestor of B (ParentBaseAncestor is not TriYes) → BranchEdited:
//     commits below Hc that are not on the default branch are someone
//     else's (a cherry-pick of Hc onto their commit, a reorder by rebase
//     -i), and a rewrite from B would drop them. The rebase button moves Hc
//     onto the base and passes.
//  5. Any commit newer than Hc that is not a merge, or a merge that is not
//     CleanMerge, or whose BaseAncestor is not TriYes → BranchEdited.
//     (TriUnknown fails closed: the caller could not prove it.)
//  6. Otherwise BranchRewritable, with E per the definition above.
//
// Details of the rules:
//   - Hc is found by FindHc: only the first MaxHistory commits are searched,
//     whatever the caller passed, for the newest whose trailers parse with a
//     matching fingerprint (compared exactly) and stream.
//   - Everything else fails closed as BranchEdited once Hc is found: a
//     chain marked Truncated (the contract says Truncated means Hc was not
//     reached, so the input contradicts itself), a chain whose first commit
//     is not Head or that skips a commit between Head and Hc (a commit's
//     first parent is not the next commit), Pairs listing a path twice (Key
//     would panic), and a "merge" with other than two parents (Update
//     branch makes two).
//   - A root Hc (no parent) has nothing below it and passes rule 4.
//   - Hc, C, CKey, HcIsHead and HcParent are set whenever Hc is found,
//     for an edited branch too (a paused PR describes C); E only for a
//     rewritable one. C is a sorted copy of Hc.Pairs. An absent branch has
//     no Head. LegacyRewritable is the caller's: it is always false here.
//
// ClassifyBranch does not modify h and never panics.
func ClassifyBranch(h BranchHistory, fingerprints []string, stream string) Branch {
	b := Branch{Name: h.Name, Head: h.Head}
	if !h.Exists {
		b.Head = ""
		return b
	}
	commits := h.Commits
	if len(commits) > MaxHistory {
		commits = commits[:MaxHistory]
	}
	at, tr := FindHc(commits, fingerprints, stream)
	if at < 0 {
		b.State = BranchForeign
		if h.Truncated {
			b.Detail = "the fetched history ends before any touchmark commit of this hub"
		} else {
			b.Detail = fmt.Sprintf("none of the last %d commits is a touchmark commit of this hub", len(commits))
		}
		return b
	}
	hc := commits[at]
	b.Hc, b.CKey, b.HcIsHead = hc.SHA, tr.Content, at == 0
	b.C = slices.Clone(hc.Pairs)
	slices.SortFunc(b.C, branchComparePairs)
	if len(hc.Parents) > 0 {
		b.HcParent = hc.Parents[0]
	}
	b.State = BranchEdited
	switch {
	case h.Truncated:
		b.Detail = "the fetched history of the branch is truncated"
	case h.Head != "" && commits[0].SHA != h.Head:
		b.Detail = "the history read does not start at the branch head " + Short(h.Head, 12)
	case !branchChained(commits[:at+1]):
		b.Detail = "the history read is not the branch's first-parent chain"
	case branchRepeatsPath(hc.Pairs):
		b.Detail = "touchmark's commit " + Short(hc.SHA, 12) + " lists a path twice"
	case Key(stream, hc.Pairs) != tr.Content:
		b.Detail = "touchmark's commit " + Short(hc.SHA, 12) + " was changed: it no longer brings the content its trailer names"
	default:
		b.Detail = branchBelowHc(hc, h.Base)
		if b.Detail == "" {
			b.Detail = branchAfterHc(commits[:at])
		}
	}
	if b.Detail != "" {
		return b
	}
	b.State, b.E = BranchRewritable, b.HcParent
	if at > 0 {
		b.E = commits[0].Parents[1] // the newest clean base merge is the head
	}
	return b
}

// FindHc returns the index in commits (a first-parent chain, newest first)
// of Hc, touchmark's newest commit for stream: the first of the first
// MaxHistory commits whose trailers parse with a fingerprint in
// fingerprints (compared exactly) and stream, with its trailers; -1 and
// zero Trailers when there is none. It is the rule ClassifyBranch applies:
// the caller uses it to know which commit's Pairs to compute, which merges
// after it to check, and which hub commit decided the branch (guard I8, see
// package distribute).
func FindHc(commits []HistoryCommit, fingerprints []string, stream string) (int, Trailers) {
	for i, c := range commits[:min(len(commits), MaxHistory)] {
		if t, ok := ParseTrailers(c.Message); ok && t.Stream == stream && slices.Contains(fingerprints, t.Fingerprint) {
			return i, t
		}
	}
	return -1, Trailers{}
}

// branchBelowHc checks what Hc sits on and returns why the branch is
// edited, or "" when Hc is a root commit or its one parent is base or
// proven an ancestor of B. touchmark never writes a merge, so a merge
// carrying our trailers is someone's too.
func branchBelowHc(hc HistoryCommit, base string) string {
	sha := Short(hc.SHA, 12)
	switch {
	case len(hc.Parents) > 1:
		return fmt.Sprintf("touchmark's commit %s is a merge of %d parents: touchmark never writes one", sha, len(hc.Parents))
	case len(hc.Parents) == 0, base != "" && hc.Parents[0] == base, hc.ParentBaseAncestor == TriYes:
		return ""
	case hc.ParentBaseAncestor == TriNo:
		return "touchmark's commit " + sha + " sits on commits that are not on the default branch"
	}
	return "touchmark's commit " + sha + " could not be proven to sit on the default branch"
}

// branchAfterHc checks the commits newer than Hc (newest first) and returns
// why the branch is edited, or "" when every one is a clean merge of the
// base. It reports the oldest offending commit: the first change after
// touchmark's.
func branchAfterHc(newer []HistoryCommit) string {
	for i := len(newer) - 1; i >= 0; i-- {
		c := newer[i]
		sha := Short(c.SHA, 12)
		switch {
		case len(c.Parents) < 2:
			return "commit " + sha + " was added after touchmark's commit"
		case len(c.Parents) > 2:
			return fmt.Sprintf("merge %s after touchmark's commit has %d parents", sha, len(c.Parents))
		case !c.CleanMerge:
			return "merge " + sha + " after touchmark's commit changes files beyond its parents' versions"
		case c.BaseAncestor == TriNo:
			return "merge " + sha + " after touchmark's commit brings commits that are not on the default branch"
		case c.BaseAncestor != TriYes:
			return "merge " + sha + " after touchmark's commit could not be proven to bring only the default branch"
		}
	}
	return ""
}

// branchChained reports whether the first parent of every commit of chain
// but the last is the commit that follows it.
func branchChained(chain []HistoryCommit) bool {
	for i := 0; i+1 < len(chain); i++ {
		if len(chain[i].Parents) == 0 || chain[i].Parents[0] != chain[i+1].SHA {
			return false
		}
	}
	return true
}

// branchRepeatsPath reports whether pairs hold a path twice.
func branchRepeatsPath(pairs []Pair) bool {
	seen := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		if seen[p.Path] {
			return true
		}
		seen[p.Path] = true
	}
	return false
}

// branchComparePairs orders pairs by path, then by from, mode and to, so
// that any two orderings of the same pairs sort alike.
func branchComparePairs(a, b Pair) int {
	return cmp.Or(
		strings.Compare(a.Path, b.Path),
		strings.Compare(a.From, b.From),
		strings.Compare(a.Mode, b.Mode),
		strings.Compare(a.To, b.To),
	)
}

// LegacyRewritable is the one-off migration rule (operations.yml
// adopt_unmarked, for a branch multi-gitter pushed without touchmark's
// trailers; see docs/guide/migrate.md): a branch without our trailer may be
// rewritten when every pair of diff (merge-base(B, H) → H) either writes a
// blob that the hub ever shipped at that path (any pack, any size) or
// deletes a blob the hub ever shipped there. An empty diff is rewritable.
//
// Paths compare exactly. A write must be a regular file (mode 100644 or
// 100755): a symlink or a submodule whose id happens to equal a shipped
// blob is not hub content. A deletion must have Mode ModeDelete and To
// ZeroOID. Any other pair, and any pair with a nil manifest, fails closed.
func LegacyRewritable(diff []Pair, m *provenance.Manifest) bool {
	for _, p := range diff {
		var oid string
		switch {
		case p.Mode == ModeDelete && p.To == ZeroOID:
			oid = p.From
		case p.Mode == modeFile || p.Mode == modeExec:
			oid = p.To
		default:
			return false
		}
		if !branchShipped(m, p.Path, oid) {
			return false
		}
	}
	return true
}

// branchShipped reports whether any pack of m ever shipped oid at path.
func branchShipped(m *provenance.Manifest, path, oid string) bool {
	if m == nil || oid == "" || oid == ZeroOID {
		return false
	}
	for _, versions := range m.Paths[path] {
		if slices.ContainsFunc(versions, func(v provenance.Version) bool { return v.OID == oid }) {
			return true
		}
	}
	return false
}

// WorkflowsDir is the directory GitHub guards with the Workflows permission.
const WorkflowsDir = ".github/workflows"

// TouchesWorkflows reports whether any pair writes or deletes a path under
// WorkflowsDir.
//
// The path is compared case-folded (pathx.Fold), and a path equal to
// WorkflowsDir counts: when in doubt the permission is asked for, since a
// push refused for want of it only blocks the target.
func TouchesWorkflows(pairs []Pair) bool {
	return slices.ContainsFunc(pairs, func(p Pair) bool { return pathx.Under(pathx.Fold(p.Path), WorkflowsDir) })
}

// PairsEqual reports whether a and b hold the same pairs, in any order.
//
// Pairs compare field by field, exactly (ZeroOID and "" differ): C read
// from git and D from Pairs are both written in the normalized form of
// Pairs. A pair listed twice must be listed twice in the other slice too.
func PairsEqual(a, b []Pair) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.SortFunc(x, branchComparePairs)
	slices.SortFunc(y, branchComparePairs)
	return slices.Equal(x, y)
}
