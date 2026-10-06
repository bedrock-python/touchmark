package gitea

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxRetryAfter bounds a wait read from response headers: a broken or
// hostile header must not park the provider for days.
const maxRetryAfter = time.Hour

// now is the clock of Retry-After dates and reset times (tests replace it).
var now = time.Now

// maskedError carries a masked message over the original error, which
// errors.Is and errors.As still see.
type maskedError struct {
	msg string
	err error
}

func (e *maskedError) Error() string { return e.msg }
func (e *maskedError) Unwrap() error { return e.err }

// masked returns err with its message masked.
func (c *client) masked(err error) error {
	return &maskedError{msg: c.mask(err.Error()), err: err}
}

// apiError classifies err, the failure of an API request of op, into a
// platform.Error: a non-2xx status by its code, a timeout as transient, a
// request httpx refused as invalid, a certificate the client does not trust, a
// response over the size bound or of an unexpected shape as unknown (none
// of them gets better by retrying), any other transport failure as
// transient. A canceled context stays unclassified. Messages never hold the
// credential nor headers.
func (c *client) apiError(op string, err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return c.statusError(op, se)
	}
	class := platform.ClassTransient
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", op, c.masked(err))
	case errors.Is(err, context.DeadlineExceeded):
	case errors.Is(err, httpx.ErrRefused):
		class = platform.ClassInvalid
	case errors.Is(err, httpx.ErrTooLarge), certificateError(err), decodeError(err):
		class = platform.ClassUnknown
	}
	return &platform.Error{Op: op, Class: class, Err: c.masked(err)}
}

// certificateError reports whether err is a TLS certificate failure.
func certificateError(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		unknown   x509.UnknownAuthorityError
		invalid   x509.CertificateInvalidError
		hostError x509.HostnameError
	)
	return errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostError)
}

// decodeError reports whether err is a JSON body that does not decode.
func decodeError(err error) bool {
	var (
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	return errors.As(err, &syntax) || errors.As(err, &typ) || errors.Is(err, errShape)
}

// errShape is wrapped by errors about responses that decode but lack what
// the driver depends on.
var errShape = errors.New("unexpected response")

// shapeError is an error of op about a response that lacks what the driver
// depends on: the API changed shape.
func shapeError(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassUnknown,
		Err: fmt.Errorf("%w: "+format, append([]any{errShape}, args...)...)}
}

// statusError classifies a non-2xx response of op.
func (c *client) statusError(op string, se *httpx.StatusError) error {
	detail := platformMessage(se.Snippet)
	msg := c.mask(describe(se, detail))
	e := &platform.Error{Op: op, Class: statusClass(se.Status), Status: se.Status}
	switch se.Status {
	case http.StatusNotFound:
		e.Err = fmt.Errorf("%s: %w", msg, platform.ErrNotFound)
		return e
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		e.RetryAfter = retryAfter(se.Header, now())
	case http.StatusForbidden:
		// A proxy in front of the instance may answer a limit with 403
		// (Gitea and Forgejo have no limits of their own).
		if rateLimited(se.Header) {
			e.Class, e.RetryAfter = platform.ClassRateLimited, retryAfter(se.Header, now())
		}
	case http.StatusLocked:
		// 423: an archived repository refuses every write; a locked
		// conversation refuses comments.
		if strings.Contains(strings.ToLower(detail), "archived") {
			e.Class, e.Rule = platform.ClassPermission, "archived"
		} else {
			e.Rule = "locked"
		}
	case http.StatusPreconditionFailed:
		// 412 refuses a pull request that was merged meanwhile (decide
		// again), or a close while it depends on open issues: a rule people
		// set, which no retry passes ("cannot close this issue or pull
		// request because it still has open dependencies" on Gitea 1.26 and
		// 1.27, "cannot close this pull request because …" on Forgejo 15
		// and 16; routers/api/v1/repo/pull.go EditPullRequest).
		if strings.Contains(strings.ToLower(detail), "open dependencies") {
			e.Class, e.Rule = platform.ClassPolicy, "dependencies"
		}
	}
	e.Err = errors.New(msg)
	return e
}

// statusClass maps an HTTP status to the class the core reacts to.
func statusClass(status int) platform.Class {
	switch {
	case status == http.StatusUnauthorized:
		return platform.ClassAuth
	case status == http.StatusForbidden:
		return platform.ClassPermission
	case status == http.StatusNotFound:
		return platform.ClassNotFound
	case status == http.StatusConflict, status == http.StatusPreconditionFailed:
		// 409: a duplicate or an edit conflict; 412: a state change the
		// pull request no longer allows (merged meanwhile; statusError
		// tells open dependencies apart). Decide again.
		return platform.ClassConflict
	case status == http.StatusLocked:
		return platform.ClassPolicy
	case status == http.StatusTooManyRequests:
		return platform.ClassRateLimited
	case status == http.StatusRequestTimeout, status == http.StatusTooEarly:
		return platform.ClassTransient
	case status == http.StatusNotImplemented:
		return platform.ClassUnsupported
	case status >= 500:
		return platform.ClassTransient
	case status >= 400:
		return platform.ClassInvalid
	}
	// An unexpected redirect or informational status.
	return platform.ClassUnknown
}

