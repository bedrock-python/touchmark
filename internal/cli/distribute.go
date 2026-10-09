package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/docsurl"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// Defaults of distribute.
const (
	// streamFile is the report stream a CI run writes when --stream is not
	// given.
	streamFile = "touchmark-report.jsonl"
	// actionsDeadline is the default --deadline on GitHub Actions, whose jobs
	// end after six hours.
	actionsDeadline = 5*time.Hour + 30*time.Minute
	// deadlineMargin is what the default --deadline on GitLab leaves of
	// CI_JOB_TIMEOUT for the report.
	deadlineMargin = 5 * time.Minute
	// minDeadline is the shortest default --deadline.
	minDeadline = time.Minute
)

// distOptions are the flags of distribute beyond those it shares with
// plan.
type distOptions struct {
	deadline    durationFlag
	reportFile  string
	streamFile  string
	recreate    []config.RecreateOp
	forget      []config.ForgetOp
	massClose   int
	adopt       bool
	allowStale  bool
	massCloseOK bool // --allow-mass-close was given
}

// localOps reports whether an operation flag was given.
func (d *distOptions) localOps() bool {
	return len(d.recreate) > 0 || len(d.forget) > 0 || d.massCloseOK || d.adopt || d.allowStale
}

// operations returns the operations the flags give (nil for none): active
// on the day of now (UTC), as operations.yml entries are.
func (d *distOptions) operations(now time.Time) *config.Operations {
	if len(d.recreate) == 0 && len(d.forget) == 0 && !d.massCloseOK && !d.adopt {
		return nil
	}
	today := now.UTC().Format(time.DateOnly)
	ops := &config.Operations{Version: 1, Recreate: slices.Clone(d.recreate), ForgetDeclines: slices.Clone(d.forget)}
	if d.massCloseOK {
		ops.AllowMassClose = &config.MassCloseOp{Max: d.massClose, Until: today}
	}
	if d.adopt {
		ops.AdoptUnmarked = &config.UntilOp{Until: today}
	}
	return ops
}

// distributeCommand is the distribute command. Its flag values beyond the
// shared options live with the command itself.
func distributeCommand() *command {
	d := &distOptions{}
	return &command{
		name: "distribute",
		synopsis: "[--hub DIR] [--dry-run] [--only REF]... [--hub-fp HOST/ID] [--deadline DURATION] [--strict] " +
			"[--format text|json|markdown] [--report FILE] [--stream FILE] [local operation flags]",
		summary: "open, update and close the pull requests of every target (--dry-run: report what it would do)",
		flags: func() (*flagSet, *options) {
			*d = distOptions{}
			return distributeFlags(d)
		},
		run: func(ctx context.Context, e *env, o *options) error { return runDistribute(ctx, e, o, d) },
	}
}

