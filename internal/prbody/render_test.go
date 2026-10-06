package prbody

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
)

// intro is the pr.intro_file of the tests.
const intro = "Acme keeps its shared engineering files (agent instructions, editor and CI settings) in one hub.\n" +
	"Review this like any other change; questions go to the platform team's channel."

// base is the input every scenario starts from.
func base(t testing.TB, flavor string) Input {
	return Input{
		Intro:         intro,
		HubName:       "acme-eng",
		HubURL:        hubURL,
		ContentCommit: hubCommit,
		Packs:         []string{"agents", "claude", "base"},
		OptInFile:     optInName,
		Caps:          capsOf(flavor),
		Marker:        testMarker(t),
	}
}

// scenarios are the golden bodies, for each platform's Caps.
var scenarios = []struct {
	name string
	edit func(in *Input)
}{
	{"new", func(in *Input) {
		in.Changes = []Change{
			{Path: "prompts/review.md", Pack: "agents", Action: ActionCreate, Mode: "100644"},
			{Path: "AGENTS.md", Pack: "agents", Action: ActionUpdate, Mode: "100644"},
			{Path: "prompts/old.md", Pack: "agents", Action: ActionDelete},
			{Path: ".claude/settings.json", Pack: "claude", Action: ActionUpdate, Mode: "100644"},
			{Path: "scripts/check.sh", Pack: "base", Action: ActionCreate, Mode: "100755"},
			{Path: "tools/lint.sh", Pack: "base", Action: ActionChmod, Mode: "100755"},
			{Path: "docs/guide.md", Pack: "base", Action: ActionChmod, Mode: "100644"},
		}
	}},
	{"local", func(in *Input) {
		// A private hub and a public target: no link, no repository name.
		in.HubURL = ""
		in.Changes = []Change{{Path: "AGENTS.md", Pack: "agents", Action: ActionUpdate, Mode: "100644"}}
		in.Local = []string{"docs/guide.md", "CONTRIBUTING.md", ".github/CODEOWNERS"}
	}},
	{"paused", func(in *Input) {
		in.Paused, in.ShowRecreate = true, true
		in.Changes = []Change{
			{Path: "AGENTS.md", Pack: "agents", Action: ActionUpdate, Mode: "100644"},
			{Path: "prompts/review.md", Pack: "agents", Action: ActionCreate, Mode: "100644"},
		}
		in.Pending = []Change{
			{Path: "AGENTS.md", Pack: "agents", Action: ActionUpdate, Mode: "100644"},
			{Path: ".github/workflows/lint.yml", Pack: "ci", Action: ActionCreate, Mode: "100644"},
		}
	}},
	{"nothing-more", func(in *Input) {
		in.NothingMore = true
		in.Changes = []Change{{Path: "prompts/review.md", Pack: "agents", Action: ActionCreate, Mode: "100644"}}
	}},
	{"declined", func(in *Input) {
		in.PreviouslyDeclined = []int64{9, 7}
		in.Changes = []Change{
			{Path: "AGENTS.md", Pack: "agents", Action: ActionUpdate, Mode: "100644"},
			{Path: "prompts/review.md", Pack: "agents", Action: ActionCreate, Mode: "100644"},
		}
	}},
	{"huge", func(in *Input) {
		in.Changes = hugeChanges(1000)
		for i := range 150 {
			in.Local = append(in.Local, fmt.Sprintf("docs/handbook/chapter-%02d/local-%03d.md", i%12, i))
		}
	}},
	{"sensitive-only", func(in *Input) {
		in.Sensitive = []string{"deploy/**"}
		in.GiteaWorkflows = true
		in.Changes = []Change{
			{Path: ".github/workflows/ci.yml", Pack: "ci", Action: ActionUpdate, Mode: "100644"},
			{Path: ".gitea/workflows/ci.yml", Pack: "ci", Action: ActionCreate, Mode: "100644"},
			{Path: "deploy/prod.yaml", Pack: "ops", Action: ActionUpdate, Mode: "100644"},
			{Path: "scripts/release.sh", Pack: "base", Action: ActionChmod, Mode: "100755"},
			{Path: ".github/CODEOWNERS", Pack: "base", Action: ActionDelete},
		}
	}},
}

