package distribute

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// runDeps are the dependencies of a run in mode over the in-memory world w
// with a git snapshot source that is never reached: the run's own errors
// come before any target is inspected.
func runDeps(w *world, mode Mode) Deps {
	d := w.deps()
	d.Snapshots = &snapshot.GitSource{Dir: w.t.TempDir(), Isolation: gitx.Isolation{Home: w.t.TempDir()}}
	d.Write.HubBlobs = func(string) (io.ReadCloser, error) { return nil, errors.New("unused") }
	if mode != ModePlan {
		d.Providers[0].Writer = w.p.Writer(w.writer)
	}
	return d
}

// TestRunErrors: the errors of a run itself, before anything is read.
func TestRunErrors(t *testing.T) {
	w := newWorld(t)
	for name, tc := range map[string]struct {
		mode Mode
		edit func(*Deps)
		want string
	}{
		"dry run without repositories":    {ModeDryRun, func(d *Deps) { d.Snapshots = w.p.Snapshots() }, "hands out no target repositories"},
		"distribute without repositories": {ModeDistribute, func(d *Deps) { d.Snapshots = w.p.Snapshots() }, "hands out no target repositories"},
		"no hub blobs":                    {ModePlan, func(d *Deps) { d.Write.HubBlobs = nil }, "no source of hub blobs"},
		"no writer":                       {ModeDryRun, func(d *Deps) { d.Providers[0].Writer = nil }, "provider gh has no writer"},
		"bad intro":                       {ModePlan, func(d *Deps) { d.Write.Intro = "/close\n" }, "pr.intro_file"},
	} {
		t.Run(name, func(t *testing.T) {
			d := runDeps(w, tc.mode)
			tc.edit(&d)
			rep, err := Run(t.Context(), d, tc.mode)
			if err == nil || rep != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, %v; want an error with %q", rep, err, tc.want)
			}
			prefix := "distribute: "
			if tc.mode == ModePlan {
				prefix = "plan: "
			}
			if !strings.HasPrefix(err.Error(), prefix) {
				t.Errorf("error %q lacks %q", err, prefix)
			}
		})
	}
}

// TestRunWriterMismatch: distribute acts as the writer hub.yml names, or
// not at all (exit 2).
func TestRunWriterMismatch(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/x", nil)
	for name, edit := range map[string]func(*world, *Deps){
		"another account": func(w *world, d *Deps) { d.Providers[0].Writer = w.p.Writer(w.known) },
		"no writer named": func(w *world, d *Deps) {
			hub, _, err := config.ParseHub([]byte(strings.Replace(defaultHubYML, "    writer: acme-write[bot]\n", "", 1)))
			if err != nil {
				t.Fatal(err)
			}
			d.Hub = hub
			d.Providers[0].Config.Writer = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := runDeps(w, ModeDryRun)
			edit(w, &d)
			rep, err := Run(t.Context(), d, ModeDryRun)
			if !errors.Is(err, ErrWriterMismatch) || rep != nil {
				t.Fatalf("Run = %v, %v; want ErrWriterMismatch", rep, err)
			}
			for _, c := range w.p.Calls() {
				if strings.HasPrefix(c, "ReadFile") || strings.HasPrefix(c, "Resolve") {
					t.Errorf("a target was read: %q", c)
				}
			}
		})
	}
	// The right writer: its login in the report, the check done.
	d := runDeps(w, ModeDryRun)
	r, err := newRun(d, ModeDryRun)
	if err != nil {
		t.Fatal(err)
	}
	if !r.connect(t.Context(), r.provs[0]) || r.err != nil {
		t.Fatalf("connect: %v", r.err)
	}
	if info := r.rep.Providers[0]; info.Writer != "acme-write[bot]" || info.WriteCheck != "ok" || r.provs[0].self.ID != w.writer.ID {
		t.Errorf("provider %+v, self %+v", info, r.provs[0].self)
	}
}

// spyWriter counts the calls of a writer.
type spyWriter struct {
	platform.Writer
	mu    sync.Mutex
	calls []string
}

func (s *spyWriter) called(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, method)
}

func (s *spyWriter) Probe(ctx context.Context) (platform.Caps, error) {
	s.called("Probe")
	return s.Writer.Probe(ctx)
}

