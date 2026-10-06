package provenance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Messages for entries a pack may not hold.
const (
	msgSymlink   = "symlink not allowed: packs ship regular files only"
	msgSubmodule = "submodule not allowed: packs ship regular files only"
	msgSpecial   = "not a regular file: packs ship regular files only"
)

// collector gathers the files and problems found while reading packs.
type collector struct {
	cur      Current
	problems []Problem
	// packOK caches the pack name check; an invalid name is reported once.
	packOK map[string]bool
}

func newCollector() *collector {
	return &collector{cur: Current{}, packOK: map[string]bool{}}
}

func (c *collector) problem(pack, path, msg string) {
	c.problems = append(c.problems, Problem{Pack: pack, Path: path, Message: msg})
}

// result returns the files and the problems in a deterministic order.
func (c *collector) result() (Current, []Problem) {
	sortProblems(c.problems)
	return c.cur, c.problems
}

// checkPack validates a pack name once and reports it when invalid.
func (c *collector) checkPack(pack string) bool {
	if ok, seen := c.packOK[pack]; seen {
		return ok
	}
	err := checkPackName(pack)
	c.packOK[pack] = err == nil
	if err != nil {
		c.problem(pack, "", err.Error())
	}
	return err == nil
}

// locate splits a hub path of a non-directory entry into pack and repository
// path. It reports a problem and returns false when the entry lies outside
// any pack, in an invalid pack or at an invalid path. kind names the entry
// for the outside-any-pack message ("file", "symlink", …).
func (c *collector) locate(hub, kind string) (pack, path string, ok bool) {
	pack, path, ok = splitHubPath(hub)
	if !ok {
		if hub == PacksDir {
			c.problem("", "", fmt.Sprintf("%s: must be a directory of packs, not a %s", PacksDir, kind))
		} else {
			c.problem("", "", fmt.Sprintf("%s: %s outside any pack: pack contents live in %s/<pack>/", display(hub), kind, PacksDir))
		}
		return "", "", false
	}
	if !c.checkPack(pack) {
		return "", "", false
	}
	if err := pathx.Validate(path); err != nil {
		c.problem(pack, path, err.Error())
		return "", "", false
	}
	return pack, path, true
}

// add records a file a pack ships.
func (c *collector) add(f File) {
	files := c.cur[f.Pack]
	if files == nil {
		files = map[string]File{}
		c.cur[f.Pack] = files
	}
	files[f.Path] = f
}

// readCommitted lists the packs at the hub's commit rev with one git ls-tree.
func readCommitted(ctx context.Context, g *gitx.Git, rev string) (Current, []Problem, error) {
	entries, err := g.LsTree(ctx, rev, PacksDir)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s at %s: %w", PacksDir, rev, err)
	}
	c := newCollector()
	for _, e := range entries {
		c.treeEntry(e)
	}
	cur, problems := c.result()
	return cur, problems, nil
}

// treeEntry sorts one ls-tree entry into a file or a problem.
func (c *collector) treeEntry(e gitx.TreeEntry) {
	var mode, kind, msg string
	switch e.Mode {
	case "100644", "100664": // 100664 is a legacy spelling of 100644
		mode, kind = modeFile, "file"
	case "100755":
		mode, kind = modeExec, "file"
	case "120000":
		kind, msg = "symlink", msgSymlink
	case "160000":
		kind, msg = "submodule", msgSubmodule
	default:
		kind, msg = "entry", fmt.Sprintf("unsupported tree entry mode %s: packs ship regular files only", e.Mode)
	}
	pack, path, ok := c.locate(e.Path, kind)
	if !ok {
		return
	}
	if msg != "" {
		c.problem(pack, path, msg)
		return
	}
	c.add(File{Pack: pack, Path: path, OID: e.OID, Size: e.Size, Mode: mode})
}

// readWorkTree walks <Dir>/packs, hashes every regular file's raw bytes and,
// when Dir is a git work tree root, replaces the ids, sizes and modes with
// what git would commit.
func readWorkTree(ctx context.Context, h *Hub) (Current, []Problem, error) {
	root := filepath.Join(h.Dir, PacksDir)
	c := newCollector()
	info, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cur, problems := c.result()
		return cur, problems, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read packs: %w", err)
	case !info.IsDir():
		c.locate(PacksDir, entryKind(info.Mode()))
		cur, problems := c.result()
		return cur, problems, nil
	}
	w := &walker{ctx: ctx, root: root, c: c}
	if err := filepath.WalkDir(root, w.visit); err != nil {
		return nil, nil, fmt.Errorf("read packs: %w", err)
	}
	if err := throughGit(ctx, h, c.cur); err != nil {
		return nil, nil, fmt.Errorf("read packs: %w", err)
	}
	cur, problems := c.result()
	return cur, problems, nil
}

// throughGit replaces the raw ids and sizes of the work tree files in cur
// with those of the blobs git would commit for them in the hub, and takes
// their modes from the hub's index where git ignores the executable bit.
// It leaves cur as it is when h.Dir is not the root of a git work tree.
func throughGit(ctx context.Context, h *Hub, cur Current) error {
	g := h.git()
	ok, err := isWorkTreeRoot(ctx, g, h.Dir)
	if err != nil || !ok || len(cur) == 0 {
		return err
	}
	type ref struct{ pack, path string }
	var refs []ref
	var paths []string
	for _, pack := range sortedKeys(cur) {
		for _, p := range sortedKeys(cur[pack]) {
			refs = append(refs, ref{pack, p})
			paths = append(paths, hubPath(pack, p))
		}
	}
	ids, sizes, err := cleanBlobs(ctx, g, paths)
	if err != nil {
		return err
	}
	modes, err := indexModes(ctx, g)
	if err != nil {
		return err
	}
	for i, r := range refs {
		f := cur[r.pack][r.path]
		f.OID, f.Size = ids[i], sizes[ids[i]]
		if modes != nil {
			f.Mode = modeFile
			if modes[paths[i]] == modeExec {
				f.Mode = modeExec
			}
		}
		cur[r.pack][r.path] = f
	}
	return nil
}

