package ghfake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

// gitTimeout bounds one git step the fake runs on its own.
const gitTimeout = 2 * time.Minute

// Tree entry modes.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
	ModeGitlink    = "160000"
	modeTree       = "040000"
)

// repoConfig is appended to the config of every bare repository: partial
// clones and fetches by id are served, pushes are accepted over HTTP,
// refs/pull/* are hidden from pushes as on GitHub ("deny updating a hidden
// ref"), and no garbage collection runs behind a test's back.
const repoConfig = `[uploadpack]
	allowFilter = true
	allowAnySHA1InWant = true
[http]
	receivepack = true
[receive]
	autogc = false
	hideRefs = refs/pull/
[gc]
	auto = 0
`

// gitEnv is how the fake runs git.
type gitEnv struct {
	dir     string // the bare repositories
	scratch string // temporary indexes and quarantines
	bin     string
	env     []string
	version [3]int
}

// newGitEnv prepares dir (empty or absent) and finds git.
func newGitEnv(dir string) (*gitEnv, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	list, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		return nil, fmt.Errorf("%s is not empty", abs)
	}
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	home, scratch := filepath.Join(abs, ".home"), filepath.Join(abs, ".tmp")
	for _, d := range []string{home, scratch} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return nil, err
		}
	}
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		return nil, err
	}
	g := &gitEnv{dir: abs, scratch: scratch, bin: bin, env: []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if g.version, err = g.runner("").Version(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

// runner returns a git runner in dir that sees neither the machine's
// configuration nor the GIT_* variables of the process.
func (g *gitEnv) runner(dir string, env ...string) *gitx.Git {
	return &gitx.Git{Dir: dir, Bin: g.bin, Env: append(slices.Clone(g.env), env...), Inherit: cleanEnviron}
}

// cleanEnviron is the process environment without GIT_* variables.
func cleanEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
			name = kv[:i+1]
		}
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// rgit is git on one bare repository.
type rgit struct {
	g       *gitx.Git
	format  string
	scratch string
	env     *gitEnv
}

// repoGit returns the runner of r.
func (s *Server) repoGit(r *repo) *rgit {
	return &rgit{g: s.git.runner(r.dir), format: r.objectFormat, scratch: s.git.scratch, env: s.git}
}

// quarantined returns a runner of r that also sees the objects in the
// quarantine directory q, where a push's pack was indexed.
func (s *Server) quarantined(r *repo, q string) *rgit {
	rg := s.repoGit(r)
	rg.g = s.git.runner(r.dir, "GIT_OBJECT_DIRECTORY="+q,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+filepath.Join(r.dir, "objects"))
	return rg
}

// zeroID is the null object id of the repository's format.
func (rg *rgit) zeroID() string {
	if rg.format == "sha256" {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}

// initRepo creates the bare repository of r.
func (s *Server) initRepo(ctx context.Context, r *repo) error {
	dir := filepath.Join(s.git.dir, "r"+strconv.FormatInt(r.id, 10)+".git")
	args := []string{"init", "-q", "--bare", "--initial-branch=" + r.defaultBranch}
	if r.objectFormat == "sha256" {
		args = append(args, "--object-format=sha256")
	}
	if _, err := s.git.runner("").Run(ctx, nil, append(args, dir)...); err != nil {
		return fmt.Errorf("create the repository of %s: %w", r.path(), err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(repoConfig); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	r.dir = dir
	return nil
}

// refs returns the refs under prefix ("refs/heads/", "refs/") with their
// objects.
func (rg *rgit) refs(ctx context.Context, prefix string) (map[string]string, error) {
	out, err := rg.g.Run(ctx, nil, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		id, ref, ok := strings.Cut(line, " ")
		if !ok || !isHexID(id) {
			return nil, fmt.Errorf("git for-each-ref: unexpected line %q", line)
		}
		refs[ref] = id
	}
	return refs, nil
}

// branches returns the branches (name without refs/heads/) with their
// commits.
func (rg *rgit) branches(ctx context.Context) (map[string]string, error) {
	all, err := rg.refs(ctx, "refs/heads/")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for ref, id := range all {
		out[strings.TrimPrefix(ref, "refs/heads/")] = id
	}
	return out, nil
}

// branch returns the tip of a branch, "" when it does not exist.
func (rg *rgit) branch(ctx context.Context, name string) (string, error) {
	refs, err := rg.refs(ctx, "refs/heads/"+name)
	if err != nil {
		return "", err
	}
	return refs["refs/heads/"+name], nil
}

// isAncestor reports whether commit a is b or an ancestor of it.
func (rg *rgit) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := rg.g.Run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	var ge *gitx.Error
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ge) && ge.Code == 1:
		return false, nil
	}
	return false, err
}

// objectType returns the type of an object, "" when it does not exist.
func (rg *rgit) objectType(ctx context.Context, id string) (string, error) {
	if !isHexID(id) {
		return "", nil
	}
	out, err := rg.g.Run(ctx, nil, "cat-file", "-t", id)
	var ge *gitx.Error
	if errors.As(err, &ge) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolve returns the commit a ref names: "HEAD" (the default branch:
// the bare repository's HEAD names it; GitHub reads git/trees/HEAD,
// contents?ref=HEAD and commits/HEAD so, observed read-only 2026-09-29), a
// branch, a tag, "refs/…", or a commit id (full or abbreviated to at least
// 7 hex digits). ok is false when it names none, "" included.
func (rg *rgit) resolve(ctx context.Context, ref string) (string, bool, error) {
	if ref == "" {
		return "", false, nil
	}
	candidates := []string{"refs/heads/" + ref, "refs/tags/" + ref}
	switch {
	case ref == "HEAD":
		candidates = []string{"HEAD"}
	case strings.HasPrefix(ref, "refs/"):
		candidates = []string{ref}
	}
	for _, c := range candidates {
		out, err := rg.g.Run(ctx, nil, "rev-parse", "-q", "--verify", "--end-of-options", c+"^{commit}")
		if err == nil {
			id, err := hexOut("git rev-parse", out)
			return id, err == nil, err
		}
	}
	if len(ref) >= 7 && isHexPrefix(ref) {
		out, err := rg.g.Run(ctx, nil, "rev-parse", "-q", "--verify", "--end-of-options", ref+"^{commit}")
		if err == nil {
			id, err := hexOut("git rev-parse", out)
			return id, err == nil, err
		}
	}
	return "", false, nil
}

// treeOf returns the tree of a commit.
func (rg *rgit) treeOf(ctx context.Context, commit string) (string, error) {
	out, err := rg.g.Run(ctx, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	return hexOut("git rev-parse", out)
}

// catFile returns the content of an object.
func (rg *rgit) catFile(ctx context.Context, typ, id string) ([]byte, error) {
	return rg.g.Run(ctx, nil, "cat-file", typ, id)
}

// treeEntry is one entry of a tree listing.
type treeEntry struct {
	mode, typ, oid string
	size           int64 // -1 for trees and commits
	path           string
}

// lsTree lists the entries of tree-ish rev: the whole tree with recursive
// (trees included, as GitHub lists them), else one level; under dir
// ("" for the root).
func (rg *rgit) lsTree(ctx context.Context, rev, dir string, recursive bool) ([]treeEntry, error) {
	args := []string{"--literal-pathspecs", "ls-tree", "-z", "--long"}
	if recursive {
		args = append(args, "-r", "-t")
	}
	target := rev
	if dir != "" {
		target = rev + ":" + dir
	}
	out, err := rg.g.Run(ctx, nil, append(args, "--end-of-options", target)...)
	if err != nil {
		return nil, err
	}
	var list []treeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return nil, fmt.Errorf("git ls-tree: unexpected record %q", rec)
		}
		size := int64(-1)
		if f[3] != "-" {
			n, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("git ls-tree: bad size in %q", rec)
			}
			size = n
		}
		list = append(list, treeEntry{mode: f[0], typ: f[1], oid: f[2], size: size, path: path})
	}
	return list, nil
}

// entryAt returns the entry at path in commit, ok false when absent.
func (rg *rgit) entryAt(ctx context.Context, commit, path string) (treeEntry, bool, error) {
	dir, base := "", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir, base = path[:i], path[i+1:]
	}
	if dir != "" {
		e, ok, err := rg.entryAt(ctx, commit, dir)
		if err != nil || !ok || e.typ != "tree" {
			return treeEntry{}, false, err
		}
	}
	list, err := rg.lsTree(ctx, commit, dir, false)
	if err != nil {
		return treeEntry{}, false, err
	}
	for _, e := range list {
		if e.path == base {
			if dir != "" {
				e.path = dir + "/" + base
			}
			return e, true, nil
		}
	}
	return treeEntry{}, false, nil
}

