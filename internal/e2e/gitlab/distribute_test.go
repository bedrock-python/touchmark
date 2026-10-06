//go:build e2e

package gitlabe2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestDistribute is the life of one hub against GitLab, through the command
// line in process (cli.Main), as a maintainer runs it:
//
//  1. plan with the reader: three targets would get a merge request (one
//     in a subgroup), one is not opted in, one is skipped for its sha256
//     object format when the instance makes sha256 projects; nothing is
//     written.
//  2. distribute with the writer: the merge requests appear with the
//     configured title, the body with the file table and the marker, the
//     label, and a commit by the writer with touchmark's trailers. A person
//     then opens a merge request from a fork of one target, on a branch
//     with the sync branch's name and a copy of the body and marker.
//  3. A person renames one merge request, and a new version of touchmark
//     runs: nothing is written.
//  4. The person closes one merge request: a decline, acknowledged in the
//     marker and by one comment.
//  5. A pack changes: the open merge request of the target that gets the
//     pack is updated in place: the sync branch is rebuilt on the default
//     branch with a force push, and the merge request stays open.
//  6. The person deletes the sync branch of another target: GitLab closes
//     its merge request, and touchmark takes the close for a decline.
//  7. Another pack change is pushed, and the person deletes that target's
//     sync branch right after the push: a decline when GitLab names the
//     person as the closer; a known gap when it names the writer (see
//     branchDeletedAfterPush).
//
// After every distribute a second one writes nothing; the fork's merge
// request is never touched; no token appears in any output, report, merge
// request, note or commit message.
func TestDistribute(t *testing.T) {
	e := needLive(t)
	s := newScenario(t, e)

	// 1. plan: what distribute would do; nothing is written.
	rep := s.plan()
	s.want("plan", rep, s.expect(map[string]string{
		"alpha": "opened:", "beta": "opened:", "gamma": "skipped:not-opted-in", "delta": "opened:", "epsilon": "skipped:sha256",
	}))
	if n := s.countMRs(); n != 0 {
		t.Fatalf("plan wrote: %d merge requests exist", n)
	}

	// 2. The first distribute opens a merge request in every opted-in target
	// but the sha256 one.
	rep = s.settle("the first run", s.expect(map[string]string{
		"alpha": "opened: #1", "beta": "opened: #1", "gamma": "skipped:not-opted-in", "delta": "opened: #1", "epsilon": "skipped:sha256",
	}))
	s.wantOps("the first run", rep, map[string][]string{
		"alpha": {"push", "create-pr #1"}, "beta": {"push", "create-pr #1"}, "delta": {"push", "create-pr #1"},
	})
	for _, name := range []string{"alpha", "beta", "delta"} {
		s.checkOpened(name, rep)
	}
	s.forkMR("delta")

	// 3. People own the title of a merge request once it exists, and a new
	// engine does not rewrite bodies: nothing is written.
	s.editMR("delta", 1, s.e.Person, map[string]any{"title": personTitle})
	engine := cli.Version
	t.Cleanup(func() { cli.Version = engine })
	cli.Version = nextEngine
	unchanged := s.stableSnapshot()
	quiet := s.distribute(0)
	s.want("a run with nothing to do", quiet, s.expect(map[string]string{
		"alpha": "unchanged: #1", "beta": "unchanged: #1", "delta": "unchanged: #1",
	}))
	if len(quiet.Ops) > 0 {
		t.Errorf("a person's title on delta !1, or a new engine, made a run write: %s", render(quiet))
	}
	if now := s.snapshot(); !sameMap(unchanged, now) {
		t.Errorf("a run after a person's edit changed GitLab:\nbefore %v\nafter  %v", unchanged, now)
	}
	if got, want := outcomes(s.plan()), outcomes(quiet); !sameMap(got, want) {
		t.Errorf("plan %v, distribute %v: plan must decide as distribute does", got, want)
	}

	// 4. A person declines alpha's merge request.
	s.e.setState(t, s.e.Person, s.ids["alpha"], 1, "close")
	if mr := s.mr("alpha", 1); mr.ClosedBy == nil || mr.ClosedBy.ID != s.e.Person.ID {
		t.Errorf("alpha !1: closed_by %v, want the person", mr.ClosedBy)
	}
	rep = s.settle("alpha declined", s.expect(map[string]string{
		"alpha": "declined: #1", "beta": "unchanged: #1", "delta": "unchanged: #1",
	}))
	s.wantOps("alpha declined", rep, map[string][]string{"alpha": {"edit-pr #1", "comment #1"}})
	s.checkDeclined("alpha", 1)

	// 5. The extra pack changes: delta's merge request is updated in place.
	// touchmark rebuilds the sync branch on the default branch and
	// force-pushes it; GitLab keeps the merge request open.
	before := s.mr("delta", 1)
	s.packs["packs/extra/docs/extra.md"] = text("extra v2")
	s.commitHub("extra v2")
	rep = s.settle("the extra pack changed", s.expect(map[string]string{
		"alpha": "declined: #1", "beta": "unchanged: #1", "delta": "updated:content #1",
	}))
	s.wantOps("the extra pack changed", rep, map[string][]string{"delta": {"push", "edit-pr #1"}})
	// GitLab moves the merge request's head from the push's background job
	// (MergeRequests::RefreshService), seconds after the push.
	after := s.e.waitMR(t, s.ids["delta"], 1, time.Minute, func(m apiMR) bool { return m.SHA != before.SHA })
	head := s.commit("delta", after.SHA)
	switch {
	case after.State != "opened" || after.SHA == before.SHA:
		t.Errorf("delta !1 after the pack change: %s at %s, want opened at a new commit (was %s)", after.State, after.SHA, before.SHA)
	case slices.Contains(head.ParentIDs, before.SHA):
		t.Errorf("delta !1: the new head %s sits on the old one %s; touchmark rebuilds the branch from the base", after.SHA, before.SHA)
	case s.file("delta", s.branch, "docs/extra.md") != text("extra v2"):
		t.Errorf("delta's sync branch does not carry extra v2")
	case after.Description == before.Description || after.Title != personTitle:
		t.Errorf("delta !1 after the pack change: body rewritten %v, title %q", after.Description != before.Description, after.Title)
	}
	s.checkCommit("delta", after)
	finding(t, "force-push-distribute", "delta !%d stays %s after touchmark force-pushed %s over %s", after.IID, after.State, after.SHA, before.SHA)

	// 6. The person deletes beta's sync branch: GitLab closes its merge
	// request.
	s.e.api(s.e.Person).ok(t, http.MethodDelete, fmt.Sprintf("/projects/%d/repository/branches/%s", s.ids["beta"], url.PathEscape(s.branch)), nil, nil)
	closed := s.e.waitMR(t, s.ids["beta"], 1, time.Minute, func(m apiMR) bool { return m.State != "opened" })
	closer := "nobody"
	if closed.ClosedBy != nil {
		closer = closed.ClosedBy.Username
	}
	finding(t, "branch-deleted-distribute", "beta !1 after the person deleted %s: %s, closed_by %s", s.branch, closed.State, closer)
	if closed.State != "closed" {
		t.Fatalf("beta !1 is %s after its source branch was deleted, want closed", closed.State)
	}
	rep = s.settle("beta's branch deleted", s.expect(map[string]string{
		"alpha": "declined: #1", "beta": "declined: #1", "delta": "unchanged: #1",
	}))
	s.wantOps("beta's branch deleted", rep, map[string][]string{"beta": {"edit-pr #1", "comment #1"}})
	if _, ok := s.e.branchHead(t, s.ids["beta"], s.branch); ok {
		t.Errorf("beta: touchmark pushed %s again after its merge request was closed", s.branch)
	}

	// 7. The person deletes delta's sync branch right after touchmark's
	// push.
	t.Run("branch-deleted-after-push", func(t *testing.T) { s.branchDeletedAfterPush(t) })
	s.scanPlatform()
}

