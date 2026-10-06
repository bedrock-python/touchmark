package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/redact"
)

// server is an httptest server that counts the requests it receives.
type server struct {
	*httptest.Server
	hits atomic.Int64
	pool *x509.CertPool // trusts the server (TLS only)
}

func newServer(t *testing.T, tlsOn bool, h http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		h(w, r)
	})
	if tlsOn {
		s.Server = httptest.NewTLSServer(counted)
		s.pool = x509.NewCertPool()
		s.pool.AddCert(s.Certificate())
	} else {
		s.Server = httptest.NewServer(counted)
	}
	t.Cleanup(s.Close)
	return s
}

// host returns the server's "host:port".
func (s *server) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// reply writes s as a response body; a client that went away is not an
// error of the test.
func reply(w io.Writer, s string) { _, _ = io.WriteString(w, s) }

func bearer(token string, hosts ...string) *Auth {
	return &Auth{Hosts: hosts, Header: func(context.Context) (string, error) { return "Bearer " + token, nil }}
}

// noNetwork is a transport that fails the test if a request reaches it.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("request to %s reached the transport", r.URL)
	return nil, errors.New("no network in this test")
}

func get(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAuthToAllowedHost(t *testing.T) {
	t.Parallel()
	var gotAuth, gotUA, gotAccept string
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA, gotAccept = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		reply(w, `{"sha":"abc"}`)
	})
	c := New(Options{RootCAs: srv.pool})
	var out struct{ SHA string }
	resp, err := c.JSON(t.Context(), http.MethodGet, srv.URL+"/repos/a/b", bearer("tok-123", srv.host(t)), nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK || out.SHA != "abc" {
		t.Errorf("status %d, decoded %+v", resp.Status, out)
	}
	if gotAuth != "Bearer tok-123" || gotUA != "touchmark" || gotAccept != "application/json" {
		t.Errorf("server saw Authorization %q, User-Agent %q, Accept %q", gotAuth, gotUA, gotAccept)
	}
}

// TestAuthHeaderName: a credential goes in the header Auth.Name names
// (GitLab's PRIVATE-TOKEN), under its canonical name, and nowhere else.
func TestAuthHeaderName(t *testing.T) {
	t.Parallel()
	var gotAuth, gotPrivate string
	srv := newServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPrivate = r.Header.Get("Authorization"), r.Header.Get("Private-Token")
	})
	c := New(Options{})
	auth := &Auth{Hosts: []string{srv.host(t)}, Name: "PRIVATE-TOKEN",
		Header: func(context.Context) (string, error) { return "private-test-value", nil }}
	if _, err := c.Do(get(t, srv.URL+"/api/v4/user"), auth); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" || gotPrivate != "private-test-value" {
		t.Errorf("server saw Authorization %q, Private-Token %q", gotAuth, gotPrivate)
	}
}

func TestAuthCaseInsensitiveHosts(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := newServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	})
	port := srv.host(t)[strings.LastIndex(srv.host(t), ":")+1:]
	c := New(Options{})
	_, err := c.Do(get(t, "http://LocalHost:"+port+"/x"), bearer("tok", "LOCALHOST:"+port))
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("server saw Authorization %q", gotAuth)
	}
}

func TestAuthRefusedForOtherHost(t *testing.T) {
	t.Parallel()
	srv := newServer(t, true, func(http.ResponseWriter, *http.Request) {})
	c := New(Options{RootCAs: srv.pool})
	for _, hosts := range [][]string{nil, {"api.example.com"}, {"127.0.0.2:" + strings.Split(srv.host(t), ":")[1]}, {"127.0.0.1"}} {
		_, err := c.Do(get(t, srv.URL+"/x"), bearer("tok", hosts...))
		if !errors.Is(err, ErrRefused) {
			t.Errorf("hosts %q: error %v, want ErrRefused", hosts, err)
		}
	}
	if n := srv.hits.Load(); n != 0 {
		t.Errorf("the server received %d requests", n)
	}
}

func TestHostAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		hosts []string
		url   string
		want  bool
	}{
		{[]string{"api.github.com"}, "https://api.github.com/x", true},
		{[]string{"api.github.com"}, "https://API.GitHub.com:443/x", true},
		{[]string{"API.GITHUB.COM:443"}, "https://api.github.com/x", true},
		{[]string{"api.github.com:0443"}, "https://api.github.com/x", true},
		{[]string{"api.github.com"}, "https://api.github.com:8443/x", false},
		{[]string{"api.github.com:8443"}, "https://api.github.com:8443/x", true},
		{[]string{"api.github.com:443"}, "https://api.github.com:8443/x", false},
		{[]string{"api.github.com"}, "https://api.github.com.evil.example/x", false},
		{[]string{"github.com"}, "https://api.github.com/x", false},
		{[]string{"example.com", "api.github.com"}, "https://api.github.com/x", true},
		{[]string{"[::1]:8443"}, "https://[::1]:8443/x", true},
		{[]string{"[::1]"}, "https://[::1]/x", true},
		{[]string{"::1"}, "https://[::1]/x", true},
		{[]string{"::1"}, "https://[::1]:8443/x", false},
		{[]string{"localhost"}, "http://localhost/x", true},
		{[]string{"localhost"}, "http://localhost:8080/x", false},
		{[]string{"https://api.github.com"}, "https://api.github.com/x", false},
		{[]string{"api.github.com/"}, "https://api.github.com/x", false},
		{[]string{"api.github.com:99999"}, "https://api.github.com/x", false},
		{[]string{"user@api.github.com"}, "https://api.github.com/x", false},
		{[]string{""}, "https://api.github.com/x", false},
		{nil, "https://api.github.com/x", false},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.url)
		if err != nil {
			t.Fatal(err)
		}
		host, port, ok := urlHost(u)
		if !ok {
			t.Fatalf("urlHost(%q) failed", tt.url)
		}
		if got := hostAllowed(tt.hosts, host, port, u.Scheme); got != tt.want {
			t.Errorf("hostAllowed(%q, %q) = %v, want %v", tt.hosts, tt.url, got, tt.want)
		}
	}
}

func TestRefusedRequestsNeverReachTheTransport(t *testing.T) {
	t.Parallel()
	c := New(Options{Transport: noNetwork{t}})
	auth := bearer("tok", "example.com", "127.0.0.1:1234")
	withHeader := func(rawURL, key, value string) *http.Request {
		req := get(t, rawURL)
		req.Header[key] = []string{value} // also non-canonical keys
		return req
	}
	withHost := func(rawURL, host string) *http.Request {
		req := get(t, rawURL)
		req.Host = host
		return req
	}
	tests := []struct {
		name string
		req  *http.Request
		auth *Auth
	}{
		{"plain http to a remote host", get(t, "http://example.com/x"), auth},
		{"credentials in the URL", get(t, "https://user:pass@example.com/x"), auth},
		{"credentials in the URL, anonymous", get(t, "https://user:pass@example.com/x"), nil},
		{"another scheme", get(t, "ftp://example.com/x"), auth},
		{"no host", &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Path: "/x"}, Header: http.Header{}}, auth},
		{"Host header differs", withHost("https://example.com/x", "evil.example"), auth},
		{"anonymous with Authorization", withHeader("https://example.com/x", "Authorization", "Bearer x"), nil},
		{"anonymous with a lowercase authorization", withHeader("https://example.com/x", "authorization", "Bearer x"), nil},
		{"anonymous with PRIVATE-TOKEN", withHeader("https://example.com/x", "PRIVATE-TOKEN", "x"), nil},
		{"anonymous with Job-Token", withHeader("https://example.com/x", "Job-Token", "x"), nil},
		{"credential without a header func", get(t, "https://example.com/x"), &Auth{Hosts: []string{"example.com"}}},
		{"empty header value", get(t, "https://example.com/x"), &Auth{Hosts: []string{"example.com"}, Header: func(context.Context) (string, error) { return "", nil }}},
		{"credential in another header", get(t, "https://example.com/x"), &Auth{Hosts: []string{"example.com"}, Name: "X-Api-Key",
			Header: func(context.Context) (string, error) { return "x", nil }}},
		{"credential in the proxy header", get(t, "https://example.com/x"), &Auth{Hosts: []string{"example.com"}, Name: "Proxy-Authorization",
			Header: func(context.Context) (string, error) { return "x", nil }}},
	}
	for _, tt := range tests {
		_, err := c.Do(tt.req, tt.auth)
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: error %v, want ErrRefused", tt.name, err)
		}
	}
	if _, err := c.Do(nil, nil); !errors.Is(err, ErrRefused) {
		t.Errorf("nil request: error %v, want ErrRefused", err)
	}
}

