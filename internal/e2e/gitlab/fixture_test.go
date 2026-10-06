//go:build e2e

package gitlabe2e

import (
	"context"
	"encoding/base64"
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
	"github.com/bedrock-python/touchmark/internal/platform/gitlab"
)

// fixture is conformance.Fixture on the live GitLab. Every subtest gets a
// fresh private subgroup of the seeded group, set up through the API by
// root. The accounts are members of the seeded group, so the subgroup
// inherits their roles: the reader is a Reporter, the writer a Developer,
// the person an Owner.
//
// GitLab cannot lower an inherited role on one project (a member's role in
// a project is never below its role in the group above), so a
// RepoSpec.ReadOnly project lives in a separate top-level group, where the
// reader and the writer are Reporters when they are service accounts; the
// bot of a group access token can be a member of its own group only, and
// does not see that project at all. Target then answers permission or
// not-found, both of which the suite accepts.
//
// Projects are private, as most targets are. Files come through the
// commits API; modes it cannot write (symlinks, submodules) through a git
// push by root. Merge requests are opened by their author's own token: the
// writer's or the person's, from a fork in the person's namespace with
// PRSpec.Fork.
type fixture struct {
	env *liveEnv
	// ns is the fixture's subgroup, "acme/conf-<rand>"; nsID its id.
	ns   string
	nsID int64
	// groups are the ids of the fixture's groups by full path, created on
	// first use: nested subgroups, the upstream group of forks, the group
	// of read-only projects.
	groups map[string]int64
	// upstream holds the sources of RepoSpec.Fork; readOnly the
	// RepoSpec.ReadOnly projects.
	upstream, readOnly string
	reader             platform.Reader
	writer             platform.Writer
	accounts           map[conformance.Role]platform.Account
	// forks are the person's forks made for PRSpec.Fork: "<project id>" →
	// the fork's id.
	forks map[int64]int64
	// n numbers the files the fixture commits.
	n int
}

// newFixture sets up a fixture with the drivers under test; see fixture.
func newFixture(t *testing.T, e *liveEnv) *fixture {
	t.Helper()
	fx := newGroup(t, e, "conf")
	fx.reader, fx.writer = newDrivers(t, e)
	fx.accounts = accountsOf(t, e, fx.reader)
	return fx
}

// newGroup sets up a fresh subgroup of the seeded group named
// prefix-<random>, without drivers: the platform alone.
func newGroup(t *testing.T, e *liveEnv, prefix string) *fixture {
	t.Helper()
	fx := &fixture{env: e, groups: map[string]int64{}, forks: map[int64]int64{}}
	name := prefix + "-" + randHex(t, 4)
	fx.ns = e.Group + "/" + name
	fx.nsID = fx.createGroup(t, name, fx.groupID(t, e.Group))
	fx.groups[fx.ns] = fx.nsID
	return fx
}

// newDrivers builds the gitlab driver's reader and writer, as touchmark
// builds them: the provider from hub.yml, the token from the environment,
// the product's HTTP client.
func newDrivers(t testing.TB, e *liveEnv) (platform.Reader, platform.Writer) {
	t.Helper()
	rp := e.provider(t)
	r := construct(t, "NewReader", func() (platform.Reader, error) { return gitlab.NewReader(rp, credential(e.Reader), e.HTTP) })
	w := construct(t, "NewWriter", func() (platform.Writer, error) { return gitlab.NewWriter(rp, credential(e.Writer), e.HTTP) })
	return r, w
}

// construct calls the driver constructor name and fails the test on its
// error.
func construct[T any](t testing.TB, name string, f func() (T, error)) T {
	t.Helper()
	v, err := f()
	if err != nil {
		t.Fatalf("gitlab.%s: %v", name, err)
	}
	return v
}

// The accounts as the platform reports them, found once per run.
var accountCache struct {
	sync.Mutex
	byRole map[conformance.Role]platform.Account
}

