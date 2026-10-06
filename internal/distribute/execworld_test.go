package distribute

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// The execute tests build works the way phase C does, with a small
// inspection of their own over the fake platform (exWorld.work), and run
// phase F on them (exWorld.execute). Most need the fake in git mode and a
// git that distribute supports; the others run on the fake in memory mode.

// The hub of the execute tests.
const (
	exFP        = "github.com/712345678"
	exHubCommit = "3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3"
	exBranch    = "touchmark/acme-eng"
	exAlias     = "chore/sync-engineering-assets"
	exOptIn     = ".engineering-assets.yml"
	exTitle     = "chore: sync engineering assets"
	exLabel     = "engineering-assets"
	// Tokens of the fake's accounts in git mode.
	exWriterToken = "writer-token-5f1c2a9e77"
	exPersonToken = "person-token-0b7d31aa42"
)

// exHubYML declares one GitHub provider whose writer is acme-write[bot].
const exHubYML = `version: 1
id: acme-eng
branch_aliases: [chore/sync-engineering-assets]
providers:
  - id: gh
    type: github
    writer: acme-write[bot]
`

// Hub files in two versions.
var (
	exAgentsV1 = "# AGENTS.md\n\nversion 1 of the shared agent instructions\n"
	exAgentsV2 = "# AGENTS.md\n\nversion 2 of the shared agent instructions\n"
	exGuide    = "# Guide\n\nthe shared engineering guide\n"
	exWorkflow = "name: lint\non: push\njobs: {}\n"
)

// exEpoch is where the executor's clock starts in the tests.
var exEpoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// exNeedGit skips a test on a git older than distribute supports.
func exNeedGit(t *testing.T) {
	t.Helper()
	v, err := (&gitx.Git{}).Version(t.Context())
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if slices.Compare(v[:], gitx.DeliveryMinVersion[:]) < 0 {
		m := gitx.DeliveryMinVersion
		t.Skipf("git %d.%d.%d is older than %d.%d, which distribute needs (the Docker run of the suite covers this test)", v[0], v[1], v[2], m[0], m[1])
	}
}

// exWorld is a fake platform with a writer and a person, a hub, and a run
// whose phase F the test drives.
type exWorld struct {
	t              *testing.T
	ctx            context.Context
	p              *fake.Platform
	git            bool
	writer, person platform.Account
	src            *snapshot.GitSource
	hub            *config.Hub
	reg            *redact.Registry
	stream         bytes.Buffer
	r              *run
	prov           *provider
	ex             *executor

	mu     sync.Mutex
	sleeps []time.Duration
	ticks  int
	// files are what the hub ships to every target: path → content.
	files map[string]string
	// order counts the targets made, for their targets.yml order.
	order int
}

// exConfig tunes a world.
type exConfig struct {
	flavor fake.Flavor
	git    bool
	hubYML string
}

// newExWorld builds a world; in git mode it serves the fake over HTTP.
func newExWorld(t *testing.T, c exConfig) *exWorld {
	t.Helper()
	if c.git {
		exNeedGit(t)
	}
	if c.flavor == "" {
		c.flavor = fake.GitHub
	}
	if c.hubYML == "" {
		c.hubYML = exHubYML
	}
	p := fake.New("github.com", fake.WithFlavor(c.flavor))
	w := &exWorld{
		t:      t,
		ctx:    t.Context(),
		p:      p,
		git:    c.git,
		writer: p.AddAccount("acme-write[bot]", platform.KindBot),
		person: p.AddAccount("jdoe", platform.KindUser),
		reg:    redact.New(),
		files:  map[string]string{"AGENTS.md": exAgentsV1, "docs/guide.md": exGuide},
	}
	if c.git {
		srv, err := p.ServeGit(t.TempDir())
		if err != nil {
			t.Fatalf("ServeGit: %v", err)
		}
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Errorf("close the git server: %v", err)
			}
		})
		p.SetToken(w.writer, exWriterToken)
		p.SetToken(w.person, exPersonToken)
		w.src = &snapshot.GitSource{Dir: t.TempDir(), Isolation: gitx.Isolation{Home: t.TempDir(), AllowHTTP: true}}
	}
	t.Cleanup(func() {
		if v := p.Violations(); len(v) > 0 {
			t.Errorf("forbidden transitions: %q", v)
		}
	})
	w.ok()
	hub, _, err := config.ParseHub([]byte(c.hubYML))
	if err != nil {
		t.Fatal(err)
	}
	w.hub = hub
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	wr := p.Writer(w.writer)
	w.prov = &provider{
		cfg:     rps[0],
		reader:  wr,
		writer:  wr,
		self:    w.writer,
		caps:    p.Caps(),
		authors: []platform.Account{w.writer},
		ids:     []string{w.writer.ID},
		info:    &report.ProviderInfo{ID: rps[0].ID},
		gate:    w.gate(rps[0].ID),
	}
	w.r = &run{
		d: Deps{
			Hub:       hub,
			HubCommit: exHubCommit,
			Engine:    "test",
			Write:     WriteDeps{Redact: w.reg, Stream: &w.stream},
		},
		rep:      report.NewDelivery("distribute", "test"),
		hub:      hub,
		targets:  &config.Targets{},
		optIn:    hub.OptInName(),
		branches: append([]string{hub.Branch}, hub.BranchAliases...),
		fps:      []string{exFP},
		provs:    []*provider{w.prov},
	}
	w.r.rep.Cost[w.prov.cfg.ID] = 0
	w.ex = newExecutor(w.r)
	w.ex.now = w.tick
	w.ex.sleep = w.sleep
	w.ex.jitter = func(d time.Duration) time.Duration { return d }
	// This run has no phase C to inspect a target again: a target that
	// moved is failed:race, unless a test inspects it with work.
	w.ex.reinspect = func(_ context.Context, wk *Work) *Work {
		wk.t.res.Outcome, wk.t.res.Reason = report.OutcomeFailed, "race"
		return nil
	}
	return w
}

