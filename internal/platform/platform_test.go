package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestClassString(t *testing.T) {
	t.Parallel()
	want := map[Class]string{
		ClassUnknown:     "unknown",
		ClassTransient:   "transient",
		ClassRateLimited: "rate-limited",
		ClassAuth:        "auth",
		ClassPermission:  "permission",
		ClassNotFound:    "not-found",
		ClassConflict:    "conflict",
		ClassPolicy:      "policy",
		ClassInvalid:     "invalid",
		ClassUnsupported: "unsupported",
	}
	seen := map[string]Class{}
	for c := ClassUnknown; c <= ClassUnsupported; c++ {
		got := c.String()
		if got != want[c] {
			t.Errorf("Class(%d).String() = %q, want %q", c, got, want[c])
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("classes %d and %d share the name %q", prev, c, got)
		}
		seen[got] = c
	}
	if got := (ClassUnsupported + 1).String(); got != "unknown" {
		t.Errorf("out-of-range class = %q, want unknown", got)
	}
	if got := Class(255).String(); got != "unknown" {
		t.Errorf("Class(255) = %q, want unknown", got)
	}
}

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "all parts",
			err: &Error{Op: "push", Class: ClassPolicy, Status: 403, Rule: "GH013",
				Err: errors.New("protected branch")},
			want: "push: policy (HTTP 403): rule GH013: protected branch",
		},
		{
			name: "no rule",
			err:  &Error{Op: "list pull requests", Class: ClassNotFound, Status: 404, Err: ErrNotFound},
			want: "list pull requests: not-found (HTTP 404): not found",
		},
		{
			name: "no status",
			err:  &Error{Op: "read file", Class: ClassTransient, Err: io.EOF},
			want: "read file: transient: EOF",
		},
		{
			name: "no op",
			err:  &Error{Class: ClassAuth, Status: 401, Err: errors.New("bad credentials")},
			want: "auth (HTTP 401): bad credentials",
		},
		{
			name: "no err",
			err:  &Error{Op: "target", Class: ClassPermission, Status: 403, Rule: "workflows"},
			want: "target: permission (HTTP 403): rule workflows",
		},
		{
			name: "class only",
			err:  &Error{},
			want: "unknown",
		},
		{
			name: "retry after is not printed",
			err:  &Error{Op: "create pull request", Class: ClassRateLimited, Status: 429, RetryAfter: time.Minute},
			want: "create pull request: rate-limited (HTTP 429)",
		},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: Error() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestErrorNil(t *testing.T) {
	t.Parallel()
	var e *Error
	if got := e.Error(); got != "<nil>" {
		t.Errorf("nil Error() = %q", got)
	}
	if e.Unwrap() != nil {
		t.Error("nil Unwrap() != nil")
	}
	// A typed nil in a chain must not panic the helpers that walk it.
	err := fmt.Errorf("wrapped: %w", e)
	if got := ClassOf(err); got != ClassUnknown {
		t.Errorf("ClassOf(typed nil in chain) = %v", got)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("typed nil matched ErrNotFound")
	}
}

func TestErrorUnwrap(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("list: %w", &Error{Op: "list pull requests", Class: ClassNotFound, Status: 404,
		Err: fmt.Errorf("acme/api: %w", ErrNotFound)})
	if !errors.Is(err, ErrNotFound) {
		t.Error("errors.Is(ErrNotFound) = false through *Error")
	}
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 404 {
		t.Errorf("errors.As = %v", pe)
	}
}

