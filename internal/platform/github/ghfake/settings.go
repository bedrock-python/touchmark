package ghfake

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The settings of a hub repository that `touchmark setup github` makes and
// `doctor --hub-token` reads: deployment environments with their branch
// policies, Actions variables and secrets (names only: a secret's value
// arrives encrypted with a libsodium sealed box, which the fake does not
// take; tests set secrets with SetSecret), repository rulesets made over
// the API, and the GitHub App manifest flow.

// repoSettings is what a repository holds besides git and pull requests.
type repoSettings struct {
	envs       map[string]*environment
	secrets    map[string]bool // Actions secrets of the repository
	depSecrets map[string]bool // Dependabot secrets
	variables  map[string]string
}

// environment is a deployment environment.
type environment struct {
	name string
	// policy is nil for "every branch"; custom selects branchPolicies.
	policy *struct{ protected, custom bool }
	// waitTimer, reviewers and preventSelfReview are its protection rules.
	waitTimer         *int
	reviewers         []map[string]any
	preventSelfReview *bool
	branchPolicies    []*branchPolicy
	secrets           map[string]bool
	variables         map[string]string
	created           time.Time
}

// branchPolicy is a custom deployment branch or tag policy.
type branchPolicy struct {
	id   int64
	name string
	typ  string
}

// settingsOf returns r's settings, made on first use. Called with mu held.
func (r *repo) settingsOf() *repoSettings {
	if r.settings == nil {
		r.settings = &repoSettings{envs: map[string]*environment{}, secrets: map[string]bool{}, depSecrets: map[string]bool{},
			variables: map[string]string{}}
	}
	return r.settings
}

// paidFeatures reports whether r gets what private repositories of free
// owners lack: environment secrets, deployment branch policies and
// rulesets (docs: "Environments, environment secrets, and deployment
// protection rules are available in public repositories for all current
// GitHub plans"; private ones need Pro, Team or Enterprise).
func paidFeatures(r *repo) bool { return !r.private() || r.owner.plan != PlanFree }

// needAdmin checks that id may administer r for a permission name
// ("administration"): an installation token with name write, a classic
// token with the repo scope of an admin of r, a fine-grained token with
// name write of an admin of r (docs: the Administration permission; the
// Admin role). Called with mu held.
func (s *Server) needAdmin(id *identity, r *repo, name string) *response {
	if !s.canSee(id, r) {
		resp := notFound()
		return &resp
	}
	if id != nil && id.kind == idToken {
		t := id.tok
		switch t.kind {
		case tokenInstallation:
			if t.inst.covers(r) && (t.repos == nil || t.repos[r.id]) && levelRank(t.perms[name]) >= levelRank(Write) {
				return nil
			}
		case tokenClassicPAT:
			if repoLevel(t.user, r) == "admin" && hasScope(t.scopes, "repo") {
				return nil
			}
		default:
			if repoLevel(t.user, r) == "admin" && (t.repos == nil || t.repos[r.id]) && levelRank(t.perms[name]) >= levelRank(Write) {
				return nil
			}
		}
	}
	resp := apiError(http.StatusForbidden, forbiddenMessage(id))
	resp.header = http.Header{"X-Accepted-Github-Permissions": {name + "=write"}}
	return &resp
}

