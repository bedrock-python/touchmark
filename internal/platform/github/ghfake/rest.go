package ghfake

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// buildRoutes returns the REST routes. Every route under
// /repos/{owner}/{repo} is also served under /repositories/{repository_id},
// the form of GitHub's redirects and Link headers, and logged under the
// /repos template.
func buildRoutes() []route {
	base := []route{
		newRoute("GET", "/rate_limit", getRateLimit),
		newRoute("GET", "/meta", getMeta),
		newRoute("GET", "/app", getApp),
		newRoute("GET", "/app/installations", listInstallations),
		newRoute("GET", "/app/installations/{installation_id}", getInstallation),
		newRoute("POST", "/app/installations/{installation_id}/access_tokens", createAccessToken),
		newRoute("GET", "/orgs/{org}/installation", ownerInstallation),
		newRoute("GET", "/users/{username}/installation", ownerInstallation),
		newRoute("GET", "/installation/repositories", installationRepos),
		newRoute("DELETE", "/installation/token", revokeToken),
		newRoute("GET", "/user", getAuthenticatedUser),
		newRoute("GET", "/user/repos", listUserRepos),
		newRoute("GET", "/users/{username}", getUser),
		newRoute("GET", "/orgs/{org}", getOrg),
		newRoute("GET", "/orgs/{org}/repos", listOwnerRepos),
		newRoute("GET", "/organizations/{org_id}/repos", listOwnerRepos),
		newRoute("GET", "/users/{username}/repos", listOwnerRepos),
		newRoute("GET", "/user/{user_id}/repos", listOwnerRepos),
		newRoute("GET", "/repositories/{repository_id}", getRepo),
	}
	perRepo := []route{
		newRoute("GET", "/repos/{owner}/{repo}", getRepo),
		newRoute("GET", "/repos/{owner}/{repo}/installation", repoInstallation),
		newRoute("GET", "/repos/{owner}/{repo}/topics", getTopics),
		newRoute("GET", "/repos/{owner}/{repo}/hash-algorithm", getHashAlgorithm),
		newRoute("GET", "/repos/{owner}/{repo}/contents/{path...}", getContents),
		newRoute("GET", "/repos/{owner}/{repo}/git/trees/{sha...}", getTree),
		newRoute("GET", "/repos/{owner}/{repo}/git/blobs/{sha}", getBlob),
		newRoute("GET", "/repos/{owner}/{repo}/git/commits/{sha}", getGitCommit),
		newRoute("POST", "/repos/{owner}/{repo}/git/commits", createGitCommit),
		newRoute("GET", "/repos/{owner}/{repo}/commits/{ref...}", getCommit),
		newRoute("GET", "/repos/{owner}/{repo}/compare/{basehead...}", compareCommits),
		newRoute("GET", "/repos/{owner}/{repo}/branches", listBranches),
		newRoute("GET", "/repos/{owner}/{repo}/branches/{branch...}", getBranch),
		newRoute("GET", "/repos/{owner}/{repo}/git/ref/{ref...}", getRef),
		newRoute("GET", "/repos/{owner}/{repo}/git/matching-refs/{ref...}", matchingRefs),
		newRoute("POST", "/repos/{owner}/{repo}/git/refs", createRef),
		newRoute("PATCH", "/repos/{owner}/{repo}/git/refs/{ref...}", updateRef),
		newRoute("DELETE", "/repos/{owner}/{repo}/git/refs/{ref...}", deleteRef),
		newRoute("GET", "/repos/{owner}/{repo}/rules/branches/{branch...}", rulesForBranch),
		newRoute("GET", "/repos/{owner}/{repo}/rulesets", listRulesets),
		newRoute("GET", "/repos/{owner}/{repo}/rulesets/{id}", getRuleset),
		newRoute("GET", "/repos/{owner}/{repo}/pulls", listPulls),
		newRoute("POST", "/repos/{owner}/{repo}/pulls", createPull),
		newRoute("GET", "/repos/{owner}/{repo}/pulls/{number}", getPull),
		newRoute("PATCH", "/repos/{owner}/{repo}/pulls/{number}", updatePull),
		newRoute("PUT", "/repos/{owner}/{repo}/pulls/{number}/merge", mergePull),
		newRoute("GET", "/repos/{owner}/{repo}/issues/{number}/comments", listComments),
		newRoute("POST", "/repos/{owner}/{repo}/issues/{number}/comments", createComment),
		newRoute("POST", "/repos/{owner}/{repo}/issues/{number}/labels", addLabels),
		newRoute("GET", "/repos/{owner}/{repo}/labels", listLabels),
		newRoute("POST", "/repos/{owner}/{repo}/labels", createLabel),
		newRoute("GET", "/repos/{owner}/{repo}/labels/{name}", getLabel),
	}
	settingsBase, settingsRepo := settingsRoutes()
	base = append(base, settingsBase...)
	perRepo = append(perRepo, settingsRepo...)
	out := base
	for _, rt := range perRepo {
		out = append(out, rt)
		alias := newRoute(rt.method, "/repositories/{repository_id}"+strings.TrimPrefix(rt.pattern, "/repos/{owner}/{repo}"), rt.h)
		if alias.pattern == "/repositories/{repository_id}" {
			continue
		}
		alias.canonical = rt.pattern
		out = append(out, alias)
	}
	return out
}

