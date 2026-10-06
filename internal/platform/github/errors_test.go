package github

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestStatusErrors: every error class from GitHub's answers,
// with the waits of rate limits and the rules of refusals.
func TestStatusErrors(t *testing.T) {
	reset := strconv.FormatInt(start.Add(90*time.Second).Unix(), 10)
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		body   any
		class  platform.Class
		rule   string
		wait   time.Duration
	}{
		{"bad credentials", 401, nil, ghError("Bad credentials"), platform.ClassAuth, "", 0},
		{"primary limit", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			ghError("API rate limit exceeded for installation ID 7001."), platform.ClassRateLimited, "", 90 * time.Second},
		{"primary limit 429", 429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			ghError("API rate limit exceeded"), platform.ClassRateLimited, "", 90 * time.Second},
		{"secondary with retry-after", 403, map[string]string{"Retry-After": "60"},
			ghError("You have exceeded a secondary rate limit. Please wait a few minutes before you try again."), platform.ClassRateLimited, "", time.Minute},
		{"secondary without headers", 403, nil,
			ghError("You have exceeded a secondary rate limit and have been temporarily blocked from content creation."), platform.ClassRateLimited, "", 0},
		{"abuse detection", 403, nil, ghError("You have triggered an abuse detection mechanism."), platform.ClassRateLimited, "", 0},
		{"a wait over an hour", 429, map[string]string{"Retry-After": "86400"}, ghError("slow down"), platform.ClassRateLimited, "", time.Hour},
		{"not accessible", 403, nil, ghError("Resource not accessible by integration"), platform.ClassPermission, "", 0},
		{"workflows", 403, nil, ghError("refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission"),
			platform.ClassPermission, "workflows", 0},
		{"sso", 403, map[string]string{"X-GitHub-SSO": "required; url=https://github.com/orgs/acme/sso?authorization_request=x"},
			ghError("Resource protected by organization SAML enforcement."), platform.ClassAuth, "sso", 0},
		{"archived", 403, nil, ghError("Repository was archived so is read-only."), platform.ClassPermission, "archived", 0},
		{"GH013", 422, nil, ghError("Repository rule violations found\n\nGH013: Cannot update this protected ref."), platform.ClassPolicy, "GH013", 0},
		{"not found", 404, nil, notFoundBody, platform.ClassNotFound, "", 0},
		{"gone", 410, nil, ghError("Issues are disabled for this repo"), platform.ClassNotFound, "", 0},
		{"conflict", 409, nil, ghError("Git Repository is empty."), platform.ClassConflict, "", 0},
		{"validation", 422, nil, ghError("Validation Failed", map[string]any{"resource": "PullRequest", "code": "custom", "message": "No commits between main and x"}),
			platform.ClassInvalid, "", 0},
		{"server error", 502, nil, "<html>Bad Gateway</html>", platform.ClassTransient, "", 0},
		{"unavailable", 503, map[string]string{"Retry-After": "5"}, ghError("Service Unavailable"), platform.ClassTransient, "", 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{kind: credToken})
			f.handle(http.MethodGet, "/repos/acme/api", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				writeJSON(w, tc.status, tc.body)
			})
			_, err := f.reader.Repo(t.Context(), "acme/api")
			wantClass(t, tc.name, err, tc.class, nil)
			var pe *platform.Error
			if !errors.As(err, &pe) {
				t.Fatalf("%v is no *platform.Error", err)
			}
			if pe.Rule != tc.rule || pe.RetryAfter != tc.wait || pe.Status != tc.status {
				t.Errorf("rule %q, wait %v, status %d; want %q, %v, %d", pe.Rule, pe.RetryAfter, pe.Status, tc.rule, tc.wait, tc.status)
			}
		})
	}
}

