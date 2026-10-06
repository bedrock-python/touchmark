package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// hubTokenEnv carries the maintainer's token of doctor --hub-token: tokens
// never go on the command line.
const hubTokenEnv = "TOUCHMARK_HUB_TOKEN"

// doctorOptions are the flags of doctor beyond those it shares with plan.
type doctorOptions struct {
	reportFile string
	hubToken   bool
}

// doctorCommand is the doctor command.
func doctorCommand() *command {
	d := &doctorOptions{}
	return &command{
		name:     "doctor",
		synopsis: "[--hub DIR] [--only REF]... [--hub-fp HOST/ID] [--strict] [--format text|json|markdown] [--report FILE] [--hub-token]",
		summary:  "check the write identity against every target, and with --hub-token where the hub keeps its write key; changes nothing",
		flags: func() (*flagSet, *options) {
			*d = doctorOptions{}
			return doctorFlags(d)
		},
		run: func(ctx context.Context, e *env, o *options) error { return runDoctor(ctx, e, o, d) },
	}
}

func doctorFlags(d *doctorOptions) (*flagSet, *options) {
	f, o := newFlagSet("doctor")
	f.fs.StringVar(&o.hub, "hub", "", "the hub checkout `DIR` (default $TOUCHMARK_HUB)")
	f.fs.Var(&o.only, "only", "check only the target `REF`, as [PROVIDER:]PATH; repeatable or comma-separated")
	f.fs.Var(&o.hubFP, "hub-fp", "the hub's fingerprint `HOST/ID`, e.g. github.com/712345678 (CI provides it)")
	f.fs.BoolVar(&o.strict, "strict", false, "exit 3 when a check warns or is unknown, or a provider's targets could not all be listed")
	f.fs.StringVar(&o.format, "format", formatText, "output `FORMAT`: text, json or markdown")
	f.fs.StringVar(&d.reportFile, "report", "", "also write the JSON report to `FILE`")
	f.fs.BoolVar(&d.hubToken, "hub-token", false, "local only: read where the hub keeps its secrets with a maintainer's token of the hub, from $"+hubTokenEnv+"; needs no write key")
	f.formats = []string{formatText, formatJSON, formatMarkdown}
	return f, o
}

