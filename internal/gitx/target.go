package gitx

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// networkTimeout bounds one ls-remote, fetch or push, unless ctx ends
// earlier.
const networkTimeout = 3 * time.Minute

// Entries InitTarget keeps in Isolation.Home.
const (
	homeConfig = "gitconfig" // empty global config (GIT_CONFIG_GLOBAL)
	homeHooks  = "hooks"     // empty hooks directory (core.hooksPath)
)

// remoteName is the one remote of every target repository.
const remoteName = "origin"

// zeroSHA1 is the null object id git prints for "no object".
const zeroSHA1 = "0000000000000000000000000000000000000000"

// emptyTreeSHA1 is the id of the empty tree; git knows it without the
// object being stored. Target repositories are always sha1: sha256
// targets are skipped.
const emptyTreeSHA1 = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// InitTarget creates a bare repository in dir (which must be empty or
// absent), with origin = remote, configured as a partial clone (promisor
// remote, filter blob:none) so blobless fetches work: git init --bare
// (sha1, no template files: no sample hooks, no info/exclude),
// core.repositoryformatversion=1, extensions.partialClone=origin,
// remote.origin.url, remote.origin.promisor=true and
// remote.origin.partialclonefilter=blob:none. No fetch refspec is
// configured: every fetch names its refs.
//
// remote is an https URL without credentials, query or fragment and with a
// lowercase scheme; an http URL of a loopback host with
// Isolation.AllowHTTP; a file:// URL or an absolute local path with
// Isolation.AllowFile. Any other remote is refused (ssh, git://, ext::,
// scp-like "host:path", relative paths). auth is used for http(s) remotes
// only.
//
// On failure InitTarget removes what it created in dir. It does not check
// git's version (DeliveryMinVersion): the caller does, once per run.
func InitTarget(ctx context.Context, dir, remote string, auth Auth, iso Isolation) (*TargetRepo, error) {
	if iso.Home == "" {
		return nil, errors.New("git target: Isolation.Home is required")
	}
	if dir == "" {
		return nil, errors.New("git target: no repository directory")
	}
	scope, protocols, err := checkRemote(remote, iso)
	if err != nil {
		return nil, fmt.Errorf("git target: %w", err)
	}
	home, err := filepath.Abs(iso.Home)
	if err != nil {
		return nil, fmt.Errorf("git target: %w", err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, fmt.Errorf("git target: %w", err)
	}
	caFile := ""
	if iso.CAFile != "" {
		if caFile, err = filepath.Abs(iso.CAFile); err != nil {
			return nil, fmt.Errorf("git target: %w", err)
		}
		if fi, err := os.Stat(caFile); err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("git target: CA file %s is not a readable file", caFile)
		}
	}
	global, hooks, err := prepareHome(home)
	if err != nil {
		return nil, fmt.Errorf("git target: %w", err)
	}
	created, err := prepareDir(dir)
	if err != nil {
		return nil, fmt.Errorf("git target: %w", err)
	}
	x := newIsolated(home, global, hooks, caFile, scope, protocols)
	t := &TargetRepo{Git: x.git(dir), Dir: dir, Remote: remote, auth: auth, iso: x}
	ok := false
	defer func() {
		if !ok {
			cleanDir(dir, created)
		}
	}()
	if _, _, err := t.exec(ctx, cmdOpts{noGitDir: true}, "init", "--bare", "--quiet", "--template="); err != nil {
		return nil, fmt.Errorf("git target: %w", err)
	}
	for _, kv := range [][2]string{
		{"core.repositoryformatversion", "1"},
		{"extensions.partialClone", remoteName},
		{"remote." + remoteName + ".url", remote},
		{"remote." + remoteName + ".promisor", "true"},
		{"remote." + remoteName + ".partialclonefilter", "blob:none"},
	} {
		if _, _, err := t.exec(ctx, cmdOpts{}, "config", kv[0], kv[1]); err != nil {
			return nil, fmt.Errorf("git target: %w", err)
		}
	}
	ok = true
	return t, nil
}

// WithAuth returns the same repository with another credential: its
// network commands send auth's header instead (for example the per-target
// write credential after a snapshot read with the read one). The copy
// shares the directory, the isolation and the fetch lock with t.
func (t *TargetRepo) WithAuth(auth Auth) *TargetRepo {
	if t == nil {
		return nil
	}
	c := *t
	c.auth = auth
	return &c
}

