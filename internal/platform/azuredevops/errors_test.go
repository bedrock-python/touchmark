package azuredevops

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// TestErrors: answers are classified by status, typeKey, TF code and
// Retry-After, and never show the token.
func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		header  map[string]string
		body    any
		want    platform.Class
		typeKey string
		after   time.Duration
	}{
		{name: "401", status: 401, body: errorBody("UnauthorizedRequestException", "TF400813: The user is not authorized to access this resource."), want: platform.ClassAuth},
		{name: "403", status: 403, body: errorBody("UnauthorizedRequestException", "TF401019: no permission"), want: platform.ClassPermission},
		{name: "404 repository", status: 404, body: errorBody(keyRepoNotFound, "TF401019: The Git repository does not exist"), want: platform.ClassNotFound, typeKey: keyRepoNotFound},
		{name: "429", status: 429, header: map[string]string{"Retry-After": "30"}, body: errorBody("RequestBlockedException", tfRateLimited+": The request has been canceled"), want: platform.ClassRateLimited, after: 30 * time.Second},
		{name: "Retry-After on 400", status: 400, header: map[string]string{"Retry-After": "5"}, body: errorBody("X", "slow down"), want: platform.ClassRateLimited, after: 5 * time.Second},
		{name: "TF400733 on 503", status: 503, body: errorBody("X", tfRateLimited+": blocked"), want: platform.ClassRateLimited},
		{name: "503", status: 503, body: "busy", want: platform.ClassTransient},
		{name: "400", status: 400, body: errorBody("InvalidArgumentValueException", "bad"), want: platform.ClassInvalid},
		{name: "409", status: 409, body: errorBody(keyPRExists, tfPRExists+": An active pull request for the source and target branch already exists."), want: platform.ClassConflict, typeKey: keyPRExists},
		{name: "400 duplicate", status: 400, body: errorBody(keyPRExists, tfPRExists+": exists"), want: platform.ClassConflict, typeKey: keyPRExists},
		{name: "sign-in redirect", status: 302, header: map[string]string{"Location": "https://spsprodcus4.vssps.visualstudio.com/_signin?realm=dev.azure.com"}, body: "moved", want: platform.ClassAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAPIServer(t)
			s.handle(http.MethodGet, apisPath("connectionData"), func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				writeJSON(w, tc.status, tc.body)
			})
			token := testToken(t)
			_, err := newTestReader(t, s, token).Self(context.Background())
			wantClass(t, tc.name, err, tc.want)
			if got := typeKeyOf(err); tc.typeKey != "" && got != tc.typeKey {
				t.Errorf("typeKey %q, want %q", got, tc.typeKey)
			}
			var pe *platform.Error
			if errors.As(err, &pe) && pe.RetryAfter != tc.after {
				t.Errorf("RetryAfter %v, want %v", pe.RetryAfter, tc.after)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("the error shows the token: %v", err)
			}
			if tc.status == 404 && !errors.Is(err, platform.ErrNotFound) {
				t.Errorf("a 404 without ErrNotFound: %v", err)
			}
		})
	}
}

// TestSignInPage: a 203 with an HTML page is a refused credential.
func TestSignInPage(t *testing.T) {
	s := newAPIServer(t)
	s.handle(http.MethodGet, apisPath("connectionData"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		_, _ = w.Write([]byte("<html><head><title>Azure DevOps Services | Sign In</title></head></html>"))
	})
	_, err := newTestReader(t, s, testToken(t)).Self(context.Background())
	wantClass(t, "203", err, platform.ClassAuth)
}

func TestRetryAfter(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		h    http.Header
		want time.Duration
	}{
		{http.Header{"Retry-After": {"12"}}, 12 * time.Second},
		{http.Header{"X-Ratelimit-Reset": {"1800000060"}}, time.Minute},
		{http.Header{"Retry-After": {"999999"}}, maxRetryAfter},
		{http.Header{"Retry-After": {"soon"}}, 0},
		{http.Header{}, 0},
	} {
		if got := retryAfter(tc.h, at); got != tc.want {
			t.Errorf("retryAfter(%v) = %v, want %v", tc.h, got, tc.want)
		}
	}
}
