package conformance

import (
	"bytes"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

func testProbe(t *testing.T, fx Fixture) {
	caps := probe(t, fx)
	if caps.Flavor == "" {
		t.Error("Probe: empty Flavor")
	}
	if caps.MaxBody <= 0 {
		t.Errorf("Probe: MaxBody %d", caps.MaxBody)
	}
	if caps.Draft == platform.DraftTitlePrefix && strings.TrimSpace(caps.DraftPrefix) == "" {
		t.Error("Probe: title-prefix drafts without a DraftPrefix")
	}
	wcaps, err := fx.Writer().Probe(t.Context())
	must(t, "writer Probe", err)
	if wcaps.Flavor != caps.Flavor {
		t.Errorf("writer Probe: flavor %q, reader %q", wcaps.Flavor, caps.Flavor)
	}
}

func testSelf(t *testing.T, fx Fixture) {
	ctx := t.Context()
	for _, tc := range []struct {
		want platform.Account
		r    platform.Reader
		name string
	}{
		{fx.Account(RoleReader), fx.Reader(), "reader"},
		{fx.Account(RoleWriter), fx.Writer(), "writer"},
	} {
		got, err := tc.r.Self(ctx)
		must(t, tc.name+" Self", err)
		if got.ID == "" || got.Login == "" {
			t.Errorf("%s Self = %+v: empty id or login", tc.name, got)
		}
		if got.ID != tc.want.ID {
			t.Errorf("%s Self id %s, want %s", tc.name, got.ID, tc.want.ID)
		}
		if got.Kind != tc.want.Kind || got.Kind == platform.KindUnknown {
			t.Errorf("%s Self kind %v, want %v", tc.name, got.Kind, tc.want.Kind)
		}
	}
	if fx.Account(RoleReader).ID == fx.Account(RoleWriter).ID {
		t.Error("the reader and the writer are one account")
	}
}

func testLookup(t *testing.T, fx Fixture) {
	ctx := t.Context()
	for _, role := range []Role{RoleWriter, RoleOther} {
		want := fx.Account(role)
		got, err := fx.Reader().Lookup(ctx, want.Login)
		must(t, "Lookup "+role.String(), err)
		if got.ID != want.ID {
			t.Errorf("Lookup(%q) id %s, want %s", want.Login, got.ID, want.ID)
		}
		// Memory tells bots from people by Kind: a pull request a bot
		// closed is no decline.
		if got.Kind != want.Kind || got.Kind == platform.KindUnknown {
			t.Errorf("Lookup(%q) kind %v, want %v", want.Login, got.Kind, want.Kind)
		}
	}
	_, err := fx.Reader().Lookup(ctx, "touchmark-conformance-missing-7f3a9c")
	wantErr(t, "Lookup of a missing login", err, platform.ErrNotFound, platform.ClassNotFound)
}

func testRepo(t *testing.T, fx Fixture) {
	ctx := t.Context()
	created := fx.CreateRepo(t, RepoSpec{Name: "alpha", Files: readme, Topics: []string{"conformance"}})
	got, err := fx.Reader().Repo(ctx, created.Path)
	must(t, "Repo", err)
	switch {
	case got.Host == "" || got.ID == "" || got.DefaultBranch == "":
		t.Errorf("Repo = %+v: empty host, id or default branch", got)
	case got.ID != created.ID || got.Path != created.Path || !strings.EqualFold(got.Host, created.Host):
		t.Errorf("Repo = %s %s/%s, created %s %s/%s", got.ID, got.Host, got.Path, created.ID, created.Host, created.Path)
	}
	if !slices.Contains([]string{"public", "internal", "private"}, got.Visibility) {
		t.Errorf("Repo: visibility %q", got.Visibility)
	}
	if got.ObjectFormat != "sha1" {
		t.Errorf("Repo: object format %q, want sha1", got.ObjectFormat)
	}
	if got.Archived || got.Fork || got.Empty || got.Disabled || got.Mirror || got.PendingDelete || got.PRsDisabled {
		t.Errorf("Repo: flags set on a plain repository: %+v", got)
	}
	if !slices.ContainsFunc(got.Topics, func(s string) bool { return strings.EqualFold(s, "conformance") }) {
		t.Errorf("Repo: topics %v, want conformance", got.Topics)
	}

	folded, err := fx.Reader().Repo(ctx, strings.ToUpper(created.Path))
	must(t, "Repo in upper case", err)
	if folded.ID != created.ID || folded.Path != created.Path {
		t.Errorf("Repo(%q) = %s %s, want %s %s (canonical path)", strings.ToUpper(created.Path),
			folded.ID, folded.Path, created.ID, created.Path)
	}

	_, err = fx.Reader().Repo(ctx, fx.Namespace()+"/no-such-repository")
	wantErr(t, "Repo of a missing repository", err, platform.ErrNotFound, platform.ClassNotFound)
}

// testRenamed: the old path of a renamed repository leads to it, by Repo
// and by a repo selector, under its canonical path and the same id; the
// old login of a renamed account leads to it, with the same id. Fixtures
// that are no Renamer skip.
func testRenamed(t *testing.T, fx Fixture) {
	rn, ok := fx.(Renamer)
	if !ok {
		t.Skip("the fixture cannot rename")
	}
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "before-rename", Files: readme})
	renamed := rn.RenameRepo(t, repo, "after-rename")
	if renamed.ID != repo.ID || strings.EqualFold(renamed.Path, repo.Path) {
		t.Fatalf("RenameRepo = %s (%s), want a new path for %s", renamed.Path, renamed.ID, repo.ID)
	}
	got, err := fx.Reader().Repo(ctx, repo.Path)
	must(t, "Repo of the old path", err)
	if got.ID != repo.ID || got.Path != renamed.Path {
		t.Errorf("Repo(%s) = %s (%s), want %s (%s)", repo.Path, got.Path, got.ID, renamed.Path, repo.ID)
	}
	res, err := fx.Reader().Resolve(ctx, platform.Selector{Repo: repo.Path})
	must(t, "Resolve of the old path", err)
	if len(res.Repos) != 1 || res.Repos[0].ID != repo.ID || res.Repos[0].Path != renamed.Path || !res.Complete {
		t.Errorf("Resolve(repo %s) = %v (complete %v), want %s", repo.Path, paths(res.Repos), res.Complete, renamed.Path)
	}

	old, account := rn.RenamedAccount(t)
	a, err := fx.Reader().Lookup(ctx, old)
	must(t, "Lookup of the old login", err)
	if a.ID != account.ID || a.Login != account.Login {
		t.Errorf("Lookup(%s) = %s (%s), want %s (%s)", old, a.Login, a.ID, account.Login, account.ID)
	}
}

