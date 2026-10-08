package bitbucket

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestStatusClasses(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		status int
		header map[string]string
		msg    string
		class  platform.Class
		wait   time.Duration
	}{
		{http.StatusBadRequest, nil, "Field \".summary.raw\" does not support filtering", platform.ClassInvalid, 0},
		{http.StatusUnauthorized, nil, "Token is invalid, expired, or not supported for this endpoint.", platform.ClassAuth, 0},
		{http.StatusForbidden, nil, "Your credentials lack one or more required privilege scopes.", platform.ClassPermission, 0},
		{http.StatusForbidden, nil, "Rate limit for this resource has been exceeded", platform.ClassRateLimited, 0},
		{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "60"}, "Forbidden", platform.ClassRateLimited, time.Minute},
		{http.StatusNotFound, nil, noRepoMessage, platform.ClassNotFound, 0},
		{http.StatusConflict, nil, "conflict", platform.ClassConflict, 0},
		{http.StatusGone, nil, "This workspace has been deleted", platform.ClassNotFound, 0},
		{http.StatusTooManyRequests, map[string]string{"X-RateLimit-Limit": "1000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "394"},
			"Rate limit for this resource has been exceeded", platform.ClassRateLimited, 394 * time.Second},
		{http.StatusTooManyRequests, map[string]string{"Retry-After": "30", "X-RateLimit-Reset": "394"}, "Too many", platform.ClassRateLimited, 30 * time.Second},
		{http.StatusTooManyRequests, map[string]string{"X-RateLimit-Reset": "999999"}, "Too many", platform.ClassRateLimited, time.Hour},
		{http.StatusInternalServerError, nil, "Something went wrong", platform.ClassTransient, 0},
		{http.StatusServiceUnavailable, map[string]string{"Retry-After": "5"}, "Unavailable", platform.ClassTransient, 5 * time.Second},
		{555, nil, "timeout", platform.ClassTransient, 0},
		{http.StatusNotImplemented, nil, "no", platform.ClassUnsupported, 0},
	} {
		path := "/repositories/acme/s" + strconv.Itoa(tc.status) + strconv.Itoa(len(tc.header))
		f.handle(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range tc.header {
				w.Header().Set(k, v)
			}
			writeJSON(w, tc.status, errorBody(tc.msg))
		})
		_, err := f.reader.Repo(t.Context(), strings.TrimPrefix(path, "/repositories/"))
		what := strconv.Itoa(tc.status) + " " + tc.msg
		wantClass(t, what, err, tc.class, nil)
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: the error %q lacks the API's message", what, err)
		}
		if d := retryAfterOf(err); d != tc.wait {
			t.Errorf("%s: RetryAfter %v, want %v", what, d, tc.wait)
		}
		if tc.status == http.StatusNotFound && !isNotFound(err) {
			t.Errorf("%s: %v is no ErrNotFound", what, err)
		}
	}
}

func TestTransportErrors(t *testing.T) {
	t.Run("a body that does not decode", func(t *testing.T) {
		f := newFixture(t)
		f.handle(http.MethodGet, "/repositories/acme/api", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("<html>maintenance</html>"))
		})
		_, err := f.reader.Repo(t.Context(), "acme/api")
		wantClass(t, "HTML instead of JSON", err, platform.ClassUnknown, nil)
	})
	t.Run("a server that is gone", func(t *testing.T) {
		f := newFixture(t)
		f.srv.Close()
		_, err := f.reader.Repo(t.Context(), "acme/api")
		wantClass(t, "a closed server", err, platform.ClassTransient, nil)
	})
}

// TestTokenMasked: the token never shows in an error, raw or in git's Basic
// form, even when the API echoes it.
func TestTokenMasked(t *testing.T) {
	f := newFixture(t)
	basic := base64.StdEncoding.EncodeToString([]byte("x-bitbucket-api-token-auth:" + f.token))
	echo := "token " + f.token + " basic " + basic + " escaped " + url.QueryEscape(f.token)
	f.json("/repositories/acme/api", http.StatusUnauthorized, errorBody(echo))
	f.json("/user", http.StatusForbidden, map[string]any{"type": "error", "error": map[string]any{"message": "no", "detail": echo}})
	f.handle(http.MethodGet, "/repositories/acme/api/src/"+headCommit+"/"+optIn, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "meta" {
			writeJSON(w, http.StatusOK, map[string]any{"path": optIn, "type": "commit_file", "attributes": []string{}, "size": 3})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream said " + echo))
	})
	_, err1 := f.reader.Repo(t.Context(), "acme/api")
	_, err2 := f.reader.Self(t.Context())
	_, err3 := f.reader.ReadFile(t.Context(), apiRepoFixture, headCommit, optIn, 100)
	for i, err := range []error{err1, err2, err3} {
		if err == nil {
			t.Fatalf("error %d: none", i)
		}
		msg := err.Error()
		if strings.Contains(msg, f.token) || strings.Contains(msg, basic) || strings.Contains(msg, url.QueryEscape(f.token)) {
			t.Errorf("error %d shows the token: %s", i, msg)
		}
		if !strings.Contains(msg, "***") {
			t.Errorf("error %d: %s; want the token masked", i, msg)
		}
	}
	wantClass(t, "a raw read that fails", err3, platform.ClassTransient, nil)
}

func TestRedirects(t *testing.T) {
	f := newFixture(t)
	f.handle(http.MethodGet, "/repositories/acme/old-name", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/2.0/repositories/acme/api", http.StatusMovedPermanently)
	})
	f.json("/repositories/acme/api", http.StatusOK, repo(repoUUID, "acme/api"))
	got, err := f.reader.Repo(t.Context(), "acme/old-name")
	if err != nil || got.Path != "acme/api" || got.ID != repoUUID {
		t.Errorf("Repo(old name) = %+v, %v; want the repository under its canonical path", got, err)
	}
	if calls := f.requests(http.MethodGet, "/repositories/acme/api"); len(calls) != 1 || calls[0].Auth != "Bearer "+f.token {
		t.Errorf("calls %+v; want the redirect followed with the credential", calls)
	}
	f.handle(http.MethodGet, "/repositories/acme/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/2.0/repositories/acme/api", http.StatusFound)
	})
	f.handle(http.MethodGet, "/repositories/acme/up", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	for _, p := range []string{"acme/away", "acme/up"} {
		_, err := f.reader.Repo(t.Context(), p)
		wantClass(t, p, err, platform.ClassUnknown, nil)
	}
}

func TestBlobID(t *testing.T) {
	// git hash-object of "hello\n" and of an empty file.
	for content, want := range map[string]string{
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
	} {
		if got := blobID([]byte(content)); got != want {
			t.Errorf("blobID(%q) = %s, want %s", content, got, want)
		}
	}
}

// TestMessageFields: the messages of a write's fields join the message, by
// field name, whether a field has a list of messages or one.
func TestMessageFields(t *testing.T) {
	m := parseMessage(`{"type": "error", "error": {"message": "Bad request", "fields": {` +
		`"reviewers": ["Malformed reviewers list", "x is the author and cannot be included as a reviewer."], ` +
		`"destination": "branch not found", "odd": {"a": 1}}}}`)
	want := "Bad request; destination: branch not found; reviewers: Malformed reviewers list, x is the author and cannot be included as a reviewer."
	if got := m.text(); got != want {
		t.Errorf("text = %q\nwant %q", got, want)
	}
}
