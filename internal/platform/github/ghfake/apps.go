package ghfake

import (
	"net/http"
	"strconv"
	"strings"
)

// needJWT refuses a request that is not authenticated as an App (message
// assumed).
func (c *call) needJWT() *response {
	if c.id != nil && c.id.kind == idJWT {
		return nil
	}
	resp := apiError(http.StatusUnauthorized, jwtUndecodable)
	if c.id != nil {
		resp = apiError(http.StatusForbidden, "A JSON web token is required to access this endpoint.")
	}
	return &resp
}

// needInstallation refuses a request that is not authenticated with an
// installation token (message assumed).
func (c *call) needInstallation() *response {
	if c.id.installation() {
		return nil
	}
	resp := apiError(http.StatusForbidden, "An installation access token is required to access this endpoint.")
	if c.id == nil {
		resp = apiError(http.StatusUnauthorized, "Requires authentication")
	}
	return &resp
}

// appJSON is the App object.
func (c *call) appJSON(a *app) map[string]any {
	return map[string]any{"id": a.id, "slug": a.slug, "node_id": nodeID("Integration", a.id), "client_id": a.clientID,
		"owner": c.accountJSON(a.owner), "name": a.name, "description": "", "external_url": c.s.http.URL,
		"html_url": c.s.http.URL + "/apps/" + a.slug, "permissions": a.perms, "events": []string{},
		"installations_count": c.s.countInstalls(a)}
}

// countInstalls counts an App's installations. Called with mu held.
func (s *Server) countInstalls(a *app) int {
	n := 0
	for _, in := range s.installs {
		if in.app == a {
			n++
		}
	}
	return n
}

// installationJSON is the installation object.
func (c *call) installationJSON(in *installation) map[string]any {
	api := c.base + "/app/installations/" + strconv.FormatInt(in.id, 10)
	out := map[string]any{"id": in.id, "account": c.accountJSON(in.account), "repository_selection": in.selection,
		"access_tokens_url": api + "/access_tokens", "repositories_url": c.base + "/installation/repositories",
		"html_url": c.s.http.URL + "/settings/installations/" + strconv.FormatInt(in.id, 10), "app_id": in.app.id,
		"app_slug": in.app.slug, "client_id": in.app.clientID, "target_id": in.account.id, "target_type": in.account.typ,
		"permissions": in.perms, "events": []string{}, "created_at": fmtTime(in.created), "updated_at": fmtTime(in.created),
		"single_file_name": nil, "suspended_at": nil, "suspended_by": nil}
	if in.suspended {
		out["suspended_at"] = fmtTime(in.created)
	}
	return out
}