// runDoctor runs doctor: the checks of each provider's write identity against
// every target of targets.yml (distribute.Doctor), and of the hub's write
// isolation; with --hub-token, run locally by a maintainer, also where the
// hub keeps its write key. It writes nothing to any platform.
//
// It reads hub.yml and targets.yml from the hub's HEAD (a hub that fails
// their checks, has no id or no fingerprint is refused, exit 2), and runs
// in CI only where distribute may (distribute.DoctorGuard: the default
// branch, no pull request, GitLab's protected environment): it holds the
// write credentials, which it reads, masks and removes from the environment
// as distribute does. --hub-token is refused in CI; the maintainer's token
// comes from TOUCHMARK_HUB_TOKEN, is masked, and goes to the hub's own API
// host only (keyLocation). It needs no write key: without every provider's
// write credential, doctor --hub-token checks the hub only, and says that
// the writer and target checks are the CI's (a write key under isolation
// lives in the CI only). In CI the hub's visibility comes
// from the CI's event or, on a schedule, through the hub channel: a public
// hub (or one of unknown visibility) names no non-public target in any
// output.
//
// It prints the report in --format, writes --report, and in CI the report
// files touchmark-doctor.{json,md}, the step summary and annotations
// (publish), and exits with the report's code: 1 when a check failed or a
// provider could not be checked, 3 with --strict when a check warns or is
// unknown.
func runDoctor(ctx context.Context, e *env, o *options, d *doctorOptions) (err error) {
	h, err := loadDoctorHub(ctx, e, o)
	if err != nil {
		return err
	}
	hctx := hubch.Detect(e.getenv, os.ReadFile)
	fp, warnings, err := planFingerprint(hctx, string(o.hubFP))
	if err != nil {
		return err
	}
	ci := inCI(hctx, e.getenv)
	if d.hubToken && ci {
		return configErrorf("--hub-token is for a maintainer's local run: a CI job must not hold a maintainer's token of the hub")
	}
	if err := distribute.DoctorGuard(hctx, e.getenv); err != nil {
		return configError(joinedError(err))
	}
	reg := redact.New()
	defer func() { err = maskError(reg, err) }()
	token := hubch.Token(hctx, e.getenv)
	reg.Add(token, basicUsers...)
	rps, err := h.resolveProviders(ctx, hctx, e.getenv)
	if err != nil {
		return configError(err)
	}
	drivers := make([]distributeDriver, len(rps))
	for i, rp := range rps {
		if drivers[i] = distributeDrivers[rp.Type]; drivers[i] == nil {
			return configErrorf("provider %s: no %s driver in this build", rp.ID, rp.Type)
		}
	}
	hubToken := ""
	if d.hubToken {
		hubToken = strings.TrimSpace(e.getenv(hubTokenEnv))
		// Failing to unset leaves the value where it was: no reason to stop.
		_ = os.Unsetenv(hubTokenEnv)
		if hubToken == "" {
			return configErrorf("--hub-token: set %s to a maintainer's token of the hub", hubTokenEnv)
		}
		reg.Add(hubToken, basicUsers...)
	}
	secrets, err := readSecrets(rps, e.getenv, reg, d.hubToken)
	if err != nil {
		return err
	}
	if hctx.CI == hubch.GitHubActions {
		if err := reg.GitHubMask(e.stderr); err != nil {
			return err
		}
		maskNewSecrets(reg, e.stderr)
	}
	pps := make([]planProvider, len(rps))
	for i, rp := range rps {
		pps[i] = planProvider{ResolvedProvider: rp}
	}
	channel := h.hubChannel(ctx, hctx, pps, token, reg)
	hctx, visWarnings := channelVisibility(ctx, hctx, channel)
	warnings = append(warnings, visWarnings...)
	var doc *report.Doctor
	if len(secrets.missing) > 0 {
		// doctor --hub-token without the write keys: the hub's checks only.
		doc = report.NewDoctor(version())
		doc.Strict = o.strict
		doc.Hub = report.DeliveryHub{ID: h.cfg.ID, Fingerprint: config.CanonicalFingerprint(fp), Commit: h.commit}
		warnings = append(warnings, fmt.Sprintf("doctor --hub-token checked the hub only: provider %s has no write credential in this run, "+
			"and the checks of the writers and the targets need every provider's; the doctor job of the hub's CI makes them "+
			"(--hub-token needs no write key)", strings.Join(secrets.missing, ", ")))
	} else {
		providers, closers, err := h.doctorWriters(ctx, rps, drivers, secrets, reg)
		defer closeDrivers(closers)
		if err != nil {
			return err
		}
		hubHost, hubPath := h.doctorHubRepo(ctx, hctx, fp)
		doc, err = distribute.Doctor(ctx, distribute.DoctorDeps{
			Deps: distribute.Deps{
				Hub:         h.cfg,
				Targets:     h.targets,
				Current:     h.current,
				Known:       h.known(),
				HubCommit:   h.commit,
				HubContext:  hctx,
				InCI:        ci,
				Fingerprint: fp,
				Providers:   providers,
				Only:        o.only,
				Strict:      o.strict,
				Engine:      version(),
				Write:       distribute.WriteDeps{Signers: secrets.signers, Redact: reg},
			},
			HubHost: hubHost,
			HubPath: hubPath,
		})
		if err != nil {
			return configError(err)
		}
	}
	doc.HubChecks = append(isolationChecks(ctx, hctx, h.cfg, e.getenv, channel), doc.HubChecks...)
	if d.hubToken {
		checks, err := h.keyLocation(ctx, e, rps, fp, hubToken, reg)
		if err != nil {
			return err
		}
		doc.HubToken = true
		doc.HubChecks = append(doc.HubChecks, checks...)
	}
	doc.Warnings = nonNil(slices.Concat(h.warnings, warnings, doc.Warnings))
	maskDoctor(reg, doc)
	doc.Summarize()
	return publishDoctor(e, hctx, reg, doc, o.format, d.reportFile)
}

