//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/platform/gitea"
)

// TestConformance runs the platform contract suite against the gitea driver
// on the live forge, one fresh organisation per subtest (see fixture).
func TestConformance(t *testing.T) {
	e := needLive(t)
	caps := probeCaps(t, e)
	finding(t, "driver-caps", "flavor %s, version %q, draft %v %q, labels by id %v, closer known %v, max body %d",
		caps.Flavor, caps.Version, caps.Draft, caps.DraftPrefix, caps.LabelsByID, caps.CloserKnown, caps.MaxBody)
	if caps.Flavor != e.Flavor {
		t.Errorf("the driver detects %q, the harness started %q (%s)", caps.Flavor, e.Flavor, e.Image)
	}
	conformance.Run(t, func(t *testing.T) conformance.Fixture { return newFixture(t, e) })
	accountCache.Lock()
	kinds := accountCache.byRole
	accountCache.Unlock()
	if kinds != nil {
		finding(t, "account-kinds", "the driver reports the reader as kind %d, the writer as %d and the person as %d (1 user, 2 bot; the API has no account type)",
			kinds[conformance.RoleReader].Kind, kinds[conformance.RoleWriter].Kind, kinds[conformance.RoleOther].Kind)
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

// TestSHA256Repo: the driver reports a repository of the sha256 object format
// as such, by Repo and by Resolve (object_format_name), which distribute
// skips (skipped:sha256), and reads its files with their 64-digit blob ids.
func TestSHA256Repo(t *testing.T) {
	e := needLive(t)
	fx := newOrg(t, e, "sha")
	resp := fx.admin().do(t, http.MethodPost, "/orgs/"+fx.org+"/repos", map[string]any{
		"name": "sha256", "private": true, "auto_init": true, "default_branch": "main", "object_format_name": "sha256",
	})
	if resp.Status/100 != 2 {
		t.Fatalf("create a sha256 repository: HTTP %d: %s", resp.Status, e.snippet(resp.Body))
	}
	var raw apiRepo
	fx.admin().get(t, "/repos/"+fx.org+"/sha256", &raw)
	r, _ := newDrivers(t, e)
	ctx := context.Background()
	repo, err := r.Repo(ctx, raw.FullName)
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	res, err := r.Resolve(ctx, platform.Selector{Namespace: fx.org})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var listed platform.Repo
	for _, x := range res.Repos {
		if x.ID == repo.ID {
			listed = x
		}
	}
	f, err := r.ReadFile(ctx, repo, "", "README.md", 64<<10)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	sum := sha256.Sum256(append([]byte("blob "+strconv.Itoa(len(f.Content))+"\x00"), f.Content...))
	switch {
	case repo.ObjectFormat != "sha256" || listed.ObjectFormat != "sha256":
		t.Errorf("object format %q by Repo, %q by Resolve; the forge says %q", repo.ObjectFormat, listed.ObjectFormat, raw.ObjectFormatName)
	case f.OID != hex.EncodeToString(sum[:]) || f.Mode != "100644":
		t.Errorf("README.md: blob %s mode %s, want the sha256 blob id %x", f.OID, f.Mode, sum)
	}
	finding(t, "sha256-driver", "a sha256 repository: object_format_name %q; the driver reports %q (Repo) and %q (Resolve) and reads README.md as blob %s",
		raw.ObjectFormatName, repo.ObjectFormat, listed.ObjectFormat, f.OID)
}

// TestAdminTokenRefused: the driver refuses a token of a site admin, which
// could act as anyone through Sudo. The token itself works:
// Probe reads the instance with it. Self refuses it, saying why, and so does
// the writer's Target, before it reads or writes anything of the
// repository.
func TestAdminTokenRefused(t *testing.T) {
	e := needLive(t)
	rp := e.provider(t)
	ctx := context.Background()
	r := construct(t, "NewReader", func() (platform.Reader, error) { return gitea.NewReader(rp, credential(e.Admin), e.HTTP) })
	if _, err := r.Probe(ctx); err != nil {
		t.Fatalf("Probe with a site admin's token: %v; the token works, Self must be what refuses it", err)
	}
	_, err := r.Self(ctx)
	checkAdminRefused(t, e, "Self", err)
	w := construct(t, "NewWriter", func() (platform.Writer, error) { return gitea.NewWriter(rp, credential(e.Admin), e.HTTP) })
	_, err = w.Target(ctx, platform.Repo{Host: e.Host, ID: "1", Path: e.Org + "/any"}, platform.Perms{Contents: true, PRs: true})
	checkAdminRefused(t, e, "Target", err)
	finding(t, "admin-token", "a site admin's token: Probe works, Self and Target refuse it: %v", err)
}

// checkAdminRefused checks that err is the driver's refusal of a site
// admin's token: ClassInvalid, naming Sudo, without the token.
func checkAdminRefused(t *testing.T, e *liveEnv, what string, err error) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s accepted a site admin's token", what)
	case platform.ClassOf(err) != platform.ClassInvalid || !strings.Contains(err.Error(), "site administrator") || !strings.Contains(err.Error(), "Sudo"):
		t.Errorf("%s with a site admin's token: %v (class %v), want the refusal of an administrator's token (invalid, naming Sudo)",
			what, err, platform.ClassOf(err))
	case e.Redact.Contains(err.Error()):
		t.Errorf("%s: the refusal shows a token", what)
	}
}