// gate returns the throttle of a provider of the world: no limits, its
// pauses recorded as the executor's waits. Its clock stands at exEpoch and
// moves only by the Gate's own waits (throttle.Gate never reads a time
// before the end of its last wait), so a pause shows as one wait of its
// full length.
func (w *exWorld) gate(id string) *throttle.Gate {
	return throttle.New(throttle.Options{Name: id, Clock: throttle.Clock{Now: func() time.Time { return exEpoch }, Sleep: w.sleep}})
}

// ok fails the test on a setup error of the fake.
func (w *exWorld) ok() {
	w.t.Helper()
	if err := w.p.Err(); err != nil {
		w.t.Fatalf("setup: %v", err)
	}
}

// tick is the executor's clock: a second later at every reading.
func (w *exWorld) tick() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ticks++
	return exEpoch.Add(time.Duration(w.ticks) * time.Second)
}

// sleep records the executor's waits and returns at once.
func (w *exWorld) sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.sleeps = append(w.sleeps, d)
	w.mu.Unlock()
	return ctx.Err()
}

// slept returns the waits recorded since the last call.
func (w *exWorld) slept() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.sleeps
	w.sleeps = nil
	return out
}

// target adds a repository with files ("path", "content" pairs) the writer
// may write to, and returns it as a target of the run.
func (w *exWorld) target(path string, files ...string) *target {
	w.t.Helper()
	r := w.p.AddRepo(platform.Repo{Path: path})
	for i := 0; i+1 < len(files); i += 2 {
		w.p.SetFile(r.ID, files[i], []byte(files[i+1]), "")
	}
	w.p.GrantWrite(r.ID, w.writer)
	w.ok()
	repo, _ := w.p.RepoByID(r.ID)
	w.order++
	t := &target{prov: w.prov, repo: repo, host: repo.Host, first: [2]int{0, w.order}}
	t.res = report.DeliveryTarget{Provider: w.prov.cfg.ID, Host: repo.Host, RepoID: repo.ID, Path: repo.Path, Packs: []string{"base"}}
	return t
}

// exState is what exWorld.work inspected, for tests that change it.
type exState struct {
	in       decide.TargetInput
	fresh    []platform.PR
	optInSHA string
}