func testRepoFlags(t *testing.T, fx Fixture) {
	ctx := t.Context()
	empty := fx.CreateRepo(t, RepoSpec{Name: "empty"})
	archived := fx.CreateRepo(t, RepoSpec{Name: "archived", Files: readme, Archived: true})
	got, err := fx.Reader().Repo(ctx, empty.Path)
	must(t, "Repo of an empty repository", err)
	if !got.Empty {
		t.Error("a repository without commits is not Empty")
	}
	got, err = fx.Reader().Repo(ctx, archived.Path)
	must(t, "Repo of an archived repository", err)
	if !got.Archived || got.Empty {
		t.Errorf("archived repository: Archived %v, Empty %v", got.Archived, got.Empty)
	}
}

func testResolveRepo(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "alpha", Files: readme, Topics: []string{"conformance"}})
	// A Repo selector ignores the other fields.
	res, err := fx.Reader().Resolve(ctx, platform.Selector{Repo: repo.Path, Topics: []string{"no-such-topic"}, Namespace: "elsewhere"})
	must(t, "Resolve repo", err)
	sameIDs(t, "Resolve repo", res.Repos, repo)
	if !res.Complete {
		t.Error("Resolve repo: incomplete")
	}
	res, err = fx.Reader().Resolve(ctx, platform.Selector{Repo: strings.ToUpper(repo.Path)})
	must(t, "Resolve repo in upper case", err)
	sameIDs(t, "Resolve repo in upper case", res.Repos, repo)

	_, err = fx.Reader().Resolve(ctx, platform.Selector{Repo: fx.Namespace() + "/no-such-repository"})
	wantErr(t, "Resolve of a missing repository", err, platform.ErrNotFound, platform.ClassNotFound)
}

