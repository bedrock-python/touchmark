package setup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// glFake is an in-memory GitLab for setup's tests: the REST endpoints setup
// and hubch.ReadKeyStore use, with GitLab's documented behaviors where
// setup depends on them (docs.gitlab.com/api, read 2026-10-02):
//   - group service accounts (GET/POST /groups/:id/service_accounts, their
//     personal access tokens with rotate and revoke) exist only when
//     saAPI is set (CE before 19.x answers 404); creating one needs
//     saCreate (self-managed lets group Owners create them only with an
//     instance setting);
//   - group access tokens make a bot user that is a member of the group
//     with the token's role; rotation keeps the bot and revokes the old
//     token;
//   - DELETE /personal_access_tokens/self revokes the token that sends it;
//   - variables: hidden only at creation (masked_and_hidden), PUT and
//     DELETE address one scope with filter[environment_scope], masked
//     values of 8 characters at least;
//   - protected branches: PATCH changes access levels only on Premium
//     (premium); on Free it changes allow_force_push alone;
//   - projects show ci_pipeline_variables_minimum_override_role (and the
//     deprecated restrict_user_defined_variables) to Maintainers, and
//     protect_merge_request_pipelines from 18.10.
type glFake struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	version    string
	saAPI      bool
	saCreate   bool
	premium    bool
	next       int64
	users      map[int64]*fUser
	tokens     map[string]*fToken
	groups     map[int64]*fGroup
	projects   map[int64]*fProject
	writes     []string // "METHOD path" of every request that is not a GET
	failRoutes map[string]int
}

type fUser struct {
	id       int64
	username string
	admin    bool
	bot      bool
	saOf     int64 // the group that owns the service account, 0 for none
}

type fToken struct {
	id      int64
	value   string
	user    int64
	name    string
	scopes  []string
	level   int   // group access tokens
	group   int64 // group access tokens: the group
	active  bool
	expires string
}

type fGroup struct {
	id      int64
	path    string // full path
	parent  int64
	members map[int64]int
	vars    []*fVar
}

type fVar struct {
	key, value, scope       string
	protected, masked, hide bool
	varType, description    string
	raw                     bool
}

type fProject struct {
	id            int64
	path          string
	namespace     int64
	defaultBranch string
	members       map[int64]int
	vars          []*fVar
	envs          []string
	branches      map[string]*fProtected
	tags          []string
	settings      map[string]any
	schedules     []map[string]any
}

type fProtected struct {
	push, merge []fLevel
	force       bool
}

type fLevel struct {
	id    int64
	level int
	user  *int64
}