// trackedBody records whether it was closed.
type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }

func TestRefusedRequestBodyIsClosed(t *testing.T) {
	t.Parallel()
	c := New(Options{Transport: noNetwork{t}})
	for _, auth := range []*Auth{
		bearer("tok", "other.example"),
		{Hosts: []string{"example.com"}, Header: func(context.Context) (string, error) { return "", errors.New("mint failed") }},
	} {
		body := &trackedBody{Reader: strings.NewReader("{}")}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/x", body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Do(req, auth); err == nil {
			t.Fatal("the request was not refused")
		}
		if !body.closed.Load() {
			t.Error("the body of a refused request was not closed")
		}
	}
}

func TestPlainHTTPToLoopback(t *testing.T) {
	t.Parallel()
	srv := newServer(t, false, func(http.ResponseWriter, *http.Request) {})
	port := srv.host(t)[strings.LastIndex(srv.host(t), ":")+1:]
	c := New(Options{})
	for _, host := range []string{"127.0.0.1", "localhost"} {
		_, err := c.Do(get(t, "http://"+host+":"+port+"/x"), bearer("tok", host+":"+port))
		if err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	// [::1] passes the rules; the server may not listen on IPv6.
	req := get(t, "http://[::1]:"+port+"/x")
	if err := checkRequest(req, bearer("tok", "[::1]:"+port)); err != nil {
		t.Errorf("[::1]: %v", err)
	}
}

func TestNoRedirectWithAuth(t *testing.T) {
	t.Parallel()
	other := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the other host received %s with Authorization %q", r.URL, r.Header.Get("Authorization"))
	})
	var landed atomic.Int64
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		case "/same":
			http.Redirect(w, r, "/landing", http.StatusTemporaryRedirect)
		case "/landing":
			landed.Add(1)
		}
	})
	c := New(Options{RootCAs: srv.pool})
	auth := bearer("tok", srv.host(t), other.host(t))
	for path, status := range map[string]int{"/away": http.StatusFound, "/same": http.StatusTemporaryRedirect} {
		resp, err := c.Do(get(t, srv.URL+path), auth)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.Status != status {
			t.Errorf("%s: status %d, want the redirect %d returned as is", path, resp.Status, status)
		}
	}
	if other.hits.Load() != 0 || landed.Load() != 0 {
		t.Errorf("a redirect was followed: other host %d, landing %d", other.hits.Load(), landed.Load())
	}
	// JSON reports the redirect as a status error.
	_, err := c.JSON(t.Context(), http.MethodGet, srv.URL+"/away", auth, nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusFound || se.Header.Get("Location") == "" {
		t.Errorf("JSON error %v, want a StatusError 302 with Location", err)
	}
}

