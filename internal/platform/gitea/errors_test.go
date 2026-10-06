package gitea

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestStatusClasses(t *testing.T) {
	fx := newFixture(t, "gitea")
	for _, tc := range []struct {
		status int
		header map[string]string
		class  platform.Class
		retry  time.Duration
		rule   string
	}{
		{http.StatusBadRequest, nil, platform.ClassInvalid, 0, ""},
		{http.StatusUnauthorized, nil, platform.ClassAuth, 0, ""},
		{http.StatusForbidden, nil, platform.ClassPermission, 0, ""},
		// A proxy in front of the instance may answer a limit with 403: any
		// of the rate-limit headers that says nothing is left, or a
		// Retry-After, makes it one.
		{http.StatusForbidden, map[string]string{"Retry-After": "12"}, platform.ClassRateLimited, 12 * time.Second, ""},
		{http.StatusForbidden, map[string]string{"RateLimit-Remaining": "0", "RateLimit-Reset": "9"}, platform.ClassRateLimited, 9 * time.Second, ""},
		{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "25"}, platform.ClassRateLimited, 25 * time.Second, ""},
		{http.StatusForbidden, map[string]string{"RateLimit": `"baseline";r=0;t=17`}, platform.ClassRateLimited, 17 * time.Second, ""},
		{http.StatusForbidden, map[string]string{"RateLimit-Remaining": "5", "RateLimit-Reset": "9"}, platform.ClassPermission, 0, ""},
		{http.StatusForbidden, map[string]string{"RateLimit": `"baseline";r=1999;t=600`}, platform.ClassPermission, 0, ""},
		{http.StatusNotFound, nil, platform.ClassNotFound, 0, ""},
		{http.StatusMethodNotAllowed, nil, platform.ClassInvalid, 0, ""},
		{http.StatusRequestTimeout, nil, platform.ClassTransient, 0, ""},
		{http.StatusConflict, nil, platform.ClassConflict, 0, ""},
		{http.StatusPreconditionFailed, nil, platform.ClassConflict, 0, ""},
		{http.StatusRequestEntityTooLarge, nil, platform.ClassInvalid, 0, ""},
		{http.StatusUnprocessableEntity, nil, platform.ClassInvalid, 0, ""},
		{http.StatusLocked, nil, platform.ClassPolicy, 0, "locked"},
		{http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, platform.ClassRateLimited, 30 * time.Second, ""},
		{http.StatusTooManyRequests, map[string]string{"RateLimit": `"baseline";r=0;t=42`, "RateLimit-Policy": `"baseline";q=2000;w=600`}, platform.ClassRateLimited, 42 * time.Second, ""},
		{http.StatusTooManyRequests, nil, platform.ClassRateLimited, 0, ""},
		{http.StatusInternalServerError, nil, platform.ClassTransient, 0, ""},
		{http.StatusNotImplemented, nil, platform.ClassUnsupported, 0, ""},
		{http.StatusBadGateway, nil, platform.ClassTransient, 0, ""},
		{http.StatusServiceUnavailable, map[string]string{"Retry-After": "5"}, platform.ClassTransient, 5 * time.Second, ""},
		{http.StatusMovedPermanently, map[string]string{"Location": "/elsewhere"}, platform.ClassUnknown, 0, ""},
	} {
		fx.handle(http.MethodGet, "/repos/acme/api", func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range tc.header {
				w.Header().Set(k, v)
			}
			writeJSON(w, tc.status, fx.apiMsg("status "+http.StatusText(tc.status)))
		})
		_, err := fx.reader.Repo(t.Context(), "acme/api")
		var pe *platform.Error
		switch {
		case !errors.As(err, &pe):
			t.Errorf("%d: %v is no *platform.Error", tc.status, err)
		case pe.Class != tc.class || pe.Status != tc.status || pe.RetryAfter != tc.retry || pe.Rule != tc.rule || pe.Op != "get repository":
			t.Errorf("%d: %+v, want class %v retry %v rule %q", tc.status, pe, tc.class, tc.retry, tc.rule)
		case (tc.status == http.StatusNotFound) != errors.Is(err, platform.ErrNotFound):
			t.Errorf("%d: ErrNotFound %v", tc.status, errors.Is(err, platform.ErrNotFound))
		case !strings.Contains(err.Error(), "status "+http.StatusText(tc.status)):
			t.Errorf("%d: the message %q lacks the platform's", tc.status, err)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header map[string]string
		want   time.Duration
	}{
		{map[string]string{"Retry-After": "120"}, 2 * time.Minute},
		{map[string]string{"Retry-After": at.Add(90 * time.Second).Format(http.TimeFormat)}, 90 * time.Second},
		{map[string]string{"Retry-After": at.Add(-time.Minute).Format(http.TimeFormat)}, 0},
		{map[string]string{"Retry-After": "soon"}, 0},
		{map[string]string{"RateLimit": `"baseline";r=0;t=17, "burst";r=5;t=1`}, 17 * time.Second},
		{map[string]string{"RateLimit-Reset": "8"}, 8 * time.Second},
		{map[string]string{"X-RateLimit-Reset": "25"}, 25 * time.Second},
		{map[string]string{"X-RateLimit-Reset": "1790510460"}, time.Unix(1790510460, 0).Sub(at)},
		{map[string]string{"Retry-After": "99999999"}, time.Hour},
		{map[string]string{"Retry-After": "-5"}, 0},
		{nil, 0},
	} {
		h := http.Header{}
		for k, v := range tc.header {
			h.Set(k, v)
		}
		want := min(max(tc.want, 0), time.Hour)
		if got := retryAfter(h, at); got != want {
			t.Errorf("retryAfter(%v) = %v, want %v", tc.header, got, want)
		}
	}
	if !rateLimited(http.Header{"Ratelimit": {`"baseline";r=0;t=3`}}) || rateLimited(http.Header{"Ratelimit": {`"baseline";r=7;t=3`}}) {
		t.Error("rateLimited reads the IETF RateLimit header wrong")
	}
	if !rateLimited(http.Header{"X-Ratelimit-Remaining": {"0"}}) || rateLimited(http.Header{}) {
		t.Error("rateLimited reads X-RateLimit-Remaining wrong")
	}
}

