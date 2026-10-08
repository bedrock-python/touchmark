// Package prbody renders the descriptions and comments touchmark writes and
// reads the two controls people may tick in them.
//
// Everything that comes from targets or the hub is escaped: paths only in
// code spans, no mentions, and no line that GitLab would execute as a quick
// action. Bodies fit the platform's budget, and the sensitive-paths section
// is never cut.
//
// Render checks its own output before it returns it: a line that starts
// with "/" after leading blanks, or an "@" outside code that could mention
// someone, makes it fail with ErrUnsafe instead. The hub's intro text
// (pr.intro_file) is Markdown touchmark does not write, so it is held to
// stricter rules that need no Markdown parser (CheckIntro).
//
// Output is deterministic: the same Input renders the same bytes, so a body
// hash changes only when the desired text does.
package prbody

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Controls people may tick.
const (
	ControlRecreate  = "recreate"
	ControlRepropose = "repropose"
)

// Actions of a Change.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
	ActionChmod  = "chmod"
)

// Errors Render wraps, so callers can tell the hub's configuration from a
// body that cannot be written.
var (
	// ErrTooLarge: the body does not fit Caps.MaxBody even with every list
	// cut to nothing (the ⚠ section, the intro and the marker are never
	// cut).
	ErrTooLarge = errors.New("body does not fit the platform's budget")
	// ErrUnsafe: the text would run a GitLab quick action or mention
	// someone. For text touchmark generates it is a bug; for the intro it
	// comes with ErrIntro.
	ErrUnsafe = errors.New("unsafe text")
	// ErrIntro: pr.intro_file breaks one of the rules of CheckIntro.
	ErrIntro = errors.New("unusable pr.intro_file")
)

// Change is one row of the changes table.
type Change struct {
	Path   string
	Pack   string
	Action string // "create", "update", "delete", "chmod"
	Mode   string // mode written ("100755" makes it sensitive)
}

// BuiltinSensitive are the built-in sensitive path patterns (pathx.Match
// semantics; docs/guide/packs.md lists them for users); files with mode
// 100755 are sensitive too.
//
// Agent skills and commands are among them: a skill's allowed-tools lets
// the agent use those tools without asking, a hooks block in its
// frontmatter registers hooks, and Claude Code applies a project skill's
// allowed-tools even in a folder nobody trusted
// (code.claude.com/docs/en/skills). The Agent Skills directories of other
// agents (.agents/skills, .github/skills) carry the same format.
var BuiltinSensitive = []string{
	".github/workflows/**", ".github/actions/**", ".github/dependabot.yml",
	".gitlab-ci.yml", ".gitlab/**/*.yml", ".gitea/**", ".forgejo/**",
	".claude/settings*.json", ".claude/hooks/**", ".claude/agents/**",
	".claude/skills/**", ".claude/commands/**", ".agents/skills/**", ".github/skills/**", ".mcp.json",
	"**/CODEOWNERS", ".gitattributes", "renovate.json*", ".pre-commit-config.yaml",
	".devcontainer/**", ".vscode/tasks.json",
}

// modeExecutable is the git mode of an executable file.
const modeExecutable = "100755"

// Sensitive reports whether a change to path (written with mode) belongs in
// the ⚠ section: a builtin or extra pattern matches, or mode is 100755.
//
// Patterns match ignoring case (pathx.NewMatcher with foldCase), as ignore
// patterns do: a case variant of a workflow path is the same directory on
// Windows and macOS checkouts, so it is flagged too. Any action counts,
// deletions included: removing a workflow deserves the same review.
func Sensitive(path, mode string, extra []string) bool {
	return mode == modeExecutable || sensitiveMatcher(extra).Match(path)
}

// sensitiveMatcher compiles the builtin patterns and extra.
func sensitiveMatcher(extra []string) *pathx.Matcher {
	return pathx.NewMatcher(slices.Concat(BuiltinSensitive, extra), true)
}