// settingsRoutes are the REST routes of this file.
func settingsRoutes() (base, perRepo []route) {
	base = []route{
		newRoute("POST", "/app-manifests/{code}/conversions", convertManifest),
		newRoute("POST", "/organizations/{org}/settings/apps/new", newAppFromManifest),
		newRoute("POST", "/settings/apps/new", newAppFromManifest),
	}
	perRepo = []route{
		newRoute("GET", "/repos/{owner}/{repo}/environments", listEnvironments),
		newRoute("GET", "/repos/{owner}/{repo}/environments/{env}", getEnvironment),
		newRoute("PUT", "/repos/{owner}/{repo}/environments/{env}", putEnvironment),
		newRoute("GET", "/repos/{owner}/{repo}/environments/{env}/deployment-branch-policies", listBranchPolicies),
		newRoute("POST", "/repos/{owner}/{repo}/environments/{env}/deployment-branch-policies", createBranchPolicy),
		newRoute("DELETE", "/repos/{owner}/{repo}/environments/{env}/deployment-branch-policies/{id}", deleteBranchPolicy),
		newRoute("GET", "/repos/{owner}/{repo}/environments/{env}/secrets", listEnvSecrets),
		newRoute("GET", "/repos/{owner}/{repo}/environments/{env}/variables/{name}", getEnvVariable),
		newRoute("POST", "/repos/{owner}/{repo}/environments/{env}/variables", createEnvVariable),
		newRoute("PATCH", "/repos/{owner}/{repo}/environments/{env}/variables/{name}", updateEnvVariable),
		newRoute("GET", "/repos/{owner}/{repo}/actions/secrets", listRepoSecrets),
		newRoute("GET", "/repos/{owner}/{repo}/actions/organization-secrets", listOrgSecretsOfRepo),
		newRoute("GET", "/repos/{owner}/{repo}/dependabot/secrets", listDependabotSecrets),
		newRoute("GET", "/repos/{owner}/{repo}/actions/variables/{name}", getRepoVariable),
		newRoute("POST", "/repos/{owner}/{repo}/actions/variables", createRepoVariable),
		newRoute("PATCH", "/repos/{owner}/{repo}/actions/variables/{name}", updateRepoVariable),
		newRoute("POST", "/repos/{owner}/{repo}/rulesets", createRuleset),
	}
	return base, perRepo
}

// envName is a valid environment name.
var envName = regexp.MustCompile(`^[^,;/\\]{1,255}$`)

// environmentOf resolves the repository and the environment {env} of a
// request; 404 when either is missing. Called with mu held.
func (c *call) environmentOf() (*repo, *environment, *response) {
	r, resp := c.repo()
	if resp != nil {
		return nil, nil, resp
	}
	e := r.settingsOf().envs[strings.ToLower(c.param("env"))]
	if e == nil {
		nf := notFound()
		return r, nil, &nf
	}
	return r, e, nil
}

// envJSON is an environment as GET /environments/{env} shows it.
func (c *call) envJSON(r *repo, e *environment) map[string]any {
	rules := []any{}
	if e.waitTimer != nil {
		rules = append(rules, map[string]any{"id": 1, "node_id": "GA_wait", "type": "wait_timer", "wait_timer": *e.waitTimer})
	}
	if len(e.reviewers) > 0 {
		var rv []any
		for _, x := range e.reviewers {
			rv = append(rv, map[string]any{"type": x["type"], "reviewer": map[string]any{"id": x["id"]}})
		}
		rule := map[string]any{"id": 2, "node_id": "GA_rev", "type": "required_reviewers", "reviewers": rv}
		if e.preventSelfReview != nil {
			rule["prevent_self_review"] = *e.preventSelfReview
		}
		rules = append(rules, rule)
	}
	out := map[string]any{"id": 1, "node_id": "EN_" + e.name, "name": e.name, "url": c.base + "/repos/" + escapePath(r.path()) + "/environments/" + url.PathEscape(e.name),
		"html_url":   c.s.http.URL + "/" + r.path() + "/deployments/activity_log?environments_filter=" + url.QueryEscape(e.name),
		"created_at": fmtTime(e.created), "updated_at": fmtTime(e.created), "protection_rules": rules, "deployment_branch_policy": nil}
	if e.policy != nil {
		out["deployment_branch_policy"] = map[string]any{"protected_branches": e.policy.protected, "custom_branch_policies": e.policy.custom}
		rules = append(rules, map[string]any{"id": 3, "node_id": "GA_pol", "type": "branch_policy"})
		out["protection_rules"] = rules
	}
	return out
}

