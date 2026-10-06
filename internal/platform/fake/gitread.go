package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// gitResolve returns the commit ref names in the bare repository of s: ""
// is the default branch, then a branch name, "refs/heads/<branch>" or a
// full commit id. An empty repository or an unknown ref is ClassNotFound.
// Called with mu held.
func (p *Platform) gitResolve(ctx context.Context, op string, s *repoState, ref string) (string, error) {
	name := ref
	if name == "" {
		name = s.repo.DefaultBranch
	}
	if tip := s.refs[strings.TrimPrefix(name, "refs/heads/")]; tip != "" {
		return tip, nil
	}
	if ref == "" {
		return "", notFound(op, "%s is empty", s.repo.Path)
	}
	if isHexID(ref) {
		id := strings.ToLower(ref)
		for _, tip := range s.refs {
			if tip == id {
				return id, nil
			}
		}
		ok, err := p.repoGit(s).isCommit(ctx, id)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
		}
		if ok {
			return id, nil
		}
	}
	return "", notFound(op, "%s has no branch or commit %q", s.repo.Path, ref)
}

// gitReadFile is ReadFile in git mode: path at ref in the bare repository
// of s, at most max bytes. The default branch's tree is mirrored in
// s.entries and blobs are cached by id, so reading it runs no git. Called
// with mu held.
func (p *Platform) gitReadFile(ctx context.Context, op string, s *repoState, ref, path string, max int64) (platform.File, error) {
	commit, err := p.gitResolve(ctx, op, s, ref)
	if err != nil {
		return platform.File{}, err
	}
	var entry snapshot.Entry
	found, dir := false, false
	if commit == s.refs[s.repo.DefaultBranch] {
		entry, found = s.entries[path]
		_, dir = s.child(path)
	} else {
		list, err := p.repoGit(s).g.LsTree(ctx, commit, path)
		if err != nil {
			return platform.File{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
		}
		for _, e := range list {
			if e.Path == path {
				entry, found = snapshot.Entry{Mode: e.Mode, OID: e.OID}, true
			} else {
				dir = true
			}
		}
	}
	switch {
	case !found && dir:
		return platform.File{}, fmt.Errorf("%s: %s: %s is a directory: %w", op, s.repo.Path, path, platform.ErrNotRegular)
	case !found:
		return platform.File{}, notFound(op, "%s: %s", s.repo.Path, path)
	case entry.Mode != ModeFile && entry.Mode != ModeExecutable:
		return platform.File{}, fmt.Errorf("%s: %s: %s has mode %s: %w", op, s.repo.Path, path, entry.Mode, platform.ErrNotRegular)
	}
	content, err := p.blob(ctx, s, entry.OID)
	if err != nil {
		return platform.File{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	if int64(len(content)) > max {
		return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w",
			op, s.repo.Path, path, len(content), max, platform.ErrTooLarge)
	}
	return platform.File{Path: path, Mode: entry.Mode, OID: entry.OID, Content: content}, nil
}

// blob returns a copy of the content of a blob of s, from the platform's
// content-addressed store, or read from git and kept there. Called with mu
// held.
func (p *Platform) blob(ctx context.Context, s *repoState, oid string) ([]byte, error) {
	if b, ok := p.blobs[oid]; ok {
		return slices.Clone(b), nil
	}
	b, err := p.repoGit(s).readBlob(ctx, oid)
	if err != nil {
		return nil, err
	}
	p.blobs[oid] = b
	return slices.Clone(b), nil
}

// gitSnapshot is Snapshot in git mode: the tree of ref in the bare
// repository of s (the mirror for the default branch). Called with mu
// held.
func (p *Platform) gitSnapshot(ctx context.Context, op string, s *repoState, ref string) (*snapshot.Tree, error) {
	commit, err := p.gitResolve(ctx, op, s, ref)
	if err != nil {
		return nil, err
	}
	if commit == s.refs[s.repo.DefaultBranch] {
		return &snapshot.Tree{Commit: commit, Entries: maps.Clone(s.entries)}, nil
	}
	entries, err := p.repoGit(s).entries(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	return &snapshot.Tree{Commit: commit, Entries: entries}, nil
}