// Input is everything a body needs.
type Input struct {
	// Intro is the content of pr.intro_file ("" for none). It must pass
	// CheckIntro.
	Intro string
	// HubName and HubURL describe the hub: HubName is the hub id (hub.yml
	// id), which is always shown; HubURL is the hub repository's web URL,
	// "" when link_hub hides it (private hub, public target): the body then
	// has no link and no hub repository name. ContentCommit is the hub
	// commit that produced the content; Packs the target's packs.
	HubName       string
	HubURL        string
	ContentCommit string
	Packs         []string
	// Changes are the table rows: D, or while paused the branch's content.
	Changes []Change
	// Pending are, while paused, what a rebuild would bring (D − C).
	Pending []Change
	// BranchUnknown is set, while paused, when touchmark cannot tell what
	// the branch holds: none of its last commits is touchmark's (its Hc is
	// gone), so it has no C. The paused block then says so and claims
	// nothing about the branch; Changes should be empty and Pending all of
	// D.
	BranchUnknown bool
	// Local are paths in state local (a collapsed list with the adopt and
	// ignore hints; a plain section where the description cannot carry
	// HTML).
	Local []string
	// Sensitive are extra sensitive patterns from hub.yml.
	Sensitive []string
	// OptInFile is the target's opt-in file name (hub.yml opt_in_file), used
	// in the hints about ignore; "" reads "the opt-in file".
	OptInFile string
	// Assumed is set when the target has no opt-in file and the hub
	// subscribes it (opt_in: assumed in targets.yml): a paragraph after the
	// hub line says how to choose packs or ignore files, and how to opt out.
	Assumed bool
	// Blocks.
	PreviouslyDeclined []int64
	Paused             bool
	UpdateBranchNeeded bool
	NothingMore        bool
	// GiteaWorkflows warns that a new .gitea/workflows or .forgejo/workflows
	// turns off the target's .github/workflows on Gitea and Forgejo.
	GiteaWorkflows bool
	// Controls to show, unticked. A platform whose descriptions cannot
	// carry them (Caps.BodyControls false: Bitbucket Cloud) shows neither:
	// the paused block asks for a recreate entry in .touchmark/operations.yml
	// instead.
	ShowRecreate  bool
	ShowRepropose bool
	// Caps of the platform: MaxBody and QuickActions matter here (the "/"
	// assertion runs whatever QuickActions says), and Flavor names things
	// ("merge request" and "!N" on GitLab).
	Caps platform.Caps
	// Marker is the encoded marker line, written last, in the frame of
	// Caps.Marker (marker.EncodeFrame).
	//
	// "" renders the human part alone: the body without the marker, fitted
	// into Caps.MaxBody less room for the longest marker marker.EncodeFrame
	// writes (marker.MaxLine) and the blank line before it. Since the marker
	// records the hash of the human part (marker.Data.Body), that is how a
	// body is built: human, then its hash into the marker, then human +
	// "\n\n" + marker, which fits the budget whatever the marker's length.
	Marker string
}

// markerRoom is what a body without its marker leaves for it: the blank
// line and the longest marker line.
const markerRoom = len("\n\n") + marker.MaxLine

// maxRows is the most rows each list shows: the changes table, what a
// rebuild would bring and the local files. The rest is counted ("…and N
// more").
const maxRows = 100

// Lists Render may shorten to fit the budget, in the order it shortens them
// (the table first, then the local files).
const (
	listChanges = iota
	listLocal
	listPending
	numLists
)