func distributeFlags(d *distOptions) (*flagSet, *options) {
	f, o := newFlagSet("distribute")
	f.fs.StringVar(&o.hub, "hub", "", "the hub checkout `DIR` (default $TOUCHMARK_HUB)")
	f.fs.BoolVar(&o.worktree, "worktree", false, "refused: distribute ships only the packs committed at the hub's HEAD")
	f.fs.BoolVar(&o.dryRun, "dry-run", false, "read and check everything with the write credential, write nothing, and report what distribute would do")
	f.fs.Var(&o.only, "only", "distribute only to the target `REF`, as [PROVIDER:]PATH; repeatable or comma-separated (the sweep is off)")
	f.fs.Var(&o.hubFP, "hub-fp", "the hub's fingerprint `HOST/ID`, e.g. github.com/712345678 (CI provides it)")
	f.fs.Var(&d.deadline, "deadline", "start no target after `DURATION`, e.g. 50m (default: CI_JOB_TIMEOUT less 5m on GitLab, 5h30m on GitHub Actions, none elsewhere; 0 for none)")
	f.fs.BoolVar(&o.strict, "strict", false, "exit 3 when a target is blocked or deferred or the sweep did not run")
	f.fs.StringVar(&o.format, "format", formatText, "output `FORMAT`: text, json or markdown")
	f.fs.StringVar(&d.reportFile, "report", "", "also write the JSON report to `FILE`")
	f.fs.StringVar(&d.streamFile, "stream", "", "write one JSON line per finished target to `FILE` as the run goes (default in CI: "+streamFile+")")
	f.fs.Func("recreate", "local only: rebuild the paused sync branch of `TARGET@HEAD` while its head is HEAD (repeatable)", func(s string) error {
		op, err := parseRecreate(s)
		d.recreate = append(d.recreate, op)
		return err
	})
	f.fs.Func("forget-declines", "local only: propose again what `TARGET#PR`, a declined pull request, carried (repeatable)", func(s string) error {
		op, err := parseForget(s)
		d.forget = append(d.forget, op)
		return err
	})
	f.fs.Func("allow-mass-close", "local only: let this run close up to `MAX` pull requests", func(s string) error {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			return fmt.Errorf("%q is not a positive number", s)
		}
		d.massClose, d.massCloseOK = n, true
		return nil
	})
	f.fs.BoolVar(&d.adopt, "adopt-unmarked", false, "local only: take over the open pull requests without a marker on the branch aliases (a multi-gitter setup's)")
	f.fs.BoolVar(&d.allowStale, "allow-stale", false, "local only: run although the hub's HEAD is not the tip of origin's default branch in this clone")
	f.formats = []string{formatText, formatJSON, formatMarkdown}
	return f, o
}

// parseRecreate parses TARGET@HEAD: a target as operations.yml names it and
// a full commit id.
func parseRecreate(s string) (config.RecreateOp, error) {
	i := strings.LastIndexByte(s, '@')
	if i < 0 {
		return config.RecreateOp{}, fmt.Errorf("%q is not TARGET@HEAD", s)
	}
	target, head := s[:i], strings.ToLower(s[i+1:])
	if _, err := config.ParseRef(target); err != nil {
		return config.RecreateOp{}, err
	}
	if (len(head) != 40 && len(head) != 64) || strings.Trim(head, "0123456789abcdef") != "" {
		return config.RecreateOp{}, fmt.Errorf("%q after @ is not a full commit id", head)
	}
	return config.RecreateOp{Target: target, Head: head}, nil
}

// parseForget parses TARGET#PR.
func parseForget(s string) (config.ForgetOp, error) {
	i := strings.LastIndexByte(s, '#')
	if i < 0 {
		return config.ForgetOp{}, fmt.Errorf("%q is not TARGET#PR", s)
	}
	target := s[:i]
	if _, err := config.ParseRef(target); err != nil {
		return config.ForgetOp{}, err
	}
	n, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil || n < 1 {
		return config.ForgetOp{}, fmt.Errorf("%q after # is not a pull request number", s[i+1:])
	}
	return config.ForgetOp{Target: target, PR: n}, nil
}

// durationFlag is the value of --deadline; set records that it was given.
type durationFlag struct {
	d   time.Duration
	set bool
}

func (f *durationFlag) String() string {
	if !f.set {
		return ""
	}
	return f.d.String()
}

func (f *durationFlag) Set(s string) error {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < 0 {
		return fmt.Errorf("%q is not a duration such as 50m or 2h30m", s)
	}
	f.d, f.set = d, true
	return nil
}

var _ flag.Value = (*durationFlag)(nil)