func testResolveNamespace(t *testing.T, fx Fixture) {
	ctx := t.Context()
	alpha := fx.CreateRepo(t, RepoSpec{Name: "alpha", Files: readme, Topics: []string{"conformance", "python"}})
	beta := fx.CreateRepo(t, RepoSpec{Name: "beta", Files: readme, Topics: []string{"conformance"}})
	gamma := fx.CreateRepo(t, RepoSpec{Name: "gamma", Files: readme, Topics: []string{"python", "conformance"}, Archived: true})
	delta := fx.CreateRepo(t, RepoSpec{Name: "delta", Topics: []string{"conformance"}})
	mine := []platform.Repo{alpha, beta, gamma, delta}

	for _, tc := range []struct {
		name   string
		topics []string
		want   []platform.Repo
	}{
		// Archived and empty repositories are listed: the core classifies them.
		{"all", nil, []platform.Repo{alpha, beta, delta, gamma}},
		{"one topic", []string{"python"}, []platform.Repo{alpha, gamma}},
		{"all topics", []string{"python", "conformance"}, []platform.Repo{alpha, gamma}},
		{"a missing topic", []string{"python", "no-such-topic"}, nil},
	} {
		res, err := fx.Reader().Resolve(ctx, platform.Selector{Namespace: fx.Namespace(), Topics: tc.topics})
		must(t, "Resolve namespace, "+tc.name, err)
		if !res.Complete {
			t.Errorf("Resolve namespace, %s: incomplete", tc.name)
		}
		if !sortedByPath(res.Repos) {
			t.Errorf("Resolve namespace, %s: not sorted by path: %v", tc.name, paths(res.Repos))
		}
		sameIDs(t, "Resolve namespace, "+tc.name, ours(res.Repos, mine...), tc.want...)
	}
}

func testResolveForks(t *testing.T, fx Fixture) {
	ctx := t.Context()
	alpha := fx.CreateRepo(t, RepoSpec{Name: "alpha", Files: readme})
	fork := fx.CreateRepo(t, RepoSpec{Name: "forked", Files: readme, Fork: true})
	if !fork.Fork {
		t.Fatalf("CreateRepo with Fork returned %s without Fork", fork.Path)
	}
	res, err := fx.Reader().Resolve(ctx, platform.Selector{Namespace: fx.Namespace()})
	must(t, "Resolve without forks", err)
	sameIDs(t, "Resolve without forks", ours(res.Repos, alpha, fork), alpha)

	res, err = fx.Reader().Resolve(ctx, platform.Selector{Namespace: fx.Namespace(), Forks: true})
	must(t, "Resolve with forks", err)
	sameIDs(t, "Resolve with forks", ours(res.Repos, alpha, fork), alpha, fork)

	// A Repo selector never skips a fork.
	res, err = fx.Reader().Resolve(ctx, platform.Selector{Repo: fork.Path})
	must(t, "Resolve a fork by path", err)
	sameIDs(t, "Resolve a fork by path", res.Repos, fork)
	if len(res.Repos) == 1 && !res.Repos[0].Fork {
		t.Error("Resolve a fork by path: Fork is not set")
	}
}

func testResolveSubgroups(t *testing.T, fx Fixture) {
	ctx := t.Context()
	top := fx.CreateRepo(t, RepoSpec{Name: "alpha", Files: readme})
	nested := fx.CreateRepo(t, RepoSpec{Name: "beta", Group: "sub", Files: readme})
	deeper := fx.CreateRepo(t, RepoSpec{Name: "gamma", Group: "sub/deeper", Files: readme})
	mine := []platform.Repo{top, nested, deeper}
	sub := path.Dir(nested.Path)

	for _, tc := range []struct {
		name string
		sel  platform.Selector
		want []platform.Repo
	}{
		{"namespace", platform.Selector{Namespace: fx.Namespace()}, []platform.Repo{top}},
		{"namespace with subgroups", platform.Selector{Namespace: fx.Namespace(), Subgroups: true}, []platform.Repo{top, nested, deeper}},
		{"subgroup", platform.Selector{Namespace: sub}, []platform.Repo{nested}},
		{"subgroup with subgroups", platform.Selector{Namespace: sub, Subgroups: true}, []platform.Repo{nested, deeper}},
	} {
		res, err := fx.Reader().Resolve(ctx, tc.sel)
		must(t, "Resolve "+tc.name, err)
		if !sortedByPath(res.Repos) {
			t.Errorf("Resolve %s: not sorted by path: %v", tc.name, paths(res.Repos))
		}
		sameIDs(t, "Resolve "+tc.name, ours(res.Repos, mine...), tc.want...)
	}
}

