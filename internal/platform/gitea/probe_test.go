package gitea

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestNewDriversRefuseBadConfigurations(t *testing.T) {
	s := newAPIServer(t)
	tok := auth.Credential{Kind: auth.Token, Token: testToken(t)}
	client := httpx.New(httpx.Options{})
	good := s.provider("gitea")
	for _, tc := range []struct {
		name   string
		p      config.ResolvedProvider
		c      auth.Credential
		client *httpx.Client
		want   string
	}{
		{"github type", func() config.ResolvedProvider { p := good; p.Type = "github"; return p }(), tok, client, `type "github"`},
		{"no client", good, tok, nil, "no HTTP client"},
		{"app credential", good, auth.Credential{Kind: auth.App, AppID: "1", AppKey: []byte("k")}, client, "GitHub App"},
		{"empty token", good, auth.Credential{Kind: auth.Token}, client, "empty token"},
		{"kindless secret", good, auth.Credential{Token: "stray"}, client, "without a kind"},
		{"no api url", func() config.ResolvedProvider { p := good; p.APIURL = ""; return p }(), tok, client, "api_url is empty"},
		{"ftp url", func() config.ResolvedProvider { p := good; p.URL = "ftp://h"; return p }(), tok, client, "not an http"},
		{"credentials in url", func() config.ResolvedProvider { p := good; p.APIURL = "https://u:p@h/api/v1"; return p }(), tok, client, "without credentials"},
	} {
		if _, err := NewReader(tc.p, tc.c, tc.client); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("NewReader, %s: %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := NewWriter(good, auth.Credential{}, client); err == nil {
		t.Error("NewWriter without a token succeeded")
	}
	for _, typ := range []string{"gitea", "forgejo"} {
		if _, err := NewReader(s.provider(typ), auth.Credential{}, client); err != nil {
			t.Errorf("anonymous %s reader: %v", typ, err)
		}
	}
}

func TestProbeGitea(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.asGitea()
	caps, err := fx.reader.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Caps{
		Flavor: "gitea", Version: "1.27.3", MaxBody: 58000, Draft: platform.DraftTitlePrefix, DraftPrefix: "WIP: ",
		LabelsByID: true, CloserKnown: true, Marker: platform.MarkerInBody,
		Limits: platform.Limits{Reads: 4, GitReads: 2, MinInterval: 250 * time.Millisecond},
	}
	if caps.Flavor != want.Flavor || caps.Version != want.Version || caps.MaxBody != want.MaxBody || caps.Draft != want.Draft ||
		caps.DraftPrefix != want.DraftPrefix || !caps.LabelsByID || !caps.CloserKnown || caps.WorkflowPerm || caps.QuickActions ||
		caps.Commit.API || caps.Marker != want.Marker || caps.Limits != want.Limits || len(caps.RuntimeOnly) != 0 {
		t.Errorf("Probe = %+v, want %+v", caps, want)
	}
	// Every request carries the token, but one: the version asked
	// anonymously, which tells whether the instance requires signing in.
	var anonymous []string
	for _, c := range fx.requests("", "") {
		switch c.Auth {
		case "token " + fx.token:
		case "":
			anonymous = append(anonymous, c.Method+" "+c.Path)
		default:
			t.Errorf("%s %s: Authorization %q, want the API token", c.Method, c.Path, c.Auth)
		}
	}
	if !slices.Equal(anonymous, []string{"GET /api/v1/version"}) {
		t.Errorf("anonymous requests %q, want the version once", anonymous)
	}
	// Probed once per driver.
	fx.reset()
	if _, err := fx.reader.Probe(t.Context()); err != nil || len(fx.requests("", "")) != 0 {
		t.Errorf("second Probe: %v, %d requests", err, len(fx.requests("", "")))
	}
}

func TestProbeForgejo(t *testing.T) {
	for _, v := range []string{"15.0.9", "16.0.5"} {
		fx := newFixture(t, "forgejo")
		fx.asForgejo(v)
		caps, err := fx.reader.Probe(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if caps.Flavor != "forgejo" || caps.Version != v || !caps.CloserKnown || caps.DraftPrefix != "WIP: " {
			t.Errorf("Probe of Forgejo %s = %+v", v, caps)
		}
		// Forgejo's /api/v1/version and its frozen suffix never tell the
		// version: it is asked once, anonymously, only to learn whether
		// the instance requires signing in.
		for _, c := range fx.requests(http.MethodGet, "/version") {
			if c.Auth != "" {
				t.Errorf("Forgejo %s: /api/v1/version asked with a credential", v)
			}
		}
		inst, _ := fx.reader.c.instance(t.Context())
		if inst.headFilter() != (v == "16.0.5") {
			t.Errorf("Forgejo %s: head filter %v", v, inst.headFilter())
		}
	}
}

// A custom api_url hides Forgejo's endpoint; the frozen suffix of
// /api/v1/version still tells Forgejo from Gitea.
func TestProbeCustomAPIURL(t *testing.T) {
	s := newAPIServer(t)
	p := s.provider("forgejo")
	p.APIURL = s.base() + "/api/custom"
	s.json(http.MethodGet, "/api/custom/version", http.StatusOK, map[string]any{"version": "15.0.9+gitea-1.22.0"})
	r, err := NewReader(p, auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	caps, err := r.Probe(t.Context())
	if err != nil || caps.Flavor != "forgejo" || caps.Version != "15.0.9" {
		t.Errorf("Probe = %+v, %v; want forgejo 15.0.9", caps, err)
	}
	for _, c := range s.requests("", "") {
		if c.Auth != "" {
			t.Errorf("an anonymous reader sent Authorization to %s", c.Path)
		}
	}
}

// An instance that requires signing in to see anything ([service]
// REQUIRE_SIGNIN_VIEW = true) reports a repository that is not private as
// private false, internal false, of an owner whose visibility is public,
// while nobody who is not signed in may see it: the driver reports it as
// internal, by Repo, Resolve and OpenPRsBy, so a public hub never names it
// in its logs. A private repository stays private.
func TestSignInRequired(t *testing.T) {
	for _, typ := range []string{"gitea", "forgejo"} {
		t.Run(typ, func(t *testing.T) {
			w, _ := sweepWorld(t, typ)
			w.signInOnly(nil)
			w.json(http.MethodGet, "/repos/acme/secret", http.StatusOK, w.apiServer.repo(9, "acme/secret", withField("private", true)))
			w.json(http.MethodGet, "/repos/acme/api", http.StatusOK, w.repo)
			w.pages("/orgs/acme/repos", []any{w.repo, w.apiServer.repo(9, "acme/secret", withField("private", true))})
			w.json(http.MethodGet, "/user", http.StatusOK, w.writer)
			for _, r := range []*reader{w.reader, &w.fixture.writer.reader} {
				got, err := r.Repo(t.Context(), "acme/api")
				if err != nil || got.Visibility != "internal" {
					t.Errorf("Repo of a repository that is not private = %q, %v; want internal", got.Visibility, err)
				}
				got, err = r.Repo(t.Context(), "acme/secret")
				if err != nil || got.Visibility != "private" {
					t.Errorf("Repo of a private repository = %q, %v; want private", got.Visibility, err)
				}
				res, err := r.Resolve(t.Context(), platform.Selector{Namespace: "acme"})
				if err != nil || len(res.Repos) != 2 || res.Repos[0].Visibility != "internal" || res.Repos[1].Visibility != "private" {
					t.Errorf("Resolve = %+v, %v; want acme/api internal, acme/secret private", res, err)
				}
				sw, err := r.OpenPRsBy(t.Context(), []platform.Account{{ID: "3", Login: "tm-writer"}}, []string{syncBranch, aliasBranch})
				if err != nil || len(sw.PRs) == 0 {
					t.Fatalf("OpenPRsBy = %+v, %v", sw, err)
				}
				for _, rp := range sw.PRs {
					if rp.Repo.Visibility != "internal" {
						t.Errorf("OpenPRsBy: %s is %q, want internal", rp.Repo.Path, rp.Repo.Visibility)
					}
				}
			}
			// Asked once per driver, whatever it reads.
			if n := len(anonymousRequests(w.apiServer)); n != 2 {
				t.Errorf("%d anonymous requests, want one per driver", n)
			}
		})
	}
}

// anonymousRequests returns the requests without an Authorization header.
func anonymousRequests(s *apiServer) []apiCall {
	var out []apiCall
	for _, c := range s.requests("", "") {
		if c.Auth == "" {
			out = append(out, c)
		}
	}
	return out
}

// What anonymous users get decides: only a version means that they see
// what is public. Anything else (a refusal, a proxy's login page or
// redirect) is the safe side: signing in is required. A transient failure
// or a rate limit fails the probe, which is not remembered.
func TestSignInRequiredAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		anon   http.HandlerFunc
		signIn bool
		class  platform.Class // of Probe's error; ClassUnknown for none
	}{
		{"version", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"version": "1.27.3"})
		}, false, platform.ClassUnknown},
		{"401", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "token is required"})
		}, true, platform.ClassUnknown},
		{"404", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
		}, true, platform.ClassUnknown},
		{"login page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>sign in</html>"))
		}, true, platform.ClassUnknown},
		{"single sign-on elsewhere", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://sso.example.com/login")
			w.WriteHeader(http.StatusFound)
		}, true, platform.ClassUnknown},
		{"no version", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{})
		}, true, platform.ClassUnknown},
		{"502", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "bad gateway"})
		}, false, platform.ClassTransient},
		{"429", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"message": "slow down"})
		}, false, platform.ClassRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, "gitea")
			fx.signInOnly(tc.anon)
			fx.json(http.MethodGet, "/repos/acme/api", http.StatusOK, fx.repo(7, "acme/api"))
			_, err := fx.reader.Probe(t.Context())
			if tc.class != platform.ClassUnknown {
				wantClass(t, "Probe", err, tc.class, nil)
				// Not remembered: the next probe asks again.
				fx.signInOnly(nil)
				if _, err := fx.reader.Probe(t.Context()); err != nil {
					t.Errorf("Probe after the failure: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			got, err := fx.reader.Repo(t.Context(), "acme/api")
			want := map[bool]string{true: "internal", false: "public"}[tc.signIn]
			if err != nil || got.Visibility != want {
				t.Errorf("Repo = %q, %v; want %s", got.Visibility, err, want)
			}
		})
	}
	// An anonymous reader asks nothing more: its own requests are anonymous.
	s := newAPIServer(t)
	s.asGitea()
	s.json(http.MethodGet, "/repos/acme/api", http.StatusOK, s.repo(7, "acme/api"))
	anon, err := NewReader(s.provider("gitea"), auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := anon.Repo(t.Context(), "acme/api"); err != nil || got.Visibility != "public" {
		t.Errorf("anonymous Repo = %q, %v; want public", got.Visibility, err)
	}
	if n := len(s.requests(http.MethodGet, "/version")); n != 1 {
		t.Errorf("an anonymous reader asked the version %d times, want once", n)
	}
}