// TestVisibility: the driver reports as "public" only what someone not
// signed in can see (a public hub never names another target). A
// repository that is not private, in a public organisation, is
// public on a default instance; on one with [service] REQUIRE_SIGNIN_VIEW =
// true (gitea.sh --require-signin) nobody sees it without signing in, while
// the API still says private false and the owner's visibility public, so
// the driver reports it as internal. A private repository is private, one
// in a private organisation internal, either way.
func TestVisibility(t *testing.T) {
	e := needLive(t)
	fx := newOrg(t, e, "vis")
	pub := "vis-pub-" + randHex(t, 4)
	fx.admin().ok(t, http.MethodPost, "/orgs", map[string]any{"username": pub, "visibility": "public"}, nil)
	fx.team(t, pub, "read-all", "read", true, e.Reader, e.Writer)
	for _, r := range []struct {
		owner, name string
		private     bool
	}{{pub, "open", false}, {pub, "closed", true}, {fx.org, "member-only", false}} {
		fx.admin().ok(t, http.MethodPost, "/orgs/"+r.owner+"/repos", map[string]any{
			"name": r.name, "private": r.private, "auto_init": true, "default_branch": "main",
		}, nil)
	}
	ctx := t.Context()
	resp, err := e.HTTP.JSON(ctx, http.MethodGet, e.URL+"/api/v1/repos/"+pub+"/open", nil, nil, nil)
	anon := 0
	if resp != nil {
		anon = resp.Status
	}
	finding(t, "require-signin-view", "REQUIRE_SIGNIN_VIEW %v: an anonymous GET of a public organisation's repository that is not private answers %d (%v)",
		e.SignInRequired, anon, err)
	if e.SignInRequired != (anon == http.StatusForbidden || anon == http.StatusUnauthorized) {
		t.Errorf("anonymous GET: %d, %v; the harness says REQUIRE_SIGNIN_VIEW is %v", anon, err, e.SignInRequired)
	}
	want := map[string]string{pub + "/open": "public", pub + "/closed": "private", fx.org + "/member-only": "internal"}
	if e.SignInRequired {
		want[pub+"/open"] = "internal"
	}
	r, w := newDrivers(t, e)
	for _, reader := range []platform.Reader{r, w} {
		for p, v := range want {
			got, err := reader.Repo(ctx, p)
			if err != nil || got.Visibility != v {
				t.Errorf("Repo(%s) = %q, %v; want %s", p, got.Visibility, err, v)
			}
		}
		res, err := reader.Resolve(ctx, platform.Selector{Namespace: pub})
		if err != nil || len(res.Repos) != 2 {
			t.Fatalf("Resolve(%s) = %+v, %v", pub, res, err)
		}
		for _, got := range res.Repos {
			if got.Visibility != want[got.Path] {
				t.Errorf("Resolve: %s is %q, want %s", got.Path, got.Visibility, want[got.Path])
			}
		}
	}
}