// checkRemote validates remote against iso. It returns the extraHeader
// base of an http(s) remote ("" for a local one) and the protocols git may
// use.
func checkRemote(remote string, iso Isolation) (scope string, protocols []string, err error) {
	protocols = []string{"https"}
	if iso.AllowHTTP {
		protocols = append(protocols, "http")
	}
	if iso.AllowFile {
		protocols = append(protocols, "file")
	}
	if remote == "" {
		return "", nil, errors.New("no remote URL")
	}
	if strings.ContainsFunc(remote, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", nil, errors.New("the remote URL holds a control character")
	}
	if !strings.Contains(remote, "://") {
		if !iso.AllowFile {
			return "", nil, errors.New("the remote is not an https URL")
		}
		if !filepath.IsAbs(remote) {
			return "", nil, fmt.Errorf("local remote %q is not an absolute path", remote)
		}
		return "", protocols, nil
	}
	u, err := url.Parse(remote)
	if err != nil {
		// url.Error quotes the URL, which may carry a secret: say less.
		return "", nil, errors.New("the remote URL does not parse")
	}
	switch {
	case u.User != nil:
		return "", nil, errors.New("the remote URL carries credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return "", nil, errors.New("the remote URL has a query, a fragment or no path")
	case !strings.HasPrefix(remote, u.Scheme+"://"):
		return "", nil, errors.New("the remote URL's scheme must be lowercase")
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
		if u.Hostname() == "" {
			return "", nil, errors.New("the remote URL has no host")
		}
		return "https://" + host + "/", protocols, nil
	case "http":
		if !iso.AllowHTTP {
			return "", nil, fmt.Errorf("http remote %s is not allowed: use https", u.Redacted())
		}
		if !isLoopback(u.Hostname()) {
			return "", nil, fmt.Errorf("http remote %s is not a loopback host", u.Redacted())
		}
		return "http://" + host + "/", protocols, nil
	case "file":
		if !iso.AllowFile {
			return "", nil, errors.New("file remotes are not allowed")
		}
		if host != "" {
			// git reads file://<host>/... as a path starting with the host.
			return "", nil, fmt.Errorf("file remote on host %q: use file:///path", u.Host)
		}
		return "", protocols, nil
	}
	return "", nil, fmt.Errorf("remote URL scheme %q is not allowed", u.Scheme)
}

// isLoopback reports whether host (without port) is a loopback address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// prepareHome makes the isolation files in home: an empty global config
// file and an empty hooks directory. It refuses a home that holds anything
// else, or files that are not empty: no user file may be read.
func prepareHome(home string) (global, hooks string, err error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", "", err
	}
	global = filepath.Join(home, homeConfig)
	f, err := os.OpenFile(global, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	if fi, err := os.Lstat(global); err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		return "", "", fmt.Errorf("isolation: %s must be an empty file", global)
	}
	hooks = filepath.Join(home, homeHooks)
	if err := os.Mkdir(hooks, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", "", err
	}
	if fi, err := os.Lstat(hooks); err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("isolation: %s must be a directory", hooks)
	}
	if entries, err := os.ReadDir(hooks); err != nil || len(entries) > 0 {
		return "", "", fmt.Errorf("isolation: hooks directory %s must be empty", hooks)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if e.Name() != homeConfig && e.Name() != homeHooks {
			return "", "", fmt.Errorf("isolation: home %s holds %q: it must be empty", home, e.Name())
		}
	}
	return global, hooks, nil
}

// prepareDir makes sure dir exists and is empty. created reports that it
// did not exist.
func prepareDir(dir string) (created bool, err error) {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	case len(entries) > 0:
		return false, fmt.Errorf("%s is not empty", dir)
	}
	return false, nil
}

