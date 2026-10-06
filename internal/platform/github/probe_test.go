package github

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestProbe: the flavor from the host, the GHES version from /meta or its
// header, and the capabilities of each credential.
func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      fixtureOpts
		meta      func(w http.ResponseWriter)
		flavor    string
		version   string
		api       bool
		signed    bool
		closer    bool
		perMinute int
	}{
		{name: "github.com App", opts: fixtureOpts{kind: credApp, host: "github.com"}, flavor: "github", api: true, signed: true, closer: true, perMinute: 60},
		{name: "GHE.com App", opts: fixtureOpts{kind: credApp, host: "acme.ghe.com"}, flavor: "ghe.com", api: true, signed: true, closer: true, perMinute: 60},
		{name: "github.com token", opts: fixtureOpts{kind: credToken, host: "github.com"}, flavor: "github", closer: true, perMinute: 60},
		{name: "github.com anonymous", opts: fixtureOpts{kind: credAnonymous, host: "github.com"}, flavor: "github", perMinute: 60},
		{name: "GHES App, /meta", opts: fixtureOpts{kind: credApp}, flavor: "ghes", version: "3.19.4", api: true, closer: true,
			meta: func(w http.ResponseWriter) {
				writeJSON(w, http.StatusOK, map[string]any{"verifiable_password_authentication": false, "installed_version": "3.19.4"})
			}},
		{name: "GHES token, header", opts: fixtureOpts{kind: credToken}, flavor: "ghes", version: "3.20.1", closer: true,
			meta: func(w http.ResponseWriter) {
				w.Header().Set("X-GitHub-Enterprise-Version", "3.20.1")
				writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Must authenticate to access this API."})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.opts)
			if tc.meta != nil {
				f.handle(http.MethodGet, "/meta", func(w http.ResponseWriter, r *http.Request) {
					if tc.opts.kind == credApp && r.Header.Get("Authorization") != "" {
						t.Error("GET /meta with the App's JWT")
					}
					tc.meta(w)
				})
			}
			caps, err := f.reader.Probe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case caps.Flavor != tc.flavor || caps.Version != tc.version:
				t.Errorf("flavor %s %s, want %s %s", caps.Flavor, caps.Version, tc.flavor, tc.version)
			case caps.Commit.API != tc.api || caps.Commit.SignedByPlatform != tc.signed || !caps.Commit.CAS:
				t.Errorf("commit %+v", caps.Commit)
			case caps.CloserKnown != tc.closer:
				t.Errorf("CloserKnown %v", caps.CloserKnown)
			case caps.MaxBody != 58000 || caps.Draft != platform.DraftNative || !caps.WorkflowPerm || caps.QuickActions || caps.LabelsByID ||
				caps.Marker != platform.MarkerInBody || !slices.Equal(caps.RuntimeOnly, []string{"branch_protection"}):
				t.Errorf("caps %+v", caps)
			case caps.Limits.WritesPerMinute != tc.perMinute || caps.Limits.MinInterval != time.Second || caps.Limits.Reads != 8:
				t.Errorf("limits %+v", caps.Limits)
			}
			if _, err := f.reader.Probe(t.Context()); err != nil || len(f.requests(http.MethodGet, "/meta")) > 1 {
				t.Errorf("probed again: %v", err)
			}
		})
	}

	f := newFixture(t, fixtureOpts{kind: credToken})
	f.json(http.MethodGet, "/meta", http.StatusOK, map[string]any{"verifiable_password_authentication": true})
	_, err := f.reader.Probe(t.Context())
	wantClass(t, "a server that is no GHES", err, platform.ClassUnknown, nil)
}

