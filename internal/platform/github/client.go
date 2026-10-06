package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// basicUser is the user name of git's Basic credentials: GitHub takes
// "x-access-token" with an installation token, and a personal access token
// as the password whatever the name (unverified for fine-grained tokens:
// the sandbox checks it). The CLI and the driver register the Basic form
// of this user for masking.
const basicUser = "x-access-token"

// REST headers (https://docs.github.com/rest/about-the-rest-api/api-versions):
// 2022-11-28 is the only version GHES 3.18 to 3.20 serve, and github.com
// serves it until 2028-03-10.
const (
	mediaType  = "application/vnd.github+json"
	apiVersion = "2022-11-28"
)

// pageSize is per_page of every REST listing: GitHub's maximum.
const pageSize = 100

// maxRedirects bounds the redirects one GET follows: GitHub answers the
// old path of a renamed or transferred repository with 301 to
// /repositories/<id>/….
const maxRedirects = 3

// Flavors (platform.Caps.Flavor).
const (
	flavorGitHub = "github"
	flavorGHE    = "ghe.com"
	flavorGHES   = "ghes"
)

// credKind tells the credential a client acts with.
type credKind uint8

const (
	credAnonymous credKind = iota
	credToken
	credApp
)

// client is the HTTP side of one driver: one provider under one identity.
// It is safe for concurrent use.
type client struct {
	http *httpx.Client
	// api is the REST base without a trailing slash; apiURL is it parsed:
	// redirects are followed only below it. graphqlURL is the GraphQL
	// endpoint.
	api        string
	apiURL     *url.URL
	graphqlURL string
	// web is the provider's web URL without a trailing slash; git remotes
	// are under it. host is the provider host (Repo.Host), flavor what it
	// is.
	web, host, flavor string
	// authHosts are the hosts credentials go to: the REST and the GraphQL
	// hosts.
	authHosts []string

	kind  credKind
	token string // a token credential, "" otherwise
	app   *app   // a GitHub App, nil otherwise
	// masks holds every secret of the driver for its own messages; run is
	// the run's registry (nil when the client has none).
	masks *redact.Registry
	run   *redact.Registry
	// now is the clock of tokens, JWTs and waits (tests replace it).
	now func() time.Time

	mu      sync.Mutex
	self    *platform.Account
	inst    *instance
	formats map[int64]string // object format by repository id
	noHash  bool             // GET /hash-algorithm answered 404 on GHES
	closers map[string]*closer
	// splitStage is set once updateRefs refused to delete a stage ref
	// together with the branch update (Commit).
	splitStage bool
}

// newClient checks the provider and the credential and builds the client.
func newClient(p config.ResolvedProvider, cred auth.Credential, hc *httpx.Client) (*client, error) {
	if p.Type != "github" {
		return nil, fmt.Errorf("github: provider %s has type %q, not github", p.ID, p.Type)
	}
	if hc == nil {
		return nil, errors.New("github: no HTTP client")
	}
	api, apiURL, err := baseURL("api_url", p.APIURL)
	if err != nil {
		return nil, fmt.Errorf("github: provider %s: %w", p.ID, err)
	}
	web, webURL, err := baseURL("url", p.URL)
	if err != nil {
		return nil, fmt.Errorf("github: provider %s: %w", p.ID, err)
	}
	gql := p.GraphQLURL
	if gql == "" {
		gql = derivedGraphQL(api)
	}
	gql, gqlURL, err := baseURL("the GraphQL URL", gql)
	if err != nil {
		return nil, fmt.Errorf("github: provider %s: %w", p.ID, err)
	}
	host := strings.ToLower(p.Host)
	if host == "" {
		host = strings.ToLower(webURL.Host)
	}
	c := &client{
		http:       hc,
		api:        api,
		apiURL:     apiURL,
		graphqlURL: gql,
		web:        web,
		host:       host,
		flavor:     flavorOf(host),
		masks:      redact.New(),
		run:        hc.Registry(),
		now:        time.Now,
		formats:    map[int64]string{},
		closers:    map[string]*closer{},
	}
	for _, h := range []string{strings.ToLower(apiURL.Host), strings.ToLower(gqlURL.Host)} {
		if !slices.Contains(c.authHosts, h) {
			c.authHosts = append(c.authHosts, h)
		}
	}
	for _, s := range cred.Secrets() {
		c.masks.Add(s, basicUser)
	}
	switch cred.Kind {
	case 0:
		if cred.Token != "" || cred.AppID != "" || len(cred.AppKey) > 0 {
			return nil, errors.New("github: a credential without a kind")
		}
	case auth.Token:
		if cred.Token == "" {
			return nil, errors.New("github: an empty token")
		}
		c.kind, c.token = credToken, cred.Token
		c.register(cred.Token)
	case auth.App:
		a, err := newApp(c, cred.AppID, cred.AppKey)
		if err != nil {
			return nil, fmt.Errorf("github: provider %s: %w", p.ID, err)
		}
		c.kind, c.app = credApp, a
	default:
		return nil, fmt.Errorf("github: provider %s: unknown credential kind %d", p.ID, cred.Kind)
	}
	return c, nil
}