// work inspects t the way phase C does and returns its work: B and its
// tree, D against the hub's files, the commit, the classified sync
// branches, the own and foreign pull requests, memory and the decision.
// edit changes the decision's input before DecideTarget.
func (w *exWorld) work(t *target, edit func(*exState)) *Work {
	w.t.Helper()
	ctx := w.ctx
	repo, ok := w.p.RepoByID(t.repo.ID)
	if !ok {
		w.t.Fatalf("no repository %s", t.repo.ID)
	}
	t.repo = repo
	wk := &Work{t: t, DefaultBranch: repo.DefaultBranch}
	entries := map[string]snapshot.Entry{}
	if w.git {
		remote, err := w.prov.reader.Remote(ctx, repo)
		exMust(w.t, err)
		tr, err := w.src.Repo(ctx, repo, remote)
		exMust(w.t, err)
		wk.Repo = tr
		b, found, err := tr.FetchBranch(ctx, repo.DefaultBranch, 1)
		if err != nil || !found {
			w.t.Fatalf("fetch the default branch: %v, found %v", err, found)
		}
		wk.B = b
		list, err := tr.Tree(ctx, b)
		exMust(w.t, err)
		for _, e := range list {
			entries[e.Path] = snapshot.Entry{Mode: e.Mode, OID: e.OID}
		}
	} else {
		wk.B = w.p.Head(repo.ID)
		tree, err := w.p.Snapshots().Snapshot(ctx, repo, platform.Remote{}, "")
		exMust(w.t, err)
		entries = tree.Entries
	}
	wk.Tree = &snapshot.Tree{Commit: wk.B, Entries: entries}
	for _, path := range slices.Sorted(maps.Keys(w.files)) {
		to := gitx.RawOID([]byte(w.files[path]))
		from := decide.ZeroOID
		if e, ok := entries[path]; ok {
			if e.OID == to {
				continue
			}
			from = e.OID
		}
		wk.D = append(wk.D, decide.Pair{Path: path, From: from, Mode: "100644", To: to})
		action := decide.Create
		if from != decide.ZeroOID {
			action = decide.Update
		}
		wk.Plan.Entries = append(wk.Plan.Entries, decide.Entry{Path: path, Action: action, Pack: "base", From: from, To: to, Mode: "100644"})
	}
	if len(wk.D) > 0 {
		wk.Key = decide.Key(decide.StreamSync, wk.D)
	}
	optIn := &config.OptIn{Version: 1}
	wk.OptInHash = optIn.Hash()
	st := &exState{optInSHA: wk.OptInHash}
	in := &st.in
	in.Stream, in.D, in.Key, in.B, in.DefaultBranch = decide.StreamSync, wk.D, wk.Key, wk.B, wk.DefaultBranch
	in.Branch = decide.Branch{Name: exBranch}
	if w.git {
		in.Branch = w.classify(wk.Repo, exBranch, wk.DefaultBranch, wk.B)
	}
	in.Aliases = map[string]decide.Branch{}
	prs, err := w.prov.reader.PRs(ctx, repo, w.r.branches, w.prov.authors)
	exMust(w.t, err)
	st.fresh = prs
	wk.listed = seenAll(prs, repo, w.r.fps)
	id := decide.Identity{Branches: w.r.branches, Authors: w.prov.ids, Fingerprints: w.r.fps}
	for _, pr := range prs {
		m, status := id.Own(pr)
		switch {
		case status == decide.Ours:
			in.Own = append(in.Own, decide.OwnPR{PR: pr, Marker: m, Alias: pr.Head != exBranch})
			if pr.State == platform.Open && pr.Head != exBranch && w.git {
				in.Aliases[pr.Head] = w.classify(wk.Repo, pr.Head, wk.DefaultBranch, wk.B)
			}
		case status == decide.OursMarkerInvalid && pr.State == platform.Open:
			in.MarkerInvalid = append(in.MarkerInvalid, pr)
		case pr.State == platform.Open && fromTargetRepo(pr, repo):
			in.ForeignOpen = append(in.ForeignOpen, pr)
		}
	}
	repropose := map[int64]bool{}
	for _, o := range in.Own {
		if o.PR.State == platform.Closed && prbody.Ticked(o.PR.Body, prbody.ControlRepropose) {
			repropose[o.PR.Number] = true
		}
	}
	in.Memory = decide.BuildMemory(decide.MemoryInput{
		Own:       in.Own,
		OptIn:     wk.OptInHash,
		Repropose: repropose,
		Now:       exEpoch,
		Config:    decide.MemoryConfig{Writers: map[string]bool{w.writer.ID: true}, CloserKnown: w.prov.caps.CloserKnown},
	})
	in.CooldownUntil, in.CooldownDeclined = in.Memory.Cooldown(wk.Key, exEpoch, 0)
	in.PlatformWorkflowPerm, in.CanWorkflows = w.prov.caps.WorkflowPerm, true
	in.Now = exEpoch
	for _, o := range in.Own {
		if o.PR.State == platform.Open && prbody.Ticked(o.PR.Body, prbody.ControlRecreate) {
			in.RecreateTicked = true
		}
	}
	if edit != nil {
		edit(st)
	}
	wk.Branch, wk.Aliases, wk.Own, wk.Memory = in.Branch, in.Aliases, in.Own, in.Memory
	wk.Decision = decide.DecideTarget(*in)
	if n := wk.Decision.PR; n > 0 {
		for i := range in.Own {
			if o := &in.Own[i]; o.PR.Number == n && o.PR.State == platform.Open {
				wk.Open = o
			}
		}
	}
	if len(wk.D) > 0 && w.git {
		wk.Built = w.build(wk)
	}
	packs := map[string]string{}
	for _, e := range wk.Plan.Entries {
		packs[e.Path] = e.Pack
	}
	wk.Body = prbody.Input{HubName: "acme-eng", ContentCommit: exHubCommit, Packs: []string{"base"}, OptInFile: exOptIn,
		Changes: rowsOf(wk.D, packs), Caps: w.prov.caps}
	wk.Marker = marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: exFP,
		DecidedAt: exHubCommit, ContentCommit: exHubCommit, Base: wk.B, OptIn: wk.OptInHash, Engine: "test",
		Packs: []string{"base"}, Changes: decide.ShortChanges(wk.D), ChangesComplete: true,
	}
	wk.NeedPerms = needPerms(wk.Decision.Steps)
	t.res.Outcome, t.res.Reason, t.res.PR = report.Outcome(wk.Decision.Outcome), wk.Decision.Reason, nil
	if n := wk.Decision.PR; n > 0 {
		t.res.PR = &report.PRRef{Number: n}
	}
	t.res.Key, t.res.Warnings, t.res.Writes = wk.Key, nil, len(wk.Decision.Steps)
	return wk
}