// loadDoctorHub opens the hub and reads hub.yml, targets.yml and the packs
// of its HEAD; it refuses a hub whose configs fail check or that has no id.
// Unlike plan it reads no history: doctor decides nothing about files.
func loadDoctorHub(ctx context.Context, e *env, o *options) (*hub, error) {
	h, err := openHub(ctx, e, o.hub, false)
	if err != nil {
		return nil, err
	}
	if errs := h.readConfigs(ctx, e); len(errs) > 0 {
		return nil, configErrorf("%w\nrun touchmark check for details", joinedError(errors.Join(errs...)))
	}
	if h.cfg.Legacy || h.cfg.ID == "" {
		return nil, configErrorf("%s has no id: the sync branch and its pull requests are named after it; add an id to %s", config.HubFile, config.HubFile)
	}
	if err := h.readPacks(ctx); err != nil {
		return nil, err
	}
	if msgs := h.checkErrors(); len(msgs) > 0 {
		return nil, configErrorf("the hub fails touchmark check:\n  %s\nrun touchmark check for details", strings.Join(msgs, "\n  "))
	}
	return h, nil
}

// doctorWriters builds each provider's write driver over its HTTP client;
// closers are the drivers that hold credentials to release.
func (h *hub) doctorWriters(ctx context.Context, rps []config.ResolvedProvider, drivers []distributeDriver, secrets writeSecrets,
	reg *redact.Registry) ([]distribute.Provider, []io.Closer, error) {
	providers := make([]distribute.Provider, len(rps))
	var closers []io.Closer
	for i, rp := range rps {
		client, err := h.providerClient(ctx, planProvider{ResolvedProvider: rp}, reg)
		if err != nil {
			return nil, closers, configErrorf("provider %s: %w", rp.ID, err)
		}
		writer, err := drivers[i](rp, secrets.creds[i], client)
		if err != nil {
			return nil, closers, configErrorf("provider %s: %w", rp.ID, err)
		}
		if c, ok := writer.(io.Closer); ok {
			closers = append(closers, c)
		}
		providers[i] = distribute.Provider{Config: rp, Writer: writer}
	}
	return providers, closers, nil
}

// doctorHubRepo returns where the hub lives: the host of its fingerprint,
// and its path there (the CI's, else the origin remote's when it is on
// that host; "" when unknown).
func (h *hub) doctorHubRepo(ctx context.Context, hctx hubch.Context, fp string) (host, path string) {
	i := strings.LastIndexByte(fp, '/')
	if i <= 0 {
		return "", ""
	}
	host = strings.ToLower(fp[:i])
	name := host
	if hn, _, err := net.SplitHostPort(host); err == nil {
		name = hn
	}
	if hctx.RepoPath != "" && strings.EqualFold(hctx.Host, host) {
		return host, hctx.RepoPath
	}
	out, err := h.git.Run(ctx, nil, "remote", "get-url", "origin")
	if err != nil {
		return host, ""
	}
	// The URL may hold credentials: only its host and path are kept.
	if rh, rp, ok := parseRemote(strings.TrimSpace(string(out))); ok && strings.EqualFold(rh, name) {
		return host, rp
	}
	return host, ""
}

// isolationChecks are the hub's write-isolation checks of this job
// (distribute.IsolationChecks), with the environment touchmark-distribute
// read through the hub channel on GitHub Actions.
func isolationChecks(ctx context.Context, hctx hubch.Context, cfg *config.Hub, getenv func(string) string, ch hubch.Channel) []report.DoctorCheck {
	in := distribute.IsolationInput{Context: hctx, Hub: cfg, Getenv: getenv}
	if reader, ok := ch.(hubch.EnvironmentReader); ok && distribute.EnvironmentGuardApplies(hctx, cfg) {
		in.EnvironmentRead = true
		in.Environment, in.EnvironmentErr = reader.Environment(ctx, distribute.DistributeEnvironment)
	}
	return distribute.IsolationChecks(in)
}

