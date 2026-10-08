// Package conformance is the contract suite every platform driver passes:
// the fake, and the GitHub driver over its HTTP fake, on every test run;
// the Gitea, Forgejo and GitLab drivers in their live e2e tests.
//
// Run checks, from the caller's side, what package platform documents about
// Reader, Writer and TargetWriter: stable ids and account kinds,
// case-insensitive repository paths, selector semantics, regular files only
// (at the default branch and at a commit id), pull requests by every author
// given and head in every state plus open ones by anyone, fork pull
// requests reported as such and never taken for the duplicate of a new
// one, one-step edits that never remove labels or change draft state and
// change nothing when refused, old names of renamed repositories and
// accounts that still lead to them (fixtures that are a Renamer),
// ErrExists with the open duplicate from the repository itself, whatever
// the base, a
// per-target identity that is dead after Close, the error classes of
// missing things, and, for the drivers that offer them, API commits with
// compare-and-swap through a stage ref (platform.Committer) and rules read
// before any write (platform.Preflighter). It asserts nothing a platform
// may legitimately do
// otherwise: extra repositories in a namespace, a refused draft, a closer
// or head commit the platform does not report, an open pull request moved
// onto the default branch when its base branch is deleted (the fixture
// skips that case; the closed one is checked everywhere).
//
// A driver brings a Fixture: a platform prepared with three accounts and a
// namespace to create repositories and pull requests in.
package conformance

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Role names an account of a fixture.
type Role uint8

const (
	// RoleReader is the read identity, Fixture.Reader.
	RoleReader Role = iota
	// RoleWriter is the write identity, Fixture.Writer: the author of
	// touchmark's own pull requests. It is not the reader's account.
	RoleWriter
	// RoleOther is anyone else with write access: a person or another bot.
	RoleOther
)

// String names the role in messages.
func (r Role) String() string {
	switch r {
	case RoleReader:
		return "reader"
	case RoleWriter:
		return "writer"
	case RoleOther:
		return "other"
	}
	return "role(" + strconv.Itoa(int(r)) + ")"
}

// File is a tree entry of a repository a fixture creates.
type File struct {
	Path string
	// Mode is "100644" (also when empty), "100755", "120000" (a symlink;
	// Content is its target) or "160000" (a submodule; Content is the
	// commit id in hex).
	Mode    string
	Content []byte
}

// RepoSpec describes a repository to create.
type RepoSpec struct {
	// Name is the repository name, lowercase and unique in the fixture.
	Name string
	// Group is a nested namespace under Fixture.Namespace ("sub",
	// "sub/deeper"), "" for the namespace itself. Fixtures of platforms
	// without nested namespaces skip.
	Group string
	// Files are committed to the default branch; none leaves the
	// repository empty (no commits).
	Files  []File
	Topics []string
	// Archived archives the repository after its files are committed.
	Archived bool
	// Fork makes the repository a fork of a repository outside the
	// namespace.
	Fork bool
	// ReadOnly withholds write access from the writer.
	ReadOnly bool
}

// PRSpec describes a pull request to open.
type PRSpec struct {
	// Head is the source branch. The fixture adds a commit to it on top of
	// the default branch, creating it when missing (in the fork with Fork),
	// so the pull request always has changes.
	Head string
	// Base is the target branch; "" is the default branch.
	Base   string
	Title  string
	Body   string
	Labels []string
	// Author opens the pull request: RoleWriter or RoleOther.
	Author Role
	// Fork opens it from a fork of the repository owned by Author.
	Fork bool
}