// listEnvironments answers GET /repos/{owner}/{repo}/environments: the
// Actions read permission, or anyone for a public repository (observed
// read-only 2026-09-29).
func listEnvironments(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "actions", Read); refused != nil {
		return *refused
	}
	var names []string
	for n := range r.settingsOf().envs {
		names = append(names, n)
	}
	sort.Strings(names)
	list := []any{}
	for _, n := range names {
		list = append(list, c.envJSON(r, r.settingsOf().envs[n]))
	}
	return ok(map[string]any{"total_count": len(list), "environments": list})
}

// getEnvironment answers GET /repos/{owner}/{repo}/environments/{env}.
func getEnvironment(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "actions", Read); refused != nil {
		return *refused
	}
	r, e, resp := c.environmentOf()
	if resp != nil {
		return *resp
	}
	return ok(c.envJSON(r, e))
}

// putEnvironment answers PUT /repos/{owner}/{repo}/environments/{env}: it
// creates or replaces the environment's protection rules and deployment
// branch policy (fields left out are cleared: assumed, the docs do not
// say). A custom policy in a private repository of a free owner is
// refused (422; message assumed).
func putEnvironment(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needAdmin(c.id, r, "administration"); refused != nil {
		return *refused
	}
	name := c.param("env")
	if !envName.MatchString(name) {
		return unprocessable("Invalid environment name")
	}
	var in struct {
		WaitTimer         *int             `json:"wait_timer"`
		PreventSelfReview *bool            `json:"prevent_self_review"`
		Reviewers         []map[string]any `json:"reviewers"`
		Policy            *struct {
			Protected *bool `json:"protected_branches"`
			Custom    *bool `json:"custom_branch_policies"`
		} `json:"deployment_branch_policy"`
	}
	if bad, okd := c.decode(&in); !okd {
		return bad
	}
	if in.Policy != nil {
		switch {
		case in.Policy.Protected == nil || in.Policy.Custom == nil:
			return validation(customError("Environment", "deployment_branch_policy", "protected_branches and custom_branch_policies are required"))
		case *in.Policy.Protected && *in.Policy.Custom:
			return unprocessable("Only one of protected_branches and custom_branch_policies can be true")
		case !paidFeatures(r):
			return unprocessable("Deployment branch policies are not available for private repositories on this plan.")
		}
	}
	set := r.settingsOf()
	e := set.envs[strings.ToLower(name)]
	if e == nil {
		e = &environment{name: name, secrets: map[string]bool{}, variables: map[string]string{}, created: s.now()}
		set.envs[strings.ToLower(name)] = e
	}
	e.waitTimer, e.reviewers, e.preventSelfReview = in.WaitTimer, in.Reviewers, in.PreventSelfReview
	if in.Policy == nil {
		e.policy, e.branchPolicies = nil, nil
	} else {
		e.policy = &struct{ protected, custom bool }{*in.Policy.Protected, *in.Policy.Custom}
		if !e.policy.custom {
			e.branchPolicies = nil
		}
	}
	return ok(c.envJSON(r, e))
}

// listBranchPolicies answers GET …/deployment-branch-policies.
func listBranchPolicies(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "actions", Read); refused != nil {
		return *refused
	}
	_, e, resp := c.environmentOf()
	if resp != nil {
		return *resp
	}
	list := []any{}
	for _, p := range e.branchPolicies {
		list = append(list, map[string]any{"id": p.id, "node_id": nodeID("DeploymentBranchPolicy", p.id), "name": p.name, "type": p.typ})
	}
	return ok(map[string]any{"total_count": len(list), "branch_policies": list})
}