// repo resolves the repository of the request ({owner}/{repo} or
// {repository_id}) for c.id: 404 when it does not see it, and a redirect
// for a renamed or transferred one (301 for GET and HEAD, 307 otherwise,
// to /repositories/{id} with the rest of the path; observed read-only
// 2026-09-29 for a transferred repository); 404 for the old path of a
// repository whose owner was renamed (docs). Called with mu held.
func (c *call) repo() (*repo, *response) {
	s := c.s
	fail := func(resp response) (*repo, *response) { return nil, &resp }
	if raw := c.param("repository_id"); raw != "" {
		id, _ := strconv.ParseInt(raw, 10, 64)
		r := s.repos[id]
		if r != nil {
			s.noteScope(c.id, r)
		}
		if r == nil || !s.canSee(c.id, r) {
			return fail(notFound())
		}
		return r, nil
	}
	key := strings.ToLower(c.param("owner") + "/" + c.param("repo"))
	if r := s.byPath[key]; r != nil {
		s.noteScope(c.id, r)
		if !s.canSee(c.id, r) {
			return fail(notFound())
		}
		return r, nil
	}
	id, moved := s.redirects[key]
	r := s.repos[id]
	if !moved || r == nil || s.apiGone[key] || !s.canSee(c.id, r) {
		return fail(notFound())
	}
	return fail(c.redirect(r))
}

// redirect is the answer to the old path of a renamed or transferred
// repository r.
func (c *call) redirect(r *repo) response {
	segs := strings.SplitN(strings.TrimPrefix(c.path, "/"), "/", 4)
	rest := ""
	if len(segs) == 4 {
		rest = "/" + segs[3]
	}
	loc := c.base + "/repositories/" + strconv.FormatInt(r.id, 10) + rest
	if c.r.URL.RawQuery != "" {
		loc += "?" + c.r.URL.RawQuery
	}
	status, msg := http.StatusMovedPermanently, "Moved Permanently"
	if c.r.Method != http.MethodGet && c.r.Method != http.MethodHead {
		status, msg = http.StatusTemporaryRedirect, "Temporary Redirect"
	}
	return response{status: status, header: http.Header{"Location": {loc}}, body: map[string]any{
		"message": msg, "url": loc,
		"documentation_url": "https://docs.github.com/rest/guides/best-practices-for-using-the-rest-api#follow-redirects"}}
}

// ctx returns a context for git work of a request.
func (c *call) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.r.Context(), gitTimeout)
}

// accountJSON is the simple user object.
func (c *call) accountJSON(a *account) any {
	if a == nil {
		return nil
	}
	html := c.s.http.URL + "/" + a.login
	if a.typ == TypeBot && a.app != nil {
		html = c.s.http.URL + "/apps/" + a.app.slug
	}
	return map[string]any{"login": a.login, "id": a.id, "node_id": nodeID(a.typ, a.id), "type": a.typ,
		"site_admin": false, "html_url": html, "url": c.base + "/users/" + url.PathEscape(a.login)}
}

