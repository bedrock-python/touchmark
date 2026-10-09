package bitbucketdc

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// properties is GET /application-properties.
func properties(version string) map[string]any {
	return map[string]any{"version": version, "buildNumber": "8019003", "buildDate": "1724900000000", "displayName": "Bitbucket"}
}

func TestProbe(t *testing.T) {
	f := newFixture(t)
	f.json("/application-properties", http.StatusOK, properties("8.19.3"))
	caps, err := f.reader.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case caps.Flavor != "bitbucket-datacenter" || caps.Version != "8.19.3":
		t.Errorf("flavor %q, version %q", caps.Flavor, caps.Version)
	case caps.Marker != platform.MarkerInRefDef || caps.MaxBody != maxBody || caps.MaxBody >= 32768:
		t.Errorf("marker %v, max body %d", caps.Marker, caps.MaxBody)
	case !caps.NoLabels || !caps.CloserKnown || !caps.ClosedImmutable || caps.Draft != platform.DraftNative:
		t.Errorf("caps = %+v", caps)
	case !slices.Contains(caps.RuntimeOnly, branchPermissions):
		t.Errorf("runtime-only checks %v lack %s", caps.RuntimeOnly, branchPermissions)
	case caps.Limits.ReadsPerMinute != 120 || caps.Commit.API:
		t.Errorf("limits %+v, API commits %v", caps.Limits, caps.Commit.API)
	case caps.BodyControls():
		t.Error("descriptions offer tick boxes on a platform that escapes HTML")
	}
	for _, v := range []string{"10.2.1", "9.4.0", "8.20.0", "11.0"} {
		f.json("/application-properties", http.StatusOK, properties(v))
		if _, err := f.reader.Probe(t.Context()); err != nil {
			t.Errorf("Probe on %s: %v", v, err)
		}
	}
	for _, v := range []string{"8.18.2", "7.21.20", "8.5"} {
		f.json("/application-properties", http.StatusOK, properties(v))
		_, err := f.reader.Probe(t.Context())
		wantClass(t, "Probe on "+v, err, platform.ClassUnsupported, nil)
	}
	for _, v := range []string{"", "latest", "v8.19"} {
		f.json("/application-properties", http.StatusOK, properties(v))
		_, err := f.reader.Probe(t.Context())
		wantClass(t, "Probe on version "+v, err, platform.ClassUnknown, nil)
	}
	if calls := f.requests(http.MethodGet, "/application-properties"); len(calls) == 0 || calls[0].Auth != "Bearer "+f.token {
		t.Errorf("the probe sent %q", calls[0].Auth)
	}
}

