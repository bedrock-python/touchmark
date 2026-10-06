package apply

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// hub is a pack "base" as decide sees it: its history and what it ships now.
type hub struct {
	manifest *provenance.Manifest
	desired  map[string]decide.Desired
	blobs    memBlobs
}

func newHub() *hub {
	return &hub{
		manifest: &provenance.Manifest{Version: provenance.ManifestVersion},
		desired:  map[string]decide.Desired{},
		blobs:    memBlobs{},
	}
}

// shipped records content as a past version of path.
func (h *hub) shipped(path, content string) {
	h.manifest.Add(path, "base", provenance.Version{OID: oid(content), Size: int64(len(content))})
}

// ships makes content the current version of path.
func (h *hub) ships(path, content string) {
	h.shipped(path, content)
	h.desired[path] = decide.Desired{
		Path: path, Pack: "base", OID: h.blobs.add(content), Size: int64(len(content)), Mode: modeFile,
	}
}

// input is what decide needs about the hub, without observations.
func (h *hub) input() decide.Input {
	return decide.Input{
		Manifest:        h.manifest,
		Selected:        []string{"base"},
		Desired:         h.desired,
		MinEvidenceSize: 1,
	}
}

// plan observes tr and decides.
func (h *hub) plan(t *testing.T, tr *tree) decide.Plan {
	t.Helper()
	in := h.input()
	in.Observed = tr.observe(decide.Paths(in)...)
	return decide.Decide(in)
}

