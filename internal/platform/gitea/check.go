package gitea

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// The scopes a writer's token needs: pushes and pull
// requests are repository writes; comments and labels issue writes;
// organizations are listed and users looked up with the read scopes.
var (
	neededWriteScopes = []string{"write:repository", "write:issue"}
	neededReadScopes  = []string{"read:organization", "read:user"}
)

// apiToken is GET /api/v1/token: the token the request carries (Gitea
// ≥ 1.27; Gitea 1.26 and Forgejo 15 and 16 answer 404).
type apiToken struct {
	Scopes []string `json:"scopes"`
}

// apiRepoBranch is GET /repos/{owner}/{repo}/branches/{branch}: whether a
// protection rule covers the branch and whether the caller may push to it.
type apiRepoBranch struct {
	Name         string `json:"name"`
	Protected    bool   `json:"protected"`
	UserCanPush  bool   `json:"user_can_push"`
	EffectiveRef string `json:"effective_branch_protection_name"`
}

// apiKey is one SSH key of GET /user/keys.
type apiKey struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	KeyType string `json:"key_type"`
}

// Check reports the writer's checks for doctor (platform.Checker).
//
// With no repository, the identity's: "token-expiry" is ok (Gitea and
// Forgejo tokens have no expiry date); "scopes" from GET /api/v1/token on
// Gitea ≥ 1.27 (write:repository and write:issue are needed, read:
// organization and read:user expected; more is a warning), unknown where
// that endpoint is missing; "2fa" is unknown: the API does not show it.
//
// With repositories, per repository: "access" from the writer's
// permissions on it (push, or admin, which is more than it needs: a
// warning), fail when it cannot see it; "rules" from the branch API of
// each of branches that exists (protected and user_can_push): a protected
// sync branch the writer may not push to fails (blocked:rules:
// protected-branch), one it may push to warns (force pushes that rebuild
// it are refused), an unprotected one is ok; when none exists yet the rules
// are unknown: branch_protections answers the writer 403 and the branch
// API knows existing branches only.
func (w *writer) Check(ctx context.Context, repos []platform.Repo, branches []string) ([]platform.Finding, error) {
	if len(repos) == 0 {
		return w.identityChecks(ctx)
	}
	var out []platform.Finding
	for _, r := range repos {
		fs, err := w.repoChecks(ctx, r, branches)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

// identityChecks are the checks of the writer's token and account.
func (w *writer) identityChecks(ctx context.Context) ([]platform.Finding, error) {
	const op = "check the token"
	if _, err := w.c.selfAccount(ctx); err != nil {
		return nil, err
	}
	out := []platform.Finding{{Check: "token-expiry", Status: platform.FindingOK,
		Detail: "Gitea and Forgejo access tokens have no expiry date"}}
	var tok apiToken
	_, err := w.c.get(ctx, op, w.c.endpoint("token"), nil, &tok)
	switch {
	case err == nil:
		out = append(out, scopesFinding(tok.Scopes))
	case stops(err):
		return nil, err
	case platform.ClassOf(err) == platform.ClassNotFound:
		out = append(out, platform.Finding{Check: "scopes", Status: platform.FindingUnknown,
			Detail: "this server does not show a token's scopes (GET /api/v1/token is in Gitea 1.27 and later)"})
	default:
		out = append(out, platform.Finding{Check: "scopes", Status: platform.FindingUnknown, Detail: err.Error()})
	}
	out = append(out, platform.Finding{Check: "2fa", Status: platform.FindingUnknown,
		Detail: "Gitea and Forgejo do not show whether an account uses two-factor authentication"})
	return out, nil
}

// scopesFinding grades the scopes of the writer's token.
func scopesFinding(scopes []string) platform.Finding {
	has := func(s string) bool {
		category := strings.TrimPrefix(strings.TrimPrefix(s, "write:"), "read:")
		return slices.Contains(scopes, s) || slices.Contains(scopes, "all") ||
			(strings.HasPrefix(s, "read:") && slices.Contains(scopes, "write:"+category))
	}
	var missing, extra []string
	for _, s := range neededWriteScopes {
		if !has(s) {
			missing = append(missing, s)
		}
	}
	for _, s := range scopes {
		if s == "all" || strings.HasPrefix(s, "write:") && !slices.Contains(neededWriteScopes, s) {
			extra = append(extra, s)
		}
	}
	var lacking []string
	for _, s := range neededReadScopes {
		if !has(s) {
			lacking = append(lacking, s)
		}
	}
	list := strings.Join(scopes, ", ")
	switch {
	case len(missing) > 0:
		return platform.Finding{Check: "scopes", Status: platform.FindingFail,
			Detail: fmt.Sprintf("the token lacks %s (it has %s): the writer cannot push or write pull requests", strings.Join(missing, ", "), list)}
	case len(extra) > 0:
		return platform.Finding{Check: "scopes", Status: platform.FindingWarn,
			Detail: fmt.Sprintf("the token has %s, more than the writer needs (write:repository, write:issue, read:organization, read:user): a leaked key could do more", strings.Join(extra, ", "))}
	case len(lacking) > 0:
		return platform.Finding{Check: "scopes", Status: platform.FindingWarn,
			Detail: fmt.Sprintf("the token lacks %s: organizations cannot be listed or authors looked up", strings.Join(lacking, ", "))}
	}
	return platform.Finding{Check: "scopes", Status: platform.FindingOK, Detail: list}
}

// repoChecks are the checks of one repository.
func (w *writer) repoChecks(ctx context.Context, r platform.Repo, branches []string) ([]platform.Finding, error) {
	const op = "check"
	find := func(check string, status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: r.Path, Check: check, Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	u := ""
	if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil && id > 0 {
		u = w.c.endpoint("repositories", r.ID)
	} else {
		owner, name, err := repoPath(op, r)
		if err != nil {
			return nil, err
		}
		u = w.c.endpoint("repos", owner, name)
	}
	var repo apiRepo
	_, err := w.c.get(ctx, op, u, nil, &repo)
	switch {
	case err == nil:
	case stops(err):
		return nil, err
	case platform.ClassOf(err) == platform.ClassNotFound:
		return []platform.Finding{find("access", platform.FindingFail, "the writer does not see this repository")}, nil
	default:
		return []platform.Finding{find("access", platform.FindingUnknown, "%v", err)}, nil
	}
	if err := repo.check(op); err != nil {
		return []platform.Finding{find("access", platform.FindingUnknown, "%v", err)}, nil
	}
	var out []platform.Finding
	p := repo.Permissions
	switch {
	case p == nil:
		out = append(out, find("access", platform.FindingUnknown, "the server did not show the writer's permissions"))
	case p.Admin:
		out = append(out, find("access", platform.FindingWarn, "the writer administers the repository: Write is enough, and a leaked key could change its settings and branch protection"))
	case p.Push:
		out = append(out, find("access", platform.FindingOK, "the writer may push and write pull requests"))
	default:
		out = append(out, find("access", platform.FindingFail, "the writer may not push: give it Write through a team"))
	}
	owner, name, _ := splitRepoPath(repo.FullName)
	rules, err := w.branchRules(ctx, op, owner, name, branches)
	if err != nil {
		if stops(err) {
			return nil, err
		}
		rules = find("rules", platform.FindingUnknown, "%v", err)
	}
	rules.Repo = r.Path
	return append(out, rules), nil
}

// branchRules grades the protection of the sync branches that exist.
func (w *writer) branchRules(ctx context.Context, op, owner, name string, branches []string) (platform.Finding, error) {
	var notes []string
	status := platform.FindingOK
	seen := 0
	for _, b := range uniqueBranches(branches) {
		var br apiRepoBranch
		_, err := w.c.get(ctx, op, w.c.endpoint("repos", owner, name, "branches", b), nil, &br)
		switch {
		case err == nil:
		case platform.ClassOf(err) == platform.ClassNotFound:
			continue
		default:
			return platform.Finding{}, err
		}
		seen++
		switch {
		case br.Protected && !br.UserCanPush:
			status = platform.FindingFail
			notes = append(notes, fmt.Sprintf("branch %s is protected and the writer may not push to it: blocked:rules:protected-branch", b))
		case br.Protected:
			if status == platform.FindingOK {
				status = platform.FindingWarn
			}
			notes = append(notes, fmt.Sprintf("branch %s is protected: a force push that rebuilds it is refused", b))
		}
	}
	switch {
	case seen == 0 && len(branches) > 0:
		return platform.Finding{Check: "rules", Status: platform.FindingUnknown,
			Detail: "no sync branch exists yet, and the writer can read the protection of existing branches only"}, nil
	case len(notes) == 0:
		return platform.Finding{Check: "rules", Status: platform.FindingOK, Detail: "no protection rule covers the sync branch"}, nil
	}
	return platform.Finding{Check: "rules", Status: status, Detail: strings.Join(notes, "; ")}, nil
}

// uniqueBranches returns the non-empty branches, each once, in order.
func uniqueBranches(branches []string) []string {
	var out []string
	for _, b := range branches {
		if b != "" && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// maxKeyPages bounds the writer's SSH keys read: 500 at the default page
// size.
const maxKeyPages = 10

// CheckSigningKey reports whether publicKey is one of the writer's SSH keys
// (GET /user/keys, read:user). Gitea and Forgejo show a commit signed with
// an SSH key as verified only when the key is its owner's and verified,
// which the API does not show: a key found is unknown, one missing fails.
func (w *writer) CheckSigningKey(ctx context.Context, publicKey string) (platform.Finding, error) {
	const op = "list the writer's keys"
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return platform.Finding{}, err
	}
	want := keyBlob(publicKey)
	found := ""
	complete, err := listAll(ctx, w.c, op, w.c.endpoint("user", "keys"), nil, listOpts{maxPages: maxKeyPages}, func(k apiKey) error {
		if want != "" && keyBlob(k.Key) == want && found == "" {
			found = k.Title
		}
		return nil
	})
	switch {
	case err != nil && stops(err):
		return platform.Finding{}, err
	case err != nil:
		return platform.Finding{Check: "signing-key", Status: platform.FindingUnknown, Detail: err.Error()}, nil
	case found != "":
		return platform.Finding{Check: "signing-key", Status: platform.FindingUnknown,
			Detail: fmt.Sprintf("the key is %s's (%q); the platform shows its signatures as verified only once the key is verified in the web UI, which the API does not show", self.Login, found)}, nil
	case !complete:
		return platform.Finding{Check: "signing-key", Status: platform.FindingUnknown, Detail: "the writer has more keys than touchmark reads"}, nil
	}
	return platform.Finding{Check: "signing-key", Status: platform.FindingFail,
		Detail: fmt.Sprintf("the signing key is not among %s's SSH keys: its signatures are not verified; add it to the account", self.Login)}, nil
}

// keyBlob returns the type and base64 blob of an authorized_keys line,
// without its comment; "" for anything else.
func keyBlob(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return ""
	}
	return f[0] + " " + f[1]
}

var (
	_ platform.Checker    = (*writer)(nil)
	_ platform.KeyChecker = (*writer)(nil)
)
