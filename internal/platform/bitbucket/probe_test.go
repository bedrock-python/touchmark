package bitbucket

import (
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

func TestProbe(t *testing.T) {
	f := newFixture(t)
	caps, err := f.reader.Probe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Caps{
		Flavor: "bitbucket", MaxBody: 60000, Draft: platform.DraftNative, CloserKnown: true, NoLabels: true, ClosedImmutable: true,
		Marker: platform.MarkerInRefDef, RuntimeOnly: []string{"branch-restrictions"},
		Limits: platform.Limits{Reads: 2, GitReads: 2, ReadsPerMinute: 15, MinInterval: time.Second},
	}
	if caps.Flavor != want.Flavor || caps.MaxBody != want.MaxBody || caps.Draft != want.Draft || !caps.CloserKnown ||
		caps.Marker != want.Marker || !slices.Equal(caps.RuntimeOnly, want.RuntimeOnly) || caps.Limits != want.Limits ||
		caps.LabelsByID || caps.QuickActions || caps.WorkflowPerm || caps.Commit.API || !caps.NoLabels || !caps.ClosedImmutable ||
		caps.BodyControls() {
		t.Errorf("Probe = %+v\nwant %+v", caps, want)
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("Probe sent %d requests; Bitbucket Cloud has one version", len(calls))
	}
}

func TestSelf(t *testing.T) {
	f := newFixture(t)
	me := account(botUUID, "touchmark bot")
	me["account_status"] = "active"
	me["has_2fa_enabled"] = nil
	me["is_staff"] = false
	me["created_on"] = "2026-09-01T10:00:00.000000+00:00"
	f.json("/user", http.StatusOK, me)
	for range 2 {
		a, err := f.reader.Self(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if a != (platform.Account{ID: botUUID, Login: botUUID, Kind: platform.KindUser}) {
			t.Errorf("Self = %+v; want the uuid as id and login, a user", a)
		}
	}
	calls := f.requests(http.MethodGet, "/user")
	if len(calls) != 1 {
		t.Fatalf("%d requests of /user; want one, then the cache", len(calls))
	}
	if calls[0].Auth != "Bearer "+f.token {
		t.Errorf("Authorization %q; want the token as a bearer token", calls[0].Auth)
	}
}

func TestSelfAppUser(t *testing.T) {
	f := newFixture(t)
	f.json("/user", http.StatusOK, appUser(botUUID, "touchmark"))
	a, err := f.reader.Self(t.Context())
	if err != nil || a.Kind != platform.KindBot || a.ID != botUUID {
		t.Errorf("Self = %+v, %v; want a bot", a, err)
	}
}

func TestSelfErrors(t *testing.T) {
	t.Run("anonymous", func(t *testing.T) {
		s := newAPIServer(t)
		r, err := NewReader(s.provider(), auth.Credential{}, httpx.New(httpx.Options{}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Self(t.Context())
		wantClass(t, "Self", err, platform.ClassAuth, nil)
	})
	t.Run("refused token", func(t *testing.T) {
		f := newFixture(t)
		f.json("/user", http.StatusUnauthorized, errorBody("Token is invalid, expired, or not supported for this endpoint."))
		_, err := f.reader.Self(t.Context())
		wantClass(t, "Self", err, platform.ClassAuth, nil)
		if err != nil && !strings.Contains(err.Error(), "Token is invalid") {
			t.Errorf("the error %q lacks the API's message", err)
		}
	})
	t.Run("no uuid", func(t *testing.T) {
		f := newFixture(t)
		f.json("/user", http.StatusOK, map[string]any{"type": "user", "nickname": "x"})
		_, err := f.reader.Self(t.Context())
		wantClass(t, "Self", err, platform.ClassUnknown, nil)
	})
}

func TestLookup(t *testing.T) {
	f := newFixture(t)
	f.json("/users/"+uuidPath(personUUID), http.StatusOK, account(personUUID, "Wilson Mendes Neto"))
	f.json("/users/5b57c56fdfe79e2c947cd85f", http.StatusOK, account(personUUID, "Wilson Mendes Neto"))
	f.json("/users/"+uuidPath(otherUUID), http.StatusNotFound, errorBody(otherUUID+" not found"))
	for _, login := range []string{personUUID, "5b57c56fdfe79e2c947cd85f"} {
		a, err := f.reader.Lookup(t.Context(), login)
		if err != nil || a.ID != personUUID || a.Login != personUUID || a.Kind != platform.KindUser {
			t.Errorf("Lookup(%s) = %+v, %v; want the account by its uuid", login, a, err)
		}
	}
	_, err := f.reader.Lookup(t.Context(), otherUUID)
	wantClass(t, "an unknown uuid", err, platform.ClassNotFound, platform.ErrNotFound)
	f.reset()
	for _, login := range []string{"touchmark-bot", "Wilson Mendes Neto", "", "{not-a-uuid}", "../user"} {
		_, err := f.reader.Lookup(t.Context(), login)
		wantClass(t, "Lookup("+login+")", err, platform.ClassInvalid, nil)
		if err != nil && !strings.Contains(err.Error(), "Bitbucket logins are account UUIDs") {
			t.Errorf("Lookup(%q): %v; want the message to say logins are UUIDs", login, err)
		}
	}
	if calls := f.requests("", ""); len(calls) != 0 {
		t.Errorf("a login that is no uuid sent %d requests", len(calls))
	}
}

func TestNewReaderAndWriter(t *testing.T) {
	s := newAPIServer(t)
	client := httpx.New(httpx.Options{})
	tok := auth.Credential{Kind: auth.Token, Token: testToken(t)}
	other := s.provider()
	other.Type = "gitea"
	noAPI := s.provider()
	noAPI.APIURL = ""
	for name, tc := range map[string]struct {
		p    config.ResolvedProvider
		c    auth.Credential
		want string
	}{
		"another type": {other, tok, `type "gitea", not bitbucket`},
		"an app":       {s.provider(), auth.Credential{Kind: auth.App, AppID: "1", AppKey: []byte("k")}, "a GitHub App cannot sign in to Bitbucket"},
		"empty token":  {s.provider(), auth.Credential{Kind: auth.Token}, "an empty token"},
		"no api url":   {noAPI, tok, "api_url is empty"},
	} {
		if _, err := NewReader(tc.p, tc.c, client); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: NewReader: %v; want %q", name, err, tc.want)
		}
	}
	if _, err := NewReader(s.provider(), tok, nil); err == nil {
		t.Error("NewReader without an HTTP client: no error")
	}
	for name, tc := range map[string]struct {
		p    config.ResolvedProvider
		c    auth.Credential
		want string
	}{
		"anonymous": {s.provider(), auth.Credential{}, "the write driver needs a token"},
		"an app":    {s.provider(), auth.Credential{Kind: auth.App, AppID: "1", AppKey: []byte("k")}, "a GitHub App cannot sign in to Bitbucket"},
		"no api":    {noAPI, tok, "api_url is empty"},
	} {
		if w, err := NewWriter(tc.p, tc.c, client); err == nil || w != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: NewWriter = %v, %v; want %q", name, w, err, tc.want)
		}
	}
	if w, err := NewWriter(s.provider(), tok, client); err != nil || w == nil {
		t.Errorf("NewWriter = %v, %v; want a writer", w, err)
	}
}