// Fixture is one platform prepared for one test; Run asks for a new one per
// subtest. Setup methods fail the test through t when the platform refuses,
// and skip it (t.Skip) when the platform or the fixture cannot arrange what
// is asked, such as nested namespaces on GitHub.
type Fixture interface {
	// Reader and Writer are the drivers under test, for RoleReader and
	// RoleWriter.
	Reader() platform.Reader
	Writer() platform.Writer
	// Account returns the account of role as the platform reports it.
	Account(role Role) platform.Account
	// Namespace is the organisation or group CreateRepo creates
	// repositories in. It may hold other repositories: the suite looks
	// only at its own.
	Namespace() string
	// CreateRepo creates a repository in Namespace, or in its nested
	// namespace spec.Group, and returns it as the platform reports it once
	// its files and flags are in place. The writer may write to it unless
	// spec.ReadOnly; RoleOther always may.
	CreateRepo(t *testing.T, spec RepoSpec) platform.Repo
	// CreateBranch creates branch name with one commit on top of the
	// default branch, so a pull request from it has changes.
	CreateBranch(t *testing.T, repo platform.Repo, name string)
	// Head returns the full id of the commit at the tip of the default
	// branch.
	Head(t *testing.T, repo platform.Repo) string
	// DeleteBranch deletes branch name, never the default branch.
	DeleteBranch(t *testing.T, repo platform.Repo, name string)
	// CreatePR opens a pull request as spec.Author and returns it as the
	// platform reports it.
	CreatePR(t *testing.T, repo platform.Repo, spec PRSpec) platform.PR
	// SetPRState closes, merges or reopens a pull request as by.
	SetPRState(t *testing.T, repo platform.Repo, number int64, state platform.PRState, by Role)
	// Comments returns the bodies of the comments on a pull request, oldest
	// first.
	Comments(t *testing.T, repo platform.Repo, number int64) []string
}

// Renamer is a Fixture that can rename. Platforms that remember old names
// (GitHub, GitLab, Gitea and Forgejo redirect them) implement it, and the
// suite checks that an old name still leads to the renamed repository or
// account: targets.yml and hub.yml may name them by an old name.
type Renamer interface {
	// RenameRepo renames repo, in its namespace, to name and returns it as
	// the platform reports it afterwards.
	RenameRepo(t *testing.T, repo platform.Repo, name string) platform.Repo
	// RenamedAccount creates an account, renames it and returns its first
	// login and the account as the platform reports it afterwards.
	RenamedAccount(t *testing.T) (oldLogin string, renamed platform.Account)
}

// Branch names the suite uses: the sync branch, an alias and unrelated
// branches.
const (
	SyncBranch  = "touchmark/conformance"
	AliasBranch = "chore/sync-engineering-assets"
	OtherBranch = "feature/other"
	OwnBranch   = "feature/own"
)

// Run runs the suite, one subtest per contract, each with its own fixture.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"Probe", testProbe},
		{"Self", testSelf},
		{"Lookup", testLookup},
		{"Repo", testRepo},
		{"Repo/flags", testRepoFlags},
		{"Renamed", testRenamed},
		{"Resolve/repo", testResolveRepo},
		{"Resolve/namespace", testResolveNamespace},
		{"Resolve/forks", testResolveForks},
		{"Resolve/subgroups", testResolveSubgroups},
		{"ReadFile", testReadFile},
		{"ReadFile/symlink", testReadFileSymlink},
		{"ReadFile/submodule", testReadFileSubmodule},
		{"ReadFile/empty-repo", testReadFileEmptyRepo},
		{"Remote", testRemote},
		{"PRs", testPRs},
		{"PRs/fork", testPRsFork},
		{"PRs/base-deleted", testPRsBaseDeleted},
		{"PRs/base-deleted-closed", testPRsBaseDeletedClosed},
		{"OpenPRsBy", testOpenPRsBy},
		{"Target", testTarget},
		{"Target/denied", testTargetDenied},
		{"Target/git", testTargetGit},
		{"CreatePR", testCreatePR},
		{"CreatePR/beside-fork", testCreatePRBesideFork},
		{"CreatePR/after-close", testCreatePRAfterClose},
		{"CreatePR/draft", testCreatePRDraft},
		{"CreatePR/max-body", testCreatePRMaxBody},
		{"EditPR", testEditPR},
		{"EditPR/state", testEditPRState},
		{"EditPR/base", testEditPRBase},
		{"EditPR/refused", testEditPRRefused},
		{"EditPR/missing", testEditPRMissing},
		{"Comment", testComment},
		{"EnsureLabels", testEnsureLabels},
		{"Commit", testCommit},
		{"Preflight", testPreflight},
		{"PushGuard", testPushGuard},
		{"Concurrent", testConcurrent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newFixture(t))
		})
	}
}

// Helpers.

// readme is the one file most repositories of the suite hold.
var readme = []File{{Path: "README.md", Content: []byte("# conformance\n")}}

