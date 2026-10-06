package apply

import (
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// lister reads directories of a target, each once, to learn how the target
// spells a path: on a case-insensitive filesystem Lstat finds "docs/guide.md"
// when the target holds "Docs/Guide.md", and on a case-sensitive one both
// can exist side by side.
type lister struct {
	root *os.Root
	dirs map[string][]fs.DirEntry
}

func newLister(root *os.Root) *lister {
	return &lister{root: root, dirs: map[string][]fs.DirEntry{}}
}

// list returns the entries of the directory dir, a repository path as the
// target spells it ("" for the root), sorted by name.
func (l *lister) list(dir string) ([]fs.DirEntry, error) {
	if entries, ok := l.dirs[dir]; ok {
		return entries, nil
	}
	name := "."
	if dir != "" {
		name = osPath(dir)
	}
	f, err := l.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	l.dirs[dir] = entries
	return entries, nil
}

// spelling returns how the target spells the existing entry at p: for each
// segment, the entry of that exact name, else its only case variant. ok is
// false when there is neither, or several variants: the filesystem equates
// names that differ by more than case (Unicode normalization, 8.3 short
// names), so touchmark cannot tell which entry it found.
func (l *lister) spelling(p string) (actual string, ok bool, err error) {
	for _, seg := range strings.Split(p, "/") {
		entries, err := l.list(actual)
		if err != nil {
			return "", false, err
		}
		name, ok := pickName(entries, seg)
		if !ok {
			return "", false, nil
		}
		actual = joinPath(actual, name)
	}
	return actual, true, nil
}

// pickName returns seg when entries hold it, else its only case variant.
func pickName(entries []fs.DirEntry, seg string) (string, bool) {
	folded := pathx.Fold(seg)
	variant, n := "", 0
	for _, e := range entries {
		switch {
		case e.Name() == seg:
			return seg, true
		case pathx.Fold(e.Name()) == folded:
			variant, n = e.Name(), n+1
		}
	}
	return variant, n == 1
}

// twin returns an existing entry of the target whose path differs from the
// absent path p only by case, "" when there is none. It follows every
// directory whose name is a case variant of the next segment, never a
// symlink.
func (l *lister) twin(p string) (string, error) {
	return l.findVariant("", strings.Split(p, "/"), false)
}

// findVariant looks in dir for the path made of segs, each segment
// compared ignoring case, and returns the first one found that differs from
// the requested spelling somewhere (differs says whether it already does
// above dir).
func (l *lister) findVariant(dir string, segs []string, differs bool) (string, error) {
	entries, err := l.list(dir)
	if err != nil {
		return "", err
	}
	folded := pathx.Fold(segs[0])
	for _, e := range entries {
		if pathx.Fold(e.Name()) != folded {
			continue
		}
		next, d := joinPath(dir, e.Name()), differs || e.Name() != segs[0]
		if len(segs) == 1 {
			if d {
				return next, nil
			}
			continue
		}
		if !e.IsDir() {
			continue
		}
		found, err := l.findVariant(next, segs[1:], d)
		if err != nil || found != "" {
			return found, err
		}
	}
	return "", nil
}

// caseTwinBeside returns an entry next to p (in the directory p would be
// created in) whose name differs from p's only by case, "" when there is
// none. The directory is read afresh.
func caseTwinBeside(root *os.Root, p string) (string, error) {
	dir, base := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	entries, err := newLister(root).list(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() != base && pathx.Fold(e.Name()) == pathx.Fold(base) {
			return joinPath(dir, e.Name()), nil
		}
	}
	return "", nil
}

// joinPath joins a repository directory path ("" for the root) and a name.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