// TestErrorsMaskSecrets: a server that echoes the credential in its error
// never gets it into an error message.
func TestErrorsMaskSecrets(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken})
	f.handle(http.MethodGet, "/repos/acme/api", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, ghError("denied for "+r.Header.Get("Authorization")))
	})
	_, err := f.reader.Repo(t.Context(), "acme/api")
	if err == nil || strings.Contains(err.Error(), f.token) {
		t.Fatalf("error %v holds the token", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("error %v: the token is not masked", err)
	}

	// A minted installation token too.
	a := newFixture(t)
	a.handle(http.MethodGet, "/repos/acme/api", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, ghError("denied for "+r.Header.Get("Authorization")))
	})
	_, err = a.reader.Repo(t.Context(), "acme/api")
	tok, _ := a.reader.c.app.ownerToken(t.Context(), "acme")
	if err == nil || strings.Contains(err.Error(), tok) || !strings.Contains(err.Error(), "***") {
		t.Errorf("error %v holds the installation token", err)
	}
}

// TestRESTHeaders: every REST request asks for GitHub's media type and API
// version 2022-11-28, and sends a token as Bearer.
func TestRESTHeaders(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken})
	f.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(101, "acme/api"))
	f.json(http.MethodGet, "/repos/acme/api/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	if _, err := f.reader.Repo(t.Context(), "acme/api"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.requests("", "") {
		if c.Accept != "application/vnd.github+json" || c.Version != "2022-11-28" || c.Auth != "Bearer "+f.token {
			t.Errorf("%s %s: Accept %q, version %q, auth set %v", c.Method, c.Path, c.Accept, c.Version, c.Auth != "")
		}
	}
}

// TestPagination: listings ask for 100 per page and follow the Link
// header's rel="next" (its query), a later failed page makes a listing
// incomplete, a cap too.
func TestPagination(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken})
	f.synthetic = true
	var items []any
	for i := range 250 {
		items = append(items, map[string]any{"type": "rule-" + strconv.Itoa(i)})
	}
	f.pages("/things", items)
	var n int
	complete, err := listAll(t.Context(), f.reader.c, "list", f.reader.c.endpoint("things"), url.Values{"x": {"1"}}, nil, 10, func(r apiRule) error {
		n++
		return nil
	})
	if err != nil || !complete || n != 250 {
		t.Fatalf("complete %v, %d items, %v", complete, n, err)
	}
	calls := f.requests(http.MethodGet, "/things")
	if len(calls) != 3 {
		t.Fatalf("%d pages", len(calls))
	}
	for i, c := range calls {
		if c.Query.Get("per_page") != "100" || c.Query.Get("x") != "1" || i > 0 && c.Query.Get("page") != strconv.Itoa(i+1) {
			t.Errorf("page %d: query %v", i+1, c.Query)
		}
	}
	// Capped.
	complete, err = listAll(t.Context(), f.reader.c, "list", f.reader.c.endpoint("things"), nil, nil, 2, func(apiRule) error { return nil })
	if err != nil || complete {
		t.Errorf("capped: complete %v, %v", complete, err)
	}
	// A later page fails: a pageError the caller can tell.
	f.handle(http.MethodGet, "/broken", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusInternalServerError, ghError("Server Error"))
			return
		}
		w.Header().Set("Link", `<http://`+r.Host+`/api/v3/broken?page=2&per_page=100>; rel="next"`)
		writeJSON(w, http.StatusOK, []any{map[string]any{"type": "a"}})
	})
	_, err = listAll(t.Context(), f.reader.c, "list", f.reader.c.endpoint("broken"), nil, nil, 10, func(apiRule) error { return nil })
	if !laterPage(err) || platform.ClassOf(err) != platform.ClassTransient {
		t.Errorf("later page: %v", err)
	}
}

// TestRedirects: a GET follows a 301 below the API base (a renamed
// repository), never one elsewhere, and never a write.
func TestRedirects(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken})
	f.handle(http.MethodGet, "/repos/acme/old", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+r.Host+"/api/v3/repositories/101")
		writeJSON(w, http.StatusMovedPermanently, map[string]any{"message": "Moved Permanently", "url": "http://" + r.Host + "/api/v3/repositories/101"})
	})
	f.json(http.MethodGet, "/repositories/101", http.StatusOK, repo(101, "acme/new"))
	f.json(http.MethodGet, "/repos/acme/new/hash-algorithm", http.StatusOK, map[string]any{"hash_algorithm": "sha1"})
	r, err := f.reader.Repo(t.Context(), "acme/old")
	if err != nil || r.Path != "acme/new" || r.ID != "101" {
		t.Fatalf("renamed: %+v, %v", r, err)
	}
	for _, c := range f.requests(http.MethodGet, "/repositories/101") {
		if c.Auth != "Bearer "+f.token {
			t.Error("the redirect lost the credential")
		}
	}

	f.handle(http.MethodGet, "/repos/acme/away", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example/api/v3/repositories/101")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	_, err = f.reader.Repo(t.Context(), "acme/away")
	wantClass(t, "a redirect to another host", err, platform.ClassUnknown, nil)
}

