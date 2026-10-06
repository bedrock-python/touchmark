package decide

import (
	"slices"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Identity tells touchmark's own pull requests from everyone else's (see
// docs/concepts/delivery.md).
type Identity struct {
	// Branches are the sync branch first, then branch_aliases.
	Branches []string
	// Authors are the stable account ids of the writer and known_authors.
	Authors []string
	// Fingerprints are the hub fingerprint first, then
	// previous_fingerprints (full forms, e.g. "github.com/712345678").
	Fingerprints []string
}

// OwnStatus is the verdict of Identity.Own.
type OwnStatus uint8

const (
	// NotOurs: another author, another branch, a fork, or another hub: a
	// well-formed marker of another hub or of an unknown version makes even
	// our author's PR on our branch someone else's.
	NotOurs OwnStatus = iota
	// Ours: our branch, same repository, our author and a valid marker
	// with one of our fingerprints.
	Ours
	// OursMarkerInvalid: our branch, same repository and our author, but
	// no marker line at all, or one whose fp attribute is ours that fails to
	// parse or whose data names another fingerprint (a marker of ours that
	// was erased, broken or tampered with). The caller reports
	// blocked:marker-invalid and never writes to the PR.
	OursMarkerInvalid
)

// String returns the verdict name for messages and tests.
func (s OwnStatus) String() string {
	switch s {
	case NotOurs:
		return "not-ours"
	case Ours:
		return "ours"
	case OursMarkerInvalid:
		return "ours-marker-invalid"
	}
	return "unknown"
}

// Own applies the rule. A PR is ours only when ALL hold:
//   - pr.Head is one of Branches;
//   - pr.HeadRepoID == pr.RepoID (never a fork);
//   - pr.Author.ID is one of Authors (by stable id, never by login);
//   - marker.Find(pr.Body, Fingerprints) returns Found.
//
// With the first three but not the fourth it returns OursMarkerInvalid when
// Find reports None or Invalid, and NotOurs when it reports Foreign: markers
// are there and none has our fp attribute (another hub's, possibly with the
// same id and writer, or another version's). Such a PR is someone else's
// (blocked:branch-in-use on the branch touchmark would push to), never one
// whose branch a recreate may rebuild. The hub id (slug) plays no part:
// after an id change the fingerprint still matches. An empty head,
// repository id or author id never matches, so a PR whose platform did not
// report them (a deleted fork, a ghost author) is not ours. The marker is
// returned only with Ours.
func (id Identity) Own(pr platform.PR) (marker.Marker, OwnStatus) {
	switch {
	case pr.Head == "" || !slices.Contains(id.Branches, pr.Head):
		return marker.Marker{}, NotOurs
	case pr.RepoID == "" || pr.HeadRepoID != pr.RepoID:
		return marker.Marker{}, NotOurs
	case pr.Author.ID == "" || !slices.Contains(id.Authors, pr.Author.ID):
		return marker.Marker{}, NotOurs
	}
	m, status := marker.Find(pr.Body, id.Fingerprints)
	switch status {
	case marker.Found:
		return m, Ours
	case marker.Foreign:
		return marker.Marker{}, NotOurs
	}
	return marker.Marker{}, OursMarkerInvalid
}
