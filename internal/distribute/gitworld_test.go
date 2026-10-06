package distribute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// The tests of Run's full phase C run the fake platform in git mode: every
// target is a bare repository served over HTTP, the snapshot source is a
// snapshot.GitSource, and people act through the fake's human actions.

// hubURL is the hub's web URL the bodies link.
const hubURL = "https://github.com/acme/engineering-assets"

// Tokens of the git-mode accounts.
var tokens = map[string]string{
	"acme-read[bot]":  "reader-token-7d1e",
	"acme-write[bot]": "writer-token-2b9f",
	"acme-old[bot]":   "known-token-5c3a",
	"jdoe":            "person-token-9e40",
	"stale[bot]":      "stale-token-0a61",
}

// needDeliveryGit skips a test when the local git is older than distribute
// needs (gitx.DeliveryMinVersion: check-attr --source, --attr-source, lazy
// fetches off); the Docker run of the suite covers these tests.
func needDeliveryGit(t *testing.T) {
	t.Helper()
	v, err := gitx.New("").Version(t.Context())
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if slices.Compare(v[:], gitx.DeliveryMinVersion[:]) < 0 {
		min := gitx.DeliveryMinVersion
		t.Skipf("git %d.%d.%d is older than %d.%d, which distribute's git steps need (check-attr --source, --attr-source); "+
			"run the suite in the golang:1.26 image to cover this test", v[0], v[1], v[2], min[0], min[1])
	}
}

// gitWorld is a world in git mode.
type gitWorld struct {
	*world
	bot   platform.Account // a stale bot
	src   *snapshot.GitSource
	blobs map[string][]byte
	// edit, when set, changes the deps of every run of the world (a pack of
	// the test's own, say).
	edit func(*Deps)
}

// newGitWorld returns a world whose platform, made with opts, serves git.
// Its cleanup stops the server and fails the test on a forbidden
// transition left unread.
func newGitWorld(t *testing.T, opts ...fake.Option) *gitWorld {
	t.Helper()
	needDeliveryGit(t)
	w := newWorld(t, opts...)
	srv, err := w.p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatalf("ServeGit: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the git server: %v", err)
		}
		if v := w.p.Violations(); len(v) > 0 {
			t.Errorf("forbidden transitions: %q", v)
		}
	})
	g := &gitWorld{
		world: w,
		bot:   w.p.AddAccount("stale[bot]", platform.KindBot),
		src:   &snapshot.GitSource{Dir: t.TempDir(), Isolation: gitx.Isolation{Home: t.TempDir(), AllowHTTP: true}},
		blobs: map[string][]byte{},
	}
	for _, a := range []platform.Account{w.reader, w.writer, w.known, w.person, g.bot} {
		w.p.SetToken(a, tokens[a.Login])
	}
	for _, content := range []string{agentsV1, agentsV2, guideV1, pythonV1} {
		g.blobs[oid(content)] = []byte(content)
	}
	w.ok()
	return g
}