// branchDeletedAfterPush: a pack change makes touchmark force-push delta's
// sync branch, and the person deletes the branch at once, while GitLab
// still processes the push. GitLab closes the merge request and names as
// its closer the person, nobody (closed_by null, seen on 19.4.1), or, while
// the push's background job runs, the writer (seen on 17.11, 18.11 and 19.4
// in TestFacts, branch-delete-closer). A person or nobody as the closer is
// a decline. The writer as the closer is taken by decide.ClassifyClose
// (rule 4: closed by the writer is a self-close) for touchmark's own close:
// the decline is lost, and the content is proposed again. That is a known
// gap, an open question of the design (should a writer's close without
// closed.by=touchmark in the marker count as a decline?); the subtest then
// checks the known behavior and skips. Before this subtest, nothing
// exercised the window.
func (s *scenario) branchDeletedAfterPush(t *testing.T) {
	parent := s.t
	s.t = t
	defer func() { s.t = parent }()
	s.packs["packs/extra/docs/extra.md"] = text("extra v3")
	s.commitHub("extra v3")
	rep := s.distribute(0)
	s.want("extra v3", rep, s.expect(map[string]string{"alpha": "declined: #1", "beta": "declined: #1", "delta": "updated:content #1"}))
	s.e.api(s.e.Person).ok(t, http.MethodDelete, fmt.Sprintf("/projects/%d/repository/branches/%s", s.ids["delta"], url.PathEscape(s.branch)), nil, nil)
	closed := s.e.waitMR(t, s.ids["delta"], 1, time.Minute, func(m apiMR) bool { return m.State != "opened" })
	closer := "nobody"
	if closed.ClosedBy != nil {
		closer = closed.ClosedBy.Username
	}
	finding(t, "branch-delete-after-push", "delta !1 after the person deleted %s right after touchmark's push: %s, closed_by %s", s.branch, closed.State, closer)
	if closed.State != "closed" {
		t.Fatalf("delta !1 is %s after its source branch was deleted, want closed", closed.State)
	}
	switch {
	case closed.ClosedBy == nil || closed.ClosedBy.ID == s.e.Person.ID:
		// The person, or nobody (seen on 19.4.1 in this window): an unknown
		// closer is a decline too.
		rep = s.settle("delta's branch deleted after a push", s.expect(map[string]string{
			"alpha": "declined: #1", "beta": "declined: #1", "delta": "declined: #1",
		}))
		s.wantOps("delta's branch deleted after a push", rep, map[string][]string{"delta": {"edit-pr #1", "comment #1"}})
		if _, ok := s.e.branchHead(t, s.ids["delta"], s.branch); ok {
			t.Errorf("delta: touchmark pushed %s again after its merge request was closed", s.branch)
		}
	case closed.ClosedBy != nil && closed.ClosedBy.ID == s.e.Writer.ID:
		rep = s.distribute(0)
		got := outcomes(rep)[s.repos["delta"]]
		if !strings.HasPrefix(got, "opened:") {
			t.Fatalf("delta after GitLab named the writer as the closer: %q; the known behavior is opened (a self-close): update this test and the report", got)
		}
		t.Skipf("known gap: GitLab named the writer as the closer of delta !1, whose branch the person deleted while "+
			"touchmark's push was being processed; decide.ClassifyClose takes it for touchmark's own close, the decline is lost and the content "+
			"is proposed again (%s)", got)
	default:
		t.Errorf("delta !1 closed by %s, want the person, nobody or the writer", closer)
	}
}

