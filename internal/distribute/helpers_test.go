package distribute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// The hub of the tests.
const (
	hubFP     = "github.com/712345678"
	prevFP    = "gitlab.example.com/1234"
	otherFP   = "github.com/999"
	hubCommit = "3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3"
	branch    = "touchmark/acme-eng"
	alias     = "chore/sync-engineering-assets"
	optInName = ".engineering-assets.yml"
)

// defaultHubYML declares one GitHub provider with a writer and a known
// author, a branch alias and a previous fingerprint.
const defaultHubYML = `version: 1
id: acme-eng
branch_aliases: [chore/sync-engineering-assets]
previous_fingerprints: [gitlab.example.com/1234]
providers:
  - id: gh
    type: github
    writer: acme-write[bot]
    known_authors: ["acme-old[bot]"]
`

// defaultTargetsYML gives base to every repository of acme, python to
// those with the python topic, and excludes acme/legacy.
const defaultTargetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - org: acme
  - org: acme
    topics: [python]
    packs: [python]
exclude:
  - acme/legacy
`

// version is the content of a pack file: large enough to be evidence of
// ownership.
func version(name string, n int) string {
	return strings.Repeat(fmt.Sprintf("%s, version %d of the shared engineering asset\n", name, n), 3)
}

func oid(content string) string { return gitx.RawOID([]byte(content)) }

// The packs: base ships AGENTS.md (v2 now, v1 before) and docs/guide.md;
// python ships docs/python.md.
var (
	agentsV1 = version("AGENTS.md", 1)
	agentsV2 = version("AGENTS.md", 2)
	guideV1  = version("docs/guide.md", 1)
	pythonV1 = version("docs/python.md", 1)
)

func manifestAndCurrent() (*provenance.Manifest, provenance.Current) {
	m := &provenance.Manifest{Version: provenance.ManifestVersion, HubCommit: hubCommit}
	cur := provenance.Current{"base": {}, "python": {}}
	add := func(pack, path, content string, now bool) {
		v := provenance.Version{OID: oid(content), Size: int64(len(content))}
		m.Add(path, pack, v)
		if now {
			cur[pack][path] = provenance.File{Pack: pack, Path: path, OID: v.OID, Size: v.Size, Mode: "100644"}
		}
	}
	add("base", "AGENTS.md", agentsV1, false)
	add("base", "AGENTS.md", agentsV2, true)
	add("base", "docs/guide.md", guideV1, true)
	add("python", "docs/python.md", pythonV1, true)
	return m, cur
}

// keyOfMissing returns the key of D for a target that holds none of the
// files of packs (base, and python when named).
func keyOfMissing(packs ...string) string {
	files := map[string]string{"AGENTS.md": agentsV2, "docs/guide.md": guideV1}
	if slices.Contains(packs, "python") {
		files["docs/python.md"] = pythonV1
	}
	var pairs []decide.Pair
	for path, content := range files {
		pairs = append(pairs, decide.Pair{Path: path, From: decide.ZeroOID, Mode: "100644", To: oid(content)})
	}
	return decide.Key(decide.StreamSync, pairs)
}

// world is a fake GitHub with the accounts of a hub, and the hub's
// configuration and packs.
type world struct {
	t                             *testing.T
	p                             *fake.Platform
	reader, writer, known, person platform.Account
	hubYML, targetsYML            string
	ctx                           hubch.Context
}

func newWorld(t *testing.T, opts ...fake.Option) *world {
	t.Helper()
	p := fake.New("github.com", opts...)
	w := &world{
		t:          t,
		p:          p,
		reader:     p.AddAccount("acme-read[bot]", platform.KindBot),
		writer:     p.AddAccount("acme-write[bot]", platform.KindBot),
		known:      p.AddAccount("acme-old[bot]", platform.KindBot),
		person:     p.AddAccount("jdoe", platform.KindUser),
		hubYML:     defaultHubYML,
		targetsYML: defaultTargetsYML,
		ctx:        hubch.Context{CI: hubch.Local},
	}
	w.ok()
	return w
}

// ok fails the test on a setup error of the fake.
func (w *world) ok() {
	w.t.Helper()
	if err := w.p.Err(); err != nil {
		w.t.Fatalf("setup: %v", err)
	}
}

// repo adds a repository with files ("path", "content" pairs).
func (w *world) repo(path string, edit func(*platform.Repo), files ...string) platform.Repo {
	w.t.Helper()
	r := platform.Repo{Path: path}
	if edit != nil {
		edit(&r)
	}
	r = w.p.AddRepo(r)
	for i := 0; i+1 < len(files); i += 2 {
		w.p.SetFile(r.ID, files[i], []byte(files[i+1]), "")
	}
	w.ok()
	got, _ := w.p.RepoByID(r.ID)
	return got
}

// optedIn adds a repository that holds the opt-in file and files.
func (w *world) optedIn(path string, edit func(*platform.Repo), files ...string) platform.Repo {
	w.t.Helper()
	return w.repo(path, edit, append([]string{optInName, "version: 1\n"}, files...)...)
}

func topics(names ...string) func(*platform.Repo) {
	return func(r *platform.Repo) { r.Topics = names }
}

// pr adds a pull request.
func (w *world) pr(r platform.Repo, pr platform.PR) int64 {
	w.t.Helper()
	n := w.p.AddPR(r.ID, pr)
	w.ok()
	return n
}

// ownPR adds an open pull request of the writer on the sync branch whose
// marker carries key.
func (w *world) ownPR(r platform.Repo, key string) int64 {
	w.t.Helper()
	return w.pr(r, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync engineering assets",
		Body: body(w.t, key, hubFP)})
}

// body is a pull request body with a marker of the hub with fingerprint fp
// over key.
func body(t *testing.T, key, fp string) string {
	t.Helper()
	line, err := marker.Encode(marker.Marker{Key: key, Data: marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: fp,
		DecidedAt: hubCommit, ContentCommit: hubCommit, Engine: "0.2.0",
		Packs: []string{"base"}, ChangesComplete: false,
		TitleSet: "chore: sync engineering assets", LabelsSet: []string{"engineering-assets"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return "Engineering assets from the hub.\n\n" + line
}

// staleKey is a key no plan of these tests computes.
const staleKey = "sha256:0f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a49380f9e8d7c6b5a4938"

// deps returns the dependencies of a plan over the world.
func (w *world) deps() Deps {
	w.t.Helper()
	hub, _, err := config.ParseHub([]byte(w.hubYML))
	if err != nil {
		w.t.Fatal(err)
	}
	targets, _, err := config.ParseTargets([]byte(w.targetsYML))
	if err != nil {
		w.t.Fatal(err)
	}
	m, cur := manifestAndCurrent()
	return Deps{
		Hub:         hub,
		Targets:     targets,
		Manifest:    m,
		Current:     cur,
		Known:       map[string]bool{"base": true, "python": true},
		HubCommit:   hubCommit,
		HubContext:  w.ctx,
		Fingerprint: hubFP,
		Providers:   w.providers(hub, map[string]platform.Reader{"github.com": w.p.Reader(w.reader)}),
		Snapshots:   w.p.Snapshots(),
		Engine:      "test",
		sleep:       instantSleep,
	}
}

// instantSleep is the sleep of the tests' runs: the retries of a read that
// failed transiently come at once.
func instantSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// transientTimes queues n failures of method with err: a read the run tries
// readAttempts times fails for good with n == readAttempts.
func transientTimes(p *fake.Platform, method string, err error, n int) {
	for range n {
		p.FailNext(method, err)
	}
}

// providers resolves hub.yml's providers and gives each the reader of its
// host.
func (w *world) providers(hub *config.Hub, readers map[string]platform.Reader) []Provider {
	w.t.Helper()
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil {
		w.t.Fatal(err)
	}
	var out []Provider
	for _, rp := range rps {
		out = append(out, Provider{Config: rp, Reader: readers[rp.Host]})
	}
	return out
}

// plan runs Plan and fails the test on an error or a setup error.
func (w *world) plan(d Deps) *report.Delivery {
	w.t.Helper()
	rep, err := Plan(w.t.Context(), d)
	if err != nil {
		w.t.Fatalf("Plan: %v", err)
	}
	w.ok()
	checkReport(w.t, rep)
	return rep
}

// targetOf returns the report entry of ref ("<provider>:<path>").
func targetOf(t *testing.T, rep *report.Delivery, ref string) report.DeliveryTarget {
	t.Helper()
	for _, tg := range rep.Targets {
		if tg.Provider+":"+tg.Path == ref {
			return tg
		}
	}
	var have []string
	for _, tg := range rep.Targets {
		have = append(have, tg.Provider+":"+tg.Path)
	}
	t.Fatalf("no target %s in %q", ref, have)
	return report.DeliveryTarget{}
}

// want checks the outcome, reason and pull request number of a target.
func want(t *testing.T, rep *report.Delivery, ref string, outcome report.Outcome, reason string, pr int64) report.DeliveryTarget {
	t.Helper()
	tg := targetOf(t, rep, ref)
	var got int64
	if tg.PR != nil {
		got = tg.PR.Number
	}
	if tg.Outcome != outcome || tg.Reason != reason || got != pr {
		t.Errorf("%s: %s:%s #%d, want %s:%s #%d (warnings %q)", ref, tg.Outcome, tg.Reason, got, outcome, reason, pr, tg.Warnings)
	}
	return tg
}

// hasWarning reports whether some warning contains s.
func hasWarning(warnings []string, s string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, s) })
}

// channel is a hub channel with a fixed answer.
type channel struct {
	head string
	err  error
}

func (c channel) Head(context.Context) (string, error) { return c.head, c.err }

// errAuth is a platform error of class auth.
var errAuth = &platform.Error{Op: "test", Class: platform.ClassAuth, Status: 401, Err: errors.New("bad credentials")}

// sources dispatches snapshots to the fake platform of the repository's
// host.
type sources map[string]snapshot.Source

func (s sources) Snapshot(ctx context.Context, r platform.Repo, remote platform.Remote, ref string) (*snapshot.Tree, error) {
	src, ok := s[r.Host]
	if !ok {
		return nil, fmt.Errorf("no platform for %s", r.Host)
	}
	return src.Snapshot(ctx, r, remote, ref)
}