// classify reads a sync branch of tr and classifies it against the default
// branch base at b, for the simple histories of these tests: commits of
// touchmark's on B and what people added on top.
func (w *exWorld) classify(tr *gitx.TargetRepo, name, base, b string) decide.Branch {
	w.t.Helper()
	ctx := w.ctx
	head, found, err := tr.FetchBranch(ctx, name, decide.MaxHistory+1)
	exMust(w.t, err)
	if !found {
		return decide.Branch{Name: name}
	}
	log, shallow, err := tr.FirstParentLog(ctx, head, decide.MaxHistory)
	exMust(w.t, err)
	var commits []decide.HistoryCommit
	for _, c := range log {
		commits = append(commits, decide.HistoryCommit{SHA: c.SHA, Parents: c.Parents, Message: c.Message})
	}
	at, _ := decide.FindHc(commits, w.r.fps, decide.StreamSync)
	if at >= 0 && len(commits[at].Parents) == 1 {
		hc := &commits[at]
		diff, err := tr.DiffTree(ctx, hc.Parents[0], hc.SHA)
		exMust(w.t, err)
		for _, e := range diff {
			p := decide.Pair{Path: e.Path, From: e.OldOID, Mode: e.NewMode, To: e.NewOID}
			if e.NewMode == decide.ModeDelete {
				p.Mode = decide.ModeDelete
			}
			hc.Pairs = append(hc.Pairs, p)
		}
		hc.ParentBaseAncestor = decide.TriNo
		if parent := hc.Parents[0]; parent != b {
			// Deepen the base to the parent's date, as phase C proves it.
			c, err := tr.Commit(ctx, parent)
			exMust(w.t, err)
			exMust(w.t, tr.DeepenSince(ctx, base, c.Time.Add(-24*time.Hour)))
		}
		if in, err := tr.IsAncestor(ctx, hc.Parents[0], b); err == nil && in {
			hc.ParentBaseAncestor = decide.TriYes
		}
	}
	return decide.ClassifyBranch(decide.BranchHistory{
		Name: name, Exists: true, Head: head, Base: b, Commits: commits, Truncated: shallow && at < 0,
	}, w.r.fps, decide.StreamSync)
}

// build builds the commit of D on B as phase C does.
func (w *exWorld) build(wk *Work) gitx.Built {
	w.t.Helper()
	msg, err := prbody.CommitMessage(w.hub.Commit.Message, decide.FormatTrailers(decide.Trailers{
		HubID: "acme-eng", Fingerprint: exFP, Stream: decide.StreamSync, Content: wk.Key, HubCommit: exHubCommit,
	}))
	exMust(w.t, err)
	blobs := map[string]gitx.Blob{}
	var changes []gitx.Change
	for _, p := range wk.D {
		if p.Mode == decide.ModeDelete {
			changes = append(changes, gitx.Change{Path: p.Path})
			continue
		}
		changes = append(changes, gitx.Change{Path: p.Path, Mode: p.Mode, OID: p.To})
		content := w.content(p.To)
		blobs[p.To] = gitx.Blob{OID: p.To, Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(content)), nil
		}}
	}
	who := gitx.Person{Name: w.writer.Login, Email: w.writer.Email}
	built, err := wk.Repo.BuildCommit(w.ctx, gitx.CommitSpec{
		Parent: wk.B, Changes: changes, Blobs: blobs, Author: who, Committer: who,
		When: exEpoch, Message: msg,
	})
	exMust(w.t, err)
	return built
}

