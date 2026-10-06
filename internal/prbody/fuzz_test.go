package prbody

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// hostileSeed joins hostile strings for the fuzzers' split inputs.
var hostileSeed = strings.Join([]string{
	"AGENTS.md", ".github/workflows/ci.yml", "`a`", "``", "@user", "/close", " /merge", "a|b", `a\`,
	"x\n/close", "x\r\n- [x] <!-- touchmark:recreate -->", "<details>", "<!--", "-->", "&#64;x",
	"[l](https://e.com)", " padded ", "\xe2\x80\xaeevil", "\xff", "",
}, "\x00")

// splitN splits s at NUL bytes, keeping at most n parts.
func splitN(s string, n int) []string {
	parts := strings.Split(s, "\x00")
	if len(parts) > n {
		parts = parts[:n]
	}
	return parts
}

// fuzzInput builds an Input from fuzzed values.
func fuzzInput(t *testing.T, paths, packs, local, hub string, budget uint16, flags uint16) Input {
	actions := []string{ActionCreate, ActionUpdate, ActionDelete, ActionChmod}
	modes := []string{"100644", "100755", ""}
	in := Input{
		HubName:            hub,
		ContentCommit:      hubCommit,
		Packs:              splitN(packs, 5),
		Local:              splitN(local, 250),
		OptInFile:          hub,
		Sensitive:          []string{"deploy/**", hub},
		Paused:             flags&1 != 0,
		UpdateBranchNeeded: flags&2 != 0,
		NothingMore:        flags&4 != 0,
		GiteaWorkflows:     flags&8 != 0,
		ShowRecreate:       flags&16 != 0,
		ShowRepropose:      flags&32 != 0,
		Caps:               capsOf(flavors[int(flags>>8)%len(flavors)]),
		Marker:             testMarker(t),
	}
	if flags&64 != 0 {
		in.Intro = intro
	}
	if flags&(1<<13) != 0 {
		in.Marker = ""
	}
	if flags&128 != 0 {
		in.HubURL = hubURL
	}
	if budget != 0 {
		in.Caps.MaxBody = int(budget)
	}
	ps := splitN(paths, 250)
	for i, p := range ps {
		c := Change{Path: p, Pack: in.Packs[i%len(in.Packs)], Action: actions[i%len(actions)], Mode: modes[i%len(modes)]}
		in.Changes = append(in.Changes, c)
		if i%3 == 0 {
			c.Action = ActionUpdate
			in.Pending = append(in.Pending, c)
		}
	}
	for i := range int(flags>>11) % 4 {
		in.PreviouslyDeclined = append(in.PreviouslyDeclined, int64(i*7+1))
	}
	return in
}

// FuzzRender renders bodies from hostile paths, packs, local files and hub
// names: whatever renders has no line that starts with "/", no "@" outside
// code, the marker last, the ⚠ section whenever a change is sensitive, and
// fits the budget. The only error is ErrTooLarge, when even the body
// without lists does not fit.
func FuzzRender(f *testing.F) {
	f.Add("AGENTS.md\x00.github/workflows/ci.yml\x00scripts/x.sh", "agents\x00ci", "docs/x.md", "acme-eng", uint16(0), uint16(0xffff))
	f.Add(hostileSeed, hostileSeed, hostileSeed, "@hub`", uint16(0), uint16(0x3bf))
	f.Add(hostileSeed, "@p", hostileSeed, "h", uint16(1500), uint16(0x1c1))
	f.Add(strings.Repeat("deep/", 90)+"x", "p", strings.Repeat("y/", 200), "", uint16(3000), uint16(0x101))
	f.Add("a", "", "", "", uint16(1), uint16(0))
	f.Add(hostileSeed, hostileSeed, hostileSeed, "x", uint16(0), uint16(0x21ff))
	f.Fuzz(func(t *testing.T, paths, packs, local, hub string, budget uint16, flags uint16) {
		in := fuzzInput(t, paths, packs, local, hub, budget, flags)
		body, err := Render(in)
		if err != nil {
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("Render: %v", err)
			}
			r, err := newRenderer(in)
			if err != nil {
				t.Fatal(err)
			}
			need := len(r.body([numLists]int{}))
			if in.Marker == "" {
				need += markerRoom
			}
			if need <= in.Caps.MaxBody {
				t.Fatalf("ErrTooLarge, but the body without lists needs %d bytes of %d", need, in.Caps.MaxBody)
			}
			return
		}
		checkBody(t, in, body)
		if !utf8.ValidString(body) {
			t.Error("the body is not valid UTF-8")
		}
	})
}