// Render returns the body (in order: intro and hub line; the paragraph of a
// target the hub subscribed without an opt-in file; ⚠ sensitive paths,
// never cut; the changes table ordered sensitive, deletes, updates, creates,
// at most 100 rows then "and N more"; local files; blocks; controls;
// footnote; marker last). It fits the body into Caps.MaxBody by shortening
// the table and the local list, never the ⚠ section or the marker, and fails
// if even that does not fit. It fails when any line (after leading spaces)
// starts with "/" (Caps.QuickActions or not: the assertion always runs), or
// when a mention could be created.
//
// Details:
//   - Rows are ordered sensitive first, then by action (delete, update,
//     chmod, create), then by path; the ⚠ section lists the sensitive paths
//     of Changes and, while Paused, of Pending ("after a rebuild").
//   - The list of what a rebuild would bring (Pending) is shown only while
//     Paused, capped like the table and shortened after the local list.
//   - A Caps.MaxBody of zero or less means no budget.
//   - The body ends with the marker, without a trailing newline. Without a
//     Marker it ends with the footnote and leaves room for one (see
//     Input.Marker).
//   - Links to a hub on github.com go through redirect.github.com, so the
//     hub gets no backlink from each target; the content commit is linked
//     only there (on GitLab a commit link would add a "mentioned in" note to
//     the hub commit per target).
//   - GitLab bodies say "merge request" and refer to "!N"; the others "pull
//     request" and "#N".
//   - Where descriptions cannot carry tick boxes (Caps.BodyControls false)
//     the body shows no control whatever ShowRecreate and ShowRepropose
//     say: the paused block names a recreate entry of
//     .touchmark/operations.yml instead of the control. There, and where a
//     closed pull request can never be edited (Caps.ClosedImmutable), the
//     footnote names the forget_declines entry that has declined changes
//     proposed again, since no control in a declined pull request can.
//     Where descriptions cannot carry tick boxes, the platform escapes HTML,
//     and the body carries none (the intro aside, which is the hub's): the
//     local files are a section under a heading instead of a details
//     element.
//
// Errors wrap ErrIntro (the intro breaks CheckIntro), ErrTooLarge or
// ErrUnsafe; an unknown Change.Action, a HubURL that is not a plain http(s)
// URL, or a Marker that is neither "" nor one marker line in a frame of
// marker.EncodeFrame are errors too.
func Render(in Input) (string, error) {
	r, err := newRenderer(in)
	if err != nil {
		return "", fmt.Errorf("render body: %w", err)
	}
	body, err := r.fit()
	if err != nil {
		return "", fmt.Errorf("render body: %w", err)
	}
	return body, nil
}

// row is one change as the body shows it.
type row struct {
	Change
	sensitive bool
	pending   bool // what a rebuild would bring
}

// actionRank orders the table within the sensitive and the other rows
// (deletes, updates, creates; a mode change is an update).
var actionRank = map[string]int{ActionDelete: 1, ActionUpdate: 2, ActionChmod: 3, ActionCreate: 4}

// renderer holds the parts of a body, rendered once: fixed text, and the
// three lists as cuttable sections whose shown rows the budget decides.
type renderer struct {
	in       Input
	noun     string // "pull request" or "merge request"
	sigil    string // "#" or "!"
	optIn    string // the opt-in file as the hints name it
	rows     []row
	pending  []row
	local    []string
	declined []int64

	// Fixed parts.
	intro, head, assumed, sensitive, declinedBlock, updateBlock, nothingBlock, controls, footnote string
	// lists are indexed by listChanges, listLocal and listPending; nil for
	// a section the body does not have.
	lists [numLists]*cuttable
}

func newRenderer(in Input) (*renderer, error) {
	if in.Marker != "" {
		if err := checkMarker(in.Marker); err != nil {
			return nil, err
		}
	}
	intro, err := prepareIntro(in.Intro)
	if err != nil {
		return nil, err
	}
	hub, err := parseHubURL(in.HubURL, in.ContentCommit)
	if err != nil {
		return nil, err
	}
	if !in.Caps.BodyControls() {
		in.ShowRecreate, in.ShowRepropose = false, false
	}
	r := &renderer{in: in, intro: intro, noun: "pull request", sigil: "#", optIn: "the opt-in file"}
	if in.Caps.Flavor == "gitlab" {
		r.noun, r.sigil = "merge request", "!"
	}
	if in.OptInFile != "" {
		r.optIn = Code(in.OptInFile)
	}
	match := sensitiveMatcher(in.Sensitive)
	if r.rows, err = rowsOf(in.Changes, match, false); err != nil {
		return nil, err
	}
	if in.Paused {
		if r.pending, err = rowsOf(in.Pending, match, true); err != nil {
			return nil, err
		}
	}
	r.local = sortedUnique(in.Local)
	r.declined = prNumbers(in.PreviouslyDeclined)

	r.head = r.hubLine(hub)
	r.assumed = r.assumedBlock()
	r.sensitive = r.sensitiveSection()
	r.declinedBlock = r.previouslyDeclined()
	r.updateBlock = r.updateBranch()
	r.nothingBlock = r.nothingMore()
	r.controls = r.controlLines()
	r.footnote = r.note()
	r.lists = [numLists]*cuttable{
		listChanges: r.changesSection(),
		listLocal:   r.localSection(),
		listPending: r.pausedBlock(),
	}
	return r, nil
}