func testReadFile(t *testing.T, fx Fixture) {
	ctx := t.Context()
	readmeText := []byte("# conformance\n\nline two ⚠\n")
	script := []byte("#!/bin/sh\necho conformance\n")
	guide := []byte("guide\n")
	repo := fx.CreateRepo(t, RepoSpec{Name: "files", Files: []File{
		{Path: "README.md", Content: readmeText},
		{Path: "scripts/run.sh", Mode: "100755", Content: script},
		{Path: "docs/guide/intro.md", Content: guide},
	}})
	r := fx.Reader()

	// Plan reads the opt-in file again at the commit of its snapshot.
	head := fx.Head(t, repo)
	if !isFullHex(head) {
		t.Fatalf("Head = %q, want a full commit id", head)
	}
	for _, tc := range []struct {
		ref, path, mode string
		content         []byte
	}{
		{"", "README.md", "100644", readmeText},
		{"", "scripts/run.sh", "100755", script},
		{"", "docs/guide/intro.md", "100644", guide},
		{repo.DefaultBranch, "README.md", "100644", readmeText},
		{head, "README.md", "100644", readmeText},
		{head, "scripts/run.sh", "100755", script},
	} {
		f, err := r.ReadFile(ctx, repo, tc.ref, tc.path, 64<<10)
		must(t, "ReadFile "+tc.path+" at "+strconv.Quote(tc.ref), err)
		switch {
		case f.Path != tc.path:
			t.Errorf("ReadFile %s: path %q", tc.path, f.Path)
		case f.Mode != tc.mode:
			t.Errorf("ReadFile %s: mode %q, want %s", tc.path, f.Mode, tc.mode)
		case !bytes.Equal(f.Content, tc.content):
			t.Errorf("ReadFile %s: content %q, want %q", tc.path, f.Content, tc.content)
		case f.OID != gitx.RawOID(tc.content):
			t.Errorf("ReadFile %s: oid %s, want %s", tc.path, f.OID, gitx.RawOID(tc.content))
		}
	}

	for _, p := range []string{"docs", "docs/guide"} {
		_, err := r.ReadFile(ctx, repo, "", p, 64<<10)
		if !errors.Is(err, platform.ErrNotRegular) {
			t.Errorf("ReadFile of directory %s: %v, want ErrNotRegular", p, err)
		}
	}
	for _, p := range []string{"missing.md", "docs/missing.md", "README.md/x"} {
		_, err := r.ReadFile(ctx, repo, "", p, 64<<10)
		wantErr(t, "ReadFile of missing "+p, err, platform.ErrNotFound, platform.ClassNotFound)
	}

	if _, err := r.ReadFile(ctx, repo, "", "README.md", int64(len(readmeText))); err != nil {
		t.Errorf("ReadFile at exactly the limit: %v", err)
	}
	_, err := r.ReadFile(ctx, repo, "", "README.md", int64(len(readmeText))-1)
	if !errors.Is(err, platform.ErrTooLarge) {
		t.Errorf("ReadFile over the limit: %v, want ErrTooLarge", err)
	}
}

func testReadFileSymlink(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "links", Files: []File{
		{Path: "target.md", Content: []byte("secret of the target\n")},
		{Path: "docs/a.md", Content: []byte("a\n")},
		{Path: "link.md", Mode: "120000", Content: []byte("target.md")},
		{Path: "dirlink", Mode: "120000", Content: []byte("docs")},
	}})
	_, err := fx.Reader().ReadFile(ctx, repo, "", "link.md", 64<<10)
	if !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("ReadFile of a symlink: %v, want ErrNotRegular", err)
	}
	// Never through a link: the file behind it is not served.
	f, err := fx.Reader().ReadFile(ctx, repo, "", "dirlink/a.md", 64<<10)
	if !errors.Is(err, platform.ErrNotFound) && !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("ReadFile through a symlinked directory = %q, %v; want ErrNotFound or ErrNotRegular", f.Content, err)
	}
}

func testReadFileSubmodule(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "submodules", Files: []File{
		readme[0],
		{Path: "vendor/lib", Mode: "160000", Content: []byte("0123456789abcdef0123456789abcdef01234567")},
	}})
	_, err := fx.Reader().ReadFile(t.Context(), repo, "", "vendor/lib", 64<<10)
	if !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("ReadFile of a submodule: %v, want ErrNotRegular", err)
	}
}

func testReadFileEmptyRepo(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "empty"})
	_, err := fx.Reader().ReadFile(t.Context(), repo, "", "README.md", 64<<10)
	wantErr(t, "ReadFile in an empty repository", err, platform.ErrNotFound, platform.ClassNotFound)
}