// content returns the hub file with blob id oid.
func (w *exWorld) content(oid string) string {
	for _, c := range []string{exAgentsV1, exAgentsV2, exGuide, exWorkflow} {
		if gitx.RawOID([]byte(c)) == oid {
			return c
		}
	}
	for _, c := range w.files {
		if gitx.RawOID([]byte(c)) == oid {
			return c
		}
	}
	w.t.Fatalf("no hub file with blob %s", oid)
	return ""
}

// execute runs phase F over works and fails the test on a setup error.
func (w *exWorld) execute(works ...*Work) {
	w.t.Helper()
	w.ex.run(w.ctx, works)
	w.ok()
}

// pr returns a pull request of t as the platform holds it.
func (w *exWorld) pr(t *target, n int64) platform.PR {
	w.t.Helper()
	pr := w.p.PR(t.repo.ID, n)
	if pr.Number != n {
		w.t.Fatalf("%s has no pull request #%d", t.repo.Path, n)
	}
	return pr
}

// marker returns the valid marker of a pull request body, failing the test
// without one.
func (w *exWorld) marker(body string) marker.Marker {
	w.t.Helper()
	m, status := marker.Find(body, w.r.fps)
	if status != marker.Found {
		w.t.Fatalf("no valid marker (%s) in:\n%s", status, body)
	}
	return m
}

// addPR adds a pull request to t.
func (w *exWorld) addPR(t *target, pr platform.PR) int64 {
	w.t.Helper()
	n := w.p.AddPR(t.repo.ID, pr)
	w.ok()
	return n
}

// push is the person's push of files ("path", "content" pairs) to branch.
func (w *exWorld) push(t *target, branch string, files ...string) string {
	w.t.Helper()
	m := map[string][]byte{}
	for i := 0; i+1 < len(files); i += 2 {
		m[files[i]] = []byte(files[i+1])
	}
	head, err := w.p.PushFiles(t.repo.ID, branch, m, w.person, time.Time{})
	exMust(w.t, err)
	return head
}

// exWant checks a target's outcome, reason and pull request number.
func exWant(t *testing.T, tg *target, outcome report.Outcome, reason string, pr int64) {
	t.Helper()
	var got int64
	if tg.res.PR != nil {
		got = tg.res.PR.Number
	}
	if tg.res.Outcome != outcome || tg.res.Reason != reason || got != pr {
		t.Errorf("%s: %s:%s #%d, want %s:%s #%d (warnings %q)", tg.repo.Path, tg.res.Outcome, tg.res.Reason, got, outcome, reason, pr, tg.res.Warnings)
	}
}

// opKinds returns the kinds of the report's ops for target t.
func (w *exWorld) opKinds(t *target) []string {
	var kinds []string
	for _, op := range w.r.rep.Ops {
		if op.Target == refOf(t) {
			kinds = append(kinds, op.Kind)
		}
	}
	return kinds
}

// streamed returns the stream's lines by target path.
func (w *exWorld) streamed() map[string]streamLine {
	w.t.Helper()
	out := map[string]streamLine{}
	for _, line := range strings.Split(strings.TrimSuffix(w.stream.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var l streamLine
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			w.t.Fatalf("stream line %q: %v", line, err)
		}
		out[l.Target.Path] = l
	}
	return out
}

// exHasWarn reports whether some warning of t contains s.
func exHasWarn(t *target, s string) bool {
	return slices.ContainsFunc(t.res.Warnings, func(w string) bool { return strings.Contains(w, s) })
}

func exMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// exWriter wraps the write identity of a world to change what the platform
// answers, or to act between the executor's calls.
type exWriter struct {
	platform.Writer
	// onPRs runs once, after the next listing of pull requests.
	onPRs func()
	// prs rewrites every listing of pull requests.
	prs func([]platform.PR) []platform.PR
	// target wraps the per-target writers Target mints.
	target func(platform.TargetWriter) platform.TargetWriter
	// targetFails fails Target with this error, without effect.
	targetFails error
}