// newGLFake starts the fake with version v.
func newGLFake(t *testing.T, v string) *glFake {
	f := &glFake{t: t, version: v, next: 100, users: map[int64]*fUser{}, tokens: map[string]*fToken{},
		groups: map[int64]*fGroup{}, projects: map[int64]*fProject{}, failRoutes: map[string]int{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *glFake) api() string { return f.srv.URL + "/api/v4" }

func (f *glFake) id() int64 { f.next++; return f.next }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// addUser adds a user with a token and returns both.
func (f *glFake) addUser(name string, admin bool) (*fUser, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := &fUser{id: f.id(), username: name, admin: admin}
	f.users[u.id] = u
	tok := f.mint(u.id, "personal", []string{"api"})
	return u, tok.value
}

// mint makes an active token. Called with mu held.
func (f *glFake) mint(user int64, name string, scopes []string) *fToken {
	t := &fToken{id: f.id(), value: "glpat-" + randHex(12), user: user, name: name, scopes: slices.Clone(scopes), active: true}
	f.tokens[t.value] = t
	return t
}

// addGroup adds a group under parent (0 for none).
func (f *glFake) addGroup(path string, parent int64, members map[int64]int) *fGroup {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := &fGroup{id: f.id(), path: path, parent: parent, members: map[int64]int{}}
	for u, l := range members {
		g.members[u] = l
	}
	f.groups[g.id] = g
	return g
}

// addProject adds a project in the group ns with the default branch main
// protected as GitLab protects a new project's (push and merge
// Maintainers).
func (f *glFake) addProject(name string, ns int64, members map[int64]int) *fProject {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fProject{id: f.id(), path: f.groups[ns].path + "/" + name, namespace: ns, defaultBranch: "main",
		members: map[int64]int{}, branches: map[string]*fProtected{}, settings: map[string]any{}}
	for u, l := range members {
		p.members[u] = l
	}
	p.branches["main"] = &fProtected{push: []fLevel{{id: f.id(), level: 40}}, merge: []fLevel{{id: f.id(), level: 40}}}
	p.settings["ci_pipeline_variables_minimum_override_role"] = "maintainer"
	p.settings["restrict_user_defined_variables"] = false
	if f.atLeast(18, 10) {
		p.settings["protect_merge_request_pipelines"] = true
	}
	f.projects[p.id] = p
	return p
}

// atLeast compares the fake's version.
func (f *glFake) atLeast(major, minor int) bool {
	parts := strings.SplitN(f.version, ".", 3)
	ma, _ := strconv.Atoi(parts[0])
	mi, _ := strconv.Atoi(parts[1])
	return ma > major || ma == major && mi >= minor
}

// writeCount returns how many writes the fake took.
func (f *glFake) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// ancestors returns the group and the groups above it. Called with mu held.
func (f *glFake) ancestors(gid int64) []*fGroup {
	var out []*fGroup
	for g := f.groups[gid]; g != nil; g = f.groups[g.parent] {
		out = append(out, g)
	}
	return out
}

// groupLevel is a user's effective level in a group. Called with mu held.
func (f *glFake) groupLevel(gid, uid int64) int {
	level := 0
	for _, g := range f.ancestors(gid) {
		level = max(level, g.members[uid])
	}
	return level
}

// projectLevel is a user's effective level in a project. Called with mu
// held.
func (f *glFake) projectLevel(p *fProject, uid int64) int {
	return max(p.members[uid], f.groupLevel(p.namespace, uid))
}

func (f *glFake) findGroup(ref string) *fGroup {
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return f.groups[n]
	}
	for _, g := range f.groups {
		if strings.EqualFold(g.path, ref) {
			return g
		}
	}
	return nil
}

func (f *glFake) findProject(ref string) *fProject {
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return f.projects[n]
	}
	for _, p := range f.projects {
		if strings.EqualFold(p.path, ref) {
			return p
		}
	}
	return nil
}

// reply writes a JSON answer.
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func notFound(w http.ResponseWriter) { reply(w, 404, map[string]string{"message": "404 Not found"}) }

func forbidden(w http.ResponseWriter) { reply(w, 403, map[string]string{"message": "403 Forbidden"}) }

func (f *glFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/v4/")
	if !ok {
		notFound(w)
		return
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i], _ = url.PathUnescape(s)
	}
	tok := f.tokens[r.Header.Get("Private-Token")]
	if tok == nil || !tok.active {
		reply(w, 401, map[string]string{"message": "401 Unauthorized"})
		return
	}
	me := f.users[tok.user]
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	route := r.Method + " " + routeOf(segs)
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, route)
	}
	if code, ok := f.failRoutes[route]; ok {
		reply(w, code, map[string]string{"message": "injected"})
		return
	}
	if route == "DELETE personal_access_tokens/:personal_access_tokens" && segs[1] == "self" {
		// Any token revokes itself.
		tok.active = false
		reply(w, 204, nil)
		return
	}
	f.serve(w, r, route, segs, me, body)
}

