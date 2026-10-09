package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// planDriver builds the read driver of one provider from its configuration,
// its read credential and an HTTP client that sends credentials only to the
// hosts they belong to. A zero Credential reads anonymously: plan gives no
// credential to a provider that a hub pull request adds or changes.
type planDriver func(config.ResolvedProvider, auth.Credential, *httpx.Client) (platform.Reader, error)

// The drivers of plan by provider type, and a snapshot source that replaces
// plan's own. This build has the Gitea and Forgejo, GitLab, GitHub,
// Bitbucket Cloud and Azure DevOps drivers (drivers.go). plan's own source is distribute's:
// a private git repository per target (sharedDeps), so that plan and
// distribute --dry-run decide alike. Tests install the fake platform here; a planSnapshots that
// is only a snapshot.Source (the fake in memory mode) keeps the in-memory
// plan (distribute.Plan). Tests that install them must not run in
// parallel.
var (
	planDrivers   = map[string]planDriver{}
	planSnapshots snapshot.Source
)

// gitVersion returns the version of the git on PATH, which plan and
// distribute run in the targets' repositories. Tests replace it.
var gitVersion = func(ctx context.Context) ([3]int, error) { return gitx.New("").Version(ctx) }

// basicUsers are the users a token goes with in HTTP Basic headers (GitHub,
// GitLab, Gitea and Bitbucket git over HTTPS, GitLab's job token, Azure
// DevOps' REST API with an empty user and its git with "touchmark"): the
// base64 forms of "user:token" are masked too.
var basicUsers = []string{"x-access-token", "oauth2", hubch.GitLabUser, "x-bitbucket-api-token-auth", "", "touchmark"}

// Suffixes of the credential variables after a provider's prefix.
var (
	readVars  = []string{"READ_TOKEN", "READ_APP_ID", "READ_APP_KEY"}
	writeVars = []string{"WRITE_TOKEN", "WRITE_APP_ID", "WRITE_APP_KEY", "SIGNING_KEY"}
)

// writeVarRe matches, uppercased, the name of a write credential or signing
// key of any provider id, or of the short form.
var writeVarRe = regexp.MustCompile(`^TOUCHMARK_(?:[A-Z0-9_]+_)?(?:WRITE_(?:TOKEN|APP_ID|APP_KEY)|SIGNING_KEY)$`)

// shortEnvPrefix starts the short variable names a hub with one provider
// accepts.
const shortEnvPrefix = "TOUCHMARK_"

func planFlags() (*flagSet, *options) {
	f, o := newFlagSet("plan")
	f.hubFlags()
	f.fs.Var(&o.only, "only", "plan only the target `REF`, as [PROVIDER:]PATH; repeatable or comma-separated")
	f.fs.Var(&o.hubFP, "hub-fp", "the hub's fingerprint `HOST/ID`: the host (with a port other than 443) of its server URL and its repository id, e.g. github.com/712345678 (CI provides it)")
	f.fs.BoolVar(&o.strict, "strict", false, "exit 3 when a target is blocked or deferred or targets.yml does not fully resolve")
	f.fs.StringVar(&o.format, "format", formatText, "output `FORMAT`: text, json or markdown")
	f.formats = []string{formatText, formatJSON, formatMarkdown}
	return f, o
}

