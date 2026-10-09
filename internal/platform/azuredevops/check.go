package azuredevops

import (
	"context"
	"fmt"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Check reports the writer's checks for doctor (platform.Checker).
//
// With no repository, the identity's: "token-expiry" and "scopes" are
// unknown, since reading a PAT's expiry and scopes takes the PAT lifecycle
// API with a Microsoft Entra token, not the PAT itself; "2fa" is unknown
// too, being the directory's business; the identity is read first
// (connectionData), so a refused token fails the call.
//
// With repositories, per repository: "access" from Has Permissions on the
// Git Repositories namespace (contribute, create branches and contribute to
// pull requests: ok; one missing: fail; the API not open to the token:
// unknown), fail when the writer does not see the repository; "rules"
// unknown: branch policies and push policies refuse a push at run time
// (TF402455), and the writer reads none upfront.
func (w *writer) Check(ctx context.Context, repos []platform.Repo, _ []string) ([]platform.Finding, error) {
	if len(repos) == 0 {
		return w.identityChecks(ctx)
	}
	var out []platform.Finding
	for _, r := range repos {
		fs, err := w.repoChecks(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

// identityChecks are the checks of the writer's token and identity.
func (w *writer) identityChecks(ctx context.Context) ([]platform.Finding, error) {
	if _, err := w.c.selfAccount(ctx); err != nil {
		return nil, err
	}
	return []platform.Finding{
		{Check: "token-expiry", Status: platform.FindingUnknown,
			Detail: "a personal access token cannot read its own expiry; the user's Personal access tokens page in Azure DevOps shows it (global PATs stop working on 2026-12-01)"},
		{Check: "scopes", Status: platform.FindingUnknown,
			Detail: "a personal access token cannot read its own scopes; the writer's token needs Code (read & write) for this organization only, and nothing more"},
		{Check: "2fa", Status: platform.FindingUnknown,
			Detail: "Azure DevOps leaves multi-factor authentication to Microsoft Entra ID, which the token cannot read"},
	}, nil
}

// repoChecks are the checks of one repository.
func (w *writer) repoChecks(ctx context.Context, r platform.Repo) ([]platform.Finding, error) {
	const op = "check"
	find := func(check string, status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: r.Path, Check: check, Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	rules := find("rules", platform.FindingUnknown, "branch policies and push policies are met at push time (TF402455); the writer reads none upfront")
	repo, err := w.c.targetRepo(ctx, op, r)
	switch {
	case err == nil:
	case stops(err):
		return nil, err
	case platform.ClassOf(err) == platform.ClassNotFound:
		return []platform.Finding{find("access", platform.FindingFail, "the writer does not see this repository")}, nil
	default:
		return []platform.Finding{find("access", platform.FindingUnknown, "%v", err), rules}, nil
	}
	var missing []string
	for _, p := range []struct {
		bits int
		name string
	}{
		{permContribute, "Contribute"},
		{permCreateBranch, "Create branch"},
		{permPullRequestContib, "Contribute to pull requests"},
	} {
		ok, known, err := w.c.hasPermission(ctx, op, repo, p.bits)
		switch {
		case err != nil && stops(err):
			return nil, err
		case err != nil:
			return []platform.Finding{find("access", platform.FindingUnknown, "%v", err), rules}, nil
		case !known:
			return []platform.Finding{find("access", platform.FindingUnknown,
				"the token may not ask Azure DevOps for its permissions (Permissions - Has Permissions); a push or a pull request meets a missing one"), rules}, nil
		case !ok:
			missing = append(missing, p.name)
		}
	}
	access := find("access", platform.FindingOK, "the writer may push, create branches and contribute to pull requests")
	if len(missing) > 0 {
		access = find("access", platform.FindingFail, "the writer lacks %v on the repository; give its user these Git repository permissions "+
			"(directly, or through a group such as the project's Contributors)", missing)
	}
	return []platform.Finding{access, rules}, nil
}