// TestSelf: an App is its bot, found through GET /app with the JWT and the
// bot's user; a token is GET /user; an installation token given as a token
// is refused with advice; an anonymous reader has no account.
func TestSelf(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	a, err := f.writer.Self(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Account{ID: "5001", Login: "touchmark-write[bot]", Email: "5001+touchmark-write[bot]@users.noreply.github.com", Kind: platform.KindBot}
	if a != want {
		t.Errorf("Self = %+v, want %+v", a, want)
	}
	if calls := f.requests(http.MethodGet, "/users/touchmark-write[bot]"); len(calls) != 1 || calls[0].RawPath != "/api/v3/users/touchmark-write%5Bbot%5D" {
		t.Errorf("bot lookup: %+v", calls)
	}
	if _, err := f.writer.Self(t.Context()); err != nil || len(f.requests(http.MethodGet, "/app")) != 1 {
		t.Errorf("Self asked again: %v", err)
	}

	tf := newFixture(t, fixtureOpts{kind: credToken, host: "github.com"})
	tf.json(http.MethodGet, "/user", http.StatusOK, user(3001, "alice", "User"))
	a, err = tf.reader.Self(t.Context())
	if err != nil || a.ID != "3001" || a.Kind != platform.KindUser || a.Email != "3001+alice@users.noreply.github.com" {
		t.Errorf("token Self = %+v, %v", a, err)
	}

	it := newFixture(t, fixtureOpts{kind: credToken})
	it.json(http.MethodGet, "/user", http.StatusForbidden, ghError("Resource not accessible by integration"))
	_, err = it.reader.Self(t.Context())
	wantClass(t, "an installation token", err, platform.ClassInvalid, nil)

	an := newFixture(t, fixtureOpts{kind: credAnonymous})
	_, err = an.reader.Self(t.Context())
	wantClass(t, "anonymous", err, platform.ClassAuth, nil)
}

// TestLookup: users and bots by login with an installation token of the
// App (a JWT cannot read users); a missing login is ErrNotFound.
func TestLookup(t *testing.T) {
	f := newFixture(t)
	f.json(http.MethodGet, "/app/installations", http.StatusOK, []any{installation(instAcme, "acme", "Organization", nil)})
	f.json(http.MethodGet, "/users/bob", http.StatusOK, user(3002, "bob", "User"))
	f.json(http.MethodGet, "/users/acme-janitor[bot]", http.StatusOK, user(6001, "acme-janitor[bot]", "Bot"))
	f.json(http.MethodGet, "/users/nobody", http.StatusNotFound, notFoundBody)
	for login, want := range map[string]platform.Account{
		"bob":               {ID: "3002", Login: "bob", Kind: platform.KindUser},
		"acme-janitor[bot]": {ID: "6001", Login: "acme-janitor[bot]", Kind: platform.KindBot},
	} {
		a, err := f.reader.Lookup(t.Context(), login)
		if err != nil || a.ID != want.ID || a.Login != want.Login || a.Kind != want.Kind {
			t.Errorf("Lookup(%s) = %+v, %v", login, a, err)
		}
	}
	for _, c := range f.requests(http.MethodGet, "/users/bob") {
		if m, ok := f.tokenOf(c.Auth); !ok || m.installation != instAcme {
			t.Errorf("GET /users/bob with %v", m)
		}
	}
	_, err := f.reader.Lookup(t.Context(), "nobody")
	wantClass(t, "missing login", err, platform.ClassNotFound, platform.ErrNotFound)
	_, err = f.reader.Lookup(t.Context(), "a/b")
	wantClass(t, "a path", err, platform.ClassNotFound, platform.ErrNotFound)
	if n := len(f.requests(http.MethodGet, "/app/installations")); n != 1 {
		t.Errorf("listed installations %d times", n)
	}
}

// TestLookupSuspended: requests about no repository never go through a
// suspended installation. The first installation listed is suspended
// (suspended_at): the next page's is used. One whose token is refused as
// suspended is skipped for the next. An App with no live installation
// reads accounts anonymously, so Self still works.
func TestLookupSuspended(t *testing.T) {
	suspended := func(id int64, login string) map[string]any {
		inst := installation(id, login, "Organization", nil)
		inst["suspended_at"] = "2026-09-20T10:00:00Z"
		return inst
	}
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	f.handle(http.MethodGet, "/app/installations", f.asApp(func(w http.ResponseWriter, r *http.Request) {
		items := []any{suspended(7009, "old-org")}
		for range 120 {
			items = append(items, suspended(7009, "old-org"))
		}
		items = append(items, installation(instAcme, "acme", "Organization", nil))
		servePage(w, r, items, nil)
	}))
	f.json(http.MethodPost, "/app/installations/7009/access_tokens", http.StatusForbidden, ghError("This installation has been suspended"))
	f.json(http.MethodGet, "/users/bob", http.StatusOK, user(3002, "bob", "User"))
	a, err := f.writer.Self(t.Context())
	if err != nil || a.Login != "touchmark-write[bot]" {
		t.Fatalf("Self = %+v, %v", a, err)
	}
	if _, err := f.reader.Lookup(t.Context(), "bob"); err != nil {
		t.Errorf("Lookup: %v", err)
	}
	for _, c := range f.requests(http.MethodGet, "/users/bob") {
		if m, ok := f.tokenOf(c.Auth); !ok || m.installation != instAcme {
			t.Errorf("GET /users/bob with %+v", m)
		}
	}
	if len(f.requests(http.MethodPost, "/app/installations/7009/access_tokens")) != 0 {
		t.Error("a token was asked of the suspended installation")
	}

	// Suspended after it was listed: the refusal moves on to another.
	g := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	g.json(http.MethodPost, "/app/installations/7001/access_tokens", http.StatusForbidden, ghError("This installation has been suspended"))
	if a, err := g.writer.Self(t.Context()); err != nil || a.ID != "5001" {
		t.Errorf("Self past a refused installation = %+v, %v", a, err)
	}
	for _, c := range g.requests(http.MethodGet, "/users/touchmark-write[bot]") {
		if m, ok := g.tokenOf(c.Auth); !ok || m.installation != instAlice {
			t.Errorf("the bot was looked up with %+v", m)
		}
	}

	// Every installation suspended: anonymous.
	h := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	h.handle(http.MethodGet, "/app/installations", h.asApp(func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, []any{suspended(7009, "old-org")}, nil)
	}))
	if a, err := h.writer.Self(t.Context()); err != nil || a.ID != "5001" {
		t.Errorf("Self without a live installation = %+v, %v", a, err)
	}
	if c := h.requests(http.MethodGet, "/users/touchmark-write[bot]"); len(c) != 1 || c[0].Auth != "" {
		t.Errorf("the bot was looked up with %+v, want anonymously", c)
	}
}
