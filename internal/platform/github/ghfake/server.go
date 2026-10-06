package ghfake

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Flavor is the product the fake imitates.
type Flavor string

// Flavors.
const (
	// DotCom is github.com (and GHE.com, which serves the same API).
	DotCom Flavor = "github.com"
	// GHES is GitHub Enterprise Server: no /hash-algorithm, API versions
	// 2022-11-28 only, rate limits off by default, API commits unsigned
	// unless web commit signing is on, short installation tokens.
	GHES Flavor = "ghes"
)

// defaultGHESVersion is the GHES version the GHES flavor reports by
// default: the oldest release touchmark supports.
const defaultGHESVersion = "3.19.0"

// API versions (X-GitHub-Api-Version).
const (
	apiVersion2022 = "2022-11-28"
	apiVersion2026 = "2026-03-10"
)

// Options configure a Server.
type Options struct {
	// Dir holds the bare repositories. It must be empty or absent; ""
	// makes a temporary directory that Close removes.
	Dir string
	// Flavor is DotCom when empty.
	Flavor Flavor
	// Now is the clock of tokens, JWTs, limits and timestamps; time.Now
	// when nil. Tests that expire tokens pass a controllable clock.
	Now func() time.Time
	// Limits are the rate limits; nil means the documented defaults on
	// DotCom and no limits on GHES (see Limits).
	Limits *Limits
	// MaxTreeEntries truncates recursive tree listings above this many
	// entries (GitHub: 100 000); 0 means 100 000.
	MaxTreeEntries int
	// WebCommitSigning makes API commits of Apps signed. nil means true on
	// DotCom and false on GHES, where a site admin turns it on.
	WebCommitSigning *bool
	// Version is the version of GitHub Enterprise Server the GHES flavor
	// reports (GET /meta installed_version and the
	// X-GitHub-Enterprise-Version header of every REST answer); "3.19.0"
	// when empty. DotCom reports none.
	Version string
	// ShortTokens mints 40-character installation tokens ("ghs_" and 36
	// characters) on DotCom too. By default DotCom mints the stateless
	// format, "ghs_" followed by a JWT-shaped string of about 520
	// characters, which github.com has rolled out since 2026-04-27.
	ShortTokens bool
}

// Server is a running fake GitHub. It is safe for concurrent use.
type Server struct {
	opts   Options
	flavor Flavor
	now    func() time.Time
	http   *httptest.Server
	dir    string
	ownDir bool
	git    *gitEnv
	secret []byte // the key of the fake web-flow signature

	// gitMu serializes every ref change: pushes, API ref updates and the
	// setup methods. It is taken before mu, never while holding it.
	gitMu sync.Mutex

	mu        sync.Mutex
	nextID    int64
	accounts  map[int64]*account
	byLogin   map[string]*account // lowercased login
	apps      map[int64]*app
	installs  map[int64]*installation
	tokens    map[string]*token
	repos     map[int64]*repo
	byPath    map[string]*repo // lowercased "owner/name"
	redirects map[string]int64 // lowercased old "owner/name" → repository id
	// apiGone holds the redirects of renamed owners: their old paths answer
	// 404 in the API and redirect only in git (docs: renaming an
	// organization or a user).
	apiGone     map[string]bool
	orgRulesets map[int64][]*ruleset
	// orgSecrets are organization secrets by org id: name → the ids of
	// the repositories they are shared with, nil for all (settings.go).
	orgSecrets map[int64]map[string][]int64
	// manifests are the App manifests waiting for their code's conversion.
	manifests  map[string]*pendingManifest
	faults     map[string][]*Fault
	requests   []Request
	limits     *limiter
	known      map[int64]map[int64]bool // pusher id → author ids it owns
	violations []string
	pending    []pendingPush
	closed     bool
}

