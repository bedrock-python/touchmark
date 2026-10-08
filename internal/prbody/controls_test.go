package prbody

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestControlLine: the exported lines are Render's, and Ticked reads them.
func TestControlLine(t *testing.T) {
	for control, label := range map[string]string{
		ControlRecreate:  "Rebuild this branch (drops commits added by others)",
		ControlRepropose: "Propose this content again",
	} {
		line := ControlLine(control)
		if line != "- [ ] <!-- touchmark:"+control+" --> "+label {
			t.Errorf("ControlLine(%s) = %q", control, line)
		}
		ticked := strings.Replace(line, "[ ]", "[x]", 1)
		if Ticked(line, control) || !Ticked(ticked, control) || Untick(ticked, control) != line {
			t.Errorf("ControlLine(%s) does not tick and untick", control)
		}
	}
	if got := ControlLine("unknown"); got != "" {
		t.Errorf("ControlLine(unknown) = %q", got)
	}
	in := scenarioInput(t, "paused", "github")
	in.ShowRepropose = true
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{ControlRecreate, ControlRepropose} {
		if !strings.Contains(body, "\n"+ControlLine(c)+"\n") {
			t.Errorf("Render does not write ControlLine(%s)", c)
		}
	}
}

// TestAddControlAndReplaceMarker: an ack adds the repropose control to a
// body touchmark wrote long ago, and a new marker replaces the old one,
// without rendering the body again.
func TestAddControlAndReplaceMarker(t *testing.T) {
	old := testMarker(t)
	line := ControlLine(ControlRepropose)
	cases := []struct{ name, body, want string }{
		{"a body with its marker", "Human part.\n\n" + old, "Human part.\n\n" + line + "\n\n" + old},
		{"CRLF and trailing blanks", "Human part.\r\n\r\n" + old + " \r\n", "Human part.\n\n" + line + "\n\n" + old},
		{"no marker", "Human part.\n", "Human part.\n\n" + line},
		{"only the marker", old, line + "\n\n" + old},
		{"empty", "", line},
		{"the control already there", "Text\n\n- [x] <!-- touchmark:repropose --> Propose this content again\n\n" + old,
			"Text\n\n- [x] <!-- touchmark:repropose --> Propose this content again\n\n" + old},
		{"two markers keep their order", "Text\n" + old + "\n<!-- touchmark:v2 x -->", "Text\n\n" + line + "\n\n" + old + "\n<!-- touchmark:v2 x -->"},
	}
	for _, c := range cases {
		got := AddControl(c.body, ControlRepropose)
		if got != c.want {
			t.Errorf("%s: AddControl =\n%q\nwant\n%q", c.name, got, c.want)
		}
		if !strings.Contains(got, line) && !Ticked(got, ControlRepropose) {
			t.Errorf("%s: no repropose control in %q", c.name, got)
		}
	}
	if got := AddControl("Text", "unknown"); got != "Text" {
		t.Errorf("AddControl(unknown) = %q", got)
	}

	fresh := "<!-- touchmark:v1 fresh -->"
	for body, want := range map[string]string{
		"Human part.\n\n" + old:               "Human part.\n\n" + fresh,
		"Human part.\r\n" + old + "\r\n":      "Human part.\n\n" + fresh,
		"Human part.":                         "Human part.\n\n" + fresh,
		old + "\n" + old:                      fresh,
		"":                                    fresh,
		"a\n- [ ] " + old[:20] + "\n\n" + old: "a\n- [ ] " + old[:20] + "\n\n" + fresh,
	} {
		got := ReplaceMarker(body, fresh)
		if got != want {
			t.Errorf("ReplaceMarker(%q) = %q, want %q", body, got, want)
		}
		if marker.Strip(got) != marker.Strip(body) {
			t.Errorf("ReplaceMarker(%q) changed the human part", body)
		}
	}
}

