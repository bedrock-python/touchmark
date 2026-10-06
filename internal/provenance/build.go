package provenance

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
)

// shipment is one blob a pack shipped at a repository path.
type shipment struct {
	pack, path, oid string
}

// historyScan is what one pass over the hub's history found.
type historyScan struct {
	// last maps each shipment to the position of its last report in git
	// log's output. git log lists newer commits first, so the highest
	// position marks the oldest commit that shows the shipment.
	last map[shipment]int
	// skipped holds hub paths that are not valid pack files.
	skipped map[string]bool
}

// scanHistory runs one git log over packs/ in the history of rev and sorts
// every reported blob into shipments and skipped paths.
func scanHistory(ctx context.Context, g *gitx.Git, rev string) (*historyScan, error) {
	s := &historyScan{last: map[shipment]int{}, skipped: map[string]bool{}}
	// Pack names repeat on every entry; validate each one once.
	packOK := map[string]bool{}
	n := 0
	err := g.History(ctx, rev, PacksDir, func(v gitx.BlobVersion) error {
		n++
		pack, path, ok := splitHubPath(v.Path)
		if ok {
			valid, seen := packOK[pack]
			if !seen {
				valid = checkPackName(pack) == nil
				packOK[pack] = valid
			}
			ok = valid && pathx.Validate(path) == nil
		}
		if !ok {
			s.skipped[v.Path] = true
			return nil
		}
		s.last[shipment{pack: pack, path: path, oid: v.OID}] = n
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read hub history: %w", err)
	}
	return s, nil
}

// oldestFirst returns the shipments in the order history first shows them:
// the reverse of git log's newest-first order.
func (s *historyScan) oldestFirst() []shipment {
	out := slices.Collect(maps.Keys(s.last))
	slices.SortFunc(out, func(a, b shipment) int { return cmp.Compare(s.last[b], s.last[a]) })
	return out
}

// oids returns the distinct blob ids of all shipments, sorted.
func (s *historyScan) oids() []string {
	set := map[string]bool{}
	for sh := range s.last {
		set[sh.oid] = true
	}
	return sortedKeys(set)
}

// manifest sizes every blob with one git call and assembles the manifest.
func (s *historyScan) manifest(ctx context.Context, g *gitx.Git, head string) (*Manifest, error) {
	sizes, err := g.Sizes(ctx, s.oids())
	if err != nil {
		return nil, fmt.Errorf("read blob sizes: %w", err)
	}
	m := &Manifest{Version: ManifestVersion, HubCommit: head, Paths: map[string]map[string][]Version{}}
	for _, sh := range s.oldestFirst() {
		m.Add(sh.path, sh.pack, Version{OID: sh.oid, Size: sizes[sh.oid]})
	}
	return m, nil
}