// repoJSON is the repository object; full adds parent and source of forks.
func (c *call) repoJSON(ctx context.Context, r *repo, full bool) map[string]any {
	s := c.s
	api := c.base + "/repos/" + escapePath(r.path())
	empty := s.tip(ctx, r, r.defaultBranch) == ""
	size := 1
	if empty {
		if refs, err := s.repoGit(r).refs(ctx, "refs/heads/"); err == nil && len(refs) == 0 {
			size = 0
		}
	}
	out := map[string]any{
		"id": r.id, "node_id": nodeID("Repository", r.id), "name": r.name, "full_name": r.path(),
		"private": r.private(), "owner": c.accountJSON(r.owner), "html_url": s.http.URL + "/" + r.path(),
		"description": nil, "fork": r.parent != nil, "url": api, "clone_url": s.CloneURL(r.path()),
		"ssh_url": "git@127.0.0.1:" + r.path() + ".git", "created_at": fmtTime(r.created),
		"updated_at": fmtTime(r.pushed), "pushed_at": fmtTime(r.pushed), "size": size,
		"default_branch": r.defaultBranch, "archived": r.archived, "disabled": r.disabled,
		"is_template": r.template, "topics": nonNil(r.topics), "visibility": r.visibility,
		"has_issues": true, "has_pull_requests": !r.prsDisabled, "pull_request_creation_policy": r.prPolicy,
		"mirror_url": nil, "allow_forking": true, "web_commit_signoff_required": false,
		"pulls_url": api + "/pulls{/number}", "contents_url": api + "/contents/{+path}",
	}
	if r.mirror != "" {
		out["mirror_url"] = r.mirror
	}
	if c.id != nil && c.id.kind == idToken {
		level, _ := s.perm(c.id, r, "contents")
		admin := c.id.tok.kind != tokenInstallation && repoLevel(c.id.who, r) == "admin"
		out["permissions"] = map[string]any{"admin": admin, "maintain": admin, "push": levelRank(level) >= levelRank(Write),
			"triage": levelRank(level) >= levelRank(Write), "pull": level != ""}
	}
	if full && r.parent != nil {
		out["parent"] = c.repoJSON(ctx, r.parent, false)
		src := r.parent
		for src.parent != nil {
			src = src.parent
		}
		out["source"] = c.repoJSON(ctx, src, false)
	}
	return out
}

// getRepo answers GET /repos/{owner}/{repo} and /repositories/{id}.
func getRepo(c *call) response {
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
	ctx, cancel := c.ctx()
	defer cancel()
	return ok(c.repoJSON(ctx, r, true))
}

// getTopics answers GET /repos/{owner}/{repo}/topics.
func getTopics(c *call) response {
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
	return ok(map[string]any{"names": nonNil(r.topics)})
}

// getHashAlgorithm answers GET /repos/{owner}/{repo}/hash-algorithm
// (github.com only; GitHub Enterprise Server 3.19 lacks it).
func getHashAlgorithm(c *call) response {
	s := c.s
	if s.flavor == GHES {
		return notFound()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "metadata", Read); refused != nil {
		return *refused
	}
	return ok(map[string]any{"hash_algorithm": r.objectFormat})
}

// getMeta answers GET /meta, anonymous or with any credential: github.com
// lists its services and keys without a version (observed read-only
// 2026-09-29: no installed_version); GHES names its version in
// installed_version (docs: rest/meta/meta on GHES). Only
// verifiable_password_authentication and installed_version are modeled.
func getMeta(c *call) response {
	body := map[string]any{"verifiable_password_authentication": c.s.flavor == GHES}
	if v := c.s.opts.Version; v != "" {
		body["installed_version"] = v
	}
	return ok(body)
}

// getRateLimit answers GET /rate_limit, which costs nothing.
func getRateLimit(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	res := map[string]any{}
	for _, resource := range []string{"core", "graphql"} {
		limit := s.limits.primaryLimit(c.id, resource)
		w := s.limits.window(now, resource+":"+identityKey(c.id))
		res[resource] = map[string]any{"limit": limit, "remaining": max(limit-w.used, 0), "used": w.used,
			"reset": w.start.Add(3600e9).Unix(), "resource": resource}
	}
	return ok(map[string]any{"resources": res, "rate": res["core"]})
}

