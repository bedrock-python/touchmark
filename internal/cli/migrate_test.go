package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// The tests of migrate replace migrateNow and the driver hooks: they must
// not run in parallel.

// migrateDay is the clock of the tests: adopt_unmarked runs 30 days on.
var migrateDay = time.Date(2026, 9, 27, 15, 4, 5, 0, time.UTC)

// The multi-gitter file of the tests, as a hub's could look: every key
// migrate reads, a token and keys it ignores.
const multiGitterConfig = `# multi-gitter run --config multi-gitter.yml ./sync.sh
Platform: gitlab
base-url: https://gitlab.example.com/api/v4/
group:
  - acme/platform
  - platform,infra
project: acme/tools/cli
topic: [python, go]
include-subgroups: true
branch: chore/sync-engineering-assets
pr-title: "chore: sync engineering assets"
pr-body: Synced by the engineering-assets hub.
commit-message: "chore: sync engineering assets"
author-name: Engineering assets bot
author-email: group_42_bot_0123456789abcdef0123456789abcdef@noreply.gitlab.example.com
skip-repo: [platform/legacy]
labels: engineering-assets
token: glpat-Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5
reviewers: [someone]
conflict-strategy: replace
`

// migrateFile writes a multi-gitter file and returns its path.
func migrateFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "multi-gitter.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixedClock sets migrateNow for the test.
func fixedClock(t *testing.T) {
	t.Helper()
	old := migrateNow
	migrateNow = func() time.Time { return migrateDay }
	t.Cleanup(func() { migrateNow = old })
}

// migrated is what migrate printed, parsed back.
type migrated struct {
	hubYML, targetsYML, opsYML string
	hub                        *config.Hub
	targets                    *config.Targets
	ops                        *config.Operations
}

// parseMigrated splits the output at its "# ==> name <==" lines and runs
// every file through the config parsers and the cross-file checks, as
// touchmark check would.
func parseMigrated(t *testing.T, out string) migrated {
	t.Helper()
	parts := map[string]string{}
	name := ""
	for _, line := range strings.SplitAfter(out, "\n") {
		if n, ok := strings.CutPrefix(strings.TrimSpace(line), "# ==> "); ok {
			name = strings.TrimSuffix(n, " <==")
			continue
		}
		if name != "" {
			parts[name] += line
		}
	}
	var m migrated
	m.hubYML, m.targetsYML, m.opsYML = parts[config.HubFile], parts[config.TargetsFile], parts[config.OperationsFile]
	var err error
	var warns []config.Warning
	if m.hub, warns, err = config.ParseHub([]byte(m.hubYML)); err != nil || len(warns) > 0 {
		t.Fatalf("hub.yml: %v %v\n%s", err, warns, m.hubYML)
	}
	if m.targets, warns, err = config.ParseTargets([]byte(m.targetsYML)); err != nil || len(warns) > 0 {
		t.Fatalf("targets.yml: %v %v\n%s", err, warns, m.targetsYML)
	}
	if m.ops, warns, err = config.ParseOperations([]byte(m.opsYML)); err != nil || len(warns) > 0 {
		t.Fatalf("operations.yml: %v %v\n%s", err, warns, m.opsYML)
	}
	if warns, errs := config.CheckOperations(m.ops, m.targets, m.hub, migrateDay); len(errs) > 0 || len(warns) > 0 {
		t.Errorf("CheckOperations: %v %v", errs, warns)
	}
	if _, err := m.hub.ResolveProviders(func(string) string { return "" }); err != nil {
		t.Errorf("ResolveProviders: %v", err)
	}
	return m
}

// checkMigrated runs config.Check over a migration with a real id.
func checkMigrated(t *testing.T, m migrated) {
	t.Helper()
	warns, errs := config.Check(m.hub, m.targets, map[string]bool{})
	if len(errs) > 0 || len(warns) > 0 {
		t.Errorf("Check: %v %v", errs, warns)
	}
}

// noGitLabDriver makes the build look as if it had no gitlab driver.
func noGitLabDriver(t *testing.T) {
	t.Helper()
	drivers := planDrivers
	t.Cleanup(func() { planDrivers = drivers })
	planDrivers = map[string]planDriver{}
}

