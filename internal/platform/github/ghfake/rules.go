package ghfake

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Rule types the fake enforces on ref updates (pushes and API ref
// updates); other types (pull_request aside) are listed but only matter
// when merging, which touchmark never does.
const (
	RuleCreation           = "creation"
	RuleUpdate             = "update"
	RuleDeletion           = "deletion"
	RuleNonFastForward     = "non_fast_forward"
	RuleRequiredSignatures = "required_signatures"
	RuleLinearHistory      = "required_linear_history"
	RulePullRequest        = "pull_request"
	RuleWorkflows          = "workflows"
)

// Rule is one rule of a ruleset.
type Rule struct {
	Type       string
	Parameters map[string]any
}

// Ruleset is a branch ruleset.
type Ruleset struct {
	Name string
	// Enforcement is "active" (the default), "evaluate" or "disabled";
	// only active rulesets are enforced and listed by rules/branches.
	Enforcement string
	// Include and Exclude are ref name patterns: "~DEFAULT_BRANCH",
	// "~ALL", or "refs/heads/<glob>" where "*" matches within a path
	// segment and "**" across segments. Include is ["~ALL"] when empty.
	Include, Exclude []string
	Rules            []Rule
	// BypassApps are the slugs of Apps that may always bypass it.
	BypassApps []string
}

// ruleset is a ruleset of a repository or an organization.
type ruleset struct {
	id      int64
	spec    Ruleset
	repo    *repo    // a repository ruleset
	org     *account // an organization ruleset
	bypass  []*app
	created time.Time
}

// AddRuleset adds a branch ruleset to a repository and returns its id.
func (s *Server) AddRuleset(path string, rs Ruleset) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repoForSetup("AddRuleset", path)
	if err != nil {
		return 0, err
	}
	x, err := s.newRuleset(rs)
	if err != nil {
		return 0, err
	}
	x.repo = r
	r.rulesets = append(r.rulesets, x)
	return x.id, nil
}

// AddOrgRuleset adds a branch ruleset that applies to every repository of
// an organization.
func (s *Server) AddOrgRuleset(org string, rs Ruleset) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(org)]
	if a == nil || a.typ != TypeOrganization {
		return 0, setupErr("AddOrgRuleset: unknown organization %q", org)
	}
	x, err := s.newRuleset(rs)
	if err != nil {
		return 0, err
	}
	x.org = a
	s.orgRulesets[a.id] = append(s.orgRulesets[a.id], x)
	return x.id, nil
}

// newRuleset validates a ruleset. Called with mu held.
func (s *Server) newRuleset(rs Ruleset) (*ruleset, error) {
	if rs.Enforcement == "" {
		rs.Enforcement = "active"
	}
	switch rs.Enforcement {
	case "active", "evaluate", "disabled":
	default:
		return nil, setupErr("invalid enforcement %q", rs.Enforcement)
	}
	if len(rs.Include) == 0 {
		rs.Include = []string{"~ALL"}
	}
	x := &ruleset{id: s.id(), spec: rs, created: s.now()}
	for _, slug := range rs.BypassApps {
		a := s.appBySlug(slug)
		if a == nil {
			return nil, setupErr("unknown App %q", slug)
		}
		x.bypass = append(x.bypass, a)
	}
	return x, nil
}

// rulesetsOf returns the rulesets of r, its own first. Called with mu held.
func (s *Server) rulesetsOf(r *repo) []*ruleset {
	out := append([]*ruleset(nil), r.rulesets...)
	if r.owner.typ == TypeOrganization {
		out = append(out, s.orgRulesets[r.owner.id]...)
	}
	return out
}

// enforced reports whether rulesets bind r: private repositories of owners
// on the free plan get none (assumed from GitHub's plans page:
// rulesets in private repositories need a paid plan).
func enforced(r *repo) bool {
	return !r.private() || r.owner.plan != PlanFree
}