// createBranchPolicy answers POST …/deployment-branch-policies: the
// environment must select custom policies (404 otherwise, docs: "the
// environment must have custom_branch_policies set to true"), and a
// duplicate name is refused (303 in the docs; 422 here, assumed).
func createBranchPolicy(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needAdmin(c.id, r, "administration"); refused != nil {
		return *refused
	}
	_, e, resp := c.environmentOf()
	if resp != nil {
		return *resp
	}
	if e.policy == nil || !e.policy.custom {
		return notFound()
	}
	var in struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if bad, okd := c.decode(&in); !okd {
		return bad
	}
	if in.Type == "" {
		in.Type = "branch"
	}
	if in.Name == "" || (in.Type != "branch" && in.Type != "tag") {
		return validation(customError("DeploymentBranchPolicy", "name", "invalid"))
	}
	for _, p := range e.branchPolicies {
		if p.name == in.Name && p.typ == in.Type {
			return unprocessable("Name has already been taken")
		}
	}
	p := &branchPolicy{id: s.id(), name: in.Name, typ: in.Type}
	e.branchPolicies = append(e.branchPolicies, p)
	return ok(map[string]any{"id": p.id, "node_id": nodeID("DeploymentBranchPolicy", p.id), "name": p.name, "type": p.typ})
}

// deleteBranchPolicy answers DELETE …/deployment-branch-policies/{id}.
func deleteBranchPolicy(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needAdmin(c.id, r, "administration"); refused != nil {
		return *refused
	}
	_, e, resp := c.environmentOf()
	if resp != nil {
		return *resp
	}
	id, _ := strconv.ParseInt(c.param("id"), 10, 64)
	for i, p := range e.branchPolicies {
		if p.id == id {
			e.branchPolicies = slices.Delete(e.branchPolicies, i, i+1)
			return noContent()
		}
	}
	return notFound()
}

// secretsList is a secrets listing of names (values are never returned).
func (s *Server) secretsList(names map[string]bool) response {
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	list := []any{}
	for _, n := range sorted {
		list = append(list, map[string]any{"name": n, "created_at": fmtTime(s.now()), "updated_at": fmtTime(s.now())})
	}
	return ok(map[string]any{"total_count": len(list), "secrets": list})
}

// listEnvSecrets answers GET …/environments/{env}/secrets: the
// Environments read permission.
func listEnvSecrets(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needPrivate(c.id, r, "environments", Read); refused != nil {
		return *refused
	}
	_, e, resp := c.environmentOf()
	if resp != nil {
		return *resp
	}
	return s.secretsList(e.secrets)
}

// needPrivate checks name at level on r for what even a public
// repository shows only to its collaborators (secrets, variables).
// Called with mu held.
func (s *Server) needPrivate(id *identity, r *repo, name, level string) *response {
	if refused := s.need(id, r, name, level); refused != nil {
		return refused
	}
	if _, inScope := s.perm(id, r, name); !inScope {
		resp := apiError(http.StatusForbidden, forbiddenMessage(id))
		return &resp
	}
	return nil
}

// listRepoSecrets answers GET /repos/{owner}/{repo}/actions/secrets.
func listRepoSecrets(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needPrivate(c.id, r, "secrets", Read); refused != nil {
		return *refused
	}
	return s.secretsList(r.settingsOf().secrets)
}

// listDependabotSecrets answers GET /repos/{owner}/{repo}/dependabot/secrets.
func listDependabotSecrets(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needPrivate(c.id, r, "dependabot_secrets", Read); refused != nil {
		return *refused
	}
	return s.secretsList(r.settingsOf().depSecrets)
}

// listOrgSecretsOfRepo answers GET
// /repos/{owner}/{repo}/actions/organization-secrets: the organization's
// secrets shared with the repository.
func listOrgSecretsOfRepo(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needPrivate(c.id, r, "secrets", Read); refused != nil {
		return *refused
	}
	names := map[string]bool{}
	for name, repos := range s.orgSecrets[r.owner.id] {
		if repos == nil || slices.Contains(repos, r.id) {
			names[name] = true
		}
	}
	return s.secretsList(names)
}

// variableJSON is an Actions variable.
func (s *Server) variableJSON(name, value string) map[string]any {
	return map[string]any{"name": name, "value": value, "created_at": fmtTime(s.now()), "updated_at": fmtTime(s.now())}
}

