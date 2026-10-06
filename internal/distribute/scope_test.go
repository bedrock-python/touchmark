package distribute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// TestScopeOf: the paths a hub pull request changes decide its scope.
func TestScopeOf(t *testing.T) {
	hub := &config.Hub{PR: config.PR{IntroFile: "docs/intro.md"}, Providers: []config.Provider{{ID: "corp", CAFile: "certs/./corp.pem"}}}
	cases := []struct {
		changed []string
		want    Scope
	}{
		{nil, Scope{Mode: report.ScopeHub}},
		{[]string{"README.md", ".github/workflows/x.yml", "packs/README.md"}, Scope{Mode: report.ScopeHub}},
		{[]string{"packs/python/docs/python.md"}, Scope{Mode: report.ScopePacks, Packs: []string{"python"}}},
		{[]string{"packs/python/a", "packs/base/b", "README.md", "packs/python/c"}, Scope{Mode: report.ScopePacks, Packs: []string{"base", "python"}}},
		{[]string{"packs/python/a", "hub.yml"}, Scope{Mode: report.ScopeAll, Reason: "hub.yml changed"}},
		{[]string{"targets.yml"}, Scope{Mode: report.ScopeAll, Reason: "targets.yml changed"}},
		{[]string{".touchmark/operations.yml"}, Scope{Mode: report.ScopeAll, Reason: ".touchmark/operations.yml changed"}},
		{[]string{"schemas/hub.schema.json"}, Scope{Mode: report.ScopeAll, Reason: "schemas/hub.schema.json changed"}},
		{[]string{"docs/intro.md"}, Scope{Mode: report.ScopeAll, Reason: "docs/intro.md changed"}},
		{[]string{"certs/corp.pem"}, Scope{Mode: report.ScopeAll, Reason: "certs/corp.pem changed"}},
		{[]string{"targets.yml", "hub.yml", ".touchmark/a", ".touchmark/b", "hub.yml"},
			Scope{Mode: report.ScopeAll, Reason: ".touchmark/a, .touchmark/b, hub.yml and 1 more changed"}},
		// A nested hub.yml is no configuration.
		{[]string{"packs/base/hub.yml"}, Scope{Mode: report.ScopePacks, Packs: []string{"base"}}},
	}
	for _, tc := range cases {
		got := ScopeOf(tc.changed, hub)
		if got.Mode != tc.want.Mode || got.Reason != tc.want.Reason || !slices.Equal(got.Packs, tc.want.Packs) {
			t.Errorf("ScopeOf(%q) = %+v, want %+v", tc.changed, got, tc.want)
		}
	}
	if got := ScopeOf([]string{"docs/intro.md"}, nil); got.Mode != report.ScopeHub {
		t.Errorf("without hub.yml: %+v", got)
	}
}

// scopeWorld is a world of a public hub's pull request, whose hub has a
// third pack, lint, that requires python, and knows python's former name
// py.
func scopeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.hubYML = defaultHubYML + "packs:\n  lint:\n    requires: [python]\n  python:\n    formerly: [py]\n"
	w.ctx = hubch.Context{CI: hubch.GitHubActions, Host: "github.com", RepoID: "712345678", Visibility: "public",
		DefaultBranch: "main", RefName: "41/merge", Event: "pull_request"}
	return w
}

// scopeDeps are the world's deps with the lint pack and scope s.
func scopeDeps(w *world, s *Scope) Deps {
	d := w.deps()
	d.Known["lint"] = true
	d.Current["lint"] = map[string]provenance.File{}
	d.Scope = s
	return d
}