// matches reports whether the ruleset covers branch of r.
func (x *ruleset) matches(r *repo, branch string) bool {
	ref := "refs/heads/" + branch
	hit := func(pattern string) bool {
		switch pattern {
		case "~ALL":
			return true
		case "~DEFAULT_BRANCH":
			return branch == r.defaultBranch
		}
		return globMatch(pattern, ref)
	}
	in := false
	for _, p := range x.spec.Include {
		in = in || hit(p)
	}
	for _, p := range x.spec.Exclude {
		if hit(p) {
			return false
		}
	}
	return in
}

// globMatch matches a ref name against a pattern where "*" and "?" stay
// within a path segment and "**" crosses segments (fnmatch with
// FNM_PATHNAME, as GitHub documents for ruleset patterns).
func globMatch(pattern, name string) bool {
	if pattern == "" {
		return name == ""
	}
	switch {
	case strings.HasPrefix(pattern, "**"):
		rest := strings.TrimPrefix(strings.TrimPrefix(pattern, "**"), "/")
		for i := 0; i <= len(name); i++ {
			if globMatch(rest, name[i:]) {
				return true
			}
		}
		return false
	case pattern[0] == '*':
		for i := 0; i <= len(name); i++ {
			if globMatch(pattern[1:], name[i:]) {
				return true
			}
			if i < len(name) && name[i] == '/' {
				return false
			}
		}
		return false
	case name == "":
		return false
	case pattern[0] == '?':
		return name[0] != '/' && globMatch(pattern[1:], name[1:])
	}
	return pattern[0] == name[0] && globMatch(pattern[1:], name[1:])
}

// ruleHit is an active rule of a ruleset that covers a branch.
type ruleHit struct {
	rule Rule
	set  *ruleset
}

// activeRules returns the active rules covering branch of r. Called with
// mu held.
func (s *Server) activeRules(r *repo, branch string) []ruleHit {
	if !enforced(r) {
		return nil
	}
	var out []ruleHit
	for _, x := range s.rulesetsOf(r) {
		if x.spec.Enforcement != "active" || !x.matches(r, branch) {
			continue
		}
		for _, rule := range x.spec.Rules {
			out = append(out, ruleHit{rule: rule, set: x})
		}
	}
	return out
}

// bypasses reports whether id may always bypass x.
func (x *ruleset) bypasses(id *identity) bool {
	if !id.installation() {
		return false
	}
	for _, a := range x.bypass {
		if a == id.tok.inst.app {
			return true
		}
	}
	return false
}

// Rule messages: the "- …" lines of GH013 and the text of API refusals
// (public reports of GH013 pushes; "Cannot force-push to this branch" and
// "Commits must have verified signatures." are widely quoted, the others
// are assumed).
var ruleMessages = map[string]string{
	RuleCreation:           "Cannot create ref due to creations being restricted.",
	RuleUpdate:             "Cannot update this protected ref.",
	RuleDeletion:           "Cannot delete this protected branch.",
	RuleNonFastForward:     "Cannot force-push to this branch",
	RuleRequiredSignatures: "Commits must have verified signatures.",
	RuleLinearHistory:      "This branch must not contain merge commits.",
	RulePullRequest:        "Changes must be made through a pull request.",
}

// refusal is why a ref update is refused.
type refusal struct {
	ref string
	// workflow is the first workflow file changed without the Workflows
	// permission ("" when that is not the reason).
	workflow string
	// rules are the violated rules' messages (GH013); unsigned the commits
	// without a verified signature.
	rules    []string
	unsigned []string
}