// hugeChanges returns n changes of every action, three of them sensitive.
func hugeChanges(n int) []Change {
	actions := []string{ActionCreate, ActionUpdate, ActionDelete, ActionChmod}
	var out []Change
	for i := range n {
		c := Change{Path: fmt.Sprintf("docs/handbook/chapter-%02d/page-%03d.md", i%12, i), Pack: "handbook", Action: actions[i%len(actions)], Mode: "100644"}
		if c.Action == ActionDelete {
			c.Mode = ""
		}
		out = append(out, c)
	}
	return append(out,
		Change{Path: ".github/workflows/docs.yml", Pack: "ci", Action: ActionUpdate, Mode: "100644"},
		Change{Path: ".pre-commit-config.yaml", Pack: "base", Action: ActionCreate, Mode: "100644"},
		Change{Path: "bin/setup", Pack: "base", Action: ActionCreate, Mode: "100755"},
	)
}

func scenarioInput(t testing.TB, name, flavor string) Input {
	in := base(t, flavor)
	for _, s := range scenarios {
		if s.name == name {
			s.edit(&in)
			return in
		}
	}
	t.Fatalf("no scenario %q", name)
	return in
}

func TestRenderGolden(t *testing.T) {
	for _, s := range scenarios {
		for _, flavor := range flavors {
			t.Run(s.name+"/"+flavor, func(t *testing.T) {
				in := scenarioInput(t, s.name, flavor)
				body, err := Render(in)
				if err != nil {
					t.Fatalf("Render: %v", err)
				}
				checkBody(t, in, body)
				golden(t, s.name+"/"+flavor+".md", body)
			})
		}
	}
}

// checkBody checks the properties every body has. A body rendered without
// a marker is checked with the longest marker appended.
func checkBody(t *testing.T, in Input, body string) {
	t.Helper()
	full := body
	if in.Marker == "" {
		full = body + "\n\n" + longestMarker
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "<!-- touchmark:") {
				t.Error("a body rendered without a marker has a marker line")
			}
		}
	}
	if in.Caps.MaxBody > 0 && len(full) > in.Caps.MaxBody {
		t.Errorf("body of %d bytes with its marker, more than %d", len(full), in.Caps.MaxBody)
	}
	if in.Marker != "" {
		lines := strings.Split(body, "\n")
		if lines[len(lines)-1] != in.Marker {
			t.Errorf("the last line is %.60q, not the marker", lines[len(lines)-1])
		}
		if m, status := marker.Find(body, []string{hubFP}); status != marker.Found || m.Key != testKey {
			t.Errorf("marker.Find: %v", status)
		}
		if marker.Strip(body) != strings.TrimSuffix(body, "\n\n"+in.Marker) {
			t.Error("the human part is not the body without its marker line")
		}
	}
	lines := strings.Split(body, "\n")
	// gen is what touchmark generated: the body after the intro, which
	// CheckIntro holds to its own rules.
	prepared, err := prepareIntro(in.Intro)
	if err != nil {
		t.Fatal(err)
	}
	gen := body
	if prepared != "" {
		if !strings.HasPrefix(body, prepared+"\n\n") {
			t.Fatal("the body does not start with the intro")
		}
		gen = body[len(prepared)+2:]
	}
	for i, line := range lines {
		if startsSlash(line) {
			t.Errorf("line %d starts with a slash: %q", i+1, line)
		}
	}
	checkSafe(t, gen, 1)
	sensitive := in.GiteaWorkflows
	var paths []string
	for _, c := range in.Changes {
		if Sensitive(c.Path, c.Mode, in.Sensitive) {
			sensitive = true
			paths = append(paths, c.Path)
		}
	}
	if in.Paused {
		for _, c := range in.Pending {
			if Sensitive(c.Path, c.Mode, in.Sensitive) {
				sensitive = true
				paths = append(paths, c.Path)
			}
		}
	}
	warn := section(gen, "### ⚠ Sensitive paths")
	if sensitive != (warn != "") {
		t.Errorf("sensitive changes: %v, ⚠ section: %q", sensitive, warn)
	}
	for _, p := range paths {
		if !strings.Contains(warn, Code(p)) {
			t.Errorf("the ⚠ section lacks %s", Code(p))
		}
	}
	if Ticked(body, ControlRecreate) || Ticked(body, ControlRepropose) {
		t.Error("a control is ticked in a fresh body")
	}
	rows := 0
	for _, l := range strings.Split(gen, "\n") {
		if strings.HasPrefix(l, "| ") {
			rows++
		}
	}
	if rows > maxRows+1 {
		t.Errorf("%d table rows", rows-1)
	}
	if again, err := Render(in); err != nil || again != body {
		t.Errorf("a second Render differs (%v)", err)
	}
}