func (s *spyWriter) Self(ctx context.Context) (platform.Account, error) {
	s.called("Self")
	return s.Writer.Self(ctx)
}

func (s *spyWriter) Lookup(ctx context.Context, login string) (platform.Account, error) {
	s.called("Lookup")
	return s.Writer.Lookup(ctx, login)
}

func (s *spyWriter) Resolve(ctx context.Context, sel platform.Selector) (platform.Resolved, error) {
	s.called("Resolve")
	return s.Writer.Resolve(ctx, sel)
}

func (s *spyWriter) Repo(ctx context.Context, path string) (platform.Repo, error) {
	s.called("Repo")
	return s.Writer.Repo(ctx, path)
}

func (s *spyWriter) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	s.called("ReadFile")
	return s.Writer.ReadFile(ctx, r, ref, path, max)
}

func (s *spyWriter) Remote(ctx context.Context, r platform.Repo) (platform.Remote, error) {
	s.called("Remote")
	return s.Writer.Remote(ctx, r)
}

func (s *spyWriter) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	s.called("PRs")
	return s.Writer.PRs(ctx, r, heads, authors)
}

func (s *spyWriter) OpenPRsBy(ctx context.Context, authors []platform.Account, heads []string) (platform.Swept, error) {
	s.called("OpenPRsBy")
	return s.Writer.OpenPRsBy(ctx, authors, heads)
}

func (s *spyWriter) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	s.called("Target")
	return s.Writer.Target(ctx, r, need)
}

// TestRunPlanNeverCallsWriter: a plan reads with the reader alone (I9); a
// dry run reads with the writer and runs its preflight.
func TestRunPlanNeverCallsWriter(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.optedIn("acme/a", nil)
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", baseFiles...)
	w.openOwn(api)
	for _, mode := range []Mode{ModePlan, ModeDryRun} {
		spy := &spyWriter{Writer: w.p.Writer(w.writer)}
		d := w.deps(mode)
		d.Providers[0].Writer = spy
		w.run(d, mode)
		switch {
		case mode == ModePlan && len(spy.calls) > 0:
			t.Errorf("plan called the writer: %q", spy.calls)
		case mode == ModeDryRun && (!slices.Contains(spy.calls, "Self") || !slices.Contains(spy.calls, "Target") || !slices.Contains(spy.calls, "PRs")):
			t.Errorf("a dry run did not read and check with the writer: %q", spy.calls)
		}
	}
}

// TestRunReinspect: execute's recheck inspects a target that moved again,
// from a fresh report line, and gets the new decision for the same target.
func TestRunReinspect(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", "AGENTS.md", agentsV1, "docs/guide.md", guideV1)
	n := w.ownOn(api, branch, keyOf("AGENTS.md", agentsV1, "docs/guide.md", guideV1), "AGENTS.md", agentsV1, "docs/guide.md", guideV1)
	d := w.deps(ModeDistribute)
	r, err := newRun(d, ModeDistribute)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := r.resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.inspectAll(t.Context(), kept)
	tg := kept[0]
	first := tg.work
	if first == nil || first.Decision.Outcome != "updated" || tg.res.Reason != "content" {
		t.Fatalf("first inspection: %+v, %+v", first, tg.res)
	}
	// The work writes, so its repository waits for phase F.
	if dirs, _ := os.ReadDir(w.src.Dir); len(dirs) != 1 {
		t.Errorf("%d repositories, want the target's", len(dirs))
	}
	w.push(api, branch, w.person, "notes.md", "mine\n")
	again := r.reinspect(t.Context(), first)
	if again == nil || again == first || again.t != tg || tg.work != again {
		t.Fatalf("reinspect: %+v", again)
	}
	if tg.res.Outcome != report.OutcomeBlocked || tg.res.Reason != "edited" || tg.res.PR == nil || tg.res.PR.Number != n {
		t.Errorf("after the push: %+v", tg.res)
	}
	if !again.Decision.Blocks.Paused || again.Branch.Head == first.Branch.Head {
		t.Errorf("decision %+v, branch %+v", again.Decision, again.Branch)
	}
	r.finished(tg, false)
	if dirs, _ := os.ReadDir(w.src.Dir); len(dirs) != 0 {
		t.Errorf("repositories left: %v", dirs)
	}
}

