// Package gitx runs git as a subprocess with an argv (never a shell) and
// parses its machine-readable output.
//
// Every command gets:
//   - "-c core.quotePath=false -c color.ui=false -c log.showSignature=false";
//   - GIT_TERMINAL_PROMPT=0, GIT_OPTIONAL_LOCKS=0, LC_ALL=C, LANGUAGE=C;
//   - no inherited variables that select another repository or change
//     pathspec matching (GIT_DIR, GIT_LITERAL_PATHSPECS, …; see Run), nor
//     touchmark's own (TOUCHMARK_*: credentials among them);
//   - NUL-separated output (-z) wherever git offers it;
//   - on a cancelled context, a stop of the whole process tree it started
//     (SIGTERM to its process group on Unix, so git cleans up its lock
//     files; every descendant terminated on Windows, where git.exe may be a
//     launcher of the real git).
//
// Hub-side reads (history, trees, blobs) do not depend on user config.
// Target-side hashing (HashPaths) deliberately uses the target's config and
// attributes, because identity must match what git would store there.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinVersion is the oldest git that local commands support.
// distribute and plan require a newer one, DeliveryMinVersion.
var MinVersion = [3]int{2, 31, 0}

// ErrNotFound is returned when a path does not exist at a revision.
// Blobs.Read and Sizes also wrap it for object ids git does not have.
var ErrNotFound = errors.New("not found")

// Git runs git in Dir.
type Git struct {
	Dir string
	// Bin is the git executable; "git" when empty.
	Bin string
	// Env is appended to the process environment after the defaults.
	Env []string
	// Inherit returns the environment git starts from, before droppedEnv
	// is removed and the defaults and Env are appended; os.Environ when
	// nil. A caller that must keep git away from the machine's config
	// filters it here.
	Inherit func() []string
}

// New returns a Git for dir.
func New(dir string) *Git { return &Git{Dir: dir} }

// Error is a failed git command with its stderr.
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("git")
	for _, a := range e.Args {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	fmt.Fprintf(&b, ": exit %d", e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		b.WriteString(": ")
		b.WriteString(s)
	}
	return b.String()
}

// stderrLimit is how much of a command's stderr is kept: the tail, where git
// prints the fatal message.
const stderrLimit = 4 << 10

// waitDelay bounds how long Wait waits for output pipes after the process
// exits or the context is cancelled.
const waitDelay = 10 * time.Second

// globalArgs precede every command's own arguments.
var globalArgs = []string{
	"-c", "core.quotePath=false",
	"-c", "color.ui=false",
	"-c", "log.showSignature=false",
}

// defaultEnv is appended to the inherited environment of every command.
var defaultEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_OPTIONAL_LOCKS=0",
	"LC_ALL=C",
	"LANGUAGE=C",
}

// droppedEnv lists inherited variables that are not passed to git; Git.Env
// may still set them explicitly. Neither is any variable whose name starts
// with droppedPrefix: touchmark's own, among them the write credentials and
// signing keys a CI job has in its environment until distribute reads them.
// git needs none of them, and whatever git starts (a hook, a filter, a
// credential helper of a repository's config) must not see them.
//
// The first group would point git at another repository, index or object
// store than Dir (git sets them for hooks, for example): the non-config
// entries of `git rev-parse --local-env-vars`. The second group changes how
// pathspecs match; GIT_LITERAL_PATHSPECS, for one, would silently turn the
// ":(top)" pathspec of History into a path that matches nothing.
var droppedEnv = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_OBJECT_DIRECTORY",
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_GRAFT_FILE",
	"GIT_INDEX_FILE",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_REPLACE_REF_BASE",
	"GIT_PREFIX",
	"GIT_INTERNAL_SUPER_PREFIX",
	"GIT_SHALLOW_FILE",
	"GIT_COMMON_DIR",

	"GIT_LITERAL_PATHSPECS",
	"GIT_GLOB_PATHSPECS",
	"GIT_NOGLOB_PATHSPECS",
	"GIT_ICASE_PATHSPECS",
}

