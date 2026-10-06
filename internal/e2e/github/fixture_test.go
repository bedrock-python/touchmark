package githube2e

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
)

// fixture is conformance.Fixture over a fresh fake GitHub: the reader and
// the writer are the Apps of the world, the other account is alice. The
// writer's installation holds the repositories the fixture creates, but
// read-only ones (ReadOnly: the App is not installed there, so Target meets
// a repository outside its installation).
type fixture struct {
	w        *world
	reader   platform.Reader
	writer   platform.Writer
	accounts map[conformance.Role]platform.Account
	forks    map[string]string // repository path → alice's fork
	topics   map[string][]string
	pushes   int
}

func newFixture(t *testing.T, opts worldOptions) *fixture {
	t.Helper()
	opts.selected = true
	w := newWorld(t, opts)
	fx := &fixture{w: w, forks: map[string]string{}, topics: map[string][]string{}}
	fx.reader, fx.writer = w.drivers()
	fx.accounts = map[conformance.Role]platform.Account{
		conformance.RoleReader: account(w.readApp.Bot),
		conformance.RoleWriter: account(w.writeApp.Bot),
		conformance.RoleOther:  account(w.personAcc),
	}
	t.Cleanup(w.violations)
	return fx
}

var (
	_ conformance.Fixture = (*fixture)(nil)
	_ conformance.Renamer = (*fixture)(nil)
)

func (fx *fixture) Reader() platform.Reader                        { return fx.reader }
func (fx *fixture) Writer() platform.Writer                        { return fx.writer }
func (fx *fixture) Namespace() string                              { return org }
func (fx *fixture) Account(role conformance.Role) platform.Account { return fx.accounts[role] }

// login is the login of role on the fake.
func (fx *fixture) login(role conformance.Role) string { return fx.accounts[role].Login }

// CreateRepo creates a public repository of acme with spec's files,
// committed by its owner, topics and archive flag; alice may write to it. A fork is a fork of
// octo-org's repository of the same name. GitHub has no nested
// namespaces: a spec with a Group skips.
func (fx *fixture) CreateRepo(t *testing.T, spec conformance.RepoSpec) platform.Repo {
	t.Helper()
	if spec.Group != "" {
		t.Skip("GitHub has no nested namespaces")
	}
	var files []ghfake.File
	for _, f := range spec.Files {
		files = append(files, ghfake.File{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	var r ghfake.Repo
	if spec.Fork {
		try(fx.w.srv.CreateRepo(ghfake.RepoSpec{Owner: outsider, Name: spec.Name, Files: files})).of(t)
		r = try(fx.w.srv.Fork(outsider+"/"+spec.Name, org)).of(t)
		if len(spec.Topics) > 0 {
			check(t, fx.w.srv.SetTopics(r.FullName, spec.Topics...))
		}
		if spec.Archived {
			check(t, fx.w.srv.SetArchived(r.FullName, true))
		}
		r, _ = fx.w.srv.Repo(r.FullName)
	} else {
		r = try(fx.w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: spec.Name, Files: files, Topics: spec.Topics,
			Archived: spec.Archived})).of(t)
	}
	check(t, fx.w.srv.Grant(r.FullName, person, "write"))
	if !spec.ReadOnly {
		fx.w.selectRepo(r.Name)
	}
	fx.topics[r.FullName] = slices.Clone(spec.Topics)
	return fx.w.repo(r, len(spec.Files) == 0, spec.Topics)
}

// commit adds a commit by author to branch of the repository at p,
// creating the branch from the default branch when it is missing, so a
// pull request from it has changes.
func (fx *fixture) commit(t *testing.T, p, branch, author string) {
	t.Helper()
	fx.pushes++
	file := fmt.Sprintf("changes/%d.md", fx.pushes)
	try(fx.w.srv.Commit(p, ghfake.CommitSpec{Branch: branch, Author: author,
		Files: []ghfake.File{{Path: file, Content: []byte(branch + "\n")}}, Message: "conformance: change " + strconv.Itoa(fx.pushes)})).of(t)
}

func (fx *fixture) CreateBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	fx.commit(t, repo.Path, name, person)
}

func (fx *fixture) Head(t *testing.T, repo platform.Repo) string {
	t.Helper()
	return fx.w.srv.Branch(repo.Path, repo.DefaultBranch)
}