// testReadFiles checks platform.BatchReader, for the drivers whose reader
// is one: one path read across repositories where it is a regular file, an
// executable, binary content, a file over the limit, a symlink, a
// submodule, a directory, missing, and an empty repository comes back as
// ReadFile reads it in each: the same file, or an error of the same kind
// (ErrNotFound, ErrNotRegular, ErrTooLarge). A nested path through a
// symlinked directory is never a file.
func testReadFiles(t *testing.T, fx Fixture) {
	ctx := t.Context()
	br, ok := fx.Reader().(platform.BatchReader)
	if !ok {
		t.Skip("the reader reads files one by one")
	}
	const p, max = "cfg.yml", 1024
	specs := []RepoSpec{
		{Name: "batch-file", Files: []File{{Path: p, Content: []byte("version: 1\n")}}},
		{Name: "batch-exec", Files: []File{{Path: p, Mode: "100755", Content: []byte("#!/bin/sh\necho batch\n")}}},
		{Name: "batch-binary", Files: []File{{Path: p, Content: []byte{0, 1, 2, 0xff, 0xfe, 0}}}},
		{Name: "batch-large", Files: []File{{Path: p, Content: bytes.Repeat([]byte("x"), 2*max)}}},
		{Name: "batch-link", Files: []File{readme[0], {Path: p, Mode: "120000", Content: []byte("README.md")}}},
		{Name: "batch-sub", Files: []File{readme[0], {Path: p, Mode: "160000", Content: []byte("0123456789abcdef0123456789abcdef01234567")}}},
		{Name: "batch-dir", Files: []File{{Path: p + "/inner.md", Content: []byte("inner\n")}}},
		{Name: "batch-missing", Files: readme},
		{Name: "batch-empty"},
	}
	repos := make([]platform.Repo, len(specs))
	for i, s := range specs {
		repos[i] = fx.CreateRepo(t, s)
	}
	files, err := br.ReadFiles(ctx, repos, p, max)
	var fe platform.FileErrors
	if err != nil && !errors.As(err, &fe) {
		t.Fatalf("ReadFiles: %v", err)
	}
	if len(files) != len(repos) || (fe != nil && len(fe) != len(repos)) {
		t.Fatalf("ReadFiles: %d files, %d errors for %d repositories", len(files), len(fe), len(repos))
	}
	for i, r := range repos {
		want, wantErr := fx.Reader().ReadFile(ctx, r, "", p, max)
		var got error
		if fe != nil {
			got = fe[i]
		}
		if wantErr == nil {
			if f := files[i]; got != nil || f.OID != want.OID || f.Mode != want.Mode || !bytes.Equal(f.Content, want.Content) {
				t.Errorf("%s: ReadFiles = %s %s %q, %v; ReadFile = %s %s %q", specs[i].Name, f.Mode, f.OID, f.Content, got, want.Mode, want.OID, want.Content)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: ReadFiles read %q; ReadFile failed: %v", specs[i].Name, files[i].Content, wantErr)
			continue
		}
		for _, sentinel := range []error{platform.ErrNotFound, platform.ErrNotRegular, platform.ErrTooLarge} {
			if errors.Is(got, sentinel) != errors.Is(wantErr, sentinel) {
				t.Errorf("%s: ReadFiles: %v; ReadFile: %v", specs[i].Name, got, wantErr)
			}
		}
	}

	linked := fx.CreateRepo(t, RepoSpec{Name: "batch-dirlink", Files: []File{
		{Path: "docs/" + p, Content: []byte("behind the link\n")},
		{Path: "conf", Mode: "120000", Content: []byte("docs")},
	}})
	files, err = br.ReadFiles(ctx, []platform.Repo{linked}, "conf/"+p, max)
	if err == nil {
		t.Errorf("ReadFiles through a symlinked directory = %q", files[0].Content)
	} else if !errors.Is(err, platform.ErrNotFound) && !errors.Is(err, platform.ErrNotRegular) {
		t.Errorf("ReadFiles through a symlinked directory: %v; want ErrNotFound or ErrNotRegular", err)
	}
}

func testRemote(t *testing.T, fx Fixture) {
	repo := fx.CreateRepo(t, RepoSpec{Name: "remote", Files: readme})
	rem, err := fx.Reader().Remote(t.Context(), repo)
	must(t, "Remote", err)
	noCredentials(t, "Remote", rem.URL)
}

// testPRs covers PRs on one repository: the writer's pull requests on the
// sync branches in every state, open ones by anyone there, nothing from
// other branches and nothing closed by others.
func testPRs(t *testing.T, fx Fixture) {
	ctx := t.Context()
	caps := probe(t, fx)
	repo := fx.CreateRepo(t, RepoSpec{Name: "prs", Files: readme})
	writer, other := fx.Account(RoleWriter), fx.Account(RoleOther)

	merged := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "merged", Body: "merged", Author: RoleWriter})
	fx.SetPRState(t, repo, merged.Number, platform.Merged, RoleOther)
	declined := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "declined", Body: "declined", Author: RoleWriter})
	fx.SetPRState(t, repo, declined.Number, platform.Closed, RoleOther)
	selfClosed := fx.CreatePR(t, repo, PRSpec{Head: AliasBranch, Title: "self-closed", Body: "self-closed", Author: RoleWriter})
	fx.SetPRState(t, repo, selfClosed.Number, platform.Closed, RoleWriter)
	foreignClosed := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "foreign closed", Body: "x", Author: RoleOther})
	fx.SetPRState(t, repo, foreignClosed.Number, platform.Closed, RoleOther)
	foreignElsewhere := fx.CreatePR(t, repo, PRSpec{Head: OtherBranch, Title: "foreign elsewhere", Body: "x", Author: RoleOther})
	ownElsewhere := fx.CreatePR(t, repo, PRSpec{Head: OwnBranch, Title: "own elsewhere", Body: "x", Author: RoleWriter})
	open := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "open", Body: markerBody, Author: RoleWriter,
		Labels: []string{"engineering-assets"}})
	foreignOpen := fx.CreatePR(t, repo, PRSpec{Head: AliasBranch, Title: "foreign open", Body: "x", Author: RoleOther})

	heads := []string{SyncBranch, AliasBranch}
	got, err := fx.Reader().PRs(ctx, repo, heads, []platform.Account{writer})
	must(t, "PRs", err)
	if want := newestFirst(merged, declined, selfClosed, open, foreignOpen); !slices.Equal(numbers(got), want) {
		t.Fatalf("PRs = %v, want %v (not %d, %d, %d: closed by others or other branches)", numbers(got), want,
			foreignClosed.Number, foreignElsewhere.Number, ownElsewhere.Number)
	}

	for _, tc := range []struct {
		pr     platform.PR
		head   string
		state  platform.PRState
		author platform.Account
		by     platform.Account
	}{
		{merged, SyncBranch, platform.Merged, writer, other},
		{declined, SyncBranch, platform.Closed, writer, other},
		{selfClosed, AliasBranch, platform.Closed, writer, writer},
		{open, SyncBranch, platform.Open, writer, platform.Account{}},
		{foreignOpen, AliasBranch, platform.Open, other, platform.Account{}},
	} {
		pr, _ := find(got, tc.pr.Number)
		switch {
		case pr.State != tc.state:
			t.Errorf("#%d: state %s, want %s", pr.Number, pr.State, tc.state)
		case pr.Head != tc.head || pr.Base != repo.DefaultBranch:
			t.Errorf("#%d: %s → %s, want %s → %s", pr.Number, pr.Head, pr.Base, tc.head, repo.DefaultBranch)
		case pr.Author.ID != tc.author.ID:
			t.Errorf("#%d: author %s, want %s", pr.Number, pr.Author.ID, tc.author.ID)
		case pr.RepoID != repo.ID || pr.HeadRepoID != repo.ID:
			t.Errorf("#%d: repo %s, head repo %s, want %s for both", pr.Number, pr.RepoID, pr.HeadRepoID, repo.ID)
		case !pr.BaseExists:
			t.Errorf("#%d: BaseExists is false", pr.Number)
		case pr.Title != tc.pr.Title:
			t.Errorf("#%d: title %q, want %q", pr.Number, pr.Title, tc.pr.Title)
		case pr.CreatedAt.IsZero():
			t.Errorf("#%d: no CreatedAt", pr.Number)
		case pr.HeadSHA != "" && !isFullHex(pr.HeadSHA):
			t.Errorf("#%d: HeadSHA %q, want a full commit id where the platform reports one", pr.Number, pr.HeadSHA)
		}
		if tc.state == platform.Open {
			if pr.ClosedBy != nil || !pr.ClosedAt.IsZero() {
				t.Errorf("#%d: open with ClosedBy %v, ClosedAt %v", pr.Number, pr.ClosedBy, pr.ClosedAt)
			}
			continue
		}
		if pr.ClosedAt.IsZero() {
			t.Errorf("#%d: %s without ClosedAt", pr.Number, pr.State)
		}
		checkClosedBy(t, caps, pr, tc.by)
	}
	if pr, _ := find(got, open.Number); pr.Body != markerBody || !slices.Contains(pr.Labels, "engineering-assets") {
		t.Errorf("#%d: body %q, labels %v; want the body verbatim and the label", pr.Number, pr.Body, pr.Labels)
	}

	// Several authors (writer and known_authors): every one's pull requests
	// in every state, not the first author's only.
	got, err = fx.Reader().PRs(ctx, repo, heads, []platform.Account{writer, other})
	must(t, "PRs of two authors", err)
	if want := newestFirst(merged, declined, selfClosed, foreignClosed, open, foreignOpen); !slices.Equal(numbers(got), want) {
		t.Errorf("PRs of two authors = %v, want %v (not %d, %d: other branches)", numbers(got), want,
			foreignElsewhere.Number, ownElsewhere.Number)
	}
	got, err = fx.Reader().PRs(ctx, repo, heads, []platform.Account{other, writer})
	must(t, "PRs of two authors, the other first", err)
	if want := newestFirst(merged, declined, selfClosed, foreignClosed, open, foreignOpen); !slices.Equal(numbers(got), want) {
		t.Errorf("PRs of two authors, the other first = %v, want %v", numbers(got), want)
	}

	// Without authors: open pull requests by anyone.
	got, err = fx.Reader().PRs(ctx, repo, heads, nil)
	must(t, "PRs without authors", err)
	if want := newestFirst(open, foreignOpen); !slices.Equal(numbers(got), want) {
		t.Errorf("PRs without authors = %v, want %v", numbers(got), want)
	}
	// One head only.
	got, err = fx.Reader().PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs of one head", err)
	if want := newestFirst(merged, declined, open); !slices.Equal(numbers(got), want) {
		t.Errorf("PRs of one head = %v, want %v", numbers(got), want)
	}
}

