package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// The fixtures of these tests follow the REST descriptions of
// api.github.com, ghec and ghes-3.19 (github/rest-api-description) and
// the GraphQL schemas of github.com and GHES 3.19, and answers recorded
// with read-only requests to public github.com repositories on
// 2026-09-29, trimmed to the fields the driver reads (and a few it
// ignores) and anonymized: the owners are acme and octo-org, the people
// alice and bob, the bots touchmark-write[bot] and acme-janitor[bot]; ids
// are made up. No real login, name, email or avatar is kept.

// apiServer is an httptest server that answers the REST routes a test
// declares under /api/v3 and GraphQL at /api/graphql, and records every
// request. An undeclared route fails the test.
type apiServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // "METHOD /path" (API-relative, unescaped)
	// synthetic marks a server whose routes test the client's plumbing
	// (paging, errors), not REST calls of the driver: the REST contract
	// is not checked there.
	synthetic bool
	gql       []gqlRoute
	calls     []apiCall
}

// gqlRoute answers GraphQL requests whose query contains marker.
type gqlRoute struct {
	marker string
	h      func(w http.ResponseWriter, req gqlCall)
}

// apiCall is one request the server received.
type apiCall struct {
	Method, Path, RawPath string
	Query                 url.Values
	Auth                  string
	Accept, Version       string
	Body                  string
}