// markerBody is a body shaped like touchmark's: text, a table, non-ASCII
// and the marker as the last line. It must survive byte for byte.
const markerBody = "Sync from hub `conformance` ⚠ sensitive paths below.\n\n" +
	"| change | file | pack |\n|---|---|---|\n| add | `AGENTS.md` | agents |\n\n" +
	"<!-- touchmark:v1 hub=conformance fp=0123456789abcdef stream=sync " +
	"key=sha256:4f0c2c6d33a0f1c5c1d1b8e0a9f3a1e2b4c6d8e0f2a4c6e8a0b2c4d6e8f0a2b4 data=H4sIAAAAAAAA -->"

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// wantErr checks that err wraps sentinel and ClassOf gives class.
func wantErr(t *testing.T, what string, err, sentinel error, class platform.Class) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error, want %v", what, sentinel)
	case !errors.Is(err, sentinel):
		t.Errorf("%s: %v, want an error wrapping %q", what, err, sentinel)
	case platform.ClassOf(err) != class:
		t.Errorf("%s: class %v of %v, want %v", what, platform.ClassOf(err), err, class)
	}
}

func ptr[T any](v T) *T { return &v }

// ids returns the ids of repos.
func ids(repos []platform.Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.ID
	}
	return out
}

// ours keeps the repositories whose ids are in mine, in order.
func ours(repos []platform.Repo, mine ...platform.Repo) []platform.Repo {
	var out []platform.Repo
	for _, r := range repos {
		if slices.ContainsFunc(mine, func(m platform.Repo) bool { return m.ID == r.ID }) {
			out = append(out, r)
		}
	}
	return out
}

// sameIDs compares id lists in order.
func sameIDs(t *testing.T, what string, got []platform.Repo, want ...platform.Repo) {
	t.Helper()
	if g, w := ids(got), ids(want); !slices.Equal(g, w) {
		t.Errorf("%s: repositories %v, want %v", what, paths(got), paths(want))
	}
}

func paths(repos []platform.Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Path
	}
	return out
}

// sortedByPath reports whether repos are in path order, case folded.
func sortedByPath(repos []platform.Repo) bool {
	return slices.IsSortedFunc(repos, func(a, b platform.Repo) int {
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})
}

// numbers returns the numbers of prs.
func numbers(prs []platform.PR) []int64 {
	out := make([]int64, len(prs))
	for i, pr := range prs {
		out[i] = pr.Number
	}
	return out
}

// newestFirst returns the numbers of prs, largest first.
func newestFirst(prs ...platform.PR) []int64 {
	out := numbers(prs)
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

func find(prs []platform.PR, number int64) (platform.PR, bool) {
	for _, pr := range prs {
		if pr.Number == number {
			return pr, true
		}
	}
	return platform.PR{}, false
}

// noCredentials checks that a remote URL carries no user info.
func noCredentials(t *testing.T, what, raw string) {
	t.Helper()
	if raw == "" {
		t.Errorf("%s: empty URL", what)
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Errorf("%s: URL %q: %v", what, raw, err)
		return
	}
	if u.User != nil {
		t.Errorf("%s: URL %q carries credentials", what, u.Redacted())
	}
}

// target returns a per-target writer with contents and pull requests,
// closed at the end of the test.
func target(t *testing.T, fx Fixture, repo platform.Repo) platform.TargetWriter {
	t.Helper()
	tw, err := fx.Writer().Target(t.Context(), repo, platform.Perms{Contents: true, PRs: true})
	must(t, "Target", err)
	t.Cleanup(func() {
		if err := tw.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return tw
}

// probe returns the capabilities the reader reports.
func probe(t *testing.T, fx Fixture) platform.Caps {
	t.Helper()
	caps, err := fx.Reader().Probe(t.Context())
	must(t, "Probe", err)
	return caps
}

// checkClosedBy checks the closer of a closed or merged pull request where
// the platform reports closers.
func checkClosedBy(t *testing.T, caps platform.Caps, pr platform.PR, by platform.Account) {
	t.Helper()
	if !caps.CloserKnown && pr.State == platform.Closed {
		return
	}
	switch {
	case pr.ClosedBy == nil && caps.CloserKnown:
		t.Errorf("#%d: ClosedBy is nil, want %s (Caps.CloserKnown)", pr.Number, by.Login)
	case pr.ClosedBy != nil && pr.ClosedBy.ID != by.ID:
		t.Errorf("#%d: ClosedBy %s (%s), want %s (%s)", pr.Number, pr.ClosedBy.Login, pr.ClosedBy.ID, by.Login, by.ID)
	}
}
