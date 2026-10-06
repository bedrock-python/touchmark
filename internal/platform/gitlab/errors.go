package gitlab

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
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
//
// GitLab's error bodies are {"message": …} with a string ("404 Project Not
// Found"), a list ("Another open merge request already exists …") or an
// object of validation errors, or OAuth's {"error": …, "error_description":
// …} for a token that is invalid, expired, revoked or lacks a scope
// (lib/api/helpers.rb render_api_error!, lib/gitlab/auth/auth_finders.rb).
func (c *client) statusError(op string, se *httpx.StatusError) error {
	m := parseMessage(se.Snippet)
	detail := m.text
	if detail == "" {
		detail = se.Snippet
	}
	msg := c.mask(describe(se, detail))
	e := &platform.Error{Op: op, Class: statusClass(se.Status), Status: se.Status}
	lower := strings.ToLower(detail)
	switch se.Status {
	case http.StatusNotFound:
		e.Err = &notFoundError{what: m.notFound(), err: fmt.Errorf("%s: %w", msg, platform.ErrNotFound)}
		return e
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		e.RetryAfter = retryAfter(se.Header, now())
	case http.StatusUnauthorized:
		if m.oauth != "" {
			e.Rule = m.oauth
		}
	case http.StatusForbidden:
		// A 403 without a rule is also what an IP ban answers to every
		// request (300 failed git authentications a minute ban an IP on
		// gitlab.com): one target cannot tell, so the core counts such
		// refusals across targets and stops the provider after a streak of
		// them (distribute's circuit).
		switch {
		case rateLimited(se.Header):
			// A limit answered with 403 (a proxy, or an IP ban after
			// failed git sign-ins behind one).
			e.Class, e.RetryAfter = platform.ClassRateLimited, retryAfter(se.Header, now())
		case m.oauth == "insufficient_scope":
			// The token lacks a scope (its role may allow the action).
			e.Class, e.Rule = platform.ClassAuth, "insufficient_scope"
		case m.oauth != "":
			e.Class, e.Rule = platform.ClassAuth, m.oauth
		case strings.Contains(lower, "archived"):
			e.Rule = "archived"
		}
	case http.StatusUnprocessableEntity, http.StatusBadRequest, http.StatusMethodNotAllowed:
		// "Target project has disabled merge requests" (:base, 422).
		if strings.Contains(lower, "disabled merge requests") {
			e.Class, e.Rule = platform.ClassPolicy, "prs-disabled"
		}
	}
	e.Err = errors.New(msg)
	return e
}

// notFoundError is the error of a 404: what GitLab says was not found
// ("Project", "File", "Commit", "Branch", …; "" when the body is no
// GitLab message), over the ErrNotFound it wraps.
type notFoundError struct {
	what string
	err  error
}

func (e *notFoundError) Error() string { return e.err.Error() }
func (e *notFoundError) Unwrap() error { return e.err }