// A multi-gitter hub's file becomes a hub that passes check, with the
// alias, the bot from author-email (unchecked without a read credential),
// the targets and 30 days of adopt_unmarked; the output is the golden file,
// twice the same, and never shows the token.
func TestMigrateMultiGitter(t *testing.T) {
	fixedClock(t)
	noGitLabDriver(t)
	file := migrateFile(t, multiGitterConfig)
	res := runWith(t, nil, "migrate", "--from-multi-gitter", file, "--id", "acme-eng")
	if res.code != exitOK || res.stderr != "" {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if again := runWith(t, nil, "migrate", "--from-multi-gitter", file, "--id", "acme-eng"); again.stdout != res.stdout {
		t.Errorf("a second run printed something else:\n%s", again.stdout)
	}
	if strings.Contains(res.stdout, "glpat-") || strings.Contains(res.stdout, "Zm9vYmFy") {
		t.Errorf("the output shows the token:\n%s", res.stdout)
	}
	newScenario(t, "migrate", nil, nil).golden("multi-gitter.txt", res.stdout)
	m := parseMigrated(t, res.stdout)
	checkMigrated(t, m)

	h := m.hub
	if h.ID != "acme-eng" || h.Branch != "touchmark/acme-eng" || !slices.Equal(h.BranchAliases, []string{"chore/sync-engineering-assets"}) {
		t.Errorf("hub: id %q, branch %q, aliases %q", h.ID, h.Branch, h.BranchAliases)
	}
	if len(h.Providers) != 1 {
		t.Fatalf("providers %+v", h.Providers)
	}
	p := h.Providers[0]
	if p.ID != "gitlab" || p.Type != "gitlab" || p.URL != "https://gitlab.example.com" || p.Writer != "" ||
		!slices.Equal(p.KnownAuthors, []string{"group_42_bot_0123456789abcdef0123456789abcdef"}) {
		t.Errorf("provider %+v", p)
	}
	if h.PR.Title != "chore: sync engineering assets" || !slices.Equal(h.PR.Labels, []string{"engineering-assets"}) ||
		h.Commit.Message != "chore: sync engineering assets" || h.Commit.Author != nil {
		t.Errorf("pr %+v, commit %+v", h.PR, h.Commit)
	}
	for _, want := range []string{"TODO: not checked", "TODO: writer:", "pr.intro_file"} {
		if !strings.Contains(m.hubYML, want) {
			t.Errorf("hub.yml lacks %q", want)
		}
	}

	type entry struct{ group, repo, topic string }
	var got []entry
	for _, e := range m.targets.Targets {
		if e.Subgroups != nil || (e.Group != "" && !e.Forks) || len(e.Topics) > 1 || len(e.Packs) > 0 {
			t.Errorf("entry %+v", e)
		}
		topic := ""
		if len(e.Topics) == 1 {
			topic = e.Topics[0]
		}
		got = append(got, entry{e.Group, e.Repo, topic})
	}
	want := []entry{
		{"acme/platform", "", "python"}, {"acme/platform", "", "go"},
		{"platform", "", "python"}, {"platform", "", "go"},
		{"infra", "", "python"}, {"infra", "", "go"},
		{"", "acme/tools/cli", ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("targets %+v\nwant %+v", got, want)
	}
	if !slices.Equal(m.targets.Exclude, []string{"platform/legacy"}) {
		t.Errorf("exclude %q", m.targets.Exclude)
	}

	a := m.ops.AdoptUnmarked
	if a == nil || a.Until != "2026-10-27" || !a.Active(migrateDay) || !a.Active(migrateDay.AddDate(0, 0, 30)) || a.Active(migrateDay.AddDate(0, 0, 31)) {
		t.Errorf("adopt_unmarked %+v", a)
	}
	if len(m.ops.Recreate)+len(m.ops.ForgetDeclines) > 0 || m.ops.AllowMassClose != nil {
		t.Errorf("operations %+v", m.ops)
	}
}

// TestMigrateDefaults: multi-gitter's defaults carry over: gitlab.com, the
// branch multi-gitter-branch, no subgroups, forks included. Without --id
// the placeholder parses and check rejects it, as the template's does; a
// file without platform that names groups is GitLab's.
func TestMigrateDefaults(t *testing.T) {
	fixedClock(t)
	noGitLabDriver(t)
	res := runWith(t, nil, "migrate", "--from-multi-gitter", migrateFile(t, "group: acme\nskip-forks: true\n"))
	if res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	m := parseMigrated(t, res.stdout)
	h := m.hub
	if h.ID != config.PlaceholderID || !slices.Equal(h.BranchAliases, []string{"multi-gitter-branch"}) {
		t.Errorf("hub: id %q, aliases %q", h.ID, h.BranchAliases)
	}
	if p := h.Providers[0]; p.ID != "gitlab" || p.URL != "https://gitlab.com" || len(p.KnownAuthors) != 0 {
		t.Errorf("provider %+v", p)
	}
	if h.PR.Title != "chore: sync engineering assets" || !slices.Equal(h.PR.Labels, []string{"engineering-assets"}) {
		t.Errorf("pr %+v: want touchmark's defaults", h.PR)
	}
	e := m.targets.Targets
	if len(e) != 1 || e[0].Group != "acme" || e[0].Subgroups == nil || *e[0].Subgroups || e[0].Forks {
		t.Errorf("targets %+v", e)
	}
	for _, want := range []string{"TODO: name the hub", "TODO: known_authors", "platform was not set"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.stdout)
		}
	}
	_, errs := config.Check(m.hub, m.targets, map[string]bool{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), config.PlaceholderID) {
		t.Errorf("Check: %v, want only the placeholder id", errs)
	}

	// The sync branch itself needs no alias; a GitLab under a path keeps
	// it; a host whose first label is no provider id names it gitlab.
	res = runWith(t, nil, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, "platform: GitLab\nbase-url: https://10.0.0.7/gitlab/\nbranch: touchmark/acme-eng\nproject: [a/b, A/B]\n"))
	if res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	m = parseMigrated(t, res.stdout)
	checkMigrated(t, m)
	if p := m.hub.Providers[0]; len(m.hub.BranchAliases) != 0 || p.ID != "gitlab" || p.URL != "https://10.0.0.7/gitlab" {
		t.Errorf("aliases %q, provider %+v", m.hub.BranchAliases, p)
	}
	if len(m.targets.Targets) != 1 || m.targets.Targets[0].Repo != "a/b" {
		t.Errorf("targets %+v: want a/b once", m.targets.Targets)
	}
	res = runWith(t, nil, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, "platform: gitlab\nbase-url: https://code.corp.example:8443\nproject: a/b\n"))
	if m := parseMigrated(t, res.stdout); m.hub.Providers[0].ID != "code" || m.hub.Providers[0].URL != "https://code.corp.example:8443" {
		t.Errorf("provider %+v", m.hub.Providers[0])
	}
}

