package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/docsurl"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/setup"
)

// setupOptions are the flags of setup beyond the shared ones.
type setupOptions struct {
	provider, url, project string
	// GitLab's.
	group, accounts        string
	readerName, writerName string
	tokenDays              int
	schedules              bool
	// GitHub's.
	listen, keyDir string
	timeout        time.Duration
}

// defaultSetupTimeout bounds the browser step of setup github: GitHub's
// code lives an hour.
const defaultSetupTimeout = 30 * time.Minute

// setupCommand is the setup command.
func setupCommand() *command {
	s := &setupOptions{}
	return &command{
		name: "setup",
		synopsis: "github|gitlab [--hub DIR] [--provider ID] [--url URL] [--project PATH] [--dry-run] [--format text|json]\n" +
			"         gitlab: --group PATH [--accounts auto|service-account|group-access-token] [--reader-name NAME] [--writer-name NAME] [--token-days N] [--schedules=false]\n" +
			"         github: [--reader-name NAME] [--writer-name NAME] [--listen ADDR] [--key-dir DIR] [--timeout DURATION]",
		summary: "set up the hub's platform with a maintainer's token from $" + hubTokenEnv + ": the reader and the writer, " +
			"the write key kept to the default branch, the hub's protection; changes only what differs",
		flags: func() (*flagSet, *options) {
			*s = setupOptions{}
			return setupFlags(s)
		},
		run: func(ctx context.Context, e *env, o *options) error { return runSetup(ctx, e, o, s) },
	}
}

func setupFlags(s *setupOptions) (*flagSet, *options) {
	f, o := newFlagSet("setup")
	f.nargs, f.argName, f.leading = 1, "the platform: github or gitlab", true
	f.fs.StringVar(&o.hub, "hub", "", "the hub checkout `DIR` (default $TOUCHMARK_HUB); setup reads its hub.yml and origin remote")
	f.fs.BoolVar(&o.dryRun, "dry-run", false, "read the platform and print what setup would change; change nothing")
	f.fs.StringVar(&o.format, "format", formatText, "output `FORMAT`: text or json")
	f.fs.StringVar(&s.provider, "provider", "", "the hub.yml provider `ID` to set up (default: the one of the platform on the hub's host)")
	f.fs.StringVar(&s.url, "url", "", "the platform's `URL`, for a self-managed instance hub.yml does not list yet")
	f.fs.StringVar(&s.project, "project", "", "the hub's `PATH` on the platform, owner/name (default: from the origin remote)")
	f.fs.StringVar(&s.group, "group", "", "gitlab: the full `PATH` of the group whose projects are the targets")
	f.fs.StringVar(&s.accounts, "accounts", setup.AccountsAuto, "gitlab: the `KIND` of the reader and the writer: auto, service-account or group-access-token")
	f.fs.StringVar(&s.readerName, "reader-name", "", "the reader's `NAME`: a service account's username, a group access token's name, a GitHub App's name")
	f.fs.StringVar(&s.writerName, "writer-name", "", "the writer's `NAME`, as --reader-name")
	f.fs.IntVar(&s.tokenDays, "token-days", setup.DefaultTokenDays, "gitlab: how many `DAYS` the tokens setup mints live")
	f.fs.BoolVar(&s.schedules, "schedules", true, "gitlab: create the daily distribute and weekly doctor pipeline schedules")
	f.fs.StringVar(&s.listen, "listen", setup.DefaultListen, "github: the loopback `ADDR` of the page of the App manifest flow")
	f.fs.StringVar(&s.keyDir, "key-dir", "", "github: the `DIR` for the new Apps' private keys (default: a new private temporary directory)")
	f.fs.DurationVar(&s.timeout, "timeout", defaultSetupTimeout, "github: how long to wait for the browser step, a `DURATION`")
	return f, o
}

// giteaRefusal explains why setup offers nothing for Gitea and Forgejo.
func giteaRefusal(platform string) error {
	return configErrorf("setup does not set up %s: its Actions give the jobs of every branch the secrets and ignore environments, "+
		"so no setup can keep the write key to the default branch. Run the hub's CI on GitHub or GitLab and deliver "+
		"to %s from there, or set security.write_isolation: none with a reason and protect every branch so that only maintainers push; "+
		"%s explains both", platform, platform, docsurl.GiteaForgejo)
}