// hubBlob serves the hub's blobs.
func (g *gitWorld) hubBlob(id string) (io.ReadCloser, error) {
	b, ok := g.blobs[id]
	if !ok {
		return nil, errors.New("no hub blob " + id)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// repo adds a repository with files ("path", "content" pairs) that the
// writer may write to.
func (g *gitWorld) repo(path string, edit func(*platform.Repo), files ...string) platform.Repo {
	g.t.Helper()
	r := g.world.repo(path, edit, files...)
	g.p.GrantWrite(r.ID, g.writer)
	g.ok()
	return r
}

// optedIn adds a repository that holds the opt-in file and files.
func (g *gitWorld) optedIn(path string, edit func(*platform.Repo), files ...string) platform.Repo {
	g.t.Helper()
	return g.repo(path, edit, append([]string{optInName, "version: 1\n"}, files...)...)
}

// deps returns the dependencies of a run in mode over the world: the git
// snapshot source, the hub's blobs, the writer in ModeDryRun and
// ModeDistribute, and in ModePlan a writer that fails the test when called.
func (g *gitWorld) deps(mode Mode) Deps {
	g.t.Helper()
	d := g.world.deps()
	d.Snapshots = g.src
	d.Write.HubBlobs = g.hubBlob
	d.Write.HubURL = hubURL
	for i := range d.Providers {
		if mode == ModePlan {
			d.Providers[i].Writer = forbiddenWriter{t: g.t}
		} else {
			d.Providers[i].Writer = g.p.Writer(g.writer)
		}
	}
	if g.edit != nil {
		g.edit(&d)
	}
	return d
}

// pack makes the hub ship one pack, base, with files ("path", "content"
// pairs, mode 100644) in every run of the world, and serves their blobs.
func (g *gitWorld) pack(files ...string) {
	g.t.Helper()
	m := &provenance.Manifest{Version: provenance.ManifestVersion, HubCommit: hubCommit}
	cur := provenance.Current{"base": {}, "python": {}}
	for i := 0; i+1 < len(files); i += 2 {
		path, content := files[i], files[i+1]
		v := provenance.Version{OID: oid(content), Size: int64(len(content))}
		m.Add(path, "base", v)
		cur["base"][path] = provenance.File{Pack: "base", Path: path, OID: v.OID, Size: v.Size, Mode: "100644"}
		g.blobs[v.OID] = []byte(content)
	}
	g.edit = func(d *Deps) { d.Manifest, d.Current = m, cur }
}

// run runs d in mode and fails the test on an error, a setup error of the
// fake, or a write in ModePlan and ModeDryRun.
func (g *gitWorld) run(d Deps, mode Mode) *report.Delivery {
	g.t.Helper()
	g.p.ResetCalls()
	rep, err := Run(g.t.Context(), d, mode)
	if err != nil {
		g.t.Fatalf("Run(%v): %v", mode, err)
	}
	g.ok()
	checkReport(g.t, rep)
	if mode != ModeDistribute {
		if writes := g.p.Writes(); len(writes) > 0 {
			g.t.Errorf("%v wrote: %q", mode, writes)
		}
	}
	return rep
}

// forbiddenWriter is the writer of a plan: any call fails the test.
type forbiddenWriter struct {
	platform.Writer
	t *testing.T
}

func (f forbiddenWriter) fail(method string) error {
	f.t.Errorf("plan called the writer's %s", method)
	return errors.New("plan called the writer")
}

func (f forbiddenWriter) Target(context.Context, platform.Repo, platform.Perms) (platform.TargetWriter, error) {
	return nil, f.fail("Target")
}

func (f forbiddenWriter) Probe(context.Context) (platform.Caps, error) {
	return platform.Caps{}, f.fail("Probe")
}

func (f forbiddenWriter) Self(context.Context) (platform.Account, error) {
	return platform.Account{}, f.fail("Self")
}

// syncCommit pushes a commit of touchmark's to branch of r, as distribute
// makes it: files ("path", "content" pairs; "" deletes) on top of parent
// (the default branch's head when ""), with the trailers of the pairs it
// brings, through the writer's per-target token. It returns the commit.
func (g *gitWorld) syncCommit(r platform.Repo, branch, parent string, files ...string) string {
	g.t.Helper()
	return g.commitOn(r, branch, parent, hubFP, files...)
}

// commitOn is syncCommit with the fingerprint fp in the trailers.
func (g *gitWorld) commitOn(r platform.Repo, branch, parent, fp string, files ...string) string {
	g.t.Helper()
	ctx := g.t.Context()
	tw, err := g.p.Writer(g.writer).Target(ctx, r, platform.Perms{Contents: true, PRs: true, Workflows: true})
	if err != nil {
		g.t.Fatalf("Target: %v", err)
	}
	defer tw.Close()
	rem := tw.Remote()
	repo, err := gitx.InitTarget(ctx, filepath.Join(g.t.TempDir(), "sync"), rem.URL, gitx.Auth{Header: rem.Header},
		gitx.Isolation{Home: g.t.TempDir(), AllowHTTP: true})
	if err != nil {
		g.t.Fatalf("InitTarget: %v", err)
	}
	if _, _, err := repo.FetchBranch(ctx, r.DefaultBranch, 1); err != nil {
		g.t.Fatalf("fetch %s: %v", r.DefaultBranch, err)
	}
	head, _, err := repo.FetchBranch(ctx, branch, 1)
	if err != nil {
		g.t.Fatalf("fetch %s: %v", branch, err)
	}
	if parent == "" {
		parent = g.p.Head(r.ID)
	} else if _, _, err := repo.FetchBranch(ctx, r.DefaultBranch, decide.MaxHistory); err != nil {
		g.t.Fatalf("fetch %s: %v", r.DefaultBranch, err)
	}
	entries, err := repo.Tree(ctx, parent)
	if err != nil {
		g.t.Fatalf("tree of %s: %v", parent, err)
	}
	old := map[string]string{}
	for _, e := range entries {
		old[e.Path] = e.OID
	}
	var pairs []decide.Pair
	spec := gitx.CommitSpec{Parent: parent, Blobs: map[string]gitx.Blob{}, When: fake.Epoch,
		Author: gitx.Person{Name: g.writer.Login, Email: g.writer.Email}, Committer: gitx.Person{Name: g.writer.Login, Email: g.writer.Email}}
	for i := 0; i+1 < len(files); i += 2 {
		path, content := files[i], files[i+1]
		p := decide.Pair{Path: path, From: decide.ZeroOID, Mode: "100644", To: oid(content)}
		if from, ok := old[path]; ok {
			p.From = from
		}
		if content == "" {
			p.Mode, p.To = decide.ModeDelete, decide.ZeroOID
			spec.Changes = append(spec.Changes, gitx.Change{Path: path})
		} else {
			data := []byte(content)
			spec.Changes = append(spec.Changes, gitx.Change{Path: path, Mode: "100644", OID: p.To})
			spec.Blobs[p.To] = gitx.Blob{OID: p.To, Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
		}
		pairs = append(pairs, p)
	}
	msg, err := prbody.CommitMessage("chore: sync engineering assets", decide.FormatTrailers(decide.Trailers{
		HubID: "acme-eng", Fingerprint: fp, Stream: decide.StreamSync, Content: decide.Key(decide.StreamSync, pairs), HubCommit: hubCommit,
	}))
	if err != nil {
		g.t.Fatal(err)
	}
	spec.Message = msg
	built, err := repo.BuildCommit(ctx, spec)
	if err != nil {
		g.t.Fatalf("BuildCommit: %v", err)
	}
	res, err := repo.Push(ctx, gitx.PushSpec{Branch: branch, Commit: built.Commit, Expect: head})
	if err != nil || (res.Status != gitx.PushOK && res.Status != gitx.PushUpToDate) {
		g.t.Fatalf("push %s: %+v, %v", branch, res, err)
	}
	return built.Commit
}

// keyOf is the content key of pairs of files ("path", "content" pairs;
// "" deletes) on a target that holds none of them.
func keyOf(files ...string) string {
	var pairs []decide.Pair
	for i := 0; i+1 < len(files); i += 2 {
		pairs = append(pairs, decide.Pair{Path: files[i], From: decide.ZeroOID, Mode: "100644", To: oid(files[i+1])})
	}
	return decide.Key(decide.StreamSync, pairs)
}

// baseFiles is what the base pack brings to a target that holds none of its
// files.
var baseFiles = []string{"AGENTS.md", agentsV2, "docs/guide.md", guideV1}

// ownOn adds an open pull request of the writer on branch whose marker
// carries key and the changes of files (a target that held none of them).
func (g *gitWorld) ownOn(r platform.Repo, branch, key string, files ...string) int64 {
	g.t.Helper()
	return g.pr(r, platform.PR{Head: branch, Author: g.writer, Title: "chore: sync engineering assets",
		Body: markerBody(g.t, key, hubFP, changesOf(files...), nil)})
}

// changesOf is the short changes of files on a target that held none of
// them.
func changesOf(files ...string) []marker.Change {
	var pairs []decide.Pair
	for i := 0; i+1 < len(files); i += 2 {
		pairs = append(pairs, decide.Pair{Path: files[i], From: decide.ZeroOID, Mode: "100644", To: oid(files[i+1])})
	}
	return decide.ShortChanges(pairs)
}

// markerBody is a body with a marker of the hub with fingerprint fp over
// key and changes, its data edited by edit.
func markerBody(t *testing.T, key, fp string, changes []marker.Change, edit func(*marker.Data)) string {
	t.Helper()
	d := marker.Data{
		V: marker.Version, Stream: decide.StreamSync, Hub: "acme-eng", FP: fp,
		DecidedAt: hubCommit, ContentCommit: hubCommit, Engine: "0.2.0",
		Packs: []string{"base"}, Changes: changes, ChangesComplete: len(changes) > 0,
		TitleSet: "chore: sync engineering assets", LabelsSet: []string{"engineering-assets"},
	}
	if edit != nil {
		edit(&d)
	}
	line, err := marker.Encode(marker.Marker{Key: key, Data: d})
	if err != nil {
		t.Fatal(err)
	}
	return "Engineering assets from the hub.\n\n" + line
}

// push is a person's push of files ("path", "content" pairs; "" deletes) to
// branch; it returns the new head.
func (g *gitWorld) push(r platform.Repo, branch string, by platform.Account, files ...string) string {
	g.t.Helper()
	m := map[string][]byte{}
	for i := 0; i+1 < len(files); i += 2 {
		if files[i+1] == "" {
			m[files[i]] = nil
		} else {
			m[files[i]] = []byte(files[i+1])
		}
	}
	head, err := g.p.PushFiles(r.ID, branch, m, by, time.Time{})
	if err != nil {
		g.t.Fatalf("PushFiles(%s, %s): %v", r.Path, branch, err)
	}
	return head
}

// same compares what two reports say about their targets, the sweep, the
// summary and the cost: the report of a plan and of a dry run of the same
// state must agree.
func same(t *testing.T, plan, dry *report.Delivery) {
	t.Helper()
	enc := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if a, b := enc(plan.Targets), enc(dry.Targets); a != b {
		t.Errorf("plan and dry run differ:\nplan %s\ndry  %s", a, b)
	}
	if plan.Sweep != dry.Sweep || !maps.Equal(plan.Summary, dry.Summary) || !maps.Equal(plan.Cost, dry.Cost) {
		t.Errorf("plan: sweep %+v summary %v cost %v; dry run: sweep %+v summary %v cost %v",
			plan.Sweep, plan.Summary, plan.Cost, dry.Sweep, dry.Summary, dry.Cost)
	}
	if !slices.Equal(plan.Warnings, dry.Warnings) {
		t.Errorf("warnings: plan %q, dry run %q", plan.Warnings, dry.Warnings)
	}
	if plan.Command != "plan" || dry.Command != "distribute" {
		t.Errorf("commands %s, %s", plan.Command, dry.Command)
	}
}

// both runs d in ModePlan and ModeDryRun, checks that they agree, and
// returns the plan's report.
func (g *gitWorld) both(edit func(*Deps)) *report.Delivery {
	g.t.Helper()
	dp, dd := g.deps(ModePlan), g.deps(ModeDryRun)
	if edit != nil {
		edit(&dp)
		edit(&dd)
	}
	plan := g.run(dp, ModePlan)
	dry := g.run(dd, ModeDryRun)
	same(g.t, plan, dry)
	return plan
}

// inspectOne runs phases A–C of d in mode for the target at path and
// returns its work (nil when it has none) and its report line.
func (g *gitWorld) inspectOne(d Deps, mode Mode, path string) (*Work, report.DeliveryTarget) {
	g.t.Helper()
	r, err := newRun(d, mode)
	if err != nil {
		g.t.Fatal(err)
	}
	kept, err := r.resolve(g.t.Context())
	if err != nil {
		g.t.Fatal(err)
	}
	for _, t := range kept {
		if t.repo.Path == path {
			// The repository stays until the test ends (the GitSource's
			// directory is the test's), so the test may read the commit.
			w := r.inspectSafe(g.t.Context(), t, time.Time{})
			return w, t.res
		}
	}
	g.t.Fatalf("no target %s", path)
	return nil, report.DeliveryTarget{}
}

// written makes open pull request n of r look as touchmark wrote it last:
// the description it renders now for the target, with a marker of the
// content it proposes (TitleSet, Body and LabelsSet as a create writes
// them). A plan then has nothing to write to it (I7).
func (g *gitWorld) written(r platform.Repo, n int64) {
	g.t.Helper()
	w, res := g.inspectOne(g.deps(ModePlan), ModePlan, r.Path)
	if w == nil {
		g.t.Fatalf("%s: no work (%s:%s)", r.Path, res.Outcome, res.Reason)
		return
	}
	human, err := w.humanBody()
	if err != nil {
		g.t.Fatal(err)
	}
	d := w.Marker
	d.TitleSet, d.Body, d.LabelsSet = "chore: sync engineering assets", decide.BodyHash(human), []string{"engineering-assets"}
	line, err := marker.Encode(marker.Marker{Key: w.Key, Data: d})
	if err != nil {
		g.t.Fatal(err)
	}
	g.p.UpdatePR(r.ID, n, func(pr *platform.PR) {
		pr.Body = human + "\n\n" + line
		pr.Title = "chore: sync engineering assets"
		pr.Labels = []string{"engineering-assets"}
	})
	g.ok()
}

// hasWarningLike reports whether a target warning contains every part.
func hasWarningLike(tg report.DeliveryTarget, parts ...string) bool {
	return slices.ContainsFunc(tg.Warnings, func(w string) bool {
		for _, p := range parts {
			if !strings.Contains(w, p) {
				return false
			}
		}
		return true
	})
}