// A proxy answering Forgejo's path with a page that is no JSON is not
// Forgejo.
func TestProbeProxyPage(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.handle(http.MethodGet, "/api/forgejo/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>welcome</html>"))
	})
	fx.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"version": "1.26.4"})
	caps, err := fx.reader.Probe(t.Context())
	if err != nil || caps.Flavor != "gitea" || caps.Version != "1.26.4" {
		t.Errorf("Probe = %+v, %v", caps, err)
	}
}

func TestProbeErrors(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/api/forgejo/v1/version", http.StatusUnauthorized, fx.apiMsg("token is required"))
	_, err := fx.reader.Probe(t.Context())
	wantClass(t, "Probe with 401", err, platform.ClassAuth, nil)

	fx = newFixture(t, "gitea")
	fx.json(http.MethodGet, "/api/forgejo/v1/version", http.StatusNotFound, fx.apiMsg("not found"))
	fx.json(http.MethodGet, "/version", http.StatusBadGateway, fx.apiMsg("bad gateway"))
	_, err = fx.reader.Probe(t.Context())
	wantClass(t, "Probe with 502", err, platform.ClassTransient, nil)
	// A failed probe is not remembered.
	fx.json(http.MethodGet, "/version", http.StatusOK, map[string]any{"version": "1.27.3"})
	if caps, err := fx.reader.Probe(t.Context()); err != nil || caps.Version != "1.27.3" {
		t.Errorf("Probe after a failure = %+v, %v", caps, err)
	}
}