// runSetup runs setup: locally, with a maintainer's token of
// the hub from TOUCHMARK_HUB_TOKEN, for the provider of hub.yml on the hub's
// own platform (the one of its origin remote, or --provider, or --url). It
// refuses CI, Gitea and Forgejo, and a hub whose security.write_isolation is
// none: setup offers only what keeps the write key isolated. The token goes
// to the hub's own API host only, as doctor --hub-token's does
// (trustedTokenProvider).
//
// It prints the report in --format and exits with its code: 1 when a step
// failed, 3 when a step is left to the person (run setup again once done),
// 0 otherwise; 2 when the hub, the token or the flags are not what setup
// needs, before anything was written.
func runSetup(ctx context.Context, e *env, o *options, s *setupOptions) (err error) {
	platform := o.args[0]
	switch platform {
	case "gitea", "forgejo":
		return giteaRefusal(platform)
	case "bitbucket":
		return configErrorf("setup does not set up bitbucket yet: a hub on Bitbucket Pipelines comes in a later release; " +
			"a hub on GitHub or GitLab delivers to Bitbucket Cloud as one of its providers")
	case "azure-devops":
		return configErrorf("setup does not set up azure-devops yet: a hub on Azure Pipelines comes in a later release; " +
			"a hub on GitHub or GitLab delivers to Azure DevOps as one of its providers")
	case "github", "gitlab":
	default:
		return usageErrorf("unknown platform %q: setup sets up github or gitlab", platform)
	}
	if err := s.check(platform); err != nil {
		return err
	}
	hctx := hubch.Detect(e.getenv, os.ReadFile)
	if inCI(hctx, e.getenv) {
		return configErrorf("setup is a maintainer's local run: a CI job must not hold a maintainer's token of the hub")
	}
	h, err := openHub(ctx, e, o.hub, true)
	if err != nil {
		return err
	}
	data, err := h.readConfig(ctx, config.HubFile)
	if err != nil {
		return configError(err)
	}
	var cfg *config.Hub
	if data != nil {
		if cfg, _, err = config.ParseHub(data); err != nil {
			return configErrorf("%w\nfix %s first (touchmark check --worktree tells more)", joinedError(err), config.HubFile)
		}
	}
	isolation := "platform"
	if cfg != nil && cfg.Security.WriteIsolation != "" {
		isolation = cfg.Security.WriteIsolation
	}
	if isolation == "none" {
		return configErrorf("setup offers only an isolated write key, and %s sets security.write_isolation: none: "+
			"set it to platform (or external) to use setup, or keep the keys by hand: %s", config.HubFile, docsurl.GettingStarted(platform))
	}
	rp, repoPath, err := h.setupProvider(ctx, cfg, platform, s)
	if err != nil {
		return err
	}
	if rp.Type == "gitea" || rp.Type == "forgejo" {
		return giteaRefusal(rp.Type)
	}
	token := strings.TrimSpace(e.getenv(hubTokenEnv))
	// Failing to unset leaves the value where it was: no reason to stop.
	_ = os.Unsetenv(hubTokenEnv)
	if token == "" {
		return configErrorf("set %s to a maintainer's token of the hub (GitLab: a Maintainer of the hub and an Owner of the group of targets, "+
			"scope api; GitHub: an admin of the hub)", hubTokenEnv)
	}
	reg := redact.New()
	defer func() { err = maskError(reg, err) }()
	reg.Add(token, basicUsers...)
	trusted, err := h.trustedTokenProvider(ctx, planProvider{ResolvedProvider: rp}, rp.Host, e.getenv)
	if err != nil {
		return err
	}
	client, err := h.providerClient(ctx, trusted, reg)
	if err != nil {
		return configErrorf("provider %s: %w", rp.ID, err)
	}
	prefix := rp.EnvPrefix
	if rp.Short {
		prefix = shortEnvPrefix
	}
	fmt.Fprintf(e.stderr, "touchmark setup: %s on %s for the hub %s, with the maintainer's token from %s%s\n",
		platform, apiHostOf(trusted.APIURL), repoPath, hubTokenEnv, map[bool]string{true: " (dry run)", false: ""}[o.dryRun])
	var rep *setup.Report
	switch platform {
	case "gitlab":
		rep, err = setup.GitLab(ctx, setup.GitLabInput{
			APIURL: trusted.APIURL, Host: rp.Host, Project: repoPath, Group: strings.Trim(s.group, "/"), Token: token, Client: client,
			Isolation: isolation, Accounts: s.accounts, ReaderName: s.readerName, WriterName: s.writerName,
			ReadVar: prefix + "READ_TOKEN", WriteVar: prefix + "WRITE_TOKEN", Writer: rp.Writer,
			TokenDays: s.tokenDays, Schedules: s.schedules, DryRun: o.dryRun, Engine: version(),
			Redact: reg, BasicUsers: basicUsers,
		})
	default:
		stderr := reg.Writer(e.stderr)
		rep, err = setup.GitHub(ctx, setup.GitHubInput{
			WebURL: rp.URL, APIURL: trusted.APIURL, Host: rp.Host, Repo: repoPath, Token: token, Client: client,
			Isolation: isolation, ReaderName: s.readerName, WriterName: s.writerName,
			ReadIDVar: prefix + "READ_APP_ID", ReadKeyVar: prefix + "READ_APP_KEY",
			WriteIDVar: prefix + "WRITE_APP_ID", WriteKeyVar: prefix + "WRITE_APP_KEY",
			Writer: rp.Writer, DryRun: o.dryRun, KeyDir: s.keyDir, Listen: s.listen, Timeout: s.timeout,
			Tell: func(u string) {
				fmt.Fprintf(stderr, "touchmark setup: open %s in a browser signed in to %s as an owner of the hub's account, "+
					"and create the Apps there; waiting at most %s\n", u, rp.Host, s.timeout)
				_ = stderr.Flush()
			},
			Engine: version(), Redact: reg,
		})
	}
	if err != nil && setup.IsPrecondition(err) {
		return configError(err)
	}
	if rep != nil && len(rep.Steps) > 0 {
		if perr := printSetup(reg.Writer(e.stdout), rep, o.format); perr != nil {
			return errors.Join(err, perr)
		}
	}
	if err != nil {
		return err
	}
	if code := rep.ExitCode(); code != exitOK {
		return exitWith(code)
	}
	return nil
}

