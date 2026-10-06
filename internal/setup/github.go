package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// The permissions of the Apps: the reader reads, the writer writes
// contents, pull requests and workflows. The writer needs workflows even
// for packs without workflow files: rebuilding a sync branch on a newer
// base moves it across others' workflow changes, which GitHub refuses
// without that permission. A target's token asks for it only when a push
// needs it.
var (
	readerPermissions = map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read"}
	writerPermissions = map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write", "workflows": "write"}
)

// RulesetName names the ruleset setup creates for the hub's default
// branch.
const RulesetName = "touchmark hub"

// GitHub's API headers (docs: API versions).
var githubHeader = http.Header{"Accept": {"application/vnd.github+json"}, "X-Github-Api-Version": {"2022-11-28"}}

// DefaultListen is where the manifest flow listens by default: a random
// port of the IPv4 loopback.
const DefaultListen = "127.0.0.1:0"

// GitHubInput is what GitHub setup needs.
type GitHubInput struct {
	// WebURL is the platform's web URL (https://github.com, a GHES
	// instance), APIURL its REST base, Host its host for the report.
	WebURL, APIURL, Host string
	// Repo is the hub repository, "owner/name".
	Repo string
	// Token is the maintainer's: an admin of the hub repository.
	Token  string
	Client *httpx.Client
	// Isolation is security.write_isolation: platform or external.
	Isolation string
	// ReaderName and WriterName are the Apps' names ("" for
	// "<owner>-assets-read" and "<owner>-assets-write").
	ReaderName, WriterName string
	// The names of the Actions variables and secrets: the reader's id and
	// key are the repository's, the writer's the environment's.
	ReadIDVar, ReadKeyVar, WriteIDVar, WriteKeyVar string
	// Writer is hub.yml's writer ("" when unset).
	Writer string
	DryRun bool
	// KeyDir receives the Apps' private keys; "" makes a new private
	// temporary directory.
	KeyDir string
	// Listen is the loopback address of the manifest flow (DefaultListen
	// when ""); Timeout bounds the flow (30 minutes when 0: GitHub's code
	// lives an hour).
	Listen  string
	Timeout time.Duration
	// Tell shows the person the URL to open in the browser.
	Tell   func(url string)
	Engine string
	Now    func() time.Time
	// Redact gets every secret of the run: the token, the Apps' keys and
	// client secrets.
	Redact *redact.Registry
}

// ghRepo is the hub repository.
type ghRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Visibility    string `json:"visibility"`
	Private       bool   `json:"private"`
	Owner         struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	Permissions *struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

// ghEnvironment is an environment of the hub.
type ghEnvironment struct {
	Name            string `json:"name"`
	ProtectionRules []struct {
		Type              string `json:"type"`
		WaitTimer         *int   `json:"wait_timer"`
		PreventSelfReview *bool  `json:"prevent_self_review"`
		Reviewers         []struct {
			Type     string `json:"type"`
			Reviewer struct {
				ID int64 `json:"id"`
			} `json:"reviewer"`
		} `json:"reviewers"`
	} `json:"protection_rules"`
	Policy *struct {
		Protected bool `json:"protected_branches"`
		Custom    bool `json:"custom_branch_policies"`
	} `json:"deployment_branch_policy"`
}

// ghApp is the reader or the writer.
type ghApp struct {
	label    string // reader or writer
	name     string
	perms    map[string]string
	idVar    string
	keyVar   string
	env      string // the environment of its variable and secret, "" for the repository's
	appID    string // from the variable, or the new App's
	slug     string // known for an App made in this run
	keyFile  string // the private key's file, for an App made in this run
	created  bool
	failed   bool
	varFound bool
}

// githubSetup is one run of GitHub setup.
type githubSetup struct {
	in     GitHubInput
	a      *api
	r      *Report
	repo   ghRepo
	owner  string
	name   string
	reader *ghApp
	writer *ghApp
	keyDir string
}

