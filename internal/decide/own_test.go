package decide

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The hub of the own-PR tests: its fingerprint, the one before it moved,
// another hub's, its writer and a known author, and its branches.
const (
	ownFP     = "github.com/712345678"
	ownPrevFP = "gitlab.example.com/1234"
	ownOther  = "github.com/999"
	writerID  = "55501017"
	knownID   = "12345"
	ownBranch = "touchmark/acme-eng"
	ownAlias  = "touchmark/old-id"
)

// markerChanges converts D to the short form a marker keeps.
func markerChanges(pairs []Pair) []marker.Change {
	var out []marker.Change
	for _, p := range pairs {
		c := marker.Change{Path: p.Path, From: Short(p.From, marker.ShortOID), Mode: p.Mode, To: Short(p.To, marker.ShortOID)}
		if p.Mode == ModeDelete {
			c.Mode = ""
		}
		out = append(out, c)
	}
	return out
}

// ownMarker encodes a marker of hub id with the fingerprint fp over
// mixedPairs in stream.
func ownMarker(t *testing.T, stream, hub, fp string) string {
	t.Helper()
	d := mixedPairs()
	line, err := marker.Encode(marker.Marker{
		Key: Key(stream, d),
		Data: marker.Data{
			V: marker.Version, Stream: stream, Hub: hub, FP: fp,
			DecidedAt: oidA, ContentCommit: oidA, Base: oidB, Engine: "0.2.0",
			Packs: []string{"agents"}, Changes: markerChanges(d), ChangesComplete: true,
			TitleSet: "chore: sync engineering assets", LabelsSet: []string{"engineering-assets"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestOwn(t *testing.T) {
	id := Identity{
		Branches:     []string{ownBranch, ownAlias},
		Authors:      []string{writerID, knownID},
		Fingerprints: []string{ownFP, ownPrevFP},
	}
	ours := ownMarker(t, StreamSync, "acme-eng", ownFP)
	foreign := ownMarker(t, StreamSync, "acme-eng", ownOther) // another hub that chose the same id
	tampered := strings.Replace(foreign, "fp="+marker.FP16(ownOther), "fp="+marker.FP16(ownFP), 1)
	pr := func() platform.PR {
		return platform.PR{
			Number: 7, State: platform.Open, Head: ownBranch, Base: "main",
			RepoID: "100", HeadRepoID: "100", BaseExists: true,
			Author: platform.Account{ID: writerID, Login: "acme-assets-write[bot]", Kind: platform.KindBot},
			Body:   "## Engineering assets\n\nSome text.\n\n" + ours,
		}
	}
	cases := []struct {
		name string
		edit func(*platform.PR)
		want OwnStatus
		fp   string // Data.FP of the marker returned with Ours
	}{
		{"ours", func(*platform.PR) {}, Ours, ownFP},
		{"ours on an alias branch", func(p *platform.PR) { p.Head = ownAlias }, Ours, ownFP},
		{"ours by a known author", func(p *platform.PR) { p.Author.ID = knownID }, Ours, ownFP},
		{"ours, closed", func(p *platform.PR) { p.State = platform.Closed }, Ours, ownFP},
		{"ours, CRLF body", func(p *platform.PR) { p.Body = strings.ReplaceAll(p.Body, "\n", "\r\n") + "\r\n" }, Ours, ownFP},
		{"previous fingerprint", func(p *platform.PR) { p.Body = ownMarker(t, StreamSync, "acme-eng", ownPrevFP) }, Ours, ownPrevFP},
		{"after a hub id change", func(p *platform.PR) {
			p.Head, p.Body = ownAlias, "text\n"+ownMarker(t, StreamSync, "old-id", ownFP)
		}, Ours, ownFP},
		{"adopt stream marker", func(p *platform.PR) { p.Body = ownMarker(t, StreamAdopt, "acme-eng", ownFP) }, Ours, ownFP},

		{"another branch", func(p *platform.PR) { p.Head = "feature/x" }, NotOurs, ""},
		{"a branch that extends ours", func(p *platform.PR) { p.Head = ownBranch + "-2" }, NotOurs, ""},
		{"another hub's sync branch", func(p *platform.PR) { p.Head = "touchmark/other" }, NotOurs, ""},
		{"branch case differs", func(p *platform.PR) { p.Head = "Touchmark/acme-eng" }, NotOurs, ""},
		{"fork", func(p *platform.PR) { p.HeadRepoID = "200" }, NotOurs, ""},
		// A fork PR from the same branch name with a byte-for-byte copy of a
		// valid marker, even from an account id we know, is never ours.
		{"fork with a copied marker", func(p *platform.PR) { p.HeadRepoID, p.Author = "200", platform.Account{ID: "31337", Login: "mallory"} }, NotOurs, ""},
		{"fork by our author id", func(p *platform.PR) { p.HeadRepoID = "200" }, NotOurs, ""},
		{"deleted fork", func(p *platform.PR) { p.HeadRepoID = "" }, NotOurs, ""},
		{"repository ids unknown", func(p *platform.PR) { p.RepoID, p.HeadRepoID = "", "" }, NotOurs, ""},
		{"another author", func(p *platform.PR) { p.Author = platform.Account{ID: "999", Login: "someone"} }, NotOurs, ""},
		{"same login, another id", func(p *platform.PR) { p.Author.ID = "31337" }, NotOurs, ""},
		{"author id unknown", func(p *platform.PR) { p.Author.ID = "" }, NotOurs, ""},
		{"another branch, broken marker", func(p *platform.PR) { p.Head, p.Body = "feature/x", "text" }, NotOurs, ""},
		// A well-formed marker of another hub (two hubs sharing an id and a
		// writer, threat T6 of docs/project/threat-model.md) or of an unknown
		// version is someone else's pull request: recreate must never take it
		// over.
		{"marker of another hub", func(p *platform.PR) { p.Body = foreign }, NotOurs, ""},
		{"marker of another version", func(p *platform.PR) { p.Body = strings.Replace(ours, "touchmark:v1 ", "touchmark:v2 ", 1) }, NotOurs, ""},
		{"markers of another hub and version", func(p *platform.PR) {
			p.Body = foreign + "\n" + strings.Replace(ours, "touchmark:v1 ", "touchmark:v2 ", 1)
		}, NotOurs, ""},

		{"marker missing", func(p *platform.PR) { p.Body = "text" }, OursMarkerInvalid, ""},
		{"marker erased", func(p *platform.PR) { p.Body = "" }, OursMarkerInvalid, ""},
		{"marker broken", func(p *platform.PR) { p.Body = ours[:len(ours)-12] + " -->" }, OursMarkerInvalid, ""},
		{"marker tampered", func(p *platform.PR) { p.Body = tampered }, OursMarkerInvalid, ""},
		{"marker tampered after ours", func(p *platform.PR) { p.Body = ours + "\n" + tampered }, OursMarkerInvalid, ""},
		{"marker tampered after another hub's", func(p *platform.PR) { p.Body = foreign + "\n" + tampered }, OursMarkerInvalid, ""},
		{"marker not on its own line", func(p *platform.PR) { p.Body = "see " + ours }, OursMarkerInvalid, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := pr()
			c.edit(&p)
			m, status := id.Own(p)
			if status != c.want {
				t.Fatalf("Own = %v, want %v", status, c.want)
			}
			if c.want != Ours {
				if !reflect.DeepEqual(m, marker.Marker{}) {
					t.Errorf("a marker came with %v", status)
				}
				return
			}
			if m.Data.FP != c.fp {
				t.Errorf("marker fp = %q, want %q", m.Data.FP, c.fp)
			}
			if m.Key != Key(m.Stream, mixedPairs()) {
				t.Errorf("marker key = %s, want the key of D", m.Key)
			}
		})
	}
}

// TestOwnEmptyValuesNeverMatch: an identity that lists an empty branch or
// author (a lookup that failed) must not claim PRs whose platform reported
// nothing.
func TestOwnEmptyValuesNeverMatch(t *testing.T) {
	id := Identity{Branches: []string{""}, Authors: []string{""}, Fingerprints: []string{ownFP}}
	p := platform.PR{RepoID: "1", HeadRepoID: "1", Body: ownMarker(t, StreamSync, "acme-eng", ownFP)}
	if _, status := id.Own(p); status != NotOurs {
		t.Errorf("Own = %v, want not-ours", status)
	}
	// The head matches a listed branch: only the empty author id is left to
	// refuse the pull request.
	p.Head = "x"
	id.Branches = []string{"x"}
	if _, status := id.Own(p); status != NotOurs {
		t.Errorf("Own = %v, want not-ours for an empty author id", status)
	}
	p.Author.ID = "1"
	id.Authors = []string{"1"}
	if _, status := id.Own(p); status != Ours {
		t.Errorf("Own = %v, want ours once values match", status)
	}
}

// TestOwnWithoutFingerprints: with no fingerprint of ours, every marker is
// someone else's, and a body without one is still a broken marker of ours.
func TestOwnWithoutFingerprints(t *testing.T) {
	id := Identity{Branches: []string{ownBranch}, Authors: []string{writerID}}
	p := platform.PR{Head: ownBranch, RepoID: "1", HeadRepoID: "1", Author: platform.Account{ID: writerID}, Body: ownMarker(t, StreamSync, "acme-eng", ownFP)}
	if _, status := id.Own(p); status != NotOurs {
		t.Errorf("Own = %v, want not-ours: no fingerprint is ours", status)
	}
	p.Body = "no marker"
	if _, status := id.Own(p); status != OursMarkerInvalid {
		t.Errorf("Own = %v, want ours-marker-invalid for a body without a marker", status)
	}
}

func TestOwnStatusString(t *testing.T) {
	for s, want := range map[OwnStatus]string{NotOurs: "not-ours", Ours: "ours", OursMarkerInvalid: "ours-marker-invalid", 9: "unknown"} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", s, got, want)
		}
	}
}