// TestMigrateNotes: what touchmark cannot express is told as a comment,
// and what it rejects is left out, with the files still valid.
func TestMigrateNotes(t *testing.T) {
	fixedClock(t)
	noGitLabDriver(t)
	cfg := `platform: gitlab
group: acme
user: jdoe
repo: acme/github-only
repo-include: "^acme/svc-"
repo-exclude: "-old$"
base-branch: develop
fork: true
skip-pr: false
push-only: "true"
commit-message: "Draft: sync"
labels: ["ok", "` + strings.Repeat("x", 51) + `"]
draft: true
`
	res := runWith(t, nil, "migrate", "--id", "acme-eng", "--from-multi-gitter", migrateFile(t, cfg))
	if res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	m := parseMigrated(t, res.stdout)
	checkMigrated(t, m)
	if m.hub.Commit.Message != "chore: sync engineering assets" || m.hub.PR.Title != "sync" || !m.hub.PR.Draft ||
		!slices.Equal(m.hub.PR.Labels, []string{"ok"}) {
		t.Errorf("commit %+v, pr %+v", m.hub.Commit, m.hub.PR)
	}
	for _, want := range []string{
		`user "jdoe"`, "left out: repo", `repo-include "^acme/svc-"`, `repo-exclude "-old$"`, `base-branch "develop"`,
		"pushed to forks", "skip-pr and push-only", "commit-message is left out", "labels have at most 50 characters",
		"draft prefix (Draft: or WIP:) is dropped",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.stdout)
		}
	}

	// A pr-title with a draft prefix: multi-gitter opened drafts, and the
	// title check and plan would pass a prefixed title that every CreatePR
	// then refuses.
	res = runWith(t, nil, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, "platform: gitlab\ngroup: acme\npr-title: \"[Draft] WIP: chore: sync\"\n"))
	m = parseMigrated(t, res.stdout)
	checkMigrated(t, m)
	if res.code != exitOK || m.hub.PR.Title != "chore: sync" || !m.hub.PR.Draft {
		t.Errorf("exit %d, pr %+v", res.code, m.hub.PR)
	}
}

