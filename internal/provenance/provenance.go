// Package provenance builds the ownership manifest from a hub's git history
// and reads the packs the hub ships now.
//
// The manifest answers one question: which blob versions has pack P ever
// shipped at repository path X? It is built from committed history only, so
// uncommitted edits and checkout filters on the hub never affect it.
package provenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

// PacksDir is the hub directory that holds packs: packs/<pack>/<path>.
const PacksDir = "packs"

// MinEvidenceSize is the smallest blob that counts as evidence of ownership.
// Near-empty content ("{}", a .gitkeep) is too likely to coincide with a file
// the target wrote on its own. Equality with the current pack version is
// exempt: writing identical bytes is a no-op either way.
const MinEvidenceSize = 64

// ManifestVersion is the version of the JSON manifest format.
const ManifestVersion = 1

// ErrShallow is returned when the hub checkout is shallow: history would be
// incomplete and managed files would silently look local.
var ErrShallow = errors.New("hub checkout is shallow: full history is required (fetch-depth: 0, GIT_DEPTH: \"0\")")

// ErrPartialClone is returned when the hub checkout is a partial clone
// (`clone --filter`): sizing every historical blob would fetch each missing
// one on its own, and fail offline.
var ErrPartialClone = errors.New("hub checkout is a partial clone: clone it without --filter")

// Version is one blob a pack shipped at a path.
type Version struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// Manifest records every version each pack ever shipped at each path.
//
// Pack names are the raw directory names found in history. Renames declared
// with `formerly` in hub.yml are applied by the caller (see Aliases), not
// baked into the manifest.
type Manifest struct {
	Version   int    `json:"version"`
	HubCommit string `json:"hub_commit"`
	// Paths maps a repository path to pack name to versions, in the order
	// they were first seen. Versions are unique per (path, pack). Build
	// lists them oldest first: in the order the hub's history first shows
	// them.
	Paths map[string]map[string][]Version `json:"paths"`
}

// Versions returns what pack shipped at path, or nil.
func (m *Manifest) Versions(path, pack string) []Version {
	if m == nil || m.Paths == nil {
		return nil
	}
	return m.Paths[path][pack]
}

// Add records a version. It is idempotent.
func (m *Manifest) Add(path, pack string, v Version) {
	if m.Paths == nil {
		m.Paths = map[string]map[string][]Version{}
	}
	byPack := m.Paths[path]
	if byPack == nil {
		byPack = map[string][]Version{}
		m.Paths[path] = byPack
	}
	for _, have := range byPack[pack] {
		if have.OID == v.OID {
			return
		}
	}
	byPack[pack] = append(byPack[pack], v)
}

// File is one file a pack ships now.
type File struct {
	Pack string
	Path string // repository path inside the target (packs/<pack>/ stripped)
	OID  string
	Size int64
	Mode string // "100644" or "100755"
}

// Current maps pack name to repository path to the file the pack ships now.
type Current map[string]map[string]File

// Source selects where the current pack contents come from.
type Source int

const (
	// Committed reads packs from the hub's commit (Hub.Rev, HEAD by
	// default). This is the default: what ships is what is committed.
	Committed Source = iota
	// WorkTree reads packs from the hub's working tree, for debugging packs
	// locally before committing. Blob OIDs are the ones git would commit:
	// the hub's clean filters and attributes applied.
	WorkTree
)

// Hub is a hub checkout.
type Hub struct {
	// Dir is the root of the hub's work tree. WorkTree reads Dir/packs.
	Dir string
	// Git runs git in the hub; gitx.New(Dir) when nil.
	Git *gitx.Git
	// Rev is the commit Build reads history from and ReadCurrent(Committed)
	// lists packs at; "HEAD" when empty. Pass a resolved commit id so that
	// every read sees the same commit even if HEAD moves meanwhile.
	Rev string
}

// rev returns the revision to read.
func (h *Hub) rev() string {
	if h.Rev != "" {
		return h.Rev
	}
	return "HEAD"
}

// git returns the Git that runs in the hub.
func (h *Hub) git() *gitx.Git {
	if h.Git != nil {
		return h.Git
	}
	return gitx.New(h.Dir)
}

var errNilHub = errors.New("provenance: nil hub")