func TestTransportErrors(t *testing.T) {
	// A closed port: the connection is refused, a transient failure.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	p := newAPIServer(t).provider("gitea")
	p.URL, p.APIURL, p.Host = "http://"+addr, "http://"+addr+"/api/v1", addr
	tok := testToken(t)
	r, err := NewReader(p, auth.Credential{Kind: auth.Token, Token: tok}, httpx.New(httpx.Options{Timeout: 2 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Repo(t.Context(), "acme/api")
	wantClass(t, "a refused connection", err, platform.ClassTransient, nil)
	if err != nil && strings.Contains(err.Error(), tok) {
		t.Errorf("the error shows the token: %v", err)
	}
	// A canceled context stays unclassified; a done deadline is transient.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.Repo(canceled, "acme/api")
	if !errors.Is(err, context.Canceled) || platform.ClassOf(err) != platform.ClassUnknown {
		t.Errorf("a canceled call: %v (class %v)", err, platform.ClassOf(err))
	}
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = r.Repo(expired, "acme/api")
	wantClass(t, "an expired deadline", err, platform.ClassTransient, nil)

	// httpx refuses to send the token over plain http to a host that is not
	// the loopback: a configuration error.
	p2 := newAPIServer(t).provider("gitea")
	p2.URL, p2.APIURL = "http://gitea.example.com", "http://gitea.example.com/api/v1"
	r2, err := NewReader(p2, auth.Credential{Kind: auth.Token, Token: tok}, httpx.New(httpx.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r2.Repo(t.Context(), "acme/api")
	wantClass(t, "a refused request", err, platform.ClassInvalid, httpx.ErrRefused)
}

func TestListAllEndings(t *testing.T) {
	fx := newFixture(t, "gitea")
	items := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"id": i + 1, "name": "l"}
		}
		return out
	}
	read := func() (int, bool, error) {
		n := 0
		complete, err := listAll(t.Context(), fx.reader.c, "list", fx.reader.c.endpoint("things"), nil, listOpts{maxPages: 10, trustTotal: true},
			func(apiLabel) error { n++; return nil })
		return n, complete, err
	}
	// The instance's page size unknown (no /settings/api): read to an empty
	// page when neither Link nor X-Total-Count tells the end.
	fx.json(http.MethodGet, "/settings/api", http.StatusNotFound, fx.apiMsg("not found"))
	all := items(3)
	fx.handle(http.MethodGet, "/things", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			writeJSON(w, http.StatusOK, all)
			return
		}
		writeJSON(w, http.StatusOK, []any{})
	})
	if n, complete, err := read(); n != 3 || !complete || err != nil {
		t.Errorf("without headers: %d, %v, %v", n, complete, err)
	}
	if got := len(fx.requests(http.MethodGet, "/things")); got != 2 {
		t.Errorf("without headers: %d requests, want 2 (to an empty page)", got)
	}
	// The settings lookup that failed with 404 is not repeated.
	if got := len(fx.requests(http.MethodGet, "/settings/api")); got != 1 {
		t.Errorf("/settings/api asked %d times", got)
	}
	// X-Total-Count ends a listing without Link.
	fx.reset()
	fx.handle(http.MethodGet, "/things", func(w http.ResponseWriter, r *http.Request) { servePage(w, r, items(120), totalOnly) })
	if n, complete, err := read(); n != 120 || !complete || err != nil {
		t.Errorf("by X-Total-Count: %d, %v, %v", n, complete, err)
	}
	if got := len(fx.requests(http.MethodGet, "/things")); got != 3 {
		t.Errorf("by X-Total-Count: %d requests, want 3", got)
	}
	// A listing that goes on is capped.
	fx.handle(http.MethodGet, "/things", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Total-Count", "100000")
		writeJSON(w, http.StatusOK, items(50))
	})
	if n, complete, err := read(); n != 500 || complete || err != nil {
		t.Errorf("capped: %d, %v, %v", n, complete, err)
	}
}

func TestLinkNext(t *testing.T) {
	for _, tc := range []struct {
		link       []string
		next, have bool
	}{
		{nil, false, false},
		{[]string{`<https://h/api/v1/x?page=2>; rel="next",<https://h/api/v1/x?page=5>; rel="last"`}, true, true},
		{[]string{`<https://h/api/v1/x?page=1>; rel="first",<https://h/api/v1/x?page=4>; rel="prev"`}, false, true},
		{[]string{`<https://h/x?q=a,b&page=2>; rel=next`}, true, true},
		{[]string{`<https://h/x?page=1>; rel="first"`, `<https://h/x?page=3>; rel="next"`}, true, true},
		{[]string{`<https://h/x?page=3>; rel="prev next"`}, true, true},
	} {
		h := http.Header{}
		for _, v := range tc.link {
			h.Add("Link", v)
		}
		if next, have := linkNext(h); next != tc.next || have != tc.have {
			t.Errorf("linkNext(%q) = %v, %v; want %v, %v", tc.link, next, have, tc.next, tc.have)
		}
	}
}
