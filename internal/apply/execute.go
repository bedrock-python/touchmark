package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// skip is an error meaning that the target is not in the state the plan was
// based on: the entry is reported as Skipped, not as failed.
type skip string

func (s skip) Error() string { return string(s) }

// changed is the skip for a path that changed since it was observed.
func changed(what string) error { return skip("changed since observed: " + what) }

// obstacle is the skip for something in the way of a write: expected to be
// gone for an AfterDeletes create, a change since Observe otherwise.
func obstacle(e decide.Entry, what string) error {
	if e.AfterDeletes {
		return skip("still in the way: " + what)
	}
	return changed(what)
}

// executor carries out plan entries in one target.
type executor struct {
	ctx   context.Context
	t     Target
	root  *os.Root
	blobs BlobSource
	// rootChecked is set once checkWorkTreeRoot ran; rootErr is its result.
	rootChecked bool
	rootErr     error
	// idx is the target's index, read on first need (see recordMode).
	idx *index
}

// run carries out one entry and records the outcome in r.
func (x *executor) run(r *Result) {
	err := x.do(r.Entry)
	var s skip
	switch {
	case err == nil:
		r.Done = true
	case errors.As(err, &s):
		r.Skipped = string(s)
	default:
		r.Err = fmt.Errorf("%s %s: %w", r.Entry.Action, r.Entry.Path, err)
	}
}

func (x *executor) do(e decide.Entry) error {
	if err := validateEntry(e); err != nil {
		return err
	}
	switch e.Action {
	case decide.Delete:
		return x.delete(e)
	case decide.Create, decide.Update, decide.Adopt:
		return x.write(e)
	case decide.Chmod:
		return x.chmod(e)
	default:
		return fmt.Errorf("unknown action %q", e.Action)
	}
}

// validateEntry rejects entries decide.Decide never produces.
func validateEntry(e decide.Entry) error {
	if err := pathx.Validate(e.Path); err != nil {
		return err
	}
	switch e.Action {
	case decide.Delete, decide.Update:
		if e.From == "" {
			return errors.New("plan entry has no From id")
		}
	}
	switch e.Action {
	case decide.Create, decide.Update, decide.Adopt:
		if e.To == "" {
			return errors.New("plan entry has no To id")
		}
	}
	return nil
}

// delete removes a file that still holds e.From, then the parents that
// became empty.
func (x *executor) delete(e decide.Entry) error {
	fi, err := x.existingFile(e.Path)
	if err != nil {
		return err
	}
	if err := x.expectContent(e.Path, fi, e.From); err != nil {
		return err
	}
	if err := x.root.Remove(osPath(e.Path)); err != nil {
		return err
	}
	x.removeEmptyParents(e.Path, "")
	return nil
}

// removeEmptyParents removes the ancestors of p that are empty directories,
// deepest first, up to and including top ("" for up to the root). It stops
// at the first one it cannot remove: not empty, not a directory any more, or
// an error.
func (x *executor) removeEmptyParents(p, top string) {
	parents := pathx.Parents(p)
	for i := len(parents) - 1; i >= 0; i-- {
		fi, exists, err := lstat(x.root, parents[i])
		if err != nil || !exists || kindOf(fi) != kindDir {
			return
		}
		if x.root.Remove(osPath(parents[i])) != nil || parents[i] == top {
			return
		}
	}
}

