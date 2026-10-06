//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/platform/gitea"
)

// fixture is conformance.Fixture on the live forge. Every subtest gets a
// fresh private organisation, set up through the API by the admin:
//   - the team "read-all" reads every repository (the reader and the
//     writer are in it);
//   - the team "write" writes the repositories CreateRepo adds to it,
//     all but RepoSpec.ReadOnly ones (the writer is in it);
//   - the person is an owner.
//
// Repositories are private, as most targets are. Files come through the
// contents API; modes it cannot write (executables, symlinks, submodules)
// through a git push by the admin. Pull requests are opened by their
// author's own token: the writer's or the person's, from a fork of theirs
// with PRSpec.Fork.
type fixture struct {
	env *liveEnv
	// org is the fixture's organisation; upstream holds the sources of the
	// forks RepoSpec.Fork asks for, created on first use.
	org, upstream string
	// writeTeam is the id of the team that writes the repositories added
	// to it.
	writeTeam int64
	reader    platform.Reader
	writer    platform.Writer
	accounts  map[conformance.Role]platform.Account
	// forks are the forks made for PRSpec.Fork: "<repository path> <owner>"
	// → the fork's path.
	forks map[string]string
	// labels are the label ids by "<repository path> <name>".
	labels map[string]int64
	// n numbers the files the fixture commits.
	n int
}

// newFixture sets up a fixture with the drivers under test; see fixture.
func newFixture(t *testing.T, e *liveEnv) *fixture {
	t.Helper()
	fx := newOrg(t, e, "conf")
	fx.reader, fx.writer = newDrivers(t, e)
	fx.accounts = accountsOf(t, e, fx.reader)
	return fx
}

// newOrg sets up a fresh organisation named prefix-<random> with the
// fixture's teams and owner, without drivers: the platform alone.
func newOrg(t *testing.T, e *liveEnv, prefix string) *fixture {
	t.Helper()
	fx := &fixture{env: e, org: prefix + "-" + randHex(t, 4), forks: map[string]string{}, labels: map[string]int64{}}
	fx.createOrg(t, fx.org)
	fx.team(t, fx.org, "read-all", "read", true, e.Reader, e.Writer)
	fx.writeTeam = fx.team(t, fx.org, "write", "write", false, e.Writer)
	fx.addOwner(t, fx.org, e.Person)
	return fx
}

// newDrivers builds the gitea driver's reader and writer for the forge, as
// touchmark builds them: the provider from hub.yml, the token from the
// environment, the product's HTTP client.
func newDrivers(t testing.TB, e *liveEnv) (platform.Reader, platform.Writer) {
	t.Helper()
	rp := e.provider(t)
	r := construct(t, "NewReader", func() (platform.Reader, error) { return gitea.NewReader(rp, credential(e.Reader), e.HTTP) })
	w := construct(t, "NewWriter", func() (platform.Writer, error) { return gitea.NewWriter(rp, credential(e.Writer), e.HTTP) })
	return r, w
}

// construct calls the driver constructor name and fails the test on its
// error.
func construct[T any](t testing.TB, name string, f func() (T, error)) T {
	t.Helper()
	v, err := f()
	if err != nil {
		t.Fatalf("gitea.%s: %v", name, err)
	}
	return v
}

// The accounts as the platform reports them, found once per run.
var accountCache struct {
	sync.Mutex
	byRole map[conformance.Role]platform.Account
}