// longestMarker stands for the longest marker line Encode writes.
var longestMarker = "<!-- touchmark:" + strings.Repeat("x", marker.MaxLine-len("<!-- touchmark: -->")) + " -->"

// TestRenderWithoutMarker checks the two-step build of a body: the human
// part, its hash in the marker, then both; the human part leaves room for
// any marker.
func TestRenderWithoutMarker(t *testing.T) {
	if len(longestMarker) != marker.MaxLine {
		t.Fatalf("longestMarker has %d bytes", len(longestMarker))
	}
	for _, s := range scenarios {
		in := scenarioInput(t, s.name, "github")
		withMarker, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		in.Marker = ""
		human, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		checkBody(t, in, human)
		// Without budget pressure the human part is the body without its
		// marker line, and Strip gives it back.
		if human != marker.Strip(withMarker) || human+"\n\n"+testMarker(t) != withMarker {
			t.Errorf("%s: the human part differs from the body without its marker", s.name)
		}
	}
	// Under pressure the human part keeps room for the longest marker.
	in := scenarioInput(t, "huge", "github")
	in.Marker = ""
	r, err := newRenderer(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Caps.MaxBody = len(r.body([numLists]int{30, 100, 0})) + markerRoom
	human, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(human, "\n| ") - 1; got != 30 {
		t.Errorf("%d table rows, want 30", got)
	}
	checkBody(t, in, human)
	in.Caps.MaxBody = markerRoom
	if _, err := Render(in); !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "no room for a marker") {
		t.Errorf("a budget of only a marker: %v", err)
	}
}

// TestRenderOrder checks the order of a body's parts, with every part shown.
func TestRenderOrder(t *testing.T) {
	in := base(t, "github")
	in.Changes = []Change{{Path: ".mcp.json", Pack: "agents", Action: ActionCreate, Mode: "100644"}}
	in.Pending = []Change{{Path: "AGENTS.md", Pack: "agents", Action: ActionUpdate, Mode: "100644"}}
	in.Local = []string{"docs/guide.md"}
	in.PreviouslyDeclined = []int64{4}
	in.Paused, in.UpdateBranchNeeded, in.NothingMore = true, true, true
	in.ShowRecreate, in.ShowRepropose = true, true
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{
		"Acme keeps its shared",
		"touchmark syncs packs",
		"### ⚠ Sensitive paths",
		"### Changes",
		"<details>",
		"### Previously declined",
		"### touchmark paused",
		"### Update branch needed",
		"### Nothing more to sync",
		"- [ ] <!-- touchmark:recreate --> Rebuild this branch (drops commits added by others)",
		"- [ ] <!-- touchmark:repropose --> Propose this content again",
		"\n---\n",
		in.Marker,
	}
	last := -1
	for _, p := range parts {
		i := strings.Index(body, p)
		if i <= last {
			t.Fatalf("%q at %d, after the previous part at %d\n%s", p, i, last, body)
		}
		last = i
	}
	if !strings.HasSuffix(body, "\n\n"+in.Marker) {
		t.Error("the marker is not the last line after a blank line")
	}
	checkBody(t, in, body)
}

