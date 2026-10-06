package gitx

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// HashPaths returns, for each path relative to the work tree root Dir, the
// blob id git computes for the file as it is on disk: clean filters,
// core.autocrlf, eol, ident and working-tree-encoding applied. It runs one
//
//	git -c core.safecrlf=false -c core.longpaths=true hash-object --stdin-paths
//
// in Dir and never writes objects. Paths are passed with '/' separators.
// Paths that start with '"' or contain control characters are C-quoted, as
// --stdin-paths expects. An empty list returns no ids without running git.
//
// This is what `git add` stores for a new file. For a file the index
// already holds with CRLF, `git add` keeps CRLF when the attributes would
// convert it (git's safer-autocrlf rule), which hash-object does not model:
// the id here is the converted one.
func (g *Git) HashPaths(ctx context.Context, paths []string) ([]string, error) {
	return g.hashPaths(ctx, paths, false)
}

// WritePaths is HashPaths that also writes the blobs into the object store
// (`hash-object -w`). Point GIT_OBJECT_DIRECTORY at a scratch directory
// through Env to leave the repository's own store untouched.
func (g *Git) WritePaths(ctx context.Context, paths []string) ([]string, error) {
	return g.hashPaths(ctx, paths, true)
}

// HashPathsEach is HashPaths that survives files git cannot hash (a path
// too long for the platform, a clean filter that fails, a file removed
// meanwhile): for such a path ids[i] is "" and errs[i] holds git's error.
// err is reserved for failures that concern no single path, such as a
// missing git or a cancelled ctx. A failing path costs at most two extra
// git runs; the others are hashed in batches.
func (g *Git) HashPathsEach(ctx context.Context, paths []string) (ids []string, errs []error, err error) {
	ids = make([]string, len(paths))
	errs = make([]error, len(paths))
	for start := 0; start < len(paths); {
		got, err := g.hashBatch(ctx, paths[start:], false)
		copy(ids[start:], got)
		if err == nil {
			return ids, errs, nil
		}
		var gerr *Error
		if !errors.As(err, &gerr) {
			return nil, nil, err
		}
		// git stops at the first path it cannot hash; the ids printed before
		// it are complete. Hash that path alone to tell whether it is the one
		// at fault, then go on after it.
		k := start + len(got)
		if k >= len(paths) {
			return nil, nil, err
		}
		one, err := g.hashBatch(ctx, paths[k:k+1], false)
		switch {
		case err == nil:
			ids[k] = one[0]
		case errors.As(err, &gerr):
			errs[k] = err
		default:
			return nil, nil, err
		}
		start = k + 1
	}
	return ids, errs, nil
}

// hashPaths runs one hash-object over paths and requires an id for each.
func (g *Git) hashPaths(ctx context.Context, paths []string, write bool) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	ids, err := g.hashBatch(ctx, paths, write)
	if err != nil {
		return nil, err
	}
	if len(ids) != len(paths) {
		return nil, fmt.Errorf("git hash-object --stdin-paths: %d ids for %d paths", len(ids), len(paths))
	}
	return ids, nil
}

// hashBatch runs `hash-object --stdin-paths` over paths. On failure it
// returns the ids git printed before failing along with the error.
func (g *Git) hashBatch(ctx context.Context, paths []string, write bool) ([]string, error) {
	var in bytes.Buffer
	for _, p := range paths {
		if err := checkStdinPath(p); err != nil {
			return nil, err
		}
		in.WriteString(quoteStdinPath(p))
		in.WriteByte('\n')
	}
	args := []string{"-c", "core.safecrlf=false", "-c", "core.longpaths=true", "hash-object"}
	if write {
		args = append(args, "-w")
	}
	out, runErr := g.runPartial(ctx, &in, append(args, "--stdin-paths")...)
	var ids []string
	for _, id := range strings.Split(string(out), "\n") {
		if id == "" {
			continue
		}
		if !isOID(id) {
			return nil, fmt.Errorf("git hash-object --stdin-paths: unexpected output %q", abbrev(id))
		}
		ids = append(ids, id)
	}
	if len(ids) > len(paths) {
		return nil, fmt.Errorf("git hash-object --stdin-paths: %d ids for %d paths", len(ids), len(paths))
	}
	return ids, runErr
}

// checkStdinPath rejects paths hash-object cannot take relative to Dir.
func checkStdinPath(p string) error {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return fmt.Errorf("invalid work tree path %q", p)
	}
	return nil
}

// quoteStdinPath C-quotes p when --stdin-paths would otherwise misread it:
// a leading '"' starts a quoted path, and line breaks end one.
func quoteStdinPath(p string) string {
	needs := strings.HasPrefix(p, `"`)
	for i := 0; i < len(p) && !needs; i++ {
		needs = p[i] < 0x20 || p[i] == 0x7f
	}
	if !needs {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RawOID computes the sha1 blob id of content without running git:
// sha1("blob <len>\x00" + content).
func RawOID(content []byte) string {
	h := blobHash(int64(len(content)))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// RawOIDReader is RawOID over a stream of known size. It fails unless r
// yields exactly size bytes.
func RawOIDReader(r io.Reader, size int64) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("hash blob: negative size %d", size)
	}
	h := blobHash(size)
	n, err := io.CopyN(h, r, size)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("hash blob: read %d of %d bytes: %w", n, size, io.ErrUnexpectedEOF)
		}
		return "", fmt.Errorf("hash blob: %w", err)
	}
	var extra [1]byte
	m, err := io.ReadFull(r, extra[:])
	if m > 0 {
		return "", fmt.Errorf("hash blob: content is longer than %d bytes", size)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("hash blob: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// blobHash returns a sha1 hash with the blob header for size written.
func blobHash(size int64) hash.Hash {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	return h
}
