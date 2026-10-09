package bitbucketdc

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// modeFile is the mode of every file the driver reads: the REST API tells
// no mode (an executable file reads as a plain one).
const modeFile = "100644"

// Node types of GET …/browse/{path}?type=true.
const (
	nodeFile      = "FILE"
	nodeDirectory = "DIRECTORY"
	nodeSubmodule = "SUBMODULE"
)

// Exceptions of a 404 that say what is missing. The reference names none;
// these are the Java exceptions of Bitbucket's API (its Javadoc), whose
// class names the error body carries.
var (
	// missingPathExceptions say the path is missing at a commit that
	// exists.
	missingPathExceptions = []string{"NoSuchPathException"}
	// missingOtherExceptions say the repository or the commit is missing.
	missingOtherExceptions = []string{"NoSuchRepositoryException", "NoSuchCommitException", "NoSuchProjectException"}
)

// ReadFile returns a regular file of r at ref ("" is the default branch
// head): its type and size from GET …/browse/{path}?type=true and ?size=
// true, then its raw bytes from GET …/raw/{path}, all at the commit ref
// resolves to, so that the answers describe the same tree. The blob id is
// computed here and the mode is always 100644: the API gives neither.
// Directories and submodules are ErrNotRegular, a file over max bytes
// ErrTooLarge. Symlinks are not documented: one may read as a file whose
// content is its target.
//
// Only a path that an existing commit lacks is ErrNotFound: the core takes a
// missing opt-in file for an opt-out and closes the target's pull requests.
// A missing repository, ref or commit, a 404 that does not say the path is
// missing while the commit is gone, and a file that the type shows and the
// raw read does not find are ClassUnknown. An empty repository has no file:
// ErrNotFound.
func (d *reader) ReadFile(ctx context.Context, r platform.Repo, ref, filePath string, max int64) (platform.File, error) {
	const op = "read file"
	key, slug, err := repoPath(op, r)
	if err != nil {
		return platform.File{}, err
	}
	if err := checkTreePath(filePath); err != nil {
		return platform.File{}, invalid(op, "%v", err)
	}
	if max < 0 {
		return platform.File{}, invalid(op, "negative size limit %d", max)
	}
	commit, err := d.c.commitOf(ctx, op, r, key, slug, ref)
	if err != nil {
		return platform.File{}, err
	}
	var node struct {
		Type string `json:"type"`
	}
	_, err = d.c.get(ctx, op, d.c.browseURL(key, slug, filePath), url.Values{"at": {commit}, "type": {"true"}}, &node)
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		return platform.File{}, d.c.missingPath(ctx, op, r, key, slug, commit, filePath, err)
	case err != nil:
		return platform.File{}, err
	}
	switch node.Type {
	case nodeFile:
	case nodeDirectory:
		return platform.File{}, fmt.Errorf("%s: %s: %s is a directory: %w", op, r.Path, filePath, platform.ErrNotRegular)
	case nodeSubmodule:
		return platform.File{}, fmt.Errorf("%s: %s: %s is a submodule: %w", op, r.Path, filePath, platform.ErrNotRegular)
	default:
		return platform.File{}, shapeError(op, "%s: node type %q", filePath, node.Type)
	}
	var size struct {
		Size *int64 `json:"size"`
	}
	_, err = d.c.get(ctx, op, d.c.browseURL(key, slug, filePath), url.Values{"at": {commit}, "size": {"true"}}, &size)
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		return platform.File{}, unknown(op, err, "%s: %s at %s: the type shows it, its size is not found", r.Path, filePath, commit)
	case err != nil:
		return platform.File{}, err
	case size.Size == nil || *size.Size < 0:
		return platform.File{}, shapeError(op, "%s: no size", filePath)
	case *size.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, filePath, *size.Size, max, platform.ErrTooLarge)
	}
	content, err := d.c.raw(ctx, op, d.c.repoURL(key, slug, append([]string{"raw"}, pathSegments(filePath)...)...), url.Values{"at": {commit}})
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		// Never ErrNotFound: the type of the same commit has just shown it.
		return platform.File{}, unknown(op, err, "%s: %s at %s: the type shows it, its content is not found", r.Path, filePath, commit)
	case err != nil:
		return platform.File{}, err
	case int64(len(content)) != *size.Size:
		return platform.File{}, shapeError(op, "%s: %d bytes of content, the size is %d", filePath, len(content), *size.Size)
	}
	return platform.File{Path: filePath, Mode: modeFile, OID: blobID(content), Content: content}, nil
}