// keyLocation reads where the hub keeps its secrets with the maintainer's
// token (hubch.ReadKeyStore) and grades it (distribute.KeyLocationChecks).
// The hub's platform and API are those of the provider on the hub's host,
// else github.com's and gitlab.com's own; the repository is the
// fingerprint's id. The secrets that carry write keys are named after
// touchmark's variables, or are those the hub's workflows hand it as such.
// A hub the token cannot read is an unknown check, not the end of the run.
//
// The token goes to the hub's own API only, whatever the hub.yml of this
// checkout says (a branch under review may say anything): the provider's
// API must be on the fingerprint's host (api.github.com for github.com,
// api.<host> for GHE.com), and a provider with a ca_file must have the
// same endpoints and CA bundle on the default branch (origin/HEAD), whose
// bundle is used (trustedTokenProvider). The host is printed on stderr
// before the first request.
func (h *hub) keyLocation(ctx context.Context, e *env, rps []config.ResolvedProvider, fp, token string, reg *redact.Registry) ([]report.DoctorCheck, error) {
	i := strings.LastIndexByte(fp, '/')
	host, id := strings.ToLower(fp[:i]), fp[i+1:]
	in := hubch.KeyStoreInput{RepoID: id, Token: token}
	var pp *planProvider
	for _, rp := range rps {
		if strings.EqualFold(rp.Host, host) {
			pp = &planProvider{ResolvedProvider: rp}
			break
		}
	}
	switch {
	case pp != nil:
		trusted, err := h.trustedTokenProvider(ctx, *pp, host, e.getenv)
		if err != nil {
			return nil, err
		}
		pp = &trusted
		in.Platform, in.APIURL = pp.Type, pp.APIURL
		client, err := h.providerClient(ctx, *pp, reg)
		if err != nil {
			return nil, configErrorf("--hub-token: provider %s: %w", pp.ID, err)
		}
		in.Client = client
	case host == "github.com":
		in.Platform, in.APIURL = "github", "https://api.github.com"
	case strings.HasSuffix(host, ".ghe.com"):
		in.Platform, in.APIURL = "github", "https://api."+host
	case host == "gitlab.com":
		in.Platform, in.APIURL = "gitlab", "https://gitlab.com/api/v4"
	default:
		return nil, configErrorf("--hub-token: no provider of %s is on the hub's host %s, so touchmark cannot tell its platform", config.HubFile, host)
	}
	if in.Client == nil {
		client, err := h.providerClient(ctx, planProvider{}, reg)
		if err != nil {
			return nil, err
		}
		in.Client = client
	}
	if u, err := url.Parse(in.APIURL); err == nil {
		fmt.Fprintf(e.stderr, "touchmark doctor: reading where the hub keeps its secrets on %s with the maintainer's token from %s\n", u.Host, hubTokenEnv)
	}
	ks, err := hubch.ReadKeyStore(ctx, in)
	if err != nil {
		// The rest of the report stands; this check could not be made.
		return []report.DoctorCheck{{Name: "key-location", Status: report.StatusUnknown,
			Detail: "the hub could not be read with the maintainer's token: " + err.Error()}}, nil
	}
	workflows, err := h.workflows(ctx)
	if err != nil {
		return nil, err
	}
	return distribute.KeyLocationChecks(ks, config.WriteKeySecrets(workflows), h.cfg), nil
}

// hubAPIHost is the host name of the API of a hub on host (a fingerprint's
// host, perhaps with a port) on platform typ: api.github.com for
// github.com, api.<host> for GHE.com, the hub's host itself elsewhere
// (GitHub Enterprise Server, GitLab, Gitea and Forgejo serve their API
// under their own URL).
func hubAPIHost(typ, host string) string {
	name := strings.ToLower(host)
	if hn, _, err := net.SplitHostPort(name); err == nil {
		name = hn
	}
	switch {
	case typ == "github" && name == "github.com":
		return "api.github.com"
	case typ == "github" && strings.HasSuffix(name, ".ghe.com"):
		return "api." + name
	}
	return name
}

