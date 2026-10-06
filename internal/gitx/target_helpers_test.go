package gitx

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Versions of git some delivery features need.
var (
	gitCheckAttrSource = [3]int{2, 40, 0} // check-attr --source
	gitAttrSource      = [3]int{2, 41, 0} // git --attr-source
	gitSSHSign         = [3]int{2, 34, 0} // gpg.format=ssh
)

var (
	localGitOnce    sync.Once
	localGitVersion [3]int
	localGitErr     error
)

// requireGit skips the test when the local git is older than min; why says
// what needs it.
func requireGit(t *testing.T, min [3]int, why string) {
	t.Helper()
	if !gitAtLeast(t, min) {
		t.Skipf("git %s is older than %s, which %s needs", formatVersion(localGitVersion), formatVersion(min), why)
	}
}

// gitAtLeast reports whether the local git is min or newer.
func gitAtLeast(t *testing.T, min [3]int) bool {
	t.Helper()
	localGitOnce.Do(func() {
		localGitVersion, localGitErr = (&Git{}).Version(t.Context())
	})
	if localGitErr != nil {
		t.Fatalf("git version: %v", localGitErr)
	}
	return slices.Compare(localGitVersion[:], min[:]) >= 0
}

// lazyFetchOff reports whether GIT_NO_LAZY_FETCH works: older git fetches
// a missing object of a partial clone when a command asks for it, so
// "not present" cannot be observed. When it does not, it logs that the
// caller's assertion is skipped, so that a green run on an old git does not
// look complete.
func lazyFetchOff(t *testing.T) bool {
	t.Helper()
	if gitAtLeast(t, DeliveryMinVersion) {
		return true
	}
	t.Logf("git %s is older than %s: an assertion about objects that are not present is skipped", formatVersion(localGitVersion), formatVersion(DeliveryMinVersion))
	return false
}

// file is one path of a served commit: content is written as a blob of
// mode (100644 when empty), or with mode 160000 content is the commit id;
// del deletes the path.
type file struct {
	path, mode, content string
	del                 bool
}

// served is a bare repository targets fetch from and push to, written with
// fast-import so that modes, symlinks, gitlinks, merges and messages are
// exactly what the test says.
type served struct {
	t    *testing.T
	dir  string
	g    *Git
	tick int
}

