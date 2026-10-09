package distribute

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// The adversarial tests run distribute end to end on the fake platform in
// git mode: a world whose hub (packs, hub.yml, targets.yml and
// operations.yml) the test changes between runs, and whose repositories
// people change through the fake's human actions. The property test
// (sim_property_test.go) drives random sequences of such events and checks
// the invariants of the package doc after every run (sim_check_test.go); the
// crash matrix (sim_crash_test.go) cuts runs at every kind of write.

// The hub of the simulations.
const (
	simOptIn   = ".engineering-assets.yml"
	simBase    = "base"
	simExtra   = "extra"
	simTitle   = "chore: sync engineering assets"
	simOtherFP = "github.com/424242"
)

// simHosts are the hosts of the fake per flavor, and simTypes the
// provider types hub.yml gives them.
var (
	simHosts = map[fake.Flavor]string{fake.GitHub: "github.com", fake.GitLab: "gitlab.example.com",
		fake.Gitea: "gitea.example.com", fake.Forgejo: "forgejo.example.com", fake.Bitbucket: "bitbucket.org",
		fake.AzureDevOps: "dev.azure.com", fake.BitbucketDataCenter: "bitbucket.example.com"}
	simTypes = map[fake.Flavor]string{fake.GitHub: "github", fake.GitLab: "gitlab", fake.Gitea: "gitea", fake.Forgejo: "forgejo",
		fake.Bitbucket: "bitbucket", fake.AzureDevOps: "azure-devops", fake.BitbucketDataCenter: "bitbucket-datacenter"}
)

// simTokens are the git tokens of the accounts of a world.
var simTokens = map[string]string{
	"acme-read[bot]":  "sim-reader-token-3b7d0e11",
	"acme-write[bot]": "sim-writer-token-9c21f4aa",
	"jdoe":            "sim-person-token-5e88a1f0",
	"stale[bot]":      "sim-stale-token-07d6c2b9",
}

// simPacks gives the pack that ships each path of the hub; paths of the
// two packs never overlap.
var simPacks = map[string]string{
	"AGENTS.md":     simBase,
	"docs/guide.md": simBase,
	"docs/old.md":   simBase,
	"docs/extra.md": simExtra,
}

// simContent is version n of the hub file at path: over the 64 bytes that
// make content evidence of ownership (provenance.MinEvidenceSize).
func simContent(path string, n int) string {
	return strings.Repeat(fmt.Sprintf("%s, version %d of the shared engineering asset\n", path, n), 3)
}

// simLocal is content of the team's own at path.
func simLocal(path string, n int) string {
	return strings.Repeat(fmt.Sprintf("%s, the team's own text, edit %d\n", path, n), 3)
}

// simClock is the clock of a world: the fake platform and every run read
// it, and each reading is a second after the last, so events are ordered.
type simClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the next second.
func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

