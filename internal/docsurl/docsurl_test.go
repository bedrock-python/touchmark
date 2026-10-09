package docsurl

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// headingRe finds the Markdown headings of a page.
var headingRe = regexp.MustCompile(`(?m)^#{1,6} +(.+?) *$`)

// slug makes a heading's anchor the way the site does: lower case, words
// joined by hyphens, punctuation dropped.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// TestPagesExist checks that every URL of the package is a page of docs/,
// and its anchor a heading of that page.
func TestPagesExist(t *testing.T) {
	docs := filepath.Join("..", "..", "docs")
	for _, url := range []string{WriteIsolation, Migrate, GiteaForgejo, GettingStarted("github"), GettingStarted("gitlab"), GettingStarted("bitbucket")} {
		rest, ok := strings.CutPrefix(url, Base)
		if !ok {
			t.Errorf("%s is not under %s", url, Base)
			continue
		}
		page, anchor, _ := strings.Cut(rest, "#")
		if !strings.HasSuffix(page, "/") {
			t.Errorf("%s: a page URL ends in a slash", url)
			continue
		}
		page = strings.TrimSuffix(page, "/")
		data, err := os.ReadFile(filepath.Join(docs, filepath.FromSlash(page)+".md"))
		if os.IsNotExist(err) {
			data, err = os.ReadFile(filepath.Join(docs, filepath.FromSlash(page), "index.md"))
		}
		if err != nil {
			t.Errorf("%s: no page: %v", url, err)
			continue
		}
		if anchor == "" {
			continue
		}
		found := false
		for _, m := range headingRe.FindAllStringSubmatch(string(data), -1) {
			if slug(m[1]) == anchor {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the page has no heading with the anchor #%s", url, anchor)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"The write key stays on the default branch": "the-write-key-stays-on-the-default-branch",
		"Three accounts":                "three-accounts",
		"1. Generate the configuration": "1-generate-the-configuration",
		"Don't `run` it":                "dont-run-it",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