// testPRsFork: a pull request from a fork with the sync branch's name and a
// copy of the body is listed, and says it comes from another repository.
func testPRsFork(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "forked-prs", Files: readme})
	writer := fx.Account(RoleWriter)
	closedFork := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "closed fork", Body: markerBody, Author: RoleOther, Fork: true})
	fx.SetPRState(t, repo, closedFork.Number, platform.Closed, RoleOther)
	own := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "own", Body: markerBody, Author: RoleWriter})
	fork := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "fork", Body: markerBody, Author: RoleOther, Fork: true})

	got, err := fx.Reader().PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	if want := newestFirst(own, fork); !slices.Equal(numbers(got), want) {
		t.Fatalf("PRs = %v, want %v (not the closed fork PR #%d)", numbers(got), want, closedFork.Number)
	}
	f, _ := find(got, fork.Number)
	switch {
	case f.RepoID != repo.ID:
		t.Errorf("fork PR: RepoID %s, want %s", f.RepoID, repo.ID)
	case f.HeadRepoID == "" || f.HeadRepoID == repo.ID:
		t.Errorf("fork PR: HeadRepoID %q, want the fork's id", f.HeadRepoID)
	case f.Head != SyncBranch || f.Body != markerBody:
		t.Errorf("fork PR: head %q, body %q", f.Head, f.Body)
	}
	if o, _ := find(got, own.Number); o.HeadRepoID != repo.ID {
		t.Errorf("own PR: HeadRepoID %s, want %s", o.HeadRepoID, repo.ID)
	}
}

