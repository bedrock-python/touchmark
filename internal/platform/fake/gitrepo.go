package fake

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// gitMode is the git side of a Platform in git mode (ServeGit). It never
// changes once set.
type gitMode struct {
	dir     string   // the bare repositories, GIT_PROJECT_ROOT of the server
	scratch string   // temporary indexes
	bin     string   // absolute path of the git executable
	env     []string // isolation of every git command the fake runs
	version [3]int
	server  *GitServer
}

// The oldest gits that merge file contents for the human actions: git
// merge-tree --write-tree for merges, with --merge-base for the commits a
// rebase replays.
var (
	contentMerge  = [3]int{2, 38, 0}
	contentReplay = [3]int{2, 40, 0}
)

// gitTimeout bounds one git step of a setup method, a human action or the
// server's bookkeeping, which have no context of their own.
const gitTimeout = 2 * time.Minute

// repoConfig is appended to the config of every bare repository: partial
// clones and fetches by id are served, pushes are accepted over HTTP, no
// garbage collection runs behind a test's back, and Git for Windows takes
// paths NTFS cannot hold (a tab, a quote), since nothing is checked out.
const repoConfig = `[core]
	protectNTFS = false
[uploadpack]
	allowFilter = true
	allowAnySHA1InWant = true
[http]
	receivepack = true
[receive]
	autogc = false
[gc]
	auto = 0
`

// Variables the git server passes to the pre-receive hook of a push.
const (
	hookDenyWorkflows = "FAKE_DENY_WORKFLOWS" // "1": the pusher may not change workflows
	hookDefaultBranch = "FAKE_DEFAULT_BRANCH" // the base of a new branch
	// Branch patterns (shell case syntax, space-separated) whose rulesets
	// hold the pusher: signatures required, force pushes refused, deletions
	// refused.
	hookSigned   = "FAKE_SIGNED_BRANCHES"
	hookNoForce  = "FAKE_NOFORCE_BRANCHES"
	hookNoDelete = "FAKE_NODELETE_BRANCHES"
)

// preReceiveHook is the pre-receive hook of every bare repository, with
// GitHub's refusals and messages, so that gitx reads them as GitHub's:
//   - an identity without the Workflows permission may not create or
//     change a file under .github/workflows: a ref update
//     is judged by its diff, so moving a branch across others' workflow
//     changes counts, and a new ref by its diff from the default branch
//     (or from nothing in an empty repository); PushWorkflows;
//   - the rulesets that hold the pusher (rules.go): the deletion of a
//     branch that refuses it ("Cannot delete this protected branch"), and,
//     for other updates, a force push to a branch that refuses them
//     ("Cannot force-push to this branch"), and commits without a
//     signature header on a branch that requires signatures — the commits
//     the update brings that no branch has yet ("Commits must have
//     verified signatures."); both a GH013 refusal.
var preReceiveHook = strings.NewReplacer("BQ", "`").Replace(`#!/bin/sh
set -f
matches() {
	b=$1
	shift
	for pat in "$@"; do
		case $b in $pat) return 0 ;; esac
	done
	return 1
}
while read -r old new ref; do
	case $new in
	*[!0]*) ;;
	*)
		case $ref in refs/heads/*) ;; *) continue ;; esac
		if [ -n "$FAKE_NODELETE_BRANCHES" ] && matches "${ref#refs/heads/}" $FAKE_NODELETE_BRANCHES; then
			echo "error: GH013: Repository rule violations found for $ref." >&2
			echo "" >&2
			echo "- Cannot delete this protected branch." >&2
			echo "" >&2
			exit 1
		fi
		continue
		;;
	esac
	if [ "$FAKE_DENY_WORKFLOWS" = 1 ]; then
		case $old in
		*[!0]*) from=$old ;;
		*) from=$(git rev-parse -q --verify "refs/heads/$FAKE_DEFAULT_BRANCH^{commit}") || from=$(git hash-object -t tree --stdin </dev/null) ;;
		esac
		path=$(git diff-tree -r --name-only --no-renames "$from" "$new" -- .github/workflows | head -n 1)
		if [ -n "$path" ]; then
			echo "refusing to allow a GitHub App to create or update workflow \BQ$path\BQ without \BQworkflows\BQ permission" >&2
			exit 1
		fi
	fi
	case $ref in refs/heads/*) ;; *) continue ;; esac
	branch=${ref#refs/heads/}
	forced=
	unsigned=
	if [ -n "$FAKE_NOFORCE_BRANCHES" ] && matches "$branch" $FAKE_NOFORCE_BRANCHES; then
		case $old in
		*[!0]*) git merge-base --is-ancestor "$old" "$new" || forced=1 ;;
		esac
	fi
	if [ -n "$FAKE_SIGNED_BRANCHES" ] && matches "$branch" $FAKE_SIGNED_BRANCHES; then
		for c in $(git rev-list "$new" --not --branches); do
			git cat-file commit "$c" | sed '/^$/q' | grep -q '^gpgsig' || unsigned="$unsigned $c"
		done
	fi
	if [ -n "$forced" ] || [ -n "$unsigned" ]; then
		echo "error: GH013: Repository rule violations found for $ref." >&2
		echo "" >&2
		if [ -n "$forced" ]; then
			echo "- Cannot force-push to this branch" >&2
			echo "" >&2
		fi
		if [ -n "$unsigned" ]; then
			echo "- Commits must have verified signatures." >&2
			for c in $unsigned; do
				echo "  $c" >&2
			done
			echo "" >&2
		fi
		exit 1
	fi
done
exit 0
`)

