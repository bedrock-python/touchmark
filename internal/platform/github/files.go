package github

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Tree entry modes, as the trees API writes them.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeTree       = "040000"
	modeSymlink    = "120000"
	modeSubmodule  = "160000"
)

// apiTree is GET /repos/{owner}/{repo}/git/trees/{tree}, not recursive.
type apiTree struct {
	SHA       string         `json:"sha"`
	Entries   []apiTreeEntry `json:"tree"`
	Truncated bool           `json:"truncated"`
}

// apiTreeEntry is one entry of a tree: path is its name.
type apiTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

// apiBlob is GET /repos/{owner}/{repo}/git/blobs/{sha}.
type apiBlob struct {
	SHA      string `json:"sha"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// ReadFile returns a regular file of r at ref ("" is the default branch
// head, HEAD on GitHub; a commit id or a branch otherwise), at most max
// bytes.
//
// It walks the tree of ref through the git trees API, one directory per
// request and never through a symlink, then reads the blob by id: the
// entry gives the mode (the contents API says "file" for an executable,
// and follows a symlink to its target), the blob API the content, which is
// checked against its id. Symlinks, submodules and directories are
// ErrNotRegular, a file over max bytes ErrTooLarge (from the entry's size,
// before the blob is read).
//
// Only a tree that was read and lacks the path says the file is missing
// (ErrNotFound): a 404 of the first tree is the repository or ref that is
// missing (ErrNotFound too, as the path is not there), a repository
// without commits (409) is ErrNotFound, but a 404 of a subtree or of the
// blob, whose ids the trees named, contradicts them: ClassUnknown. The core
// takes a missing opt-in file for an opt-out, so a
// repository that goes away in the middle must not look like one.
func (d *reader) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	return d.c.readFile(ctx, r, ref, path, max)
}

func (c *client) readFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	const op = "read file"
	if err := c.checkHost(op, r); err != nil {
		return platform.File{}, err
	}
	owner, name, err := repoPath(op, r)
	if err != nil {
		return platform.File{}, err
	}
	if err := checkTreePath(path); err != nil {
		return platform.File{}, invalid(op, "%v", err)
	}
	if max < 0 {
		return platform.File{}, invalid(op, "negative size limit %d", max)
	}
	a, err := c.ownerAuth(ctx, owner)
	if err != nil {
		return platform.File{}, err
	}
	treeish := ref
	if treeish == "" {
		treeish = "HEAD"
	}
	segments := strings.Split(path, "/")
	var entry *apiTreeEntry
	for i, seg := range segments {
		t, err := c.tree(ctx, op, a, owner, name, treeish, i == 0)
		if err != nil {
			return platform.File{}, err
		}
		e := t.find(seg)
		switch {
		case e == nil && t.Truncated:
			return platform.File{}, &platform.Error{Op: op, Class: platform.ClassUnknown,
				Err: fmt.Errorf("%s: the tree of %s is too large for the trees API", r.Path, strings.Join(segments[:i], "/"))}
		case e == nil:
			return platform.File{}, notFound(op, "%s: %s at %s", r.Path, path, treeish)
		case i == len(segments)-1:
			entry = e
			continue
		}
		switch normalMode(e.Mode) {
		case modeTree:
			treeish = e.SHA
		case modeSymlink:
			return platform.File{}, fmt.Errorf("%s: %s: %s leads through a symlink at %s: %w", op, r.Path, path, strings.Join(segments[:i+1], "/"), platform.ErrNotRegular)
		default:
			// A file or a submodule on the way: git has no such path.
			return platform.File{}, notFound(op, "%s: %s at %s: %s is no directory", r.Path, path, treeish, strings.Join(segments[:i+1], "/"))
		}
	}
	mode := normalMode(entry.Mode)
	switch {
	case mode != modeFile && mode != modeExecutable:
		return platform.File{}, fmt.Errorf("%s: %s: %s has mode %s: %w", op, r.Path, path, mode, platform.ErrNotRegular)
	case !isHexOID(entry.SHA):
		return platform.File{}, shapeError(op, "%s: %s: a tree entry without a blob id", r.Path, path)
	case entry.Size != nil && *entry.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, path, *entry.Size, max, platform.ErrTooLarge)
	}
	content, err := c.blob(ctx, op, a, owner, name, entry.SHA, max)
	if err != nil {
		return platform.File{}, fmt.Errorf("%s: %s: %w", r.Path, path, err)
	}
	return platform.File{Path: path, Mode: mode, OID: strings.ToLower(entry.SHA), Content: content}, nil
}

// tree reads one tree, not recursively. For the first tree of a walk (a
// ref), a 404 is the repository or the ref missing and a 409 a repository
// without commits: ErrNotFound; for a subtree, named by the tree above, a
// 404 contradicts that tree: ClassUnknown.
func (c *client) tree(ctx context.Context, op string, a *httpx.Auth, owner, name, treeish string, first bool) (*apiTree, error) {
	var t apiTree
	_, err := c.get(ctx, op, c.repoURL(owner, name, "git", "trees", treeish), nil, a, &t)
	switch {
	case err == nil:
		return &t, nil
	case first && platform.ClassOf(err) == platform.ClassNotFound:
		return nil, err
	case first && statusOf(err) == http.StatusConflict:
		// "Git Repository is empty."
		return nil, notFound(op, "%s/%s has no commits", owner, name)
	case platform.ClassOf(err) == platform.ClassNotFound:
		return nil, shapeError(op, "%s/%s: the tree %s that a tree named is not found", owner, name, treeish)
	}
	return nil, err
}

// find returns the entry named seg, nil when the tree lacks it.
func (t *apiTree) find(seg string) *apiTreeEntry {
	for i := range t.Entries {
		if t.Entries[i].Path == seg {
			return &t.Entries[i]
		}
	}
	return nil
}

// blob reads blob sha of at most max bytes and checks its content against
// its id. A 404 contradicts the tree that named it: ClassUnknown.
func (c *client) blob(ctx context.Context, op string, a *httpx.Auth, owner, name, sha string, max int64) ([]byte, error) {
	var b apiBlob
	_, err := c.get(ctx, op, c.repoURL(owner, name, "git", "blobs", sha), nil, a, &b)
	switch {
	case platform.ClassOf(err) == platform.ClassNotFound:
		return nil, shapeError(op, "the blob %s that the tree names is not found", sha)
	case err != nil:
		return nil, err
	case b.Size > max:
		return nil, fmt.Errorf("%s: blob %s has %d bytes, more than %d: %w", op, sha, b.Size, max, platform.ErrTooLarge)
	case b.Encoding != "base64":
		return nil, shapeError(op, "blob %s: encoding %q", sha, b.Encoding)
	}
	content, derr := base64.StdEncoding.DecodeString(stripSpace(b.Content))
	switch {
	case derr != nil:
		return nil, shapeError(op, "blob %s: the content is not base64", sha)
	case int64(len(content)) != b.Size:
		return nil, shapeError(op, "blob %s: %d bytes of content, the API says %d", sha, len(content), b.Size)
	case int64(len(content)) > max:
		return nil, fmt.Errorf("%s: blob %s has %d bytes, more than %d: %w", op, sha, len(content), max, platform.ErrTooLarge)
	case blobID(content, len(sha)) != strings.ToLower(sha):
		return nil, shapeError(op, "blob %s: the content does not hash to its id", sha)
	}
	return content, nil
}

// normalMode returns a tree entry mode in git's six-digit form.
func normalMode(m string) string {
	if len(m) < 6 {
		return strings.Repeat("0", 6-len(m)) + m
	}
	return m
}

// gqlMode turns the integer mode of GraphQL's TreeEntry into git's octal
// form: 33188 is 100644.
func gqlMode(m int) string { return normalMode(fmt.Sprintf("%o", m)) }

// checkTreePath accepts a repository-relative path: no leading slash, no
// empty, "." or ".." segment, no NUL.
func checkTreePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("path %q contains NUL", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q has an empty, \".\" or \"..\" segment", p)
		}
	}
	return nil
}

// blobID is git's id of a blob with content in the object format whose ids
// have hexLen digits: SHA-1 for 40, SHA-256 for 64.
func blobID(content []byte, hexLen int) string {
	var h hash.Hash
	if hexLen == sha256.Size*2 {
		h = sha256.New()
	} else {
		h = sha1.New()
	}
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// isHexOID reports whether s is a full SHA-1 or SHA-256 object id.
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
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

// stripSpace removes the line breaks GitHub puts into base64.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, s)
}