// Advance moves the clock by d.
func (c *simClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// simTarget is one repository of a world.
type simTarget struct {
	name string
	repo platform.Repo
}

// simWorld is the fake platform, the hub and the targets of one scenario.
type simWorld struct {
	t      *testing.T
	ctx    context.Context
	p      *fake.Platform
	flavor fake.Flavor
	host   string
	clock  *simClock
	// app marks a GitHub world whose writer is a GitHub App: the platform
	// reads rules upfront and makes API commits, and every target requires
	// signed commits, so that each push of touchmark's goes through the
	// stage ref and the API commit.
	app bool
	// In an app world: workflowsOff takes the Workflows permission from the
	// writer (on every target, as on GitHub, where it is the installation's;
	// the runs' CanWorkflows follows it, as an owner who knows would set it);
	// noForce holds the targets whose sync branches a ruleset keeps from force
	// pushes; apiUnsigned makes the platform's API commits unsigned (GitHub
	// Enterprise Server without web commit signing).
	workflowsOff bool
	noForce      map[string]bool
	apiUnsigned  bool

	reader, writer, person, bot platform.Account
	src                         *snapshot.GitSource

	// The hub: its id and branch aliases (every id it had, older first),
	// what each pack ships now (pack → path → content), the manifest of
	// everything ever shipped, the hub's blobs, and the hub commit with its
	// date.
	hubID     string
	aliases   []string
	files     map[string]map[string]string
	manifest  *provenance.Manifest
	blobs     map[string][]byte
	hubN      int
	hubCommit string
	hubTime   time.Time
	// versions is the newest version number used for each path.
	versions map[string]int
	// excluded are the targets targets.yml excludes; ops is operations.yml
	// (nil for none).
	excluded map[string]bool
	ops      *config.Operations

	targets []*simTarget
	fork    platform.Repo
	// edits counts the team's own edits, for distinct content.
	edits int
	// gitConfig is an empty global git config for the test's own git.
	gitConfig string
}

// simHeavy skips a heavy adversarial test outside Linux unless
// TOUCHMARK_HEAVY_TESTS is set: every run starts dozens of git processes,
// which cost many times more on Windows and macOS (where CI runs the suite
// without the race detector); the Linux job runs them.
func simHeavy(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && os.Getenv("TOUCHMARK_HEAVY_TESTS") == "" {
		t.Skipf("a heavy test: it runs on Linux (set TOUCHMARK_HEAVY_TESTS=1 to run it on %s)", runtime.GOOS)
	}
}

// newSimWorld builds a world of flavor with a target per name, each opted
// in with "version: 1". The hub ships base (AGENTS.md, docs/guide.md,
// docs/old.md) and extra (docs/extra.md), version 1 of each; targets.yml
// gives base to every repository of acme.
func newSimWorld(t *testing.T, flavor fake.Flavor, names ...string) *simWorld {
	t.Helper()
	return newSimWorldOf(t, flavor, false, names...)
}

// newSimWorldOf is newSimWorld, a GitHub App's world with app (see
// simWorld.app).
func newSimWorldOf(t *testing.T, flavor fake.Flavor, app bool, names ...string) *simWorld {
	t.Helper()
	needDeliveryGit(t)
	clock := &simClock{now: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	host := simHosts[flavor]
	opts := []fake.Option{fake.WithFlavor(flavor), fake.WithClock(clock.Now)}
	if app {
		opts = append(opts, fake.WithPreflight(), fake.WithAPICommits())
	}
	p := fake.New(host, opts...)
	w := &simWorld{
		t: t, ctx: t.Context(), p: p, flavor: flavor, host: host, clock: clock, app: app,
		reader:   p.AddAccount("acme-read[bot]", platform.KindBot),
		writer:   p.AddAccount("acme-write[bot]", platform.KindBot),
		person:   p.AddAccount("jdoe", platform.KindUser),
		bot:      p.AddAccount("stale[bot]", platform.KindBot),
		hubID:    "acme-eng",
		files:    map[string]map[string]string{simBase: {}, simExtra: {}},
		manifest: &provenance.Manifest{Version: provenance.ManifestVersion},
		blobs:    map[string][]byte{},
		versions: map[string]int{},
		excluded: map[string]bool{},
		noForce:  map[string]bool{},
	}
	srv, err := p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatalf("ServeGit: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the git server: %v", err)
		}
	})
	for _, a := range []platform.Account{w.reader, w.writer, w.person, w.bot} {
		p.SetToken(a, simTokens[a.Login])
	}
	w.src = &snapshot.GitSource{Dir: t.TempDir(), Isolation: gitx.Isolation{Home: t.TempDir(), AllowHTTP: true}}
	w.gitConfig = filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(w.gitConfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"AGENTS.md", "docs/guide.md", "docs/old.md", "docs/extra.md"} {
		w.ship(path, 1)
	}
	w.hubChanged()
	for _, name := range names {
		w.addTarget(name)
	}
	w.ok()
	return w
}

// ok fails the test on a setup error of the fake.
func (w *simWorld) ok() {
	w.t.Helper()
	if err := w.p.Err(); err != nil {
		w.t.Fatalf("setup: %v", err)
	}
}

// addTarget adds acme/<name>, opted in, with a README, which the writer may
// write to.
func (w *simWorld) addTarget(name string) *simTarget {
	w.t.Helper()
	r := w.p.AddRepo(platform.Repo{Path: "acme/" + name})
	w.p.SetFile(r.ID, simOptIn, []byte("version: 1\n"), "")
	w.p.SetFile(r.ID, "README.md", []byte("# "+name+"\n"), "")
	w.p.GrantWrite(r.ID, w.writer)
	repo, _ := w.p.RepoByID(r.ID)
	tg := &simTarget{name: name, repo: repo}
	if w.app {
		w.setRules(tg)
	}
	w.ok()
	w.targets = append(w.targets, tg)
	return tg
}

