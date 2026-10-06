package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/docsurl"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// migrate turns the configuration of a hub that delivered with multi-gitter
// on GitLab into touchmark's: hub.yml with one gitlab provider, the
// multi-gitter branch as a branch alias and the bot that opened its merge
// requests in known_authors; targets.yml from the groups, projects and
// topics; and .touchmark/operations.yml with adopt_unmarked for 30 days, so
// that distribute takes the open merge requests multi-gitter left over.
//
// The multi-gitter keys and their meaning are those of multi-gitter's
// public documentation and source (github.com/lindell/multi-gitter,
// README "All available run options", cmd/platform.go, cmd/config.go and
// internal/scm/gitlab/gitlab.go, v0.63):
//   - the file is read by viper and each value is set on the flag of the
//     same name (keys are case-insensitive); a list sets the flag once per
//     item, and a string slice flag splits every value as CSV;
//   - platform defaults to github; base-url to gitlab.com for gitlab, and
//     the GitLab client appends /api/v4 when the URL lacks it;
//   - GitLab takes group, user and project (repo and org are GitHub's and
//     Gitea's); include-subgroups is off by default; forks are included
//     unless skip-forks; archived projects and projects without merge
//     requests are left out;
//   - topic filters every project, from groups and projects alike, and a
//     project passes with any one of the topics;
//   - skip-repo drops projects by path; repo-include and repo-exclude are
//     regular expressions over the path;
//   - branch defaults to multi-gitter-branch; pr-title to the first line of
//     commit-message; merge requests are opened by the token's user.

// Limits of migrate.
const (
	// maxMigrateFile bounds the multi-gitter file.
	maxMigrateFile = 1 << 20
	// migrateScanLimit bounds how many projects migrate lists merge
	// requests in while it looks for the bot multi-gitter ran as.
	migrateScanLimit = 100
	// adoptDays is how long the generated adopt_unmarked entry is active.
	adoptDays = 30
	// multiGitterBranch is multi-gitter's default branch.
	multiGitterBranch = "multi-gitter-branch"
)

// migrateNow is the clock of migrate: the adopt_unmarked date counts from
// it. Tests replace it and must not run in parallel.
var migrateNow = time.Now

// migrateOptions are migrate's flags.
type migrateOptions struct {
	from   string
	id     string
	writer string
	bots   []string
	caFile string
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty value")
	}
	*l = append(*l, s)
	return nil
}

// migrateCommand is the migrate command. Its flag values live with the
// command itself, as distribute's do.
func migrateCommand() *command {
	m := &migrateOptions{}
	return &command{
		name:     "migrate",
		synopsis: "--from-multi-gitter FILE [--id ID] [--writer LOGIN] [--bot LOGIN]... [--ca-file FILE]",
		summary:  "print hub.yml, targets.yml and operations.yml for a hub that replaces a multi-gitter setup on GitLab",
		flags: func() (*flagSet, *options) {
			*m = migrateOptions{}
			f, o := newFlagSet("migrate")
			f.fs.StringVar(&m.from, "from-multi-gitter", "", "the multi-gitter config `FILE` (YAML) to migrate from")
			f.fs.StringVar(&m.id, "id", "", "the new hub's `ID`, a slug like acme-eng (default: a placeholder that check rejects)")
			f.fs.StringVar(&m.writer, "writer", "", "the writer service account's `LOGIN`")
			f.fs.Var((*stringList)(&m.bots), "bot", "the `LOGIN` that opened multi-gitter's merge requests; repeatable (default: found through the read credential)")
			f.fs.StringVar(&m.caFile, "ca-file", "", "a PEM `FILE` of CA certificates to trust, besides the system's, when checking accounts")
			return f, o
		},
		run: func(ctx context.Context, e *env, _ *options) error { return runMigrate(ctx, e, m) },
	}
}