// personTitle is the title a person gives delta's merge request.
const personTitle = "chore: sync the shared docs (renamed by a person)"

// nextEngine is the version of touchmark from step 3 on.
const nextEngine = "v0.99.0-e2e"

// scenario is one hub and its targets on GitLab.
type scenario struct {
	t *testing.T
	e *liveEnv
	// id is the hub's id and branch its sync branch; both are new in every
	// run, so runs against a kept instance never meet.
	id, branch string
	aliases    []string
	fp         string
	topic      string
	// provider is the id of the hub's gitlab provider: "gl", or "gitlab" as
	// migrate names it.
	provider string
	// repos are the targets' paths and ids by short name.
	repos map[string]string
	ids   map[string]int64
	// sha256 is set when the instance made epsilon a sha256 project.
	sha256 bool
	hub    string
	packs  map[string]string
	// known are known_authors of the provider; ops the content of
	// .touchmark/operations.yml ("" for none).
	known []string
	ops   string
	// outputs are every output of the command line, for the token scan.
	outputs []string
	dir     string
	// fork is the person's merge request from a fork into forkTarget, as it
	// was opened; nil before forkMR.
	fork       *apiMR
	forkTarget string
}

// The targets: alpha and gamma in the seeded group, beta in its subgroup,
// all three with the hub's topic (gamma is not opted in); delta named in
// targets.yml, with the extra pack; epsilon with the topic and opted in,
// of the sha256 object format when the instance makes one (skipped);
// noise opted in but never selected.
var scenarioRepos = []struct {
	name     string
	subgroup bool
	topic    bool
	optedIn  bool
	sha256   bool
}{
	{"alpha", false, true, true, false}, {"beta", true, true, true, false}, {"gamma", false, true, false, false},
	{"delta", false, false, true, false}, {"epsilon", false, true, true, true}, {"noise", false, false, true, false},
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
		id:       "e2e-" + suffix,
		topic:    "touchmark-e2e-" + suffix,
		fp:       fmt.Sprintf("%s/%d", e.Host, n.Int64()+1),
		provider: "gl",
		repos:    map[string]string{}, ids: map[string]int64{},
		packs: map[string]string{
			"packs/base/AGENTS.md":      text("base AGENTS.md v1"),
			"packs/base/docs/guide.md":  text("base guide v1"),
			"packs/extra/docs/extra.md": text("extra v1"),
		},
		dir: t.TempDir(),
	}
	s.branch = "touchmark/" + s.id
	root := e.api(e.Root)
	namespaces := map[bool]int64{}
	for sub, p := range map[bool]string{false: e.Group, true: e.Subgroup} {
		var g struct {
			ID int64 `json:"id"`
		}
		root.get(t, "/groups/"+pid(p), &g)
		namespaces[sub] = g.ID
	}
	for _, r := range scenarioRepos {
		name := s.id + "-" + r.name
		var p apiProject
		if r.sha256 {
			var how string
			p, how = e.sha256Project(t, namespaces[r.subgroup], name)
			s.sha256 = p.RepositoryObjectFormat == "sha256"
			finding(t, "scenario-sha256", "epsilon is a sha256 project: %v (%s)", s.sha256, how)
			if !s.sha256 {
				// No sha256 target on this instance: epsilon is left out.
				continue
			}
		} else {
			p = e.createProject(t, map[string]any{
				"name": name, "path": name, "namespace_id": namespaces[r.subgroup], "visibility": "private",
				"initialize_with_readme": true, "default_branch": "main",
			})
		}
		e.waitAccess(t, p.ID, e.Reader, e.Writer, e.Person)
		s.repos[r.name], s.ids[r.name] = p.PathWithNamespace, p.ID
		if r.optedIn {
			e.commitFiles(t, e.Root, p.ID, "main", "", fileList(map[string]string{config.DefaultOptIn: "version: 1\n"}), "opt in")
		}
		if r.topic {
			root.ok(t, http.MethodPut, fmt.Sprintf("/projects/%d", p.ID), map[string]any{"topics": []string{s.topic}}, nil)
		}
	}
	s.hub = t.TempDir()
	gitCmd(t, s.hub, nil, nil, "init", "-q", "-b", "master")
	s.commitHub("the hub")
	return s
}

