package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The lifecycle of distribute through the command line, on
// the fake platform in git mode for GitHub, GitLab and Gitea: a hub opens
// pull requests in nine targets, people merge, decline, let a bot close,
// push to, Update-branch and cherry-pick them, the packs change, rebuilds
// come from a ticked control and from operations.yml, opt-in files change,
// the hub reverts a file, targets are dropped one by one and at once, and
// the hub changes its id. After every distribute the fake saw no forbidden
// transition, a second distribute writes nothing, and the write token
// never shows up in the output, the report, the stream, the pull requests,
// the commits or a git process but through GIT_CONFIG_VALUE_n.

// lifeTargets are the repositories of acme in the lifecycle, with their
// topics: tools gives the tools pack, revert the history pack.
var lifeTargets = []struct{ name, topic string }{
	{"merged", "tools"}, {"declined", ""}, {"stale", "tools"}, {"edited", "tools"}, {"updated", "tools"},
	{"picked", "tools"}, {"dropped", "tools"}, {"reverted", "revert"}, {"remembered", ""},
}

// lifeWorlds are the worlds of the running lifecycles by provider host,
// for the drivers lifecycle tests install.
var lifeWorlds sync.Map

// installLifeDrivers makes distribute's drivers the fake platforms of the
// lifecycle worlds, found by the provider's host, until the test ends.
func installLifeDrivers(t *testing.T) {
	t.Helper()
	drivers := distributeDrivers
	t.Cleanup(func() { distributeDrivers = drivers })
	driver := func(rp config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
		v, ok := lifeWorlds.Load(rp.Host)
		if !ok {
			return nil, fmt.Errorf("no fake platform on %s", rp.Host)
		}
		w := v.(*lifeWorld)
		if client == nil || c.Token != w.token {
			return nil, errors.New("unexpected credential")
		}
		return w.p.Writer(w.writer), nil
	}
	distributeDrivers = map[string]distributeDriver{"github": driver, "gitlab": driver, "gitea": driver}
}

// lifeWorld is one lifecycle: a hub and a fake platform of one flavor.
type lifeWorld struct {
	t      *testing.T
	flavor fake.Flavor
	host   string
	token  string
	p      *fake.Platform

	writer, person, bot platform.Account
	repos               map[string]platform.Repo

	hub     *repo
	id      string
	aliases []string
	exclude []string
	ops     string
	packs   map[string]string // hub path → content

	dir     string // the report and the stream
	tracker *canaryTracker
}