// newGitMode prepares dir (empty or absent) for the bare repositories and
// finds git.
func newGitMode(ctx context.Context, dir string) (*gitMode, error) {
	if dir == "" {
		return nil, errors.New("empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	if list, err := os.ReadDir(abs); err != nil {
		return nil, err
	} else if len(list) > 0 {
		return nil, fmt.Errorf("%s is not empty", abs)
	}
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, err
	}
	home := filepath.Join(abs, ".home")
	scratch := filepath.Join(abs, ".tmp")
	for _, d := range []string{home, scratch} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return nil, err
		}
	}
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		return nil, err
	}
	g := &gitMode{
		dir:     abs,
		scratch: scratch,
		bin:     bin,
		env: []string{
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + global,
			"GIT_ATTR_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + home,
		},
	}
	if g.version, err = g.runner("").Version(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

// runner returns a git runner in dir ("" for none) that sees neither the
// machine's configuration nor the GIT_* variables of the process.
func (g *gitMode) runner(dir string) *gitx.Git {
	return &gitx.Git{Dir: dir, Bin: g.bin, Env: g.env, Inherit: cleanEnviron}
}

// cleanEnviron is the process environment without GIT_* variables, which
// could point git at another repository or configuration.
func cleanEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
			name = kv[:i+1] // Windows keeps entries like "=C:=C:\x"
		}
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// repoGit is one bare repository of the git mode.
type repoGit struct {
	g       *gitx.Git
	format  string // "sha1" or "sha256"
	scratch string
	version [3]int
}

// repoGit returns the runner of the bare repository of s. Called with mu
// held.
func (p *Platform) repoGit(s *repoState) *repoGit {
	return &repoGit{g: p.git.runner(s.dir), format: s.repo.ObjectFormat, scratch: p.git.scratch, version: p.git.version}
}

// initRepo creates the bare repository of s on its default branch and
// object format. Called with mu held.
func (p *Platform) initRepo(ctx context.Context, s *repoState) error {
	dir := filepath.Join(p.git.dir, repoDirName(s.repo.ID))
	args := []string{"init", "-q", "--bare", "--initial-branch=" + s.repo.DefaultBranch}
	if s.repo.ObjectFormat == "sha256" {
		args = append(args, "--object-format=sha256")
	}
	if _, err := p.git.runner("").Run(ctx, nil, append(args, dir)...); err != nil {
		return fmt.Errorf("create the repository of %s: %w", s.repo.Path, err)
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
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(preReceiveHook), 0o755); err != nil {
		return err
	}
	s.dir, s.refs = dir, map[string]string{}
	return nil
}

// convertRepo turns a repository of the memory mode into a bare
// repository: its files become one commit on the default branch. Called
// with gitMu and mu held.
func (p *Platform) convertRepo(ctx context.Context, s *repoState) error {
	if err := p.initRepo(ctx, s); err != nil {
		return err
	}
	if len(s.entries) == 0 {
		return nil
	}
	paths := slices.Sorted(maps.Keys(s.entries))
	changes := make([]fileChange, len(paths))
	for i, path := range paths {
		e := s.entries[path]
		changes[i] = fileChange{path: path, mode: e.Mode, data: p.blobs[e.OID], oid: e.OID}
	}
	sign := p.setupSign()
	branch := s.repo.DefaultBranch
	head, err := p.repoGit(s).importCommit(ctx, branch, "", changes, sign, sign, "fake: files set before git mode\n")
	if err != nil {
		return fmt.Errorf("commit the files of %s: %w", s.repo.Path, err)
	}
	// The ids of the memory mode are sha1 even in a sha256 repository.
	return p.refsMoved(ctx, s, []refChange{{branch: branch, new: head}}, nil, false, false)
}