// expect adapts want to the instance: epsilon is skipped:sha256 where the
// instance made it a sha256 project, and is no target at all otherwise.
func (s *scenario) expect(want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		out[k] = v
	}
	if _, ok := out["epsilon"]; !ok {
		if s.sha256 {
			out["epsilon"] = "skipped:sha256"
		}
		return out
	}
	if !s.sha256 {
		delete(out, "epsilon")
	}
	return out
}

// fileList turns path → content into files, sorted by path.
func fileList(files map[string]string) []conformance.File {
	var out []conformance.File
	for p, c := range files {
		out = append(out, conformance.File{Path: p, Content: []byte(c)})
	}
	slices.SortFunc(out, func(a, b conformance.File) int { return strings.Compare(a.Path, b.Path) })
	return out
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
	hub += fmt.Sprintf("providers:\n  - id: gl\n    type: gitlab\n    url: %s\n    writer: %s\n", s.e.URL, s.e.Writer.Login)
	if len(s.known) > 0 {
		hub += "    known_authors: [" + strings.Join(s.known, ", ") + "]\n"
	}
	hub += "pr:\n  title: \"chore: sync engineering assets\"\n"
	targets := fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - group: %s\n    subgroups: true\n    topics: [%s]\n",
		s.e.Group, s.topic)
	if s.repos["delta"] != "" {
		targets += fmt.Sprintf("  - repo: %s\n    packs: [extra]\n", s.repos["delta"])
	}
	files := map[string]string{config.HubFile: hub, config.TargetsFile: targets}
	for p, content := range s.packs {
		files[p] = content
	}
	opsPath := filepath.Join(s.hub, filepath.FromSlash(config.OperationsFile))
	if s.ops != "" {
		files[config.OperationsFile] = s.ops
	} else if err := os.Remove(opsPath); err != nil && !os.IsNotExist(err) {
		s.t.Fatal(err)
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
// at least 64 bytes long: check refuses a smaller pack file.
func text(label string) string {
	return label + "\nshared engineering asset, kept in sync by touchmark\nmore shared content\n"
}

// run runs the command line in process with vars in the environment and
// fails the test unless it exits with code. Every output is kept for the
// token scan.
func (s *scenario) run(code int, vars map[string]string, args ...string) string {
	s.t.Helper()
	out, errOut, got := s.try(vars, args...)
	if got != code {
		s.t.Fatalf("touchmark %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, code,
			s.e.Redact.Replace(out), s.e.Redact.Replace(errOut))
	}
	return out
}

// try runs the command line in process and returns its outputs and exit
// code.
func (s *scenario) try(vars map[string]string, args ...string) (string, string, int) {
	s.t.Helper()
	for k, v := range vars {
		s.t.Setenv(k, v)
	}
	var stdout, stderr bytes.Buffer
	got := cli.Main(context.Background(), args, &stdout, &stderr)
	s.outputs = append(s.outputs, stdout.String(), stderr.String())
	return stdout.String(), stderr.String(), got
}

// envVar is the name of the provider's variable suffix:
// TOUCHMARK_<ID>_<suffix>.
func (s *scenario) envVar(suffix string) string {
	return "TOUCHMARK_" + strings.ToUpper(strings.ReplaceAll(s.provider, "-", "_")) + "_" + suffix
}

// plan runs plan with the reader's token.
func (s *scenario) plan() report.Delivery {
	s.t.Helper()
	out := s.run(0, map[string]string{s.envVar("READ_TOKEN"): s.e.Reader.Token},
		"plan", "--hub", s.hub, "--hub-fp", s.fp, "--format", "json")
	return s.decode(out)
}

// distribute runs distribute with the writer's token, with a report file
// and a stream, both kept for the token scan.
func (s *scenario) distribute(code int) report.Delivery {
	s.t.Helper()
	reportFile, stream := filepath.Join(s.dir, "report.json"), filepath.Join(s.dir, "stream.jsonl")
	out := s.run(code, map[string]string{s.envVar("WRITE_TOKEN"): s.e.Writer.Token},
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
// ("outcome:reason #mr") and then that a second distribute writes nothing,
// by its report and on GitLab. It returns the first run's report.
func (s *scenario) settle(what string, want map[string]string) report.Delivery {
	s.t.Helper()
	rep := s.distribute(0)
	s.want(what, rep, want)
	s.t.Logf("%s: %d writes", what, len(rep.Ops))
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
		s.t.Errorf("%s: a second distribute changed GitLab:\nbefore %v\nafter  %v", what, before, after)
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
	if p, ok := s.repos["noise"]; ok {
		if _, ok := got[p]; ok {
			s.t.Errorf("%s: %s is a target, but no entry of targets.yml selects it", what, p)
		}
	}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeFailed {
			s.t.Errorf("%s: %s failed: %q", what, tg.Path, tg.Warnings)
		}
	}
}

// outcomes returns "outcome:reason #mr" by target path.
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

// countMRs counts the merge requests of every target.
func (s *scenario) countMRs() int {
	s.t.Helper()
	n := 0
	for _, id := range s.ids {
		n += len(s.e.mrs(s.t, id, "all"))
	}
	return n
}

// snapshot describes every merge request of the targets, the notes people
// and bots wrote on them and the sync branches, to tell whether anything
// was written. updated_at is left out: GitLab moves it in the background
// after a push (the merge status check, the system note of the push), which
// is no write of touchmark's; every write of touchmark's shows in the
// fields kept.
func (s *scenario) snapshot() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	for name, id := range s.ids {
		for _, mr := range s.e.mrs(s.t, id, "all") {
			sum := sha256.Sum256([]byte(mr.Title + "\x00" + mr.Description))
			labels := slices.Clone(mr.Labels)
			slices.Sort(labels)
			out[fmt.Sprintf("%s!%d", name, mr.IID)] = fmt.Sprintf("%s draft %v head %s notes %d labels %v text %x",
				mr.State, mr.Draft, mr.SHA, len(s.e.notes(s.t, id, mr.IID)), labels, sum[:6])
		}
		for _, b := range s.syncBranches() {
			if head, ok := s.e.branchHead(s.t, id, b); ok {
				out[name+" "+b] = head
			}
		}
	}
	return out
}

// stableSnapshot waits until two snapshots a second apart agree and every
// open merge request from a sync branch of its own project shows the head
// of that branch, at most a minute, and returns the last: GitLab finishes
// some work of a push in the background (MergeRequests::RefreshService
// from Sidekiq, up to 10 s late on 19.4 under load), which must not count
// as a write of the next run. Two equal snapshots alone may both predate
// that work.
func (s *scenario) stableSnapshot() map[string]string {
	s.t.Helper()
	last := s.snapshot()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		next := s.snapshot()
		if sameMap(last, next) && s.headsSettled() {
			return next
		}
		last = next
	}
	s.t.Logf("GitLab kept changing for a minute after a run")
	return last
}