// runMigrate prints the three files on stdout, one
// after the other under "# ==> <name> <==" lines, and checks each with the
// config parsers before printing.
//
// The bot multi-gitter ran as is the author of open merge requests on the
// multi-gitter branch: with a read credential for the provider
// (TOUCHMARK_<ID>_READ_TOKEN or TOUCHMARK_READ_TOKEN), migrate lists them
// in up to migrateScanLimit projects the file selects. Logins from --bot,
// or the login in multi-gitter's author-email when nothing else names one
// (GitLab's addresses <login>@noreply.<host> of bots and service accounts,
// <id>-<login>@<commit email host> of users), are looked up through the
// same credential. Without a credential, or a gitlab driver in this build,
// the logins are printed unchecked with a TODO comment.
//
// The exit code is 2 for a usage or configuration error (the file cannot be
// read, is not a GitLab multi-gitter config, or names something touchmark
// cannot express), 1 when a login was checked and not found or the check
// failed, 0 otherwise.
func runMigrate(ctx context.Context, e *env, m *migrateOptions) (err error) {
	if m.from == "" {
		return usageErrorf("missing --from-multi-gitter FILE")
	}
	if m.id != "" && m.id != config.PlaceholderID {
		if _, _, err := config.ParseHub([]byte("version: 1\nid: " + jsonString(m.id) + "\n")); err != nil {
			return usageErrorf("--id: %q must be 3 to 40 lowercase letters and digits in words joined by single hyphens", m.id)
		}
	}
	reg := redact.New()
	defer func() { err = maskError(reg, err) }()
	data, err := readLimited(m.from, maxMigrateFile)
	if err != nil {
		return configErrorf("--from-multi-gitter: %w", err)
	}
	if int64(len(data)) > maxMigrateFile {
		return configErrorf("--from-multi-gitter: %s is larger than %d bytes", m.from, maxMigrateFile)
	}
	mg, err := parseMultiGitter(data)
	if err != nil {
		return configErrorf("%s: %w", m.from, err)
	}
	if mg.token != "" {
		reg.Add(mg.token, basicUsers...)
	}
	plan, err := newMigration(mg, m)
	if err != nil {
		return configErrorf("%s: %w", m.from, err)
	}
	checked := plan.checkAccounts(ctx, e, reg, m.caFile)
	files, err := plan.render(migrateNow())
	if err != nil {
		return err
	}
	out := reg.Writer(e.stdout)
	for _, f := range files {
		if _, err := io.WriteString(out, f); err != nil {
			return err
		}
	}
	if err := out.Flush(); err != nil {
		return err
	}
	if !checked {
		return exitWith(exitFailed)
	}
	return nil
}

// multiGitter is what migrate reads from a multi-gitter config file.
type multiGitter struct {
	platform         string
	platformSet      bool
	baseURL          string
	groups           []string
	users            []string
	projects         []string
	topics           []string
	includeSubgroups bool
	skipForks        bool
	skipRepos        []string
	repoInclude      string
	repoExclude      string
	branch           string
	baseBranch       string
	prTitle          string
	prBody           string
	commitMessage    string
	labels           []string
	draft            bool
	authorEmail      string
	fork             bool
	skipPR           bool
	pushOnly         bool
	// ignoredGitHub lists the keys GitLab does not read (repo, org) that
	// the file sets.
	ignoredGitHub []string
	// token is the token the file holds, if any: never printed.
	token string
}

// yamlLineRe finds the line number in a YAML error message.
var yamlLineRe = regexp.MustCompile(`line ([0-9]+)`)

// parseMultiGitter reads a multi-gitter config file. Keys compare without
// case, as viper reads them; keys migrate has no use for are ignored, as
// multi-gitter ignores keys of other commands.
func parseMultiGitter(data []byte) (*multiGitter, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// A YAML message may quote the text around the error, which could
		// be the token: only the line is told.
		if l := yamlLineRe.FindStringSubmatch(err.Error()); l != nil {
			return nil, fmt.Errorf("not valid YAML (line %s)", l[1])
		}
		return nil, errors.New("not valid YAML")
	}
	mg := &multiGitter{platform: "github", branch: multiGitterBranch}
	if len(root.Content) == 0 {
		return mg, nil
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping of multi-gitter options")
	}
	values := map[string]*yaml.Node{}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		k, v := doc.Content[i], doc.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: a key must be a plain name", k.Line)
		}
		key := strings.ToLower(k.Value)
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("line %d: %s is set twice", k.Line, key)
		}
		values[key] = v
	}
	r := mgReader{values: values}
	if n := values["platform"]; n != nil {
		mg.platformSet = true
	}
	mg.platform = r.str("platform", mg.platform)
	mg.baseURL = r.str("base-url", "")
	mg.groups = r.list("group")
	mg.users = r.list("user")
	mg.projects = r.list("project")
	mg.topics = r.list("topic")
	mg.includeSubgroups = r.flag("include-subgroups")
	mg.skipForks = r.flag("skip-forks")
	mg.skipRepos = r.list("skip-repo")
	mg.repoInclude = r.str("repo-include", "")
	mg.repoExclude = r.str("repo-exclude", "")
	mg.branch = r.str("branch", multiGitterBranch)
	mg.baseBranch = r.str("base-branch", "")
	mg.prTitle = r.str("pr-title", "")
	mg.prBody = r.str("pr-body", "")
	mg.commitMessage = r.str("commit-message", "")
	mg.labels = r.list("labels")
	mg.draft = r.flag("draft")
	mg.authorEmail = r.str("author-email", "")
	mg.fork = r.flag("fork")
	mg.skipPR = r.flag("skip-pr")
	mg.pushOnly = r.flag("push-only")
	mg.token = r.str("token", "")
	for _, k := range []string{"repo", "org"} {
		if len(r.list(k)) > 0 {
			mg.ignoredGitHub = append(mg.ignoredGitHub, k)
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return mg, nil
}

// mgReader converts the values of a multi-gitter file as multi-gitter sets
// them on its flags; the first error sticks.
type mgReader struct {
	values map[string]*yaml.Node
	err    error
}

func (r *mgReader) invalid(n *yaml.Node, key, format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("line %d: %s: %s", n.Line, key, fmt.Sprintf(format, args...))
	}
}