// gqlCall is a GraphQL request as the server read it.
type gqlCall struct {
	Query     string
	Variables map[string]any
	Auth      string
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// api is the REST base (the GHES layout); GraphQL is at /api/graphql.
func (s *apiServer) api() string { return s.srv.URL + "/api/v3" }

func (s *apiServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	s.mu.Lock()
	s.calls = append(s.calls, apiCall{Method: r.Method, Path: path, RawPath: r.URL.EscapedPath(), Query: r.URL.Query(),
		Auth: r.Header.Get("Authorization"), Accept: r.Header.Get("Accept"), Version: r.Header.Get("X-GitHub-Api-Version"),
		Body: string(body)})
	h := s.routes[r.Method+" "+path]
	gql := slices.Clone(s.gql)
	synthetic := s.synthetic
	s.mu.Unlock()
	if r.URL.Path == "/api/graphql" && r.Method == http.MethodPost {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.t.Errorf("GraphQL request does not decode: %v", err)
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Problems parsing JSON"})
			return
		}
		for i := len(gql) - 1; i >= 0; i-- {
			if strings.Contains(req.Query, gql[i].marker) {
				gql[i].h(w, gqlCall{Query: req.Query, Variables: req.Variables, Auth: r.Header.Get("Authorization")})
				return
			}
		}
		s.t.Errorf("unexpected GraphQL query:\n%s", req.Query)
		writeJSON(w, http.StatusOK, map[string]any{"errors": []any{map[string]any{"message": "no route in the test"}}})
		return
	}
	if err := checkRESTContract(r.Method, path, r.URL.Query()); err != nil && !synthetic {
		s.t.Error(err)
	}
	if h == nil {
		s.t.Errorf("unexpected request %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		writeJSON(w, http.StatusNotFound, notFoundBody)
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	h(w, r)
}

// notFoundBody is GitHub's 404 (recorded from api.github.com).
var notFoundBody = map[string]any{"message": "Not Found", "documentation_url": "https://docs.github.com/rest", "status": "404"}

// handle declares a REST route: method and an API path such as
// "/repos/acme/api" (unescaped).
func (s *apiServer) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// json declares a route that answers status with v as JSON.
func (s *apiServer) json(method, path string, status int, v any) {
	s.handle(method, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// graphql declares the answer to GraphQL queries holding marker; later
// declarations win.
func (s *apiServer) graphql(marker string, h func(w http.ResponseWriter, req gqlCall)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gql = append(s.gql, gqlRoute{marker: marker, h: h})
}

// requests returns the recorded calls of method to path ("" matches all).
func (s *apiServer) requests(method, path string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []apiCall
	for _, c := range s.calls {
		if (method == "" || c.Method == method) && (path == "" || c.Path == path) {
			out = append(out, c)
		}
	}
	return out
}

// gqlCalls returns the recorded GraphQL requests.
func (s *apiServer) gqlCalls() []apiCall { return s.requests(http.MethodPost, "/api/graphql") }

// reset forgets the recorded calls.
func (s *apiServer) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ghError is GitHub's error body with a message and validation errors.
func ghError(message string, errs ...any) map[string]any {
	m := map[string]any{"message": message, "documentation_url": "https://docs.github.com/rest"}
	if len(errs) > 0 {
		m["errors"] = errs
	}
	return m
}

// pages answers a REST listing from items by page and per_page, with the
// Link header GitHub sends (absolute URLs, rel next/last/first/prev).
func (s *apiServer) pages(path string, items []any) {
	s.handle(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) { servePage(w, r, items, nil) })
}

// servePage writes the page of items r asks for; wrap, when set, wraps
// the page (GET /installation/repositories: {total_count, repositories}).
func servePage(w http.ResponseWriter, r *http.Request, items []any, wrap func([]any) any) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per <= 0 {
		per = 30
	}
	if page <= 0 {
		page = 1
	}
	from := min((page-1)*per, len(items))
	to := min(from+per, len(items))
	last := (len(items) + per - 1) / per
	if last > 1 {
		var links []string
		link := func(p int, rel string) {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(p))
			links = append(links, fmt.Sprintf("<http://%s%s?%s>; rel=%q", r.Host, r.URL.Path, q.Encode(), rel))
		}
		if page < last {
			link(page+1, "next")
			link(last, "last")
		}
		if page > 1 {
			link(1, "first")
			link(page-1, "prev")
		}
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	chunk := items[from:to]
	if chunk == nil {
		chunk = []any{}
	}
	var v any = chunk
	if wrap != nil {
		v = wrap(chunk)
	}
	writeJSON(w, http.StatusOK, v)
}

// gqlData writes a GraphQL answer with data and errors.
func gqlData(w http.ResponseWriter, data any, errs ...map[string]any) {
	body := map[string]any{"data": data}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	writeJSON(w, http.StatusOK, body)
}

// gqlErr is a GraphQL error of GitHub: type, path and message.
func gqlErr(typ, message string, path ...any) map[string]any {
	e := map[string]any{"type": typ, "message": message, "locations": []any{map[string]any{"line": 1, "column": 1}}}
	if len(path) > 0 {
		e["path"] = path
	}
	return e
}

// randomHex returns n random bytes in hex: tokens are made at run time.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// testKey is the App key of the tests, generated once per run.
var testKey = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

// appKeyPEM returns the tests' App key in PEM (PKCS #1, as GitHub gives
// it).
func appKeyPEM(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// provider returns a provider at the server: the GHES layout (flavor ghes,
// host 127.0.0.1:port) unless host names another (github.com, a
// *.ghe.com host), whose API is still the server.
func (s *apiServer) provider(host string) config.ResolvedProvider {
	u, _ := url.Parse(s.srv.URL)
	web := s.srv.URL
	if host == "" {
		host = strings.ToLower(u.Host)
	} else {
		web = "https://" + host
	}
	return config.ResolvedProvider{
		Provider:   config.Provider{ID: "gh", Type: "github", URL: web},
		Host:       host,
		APIURL:     s.api(),
		GraphQLURL: s.srv.URL + "/api/graphql",
		EnvPrefix:  "TOUCHMARK_GH_",
	}
}

// clock is a settable clock for tokens and JWTs.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// start is the tests' clock origin.
var start = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

// fixture is a driver pair over one test server.
type fixture struct {
	*apiServer
	reg    *redact.Registry
	clock  *clock
	key    *rsa.PrivateKey
	token  string // the token credential; "" for an App
	reader *reader
	writer *writer
	// minted counts installation tokens by installation id; tokens holds
	// every token minted, revoked the revoked ones.
	mu      sync.Mutex
	minted  map[int64]int
	tokens  map[string]mintedToken
	revoked []string
}

// mintedToken is what a token minted by the fake App covers.
type mintedToken struct {
	installation int64
	repos        []int64
	perms        map[string]string
}

// appID is the id of the tests' App; its slug is touchmark-write.
const appID = "424242"

// fixtureOpts choose a fixture's credential and host.
type fixtureOpts struct {
	kind credKind
	host string // "" for GHES at the server
}

// newFixture returns a reader and a writer with an App (the default), a
// token or nothing, on a server that knows the App: its JWTs verify, GET
// /app names it, its bot is a user, and installations mint tokens.
func newFixture(t *testing.T, opts ...fixtureOpts) *fixture {
	t.Helper()
	o := fixtureOpts{kind: credApp}
	if len(opts) > 0 {
		o = opts[0]
	}
	s := newAPIServer(t)
	f := &fixture{apiServer: s, reg: redact.New(), clock: &clock{t: start}, minted: map[int64]int{}, tokens: map[string]mintedToken{}}
	var cred auth.Credential
	switch o.kind {
	case credApp:
		key, keyPEM := appKeyPEM(t)
		f.key = key
		cred = auth.Credential{Kind: auth.App, AppID: appID, AppKey: keyPEM}
		f.appRoutes()
	case credToken:
		f.token = "tok_" + randomHex(t, 20)
		cred = auth.Credential{Kind: auth.Token, Token: f.token}
	}
	client := httpx.New(httpx.Options{Redact: f.reg})
	r, err := NewReader(s.provider(o.host), cred, client)
	if err != nil {
		t.Fatal(err)
	}
	f.reader = r.(*reader)
	f.reader.c.now = f.clock.now
	if o.kind != credAnonymous {
		w, err := NewWriter(s.provider(o.host), cred, client)
		if err != nil {
			t.Fatal(err)
		}
		f.writer = w.(*writer)
		f.writer.c.now = f.clock.now
	}
	return f
}

// Installations of the tests' App: acme (an organization, id 7001) and
// alice (a user, id 7002).
const (
	instAcme  int64 = 7001
	instAlice int64 = 7002
)

// appRoutes declares what the App side of the API answers: GET /app, the
// installations of acme and alice, token minting and revocation.
func (f *fixture) appRoutes() {
	f.handle(http.MethodGet, "/app", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 424242, "slug": "touchmark-write", "node_id": "A_kgAAAAAABnkS",
			"name": "touchmark-write", "owner": map[string]any{"login": "acme", "id": 1001, "type": "Organization"}})
	}))
	f.json(http.MethodGet, "/users/touchmark-write[bot]", http.StatusOK, user(5001, "touchmark-write[bot]", "Bot"))
	f.handle(http.MethodGet, "/orgs/acme/installation", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, installation(instAcme, "acme", "Organization", nil))
	}))
	f.handle(http.MethodGet, "/orgs/alice/installation", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, notFoundBody)
	}))
	f.handle(http.MethodGet, "/users/alice/installation", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, installation(instAlice, "alice", "User", nil))
	}))
	insts := []any{installation(instAcme, "acme", "Organization", nil), installation(instAlice, "alice", "User", nil)}
	f.handle(http.MethodGet, "/app/installations", f.asApp(func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, insts, nil)
	}))
	for _, id := range []int64{instAcme, instAlice} {
		f.handle(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), f.asApp(f.mint(id)))
	}
	f.handle(http.MethodDelete, "/installation/token", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		_, known := f.tokens[tok]
		if known {
			f.revoked = append(f.revoked, tok)
			delete(f.tokens, tok)
		}
		f.mu.Unlock()
		if !known {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// installation is an installation as the API sends it; perms default to
// the writer App's permissions.
func installation(id int64, login, typ string, perms map[string]string) map[string]any {
	if perms == nil {
		perms = map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write", "workflows": "write"}
	}
	return map[string]any{"id": id, "app_id": 424242, "app_slug": "touchmark-write", "target_type": typ,
		"account": map[string]any{"login": login, "id": id - 6000, "type": typ}, "permissions": perms,
		"repository_selection": "selected", "events": []any{}, "created_at": "2026-09-01T10:00:00Z",
		"updated_at": "2026-09-01T10:00:00Z", "single_file_name": nil, "suspended_at": nil}
}

// asApp wraps h: the request must carry the App's valid JWT.
func (f *fixture) asApp(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f.verifyJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			f.t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "A JSON web token could not be decoded"})
			return
		}
		h(w, r)
	}
}