// TestMigrateErrors: a file migrate cannot read, or that is not a GitLab
// multi-gitter config, is a configuration error (exit 2) whose message
// never quotes the file's token or the URL's password.
func TestMigrateErrors(t *testing.T) {
	fixedClock(t)
	noGitLabDriver(t)
	secret := "glpat-Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5"
	for _, tc := range []struct {
		name, content string
		args          []string
		stderr        string
	}{
		{name: "no file", args: []string{}, stderr: "missing --from-multi-gitter FILE"},
		{name: "missing", args: []string{"--from-multi-gitter", filepath.Join(t.TempDir(), "nope.yml")}, stderr: "--from-multi-gitter:"},
		{name: "github", content: "platform: github\norg: acme\n", stderr: `platform: "github"; migrate reads GitLab configurations only`},
		{name: "github by default", content: "org: acme\n", stderr: "platform: not set, so multi-gitter used github"},
		{name: "nothing", content: "platform: gitlab\n", stderr: "no group, project or user"},
		{name: "empty", content: "", stderr: "platform: not set"},
		{name: "http", content: "platform: gitlab\nbase-url: http://gitlab.example.com\ngroup: a\n", stderr: "https only"},
		{name: "password", content: "platform: gitlab\nbase-url: https://root:" + secret + "@gitlab.example.com\ngroup: a\n", stderr: "holds credentials"},
		{name: "bad yaml", content: "platform: gitlab\ntoken: " + secret + "\n\tgroup: [a\n", stderr: "not valid YAML (line"},
		{name: "not a mapping", content: "- gitlab\n", stderr: "not a YAML mapping"},
		{name: "twice", content: "platform: gitlab\ngroup: a\nGroup: b\n", stderr: "line 3: group is set twice"},
		{name: "bool", content: "platform: gitlab\ngroup: a\ninclude-subgroups: sometimes\n", stderr: `include-subgroups: "sometimes" is not true or false`},
		{name: "map value", content: "platform: gitlab\ngroup: {a: b}\n", stderr: "group: must be a value or a list"},
		{name: "bad group", content: "platform: gitlab\ngroup: a//b\n", stderr: `group: "a//b" is not a GitLab path`},
		{name: "one-segment project", content: "platform: gitlab\nproject: cli\n", stderr: `project: "cli" is not a GitLab path`},
		{name: "prefixed project", content: "platform: gitlab\nproject: gl:a/b\n", stderr: `project: "gl:a/b" is not a GitLab path`},
		{name: "branch", content: "platform: gitlab\ngroup: a\nbranch: a..b\n", stderr: `branch: "a..b" is not a branch name`},
		{name: "id", content: "platform: gitlab\ngroup: a\n", args: []string{"--id", "Acme_Eng"}, stderr: "--id:"},
		{name: "bot", content: "platform: gitlab\ngroup: a\n", args: []string{"--bot", " "}, stderr: "flag -bot: empty value"},
		{name: "writer", content: "platform: gitlab\ngroup: a\n", args: []string{"--writer", "a b"}, stderr: `--writer: "a b" is not an account name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"migrate"}
			if tc.args == nil || tc.content != "" {
				args = append(args, "--from-multi-gitter", migrateFile(t, tc.content))
			}
			args = append(args, tc.args...)
			res := runWith(t, nil, args...)
			if res.code != exitUsage || !strings.Contains(res.stderr, tc.stderr) || res.stdout != "" {
				t.Errorf("exit %d\nstdout: %s\nstderr: %s\nwant exit 2, stderr with %q", res.code, res.stdout, res.stderr, tc.stderr)
			}
			if strings.Contains(res.stderr, "Zm9vYmFy") {
				t.Errorf("stderr shows the secret: %s", res.stderr)
			}
		})
	}
}

// migrateWorld is a fake GitLab with a multi-gitter hub's merge requests.
type migrateWorld struct {
	p                   *fake.Platform
	reader, bot, person platform.Account
	old                 platform.Account
	built               []auth.Credential
}

const migrateReadToken = "reader-token-4f2a9c"

// newMigrateWorld builds gitlab.example.com: the bot multi-gitter ran as opened
// merge requests on the alias in two projects of acme (one in a
// subgroup), a person one, an older bot's are closed, and merge requests on
// other branches or from forks do not count.
func newMigrateWorld(t *testing.T) *migrateWorld {
	t.Helper()
	p := fake.New("gitlab.example.com")
	w := &migrateWorld{
		p:      p,
		reader: p.AddAccount("touchmark-reader", platform.KindServiceAccount),
		bot:    p.AddAccount("group_42_bot_0123456789abcdef0123456789abcdef", platform.KindBot),
		person: p.AddAccount("jdoe", platform.KindUser),
		old:    p.AddAccount("group_42_bot_ffffffffffffffffffffffffffffffff", platform.KindBot),
	}
	api := p.AddRepo(platform.Repo{Path: "acme/api"})
	web := p.AddRepo(platform.Repo{Path: "acme/web"})
	deep := p.AddRepo(platform.Repo{Path: "acme/sub/deep"})
	cli := p.AddRepo(platform.Repo{Path: "tools/cli"})
	const alias = "chore/sync-engineering-assets"
	open := func(r platform.Repo, by platform.Account, head string) int64 {
		return p.AddPR(r.ID, platform.PR{Head: head, Author: by, Title: "chore: sync engineering assets", Body: "Synced."})
	}
	open(api, w.bot, alias)
	open(cli, w.bot, alias)
	open(deep, w.bot, alias)
	open(web, w.person, alias)
	open(web, w.bot, "feature")
	closed := open(api, w.old, alias)
	p.SetPRState(api.ID, closed, platform.Closed, &w.old, time.Time{})
	fork := p.AddRepo(platform.Repo{Path: "jdoe/api", Fork: true})
	p.AddPR(api.ID, platform.PR{Head: alias, HeadRepoID: fork.ID, Author: w.old, Title: "from a fork"})
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	drivers := planDrivers
	t.Cleanup(func() { planDrivers = drivers })
	planDrivers = map[string]planDriver{"gitlab": func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
		if client == nil || rp.Type != "gitlab" || rp.URL != "https://gitlab.example.com" || rp.APIURL != "https://gitlab.example.com/api/v4" {
			return nil, errors.New("unexpected provider")
		}
		w.built = append(w.built, c)
		if c.Token != migrateReadToken {
			return p.Reader(platform.Account{ID: "nobody"}), nil
		}
		return p.Reader(w.reader), nil
	}}
	return w
}

// TestMigrateFindsTheBot: with a read credential, the bot is the author of
// open merge requests on the alias in the projects the file selects; a
// person's is only a note, and closed ones, other branches and forks do
// not count.
func TestMigrateFindsTheBot(t *testing.T) {
	fixedClock(t)
	w := newMigrateWorld(t)
	cfg := "platform: gitlab\nbase-url: https://gitlab.example.com\ngroup: acme\ninclude-subgroups: true\nproject: tools/cli\n" +
		"branch: chore/sync-engineering-assets\nauthor-email: someone@example.com\n"
	file := migrateFile(t, cfg)
	vars := map[string]string{"TOUCHMARK_GITLAB_READ_TOKEN": migrateReadToken}
	res := runWith(t, vars, "migrate", "--id", "acme-eng", "--writer", "touchmark-writer", "--from-multi-gitter", file)
	if res.code != exitOK {
		t.Fatalf("exit %d: %s\n%s", res.code, res.stderr, res.stdout)
	}
	if len(w.built) != 1 || w.built[0].Token != migrateReadToken {
		t.Errorf("drivers built with %+v", w.built)
	}
	if strings.Contains(res.stdout, migrateReadToken) {
		t.Errorf("the output shows the token")
	}
	m := parseMigrated(t, res.stdout)
	checkMigrated(t, m)
	p := m.hub.Providers[0]
	if p.Writer != "touchmark-writer" || !slices.Equal(p.KnownAuthors, []string{w.bot.Login}) {
		t.Errorf("writer %q, known_authors %q", p.Writer, p.KnownAuthors)
	}
	for _, want := range []string{
		"opened 3 open merge requests on the branch; the account exists (a bot)",
		`# - jdoe  # opened 1 open merge request on the branch; TODO: a person's account`,
		"looked for open merge requests on chore/sync-engineering-assets in 4 projects",
	} {
		if !strings.Contains(m.hubYML, want) {
			t.Errorf("hub.yml lacks %q:\n%s", want, m.hubYML)
		}
	}
	if strings.Contains(m.hubYML, w.old.Login) {
		t.Errorf("hub.yml names the older bot, whose merge requests are closed or from a fork:\n%s", m.hubYML)
	}

	// Without subgroups the deep project is not scanned.
	res = runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, strings.Replace(cfg, "include-subgroups: true", "include-subgroups: false", 1)))
	if !strings.Contains(res.stdout, "opened 2 open merge requests") || !strings.Contains(res.stdout, "in 3 projects") {
		t.Errorf("without subgroups:\n%s", res.stdout)
	}
}

