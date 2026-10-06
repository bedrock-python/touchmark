package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxRulePages bounds the rules of one branch: 1 000 rules.
const maxRulePages = 10

// apiRule is a rule of GET /repos/{owner}/{repo}/rules/branches/{branch}:
// its type and the ruleset it comes from.
type apiRule struct {
	Type      string `json:"type"`
	RulesetID int64  `json:"ruleset_id"`
}

// Rule types Preflight reports.
const (
	ruleSignatures = "required_signatures"
	ruleNoForce    = "non_fast_forward"
	ruleNoDelete   = "deletion"
)

// Preflight reads the rules of rulesets on branches of r: GET
// /rules/branches/{b} for each branch, which answers for a branch that
// does not exist yet (the sync branch before its first push) and needs
// Metadata read only. required_signatures is SignedCommits,
// non_fast_forward NoForcePush, deletion NoDelete. A 403 or 404 of the
// endpoint is Known false (an identity that cannot read the rules; a
// repository gone); other failures are returned. Classic branch protection
// is not visible without Administration (Caps.RuntimeOnly
// "branch_protection").
//
// rules/branches lists every active rule whoever may bypass it (docs). The
// reader cannot tell whether the writer is on a ruleset's bypass list
// (current_user_can_bypass is the caller's, and bypass_actors shows only
// to those who may edit the ruleset): it reports every rule, and a plan
// may predict a block that distribute does not meet. The reader knows
// nothing of the writer's rights either: WorkflowsKnown stays false.
func (d *reader) Preflight(ctx context.Context, r platform.Repo, branches []string) (platform.Rules, error) {
	return d.c.rules(ctx, r, branches, false)
}

// Preflight is the reader's, but for the rules the writer itself may
// bypass on a branch other than the default one, plus, for a GitHub App,
// whether its installation on r may change workflow files
// (permissions.workflows "write" of the installation: GET
// /repos/{owner}/{repo}/installation the first time, then cached per
// installation for the run). A token's right to workflow files is not
// readable: WorkflowsKnown stays false.
//
// Bypass: for a required_signatures, non_fast_forward or deletion rule on
// a sync branch, GET /repos/{owner}/{repo}/rulesets/{ruleset_id} (once per
// ruleset and call; Metadata read) names the caller's
// current_user_can_bypass: "always" or "exempt" drop the rule (a ruleset
// with the App on its bypass list lets it push as if the rule were not
// there), "pull_requests_only" and "never" keep it, and so does a ruleset
// the writer cannot read. The installation token the writer reads with
// acts as the App, as its per-target token does (unverified: the sandbox
// confirms that a bypassing App reads "always"). The default branch's
// rules stay: the person who merges meets them, not the writer.
func (w *writer) Preflight(ctx context.Context, r platform.Repo, branches []string) (platform.Rules, error) {
	rules, err := w.c.rules(ctx, r, branches, true)
	if err != nil || w.c.kind != credApp {
		return rules, err
	}
	owner, name, _ := splitRepoPath(r.Path)
	perms, err := w.c.app.installationPerms(ctx, "preflight", owner, name)
	switch {
	case err == nil:
		rules.WorkflowsKnown, rules.Workflows = true, perms["workflows"] == "write"
	case platform.ClassOf(err) == platform.ClassNotFound:
		// Not installed on r: Target says so.
	default:
		return platform.Rules{}, err
	}
	return rules, nil
}

// rules reads the branch rules of r; with bypass, the rules of branches
// other than the default one that the caller may bypass are left out
// (writer.Preflight).
func (c *client) rules(ctx context.Context, r platform.Repo, branches []string, bypass bool) (platform.Rules, error) {
	const op = "preflight"
	if err := c.checkHost(op, r); err != nil {
		return platform.Rules{}, err
	}
	owner, name, err := repoPath(op, r)
	if err != nil {
		return platform.Rules{}, err
	}
	a, err := c.ownerAuth(ctx, owner)
	if err != nil {
		return platform.Rules{}, err
	}
	out := platform.Rules{Known: true}
	bypassed := map[int64]bool{}
	for _, b := range uniqueHeads(branches) {
		rules, err := c.branchRules(ctx, op, a, owner, name, b)
		switch {
		case err == nil:
		case platform.ClassOf(err) == platform.ClassNotFound, statusOf(err) == http.StatusForbidden && platform.ClassOf(err) == platform.ClassPermission:
			return platform.Rules{}, nil
		default:
			return platform.Rules{}, err
		}
		for _, rule := range rules {
			if rule.Type != ruleSignatures && rule.Type != ruleNoForce && rule.Type != ruleNoDelete {
				continue
			}
			if bypass && b != r.DefaultBranch && rule.RulesetID > 0 {
				can, seen := bypassed[rule.RulesetID]
				if !seen {
					if can, err = c.canBypass(ctx, op, a, owner, name, rule.RulesetID); err != nil {
						return platform.Rules{}, err
					}
					bypassed[rule.RulesetID] = can
				}
				if can {
					continue
				}
			}
			switch rule.Type {
			case ruleSignatures:
				out.SignedCommits = true
			case ruleNoForce:
				if !slices.Contains(out.NoForcePush, b) {
					out.NoForcePush = append(out.NoForcePush, b)
				}
			case ruleNoDelete:
				if !slices.Contains(out.NoDelete, b) {
					out.NoDelete = append(out.NoDelete, b)
				}
			}
		}
	}
	return out, nil
}

