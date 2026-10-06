package gitea

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"net/url"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxTreePages bounds the listing of one directory: 100 000 entries at the
// default page size.
const maxTreePages = 100

// Tree entry modes.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeTree       = "040000"
	modeSymlink    = "120000"
)

// apiContent is a file, symlink or submodule of the contents API. A
// directory is a JSON array of entries instead.
type apiContent struct {
	Type          string  `json:"type"` // file, dir, symlink, submodule
	Size          int64   `json:"size"`
	SHA           string  `json:"sha"`
	LastCommitSHA string  `json:"last_commit_sha"`
	Encoding      *string `json:"encoding"`
	Content       *string `json:"content"`
}

// apiTree is one page of GET /repos/{owner}/{repo}/git/trees/{sha}.
type apiTree struct {
	Entries   []apiTreeEntry `json:"tree"`
	Truncated bool           `json:"truncated"`
}

// apiTreeEntry is an entry of a tree listing: path is its name in a
// listing that is not recursive.
type apiTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// ReadFile returns a regular file of r at ref ("" is the default branch):
// its content and blob id from the contents API, its mode from the tree of
// the last commit that changed it (the contents API says "file" for an
// executable too). Symlinks, submodules and directories are ErrNotRegular,
// a missing path (or one through a symlink) ErrNotFound, a file over max
// bytes ErrTooLarge. The content is checked against the blob id.
//
// Only the contents API says a file is missing. The core takes a missing
// opt-in file for an opt-out and closes the target's pull requests, so
// once the contents API found the file, a failure to
// read the trees keeps its class (a 404 of the trees API becomes
// ClassUnknown), and a tree of the file's last commit, which cannot change,
// that lacks the file or holds another blob contradicts the contents API:
// ClassUnknown too. Only when the server names no such commit, and the tree
// is read at the ref instead, does a tree without the file mean that it
// went away in between (ErrNotFound), and another blob that it changed
// (ClassConflict).
func (d *reader) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	const op = "read file"
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
	segments := append([]string{"repos", owner, name, "contents"}, strings.Split(path, "/")...)
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	var raw json.RawMessage
	if _, err := d.c.get(ctx, op, d.c.endpoint(segments...), q, &raw); err != nil {
		return platform.File{}, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return platform.File{}, shapeError(op, "a directory listing that does not decode")
		}
		// Forgejo lists nothing for a path of an empty repository.
		if len(entries) == 0 {
			return platform.File{}, notFound(op, "%s: %s", r.Path, path)
		}
		return platform.File{}, fmt.Errorf("%s: %s: %s is a directory: %w", op, r.Path, path, platform.ErrNotRegular)
	}
	var f apiContent
	if err := json.Unmarshal(raw, &f); err != nil {
		return platform.File{}, shapeError(op, "contents of %s do not decode", path)
	}
	switch {
	case f.Type != "file":
		return platform.File{}, fmt.Errorf("%s: %s: %s is a %s: %w", op, r.Path, path, cmp.Or(f.Type, "thing without a type"), platform.ErrNotRegular)
	case f.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, path, f.Size, max, platform.ErrTooLarge)
	case !isHexOID(f.SHA):
		return platform.File{}, shapeError(op, "%s: no blob id", path)
	case f.Content == nil || f.Encoding == nil || *f.Encoding != "base64":
		// The server leaves out files over [api] DEFAULT_MAX_BLOB_SIZE.
		return platform.File{}, &platform.Error{Op: op, Class: platform.ClassUnsupported,
			Err: fmt.Errorf("%s: %s: the API did not return the content of %d bytes", r.Path, path, f.Size)}
	}
	content, err := base64.StdEncoding.DecodeString(stripSpace(*f.Content))
	switch {
	case err != nil:
		return platform.File{}, shapeError(op, "%s: the content is not base64", path)
	case int64(len(content)) != f.Size:
		return platform.File{}, shapeError(op, "%s: %d bytes of content, the API says %d", path, len(content), f.Size)
	case blobID(content, len(f.SHA)) != strings.ToLower(f.SHA):
		return platform.File{}, shapeError(op, "%s: the content does not hash to blob %s", path, f.SHA)
	}
	// The last commit that changed the file, or the ref when the server
	// names none: fixed is set when that is a commit id, whose tree cannot
	// change.
	commit := f.LastCommitSHA
	if !isHexOID(commit) {
		commit = cmp.Or(ref, r.DefaultBranch, "HEAD")
	}
	fixed := isHexOID(commit)
	e, why, err := d.c.entry(ctx, op, owner, name, commit, path)
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		// Never ErrNotFound: the contents API has just found the file.
		return platform.File{}, shapeError(op, "%s: %s: the contents API found it, the trees API does not know the tree on the way (%v)",
			r.Path, path, err)
	case err != nil:
		return platform.File{}, err
	case e == nil && why == whyLink:
		return platform.File{}, fmt.Errorf("%s: %s: %s leads through a symlink at %s: %w", op, r.Path, path, commit, platform.ErrNotRegular)
	case e == nil && fixed:
		return platform.File{}, shapeError(op, "%s: %s: the contents API found it, the tree of its last commit %s lacks it (%s)",
			r.Path, path, commit, why)
	case e == nil:
		return platform.File{}, notFound(op, "%s: %s at %s: %s", r.Path, path, commit, why)
	case !strings.EqualFold(e.SHA, f.SHA) && fixed:
		return platform.File{}, shapeError(op, "%s: %s: the tree of its last commit %s holds another blob than the contents API returned",
			r.Path, path, commit)
	case !strings.EqualFold(e.SHA, f.SHA):
		return platform.File{}, &platform.Error{Op: op, Class: platform.ClassConflict,
			Err: fmt.Errorf("%s: %s: the tree of %s holds another blob than the contents API returned", r.Path, path, commit)}
	}
	mode := normalMode(e.Mode)
	if mode != modeFile && mode != modeExecutable {
		return platform.File{}, fmt.Errorf("%s: %s: %s has mode %s: %w", op, r.Path, path, mode, platform.ErrNotRegular)
	}
	return platform.File{Path: path, Mode: mode, OID: strings.ToLower(f.SHA), Content: content}, nil
}

