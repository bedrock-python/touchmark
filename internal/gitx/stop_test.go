package gitx

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stallServer serves the bare repositories under root, anonymously, like
// gitServer; but the next upload-pack response (stallFetch) is cut after
// its shallow and packfile headers, and the next receive-pack request
// (stallPush) gets no answer at all. The rest never comes, until the
// client goes away: a server that hangs in the middle of a transfer.
type stallServer struct {
	URL                   string
	stallFetch, stallPush atomic.Bool
	// stalled counts the requests that were stalled.
	stalled atomic.Int32
}

func newStallServer(t *testing.T, root string) *stallServer {
	t.Helper()
	backend := gitBackend(t, root)
	s := &stallServer{}
	stop := make(chan struct{})
	hang := func(r *http.Request) {
		s.stalled.Add(1)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") && s.stallFetch.CompareAndSwap(true, false):
			rec := httptest.NewRecorder()
			backend.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			cut := len(body) / 2
			if i := bytes.Index(body, []byte("packfile\n")); i >= 0 {
				// Past the section header and into the pack: the client
				// holds shallow.lock and runs index-pack by then.
				cut = min(len(body)-1, i+len("packfile\n")+16)
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body[:cut])
			w.(http.Flusher).Flush()
			hang(r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") && s.stallPush.CompareAndSwap(true, false):
			hang(r)
		default:
			backend.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	s.URL = srv.URL
	return s
}

// stopSlack is how much longer than its bound a stopped command may take:
// the whole process tree goes at once, so it is far below waitDelay, which
// an orphan holding the output pipes would cost.
const stopSlack = 7 * time.Second

// TestNetworkTimeout: a network command that runs into its own bound
// stops with every process it started, is transient (ErrNetworkTimeout,
// not the caller's context error), leaves no lock behind (the next fetch
// works), and a timed-out push is a transient PushError whose outcome the
// caller must reconcile.
func TestNetworkTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.chain("main", "", 3)[2]
	srv := newStallServer(t, root)
	tr := newTarget(t, srv.URL+"/repo.git", Auth{})
	// Room for the fetch to reach the stall on a loaded machine: git and
	// the CGI backend start several processes first.
	const bound = 10 * time.Second
	tr.iso.timeout = bound

	srv.stallFetch.Store(true)
	start := time.Now()
	_, _, err := tr.FetchBranch(t.Context(), "main", 2)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrNetworkTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchBranch(stalled) = %v, want ErrNetworkTimeout only", err)
	}
	if f := ClassifyFailure(err); f != FailureTransient {
		t.Errorf("ClassifyFailure(%v) = %v, want FailureTransient", err, f)
	}
	if srv.stalled.Load() != 1 {
		t.Fatalf("the server stalled %d requests, want 1", srv.stalled.Load())
	}
	if elapsed > bound+stopSlack {
		t.Errorf("FetchBranch returned %v after its start, bound %v: a process outlived the timeout", elapsed, bound)
	}
	// The next fetch, a transient retry, works: no shallow.lock is left.
	if sha, ok, err := tr.FetchBranch(t.Context(), "main", 2); err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch after a timeout = %s, %v, %v; want %s", sha, ok, err, head)
	}

	srv.stallPush.Store(true)
	start = time.Now()
	res, err := tr.Push(t.Context(), PushSpec{Branch: "touchmark/hub", Commit: head})
	elapsed = time.Since(start)
	if err != nil || res.Status != PushError || !res.Transient() || !strings.Contains(res.Message, "unknown") {
		t.Errorf("Push(stalled) = %+v, %v (transient %v); want a transient PushError", res, err, res.Transient())
	}
	if elapsed > bound+stopSlack {
		t.Errorf("Push returned %v after its start, bound %v", elapsed, bound)
	}
	if got, err := tr.RemoteRefs(t.Context(), "refs/heads/touchmark/hub"); err != nil || len(got) != 0 {
		t.Errorf("the stalled push moved the branch: %v, %v", got, err)
	}
}

