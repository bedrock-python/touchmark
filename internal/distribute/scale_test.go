package distribute

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// The scale tests run the whole of distribute (Run, phases A to G) over a
// fleet of thousands of targets on two fake platforms, a GitHub and a
// GitLab, in memory mode. No git can serve such a fleet in a test, so the
// targets' repositories are a model (fleetGit, through Deps.git): each
// target's sync branch is absent, touchmark's own with the content its pull
// request names, or a person's. Every other step is the real one: resolve,
// opt-in files, snapshots, pull requests, memory, decisions, the sweep, the
// gate, the write queues, the throttle and the report. Nothing a run decides
// may push: the model's branches already hold the content of every target
// that needs a pull request (a run that pushed and stopped), so a push would
// fail the target (failed:internal), which the test refuses.
//
// Each provider runs on a fake clock of its own (Deps.clocks): its Gate
// waits on it and its write queue reads it, so its calls are timed on its
// own timeline while both queues write at once, and the fake platforms
// stamp every request with it (fake.WithRequestLog). The platforms answer
// as busy ones do (fleetFaults, seeded): rate limits with and without
// Retry-After, GitHub's secondary limits, server errors, timeouts with the
// answer lost after the platform acted, and answers whose headers say the
// budget is spent; and, in some runs, storms: a provider that keeps
// limiting the rate, a credential refused for target after target.

// The fleet's hub.
const (
	fleetTitle    = "chore: sync engineering assets"
	fleetOldTitle = "chore: sync shared files"
	// fleetRunLength is a run's deadline: GitHub Actions' default.
	fleetRunLength = 5*time.Hour + 30*time.Minute
	// fleetMaxNew is the hub's limits.max_new_prs_per_run.
	fleetMaxNew = 500
	// fleetSpareRuns are the runs the fleet may take to finish beyond those
	// the rollout limit needs: the run that writes nothing, and those the
	// storms and the deadline cost.
	fleetSpareRuns = 6
	// fleetSeed seeds the fleet and its faults.
	fleetSeed = 20261002
)

// fleetHubYML is the hub of the fleet: a GitHub and a GitLab provider.
const fleetHubYML = `version: 1
id: acme-eng
branch_aliases: [chore/sync-engineering-assets]
providers:
  - id: gh
    type: github
    writer: acme-write[bot]
  - id: gl
    type: gitlab
    url: https://gitlab.example.com
    writer: acme-write[bot]
limits:
  max_new_prs_per_run: 500
  max_close_fraction: 0.5
`

// fleetTargetsYML gives base to every repository of acme on both
// providers, and python to those with the python topic.
const fleetTargetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - org: acme
    provider: gh
  - org: acme
    provider: gl
  - org: acme
    provider: gh
    topics: [python]
    packs: [python]
  - org: acme
    provider: gl
    topics: [python]
    packs: [python]
