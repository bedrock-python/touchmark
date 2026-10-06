package gitx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHTTPExtraHeader: the credential reaches the server as an extra
// header on every request of every network command, and never appears in
// argv, the repository's config or an error.
func TestHTTPExtraHeader(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "acme/api.git")
	ids := s.chain("main", "", 3)
	const token = "tm-test-token-5f2a9c"
	header := basic("x-access-token", token)
	srv := newGitServer(t, root, header)
	remote := srv.URL + "/acme/api.git"

	var calls atomic.Int32
	tr := newTarget(t, remote, staticAuth(header, &calls))
	var argvs, envs [][]string
	tr.iso.trace = func(args, env []string) {
		argvs = append(argvs, args)
		envs = append(envs, env)
	}

	refs, err := tr.RemoteRefs(t.Context(), "refs/heads/main", "refs/heads/none")
	if err != nil || len(refs) != 1 || refs["refs/heads/main"] != ids[2] {
		t.Fatalf("RemoteRefs() = %v, %v", refs, err)
	}
	sha, ok, err := tr.FetchBranch(t.Context(), "main", 2)
	if err != nil || !ok || sha != ids[2] {
		t.Fatalf("FetchBranch() = %s, %v, %v", sha, ok, err)
	}
	if _, ok, err := tr.FetchBranch(t.Context(), "none", 1); err != nil || ok {
		t.Errorf("FetchBranch(none) = %v, %v", ok, err)
	}
	res, err := tr.Push(t.Context(), PushSpec{Branch: "touchmark/acme", Commit: ids[1]})
	if err != nil || res.Status != PushOK {
		t.Fatalf("Push() = %+v, %v", res, err)
	}
	if got := s.git("rev-parse", "refs/heads/touchmark/acme"); got != ids[1] {
		t.Errorf("pushed branch = %s, want %s", got, ids[1])
	}

	if n := calls.Load(); n != 4 {
		t.Errorf("Header called %d times, want once per network command (4)", n)
	}
	seen := srv.seen()
	if len(seen) == 0 {
		t.Fatal("the server saw no request")
	}
	for _, h := range seen {
		if h != header {
			t.Errorf("a request carried Authorization %q", h)
		}
	}
	scopeKey := "http." + srv.URL + "/.extraHeader"
	network := 0
	for i, argv := range argvs {
		for _, a := range argv {
			if strings.Contains(a, token) || strings.Contains(a, header) {
				t.Errorf("argv carries the credential: %v", argv)
			}
		}
		keyIdx := slices.IndexFunc(envs[i], func(kv string) bool { return strings.HasSuffix(kv, "="+scopeKey) })
		isNetwork := slices.ContainsFunc(argv, func(a string) bool { return a == "fetch" || a == "push" || a == "ls-remote" })
		if isNetwork {
			network++
		}
		if (keyIdx >= 0) != isNetwork {
			t.Errorf("command %v: extra header present = %v", argv, keyIdx >= 0)
		}
		for _, kv := range envs[i] {
			if strings.Contains(kv, token) || strings.Contains(kv, header) {
				n, _, _ := strings.Cut(kv, "=")
				if !strings.HasPrefix(n, "GIT_CONFIG_VALUE_") || kv[len(n)+1:] != "Authorization: "+header {
					t.Errorf("the credential is in %s", n)
				}
			}
		}
	}
	if network != 4 {
		t.Errorf("%d network commands traced, want 4", network)
	}
	config, err := os.ReadFile(filepath.Join(tr.Dir, "config"))
	if err != nil || strings.Contains(string(config), token) || strings.Contains(string(config), "extraHeader") {
		t.Errorf("config = %q, %v", config, err)
	}
}

func TestHTTPAuthFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.chain("main", "", 1)[0]
	const good, bad = "tm-good-token-7e1d", "tm-bad-token-3c9b"
	srv := newGitServer(t, root, basic("oauth2", good))
	remote := srv.URL + "/repo.git"

	tr := newTarget(t, remote, staticAuth(basic("oauth2", bad), nil))
	// 401 is a refused credential; 403 an identity the repository refuses,
	// which must not count towards provider-down.
	for _, c := range []struct {
		status int
		fetch  Failure
		push   PushStatus
	}{
		{http.StatusUnauthorized, FailureAuth, PushAuth},
		{http.StatusForbidden, FailurePermission, PushPermission},
		{http.StatusTooManyRequests, FailureRateLimited, PushRateLimited},
	} {
		srv.setDeny(c.status)
		_, _, err := tr.FetchBranch(t.Context(), "main", 1)
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%d: FetchBranch() = %v, want a git error", c.status, err)
		}
		if f := ClassifyFailure(err); f != c.fetch {
			t.Errorf("%d: ClassifyFailure(%v) = %v, want %v", c.status, err, f, c.fetch)
		}
		if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), basic("oauth2", bad)) {
			t.Errorf("%d: the error leaks the credential: %v", c.status, err)
		}
		res, err := tr.Push(t.Context(), PushSpec{Branch: "x", Commit: head})
		if err != nil || res.Status != c.push || res.Transient() {
			t.Errorf("%d: Push() = %+v, %v (transient %v); want %v", c.status, res, err, res.Transient(), c.push)
		}
		if strings.Contains(res.Message, bad) {
			t.Errorf("%d: the message leaks the credential: %q", c.status, res.Message)
		}
	}

	// Anonymous access to a server that wants a credential.
	anon := newTarget(t, remote, Auth{})
	if _, _, err := anon.FetchBranch(t.Context(), "main", 1); err == nil {
		t.Error("anonymous FetchBranch succeeded")
	}

	// A credential that cannot be read, or holds a line break.
	boom := errors.New("token endpoint down")
	failing := newTarget(t, remote, Auth{Header: func(context.Context) (string, error) { return "", boom }})
	if _, _, err := failing.FetchBranch(t.Context(), "main", 1); !errors.Is(err, boom) {
		t.Errorf("FetchBranch(failing credential) = %v", err)
	}
	if _, err := failing.Push(t.Context(), PushSpec{Branch: "x", Commit: head}); !errors.Is(err, boom) {
		t.Errorf("Push(failing credential) = %v", err)
	}
	injected := newTarget(t, remote, staticAuth(basic("oauth2", good)+"\r\nX-Evil: 1", nil))
	if _, err := injected.RemoteRefs(t.Context()); err == nil || strings.Contains(err.Error(), good) {
		t.Errorf("RemoteRefs(header with a line break) = %v", err)
	}

	// WithAuth switches the credential of the same repository.
	fixed := tr.WithAuth(staticAuth(basic("oauth2", good), nil))
	if fixed.Dir != tr.Dir {
		t.Errorf("WithAuth changed the directory")
	}
	if sha, ok, err := fixed.FetchBranch(t.Context(), "main", 1); err != nil || !ok || sha != head {
		t.Errorf("FetchBranch(WithAuth) = %s, %v, %v", sha, ok, err)
	}
}

// TestHTTPSCAFile: an https remote is trusted through Isolation.CAFile
// (http.sslCAInfo) and nothing else: without it, the self-signed
// certificate is refused before any request carries the credential.
func TestHTTPSCAFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.chain("main", "", 1)[0]
	header := basic("x-access-token", "tm-tls-token-4b3a")
	srv := newTLSGitServer(t, root, header)
	remote := srv.URL + "/repo.git"
	if !strings.HasPrefix(remote, "https://127.0.0.1:") {
		t.Fatalf("remote = %s", remote)
	}
	trusted, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "t"), remote, staticAuth(header, nil),
		Isolation{Home: t.TempDir(), CAFile: srv.CAFile})
	if err != nil {
		t.Fatal(err)
	}
	if sha, ok, err := trusted.FetchBranch(t.Context(), "main", 1); err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch(CAFile) = %s, %v, %v", sha, ok, err)
	}
	before := len(srv.seen())
	untrusted, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "t"), remote, staticAuth(header, nil),
		Isolation{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := untrusted.FetchBranch(t.Context(), "main", 1); err == nil {
		t.Error("FetchBranch trusted a self-signed certificate without CAFile")
	}
	if n := len(srv.seen()); n != before {
		t.Errorf("the server saw %d requests from the untrusted client", n-before)
	}
}

// TestHTTPEchoedCredential: a server that prints the credential back (a
// hook echoing the request's Authorization header) does not get it into a
// push result or an error.
func TestHTTPEchoedCredential(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.chain("main", "", 1)[0]
	const token = "tm-echoed-token-9f8e7d"
	header := basic("x-access-token", token)
	srv := newGitServer(t, root, header)
	s.hook("pre-receive", "#!/bin/sh\necho \"leak: $HTTP_AUTHORIZATION\" >&2\nprintf '%s' \"$HTTP_AUTHORIZATION\" | cut -d' ' -f2 | base64 -d >&2 2>/dev/null\necho >&2\nexit 1\n")
	tr := newTarget(t, srv.URL+"/repo.git", staticAuth(header, nil))
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	res, err := tr.Push(t.Context(), PushSpec{Branch: "x", Commit: head})
	if err != nil || res.Status != PushPolicy {
		t.Fatalf("Push() = %+v, %v", res, err)
	}
	if !strings.Contains(res.Message, "leak: ***") {
		t.Errorf("message %q: the echoed header is not masked where expected", res.Message)
	}
	if strings.Contains(res.Message, token) || strings.Contains(res.Message, strings.TrimPrefix(header, "Basic ")) {
		t.Errorf("message leaks the credential: %q", res.Message)
	}
}

