package fake

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// gitServices are the smart HTTP endpoints the server answers, after
// "/<repository path>.git".
var gitServices = []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"}

// maxGitBody bounds a chunked request body, which CGI cannot stream and
// the server reads into memory first.
const maxGitBody = 256 << 20

// splitGitPath splits "/<repository path>.git<service>" into its parts.
func splitGitPath(urlPath string) (repo, service string, ok bool) {
	for _, svc := range gitServices {
		rest, found := strings.CutSuffix(urlPath, ".git"+svc)
		if found && len(rest) > 1 && rest[0] == '/' {
			return rest[1:], svc, true
		}
	}
	return "", "", false
}

// remoteURL is the URL of s: its git server URL in git mode. Called with
// mu held.
func (p *Platform) remoteURL(s *repoState) string {
	if p.git == nil {
		return "fake://" + p.host + "/" + s.repo.Path
	}
	segs := strings.Split(s.repo.Path, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return p.git.server.URL + "/" + strings.Join(segs, "/") + ".git"
}

// readOnlyPrefix marks the read-only form of an account's token that
// Reader.Remote and Writer.Remote send on GitHub-like flavors.
const readOnlyPrefix = "ro."

// gitHeader returns the header source of Reader.Remote and Writer.Remote
// for the account with id, nil when it has none. On GitHub-like flavors it
// sends a read-only form of the account's token, as an App's installation
// token for reading is (writes go through Target); on the
// others the token itself, a personal access token. Called with mu held.
func (p *Platform) gitHeader(id string) func(context.Context) (string, error) {
	if p.gitTokens[id] == "" {
		return nil
	}
	prefix := ""
	if p.appTokens() {
		prefix = readOnlyPrefix
	}
	return func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		a, token := p.accounts[id], p.gitTokens[id]
		if a == nil || token == "" {
			return "", &platform.Error{Op: "git credentials", Class: platform.ClassAuth,
				Err: errors.New("the account has no token")}
		}
		return "Basic " + basic(a.Login, prefix+token), nil
	}
}

// appTokens reports whether the flavor is GitHub-like: identities are Apps
// whose tokens carry permissions (every flavor but GitLab, Gitea and
// Forgejo). Called with mu held.
func (p *Platform) appTokens() bool {
	switch Flavor(p.caps.Flavor) {
	case GitLab, Gitea, Forgejo:
		return false
	}
	return true
}

// basic encodes Basic credentials.
func basic(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// gitRequest is an admitted request of the git server.
type gitRequest struct {
	account platform.Account
	repoID  string
	path    string // the repository path, for the call log
	dir     string // the bare repository
	// denyWorkflows makes the repository's pre-receive hook refuse a push
	// that creates or changes a file under .github/workflows (branch is
	// the default branch, the base of a new branch).
	denyWorkflows bool
	branch        string
	// signed, noForce, noDelete and noPush are the branch patterns of the
	// rulesets that hold the pusher (rules.go): signatures required, force
	// pushes refused, deletions refused, every push refused.
	signed, noForce, noDelete, noPush []string
}

// credential is who a request's Basic credentials name: an account, with
// its own token (a person's or a personal access token), its read-only
// form (readOnly), or a per-target token (target).
type credential struct {
	account  platform.Account
	readOnly bool
	target   *target
}

// handle serves one request of the git server.
func (s *GitServer) handle(w http.ResponseWriter, r *http.Request) {
	repoPath, svc, ok := splitGitPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	service, method := strings.TrimPrefix(svc, "/"), http.MethodPost
	if svc == "/info/refs" {
		service, method = r.URL.Query().Get("service"), http.MethodGet
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "the fake serves the smart HTTP protocol only", http.StatusForbidden)
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	push := service == "git-receive-pack"
	req, status, msg := s.p.admit(r, repoPath, push)
	if status != 0 {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake git"`)
		}
		http.Error(w, msg, status)
		return
	}
	switch {
	case push && svc == "/git-receive-pack":
		s.push(w, r, req)
	case !push && svc == "/info/refs":
		// One per fetch or ls-remote: git starts every one here.
		if err := s.p.gitCall("Fetch", req.path); err != nil {
			fail(w, err)
			return
		}
		s.backend(w, r, req, svc)
	default:
		s.backend(w, r, req, svc)
	}
}

// admit authenticates a request and finds its repository. A refused
// request gets an HTTP status and a message: 401 without a valid token or
// with a revoked one; 404 for an unknown repository, or one the per-target
// token was not minted for; 403 for a push by an identity without write
// access (the account's contents grant, and for a per-target token
// Contents in its perms; never with a read-only token), or to an archived
// repository.
//
// A push by an identity that may not change workflows (on flavors with
// Caps.WorkflowPerm: the account's workflows grant, and for a per-target
// token Workflows in its perms) is admitted with denyWorkflows: the
// repository's pre-receive hook then refuses one that creates or changes a
// file under .github/workflows.
func (p *Platform) admit(r *http.Request, repoPath string, push bool) (gitRequest, int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return gitRequest{}, http.StatusInternalServerError, "broken fixture"
	}
	c, ok := p.authenticate(r)
	switch {
	case !ok:
		return gitRequest{}, http.StatusUnauthorized, "authentication failed"
	case c.target != nil && c.target.closed:
		return gitRequest{}, http.StatusUnauthorized, "the token was revoked"
	}
	a := c.account
	st := p.repoByPathLocked(repoPath)
	if st == nil || st.dir == "" || (c.target != nil && c.target.repoID != st.repo.ID) {
		return gitRequest{}, http.StatusNotFound, "repository not found"
	}
	req := gitRequest{account: a, repoID: st.repo.ID, path: st.repo.Path, dir: st.dir, branch: st.repo.DefaultBranch}
	if push {
		grant := st.grants[a.ID]
		denied := "Permission to " + st.repo.Path + ".git denied to " + a.Login + "."
		switch {
		case c.readOnly, !grant.Contents, c.target != nil && !c.target.perms.Contents:
			return gitRequest{}, http.StatusForbidden, denied
		case st.repo.Archived:
			return gitRequest{}, http.StatusForbidden, st.repo.Path + " is archived and read-only"
		}
		workflows := grant.Workflows && (c.target == nil || c.target.perms.Workflows)
		req.denyWorkflows = p.caps.WorkflowPerm && !workflows
		req.signed, req.noForce, req.noDelete, req.noPush = p.hookRules(st, a)
	}
	return req, 0, ""
}