// rowsOf validates and orders changes: sensitive first, then by action rank
// and path (pack and mode break ties, so the order is total).
func rowsOf(changes []Change, match *pathx.Matcher, pending bool) ([]row, error) {
	rows := make([]row, 0, len(changes))
	for _, c := range changes {
		if _, ok := actionRank[c.Action]; !ok {
			return nil, fmt.Errorf("change of %.80q: unknown action %.40q", c.Path, c.Action)
		}
		rows = append(rows, row{Change: c, sensitive: c.Mode == modeExecutable || match.Match(c.Path), pending: pending})
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if a.sensitive != b.sensitive {
			if a.sensitive {
				return -1
			}
			return 1
		}
		if d := actionRank[a.Action] - actionRank[b.Action]; d != 0 {
			return d
		}
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Pack, b.Pack); c != 0 {
			return c
		}
		return strings.Compare(a.Mode, b.Mode)
	})
	return rows, nil
}

// fit starts with every list at its cap and shortens the lists one row at
// a time, in the order of the list constants, until the body fits
// Caps.MaxBody; sizes are computed, so only the final body is built. The
// result passes check.
func (r *renderer) fit() (string, error) {
	var n [numLists]int
	for k, c := range r.lists {
		if c != nil {
			n[k] = len(c.items)
		}
	}
	limit := r.in.Caps.MaxBody
	if limit > 0 && r.in.Marker == "" {
		if limit <= markerRoom {
			return "", fmt.Errorf("%w: %d bytes leave no room for a marker of up to %d", ErrTooLarge, limit, marker.MaxLine)
		}
		limit -= markerRoom
	}
	for k := 0; limit > 0 && r.size(n) > limit; {
		if k == numLists {
			return "", fmt.Errorf("%w: %d bytes with every list cut, more than %d", ErrTooLarge, r.size(n), limit)
		}
		if n[k] == 0 {
			k++
			continue
		}
		n[k]--
	}
	body := r.body(n)
	if err := r.check(body); err != nil {
		return "", err
	}
	return body, nil
}

// piece is one part of a body: fixed text, or a list showing rows rows.
type piece struct {
	text string
	list *cuttable
	rows int
}

func (p piece) size() int {
	if p.list != nil {
		return p.list.size(p.rows)
	}
	return len(p.text)
}

func (p piece) render() string {
	if p.list != nil {
		return p.list.render(p.rows)
	}
	return p.text
}

// pieces are the parts of the body in order, each list showing its n rows.
func (r *renderer) pieces(n [numLists]int) [13]piece {
	return [...]piece{
		{text: r.intro},
		{text: r.head},
		{text: r.assumed},
		{text: r.sensitive},
		{list: r.lists[listChanges], rows: n[listChanges]},
		{list: r.lists[listLocal], rows: n[listLocal]},
		{text: r.declinedBlock},
		{list: r.lists[listPending], rows: n[listPending]},
		{text: r.updateBlock},
		{text: r.nothingBlock},
		{text: r.controls},
		{text: r.footnote},
		{text: r.in.Marker},
	}
}

// size is len(r.body(n)), without building the body.
func (r *renderer) size(n [numLists]int) int {
	total, parts := 0, 0
	for _, p := range r.pieces(n) {
		if s := p.size(); s > 0 {
			total += s
			parts++
		}
	}
	return total + 2*max(parts-1, 0)
}

