package bitbucket

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// basicUser is the user name of git's Basic credentials for an API token
// of an account (support.atlassian.com/bitbucket-cloud, "Using API tokens":
// the git user of an API token is x-bitbucket-api-token-auth, or the
// account's username, which touchmark does not know). It is one of the
// users whose Basic form the CLI registers for masking.
const basicUser = "x-bitbucket-api-token-auth"

// Page lengths asked for: the API's global bounds are 10 and 100 (REST
// intro, "Pagination"); the pull request listings are taken to stop at 50
// (an assumption to confirm live; a smaller page only costs pages).
const (
	repoPageLen = 100
	prPageLen   = 50
)

// maxRedirects bounds the redirects one GET follows.
const maxRedirects = 3

// snippetLimit bounds the body kept from a failed raw request, in bytes, as
// httpx keeps for JSON requests.
const snippetLimit = 1 << 10

// client is the HTTP side of one driver: one provider under one identity.
// It is safe for concurrent use.
type client struct {
	http *httpx.Client
	// api is the REST base without a trailing slash
	// (https://api.bitbucket.org/2.0); apiURL is api parsed: redirects
	// and pagination links are followed only below it.
	api    string
	apiURL *url.URL
	// web is the provider's web URL without a trailing slash; git remotes
	// are under it. host is the provider host (Repo.Host).
	web, host string
	token     string // "" when anonymous
	auth      *httpx.Auth
	masks     *redact.Registry

	mu   sync.Mutex
	self *platform.Account // cached by selfAccount
	// commits maps "<repository uuid> <short hash>" to the full commit id
	// (fullCommit): a short id of a commit that exists never names another.
	commits map[string]string
}

// newClient checks the provider and the credential and builds the client.
func newClient(p config.ResolvedProvider, cred auth.Credential, hc *httpx.Client) (*client, error) {
	if p.Type != "bitbucket" {
		return nil, fmt.Errorf("bitbucket: provider %s has type %q, not bitbucket", p.ID, p.Type)
	}
	if hc == nil {
		return nil, errors.New("bitbucket: no HTTP client")
	}
	api, apiURL, err := baseURL("api_url", p.APIURL)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: provider %s: %w", p.ID, err)
	}
	web, webURL, err := baseURL("url", p.URL)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: provider %s: %w", p.ID, err)
	}
	host := strings.ToLower(p.Host)
	if host == "" {
		host = strings.ToLower(webURL.Host)
	}
	c := &client{
		http:    hc,
		api:     api,
		apiURL:  apiURL,
		web:     web,
		host:    host,
		masks:   redact.New(),
		commits: map[string]string{},
	}
	switch cred.Kind {
	case 0:
		if cred.Token != "" || cred.AppID != "" || len(cred.AppKey) > 0 {
			return nil, errors.New("bitbucket: a credential without a kind")
		}
	case auth.Token:
		if cred.Token == "" {
			return nil, errors.New("bitbucket: an empty token")
		}
		c.token = cred.Token
		c.masks.Add(cred.Token, basicUser)
		c.auth = &httpx.Auth{Hosts: []string{strings.ToLower(apiURL.Host)}, Header: c.apiHeader}
	case auth.App:
		return nil, fmt.Errorf("bitbucket: provider %s: a GitHub App cannot sign in to Bitbucket; use an API token of a bot account", p.ID)
	default:
		return nil, fmt.Errorf("bitbucket: provider %s: unknown credential kind %d", p.ID, cred.Kind)
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

// apiHeader is the Authorization header of API requests: an API token is
// a bearer token of the REST API (REST intro, "API tokens").
func (c *client) apiHeader(context.Context) (string, error) { return "Bearer " + c.token, nil }

// gitHeader is the Authorization header of git over HTTPS.
func (c *client) gitHeader(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(basicUser+":"+c.token)), nil
}

// mask replaces the credential's forms in s.
func (c *client) mask(s string) string { return c.masks.Replace(s) }

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

// pathSegments escapes each segment of a slash-separated path (a file
// path, a branch name) and joins them with slashes, which the API reads as
// they are: the branch API's own links keep the slashes of a branch name
// ("…/refs/branches/issue-9.3/AUI-5343-assistive-class" in the OpenAPI
// description).
func pathSegments(p string) []string { return strings.Split(p, "/") }

// call sends one API request of op and decodes a 2xx JSON response into
// out (nil skips decoding). Every error is a masked *platform.Error, but a
// canceled context's.
//
// A GET follows a redirect to the same origin below the API base, at most
// maxRedirects times, with the same credential (a renamed repository may
// answer with one; whether it does is to be confirmed live). httpx follows
// no redirect of an authenticated request itself.
func (c *client) call(ctx context.Context, op, method, u string, query url.Values, in, out any) (*httpx.Response, error) {
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for hops := 0; ; hops++ {
		resp, err := c.http.JSON(ctx, method, u, c.auth, in, out)
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
func (c *client) get(ctx context.Context, op, u string, query url.Values, out any) (*httpx.Response, error) {
	return c.call(ctx, op, http.MethodGet, u, query, nil, out)
}

// redirect returns where the redirect err answered to a request for from
// leads, when it is one to follow (see below).
func (c *client) redirect(from string, err error) (string, bool) {
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return "", false
	}
	return c.follow(from, se.Status, se.Header)
}

// follow returns where a response of status with header to a request for
// from leads, when it is a redirect to follow: a 301, 302, 303, 307 or 308
// whose Location, resolved against from, is on the API's scheme, host and
// port, without credentials, and below the API base.
func (c *client) follow(from string, status int, header http.Header) (string, bool) {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", false
	}
	loc := strings.TrimSpace(header.Get("Location"))
	base, perr := url.Parse(from)
	if loc == "" || perr != nil {
		return "", false
	}
	to, perr := base.Parse(loc)
	if perr != nil || !c.belowAPI(to) {
		return "", false
	}
	to.Fragment, to.RawFragment = "", ""
	return to.String(), true
}