// getAuthenticatedUser answers GET /user: the user of a personal access
// token; an installation token gets 403 "Resource not accessible by
// integration" (GitHub's documentation).
func getAuthenticatedUser(c *call) response {
	switch {
	case c.id == nil:
		return apiError(http.StatusUnauthorized, "Requires authentication")
	case c.id.kind != idToken || c.id.tok.kind == tokenInstallation:
		return apiError(http.StatusForbidden, "Resource not accessible by integration")
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return ok(c.userJSON(c.id.who))
}

// userJSON is the full user object.
func (c *call) userJSON(a *account) map[string]any {
	out := c.accountJSON(a).(map[string]any)
	out["name"] = a.name
	if a.typ == TypeBot {
		out["name"] = nil
	}
	out["email"] = nil
	out["created_at"] = fmtTime(a.created)
	out["updated_at"] = fmtTime(a.created)
	return out
}

// getUser answers GET /users/{username} for users, bots ("<slug>[bot]")
// and organizations. A renamed account's old login is not found.
func getUser(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(c.param("username"))]
	if a == nil {
		return notFound()
	}
	return ok(c.userJSON(a))
}

// getOrg answers GET /orgs/{org}.
func getOrg(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.byLogin[strings.ToLower(c.param("org"))]
	if a == nil || a.typ != TypeOrganization {
		return notFound()
	}
	return ok(map[string]any{"login": a.login, "id": a.id, "node_id": nodeID(a.typ, a.id), "type": a.typ,
		"url": c.base + "/orgs/" + url.PathEscape(a.login), "repos_url": c.base + "/orgs/" + url.PathEscape(a.login) + "/repos",
		"html_url": s.http.URL + "/" + a.login, "created_at": fmtTime(a.created)})
}

