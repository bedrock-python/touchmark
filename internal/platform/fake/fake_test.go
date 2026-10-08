package fake_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

func TestCapsFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flavor       fake.Flavor
		maxBody      int
		draft        platform.DraftStyle
		prefix       string
		quick        bool
		byID         bool
		workflow     bool
		closerKnown  bool
		runtimeRules []string
	}{
		{fake.GitHub, 58000, platform.DraftNative, "", false, false, true, true, nil},
		{fake.GitLab, 200000, platform.DraftTitlePrefix, "Draft: ", true, false, false, true, []string{"push_rules"}},
		{fake.Gitea, 58000, platform.DraftTitlePrefix, "WIP: ", false, true, false, false, nil},
		{fake.Forgejo, 58000, platform.DraftTitlePrefix, "WIP: ", false, true, false, false, nil},
	} {
		c := fake.CapsFor(tc.flavor)
		if c.Flavor != string(tc.flavor) || c.MaxBody != tc.maxBody || c.Draft != tc.draft || c.DraftPrefix != tc.prefix ||
			c.QuickActions != tc.quick || c.LabelsByID != tc.byID || c.WorkflowPerm != tc.workflow ||
			c.CloserKnown != tc.closerKnown || c.Marker != platform.MarkerInBody || c.Commit.API {
			t.Errorf("CapsFor(%s) = %+v", tc.flavor, c)
		}
		sameList(t, string(tc.flavor)+" RuntimeOnly", c.RuntimeOnly, tc.runtimeRules)
		if c.Limits.Reads <= 0 || c.Limits.GitReads <= 0 || c.Limits.MinInterval <= 0 {
			t.Errorf("CapsFor(%s): limits %+v", tc.flavor, c.Limits)
		}
		got, err := fake.New("h.example", fake.WithFlavor(tc.flavor)).Reader(platform.Account{}).Probe(t.Context())
		if err == nil || got.Flavor != "" {
			t.Errorf("Probe with an unknown account = %+v, %v; want an auth error", got, err)
		}
	}
	if c := fake.CapsFor("azure-devops"); c.Flavor != "azure-devops" || !c.WorkflowPerm || c.Draft != platform.DraftNative {
		t.Errorf("CapsFor(unknown) = %+v, want GitHub's under its own name", c)
	}
}

func TestProbeAndSetCaps(t *testing.T) {
	t.Parallel()
	e := newEnv(t, fake.WithFlavor(fake.GitLab))
	caps, err := e.p.Reader(e.reader).Probe(t.Context())
	if err != nil || caps.Flavor != "gitlab" {
		t.Fatalf("Probe = %+v, %v", caps, err)
	}
	caps.RuntimeOnly[0] = "mutated"
	if got := e.p.Caps().RuntimeOnly[0]; got != "push_rules" {
		t.Errorf("Probe result aliases the platform's caps: %q", got)
	}
	c := fake.CapsFor(fake.Gitea)
	c.CloserKnown = true
	e.p.SetCaps(c)
	if got, _ := e.p.Reader(e.reader).Probe(t.Context()); got.Flavor != "gitea" || !got.CloserKnown {
		t.Errorf("after SetCaps: %+v", got)
	}
	if e.p.Host() != "github.com" {
		t.Errorf("Host() = %q", e.p.Host())
	}
}

