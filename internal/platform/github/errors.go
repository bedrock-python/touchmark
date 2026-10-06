package github

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
// platform.Error: a non-2xx status by its code and message, a timeout as
// transient, a request httpx refused as invalid, a certificate the client
// does not trust, a response over the size bound or of an unexpected shape
// as unknown (none of them gets better by retrying), any other transport
// failure as transient. A canceled context stays unclassified. Messages
// never hold a credential nor headers.
func (c *client) apiError(op string, err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return c.statusError(op, se)
	}
	var pe *platform.Error
	if errors.As(err, &pe) {
		// Already classified (a credential that could not be minted).
		return err
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

// apiMessage is GitHub's error body: a message, and validation errors that
// are objects ({resource, field, code, message}) or strings.
type apiMessage struct {
	Message string            `json:"message"`
	Errors  []json.RawMessage `json:"errors"`
}

// apiFieldError is one validation error of an apiMessage.
type apiFieldError struct {
	Resource string `json:"resource"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// parsedMessage is what statusError reads from an error body.
type parsedMessage struct {
	text  string   // message and details, one line
	codes []string // codes of the validation errors
}

// parseMessage reads GitHub's error JSON from snippet (at most 1 KiB,
// masked by httpx); anything else is the snippet itself.
func parseMessage(snippet string) parsedMessage {
	var m apiMessage
	if json.Unmarshal([]byte(snippet), &m) != nil || m.Message == "" && len(m.Errors) == 0 {
		return parsedMessage{text: oneLine(snippet)}
	}
	var out parsedMessage
	parts := []string{}
	if m.Message != "" {
		parts = append(parts, m.Message)
	}
	for _, raw := range m.Errors {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s != "" && s != m.Message {
				parts = append(parts, s)
			}
			continue
		}
		var fe apiFieldError
		if json.Unmarshal(raw, &fe) != nil {
			continue
		}
		if fe.Code != "" {
			out.codes = append(out.codes, fe.Code)
		}
		switch {
		case fe.Message != "" && fe.Message != m.Message:
			parts = append(parts, fe.Message)
		case fe.Message == "" && fe.Code != "":
			parts = append(parts, strings.TrimSpace(fe.Resource+" "+fe.Field+" "+fe.Code))
		}
	}
	out.text = oneLine(strings.Join(parts, "; "))
	return out
}

// oneLine collapses whitespace and control characters into single spaces.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// statusError classifies a non-2xx response of op:
//   - 401 is auth; 403 with X-GitHub-SSO too (the token is not authorized
//     for SAML single sign-on, Rule "sso");
//   - a 403 or 429 with x-ratelimit-remaining: 0, a Retry-After, or a
//     message about a secondary rate limit (or the older "abuse detection")
//     is rate-limited, waiting for Retry-After, else x-ratelimit-reset
//     (https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api);
//   - a refusal to let an App or a token change workflow files is
//     permission, Rule "workflows"; a repository rule violation (GH013) is
//     policy, Rule "GH013"; a protected branch (GH006) policy, Rule
//     "protected-branch"; an archived repository permission, Rule
//     "archived";
//   - other 403 are permission, 404 and 410 not found, 409 conflict, 422
//     invalid, 5xx transient.
func (c *client) statusError(op string, se *httpx.StatusError) error {
	m := parseMessage(se.Snippet)
	msg := c.mask(describe(se, m.text))
	lower := strings.ToLower(m.text)
	e := &platform.Error{Op: op, Class: statusClass(se.Status), Status: se.Status}
	switch {
	case se.Status == http.StatusNotFound || se.Status == http.StatusGone:
		e.Class = platform.ClassNotFound
		e.Err = fmt.Errorf("%s: %w", msg, platform.ErrNotFound)
		return e
	case (se.Status == http.StatusForbidden || se.Status == http.StatusTooManyRequests) && rateLimited(se.Header, lower):
		e.Class, e.RetryAfter = platform.ClassRateLimited, retryAfter(se.Header, c.now())
	case se.Status == http.StatusTooManyRequests, se.Status == http.StatusServiceUnavailable:
		e.RetryAfter = retryAfter(se.Header, c.now())
	case se.Status == http.StatusForbidden && se.Header.Get("X-GitHub-SSO") != "":
		e.Class, e.Rule = platform.ClassAuth, "sso"
	}
	if rule, class, ok := ruleOf(lower); ok && e.Class != platform.ClassRateLimited && se.Status >= 400 && se.Status < 500 &&
		se.Status != http.StatusUnauthorized {
		e.Class, e.Rule = class, rule
	}
	e.Err = errors.New(msg)
	return e
}

// ruleOf names the rule a refusal's message (lowercased) tells, with the
// class the core reacts to.
func ruleOf(lower string) (rule string, class platform.Class, ok bool) {
	switch {
	case strings.Contains(lower, "refusing to allow a github app to create or update workflow"),
		strings.Contains(lower, "refusing to allow an oauth app to create or update workflow"),
		strings.Contains(lower, "refusing to allow a personal access token to create or update workflow"),
		strings.Contains(lower, "`workflows` permission"), strings.Contains(lower, "`workflow` scope"):
		return "workflows", platform.ClassPermission, true
	case strings.Contains(lower, "gh013"), strings.Contains(lower, "repository rule violation"):
		return "GH013", platform.ClassPolicy, true
	case strings.Contains(lower, "gh006"), strings.Contains(lower, "protected branch"):
		return "protected-branch", platform.ClassPolicy, true
	case strings.Contains(lower, "was archived so is read-only"), strings.Contains(lower, "repository is archived"):
		return "archived", platform.ClassPermission, true
	}
	return "", 0, false
}

// statusClass maps an HTTP status to the class the core reacts to.
func statusClass(status int) platform.Class {
	switch {
	case status == http.StatusUnauthorized:
		return platform.ClassAuth
	case status == http.StatusForbidden:
		return platform.ClassPermission
	case status == http.StatusNotFound, status == http.StatusGone:
		return platform.ClassNotFound
	case status == http.StatusConflict:
		return platform.ClassConflict
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

// rateLimited reports whether a 403 or 429 is a rate limit: no requests
// left (x-ratelimit-remaining: 0), a Retry-After, or a message about a
// secondary rate limit.
func rateLimited(h http.Header, lowerMessage string) bool {
	return strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0" || h.Get("Retry-After") != "" ||
		limitMessage(lowerMessage)
}

// limitMessage reports whether a lowercased message is GitHub's about a
// rate limit: a secondary one ("You have exceeded a secondary rate limit",
// the older "abuse detection mechanism") or the primary one ("API rate
// limit exceeded"), for answers whose headers a proxy dropped.
func limitMessage(lower string) bool {
	return strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "abuse detection") ||
		strings.Contains(lower, "api rate limit exceeded")
}

// retryAfter reads how long to wait from a response's headers: Retry-After
// (seconds or an HTTP date), else the reset of x-ratelimit-reset (a Unix
// time) when no requests are left. It is 0 when none is usable (the core
// waits at least a minute then, as GitHub asks), and at most maxRetryAfter.
func retryAfter(h http.Header, at time.Time) time.Duration {
	d, ok := time.Duration(0), false
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			d, ok = seconds(secs), true
		} else if t, err := http.ParseTime(v); err == nil {
			d, ok = t.Sub(at), true
		}
	}
	if !ok && strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0" {
		if n, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			d, ok = time.Unix(n, 0).Sub(at), true
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

// messageOf returns the lowercased message of err, for matching GitHub's
// texts.
func messageOf(err error) string {
	if err == nil {
		return ""
	}
	return strings.ToLower(err.Error())
}

// stops reports whether err ends a call at once, whatever it already
// found: the context ended, the platform limits the rate or refuses the
// credential.
func stops(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	c := platform.ClassOf(err)
	return c == platform.ClassRateLimited || c == platform.ClassAuth
}

// fatal reports whether err must fail a listing instead of making it
// incomplete: it stops the call, or the platform failed for now (the caller
// retries the whole listing).
func fatal(err error) bool { return stops(err) || platform.ClassOf(err) == platform.ClassTransient }
