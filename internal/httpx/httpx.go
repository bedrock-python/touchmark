// Package httpx is the HTTP client every driver and the hub channel use.
//
// It enforces two rules: an Authorization header goes only
// to the hosts its credential belongs to, and responses are bounded.
// Retries are not here, and neither is pacing: the internal/throttle Meter
// a request's context carries is asked before it is sent (throttle.Request),
// so that the budget counts every HTTP write of a driver.
package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// Defaults.
const (
	DefaultTimeout = 30 * time.Second
	DefaultMaxBody = 32 << 20
)

const (
	defaultUserAgent = "touchmark"
	// maxRedirects bounds the redirects an anonymous request follows.
	maxRedirects = 5
	// snippetLimit bounds StatusError.Snippet, in bytes.
	snippetLimit = 1 << 10
	// maxHeaderBytes bounds a response header.
	maxHeaderBytes = 1 << 20
)

// Errors that Do wraps.
var (
	// ErrRefused is wrapped when Do does not send a request because it
	// breaks a rule: a credential for a host outside Auth.Hosts or over
	// plain http, credentials in the URL, or a credential header on an
	// anonymous request.
	ErrRefused = errors.New("request refused")
	// ErrTooLarge is wrapped when a response body exceeds MaxBody.
	ErrTooLarge = errors.New("response body too large")
)

// Options configure a Client.
type Options struct {
	// UserAgent defaults to "touchmark".
	UserAgent string
	// Timeout per request, DefaultTimeout when zero.
	Timeout time.Duration
	// MaxBody bounds a response body, DefaultMaxBody when zero.
	MaxBody int64
	// RootCAs replaces the system roots when set (providers[].ca_file).
	RootCAs *x509.CertPool
	// Redact masks secrets in errors. May be nil.
	Redact *redact.Registry
	// Transport overrides the transport (tests). May be nil.
	Transport http.RoundTripper
}

// Auth attaches a credential to requests for a fixed set of hosts.
type Auth struct {
	// Hosts are "host" or "host:port", compared case-insensitively. A
	// request to any other host is refused before it is sent.
	Hosts []string
	// Header returns the credential header value for one request.
	Header func(ctx context.Context) (string, error)
	// Name is the header the credential goes in: "" (Authorization),
	// "Authorization", "Private-Token" (GitLab's personal, project, group
	// and service account tokens) or "Job-Token" (GitLab's CI_JOB_TOKEN),
	// compared ignoring case. Any other name is refused before sending.
	Name string
}

// credentialHeader returns the canonical name of the header a's credential
// goes in; ok is false for a name Auth does not allow.
func (a *Auth) credentialHeader() (name string, ok bool) {
	if a.Name == "" {
		return "Authorization", true
	}
	for _, n := range []string{"Authorization", "Private-Token", "Job-Token"} {
		if strings.EqualFold(a.Name, n) {
			return n, true
		}
	}
	return "", false
}

// Response is a fully read response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Client sends requests. It is safe for concurrent use.
type Client struct {
	userAgent string
	timeout   time.Duration
	maxBody   int64
	redact    *redact.Registry
	// authed never follows redirects; anon follows same-origin ones.
	authed *http.Client
	anon   *http.Client
}

// New returns a client. It never follows redirects of authenticated
// requests; unauthenticated requests follow up to 5 redirects to the same
// host and scheme only. TLS 1.2 is the minimum.
//
// A redirect that is not followed is returned as the response. Without
// Options.Transport, the client uses its own transport: the proxy from the
// environment, HTTP/2, RootCAs, and a 1 MiB bound on response headers.
func New(o Options) *Client {
	c := &Client{userAgent: o.UserAgent, timeout: o.Timeout, maxBody: o.MaxBody, redact: o.Redact}
	if c.userAgent == "" {
		c.userAgent = defaultUserAgent
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.maxBody <= 0 {
		c.maxBody = DefaultMaxBody
	}
	rt := o.Transport
	if rt == nil {
		rt = newTransport(o.RootCAs)
	}
	c.authed = &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c.anon = &http.Client{Transport: rt, CheckRedirect: sameOriginRedirect}
	return c
}

func newTransport(roots *x509.CertPool) *http.Transport {
	return &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    16,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxHeaderBytes,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	}
}