func TestAccounts(t *testing.T) {
	t.Parallel()
	p := fake.New("GitHub.com")
	bot := p.AddAccount("acme[bot]", platform.KindBot)
	user := p.AddAccount("jdoe", platform.KindUser)
	if bot.ID == "" || bot.ID == user.ID || bot.Kind != platform.KindBot || user.Kind != platform.KindUser {
		t.Fatalf("accounts %+v %+v", bot, user)
	}
	if bot.Email != bot.ID+"+acme[bot]@users.noreply.github.com" {
		t.Errorf("email %q", bot.Email)
	}
	if again := p.AddAccount("ACME[bot]", platform.KindUser); again != bot {
		t.Errorf("AddAccount of a taken login = %+v, want %+v", again, bot)
	}
	// Ids depend only on the order of calls.
	q := fake.New("github.com")
	if b, u := q.AddAccount("acme[bot]", platform.KindBot), q.AddAccount("jdoe", platform.KindUser); b.ID != bot.ID || u.ID != user.ID {
		t.Errorf("ids are not deterministic: %s %s vs %s %s", b.ID, u.ID, bot.ID, user.ID)
	}

	r := p.Reader(bot)
	got, err := r.Lookup(t.Context(), "JDOE")
	if err != nil || got != user {
		t.Errorf("Lookup(JDOE) = %+v, %v", got, err)
	}
	_, err = r.Lookup(t.Context(), "")
	wantIs(t, "Lookup of an empty login", err, platform.ErrNotFound)

	// A rename keeps the id; PRs report the new login.
	repo := p.AddRepo(platform.Repo{Path: "acme/api"})
	n := p.AddPR(repo.ID, platform.PR{Head: "touchmark/hub", Author: user})
	p.RenameAccount(user.ID, "jdoe-renamed")
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup(t.Context(), "jdoe"); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("Lookup of the old login: %v", err)
	}
	prs, err := r.PRs(t.Context(), repo, []string{"touchmark/hub"}, nil)
	if err != nil || len(prs) != 1 || prs[0].Author.ID != user.ID || prs[0].Author.Login != "jdoe-renamed" {
		t.Errorf("PRs after the rename = %+v, %v", prs, err)
	}
	if got := p.PR(repo.ID, n).Author.Login; got != "jdoe-renamed" {
		t.Errorf("PR author after the rename: %q", got)
	}
	// The old login is free again and names a new account.
	if again := p.AddAccount("jdoe", platform.KindUser); again.ID == user.ID {
		t.Error("a reused login got the renamed account's id")
	}

	p.RenameAccount(user.ID, "acme[bot]")
	wantSetupErr(t, p, "taken")
}

func TestAccountSetupErrors(t *testing.T) {
	t.Parallel()
	for name, fn := range map[string]func(p *fake.Platform){
		"empty login":    func(p *fake.Platform) { p.AddAccount("", platform.KindUser) },
		"rename unknown": func(p *fake.Platform) { p.RenameAccount("999", "x") },
		"rename empty": func(p *fake.Platform) {
			a := p.AddAccount("x", platform.KindUser)
			p.RenameAccount(a.ID, "")
		},
		"empty host": func(*fake.Platform) {},
	} {
		host := "github.com"
		if name == "empty host" {
			host = ""
		}
		p := fake.New(host)
		fn(p)
		if p.Err() == nil {
			t.Errorf("%s: no setup error", name)
		}
	}
}

func TestAddRepo(t *testing.T) {
	t.Parallel()
	p := fake.New("GitLab.Example.com", fake.WithFlavor(fake.GitLab))
	r := p.AddRepo(platform.Repo{Path: "group/sub/api", Topics: []string{"python"}, Host: "ignored"})
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	want := platform.Repo{Host: "gitlab.example.com", ID: r.ID, Path: "group/sub/api", DefaultBranch: "main",
		WebURL: "https://gitlab.example.com/group/sub/api", Visibility: "public", ObjectFormat: "sha1", Empty: true,
		Topics: []string{"python"}}
	if fmt.Sprint(r) != fmt.Sprint(want) {
		t.Errorf("AddRepo = %+v\nwant %+v", r, want)
	}
	given := p.AddRepo(platform.Repo{Path: "group/other", ID: "77", DefaultBranch: "master", Visibility: "private"})
	if given.ID != "77" || given.DefaultBranch != "master" || given.Visibility != "private" {
		t.Errorf("AddRepo with values = %+v", given)
	}
	p.SetFile(r.ID, "README.md", []byte("x"), "")
	if got, ok := p.RepoByID(r.ID); !ok || got.Empty {
		t.Errorf("a repository with a file: %+v, %v", got, ok)
	}
	if _, ok := p.RepoByID("nope"); ok {
		t.Error("RepoByID of an unknown id")
	}

	for name, fn := range map[string]func(p *fake.Platform){
		"taken path":   func(p *fake.Platform) { p.AddRepo(platform.Repo{Path: "GROUP/SUB/API"}) },
		"taken id":     func(p *fake.Platform) { p.AddRepo(platform.Repo{Path: "group/new", ID: r.ID}) },
		"no namespace": func(p *fake.Platform) { p.AddRepo(platform.Repo{Path: "api"}) },
		"bad segment":  func(p *fake.Platform) { p.AddRepo(platform.Repo{Path: "group//api"}) },
		"dot segment":  func(p *fake.Platform) { p.AddRepo(platform.Repo{Path: "group/../api"}) },
		"trailing":     func(p *fake.Platform) { p.AddRepo(platform.Repo{Path: "group/api/"}) },
	} {
		q := fake.New("gitlab.example.com")
		q.AddRepo(platform.Repo{Path: "group/sub/api", ID: r.ID})
		fn(q)
		if q.Err() == nil {
			t.Errorf("%s: no setup error", name)
		}
	}
}

func TestUpdateRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	other := e.repo("acme/web", "README.md", "y")
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) {
		repo.Path = "acme/API-renamed"
		repo.Archived = true
		repo.Topics = append(repo.Topics, "python")
		repo.ID, repo.Host = "hijack", "evil.example"
	})
	e.ok()
	got, err := e.p.Reader(e.reader).Repo(t.Context(), "acme/api-renamed")
	if err != nil || got.ID != r.ID || got.Path != "acme/API-renamed" || !got.Archived || got.Host != "github.com" {
		t.Errorf("after UpdateRepo: %+v, %v", got, err)
	}
	if _, err := e.p.Reader(e.reader).Repo(t.Context(), "acme/api"); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("the old path still resolves: %v", err)
	}
	// A path of the same repository in another case is fine.
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.Path = "acme/api-RENAMED" })
	e.ok()
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.Path = strings.ToUpper(other.Path) })
	wantSetupErr(t, e.p, "taken")
}

func TestFilesAndHead(t *testing.T) {
	t.Parallel()
	p := fake.New("github.com")
	r := p.AddRepo(platform.Repo{Path: "acme/api"})
	if p.Head(r.ID) != "" {
		t.Error("an empty repository has a head")
	}
	p.SetFile(r.ID, "README.md", []byte("one\n"), "")
	p.SetFile(r.ID, "bin/run", []byte("#!/bin/sh\n"), fake.ModeExecutable)
	p.SetSymlink(r.ID, "link", "README.md")
	p.SetGitlink(r.ID, "vendor/lib", strings.Repeat("AB", 20))
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	h1 := p.Head(r.ID)
	if len(h1) != 40 {
		t.Fatalf("head %q", h1)
	}
	tree, err := p.Snapshots().Snapshot(t.Context(), r, platform.Remote{}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"README.md":  {"100644", gitx.RawOID([]byte("one\n"))},
		"bin/run":    {"100755", gitx.RawOID([]byte("#!/bin/sh\n"))},
		"link":       {"120000", gitx.RawOID([]byte("README.md"))},
		"vendor/lib": {"160000", strings.Repeat("ab", 20)},
	}
	if tree.Commit != h1 || len(tree.Entries) != len(want) {
		t.Fatalf("tree %s with %d entries, want %s with %d", tree.Commit, len(tree.Entries), h1, len(want))
	}
	for path, w := range want {
		if e := tree.Entries[path]; e.Mode != w[0] || e.OID != w[1] {
			t.Errorf("%s: %+v, want %v", path, e, w)
		}
	}

	p.SetFile(r.ID, "README.md", []byte("two\n"), "")
	h2 := p.Head(r.ID)
	p.SetFile(r.ID, "README.md", []byte("one\n"), fake.ModeFile)
	if h2 == h1 || p.Head(r.ID) != h1 {
		t.Errorf("heads %s → %s → %s: want a change and the same id for the same state", h1, h2, p.Head(r.ID))
	}
	p.RemoveFile(r.ID, "link")
	if p.Head(r.ID) == h1 {
		t.Error("RemoveFile did not change the head")
	}
	// Same tree, other repository: another commit.
	o := p.AddRepo(platform.Repo{Path: "acme/web"})
	p.SetFile(o.ID, "README.md", []byte("one\n"), "")
	q := p.AddRepo(platform.Repo{Path: "acme/cli"})
	p.SetFile(q.ID, "README.md", []byte("one\n"), "")
	if p.Head(o.ID) == p.Head(q.ID) {
		t.Error("two repositories share a head")
	}
	// Deterministic across platforms.
	p2 := fake.New("github.com")
	r2 := p2.AddRepo(platform.Repo{Path: "acme/api"})
	p2.SetFile(r2.ID, "README.md", []byte("one\n"), "")
	p3 := fake.New("github.com")
	r3 := p3.AddRepo(platform.Repo{Path: "acme/api"})
	p3.SetFile(r3.ID, "README.md", []byte("one\n"), "")
	if p2.Head(r2.ID) != p3.Head(r3.ID) {
		t.Error("the same steps gave different heads")
	}
	if p.Head("unknown") != "" {
		t.Error("Head of an unknown repository")
	}
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestFileSetupErrors(t *testing.T) {
	t.Parallel()
	for name, fn := range map[string]func(p *fake.Platform, id string){
		"unknown repo":       func(p *fake.Platform, _ string) { p.SetFile("nope", "a", nil, "") },
		"bad mode":           func(p *fake.Platform, id string) { p.SetFile(id, "a", nil, "120000") },
		"empty path":         func(p *fake.Platform, id string) { p.SetFile(id, "", nil, "") },
		"leading slash":      func(p *fake.Platform, id string) { p.SetFile(id, "/a", nil, "") },
		"dotdot":             func(p *fake.Platform, id string) { p.SetFile(id, "a/../b", nil, "") },
		"NUL":                func(p *fake.Platform, id string) { p.SetFile(id, "a\x00b", nil, "") },
		"under a file":       func(p *fake.Platform, id string) { p.SetFile(id, "README.md/x", nil, "") },
		"under a symlink":    func(p *fake.Platform, id string) { p.SetFile(id, "link/x", nil, "") },
		"over a directory":   func(p *fake.Platform, id string) { p.SetFile(id, "docs", nil, "") },
		"symlink no target":  func(p *fake.Platform, id string) { p.SetSymlink(id, "l2", "") },
		"gitlink not an id":  func(p *fake.Platform, id string) { p.SetGitlink(id, "sub", "main") },
		"remove missing":     func(p *fake.Platform, id string) { p.RemoveFile(id, "missing") },
		"remove a directory": func(p *fake.Platform, id string) { p.RemoveFile(id, "docs") },
	} {
		p := fake.New("github.com")
		r := p.AddRepo(platform.Repo{Path: "acme/api"})
		p.SetFile(r.ID, "README.md", []byte("x"), "")
		p.SetFile(r.ID, "docs/a.md", []byte("x"), "")
		p.SetSymlink(r.ID, "link", "docs")
		if err := p.Err(); err != nil {
			t.Fatal(err)
		}
		fn(p, r.ID)
		if p.Err() == nil {
			t.Errorf("%s: no setup error", name)
		}
	}
	// Replacing an entry of another kind at the same path is fine.
	p := fake.New("github.com")
	r := p.AddRepo(platform.Repo{Path: "acme/api"})
	p.SetSymlink(r.ID, "a", "b")
	p.SetFile(r.ID, "a", []byte("now a file"), "")
	p.SetFile(r.ID, "weird name:?*.md", []byte("git allows it"), "")
	if err := p.Err(); err != nil {
		t.Errorf("replacing an entry: %v", err)
	}
}

// TestSetupErrorIsSticky: after a misuse every call fails, so a broken
// fixture cannot pass.
func TestSetupErrorIsSticky(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	e.p.SetFile("no-such-id", "a", nil, "")
	setup := e.p.Err()
	if setup == nil || !strings.Contains(setup.Error(), "no-such-id") {
		t.Fatalf("Err() = %v", setup)
	}
	ctx := t.Context()
	rd := e.p.Reader(e.reader)
	_, err1 := rd.Probe(ctx)
	_, err2 := rd.ReadFile(ctx, r, "", "README.md", 10)
	_, err3 := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, "")
	_, err4 := tw.CreatePR(ctx, platform.NewPR{Head: "h", Base: "main", Title: "t"})
	for i, err := range []error{err1, err2, err3, err4} {
		if !errors.Is(err, setup) {
			t.Errorf("call %d: %v, want the setup error", i+1, err)
		}
	}
	// The first error stays.
	e.p.AddRepo(platform.Repo{Path: "x"})
	if got := e.p.Err(); got.Error() != setup.Error() {
		t.Errorf("Err() changed to %v", got)
	}
}

func TestAddPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flavor fake.Flavor
		url    string
	}{
		{fake.GitHub, "https://h.example/acme/api/pull/"},
		{fake.GitLab, "https://h.example/acme/api/-/merge_requests/"},
		{fake.Gitea, "https://h.example/acme/api/pulls/"},
		{fake.Forgejo, "https://h.example/acme/api/pulls/"},
	} {
		p := fake.New("h.example", fake.WithFlavor(tc.flavor))
		bot := p.AddAccount("bot", platform.KindBot)
		r := p.AddRepo(platform.Repo{Path: "acme/api", DefaultBranch: "master"})
		n1 := p.AddPR(r.ID, platform.PR{Head: "touchmark/hub", Author: bot, Labels: []string{"a", "a", "b"}, ClosedBy: &bot})
		n7 := p.AddPR(r.ID, platform.PR{Number: 7, Head: "x", Author: bot, State: platform.Closed, Base: "develop"})
		n8 := p.AddPR(r.ID, platform.PR{Head: "y", Author: bot, HeadRepoID: "999", BaseExists: false})
		if err := p.Err(); err != nil {
			t.Fatal(err)
		}
		if n1 != 1 || n7 != 7 || n8 != 8 {
			t.Errorf("%s: numbers %d %d %d, want 1 7 8", tc.flavor, n1, n7, n8)
		}
		pr := p.PR(r.ID, n1)
		if pr.State != platform.Open || pr.Base != "master" || pr.RepoID != r.ID || pr.HeadRepoID != r.ID ||
			!pr.BaseExists || pr.URL != tc.url+"1" || pr.CreatedAt.IsZero() || pr.ClosedBy != nil || !pr.ClosedAt.IsZero() {
			t.Errorf("%s: defaults %+v", tc.flavor, pr)
		}
		sameList(t, "labels", pr.Labels, []string{"a", "b"})
		sameList(t, "repository labels", p.Labels(r.ID), []string{"a", "b"})
		if c := p.PR(r.ID, n7); c.ClosedAt.IsZero() || c.Base != "develop" || !c.BaseExists {
			t.Errorf("%s: closed PR %+v", tc.flavor, c)
		}
		if f := p.PR(r.ID, n8); f.HeadRepoID != "999" || !f.BaseExists {
			t.Errorf("%s: fork PR %+v", tc.flavor, f)
		}
		sameList(t, "PRList", numbers(p.PRList(r.ID)), []int64{1, 7, 8})
		if p.PR(r.ID, 99).Number != 0 || p.PRList("nope") != nil {
			t.Error("PR or PRList of something missing")
		}
	}

	for name, fn := range map[string]func(p *fake.Platform, id string, a platform.Account){
		"unknown repo": func(p *fake.Platform, _ string, a platform.Account) {
			p.AddPR("nope", platform.PR{Head: "h", Author: a})
		},
		"no head":   func(p *fake.Platform, id string, a platform.Account) { p.AddPR(id, platform.PR{Author: a}) },
		"no author": func(p *fake.Platform, id string, _ platform.Account) { p.AddPR(id, platform.PR{Head: "h"}) },
		"taken number": func(p *fake.Platform, id string, a platform.Account) {
			p.AddPR(id, platform.PR{Head: "h", Author: a})
			p.AddPR(id, platform.PR{Number: 1, Head: "h", Author: a})
		},
		"negative": func(p *fake.Platform, id string, a platform.Account) {
			p.AddPR(id, platform.PR{Number: -1, Head: "h", Author: a})
		},
		"bad state": func(p *fake.Platform, id string, a platform.Account) {
			p.AddPR(id, platform.PR{Head: "h", Author: a, State: "draft"})
		},
		"other repo id": func(p *fake.Platform, id string, a platform.Account) {
			p.AddPR(id, platform.PR{Head: "h", Author: a, RepoID: "other"})
		},
		"blank label": func(p *fake.Platform, id string, a platform.Account) {
			p.AddPR(id, platform.PR{Head: "h", Author: a, Labels: []string{" "}})
		},
	} {
		p := fake.New("github.com")
		a := p.AddAccount("bot", platform.KindBot)
		r := p.AddRepo(platform.Repo{Path: "acme/api"})
		fn(p, r.ID, a)
		if p.Err() == nil {
			t.Errorf("%s: no setup error", name)
		}
	}
}

