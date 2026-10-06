package ghfake

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// gitServices are the smart HTTP endpoints, after "/<owner>/<repo>.git".
var gitServices = []string{"/info/refs", "/git-upload-pack", "/git-receive-pack"}

// maxGitBody bounds a push or fetch request body.
const maxGitBody = 256 << 20

// splitGitPath splits "/<owner>/<repo>.git<service>" (escaped) into the
// repository path and the service.
func splitGitPath(p string) (repoPath, service string, ok bool) {
	for _, svc := range gitServices {
		rest, found := strings.CutSuffix(p, ".git"+svc)
		if !found || len(rest) < 2 || rest[0] != '/' {
			continue
		}
		segs := strings.Split(rest[1:], "/")
		if len(segs) != 2 {
			return "", "", false
		}
		for i, seg := range segs {
			u, err := url.PathUnescape(seg)
			if err != nil || u == "" {
				return "", "", false
			}
			segs[i] = u
		}
		return segs[0] + "/" + segs[1], svc, true
	}
	return "", "", false
}

// gitAuth authenticates a git request: HTTP Basic with any user name and a
// token as the password (git's "x-access-token:<token>" for installation
// tokens). No credentials is anonymous. Called with mu held.
func (s *Server) gitAuth(r *http.Request) (*identity, int, string) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		if r.Header.Get("Authorization") != "" {
			return nil, http.StatusUnauthorized, "Invalid username or token. Password authentication is not supported for Git operations."
		}
		return nil, 0, ""
	}
	_ = user
	t := s.tokens[pass]
	if t == nil || subtle.ConstantTimeCompare([]byte(t.value), []byte(pass)) != 1 {
		return nil, http.StatusUnauthorized, "Invalid username or token. Password authentication is not supported for Git operations."
	}
	if why := s.stale(t); why != "" {
		if r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			s.violate("stale-token %s: git push: the token is %s", tokenOwner(t), why)
		}
		return nil, http.StatusUnauthorized, "Invalid username or token. Password authentication is not supported for Git operations."
	}
	return &identity{kind: idToken, tok: t, who: t.user}, 0, ""
}

// serveGit answers git smart HTTP for a repository: fetches need Contents
// read (anyone on a public repository), pushes Contents write on a
// repository that is not archived. What the caller cannot see is 404
// "Repository not found." (observed read-only 2026-09-29 for an anonymous
// ls-remote of a missing repository; the same for private ones is
// assumed); an anonymous push is 401; a push without write access gets
// 403 "Permission to <repo>.git denied to <login>." The old path of a
// renamed repository redirects (assumed).
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request, repoPath, svc string) {
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
	s.mu.Lock()
	id, status, msg := s.gitAuth(r)
	who := identityKey(id)
	logged := func(route string, status int) {
		s.logRequest(r.Method, route, who, status)
	}
	route := "GIT fetch " + repoPath
	if push {
		route = "GIT push " + repoPath
	}
	fail := func(status int, msg string) {
		logged(route, status)
		s.mu.Unlock()
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		}
		http.Error(w, msg, status)
	}
	if status != 0 {
		fail(status, msg)
		return
	}
	key := strings.ToLower(repoPath)
	rp := s.byPath[key]
	if rp == nil {
		if rid, moved := s.redirects[key]; moved && s.repos[rid] != nil && s.canSee(id, s.repos[rid]) {
			loc := s.http.URL + "/" + escapePath(s.repos[rid].path()) + ".git" + svc
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			logged(route, http.StatusMovedPermanently)
			s.mu.Unlock()
			http.Redirect(w, r, loc, http.StatusMovedPermanently)
			return
		}
	}
	if rp != nil {
		s.noteScope(id, rp)
	}
	if rp == nil || !s.canSee(id, rp) {
		fail(http.StatusNotFound, "Repository not found.")
		return
	}
	if push {
		if id == nil {
			fail(http.StatusUnauthorized, "Authentication required")
			return
		}
		if refused := s.need(id, rp, "contents", Write); refused != nil {
			fail(http.StatusForbidden, "Permission to "+rp.path()+".git denied to "+id.who.login+".")
			return
		}
		if rp.archived {
			fail(http.StatusForbidden, "This repository was archived so it is read-only.")
			return
		}
	} else if refused := s.need(id, rp, "contents", Read); refused != nil {
		fail(http.StatusNotFound, "Repository not found.")
		return
	}
	if push && svc == "/git-receive-pack" {
		s.mu.Unlock()
		s.push(w, r, rp, id, route)
		return
	}
	if !push && svc == "/info/refs" {
		// One per fetch or ls-remote: git starts every one here.
		if f, faulted := s.takeFault("GIT fetch"); faulted {
			fail(faultStatus(f), "ghfake: injected failure")
			return
		}
		logged(route, http.StatusOK)
	}
	s.mu.Unlock()
	body, ok := readGitBody(w, r)
	if !ok {
		return
	}
	s.backend(w, r, rp, id, svc, body)
}

// faultStatus is the HTTP status of a fault.
func faultStatus(f *Fault) int {
	if f.Status == 0 {
		return http.StatusInternalServerError
	}
	return f.Status
}