// variablesOf returns the variables of the repository or of its
// environment {env}, and the permission that guards them. Called with mu
// held.
func (c *call) variablesOf() (*repo, map[string]string, string, *response) {
	r, resp := c.repo()
	if resp != nil {
		return nil, nil, "", resp
	}
	if c.param("env") == "" {
		return r, r.settingsOf().variables, "variables", nil
	}
	_, e, resp := c.environmentOf()
	if resp != nil {
		return r, nil, "", resp
	}
	return r, e.variables, "environments", nil
}

// getVariable answers GET …/variables/{name}.
func getVariable(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, vars, perm, resp := c.variablesOf()
	if r != nil {
		if refused := s.needPrivate(c.id, r, cmpOr(perm, "variables"), Read); refused != nil {
			return *refused
		}
	}
	if resp != nil {
		return *resp
	}
	for n, v := range vars {
		if strings.EqualFold(n, c.param("name")) {
			return ok(s.variableJSON(n, v))
		}
	}
	return apiError(http.StatusNotFound, "Not Found")
}

// cmpOr returns a unless it is empty, else b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// variableName is a valid variable name (docs: alphanumerics and
// underscores, not starting with a number or GITHUB_).
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// createVariable answers POST …/variables: 201, or 409 when it exists
// (docs: "Variable already exists").
func createVariable(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, vars, perm, resp := c.variablesOf()
	if r != nil {
		if refused := s.needPrivate(c.id, r, cmpOr(perm, "variables"), Write); refused != nil {
			return *refused
		}
	}
	if resp != nil {
		return *resp
	}
	var in struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if bad, okd := c.decode(&in); !okd {
		return bad
	}
	if !variableName.MatchString(in.Name) || strings.HasPrefix(strings.ToUpper(in.Name), "GITHUB_") {
		return validation(customError("Variable", "name", "invalid"))
	}
	for n := range vars {
		if strings.EqualFold(n, in.Name) {
			return apiError(http.StatusConflict, "Already exists - Variable already exists")
		}
	}
	vars[strings.ToUpper(in.Name)] = in.Value
	return created(map[string]any{})
}

// updateVariable answers PATCH …/variables/{name}: 204.
func updateVariable(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, vars, perm, resp := c.variablesOf()
	if r != nil {
		if refused := s.needPrivate(c.id, r, cmpOr(perm, "variables"), Write); refused != nil {
			return *refused
		}
	}
	if resp != nil {
		return *resp
	}
	var in struct {
		Value *string `json:"value"`
	}
	if bad, okd := c.decode(&in); !okd {
		return bad
	}
	for n := range vars {
		if strings.EqualFold(n, c.param("name")) {
			if in.Value != nil {
				vars[n] = *in.Value
			}
			return noContent()
		}
	}
	return notFound()
}

func getRepoVariable(c *call) response    { return getVariable(c) }
func createRepoVariable(c *call) response { return createVariable(c) }
func updateRepoVariable(c *call) response { return updateVariable(c) }
func getEnvVariable(c *call) response     { return getVariable(c) }
func createEnvVariable(c *call) response  { return createVariable(c) }
func updateEnvVariable(c *call) response  { return updateVariable(c) }

// createRuleset answers POST /repos/{owner}/{repo}/rulesets: the
// Administration write permission; a private repository of a free owner
// is refused (403; message from public reports, assumed). Rules the fake
// enforces are enforced at once.
func createRuleset(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needAdmin(c.id, r, "administration"); refused != nil {
		return *refused
	}
	if !paidFeatures(r) {
		return apiError(http.StatusForbidden, "Upgrade to GitHub Pro or make this repository public to enable this feature.")
	}
	var in struct {
		Name        string `json:"name"`
		Target      string `json:"target"`
		Enforcement string `json:"enforcement"`
		Conditions  struct {
			RefName struct {
				Include []string `json:"include"`
				Exclude []string `json:"exclude"`
			} `json:"ref_name"`
		} `json:"conditions"`
		Rules []struct {
			Type       string         `json:"type"`
			Parameters map[string]any `json:"parameters"`
		} `json:"rules"`
	}
	if bad, okd := c.decode(&in); !okd {
		return bad
	}
	if in.Name == "" || in.Enforcement == "" {
		return validation(customError("Ruleset", "name", "missing"))
	}
	if in.Target != "" && in.Target != "branch" {
		return unprocessable("Only branch rulesets are modeled")
	}
	for _, x := range r.rulesets {
		if x.spec.Name == in.Name {
			return validation(customError("Ruleset", "name", "Name must be unique"))
		}
	}
	spec := Ruleset{Name: in.Name, Enforcement: in.Enforcement, Include: in.Conditions.RefName.Include, Exclude: in.Conditions.RefName.Exclude}
	for _, rule := range in.Rules {
		spec.Rules = append(spec.Rules, Rule{Type: rule.Type, Parameters: rule.Parameters})
	}
	x, err := s.newRuleset(spec)
	if err != nil {
		return unprocessable(err.Error())
	}
	x.repo = r
	r.rulesets = append(r.rulesets, x)
	return created(c.rulesetJSON(r, x, true))
}