func TestSetPRStateAndUpdatePR(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	n := e.pr(r, platform.PR{Head: "touchmark/hub", Author: e.writer, Title: "sync"})
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	e.p.SetPRState(r.ID, n, platform.Closed, &e.other, at)
	e.ok()
	if pr := e.p.PR(r.ID, n); pr.State != platform.Closed || !pr.ClosedAt.Equal(at) || pr.ClosedBy == nil || pr.ClosedBy.ID != e.other.ID {
		t.Errorf("closed: %+v", pr)
	}
	e.p.SetPRState(r.ID, n, platform.Open, &e.other, at)
	if pr := e.p.PR(r.ID, n); pr.State != platform.Open || !pr.ClosedAt.IsZero() || pr.ClosedBy != nil {
		t.Errorf("reopened: %+v", pr)
	}
	e.p.SetPRState(r.ID, n, platform.Merged, nil, time.Time{})
	if pr := e.p.PR(r.ID, n); pr.State != platform.Merged || pr.ClosedAt.IsZero() || pr.ClosedBy != nil {
		t.Errorf("merged, closer not reported: %+v", pr)
	}
	e.ok()

	e.p.UpdatePR(r.ID, n, func(pr *platform.PR) {
		pr.Title = "renamed by a person"
		pr.Labels = append(pr.Labels, "keep", "keep")
		pr.Number, pr.RepoID = 42, "other"
		pr.BaseExists = false
		pr.State = platform.Open
	})
	e.ok()
	pr := e.p.PR(r.ID, n)
	if pr.Number != n || pr.RepoID != r.ID || pr.Title != "renamed by a person" || pr.BaseExists || pr.State != platform.Open || !pr.ClosedAt.IsZero() {
		t.Errorf("after UpdatePR: %+v", pr)
	}
	sameList(t, "labels", pr.Labels, []string{"keep"})
	if pr.HeadRepoID != r.ID {
		t.Errorf("UpdatePR moved the head repository to %q", pr.HeadRepoID)
	}
	// A deleted fork leaves no head repository.
	e.p.UpdatePR(r.ID, n, func(pr *platform.PR) { pr.HeadRepoID = "" })
	e.ok()
	if got := e.p.PR(r.ID, n).HeadRepoID; got != "" {
		t.Errorf("HeadRepoID of a PR from a deleted fork = %q", got)
	}

	e.p.DeleteBranch(r.ID, "main")
	wantSetupErr(t, e.p, "not a deletable branch")

	for name, fn := range map[string]func(p *fake.Platform){
		"state of a missing PR": func(p *fake.Platform) { p.SetPRState(r.ID, 99, platform.Closed, nil, time.Time{}) },
		"unknown state":         func(p *fake.Platform) { p.SetPRState(r.ID, n, "gone", nil, time.Time{}) },
		"update a missing PR":   func(p *fake.Platform) { p.UpdatePR(r.ID, 99, func(*platform.PR) {}) },
		"update drops author":   func(p *fake.Platform) { p.UpdatePR(r.ID, n, func(pr *platform.PR) { pr.Author = platform.Account{} }) },
		"delete in a missing":   func(p *fake.Platform) { p.DeleteBranch("nope", "x") },
	} {
		q := newEnv(t)
		q.p.AddRepo(platform.Repo{Path: "acme/api", ID: r.ID})
		q.p.AddPR(r.ID, platform.PR{Head: "h", Author: q.writer})
		q.ok()
		fn(q.p)
		if q.p.Err() == nil {
			t.Errorf("%s: no setup error", name)
		}
	}
}

func TestDeleteBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	onRelease := e.pr(r, platform.PR{Head: "touchmark/hub", Base: "release", Author: e.writer})
	onMain := e.pr(r, platform.PR{Head: "feature", Author: e.other})
	e.p.DeleteBranch(r.ID, "release")
	e.ok()
	if e.p.PR(r.ID, onRelease).BaseExists || !e.p.PR(r.ID, onMain).BaseExists {
		t.Error("DeleteBranch changed the wrong PRs")
	}
	// A new base through EditPR exists again.
	tw := e.target(r)
	pr, err := tw.EditPR(t.Context(), onRelease, platform.PREdit{Base: ptr("main")})
	if err != nil || !pr.BaseExists || pr.Base != "main" {
		t.Errorf("EditPR base = %+v, %v", pr, err)
	}
}

func TestClock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.repo("acme/api")
	n1 := e.pr(r, platform.PR{Head: "a", Author: e.writer})
	n2 := e.pr(r, platform.PR{Head: "b", Author: e.writer})
	c1, c2 := e.p.PR(r.ID, n1).CreatedAt, e.p.PR(r.ID, n2).CreatedAt
	if !c1.After(fake.Epoch) || !c2.After(c1) {
		t.Errorf("default clock: %v then %v", c1, c2)
	}

	fixed := time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC)
	q := newEnv(t, fake.WithClock(func() time.Time { return fixed }))
	qr := q.repo("acme/api")
	tw := q.target(qr)
	pr, err := tw.CreatePR(t.Context(), platform.NewPR{Head: "h", Base: "main", Title: "t"})
	if err != nil || !pr.CreatedAt.Equal(fixed) {
		t.Errorf("CreatePR with a clock = %v, %v", pr.CreatedAt, err)
	}
	must(t, tw.Comment(t.Context(), pr.Number, "hello"))
	if c := q.p.Comments(qr.ID, pr.Number); len(c) != 1 || !c[0].CreatedAt.Equal(fixed) || c[0].Author.ID != q.writer.ID {
		t.Errorf("comment = %+v", c)
	}
}

