package azuredevops

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Git modes of tree entries, as the Trees API writes them ("100644",
// "40000"; the sample answers of Trees - Get).
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeSymlink    = "120000"
	modeSubmodule  = "160000"
)

// apiItem is an item of the Items API (GitItem): a path at a commit.
type apiItem struct {
	ObjectID      string `json:"objectId"`
	GitObjectType string `json:"gitObjectType"`
	CommitID      string `json:"commitId"`
	Path          string `json:"path"`
	IsFolder      bool   `json:"isFolder"`
	IsSymLink     bool   `json:"isSymLink"`
}

// apiTree is a tree of the Trees API (GitTreeRef) with its entries.
type apiTree struct {
	ObjectID    string         `json:"objectId"`
	TreeEntries []apiTreeEntry `json:"treeEntries"`
}

// apiTreeEntry is one entry of a tree (GitTreeEntryRef).
type apiTreeEntry struct {
	ObjectID      string `json:"objectId"`
	RelativePath  string `json:"relativePath"`
	Mode          string `json:"mode"`
	GitObjectType string `json:"gitObjectType"`
	Size          *int64 `json:"size"`
}

// ReadFile returns a regular file of r at ref ("" is the default branch
// head, a full commit id that commit, anything else a branch), in three
// reads that describe the same commit:
//
//  1. the Items API describes the file's folder at ref (its tree id and the
//     commit ref resolves to): GET …/items?path=<folder>&versionDescriptor…;
//  2. the Trees API lists that folder: the file's entry gives its git mode,
//     blob id and size (the Items API has no mode, and no size);
//  3. the Blobs API gives the blob's bytes ($format=octetstream), whose
//     git id must be the entry's.
//
// Symlinks, submodules and directories are ErrNotRegular, a file over max
// bytes ErrTooLarge.
//
// Only a path missing at an existing commit is ErrNotFound: the folder's
// 404 with typeKey GitItemNotFoundException, or a folder without the
// entry. The core takes a missing opt-in file for an opt-out and closes the
// target's pull requests: a missing branch or commit
// (GitUnresolvableToCommitException), a missing repository, or any other
// 404 are ClassUnknown. An empty repository has no file: ErrNotFound. A
// default branch that r names and the repository no longer has is read
// once more as the repository names it now.
func (d *reader) ReadFile(ctx context.Context, r platform.Repo, ref, filePath string, max int64) (platform.File, error) {
	const op = "read file"
	if !guidRe.MatchString(r.ID) {
		return platform.File{}, invalid(op, "repository %s has no id", r.Path)
	}
	if err := checkTreePath(filePath); err != nil {
		return platform.File{}, invalid(op, "%v", err)
	}
	if max < 0 {
		return platform.File{}, invalid(op, "negative size limit %d", max)
	}
	dir, name := path.Split(filePath)
	dir = "/" + strings.TrimSuffix(dir, "/")
	folder, err := d.c.folder(ctx, op, r, ref, dir)
	if err != nil {
		return platform.File{}, err
	}
	switch {
	case folder.IsSymLink:
		return platform.File{}, fmt.Errorf("%s: %s: %s leads through a symlink at %s: %w", op, r.Path, filePath, dir, platform.ErrNotRegular)
	case !strings.EqualFold(folder.GitObjectType, "tree"):
		return platform.File{}, notFound(op, "%s: %s at %s: %s is no directory", r.Path, filePath, folder.CommitID, dir)
	case !isHexOID(folder.ObjectID):
		return platform.File{}, shapeError(op, "%s: the folder %s has no tree id", r.Path, dir)
	}
	var tree apiTree
	if _, err := d.c.get(ctx, op, d.c.repoAPI(r.ID, "trees", strings.ToLower(folder.ObjectID)), nil, &tree); err != nil {
		if platform.ClassOf(err) == platform.ClassNotFound {
			// Never ErrNotFound: the Items API has just shown the tree.
			return platform.File{}, unknown(op, err, "%s: the tree of %s at %s is not found", r.Path, dir, folder.CommitID)
		}
		return platform.File{}, err
	}
	var e *apiTreeEntry
	for i := range tree.TreeEntries {
		if tree.TreeEntries[i].RelativePath == name {
			e = &tree.TreeEntries[i]
			break
		}
	}
	if e == nil {
		return platform.File{}, notFound(op, "%s: %s at %s", r.Path, filePath, folder.CommitID)
	}
	what := ""
	switch {
	case e.Mode == modeSymlink:
		what = "a symlink"
	case e.Mode == modeSubmodule || strings.EqualFold(e.GitObjectType, "commit"):
		what = "a submodule"
	case strings.EqualFold(e.GitObjectType, "tree"):
		what = "a directory"
	case e.Mode != modeFile && e.Mode != modeExecutable || !strings.EqualFold(e.GitObjectType, "blob"):
		what = fmt.Sprintf("an entry of mode %q and type %q", e.Mode, e.GitObjectType)
	}
	if what != "" {
		return platform.File{}, fmt.Errorf("%s: %s: %s is %s: %w", op, r.Path, filePath, what, platform.ErrNotRegular)
	}
	switch {
	case !isHexOID(e.ObjectID):
		return platform.File{}, shapeError(op, "%s: the entry of %s has no blob id", r.Path, filePath)
	case e.Size == nil || *e.Size < 0:
		return platform.File{}, shapeError(op, "%s: the entry of %s has no size", r.Path, filePath)
	case *e.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, filePath, *e.Size, max, platform.ErrTooLarge)
	}
	oid := strings.ToLower(e.ObjectID)
	content, err := d.c.raw(ctx, op, d.c.repoAPI(r.ID, "blobs", oid), url.Values{"$format": {"octetstream"}})
	switch {
	case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
		// Never ErrNotFound: the tree of the same commit has just shown it.
		return platform.File{}, unknown(op, err, "%s: %s at %s: the tree shows it, its blob is not found", r.Path, filePath, folder.CommitID)
	case err != nil:
		return platform.File{}, err
	case int64(len(content)) != *e.Size || blobID(content) != oid:
		return platform.File{}, shapeError(op, "%s: the blob of %s is not %s (%d bytes)", r.Path, filePath, oid, *e.Size)
	}
	mode := modeFile
	if e.Mode == modeExecutable {
		mode = modeExecutable
	}
	return platform.File{Path: filePath, Mode: mode, OID: oid, Content: content}, nil
}