// trustedTokenProvider returns the provider pp, on the hub's host, through
// which the maintainer's token may go: its API on the hub's own API host
// (hubAPIHost; ports aside), and, when it trusts a ca_file, the same
// provider of the default branch's hub.yml (origin/HEAD) with the same
// endpoints and CA bundle path, the bundle read at that commit. A
// checkout's hub.yml that sends the token elsewhere, or trusts a CA the
// default branch does not, is refused (exit 2).
func (h *hub) trustedTokenProvider(ctx context.Context, pp planProvider, host string, getenv func(string) string) (planProvider, error) {
	u, err := url.Parse(pp.APIURL)
	if err != nil || !strings.EqualFold(u.Hostname(), hubAPIHost(pp.Type, host)) {
		return planProvider{}, configErrorf("--hub-token: provider %s sends its API requests to %s, not to the hub's host %s: "+
			"the maintainer's token goes to the hub's own API only; check the provider's api_url in %s", pp.ID, apiHostOf(pp.APIURL), host, config.HubFile)
	}
	if pp.CAFile == "" {
		return pp, nil
	}
	rev, err := h.git.RevParse(ctx, "refs/remotes/origin/HEAD^{commit}")
	if err != nil {
		return planProvider{}, configErrorf("--hub-token: provider %s trusts ca_file %s, and this clone does not know the default branch (refs/remotes/origin/HEAD) "+
			"to check that the default branch trusts it too: run git remote set-head origin --auto", pp.ID, pp.CAFile)
	}
	base, err := h.providersAt(ctx, rev, getenv)
	if err != nil {
		return planProvider{}, configErrorf("--hub-token: provider %s trusts ca_file %s, and %v", pp.ID, pp.CAFile, err)
	}
	b, ok := base[pp.ID]
	if !ok || !sameEndpoints(b, pp.ResolvedProvider) {
		return planProvider{}, configErrorf("--hub-token: provider %s: this checkout's type, url, api_url or ca_file differs from the default branch's (origin/HEAD): "+
			"run doctor --hub-token on the default branch, whose %s is reviewed", pp.ID, config.HubFile)
	}
	return planProvider{ResolvedProvider: b, caRev: rev}, nil
}

// apiHostOf returns the host of an API URL for messages.
func apiHostOf(api string) string {
	if u, err := url.Parse(api); err == nil && u.Host != "" {
		return u.Host
	}
	return "an unknown host"
}

// maskDoctor masks the registered secrets in the free text of a doctor
// report: details and warnings quote platform messages.
func maskDoctor(reg *redact.Registry, doc *report.Doctor) {
	mask := func(cs []report.DoctorCheck) {
		for i := range cs {
			cs[i].Detail = reg.Replace(cs[i].Detail)
		}
	}
	mask(doc.HubChecks)
	for i := range doc.Providers {
		p := &doc.Providers[i]
		p.Error = reg.Replace(p.Error)
		mask(p.Checks)
	}
	for i := range doc.Targets {
		mask(doc.Targets[i].Checks)
	}
	for i, w := range doc.Warnings {
		doc.Warnings[i] = reg.Replace(w)
	}
}

// printDoctor writes the doctor report in format.
func printDoctor(w io.Writer, doc *report.Doctor, format string) error {
	switch format {
	case formatJSON:
		return report.WriteJSON(w, doc)
	case formatMarkdown:
		return doc.WriteMarkdown(w)
	}
	return doc.WriteText(w)
}

// publishDoctor publishes the doctor report (publish) and returns its exit
// code as an error (nil for 0).
func publishDoctor(e *env, hctx hubch.Context, reg *redact.Registry, doc *report.Doctor, format, reportFile string) error {
	err := publish(e, hctx, reg, renderers{
		print:       func(w io.Writer) error { return printDoctor(w, doc, format) },
		json:        func(w io.Writer) error { return report.WriteJSON(w, doc) },
		markdown:    doc.WriteMarkdown,
		summary:     doc.WriteSummary,
		annotations: func() string { return doctorAnnotations(doc) },
	}, doctorFiles, reportFile)
	if err != nil {
		return err
	}
	if code := doc.ExitCode(); code != exitOK {
		return exitWith(code)
	}
	return nil
}

// doctorAnnotations returns the GitHub Actions workflow commands for the
// first failed checks (::error) and the first that warn (::warning), of
// the hub, the providers and the named targets. A target a public hub does
// not name is never named.
func doctorAnnotations(doc *report.Doctor) string {
	var b strings.Builder
	counts := map[report.CheckStatus]int{}
	add := func(where string, cs []report.DoctorCheck) {
		for _, c := range cs {
			level := ""
			switch c.Status {
			case report.StatusFail:
				level = "error"
			case report.StatusWarn:
				level = "warning"
			default:
				continue
			}
			if counts[c.Status] == maxAnnotations {
				continue
			}
			counts[c.Status]++
			b.WriteString(annotation(level, where+" "+c.Name+": "+c.Detail))
		}
	}
	add("hub", doc.HubChecks)
	for _, p := range doc.Providers {
		add("provider "+p.ID, p.Checks)
	}
	for _, t := range doc.Targets {
		if t.Path != "" {
			add(t.Provider+":"+t.Path, t.Checks)
		}
	}
	return b.String()
}
