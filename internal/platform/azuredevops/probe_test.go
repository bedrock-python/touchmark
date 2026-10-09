package azuredevops

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// connJSON is connectionData as the API answers it for identity id.
func connJSON(id, descriptor, account string) map[string]any {
	u := map[string]any{"id": id, "descriptor": descriptor, "providerDisplayName": "touchmark bot", "isActive": true,
		"properties": map[string]any{"Account": map[string]any{"$type": "System.String", "$value": account}}, "resourceVersion": 2}
	return map[string]any{"authenticatedUser": u, "authorizedUser": u, "instanceId": "6fcc92e5-73a7-4f88-8d13-d9045b45fb27",
		"deploymentType": "hosted", "locationServiceData": map[string]any{"serviceOwner": "00025394-6065-48ca-87d9-7f5672854ef7"}}
}

func TestProbe(t *testing.T) {
	s := newAPIServer(t)
	c, err := newTestReader(t, s, "").Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case c.Flavor != "azure-devops", c.MaxBody != 4000, c.Draft != platform.DraftNative,
		!c.CloserKnown, !c.ClosedImmutable, c.NoLabels, c.LabelsByID, c.WorkflowPerm, c.QuickActions,
		c.Marker != platform.MarkerInProperties, !c.BodyControls():
		t.Errorf("Probe = %+v", c)
	}
	if !slices.Equal(c.RuntimeOnly, []string{"branch-policies", "push-policies"}) || c.Limits.Reads == 0 || c.Limits.ReadsPerMinute == 0 {
		t.Errorf("RuntimeOnly %v, Limits %+v", c.RuntimeOnly, c.Limits)
	}
	if len(s.requests("", "")) != 0 {
		t.Error("Probe asked the API")
	}
}

func TestSelf(t *testing.T) {
	s := newAPIServer(t)
	token := testToken(t)
	s.json(apisPath("connectionData"), http.StatusOK, connJSON(strings.ToUpper(botID), "Microsoft.IdentityModel.Claims.ClaimsIdentity;x\\bot@acme.example", "bot@acme.example"))
	r := newTestReader(t, s, token)
	a, err := r.Self(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != botID || a.Login != botID || a.Email != "bot@acme.example" || a.Kind != platform.KindUser {
		t.Errorf("Self = %+v", a)
	}
	if _, err := r.Self(context.Background()); err != nil || len(s.requests("", "")) != 1 {
		t.Errorf("Self again: %v, %d requests", err, len(s.requests("", "")))
	}
	// The REST API gets Basic with an empty user name.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
	if got := s.requests("", "")[0].Auth; got != want {
		t.Errorf("Authorization %q, want Basic :<token>", got)
	}

	// A token Azure DevOps takes for anonymous access is refused.
	s2 := newAPIServer(t)
	s2.json(apisPath("connectionData"), http.StatusOK, connJSON(publicAccess, "System:PublicAccess;"+publicAccess, "Anonymous"))
	_, err = newTestReader(t, s2, token).Self(context.Background())
	wantClass(t, "public access", err, platform.ClassAuth)

	// An anonymous reader has no identity.
	_, err = newTestReader(t, newAPIServer(t), "").Self(context.Background())
	wantClass(t, "anonymous", err, platform.ClassAuth)
}

func TestLookup(t *testing.T) {
	s := newAPIServer(t)
	s.handle(http.MethodGet, apisPath("identities"), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("identityIds") != userID {
			writeJSON(w, http.StatusOK, collection())
			return
		}
		writeJSON(w, http.StatusOK, collection(map[string]any{
			"id": userID, "descriptor": "Microsoft.IdentityModel.Claims.ClaimsIdentity;x\\dev@acme.example",
			"subjectDescriptor": "aad.MDhjMzdjMGQtZGM2Yy03NjM1LTk2NjItMWZhOGRhZDkwODc4", "providerDisplayName": "Dev",
			"isActive": true, "properties": map[string]any{"Account": map[string]any{"$type": "System.String", "$value": "dev@acme.example"}},
		}))
	})
	r := newTestReader(t, s, testToken(t))
	a, err := r.Lookup(context.Background(), userID)
	if err != nil || a.ID != userID || a.Login != userID || a.Kind != platform.KindUser {
		t.Errorf("Lookup = %+v, %v", a, err)
	}
	_, err = r.Lookup(context.Background(), botID)
	if !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("an unknown id: %v", err)
	}
	for _, login := range []string{"dev@acme.example", "Dev", "{" + userID + "}"} {
		_, err := r.Lookup(context.Background(), login)
		wantClass(t, login, err, platform.ClassInvalid)
		if err != nil && !strings.Contains(err.Error(), "identity ids (GUIDs)") {
			t.Errorf("%s: %v", login, err)
		}
	}
}

// TestKinds: descriptors tell people from automation.
func TestKinds(t *testing.T) {
	for _, tc := range []struct {
		subject, descriptor string
		want                platform.AccountKind
	}{
		{"svc.NmZjYzkyZTU", "", platform.KindBot},
		{"aadsp.MDA0", "", platform.KindServiceAccount},
		{"aad.MDA0", "", platform.KindUser},
		{"msa.MDA0", "", platform.KindUser},
		{"", "Microsoft.TeamFoundation.ServiceIdentity;x", platform.KindBot},
		{"vssgp.Uy0x", "", platform.KindUnknown},
	} {
		if got := kindOf(tc.subject, tc.descriptor, false); got != tc.want {
			t.Errorf("kindOf(%q, %q) = %v, want %v", tc.subject, tc.descriptor, got, tc.want)
		}
	}
	if kindOf("aad.x", "", true) != platform.KindUnknown {
		t.Error("a group is a user")
	}
}

func TestNewClient(t *testing.T) {
	p := config.ResolvedProvider{Provider: config.Provider{ID: "ado", Type: "azure-devops", URL: "https://dev.azure.com/acme"},
		Host: "dev.azure.com", APIURL: "https://dev.azure.com/acme"}
	c, err := newClient(p, auth.Credential{Kind: auth.Token, Token: "t0k3n-0123456789abcdef"}, httpClient())
	if err != nil {
		t.Fatal(err)
	}
	if c.org != "acme" || c.vssps != "https://vssps.dev.azure.com/acme" || !slices.Contains(c.auth.Hosts, "vssps.dev.azure.com") {
		t.Errorf("client: org %q, vssps %q, hosts %v", c.org, c.vssps, c.auth.Hosts)
	}
	for _, bad := range []struct {
		p    config.ResolvedProvider
		cred auth.Credential
	}{
		{config.ResolvedProvider{Provider: config.Provider{ID: "x", Type: "github", URL: "https://github.com"}, APIURL: "https://api.github.com"}, auth.Credential{}},
		{config.ResolvedProvider{Provider: config.Provider{ID: "x", Type: "azure-devops", URL: "https://dev.azure.com"}, APIURL: "https://dev.azure.com"}, auth.Credential{}},
		{p, auth.Credential{Kind: auth.App, AppID: "1"}},
	} {
		if _, err := newClient(bad.p, bad.cred, httpClient()); err == nil {
			t.Errorf("newClient(%+v) accepted", bad.p.Provider)
		}
	}
	if _, err := NewWriter(p, auth.Credential{}, httpClient()); err == nil {
		t.Error("an anonymous writer")
	}
}
