package gitlab

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"path"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxTreePages bounds the listing of one directory: 50 000 entries, the
// default maximum offset of offset pagination.
const maxTreePages = 500

// Tree entry modes.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeTree       = "040000"
	modeSymlink    = "120000"
	modeSubmodule  = "160000"
)

// apiFile is GET /projects/:id/repository/files/:file_path
// (https://docs.gitlab.com/api/repository_files/#get-file-from-repository).
type apiFile struct {
	FilePath        string  `json:"file_path"`
	Size            int64   `json:"size"`
	Encoding        string  `json:"encoding"`
	Content         *string `json:"content"`
	BlobID          string  `json:"blob_id"`
	CommitID        string  `json:"commit_id"`
	LastCommitID    string  `json:"last_commit_id"`
	ExecuteFilemode *bool   `json:"execute_filemode"`
}

// apiTreeEntry is an entry of GET /projects/:id/repository/tree: type
// "blob", "tree" or "commit" (a submodule), mode in git's form.
type apiTreeEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// ReadFile returns a regular file of r at ref ("" is the default branch):
// its content and blob id from the files API, its mode from the tree of the
// commit the files API read (the files API returns a symlink's target and
// a submodule as blobs: Gitlab::Git::Blob.tree_entry). Symlinks,
// submodules and directories are ErrNotRegular, a missing path ErrNotFound,
// a file over max bytes ErrTooLarge. The content is checked against the
// blob id.
//
// Only "404 File Not Found" of the files API, or a missing default branch
// commit of an empty project, is ErrNotFound: the core takes a missing
// opt-in file for an opt-out and closes the target's pull requests, so a
// missing project or ref ("404 Project Not Found",
// "404 Commit Not Found"), a 404 of anything else in front of GitLab, and a
// tree that contradicts the files API are ClassUnknown.
func (d *reader) ReadFile(ctx context.Context, r platform.Repo, ref, filePath string, max int64) (platform.File, error) {
	const op = "read file"
	if !checkFullPath(r.Path, 2) && projectID(r) == r.Path {
		return platform.File{}, invalid(op, "%q is not a group/…/project path", r.Path)
	}
	if err := checkTreePath(filePath); err != nil {
		return platform.File{}, invalid(op, "%v", err)
	}
	if max < 0 {
		return platform.File{}, invalid(op, "negative size limit %d", max)
	}
	id := projectID(r)
	at := ref
	if at == "" {
		// The default branch (https://docs.gitlab.com/api/repository_files/).
		at = "HEAD"
	}
	var f apiFile
	_, err := d.c.get(ctx, op, d.c.projectURL(id, "repository", "files", filePath), url.Values{"ref": {at}}, &f)
	if err != nil {
		return platform.File{}, d.fileError(ctx, op, r, id, ref, at, filePath, err)
	}
	switch {
	case !isHexOID(f.BlobID):
		return platform.File{}, shapeError(op, "%s: no blob id", filePath)
	case !isHexOID(f.CommitID):
		return platform.File{}, shapeError(op, "%s: no commit id", filePath)
	}
	// The mode comes from the tree of the commit just read, which cannot
	// change: anything but the file's entry there contradicts the files API.
	e, why, err := d.c.entry(ctx, op, id, f.CommitID, filePath)
	switch {
	case err != nil:
		if platform.ClassOf(err) == platform.ClassNotFound {
			return platform.File{}, unknown(op, fmt.Errorf("%s: %s: the files API found it, the tree of %s does not (%w)", r.Path, filePath, f.CommitID, errNoEntry))
		}
		return platform.File{}, err
	case e == nil && why == whyLink:
		return platform.File{}, fmt.Errorf("%s: %s: %s leads through a symlink: %w", op, r.Path, filePath, platform.ErrNotRegular)
	case e == nil:
		return platform.File{}, shapeError(op, "%s: %s: the files API found it, the tree of %s lacks it (%s)", r.Path, filePath, f.CommitID, why)
	}
	mode := normalMode(e.Mode)
	switch {
	case mode != modeFile && mode != modeExecutable:
		return platform.File{}, fmt.Errorf("%s: %s: %s has mode %s: %w", op, r.Path, filePath, mode, platform.ErrNotRegular)
	case !strings.EqualFold(e.ID, f.BlobID):
		return platform.File{}, shapeError(op, "%s: %s: the tree of %s holds another blob than the files API returned", r.Path, filePath, f.CommitID)
	case f.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, filePath, f.Size, max, platform.ErrTooLarge)
	case f.Content == nil || f.Encoding != "base64":
		return platform.File{}, shapeError(op, "%s: the content is not base64", filePath)
	}
	content, err := base64.StdEncoding.DecodeString(stripSpace(*f.Content))
	switch {
	case err != nil:
		return platform.File{}, shapeError(op, "%s: the content is not base64", filePath)
	case int64(len(content)) != f.Size:
		return platform.File{}, shapeError(op, "%s: %d bytes of content, the API says %d", filePath, len(content), f.Size)
	case blobID(content, len(f.BlobID)) != strings.ToLower(f.BlobID):
		return platform.File{}, shapeError(op, "%s: the content does not hash to blob %s", filePath, f.BlobID)
	}
	return platform.File{Path: filePath, Mode: mode, OID: strings.ToLower(f.BlobID), Content: content}, nil
}