func TestAnonymousRedirects(t *testing.T) {
	t.Parallel()
	other := newServer(t, true, func(http.ResponseWriter, *http.Request) {})
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/r/"):
			var n int
			if _, err := fmt.Sscanf(r.URL.Path, "/r/%d", &n); err != nil {
				http.NotFound(w, r)
				return
			}
			if n == 0 {
				reply(w, "done")
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/r/%d", n-1), http.StatusFound)
		case r.URL.Path == "/away":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case r.URL.Path == "/downgrade":
			http.Redirect(w, r, "http://"+r.Host+"/r/0", http.StatusFound)
		case r.URL.Path == "/userinfo":
			http.Redirect(w, r, "https://user:pass@"+r.Host+"/r/0", http.StatusFound)
		}
	})
	c := New(Options{RootCAs: srv.pool})
	tests := []struct {
		path   string
		status int
		body   string
	}{
		{"/r/5", http.StatusOK, "done"},      // five redirects are followed
		{"/r/6", http.StatusFound, ""},       // the sixth is returned
		{"/away", http.StatusFound, ""},      // another host
		{"/downgrade", http.StatusFound, ""}, // another scheme
		{"/userinfo", http.StatusFound, ""},  // credentials in the location
	}
	for _, tt := range tests {
		resp, err := c.Do(get(t, srv.URL+tt.path), nil)
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		if resp.Status != tt.status || (tt.body != "" && string(resp.Body) != tt.body) {
			t.Errorf("%s: status %d body %q, want %d %q", tt.path, resp.Status, resp.Body, tt.status, tt.body)
		}
	}
	if n := other.hits.Load(); n != 0 {
		t.Errorf("the other host received %d requests", n)
	}
}

func TestMaxBody(t *testing.T) {
	t.Parallel()
	const limit = 1024
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exact":
			reply(w, strings.Repeat("a", limit))
		case "/over":
			w.Header().Set("Content-Length", fmt.Sprint(limit+1))
			reply(w, strings.Repeat("a", limit+1))
		case "/chunked":
			// No Content-Length: the limit is found while reading.
			reply(w, strings.Repeat("a", limit/2))
			w.(http.Flusher).Flush()
			reply(w, strings.Repeat("a", limit))
		}
	})
	c := New(Options{RootCAs: srv.pool, MaxBody: limit})
	resp, err := c.Do(get(t, srv.URL+"/exact"), nil)
	if err != nil || len(resp.Body) != limit {
		t.Fatalf("exact: %v", err)
	}
	for _, path := range []string{"/over", "/chunked"} {
		_, err := c.Do(get(t, srv.URL+path), nil)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: error %v, want ErrTooLarge", path, err)
		}
	}
	// HEAD announces the size of a body it does not send.
	head, err := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/over", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Do(head, nil); err != nil || len(resp.Body) != 0 {
		t.Errorf("HEAD: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	// The handler holds the response until the test ends. Returning when
	// the client goes away would race: the client's close could let the
	// rest of the response through and complete the body.
	release := make(chan struct{})
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-body" {
			reply(w, "partial")
			w.(http.Flusher).Flush()
		}
		<-release
	})
	t.Cleanup(func() { close(release) }) // runs before the server closes
	c := New(Options{RootCAs: srv.pool, Timeout: 100 * time.Millisecond})
	for _, path := range []string{"/slow-headers", "/slow-body"} {
		start := time.Now()
		_, err := c.Do(get(t, srv.URL+path), bearer("tok", srv.host(t)))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: error %v, want context.DeadlineExceeded", path, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: took %v", path, d)
		}
	}
}