func TestRenderTableOrder(t *testing.T) {
	in := base(t, "github")
	in.Changes = []Change{
		{Path: "b.md", Pack: "p", Action: ActionCreate},
		{Path: "a.md", Pack: "p", Action: ActionCreate},
		{Path: "z.md", Pack: "p", Action: ActionDelete},
		{Path: "m.sh", Pack: "p", Action: ActionChmod, Mode: "100644"},
		{Path: "c.md", Pack: "p", Action: ActionUpdate},
		{Path: ".gitattributes", Pack: "p", Action: ActionCreate},
		{Path: ".github/workflows/x.yml", Pack: "p", Action: ActionDelete},
		{Path: "run.sh", Pack: "p", Action: ActionUpdate, Mode: "100755"},
	}
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| Change") {
			got = append(got, l)
		}
	}
	want := []string{
		"| ⚠ delete | `.github/workflows/x.yml` | `p` |",
		"| ⚠ update, executable | `run.sh` | `p` |",
		"| ⚠ create | `.gitattributes` | `p` |",
		"| delete | `z.md` | `p` |",
		"| update | `c.md` | `p` |",
		"| chmod -x | `m.sh` | `p` |",
		"| create | `a.md` | `p` |",
		"| create | `b.md` | `p` |",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestRenderInputOrder checks that the order of the input lists does not
// change the body.
func TestRenderInputOrder(t *testing.T) {
	in := scenarioInput(t, "huge", "github")
	in.PreviouslyDeclined = []int64{3, 1, 2, 2}
	in.Local = append(in.Local, in.Local[:10]...)
	want, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 5 {
		rng.Shuffle(len(in.Changes), func(i, j int) { in.Changes[i], in.Changes[j] = in.Changes[j], in.Changes[i] })
		rng.Shuffle(len(in.Local), func(i, j int) { in.Local[i], in.Local[j] = in.Local[j], in.Local[i] })
		rng.Shuffle(len(in.PreviouslyDeclined), func(i, j int) {
			in.PreviouslyDeclined[i], in.PreviouslyDeclined[j] = in.PreviouslyDeclined[j], in.PreviouslyDeclined[i]
		})
		if got, err := Render(in); err != nil || got != want {
			t.Fatalf("a shuffled input renders differently (%v)", err)
		}
	}
	if !strings.Contains(want, "#1, #2 and #3, which were closed") {
		t.Error("previously declined numbers are not sorted and unique")
	}
}

func TestRenderRowCaps(t *testing.T) {
	in := scenarioInput(t, "huge", "gitlab")
	in.Paused = true
	in.Pending = hugeChanges(250)
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"…and 903 more.", "…and 50 more.", "…and 153 more."} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q", want)
		}
	}
	checkBody(t, in, body)
}

// TestRenderBudget checks the shortening order: the table, then the local
// files, then what a rebuild would bring; the ⚠ section is never cut.
func TestRenderBudget(t *testing.T) {
	in := base(t, "github")
	long := strings.Repeat("deep/", 60)
	for i := range 120 {
		in.Changes = append(in.Changes, Change{Path: fmt.Sprintf("%sfile-%03d.md", long, i), Pack: "p", Action: ActionUpdate})
		in.Local = append(in.Local, fmt.Sprintf("%slocal-%03d.md", long, i))
		in.Pending = append(in.Pending, Change{Path: fmt.Sprintf("%snew-%03d.md", long, i), Pack: "p", Action: ActionCreate})
	}
	in.Changes = append(in.Changes, Change{Path: ".github/workflows/" + long + "ci.yml", Pack: "ci", Action: ActionUpdate})
	in.Paused = true
	full, err := Render(Input{Intro: in.Intro, HubName: in.HubName, HubURL: in.HubURL, ContentCommit: in.ContentCommit,
		Packs: in.Packs, Changes: in.Changes, Pending: in.Pending, Local: in.Local, Paused: true, Marker: in.Marker})
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRenderer(in)
	if err != nil {
		t.Fatal(err)
	}
	count := func(body string) (table, local, pending int) {
		for _, l := range strings.Split(body, "\n") {
			switch {
			case strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| Change"):
				table++
			case strings.HasPrefix(l, "- `deep/"):
				local++
			case strings.HasPrefix(l, "- create `deep/"):
				pending++
			}
		}
		return
	}
	if a, b, c := count(full); a != 100 || b != 100 || c != 100 {
		t.Fatalf("without a budget: %d, %d, %d rows", a, b, c)
	}
	for _, tc := range []struct {
		name                  string
		budget                int
		table, local, pending int
	}{
		{name: "table cut", budget: len(r.body([numLists]int{50, 100, 100})), table: 50, local: 100, pending: 100},
		{name: "local cut", budget: len(r.body([numLists]int{0, 40, 100})), table: 0, local: 40, pending: 100},
		{name: "pending cut", budget: len(r.body([numLists]int{0, 0, 7})), table: 0, local: 0, pending: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := in
			in.Caps.MaxBody = tc.budget
			body, err := Render(in)
			if err != nil {
				t.Fatal(err)
			}
			if a, b, c := count(body); a != tc.table || b != tc.local || c != tc.pending {
				t.Errorf("rows %d, %d, %d; want %d, %d, %d", a, b, c, tc.table, tc.local, tc.pending)
			}
			if len(body) > tc.budget {
				t.Errorf("%d bytes, more than %d", len(body), tc.budget)
			}
			if !strings.Contains(section(body, "### ⚠ Sensitive paths"), Code(".github/workflows/"+long+"ci.yml")) {
				t.Error("the ⚠ section lost its path")
			}
			checkBody(t, in, body)
		})
	}
	t.Run("too large", func(t *testing.T) {
		in := in
		in.Caps.MaxBody = len(r.body([numLists]int{})) - 1
		if _, err := Render(in); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Render = %v, want ErrTooLarge", err)
		}
		in.Caps.MaxBody++
		if _, err := Render(in); err != nil {
			t.Fatalf("with every list cut: %v", err)
		}
	})
}