// accountsOf returns the accounts of the roles: id, login and email from
// root's view of each user, the kind from the reader driver's Lookup (the
// suite checks that Self and Lookup agree on it and that it is known).
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
		e.api(e.Root).get(t, fmt.Sprintf("/users/%d", a.ID), &u)
		got, err := reader.Lookup(context.Background(), a.Login)
		if err != nil {
			t.Fatalf("the reader driver's Lookup(%s): %v", a.Login, err)
		}
		out[role] = platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Username, Email: u.Email, Kind: got.Kind}
	}
	accountCache.byRole = out
	return out
}

func (fx *fixture) Reader() platform.Reader { return fx.reader }
func (fx *fixture) Writer() platform.Writer { return fx.writer }
func (fx *fixture) Namespace() string       { return fx.ns }

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

func (fx *fixture) root() glAPI { return fx.env.api(fx.env.Root) }

// groupID returns the id of the group at full path p.
func (fx *fixture) groupID(t testing.TB, p string) int64 {
	t.Helper()
	if id, ok := fx.groups[p]; ok {
		return id
	}
	var g struct {
		ID int64 `json:"id"`
	}
	fx.root().get(t, "/groups/"+pid(p), &g)
	fx.groups[p] = g.ID
	return g.ID
}

// createGroup creates a private group named name under parent (0 for a
// top-level group) and returns its id.
func (fx *fixture) createGroup(t testing.TB, name string, parent int64) int64 {
	t.Helper()
	body := map[string]any{"name": name, "path": name, "visibility": "private"}
	if parent != 0 {
		body["parent_id"] = parent
	}
	var g struct {
		ID       int64  `json:"id"`
		FullPath string `json:"full_path"`
	}
	fx.root().ok(t, http.MethodPost, "/groups", body, &g)
	fx.groups[g.FullPath] = g.ID
	return g.ID
}

// ensureGroup returns the id of the nested group rel ("sub/deeper") under
// the fixture's subgroup, creating its missing levels.
func (fx *fixture) ensureGroup(t testing.TB, rel string) (string, int64) {
	t.Helper()
	full, parent := fx.ns, fx.nsID
	for _, part := range strings.Split(rel, "/") {
		full += "/" + part
		id, ok := fx.groups[full]
		if !ok {
			id = fx.createGroup(t, part, parent)
		}
		parent = id
	}
	return full, parent
}

// sideGroup returns a top-level group of the fixture's own, named after
// its subgroup and suffix, creating it on first use with members.
func (fx *fixture) sideGroup(t testing.TB, suffix string, members map[int64]int) string {
	t.Helper()
	name := path.Base(fx.ns) + "-" + suffix
	if _, ok := fx.groups[name]; ok {
		return name
	}
	id := fx.createGroup(t, name, 0)
	for user, level := range members {
		fx.root().ok(t, http.MethodPost, fmt.Sprintf("/groups/%d/members", id), map[string]any{"user_id": user, "access_level": level}, nil)
	}
	return name
}

// siblingGroup returns a subgroup of the seeded group next to the
// fixture's own, named after it and suffix, creating it on first use: it
// is outside the fixture's namespace, and the accounts see it through the
// seeded group.
func (fx *fixture) siblingGroup(t testing.TB, suffix string) string {
	t.Helper()
	full := fx.env.Group + "/" + path.Base(fx.ns) + "-" + suffix
	if _, ok := fx.groups[full]; ok {
		return full
	}
	fx.createGroup(t, path.Base(full), fx.groupID(t, fx.env.Group))
	return full
}