// TestMigrateChecksLogins: --bot and the login in author-email are looked
// up; one that does not exist is shown commented out and fails the run
// (exit 1) after the files are printed. Nothing found on the branch falls
// back to author-email.
func TestMigrateChecksLogins(t *testing.T) {
	fixedClock(t)
	w := newMigrateWorld(t)
	vars := map[string]string{"TOUCHMARK_READ_TOKEN": migrateReadToken}
	base := "platform: gitlab\nbase-url: https://gitlab.example.com\nproject: tools/cli\nbranch: nothing-here\n"

	res := runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, base+"author-email: "+w.bot.Login+"@noreply.gitlab.example.com\n"))
	if res.code != exitOK {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	m := parseMigrated(t, res.stdout)
	if got := m.hub.Providers[0].KnownAuthors; !slices.Equal(got, []string{w.bot.Login}) ||
		!strings.Contains(m.hubYML, "from author-email; the account exists (a bot)") || !strings.Contains(m.hubYML, "none found") {
		t.Errorf("known_authors %q:\n%s", got, m.hubYML)
	}

	// A user's private commit address names the user.
	res = runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, base+"author-email: 17-jdoe@users.noreply.gitlab.example.com\n"))
	if m := parseMigrated(t, res.stdout); res.code != exitOK || !slices.Equal(m.hub.Providers[0].KnownAuthors, []string{"jdoe"}) ||
		!strings.Contains(m.hubYML, "(a person's account)") {
		t.Errorf("exit %d:\n%s", res.code, res.stdout)
	}

	res = runWith(t, vars, "migrate", "--id", "acme-eng", "--bot", w.bot.Login, "--bot", "group_7_bot_gone", "--from-multi-gitter",
		migrateFile(t, base+"author-email: "+w.old.Login+"@noreply.gitlab.example.com\n"))
	// The host and the variable are told before the credential goes out.
	if res.code != exitFailed || res.stderr != "touchmark migrate: checking accounts on https://gitlab.example.com with TOUCHMARK_READ_*\n" {
		t.Fatalf("exit %d, stderr %q; want 1, the files and the host", res.code, res.stderr)
	}
	m = parseMigrated(t, res.stdout)
	checkMigrated(t, m)
	if got := m.hub.Providers[0].KnownAuthors; !slices.Equal(got, []string{w.bot.Login}) {
		t.Errorf("known_authors %q: want only the --bot that exists (author-email is not used with --bot)", got)
	}
	if !strings.Contains(m.hubYML, `# - group_7_bot_gone  # from --bot; TODO: no such account`) {
		t.Errorf("hub.yml:\n%s", m.hubYML)
	}

	// A credential the platform refuses: the check failed, exit 1.
	res = runWith(t, map[string]string{"TOUCHMARK_READ_TOKEN": "wrong-token-000000"}, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, base+"author-email: "+w.bot.Login+"@noreply.gitlab.example.com\n"))
	if res.code != exitFailed || !strings.Contains(res.stdout, "TODO: looking for multi-gitter's merge requests failed") {
		t.Errorf("exit %d:\n%s", res.code, res.stdout)
	}
	parseMigrated(t, res.stdout)

	// No gitlab driver in the build: nothing is checked.
	planDrivers = map[string]planDriver{}
	res = runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter",
		migrateFile(t, base+"author-email: "+w.bot.Login+"@noreply.gitlab.example.com\n"))
	if res.code != exitOK || !strings.Contains(res.stdout, "TODO: not checked: this build has no gitlab driver yet") {
		t.Errorf("exit %d:\n%s", res.code, res.stdout)
	}
}