// FuzzCode checks that a code span shows its text exactly: alone, in a list
// item and, through cellCode, in a table cell.
func FuzzCode(f *testing.F) {
	for _, s := range strings.Split(hostileSeed, "\x00") {
		f.Add(s)
	}
	f.Add("```` `` ` ")
	f.Add(" ` ")
	f.Add("\\`|\\|")
	f.Fuzz(func(t *testing.T, s string) {
		want := shown(s)
		out := Code(s)
		if strings.ContainsAny(out, "\r\n") || !utf8.ValidString(out) {
			t.Fatalf("Code(%q) = %q", s, out)
		}
		if sp := codeSpans(out); len(sp) != 1 || sp[0].start != 0 || sp[0].end != len(out) || sp[0].text != want {
			t.Fatalf("Code(%q) = %q parses as %+v, want %q", s, out, sp, want)
		}
		line := "- " + out + ": update"
		if sp := codeSpans(line); len(sp) != 1 || sp[0].start != 2 || sp[0].text != want {
			t.Fatalf("in a list item %q parses as %+v", line, sp)
		}
		cells := gfmCells("| update | " + cellCode(s) + " | `p` |")
		if len(cells) != 3 {
			t.Fatalf("cellCode(%q) splits the row into %q", s, cells)
		}
		if sp := codeSpans(cells[1]); len(sp) != 1 || sp[0].start != 0 || sp[0].end != len(cells[1]) || sp[0].text != want {
			t.Fatalf("cellCode(%q): cell %q parses as %+v, want %q", s, cells[1], sp, want)
		}
		if mentionAt(line) >= 0 {
			t.Fatalf("mentionAt(%q) finds a mention inside the span", line)
		}
	})
}

// FuzzTicked checks Ticked and Untick against each other on any body.
func FuzzTicked(f *testing.F) {
	f.Add("- [x] <!-- touchmark:recreate --> Rebuild\n- [ ] <!-- touchmark:repropose -->")
	f.Add("* [ X ]<!--touchmark:repropose-->\r\n- [x] <!-- touchmark:recreate")
	f.Add("-[x]<!-- touchmark:recreate -->x\n\n- [x] <!-- touchmark:recreatex -->")
	f.Fuzz(func(t *testing.T, body string) {
		controls := []string{ControlRecreate, ControlRepropose}
		for i, c := range controls {
			other := controls[1-i]
			u := Untick(body, c)
			switch {
			case Ticked(u, c):
				t.Fatalf("Untick(%q, %s) is still ticked", body, c)
			case Untick(u, c) != u:
				t.Fatalf("Untick(%q, %s) is not idempotent", body, c)
			case Ticked(u, other) != Ticked(body, other):
				t.Fatalf("Untick(%q, %s) changed %s", body, c, other)
			case strings.Count(u, "\n") != strings.Count(body, "\n") || len(u) > len(body):
				t.Fatalf("Untick(%q, %s) = %q changed more than a checkbox", body, c, u)
			case !Ticked(body, c) && u != body:
				t.Fatalf("Untick(%q, %s) changed a body without the control ticked", body, c)
			}
		}
	})
}

