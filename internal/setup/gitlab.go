package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// Account kinds on GitLab (GitLabInput.Accounts, Report.Accounts).
const (
	AccountsAuto    = "auto"
	AccountsService = "service-account"
	AccountsGroup   = "group-access-token"
)

// GitLab's access levels.
const (
	glNoOne      = 0
	glReporter   = 20
	glDeveloper  = 30
	glMaintainer = 40
	glOwner      = 50
)

// The pipeline schedules of the template's .gitlab-ci.yml: its doctor job
// runs in schedules whose description contains "doctor", distribute in the
// others. The times are those of the template's GitHub workflow.
var glSchedules = []struct{ description, cron string }{
	{"touchmark distribute", "23 3 * * *"},
	{"touchmark doctor", "47 4 * * 1"},
}

// DefaultTokenDays is how long the tokens setup mints live: GitLab's
// default maximum lifetime of an access token (docs: "the expiry date
// cannot be more than 365 days from today").
const DefaultTokenDays = 365

// GitLabInput is what GitLab setup needs.
type GitLabInput struct {
	// APIURL is the instance's REST base (…/api/v4), Host its host for the
	// report.
	APIURL, Host string
	// Project is the hub project's path, Group the full path of the group
	// whose projects are the targets.
	Project, Group string
	// Token is the maintainer's: a Maintainer of the hub and an Owner of
	// the group (or an administrator).
	Token  string
	Client *httpx.Client
	// Isolation is security.write_isolation: platform or external.
	Isolation string
	// WriterOnHub is security.writer_on_hub: refuse (or "") keeps the hub
	// outside the group of targets; guard lets the writer reach the hub as
	// a Developer, and sets up and checks what then keeps it from getting
	// content onto the hub's default branch.
	WriterOnHub string
	// Accounts is AccountsAuto (service accounts where the instance lets
	// the token create them, else group access tokens), AccountsService or
	// AccountsGroup.
	Accounts string
	// ReaderName and WriterName name the accounts: the usernames of service
	// accounts, or the names of group access tokens (their bots' usernames
	// are GitLab's). Empty: DefaultNames.
	ReaderName, WriterName string
	// ReadVar and WriteVar are the names of the hub's variables.
	ReadVar, WriteVar string
	// Writer is hub.yml's writer ("" when unset): Next says when it must
	// change.
	Writer string
	// TokenDays is the lifetime of the tokens setup mints (DefaultTokenDays
	// when 0).
	TokenDays int
	// Schedules creates the two pipeline schedules.
	Schedules bool
	DryRun    bool
	Engine    string
	// Now is time.Now when nil.
	Now func() time.Time
	// Redact gets every token setup mints, with BasicUsers' Basic forms,
	// before it is used.
	Redact     *redact.Registry
	BasicUsers []string
}

// role is the reader or the writer.
type role struct {
	name   string // "reader" or "writer"
	level  int
	scopes []string
}

var (
	glReader = role{"reader", glReporter, []string{"read_api", "read_repository"}}
	glWriter = role{"writer", glDeveloper, []string{"api", "write_repository"}}
)

// DefaultNames returns the default names of the reader and the writer on
// GitLab: service accounts' usernames from the group's path
// ("acme-services-touchmark-reader"); group access tokens' names from the
// hub project's id, so that hubs sharing a group never rotate each other's
// tokens ("touchmark-reader-hub-42").
func DefaultNames(accounts, group string, hubID int64) (reader, writer string) {
	if accounts == AccountsGroup {
		return fmt.Sprintf("touchmark-reader-hub-%d", hubID), fmt.Sprintf("touchmark-writer-hub-%d", hubID)
	}
	slug := strings.ToLower(strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(group))
	slug = strings.Trim(slug, "-")
	const most = 200
	if len(slug) > most {
		slug = strings.TrimRight(slug[:most], "-")
	}
	return slug + "-touchmark-reader", slug + "-touchmark-writer"
}

// glUser is a user of GitLab.
type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
	State    string `json:"state"`
}

// glProject is the hub project.
type glProject struct {
	ID            int64  `json:"id"`
	Path          string `json:"path_with_namespace"`
	DefaultBranch string `json:"default_branch"`
	Namespace     struct {
		ID       int64  `json:"id"`
		Kind     string `json:"kind"`
		FullPath string `json:"full_path"`
	} `json:"namespace"`
	Permissions struct {
		Project *struct {
			Level int `json:"access_level"`
		} `json:"project_access"`
		Group *struct {
			Level int `json:"access_level"`
		} `json:"group_access"`
	} `json:"permissions"`
	PipelineVariablesRole *string `json:"ci_pipeline_variables_minimum_override_role"`
	RestrictVariables     *bool   `json:"restrict_user_defined_variables"`
	ProtectMRPipelines    *bool   `json:"protect_merge_request_pipelines"`
	// The settings of security.writer_on_hub guard.
	CIConfigPath       *string `json:"ci_config_path"`
	MergeAfterPipeline *bool   `json:"only_allow_merge_if_pipeline_succeeds"`
	MergeOnSkipped     *bool   `json:"allow_merge_on_skipped_pipeline"`
}

// glGroup is a group.
type glGroup struct {
	ID       int64  `json:"id"`
	FullPath string `json:"full_path"`
	ParentID *int64 `json:"parent_id"`
}

// glToken is an access token as GitLab lists it, or a new one (Token).
type glToken struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	UserID      int64    `json:"user_id"`
	Scopes      []string `json:"scopes"`
	Active      bool     `json:"active"`
	Revoked     bool     `json:"revoked"`
	AccessLevel int      `json:"access_level"`
	ExpiresAt   *string  `json:"expires_at"`
	Token       string   `json:"token"`
}

// glVariable is a CI/CD variable of the hub; its value is never decoded.
type glVariable struct {
	Key          string `json:"key"`
	Scope        string `json:"environment_scope"`
	Protected    bool   `json:"protected"`
	Masked       bool   `json:"masked"`
	Hidden       *bool  `json:"hidden"`
	VariableType string `json:"variable_type"`
	// Value is null for a hidden variable.
	Value *string `json:"value"`
}

// glLevel is one access level of a protected branch.
type glLevel struct {
	ID          int64  `json:"id"`
	AccessLevel int    `json:"access_level"`
	UserID      *int64 `json:"user_id"`
	GroupID     *int64 `json:"group_id"`
	DeployKeyID *int64 `json:"deploy_key_id"`
}

// glProtectedBranch is a protected branch.
type glProtectedBranch struct {
	Name           string    `json:"name"`
	Push           []glLevel `json:"push_access_levels"`
	Merge          []glLevel `json:"merge_access_levels"`
	AllowForcePush bool      `json:"allow_force_push"`
}

// glAccount is the reader or the writer as setup found or made it.
type glAccount struct {
	role  role
	name  string // the username (service account) or token name (group access token)
	login string // "" until known
	id    int64  // 0 until known
	// owner is the group that owns the service account, or the group of
	// the group access token.
	owner glGroup
	// tokenID is setup's active token of the account, 0 for none.
	tokenID     int64
	tokenScopes []string
	// fresh is the token GitLab returned when it created the account (a
	// group access token), not yet stored.
	fresh string
	// created is set when this run made (or, in a dry run, would make) the
	// account: a variable cannot hold its token yet.
	created bool
	// skip is set when the account could not be made: its later steps are
	// skipped. refused is set when GitLab refused to create a service
	// account for the token (403 or 404).
	skip    bool
	refused bool
}

// gitlabSetup is one run of GitLab setup.
type gitlabSetup struct {
	in      GitLabInput
	a       *api
	r       *Report
	me      glUser
	major   int
	minor   int
	hub     glProject
	group   glGroup
	top     glGroup
	mode    string
	reader  *glAccount
	writer  *glAccount
	variabs []glVariable
	// inside is set when the hub is inside the group of targets
	// (security.writer_on_hub guard only).
	inside bool
}