// body joins the non-empty parts with blank lines, each list showing its n
// rows.
func (r *renderer) body(n [numLists]int) string {
	var b strings.Builder
	b.Grow(r.size(n))
	for _, p := range r.pieces(n) {
		s := p.render()
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

// cuttable is a section with a list the budget may shorten: prefix, then
// open and the first n items, then a count of the rows not shown, then
// suffix. A nil cuttable is an absent section.
type cuttable struct {
	prefix, open, suffix string
	// items are the rendered rows, at most maxRows; each starts with its
	// line break.
	items []string
	// total counts every row of the list, shown or not.
	total int
	// one and many name a row in the count.
	one, many string
	// sums[n] is the length of items[:n].
	sums []int
}

func newCuttable(prefix, open, suffix string, items []string, total int, one, many string) *cuttable {
	c := &cuttable{prefix: prefix, open: open, suffix: suffix, items: items, total: total, one: one, many: many}
	c.sums = make([]int, len(items)+1)
	for i, it := range items {
		c.sums[i+1] = c.sums[i] + len(it)
	}
	return c
}

// render returns the section with the first n items.
func (c *cuttable) render(n int) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.Grow(c.size(n))
	b.WriteString(c.prefix)
	if n > 0 {
		b.WriteString(c.open)
		for _, it := range c.items[:n] {
			b.WriteString(it)
		}
	}
	if more := c.total - n; more > 0 {
		b.WriteString("\n\n" + moreLine(n, more, c.one, c.many))
	}
	b.WriteString(c.suffix)
	return b.String()
}

// size is len(c.render(n)).
func (c *cuttable) size(n int) int {
	if c == nil {
		return 0
	}
	s := len(c.prefix) + len(c.suffix)
	if n > 0 {
		s += len(c.open) + c.sums[n]
	}
	if more := c.total - n; more > 0 {
		s += len("\n\n") + len(moreLine(n, more, c.one, c.many))
	}
	return s
}

// check is the final assertion: no line starts with "/" after leading
// blanks (GitLab quick actions run from any line of a description, through
// the API too), and no line after the intro has an "@" outside code that
// could mention someone. The intro passed the stricter CheckIntro rules.
func (r *renderer) check(body string) error {
	if strings.IndexByte(body, '\r') >= 0 {
		return fmt.Errorf("%w: a carriage return, which Markdown reads as a line break", ErrUnsafe)
	}
	introLines := 0
	if r.intro != "" {
		introLines = strings.Count(r.intro, "\n") + 1
	}
	for i, line := range strings.Split(body, "\n") {
		if startsWithSlash(line) {
			return fmt.Errorf("%w: line %d starts with %q, which GitLab runs as a quick action", ErrUnsafe, i+1, "/")
		}
		if i < introLines {
			continue
		}
		if at := mentionAt(line); at >= 0 {
			return fmt.Errorf("%w: line %d: the %q at byte %d could mention someone", ErrUnsafe, i+1, "@", at+1)
		}
	}
	return nil
}

// hubLine is the line after the intro: packs, hub and content commit.
func (r *renderer) hubLine(hub hubRef) string {
	var b strings.Builder
	b.WriteString("touchmark syncs ")
	if packs := uniqueInOrder(r.in.Packs); len(packs) == 0 {
		b.WriteString("engineering assets")
	} else {
		b.WriteString(plural(len(packs), "pack ", "packs "))
		b.WriteString(joinAnd(codes(packs)))
	}
	b.WriteString(" from the hub")
	link := ""
	if hub.url != "" {
		link = "[" + Code(hub.name) + "](" + hub.url + ")"
	}
	switch {
	case r.in.HubName != "" && link != "":
		b.WriteString(" " + Code(r.in.HubName) + " (" + link + ")")
	case r.in.HubName != "":
		b.WriteString(" " + Code(r.in.HubName))
	case link != "":
		b.WriteString(" " + link)
	}
	if c := r.in.ContentCommit; c != "" {
		commit := Code(shortCommit(c))
		if hub.commit != "" {
			commit = "[" + commit + "](" + hub.commit + ")"
		}
		b.WriteString(" at commit " + commit)
	}
	b.WriteString(".")
	return b.String()
}

// assumedBlock is the paragraph of a target the hub subscribed without an
// opt-in file: how to choose packs or ignore files, and how to opt out.
func (r *renderer) assumedBlock() string {
	if !r.in.Assumed {
		return ""
	}
	name, file, it := "opt-in file", "an opt-in file", "one"
	if r.in.OptInFile != "" {
		name, file, it = Code(r.in.OptInFile), Code(r.in.OptInFile), "it"
	}
	return "The hub subscribed this repository, which has no " + name + ". " +
		"To add packs or keep files out of the sync, add " + file + " with `packs` or `ignore`; " +
		"to stop these " + r.noun + "s, add " + it + " with `enabled: false`."
}

// sensitiveSection is the ⚠ section: the sensitive rows of the table and of
// what a rebuild would bring, by path, and the Gitea workflows warning.
func (r *renderer) sensitiveSection() string {
	var items []row
	for _, x := range slices.Concat(r.rows, r.pending) {
		if x.sensitive {
			items = append(items, x)
		}
	}
	if len(items) == 0 && !r.in.GiteaWorkflows {
		return ""
	}
	slices.SortStableFunc(items, func(a, b row) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if a.pending != b.pending {
			if b.pending {
				return -1
			}
			return 1
		}
		return actionRank[a.Action] - actionRank[b.Action]
	})
	var b strings.Builder
	b.WriteString("### ⚠ Sensitive paths")
	if len(items) > 0 {
		b.WriteString("\n\nThese files can run code in CI or change how tools and agents work in this repository. Review them first.\n")
		for _, x := range items {
			b.WriteString("\n- " + Code(x.Path) + ": " + actionText(x.Change))
			if x.Pack != "" {
				b.WriteString(", pack " + Code(x.Pack))
			}
			if x.pending {
				b.WriteString(" (after a rebuild)")
			}
		}
	}
	if r.in.GiteaWorkflows {
		b.WriteString("\n\nThis " + r.noun + " adds `.gitea/workflows` or `.forgejo/workflows`, which turns off `.github/workflows` in this repository on Gitea and Forgejo.")
	}
	return b.String()
}

