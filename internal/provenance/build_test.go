package provenance

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
)

// TestBuild covers edits, a mode-only change, deletion, a pack rename, a
// side branch merged with an evil merge, a branch that is never merged, a
// revert to an old version and historical entries that fail today's rules.
func TestBuild(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	a1, a2, a3, a4 := text("agents v1"), text("agents v2"), text("agents side"), text("agents evil")
	d1, e1, x1, v1 := text("docs a"), text("editorconfig"), text("extra"), text("evil new file")

	r.write("packs/agents/AGENTS.md", a1)
	r.write("packs/agents/docs/a.md", d1)
	r.write("packs/base/.editorconfig", e1)
	r.write("README.md", "outside packs/\n")
	r.write("packs/README.md", "directly under packs/\n")
	r.setEntry("100644", text("bad pack"), "packs/Bad_Name/x.md")
	r.setEntry("100644", text("bad path"), "packs/agents/a:b.md")
	r.setEntry("120000", "AGENTS.md", "packs/agents/link")
	r.commit("c1")

	r.write("packs/agents/AGENTS.md", a2)
	r.git("update-index", "--chmod=+x", "packs/agents/docs/a.md")
	r.git("rm", "-q", "--cached", "--", "packs/Bad_Name/x.md", "packs/agents/a:b.md", "packs/agents/link")
	r.commit("c2: edit, chmod, drop invalid entries")

	r.git("rm", "-q", "-f", "--", "packs/agents/docs/a.md")
	r.commit("c3: delete")

	r.git("mv", "packs/base", "packs/core")
	r.commit("c4: rename pack")

	r.git("checkout", "-q", "-b", "side")
	r.write("packs/agents/AGENTS.md", a3)
	r.commit("side")

	r.git("checkout", "-q", "-b", "unmerged")
	r.write("packs/agents/AGENTS.md", text("never merged"))
	r.commit("unmerged")

	r.git("checkout", "-q", "master")
	r.write("packs/core/extra.md", x1)
	r.commit("c5")

	// Evil merge: the result holds versions neither parent has.
	r.git("-c", "merge.renames=false", "merge", "-q", "--no-ff", "--no-commit", "side")
	r.write("packs/agents/AGENTS.md", a4)
	r.write("packs/agents/evil.md", v1)
	r.commit("evil merge")

	r.write("packs/agents/AGENTS.md", a1)
	head := r.commit("c6: back to v1")

	want := &Manifest{
		Version:   ManifestVersion,
		HubCommit: head,
		Paths: map[string]map[string][]Version{
			"AGENTS.md":     {"agents": {ver(a1), ver(a2), ver(a3), ver(a4)}},
			"docs/a.md":     {"agents": {ver(d1)}},
			"evil.md":       {"agents": {ver(v1)}},
			".editorconfig": {"base": {ver(e1)}, "core": {ver(e1)}},
			"extra.md":      {"core": {ver(x1)}},
		},
	}
	wantSkipped := []string{"packs/Bad_Name/x.md", "packs/README.md", "packs/agents/a:b.md"}

	m, skipped, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("Build() manifest =\n%s\nwant\n%s", dump(t, m), dump(t, want))
	}
	if !slices.Equal(skipped, wantSkipped) {
		t.Errorf("Build() skipped = %q, want %q", skipped, wantSkipped)
	}

	// Uncommitted edits, staged or not, never reach the manifest.
	r.writeFile("packs/agents/AGENTS.md", text("dirty"))
	r.write("packs/agents/staged.md", text("staged"))
	again, _, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Errorf("Build() after uncommitted edits =\n%s", dump(t, again))
	}

	// Without an explicit Git, the hub's Dir is used.
	plain, _, err := Build(t.Context(), &Hub{Dir: r.dir})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, want) {
		t.Errorf("Build(Hub without Git) =\n%s", dump(t, plain))
	}
}

func TestBuildWithoutPacks(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	r.write("README.md", "a hub without packs\n")
	head := r.commit("c1")
	m, skipped, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	want := &Manifest{Version: ManifestVersion, HubCommit: head, Paths: map[string]map[string][]Version{}}
	if !reflect.DeepEqual(m, want) || len(skipped) != 0 {
		t.Errorf("Build() = %+v, %q; want an empty manifest", m, skipped)
	}
}

func TestBuildErrors(t *testing.T) {
	t.Parallel()
	if _, _, err := Build(t.Context(), nil); err == nil {
		t.Error("Build(nil): want error")
	}
	empty := newHubRepo(t, false)
	if _, _, err := Build(t.Context(), empty.hub()); err == nil || errors.Is(err, ErrShallow) {
		t.Errorf("Build(no commits) = %v; want a HEAD error", err)
	}
	notRepo := &Hub{Dir: t.TempDir(), Git: &gitx.Git{Dir: t.TempDir(), Env: testEnv(t)}}
	if _, _, err := Build(t.Context(), notRepo); err == nil {
		t.Error("Build(not a repository): want error")
	}
}

