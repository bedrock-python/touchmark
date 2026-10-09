package bitbucketdc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// providerType is the type of a Bitbucket Data Center provider in hub.yml.
const providerType = "bitbucket-datacenter"

// pageLimit is the limit a listing asks for: the default cap of most
// collections (page.max.repositories, page.max.pullrequests,
// page.max.users and page.max.branches are 1 000; an instance may lower
// them, and the answer's own limit and nextPageStart are what count).
const pageLimit = 1000

// activityLimit is the limit of a pull request's activities
// (page.max.pullrequest.activities is 500).
const activityLimit = 500

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
	// (https://bitbucket.example.com/rest/api/latest); apiURL is api
	// parsed: redirects and the like are followed only below it.
	api    string
	apiURL *url.URL
	// web is the provider's url without a trailing slash (the instance's
	// base URL, with its context path); clone URLs are under it
	// (<web>/scm/<key>/<slug>.git). host is the provider host (Repo.Host).
	web, host string
	token     string // "" when anonymous
	auth      *httpx.Auth
	masks     *redact.Registry

	mu   sync.Mutex
	self *platform.Account // cached by selfAccount
	// version is the instance's version, read once by Probe.
	version *[3]int
	// names maps the id of a user that Self or Lookup resolved to its
	// name, which the dashboard API takes (OpenPRsBy).
	names map[string]string
}

// newClient checks the provider and the credential and builds the client.
func newClient(p config.ResolvedProvider, cred auth.Credential, hc *httpx.Client) (*client, error) {
	if p.Type != providerType {
		return nil, fmt.Errorf("bitbucket-datacenter: provider %s has type %q, not %s", p.ID, p.Type, providerType)
	}
	if hc == nil {
		return nil, errors.New("bitbucket-datacenter: no HTTP client")
	}
	api, apiURL, err := baseURL("api_url", p.APIURL)
	if err != nil {
		return nil, fmt.Errorf("bitbucket-datacenter: provider %s: %w", p.ID, err)
	}
	web, webURL, err := baseURL("url", p.URL)
	if err != nil {
		return nil, fmt.Errorf("bitbucket-datacenter: provider %s: %w", p.ID, err)
	}
	host := strings.ToLower(p.Host)
	if host == "" {
		host = strings.ToLower(webURL.Host)
	}
	c := &client{http: hc, api: api, apiURL: apiURL, web: web, host: host, masks: redact.New(), names: map[string]string{}}
	switch cred.Kind {
	case 0:
		if cred.Token != "" || cred.AppID != "" || len(cred.AppKey) > 0 {
			return nil, errors.New("bitbucket-datacenter: a credential without a kind")
		}
	case auth.Token:
		if cred.Token == "" {
			return nil, errors.New("bitbucket-datacenter: an empty token")
		}
		c.token = cred.Token
		c.masks.Add(cred.Token)
		// The token goes to the API's host and to the web URL's (git), which
		// are one host unless api_url says otherwise.
		hosts := []string{strings.ToLower(apiURL.Host)}
		if h := strings.ToLower(webURL.Host); h != hosts[0] {
			hosts = append(hosts, h)
		}
		c.auth = &httpx.Auth{Hosts: hosts, Header: c.header}
	case auth.App:
		return nil, fmt.Errorf("bitbucket-datacenter: provider %s: a GitHub App cannot sign in to Bitbucket; use an HTTP access token", p.ID)
	default:
		return nil, fmt.Errorf("bitbucket-datacenter: provider %s: unknown credential kind %d", p.ID, cred.Kind)
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

// header is the Authorization header of API requests and of git over
// HTTPS: every kind of HTTP access token is a bearer token on 8.19 and
// later.
func (c *client) header(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "Bearer " + c.token, nil
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

// repoURL is the endpoint of repository key/slug and more segments.
func (c *client) repoURL(key, slug string, more ...string) string {
	return c.endpoint(append([]string{"projects", key, "repos", slug}, more...)...)
}

// pathSegments splits a slash-separated file path into its segments, which
// endpoint escapes one by one.
func pathSegments(p string) []string { return strings.Split(p, "/") }

// call sends one API request of op and decodes a 2xx JSON response into
// out (nil skips decoding). Every error is a masked *platform.Error, but a
// canceled context's. A GET follows a redirect to the same origin below
// the API base, at most maxRedirects times, with the same credential;
// httpx follows no redirect of an authenticated request itself.
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
// leads, when it is one to follow.
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

// raw reads the body of u with query, a GET outside JSON (a file's raw
// content). A redirect below the API base is followed as call follows it;
// a non-2xx answer is classified as call classifies it.
func (c *client) raw(ctx context.Context, op, u string, query url.Values) ([]byte, error) {
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
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

// page is one page of a paged collection (REST intro, "Paged APIs"):
// values, isLastPage, and nextPageStart, the start of the next page, which
// a client must use as it is.
type page[T any] struct {
	Values        []T    `json:"values"`
	IsLastPage    *bool  `json:"isLastPage"`
	NextPageStart *int64 `json:"nextPageStart"`
}

// listAll reads a paged collection of T at u with query and calls each
// for every item, in order, asking for limit items a page and following
// nextPageStart. complete is false when the listing was capped at
// maxPages. An error of a page after the first comes with the page in err
// (see pageError), so a caller can tell a failed listing from a failed
// first request. A page that says it is not the last but names no next
// start, or one that does not move forward, is an unexpected answer.
func listAll[T any](ctx context.Context, c *client, op, u string, query url.Values, limit, maxPages int, each func(T) error) (complete bool, err error) {
	start := int64(0)
	for n := 1; n <= maxPages; n++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("limit", strconv.Itoa(limit))
		if start > 0 {
			q.Set("start", strconv.FormatInt(start, 10))
		}
		var p page[T]
		if _, err := c.get(ctx, op, u, q, &p); err != nil {
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
		switch {
		case p.IsLastPage == nil:
			return false, shapeError(op, "a page without isLastPage")
		case *p.IsLastPage:
			return true, nil
		case p.NextPageStart == nil || *p.NextPageStart <= start:
			return false, shapeError(op, "a page that is not the last names no later start")
		}
		start = *p.NextPageStart
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

// splitRepoPath splits "<project key>/<slug>". Bitbucket Data Center has
// no nested projects: a path of another shape names no repository (ok
// false).
func splitRepoPath(p string) (key, slug string, ok bool) {
	key, slug, found := strings.Cut(p, "/")
	if !found || key == "" || slug == "" || strings.Contains(slug, "/") ||
		strings.ContainsAny(p, "\x00?#") || key == "." || key == ".." || slug == "." || slug == ".." {
		return "", "", false
	}
	return key, slug, true
}

// repoPath returns the project key and slug of r, or a ClassInvalid error
// for op.
func repoPath(op string, r platform.Repo) (key, slug string, err error) {
	key, slug, ok := splitRepoPath(r.Path)
	if !ok {
		return "", "", invalid(op, "%q is not a <project key>/<repository> path", r.Path)
	}
	return key, slug, nil
}