// testPRsBaseDeleted: an open pull request whose base branch is deleted
// says so, whether the platform leaves it open or closes it. A platform
// may instead move it onto the default branch (Gitea by default,
// RETARGET_CHILDREN_ON_MERGE), so that such a pull request never arises:
// its fixture then skips in DeleteBranch, and testPRsBaseDeletedClosed
// still checks BaseExists there.
func testPRsBaseDeleted(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "base-deleted", Files: readme})
	writer := fx.Account(RoleWriter)
	fx.CreateBranch(t, repo, "release")
	pr := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Base: "release", Title: "to release", Body: "x", Author: RoleWriter})
	got, err := fx.Reader().PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	if p, ok := find(got, pr.Number); !ok || !p.BaseExists || p.Base != "release" {
		t.Fatalf("before the deletion: %+v, want base release that exists", p)
	}
	fx.DeleteBranch(t, repo, "release")
	got, err = fx.Reader().PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	p, ok := find(got, pr.Number)
	if !ok || p.BaseExists || p.Base != "release" {
		t.Errorf("after the deletion: found %v, base %q, BaseExists %v; want base release that does not exist",
			ok, p.Base, p.BaseExists)
	}
}

// testPRsBaseDeletedClosed: a closed pull request whose base branch is
// deleted afterwards says so too. Memory takes the close of an own pull
// request whose base is gone for an auto-close, not a decline, and every
// platform leaves a closed pull request as it is when its base goes, so
// this holds where testPRsBaseDeleted cannot be arranged.
func testPRsBaseDeletedClosed(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "base-deleted-closed", Files: readme})
	writer := fx.Account(RoleWriter)
	fx.CreateBranch(t, repo, "release")
	pr := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Base: "release", Title: "to release", Body: "x", Author: RoleWriter})
	fx.SetPRState(t, repo, pr.Number, platform.Closed, RoleOther)
	got, err := fx.Reader().PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	if p, ok := find(got, pr.Number); !ok || p.State != platform.Closed || !p.BaseExists || p.Base != "release" {
		t.Fatalf("closed, before the deletion: found %v, state %s, base %q, BaseExists %v; want closed, base release that exists",
			ok, p.State, p.Base, p.BaseExists)
	}
	fx.DeleteBranch(t, repo, "release")
	got, err = fx.Reader().PRs(ctx, repo, []string{SyncBranch}, []platform.Account{writer})
	must(t, "PRs", err)
	p, ok := find(got, pr.Number)
	if !ok || p.State != platform.Closed || p.BaseExists || p.Base != "release" {
		t.Errorf("closed, after the deletion: found %v, state %s, base %q, BaseExists %v; want closed, base release that does not exist",
			ok, p.State, p.Base, p.BaseExists)
	}
}