// browseURL is the browse API's URL of filePath in key/slug.
func (c *client) browseURL(key, slug, filePath string) string {
	return c.repoURL(key, slug, append([]string{"browse"}, pathSegments(filePath)...)...)
}

// commitOf resolves ref of r to a full commit id: a full id is taken as it
// is (missingPath checks that it exists when a path is missing), a branch
// through the branch API, and "" through the default branch (GET
// …/branches/default). A ref the API does not find, or a repository it
// does not find, is ClassUnknown: never a missing file. An empty
// repository is ErrNotFound.
func (c *client) commitOf(ctx context.Context, op string, r platform.Repo, key, slug, ref string) (string, error) {
	if isHexOID(ref) {
		return strings.ToLower(ref), nil
	}
	if ref != "" {
		b, ok, err := c.branch(ctx, op, key, slug, strings.TrimPrefix(ref, "refs/heads/"))
		switch {
		case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
			return "", unknown(op, err, "%s: the repository is not found", r.Path)
		case err != nil:
			return "", err
		case !ok:
			return "", unknown(op, nil, "%s: ref %s is not found", r.Path, ref)
		}
		return strings.ToLower(b.LatestCommit), nil
	}
	def, err := c.defaultBranchOf(ctx, op, key, slug)
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		return "", unknown(op, err, "%s: the repository is not found", r.Path)
	case err != nil:
		return "", err
	case def.empty:
		return "", notFound(op, "%s: the repository is empty", r.Path)
	case def.missing || def.head == "":
		return "", unknown(op, nil, "%s: the default branch %s does not exist", r.Path, def.name)
	}
	return def.head, nil
}

// missingPath judges err, a 404 of the type of filePath at commit. A path
// is missing only at a commit that exists: the 404 names
// NoSuchPathException, or names no exception of a missing repository or
// commit and the commit API finds the commit (GET …/commits/{id}); else
// the repository or the commit is gone, which is ClassUnknown.
func (c *client) missingPath(ctx context.Context, op string, r platform.Repo, key, slug, commit, filePath string, err error) error {
	if named, _ := missingException(err, missingPathExceptions...); named {
		return notFound(op, "%s: %s at %s", r.Path, filePath, commit)
	}
	if named, _ := missingException(err, missingOtherExceptions...); named {
		return unknown(op, err, "%s: %s at %s: the repository or the commit is not found", r.Path, filePath, commit)
	}
	_, cerr := c.get(ctx, op, c.repoURL(key, slug, "commits", commit), nil, nil)
	switch {
	case cerr != nil && platform.ClassOf(cerr) == platform.ClassNotFound:
		return unknown(op, err, "%s: %s at %s: neither the file nor the commit is found", r.Path, filePath, commit)
	case cerr != nil:
		return cerr
	}
	return notFound(op, "%s: %s at %s", r.Path, filePath, commit)
}

// checkTreePath accepts a repository-relative path: no leading slash, no
// empty, "." or ".." segment, no NUL.
func checkTreePath(p string) error {
	if p == "" {
		return errors.New("empty path")
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

// blobID is git's SHA-1 id of a blob with content: the driver reads SHA-1
// repositories only (Repo.ObjectFormat).
func blobID(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// isHexOID reports whether s is a full SHA-1 object id.
func isHexOID(s string) bool { return len(s) == 40 && isHex(s) }

// isHex reports whether s is a non-empty string of hexadecimal digits.
func isHex(s string) bool {
	if s == "" {
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