// readGitBody reads a request body whole: CGI cannot stream a chunked one.
func readGitBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGitBody+1))
	switch {
	case err != nil:
		http.Error(w, "cannot read the request", http.StatusBadRequest)
		return nil, false
	case len(body) > maxGitBody:
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

// backend runs git http-backend on the bare repository of rp.
func (s *Server) backend(w http.ResponseWriter, r *http.Request, rp *repo, id *identity, svc string, body []byte) {
	in := r.Clone(r.Context())
	in.URL.Path = "/" + filepath.Base(rp.dir) + svc
	in.URL.RawPath = ""
	in.Header.Del("Authorization")
	in.Body, in.ContentLength, in.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	user := "anonymous"
	if id != nil {
		user = id.who.login
	}
	env := append(slices.Clone(s.git.env), "GIT_PROJECT_ROOT="+s.git.dir, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER="+user)
	h := &cgi.Handler{Path: s.git.bin, Dir: s.git.dir, Args: []string{"http-backend"}, Env: env,
		Logger: log.New(io.Discard, "", 0), Stderr: io.Discard}
	h.ServeHTTP(w, in)
}

// pushCmd is one ref update of a push.
type pushCmd struct {
	old, new, ref string
}

// parseReceive parses the commands of a receive-pack request (pkt-lines:
// shallow lines, "<old> <new> <ref>" with capabilities after a NUL on the
// first, a flush, push options when negotiated, then the pack) and
// returns them with the capabilities and the offset of the pack.
func parseReceive(data []byte) ([]pushCmd, []string, int, error) {
	var cmds []pushCmd
	var caps []string
	pos := 0
	readLine := func() (string, bool, error) {
		if pos+4 > len(data) {
			return "", false, errors.New("truncated pkt-line")
		}
		n, err := strconv.ParseUint(string(data[pos:pos+4]), 16, 16)
		if err != nil {
			return "", false, fmt.Errorf("bad pkt-line length %q", data[pos:pos+4])
		}
		if n == 0 {
			pos += 4
			return "", true, nil
		}
		if n < 4 || pos+int(n) > len(data) {
			return "", false, errors.New("bad pkt-line")
		}
		line := string(data[pos+4 : pos+int(n)])
		pos += int(n)
		return strings.TrimSuffix(line, "\n"), false, nil
	}
	for {
		line, flush, err := readLine()
		if err != nil {
			return nil, nil, 0, err
		}
		if flush {
			break
		}
		if strings.HasPrefix(line, "shallow ") {
			continue
		}
		if len(cmds) == 0 {
			var capPart string
			line, capPart, _ = strings.Cut(line, "\x00")
			caps = strings.Fields(capPart)
		}
		f := strings.Fields(line)
		if len(f) != 3 || !isHexID(f[0]) || !isHexID(f[1]) {
			return nil, nil, 0, fmt.Errorf("bad command %q", line)
		}
		cmds = append(cmds, pushCmd{old: strings.ToLower(f[0]), new: strings.ToLower(f[1]), ref: f[2]})
	}
	if slices.Contains(caps, "push-options") {
		for {
			_, flush, err := readLine()
			if err != nil {
				return nil, nil, 0, err
			}
			if flush {
				break
			}
		}
	}
	return cmds, caps, pos, nil
}

// push serves a push under gitMu: the pack is indexed into a quarantine
// first, so that each ref update meets the Workflows permission and the
// rulesets like on GitHub (refused updates answer "[remote rejected]"
// with GitHub's reason and remote lines, and nothing moves); an admitted
// push runs git http-backend, and the response is sent once pull requests
// have followed the refs.
func (s *Server) push(w http.ResponseWriter, r *http.Request, rp *repo, id *identity, route string) {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	body, ok := readGitBody(w, r)
	if !ok {
		return
	}
	plain := body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			plain, err = io.ReadAll(io.LimitReader(zr, maxGitBody+1))
		}
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), gitTimeout)
	defer cancel()
	s.mu.Lock()
	f, faulted := s.takeFault("GIT push")
	if faulted && !f.Applied {
		s.logRequest(r.Method, route, identityKey(id), faultStatus(f))
		s.mu.Unlock()
		http.Error(w, "ghfake: injected failure", faultStatus(f))
		return
	}
	rg := s.repoGit(rp)
	before, err := rg.refs(ctx, "refs/")
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "ghfake: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cmds, caps, packAt, perr := parseReceive(plain)
	var refusals map[string]*refusal
	if perr == nil {
		refusals, err = s.prePush(ctx, rp, id, cmds, plain[packAt:])
		if err != nil {
			http.Error(w, "ghfake: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if len(refusals) > 0 {
		s.mu.Lock()
		s.logRequest(r.Method, route, identityKey(id), http.StatusOK)
		s.mu.Unlock()
		s.refusePush(w, rp, id, cmds, caps, refusals)
		return
	}
	rec := &recorder{header: http.Header{}}
	s.backend(rec, r, rp, id, "/git-receive-pack", body)
	s.mu.Lock()
	after, err := rg.refs(ctx, "refs/")
	var changes []refChange
	if err == nil {
		changes = diffRefs(before, after)
		err = s.refsMoved(ctx, rp, changes, id.who, true)
	}
	s.limits.use(identityKey(id)).Pushes++
	var moved []string
	for _, c := range changes {
		moved = append(moved, c.ref)
	}
	status := max(rec.status, http.StatusOK)
	if faulted {
		status = faultStatus(f)
	}
	s.logRequest(r.Method, strings.TrimSpace(route+" "+strings.Join(moved, " ")), identityKey(id), status)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "ghfake: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if faulted {
		http.Error(w, "ghfake: injected failure", faultStatus(f))
		return
	}
	rec.flush(w)
}

