package github

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestJWT: RS256 over the documented claims, iss the App's id as a string,
// iat 60 s back, exp 9 minutes ahead (at most 10), the same JWT until 2
// minutes before it expires, then a new one; each registered as a secret.
func TestJWT(t *testing.T) {
	f := newFixture(t)
	a := f.reader.c.app
	jwt, err := a.token()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.verifyJWT(jwt); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(jwt, ".")[1])
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	now := start.Unix()
	if claims["iss"] != appID || claims["iat"] != float64(now-60) || claims["exp"] != float64(now+9*60) || len(claims) != 3 {
		t.Errorf("claims %v", claims)
	}
	if !f.reg.Contains(jwt) || !f.reader.c.masks.Contains(jwt) {
		t.Error("the JWT is not registered as a secret")
	}

	f.clock.add(6*time.Minute + 59*time.Second)
	if again, _ := a.token(); again != jwt {
		t.Error("the JWT was signed again with more than 2 minutes left")
	}
	f.clock.add(2 * time.Second)
	next, err := a.token()
	if err != nil || next == jwt {
		t.Fatalf("no new JWT 2 minutes before expiry: %v", err)
	}
	if err := f.verifyJWT(next); err != nil {
		t.Error(err)
	}
}

// TestAppKeyForms: PKCS #1 and PKCS #8 RSA keys parse; other keys and
// garbage are refused when the driver is built.
func TestAppKeyForms(t *testing.T) {
	key, pkcs1 := appKeyPEM(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s := newAPIServer(t)
	for name, k := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		if _, err := NewReader(s.provider(""), auth.Credential{Kind: auth.App, AppID: appID, AppKey: k}, httpx.New(httpx.Options{})); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, k := range map[string][]byte{
		"not pem":   []byte("not a key"),
		"not rsa":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
		"bad pkcs1": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
	} {
		if _, err := NewReader(s.provider(""), auth.Credential{Kind: auth.App, AppID: appID, AppKey: k}, httpx.New(httpx.Options{})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestInstallationTokens: the installation of an owner is found once (an
// organization, else a user), its reading token is minted with read
// permissions only, reused, renewed 10 minutes before it expires, and
// registered with its Basic form.
func TestInstallationTokens(t *testing.T) {
	f := newFixture(t)
	a := f.reader.c.app
	ctx := t.Context()
	tok, err := a.ownerToken(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := f.tokenOf("Bearer " + tok)
	if !ok || m.installation != instAcme || len(m.repos) != 0 {
		t.Fatalf("token %v", m)
	}
	if want := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}; !mapsEqual(m.perms, want) {
		t.Errorf("reading token permissions %v, want %v", m.perms, want)
	}
	basicForm := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	if !slices.Contains(f.reg.Forms(), tok) || !slices.Contains(f.reg.Forms(), basicForm) {
		t.Error("the token or its Basic form is not in the run's registry")
	}
	// Reused, also for another case of the owner's login.
	if again, _ := a.ownerToken(ctx, "ACME"); again != tok {
		t.Error("a second token was minted within its hour")
	}
	if n := len(f.requests(http.MethodGet, "/orgs/acme/installation")); n != 1 {
		t.Errorf("the installation of acme was looked up %d times", n)
	}
	f.clock.add(49*time.Minute + 59*time.Second)
	if again, _ := a.ownerToken(ctx, "acme"); again != tok {
		t.Error("renewed with more than 10 minutes left")
	}
	f.clock.add(2 * time.Second)
	renewed, err := a.ownerToken(ctx, "acme")
	if err != nil || renewed == tok {
		t.Fatalf("not renewed 10 minutes before expiry: %v", err)
	}
	if f.mintedCount(instAcme) != 2 {
		t.Errorf("minted %d tokens", f.mintedCount(instAcme))
	}

	// A user's installation: /orgs answers 404, /users has it.
	if _, err := a.ownerToken(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if f.mintedCount(instAlice) != 1 {
		t.Errorf("alice: minted %d", f.mintedCount(instAlice))
	}
}

// TestInstallationMissing: an owner without the App is ClassNotFound, and
// asked for once.
func TestInstallationMissing(t *testing.T) {
	f := newFixture(t)
	f.json(http.MethodGet, "/orgs/nobody/installation", http.StatusNotFound, notFoundBody)
	f.json(http.MethodGet, "/users/nobody/installation", http.StatusNotFound, notFoundBody)
	for range 2 {
		_, err := f.reader.c.app.ownerToken(t.Context(), "nobody")
		wantClass(t, "no installation", err, platform.ClassNotFound, platform.ErrNotFound)
	}
	if n := len(f.requests(http.MethodGet, "/orgs/nobody/installation")); n != 1 {
		t.Errorf("asked %d times", n)
	}
	// A failure that is not a 404 is not remembered.
	f.json(http.MethodGet, "/orgs/flaky/installation", http.StatusBadGateway, ghError("Server Error"))
	for range 2 {
		_, err := f.reader.c.app.ownerToken(t.Context(), "flaky")
		wantClass(t, "5xx", err, platform.ClassTransient, nil)
	}
	if n := len(f.requests(http.MethodGet, "/orgs/flaky/installation")); n != 2 {
		t.Errorf("a transient failure was remembered: %d requests", n)
	}
}

// TestMintRefused: a mint that does not grant a write permission asked for
// is revoked and refused; a 422 about permissions is ClassPermission, one
// about repositories outside the installation ClassNotFound.
func TestMintRefused(t *testing.T) {
	f := newFixture(t)
	a := f.reader.c.app
	f.handle(http.MethodPost, "/app/installations/7001/access_tokens", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
		tok := "ghs_" + randomHex(t, 30)
		f.mu.Lock()
		f.tokens[tok] = mintedToken{installation: instAcme}
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": start.Add(time.Hour).Format(time.RFC3339),
			"permissions": map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}})
	}))
	_, err := a.mint(t.Context(), "mint", instAcme, mintRequest{RepositoryIDs: []int64{1}, Permissions: targetPermissions(platform.Perms{Contents: true, PRs: true, Workflows: true})})
	wantClass(t, "workflows not granted", err, platform.ClassPermission, nil)
	if ruleOfErr(err) != "workflows" || len(f.revokedTokens()) != 1 {
		t.Errorf("rule %q, revoked %d", ruleOfErr(err), len(f.revokedTokens()))
	}

	for _, tc := range []struct {
		msg   string
		class platform.Class
		rule  string
	}{
		{"The permissions requested are not granted to this installation.", platform.ClassPermission, "workflows"},
		{"There is at least one repository that does not exist or is not accessible to the parent installation.", platform.ClassNotFound, ""},
	} {
		f.json(http.MethodPost, "/app/installations/7001/access_tokens", http.StatusUnprocessableEntity, ghError(tc.msg))
		_, err := a.mint(t.Context(), "mint", instAcme, mintRequest{RepositoryIDs: []int64{1}, Permissions: targetPermissions(platform.Perms{Contents: true, Workflows: true})})
		wantClass(t, tc.msg, err, tc.class, nil)
		if ruleOfErr(err) != tc.rule {
			t.Errorf("%s: rule %q", tc.msg, ruleOfErr(err))
		}
	}
}

// TestMintWider: a token wider than asked for is revoked and refused: a
// permission not asked for or above the level asked for (metadata read
// aside), a repository not asked for, one asked for missing, or a
// selection of "all" while repositories were named (GitHub widens a token
// that way when it ignores the request).
func TestMintWider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		perms     map[string]string
		selection string
		repos     []any
		ok        bool
	}{
		{"exact", map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}, "selected", []any{map[string]any{"id": 1}}, true},
		{"no repositories listed", map[string]string{"contents": "write", "pull_requests": "write"}, "selected", nil, true},
		{"an extra permission", map[string]string{"contents": "write", "pull_requests": "write", "administration": "write"}, "selected", []any{map[string]any{"id": 1}}, false},
		{"a higher level", map[string]string{"contents": "write", "pull_requests": "write", "metadata": "write"}, "selected", []any{map[string]any{"id": 1}}, false},
		{"another repository", map[string]string{"contents": "write", "pull_requests": "write"}, "selected", []any{map[string]any{"id": 1}, map[string]any{"id": 2}}, false},
		{"the repository missing", map[string]string{"contents": "write", "pull_requests": "write"}, "selected", []any{map[string]any{"id": 2}}, false},
		{"every repository", map[string]string{"contents": "write", "pull_requests": "write"}, "all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.handle(http.MethodPost, "/app/installations/7001/access_tokens", f.asApp(func(w http.ResponseWriter, _ *http.Request) {
				tok := "ghs_" + randomHex(t, 30)
				f.mu.Lock()
				f.tokens[tok] = mintedToken{installation: instAcme}
				f.mu.Unlock()
				body := map[string]any{"token": tok, "expires_at": start.Add(time.Hour).Format(time.RFC3339),
					"permissions": tc.perms, "repository_selection": tc.selection}
				if tc.repos != nil {
					body["repositories"] = tc.repos
				}
				writeJSON(w, http.StatusCreated, body)
			}))
			_, err := f.reader.c.app.mint(t.Context(), "mint", instAcme,
				mintRequest{RepositoryIDs: []int64{1}, Permissions: targetPermissions(platform.Perms{Contents: true, PRs: true})})
			switch {
			case tc.ok && (err != nil || len(f.revokedTokens()) != 0):
				t.Errorf("%v, %d revoked", err, len(f.revokedTokens()))
			case !tc.ok && (err == nil || len(f.revokedTokens()) != 1):
				t.Errorf("a wider token: %v, %d revoked", err, len(f.revokedTokens()))
			}
		})
	}
}