// canBypass reports whether the caller may bypass ruleset id of
// owner/name for pushes: GET /repos/{owner}/{repo}/rulesets/{id} names
// current_user_can_bypass "always" or "exempt". A ruleset the caller
// cannot read (403, 404) or a value the driver does not know counts as no
// bypass; a rate limit, a refused credential or a transient failure is
// returned.
func (c *client) canBypass(ctx context.Context, op string, a *httpx.Auth, owner, name string, id int64) (bool, error) {
	var rs struct {
		CurrentUserCanBypass string `json:"current_user_can_bypass"`
	}
	_, err := c.get(ctx, op, c.repoURL(owner, name, "rulesets", itoa(id)), nil, a, &rs)
	switch {
	case err == nil:
		return rs.CurrentUserCanBypass == "always" || rs.CurrentUserCanBypass == "exempt", nil
	case fatal(err):
		return false, err
	}
	return false, nil
}

// branchRules lists the active rules on branch, one per type and ruleset.
func (c *client) branchRules(ctx context.Context, op string, a *httpx.Auth, owner, name, branch string) ([]apiRule, error) {
	var rules []apiRule
	complete, err := listAll(ctx, c, op, c.repoURL(owner, name, "rules", "branches", branch), nil, a, maxRulePages, func(r apiRule) error {
		if r.Type != "" && !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%s/%s: branch %s has more rules than touchmark reads", owner, name, branch)}
	}
	return rules, nil
}

// Check runs the writer's checks for doctor on each repository: "access"
// (the App is installed on it, or the token may push), "permissions"
// (contents and pull requests write), "workflows" (the installation's
// Workflows permission, unknown for a token) and "rules" (rulesets of
// branches: required signatures, blocked force pushes). Whether the hub is
// visible to the writer is the core's check. A rate limit, a refused
// credential or the end of ctx fails the call.
//
// With no repository it reports the identity's own checks: a GitHub App
// mints its tokens for each run (token-expiry ok) and has no password or
// second factor (2fa ok); a token's expiry and its account's second
// factor are not read (unknown).
func (w *writer) Check(ctx context.Context, repos []platform.Repo, branches []string) ([]platform.Finding, error) {
	if len(repos) == 0 {
		return w.identityChecks(), nil
	}
	var out []platform.Finding
	for _, r := range repos {
		fs, err := w.check(ctx, r, branches)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

// check runs the checks of one repository.
func (w *writer) check(ctx context.Context, r platform.Repo, branches []string) ([]platform.Finding, error) {
	const op = "check"
	owner, name, err := repoPath(op, r)
	if err != nil {
		return nil, err
	}
	find := func(check string, status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: r.Path, Check: check, Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	var out []platform.Finding
	switch w.c.kind {
	case credApp:
		inst, err := w.c.app.repoInstallation(ctx, op, owner, name)
		switch {
		case err == nil:
			out = append(out, find("access", platform.FindingOK, "the GitHub App is installed (installation %d)", inst.ID))
			status, detail := appPermissions(inst.Permissions)
			out = append(out, find("permissions", status, "%s", detail))
			if inst.Permissions["workflows"] == "write" {
				out = append(out, find("workflows", platform.FindingOK, "workflows:write"))
			} else {
				out = append(out, find("workflows", platform.FindingWarn,
					"no Workflows permission: changes to .github/workflows and rebuilds over others' workflow changes are blocked"))
			}
		case stops(err):
			return nil, err
		case platform.ClassOf(err) == platform.ClassNotFound:
			return append(out, find("access", platform.FindingFail, "the GitHub App is not installed on this repository")), nil
		default:
			out = append(out, find("access", platform.FindingUnknown, "%v", err))
		}
	case credToken:
		var repo apiRepo
		_, err := w.c.get(ctx, op, w.c.repoURL(owner, name), nil, w.c.staticAuth("Bearer "+w.c.token), &repo)
		switch {
		case err == nil && repo.Permissions.canPush():
			out = append(out, find("access", platform.FindingOK, "the token's account may push"))
		case err == nil:
			out = append(out, find("access", platform.FindingFail, "the token's account may not push"))
		case stops(err):
			return nil, err
		case platform.ClassOf(err) == platform.ClassNotFound:
			return append(out, find("access", platform.FindingFail, "the token does not see this repository")), nil
		default:
			out = append(out, find("access", platform.FindingUnknown, "%v", err))
		}
		out = append(out, find("workflows", platform.FindingUnknown, "a token's right to change workflow files shows only when a push is refused"))
	default:
		return nil, &platform.Error{Op: op, Class: platform.ClassAuth, Err: errors.New("no credential: the writer is anonymous")}
	}
	rules, err := w.c.rules(ctx, r, branches, true)
	switch {
	case err != nil && stops(err):
		return nil, err
	case err != nil:
		out = append(out, find("rules", platform.FindingUnknown, "%v", err))
	case !rules.Known:
		out = append(out, find("rules", platform.FindingUnknown, "the branch rules are not readable"))
	default:
		var notes []string
		status := platform.FindingOK
		if rules.SignedCommits {
			if w.c.kind == credApp {
				notes = append(notes, "signed commits required: touchmark commits through the API")
			} else {
				notes = append(notes, "signed commits required: set a signing key (a token's API commits are not signed)")
				status = platform.FindingWarn
			}
		}
		if len(rules.NoForcePush) > 0 {
			notes = append(notes, "force pushes blocked on "+strings.Join(rules.NoForcePush, ", ")+": the sync branch cannot be rebuilt there")
			status = platform.FindingWarn
		}
		if len(notes) == 0 {
			notes = append(notes, "no ruleset stands in the way (classic branch protection is not visible)")
		}
		out = append(out, find("rules", status, "%s", strings.Join(notes, "; ")))
	}
	return out, nil
}

// appNeeds are the installation permissions the writer uses: metadata to
// read, contents and pull requests to write, workflows to change workflow
// files.
var appNeeds = map[string]bool{"metadata": true, "contents": true, "pull_requests": true, "workflows": true}

// appDangerous reports whether an installation permission gives a leaked
// key more than the targets' files and pull requests: their settings and
// branch protection, their secrets, their Actions, or the organization's
// members and settings (threat T3 in docs/project/threat-model.md).
func appDangerous(name, level string) bool {
	switch {
	case name == "administration", name == "secrets", name == "members", strings.HasPrefix(name, "organization_"):
		return true
	case name == "actions":
		return level == "write"
	}
	return false
}

// appPermissions grades an installation's permissions (doctor's
// "permissions"): contents:write or pull_requests:write missing fails; a
// permission appDangerous names fails too, since the App's key mints tokens
// with all of the installation's permissions on every repository it is
// installed on (touchmark narrows its own tokens, a leaked key does not);
// any other beyond appNeeds warns.
func appPermissions(perms map[string]string) (platform.FindingStatus, string) {
	var missing, dangerous, extra []string
	for _, p := range []string{"contents", "pull_requests"} {
		if perms[p] != "write" {
			missing = append(missing, p+":write")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(perms)) {
		level := perms[name]
		switch {
		case appNeeds[name] || level == "" || level == "none":
		case appDangerous(name, level):
			dangerous = append(dangerous, name+":"+level)
		default:
			extra = append(extra, name+":"+level)
		}
	}
	status := platform.FindingOK
	var notes []string
	if len(missing) > 0 {
		status = platform.FindingFail
		notes = append(notes, "the installation lacks "+strings.Join(missing, ", "))
	}
	more := slices.Concat(dangerous, extra)
	if len(more) > 0 {
		if status == platform.FindingOK {
			status = platform.FindingWarn
		}
		if len(dangerous) > 0 {
			status = platform.FindingFail
		}
		notes = append(notes, "the installation has more than touchmark needs: "+strings.Join(more, ", ")+
			"; the App's key mints tokens with all of them on every repository it is installed on: "+
			"keep contents, pull_requests and workflows (write) and metadata")
	}
	if len(notes) == 0 {
		notes = append(notes, "contents:write, pull_requests:write")
	}
	return status, strings.Join(notes, "; ")
}

// identityChecks are the checks of the writer's credential itself.
func (w *writer) identityChecks() []platform.Finding {
	if w.c.kind == credApp {
		return []platform.Finding{
			{Check: "token-expiry", Status: platform.FindingOK, Detail: "a GitHub App mints short-lived tokens for each run from its key"},
			{Check: "2fa", Status: platform.FindingOK, Detail: "a GitHub App has no password or second factor"},
		}
	}
	return []platform.Finding{
		{Check: "token-expiry", Status: platform.FindingUnknown, Detail: "touchmark does not read a token's expiry on GitHub; a GitHub App avoids it"},
		{Check: "2fa", Status: platform.FindingUnknown, Detail: "touchmark does not read the second factor of a token's account"},
	}
}