// ident is the author or committer of a commit.
type ident struct {
	name, email string
	when        time.Time
}

// String formats i as git writes it.
func (i ident) String() string {
	return fmt.Sprintf("%s <%s> %d +0000", identPart(i.name), identPart(i.email), i.when.Unix())
}

// identPart drops what git does not allow in a name or an email.
func identPart(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, s)
}

// parseIdent parses "name <email> <unix> <zone>".
func parseIdent(line string) ident {
	lt, gt := strings.IndexByte(line, '<'), strings.LastIndexByte(line, '>')
	if lt < 0 || gt < lt {
		return ident{name: line}
	}
	id := ident{name: strings.TrimSpace(line[:lt]), email: line[lt+1 : gt]}
	f := strings.Fields(line[gt+1:])
	if len(f) > 0 {
		if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			id.when = time.Unix(n, 0).UTC()
		}
	}
	return id
}

// commitInfo is a parsed commit object.
type commitInfo struct {
	id        string
	tree      string
	parents   []string
	author    ident
	committer ident
	sig       string // the gpgsig header, unfolded
	message   string
	payload   string // the object without its gpgsig header
}

// readCommit reads and parses a commit object.
func (rg *rgit) readCommit(ctx context.Context, id string) (commitInfo, error) {
	raw, err := rg.catFile(ctx, "commit", id)
	if err != nil {
		return commitInfo{}, err
	}
	c := parseCommit(string(raw))
	c.id = id
	if c.tree == "" {
		return commitInfo{}, fmt.Errorf("commit %s has no tree", id)
	}
	return c, nil
}