// scalar returns a scalar value; null and absent are "".
func (r *mgReader) scalar(key string) (string, *yaml.Node, bool) {
	n := r.values[key]
	switch {
	case n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null"):
		return "", nil, false
	case n.Kind != yaml.ScalarNode:
		r.invalid(n, key, "must be a single value")
		return "", nil, false
	}
	return n.Value, n, true
}

func (r *mgReader) str(key, def string) string {
	v, _, ok := r.scalar(key)
	if !ok {
		return def
	}
	return v
}

// flag reads a boolean as pflag parses it (strconv.ParseBool).
func (r *mgReader) flag(key string) bool {
	v, n, ok := r.scalar(key)
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.invalid(n, key, "%s is not true or false", strconv.Quote(v))
	}
	return b
}

// list reads a string slice: a scalar or a list of scalars, each split as
// CSV (pflag's StringSlice), items trimmed, empty items dropped.
func (r *mgReader) list(key string) []string {
	n := r.values[key]
	if n == nil {
		return nil
	}
	var raw []*yaml.Node
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil
		}
		raw = []*yaml.Node{n}
	case yaml.SequenceNode:
		raw = n.Content
	default:
		r.invalid(n, key, "must be a value or a list of values")
		return nil
	}
	var out []string
	for _, item := range raw {
		if item.Kind != yaml.ScalarNode {
			r.invalid(item, key, "every item must be a single value")
			return nil
		}
		if item.Tag == "!!null" {
			continue
		}
		fields, err := csv.NewReader(strings.NewReader(item.Value)).Read()
		if err != nil && !errors.Is(err, io.EOF) {
			r.invalid(item, key, "not a comma-separated list")
			return nil
		}
		for _, f := range fields {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

// migration is the hub a multi-gitter file becomes.
type migration struct {
	mg       *multiGitter
	id       string
	url      string // the provider's web URL
	host     string // lowercased, with a port
	provider string // the provider id
	writer   string
	groups   []string
	projects []string
	exclude  []string
	title    string
	commit   string
	labels   []string
	// draft is set when the title had a draft prefix.
	draft bool
	// accounts are the known_authors candidates in the order found.
	accounts []*candidate
	// hubNotes, targetNotes and accountNotes are TODO and info lines.
	hubNotes, targetNotes, accountNotes []string
}

// candidate is a login for known_authors.
type candidate struct {
	login  string
	source string // how migrate found it
	// status of the check: "" not checked, "ok", "missing", "failed" (the
	// lookup failed), and, for an author of merge requests, "user" (a
	// person), "unsure" (GitLab did not tell the kind) or "other" (a bot
	// beside the main one): those are not proposed.
	status string
	kind   platform.AccountKind
	mrs    int // open merge requests found on the branch
}

// newMigration validates the file for GitLab and builds the migration.
func newMigration(mg *multiGitter, m *migrateOptions) (*migration, error) {
	switch {
	case !mg.platformSet && len(mg.groups)+len(mg.projects) == 0:
		return nil, errors.New("platform: not set, so multi-gitter used github; migrate reads GitLab configurations only (add platform: gitlab if the platform came from a flag)")
	case mg.platformSet && !strings.EqualFold(mg.platform, "gitlab"):
		return nil, fmt.Errorf("platform: %s; migrate reads GitLab configurations only", strconv.Quote(mg.platform))
	}
	web, host, err := gitlabURL(mg.baseURL)
	if err != nil {
		return nil, fmt.Errorf("base-url: %w", err)
	}
	p := &migration{mg: mg, id: m.id, url: web, host: host, provider: providerIDFor(host), writer: m.writer}
	if p.id == "" {
		p.id = config.PlaceholderID
	}
	if len(mg.groups)+len(mg.projects)+len(mg.users) == 0 {
		return nil, errors.New("no group, project or user: multi-gitter selects no project")
	}
	if p.groups, err = uniquePaths("group", mg.groups, 1); err != nil {
		return nil, err
	}
	if p.projects, err = uniquePaths("project", mg.projects, 2); err != nil {
		return nil, err
	}
	if p.exclude, err = uniquePaths("skip-repo", mg.skipRepos, 2); err != nil {
		return nil, err
	}
	for _, t := range mg.topics {
		if strings.TrimSpace(t) == "" {
			return nil, errors.New("topic: an empty topic")
		}
	}
	if !p.isSyncBranch(mg.branch) {
		if _, _, err := config.ParseHub([]byte("version: 1\nid: abc\nbranch_aliases: [" + jsonString(mg.branch) + "]\n")); err != nil {
			return nil, fmt.Errorf("branch: %s is not a branch name touchmark accepts", strconv.Quote(mg.branch))
		}
	}
	if m.writer != "" && !validAccount(m.writer) {
		return nil, fmt.Errorf("--writer: %s is not an account name", strconv.Quote(m.writer))
	}
	for _, b := range m.bots {
		if !validAccount(b) {
			return nil, fmt.Errorf("--bot: %s is not an account name", strconv.Quote(b))
		}
		p.addCandidate(b, "--bot")
	}
	p.prFields()
	p.notes()
	return p, nil
}

// isSyncBranch reports whether b is the new hub's own sync branch.
func (p *migration) isSyncBranch(b string) bool {
	return p.id != config.PlaceholderID && b == "touchmark/"+p.id
}

// gitlabURL returns the web URL of the GitLab instance at base-url and its
// host: gitlab.com when empty; a trailing /api/v4, which the GitLab client
// multi-gitter uses accepts, is dropped.
func gitlabURL(base string) (web, host string, err error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "https://gitlab.com", "gitlab.com", nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", "", errors.New("not a URL")
	}
	if u.User != nil {
		return "", "", errors.New("the URL holds credentials; remove them")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("the URL has a query or a fragment")
	}
	host = strings.ToLower(u.Host)
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()):
	default:
		return "", "", fmt.Errorf("%s: touchmark reaches GitLab over https only (http for localhost)", strconv.Quote(base))
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/api/v4") {
		path = strings.TrimRight(path[:len(path)-len("/api/v4")], "/")
	}
	if host == "gitlab.com" && path == "" && u.Scheme == "https" {
		return "https://gitlab.com", host, nil
	}
	return u.Scheme + "://" + host + path, host, nil
}