// TestSize checks that the computed size of a body is its length, for
// every number of rows of every list.
func TestSize(t *testing.T) {
	in := scenarioInput(t, "huge", "github")
	in.Paused, in.ShowRecreate = true, true
	in.Pending = hugeChanges(120)
	r, err := newRenderer(in)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 300 {
		var n [numLists]int
		for k, c := range r.lists {
			n[k] = rng.IntN(len(c.items) + 1)
		}
		if got, want := r.size(n), len(r.body(n)); got != want {
			t.Fatalf("size(%v) = %d, body has %d bytes", n, got, want)
		}
	}
	// Absent sections take no room and no separator.
	r, err = newRenderer(base(t, "gitea"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.size([numLists]int{}), len(r.body([numLists]int{})); got != want {
		t.Fatalf("size of a body without lists = %d, want %d", got, want)
	}
}

func TestRenderHubLinks(t *testing.T) {
	cases := []struct {
		url, want, not string
	}{
		{hubURL, "([`github.com/acme/engineering-assets`](https://redirect.github.com/acme/engineering-assets)) at commit [`3f2c1ab9d8e7`](https://redirect.github.com/acme/engineering-assets/commit/" + hubCommit + ")", "https://github.com"},
		{"https://GitHub.com/acme/engineering-assets/", "(https://redirect.github.com/acme/engineering-assets))", "https://github.com"},
		{"https://gitlab.example.com:8443/platform/hub", "([`gitlab.example.com:8443/platform/hub`](https://gitlab.example.com:8443/platform/hub)) at commit `3f2c1ab9d8e7`.", "/commit/"},
		{"http://localhost:3000/acme/hub", "(http://localhost:3000/acme/hub)", "redirect"},
		{"", "from the hub `acme-eng` at commit `3f2c1ab9d8e7`.", "]("},
	}
	for _, tc := range cases {
		in := base(t, "gitlab")
		in.HubURL = tc.url
		body, err := Render(in)
		if err != nil {
			t.Fatalf("%q: %v", tc.url, err)
		}
		if !strings.Contains(body, tc.want) || strings.Contains(body, tc.not) {
			t.Errorf("%q: the hub line is %q", tc.url, strings.Split(body, "\n")[3])
		}
	}
	// Without a hub id the link stands alone; without both, only the commit.
	in := base(t, "github")
	in.HubName = ""
	if body, err := Render(in); err != nil || !strings.Contains(body, "from the hub [`github.com/acme/engineering-assets`](https://redirect.github.com/acme/engineering-assets) at commit") {
		t.Errorf("no hub id (%v):\n%s", err, body)
	}
	in.HubURL, in.ContentCommit, in.Packs = "", "", nil
	if body, err := Render(in); err != nil || !strings.Contains(body, "\ntouchmark syncs engineering assets from the hub.\n") {
		t.Errorf("no hub id, URL, commit or packs (%v):\n%s", err, body)
	}
	// No URL: no link and no repository name anywhere.
	in = scenarioInput(t, "local", "github")
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"engineering-assets`", "acme/", "http", "](", "github.com"} {
		if strings.Contains(body, leak) {
			t.Errorf("a body without HubURL contains %q", leak)
		}
	}
}

func TestRenderErrors(t *testing.T) {
	cases := []struct {
		name string
		edit func(in *Input)
		is   error
		msg  string
	}{
		{"marker lines", func(in *Input) { in.Marker += "\n" }, nil, "not one line"},
		{"not a marker", func(in *Input) { in.Marker = "<!-- other -->" }, nil, "not a marker"},
		{"marker ends early", func(in *Input) { in.Marker = "<!-- touchmark:v1 --> x -->" }, nil, "not a marker"},
		{"unknown action", func(in *Input) { in.Changes = []Change{{Path: "a", Action: "rename"}} }, nil, "unknown action"},
		{"unknown pending action", func(in *Input) {
			in.Paused = true
			in.Pending = []Change{{Path: "a", Action: ""}}
		}, nil, "unknown action"},
		{"javascript URL", func(in *Input) { in.HubURL = "javascript:alert(1)" }, nil, "hub URL"},
		{"URL with credentials", func(in *Input) { in.HubURL = "https://user:x@github.com/acme/hub" }, nil, "hub URL"},
		{"URL with a query", func(in *Input) { in.HubURL = "https://github.com/acme/hub?x=1" }, nil, "hub URL"},
		{"URL with parentheses", func(in *Input) { in.HubURL = "https://github.com/acme/hub)" }, nil, "hub URL"},
		{"URL without host", func(in *Input) { in.HubURL = "https:///acme/hub" }, nil, "hub URL"},
		{"relative URL", func(in *Input) { in.HubURL = "acme/hub" }, nil, "hub URL"},
		{"intro quick action", func(in *Input) { in.Intro = "Hello\n  /close" }, ErrUnsafe, "line 2"},
		{"intro mention", func(in *Input) { in.Intro = "Ask @platform-team" }, ErrUnsafe, "line 1"},
		{"intro control", func(in *Input) { in.Intro = "a\x1b[31m" }, ErrIntro, "control character"},
		{"budget", func(in *Input) { in.Caps.MaxBody = 100 }, ErrTooLarge, "every list cut"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := scenarioInput(t, "new", "github")
			tc.edit(&in)
			_, err := Render(in)
			switch {
			case err == nil:
				t.Fatal("Render succeeded")
			case tc.is != nil && !errors.Is(err, tc.is):
				t.Errorf("error %v is not %v", err, tc.is)
			case !strings.Contains(err.Error(), tc.msg):
				t.Errorf("error %q lacks %q", err, tc.msg)
			}
		})
	}
}