// snapshotted lists the repositories the platform took snapshots of.
func snapshotted(p interface{ Calls() []string }) []string {
	var out []string
	for _, c := range p.Calls() {
		if repo, ok := strings.CutPrefix(c, "Snapshot "); ok {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

// A plan limited to the targets of one pack processes the targets whose
// final pack list holds it, through targets.yml, requires or a former name;
// a target whose outcome is known before its packs (not opted in, invalid,
// skipped, private) counts when the hub's packs or those its opt-in file
// names hold it. The others are counted, not listed, and never snapshotted.
func TestPlanScopeSelection(t *testing.T) {
	w := scopeWorld(t)
	w.optedIn("acme/plain", nil)
	w.optedIn("acme/snake", topics("python"))
	w.repo("acme/lint", nil, optInName, "version: 1\npacks: [lint]\n")
	w.repo("acme/old-name", nil, optInName, "version: 1\npacks: [py]\n")
	w.repo("acme/bad", nil, optInName, "version: 1\npacks: [python, nope]\n")
	w.repo("acme/bad-base", nil, optInName, "version: 1\npacks: [nope]\n")
	w.repo("acme/quiet", nil, "README.md", "not opted in")
	w.repo("acme/quiet-py", topics("python"), "README.md", "not opted in")
	w.optedIn("acme/arch", func(r *platform.Repo) { r.Archived = true; r.Topics = []string{"python"} })
	w.optedIn("acme/arch-base", func(r *platform.Repo) { r.Archived = true })
	w.optedIn("acme/secret", func(r *platform.Repo) { r.Visibility = "private" })
	w.optedIn("acme/secret-py", func(r *platform.Repo) { r.Visibility = "private"; r.Topics = []string{"python"} })

	for _, packs := range [][]string{{"python"}, {"py"}} {
		w.p.ResetCalls()
		rep := w.plan(scopeDeps(w, &Scope{Mode: report.ScopePacks, Packs: packs, Base: hubCommit}))
		want(t, rep, "gh:acme/snake", report.OutcomeOpened, "", 0)
		want(t, rep, "gh:acme/lint", report.OutcomeOpened, "", 0)
		want(t, rep, "gh:acme/old-name", report.OutcomeOpened, "", 0)
		want(t, rep, "gh:acme/bad", report.OutcomeBlocked, "opt-in-invalid", 0)
		want(t, rep, "gh:acme/quiet-py", report.OutcomeSkipped, "not-opted-in", 0)
		want(t, rep, "gh:acme/arch", report.OutcomeSkipped, "archived", 0)
		want(t, rep, "gh:", report.OutcomeSkipped, report.ReasonPrivate, 0)
		if len(rep.Targets) != 7 {
			t.Errorf("scope %q: %d targets, want 7: %+v", packs, len(rep.Targets), rep.Targets)
		}
		wantScope := report.Scope{Mode: report.ScopePacks, Packs: []string{"py", "python"}, Base: hubCommit, Processed: 7, Total: 12}
		if packs[0] == "python" {
			wantScope.Packs = []string{"python"}
		}
		if s := rep.Scope; s == nil || s.Mode != wantScope.Mode || !slices.Equal(s.Packs, wantScope.Packs) || s.Base != hubCommit ||
			s.Processed != wantScope.Processed || s.Total != wantScope.Total {
			t.Errorf("scope %q: %+v, want %+v", packs, s, wantScope)
		}
		if got := snapshotted(w.p); !slices.Equal(got, []string{"acme/lint", "acme/old-name", "acme/snake"}) {
			t.Errorf("scope %q: snapshots of %q", packs, got)
		}
		for _, repo := range []string{"acme/plain", "acme/bad-base"} {
			if !slices.Contains(w.p.Calls(), "ReadFile "+repo+" "+optInName) {
				t.Errorf("scope %q: the opt-in file of %s was not read", packs, repo)
			}
		}
	}

	// A pull request of hub files only: the resolve runs, nothing else.
	w.p.ResetCalls()
	rep := w.plan(scopeDeps(w, &Scope{Mode: report.ScopeHub}))
	if len(rep.Targets) != 0 || rep.Scope == nil || rep.Scope.Mode != report.ScopeHub || rep.Scope.Processed != 0 || rep.Scope.Total != 12 {
		t.Errorf("hub only: %d targets, scope %+v", len(rep.Targets), rep.Scope)
	}
	for _, c := range w.p.Calls() {
		if strings.HasPrefix(c, "ReadFile ") || strings.HasPrefix(c, "Snapshot ") || strings.HasPrefix(c, "PRs ") {
			t.Errorf("hub only: %s", c)
		}
	}
	rep.Strict = true
	if code := rep.ExitCode(); code != 0 {
		t.Errorf("hub only: strict exit code %d", code)
	}

	// Every target, with the scope in the report.
	rep = w.plan(scopeDeps(w, &Scope{Mode: report.ScopeAll, Reason: "--all"}))
	if len(rep.Targets) != 12 || rep.Scope == nil || rep.Scope.Reason != "--all" || rep.Scope.Processed != 12 {
		t.Errorf("--all: %d targets, scope %+v", len(rep.Targets), rep.Scope)
	}
	// No scope: every target, none in the report.
	if rep = w.plan(scopeDeps(w, nil)); len(rep.Targets) != 12 || rep.Scope != nil {
		t.Errorf("no scope: %d targets, scope %+v", len(rep.Targets), rep.Scope)
	}
}

// batchReader reads opt-in files in batches over the fake's reader, and
// counts the reads the run makes one by one.
type batchReader struct {
	platform.Reader
	mu sync.Mutex
	// batches are the repositories of each ReadFiles call; single the
	// repositories ReadFile read.
	batches [][]string
	single  []string
	// fail are the errors of the next whole calls; errs the errors of a
	// repository's file in every call.
	fail []error
	errs map[string]error
}

func (b *batchReader) ReadFiles(ctx context.Context, repos []platform.Repo, path string, max int64) ([]platform.File, error) {
	b.mu.Lock()
	var paths []string
	for _, r := range repos {
		paths = append(paths, r.Path)
	}
	b.batches = append(b.batches, paths)
	var fail error
	if len(b.fail) > 0 {
		fail, b.fail = b.fail[0], b.fail[1:]
	}
	b.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	files := make([]platform.File, len(repos))
	errs := make(platform.FileErrors, len(repos))
	failed := false
	for i, r := range repos {
		err := b.errs[r.Path]
		if err == nil {
			files[i], err = b.Reader.ReadFile(ctx, r, "", path, max)
		}
		if err != nil {
			errs[i], failed = err, true
		}
	}
	if failed {
		return files, errs
	}
	return files, nil
}

func (b *batchReader) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	b.mu.Lock()
	b.single = append(b.single, r.Path)
	b.mu.Unlock()
	return b.Reader.ReadFile(ctx, r, ref, path, max)
}

// A provider whose reader is a platform.BatchReader reads the opt-in files
// in chunks of 50, in the order phase C inspects the targets: the targets
// skipped before their file are left out; a missing file of the batch
// needs no read of its own, a file the batch failed for is read on its own,
// and so is every file of a chunk whose call failed for good. The plan is
// the same as one that reads file by file.
func TestPlanBatchOptIn(t *testing.T) {
	w := newWorld(t)
	for i := range 60 {
		w.optedIn(fmt.Sprintf("acme/t%02d", i), nil)
	}
	w.repo("acme/web", nil, "README.md", "not opted in")
	w.optedIn("acme/archived", func(r *platform.Repo) { r.Archived = true })
	flaky := &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}

	single := w.plan(w.deps())
	br := &batchReader{Reader: w.p.Reader(w.reader), errs: map[string]error{"acme/t07": flaky}}
	d := w.deps()
	d.Concurrency = 1
	d.Providers[0].Reader = br
	batched := w.plan(d)
	if a, b := fmt.Sprint(single.Targets), fmt.Sprint(batched.Targets); a != b {
		t.Errorf("batched reads plan otherwise:\n%s\n%s", b, a)
	}
	if len(br.batches) != 2 || len(br.batches[0]) != 50 || len(br.batches[1]) != 11 {
		t.Fatalf("batches of %v", br.batches)
	}
	if slices.Contains(slices.Concat(br.batches...), "acme/archived") {
		t.Error("the archived target's file was read")
	}
	if !slices.Equal(br.single, []string{"acme/t07"}) {
		t.Errorf("single reads of %q, want acme/t07 only", br.single)
	}
	want(t, batched, "gh:acme/web", report.OutcomeSkipped, "not-opted-in", 0)

	// A chunk whose call fails for good (three transient failures) reads
	// its files on its own; the other chunk is read in one call.
	br = &batchReader{Reader: w.p.Reader(w.reader), fail: []error{flaky, flaky, flaky}}
	d.Providers[0].Reader = br
	batched = w.plan(d)
	if a, b := fmt.Sprint(single.Targets), fmt.Sprint(batched.Targets); a != b {
		t.Errorf("after a failed batch the plan differs:\n%s\n%s", b, a)
	}
	if len(br.batches) != 4 || len(br.single) != 50 {
		t.Errorf("%d batch calls, %d single reads; want 4 and 50", len(br.batches), len(br.single))
	}

	// A plan that processes no target reads no opt-in file at all.
	br = &batchReader{Reader: w.p.Reader(w.reader)}
	d.Providers[0].Reader = br
	d.Scope = &Scope{Mode: report.ScopeHub}
	w.plan(d)
	if len(br.batches)+len(br.single) > 0 {
		t.Errorf("hub only: batches %v, single %v", br.batches, br.single)
	}
}

// plan --assume-opt-in plans a target without an opt-in file, or with one
// that is not a regular file, as if it had an empty one; an invalid opt-in
// file stays blocked. The report says so.
func TestPlanAssumeOptIn(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/api", nil)
	w.repo("acme/web", topics("python"), "README.md", "not opted in")
	link := w.repo("acme/link", nil, "README.md", "the target")
	w.p.SetSymlink(link.ID, optInName, "README.md")
	w.repo("acme/bad", nil, optInName, "version: 2\n")

	d := w.deps()
	d.AssumeOptIn = true
	rep := w.plan(d)
	if !rep.Assumed {
		t.Error("the report does not say --assume-opt-in")
	}
	api := want(t, rep, "gh:acme/api", report.OutcomeOpened, "", 0)
	web := want(t, rep, "gh:acme/web", report.OutcomeOpened, "", 0)
	lnk := want(t, rep, "gh:acme/link", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/bad", report.OutcomeBlocked, "opt-in-invalid", 0)
	if api.Assumed || !web.Assumed || !lnk.Assumed {
		t.Errorf("assumed: api %v, web %v, link %v", api.Assumed, web.Assumed, lnk.Assumed)
	}
	if !slices.Equal(web.Packs, []string{"base", "python"}) || web.Key != keyOfMissing("base", "python") {
		t.Errorf("web: packs %q, key %s", web.Packs, web.Key)
	}
	if !hasWarning(lnk.Warnings, "not a regular file") {
		t.Errorf("link: warnings %q", lnk.Warnings)
	}
	var text strings.Builder
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "--assume-opt-in: every target counts as opted in, for this report only; 2 targets without an opt-in file") {
		t.Errorf("text:\n%s", text.String())
	}

	// Without the flag, nothing is assumed.
	rep = w.plan(w.deps())
	if rep.Assumed {
		t.Error("a plan without --assume-opt-in says it")
	}
	want(t, rep, "gh:acme/web", report.OutcomeSkipped, "not-opted-in", 0)
}

// plan --assume-opt-in takes an oversized opt-in file, a regular file in the
// tree, for an empty one: it is not read again at the snapshot's commit, nor
// taken for a race, and its warning is given once.
func TestPlanAssumeOptInTooLarge(t *testing.T) {
	w := newWorld(t)
	w.repo("acme/big", nil, optInName, "version: 1\n#"+strings.Repeat("x", maxOptIn+1024)+"\n")

	rep := w.plan(w.deps())
	want(t, rep, "gh:acme/big", report.OutcomeSkipped, "unsafe-opt-in", 0)

	d := w.deps()
	d.AssumeOptIn = true
	rep = w.plan(d)
	big := want(t, rep, "gh:acme/big", report.OutcomeOpened, "", 0)
	if !big.Assumed {
		t.Error("the oversized opt-in file is not assumed")
	}
	n := 0
	for _, s := range big.Warnings {
		if strings.Contains(s, "snapshot") {
			t.Errorf("warning %q", s)
		}
		if strings.Contains(s, optInName) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warnings %q, want the size once", big.Warnings)
	}
	if code := rep.ExitCode(); code != 0 {
		t.Errorf("exit code %d", code)
	}
}

// TestWriteTime: n writes at a provider's limits take what the throttle's
// windows let them.
func TestWriteTime(t *testing.T) {
	github := throttle.Limits{WritesPerMinute: 60, WritesPerHour: 450, MinInterval: time.Second}
	cases := []struct {
		n    int
		l    throttle.Limits
		want time.Duration
	}{
		{0, github, 0},
		{1, github, 0},
		{2, github, time.Second},
		{60, github, 59 * time.Second},
		{61, github, time.Minute},
		{450, github, 7*time.Minute + 29*time.Second},
		{451, github, time.Hour},
		// A first rollout over 500 GitHub targets, 4 writes each.
		{2000, github, 4*time.Hour + 3*time.Minute + 19*time.Second},
		{5, throttle.Limits{MinInterval: 250 * time.Millisecond}, time.Second},
		{1000, throttle.Limits{}, 0},
	}
	for _, tc := range cases {
		if got := writeTime(tc.n, tc.l); got != tc.want {
			t.Errorf("writeTime(%d, %+v) = %v, want %v", tc.n, tc.l, got, tc.want)
		}
	}
}

// The estimate of a plan: per provider the writes and their time, and the
// runs max_new_prs_per_run gives the rollout, with the writes of every run.
func TestPlanEstimate(t *testing.T) {
	w := newWorld(t)
	w.hubYML = defaultHubYML + "limits:\n  max_new_prs_per_run: 2\n"
	for i := range 5 {
		w.optedIn(fmt.Sprintf("acme/new%d", i), nil)
	}
	old := w.optedIn("acme/old", nil, "AGENTS.md", agentsV2, "docs/guide.md", guideV1)
	w.ownPR(old, keyOfMissing("base"))
	rep := w.plan(w.deps())
	e := rep.Estimate
	if e == nil {
		t.Fatal("no estimate")
		return
	}
	// Two open now (3 writes and the label each), one closes (3); three
	// more open in two runs after this one.
	gh := e.Providers["gh"]
	if e.NewPRs != 5 || e.MaxNewPRs != 2 || e.Runs != 3 || gh.Writes != 11 || gh.TotalWrites != 23 || gh.APIWrites != 0 {
		t.Errorf("estimate %+v, gh %+v", e, gh)
	}
	if gh.Seconds != 10 || gh.TotalSeconds != 22 {
		t.Errorf("gh: %d s, %d s in all", gh.Seconds, gh.TotalSeconds)
	}
	if rep.Cost["gh"] != gh.Writes {
		t.Errorf("cost %d, estimate %d", rep.Cost["gh"], gh.Writes)
	}
	var text strings.Builder
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "Cost  gh ≈ 11 writes (<1 min), ≈ 23 (<1 min) over all runs · 3 runs (5 new pull requests, max_new_prs_per_run 2)") {
		t.Errorf("text:\n%s", text.String())
	}

	// max_new_prs_per_run 0 opens none: no run can.
	w.hubYML = defaultHubYML + "limits:\n  max_new_prs_per_run: 0\n"
	if e := w.plan(w.deps()).Estimate; e.Runs != 0 || e.NewPRs != 5 {
		t.Errorf("max 0: %+v", e)
	}
}
