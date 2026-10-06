//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestDistribute is the life of one hub against the forge, through the
// command line in process (cli.Main), as a maintainer runs it:
//
//  1. plan with the reader: four targets would get a pull request, one
//     is not opted in, one is skipped for its sha256 object format;
//     nothing is written.
//  2. distribute with the writer: the pull requests appear with the
//     configured title, the body with the file table and the marker, the
//     label, and a commit authored by the writer with touchmark's
//     trailers. A person then opens a pull request from a fork of one
//     target, on a branch with the sync branch's name and a copy of the
//     body and marker.
//  3. A person renames one pull request and takes its label off, and a
//     new version of touchmark runs: distribute writes nothing (people
//     own the title and the labels, and the engine's version is not
//     compared), and plan reports the same outcomes as
//     distribute.
//  4. The person closes one pull request, the writer's account closes
//     another by hand: the first is a decline (acknowledged in the marker,
//     one comment); the second shows whether the driver reads the closer
//     from the timeline (a self-close proposes the content again) or not
//     (an unknown closer declines).
//  5. A pack changes: the open pull request of the target that gets the
//     pack is updated in place (branch and body; the person's title and
//     labels stay); the decline holds.
//  6. The hub changes its id, and its old sync branch becomes an alias:
//     the open pull requests stay on it, their bodies name the
//     new id, and the decline holds.
//  7. People use the sync branches: a person's own pull
//     request from one stops touchmark there (blocked:branch-in-use, no
//     write); a person's merge of a branch that is not the base into
//     another pauses it (blocked:edited: one write, the paused block with
//     the recreate control; the person's commits stay); a comment added to
//     an opt-in file leaves its decline in place.
//  8. Closes: the hub's files reach the default branch
//     of a target with autodetect_manual_merge on, and touchmark closes its
//     pull request as no-diff (the closed marker, a comment, the branch
//     deleted with a lease), which the forge does not record as a merge; a
//     target leaves targets.yml, and the sweep closes its pull request as
//     target-dropped (the closed marker, a comment; the branch, which
//     carries the person's merge, stays); a person fast-forwards the
//     default branch of a target to its sync branch, the forge records a
//     manual merge, and touchmark takes the content as delivered; a team
//     ignores a path of its declined pull request, the decline no longer
//     holds, and a new pull request brings the rest on the renamed hub's
//     branch.
//
// After every distribute a second one writes nothing; the fork's pull
// request is never touched; no token appears in any output, report, pull
// request, comment or commit message.
func TestDistribute(t *testing.T) {
	e := needLive(t)
	s := newScenario(t, e)

	// 1. plan: what distribute would do; nothing is written.
	rep := s.plan()
	s.want("plan", rep, map[string]string{
		"alpha": "opened:", "beta": "opened:", "gamma": "skipped:not-opted-in", "delta": "opened:", "epsilon": "skipped:sha256",
		"zeta": "opened:",
	})
	if n := s.countPRs(); n != 0 {
		t.Fatalf("plan wrote: %d pull requests exist", n)
	}

	// 2. The first distribute opens a pull request in every opted-in target
	// but the sha256 one.
	rep = s.settle("the first run", map[string]string{
		"alpha": "opened: #1", "beta": "opened: #1", "gamma": "skipped:not-opted-in", "delta": "opened: #1", "epsilon": "skipped:sha256",
		"zeta": "opened: #1",
	})
	s.wantOps("the first run", rep, map[string][]string{
		"alpha": {"push", "create-pr #1"}, "beta": {"push", "create-pr #1"},
		"delta": {"push", "create-pr #1"}, "zeta": {"push", "create-pr #1"},
	})
	for _, name := range []string{"alpha", "beta", "delta", "zeta"} {
		s.checkOpened(name, rep)
	}
	// A person's pull request from a fork, on the sync branch's name, with
	// a copy of the marker: from now on every settle checks it untouched.
	s.forkPR("delta")

	// 3. People own the title and the labels of a pull request once it
	// exists: a person renames delta's and takes its label off. A new version
	// of touchmark does not rewrite bodies either: the engine's version is
	// written with a write for another reason, never compared. The run writes
	// nothing; plan agrees with it.
	s.editPR("delta", 1, s.e.Person, map[string]any{"title": personTitle})
	s.unlabel("delta", 1, s.e.Person, "engineering-assets")
	engine := cli.Version
	t.Cleanup(func() { cli.Version = engine })
	cli.Version = nextEngine
	unchanged := s.stableSnapshot()
	quiet := s.distribute(0)
	s.want("a run with nothing to do", quiet, map[string]string{
		"alpha": "unchanged: #1", "beta": "unchanged: #1", "gamma": "skipped:not-opted-in", "delta": "unchanged: #1",
		"epsilon": "skipped:sha256", "zeta": "unchanged: #1",
	})
	if quiet.Engine != nextEngine {
		t.Errorf("the quiet run reports the engine %q, want %q", quiet.Engine, nextEngine)
	}
	if len(quiet.Ops) > 0 {
		t.Errorf("a person's title and label on delta #1, or a new engine, made a run write: %s", render(quiet))
	}
	if now := s.snapshot(); !sameMap(unchanged, now) {
		t.Errorf("a run after a person's edits changed the platform:\nbefore %v\nafter  %v", unchanged, now)
	}
	if m := s.marker("beta", 1); m.Data.Engine == nextEngine {
		t.Errorf("beta #1: the marker names the new engine %s: its body was rewritten", nextEngine)
	}
	if got, want := outcomes(s.plan()), outcomes(quiet); !sameMap(got, want) {
		t.Errorf("plan %v, distribute %v: plan must decide as distribute does", got, want)
	}

	// 4. A person declines alpha; the writer's account closes beta by hand.
	s.setState("alpha", 1, s.e.Person, "closed")
	s.setState("beta", 1, s.e.Writer, "closed")
	s.checkCloser("alpha", 1, s.e.Person)
	s.checkCloser("beta", 1, s.e.Writer)
	beta := "declined: #1"
	if s.caps.CloserKnown {
		// Closed by the writer: a self-close, which memory ignores, so the
		// content is proposed again.
		beta = "opened: #2"
	}
	rep = s.settle("alpha declined, beta closed by the writer", map[string]string{
		"alpha": "declined: #1", "beta": beta, "gamma": "skipped:not-opted-in", "delta": "unchanged: #1",
		"epsilon": "skipped:sha256", "zeta": "unchanged: #1",
	})
	// The decline is acknowledged once, in the marker and by a comment; a
	// self-closed pull request's content comes again from the branch that
	// still carries it.
	declineOps := map[string][]string{"alpha": {"edit-pr #1", "comment #1"}}
	if s.caps.CloserKnown {
		declineOps["beta"] = []string{"create-pr #2"}
	}
	s.wantOps("alpha declined, beta closed by the writer", rep, declineOps)
	// Whether the forge names who closed a pull request decides
	// Caps.CloserKnown: the outcome of beta shows what the driver made of the
	// timeline.
	finding(t, "closer", "the driver reports CloserKnown=%v; a pull request the writer's account closed by hand came out %q",
		s.caps.CloserKnown, s.outcome(rep, "beta"))
	if !s.caps.CloserKnown {
		t.Fatalf("the steps below expect beta #2 open: the driver reports CloserKnown=false")
	}
	s.checkDeclined("alpha", 1)
	// Platform facts (no head filter before Forgejo 16, pulls/{base}/{head}
	// unsorted, the closer from the timeline): what the driver lists for the
	// sync branch, now that alpha has a declined pull request, beta two on
	// one head (or one declined) and delta a fork's beside its own.
	for _, name := range []string{"alpha", "beta", "delta"} {
		s.checkDriverView(name)
	}

	// 5. The extra pack changes: delta's pull request is updated in place,
	// its body and branch by touchmark, its title and labels still as the
	// person left them.
	before := s.pr("delta", 1)
	s.packs["packs/extra/docs/extra.md"] = text("extra v2")
	s.commitHub("extra v2")
	rep = s.settle("the extra pack changed", map[string]string{
		"alpha": "declined: #1", "beta": "unchanged: #2", "gamma": "skipped:not-opted-in", "delta": "updated:content #1",
		"epsilon": "skipped:sha256", "zeta": "unchanged: #1",
	})
	// The pack reaches delta alone: the others' bodies stay as they are.
	s.wantOps("the extra pack changed", rep, map[string][]string{"delta": {"push", "edit-pr #1"}})
	after := s.pr("delta", 1)
	switch {
	case after.State != "open" || after.Head.SHA == before.Head.SHA:
		t.Errorf("delta #1 after the pack change: %s at %s, want open at a new commit (was %s)", after.State, after.Head.SHA, before.Head.SHA)
	case s.file("delta", s.branch, "docs/extra.md") != text("extra v2"):
		t.Errorf("delta's sync branch does not carry extra v2")
	case after.Body == before.Body || !strings.Contains(after.Body, "`docs/extra.md`"):
		t.Errorf("delta #1 after the pack change: the body is not rewritten:\n%s", after.Body)
	case after.Title != personTitle || len(after.Labels) != 0:
		t.Errorf("delta #1 after the pack change: title %q, labels %v; want the person's title and no label", after.Title, after.Labels)
	}
	s.checkCommit("delta", after)

	// 6. The hub changes its id: the old sync branch becomes
	// an alias. The open pull requests stay open on it and are maintained
	// there; memory goes by the fingerprint, so the decline holds. Their
	// bodies name the hub: each is rewritten once, and nothing else is
	// written.
	old := s.branch
	s.rename(s.id + "-renamed")
	s.commitHub("the hub is renamed")
	rep = s.settle("the hub renamed", map[string]string{
		"alpha": "declined: #1", "beta": "updated:body #2", "delta": "updated:body #1", "zeta": "updated:body #1",
	})
	s.wantOps("the hub renamed", rep, map[string][]string{
		"beta": {"edit-pr #2"}, "delta": {"edit-pr #1"}, "zeta": {"edit-pr #1"},
	})
	for _, open := range []struct {
		name string
		n    int64
	}{{"beta", 2}, {"delta", 1}, {"zeta", 1}} {
		pr := s.pr(open.name, open.n)
		m := s.marker(open.name, open.n)
		switch {
		case pr.State != "open" || pr.Head.Label != old:
			t.Errorf("%s #%d after the rename: %s on %s, want open on the old branch %s", open.name, open.n, pr.State, pr.Head.Label, old)
		case m.Data.Hub != s.id || m.Data.FP != s.fp:
			t.Errorf("%s #%d after the rename: the marker names hub %s (fp %s), want %s (fp %s)", open.name, open.n, m.Data.Hub, m.Data.FP, s.id, s.fp)
		case !strings.Contains(pr.Body, s.id):
			t.Errorf("%s #%d after the rename: the body does not name the hub %s:\n%s", open.name, open.n, s.id, pr.Body)
		}
	}
	for _, name := range []string{"alpha", "beta", "delta", "zeta"} {
		if head, ok := s.branchHead(name, s.branch); ok {
			t.Errorf("%s: the renamed hub pushed %s (%s); its open pull request stays on %s", name, s.branch, head, old)
		}
	}

	// 7. People use the sync branches:
	//   - a person opens a pull request of their own from beta's sync branch
	//     to a release branch: touchmark leaves the branch and its own pull
	//     request alone (blocked:branch-in-use) and writes nothing;
	//   - a person merges a feature branch, not the base, into delta's sync
	//     branch: rebuilding it would drop their commits, so touchmark pauses
	//     (blocked:edited) with one write, the paused block and the recreate
	//     control in the body, and does not push;
	//   - a person adds a comment to alpha's opt-in file: the parsed opt-in
	//     is the same, and the decline holds.
	s.change(s.e.Person, "beta", "release", "main", "a release branch", map[string]string{"RELEASE.md": "release notes\n"})
	inUse := s.openPR(s.e.Person, "beta", old, "release", "ship the shared files with the release")
	betaHead := s.mustBranchHead("beta", old)
	s.change(s.e.Person, "delta", "feature/notes", "main", "notes", map[string]string{"notes.md": "notes of the team\n"})
	merge := s.mergeInto(s.e.Person, "delta", old, "feature/notes", "notes.md")
	s.awaitPush("delta", 1, merge)
	optIn := "version: 1\n# the team reviews the shared files before they land\n"
	s.change(s.e.Person, "alpha", "main", "", "a comment in the opt-in file", map[string]string{config.DefaultOptIn: optIn})
	rep = s.settle("people use the sync branches", map[string]string{
		"alpha": "declined: #1", "beta": fmt.Sprintf("blocked:branch-in-use #%d", inUse.Number), "delta": "blocked:edited #1",
		"zeta": "unchanged: #1",
	})
	s.wantOps("people use the sync branches", rep, map[string][]string{"delta": {"edit-pr #1"}})
	switch pr := s.pr("delta", 1); {
	case pr.State != "open" || pr.Head.SHA != merge:
		t.Errorf("delta #1 after the person's merge: %s at %s, want open at the merge %s", pr.State, pr.Head.SHA, merge)
	case !strings.Contains(pr.Body, "### touchmark paused") || !strings.Contains(pr.Body, "<!-- touchmark:recreate -->"):
		t.Errorf("delta #1 is paused, but its body lacks the paused block or the recreate control:\n%s", pr.Body)
	}
	if head, _ := s.branchHead("beta", old); head != betaHead {
		t.Errorf("beta: %s moved from %s to %s while the person's #%d is open on it", old, betaHead, head, inUse.Number)
	}
	if pr := s.pr("beta", inUse.Number); pr.State != "open" || pr.Comments != 0 || pr.Body != inUse.Body || pr.Title != inUse.Title {
		t.Errorf("the person's pull request #%d on beta's sync branch was touched: %s, %d comments", inUse.Number, pr.State, pr.Comments)
	}

	// 8. Closes:
	//   - the person closes their pull request on beta's branch, turns on
	//     autodetect_manual_merge, and commits the hub's files to beta's
	//     default branch: D is empty, and touchmark closes #2 as no-diff (the
	//     closed marker and the state in one edit, a comment, the branch
	//     deleted with a lease). #2 is closed, not merged: touchmark never
	//     moves a branch onto the base, which the forge would record as a
	//     merge;
	//   - delta leaves targets.yml: the sweep closes #1 as target-dropped
	//     (the closed marker, a comment) and leaves the branch, which carries
	//     the person's merge;
	//   - the person fast-forwards zeta's default branch to its sync branch:
	//     with autodetect_manual_merge the forge records #1 as merged, and
	//     touchmark takes the content as delivered, without a write;
	//   - alpha's team ignores docs/guide.md: the parsed opt-in changed, so
	//     the decline no longer holds, and a new pull request brings
	//     AGENTS.md alone, on the renamed hub's branch.
	s.setState("beta", inUse.Number, s.e.Person, "closed")
	for _, name := range []string{"beta", "zeta"} {
		s.e.api(s.e.Admin).ok(t, http.MethodPatch, "/repos/"+s.repos[name], map[string]any{"autodetect_manual_merge": true}, nil)
	}
	s.change(s.e.Person, "beta", "main", "", "take the shared files", map[string]string{
		"AGENTS.md": s.packs["packs/base/AGENTS.md"], "docs/guide.md": s.packs["packs/base/docs/guide.md"],
	})
	s.e.pushBranch(t, s.e.Person, s.repos["zeta"], old, "main")
	if pr := s.waitPR("zeta", 1, func(pr apiPR) bool { return pr.State != "open" }); !pr.Merged {
		t.Fatalf("zeta #1 after the person fast-forwarded main to it: %s, merged %v; the forge should record a manual merge", pr.State, pr.Merged)
	}
	s.change(s.e.Person, "alpha", "main", "", "the team keeps its own guide", map[string]string{
		config.DefaultOptIn: optIn + "ignore:\n  - docs/guide.md\n",
	})
	s.dropDelta = true
	s.commitHub("delta leaves the hub")
	rep = s.settle("closes", map[string]string{
		"alpha": "opened: #2", "beta": "closed:no-diff #2", "delta": "closed:target-dropped #1", "zeta": "unchanged:",
	})
	s.wantOps("closes", rep, map[string][]string{
		"alpha": {"push", "create-pr #2"},
		"beta":  {"edit-pr #2", "comment #2", "delete-branch"},
		"delta": {"edit-pr #1", "comment #1"},
	})
	s.checkClosed("beta", 2, "no-diff")
	if head, ok := s.branchHead("beta", old); ok {
		t.Errorf("beta: %s, the branch of the pull request closed as no-diff, is still there at %s", old, head)
	}
	s.checkClosed("delta", 1, "target-dropped")
	if head, ok := s.branchHead("delta", old); !ok || head != merge {
		t.Errorf("delta: the sweep moved or deleted %s (now %q), which carries the person's merge %s", old, head, merge)
	}
	if pr := s.pr("zeta", 1); !pr.Merged || pr.Comments != 0 {
		t.Errorf("zeta #1 after the manual merge: merged %v, %d comments; want merged, and no comment", pr.Merged, pr.Comments)
	}
	fresh := s.pr("alpha", 2)
	switch {
	case fresh.State != "open" || fresh.Head.Label != s.branch || fresh.Head.RepoID != fresh.Base.RepoID:
		t.Errorf("alpha #2: %s from %s (repo %d), want open from %s in alpha itself", fresh.State, fresh.Head.Label, fresh.Head.RepoID, s.branch)
	case !strings.Contains(fresh.Body, "`AGENTS.md`") || strings.Contains(fresh.Body, "`docs/guide.md`"):
		t.Errorf("alpha #2 should bring AGENTS.md alone:\n%s", fresh.Body)
	case s.file("alpha", s.branch, "AGENTS.md") != s.packs["packs/base/AGENTS.md"]:
		t.Errorf("alpha's %s does not carry AGENTS.md", s.branch)
	case s.exists("alpha", s.branch, "docs/guide.md"):
		t.Errorf("alpha's %s carries docs/guide.md, which the team ignores", s.branch)
	}
	s.checkCommit("alpha", fresh)
	s.scanPlatform()
}