// setRules gives target tg of an app world the rules and the writer's
// permissions the world's state says: signed commits required on every
// branch, force pushes refused on the sync branches with noForce, and the
// Workflows permission unless workflowsOff.
func (w *simWorld) setRules(tg *simTarget) {
	rules := []fake.Ruleset{{Branches: []string{"~ALL"}, RequiredSignatures: true}}
	if w.noForce[tg.name] {
		rules = append(rules, fake.Ruleset{Branches: []string{"touchmark/*"}, NonFastForward: true})
	}
	w.p.SetRulesets(tg.repo.ID, rules...)
	w.p.Grant(tg.repo.ID, w.writer, platform.Perms{Contents: true, PRs: true, Workflows: !w.workflowsOff})
}

// target returns the target named name.
func (w *simWorld) target(name string) *simTarget {
	w.t.Helper()
	for _, tg := range w.targets {
		if tg.name == name {
			return tg
		}
	}
	w.t.Fatalf("no target %s", name)
	return nil
}

// ship makes the hub ship version n of path now (in its pack) and records
// it in the manifest.
func (w *simWorld) ship(path string, n int) {
	content := simContent(path, n)
	pack := simPacks[path]
	w.files[pack][path] = content
	id := oid(content)
	w.blobs[id] = []byte(content)
	w.manifest.Add(path, pack, provenance.Version{OID: id, Size: int64(len(content))})
	w.versions[path] = max(w.versions[path], n)
}

// retire makes the hub stop shipping path.
func (w *simWorld) retire(path string) {
	delete(w.files[simPacks[path]], path)
}

// hubChanged makes a new hub commit, dated now.
func (w *simWorld) hubChanged() {
	w.hubN++
	sum := sha1.Sum([]byte(fmt.Sprintf("hub commit %d", w.hubN)))
	w.hubCommit = hex.EncodeToString(sum[:])
	w.hubTime = w.clock.Now()
	w.manifest.HubCommit = w.hubCommit
}