// providerIDRe is the provider id pattern of hub.yml.
var providerIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// providerIDFor names the provider after the first label of its host
// (gitlab.example.com → gitlab), or "gitlab" when that label is no valid
// provider id.
func providerIDFor(host string) string {
	name, _, _ := strings.Cut(host, ":")
	label, _, _ := strings.Cut(name, ".")
	if providerIDRe.MatchString(label) {
		return label
	}
	return "gitlab"
}

// uniquePaths validates paths of at least minSegments segments and drops
// repeats (ignoring case), keeping the first spelling.
func uniquePaths(key string, paths []string, minSegments int) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		p = strings.Trim(p, "/")
		ref := p
		if minSegments < 2 {
			ref = "namespace/" + p // a namespace: one segment is enough
		}
		if strings.Contains(p, ":") {
			return nil, fmt.Errorf("%s: %s is not a GitLab path", key, strconv.Quote(p))
		}
		if _, err := config.ParseRef(ref); err != nil {
			return nil, fmt.Errorf("%s: %s is not a GitLab path", key, strconv.Quote(p))
		}
		if k := strings.ToLower(p); !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// validAccount reports whether s is an account name hub.yml accepts.
func validAccount(s string) bool {
	return s != "" && len(s) <= 255 && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// draftTitleRe matches the prefixes that turn a GitLab merge request into a
// draft, which hub.yml refuses in commit.message and pr.title.
var draftTitleRe = regexp.MustCompile(`(?i)^\s*(draft:|\[draft\]|\(draft\)|wip:|\[wip\])`)

// prFields maps pr-title, commit-message and labels.
func (p *migration) prFields() {
	mg := p.mg
	msg := strings.TrimSpace(mg.commitMessage)
	switch {
	case msg == "":
	case draftTitleRe.MatchString(msg) || strings.Contains(msg, "------------------------ >8 ------------------------"):
		p.hubNotes = append(p.hubNotes, "commit-message is left out: touchmark refuses a message that starts with Draft: or WIP: or holds git's scissors line")
	default:
		p.commit = msg
	}
	title := strings.TrimSpace(mg.prTitle)
	if title == "" && msg != "" {
		// multi-gitter's default title: the first line of the message.
		title, _, _ = strings.Cut(msg, "\n")
		title = strings.TrimSpace(title)
	}
	if strings.ContainsAny(title, "\r\n") {
		title, _, _ = strings.Cut(strings.ReplaceAll(title, "\r", "\n"), "\n")
	}
	// A draft prefix made multi-gitter's merge requests drafts; hub.yml
	// refuses it in pr.title (the driver adds the prefix of pr.draft
	// itself), so it becomes pr.draft.
	for draftTitleRe.MatchString(title) {
		title = strings.TrimSpace(draftTitleRe.ReplaceAllString(title, ""))
		p.draft = true
	}
	if p.draft {
		p.hubNotes = append(p.hubNotes, "the title's draft prefix (Draft: or WIP:) is dropped: pr.draft: true opens drafts instead")
	}
	p.title = title
	for _, l := range mg.labels {
		if strings.Contains(l, ",") || len([]rune(l)) > 50 {
			p.hubNotes = append(p.hubNotes, "label "+strconv.Quote(l)+" is left out: labels have at most 50 characters and no commas")
			continue
		}
		if !slices.Contains(p.labels, l) {
			p.labels = append(p.labels, l)
		}
	}
}

// notes records what the file sets that touchmark does differently.
func (p *migration) notes() {
	mg := p.mg
	if mg.prBody != "" {
		p.hubNotes = append(p.hubNotes, "TODO: multi-gitter's pr-body has no key here: put the text in a file of the hub and set pr.intro_file")
	}
	if mg.baseBranch != "" {
		p.hubNotes = append(p.hubNotes, "base-branch "+strconv.Quote(mg.baseBranch)+" is dropped: touchmark proposes to each project's default branch")
	}
	if mg.fork {
		p.hubNotes = append(p.hubNotes, "TODO: multi-gitter pushed to forks; merge requests from forks are never touchmark's, so it opens new ones: close the old ones after the move")
	}
	if mg.skipPR || mg.pushOnly {
		p.hubNotes = append(p.hubNotes, "skip-pr and push-only have no counterpart: touchmark always delivers through merge requests")
	}
	if p.mg.authorEmail != "" {
		p.hubNotes = append(p.hubNotes, "author-name and author-email are dropped: the writer authors every commit")
	}
	if !mg.platformSet {
		p.hubNotes = append(p.hubNotes, "platform was not set in the file; read as gitlab, whose keys group and project it uses")
	}
	if len(mg.ignoredGitHub) > 0 {
		p.targetNotes = append(p.targetNotes, "left out: "+strings.Join(mg.ignoredGitHub, " and ")+", which multi-gitter reads on GitHub and Gitea only")
	}
	for _, u := range mg.users {
		p.targetNotes = append(p.targetNotes, "TODO: multi-gitter took the projects of user "+strconv.Quote(u)+"; list them under repo:")
	}
	if mg.repoInclude != "" {
		p.targetNotes = append(p.targetNotes, "TODO: repo-include "+strconv.Quote(mg.repoInclude)+" has no counterpart: list the projects it kept under repo:, or exclude the others")
	}
	if mg.repoExclude != "" {
		p.targetNotes = append(p.targetNotes, "TODO: repo-exclude "+strconv.Quote(mg.repoExclude)+" has no counterpart: list the projects it dropped under exclude:")
	}
}

// addCandidate records a login for known_authors once, ignoring case.
func (p *migration) addCandidate(login, source string) *candidate {
	for _, c := range p.accounts {
		if strings.EqualFold(c.login, login) {
			return c
		}
	}
	c := &candidate{login: login, source: source}
	p.accounts = append(p.accounts, c)
	return c
}

// loginFromEmail returns the GitLab login an address names, if its form
// tells it: <login>@noreply.<host>, the address GitLab gives the bot users
// of group and project access tokens and service accounts
// (Gitlab::Utils::UsernameAndEmailGenerator; ResourceAccessTokens::
// CreateService, Users::ServiceAccounts::CreateService), and
// <id>-<login>@users.noreply.<host>, a user's private commit address
// (Gitlab::PrivateCommitEmail, with the default commit email host).
func loginFromEmail(email, host string) string {
	local, domain, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok || local == "" {
		return ""
	}
	name, _, _ := strings.Cut(strings.ToLower(host), ":")
	domain = strings.ToLower(domain)
	switch domain {
	case "noreply." + name:
		return local
	case "users.noreply." + name:
		if id, login, ok := strings.Cut(local, "-"); ok && id != "" && strings.Trim(id, "0123456789") == "" && login != "" {
			return login
		}
	}
	return ""
}

// checkAccounts finds and checks the known_authors candidates through the
// provider's read credential. It returns false when a check ran and failed
// (a login not found, the API refused): the output tells which.
//
// A candidate found as the author of merge requests is proposed only when
// GitLab says it is a bot or a service account: the short user of a merge
// request carries no kind, so every author is looked up (GET /users/:id
// tells the bot flag). A person's account, or one whose kind GitLab does not
// tell, is a commented TODO: a person in known_authors would make that
// person's closes touchmark's own, and their declines would be forgotten.
// Logins from --bot and author-email are the operator's
// word and are proposed when they exist.
func (p *migration) checkAccounts(ctx context.Context, e *env, reg *redact.Registry, caFile string) bool {
	emailLogin := loginFromEmail(p.mg.authorEmail, p.host)
	reader, why := p.reader(e, reg, caFile)
	if reader == nil {
		if len(p.accounts) == 0 && emailLogin != "" {
			p.addCandidate(emailLogin, "author-email")
		}
		if len(p.accounts) > 0 {
			p.accountNotes = append(p.accountNotes, "TODO: not checked: "+why)
		}
		return true
	}
	// The host is the file's: tell the operator where the read credential
	// goes before it goes there.
	fmt.Fprintf(e.stderr, "touchmark migrate: checking accounts on %s with %s\n", p.url, p.credentialName(e))
	ok := true
	lookup := func() {
		for _, c := range p.accounts {
			if c.status != "" {
				continue
			}
			a, err := reader.Lookup(ctx, c.login)
			switch {
			case err == nil:
				c.kind, c.login = a.Kind, cmpOr(a.Login, c.login)
				c.status = candidateStatus(c.source, a.Kind)
			case platform.ClassOf(err) == platform.ClassNotFound:
				c.status = "missing"
				ok = false
			default:
				p.accountNotes = append(p.accountNotes, "TODO: checking "+strconv.Quote(c.login)+" failed: "+oneLine(reg.Replace(err.Error())))
				c.status = "failed"
				ok = false
			}
		}
	}
	if len(p.accounts) == 0 {
		if err := p.discover(ctx, reader); err != nil {
			p.accountNotes = append(p.accountNotes, "TODO: looking for multi-gitter's merge requests failed: "+oneLine(reg.Replace(err.Error())))
			ok = false
		}
	}
	lookup()
	p.keepMainBot()
	fromBot := slices.ContainsFunc(p.accounts, func(c *candidate) bool { return c.source == "--bot" })
	found := slices.ContainsFunc(p.accounts, func(c *candidate) bool { return c.status == "ok" })
	if !fromBot && !found && emailLogin != "" {
		p.addCandidate(emailLogin, "author-email")
		lookup()
	}
	return ok
}

// candidateStatus is the status of a candidate that exists, of kind k: a
// merge request author is proposed only as a bot or a service account.
func candidateStatus(source string, k platform.AccountKind) string {
	if source != "merge requests" {
		return "ok"
	}
	switch k {
	case platform.KindBot, platform.KindServiceAccount:
		return "ok"
	case platform.KindUser:
		return "user"
	}
	return "unsure"
}

// keepMainBot keeps one bot found through merge requests in known_authors:
// the one with the most open merge requests on the branch (the first by
// login on a tie). multi-gitter's default branch is shared by everyone who
// runs it with defaults, so another bot there may be another team's; the
// others become commented TODOs.
func (p *migration) keepMainBot() {
	var main *candidate
	for _, c := range p.accounts {
		if c.source == "merge requests" && c.status == "ok" && (main == nil || c.mrs > main.mrs) {
			main = c
		}
	}
	for _, c := range p.accounts {
		if c != main && c.source == "merge requests" && c.status == "ok" {
			c.status = "other"
		}
	}
}

// credentialName names the variable migrate took the read credential from.
func (p *migration) credentialName(e *env) string {
	prefix := config.EnvPrefix(p.provider)
	for _, suffix := range []string{"READ_TOKEN", "READ_APP_ID", "READ_APP_KEY"} {
		if strings.TrimSpace(e.getenv(prefix+suffix)) != "" {
			return prefix + "READ_*"
		}
	}
	return "TOUCHMARK_READ_*"
}

// cmpOr returns a unless it is empty, else b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reader builds the read driver of the provider, or returns why it cannot.
func (p *migration) reader(e *env, reg *redact.Registry, caFile string) (platform.Reader, string) {
	prefix := config.EnvPrefix(p.provider)
	c, ok, err := auth.FromEnv(prefix, true, auth.Read, e.getenv)
	switch {
	case err != nil:
		return nil, oneLine(err.Error())
	case !ok:
		return nil, "set " + prefix + "READ_TOKEN (or TOUCHMARK_READ_TOKEN) to a token with read_api to check the accounts"
	}
	for _, s := range c.Secrets() {
		reg.Add(s, basicUsers...)
	}
	d := planDrivers["gitlab"]
	if d == nil {
		return nil, "this build has no gitlab driver yet"
	}
	hub := &config.Hub{Providers: []config.Provider{{ID: p.provider, Type: "gitlab", URL: p.url}}}
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil {
		return nil, oneLine(err.Error())
	}
	opts := httpx.Options{Redact: reg}
	if caFile != "" {
		pool, err := httpx.LoadCAFile(caFile)
		if err != nil {
			return nil, "--ca-file: " + oneLine(err.Error())
		}
		opts.RootCAs = pool
	}
	r, err := d(rps[0], c, httpx.New(opts))
	if err != nil {
		return nil, oneLine(reg.Replace(err.Error()))
	}
	return r, ""
}

// discover lists the open merge requests on the multi-gitter branch in the
// projects the file selects (projects first, then groups, at most
// migrateScanLimit projects) and records their authors: bots and service
// accounts as candidates, people as notes. Projects that are gone are
// skipped.
func (p *migration) discover(ctx context.Context, r platform.Reader) error {
	var repos []platform.Repo
	seen := map[string]bool{}
	add := func(repo platform.Repo) {
		if !seen[repo.ID] && len(repos) < migrateScanLimit {
			seen[repo.ID] = true
			repos = append(repos, repo)
		}
	}
	for _, path := range p.projects {
		repo, err := r.Repo(ctx, path)
		if platform.ClassOf(err) == platform.ClassNotFound {
			p.targetNotes = append(p.targetNotes, "project "+strconv.Quote(path)+" was not found through the read credential")
			continue
		}
		if err != nil {
			return err
		}
		add(repo)
	}
	capped := false
	for _, g := range p.groups {
		if len(repos) >= migrateScanLimit {
			capped = true
			break
		}
		res, err := r.Resolve(ctx, platform.Selector{Namespace: g, Subgroups: p.mg.includeSubgroups, Forks: !p.mg.skipForks})
		if err != nil {
			return err
		}
		for _, repo := range res.Repos {
			if len(repos) >= migrateScanLimit && !seen[repo.ID] {
				capped = true
			}
			add(repo)
		}
	}
	type found struct {
		a   platform.Account
		mrs int
	}
	byID := map[string]*found{}
	var order []string
	for _, repo := range repos {
		prs, err := r.PRs(ctx, repo, []string{p.mg.branch}, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", repo.Path, err)
		}
		for _, pr := range prs {
			if pr.State != platform.Open || pr.Head != p.mg.branch || pr.HeadRepoID != pr.RepoID || pr.Author.ID == "" {
				continue
			}
			f := byID[pr.Author.ID]
			if f == nil {
				f = &found{a: pr.Author}
				byID[pr.Author.ID] = f
				order = append(order, pr.Author.ID)
			}
			f.mrs++
		}
	}
	slices.SortFunc(order, func(a, b string) int { return strings.Compare(byID[a].a.Login, byID[b].a.Login) })
	for _, id := range order {
		f := byID[id]
		c := p.addCandidate(f.a.Login, "merge requests")
		c.mrs, c.kind = f.mrs, f.a.Kind
		if f.a.Kind == platform.KindUser {
			c.status = "user"
		}
	}
	desc := fmt.Sprintf("looked for open merge requests on %s in %d projects", p.mg.branch, len(repos))
	if capped {
		desc += fmt.Sprintf(" (the first %d the file selects)", migrateScanLimit)
	}
	if len(order) == 0 {
		desc += ": none found"
	}
	p.accountNotes = append(p.accountNotes, desc)
	return nil
}

// render returns the three files, each under its "# ==> name <==" line,
// after checking them with the config parsers.
func (p *migration) render(now time.Time) ([]string, error) {
	hub := p.hubYML()
	targets := p.targetsYML()
	ops := p.operationsYML(now)
	h, _, err := config.ParseHub([]byte(hub))
	if err != nil {
		return nil, fmt.Errorf("the generated %s does not parse (a bug): %w", config.HubFile, err)
	}
	t, _, err := config.ParseTargets([]byte(targets))
	if err != nil {
		return nil, fmt.Errorf("the generated %s does not parse (a bug): %w", config.TargetsFile, err)
	}
	o, _, err := config.ParseOperations([]byte(ops))
	if err != nil {
		return nil, fmt.Errorf("the generated %s does not parse (a bug): %w", config.OperationsFile, err)
	}
	if _, errs := config.CheckOperations(o, t, h, now); len(errs) > 0 {
		return nil, fmt.Errorf("the generated %s fails check (a bug): %w", config.OperationsFile, errors.Join(errs...))
	}
	head := "# Generated by touchmark migrate --from-multi-gitter. Save each part below\n" +
		"# as its own file in the hub, resolve every TODO, then run touchmark check and plan.\n" +
		"# The steps: " + docsurl.Migrate + "\n\n"
	return []string{head, section(config.HubFile, hub), "\n", section(config.TargetsFile, targets), "\n", section(config.OperationsFile, ops)}, nil
}

// section puts a file under its name line.
func section(name, body string) string { return "# ==> " + name + " <==\n" + body }

// yamlWriter writes block YAML with comments.
type yamlWriter struct{ b bytes.Buffer }

func (w *yamlWriter) line(indent int, s string) {
	w.b.WriteString(strings.Repeat("  ", indent))
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

func (w *yamlWriter) comment(indent int, s string) { w.line(indent, "# "+s) }

func (w *yamlWriter) comments(indent int, notes []string) {
	for _, n := range notes {
		w.comment(indent, n)
	}
}

func (p *migration) hubYML() string {
	var w yamlWriter
	w.line(0, "version: 1")
	if p.id == config.PlaceholderID {
		w.comment(0, "TODO: name the hub: a short slug; new merge requests open on touchmark/<id>.")
	}
	w.line(0, "id: "+yamlString(p.id))
	if p.isSyncBranch(p.mg.branch) {
		w.comment(0, "multi-gitter's branch "+strconv.Quote(p.mg.branch)+" is the sync branch itself: no alias needed.")
	} else {
		w.comment(0, "multi-gitter's merge requests live on its branch; touchmark keeps them there.")
		w.line(0, "branch_aliases:")
		w.line(1, "- "+yamlString(p.mg.branch))
	}
	w.line(0, "providers:")
	w.line(1, "- id: "+yamlString(p.provider))
	w.line(2, "type: gitlab")
	w.line(2, "url: "+yamlString(p.url))
	if p.writer != "" {
		w.line(2, "writer: "+yamlString(p.writer))
	} else {
		w.comment(2, "TODO: writer: the login of the writer service account (Developer, api and write_repository).")
	}
	p.knownAuthors(&w)
	if p.commit != "" {
		w.line(0, "commit:")
		w.line(1, "message: "+yamlString(p.commit))
	}
	if p.title != "" || len(p.labels) > 0 || p.mg.draft || p.draft {
		w.line(0, "pr:")
		if p.title != "" {
			w.line(1, "title: "+yamlString(p.title))
		}
		if len(p.labels) > 0 {
			w.line(1, "labels:")
			for _, l := range p.labels {
				w.line(2, "- "+yamlString(l))
			}
		}
		if p.mg.draft || p.draft {
			w.line(1, "draft: true")
		}
	}
	w.comments(0, p.hubNotes)
	return w.b.String()
}

// knownAuthors writes known_authors and the notes about the accounts.
func (p *migration) knownAuthors(w *yamlWriter) {
	var listed, other []*candidate
	for _, c := range p.accounts {
		if c.status == "ok" || c.status == "" {
			listed = append(listed, c)
		} else {
			other = append(other, c)
		}
	}
	if len(listed) == 0 {
		w.comment(2, "TODO: known_authors: the account that opened multi-gitter's merge requests;")
		w.comment(2, "keep its token until the move is over.")
		w.comments(2, p.accountNotes)
	} else {
		w.comment(2, "The account that opened multi-gitter's merge requests: its open ones are adopted.")
		w.comments(2, p.accountNotes)
		w.line(2, "known_authors:")
		for _, c := range listed {
			w.line(3, "- "+yamlString(c.login)+"  # "+c.describe())
		}
	}
	for _, c := range other {
		w.comment(2, "- "+yamlString(c.login)+"  # "+c.describe())
	}
}

// describe says where a candidate came from and what the check found.
func (c *candidate) describe() string {
	var parts []string
	switch c.source {
	case "merge requests":
		parts = append(parts, fmt.Sprintf("opened %d open merge request%s on the branch", c.mrs, plural(c.mrs)))
	default:
		parts = append(parts, "from "+c.source)
	}
	switch c.status {
	case "ok":
		parts = append(parts, "the account exists"+kindSuffix(c.kind))
	case "":
		parts = append(parts, "TODO: not checked")
	case "missing":
		parts = append(parts, "TODO: no such account (a revoked token's bot may be gone)")
	case "user":
		parts = append(parts, "TODO: a person's account; add it only if multi-gitter ran with that person's token")
	case "unsure":
		parts = append(parts, "TODO: GitLab does not tell whether it is a bot; add it only if multi-gitter ran with its token")
	case "other":
		parts = append(parts, "TODO: another bot on the branch; add it only if it opened multi-gitter's merge requests too")
	case "failed":
		parts = append(parts, "TODO: the check failed")
	}
	return strings.Join(parts, "; ")
}

func kindSuffix(k platform.AccountKind) string {
	switch k {
	case platform.KindBot:
		return " (a bot)"
	case platform.KindServiceAccount:
		return " (a service account)"
	case platform.KindUser:
		return " (a person's account)"
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (p *migration) targetsYML() string {
	var w yamlWriter
	w.line(0, "version: 1")
	w.comments(0, p.targetNotes)
	if len(p.groups) == 0 && len(p.projects) == 0 {
		w.line(0, "targets: []")
	} else {
		w.line(0, "targets:")
	}
	topics := p.mg.topics
	if len(p.groups) > 0 && len(topics) > 1 {
		w.comment(1, "multi-gitter took a project with any one of the topics; an entry here needs all of its topics,")
		w.comment(1, "so there is one entry per topic.")
	}
	if len(p.groups) > 0 && !p.mg.skipForks {
		w.comment(1, "multi-gitter took forks too (skip-forks was off).")
	}
	for _, g := range p.groups {
		each := [][]string{nil}
		if len(topics) > 0 {
			each = nil
			for _, t := range topics {
				each = append(each, []string{t})
			}
		}
		for _, ts := range each {
			w.line(1, "- group: "+yamlString(g))
			if !p.mg.includeSubgroups {
				w.line(2, "subgroups: false")
			}
			if !p.mg.skipForks {
				w.line(2, "forks: true")
			}
			if len(ts) > 0 {
				w.line(2, "topics: ["+yamlString(ts[0])+"]")
			}
		}
	}
	if len(topics) > 0 && len(p.projects) > 0 {
		w.comment(1, "multi-gitter took these projects only with one of the topics "+quoteList(topics)+"; repo entries take no topics.")
	}
	for _, r := range p.projects {
		w.line(1, "- repo: "+yamlString(r))
	}
	w.comment(0, "TODO: packs: which packs every target gets (defaults.packs, packs per entry); the opt-in files add theirs.")
	if len(p.exclude) > 0 {
		w.line(0, "exclude:")
		for _, r := range p.exclude {
			w.line(1, "- "+yamlString(r))
		}
	}
	return w.b.String()
}

func (p *migration) operationsYML(now time.Time) string {
	until := now.UTC().AddDate(0, 0, adoptDays).Format("2006-01-02")
	var w yamlWriter
	w.line(0, "version: 1")
	w.comment(0, "Take over multi-gitter's open merge requests on the alias: distribute adds its marker")
	w.comment(0, "and its commit at the first write. Remove the entry once the move is over.")
	w.line(0, "adopt_unmarked:")
	w.line(1, "until: "+until)
	return w.b.String()
}

// plainRe matches strings YAML reads back as the same string when written
// plain.
var plainRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_./\-]*$`)

// yamlString writes s as a YAML scalar: plain when that reads back as the
// same string, else double-quoted (a JSON string is a valid YAML one).
func yamlString(s string) string {
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "y", "n":
		return jsonString(s)
	}
	if plainRe.MatchString(s) {
		return s
	}
	return jsonString(s)
}

// jsonString quotes s as JSON.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return strconv.Quote(s)
	}
	return string(b)
}

// quoteList quotes items for a comment.
func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

// oneLine keeps a message on one line, for a YAML comment.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