// headsSettled reports whether every open merge request of the targets
// from one of the sync branches of its own project shows that branch's
// head: GitLab has moved it after the last push.
func (s *scenario) headsSettled() bool {
	s.t.Helper()
	for _, id := range s.ids {
		for _, mr := range s.e.mrs(s.t, id, "opened") {
			if mr.SourceProjectID != mr.TargetProjectID || !slices.Contains(s.syncBranches(), mr.SourceBranch) {
				continue
			}
			if head, ok := s.e.branchHead(s.t, id, mr.SourceBranch); ok && head != mr.SHA {
				return false
			}
		}
	}
	return true
}

// mr returns merge request n of target name, as root sees it.
func (s *scenario) mr(name string, n int64) apiMR {
	s.t.Helper()
	return s.e.mr(s.t, s.ids[name], n)
}

// commit returns a commit of target name.
func (s *scenario) commit(name, sha string) apiCommit {
	s.t.Helper()
	var c apiCommit
	s.e.api(s.e.Root).get(s.t, fmt.Sprintf("/projects/%d/repository/commits/%s", s.ids[name], sha), &c)
	return c
}

// file returns the content of path at ref in target name.
func (s *scenario) file(name, ref, p string) string {
	s.t.Helper()
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	s.e.api(s.e.Root).get(s.t, fmt.Sprintf("/projects/%d/repository/files/%s?ref=%s", s.ids[name], url.PathEscape(p), url.QueryEscape(ref)), &f)
	data, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil || f.Encoding != "base64" {
		s.t.Fatalf("%s of %s at %s: encoding %q: %v", p, name, ref, f.Encoding, err)
	}
	return string(data)
}