// folder reads the item of dir (an absolute folder path, "/" for the root)
// of r at ref. A missing path is ErrNotFound; a missing branch, commit or
// repository, or any other 404, is ClassUnknown. ref "" is r's default
// branch: an empty repository is ErrNotFound, and a default branch the
// repository no longer has is read once more as the repository names it.
func (c *client) folder(ctx context.Context, op string, r platform.Repo, ref, dir string) (*apiItem, error) {
	if ref != "" {
		return c.item(ctx, op, r, ref, dir)
	}
	branch := r.DefaultBranch
	for range 2 {
		if branch != "" {
			it, err := c.item(ctx, op, r, branch, dir)
			if err == nil || typeKeyOf(err) != keyUnresolvable {
				return it, err
			}
		}
		// No default branch, or not where r says: the repository tells.
		repo, err := c.getRepo(ctx, op, r.ID)
		switch {
		case err != nil && platform.ClassOf(err) == platform.ClassNotFound:
			return nil, unknown(op, err, "%s: the repository is not found", r.Path)
		case err != nil:
			return nil, err
		case repo.DefaultBranch == "":
			return nil, notFound(op, "%s: the repository is empty", r.Path)
		case strings.TrimPrefix(repo.DefaultBranch, "refs/heads/") == branch:
			return nil, unknown(op, nil, "%s: the default branch %s does not resolve", r.Path, branch)
		}
		branch = strings.TrimPrefix(repo.DefaultBranch, "refs/heads/")
	}
	return nil, unknown(op, nil, "%s: the default branch moved while it was read", r.Path)
}

// item reads the item of dir at version (a branch name or a full commit
// id): GET {org}/_apis/git/repositories/{id}/items?path=…&
// versionDescriptor.version=…&versionDescriptor.versionType=branch|commit.
func (c *client) item(ctx context.Context, op string, r platform.Repo, version, dir string) (*apiItem, error) {
	kind := "branch"
	if isHexOID(version) {
		kind = "commit"
	}
	q := url.Values{
		"path":                          {dir},
		"versionDescriptor.version":     {version},
		"versionDescriptor.versionType": {kind},
	}
	var it apiItem
	_, err := c.get(ctx, op, c.repoAPI(r.ID, "items"), q, &it)
	switch {
	case err == nil:
		return &it, nil
	case typeKeyOf(err) == keyItemNotFound:
		return nil, notFound(op, "%s: %s at %s %s", r.Path, dir, kind, version)
	case platform.ClassOf(err) == platform.ClassNotFound:
		// The branch, the commit or the repository: never a missing file.
		return nil, unknown(op, err, "%s: %s %s", r.Path, kind, version)
	}
	return nil, err
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

// blobID is git's SHA-1 id of a blob with content: Azure Repos hosts SHA-1
// repositories only.
func blobID(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// isHexOID reports whether s is a full SHA-1 object id.
func isHexOID(s string) bool {
	if len(s) != 40 {
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