// missing returns what a 404 in err says was not found, lowercased ("file",
// "commit", "project", "branch", "404 not found" …); ok is false when err
// is no 404 of the API.
func missing(err error) (what string, ok bool) {
	var nf *notFoundError
	if !errors.As(err, &nf) {
		return "", false
	}
	return strings.ToLower(nf.what), true
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

// apiMessage is a GitLab error body.
type apiMessage struct {
	// text is the message on one line, "" when the body is none of
	// GitLab's.
	text string
	// oauth is the OAuth error code ("invalid_token",
	// "insufficient_scope"), "" for none.
	oauth string
	// single is the message when it is one string ("404 File Not Found").
	single string
}

// notFound returns what a 404 message names: "File" of "404 File Not
// Found", the whole message when it has another shape, "" for none.
func (m apiMessage) notFound() string {
	s := strings.TrimSpace(m.single)
	s = strings.TrimPrefix(s, "404 ")
	if what, ok := strings.CutSuffix(s, " Not Found"); ok {
		return what
	}
	return s
}

// parseMessage reads GitLab's error body from the snippet httpx kept (at
// most 1 KiB, on one line, masked).
func parseMessage(snippet string) apiMessage {
	var raw struct {
		Message          json.RawMessage `json:"message"`
		Error            string          `json:"error"`
		ErrorDescription string          `json:"error_description"`
		Scope            string          `json:"scope"`
	}
	if json.Unmarshal([]byte(snippet), &raw) != nil {
		return apiMessage{}
	}
	var m apiMessage
	var parts []string
	if len(raw.Message) > 0 {
		var s string
		if json.Unmarshal(raw.Message, &s) == nil {
			m.single = s
			parts = append(parts, s)
		} else {
			parts = append(parts, flattenMessage(raw.Message)...)
		}
	}
	if raw.Error != "" {
		m.oauth = raw.Error
		parts = append(parts, raw.Error)
		if raw.ErrorDescription != "" {
			parts = append(parts, raw.ErrorDescription)
		}
		if raw.Scope != "" {
			parts = append(parts, "scope "+raw.Scope)
		}
	}
	m.text = oneLine(strings.Join(parts, "; "))
	return m
}

// flattenMessage returns the strings of a message that is a list or an
// object of lists ({"title": ["can't be blank"]} → "title can't be
// blank"), keys in order.
func flattenMessage(raw json.RawMessage) []string {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, item := range list {
			out = append(out, flattenMessage(item)...)
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var out []string
		for _, k := range keys {
			for _, s := range flattenMessage(obj[k]) {
				out = append(out, k+" "+s)
			}
		}
		return out
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []string{s}
	}
	return nil
}

// oneLine collapses whitespace and control characters into single spaces.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// rateLimited reports whether a response's headers say a rate limit was
// hit: a Retry-After, or no requests left in RateLimit-Remaining (GitLab's
// headers, https://docs.gitlab.com/administration/settings/user_and_ip_rate_limits/#response-headers).
func rateLimited(h http.Header) bool {
	if h.Get("Retry-After") != "" {
		return true
	}
	return strings.TrimSpace(h.Get("RateLimit-Remaining")) == "0"
}

// retryAfter reads how long to wait from a response's headers, in order:
// Retry-After (seconds or an HTTP date), RateLimit-Reset (a Unix time on
// GitLab; a small number is taken for seconds), RateLimit-ResetTime (an
// HTTP date). It is 0 when none is usable, and at most maxRetryAfter.
// GitLab's Projects, Groups and Users APIs answer 429 without any of them:
// the throttle then backs off on its own.
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
		if n, err := strconv.ParseInt(strings.TrimSpace(h.Get("RateLimit-Reset")), 10, 64); err == nil {
			// A Unix time is far above any sensible number of seconds.
			if n > 1_000_000_000 {
				d, ok = time.Unix(n, 0).Sub(at), true
			} else {
				d, ok = seconds(n), true
			}
		}
	}
	if !ok {
		if t, err := http.ParseTime(strings.TrimSpace(h.Get("RateLimit-ResetTime"))); err == nil {
			d, ok = t.Sub(at), true
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

// conflict is a ClassConflict error of op: the state changed or does not
// allow the request; decide again.
func conflict(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassConflict, Err: fmt.Errorf(format, args...)}
}

// notFound is a ClassNotFound error of op that wraps ErrNotFound.
func notFound(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassNotFound, Err: fmt.Errorf(format+": %w", append(args, platform.ErrNotFound)...)}
}

// unknown is a ClassUnknown error of op with err's message: something the
// driver cannot judge, never a missing thing. It does not wrap err, whose
// class (a 404's ClassNotFound, ErrNotFound) platform.ClassOf would
// otherwise find beneath it.
func unknown(op string, err error) error {
	text := err.Error()
	var pe *platform.Error
	if errors.As(err, &pe) && pe.Err != nil {
		text = strings.TrimSuffix(pe.Err.Error(), ": "+platform.ErrNotFound.Error())
	}
	return &platform.Error{Op: op, Class: platform.ClassUnknown, Status: statusOf(err), Err: errors.New(text)}
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
