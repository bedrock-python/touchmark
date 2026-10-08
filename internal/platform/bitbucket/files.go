package bitbucket

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// File modes of regular files.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
)

// missingFileMessage starts the message of a 404 of the source API for a
// path that a commit lacks ("No such file or directory: <path>", seen
// anonymously); a missing repository or commit says something else.
const missingFileMessage = "no such file or directory"

// apiEntry is GET …/src/{commit}/{path}?format=meta: a commit_file or a
// commit_directory.
type apiEntry struct {
	Type       string     `json:"type"`
	Path       string     `json:"path"`
	Size       *int64     `json:"size"`
	Attributes attributes `json:"attributes"`
}

// attributes are a file's modifiers: link, executable, subrepository,
// binary, lfs. The OpenAPI description types them as one string; the API
// sends a list ([] for a plain file), so both are read.
type attributes []string

func (a *attributes) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*a = nil
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = attributes{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*a = list
	return nil
}

// has reports whether a holds the attribute name.
func (a attributes) has(name string) bool { return slices.Contains(a, name) }

// ReadFile returns a regular file of r at ref ("" is the default branch
// head): its kind, size and mode from the source API's metadata, then its
// raw bytes, at the commit ref resolves to, so that both answers describe
// the same tree. The blob id is computed here (the API gives none).
// Symlinks, submodules, directories and Git LFS files are ErrNotRegular, a
// file over max bytes ErrTooLarge.
//
// Only a path that an existing commit lacks is ErrNotFound: the core takes a
// missing opt-in file for an opt-out and closes the target's pull requests.
// A missing repository, ref or commit, a 404 the source API gives for
// anything else, and a file that the metadata shows and the raw read does
// not find are ClassUnknown. An empty repository has no file: ErrNotFound.
func (d *reader) ReadFile(ctx context.Context, r platform.Repo, ref, filePath string, max int64) (platform.File, error) {
	const op = "read file"
	ws, slug, err := repoPath(op, r)
	if err != nil {
		return platform.File{}, err
	}
	if err := checkTreePath(filePath); err != nil {
		return platform.File{}, invalid(op, "%v", err)
	}
	if max < 0 {
		return platform.File{}, invalid(op, "negative size limit %d", max)
	}
	commit, err := d.c.commitOf(ctx, op, r, ws, slug, ref)
	if err != nil {
		return platform.File{}, err
	}
	e, err := d.c.meta(ctx, op, ws, slug, commit, filePath)
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		return platform.File{}, d.c.missingPath(ctx, op, r, ws, slug, commit, filePath, err)
	case err != nil:
		return platform.File{}, err
	}
	what := ""
	switch {
	case e.Type == "commit_directory":
		what = "a directory"
	case e.Type != "commit_file":
		what = "a " + cmp.Or(e.Type, "thing without a type")
	case e.Attributes.has("link"):
		what = "a symlink"
	case e.Attributes.has("subrepository"):
		what = "a submodule"
	case e.Attributes.has("lfs"):
		what = "a Git LFS file"
	}
	if what != "" {
		return platform.File{}, fmt.Errorf("%s: %s: %s is %s: %w", op, r.Path, filePath, what, platform.ErrNotRegular)
	}
	switch {
	case e.Path != "" && e.Path != filePath:
		return platform.File{}, shapeError(op, "%s: the metadata describes %s", filePath, e.Path)
	case e.Size == nil || *e.Size < 0:
		return platform.File{}, shapeError(op, "%s: the metadata has no size", filePath)
	case *e.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, filePath, *e.Size, max, platform.ErrTooLarge)
	}
	content, err := d.c.raw(ctx, op, d.c.srcURL(ws, slug, commit, filePath))
	switch {
	case errors.Is(err, errOffsite):
		return platform.File{}, fmt.Errorf("%s: %s: %s: the API serves its content from another host, as it serves Git LFS files: %w",
			op, r.Path, filePath, platform.ErrNotRegular)
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		// Never ErrNotFound: the metadata of the same commit has just shown it.
		return platform.File{}, unknown(op, err, "%s: %s at %s: the metadata shows it, its content is not found", r.Path, filePath, commit)
	case err != nil:
		return platform.File{}, err
	case int64(len(content)) != *e.Size:
		return platform.File{}, shapeError(op, "%s: %d bytes of content, the metadata says %d", filePath, len(content), *e.Size)
	}
	mode := modeFile
	if e.Attributes.has("executable") {
		mode = modeExecutable
	}
	return platform.File{Path: filePath, Mode: mode, OID: blobID(content), Content: content}, nil
}