// cleanDir undoes prepareDir and what followed it: dir was empty or
// absent, so everything in it is InitTarget's.
func cleanDir(dir string, created bool) {
	if created {
		_ = os.RemoveAll(dir)
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// newIsolated builds the isolation of a target repository.
//
// Besides the settings that keep the machine and the remote out, three
// settings that `git init` probes from the file system (and writes into
// the repository's config) are pinned, so that a hub and a target give the
// same answers on every runner, so a rerun writes nothing:
// core.ignorecase=true (attribute patterns match ignoring case, as in the
// Windows and macOS checkouts of the target: the fail-closed choice for the
// filter, encoding and renormalize checks), core.precomposeunicode=false
// (paths are taken as the trees spell them) and core.autocrlf=false.
func newIsolated(home, global, hooks, caFile, scope string, protocols []string) *isolated {
	config := [][2]string{
		{"core.hooksPath", filepath.ToSlash(hooks)},
		{"core.fsmonitor", "false"},
		{"core.ignorecase", "true"},
		{"core.precomposeunicode", "false"},
		{"core.autocrlf", "false"},
		{"core.askPass", ""},
		{"credential.helper", ""},
		{"protocol.allow", "never"},
	}
	for _, p := range protocols {
		config = append(config, [2]string{"protocol." + p + ".allow", "always"})
	}
	config = append(config, [2]string{"http.followRedirects", "false"})
	if caFile != "" {
		config = append(config, [2]string{"http.sslCAInfo", filepath.ToSlash(caFile)})
	}
	config = append(config,
		[2]string{"transfer.fsckObjects", "true"},
		[2]string{"submodule.recurse", "false"},
		[2]string{"gc.auto", "0"},
		[2]string{"maintenance.auto", "false"},
	)
	args := make([]string, 0, 2*len(config))
	for _, kv := range config {
		args = append(args, "-c", kv[0]+"="+kv[1])
	}
	return &isolated{
		home:   home,
		global: global,
		hooks:  hooks,
		scope:  scope,
		args:   args,
		config: config,
		env: []string{
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + global,
			"GIT_ATTR_NOSYSTEM=1",
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_ASKPASS=",
			"GIT_NO_LAZY_FETCH=1",
		},
	}
}

// git returns the Git of the repository in dir: the isolation environment
// with the isolation settings in GIT_CONFIG_COUNT and no credential.
func (x *isolated) git(dir string) *Git {
	return &Git{Dir: dir, Env: x.environ(dir, nil), Inherit: isolatedEnviron}
}

// environ returns the environment of a command: the isolation variables,
// GIT_DIR=gitDir unless it is "", and the isolation settings plus extra in
// GIT_CONFIG_COUNT.
func (x *isolated) environ(gitDir string, extra [][2]string) []string {
	config := append(slices.Clone(x.config), extra...)
	env := make([]string, 0, len(x.env)+2+2*len(config))
	env = append(env, x.env...)
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
	}
	return env
}

// inheritedEnv are the only variables of the process environment a
// target's git sees (the isolation variables come on top): what git needs
// to start and to reach the network. The search path and, on Windows, the
// system root (Winsock needs it) and shell; temporary directories; proxies,
// in the spellings curl reads; OpenSSL's trust store overrides.
//
// Everything else stays out: credentials above all,
// touchmark's own (TOUCHMARK_*), the CI's (GITHUB_TOKEN, CI_JOB_TOKEN,
// ACTIONS_ID_TOKEN_REQUEST_TOKEN, GITEA_TOKEN, …) and any other; git's own
// variables (GIT_*), which could point it at other config, objects or
// hooks, turn on traces that print headers, or name a proxy or an ssh
// command; and the rest of the user's environment.
var inheritedEnv = []string{
	"PATH", "PATHEXT", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC",
	"TEMP", "TMP", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// isolatedEnviron is the process environment reduced to inheritedEnv.
// Names compare case-insensitively (curl reads http_proxy in lowercase;
// Windows spells names as it likes).
func isolatedEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := envName(kv)
		if slices.ContainsFunc(inheritedEnv, func(n string) bool { return strings.EqualFold(n, name) }) {
			out = append(out, kv)
		}
	}
	return out
}

// cmdOpts are the options of one command of a target repository.
type cmdOpts struct {
	// network sends the credential and bounds the command by
	// networkTimeout.
	network bool
	// noGitDir leaves GIT_DIR unset (git init).
	noGitDir bool
	stdin    io.Reader
	// env is added to the environment (GIT_INDEX_FILE).
	env []string
	// config is added to the settings of GIT_CONFIG_COUNT, which the
	// commands git starts for this one (send-pack, pack-objects) see too.
	config [][2]string
	// stderrLimit, when set, is how much of stderr is kept instead of
	// stderrLimit.
	stderrLimit int
}