// getApp answers GET /app for a JWT.
func getApp(c *call) response {
	if refused := c.needJWT(); refused != nil {
		return *refused
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return ok(c.appJSON(c.id.app))
}

// listInstallations answers GET /app/installations for a JWT.
func listInstallations(c *call) response {
	if refused := c.needJWT(); refused != nil {
		return *refused
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []*installation
	for _, id := range sortedIDs(s.installs) {
		if in := s.installs[id]; in.app == c.id.app {
			list = append(list, in)
		}
	}
	lo, hi, h, _ := c.page(len(list), c.base+"/app/installations")
	out := []any{}
	for _, in := range list[lo:hi] {
		out = append(out, c.installationJSON(in))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// getInstallation answers GET /app/installations/{installation_id}.
func getInstallation(c *call) response {
	if refused := c.needJWT(); refused != nil {
		return *refused
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	in := c.ownInstallation()
	if in == nil {
		return notFound()
	}
	return ok(c.installationJSON(in))
}

// ownInstallation returns the installation of the path, when it belongs to
// the JWT's App. Called with mu held.
func (c *call) ownInstallation() *installation {
	id, _ := strconv.ParseInt(c.param("installation_id"), 10, 64)
	in := c.s.installs[id]
	if in == nil || in.app != c.id.app {
		return nil
	}
	return in
}

// ownerInstallation answers GET /orgs/{org}/installation and
// /users/{username}/installation for a JWT: the App's installation on that
// account, or 404.
func ownerInstallation(c *call) response {
	if refused := c.needJWT(); refused != nil {
		return *refused
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	login := c.param("org")
	if login == "" {
		login = c.param("username")
	}
	a := s.byLogin[strings.ToLower(login)]
	if a == nil || (c.param("org") != "" && a.typ != TypeOrganization) {
		return notFound()
	}
	for _, id := range sortedIDs(s.installs) {
		if in := s.installs[id]; in.app == c.id.app && in.account == a {
			return ok(c.installationJSON(in))
		}
	}
	return notFound()
}

// repoInstallation answers GET /repos/{owner}/{repo}/installation for a
// JWT: the App's installation that covers the repository, or 404 (also
// for a repository the installation does not select).
func repoInstallation(c *call) response {
	if refused := c.needJWT(); refused != nil {
		return *refused
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(c.param("owner") + "/" + c.param("repo"))
	r := s.byPath[key]
	if id, moved := s.redirects[key]; r == nil && moved && !s.apiGone[key] && s.repos[id] != nil && !s.repos[id].deleted {
		return c.redirect(s.repos[id])
	}
	if raw := c.param("repository_id"); raw != "" {
		id, _ := strconv.ParseInt(raw, 10, 64)
		r = s.repos[id]
	}
	if r == nil || r.deleted {
		return notFound()
	}
	for _, id := range sortedIDs(s.installs) {
		if in := s.installs[id]; in.app == c.id.app && in.covers(r) {
			return ok(c.installationJSON(in))
		}
	}
	return notFound()
}

// createAccessToken answers POST
// /app/installations/{installation_id}/access_tokens: a token narrowed to
// repository_ids or repositories (names, at most 500 together; 422 when
// one is not the installation's) and permissions (422 when one exceeds
// the installation's), with metadata read always granted. It expires in
// an hour. Messages of the 422s are from public reports.
func createAccessToken(c *call) response {
	if refused := c.needJWT(); refused != nil {
		return *refused
	}
	var in struct {
		Repositories  []string          `json:"repositories"`
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := c.ownInstallation()
	if inst == nil {
		return notFound()
	}
	if inst.suspended {
		return apiError(http.StatusForbidden, "This installation has been suspended")
	}
	if len(in.Repositories)+len(in.RepositoryIDs) > 500 {
		return unprocessable("Too many repositories: at most 500 may be listed.")
	}
	var repos map[int64]bool
	if in.Repositories != nil || in.RepositoryIDs != nil {
		repos = map[int64]bool{}
		notAccessible := unprocessable("There is at least one repository that does not exist or is not accessible to the parent installation.")
		for _, id := range in.RepositoryIDs {
			r := s.repos[id]
			if r == nil || r.deleted || !inst.covers(r) {
				return notAccessible
			}
			repos[id] = true
		}
		for _, name := range in.Repositories {
			r := s.byPath[strings.ToLower(inst.account.login+"/"+name)]
			if r == nil || !inst.covers(r) {
				return notAccessible
			}
			repos[r.id] = true
		}
	}
	var perms Permissions
	if in.Permissions != nil {
		perms = Permissions{}
		for name, level := range in.Permissions {
			if levelRank(level) == 0 || levelRank(level) > levelRank(inst.perms[name]) {
				return unprocessable("The permissions requested are not granted to this installation.")
			}
			perms[name] = level
		}
	}
	if refused := s.limits.mint(s.now(), inst.app, identityKey(c.id)); refused != nil {
		return *refused
	}
	t := s.mintToken(inst, repos, perms)
	out := map[string]any{"token": t.value, "expires_at": fmtTime(t.expires), "permissions": t.perms,
		"repository_selection": inst.selection}
	if repos != nil {
		out["repository_selection"] = "selected"
		ctx, cancel := c.ctx()
		defer cancel()
		list := []any{}
		for _, id := range sortedIDs(repos) {
			list = append(list, c.repoJSON(ctx, s.repos[id], false))
		}
		out["repositories"] = list
	}
	return created(out)
}

// installationRepos answers GET /installation/repositories: the
// repositories the token reaches, by id.
func installationRepos(c *call) response {
	if refused := c.needInstallation(); refused != nil {
		return *refused
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	t := c.id.tok
	var list []*repo
	for _, id := range sortedIDs(s.repos) {
		r := s.repos[id]
		if !r.deleted && t.inst.covers(r) && (t.repos == nil || t.repos[r.id]) {
			list = append(list, r)
		}
	}
	lo, hi, h, _ := c.page(len(list), c.base+"/installation/repositories")
	ctx, cancel := c.ctx()
	defer cancel()
	out := []any{}
	for _, r := range list[lo:hi] {
		out = append(out, c.repoJSON(ctx, r, false))
	}
	selection := t.inst.selection
	if t.repos != nil {
		selection = "selected"
	}
	return response{status: http.StatusOK, header: h,
		body: map[string]any{"total_count": len(list), "repository_selection": selection, "repositories": out}}
}

// revokeToken answers DELETE /installation/token: the token stops working
// at once (204).
func revokeToken(c *call) response {
	if refused := c.needInstallation(); refused != nil {
		return *refused
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.id.tok.revoked = true
	return noContent()
}
