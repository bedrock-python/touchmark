package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sync"
	"time"
)

// The GitHub App manifest flow (docs.github.com/en/apps/sharing-github-apps/
// registering-a-github-app-from-a-manifest): a page POSTs the manifest to
// GitHub's "new App" page of the owner, the person confirms the App there
// (the browser step setup cannot skip), GitHub redirects to the
// manifest's redirect_url with a one-hour code, and POST
// /app-manifests/{code}/conversions returns the App's id and private key.
//
// setup serves the page and the redirect on 127.0.0.1 itself, for the
// length of the flow: one listener on a loopback address (a random port by
// default), answering GET only, to requests whose Host is that address (a
// DNS-rebound name is refused), with a random state per App checked in
// constant time, no-store, no-referrer and a CSP that lets the page post to
// GitHub only. The code is exchanged by setup, not by the page, and the
// private key never reaches the browser.

// appManifest is the manifest of one App.
type appManifest struct {
	Name           string            `json:"name"`
	URL            string            `json:"url"`
	Description    string            `json:"description"`
	HookAttributes hookAttributes    `json:"hook_attributes"`
	RedirectURL    string            `json:"redirect_url"`
	Public         bool              `json:"public"`
	Permissions    map[string]string `json:"default_permissions"`
	Events         []string          `json:"default_events"`
	RequestOAuth   bool              `json:"request_oauth_on_install"`
	SetupOnUpdate  bool              `json:"setup_on_update"`
}

// hookAttributes is the manifest's webhook: inactive (touchmark takes no
// events), with the hub's URL because the manifest requires one.
type hookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// conversion is POST /app-manifests/{code}/conversions's answer.
type conversion struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	HTMLURL     string            `json:"html_url"`
	ClientID    string            `json:"client_id"`
	Permissions map[string]string `json:"permissions"`
	Owner       struct {
		Login string `json:"login"`
	} `json:"owner"`
	PEM           string  `json:"pem"`
	ClientSecret  string  `json:"client_secret"`
	WebhookSecret *string `json:"webhook_secret"`
}

// codeRe bounds the code GitHub hands back.
var codeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// flowRequest is one App the flow creates.
type flowRequest struct {
	label    string // "reader" or "writer"
	manifest appManifest
	state    string
	newApp   string // GitHub's "new App" URL with the state
	// result carries the page shown after the exchange; received is set
	// once a code came for it (a second one is refused).
	result   chan string
	received bool
}

// manifestFlow serves the pages of one setup run.
type manifestFlow struct {
	addr   string // the listener's host:port
	origin string // GitHub's web origin, for the page's CSP

	mu      sync.Mutex
	current *flowRequest
	codes   chan string
	done    bool
}

// newState returns 16 random bytes in hex.
func newState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// checkLoopback refuses a listen address that is not a loopback one.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--listen %q: the manifest flow listens on a loopback address only, like 127.0.0.1:0", addr)
	}
	return nil
}

// ServeHTTP answers the page (GET /) and GitHub's redirect (GET
// /callback).
func (f *manifestFlow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action "+f.origin)
	if r.Host != f.addr {
		http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/":
		f.page(w)
	case "/callback":
		f.callback(w, r)
	default:
		http.NotFound(w, r)
	}
}

// writePage writes an HTML page.
func writePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>%s</title>"+
		"<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem}"+
		"button{font:inherit;padding:.5rem 1rem}code{background:#eee;padding:0 .2rem}</style></head>"+
		"<body><h1>%s</h1>\n%s\n</body></html>\n", html.EscapeString(title), html.EscapeString(title), body)
}