// marker returns the marker of merge request n of target name, which must
// be the hub's.
func (s *scenario) marker(name string, n int64) marker.Marker {
	s.t.Helper()
	m, status := marker.Find(s.mr(name, n).Description, []string{s.fp})
	if status != marker.Found {
		s.t.Fatalf("%s !%d: the hub's marker is %s", name, n, status)
	}
	return m
}

// editMR changes fields of merge request n of target name as as, as a
// person does in the web interface.
func (s *scenario) editMR(name string, n int64, as account, fields map[string]any) {
	s.t.Helper()
	s.e.api(as).ok(s.t, http.MethodPut, fmt.Sprintf("/projects/%d/merge_requests/%d", s.ids[name], n), fields, nil)
}

// wantOps checks the writes of rep by target: exactly want[name] for the
// targets it names, in the order of the journal, and none for the others.
// A write reads "<kind>", with " #<n>" when it is to merge request n.
func (s *scenario) wantOps(what string, rep report.Delivery, want map[string][]string) {
	s.t.Helper()
	byTarget := map[string]string{}
	for name, p := range s.repos {
		byTarget[s.provider+":"+p] = name
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

// checkOpened checks target name's only merge request against the report
// and against what touchmark promises: the sync branch of the target itself
// to the default branch, by the writer, with the title, the label, a body
// that lists the files and ends in our marker, and the pack files on the
// branch.
func (s *scenario) checkOpened(name string, rep report.Delivery) {
	s.t.Helper()
	mrs := s.e.mrs(s.t, s.ids[name], "all")
	if len(mrs) != 1 {
		s.t.Fatalf("%s has %d merge requests, want 1", name, len(mrs))
	}
	mr := mrs[0]
	var tg report.DeliveryTarget
	for _, x := range rep.Targets {
		if x.Path == s.repos[name] {
			tg = x
		}
	}
	switch {
	case tg.PR == nil || tg.PR.Number != mr.IID:
		s.t.Errorf("%s: the report names %+v, GitLab has !%d", name, tg.PR, mr.IID)
	case mr.State != "opened" || mr.Draft:
		s.t.Errorf("%s !%d: state %s, draft %v; want opened and ready", name, mr.IID, mr.State, mr.Draft)
	case mr.SourceBranch != s.branch || mr.SourceProjectID != mr.TargetProjectID || mr.TargetBranch != "main":
		s.t.Errorf("%s !%d: %s (project %d) → %s (project %d), want %s → main in the target itself", name, mr.IID,
			mr.SourceBranch, mr.SourceProjectID, mr.TargetBranch, mr.TargetProjectID, s.branch)
	case mr.Author.ID != s.e.Writer.ID:
		s.t.Errorf("%s !%d: opened by %s, want the writer %s", name, mr.IID, mr.Author.Username, s.e.Writer.Login)
	case mr.Title != "chore: sync engineering assets":
		s.t.Errorf("%s !%d: title %q", name, mr.IID, mr.Title)
	case !slices.Contains(mr.Labels, "engineering-assets"):
		s.t.Errorf("%s !%d: labels %v, want engineering-assets", name, mr.IID, mr.Labels)
	}
	m, status := marker.Find(mr.Description, []string{s.fp})
	wantPacks := []string{"base"}
	files := []string{"AGENTS.md", "docs/guide.md"}
	if name == "delta" {
		wantPacks = []string{"base", "extra"}
		files = append(files, "docs/extra.md")
	}
	switch {
	case status != marker.Found:
		s.t.Errorf("%s !%d: marker %s in the body:\n%s", name, mr.IID, status, mr.Description)
	case m.Data.Hub != s.id || m.Data.FP != s.fp || m.Stream != "sync" || m.Data.ContentCommit != s.hubHead():
		s.t.Errorf("%s !%d: marker hub %s fp %s stream %s content %s", name, mr.IID, m.Data.Hub, m.Data.FP, m.Stream, m.Data.ContentCommit)
	case !slices.Equal(sorted(m.Data.Packs), wantPacks):
		s.t.Errorf("%s !%d: marker packs %v, want %v", name, mr.IID, m.Data.Packs, wantPacks)
	case !strings.HasPrefix(lastLine(mr.Description), "<!-- touchmark:v1 "):
		s.t.Errorf("%s !%d: the marker is not the last line of the body", name, mr.IID)
	}
	for _, line := range strings.Split(mr.Description, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "/") {
			s.t.Errorf("%s !%d: the body has a line that GitLab could run as a quick action: %q", name, mr.IID, line)
		}
	}
	for _, f := range files {
		if !strings.Contains(mr.Description, "`"+f+"`") {
			s.t.Errorf("%s !%d: the body does not list %s:\n%s", name, mr.IID, f, mr.Description)
		}
		if got, want := s.file(name, s.branch, f), s.packs[packPath(f)]; got != want {
			s.t.Errorf("%s: %s on %s is %q, want %q", name, f, s.branch, got, want)
		}
	}
	s.checkCommit(name, mr)
}

// packPath returns the hub path of target file f.
func packPath(f string) string {
	if f == "docs/extra.md" {
		return "packs/extra/" + f
	}
	return "packs/base/" + f
}

// checkCommit checks the head commit of a merge request of touchmark's: one
// commit on the default branch's tip, authored and committed alike, by the
// writer's name, with touchmark's trailers.
func (s *scenario) checkCommit(name string, mr apiMR) {
	s.t.Helper()
	c := s.commit(name, mr.SHA)
	main, _ := s.e.branchHead(s.t, s.ids[name], "main")
	msg := c.Message
	switch {
	case c.AuthorName != c.CommitterName || c.AuthorEmail != c.CommitterEmail || c.AuthorEmail == "":
		s.t.Errorf("%s %s: author %s <%s>, committer %s <%s>", name, c.ID, c.AuthorName, c.AuthorEmail, c.CommitterName, c.CommitterEmail)
	case len(c.ParentIDs) != 1 || c.ParentIDs[0] != main:
		s.t.Errorf("%s %s: parents %v, want the tip of main %s", name, c.ID, c.ParentIDs, main)
	case !strings.Contains(msg, "\nTouchmark-Hub: "+s.id+"@"+s.fp+"\n") || !strings.Contains(msg, "\nTouchmark-Content: sha256:") ||
		!strings.Contains(msg, "\nTouchmark-Hub-Commit: "+s.hubHead()):
		s.t.Errorf("%s %s: message without touchmark's trailers:\n%s", name, c.ID, msg)
	}
	finding(s.t, "commit-identity", "%s: touchmark's commit is by %s <%s>", name, c.AuthorName, c.AuthorEmail)
}

// checkDeclined checks what distribute writes when it first sees a decline:
// ack in the marker and one note by the writer.
func (s *scenario) checkDeclined(name string, n int64) {
	s.t.Helper()
	m := s.marker(name, n)
	if !m.Data.Ack {
		s.t.Errorf("%s !%d: ack %v; want an acknowledged decline", name, n, m.Data.Ack)
	}
	notes := s.e.notes(s.t, s.ids[name], n)
	if len(notes) != 1 || notes[0].Author.ID != s.e.Writer.ID {
		s.t.Errorf("%s !%d: %d notes (%v), want one by the writer", name, n, len(notes), notes)
	}
}

// forkMR opens, as the person, a merge request into target name from a fork
// in the person's namespace, on a branch named like the sync branch, with a
// copy of the body of touchmark's merge request and its marker (threat
// T9), and keeps it for checkFork.
func (s *scenario) forkMR(name string) {
	s.t.Helper()
	person := s.e.api(s.e.Person)
	var fork apiProject
	forkName := s.id + "-" + name + "-fork"
	person.ok(s.t, http.MethodPost, fmt.Sprintf("/projects/%d/fork", s.ids[name]), map[string]any{
		"namespace_path": s.e.Person.Login, "name": forkName, "path": forkName,
	}, &fork)
	deadline := time.Now().Add(2 * time.Minute)
	for fork.ImportStatus != "finished" && fork.ImportStatus != "none" && fork.ImportStatus != "" {
		if time.Now().After(deadline) || fork.ImportStatus == "failed" {
			s.t.Fatalf("the fork %s: import %s", fork.PathWithNamespace, fork.ImportStatus)
		}
		time.Sleep(500 * time.Millisecond)
		person.get(s.t, fmt.Sprintf("/projects/%d", fork.ID), &fork)
	}
	// A fork copies every branch, the sync branch too: the person commits on
	// top of that copy when there is one.
	start := ""
	if !s.e.branchExists(s.t, fork.ID, s.branch) {
		start = "main"
	}
	s.e.commitFiles(s.t, s.e.Person, fork.ID, s.branch, start, fileList(map[string]string{"mine.md": "mine\n"}), "my own sync")
	own := s.mr(name, 1)
	var mr apiMR
	person.ok(s.t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", fork.ID), map[string]any{
		"source_branch": s.branch, "target_branch": "main", "target_project_id": s.ids[name],
		"title": own.Title, "description": own.Description,
	}, &mr)
	if mr.SourceProjectID == mr.TargetProjectID || mr.SourceBranch != s.branch {
		s.t.Fatalf("the fork's merge request !%d: %s in project %d, target project %d", mr.IID, mr.SourceBranch, mr.SourceProjectID, mr.TargetProjectID)
	}
	s.fork, s.forkTarget = &mr, name
}

