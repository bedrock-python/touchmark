package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
)

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustParseHub(t *testing.T, data []byte) *Hub {
	t.Helper()
	h, _, err := ParseHub(data)
	if err != nil {
		t.Fatalf("ParseHub: %v", err)
	}
	return h
}

func TestParseHubDefaults(t *testing.T) {
	h := mustParseHub(t, readFixture(t, "hub/valid/minimal.yml"))
	want := &Hub{
		Version: 1,
		ID:      "acme-eng",
		Branch:  "touchmark/acme-eng",
		Commit:  Commit{Message: "chore: sync engineering assets"},
		PR: PR{
			Title:   "chore: sync engineering assets",
			Labels:  []string{"engineering-assets"},
			LinkHub: "auto",
		},
		Limits:   Limits{MaxNewPRsPerRun: 100, MaxCloseFraction: 0.1},
		Memory:   Memory{AutoCloseCooldown: "30d"},
		Security: Security{WriteIsolation: "platform", PrivateTargetsInPublicHub: "skip"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("got  %+v\nwant %+v", h, want)
	}
	if h.OptInName() != DefaultOptIn {
		t.Errorf("OptInName = %q", h.OptInName())
	}
}

func TestParseHubLegacy(t *testing.T) {
	h, warns, err := ParseHub(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Legacy || h.Version != 1 || h.ID != "" || h.Branch != "" {
		t.Errorf("legacy hub = %+v", h)
	}
	if h.PR.LinkHub != "auto" || h.Limits.MaxNewPRsPerRun != 100 || h.Security.WriteIsolation != "platform" {
		t.Errorf("legacy hub lacks defaults: %+v", h)
	}
	if len(warns) != 1 || warns[0].File != HubFile {
		t.Errorf("warnings = %v", warns)
	}
	// An empty file is not a legacy hub: it lacks the id.
	if _, _, err := ParseHub([]byte{}); err == nil || !strings.Contains(err.Error(), "id: required") {
		t.Errorf("empty hub.yml: %v", err)
	}
}

func TestParseHubExplicitValues(t *testing.T) {
	h := mustParseHub(t, readFixture(t, "hub/valid/full.yml"))
	if h.Limits.MaxNewPRsPerRun != 0 || h.Limits.MaxCloseFraction != 1 {
		t.Errorf("limits = %+v, want the explicit 0 and 1", h.Limits)
	}
	if h.Memory.AutoCloseCooldown != "12h" || h.PR.LinkHub != "never" || !h.PR.Draft {
		t.Errorf("memory/pr = %+v %+v", h.Memory, h.PR)
	}
	if h.OptInName() != ".github/engineering-assets.yml" {
		t.Errorf("OptInName = %q", h.OptInName())
	}
	if !strings.HasPrefix(h.Commit.Message, "chore: sync engineering assets\n\nDelivered") {
		t.Errorf("commit message = %q", h.Commit.Message)
	}
	want := []Provider{
		{
			ID: "gh", Type: "github", URL: "https://github.com", Writer: "acme-touchmark[bot]",
			KnownAuthors:       []string{"old-app[bot]"},
			AutomationAccounts: []string{"dependabot[bot]", "stale[bot]"},
			Sign:               "always",
			Limits:             ProviderLimits{WritesPerMinute: 30, WritesPerHour: 300, Reads: 4},
		},
		{
			ID: "corp", Type: "gitlab", URL: "https://gitlab.example.com:8443/gitlab",
			APIURL: "https://gitlab.example.com:8443/gitlab/api/v4", CAFile: "certs/corp-ca.pem",
			Writer: "touchmark-writer", Sign: "auto",
		},
	}
	if !reflect.DeepEqual(h.Providers, want) {
		t.Errorf("providers:\n got  %+v\n want %+v", h.Providers, want)
	}
	aliases := h.Aliases()
	if !reflect.DeepEqual(aliases, map[string][]string{"agents": {"base"}, "python-service": {"python"}}) {
		t.Errorf("Aliases = %v", aliases)
	}
}

func TestParseHubShorthand(t *testing.T) {
	h, warns, err := ParseHub(readFixture(t, "hub/valid/shorthand-commit-author.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{{ID: "gitlab", Type: "gitlab", URL: "https://gitlab.example.com", Sign: "auto"}}
	if !reflect.DeepEqual(h.Providers, want) {
		t.Errorf("providers = %+v", h.Providers)
	}
	if h.Commit.Author != nil || len(warns) != 1 {
		t.Errorf("commit.author = %v, warnings %v", h.Commit.Author, warns)
	}
	if h.Branch != "chore/sync-engineering-assets" {
		t.Errorf("branch = %q", h.Branch)
	}

	h = mustParseHub(t, readFixture(t, "hub/valid/shorthand-gitea.yml"))
	want = []Provider{{ID: "gitea", Type: "gitea", URL: "https://git.example.com", Writer: "touchmark-bot", Sign: "always"}}
	if !reflect.DeepEqual(h.Providers, want) {
		t.Errorf("providers = %+v", h.Providers)
	}
}

func TestParseHubAnchors(t *testing.T) {
	h := mustParseHub(t, readFixture(t, "hub/valid/anchors.yml"))
	want := []Provider{
		{ID: "github", Type: "github", URL: "https://github.com", KnownAuthors: []string{"old-app[bot]", "older-app[bot]"}, Sign: "always"},
		{ID: "ghe", Type: "github", URL: "https://github.example.com", KnownAuthors: []string{"old-app[bot]", "older-app[bot]"}, Sign: "always"},
	}
	if !reflect.DeepEqual(h.Providers, want) {
		t.Errorf("providers:\n got  %+v\n want %+v", h.Providers, want)
	}
}

func TestParseHubEmptyLabels(t *testing.T) {
	h := mustParseHub(t, []byte("id: acme-eng\npr:\n  labels: []\n"))
	if h.PR.Labels == nil || len(h.PR.Labels) != 0 {
		t.Errorf("labels = %#v, want an explicit empty list", h.PR.Labels)
	}
	h = mustParseHub(t, []byte("id: acme-eng\npr:\n  labels:\n"))
	if !reflect.DeepEqual(h.PR.Labels, []string{"engineering-assets"}) {
		t.Errorf("labels = %#v, want the default", h.PR.Labels)
	}
}

func TestParseHubCollectsErrors(t *testing.T) {
	_, _, err := ParseHub([]byte("version: 2\nid: X\npr:\n  link_hub: maybe\n"))
	if err == nil {
		t.Fatal("want errors")
	}
	for _, s := range []string{"version: must be 1", `id: "X"`, `pr.link_hub: "maybe"`} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q lacks %q", err, s)
		}
	}
}

func TestParseRejectsSecrets(t *testing.T) {
	body := strings.Repeat("A1b2", 9)
	secrets := []struct{ value, kind string }{
		{"ghp_" + body, "ghp_"},
		{"github_pat_11" + body + "_" + body, "github_pat_"},
		{"ghs_" + body, "ghs_"},
		{"gho_" + body, "gho_"},
		{"glpat-" + body[:20], "glpat-"},
		{"-----" + "BEGIN OPENSSH PRIVATE KEY-----", "-----BEGIN"},
	}
	for _, s := range secrets {
		hub := fmt.Sprintf("version: 1\nid: acme-eng\npacks:\n  agents:\n    description: %q\n", "token "+s.value)
		_, _, err := ParseHub([]byte(hub))
		if err == nil || !strings.Contains(err.Error(), "line 5 looks like a secret ("+s.kind) {
			t.Errorf("hub.yml with %s: %v", s.kind, err)
		} else if strings.Contains(err.Error(), body[:12]) {
			t.Errorf("the error leaks the secret: %v", err)
		}
		targets := fmt.Sprintf("version: 1\n# %s\ntargets: []\n", s.value)
		if _, _, err := ParseTargets([]byte(targets)); err == nil || !strings.Contains(err.Error(), "line 2 looks like a secret") {
			t.Errorf("targets.yml with %s in a comment: %v", s.kind, err)
		}
	}
	// Prefixes alone, or inside longer words, are not secrets.
	for _, s := range []string{"ghp_ tokens are rejected", "xghp_" + body, "use a glpat- token", "BEGIN", "eyJ alone"} {
		hub := fmt.Sprintf("id: acme-eng\npacks:\n  agents:\n    description: %q\n", s)
		if _, _, err := ParseHub([]byte(hub)); err != nil {
			t.Errorf("description %q: %v", s, err)
		}
	}
}

// TestSecretsNeverPrinted: a secret is reported even where the file fails to
// decode or a check would quote it, and no message carries it.
func TestSecretsNeverPrinted(t *testing.T) {
	body := strings.Repeat("A1b2", 9)
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJwcm9qZWN0In0." + body
	for _, tc := range []struct {
		name, file, text, kind string
	}{
		{"unknown key", "hub", "version: 1\nid: acme-eng\ngithub_token: ghp_" + body + "\n", "ghp_"},
		{"type mismatch", "hub", "version: 1\nid: acme-eng\nlimits:\n  max_new_prs_per_run: ghp_" + body + "\n", "ghp_"},
		{"token as a key", "hub", "version: 1\nid: acme-eng\nghp_" + body + ": 1\n", "ghp_"},
		{"token as a pack name", "hub", "version: 1\nid: acme-eng\npacks:\n  glcbt-" + body + ": {}\n", "glcbt-"},
		{"syntax error", "hub", "version: 1\nid: [acme\ntoken: glsoat-" + body + "\n", "glsoat-"},
		{"exclude", "targets", "version: 1\nexclude: [ghp_" + body + "]\n", "ghp_"},
		{"jwt", "targets", "version: 1\ndefaults:\n  provider: " + jwt + "\n", "eyJ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.file == "hub" {
				_, _, err = ParseHub([]byte(tc.text))
			} else {
				_, _, err = ParseTargets([]byte(tc.text))
			}
			if err == nil || !strings.Contains(err.Error(), "looks like a secret ("+tc.kind) {
				t.Fatalf("error %v, want a secret finding", err)
			}
			if strings.Contains(err.Error(), body[:12]) || strings.Contains(err.Error(), "unknown key") {
				t.Errorf("the error quotes the file: %v", err)
			}
		})
	}
	for _, prefix := range []string{"gloas-", "glft-", "glimt-", "glagent-", "glffct-", "glrtr-"} {
		hub := "version: 1\nid: acme-eng\n# " + prefix + body + "\n"
		if _, _, err := ParseHub([]byte(hub)); err == nil || !strings.Contains(err.Error(), prefix) {
			t.Errorf("%s token: %v", prefix, err)
		}
	}
	// The opt-in file is not scanned, but its messages are redacted.
	_, _, err := ParseOptIn([]byte("packs: [ghp_" + body + "]\n"))
	if err == nil || strings.Contains(err.Error(), body[:12]) || !strings.Contains(err.Error(), "ghp_…") {
		t.Errorf("opt-in with a token: %v", err)
	}
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + body + "\n-----END OPENSSH PRIVATE KEY-----\n"
	_, _, err = ParseOptIn([]byte(fmt.Sprintf("ignore: [%q]\nversion: %q\n", "x", key)))
	if err == nil || strings.Contains(err.Error(), body[:12]) {
		t.Errorf("opt-in with a key: %v", err)
	}
}

