package prbody

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// draftRe matches the title prefixes that make a change request a draft:
// "Draft:", "[Draft]" and "(Draft)" on GitLab (a merge request takes its
// default title from the commit message, and a pushed commit whose message
// starts so turns the merge request into a draft), "WIP:" and "[WIP]" as
// Gitea's default WORK_IN_PROGRESS_PREFIXES, in any case, after leading
// blanks. It is the pattern `touchmark check` applies to commit.message
// (config's draftPattern), blanks spelled as ECMA-262 \s like there, so the
// check and the commit agree.
var draftRe = regexp.MustCompile(`^[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*(?i:draft:|\[draft\]|\(draft\)|wip:|\[wip\])`)

// trailerRe is one trailer line: a token, a colon and the value.
var trailerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*:(?: .*)?$`)

// Scissors is git's scissors line: git drops a commit message from it on
// (`git commit -v` cleanup, and the trailer parsing of git and of
// decide.ParseTrailers), so a message holding it would hide touchmark's
// trailers and every sync branch would look foreign.
const Scissors = "# ------------------------ >8 ------------------------"

// CommitMessage returns the commit message: message (commit.message, which
// must not start with "Draft:" or "WIP:" in any case), a blank line and the
// trailer block.
//
// Besides "Draft:" and "WIP:", the prefixes "[Draft]", "(Draft)" and
// "[WIP]" are refused too (see draftRe): GitLab and Gitea treat them the
// same, and `touchmark check` refuses the same set. A line equal to
// Scissors is refused as well, as `touchmark check` does.
//
// The message is cleaned the way git cleans a -m message: line breaks as
// "\n", trailing blanks removed from every line, runs of blank lines folded
// into one, leading and trailing blank lines dropped. It must not be blank
// and must be valid UTF-8 without control characters other than tabs.
// trailers (decide.FormatTrailers) must be one paragraph of "Key: value"
// lines, so that it stays the last paragraph git and ParseTrailers read.
// The result ends with a newline, as git writes messages; the same input
// always gives the same bytes, and so the same commit id.
func CommitMessage(message, trailers string) (string, error) {
	msg := cleanMessage(message)
	switch {
	case !utf8.ValidString(msg):
		return "", errors.New("commit message: not valid UTF-8")
	case msg == "":
		return "", errors.New("commit message: blank")
	case draftRe.MatchString(msg):
		return "", errors.New("commit message: starts with a draft prefix (Draft:, [Draft], (Draft), WIP: or [WIP]), which turns a GitLab merge request into a draft")
	case slices.Contains(strings.Split(msg, "\n"), Scissors):
		return "", errors.New("commit message: holds git's scissors line, below which git drops the message and touchmark's trailers with it")
	}
	if j := strings.IndexFunc(msg, func(r rune) bool { return unicode.IsControl(r) && r != '\t' && r != '\n' }); j >= 0 {
		r, _ := utf8.DecodeRuneInString(msg[j:])
		return "", fmt.Errorf("commit message: control character %U", r)
	}
	t := strings.TrimSpace(strings.ReplaceAll(trailers, "\r\n", "\n"))
	if t == "" {
		return "", errors.New("commit message: no trailers")
	}
	for line := range strings.SplitSeq(t, "\n") {
		if !trailerRe.MatchString(line) || strings.IndexFunc(line, unicode.IsControl) >= 0 || !utf8.ValidString(line) {
			return "", fmt.Errorf("commit message: %.80q is not a trailer line", line)
		}
	}
	return msg + "\n\n" + t + "\n", nil
}

// cleanMessage applies git's whitespace cleanup to a message.
func cleanMessage(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	gap := false
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t\v\f")
		if line == "" {
			gap = len(out) > 0
			continue
		}
		if gap {
			out = append(out, "")
			gap = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Close reasons ClosedComment explains (decide.Reason*).
const (
	reasonNoDiff        = "no-diff"
	reasonOptedOut      = "opted-out"
	reasonTargetDropped = "target-dropped"
	reasonDuplicate     = "duplicate"
)

// Why a repository is no longer opted in, for an opted-out close
// (ClosedComment's cause).
const (
	// CauseDisabled: its opt-in file says enabled: false.
	CauseDisabled = "disabled"
	// CauseNoOptIn: it has no opt-in file, and targets.yml does not
	// subscribe it (the file was deleted, or the hub withdrew opt_in:
	// assumed).
	CauseNoOptIn = "no-opt-in"
)

// ClosedComment is the comment after touchmark closes its own PR, for reason
// (decide.Reason* close reasons). An opted-out close says why the
// repository is no longer opted in by its cause (CauseDisabled,
// CauseNoOptIn; any other value covers both), naming the opt-in file
// optInFile ("" for "the opt-in file").
//
// It says why and what comes next, without naming the request "pull" or
// "merge" (it has no platform) and without a mention or a line that starts
// with "/". An unknown reason is shown in a code span.
func ClosedComment(reason, cause, optInFile string) string {
	optIn := "the opt-in file"
	if optInFile != "" {
		optIn = "the opt-in file " + Code(optInFile)
	}
	var why, next string
	switch reason {
	case reasonNoDiff:
		// D is empty also when the team ignored the paths or changed them
		// here, and then no new one comes: the text promises nothing.
		why = "there is nothing left to sync (the default branch has the hub's versions of these files, or they are ignored or changed here)"
		next = "This is not a decline: nothing is remembered."
	case reasonOptedOut:
		switch cause {
		case CauseDisabled:
			why = "this repository opted out, as " + optIn + " says `enabled: false`"
			next = "To get engineering assets again, remove `enabled: false` from it."
		case CauseNoOptIn:
			// The repository may never have had the file: the hub may
			// have withdrawn its subscription.
			why = "this repository has no opt-in file, and the hub does not subscribe it (the file was deleted, or the hub stopped subscribing it)"
			next = "To get engineering assets again, add " + optIn + "."
		default:
			why = "this repository is no longer opted in (it has no opt-in file and the hub does not subscribe it, or its opt-in file says `enabled: false`)"
			next = "To get engineering assets again, add " + optIn + ", or remove `enabled: false` from it."
		}
	case reasonTargetDropped:
		why = "the hub no longer syncs this repository"
		next = "This is not a decline: if the hub adds this repository back, a new one comes."
	case reasonDuplicate:
		why = "another one from the same hub is open on its current branch and carries these changes from now on"
	default:
		why = "reason " + Code(reason)
	}
	s := "touchmark closed this: " + why + "."
	if next != "" {
		s += "\n\n" + next
	}
	return s
}

// DeclinedComment is the one comment after touchmark first sees a decline:
// what is remembered, when a new PR comes, that editing packs or ignore (or
// the file) lifts it, how to undo it, and ready YAML for ignore listing the
// paths.
//
// The YAML is a fenced block of double-quoted entries (escaped, so any path
// is one valid scalar on its own line) of patterns that match exactly
// their path under the ignore rules: "*", "?" and "[" become one-character
// classes, and blanks at either end become "[ ]"-style classes, which the
// pattern normalization would otherwise trim. Paths are sorted and listed
// once, at most 100 of them and about 32 KiB, and then counted in a YAML
// comment, so the comment stays well inside every platform's limit; a path
// that gives no pattern (empty, or only slashes) is left out, so the block
// is always a valid ignore list. A pr of zero or less leaves out the hint
// about forget_declines.
func DeclinedComment(pr int64, paths []string, optInFile string) string {
	optIn := "the opt-in file"
	if optInFile != "" {
		optIn = Code(optInFile)
	}
	var b strings.Builder
	b.WriteString("touchmark noted that this was closed without merging: it remembers these changes and will not propose them again.\n\n")
	b.WriteString("- A new one comes when the hub changes these files.\n")
	b.WriteString("- Editing `packs` or `ignore` in " + optIn + ", or changing one of these files here, lifts this decline.\n")
	b.WriteString("- To have the same changes proposed again, reopen this or tick **Propose this content again** in its description.")
	if pr > 0 {
		b.WriteString(" The hub's maintainers can also add a `forget_declines` entry with `pr: " + strconv.FormatInt(pr, 10) + "` to `.touchmark/operations.yml`.")
	}
	var ps []string
	for _, p := range sortedUnique(paths) {
		if pathx.NormalizePattern(ignorePattern(p)) != "" {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return b.String()
	}
	var y strings.Builder
	y.WriteString("ignore:\n")
	listed, size := 0, 0
	for _, p := range ps {
		entry := "  - " + yamlQuote(ignorePattern(p)) + "\n"
		if listed == maxRows || (listed > 0 && size+len(entry) > maxYAML) {
			break
		}
		y.WriteString(entry)
		listed, size = listed+1, size+len(entry)
	}
	if more := len(ps) - listed; more > 0 {
		y.WriteString("  # …and " + strconv.Itoa(more) + " more\n")
	}
	// A path may hold backticks: the fence is longer than any run of them
	// inside, so a path can never close the block early.
	fence := strings.Repeat("`", max(3, longestRun(y.String(), '`')+1))
	b.WriteString("\n\nTo keep your own versions of these files for good, add them to `ignore` in " + optIn + ":\n\n" + fence + "yaml\n")
	b.WriteString(y.String())
	b.WriteString(fence)
	return b.String()
}