// authenticate returns who the request's Basic credentials name: a
// per-target token (with any login), the read-only form of an account's
// token, or an account's token. Called with mu held.
func (p *Platform) authenticate(r *http.Request) (credential, bool) {
	login, token, ok := r.BasicAuth()
	if !ok || token == "" {
		return credential{}, false
	}
	if t := p.targetTokens[token]; t != nil {
		return credential{account: p.refresh(t.as), target: t}, true
	}
	a := p.accountByLogin(login)
	if a == nil {
		return credential{}, false
	}
	want := p.gitTokens[a.ID]
	if want == "" {
		return credential{}, false
	}
	if rest, ro := strings.CutPrefix(token, readOnlyPrefix); ro && subtle.ConstantTimeCompare([]byte(want), []byte(rest)) == 1 {
		return credential{account: *a, readOnly: true}, true
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(token)) != 1 {
		return credential{}, false
	}
	return credential{account: *a}, true
}

// gitCall logs a git request as a call of method and returns the fault it
// meets, if any.
func (p *Platform) gitCall(method string, args ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logCall(method, args...)
	if f, ok := p.takeFault(method); ok {
		return f.err
	}
	return nil
}

// push serves a push: under gitMu, so the branches it moves are known
// exactly, and answered only once the platform has followed them, so a
// client that goes on to the API sees them.
func (s *GitServer) push(w http.ResponseWriter, r *http.Request, req gitRequest) {
	p := s.p
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	p.mu.Lock()
	st := p.repos[req.repoID]
	f, faulted := p.takeFault("Push")
	if faulted && !f.applied {
		p.logCall("Push", req.path)
		p.mu.Unlock()
		fail(w, f.err)
		return
	}
	before := maps.Clone(st.refs)
	rg := p.repoGit(st)
	p.mu.Unlock()
	rec := &recorder{header: http.Header{}}
	s.backend(rec, r, req, "/git-receive-pack")

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	after, err := rg.branches(ctx)
	p.mu.Lock()
	var changes []refChange
	if err == nil {
		changes = diffRefs(before, after)
		err = p.refsMoved(ctx, st, changes, &req.account, true, false)
	}
	if err != nil {
		p.setupf("push to %s: %v", req.path, err)
	}
	args := []string{req.path}
	for _, c := range changes {
		args = append(args, c.branch)
	}
	p.logCall("Push", args...)
	p.mu.Unlock()
	if faulted {
		fail(w, f.err)
		return
	}
	rec.flush(w)
}

// backend runs git http-backend for a request, as a CGI program, on the
// bare repository of req.
func (s *GitServer) backend(w http.ResponseWriter, r *http.Request, req gitRequest, svc string) {
	in := r.Clone(r.Context())
	in.URL.Path = "/" + filepath.Base(req.dir) + svc
	in.URL.RawPath = ""
	// The backend needs no credentials: they stay out of its environment.
	in.Header.Del("Authorization")
	if len(r.TransferEncoding) > 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxGitBody+1))
		switch {
		case err != nil:
			http.Error(w, "cannot read the request", http.StatusBadRequest)
			return
		case len(body) > maxGitBody:
			http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		in.Body, in.ContentLength, in.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	}
	g := s.p.git
	env := append(slices.Clone(g.env),
		"GIT_PROJECT_ROOT="+g.dir,
		"GIT_HTTP_EXPORT_ALL=1",
		"REMOTE_USER="+req.account.Login)
	if req.denyWorkflows {
		env = append(env, hookDenyWorkflows+"=1", hookDefaultBranch+"="+req.branch)
	}
	if len(req.signed) > 0 {
		env = append(env, hookSigned+"="+strings.Join(req.signed, " "))
	}
	if len(req.noForce) > 0 {
		env = append(env, hookNoForce+"="+strings.Join(req.noForce, " "))
	}
	if len(req.noDelete) > 0 {
		env = append(env, hookNoDelete+"="+strings.Join(req.noDelete, " "))
	}
	if len(req.noPush) > 0 {
		env = append(env, hookNoPush+"="+strings.Join(req.noPush, " "))
	}
	h := &cgi.Handler{
		Path:   g.bin,
		Dir:    g.dir,
		Args:   []string{"http-backend"},
		Env:    env,
		Logger: s.log,
		Stderr: io.Discard,
	}
	h.ServeHTTP(w, in)
}

// fail answers a request that met an injected fault: the Status of a
// *platform.Error in err, 500 otherwise.
func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var pe *platform.Error
	if errors.As(err, &pe) && pe.Status >= 400 && pe.Status <= 599 {
		status = pe.Status
	}
	http.Error(w, "fake: injected failure", status)
}

// recorder keeps a response to send later.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}

// flush sends the recorded response.
func (r *recorder) flush(w http.ResponseWriter) {
	maps.Copy(w.Header(), r.header)
	w.WriteHeader(max(r.status, http.StatusOK))
	_, _ = w.Write(r.body.Bytes())
}
