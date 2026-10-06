package ghfake

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestJWT(t *testing.T) {
	w := newWorld(t, Options{})
	now := w.clock.Now()
	key := appKey(t)
	sign := func(iss string, iat, exp time.Time) string {
		return must[string](t)(SignJWT(key, iss, iat, exp))
	}
	for _, tc := range []struct {
		name   string
		jwt    string
		status int
		msg    string
	}{
		{"client id", sign(w.app.ClientID, now.Add(-time.Minute), now.Add(9*time.Minute)), 200, ""},
		{"app id", sign(itoa(w.app.ID), now.Add(-time.Minute), now.Add(9*time.Minute)), 200, ""},
		{"exp too far", sign(w.app.ClientID, now.Add(-time.Minute), now.Add(11*time.Minute)), 401, jwtExpTooFar},
		{"expired", sign(w.app.ClientID, now.Add(-20*time.Minute), now.Add(-time.Minute)), 401, jwtExpired},
		{"iat ahead", sign(w.app.ClientID, now.Add(time.Minute), now.Add(5*time.Minute)), 401, jwtIatFuture},
		{"unknown issuer", sign("Iv23liunknown", now.Add(-time.Minute), now.Add(5*time.Minute)), 401, jwtUndecodable},
		{"tampered", sign(w.app.ClientID, now.Add(-time.Minute), now.Add(5*time.Minute)) + "x", 401, jwtUndecodable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := w.call("GET", "/app", tc.jwt, nil)
			wantStatus(t, "GET /app", r, tc.status)
			if tc.status == 200 {
				if got := r.obj(t); got["slug"] != "hub-writer" || got["client_id"] != w.app.ClientID {
					t.Errorf("GET /app: %v", got)
				}
				return
			}
			if got := r.obj(t)["message"]; got != tc.msg {
				t.Errorf("message %q, want %q", got, tc.msg)
			}
		})
	}
	t.Run("installation token", func(t *testing.T) {
		r := w.call("GET", "/app", w.token(nil, nil), nil)
		if r.status != http.StatusForbidden {
			t.Errorf("GET /app with an installation token: HTTP %d", r.status)
		}
	})
}

func TestInstallations(t *testing.T) {
	w := newWorld(t, Options{})
	must[Account](t)(w.s.AddOrg("other", PlanFree))
	jwt := w.jwt()
	r := w.call("GET", "/orgs/acme/installation", jwt, nil)
	wantStatus(t, "org installation", r, 200)
	inst := r.obj(t)
	if field(inst, "id") != float64(w.inst.ID) || field(inst, "account", "login") != "acme" ||
		field(inst, "repository_selection") != "all" || field(inst, "permissions", "workflows") != Write {
		t.Errorf("installation: %v", inst)
	}
	wantStatus(t, "repo installation", w.call("GET", "/repos/acme/api/installation", jwt, nil), 200)
	wantStatus(t, "not installed", w.call("GET", "/orgs/other/installation", jwt, nil), 404)
	wantStatus(t, "user installation", w.call("GET", "/users/alice/installation", jwt, nil), 404)
	wantStatus(t, "list", w.call("GET", "/app/installations", jwt, nil), 200)
	wantStatus(t, "without JWT", w.call("GET", "/orgs/acme/installation", "", nil), 401)
}