// runDistribute runs distribute: it opens, updates and
// closes the pull requests of every target through each provider's write
// identity, or with --dry-run reports what it would do and writes nothing.
//
// It loads the hub like plan (configs, operations, history and packs from
// HEAD; a hub that fails check, has no id or no fingerprint is refused),
// refuses --worktree (distribute ships only committed packs), and runs the
// guards of distribute.DistributeGuard (git version, operation flags only
// locally, in CI the default branch, the protected environment and the
// write isolation probe) before anything else; on GitHub Actions under
// security.write_isolation platform the hub channel then checks that only
// the default branch may use the environment touchmark-distribute
// (environmentGuard); a local run also checks that the hub's HEAD is the
// tip of origin's default branch, unless --allow-stale. Every refusal
// exits 2.
//
// It then reads each provider's write credential and signing key (removing
// the variables from the process environment), masks every secret in all
// it prints (::add-mask:: first on GitHub Actions), builds the writers
// through the driver registry, and runs distribute.Run with the hub's blobs,
// operations.yml of HEAD and the operation flags, the intro, the hub's web
// URL, a private git repository per target under a temporary directory,
// the report stream (--stream, touchmark-report.jsonl in CI) and the
// deadline. It prints the report, writes --report and the GitHub Actions
// step summary and annotations, and exits with the report's code.
func runDistribute(ctx context.Context, e *env, o *options, d *distOptions) (err error) {
	if o.worktree {
		return configErrorf("--worktree: distribute ships only committed packs; commit them first (plan --worktree previews uncommitted ones)")
	}
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
	gitV, err := gitVersion(ctx)
	if err != nil {
		return configError(err)
	}
	if err := distribute.DistributeGuard(ctx, distribute.GuardInput{
		Context: hctx, Hub: h.cfg, Getenv: e.getenv, LocalOps: d.localOps(), GitVersion: gitV, DryRun: o.dryRun,
	}); err != nil {
		return configError(joinedError(err))
	}
	now := time.Now()
	localOps := d.operations(now)
	if _, errs := config.CheckOperations(localOps, h.targets, h.cfg, now); len(errs) > 0 {
		return usageErrorf("operation flags: %v", joinedError(errors.Join(errs...)))
	}
	reg := redact.New()
	defer func() { err = maskError(reg, err) }()
	token := hubch.Token(hctx, e.getenv)
	reg.Add(token, basicUsers...)
	rps, err := h.resolveProviders(ctx, hctx, e.getenv)
	if err != nil {
		return configError(err)
	}
	pps := make([]planProvider, len(rps))
	for i, rp := range rps {
		pps[i] = planProvider{ResolvedProvider: rp}
	}
	channel := h.hubChannel(ctx, hctx, pps, token, reg)
	if distribute.EnvironmentGuardApplies(hctx, h.cfg) {
		warning, err := environmentGuard(ctx, hctx, channel)
		if err != nil {
			return configError(err)
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if !inCI(hctx, e.getenv) && !d.allowStale {
		if channel, err = h.localHead(ctx); err != nil {
			return err
		}
	}
	drivers := make([]distributeDriver, len(rps))
	for i, rp := range rps {
		if drivers[i] = distributeDrivers[rp.Type]; drivers[i] == nil {
			return configErrorf("provider %s: no %s driver in this build", rp.ID, rp.Type)
		}
	}
	secrets, err := readWriteSecrets(rps, e.getenv, reg)
	if err != nil {
		return err
	}
	if hctx.CI == hubch.GitHubActions {
		if err := reg.GitHubMask(e.stderr); err != nil {
			return err
		}
		maskNewSecrets(reg, e.stderr)
	}
	hctx, visWarnings := channelVisibility(ctx, hctx, channel)
	warnings = append(warnings, visWarnings...)
	run, err := h.distributeRun(ctx, e, runInput{
		hctx: hctx, fp: fp, rps: rps, drivers: drivers, secrets: secrets, reg: reg, channel: channel,
		o: o, d: d, localOps: localOps, now: now,
	})
	if err != nil {
		return err
	}
	defer run.close()
	rep, err := distribute.Run(ctx, run.deps, run.mode)
	if err != nil {
		return configError(err)
	}
	if w := isolationWarning(h.cfg); w != "" {
		warnings = append(warnings, w)
	}
	rep.Warnings = nonNil(slices.Concat(h.warnings, warnings, run.warnings, rep.Warnings))
	if err := run.closeStream(); err != nil {
		rep.Warnings = append(rep.Warnings, "the report stream: "+err.Error())
	}
	maskDelivery(reg, rep)
	return publishDelivery(e, hctx, reg, rep, o.format, d.reportFile)
}

// maskNewSecrets makes every secret reg registers from now on print its
// "::add-mask::" commands to w before the registration returns: on GitHub
// Actions the drivers register each JWT and token they mint, and the
// runner must mask them before their first use, also in
// output that bypasses touchmark's own masking writers. w is the raw
// stderr, never a writer of reg; the drivers mint concurrently.
func maskNewSecrets(reg *redact.Registry, w io.Writer) {
	var mu sync.Mutex
	reg.OnAdd(func(forms []string) {
		mu.Lock()
		defer mu.Unlock()
		_ = redact.GitHubMaskForms(w, forms)
	})
}

// environmentGuard reads the environment touchmark-distribute through the
// hub channel and checks that only the hub's default branch may use it
// (distribute.CheckEnvironment): an error stops the run
// (exit 2), a warning says what could not be verified. It fails closed: a
// hub channel that cannot read environments is an error too. The default
// branch is the one the channel reads the tip of: the CI's default branch,
// or the branch a scheduled run builds.
func environmentGuard(ctx context.Context, hctx hubch.Context, ch hubch.Channel) (string, error) {
	reader, ok := ch.(hubch.EnvironmentReader)
	if !ok {
		return "", fmt.Errorf("the environment %s cannot be read through the hub channel, so nothing shows that only the default branch "+
			"may use it: run distribute on GitHub Actions with the workflow's GITHUB_TOKEN, or set security.write_isolation to external "+
			"or to none with a reason; see "+docsurl.WriteIsolation, distribute.DistributeEnvironment)
	}
	def := hctx.DefaultBranch
	if def == "" && hctx.RefIsBranch {
		def = hctx.RefName
	}
	env, err := reader.Environment(ctx, distribute.DistributeEnvironment)
	return distribute.CheckEnvironment(env, err, def)
}

// isolationWarning is the report warning of a hub that accepted the risk of
// a write key other branches' jobs may see (security.write_isolation:
// none), "" otherwise.
func isolationWarning(cfg *config.Hub) string {
	if cfg == nil || cfg.Security.WriteIsolation != "none" {
		return ""
	}
	return "write isolation is off (security.write_isolation: none): jobs of any branch of the hub may see the write key; reason: " +
		strings.TrimSpace(cfg.Security.Reason)
}

// localHead is the hub channel of a local run (the head guard):
// the tip of origin's default branch in the hub clone
// (refs/remotes/origin/HEAD) must be the hub's HEAD, else the run is
// refused (--allow-stale lifts it): a maintainer's run ships only the
// reviewed hub. Without origin/HEAD it returns no channel, and the report
// says the head was not checked.
func (h *hub) localHead(ctx context.Context) (hubch.Channel, error) {
	out, err := h.git.Run(ctx, nil, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	ref := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(ref, "refs/remotes/origin/") {
		return nil, nil
	}
	tip, err := h.git.RevParse(ctx, ref+"^{commit}")
	if err != nil {
		return nil, configErrorf("read the tip of %s: %w", ref, err)
	}
	if tip != h.commit {
		return nil, configErrorf("the hub's HEAD %s is not the tip of %s (%s): distribute ships only the reviewed default branch; "+
			"update the clone and check it out, or pass --allow-stale", short(h.commit), strings.TrimPrefix(ref, "refs/remotes/"), short(tip))
	}
	return cloneTip(tip), nil
}

// cloneTip is the hub channel of a local run: the tip of origin's default
// branch in the clone, read once.
type cloneTip string

func (c cloneTip) Head(context.Context) (string, error) { return string(c), nil }

// short returns the first 12 hex digits of a commit id.
func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}
