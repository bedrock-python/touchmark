package distribute

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/docsurl"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The hub's checks of doctor: where the write key lives, as the job of
// doctor can tell (IsolationChecks), and as a maintainer's token reads it
// (KeyLocationChecks, doctor --hub-token).

// DoctorGuard runs the guards of doctor before any platform call: in CI
// (a CI hubch recognizes, or CI set), the job builds the hub's default
// branch for no pull request, and on GitLab, Bitbucket and Azure
// Pipelines in the environment touchmark-distribute, as distribute's (the
// writer is used on the default branch only). A local run passes.
func DoctorGuard(c hubch.Context, getenv func(string) string) error {
	if !guardInCI(c, getenv) {
		return nil
	}
	return errors.Join(guardContextOf("doctor", c)...)
}

// IsolationInput is what IsolationChecks reads.
type IsolationInput struct {
	Context hubch.Context
	Hub     *config.Hub
	// Getenv reads TOUCHMARK_KEY_EXPOSED, the probe job's answer.
	Getenv func(string) string
	// Environment and EnvironmentErr are what the hub channel read of the
	// environment touchmark-distribute on GitHub Actions; EnvironmentRead
	// is set when it was asked.
	Environment     hubch.Environment
	EnvironmentErr  error
	EnvironmentRead bool
}

// IsolationChecks grade security.write_isolation as the job of doctor sees
// it (check write-isolation, and environment on GitHub Actions):
//   - external: ok, the key comes from a store outside the hub's CI;
//   - none: warn, with the reason the hub gave (the risk is accepted);
//   - platform, on GitHub Actions: the probe job's answer in
//     TOUCHMARK_KEY_EXPOSED (false ok, true fail, unset unknown), and the
//     environment touchmark-distribute through the hub channel as
//     distribute checks it (CheckEnvironment; a failure to read it is
//     unknown here);
//   - platform, on GitLab CI: unknown, the probe of the merge request
//     pipelines and doctor --hub-token tell;
//   - platform, on Gitea and Forgejo Actions: fail, their secrets reach
//     every branch's jobs;
//   - platform, on Bitbucket Pipelines: fail without security.reason
//     (BitbucketPlatformRefusal); with it unknown: the API does not show
//     who may deploy, and the reason states it;
//   - platform, on Azure Pipelines: fail without security.reason
//     (AzurePlatformRefusal); with it unknown: the job's token is not
//     known to read the variable group's checks, and the reason states
//     them;
//   - platform, locally: unknown, doctor --hub-token tells.
func IsolationChecks(in IsolationInput) []report.DoctorCheck {
	mode := "platform"
	if in.Hub != nil && in.Hub.Security.WriteIsolation != "" {
		mode = in.Hub.Security.WriteIsolation
	}
	c := report.DoctorCheck{Name: "write-isolation"}
	switch mode {
	case "external":
		c.Status, c.Detail = report.StatusOK, "the write key comes from an external store through the CI's OIDC token (security.write_isolation: external)"
		return []report.DoctorCheck{c}
	case "none":
		reason := ""
		if in.Hub != nil {
			reason = strings.TrimSpace(in.Hub.Security.Reason)
		}
		c.Status, c.Detail = report.StatusWarn, "write isolation is off (security.write_isolation: none): jobs of any branch of the hub may see the write key; reason: "+reason
		return []report.DoctorCheck{c}
	}
	getenv := in.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	switch in.Context.CI {
	case hubch.GitHubActions:
		switch v := strings.ToLower(strings.TrimSpace(getenv(keyExposedVar))); v {
		case "false":
			c.Status, c.Detail = report.StatusOK, "the probe job sees no write key outside the environment "+distributeEnvironment+" ("+keyExposedVar+"=false)"
		case "true":
			c.Status, c.Detail = report.StatusFail, "the probe job sees a write key outside the environment "+distributeEnvironment+
				" ("+keyExposedVar+"=true): a repository or organization secret holds it; keep write keys in the environment only"
		default:
			c.Status, c.Detail = report.StatusUnknown, keyExposedVar+" is not set: this job has no probe answer; doctor --hub-token reads where the secrets are"
		}
		env := report.DoctorCheck{Name: "environment"}
		// A scheduled run names no default branch: it builds it.
		def := in.Context.DefaultBranch
		if def == "" && in.Context.RefIsBranch {
			def = in.Context.RefName
		}
		switch _, err := CheckEnvironment(in.Environment, in.EnvironmentErr, def); {
		case !in.EnvironmentRead:
			env.Status, env.Detail = report.StatusUnknown, "the hub channel cannot read the environment "+distributeEnvironment
		case in.EnvironmentErr != nil && !errors.Is(in.EnvironmentErr, hubch.ErrNoEnvironment):
			env.Status, env.Detail = report.StatusUnknown, fmt.Sprintf("the environment %s could not be read: %v", distributeEnvironment, in.EnvironmentErr)
		case err != nil:
			env.Status, env.Detail = report.StatusFail, err.Error()
		default:
			env.Status, env.Detail = report.StatusOK, "only the default branch may use the environment "+distributeEnvironment
		}
		return []report.DoctorCheck{c, env}
	case hubch.GitLabCI:
		c.Status, c.Detail = report.StatusUnknown, "a job cannot see how its variables are kept: `touchmark probe` in the hub's merge request pipelines "+
			"fails when a write key reaches them, and doctor --hub-token reads the variables"
	case hubch.GiteaActions, hubch.ForgejoActions:
		c.Status, c.Detail = report.StatusFail, "security.write_isolation: platform cannot hold on Gitea and Forgejo Actions, whose secrets every branch's jobs see: "+
			"run distribute on GitHub Actions or GitLab CI, or set write_isolation to external, or to none with a reason; see "+docsurl.WriteIsolation
	case hubch.BitbucketPipelines:
		reason := ""
		if in.Hub != nil {
			reason = strings.TrimSpace(in.Hub.Security.Reason)
		}
		if reason == "" {
			c.Status, c.Detail = report.StatusFail, BitbucketPlatformRefusal
			break
		}
		c.Status, c.Detail = report.StatusUnknown, "Bitbucket's API does not show which branches may deploy to "+distributeEnvironment+
			" (Premium deployment permissions); the hub states: "+reason+". `touchmark probe` in the hub's pipelines fails when a repository "+
			"or workspace variable holds a write key, and doctor --hub-token reads the variables"
	case hubch.AzurePipelines:
		reason := ""
		if in.Hub != nil {
			reason = strings.TrimSpace(in.Hub.Security.Reason)
		}
		if reason == "" {
			c.Status, c.Detail = report.StatusFail, AzurePlatformRefusal
			break
		}
		c.Status, c.Detail = report.StatusUnknown, "a job cannot read the checks of the variable group "+distributeEnvironment+
			"; the hub states: "+reason+". `touchmark probe` in the hub's pull request builds fails when a pipeline variable holds a "+
			"write key, and doctor --hub-token reads the variable groups and their checks"
	default:
		c.Status, c.Detail = report.StatusUnknown, "a local run cannot see where the hub's CI keeps the write key: doctor --hub-token reads it with a maintainer's token"
	}
	return []report.DoctorCheck{c}
}

