package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// items returns n numbered items for a listing.
func items(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"id": i + 1}
	}
	return out
}

type idItem struct {
	ID int `json:"id"`
}

func TestListAllPagination(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style pageStyle
		n     int
	}{
		{"offset", offset, 250},
		// Above 10 000 rows GitLab drops X-Total and rel="last": the
		// listing must go on by rel="next" and X-Next-Page alone.
		{"offset without totals", offsetLarge, 250},
		{"keyset", keyset, 250},
		{"keyset, exact pages", keyset, 200},
		{"no headers", bare, 250},
		{"no headers, exact pages", bare, 200},
		{"empty", offset, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.pagesWith("/things", items(tc.n), tc.style)
			var got []int
			complete, err := listAll(t.Context(), fx.reader.c, "list", fx.reader.c.endpoint("things"), url.Values{"a": {"b"}}, 10,
				func(it idItem) error { got = append(got, it.ID); return nil })
			if err != nil || !complete {
				t.Fatalf("complete %v, err %v", complete, err)
			}
			if len(got) != tc.n {
				t.Fatalf("%d items, want %d", len(got), tc.n)
			}
			for i, id := range got {
				if id != i+1 {
					t.Fatalf("item %d is %d", i, id)
				}
			}
			for _, c := range fx.requests(http.MethodGet, "/things") {
				if c.Query.Get("per_page") != "100" || c.Query.Get("a") != "b" {
					t.Errorf("query %v lost per_page or a filter", c.Query)
				}
			}
		})
	}
}

func TestListAllCapAndLaterPage(t *testing.T) {
	fx := newFixture(t)
	fx.pages("/things", items(350))
	complete, err := listAll(t.Context(), fx.reader.c, "list", fx.reader.c.endpoint("things"), nil, 2,
		func(idItem) error { return nil })
	if err != nil || complete {
		t.Errorf("a capped listing: complete %v, err %v", complete, err)
	}

	fx = newFixture(t)
	fx.handle(http.MethodGet, "/things", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusInternalServerError, msg("500 Internal Server Error"))
			return
		}
		servePage(w, r, items(250), offset)
	})
	_, err = listAll(t.Context(), fx.reader.c, "list", fx.reader.c.endpoint("things"), nil, 10, func(idItem) error { return nil })
	if !laterPage(err) || platform.ClassOf(err) != platform.ClassTransient {
		t.Errorf("a failed second page: %v (later %v)", err, laterPage(err))
	}
}

// TestNextPageForeignLink: only the query of a Link is taken; the request
// stays on the API's origin even when GitLab builds links for another host
// (an external_url behind a proxy).
func TestNextPageForeignLink(t *testing.T) {
	h := http.Header{}
	h.Set("Link", `<http://evil.example/api/v4/things?page=2&per_page=100>; rel="next", <http://evil.example/api/v4/things?page=1>; rel="first"`)
	next, ok := nextPage(h, url.Values{"page": {"1"}})
	if !ok || next.Get("page") != "2" {
		t.Errorf("nextPage = %v, %v", next, ok)
	}
	h = http.Header{}
	h.Set("Link", `<http://x/api/v4/things?page=1>; rel="first"`)
	h.Set("X-Next-Page", "")
	if next, ok := nextPage(h, url.Values{}); !ok || next != nil {
		t.Errorf("last page: %v, %v", next, ok)
	}
	h = http.Header{}
	h.Set("X-Next-Page", "3")
	if next, ok := nextPage(h, url.Values{"x": {"y"}}); !ok || next.Get("page") != "3" || next.Get("x") != "y" {
		t.Errorf("X-Next-Page: %v, %v", next, ok)
	}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"acme/api":             "acme%2Fapi",
		"group/sub/my.project": "group%2Fsub%2Fmy%2Eproject",
		"docs/guide/intro.md":  "docs%2Fguide%2Fintro%2Emd",
		"touchmark/acme-eng":   "touchmark%2Facme-eng",
		"a b?#%":               "a%20b%3F%23%25",
	} {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorClasses(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		body   any
		class  platform.Class
		rule   string
		retry  bool
		notFnd bool
	}{
		{"401", 401, nil, msg("401 Unauthorized"), platform.ClassAuth, "", false, false},
		{"401 revoked", 401, nil, map[string]any{"error": "invalid_token", "error_description": "Token was revoked."}, platform.ClassAuth, "invalid_token", false, false},
		{"403", 403, nil, msg("403 Forbidden"), platform.ClassPermission, "", false, false},
		{"403 scope", 403, nil, map[string]any{"error": "insufficient_scope",
			"error_description": "The request requires higher privileges than provided by the access token.", "scope": "api"},
			platform.ClassAuth, "insufficient_scope", false, false},
		{"403 limited", 403, map[string]string{"RateLimit-Remaining": "0", "RateLimit-Reset": "60"}, msg("403 Forbidden"), platform.ClassRateLimited, "", true, false},
		{"403 archived", 403, nil, msg("403 Forbidden - Project is archived"), platform.ClassPermission, "archived", false, false},
		// A banned IP (Rack::Attack's blocklist answers "Forbidden" in plain
		// text, without rate-limit headers): a permission error without a
		// rule, which the core's breaker counts.
		{"403 plain ban", 403, nil, "Forbidden\n", platform.ClassPermission, "", false, false},
		{"404", 404, nil, msg("404 Project Not Found"), platform.ClassNotFound, "", false, true},
		{"409", 409, nil, msg([]string{"Another open merge request already exists for this source branch: !5"}), platform.ClassConflict, "", false, false},
		{"400", 400, nil, map[string]any{"error": "target_branch is missing"}, platform.ClassInvalid, "", false, false},
		{"422", 422, nil, msg(map[string]any{"title": []string{"is too long (maximum is 255 characters)"}}), platform.ClassInvalid, "", false, false},
		{"422 disabled", 422, nil, msg([]string{"Target project has disabled merge requests"}), platform.ClassPolicy, "prs-disabled", false, false},
		{"429", 429, map[string]string{"Retry-After": "30", "RateLimit-Remaining": "0"}, msg("Retry later"), platform.ClassRateLimited, "", true, false},
		{"429 reset time", 429, map[string]string{"RateLimit-ResetTime": future}, "Retry later", platform.ClassRateLimited, "", true, false},
		{"429 bare", 429, nil, "Retry later", platform.ClassRateLimited, "", false, false},
		{"500", 500, nil, msg("500 Internal Server Error"), platform.ClassTransient, "", false, false},
		{"502", 502, nil, "<html>Bad Gateway</html>", platform.ClassTransient, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.handle(http.MethodGet, "/thing", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				if s, ok := tc.body.(string); ok {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(s))
					return
				}
				writeJSON(w, tc.status, tc.body)
			})
			_, err := fx.reader.c.get(t.Context(), "read", fx.reader.c.endpoint("thing"), nil, nil)
			var pe *platform.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error %v is no *platform.Error", err)
			}
			if pe.Class != tc.class || pe.Rule != tc.rule || pe.Status != tc.status {
				t.Errorf("class %v rule %q status %d, want %v %q %d (%v)", pe.Class, pe.Rule, pe.Status, tc.class, tc.rule, tc.status, err)
			}
			if tc.retry != (pe.RetryAfter > 0) {
				t.Errorf("RetryAfter %v", pe.RetryAfter)
			}
			if tc.notFnd != errors.Is(err, platform.ErrNotFound) {
				t.Errorf("wraps ErrNotFound: %v", errors.Is(err, platform.ErrNotFound))
			}
			if strings.Contains(err.Error(), fx.token) {
				t.Error("the error holds the token")
			}
		})
	}
}