// personTitle is the title a person gives delta's pull request.
const personTitle = "chore: sync the shared docs (renamed by a person)"

// nextEngine is the version of touchmark from step 3 on: a release after the
// one that wrote the first bodies.
const nextEngine = "v0.99.0-e2e"

// scenario is one hub and its targets on the forge.
type scenario struct {
	t *testing.T
	e *liveEnv
	// id is the hub's id and branch its sync branch; both are new in every
	// run, so runs against a kept forge never meet. aliases are the sync
	// branches of the hub's former ids (branch_aliases).
	id, branch string
	aliases    []string
	// fp is the hub's fingerprint, random in every run.
	fp    string
	topic string
	// repos are the targets' paths by short name.
	repos map[string]string
	hub   string // the hub's directory
	packs map[string]string
	// dropDelta leaves delta's entry out of targets.yml.
	dropDelta bool
	// caps is what the driver's Probe reports.
	caps platform.Caps
	// writer is the writer's user as the admin sees it.
	writer apiUser
	// outputs are every output of the command line, for the token scan.
	outputs []string
	dir     string
	// fork is the person's pull request from a fork into forkTarget, as it
	// was opened; nil before forkPR.
	fork       *apiPR
	forkTarget string
}

// The targets: alpha, beta, gamma and zeta carry the hub's topic (gamma is
// not opted in), delta is named in targets.yml and also gets the extra
// pack, epsilon carries the topic and is opted in but has the sha256 object
// format, which distribute skips, noise is opted in but
// never selected.
var scenarioRepos = []struct {
	name    string
	topic   bool
	optedIn bool
	sha256  bool
}{
	{"alpha", true, true, false}, {"beta", true, true, false}, {"gamma", true, false, false}, {"delta", false, true, false},
	{"epsilon", true, true, true}, {"noise", false, true, false}, {"zeta", true, true, false},
}