// checkUpdate judges one ref update of r by id before it happens: the
// Workflows permission for any ref, and the rulesets for branches. rg sees
// the new objects. Called with mu held.
func (s *Server) checkUpdate(ctx context.Context, rg *rgit, r *repo, id *identity, c refChange) (*refusal, error) {
	ref := &refusal{ref: c.ref}
	if c.new != "" {
		if level, _ := s.perm(id, r, "workflows"); levelRank(level) < levelRank(Write) {
			from := c.old
			if from == "" {
				from = s.tip(ctx, r, r.defaultBranch)
			}
			paths, err := rg.changedPaths(ctx, from, c.new, ".github/workflows")
			if err != nil {
				return nil, err
			}
			if len(paths) > 0 {
				ref.workflow = paths[0]
				return ref, nil
			}
		}
	}
	branch, ok := branchOf(c.ref)
	if !ok {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, h := range s.activeRules(r, branch) {
		if h.set.bypasses(id) || seen[h.rule.Type] {
			continue
		}
		hit, err := s.ruleHits(ctx, rg, r, h.rule.Type, c, ref)
		if err != nil {
			return nil, err
		}
		if hit {
			seen[h.rule.Type] = true
			ref.rules = append(ref.rules, ruleMessages[h.rule.Type])
		}
	}
	if len(ref.rules) == 0 {
		return nil, nil
	}
	return ref, nil
}

// ruleHits reports whether a rule refuses c. For required signatures it
// fills ref.unsigned. Called with mu held.
func (s *Server) ruleHits(ctx context.Context, rg *rgit, r *repo, typ string, c refChange, ref *refusal) (bool, error) {
	switch typ {
	case RuleCreation:
		return c.old == "" && c.new != "", nil
	case RuleUpdate:
		return c.old != "" && c.new != "", nil
	case RuleDeletion:
		return c.new == "", nil
	case RulePullRequest:
		return c.old != "" && c.new != "", nil
	case RuleNonFastForward:
		if c.old == "" || c.new == "" {
			return false, nil
		}
		ff, err := rg.isAncestor(ctx, c.old, c.new)
		return !ff, err
	case RuleRequiredSignatures, RuleLinearHistory:
		if c.new == "" {
			return false, nil
		}
		commits, err := s.newCommits(ctx, rg, c)
		if err != nil {
			return false, err
		}
		for _, id := range commits {
			info, err := rg.readCommit(ctx, id)
			if err != nil {
				return false, err
			}
			if typ == RuleLinearHistory {
				if len(info.parents) > 1 {
					return true, nil
				}
				continue
			}
			if !s.verify(info).verified {
				ref.unsigned = append(ref.unsigned, id)
			}
		}
		return len(ref.unsigned) > 0 && typ == RuleRequiredSignatures, nil
	}
	return false, nil
}

// newCommits returns the commits an update brings to its branch: from the
// old tip for an update, and those no other branch has for a new branch
// (assumed: creation checks only commits not reachable from other
// branches).
func (s *Server) newCommits(ctx context.Context, rg *rgit, c refChange) ([]string, error) {
	if c.old != "" {
		return rg.revList(ctx, c.new, []string{c.old})
	}
	branches, err := rg.refs(ctx, "refs/heads/")
	if err != nil {
		return nil, err
	}
	var not []string
	for ref, id := range branches {
		if ref != c.ref {
			not = append(not, id)
		}
	}
	return rg.revList(ctx, c.new, not)
}

// workflowMessage is GitHub's refusal of a workflow change without the
// permission, by identity kind (App: widely quoted; tokens: assumed).
func workflowMessage(id *identity, path string) string {
	switch {
	case id.installation():
		return "refusing to allow a GitHub App to create or update workflow `" + path + "` without `workflows` permission"
	case id != nil && id.kind == idToken && id.tok.kind == tokenFineGrainedPAT:
		return "refusing to allow a Personal Access Token to create or update workflow `" + path + "` without `workflows` permission"
	}
	return "refusing to allow a Personal Access Token to create or update workflow `" + path + "` without `workflow` scope"
}

// gh013Lines are the remote lines of a GH013 refusal for one ref.
func (s *Server) gh013Lines(r *repo, ref *refusal) []string {
	lines := []string{
		"error: GH013: Repository rule violations found for " + ref.ref + ".",
		"Review all repository rules at " + s.http.URL + "/" + r.path() + "/rules?ref=" + url.QueryEscape(ref.ref),
		"",
	}
	for _, msg := range ref.rules {
		lines = append(lines, "- "+msg)
		if msg == ruleMessages[RuleRequiredSignatures] {
			n := len(ref.unsigned)
			noun := "violation"
			if n != 1 {
				noun = "violations"
			}
			lines = append(lines, "  Found "+strconv.Itoa(n)+" "+noun+":", "")
			for _, id := range ref.unsigned {
				lines = append(lines, "  "+id)
			}
		}
		lines = append(lines, "")
	}
	return lines
}

// apiRefusal is the REST answer to a refused API ref update: 403 for the
// Workflows permission (assumed), 422 "Repository rule violations found"
// for rules (public reports).
func apiRefusal(id *identity, ref *refusal) response {
	if ref.workflow != "" {
		resp := apiError(http.StatusForbidden, workflowMessage(id, ref.workflow))
		resp.header = http.Header{"X-Accepted-Github-Permissions": {"workflows=write"}}
		return resp
	}
	return unprocessable("Repository rule violations found\n\n" + strings.Join(ref.rules, "\n\n") + "\n\n")
}

// rulesForBranch answers GET /repos/{owner}/{repo}/rules/branches/{branch}.
func rulesForBranch(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "metadata", Read); refused != nil {
		return *refused
	}
	out := []any{}
	for _, h := range s.activeRules(r, c.param("branch")) {
		item := map[string]any{"type": h.rule.Type, "ruleset_id": h.set.id}
		if h.rule.Parameters != nil {
			item["parameters"] = h.rule.Parameters
		}
		if h.set.org != nil {
			item["ruleset_source_type"], item["ruleset_source"] = "Organization", h.set.org.login
		} else {
			item["ruleset_source_type"], item["ruleset_source"] = "Repository", r.path()
		}
		out = append(out, item)
	}
	return ok(out)
}