// errNoEntry says a tree lacks a path the files API found.
var errNoEntry = errors.New("no such tree entry")

// fileError judges err, a failed GET of filePath at at (ref, or "HEAD" for
// ""). See ReadFile.
func (d *reader) fileError(ctx context.Context, op string, r platform.Repo, id, ref, at, filePath string, err error) error {
	if errors.Is(err, httpx.ErrTooLarge) {
		return fmt.Errorf("%s: %s: %s: the response is over the size bound: %w", op, r.Path, filePath, platform.ErrTooLarge)
	}
	what, is404 := missing(err)
	switch {
	case !is404:
		return err
	case what == "file":
		// A missing path, or a directory (the files API reads blobs only).
		e, why, terr := d.c.entry(ctx, op, id, at, filePath)
		switch {
		case terr != nil && platform.ClassOf(terr) == platform.ClassNotFound:
			// The project went away in between: never an opt-out.
			return unknown(op, terr)
		case terr != nil:
			return terr
		case e == nil && why == whyLink:
			return fmt.Errorf("%s: %s: %s leads through a symlink: %w", op, r.Path, filePath, platform.ErrNotRegular)
		case e == nil:
			return err
		case e.Type == "tree" || e.Type == "commit":
			return fmt.Errorf("%s: %s: %s is a %s: %w", op, r.Path, filePath, e.Type, platform.ErrNotRegular)
		}
		// A blob the files API did not find: the ref moved in between.
		return &platform.Error{Op: op, Class: platform.ClassConflict,
			Err: fmt.Errorf("%s: %s at %s: the tree has it, the files API had not", r.Path, filePath, at)}
	case what == "commit" && ref == "":
		// No commit at the default branch: an empty project, or one whose
		// default branch is gone.
		p, perr := d.c.getProject(ctx, op, id)
		switch {
		case perr != nil:
			if platform.ClassOf(perr) == platform.ClassNotFound {
				return unknown(op, perr)
			}
			return perr
		case p.EmptyRepo:
			return notFound(op, "%s: %s: the repository is empty", r.Path, filePath)
		}
	}
	// A missing project or ref, or a 404 of something that is not GitLab:
	// never an opt-out.
	return unknown(op, err)
}

// Why the walk of entry found no entry.
const (
	whyMissing = "missing"
	// whyLink: a directory on the way is a symlink.
	whyLink = "through a symlink"
	// whyFile: a directory on the way is a file or a submodule.
	whyFile = "through a file"
)

// entry finds the entry of filePath in the tree of ref: from the listing
// of its directory, and, when that directory is not there, from the
// listings of the directories on the way from the root, so that a symlink
// or a file on the way tells why (git never looks through a symlink). A
// failed listing comes back as it is.
func (c *client) entry(ctx context.Context, op, id, ref, filePath string) (*apiTreeEntry, string, error) {
	dir, name := path.Split(filePath)
	dir = strings.TrimSuffix(dir, "/")
	e, dirFound, err := c.treeEntry(ctx, op, id, ref, dir, name)
	switch {
	case err != nil:
		return nil, "", err
	case e != nil:
		return e, "", nil
	case dirFound || dir == "":
		return nil, whyMissing, nil
	}
	// The directory is not there: walk from the root to tell why.
	cur := ""
	for seg := range strings.SplitSeq(dir, "/") {
		e, _, err := c.treeEntry(ctx, op, id, ref, cur, seg)
		switch {
		case err != nil:
			return nil, "", err
		case e == nil:
			return nil, whyMissing, nil
		}
		switch normalMode(e.Mode) {
		case modeTree:
		case modeSymlink:
			return nil, whyLink, nil
		default:
			return nil, whyFile, nil
		}
		cur = path.Join(cur, seg)
	}
	return nil, whyMissing, nil
}

// treeEntry finds the entry named name in directory dir ("" for the root)
// of the tree of ref, reading pages until it shows; nil when the directory
// lacks it. dirFound is false when the directory is not there: GitLab ≥
// 17.7 answers 404 for a path that is no directory, older ones an empty
// list (a git tree is never empty but the root of an empty repository).
// Every 404 of GitLab but "404 Project Not Found" counts as that: ref is a
// commit the files API has just resolved.
func (c *client) treeEntry(ctx context.Context, op, id, ref, dir, name string) (e *apiTreeEntry, dirFound bool, err error) {
	q := url.Values{"ref": {ref}}
	if dir != "" {
		q.Set("path", dir)
	}
	errFound := errors.New("found")
	complete, err := listAll(ctx, c, op, c.projectURL(id, "repository", "tree"), q, maxTreePages, func(it apiTreeEntry) error {
		dirFound = true
		if it.Name == name {
			e = &it
			return errFound
		}
		return nil
	})
	switch {
	case errors.Is(err, errFound):
		return e, true, nil
	case err != nil:
		if what, ok := missing(err); ok && what != "" && what != "project" && !laterPage(err) {
			return nil, false, nil
		}
		return nil, false, err
	case !complete:
		return nil, false, unknown(op, fmt.Errorf("the directory %q at %s has more than %d pages", dir, ref, maxTreePages))
	}
	return nil, dirFound, nil
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