// printSetup writes the report through the redacting writer w.
func printSetup(w *redact.Writer, rep *setup.Report, format string) error {
	var err error
	if format == formatJSON {
		err = rep.WriteJSON(w)
	} else {
		err = rep.WriteText(w)
	}
	return errors.Join(err, w.Flush())
}

// check refuses the flags of the other platform.
func (s *setupOptions) check(platform string) error {
	var other []string
	if platform == "github" {
		if s.group != "" {
			other = append(other, "--group")
		}
		if s.accounts != setup.AccountsAuto {
			other = append(other, "--accounts")
		}
		if s.tokenDays != setup.DefaultTokenDays {
			other = append(other, "--token-days")
		}
		if !s.schedules {
			other = append(other, "--schedules")
		}
	} else {
		if s.group == "" {
			return usageErrorf("setup gitlab needs --group, the group whose projects are the targets")
		}
		if s.listen != setup.DefaultListen {
			other = append(other, "--listen")
		}
		if s.keyDir != "" {
			other = append(other, "--key-dir")
		}
		if s.timeout != defaultSetupTimeout {
			other = append(other, "--timeout")
		}
		if s.tokenDays <= 0 {
			return usageErrorf("--token-days must be positive")
		}
	}
	if len(other) > 0 {
		return usageErrorf("%s: not for setup %s", strings.Join(other, ", "), platform)
	}
	return nil
}

