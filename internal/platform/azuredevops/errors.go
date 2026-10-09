package azuredevops

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

// typeKeys of the 404s the driver tells apart (the typeKey of Azure
// DevOps' error body; the REST reference of Items - Get and the answers of
// public repositories, 2026-10-09).
const (
	keyItemNotFound    = "GitItemNotFoundException"
	keyUnresolvable    = "GitUnresolvableToCommitException"
	keyRepoNotFound    = "GitRepositoryNotFoundException"
	keyPRExists        = "GitPullRequestExistsException"
	keyProjectNotFound = "ProjectDoesNotExistWithNameException"
)

// Error codes in messages: TF400733 is a request blocked by the rate limit
// (https://learn.microsoft.com/en-us/azure/devops/integrate/concepts/rate-limits),
// TF400813 a credential the organization does not authorize, TF401179 an
// active pull request from the same source and target that exists already
// (assumed: the message Azure Repos is known to send; to confirm live).
const (
	tfRateLimited = "TF400733"
	tfNotAuthed   = "TF400813"
	tfPRExists    = "TF401179"
)

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
// unknown, any other transport failure as transient. A canceled context
// stays unclassified. Messages never hold the credential nor headers.
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

// apiMessage is Azure DevOps' error body: {"$id": "1", "innerException":
// null, "message": "TF401174: The item … could not be found …",
// "typeName": "…GitItemNotFoundException, …", "typeKey":
// "GitItemNotFoundException", "errorCode": 0, "eventId": 3000}.
type apiMessage struct {
	Message string `json:"message"`
	TypeKey string `json:"typeKey"`
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

// apiFailure is the error under a classified non-2xx answer: its masked
// description, and the typeKey of the body ("" when none).
type apiFailure struct {
	msg     string
	typeKey string
	err     error // ErrNotFound for a 404, nil otherwise
}

func (e *apiFailure) Error() string { return e.msg }
func (e *apiFailure) Unwrap() error { return e.err }

// typeKeyOf returns the typeKey of the API error in err, "" when there is
// none.
func typeKeyOf(err error) string {
	var f *apiFailure
	if errors.As(err, &f) {
		return f.typeKey
	}
	return ""
}

// statusError classifies a non-2xx response of op: 401 auth, 403
// permission, 404 not found (with its typeKey, which callers read), 409
// conflict, 429 or TF400733 rate limited, any status with Retry-After rate
// limited too, 5xx and 408 transient, other 4xx invalid. A 401 or a
// TF400813 is auth whatever else it says.
func (c *client) statusError(op string, se *httpx.StatusError) error {
	m := parseMessage(se.Snippet)
	detail := oneLine(m.Message)
	if detail == "" {
		detail = se.Snippet
	}
	if m.TypeKey != "" {
		detail += " (" + m.TypeKey + ")"
	}
	f := &apiFailure{msg: c.mask(describe(se, detail)), typeKey: m.TypeKey}
	e := &platform.Error{Op: op, Class: statusClass(se.Status), Status: se.Status, Err: f}
	switch {
	case se.Status == http.StatusNotFound:
		f.err = platform.ErrNotFound
		f.msg += ": " + platform.ErrNotFound.Error()
	case se.Status == http.StatusTooManyRequests || strings.Contains(m.Message, tfRateLimited) || se.Header.Get("Retry-After") != "":
		// Retry-After on any refusal means the platform wants a pause
		// (https://learn.microsoft.com/en-us/azure/devops/integrate/concepts/rate-limits).
		e.Class, e.RetryAfter = platform.ClassRateLimited, retryAfter(se.Header, now())
	case strings.Contains(m.Message, tfNotAuthed):
		e.Class = platform.ClassAuth
	case se.Status == http.StatusBadRequest && (m.TypeKey == keyPRExists || strings.Contains(m.Message, tfPRExists)):
		e.Class = platform.ClassConflict
	}
	return e
}

// oneLine joins s's words with single spaces.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
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
	case status == http.StatusRequestTimeout:
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

// retryAfter reads how long to wait from a response's headers, in order:
// Retry-After (seconds or an HTTP date), then X-RateLimit-Reset, a Unix
// time (seconds to wait when it is small). It is 0 when none is usable,
// and at most maxRetryAfter.
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

// conflict is a ClassConflict error of op.
func conflict(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassConflict, Err: fmt.Errorf(format, args...)}
}

// unsupported is a ClassUnsupported error of op.
func unsupported(op, format string, args ...any) error {
	return &platform.Error{Op: op, Class: platform.ClassUnsupported, Err: fmt.Errorf(format, args...)}
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
	// The typeKey stays readable (typeKeyOf); the ErrNotFound does not.
	return &platform.Error{Op: op, Class: platform.ClassUnknown, Status: statusOf(err), Err: &apiFailure{msg: msg, typeKey: typeKeyOf(err)}}
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