// FuzzDeclinedComment checks that the comment is safe for any paths and
// that its YAML is an ignore list the opt-in parser accepts, whose patterns
// match exactly their valid paths.
func FuzzDeclinedComment(f *testing.F) {
	f.Add(int64(12), "AGENTS.md\x00docs/[draft].md\x00@x/y.md", optInName)
	f.Add(int64(0), hostileSeed, "@opt`in")
	f.Add(int64(-3), " \x00/\x00./a\x00a*b?c\x00\t tab", "")
	f.Fuzz(func(t *testing.T, pr int64, paths, optIn string) {
		ps := splitN(paths, 300)
		got := DeclinedComment(pr, ps, optIn)
		checkComment(t, got)
		if !strings.Contains(got, "```yaml") {
			return
		}
		patterns := yamlIgnore(t, got)
		if len(patterns) == 0 || len(patterns) > maxRows {
			t.Fatalf("%d patterns", len(patterns))
		}
		if _, _, err := config.ParseOptIn([]byte("version: 1\n" + yamlBlock(t, got))); err != nil {
			t.Fatalf("ParseOptIn: %v", err)
		}
		var listed []string
		for _, p := range sortedUnique(ps) {
			if pathx.NormalizePattern(ignorePattern(p)) != "" {
				listed = append(listed, p)
			}
		}
		for i, pattern := range patterns {
			p := listed[i]
			if pathx.Validate(p) != nil {
				continue
			}
			if pattern != ignorePattern(p) || !pathx.NewMatcher([]string{pattern}, true).Match(p) {
				t.Fatalf("pattern %q for %q", pattern, p)
			}
		}
	})
}

// FuzzCheckIntro checks that an accepted intro keeps its promises and
// renders.
func FuzzCheckIntro(f *testing.F) {
	f.Add(intro)
	f.Add("```\ncode\n```\n<details>\n</details>")
	f.Add("a@b and @c\n/close\n<!-- touchmark:recreate -->")
	f.Add("~~~\n<pre>\n~~~")
	f.Fuzz(func(t *testing.T, s string) {
		prepared, err := prepareIntro(s)
		if err != nil {
			if !errors.Is(err, ErrIntro) {
				t.Fatalf("prepareIntro(%q): %v", s, err)
			}
			return
		}
		if CheckIntro(s) != nil {
			t.Fatal("CheckIntro disagrees with prepareIntro")
		}
		for _, line := range strings.Split(prepared, "\n") {
			if startsSlash(line) || hasTouchmarkComment(line) {
				t.Fatalf("accepted line %q", line)
			}
			for i := 0; i < len(line); i++ {
				if line[i] == '@' && (i == 0 || !isWordByte(line[i-1])) {
					t.Fatalf("accepted a mention in %q", line)
				}
			}
		}
		in := base(t, "gitlab")
		in.Intro = s
		body, err := Render(in)
		if err != nil {
			t.Fatalf("Render with an accepted intro: %v", err)
		}
		if prepared != "" && !strings.HasPrefix(body, prepared+"\n\n") {
			t.Fatal("the body does not start with the intro")
		}
		if !strings.HasSuffix(body, "\n\n"+in.Marker) || Ticked(body, ControlRecreate) || Ticked(body, ControlRepropose) {
			t.Fatal("an accepted intro changed the end of the body or ticked a control")
		}
	})
}

// hasTouchmarkComment reports whether line has "<!--", blanks, then
// "touchmark:" in any case.
func hasTouchmarkComment(line string) bool {
	lower := strings.ToLower(line)
	for i := strings.Index(lower, "<!--"); i >= 0; {
		rest := strings.TrimLeft(lower[i+len("<!--"):], " \t\n\f\r")
		if strings.HasPrefix(rest, "touchmark:") {
			return true
		}
		j := strings.Index(lower[i+1:], "<!--")
		if j < 0 {
			break
		}
		i += 1 + j
	}
	return false
}