// Run runs git with args and returns stdout. stdin may be nil.
//
// A command that exits non-zero returns an *Error. Variables that locate
// another repository (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, …) or change
// pathspec matching (GIT_LITERAL_PATHSPECS, …) are not inherited from the
// environment; set them through Env when needed. Neither are touchmark's
// own variables (TOUCHMARK_*).
func (g *Git) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	out, err := g.runPartial(ctx, stdin, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runPartial is Run that also returns what the command printed on stdout
// before it failed.
func (g *Git) runPartial(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := g.command(ctx, args)
	var stdout bytes.Buffer
	stderr := newTailBuffer(stderrLimit)
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), commandError(ctx, args, err, stderr)
	}
	return stdout.Bytes(), nil
}

// command builds the exec.Cmd for args with the global options and the
// environment applied.
func (g *Git) command(ctx context.Context, args []string) *exec.Cmd {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	argv := make([]string, 0, 2+len(globalArgs)+len(args))
	if g.Dir != "" {
		argv = append(argv, "-C", g.Dir)
	}
	argv = append(argv, globalArgs...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = g.environ()
	cmd.WaitDelay = waitDelay
	configureCancel(cmd)
	traceCommand(cmd.Args, cmd.Env)
	return cmd
}

// environ returns the inherited environment (g.Inherit, or the process's)
// without droppedEnv, followed by the defaults and g.Env.
func (g *Git) environ() []string {
	inherit := g.Inherit
	if inherit == nil {
		inherit = os.Environ
	}
	inherited := inherit()
	env := make([]string, 0, len(inherited)+len(defaultEnv)+len(g.Env))
	for _, kv := range inherited {
		if !isDroppedEnv(kv) {
			env = append(env, kv)
		}
	}
	env = append(env, defaultEnv...)
	return append(env, g.Env...)
}

// droppedPrefix starts the names of touchmark's variables, which no git
// process inherits (see droppedEnv).
const droppedPrefix = "TOUCHMARK_"

// isDroppedEnv reports whether the "NAME=value" entry kv sets one of
// droppedEnv, or a variable of touchmark's. Names compare
// case-insensitively, as on Windows.
func isDroppedEnv(kv string) bool {
	name := envName(kv)
	if len(name) >= len(droppedPrefix) && strings.EqualFold(name[:len(droppedPrefix)], droppedPrefix) {
		return true
	}
	return slices.ContainsFunc(droppedEnv, func(v string) bool { return strings.EqualFold(v, name) })
}

// envName returns the name of a "NAME=value" entry. Windows keeps per-drive
// entries such as "=C:=C:\x", whose name starts with '='.
func envName(kv string) string {
	start := 0
	if strings.HasPrefix(kv, "=") {
		start = 1
	}
	if i := strings.IndexByte(kv[start:], '='); i >= 0 {
		return kv[:start+i]
	}
	return kv
}

// commandError converts the error of a finished command into an *Error when
// git ran and exited non-zero, and wraps it with the command line otherwise.
func commandError(ctx context.Context, args []string, err error, stderr *tailBuffer) error {
	line := "git " + strings.Join(args, " ")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", line, ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &Error{Args: slices.Clone(args), Code: exitErr.ExitCode(), Stderr: stderr.String()}
	}
	return fmt.Errorf("%s: %w", line, err)
}

// tailBuffer is an io.Writer that keeps the last limit bytes written to it.
// It is safe for concurrent use: exec copies stderr on its own goroutine.
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0:0], t.buf[over:]...)
		t.truncated = true
	}
	return len(p), nil
}

// String returns the kept bytes, prefixed with "..." when earlier output was
// dropped.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.truncated {
		return "..." + string(t.buf)
	}
	return string(t.buf)
}

