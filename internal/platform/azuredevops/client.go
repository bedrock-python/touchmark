package azuredevops

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
	"time"
	"unicode"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// apiVersion is the REST API version every request asks for.
const apiVersion = "7.1"

// gitUser is the user name of git's Basic credentials: Azure Repos takes
// any non-empty one with a PAT. The REST API gets an empty one (its Basic
// form is ":<token>"). Both forms are registered for masking.
const gitUser = "touchmark"

// cloudHost is the host of Azure DevOps Services; its identities live under
// vssps.dev.azure.com.
const cloudHost = "dev.azure.com"

// snippetLimit bounds the body kept from a failed raw request, in bytes, as
// httpx keeps for JSON requests.
const snippetLimit = 1 << 10

// client is the HTTP side of one driver: one organization under one
// identity. It is safe for concurrent use.
type client struct {
	http *httpx.Client
	// org is the organization; api the REST base without a trailing slash
	// (https://dev.azure.com/acme), whose /_apis/… are the organization's
	// APIs; vssps the base of the identity APIs
	// (https://vssps.dev.azure.com/acme; api itself elsewhere, a test
	// server).
	org, api, vssps string
	apiURL          *url.URL
	// web is the provider's url without a trailing slash; git remotes and
	// web links are under it. host is the provider host (Repo.Host).
	web, host string
	token     string // "" when anonymous
	auth      *httpx.Auth
	masks     *redact.Registry

	mu   sync.Mutex
	self *platform.Account // cached by selfAccount
}

// newClient checks the provider and the credential and builds the client.
func newClient(p config.ResolvedProvider, cred auth.Credential, hc *httpx.Client) (*client, error) {
	if p.Type != "azure-devops" {
		return nil, fmt.Errorf("azure-devops: provider %s has type %q, not azure-devops", p.ID, p.Type)
	}
	if hc == nil {
		return nil, errors.New("azure-devops: no HTTP client")
	}
	org, ok := config.AzureDevOpsOrg(p.URL)
	if !ok {
		return nil, fmt.Errorf("azure-devops: provider %s: url %q does not name one organization", p.ID, p.URL)
	}
	api, apiURL, err := baseURL("api_url", p.APIURL)
	if err != nil {
		return nil, fmt.Errorf("azure-devops: provider %s: %w", p.ID, err)
	}
	web, webURL, err := baseURL("url", p.URL)
	if err != nil {
		return nil, fmt.Errorf("azure-devops: provider %s: %w", p.ID, err)
	}
	host := strings.ToLower(p.Host)
	if host == "" {
		host = strings.ToLower(webURL.Host)
	}
	c := &client{
		http:   hc,
		org:    org,
		api:    api,
		apiURL: apiURL,
		vssps:  api,
		web:    web,
		host:   host,
		masks:  redact.New(),
	}
	hosts := []string{strings.ToLower(apiURL.Host)}
	if strings.EqualFold(apiURL.Host, cloudHost) {
		c.vssps = "https://vssps." + cloudHost + "/" + url.PathEscape(org)
		hosts = append(hosts, "vssps."+cloudHost)
	}
	switch cred.Kind {
	case 0:
		if cred.Token != "" || cred.AppID != "" || len(cred.AppKey) > 0 {
			return nil, errors.New("azure-devops: a credential without a kind")
		}
	case auth.Token:
		if cred.Token == "" {
			return nil, errors.New("azure-devops: an empty token")
		}
		c.token = cred.Token
		c.masks.Add(cred.Token, "", gitUser)
		c.auth = &httpx.Auth{Hosts: hosts, Header: c.apiHeader}
	case auth.App:
		return nil, fmt.Errorf("azure-devops: provider %s: a GitHub App cannot sign in to Azure DevOps; use a personal access token of a user", p.ID)
	default:
		return nil, fmt.Errorf("azure-devops: provider %s: unknown credential kind %d", p.ID, cred.Kind)
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

// apiHeader is the Authorization header of API requests: Basic with an
// empty user name and the PAT.
func (c *client) apiHeader(context.Context) (string, error) {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+c.token)), nil
}

// gitHeader is the Authorization header of git over HTTPS: Basic with a
// non-empty user name and the PAT.
func (c *client) gitHeader(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(gitUser+":"+c.token)), nil
}

// mask replaces the credential's forms in s.
func (c *client) mask(s string) string { return c.masks.Replace(s) }