// setupSign is the author and committer of the commits of setup methods.
// Called with mu held.
func (p *Platform) setupSign() ident {
	return ident{name: "fake", email: "fake@" + p.host, when: p.now()}
}

// repoDirName is the directory of the bare repository of id: the id when
// it is a plain lowercase word that every file system takes as a name, its
// hex encoding otherwise (ids may collide on a case-insensitive file
// system, hold separators, or name a Windows device: "con.git" is the
// console there, whatever the extension).
func repoDirName(id string) string {
	plain := id != "" && len(id) <= 64
	for i := 0; plain && i < len(id); i++ {
		c := id[i]
		plain = c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c == '-' || c == '_'
	}
	if plain && pathx.Validate(id+".git") == nil {
		return id + ".git"
	}
	return "x" + hex.EncodeToString([]byte(id)) + ".git"
}

// ident is the author or committer of a commit.
type ident struct {
	name, email string
	when        time.Time
}

// String formats i as git writes it: "name <email> <unix time> +0000".
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

// objectID is the id git gives an object of type typ with data in a
// repository of format ("sha1" unless "sha256").
func objectID(format, typ string, data []byte) string {
	h := sha1.New()
	if format == "sha256" {
		h = sha256.New()
	}
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// zeroID is the null object id of format.
func (r *repoGit) zeroID() string {
	if r.format == "sha256" {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}

// fileChange is one path of a commit written with fast-import.
type fileChange struct {
	path string
	mode string // "" deletes the path
	data []byte // the content, or the target of a symlink
	oid  string // the commit of a gitlink
}

// importCommit writes a commit on branch with fast-import, from parent (""
// for a root commit), and moves the branch to it; it returns the commit.
// The caller serializes ref changes (gitMu), so parent is the branch's tip
// or the branch is new.
func (r *repoGit) importCommit(ctx context.Context, branch, parent string, changes []fileChange, author, committer ident, msg string) (string, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "commit refs/heads/%s\nmark :1\nauthor %s\ncommitter %s\ndata %d\n%s\n", branch, author, committer, len(msg), msg)
	if parent != "" {
		fmt.Fprintf(&b, "from %s\n", parent)
	}
	for _, c := range changes {
		path := fastImportPath(c.path)
		switch c.mode {
		case "":
			fmt.Fprintf(&b, "D %s\n", path)
		case ModeGitlink:
			fmt.Fprintf(&b, "M %s %s %s\n", c.mode, c.oid, path)
		default:
			fmt.Fprintf(&b, "M %s inline %s\ndata %d\n", c.mode, path, len(c.data))
			b.Write(c.data)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nget-mark :1\ndone\n")
	out, err := r.g.Run(ctx, &b, "fast-import", "--quiet", "--done")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if !isHexID(id) {
		return "", fmt.Errorf("git fast-import: unexpected output %q", id)
	}
	return strings.ToLower(id), nil
}

// fastImportPath quotes path C-style where fast-import needs it: a leading
// double quote or a control character.
func fastImportPath(path string) string {
	plain := !strings.HasPrefix(path, `"`)
	for i := 0; plain && i < len(path); i++ {
		plain = path[i] >= 0x20 && path[i] != 0x7f
	}
	if plain {
		return path
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(path); i++ {
		switch c := path[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\%03o`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// commitObject writes a commit object as given (hash-object -t commit) and
// returns its id. author and committer are formatted ident lines.
func (r *repoGit) commitObject(ctx context.Context, tree string, parents []string, author, committer, msg string) (string, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, parent := range parents {
		fmt.Fprintf(&b, "parent %s\n", parent)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s", author, committer, msg)
	out, err := r.g.Run(ctx, &b, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return r.id("git hash-object", out)
}

// id checks that out is one object id.
func (r *repoGit) id(what string, out []byte) (string, error) {
	id := strings.TrimSpace(string(out))
	if !isHexID(id) {
		return "", fmt.Errorf("%s: unexpected output %q", what, id)
	}
	return strings.ToLower(id), nil
}

// updateRef moves branch from old ("" when it must not exist) to new (""
// deletes it), atomically.
func (r *repoGit) updateRef(ctx context.Context, branch, newID, oldID string) error {
	if oldID == "" {
		oldID = r.zeroID()
	}
	ref := "refs/heads/" + branch
	var err error
	if newID == "" {
		_, err = r.g.Run(ctx, nil, "update-ref", "-d", ref, oldID)
	} else {
		_, err = r.g.Run(ctx, nil, "update-ref", ref, newID, oldID)
	}
	return err
}

// branches returns every branch with its commit.
func (r *repoGit) branches(ctx context.Context) (map[string]string, error) {
	out, err := r.g.Run(ctx, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/")
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
		name, isBranch := strings.CutPrefix(ref, "refs/heads/")
		if !ok || !isBranch || !isHexID(id) {
			return nil, fmt.Errorf("git for-each-ref: unexpected line %q", line)
		}
		refs[name] = id
	}
	return refs, nil
}

// entries lists the tree of rev: path → entry, trees left out.
func (r *repoGit) entries(ctx context.Context, rev string) (map[string]snapshot.Entry, error) {
	list, err := r.g.LsTree(ctx, rev, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]snapshot.Entry, len(list))
	for _, e := range list {
		out[e.Path] = snapshot.Entry{Mode: e.Mode, OID: e.OID}
	}
	return out, nil
}

// treeID returns the tree of a commit id.
func (r *repoGit) treeID(ctx context.Context, rev string) (string, error) {
	out, err := r.g.Run(ctx, nil, "rev-parse", "--verify", rev+"^{tree}")
	if err != nil {
		return "", err
	}
	return r.id("git rev-parse", out)
}

// isAncestor reports whether commit a is b or an ancestor of it.
func (r *repoGit) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.g.Run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	var ge *gitx.Error
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ge) && ge.Code == 1:
		return false, nil
	}
	return false, err
}

// mergeBase returns the best common ancestor of a and b, "" when they have
// none.
func (r *repoGit) mergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.g.Run(ctx, nil, "merge-base", a, b)
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return r.id("git merge-base", out)
}

// isCommit reports whether id names a commit of the repository.
func (r *repoGit) isCommit(ctx context.Context, id string) (bool, error) {
	out, err := r.g.Run(ctx, nil, "cat-file", "-t", id)
	var ge *gitx.Error
	if errors.As(err, &ge) {
		return false, nil // unknown object
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "commit", nil
}

// readBlob returns the content of a blob.
func (r *repoGit) readBlob(ctx context.Context, oid string) ([]byte, error) {
	return r.g.Run(ctx, nil, "cat-file", "blob", oid)
}

// commitRef is a commit with its parents.
type commitRef struct {
	id      string
	parents []string
}

// revList returns the commits reachable from head but not from base,
// oldest first in topological order; with noMerges, merge commits are left
// out.
func (r *repoGit) revList(ctx context.Context, base, head string, noMerges bool) ([]commitRef, error) {
	args := []string{"rev-list", "--reverse", "--topo-order", "--parents"}
	if noMerges {
		args = append(args, "--no-merges")
	}
	out, err := r.g.Run(ctx, nil, append(args, head, "--not", base)...)
	if err != nil {
		return nil, err
	}
	var list []commitRef
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		list = append(list, commitRef{id: f[0], parents: f[1:]})
	}
	return list, nil
}

// commitInfo is what a replay keeps of a commit.
type commitInfo struct {
	tree    string
	parents []string
	author  string // "name <email> <time> <zone>"
	message string
}

// readCommit parses a commit object.
func (r *repoGit) readCommit(ctx context.Context, id string) (commitInfo, error) {
	out, err := r.g.Run(ctx, nil, "cat-file", "commit", id)
	if err != nil {
		return commitInfo{}, err
	}
	head, msg, _ := strings.Cut(string(out), "\n\n")
	var c commitInfo
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, " ") {
			continue // a continuation line of a multi-line header (gpgsig)
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.tree = value
		case "parent":
			c.parents = append(c.parents, value)
		case "author":
			c.author = value
		}
	}
	if c.tree == "" || c.author == "" {
		return commitInfo{}, fmt.Errorf("git cat-file commit %s: no tree or author", id)
	}
	c.message = msg
	return c, nil
}

// mergeInto returns the tree of merging theirs into ours. contained says
// that theirs already contains ours, so the result is theirs' tree.
func (r *repoGit) mergeInto(ctx context.Context, ours, theirs string, contained bool) (string, error) {
	if contained {
		return r.treeID(ctx, theirs)
	}
	base, err := r.mergeBase(ctx, ours, theirs)
	if err != nil {
		return "", err
	}
	if base == "" {
		return "", fmt.Errorf("%w: %s and %s share no history", ErrMergeConflict, ours, theirs)
	}
	return r.merge(ctx, base, ours, theirs, false)
}

// merge returns the tree of a three-way merge of theirs into ours against
// base. It works path by path, on any git, when no path changed on both
// sides in different ways; otherwise it merges contents with git
// merge-tree --write-tree (git ≥ 2.38), which finds the merge base itself
// unless explicit asks it to use base (--merge-base, git ≥ 2.40, for the
// commits a rebase replays); an older git gives ErrGitTooOld. Conflicts
// are ErrMergeConflict.
func (r *repoGit) merge(ctx context.Context, base, ours, theirs string, explicit bool) (string, error) {
	var sides [3]map[string]snapshot.Entry
	for i, rev := range []string{base, ours, theirs} {
		e, err := r.entries(ctx, rev)
		if err != nil {
			return "", err
		}
		sides[i] = e
	}
	if merged, ok := mergePaths(sides[0], sides[1], sides[2]); ok {
		return r.writeTree(ctx, merged)
	}
	need, args := contentMerge, []string{"merge-tree", "--write-tree"}
	if explicit {
		need, args = contentReplay, append(args, "--merge-base="+base)
	}
	if slices.Compare(r.version[:], need[:]) < 0 {
		return "", fmt.Errorf("%w: merging %s into %s needs git %d.%d or newer (have %d.%d.%d)", ErrGitTooOld,
			theirs, ours, need[0], need[1], r.version[0], r.version[1], r.version[2])
	}
	out, err := r.g.Run(ctx, nil, append(args, ours, theirs)...)
	var ge *gitx.Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return "", fmt.Errorf("%w: merging %s into %s", ErrMergeConflict, theirs, ours)
	}
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return r.id("git merge-tree", []byte(first))
}

// mergePaths merges theirs into ours against base path by path: a path
// takes the side that changed it, or either when both changed it the same
// way. It fails when both sides changed a path differently, or when the
// result would hold a file where a directory is needed.
func mergePaths(base, ours, theirs map[string]snapshot.Entry) (map[string]snapshot.Entry, bool) {
	same := func(a map[string]snapshot.Entry, b map[string]snapshot.Entry, path string) bool {
		x, xok := a[path]
		y, yok := b[path]
		return xok == yok && x == y
	}
	out := maps.Clone(ours)
	paths := map[string]bool{}
	for _, side := range []map[string]snapshot.Entry{base, ours, theirs} {
		for path := range side {
			paths[path] = true
		}
	}
	for path := range paths {
		switch {
		case same(theirs, base, path): // theirs left it: ours stands
		case same(ours, base, path) || same(ours, theirs, path):
			if e, ok := theirs[path]; ok {
				out[path] = e
			} else {
				delete(out, path)
			}
		default:
			return nil, false
		}
	}
	for path := range out {
		for i := strings.IndexByte(path, '/'); i > 0; i = next(path, i) {
			if _, ok := out[path[:i]]; ok {
				return nil, false
			}
		}
	}
	return out, true
}

// next returns the index of the '/' after the one at i in path, or -1.
func next(path string, i int) int {
	j := strings.IndexByte(path[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// writeTree writes the tree of entries through a temporary index.
func (r *repoGit) writeTree(ctx context.Context, entries map[string]snapshot.Entry) (string, error) {
	dir, err := os.MkdirTemp(r.scratch, "index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	g := *r.g
	g.Env = append(slices.Clone(r.g.Env), "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var in bytes.Buffer
	for _, path := range paths {
		e := entries[path]
		fmt.Fprintf(&in, "%s %s\t%s\x00", e.Mode, e.OID, path)
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
	return r.id("git write-tree", out)
}

// replay copies the commits of head that onto lacks onto onto, oldest
// first, keeping their authors and messages with committer as the
// committer, and returns the new tip. With skipMerges merge commits are
// left out, as git rebase does; without, they are an error. A copy that
// would change nothing is dropped.
func (r *repoGit) replay(ctx context.Context, onto, head string, committer ident, skipMerges bool) (string, error) {
	commits, err := r.revList(ctx, onto, head, skipMerges)
	if err != nil {
		return "", err
	}
	cur := onto
	curTree, err := r.treeID(ctx, cur)
	if err != nil {
		return "", err
	}
	for _, c := range commits {
		if len(c.parents) != 1 {
			return "", fmt.Errorf("%s has %d parents: only single-parent commits are replayed", c.id, len(c.parents))
		}
		info, err := r.readCommit(ctx, c.id)
		if err != nil {
			return "", err
		}
		parentTree, err := r.treeID(ctx, c.parents[0])
		if err != nil {
			return "", err
		}
		tree := info.tree
		if parentTree != curTree {
			if tree, err = r.merge(ctx, c.parents[0], cur, c.id, true); err != nil {
				return "", err
			}
		}
		if tree == curTree {
			continue
		}
		if cur, err = r.commitObject(ctx, tree, []string{cur}, info.author, committer.String(), info.message); err != nil {
			return "", err
		}
		curTree = tree
	}
	return cur, nil
}