func TestAccessTokens(t *testing.T) {
	w := newWorld(t, Options{})
	other := w.repo(RepoSpec{Owner: "acme", Name: "secret", Visibility: "private",
		Files: []File{{Path: "a.txt", Content: []byte("a\n")}}})
	mint := func(body any) reply {
		return w.call("POST", "/app/installations/"+itoa(w.inst.ID)+"/access_tokens", w.jwt(), body)
	}

	t.Run("whole installation", func(t *testing.T) {
		r := mint(nil)
		wantStatus(t, "mint", r, 201)
		got := r.obj(t)
		tok, _ := got["token"].(string)
		if !strings.HasPrefix(tok, "ghs_") || strings.Count(tok, ".") != 2 || len(tok) < 400 {
			t.Errorf("token of %d characters %q…: want the stateless format", len(tok), tok[:min(len(tok), 8)])
		}
		exp, err := time.Parse(time.RFC3339, got["expires_at"].(string))
		if err != nil || !exp.Equal(w.clock.Now().Add(time.Hour)) {
			t.Errorf("expires_at %v, want an hour from now", got["expires_at"])
		}
		if field(got, "permissions", "metadata") != Read || got["repository_selection"] != "all" || got["repositories"] != nil {
			t.Errorf("token: %v", got)
		}
		wantStatus(t, "private repository", w.call("GET", "/repos/acme/secret", tok, nil), 200)
	})

	t.Run("narrowed", func(t *testing.T) {
		r := mint(map[string]any{"repository_ids": []int64{w.api.ID},
			"permissions": map[string]string{"contents": Write, "pull_requests": Write}})
		wantStatus(t, "mint", r, 201)
		got := r.obj(t)
		tok := got["token"].(string)
		if got["repository_selection"] != "selected" || field(got, "repositories", 0, "full_name") != "acme/api" {
			t.Errorf("narrowed token: %v", got)
		}
		if p := got["permissions"].(map[string]any); p["workflows"] != nil || p["metadata"] != Read || p["contents"] != Write {
			t.Errorf("permissions %v", p)
		}
		info, _ := w.s.Token(tok)
		if !slices.Equal(info.Repos, []int64{w.api.ID}) || info.Kind != "installation" {
			t.Errorf("Token: %+v", info)
		}
		// Another private repository of the installation is out of reach,
		// and reaching for it is a violation.
		wantStatus(t, "other repository", w.call("GET", "/repos/acme/secret", tok, nil), 404)
		if v := w.s.Violations(); len(v) != 1 || !strings.HasPrefix(v[0], "token-scope installation/") {
			t.Errorf("violations %q, want one token-scope", v)
		}
		// Workflows were not asked for.
		pr := w.call("POST", "/repos/acme/api/git/refs", tok, map[string]any{"ref": "refs/heads/x", "sha": w.s.Branch("acme/api", "main")})
		wantStatus(t, "create a ref", pr, 201)
		w.noViolations()
	})

	t.Run("by name", func(t *testing.T) {
		r := mint(map[string]any{"repositories": []string{"secret"}})
		wantStatus(t, "mint", r, 201)
		if field(r.obj(t), "repositories", 0, "id") != float64(other.ID) {
			t.Errorf("by name: %s", r.body)
		}
	})

	for _, tc := range []struct {
		name string
		body map[string]any
		msg  string
	}{
		{"more permissions", map[string]any{"permissions": map[string]string{"administration": Write}},
			"The permissions requested are not granted to this installation."},
		{"higher level", map[string]any{"permissions": map[string]string{"metadata": Write}},
			"The permissions requested are not granted to this installation."},
		{"unknown repository", map[string]any{"repository_ids": []int64{99999999}},
			"There is at least one repository that does not exist or is not accessible to the parent installation."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mint(tc.body)
			wantStatus(t, "mint", r, 422)
			if got := r.obj(t)["message"]; got != tc.msg {
				t.Errorf("message %q, want %q", got, tc.msg)
			}
		})
	}

	t.Run("not the App's installation", func(t *testing.T) {
		r := w.call("POST", "/app/installations/12345/access_tokens", w.jwt(), nil)
		wantStatus(t, "mint", r, 404)
	})
}

func TestTokenLifetime(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token([]string{"api"}, nil)
	wantStatus(t, "fresh", w.call("GET", "/repos/acme/api/pulls", tok, nil), 200)

	w.clock.Advance(time.Hour)
	r := w.call("GET", "/repos/acme/api/pulls", tok, nil)
	wantStatus(t, "expired", r, 401)
	if r.obj(t)["message"] != badCredentials {
		t.Errorf("expired: %s", r.body)
	}
	w.noViolations()
	wantStatus(t, "write with an expired token", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "x"}), 401)
	if v := w.s.Violations(); len(v) != 1 || !strings.HasPrefix(v[0], "stale-token installation/") || !strings.Contains(v[0], "expired") {
		t.Errorf("violations %q", v)
	}

	tok = w.token([]string{"api"}, nil)
	wantStatus(t, "revoke", w.call("DELETE", "/installation/token", tok, nil), 204)
	wantStatus(t, "after revoke", w.call("GET", "/repos/acme/api", tok, nil), 401)
	if info, _ := w.s.Token(tok); !info.Revoked {
		t.Errorf("Token: %+v, want revoked", info)
	}
	wantStatus(t, "write after revoke", w.call("POST", "/repos/acme/api/labels", tok, map[string]any{"name": "x"}), 401)
	if v := w.s.Violations(); len(v) != 1 || !strings.Contains(v[0], "revoked") {
		t.Errorf("violations %q", v)
	}
	wantStatus(t, "revoke without a token", w.call("DELETE", "/installation/token", "", nil), 401)
}

func TestShortTokens(t *testing.T) {
	for _, opts := range []Options{{Flavor: GHES}, {ShortTokens: true}} {
		w := newWorld(t, opts)
		if tok := w.token(nil, nil); len(tok) != 40 || !strings.HasPrefix(tok, "ghs_") {
			t.Errorf("%+v: token of %d characters", opts, len(tok))
		}
	}
}