// SetSecret sets (or, with del, deletes) the Actions secret name of a
// repository, or of its environment env, as `gh secret set` does: the
// fake keeps names only.
func (s *Server) SetSecret(path, env, name string, del bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("SetSecret", path)
	if err != nil {
		return err
	}
	names := r.settingsOf().secrets
	if env != "" {
		e := r.settingsOf().envs[strings.ToLower(env)]
		if e == nil {
			return setupErr("SetSecret: %s has no environment %q", path, env)
		}
		if !paidFeatures(r) {
			return setupErr("SetSecret: environment secrets need a paid plan for private repositories")
		}
		names = e.secrets
	}
	if del {
		delete(names, strings.ToUpper(name))
	} else {
		names[strings.ToUpper(name)] = true
	}
	return nil
}

// SetOrgSecret shares an organization secret with the repositories repos
// ("owner/name" paths; nil for all of the organization's).
func (s *Server) SetOrgSecret(org, name string, repos []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(org)]
	if a == nil || a.typ != TypeOrganization {
		return setupErr("SetOrgSecret: unknown organization %q", org)
	}
	var ids []int64
	for _, p := range repos {
		r := s.byPath[strings.ToLower(p)]
		if r == nil {
			return setupErr("SetOrgSecret: unknown repository %q", p)
		}
		ids = append(ids, r.id)
	}
	if repos != nil && ids == nil {
		ids = []int64{}
	}
	if s.orgSecrets == nil {
		s.orgSecrets = map[int64]map[string][]int64{}
	}
	if s.orgSecrets[a.id] == nil {
		s.orgSecrets[a.id] = map[string][]int64{}
	}
	s.orgSecrets[a.id][strings.ToUpper(name)] = ids
	return nil
}

// EnvironmentSpec is a deployment environment as a test arranges or reads
// it.
type EnvironmentSpec struct {
	// AllBranches leaves it without a deployment branch policy; Protected
	// selects protected branches; otherwise Policies are its custom
	// policies ("branch:main", "tag:v*").
	AllBranches bool
	Protected   bool
	Policies    []string
	Secrets     []string
	Variables   map[string]string
	WaitTimer   *int
}

// SetEnvironment creates or replaces an environment of a repository.
func (s *Server) SetEnvironment(path, name string, spec EnvironmentSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("SetEnvironment", path)
	if err != nil {
		return err
	}
	e := &environment{name: name, secrets: map[string]bool{}, variables: map[string]string{}, created: s.now(), waitTimer: spec.WaitTimer}
	switch {
	case spec.AllBranches:
	case spec.Protected:
		e.policy = &struct{ protected, custom bool }{true, false}
	default:
		e.policy = &struct{ protected, custom bool }{false, true}
		for _, p := range spec.Policies {
			typ, n, ok := strings.Cut(p, ":")
			if !ok {
				typ, n = "branch", p
			}
			e.branchPolicies = append(e.branchPolicies, &branchPolicy{id: s.id(), name: n, typ: typ})
		}
	}
	for _, sec := range spec.Secrets {
		e.secrets[strings.ToUpper(sec)] = true
	}
	for k, v := range spec.Variables {
		e.variables[strings.ToUpper(k)] = v
	}
	r.settingsOf().envs[strings.ToLower(name)] = e
	return nil
}