`

// fleetKind is what a repository of the fleet is at the start.
type fleetKind string

const (
	// fleetFresh: opted in, no pull request; the sync branch holds the
	// content already.
	fleetFresh fleetKind = "fresh"
	// fleetCurrent: an open pull request of the content, whose body an
	// older touchmark wrote.
	fleetCurrent fleetKind = "current"
	// fleetRetitle: an open pull request with the title an older pr.title
	// set.
	fleetRetitle fleetKind = "retitle"
	// fleetDeclined: a pull request of the content a person closed.
	fleetDeclined fleetKind = "declined"
	// fleetSettled: the team took the files; its pull request is open.
	fleetSettled fleetKind = "settled"
	// fleetOptedOut: the opt-in file is gone; the pull request is open.
	fleetOptedOut fleetKind = "opted-out"
	// fleetDropped: an open pull request in a repository targets.yml does
	// not select; fleetHidden the same in a private one, which a public hub
	// never names.
	fleetDropped fleetKind = "dropped"
	fleetHidden  fleetKind = "hidden-dropped"
	// fleetNotOpted: no opt-in file; fleetArchived: archived; fleetInvalid:
	// an opt-in file that asks for an unknown pack.
	fleetNotOpted fleetKind = "not-opted-in"
	fleetArchived fleetKind = "archived"
	fleetInvalid  fleetKind = "invalid"
	// fleetForeign: a person's open pull request on the sync branch.
	fleetForeign fleetKind = "foreign"
	// fleetPrivate: a private repository of acme, which the public hub
	// skips without naming it.
	fleetPrivate fleetKind = "private"
)

// fleetMix is the share of each kind, in percent.
var fleetMix = []struct {
	kind   fleetKind
	weight int
}{
	{fleetFresh, 45}, {fleetCurrent, 16}, {fleetRetitle, 6}, {fleetDeclined, 6}, {fleetSettled, 5},
	{fleetOptedOut, 4}, {fleetDropped, 2}, {fleetHidden, 1}, {fleetNotOpted, 6}, {fleetArchived, 3},
	{fleetInvalid, 2}, {fleetForeign, 2}, {fleetPrivate, 2},
}

// fleetTarget is one repository of the fleet.
type fleetTarget struct {
	prov   *fleetProv
	kind   fleetKind
	repo   platform.Repo
	python bool
	// pr is the pull request the setup made (0 for none); foreign is the
	// person's (fleetForeign), as it was made.
	pr      int64
	foreign platform.PR
	// key is the content key of what the target lacks (base, and python
	// with the topic).
	key string
}

// ref is the target as the report names it.
func (st *fleetTarget) ref() string { return st.prov.id + ":" + st.repo.Path }

// fleetProv is one provider of the fleet: a fake platform on a clock of
// its own.
type fleetProv struct {
	id                            string
	host                          string
	flavor                        fake.Flavor
	p                             *fake.Platform
	clock                         *fakeClock
	faults                        *fleetFaults
	reader, writer, person, other platform.Account
}

// fleetWorld is the fleet and its hub.
type fleetWorld struct {
	t       *testing.T
	provs   []*fleetProv
	fleet   []*fleetTarget
	byRef   map[string]*fleetTarget
	git     fleetGit
	hub     *config.Hub
	targets *config.Targets
	rps     []config.ResolvedProvider
}

// newFleet builds a fleet of quota[i] targets in acme on each
// provider (GitHub, then GitLab), the kinds drawn from fleetMix, plus the
// repositories outside acme that the draw makes dropped.
func newFleet(t *testing.T, quota [2]int) *fleetWorld {
	t.Helper()
	w := &fleetWorld{t: t, byRef: map[string]*fleetTarget{}, git: fleetGit{branches: map[string]decide.Branch{}}}
	var err error
	if w.hub, _, err = config.ParseHub([]byte(fleetHubYML)); err != nil {
		t.Fatal(err)
	}
	if w.targets, _, err = config.ParseTargets([]byte(fleetTargetsYML)); err != nil {
		t.Fatal(err)
	}
	if w.rps, err = w.hub.ResolveProviders(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	for i, spec := range []struct {
		id, host string
		flavor   fake.Flavor
		prefix   string
	}{
		{"gh", "github.com", fake.GitHub, "svc"},
		{"gl", "gitlab.example.com", fake.GitLab, "lib"},
	} {
		clock := newFakeClock(start)
		p := fake.New(spec.host, fake.WithFlavor(spec.flavor), fake.WithClock(clock.Now), fake.WithRequestLog())
		sp := &fleetProv{id: spec.id, host: spec.host, flavor: spec.flavor, p: p, clock: clock,
			reader: p.AddAccount("acme-read[bot]", platform.KindBot),
			writer: p.AddAccount("acme-write[bot]", platform.KindBot),
			person: p.AddAccount("alice", platform.KindUser),
			other:  p.AddAccount("bob", platform.KindUser),
		}
		sp.faults = newFleetFaults(sp)
		p.Inject(sp.faults.inject)
		w.provs = append(w.provs, sp)
		rng := rand.New(rand.NewPCG(fleetSeed, uint64(i)))
		for n, made := 0, 0; made < quota[i]; n++ {
			st := w.add(sp, rng, spec.prefix, n)
			if st.kind != fleetDropped && st.kind != fleetHidden {
				made++
			}
		}
		if err := p.Err(); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return w
}

// fleetQuota is the fleet's size: TOUCHMARK_SCALE_TARGETS targets (5000 by
// default; the nightly job runs more), three fifths on GitHub and two on
// GitLab; a tenth with -short.
func fleetQuota(t *testing.T) [2]int {
	t.Helper()
	n := 5000
	if v := os.Getenv("TOUCHMARK_SCALE_TARGETS"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 50 {
			t.Fatalf("TOUCHMARK_SCALE_TARGETS=%q: want a number of targets, 50 at least", v)
		}
	}
	if testing.Short() {
		n /= 10
	}
	return [2]int{n * 3 / 5, n - n*3/5}
}

// draw picks a kind from fleetMix.
func draw(rng *rand.Rand) fleetKind {
	n := rng.IntN(100)
	for _, m := range fleetMix {
		if n < m.weight {
			return m.kind
		}
		n -= m.weight
	}
	return fleetFresh
}

// fleetPairs are the pairs of what the base pack, and the python pack with
// python, bring to a target that holds none of their files, by path.
func fleetPairs(python bool) []decide.Pair {
	files := slices.Clone(baseFiles)
	if python {
		files = append(files, "docs/python.md", pythonV1)
	}
	var pairs []decide.Pair
	for i := 0; i+1 < len(files); i += 2 {
		pairs = append(pairs, decide.Pair{Path: files[i], From: decide.ZeroOID, Mode: "100644", To: oid(files[i+1])})
	}
	slices.SortFunc(pairs, func(a, b decide.Pair) int { return cmp.Compare(a.Path, b.Path) })
	return pairs
}

// add makes repository n of sp, of a kind drawn from rng.
func (w *fleetWorld) add(sp *fleetProv, rng *rand.Rand, prefix string, n int) *fleetTarget {
	kind := draw(rng)
	python := rng.IntN(10) == 0
	st := &fleetTarget{prov: sp, kind: kind, python: python}
	ns, vis := "acme", "public"
	switch kind {
	case fleetDropped:
		ns = "acme-old"
	case fleetHidden:
		ns, vis = "acme-old", "private"
	case fleetPrivate:
		vis = "private"
	}
	r := platform.Repo{Path: fmt.Sprintf("%s/%s-%04d", ns, prefix, n), Visibility: vis, Archived: kind == fleetArchived}
	if python {
		r.Topics = []string{"python"}
	}
	p := sp.p
	r = p.AddRepo(r)
	p.SetFile(r.ID, "README.md", []byte("# "+r.Path+"\n"), "")
	switch kind {
	case fleetNotOpted, fleetOptedOut:
	case fleetInvalid:
		p.SetFile(r.ID, optInName, []byte("version: 1\npacks: [no-such-pack]\n"), "")
	default:
		p.SetFile(r.ID, optInName, []byte("version: 1\n"), "")
	}
	pairs := fleetPairs(python)
	st.key = decide.Key(decide.StreamSync, pairs)
	if kind == fleetSettled {
		for _, pr := range pairs {
			content := map[string]string{"AGENTS.md": agentsV2, "docs/guide.md": guideV1, "docs/python.md": pythonV1}[pr.Path]
			p.SetFile(r.ID, pr.Path, []byte(content), "")
		}
	}
	p.GrantWrite(r.ID, sp.writer)
	switch kind {
	case fleetCurrent, fleetRetitle, fleetDeclined, fleetSettled, fleetOptedOut, fleetDropped, fleetHidden:
		title := fleetTitle
		if kind == fleetRetitle {
			title = fleetOldTitle
		}
		packs := []string{"base"}
		if python {
			packs = append(packs, "python")
		}
		st.pr = p.AddPR(r.ID, platform.PR{Head: branch, Author: sp.writer, Title: title, Labels: []string{"engineering-assets"},
			Body: markerBody(w.t, st.key, hubFP, decide.ShortChanges(pairs), func(d *marker.Data) {
				d.Packs, d.TitleSet = packs, title
			})})
		if kind == fleetDeclined {
			p.SetPRState(r.ID, st.pr, platform.Closed, &sp.person, time.Time{})
		}
	case fleetForeign:
		n := p.AddPR(r.ID, platform.PR{Head: branch, Author: sp.other, Title: "wip: my own sync", Body: "Work in progress."})
		st.foreign = p.PR(r.ID, n)
	}
	st.repo, _ = p.RepoByID(r.ID)
	key := sp.host + "/" + r.ID
	switch kind {
	case fleetFresh, fleetCurrent, fleetRetitle, fleetDeclined, fleetSettled:
		head := fleetHead(r.ID, st.key)
		w.git.branches[key] = decide.Branch{Head: head, State: decide.BranchRewritable, Hc: head, C: pairs, CKey: st.key, HcIsHead: true}
	case fleetForeign:
		w.git.branches[key] = decide.Branch{Head: fleetHead(r.ID, "foreign"), State: decide.BranchForeign, Detail: "commits of bob"}
	}
	w.fleet = append(w.fleet, st)
	w.byRef[st.ref()] = st
	return st
}

// fleetHead is a commit id of the model's branches.
func fleetHead(parts ...string) string {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
	}
	return fmt.Sprintf("%016x%016x%08x", h.Sum64(), h.Sum64()^0x5ca1e, uint32(h.Sum64()))
}

// fleetGit is the model of the fleet's repositories (Deps.git): step 8
// plans D without a commit, and step 6 reads each target's sync branch
// from the model, which no run changes (no work of the fleet pushes).
type fleetGit struct {
	// branches are the sync branches by host/repository id; the default
	// branch they are based on is B, whatever it is now.
	branches map[string]decide.Branch
}

func (g fleetGit) settle(_ context.Context, r *run, w *Work, optIn *config.OptIn, sel config.Selection) error {
	w.Plan = decide.Decide(r.decideInput(optIn, sel, w.Tree, nil))
	w.D = decide.Pairs(w.Plan)
	w.Key, w.Built = "", gitx.Built{}
	if len(w.D) > 0 {
		w.Key = decide.Key(decide.StreamSync, w.D)
	}
	return nil
}

func (g fleetGit) readBranches(_ context.Context, r *run, w *Work, names []string, _ bool, _ string) (branchReads, error) {
	reads := branchReads{branches: map[string]decide.Branch{}, hubCommits: map[string]string{}}
	for _, name := range names {
		b, ok := g.branches[w.t.host+"/"+w.t.repo.ID]
		if !ok || name != r.branches[0] {
			reads.branches[name] = decide.Branch{Name: name}
			continue
		}
		b.Name = name
		b.C = slices.Clone(b.C)
		if b.State == decide.BranchRewritable {
			b.E, b.HcParent = w.B, w.B
			reads.hubCommits[name] = hubCommit
		}
		reads.branches[name] = b
	}
	return reads, nil
}

// fleetRepos are the fleet's snapshots, by host; there are no
// repositories to hand out (fleetGit stands in for them).
type fleetRepos struct{ sources }

func (fleetRepos) Repo(context.Context, platform.Repo, platform.Remote) (*gitx.TargetRepo, error) {
	return nil, errors.New("the scale world has no git repositories")
}

func (fleetRepos) Release(platform.Repo) error { return nil }

// deps returns the dependencies of a run in mode with deadline: the
// writers, or in a plan the readers and writers that fail the test.
func (w *fleetWorld) deps(mode Mode, deadline time.Time) Deps {
	srcs := sources{}
	clocks := map[string]throttle.Clock{}
	var providers []Provider
	for i, sp := range w.provs {
		srcs[sp.host] = sp.p.Snapshots()
		clocks[sp.id] = throttle.Clock{Now: sp.clock.Now, Sleep: sp.clock.Sleep}
		prov := Provider{Config: w.rps[i]}
		if mode == ModePlan {
			prov.Reader, prov.Writer = sp.p.Reader(sp.reader), forbiddenWriter{t: w.t}
		} else {
			prov.Writer = sp.p.Writer(sp.writer)
		}
		providers = append(providers, prov)
	}
	m, cur := manifestAndCurrent()
	return Deps{
		Hub:         w.hub,
		Targets:     w.targets,
		Manifest:    m,
		Current:     cur,
		Known:       map[string]bool{"base": true, "python": true},
		HubCommit:   hubCommit,
		HubContext:  hubch.Context{CI: hubch.Local, Visibility: "public"},
		Fingerprint: hubFP,
		Channel:     channel{head: hubCommit},
		Providers:   providers,
		Snapshots:   fleetRepos{srcs},
		Engine:      "test",
		Now:         w.now,
		Concurrency: 1,
		sleep:       instantSleep,
		clocks:      clocks,
		git:         w.git,
		Write: WriteDeps{
			HubBlobs:     func(string) (io.ReadCloser, error) { return nil, errors.New("the scale world builds no commits") },
			Deadline:     deadline,
			CanWorkflows: map[string]bool{"gh": true, "gl": true},
		},
	}
}

// now is the run's clock: the latest of the providers' clocks.
func (w *fleetWorld) now() time.Time {
	var t time.Time
	for _, sp := range w.provs {
		t = fleetLater(t, sp.clock.Now())
	}
	return t
}

// fleetLater returns the later of a and b.
func fleetLater(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// sync sets every provider's clock to at.
func (w *fleetWorld) sync(at time.Time) {
	for _, sp := range w.provs {
		if d := at.Sub(sp.clock.Now()); d > 0 {
			sp.clock.Advance(d)
		}
	}
}