// rulesetJSON is the REST form of a ruleset; full adds conditions, rules,
// bypass actors and current_user_can_bypass.
func (c *call) rulesetJSON(r *repo, x *ruleset, full bool) map[string]any {
	out := map[string]any{"id": x.id, "name": x.spec.Name, "target": "branch", "enforcement": x.spec.Enforcement,
		"node_id": nodeID("RepositoryRuleset", x.id), "created_at": fmtTime(x.created), "updated_at": fmtTime(x.created)}
	if x.org != nil {
		out["source_type"], out["source"] = "Organization", x.org.login
	} else {
		out["source_type"], out["source"] = "Repository", r.path()
	}
	if !full {
		return out
	}
	out["conditions"] = map[string]any{"ref_name": map[string]any{"include": nonNil(x.spec.Include), "exclude": nonNil(x.spec.Exclude)}}
	rules := []any{}
	for _, rule := range x.spec.Rules {
		item := map[string]any{"type": rule.Type}
		if rule.Parameters != nil {
			item["parameters"] = rule.Parameters
		}
		rules = append(rules, item)
	}
	out["rules"] = rules
	can := "never"
	if x.bypasses(c.id) {
		can = "always"
	}
	out["current_user_can_bypass"] = can
	return out
}

// nonNil returns list, or an empty list for nil.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// listRulesets answers GET /repos/{owner}/{repo}/rulesets.
func listRulesets(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "metadata", Read); refused != nil {
		return *refused
	}
	out := []any{}
	sets := r.rulesets
	if c.query.Get("includes_parents") != "false" {
		sets = s.rulesetsOf(r)
	}
	for _, x := range sets {
		out = append(out, c.rulesetJSON(r, x, false))
	}
	return ok(out)
}

// getRuleset answers GET /repos/{owner}/{repo}/rulesets/{id}.
func getRuleset(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "metadata", Read); refused != nil {
		return *refused
	}
	id, _ := strconv.ParseInt(c.param("id"), 10, 64)
	for _, x := range s.rulesetsOf(r) {
		if x.id == id {
			return ok(c.rulesetJSON(r, x, true))
		}
	}
	return notFound()
}