func newScenario(t *testing.T, e *liveEnv) *scenario {
	t.Helper()
	suffix := randHex(t, 3)
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{
		t: t, e: e,
		id:    "e2e-" + suffix,
		topic: "touchmark-e2e-" + suffix,
		fp:    fmt.Sprintf("%s/%d", e.Host, n.Int64()+1),
		repos: map[string]string{},
		packs: map[string]string{
			"packs/base/AGENTS.md":      text("base AGENTS.md v1"),
			"packs/base/docs/guide.md":  text("base guide v1"),
			"packs/extra/docs/extra.md": text("extra v1"),
		},
		dir: t.TempDir(),
	}
	s.branch = "touchmark/" + s.id
	admin := e.api(e.Admin)
	admin.get(t, "/users/"+e.Writer.Login, &s.writer)
	for _, r := range scenarioRepos {
		p := e.Org + "/" + s.id + "-" + r.name
		s.repos[r.name] = p
		create := map[string]any{"name": s.id + "-" + r.name, "private": true, "auto_init": false, "default_branch": "main"}
		var files []map[string]any
		if r.sha256 {
			// The forge's own first commit, with a README.
			create["auto_init"], create["object_format_name"] = true, "sha256"
		} else {
			files = append(files, map[string]any{"operation": "create", "path": "README.md", "content": b64([]byte("# " + r.name + "\n"))})
		}
		admin.ok(t, http.MethodPost, "/orgs/"+e.Org+"/repos", create, nil)
		if r.optedIn {
			files = append(files, map[string]any{"operation": "create", "path": config.DefaultOptIn, "content": b64([]byte("version: 1\n"))})
		}
		if len(files) > 0 {
			admin.ok(t, http.MethodPost, "/repos/"+p+"/contents", map[string]any{"message": "init", "files": files}, nil)
		}
		if r.topic {
			admin.ok(t, http.MethodPut, "/repos/"+p+"/topics", map[string]any{"topics": []string{s.topic}}, nil)
		}
	}
	s.hub = t.TempDir()
	gitCmd(t, s.hub, nil, nil, "init", "-q", "-b", "master")
	s.commitHub("the hub")
	r, _ := newDrivers(t, e)
	if s.caps, err = r.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return s
}