// parseCommit parses the text of a commit object.
func parseCommit(raw string) commitInfo {
	head, msg, _ := strings.Cut(raw, "\n\n")
	var c commitInfo
	var payload strings.Builder
	var sig []string
	inSig := false
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, " ") {
			if inSig {
				sig = append(sig, line[1:])
				continue
			}
			payload.WriteString(line + "\n")
			continue
		}
		inSig = false
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.tree = value
		case "parent":
			c.parents = append(c.parents, value)
		case "author":
			c.author = parseIdent(value)
		case "committer":
			c.committer = parseIdent(value)
		case "gpgsig", "gpgsig-sha256":
			inSig = true
			sig = append(sig, value)
			continue
		}
		payload.WriteString(line + "\n")
	}
	c.sig = strings.Join(sig, "\n")
	c.message = msg
	c.payload = payload.String() + "\n" + msg
	return c
}

// commitText builds the text of a commit object. sig, when set, becomes
// the gpgsig header after the committer.
func commitText(tree string, parents []string, author, committer ident, sig, msg string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n", author, committer)
	if sig != "" {
		lines := strings.Split(sig, "\n")
		b.WriteString("gpgsig " + lines[0] + "\n")
		for _, l := range lines[1:] {
			b.WriteString(" " + l + "\n")
		}
	}
	b.WriteString("\n" + msg)
	return b.String()
}