// checkFork checks that the fork's merge request is as it was opened: no
// edit, no note, no close, no push: it is not touchmark's own.
func (s *scenario) checkFork() {
	s.t.Helper()
	if s.fork == nil {
		return
	}
	opened := *s.fork
	now := s.mr(s.forkTarget, opened.IID)
	notes := s.e.notes(s.t, s.ids[s.forkTarget], opened.IID)
	if now.State != "opened" || now.Description != opened.Description || now.Title != opened.Title || now.SHA != opened.SHA ||
		len(notes) != 0 || len(now.Labels) != len(opened.Labels) {
		s.t.Errorf("the fork's merge request !%d was touched: state %s, %d notes, labels %v, head %s (was %s), body changed %v",
			opened.IID, now.State, len(notes), now.Labels, now.SHA, opened.SHA, now.Description != opened.Description)
	}
}

// scanPlatform checks that no token reached a merge request, a note or a
// commit message of the targets, nor any output of the command line.
func (s *scenario) scanPlatform() {
	s.t.Helper()
	for i, out := range s.outputs {
		if s.e.Redact.Contains(out) {
			s.t.Errorf("output %d of the command line holds a token", i)
		}
	}
	for name, id := range s.ids {
		for _, mr := range s.e.mrs(s.t, id, "all") {
			if s.e.Redact.Contains(mr.Title + "\n" + mr.Description) {
				s.t.Errorf("%s !%d holds a token", name, mr.IID)
			}
			for _, n := range s.e.notes(s.t, id, mr.IID) {
				if s.e.Redact.Contains(n.Body) {
					s.t.Errorf("a note on %s !%d holds a token", name, mr.IID)
				}
			}
		}
		for _, b := range s.syncBranches() {
			resp := s.e.api(s.e.Root).do(s.t, http.MethodGet, fmt.Sprintf("/projects/%d/repository/commits?per_page=50&ref_name=%s", id, url.QueryEscape(b)), nil)
			if resp.Status != http.StatusOK {
				continue
			}
			var commits []apiCommit
			decode(s.t, "commits", resp, &commits)
			for _, c := range commits {
				if s.e.Redact.Contains(c.Message) {
					s.t.Errorf("commit %s of %s on %s holds a token", c.ID, name, b)
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
