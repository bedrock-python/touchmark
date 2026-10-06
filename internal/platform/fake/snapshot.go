package fake

import (
	"context"
	"fmt"
	"maps"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// Snapshots returns a snapshot.Source over the platform's trees: the tree
// of the default branch with Tree.Commit = Head. It serves the refs
// ReadFile serves; any other ref is ClassUnsupported. An empty repository
// is ClassNotFound wrapping platform.ErrNotFound. A remote with a URL must
// be the repository's (Reader.Remote or TargetWriter.Remote); an empty one
// is accepted. Calls are logged as "Snapshot <repo>" and take faults
// injected for "Snapshot". In git mode it reads the bare repository
// directly, at the refs ReadFile serves there; an unknown ref is
// ClassNotFound.
func (p *Platform) Snapshots() snapshot.Source { return source{p} }

type source struct{ p *Platform }

func (s source) Snapshot(ctx context.Context, repo platform.Repo, remote platform.Remote, ref string) (*snapshot.Tree, error) {
	op := ops["Snapshot"]
	return do(ctx, s.p, nil, "Snapshot", []string{repo.Path}, func() (*snapshot.Tree, error) {
		p := s.p
		st, err := p.repoOf(op, repo)
		if err != nil {
			return nil, err
		}
		if remote.URL != "" && remote.URL != p.remoteURL(st) {
			return nil, invalid(op, "remote %q is not the remote of %s", remote.URL, st.repo.Path)
		}
		if p.git != nil {
			return p.gitSnapshot(ctx, op, st, ref)
		}
		if err := checkRef(op, st, ref); err != nil {
			return nil, err
		}
		if len(st.entries) == 0 {
			return nil, &platform.Error{Op: op, Class: platform.ClassNotFound,
				Err: fmt.Errorf("%s is empty: %w", st.repo.Path, platform.ErrNotFound)}
		}
		return &snapshot.Tree{Commit: st.head(), Entries: maps.Clone(st.entries)}, nil
	})
}
