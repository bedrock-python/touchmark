package gitlab

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestNewDriverRefusals(t *testing.T) {
	s := newAPIServer(t)
	client := httpx.New(httpx.Options{})
	tok := auth.Credential{Kind: auth.Token, Token: testToken(t)}
	bad := func(mut func(*config.ResolvedProvider)) config.ResolvedProvider {
		p := s.provider()
		mut(&p)
		return p
	}
	for _, tc := range []struct {
		name string
		p    config.ResolvedProvider
		c    auth.Credential
	}{
		{"another type", bad(func(p *config.ResolvedProvider) { p.Type = "gitea" }), tok},
		{"no api_url", bad(func(p *config.ResolvedProvider) { p.APIURL = "" }), tok},
		{"api_url with credentials", bad(func(p *config.ResolvedProvider) { p.APIURL = "https://u:p@gitlab.example.com/api/v4" }), tok},
		{"an app", s.provider(), auth.Credential{Kind: auth.App, AppID: "1", AppKey: []byte("k")}},
		{"an empty token", s.provider(), auth.Credential{Kind: auth.Token}},
	} {
		if _, err := NewReader(tc.p, tc.c, client); err == nil {
			t.Errorf("NewReader, %s: no error", tc.name)
		}
	}
	if _, err := NewReader(s.provider(), tok, nil); err == nil {
		t.Error("NewReader without an HTTP client: no error")
	}
	if _, err := NewWriter(s.provider(), auth.Credential{}, client); err == nil {
		t.Error("NewWriter without a token: no error")
	}
}

func TestProbe(t *testing.T) {
	fx := newFixture(t)
	caps, err := fx.reader.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case caps.Flavor != "gitlab" || caps.Version != "18.11.2":
		t.Errorf("flavor %q version %q", caps.Flavor, caps.Version)
	case caps.MaxBody != 200000 || caps.Draft != platform.DraftTitlePrefix || caps.DraftPrefix != "Draft: ":
		t.Errorf("body %d draft %v %q", caps.MaxBody, caps.Draft, caps.DraftPrefix)
	case !caps.QuickActions || caps.LabelsByID || caps.WorkflowPerm || !caps.CloserKnown:
		t.Errorf("quick actions %v, labels by id %v, workflow perm %v, closer known %v",
			caps.QuickActions, caps.LabelsByID, caps.WorkflowPerm, caps.CloserKnown)
	case caps.Commit.API || caps.Marker != platform.MarkerInBody:
		t.Errorf("commit %+v marker %v", caps.Commit, caps.Marker)
	case len(caps.RuntimeOnly) != 1 || caps.RuntimeOnly[0] != "push_rules":
		t.Errorf("runtime only %v", caps.RuntimeOnly)
	case caps.Limits.MinInterval != 100*time.Millisecond || caps.Limits.Reads != 8:
		t.Errorf("self-managed limits %+v", caps.Limits)
	}
	// Once per run: the second Probe asks nothing.
	fx.reset()
	if _, err := fx.reader.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(fx.requests("", "")); n != 0 {
		t.Errorf("a second Probe sent %d requests", n)
	}
	if got := limitsFor("GitLab.com"); got.MinInterval != 250*time.Millisecond || got.CommentsPerMinute != 50 {
		t.Errorf("gitlab.com limits %+v", got)
	}
}

func TestProbeTokenHeader(t *testing.T) {
	fx := newFixture(t)
	if _, err := fx.reader.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.requests("", "") {
		if c.Token != fx.token || c.Auth != "" {
			t.Errorf("%s %s: PRIVATE-TOKEN %v, Authorization %q; want the token in PRIVATE-TOKEN only", c.Method, c.Path, c.Token == fx.token, c.Auth)
		}
	}
}

