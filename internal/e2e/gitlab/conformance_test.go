//go:build e2e

package gitlabe2e

import (
	"context"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/platform/gitlab"
)

// TestConformance runs the platform contract suite against the gitlab
// driver on the live GitLab, one fresh subgroup per subtest (see fixture).
func TestConformance(t *testing.T) {
	e := needLive(t)
	caps := probeCaps(t, e)
	finding(t, "driver-caps", "flavor %s, version %q, draft %v %q, quick actions %v, labels by id %v, closer known %v, max body %d, runtime-only %v",
		caps.Flavor, caps.Version, caps.Draft, caps.DraftPrefix, caps.QuickActions, caps.LabelsByID, caps.CloserKnown, caps.MaxBody, caps.RuntimeOnly)
	if caps.Flavor != "gitlab" {
		t.Errorf("the driver detects %q on %s", caps.Flavor, e.Image)
	}
	if !caps.QuickActions || caps.Draft != platform.DraftTitlePrefix || caps.LabelsByID {
		t.Errorf("Caps: quick actions %v, draft %v %q, labels by id %v; GitLab runs quick actions, drafts by the \"Draft: \" prefix and takes labels by name",
			caps.QuickActions, caps.Draft, caps.DraftPrefix, caps.LabelsByID)
	}
	conformance.Run(t, func(t *testing.T) conformance.Fixture { return newFixture(t, e) })
	accountCache.Lock()
	kinds := accountCache.byRole
	accountCache.Unlock()
	if kinds != nil {
		finding(t, "account-kinds", "the driver reports the reader (%s) as kind %d, the writer as %d and the person as %d (1 user, 2 bot, 3 service account)",
			e.Accounts, kinds[conformance.RoleReader].Kind, kinds[conformance.RoleWriter].Kind, kinds[conformance.RoleOther].Kind)
	}
}

// probeCaps returns what the reader driver's Probe reports.
func probeCaps(t *testing.T, e *liveEnv) platform.Caps {
	t.Helper()
	r, _ := newDrivers(t, e)
	caps, err := r.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return caps
}

// TestAdminTokenRefused: the driver refuses an administrator's token, which
// could act as anyone through Sudo. The token itself works:
// Probe reads the instance with it. Self refuses it, and so does the
// writer's Target, before it reads or writes anything of the project.
func TestAdminTokenRefused(t *testing.T) {
	e := needLive(t)
	rp := e.provider(t)
	ctx := context.Background()
	r := construct(t, "NewReader", func() (platform.Reader, error) { return gitlab.NewReader(rp, credential(e.Root), e.HTTP) })
	if _, err := r.Probe(ctx); err != nil {
		t.Fatalf("Probe with an administrator's token: %v; the token works, Self must be what refuses it", err)
	}
	_, err := r.Self(ctx)
	checkAdminRefused(t, e, "Self", err)
	w := construct(t, "NewWriter", func() (platform.Writer, error) { return gitlab.NewWriter(rp, credential(e.Root), e.HTTP) })
	_, err = w.Target(ctx, platform.Repo{Host: e.Host, ID: "1", Path: e.Group + "/any"}, platform.Perms{Contents: true, PRs: true})
	checkAdminRefused(t, e, "Target", err)
	finding(t, "admin-token", "an administrator's token: Probe works, Self and Target refuse it: %v", err)
}

// checkAdminRefused checks that err is the driver's refusal of an
// administrator's token: ClassInvalid, naming Sudo, without the token.
func checkAdminRefused(t *testing.T, e *liveEnv, what string, err error) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s accepted an administrator's token", what)
	case platform.ClassOf(err) != platform.ClassInvalid || !strings.Contains(err.Error(), "Sudo"):
		t.Errorf("%s with an administrator's token: %v (class %v), want a refusal (invalid, naming Sudo)",
			what, err, platform.ClassOf(err))
	case e.Redact.Contains(err.Error()):
		t.Errorf("%s: the refusal shows a token", what)
	}
}
