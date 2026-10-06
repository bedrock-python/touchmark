package gitea

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

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// basicUser is the user name of git's Basic credentials. Gitea and Forgejo
// ignore it when the password is a token; it is one of the users whose
// Basic form the CLI registers for masking.
const basicUser = "x-access-token"

// Page sizes and bounds of listings.
const (
	// defaultPageSize is asked for when the instance's maximum is unknown:
	// the default of [api] MAX_RESPONSE_ITEMS.
	defaultPageSize = 50
	// treePageSize is asked for tree listings; the server caps it at
	// [api] DEFAULT_GIT_TREES_PER_PAGE (1000 by default).
	treePageSize = 1000
)

// maxRedirects bounds the redirects one GET follows: a renamed owner and
// then a renamed repository take two.
const maxRedirects = 3

// client is the HTTP side of one driver: one provider under one identity.
// It is safe for concurrent use.
type client struct {
	http *httpx.Client
	// api is the REST base without a trailing slash (…/api/v1); forgejoURL
	// is Forgejo's version endpoint on the same host ("" when it cannot be
	// derived from api).
	api, forgejoURL string
	// apiURL is api parsed: redirects are followed only below it.
	apiURL *url.URL
	// web is the provider's web URL without a trailing slash; git remotes
	// are under it. host is the provider host (Repo.Host).
	web, host string
	token     string // "" when anonymous
	auth      *httpx.Auth
	masks     *redact.Registry

	mu       sync.Mutex
	self     *platform.Account // cached by selfAccount
	inst     *instance         // cached by instance
	pageSize int               // the instance's page size, 0 until known
	sizeDone bool              // the page size was looked up (maybe in vain)
	closers  map[closerKey]*platform.Account
}

// newClient checks the provider and the credential and builds the client.
func newClient(p config.ResolvedProvider, cred auth.Credential, hc *httpx.Client) (*client, error) {
	switch p.Type {
	case "gitea", "forgejo":
	default:
		return nil, fmt.Errorf("gitea: provider %s has type %q, not gitea or forgejo", p.ID, p.Type)
	}
	if hc == nil {
		return nil, errors.New("gitea: no HTTP client")
	}
	api, apiURL, err := baseURL("api_url", p.APIURL)
	if err != nil {
		return nil, fmt.Errorf("gitea: provider %s: %w", p.ID, err)
	}
	web, webURL, err := baseURL("url", p.URL)
	if err != nil {
		return nil, fmt.Errorf("gitea: provider %s: %w", p.ID, err)
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
		closers: map[closerKey]*platform.Account{},
	}
	// Forgejo serves its own API next to /api/v1, which tells it from
	// Gitea.
	if root, ok := strings.CutSuffix(api, "/v1"); ok {
		c.forgejoURL = root + "/forgejo/v1/version"
	}
	switch cred.Kind {
	case 0:
		if cred.Token != "" || cred.AppID != "" || len(cred.AppKey) > 0 {
			return nil, errors.New("gitea: a credential without a kind")
		}
	case auth.Token:
		if cred.Token == "" {
			return nil, errors.New("gitea: an empty token")
		}
		c.token = cred.Token
		c.masks.Add(cred.Token, basicUser)
		c.auth = &httpx.Auth{Hosts: []string{strings.ToLower(apiURL.Host)}, Header: c.apiHeader}
	case auth.App:
		return nil, fmt.Errorf("gitea: provider %s: a GitHub App cannot sign in to %s; use a personal access token of a bot user", p.ID, p.Type)
	default:
		return nil, fmt.Errorf("gitea: provider %s: unknown credential kind %d", p.ID, cred.Kind)
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

// apiHeader is the Authorization header of API requests.
func (c *client) apiHeader(context.Context) (string, error) { return "token " + c.token, nil }

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

// call sends one API request of op and decodes a 2xx JSON response into
// out (nil skips decoding). Every error is a masked *platform.Error, but a
// canceled context's.
//
// A GET follows a redirect below the API base on the same origin, at most
// maxRedirects times, with the same credential: the servers answer a path
// of a renamed repository with 301, and one of a renamed user or
// organization, or of a repository under it, with 307 to the new path, the
// query kept (context.RedirectToRepo and RedirectToUser of both platforms;
// checked on Gitea 1.26 and 1.27 and Forgejo 15 and 16). httpx follows no
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
	if perr != nil || to.User != nil || !sameOrigin(to, c.apiURL) {
		return "", false
	}
	root := strings.TrimRight(c.apiURL.Path, "/") + "/"
	if !strings.HasPrefix(to.Path, root) || strings.Contains(to.Path, "/../") {
		return "", false
	}
	to.Fragment, to.RawFragment = "", ""
	return to.String(), true
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

// listOpts bound and interpret one listing.
type listOpts struct {
	// maxPages bounds the pages read; a listing that has more is capped.
	maxPages int
	// trustTotal tells that X-Total-Count is the size of the whole listing.
	// The timeline reports the size of the page instead.
	trustTotal bool
}

// listAll reads a paginated listing of T at u with query and calls each for
// every item, in order. complete is false when the listing was capped at
// opts.maxPages. An error of page n > 1 comes with the page in err (see
// pageError), so a caller can tell a failed listing from a failed first
// request.
//
// A listing ends at an empty page; else where the Link header has no
// rel="next" (the servers compute it from the total, so pages shortened by
// permission filtering do not end it early); else where X-Total-Count says
// so, when it is trusted; else at a page shorter than the instance's page
// size, when that size is known.
func listAll[T any](ctx context.Context, c *client, op, u string, query url.Values, opts listOpts, each func(T) error) (complete bool, err error) {
	size, known := c.pages(ctx)
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(size))
	seen := 0
	for page := 1; page <= opts.maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
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
		seen += len(items)
		if len(items) == 0 {
			return true, nil
		}
		if next, ok := linkNext(resp.Header); ok {
			if !next {
				return true, nil
			}
			continue
		}
		if total, ok := totalCount(resp.Header); ok && opts.trustTotal {
			if seen >= total {
				return true, nil
			}
			continue
		}
		if known && len(items) < size {
			return true, nil
		}
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

// linkNext reports whether the Link header h holds has rel="next"; ok is
// false without a Link header.
func linkNext(h http.Header) (next, ok bool) {
	values := h.Values("Link")
	if len(values) == 0 {
		return false, false
	}
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			_, params, found := strings.Cut(part, ";")
			if !found {
				continue
			}
			for p := range strings.SplitSeq(params, ";") {
				name, value, _ := strings.Cut(strings.TrimSpace(p), "=")
				if strings.EqualFold(strings.TrimSpace(name), "rel") {
					for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(value), `"`)) {
						if strings.EqualFold(rel, "next") {
							return true, true
						}
					}
				}
			}
		}
	}
	return false, true
}