// Environment returns an environment of a repository as EnvironmentSpec;
// false when it does not exist.
func (s *Server) Environment(path, name string) (EnvironmentSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return EnvironmentSpec{}, false
	}
	e := r.settingsOf().envs[strings.ToLower(name)]
	if e == nil {
		return EnvironmentSpec{}, false
	}
	out := EnvironmentSpec{AllBranches: e.policy == nil, Variables: map[string]string{}, WaitTimer: e.waitTimer}
	if e.policy != nil {
		out.Protected = e.policy.protected
	}
	for _, p := range e.branchPolicies {
		out.Policies = append(out.Policies, p.typ+":"+p.name)
	}
	for n := range e.secrets {
		out.Secrets = append(out.Secrets, n)
	}
	sort.Strings(out.Secrets)
	for k, v := range e.variables {
		out.Variables[k] = v
	}
	return out, true
}

// RepoSecrets returns the names of a repository's Actions secrets and
// its variables.
func (s *Server) RepoSecrets(path string) (secrets []string, variables map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return nil, nil
	}
	for n := range r.settingsOf().secrets {
		secrets = append(secrets, n)
	}
	sort.Strings(secrets)
	variables = map[string]string{}
	for k, v := range r.settingsOf().variables {
		variables[k] = v
	}
	return secrets, variables
}

// Rulesets returns the names of a repository's own rulesets.
func (s *Server) Rulesets(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byPath[strings.ToLower(path)]
	if r == nil {
		return nil
	}
	var out []string
	for _, x := range r.rulesets {
		out = append(out, x.spec.Name)
	}
	return out
}

// pendingManifest is an App registration GitHub's "new App" page made from
// a manifest, waiting for its code to be converted.
type pendingManifest struct {
	owner    *account
	manifest map[string]any
	expires  time.Time
}

// manifestCodeTTL is how long a manifest code lives (docs: "within one
// hour").
const manifestCodeTTL = time.Hour

// newAppFromManifest answers the browser's POST of a manifest form to
// /organizations/{org}/settings/apps/new or /settings/apps/new: GitHub
// shows a page where the person confirms the App; the fake skips it and
// redirects to the manifest's redirect_url with a code and the state, as
// GitHub does once the person confirms (docs). A personal account's page
// needs the person's token (the fake's stand-in for a signed-in browser);
// an organization's takes anyone, as the fake models no membership.
func newAppFromManifest(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	form, err := url.ParseQuery(string(c.body))
	if err != nil {
		return apiError(http.StatusBadRequest, "bad form")
	}
	var owner *account
	if org := c.param("org"); org != "" {
		owner = s.byLogin[strings.ToLower(org)]
		if owner == nil || owner.typ != TypeOrganization {
			return notFound()
		}
	} else {
		if c.id == nil || c.id.kind != idToken || c.id.tok.kind == tokenInstallation {
			return apiError(http.StatusUnauthorized, "sign in")
		}
		owner = c.id.who
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(form.Get("manifest")), &m); err != nil {
		return unprocessable("The manifest is not valid JSON")
	}
	if u, _ := m["url"].(string); u == "" {
		return unprocessable("url is required")
	}
	if h, ok := m["hook_attributes"].(map[string]any); ok {
		if u, _ := h["url"].(string); u == "" {
			return unprocessable("hook_attributes.url is required")
		}
	}
	redirect, _ := m["redirect_url"].(string)
	ru, err := url.Parse(redirect)
	if err != nil || ru.Scheme == "" || ru.Host == "" {
		return unprocessable("redirect_url is not a URL")
	}
	code := randomAlnum(20)
	if s.manifests == nil {
		s.manifests = map[string]*pendingManifest{}
	}
	s.manifests[code] = &pendingManifest{owner: owner, manifest: m, expires: s.now().Add(manifestCodeTTL)}
	q := ru.Query()
	q.Set("code", code)
	if st := c.query.Get("state"); st != "" {
		q.Set("state", st)
	}
	ru.RawQuery = q.Encode()
	return response{status: http.StatusFound, header: http.Header{"Location": {ru.String()}}}
}