func newLifeWorld(t *testing.T, flavor fake.Flavor, port int) *lifeWorld {
	t.Helper()
	needDistributeGit(t)
	host := fmt.Sprintf("localhost:%d", port)
	p := fake.New(host, fake.WithFlavor(flavor), fake.WithClock(time.Now))
	w := &lifeWorld{
		t: t, flavor: flavor, host: host, p: p,
		token:  fmt.Sprintf("tmcanary%d-%s-7f3a91c0e5d24b68", port, flavor),
		writer: p.AddAccount("acme-write[bot]", platform.KindBot),
		person: p.AddAccount("jdoe", platform.KindUser),
		bot:    p.AddAccount("stale[bot]", platform.KindBot),
		repos:  map[string]platform.Repo{},
		id:     "acme-eng",
		packs: map[string]string{
			"packs/base/AGENTS.md":       text("base AGENTS.md v1"),
			"packs/base/docs/guide.md":   text("base guide v1"),
			"packs/tools/docs/tools.md":  text("tools v1"),
			"packs/history/docs/hist.md": text("history v1"),
		},
		dir: t.TempDir(),
	}
	srv, err := p.ServeGit(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetToken(w.writer, w.token)
	p.SetToken(w.person, "person-"+w.token[len("tmcanary"):])
	for _, tg := range lifeTargets {
		r := platform.Repo{Path: "acme/" + tg.name}
		if tg.topic != "" {
			r.Topics = []string{tg.topic}
		}
		r = p.AddRepo(r)
		p.SetFile(r.ID, planOptIn, []byte("version: 1\n"), "")
		p.SetFile(r.ID, "README.md", []byte("# "+tg.name+"\n"), "")
		p.GrantWrite(r.ID, w.writer)
		got, _ := p.RepoByID(r.ID)
		w.repos[tg.name] = got
	}
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	w.tracker = newCanaryTracker(t, w.token)
	lifeWorlds.Store(host, w)
	t.Cleanup(func() {
		lifeWorlds.Delete(host)
		w.tracker.stop()
		if err := srv.Close(); err != nil {
			t.Errorf("close the git server: %v", err)
		}
		if v := p.Violations(); len(v) > 0 {
			t.Errorf("forbidden transitions: %q", v)
		}
	})
	w.hub = newHub(t)
	w.commitHub("the hub")
	return w
}

// commitHub commits the hub's configuration and packs as they are now.
func (w *lifeWorld) commitHub(msg string) {
	w.t.Helper()
	h := w.hub
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nid: %s\n", w.id)
	if len(w.aliases) > 0 {
		fmt.Fprintf(&b, "branch_aliases: [%s]\n", strings.Join(w.aliases, ", "))
	}
	fmt.Fprintf(&b, "providers:\n  - id: gh\n    type: %s\n    url: http://%s\n    writer: acme-write[bot]\n", w.flavor, w.host)
	h.write("hub.yml", b.String())
	b.Reset()
	b.WriteString("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: acme\n  - org: acme\n    topics: [tools]\n    packs: [tools]\n" +
		"  - org: acme\n    topics: [revert]\n    packs: [history]\n")
	if len(w.exclude) > 0 {
		b.WriteString("exclude:\n")
		for _, name := range w.exclude {
			fmt.Fprintf(&b, "  - acme/%s\n", name)
		}
	}
	h.write("targets.yml", b.String())
	for path, content := range w.packs {
		h.write(path, content)
	}
	if w.ops != "" {
		h.write(config.OperationsFile, w.ops)
	} else if h.exists(config.OperationsFile) {
		h.rm(config.OperationsFile)
	}
	h.commit(msg)
}

// distribute runs distribute and fails the test unless it exits with code;
// it returns the report and the writes the fake saw.
func (w *lifeWorld) distribute(code int, extra ...string) (report.Delivery, []string) {
	w.t.Helper()
	w.p.ResetCalls()
	reportFile, stream := filepath.Join(w.dir, "report.json"), filepath.Join(w.dir, "stream.jsonl")
	args := append([]string{"distribute", "--hub", w.hub.dir, "--hub-fp", distFP, "--format", "json", "--report", reportFile, "--stream", stream}, extra...)
	res := runWith(w.t, map[string]string{"TOUCHMARK_GH_WRITE_TOKEN": w.token}, args...)
	if res.code != code {
		w.t.Fatalf("distribute %v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", extra, res.code, code, res.stdout, res.stderr)
	}
	w.tracker.text("stdout", res.stdout)
	w.tracker.text("stderr", res.stderr)
	for _, name := range []string{reportFile, stream} {
		data, err := os.ReadFile(name)
		if err != nil {
			w.t.Fatal(err)
		}
		w.tracker.text(filepath.Base(name), string(data))
	}
	if v := w.p.Violations(); len(v) > 0 {
		w.t.Errorf("forbidden transitions: %q", v)
	}
	var writes []string
	for _, c := range w.p.Writes() {
		if !strings.HasPrefix(c, "CreateLabel ") {
			writes = append(writes, c)
		}
	}
	return decodeDelivery(w.t, res.stdout), writes
}

// settle runs distribute, checks the outcomes of the targets named in want
// ("outcome:reason #pr", the number left out for none) and that a second
// run writes nothing; it returns the first run's report.
func (w *lifeWorld) settle(what string, want map[string]string, extra ...string) report.Delivery {
	w.t.Helper()
	rep, writes := w.distribute(exitOK, extra...)
	w.want(what, rep, want)
	w.t.Logf("%s: %d writes", what, len(writes))
	again, writes := w.distribute(exitOK)
	if len(writes) > 0 {
		w.t.Errorf("%s: a second distribute wrote %q (%s)", what, writes, lifeOutcomes(again))
	}
	return rep
}

// want checks the outcomes of the targets named in want.
func (w *lifeWorld) want(what string, rep report.Delivery, want map[string]string) {
	w.t.Helper()
	got := map[string]string{}
	for _, tg := range rep.Targets {
		s := string(tg.Outcome) + ":" + tg.Reason
		if tg.PR != nil {
			s += fmt.Sprintf(" #%d", tg.PR.Number)
		}
		got[strings.TrimPrefix(tg.Path, "acme/")] = s
		if tg.Outcome == report.OutcomeFailed {
			w.t.Errorf("%s: %s failed: %q", what, tg.Path, tg.Warnings)
		}
	}
	for _, name := range sortedKeys(want) {
		if got[name] != want[name] {
			w.t.Errorf("%s: %s is %q, want %q (%s)", what, name, got[name], want[name], lifeOutcomes(rep))
		}
	}
}

// lifeOutcomes renders the outcomes of a report for messages.
func lifeOutcomes(rep report.Delivery) string {
	var out []string
	for _, tg := range rep.Targets {
		out = append(out, fmt.Sprintf("%s %s:%s", tg.Path, tg.Outcome, tg.Reason))
	}
	return strings.Join(out, ", ")
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// pr returns pull request n of target name.
func (w *lifeWorld) pr(name string, n int64) platform.PR {
	w.t.Helper()
	pr := w.p.PR(w.repos[name].ID, n)
	if pr.Number != n {
		w.t.Fatalf("%s has no pull request #%d", name, n)
	}
	return pr
}

// push is a person's push of files to branch of target name.
func (w *lifeWorld) push(name, branch string, files map[string]string) string {
	w.t.Helper()
	m := map[string][]byte{}
	for path, content := range files {
		if content == "" {
			m[path] = nil
		} else {
			m[path] = []byte(content)
		}
	}
	head, err := w.p.PushFiles(w.repos[name].ID, branch, m, w.person, time.Time{})
	if err != nil {
		w.t.Fatalf("PushFiles(%s, %s): %v", name, branch, err)
	}
	return head
}

// tick ticks control in pull request n of target name.
func (w *lifeWorld) tick(name string, n int64, control string) {
	w.t.Helper()
	line := prbody.ControlLine(control)
	pr := w.pr(name, n)
	if !strings.Contains(pr.Body, line) {
		w.t.Fatalf("#%d of %s has no %s control:\n%s", n, name, control, pr.Body)
	}
	w.p.UpdatePR(w.repos[name].ID, n, func(pr *platform.PR) {
		pr.Body = strings.Replace(pr.Body, line, strings.Replace(line, "- [ ]", "- [x]", 1), 1)
	})
}

// cherryPick puts the commits of open pull request n of target name onto a
// commit of the person's own, as a cherry-pick of touchmark's commit onto
// their work would, and returns that commit.
func (w *lifeWorld) cherryPick(name string, n int64) string {
	w.t.Helper()
	id := w.repos[name].ID
	mine := w.push(name, "feature/mine", map[string]string{"src/mine.txt": text("my own work")})
	w.p.UpdatePR(id, n, func(pr *platform.PR) { pr.Base = "feature/mine" })
	if _, err := w.p.RebaseBranch(id, n, w.person, time.Time{}); err != nil {
		w.t.Fatalf("RebaseBranch: %v", err)
	}
	w.p.UpdatePR(id, n, func(pr *platform.PR) { pr.Base = "main" })
	w.p.DeleteBranch(id, "feature/mine")
	if err := w.p.Err(); err != nil {
		w.t.Fatal(err)
	}
	return mine
}

// reachable reports whether commit is reachable from a branch of target
// name on the platform's side.
func (w *lifeWorld) reachable(name, commit string) bool {
	w.t.Helper()
	g := gitx.New(w.p.GitDir(w.repos[name].ID))
	out, err := g.Run(w.t.Context(), nil, "branch", "--contains", commit)
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func TestLifecycle(t *testing.T) {
	needDistributeGit(t)
	// Every run starts dozens of git processes, which cost many times more
	// on Windows and macOS (where CI runs the suite without the race
	// detector); the Linux job runs the lifecycle.
	if runtime.GOOS != "linux" && os.Getenv("TOUCHMARK_HEAVY_TESTS") == "" {
		t.Skipf("a heavy test: it runs on Linux (set TOUCHMARK_HEAVY_TESTS=1 to run it on %s)", runtime.GOOS)
	}
	installLifeDrivers(t)
	for i, flavor := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea} {
		t.Run(string(flavor), func(t *testing.T) {
			t.Parallel()
			lifecycle(t, newLifeWorld(t, flavor, 3001+i))
		})
	}
}

// lifecycle runs the lifecycle in w.
func lifecycle(t *testing.T, w *lifeWorld) {
	const syncBranch = "touchmark/acme-eng"
	all := func(s string) map[string]string {
		m := map[string]string{}
		for _, tg := range lifeTargets {
			m[tg.name] = s
		}
		return m
	}

	// 1. The hub's first run opens a pull request in every target.
	w.settle("the first run", all("opened: #1"))

	// 2. People: merge one, decline one, a stale bot closes one (on Gitea
	// the closer is unknown: a decline), push to one sync branch, press
	// Update branch on another, cherry-pick touchmark's commit onto their own
	// work in a third, and decline two more.
	if _, err := w.p.MergePR(w.repos["merged"].ID, 1, fake.MergeCommit, w.person, time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"declined", "reverted", "remembered"} {
		w.p.SetPRState(w.repos[name].ID, 1, platform.Closed, &w.person, time.Time{})
	}
	w.p.SetPRState(w.repos["stale"].ID, 1, platform.Closed, &w.bot, time.Time{})
	pushed := w.push("edited", syncBranch, map[string]string{"notes.md": text("the team's notes on the sync branch")})
	w.push("updated", "main", map[string]string{"src/app.txt": text("the team's app")})
	if _, err := w.p.UpdateBranch(w.repos["updated"].ID, 1, w.person, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mine := w.cherryPick("picked", 1)
	stale := "deferred:cooldown #1"
	if w.flavor == fake.Gitea {
		stale = "declined: #1"
	}
	w.settle("after people acted", map[string]string{
		"merged": "unchanged:", "declined": "declined: #1", "stale": stale, "edited": "blocked:edited #1",
		"updated": "unchanged: #1", "picked": "blocked:edited #1", "dropped": "unchanged: #1",
		"reverted": "declined: #1", "remembered": "declined: #1",
	})
	for _, name := range []string{"declined", "remembered"} {
		if m, _ := marker.Find(w.pr(name, 1).Body, []string{distFP}); !m.Data.Ack || len(w.p.Comments(w.repos[name].ID, 1)) != 1 {
			t.Errorf("%s #1: ack %v, %d comments", name, m.Data.Ack, len(w.p.Comments(w.repos[name].ID, 1)))
		}
	}

	// 3. A pack changes: the merged target gets a fresh pull request, the
	// declined ones stay declined (their packs did not change), the edited
	// and cherry-picked branches stay paused (never overwritten), the
	// Update-branch one is updated, the bot's close is proposed again with
	// the new content, and the history pack's new version reaches the
	// target that declined the old one.
	w.packs["packs/tools/docs/tools.md"] = text("tools v2")
	w.packs["packs/history/docs/hist.md"] = text("history v2")
	w.commitHub("tools v2, history v2")
	w.settle("the tools pack changed", map[string]string{
		"merged": "opened: #2", "declined": "declined: #1", "stale": "opened: #2", "edited": "blocked:edited #1",
		"updated": "updated:content #1", "picked": "blocked:edited #1", "dropped": "updated:content #1",
		"reverted": "opened: #2", "remembered": "declined: #1",
	})
	if !w.reachable("edited", pushed) || !w.reachable("picked", mine) {
		t.Errorf("a person's commit was lost: edited %v, picked %v", w.reachable("edited", pushed), w.reachable("picked", mine))
	}

	// 4. Rebuilds: a ticked control, and a recreate in operations.yml.
	w.tick("edited", 1, prbody.ControlRecreate)
	w.settle("the rebuild control ticked", map[string]string{"edited": "updated:recreate #1", "picked": "blocked:edited #1"})
	head := w.p.Branch(w.repos["picked"].ID, syncBranch)
	w.ops = fmt.Sprintf("version: 1\nrecreate:\n  - target: gh:acme/picked\n    head: %s\n", head)
	w.commitHub("rebuild acme/picked")
	w.settle("operations.yml recreates", map[string]string{"picked": "updated:recreate #1", "edited": "unchanged: #1"})

	// 5. A decline holds until the team changes its opt-in file: a new
	// comment in it changes nothing, an ignore entry does.
	w.push("declined", "main", map[string]string{planOptIn: "version: 1\n# we keep an eye on the guide\n"})
	w.settle("a comment in the opt-in file", map[string]string{"declined": "declined: #1"})
	w.push("declined", "main", map[string]string{planOptIn: "version: 1\nignore: [docs/guide.md]\n"})
	w.settle("an ignore entry in the opt-in file", map[string]string{"declined": "opened: #2", "remembered": "declined: #1"})

	// 6. The hub goes back to a version the target declined, after the team
	// merged a later one: a new pair, delivered.
	if _, err := w.p.MergePR(w.repos["reverted"].ID, 2, fake.MergeSquash, w.person, time.Time{}); err != nil {
		t.Fatal(err)
	}
	w.packs["packs/history/docs/hist.md"] = text("history v1")
	w.commitHub("history back to v1")
	w.settle("the hub reverts", map[string]string{"reverted": "opened: #3"})

	// 7. A target dropped from targets.yml: the sweep closes its pull
	// request.
	w.exclude = []string{"dropped"}
	w.commitHub("drop acme/dropped")
	w.settle("a target dropped", map[string]string{"dropped": "closed:target-dropped #1"})
	if pr := w.pr("dropped", 1); pr.State != platform.Closed {
		t.Errorf("dropped #1 is %s", pr.State)
	}

	// 8. Dropping six targets at once is more than the mass-close guard
	// lets a run close (max(5, 10% of the open ones)): nothing is closed
	// until the maintainer allows it.
	w.exclude = []string{"dropped", "edited", "merged", "picked", "reverted", "stale", "updated"}
	w.commitHub("drop six targets")
	rep, writes := w.distribute(exitFailed)
	blocked := map[string]string{}
	for _, name := range []string{"edited", "merged", "picked", "reverted", "stale", "updated"} {
		n := int64(1)
		switch name {
		case "merged", "stale":
			n = 2
		case "reverted":
			n = 3
		}
		blocked[name] = fmt.Sprintf("blocked:mass-close #%d", n)
		if pr := w.pr(name, n); pr.State != platform.Open {
			t.Errorf("%s #%d is %s after a refused mass close", name, n, pr.State)
		}
	}
	w.want("a mass close", rep, blocked)
	if len(writes) > 0 {
		t.Errorf("a refused mass close wrote %q", writes)
	}
	closed := map[string]string{}
	for name, s := range blocked {
		closed[name] = strings.Replace(s, "blocked:mass-close", "closed:target-dropped", 1)
	}
	w.settle("a mass close allowed", closed, "--allow-mass-close", "10")

	// 9. The hub changes its id: the old sync branch becomes an alias; its
	// open pull request is kept (its description names the new id), memory
	// holds, nothing opens or closes.
	w.id, w.aliases = "acme-platform", []string{syncBranch}
	w.exclude = []string{"dropped", "edited", "merged", "picked", "reverted", "stale", "updated"}
	w.commitHub("rename the hub")
	w.settle("the hub's id changed", map[string]string{"declined": "updated:body #2", "remembered": "declined: #1"})

	// 10. A pack change after the rename: the pull request on the alias is
	// updated on its branch, a new one opens on the new sync branch.
	w.packs["packs/base/AGENTS.md"] = text("base AGENTS.md v2")
	w.commitHub("AGENTS.md v2")
	w.settle("a change after the rename", map[string]string{"declined": "updated:content #2", "remembered": "opened: #2"})
	if pr := w.pr("remembered", 2); pr.Head != "touchmark/acme-platform" {
		t.Errorf("the new pull request is on %s", pr.Head)
	}
	if pr := w.pr("declined", 2); pr.Head != syncBranch || pr.HeadSHA != w.p.Branch(w.repos["declined"].ID, syncBranch) {
		t.Errorf("the alias pull request: head %s at %s", pr.Head, pr.HeadSHA)
	}

	// The token never reached the platform's texts or commits.
	w.scanPlatform()
}

// scanPlatform checks that no pull request title, body, label or comment,
// no branch name and no commit message on the platform holds the token.
func (w *lifeWorld) scanPlatform() {
	w.t.Helper()
	for _, name := range sortedKeys(w.repos) {
		r := w.repos[name]
		for _, pr := range w.p.PRList(r.ID) {
			w.tracker.text(fmt.Sprintf("%s #%d", name, pr.Number), pr.Title+"\n"+pr.Body+"\n"+strings.Join(pr.Labels, "\n")+"\n"+pr.Head)
			for _, c := range w.p.Comments(r.ID, pr.Number) {
				w.tracker.text(fmt.Sprintf("a comment on %s #%d", name, pr.Number), c.Body)
			}
		}
		g := gitx.New(w.p.GitDir(r.ID))
		out, err := g.Run(w.t.Context(), nil, "log", "--all", "--format=%H%n%B%n%(trailers)")
		if err != nil {
			w.t.Fatalf("git log of %s: %v", name, err)
		}
		w.tracker.text("the commits of "+name, string(out))
		refs, err := g.Run(w.t.Context(), nil, "for-each-ref", "--format=%(refname)")
		if err != nil {
			w.t.Fatal(err)
		}
		w.tracker.text("the branches of "+name, string(refs))
	}
}