// GitHub sets up the hub's GitHub for touchmark (see the package
// documentation). It returns the report; a PreconditionError when the
// token or the hub is not what setup needs, before anything was written;
// another error when a request failed in a way that ends the run.
func GitHub(ctx context.Context, in GitHubInput) (*Report, error) {
	r := newReport("github", in.Engine, in.DryRun)
	r.Host, r.Hub, r.Isolation = in.Host, in.Repo, in.Isolation
	switch in.Isolation {
	case "platform", "external":
	default:
		return r, precondition("setup sets up an isolated write key only: security.write_isolation must be platform or external, not %q", in.Isolation)
	}
	owner, name, ok := strings.Cut(in.Repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return r, precondition("the hub repository %q is not owner/name", in.Repo)
	}
	if in.Listen == "" {
		in.Listen = DefaultListen
	}
	if in.Timeout <= 0 {
		in.Timeout = 30 * time.Minute
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.Tell == nil {
		in.Tell = func(string) {}
	}
	a, err := newAPI(in.Client, in.APIURL, "", "Bearer ", in.Token, githubHeader, in.DryRun)
	if err != nil {
		return r, precondition("%v", err)
	}
	g := &githubSetup{in: in, a: a, r: r, owner: owner, name: name}
	if err := g.identify(ctx); err != nil {
		return r, err
	}
	g.r.Accounts = "app"
	envOK, envReady := g.environment(ctx)
	if envOK {
		g.branchPolicy(ctx, envReady)
	}
	g.reader = &ghApp{label: "reader", name: cmpOr(in.ReaderName, defaultAppName(g.owner, "read")), perms: readerPermissions,
		idVar: in.ReadIDVar, keyVar: in.ReadKeyVar}
	g.writer = &ghApp{label: "writer", name: cmpOr(in.WriterName, defaultAppName(g.owner, "write")), perms: writerPermissions,
		idVar: in.WriteIDVar, keyVar: in.WriteKeyVar, env: Environment}
	g.findApps(ctx, envOK)
	g.createApps(ctx, envOK)
	g.variables(ctx, envOK)
	g.secrets(ctx)
	g.ruleset(ctx)
	g.verify(ctx)
	g.nextSteps()
	return r, nil
}

// defaultAppName is "<owner>-assets-<role>", the owner cut so the name
// fits GitHub's 34 characters.
func defaultAppName(owner, role string) string {
	suffix := "-assets-" + role
	owner = strings.ToLower(owner)
	if most := 34 - len("-assets-write"); len(owner) > most {
		owner = strings.TrimRight(owner[:most], "-")
	}
	return owner + suffix
}

// repoPath returns the API URL of the hub repository and segs below it.
func (g *githubSetup) repoPath(segs ...string) string {
	return g.a.path(append([]string{"repos", g.owner, g.name}, segs...)...)
}

// identify reads the hub and checks that the token administers it.
func (g *githubSetup) identify(ctx context.Context) error {
	if err := g.a.get(ctx, g.repoPath(), &g.repo); err != nil {
		switch {
		case isStatus(err, http.StatusUnauthorized):
			return precondition("GitHub refuses the token: %v", err)
		case isStatus(err, http.StatusNotFound, http.StatusForbidden):
			return precondition("the hub repository %s is not visible to the token", g.in.Repo)
		}
		return err
	}
	if o, n, ok := strings.Cut(g.repo.FullName, "/"); ok {
		g.owner, g.name = o, n
	}
	g.r.Hub, g.r.HubID = g.repo.FullName, strconv.FormatInt(g.repo.ID, 10)
	g.r.Org = g.repo.Owner.Login
	if g.repo.Permissions == nil || !g.repo.Permissions.Admin {
		return precondition("the token does not administer %s: setup creates its environment, variables and ruleset, "+
			"which needs the Admin role on the repository (a classic token with repo, or a fine-grained one with "+
			"Administration, Environments and Variables write, and Secrets read)", g.repo.FullName)
	}
	if g.repo.DefaultBranch == "" {
		return precondition("the hub %s has no default branch yet: push the hub's files first", g.repo.FullName)
	}
	vis := g.repo.Visibility
	if vis == "" {
		vis = map[bool]string{true: "private", false: "public"}[g.repo.Private]
	}
	g.r.add("hub", StatusOK, "%s, %s, default branch %s; the token administers it", g.repo.FullName, vis, g.repo.DefaultBranch)
	return nil
}

// freePlanHint explains a refusal that GitHub Free gives private hubs.
func (g *githubSetup) freePlanHint() string {
	if !g.repo.Private {
		return ""
	}
	return "; a private hub on GitHub Free cannot limit secrets to one branch (environments with deployment branch policies need " +
		"GitHub Pro, Team or Enterprise for private repositories): make the hub public, change the plan, or use security.write_isolation: external"
}

// environment makes the environment touchmark-distribute usable by
// selected branches only (custom deployment branch policies), keeping its
// protection rules. ok reports whether the environment exists afterwards
// (or would, in a dry run), ready whether it already had custom policies
// that can be listed. Docs:
// https://docs.github.com/en/rest/deployments/environments#create-or-update-an-environment
func (g *githubSetup) environment(ctx context.Context) (ok, ready bool) {
	var env ghEnvironment
	err := g.a.get(ctx, g.repoPath("environments", Environment), &env)
	exists := err == nil
	if err != nil && !isStatus(err, http.StatusNotFound) {
		g.r.add("environment", StatusFail, "cannot read the environment %s: %v", Environment, err)
		return false, false
	}
	if exists && env.Policy != nil && env.Policy.Custom && !env.Policy.Protected {
		g.r.add("environment", StatusOK, "the environment %s takes selected branches only", Environment)
		return true, true
	}
	body := map[string]any{"deployment_branch_policy": map[string]bool{"protected_branches": false, "custom_branch_policies": true}}
	if exists {
		// A PUT replaces the protection rules: keep the ones it has.
		for _, rule := range env.ProtectionRules {
			switch rule.Type {
			case "wait_timer":
				if rule.WaitTimer != nil {
					body["wait_timer"] = *rule.WaitTimer
				}
			case "required_reviewers":
				var reviewers []map[string]any
				for _, rv := range rule.Reviewers {
					reviewers = append(reviewers, map[string]any{"type": rv.Type, "id": rv.Reviewer.ID})
				}
				body["reviewers"] = reviewers
				if rule.PreventSelfReview != nil {
					body["prevent_self_review"] = *rule.PreventSelfReview
				}
			}
		}
	}
	what := "create the environment " + Environment + " with selected deployment branches"
	if exists {
		what = "limit the environment " + Environment + " to selected deployment branches (it allowed " + envPolicyText(env) + ")"
	}
	if g.r.DryRun {
		g.r.add("environment", StatusWould, "would %s", what)
		return true, false
	}
	if err := g.a.do(ctx, http.MethodPut, g.repoPath("environments", Environment), body, nil); err != nil {
		g.r.add("environment", StatusFail, "cannot %s: %v%s", what, err, g.freePlanHint())
		return false, false
	}
	verb := map[bool]string{true: "limited", false: "created"}[exists]
	g.r.add("environment", StatusDone, "%s the environment %s: selected deployment branches", verb, Environment)
	return true, true
}

// envPolicyText describes an environment's deployment branch policy.
func envPolicyText(env ghEnvironment) string {
	switch {
	case env.Policy == nil:
		return "every branch"
	case env.Policy.Protected:
		return "protected branches, every branch when none is protected"
	}
	return "selected branches"
}

// branchPolicy leaves the environment one deployment branch policy: the
// default branch. Other policies let other refs read the write key and are
// deleted. The policies apply only with custom_branch_policies true (docs:
// https://docs.github.com/en/rest/deployments/branch-policies).
func (g *githubSetup) branchPolicy(ctx context.Context, ready bool) {
	type policy struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	var policies []policy
	if ready {
		for page := 1; page <= listPages; page++ {
			var resp struct {
				Total    int      `json:"total_count"`
				Policies []policy `json:"branch_policies"`
			}
			u := g.repoPath("environments", Environment, "deployment-branch-policies") + fmt.Sprintf("?per_page=%d&page=%d", listPageSize, page)
			if err := g.a.get(ctx, u, &resp); err != nil {
				g.r.add("deployment-branches", StatusFail, "cannot list the deployment branch policies of %s: %v", Environment, err)
				return
			}
			policies = append(policies, resp.Policies...)
			if len(resp.Policies) < listPageSize || len(policies) >= resp.Total {
				break
			}
		}
	}
	def := g.repo.DefaultBranch
	found := false
	var extra []policy
	for _, p := range policies {
		if p.Name == def && (p.Type == "" || p.Type == "branch") && !found {
			found = true
			continue
		}
		extra = append(extra, p)
	}
	if found && len(extra) == 0 {
		g.r.add("deployment-branches", StatusOK, "only %s may use %s", def, Environment)
		return
	}
	var plan, did []string
	if !found {
		plan, did = append(plan, "add the branch "+def), append(did, "added the branch "+def)
	}
	for _, p := range extra {
		kind := cmpOr(p.Type, "branch")
		plan = append(plan, fmt.Sprintf("delete the %s policy %q", kind, p.Name))
		did = append(did, fmt.Sprintf("deleted the %s policy %q", kind, p.Name))
	}
	if g.r.DryRun {
		g.r.add("deployment-branches", StatusWould, "would %s: only %s may use %s", strings.Join(plan, ", "), def, Environment)
		return
	}
	if !found {
		if err := g.a.do(ctx, http.MethodPost, g.repoPath("environments", Environment, "deployment-branch-policies"),
			map[string]string{"name": def, "type": "branch"}, nil); err != nil {
			g.r.add("deployment-branches", StatusFail, "cannot let %s use %s: %v%s", def, Environment, err, g.freePlanHint())
			return
		}
	}
	for _, p := range extra {
		if err := g.a.do(ctx, http.MethodDelete, g.repoPath("environments", Environment, "deployment-branch-policies", strconv.FormatInt(p.ID, 10)), nil, nil); err != nil {
			g.r.add("deployment-branches", StatusFail, "cannot delete the policy %q of %s: %v; it lets other refs read the write key", p.Name, Environment, err)
			return
		}
	}
	g.r.add("deployment-branches", StatusDone, "%s: only %s may use %s", strings.Join(did, ", "), def, Environment)
}

// variableURL is the API URL of an Actions variable of the App's place:
// the repository, or the environment.
func (g *githubSetup) variableURL(app *ghApp, name bool) string {
	segs := []string{"actions", "variables"}
	if app.env != "" {
		segs = []string{"environments", app.env, "variables"}
	}
	if name {
		segs = append(segs, app.idVar)
	}
	return g.repoPath(segs...)
}

// findApps reads the Apps' ids from their variables: an App whose variable
// holds an id exists.
func (g *githubSetup) findApps(ctx context.Context, envOK bool) {
	for _, app := range []*ghApp{g.reader, g.writer} {
		if app.env != "" && !envOK {
			continue
		}
		var v struct {
			Value string `json:"value"`
		}
		err := g.a.get(ctx, g.variableURL(app, true), &v)
		switch {
		case err == nil && strings.TrimSpace(v.Value) != "":
			app.appID, app.varFound = strings.TrimSpace(v.Value), true
		case err != nil && !isStatus(err, http.StatusNotFound):
			app.failed = true
			g.r.add(app.label+"-app", StatusFail, "cannot read the variable %s: %v", app.idVar, err)
		}
	}
}

// newAppURL is GitHub's page that registers an App from a manifest: the
// organization's, or the user's own.
func (g *githubSetup) newAppURL() string {
	web := strings.TrimRight(g.in.WebURL, "/")
	if strings.EqualFold(g.repo.Owner.Type, "Organization") {
		return web + "/organizations/" + url.PathEscape(g.repo.Owner.Login) + "/settings/apps/new"
	}
	return web + "/settings/apps/new"
}

// createApps creates the Apps that have no id yet through the manifest
// flow, and writes their private keys to the key directory.
func (g *githubSetup) createApps(ctx context.Context, envOK bool) {
	var need []*ghApp
	for _, app := range []*ghApp{g.reader, g.writer} {
		switch {
		case app.failed:
		case app.env != "" && !envOK:
			app.failed = true
			g.r.add(app.label+"-app", StatusFail, "skipped: the environment %s is not ready", Environment)
		case app.varFound:
			g.r.add(app.label+"-app", StatusOK, "App id %s (the variable %s)", app.appID, app.idVar)
		case g.r.DryRun:
			g.r.add(app.label+"-app", StatusWould, "would create the App %s (%s) through GitHub's manifest flow, with a page on %s",
				app.name, permText(app.perms), g.in.Listen)
		default:
			need = append(need, app)
		}
	}
	if len(need) == 0 {
		return
	}
	if err := g.makeKeyDir(); err != nil {
		for _, app := range need {
			app.failed = true
			g.r.add(app.label+"-app", StatusFail, "%v", err)
		}
		return
	}
	var reqs []*flowRequest
	for _, app := range need {
		state, err := newState()
		if err != nil {
			app.failed = true
			g.r.add(app.label+"-app", StatusFail, "%v", err)
			return
		}
		reqs = append(reqs, &flowRequest{label: app.label, state: state, newApp: g.newAppURL(), manifest: appManifest{
			Name: app.name, URL: g.repo.HTMLURL, Description: fmt.Sprintf("touchmark %s of the hub %s", app.label, g.repo.FullName),
			HookAttributes: hookAttributes{URL: g.repo.HTMLURL, Active: false}, Public: false,
			Permissions: maps.Clone(app.perms), Events: []string{},
		}})
	}
	web, _ := url.Parse(g.in.WebURL)
	origin := ""
	if web != nil {
		origin = web.Scheme + "://" + web.Host
	}
	fctx, cancel := context.WithTimeout(ctx, g.in.Timeout)
	defer cancel()
	convs, err := runFlow(fctx, g.in.Listen, origin, reqs, g.in.Tell, g.exchange)
	for i, app := range need {
		if i >= len(convs) {
			app.failed = true
			g.r.add(app.label+"-app", StatusFail, "the App %s was not created: %v", app.name, err)
			continue
		}
		c := convs[i]
		app.appID, app.slug, app.created = strconv.FormatInt(c.ID, 10), c.Slug, true
		file, werr := g.writeKey(app.label, c.PEM)
		if werr != nil {
			app.failed = true
			g.r.add(app.label+"-app", StatusFail, "created the App %s (id %d) and could not write its private key: %v; "+
				"generate a new private key in its settings (%s)", c.Slug, c.ID, werr, c.HTMLURL)
			continue
		}
		app.keyFile = file
		g.r.add(app.label+"-app", StatusDone, "created the App %s (id %d, %s); its private key is in %s", c.Slug, c.ID, permText(c.Permissions), file)
	}
}

// exchange trades the flow's code for the App, registers its secrets and
// checks that GitHub gave it the manifest's permissions.
func (g *githubSetup) exchange(ctx context.Context, req *flowRequest, code string) (conversion, error) {
	anon, err := newAPI(g.a.client, g.in.APIURL, "", "", "", githubHeader, false)
	if err != nil {
		return conversion{}, err
	}
	var c conversion
	err = anon.do(ctx, http.MethodPost, anon.path("app-manifests", code, "conversions"), nil, &c)
	// What GitHub returns is secret, also when the answer is not what setup
	// expects.
	g.in.Redact.Add(c.PEM)
	g.in.Redact.Add(c.ClientSecret)
	if c.WebhookSecret != nil {
		g.in.Redact.Add(*c.WebhookSecret)
	}
	if err != nil {
		return conversion{}, fmt.Errorf("exchange GitHub's code for the %s App: %w", req.label, err)
	}
	if c.ID == 0 || c.Slug == "" || !strings.Contains(c.PEM, "PRIVATE KEY") {
		return conversion{}, fmt.Errorf("GitHub's answer for the %s App has no id, slug or private key", req.label)
	}
	for k, want := range req.manifest.Permissions {
		if c.Permissions[k] != want {
			return c, fmt.Errorf("GitHub gave the App %s %s %q, want %q: delete it in its settings (%s) and run setup again", c.Slug, k, c.Permissions[k], want, c.HTMLURL)
		}
	}
	for k, got := range c.Permissions {
		if _, ok := req.manifest.Permissions[k]; !ok {
			return c, fmt.Errorf("GitHub gave the App %s the permission %s %q the manifest did not ask for: delete it in its settings (%s) and run setup again", c.Slug, k, got, c.HTMLURL)
		}
	}
	return c, nil
}

// permText lists permissions as "contents write, metadata read".
func permText(perms map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(perms) {
		parts = append(parts, k+" "+perms[k])
	}
	return strings.Join(parts, ", ")
}

// makeKeyDir makes the directory of the private keys: the given one, or a
// new temporary directory only its owner can read.
func (g *githubSetup) makeKeyDir() error {
	if g.keyDir != "" {
		return nil
	}
	if g.in.KeyDir != "" {
		if err := os.MkdirAll(g.in.KeyDir, 0o700); err != nil {
			return fmt.Errorf("the key directory: %w", err)
		}
		abs, err := filepath.Abs(g.in.KeyDir)
		if err != nil {
			return err
		}
		g.keyDir = abs
		return nil
	}
	dir, err := os.MkdirTemp("", "touchmark-setup-")
	if err != nil {
		return fmt.Errorf("the key directory: %w", err)
	}
	g.keyDir = dir
	return nil
}

// writeKey writes an App's private key to <keydir>/<label>.pem, readable
// by its owner only, never over an existing file.
func (g *githubSetup) writeKey(label, pem string) (string, error) {
	p := filepath.Join(g.keyDir, label+".pem")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%s exists: setup does not overwrite a key file", p)
		}
		return "", err
	}
	_, werr := f.WriteString(pem)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(p)
		return "", err
	}
	return p, nil
}