// verifyJWT checks a JWT as GitHub does: RS256 over header.claims with the
// App's key, iss the App's id, iat in the past, exp at most 10 minutes
// ahead and not passed (the fixture's clock).
func (f *fixture) verifyJWT(jwt string) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("the credential is no JWT")
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(head) != `{"alg":"RS256","typ":"JWT"}` {
		return fmt.Errorf("JWT header %q", head)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("JWT signature: %w", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var c struct {
		IAT int64 `json:"iat"`
		EXP int64 `json:"exp"`
		ISS any   `json:"iss"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	now := f.clock.now().Unix()
	switch {
	case c.ISS != appID:
		return fmt.Errorf("JWT iss %v, want %q", c.ISS, appID)
	case c.IAT > now:
		return fmt.Errorf("JWT iat %d is in the future (now %d)", c.IAT, now)
	case c.EXP > now+600:
		return fmt.Errorf("JWT exp %d is more than 10 minutes ahead of %d", c.EXP, now)
	case c.EXP <= now:
		return fmt.Errorf("JWT expired at %d (now %d)", c.EXP, now)
	}
	return nil
}

// mint answers POST /app/installations/{id}/access_tokens: a token of an
// hour with the permissions asked for, the repositories asked for.
func (f *fixture) mint(id int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		tok := "ghs_" + randomHex(f.t, 30)
		f.mu.Lock()
		f.minted[id]++
		f.tokens[tok] = mintedToken{installation: id, repos: req.RepositoryIDs, perms: req.Permissions}
		f.mu.Unlock()
		var repos []any
		for _, rid := range req.RepositoryIDs {
			repos = append(repos, map[string]any{"id": rid})
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": f.clock.now().Add(time.Hour).Format(time.RFC3339),
			"permissions": req.Permissions, "repository_selection": "selected", "repositories": repos})
	}
}

// mintedCount returns how many tokens installation id minted.
func (f *fixture) mintedCount(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minted[id]
}

// revokedTokens returns the tokens revoked so far.
func (f *fixture) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.revoked)
}

// tokenOf returns what the token of a request covers; ok is false for a
// token the App did not mint (or one revoked).
func (f *fixture) tokenOf(authz string) (mintedToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.tokens[strings.TrimPrefix(authz, "Bearer ")]
	return m, ok
}

// user is a user or bot as GET /users/{login} sends it (trimmed).
func user(id int64, login, typ string) map[string]any {
	return map[string]any{"login": login, "id": id, "node_id": "U_" + strconv.FormatInt(id, 10), "type": typ,
		"site_admin": false, "user_view_type": "public"}
}

// repoOpt changes a repository fixture.
type repoOpt func(map[string]any)

func withField(k string, v any) repoOpt { return func(m map[string]any) { m[k] = v } }

// repo is a repository as GET /repos/{owner}/{repo} and listings send it
// (trimmed from a recorded answer).
func repo(id int64, fullName string, opts ...repoOpt) map[string]any {
	owner, _, _ := strings.Cut(fullName, "/")
	m := map[string]any{
		"id": id, "node_id": "R_" + strconv.FormatInt(id, 10), "name": strings.SplitN(fullName, "/", 2)[1], "full_name": fullName,
		"private": false, "owner": map[string]any{"login": owner, "id": 1001, "type": "Organization"},
		"html_url": "https://github.com/" + fullName, "fork": false, "size": 42, "default_branch": "main",
		"is_template": false, "topics": []string{}, "has_pull_requests": true, "pull_request_creation_policy": "all",
		"archived": false, "disabled": false, "visibility": "public", "mirror_url": nil,
		"permissions": map[string]any{"admin": false, "maintain": false, "push": false, "triage": false, "pull": true},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// prSpec describes a pull request fixture.
type prSpec struct {
	number      int64
	author      map[string]any
	head        string
	headRepo    map[string]any // defaults to the base repository; nil with deletedFork
	deletedFork bool
	base        string // defaults to "main"
	state       string // "open" (default), "closed", "merged"
	labels      []string
	title       string
	body        *string
	draft       bool
	closedAt    string
}

// pr is a pull request as GET /pulls sends it (trimmed from a recorded
// answer).
func pr(base map[string]any, p prSpec) map[string]any {
	head := p.headRepo
	if head == nil && !p.deletedFork {
		head = base
	}
	headOwner := ""
	if head != nil {
		headOwner, _, _ = strings.Cut(head["full_name"].(string), "/")
	}
	labels := []any{}
	for _, l := range p.labels {
		labels = append(labels, map[string]any{"name": l, "color": "ededed"})
	}
	state, closedAt, mergedAt := "open", any(nil), any(nil)
	switch p.state {
	case "closed":
		state, closedAt = "closed", cmpStr(p.closedAt, "2026-09-10T12:00:00Z")
	case "merged":
		state, closedAt, mergedAt = "closed", cmpStr(p.closedAt, "2026-09-10T12:00:00Z"), cmpStr(p.closedAt, "2026-09-10T12:00:00Z")
	}
	var body any
	if p.body != nil {
		body = *p.body
	}
	fullName := base["full_name"].(string)
	return map[string]any{
		"id": p.number + 9000, "node_id": fmt.Sprintf("PR_%d", p.number), "number": p.number,
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", fullName, p.number),
		"state":    state, "draft": p.draft, "title": cmpStr(p.title, "sync"), "body": body, "user": p.author,
		"labels": labels, "created_at": "2026-09-05T08:00:00Z", "updated_at": "2026-09-10T12:00:00Z",
		"closed_at": closedAt, "merged_at": mergedAt,
		"head": map[string]any{"label": headOwner + ":" + p.head, "ref": p.head,
			"sha": strings.Repeat("a", 39) + strconv.FormatInt(p.number%10, 10), "repo": head},
		"base": map[string]any{"label": strings.Split(fullName, "/")[0] + ":" + cmpStr(p.base, "main"), "ref": cmpStr(p.base, "main"),
			"sha": strings.Repeat("b", 40), "repo": base},
	}
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ptr[T any](v T) *T { return &v }

// Accounts of the tests.
var (
	alice   = user(3001, "alice", "User")
	bob     = user(3002, "bob", "User")
	botUser = user(5001, "touchmark-write[bot]", "Bot")
)

// wantClass checks the class of err (and, with sentinel, that it wraps it).
func wantClass(t *testing.T, what string, err error, class platform.Class, sentinel error) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error, want class %v", what, class)
	case platform.ClassOf(err) != class:
		t.Errorf("%s: class %v of %v, want %v", what, platform.ClassOf(err), err, class)
	case sentinel != nil && !errors.Is(err, sentinel):
		t.Errorf("%s: %v does not wrap %v", what, err, sentinel)
	}
}

// ruleOfErr returns the Rule of err's *platform.Error.
func ruleOfErr(err error) string {
	var pe *platform.Error
	if errors.As(err, &pe) {
		return pe.Rule
	}
	return ""
}

// gitBlobID is git's id of a blob (SHA-1).
func gitBlobID(content string) string { return blobID([]byte(content), 40) }

// b64 is the base64 the blob API sends, broken in lines of 60 as GitHub
// does.
func b64(content string) string {
	s := base64.StdEncoding.EncodeToString([]byte(content))
	var b strings.Builder
	for len(s) > 60 {
		b.WriteString(s[:60] + "\n")
		s = s[60:]
	}
	b.WriteString(s)
	return b.String()
}