func TestErrorMasksToken(t *testing.T) {
	fx := newFixture(t)
	fx.handle(http.MethodGet, "/thing", func(w http.ResponseWriter, r *http.Request) {
		// A server that echoes the credential must not leak it.
		writeJSON(w, http.StatusBadRequest, msg("bad token "+r.Header.Get("Private-Token")))
	})
	_, err := fx.reader.c.get(t.Context(), "read", fx.reader.c.endpoint("thing"), nil, nil)
	if err == nil || strings.Contains(err.Error(), fx.token) {
		t.Errorf("error %v", err)
	}
}

func TestTransportErrors(t *testing.T) {
	fx := newFixture(t)
	fx.handle(http.MethodGet, "/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := fx.reader.c.get(ctx, "read", fx.reader.c.endpoint("slow"), nil, nil)
	wantClass(t, "a timeout", err, platform.ClassTransient, nil)

	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	_, err = fx.reader.c.get(ctx, "read", fx.reader.c.endpoint("slow"), nil, nil)
	if !errors.Is(err, context.Canceled) || platform.ClassOf(err) != platform.ClassUnknown {
		t.Errorf("a canceled request: %v, class %v", err, platform.ClassOf(err))
	}

	fx.handle(http.MethodGet, "/garbled", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
	})
	var out map[string]any
	_, err = fx.reader.c.get(t.Context(), "read", fx.reader.c.endpoint("garbled"), nil, &out)
	wantClass(t, "a garbled body", err, platform.ClassUnknown, nil)
}

func TestRetryAfter(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		h    map[string]string
		want time.Duration
	}{
		{map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{map[string]string{"Retry-After": at.Add(time.Minute).UTC().Format(http.TimeFormat)}, time.Minute},
		{map[string]string{"RateLimit-Reset": strconv.FormatInt(at.Add(2*time.Minute).Unix(), 10)}, 2 * time.Minute},
		{map[string]string{"RateLimit-Reset": "45"}, 45 * time.Second},
		{map[string]string{"RateLimit-ResetTime": at.Add(3 * time.Minute).UTC().Format(http.TimeFormat)}, 3 * time.Minute},
		{map[string]string{"Retry-After": "999999999"}, maxRetryAfter},
		{map[string]string{"Retry-After": "-5"}, 0},
		{map[string]string{"RateLimit-Reset": strconv.FormatInt(at.Add(-time.Minute).Unix(), 10)}, 0},
		{nil, 0},
	} {
		h := http.Header{}
		for k, v := range tc.h {
			h.Set(k, v)
		}
		if got := retryAfter(h, at); got != tc.want {
			t.Errorf("retryAfter(%v) = %v, want %v", tc.h, got, tc.want)
		}
	}
}

// TestRedirectFollowed: a GET follows a same-origin redirect below the API
// base (a proxy's), never one elsewhere.
func TestRedirectFollowed(t *testing.T) {
	fx := newFixture(t)
	fx.handle(http.MethodGet, "/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v4/new?x=1", http.StatusMovedPermanently)
	})
	fx.json(http.MethodGet, "/new", http.StatusOK, map[string]any{"ok": true})
	fx.handle(http.MethodGet, "/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example/api/v4/new", http.StatusFound)
	})
	var out map[string]any
	if _, err := fx.reader.c.get(t.Context(), "read", fx.reader.c.endpoint("old"), nil, &out); err != nil || out["ok"] != true {
		t.Errorf("same-origin redirect: %v, %v", out, err)
	}
	if calls := fx.requests(http.MethodGet, "/new"); len(calls) != 1 || calls[0].Token != fx.token {
		t.Errorf("the redirect target got %d requests", len(calls))
	}
	if _, err := fx.reader.c.get(t.Context(), "read", fx.reader.c.endpoint("away"), nil, &out); err == nil {
		t.Error("a redirect to another host was followed")
	}
}