// runPlan resolves the targets through each provider's read account and
// reports what distribute would do, writing nothing.
//
// It loads the hub like status (configs from HEAD, the manifest, the packs)
// and refuses a hub that fails check, a hub without an id, a hub whose
// fingerprint is unknown (CI provides it; --hub-fp outside CI), a CI job
// that sees a write credential or signing key, a provider without a driver
// in this build and a provider without a read credential: all exit 2.
//
// A CI run that builds anything but the hub's default branch (a hub pull
// request, another branch, a tag) runs code nobody reviewed yet: its
// providers and their ca_file come from the default branch's hub.yml, and
// a provider it adds or whose endpoints it changes is planned without a
// credential (planProviders); its sensitive paths are the
// default branch's and its own (unionSensitive). The credentials are
// masked in everything plan prints, and on GitHub Actions registered with
// ::add-mask:: first. The exit code is the report's (report.ExitCode).
//
// A hub pull request without any read secret (a pull request from
// Dependabot or from a fork, which get no Actions secrets) is planned
// offline (offlinePlan): no provider is reached, the report
// shows the hub's side and warns that no target was checked; the resolve is
// incomplete, so the exit code is 0, or 3 with --strict.
//
// In a hub pull request the plan processes only the targets of the packs
// the pull request changes (planScope; every target with --all), and with
// --comment keeps its report in one comment of the pull request
// (postPlanComment). --assume-opt-in plans every target without an opt-in
// file as opted in. In CI it leaves the outputs distribute does (publish):
// the step summary on GitHub Actions and Gitea, annotations of the first
// failed and blocked targets on GitHub Actions, and
// touchmark-report.{json,md} in the working directory for the job's
// artifact.
func runPlan(ctx context.Context, e *env, o *options, p *planOptions) (err error) {
	h, err := loadPlanHub(ctx, e, o)
	if err != nil {
		return err
	}
	hctx := hubch.Detect(e.getenv, os.ReadFile)
	hctx, repoWarnings := hubRepository(ctx, hctx, e.getenv)
	fp, warnings, err := planFingerprint(hctx, string(o.hubFP))
	if err != nil {
		return err
	}
	warnings = append(warnings, repoWarnings...)
	rps, err := h.resolveProviders(ctx, hctx, e.getenv)
	if err != nil {
		return configError(err)
	}
	if err := writeGuard(hctx, rps, e); err != nil {
		return err
	}
	pps, trustWarnings := h.planProviders(ctx, hctx, e.getenv, rps)
	warnings = append(warnings, trustWarnings...)
	offline := offlinePlan(hctx, pps, e.getenv)
	if offline {
		for i := range pps {
			pps[i].anonymous = true
		}
		warnings = append(warnings, offlineWarning)
	} else if planSnapshots == nil {
		// plan reads every target into a repository of its own, as
		// distribute does (sharedDeps), and needs the same git. An offline
		// plan reads no target, and a test's snapshot source runs no git.
		v, err := gitVersion(ctx)
		if err != nil {
			return configError(err)
		}
		if err := distribute.GitGuard("plan", v); err != nil {
			return configError(err)
		}
	}
	h.unionSensitive(ctx, hctx)
	drivers := make([]planDriver, len(pps))
	for i, pp := range pps {
		d := planDrivers[pp.Type]
		if d == nil {
			return configErrorf("provider %s: no %s driver in this build", pp.ID, pp.Type)
		}
		drivers[i] = d
	}
	reg := redact.New()
	defer func() { err = maskError(reg, err) }()
	creds, err := readCredentials(pps, e.getenv, reg)
	if err != nil {
		return err
	}
	token := hubch.Token(hctx, e.getenv)
	reg.Add(token, basicUsers...)
	if hctx.CI == hubch.GitHubActions {
		if err := reg.GitHubMask(e.stderr); err != nil {
			return err
		}
		maskNewSecrets(reg, e.stderr)
	}
	providers := make([]distribute.Provider, len(pps))
	var closers []io.Closer
	defer func() { closeDrivers(closers) }()
	for i, pp := range pps {
		client, err := h.providerClient(ctx, pp, reg)
		if err != nil {
			return configErrorf("provider %s: %w", pp.ID, err)
		}
		var reader platform.Reader = offlineReader{}
		if !offline {
			if reader, err = drivers[i](pp.ResolvedProvider, creds[i], client); err != nil {
				return configErrorf("provider %s: %w", pp.ID, err)
			}
		}
		if c, ok := reader.(io.Closer); ok {
			closers = append(closers, c)
		}
		providers[i] = distribute.Provider{Config: pp.ResolvedProvider, Reader: reader, Anonymous: pp.anonymous}
	}
	channel := h.hubChannel(ctx, hctx, pps, token, reg)
	hctx, visWarnings := channelVisibility(ctx, hctx, channel)
	warnings = append(warnings, visWarnings...)
	// A hub pull request's plan reads the default branch's tip and keeps
	// its comment through the hub channel, which the head guard does not
	// need.
	var prChannel hubch.Channel
	var tip func(context.Context) (string, error)
	if pullRequestRun(hctx) {
		prChannel = h.newChannel(ctx, hctx, pps, token, reg)
		tip = prChannel.Head
	}
	scope, scopeWarnings := h.planScope(ctx, hctx, e.getenv, p, tip)
	warnings = append(warnings, scopeWarnings...)
	shared, err := h.sharedDeps(ctx, hctx, pps, e.getenv)
	if err != nil {
		return err
	}
	defer shared.close()
	write := shared.write
	write.Redact = reg
	var snapshots snapshot.Source = shared.repos
	if planSnapshots != nil {
		snapshots = planSnapshots
	}
	rep, err := distribute.Plan(ctx, distribute.Deps{
		Hub:         h.cfg,
		Targets:     h.targets,
		Manifest:    h.manifest,
		Current:     h.current,
		Known:       h.known(),
		HubCommit:   h.commit,
		HubContext:  hctx,
		InCI:        inCI(hctx, e.getenv),
		Fingerprint: fp,
		Channel:     channel,
		Providers:   providers,
		Snapshots:   snapshots,
		Only:        o.only,
		Strict:      o.strict,
		Engine:      version(),
		Scope:       scope,
		AssumeOptIn: p.assumeOptIn,
		Write:       write,
	})
	if errors.Is(err, distribute.ErrReaderIsWriter) {
		return configError(err)
	}
	if err != nil {
		return err
	}
	rep.Warnings = nonNil(slices.Concat(h.warnings, warnings, rep.Warnings))
	rep.Hub.PR = hubPR(hctx, e.getenv)
	maskDelivery(reg, rep)
	if p.comment {
		if w := postPlanComment(ctx, hctx, prChannel, rep.Hub.PR, rep, reg); w != "" {
			rep.Warnings = append(rep.Warnings, reg.Replace(w))
		}
	}
	// The outputs of distribute's CI: the step summary,
	// the annotations and the report files of the job.
	return publishDelivery(e, hctx, reg, rep, o.format, "")
}