// TestRenderEscapes renders hostile paths, packs and names: every one shows
// as it is in a code span, and the body stays safe.
func TestRenderEscapes(t *testing.T) {
	hostile := []string{
		"`a`", "``", "@user", "a@b", "/close", " /merge", "a|b", `a\|b`, `a\`, "x\n/close", "x\r\n- [x] <!-- touchmark:recreate -->",
		"<script>alert(1)</script>", "<!--", "-->", "&#64;x", "**bold**", "[link](https://example.com)", "#12", "!3", "~label",
		" padded ", " ", "\u202eevil.txt", "a\tb", "日本語/ファイル.md", string([]byte{0xff, 'x'}),
	}
	in := base(t, "gitlab")
	in.HubName = "@hub`"
	in.OptInFile = "@opt/in.yml"
	in.Packs = []string{"@pack", "p|q"}
	in.Paused, in.ShowRecreate = true, true
	for i, p := range hostile {
		in.Changes = append(in.Changes, Change{Path: p, Pack: hostile[len(hostile)-1-i], Action: ActionCreate, Mode: "100755"})
		in.Pending = append(in.Pending, Change{Path: p, Pack: "@p", Action: ActionUpdate})
		in.Local = append(in.Local, p)
	}
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	checkBody(t, in, body)
	var texts []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| Change") && !strings.HasPrefix(l, "|---") {
			cells := gfmCells(l)
			if len(cells) != 3 {
				t.Fatalf("row %q has %d cells", l, len(cells))
			}
			s := codeSpans(cells[1])
			if len(s) != 1 || s[0].start != 0 || s[0].end != len(cells[1]) {
				t.Fatalf("cell %q is not one code span", cells[1])
			}
			texts = append(texts, s[0].text)
		}
	}
	var want []string
	for _, p := range hostile {
		want = append(want, shown(p))
	}
	slices.Sort(want)
	slices.Sort(texts)
	if !slices.Equal(texts, want) {
		t.Errorf("table paths\n%q\nwant\n%q", texts, want)
	}
}