// AutoDeclinedComment is DeclinedComment for an auto-close touchmark now
// counts as a decline: a bot closed the same content the third time in a row
// (decide.CommentAutoDeclined). A first paragraph says so and advises
// exempting touchmark's pull requests from the bot through labels, the hub's
// pr.labels (each in a code span; none gives a general advice).
func AutoDeclinedComment(pr int64, paths []string, optInFile string, labels []string) string {
	var b strings.Builder
	b.WriteString("A bot closed this without merging for the third time in a row, so touchmark now remembers it as declined, like a person's decision. ")
	if ls := uniqueInOrder(labels); len(ls) > 0 {
		b.WriteString("To keep a stale bot from closing touchmark's changes, exempt the " + plural(len(ls), "label ", "labels ") + joinAnd(codes(ls)) + " in its settings.")
	} else {
		b.WriteString("To keep a stale bot from closing touchmark's changes, exempt them in its settings.")
	}
	return b.String() + "\n\n" + DeclinedComment(pr, paths, optInFile)
}

// maxYAML bounds a declined comment: it stays far below the smallest
// comment limit (GitHub's 65536 characters).
const maxYAML = 32 << 10

// ignorePattern returns an ignore entry that matches exactly path under
// pathx.Match: the glob characters "*", "?" and "[" become one-character
// classes, and so do blanks at either end, which pathx.NormalizePattern
// would trim. (A backslash cannot be expressed: normalization turns it into
// "/"; no valid path holds one.)
func ignorePattern(path string) string {
	start := len(path) - len(strings.TrimLeftFunc(path, unicode.IsSpace))
	end := len(strings.TrimRightFunc(path, unicode.IsSpace))
	if end < start {
		end = start
	}
	var b strings.Builder
	for i, r := range path {
		if r == '*' || r == '?' || r == '[' || i < start || i >= end {
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteByte(']')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// yamlQuote returns s as a YAML double-quoted scalar: '"' and '\' escaped,
// and every character YAML does not allow in a scalar, or that could
// reorder or hide text, as an escape. Invalid UTF-8 becomes U+FFFD. An "@"
// is written as the escape \x40, so that no line of a comment holds one
// that could mention someone, even inside the code block (CheckText reads
// lines, not blocks).
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '@':
			b.WriteString(`\x40`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsControl(r) || isBidi(r) || r == 0x2028 || r == 0x2029 || r == 0xfeff || r == 0xfffe || r == 0xffff:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