// TestRunDryRunPreflight: a dry run checks the write permissions of every
// target that would write (Writer.Target), where a plan cannot.
func TestRunDryRunPreflight(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	r := w.optedIn("acme/x", nil)
	w.p.Grant(r.ID, w.writer, platform.Perms{PRs: true})
	w.optedIn("acme/y", nil)
	plan := w.run(w.deps(ModePlan), ModePlan)
	want(t, plan, "gh:acme/x", report.OutcomeOpened, "", 0)
	dry := w.run(w.deps(ModeDryRun), ModeDryRun)
	tg := want(t, dry, "gh:acme/x", report.OutcomeBlocked, "permission:contents", 0)
	if tg.Writes != 0 || !hasWarningLike(tg, "preflight: ") {
		t.Errorf("writes %d, warnings %q", tg.Writes, tg.Warnings)
	}
	want(t, dry, "gh:acme/y", report.OutcomeOpened, "", 0)
	if dry.Cost["gh"] != 4 {
		t.Errorf("cost %v", dry.Cost)
	}

	// A preflight that fails otherwise is tried again, and then fails the
	// target by its class; every per-target writer it got is closed. The
	// faults hit the first target in targets.yml order, acme/x.
	for _, tc := range []struct {
		faults  int
		outcome report.Outcome
		reason  string
	}{
		{1, report.OutcomeBlocked, "permission:contents"},
		{readAttempts, report.OutcomeFailed, "transient"},
	} {
		w.p.ResetCalls()
		transientTimes(w.p, "Target", errTransient, tc.faults)
		dry := w.run(w.deps(ModeDryRun), ModeDryRun)
		tg := want(t, dry, "gh:acme/x", tc.outcome, tc.reason, 0)
		if !hasWarningLike(tg, "preflight: ") {
			t.Errorf("%d faults: warnings %q", tc.faults, tg.Warnings)
		}
		want(t, dry, "gh:acme/y", report.OutcomeOpened, "", 0)
		if n := countCalls(w.p, "Target acme/x"); n != min(tc.faults+1, readAttempts) {
			t.Errorf("%d faults: %d preflights of acme/x, want %d", tc.faults, n, min(tc.faults+1, readAttempts))
		}
		if writes := w.p.Writes(); len(writes) > 0 {
			t.Errorf("%d faults: the dry run wrote %q", tc.faults, writes)
		}
		if open := w.p.OpenTargetWriters(); len(open) > 0 {
			t.Errorf("%d faults: per-target writers left open for %q", tc.faults, open)
		}
	}
}

// TestRunStream: one JSON line per target once its outcome is final, in
// the stream's format, secrets masked; repositories released.
func TestRunStream(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil, baseFiles...)
	link := w.repo("acme/unsafe-secret-path", nil, "README.md", "x\n")
	w.p.SetSymlink(link.ID, optInName, "README.md")
	w.optedIn("acme/c", func(r *platform.Repo) { r.Archived = true })
	reg := redact.New()
	reg.Add("unsafe-secret-path")
	var buf bytes.Buffer
	out := bufio.NewWriter(&buf)
	d := w.deps(ModePlan)
	d.Write.Stream, d.Write.Redact = out, reg
	rep := w.run(d, ModePlan)
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(rep.Targets) {
		t.Fatalf("%d lines for %d targets:\n%s", len(lines), len(rep.Targets), buf.String())
	}
	seen := map[string]report.Outcome{}
	for _, line := range lines {
		var l streamLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		seen[l.Target.Path] = l.Target.Outcome
		if l.Ops == nil {
			t.Errorf("line %q has no ops list", line)
		}
		for _, warning := range l.Target.Warnings {
			if strings.Contains(warning, "unsafe-secret-path") {
				t.Errorf("a secret in a warning: %q", warning)
			}
		}
	}
	for _, tg := range rep.Targets {
		if seen[tg.Path] != tg.Outcome {
			t.Errorf("%s: streamed %s, reported %s", tg.Path, seen[tg.Path], tg.Outcome)
		}
	}
	if !strings.Contains(buf.String(), "***") {
		t.Errorf("no masked warning:\n%s", buf.String())
	}
	// Every repository was released.
	if dirs, err := os.ReadDir(w.src.Dir); err != nil || len(dirs) != 0 {
		t.Errorf("repositories left: %v, %v", dirs, err)
	}
}