// TestGraphQLErrors: GitHub answers limits, refusals and missing objects
// with HTTP 200 and errors; a whole-request error is classified, a field's
// is the caller's.
func TestGraphQLErrors(t *testing.T) {
	f := newFixture(t, fixtureOpts{kind: credToken})
	c := f.reader.c
	a := c.staticAuth("Bearer " + f.token)
	reset := strconv.FormatInt(start.Add(2*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name  string
		write func(w http.ResponseWriter)
		class platform.Class
		wait  time.Duration
	}{
		{"primary limit", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", reset)
			gqlData(w, nil, gqlErr("RATE_LIMITED", "API rate limit exceeded for installation ID 7001."))
		}, platform.ClassRateLimited, 2 * time.Minute},
		{"secondary limit 200", func(w http.ResponseWriter) {
			gqlData(w, nil, map[string]any{"message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."})
		}, platform.ClassRateLimited, 0},
		{"secondary limit 403", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "You have exceeded a secondary rate limit."})
		}, platform.ClassRateLimited, 30 * time.Second},
		{"forbidden", func(w http.ResponseWriter) {
			gqlData(w, nil, gqlErr("FORBIDDEN", "Resource not accessible by integration"))
		}, platform.ClassPermission, 0},
		{"not found", func(w http.ResponseWriter) {
			gqlData(w, nil, gqlErr("NOT_FOUND", "Could not resolve to a node with the global id of 'x'"))
		}, platform.ClassNotFound, 0},
		{"timeout", func(w http.ResponseWriter) {
			gqlData(w, nil, map[string]any{"message": "Something went wrong while executing your query. This may be the result of a timeout."})
		}, platform.ClassTransient, 0},
		{"parse error", func(w http.ResponseWriter) {
			gqlData(w, nil, map[string]any{"message": "Parse error on \"}\" (RCURLY) at [1, 2]"})
		}, platform.ClassInvalid, 0},
		{"bad credentials", func(w http.ResponseWriter) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		}, platform.ClassAuth, 0},
	} {
		f.graphql("query {", func(w http.ResponseWriter, _ gqlCall) { tc.write(w) })
		_, err := c.graphql(t.Context(), "q", a, "query { viewer { login } }", nil, nil)
		wantClass(t, tc.name, err, tc.class, nil)
		var pe *platform.Error
		if errors.As(err, &pe) && pe.RetryAfter != tc.wait {
			t.Errorf("%s: wait %v, want %v", tc.name, pe.RetryAfter, tc.wait)
		}
	}
	// A field error comes back with the data.
	f.graphql("query {", func(w http.ResponseWriter, _ gqlCall) {
		gqlData(w, map[string]any{"r0": nil, "r1": map[string]any{"databaseId": 1}},
			gqlErr("NOT_FOUND", "Could not resolve to a Repository with the name 'acme/x'.", "r0"))
	})
	var data map[string]any
	errs, err := c.graphql(t.Context(), "q", a, "query { r0: x r1: y }", nil, &data)
	if err != nil || len(errs) != 1 || errs.at("r0") == nil || errs.at("r1") != nil || data["r1"] == nil {
		t.Errorf("field errors %v, %v, data %v", errs, err, data)
	}
	wantClass(t, "field error", c.fieldError("q", errs.at("r0")), platform.ClassNotFound, platform.ErrNotFound)

	// Without a credential there is no GraphQL.
	_, err = c.graphql(t.Context(), "q", nil, "query { x }", nil, nil)
	wantClass(t, "anonymous", err, platform.ClassUnsupported, nil)
}
