package bitbucketdc

import (
	"context"
	"fmt"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Check reports the writer's checks for doctor (platform.Checker).
//
// With no repository, the identity's: "token-expiry" and "scopes" are
// unknown, since the API does not tell which of a user's HTTP access
// tokens a request came with (the user's Manage account page shows their
// expiry and permissions), and so is "2fa"; the user itself is read first
// (Self), so a refused token fails the call.
//
// With repositories, per repository: "access" from the writer's permission
// on it (REPO_WRITE ok; REPO_ADMIN warns: more than it needs; less fails),
// fail when it does not see the repository; "rules" unknown: branch
// permissions are readable with admin rights only, so a push meets them.
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

// identityChecks are the checks of the writer's token and user.
func (w *writer) identityChecks(ctx context.Context) ([]platform.Finding, error) {
	if _, err := w.c.selfAccount(ctx); err != nil {
		return nil, err
	}
	return []platform.Finding{
		{Check: "token-expiry", Status: platform.FindingUnknown,
			Detail: "Bitbucket Data Center's API does not tell which HTTP access token a request came with; the writer's Manage account > HTTP access tokens page shows when each expires"},
		{Check: "scopes", Status: platform.FindingUnknown,
			Detail: "Bitbucket Data Center's API does not show the token's permissions; the writer's token needs repository write, and no admin permission"},
		{Check: "2fa", Status: platform.FindingUnknown,
			Detail: "Bitbucket Data Center's API does not show whether a user signs in with two-step verification"},
	}, nil
}

// repoChecks are the checks of one repository.
func (w *writer) repoChecks(ctx context.Context, r platform.Repo) ([]platform.Finding, error) {
	const op = "check"
	find := func(check string, status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: r.Path, Check: check, Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	rules := find("rules", platform.FindingUnknown, "branch permissions need repository admin to read; a push meets them")
	repo, err := w.c.targetRepo(ctx, op, r)
	switch {
	case err == nil:
	case stops(err):
		return nil, err
	case platform.ClassOf(err) == platform.ClassNotFound, platform.ClassOf(err) == platform.ClassPermission:
		return []platform.Finding{find("access", platform.FindingFail, "the writer does not see this repository")}, nil
	default:
		return []platform.Finding{find("access", platform.FindingUnknown, "%v", err), rules}, nil
	}
	write, err := w.c.hasPermission(ctx, op, repo, permWrite)
	admin := false
	if err == nil && write {
		admin, err = w.c.hasPermission(ctx, op, repo, permAdmin)
	}
	var access platform.Finding
	switch {
	case err != nil && stops(err):
		return nil, err
	case err != nil:
		access = find("access", platform.FindingUnknown, "%v", err)
	case admin:
		access = find("access", platform.FindingWarn, "the writer administers the repository: write is enough, "+
			"and a leaked token could change its settings and branch permissions")
	case write:
		access = find("access", platform.FindingOK, "the writer may push and write pull requests")
	default:
		access = find("access", platform.FindingFail, "the writer may read the repository, not write to it: it may not push; "+
			"give it write permission (directly, or through a group or the project)")
	}
	return []platform.Finding{access, rules}, nil
}
