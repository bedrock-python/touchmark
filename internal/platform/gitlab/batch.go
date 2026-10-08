package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// batchSize is how many projects one GraphQL request reads a file of. Its
// query complexity stays far below GitLab's limits (200 without a token,
// 250 with one: https://docs.gitlab.com/api/graphql/#limits); gitlab.com
// scored 45 for 50 projects on 2026-10-08.
const batchSize = 50

// batchFilesQuery reads one path at the default branch head of projects,
// by id: the head commit, which tells that the default branch exists, and
// the path's blob. Data only in variables.
const batchFilesQuery = `query($ids: [ID!]!, $first: Int!, $paths: [String!]!) {
  projects(ids: $ids, first: $first) {
    nodes {
      id
      repository {
        tree { lastCommit { sha } }
        blobs(paths: $paths, first: 1) { nodes { path mode oid size storedExternally rawTextBlob } }
      }
    }
  }
}`

// gqlProject is one node of batchFilesQuery.
type gqlProject struct {
	ID         string `json:"id"`
	Repository *struct {
		Tree *struct {
			LastCommit *struct {
				SHA string `json:"sha"`
			} `json:"lastCommit"`
		} `json:"tree"`
		Blobs *struct {
			Nodes []gqlBlob `json:"nodes"`
		} `json:"blobs"`
	} `json:"repository"`
}

// gqlBlob is a RepositoryBlob: mode as git writes it ("100644", "40000"),
// size a BigInt, rawTextBlob null for binary content.
type gqlBlob struct {
	Path             string  `json:"path"`
	Mode             string  `json:"mode"`
	OID              string  `json:"oid"`
	Size             bigInt  `json:"size"`
	StoredExternally *bool   `json:"storedExternally"`
	RawTextBlob      *string `json:"rawTextBlob"`
}

// bigInt is GraphQL's BigInt, which GitLab sends as a string or a number.
type bigInt int64

func (b *bigInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" || s == "" {
		*b = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("BigInt %s: %w", data, err)
	}
	*b = bigInt(n)
	return nil
}

// ReadFiles reads path at the default branch head of each project
// (platform.BatchReader): one GraphQL request per 50 projects, by id
// (projects(ids:) follows renames), each with its head commit and the
// blob at path (repository.blobs). A blob's mode tells regular files from
// symlinks, submodules and directories; a text blob comes in the answer
// and is checked against its id. What the answer does not settle is read
// with ReadFile, which judges it as it always does: a project the request
// did not return, one without a head commit (an empty project), a nested
// path it found no blob for (a symlink on the way), a blob stored
// externally (LFS), binary or not hashing to its id. A path in the
// repository's root that a resolved head lacks is ErrNotFound.
//
// A request that fails with a rate limit, a refused credential or the end
// of ctx fails the call; another failure, or an answer with errors, reads
// that request's projects with ReadFile, and an instance without GraphQL
// (404) is read file by file from then on.
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
	single := func(i int) error {
		f, err := d.ReadFile(ctx, repos[i], "", path, max)
		switch {
		case err == nil:
			files[i] = f
		case stops(err):
			return err
		default:
			errs[i], failed = err, true
		}
		return nil
	}
	var batch []int
	for i, r := range repos {
		if id, err := strconv.ParseInt(r.ID, 10, 64); err == nil && id > 0 && !d.c.noGraphQL.Load() {
			batch = append(batch, i)
			continue
		}
		if err := single(i); err != nil {
			return nil, err
		}
	}
	for start := 0; start < len(batch); start += batchSize {
		chunk := batch[start:min(start+batchSize, len(batch))]
		ids := make([]string, len(chunk))
		for j, i := range chunk {
			ids[j] = projectGID(repos[i].ID)
		}
		var data struct {
			Projects struct {
				Nodes []gqlProject `json:"nodes"`
			} `json:"projects"`
		}
		var got map[string]*gqlProject
		err := d.c.graphql(ctx, op, batchFilesQuery, map[string]any{"ids": ids, "first": len(chunk), "paths": []string{path}}, &data)
		switch {
		case err == nil:
			got = make(map[string]*gqlProject, len(data.Projects.Nodes))
			for k := range data.Projects.Nodes {
				got[data.Projects.Nodes[k].ID] = &data.Projects.Nodes[k]
			}
		case stops(err):
			return nil, err
		case platform.ClassOf(err) == platform.ClassNotFound, platform.ClassOf(err) == platform.ClassInvalid:
			// No GraphQL here, or one without these fields: file by file
			// from now on.
			d.c.noGraphQL.Store(true)
		}
		for _, i := range chunk {
			f, settled, err := batchFile(op, repos[i], path, max, got[projectGID(repos[i].ID)])
			switch {
			case !settled:
				if err := single(i); err != nil {
					return nil, err
				}
			case err != nil:
				errs[i], failed = err, true
			default:
				files[i] = f
			}
		}
	}
	if failed {
		return files, platform.FileErrors(errs)
	}
	return files, nil
}