// TestConcurrentUse drives one platform from many goroutines; run with
// -race.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t, fake.WithFlavor(fake.GitLab))
	ctx := t.Context()
	var repos []platform.Repo
	for i := range 8 {
		repos = append(repos, e.repo(fmt.Sprintf("acme/r%d", i), "README.md", "x"))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i, r := range repos {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tw, err := e.p.Writer(e.writer).Target(ctx, r, platform.Perms{Contents: true, PRs: true})
			if err != nil {
				errs <- err
				return
			}
			defer tw.Close()
			pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Labels: []string{"l"}})
			if err != nil {
				errs <- err
				return
			}
			if _, err := tw.EditPR(ctx, pr.Number, platform.PREdit{Body: ptr("edited")}); err != nil {
				errs <- err
			}
			if err := tw.Comment(ctx, pr.Number, "hi"); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			rd := e.p.Reader(e.reader)
			for range 20 {
				if _, err := rd.ReadFile(ctx, r, "", "README.md", 100); err != nil {
					errs <- err
				}
				if _, err := rd.PRs(ctx, r, []string{"touchmark/hub"}, []platform.Account{e.writer}); err != nil {
					errs <- err
				}
				if _, err := rd.OpenPRsBy(ctx, []platform.Account{e.writer}, []string{"touchmark/hub"}); err != nil {
					errs <- err
				}
				if _, err := e.p.Snapshots().Snapshot(ctx, r, platform.Remote{}, ""); err != nil {
					errs <- err
				}
			}
			e.p.SetFile(r.ID, fmt.Sprintf("f%d.md", i), []byte("y"), "")
			_ = e.p.Calls()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	e.ok()
	sw, err := e.p.Reader(e.reader).OpenPRsBy(ctx, []platform.Account{e.writer}, []string{"touchmark/hub"})
	if err != nil || len(sw.PRs) != len(repos) {
		t.Errorf("OpenPRsBy after the run: %d PRs, %v", len(sw.PRs), err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