func TestSelf(t *testing.T) {
	f := newFixture(t)
	f.json("/application-properties", http.StatusOK, properties("9.4.0"))
	f.pages("/users", 1000, []any{
		user(99, "touchmark.reader2", "touchmark.reader2", "NORMAL"),
		user(readerID, "touchmark.reader", "touchmark.reader", "NORMAL"),
	})
	a, err := f.reader.Self(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Account{ID: "11", Login: "touchmark.reader", Email: "touchmark.reader@example.com", Kind: platform.KindUser}
	if a != want {
		t.Errorf("Self = %+v, want %+v", a, want)
	}
	if q := f.requests(http.MethodGet, "/users")[0].Query; q.Get("filter") != "touchmark.reader" {
		t.Errorf("the users query %v", q)
	}
	if _, err := f.reader.Self(t.Context()); err != nil || len(f.requests(http.MethodGet, "/users")) != 1 {
		t.Errorf("Self is not cached: %v", err)
	}
	if f.reader.c.nameOf("11") != "touchmark.reader" {
		t.Error("Self does not remember the user's name")
	}
}

func TestSelfFailures(t *testing.T) {
	// No X-AUSERNAME: the token is refused, or a proxy drops the header.
	f := newFixture(t)
	f.username = ""
	f.json("/application-properties", http.StatusOK, properties("9.4.0"))
	_, err := f.reader.Self(t.Context())
	wantClass(t, "an answer without X-AUSERNAME", err, platform.ClassUnknown, nil)
	if err != nil && !strings.Contains(err.Error(), "X-AUSERNAME") {
		t.Errorf("the error %q does not say what is missing", err)
	}

	// A name sent percent-encoded is read unescaped.
	f = newFixture(t)
	f.username = "jane%40example.com"
	f.json("/application-properties", http.StatusOK, properties("9.4.0"))
	f.pages("/users", 1000, []any{user(personID, "jane@example.com", "jane_example.com", "NORMAL")})
	if a, err := f.reader.Self(t.Context()); err != nil || a.ID != "13" {
		t.Errorf("Self with an encoded name = %+v, %v", a, err)
	}

	// A name no user has.
	f = newFixture(t)
	f.json("/application-properties", http.StatusOK, properties("9.4.0"))
	f.pages("/users", 1000, []any{user(99, "touchmark.reader2", "touchmark.reader2", "NORMAL")})
	_, err = f.reader.Self(t.Context())
	wantClass(t, "a name no user has", err, platform.ClassUnknown, nil)

	// A service user without an address: a project token's.
	f = newFixture(t)
	f.username = "project_1_bot"
	f.json("/application-properties", http.StatusOK, properties("9.4.0"))
	bot := user(serviceID, "project_1_bot", "project_1_bot", "SERVICE")
	delete(bot, "emailAddress")
	f.pages("/users", 1000, []any{bot})
	a, err := f.reader.Self(t.Context())
	if err != nil || a.Kind != platform.KindBot || a.Email != "" || a.ID != "14" {
		t.Errorf("Self of a project token = %+v, %v", a, err)
	}

	// Anonymous.
	s := newAPIServer(t)
	r, err := NewReader(s.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Self(t.Context())
	wantClass(t, "an anonymous Self", err, platform.ClassAuth, nil)
	if len(s.requests("", "")) != 0 {
		t.Error("an anonymous Self sent requests")
	}
}

func TestLookup(t *testing.T) {
	f := newFixture(t)
	f.json("/users/jane.doe", http.StatusOK, user(personID, "Jane.Doe", "jane.doe", "NORMAL"))
	f.json("/users/gone", http.StatusNotFound, errorBody("com.atlassian.bitbucket.user.NoSuchUserException", "User gone does not exist."))
	a, err := f.reader.Lookup(t.Context(), "jane.doe")
	if err != nil || a.ID != "13" || a.Login != "jane.doe" || a.Kind != platform.KindUser || a.Email != "" {
		t.Errorf("Lookup = %+v, %v", a, err)
	}
	if f.reader.c.nameOf("13") != "Jane.Doe" {
		t.Error("Lookup does not remember the user's name")
	}
	_, err = f.reader.Lookup(t.Context(), "gone")
	wantClass(t, "a missing user", err, platform.ClassNotFound, platform.ErrNotFound)
	f.reset()
	for _, login := range []string{"", "a/b", "..", "x?y"} {
		_, err := f.reader.Lookup(t.Context(), login)
		wantClass(t, "Lookup("+login+")", err, platform.ClassInvalid, nil)
	}
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("invalid logins sent %d requests", n)
	}
}

func TestToAccount(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		kind platform.AccountKind
	}{{"NORMAL", platform.KindUser}, {"SERVICE", platform.KindBot}, {"", platform.KindUnknown}, {"ROBOT", platform.KindUnknown}} {
		a := toAccount(&apiUser{ID: 5, Slug: "x", Type: tc.typ})
		if a.Kind != tc.kind || a.ID != "5" || a.Login != "x" {
			t.Errorf("type %q: %+v", tc.typ, a)
		}
	}
	if a := toAccount(&apiUser{Slug: "x"}); a.ID != "" {
		t.Errorf("a user without an id = %+v", a)
	}
	if a := toAccount(nil); a.ID != "" {
		t.Errorf("no user = %+v", a)
	}
}

func TestNewClient(t *testing.T) {
	s := newAPIServer(t)
	p := s.provider()
	hc := httpx.New(httpx.Options{})
	if _, err := NewWriter(p, auth.Credential{}, hc); err == nil {
		t.Error("an anonymous writer")
	}
	if _, err := NewReader(p, auth.Credential{Kind: auth.App, AppID: "1"}, hc); err == nil {
		t.Error("an app credential")
	}
	other := p
	other.Type = "bitbucket"
	if _, err := NewReader(other, auth.Credential{}, hc); err == nil {
		t.Error("a provider of another type")
	}
	bad := p
	bad.APIURL = "ftp://example.com"
	if _, err := NewReader(bad, auth.Credential{}, hc); err == nil {
		t.Error("an api_url that is not http")
	}
}