// loadPlanHub opens the hub, reads its configs, operations, history and
// packs, and refuses one that fails check or has no id.
func loadPlanHub(ctx context.Context, e *env, o *options) (*hub, error) {
	h, err := openHub(ctx, e, o.hub, o.worktree)
	if err != nil {
		return nil, err
	}
	if errs := h.readConfigs(ctx, e); len(errs) > 0 {
		return nil, configErrorf("%w\nrun touchmark check for details", joinedError(errors.Join(errs...)))
	}
	if h.cfg.Legacy || h.cfg.ID == "" {
		return nil, configErrorf("%s has no id: the sync branch and its pull requests are named after it; add an id to %s", config.HubFile, config.HubFile)
	}
	if err := h.build(ctx); err != nil {
		return nil, err
	}
	if err := h.readPacks(ctx); err != nil {
		return nil, err
	}
	msgs := h.checkErrors()
	if err := h.readOperations(ctx); err != nil {
		msgs = append(msgs, flatten(err)...)
	} else if h.ops != nil {
		_, errs := config.CheckOperations(h.ops, h.targets, h.cfg, time.Now())
		for _, err := range errs {
			msgs = append(msgs, flatten(err)...)
		}
	}
	if len(msgs) > 0 {
		return nil, configErrorf("the hub fails touchmark check:\n  %s\nrun touchmark check for details", strings.Join(msgs, "\n  "))
	}
	return h, nil
}

// planFingerprint returns the hub's fingerprint: --hub-fp, else the CI's.
// A --hub-fp that differs from the CI's is a warning; no fingerprint at all
// is a configuration error.
func planFingerprint(hctx hubch.Context, flag string) (string, []string, error) {
	ci := hctx.Fingerprint()
	var warnings []string
	switch {
	case flag == "":
		flag = ci
	case ci != "" && ci != flag:
		warnings = append(warnings, fmt.Sprintf("--hub-fp %s differs from %s, the fingerprint CI reports", flag, ci))
	}
	if flag == "" {
		return "", nil, configErrorf("the hub's fingerprint is unknown: CI provides it (GITHUB_REPOSITORY_ID, CI_PROJECT_ID, BITBUCKET_REPO_UUID); " +
			"outside CI pass --hub-fp HOST/ID, the hub's host and repository id, e.g. github.com/712345678 " +
			"(on Bitbucket the repository's UUID, BITBUCKET_REPO_UUID: bitbucket.org/{…})")
	}
	return flag, warnings, nil
}

// inCI reports whether plan runs in CI: a CI hubch recognizes, or any other
// that sets CI.
func inCI(hctx hubch.Context, getenv func(string) string) bool {
	if hctx.CI != hubch.Local && hctx.CI != "" {
		return true
	}
	v := strings.ToLower(strings.TrimSpace(getenv("CI")))
	return v != "" && v != "false" && v != "0"
}