// TestCloseRevokes: closing the driver revokes the reading token of every
// installation it used; a per-target token renewed away whose revocation
// failed is revoked again when the target closes.
func TestCloseRevokes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	acme, err := f.reader.c.app.ownerToken(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := f.reader.c.app.ownerToken(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reader.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.revokedTokens(); len(got) != 2 || !slices.Contains(got, acme) || !slices.Contains(got, alice) {
		t.Errorf("revoked %d tokens, want the two reading tokens", len(got))
	}
	// Used again, it mints again.
	if again, err := f.reader.c.app.ownerToken(ctx, "acme"); err != nil || again == acme {
		t.Errorf("after Close: %v (the same token: %v)", err, again == acme)
	}

	g := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	g.repoInstallation(nil)
	tw, err := g.writer.Target(ctx, apiRepoTarget, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := tw.(*appTarget).credential(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	g.handle(http.MethodDelete, "/installation/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadGateway, ghError("Server Error"))
	})
	g.clock.add(51 * time.Minute)
	second, err := tw.(*appTarget).credential(ctx, "test")
	if err != nil || second == first {
		t.Fatalf("not renewed: %v", err)
	}
	var revoked []string
	g.handle(http.MethodDelete, "/installation/token", func(w http.ResponseWriter, r *http.Request) {
		revoked = append(revoked, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(http.StatusNoContent)
	})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(revoked, []string{second, first}) {
		t.Errorf("Close revoked %d tokens, want the current one and the one whose revocation failed", len(revoked))
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