// apiMessage is the error body of Gitea and Forgejo.
type apiMessage struct {
	Message string   `json:"message"`
	Errors  []string `json:"errors"`
}

// describe returns "METHOD URL: status text: detail".
func describe(se *httpx.StatusError, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d", se.Method, se.URL, se.Status)
	if text := http.StatusText(se.Status); text != "" {
		b.WriteString(" " + text)
	}
	if detail != "" {
		b.WriteString(": " + detail)
	}
	return b.String()
}

// platformMessage returns the message of an error body on one line: the
// message and its details when it is Gitea's error JSON, else the snippet
// httpx kept (at most 1 KiB, on one line, masked by httpx's registry).
func platformMessage(snippet string) string {
	var m apiMessage
	if json.Unmarshal([]byte(snippet), &m) != nil || m.Message == "" && len(m.Errors) == 0 {
		return snippet
	}
	parts := make([]string, 0, 1+len(m.Errors))
	if m.Message != "" {
		parts = append(parts, m.Message)
	}
	for _, e := range m.Errors {
		if e != "" && e != m.Message {
			parts = append(parts, e)
		}
	}
	// Forgejo's messages may hold several lines.
	return strings.Join(strings.FieldsFunc(strings.Join(parts, "; "), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// rateLimited reports whether a response's headers say a rate limit was
// hit: a Retry-After, or no requests left in the RateLimit (IETF draft,
// Codeberg) or X-RateLimit-Remaining header.
func rateLimited(h http.Header) bool {
	if h.Get("Retry-After") != "" {
		return true
	}
	if r, ok := rateLimitParam(h.Get("RateLimit"), "r"); ok && r == 0 {
		return true
	}
	if v := strings.TrimSpace(h.Get("RateLimit-Remaining")); v == "0" {
		return true
	}
	return strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0"
}

// retryAfter reads how long to wait from a response's headers, in order:
// Retry-After (seconds or an HTTP date), the reset of the IETF RateLimit
// header ("…;r=0;t=30", Codeberg), RateLimit-Reset (seconds),
// X-RateLimit-Reset (a Unix time, or seconds when small). It is 0 when none
// is usable, and at most maxRetryAfter.
func retryAfter(h http.Header, at time.Time) time.Duration {
	d, ok := time.Duration(0), false
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			d, ok = seconds(secs), true
		} else if t, err := http.ParseTime(v); err == nil {
			d, ok = t.Sub(at), true
		}
	}
	if !ok {
		if t, found := rateLimitParam(h.Get("RateLimit"), "t"); found {
			d, ok = seconds(t), true
		}
	}
	if !ok {
		if secs, err := strconv.ParseInt(strings.TrimSpace(h.Get("RateLimit-Reset")), 10, 64); err == nil {
			d, ok = seconds(secs), true
		}
	}
	if !ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			// A Unix time is far above any sensible number of seconds.
			if n > 1_000_000_000 {
				d, ok = time.Unix(n, 0).Sub(at), true
			} else {
				d, ok = seconds(n), true
			}
		}
	}
	switch {
	case !ok || d < 0:
		return 0
	case d > maxRetryAfter:
		return maxRetryAfter
	}
	return d
}

// seconds converts a count of seconds, saturating instead of overflowing.
func seconds(n int64) time.Duration {
	if n < 0 {
		return -1
	}
	if n > math.MaxInt64/int64(time.Second) {
		return maxRetryAfter
	}
	return time.Duration(n) * time.Second
}

// rateLimitParam returns the integer parameter name ("r", "t") of the first
// policy of an IETF RateLimit header value such as `"baseline";r=10;t=30`.
func rateLimitParam(v, name string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	first, _, _ := strings.Cut(v, ",")
	for part := range strings.SplitSeq(first, ";") {
		k, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(k), name) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// closedError is the error of a TargetWriter used after Close.
func closedError(op string) error {
	return &platform.Error{Op: op, Class: platform.ClassAuth, Err: errors.New("the target writer is closed")}
}

// invalid is a ClassInvalid error of op: the request was wrong.
func invalid(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassInvalid, Err: fmt.Errorf(format, args...)}
}

// notFound is a ClassNotFound error of op that wraps ErrNotFound.
func notFound(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassNotFound, Err: fmt.Errorf(format+": %w", append(args, platform.ErrNotFound)...)}
}

// statusOf returns the HTTP status of a classified error, 0 for none.
func statusOf(err error) int {
	var pe *platform.Error
	if errors.As(err, &pe) {
		return pe.Status
	}
	return 0
}

// isStatus reports whether err is an API error with status code.
func isStatus(err error, code int) bool { return statusOf(err) == code }