// totalCount returns X-Total-Count; ok is false without a valid one.
func totalCount(h http.Header) (int, bool) {
	v := strings.TrimSpace(h.Get("X-Total-Count"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// apiSettings is GET /settings/api.
type apiSettings struct {
	MaxResponseItems int `json:"max_response_items"`
}

// pages returns the page size to ask for and whether it is the instance's
// own (so a shorter page is the last): [api] MAX_RESPONSE_ITEMS, at most
// defaultPageSize. The lookup is made once; a failure that is not transient
// is remembered, and the listings then read up to an empty page.
func (c *client) pages(ctx context.Context) (int, bool) {
	c.mu.Lock()
	if c.sizeDone {
		size := c.pageSize
		c.mu.Unlock()
		if size == 0 {
			return defaultPageSize, false
		}
		return size, true
	}
	c.mu.Unlock()
	var s apiSettings
	_, err := c.get(ctx, "read the API settings", c.endpoint("settings", "api"), nil, &s)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil && s.MaxResponseItems > 0:
		c.pageSize, c.sizeDone = min(s.MaxResponseItems, defaultPageSize), true
		return c.pageSize, true
	case err == nil || (platform.ClassOf(err) != platform.ClassTransient && ctx.Err() == nil):
		c.sizeDone = true
	}
	return defaultPageSize, false
}

// splitRepoPath splits "owner/name". Gitea and Forgejo have no nested
// namespaces: a path of another shape cannot name a repository (ok false).
func splitRepoPath(p string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(p, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") ||
		strings.ContainsAny(p, "\x00?#") || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", false
	}
	return owner, name, true
}

// repoPath returns the owner and name of r, or a ClassInvalid error for op.
func repoPath(op string, r platform.Repo) (owner, name string, err error) {
	owner, name, ok := splitRepoPath(r.Path)
	if !ok {
		return "", "", &platform.Error{Op: op, Class: platform.ClassInvalid, Err: fmt.Errorf("%q is not an owner/name repository path", r.Path)}
	}
	return owner, name, nil
}