func TestClassifyFailure(t *testing.T) {
	t.Parallel()
	gitErr := func(stderr string) error { return &Error{Args: []string{"fetch"}, Code: 128, Stderr: stderr} }
	for _, c := range []struct {
		err  error
		want Failure
	}{
		{gitErr("fatal: could not read Username for 'https://github.com': terminal prompts disabled"), FailureAuth},
		{gitErr("fatal: unable to access 'https://x/r.git/': The requested URL returned error: 401"), FailureAuth},
		{gitErr("remote: HTTP Basic: Access denied.\nfatal: Authentication failed for 'https://gitlab.com/g/p.git/'"), FailureAuth},
		{gitErr("remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/o/r.git/'"), FailureAuth},
		{gitErr("fatal: unable to access 'https://x/r.git/': The requested URL returned error: 403"), FailurePermission},
		{gitErr("remote: Permission to o/r.git denied to bot.\nfatal: unable to access"), FailurePermission},
		{gitErr("fatal: unable to access 'https://x/r.git/': The requested URL returned error: 429"), FailureRateLimited},
		{gitErr("error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429\nfatal: expected flush after ref listing"), FailureRateLimited},
		{gitErr("error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\nfatal: expected flush after ref listing"), FailureTransient},
		{gitErr("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: the remote end hung up unexpectedly"), FailureTransient},
		{gitErr("error: RPC failed; HTTP 504 curl 92 HTTP/2 stream 1 was not closed cleanly\nfatal: early EOF"), FailureTransient},
		// Other 4xx statuses come back on a retry: not transient, despite the
		// generic markers git prints with them.
		{gitErr("error: RPC failed; HTTP 413 curl 22 The requested URL returned error: 413\nfatal: the remote end hung up unexpectedly"), FailureUnknown},
		{gitErr("error: RPC failed; HTTP 400 curl 22 The requested URL returned error: 400\nfatal: the remote end hung up unexpectedly"), FailureUnknown},
		{gitErr("error: RPC failed; HTTP 422 curl 22 The requested URL returned error: 422\nfatal: early EOF"), FailureUnknown},
		{gitErr("error: RPC failed; HTTP 404 curl 22 The requested URL returned error: 404\nfatal: the remote end hung up unexpectedly"), FailureUnknown},
		// The command's own bound is transient; the caller's deadline is not.
		{fmt.Errorf("git fetch: %w after 3m0s", ErrNetworkTimeout), FailureTransient},
		{fmt.Errorf("git fetch: %w", context.DeadlineExceeded), FailureUnknown},
		{gitErr("fatal: unable to access 'https://x/': Could not resolve host: x"), FailureTransient},
		{gitErr("fatal: unable to access 'http://127.0.0.1:1/r.git/': Failed to connect to 127.0.0.1 port 1: Connection refused"), FailureTransient},
		{gitErr("fatal: unable to access 'http://127.0.0.1:1/r.git/': Failed to connect to 127.0.0.1 port 1 after 0 ms: Could not connect to server"), FailureTransient},
		{gitErr("fatal: unable to access 'https://x/': Couldn't connect to server"), FailureTransient},
		{gitErr("fatal: unable to access 'https://x/': Operation timed out after 30000 milliseconds"), FailureTransient},
		{gitErr("fatal: unable to access 'https://x/': Recv failure: Connection reset by peer"), FailureTransient},
		{gitErr("fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF"), FailureTransient},
		{gitErr("fatal: couldn't find remote ref refs/heads/x"), FailureUnknown},
		{gitErr("error: object 84cb: nulInCommit: NUL byte in the commit object body\nfatal: fsck error in packed object"), FailureUnknown},
		{gitErr("The requested URL returned error: 404"), FailureUnknown},
		{errors.New("401"), FailureUnknown},
		{context.DeadlineExceeded, FailureUnknown},
		{nil, FailureUnknown},
	} {
		if got := ClassifyFailure(c.err); got != c.want {
			t.Errorf("ClassifyFailure(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	// The local git's own wording for a server that is gone.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	tr := newTarget(t, gone.URL+"/r.git", Auth{})
	_, err := tr.RemoteRefs(t.Context())
	if f := ClassifyFailure(err); f != FailureTransient {
		t.Errorf("ClassifyFailure(%v) = %v, want FailureTransient", err, f)
	}
}

func TestHeaderSecrets(t *testing.T) {
	t.Parallel()
	h := basic("x-access-token", "ghs_0123456789")
	got := headerSecrets(h)
	want := []string{h, strings.TrimPrefix(h, "Basic "), "x-access-token:ghs_0123456789", "ghs_0123456789"}
	if !slices.Equal(got, want) {
		t.Errorf("headerSecrets() = %q, want %q", got, want)
	}
	msg := maskSecrets("fatal: "+h+" / ghs_0123456789", got)
	if strings.Contains(msg, "ghs_0123456789") || strings.Contains(msg, "Basic eC1") {
		t.Errorf("maskSecrets() = %q", msg)
	}
	if got := headerSecrets("Bearer abc"); len(got) != 1 {
		t.Errorf("headerSecrets(short) = %q", got)
	}
}
