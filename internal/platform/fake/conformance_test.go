package fake_test

import (
	"fmt"
	"path"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

// TestConformance runs the platform contract suite against the fake in
// every flavor.
func TestConformance(t *testing.T) {
	t.Parallel()
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea, fake.Forgejo} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			conformance.Run(t, func(t *testing.T) conformance.Fixture { return newFixture(t, f) })
		})
	}
}

// TestConformanceGit runs the suite against the fake in git mode, where
// branches are real and pull requests follow them. The fixture's pushes are
// people's (PushFiles), so no violation may appear.
func TestConformanceGit(t *testing.T) {
	t.Parallel()
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea, fake.Forgejo} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			conformance.Run(t, func(t *testing.T) conformance.Fixture { return newGitFixture(t, f) })
		})
	}
}

// fixture is conformance.Fixture over a fresh fake platform.
type fixture struct {
	p                     *fake.Platform
	reader, writer, other platform.Account
	forks                 map[string]string // repository id → fork id
	// git is set in git mode, where branches get commits; pushes counts
	// them, to name their files.
	git    bool
	pushes int
}

// newGitFixture is newFixture in git mode, with a token for each account.
func newGitFixture(t *testing.T, f fake.Flavor) *fixture {
	var opts []fake.Option
	if f == fake.GitHub {
		// A GitHub App's writer: API commits and rules read upfront, which
		// the suite checks where they are offered.
		opts = append(opts, fake.WithAPICommits(), fake.WithPreflight())
	}
	fx := newFixture(t, f, opts...)
	srv, err := fx.p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatalf("ServeGit: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if v := fx.p.Violations(); len(v) > 0 {
			t.Errorf("violations: %q", v)
		}
	})
	for _, a := range []platform.Account{fx.reader, fx.writer, fx.other} {
		fx.p.SetToken(a, a.Login+"-token")
	}
	fx.git = true
	fx.check(t)
	return fx
}

// push adds a commit to branch of the repository with id as author, from
// the default branch when the branch is missing (git mode only).
func (fx *fixture) push(t *testing.T, repoID, branch string, author platform.Account) {
	t.Helper()
	if !fx.git {
		return
	}
	fx.pushes++
	file := fmt.Sprintf("changes/%d.md", fx.pushes)
	if _, err := fx.p.PushFiles(repoID, branch, map[string][]byte{file: []byte(branch + "\n")}, author, time.Time{}); err != nil {
		t.Fatalf("PushFiles(%s): %v", branch, err)
	}
}

func newFixture(t *testing.T, f fake.Flavor, opts ...fake.Option) *fixture {
	p := fake.New("git.example.com", append([]fake.Option{fake.WithFlavor(f)}, opts...)...)
	fx := &fixture{
		p:      p,
		reader: p.AddAccount("touchmark-reader", platform.KindBot),
		writer: p.AddAccount("touchmark-writer", platform.KindBot),
		other:  p.AddAccount("jdoe", platform.KindUser),
		forks:  map[string]string{},
	}
	fx.check(t)
	t.Cleanup(func() {
		if err := p.Err(); err != nil {
			t.Errorf("fake setup: %v", err)
		}
	})
	return fx
}

// check fails the test on a setup error of the fake.
func (fx *fixture) check(t *testing.T) {
	t.Helper()
	if err := fx.p.Err(); err != nil {
		t.Fatal(err)
	}
}

func (fx *fixture) Reader() platform.Reader { return fx.p.Reader(fx.reader) }
func (fx *fixture) Writer() platform.Writer { return fx.p.Writer(fx.writer) }
func (fx *fixture) Namespace() string       { return "conformance" }

func (fx *fixture) Account(role conformance.Role) platform.Account {
	switch role {
	case conformance.RoleReader:
		return fx.reader
	case conformance.RoleWriter:
		return fx.writer
	}
	return fx.other
}

func (fx *fixture) CreateRepo(t *testing.T, spec conformance.RepoSpec) platform.Repo {
	t.Helper()
	p := path.Join(fx.Namespace(), spec.Group, spec.Name)
	repo := fx.p.AddRepo(platform.Repo{Path: p, Topics: spec.Topics, Fork: spec.Fork})
	fx.check(t)
	for _, f := range spec.Files {
		switch f.Mode {
		case "", fake.ModeFile, fake.ModeExecutable:
			fx.p.SetFile(repo.ID, f.Path, f.Content, f.Mode)
		case fake.ModeSymlink:
			fx.p.SetSymlink(repo.ID, f.Path, string(f.Content))
		case fake.ModeGitlink:
			fx.p.SetGitlink(repo.ID, f.Path, string(f.Content))
		default:
			t.Fatalf("CreateRepo: mode %q", f.Mode)
		}
	}
	if spec.Archived {
		fx.p.UpdateRepo(repo.ID, func(r *platform.Repo) { r.Archived = true })
	}
	if !spec.ReadOnly {
		fx.p.GrantWrite(repo.ID, fx.writer)
	}
	fx.p.GrantWrite(repo.ID, fx.other)
	fx.check(t)
	got, _ := fx.p.RepoByID(repo.ID)
	return got
}

// CreateBranch has nothing to do in memory mode: the fake keeps branches
// only as names on pull requests. In git mode a person pushes a commit.
func (fx *fixture) CreateBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	fx.push(t, repo.ID, name, fx.other)
}

func (fx *fixture) Head(t *testing.T, repo platform.Repo) string {
	t.Helper()
	return fx.p.Head(repo.ID)
}

func (fx *fixture) DeleteBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	fx.p.DeleteBranch(repo.ID, name)
	fx.check(t)
}

func (fx *fixture) CreatePR(t *testing.T, repo platform.Repo, spec conformance.PRSpec) platform.PR {
	t.Helper()
	pr := platform.PR{
		Head:   spec.Head,
		Base:   spec.Base,
		Title:  spec.Title,
		Body:   spec.Body,
		Labels: spec.Labels,
		Author: fx.Account(spec.Author),
	}
	headRepo := repo.ID
	if spec.Fork {
		pr.HeadRepoID = fx.fork(t, repo, pr.Author)
		headRepo = pr.HeadRepoID
	}
	fx.push(t, headRepo, spec.Head, pr.Author)
	n := fx.p.AddPR(repo.ID, pr)
	fx.check(t)
	return fx.p.PR(repo.ID, n)
}

// fork returns the id of the fork of repo owned by owner, creating it once.
func (fx *fixture) fork(t *testing.T, repo platform.Repo, owner platform.Account) string {
	t.Helper()
	if id, ok := fx.forks[repo.ID]; ok {
		return id
	}
	f := fx.p.AddRepo(platform.Repo{Path: owner.Login + "/" + path.Base(repo.Path), Fork: true})
	fx.check(t)
	fx.forks[repo.ID] = f.ID
	return f.ID
}

func (fx *fixture) SetPRState(t *testing.T, repo platform.Repo, number int64, state platform.PRState, by conformance.Role) {
	t.Helper()
	var closer *platform.Account
	if state != platform.Open {
		a := fx.Account(by)
		closer = &a
	}
	fx.p.SetPRState(repo.ID, number, state, closer, time.Time{})
	fx.check(t)
}

func (fx *fixture) Comments(t *testing.T, repo platform.Repo, number int64) []string {
	t.Helper()
	var out []string
	for _, c := range fx.p.Comments(repo.ID, number) {
		out = append(out, c.Body)
	}
	return out
}