// writeGuard refuses a CI job of plan that can see a write credential or a
// signing key: the job runs for every branch of the hub,
// so what it sees, anyone who can push a branch sees. Any variable named
// like one counts, whichever provider id it carries (a renamed or removed
// provider's, or the short form), and so do the names of the providers of
// rps looked up directly.
func writeGuard(hctx hubch.Context, rps []config.ResolvedProvider, e *env) error {
	if !inCI(hctx, e.getenv) {
		return nil
	}
	var names []string
	if e.environ != nil {
		for _, kv := range e.environ() {
			name, value, _ := strings.Cut(kv, "=")
			if writeVarRe.MatchString(strings.ToUpper(name)) && strings.TrimSpace(value) != "" {
				names = append(names, name)
			}
		}
	}
	prefixes := []string{shortEnvPrefix}
	for _, rp := range rps {
		prefixes = append(prefixes, rp.EnvPrefix)
	}
	for _, prefix := range prefixes {
		for _, suffix := range writeVars {
			if name := prefix + suffix; strings.TrimSpace(e.getenv(name)) != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	names = slices.Compact(names)
	verb := "is"
	if len(names) > 1 {
		verb = "are"
	}
	return configErrorf("%s %s set: plan reads with the read credential only, and its CI job must not see write credentials or signing keys; "+
		"keep them in the protected environment of the distribute job", strings.Join(names, ", "), verb)
}

// planProvider is a provider as plan reads it: its configuration, whether
// it is read without a credential, and the hub commit its ca_file is read
// at ("" reads it like the other configs: from HEAD, or the work tree with
// --worktree).
type planProvider struct {
	config.ResolvedProvider
	anonymous bool
	caRev     string
}

// resolveProviders resolves the providers of the hub.yml plan read. Outside
// CI, a hub without providers takes its provider from the host of its
// origin remote (config.Hub.ResolveProvidersWithOrigin).
func (h *hub) resolveProviders(ctx context.Context, hctx hubch.Context, getenv func(string) string) ([]config.ResolvedProvider, error) {
	origin := ""
	if len(h.cfg.Providers) == 0 && h.cfg.Platform == "" && (hctx.CI == hubch.Local || hctx.CI == "") {
		origin = h.originHost(ctx)
	}
	return h.cfg.ResolveProvidersWithOrigin(getenv, origin)
}

// originHost returns the host of the hub's origin remote, "" when there is
// none or it is not a URL touchmark understands.
func (h *hub) originHost(ctx context.Context) string {
	out, err := h.git.Run(ctx, nil, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	// The URL may hold credentials: only its host is kept.
	host, _, ok := parseRemote(strings.TrimSpace(string(out)))
	if !ok {
		return ""
	}
	return host
}

// untrustedRun reports whether a CI run builds something other than the
// hub's default branch: a pull or merge request, another branch or a tag.
// Its hub.yml has not been reviewed, so it must not decide where a
// credential goes: anyone who can push a branch to the hub could send the
// read credential to a host of their choice.
func untrustedRun(hctx hubch.Context) bool {
	if hctx.CI == hubch.Local || hctx.CI == "" {
		return false
	}
	ev := hctx.Event
	if strings.HasPrefix(ev, "pull_request") || ev == "merge_request_event" || ev == "external_pull_request_event" {
		return true
	}
	return !distribute.HeadGuard(hctx)
}

// planProviders decides how plan reads each provider of rps, the providers
// of the hub.yml it read. Outside an untrusted run (untrustedRun) every
// provider is read as configured, with its credential.
//
// In an untrusted run the providers and ca_file come from the hub.yml of
// the default branch's tip (refs/remotes/origin/<branch>, else
// refs/heads/<branch>), matched by id: a provider whose
// type, URL, API and GraphQL URLs and ca_file path are the same there is
// read with its credential and the default branch's configuration, its CA
// bundle read at that commit, and with the run's writer only where the
// default branch names none (a new hub's first pull request, which names
// it); one the run adds, or whose endpoints differ,
// is read anonymously with the run's configuration, with a warning. When
// the default branch's providers cannot be read, every provider is read
// anonymously, with a warning.
func (h *hub) planProviders(ctx context.Context, hctx hubch.Context, getenv func(string) string, rps []config.ResolvedProvider) ([]planProvider, []string) {
	out := make([]planProvider, len(rps))
	for i, rp := range rps {
		out[i] = planProvider{ResolvedProvider: rp}
	}
	if !untrustedRun(hctx) {
		return out, nil
	}
	base, rev, err := h.defaultBranchProviders(ctx, hctx, getenv)
	if err != nil {
		for i := range out {
			out[i].anonymous = true
		}
		return out, []string{fmt.Sprintf("every provider is planned without credentials: this run builds a hub pull request or a branch other than the default, "+
			"whose providers come from the default branch, and %v", err)}
	}
	var warnings []string
	for i, rp := range rps {
		b, ok := base[rp.ID]
		switch {
		case !ok:
			out[i].anonymous = true
			warnings = append(warnings, fmt.Sprintf("provider %s is not in the default branch's %s: planned without credentials until it is merged", rp.ID, config.HubFile))
		case !sameEndpoints(b, rp):
			out[i].anonymous = true
			warnings = append(warnings, fmt.Sprintf("provider %s: this run's type, url, api_url or ca_file differs from the default branch's: planned without credentials until it is merged", rp.ID))
		default:
			out[i] = planProvider{ResolvedProvider: b, caRev: rev}
			if b.Writer == "" && rp.Writer != "" {
				// A new hub's first pull request names the writer (the
				// template's default branch has none). The writer decides no
				// destination of a credential, only which pull requests
				// plan counts as touchmark's: without it plan could not
				// recognize them, nor sweep, and --strict would fail every
				// such pull request.
				out[i].Writer = rp.Writer
				warnings = append(warnings, fmt.Sprintf("provider %s: the default branch's %s names no writer: plan takes this run's, %s",
					rp.ID, config.HubFile, rp.Writer))
			}
			if !reflect.DeepEqual(out[i].Provider, rp.Provider) {
				warnings = append(warnings, fmt.Sprintf("provider %s: this run changes its settings; plan uses the default branch's until it is merged", rp.ID))
			}
		}
	}
	return out, warnings
}

// sameEndpoints reports whether a and b send requests to the same places
// with the same trust: type, URL, host, API and GraphQL URLs, ca_file.
func sameEndpoints(a, b config.ResolvedProvider) bool {
	return a.Type == b.Type && a.URL == b.URL && a.Host == b.Host && a.APIURL == b.APIURL &&
		a.GraphQLURL == b.GraphQLURL && a.CAFile == b.CAFile
}

// defaultBranchProviders returns, by id, the providers of hub.yml at the tip
// of the hub's default branch in this clone, and that commit.
func (h *hub) defaultBranchProviders(ctx context.Context, hctx hubch.Context, getenv func(string) string) (map[string]config.ResolvedProvider, string, error) {
	rev, err := h.defaultBranchTip(ctx, hctx)
	if err != nil {
		return nil, "", err
	}
	out, err := h.providersAt(ctx, rev, getenv)
	return out, rev, err
}

// defaultBranchTip returns the tip of the hub's default branch in this
// clone: refs/remotes/origin/<branch>, else refs/heads/<branch>.
func (h *hub) defaultBranchTip(ctx context.Context, hctx hubch.Context) (string, error) {
	branch := hctx.DefaultBranch
	switch {
	case branch == "":
		return "", errors.New("the CI does not name the hub's default branch")
	case !safeBranch(branch):
		return "", fmt.Errorf("the default branch %q is not a branch name touchmark reads", branch)
	}
	for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
		if oid, err := h.git.RevParse(ctx, ref+"^{commit}"); err == nil {
			return oid, nil
		}
	}
	return "", fmt.Errorf("the default branch %s is not in this clone (neither refs/remotes/origin/%s nor refs/heads/%s): "+
		"fetch it, e.g. git fetch origin %s:refs/remotes/origin/%s", branch, branch, branch, branch, branch)
}

// hubAt parses hub.yml at commit rev (the default branch's tip).
func (h *hub) hubAt(ctx context.Context, rev string) (*config.Hub, error) {
	data, err := h.git.ShowFile(ctx, rev, config.HubFile)
	if err != nil && !errors.Is(err, gitx.ErrNotFound) {
		return nil, fmt.Errorf("the default branch's %s cannot be read: %w", config.HubFile, err)
	}
	cfg, _, err := config.ParseHub(data)
	if err != nil {
		return nil, fmt.Errorf("the default branch's %s does not parse", config.HubFile)
	}
	return cfg, nil
}

// unionSensitive makes the sensitive paths of an untrusted run (a hub pull
// request, another branch, a tag) the union of the default branch's
// sensitive_paths and the run's own, the default branch's
// first: a pull request that drops a pattern cannot hide a change to its
// paths from the ⚠ marks of the plan in the same pull request. When the
// default branch's hub.yml cannot be read, the run's own list stays
// (planProviders warns about that branch already).
func (h *hub) unionSensitive(ctx context.Context, hctx hubch.Context) {
	if !untrustedRun(hctx) || h.cfg == nil {
		return
	}
	rev, err := h.defaultBranchTip(ctx, hctx)
	if err != nil {
		return
	}
	base, err := h.hubAt(ctx, rev)
	if err != nil {
		return
	}
	union := slices.Clone(base.SensitivePaths)
	for _, p := range h.cfg.SensitivePaths {
		if !slices.Contains(union, p) {
			union = append(union, p)
		}
	}
	if slices.Equal(union, h.cfg.SensitivePaths) {
		return
	}
	cfg := *h.cfg
	cfg.SensitivePaths = union
	h.cfg = &cfg
}

// providersAt returns, by id, the providers of hub.yml at commit rev.
func (h *hub) providersAt(ctx context.Context, rev string, getenv func(string) string) (map[string]config.ResolvedProvider, error) {
	cfg, err := h.hubAt(ctx, rev)
	if err != nil {
		return nil, err
	}
	rps, err := cfg.ResolveProviders(getenv)
	if err != nil {
		return nil, fmt.Errorf("the default branch's providers do not resolve: %w", err)
	}
	out := make(map[string]config.ResolvedProvider, len(rps))
	for _, rp := range rps {
		out[rp.ID] = rp
	}
	return out, nil
}

// safeBranch reports whether b can name a branch in a revision without
// changing its meaning.
func safeBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.Contains(b, "..") || strings.Contains(b, "@{") {
		return false
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}

// readCredentials reads the read credential of every provider that is not
// anonymous, registers its secrets with reg, and removes the read variables
// of every provider from the process environment, so that
// no child process inherits them. An anonymous provider gets the zero
// Credential and never an error.
func readCredentials(pps []planProvider, getenv func(string) string, reg *redact.Registry) ([]auth.Credential, error) {
	creds := make([]auth.Credential, len(pps))
	var read []string
	for i, pp := range pps {
		prefixes := []string{pp.EnvPrefix}
		if pp.Short {
			prefixes = append(prefixes, shortEnvPrefix)
		}
		for _, prefix := range prefixes {
			for _, suffix := range readVars {
				if name := prefix + suffix; getenv(name) != "" {
					read = append(read, name)
				}
			}
		}
		if pp.anonymous {
			continue
		}
		c, ok, err := auth.FromEnv(pp.EnvPrefix, pp.Short, auth.Read, getenv)
		if err != nil {
			return nil, configErrorf("provider %s: %w", pp.ID, err)
		}
		if !ok {
			p := pp.EnvPrefix
			return nil, configErrorf("provider %s: no read credential: set %sREAD_TOKEN, or %sREAD_APP_ID and %sREAD_APP_KEY", pp.ID, p, p, p)
		}
		for _, s := range c.Secrets() {
			reg.Add(s, basicUsers...)
		}
		creds[i] = c
	}
	for _, name := range read {
		// The value was read, or is not to be used; failing to unset it
		// leaves it where it was, which is no reason to stop.
		_ = os.Unsetenv(name)
	}
	return creds, nil
}

// providerClient returns the HTTP client of a provider, trusting the
// certificates of its ca_file (read from the hub at pp.caRev) besides the
// system roots.
func (h *hub) providerClient(ctx context.Context, pp planProvider, reg *redact.Registry) (*httpx.Client, error) {
	opts := httpx.Options{Redact: reg}
	if pp.CAFile != "" {
		pool, err := h.caPool(ctx, pp.CAFile, pp.caRev)
		if err != nil {
			return nil, fmt.Errorf("ca_file %s: %w", pp.CAFile, err)
		}
		opts.RootCAs = pool
	}
	return httpx.New(opts), nil
}

// caPool reads a CA bundle from the hub into a pool with the system roots:
// at commit rev, or like the other configs when rev is "" (its HEAD, or its
// work tree with --worktree).
func (h *hub) caPool(ctx context.Context, name, rev string) (*x509.CertPool, error) {
	var data []byte
	var err error
	if rev == "" {
		data, err = h.readConfig(ctx, name)
	} else if data, err = h.git.ShowFile(ctx, rev, name); errors.Is(err, gitx.ErrNotFound) {
		data, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("no such file in the hub")
	}
	f, err := os.CreateTemp("", "touchmark-ca-*.pem")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		return nil, err
	}
	return httpx.LoadCAFile(f.Name())
}

// hubPR returns the number of the hub pull request a CI run builds: from
// GITHUB_REF (refs/pull/<n>/merge) on Actions, CI_MERGE_REQUEST_IID on
// GitLab, BITBUCKET_PR_ID on Bitbucket Pipelines; 0 when the run builds
// none.
func hubPR(hctx hubch.Context, getenv func(string) string) int64 {
	var n string
	switch hctx.CI {
	case hubch.GitHubActions, hubch.GiteaActions, hubch.ForgejoActions:
		rest, ok := strings.CutPrefix(strings.TrimSpace(getenv("GITHUB_REF")), "refs/pull/")
		if !ok {
			return 0
		}
		n, _, _ = strings.Cut(rest, "/")
	case hubch.GitLabCI:
		n = strings.TrimSpace(getenv("CI_MERGE_REQUEST_IID"))
	case hubch.BitbucketPipelines:
		n = strings.TrimSpace(getenv("BITBUCKET_PR_ID"))
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// printDelivery writes the report in format.
func printDelivery(w io.Writer, rep *report.Delivery, format string) error {
	switch format {
	case formatJSON:
		return report.WriteJSON(w, rep)
	case formatMarkdown:
		return rep.WriteMarkdown(w)
	}
	return rep.WriteText(w)
}

// maskDelivery masks the registered secrets in the free text of a report:
// warnings and provider errors quote platform messages. Masking the data
// before it is rendered matters for Markdown and JSON, whose escapes would
// hide a secret from the masking writer.
func maskDelivery(reg *redact.Registry, rep *report.Delivery) {
	mask := func(list []string) {
		for i, s := range list {
			list[i] = reg.Replace(s)
		}
	}
	mask(rep.Warnings)
	for i := range rep.Providers {
		p := &rep.Providers[i]
		p.Error = reg.Replace(p.Error)
		mask(p.Missing)
	}
	for i := range rep.Targets {
		mask(rep.Targets[i].Warnings)
	}
	for k, v := range rep.Notes {
		rep.Notes[k] = reg.Replace(v)
	}
}

// maskError returns err with every registered secret masked, keeping its
// exit code; err itself when it holds none.
func maskError(reg *redact.Registry, err error) error {
	if err == nil || !reg.Contains(err.Error()) {
		return err
	}
	var ce *cliError
	if errors.As(err, &ce) {
		return &cliError{code: ce.code, usage: ce.usage, err: errors.New(reg.Replace(err.Error()))}
	}
	return errors.New(reg.Replace(err.Error()))
}

// refList is the value of --only: targets as [PROVIDER:]PATH, comma-separated;
// the flag may repeat.
type refList []config.Ref

func (l *refList) String() string {
	parts := make([]string, len(*l))
	for i, r := range *l {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

func (l *refList) Set(s string) error {
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return errors.New("empty target")
		}
		ref, err := config.ParseRef(part)
		if err != nil {
			return err
		}
		*l = append(*l, ref)
	}
	return nil
}

// fingerprintRe is a hub fingerprint: a host with an optional port, and a
// positive repository id or a Bitbucket repository UUID, with or without
// its braces (BITBUCKET_REPO_UUID has them).
var fingerprintRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?/([1-9][0-9]{0,19}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}|\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\})$`)

// fingerprintFlag is the value of --hub-fp: HOST[:PORT]/ID in the form CI
// reports it (config.CanonicalFingerprint: the host lowercased, without the
// default ports 443 and 80; a Bitbucket UUID lowercased, without braces).
type fingerprintFlag string

func (f *fingerprintFlag) String() string { return string(*f) }

func (f *fingerprintFlag) Set(s string) error {
	s = strings.TrimSpace(s)
	if !fingerprintRe.MatchString(s) {
		return fmt.Errorf("%q is not HOST/ID, like github.com/712345678 or bitbucket.org/{repository-uuid}", s)
	}
	*f = fingerprintFlag(config.CanonicalFingerprint(s))
	return nil
}