// routeOf replaces ids and names in a path by placeholders, for routing,
// the journal and injected failures: every second segment after the
// resource it names ("projects/:projects/variables/:variables"), and a
// number elsewhere (members/all/<id>: "…/members/:members/:n").
func routeOf(segs []string) string {
	var out []string
	for i, s := range segs {
		_, err := strconv.Atoi(s)
		switch {
		case i%2 == 1:
			out = append(out, ":"+segs[i-1])
		case err == nil:
			out = append(out, ":n")
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

func num(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func boolOf(v any) bool { b, _ := v.(bool); return b }

func (f *glFake) userJSON(u *fUser) map[string]any {
	return map[string]any{"id": u.id, "username": u.username, "is_admin": u.admin, "bot": u.bot, "state": "active"}
}

func (f *glFake) tokenJSON(t *fToken, withValue bool) map[string]any {
	out := map[string]any{"id": t.id, "name": t.name, "user_id": t.user, "scopes": t.scopes, "active": t.active,
		"revoked": !t.active, "access_level": t.level, "expires_at": t.expires}
	if withValue {
		out["token"] = t.value
	}
	return out
}

func varJSON(v *fVar) map[string]any {
	out := map[string]any{"key": v.key, "environment_scope": v.scope, "protected": v.protected, "masked": v.masked,
		"hidden": v.hide, "variable_type": v.varType, "raw": v.raw, "description": v.description}
	if !v.hide {
		out["value"] = v.value
	}
	return out
}

func levelsJSON(levels []fLevel) []map[string]any {
	out := []map[string]any{}
	for _, l := range levels {
		m := map[string]any{"id": l.id, "access_level": l.level, "user_id": nil, "group_id": nil, "deploy_key_id": nil}
		if l.user != nil {
			m["user_id"] = *l.user
		}
		out = append(out, m)
	}
	return out
}

func (f *glFake) projectJSON(p *fProject, me *fUser) map[string]any {
	g := f.groups[p.namespace]
	level := f.projectLevel(p, me.id)
	out := map[string]any{"id": p.id, "path_with_namespace": p.path, "default_branch": p.defaultBranch, "visibility": "private",
		"namespace": map[string]any{"id": g.id, "kind": "group", "full_path": g.path},
		"permissions": map[string]any{"project_access": map[string]any{"access_level": p.members[me.id]},
			"group_access": map[string]any{"access_level": f.groupLevel(p.namespace, me.id)}}}
	if level >= 40 || me.admin {
		for k, v := range p.settings {
			out[k] = v
		}
	}
	return out
}

// serve answers one request. Called with mu held.
func (f *glFake) serve(w http.ResponseWriter, r *http.Request, route string, s []string, me *fUser, body map[string]any) {
	q := r.URL.Query()
	switch {
	case route == "GET user":
		reply(w, 200, f.userJSON(me))
	case route == "GET version":
		reply(w, 200, map[string]string{"version": f.version + "-ce"})
	case route == "GET users":
		out := []any{}
		for _, u := range f.users {
			if strings.EqualFold(u.username, q.Get("username")) {
				out = append(out, f.userJSON(u))
			}
		}
		reply(w, 200, out)
	case route == "GET users/:users":
		u := f.users[int64(num(s[1]))]
		if u == nil {
			notFound(w)
			return
		}
		reply(w, 200, f.userJSON(u))
	case strings.HasPrefix(route, "GET groups/:groups") || strings.HasPrefix(route, "POST groups/:groups") ||
		strings.HasPrefix(route, "PUT groups/:groups") || strings.HasPrefix(route, "DELETE groups/:groups"):
		f.serveGroup(w, r, route, s, me, body)
	case strings.HasPrefix(route, "GET projects/:projects") || strings.HasPrefix(route, "POST projects/:projects") ||
		strings.HasPrefix(route, "PUT projects/:projects") || strings.HasPrefix(route, "PATCH projects/:projects") ||
		strings.HasPrefix(route, "DELETE projects/:projects"):
		f.serveProject(w, r, route, s, me, body)
	default:
		notFound(w)
	}
}

func (f *glFake) serveGroup(w http.ResponseWriter, r *http.Request, route string, s []string, me *fUser, body map[string]any) {
	g := f.findGroup(s[1])
	if g == nil || (!me.admin && f.groupLevel(g.id, me.id) == 0) {
		notFound(w)
		return
	}
	owner := me.admin || f.groupLevel(g.id, me.id) >= 50
	switch route {
	case "GET groups/:groups":
		out := map[string]any{"id": g.id, "full_path": g.path, "parent_id": nil}
		if g.parent != 0 {
			out["parent_id"] = g.parent
		}
		reply(w, 200, out)
	case "GET groups/:groups/members/:members/:n":
		uid := int64(num(s[4]))
		if l := f.groupLevel(g.id, uid); l > 0 {
			reply(w, 200, map[string]any{"id": uid, "access_level": l})
			return
		}
		notFound(w)
	case "GET groups/:groups/members/:members":
		uid := int64(num(s[3]))
		if l, ok := g.members[uid]; ok {
			reply(w, 200, map[string]any{"id": uid, "access_level": l})
			return
		}
		notFound(w)
	case "POST groups/:groups/members":
		if !owner {
			forbidden(w)
			return
		}
		uid := int64(num(body["user_id"]))
		if f.users[uid] == nil {
			notFound(w)
			return
		}
		if sa := f.users[uid].saOf; sa != 0 && !slices.ContainsFunc(f.ancestors(g.id), func(a *fGroup) bool { return a.id == sa }) {
			reply(w, 400, map[string]string{"message": "service account of another group"})
			return
		}
		g.members[uid] = num(body["access_level"])
		reply(w, 201, map[string]any{"id": uid, "access_level": g.members[uid]})
	case "PUT groups/:groups/members/:members":
		if !owner {
			forbidden(w)
			return
		}
		uid := int64(num(s[3]))
		if _, ok := g.members[uid]; !ok {
			notFound(w)
			return
		}
		g.members[uid] = num(body["access_level"])
		reply(w, 200, map[string]any{"id": uid, "access_level": g.members[uid]})
	case "GET groups/:groups/service_accounts":
		if !f.saAPI {
			notFound(w)
			return
		}
		if !owner {
			forbidden(w)
			return
		}
		out := []any{}
		for _, u := range f.users {
			if u.saOf == g.id {
				out = append(out, f.userJSON(u))
			}
		}
		reply(w, 200, out)
	case "POST groups/:groups/service_accounts":
		if !f.saAPI {
			notFound(w)
			return
		}
		if !owner || !f.saCreate && !me.admin {
			forbidden(w)
			return
		}
		name, _ := body["username"].(string)
		for _, u := range f.users {
			if strings.EqualFold(u.username, name) {
				reply(w, 400, map[string]string{"message": "Username has already been taken"})
				return
			}
		}
		u := &fUser{id: f.id(), username: name, bot: true, saOf: g.id}
		f.users[u.id] = u
		reply(w, 201, f.userJSON(u))
	case "GET groups/:groups/service_accounts/:service_accounts/personal_access_tokens",
		"POST groups/:groups/service_accounts/:service_accounts/personal_access_tokens",
		"POST groups/:groups/service_accounts/:service_accounts/personal_access_tokens/:personal_access_tokens/rotate",
		"DELETE groups/:groups/service_accounts/:service_accounts/personal_access_tokens/:personal_access_tokens":
		f.saTokens(w, r, s, g, owner, body)
	case "GET groups/:groups/access_tokens":
		if !owner {
			forbidden(w)
			return
		}
		out := []any{}
		for _, t := range f.sortedTokens() {
			if t.group == g.id && (r.URL.Query().Get("state") != "active" || t.active) {
				out = append(out, f.tokenJSON(t, false))
			}
		}
		reply(w, 200, out)
	case "POST groups/:groups/access_tokens/:access_tokens/rotate":
		if !owner {
			forbidden(w)
			return
		}
		f.rotate(w, int64(num(s[3])), body, func(t *fToken) bool { return t.group == g.id })
	case "POST groups/:groups/access_tokens":
		if !owner {
			forbidden(w)
			return
		}
		name, _ := body["name"].(string)
		bot := &fUser{id: f.id(), username: fmt.Sprintf("group_%d_bot_%s", g.id, randHex(8)), bot: true}
		f.users[bot.id] = bot
		g.members[bot.id] = num(body["access_level"])
		t := f.mint(bot.id, name, strs(body["scopes"]))
		t.group, t.level, t.expires = g.id, num(body["access_level"]), fmt.Sprint(body["expires_at"])
		reply(w, 201, f.tokenJSON(t, true))
	case "GET groups/:groups/variables":
		out := []any{}
		for _, v := range g.vars {
			out = append(out, varJSON(v))
		}
		reply(w, 200, out)
	default:
		notFound(w)
	}
}

// saTokens answers …/service_accounts/:user_id/personal_access_tokens[/:id[/rotate]].
func (f *glFake) saTokens(w http.ResponseWriter, r *http.Request, s []string, g *fGroup, owner bool, body map[string]any) {
	if !f.saAPI || len(s) < 5 || s[4] != "personal_access_tokens" {
		notFound(w)
		return
	}
	if !owner {
		forbidden(w)
		return
	}
	uid := int64(num(s[3]))
	u := f.users[uid]
	if u == nil || u.saOf != g.id {
		notFound(w)
		return
	}
	switch {
	case r.Method == http.MethodGet && len(s) == 5:
		out := []any{}
		for _, t := range f.sortedTokens() {
			if t.user == uid && (r.URL.Query().Get("state") != "active" || t.active) {
				out = append(out, f.tokenJSON(t, false))
			}
		}
		reply(w, 200, out)
	case r.Method == http.MethodPost && len(s) == 5:
		name, _ := body["name"].(string)
		t := f.mint(uid, name, strs(body["scopes"]))
		t.expires = fmt.Sprint(body["expires_at"])
		reply(w, 201, f.tokenJSON(t, true))
	case r.Method == http.MethodPost && len(s) == 7 && s[6] == "rotate":
		f.rotate(w, int64(num(s[5])), body, func(t *fToken) bool { return t.user == uid })
	case r.Method == http.MethodDelete && len(s) == 6:
		for _, t := range f.tokens {
			if t.id == int64(num(s[5])) && t.user == uid {
				t.active = false
				reply(w, 204, nil)
				return
			}
		}
		notFound(w)
	default:
		notFound(w)
	}
}

// rotate revokes the token id and mints one with the same name, scopes,
// role and bot.
func (f *glFake) rotate(w http.ResponseWriter, id int64, body map[string]any, mine func(*fToken) bool) {
	for _, t := range f.tokens {
		if t.id == id && mine(t) {
			if !t.active {
				reply(w, 401, map[string]string{"message": "401 Unauthorized"})
				return
			}
			if _, ok := body["expires_at"]; !ok {
				f.t.Errorf("rotate without expires_at: GitLab then gives the token a week")
			}
			t.active = false
			n := f.mint(t.user, t.name, t.scopes)
			n.group, n.level, n.expires = t.group, t.level, fmt.Sprint(body["expires_at"])
			reply(w, 200, f.tokenJSON(n, true))
			return
		}
	}
	notFound(w)
}

// sortedTokens returns the tokens by id. Called with mu held.
func (f *glFake) sortedTokens() []*fToken {
	var out []*fToken
	for _, t := range f.tokens {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *fToken) int { return int(a.id - b.id) })
	return out
}

func strs(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func (f *glFake) serveProject(w http.ResponseWriter, r *http.Request, route string, s []string, me *fUser, body map[string]any) {
	p := f.findProject(s[1])
	if p == nil || (!me.admin && f.projectLevel(p, me.id) == 0) {
		notFound(w)
		return
	}
	maintainer := me.admin || f.projectLevel(p, me.id) >= 40
	if r.Method != http.MethodGet && !maintainer {
		forbidden(w)
		return
	}
	switch route {
	case "GET projects/:projects":
		reply(w, 200, f.projectJSON(p, me))
	case "PUT projects/:projects":
		for k, v := range body {
			if _, known := p.settings[k]; known {
				p.settings[k] = v
			}
		}
		reply(w, 200, f.projectJSON(p, me))
	case "GET projects/:projects/members/:members/:n":
		uid := int64(num(s[4]))
		if l := f.projectLevel(p, uid); l > 0 {
			reply(w, 200, map[string]any{"id": uid, "access_level": l})
			return
		}
		notFound(w)
	case "GET projects/:projects/environments":
		out := []any{}
		for _, e := range p.envs {
			if q := r.URL.Query().Get("name"); q == "" || q == e {
				out = append(out, map[string]any{"name": e})
			}
		}
		reply(w, 200, out)
	case "POST projects/:projects/environments":
		name, _ := body["name"].(string)
		if slices.Contains(p.envs, name) {
			reply(w, 400, map[string]string{"message": "name has already been taken"})
			return
		}
		p.envs = append(p.envs, name)
		reply(w, 201, map[string]any{"name": name})
	case "GET projects/:projects/variables":
		out := []any{}
		for _, v := range p.vars {
			out = append(out, varJSON(v))
		}
		reply(w, 200, out)
	case "POST projects/:projects/variables":
		v := &fVar{key: fmt.Sprint(body["key"]), value: fmt.Sprint(body["value"]), scope: "*", varType: "env_var",
			protected: boolOf(body["protected"]), masked: boolOf(body["masked"]) || boolOf(body["masked_and_hidden"]),
			hide: boolOf(body["masked_and_hidden"]), raw: boolOf(body["raw"])}
		if sc, ok := body["environment_scope"].(string); ok {
			v.scope = sc
		}
		if v.masked && len(v.value) < 8 {
			reply(w, 400, map[string]any{"message": map[string]any{"value": []string{"is invalid"}}})
			return
		}
		for _, o := range p.vars {
			if o.key == v.key && o.scope == v.scope {
				reply(w, 400, map[string]any{"message": map[string]any{"key": []string{"has already been taken"}}})
				return
			}
		}
		p.vars = append(p.vars, v)
		reply(w, 201, varJSON(v))
	case "PUT projects/:projects/variables/:variables", "DELETE projects/:projects/variables/:variables":
		scope := r.URL.Query().Get("filter[environment_scope]")
		var matches []int
		for i, v := range p.vars {
			if v.key == s[3] && (scope == "" || v.scope == scope) {
				matches = append(matches, i)
			}
		}
		switch {
		case len(matches) == 0:
			notFound(w)
			return
		case len(matches) > 1:
			reply(w, 409, map[string]string{"message": "There are multiple variables with provided parameters. Please use 'filter[environment_scope]'"})
			return
		}
		v := p.vars[matches[0]]
		if r.Method == http.MethodDelete {
			p.vars = slices.Delete(p.vars, matches[0], matches[0]+1)
			reply(w, 204, nil)
			return
		}
		if x, ok := body["value"]; ok {
			v.value = fmt.Sprint(x)
		}
		if x, ok := body["protected"]; ok {
			v.protected = boolOf(x)
		}
		if x, ok := body["masked"]; ok {
			v.masked = boolOf(x)
		}
		if x, ok := body["variable_type"].(string); ok {
			v.varType = x
		}
		if x, ok := body["environment_scope"].(string); ok {
			v.scope = x
		}
		reply(w, 200, varJSON(v))
	case "GET projects/:projects/protected_branches":
		if len(s) == 3 {
			out := []any{}
			for name, b := range p.branches {
				out = append(out, map[string]any{"name": name, "push_access_levels": levelsJSON(b.push), "merge_access_levels": levelsJSON(b.merge), "allow_force_push": b.force})
			}
			reply(w, 200, out)
			return
		}
		notFound(w)
	case "GET projects/:projects/protected_branches/:protected_branches":
		b := p.branches[s[3]]
		if b == nil {
			notFound(w)
			return
		}
		reply(w, 200, map[string]any{"name": s[3], "push_access_levels": levelsJSON(b.push), "merge_access_levels": levelsJSON(b.merge), "allow_force_push": b.force})
	case "POST projects/:projects/protected_branches":
		name := fmt.Sprint(body["name"])
		if p.branches[name] != nil {
			reply(w, 409, map[string]string{"message": "Protected branch '" + name + "' already exists"})
			return
		}
		p.branches[name] = &fProtected{push: []fLevel{{id: f.id(), level: num(body["push_access_level"])}},
			merge: []fLevel{{id: f.id(), level: num(body["merge_access_level"])}}, force: boolOf(body["allow_force_push"])}
		reply(w, 201, map[string]any{"name": name})
	case "PATCH projects/:projects/protected_branches/:protected_branches":
		b := p.branches[s[3]]
		if b == nil {
			notFound(w)
			return
		}
		if x, ok := body["allow_force_push"]; ok {
			b.force = boolOf(x)
		}
		if f.premium {
			b.push = f.applyLevels(b.push, body["allowed_to_push"])
			b.merge = f.applyLevels(b.merge, body["allowed_to_merge"])
		}
		reply(w, 200, map[string]any{"name": s[3]})
	case "DELETE projects/:projects/protected_branches/:protected_branches":
		if p.branches[s[3]] == nil {
			notFound(w)
			return
		}
		delete(p.branches, s[3])
		reply(w, 204, nil)
	case "GET projects/:projects/protected_tags":
		out := []any{}
		for _, t := range p.tags {
			out = append(out, map[string]any{"name": t, "create_access_levels": []any{map[string]any{"access_level": 30}}})
		}
		reply(w, 200, out)
	case "GET projects/:projects/pipeline_schedules":
		out := []any{}
		for _, sc := range p.schedules {
			out = append(out, sc)
		}
		reply(w, 200, out)
	case "POST projects/:projects/pipeline_schedules":
		sc := map[string]any{"id": f.id(), "description": body["description"], "ref": body["ref"], "cron": body["cron"],
			"cron_timezone": body["cron_timezone"], "active": body["active"], "owner": map[string]any{"username": me.username}}
		p.schedules = append(p.schedules, sc)
		reply(w, 201, sc)
	default:
		notFound(w)
	}
}

// applyLevels applies a PATCH's allowed_to_* array (Premium).
func (f *glFake) applyLevels(levels []fLevel, change any) []fLevel {
	list, _ := change.([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		if boolOf(m["_destroy"]) {
			id := int64(num(m["id"]))
			levels = slices.DeleteFunc(levels, func(l fLevel) bool { return l.id == id })
			continue
		}
		if _, ok := m["access_level"]; ok {
			levels = append(levels, fLevel{id: f.id(), level: num(m["access_level"])})
		}
	}
	return levels
}

// variable returns a variable of a project by key and scope.
func (f *glFake) variable(p *fProject, key, scope string) *fVar {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range p.vars {
		if v.key == key && v.scope == scope {
			return v
		}
	}
	return nil
}

// tokenOf returns the token of a value.
func (f *glFake) tokenOf(value string) *fToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[value]
}