// isWorkTreeRoot reports whether dir is the root of the git work tree g
// runs in.
func isWorkTreeRoot(ctx context.Context, g *gitx.Git, dir string) (bool, error) {
	top, ok, err := g.TopLevel(ctx)
	if err != nil || !ok {
		return false, err
	}
	topInfo, err := os.Stat(top)
	if err != nil {
		return false, err
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	return os.SameFile(topInfo, dirInfo), nil
}

// cleanBlobs returns the ids git would commit for the work tree files at
// paths, and the size of each id. It writes the blobs into a scratch object
// directory, the only way to learn the size of a filtered blob, and removes
// the directory afterwards.
func cleanBlobs(ctx context.Context, g *gitx.Git, paths []string) (ids []string, sizes map[string]int64, err error) {
	scratch, err := os.MkdirTemp("", "touchmark-objects-")
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if rerr := os.RemoveAll(scratch); err == nil && rerr != nil {
			err = rerr
		}
	}()
	sg := g.WithObjectDir(scratch)
	ids, err = sg.WritePaths(ctx, paths)
	if err != nil {
		return nil, nil, fmt.Errorf("hash pack files through git: %w", err)
	}
	sizes, err = sg.Sizes(ctx, sortedKeys(setOf(ids)))
	if err != nil {
		return nil, nil, fmt.Errorf("size pack files: %w", err)
	}
	return ids, sizes, nil
}

// indexModes returns the mode of every file the index of g's work tree
// records, keyed by path, when git ignores the executable bit on disk there
// (Windows, core.fileMode=false); nil when the executable bit on disk is
// what git reads.
func indexModes(ctx context.Context, g *gitx.Git) (map[string]string, error) {
	fileMode, err := g.ConfigBool(ctx, "core.fileMode", true)
	if err != nil {
		return nil, err
	}
	if fileMode && runtime.GOOS != "windows" {
		return nil, nil
	}
	entries, err := g.IndexEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the hub's index: %w", err)
	}
	modes := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Stage == 0 {
			modes[e.Path] = e.Mode
		}
	}
	return modes, nil
}

func setOf(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// walker holds the state of one walk over the packs directory.
type walker struct {
	ctx  context.Context
	root string
	c    *collector
}

// visit is the filepath.WalkDir callback.
func (w *walker) visit(osPath string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	rel, err := filepath.Rel(w.root, osPath)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	hub := PacksDir + "/" + filepath.ToSlash(rel)
	switch t := d.Type(); {
	case d.IsDir():
		return w.dir(hub)
	case t.IsRegular():
		return w.file(osPath, hub, d)
	default:
		w.nonRegular(hub, t)
		return nil
	}
}

// dir checks a directory: a pack name directly under packs/, a valid
// repository path below. An invalid directory is reported once and skipped.
func (w *walker) dir(hub string) error {
	rest := strings.TrimPrefix(hub, PacksDir+"/")
	pack, path, nested := strings.Cut(rest, "/")
	if !nested {
		if !w.c.checkPack(pack) {
			return fs.SkipDir
		}
		return nil
	}
	if err := pathx.Validate(path); err != nil {
		w.c.problem(pack, path, err.Error())
		return fs.SkipDir
	}
	return nil
}

// file hashes a regular file and records it.
func (w *walker) file(osPath, hub string, d fs.DirEntry) error {
	pack, path, ok := w.c.locate(hub, "file")
	if !ok {
		return nil
	}
	f, err := hashFile(osPath, d)
	if err != nil {
		return err
	}
	f.Pack, f.Path = pack, path
	w.c.add(f)
	return nil
}

// nonRegular reports a symlink, device, pipe, socket or other special entry.
func (w *walker) nonRegular(hub string, t fs.FileMode) {
	kind := entryKind(t)
	pack, path, ok := w.c.locate(hub, kind)
	if !ok {
		return
	}
	msg := msgSpecial
	if kind == "symlink" {
		msg = msgSymlink
	}
	w.c.problem(pack, path, msg)
}

// entryKind names a non-directory entry type for messages.
func entryKind(t fs.FileMode) string {
	switch {
	case t&fs.ModeSymlink != 0:
		return "symlink"
	case t.IsRegular():
		return "file"
	default:
		return "special file"
	}
}

// hashFile computes the raw blob id, size and mode of a regular file. It
// fails if the entry the walk saw was replaced before it could be opened.
//
// The mode is 100755 when any executable bit is set. Windows records no
// executable bits, so every file reads as 100644 there.
func hashFile(osPath string, d fs.DirEntry) (File, error) {
	seen, err := d.Info()
	if err != nil {
		return File{}, err
	}
	fh, err := os.Open(osPath)
	if err != nil {
		return File{}, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(seen, info) {
		return File{}, fmt.Errorf("%s: changed while packs were read", osPath)
	}
	oid, err := gitx.RawOIDReader(fh, info.Size())
	if err != nil {
		return File{}, fmt.Errorf("%s: %w", osPath, err)
	}
	mode := modeFile
	if info.Mode().Perm()&0o111 != 0 {
		mode = modeExec
	}
	return File{OID: oid, Size: info.Size(), Mode: mode}, nil
}