func (fx *fixture) DeleteBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	check(t, fx.w.srv.SetBranch(repo.Path, name, "", person))
}

// CreatePR commits to the head branch as the author, in alice's fork with
// spec.Fork, and opens the pull request as the author.
func (fx *fixture) CreatePR(t *testing.T, repo platform.Repo, spec conformance.PRSpec) platform.PR {
	t.Helper()
	author := fx.login(spec.Author)
	headRepo := ""
	if spec.Fork {
		headRepo = fx.fork(t, repo)
		fx.commit(t, headRepo, spec.Head, author)
	} else {
		fx.commit(t, repo.Path, spec.Head, author)
	}
	pr := try(fx.w.srv.OpenPR(repo.Path, ghfake.PRSpec{Head: spec.Head, Base: spec.Base, HeadRepo: headRepo,
		Title: spec.Title, Body: spec.Body, Labels: spec.Labels, Author: author})).of(t)
	return fx.pr(repo, pr)
}

// fork returns alice's fork of repo, forking it on first use.
func (fx *fixture) fork(t *testing.T, repo platform.Repo) string {
	t.Helper()
	if f, ok := fx.forks[repo.Path]; ok {
		return f
	}
	f := try(fx.w.srv.Fork(repo.Path, person)).of(t)
	fx.forks[repo.Path] = f.FullName
	return f.FullName
}

// pr converts a pull request of the fake.
func (fx *fixture) pr(repo platform.Repo, p ghfake.PR) platform.PR {
	out := platform.PR{
		Number: p.Number, Draft: p.Draft, Head: p.Head, HeadSHA: p.HeadSHA, Base: p.Base, RepoID: repo.ID,
		HeadRepoID: strconv.FormatInt(p.HeadRepoID, 10), BaseExists: true, Title: p.Title, Body: p.Body,
		Labels: p.Labels, Author: account(p.Author), CreatedAt: p.CreatedAt, ClosedAt: p.ClosedAt,
		State: platform.Open,
	}
	switch {
	case p.Merged:
		out.State = platform.Merged
	case p.State == "closed":
		out.State = platform.Closed
	}
	if p.ClosedBy != nil {
		a := account(*p.ClosedBy)
		out.ClosedBy = &a
	}
	return out
}

func (fx *fixture) SetPRState(t *testing.T, repo platform.Repo, number int64, state platform.PRState, by conformance.Role) {
	t.Helper()
	login := fx.login(by)
	switch state {
	case platform.Open:
		check(t, fx.w.srv.SetPRState(repo.Path, number, "open", login))
	case platform.Closed:
		check(t, fx.w.srv.SetPRState(repo.Path, number, "closed", login))
	case platform.Merged:
		try(fx.w.srv.MergePR(repo.Path, number, ghfake.MergeCommit, login)).of(t)
	default:
		t.Fatalf("SetPRState: state %q", state)
	}
}

func (fx *fixture) Comments(t *testing.T, repo platform.Repo, number int64) []string {
	t.Helper()
	var out []string
	for _, c := range fx.w.srv.Comments(repo.Path, number) {
		out = append(out, c.Body)
	}
	return out
}

// RenameRepo renames repo in acme; GitHub answers the old path with a 301
// to /repositories/<id>.
func (fx *fixture) RenameRepo(t *testing.T, repo platform.Repo, name string) platform.Repo {
	t.Helper()
	r := try(fx.w.srv.RenameRepo(repo.Path, name)).of(t)
	if i := slices.Index(fx.w.selectedRepos, path.Base(repo.Path)); i >= 0 {
		fx.w.selectedRepos[i] = r.Name
	}
	return fx.w.repo(r, repo.Empty, fx.topics[repo.Path])
}

// RenamedAccount skips: GitHub frees an old login at once and answers it
// with 404, it never leads to the renamed account ("Links to your previous
// profile page ... will return a 404 error", docs.github.com, changing
// your username). hub.yml names accounts by their current logins.
func (fx *fixture) RenamedAccount(t *testing.T) (string, platform.Account) {
	t.Helper()
	t.Skip("GitHub does not redirect an old login: GET /users/<old login> is 404 once the account is renamed")
	return "", platform.Account{}
}