func TestProbeVersions(t *testing.T) {
	// An instance without /metadata (a proxy that hides it) answers /version.
	s := newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusNotFound, map[string]any{"error": "404 Not Found"})
	s.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"version": "17.11.7-ee", "revision": "abc"})
	r, err := NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	caps, err := r.Probe(t.Context())
	if err != nil || caps.Version != "17.11.7-ee" {
		t.Errorf("Probe by /version = %q, %v", caps.Version, err)
	}

	// A /metadata answer without the version (seen once on CE 19.4.1 under
	// load) falls back to /version; when neither tells it, the error shows
	// what came back.
	s = newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusOK, map[string]any{"kas": map[string]any{"enabled": false}})
	s.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"version": "19.4.1", "revision": "abc"})
	r, _ = NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
	if caps, err := r.Probe(t.Context()); err != nil || caps.Version != "19.4.1" {
		t.Errorf("Probe with /metadata without a version = %q, %v", caps.Version, err)
	}
	s = newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusOK, map[string]any{})
	s.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"odd": true})
	r, _ = NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
	_, err = r.Probe(t.Context())
	wantClass(t, "Probe without any version", err, platform.ClassUnknown, nil)
	if err == nil || !strings.Contains(err.Error(), `HTTP 200: {"odd":true}`) {
		t.Errorf("error %v does not show the answer", err)
	}

	// GitLab 16 is too old.
	s = newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusOK, map[string]any{"version": "16.11.10", "revision": "abc", "enterprise": false})
	r, _ = NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
	_, err = r.Probe(t.Context())
	wantClass(t, "Probe of GitLab 16", err, platform.ClassUnsupported, nil)

	// An anonymous reader cannot read the version, and goes on.
	s = newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusUnauthorized, msg("401 Unauthorized"))
	r, _ = NewReader(s.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
	caps, err = r.Probe(t.Context())
	if err != nil || caps.Flavor != "gitlab" || caps.Version != "unknown" {
		t.Errorf("anonymous Probe = %q %q, %v", caps.Flavor, caps.Version, err)
	}
	for _, c := range s.requests("", "") {
		if c.Token != "" || c.Auth != "" {
			t.Errorf("an anonymous request carried a credential")
		}
	}

	// A token the server refuses fails the probe.
	s = newAPIServer(t)
	s.json(http.MethodGet, "/metadata", http.StatusUnauthorized, map[string]any{"error": "invalid_token",
		"error_description": "Token was revoked. You have to re-authorize from the user."})
	r, _ = NewReader(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
	_, err = r.Probe(t.Context())
	wantClass(t, "Probe with a revoked token", err, platform.ClassAuth, nil)
	if err != nil && !strings.Contains(err.Error(), "invalid_token") {
		t.Errorf("error %v lacks the OAuth code", err)
	}
}

func TestSelf(t *testing.T) {
	for _, tc := range []struct {
		name     string
		username string
		bot      bool
		want     platform.AccountKind
	}{
		{"service account", "service_account_group_42_1f2e3d", true, platform.KindServiceAccount},
		{"named service account", "tm-writer", true, platform.KindServiceAccount},
		{"group access token", "group_42_bot_0a1b2c3d4e5f", true, platform.KindBot},
		{"project access token", "project_7_bot_9f8e7d", true, platform.KindBot},
		{"person", "jdoe", false, platform.KindUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.json(http.MethodGet, "/user", http.StatusOK, self(3, tc.username, tc.bot))
			a, err := fx.reader.Self(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if a.ID != "3" || a.Login != tc.username || a.Kind != tc.want {
				t.Errorf("Self = %+v, want id 3, %s, kind %v", a, tc.username, tc.want)
			}
			if a.Email != tc.username+"@noreply.gitlab.example.com" {
				t.Errorf("email %q", a.Email)
			}
			// Looked up once.
			if _, err := fx.reader.Self(t.Context()); err != nil {
				t.Fatal(err)
			}
			if n := len(fx.requests(http.MethodGet, "/user")); n != 1 {
				t.Errorf("%d requests of /user", n)
			}
		})
	}
}

func TestSelfRefusesAdmin(t *testing.T) {
	fx := newFixture(t)
	admin := self(1, "root", false)
	admin["is_admin"] = true
	admin["note"] = ""
	fx.json(http.MethodGet, "/user", http.StatusOK, admin)
	_, err := fx.reader.Self(t.Context())
	wantClass(t, "Self of an administrator", err, platform.ClassInvalid, nil)
	if err != nil && !strings.Contains(err.Error(), "Sudo") {
		t.Errorf("error %v does not say why", err)
	}
	_, err = fx.writer.Target(t.Context(), platform.Repo{ID: "7", Path: "acme/api"}, platform.Perms{Contents: true, PRs: true})
	wantClass(t, "Target of an administrator", err, platform.ClassInvalid, nil)
	if n := len(fx.requests(http.MethodGet, "/projects/7")); n != 0 {
		t.Error("Target of an administrator read the project")
	}

	anon, _ := NewReader(fx.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
	_, err = anon.Self(t.Context())
	wantClass(t, "Self of an anonymous reader", err, platform.ClassAuth, nil)
}

func TestLookup(t *testing.T) {
	fx := newFixture(t)
	fx.handle(http.MethodGet, "/users", func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToLower(r.URL.Query().Get("username")) {
		case "tm-writer":
			writeJSON(w, http.StatusOK, []any{basic(3, "tm-writer")})
		case "group_42_bot_0a1b2c":
			// A bot of a revoked group access token: blocked, still listed.
			b := basic(11, "group_42_bot_0a1b2c")
			b["state"] = "blocked"
			writeJSON(w, http.StatusOK, []any{b})
		case "hidden":
			writeJSON(w, http.StatusOK, []any{basic(12, "hidden")})
		default:
			writeJSON(w, http.StatusOK, []any{})
		}
	})
	fx.json(http.MethodGet, "/users/3", http.StatusOK, user(3, "tm-writer", true))
	fx.json(http.MethodGet, "/users/12", http.StatusNotFound, msg("404 User Not Found"))

	a, err := fx.reader.Lookup(t.Context(), "TM-Writer")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "3" || a.Login != "tm-writer" || a.Kind != platform.KindServiceAccount {
		t.Errorf("Lookup = %+v", a)
	}
	if q := fx.requests(http.MethodGet, "/users")[0].Query.Get("username"); q != "TM-Writer" {
		t.Errorf("username filter %q", q)
	}
	a, err = fx.reader.Lookup(t.Context(), "group_42_bot_0a1b2c")
	if err != nil || a.ID != "11" || a.Kind != platform.KindBot {
		t.Errorf("Lookup of a blocked bot = %+v, %v", a, err)
	}
	a, err = fx.reader.Lookup(t.Context(), "hidden")
	if err != nil || a.ID != "12" || a.Kind != platform.KindUnknown {
		t.Errorf("Lookup of a user GET /users/:id hides = %+v, %v", a, err)
	}
	for _, login := range []string{"nobody", "", "a/b", ".."} {
		_, err := fx.reader.Lookup(t.Context(), login)
		wantClass(t, "Lookup("+login+")", err, platform.ClassNotFound, platform.ErrNotFound)
	}
}

func TestKindOf(t *testing.T) {
	yes, no := ptr(true), ptr(false)
	for _, tc := range []struct {
		username string
		bot      *bool
		want     platform.AccountKind
	}{
		{"project_7_bot_9f8e7d", nil, platform.KindBot},
		{"project_7_bot", yes, platform.KindBot},
		{"project_7_bot2", yes, platform.KindBot},
		{"group_42_bot_0a1b", yes, platform.KindBot},
		{"service_account_0a1b2c", yes, platform.KindServiceAccount},
		{"renovate-sa", yes, platform.KindServiceAccount},
		{"support-bot", yes, platform.KindBot},
		{"GitLab-Admin-Bot", yes, platform.KindBot},
		{"jdoe", no, platform.KindUser},
		{"jdoe", nil, platform.KindUnknown},
		{"project_7_botanist", no, platform.KindUser},
	} {
		if got := kindOf(tc.username, tc.bot); got != tc.want {
			t.Errorf("kindOf(%q, %v) = %v, want %v", tc.username, tc.bot, got, tc.want)
		}
	}
}
