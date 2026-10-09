package gitlab

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Checks of doctor.
const (
	// expiryWarning is how long before its expiry a token or key warns.
	expiryWarning = 30 * 24 * time.Hour
	// accessMaintainer is the level above what the writer needs.
	accessMaintainer = 40
	// maxProtectedPages bounds the protected branches of one project read:
	// 2 000.
	maxProtectedPages = 20
	// maxKeyPages bounds the writer's SSH keys read: 1 000.
	maxKeyPages = 10
)

// broadScopes are the scopes of a token that do more than the writer needs.
var broadScopes = []string{"sudo", "admin_mode", "manage_runner", "create_runner", "k8s_proxy", "ai_features"}

// apiTokenSelf is GET /personal_access_tokens/self: the token the request
// carries (personal, group and project access tokens alike).
type apiTokenSelf struct {
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"` // a date, "2026-10-31"; null without one
	Active    bool     `json:"active"`
	Revoked   bool     `json:"revoked"`
}

// apiSelf2FA is the part of GET /user doctor reads.
type apiSelf2FA struct {
	Username   string `json:"username"`
	Bot        *bool  `json:"bot"`
	TwoFactor  *bool  `json:"two_factor_enabled"`
	IsExternal bool   `json:"external"`
}

// apiProtectedBranch is one of GET /projects/:id/protected_branches.
type apiProtectedBranch struct {
	Name           string              `json:"name"`
	AllowForcePush bool                `json:"allow_force_push"`
	Push           []apiProtectedLevel `json:"push_access_levels"`
	Merge          []apiProtectedLevel `json:"merge_access_levels"`
}

// apiProtectedLevel is who may push or merge to a protected branch, or
// create a protected tag: a role (0 is No one, 30 Developers, 40
// Maintainers), a user, a group or a deploy key.
type apiProtectedLevel struct {
	AccessLevel int    `json:"access_level"`
	UserID      *int64 `json:"user_id"`
	GroupID     *int64 `json:"group_id"`
	DeployKeyID *int64 `json:"deploy_key_id"`
}

// apiKey is one of GET /user/keys.
type apiKey struct {
	Title     string  `json:"title"`
	Key       string  `json:"key"`
	UsageType string  `json:"usage_type"` // auth, signing or auth_and_signing
	ExpiresAt *string `json:"expires_at"`
}

// Check reports the writer's checks for doctor (platform.Checker).
//
// With no repository, the identity's: "token-expiry" and "scopes" from
// GET /personal_access_tokens/self (checked live on CE 17.11 to 19.4): an
// expired or revoked token fails, one expiring within 30 days warns; the
// writer needs api (write_repository alone cannot write merge requests),
// and sudo or admin_mode are more than it needs. "2fa" from GET /user: a
// bot or service account signs in with tokens only (ok); a person without
// two-factor authentication warns.
//
// With repositories, per project: "access" from the writer's access level
// (the project's permissions, else members/all as Target reads it):
// Developer is ok, Maintainer or Owner warn (a leaked key could read the
// target's CI variables and change its protected branches), below
// Developer fails. "rules" from the protected branches (Developer reads
// them), the most permissive of those whose name or wildcard covers a sync
// branch deciding, as GitLab does: when none lets the writer push it fails
// (blocked:rules:protected-branch), when none allows force pushes it warns
// (a rebuild is refused), and when only a group the writer may belong to
// could push it is unknown; push rules (EE) show only at push time.
func (w *writer) Check(ctx context.Context, repos []platform.Repo, branches []string) ([]platform.Finding, error) {
	if len(repos) == 0 {
		return w.identityChecks(ctx)
	}
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return nil, err
	}
	var out []platform.Finding
	for _, r := range repos {
		fs, err := w.projectChecks(ctx, self, r, branches)
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
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return nil, err
	}
	var out []platform.Finding
	var tok apiTokenSelf
	_, err = w.c.get(ctx, op, w.c.endpoint("personal_access_tokens", "self"), nil, &tok)
	switch {
	case err == nil:
		out = append(out, expiryFinding(tok, now()), scopesFinding(tok.Scopes))
	case stops(err):
		return nil, err
	default:
		out = append(out,
			platform.Finding{Check: "token-expiry", Status: platform.FindingUnknown, Detail: err.Error()},
			platform.Finding{Check: "scopes", Status: platform.FindingUnknown, Detail: err.Error()})
	}
	var u apiSelf2FA
	_, err = w.c.get(ctx, op, w.c.endpoint("user"), nil, &u)
	switch {
	case err == nil:
		out = append(out, twoFactorFinding(self, u))
	case stops(err):
		return nil, err
	default:
		out = append(out, platform.Finding{Check: "2fa", Status: platform.FindingUnknown, Detail: err.Error()})
	}
	return out, nil
}