func testOpenPRsBy(t *testing.T, fx Fixture) {
	ctx := t.Context()
	a := fx.CreateRepo(t, RepoSpec{Name: "sweep-a", Files: readme})
	b := fx.CreateRepo(t, RepoSpec{Name: "sweep-b", Files: readme})
	writer, other := fx.Account(RoleWriter), fx.Account(RoleOther)
	onA := fx.CreatePR(t, a, PRSpec{Head: SyncBranch, Title: "a", Body: markerBody, Author: RoleWriter})
	onB := fx.CreatePR(t, b, PRSpec{Head: AliasBranch, Title: "b", Body: markerBody, Author: RoleWriter})
	closed := fx.CreatePR(t, b, PRSpec{Head: SyncBranch, Title: "closed", Body: "x", Author: RoleWriter})
	fx.SetPRState(t, b, closed.Number, platform.Closed, RoleWriter)
	foreign := fx.CreatePR(t, a, PRSpec{Head: AliasBranch, Title: "foreign", Body: markerBody, Author: RoleOther})
	fx.CreatePR(t, a, PRSpec{Head: OwnBranch, Title: "elsewhere", Body: "x", Author: RoleWriter})

	ref := func(r platform.Repo, pr platform.PR) string { return r.Path + "#" + itoa(pr.Number) }
	for _, tc := range []struct {
		name    string
		authors []platform.Account
		want    []string
	}{
		{"the writer", []platform.Account{writer}, []string{ref(a, onA), ref(b, onB)}},
		// known_authors are swept too: every author, not the first only.
		{"two authors", []platform.Account{writer, other}, []string{ref(a, onA), ref(b, onB), ref(a, foreign)}},
	} {
		for name, r := range map[string]platform.Reader{"reader": fx.Reader(), "writer": fx.Writer()} {
			sw, err := r.OpenPRsBy(ctx, tc.authors, []string{SyncBranch, AliasBranch})
			must(t, name+" OpenPRsBy "+tc.name, err)
			if !sw.Complete {
				t.Errorf("%s OpenPRsBy %s: incomplete", name, tc.name)
			}
			var found []string
			for _, rp := range sw.PRs {
				if rp.Repo.ID != a.ID && rp.Repo.ID != b.ID {
					continue
				}
				found = append(found, ref(rp.Repo, rp.PR))
				byAuthor := slices.ContainsFunc(tc.authors, func(acc platform.Account) bool { return acc.ID == rp.PR.Author.ID })
				if rp.PR.State != platform.Open || !byAuthor || rp.PR.RepoID != rp.Repo.ID {
					t.Errorf("%s OpenPRsBy %s: %s#%d state %s author %s repo %s", name, tc.name, rp.Repo.Path, rp.PR.Number,
						rp.PR.State, rp.PR.Author.ID, rp.PR.RepoID)
				}
			}
			want := slices.Clone(tc.want)
			slices.Sort(found)
			slices.Sort(want)
			if !slices.Equal(found, want) {
				t.Errorf("%s OpenPRsBy %s = %v, want %v", name, tc.name, found, want)
			}
		}
	}
}

// isFullHex reports whether s is a full commit id: 40 or 64 lowercase hex
// digits.
func isFullHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// testConcurrent reads from several goroutines at once and expects what a
// sequential read gives (and no data race under -race).
func testConcurrent(t *testing.T, fx Fixture) {
	ctx := t.Context()
	repo := fx.CreateRepo(t, RepoSpec{Name: "concurrent", Files: readme})
	pr := fx.CreatePR(t, repo, PRSpec{Head: SyncBranch, Title: "concurrent", Body: markerBody, Author: RoleWriter})
	writer := fx.Account(RoleWriter)
	r := fx.Reader()

	const workers, rounds = 8, 4
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				errs <- concurrentRound(ctx, r, repo, pr.Number, writer)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