// Version returns git's version as [major, minor, patch].
// "2.33.0.windows.2" gives [2, 33, 0].
func (g *Git) Version(ctx context.Context) ([3]int, error) {
	out, err := g.Run(ctx, nil, "version")
	if err != nil {
		return [3]int{}, err
	}
	return parseVersion(string(out))
}

// parseVersion parses the output of `git version`, such as
// "git version 2.33.0.windows.2" or "git version 2.45.1 (Apple Git-154)".
// A missing patch number reads as 0.
func parseVersion(s string) ([3]int, error) {
	var v [3]int
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "git version ")
	fields := strings.Fields(rest)
	if !ok || len(fields) == 0 {
		return v, fmt.Errorf("unexpected git version output %q", s)
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) < 2 {
		return v, fmt.Errorf("unexpected git version %q", fields[0])
	}
	for i := 0; i < len(v) && i < len(parts); i++ {
		n, ok := leadingInt(parts[i])
		if !ok {
			if i < 2 {
				return v, fmt.Errorf("unexpected git version %q", fields[0])
			}
			break
		}
		v[i] = n
	}
	return v, nil
}

// leadingInt parses the decimal digits at the start of s ("0-rc1" gives 0).
func leadingInt(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

// CheckVersion fails if git is older than min.
func (g *Git) CheckVersion(ctx context.Context, min [3]int) error {
	v, err := g.Version(ctx)
	if err != nil {
		return err
	}
	if slices.Compare(v[:], min[:]) < 0 {
		return fmt.Errorf("git %s is too old: git %s or newer is required", formatVersion(v), formatVersion(min))
	}
	return nil
}

func formatVersion(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// TopLevel returns the root of the work tree containing Dir, and false when
// Dir is not inside a git work tree.
func (g *Git) TopLevel(ctx context.Context) (string, bool, error) {
	out, err := g.Run(ctx, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if isNotWorkTree(err) {
			return "", false, nil
		}
		return "", false, err
	}
	dir := trimEOL(out)
	if dir == "" {
		// Old git prints nothing in a bare repository.
		return "", false, nil
	}
	return filepath.Clean(filepath.FromSlash(dir)), true, nil
}

// isNotWorkTree reports whether err is git saying that there is no work tree
// here: no repository at all, or a bare repository or .git directory. Other
// failures (a missing Dir, an unsafe repository) are real errors.
func isNotWorkTree(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return strings.Contains(e.Stderr, "not a git repository (or any") ||
		strings.Contains(e.Stderr, "must be run in a work tree")
}

// IsShallow reports whether the repository is a shallow clone.
func (g *Git) IsShallow(ctx context.Context) (bool, error) {
	out, err := g.Run(ctx, nil, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}
	switch s := trimEOL(out); s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("git rev-parse --is-shallow-repository: unexpected output %q", s)
	}
}

// ObjectFormat returns "sha1" or "sha256".
func (g *Git) ObjectFormat(ctx context.Context) (string, error) {
	const opt = "--show-object-format"
	out, err := g.Run(ctx, nil, "rev-parse", opt)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && strings.Contains(e.Stderr, opt) {
			return "sha1", nil // git without the option predates sha256
		}
		return "", err
	}
	switch s := trimEOL(out); s {
	case "sha1", "sha256":
		return s, nil
	case opt:
		// Old rev-parse echoes options it does not know.
		return "sha1", nil
	default:
		return "", fmt.Errorf("git rev-parse %s: unexpected output %q", opt, s)
	}
}

// RevParse resolves rev to a full object id.
func (g *Git) RevParse(ctx context.Context, rev string) (string, error) {
	if err := checkRev(rev); err != nil {
		return "", err
	}
	out, err := g.Run(ctx, nil, "rev-parse", "--verify", rev)
	if err != nil {
		return "", err
	}
	id := trimEOL(out)
	if !isOID(id) {
		return "", fmt.Errorf("git rev-parse --verify %s: unexpected output %q", rev, id)
	}
	return id, nil
}

