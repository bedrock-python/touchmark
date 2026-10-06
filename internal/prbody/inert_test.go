package prbody

import (
	"errors"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestInert: people's text touchmark writes back runs no quick action and
// mentions no one, whatever it held, and the check agrees.
func TestInert(t *testing.T) {
	wj := string(wordJoiner)
	for _, tc := range []struct{ in, want string }{
		{"plain text\nand more", "plain text\nand more"},
		{"/label ~smuggled", `\/label ~smuggled`},
		{"<!-- touchmark:note -->\n/label ~smuggled\n</p>", "<!-- touchmark:note -->\n" + `\/label ~smuggled` + "\n</p>"},
		{"  /merge", `  \/merge`},
		{"\u200b/close", "\u200b" + `\/close`},
		{"a line\r/close\r\nnext", "a line\n" + `\/close` + "\nnext"},
		{"cc @alice and @bob", "cc @" + wj + "alice and @" + wj + "bob"},
		{`escaped \@carol`, `escaped \@` + wj + "carol"},
		{"entity &#64;dave here", "entity @" + wj + "dave here"},
		{"mail me: someone@example.com", "mail me: someone@example.com"},
		{"code `@not-a-mention` stays", "code `@not-a-mention` stays"},
		{"a path/with/slashes is fine", "a path/with/slashes is fine"},
	} {
		got := Inert(tc.in)
		if got != tc.want {
			t.Errorf("Inert(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if err := CheckText(got); err != nil {
			t.Errorf("CheckText(Inert(%q)): %v", tc.in, err)
		}
		if again := Inert(got); again != got {
			t.Errorf("Inert is not idempotent on %q: %q", got, again)
		}
	}
}

// TestCheckText: what CheckText refuses, and that a body Render wrote
// passes it.
func TestCheckText(t *testing.T) {
	for _, bad := range []string{"ok\n/close", "  /merge", "hi @alice", `\@bob`, "&#64;carol", "a\rb"} {
		if err := CheckText(bad); !errors.Is(err, ErrUnsafe) {
			t.Errorf("CheckText(%q) = %v, want ErrUnsafe", bad, err)
		}
	}
	body, err := Render(Input{
		Intro:   "Maintained by platform-team@example.com.",
		HubName: "acme-eng", Packs: []string{"base"}, ContentCommit: strings.Repeat("a", 40),
		Changes:      []Change{{Path: "docs/@types/x.md", Pack: "base", Action: ActionCreate, Mode: "100644"}},
		Local:        []string{"@scope/pkg.json"},
		Paused:       true,
		Pending:      []Change{{Path: "/odd", Action: ActionUpdate, Mode: "100644"}},
		ShowRecreate: true,
		Caps:         platform.Caps{Flavor: "gitlab", QuickActions: true, MaxBody: 200000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckText(body); err != nil {
		t.Errorf("a rendered body fails CheckText: %v\n%s", err, body)
	}
	if Inert(body) != body {
		t.Errorf("Inert changes a rendered body:\n%s", body)
	}
	for _, c := range []string{ClosedComment("no-diff", "", ""), ClosedComment("opted-out", CauseDisabled, ".engineering-assets.yml"), DeclinedComment(3, []string{"@team/notes.md", "a/b.md"}, ".engineering-assets.yml"),
		AutoDeclinedComment(4, []string{"@x.md"}, "", []string{"engineering-assets"})} {
		if err := CheckText(c); err != nil {
			t.Errorf("a comment fails CheckText: %v\n%s", err, c)
		}
	}
}

// TestPausedUnknownBranch: a paused branch whose content touchmark cannot
// tell (no commit of its own on it) gets no claim about what it holds, and
// the whole of what a rebuild brings.
func TestPausedUnknownBranch(t *testing.T) {
	in := Input{
		HubName: "acme-eng", Packs: []string{"base"},
		Pending:       []Change{{Path: "AGENTS.md", Pack: "base", Action: ActionUpdate, Mode: "100644"}, {Path: "docs/guide.md", Pack: "base", Action: ActionCreate, Mode: "100644"}},
		Paused:        true,
		BranchUnknown: true,
		ShowRecreate:  true,
	}
	body, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cannot tell which of its changes the branch holds now", "A rebuild from the default branch would bring:",
		"update `AGENTS.md` (pack `base`)", "create `docs/guide.md` (pack `base`)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lacks %q:\n%s", want, body)
		}
	}
	for _, not := range []string{"The changes above are what the branch holds now", "would bring nothing new", "### Changes"} {
		if strings.Contains(body, not) {
			t.Errorf("the body claims %q:\n%s", not, body)
		}
	}
}
