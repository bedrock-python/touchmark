package bitbucketdc

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
// request httpx refused as invalid, a certificate the client does not
// trust, a response over the size bound or of an unexpected shape as
// unknown (none of them gets better by retrying), any other transport
// failure as transient. A canceled context stays unclassified. Messages
// never hold the credential nor headers.
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
// Bitbucket Data Center's error body is {"errors": [{"context": …,
// "message": …, "exceptionName": …}]} for 401, 403, 404 and 409 (REST
// intro, "Errors"); the exception names are Java class names the reference
// does not list (com.atlassian.bitbucket.repository.NoSuchRepositoryException
// and the like). A 401 of an authenticated user is a missing permission,
// not a refused credential (see authenticated). A rate limit is 429;
// whether it carries Retry-After is not documented, and one is honoured
// when it does.
func (c *client) statusError(op string, se *httpx.StatusError) error {
	m := parseMessage(se.Snippet)
	detail := m.text()
	if detail == "" {
		detail = se.Snippet
	}
	msg := c.mask(describe(se, detail))
	e := &platform.Error{Op: op, Class: statusClass(se.Status), Status: se.Status}
	if se.Status == http.StatusUnauthorized && authenticated(se.Header, m) {
		e.Class = platform.ClassPermission
	}
	switch se.Status {
	case http.StatusNotFound, http.StatusGone:
		e.Err = &notFoundError{exceptions: m.exceptions(), err: fmt.Errorf("%s: %w", msg, platform.ErrNotFound)}
		return e
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		e.RetryAfter = retryAfter(se.Header, now())
	}
	e.Err = errors.New(msg)
	return e
}

// authenticated reports whether a 401 answered a user Bitbucket knows: it
// answers 401 both to a credential it refuses and to a user without the
// permission a resource needs ("The currently authenticated user has
// insufficient permissions", throughout the reference). The second carries
// the user's name in X-AUSERNAME, as every authenticated answer does, or
// names an AuthorisationException; a refused or missing credential does
// neither.
func authenticated(h http.Header, m apiMessage) bool {
	if strings.TrimSpace(h.Get("X-AUSERNAME")) != "" {
		return true
	}
	for _, e := range m.exceptions() {
		simple := e[strings.LastIndexByte(e, '.')+1:]
		if simple == "AuthorisationException" || simple == "AuthorizationException" {
			return true
		}
	}
	return false
}

// notFoundError is the error of a 404 (or 410): the exception names of
// Bitbucket's body (none when the body is no Bitbucket error), over the
// ErrNotFound it wraps.
type notFoundError struct {
	exceptions []string
	err        error
}

func (e *notFoundError) Error() string { return e.err.Error() }
func (e *notFoundError) Unwrap() error { return e.err }

// missingException reports whether err is a 404 of the API whose body names
// an exception whose simple name is one of names (NoSuchPathException);
// ok is false when err is no 404 of the API.
func missingException(err error, names ...string) (named, ok bool) {
	var nf *notFoundError
	if !errors.As(err, &nf) {
		return false, false
	}
	for _, e := range nf.exceptions {
		simple := e[strings.LastIndexByte(e, '.')+1:]
		if slices.Contains(names, simple) {
			return true, true
		}
	}
	return false, true
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

// apiMessage is Bitbucket Data Center's error body.
type apiMessage struct {
	Errors []struct {
		Context       *string `json:"context"`
		Message       string  `json:"message"`
		ExceptionName *string `json:"exceptionName"`
	} `json:"errors"`
}

// text returns the messages on one line, each with its context (a field)
// when it names one.
func (m apiMessage) text() string {
	parts := make([]string, 0, len(m.Errors))
	for _, e := range m.Errors {
		t := e.Message
		if e.Context != nil && *e.Context != "" {
			t = *e.Context + ": " + t
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(strings.FieldsFunc(strings.Join(parts, "; "), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// exceptions returns the exception names of the body.
func (m apiMessage) exceptions() []string {
	var out []string
	for _, e := range m.Errors {
		if e.ExceptionName != nil && *e.ExceptionName != "" {
			out = append(out, *e.ExceptionName)
		}
	}
	return out
}

// parseMessage decodes an error body; a body of another shape gives the
// zero apiMessage.
func parseMessage(snippet string) apiMessage {
	var m apiMessage
	if json.Unmarshal([]byte(snippet), &m) != nil {
		return apiMessage{}
	}
	return m
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

// retryAfter reads how long to wait from a response's headers, in order:
// Retry-After (seconds or an HTTP date), then X-RateLimit-Reset (seconds
// until the window resets, or a Unix time when it is large), should a
// proxy or a later version send one: Bitbucket Data Center documents
// neither. It is 0 when none is usable, and at most maxRetryAfter.
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

// unknown is a ClassUnknown error of op that says what happened (format
// and args) and, when err is not nil, what err says, with err's status but
// without the ErrNotFound err may wrap: a 404 that must not count as a
// missing file.
func unknown(op string, err error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		msg += ": " + detail(err)
	}
	return &platform.Error{Op: op, Class: platform.ClassUnknown, Status: statusOf(err), Err: errors.New(msg)}
}

// detail returns what err says without its class: the message of the
// *platform.Error in it, without the ErrNotFound suffix.
func detail(err error) string {
	var pe *platform.Error
	if errors.As(err, &pe) && pe.Err != nil {
		return strings.TrimSuffix(pe.Err.Error(), ": "+platform.ErrNotFound.Error())
	}
	return err.Error()
}

// statusOf returns the HTTP status of a classified error, 0 for none.
func statusOf(err error) int {
	var pe *platform.Error
	if errors.As(err, &pe) {
		return pe.Status
	}
	return 0
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