// variables stores the Apps' ids in their Actions variables: the
// reader's in the repository, the writer's in the environment. Docs:
// https://docs.github.com/en/rest/actions/variables
func (g *githubSetup) variables(ctx context.Context, envOK bool) {
	for _, app := range []*ghApp{g.reader, g.writer} {
		step := app.label + "-variable"
		switch {
		case app.failed:
			continue
		case app.varFound:
			continue // the -app step said where the id came from
		case g.r.DryRun:
			g.r.add(step, StatusWould, "would set the variable %s%s to the App's id", app.idVar, where(app))
			continue
		}
		err := g.a.do(ctx, http.MethodPost, g.variableURL(app, false), map[string]string{"name": app.idVar, "value": app.appID}, nil)
		if isStatus(err, http.StatusConflict, http.StatusUnprocessableEntity) {
			err = g.a.do(ctx, http.MethodPatch, g.variableURL(app, true), map[string]string{"name": app.idVar, "value": app.appID}, nil)
		}
		if err != nil {
			g.r.add(step, StatusFail, "cannot set the variable %s%s to %s: %v", app.idVar, where(app), app.appID, err)
			continue
		}
		g.r.add(step, StatusDone, "set the variable %s%s to %s", app.idVar, where(app), app.appID)
	}
}

// where names the place of an App's variable and secret.
func where(app *ghApp) string {
	if app.env != "" {
		return " of the environment " + app.env
	}
	return " of the repository"
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./:=@+", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ghRepoFlag is the hub as gh's --repo takes it: [HOST/]OWNER/REPO, the
// host for an instance other than github.com.
func (g *githubSetup) ghRepoFlag() string {
	if host := strings.ToLower(g.in.Host); host != "" && host != "github.com" {
		return host + "/" + g.repo.FullName
	}
	return g.repo.FullName
}

// secretNames lists the names of a GitHub secrets listing.
func (g *githubSetup) secretNames(ctx context.Context, u string) ([]string, error) {
	var names []string
	for page := 1; page <= listPages; page++ {
		var resp struct {
			Total   int `json:"total_count"`
			Secrets []struct {
				Name string `json:"name"`
			} `json:"secrets"`
		}
		if err := g.a.get(ctx, fmt.Sprintf("%s?per_page=%d&page=%d", u, listPageSize, page), &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Secrets {
			names = append(names, s.Name)
		}
		if len(resp.Secrets) < listPageSize || len(names) >= resp.Total {
			break
		}
	}
	return names, nil
}

// secrets checks the Apps' private keys in their secrets, and prints the
// gh commands that store the keys of the Apps made in this run: setup
// cannot encrypt a secret itself (GitHub takes libsodium sealed boxes:
// https://docs.github.com/en/rest/actions/secrets). The listings it reads
// give names only.
func (g *githubSetup) secrets(ctx context.Context) {
	for _, app := range []*ghApp{g.reader, g.writer} {
		step := app.label + "-key"
		if app.failed {
			continue
		}
		external := app.env != "" && g.in.Isolation == "external"
		switch {
		case g.r.DryRun && !app.varFound:
			if external {
				g.r.add(step, StatusWould, "would leave the new App's private key in a file for your secrets store")
			} else {
				g.r.add(step, StatusWould, "would print the command that stores the new App's private key in the secret %s%s", app.keyVar, where(app))
			}
			continue
		case app.created && external:
			g.r.add(step, StatusManual, "put the writer's private key %s into your secrets store, released to the OIDC subject repo:%s:environment:%s only, then delete the file",
				app.keyFile, g.repo.FullName, Environment)
			continue
		case app.created:
			s := g.r.add(step, StatusManual, "store the private key in the secret %s%s, then delete the key file; run setup again to check", app.keyVar, where(app))
			s.Commands = []string{g.secretCommand(app, app.keyFile)}
			continue
		}
		u := g.repoPath("actions", "secrets")
		if app.env != "" {
			u = g.repoPath("environments", app.env, "secrets")
		}
		names, err := g.secretNames(ctx, u)
		switch {
		case err != nil:
			g.r.add(step, StatusUnknown, "cannot list the secrets%s: %v", where(app), err)
		case external && slices.Contains(names, app.keyVar):
			g.r.add(step, StatusFail, "the environment %s holds %s although security.write_isolation is external: delete it (gh secret delete %s --repo %s --env %s)",
				app.env, app.keyVar, app.keyVar, shellQuote(g.ghRepoFlag()), app.env)
		case external:
			g.r.add(step, StatusOK, "the writer's private key lives in your secrets store (security.write_isolation: external)")
		case slices.Contains(names, app.keyVar):
			g.r.add(step, StatusOK, "the secret %s%s exists", app.keyVar, where(app))
		default:
			s := g.r.add(step, StatusManual, "the secret %s%s is missing: generate a private key in the App's settings (%s) and store it", app.keyVar, where(app), g.appsSettingsURL())
			s.Commands = []string{g.secretCommand(app, "<private-key.pem>")}
		}
	}
	if g.keyDir != "" && !g.r.DryRun {
		g.r.next("delete %s once the keys are stored: rm -r %s (PowerShell: Remove-Item -Recurse %s)", g.keyDir, shellQuote(g.keyDir), shellQuote(g.keyDir))
		g.r.next("PowerShell has no '<': run each gh secret set as Get-Content -Raw FILE | gh secret set ... instead")
	}
}

// secretCommand is the gh command that stores file in the App's secret:
// gh reads the value from standard input without --body, and --repo takes
// [HOST/]OWNER/REPO (https://cli.github.com/manual/gh_secret_set).
func (g *githubSetup) secretCommand(app *ghApp, file string) string {
	cmd := "gh secret set " + app.keyVar + " --repo " + shellQuote(g.ghRepoFlag())
	if app.env != "" {
		cmd += " --env " + app.env
	}
	if strings.HasPrefix(file, "<") {
		return cmd + " < " + file
	}
	return cmd + " < " + shellQuote(file)
}

// appsSettingsURL is the page listing the owner's Apps.
func (g *githubSetup) appsSettingsURL() string {
	web := strings.TrimRight(g.in.WebURL, "/")
	if strings.EqualFold(g.repo.Owner.Type, "Organization") {
		return web + "/organizations/" + url.PathEscape(g.repo.Owner.Login) + "/settings/apps"
	}
	return web + "/settings/apps"
}

// ruleset protects the default branch with a ruleset when the hub has none
// named RulesetName: pull requests with an approving review and a code
// owner's, no force push, no deletion. Where the token or the plan cannot
// (GitHub Free for private repositories), the person is told how. Docs:
// https://docs.github.com/en/rest/repos/rules#create-a-repository-ruleset
func (g *githubSetup) ruleset(ctx context.Context) {
	var sets []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Enforcement string `json:"enforcement"`
	}
	manual := "add a ruleset for the default branch in Settings > Rules > Rulesets: require a pull request with one approval and a review from Code Owners, block force pushes and deletions"
	err := g.a.get(ctx, g.repoPath("rulesets")+"?includes_parents=false&per_page=100", &sets)
	if err != nil {
		g.r.add("ruleset", StatusManual, "cannot list the hub's rulesets (%v): %s", err, manual)
		return
	}
	for _, s := range sets {
		if s.Name == RulesetName {
			if s.Enforcement != "active" {
				g.r.add("ruleset", StatusWarn, "the ruleset %q is %s: make it active", RulesetName, s.Enforcement)
				return
			}
			g.r.add("ruleset", StatusOK, "the ruleset %q protects the default branch", RulesetName)
			return
		}
	}
	if g.r.DryRun {
		g.r.add("ruleset", StatusWould, "would add the ruleset %q: pull requests with an approval and a code owner's review, no force push, no deletion of the default branch", RulesetName)
		return
	}
	body := map[string]any{
		"name": RulesetName, "target": "branch", "enforcement": "active",
		"conditions": map[string]any{"ref_name": map[string]any{"include": []string{"~DEFAULT_BRANCH"}, "exclude": []string{}}},
		"rules": []map[string]any{
			{"type": "deletion"},
			{"type": "non_fast_forward"},
			{"type": "pull_request", "parameters": map[string]any{
				"required_approving_review_count": 1, "require_code_owner_review": true, "dismiss_stale_reviews_on_push": true,
				"require_last_push_approval": false, "required_review_thread_resolution": false,
			}},
		},
	}
	if err := g.a.do(ctx, http.MethodPost, g.repoPath("rulesets"), body, nil); err != nil {
		g.r.add("ruleset", StatusManual, "GitHub refused the ruleset (%v): %s", err, manual)
		return
	}
	g.r.add("ruleset", StatusDone, "added the ruleset %q: pull requests with an approval and a code owner's review, no force push, no deletion of the default branch", RulesetName)
}

// verify grades where the hub keeps its write key as doctor --hub-token
// does.
func (g *githubSetup) verify(ctx context.Context) {
	if g.r.DryRun {
		return
	}
	ks, err := hubch.ReadKeyStore(ctx, hubch.KeyStoreInput{Platform: "github", APIURL: g.a.base, RepoID: strconv.FormatInt(g.repo.ID, 10),
		Token: g.in.Token, Client: g.a.client})
	if err != nil {
		g.r.add("check", StatusUnknown, "cannot read where the hub keeps its keys: %v", err)
		return
	}
	addChecks(g.r, distribute.KeyLocationChecks(ks, []string{g.in.WriteKeyVar}, &config.Hub{Security: config.Security{WriteIsolation: g.in.Isolation}}))
}

// nextSteps tells the person what is left.
func (g *githubSetup) nextSteps() {
	for _, app := range []*ghApp{g.reader, g.writer} {
		if app.slug != "" {
			g.r.next("install the %s App on the target repositories only, never on the hub: %s/apps/%s/installations/new",
				app.label, strings.TrimRight(g.in.WebURL, "/"), app.slug)
		}
	}
	if g.writer.slug != "" {
		g.r.Writer = g.writer.slug + "[bot]"
		if g.r.Writer != g.in.Writer {
			g.r.next("in hub.yml, set the writer to %s and merge it", g.r.Writer)
		}
	}
	if g.reader.slug != "" {
		g.r.Reader = g.reader.slug + "[bot]"
	}
	if g.in.Isolation == "external" {
		g.r.next("let your secrets store release the writer's key to the OIDC subject repo:%s:environment:%s (or repo:%s:ref:refs/heads/%s) only",
			g.repo.FullName, Environment, g.repo.FullName, g.repo.DefaultBranch)
	}
	g.r.next("replace the team in .github/CODEOWNERS, open a pull request in the hub, and run the workflow by hand once to see doctor's report")
}