// sameOriginRedirect lets an anonymous request follow a redirect to the
// same scheme, host and port, up to maxRedirects times; otherwise the
// redirect response is returned as is.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects || req.URL.User != nil || !sameOrigin(req.URL, via[0].URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	ah, ap, ok := urlHost(a)
	bh, bp, ok2 := urlHost(b)
	return ok && ok2 && strings.EqualFold(a.Scheme, b.Scheme) && ah == bh && ap == bp
}

// Do sends req with auth (nil for anonymous) and reads the whole body up to
// MaxBody; a larger body is an error and the connection is closed. With
// auth, the request URL must be https (http only for localhost and
// 127.0.0.1, for tests) and its host must be in auth.Hosts, else Do fails
// without sending. Errors are masked with Redact and never include header
// values.
//
// Plain http is also allowed for [::1]. Do never changes req: it sends a
// copy with the User-Agent and Authorization headers set. As with
// http.Client.Do, the request body is closed, also when Do refuses to send.
// A refused request wraps ErrRefused, a body over MaxBody ErrTooLarge, and
// a timeout context.DeadlineExceeded. The URL in errors has no query
// string or userinfo.
func (c *Client) Do(req *http.Request, auth *Auth) (*Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("no request: %w", ErrRefused)
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	where := c.show(req.URL)
	if err := checkRequest(req, auth); err != nil {
		closeBody(req)
		return nil, c.fail(method, where, err)
	}
	// The throttle the request's context carries paces and counts it: a
	// read, or a write of the platform's budget.
	// A request it refuses is not sent.
	if err := throttle.Request(req.Context(), method); err != nil {
		closeBody(req)
		return nil, c.fail(method, where, err)
	}

	ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
	defer cancel()
	out := req.Clone(ctx)
	out.Header.Set("User-Agent", c.userAgent)
	client := c.anon
	if auth != nil {
		value, err := auth.Header(ctx)
		if err == nil {
			err = checkHeaderValue(value)
		} else {
			err = fmt.Errorf("credential: %w", err)
		}
		if err != nil {
			closeBody(req)
			return nil, c.fail(method, where, err)
		}
		name, _ := auth.credentialHeader()
		out.Header.Set(name, value)
		client = c.authed
	}

	resp, err := client.Do(out)
	if err != nil {
		return nil, c.fail(method, where, transportError(ctx, err))
	}
	defer resp.Body.Close()
	// The answer's headers may show the platform's budget spent: the
	// throttle pauses the provider until it resets.
	throttle.Observe(req.Context(), resp.Header)
	body, err := c.readBody(ctx, resp)
	if err != nil {
		return nil, c.fail(method, where, err)
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

// readBody reads at most maxBody bytes. It does not drain a larger body:
// closing it unread makes the transport drop the connection. The length a
// HEAD response announces is the resource's, not a body's.
func (c *Client) readBody(ctx context.Context, resp *http.Response) ([]byte, error) {
	head := resp.Request != nil && resp.Request.Method == http.MethodHead
	if resp.ContentLength > c.maxBody && !head {
		return nil, fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, resp.ContentLength, c.maxBody)
	}
	limit := c.maxBody
	if limit < math.MaxInt64 {
		limit++
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", transportError(ctx, err))
	}
	if int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, c.maxBody)
	}
	return body, nil
}

// Registry returns the registry of secrets the client masks its errors
// with (Options.Redact), nil for none. A driver registers there every
// secret it mints during a run (a GitHub App's JWT and installation
// tokens), so that everything the run prints masks them.
func (c *Client) Registry() *redact.Registry { return c.redact }

// JSON sends a request with an optional JSON body and decodes a 2xx JSON
// response into out (skipped when out is nil). Non-2xx responses return a
// *StatusError with the status and at most 1 KiB of the body (masked).
//
// The response is returned with a *StatusError and with a decoding error,
// so callers can read headers such as Retry-After. A 204 response is not
// decoded.
func (c *Client) JSON(ctx context.Context, method, url string, auth *Auth, in, out any) (*Response, error) {
	return c.JSONWith(ctx, method, url, auth, nil, in, out)
}