// setupProvider returns the provider setup works on and the hub's path on
// its platform. The provider is --provider of hub.yml; else the one of the
// platform at --url's host, or at the origin remote's host; else, when
// hub.yml lists none there, one made from --url or from a public
// instance's host (github.com, *.ghe.com, gitlab.com), with the short
// variable names when the hub has at most one provider. The hub's path is
// --project, else the origin remote's, which must be on the provider's
// host.
func (h *hub) setupProvider(ctx context.Context, cfg *config.Hub, platform string, s *setupOptions) (config.ResolvedProvider, string, error) {
	var originHost, originPath string
	if out, err := h.git.Run(ctx, nil, "remote", "get-url", "origin"); err == nil {
		// The URL may hold credentials: only its host and path are kept.
		originHost, originPath, _ = parseRemote(strings.TrimSpace(string(out)))
	}
	noEnv := func(string) string { return "" }
	urlHost := ""
	if s.url != "" {
		probe := &config.Hub{Providers: []config.Provider{{ID: platform, Type: platform, URL: s.url}}}
		rps, err := probe.ResolveProviders(noEnv)
		if err != nil {
			return config.ResolvedProvider{}, "", usageErrorf("--url: %v", err)
		}
		urlHost = rps[0].Host
	}
	var declared []config.ResolvedProvider
	if cfg != nil && (len(cfg.Providers) > 0 || cfg.Platform != "") {
		rps, err := cfg.ResolveProviders(noEnv)
		if err != nil {
			return config.ResolvedProvider{}, "", configErrorf("%s: %v", config.HubFile, err)
		}
		declared = rps
	}
	var rp *config.ResolvedProvider
	for i := range declared {
		d := &declared[i]
		switch {
		case s.provider != "":
			if d.ID == s.provider {
				rp = d
			}
		case urlHost != "":
			if d.Type == platform && strings.EqualFold(d.Host, urlHost) {
				rp = d
			}
		case originHost != "":
			if d.Type == platform && strings.EqualFold(hostName(d.Host), originHost) {
				rp = d
			}
		}
		if rp != nil {
			break
		}
	}
	switch {
	case rp != nil && rp.Type != platform:
		return config.ResolvedProvider{}, "", usageErrorf("provider %s is %s, not %s", rp.ID, rp.Type, platform)
	case rp == nil && s.provider != "":
		return config.ResolvedProvider{}, "", configErrorf("%s has no provider %q", config.HubFile, s.provider)
	case rp == nil:
		u := s.url
		if u == "" {
			switch {
			case platform == "github" && (originHost == "github.com" || strings.HasSuffix(originHost, ".ghe.com")):
				u = "https://" + originHost
			case platform == "gitlab" && originHost == "gitlab.com":
				u = "https://gitlab.com"
			case originHost == "":
				return config.ResolvedProvider{}, "", configErrorf("the hub has no origin remote: pass --url and --project")
			default:
				return config.ResolvedProvider{}, "", configErrorf("the hub's origin is on %s, a self-managed instance %s does not list as a %s provider: "+
					"pass --url, or add the provider to %s", originHost, config.HubFile, platform, config.HubFile)
			}
		}
		if len(declared) > 1 {
			return config.ResolvedProvider{}, "", configErrorf("%s lists several providers and none is %s on %s: add it there first, "+
				"so that its variables have their names (TOUCHMARK_<ID>_…)", config.HubFile, platform, hostName(cmpOrStr(urlHost, originHost)))
		}
		made := &config.Hub{Providers: []config.Provider{{ID: platform, Type: platform, URL: u}}}
		rps, err := made.ResolveProviders(noEnv)
		if err != nil {
			return config.ResolvedProvider{}, "", usageErrorf("%v", err)
		}
		p := rps[0]
		p.Short, p.Implicit = true, true
		if len(declared) == 1 {
			// One declared provider of another platform or host: the new one
			// would make two, with prefixed names.
			return config.ResolvedProvider{}, "", configErrorf("%s's provider %s is not %s on %s: add the %s provider to %s first",
				config.HubFile, declared[0].ID, platform, hostName(p.Host), platform, config.HubFile)
		}
		if cfg != nil {
			p.Writer = cfg.Writer
		}
		rp = &p
	}
	repoPath := strings.Trim(s.project, "/")
	if repoPath == "" {
		if originPath == "" || !strings.EqualFold(originHost, hostName(rp.Host)) {
			return config.ResolvedProvider{}, "", configErrorf("the hub's origin remote is not on %s: pass --project, the hub's path there", rp.Host)
		}
		repoPath = originPath
	}
	return *rp, repoPath, nil
}

// hostName strips the port of host.
func hostName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// cmpOrStr returns a unless it is empty, else b.
func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