// rename gives the hub the id id: its sync branch is named
// after it, and the old one becomes an alias, where the hub's open pull
// requests stay.
func (s *scenario) rename(id string) {
	s.aliases = append(s.aliases, s.branch)
	s.id, s.branch = id, "touchmark/"+id
}

// syncBranches are the hub's sync branch and its aliases.
func (s *scenario) syncBranches() []string {
	return append([]string{s.branch}, s.aliases...)
}

// commitHub writes the hub's configuration and packs and commits them.
func (s *scenario) commitHub(msg string) {
	s.t.Helper()
	hub := fmt.Sprintf("version: 1\nid: %s\n", s.id)
	if len(s.aliases) > 0 {
		hub += "branch_aliases: [" + strings.Join(s.aliases, ", ") + "]\n"
	}
	hub += fmt.Sprintf("providers:\n  - id: forge\n    type: %s\n    url: %s\n    writer: %s\n"+
		"pr:\n  title: \"chore: sync engineering assets\"\n", s.e.Flavor, s.e.URL, s.e.Writer.Login)
	targets := fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: %s\n    topics: [%s]\n", s.e.Org, s.topic)
	if !s.dropDelta {
		targets += fmt.Sprintf("  - repo: %s\n    packs: [extra]\n", s.repos["delta"])
	}
	files := map[string]string{config.HubFile: hub, config.TargetsFile: targets}
	for p, content := range s.packs {
		files[p] = content
	}
	for p, content := range files {
		full := filepath.Join(s.hub, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	gitCmd(s.t, s.hub, nil, nil, "add", "-A")
	gitCmd(s.t, s.hub, nil, nil, "commit", "-q", "-m", msg)
}

// hubHead returns the hub's HEAD commit.
func (s *scenario) hubHead() string {
	s.t.Helper()
	return gitCmd(s.t, s.hub, nil, nil, "rev-parse", "HEAD")
}

// text returns distinct content of a pack file whose first line is label,
// at least 64 bytes long: check refuses a smaller pack file, whose content
// cannot prove where it came from.
func text(label string) string {
	return label + "\nshared engineering asset, kept in sync by touchmark\nmore shared content\n"
}

// run runs the command line in process with vars in the environment and
// fails the test unless it exits with code. Every output is kept for the
// token scan.
func (s *scenario) run(code int, vars map[string]string, args ...string) string {
	s.t.Helper()
	for k, v := range vars {
		s.t.Setenv(k, v)
	}
	var stdout, stderr bytes.Buffer
	got := cli.Main(context.Background(), args, &stdout, &stderr)
	s.outputs = append(s.outputs, stdout.String(), stderr.String())
	if got != code {
		s.t.Fatalf("touchmark %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, code,
			s.e.Redact.Replace(stdout.String()), s.e.Redact.Replace(stderr.String()))
	}
	return stdout.String()
}

// plan runs plan with the reader's token.
func (s *scenario) plan() report.Delivery {
	s.t.Helper()
	out := s.run(0, map[string]string{"TOUCHMARK_FORGE_READ_TOKEN": s.e.Reader.Token},
		"plan", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json")
	return s.decode(out)
}

// distribute runs distribute with the writer's token, with a report file
// and a stream, both kept for the token scan.
func (s *scenario) distribute(code int) report.Delivery {
	s.t.Helper()
	reportFile, stream := filepath.Join(s.dir, "report.json"), filepath.Join(s.dir, "stream.jsonl")
	out := s.run(code, map[string]string{"TOUCHMARK_FORGE_WRITE_TOKEN": s.e.Writer.Token},
		"distribute", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json", "--report", reportFile, "--stream", stream)
	for _, name := range []string{reportFile, stream} {
		data, err := os.ReadFile(name)
		if err != nil {
			s.t.Fatal(err)
		}
		s.outputs = append(s.outputs, string(data))
	}
	rep := s.decode(out)
	for _, op := range rep.Ops {
		if op.Account != s.e.Writer.Login {
			s.t.Errorf("op %s %s by %q, want the writer %s", op.Target, op.Kind, op.Account, s.e.Writer.Login)
		}
	}
	return rep
}

func (s *scenario) decode(out string) report.Delivery {
	s.t.Helper()
	validate(s.t, "report", []byte(out), s.e.Redact.Replace)
	var rep report.Delivery
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		s.t.Fatalf("decode the report: %v\n%s", err, s.e.Redact.Replace(out))
	}
	return rep
}

// settle runs distribute, checks the outcomes of the targets in want
// ("outcome:reason #pr") and then that a second distribute writes nothing,
// by its report and on the platform. It returns the first run's report.
func (s *scenario) settle(what string, want map[string]string) report.Delivery {
	s.t.Helper()
	heads := s.openHeads()
	rep := s.distribute(0)
	s.want(what, rep, want)
	s.t.Logf("%s: %d writes", what, len(rep.Ops))
	s.awaitPushes(heads)
	before := s.stableSnapshot()
	again := s.distribute(0)
	if len(again.Ops) > 0 {
		var ops []string
		for _, op := range again.Ops {
			ops = append(ops, op.Target+" "+op.Kind)
		}
		s.t.Errorf("%s: a second distribute wrote %q (%s)", what, ops, render(again))
	}
	if after := s.snapshot(); !sameMap(before, after) {
		s.t.Errorf("%s: a second distribute changed the platform:\nbefore %v\nafter  %v", what, before, after)
	}
	s.checkFork()
	return rep
}

// want checks the outcomes of the targets named in want, by short name;
// noise must never be a target.
func (s *scenario) want(what string, rep report.Delivery, want map[string]string) {
	s.t.Helper()
	got := outcomes(rep)
	for name, w := range want {
		if g := got[s.repos[name]]; g != w {
			s.t.Errorf("%s: %s is %q, want %q (%s)", what, name, g, w, render(rep))
		}
	}
	if _, ok := got[s.repos["noise"]]; ok {
		s.t.Errorf("%s: %s is a target, but no entry of targets.yml selects it", what, s.repos["noise"])
	}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeFailed {
			s.t.Errorf("%s: %s failed: %q", what, tg.Path, tg.Warnings)
		}
	}
}

// outcome returns the outcome of target name in rep, as want spells it.
func (s *scenario) outcome(rep report.Delivery, name string) string {
	return outcomes(rep)[s.repos[name]]
}

// outcomes returns "outcome:reason #pr" by target path.
func outcomes(rep report.Delivery) map[string]string {
	out := map[string]string{}
	for _, tg := range rep.Targets {
		v := string(tg.Outcome) + ":" + tg.Reason
		if tg.PR != nil {
			v += fmt.Sprintf(" #%d", tg.PR.Number)
		}
		out[tg.Path] = v
	}
	return out
}

func render(rep report.Delivery) string {
	var parts []string
	for _, tg := range rep.Targets {
		parts = append(parts, fmt.Sprintf("%s %s:%s", tg.Path, tg.Outcome, tg.Reason))
	}
	return strings.Join(parts, ", ")
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// countPRs counts the pull requests of every target.
func (s *scenario) countPRs() int {
	s.t.Helper()
	n := 0
	for _, p := range s.repos {
		var prs []apiPR
		s.e.api(s.e.Admin).get(s.t, "/repos/"+p+"/pulls?state=all&limit=50", &prs)
		n += len(prs)
	}
	return n
}

// snapshot describes every pull request of the targets, their comments and
// the sync branches, to tell whether anything was written.
//
// The update time counts only for the writer's pull requests. A person's
// pull request is never written to (its state, head, text, labels and
// comments tell), and the forge may record the person's own push in its
// timeline seconds later, from a queue: the push of forkPR, an update of a
// branch that existed, adds a "pull_push" event to the pull request opened
// right after it, which moves its updated_at.
func (s *scenario) snapshot() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	admin := s.e.api(s.e.Admin)
	for name, p := range s.repos {
		var prs []apiPR
		admin.get(s.t, "/repos/"+p+"/pulls?state=all&limit=50", &prs)
		for _, pr := range prs {
			sum := sha256.Sum256([]byte(pr.Title + "\x00" + pr.Body))
			var labels []string
			for _, l := range pr.Labels {
				labels = append(labels, l.Name)
			}
			slices.Sort(labels)
			updated := "-"
			if pr.User.ID == s.writer.ID {
				updated = pr.Updated.UTC().Format(time.RFC3339)
			}
			out[fmt.Sprintf("%s#%d", name, pr.Number)] = fmt.Sprintf("%s merged %v head %s updated %s comments %d labels %v text %x",
				pr.State, pr.Merged, pr.Head.SHA, updated, pr.Comments, labels, sum[:6])
		}
		for _, b := range s.syncBranches() {
			if head, ok := s.branchHead(name, b); ok {
				out[name+" "+b] = head
			}
		}
	}
	return out
}

// openHeads returns the head commit of every open pull request of the
// targets, by "<target>#<number>".
func (s *scenario) openHeads() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	for name, p := range s.repos {
		var prs []apiPR
		s.e.api(s.e.Admin).get(s.t, "/repos/"+p+"/pulls?state=open&limit=50", &prs)
		for _, pr := range prs {
			out[fmt.Sprintf("%s#%d", name, pr.Number)] = pr.Head.SHA
		}
	}
	return out
}

// awaitPushes waits, at most a minute, until the forge has recorded the
// push of every open pull request whose head moved since heads (openHeads
// before a run): the "pull_push" event with the new head in its timeline.
// Gitea and Forgejo write that event from a queue, seconds after the push
// (seen 4 s late on Gitea 1.27.3 under load), and it changes the pull
// request's updated_at, which must not count as a write of the next run.
func (s *scenario) awaitPushes(heads map[string]string) {
	s.t.Helper()
	for key, head := range s.openHeads() {
		if old, ok := heads[key]; ok && old != head {
			name, n, _ := strings.Cut(key, "#")
			number, _ := strconv.ParseInt(n, 10, 64)
			s.awaitPush(name, number, head)
		}
	}
}

// awaitPush waits, at most a minute, until the timeline of pull request n
// of target name holds the "pull_push" event of head: the forge's record of
// a push to its branch, written from a queue (see awaitPushes).
func (s *scenario) awaitPush(name string, n int64, head string) {
	s.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var events []apiTimeline
		s.e.api(s.e.Admin).get(s.t, fmt.Sprintf("/repos/%s/issues/%d/timeline?limit=50", s.repos[name], n), &events)
		if slices.ContainsFunc(events, func(ev apiTimeline) bool {
			return ev.Type == "pull_push" && strings.Contains(ev.Body, head)
		}) {
			return
		}
		if time.Now().After(deadline) {
			s.t.Logf("%s #%d: no push event of %s in its timeline after a minute", name, n, head)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stableSnapshot waits until two snapshots a second apart agree, at most
// 20 s, and returns the last: the forges finish some work of a push in the
// background (the push comment in the timeline, which awaitPushes waits for,
// the mergeability check), which must not count as a write of the next run.
func (s *scenario) stableSnapshot() map[string]string {
	s.t.Helper()
	last := s.snapshot()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		next := s.snapshot()
		if sameMap(last, next) {
			return next
		}
		last = next
	}
	s.t.Logf("the platform kept changing for 20 s after a run")
	return last
}

// pr returns pull request n of target name, as the admin sees it.
func (s *scenario) pr(name string, n int64) apiPR {
	s.t.Helper()
	var pr apiPR
	s.e.api(s.e.Admin).get(s.t, fmt.Sprintf("/repos/%s/pulls/%d", s.repos[name], n), &pr)
	return pr
}

// file returns the content of path at ref in target name.
func (s *scenario) file(name, ref, path string) string {
	s.t.Helper()
	var c apiContent
	s.e.api(s.e.Admin).get(s.t, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", s.repos[name], path, url.QueryEscape(ref)), &c)
	return c.text(s.t)
}

// exists reports whether target name has path at ref.
func (s *scenario) exists(name, ref, path string) bool {
	s.t.Helper()
	resp := s.e.api(s.e.Admin).do(s.t, http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", s.repos[name], path, url.QueryEscape(ref)), nil)
	switch resp.Status {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	}
	s.t.Fatalf("GET %s of %s at %s: HTTP %d: %s", path, name, ref, resp.Status, s.e.snippet(resp.Body))
	return false
}

// branchHead returns the head of branch in target name; ok is false when
// the branch does not exist.
func (s *scenario) branchHead(name, branch string) (string, bool) {
	s.t.Helper()
	resp := s.e.api(s.e.Admin).do(s.t, http.MethodGet, "/repos/"+s.repos[name]+"/branches/"+branch, nil)
	switch resp.Status {
	case http.StatusOK:
		var b apiBranch
		decode(s.t, "branch", resp, &b)
		return b.Commit.ID, true
	case http.StatusNotFound:
		return "", false
	}
	s.t.Fatalf("GET branch %s of %s: HTTP %d: %s", branch, name, resp.Status, s.e.snippet(resp.Body))
	return "", false
}

// mustBranchHead is branchHead of a branch that must exist.
func (s *scenario) mustBranchHead(name, branch string) string {
	s.t.Helper()
	head, ok := s.branchHead(name, branch)
	if !ok {
		s.t.Fatalf("%s has no branch %s", name, branch)
	}
	return head
}

// marker returns the marker of pull request n of target name, which must be
// the hub's.
func (s *scenario) marker(name string, n int64) marker.Marker {
	s.t.Helper()
	m, status := marker.Find(s.pr(name, n).Body, []string{s.fp})
	if status != marker.Found {
		s.t.Fatalf("%s #%d: the hub's marker is %s", name, n, status)
	}
	return m
}

// change commits files (path → content) to branch of target name as as,
// through the contents API, as a person edits files in the web interface:
// a file the branch has is updated, else created. With from, the commit
// starts branch anew from the tip of from. It returns the commit.
func (s *scenario) change(as account, name, branch, from, message string, files map[string]string) string {
	s.t.Helper()
	at := branch
	body := map[string]any{"message": message, "branch": branch}
	if from != "" {
		at = from
		body["branch"], body["new_branch"] = from, branch
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var ops []map[string]any
	for _, p := range paths {
		op := map[string]any{"operation": "create", "path": p, "content": b64([]byte(files[p]))}
		resp := s.e.api(as).do(s.t, http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", s.repos[name], p, url.QueryEscape(at)), nil)
		if resp.Status == http.StatusOK {
			var c apiContent
			decode(s.t, "contents", resp, &c)
			op["operation"], op["sha"] = "update", c.SHA
		}
		ops = append(ops, op)
	}
	body["files"] = ops
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	s.e.api(as).ok(s.t, http.MethodPost, "/repos/"+s.repos[name]+"/contents", body, &out)
	return out.Commit.SHA
}

// openPR opens a pull request of as in target name, from its branch head to
// base, and returns it as the forge answered.
func (s *scenario) openPR(as account, name, head, base, title string) apiPR {
	s.t.Helper()
	var pr apiPR
	s.e.api(as).ok(s.t, http.MethodPost, "/repos/"+s.repos[name]+"/pulls", map[string]any{
		"head": head, "base": base, "title": title, "body": "My own pull request from " + head + ".",
	}, &pr)
	return pr
}

// mergeInto merges branch feature of target name into its branch as as,
// with git, as a person does: a merge commit whose first parent is the head
// of branch and whose second is the head of feature, with the files of
// both (feature adds the files named in added, which branch lacks). It
// pushes the merge and returns it.
func (s *scenario) mergeInto(as account, name, branch, feature string, added ...string) string {
	s.t.Helper()
	remote := s.e.remote(s.repos[name])
	auth := s.e.gitAuth(as)
	dir := s.t.TempDir()
	gitCmd(s.t, dir, nil, nil, "init", "-q")
	gitCmd(s.t, dir, nil, auth, "fetch", "-q", remote, "refs/heads/"+branch+":refs/heads/ours", "refs/heads/"+feature+":refs/heads/theirs")
	gitCmd(s.t, dir, nil, nil, "read-tree", "ours")
	for _, p := range added {
		blob := gitCmd(s.t, dir, nil, nil, "rev-parse", "theirs:"+p)
		gitCmd(s.t, dir, nil, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p)
	}
	tree := gitCmd(s.t, dir, nil, nil, "write-tree")
	merge := gitCmd(s.t, dir, nil, identity(as), "commit-tree", tree, "-p", "ours", "-p", "theirs",
		"-m", "Merge branch '"+feature+"' into "+branch)
	gitCmd(s.t, dir, nil, auth, "push", "-q", remote, merge+":refs/heads/"+branch)
	return merge
}

// identity is the environment that makes as the author and committer of a
// commit git makes.
func identity(as account) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + as.Login, "GIT_AUTHOR_EMAIL=" + as.Login + "@example.com",
		"GIT_COMMITTER_NAME=" + as.Login, "GIT_COMMITTER_EMAIL=" + as.Login + "@example.com",
	}
}

// waitPR re-reads pull request n of target name until done holds, for at
// most 30 s (the forge adjusts pull requests from a queue after a push),
// and returns it.
func (s *scenario) waitPR(name string, n int64, done func(apiPR) bool) apiPR {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		pr := s.pr(name, n)
		if done(pr) || time.Now().After(deadline) {
			return pr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// wantOps checks the writes of rep by target: exactly want[name] for the
// targets it names, in the order of the journal, and none for the others.
// A write reads "<kind>", with " #<n>" when it is to pull request n.
func (s *scenario) wantOps(what string, rep report.Delivery, want map[string][]string) {
	s.t.Helper()
	byTarget := map[string]string{}
	for name, p := range s.repos {
		byTarget["forge:"+p] = name
	}
	got := map[string][]string{}
	for _, op := range rep.Ops {
		name, ok := byTarget[op.Target]
		if !ok {
			name = op.Target
		}
		w := op.Kind
		if op.PR != 0 {
			w += fmt.Sprintf(" #%d", op.PR)
		}
		got[name] = append(got[name], w)
	}
	for name, ops := range got {
		if _, ok := want[name]; !ok {
			s.t.Errorf("%s: %s got the writes %q, want none", what, name, ops)
		}
	}
	for name, ops := range want {
		if !slices.Equal(got[name], ops) {
			s.t.Errorf("%s: %s got the writes %q, want %q", what, name, got[name], ops)
		}
	}
}

// checkClosed checks pull request n of target name, which touchmark closed
// for reason: closed and not merged, with no merge in its
// timeline, closed by the writer, the marker's closed naming touchmark and
// reason, and the writer's comment last.
func (s *scenario) checkClosed(name string, n int64, reason string) {
	s.t.Helper()
	pr := s.pr(name, n)
	m, status := marker.Find(pr.Body, []string{s.fp})
	switch {
	case pr.State != "closed" || pr.Merged || pr.MergedBy != nil:
		s.t.Errorf("%s #%d: %s, merged %v by %v; want closed without a merge", name, n, pr.State, pr.Merged, pr.MergedBy)
	case status != marker.Found || m.Data.Closed == nil || m.Data.Closed.By != "touchmark" || m.Data.Closed.Reason != reason:
		s.t.Errorf("%s #%d: marker %s, closed %+v; want closed by touchmark for %s", name, n, status, m.Data.Closed, reason)
	}
	if ev, ok := lastEvent(s.t, s.e, s.e.Admin, s.repos[name], n, "merge_pull"); ok {
		s.t.Errorf("%s #%d: its timeline records a merge by %v", name, n, ev.User)
	}
	if ev, ok := lastEvent(s.t, s.e, s.e.Admin, s.repos[name], n, "close"); !ok || ev.User == nil || ev.User.Login != s.e.Writer.Login {
		s.t.Errorf("%s #%d: the last close event is by %v, want the writer %s", name, n, ev.User, s.e.Writer.Login)
	}
	var comments []apiComment
	s.e.api(s.e.Admin).get(s.t, fmt.Sprintf("/repos/%s/issues/%d/comments", s.repos[name], n), &comments)
	if len(comments) == 0 || comments[len(comments)-1].User.Login != s.e.Writer.Login {
		s.t.Errorf("%s #%d: comments %v; want the writer's comment on the close last", name, n, comments)
	}
}

// checkOpened checks target name's only pull request against the report
// and against what touchmark promises: the sync branch of the target itself
// to the default branch, by the writer, with the title, the label, a body
// that lists the files and ends in our marker, and the pack files on the
// branch.
func (s *scenario) checkOpened(name string, rep report.Delivery) {
	s.t.Helper()
	var prs []apiPR
	s.e.api(s.e.Admin).get(s.t, "/repos/"+s.repos[name]+"/pulls?state=all&limit=50", &prs)
	if len(prs) != 1 {
		s.t.Fatalf("%s has %d pull requests, want 1", name, len(prs))
	}
	pr := prs[0]
	var tg report.DeliveryTarget
	for _, x := range rep.Targets {
		if x.Path == s.repos[name] {
			tg = x
		}
	}
	switch {
	case tg.PR == nil || tg.PR.Number != pr.Number:
		s.t.Errorf("%s: the report names %+v, the platform has #%d", name, tg.PR, pr.Number)
	case pr.State != "open" || pr.Draft:
		s.t.Errorf("%s #%d: state %s, draft %v; want open and ready", name, pr.Number, pr.State, pr.Draft)
	case pr.Head.Label != s.branch || pr.Head.RepoID != pr.Base.RepoID || pr.Base.Ref != "main":
		s.t.Errorf("%s #%d: %s (repo %d) → %s (repo %d), want %s → main in the target itself", name, pr.Number,
			pr.Head.Label, pr.Head.RepoID, pr.Base.Ref, pr.Base.RepoID, s.branch)
	case pr.User.Login != s.e.Writer.Login:
		s.t.Errorf("%s #%d: opened by %s, want the writer %s", name, pr.Number, pr.User.Login, s.e.Writer.Login)
	case pr.Title != "chore: sync engineering assets":
		s.t.Errorf("%s #%d: title %q", name, pr.Number, pr.Title)
	}
	if !slices.ContainsFunc(pr.Labels, func(l apiLabel) bool { return l.Name == "engineering-assets" }) {
		s.t.Errorf("%s #%d: labels %v, want engineering-assets", name, pr.Number, pr.Labels)
	}
	m, status := marker.Find(pr.Body, []string{s.fp})
	wantPacks := []string{"base"}
	files := []string{"AGENTS.md", "docs/guide.md"}
	if name == "delta" {
		wantPacks = []string{"base", "extra"}
		files = append(files, "docs/extra.md")
	}
	switch {
	case status != marker.Found:
		s.t.Errorf("%s #%d: marker %s in the body:\n%s", name, pr.Number, status, pr.Body)
	case m.Data.Hub != s.id || m.Data.FP != s.fp || m.Stream != "sync" || m.Data.ContentCommit != s.hubHead():
		s.t.Errorf("%s #%d: marker hub %s fp %s stream %s content %s", name, pr.Number, m.Data.Hub, m.Data.FP, m.Stream, m.Data.ContentCommit)
	case !slices.Equal(sorted(m.Data.Packs), wantPacks):
		s.t.Errorf("%s #%d: marker packs %v, want %v", name, pr.Number, m.Data.Packs, wantPacks)
	case !strings.HasPrefix(lastLine(pr.Body), "<!-- touchmark:v1 "):
		s.t.Errorf("%s #%d: the marker is not the last line of the body", name, pr.Number)
	}
	for _, f := range files {
		if !strings.Contains(pr.Body, "`"+f+"`") {
			s.t.Errorf("%s #%d: the body does not list %s:\n%s", name, pr.Number, f, pr.Body)
		}
		if got, want := s.file(name, s.branch, f), s.packs[packPath(f, name)]; got != want {
			s.t.Errorf("%s: %s on %s is %q, want %q", name, f, s.branch, got, want)
		}
	}
	s.checkCommit(name, pr)
}

// packPath returns the hub path of target file f.
func packPath(f, target string) string {
	if f == "docs/extra.md" {
		return "packs/extra/" + f
	}
	return "packs/base/" + f
}

// checkCommit checks the head commit of a pull request of touchmark's: one
// commit on the default branch's tip, authored and committed by the writer,
// whom the platform recognises by the email, with touchmark's trailers.
func (s *scenario) checkCommit(name string, pr apiPR) {
	s.t.Helper()
	var c apiCommit
	s.e.api(s.e.Admin).get(s.t, fmt.Sprintf("/repos/%s/git/commits/%s", s.repos[name], pr.Head.SHA), &c)
	var main apiBranch
	s.e.api(s.e.Admin).get(s.t, "/repos/"+s.repos[name]+"/branches/main", &main)
	msg := c.Commit.Message
	switch {
	case c.Commit.Author.Name != s.e.Writer.Login || c.Commit.Committer.Name != s.e.Writer.Login:
		s.t.Errorf("%s %s: author %q, committer %q; want the writer %s", name, c.SHA, c.Commit.Author.Name, c.Commit.Committer.Name, s.e.Writer.Login)
	case c.Commit.Author.Email == "" || c.Commit.Author.Email != c.Commit.Committer.Email:
		s.t.Errorf("%s %s: author email %q, committer email %q", name, c.SHA, c.Commit.Author.Email, c.Commit.Committer.Email)
	case c.Author == nil || c.Author.Login != s.e.Writer.Login:
		s.t.Errorf("%s %s: the platform attributes the commit to %v, want the writer's account (email %s)", name, c.SHA, c.Author, c.Commit.Author.Email)
	case len(c.Parents) != 1 || c.Parents[0].SHA != main.Commit.ID:
		s.t.Errorf("%s %s: parents %v, want the tip of main %s", name, c.SHA, c.Parents, main.Commit.ID)
	case !strings.Contains(msg, "\nTouchmark-Hub: "+s.id+"@"+s.fp+"\n") || !strings.Contains(msg, "\nTouchmark-Content: sha256:") ||
		!strings.Contains(msg, "\nTouchmark-Hub-Commit: "+s.hubHead()):
		s.t.Errorf("%s %s: message without touchmark's trailers:\n%s", name, c.SHA, msg)
	}
}

// setState closes or reopens pull request n of target name as as.
func (s *scenario) setState(name string, n int64, as account, state string) {
	s.t.Helper()
	s.editPR(name, n, as, map[string]any{"state": state})
}

// editPR changes fields of pull request n of target name as as, as a
// person does in the web interface.
func (s *scenario) editPR(name string, n int64, as account, fields map[string]any) {
	s.t.Helper()
	s.e.api(as).ok(s.t, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", s.repos[name], n), fields, nil)
}

// unlabel takes label off pull request n of target name as as.
func (s *scenario) unlabel(name string, n int64, as account, label string) {
	s.t.Helper()
	for _, l := range s.pr(name, n).Labels {
		if l.Name == label {
			s.e.api(as).ok(s.t, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%d", s.repos[name], n, l.ID), nil, nil)
			return
		}
	}
	s.t.Fatalf("%s #%d has no label %s", name, n, label)
}

// checkCloser verifies through the timeline, as the reader reads it, that
// the platform names who closed pull request n: the fact behind
// Caps.CloserKnown.
func (s *scenario) checkCloser(name string, n int64, by account) {
	s.t.Helper()
	ev, ok := lastEvent(s.t, s.e, s.e.Reader, s.repos[name], n, "close")
	switch {
	case !ok:
		s.t.Errorf("%s #%d: the timeline the reader reads has no close event", name, n)
	case ev.User == nil || ev.User.Login != by.Login:
		s.t.Errorf("%s #%d: the timeline's close event names %v, want %s", name, n, ev.User, by.Login)
	default:
		finding(s.t, "timeline-closer", "GET /repos/{o}/{r}/issues/%d/timeline shows the reader an event of type %q by %s when %s closes a pull request",
			n, ev.Type, ev.User.Login, by.Login)
	}
}

// lastEvent returns the last timeline event of type typ of pull request n
// of the repository at p, as as reads it.
func lastEvent(t testing.TB, e *liveEnv, as account, p string, n int64, typ string) (apiTimeline, bool) {
	t.Helper()
	var events []apiTimeline
	e.api(as).get(t, fmt.Sprintf("/repos/%s/issues/%d/timeline?limit=50", p, n), &events)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return events[i], true
		}
	}
	return apiTimeline{}, false
}

// checkDeclined checks what distribute writes when it first sees a decline:
// ack in the marker and one comment by the writer.
func (s *scenario) checkDeclined(name string, n int64) {
	s.t.Helper()
	pr := s.pr(name, n)
	m, status := marker.Find(pr.Body, []string{s.fp})
	if status != marker.Found || !m.Data.Ack {
		s.t.Errorf("%s #%d: marker %s, ack %v; want an acknowledged decline", name, n, status, m.Data.Ack)
	}
	var comments []apiComment
	s.e.api(s.e.Admin).get(s.t, fmt.Sprintf("/repos/%s/issues/%d/comments", s.repos[name], n), &comments)
	if len(comments) != 1 || comments[0].User.Login != s.e.Writer.Login {
		s.t.Errorf("%s #%d: %d comments (%v), want one by the writer", name, n, len(comments), comments)
	}
}

// checkDriverView compares what the reader driver's PRs lists for the sync
// branch of target name, with the writer as the author, with the platform's
// own answer filtered here: every pull request of the writer from the
// branch, in any state, and every open one from a branch of that name,
// forks included, newest first; the head repository of each, and for a
// closed one, the closer the timeline names when the driver says it knows
// closers.
func (s *scenario) checkDriverView(name string) {
	s.t.Helper()
	r, _ := newDrivers(s.t, s.e)
	var raw apiRepo
	admin := s.e.api(s.e.Admin)
	admin.get(s.t, "/repos/"+s.repos[name], &raw)
	writer := platform.Account{ID: strconv.FormatInt(s.writer.ID, 10), Login: s.writer.Login}
	got, err := r.PRs(context.Background(), raw.platform(s.e.Host), []string{s.branch}, []platform.Account{writer})
	if err != nil {
		s.t.Fatalf("the driver's PRs of %s: %v", name, err)
	}
	var all []apiPR
	admin.get(s.t, "/repos/"+s.repos[name]+"/pulls?state=all&limit=50", &all)
	want := map[int64]apiPR{}
	var wantNumbers []int64
	for _, pr := range all {
		if pr.Head.Label == s.branch && (pr.User.ID == s.writer.ID || pr.State == "open") {
			want[pr.Number] = pr
			wantNumbers = append(wantNumbers, pr.Number)
		}
	}
	slices.Sort(wantNumbers)
	slices.Reverse(wantNumbers)
	var gotNumbers []int64
	var seen []string
	for _, pr := range got {
		gotNumbers = append(gotNumbers, pr.Number)
		w, ok := want[pr.Number]
		if !ok {
			continue
		}
		fork := pr.HeadRepoID != pr.RepoID
		closer := ""
		if pr.ClosedBy != nil {
			closer = pr.ClosedBy.Login
		}
		seen = append(seen, fmt.Sprintf("#%d %s fork=%v closed-by=%q", pr.Number, pr.State, fork, closer))
		if fork != (w.Head.RepoID != w.Base.RepoID) || pr.Head != s.branch {
			s.t.Errorf("%s #%d: the driver says head %s, from a fork %v; the platform %s, from a fork %v",
				name, pr.Number, pr.Head, fork, w.Head.Label, w.Head.RepoID != w.Base.RepoID)
		}
		if pr.State == platform.Closed && s.caps.CloserKnown {
			ev, ok := lastEvent(s.t, s.e, s.e.Reader, s.repos[name], pr.Number, "close")
			if !ok || ev.User == nil || pr.ClosedBy == nil || pr.ClosedBy.Login != ev.User.Login {
				s.t.Errorf("%s #%d: the driver says closed by %v, the timeline %v", name, pr.Number, pr.ClosedBy, ev.User)
			}
		}
	}
	if !slices.Equal(gotNumbers, wantNumbers) {
		s.t.Errorf("%s: the driver lists %v for %s, the platform has %v", name, gotNumbers, s.branch, wantNumbers)
	}
	finding(s.t, "driver-prs", "%s: PRs(%s, writer) = %s", name, s.branch, strings.Join(seen, "; "))
}

// forkPR opens, as the person, a pull request into target name from a
// fork, on a branch named like the sync branch, with a copy of the body of
// touchmark's pull request and its marker (threat T9), and keeps it for
// checkFork.
func (s *scenario) forkPR(name string) {
	s.t.Helper()
	person := s.e.api(s.e.Person)
	var fork apiRepo
	person.ok(s.t, http.MethodPost, "/repos/"+s.repos[name]+"/forks", map[string]any{"name": s.id + "-" + name + "-fork"}, &fork)
	deadline := time.Now().Add(time.Minute)
	for fork.Empty {
		if time.Now().After(deadline) {
			s.t.Fatalf("the fork %s is still empty", fork.FullName)
		}
		time.Sleep(200 * time.Millisecond)
		person.get(s.t, "/repos/"+fork.FullName, &fork)
	}
	// A fork copies every branch, the sync branch too: the person commits on
	// top of that copy when there is one.
	change := map[string]any{
		"message": "my own sync",
		"files":   []map[string]any{{"operation": "create", "path": "mine.md", "content": b64([]byte("mine\n"))}},
	}
	if resp := person.do(s.t, http.MethodGet, "/repos/"+fork.FullName+"/branches/"+s.branch, nil); resp.Status == http.StatusOK {
		change["branch"] = s.branch
	} else {
		change["new_branch"] = s.branch
	}
	person.ok(s.t, http.MethodPost, "/repos/"+fork.FullName+"/contents", change, nil)
	own := s.pr(name, 1)
	var pr apiPR
	person.ok(s.t, http.MethodPost, "/repos/"+s.repos[name]+"/pulls", map[string]any{
		"head": s.e.Person.Login + ":" + s.branch, "base": "main", "title": own.Title, "body": own.Body,
	}, &pr)
	if pr.Head.RepoID == pr.Base.RepoID || pr.Head.Label != s.branch {
		s.t.Fatalf("the fork's pull request #%d: head %s in repo %d, base repo %d", pr.Number, pr.Head.Label, pr.Head.RepoID, pr.Base.RepoID)
	}
	s.fork, s.forkTarget = &pr, name
}

// checkFork checks that the fork's pull request is as it was opened: no
// edit, no comment, no close, no push: it is not touchmark's own.
func (s *scenario) checkFork() {
	s.t.Helper()
	if s.fork == nil {
		return
	}
	opened := *s.fork
	now := s.pr(s.forkTarget, opened.Number)
	if now.State != "open" || now.Body != opened.Body || now.Title != opened.Title || now.Head.SHA != opened.Head.SHA ||
		now.Comments != 0 || len(now.Labels) != len(opened.Labels) {
		s.t.Errorf("the fork's pull request #%d was touched: state %s, %d comments, labels %v, head %s (was %s), body changed %v",
			opened.Number, now.State, now.Comments, now.Labels, now.Head.SHA, opened.Head.SHA, now.Body != opened.Body)
	}
}

// scanPlatform checks that no token reached a pull request, a comment or a
// commit message of the targets, nor any output of the command line.
func (s *scenario) scanPlatform() {
	s.t.Helper()
	for i, out := range s.outputs {
		if s.e.Redact.Contains(out) {
			s.t.Errorf("output %d of the command line holds a token", i)
		}
	}
	admin := s.e.api(s.e.Admin)
	for name, p := range s.repos {
		var prs []apiPR
		admin.get(s.t, "/repos/"+p+"/pulls?state=all&limit=50", &prs)
		for _, pr := range prs {
			if s.e.Redact.Contains(pr.Title + "\n" + pr.Body) {
				s.t.Errorf("%s #%d holds a token", name, pr.Number)
			}
			var comments []apiComment
			admin.get(s.t, fmt.Sprintf("/repos/%s/issues/%d/comments", p, pr.Number), &comments)
			for _, c := range comments {
				if s.e.Redact.Contains(c.Body) {
					s.t.Errorf("a comment on %s #%d holds a token", name, pr.Number)
				}
			}
		}
		for _, b := range s.syncBranches() {
			var commits []apiCommit
			resp := admin.do(s.t, http.MethodGet, "/repos/"+p+"/commits?limit=50&sha="+url.QueryEscape(b), nil)
			if resp.Status == http.StatusOK {
				decode(s.t, "commits", resp, &commits)
				for _, c := range commits {
					if s.e.Redact.Contains(c.Commit.Message) {
						s.t.Errorf("commit %s of %s on %s holds a token", c.SHA, name, b)
					}
				}
			}
		}
	}
}

func sorted(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return out
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