// changesSection is the table of changes.
func (r *renderer) changesSection() *cuttable {
	if len(r.rows) == 0 {
		return nil
	}
	items := make([]string, 0, min(len(r.rows), maxRows))
	for _, x := range r.rows[:min(len(r.rows), maxRows)] {
		var b strings.Builder
		b.WriteString("\n| ")
		if x.sensitive {
			b.WriteString("⚠ ")
		}
		b.WriteString(actionText(x.Change) + " | " + cellCode(x.Path) + " |")
		if x.Pack != "" {
			b.WriteString(" " + cellCode(x.Pack))
		}
		b.WriteString(" |")
		items = append(items, b.String())
	}
	return newCuttable("### Changes", "\n\n| Change | File | Pack |\n|---|---|---|", "", items, len(r.rows), "change", "changes")
}

// localSection is the list of local files: collapsed in a details element,
// or, where the description cannot carry HTML (renderer.html), plain
// Markdown under a heading.
func (r *renderer) localSection() *cuttable {
	total := len(r.local)
	if total == 0 {
		return nil
	}
	summary := strconv.Itoa(total) + " files here differ from the hub and stay as they are"
	if total == 1 {
		summary = "1 file here differs from the hub and stays as it is"
	}
	hints := "touchmark does not update files changed in this repository. " +
		"To take the hub's version of one, run `touchmark apply --adopt <path>`. " +
		"To stop seeing it here, add it to `ignore` in " + r.optIn + "."
	prefix := "<details>\n<summary>" + summary + "</summary>\n\n" + hints
	suffix := "\n\n</details>"
	if !r.html() {
		prefix, suffix = "### Local files\n\n"+summary+". "+hints, ""
	}
	items := make([]string, 0, min(total, maxRows))
	for _, p := range r.local[:min(total, maxRows)] {
		items = append(items, "\n- "+Code(p))
	}
	return newCuttable(prefix, "\n", suffix, items, total, "file", "files")
}

// html reports whether the description may carry touchmark's HTML: the
// details element of the local files, and the comments that tag the
// controls. A platform that escapes HTML in descriptions shows it as text;
// it is the platform whose marker is a reference definition and whose
// descriptions carry no controls (Caps.BodyControls false: Bitbucket
// Cloud).
func (r *renderer) html() bool { return r.in.Caps.BodyControls() }

// previouslyDeclined is the block for declines that overlap D.
func (r *renderer) previouslyDeclined() string {
	if len(r.declined) == 0 {
		return ""
	}
	refs := make([]string, len(r.declined))
	for i, n := range r.declined {
		refs[i] = r.sigil + strconv.FormatInt(n, 10)
	}
	return "### Previously declined\n\nSome of these changes were in " + joinAnd(refs) + ", " +
		plural(len(refs), "which was", "which were") + " closed without merging. " +
		"This " + r.noun + " brings the whole current set anyway, because the files of a pack go together. " +
		"To keep a file as it is here, add it to `ignore` in " + r.optIn + "."
}