// writeObject writes an object and returns its id.
func (rg *rgit) writeObject(ctx context.Context, typ string, data []byte) (string, error) {
	out, err := rg.g.Run(ctx, bytes.NewReader(data), "hash-object", "-t", typ, "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return hexOut("git hash-object", out)
}

// fileChange is one path of a commit built by the fake.
type fileChange struct {
	path string
	mode string // "" deletes the path
	data []byte // the content, or the target of a symlink
	oid  string // the commit of a gitlink
}

// buildTree writes the tree of parent ("" for none) with changes applied,
// through a temporary index.
func (rg *rgit) buildTree(ctx context.Context, parent string, changes []fileChange) (string, error) {
	dir, err := os.MkdirTemp(rg.scratch, "index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	g := *rg.g
	g.Env = append(slices.Clone(rg.g.Env), "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	if parent != "" {
		if _, err := g.Run(ctx, nil, "read-tree", parent); err != nil {
			return "", err
		}
	}
	var in bytes.Buffer
	for _, c := range changes {
		switch c.mode {
		case "":
			fmt.Fprintf(&in, "0 %s\t%s\x00", rg.zeroID(), c.path)
		case ModeGitlink:
			fmt.Fprintf(&in, "%s %s\t%s\x00", c.mode, c.oid, c.path)
		default:
			oid, err := rg.writeObject(ctx, "blob", c.data)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&in, "%s %s\t%s\x00", c.mode, oid, c.path)
		}
	}
	if in.Len() > 0 {
		if _, err := g.Run(ctx, &in, "update-index", "-z", "--index-info"); err != nil {
			return "", err
		}
	}
	out, err := g.Run(ctx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return hexOut("git write-tree", out)
}

// refTx is one ref of an atomic update: old "" means the ref must not
// exist, new "" deletes it.
type refTx struct {
	ref, old, new string
}

// updateRefs applies updates in one transaction, checking every old value.
func (rg *rgit) updateRefs(ctx context.Context, updates []refTx) error {
	var b strings.Builder
	b.WriteString("start\n")
	for _, u := range updates {
		old := u.old
		if old == "" {
			old = rg.zeroID()
		}
		if u.new == "" {
			fmt.Fprintf(&b, "delete %s %s\n", u.ref, old)
		} else {
			fmt.Fprintf(&b, "update %s %s %s\n", u.ref, u.new, old)
		}
	}
	b.WriteString("prepare\ncommit\n")
	_, err := rg.g.Run(ctx, strings.NewReader(b.String()), "update-ref", "--stdin")
	return err
}

// revList returns the commits reachable from head and from none of not.
func (rg *rgit) revList(ctx context.Context, head string, not []string) ([]string, error) {
	args := []string{"rev-list", head}
	if len(not) > 0 {
		args = append(args, "--not")
		args = append(args, not...)
	}
	out, err := rg.g.Run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// changedPaths returns the paths under prefix that differ between two
// trees or commits ("" for the empty tree), sorted.
func (rg *rgit) changedPaths(ctx context.Context, from, to, prefix string) ([]string, error) {
	if from == "" {
		empty, err := rg.writeObject(ctx, "tree", nil)
		if err != nil {
			return nil, err
		}
		from = empty
	}
	out, err := rg.g.Run(ctx, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", from, to, "--", prefix)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// mergeTree returns the tree of merging theirs into ours: the tree of
// theirs when it contains ours, otherwise a three-way merge with git
// merge-tree --write-tree (git 2.38 or newer).
func (rg *rgit) mergeTree(ctx context.Context, ours, theirs string) (string, error) {
	contained, err := rg.isAncestor(ctx, ours, theirs)
	if err != nil {
		return "", err
	}
	if contained {
		return rg.treeOf(ctx, theirs)
	}
	if slices.Compare(rg.env.version[:], []int{2, 38, 0}) < 0 {
		return "", fmt.Errorf("%w: merging needs git 2.38 or newer", ErrMergeConflict)
	}
	out, err := rg.g.Run(ctx, nil, "merge-tree", "--write-tree", ours, theirs)
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return "", ErrMergeConflict
	}
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return hexOut("git merge-tree", []byte(first))
}

// fetchFrom copies commit id and its history from another bare repository
// into rg (for pull requests from forks), pointing ref at it unless ref is
// "": the objects then stay unreferenced, and no ref of the fake's own
// shows in advertisements.
func (rg *rgit) fetchFrom(ctx context.Context, dir, id, ref string) error {
	spec := id
	if ref != "" {
		spec = "+" + id + ":" + ref
	}
	_, err := rg.g.Run(ctx, nil, "fetch", "-q", "--no-tags", "--no-write-fetch-head", dir, spec)
	return err
}

// ErrMergeConflict is returned by MergePR when the merge has conflicts or
// needs a newer git.
var ErrMergeConflict = errors.New("ghfake: merge conflict")

// hexOut checks that out is one object id.
func hexOut(what string, out []byte) (string, error) {
	id := strings.TrimSpace(string(out))
	if !isHexID(id) {
		return "", fmt.Errorf("%s: unexpected output %q", what, id)
	}
	return strings.ToLower(id), nil
}

// isHexID reports whether s is a full sha1 or sha256 object id.
func isHexID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHexPrefix(s)
}

// isHexPrefix reports whether s is non-empty hex.
func isHexPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isZeroID reports whether s is a null object id.
func isZeroID(s string) bool {
	return isHexID(s) && strings.Trim(s, "0") == ""
}
