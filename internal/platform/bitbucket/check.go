package bitbucket

import (
	"context"
	"fmt"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// writerScopes are the scopes of the writer's API token: its account and
// workspaces (GET /2.0/user, /2.0/user/emails, the sweep's
// /2.0/user/workspaces), repositories (reads, the permission listing,
// pushes) and pull requests (reads, creating, editing, declining,
// commenting).
const writerScopes = "read:user:bitbucket, read:workspace:bitbucket, read:repository:bitbucket, write:repository:bitbucket, " +
	"read:pullrequest:bitbucket and write:pullrequest:bitbucket"

// Check reports the writer's checks for doctor (platform.Checker).
//
// With no repository, the identity's: "token-expiry" and "scopes" are
// unknown, since Bitbucket's API shows neither an API token's expiry nor
// its scopes (the account's settings at id.atlassian.com do), and so is
// "2fa", which it does not show either; the account itself is read first
// (GET /2.0/user), so a refused token fails the call.
//
// With repositories, per repository: "access" from the writer's permission
// on it (write ok; admin warns: more than it needs; read or none fails),
// fail when it does not see the repository; "rules" unknown: branch
// restrictions are readable with admin rights only, so a push meets them.
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

// identityChecks are the checks of the writer's token and account.
func (w *writer) identityChecks(ctx context.Context) ([]platform.Finding, error) {
	if _, err := w.c.selfAccount(ctx); err != nil {
		return nil, err
	}
	return []platform.Finding{
		{Check: "token-expiry", Status: platform.FindingUnknown,
			Detail: "Bitbucket's API does not show when an API token expires (at most a year after it was made); the account's API tokens page at id.atlassian.com does"},
		{Check: "scopes", Status: platform.FindingUnknown,
			Detail: "Bitbucket's API does not show an API token's scopes; the writer's token needs " + writerScopes + ", and no admin or delete scope"},
		{Check: "2fa", Status: platform.FindingUnknown,
			Detail: "Bitbucket's API does not show whether an account uses two-step verification"},
	}, nil
}

// repoChecks are the checks of one repository.
func (w *writer) repoChecks(ctx context.Context, r platform.Repo) ([]platform.Finding, error) {
	const op = "check"
	find := func(check string, status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: r.Path, Check: check, Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	rules := find("rules", platform.FindingUnknown, "branch restrictions need admin to read; a push meets them")
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
	ws, _, _ := splitRepoPath(repo.FullName)
	perm, err := w.c.permission(ctx, op, ws, repo.UUID)
	var access platform.Finding
	switch {
	case err != nil && stops(err):
		return nil, err
	case err != nil:
		access = find("access", platform.FindingUnknown, "%v", err)
	case perm == permAdmin:
		access = find("access", platform.FindingWarn, "the writer administers the repository: write is enough, "+
			"and a leaked token could change its settings and branch restrictions")
	case perm == permWrite:
		access = find("access", platform.FindingOK, "the writer may push and write pull requests")
	default:
		access = find("access", platform.FindingFail, "the writer has %s permission, not write: it may not push; "+
			"give it write access to the repository (directly, or through a group or the project)", perm)
	}
	return []platform.Finding{access, rules}, nil
}