// Build walks the hub's full history below packs/ and returns the manifest.
//
// It fails with ErrShallow on a shallow clone and ErrPartialClone on a
// partial one. It records every blob version that appeared at
// packs/<pack>/<path> in any commit reachable from Rev (HEAD by default),
// including side branches and merge results. Entries whose pack name or path
// fails validation are skipped (old commits may predate today's rules) and
// reported through skipped.
//
// HubCommit is Rev resolved once; history is read from that commit. The
// whole history is read by one git log and all sizes by one git cat-file,
// whatever the number of commits. A hub without packs/ yields an empty
// manifest. skipped holds hub paths ("packs/<pack>/<path>", and files
// directly under packs/), deduplicated and sorted; they are raw bytes from
// history, so quote them before printing.
func Build(ctx context.Context, h *Hub) (m *Manifest, skipped []string, err error) {
	if h == nil {
		return nil, nil, errNilHub
	}
	g := h.git()
	shallow, err := g.IsShallow(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("build manifest: %w", err)
	}
	if shallow {
		return nil, nil, ErrShallow
	}
	partial, err := g.IsPartialClone(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("build manifest: %w", err)
	}
	if partial {
		return nil, nil, ErrPartialClone
	}
	head, err := g.RevParse(ctx, h.rev())
	if err != nil {
		return nil, nil, fmt.Errorf("build manifest: resolve %s: %w", h.rev(), err)
	}
	scan, err := scanHistory(ctx, g, head)
	if err != nil {
		return nil, nil, fmt.Errorf("build manifest: %w", err)
	}
	m, err = scan.manifest(ctx, g, head)
	if err != nil {
		return nil, nil, fmt.Errorf("build manifest: %w", err)
	}
	return m, sortedKeys(scan.skipped), nil
}

// ReadCurrent returns the files every pack ships now, from src.
//
// Symlinks and submodules inside packs are returned as problems, not files;
// `check` turns them into errors. Paths that fail pathx.Validate are problems
// too.
//
// Committed lists packs/ at Rev (HEAD by default) with one git ls-tree;
// modes 100644 and 100755 are files. WorkTree walks Dir/packs without
// following symlinks and reads every regular file, skipping nothing: ignored
// and untracked files count. When Dir is the root of a git work tree, its
// ids and sizes are those git would commit, from one
// `git hash-object -w --stdin-paths` into a scratch object directory that is
// removed afterwards (the hub's own store is not touched), so a CRLF
// checkout of LF blobs reads the same as the commit; outside git they are
// the raw bytes'. Its modes come from the executable bits, or from the hub's
// index where git ignores those (Windows, core.fileMode=false; untracked
// files are then 100644). Its ids are sha1, like the ids targets compute.
//
// Invalid pack names are reported once per pack, entries directly under
// packs/ (or a packs that is not a directory) as problems without a pack,
// and a directory whose path is invalid once, without descending into it.
// A hub without packs/ has no packs. Problems are sorted by pack, path and
// message.
func ReadCurrent(ctx context.Context, h *Hub, src Source) (cur Current, problems []Problem, err error) {
	if h == nil {
		return nil, nil, errNilHub
	}
	switch src {
	case Committed:
		return readCommitted(ctx, h.git(), h.rev())
	case WorkTree:
		return readWorkTree(ctx, h)
	default:
		return nil, nil, fmt.Errorf("read packs: unknown source %d", src)
	}
}

// Problem is a pack defect found while reading or checking packs.
type Problem struct {
	Pack    string // pack name, or "" for problems outside any pack
	Path    string // repository path, or "" for pack-level problems
	Message string
}

// String returns the problem prefixed with its hub path
// ("packs/<pack>/<path>: message"), quoted when it holds characters that do
// not print cleanly. Problems outside any pack carry their location in
// Message.
func (p Problem) String() string {
	if p.Pack == "" {
		return p.Message
	}
	return display(hubPath(p.Pack, p.Path)) + ": " + p.Message
}

// Aliases maps a current pack name to the old names whose history counts as
// its own (hub.yml packs.<name>.formerly).
type Aliases map[string][]string

// Expand returns packs followed by all their aliases, without duplicates,
// preserving order.
func (a Aliases) Expand(packs []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range packs {
		add(p)
	}
	for _, p := range packs {
		for _, old := range a[p] {
			add(old)
		}
	}
	return out
}