// planCommitted decides on the tree tr's HEAD commits, as plan and
// distribute do: from the committed blob ids, with nothing checked out.
func (h *hub) planCommitted(t *testing.T, tr *tree) decide.Plan {
	t.Helper()
	entries, err := tr.git.LsTree(t.Context(), "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	head := &snapshot.Tree{Entries: map[string]snapshot.Entry{}}
	for _, e := range entries {
		head.Entries[e.Path] = snapshot.Entry{Mode: e.Mode, OID: e.OID}
	}
	in := h.input()
	in.Observed = head.Observe(decide.Paths(in))
	return decide.Decide(in)
}

// apply plans, executes, and checks that a second plan has nothing to do.
func (h *hub) apply(t *testing.T, tr *tree) []Result {
	t.Helper()
	res := Execute(t.Context(), tr.target(), h.plan(t, tr), h.blobs)
	if changes := h.plan(t, tr).Changes(); len(changes) != 0 {
		t.Errorf("second plan is not empty: %+v", changes)
	}
	return res
}

// wantPlan fails the test unless plan holds exactly these "action path"
// lines, with a "+" suffix for AfterDeletes, ignoring Keep entries.
func wantPlan(t *testing.T, plan decide.Plan, want ...string) {
	t.Helper()
	var got []string
	for _, e := range plan.Changes() {
		s := string(e.Action) + " " + e.Path
		if e.AfterDeletes {
			s += "+"
		}
		got = append(got, s)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestPlanFileBecomesDirectory: a pack stops shipping "docs" and ships
// "docs/x.md" instead.
func TestPlanFileBecomesDirectory(t *testing.T) {
	t.Parallel()
	h := newHub()
	h.shipped("docs", "old docs file\n")
	h.ships("docs/x.md", "x\n")
	tr := newTree(t)
	tr.write("docs", "old docs file\n")
	wantPlan(t, h.plan(t, tr), "delete docs", "create docs/x.md+")
	wantDone(t, h.apply(t, tr))
	if got := tr.read("docs/x.md"); got != "x\n" {
		t.Errorf("docs/x.md = %q", got)
	}
}

// TestPlanDirectoryBecomesFile: a pack stops shipping "docs/x.md" and ships
// "docs" instead.
func TestPlanDirectoryBecomesFile(t *testing.T) {
	t.Parallel()
	h := newHub()
	h.shipped("docs/sub/x.md", "x\n")
	h.ships("docs", "docs file\n")
	tr := newTree(t)
	tr.write("docs/sub/x.md", "x\n")
	wantPlan(t, h.plan(t, tr), "delete docs/sub/x.md", "create docs+")
	wantDone(t, h.apply(t, tr))
	if got := tr.read("docs"); got != "docs file\n" {
		t.Errorf("docs = %q", got)
	}
}

// TestPlanDirectoryNotEmpty: the directory holds a file of the target's own,
// so the create is skipped after the retired file is deleted.
func TestPlanDirectoryNotEmpty(t *testing.T) {
	t.Parallel()
	h := newHub()
	h.shipped("docs/x.md", "x\n")
	h.ships("docs", "docs file\n")
	tr := newTree(t)
	tr.write("docs/x.md", "x\n")
	tr.write("docs/mine.md", "mine\n")
	res := Execute(t.Context(), tr.target(), h.plan(t, tr), h.blobs)
	wantDone(t, res[:1])
	wantSkipped(t, res[1], "directory not empty")
	if got := tr.read("docs/mine.md"); got != "mine\n" {
		t.Errorf("docs/mine.md = %q", got)
	}
	if tr.lstat("docs/x.md") != nil {
		t.Error("docs/x.md was not deleted")
	}
}

// TestPlanBlockerChanged: the retired file in the way was edited after
// Observe, so neither the delete nor the create runs.
func TestPlanBlockerChanged(t *testing.T) {
	t.Parallel()
	h := newHub()
	h.shipped("docs", "old docs file\n")
	h.ships("docs/x.md", "x\n")
	tr := newTree(t)
	tr.write("docs", "old docs file\n")
	plan := h.plan(t, tr)
	tr.write("docs", "edited\n")
	res := Execute(t.Context(), tr.target(), plan, h.blobs)
	wantSkipped(t, res[0], "content differs")
	wantSkipped(t, res[1], "still in the way: parent docs is a file")
	if got := tr.read("docs"); got != "edited\n" {
		t.Errorf("docs = %q", got)
	}
}

// TestPlanCRLFCheckout runs the whole cycle on a Windows-style checkout:
// files with CRLF whose committed form is a pack version are managed, not
// local, and a second run has nothing to do.
func TestPlanCRLFCheckout(t *testing.T) {
	t.Parallel()
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	h := newHub()
	h.shipped("outdated.md", "version one\n")
	h.ships("outdated.md", "version two\n")
	h.ships("current.md", "current\n")
	h.shipped("retired.md", "retired\n")
	h.ships("missing.md", "missing\n")
	h.ships("local.md", "pack\n")

	tr := newRepo(t, true)
	tr.write("outdated.md", crlf("version one\n"))
	tr.write("current.md", crlf("current\n"))
	tr.write("retired.md", crlf("retired\n"))
	tr.write("local.md", crlf("our own\n"))
	wantPlan(t, h.plan(t, tr), "delete retired.md", "create missing.md", "update outdated.md")
	wantDone(t, h.apply(t, tr))
	for p, want := range map[string]string{
		"outdated.md": "version two\n",
		"current.md":  crlf("current\n"),
		"missing.md":  "missing\n",
		"local.md":    crlf("our own\n"),
	} {
		if got := tr.read(p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if tr.lstat("retired.md") != nil {
		t.Error("retired.md was not deleted")
	}
}

// TestPlanCommittedCRLF: status and apply judge a file the target has
// committed by its committed blob, as plan and distribute do. A file
// committed with CRLF whose LF form is a pack version is the target's own
// (core.autocrlf=true would hash it as LF), and apply leaves it byte for
// byte; an LF blob checked out with CRLF is still the pack's. Both views
// agree before apply, and again once its result is committed.
func TestPlanCommittedCRLF(t *testing.T) {
	t.Parallel()
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	h := newHub()
	files := map[string]string{}
	for _, eol := range []string{"lf", "crlf"} {
		conv := func(s string) string { return s }
		if eol == "crlf" {
			conv = crlf
		}
		h.shipped(eol+"-outdated.md", "version one\n")
		h.ships(eol+"-outdated.md", "version two\n")
		h.ships(eol+"-current.md", "current\n")
		h.shipped(eol+"-retired.md", "retired\n")
		files[eol+"-outdated.md"] = conv("version one\n")
		files[eol+"-current.md"] = conv("current\n")
		files[eol+"-retired.md"] = conv("retired\n")
	}

	tr := newRepo(t, false)
	for p, content := range files {
		tr.write(p, content)
	}
	tr.run("add", "-A")
	tr.run("commit", "-q", "-m", "target")
	// A fresh Windows checkout: LF blobs come out with CRLF.
	tr.run("config", "core.autocrlf", "true")
	for p := range files {
		tr.remove(p)
	}
	tr.run("checkout", "--", ".")
	if got := tr.read("lf-current.md"); got != crlf("current\n") {
		t.Fatalf("lf-current.md was checked out as %q, want CRLF", got)
	}

	local := h.plan(t, tr)
	wantStates(t, local, map[string]decide.State{
		"lf-outdated.md":   decide.Outdated,
		"lf-current.md":    decide.Current,
		"lf-retired.md":    decide.Retired,
		"crlf-outdated.md": decide.Local,
		"crlf-current.md":  decide.Local,
		"crlf-retired.md":  decide.RetiredLocal,
	})
	sameDecisions(t, local, h.planCommitted(t, tr))

	wantPlan(t, local, "delete lf-retired.md", "update lf-outdated.md")
	wantDone(t, h.apply(t, tr))
	for _, p := range []string{"crlf-outdated.md", "crlf-current.md", "crlf-retired.md"} {
		if got := tr.read(p); got != files[p] {
			t.Errorf("%s = %q, want it untouched", p, got)
		}
	}
	tr.run("add", "-A")
	tr.run("commit", "-q", "-m", "apply")
	sameDecisions(t, h.plan(t, tr), h.planCommitted(t, tr))
}

// TestPlanCleanFilter: a file that git stores through a clean filter (Git
// LFS, for one) is judged by its bytes on disk too, not by the filtered
// blob alone, which no pack ships: a pack version is outdated, and current
// once apply has updated it and the result is committed.
func TestPlanCleanFilter(t *testing.T) {
	t.Parallel()
	h := newHub()
	h.shipped("img.up", "version one\n")
	h.ships("img.up", "version two\n")

	tr := newRepo(t, false)
	tr.run("config", "filter.up.clean", "tr a-z A-Z")
	tr.write(".gitattributes", "*.up filter=up\n")
	tr.write("img.up", "version one\n")
	tr.run("add", "-A")
	tr.run("commit", "-q", "-m", "target")

	wantStates(t, h.plan(t, tr), map[string]decide.State{"img.up": decide.Outdated})
	wantDone(t, h.apply(t, tr))
	tr.run("add", "-A")
	tr.run("commit", "-q", "-m", "apply")
	wantStates(t, h.plan(t, tr), map[string]decide.State{"img.up": decide.Current})
}

// wantStates fails the test unless plan decides each path as want says
// and holds no other path.
func wantStates(t *testing.T, plan decide.Plan, want map[string]decide.State) {
	t.Helper()
	got := map[string]decide.State{}
	for _, e := range plan.Entries {
		got[e.Path] = e.State
	}
	if !maps.Equal(got, want) {
		t.Errorf("states %v\nwant %v", got, want)
	}
}

// sameDecisions fails the test unless the working tree's plan and the
// committed tree's decide every path alike.
func sameDecisions(t *testing.T, worktree, committed decide.Plan) {
	t.Helper()
	lines := func(p decide.Plan) []string {
		var out []string
		for _, e := range p.Entries {
			out = append(out, fmt.Sprintf("%s %s %s pack=%s from=%s to=%s mode=%s", e.Path, e.State, e.Action, e.Pack, e.From, e.To, e.Mode))
		}
		return out
	}
	if a, b := lines(worktree), lines(committed); !slices.Equal(a, b) {
		t.Errorf("the working tree and the committed tree disagree:\n%s\nvs\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
}