// belowAPI reports whether u is on the API's scheme, host and port, without
// credentials, and below the API base.
func (c *client) belowAPI(u *url.URL) bool {
	if u.User != nil || !sameOrigin(u, c.apiURL) {
		return false
	}
	root := strings.TrimRight(c.apiURL.Path, "/") + "/"
	return strings.HasPrefix(u.Path, root) && !strings.Contains(u.Path, "/../") && !strings.HasSuffix(u.Path, "/..")
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

// errOffsite says the API redirected a raw read away from itself: Git LFS
// content lives on Atlassian's media platform (the OpenAPI description of
// GET …/src/{commit}/{path}).
var errOffsite = errors.New("the content is served from another host")

// raw reads the body of u, a GET outside JSON (a file's raw content). A
// redirect below the API base is followed as call follows it; one
// elsewhere is errOffsite. A non-2xx answer is classified as call
// classifies it.
func (c *client) raw(ctx context.Context, op, u string) ([]byte, error) {
	for hops := 0; ; hops++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, &platform.Error{Op: op, Class: platform.ClassInvalid, Err: c.masked(err)}
		}
		req.Header.Set("Accept", "*/*")
		resp, err := c.http.Do(req, c.auth)
		if err != nil {
			return nil, c.apiError(op, err)
		}
		if resp.Status >= 200 && resp.Status <= 299 {
			return resp.Body, nil
		}
		if next, ok := c.follow(u, resp.Status, resp.Header); ok && hops < maxRedirects {
			u = next
			continue
		}
		if resp.Status >= 300 && resp.Status <= 399 {
			return nil, &platform.Error{Op: op, Class: platform.ClassUnknown, Status: resp.Status,
				Err: fmt.Errorf("GET %s: %d: %w", c.mask(withoutQuery(u)), resp.Status, errOffsite)}
		}
		return nil, c.statusError(op, &httpx.StatusError{
			Method: http.MethodGet, URL: withoutQuery(u), Status: resp.Status,
			Header: resp.Header, Snippet: snippet(resp.Body),
		})
	}
}

// withoutQuery returns u without its query and fragment, as httpx shows
// URLs in errors.
func withoutQuery(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

// snippet returns at most snippetLimit bytes of body on one line.
func snippet(body []byte) string {
	if len(body) > snippetLimit {
		body = body[:snippetLimit]
	}
	return strings.Join(strings.FieldsFunc(strings.ToValidUTF8(string(body), "?"), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// page is one page of a paginated collection (REST intro, "Pagination"):
// values, and next, the opaque link to the following page, absent on the
// last.
type page[T any] struct {
	Values []T    `json:"values"`
	Next   string `json:"next"`
}

// listAll reads a paginated collection of T at u with query and calls each
// for every item, in order, following the next links, which must stay
// below the API base. complete is false when the listing was capped at
// maxPages. An error of a page after the first comes with the page in err
// (see pageError), so a caller can tell a failed listing from a failed
// first request.
func listAll[T any](ctx context.Context, c *client, op, u string, query url.Values, maxPages int, each func(T) error) (complete bool, err error) {
	next := u
	if len(query) > 0 {
		next += "?" + query.Encode()
	}
	for n := 1; n <= maxPages; n++ {
		var p page[T]
		if _, err := c.get(ctx, op, next, nil, &p); err != nil {
			if n > 1 {
				return false, &pageError{page: n, err: err}
			}
			return false, err
		}
		for _, it := range p.Values {
			if err := each(it); err != nil {
				return false, err
			}
		}
		if p.Next == "" {
			return true, nil
		}
		to, perr := url.Parse(p.Next)
		if perr != nil || !c.belowAPI(to) {
			return false, shapeError(op, "a next page link outside the API: %s", c.mask(withoutQuery(p.Next)))
		}
		next = to.String()
	}
	return false, nil
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

// splitRepoPath splits "workspace/slug". Bitbucket has no nested
// namespaces: a path of another shape cannot name a repository (ok false).
func splitRepoPath(p string) (workspace, slug string, ok bool) {
	workspace, slug, found := strings.Cut(p, "/")
	if !found || workspace == "" || slug == "" || strings.Contains(slug, "/") ||
		strings.ContainsAny(p, "\x00?#") || workspace == "." || workspace == ".." || slug == "." || slug == ".." {
		return "", "", false
	}
	return workspace, slug, true
}

// repoPath returns the workspace and slug of r, or a ClassInvalid error for
// op.
func repoPath(op string, r platform.Repo) (workspace, slug string, err error) {
	workspace, slug, ok := splitRepoPath(r.Path)
	if !ok {
		return "", "", invalid(op, "%q is not a workspace/repository path", r.Path)
	}
	return workspace, slug, nil
}