// New starts a fake GitHub on 127.0.0.1. REST is served under / (the
// github.com layout, where the API has its own host) and under /api/v3
// (GitHub Enterprise Server); GraphQL at /graphql and /api/graphql; git
// smart HTTP at /<owner>/<repo>.git.
func New(opts Options) (*Server, error) {
	if opts.Flavor == "" {
		opts.Flavor = DotCom
	}
	if opts.Flavor != DotCom && opts.Flavor != GHES {
		return nil, fmt.Errorf("ghfake: unknown flavor %q", opts.Flavor)
	}
	if opts.Flavor == GHES && opts.Version == "" {
		opts.Version = defaultGHESVersion
	}
	if opts.Flavor == DotCom {
		opts.Version = ""
	}
	s := &Server{
		opts:        opts,
		flavor:      opts.Flavor,
		now:         opts.Now,
		accounts:    map[int64]*account{},
		byLogin:     map[string]*account{},
		apps:        map[int64]*app{},
		installs:    map[int64]*installation{},
		tokens:      map[string]*token{},
		repos:       map[int64]*repo{},
		byPath:      map[string]*repo{},
		redirects:   map[string]int64{},
		apiGone:     map[string]bool{},
		orgRulesets: map[int64][]*ruleset{},
		faults:      map[string][]*Fault{},
		known:       map[int64]map[int64]bool{},
		nextID:      1000,
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.limits = newLimiter(opts.Limits, opts.Flavor)
	s.secret = make([]byte, 32)
	if _, err := rand.Read(s.secret); err != nil {
		return nil, fmt.Errorf("ghfake: %w", err)
	}
	dir := opts.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "ghfake-")
		if err != nil {
			return nil, fmt.Errorf("ghfake: %w", err)
		}
		dir, s.ownDir = d, true
	}
	g, err := newGitEnv(dir)
	if err != nil {
		if s.ownDir {
			_ = os.RemoveAll(dir)
		}
		return nil, fmt.Errorf("ghfake: %w", err)
	}
	s.dir, s.git = dir, g
	s.http = httptest.NewServer(s)
	return s, nil
}

// Close stops the server and removes a temporary directory it made. It
// may be called more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	done := s.closed
	s.closed = true
	s.mu.Unlock()
	if done {
		return nil
	}
	s.http.Close()
	if s.ownDir {
		return os.RemoveAll(s.dir)
	}
	return nil
}

// URL is the server's base URL, "http://127.0.0.1:<port>": the web host
// of clones and html_url, and the REST base of the github.com layout.
func (s *Server) URL() string { return s.http.URL }

// APIURL is the REST base of the github.com layout (the same as URL: the
// fake serves github.com's two hosts on one).
func (s *Server) APIURL() string { return s.http.URL }

// GraphQLURL is the GraphQL endpoint of the github.com layout.
func (s *Server) GraphQLURL() string { return s.http.URL + "/graphql" }

// EnterpriseAPIURL is the REST base of the GitHub Enterprise Server
// layout, <URL>/api/v3; its GraphQL endpoint is <URL>/api/graphql.
func (s *Server) EnterpriseAPIURL() string { return s.http.URL + "/api/v3" }

// CloneURL returns the git URL of a repository, <URL>/<owner>/<repo>.git.
func (s *Server) CloneURL(path string) string { return s.http.URL + "/" + escapePath(path) + ".git" }

// Client returns an HTTP client for the server.
func (s *Server) Client() *http.Client { return s.http.Client() }

// id returns a new database id. Called with mu held.
func (s *Server) id() int64 {
	s.nextID++
	return s.nextID
}

// Request is one request the server answered.
type Request struct {
	Method string
	// Route is the route template ("GET /repos/{owner}/{repo}"), "POST
	// /graphql <fields>" with the root fields of the operation, "GIT
	// fetch <path>" or "GIT push <path>".
	Route string
	// Identity is who sent it: "anonymous", "app/<slug>" (a JWT),
	// "installation/<id>", "user/<login>" (a personal access token).
	Identity string
	Status   int
}

// Requests returns every request the server answered, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// logRequest records a request. Called with mu held.
func (s *Server) logRequest(method, route, who string, status int) {
	s.requests = append(s.requests, Request{Method: method, Route: route, Identity: who, Status: status})
}

// Fault is an injected failure of the next requests to a route.
type Fault struct {
	// Status is the HTTP status of the failure (500 when 0). A 403 or 429
	// is sent with Retry-After when RetryAfter is set.
	Status int
	// Message is the "message" of the JSON body; a generic one when empty.
	Message    string
	RetryAfter time.Duration
	// GraphQL answers a GraphQL request with HTTP 200 and this error type
	// ("RATE_LIMITED", "INTERNAL", …) instead of Status.
	GraphQL string
	// Applied lets the request take effect before it fails, as when the
	// response is lost on the way.
	Applied bool
	// Times is how many requests fail; 1 when 0.
	Times int
}

// Fail makes the next requests to route fail. route is a Request.Route
// template ("POST /repos/{owner}/{repo}/pulls"), "POST /graphql" for every
// GraphQL request or "GRAPHQL <root field>" ("GRAPHQL updateRefs") for
// operations selecting that field, "GIT fetch" or "GIT push" for git, or
// "*" for any request.
func (s *Server) Fail(route string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Times <= 0 {
		f.Times = 1
	}
	s.faults[route] = append(s.faults[route], &f)
}

// takeFault returns the fault queued for one of keys, if any. Called with
// mu held.
func (s *Server) takeFault(keys ...string) (*Fault, bool) {
	for _, k := range append(keys, "*") {
		q := s.faults[k]
		if len(q) == 0 {
			continue
		}
		f := q[0]
		f.Times--
		if f.Times <= 0 {
			s.faults[k] = q[1:]
		}
		return f, true
	}
	return nil, false
}