// listOwnerRepos answers GET /orgs/{org}/repos, /users/{username}/repos
// and their id forms: the repositories of the owner id sees, filtered by
// type (all, public, private, forks, sources, member) and sorted by
// created (newest first), updated, pushed or full_name (A to Z). An
// installation token also sees public repositories its installation does
// not cover (assumed; needs the live sandbox). The user forms list public
// repositories only, whoever asks (docs:
// rest/repos/repos#list-repositories-for-a-user, "Lists public
// repositories for the specified user").
func listOwnerRepos(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	publicOnly := c.param("username") != "" || c.param("user_id") != ""
	var owner *account
	switch {
	case c.param("org") != "":
		owner = s.byLogin[strings.ToLower(c.param("org"))]
		if owner != nil && owner.typ != TypeOrganization {
			owner = nil
		}
	case c.param("username") != "":
		owner = s.byLogin[strings.ToLower(c.param("username"))]
	default:
		raw := c.param("org_id")
		if raw == "" {
			raw = c.param("user_id")
		}
		id, _ := strconv.ParseInt(raw, 10, 64)
		owner = s.accounts[id]
	}
	if owner == nil {
		return notFound()
	}
	typ := c.query.Get("type")
	var list []*repo
	for _, id := range sortedIDs(s.repos) {
		r := s.repos[id]
		if r.owner != owner || r.deleted || !s.canSee(c.id, r) || (publicOnly && r.private()) {
			continue
		}
		switch typ {
		case "public":
			if r.private() {
				continue
			}
		case "private":
			if !r.private() {
				continue
			}
		case "forks":
			if r.parent == nil {
				continue
			}
		case "sources":
			if r.parent != nil {
				continue
			}
		}
		list = append(list, r)
	}
	sortRepos(list, c.query.Get("sort"), c.query.Get("direction"))
	link := c.base + "/organizations/" + strconv.FormatInt(owner.id, 10) + "/repos"
	if owner.typ != TypeOrganization {
		link = c.base + "/user/" + strconv.FormatInt(owner.id, 10) + "/repos"
	}
	lo, hi, h, bad := c.page(len(list), link)
	if bad != nil {
		return *bad
	}
	ctx, cancel := c.ctx()
	defer cancel()
	out := []any{}
	for _, r := range list[lo:hi] {
		out = append(out, c.repoJSON(ctx, r, false))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// listUserRepos answers GET /user/repos for a personal access token: the
// repositories its user owns (affiliation owner) or collaborates on
// (collaborator; organization membership is not modeled), those the
// token reaches, filtered by visibility (all, public, private) or type
// (all, owner, public, private, member; 422 together with visibility or
// affiliation, as documented) and sorted as the other listings, full_name
// by default (docs: rest/repos/repos#list-repositories-for-the-
// authenticated-user). An installation token gets 403.
func listUserRepos(c *call) response {
	switch {
	case c.id == nil:
		return apiError(http.StatusUnauthorized, "Requires authentication")
	case c.id.kind != idToken || c.id.tok.kind == tokenInstallation:
		return apiError(http.StatusForbidden, "Resource not accessible by integration")
	}
	q := c.query
	typ, vis, aff := q.Get("type"), q.Get("visibility"), q.Get("affiliation")
	if typ != "" && (vis != "" || aff != "") {
		return validation(customError("Repository", "type", "type cannot be used with visibility or affiliation"))
	}
	affs := map[string]bool{"owner": true, "collaborator": true, "organization_member": true}
	if aff != "" {
		affs = map[string]bool{}
		for _, a := range strings.Split(aff, ",") {
			affs[strings.TrimSpace(a)] = true
		}
	}
	switch typ {
	case "owner":
		affs = map[string]bool{"owner": true}
	case "member":
		affs = map[string]bool{"collaborator": true, "organization_member": true}
	case "public", "private":
		vis = typ
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	who := c.id.who
	var list []*repo
	for _, id := range sortedIDs(s.repos) {
		r := s.repos[id]
		if r.deleted || !s.canSee(c.id, r) || (vis == "public" && r.private()) || (vis == "private" && !r.private()) {
			continue
		}
		if _, inScope := s.perm(c.id, r, "metadata"); !inScope {
			continue
		}
		owned := r.owner == who
		if (owned && affs["owner"]) || (!owned && r.collaborators[who.id] != "" && affs["collaborator"]) {
			list = append(list, r)
		}
	}
	by := q.Get("sort")
	if by == "" {
		by = "full_name"
	}
	sortRepos(list, by, q.Get("direction"))
	lo, hi, h, bad := c.page(len(list), c.base+"/user/repos")
	if bad != nil {
		return *bad
	}
	ctx, cancel := c.ctx()
	defer cancel()
	out := []any{}
	for _, r := range list[lo:hi] {
		out = append(out, c.repoJSON(ctx, r, false))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// sortRepos orders a listing: created (the default) and the other dates
// newest first, full_name A to Z, direction overriding.
func sortRepos(list []*repo, by, dir string) {
	asc := by == "full_name"
	switch dir {
	case "asc":
		asc = true
	case "desc":
		asc = false
	}
	less := func(a, b *repo) bool {
		switch by {
		case "full_name":
			return strings.ToLower(a.path()) < strings.ToLower(b.path())
		case "updated", "pushed":
			if !a.pushed.Equal(b.pushed) {
				return a.pushed.Before(b.pushed)
			}
		}
		return a.id < b.id
	}
	sort.SliceStable(list, func(i, j int) bool {
		if asc {
			return less(list[i], list[j])
		}
		return less(list[j], list[i])
	})
}

// page applies per_page (default 30, at most 100) and page to a listing of
// n items and returns the bounds and the Link header, whose URLs use link
// with the request's other query parameters (observed read-only
// 2026-09-29: prev, next, last, first, in that order).
func (c *call) page(n int, link string) (lo, hi int, h http.Header, bad *response) {
	perPage, page := 30, 1
	if v := c.query.Get("per_page"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 {
			p = 30
		}
		perPage = min(p, 100)
	}
	if v := c.query.Get("page"); v != "" {
		p, err := strconv.Atoi(v)
		if err == nil && p > 0 {
			page = p
		}
	}
	last := max((n+perPage-1)/perPage, 1)
	lo = min((page-1)*perPage, n)
	hi = min(lo+perPage, n)
	var kept []string
	for _, kv := range strings.Split(c.r.URL.RawQuery, "&") {
		if kv != "" && !strings.HasPrefix(kv, "page=") && kv != "page" {
			kept = append(kept, kv)
		}
	}
	at := func(p int) string {
		return "<" + link + "?" + strings.Join(append(slices.Clone(kept), "page="+strconv.Itoa(p)), "&") + ">"
	}
	var parts []string
	if page > 1 {
		parts = append(parts, at(min(page-1, last))+`; rel="prev"`)
	}
	if page < last {
		parts = append(parts, at(page+1)+`; rel="next"`, at(last)+`; rel="last"`)
	}
	if page > 1 {
		parts = append(parts, at(1)+`; rel="first"`)
	}
	if len(parts) > 0 {
		h = http.Header{"Link": {strings.Join(parts, ", ")}}
	}
	return lo, hi, h, nil
}
