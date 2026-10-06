package apply

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

const (
	modeFile = "100644"
	modeExec = "100755"
)

// errChanged reports that a file was replaced between its Lstat and Open.
var errChanged = errors.New("replaced while being read")

// osPath converts a repository path into the form os.Root takes.
func osPath(p string) string { return filepath.FromSlash(p) }

// fileKind classifies what Lstat found.
type fileKind int

const (
	kindRegular fileKind = iota
	kindDir
	kindSymlink
	kindOther // device, pipe, socket, Windows junction, …
)

func kindOf(fi fs.FileInfo) fileKind {
	t := fi.Mode().Type()
	switch {
	case t == 0:
		return kindRegular
	case t == fs.ModeDir:
		return kindDir
	case t&fs.ModeSymlink != 0:
		return kindSymlink
	default:
		return kindOther
	}
}

// describe names what fi is, to follow "is" in messages.
func describe(fi fs.FileInfo) string {
	switch kindOf(fi) {
	case kindRegular:
		return "a file"
	case kindDir:
		return "a directory"
	case kindSymlink:
		return "a symlink"
	default:
		return "a special file"
	}
}

// modeOf returns the git mode of a regular file: git only looks at the
// owner's executable bit. The executable bit means nothing on Windows, where
// only the index records it (see applyIndex).
func modeOf(fi fs.FileInfo) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if fi.Mode().Perm()&0o100 != 0 {
		return modeExec
	}
	return modeFile
}

// blocker is an existing ancestor of a path that is not a real directory of
// the target: a file, a symlink, or a directory of another repository.
type blocker struct {
	path string
	info fs.FileInfo
	// nested is set for a directory that holds a .git entry.
	nested bool
}

func (b *blocker) isFile() bool { return !b.nested && kindOf(b.info) == kindRegular }

func (b *blocker) detail() string {
	if b.nested {
		return "parent " + b.path + " is a nested repository or submodule"
	}
	return "parent " + b.path + " is " + describe(b.info)
}

// inspectParents walks the ancestors of the repository path p from the top
// down with Lstat. It stops at the first ancestor that does not exist
// (returned as missing) or is not a real directory of the target (b): not a
// directory, or a directory holding a .git entry, which git treats as
// another repository (a nested one or a checked-out submodule), so files
// written there never reach the target's commits. Symlinks are never
// followed.
func inspectParents(root *os.Root, p string) (missing string, b *blocker, err error) {
	for _, dir := range pathx.Parents(p) {
		fi, exists, err := lstat(root, dir)
		if err != nil {
			return "", nil, err
		}
		if !exists {
			return dir, nil, nil
		}
		if kindOf(fi) != kindDir {
			return "", &blocker{path: dir, info: fi}, nil
		}
		_, nested, err := lstat(root, dir+"/.git")
		if err != nil {
			return "", nil, err
		}
		if nested {
			return "", &blocker{path: dir, info: fi, nested: true}, nil
		}
	}
	return "", nil, nil
}

// lstat is root.Lstat with "does not exist" as a result instead of an error.
func lstat(root *os.Root, p string) (fi fs.FileInfo, exists bool, err error) {
	fi, err = root.Lstat(osPath(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return fi, true, nil
}

// rawOID opens the regular file p that Lstat described as lst and returns
// the blob id of its raw bytes and the opened file's info. It fails with
// errChanged when the opened file is not the one Lstat saw, so a symlink
// swapped in after the Lstat is never read through.
func rawOID(root *os.Root, p string, lst fs.FileInfo) (string, fs.FileInfo, error) {
	f, err := root.Open(osPath(p))
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if kindOf(fi) != kindRegular || !os.SameFile(lst, fi) {
		return "", nil, errChanged
	}
	oid, err := gitx.RawOIDReader(f, fi.Size())
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", p, err)
	}
	return oid, fi, nil
}

// checkWorkTreeRoot fails unless t.Git's work tree root is t.Root:
// `git hash-object --stdin-paths` resolves paths from the work tree root, so
// with Root below it the wrong files would be hashed.
func checkWorkTreeRoot(ctx context.Context, t Target) error {
	top, ok, err := t.Git.TopLevel(ctx)
	if err != nil {
		return fmt.Errorf("find the git work tree of %s: %w", t.Root, err)
	}
	if !ok {
		return fmt.Errorf("%s is not inside a git work tree", t.Root)
	}
	topInfo, err := os.Stat(top)
	if err != nil {
		return fmt.Errorf("git work tree root: %w", err)
	}
	rootInfo, err := os.Stat(t.Root)
	if err != nil {
		return fmt.Errorf("target root: %w", err)
	}
	if !os.SameFile(topInfo, rootInfo) {
		return fmt.Errorf("target root %s is not the git work tree root %s", t.Root, top)
	}
	return nil
}