// faultResponse is the REST response of a fault.
func faultResponse(f *Fault) response {
	status := f.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	msg := f.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	resp := apiError(status, msg)
	if f.RetryAfter > 0 {
		resp.header = http.Header{"Retry-After": {strconv.Itoa(int(f.RetryAfter / time.Second))}}
	}
	return resp
}

// response is what a REST handler answers.
type response struct {
	status int
	body   any // JSON-encoded; nil sends no body (204)
	raw    []byte
	ctype  string // with raw
	header http.Header
}

// docURL is the documentation_url of errors.
const docURL = "https://docs.github.com/rest"

// apiError is GitHub's error body: {"message", "documentation_url",
// "status"} (observed read-only 2026-09-29: the status as a string).
func apiError(status int, msg string) response {
	return response{status: status, body: map[string]any{
		"message": msg, "documentation_url": docURL, "status": strconv.Itoa(status)}}
}

// validation is a 422 "Validation Failed" with errors.
func validation(errs ...map[string]any) response {
	list := make([]any, len(errs))
	for i, e := range errs {
		list[i] = e
	}
	return response{status: http.StatusUnprocessableEntity, body: map[string]any{
		"message": "Validation Failed", "errors": list, "documentation_url": docURL, "status": "422"}}
}

// customError is one "custom" validation error with a message.
func customError(resource, field, msg string) map[string]any {
	e := map[string]any{"resource": resource, "code": "custom", "message": msg}
	if field != "" {
		e["field"] = field
	}
	return e
}

// unprocessable is a 422 with a plain message.
func unprocessable(msg string) response { return apiError(http.StatusUnprocessableEntity, msg) }

// notFound is GitHub's 404.
func notFound() response { return apiError(http.StatusNotFound, "Not Found") }

// ok answers 200 with body.
func ok(body any) response { return response{status: http.StatusOK, body: body} }

// created answers 201 with body.
func created(body any) response { return response{status: http.StatusCreated, body: body} }

// noContent answers 204.
func noContent() response { return response{status: http.StatusNoContent} }

// ServeHTTP dispatches a request: git, GraphQL or REST.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	if repoPath, svc, ok := splitGitPath(p); ok {
		s.serveGit(w, r, repoPath, svc)
		return
	}
	switch p {
	case "/graphql", "/api/graphql":
		s.serveGraphQL(w, r)
		return
	}
	base := s.http.URL
	if rest, ok := strings.CutPrefix(p, "/api/v3"); ok && (rest == "" || rest[0] == '/') {
		base += "/api/v3"
		p = rest
		if p == "" {
			p = "/"
		}
	}
	s.serveREST(w, r, base, p)
}

// call is one REST request in a handler.
type call struct {
	s      *Server
	r      *http.Request
	id     *identity
	params map[string]string
	route  string
	base   string // the REST base: URL or URL/api/v3
	path   string // the escaped path after base
	body   []byte
	query  url.Values
}

// param returns a path parameter.
func (c *call) param(name string) string { return c.params[name] }

// decode unmarshals the JSON body into v; an invalid body is a 400.
func (c *call) decode(v any) (response, bool) {
	if len(bytes.TrimSpace(c.body)) == 0 {
		return response{}, true
	}
	if err := json.Unmarshal(c.body, v); err != nil {
		return apiError(http.StatusBadRequest, "Problems parsing JSON"), false
	}
	return response{}, true
}

// maxRequestBody bounds a REST or GraphQL request body.
const maxRequestBody = 32 << 20

// serveREST answers a REST request at path p (without the /api/v3 prefix).
func (s *Server) serveREST(w http.ResponseWriter, r *http.Request, base, p string) {
	rt, params, ok := matchRoute(r.Method, p)
	who := "anonymous"
	finish := func(route string, resp response) {
		s.mu.Lock()
		s.logRequest(r.Method, route, who, resp.status)
		s.mu.Unlock()
		if s.opts.Version != "" {
			// Every answer of GHES names its version (docs: the REST API
			// on GitHub Enterprise Server).
			resp.header = mergeHeader(resp.header, http.Header{"X-Github-Enterprise-Version": {s.opts.Version}})
		}
		writeResponse(w, resp)
	}
	if !ok {
		finish(r.Method+" "+p, notFound())
		return
	}
	route := rt.key()
	version := r.Header.Get("X-GitHub-Api-Version")
	if version == "" {
		version = apiVersion2022
	}
	if !s.supportsVersion(version) {
		finish(route, apiError(http.StatusBadRequest,
			fmt.Sprintf("API version %s is not supported.", version)))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		finish(route, apiError(http.StatusBadRequest, "Problems parsing JSON"))
		return
	}
	id, resp, authOK := s.authenticate(r)
	if id != nil {
		who = id.key()
	}
	if !authOK {
		finish(route, resp)
		return
	}
	c := &call{s: s, r: r, id: id, params: params, route: route, base: base, path: p, body: body, query: r.URL.Query()}
	s.mu.Lock()
	limited, lh := s.limits.admit(s.now(), id, restPoints(r.Method), rt.pattern == "/rate_limit", "core")
	fault, faulted := s.takeFault(route)
	s.mu.Unlock()
	if limited != nil {
		*limited = withHeader(*limited, lh)
		finish(route, *limited)
		return
	}
	if faulted && !fault.Applied {
		finish(route, withHeader(faultResponse(fault), lh))
		return
	}
	resp = rt.h(c)
	if faulted {
		resp = faultResponse(fault)
	}
	resp.header = mergeHeader(resp.header, lh)
	resp.header = mergeHeader(resp.header, http.Header{"X-Github-Api-Version-Selected": {version}})
	finish(route, resp)
}

