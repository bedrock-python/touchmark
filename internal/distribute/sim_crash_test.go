package distribute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// The crash matrix: every write of every kind a run makes (a push that
// creates, moves or deletes a branch, a new pull request, an edit of its
// content, a close, an ack, a revocation, a consumed recreate, a comment, a
// sweep close) goes wrong once: it fails right before the platform applies
// it (fake.FailNext) or right after (fake.FailNextApplied, the answer lost),
// with an error that ends the target (401), or after it with one the run
// reads the platform back for (502); or the run is cancelled while the write
// is in flight (SIGTERM). The report stream keeps one line per target
// whatever happened. Then distribute runs again, and the world must converge
// to what a run without the fault left: the same pull requests of
// touchmark's with the same state, head and marker (no duplicate, no false
// decline), no commit of a person lost, no forbidden transition, no comment
// posted twice, and a run after that writes nothing. A branch a failed run
// could not delete after its close may stay (the next run leaves the branch
// of a closed pull request alone); the test logs it.

// simCrashCase is one state whose next run makes the writes the matrix
// cuts.
type simCrashCase struct {
	name   string
	flavor fake.Flavor
	// app makes the world a GitHub App's that commits through the API
	// (simWorld.app).
	app bool
	// setup brings a world with target api to the state; edit changes the
	// deps of the runs that follow (nil for none).
	setup func(w *simWorld)
	edit  func(w *simWorld, d *Deps)
}

// editFor returns the case's deps edit for world w, nil for none.
func (cc simCrashCase) editFor(w *simWorld) func(*Deps) {
	if cc.edit == nil {
		return nil
	}
	return func(d *Deps) { cc.edit(w, d) }
}

// simCrashCases are the states of the matrix.
var simCrashCases = []simCrashCase{
	{name: "open", flavor: fake.GitHub, setup: func(*simWorld) {}},
	{name: "open-gitlab", flavor: fake.GitLab, setup: func(*simWorld) {}},
	{name: "open-gitea", flavor: fake.Gitea, setup: func(*simWorld) {}},
	{name: "open-bitbucket", flavor: fake.Bitbucket, setup: func(*simWorld) {}},
	{name: "update", flavor: fake.GitHub, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.ship("AGENTS.md", 2)
		w.hubChanged()
	}},
	// A GitHub App that must sign commits through the API: the push to the
	// stage ref, the API commit and the pull request are each cut.
	{name: "open-api", flavor: fake.GitHub, app: true, setup: func(*simWorld) {}},
	{name: "update-api", flavor: fake.GitHub, app: true, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.ship("AGENTS.md", 2)
		w.hubChanged()
	}},
	// The branch is rebuilt through the API where a ruleset refuses force
	// pushes and no pull request is open (the writer's bot closed it, so the
	// content is proposed again): the branch is deleted with a lease, then
	// the stage ref pushed and the API commit made on no branch (Expect ""),
	// then the pull request opened. A run
	// cut between the deletion and the commit leaves no branch; the next
	// converges without a duplicate pull request.
	{name: "recreate-api", flavor: fake.GitHub, app: true, setup: func(w *simWorld) {
		tg := w.target("api")
		w.run(ModeDistribute, nil)
		w.p.SetPRState(tg.repo.ID, 1, platform.Closed, &w.writer, time.Time{})
		w.noForce[tg.name] = true
		w.setRules(tg)
		w.ship("AGENTS.md", 2)
		w.hubChanged()
	}},
	// A signed commit is deterministic (ed25519): the commit a run builds
	// again after a push whose answer was lost is the one on the branch.
	{name: "update-signed", flavor: fake.GitHub, setup: func(w *simWorld) {
		w.run(ModeDistribute, func(d *Deps) { simSign(w, d) })
		w.ship("AGENTS.md", 2)
		w.hubChanged()
	}, edit: simSign},
	{name: "update-after-merge", flavor: fake.Forgejo, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.mustDo(w.p.MergePR(w.target("api").repo.ID, 1, fake.MergeSquash, w.person, time.Time{}))
		w.ship("docs/guide.md", 2)
		w.hubChanged()
	}},
	{name: "close-no-diff", flavor: fake.GitHub, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.mustDo(w.push(w.target("api"), "main", maps.Clone(w.files[simBase])))
	}},
	// On Bitbucket the close is one edit that writes the closed marker and
	// then declines: cut before or after it, the next run converges.
	{name: "close-no-diff-bitbucket", flavor: fake.Bitbucket, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.mustDo(w.push(w.target("api"), "main", maps.Clone(w.files[simBase])))
	}},
	{name: "sweep-dropped-bitbucket", flavor: fake.Bitbucket, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.excluded["api"] = true
	}},
	{name: "ack", flavor: fake.GitHub, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.p.SetPRState(w.target("api").repo.ID, 1, platform.Closed, &w.person, time.Time{})
	}},
	{name: "revoke", flavor: fake.GitLab, setup: func(w *simWorld) {
		tg := w.target("api")
		w.run(ModeDistribute, nil)
		w.p.SetPRState(tg.repo.ID, 1, platform.Closed, &w.person, time.Time{})
		w.run(ModeDistribute, nil)
		if !w.tick(tg, 1, prbody.ControlRepropose) {
			w.t.Fatal("no repropose control in #1")
		}
	}},
	{name: "recreate", flavor: fake.GitHub, setup: func(w *simWorld) {
		tg := w.target("api")
		w.run(ModeDistribute, nil)
		w.mustDo(w.push(tg, "touchmark/acme-eng", map[string]string{"notes.md": simLocal("notes.md", 1)}))
		w.run(ModeDistribute, nil)
		if !w.tick(tg, 1, prbody.ControlRecreate) {
			w.t.Fatal("no recreate control in #1")
		}
	}},
	{name: "sweep-dropped", flavor: fake.GitHub, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.excluded["api"] = true
	}},
	{name: "sweep-opted-out", flavor: fake.Gitea, setup: func(w *simWorld) {
		w.run(ModeDistribute, nil)
		w.mustDo("", w.setOptIn(w.target("api"), ""))
	}},
	{name: "duplicate", flavor: fake.GitHub, setup: func(w *simWorld) {
		tg := w.target("api")
		w.run(ModeDistribute, nil)
		w.renameHub("acme-platform")
		w.p.SetPRState(tg.repo.ID, 1, platform.Closed, &w.person, time.Time{})
		w.ship("AGENTS.md", 2)
		w.hubChanged()
		w.run(ModeDistribute, nil)
		w.p.SetPRState(tg.repo.ID, 1, platform.Open, nil, time.Time{})
	}},
	{name: "recreate-branch", flavor: fake.GitHub, setup: func(w *simWorld) {
		tg := w.target("api")
		w.run(ModeDistribute, nil)
		w.p.SetPRState(tg.repo.ID, 1, platform.Closed, &w.writer, time.Time{})
		w.mustDo(w.push(tg, "main", map[string]string{".github/workflows/ci.yml": "name: ci\non: push\njobs: {}\n"}))
		w.ship("AGENTS.md", 2)
		w.hubChanged()
	}, edit: func(_ *simWorld, d *Deps) { d.Write.CanWorkflows = map[string]bool{"gh": false} }},
}