// accountsOf returns the accounts of the roles: id, login and email from the
// admin's view of each user, the kind from the reader driver's Lookup.
// Gitea and Forgejo expose no account type in their API (the bot type of
// Gitea is not in api.User), so the kind is the driver's convention; the
// suite checks that Self and Lookup agree on it and that it is known.
func accountsOf(t testing.TB, e *liveEnv, reader platform.Reader) map[conformance.Role]platform.Account {
	t.Helper()
	accountCache.Lock()
	defer accountCache.Unlock()
	if accountCache.byRole != nil {
		return accountCache.byRole
	}
	out := map[conformance.Role]platform.Account{}
	for role, a := range map[conformance.Role]account{
		conformance.RoleReader: e.Reader,
		conformance.RoleWriter: e.Writer,
		conformance.RoleOther:  e.Person,
	} {
		var u apiUser
		e.api(e.Admin).get(t, "/users/"+url.PathEscape(a.Login), &u)
		got, err := reader.Lookup(context.Background(), a.Login)
		if err != nil {
			t.Fatalf("the reader driver's Lookup(%s): %v", a.Login, err)
		}
		out[role] = platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Email: u.Email, Kind: got.Kind}
	}
	accountCache.byRole = out
	return out
}

func (fx *fixture) Reader() platform.Reader { return fx.reader }
func (fx *fixture) Writer() platform.Writer { return fx.writer }
func (fx *fixture) Namespace() string       { return fx.org }

func (fx *fixture) Account(role conformance.Role) platform.Account { return fx.accounts[role] }

// as returns the seeded account of a role.
func (fx *fixture) as(role conformance.Role) account {
	switch role {
	case conformance.RoleReader:
		return fx.env.Reader
	case conformance.RoleWriter:
		return fx.env.Writer
	}
	return fx.env.Person
}

func (fx *fixture) admin() forgeAPI { return fx.env.api(fx.env.Admin) }

// createOrg creates a private organisation owned by the admin.
func (fx *fixture) createOrg(t testing.TB, name string) {
	t.Helper()
	fx.admin().ok(t, http.MethodPost, "/orgs", map[string]any{
		"username": name, "visibility": "private", "repo_admin_change_team_access": false,
	}, nil)
}

