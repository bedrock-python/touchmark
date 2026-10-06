package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// batchSize is how many repositories one GraphQL request reads a file of.
const batchSize = 50

// gqlRepoFile is one alias of batchFilesQuery.
type gqlRepoFile struct {
	DatabaseID int64 `json:"databaseId"`
	Object     *struct {
		OID  string `json:"oid"`
		File *struct {
			Mode   int    `json:"mode"`
			Type   string `json:"type"`
			OID    string `json:"oid"`
			Size   int64  `json:"size"`
			Object *struct {
				ByteSize    int64   `json:"byteSize"`
				IsBinary    *bool   `json:"isBinary"`
				IsTruncated bool    `json:"isTruncated"`
				OID         string  `json:"oid"`
				Text        *string `json:"text"`
			} `json:"object"`
		} `json:"file"`
	} `json:"object"`
}

// ReadFiles reads path at the default branch head of each repository
// (platform.BatchReader): one GraphQL request per 50 repositories of one
// owner (an App's token covers one installation), aliases r0 … r49 with
// repository(owner:, name:) { object(expression: "HEAD") { … on Commit {
// file(path:) } } }. The entry's mode tells regular files from symlinks
// (120000), submodules (160000) and directories; a missing repository,
// commit or file is ErrNotFound (NOT_FOUND on its alias). A text blob
// comes in the answer and is checked against its id; a binary, truncated
// or unchecked one is read through ReadFile at the commit the query saw.
//
// An anonymous reader has no GraphQL: it reads file by file. A whole
// request that fails with a rate limit, a refused credential or the end of
// ctx fails the call; other failures are the failures of its repositories
// (FileErrors).
func (d *reader) ReadFiles(ctx context.Context, repos []platform.Repo, path string, max int64) ([]platform.File, error) {
	const op = "read files"
	if err := checkTreePath(path); err != nil {
		return nil, invalid(op, "%v", err)
	}
	if max < 0 {
		return nil, invalid(op, "negative size limit %d", max)
	}
	files := make([]platform.File, len(repos))
	errs := make([]error, len(repos))
	failed := false
	fail := func(i int, err error) {
		errs[i], failed = err, true
	}
	if d.c.kind == credAnonymous {
		for i, r := range repos {
			f, err := d.c.readFile(ctx, r, "", path, max)
			if err != nil {
				if stops(err) {
					return nil, err
				}
				fail(i, err)
				continue
			}
			files[i] = f
		}
		return result(files, errs, failed)
	}
	// Group by owner, in the order of first appearance.
	var owners []string
	byOwner := map[string][]int{}
	for i, r := range repos {
		if err := d.c.checkHost(op, r); err != nil {
			fail(i, err)
			continue
		}
		owner, _, err := repoPath(op, r)
		if err != nil {
			fail(i, err)
			continue
		}
		key := strings.ToLower(owner)
		if _, ok := byOwner[key]; !ok {
			owners = append(owners, key)
		}
		byOwner[key] = append(byOwner[key], i)
	}
	for _, key := range owners {
		idx := byOwner[key]
		owner, _, _ := splitRepoPath(repos[idx[0]].Path)
		a, err := d.c.ownerAuth(ctx, owner)
		if err != nil {
			if stops(err) {
				return nil, err
			}
			for _, i := range idx {
				fail(i, err)
			}
			continue
		}
		for start := 0; start < len(idx); start += batchSize {
			chunk := idx[start:min(start+batchSize, len(idx))]
			vars := map[string]any{"path": path}
			for j, i := range chunk {
				o, n, _ := splitRepoPath(repos[i].Path)
				vars["o"+strconv.Itoa(j)], vars["n"+strconv.Itoa(j)] = o, n
			}
			var data map[string]*gqlRepoFile
			fieldErrs, err := d.c.graphql(ctx, op, a, batchFilesQuery(len(chunk)), vars, &data)
			if err != nil {
				if fatal(err) {
					return nil, err
				}
				for _, i := range chunk {
					fail(i, err)
				}
				continue
			}
			for j, i := range chunk {
				alias := "r" + strconv.Itoa(j)
				f, err := d.batchFile(ctx, op, repos[i], path, max, data[alias], fieldErrs, alias)
				if err != nil {
					if stops(err) {
						return nil, err
					}
					fail(i, err)
					continue
				}
				files[i] = f
			}
		}
	}
	return result(files, errs, failed)
}

// result returns files, with FileErrors when any failed.
func result(files []platform.File, errs []error, failed bool) ([]platform.File, error) {
	if failed {
		return files, platform.FileErrors(errs)
	}
	return files, nil
}

// batchFile turns one alias of the batch into a File or ReadFile's error.
func (d *reader) batchFile(ctx context.Context, op string, r platform.Repo, path string, max int64,
	got *gqlRepoFile, errs gqlErrors, alias string) (platform.File, error) {
	if e := errs.at(alias, "object", "file"); e != nil {
		return platform.File{}, d.c.fieldError(op, e)
	}
	if e := errs.at(alias); e != nil && (got == nil || e.Type != "NOT_FOUND" || len(e.Path) == 1) {
		return platform.File{}, fmt.Errorf("%s: %w", r.Path, d.c.fieldError(op, e))
	}
	switch {
	case got == nil:
		return platform.File{}, shapeError(op, "%s: no answer for the repository", r.Path)
	case got.Object == nil:
		return platform.File{}, notFound(op, "%s has no commits", r.Path)
	case got.Object.File == nil:
		return platform.File{}, notFound(op, "%s: %s at %s", r.Path, path, got.Object.OID)
	}
	if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil && got.DatabaseID != 0 && got.DatabaseID != id {
		// The path leads to another repository now (renamed and replaced):
		// nothing about r, and never a missing opt-in file.
		return platform.File{}, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%s is now repository %d, not %s", r.Path, got.DatabaseID, r.ID)}
	}
	f := got.Object.File
	mode := gqlMode(f.Mode)
	switch {
	case mode != modeFile && mode != modeExecutable:
		return platform.File{}, fmt.Errorf("%s: %s: %s has mode %s: %w", op, r.Path, path, mode, platform.ErrNotRegular)
	case f.Size > max:
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, path, f.Size, max, platform.ErrTooLarge)
	case !isHexOID(f.OID):
		return platform.File{}, shapeError(op, "%s: %s: an entry without a blob id", r.Path, path)
	}
	if b := f.Object; b != nil && b.Text != nil && !b.IsTruncated && (b.IsBinary == nil || !*b.IsBinary) {
		content := []byte(*b.Text)
		if int64(len(content)) <= max && blobID(content, len(f.OID)) == strings.ToLower(f.OID) {
			return platform.File{Path: path, Mode: mode, OID: strings.ToLower(f.OID), Content: content}, nil
		}
	}
	// Binary, truncated, or text that does not hash to the blob (GraphQL
	// returns text as UTF-8): read the blob itself, at the commit seen.
	return d.c.readFile(ctx, r, got.Object.OID, path, max)
}