// simSign signs the provider's commits with the test key (sshsig).
func simSign(w *simWorld, d *Deps) {
	signer, err := sshsig.ParsePrivateKey(testKey(w.t))
	if err != nil {
		w.t.Fatal(err)
	}
	d.Write.Signers = map[string]*sshsig.Signer{"gh": signer}
}

// mustDo fails the test when a human action failed.
func (w *simWorld) mustDo(_ string, err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatalf("human action: %v", err)
	}
}

// Faults of the matrix.
var (
	simAuthErr      = &platform.Error{Op: "test", Class: platform.ClassAuth, Status: http.StatusUnauthorized, Err: errors.New("bad credentials")}
	simTransientErr = &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
)

// simFault is how the k-th write of a run goes wrong: it fails before or
// after the platform applies it, with err; or, with interrupt, the run is
// cancelled while the write is in flight (SIGTERM: the write finishes in
// its grace period, nothing after it starts).
type simFault struct {
	name      string
	applied   bool
	err       error
	interrupt bool
}

// simFaultKinds are the variants of every cut of the matrix. A 502 before
// the platform applies the write only makes the run try it again, which
// the executor's own tests cover.
var simFaultKinds = []simFault{
	{name: "before/401", err: simAuthErr},
	{name: "after/401", applied: true, err: simAuthErr},
	{name: "after/502", applied: true, err: simTransientErr},
	{name: "interrupt", interrupt: true},
}

func TestCrashMatrix(t *testing.T) {
	needDeliveryGit(t)
	simHeavy(t)
	for _, cc := range simCrashCases {
		t.Run(cc.name, func(t *testing.T) {
			t.Parallel()
			// A run without the fault: the writes to cut and where they lead.
			ref := newSimWorldOf(t, cc.flavor, cc.app, "api")
			cc.setup(ref)
			ref.p.ResetCalls()
			ref.run(ModeDistribute, cc.editFor(ref))
			writes := simWrites(ref.p.Writes())
			if len(writes) == 0 {
				t.Fatalf("the run of %s writes nothing", cc.name)
			}
			want := ref.digest()
			t.Logf("the writes of %s: %q", cc.name, writes)
			for k, call := range writes {
				for _, fault := range simFaultKinds {
					t.Run(fmt.Sprintf("%d-%s/%s", k, strings.Fields(call)[0], fault.name), func(t *testing.T) {
						t.Parallel()
						crashOnce(t, cc, k, fault, want)
					})
				}
			}
		})
	}
}