// kindless is a reader whose merge request authors carry no kind, as
// GitLab's short users of merge requests do (UserBasic has no bot flag):
// only Lookup tells a bot from a person.
type kindless struct{ platform.Reader }

func (k kindless) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	prs, err := k.Reader.PRs(ctx, r, heads, authors)
	for i := range prs {
		prs[i].Author.Kind = platform.KindUnknown
	}
	return prs, err
}

// kindlessDrivers makes the gitlab driver of w report authors without a
// kind.
func kindlessDrivers(w *migrateWorld) {
	planDrivers = map[string]planDriver{"gitlab": func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
		w.built = append(w.built, c)
		return kindless{w.p.Reader(w.reader)}, nil
	}}
}

// TestMigrateAuthorKinds: the authors of merge requests come without a
// kind, as from GitLab, and are looked up: a person is never proposed (the
// person's closes would count as touchmark's own and their declines be
// forgotten), the bot with the most merge requests is, another bot is a
// TODO, and with only a person on the branch the author-email's bot is the
// fallback. A person's account once became an active entry.
func TestMigrateAuthorKinds(t *testing.T) {
	fixedClock(t)
	w := newMigrateWorld(t)
	kindlessDrivers(w)
	vars := map[string]string{"TOUCHMARK_GITLAB_READ_TOKEN": migrateReadToken}
	cfg := "platform: gitlab\nbase-url: https://gitlab.example.com\ngroup: acme\ninclude-subgroups: true\nproject: tools/cli\n" +
		"branch: chore/sync-engineering-assets\n"
	res := runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter", migrateFile(t, cfg))
	if res.code != exitOK {
		t.Fatalf("exit %d: %s\n%s", res.code, res.stderr, res.stdout)
	}
	m := parseMigrated(t, res.stdout)
	if got := m.hub.Providers[0].KnownAuthors; !slices.Equal(got, []string{w.bot.Login}) {
		t.Errorf("known_authors %q, want the bot alone:\n%s", got, m.hubYML)
	}
	if !strings.Contains(m.hubYML, `# - jdoe  # opened 1 open merge request on the branch; TODO: a person's account`) {
		t.Errorf("hub.yml does not keep jdoe out:\n%s", m.hubYML)
	}

	// A second bot with fewer merge requests on the branch is a TODO.
	other := w.p.AddAccount("group_77_bot_0000000000000000000000000000beef", platform.KindBot)
	api, err := w.p.Reader(w.reader).Repo(t.Context(), "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	w.p.AddPR(api.ID, platform.PR{Head: "chore/sync-engineering-assets", Author: other, Title: "theirs"})
	if err := w.p.Err(); err != nil {
		t.Fatal(err)
	}
	res = runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter", migrateFile(t, cfg))
	m = parseMigrated(t, res.stdout)
	if got := m.hub.Providers[0].KnownAuthors; !slices.Equal(got, []string{w.bot.Login}) ||
		!strings.Contains(m.hubYML, "# - "+other.Login+"  # opened 1 open merge request on the branch; TODO: another bot on the branch") {
		t.Errorf("known_authors %q with another bot on the branch:\n%s", got, m.hubYML)
	}

	// Only a person on the branch: the author-email's bot is proposed.
	res = runWith(t, vars, "migrate", "--id", "acme-eng", "--from-multi-gitter", migrateFile(t,
		"platform: gitlab\nbase-url: https://gitlab.example.com\nproject: acme/web\nbranch: chore/sync-engineering-assets\n"+
			"author-email: "+w.bot.Login+"@noreply.gitlab.example.com\n"))
	m = parseMigrated(t, res.stdout)
	if got := m.hub.Providers[0].KnownAuthors; res.code != exitOK || !slices.Equal(got, []string{w.bot.Login}) ||
		!strings.Contains(m.hubYML, "# - jdoe  #") {
		t.Errorf("exit %d, known_authors %q:\n%s", res.code, got, m.hubYML)
	}
}

func TestLoginFromEmail(t *testing.T) {
	for _, tc := range []struct{ email, host, want string }{
		{"group_42_bot_abc@noreply.gitlab.example.com", "gitlab.example.com", "group_42_bot_abc"},
		{"project_7_bot_abc1@noreply.gitlab.example.com", "gitlab.example.com:8443", "project_7_bot_abc1"},
		{"service_account_group_9_x@NoReply.GitLab.example.com", "gitlab.example.com", "service_account_group_9_x"},
		{"17-jdoe@users.noreply.gitlab.com", "gitlab.com", "jdoe"},
		{"x-jdoe@users.noreply.gitlab.com", "gitlab.com", ""},
		{"jdoe@users.noreply.gitlab.com", "gitlab.com", ""},
		{"bot@noreply.other.example.com", "gitlab.example.com", ""},
		{"bot@example.com", "gitlab.example.com", ""},
		{"", "gitlab.com", ""},
	} {
		if got := loginFromEmail(tc.email, tc.host); got != tc.want {
			t.Errorf("loginFromEmail(%q, %q) = %q, want %q", tc.email, tc.host, got, tc.want)
		}
	}
}

func TestGitLabURL(t *testing.T) {
	for _, tc := range []struct{ in, web, host string }{
		{"", "https://gitlab.com", "gitlab.com"},
		{"https://gitlab.com/api/v4/", "https://gitlab.com", "gitlab.com"},
		{"https://GitLab.Example.com:8443/api/v4", "https://gitlab.example.com:8443", "gitlab.example.com:8443"},
		{"https://example.com/gitlab/api/v4/", "https://example.com/gitlab", "example.com"},
		{"http://localhost:8080/", "http://localhost:8080", "localhost:8080"},
	} {
		web, host, err := gitlabURL(tc.in)
		if err != nil || web != tc.web || host != tc.host {
			t.Errorf("gitlabURL(%q) = %q, %q, %v; want %q, %q", tc.in, web, host, err, tc.web, tc.host)
		}
	}
	for _, in := range []string{"gitlab.example.com", "ftp://gitlab.example.com", "https://gitlab.example.com/?a=b", "http://gitlab.example.com"} {
		if _, _, err := gitlabURL(in); err == nil {
			t.Errorf("gitlabURL(%q): no error", in)
		}
	}
}

// TestYAMLString: every string reads back as the same YAML string, with the
// !!str tag touchmark's strict decoding wants.
func TestYAMLString(t *testing.T) {
	for _, s := range []string{"acme", "chore/sync-engineering-assets", "yes", "No", "null", "1.0", "2026-10-01", "a b", "a: b",
		"#x", "[bot]", "acme[bot]", `quote"d`, "multi\nline", "-x", "~", "é", "0x1f", "1e3", ".inf"} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte("v: "+yamlString(s)+"\n"), &doc); err != nil {
			t.Errorf("yamlString(%q) = %s: %v", s, yamlString(s), err)
			continue
		}
		v := doc.Content[0].Content[1]
		if v.Tag != "!!str" || v.Value != s {
			t.Errorf("yamlString(%q) = %s: reads back %s %q", s, yamlString(s), v.Tag, v.Value)
		}
	}
}