func (x *exWriter) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	prs, err := x.Writer.PRs(ctx, r, heads, authors)
	if f := x.onPRs; f != nil {
		x.onPRs = nil
		f()
	}
	if err == nil && x.prs != nil {
		prs = x.prs(prs)
	}
	return prs, err
}

func (x *exWriter) Target(ctx context.Context, r platform.Repo, need platform.Perms) (platform.TargetWriter, error) {
	if x.targetFails != nil {
		return nil, x.targetFails
	}
	tw, err := x.Writer.Target(ctx, r, need)
	if err == nil && x.target != nil {
		tw = x.target(tw)
	}
	return tw, err
}

// wrap puts an exWriter over the world's write identity and returns it.
func (w *exWorld) wrap() *exWriter {
	x := &exWriter{Writer: w.p.Writer(w.writer)}
	w.prov.reader, w.prov.writer = x, x
	return x
}

// exTargetWriter wraps a per-target writer: before runs before each of its
// calls (by method name), and the answers of CreatePR and EditPR can be
// rewritten.
type exTargetWriter struct {
	platform.TargetWriter
	before func(method string)
	answer func(platform.PR) platform.PR
}

// exWrap wraps tw in an exTargetWriter that runs before ahead of each
// write, and that commits through the API when tw does.
func exWrap(tw platform.TargetWriter, before func(method string)) platform.TargetWriter {
	x := &exTargetWriter{TargetWriter: tw, before: before}
	if _, ok := tw.(platform.Committer); ok {
		return exCommitter{x}
	}
	return x
}

// exCommitter is an exTargetWriter over a platform.Committer.
type exCommitter struct{ *exTargetWriter }

func (x exCommitter) Commit(ctx context.Context, r platform.Repo, req platform.CommitRequest) (platform.Commit, error) {
	x.call("Commit")
	return x.TargetWriter.(platform.Committer).Commit(ctx, r, req)
}

// Preflight is the writer's when it reads rules; nothing is known
// otherwise.
func (x *exWriter) Preflight(ctx context.Context, r platform.Repo, branches []string) (platform.Rules, error) {
	if pf, ok := x.Writer.(platform.Preflighter); ok {
		return pf.Preflight(ctx, r, branches)
	}
	return platform.Rules{}, nil
}

func (x *exTargetWriter) call(method string) {
	if x.before != nil {
		x.before(method)
	}
}

func (x *exTargetWriter) CreatePR(ctx context.Context, np platform.NewPR) (platform.PR, error) {
	x.call("CreatePR")
	pr, err := x.TargetWriter.CreatePR(ctx, np)
	if err == nil && x.answer != nil {
		pr = x.answer(pr)
	}
	return pr, err
}

func (x *exTargetWriter) EditPR(ctx context.Context, n int64, e platform.PREdit) (platform.PR, error) {
	x.call("EditPR")
	pr, err := x.TargetWriter.EditPR(ctx, n, e)
	if err == nil && x.answer != nil {
		pr = x.answer(pr)
	}
	return pr, err
}

func (x *exTargetWriter) Comment(ctx context.Context, n int64, body string) error {
	x.call("Comment")
	return x.TargetWriter.Comment(ctx, n, body)
}

// ownBody is the body of a pull request of touchmark's over pairs: a line of
// text and the marker of this hub.
func (w *exWorld) ownBody(pairs []decide.Pair, edit func(*marker.Data)) string {
	w.t.Helper()
	d := marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: exFP,
		DecidedAt: exHubCommit, ContentCommit: exHubCommit, Engine: "test", Packs: []string{"base"},
		Changes: decide.ShortChanges(pairs), ChangesComplete: true,
		TitleSet: exTitle, LabelsSet: []string{exLabel},
	}
	if edit != nil {
		edit(&d)
	}
	line, err := marker.Encode(marker.Marker{Key: decide.Key(decide.StreamSync, pairs), Data: d})
	exMust(w.t, err)
	return "Engineering assets from the hub.\n\n" + line
}

// missing returns D of a target that holds none of the hub's files.
func (w *exWorld) missing() []decide.Pair {
	var pairs []decide.Pair
	for _, path := range slices.Sorted(maps.Keys(w.files)) {
		pairs = append(pairs, decide.Pair{Path: path, From: decide.ZeroOID, Mode: "100644", To: gitx.RawOID([]byte(w.files[path]))})
	}
	return pairs
}