// simWrites returns the writes of a call log that count as one each: the
// labels a new pull request creates are part of it, and the update of the
// refs is part of its API commit.
func simWrites(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "CreateLabel ") && !strings.HasPrefix(c, "UpdateRefs ") {
			out = append(out, c)
		}
	}
	return out
}

// crashOnce builds the case's state again, cuts its k-th write with fault,
// and checks what the run and the runs after it leave against want, the
// digest of the run without the fault.
func crashOnce(t *testing.T, cc simCrashCase, k int, fault simFault, want simDigest) {
	w := newSimWorldOf(t, cc.flavor, cc.app, "api")
	cc.setup(w)
	s := &simScenario{simWorld: w, rng: rand.New(rand.NewPCG(1, 2))}
	s.c = newSimChecker(w, func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
	})
	s.c.observe()
	// A ticked rebuild or an operations.yml recreate lets the run drop
	// people's commits (I2): the checks must know it before the run.
	s.c.release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &simFaults{w: w, k: k, applied: fault.applied, err: fault.err}
	if fault.interrupt {
		f.cancel = cancel
	}
	remove := gitx.TraceCommands(f.trace)
	defer remove()
	before := s.c.snapshot()
	w.p.ResetCalls()
	var stream bytes.Buffer
	d := w.deps(ModeDistribute)
	if edit := cc.editFor(w); edit != nil {
		edit(&d)
	}
	d.Providers[0].Writer = &exWriter{Writer: w.p.Writer(w.writer), target: func(tw platform.TargetWriter) platform.TargetWriter {
		return exWrap(tw, f.api)
	}}
	d.Write.Stream = &stream
	rep, err := Run(ctx, d, ModeDistribute)
	remove()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	w.ok()
	checkReport(t, rep)
	if f.hit == "" {
		t.Fatalf("write %d never came (%s)", k, outcomes(rep))
	}
	checkStream(t, rep, stream.String())
	// Violations are read after the next run: a push to the branch of a
	// merged pull request is one only if no new pull request follows it.
	writes := w.p.Writes()
	s.c.checkWrites(writes, before)
	for _, op := range rep.Ops {
		if tg := w.byRef(op.Target); tg != nil && (op.Kind == "push" || op.Kind == "update-refs") {
			s.c.checkPush(tg, op, before)
		}
	}
	for _, tg := range w.targets {
		s.c.checkKept(tg)
		// The stage ref of an API commit never survives a run, whichever
		// write failed.
		if refs := w.p.HiddenRefs(tg.repo.ID); len(refs) > 0 {
			t.Errorf("the failed run left %q in %s", refs, tg.repo.Path)
		}
	}
	t.Logf("the failed run (%s at write %d): %s %q", f.hit, k, outcomes(rep), rep.Targets[0].Warnings)
	// The next runs converge.
	s.settle(cc.editFor(w))
	w.compare(t, want)
}

// simFaults fails the k-th write of a run (0-based): pull request writes
// through the per-target writer, pushes through gitx's command trace.
type simFaults struct {
	w       *simWorld
	k       int
	applied bool
	err     error

	// cancel, when set, cancels the run instead of arming a fault.
	cancel func()

	mu  sync.Mutex
	n   int
	hit string
}

// next counts a write of method about to happen and arms the fault on the
// k-th (or cancels the run).
func (f *simFaults) next(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == f.k {
		switch {
		case f.cancel != nil:
			f.cancel()
		case f.applied:
			f.w.p.FailNextApplied(method, f.err)
		default:
			f.w.p.FailNext(method, f.err)
		}
		f.hit = method
	}
	f.n++
}

// api counts the calls of the per-target writer that write.
func (f *simFaults) api(method string) {
	switch method {
	case "CreatePR", "EditPR", "Comment", "Commit":
		f.next(method)
	}
}

// trace counts the pushes of the world's target repositories.
func (f *simFaults) trace(args, env []string) {
	if !slices.Contains(args, "push") {
		return
	}
	dir := filepathSlash(f.w.src.Dir)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_DIR="); ok && strings.HasPrefix(filepathSlash(v), dir) {
			f.next("Push")
			return
		}
	}
}