// TestMessagesEscapeControlCharacters: values from the file cannot forge
// log lines, CI annotations or terminal escapes.
func TestMessagesEscapeControlCharacters(t *testing.T) {
	for _, text := range []string{
		"version: !!int \"\\n::error file=README.md,line=1::Hub compromised\\n\\e[2K\"\n",
		"version: \"\\n::error::x\"\n",
		"packs: [\"a\\u202eb\"]\n",
		"ignore: 5\npacks: [\"\\e[31mred\"]\n",
		"version: 1\n\"odd\\nkey\": 1\n",
		"version: [\"\\n::error::x\"]\n",
		"version: 1\n\x1b[2J: 1\n",
	} {
		_, warns, err := ParseOptIn([]byte(text))
		if err == nil {
			t.Errorf("%q: want an error", text)
			continue
		}
		msg := err.Error()
		for _, line := range strings.Split(msg, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "::") {
				t.Errorf("%q: a message line starts a CI annotation: %q", text, msg)
			}
		}
		if strings.ContainsAny(msg, "\x1b\u202e") {
			t.Errorf("%q: the message holds a raw escape: %q", text, msg)
		}
		for _, w := range warns {
			if strings.ContainsFunc(w.Message, unicode.IsControl) {
				t.Errorf("%q: warning %q holds a control character", text, w.Message)
			}
		}
	}
	// Hub map keys land in messages too.
	_, _, err := ParseHub([]byte("version: 1\nid: acme-eng\npacks:\n  \"x\\ny\": {requires: 5}\n"))
	if err == nil || strings.Contains(err.Error(), "x\ny") {
		t.Errorf("hub pack key with a newline: %v", err)
	}
}

func TestOptInIgnoreLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("ignore:\n")
	for i := range maxIgnorePatterns {
		fmt.Fprintf(&b, "  - docs/%d/**\n", i)
	}
	if _, _, err := ParseOptIn([]byte(b.String())); err != nil {
		t.Fatalf("%d patterns: %v", maxIgnorePatterns, err)
	}
	b.WriteString("  - one/more\n")
	if _, _, err := ParseOptIn([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "more than the 1000 allowed") {
		t.Errorf("%d patterns: %v", maxIgnorePatterns+1, err)
	}
	sch := compileSchema(t, "opt-in")
	if schemaAccepts(sch, []byte(b.String())) {
		t.Errorf("the schema accepts %d patterns", maxIgnorePatterns+1)
	}
}

// TestHashInQuotes: '#' inside a quoted scalar is content, not a comment
// (a hand-rolled parser would cut it off).
func TestHashInQuotes(t *testing.T) {
	o, _, err := ParseOptIn(readFixture(t, "opt-in/valid/hash-in-quotes.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docs/#drafts/**", "notes #1.md", "a#b"}; !slices.Equal(o.Ignore, want) {
		t.Errorf("ignore = %q, want %q", o.Ignore, want)
	}
	h, _, err := ParseHub(readFixture(t, "hub/valid/hash-in-quotes.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Commit.Message != "chore: sync #42" || h.PR.Title != "sync # engineering assets" {
		t.Errorf("commit.message %q, pr.title %q", h.Commit.Message, h.PR.Title)
	}
}

func TestParseTargets(t *testing.T) {
	tg, warns, err := ParseTargets(nil)
	if err != nil || len(warns) > 0 || tg.Version != 1 || len(tg.Targets) > 0 || tg.Legacy {
		t.Errorf("ParseTargets(nil) = %+v, %v, %v", tg, warns, err)
	}

	tg, warns, err = ParseTargets(readFixture(t, "targets/valid/legacy-repos.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Repo: "acme/billing"}, {Repo: "acme/platform/tools/engineering-assets"}}
	if !tg.Legacy || tg.Version != 1 || !reflect.DeepEqual(tg.Targets, want) || len(warns) != 1 {
		t.Errorf("legacy targets = %+v, warnings %v", tg, warns)
	}

	tg, _, err = ParseTargets(readFixture(t, "targets/valid/example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.Targets) != 3 || tg.Targets[1].Group != "acme/platform" || tg.Targets[2].Org != "acme" ||
		!reflect.DeepEqual(tg.Exclude, []string{"acme/legacy-monolith"}) || tg.Legacy {
		t.Errorf("targets = %+v", tg)
	}
}

func TestParseOptIn(t *testing.T) {
	tests := []struct {
		data   string
		legacy bool
		packs  []string
	}{
		{"", true, nil},
		{"  \n\t\n", true, nil},
		{"\xef\xbb\xbf", true, nil},
		{"# only a comment\n", true, nil},
		{"packs: [claude]\n", true, []string{"claude"}},
		{"version: 1\npacks: [claude]\n", false, []string{"claude"}},
		{"version: 1\r\npacks:\r\n  - claude\r\n", false, []string{"claude"}},
		{"\xef\xbb\xbfversion: 1\npacks: [claude]\n", false, []string{"claude"}},
		{"version: 1\n", false, nil},
	}
	for _, tt := range tests {
		o, warns, err := ParseOptIn([]byte(tt.data))
		if err != nil {
			t.Errorf("%q: %v", tt.data, err)
			continue
		}
		if o.Legacy != tt.legacy || o.Version != 1 || !reflect.DeepEqual(o.Packs, tt.packs) || len(warns) > 0 {
			t.Errorf("%q: %+v, warnings %v", tt.data, o, warns)
		}
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		in      string
		want    Ref
		wantErr string
	}{
		{in: "acme/billing", want: Ref{Path: "acme/billing"}},
		{in: "Acme/Billing", want: Ref{Path: "Acme/Billing"}},
		{in: "gh:acme/billing", want: Ref{Provider: "gh", Path: "acme/billing"}},
		{in: "corp-gl:group/sub/project", want: Ref{Provider: "corp-gl", Path: "group/sub/project"}},
		{in: "acme/.github", want: Ref{Path: "acme/.github"}},
		{in: "", wantErr: "empty path"},
		{in: "acme", wantErr: "owner and a name"},
		{in: "gh:", wantErr: "empty path"},
		{in: ":acme/x", wantErr: `provider id ""`},
		{in: "GH:acme/x", wantErr: `provider id "GH"`},
		{in: "gh:corp:acme/x", wantErr: "character ':'"},
		{in: "/acme/x", wantErr: "leading or trailing slash"},
		{in: "acme/x/", wantErr: "leading or trailing slash"},
		{in: "acme//x", wantErr: "empty path segment"},
		{in: "acme/../x", wantErr: `".." path segment`},
		{in: "acme/./x", wantErr: `"." path segment`},
		{in: "acme/my x", wantErr: "character ' '"},
		{in: "acme\\x", wantErr: `character '\\'`},
		{in: "acme/" + strings.Repeat("x", 600), wantErr: "longer than 512 bytes"},
	}
	for _, tt := range tests {
		got, err := ParseRef(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseRef(%q) = %v, %v; want error %q", tt.in, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
		if got.String() != tt.in {
			t.Errorf("Ref.String() = %q, want %q", got.String(), tt.in)
		}
	}
}

func TestCheckBranchName(t *testing.T) {
	for _, ok := range []string{"touchmark/acme-eng", "chore/sync-engineering-assets", "a", "feat/x.y", "v1@2"} {
		if err := checkBranchName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "@", "-x", "/x", "x/", "a//b", "a.", "a..b", "a@{b", "a b", "a~b", "a^b",
		"a:b", "a?b", "a*b", "a[b", `a\b`, "a\x7fb", ".a", "a/.b", "a.lock", "a/b.lock/c"} {
		if err := checkBranchName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecodeLimits(t *testing.T) {
	t.Run("size", func(t *testing.T) {
		data := []byte("packs: []\n# " + strings.Repeat("x", maxConfigSize) + "\n")
		if _, _, err := ParseOptIn(data); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("oversized: %v", err)
		}
	})
	t.Run("aliases", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("packs: [&p agents")
		for i := 0; i <= maxAliases; i++ {
			b.WriteString(", *p")
		}
		b.WriteString("]\n")
		if _, _, err := ParseOptIn([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "aliases") {
			t.Errorf("too many aliases: %v", err)
		}
	})
	t.Run("expansion", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("id: acme-eng\nx-authors: &a [")
		for i := 0; i < 2000; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("u")
		}
		b.WriteString("]\n")
		// The unknown key fails first; move the anchor into a real field.
		doc := strings.Replace(b.String(), "x-authors: &a", "providers:\n  - id: p0\n    type: github\n    known_authors: &a", 1)
		var sb strings.Builder
		sb.WriteString(doc)
		for i := 1; i < 90; i++ {
			fmt.Fprintf(&sb, "  - {id: p%d, type: github, known_authors: *a}\n", i)
		}
		if _, _, err := ParseHub([]byte(sb.String())); err == nil || !strings.Contains(err.Error(), "YAML nodes") {
			t.Errorf("alias expansion: %v", err)
		}
	})
	t.Run("types", func(t *testing.T) {
		tests := map[string]string{
			"version: '1'\n":           "expected an integer, got a string",
			"version: true\n":          "expected an integer, got a boolean (true)",
			"packs: {a: b}\n":          "packs: expected a list, got a mapping",
			"packs: [[a]]\n":           "packs[0]: expected a string, got a list",
			"packs: [on]\n":            "", // "on" is a string in YAML 1.2
			"ignore: [~]\n":            "ignore[0]: expected a string, got null",
			"packs: [!!binary YQ==]\n": "expected a string, got a !!binary value",
		}
		for data, want := range tests {
			_, _, err := ParseOptIn([]byte(data))
			if want == "" {
				if err != nil {
					t.Errorf("%q: %v", data, err)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%q: %v, want %q", data, err, want)
			}
		}
	})
}

// FuzzParse checks that no input makes the parsers or Select panic. The
// opt-in file is untrusted input.
func FuzzParse(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*", "*", "*.yml"))
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil {
			f.Add(data)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		h, _, _ := ParseHub(data)
		tg, _, _ := ParseTargets(data)
		o, _, err := ParseOptIn(data)
		if err == nil {
			known := map[string]bool{}
			for _, p := range o.Packs {
				known[p] = true
			}
			_, _, _ = Select(h, tg, o, Ref{Path: "acme/billing"}, known)
			_, _ = Check(h, tg, known)
		}
	})
}
