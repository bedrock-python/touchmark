package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// Sizes returns the size of each blob, via one `git cat-file --batch-check`.
// Unknown or non-blob ids are errors; unknown ids wrap ErrNotFound.
func (g *Git) Sizes(ctx context.Context, oids []string) (map[string]int64, error) {
	sizes := make(map[string]int64, len(oids))
	if len(oids) == 0 {
		return sizes, nil
	}
	var in bytes.Buffer
	for _, id := range oids {
		if !isOID(id) {
			return nil, fmt.Errorf("invalid object id %q", id)
		}
		in.WriteString(id)
		in.WriteByte('\n')
	}
	out, err := g.Run(ctx, &in, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(oids) {
		return nil, fmt.Errorf("git cat-file --batch-check: %d lines for %d objects", len(lines), len(oids))
	}
	for i, line := range lines {
		h, err := parseObjectHeader(line, oids[i])
		if err != nil {
			return nil, fmt.Errorf("git cat-file --batch-check: %w", err)
		}
		if err := h.blobErr(); err != nil {
			return nil, err
		}
		sizes[h.oid] = h.size
	}
	return sizes, nil
}

// objectHeader is a `git cat-file --batch(-check)` response line:
// "<oid> <type> <size>" or "<oid> missing".
type objectHeader struct {
	oid     string
	typ     string
	size    int64
	missing bool
}

// parseObjectHeader parses the response line for the requested id want.
func parseObjectHeader(line, want string) (objectHeader, error) {
	f := strings.Split(line, " ")
	switch {
	case len(f) == 2 && f[0] == want && f[1] == "missing":
		return objectHeader{oid: want, missing: true}, nil
	case len(f) == 3 && f[0] == want:
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || n < 0 {
			return objectHeader{}, fmt.Errorf("bad object size in %q", line)
		}
		return objectHeader{oid: want, typ: f[1], size: n}, nil
	default:
		return objectHeader{}, fmt.Errorf("unexpected response %q for object %s", abbrev(line), want)
	}
}

// blobErr returns the error for a header that is not an existing blob.
func (h objectHeader) blobErr() error {
	switch {
	case h.missing:
		return fmt.Errorf("object %s: %w", h.oid, ErrNotFound)
	case h.typ != "blob":
		return fmt.Errorf("object %s is a %s, not a blob", h.oid, h.typ)
	}
	return nil
}

// Blobs reads blob contents through one long-running `git cat-file --batch`.
// It is not safe for concurrent use. Close it when done.
type Blobs struct {
	ctx    context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	args   []string
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *tailBuffer
	// err is set once the stream is out of sync or the process is gone;
	// every later Read returns it.
	err    error
	closed bool
}

// errBlobsClosed is returned by Read after Close.
var errBlobsClosed = errors.New("git cat-file --batch: already closed")

// OpenBlobs starts `git cat-file --batch`.
func (g *Git) OpenBlobs(ctx context.Context) (*Blobs, error) {
	args := []string{"cat-file", "--batch"}
	ctx, cancel := context.WithCancel(ctx)
	cmd := g.command(ctx, args)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	stderr := newTailBuffer(stderrLimit)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("git cat-file --batch: start: %w", err)
	}
	return &Blobs{
		ctx:    ctx,
		cancel: cancel,
		cmd:    cmd,
		args:   args,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 64<<10),
		stderr: stderr,
	}, nil
}

// Read returns the content of blob oid. A missing object wraps ErrNotFound;
// a missing or non-blob object leaves the reader usable.
func (b *Blobs) Read(oid string) ([]byte, error) {
	if b.closed {
		return nil, errBlobsClosed
	}
	if b.err != nil {
		return nil, b.err
	}
	if !isOID(oid) {
		return nil, fmt.Errorf("invalid object id %q", oid)
	}
	if _, err := io.WriteString(b.stdin, oid+"\n"); err != nil {
		return nil, b.fail(err)
	}
	line, err := b.stdout.ReadString('\n')
	if err != nil {
		return nil, b.fail(err)
	}
	h, err := parseObjectHeader(strings.TrimSuffix(line, "\n"), oid)
	if err != nil {
		return nil, b.fail(err)
	}
	if h.missing {
		return nil, h.blobErr()
	}
	if h.typ != "blob" {
		if err := b.skip(h.size); err != nil {
			return nil, err
		}
		return nil, h.blobErr()
	}
	return b.content(h.size)
}

// content reads size bytes of object content and the newline after them.
func (b *Blobs) content(size int64) ([]byte, error) {
	if size > math.MaxInt {
		return nil, b.fail(fmt.Errorf("object of %d bytes does not fit in memory", size))
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(b.stdout, buf); err != nil {
		return nil, b.fail(err)
	}
	if err := b.endOfObject(); err != nil {
		return nil, err
	}
	return buf, nil
}

// skip discards size bytes of object content and the newline after them.
func (b *Blobs) skip(size int64) error {
	if _, err := io.CopyN(io.Discard, b.stdout, size); err != nil {
		return b.fail(err)
	}
	return b.endOfObject()
}

// endOfObject consumes the newline git prints after each object.
func (b *Blobs) endOfObject() error {
	c, err := b.stdout.ReadByte()
	if err != nil {
		return b.fail(err)
	}
	if c != '\n' {
		return b.fail(fmt.Errorf("missing newline after object content"))
	}
	return nil
}

// fail records err as the reader's sticky error, with git's stderr.
func (b *Blobs) fail(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	if ctxErr := b.ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if s := strings.TrimSpace(b.stderr.String()); s != "" {
		b.err = fmt.Errorf("git cat-file --batch: %w: %s", err, s)
	} else {
		b.err = fmt.Errorf("git cat-file --batch: %w", err)
	}
	return b.err
}

// Close stops the process.
func (b *Blobs) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	defer b.cancel()
	closeErr := b.stdin.Close()
	if b.err != nil {
		// Unread output may be pending and would block git: kill it. The
		// failure was already reported by Read.
		b.cancel()
		_ = b.cmd.Wait()
		return nil
	}
	if err := b.cmd.Wait(); err != nil {
		return commandError(b.ctx, b.args, err, b.stderr)
	}
	if closeErr != nil {
		return fmt.Errorf("git cat-file --batch: close stdin: %w", closeErr)
	}
	return nil
}