// filepathSlash compares paths whatever separators they use.
func filepathSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// checkStream checks the report stream of a run: one line per target of
// its report, with the same outcome, reason and pull request.
func checkStream(t *testing.T, rep *report.Delivery, stream string) {
	t.Helper()
	checkStreamSchema(t, stream)
	lines := map[string]report.DeliveryTarget{}
	for _, line := range strings.Split(strings.TrimSuffix(stream, "\n"), "\n") {
		if line == "" {
			continue
		}
		var l streamLine
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			t.Errorf("stream line %q: %v", line, err)
			continue
		}
		if _, dup := lines[l.Target.Path]; dup {
			t.Errorf("%s streamed twice", l.Target.Path)
		}
		lines[l.Target.Path] = l.Target
	}
	for _, tg := range rep.Targets {
		got, ok := lines[tg.Path]
		switch {
		case !ok:
			t.Errorf("%s is not in the stream", tg.Path)
		case got.Outcome != tg.Outcome || got.Reason != tg.Reason || !reflect.DeepEqual(got.PR, tg.PR):
			t.Errorf("%s streamed %s:%s %+v, reported %s:%s %+v", tg.Path, got.Outcome, got.Reason, got.PR, tg.Outcome, tg.Reason, tg.PR)
		}
	}
	if len(lines) != len(rep.Targets) {
		t.Errorf("%d stream lines for %d targets", len(lines), len(rep.Targets))
	}
}

// simDigest is the state of a world a crash must converge to: every pull
// request of touchmark's (number, state, head, marker key and state fields)
// and every sync branch's head, by target; and the comments on each pull
// request.
type simDigest struct {
	prs      []string
	branches map[string]string
	comments map[string]int
}

// digest returns the digest of w now.
func (w *simWorld) digest() simDigest {
	d := simDigest{branches: map[string]string{}, comments: map[string]int{}}
	for _, tg := range w.targets {
		for _, pr := range w.prs(tg) {
			if pr.Author.ID != w.writer.ID {
				continue
			}
			m, status := marker.Find(pr.Body, []string{hubFP})
			closed, rf := "", ""
			if c := m.Data.Closed; c != nil {
				closed = c.By + "/" + c.Reason
			}
			if m.Data.RecreateFor != nil {
				rf = short(*m.Data.RecreateFor)
			}
			d.prs = append(d.prs, fmt.Sprintf("%s #%d %s head=%s on %s marker=%s key=%s closed=%s ack=%v revoked=%v recreate_for=%s title=%q",
				tg.name, pr.Number, pr.State, short(pr.HeadSHA), pr.Head, status, m.Key, closed, m.Data.Ack, m.Data.Revoked, rf, pr.Title))
			d.comments[fmt.Sprintf("%s #%d", tg.name, pr.Number)] = len(w.p.Comments(tg.repo.ID, pr.Number))
		}
		for _, b := range w.branches() {
			if head := w.p.Branch(tg.repo.ID, b); head != "" {
				d.branches[tg.name+" "+b] = head
			}
		}
	}
	return d
}

// compare checks that w converged to want: the same pull requests of
// touchmark's, the same sync branches (but for the branch of a closed pull
// request a failed run could not delete), and no comment posted more often.
func (w *simWorld) compare(t *testing.T, want simDigest) {
	t.Helper()
	got := w.digest()
	if !slices.Equal(got.prs, want.prs) {
		t.Errorf("the pull requests did not converge:\ngot  %s\nwant %s", strings.Join(got.prs, "\n     "), strings.Join(want.prs, "\n     "))
	}
	for _, key := range slices.Sorted(maps.Keys(got.branches)) {
		head, ok := want.branches[key]
		switch {
		case ok && head == got.branches[key]:
		case !ok && w.leftover(key):
			t.Logf("left over: %s at %s, which the run without the fault deleted", key, short(got.branches[key]))
		default:
			t.Errorf("branch %s is at %s, want %s", key, short(got.branches[key]), short(head))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(want.branches)) {
		if _, ok := got.branches[key]; !ok {
			t.Errorf("branch %s is gone, want it at %s", key, short(want.branches[key]))
		}
	}
	for key, n := range got.comments {
		if n > want.comments[key] {
			t.Errorf("%s has %d comments, the run without the fault posted %d", key, n, want.comments[key])
		}
	}
}

// leftover reports whether the branch "<target> <name>" carries only closed
// pull requests of touchmark's.
func (w *simWorld) leftover(key string) bool {
	name, branch, _ := strings.Cut(key, " ")
	tg := w.target(name)
	for _, pr := range w.prs(tg) {
		if pr.Head == branch && pr.HeadRepoID == tg.repo.ID && pr.State == platform.Open {
			return false
		}
	}
	return true
}