func TestClassOf(t *testing.T) {
	t.Parallel()
	deadline, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	<-deadline.Done()
	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()

	perm := &Error{Op: "target", Class: ClassPermission, Status: 403, Rule: "workflows"}
	for _, tc := range []struct {
		name string
		err  error
		want Class
	}{
		{"nil", nil, ClassUnknown},
		{"plain", errors.New("boom"), ClassUnknown},
		{"io.EOF", io.EOF, ClassUnknown},
		{"*Error", perm, ClassPermission},
		{"wrapped *Error", fmt.Errorf("a: %w", fmt.Errorf("b: %w", perm)), ClassPermission},
		{"joined *Error", errors.Join(errors.New("other"), perm), ClassPermission},
		{"*Error value in chain", fmt.Errorf("x: %w", &Error{Class: ClassRateLimited, Status: 429}), ClassRateLimited},
		{"ErrNotFound", ErrNotFound, ClassNotFound},
		{"wrapped ErrNotFound", fmt.Errorf("read: %w", ErrNotFound), ClassNotFound},
		{"ErrExists", ErrExists, ClassConflict},
		{"wrapped ErrExists", fmt.Errorf("create: %w", ErrExists), ClassConflict},
		{"ErrNotRegular", ErrNotRegular, ClassUnknown},
		{"ErrTooLarge", fmt.Errorf("x: %w", ErrTooLarge), ClassUnknown},
		{"DeadlineExceeded", context.DeadlineExceeded, ClassTransient},
		{"ctx deadline", deadline.Err(), ClassTransient},
		{"wrapped deadline", fmt.Errorf("fetch: %w", context.DeadlineExceeded), ClassTransient},
		{"Canceled", context.Canceled, ClassUnknown},
		{"ctx canceled", canceled.Err(), ClassUnknown},
		{"wrapped canceled", fmt.Errorf("fetch: %w", context.Canceled), ClassUnknown},
		// The first *Error decides over a sentinel beneath it.
		{"*Error over ErrNotFound", &Error{Class: ClassAuth, Status: 401, Err: ErrNotFound}, ClassAuth},
		{"*Error over deadline", &Error{Class: ClassPermission, Err: context.DeadlineExceeded}, ClassPermission},
		{"outer *Error first", &Error{Class: ClassConflict, Err: &Error{Class: ClassTransient}}, ClassConflict},
		// An unclassified *Error defers to what it wraps.
		{"unknown *Error over ErrNotFound", &Error{Op: "read file", Status: 404, Err: ErrNotFound}, ClassNotFound},
		{"unknown *Error over *Error", &Error{Op: "outer", Err: fmt.Errorf("x: %w", perm)}, ClassPermission},
		{"unknown *Error over deadline", &Error{Err: context.DeadlineExceeded}, ClassTransient},
		{"unknown *Error over canceled", &Error{Err: context.Canceled}, ClassUnknown},
		{"unknown *Error alone", &Error{Op: "x", Status: 418}, ClassUnknown},
		{"joined unknown *Error and sentinel", errors.Join(&Error{Op: "x"}, ErrExists), ClassConflict},
		// A classified sibling counts wherever it sits in the tree.
		{"joined unknown *Error and classified", errors.Join(&Error{Op: "list page 2", Status: 500}, &Error{Class: ClassAuth}), ClassAuth},
		{"joined classified and unknown *Error", errors.Join(&Error{Class: ClassRateLimited}, &Error{Op: "x"}), ClassRateLimited},
		{"multi-%w unknown first", fmt.Errorf("%w; %w", &Error{Op: "a"}, &Error{Class: ClassAuth}), ClassAuth},
		{"joined sentinel and classified", errors.Join(ErrNotFound, &Error{Class: ClassRateLimited}), ClassRateLimited},
		{"nested join", fmt.Errorf("x: %w", errors.Join(errors.New("a"), fmt.Errorf("b: %w", &Error{Class: ClassPolicy}))), ClassPolicy},
		{"first classified wins in a join", errors.Join(&Error{Class: ClassTransient}, &Error{Class: ClassAuth}), ClassTransient},
		{"typed nil in a join", errors.Join((*Error)(nil), &Error{Class: ClassAuth}), ClassAuth},
	} {
		if got := ClassOf(tc.err); got != tc.want {
			t.Errorf("%s: ClassOf = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestErrorNoSecretFields guards the message format: only the documented
// fields appear, so a driver that masks Err cannot leak through the rest.
func TestErrorNoSecretFields(t *testing.T) {
	t.Parallel()
	e := &Error{Op: "op", Class: ClassAuth, Status: 401, RetryAfter: 42 * time.Second, Err: errors.New("masked")}
	msg := e.Error()
	for _, bad := range []string{"42", "Retry", "Authorization"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message %q contains %q", msg, bad)
		}
	}
}

// TestFileErrors: the per-repository errors of BatchReader.ReadFiles keep
// their classes and sentinels for errors.Is and ClassOf, and name the
// failures only.
func TestFileErrors(t *testing.T) {
	t.Parallel()
	missing := &Error{Op: "read files", Class: ClassNotFound, Err: fmt.Errorf("acme/a: %w", ErrNotFound)}
	link := fmt.Errorf("acme/b: x is a symlink: %w", ErrNotRegular)
	var err error = FileErrors{nil, missing, link}
	var fe FileErrors
	switch {
	case !errors.As(err, &fe) || len(fe) != 3 || fe[0] != nil:
		t.Errorf("errors.As: %v", fe)
	case !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrNotRegular) || errors.Is(err, ErrTooLarge):
		t.Errorf("errors.Is on %v", err)
	case ClassOf(err) != ClassNotFound:
		t.Errorf("ClassOf = %v", ClassOf(err))
	case err.Error() != missing.Error()+"; "+link.Error():
		t.Errorf("message %q", err.Error())
	}
	if msg := (FileErrors{nil, nil}).Error(); msg != "no file errors" {
		t.Errorf("empty: %q", msg)
	}
}