func TestBuildShallow(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	r.write("packs/agents/AGENTS.md", text("v1"))
	r.commit("c1")
	r.write("packs/agents/AGENTS.md", text("v2"))
	r.commit("c2")
	full, _, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}

	cloneInto := func(name string, args ...string) *Hub {
		dst := filepath.Join(t.TempDir(), name)
		g := &gitx.Git{Env: r.g.Env}
		args = append(append([]string{"clone", "-q"}, args...), fileURL(r.dir), dst)
		if _, err := g.Run(t.Context(), nil, args...); err != nil {
			t.Fatal(err)
		}
		g.Dir = dst
		return &Hub{Dir: dst, Git: g}
	}

	shallow := cloneInto("shallow", "--depth", "1")
	if _, _, err := Build(t.Context(), shallow); !errors.Is(err, ErrShallow) {
		t.Errorf("Build(shallow clone) = %v; want ErrShallow", err)
	}

	// A blobless clone would fetch every historical blob one at a time.
	r.git("config", "uploadpack.allowFilter", "true")
	blobless := cloneInto("blobless", "--filter=blob:none")
	if _, _, err := Build(t.Context(), blobless); !errors.Is(err, ErrPartialClone) {
		t.Errorf("Build(blobless clone) = %v; want ErrPartialClone", err)
	}

	m, _, err := Build(t.Context(), cloneInto("full"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, full) {
		t.Errorf("Build(full clone) =\n%s\nwant\n%s", dump(t, m), dump(t, full))
	}
}

// TestBuildAndReadAtRev: with Rev set, both reads stay on that commit
// whatever HEAD says.
func TestBuildAndReadAtRev(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false)
	r.write("packs/agents/AGENTS.md", text("v1"))
	c1 := r.commit("c1")
	r.write("packs/agents/AGENTS.md", text("v2"))
	r.commit("c2")
	h := r.hub()
	h.Rev = c1
	m, _, err := Build(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if vs := m.Versions("AGENTS.md", "agents"); m.HubCommit != c1 || len(vs) != 1 || vs[0] != ver(text("v1")) {
		t.Errorf("Build at c1 = %s", dump(t, m))
	}
	cur, _ := readCurrent(t, h, Committed)
	if got := cur["agents"]["AGENTS.md"].OID; got != blob(text("v1")) {
		t.Errorf("ReadCurrent at c1 = %s, want v1", got)
	}
}

func TestBuildSHA256(t *testing.T) {
	t.Parallel()
	r := newHubRepo(t, false, "--object-format=sha256")
	r.write("packs/agents/AGENTS.md", text("v1"))
	r.commit("c1")
	r.write("packs/agents/AGENTS.md", text("v2"))
	head := r.commit("c2")
	m, _, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	vs := m.Versions("AGENTS.md", "agents")
	if m.HubCommit != head || len(vs) != 2 || len(vs[0].OID) != 64 || vs[0].Size != int64(len(text("v1"))) {
		t.Fatalf("Build(sha256) = %s", dump(t, m))
	}
	data, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifestJSON(data)
	if err != nil || !reflect.DeepEqual(back, m) {
		t.Errorf("sha256 round trip = %+v, %v", back, err)
	}
}

// TestBuildManyCommits builds the manifest of a hub with 5000 commits,
// created in one git fast-import.
func TestBuildManyCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	t.Parallel()
	const n = 5000
	r := newHubRepo(t, false)
	var stream strings.Builder
	want := map[string][]Version{}
	for i := 1; i <= n; i++ {
		path := fmt.Sprintf("p%d/file%d.md", i%3, i%10)
		content := fmt.Sprintf("version %d of %s\n", i, path)
		msg := fmt.Sprintf("commit %d\n", i)
		fmt.Fprintf(&stream, "commit refs/heads/master\ncommitter t <t@example.com> %d +0000\ndata %d\n%s",
			1767225600+i, len(msg), msg)
		fmt.Fprintf(&stream, "M 100644 inline packs/%s\ndata %d\n%s\n", path, len(content), content)
		want[path] = append(want[path], ver(content))
	}
	r.gitIn([]byte(stream.String()), "fast-import", "--quiet")

	start := time.Now()
	m, skipped, err := Build(t.Context(), r.hub())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Build over %d commits took %v", n, time.Since(start))
	pairs := 0
	for _, byPack := range m.Paths {
		pairs += len(byPack)
	}
	if len(skipped) != 0 || len(m.Paths) != 10 || pairs != len(want) {
		t.Fatalf("Build() has %d paths and %d (path, pack) pairs, skipped %q; want 10 and %d",
			len(m.Paths), pairs, skipped, len(want))
	}
	for key, versions := range want {
		pack, path, _ := strings.Cut(key, "/")
		if got := m.Versions(path, pack); !slices.Equal(got, versions) {
			t.Errorf("%s: %d versions, want %d in commit order", key, len(got), len(versions))
		}
	}
}

// dump renders a manifest for failure messages.
func dump(t *testing.T, m *Manifest) string {
	t.Helper()
	var b strings.Builder
	if err := m.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