// commitOf resolves ref of r to a full commit id: a full id is taken as it
// is (missingPath checks that it exists when a path is missing), a branch
// through the branch API, and "" through the default branch. A ref the API
// does not find, or a repository it does not find, is ClassUnknown: never a
// missing file. A repository without a main branch is empty: ErrNotFound.
// When the default branch r names is gone, the repository's current one is
// read once more (the default branch was renamed in between).
func (c *client) commitOf(ctx context.Context, op string, r platform.Repo, ws, slug, ref string) (string, error) {
	if isHexOID(ref) {
		return strings.ToLower(ref), nil
	}
	if ref != "" {
		hash, err := c.branchHead(ctx, op, ws, slug, ref)
		if err != nil && platform.ClassOf(err) == platform.ClassNotFound {
			return "", unknown(op, err, "%s: ref %s is not found", r.Path, ref)
		}
		return hash, err
	}
	branch := r.DefaultBranch
	for range 2 {
		if branch != "" {
			hash, err := c.branchHead(ctx, op, ws, slug, branch)
			if err == nil || platform.ClassOf(err) != platform.ClassNotFound {
				return hash, err
			}
		}
		// No default branch, or not where r says: the repository tells.
		repo, err := c.getRepo(ctx, op, ws, slug)
		switch {
		case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
			return "", unknown(op, err, "%s: the repository is not found", r.Path)
		case err != nil:
			return "", err
		case repo.MainBranch == nil || repo.MainBranch.Name == "":
			return "", notFound(op, "%s: the repository is empty", r.Path)
		case repo.MainBranch.Name == branch:
			return "", unknown(op, nil, "%s: the branch API does not find the default branch %s", r.Path, branch)
		}
		branch = repo.MainBranch.Name
	}
	return "", unknown(op, nil, "%s: the default branch moved while it was read", r.Path)
}

// branchHead returns the head commit of branch in ws/slug: GET
// …/refs/branches/{name}, the name's slashes kept.
func (c *client) branchHead(ctx context.Context, op, ws, slug, branch string) (string, error) {
	var b apiBranch
	if _, err := c.get(ctx, op, c.endpoint(append([]string{"repositories", ws, slug, "refs", "branches"}, pathSegments(branch)...)...), nil, &b); err != nil {
		return "", err
	}
	if b.Target == nil || !isHexOID(b.Target.Hash) {
		return "", shapeError(op, "branch %s: no head commit", branch)
	}
	return strings.ToLower(b.Target.Hash), nil
}

// srcURL is the source API's URL of filePath at commit.
func (c *client) srcURL(ws, slug, commit, filePath string) string {
	return c.endpoint(append([]string{"repositories", ws, slug, "src", commit}, pathSegments(filePath)...)...)
}

// meta reads the metadata of filePath at commit.
func (c *client) meta(ctx context.Context, op, ws, slug, commit, filePath string) (*apiEntry, error) {
	var e apiEntry
	if _, err := c.get(ctx, op, c.srcURL(ws, slug, commit, filePath), url.Values{"format": {"meta"}}, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// missingPath judges err, a 404 of the metadata of filePath at commit. A
// path is missing only at a commit that exists: the 404 says so ("No such
// file or directory"), or the commit API finds the commit; else the
// repository or the commit is gone, which is ClassUnknown. A missing path
// is ErrNotFound, but one that leads through a symlink, which git never
// follows, is ErrNotRegular, as on the other platforms: the directories on
// the way are read until one is missing or no directory.
func (c *client) missingPath(ctx context.Context, op string, r platform.Repo, ws, slug, commit, filePath string, err error) error {
	msg, _ := missingMessage(err)
	if !strings.HasPrefix(strings.ToLower(msg), missingFileMessage) {
		_, cerr := c.get(ctx, op, c.endpoint("repositories", ws, slug, "commit", commit), nil, nil)
		switch {
		case cerr != nil && platform.ClassOf(cerr) == platform.ClassNotFound:
			return unknown(op, err, "%s: %s at %s: neither the file nor the commit is found", r.Path, filePath, commit)
		case cerr != nil:
			return cerr
		}
	}
	segs := strings.Split(filePath, "/")
	for i := 1; i < len(segs); i++ {
		dir := strings.Join(segs[:i], "/")
		e, derr := c.meta(ctx, op, ws, slug, commit, dir)
		switch {
		case derr != nil && platform.ClassOf(derr) == platform.ClassNotFound:
			return notFound(op, "%s: %s at %s: %s is missing", r.Path, filePath, commit, dir)
		case derr != nil:
			return derr
		case e.Type == "commit_directory":
			continue
		case e.Type == "commit_file" && e.Attributes.has("link"):
			return fmt.Errorf("%s: %s: %s leads through a symlink at %s: %w", op, r.Path, filePath, dir, platform.ErrNotRegular)
		}
		return notFound(op, "%s: %s at %s: %s is no directory", r.Path, filePath, commit, dir)
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

// blobID is git's SHA-1 id of a blob with content: Bitbucket hosts SHA-1
// repositories only.
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