// Why the walk of entry found no entry.
const (
	whyMissing = "missing"
	// whyLink: a directory on the way is a symlink.
	whyLink = "through a symlink"
	// whyFile: a directory on the way is a file or a submodule.
	whyFile = "through a file"
)

// entry walks the tree of commit to path, one directory per request and
// never through a symlink, and returns path's entry; nil and why the walk
// ended when a directory on the way or the entry is missing, or a part of
// the way is no directory. A failed request comes back as it is, with its
// class and wait: the caller judges a 404.
func (c *client) entry(ctx context.Context, op, owner, name, commit, path string) (*apiTreeEntry, string, error) {
	treeish := commit
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		e, err := c.treeEntry(ctx, op, owner, name, treeish, seg)
		switch {
		case err != nil:
			return nil, "", err
		case e == nil:
			return nil, whyMissing, nil
		case i == len(segments)-1:
			return e, "", nil
		}
		switch normalMode(e.Mode) {
		case modeTree:
			treeish = e.SHA
		case modeSymlink:
			return nil, whyLink, nil
		default:
			return nil, whyFile, nil
		}
	}
	return nil, whyMissing, nil
}

// treeEntry finds the entry named seg in the tree treeish (a commit or a
// tree id), reading pages until it shows; nil when the tree lacks it.
func (c *client) treeEntry(ctx context.Context, op, owner, name, treeish, seg string) (*apiTreeEntry, error) {
	u := c.endpoint("repos", owner, name, "git", "trees", treeish)
	q := url.Values{"per_page": {strconv.Itoa(treePageSize)}}
	for page := 1; page <= maxTreePages; page++ {
		q.Set("page", strconv.Itoa(page))
		var t apiTree
		if _, err := c.get(ctx, op, u, q, &t); err != nil {
			return nil, err
		}
		for i := range t.Entries {
			if t.Entries[i].Path == seg {
				return &t.Entries[i], nil
			}
		}
		if !t.Truncated || len(t.Entries) == 0 {
			return nil, nil
		}
	}
	return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
		Err: fmt.Errorf("%s/%s: the tree %s has more than %d pages", owner, name, treeish, maxTreePages)}
}

// normalMode returns a tree entry mode in git's six-digit form.
func normalMode(m string) string {
	if len(m) < 6 {
		return strings.Repeat("0", 6-len(m)) + m
	}
	return m
}

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

// stripSpace removes the line breaks some servers put into base64.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, s)
}