// pausedBlock is "touchmark paused" with the list of what a rebuild would
// bring.
func (r *renderer) pausedBlock() *cuttable {
	if !r.in.Paused {
		return nil
	}
	prefix := "### touchmark paused\n\nThis branch has commits touchmark did not make, so touchmark stopped updating it to keep them."
	switch {
	case r.in.BranchUnknown:
		prefix += " None of its latest commits is touchmark's, so touchmark cannot tell which of its changes the branch holds now."
	case len(r.rows) > 0:
		prefix += " The changes above are what the branch holds now."
	}
	switch {
	case len(r.pending) > 0:
		prefix += " A rebuild from the default branch would bring:"
	case !r.in.BranchUnknown:
		prefix += " A rebuild from the default branch would bring nothing new; it would only drop the other commits."
	}
	items := make([]string, 0, min(len(r.pending), maxRows))
	for _, x := range r.pending[:min(len(r.pending), maxRows)] {
		item := "\n- "
		if x.sensitive {
			item += "⚠ "
		}
		item += actionText(x.Change) + " " + Code(x.Path)
		if x.Pack != "" {
			item += " (pack " + Code(x.Pack) + ")"
		}
		items = append(items, item)
	}
	suffix := ""
	switch {
	case r.in.ShowRecreate:
		suffix = "\n\nTo rebuild the branch and drop the other commits, tick **Rebuild this branch** below."
	case !r.in.Caps.BodyControls():
		suffix = "\n\nTo rebuild the branch and drop the other commits, ask the hub's maintainers to add a `recreate` entry " +
			"with this branch's head to `.touchmark/operations.yml`."
	}
	return newCuttable(prefix, "\n", suffix, items, len(r.pending), "change", "changes")
}

// updateBranch is the block of a move GitHub refuses without the Workflows
// permission: the default branch brought workflow changes, and Update branch
// makes the move instead. While paused, the rebuild that needed the move was
// asked for and could not happen: the request does not survive Update branch
// (a ticked control is unticked when the body is written, an operations.yml
// entry names the old head), so the block asks for it again instead of
// promising to continue.
func (r *renderer) updateBranch() string {
	if !r.in.UpdateBranchNeeded {
		return ""
	}
	s := "### Update branch needed\n\ntouchmark has to move this branch onto the latest default branch, " +
		"and that carries workflow changes its token may not push. "
	if !r.in.Paused {
		return s + "Press **Update branch** on this " + r.noun + "; touchmark continues on the next run."
	}
	s += "Press **Update branch** on this " + r.noun + ", then ask for the rebuild again"
	if r.in.ShowRecreate {
		return s + ": tick **Rebuild this branch** below. touchmark rebuilds the branch on the next run."
	}
	return s + "."
}

// nothingMore is the block of an edited branch when D is empty. D is empty
// when the default branch has the hub's versions, and also when the team
// ignored the paths or changed them here: the text claims neither.
func (r *renderer) nothingMore() string {
	if !r.in.NothingMore {
		return ""
	}
	return "### Nothing more to sync\n\nThe hub proposes nothing more for this repository: the default branch has the hub's versions of these files, " +
		"or they are ignored or changed here. " +
		"This branch has commits touchmark did not make, so touchmark leaves this " + r.noun + " open. " +
		"Close it, or keep your commits and merge it."
}

// controlLines are the unticked controls.
func (r *renderer) controlLines() string {
	var lines []string
	if r.in.ShowRecreate {
		lines = append(lines, ControlLine(ControlRecreate))
	}
	if r.in.ShowRepropose {
		lines = append(lines, ControlLine(ControlRepropose))
	}
	return strings.Join(lines, "\n")
}

// controlLine is one unticked control, as Ticked reads it.
func controlLine(name, label string) string {
	return "- [ ] <!-- touchmark:" + name + " --> " + label
}

// note is the footnote: how memory works and how to talk to touchmark. It
// is plain Markdown after a thematic break: no HTML before its code spans,
// which the assertion would read as possibly swallowing them.
func (r *renderer) note() string {
	return "---\n\nIf this " + r.noun + " is closed without merging, touchmark remembers it and does not propose the same changes again; " +
		"a new one comes when the hub changes these files. " +
		"Editing `packs` or `ignore` in " + r.optIn + " resets that memory, and `ignore` opts a file out for good. " +
		r.forgetHint() +
		"touchmark rewrites this description, so please comment instead of editing it."
}

// forgetHint is the footnote's sentence on the forget_declines entry, where
// nothing in a declined pull request can bring its changes back: its
// description carries no control, or can never be edited once declined.
// "" elsewhere.
func (r *renderer) forgetHint() string {
	if r.in.Caps.BodyControls() && !r.in.Caps.ClosedImmutable {
		return ""
	}
	return "To have the same changes proposed again sooner, the hub's maintainers can add a `forget_declines` entry " +
		"with the number of this " + r.noun + " to `.touchmark/operations.yml`. "
}