func (fx *fixture) CreateRepo(t *testing.T, spec conformance.RepoSpec) platform.Repo {
	t.Helper()
	var p apiProject
	switch {
	case spec.Fork:
		// The source is a project the reader sees: GitLab shows
		// forked_from_project only to those who can read the source
		// (TestFacts/fork-visibility), and a fork of an invisible project
		// is no fork to a driver.
		if fx.upstream == "" {
			fx.upstream = fx.siblingGroup(t, "up")
		}
		up := fx.createProject(t, fx.groupID(t, fx.upstream), spec.Name, spec.Files)
		fx.root().ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/fork", up.ID), map[string]any{
			"namespace_path": fx.ns, "name": projectName(spec.Name), "path": projectName(spec.Name),
		}, &p)
		p = fx.waitForked(t, p.ID)
		fx.env.waitAccess(t, p.ID, fx.env.Reader, fx.env.Writer, fx.env.Person)
	case spec.ReadOnly:
		if fx.readOnly == "" {
			members := map[int64]int{fx.env.Person.ID: levelOwner}
			if fx.env.Accounts == accountsService {
				members[fx.env.Reader.ID] = levelReporter
				members[fx.env.Writer.ID] = levelReporter
			}
			fx.readOnly = fx.sideGroup(t, "ro", members)
		}
		p = fx.createProject(t, fx.groupID(t, fx.readOnly), spec.Name, spec.Files)
		who := []account{fx.env.Person}
		if fx.env.Accounts == accountsService {
			who = append(who, fx.env.Reader, fx.env.Writer)
		}
		fx.env.waitAccess(t, p.ID, who...)
	default:
		parent := fx.nsID
		if spec.Group != "" {
			_, parent = fx.ensureGroup(t, spec.Group)
		}
		p = fx.createProject(t, parent, spec.Name, spec.Files)
		fx.env.waitAccess(t, p.ID, fx.env.Reader, fx.env.Writer, fx.env.Person)
	}
	if len(spec.Topics) > 0 {
		fx.root().ok(t, http.MethodPut, fmt.Sprintf("/projects/%d", p.ID), map[string]any{"topics": spec.Topics}, nil)
	}
	if spec.Archived {
		fx.root().ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/archive", p.ID), nil, nil)
	}
	return fx.repo(t, p.ID)
}

// createProject creates a private project in the group with the id
// namespace, with files on its default branch main: regular files through
// the commits API, the others through git.
func (fx *fixture) createProject(t testing.TB, namespace int64, name string, files []conformance.File) apiProject {
	t.Helper()
	name = projectName(name)
	p := fx.env.createProject(t, map[string]any{
		"name": name, "path": name, "namespace_id": namespace, "visibility": "private",
		"initialize_with_readme": false, "default_branch": "main",
	})
	var regular, special []conformance.File
	for _, f := range files {
		if f.Mode == "" || f.Mode == "100644" {
			regular = append(regular, f)
		} else {
			special = append(special, f)
		}
	}
	if len(regular) > 0 {
		fx.env.commitFiles(t, fx.env.Root, p.ID, "main", "", regular, "conformance: files")
	}
	if len(special) > 0 {
		fx.env.pushFiles(t, fx.env.Root, p.PathWithNamespace, "main", special, "conformance: special files")
	}
	return p
}