func TestSelf(t *testing.T) {
	fx := newFixture(t, "gitea")
	me := fx.user(3, "tm-writer")
	me["email"] = "writer@example.com"
	fx.json(http.MethodGet, "/user", http.StatusOK, me)
	a, err := fx.writer.Self(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a != (platform.Account{ID: "3", Login: "tm-writer", Email: "writer@example.com", Kind: platform.KindUser}) {
		t.Errorf("Self = %+v", a)
	}
	fx.reset()
	if _, err := fx.writer.Self(t.Context()); err != nil || len(fx.requests("", "")) != 0 {
		t.Errorf("second Self: %v, %d requests", err, len(fx.requests("", "")))
	}
}

func TestSelfRefusesAdministrators(t *testing.T) {
	fx := newFixture(t, "forgejo")
	admin := fx.user(1, "root")
	admin["is_admin"] = true
	fx.json(http.MethodGet, "/user", http.StatusOK, admin)
	_, err := fx.reader.Self(t.Context())
	wantClass(t, "Self of an admin", err, platform.ClassInvalid, nil)
	if err != nil && !strings.Contains(err.Error(), "Sudo") {
		t.Errorf("error %q does not say why", err)
	}
	// Target refuses it too, before any write.
	_, err = fx.writer.Target(t.Context(), platform.Repo{ID: "7", Path: "acme/api"}, platform.Perms{Contents: true, PRs: true})
	wantClass(t, "Target of an admin", err, platform.ClassInvalid, nil)
}

func TestSelfErrors(t *testing.T) {
	s := newAPIServer(t)
	anon, err := NewReader(s.provider("gitea"), auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = anon.Self(t.Context())
	wantClass(t, "anonymous Self", err, platform.ClassAuth, nil)

	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/user", http.StatusForbidden, fx.apiMsg("You must change your password. Change it at: "+fx.base()+"/user/change_password"))
	_, err = fx.reader.Self(t.Context())
	wantClass(t, "Self of a user who must change the password", err, platform.ClassPermission, nil)
	if err != nil && !strings.Contains(err.Error(), "must change your password") {
		t.Errorf("error %q lacks the platform's message", err)
	}
}

func TestLookup(t *testing.T) {
	fx := newFixture(t, "gitea")
	fx.json(http.MethodGet, "/users/tm-writer", http.StatusOK, fx.user(3, "tm-writer"))
	fx.json(http.MethodGet, "/users/nobody", http.StatusNotFound, fx.apiMsg("user redirect does not exist [name: nobody]"))
	a, err := fx.reader.Lookup(t.Context(), "tm-writer")
	if err != nil || a.ID != "3" || a.Login != "tm-writer" || a.Kind != platform.KindUser {
		t.Errorf("Lookup = %+v, %v", a, err)
	}
	_, err = fx.reader.Lookup(t.Context(), "nobody")
	wantClass(t, "Lookup of a missing login", err, platform.ClassNotFound, platform.ErrNotFound)
	for _, bad := range []string{"", "a/b", ".."} {
		_, err := fx.reader.Lookup(t.Context(), bad)
		wantClass(t, "Lookup of "+bad, err, platform.ClassNotFound, platform.ErrNotFound)
	}
}

func TestToAccountKinds(t *testing.T) {
	for _, tc := range []struct {
		u    *apiUser
		want platform.AccountKind
	}{
		{&apiUser{ID: 5, Login: "jdoe"}, platform.KindUser},
		{&apiUser{ID: -2, Login: "gitea-actions"}, platform.KindBot},
		{&apiUser{ID: -2, Login: "forgejo-actions"}, platform.KindBot},
		{&apiUser{ID: -1, Login: "Ghost"}, platform.KindUnknown},
	} {
		if got := toAccount(tc.u); got.Kind != tc.want || got.Login != tc.u.Login {
			t.Errorf("toAccount(%+v) = %+v, want kind %v", tc.u, got, tc.want)
		}
	}
	if got := toAccount(nil); got != (platform.Account{}) {
		t.Errorf("toAccount(nil) = %+v", got)
	}
}

// Forgejo quotes a token it does not know in its 401 message: the driver
// masks it.
func TestErrorsMaskTheToken(t *testing.T) {
	fx := newFixture(t, "forgejo")
	fx.json(http.MethodGet, "/user", http.StatusUnauthorized, map[string]any{
		"message": "access token does not exist [sha: " + fx.token + "]\ntask with token \"" + fx.token + "\": resource does not exist",
		"url":     fx.base() + "/api/swagger", "errors": []string{},
	})
	_, err := fx.reader.Self(t.Context())
	wantClass(t, "Self with a bad token", err, platform.ClassAuth, nil)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), fx.token) {
		t.Errorf("the error shows the token: %s", err)
	}
	if !strings.Contains(err.Error(), "access token does not exist") || strings.ContainsAny(err.Error(), "\r\n") {
		t.Errorf("the error lost the platform's message or spans lines: %q", err)
	}
	var pe *platform.Error
	if !errors.As(err, &pe) || pe.Status != http.StatusUnauthorized || pe.Op != "get account" {
		t.Errorf("error %#v", err)
	}
}
