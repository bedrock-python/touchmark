package distribute

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/docsurl"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
)

// GuardInput is what the distribute guards read.
type GuardInput struct {
	Context hubch.Context
	Hub     *config.Hub
	// Getenv reads the environment (TOUCHMARK_KEY_EXPOSED, CI, …).
	Getenv func(string) string
	// LocalOps is set when operation flags were given (--recreate,
	// --forget-declines, --allow-mass-close, --adopt-unmarked,
	// --allow-stale).
	LocalOps bool
	// GitVersion is the version of git on PATH.
	GitVersion [3]int
	// DryRun is set for distribute --dry-run. It relaxes no check: a dry
	// run holds the write credential as distribute does.
	DryRun bool
}

// Names of the guards' environment.
const (
	// keyExposedVar carries the probe job's answer on GitHub Actions:
	// "false" when no write key is visible outside the environment.
	keyExposedVar = "TOUCHMARK_KEY_EXPOSED"
	// distributeEnvironment is the CI environment that holds the write key.
	distributeEnvironment = "touchmark-distribute"
)

// DistributeGuard runs the guards of distribute before any platform call
// (exit 2 when one fails; see docs/concepts/security.md):
//   - git ≥ gitx.DeliveryMinVersion;
//   - operation flags only outside CI (CI set, or a detected CI) — in CI
//     operations come from .touchmark/operations.yml;
//   - in CI: the run builds the hub's default branch (RefIsBranch and
//     RefName == DefaultBranch) and the event is not pull_request* or
//     merge_request_event; on GitLab also CI_COMMIT_REF_PROTECTED=true and
//     CI_ENVIRONMENT_NAME=touchmark-distribute;
//   - write isolation (security.write_isolation): "platform" on GitHub
//     Actions needs TOUCHMARK_KEY_EXPOSED=false (true: the key is visible
//     outside the environment; unset: the probe job is missing); "none"
//     needs security.reason (config enforces it) and adds a red report
//     warning; "external" skips the probe.
//
// A dry run gets every check: it holds the write credential as distribute
// does, and builds its drivers from the hub.yml of the ref it runs on (no
// command that holds it is exempt).
//
// In detail:
//   - CI is a CI hubch recognizes, or CI set to anything but "", "false"
//     and "0" (another CI): there the ref cannot be told, and distribute is
//     refused.
//   - The events of pull and merge requests are pull_request*,
//     merge_request_event and external_pull_request_event. A branch run
//     whose CI names no default branch passes only for GitHub's and Gitea's
//     schedule event, which always builds the default branch (its payload
//     names no repository).
//   - "platform" cannot hold on Gitea and Forgejo Actions, whose secrets
//     every branch's jobs see: refused there, with the ways out (external,
//     or none with a reason). On GitLab the probe runs in the hub's merge
//     request pipelines instead (`touchmark probe`), and a local run has no
//     probe. The red warning for "none" is the caller's to add to the
//     report: the guards return errors only.
//
// Every failed check is reported, joined, never a variable's value.
func DistributeGuard(ctx context.Context, in GuardInput) error {
	_ = ctx // the guards read no platform
	var errs []error
	if err := GitGuard("distribute", in.GitVersion); err != nil {
		errs = append(errs, err)
	}
	getenv := in.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	ci := guardInCI(in.Context, getenv)
	if in.LocalOps && ci {
		errs = append(errs, errors.New("operation flags (--recreate, --forget-declines, --allow-mass-close, --adopt-unmarked, --allow-stale) work only in a local run: "+
			"in CI one-off operations come from "+config.OperationsFile+" on the default branch, after review"))
	}
	if ci {
		errs = append(errs, guardContext(in.Context)...)
	}
	if ci {
		if err := guardIsolation(in, getenv); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// guardInCI reports whether the run is in CI: a CI hubch recognizes, or any
// other that sets CI.
func guardInCI(c hubch.Context, getenv func(string) string) bool {
	if c.CI != hubch.Local && c.CI != "" {
		return true
	}
	if getenv == nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(getenv("CI")))
	return v != "" && v != "false" && v != "0"
}

// guardContext checks that a CI job of distribute builds the hub's default
// branch, for no pull request, and on GitLab in the protected environment.
func guardContext(c hubch.Context) []error { return guardContextOf("distribute", c) }

// guardContextOf is guardContext for command cmd (distribute, doctor): the
// commands that hold the write key.
func guardContextOf(cmd string, c hubch.Context) []error {
	var errs []error
	ev := c.Event
	if strings.HasPrefix(ev, "pull_request") || ev == "merge_request_event" || ev == "external_pull_request_event" {
		errs = append(errs, fmt.Errorf("%s does not run for the %s event: only a run of the hub's default branch may hold the write key", cmd, ev))
	}
	switch {
	case c.CI != hubch.GitHubActions && c.CI != hubch.GitLabCI && c.CI != hubch.GiteaActions && c.CI != hubch.ForgejoActions:
		errs = append(errs, fmt.Errorf("%s cannot tell which ref this CI job builds: it runs in CI only on GitHub Actions, GitLab CI, Gitea Actions and Forgejo Actions", cmd))
	case !c.RefIsBranch:
		errs = append(errs, fmt.Errorf("%s runs only on the hub's default branch; this job builds %s, which is not a branch", cmd, guardRef(c.RefName)))
	case c.DefaultBranch != "" && c.RefName != c.DefaultBranch:
		errs = append(errs, fmt.Errorf("%s runs only on the hub's default branch %s, not on %s", cmd, c.DefaultBranch, c.RefName))
	case c.DefaultBranch == "" && c.Event != "schedule":
		errs = append(errs, fmt.Errorf("the CI names no default branch of the hub, so %s cannot tell whether %s is it", cmd, c.RefName))
	}
	if c.CI == hubch.GitLabCI {
		if !c.RefProtected {
			errs = append(errs, fmt.Errorf("%s needs a protected ref on GitLab (CI_COMMIT_REF_PROTECTED=true): protect the default branch", cmd))
		}
		if c.Environment != distributeEnvironment {
			errs = append(errs, fmt.Errorf("%s needs the job's environment %s on GitLab (CI_ENVIRONMENT_NAME), which holds the write key", cmd, distributeEnvironment))
		}
	}
	return errs
}

// guardIsolation checks security.write_isolation in a CI job.
func guardIsolation(in GuardInput, getenv func(string) string) error {
	mode := "platform"
	if in.Hub != nil && in.Hub.Security.WriteIsolation != "" {
		mode = in.Hub.Security.WriteIsolation
	}
	if mode != "platform" {
		return nil
	}
	switch in.Context.CI {
	case hubch.GitHubActions:
		switch v := strings.ToLower(strings.TrimSpace(getenv(keyExposedVar))); v {
		case "false":
			return nil
		case "true":
			return fmt.Errorf("%s=true: the probe job sees a write key outside the environment %s (a repository or organization secret): "+
				"keep write keys in the environment only (security.write_isolation: platform)", keyExposedVar, distributeEnvironment)
		case "":
			return fmt.Errorf("%s is not set: the workflow has no probe job, so nothing shows that the write key is kept in the environment %s "+
				"(security.write_isolation: platform; see "+docsurl.WriteIsolation+")", keyExposedVar, distributeEnvironment)
		default:
			return fmt.Errorf("%s must be true or false, the probe job's answer", keyExposedVar)
		}
	case hubch.GiteaActions, hubch.ForgejoActions:
		return errors.New("security.write_isolation: platform cannot hold on Gitea and Forgejo Actions, whose secrets every branch's jobs see: " +
			"run distribute on GitHub Actions or GitLab CI, or set write_isolation to external, or to none with a reason")
	}
	return nil
}

// DistributeEnvironment is the environment of the hub's CI that holds the
// write key and runs distribute.
const DistributeEnvironment = distributeEnvironment

// EnvironmentGuardApplies reports whether the environment check of
// CheckEnvironment applies to a run in context c of a hub with hub.yml
// cfg: distribute or a dry run on GitHub Actions under
// security.write_isolation platform (the default).
func EnvironmentGuardApplies(c hubch.Context, cfg *config.Hub) bool {
	mode := "platform"
	if cfg != nil && cfg.Security.WriteIsolation != "" {
		mode = cfg.Security.WriteIsolation
	}
	return c.CI == hubch.GitHubActions && mode == "platform"
}

// CheckEnvironment checks what the hub channel read of the environment
// touchmark-distribute (Deployment branches and tags set to Selected
// branches, with the hub's default branch only, so that no other branch's
// workflow sees the write key). readErr is the channel's failure to read it.
// It fails closed: whatever does not show that only the default branch may
// use the environment is an error, and distribute exits 2 (a guard).
//
//   - No environment (hubch.ErrNoEnvironment), no deployment branch policy
//     (any ref may use it), a tag policy, or a branch policy other than
//     the default branch.
//   - Protected branches only: GitHub lets every branch deploy to such an
//     environment when the repository protects no branch (docs), and a
//     branch a ruleset protects may count as none; which branches are
//     protected is not readable with the hub's token.
//   - A failure to read it (a workflow whose permissions leave out
//     actions: read, a 5xx). The probe (TOUCHMARK_KEY_EXPOSED) does not
//     make up for it: its job runs without the environment, so it never
//     sees the environment's own secrets, which are what a misconfigured
//     environment exposes.
//
// The way out of the check is security.write_isolation external (a vault)
// or none with a reason (EnvironmentGuardApplies is then false). warning
// is always "" now; it stays for callers that print one. Custom policies
// without any branch let no branch run the job: nothing is exposed.
func CheckEnvironment(env hubch.Environment, readErr error, defaultBranch string) (warning string, err error) {
	name := distributeEnvironment
	switch {
	case errors.Is(readErr, hubch.ErrNoEnvironment):
		return "", fmt.Errorf("the hub has no environment %s: keep the write key in its secrets and run distribute in it, "+
			"with Deployment branches and tags set to the default branch only (security.write_isolation: platform; see "+docsurl.WriteIsolation+")", name)
	case readErr != nil:
		return "", fmt.Errorf("the environment %s could not be read (%v), so nothing shows that only the default branch may use it "+
			"and see the write key: give the workflow's GITHUB_TOKEN the actions: read permission and run again, or set "+
			"security.write_isolation to external or to none with a reason; see "+docsurl.WriteIsolation+"", name, readErr)
	case env.AllRefs:
		return "", fmt.Errorf("the environment %s lets any branch and tag use it, and so see the write key: "+
			"set Deployment branches and tags to Selected branches, with %s only; see "+docsurl.WriteIsolation+"", name, guardRef(defaultBranch))
	case len(env.Policies) == 0 && env.ProtectedBranches:
		return "", fmt.Errorf("the environment %s lets every protected branch use it, and so see the write key: which branches are "+
			"protected is not readable here, and with no branch protection rule every branch may: set Deployment branches and tags "+
			"to Selected branches, with %s only; see "+docsurl.WriteIsolation+"", name, guardRef(defaultBranch))
	}
	var extra []string
	for _, p := range env.Policies {
		if p.Type != "branch" || p.Name != defaultBranch || defaultBranch == "" {
			extra = append(extra, p.Type+" "+p.Name)
		}
	}
	if len(extra) > 0 {
		return "", fmt.Errorf("the environment %s lets %s use it besides the default branch %s, and so see the write key: "+
			"keep only the default branch in its deployment policies; see "+docsurl.WriteIsolation+"", name, strings.Join(extra, ", "), guardRef(defaultBranch))
	}
	return "", nil
}

// guardVersion formats a git version.
// GitGuard refuses a git older than gitx.DeliveryMinVersion for command:
// plan and distribute build and check each target's commit in a blobless
// repository of their own, which needs lazy fetches off, check-attr
// --source and --attr-source. An older git fetches what it misses from the
// target instead of failing, or fails on every target with a change.
func GitGuard(command string, v [3]int) error {
	if slices.Compare(v[:], gitx.DeliveryMinVersion[:]) < 0 {
		return fmt.Errorf("git %s is too old: %s needs git %s or newer (lazy fetches off, check-attr --source, --attr-source)",
			guardVersion(v), command, guardVersion(gitx.DeliveryMinVersion))
	}
	return nil
}

func guardVersion(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// guardRef returns s, or "an unknown ref" when it is empty.
func guardRef(s string) string {
	if s == "" {
		return "an unknown ref"
	}
	return s
}

// ProbeResult is what `touchmark probe` found.
type ProbeResult struct {
	// Exposed lists the write-key variables this job can see.
	Exposed []string
}

// probeVarRe matches, uppercased, the name of a write credential or a
// signing key of any provider, or of the short form: TOUCHMARK_[<ID>_]WRITE_*
// and TOUCHMARK_[<ID>_]SIGNING_KEY.
var probeVarRe = regexp.MustCompile(`^TOUCHMARK_(?:[A-Z0-9_]+_)?(?:WRITE_[A-Z0-9_]+|SIGNING_KEY)$`)

// Probe looks for write keys in the environment of a job that any branch of
// the hub can run (a GitLab merge request pipeline): every
// TOUCHMARK_[<ID>_]WRITE_* and *_SIGNING_KEY variable. The CLI exits 2 when
// Exposed is not empty.
//
// A variable counts when its value is not blank; names compare ignoring
// case (as Windows does), and Exposed lists them as the environment spells
// them, sorted, each once. Values are never kept. The short names
// (TOUCHMARK_WRITE_TOKEN, …) are also looked up through getenv, for an
// environment that environ does not list. Nil functions read nothing.
func Probe(getenv func(string) string, environ func() []string) ProbeResult {
	var names []string
	if environ != nil {
		for _, kv := range environ() {
			name, value, _ := strings.Cut(kv, "=")
			if name != "" && probeVarRe.MatchString(strings.ToUpper(name)) && strings.TrimSpace(value) != "" {
				names = append(names, name)
			}
		}
	}
	if getenv != nil {
		for _, name := range []string{"TOUCHMARK_WRITE_TOKEN", "TOUCHMARK_WRITE_APP_ID", "TOUCHMARK_WRITE_APP_KEY", "TOUCHMARK_SIGNING_KEY"} {
			if strings.TrimSpace(getenv(name)) != "" && !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) }) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return ProbeResult{Exposed: slices.Compact(names)}
}