// write creates or replaces e.Path with blob e.To, atomically. When it
// fails, the directories it created for the file are removed again.
func (x *executor) write(e decide.Entry) (err error) {
	// A cheap check first, so that a skipped entry creates no directory.
	missing, err := x.checkWrite(e, false)
	if err != nil {
		return err
	}
	if missing != "" {
		defer func() {
			if err != nil {
				x.removeEmptyParents(e.Path, missing)
			}
		}()
		if err := x.root.MkdirAll(osPath(path.Dir(e.Path)), 0o755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		if _, err := x.writableParents(e); err != nil {
			return err
		}
	}
	tmp, err := x.writeTemp(path.Dir(e.Path), e)
	if err != nil {
		return err
	}
	// The full check runs right before the rename, so that the window in
	// which a change goes unnoticed does not include writing the content.
	if _, err := x.checkWrite(e, true); err != nil {
		x.discard(tmp)
		return err
	}
	if err := x.place(e, tmp); err != nil {
		x.discard(tmp)
		return err
	}
	return x.recordMode(e.Path, e.Mode)
}

// place moves the temp file tmp to e.Path. A create links it there, which
// fails if something appeared at the path since the last check, where a
// rename would silently replace it; a filesystem without hard links falls
// back to the rename. An update or adopt renames over the file, clearing a
// Windows read-only attribute first if the rename is refused for it.
func (x *executor) place(e decide.Entry, tmp string) error {
	if e.Action == decide.Create {
		err := x.root.Link(osPath(tmp), osPath(e.Path))
		switch {
		case err == nil:
			x.discard(tmp)
			return nil
		case errors.Is(err, fs.ErrExist):
			return obstacle(e, "something appeared at the path")
		}
		// No hard links here: rename, as for an update.
	}
	err := x.root.Rename(osPath(tmp), osPath(e.Path))
	if err != nil && runtime.GOOS == "windows" && e.Action != decide.Create && x.clearReadOnly(e.Path) {
		err = x.root.Rename(osPath(tmp), osPath(e.Path))
	}
	if err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// clearReadOnly makes a read-only file at p writable, so that it can be
// replaced on Windows; it reports whether it changed anything. The content
// was checked right before, and is replaced right after.
func (x *executor) clearReadOnly(p string) bool {
	fi, exists, err := lstat(x.root, p)
	if err != nil || !exists || kindOf(fi) != kindRegular || fi.Mode().Perm()&0o200 != 0 {
		return false
	}
	return x.root.Chmod(osPath(p), fi.Mode().Perm()|0o200) == nil
}

// recordMode records mode 100755 for p in the index where git ignores the
// executable bit on disk (Windows, core.fileMode=false), unless the index
// already has it: there the index is the only place the bit exists, and a
// new file would otherwise be added as 100644. Recording it adds p to the
// index (git update-index --add --chmod=+x).
func (x *executor) recordMode(p, mode string) error {
	if mode != modeExec || x.t.Git == nil {
		return nil
	}
	if x.idx == nil {
		if err := x.checkRoot(); err != nil {
			return err
		}
		idx, err := readIndex(x.ctx, x.t.Git)
		if err != nil {
			return err
		}
		x.idx = idx
	}
	if !x.idx.trustModes || x.idx.modes[p] == modeExec {
		return nil
	}
	if err := x.t.Git.SetExecutable(x.ctx, p); err != nil {
		return fmt.Errorf("record the executable bit in the index: %w", err)
	}
	x.idx.modes[p] = modeExec
	return nil
}

// checkWrite re-checks the state a create, update or adopt was planned on
// and returns the first missing ancestor of e.Path ("" if none). The
// content of an existing file, which may take a git run to hash, is checked
// only when withContent is set.
func (x *executor) checkWrite(e decide.Entry, withContent bool) (missing string, err error) {
	missing, err = x.writableParents(e)
	if err != nil {
		return "", err
	}
	fi, exists, err := lstat(x.root, e.Path)
	if err != nil {
		return "", err
	}
	if e.Action == decide.Create {
		if err := checkAbsent(e, fi, exists); err != nil {
			return "", err
		}
		return missing, x.checkNoTwin(e)
	}
	switch {
	case !exists:
		return "", changed("the file is gone")
	case kindOf(fi) != kindRegular:
		return "", changed("now " + describe(fi))
	case e.From == "" || !withContent:
		// Adopt has no From (see Execute); the content check comes last.
		return "", nil
	}
	return "", x.expectContent(e.Path, fi, e.From)
}

// writableParents requires every existing ancestor of e.Path to be a real
// directory and returns the first missing one ("" if none).
func (x *executor) writableParents(e decide.Entry) (missing string, err error) {
	missing, b, err := inspectParents(x.root, e.Path)
	if err != nil {
		return "", err
	}
	if b != nil {
		return "", obstacle(e, b.detail())
	}
	return missing, nil
}

// checkNoTwin refuses a create next to an entry whose name differs only by
// case: on a case-sensitive filesystem both would exist, and git would track
// two paths that collide on every case-insensitive checkout.
func (x *executor) checkNoTwin(e decide.Entry) error {
	if len(pathx.Parents(e.Path)) > 0 {
		if _, exists, err := lstat(x.root, path.Dir(e.Path)); err != nil || !exists {
			return err // the directory is created with the file
		}
	}
	twin, err := caseTwinBeside(x.root, e.Path)
	if err != nil {
		return err
	}
	if twin != "" {
		return obstacle(e, twin+" differs only by case")
	}
	return nil
}

// checkAbsent requires the path of a create to be free.
func checkAbsent(e decide.Entry, fi fs.FileInfo, exists bool) error {
	switch {
	case !exists:
		return nil
	case e.AfterDeletes && kindOf(fi) == kindDir:
		return skip("directory not empty")
	default:
		return obstacle(e, "the path is "+describe(fi))
	}
}

// writeTemp writes blob e.To into a new temp file in dir (a repository path,
// "." for the root), verifies it, sets its mode and returns its repository
// path. The temp file is removed on failure.
func (x *executor) writeTemp(dir string, e decide.Entry) (string, error) {
	name := path.Join(dir, tempName())
	f, err := x.root.OpenFile(osPath(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	err = fill(f, x.blobs, e)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("close temp file: %w", cerr)
	}
	if err != nil {
		x.discard(name)
		return "", err
	}
	return name, nil
}

// discard removes a temp file; there is nothing to do if that fails.
func (x *executor) discard(name string) { _ = x.root.Remove(osPath(name)) }

// tempName returns a fresh temp file name. The prefix keeps it recognizable
// if a crash leaves it behind.
func tempName() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return ".touchmark-" + hex.EncodeToString(b[:]) + ".tmp"
}

// fill writes blob e.To into f, verifies it and sets the mode for e.Mode.
// Windows only knows a read-only bit, so a chmod failure there is ignored.
func fill(f *os.File, blobs BlobSource, e decide.Entry) error {
	if err := copyBlob(f, blobs, e.To); err != nil {
		return err
	}
	if err := f.Chmod(filePerm(e.Mode)); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	return nil
}

// copyBlob writes blob oid into f and checks that what f holds hashes to
// oid, by reading it back.
func copyBlob(f *os.File, blobs BlobSource, oid string) error {
	if blobs == nil {
		return errors.New("no blob source")
	}
	rc, err := blobs.Open(oid)
	if err != nil {
		return fmt.Errorf("open blob %s: %w", oid, err)
	}
	n, err := io.Copy(f, rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy blob %s: %w", oid, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("verify blob %s: %w", oid, err)
	}
	got, err := gitx.RawOIDReader(f, n)
	if err != nil {
		return fmt.Errorf("verify blob %s: %w", oid, err)
	}
	if got != oid {
		return fmt.Errorf("blob %s: the written content hashes to %s", oid, got)
	}
	return nil
}

// filePerm is the permission of a written file with git mode mode.
func filePerm(mode string) fs.FileMode {
	if mode == modeExec {
		return 0o755
	}
	return 0o644
}

// chmod sets or clears the executable bits of a file that is still regular
// (and still holds e.From, when set).
func (x *executor) chmod(e decide.Entry) error {
	fi, err := x.existingFile(e.Path)
	if err != nil {
		return err
	}
	if e.From != "" {
		if err := x.expectContent(e.Path, fi, e.From); err != nil {
			return err
		}
	}
	err = x.root.Chmod(osPath(e.Path), chmodPerm(fi.Mode().Perm(), e.Mode))
	if err != nil && runtime.GOOS != "windows" {
		return err
	}
	// Windows has no executable bit to set; the index keeps it there.
	return x.recordMode(e.Path, e.Mode)
}

// chmodPerm is perm with the executable bit set for mode "100755" like
// `chmod +x` (owner, plus group and others where they may read), and cleared
// otherwise.
func chmodPerm(perm fs.FileMode, mode string) fs.FileMode {
	if mode == modeExec {
		return perm | 0o100 | (perm&0o044)>>2
	}
	return perm &^ 0o111
}

// existingFile re-checks that p is still a regular file below real
// directories and returns its Lstat info.
func (x *executor) existingFile(p string) (fs.FileInfo, error) {
	missing, b, err := inspectParents(x.root, p)
	switch {
	case err != nil:
		return nil, err
	case missing != "":
		return nil, changed("the file is gone")
	case b != nil:
		return nil, changed(b.detail())
	}
	fi, exists, err := lstat(x.root, p)
	switch {
	case err != nil:
		return nil, err
	case !exists:
		return nil, changed("the file is gone")
	case kindOf(fi) != kindRegular:
		return nil, changed("now " + describe(fi))
	}
	return fi, nil
}

// expectContent requires the file p, which Lstat described as lst, to hash
// to oid: by its raw bytes or, when the target is a git work tree, after its
// clean filters.
func (x *executor) expectContent(p string, lst fs.FileInfo, oid string) error {
	raw, _, err := rawOID(x.root, p, lst)
	if errors.Is(err, errChanged) {
		return changed(err.Error())
	}
	if err != nil {
		return err
	}
	if raw == oid {
		return nil
	}
	filtered, err := x.filteredOID(p)
	if err != nil {
		return err
	}
	if filtered == oid {
		return nil
	}
	return changed("the content differs")
}

// checkRoot runs checkWorkTreeRoot once and returns its result.
func (x *executor) checkRoot() error {
	if !x.rootChecked {
		x.rootErr = checkWorkTreeRoot(x.ctx, x.t)
		x.rootChecked = true
	}
	return x.rootErr
}

// filteredOID returns the id git would store for p, or "" without git.
func (x *executor) filteredOID(p string) (string, error) {
	if x.t.Git == nil {
		return "", nil
	}
	if err := x.checkRoot(); err != nil {
		return "", err
	}
	ids, err := x.t.Git.HashPaths(x.ctx, []string{p})
	if err != nil {
		return "", fmt.Errorf("hash through git: %w", err)
	}
	return ids[0], nil
}