// TestRunRolloutLimit: new pull requests beyond max_new_prs_per_run wait
// for the next run, and cost nothing.
func TestRunRolloutLimit(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.hubYML += "limits:\n  max_new_prs_per_run: 1\n"
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil)
	rep := w.both(nil)
	want(t, rep, "gh:acme/a", report.OutcomeOpened, "", 0)
	tg := want(t, rep, "gh:acme/b", report.OutcomeDeferred, "rollout-limit", 0)
	if tg.Writes != 0 || rep.Cost["gh"] != 4 {
		t.Errorf("writes %d, cost %v", tg.Writes, rep.Cost)
	}
}

// TestRunDeadline: past Write.Deadline no target is started.
func TestRunDeadline(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", func(r *platform.Repo) { r.Archived = true })
	rep := w.both(func(d *Deps) { d.Write.Deadline = time.Now().Add(-time.Minute) })
	want(t, rep, "gh:acme/a", report.OutcomeDeferred, "deadline", 0)
	want(t, rep, "gh:acme/b", report.OutcomeSkipped, "archived", 0)
	for _, c := range w.p.Calls() {
		if strings.HasPrefix(c, "ReadFile") {
			t.Errorf("a target was read past the deadline: %q", c)
		}
	}
}

// TestRunLookupFails: a pull request that may be ours while a Lookup of
// the writer failed fails the target, as in the snapshot-only plan.
func TestRunLookupFails(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", baseFiles...)
	w.openOwn(api)
	w.optedIn("acme/free", nil)
	fail := &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	transientTimes(w.p, "Lookup", fail, readAttempts)
	rep := w.run(w.deps(ModePlan), ModePlan)
	tg := want(t, rep, "gh:acme/api", report.OutcomeFailed, "transient", 1)
	if !hasWarningLike(tg, "tell whether #1 by acme-write[bot] is touchmark's pull request") {
		t.Errorf("warnings %q", tg.Warnings)
	}
	want(t, rep, "gh:acme/free", report.OutcomeOpened, "", 0)
}

// TestRunGitLab: a GitLab target in git mode; a new merge request costs 2.
func TestRunGitLab(t *testing.T) {
	t.Parallel()
	needDeliveryGit(t)
	p := fake.New("gitlab.example.com", fake.WithFlavor(fake.GitLab))
	srv, err := p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	reader := p.AddAccount("tm-reader", platform.KindServiceAccount)
	writer := p.AddAccount("tm-writer", platform.KindServiceAccount)
	p.SetToken(reader, "gitlab-reader-token-1")
	p.SetToken(writer, "gitlab-writer-token-2")
	r := p.AddRepo(platform.Repo{Path: "platform/api"})
	p.SetFile(r.ID, optInName, []byte("version: 1\n"), "")
	p.GrantWrite(r.ID, writer)
	w := newWorld(t)
	w.hubYML = "version: 1\nid: acme-eng\nproviders:\n  - id: corp\n    type: gitlab\n    url: https://gitlab.example.com\n    writer: tm-writer\n"
	w.targetsYML = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - group: platform\n"
	g := &gitWorld{world: w, src: &snapshot.GitSource{Dir: t.TempDir(), Isolation: gitx.Isolation{Home: t.TempDir(), AllowHTTP: true}},
		blobs: map[string][]byte{oid(agentsV2): []byte(agentsV2), oid(guideV1): []byte(guideV1)}}
	hub, _, err := config.ParseHub([]byte(w.hubYML))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModePlan, ModeDryRun} {
		d := g.deps(mode)
		d.Providers = w.providers(hub, map[string]platform.Reader{"gitlab.example.com": p.Reader(reader)})
		if mode == ModeDryRun {
			d.Providers[0].Writer = p.Writer(writer)
		}
		rep, err := Run(t.Context(), d, mode)
		if err != nil {
			t.Fatal(err)
		}
		checkReport(t, rep)
		tg := want(t, rep, "corp:platform/api", report.OutcomeOpened, "", 0)
		if tg.Writes != 2 || rep.Cost["corp"] != 2 {
			t.Errorf("%v: writes %d, cost %v", mode, tg.Writes, rep.Cost)
		}
	}
	if writes := p.Writes(); len(writes) > 0 {
		t.Errorf("wrote %q", writes)
	}
}
