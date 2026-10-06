package gitx

import (
	"errors"
	"strconv"
	"strings"
)

// Failure tells why a network command of a TargetRepo failed, so that
// callers can report failed:auth, failed:access, deferred:rate-limit or
// failed:transient instead of failed:git.
type Failure uint8

const (
	// FailureUnknown: anything else, a context error included (the caller
	// sees context.DeadlineExceeded or context.Canceled through
	// errors.Is), and an HTTP 4xx status that no other class names (400,
	// 404, 413, 422, …): retrying would give the same answer.
	FailureUnknown Failure = iota
	// FailureAuth: the remote refused the credential: HTTP 401, git's
	// "Authentication failed", "could not read Username" (a 401 with
	// prompts off), GitLab's "HTTP Basic: Access denied", GitHub's
	// "Invalid username or password". The credential is broken: three in
	// a row make the rest of the provider deferred:provider-down.
	FailureAuth
	// FailureRateLimited: HTTP 429.
	FailureRateLimited
	// FailureTransient: HTTP 5xx or 408, a host that did not resolve, a
	// connection refused, reset or timed out, an early EOF or an RPC
	// failure, and a command that ran into its own three-minute bound
	// (ErrNetworkTimeout).
	FailureTransient
	// FailurePermission: the credential was accepted, but the identity may
	// not do this in this repository: HTTP 403, GitHub's "Permission to …
	// denied". A refusal of one target (failed:access, or
	// blocked:permission for a push), which says nothing about the
	// credential: it does not count towards provider-down.
	FailurePermission
)

// ErrNetworkTimeout is wrapped by the error of a network command
// (RemoteRefs, FetchBranch, DeepenSince, FetchBlobs) that ran into its own
// bound of three minutes while the caller's context was
// still live: a transient failure (ClassifyFailure). It does not wrap
// context.DeadlineExceeded: an error that does is the caller's own
// deadline or cancellation (the run's --deadline, the target's budget,
// SIGTERM), which the caller tells by its context. Push reports its own
// timeout as a PushError whose Transient is true instead (the push may
// have been applied).
var ErrNetworkTimeout = errors.New("git network command timed out")

// ClassifyFailure classifies an error of RemoteRefs, FetchBranch,
// DeepenSince or FetchBlobs: ErrNetworkTimeout is FailureTransient, a git
// exit (an *Error) is classified by its stderr, and anything else is
// FailureUnknown.
//
// The stderr rules apply in this order: the authentication failures of
// FailureAuth; HTTP 403 and "Permission to … denied" (FailurePermission);
// HTTP 429 (FailureRateLimited); any other 4xx status but 408
// (FailureUnknown); a 5xx status, 408 or a network failure
// (FailureTransient). Statuses are read from curl's "The requested URL
// returned error: <status>" and git's "RPC failed; HTTP <status>".
func ClassifyFailure(err error) Failure {
	if errors.Is(err, ErrNetworkTimeout) {
		return FailureTransient
	}
	var e *Error
	if !errors.As(err, &e) {
		return FailureUnknown
	}
	return classifyStderr(e.Stderr)
}

// classifyStderr applies the stderr rules of ClassifyFailure.
func classifyStderr(stderr string) Failure {
	s := strings.ToLower(stderr)
	codes := httpStatuses(s)
	switch {
	case isAuthFailure(s, codes):
		return FailureAuth
	case isPermissionFailure(s, codes):
		return FailurePermission
	case codes[429]:
		return FailureRateLimited
	case hasPermanent4xx(codes):
		return FailureUnknown
	case codes[408] || has5xx(codes) || containsAny(s, transientMarkers):
		return FailureTransient
	}
	return FailureUnknown
}

// Transient reports whether a PushError may be retried after a reconciling
// read of the branch: its outcome is unknown, since the
// push may have been applied before the answer was lost. That is the case
// for the network failures of FailureTransient (a 5xx or 408 status, a
// connection lost, the push's own timeout) and for a server that applied
// the push but did not report the ref's status ("remote failed to report
// status"), unless the message also names another 4xx status, which a
// retry would meet again. Other statuses, rate limits (PushRateLimited)
// included, are not transient.
func (r PushResult) Transient() bool {
	if r.Status != PushError {
		return false
	}
	s := strings.ToLower(r.Message)
	codes := httpStatuses(s)
	if hasPermanent4xx(codes) {
		return false
	}
	return codes[408] || has5xx(codes) || containsAny(s, transientMarkers) || strings.Contains(s, "remote failed to report status")
}

// httpStatusPrefixes precede an HTTP status in git's stderr: curl's
// message, and git's own report of a failed smart-HTTP request.
var httpStatusPrefixes = []string{"the requested url returned error: ", "rpc failed; http "}

// httpStatuses returns the HTTP statuses s (lowercase) names after
// httpStatusPrefixes.
func httpStatuses(s string) map[int]bool {
	codes := map[int]bool{}
	for _, prefix := range httpStatusPrefixes {
		for rest := s; ; {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i+len(prefix):]
			if len(rest) >= 3 {
				if n, err := strconv.Atoi(rest[:3]); err == nil && n >= 100 && (len(rest) == 3 || rest[3] < '0' || rest[3] > '9') {
					codes[n] = true
				}
			}
		}
	}
	return codes
}

// has5xx reports whether codes hold a server error.
func has5xx(codes map[int]bool) bool {
	for c := range codes {
		if c >= 500 && c <= 599 {
			return true
		}
	}
	return false
}

// hasPermanent4xx reports whether codes hold a client error that retrying
// does not cure: every 4xx but 408 (Request Timeout) and 429, and neither
// 401 nor 403, which have classes of their own.
func hasPermanent4xx(codes map[int]bool) bool {
	for c := range codes {
		if c >= 400 && c <= 499 && c != 408 && c != 429 && c != 401 && c != 403 {
			return true
		}
	}
	return false
}

// Markers of git's stderr, lowercase.
var (
	// authMarkers are git's and the platforms' wordings of a refused
	// credential (see FailureAuth).
	authMarkers = []string{
		"authentication failed",
		"could not read username",
		"could not read password",
		"http basic: access denied",
		"invalid username or password",
	}
	// Curl's and git's wordings of network failures, old and new ("Failed
	// to connect to h port p after 0 ms: Could not connect to server",
	// "Couldn't connect to server", "Connection refused", …).
	transientMarkers = []string{
		"could not resolve host",
		"failed to connect",
		"connect to server",
		"connection refused",
		"connection reset",
		"timed out",
		"empty reply from server",
		"recv failure",
		"send failure",
		"early eof",
		"rpc failed",
		"unexpected disconnect",
		"the remote end hung up unexpectedly",
	}
)

// isAuthFailure reports whether git's stderr (lowercase, with its HTTP
// statuses) says the credential was refused.
func isAuthFailure(s string, codes map[int]bool) bool {
	return codes[401] || containsAny(s, authMarkers)
}

// isPermissionFailure reports whether git's stderr (lowercase, with its
// HTTP statuses) says the identity may not do this: HTTP 403, or GitHub's
// "remote: Permission to owner/repo.git denied to user."
func isPermissionFailure(s string, codes map[int]bool) bool {
	if codes[403] {
		return true
	}
	i := strings.Index(s, "permission to ")
	return i >= 0 && strings.Contains(s[i:], " denied")
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