func TestStatusErrorMasksEchoedToken(t *testing.T) {
	t.Parallel()
	const token = "ghs_EchoedTokenValue1234567890"
	reg := redact.New()
	reg.Add(token, "x-access-token")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "{\"message\": \"Bad credentials: %s\",\n \"basic\": \"%s\", \"query\": %q}",
			r.Header.Get("Authorization"), basic, r.URL.RawQuery)
	})
	c := New(Options{RootCAs: srv.pool, Redact: reg})
	_, err := c.JSON(t.Context(), http.MethodGet, srv.URL+"/repos/a/b?page=2&token="+token, bearer(token, srv.host(t)), nil, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v, want *StatusError", err)
	}
	if se.Status != http.StatusUnauthorized || se.Method != http.MethodGet {
		t.Errorf("status %d method %s", se.Status, se.Method)
	}
	if strings.Contains(se.URL, "?") || strings.Contains(se.URL, "page=") {
		t.Errorf("URL %q keeps the query", se.URL)
	}
	msg := err.Error()
	for _, secret := range []string{token, basic} {
		if strings.Contains(msg, secret) || strings.Contains(se.Snippet, secret) {
			t.Errorf("error leaks %q: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "Bad credentials: Bearer ***") || !strings.Contains(msg, "401 Unauthorized") {
		t.Errorf("error = %s", msg)
	}
	if strings.ContainsAny(se.Snippet, "\n\r") {
		t.Errorf("snippet spans lines: %q", se.Snippet)
	}
}

func TestSnippet(t *testing.T) {
	t.Parallel()
	reg := redact.New()
	reg.Add("secret-in-the-middle")
	c := New(Options{Redact: reg})
	body := "  start\x1b[31m\xe2\x80\xaeevil\n\n\tline\xff " + strings.Repeat("λ", 600) + "secret-in-the-middle" + strings.Repeat("x", 3000)
	s := c.snippet([]byte(body))
	if len(s) > snippetLimit {
		t.Errorf("snippet has %d bytes", len(s))
	}
	if !utf8.ValidString(s) || !strings.HasSuffix(s, "...") || !strings.HasPrefix(s, "start [31m evil line� λ") {
		t.Errorf("snippet = %q", s[:min(len(s), 80)])
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || isBidi(r) {
			t.Fatalf("snippet has control character %U", r)
		}
	}
	// A secret beyond the cut is masked before the cut, not after.
	short := c.snippet([]byte(strings.Repeat("y", snippetLimit-10) + "secret-in-the-middle"))
	if strings.Contains(short, "secret-in") {
		t.Errorf("a cut secret leaks: %q", short[len(short)-30:])
	}
}

func TestErrorsNeverContainHeaderValues(t *testing.T) {
	t.Parallel()
	const value = "Bearer header-value-never-printed"
	// A transport that fails like a refused connection: no port is dialled,
	// so no other test's server can answer in its place.
	const base = "https://unreachable.example.com"
	host := "unreachable.example.com"

	c := New(Options{Transport: refused{}})
	auth := &Auth{Hosts: []string{host}, Header: func(context.Context) (string, error) { return value, nil }}
	_, err := c.Do(get(t, base+"/x"), auth)
	if err == nil || strings.Contains(err.Error(), "header-value-never-printed") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("connection error %v", err)
	}

	bad := &Auth{Hosts: []string{host}, Header: func(context.Context) (string, error) { return value + "\r\nX-Injected: 1", nil }}
	_, err = c.Do(get(t, base+"/x"), bad)
	if !errors.Is(err, ErrRefused) || strings.Contains(err.Error(), "never-printed") {
		t.Errorf("bad header error %v", err)
	}

	reg := redact.New()
	reg.Add("secret-in-an-error")
	c2 := New(Options{Redact: reg, Transport: refused{}})
	failing := &Auth{Hosts: []string{host}, Header: func(context.Context) (string, error) {
		return "", errors.New("mint failed for secret-in-an-error")
	}}
	_, err = c2.Do(get(t, base+"/x"), failing)
	if err == nil || strings.Contains(err.Error(), "secret-in-an-error") || !strings.Contains(err.Error(), "***") {
		t.Errorf("credential error %v", err)
	}
}

// refused is a transport whose every request fails as a refused
// connection would.
type refused struct{}

func (refused) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp 192.0.2.1:443: connect: connection refused")
}