// actionText is how a change reads in the table and the lists.
func actionText(c Change) string {
	switch c.Action {
	case ActionDelete:
		return "delete"
	case ActionChmod:
		switch c.Mode {
		case modeExecutable:
			return "chmod +x"
		case "100644":
			return "chmod -x"
		}
		return "chmod"
	}
	if c.Mode == modeExecutable {
		return c.Action + ", executable"
	}
	return c.Action
}

// moreLine counts the rows a list does not show.
func moreLine(shown, more int, one, many string) string {
	if shown == 0 {
		return strconv.Itoa(more) + " " + plural(more, one, many) + ", too many to list here."
	}
	return "…and " + strconv.Itoa(more) + " more."
}

// checkMarker accepts one "<!-- touchmark:… -->" line whose comment ends
// only at the end of the line, or one `[touchmark]: # "touchmark:…"`
// reference definition whose title ends only at the end of the line (the
// two frames of marker.EncodeFrame).
func checkMarker(m string) error {
	const refDef = `[touchmark]: # "touchmark:`
	switch {
	case strings.ContainsAny(m, "\r\n"):
		return errors.New("the marker is not one line")
	case strings.HasPrefix(m, refDef):
		if !strings.HasSuffix(m, `"`) || strings.IndexByte(m[len(refDef):], '"') != len(m)-len(refDef)-1 ||
			strings.Contains(m, `\`) {
			return fmt.Errorf("%.40q… is not a marker reference definition", m)
		}
	case !strings.HasPrefix(m, "<!-- touchmark:") || !strings.HasSuffix(m, " -->") || strings.Index(m, "-->") != len(m)-len("-->"):
		return fmt.Errorf("%.40q… is not a marker comment", m)
	}
	return nil
}

// hubRef is the hub as the hub line links it.
type hubRef struct {
	name   string // host and path, the link text
	url    string // the repository link, "" for none
	commit string // the content commit link, "" for none
}

// parseHubURL checks raw, a hub repository URL, and derives the links: an
// absolute http(s) URL of unreserved characters, without credentials, query
// or fragment. A hub on github.com is linked through redirect.github.com,
// and only there is the content commit linked too.
func parseHubURL(raw, commit string) (hubRef, error) {
	if raw == "" {
		return hubRef{}, nil
	}
	for _, c := range []byte(raw) {
		if !isURLByte(c) {
			return hubRef{}, fmt.Errorf("hub URL %.80q has a character that is unsafe in a Markdown link: %q", raw, c)
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return hubRef{}, fmt.Errorf("hub URL %.80q is not an absolute http(s) URL", raw)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	host := strings.ToLower(u.Host)
	ref := hubRef{name: host + path, url: u.Scheme + "://" + u.Host + path}
	if host == "github.com" || host == "www.github.com" {
		ref.url = "https://redirect.github.com" + path
		if path != "" && isCommitID(commit) {
			ref.commit = ref.url + "/commit/" + commit
		}
	}
	return ref, nil
}

// isURLByte reports whether c may appear in a hub URL: letters, digits and
// "-._~:/%+". Nothing else is needed for a host and a repository path, and
// none of these can end a Markdown link destination or start a mention.
func isURLByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~:/%+", c) >= 0
}

// isCommitID reports whether s is a full SHA-1 or SHA-256 commit id in
// lowercase hex.
func isCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// shortCommit returns the first 12 characters of a commit id, or c when it
// is not one.
func shortCommit(c string) string {
	if isCommitID(c) {
		return c[:12]
	}
	return c
}

// prNumbers returns the positive numbers of ns, sorted, without repeats.
func prNumbers(ns []int64) []int64 {
	var out []int64
	for _, n := range ns {
		if n > 0 {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sortedUnique returns ss sorted by bytes, without repeats.
func sortedUnique(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return slices.Compact(out)
}

// uniqueInOrder returns ss without repeats, keeping first positions.
func uniqueInOrder(ss []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// codes puts each of ss in a code span.
func codes(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = Code(s)
	}
	return out
}

// joinAnd joins items as "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// plural returns one for n == 1 and many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