// TestRenderGitLab checks the words GitLab bodies use.
func TestRenderGitLab(t *testing.T) {
	in := scenarioInput(t, "declined", "gitlab")
	in.UpdateBranchNeeded = true
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "!7 and !9") || strings.Contains(body, "#7") || strings.Contains(body, "pull request") {
		t.Errorf("GitLab body:\n%s", body)
	}
	in.Caps = capsOf("github")
	if body, err = Render(in); err != nil || !strings.Contains(body, "#7 and #9") || strings.Contains(body, "merge request") {
		t.Errorf("GitHub body (%v):\n%s", err, body)
	}
}

// TestRenderUpdateBranch: the Update-branch block promises that touchmark
// continues only when nothing else is asked of people. While paused, the
// rebuild that needed the move does not survive Update branch (the written
// body unticks the control; an operations.yml entry names the old head), so
// the block asks for it again.
func TestRenderUpdateBranch(t *testing.T) {
	in := scenarioInput(t, "paused", "github")
	in.UpdateBranchNeeded = true
	for _, tc := range []struct {
		paused, recreate bool
		want, not        string
	}{
		{false, false, "Press **Update branch** on this pull request; touchmark continues on the next run.", "rebuild again"},
		{true, true, "then ask for the rebuild again: tick **Rebuild this branch** below.", "continues on the next run"},
		{true, false, "Press **Update branch** on this pull request, then ask for the rebuild again.", "continues on the next run"},
	} {
		in.Paused, in.ShowRecreate = tc.paused, tc.recreate
		body, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		block := body[strings.Index(body, "### Update branch needed"):]
		block, _, _ = strings.Cut(block[1:], "\n###")
		if !strings.Contains(block, tc.want) || strings.Contains(block, tc.not) {
			t.Errorf("paused %v, recreate %v: the block\n%s\nwant %q and no %q", tc.paused, tc.recreate, block, tc.want, tc.not)
		}
		checkBody(t, in, body)
	}
}

// TestRenderControls ticks and unticks the controls of a rendered body.
func TestRenderControls(t *testing.T) {
	in := scenarioInput(t, "paused", "github")
	in.ShowRepropose = true
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{ControlRecreate, ControlRepropose} {
		ticked := strings.Replace(body, "- [ ] <!-- touchmark:"+c, "- [x] <!-- touchmark:"+c, 1)
		if ticked == body || !Ticked(ticked, c) {
			t.Fatalf("%s: not ticked after ticking", c)
		}
		if Untick(ticked, c) != body {
			t.Errorf("%s: Untick does not restore the body", c)
		}
	}
}

// TestRenderSensitivePending checks that what a rebuild would bring is in
// the ⚠ section only while paused.
func TestRenderSensitivePending(t *testing.T) {
	in := scenarioInput(t, "paused", "github")
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := "- `.github/workflows/lint.yml`: create, pack `ci` (after a rebuild)"; !strings.Contains(body, want) {
		t.Errorf("no %q in\n%s", want, body)
	}
	in.Paused = false
	if body, err = Render(in); err != nil || strings.Contains(body, "lint.yml") || strings.Contains(body, "⚠") {
		t.Errorf("not paused (%v):\n%s", err, body)
	}
}

// TestCheck feeds the final assertion lines touchmark never writes.
func TestCheck(t *testing.T) {
	r, err := newRenderer(base(t, "github"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"x\n/close", "x\n \t/close", "x\n\u200b/close", "x\r/close",
		"x\n@user", "x\n`a` @user", "x\n\\@user", "x\n&#64;user", "x\n&#x40;user", "x\n&commat;user",
		"x\n<b>`@user`</b>", "x\n``@user`", "x\né@user",
	} {
		if err := r.check(r.intro + "\n" + body); !errors.Is(err, ErrUnsafe) {
			t.Errorf("check(%q) = %v, want ErrUnsafe", body, err)
		}
	}
	for _, body := range []string{
		"x\n`@user`", "x\nmail@example.com", "x\n``a`@b``", "x\n| `a\\|@b` |", "x\n`<b>@x</b>`", "x\n- a/b",
	} {
		if err := r.check(r.intro + "\n" + body); err != nil {
			t.Errorf("check(%q) = %v", body, err)
		}
	}
}