func TestDoDoesNotChangeTheRequest(t *testing.T) {
	t.Parallel()
	srv := newServer(t, true, func(http.ResponseWriter, *http.Request) {})
	c := New(Options{RootCAs: srv.pool, UserAgent: "touchmark/0.2.0"})
	req := get(t, srv.URL+"/x")
	if _, err := c.Do(req, bearer("tok", srv.host(t))); err != nil {
		t.Fatal(err)
	}
	if len(req.Header) != 0 {
		t.Errorf("Do changed the caller's headers: %v", req.Header)
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			var in map[string]string
			if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&in) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]string{"got": in["name"], "ua": r.Header.Get("User-Agent")}); err != nil {
				t.Error(err)
			}
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/html":
			reply(w, "<html>not json</html>")
		}
	})
	c := New(Options{RootCAs: srv.pool, UserAgent: "touchmark/0.2.0"})
	var out map[string]string
	if _, err := c.JSON(t.Context(), http.MethodPost, srv.URL+"/echo", nil, map[string]string{"name": "hub"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["got"] != "hub" || out["ua"] != "touchmark/0.2.0" {
		t.Errorf("decoded %v", out)
	}
	if resp, err := c.JSON(t.Context(), http.MethodDelete, srv.URL+"/empty", nil, nil, &out); err != nil || resp.Status != http.StatusNoContent {
		t.Errorf("204: %v", err)
	}
	resp, err := c.JSON(t.Context(), "", srv.URL+"/html", nil, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "decode response") || resp == nil {
		t.Errorf("non-JSON body: %v", err)
	}
	if _, err := c.JSON(t.Context(), http.MethodPost, srv.URL+"/echo", nil, func() {}, nil); err == nil {
		t.Error("unencodable body: no error")
	}
	if _, err := c.JSON(t.Context(), http.MethodGet, "https://h/%zz?token=secret", nil, nil, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("bad URL: %v", err)
	}
}

// writeCA writes the server's certificate as a PEM file.
func writeCA(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCAFile(t *testing.T) {
	t.Parallel()
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) { reply(w, "ok") })
	pool, err := LoadCAFile(writeCA(t, srv.Certificate()))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := New(Options{RootCAs: pool}).Do(get(t, srv.URL), nil)
	if err != nil || string(resp.Body) != "ok" {
		t.Fatalf("request with the loaded pool: %v", err)
	}
	// Without the file, the test server's certificate is not trusted.
	if _, err := New(Options{}).Do(get(t, srv.URL), nil); err == nil {
		t.Error("the system roots trust the test server")
	}

	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := map[string]string{
		"missing": filepath.Join(dir, "missing.pem"),
		"empty":   write("empty.pem", "no pem here\n"),
		"garbage": write("garbage.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}))),
		"key":     write("key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("k")}))),
	}
	for name, path := range bad {
		if _, err := LoadCAFile(path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestTLSMinimum(t *testing.T) {
	t.Parallel()
	tr := New(Options{}).authed.Transport.(*http.Transport)
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x", tr.TLSClientConfig.MinVersion)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	if _, err := New(Options{RootCAs: pool}).Do(get(t, srv.URL), nil); err == nil {
		t.Error("a TLS 1.1 server was accepted")
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	if c.timeout != DefaultTimeout || c.maxBody != DefaultMaxBody || c.userAgent != "touchmark" {
		t.Errorf("defaults: %v %v %q", c.timeout, c.maxBody, c.userAgent)
	}
	se := &StatusError{Method: "GET", URL: "https://h/x", Status: 404}
	if got := se.Error(); got != "GET https://h/x: 404 Not Found" {
		t.Errorf("StatusError.Error() = %q", got)
	}
}

// TestJSONWith: more headers go out, Accept can be replaced, and a header
// that may carry a credential is refused before anything is sent.
func TestJSONWith(t *testing.T) {
	t.Parallel()
	srv := newServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]string{"accept": r.Header.Get("Accept"), "version": r.Header.Get("X-Api-Version")}); err != nil {
			t.Error(err)
		}
	})
	reg := redact.New()
	c := New(Options{RootCAs: srv.pool, Redact: reg})
	if c.Registry() != reg {
		t.Error("Registry is not the client's")
	}
	if New(Options{}).Registry() != nil {
		t.Error("a client without a registry has one")
	}
	var out map[string]string
	h := http.Header{"Accept": {"application/vnd.example+json"}, "X-Api-Version": {"2022-11-28"}}
	if _, err := c.JSONWith(t.Context(), http.MethodGet, srv.URL, nil, h, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out["accept"] != "application/vnd.example+json" || out["version"] != "2022-11-28" {
		t.Errorf("headers %v", out)
	}
	for _, name := range []string{"Authorization", "private-token", "Job-Token", "Proxy-Authorization"} {
		_, err := c.JSONWith(t.Context(), http.MethodGet, srv.URL, nil, http.Header{name: {"secret-value"}}, nil, nil)
		if !errors.Is(err, ErrRefused) || strings.Contains(err.Error(), "secret-value") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := srv.hits.Load(); n != 1 {
		t.Errorf("%d requests reached the server", n)
	}
}