func TestTicked(t *testing.T) {
	ticked := []string{
		"- [x] <!-- touchmark:recreate --> Rebuild this branch (drops commits added by others)",
		"- [X] <!-- touchmark:recreate -->",
		"* [x]<!--touchmark:recreate-->",
		"+ [x] <!--  touchmark:recreate  --> text",
		"   - [ x ] <!-- touchmark:recreate -->",
		"\t-\t[x]\t<!--\ttouchmark:recreate\t-->",
		"-[x]<!-- touchmark:recreate -->",
		"- [x] <!-- touchmark:recreate -->\r",
		"intro\r\n- [x] <!-- touchmark:recreate --> Rebuild\r\nmore",
		"- [ ] <!-- touchmark:recreate -->\n- [x] <!-- touchmark:recreate -->",
	}
	for _, body := range ticked {
		if !Ticked(body, ControlRecreate) {
			t.Errorf("Ticked(%q) = false", body)
		}
		if Ticked(body, ControlRepropose) {
			t.Errorf("Ticked(%q, repropose) = true", body)
		}
	}
	unticked := []string{
		"",
		"- [ ] <!-- touchmark:recreate --> Rebuild this branch (drops commits added by others)",
		"- [] <!-- touchmark:recreate -->",
		"- [x] Rebuild this branch (drops commits added by others)",
		"- [x] text <!-- touchmark:recreate -->",
		"[x] <!-- touchmark:recreate -->",
		"> - [x] <!-- touchmark:recreate -->",
		"1. [x] <!-- touchmark:recreate -->",
		"- [xx] <!-- touchmark:recreate -->",
		"- [v] <!-- touchmark:recreate -->",
		"- [x] <!-- touchmark:recreatex -->",
		"- [x] <!-- touchmark:recreate",
		"- [x] <!-- Touchmark:recreate -->",
		"- [x] <!-- touchmark: recreate -->",
		"- [x] <!-- touchmark:repropose -->",
		"- [x <!-- touchmark:recreate -->",
		"text - [x] <!-- touchmark:recreate -->",
		"<!-- touchmark:v1 hub=a fp=0 stream=sync key=k data=x -->",
	}
	for _, body := range unticked {
		if Ticked(body, ControlRecreate) {
			t.Errorf("Ticked(%q) = true", body)
		}
	}
	if !Ticked("- [x] <!-- touchmark:repropose --> Propose this content again", ControlRepropose) {
		t.Error("repropose is not ticked")
	}
}

func TestUntick(t *testing.T) {
	cases := []struct{ in, want string }{
		{"- [x] <!-- touchmark:recreate --> Rebuild", "- [ ] <!-- touchmark:recreate --> Rebuild"},
		{"- [ X ] <!-- touchmark:recreate -->", "- [ ] <!-- touchmark:recreate -->"},
		{"a\r\n- [X] <!-- touchmark:recreate -->\r\nb\r\n", "a\r\n- [ ] <!-- touchmark:recreate -->\r\nb\r\n"},
		{
			"- [x] <!-- touchmark:recreate -->\n- [x] <!-- touchmark:repropose -->\n* [x]<!--touchmark:recreate-->",
			"- [ ] <!-- touchmark:recreate -->\n- [x] <!-- touchmark:repropose -->\n* [ ]<!--touchmark:recreate-->",
		},
		{"- [ ] <!-- touchmark:recreate -->", "- [ ] <!-- touchmark:recreate -->"},
		{"- [x] text", "- [x] text"},
		{"", ""},
		{"\n", "\n"},
	}
	for _, tc := range cases {
		got := Untick(tc.in, ControlRecreate)
		if got != tc.want {
			t.Errorf("Untick(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if Ticked(got, ControlRecreate) {
			t.Errorf("Untick(%q) is still ticked", tc.in)
		}
	}
}

// TestNoBodyControls: where descriptions cannot carry tick boxes
// (Caps.BodyControls false: the marker is a reference definition), Render
// writes no control whatever ShowRecreate and ShowRepropose ask, names the
// recreate entry of operations.yml in the paused block and the
// forget_declines entry in the footnote. Where only closed pull requests
// are immutable, the controls stay and the footnote names the entry too;
// elsewhere it does not.
func TestNoBodyControls(t *testing.T) {
	const recreateHint = "add a `recreate` entry with this branch's head to `.touchmark/operations.yml`"
	const forgetHint = "add a `forget_declines` entry with the number of this pull request to `.touchmark/operations.yml`"
	for _, c := range []struct {
		name                       string
		marker                     platform.MarkerStore
		immutable                  bool
		controls, recreate, forget bool
	}{
		{"reference definition, immutable", platform.MarkerInRefDef, true, false, true, true},
		{"reference definition", platform.MarkerInRefDef, false, false, true, true},
		{"comment, immutable", platform.MarkerInBody, true, true, false, true},
		{"comment", platform.MarkerInBody, false, true, false, false},
	} {
		in := scenarioInput(t, "paused", "github")
		in.Caps.Marker, in.Caps.ClosedImmutable = c.marker, c.immutable
		in.ShowRepropose = true
		in.Marker = ""
		body, err := Render(in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, control := range []string{ControlRecreate, ControlRepropose} {
			if got := strings.Contains(body, ControlLine(control)); got != c.controls {
				t.Errorf("%s: control %s shown %v, want %v", c.name, control, got, c.controls)
			}
		}
		if strings.Contains(body, "<!--") == !c.controls {
			t.Errorf("%s: an HTML comment %v, want %v:\n%s", c.name, !c.controls, c.controls, body)
		}
		if got := strings.Contains(body, recreateHint); got != c.recreate {
			t.Errorf("%s: recreate hint %v, want %v", c.name, got, c.recreate)
		}
		if got := strings.Contains(body, forgetHint); got != c.forget {
			t.Errorf("%s: forget hint %v, want %v", c.name, got, c.forget)
		}
	}
}
