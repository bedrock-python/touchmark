package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// basicUser is the user name of git's Basic credentials: GitLab takes any
// non-blank name with a token as the password, "oauth2" by convention
// (https://docs.gitlab.com/user/profile/personal_access_tokens/). The CLI
// registers the Basic form of this user for masking.
const basicUser = "oauth2"

// tokenHeader is the header of API requests
// (https://docs.gitlab.com/api/rest/authentication/).
const tokenHeader = "PRIVATE-TOKEN"

// pageSize is per_page of every listing: GitLab's maximum.
const pageSize = 100

// maxRedirects bounds the redirects one GET follows. GitLab answers an old
// project path with the project itself; a proxy in front of it may still
// redirect.
const maxRedirects = 3

// client is the HTTP side of one driver: one provider under one identity.
// It is safe for concurrent use.
type client struct {
	http *httpx.Client
	// api is the REST base without a trailing slash (…/api/v4); apiURL is
	// it parsed: redirects are followed only below it.
	api    string
	apiURL *url.URL
	// web is the provider's web URL without a trailing slash; git remotes
	// are under it. host is the provider host (Repo.Host).
	web, host string
	token     string // "" when anonymous
	auth      *httpx.Auth
	// gqlAuth is the token as a bearer token, for GraphQL; noGraphQL is set
	// once the instance turned a GraphQL request down (ReadFiles).
	gqlAuth   *httpx.Auth
	noGraphQL atomic.Bool
	masks     *redact.Registry

	mu    sync.Mutex
	self  *platform.Account // cached by selfAccount
	inst  *instance         // cached by instance
	users map[int64]*apiUser
}

// newClient checks the provider and the credential and builds the client.
func newClient(p config.ResolvedProvider, cred auth.Credential, hc *httpx.Client) (*client, error) {
	if p.Type != "gitlab" {
		return nil, fmt.Errorf("gitlab: provider %s has type %q, not gitlab", p.ID, p.Type)
	}
	if hc == nil {
		return nil, errors.New("gitlab: no HTTP client")
	}
	api, apiURL, err := baseURL("api_url", p.APIURL)
	if err != nil {
		return nil, fmt.Errorf("gitlab: provider %s: %w", p.ID, err)
	}
	web, webURL, err := baseURL("url", p.URL)
	if err != nil {
		return nil, fmt.Errorf("gitlab: provider %s: %w", p.ID, err)
	}
	host := strings.ToLower(p.Host)
	if host == "" {
		host = strings.ToLower(webURL.Host)
	}
	c := &client{
		http:   hc,
		api:    api,
		apiURL: apiURL,
		web:    web,
		host:   host,
		masks:  redact.New(),
		users:  map[int64]*apiUser{},
	}
	switch cred.Kind {
	case 0:
		if cred.Token != "" || cred.AppID != "" || len(cred.AppKey) > 0 {
			return nil, errors.New("gitlab: a credential without a kind")
		}
	case auth.Token:
		if cred.Token == "" {
			return nil, errors.New("gitlab: an empty token")
		}
		c.token = cred.Token
		c.masks.Add(cred.Token, basicUser)
		c.auth = &httpx.Auth{Hosts: []string{strings.ToLower(apiURL.Host)}, Name: tokenHeader, Header: c.apiHeader}
		c.gqlAuth = &httpx.Auth{Hosts: []string{strings.ToLower(apiURL.Host)}, Name: "Authorization", Header: c.bearerHeader}
	case auth.App:
		return nil, fmt.Errorf("gitlab: provider %s: a GitHub App cannot sign in to GitLab; use a token of a service account or a group access token", p.ID)
	default:
		return nil, fmt.Errorf("gitlab: provider %s: unknown credential kind %d", p.ID, cred.Kind)
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

// apiHeader is the PRIVATE-TOKEN header of API requests.
func (c *client) apiHeader(context.Context) (string, error) { return c.token, nil }

// bearerHeader is the Authorization header of GraphQL requests
// (https://docs.gitlab.com/api/graphql/#authentication).
func (c *client) bearerHeader(context.Context) (string, error) { return "Bearer " + c.token, nil }

// gitHeader is the Authorization header of git over HTTPS.
func (c *client) gitHeader(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(basicUser+":"+c.token)), nil
}

// mask replaces the credential's forms in s.
func (c *client) mask(s string) string { return c.masks.Replace(s) }

// escape escapes one path segment of the API: a project or group path, a
// file path or a branch name goes in whole, its slashes as %2F
// (https://docs.gitlab.com/api/rest/#namespaced-paths). Dots are escaped
// too (%2E, as the documentation's examples do), so that a name ending in
// ".json" is never read as a format.
func escape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), ".", "%2E")
}

// endpoint joins the API base and path segments, each escaped whole.
func (c *client) endpoint(segments ...string) string {
	var b strings.Builder
	b.WriteString(c.api)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(escape(s))
	}
	return b.String()
}