// JSONWith is JSON with more request headers, such as a media type in
// Accept or an API version (GitHub's X-GitHub-Api-Version). Accept defaults
// to application/json. A header that may carry a credential
// (Authorization, Proxy-Authorization, Private-Token, Job-Token) is
// refused (ErrRefused) before anything is sent: the credential goes through
// auth only.
func (c *Client) JSONWith(ctx context.Context, method, url string, auth *Auth, header http.Header, in, out any) (*Response, error) {
	if method == "" {
		method = http.MethodGet
	}
	for key := range header {
		for _, name := range credentialHeaders {
			if strings.EqualFold(key, name) {
				return nil, c.fail(method, c.showRaw(url), fmt.Errorf("%w: header %s is set outside the credential", ErrRefused, name))
			}
		}
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, c.fail(method, c.showRaw(url), fmt.Errorf("encode request body: %w", err))
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, c.fail(method, c.showRaw(url), buildError(err))
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, values := range header {
		req.Header.Del(key)
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	resp, err := c.Do(req, auth)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return resp, &StatusError{
			Method:  method,
			URL:     c.show(req.URL),
			Status:  resp.Status,
			Header:  resp.Header,
			Snippet: c.snippet(resp.Body),
		}
	}
	if out != nil && resp.Status != http.StatusNoContent {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, c.fail(method, c.show(req.URL), fmt.Errorf("decode response: %w", err))
		}
	}
	return resp, nil
}

// StatusError is a non-2xx response.
type StatusError struct {
	Method, URL string // URL without query string
	Status      int
	Header      http.Header
	Snippet     string
}

func (e *StatusError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d", e.Method, e.URL, e.Status)
	if text := http.StatusText(e.Status); text != "" {
		b.WriteString(" " + text)
	}
	if e.Snippet != "" {
		b.WriteString(": " + e.Snippet)
	}
	return b.String()
}

// LoadCAFile reads a PEM bundle into a pool that also holds the system
// roots.
//
// Every PEM block must be a certificate that parses, and there must be at
// least one. Where the system roots are not available, the pool holds the
// file's certificates only.
func LoadCAFile(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	n := 0
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("CA file %s: unexpected PEM block %q", path, block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("CA file %s: certificate %d: %w", path, n+1, err)
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("CA file %s: no PEM certificates", path)
	}
	return pool, nil
}

// requestError is an error of Do: a masked message over the original chain,
// which errors.Is and errors.As still see.
type requestError struct {
	msg string
	err error
}

func (e *requestError) Error() string { return e.msg }
func (e *requestError) Unwrap() error { return e.err }

// fail returns err about the request method where, masked.
func (c *Client) fail(method, where string, err error) error {
	return &requestError{msg: c.redact.Replace(method + " " + where + ": " + err.Error()), err: err}
}

// show returns u for messages: without userinfo, query and fragment, masked.
func (c *Client) show(u *url.URL) string {
	v := *u
	v.User = nil
	v.RawQuery, v.ForceQuery = "", false
	v.Fragment, v.RawFragment = "", ""
	return c.redact.Replace(v.String())
}

// showRaw is show for a URL string that may not parse.
func (c *Client) showRaw(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	return c.show(u)
}

// snippet returns at most snippetLimit bytes of body for an error message:
// masked, on one line, without control characters.
func (c *Client) snippet(body []byte) string {
	s := sanitize(c.redact.Replace(string(body)))
	if len(s) > snippetLimit {
		const ellipsis = "..."
		cut := snippetLimit - len(ellipsis)
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + ellipsis
	}
	// Collapsing whitespace could join the parts of a form.
	return c.redact.Replace(s)
}

// sanitize turns runs of whitespace, control and bidirectional formatting
// characters into single spaces, trims the ends and replaces invalid UTF-8.
func sanitize(s string) string {
	var b strings.Builder
	gap := false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || isBidi(r) {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte(' ')
		}
		gap = false
		b.WriteRune(r)
	}
	return b.String()
}