// branches are the sync branch of the hub's id and those of its former
// ids (the aliases), the sync branch first.
func (w *simWorld) branches() []string {
	out := []string{"touchmark/" + w.hubID}
	for _, a := range w.aliases {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// renameHub changes the hub's id; the old sync branch becomes an alias.
func (w *simWorld) renameHub(id string) {
	w.aliases = append(w.aliases, "touchmark/"+w.hubID)
	w.hubID = id
	w.hubChanged()
}

// hubYML is hub.yml now.
func (w *simWorld) hubYML() string {
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nid: %s\n", w.hubID)
	if len(w.aliases) > 0 {
		fmt.Fprintf(&b, "branch_aliases: [%s]\n", strings.Join(w.aliases, ", "))
	}
	fmt.Fprintf(&b, "providers:\n  - id: gh\n    type: %s\n", simTypes[w.flavor])
	switch w.flavor {
	case fake.GitHub, fake.Bitbucket:
	case fake.AzureDevOps:
		// The organization is the provider: acme/<name> are the project
		// acme's repositories, and the org entry names the organization.
		fmt.Fprintf(&b, "    url: https://%s/acme\n", w.host)
	default:
		fmt.Fprintf(&b, "    url: https://%s\n", w.host)
	}
	b.WriteString("    writer: acme-write[bot]\n")
	return b.String()
}

// targetsYML is targets.yml now.
func (w *simWorld) targetsYML() string {
	var b strings.Builder
	b.WriteString("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\n")
	if len(w.excluded) > 0 {
		b.WriteString("exclude:\n")
		for _, name := range slices.Sorted(maps.Keys(w.excluded)) {
			fmt.Fprintf(&b, "  - acme/%s\n", name)
		}
	}
	return b.String()
}

// current is what the packs ship now, as provenance.Current.
func (w *simWorld) current() provenance.Current {
	cur := provenance.Current{}
	for pack, files := range w.files {
		cur[pack] = map[string]provenance.File{}
		for path, content := range files {
			cur[pack][path] = provenance.File{Pack: pack, Path: path, OID: oid(content), Size: int64(len(content)), Mode: "100644"}
		}
	}
	return cur
}

// hubBlob serves the hub's blobs.
func (w *simWorld) hubBlob(id string) (io.ReadCloser, error) {
	b, ok := w.blobs[id]
	if !ok {
		return nil, errors.New("no hub blob " + id)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// deps returns the dependencies of a run in mode: the reader in ModePlan
// (with a writer that fails the test when called), the writer otherwise.
func (w *simWorld) deps(mode Mode) Deps {
	w.t.Helper()
	hub, _, err := config.ParseHub([]byte(w.hubYML()))
	if err != nil {
		w.t.Fatalf("hub.yml: %v\n%s", err, w.hubYML())
	}
	targets, _, err := config.ParseTargets([]byte(w.targetsYML()))
	if err != nil {
		w.t.Fatalf("targets.yml: %v", err)
	}
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil {
		w.t.Fatal(err)
	}
	prov := Provider{Config: rps[0]}
	if mode == ModePlan {
		prov.Reader, prov.Writer = w.p.Reader(w.reader), forbiddenWriter{t: w.t}
	} else {
		prov.Writer = w.p.Writer(w.writer)
	}
	m := &provenance.Manifest{Version: w.manifest.Version, HubCommit: w.hubCommit, Paths: map[string]map[string][]provenance.Version{}}
	for path, byPack := range w.manifest.Paths {
		for pack, vs := range byPack {
			for _, v := range vs {
				m.Add(path, pack, v)
			}
		}
	}
	return Deps{
		Hub:         hub,
		Targets:     targets,
		Manifest:    m,
		Current:     w.current(),
		Known:       map[string]bool{simBase: true, simExtra: true},
		HubCommit:   w.hubCommit,
		HubContext:  hubch.Context{CI: hubch.Local},
		Fingerprint: hubFP,
		Providers:   []Provider{prov},
		Snapshots:   w.src,
		Engine:      "test",
		Now:         w.clock.Now,
		sleep:       instantSleep,
		Write: WriteDeps{
			Operations:    w.ops,
			HubBlobs:      w.hubBlob,
			HubCommitTime: w.hubTime,
			HubURL:        hubURL,
			CanWorkflows:  map[string]bool{"gh": !w.workflowsOff},
		},
	}
}

// run runs the world's deps in mode, as edited by edit, and fails the test
// on an error of the run or a setup error of the fake.
func (w *simWorld) run(mode Mode, edit func(*Deps)) *report.Delivery {
	w.t.Helper()
	d := w.deps(mode)
	if edit != nil {
		edit(&d)
	}
	rep, err := Run(w.ctx, d, mode)
	if err != nil {
		w.t.Fatalf("Run(%v): %v", mode, err)
	}
	w.ok()
	checkReport(w.t, rep)
	return rep
}

// Human actions. They return an error when the action is not possible in
// the current state (the property test then skips it).

// push is a person's push of files ("" deletes a path) to branch of tg,
// creating the branch from the default branch when absent.
func (w *simWorld) push(tg *simTarget, branch string, files map[string]string) (string, error) {
	m := map[string][]byte{}
	for path, content := range files {
		if content == "" {
			m[path] = nil
		} else {
			m[path] = []byte(content)
		}
	}
	return w.p.PushFiles(tg.repo.ID, branch, m, w.person, time.Time{})
}

// edit is content of the team's own for path, new every time.
func (w *simWorld) edit(path string) string {
	w.edits++
	return simLocal(path, w.edits)
}

// setOptIn writes the opt-in file of tg on its default branch ("" removes
// it), as a person's push.
func (w *simWorld) setOptIn(tg *simTarget, content string) error {
	_, err := w.push(tg, tg.repo.DefaultBranch, map[string]string{simOptIn: content})
	return err
}

// optIn returns the opt-in file of tg now ("" when absent).
func (w *simWorld) optIn(tg *simTarget) string {
	tree, err := w.p.Snapshots().Snapshot(w.ctx, tg.repo, platform.Remote{}, "")
	if err != nil {
		return ""
	}
	e, ok := tree.Entries[simOptIn]
	if !ok {
		return ""
	}
	f, err := w.p.Reader(w.person).ReadFile(w.ctx, tg.repo, "", simOptIn, maxOptIn)
	if err != nil || f.OID != e.OID {
		return ""
	}
	return string(f.Content)
}

// tree returns the entries of tg's default branch.
func (w *simWorld) tree(tg *simTarget) map[string]snapshot.Entry {
	w.t.Helper()
	tree, err := w.p.Snapshots().Snapshot(w.ctx, tg.repo, platform.Remote{}, "")
	if err != nil {
		w.t.Fatalf("snapshot of %s: %v", tg.repo.Path, err)
	}
	return tree.Entries
}

// prs returns every pull request of tg, by number.
func (w *simWorld) prs(tg *simTarget) []platform.PR { return w.p.PRList(tg.repo.ID) }

// ownPRs returns the pull requests of tg that touchmark opened: the
// writer's, from the repository itself, on a sync branch, with a marker of
// this hub; in every state, by number.
func (w *simWorld) ownPRs(tg *simTarget) []platform.PR {
	var out []platform.PR
	for _, pr := range w.prs(tg) {
		if w.isOwn(pr) {
			out = append(out, pr)
		}
	}
	return out
}

// isOwn reports whether pr is touchmark's, by the rule of decide.Identity.
func (w *simWorld) isOwn(pr platform.PR) bool {
	if pr.Author.ID != w.writer.ID || pr.HeadRepoID != pr.RepoID || !slices.Contains(w.branches(), pr.Head) {
		return false
	}
	_, status := marker.Find(pr.Body, []string{hubFP})
	return status == marker.Found
}

// openOwn returns the open pull requests touchmark owns in tg.
func (w *simWorld) openOwn(tg *simTarget) []platform.PR {
	var out []platform.PR
	for _, pr := range w.ownPRs(tg) {
		if pr.State == platform.Open {
			out = append(out, pr)
		}
	}
	return out
}

// tick ticks control in the body of pull request n of tg, as a person;
// false when the body has no unticked line of it.
func (w *simWorld) tick(tg *simTarget, n int64, control string) bool {
	pr := w.p.PR(tg.repo.ID, n)
	line := prbody.ControlLine(control)
	if line == "" || !strings.Contains(pr.Body, line) {
		return false
	}
	ticked := strings.Replace(line, "- [ ]", "- [x]", 1)
	w.p.UpdatePR(tg.repo.ID, n, func(pr *platform.PR) { pr.Body = strings.Replace(pr.Body, line, ticked, 1) })
	w.ok()
	return true
}

// cherryPick puts touchmark's commits of open pull request n of tg onto a
// commit of the person's own, as `git cherry-pick` of our commit onto their
// work would: the person commits on a new branch from the default branch,
// the pull request's head is rebased onto it (keeping our commit's message
// and trailers), and the helper branch goes. It returns the person's
// commit.
func (w *simWorld) cherryPick(tg *simTarget, n int64) (string, error) {
	pr := w.p.PR(tg.repo.ID, n)
	if pr.State != platform.Open || pr.HeadRepoID != tg.repo.ID {
		return "", errors.New("the pull request is not open")
	}
	w.edits++
	helper := fmt.Sprintf("feature/work-%d", w.edits)
	mine, err := w.push(tg, helper, map[string]string{fmt.Sprintf("src/work-%d.txt", w.edits): simLocal("src/work.txt", w.edits)})
	if err != nil {
		return "", err
	}
	base := pr.Base
	w.p.UpdatePR(tg.repo.ID, n, func(pr *platform.PR) { pr.Base = helper })
	_, rerr := w.p.RebaseBranch(tg.repo.ID, n, w.person, time.Time{})
	w.p.UpdatePR(tg.repo.ID, n, func(pr *platform.PR) { pr.Base = base })
	w.p.DeleteBranch(tg.repo.ID, helper)
	w.ok()
	if rerr != nil {
		return "", rerr
	}
	return mine, nil
}

// mergeInto merges a branch of the person's own, not the default branch,
// into the head of open pull request n of tg, as `git merge feature` on the
// sync branch would: the person commits on a new branch from the default
// branch, the pull request's base is pointed at it for Update branch, which
// merges it, and the helper branch goes (its commit stays reachable through
// the merge). It returns the person's commit. Such a merge leaves the
// branch edited, never rewritten.
func (w *simWorld) mergeInto(tg *simTarget, n int64) (string, error) {
	pr := w.p.PR(tg.repo.ID, n)
	if pr.State != platform.Open || pr.HeadRepoID != tg.repo.ID {
		return "", errors.New("the pull request is not open")
	}
	w.edits++
	helper := fmt.Sprintf("feature/merge-%d", w.edits)
	mine, err := w.push(tg, helper, map[string]string{fmt.Sprintf("src/feature-%d.txt", w.edits): simLocal("src/feature.txt", w.edits)})
	if err != nil {
		return "", err
	}
	base := pr.Base
	w.p.UpdatePR(tg.repo.ID, n, func(pr *platform.PR) { pr.Base = helper })
	_, merr := w.p.UpdateBranch(tg.repo.ID, n, w.person, time.Time{})
	w.p.UpdatePR(tg.repo.ID, n, func(pr *platform.PR) { pr.Base = base })
	w.p.DeleteBranch(tg.repo.ID, helper)
	w.ok()
	if merr != nil {
		return "", merr
	}
	return mine, nil
}

// foreignPR opens a pull request of the person from the sync branch of tg
// (created with a commit of theirs when absent): someone else's pull
// request on touchmark's branch. It never stops the test, so that a
// mid-run event may call it from the goroutine of a run.
func (w *simWorld) foreignPR(tg *simTarget) (int64, error) {
	b := w.branches()[0]
	if w.p.Branch(tg.repo.ID, b) == "" {
		w.edits++
		if _, err := w.push(tg, b, map[string]string{"notes.md": simLocal("notes.md", w.edits)}); err != nil {
			return 0, err
		}
	}
	n := w.p.AddPR(tg.repo.ID, platform.PR{Head: b, Author: w.person, Title: "my own work on the sync branch", Body: "Mine."})
	return n, w.p.Err()
}

// otherHubPR opens a pull request by the writer's account from the sync
// branch of tg with the marker of another hub (same id, another
// fingerprint): not touchmark's (threat T6 of docs/project/threat-model.md).
func (w *simWorld) otherHubPR(tg *simTarget) (int64, error) {
	b := w.branches()[0]
	if w.p.Branch(tg.repo.ID, b) == "" {
		w.edits++
		if _, err := w.push(tg, b, map[string]string{"other.md": simLocal("other.md", w.edits)}); err != nil {
			return 0, err
		}
	}
	line, err := marker.Encode(marker.Marker{Key: staleKey, Data: marker.Data{V: marker.Version, Stream: "sync", Hub: w.hubID, FP: simOtherFP}})
	if err != nil {
		return 0, err
	}
	n := w.p.AddPR(tg.repo.ID, platform.PR{Head: b, Author: w.writer, Title: simTitle, Body: "Another hub.\n\n" + line})
	w.ok()
	return n, nil
}

// forkPR opens a pull request from a fork of tg with the sync branch's name
// and a copy of the marker of one of touchmark's pull requests (or a
// marker of this hub made up when there is none): never touchmark's
// (threat T9).
func (w *simWorld) forkPR(tg *simTarget) int64 {
	if w.fork.ID == "" {
		w.fork = w.p.AddRepo(platform.Repo{Path: "forks/" + tg.name, Fork: true})
		w.p.SetFile(w.fork.ID, "README.md", []byte("fork\n"), "")
	}
	body := ""
	for _, pr := range w.ownPRs(tg) {
		body = pr.Body
	}
	if body == "" {
		line, err := marker.Encode(marker.Marker{Key: staleKey, Data: marker.Data{V: marker.Version, Stream: "sync", Hub: w.hubID, FP: hubFP}})
		if err != nil {
			w.t.Fatal(err)
		}
		body = "Copied.\n\n" + line
	}
	n := w.p.AddPR(tg.repo.ID, platform.PR{Head: w.branches()[0], HeadRepoID: w.fork.ID, Author: w.person, Title: simTitle, Body: body})
	w.ok()
	return n
}

// reachable returns the commits reachable from the branches of tg's bare
// repository on the fake's side.
func (w *simWorld) reachable(tg *simTarget) (map[string]bool, error) {
	out, err := w.git(tg, "rev-list", "--branches")
	if err != nil {
		return nil, fmt.Errorf("rev-list of %s: %w", tg.repo.Path, err)
	}
	set := map[string]bool{}
	for _, line := range strings.Fields(out) {
		set[line] = true
	}
	return set, nil
}

// git runs git in the bare repository of tg on the fake's side, away from
// the machine's configuration.
func (w *simWorld) git(tg *simTarget, args ...string) (string, error) {
	g := gitx.New(w.p.GitDir(tg.repo.ID))
	g.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + w.gitConfig}
	out, err := g.Run(w.ctx, nil, args...)
	return string(out), err
}