// expiryFinding grades the expiry of the writer's token at t.
func expiryFinding(tok apiTokenSelf, t time.Time) platform.Finding {
	f := platform.Finding{Check: "token-expiry"}
	switch {
	case tok.Revoked || !tok.Active:
		f.Status, f.Detail = platform.FindingFail, "the token is revoked or inactive"
		return f
	case tok.ExpiresAt == nil || *tok.ExpiresAt == "":
		f.Status, f.Detail = platform.FindingOK, "the token has no expiry date"
		return f
	}
	day, err := time.Parse(time.DateOnly, *tok.ExpiresAt)
	if err != nil {
		f.Status, f.Detail = platform.FindingUnknown, fmt.Sprintf("the token's expiry %q does not parse", *tok.ExpiresAt)
		return f
	}
	// A token is valid through its expiry date (UTC).
	end := day.Add(24 * time.Hour)
	left := end.Sub(t)
	switch {
	case left <= 0:
		f.Status, f.Detail = platform.FindingFail, "the token expired on "+*tok.ExpiresAt
	case left <= expiryWarning:
		f.Status, f.Detail = platform.FindingWarn, fmt.Sprintf("the token expires on %s, in %d days: rotate it", *tok.ExpiresAt, int(left.Hours()/24))
	default:
		f.Status, f.Detail = platform.FindingOK, "the token expires on "+*tok.ExpiresAt
	}
	return f
}

// scopesFinding grades the scopes of the writer's token.
func scopesFinding(scopes []string) platform.Finding {
	f := platform.Finding{Check: "scopes"}
	list := strings.Join(scopes, ", ")
	var broad []string
	for _, s := range scopes {
		if slices.Contains(broadScopes, s) {
			broad = append(broad, s)
		}
	}
	switch {
	case !slices.Contains(scopes, "api"):
		f.Status, f.Detail = platform.FindingFail, fmt.Sprintf("the token lacks api (it has %s): the writer cannot write merge requests", list)
	case len(broad) > 0:
		f.Status, f.Detail = platform.FindingWarn, fmt.Sprintf("the token has %s, more than the writer needs (api, write_repository): a leaked key could do more", strings.Join(broad, ", "))
	default:
		f.Status, f.Detail = platform.FindingOK, list
	}
	return f
}

// twoFactorFinding grades how the writer's account signs in.
func twoFactorFinding(self platform.Account, u apiSelf2FA) platform.Finding {
	f := platform.Finding{Check: "2fa"}
	switch {
	case self.Kind == platform.KindBot || self.Kind == platform.KindServiceAccount || (u.Bot != nil && *u.Bot):
		f.Status, f.Detail = platform.FindingOK, self.Login+" is a bot or service account: it has no password to sign in with"
	case u.TwoFactor == nil:
		f.Status, f.Detail = platform.FindingUnknown, "GET /user does not show two_factor_enabled"
	case *u.TwoFactor:
		f.Status, f.Detail = platform.FindingOK, self.Login+" signs in with two-factor authentication"
	default:
		f.Status, f.Detail = platform.FindingWarn, self.Login+" is a person's account without two-factor authentication: use a service account, or turn it on"
	}
	return f
}

