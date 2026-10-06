package fake_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// env is a platform with a reader, a writer and a person.
type env struct {
	t                     *testing.T
	p                     *fake.Platform
	reader, writer, other platform.Account
}

func newEnv(t *testing.T, opts ...fake.Option) *env {
	t.Helper()
	p := fake.New("github.com", opts...)
	e := &env{
		t:      t,
		p:      p,
		reader: p.AddAccount("acme-read[bot]", platform.KindBot),
		writer: p.AddAccount("acme-write[bot]", platform.KindBot),
		other:  p.AddAccount("jdoe", platform.KindUser),
	}
	e.ok()
	return e
}

// ok fails the test on a setup error.
func (e *env) ok() {
	e.t.Helper()
	if err := e.p.Err(); err != nil {
		e.t.Fatalf("setup: %v", err)
	}
}

// repo adds a repository with files ("path", "content" pairs) that the
// writer may write to.
func (e *env) repo(path string, files ...string) platform.Repo {
	e.t.Helper()
	r := e.p.AddRepo(platform.Repo{Path: path})
	for i := 0; i+1 < len(files); i += 2 {
		e.p.SetFile(r.ID, files[i], []byte(files[i+1]), "")
	}
	e.p.GrantWrite(r.ID, e.writer)
	e.ok()
	got, _ := e.p.RepoByID(r.ID)
	return got
}

// pr adds a pull request and returns its number.
func (e *env) pr(r platform.Repo, pr platform.PR) int64 {
	e.t.Helper()
	n := e.p.AddPR(r.ID, pr)
	e.ok()
	return n
}

// target returns the writer's target writer for r with contents and PRs.
func (e *env) target(r platform.Repo) platform.TargetWriter {
	e.t.Helper()
	tw, err := e.p.Writer(e.writer).Target(e.t.Context(), r, platform.Perms{Contents: true, PRs: true})
	if err != nil {
		e.t.Fatalf("Target: %v", err)
	}
	return tw
}

// wantClass checks the class of err.
func wantClass(t *testing.T, what string, err error, class platform.Class) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want class %v", what, class)
		return
	}
	if got := platform.ClassOf(err); got != class {
		t.Errorf("%s: class %v of %v, want %v", what, got, err, class)
	}
}

// wantIs checks that err wraps target.
func wantIs(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: %v, want an error wrapping %v", what, err, target)
	}
}

// wantRule checks that err is a *platform.Error with class, status and rule.
func wantRule(t *testing.T, what string, err error, class platform.Class, status int, rule string) {
	t.Helper()
	var pe *platform.Error
	if !errors.As(err, &pe) {
		t.Errorf("%s: %v is not a *platform.Error", what, err)
		return
	}
	if pe.Class != class || pe.Status != status || pe.Rule != rule {
		t.Errorf("%s: %v: class %v status %d rule %q, want %v %d %q", what, err, pe.Class, pe.Status, pe.Rule, class, status, rule)
	}
}

// wantSetupErr checks that the platform recorded a setup error mentioning
// want.
func wantSetupErr(t *testing.T, p *fake.Platform, want string) {
	t.Helper()
	err := p.Err()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Err() = %v, want one mentioning %q", err, want)
	}
}

func numbers(prs []platform.PR) []int64 {
	out := make([]int64, len(prs))
	for i, pr := range prs {
		out[i] = pr.Number
	}
	return out
}

func sameList[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func ptr[T any](v T) *T { return &v }
