package bitbucket

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestCheckIdentity(t *testing.T) {
	f := newWriterFixture(t)
	fs, err := f.writer.Check(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]platform.Finding{}
	for _, x := range fs {
		got[x.Check] = x
	}
	for _, check := range []string{"token-expiry", "scopes", "2fa"} {
		x, ok := got[check]
		if !ok || x.Status != platform.FindingUnknown || x.Repo != "" || x.Detail == "" {
			t.Errorf("%s: %+v; want unknown, with the reason", check, x)
		}
	}
	if len(fs) != 3 || !strings.Contains(got["scopes"].Detail, "write:pullrequest:bitbucket") {
		t.Errorf("findings %+v", fs)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("writes %+v", w)
	}

	s := newAPIServer(t)
	w, err := NewWriter(s.provider(), auth.Credential{Kind: auth.Token, Token: testToken(t)}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	s.json("/user", http.StatusUnauthorized, errorBody("Token is invalid, expired, or not supported for this endpoint."))
	_, err = w.(platform.Checker).Check(t.Context(), nil, nil)
	wantClass(t, "Check with a refused token", err, platform.ClassAuth, nil)
}

func TestCheckRepository(t *testing.T) {
	for name, tc := range map[string]struct {
		perm   string
		status int // of the permission listing, 0 for 200
		want   platform.FindingStatus
		detail string
	}{
		"write":   {permWrite, 0, platform.FindingOK, "may push"},
		"admin":   {permAdmin, 0, platform.FindingWarn, "administers"},
		"read":    {permRead, 0, platform.FindingFail, "read permission"},
		"none":    {"", 0, platform.FindingFail, "none permission"},
		"refused": {"", http.StatusForbidden, platform.FindingUnknown, "403"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			f.repoRoute()
			if tc.status != 0 {
				f.json(permsPath, tc.status, errorBody("The requesting user does not have access to the workspace."))
			} else {
				f.permRoute(tc.perm)
			}
			fs, err := f.writer.Check(t.Context(), []platform.Repo{apiRepoFixture}, []string{syncBranch})
			if err != nil {
				t.Fatal(err)
			}
			if len(fs) != 2 {
				t.Fatalf("findings %+v", fs)
			}
			access, rules := fs[0], fs[1]
			if access.Check != "access" || access.Repo != "acme/api" || access.Status != tc.want || !strings.Contains(access.Detail, tc.detail) {
				t.Errorf("access %+v; want %s with %q", access, tc.want, tc.detail)
			}
			if rules.Check != "rules" || rules.Repo != "acme/api" || rules.Status != platform.FindingUnknown ||
				rules.Detail != "branch restrictions need admin to read; a push meets them" {
				t.Errorf("rules %+v", rules)
			}
		})
	}
}

func TestCheckRepositoryErrors(t *testing.T) {
	t.Run("hidden", func(t *testing.T) {
		f := newWriterFixture(t)
		f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusNotFound, errorBody(noRepoMessage))
		fs, err := f.writer.Check(t.Context(), []platform.Repo{apiRepoFixture}, nil)
		if err != nil || len(fs) != 1 || fs[0].Check != "access" || fs[0].Status != platform.FindingFail {
			t.Errorf("Check = %+v, %v; want access failing", fs, err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		f := newWriterFixture(t)
		f.json("/repositories/acme/"+uuidPath(repoUUID), http.StatusInternalServerError, errorBody("Something went wrong"))
		fs, err := f.writer.Check(t.Context(), []platform.Repo{apiRepoFixture}, nil)
		if err != nil || len(fs) != 2 || fs[0].Status != platform.FindingUnknown || fs[1].Check != "rules" {
			t.Errorf("Check = %+v, %v; want access unknown", fs, err)
		}
	})
	for name, status := range map[string]int{"rate limited": http.StatusTooManyRequests, "refused token": http.StatusUnauthorized} {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			f.repoRoute()
			f.json(permsPath, status, errorBody("no"))
			_, err := f.writer.Check(t.Context(), []platform.Repo{apiRepoFixture}, nil)
			if err == nil {
				t.Error("Check: no error; a rate limit or a refused credential fails the call")
			}
		})
	}
}