// ManifestCode registers a manifest for owner as GitHub's page does when
// the person confirms it, and returns the code its redirect carries.
func (s *Server) ManifestCode(owner, manifest string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(owner)]
	if a == nil || a.typ == TypeBot {
		return "", setupErr("ManifestCode: unknown owner %q", owner)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		return "", setupErr("ManifestCode: %v", err)
	}
	code := randomAlnum(20)
	if s.manifests == nil {
		s.manifests = map[string]*pendingManifest{}
	}
	s.manifests[code] = &pendingManifest{owner: a, manifest: m, expires: s.now().Add(manifestCodeTTL)}
	return code, nil
}

// slugOf makes an App's slug from its name: lowercase, runs of other
// characters as one '-'.
func slugOf(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// convertManifest answers POST /app-manifests/{code}/conversions: once per
// code, within an hour; 201 with the App and its id, slug, client id and
// secret, webhook secret and private key (docs: create a GitHub App from a
// manifest). The App gets the manifest's default_permissions (metadata
// read always).
func convertManifest(c *call) response {
	s := c.s
	s.mu.Lock()
	pm := s.manifests[c.param("code")]
	if pm != nil {
		delete(s.manifests, c.param("code"))
	}
	now := s.now()
	s.mu.Unlock()
	if pm == nil || now.After(pm.expires) {
		return notFound()
	}
	key, pemBytes, err := GenerateAppKey()
	if err != nil {
		return apiError(http.StatusInternalServerError, err.Error())
	}
	name, _ := pm.manifest["name"].(string)
	if name == "" {
		name = "app-" + strings.ToLower(randomAlnum(6))
	}
	perms := Permissions{}
	if dp, ok := pm.manifest["default_permissions"].(map[string]any); ok {
		for k, v := range dp {
			if lv, ok := v.(string); ok {
				perms[k] = lv
			}
		}
	}
	a, err := s.RegisterApp(AppSpec{Slug: slugOf(name), Name: name, Owner: pm.owner.login, PublicKey: &key.PublicKey, Permissions: perms})
	if err != nil {
		return unprocessable("Name is already in use")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ap := s.apps[a.ID]
	permsOut := map[string]string{}
	for k, v := range ap.perms {
		permsOut[k] = v
	}
	return response{status: http.StatusCreated, body: map[string]any{
		"id": a.ID, "slug": a.Slug, "node_id": nodeID("Integration", a.ID), "client_id": a.ClientID, "name": a.Name,
		"owner": c.accountJSON(pm.owner), "description": pm.manifest["description"], "external_url": pm.manifest["url"],
		"html_url": s.http.URL + "/apps/" + a.Slug, "created_at": fmtTime(now), "updated_at": fmtTime(now),
		"permissions": permsOut, "events": []string{}, "installations_count": 0,
		"client_secret": randomAlnum(40), "webhook_secret": randomAlnum(32), "pem": string(pemBytes),
	}}
}

// AppBySlug returns a registered App and the permissions it asks for.
func (s *Server) AppBySlug(slug string) (App, Permissions, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.appBySlug(slug)
	if a == nil {
		return App{}, nil, false
	}
	return a.snapshot(), a.perms.clone(), true
}

// SetVariable sets an Actions variable of a repository, or of its
// environment env.
func (s *Server) SetVariable(path, env, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("SetVariable", path)
	if err != nil {
		return err
	}
	vars := r.settingsOf().variables
	if env != "" {
		e := r.settingsOf().envs[strings.ToLower(env)]
		if e == nil {
			return setupErr("SetVariable: %s has no environment %q", path, env)
		}
		vars = e.variables
	}
	vars[strings.ToUpper(name)] = value
	return nil
}