// TestCancelledFetch: the caller's own deadline is its context error, not
// a transient failure; the stopped fetch leaves nothing that fails the
// next one.
func TestCancelledFetch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.chain("main", "", 3)[2]
	srv := newStallServer(t, root)
	tr := newTarget(t, srv.URL+"/repo.git", Auth{})

	srv.stallFetch.Store(true)
	const bound = 3 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	start := time.Now()
	_, _, err := tr.FetchBranch(ctx, "main", 1)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNetworkTimeout) {
		t.Fatalf("FetchBranch(deadline) = %v, want the context's error", err)
	}
	if f := ClassifyFailure(err); f != FailureUnknown {
		t.Errorf("ClassifyFailure(%v) = %v, want FailureUnknown", err, f)
	}
	if elapsed > bound+stopSlack {
		t.Errorf("FetchBranch returned %v after its start, deadline %v", elapsed, bound)
	}
	// On a loaded machine the deadline can come before the fetch reached
	// the server: the stall is then still armed and would hold the next
	// fetch until git's own network bound.
	srv.stallFetch.Store(false)
	if sha, ok, err := tr.FetchBranch(t.Context(), "main", 1); err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch after a cancelled one = %s, %v, %v; want %s", sha, ok, err, head)
	}
}

// TestStaleLocksRemoved: the lock files a killed fetch leaves (on Windows
// git gets no chance to remove them) do not fail the next fetch.
func TestStaleLocksRemoved(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	head := s.chain("main", "", 2)[1]
	tr := newTarget(t, s.dir, Auth{})
	if _, _, err := tr.FetchBranch(t.Context(), "main", 1); err != nil {
		t.Fatal(err)
	}
	locks := []string{"shallow.lock", "packed-refs.lock", filepath.Join("refs", "touchmark", "remote", "main.lock")}
	for _, l := range locks {
		if err := os.WriteFile(filepath.Join(tr.Dir, l), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if sha, ok, err := tr.FetchBranch(t.Context(), "main", 2); err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch with stale locks = %s, %v, %v", sha, ok, err)
	}
	for _, l := range locks {
		if _, err := os.Stat(filepath.Join(tr.Dir, l)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there: %v", l, err)
		}
	}
	if err := tr.DeepenSince(t.Context(), "main", time.Unix(1, 0)); err != nil {
		t.Errorf("DeepenSince after the cleanup: %v", err)
	}
}

// TestMaskedTail: a secret the tail of stderr cuts is masked whole or
// dropped whole, never shown in part.
func TestMaskedTail(t *testing.T) {
	t.Parallel()
	const secret = "SECRETSECRETSECRETSECRET0123456789"
	secrets := headerSecrets("Bearer " + secret)
	for size := 0; size < 3*len(secret); size++ {
		// A server echoing the credential in a body of any size, then git's
		// own line.
		out := strings.Repeat("x", size) + " token=" + secret + " rejected\nfatal: unable to access: The requested URL returned error: 403\n"
		for _, limit := range []int{20, 48, 64, 80} {
			b := newTailBuffer(limit)
			_, _ = b.Write([]byte(out))
			got := b.masked(secrets)
			for n := 6; n <= len(secret); n++ {
				for i := 0; i+n <= len(secret); i++ {
					if strings.Contains(got, secret[i:i+n]) {
						t.Fatalf("size %d, limit %d: %q shows %q of the secret", size, limit, got, secret[i:i+n])
					}
				}
			}
			if !strings.Contains(got, "403") && limit >= 80 {
				t.Errorf("size %d, limit %d: %q lost git's last line", size, limit, got)
			}
		}
	}
	// Without secrets nothing but the dropped output goes.
	b := newTailBuffer(4)
	_, _ = b.Write([]byte("abc\ndefg"))
	if got := b.masked(nil); got != "...defg" {
		t.Errorf("masked(nil) = %q", got)
	}
	b = newTailBuffer(64)
	_, _ = b.Write([]byte("a " + secret + " b"))
	if got := b.masked(secrets); got != "a *** b" {
		t.Errorf("masked(untruncated) = %q", got)
	}
}