// supportsVersion reports whether the flavor serves an API version.
func (s *Server) supportsVersion(v string) bool {
	switch v {
	case apiVersion2022:
		return true
	case apiVersion2026:
		return s.flavor == DotCom
	}
	return false
}

// withHeader returns resp with h added.
func withHeader(resp response, h http.Header) response {
	resp.header = mergeHeader(resp.header, h)
	return resp
}

// mergeHeader returns a with the entries of b it lacks.
func mergeHeader(a, b http.Header) http.Header {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = http.Header{}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			a[k] = slices.Clone(v)
		}
	}
	return a
}

// writeResponse sends resp.
func writeResponse(w http.ResponseWriter, resp response) {
	maps.Copy(w.Header(), resp.header)
	switch {
	case resp.raw != nil:
		w.Header().Set("Content-Type", resp.ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(resp.raw)))
		w.WriteHeader(resp.status)
		_, _ = w.Write(resp.raw)
	case resp.body == nil:
		w.WriteHeader(resp.status)
	default:
		data, err := json.Marshal(resp.body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.status)
		_, _ = w.Write(data)
	}
}

// route is one REST route.
type route struct {
	method  string
	pattern string
	// canonical is the /repos template of a /repositories alias.
	canonical string
	segs      []string
	h         func(*call) response
}

// key is the template a request is logged and faulted under.
func (rt route) key() string {
	if rt.canonical != "" {
		return rt.method + " " + rt.canonical
	}
	return rt.method + " " + rt.pattern
}

// routes is the REST route table, built once.
var (
	routesOnce sync.Once
	routeTable []route
)

// matchRoute finds the route of method and escaped path p and its
// parameters. A parameter "{name...}" takes the rest of the path.
func matchRoute(method, p string) (route, map[string]string, bool) {
	routesOnce.Do(func() { routeTable = buildRoutes() })
	raw := strings.Split(strings.TrimPrefix(p, "/"), "/")
	segs := make([]string, len(raw))
	for i, seg := range raw {
		u, err := url.PathUnescape(seg)
		if err != nil {
			return route{}, nil, false
		}
		segs[i] = u
	}
	for _, rt := range routeTable {
		if rt.method != method && (method != http.MethodHead || rt.method != http.MethodGet) {
			continue
		}
		if params, ok := matchSegs(rt.segs, segs); ok {
			return rt, params, true
		}
	}
	return route{}, nil, false
}

// matchSegs matches path segments against pattern segments.
func matchSegs(pattern, segs []string) (map[string]string, bool) {
	params := map[string]string{}
	for i, ps := range pattern {
		if name, ok := strings.CutSuffix(strings.TrimPrefix(ps, "{"), "...}"); ok && strings.HasPrefix(ps, "{") {
			if i > len(segs) {
				return nil, false
			}
			params[name] = strings.Join(segs[i:], "/")
			return params, true
		}
		if i >= len(segs) {
			return nil, false
		}
		if strings.HasPrefix(ps, "{") && strings.HasSuffix(ps, "}") {
			if segs[i] == "" {
				return nil, false
			}
			params[ps[1:len(ps)-1]] = segs[i]
			continue
		}
		if ps != segs[i] {
			return nil, false
		}
	}
	return params, len(pattern) == len(segs)
}

// newRoute builds a route.
func newRoute(method, pattern string, h func(*call) response) route {
	return route{method: method, pattern: pattern, segs: strings.Split(strings.TrimPrefix(pattern, "/"), "/"), h: h}
}

// escapePath escapes each segment of an "owner/name" path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// errSetup marks misuse of a setup method.
var errSetup = errors.New("ghfake")

// setupErr returns a setup error.
func setupErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSetup, fmt.Sprintf(format, args...))
}