// baseURL checks an http(s) base URL without credentials, query or
// fragment, and returns it without a trailing slash.
func baseURL(what, raw string) (string, *url.URL, error) {
	s := strings.TrimRight(raw, "/")
	u, err := url.Parse(s)
	switch {
	case s == "":
		return "", nil, fmt.Errorf("%s is empty", what)
	case err != nil:
		return "", nil, fmt.Errorf("%s does not parse", what)
	case u.Scheme != "https" && u.Scheme != "http":
		return "", nil, fmt.Errorf("%s is not an http or https URL", what)
	case u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return "", nil, fmt.Errorf("%s must be a URL with a host and without credentials, query or fragment", what)
	}
	return s, u, nil
}

// derivedGraphQL returns the GraphQL endpoint next to a REST base:
// …/api/v3 → …/api/graphql (GHES), else <api>/graphql (api.github.com,
// api.<sub>.ghe.com), as config does.
func derivedGraphQL(api string) string {
	if root, ok := strings.CutSuffix(api, "/api/v3"); ok {
		return root + "/api/graphql"
	}
	return api + "/graphql"
}

// flavorOf tells github.com, GHE.com (a *.ghe.com host) and GitHub
// Enterprise Server (any other host) apart.
func flavorOf(host string) string {
	h := host
	if name, _, err := net.SplitHostPort(host); err == nil {
		h = name
	}
	h = strings.ToLower(h)
	switch {
	case h == "github.com":
		return flavorGitHub
	case strings.HasSuffix(h, ".ghe.com"):
		return flavorGHE
	}
	return flavorGHES
}

// register adds a secret the driver holds or minted to its masks and to
// the run's registry, with the Basic form git sends.
func (c *client) register(secret string) {
	c.masks.Add(secret, basicUser)
	c.run.Add(secret, basicUser)
}

// mask replaces the driver's secrets in s.
func (c *client) mask(s string) string { return c.masks.Replace(c.run.Replace(s)) }

// staticAuth returns the credential header value as an httpx.Auth for the
// API and GraphQL hosts.
func (c *client) staticAuth(value string) *httpx.Auth {
	return &httpx.Auth{Hosts: c.authHosts, Header: func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return value, nil
	}}
}

// basic is git's Basic header for token.
func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(basicUser+":"+token))
}