// exec runs git with the isolation options before args and returns stdout,
// the tail of stderr and the error, which is an *Error when git ran and
// exited non-zero. Secrets of the credential are masked in stdout, stderr
// and the error. stdout is returned even when git failed.
func (t *TargetRepo) exec(ctx context.Context, o cmdOpts, args ...string) ([]byte, string, error) {
	if err := t.ready(); err != nil {
		return nil, "", err
	}
	if len(args) == 0 {
		return nil, "", errors.New("git: no command")
	}
	extra := slices.Clone(o.config)
	var secrets []string
	if o.network && t.auth.Header != nil && t.iso.scope != "" {
		h, err := t.auth.Header(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("git %s: get the credential: %w", args[0], err)
		}
		if err := checkHeader(h); err != nil {
			return nil, "", fmt.Errorf("git %s: %w", args[0], err)
		}
		extra = append(extra, [2]string{"http." + t.iso.scope + ".extraHeader", "Authorization: " + h})
		secrets = headerSecrets(h)
	}
	runCtx := ctx
	if o.network {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, t.iso.netTimeout())
		defer cancel()
	}
	g := *t.Git
	gitDir := t.Dir
	if o.noGitDir {
		gitDir = ""
	}
	g.Env = append(t.iso.environ(gitDir, extra), o.env...)
	cmd := g.command(runCtx, append(slices.Clone(t.iso.args), args...))
	if t.iso.trace != nil {
		t.iso.trace(slices.Clone(cmd.Args), slices.Clone(cmd.Env))
	}
	limit := stderrLimit
	if o.stderrLimit > 0 {
		limit = o.stderrLimit
	}
	var stdout bytes.Buffer
	stderr := newTailBuffer(limit)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.stdin, &stdout, stderr
	runErr := cmd.Run()
	errText := stderr.masked(secrets)
	out := stdout.Bytes()
	if len(secrets) > 0 {
		// A network command's output comes from the server: a remote
		// that echoes the credential must not get it into a report.
		out = []byte(maskSecrets(string(out), secrets))
	}
	if runErr != nil {
		return out, errText, t.runError(ctx, runCtx, args, runErr, errText)
	}
	return out, errText, nil
}

// errNotInitialized is returned by the methods of a TargetRepo that
// InitTarget did not make.
var errNotInitialized = errors.New("git target: the repository was not made by InitTarget")

// ready fails for a TargetRepo that InitTarget did not make.
func (t *TargetRepo) ready() error {
	if t == nil || t.iso == nil || t.Git == nil {
		return errNotInitialized
	}
	return nil
}

// lockFetch takes the repository's fetch lock, removes the lock files a
// killed fetch may have left (removeStaleLocks), and returns the lock's
// release.
func (t *TargetRepo) lockFetch() (unlock func(), err error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	t.iso.fetchMu.Lock()
	removeStaleLocks(t.Dir)
	return t.iso.fetchMu.Unlock, nil
}

// staleLocks are the lock files of the repository's top directory a fetch
// takes: the shallow file (depth and deepen fetches), packed refs and the
// config (a partial clone may record its filter).
var staleLocks = []string{"shallow.lock", "packed-refs.lock", "config.lock"}

// removeStaleLocks removes the lock files a fetch that was killed left in
// the repository dir: staleLocks, and the ref locks under refs/. On a
// cancelled context git gets the chance to remove them itself on Unix, but
// not on Windows (TerminateProcess), nor when it outlives waitDelay or
// touchmark is killed; left in place, they fail every later fetch ("Unable
// to create '…/shallow.lock': File exists"). Only fetches take these locks,
// and fetches are serialized by fetchMu, whose holder calls this: a lock
// found here belongs to no running command. Failures are ignored: the
// fetch reports a lock it still meets.
func removeStaleLocks(dir string) {
	for _, name := range staleLocks {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			_ = os.Remove(path)
		}
		return nil
	})
}

