package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// GitSource takes snapshots through git: one isolated bare repository per
// target under Dir, a blobless fetch of the default branch with depth 1, and
// ls-tree. It never checks anything out.
//
// A GitSource is safe for concurrent use: targets are fetched in parallel,
// and calls for one target share its repository (its fetches are
// serialized). The zero value with Dir and Isolation set is ready.
//
// A target's repository lives from the first Snapshot or Repo of the
// target to its Release, which the pipeline calls once it is done with the
// target and which removes the target's directory; a later call for the
// target starts a new repository.
type GitSource struct {
	// Dir holds one repository per target (created on demand, reused within
	// a run, removed by Release); the caller removes Dir itself.
	Dir string
	// Isolation is passed to gitx.InitTarget.
	Isolation gitx.Isolation

	mu    sync.Mutex
	repos map[string]*gitRepo
}

// gitRepo is the repository of one target; mu is held while it is
// initialized or released. A released gitRepo is out of the map: whoever
// finds it looks again.
type gitRepo struct {
	mu       sync.Mutex
	repo     *gitx.TargetRepo
	released bool
}

// snapshotOp names the operation in errors.
const snapshotOp = "snapshot"

// Snapshot implements Source. ref "" is the repository's default branch.
// The returned Tree has Commit set to the fetched head and every non-tree
// entry. An empty repository (no such branch) is an error.
//
// ref may also be a branch name, "refs/heads/<branch>", or the full id of
// a commit already fetched into the target's repository (a snapshot's
// Commit): a branch is fetched with depth 1 and without blobs
// (FetchBranch), a commit is read as it is. A branch the remote does not
// have is ClassNotFound wrapping platform.ErrNotFound, and so is a commit
// that is not present. A fetch whose credential the remote refused is
// ClassAuth, one the repository refused to the identity (HTTP 403)
// ClassPermission, HTTP 429 ClassRateLimited, a network failure or a fetch
// that ran into its own time bound ClassTransient (gitx.ClassifyFailure);
// other git failures are unclassified (the caller reports failed:git). The
// end of ctx is left as it is (platform.ClassOf reads a deadline as
// transient): the caller tells its own deadline or cancellation by ctx.
func (s *GitSource) Snapshot(ctx context.Context, repo platform.Repo, remote platform.Remote, ref string) (*Tree, error) {
	t, err := s.Repo(ctx, repo, remote)
	if err != nil {
		return nil, err
	}
	commit, err := s.head(ctx, t, repo, ref)
	if err != nil {
		return nil, err
	}
	entries, err := t.Tree(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", repo.Path, err)
	}
	tree := &Tree{Commit: commit, Entries: make(map[string]Entry, len(entries))}
	for _, e := range entries {
		tree.Entries[e.Path] = Entry{Mode: e.Mode, OID: e.OID}
	}
	return tree, nil
}