// endpoint joins the API base and path segments, each escaped.
func (c *client) endpoint(segments ...string) string {
	var b strings.Builder
	b.WriteString(c.api)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// repoURL is the endpoint of repository owner/name with more segments.
func (c *client) repoURL(owner, name string, more ...string) string {
	return c.endpoint(append([]string{"repos", owner, name}, more...)...)
}

// restHeader are the headers of every REST request.
var restHeader = http.Header{"Accept": {mediaType}, "X-GitHub-Api-Version": {apiVersion}}

// call sends one REST request of op with auth (nil: anonymous) and decodes
// a 2xx JSON response into out (nil skips decoding). Every error is a
// masked *platform.Error, but a canceled context's.
//
// A GET follows a redirect below the API base on the same origin, at most
// maxRedirects times, with the same credential: GitHub answers the old
// path of a renamed repository with 301 to /repositories/<id> (checked on
// api.github.com). httpx follows no redirect of an authenticated request
// itself, and a write is never repeated elsewhere: its redirect stays an
// error.
func (c *client) call(ctx context.Context, op, method, u string, query url.Values, a *httpx.Auth, in, out any) (*httpx.Response, error) {
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for hops := 0; ; hops++ {
		resp, err := c.http.JSONWith(ctx, method, u, a, restHeader, in, out)
		if err == nil {
			return resp, nil
		}
		if method == http.MethodGet && hops < maxRedirects {
			if next, ok := c.redirect(u, err); ok {
				u = next
				continue
			}
		}
		return resp, c.apiError(op, err)
	}
}

// get is call with GET.
func (c *client) get(ctx context.Context, op, u string, query url.Values, a *httpx.Auth, out any) (*httpx.Response, error) {
	return c.call(ctx, op, http.MethodGet, u, query, a, nil, out)
}

// redirect returns where the redirect err answered to a request for from
// leads, when it is one to follow: a 301, 302, 303, 307 or 308 whose
// Location, resolved against from, is on the same scheme, host and port,
// without credentials, and below the API base.
func (c *client) redirect(from string, err error) (string, bool) {
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return "", false
	}
	switch se.Status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", false
	}
	loc := strings.TrimSpace(se.Header.Get("Location"))
	base, perr := url.Parse(from)
	if loc == "" || perr != nil {
		return "", false
	}
	to, perr := base.Parse(loc)
	if perr != nil || !c.below(to) {
		return "", false
	}
	to.Fragment, to.RawFragment = "", ""
	return to.String(), true
}

// below reports whether u is on the API's origin, without credentials,
// below the API base.
func (c *client) below(u *url.URL) bool {
	if u.User != nil || !sameOrigin(u, c.apiURL) {
		return false
	}
	root := strings.TrimRight(c.apiURL.Path, "/") + "/"
	return strings.HasPrefix(u.Path, root) && !strings.Contains(u.Path, "/../")
}

// sameOrigin reports whether a and b have the same scheme, host and port
// (the scheme's default when absent), ignoring case.
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// listPages reads a paginated REST listing at u with query and auth and
// hands each page's body to page, which decodes it and returns how many
// items it held. complete is false when the listing was capped at
// maxPages. An error of page n > 1 comes with the page in err (see
// pageError), so a caller can tell a failed listing from a failed first
// request.
//
// The next page is the one the Link header's rel="next" names: only its
// query is taken, the path and origin stay u's (GitHub's next link of
// /orgs/{org}/repos names /organizations/<id>/repos). A page without a
// rel="next" or without items is the last.
func listPages(ctx context.Context, c *client, op, u string, query url.Values, a *httpx.Auth, maxPages int,
	page func(body []byte) (int, error)) (complete bool, err error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(pageSize))
	for n := 1; n <= maxPages; n++ {
		resp, err := c.get(ctx, op, u, q, a, nil)
		if err != nil {
			if n > 1 {
				return false, &pageError{page: n, err: err}
			}
			return false, err
		}
		items, err := page(resp.Body)
		if err != nil {
			return false, err
		}
		next, ok := nextQuery(resp.Header)
		if items == 0 || !ok {
			return true, nil
		}
		q = next
		if q.Get("per_page") == "" {
			q.Set("per_page", strconv.Itoa(pageSize))
		}
	}
	return false, nil
}

// listAll is listPages over a JSON array of T, calling each for every item.
func listAll[T any](ctx context.Context, c *client, op, u string, query url.Values, a *httpx.Auth, maxPages int, each func(T) error) (bool, error) {
	return listPages(ctx, c, op, u, query, a, maxPages, func(body []byte) (int, error) {
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return 0, shapeError(op, "a page that is not a list: %v", err)
		}
		for _, it := range items {
			if err := each(it); err != nil {
				return 0, err
			}
		}
		return len(items), nil
	})
}