// run is exec for commands whose stderr only matters on failure.
func (t *TargetRepo) run(ctx context.Context, o cmdOpts, args ...string) ([]byte, error) {
	out, _, err := t.exec(ctx, o, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runError is commandError for a stderr text that is already masked. ctx
// is the caller's context, runCtx the one the command ran under (bounded by
// the network timeout for network commands): the caller's own end wraps
// its context error, the command's bound ErrNetworkTimeout.
func (t *TargetRepo) runError(ctx, runCtx context.Context, args []string, err error, stderr string) error {
	line := "git " + strings.Join(args, " ")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", line, ctxErr)
	}
	if runCtx.Err() != nil {
		return fmt.Errorf("%s: %w after %v", line, ErrNetworkTimeout, t.iso.netTimeout())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &Error{Args: slices.Clone(args), Code: exitErr.ExitCode(), Stderr: stderr}
	}
	return fmt.Errorf("%s: %w", line, err)
}

// checkHeader accepts an Authorization value that git can send as is: not
// empty, printable ASCII, no line breaks.
func checkHeader(h string) error {
	if h == "" {
		return errors.New("the credential gave an empty Authorization header")
	}
	for i := 0; i < len(h); i++ {
		if h[i] < 0x20 || h[i] > 0x7e {
			return errors.New("the credential's Authorization header holds a control or non-ASCII character")
		}
	}
	return nil
}

// minSecret is the shortest string maskSecrets replaces: shorter ones
// would mask ordinary text (redact.MinLen).
const minSecret = 8

// headerSecrets returns the forms of an Authorization value that must not
// be printed, longest first: the value, its credential part and, for Basic,
// the decoded "user:secret" and the secret.
func headerSecrets(h string) []string {
	forms := []string{h}
	if scheme, cred, ok := strings.Cut(h, " "); ok && cred != "" {
		forms = append(forms, cred)
		if strings.EqualFold(scheme, "Basic") {
			if dec, err := base64.StdEncoding.DecodeString(cred); err == nil {
				forms = append(forms, string(dec))
				if _, pass, ok := strings.Cut(string(dec), ":"); ok {
					forms = append(forms, pass)
				}
			}
		}
	}
	out := forms[:0]
	for _, f := range forms {
		if len(f) >= minSecret {
			out = append(out, f)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// maskSecrets replaces every secret in s with "***".
func maskSecrets(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

// masked returns the kept bytes with every secret masked, prefixed with
// "..." when earlier output was dropped. Masking comes first, on the kept
// bytes; but when output was dropped, the kept bytes may start inside a
// secret, whose tail masking cannot recognize: with secrets to mask, that
// partial first line goes too (a header value never spans lines,
// checkHeader).
func (t *tailBuffer) masked(secrets []string) string {
	t.mu.Lock()
	s, truncated := string(t.buf), t.truncated
	t.mu.Unlock()
	if !truncated {
		return maskSecrets(s, secrets)
	}
	if len(secrets) > 0 {
		_, s, _ = strings.Cut(s, "\n")
	}
	return "..." + maskSecrets(s, secrets)
}

// checkRefName rejects what git check-ref-format rejects, so that a name
// can be neither an option nor a revision expression nor a pattern: empty
// components, components starting with '.' or ending in ".lock", "..",
// "@{", a trailing '.' or '/', control characters, spaces and any of
// "~^:?*[\".
func checkRefName(name string) error {
	bad := func(why string) error { return fmt.Errorf("invalid ref name %q: %s", abbrev(name), why) }
	switch {
	case name == "" || name == "@":
		return bad("empty or @")
	case strings.HasPrefix(name, "-"):
		return bad("starts with '-'")
	case strings.HasSuffix(name, "."):
		return bad("ends with '.'")
	case strings.Contains(name, ".."):
		return bad(`holds ".."`)
	case strings.Contains(name, "@{"):
		return bad(`holds "@{"`)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return bad("holds a forbidden character")
		}
	}
	for _, comp := range strings.Split(name, "/") {
		switch {
		case comp == "":
			return bad("has an empty component")
		case strings.HasPrefix(comp, "."):
			return bad("has a component starting with '.'")
		case strings.HasSuffix(comp, ".lock"):
			return bad(`has a component ending in ".lock"`)
		}
	}
	return nil
}

// checkBranch rejects a branch name that refs/heads/<name> would not
// make a valid ref of, and what git check-ref-format --branch refuses
// besides: a leading '-' and "@".
func checkBranch(name string) error {
	switch {
	case strings.HasPrefix(name, "refs/"):
		return fmt.Errorf("invalid branch name %q: give the name without refs/heads/", abbrev(name))
	case strings.HasPrefix(name, "-") || name == "@":
		return fmt.Errorf("invalid branch name %q", abbrev(name))
	}
	if err := checkRefName("refs/heads/" + name); err != nil {
		return fmt.Errorf("invalid branch name %q: %w", abbrev(name), err)
	}
	return nil
}

// checkTreePath rejects paths that are not plain repository paths: empty,
// absolute, with empty, "." or ".." components, or holding NUL.
func checkTreePath(p string) error {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("invalid repository path %q", abbrev(p))
	}
	for _, comp := range strings.Split(p, "/") {
		if comp == "" || comp == "." || comp == ".." {
			return fmt.Errorf("invalid repository path %q", abbrev(p))
		}
	}
	return nil
}