// page offers the form of the App to create next.
func (f *manifestFlow) page(w http.ResponseWriter) {
	f.mu.Lock()
	cur, done := f.current, f.done
	f.mu.Unlock()
	if cur == nil {
		msg := "<p>Nothing to create right now. Return to the terminal.</p>"
		if done {
			msg = "<p>The Apps are created. Return to the terminal.</p>"
		}
		writePage(w, http.StatusOK, "touchmark setup", msg)
		return
	}
	data, err := json.Marshal(cur.manifest)
	if err != nil {
		http.Error(w, "manifest", http.StatusInternalServerError)
		return
	}
	perms := ""
	for _, k := range sortedKeys(cur.manifest.Permissions) {
		perms += fmt.Sprintf("<li><code>%s</code>: %s</li>", html.EscapeString(k), html.EscapeString(cur.manifest.Permissions[k]))
	}
	body := fmt.Sprintf("<p>touchmark creates the <strong>%s</strong> App <code>%s</code> with these permissions:</p><ul>%s</ul>"+
		"<p>GitHub asks you to confirm the App. Keep the name or change it; leave the permissions as they are.</p>"+
		"<form action=\"%s\" method=\"post\"><input type=\"hidden\" name=\"manifest\" value=\"%s\">"+
		"<button type=\"submit\">Create the %s App on GitHub</button></form>",
		html.EscapeString(cur.label), html.EscapeString(cur.manifest.Name), perms,
		html.EscapeString(cur.newApp), html.EscapeString(string(data)), html.EscapeString(cur.label))
	writePage(w, http.StatusOK, "touchmark setup: the "+cur.label+" App", body)
}

// callback takes GitHub's redirect: it checks the state, hands the code to
// the flow and shows what came of it.
func (f *manifestFlow) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code, state := q.Get("code"), q.Get("state")
	f.mu.Lock()
	cur := f.current
	switch {
	case cur == nil || subtle.ConstantTimeCompare([]byte(state), []byte(cur.state)) != 1:
		f.mu.Unlock()
		writePage(w, http.StatusBadRequest, "Unknown request", "<p>This link does not belong to the App touchmark is creating now. Return to the terminal.</p>")
		return
	case !codeRe.MatchString(code):
		f.mu.Unlock()
		writePage(w, http.StatusBadRequest, "No code", "<p>GitHub sent no usable code. Start again from the touchmark page.</p>")
		return
	case cur.received:
		f.mu.Unlock()
		writePage(w, http.StatusConflict, "Already received", "<p>touchmark already received this App. Return to the terminal.</p>")
		return
	}
	cur.received = true
	f.mu.Unlock()
	f.codes <- code
	select {
	case msg := <-cur.result:
		writePage(w, http.StatusOK, "touchmark setup", msg)
	case <-r.Context().Done():
	}
}

// runFlow creates the Apps of reqs one after the other through the
// manifest flow: it listens on listen, tells the person the page's URL
// (tell), and for each App waits for GitHub's code and exchanges it
// (exchange). It returns the conversions in reqs' order; the first error
// ends the flow.
func runFlow(ctx context.Context, listen, webOrigin string, reqs []*flowRequest, tell func(string),
	exchange func(context.Context, *flowRequest, string) (conversion, error)) ([]conversion, error) {
	if err := checkLoopback(listen); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s for the manifest flow: %w", listen, err)
	}
	f := &manifestFlow{addr: ln.Addr().String(), origin: webOrigin, codes: make(chan string, 1)}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ln)
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-served
	}()
	base := "http://" + f.addr
	for _, req := range reqs {
		req.manifest.RedirectURL = base + "/callback"
		u, err := url.Parse(req.newApp)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("state", req.state)
		u.RawQuery = q.Encode()
		req.newApp = u.String()
		req.result = make(chan string, 1)
	}
	f.mu.Lock()
	f.current = reqs[0]
	f.mu.Unlock()
	tell(base + "/")
	var out []conversion
	for i, req := range reqs {
		var code string
		select {
		case code = <-f.codes:
		case <-ctx.Done():
			return out, fmt.Errorf("waiting for GitHub to create the %s App: %w", req.label, ctx.Err())
		}
		c, err := exchange(ctx, req, code)
		// The next App's page is ready before the person reads this one.
		f.mu.Lock()
		switch {
		case err != nil:
			f.current = nil
		case i+1 < len(reqs):
			f.current = reqs[i+1]
		default:
			f.current, f.done = nil, true
		}
		f.mu.Unlock()
		if err != nil {
			req.result <- "<p>touchmark could not finish the App: " + html.EscapeString(err.Error()) + ". Return to the terminal.</p>"
			return out, err
		}
		msg := fmt.Sprintf("<p>Created the %s App <code>%s</code>.</p>", html.EscapeString(req.label), html.EscapeString(c.Slug))
		if i+1 < len(reqs) {
			msg += "<p><a href=\"/\">Continue with the next App</a>.</p>"
		} else {
			msg += "<p>Return to the terminal.</p>"
		}
		req.result <- msg
		out = append(out, c)
	}
	return out, nil
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