// nextQuery returns the query of the Link header's rel="next"; ok is false
// without one.
func nextQuery(h http.Header) (url.Values, bool) {
	for _, v := range h.Values("Link") {
		for part := range strings.SplitSeq(v, ",") {
			target, params, found := strings.Cut(part, ";")
			if !found {
				continue
			}
			for p := range strings.SplitSeq(params, ";") {
				name, value, _ := strings.Cut(strings.TrimSpace(p), "=")
				if !strings.EqualFold(strings.TrimSpace(name), "rel") {
					continue
				}
				for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(value), `"`)) {
					if !strings.EqualFold(rel, "next") {
						continue
					}
					u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
					if err != nil {
						return nil, false
					}
					q, err := url.ParseQuery(u.RawQuery)
					if err != nil || len(q) == 0 {
						return nil, false
					}
					return q, true
				}
			}
		}
	}
	return nil, false
}

// pageError is the failure of a page after the first of a listing.
type pageError struct {
	page int
	err  error
}

func (e *pageError) Error() string { return fmt.Sprintf("page %d: %v", e.page, e.err) }
func (e *pageError) Unwrap() error { return e.err }

// laterPage reports whether err is the failure of a page after the first.
func laterPage(err error) bool {
	var pe *pageError
	return errors.As(err, &pe)
}

// splitRepoPath splits "owner/name". GitHub has no nested namespaces: a
// path of another shape cannot name a repository (ok false).
func splitRepoPath(p string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(p, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") ||
		strings.ContainsAny(p, "\x00?#\\") || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", false
	}
	return owner, name, true
}

// repoPath returns the owner and name of r, or a ClassInvalid error for op.
func repoPath(op string, r platform.Repo) (owner, name string, err error) {
	owner, name, ok := splitRepoPath(r.Path)
	if !ok {
		return "", "", invalid(op, "%q is not an owner/name repository path", r.Path)
	}
	return owner, name, nil
}

// repoID returns the numeric id of r, or a ClassInvalid error for op.
func repoID(op string, r platform.Repo) (int64, error) {
	id, err := strconv.ParseInt(r.ID, 10, 64)
	if err != nil || id <= 0 {
		return 0, invalid(op, "repository %s has no numeric id (%q)", r.Path, r.ID)
	}
	return id, nil
}

// checkHost refuses a repository of another provider.
func (c *client) checkHost(op string, r platform.Repo) error {
	if r.Host != "" && !strings.EqualFold(r.Host, c.host) {
		return invalid(op, "repository %s is on %s, not %s", r.Path, r.Host, c.host)
	}
	return nil
}

// ownerAuth returns the credential for requests about owner's
// repositories: the token as is, the installation token of owner for an
// App, nil when anonymous.
func (c *client) ownerAuth(ctx context.Context, owner string) (*httpx.Auth, error) {
	switch c.kind {
	case credToken:
		return c.staticAuth("Bearer " + c.token), nil
	case credApp:
		tok, err := c.app.ownerToken(ctx, owner)
		if err != nil {
			return nil, err
		}
		return c.staticAuth("Bearer " + tok), nil
	}
	return nil, nil
}

// anyAuth returns a credential for requests about no repository (a user,
// the instance): the token, a token of any installation of an App that is
// not suspended, nil when anonymous. An App without such an installation
// asks anonymously (a public account is readable without credentials on
// github.com and GHE.com; a GitHub Enterprise Server in private mode
// refuses, and the error says so).
func (c *client) anyAuth(ctx context.Context) (*httpx.Auth, error) {
	switch c.kind {
	case credToken:
		return c.staticAuth("Bearer " + c.token), nil
	case credApp:
		tok, err := c.app.anyToken(ctx)
		switch {
		case platform.ClassOf(err) == platform.ClassNotFound:
			return nil, nil
		case err != nil:
			return nil, err
		}
		return c.staticAuth("Bearer " + tok), nil
	}
	return nil, nil
}

// gitHeader returns the Basic header of git for owner's repositories: the
// current token (renewed when due), "" when anonymous.
func (c *client) gitHeader(ctx context.Context, owner string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch c.kind {
	case credToken:
		return basic(c.token), nil
	case credApp:
		tok, err := c.app.ownerToken(ctx, owner)
		if err != nil {
			return "", err
		}
		return basic(tok), nil
	}
	return "", nil
}

// itoa formats a number for a path segment.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