// prePush indexes pack into a quarantine and judges every command. It
// returns the refusals by ref, none when the push may proceed.
func (s *Server) prePush(ctx context.Context, rp *repo, id *identity, cmds []pushCmd, pack []byte) (map[string]*refusal, error) {
	q, err := os.MkdirTemp(s.git.scratch, "quarantine-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(q)
	if err := os.MkdirAll(filepath.Join(q, "pack"), 0o755); err != nil {
		return nil, err
	}
	s.mu.Lock()
	qrg := s.quarantined(rp, q)
	s.mu.Unlock()
	if len(pack) > 0 {
		if _, err := qrg.g.Run(ctx, bytes.NewReader(pack), "index-pack", "--stdin", "--fix-thin"); err != nil {
			// A broken pack: http-backend refuses it itself.
			return nil, nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]*refusal{}
	for _, c := range cmds {
		if strings.HasPrefix(c.ref, "refs/pull/") {
			continue // receive.hideRefs refuses it: "deny updating a hidden ref"
		}
		ch := refChange{ref: c.ref, old: c.old, new: c.new}
		if isZeroID(ch.old) {
			ch.old = ""
		}
		if isZeroID(ch.new) {
			ch.new = ""
		}
		ref, err := s.checkUpdate(ctx, qrg, rp, id, ch)
		if err != nil {
			return nil, err
		}
		if ref != nil {
			out[c.ref] = ref
		}
	}
	return out, nil
}

// refusePush answers a refused push with report-status: "unpack ok" and
// "ng <ref> <reason>" for every command, the remote lines on side band 2.
// A GH013 refusal reads "push declined due to repository rule violations",
// a Workflows one GitHub's "refusing to allow …" message; with the atomic
// capability the other refs read "atomic push failure".
func (s *Server) refusePush(w http.ResponseWriter, rp *repo, id *identity, cmds []pushCmd, caps []string, refusals map[string]*refusal) {
	var lines []string
	first := ""
	for _, c := range cmds {
		ref := refusals[c.ref]
		if ref == nil {
			continue
		}
		reason := "push declined due to repository rule violations"
		if ref.workflow != "" {
			reason = workflowMessage(id, ref.workflow)
		} else {
			lines = append(lines, s.gh013Lines(rp, ref)...)
		}
		if first == "" {
			first = reason
		}
	}
	var status bytes.Buffer
	status.WriteString(pkt("unpack ok\n"))
	for _, c := range cmds {
		reason := first
		switch ref := refusals[c.ref]; {
		case ref != nil && ref.workflow != "":
			reason = workflowMessage(id, ref.workflow)
		case ref != nil:
			reason = "push declined due to repository rule violations"
		case slices.Contains(caps, "atomic"):
			reason = "atomic push failure"
		}
		status.WriteString(pkt("ng " + c.ref + " " + reason + "\n"))
	}
	status.WriteString("0000")
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	band := 0
	switch {
	case slices.Contains(caps, "side-band-64k"):
		band = 65515
	case slices.Contains(caps, "side-band"):
		band = 995
	}
	if band == 0 {
		_, _ = w.Write(status.Bytes())
		return
	}
	var out bytes.Buffer
	for _, l := range lines {
		msg := l + "\n"
		for len(msg) > 0 {
			n := min(len(msg), band)
			out.WriteString(pkt("\x02" + msg[:n]))
			msg = msg[n:]
		}
	}
	data := status.Bytes()
	for len(data) > 0 {
		n := min(len(data), band)
		out.WriteString(pkt("\x01" + string(data[:n])))
		data = data[n:]
	}
	out.WriteString("0000")
	_, _ = w.Write(out.Bytes())
}

// pkt encodes one pkt-line.
func pkt(payload string) string {
	return fmt.Sprintf("%04x", len(payload)+4) + payload
}

// diffRefs returns the refs that differ between before and after, sorted,
// leaving out refs/pull/ (the fake maintains them).
func diffRefs(before, after map[string]string) []refChange {
	var out []refChange
	for ref, id := range after {
		if before[ref] != id {
			out = append(out, refChange{ref: ref, old: before[ref], new: id})
		}
	}
	for ref, id := range before {
		if _, ok := after[ref]; !ok {
			out = append(out, refChange{ref: ref, old: id})
		}
	}
	out = slices.DeleteFunc(out, func(c refChange) bool { return strings.HasPrefix(c.ref, "refs/pull/") })
	slices.SortFunc(out, func(a, b refChange) int { return strings.Compare(a.ref, b.ref) })
	return out
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
