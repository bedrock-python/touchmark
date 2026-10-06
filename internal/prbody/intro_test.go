package prbody

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckIntro(t *testing.T) {
	good := []string{
		"",
		"\n\n  \n",
		intro,
		"Plain text.\r\nWith CRLF.\r\n",
		"Write to platform-team@acme.example for help.",
		"## Heading\n\n- a list\n- `code`\n\n```yaml\nkey: value\n```\n",
		"~~~\ncode\n~~~",
		"````\n```\nnested\n````",
		"<details>\n<summary>More</summary>\n\nText.\n\n</details>",
		"<!-- a comment -->\nText.",
		"A <code>tag</code> and a <pre>block</pre>.",
		"Tabs\tinside are fine.",
		"a/b paths and https://example.com/x/y links",
		"    indented ```not a fence",
		"x_@y and 1@2 are inside words",
		// HTML blocks that end within the intro.
		"<?php echo 1; ?>",
		"<!DOCTYPE html>",
		"<![CDATA[x]]>",
		"<!-->",
		"<!--->\ntext <!-- x -->",
		"<script>\nvar x;\n</script>",
		"<pre>\na\n\nb\n</pre>",
		"<PRE>\nx\n</Pre>",
		"<pre>x</pre>\n\ntext",
		"    <?not a block: indented code",
		"<prefix is not a pre tag>",
		"```\n<?\n```",
		"<details>\n<details>\n</details>\n</details>",
	}
	for _, s := range good {
		if err := CheckIntro(s); err != nil {
			t.Errorf("CheckIntro(%q) = %v", s, err)
		}
	}
	bad := []struct {
		intro string
		is    error
		msg   string
	}{
		{"/close", ErrUnsafe, "line 1"},
		{"Hello\n  /merge", ErrUnsafe, "line 2"},
		{"a\r/label ~x", ErrUnsafe, "line 2"},
		{"```\n/usr/bin/env\n```", ErrUnsafe, "line 2"},
		{"Ask @platform", ErrUnsafe, "mention"},
		{"@platform", ErrUnsafe, "mention"},
		{"`npm i @scope/pkg`", ErrUnsafe, "mention"},
		{"(@x)", ErrUnsafe, "mention"},
		{"a\\@x", ErrUnsafe, "mention"},
		{"&#64;x", ErrUnsafe, "mention"},
		{"x &#x40;y", ErrUnsafe, "mention"},
		{"&commat;y", ErrUnsafe, "mention"},
		{"é@x", ErrUnsafe, "mention"},
		{"- [x] <!-- touchmark:recreate --> Rebuild", ErrIntro, "reserved"},
		{"<!--touchmark:v1 -->", ErrIntro, "reserved"},
		{"<!--  TouchMark:x -->", ErrIntro, "reserved"},
		{"text\n<!--\n touchmark:recreate -->", ErrIntro, "line 2"},
		{"```\ncode", ErrIntro, "never closed"},
		{"~~~~\ncode\n~~~", ErrIntro, "never closed"},
		{"text\n<!-- open", ErrIntro, "line 2"},
		{"<!-->\n<!-- open", ErrIntro, "line 2"},
		{"<details>\n<summary>x</summary>\n\nText.", ErrIntro, "<details>"},
		{"<pre>\ntext", ErrIntro, "<pre>"},
		{"<TEXTAREA>", ErrIntro, "<textarea>"},
		{"</div>", ErrIntro, "<div>"},
		{"a\x00b", ErrIntro, "control character"},
		{"a\x1b[31m", ErrIntro, "control character"},
		{"a\xc2\x85b", ErrIntro, "control character"},
		{"a @ b", ErrUnsafe, "mention"},
		{string([]byte{'a', 0xff}), ErrIntro, "UTF-8"},
		// CommonMark HTML blocks of types 1 to 5 run until their end
		// condition, across blank lines: never ended, they swallow the ⚠
		// section and everything after it.
		{"Welcome.\n\n<?", ErrIntro, "<? HTML block of line 3 never ends"},
		{"Welcome.\n\n<!DOCTYPE html", ErrIntro, "never ends"},
		{"Welcome.\n\n<![CDATA[", ErrIntro, "<![CDATA[ HTML block"},
		{"Welcome.\n\n  <?php", ErrIntro, "never ends"},
		{"Welcome.\n</script>\n\n<script>", ErrIntro, "<script> HTML block of line 4"},
		{"Welcome.\n</style>\n\n<style>", ErrIntro, "<style> HTML block"},
		{"Welcome.\n</textarea>\n\n<textarea>", ErrIntro, "<textarea> HTML block"},
		{"<SCRIPT type=x>\nx", ErrIntro, "never ends"},
		{"<pre>\n</script>", ErrIntro, "<pre> is opened and never closed"},
		// Order counts: an element closed before it opens stays open.
		{"Welcome.\n</details>\n\n<details>", ErrIntro, "</details> on line 2 closes a <details> that is not open"},
		{"Welcome.\n</div>\n\n<div>", ErrIntro, "closes a <div>"},
	}
	for _, tc := range bad {
		err := CheckIntro(tc.intro)
		switch {
		case err == nil:
			t.Errorf("CheckIntro(%q) = nil", tc.intro)
		case !errors.Is(err, ErrIntro) || !errors.Is(err, tc.is):
			t.Errorf("CheckIntro(%q) = %v, want %v", tc.intro, err, tc.is)
		case !strings.Contains(err.Error(), tc.msg):
			t.Errorf("CheckIntro(%q) = %q, want %q in it", tc.intro, err, tc.msg)
		}
	}
}

func TestPrepareIntro(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"\n \n\t\n", ""},
		{"\n\nText\r\nmore  \r\n\r\n", "Text\nmore"},
		{"  indented\n", "  indented"},
		{"a\n\n\nb", "a\n\n\nb"},
	}
	for _, tc := range cases {
		got, err := prepareIntro(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("prepareIntro(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	// Render writes the prepared intro first.
	in := base(t, "github")
	in.Intro = "\r\n\r\nHello.\r\n"
	body, err := Render(in)
	if err != nil || !strings.HasPrefix(body, "Hello.\n\ntouchmark syncs") {
		t.Errorf("Render with a CRLF intro (%v):\n%.80q", err, body)
	}
	in.Intro = ""
	if body, err = Render(in); err != nil || !strings.HasPrefix(body, "touchmark syncs") {
		t.Errorf("Render without an intro (%v):\n%.80q", err, body)
	}
}
