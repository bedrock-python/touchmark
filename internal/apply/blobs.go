package apply

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

// GitBlobs is a BlobSource over a hub repository's object store. Like
// gitx.Blobs it is not safe for concurrent use; Execute uses it from one
// goroutine. The caller opens and closes B.
type GitBlobs struct{ B *gitx.Blobs }

// Open returns the content of blob oid. gitx.Blobs reads a blob whole, so
// the content is held in memory. A missing blob wraps gitx.ErrNotFound.
func (g GitBlobs) Open(oid string) (io.ReadCloser, error) {
	if g.B == nil {
		return nil, errors.New("git blob reader is not open")
	}
	data, err := g.B.Read(oid)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// DirBlobs is a BlobSource over files on disk whose ids are those of their
// raw bytes, such as packs read outside git. (--worktree on a git hub reads
// blobs through the hub's clean filters instead: GitBlobs over a scratch
// object store.) Execute verifies every blob against its id, so a file
// edited in the meantime fails the write instead of shipping other content.
type DirBlobs struct {
	// Dir is the base of relative Paths.
	Dir string
	// Paths maps a blob id to a file with that content: an OS path, relative
	// to Dir or absolute.
	Paths map[string]string
}

// Open opens the file for oid. An unknown id wraps gitx.ErrNotFound.
func (d DirBlobs) Open(oid string) (io.ReadCloser, error) {
	p, ok := d.Paths[oid]
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", oid, gitx.ErrNotFound)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(d.Dir, p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", oid, err)
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", p)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("blob %s: %w", oid, err)
	}
	return f, nil
}