// team creates a team of org with access to the code, issues and pull
// requests of its repositories (all of them with all) and the members.
func (fx *fixture) team(t testing.TB, org, name, access string, all bool, members ...account) int64 {
	t.Helper()
	var team struct {
		ID int64 `json:"id"`
	}
	fx.admin().ok(t, http.MethodPost, "/orgs/"+org+"/teams", map[string]any{
		"name": name, "permission": access, "includes_all_repositories": all, "can_create_org_repo": false,
		"units":     []string{"repo.code", "repo.issues", "repo.pulls"},
		"units_map": map[string]string{"repo.code": access, "repo.issues": access, "repo.pulls": access},
	}, &team)
	for _, m := range members {
		fx.admin().ok(t, http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", team.ID, url.PathEscape(m.Login)), nil, nil)
	}
	return team.ID
}

// addOwner puts a into the Owners team of org.
func (fx *fixture) addOwner(t testing.TB, org string, a account) {
	t.Helper()
	var teams []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	fx.admin().get(t, "/orgs/"+org+"/teams", &teams)
	for _, team := range teams {
		if team.Name == "Owners" {
			fx.admin().ok(t, http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", team.ID, url.PathEscape(a.Login)), nil, nil)
			return
		}
	}
	t.Fatalf("%s has no Owners team", org)
}

func (fx *fixture) CreateRepo(t *testing.T, spec conformance.RepoSpec) platform.Repo {
	t.Helper()
	if spec.Group != "" {
		t.Skip("Gitea and Forgejo have no nested namespaces: organisations are flat")
	}
	p := fx.org + "/" + spec.Name
	if spec.Fork {
		if fx.upstream == "" {
			fx.upstream = fx.org + "-up"
			fx.createOrg(t, fx.upstream)
		}
		fx.createRepo(t, fx.upstream, spec.Name, spec.Files)
		var fork apiRepo
		fx.admin().ok(t, http.MethodPost, "/repos/"+fx.upstream+"/"+spec.Name+"/forks",
			map[string]any{"organization": fx.org, "name": spec.Name}, &fork)
		fx.waitFilled(t, p, len(spec.Files) > 0)
	} else {
		fx.createRepo(t, fx.org, spec.Name, spec.Files)
	}
	if len(spec.Topics) > 0 {
		fx.admin().ok(t, http.MethodPut, "/repos/"+p+"/topics", map[string]any{"topics": spec.Topics}, nil)
	}
	if !spec.ReadOnly {
		fx.admin().ok(t, http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s", fx.writeTeam, p), nil, nil)
	}
	if spec.Archived {
		fx.admin().ok(t, http.MethodPatch, "/repos/"+p, map[string]any{"archived": true}, nil)
	}
	return fx.repo(t, p)
}

// createRepo creates a private repository of owner with files on its
// default branch main: regular files through the contents API, the others
// through git.
func (fx *fixture) createRepo(t testing.TB, owner, name string, files []conformance.File) {
	t.Helper()
	fx.admin().ok(t, http.MethodPost, "/orgs/"+owner+"/repos", map[string]any{
		"name": name, "private": true, "auto_init": false, "default_branch": "main",
	}, nil)
	var regular, special []conformance.File
	for _, f := range files {
		if f.Mode == "" || f.Mode == "100644" {
			regular = append(regular, f)
		} else {
			special = append(special, f)
		}
	}
	p := owner + "/" + name
	if len(regular) > 0 {
		ops := make([]map[string]any, len(regular))
		for i, f := range regular {
			ops[i] = map[string]any{"operation": "create", "path": f.Path, "content": b64(f.Content)}
		}
		fx.admin().ok(t, http.MethodPost, "/repos/"+p+"/contents", map[string]any{
			"message": "conformance: files", "files": ops,
		}, nil)
	}
	if len(special) > 0 {
		fx.env.pushFiles(t, fx.env.Admin, p, "main", special, "conformance: special files")
	}
}

// waitFilled waits until the repository at p has commits, when it should
// (a fork is copied in the background on some versions).
func (fx *fixture) waitFilled(t testing.TB, p string, filled bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var r apiRepo
		fx.admin().get(t, "/repos/"+p, &r)
		if !filled || !r.Empty {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still empty a minute after it was forked", p)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// repo returns the repository at p from the admin's view.
func (fx *fixture) repo(t testing.TB, p string) platform.Repo {
	t.Helper()
	var r apiRepo
	fx.admin().get(t, "/repos/"+p, &r)
	return r.platform(fx.env.Host)
}

func (fx *fixture) CreateBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	fx.commit(t, fx.env.Person, repo.Path, name)
}

// commit adds a new file to branch of the repository at p as as, creating
// the branch from the default branch when it is missing.
func (fx *fixture) commit(t testing.TB, as account, p, branch string) {
	t.Helper()
	fx.n++
	file := fmt.Sprintf("changes/%d-%s.md", fx.n, randHex(t, 3))
	body := map[string]any{
		"message": "conformance: change " + strconv.Itoa(fx.n),
		"files":   []map[string]any{{"operation": "create", "path": file, "content": b64([]byte(branch + "\n"))}},
	}
	if fx.branchExists(t, p, branch) {
		body["branch"] = branch
	} else {
		body["new_branch"] = branch
	}
	fx.env.api(as).ok(t, http.MethodPost, "/repos/"+p+"/contents", body, nil)
}

// branchExists reports whether the repository at p has branch.
func (fx *fixture) branchExists(t testing.TB, p, branch string) bool {
	t.Helper()
	resp := fx.admin().do(t, http.MethodGet, "/repos/"+p+"/branches/"+branch, nil)
	switch resp.Status {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	}
	t.Fatalf("GET branch %s of %s: HTTP %d: %s", branch, p, resp.Status, fx.env.snippet(resp.Body))
	return false
}

func (fx *fixture) Head(t *testing.T, repo platform.Repo) string {
	t.Helper()
	var b apiBranch
	fx.admin().get(t, "/repos/"+repo.Path+"/branches/"+repo.DefaultBranch, &b)
	return b.Commit.ID
}

// DeleteBranch deletes a branch as the admin and waits for what the forge
// does next in the background to its open pull requests: Forgejo closes
// those from and to the branch; Gitea closes those from it and retargets
// those to it onto the default branch (RETARGET_CHILDREN_ON_MERGE, on by
// default; services/pull/pull.go AdjustPullsCausedByBranchDeleted). An open
// pull request whose base is gone therefore never arises on Gitea: the
// fixture reports it and skips.
func (fx *fixture) DeleteBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	if name == repo.DefaultBranch {
		t.Fatalf("DeleteBranch of the default branch %s", name)
	}
	affected := map[int64]apiPR{}
	for _, pr := range fx.pulls(t, repo.Path, "open") {
		if pr.Base.Ref == name || (pr.Head.Label == name && pr.Head.RepoID == pr.Base.RepoID) {
			affected[pr.Number] = pr
		}
	}
	fx.admin().ok(t, http.MethodDelete, "/repos/"+repo.Path+"/branches/"+name, nil, nil)
	deadline := time.Now().Add(30 * time.Second)
	for n, before := range affected {
		for {
			var pr apiPR
			fx.admin().get(t, fmt.Sprintf("/repos/%s/pulls/%d", repo.Path, n), &pr)
			if pr.State != "open" {
				break
			}
			if pr.Base.Ref != before.Base.Ref {
				finding(t, "deleted-base", "%s moves the open pull request #%d onto %s when its base branch %s is deleted: an open pull request with a missing base does not arise",
					fx.env.Flavor, n, pr.Base.Ref, name)
				t.Skipf("the platform retargeted #%d from the deleted %s to %s", n, name, pr.Base.Ref)
			}
			if time.Now().After(deadline) {
				finding(t, "deleted-base", "#%d stayed open on the deleted branch %s for 30 s", n, name)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// pulls lists the pull requests of the repository at p in state, all
// pages.
func (fx *fixture) pulls(t testing.TB, p, state string) []apiPR {
	t.Helper()
	var all []apiPR
	for page := 1; ; page++ {
		var prs []apiPR
		fx.admin().get(t, fmt.Sprintf("/repos/%s/pulls?state=%s&limit=50&page=%d", p, state, page), &prs)
		all = append(all, prs...)
		if len(prs) < 50 {
			return all
		}
	}
}

func (fx *fixture) CreatePR(t *testing.T, repo platform.Repo, spec conformance.PRSpec) platform.PR {
	t.Helper()
	base := spec.Base
	if base == "" {
		base = repo.DefaultBranch
	}
	pr := fx.openPR(t, repo, fx.as(spec.Author), prOptions{
		head: spec.Head, base: base, title: spec.Title, body: spec.Body, labels: spec.Labels, fork: spec.Fork,
	})
	return pr.platform()
}

// prOptions describe a pull request openPR opens.
type prOptions struct {
	head, base, title, body string
	labels                  []string
	// fork opens it from the author's fork of the repository.
	fork bool
}

// openPR adds a commit to the head branch, in the author's fork with
// o.fork, and opens a pull request from it as author.
func (fx *fixture) openPR(t testing.TB, repo platform.Repo, author account, o prOptions) apiPR {
	t.Helper()
	headPath, head := repo.Path, o.head
	if o.fork {
		headPath = fx.fork(t, repo, author)
		head = author.Login + ":" + o.head
	}
	fx.commit(t, author, headPath, o.head)
	body := map[string]any{"head": head, "base": o.base, "title": o.title, "body": o.body}
	if len(o.labels) > 0 {
		body["labels"] = fx.labelIDs(t, repo.Path, o.labels)
	}
	var pr apiPR
	fx.env.api(author).ok(t, http.MethodPost, "/repos/"+repo.Path+"/pulls", body, &pr)
	return pr
}

// fork returns the path of owner's fork of repo, forking it on first use.
// The fork's name carries the organisation's, so that forks of fixtures
// never collide in owner's namespace.
func (fx *fixture) fork(t testing.TB, repo platform.Repo, owner account) string {
	t.Helper()
	key := repo.Path + " " + owner.Login
	if p, ok := fx.forks[key]; ok {
		return p
	}
	var r apiRepo
	fx.env.api(owner).ok(t, http.MethodPost, "/repos/"+repo.Path+"/forks",
		map[string]any{"name": fx.org + "-" + path.Base(repo.Path)}, &r)
	fx.waitFilled(t, r.FullName, !repo.Empty)
	fx.forks[key] = r.FullName
	return r.FullName
}

// labelIDs returns the ids of labels of the repository at p, creating the
// missing ones as the admin: the forges take label ids, not names, when a
// pull request is created.
func (fx *fixture) labelIDs(t testing.TB, p string, names []string) []int64 {
	t.Helper()
	ids := make([]int64, len(names))
	for i, name := range names {
		key := p + " " + name
		if id, ok := fx.labels[key]; ok {
			ids[i] = id
			continue
		}
		var l apiLabel
		fx.admin().ok(t, http.MethodPost, "/repos/"+p+"/labels", map[string]any{"name": name, "color": "#ededed"}, &l)
		fx.labels[key] = l.ID
		ids[i] = l.ID
	}
	return ids
}

func (fx *fixture) SetPRState(t *testing.T, repo platform.Repo, number int64, state platform.PRState, by conformance.Role) {
	t.Helper()
	as := fx.as(by)
	p := fmt.Sprintf("/repos/%s/pulls/%d", repo.Path, number)
	switch state {
	case platform.Open, platform.Closed:
		fx.env.api(as).ok(t, http.MethodPatch, p, map[string]any{"state": string(state)}, nil)
	case platform.Merged:
		fx.env.merge(t, as, repo.Path, number)
	default:
		t.Fatalf("SetPRState: state %q", state)
	}
}

// merge merges pull request n of the repository at p as as with a merge
// commit, once the forge has checked that it can: the check runs in the
// background after every push.
func (e *liveEnv) merge(t testing.TB, as account, p string, n int64) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		resp := e.api(as).do(t, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/merge", p, n),
			map[string]any{"Do": "merge", "delete_branch_after_merge": false})
		if resp.Status/100 == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("merge #%d of %s as %s: HTTP %d: %s", n, p, as.Login, resp.Status, e.snippet(resp.Body))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (fx *fixture) Comments(t *testing.T, repo platform.Repo, number int64) []string {
	t.Helper()
	var comments []apiComment
	fx.admin().get(t, fmt.Sprintf("/repos/%s/issues/%d/comments", repo.Path, number), &comments)
	out := make([]string, len(comments))
	for i, c := range comments {
		out[i] = c.Body
	}
	return out
}

var _ conformance.Fixture = (*fixture)(nil)

// RenameRepo renames repo through the API as the admin; the forges keep
// the old path as a redirect (repo_redirect).
func (fx *fixture) RenameRepo(t *testing.T, repo platform.Repo, name string) platform.Repo {
	t.Helper()
	fx.admin().ok(t, http.MethodPatch, "/repos/"+repo.Path, map[string]any{"name": name}, nil)
	owner, _, _ := strings.Cut(repo.Path, "/")
	return fx.repo(t, owner+"/"+name)
}

// RenamedAccount creates a user as the admin and renames it
// (POST /admin/users/{username}/rename); the forges keep the old login as
// a redirect (user_redirect). The user is deleted when the test ends.
func (fx *fixture) RenamedAccount(t *testing.T) (string, platform.Account) {
	t.Helper()
	old, renamed := "conf-old-"+randHex(t, 4), "conf-new-"+randHex(t, 4)
	fx.admin().ok(t, http.MethodPost, "/admin/users", map[string]any{
		"username": old, "email": old + "@example.com", "password": "Pw-" + randHex(t, 12),
		"must_change_password": false, "visibility": "public",
	}, nil)
	t.Cleanup(func() {
		fx.admin().do(t, http.MethodDelete, "/admin/users/"+renamed+"?purge=true", nil)
	})
	fx.admin().ok(t, http.MethodPost, "/admin/users/"+old+"/rename", map[string]any{"new_username": renamed}, nil)
	var u apiUser
	fx.admin().get(t, "/users/"+renamed, &u)
	return old, platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login}
}
