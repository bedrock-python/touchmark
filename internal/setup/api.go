package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
)

// api sends the maintainer's requests to one platform's REST API: the
// credential goes to the API's host only (httpx), and a dry run refuses
// writes before they are sent.
type api struct {
	client *httpx.Client
	base   string // the REST base, without a trailing slash
	auth   *httpx.Auth
	header http.Header
	dryRun bool
}

// newAPI returns an api over base with token in the header name ("" for
// Authorization) and value prefix ("Bearer ").
func newAPI(client *httpx.Client, base, name, prefix, token string, header http.Header, dryRun bool) (*api, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("the API URL %q is not an absolute http(s) URL without credentials", base)
	}
	if client == nil {
		client = httpx.New(httpx.Options{})
	}
	value := prefix + token
	a := &api{client: client, base: u.String(), header: header, dryRun: dryRun}
	if token != "" {
		a.auth = &httpx.Auth{Hosts: []string{u.Host}, Name: name, Header: func(context.Context) (string, error) { return value, nil }}
	}
	return a, nil
}

// errDryRun is returned for a write in a dry run: a bug of the caller.
var errDryRun = errors.New("setup: a write in a dry run")

// path joins escaped path segments under the base.
func (a *api) path(segs ...string) string {
	var b strings.Builder
	b.WriteString(a.base)
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// do sends one request with the JSON body in (nil for none) and decodes a
// 2xx answer into out (nil to skip). A non-2xx answer is an
// *httpx.StatusError.
func (a *api) do(ctx context.Context, method, u string, in, out any) error {
	if method != http.MethodGet && a.dryRun {
		return errDryRun
	}
	_, err := a.client.JSONWith(ctx, method, u, a.auth, a.header, in, out)
	return err
}

// get reads u into out.
func (a *api) get(ctx context.Context, u string, out any) error {
	return a.do(ctx, http.MethodGet, u, nil, out)
}

// statusOf returns the HTTP status of a failed request, 0 for none.
func statusOf(err error) int {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// isStatus reports whether err is an answer with one of the statuses.
func isStatus(err error, statuses ...int) bool {
	s := statusOf(err)
	for _, want := range statuses {
		if s == want {
			return true
		}
	}
	return false
}

// Bounds of a listing: 10 pages of 100.
const (
	listPages    = 10
	listPageSize = 100
)

// errTooMany is a listing longer than setup reads.
var errTooMany = errors.New("more items than setup reads (1000)")

// list reads a paginated listing of T (per_page and page parameters, a
// JSON array per page) until a short page.
func list[T any](ctx context.Context, a *api, u string, query url.Values) ([]T, error) {
	var out []T
	for page := 1; page <= listPages; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("per_page", fmt.Sprint(listPageSize))
		q.Set("page", fmt.Sprint(page))
		var items []T
		if err := a.get(ctx, u+"?"+q.Encode(), &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) < listPageSize {
			return out, nil
		}
	}
	return out, errTooMany
}