func TestInstallationRepositories(t *testing.T) {
	w := newWorld(t, Options{})
	for _, name := range []string{"b", "c", "d"} {
		w.repo(RepoSpec{Owner: "acme", Name: name, Visibility: "private"})
	}
	tok := w.token(nil, nil)
	r := w.call("GET", "/installation/repositories?per_page=2", tok, nil)
	wantStatus(t, "list", r, 200)
	got := r.obj(t)
	if got["total_count"] != float64(4) || len(got["repositories"].([]any)) != 2 {
		t.Errorf("page 1: %v", got)
	}
	if link := r.header.Get("Link"); !strings.Contains(link, `per_page=2`) || !strings.Contains(link, `rel="next"`) ||
		!strings.Contains(link, `rel="last"`) || strings.Contains(link, `rel="prev"`) {
		t.Errorf("Link %q", link)
	}
	narrow := w.token([]string{"c"}, nil)
	r = w.call("GET", "/installation/repositories", narrow, nil)
	if got := r.obj(t); got["total_count"] != float64(1) || got["repository_selection"] != "selected" {
		t.Errorf("narrowed: %v", got)
	}
	wantStatus(t, "with a JWT", w.call("GET", "/installation/repositories", w.jwt(), nil), 403)
	check(t, w.s.SetInstallationRepos(w.inst.ID, []string{"api"}))
	if got := w.call("GET", "/installation/repositories", tok, nil).obj(t); got["total_count"] != float64(1) {
		t.Errorf("after reselection: %v", got)
	}
}

func TestSuspendedInstallation(t *testing.T) {
	w := newWorld(t, Options{})
	tok := w.token(nil, nil)
	check(t, w.s.SuspendInstallation(w.inst.ID, true))
	wantStatus(t, "token of a suspended installation", w.call("GET", "/repos/acme/api", tok, nil), 401)
	r := w.call("POST", "/app/installations/"+itoa(w.inst.ID)+"/access_tokens", w.jwt(), nil)
	wantStatus(t, "mint", r, 403)
}

func TestTokenMintLimit(t *testing.T) {
	w := newWorld(t, Options{Limits: &Limits{TokenMintsPerHour: 2}})
	path := "/app/installations/" + itoa(w.inst.ID) + "/access_tokens"
	for i := range 2 {
		wantStatus(t, "mint "+itoa(int64(i)), w.call("POST", path, w.jwt(), nil), 201)
	}
	r := w.call("POST", path, w.jwt(), nil)
	wantStatus(t, "third mint", r, 403)
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if u := w.s.Usage("app/hub-writer"); u.TokenMints != 2 {
		t.Errorf("usage %+v", u)
	}
}

func TestPersonalAccessTokens(t *testing.T) {
	w := newWorld(t, Options{})
	check(t, w.s.Grant("acme/api", "alice", "write"))
	classic := must[string](t)(w.s.AddPAT("alice", PATSpec{Scopes: []string{"repo"}}))
	if !strings.HasPrefix(classic, "ghp_") {
		t.Errorf("classic token %q…", classic[:4])
	}
	r := w.call("GET", "/user", classic, nil)
	wantStatus(t, "GET /user", r, 200)
	if r.obj(t)["login"] != "alice" {
		t.Errorf("GET /user: %s", r.body)
	}
	wantStatus(t, "GET /user with an installation token", w.call("GET", "/user", w.token(nil, nil), nil), 403)
	wantStatus(t, "label with write", w.call("POST", "/repos/acme/api/labels", classic, map[string]any{"name": "a"}), 201)
	readOnly := must[string](t)(w.s.AddPAT("bob", PATSpec{Scopes: []string{"repo"}}))
	r = w.call("POST", "/repos/acme/api/labels", readOnly, map[string]any{"name": "b"})
	wantStatus(t, "label without access", r, 403)
	if r.obj(t)["message"] != "Resource not accessible by personal access token" {
		t.Errorf("message: %s", r.body)
	}
	fine := must[string](t)(w.s.AddPAT("alice", PATSpec{FineGrained: true, Repos: []string{"acme/api"},
		Permissions: Permissions{"pull_requests": Read}}))
	if !strings.HasPrefix(fine, "github_pat_") {
		t.Errorf("fine-grained token %q…", fine[:11])
	}
	wantStatus(t, "fine-grained label without the permission",
		w.call("POST", "/repos/acme/api/labels", fine, map[string]any{"name": "c"}), 403)
	check(t, w.s.RevokeToken(fine))
	wantStatus(t, "revoked", w.call("GET", "/repos/acme/api", fine, nil), 401)
}