func isBidi(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// credentialHeaders may carry a credential; an anonymous request must not
// set them.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Private-Token", "Job-Token"}

// checkRequest applies the rules Do enforces before sending.
func checkRequest(req *http.Request, auth *Auth) error {
	u := req.URL
	if u.User != nil {
		return fmt.Errorf("%w: credentials in the URL", ErrRefused)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("%w: scheme %q is not http or https", ErrRefused, u.Scheme)
	}
	host, port, ok := urlHost(u)
	if !ok {
		return fmt.Errorf("%w: no valid host in the URL", ErrRefused)
	}
	if auth == nil {
		for _, name := range credentialHeaders {
			for key := range req.Header {
				if strings.EqualFold(key, name) {
					return fmt.Errorf("%w: header %s on a request without a credential", ErrRefused, name)
				}
			}
		}
		return nil
	}
	if auth.Header == nil {
		return fmt.Errorf("%w: the credential has no header", ErrRefused)
	}
	if _, ok := auth.credentialHeader(); !ok {
		return fmt.Errorf("%w: a credential cannot go in header %q", ErrRefused, auth.Name)
	}
	if scheme == "http" && !isLoopback(host) {
		return fmt.Errorf("%w: a credential is sent over https only", ErrRefused)
	}
	if req.Host != "" {
		h, p, ok := splitHost(req.Host)
		if p == "" {
			p = port
		}
		if !ok || h != host || p != port {
			return fmt.Errorf("%w: the Host header differs from the URL", ErrRefused)
		}
	}
	if !hostAllowed(auth.Hosts, host, port, scheme) {
		return fmt.Errorf("%w: host %s is not one of the credential's hosts", ErrRefused, u.Host)
	}
	return nil
}

// hostAllowed reports whether host and port (from a URL with scheme) match
// an entry of hosts. An entry without a port stands for the scheme's
// default port.
func hostAllowed(hosts []string, host, port, scheme string) bool {
	for _, entry := range hosts {
		h, p, ok := splitHost(entry)
		if !ok {
			continue
		}
		if p == "" {
			p = defaultPort(scheme)
		}
		if h == host && p == port {
			return true
		}
	}
	return false
}

// urlHost returns the lowercased host of u (IPv6 without brackets) and its
// port, the scheme's default when u has none.
func urlHost(u *url.URL) (host, port string, ok bool) {
	host = strings.ToLower(u.Hostname())
	if host == "" {
		return "", "", false
	}
	if p := u.Port(); p != "" {
		port, ok = canonicalPort(p)
		return host, port, ok
	}
	port = defaultPort(u.Scheme)
	return host, port, port != ""
}

// splitHost parses "host", "host:port", "[v6]", "[v6]:port" or a bare IPv6
// address into a lowercased host and a canonical port ("" when absent).
func splitHost(s string) (host, port string, ok bool) {
	if s == "" || strings.ContainsAny(s, "/?#@ \t\r\n") {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(s, "["); found {
		h, after, found := strings.Cut(rest, "]")
		if !found || h == "" {
			return "", "", false
		}
		if after == "" {
			return strings.ToLower(h), "", true
		}
		p, found := strings.CutPrefix(after, ":")
		if !found {
			return "", "", false
		}
		port, ok = canonicalPort(p)
		return strings.ToLower(h), port, ok
	}
	if strings.Count(s, ":") > 1 {
		return strings.ToLower(s), "", true // a bare IPv6 address
	}
	if h, p, found := strings.Cut(s, ":"); found {
		port, ok = canonicalPort(p)
		return strings.ToLower(h), port, ok && h != ""
	}
	return strings.ToLower(s), "", true
}

// canonicalPort returns p as a decimal number without leading zeros, if it
// is a valid port.
func canonicalPort(p string) (string, bool) {
	if p == "" || len(p) > 5 || strings.Trim(p, "0123456789") != "" {
		return "", false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return strconv.Itoa(n), true
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// isLoopback reports whether host is one of the names plain http may carry
// a credential to.
func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// checkHeaderValue rejects a header value the transport would refuse or
// that could split the header. The error never quotes the value.
func checkHeaderValue(v string) error {
	if v == "" {
		return fmt.Errorf("%w: the credential header is empty", ErrRefused)
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return fmt.Errorf("%w: the credential header has a control character", ErrRefused)
		}
	}
	return nil
}

// transportError drops the *url.Error around err, whose text repeats the
// full URL, and makes a context error visible to errors.Is.
func transportError(ctx context.Context, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = fmt.Errorf("%w: %w", ctxErr, err)
	}
	return err
}

// buildError is an error of http.NewRequest without the raw URL it quotes.
func buildError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("invalid URL: %w", ue.Err)
	}
	return err
}