// projectChecks are the checks of one project.
func (w *writer) projectChecks(ctx context.Context, self platform.Account, r platform.Repo, branches []string) ([]platform.Finding, error) {
	const op = "check"
	find := func(check string, status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: r.Path, Check: check, Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	p, err := w.c.getProject(ctx, op, projectID(r))
	switch {
	case err == nil:
	case stops(err):
		return nil, err
	case platform.ClassOf(err) == platform.ClassNotFound:
		return []platform.Finding{find("access", platform.FindingFail, "the writer does not see this project")}, nil
	default:
		return []platform.Finding{find("access", platform.FindingUnknown, "%v", err)}, nil
	}
	id := strconv.FormatInt(p.ID, 10)
	level, err := w.accessLevel(ctx, op, self, p)
	var out []platform.Finding
	switch {
	case err != nil && stops(err):
		return nil, err
	case err != nil:
		out = append(out, find("access", platform.FindingUnknown, "%v", err))
	case level >= accessMaintainer:
		out = append(out, find("access", platform.FindingWarn, "the writer has access level %d on the project: Developer (30) is enough, "+
			"and a leaked key could read the project's CI variables and change its protected branches", level))
	case level >= accessDeveloper:
		out = append(out, find("access", platform.FindingOK, "the writer is a Developer: it may push and write merge requests"))
	default:
		out = append(out, find("access", platform.FindingFail, "the writer has access level %d, not Developer (30) or higher: it may not push", level))
	}
	rules, err := w.protectedRules(ctx, op, id, self, level, branches)
	if err != nil {
		if stops(err) {
			return nil, err
		}
		rules = platform.Finding{Check: "rules", Status: platform.FindingUnknown, Detail: err.Error()}
	}
	rules.Repo = r.Path
	return append(out, rules), nil
}

// accessLevel is the writer's access level on p, as Target reads it.
func (w *writer) accessLevel(ctx context.Context, op string, self platform.Account, p *apiProject) (int, error) {
	level := 0
	if p.Permissions != nil {
		if a := p.Permissions.ProjectAccess; a != nil {
			level = max(level, a.AccessLevel)
		}
		if a := p.Permissions.GroupAccess; a != nil {
			level = max(level, a.AccessLevel)
		}
	}
	if level >= accessDeveloper {
		return level, nil
	}
	var m apiAccess
	_, err := w.c.get(ctx, op, w.c.projectURL(strconv.FormatInt(p.ID, 10), "members", "all", self.ID), nil, &m)
	switch {
	case err == nil:
		level = max(level, m.AccessLevel)
	case platform.ClassOf(err) == platform.ClassNotFound:
	default:
		return 0, err
	}
	return level, nil
}

// listProtected lists the protected branches of project id; complete is
// false when it has more than touchmark reads.
func (w *writer) listProtected(ctx context.Context, op, id string) (rules []apiProtectedBranch, complete bool, err error) {
	complete, err = listAll(ctx, w.c, op, w.c.projectURL(id, "protected_branches"), nil, maxProtectedPages, func(b apiProtectedBranch) error {
		rules = append(rules, b)
		return nil
	})
	return rules, complete, err
}

// branchVerdict is what the protected branches that match one branch let
// the writer do there. GitLab applies the most permissive of the rules
// that match a branch, to pushes and to force pushes alike.
type branchVerdict struct {
	// rules are the names of the protected branches that match it; none
	// when it is not protected.
	rules []string
	// push is ok when one of them lets the writer push, unknown when none
	// does but one lets a group push, whose members the writer cannot read,
	// and fail otherwise.
	push platform.FindingStatus
	// force is set when one of them allows force pushes.
	force bool
}

// verdictOf grades branch under rules for the writer (selfID, with level on
// the project).
func verdictOf(rules []apiProtectedBranch, branch string, selfID int64, level int) branchVerdict {
	v := branchVerdict{push: platform.FindingOK}
	group := false
	pushes := false
	for _, rule := range rules {
		if !protectedMatch(rule.Name, branch) {
			continue
		}
		v.rules = append(v.rules, rule.Name)
		v.force = v.force || rule.AllowForcePush
		switch canPush(rule.Push, selfID, level) {
		case platform.FindingOK:
			pushes = true
		case platform.FindingUnknown:
			group = true
		}
	}
	switch {
	case len(v.rules) == 0, pushes:
	case group:
		v.push = platform.FindingUnknown
	default:
		v.push = platform.FindingFail
	}
	return v
}

// protectedRules grades the protected branches that cover the sync
// branches, for the writer with level on the project.
func (w *writer) protectedRules(ctx context.Context, op, id string, self platform.Account, level int, branches []string) (platform.Finding, error) {
	rules, complete, err := w.listProtected(ctx, op, id)
	if err != nil {
		return platform.Finding{}, err
	}
	selfID, _ := strconv.ParseInt(self.ID, 10, 64)
	status := platform.FindingOK
	worse := func(s platform.FindingStatus) {
		rank := map[platform.FindingStatus]int{platform.FindingOK: 0, platform.FindingUnknown: 1, platform.FindingWarn: 2, platform.FindingFail: 3}
		if rank[s] > rank[status] {
			status = s
		}
	}
	var notes []string
	for _, b := range branches {
		if b == "" {
			continue
		}
		v := verdictOf(rules, b, selfID, level)
		if len(v.rules) == 0 {
			continue
		}
		names := strings.Join(v.rules, ", ")
		switch {
		case v.push == platform.FindingFail:
			worse(platform.FindingFail)
			notes = append(notes, fmt.Sprintf("protected branch %s keeps the writer from pushing to %s: blocked:rules:protected-branch", names, b))
		case v.push == platform.FindingUnknown:
			worse(platform.FindingUnknown)
			notes = append(notes, fmt.Sprintf("protected branch %s covers %s and lets a group push, whose members the writer cannot read", names, b))
		case !v.force:
			worse(platform.FindingWarn)
			notes = append(notes, fmt.Sprintf("protected branch %s covers %s without force pushes: a rebuild of the sync branch is refused", names, b))
		}
	}
	if !complete {
		worse(platform.FindingUnknown)
		notes = append(notes, "the project has more protected branches than touchmark reads")
	}
	if len(notes) == 0 {
		notes = append(notes, "no protected branch covers the sync branch")
	}
	notes = append(notes, "push rules show only at push time")
	return platform.Finding{Check: "rules", Status: status, Detail: strings.Join(notes, "; ")}, nil
}

// NoPush lists the branches among branches, the default branch aside,
// that protected branches keep the writer from pushing to
// (platform.PushGuard): the rules that match the branch all deny the
// writer, and none lets a group push. One request lists the protected
// branches; the project and the writer's access level are read only when
// one matches. A listing touchmark cannot read whole, or anything else it
// cannot read, leaves the branches out: their pushes find the rules. Push
// rules (EE) show only at push time.
func (w *writer) NoPush(ctx context.Context, r platform.Repo, branches []string) ([]platform.Protected, error) {
	const op = "read protected branches"
	var want []string
	for _, b := range branches {
		if b != "" && b != r.DefaultBranch && !slices.Contains(want, b) {
			want = append(want, b)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	unread := func(err error) ([]platform.Protected, error) {
		if stops(err) {
			return nil, err
		}
		return nil, nil
	}
	id := projectID(r)
	rules, complete, err := w.listProtected(ctx, op, id)
	if err != nil {
		return unread(err)
	}
	if !complete || !slices.ContainsFunc(want, func(b string) bool {
		return slices.ContainsFunc(rules, func(rule apiProtectedBranch) bool { return protectedMatch(rule.Name, b) })
	}) {
		return nil, nil
	}
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return unread(err)
	}
	p, err := w.c.getProject(ctx, op, id)
	if err != nil {
		return unread(err)
	}
	level, err := w.accessLevel(ctx, op, self, p)
	if err != nil {
		return unread(err)
	}
	selfID, _ := strconv.ParseInt(self.ID, 10, 64)
	var out []platform.Protected
	for _, b := range want {
		if v := verdictOf(rules, b, selfID, level); v.push == platform.FindingFail {
			out = append(out, platform.Protected{Branch: b, Rule: strings.Join(v.rules, ", ")})
		}
	}
	return out, nil
}

// protectedMatch reports whether a protected branch name, where '*' is a
// wildcard for any run of characters, covers branch.
func protectedMatch(pattern, branch string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == branch
	}
	var b strings.Builder
	b.WriteString("^")
	for i, part := range strings.Split(pattern, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(branch)
}

// canPush tells whether the push access levels of a protected branch let
// the writer (selfID, with level on the project) push: ok, fail, or
// unknown when only a group the writer may belong to could.
func canPush(levels []apiProtectedLevel, selfID int64, level int) platform.FindingStatus {
	group := false
	for _, l := range levels {
		switch {
		case l.UserID != nil:
			if *l.UserID == selfID {
				return platform.FindingOK
			}
		case l.GroupID != nil:
			group = true
		case l.DeployKeyID != nil:
		case l.AccessLevel > 0 && level >= l.AccessLevel:
			return platform.FindingOK
		}
	}
	if group {
		return platform.FindingUnknown
	}
	return platform.FindingFail
}

// CheckSigningKey reports whether publicKey is one of the writer's SSH keys
// that may sign (GET /user/keys: usage_type signing or auth_and_signing),
// and when it expires.
func (w *writer) CheckSigningKey(ctx context.Context, publicKey string) (platform.Finding, error) {
	const op = "list the writer's keys"
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return platform.Finding{}, err
	}
	want := keyBlob(publicKey)
	var found *apiKey
	complete, err := listAll(ctx, w.c, op, w.c.endpoint("user", "keys"), nil, maxKeyPages, func(k apiKey) error {
		if found == nil && want != "" && keyBlob(k.Key) == want {
			found = &k
		}
		return nil
	})
	f := platform.Finding{Check: "signing-key"}
	switch {
	case err != nil && stops(err):
		return platform.Finding{}, err
	case err != nil:
		f.Status, f.Detail = platform.FindingUnknown, err.Error()
	case found == nil && !complete:
		f.Status, f.Detail = platform.FindingUnknown, "the writer has more keys than touchmark reads"
	case found == nil:
		f.Status, f.Detail = platform.FindingFail, fmt.Sprintf("the signing key is not among %s's SSH keys: GitLab does not verify its signatures; add it with usage type signing", self.Login)
	case found.UsageType == "auth":
		f.Status, f.Detail = platform.FindingFail, fmt.Sprintf("the key %q of %s may authenticate only, not sign: set its usage type to signing", found.Title, self.Login)
	default:
		f.Status, f.Detail = platform.FindingOK, fmt.Sprintf("the key %q is %s's signing key", found.Title, self.Login)
		if found.ExpiresAt != nil && *found.ExpiresAt != "" {
			if at, err := time.Parse(time.RFC3339, *found.ExpiresAt); err == nil {
				switch left := at.Sub(now()); {
				case left <= 0:
					f.Status, f.Detail = platform.FindingFail, fmt.Sprintf("the key %q of %s expired on %s", found.Title, self.Login, at.Format(time.DateOnly))
				case left <= expiryWarning:
					f.Status, f.Detail = platform.FindingWarn, fmt.Sprintf("the key %q of %s expires on %s", found.Title, self.Login, at.Format(time.DateOnly))
				}
			}
		}
	}
	return f, nil
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
	_ platform.PushGuard  = (*writer)(nil)
)
