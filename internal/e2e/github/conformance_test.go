package githube2e

import (
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
)

// TestConformance runs the platform contract suite against the GitHub
// driver over the fake: as github.com, and as GitHub Enterprise Server
// with web commit signing off (its default).
func TestConformance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		opts   worldOptions
		flavor string
		heavy  bool
	}{
		{"github.com", worldOptions{flavor: ghfake.DotCom}, "github", false},
		{"ghes", worldOptions{flavor: ghfake.GHES}, "ghes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.heavy {
				heavy(t)
			}
			caps := probeCaps(t, tc.opts)
			if caps.Flavor != tc.flavor || !caps.CloserKnown || !caps.WorkflowPerm || caps.Draft != platform.DraftNative ||
				!caps.Commit.API || caps.Commit.SignedByPlatform != (tc.flavor != "ghes") {
				t.Errorf("Caps = %+v", caps)
			}
			conformance.Run(t, func(t *testing.T) conformance.Fixture { return newFixture(t, tc.opts) })
		})
	}
}

// probeCaps returns what the reader reports on a new world.
func probeCaps(t *testing.T, opts worldOptions) platform.Caps {
	t.Helper()
	w := newWorld(t, opts)
	r, _ := w.drivers()
	caps, err := r.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return caps
}