// waitAccess waits, at most two minutes, until each of who sees the
// project with the id. GitLab gives the members of the groups above a new
// project their access from a Sidekiq job
// (AuthorizedProjectUpdate::ProjectCreateWorker), seconds later on a busy
// instance; until then the project is 404 to them.
func (e *liveEnv) waitAccess(t testing.TB, id int64, who ...account) {
	t.Helper()
	start := time.Now()
	for _, a := range who {
		for {
			resp := e.api(a).do(t, http.MethodGet, fmt.Sprintf("/projects/%d", id), nil)
			if resp.Status == http.StatusOK {
				break
			}
			if resp.Status != http.StatusNotFound || time.Since(start) > 2*time.Minute {
				t.Fatalf("project %d as %s: HTTP %d after %s", id, a.Login, resp.Status, time.Since(start).Round(time.Second))
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		accessLag.Lock()
		first := accessLag.max == 0
		if d > accessLag.max {
			accessLag.max = d
		}
		accessLag.Unlock()
		if first {
			finding(t, "project-access-lag", "a new project of a subgroup reached the members of the group above %s after it was created (a Sidekiq job gives them access)", d.Round(100*time.Millisecond))
		}
	}
}

// accessLag is the longest wait of waitAccess in the run.
var accessLag struct {
	sync.Mutex
	max time.Duration
}

// createProject creates a project as root with the attributes of
// POST /projects. A busy instance may answer 400 "Failed to create
// repository" when Gitaly's CreateRepository outlasts the Rails side's
// deadline (seen on 19.4.1 under load, after 11 s); nothing is saved then,
// and the request is sent again, at most three times.
func (e *liveEnv) createProject(t testing.TB, attrs map[string]any) apiProject {
	t.Helper()
	for attempt := 1; ; attempt++ {
		resp := e.api(e.Root).do(t, http.MethodPost, "/projects", attrs)
		if resp.Status/100 == 2 {
			var p apiProject
			decode(t, "POST /projects", resp, &p)
			return p
		}
		if attempt == 3 || resp.Status != http.StatusBadRequest || !strings.Contains(string(resp.Body), "Failed to create repository") {
			t.Fatalf("POST /projects as root: HTTP %d: %s", resp.Status, e.snippet(resp.Body))
		}
		finding(t, "create-repository-retry", "POST /projects answered 400 %s; attempt %d", e.snippet(resp.Body), attempt+1)
		time.Sleep(5 * time.Second)
	}
}

// reservedPaths are project paths GitLab refuses ("files is a reserved
// name"): the routes below a project (lib/gitlab/path_regex.rb,
// PROJECT_WILDCARD_ROUTES), which the suite's names meet.
var reservedPaths = map[string]bool{
	"badges": true, "blame": true, "blob": true, "builds": true, "commits": true, "create": true,
	"create_dir": true, "edit": true, "files": true, "find_file": true, "new": true, "preview": true,
	"raw": true, "refs": true, "tree": true, "update": true, "wikis": true,
}

// projectName is the name and path of a project the suite names name: the
// name itself, with "-project" after a name GitLab reserves.
func projectName(name string) string {
	if reservedPaths[name] {
		return name + "-project"
	}
	return name
}

// commitFiles commits regular files to branch of project id as as through
// the commits API, creating or updating each, starting the branch from
// start when it is not "". It returns the commit.
func (e *liveEnv) commitFiles(t testing.TB, as account, id int64, branch, start string, files []conformance.File, message string) string {
	t.Helper()
	ref := branch
	if start != "" {
		ref = start
	}
	var actions []map[string]any
	for _, f := range files {
		action := "create"
		if e.fileExists(t, id, ref, f.Path) {
			action = "update"
		}
		actions = append(actions, map[string]any{
			"action": action, "file_path": f.Path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(f.Content),
		})
	}
	body := map[string]any{"branch": branch, "commit_message": message, "actions": actions}
	if start != "" {
		body["start_branch"] = start
	}
	var c apiCommit
	e.api(as).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/repository/commits", id), body, &c)
	return c.ID
}

// waitForked waits until the fork with the id has its repository (GitLab
// copies it in the background) and returns it.
func (fx *fixture) waitForked(t testing.TB, id int64) apiProject {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var p apiProject
		fx.root().get(t, fmt.Sprintf("/projects/%d", id), &p)
		if p.ImportStatus == "finished" || p.ImportStatus == "none" || p.ImportStatus == "" {
			return p
		}
		if p.ImportStatus == "failed" || time.Now().After(deadline) {
			t.Fatalf("the fork %s: import %s", p.PathWithNamespace, p.ImportStatus)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// repo returns the project with the id from root's view.
func (fx *fixture) repo(t testing.TB, id int64) platform.Repo {
	t.Helper()
	var p apiProject
	fx.root().get(t, fmt.Sprintf("/projects/%d", id), &p)
	return p.platform(fx.env.Host)
}

// projectID returns the id of repo.
func projectID(t testing.TB, repo platform.Repo) int64 {
	t.Helper()
	id, err := strconv.ParseInt(repo.ID, 10, 64)
	if err != nil {
		t.Fatalf("project id %q: %v", repo.ID, err)
	}
	return id
}

func (fx *fixture) CreateBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	fx.commit(t, fx.env.Person, projectID(t, repo), repo.DefaultBranch, name)
}

// commit adds a new file to branch of the project id as as, creating the
// branch from base when it is missing.
func (fx *fixture) commit(t testing.TB, as account, id int64, base, branch string) {
	t.Helper()
	fx.n++
	file := conformance.File{
		Path:    fmt.Sprintf("changes/%d-%s.md", fx.n, randHex(t, 3)),
		Content: []byte(branch + "\n"),
	}
	start := ""
	if !fx.env.branchExists(t, id, branch) {
		start = base
	}
	fx.env.commitFiles(t, as, id, branch, start, []conformance.File{file}, "conformance: change "+strconv.Itoa(fx.n))
}

// fileExists reports whether the project id has path at ref; an empty
// project has no file anywhere.
func (e *liveEnv) fileExists(t testing.TB, id int64, ref, p string) bool {
	t.Helper()
	resp := e.api(e.Root).do(t, http.MethodHead, fmt.Sprintf("/projects/%d/repository/files/%s?ref=%s", id, url.PathEscape(p), url.QueryEscape(ref)), nil)
	switch resp.Status {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	}
	t.Fatalf("HEAD %s of %d at %s: HTTP %d", p, id, ref, resp.Status)
	return false
}

// branchExists reports whether the project id has branch.
func (e *liveEnv) branchExists(t testing.TB, id int64, branch string) bool {
	t.Helper()
	_, ok := e.branchHead(t, id, branch)
	return ok
}

// branchHead returns the head of branch in the project id; ok is false when
// the branch does not exist.
func (e *liveEnv) branchHead(t testing.TB, id int64, branch string) (string, bool) {
	t.Helper()
	resp := e.api(e.Root).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/repository/branches/%s", id, url.PathEscape(branch)), nil)
	switch resp.Status {
	case http.StatusOK:
		var b apiBranch
		decode(t, "branch", resp, &b)
		return b.Commit.ID, true
	case http.StatusNotFound:
		return "", false
	}
	t.Fatalf("GET branch %s of %d: HTTP %d: %s", branch, id, resp.Status, e.snippet(resp.Body))
	return "", false
}

func (fx *fixture) Head(t *testing.T, repo platform.Repo) string {
	t.Helper()
	head, ok := fx.env.branchHead(t, projectID(t, repo), repo.DefaultBranch)
	if !ok {
		t.Fatalf("%s has no branch %s", repo.Path, repo.DefaultBranch)
	}
	return head
}

// DeleteBranch deletes a branch as root and waits for what GitLab does next
// in the background: it closes the open merge requests from the branch in
// the same project (MergeRequests::RefreshService, from a Sidekiq job).
// Those to the branch stay open with their target gone.
func (fx *fixture) DeleteBranch(t *testing.T, repo platform.Repo, name string) {
	t.Helper()
	if name == repo.DefaultBranch {
		t.Fatalf("DeleteBranch of the default branch %s", name)
	}
	id := projectID(t, repo)
	var from, to []int64
	for _, mr := range fx.env.mrs(t, id, "opened") {
		switch {
		case mr.SourceBranch == name && mr.SourceProjectID == id:
			from = append(from, mr.IID)
		case mr.TargetBranch == name:
			to = append(to, mr.IID)
		}
	}
	fx.root().ok(t, http.MethodDelete, fmt.Sprintf("/projects/%d/repository/branches/%s", id, url.PathEscape(name)), nil, nil)
	for _, n := range from {
		mr := fx.env.waitMR(t, id, n, time.Minute, func(m apiMR) bool { return m.State != "opened" })
		if mr.State == "opened" {
			t.Fatalf("!%d of %s is still open a minute after its source branch %s was deleted", n, repo.Path, name)
		}
	}
	for _, n := range to {
		var mr apiMR
		fx.root().get(t, fmt.Sprintf("/projects/%d/merge_requests/%d", id, n), &mr)
		finding(t, "deleted-target", "after its target branch %s was deleted, !%d is %s with target %s", name, n, mr.State, mr.TargetBranch)
	}
}

// mrs lists the merge requests of the project id in state ("opened",
// "closed", "merged", "all"), all pages.
func (e *liveEnv) mrs(t testing.TB, id int64, state string) []apiMR {
	t.Helper()
	return all[apiMR](t, e.api(e.Root), fmt.Sprintf("/projects/%d/merge_requests?state=%s", id, state))
}

// mr returns merge request n of the project id, as root sees it.
func (e *liveEnv) mr(t testing.TB, id, n int64) apiMR {
	t.Helper()
	var mr apiMR
	e.api(e.Root).get(t, fmt.Sprintf("/projects/%d/merge_requests/%d", id, n), &mr)
	return mr
}

// waitMR re-reads merge request n of the project id until done holds or
// the time is up, and returns it.
func (e *liveEnv) waitMR(t testing.TB, id, n int64, d time.Duration, done func(apiMR) bool) apiMR {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		mr := e.mr(t, id, n)
		if done(mr) || time.Now().After(deadline) {
			return mr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (fx *fixture) CreatePR(t *testing.T, repo platform.Repo, spec conformance.PRSpec) platform.PR {
	t.Helper()
	base := spec.Base
	if base == "" {
		base = repo.DefaultBranch
	}
	mr := fx.openMR(t, repo, fx.as(spec.Author), mrOptions{
		head: spec.Head, base: base, title: spec.Title, body: spec.Body, labels: spec.Labels, fork: spec.Fork,
	})
	return mr.platform()
}

// mrOptions describe a merge request openMR opens.
type mrOptions struct {
	head, base, title, body string
	labels                  []string
	// fork opens it from the author's fork of the project.
	fork bool
}

// openMR adds a commit to the head branch, in the author's fork with
// o.fork, and opens a merge request from it as author.
func (fx *fixture) openMR(t testing.TB, repo platform.Repo, author account, o mrOptions) apiMR {
	t.Helper()
	target := projectID(t, repo)
	source := target
	if o.fork {
		source = fx.fork(t, repo, author)
	}
	fx.commit(t, author, source, repo.DefaultBranch, o.head)
	body := map[string]any{
		"source_branch": o.head, "target_branch": o.base, "title": o.title, "description": o.body,
		"target_project_id": target,
	}
	if len(o.labels) > 0 {
		body["labels"] = strings.Join(o.labels, ",")
	}
	var mr apiMR
	fx.env.api(author).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", source), body, &mr)
	return mr
}

// fork returns the id of owner's fork of repo in owner's namespace, forking
// it on first use. The fork's path carries the fixture's subgroup, so that
// forks of fixtures never collide in owner's namespace.
func (fx *fixture) fork(t testing.TB, repo platform.Repo, owner account) int64 {
	t.Helper()
	id := projectID(t, repo)
	if f, ok := fx.forks[id]; ok {
		return f
	}
	name := path.Base(fx.ns) + "-" + path.Base(repo.Path)
	var p apiProject
	fx.env.api(owner).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/fork", id), map[string]any{
		"namespace_path": owner.Login, "name": name, "path": name,
	}, &p)
	p = fx.waitForked(t, p.ID)
	fx.forks[id] = p.ID
	return p.ID
}

func (fx *fixture) SetPRState(t *testing.T, repo platform.Repo, number int64, state platform.PRState, by conformance.Role) {
	t.Helper()
	as := fx.as(by)
	id := projectID(t, repo)
	switch state {
	case platform.Open:
		fx.env.setState(t, as, id, number, "reopen")
	case platform.Closed:
		fx.env.setState(t, as, id, number, "close")
	case platform.Merged:
		fx.env.merge(t, as, id, number)
	default:
		t.Fatalf("SetPRState: state %q", state)
	}
}

// setState sends state_event ("close" or "reopen") for merge request n of
// the project id as as.
func (e *liveEnv) setState(t testing.TB, as account, id, n int64, event string) {
	t.Helper()
	var mr apiMR
	e.api(as).ok(t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d", id, n), map[string]any{"state_event": event}, &mr)
	want := map[string]string{"close": "closed", "reopen": "opened"}[event]
	if mr.State != want {
		t.Fatalf("!%d of project %d after %s by %s: %s", n, id, event, as.Login, mr.State)
	}
}

// merge merges merge request n of the project id as as with a merge commit,
// once GitLab has checked that it can: the check runs in the background
// after every push, and the merge answers 405, 406 or 422 until then.
func (e *liveEnv) merge(t testing.TB, as account, id, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp := e.api(as).do(t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d/merge", id, n),
			map[string]any{"should_remove_source_branch": false})
		if resp.Status/100 == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("merge !%d of project %d as %s: HTTP %d: %s", n, id, as.Login, resp.Status, e.snippet(resp.Body))
		}
		// A GET of the merge request makes GitLab check its merge status
		// when it is unchecked (MergeRequest#check_mergeability).
		e.api(as).do(t, http.MethodGet, fmt.Sprintf("/projects/%d/merge_requests/%d", id, n), nil)
		time.Sleep(time.Second)
	}
}

func (fx *fixture) Comments(t *testing.T, repo platform.Repo, number int64) []string {
	t.Helper()
	var out []string
	for _, n := range fx.env.notes(t, projectID(t, repo), number) {
		out = append(out, n.Body)
	}
	return out
}

// notes returns the notes people and bots wrote on merge request n of the
// project id, oldest first: GitLab's own system notes are left out.
func (e *liveEnv) notes(t testing.TB, id, n int64) []apiNote {
	t.Helper()
	var out []apiNote
	for _, note := range all[apiNote](t, e.api(e.Root), fmt.Sprintf("/projects/%d/merge_requests/%d/notes?sort=asc&order_by=created_at", id, n)) {
		if !note.System {
			out = append(out, note)
		}
	}
	return out
}

var (
	_ conformance.Fixture = (*fixture)(nil)
	_ conformance.Renamer = (*fixture)(nil)
)

// RenameRepo renames repo's name and path as root; GitLab keeps the old
// path as a redirect route, which the API follows for projects
// (Project.find_by_full_path with follow_redirects).
func (fx *fixture) RenameRepo(t *testing.T, repo platform.Repo, name string) platform.Repo {
	t.Helper()
	id := projectID(t, repo)
	fx.root().ok(t, http.MethodPut, fmt.Sprintf("/projects/%d", id), map[string]any{"name": name, "path": name}, nil)
	return fx.repo(t, id)
}

// RenamedAccount creates a user as root and renames it. GitLab keeps the
// old username as a redirect route of the user's namespace, but its users
// API finds users by their current username only (GET /users?username=):
// when it does not find the old one either, the lookup of an old login
// cannot work on GitLab, and the test skips with a finding.
func (fx *fixture) RenamedAccount(t *testing.T) (string, platform.Account) {
	t.Helper()
	old, renamed := "conf-old-"+randHex(t, 4), "conf-new-"+randHex(t, 4)
	var u apiUser
	fx.root().ok(t, http.MethodPost, "/users", map[string]any{
		"username": old, "name": old, "email": old + "@example.com", "password": "Pw-" + randHex(t, 12),
		"skip_confirmation": true,
	}, &u)
	t.Cleanup(func() {
		fx.root().do(t, http.MethodDelete, fmt.Sprintf("/users/%d?hard_delete=true", u.ID), nil)
	})
	fx.root().ok(t, http.MethodPut, fmt.Sprintf("/users/%d", u.ID), map[string]any{"username": renamed}, &u)
	var found []apiUser
	fx.root().get(t, "/users?username="+url.QueryEscape(old), &found)
	if len(found) == 0 {
		finding(t, "renamed-user", "GET /users?username=<old login> finds nobody after a rename (the users API knows current usernames only)")
		t.Skip("GitLab finds a user by the current username only: an old login cannot lead to the account")
	}
	return old, platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Username}
}