// projectURL is the endpoint of project id (a numeric id or a full path)
// with more segments.
func (c *client) projectURL(id string, more ...string) string {
	return c.endpoint(append([]string{"projects", id}, more...)...)
}

// call sends one API request of op and decodes a 2xx JSON response into
// out (nil skips decoding). Every error is a masked *platform.Error, but a
// canceled context's.
//
// A GET follows a redirect below the API base on the same origin, at most
// maxRedirects times, with the same credential (GitLab itself answers old
// project paths without a redirect; a proxy may redirect). httpx follows no
// redirect of an authenticated request itself, and a write is never
// repeated elsewhere: its redirect stays an error.
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

// get is call with GET.
func (c *client) get(ctx context.Context, op, u string, query url.Values, out any) (*httpx.Response, error) {
	return c.call(ctx, op, http.MethodGet, u, query, nil, out)
}

// listAll reads a paginated listing of T at u with query and calls each for
// every item, in order, reading at most maxPages pages of pageSize items.
// complete is false when the listing was capped. An error of page n > 1
// comes with the page in err (see pageError), so a caller can tell a failed
// listing from a failed first request.
//
// The next page is the one the Link header's rel="next" names (offset and
// keyset pagination both send it; only its query is taken, the path and
// origin stay u's), else the X-Next-Page header's (offset pagination; empty
// on the last page). X-Total and X-Total-Pages are never read: GitLab
// leaves them out above 10 000 rows
// (https://docs.gitlab.com/api/rest/#pagination). A page without either
// header after one with them is the last (keyset pagination's last page
// has no Link); a listing that never had them ends at an empty or short
// page.
func listAll[T any](ctx context.Context, c *client, op, u string, query url.Values, maxPages int, each func(T) error) (complete bool, err error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(pageSize))
	// paged is set once a response carried pagination headers: from then
	// on a response without them is the last page (keyset pagination
	// sends no Link on its last page).
	paged := false
	for page := 1; page <= maxPages; page++ {
		var items []T
		resp, err := c.get(ctx, op, u, q, &items)
		if err != nil {
			if page > 1 {
				return false, &pageError{page: page, err: err}
			}
			return false, err
		}
		for _, it := range items {
			if err := each(it); err != nil {
				return false, err
			}
		}
		if len(items) == 0 {
			return true, nil
		}
		next, ok := nextPage(resp.Header, q)
		switch {
		case ok && next == nil:
			return true, nil
		case ok:
			q, paged = next, true
		case paged, len(items) < pageSize:
			return true, nil
		default:
			q.Set("page", strconv.Itoa(page+1))
		}
	}
	return false, nil
}

// nextPage returns the query of the next page after a page asked for with
// q: from the Link header's rel="next", else from X-Next-Page. ok is false
// when the response has neither header; next is nil on the last page.
func nextPage(h http.Header, q url.Values) (next url.Values, ok bool) {
	if link, found := linkNext(h); found {
		if link == "" {
			// A Link header without rel="next" ends the listing, unless
			// X-Next-Page says otherwise (a proxy may drop parts of it).
			if n, has := xNextPage(h, q); has {
				return n, true
			}
			return nil, true
		}
		u, err := url.Parse(link)
		if err == nil {
			if lq, err := url.ParseQuery(u.RawQuery); err == nil && len(lq) > 0 {
				return lq, true
			}
		}
	}
	return xNextPage(h, q)
}

// xNextPage reads X-Next-Page: ok is false without the header, next nil
// when it is empty (the last page).
func xNextPage(h http.Header, q url.Values) (url.Values, bool) {
	values := h.Values("X-Next-Page")
	if len(values) == 0 {
		return nil, false
	}
	v := strings.TrimSpace(values[0])
	if v == "" {
		return nil, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return nil, false
	}
	next := url.Values{}
	for k, vs := range q {
		next[k] = vs
	}
	next.Set("page", strconv.Itoa(n))
	return next, true
}

// linkNext returns the URL of rel="next" in the Link header; found is false
// without a Link header, and the URL is "" when the header has no next.
func linkNext(h http.Header) (next string, found bool) {
	values := h.Values("Link")
	if len(values) == 0 {
		return "", false
	}
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			target, params, ok := strings.Cut(part, ";")
			if !ok {
				continue
			}
			for p := range strings.SplitSeq(params, ";") {
				name, value, _ := strings.Cut(strings.TrimSpace(p), "=")
				if !strings.EqualFold(strings.TrimSpace(name), "rel") {
					continue
				}
				for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(value), `"`)) {
					if strings.EqualFold(rel, "next") {
						return strings.Trim(strings.TrimSpace(target), "<>"), true
					}
				}
			}
		}
	}
	return "", true
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

// checkFullPath accepts a GitLab full path: segments separated by "/",
// none empty, "." or "..", no NUL, "?" or "#". A project path has at least
// two segments.
func checkFullPath(p string, minSegments int) bool {
	if p == "" || strings.ContainsAny(p, "\x00?#") {
		return false
	}
	n := 0
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		n++
	}
	return n >= minSegments
}