// writeKeyNameRe matches the name of a secret or variable that holds a
// write key or a signing key by touchmark's names: TOUCHMARK_[<ID>_]WRITE_
// TOKEN, …_WRITE_APP_KEY, …_SIGNING_KEY, and the same without the
// TOUCHMARK_ prefix (a secret the workflow maps to the variable).
var writeKeyNameRe = regexp.MustCompile(`^(?:TOUCHMARK_)?(?:[A-Z0-9_]+_)?(?:WRITE_TOKEN|WRITE_APP_KEY|SIGNING_KEY)$`)

// IsWriteKeyName reports whether a secret or variable called name holds a
// write key: its name is one of touchmark's (compared uppercased, '-' as
// '_'), or it is among known (the secrets the hub's workflows hand
// touchmark as write keys, config.WriteKeySecrets).
func IsWriteKeyName(name string, known []string) bool {
	if slices.ContainsFunc(known, func(k string) bool { return strings.EqualFold(k, name) }) {
		return true
	}
	return writeKeyNameRe.MatchString(strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
}

// KeyLocationChecks grade where the hub keeps its write keys, as a
// maintainer's token read it (doctor --hub-token); known are the secret
// names the hub's workflows hand touchmark as write keys.
//
//   - GitHub: a write key in a repository, organization or Dependabot
//     secret fails (every branch's workflows, or Dependabot's pull
//     requests, read it); one in an environment is ok when only the
//     default branch may use it (CheckEnvironment), and fails otherwise;
//     in another environment than touchmark-distribute it warns too.
//   - GitLab: a write key in a variable that is not protected fails (the
//     pipelines of every branch, merge requests included, read it); a
//     protected one scoped to another environment than
//     touchmark-distribute warns; one not masked warns. Protected branches
//     other than the default one and protected tags that Developers may
//     push or create fail (their pipelines read protected variables), those
//     of Maintainers warn; a minimum role for pipeline variables other than
//     no_one_allowed warns.
//   - Gitea and Forgejo: a write key in an Actions secret fails, unless
//     the hub accepted the risk (none: warn).
//   - Bitbucket: a write key in a repository or workspace variable fails
//     (every branch's pipelines, pull requests included, read it); one in
//     a deployment variable of touchmark-distribute is unknown (the API
//     does not show which branches may deploy), ok when only admins may
//     deploy there, a warning under none, and a warning in another
//     environment; one not secured warns.
//   - Azure DevOps: a write key in a variable of a pipeline that builds
//     the hub fails (every run reads it, of any branch, pull request
//     builds included); one in a variable group is ok when the group has a
//     Branch control check that admits the default branch alone, fails
//     without one or with another branch, is unknown when its checks could
//     not be read, and warns in another group than touchmark-distribute or
//     under none; a check without Verify branch protection, and a key that
//     is not secret, warn; a group every pipeline may use warns, and fails
//     without Verify branch protection. The project's pipeline settings
//     are graded too (azureSettingsChecks).
//
// Under security.write_isolation external no write key is expected in the
// hub, and one found there is graded the same. A key found nowhere is
// unknown: it may live where the token cannot see.
func KeyLocationChecks(ks hubch.KeyStore, known []string, cfg *config.Hub) []report.DoctorCheck {
	mode := "platform"
	if cfg != nil && cfg.Security.WriteIsolation != "" {
		mode = cfg.Security.WriteIsolation
	}
	var out []report.DoctorCheck
	add := func(status report.CheckStatus, format string, args ...any) {
		out = append(out, report.DoctorCheck{Name: "key-location", Status: status, Detail: fmt.Sprintf(format, args...)})
	}
	envs := map[string]hubch.NamedEnvironment{}
	for _, e := range ks.Environments {
		envs[e.Name] = e
	}
	found := 0
	for _, s := range ks.Secrets {
		if !IsWriteKeyName(s.Name, known) {
			continue
		}
		found++
		switch ks.Platform {
		case "github":
			out = append(out, githubKeyCheck(s, envs, ks.DefaultBranch))
		case "gitlab":
			out = append(out, gitlabKeyChecks(s)...)
		case "bitbucket":
			out = append(out, bitbucketKeyChecks(s, envs, mode)...)
		case "azure-devops":
			out = append(out, azureKeyChecks(s, ks, mode)...)
		default:
			if mode == "none" {
				add(report.StatusWarn, "Actions secret %s (%s): every branch's jobs read it; the hub accepted the risk (security.write_isolation: none)", s.Name, s.Where)
			} else {
				add(report.StatusFail, "Actions secret %s (%s): Gitea and Forgejo give every branch's jobs the secrets, so anyone who pushes a branch reads the write key", s.Name, s.Where)
			}
		}
	}
	if found == 0 {
		add(report.StatusUnknown, "no secret or variable named like a write key was found where this token can see; the key may live elsewhere (an organization secret not shared with the hub, a parent group, an external store)")
	}
	if ks.Platform == "github" || ks.Platform == "bitbucket" {
		switch _, ok := envs[distributeEnvironment]; {
		case ok || mode != "platform":
		case ks.EnvironmentsUnread != "":
			add(report.StatusUnknown, "whether the hub has the environment %s is not known: %s", distributeEnvironment, ks.EnvironmentsUnread)
		default:
			add(report.StatusFail, "the hub has no environment %s: keep the write key in its secrets (its deployment variables on Bitbucket), with deployments limited to the default branch", distributeEnvironment)
		}
	}
	if ks.Platform == "azure-devops" && mode == "platform" && !slices.ContainsFunc(ks.VariableGroups, func(g hubch.VariableGroup) bool {
		return strings.EqualFold(g.Name, distributeEnvironment)
	}) {
		if ks.VariableGroupsUnread != "" {
			add(report.StatusUnknown, "whether the hub's project has the variable group %s is not known: %s", distributeEnvironment, ks.VariableGroupsUnread)
		} else {
			add(report.StatusFail, "the hub's project has no variable group %s: keep the write key in it, as a secret, with a Branch control check that admits the default branch only", distributeEnvironment)
		}
	}
	if ks.Platform == "azure-devops" {
		out = append(out, azureSettingsChecks(ks)...)
	}
	if ks.Platform == "gitlab" {
		out = append(out, gitlabRefChecks(ks)...)
	}
	for _, u := range ks.Unread {
		add(report.StatusUnknown, "not readable with this token: %s", u)
	}
	return out
}

// githubKeyCheck grades one write-key secret on GitHub.
func githubKeyCheck(s hubch.Secret, envs map[string]hubch.NamedEnvironment, defaultBranch string) report.DoctorCheck {
	c := report.DoctorCheck{Name: "key-location"}
	switch s.Where {
	case "repository":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("repository secret %s: the workflows of every branch read it; move it into the environment %s", s.Name, distributeEnvironment)
	case "organization":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("organization secret %s is shared with the hub: the workflows of every branch read it; move it into the environment %s", s.Name, distributeEnvironment)
	case "dependabot":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("Dependabot secret %s: the workflows of Dependabot's pull requests read it", s.Name)
	case "environment":
		e := envs[s.Environment]
		_, err := CheckEnvironment(e.Environment, e.Err, defaultBranch)
		switch {
		case e.Err != nil && !errors.Is(e.Err, hubch.ErrNoEnvironment):
			// Distribute's guard fails closed on it; here it is a check that
			// could not be made.
			c.Status, c.Detail = report.StatusUnknown, fmt.Sprintf("secret %s of environment %s: who may use the environment could not be read: %v", s.Name, s.Environment, e.Err)
		case err != nil:
			c.Status, c.Detail = report.StatusFail, fmt.Sprintf("secret %s of environment %s: %s", s.Name, s.Environment, strings.ReplaceAll(err.Error(), distributeEnvironment, s.Environment))
		case s.Environment != distributeEnvironment:
			c.Status, c.Detail = report.StatusWarn, fmt.Sprintf("secret %s is in environment %s, which only the default branch may use; distribute's job uses %s", s.Name, s.Environment, distributeEnvironment)
		default:
			c.Status, c.Detail = report.StatusOK, fmt.Sprintf("secret %s is in environment %s, which only the default branch may use", s.Name, s.Environment)
		}
	default:
		c.Status, c.Detail = report.StatusUnknown, fmt.Sprintf("secret %s (%s)", s.Name, s.Where)
	}
	return c
}

// bitbucketKeyChecks grade one write-key variable on Bitbucket.
func bitbucketKeyChecks(s hubch.Secret, envs map[string]hubch.NamedEnvironment, mode string) []report.DoctorCheck {
	c := report.DoctorCheck{Name: "key-location"}
	where := s.Where + " variable " + s.Name
	switch s.Where {
	case "repository", "workspace":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("%s: the pipelines of every branch, pull requests included, read it; "+
			"move it into the deployment environment %s", where, distributeEnvironment)
	case "environment":
		where = fmt.Sprintf("deployment variable %s of environment %s", s.Name, s.Environment)
		switch {
		case s.Environment != distributeEnvironment:
			c.Status, c.Detail = report.StatusWarn, fmt.Sprintf("%s: distribute's step deploys to %s", where, distributeEnvironment)
		case mode == "none":
			c.Status, c.Detail = report.StatusWarn, where+": any branch's pipeline may deploy there and read it; the hub accepted the risk (security.write_isolation: none)"
		case envs[s.Environment].AdminOnly:
			c.Status, c.Detail = report.StatusOK, where+": only admins may deploy there (restrictions.admin_only), so the step of anyone else pauses"
		default:
			c.Status, c.Detail = report.StatusUnknown, where+": Bitbucket's API does not show which branches may deploy there; with Premium "+
				"restrict it to the default branch (Repository settings > Deployments) and state that in security.reason; without Premium "+
				"any branch's pipeline reads it"
		}
	default:
		c.Status, c.Detail = report.StatusUnknown, fmt.Sprintf("variable %s (%s)", s.Name, s.Where)
	}
	out := []report.DoctorCheck{c}
	if !s.Masked {
		out = append(out, report.DoctorCheck{Name: "key-location", Status: report.StatusWarn,
			Detail: where + " is not secured: its value shows in the settings and the API, and the logs do not mask it; make it Secured"})
	}
	return out
}

// azureKeyChecks grade one write-key variable on Azure DevOps.
func azureKeyChecks(s hubch.Secret, ks hubch.KeyStore, mode string) []report.DoctorCheck {
	c := report.DoctorCheck{Name: "key-location"}
	where := fmt.Sprintf("variable %s of %s %s", s.Name, s.Where, s.Group)
	var extra []report.DoctorCheck
	switch s.Where {
	case "pipeline":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("%s: every run of the pipeline reads it, of any branch, pull request builds included "+
			"(any branch's YAML may map a secret variable); move it into the variable group %s", where, distributeEnvironment)
	case "variable group":
		var g hubch.VariableGroup
		for _, vg := range ks.VariableGroups {
			if vg.Name == s.Group {
				g = vg
				break
			}
		}
		c.Status, c.Detail = azureGroupCheck(where, g, ks.DefaultBranch)
		switch {
		case mode == "none":
			c.Status, c.Detail = report.StatusWarn, where+": the hub accepted the risk that other branches read it (security.write_isolation: none)"
		case !strings.EqualFold(s.Group, distributeEnvironment) && c.Status == report.StatusOK:
			c.Status, c.Detail = report.StatusWarn, fmt.Sprintf("%s: distribute's stage links the variable group %s", where, distributeEnvironment)
		}
		switch {
		case g.PermissionsUnread != "":
			extra = append(extra, report.DoctorCheck{Name: "key-location", Status: report.StatusUnknown,
				Detail: fmt.Sprintf("which pipelines may use variable group %s could not be read: %s", s.Group, g.PermissionsUnread)})
		case g.AllPipelines:
			// Branch control compares the name of the run's branch: the
			// pipeline of another repository of the project, run on its own
			// default branch, passes it unless the check also verifies the
			// branch's protection, and even then a protected default branch
			// of another repository does.
			status := report.StatusWarn
			if !azureVerified(g) {
				status = report.StatusFail
			}
			extra = append(extra, report.DoctorCheck{Name: "key-location", Status: status,
				Detail: fmt.Sprintf("variable group %s is open to every pipeline of the project: Branch control compares only the run's branch name, "+
					"so a pipeline of another repository, run on its own default branch, may link the group and read the key; under Pipeline "+
					"permissions give the hub's pipeline alone the use of it", s.Group)})
		}
	default:
		c.Status, c.Detail = report.StatusUnknown, fmt.Sprintf("variable %s (%s)", s.Name, s.Where)
	}
	out := append([]report.DoctorCheck{c}, extra...)
	if !s.Masked {
		out = append(out, report.DoctorCheck{Name: "key-location", Status: report.StatusWarn,
			Detail: where + " is not secret: its value shows in the Library and the API, every step gets it in its environment, and the logs do not mask it; make it secret"})
	}
	return out
}

// azureVerified reports whether a Branch control check of g verifies the
// branch's protection without letting an unknown status pass.
func azureVerified(g hubch.VariableGroup) bool {
	return slices.ContainsFunc(g.BranchChecks, func(bc hubch.BranchCheck) bool { return bc.Protection && !bc.AllowUnknown })
}

// azureGroupCheck grades the Branch control checks of a variable group that
// holds a write key. A stage passes only when every check passes, so one
// check that admits the default branch alone (refs/heads/<branch>) keeps
// every other branch out: ok when one does and one verifies branch
// protection without letting an unknown status pass; warn when none
// verifies it, or when a check names the branch without refs/heads/ (it
// then admits no run at all); fail when no check admits the default branch
// alone; unknown when the checks could not be read, or a check lists no
// branch (its settings are not in the shape touchmark reads).
func azureGroupCheck(where string, g hubch.VariableGroup, defaultBranch string) (report.CheckStatus, string) {
	if g.ChecksUnread != "" {
		return report.StatusUnknown, where + ": its checks could not be read (" + g.ChecksUnread + "); a maintainer's token with Build: Read reads them"
	}
	if len(g.BranchChecks) == 0 {
		return report.StatusFail, where + ": the group has no Branch control check, so a stage of any branch that links it reads the key: " +
			"add one that admits refs/heads/" + guardRef(defaultBranch) + " only (Pipelines > Library > the group > Approvals and checks)"
	}
	want := "refs/heads/" + defaultBranch
	restricted, bare := false, false
	var others []string
	for _, bc := range g.BranchChecks {
		if len(bc.Allowed) == 0 {
			return report.StatusUnknown, where + ": a Branch control check of the group lists no branch that touchmark can read; check its " +
				"Allowed branches by hand"
		}
		var extra []string
		for _, b := range bc.Allowed {
			switch {
			case defaultBranch != "" && b == want:
			case defaultBranch != "" && b == defaultBranch:
				bare = true
			default:
				extra = append(extra, b)
			}
		}
		if len(extra) == 0 {
			restricted = true
		}
		others = append(others, extra...)
	}
	switch {
	case !restricted:
		slices.Sort(others)
		return report.StatusFail, where + ": its Branch control checks admit " + strings.Join(slices.Compact(others), ", ") +
			" besides the default branch " + guardRef(defaultBranch) + ", whose stages then read the key: admit refs/heads/" + guardRef(defaultBranch) + " only"
	case bare:
		return report.StatusWarn, where + ": a Branch control check names the branch " + defaultBranch + " without refs/heads/, while the check " +
			"compares the run's full ref: write refs/heads/" + defaultBranch + ", or distribute's stage never starts"
	case !azureVerified(g):
		return report.StatusWarn, where + ": a Branch control check admits the default branch " + defaultBranch + " only, but none verifies " +
			"branch protection (or it lets an unknown status pass): turn Verify branch protection on, so the key reaches only a default branch that takes no direct push"
	}
	return report.StatusOK, where + ": a Branch control check admits the default branch " + defaultBranch + " only, and verifies its protection"
}

// azureSettingsChecks grade the Azure DevOps project's pipeline settings
// that bear on the write key (check pipeline-settings): variables settable
// at queue time fail (whoever may queue a run of the default branch sets
// BASH_ENV or DOCKER_HOST in the steps that hold the key); a job scope wider
// than the project, and repositories not limited to those a pipeline
// checks out, warn.
func azureSettingsChecks(ks hubch.KeyStore) []report.DoctorCheck {
	c := report.DoctorCheck{Name: "pipeline-settings"}
	st := ks.PipelineSettings
	if st == nil {
		c.Status, c.Detail = report.StatusUnknown, "the project's pipeline settings could not be read: "+ks.PipelineSettingsUnread
		return []report.DoctorCheck{c}
	}
	var fails, warns []string
	if !st.SettableVarsLimited {
		fails = append(fails, "Limit variables that can be set at queue time is off: whoever may queue a run of the default branch sets any "+
			"variable in the steps that hold the write key (BASH_ENV runs a command, DOCKER_HOST sends the container elsewhere)")
	}
	if !st.JobScopeLimited {
		warns = append(warns, "Limit job authorization scope to current project is off: the job access token reaches every project of the organization")
	}
	if !st.ReposProtected {
		warns = append(warns, "Protect access to repositories in YAML pipelines is off: the job access token reaches every repository of the project")
	}
	switch {
	case len(fails) > 0:
		c.Status, c.Detail = report.StatusFail, strings.Join(append(fails, warns...), "; ")
	case len(warns) > 0:
		c.Status, c.Detail = report.StatusWarn, strings.Join(warns, "; ")
	default:
		c.Status, c.Detail = report.StatusOK, "queue-time variables are limited, and the job access token is limited to the project and the repositories a pipeline checks out"
	}
	return []report.DoctorCheck{c}
}

// gitlabKeyChecks grade one write-key variable on GitLab.
func gitlabKeyChecks(s hubch.Secret) []report.DoctorCheck {
	where := "project variable " + s.Name
	if s.Where == "group" {
		where = "variable " + s.Name + " of group " + s.Group
	}
	c := report.DoctorCheck{Name: "key-location"}
	switch {
	case !s.Protected:
		c.Status, c.Detail = report.StatusFail, where+" is not protected: the pipelines of every branch, merge request pipelines included, read it; make it Protected"
	case s.Scope != distributeEnvironment:
		c.Status, c.Detail = report.StatusWarn, fmt.Sprintf("%s is protected with environment scope %q: every job of a protected branch or tag reads it; scope it to %s", where, s.Scope, distributeEnvironment)
	default:
		c.Status, c.Detail = report.StatusOK, fmt.Sprintf("%s is protected and scoped to the environment %s", where, distributeEnvironment)
	}
	out := []report.DoctorCheck{c}
	if !s.Masked {
		out = append(out, report.DoctorCheck{Name: "key-location", Status: report.StatusWarn,
			Detail: where + " is not masked: a job that prints it shows it in the log; make it Masked and hidden"})
	}
	return out
}

// gitlabRefChecks grade the refs whose pipelines read protected variables,
// and who may set pipeline variables. A listing the token could not read
// leaves its check unknown.
func gitlabRefChecks(ks hubch.KeyStore) []report.DoctorCheck {
	var out []report.DoctorCheck
	branches := report.DoctorCheck{Name: "protected-branches", Status: report.StatusOK, Detail: "only the default branch " + ks.DefaultBranch + " is protected"}
	if ks.ProtectedBranchesUnread != "" {
		branches.Status, branches.Detail = report.StatusUnknown, "not readable with this token: "+ks.ProtectedBranchesUnread
	}
	var bnotes []string
	for _, b := range ks.ProtectedBranches {
		if b.Name == ks.DefaultBranch {
			if slices.ContainsFunc(b.Levels, func(l int) bool { return l > 0 && l <= 30 }) {
				bnotes = append(bnotes, fmt.Sprintf("Developers may push or merge to the default branch %s without review", b.Name))
				branches.Status = worse(branches.Status, report.StatusWarn)
			}
			continue
		}
		switch {
		case slices.ContainsFunc(b.Levels, func(l int) bool { return l > 0 && l <= 30 }):
			bnotes = append(bnotes, fmt.Sprintf("protected branch %s lets %s push or merge: their pipelines read protected variables, the write key included", b.Name, hubch.FormatLevels(b.Levels)))
			branches.Status = worse(branches.Status, report.StatusFail)
		case b.Others:
			bnotes = append(bnotes, fmt.Sprintf("protected branch %s lets users or groups push or merge: their pipelines read protected variables", b.Name))
			branches.Status = worse(branches.Status, report.StatusWarn)
		default:
			bnotes = append(bnotes, fmt.Sprintf("protected branch %s: its pipelines read protected variables; protect the default branch only", b.Name))
			branches.Status = worse(branches.Status, report.StatusWarn)
		}
	}
	if len(bnotes) > 0 {
		branches.Detail = strings.Join(bnotes, "; ")
	}
	out = append(out, branches)
	tags := report.DoctorCheck{Name: "protected-tags", Status: report.StatusOK, Detail: "no protected tag"}
	if ks.ProtectedTagsUnread != "" {
		tags.Status, tags.Detail = report.StatusUnknown, "not readable with this token: "+ks.ProtectedTagsUnread
	}
	var tnotes []string
	for _, t := range ks.ProtectedTags {
		switch {
		case slices.ContainsFunc(t.Levels, func(l int) bool { return l > 0 && l <= 30 }):
			tnotes = append(tnotes, fmt.Sprintf("protected tag %s may be created by %s: its pipelines read protected variables, the write key included", t.Name, hubch.FormatLevels(t.Levels)))
			tags.Status = worse(tags.Status, report.StatusFail)
		case t.Others || slices.ContainsFunc(t.Levels, func(l int) bool { return l > 30 }):
			tnotes = append(tnotes, fmt.Sprintf("protected tag %s may be created by %s: its pipelines read protected variables; let No one create it", t.Name, cmp.Or(hubch.FormatLevels(t.Levels), "users or groups")))
			tags.Status = worse(tags.Status, report.StatusWarn)
		default:
			tnotes = append(tnotes, fmt.Sprintf("protected tag %s: No one may create it", t.Name))
		}
	}
	if len(tnotes) > 0 {
		tags.Detail = strings.Join(tnotes, "; ")
	}
	out = append(out, tags)
	pv := report.DoctorCheck{Name: "pipeline-variables"}
	switch ks.PipelineVariables {
	case "no_one_allowed":
		pv.Status, pv.Detail = report.StatusOK, "no one may run a pipeline with variables"
	case "":
		pv.Status, pv.Detail = report.StatusUnknown, "the minimum role for pipeline variables is not shown to this token"
	default:
		pv.Status, pv.Detail = report.StatusWarn, fmt.Sprintf("the minimum role for pipeline variables is %s: set it to no_one_allowed (Settings > CI/CD > Variables)", ks.PipelineVariables)
	}
	return append(out, pv)
}

// worse returns the worse of two statuses.
func worse(a, b report.CheckStatus) report.CheckStatus {
	rank := map[report.CheckStatus]int{report.StatusOK: 0, report.StatusUnknown: 1, report.StatusWarn: 2, report.StatusFail: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