// endpoint joins base and path segments, each escaped.
func endpoint(base string, segments ...string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// apis returns the organization's API URL of segments: {api}/_apis/….
func (c *client) apis(segments ...string) string {
	return endpoint(c.api, append([]string{"_apis"}, segments...)...)
}

// repoAPI returns the URL of a repository's API, by its id, without the
// project: {api}/_apis/git/repositories/{id}/….
func (c *client) repoAPI(id string, segments ...string) string {
	return c.apis(append([]string{"git", "repositories", id}, segments...)...)
}

// headers are sent with every API request: JSON, and no redirect to the
// sign-in page for a refused credential (401 instead).
func headers(accept string) http.Header {
	h := http.Header{}
	h.Set("Accept", accept)
	h.Set("X-TFS-FedAuthRedirect", "Suppress")
	return h
}

// call sends one API request of op with api-version and query, a JSON
// body in (nil for none; sent as contentType when it is not ""), and
// decodes a 2xx JSON response into out (nil skips decoding). Every error is
// a masked *platform.Error, but a canceled context's.
func (c *client) call(ctx context.Context, op, method, u string, query url.Values, contentType string, in, out any) (*httpx.Response, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("api-version", apiVersion)
	u += "?" + q.Encode()
	h := headers("application/json")
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	resp, err := c.http.JSONWith(ctx, method, u, c.auth, h, in, out)
	if resp != nil && signIn(resp.Status, resp.Header) {
		return resp, c.signInError(op, resp.Status)
	}
	if err != nil {
		return resp, c.apiError(op, err)
	}
	slowDown(ctx, resp.Status, resp.Header)
	return resp, nil
}

// slowDown pauses the provider's throttle when a successful answer
// carries Retry-After: Azure DevOps delays the requests of an identity
// over its threshold and asks it, in Retry-After, to wait before the next
// one so as not to be delayed or blocked
// (https://learn.microsoft.com/en-us/azure/devops/integrate/concepts/rate-limits).
// The wait goes to the Meter of ctx as an exhausted budget
// (throttle.Observe: a pause without a strike), bounded by maxRetryAfter;
// nothing without a Meter. A refusal with Retry-After is a rate limit
// instead (statusError).
func slowDown(ctx context.Context, status int, h http.Header) {
	if status < 200 || status > 299 || strings.TrimSpace(h.Get("Retry-After")) == "" {
		return
	}
	d := retryAfter(http.Header{"Retry-After": h.Values("Retry-After")}, now())
	if d <= 0 {
		return
	}
	secs := int64((d + time.Second - 1) / time.Second)
	throttle.Observe(ctx, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(secs, 10)}})
}

// get is call with GET.
func (c *client) get(ctx context.Context, op, u string, query url.Values, out any) (*httpx.Response, error) {
	return c.call(ctx, op, http.MethodGet, u, query, "", nil, out)
}

// signIn reports whether a response is Azure DevOps sending a refused
// credential to its sign-in page: a redirect to _signin (or to another
// host), or a 203 with HTML, which some refusals get even with
// X-TFS-FedAuthRedirect (assumed from the observed redirect of an
// anonymous request for a private resource, 2026-10-09).
func signIn(status int, h http.Header) bool {
	switch status {
	case http.StatusNonAuthoritativeInfo:
		return strings.Contains(strings.ToLower(h.Get("Content-Type")), "html")
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
		loc := strings.ToLower(h.Get("Location"))
		return strings.Contains(loc, "_signin") || strings.Contains(loc, "/signin") || strings.Contains(loc, "login.microsoftonline")
	}
	return false
}

// signInError is the ClassAuth error of a redirect to the sign-in page.
func (c *client) signInError(op string, status int) error {
	why := "Azure DevOps sent the request to its sign-in page: the credential is missing, expired or refused"
	if c.token == "" {
		why = "Azure DevOps sent the anonymous request to its sign-in page: it answers some reads only with a token, " +
			"even in public projects (the Trees API, pull request properties); give the reader a personal access token with Code (read)"
	}
	return &platform.Error{Op: op, Class: platform.ClassAuth, Status: status, Err: errors.New(why)}
}

// raw reads the bytes of u with api-version and query, a GET outside JSON
// (a blob's content). A non-2xx answer is classified as call classifies it.
func (c *client) raw(ctx context.Context, op, u string, query url.Values) ([]byte, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("api-version", apiVersion)
	full := u + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, &platform.Error{Op: op, Class: platform.ClassInvalid, Err: c.masked(err)}
	}
	for k, v := range headers("application/octet-stream") {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req, c.auth)
	if err != nil {
		return nil, c.apiError(op, err)
	}
	if signIn(resp.Status, resp.Header) {
		return nil, c.signInError(op, resp.Status)
	}
	if resp.Status >= 200 && resp.Status <= 299 {
		slowDown(ctx, resp.Status, resp.Header)
		return resp.Body, nil
	}
	return nil, c.statusError(op, &httpx.StatusError{
		Method: http.MethodGet, URL: u, Status: resp.Status,
		Header: resp.Header, Snippet: snippet(resp.Body),
	})
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

// list is a collection answer: {"count": n, "value": [...]}.
type list[T any] struct {
	Count int `json:"count"`
	Value []T `json:"value"`
}

// pageSize is the $top of the paged listings (pull requests).
const pageSize = 100

// listPages reads a collection of T at u with query, $top pageSize and
// growing $skip, and calls each for every item in order, until a page is
// short. complete is false when the listing was capped at maxPages. An
// error of a page after the first comes with the page in err (pageError).
func listPages[T any](ctx context.Context, c *client, op, u string, query url.Values, maxPages int, each func(T) error) (complete bool, err error) {
	for n := 0; n < maxPages; n++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("$top", fmt.Sprint(pageSize))
		q.Set("$skip", fmt.Sprint(n*pageSize))
		var p list[T]
		if _, err := c.get(ctx, op, u, q, &p); err != nil {
			if n > 0 {
				return false, &pageError{page: n + 1, err: err}
			}
			return false, err
		}
		for _, it := range p.Value {
			if err := each(it); err != nil {
				return false, err
			}
		}
		if len(p.Value) < pageSize {
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

// splitRepoPath splits "project/repository". Azure DevOps has no nested
// projects: a path of another shape names no repository (ok false).
func splitRepoPath(p string) (project, name string, ok bool) {
	project, name, found := strings.Cut(p, "/")
	if !found || project == "" || name == "" || strings.Contains(name, "/") ||
		strings.ContainsAny(p, "\x00?#") || project == "." || project == ".." || name == "." || name == ".." {
		return "", "", false
	}
	return project, name, true
}