// head returns the commit ref names, fetching a branch.
func (s *GitSource) head(ctx context.Context, t *gitx.TargetRepo, repo platform.Repo, ref string) (string, error) {
	if isCommitID(ref) {
		c, err := t.Commit(ctx, ref)
		switch {
		case errors.Is(err, gitx.ErrNotFound):
			return "", notFound("commit %s of %s was not fetched: %w", ref, repo.Path, platform.ErrNotFound)
		case err != nil:
			return "", fmt.Errorf("snapshot %s: %w", repo.Path, err)
		}
		return c.SHA, nil
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if ref == "" {
		branch = repo.DefaultBranch
	}
	if branch == "" {
		return "", fmt.Errorf("snapshot %s: the default branch is unknown", repo.Path)
	}
	sha, ok, err := t.FetchBranch(ctx, branch, 1)
	switch {
	case err != nil:
		return "", classify(repo, err)
	case !ok:
		return "", notFound("%s has no branch %s: %w", repo.Path, branch, platform.ErrNotFound)
	}
	return sha, nil
}

// notFound is a ClassNotFound platform error.
func notFound(format string, args ...any) error {
	return &platform.Error{Op: snapshotOp, Class: platform.ClassNotFound, Err: fmt.Errorf(format, args...)}
}

// classify gives a failed fetch the class distribute reports it by: a
// refused credential is failed:auth, a refused identity failed:access, HTTP
// 429 deferred:rate-limit, a network failure failed:transient; anything
// else stays unclassified (failed:git).
func classify(repo platform.Repo, err error) error {
	class := platform.ClassUnknown
	switch gitx.ClassifyFailure(err) {
	case gitx.FailureAuth:
		class = platform.ClassAuth
	case gitx.FailurePermission:
		class = platform.ClassPermission
	case gitx.FailureRateLimited:
		class = platform.ClassRateLimited
	case gitx.FailureTransient:
		class = platform.ClassTransient
	}
	if class == platform.ClassUnknown {
		return fmt.Errorf("snapshot %s: %w", repo.Path, err)
	}
	return &platform.Error{Op: snapshotOp + " " + repo.Path, Class: class, Err: err}
}

// Repo returns the target's isolated repository (initialized on first use),
// for the delivery steps that follow the snapshot (history, commit, push).
//
// The repository is keyed by (Host ignoring case, ID) and lives in a
// directory of Dir named after a hash of that key. It is created with
// remote's URL; a later call must name the same URL. The result sends
// remote's Header (gitx.TargetRepo.WithAuth), so the push step passes the
// per-target write remote and gets the same repository with that
// credential.
func (s *GitSource) Repo(ctx context.Context, repo platform.Repo, remote platform.Remote) (*gitx.TargetRepo, error) {
	if s.Dir == "" {
		return nil, errors.New("snapshot: GitSource.Dir is not set")
	}
	if repo.Host == "" || repo.ID == "" {
		return nil, fmt.Errorf("snapshot %s: the repository has no host or id", repo.Path)
	}
	key := strings.ToLower(repo.Host) + "\x00" + repo.ID
	e := s.entry(key)
	defer e.mu.Unlock()
	auth := gitx.Auth{Header: remote.Header}
	if e.repo == nil {
		t, err := gitx.InitTarget(ctx, filepath.Join(s.Dir, repoDir(key)), remote.URL, auth, s.Isolation)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", repo.Path, err)
		}
		e.repo = t
		return t, nil
	}
	if remote.URL != e.repo.Remote {
		return nil, fmt.Errorf("snapshot %s: the remote URL changed during the run", repo.Path)
	}
	return e.repo.WithAuth(auth), nil
}

// entry returns the live entry of key, locked, making it when there is
// none.
func (s *GitSource) entry(key string) *gitRepo {
	for {
		s.mu.Lock()
		if s.repos == nil {
			s.repos = map[string]*gitRepo{}
		}
		e := s.repos[key]
		if e == nil {
			e = &gitRepo{}
			s.repos[key] = e
		}
		s.mu.Unlock()
		e.mu.Lock()
		if !e.released {
			return e
		}
		e.mu.Unlock()
	}
}

// Release forgets the repository of repo (by host, ignoring case, and id)
// and removes its directory. It waits for a Repo or Snapshot of the target
// in progress; the caller must not use a TargetRepo of the target any more.
// A Repo or Snapshot of the target after Release (or waiting for it) starts
// a new repository once the old one is gone. Releasing a target without a
// repository does nothing.
func (s *GitSource) Release(repo platform.Repo) error {
	if s.Dir == "" || repo.Host == "" || repo.ID == "" {
		return nil
	}
	key := strings.ToLower(repo.Host) + "\x00" + repo.ID
	s.mu.Lock()
	e := s.repos[key]
	s.mu.Unlock()
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.released {
		return nil
	}
	// The directory goes first, while the entry keeps others waiting: no new
	// repository may start in it before it is gone.
	e.repo = nil
	err := os.RemoveAll(filepath.Join(s.Dir, repoDir(key)))
	e.released = true
	s.mu.Lock()
	if s.repos[key] == e {
		delete(s.repos, key)
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("snapshot %s: remove the repository: %w", repo.Path, err)
	}
	return nil
}

// repoDir names the directory of a target's repository: 24 hex digits of
// sha256 of the key, which neither collides nor needs escaping (hosts carry
// ports, ids are any string, and names such as "nul" are devices on
// Windows).
func repoDir(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// isCommitID reports whether ref is a full lowercase hex object id.
func isCommitID(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		if c := ref[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