// projectGID is the global id of the project with numeric id.
func projectGID(id string) string { return "gid://gitlab/Project/" + id }

// batchFile turns a project's node of the batch into a File or ReadFile's
// error; settled is false when ReadFile must judge it (see ReadFiles).
func batchFile(op string, r platform.Repo, path string, max int64, p *gqlProject) (f platform.File, settled bool, err error) {
	if p == nil || p.Repository == nil || p.Repository.Tree == nil || p.Repository.Tree.LastCommit == nil ||
		!isHexOID(p.Repository.Tree.LastCommit.SHA) || p.Repository.Blobs == nil {
		return platform.File{}, false, nil
	}
	var b *gqlBlob
	for k := range p.Repository.Blobs.Nodes {
		if p.Repository.Blobs.Nodes[k].Path == path {
			b = &p.Repository.Blobs.Nodes[k]
		}
	}
	if b == nil {
		if strings.Contains(path, "/") {
			return platform.File{}, false, nil
		}
		return platform.File{}, true, notFound(op, "%s: %s at %s", r.Path, path, p.Repository.Tree.LastCommit.SHA)
	}
	mode := normalMode(b.Mode)
	switch {
	case mode != modeFile && mode != modeExecutable:
		return platform.File{}, true, fmt.Errorf("%s: %s: %s has mode %s: %w", op, r.Path, path, mode, platform.ErrNotRegular)
	case int64(b.Size) > max:
		return platform.File{}, true, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w", op, r.Path, path, b.Size, max, platform.ErrTooLarge)
	case !isHexOID(b.OID), b.StoredExternally != nil && *b.StoredExternally, b.RawTextBlob == nil:
		return platform.File{}, false, nil
	}
	content := []byte(*b.RawTextBlob)
	if int64(len(content)) != int64(b.Size) || blobID(content, len(b.OID)) != strings.ToLower(b.OID) {
		// GraphQL returns text as UTF-8: what does not hash to the blob is
		// read as it is.
		return platform.File{}, false, nil
	}
	return platform.File{Path: path, Mode: mode, OID: strings.ToLower(b.OID), Content: content}, true, nil
}

// gqlRequest and gqlResponse are a GraphQL request and its answer.
type (
	gqlRequest struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	gqlResponse struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
)

// graphql sends a GraphQL query of op, a read, to the instance's
// /api/graphql with the token as a bearer token (anonymously without one)
// and decodes its data into out. An answer with errors fails: a timeout
// as ClassTransient, anything else ClassInvalid (a field the instance
// lacks, a complexity over its limit).
func (c *client) graphql(ctx context.Context, op, query string, vars map[string]any, out any) error {
	u, ok := strings.CutSuffix(c.api, "/v4")
	if !ok {
		return &platform.Error{Op: op, Class: platform.ClassNotFound, Err: fmt.Errorf("no GraphQL endpoint next to %s", c.api)}
	}
	var resp gqlResponse
	_, err := c.http.JSON(throttle.AsRead(ctx), http.MethodPost, u+"/graphql", c.gqlAuth, gqlRequest{Query: query, Variables: vars}, &resp)
	if err != nil {
		return c.apiError(op, err)
	}
	if len(resp.Errors) > 0 {
		msg := resp.Errors[0].Message
		class := platform.ClassInvalid
		if strings.Contains(strings.ToLower(msg), "timeout") || strings.Contains(strings.ToLower(msg), "timed out") {
			class = platform.ClassTransient
		}
		return &platform.Error{Op: op, Class: class, Err: errors.New(c.mask("GraphQL: " + oneLine(msg)))}
	}
	if len(resp.Data) == 0 || string(resp.Data) == "null" {
		return shapeError(op, "GraphQL answered without data")
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return shapeError(op, "GraphQL data does not decode: %v", err)
	}
	return nil
}

var _ platform.BatchReader = (*reader)(nil)