// TreeEntry is one line of `git ls-tree -r -z --full-tree --long`.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "160000"
	Type string // "blob" or "commit"
	OID  string
	Size int64  // -1 for non-blobs
	Path string // full path from the repository root
}

// LsTree lists every entry under prefix (a directory path, "" for all) at
// rev, recursively. It returns an empty list when prefix does not exist.
func (g *Git) LsTree(ctx context.Context, rev, prefix string) ([]TreeEntry, error) {
	return g.lsTree(ctx, rev, cleanPrefix(prefix), true)
}

// lsTree runs `git ls-tree -z --full-tree --long` with path as a literal
// pathspec ("" for none), recursively or not, and keeps the entries at or
// under path.
func (g *Git) lsTree(ctx context.Context, rev, path string, recursive bool) ([]TreeEntry, error) {
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	args := []string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", "--long"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, rev)
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := g.Run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseLsTree(out, path)
}

// parseLsTree parses NUL-terminated `ls-tree --long` records
// "<mode> SP <type> SP <oid> SP+ <size> TAB <path>" and drops entries that
// are not at or under prefix.
func parseLsTree(out []byte, prefix string) ([]TreeEntry, error) {
	var entries []TreeEntry
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if rec == "" {
			continue
		}
		e, err := parseTreeRecord(rec)
		if err != nil {
			return nil, err
		}
		if prefix == "" || under(e.Path, prefix) {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func parseTreeRecord(rec string) (TreeEntry, error) {
	meta, path, ok := strings.Cut(rec, "\t")
	f := strings.Fields(meta)
	if !ok || path == "" || len(f) != 4 || !isOID(f[2]) {
		return TreeEntry{}, fmt.Errorf("git ls-tree: unexpected record %q", rec)
	}
	size := int64(-1)
	if f[3] != "-" {
		n, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil || n < 0 {
			return TreeEntry{}, fmt.Errorf("git ls-tree: bad size in record %q", rec)
		}
		size = n
	}
	return TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Size: size, Path: path}, nil
}

// ShowFile returns the content of path at rev, or ErrNotFound.
func (g *Git) ShowFile(ctx context.Context, rev, path string) ([]byte, error) {
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	if err := checkRepoPath(path); err != nil {
		return nil, err
	}
	out, err := g.Run(ctx, nil, "cat-file", "blob", rev+":"+path)
	if err == nil {
		return out, nil
	}
	var gitErr *Error
	if !errors.As(err, &gitErr) {
		return nil, err
	}
	// git reports a missing path and a bad revision alike; look at the tree
	// to tell them apart. A failing lookup means the revision is the problem.
	entries, lerr := g.lsTree(ctx, rev, path, false)
	if lerr != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Path == path {
			return nil, fmt.Errorf("%s:%s is a %s, not a file: %w", rev, path, e.Type, err)
		}
	}
	return nil, fmt.Errorf("%s:%s: %w", rev, path, ErrNotFound)
}

// checkRev rejects revisions git would parse as options.
func checkRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return fmt.Errorf("invalid git revision %q", rev)
	}
	return nil
}

// checkRepoPath rejects paths that "<rev>:<path>" would not read as a path
// from the repository root.
func checkRepoPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || p == "." || p == ".." ||
		strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("invalid repository path %q", p)
	}
	return nil
}

// cleanPrefix turns a directory prefix into a pathspec: no trailing slash.
func cleanPrefix(prefix string) string { return strings.TrimRight(prefix, "/") }

// under reports whether repository path p is dir or inside it.
func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// isOID reports whether s is a full lowercase hex object id (sha1 or sha256).
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// trimEOL returns out as a string without its final line ending.
func trimEOL(out []byte) string {
	s := strings.TrimSuffix(string(out), "\n")
	return strings.TrimSuffix(s, "\r")
}