// GitLab sets up the hub's GitLab for touchmark (see the package
// documentation). It returns the report of what it did; a
// PreconditionError when the token, the hub or the group is not what setup
// needs, before anything was written; another error when a request failed
// in a way that ends the run (the report holds what was done before).
func GitLab(ctx context.Context, in GitLabInput) (*Report, error) {
	r := newReport("gitlab", in.Engine, in.DryRun)
	r.Host, r.Hub, r.Group, r.Isolation = in.Host, in.Project, in.Group, in.Isolation
	switch in.Isolation {
	case "platform", "external":
	default:
		return r, precondition("setup sets up an isolated write key only: security.write_isolation must be platform or external, not %q", in.Isolation)
	}
	switch in.Accounts {
	case "":
		in.Accounts = AccountsAuto
	case AccountsAuto, AccountsService, AccountsGroup:
	default:
		return r, precondition("accounts %q is not %s, %s or %s", in.Accounts, AccountsAuto, AccountsService, AccountsGroup)
	}
	if in.ReadVar == "" || in.WriteVar == "" {
		return r, precondition("the names of the hub's variables are not known")
	}
	if in.TokenDays <= 0 {
		in.TokenDays = DefaultTokenDays
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	a, err := newAPI(in.Client, in.APIURL, "Private-Token", "", in.Token, nil, in.DryRun)
	if err != nil {
		return r, precondition("%v", err)
	}
	g := &gitlabSetup{in: in, a: a, r: r}
	if err := g.identify(ctx); err != nil {
		return r, err
	}
	if err := g.chooseAccounts(ctx); err != nil {
		return r, err
	}
	g.reader = g.account(ctx, glReader, in.ReaderName)
	if g.reader.refused && in.Accounts == AccountsAuto {
		// The instance lists service accounts but refused to make one for
		// this token: group access tokens.
		failed := r.Steps[len(r.Steps)-1]
		r.Steps = r.Steps[:len(r.Steps)-1]
		g.mode, r.Accounts = AccountsGroup, AccountsGroup
		g.reader = g.account(ctx, glReader, in.ReaderName)
		r.Steps[len(r.Steps)-1].Detail += " (" + failed.Detail + "; group access tokens instead)"
	}
	g.role(ctx, g.reader)
	if in.Isolation == "external" && g.mode == AccountsGroup {
		g.writer = &glAccount{role: glWriter, skip: true}
		r.add("writer-account", StatusManual, "under security.write_isolation external the writer's token lives in your secrets store: "+
			"create a group access token of %s yourself (role Developer, scopes api and write_repository) and keep it there; "+
			"a group access token is its own account, and setup mints no write key it cannot keep in the hub", in.Group)
	} else {
		g.writer = g.account(ctx, glWriter, in.WriterName)
		g.role(ctx, g.writer)
	}
	r.Reader, r.Writer = g.reader.login, g.writer.login
	if g.guard() {
		g.writerGuard(ctx)
	} else {
		g.writerHidden(ctx)
	}
	g.environment(ctx)
	if err := g.readVariables(ctx); err != nil {
		r.add("variables", StatusFail, "cannot list the hub's CI/CD variables: %v", err)
	} else {
		g.readerVariable(ctx)
		g.writerVariable(ctx)
	}
	g.defaultBranch(ctx)
	g.projectSettings(ctx)
	if in.Schedules {
		g.schedules(ctx)
	}
	g.verify(ctx)
	g.nextSteps()
	return r, nil
}

// get and send send one request to the path segments.
func (g *gitlabSetup) get(ctx context.Context, out any, segs ...string) error {
	return g.a.get(ctx, g.a.path(segs...), out)
}

func (g *gitlabSetup) send(ctx context.Context, method string, in, out any, segs ...string) error {
	return g.a.do(ctx, method, g.a.path(segs...), in, out)
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// identify reads who the token is, the hub and the group, and checks that
// setup may go on: a Maintainer of the hub, an Owner of the group, and the
// hub outside the group unless security.writer_on_hub is guard.
func (g *gitlabSetup) identify(ctx context.Context) error {
	in := g.in
	if err := g.get(ctx, &g.me, "user"); err != nil {
		if isStatus(err, http.StatusUnauthorized, http.StatusForbidden) {
			return precondition("GitLab refuses the token: %v", err)
		}
		return err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := g.get(ctx, &v, "version"); err == nil {
		parts := strings.SplitN(strings.SplitN(v.Version, "-", 2)[0], ".", 3)
		if len(parts) >= 2 {
			g.major, _ = strconv.Atoi(parts[0])
			g.minor, _ = strconv.Atoi(parts[1])
		}
	}
	if err := g.get(ctx, &g.hub, "projects", in.Project); err != nil {
		if isStatus(err, http.StatusNotFound, http.StatusForbidden) {
			return precondition("the hub project %s is not visible to %s", in.Project, g.me.Username)
		}
		return err
	}
	g.r.Hub, g.r.HubID = g.hub.Path, id(g.hub.ID)
	level := 0
	if p := g.hub.Permissions.Project; p != nil {
		level = p.Level
	}
	if p := g.hub.Permissions.Group; p != nil {
		level = max(level, p.Level)
	}
	if level < glMaintainer && !g.me.IsAdmin {
		return precondition("%s is not a Maintainer of the hub %s (access level %d): setup needs a Maintainer's token of the hub", g.me.Username, g.hub.Path, level)
	}
	if g.hub.DefaultBranch == "" {
		return precondition("the hub %s has no default branch yet: push the hub's files first", g.hub.Path)
	}
	if err := g.get(ctx, &g.group, "groups", in.Group); err != nil {
		if isStatus(err, http.StatusNotFound, http.StatusForbidden) {
			return precondition("the group %s is not visible to %s", in.Group, g.me.Username)
		}
		return err
	}
	g.r.Group = g.group.FullPath
	ns, gp := strings.ToLower(g.hub.Namespace.FullPath), strings.ToLower(g.group.FullPath)
	g.inside = ns == gp || strings.HasPrefix(ns, gp+"/")
	if g.inside && !g.guard() {
		return precondition("the hub %s is inside the group of targets %s: the writer, a member of that group, would reach the hub "+
			"and could change the packs with a leaked key; keep the hub in a group outside it, or set security.writer_on_hub: guard "+
			"in hub.yml so that setup checks the writer cannot get content onto the hub's default branch", g.hub.Path, g.group.FullPath)
	}
	if !g.me.IsAdmin {
		var m struct {
			Level int `json:"access_level"`
		}
		err := g.get(ctx, &m, "groups", id(g.group.ID), "members", "all", id(g.me.ID))
		if err != nil && !isStatus(err, http.StatusNotFound) {
			return err
		}
		if m.Level < glOwner {
			return precondition("%s is not an Owner of the group %s: creating the reader and the writer and their tokens needs the Owner role", g.me.Username, g.group.FullPath)
		}
	}
	g.top = g.group
	if g.group.ParentID != nil {
		topPath, _, _ := strings.Cut(g.group.FullPath, "/")
		if err := g.get(ctx, &g.top, "groups", topPath); err != nil {
			return err
		}
	}
	who := levelName(level) + " of it"
	if strings.ContainsRune("AEIOU", rune(who[0])) {
		who = "an " + who
	} else {
		who = "a " + who
	}
	if g.me.IsAdmin {
		who = "an administrator"
	}
	version := ""
	if g.major > 0 {
		version = fmt.Sprintf("GitLab %d.%d, ", g.major, g.minor)
	}
	g.r.add("hub", StatusOK, "%s%s, default branch %s; %s is %s", version, g.hub.Path, g.hub.DefaultBranch, g.me.Username, who)
	if g.inside {
		g.r.add("group", StatusOK, "%s holds the targets and the hub (security.writer_on_hub: guard)", g.group.FullPath)
	} else {
		g.r.add("group", StatusOK, "%s holds the targets; the hub is outside it", g.group.FullPath)
	}
	return nil
}

// guard reports whether the run sets up security.writer_on_hub guard.
func (g *gitlabSetup) guard() bool { return g.in.WriterOnHub == "guard" }

// chooseAccounts decides what the reader and the writer are: what the run
// was told; else group access tokens when setup's tokens already exist in
// the group; else service accounts when the instance lists them; else
// group access tokens.
func (g *gitlabSetup) chooseAccounts(ctx context.Context) error {
	g.mode = g.in.Accounts
	if g.mode == AccountsAuto {
		rn, wn := g.in.ReaderName, g.in.WriterName
		dr, dw := DefaultNames(AccountsGroup, g.group.FullPath, g.hub.ID)
		names := []string{cmpOr(rn, dr), cmpOr(wn, dw)}
		tokens, err := list[glToken](ctx, g.a, g.a.path("groups", id(g.group.ID), "access_tokens"), url.Values{"state": {"active"}})
		if err == nil && slices.ContainsFunc(tokens, func(t glToken) bool { return t.Active && !t.Revoked && slices.Contains(names, t.Name) }) {
			g.mode = AccountsGroup
		}
	}
	if g.mode != AccountsGroup {
		_, err := list[glUser](ctx, g.a, g.a.path("groups", id(g.top.ID), "service_accounts"), nil)
		switch {
		case err == nil:
			g.mode = AccountsService
		case isStatus(err, http.StatusNotFound, http.StatusForbidden) && g.mode == AccountsAuto:
			g.mode = AccountsGroup
		case isStatus(err, http.StatusNotFound, http.StatusForbidden):
			return precondition("the group %s has no service accounts this token may use (HTTP %d): GitLab CE has them from 19.x, "+
				"and a self-managed instance lets group Owners create them only with allow_top_level_group_owners_to_create_service_accounts; "+
				"use --accounts %s or %s", g.top.FullPath, statusOf(err), AccountsGroup, AccountsAuto)
		default:
			return err
		}
	}
	g.r.Accounts = g.mode
	return nil
}

// cmpOr returns a unless it is empty, else b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// account finds or makes the reader or the writer and adds its step.
func (g *gitlabSetup) account(ctx context.Context, ro role, name string) *glAccount {
	dr, dw := DefaultNames(g.mode, g.group.FullPath, g.hub.ID)
	if name == "" {
		name = dr
		if ro.name == "writer" {
			name = dw
		}
	}
	acc := &glAccount{role: ro, name: name}
	step := ro.name + "-account"
	var err error
	if g.mode == AccountsService {
		err = g.serviceAccount(ctx, acc)
	} else {
		err = g.groupToken(ctx, acc)
	}
	if err != nil {
		acc.skip, acc.refused = true, errors.Is(err, errRefused)
		g.r.add(step, StatusFail, "%v", err)
		return acc
	}
	what := "service account " + acc.login
	if g.mode == AccountsGroup {
		what = fmt.Sprintf("group access token %q of %s", acc.name, g.group.FullPath)
		if acc.login != "" {
			what += ", bot " + acc.login
		}
	}
	switch {
	case acc.created && g.r.DryRun:
		g.r.add(step, StatusWould, "would create the %s", what)
	case acc.created:
		g.r.add(step, StatusDone, "created the %s (user %d)", what, acc.id)
	default:
		g.r.add(step, StatusOK, "%s (user %d)", what, acc.id)
	}
	return acc
}

// serviceAccount finds the service account acc.name among the service
// accounts of the top-level group and the group, or creates it in the
// top-level group (in the group when the top-level group refuses). Docs:
// https://docs.gitlab.com/api/service_accounts/#group-service-accounts
func (g *gitlabSetup) serviceAccount(ctx context.Context, acc *glAccount) error {
	owners := []glGroup{g.top}
	if g.group.ID != g.top.ID {
		owners = append(owners, g.group)
	}
	for _, owner := range owners {
		users, err := list[glUser](ctx, g.a, g.a.path("groups", id(owner.ID), "service_accounts"), nil)
		if err != nil {
			continue
		}
		for _, u := range users {
			if strings.EqualFold(u.Username, acc.name) {
				acc.id, acc.login, acc.owner = u.ID, u.Username, owner
				return g.ownToken(ctx, acc)
			}
		}
	}
	var users []glUser
	if err := g.a.get(ctx, g.a.path("users")+"?"+url.Values{"username": {acc.name}}.Encode(), &users); err == nil && len(users) > 0 {
		return fmt.Errorf("the user %s exists and is no service account of %s: choose another name with --%s-name", acc.name, g.top.FullPath, acc.role.name)
	}
	acc.created, acc.login = true, acc.name
	if g.r.DryRun {
		acc.owner = g.top
		return nil
	}
	body := map[string]any{"name": "touchmark " + acc.role.name, "username": acc.name}
	var u glUser
	var err error
	for _, owner := range owners {
		if err = g.send(ctx, http.MethodPost, body, &u, "groups", id(owner.ID), "service_accounts"); err == nil {
			acc.id, acc.login, acc.owner = u.ID, u.Username, owner
			return nil
		}
		if !isStatus(err, http.StatusForbidden, http.StatusNotFound) {
			acc.created = false
			return fmt.Errorf("cannot create the service account %s: %w", acc.name, err)
		}
	}
	acc.created = false
	return fmt.Errorf("cannot create the service account %s: %w: %w", acc.name, errRefused, err)
}

// errRefused: GitLab refused to create a service account for the token.
var errRefused = errors.New("GitLab refused it")

// ownToken finds setup's active token of a service account: named after
// the hub, so that each hub rotates its own.
func (g *gitlabSetup) ownToken(ctx context.Context, acc *glAccount) error {
	tokens, err := list[glToken](ctx, g.a, g.a.path("groups", id(acc.owner.ID), "service_accounts", id(acc.id), "personal_access_tokens"),
		url.Values{"state": {"active"}})
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil // before 17.11: no listing, a new token each time
		}
		return fmt.Errorf("cannot list the tokens of %s: %w", acc.login, err)
	}
	name := g.tokenName()
	for _, t := range tokens {
		if t.Name == name && t.Active && !t.Revoked && t.ID > acc.tokenID {
			acc.tokenID, acc.tokenScopes = t.ID, t.Scopes
		}
	}
	return nil
}

// tokenName names the personal access tokens setup mints for service
// accounts: one per hub.
func (g *gitlabSetup) tokenName() string { return fmt.Sprintf("touchmark-hub-%d", g.hub.ID) }

// groupToken finds the group access token acc.name of the group, or
// creates it, with its bot. Docs:
// https://docs.gitlab.com/api/group_access_tokens/
func (g *gitlabSetup) groupToken(ctx context.Context, acc *glAccount) error {
	acc.owner = g.group
	tokens, err := list[glToken](ctx, g.a, g.a.path("groups", id(g.group.ID), "access_tokens"), url.Values{"state": {"active"}})
	if err != nil {
		return fmt.Errorf("cannot list the group access tokens of %s: %w", g.group.FullPath, err)
	}
	var mine []glToken
	for _, t := range tokens {
		if t.Name == acc.name && t.Active && !t.Revoked {
			mine = append(mine, t)
		}
	}
	switch len(mine) {
	case 0:
	case 1:
		t := mine[0]
		if t.AccessLevel != acc.role.level || !sameSet(t.Scopes, acc.role.scopes) {
			return fmt.Errorf("the group access token %q has access level %d and scopes %s, want %d and %s: revoke it and run setup again",
				acc.name, t.AccessLevel, strings.Join(t.Scopes, ", "), acc.role.level, strings.Join(acc.role.scopes, ", "))
		}
		acc.tokenID, acc.tokenScopes, acc.id = t.ID, t.Scopes, t.UserID
		return g.login(ctx, acc)
	default:
		return fmt.Errorf("the group %s has %d active access tokens named %q: revoke all but one", g.group.FullPath, len(mine), acc.name)
	}
	acc.created = true
	if g.r.DryRun {
		return nil
	}
	body := map[string]any{"name": acc.name, "scopes": acc.role.scopes, "access_level": acc.role.level,
		"expires_at": dateAfter(g.in.Now(), g.in.TokenDays), "description": "touchmark " + acc.role.name + " of the hub " + g.hub.Path}
	var t glToken
	if err := g.send(ctx, http.MethodPost, body, &t, "groups", id(g.group.ID), "access_tokens"); err != nil {
		acc.created = false
		return fmt.Errorf("cannot create the group access token %q: %w", acc.name, err)
	}
	g.register(t.Token)
	acc.fresh, acc.tokenID, acc.tokenScopes, acc.id = t.Token, t.ID, t.Scopes, t.UserID
	return g.login(ctx, acc)
}

// login reads the username of acc.id.
func (g *gitlabSetup) login(ctx context.Context, acc *glAccount) error {
	var u glUser
	if err := g.get(ctx, &u, "users", id(acc.id)); err != nil {
		return fmt.Errorf("cannot read the user %d: %w", acc.id, err)
	}
	acc.login = u.Username
	return nil
}

// register adds a token to the run's redaction.
func (g *gitlabSetup) register(token string) {
	g.in.Redact.Add(token, g.in.BasicUsers...)
}

// sameSet reports whether a and b hold the same strings.
func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// levelName names a GitLab access level.
func levelName(l int) string {
	switch l {
	case glNoOne:
		return "No one"
	case glReporter:
		return "Reporter"
	case glDeveloper:
		return "Developer"
	case glMaintainer:
		return "Maintainer"
	case glOwner:
		return "Owner"
	}
	return "level " + strconv.Itoa(l)
}

// role gives a service account its role in the group, and checks the role
// it ends up with (an inherited one may be higher). A group access token's
// bot has the token's role.
func (g *gitlabSetup) role(ctx context.Context, acc *glAccount) {
	step := acc.role.name + "-role"
	want := levelName(acc.role.level)
	switch {
	case acc.skip:
		return
	case g.mode == AccountsGroup:
		g.r.add(step, StatusOK, "%s in %s (the token's role)", want, g.group.FullPath)
		return
	case acc.created && g.r.DryRun:
		g.r.add(step, StatusWould, "would add %s to %s as %s", acc.name, g.group.FullPath, want)
		return
	}
	var m struct {
		Level int `json:"access_level"`
	}
	err := g.get(ctx, &m, "groups", id(g.group.ID), "members", id(acc.id))
	switch {
	case isStatus(err, http.StatusNotFound):
		if g.r.DryRun {
			g.r.add(step, StatusWould, "would add %s to %s as %s", acc.login, g.group.FullPath, want)
			return
		}
		err = g.send(ctx, http.MethodPost, map[string]any{"user_id": acc.id, "access_level": acc.role.level}, nil, "groups", id(g.group.ID), "members")
	case err == nil && m.Level != acc.role.level:
		if g.r.DryRun {
			g.r.add(step, StatusWould, "would change the role of %s in %s from %s to %s", acc.login, g.group.FullPath, levelName(m.Level), want)
			return
		}
		err = g.send(ctx, http.MethodPut, map[string]any{"access_level": acc.role.level}, nil, "groups", id(g.group.ID), "members", id(acc.id))
	case err == nil:
		g.effectiveRole(ctx, acc, StatusOK, fmt.Sprintf("%s in %s", want, g.group.FullPath))
		return
	}
	if err != nil {
		g.r.add(step, StatusFail, "cannot make %s a %s of %s: %v", acc.login, want, g.group.FullPath, err)
		return
	}
	g.effectiveRole(ctx, acc, StatusDone, fmt.Sprintf("made %s a %s of %s", acc.login, want, g.group.FullPath))
}

// effectiveRole adds the role step, failed when the account's role in the
// group, inherited ones included, is not the one it needs.
func (g *gitlabSetup) effectiveRole(ctx context.Context, acc *glAccount, status Status, detail string) {
	step := acc.role.name + "-role"
	var m struct {
		Level int `json:"access_level"`
	}
	if err := g.get(ctx, &m, "groups", id(g.group.ID), "members", "all", id(acc.id)); err == nil && m.Level > acc.role.level {
		g.r.add(step, StatusFail, "%s is a %s of %s through a group above it, more than the %s it needs: lower that membership",
			acc.login, levelName(m.Level), g.group.FullPath, levelName(acc.role.level))
		return
	}
	g.r.add(step, status, "%s", detail)
}

// writerHidden checks that the writer cannot reach the hub, so a leaked
// write key cannot change the packs: no membership of the hub project,
// direct, inherited or shared.
func (g *gitlabSetup) writerHidden(ctx context.Context) {
	w := g.writer
	switch {
	case w.skip:
		return
	case w.id == 0:
		g.r.add("writer-hidden", StatusWould, "would check that the writer is no member of the hub")
		return
	}
	var m struct {
		Level int `json:"access_level"`
	}
	err := g.get(ctx, &m, "projects", id(g.hub.ID), "members", "all", id(w.id))
	switch {
	case isStatus(err, http.StatusNotFound):
		g.r.add("writer-hidden", StatusOK, "%s is no member of the hub", w.login)
	case err != nil:
		g.r.add("writer-hidden", StatusUnknown, "cannot read whether %s is a member of the hub: %v", w.login, err)
	default:
		g.r.add("writer-hidden", StatusFail, "%s reaches the hub as %s (directly, through a group or a share): remove it, "+
			"a leaked write key must not change the packs", w.login, levelName(m.Level))
	}
}

// writerGuard checks, under security.writer_on_hub guard, that the
// writer's role on the hub is below Maintainer: a Maintainer changes the
// hub's protection, settings and variables. The rest of the guard is what
// setup sets up after it (defaultBranch, projectSettings) and checks
// (verify).
func (g *gitlabSetup) writerGuard(ctx context.Context) {
	w := g.writer
	switch {
	case w.skip:
		return
	case w.id == 0:
		g.r.add("writer-guard", StatusWould, "would check that the writer is below Maintainer on the hub")
		return
	}
	var m struct {
		Level int `json:"access_level"`
	}
	err := g.get(ctx, &m, "projects", id(g.hub.ID), "members", "all", id(w.id))
	switch {
	case isStatus(err, http.StatusNotFound):
		g.r.add("writer-guard", StatusOK, "%s is no member of the hub", w.login)
	case err != nil:
		g.r.add("writer-guard", StatusUnknown, "cannot read the role of %s on the hub: %v", w.login, err)
	case m.Level >= glMaintainer:
		g.r.add("writer-guard", StatusFail, "%s reaches the hub as %s, and a Maintainer changes the hub's protection, settings and variables: "+
			"give it the Developer role", w.login, levelName(m.Level))
	default:
		g.r.add("writer-guard", StatusOK, "%s reaches the hub as %s, below Maintainer (security.writer_on_hub: guard)", w.login, levelName(m.Level))
	}
}

// environment creates the environment touchmark-distribute. Docs:
// https://docs.gitlab.com/api/environments/#create-an-environment
func (g *gitlabSetup) environment(ctx context.Context) {
	var envs []struct {
		Name string `json:"name"`
	}
	if err := g.a.get(ctx, g.a.path("projects", id(g.hub.ID), "environments")+"?"+url.Values{"name": {Environment}}.Encode(), &envs); err != nil {
		g.r.add("environment", StatusFail, "cannot list the hub's environments: %v", err)
		return
	}
	for _, e := range envs {
		if e.Name == Environment {
			g.r.add("environment", StatusOK, "the hub has the environment %s", Environment)
			return
		}
	}
	if g.r.DryRun {
		g.r.add("environment", StatusWould, "would create the environment %s", Environment)
		return
	}
	if err := g.send(ctx, http.MethodPost, map[string]any{"name": Environment}, nil, "projects", id(g.hub.ID), "environments"); err != nil {
		g.r.add("environment", StatusFail, "cannot create the environment %s: %v", Environment, err)
		return
	}
	g.r.add("environment", StatusDone, "created the environment %s", Environment)
}

// readVariables lists the hub's variables. The values of the reader's and
// the writer's variables are tokens: they are registered with the
// redaction as soon as they are read.
func (g *gitlabSetup) readVariables(ctx context.Context) error {
	vars, err := list[glVariable](ctx, g.a, g.a.path("projects", id(g.hub.ID), "variables"), nil)
	for _, v := range vars {
		if (v.Key == g.in.ReadVar || v.Key == g.in.WriteVar) && v.Value != nil && strings.TrimSpace(*v.Value) != "" {
			g.register(strings.TrimSpace(*v.Value))
		}
	}
	g.variabs = vars
	return err
}

// variablesNamed returns the hub's variables called key.
func (g *gitlabSetup) variablesNamed(key string) []glVariable {
	var out []glVariable
	for _, v := range g.variabs {
		if v.Key == key {
			out = append(out, v)
		}
	}
	return out
}

// variableURL is the path of a variable of the hub in one scope.
func (g *gitlabSetup) variableURL(key, scope string) string {
	return g.a.path("projects", id(g.hub.ID), "variables", key) + "?" + url.Values{"filter[environment_scope]": {scope}}.Encode()
}

// mint returns a new token of acc: the one GitLab returned when it created
// the account, else a rotation of setup's own token (which revokes the old
// one), else a new token. Every token is registered with the redaction
// before it is returned. stale is the id of a token to revoke once the new
// one is stored (a token of setup's with other scopes), 0 for none. Docs:
// https://docs.gitlab.com/api/group_access_tokens/#rotate-a-group-access-token
// and https://docs.gitlab.com/api/service_accounts/ (personal access
// tokens of a group service account: create and rotate).
func (g *gitlabSetup) mint(ctx context.Context, acc *glAccount) (token string, stale int64, err error) {
	if acc.fresh != "" {
		token, acc.fresh = acc.fresh, ""
		return token, 0, nil
	}
	exp := map[string]any{"expires_at": dateAfter(g.in.Now(), g.in.TokenDays)}
	var t glToken
	switch {
	case g.mode == AccountsGroup:
		err = g.send(ctx, http.MethodPost, exp, &t, "groups", id(g.group.ID), "access_tokens", id(acc.tokenID), "rotate")
	case acc.tokenID != 0 && sameSet(acc.tokenScopes, acc.role.scopes):
		err = g.send(ctx, http.MethodPost, exp, &t, "groups", id(acc.owner.ID), "service_accounts", id(acc.id), "personal_access_tokens", id(acc.tokenID), "rotate")
	default:
		stale = acc.tokenID
		body := map[string]any{"name": g.tokenName(), "scopes": acc.role.scopes, "expires_at": exp["expires_at"],
			"description": "touchmark " + acc.role.name + " of the hub " + g.hub.Path}
		err = g.send(ctx, http.MethodPost, body, &t, "groups", id(acc.owner.ID), "service_accounts", id(acc.id), "personal_access_tokens")
	}
	if err != nil {
		return "", 0, fmt.Errorf("cannot mint a token for %s: %w", acc.login, err)
	}
	if t.Token == "" {
		return "", 0, fmt.Errorf("GitLab minted a token for %s and did not return it", acc.login)
	}
	g.register(t.Token)
	acc.tokenID, acc.tokenScopes = t.ID, t.Scopes
	return t.Token, stale, nil
}

// revokeStale revokes a service account's token setup replaced.
func (g *gitlabSetup) revokeStale(ctx context.Context, acc *glAccount, stale int64) string {
	if stale == 0 {
		return ""
	}
	if err := g.send(ctx, http.MethodDelete, nil, nil, "groups", id(acc.owner.ID), "service_accounts", id(acc.id), "personal_access_tokens", id(stale)); err != nil {
		return fmt.Sprintf("; the old token %d could not be revoked: %v", stale, err)
	}
	return fmt.Sprintf("; revoked the old token %d", stale)
}

// readerVariable keeps the reader's token in a masked variable that is not
// protected and has no environment scope: plan runs in merge request
// pipelines, which protected and scoped variables do not reach.
func (g *gitlabSetup) readerVariable(ctx context.Context) {
	acc, key, step := g.reader, g.in.ReadVar, "reader-variable"
	if acc.skip {
		g.r.add(step, StatusFail, "no reader: the variable %s was not set", key)
		return
	}
	var plain *glVariable
	var scoped []string
	for _, v := range g.variablesNamed(key) {
		if v.Scope == "*" {
			plain = &v
		} else {
			scoped = append(scoped, v.Scope)
		}
	}
	note := ""
	if len(scoped) > 0 {
		note = fmt.Sprintf("; %s is also set for environment %s, which the plan job does not use", key, strings.Join(scoped, ", "))
	}
	if plain != nil && !acc.created {
		if !plain.Protected && plain.Masked && plain.VariableType != "file" {
			g.r.add(step, StatusOK, "%s: masked, not protected, every environment%s", key, note)
			return
		}
		if g.r.DryRun {
			g.r.add(step, StatusWould, "would make %s masked and not protected (merge request pipelines read it)%s", key, note)
			return
		}
		body := map[string]any{"protected": false, "masked": true, "variable_type": "env_var"}
		if err := g.a.do(ctx, http.MethodPut, g.variableURL(key, "*"), body, nil); err != nil {
			g.r.add(step, StatusFail, "cannot update %s: %v", key, err)
			return
		}
		g.r.add(step, StatusDone, "made %s masked and not protected%s", key, note)
		return
	}
	if g.r.DryRun {
		g.r.add(step, StatusWould, "would mint a token for the reader into %s (masked and hidden, not protected)%s", key, note)
		return
	}
	token, stale, err := g.mint(ctx, acc)
	if err != nil {
		g.r.add(step, StatusFail, "%v", err)
		return
	}
	body := map[string]any{"key": key, "value": token, "protected": false, "masked": true, "masked_and_hidden": true,
		"raw": true, "environment_scope": "*", "description": "touchmark: the reader's token (touchmark setup)"}
	if plain != nil {
		// The variable held another token; hidden or not, it gets this one.
		err = g.a.do(ctx, http.MethodPut, g.variableURL(key, "*"), map[string]any{"value": token, "protected": false, "masked": true, "raw": true}, nil)
	} else {
		err = g.send(ctx, http.MethodPost, body, nil, "projects", id(g.hub.ID), "variables")
	}
	if err != nil {
		g.r.add(step, StatusFail, "minted a token for %s and could not store it in %s: %v; the token is not printed, run setup again to replace it", acc.login, key, err)
		return
	}
	g.r.add(step, StatusDone, "stored a new token of %s in %s%s%s", acc.login, key, g.revokeStale(ctx, acc, stale), note)
}

// writerVariable keeps the writer's token in a variable that is protected,
// masked and hidden (masked_and_hidden, GitLab 17.4; only at creation),
// and scoped to the environment touchmark-distribute (docs:
// https://docs.gitlab.com/api/project_level_variables/#create-a-variable);
// under external isolation it checks that the hub keeps no write key.
// A copy in another scope, or a variable that is not protected or not
// masked, may have shown the token: setup mints a new token (rotating its
// own revokes the old one), revokes the token each exposed variable held
// (revokeExposed: it may be no token of setup's, such as one made by
// hand), deletes the copies and makes the variable again.
func (g *gitlabSetup) writerVariable(ctx context.Context) {
	acc, key, step := g.writer, g.in.WriteVar, "writer-variable"
	vars := g.variablesNamed(key)
	if g.in.Isolation == "external" {
		if len(vars) == 0 {
			g.r.add(step, StatusOK, "the hub keeps no %s: under external isolation the writer's token comes from your secrets store", key)
			if !acc.skip && acc.id != 0 {
				g.r.add("writer-token", StatusManual, "mint the writer's token (scopes %s) straight into your secrets store: "+
					"Group > Settings > Service accounts in %s, or POST /groups/%d/service_accounts/%d/personal_access_tokens",
					strings.Join(acc.role.scopes, ", "), acc.owner.FullPath, acc.owner.ID, acc.id)
			}
		} else {
			g.r.add(step, StatusFail, "the hub keeps %s although security.write_isolation is external: delete it from the hub's CI/CD variables", key)
		}
		return
	}
	if acc.skip {
		g.r.add(step, StatusFail, "no writer: the variable %s was not set", key)
		return
	}
	var inScope *glVariable
	var others []glVariable
	for _, v := range vars {
		if v.Scope == Environment {
			inScope = &v
		} else {
			others = append(others, v)
		}
	}
	if inScope != nil && len(others) == 0 && !acc.created && inScope.Protected && inScope.Masked && inScope.VariableType != "file" {
		if inScope.Hidden != nil && !*inScope.Hidden {
			g.r.add(step, StatusWarn, "%s: protected, masked, environment %s; not hidden, so Maintainers can read it in the UI "+
				"(GitLab hides a variable only when it is created)", key, Environment)
			return
		}
		g.r.add(step, StatusOK, "%s: protected, masked and hidden, environment %s", key, Environment)
		return
	}
	var why []string
	switch {
	case acc.created:
		why = append(why, "the writer is new")
	case inScope == nil:
		why = append(why, "missing")
	default:
		if !inScope.Protected {
			why = append(why, "it was not protected")
		}
		if !inScope.Masked || inScope.VariableType == "file" {
			why = append(why, "it was not masked")
		}
	}
	for _, o := range others {
		why = append(why, fmt.Sprintf("a copy with environment scope %q reached jobs outside %s", o.Scope, Environment))
	}
	if g.r.DryRun {
		g.r.add(step, StatusWould, "would mint a token for the writer into %s (protected, masked and hidden, environment %s): %s",
			key, Environment, strings.Join(why, "; "))
		return
	}
	token, stale, err := g.mint(ctx, acc)
	if err != nil {
		g.r.add(step, StatusFail, "%v", err)
		return
	}
	exposed := slices.Clone(others)
	if inScope != nil && (!inScope.Protected || !inScope.Masked || inScope.VariableType == "file") {
		exposed = append(exposed, *inScope)
	}
	removed := g.revokeExposed(ctx, key, exposed, token)
	for _, o := range others {
		if err := g.a.do(ctx, http.MethodDelete, g.variableURL(key, o.Scope), nil, nil); err != nil {
			g.r.add(step, StatusFail, "cannot delete %s with environment scope %q: %v; delete it by hand (the step writer-exposed-token says whether its token is revoked)",
				key, o.Scope, err)
		} else {
			removed = append(removed, fmt.Sprintf("deleted the copy with scope %q", o.Scope))
		}
	}
	if inScope != nil {
		if err := g.a.do(ctx, http.MethodDelete, g.variableURL(key, Environment), nil, nil); err != nil {
			g.r.add(step, StatusFail, "minted a token for %s and could not replace %s: %v; the token is not printed, run setup again", acc.login, key, err)
			return
		}
	}
	body := map[string]any{"key": key, "value": token, "protected": true, "masked": true, "masked_and_hidden": true,
		"raw": true, "environment_scope": Environment, "description": "touchmark: the writer's token (touchmark setup)"}
	var made glVariable
	if err := g.send(ctx, http.MethodPost, body, &made, "projects", id(g.hub.ID), "variables"); err != nil {
		g.r.add(step, StatusFail, "minted a token for %s and could not store it in %s: %v; the token is not printed, run setup again to replace it", acc.login, key, err)
		return
	}
	detail := fmt.Sprintf("stored a new token of %s in %s (protected, masked and hidden, environment %s): %s",
		acc.login, key, Environment, strings.Join(why, "; "))
	if len(removed) > 0 {
		detail += "; " + strings.Join(removed, "; ")
	}
	detail += g.revokeStale(ctx, acc, stale)
	if made.Hidden != nil && !*made.Hidden {
		g.r.add(step, StatusWarn, "%s; GitLab did not hide it (before 17.4 it cannot)", detail)
		return
	}
	g.r.add(step, StatusDone, "%s", detail)
}

// revokeExposed revokes the token each variable of exposed held: a copy of
// the write key in another scope, or one that was not protected or not
// masked. Rotating setup's own token revoked that one already, but the
// variable may have held another, such as a token made by hand, which the
// jobs that could read it may still use. GitLab revokes a token with a
// request authenticated by the token itself, whatever kind it is
// (DELETE /personal_access_tokens/self, docs:
// https://docs.gitlab.com/api/personal_access_tokens/#revoke-a-personal-access-token);
// a token that no longer works answers 401. It returns what it did for the
// writer-variable step, and adds the step writer-exposed-token, failed for
// a token it could not revoke and manual for a variable whose value GitLab
// hides, which setup cannot read.
func (g *gitlabSetup) revokeExposed(ctx context.Context, key string, exposed []glVariable, fresh string) []string {
	var done, failed, hidden []string
	for _, v := range exposed {
		where := fmt.Sprintf("%s with environment scope %q", key, v.Scope)
		value := ""
		if v.Value != nil {
			value = strings.TrimSpace(*v.Value)
		}
		if value == "" {
			hidden = append(hidden, where)
			continue
		}
		if value == fresh {
			continue
		}
		g.register(value)
		leaked, err := newAPI(g.in.Client, g.in.APIURL, "Private-Token", "", value, nil, false)
		if err == nil {
			err = leaked.do(ctx, http.MethodDelete, leaked.path("personal_access_tokens", "self"), nil, nil)
		}
		switch {
		case err == nil:
			done = append(done, fmt.Sprintf("revoked the token %s held", where))
		case isStatus(err, http.StatusUnauthorized):
			done = append(done, fmt.Sprintf("the token %s held no longer works", where))
		default:
			failed = append(failed, fmt.Sprintf("%s: %v", where, g.in.Redact.Replace(err.Error())))
		}
	}
	switch {
	case len(failed) > 0:
		g.r.add("writer-exposed-token", StatusFail, "could not revoke the token that was stored in %s: revoke it by hand "+
			"(the account's access tokens in GitLab, or DELETE /personal_access_tokens/self with the token)", strings.Join(failed, "; "))
	case len(hidden) > 0:
		g.r.add("writer-exposed-token", StatusManual, "GitLab hides the value of %s, so setup cannot revoke the token it held: "+
			"unless it was setup's own token of %s, which setup rotated, revoke that token by hand", strings.Join(hidden, ", "), g.writer.login)
	}
	return done
}

// pushOK reports whether push levels let no one push: at least one, all
// "No one", none for a user, a group or a deploy key.
func pushOK(levels []glLevel) bool {
	if len(levels) == 0 {
		return false
	}
	for _, l := range levels {
		if l.AccessLevel != glNoOne || l.UserID != nil || l.GroupID != nil || l.DeployKeyID != nil {
			return false
		}
	}
	return true
}

// mergeOK reports whether merge levels keep merges to Maintainers (or
// named users and groups): none for Developers.
func mergeOK(levels []glLevel) bool {
	if len(levels) == 0 {
		return false
	}
	for _, l := range levels {
		if l.UserID == nil && l.GroupID == nil && l.AccessLevel > glNoOne && l.AccessLevel < glMaintainer {
			return false
		}
	}
	return true
}

// defaultBranch protects the default branch so that no one pushes to it:
// changes come through merge requests that Maintainers merge, and no force
// push. GitLab Free changes the access levels of a protected branch only by
// protecting it again: when an update leaves them, the protection is made
// again (the branch is unprotected for the moment between two requests).
// Docs: https://docs.gitlab.com/api/protected_branches/ (allowed_to_push
// and PATCH of access levels are Premium and Ultimate).
func (g *gitlabSetup) defaultBranch(ctx context.Context) {
	b, step := g.hub.DefaultBranch, "default-branch"
	var pb glProtectedBranch
	err := g.get(ctx, &pb, "projects", id(g.hub.ID), "protected_branches", b)
	if err != nil && !isStatus(err, http.StatusNotFound) {
		g.r.add(step, StatusFail, "cannot read the protection of %s: %v", b, err)
		return
	}
	protect := map[string]any{"name": b, "push_access_level": glNoOne, "merge_access_level": glMaintainer, "allow_force_push": false}
	if err != nil {
		if g.r.DryRun {
			g.r.add(step, StatusWould, "would protect %s: push No one, merge Maintainers, no force push", b)
			return
		}
		perr := g.send(ctx, http.MethodPost, protect, nil, "projects", id(g.hub.ID), "protected_branches")
		if perr == nil {
			g.r.add(step, StatusDone, "protected %s: push No one, merge Maintainers, no force push", b)
			return
		}
		// GitLab protects a new project's default branch from a background
		// job: the read above may miss a protection the POST then meets
		// (409). Read it again, and go on as for a branch found protected.
		if !isStatus(perr, http.StatusConflict) {
			g.r.add(step, StatusFail, "cannot protect %s: %v", b, perr)
			return
		}
		if err := g.get(ctx, &pb, "projects", id(g.hub.ID), "protected_branches", b); err != nil {
			g.r.add(step, StatusFail, "cannot protect %s: %v; and reading the protection GitLab reports: %v", b, perr, err)
			return
		}
	}
	if pushOK(pb.Push) && mergeOK(pb.Merge) && !pb.AllowForcePush {
		g.r.add(step, StatusOK, "%s is protected: push No one, merge %s, no force push", b, mergeLevels(pb.Merge))
		return
	}
	if g.r.DryRun {
		g.r.add(step, StatusWould, "would protect %s with push No one, merge Maintainers and no force push (now push %s, merge %s, force push %v)",
			b, mergeLevels(pb.Push), mergeLevels(pb.Merge), pb.AllowForcePush)
		return
	}
	// Premium updates the levels in place.
	var push, merge []map[string]any
	for _, l := range pb.Push {
		if l.AccessLevel != glNoOne || l.UserID != nil || l.GroupID != nil || l.DeployKeyID != nil {
			push = append(push, map[string]any{"id": l.ID, "_destroy": true})
		}
	}
	if !slices.ContainsFunc(pb.Push, func(l glLevel) bool {
		return l.AccessLevel == glNoOne && l.UserID == nil && l.GroupID == nil && l.DeployKeyID == nil
	}) {
		push = append(push, map[string]any{"access_level": glNoOne})
	}
	keep := 0
	for _, l := range pb.Merge {
		if l.UserID == nil && l.GroupID == nil && l.AccessLevel > glNoOne && l.AccessLevel < glMaintainer {
			merge = append(merge, map[string]any{"id": l.ID, "_destroy": true})
		} else {
			keep++
		}
	}
	if keep == 0 {
		merge = append(merge, map[string]any{"access_level": glMaintainer})
	}
	patch := map[string]any{"allow_force_push": false, "allowed_to_push": push}
	if len(merge) > 0 {
		patch["allowed_to_merge"] = merge
	}
	perr := g.send(ctx, http.MethodPatch, patch, nil, "projects", id(g.hub.ID), "protected_branches", b)
	var after glProtectedBranch
	if perr == nil {
		perr = g.get(ctx, &after, "projects", id(g.hub.ID), "protected_branches", b)
	}
	if perr == nil && pushOK(after.Push) && mergeOK(after.Merge) && !after.AllowForcePush {
		g.r.add(step, StatusDone, "%s: push No one, merge %s, no force push (updated)", b, mergeLevels(after.Merge))
		return
	}
	if err := g.send(ctx, http.MethodDelete, nil, nil, "projects", id(g.hub.ID), "protected_branches", b); err != nil {
		g.r.add(step, StatusFail, "cannot change the protection of %s (push %s, merge %s): %v", b, mergeLevels(pb.Push), mergeLevels(pb.Merge), err)
		return
	}
	err = g.send(ctx, http.MethodPost, protect, nil, "projects", id(g.hub.ID), "protected_branches")
	if err != nil {
		// Once more: the branch is unprotected now.
		err = g.send(ctx, http.MethodPost, protect, nil, "projects", id(g.hub.ID), "protected_branches")
	}
	if err != nil {
		g.r.add(step, StatusFail, "%s IS NOT PROTECTED NOW: unprotected it to change its access levels, and protecting it again failed: %v; "+
			"protect it in Settings > Repository > Protected branches (push No one, merge Maintainers)", b, err)
		return
	}
	g.r.add(step, StatusDone, "protected %s again with push No one, merge Maintainers, no force push (was push %s, merge %s): "+
		"this GitLab changes the access levels of a protected branch only so", b, mergeLevels(pb.Push), mergeLevels(pb.Merge))
}

// mergeLevels names the access levels of a protected branch, as GitLab's
// settings do: "No one", "Developers", "Maintainers".
func mergeLevels(levels []glLevel) string {
	var names []string
	for _, l := range levels {
		switch {
		case l.UserID != nil:
			names = append(names, "a user")
		case l.GroupID != nil:
			names = append(names, "a group")
		case l.DeployKeyID != nil:
			names = append(names, "a deploy key")
		case l.AccessLevel == glNoOne:
			names = append(names, "No one")
		default:
			names = append(names, levelName(l.AccessLevel)+"s")
		}
	}
	if len(names) == 0 {
		return "nobody listed"
	}
	return strings.Join(names, ", ")
}

// atLeast reports whether the instance's version is at least major.minor;
// unknown is false.
func (g *gitlabSetup) atLeast(major, minor int) bool {
	return g.major > major || (g.major == major && g.minor >= minor)
}

// projectSettings sets "Minimum role to use pipeline variables" to
// no_one_allowed and keeps protected variables out of merge request
// pipelines (protect_merge_request_pipelines false: true lets merge
// request pipelines between protected branches read them, GitLab 18.1;
// API attribute from 18.10). Docs:
// https://docs.gitlab.com/api/projects/#update-a-project and
// https://docs.gitlab.com/ci/pipelines/merge_request_pipelines/#control-access-to-protected-variables-and-runners
func (g *gitlabSetup) projectSettings(ctx context.Context) {
	h := g.hub
	put := map[string]any{}
	var did []string
	switch {
	case h.PipelineVariablesRole == nil:
		g.r.add("pipeline-variables", StatusManual, "this GitLab does not show the minimum role for pipeline variables to the token: "+
			"set Settings > CI/CD > Variables > Minimum role to use pipeline variables to No one allowed")
	case *h.PipelineVariablesRole == "no_one_allowed":
		g.r.add("pipeline-variables", StatusOK, "no one may run a pipeline with variables")
	default:
		put["ci_pipeline_variables_minimum_override_role"] = "no_one_allowed"
		if h.RestrictVariables != nil && !*h.RestrictVariables {
			// GitLab 17.1 to 17.7 apply the role only with this set.
			put["restrict_user_defined_variables"] = true
		}
		did = append(did, "pipeline-variables")
	}
	if g.guard() {
		did = append(did, g.guardSettings(put)...)
	}
	switch {
	case h.ProtectMRPipelines != nil && *h.ProtectMRPipelines:
		put["protect_merge_request_pipelines"] = false
		did = append(did, "mr-pipelines")
	case h.ProtectMRPipelines != nil:
		g.r.add("mr-pipelines", StatusOK, "merge request pipelines get no protected variables")
	case g.major > 0 && !g.atLeast(18, 1):
		g.r.add("mr-pipelines", StatusOK, "GitLab %d.%d gives merge request pipelines no protected variables", g.major, g.minor)
	default:
		g.r.add("mr-pipelines", StatusManual, "this GitLab does not show the setting to the API: keep Settings > CI/CD > Variables > "+
			"Allow merge request pipelines to access protected variables and runners off")
	}
	if len(put) == 0 {
		return
	}
	detail := map[string]string{
		"pipeline-variables": "the minimum role for pipeline variables is No one allowed",
		"mr-pipelines":       "merge request pipelines get no protected variables",
		"merge-checks":       "a merge waits for a pipeline that succeeded, and a skipped pipeline does not count",
		"ci-config":          "the pipelines read their CI file from the default branch: " + g.pinnedCIConfig(),
	}
	if g.r.DryRun {
		for _, d := range did {
			g.r.add(d, StatusWould, "would set: %s", detail[d])
		}
		return
	}
	var after glProject
	err := g.send(ctx, http.MethodPut, put, &after, "projects", id(h.ID))
	for _, d := range did {
		switch {
		case err != nil:
			g.r.add(d, StatusFail, "cannot update the hub's settings: %v", err)
		case d == "pipeline-variables" && (after.PipelineVariablesRole == nil || *after.PipelineVariablesRole != "no_one_allowed"):
			g.r.add(d, StatusFail, "GitLab kept the minimum role for pipeline variables: set it to No one allowed in Settings > CI/CD > Variables")
		case d == "mr-pipelines" && (after.ProtectMRPipelines == nil || *after.ProtectMRPipelines):
			g.r.add(d, StatusFail, "GitLab still lets merge request pipelines read protected variables: turn it off in Settings > CI/CD > Variables")
		case d == "merge-checks" && (after.MergeAfterPipeline == nil || !*after.MergeAfterPipeline || after.MergeOnSkipped == nil || *after.MergeOnSkipped):
			g.r.add(d, StatusFail, "GitLab kept the merge checks: in Settings > Merge requests turn on Pipelines must succeed and turn off "+
				"Skipped pipelines are considered successful")
		case d == "ci-config" && (after.CIConfigPath == nil || *after.CIConfigPath != g.pinnedCIConfig()):
			g.r.add(d, StatusFail, "GitLab kept the CI configuration path: set Settings > CI/CD > General pipelines > CI/CD configuration file "+
				"to %s", g.pinnedCIConfig())
		default:
			g.r.add(d, StatusDone, "%s", detail[d])
		}
	}
}

// guardSettings adds to put, under security.writer_on_hub guard, the
// hub's settings that make a merge wait for the plan of the default
// branch's CI configuration: Pipelines must succeed, no merge on a skipped
// pipeline, and the CI file read from the default branch
// (ci_config_path <file>@<hub>:<default branch>), so that a merge request
// pipeline runs the reviewed configuration, whose plan refuses a merge
// request the writer opened or pushed to. It reports the settings already
// so, and returns the steps whose settings it put.
func (g *gitlabSetup) guardSettings(put map[string]any) []string {
	h := g.hub
	var did []string
	switch {
	case h.MergeAfterPipeline == nil || h.MergeOnSkipped == nil:
		g.r.add("merge-checks", StatusManual, "this GitLab does not show the merge checks to the token: in Settings > Merge requests turn on "+
			"Pipelines must succeed and turn off Skipped pipelines are considered successful")
	case *h.MergeAfterPipeline && !*h.MergeOnSkipped:
		g.r.add("merge-checks", StatusOK, "a merge waits for a pipeline that succeeded, and a skipped pipeline does not count")
	default:
		put["only_allow_merge_if_pipeline_succeeds"] = true
		put["allow_merge_on_skipped_pipeline"] = false
		did = append(did, "merge-checks")
	}
	pinned := g.pinnedCIConfig()
	switch {
	case h.CIConfigPath == nil:
		g.r.add("ci-config", StatusManual, "this GitLab does not show the CI configuration path to the token: set Settings > CI/CD > "+
			"General pipelines > CI/CD configuration file to %s", pinned)
	case config.PinnedCIConfig(*h.CIConfigPath, h.Path, h.DefaultBranch):
		g.r.add("ci-config", StatusOK, "the pipelines read their CI file from the default branch: %s", *h.CIConfigPath)
	default:
		put["ci_config_path"] = pinned
		did = append(did, "ci-config")
	}
	return did
}

// pinnedCIConfig is the hub's CI configuration path that reads its CI file
// from the default branch: the file the hub reads now (.gitlab-ci.yml by
// default), @ the hub's path, : the default branch.
func (g *gitlabSetup) pinnedCIConfig() string {
	file := ".gitlab-ci.yml"
	if p := g.hub.CIConfigPath; p != nil && *p != "" && !strings.ContainsAny(*p, "@:") {
		file = *p
	} else if p != nil && *p != "" {
		if f, _, ok := strings.Cut(*p, "@"); ok && f != "" && !strings.Contains(f, ":") {
			file = f
		}
	}
	return file + "@" + g.hub.Path + ":" + g.hub.DefaultBranch
}

// schedules creates the daily distribute and weekly doctor schedules of
// the default branch, owned by the token's user. Docs:
// https://docs.gitlab.com/api/pipeline_schedules/
func (g *gitlabSetup) schedules(ctx context.Context) {
	type schedule struct {
		ID          int64  `json:"id"`
		Description string `json:"description"`
		Ref         string `json:"ref"`
		Active      bool   `json:"active"`
	}
	have, err := list[schedule](ctx, g.a, g.a.path("projects", id(g.hub.ID), "pipeline_schedules"), nil)
	if err != nil {
		g.r.add("schedules", StatusFail, "cannot list the hub's pipeline schedules: %v", err)
		return
	}
	for _, want := range glSchedules {
		step := "schedule"
		i := slices.IndexFunc(have, func(s schedule) bool { return strings.EqualFold(strings.TrimSpace(s.Description), want.description) })
		switch {
		case i >= 0 && !have[i].Active:
			g.r.add(step, StatusWarn, "%q exists and is inactive: activate it in Build > Pipeline schedules", want.description)
		case i >= 0:
			g.r.add(step, StatusOK, "%q runs %s", want.description, have[i].Ref)
		case g.r.DryRun:
			g.r.add(step, StatusWould, "would create %q (%s UTC) on %s", want.description, want.cron, g.hub.DefaultBranch)
		default:
			body := map[string]any{"description": want.description, "ref": g.hub.DefaultBranch, "cron": want.cron,
				"cron_timezone": "UTC", "active": true}
			if err := g.send(ctx, http.MethodPost, body, nil, "projects", id(g.hub.ID), "pipeline_schedules"); err != nil {
				g.r.add(step, StatusFail, "cannot create %q: %v", want.description, err)
				continue
			}
			g.r.add(step, StatusDone, "created %q (%s UTC) on %s, owned by %s: a schedule stops when its owner loses access, "+
				"so give it to an account that stays", want.description, want.cron, g.hub.DefaultBranch, g.me.Username)
		}
	}
}

// verify grades where the hub keeps its write key as doctor --hub-token
// does (protected variables, protected branches and tags, pipeline
// variables). A dry run changed nothing to grade.
func (g *gitlabSetup) verify(ctx context.Context) {
	if g.r.DryRun {
		return
	}
	ks, err := hubch.ReadKeyStore(ctx, hubch.KeyStoreInput{Platform: "gitlab", APIURL: g.a.base, RepoID: id(g.hub.ID), Token: g.in.Token, Client: g.a.client})
	if err != nil {
		g.r.add("check", StatusUnknown, "cannot read where the hub keeps its keys: %v", err)
		return
	}
	addChecks(g.r, distribute.KeyLocationChecks(ks, []string{g.in.WriteVar}, &config.Hub{Security: config.Security{WriteIsolation: g.in.Isolation,
		WriterOnHub: g.in.WriterOnHub}}))
}

// addChecks adds doctor's checks as steps "check <name>".
func addChecks(r *Report, checks []report.DoctorCheck) {
	for _, c := range checks {
		status := StatusUnknown
		switch c.Status {
		case report.StatusOK:
			status = StatusOK
		case report.StatusWarn:
			status = StatusWarn
		case report.StatusFail:
			status = StatusFail
		}
		r.add("check "+c.Name, status, "%s", c.Detail)
	}
}

// nextSteps tells the person what is left.
func (g *gitlabSetup) nextSteps() {
	if w := g.writer.login; w != "" && w != g.in.Writer {
		g.r.next("in hub.yml, set the writer to %s (the provider's writer, or the top-level writer with one provider), and merge it", w)
	}
	if g.in.Isolation == "external" {
		g.r.next("let your secrets store release the writer's token to the hub's id_tokens only when project_path is %s, ref is %s, ref_type is branch and ref_protected is true",
			g.hub.Path, g.hub.DefaultBranch)
	}
	g.r.next("in every target project, turn on the CI/CD job token allowlist (Settings > CI/CD > Job token permissions): sync pipelines run as the writer")
	g.r.next("open a merge request in the hub: check, plan and probe run on it; run touchmark doctor --hub-token to check the hub again at any time")
}