// newServed creates a bare repository dir/name that serves filters, any
// object by id and pushes.
func newServed(t *testing.T, root, name string) *served {
	t.Helper()
	dir := filepath.Join(root, name)
	g := &Git{Env: testEnv(t)}
	if _, err := g.Run(t.Context(), nil, "init", "-q", "--bare", "-b", "main", dir); err != nil {
		t.Fatal(err)
	}
	// Appended rather than set with git config: three git runs fewer per
	// repository, which counts on Windows.
	f, err := os.OpenFile(filepath.Join(dir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteString("[uploadpack]\n\tallowFilter = true\n\tallowAnySHA1InWant = true\n[http]\n\treceivepack = true\n")
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		t.Fatal(err)
	}
	return &served{t: t, dir: dir, g: &Git{Dir: dir, Env: g.Env}}
}

// git runs git in the served repository.
func (s *served) git(args ...string) string {
	s.t.Helper()
	return s.gitIn("", args...)
}

func (s *served) gitIn(stdin string, args ...string) string {
	s.t.Helper()
	out, err := s.g.Run(s.t.Context(), strings.NewReader(stdin), args...)
	if err != nil {
		s.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// commit writes a commit on branch with parents (none: a root commit), the
// tree of the first parent with files applied, and message msg; it moves
// branch to it and returns its id. Dates are distinct: one minute apart.
func (s *served) commit(branch string, parents []string, files []file, msg string) string {
	s.t.Helper()
	s.tick++
	return s.commitAt(branch, parents, files, msg, fmt.Sprintf("%d +0000", 1767225600+60*s.tick))
}

// commitAt is commit with an explicit "<unix> <tz>" date.
func (s *served) commitAt(branch string, parents []string, files []file, msg, date string) string {
	s.t.Helper()
	return s.importCommits([]importCommit{{branch: branch, parents: parents, files: files, msg: msg, date: date}})[0]
}

// importCommit is one commit of importCommits; a parent ":<n>" is the n-th
// commit of the same import (1-based).
type importCommit struct {
	branch    string
	parents   []string
	files     []file
	msg, date string
}

// importCommits writes commits in one fast-import run and returns their
// ids.
func (s *served) importCommits(commits []importCommit) []string {
	s.t.Helper()
	var b strings.Builder
	for n, c := range commits {
		ref := "refs/heads/" + c.branch
		if len(c.parents) == 0 {
			fmt.Fprintf(&b, "reset %s\n", ref)
		}
		fmt.Fprintf(&b, "commit %s\nmark :%d\n", ref, n+1)
		fmt.Fprintf(&b, "author A U Thor <author@example.com> %s\n", c.date)
		fmt.Fprintf(&b, "committer C O Mitter <committer@example.com> %s\n", c.date)
		fmt.Fprintf(&b, "data %d\n%s\n", len(c.msg), c.msg)
		for i, p := range c.parents {
			if i == 0 {
				fmt.Fprintf(&b, "from %s\n", p)
			} else {
				fmt.Fprintf(&b, "merge %s\n", p)
			}
		}
		for _, f := range c.files {
			mode := f.mode
			if mode == "" {
				mode = "100644"
			}
			switch {
			case f.del:
				fmt.Fprintf(&b, "D %s\n", f.path)
			case mode == "160000":
				fmt.Fprintf(&b, "M 160000 %s %s\n", f.content, f.path)
			default:
				fmt.Fprintf(&b, "M %s inline %s\ndata %d\n%s\n", mode, f.path, len(f.content), f.content)
			}
		}
		b.WriteString("\n")
	}
	marks := filepath.Join(s.t.TempDir(), "marks")
	s.gitIn(b.String(), "-c", "core.autocrlf=false", "fast-import", "--quiet", "--force", "--export-marks="+marks)
	data, err := os.ReadFile(marks)
	if err != nil {
		s.t.Fatal(err)
	}
	ids := make([]string, len(commits))
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var n int
		var id string
		if _, err := fmt.Sscanf(line, ":%d %s", &n, &id); err != nil || n < 1 || n > len(commits) {
			s.t.Fatalf("marks line %q: %v", line, err)
		}
		ids[n-1] = id
	}
	return ids
}

// chain writes n commits on branch after parent ("" for a root), each
// changing a.txt, and returns their ids, oldest first.
func (s *served) chain(branch, parent string, n int) []string {
	s.t.Helper()
	commits := make([]importCommit, n)
	for i := range commits {
		c := importCommit{branch: branch, files: []file{{path: "a.txt", content: fmt.Sprintf("%s %d\n", branch, i+1)}},
			msg: fmt.Sprintf("%s %d\n", branch, i+1)}
		switch {
		case i > 0:
			c.parents = []string{fmt.Sprintf(":%d", i)}
		case parent != "":
			c.parents = []string{parent}
		}
		s.tick++
		c.date = fmt.Sprintf("%d +0000", 1767225600+60*s.tick)
		commits[i] = c
	}
	return s.importCommits(commits)
}

// blob returns the id of path at rev in the served repository.
func (s *served) blob(rev, path string) string {
	s.t.Helper()
	return s.git("rev-parse", "--verify", rev+":"+path)
}

// hook installs a server-side hook script.
func (s *served) hook(name, script string) {
	s.t.Helper()
	dir := filepath.Join(s.dir, "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

// newTarget creates a target repository for remote with file and loopback
// http remotes allowed.
func newTarget(t *testing.T, remote string, auth Auth) *TargetRepo {
	t.Helper()
	iso := Isolation{Home: t.TempDir(), AllowFile: true, AllowHTTP: true}
	tr, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "target"), remote, auth, iso)
	if err != nil {
		t.Fatalf("InitTarget(%s): %v", remote, err)
	}
	return tr
}

// basic returns a Basic Authorization value.
func basic(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

// staticAuth sends header, counting the calls in calls (may be nil).
func staticAuth(header string, calls *atomic.Int32) Auth {
	return Auth{Header: func(context.Context) (string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return header, nil
	}}
}

// gitServer is a smart-HTTP git server: `git http-backend` behind
// net/http/cgi, serving every bare repository under root. When want is
// set, a request without that Authorization value gets deny (401 with a
// Basic challenge, or 403).
type gitServer struct {
	URL string
	// CAFile is the PEM certificate of a TLS server.
	CAFile string

	mu      sync.Mutex
	headers []string
	deny    int
	want    string
}

func newGitServer(t *testing.T, root, want string) *gitServer {
	t.Helper()
	return startGitServer(t, root, want, false)
}

// newTLSGitServer is newGitServer over https with a self-signed certificate
// for 127.0.0.1; CAFile holds it in PEM.
func newTLSGitServer(t *testing.T, root, want string) *gitServer {
	t.Helper()
	return startGitServer(t, root, want, true)
}

// gitBackend is `git http-backend` behind net/http/cgi, serving every bare
// repository under root.
func gitBackend(t *testing.T, root string) http.Handler {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return &cgi.Handler{
		Path: bin,
		Args: []string{"http-backend"},
		Dir:  root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + global,
			"HOME=" + home,
		},
		Logger: log.New(io.Discard, "", 0),
		Stderr: io.Discard,
	}
}

func startGitServer(t *testing.T, root, want string, useTLS bool) *gitServer {
	t.Helper()
	backend := gitBackend(t, root)
	s := &gitServer{deny: http.StatusUnauthorized, want: want}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		s.mu.Lock()
		s.headers = append(s.headers, got)
		deny, want := s.deny, s.want
		s.mu.Unlock()
		if want != "" && got != want {
			if deny == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Basic realm="touchmark test"`)
			}
			http.Error(w, http.StatusText(deny), deny)
			return
		}
		backend.ServeHTTP(w, r)
	})
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(handler)
		s.CAFile = filepath.Join(t.TempDir(), "ca.pem")
		cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		if err := os.WriteFile(s.CAFile, cert, 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		srv = httptest.NewServer(handler)
	}
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// seen returns the Authorization values received so far.
func (s *gitServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.headers)
}

// setDeny sets the status of refused requests.
func (s *gitServer) setDeny(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny = status
}
