package distribute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// TestPlanOutcomes plans a world with a target for every outcome plan can
// reach on its own.
func TestPlanOutcomes(t *testing.T) {
	w := newWorld(t)
	base := keyOfMissing("base")

	w.optedIn("acme/billing", nil)
	api := w.optedIn("acme/api", nil)
	w.ownPR(api, base)
	sdk := w.optedIn("acme/sdk", topics("python"))
	w.ownPR(sdk, staleKey)
	old := w.optedIn("acme/old", nil, "AGENTS.md", agentsV2, "docs/guide.md", guideV1)
	w.ownPR(old, base)
	w.optedIn("acme/current", nil, "AGENTS.md", agentsV2, "docs/guide.md", guideV1)
	w.optedIn("acme/outdated", nil, "AGENTS.md", agentsV1)
	w.repo("acme/web", nil, "README.md", "not opted in")
	link := w.repo("acme/link", nil, "README.md", "the target")
	w.p.SetSymlink(link.ID, optInName, "README.md")
	w.repo("acme/huge", nil, optInName, "version: 1\n"+strings.Repeat("# padding\n", 7000))
	w.repo("acme/broken-opt-in", nil, optInName, "version: 2\n")
	w.repo("acme/unknown-pack", nil, optInName, "version: 1\npacks: [nope]\n")
	w.optedIn("acme/archived", func(r *platform.Repo) { r.Archived = true })
	w.optedIn("acme/disabled", func(r *platform.Repo) { r.Disabled = true })
	w.repo("acme/empty", nil)
	w.optedIn("acme/mirror", func(r *platform.Repo) { r.Mirror = true })
	w.optedIn("acme/pending", func(r *platform.Repo) { r.PendingDelete = true })
	w.optedIn("acme/no-prs", func(r *platform.Repo) { r.PRsDisabled = true })
	w.optedIn("acme/sha256", func(r *platform.Repo) { r.ObjectFormat = "sha256" })
	cli := w.optedIn("acme/cli", nil)
	w.pr(cli, platform.PR{Head: branch, Author: w.person, Title: "my own work"})
	forked := w.optedIn("acme/forked", nil)
	w.pr(forked, platform.PR{Head: branch, Author: w.person, HeadRepoID: "999", Title: "copied", Body: body(t, base, hubFP)})
	badMarker := w.optedIn("acme/bad-marker", nil)
	w.pr(badMarker, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync engineering assets", Body: "no marker"})
	otherHub := w.optedIn("acme/other-hub", nil)
	w.pr(otherHub, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: body(t, base, otherFP)})
	declined := w.optedIn("acme/declined", nil)
	n := w.ownPR(declined, base)
	w.p.SetPRState(declined.ID, n, platform.Closed, &w.person, fake.Epoch)
	known := w.optedIn("acme/known", nil)
	w.pr(known, platform.PR{Head: branch, Author: w.known, Title: "chore: sync", Body: body(t, base, hubFP)})
	moved := w.optedIn("acme/moved", nil)
	w.pr(moved, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: body(t, base, prevFP)})
	aliased := w.optedIn("acme/aliased", nil)
	w.pr(aliased, platform.PR{Head: alias, Author: w.writer, Title: "chore: sync", Body: body(t, staleKey, hubFP)})
	foreignAlias := w.optedIn("acme/foreign-alias", nil)
	w.pr(foreignAlias, platform.PR{Head: alias, Author: w.person, Title: "someone else's"})
	foreignNoDiff := w.optedIn("acme/foreign-no-diff", nil, "AGENTS.md", agentsV2, "docs/guide.md", guideV1)
	w.pr(foreignNoDiff, platform.PR{Head: branch, Author: w.person, Title: "someone else's"})
	w.optedIn("acme/orphan", nil, "docs/python.md", pythonV1)
	w.optedIn("acme/legacy", nil)
	w.optedIn("other/outside", nil)
	// I3: someone else's open pull request on the branch blocks it even
	// with an own open pull request there.
	ownAndForeign := w.optedIn("acme/own-and-foreign", nil)
	w.ownPR(ownAndForeign, staleKey)
	w.pr(ownAndForeign, platform.PR{Head: branch, Base: "develop", Author: w.person, Title: "someone else's, to develop"})
	// An own pull request on an alias is kept on the alias: someone else's
	// on hub.yml's branch does not block it.
	aliasOwn := w.optedIn("acme/alias-own", nil)
	w.pr(aliasOwn, platform.PR{Head: alias, Author: w.writer, Title: "chore: sync", Body: body(t, staleKey, hubFP)})
	w.pr(aliasOwn, platform.PR{Head: branch, Author: w.person, Title: "someone else's"})
	// Own pull requests on the branch and on an alias: the branch's counts,
	// though the alias's is newer.
	bothOwn := w.optedIn("acme/both-own", nil)
	w.ownPR(bothOwn, base)
	w.pr(bothOwn, platform.PR{Head: alias, Author: w.writer, Title: "chore: sync", Body: body(t, staleKey, hubFP)})
	// The default branch was renamed after the pull request was opened.
	renamedBase := w.optedIn("acme/renamed-base", nil)
	w.pr(renamedBase, platform.PR{Head: branch, Base: "master", Author: w.writer, Title: "chore: sync", Body: body(t, base, hubFP)})
	// Another hub's pull request where this hub proposes nothing.
	otherHubCurrent := w.optedIn("acme/other-hub-current", nil, "AGENTS.md", agentsV2, "docs/guide.md", guideV1)
	w.pr(otherHubCurrent, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: body(t, base, otherFP)})

	rep := w.plan(w.deps())

	want(t, rep, "gh:acme/billing", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/api", report.OutcomeUnchanged, "", 1)
	want(t, rep, "gh:acme/sdk", report.OutcomeUpdated, "content", 1)
	want(t, rep, "gh:acme/old", report.OutcomeClosed, "no-diff", 1)
	want(t, rep, "gh:acme/current", report.OutcomeUnchanged, "", 0)
	want(t, rep, "gh:acme/outdated", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/web", report.OutcomeSkipped, "not-opted-in", 0)
	want(t, rep, "gh:acme/link", report.OutcomeSkipped, "unsafe-opt-in", 0)
	want(t, rep, "gh:acme/huge", report.OutcomeSkipped, "unsafe-opt-in", 0)
	want(t, rep, "gh:acme/broken-opt-in", report.OutcomeBlocked, "opt-in-invalid", 0)
	want(t, rep, "gh:acme/unknown-pack", report.OutcomeBlocked, "opt-in-invalid", 0)
	want(t, rep, "gh:acme/archived", report.OutcomeSkipped, "archived", 0)
	want(t, rep, "gh:acme/disabled", report.OutcomeSkipped, "disabled", 0)
	want(t, rep, "gh:acme/empty", report.OutcomeSkipped, "empty", 0)
	want(t, rep, "gh:acme/mirror", report.OutcomeSkipped, "mirror", 0)
	want(t, rep, "gh:acme/pending", report.OutcomeSkipped, "pending-deletion", 0)
	want(t, rep, "gh:acme/no-prs", report.OutcomeSkipped, "prs-disabled", 0)
	want(t, rep, "gh:acme/sha256", report.OutcomeSkipped, "sha256", 0)
	want(t, rep, "gh:acme/cli", report.OutcomeBlocked, "branch-in-use", 1)
	// A fork's pull request with a copied marker is not ours, and its
	// branch lives in the fork: the target gets its own pull request.
	forkedRes := want(t, rep, "gh:acme/forked", report.OutcomeOpened, "", 0)
	if !hasWarning(forkedRes.Warnings, "#1 from another repository carries this hub's marker") {
		t.Errorf("acme/forked: warnings %q", forkedRes.Warnings)
	}
	want(t, rep, "gh:acme/bad-marker", report.OutcomeBlocked, "marker-invalid", 1)
	// A valid marker of another hub, by our writer on our branch, is someone
	// else's pull request: the branch is in use, and a recreate never takes it
	// over.
	want(t, rep, "gh:acme/other-hub", report.OutcomeBlocked, "branch-in-use", 1)
	want(t, rep, "gh:acme/other-hub-current", report.OutcomeUnchanged, "", 0)
	want(t, rep, "gh:acme/own-and-foreign", report.OutcomeBlocked, "branch-in-use", 2)
	want(t, rep, "gh:acme/alias-own", report.OutcomeUpdated, "content", 1)
	want(t, rep, "gh:acme/both-own", report.OutcomeUnchanged, "", 1)
	want(t, rep, "gh:acme/renamed-base", report.OutcomeUpdated, "base-renamed", 1)
	// The snapshot-only plan reads no memory: a closed PR changes nothing.
	want(t, rep, "gh:acme/declined", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/known", report.OutcomeUnchanged, "", 1)
	want(t, rep, "gh:acme/moved", report.OutcomeUnchanged, "", 1)
	want(t, rep, "gh:acme/aliased", report.OutcomeUpdated, "content", 1)
	want(t, rep, "gh:acme/foreign-alias", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/foreign-no-diff", report.OutcomeUnchanged, "", 0)
	orphan := want(t, rep, "gh:acme/orphan", report.OutcomeOpened, "", 0)
	if !slices.Equal(orphan.Orphaned, []string{"docs/python.md"}) {
		t.Errorf("acme/orphan: orphaned %q, want docs/python.md", orphan.Orphaned)
	}
	for _, tg := range rep.Targets {
		if tg.Path == "acme/legacy" || tg.Path == "other/outside" {
			t.Errorf("%s is in the report", tg.Path)
		}
	}
	if len(rep.Targets) != 34 {
		t.Errorf("%d targets, want 34", len(rep.Targets))
	}

	// What each target would receive.
	billing := targetOf(t, rep, "gh:acme/billing")
	if billing.Key != base || !slices.Equal(billing.Packs, []string{"base"}) || billing.Changes != (report.ChangeCounts{Create: 2}) {
		t.Errorf("acme/billing: key %s packs %q changes %+v; want %s [base] 2 creates", billing.Key, billing.Packs, billing.Changes, base)
	}
	sdkRes := targetOf(t, rep, "gh:acme/sdk")
	if sdkRes.Key != keyOfMissing("base", "python") || !slices.Equal(sdkRes.Packs, []string{"base", "python"}) {
		t.Errorf("acme/sdk: key %s packs %q", sdkRes.Key, sdkRes.Packs)
	}
	outdated := targetOf(t, rep, "gh:acme/outdated")
	if outdated.Changes != (report.ChangeCounts{Create: 1, Update: 1}) {
		t.Errorf("acme/outdated: changes %+v", outdated.Changes)
	}
	if k := targetOf(t, rep, "gh:acme/current").Key; k != "" {
		t.Errorf("acme/current: key %q, want none", k)
	}
	if pr := targetOf(t, rep, "gh:acme/api").PR; pr.URL != "https://github.com/acme/api/pull/1" || pr.State != "open" {
		t.Errorf("acme/api: PR %+v", pr)
	}

	// Writes on GitHub: opened 3, and 1 more for the label where no pull
	// request of ours was ever opened; updated 2; closed 3.
	opened, updated, closed := rep.Summary[report.OutcomeOpened], rep.Summary[report.OutcomeUpdated], rep.Summary[report.OutcomeClosed]
	if opened != 6 || updated != 4 || closed != 1 {
		t.Errorf("summary %v", rep.Summary)
	}
	sum := 0
	for _, tg := range rep.Targets {
		sum += tg.Writes
	}
	if got, want := rep.Cost["gh"], 4*opened-1+2*updated+3*closed; got != want || sum != want {
		t.Errorf("cost %d, sum of targets %d, want %d", got, sum, want)
	}
	for ref, n := range map[string]int{"gh:acme/billing": 4, "gh:acme/forked": 4, "gh:acme/declined": 3, "gh:acme/sdk": 2, "gh:acme/cli": 0} {
		if w := targetOf(t, rep, ref).Writes; w != n {
			t.Errorf("%s: %d writes, want %d", ref, w, n)
		}
	}

	// The report as a whole.
	if rep.Command != "plan" || rep.Engine != "test" || rep.Outcome != report.Completed {
		t.Errorf("report %s %s %s", rep.Command, rep.Engine, rep.Outcome)
	}
	if rep.Hub != (report.DeliveryHub{ID: "acme-eng", Fingerprint: hubFP, Commit: hubCommit}) {
		t.Errorf("hub %+v", rep.Hub)
	}
	wantInfo := report.ProviderInfo{ID: "gh", Type: "github", Host: "github.com", Reader: "acme-read[bot]",
		Writer: "acme-write[bot]", WriteCheck: "not checked", ResolveComplete: true}
	if len(rep.Providers) != 1 || !equalInfo(rep.Providers[0], wantInfo) {
		t.Errorf("providers %+v", rep.Providers)
	}
	if !slices.Equal(rep.Warnings, []string{"hub head not checked: a local run cannot read the tip of the hub's default branch"}) {
		t.Errorf("warnings %q", rep.Warnings)
	}
	if rep.Sweep != (report.SweepInfo{}) {
		t.Errorf("sweep %+v", rep.Sweep)
	}
	if code := rep.ExitCode(); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	rep.Strict = true
	if code := rep.ExitCode(); code != 3 {
		t.Errorf("strict exit code %d, want 3", code)
	}
	// Plan reads only: nothing was written.
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("plan wrote: %q", writes)
	}
	// An excluded repository is never read, a skipped one is not read
	// further than classify.
	for _, c := range w.p.Calls() {
		if strings.Contains(c, "acme/legacy") || strings.Contains(c, "acme/archived") {
			t.Errorf("call %q", c)
		}
	}
}

func equalInfo(a, b report.ProviderInfo) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Host == b.Host && a.Reader == b.Reader && a.Writer == b.Writer &&
		a.WriteCheck == b.WriteCheck && a.ResolveComplete == b.ResolveComplete && slices.Equal(a.Missing, b.Missing) && a.Error == b.Error
}

// A public hub never names its non-public targets, and does not look into
// them, unless it delivers to them.
func TestPlanPrivateInPublicHub(t *testing.T) {
	setup := func(t *testing.T) *world {
		w := newWorld(t)
		// A hub pull request in GitHub Actions: the head guard does not
		// apply.
		w.ctx = hubch.Context{CI: hubch.GitHubActions, Host: "github.com", RepoID: "712345678", Visibility: "public",
			DefaultBranch: "main", RefName: "41/merge", Event: "pull_request"}
		w.optedIn("acme/secret-sauce", func(r *platform.Repo) { r.Visibility = "private"; r.Archived = true })
		w.optedIn("acme/inner-source", func(r *platform.Repo) { r.Visibility = "internal" })
		w.optedIn("acme/open", nil)
		return w
	}
	t.Run("skip", func(t *testing.T) {
		w := setup(t)
		d := w.deps()
		d.Fingerprint = "" // from the context
		rep := w.plan(d)
		want(t, rep, "gh:acme/open", report.OutcomeOpened, "", 0)
		hidden := 0
		for _, tg := range rep.Targets {
			if tg.Reason != report.ReasonPrivate {
				continue
			}
			hidden++
			if tg.Path != "" || tg.RepoID != "" || tg.Outcome != report.OutcomeSkipped || tg.PR != nil || tg.Packs != nil || tg.Warnings != nil {
				t.Errorf("a private target is described: %+v", tg)
			}
		}
		if hidden != 2 || len(rep.Targets) != 3 {
			t.Errorf("%d hidden of %d targets, want 2 of 3", hidden, len(rep.Targets))
		}
		if rep.Hub.Fingerprint != hubFP {
			t.Errorf("fingerprint %q, want the context's %s", rep.Hub.Fingerprint, hubFP)
		}
		if len(rep.Warnings) != 0 {
			t.Errorf("warnings %q", rep.Warnings)
		}
		data, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secret-sauce", "inner-source"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("the report names %s:\n%s", secret, data)
			}
		}
		for _, c := range w.p.Calls() {
			if strings.Contains(c, "secret-sauce") || strings.Contains(c, "inner-source") {
				t.Errorf("a private target was read: %q", c)
			}
		}
	})
	t.Run("deliver", func(t *testing.T) {
		w := setup(t)
		w.hubYML += "security:\n  private_targets_in_public_hub: deliver\n"
		rep := w.plan(w.deps())
		want(t, rep, "gh:acme/secret-sauce", report.OutcomeSkipped, "archived", 0)
		want(t, rep, "gh:acme/inner-source", report.OutcomeOpened, "", 0)
	})
	t.Run("private hub", func(t *testing.T) {
		w := setup(t)
		w.ctx.Visibility = "private"
		rep := w.plan(w.deps())
		want(t, rep, "gh:acme/inner-source", report.OutcomeOpened, "", 0)
	})
	// A CI event that does not tell the hub's visibility (a scheduled run
	// with an empty payload on Gitea, a payload not mounted into a
	// container) never names non-public targets: the hub counts as public.
	for _, ci := range []hubch.CI{hubch.GitHubActions, hubch.GiteaActions, hubch.ForgejoActions, hubch.GitLabCI} {
		t.Run("unknown visibility on "+string(ci), func(t *testing.T) {
			w := setup(t)
			w.ctx.CI, w.ctx.Visibility = ci, ""
			rep := w.plan(w.deps())
			want(t, rep, "gh:acme/open", report.OutcomeOpened, "", 0)
			if rep.Summary[report.OutcomeSkipped] != 2 {
				t.Errorf("summary %v", rep.Summary)
			}
			data, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "secret-sauce") || strings.Contains(string(data), "inner-source") {
				t.Errorf("the report names a private target:\n%s", data)
			}
			if !hasWarning(rep.Warnings, "the hub's visibility is unknown") {
				t.Errorf("warnings %q", rep.Warnings)
			}
		})
	}
	// A CI touchmark does not recognize (CI=true, Deps.InCI) is a CI all the
	// same: an unknown visibility counts as public there too. Such a run
	// once named private targets in its report files.
	t.Run("unknown visibility in another CI", func(t *testing.T) {
		w := setup(t)
		w.ctx = hubch.Context{CI: hubch.Local}
		d := w.deps()
		d.InCI = true
		rep := w.plan(d)
		data, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret-sauce") || strings.Contains(string(data), "inner-source") ||
			!hasWarning(rep.Warnings, "the hub's visibility is unknown") {
			t.Errorf("the report names a private target, or does not warn:\n%s", data)
		}
	})
	t.Run("unknown visibility, deliver", func(t *testing.T) {
		w := setup(t)
		w.ctx.Visibility = ""
		w.hubYML += "security:\n  private_targets_in_public_hub: deliver\n"
		rep := w.plan(w.deps())
		want(t, rep, "gh:acme/inner-source", report.OutcomeOpened, "", 0)
		if hasWarning(rep.Warnings, "visibility is unknown") {
			t.Errorf("warnings %q", rep.Warnings)
		}
	})
	t.Run("unknown visibility, local", func(t *testing.T) {
		w := setup(t)
		w.ctx = hubch.Context{CI: hubch.Local}
		rep := w.plan(w.deps())
		want(t, rep, "gh:acme/inner-source", report.OutcomeOpened, "", 0)
	})
}

// New pull requests beyond max_new_prs_per_run wait for the next run, in
// targets.yml order.
func TestPlanRolloutLimit(t *testing.T) {
	w := newWorld(t)
	w.hubYML += "limits:\n  max_new_prs_per_run: 2\n"
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - repo: acme/zulu
  - org: acme
  - repo: acme/alpha
`
	for _, name := range []string{"alpha", "beta", "gamma", "zulu"} {
		w.optedIn("acme/"+name, nil)
	}
	api := w.optedIn("acme/api", nil)
	w.ownPR(api, staleKey)
	rep := w.plan(w.deps())
	want(t, rep, "gh:acme/zulu", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/alpha", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/beta", report.OutcomeDeferred, "rollout-limit", 0)
	want(t, rep, "gh:acme/gamma", report.OutcomeDeferred, "rollout-limit", 0)
	want(t, rep, "gh:acme/api", report.OutcomeUpdated, "content", 1)
	if rep.Cost["gh"] != 2*4+2 {
		t.Errorf("cost %d, want 10: two first pull requests and an update", rep.Cost["gh"])
	}
	if w := targetOf(t, rep, "gh:acme/beta").Writes; w != 0 {
		t.Errorf("a deferred target costs %d writes", w)
	}

	// max_new_prs_per_run: 0 opens nothing.
	w.hubYML = defaultHubYML + "limits:\n  max_new_prs_per_run: 0\n"
	rep = w.plan(w.deps())
	if rep.Summary[report.OutcomeOpened] != 0 || rep.Summary[report.OutcomeDeferred] != 4 {
		t.Errorf("summary %v", rep.Summary)
	}

	// Without labels in hub.yml, a first pull request creates none.
	w.hubYML = defaultHubYML + "pr:\n  labels: []\n"
	rep = w.plan(w.deps())
	if w := targetOf(t, rep, "gh:acme/zulu").Writes; w != 3 {
		t.Errorf("acme/zulu without labels: %d writes, want 3", w)
	}
}

// The rollout limit follows targets.yml across providers, not the order of
// the providers in hub.yml.
func TestPlanRolloutAcrossProviders(t *testing.T) {
	w := newWorld(t)
	w.hubYML = `version: 1
id: acme-eng
limits:
  max_new_prs_per_run: 1
providers:
  - id: gh
    type: github
    writer: acme-write[bot]
  - id: gh2
    type: github
    writer: acme-old[bot]
`
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - repo: gh2:acme/a
  - repo: gh:acme/b
`
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil)
	hub, _, err := config.ParseHub([]byte(w.hubYML))
	if err != nil {
		t.Fatal(err)
	}
	d := w.deps()
	d.Providers = w.providers(hub, map[string]platform.Reader{"github.com": w.p.Reader(w.reader)})
	rep := w.plan(d)
	want(t, rep, "gh2:acme/a", report.OutcomeOpened, "", 0)
	want(t, rep, "gh:acme/b", report.OutcomeDeferred, "rollout-limit", 0)
}

// A platform error of one target fails that target only.
func TestPlanTargetErrors(t *testing.T) {
	transient := &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	limited := &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests, Err: errors.New("slow down")}
	denied := &platform.Error{Op: "test", Class: platform.ClassPermission, Status: http.StatusForbidden, Err: errors.New("forbidden")}
	// A classified error keeps its class over the ErrNotFound it wraps: it
	// is not a missing file or an empty repository.
	authOverNotFound := &platform.Error{Op: "test", Class: platform.ClassAuth, Status: http.StatusUnauthorized, Err: platform.ErrNotFound}
	cases := []struct {
		method  string
		err     error
		outcome report.Outcome
		reason  string
		// times is how often the call fails (a transient failure is tried
		// readAttempts times, a rate limit after each pause until the
		// provider is out of budget); rest is the outcome of the other target
		// (a provider out of budget takes no more calls).
		times int
		rest  report.Outcome
	}{
		{"ReadFile", errAuth, report.OutcomeFailed, "auth", 1, report.OutcomeOpened},
		{"ReadFile", denied, report.OutcomeFailed, "access", 1, report.OutcomeOpened},
		{"ReadFile", authOverNotFound, report.OutcomeFailed, "auth", 1, report.OutcomeOpened},
		{"ReadFile", transient, report.OutcomeFailed, "transient", readAttempts, report.OutcomeOpened},
		{"Remote", transient, report.OutcomeFailed, "transient", readAttempts, report.OutcomeOpened},
		{"Snapshot", errors.New("fetch: early EOF"), report.OutcomeFailed, "git", 1, report.OutcomeOpened},
		{"Snapshot", transient, report.OutcomeFailed, "transient", readAttempts, report.OutcomeOpened},
		{"Snapshot", authOverNotFound, report.OutcomeFailed, "auth", 1, report.OutcomeOpened},
		{"PRs", limited, report.OutcomeDeferred, "rate-limit", throttle.Strikes, report.OutcomeDeferred},
		{"PRs", transient, report.OutcomeFailed, "transient", readAttempts, report.OutcomeOpened},
		{"PRs", &platform.Error{Class: platform.ClassInvalid, Err: errors.New("bad request")}, report.OutcomeFailed, "internal", 1, report.OutcomeOpened},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.reason+" "+tc.err.Error(), func(t *testing.T) {
			w := newWorld(t)
			w.optedIn("acme/x", nil)
			w.optedIn("acme/y", nil)
			transientTimes(w.p, tc.method, tc.err, tc.times)
			d := w.deps()
			d.Concurrency = 1
			rep := w.plan(d)
			tg := want(t, rep, "gh:acme/x", tc.outcome, tc.reason, 0)
			if !hasWarning(tg.Warnings, tc.err.Error()) {
				t.Errorf("warnings %q lack %q", tg.Warnings, tc.err)
			}
			restReason := ""
			if tc.rest == report.OutcomeDeferred {
				restReason = "rate-limit"
			}
			want(t, rep, "gh:acme/y", tc.rest, restReason, 0)
			wantCode := 0
			if tc.outcome == report.OutcomeFailed {
				wantCode = 1
			}
			if code := rep.ExitCode(); code != wantCode {
				t.Errorf("exit code %d, want %d", code, wantCode)
			}
		})
	}
}

// A provider whose reader cannot connect is unavailable: no target, exit 1.
func TestPlanProviderUnavailable(t *testing.T) {
	for _, method := range []string{"Probe", "Self"} {
		t.Run(method, func(t *testing.T) {
			w := newWorld(t)
			w.optedIn("acme/x", nil)
			w.p.FailNext(method, errAuth)
			rep := w.plan(w.deps())
			p := rep.Providers[0]
			if !strings.Contains(p.Error, "bad credentials") || p.ResolveComplete || len(rep.Targets) != 0 {
				t.Errorf("provider %+v, %d targets", p, len(rep.Targets))
			}
			if code := rep.ExitCode(); code != 1 {
				t.Errorf("exit code %d, want 1", code)
			}
		})
	}
	// A bad credential: the fake refuses an unknown account.
	w := newWorld(t)
	d := w.deps()
	d.Providers[0].Reader = w.p.Reader(platform.Account{ID: "404", Login: "nobody"})
	rep := w.plan(d)
	if rep.Providers[0].Error == "" {
		t.Error("an unknown account is not an error")
	}
}

// Errors of phase B are classified as for targets: rate limits that put the
// provider out of budget defer it, an auth error makes it unavailable, other
// errors leave its resolve incomplete.
func TestPlanResolveErrors(t *testing.T) {
	limited := &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests, Err: errors.New("slow down")}
	transient := &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	denied := &platform.Error{Op: "test", Class: platform.ClassPermission, Status: http.StatusForbidden, Err: errors.New("forbidden")}
	authOverNotFound := &platform.Error{Op: "test", Class: platform.ClassAuth, Status: http.StatusUnauthorized, Err: platform.ErrNotFound}
	const repos = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - repo: acme/x\n  - repo: acme/py\n"
	cases := []struct {
		name, method string
		err          error
		targetsYML   string
		// providerErr is part of ProviderInfo.Error, "" for none; warning
		// part of a run warning.
		providerErr, warning string
		targets              int
		code, strictCode     int
	}{
		{"probe rate-limited", "Probe", limited, "", "", "deferred:rate-limit: probe", 0, 0, 3},
		{"probe transient", "Probe", transient, "", "bad gateway", "", 0, 1, 1},
		{"self rate-limited", "Self", limited, "", "", "deferred:rate-limit", 0, 0, 3},
		{"resolve auth", "Resolve", errAuth, "", "bad credentials", "", 0, 1, 1},
		{"resolve rate-limited", "Resolve", limited, "", "", "deferred:rate-limit: resolve targets.yml targets[0]", 0, 0, 3},
		{"resolve transient", "Resolve", transient, "", "", "resolve targets.yml targets[0] (acme)", 1, 0, 3},
		{"resolve denied", "Resolve", denied, "", "", "forbidden", 1, 0, 3},
		{"repo auth", "Repo", errAuth, repos, "bad credentials", "", 0, 1, 1},
		{"repo auth over not found", "Repo", authOverNotFound, repos, "not found", "", 0, 1, 1},
		{"repo rate-limited", "Repo", limited, repos, "", "deferred:rate-limit: look up acme/x", 0, 0, 3},
		{"repo transient", "Repo", transient, repos, "", "look up acme/x (targets.yml targets[0])", 1, 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			if tc.targetsYML != "" {
				w.targetsYML = tc.targetsYML
			}
			w.optedIn("acme/x", nil)
			w.optedIn("acme/py", topics("python"))
			times := 1
			switch platform.ClassOf(tc.err) {
			case platform.ClassTransient:
				times = readAttempts // a transient failure is tried again
			case platform.ClassRateLimited:
				times = throttle.Strikes // a rate limit pauses and tries again
			}
			transientTimes(w.p, tc.method, tc.err, times)
			rep := w.plan(w.deps())
			p := rep.Providers[0]
			switch {
			case tc.providerErr == "" && p.Error != "":
				t.Errorf("provider error %q, want none", p.Error)
			case tc.providerErr != "" && !strings.Contains(p.Error, tc.providerErr):
				t.Errorf("provider error %q, want %q", p.Error, tc.providerErr)
			}
			if tc.warning != "" && !hasWarning(rep.Warnings, tc.warning) {
				t.Errorf("warnings %q lack %q", rep.Warnings, tc.warning)
			}
			if p.ResolveComplete {
				t.Error("the resolve is complete")
			}
			if len(rep.Targets) != tc.targets {
				t.Errorf("%d targets, want %d", len(rep.Targets), tc.targets)
			}
			if code := rep.ExitCode(); code != tc.code {
				t.Errorf("exit code %d, want %d", code, tc.code)
			}
			rep.Strict = true
			if code := rep.ExitCode(); code != tc.strictCode {
				t.Errorf("strict exit code %d, want %d", code, tc.strictCode)
			}
		})
	}
}

// An anonymous provider (one a hub pull request adds or changes, planned
// without a credential) is never asked who it is, and failures to reach it
// or to look its authors up are warnings, not an unavailable provider or
// failed targets.
func TestPlanAnonymousProvider(t *testing.T) {
	setup := func(t *testing.T) (*world, Deps) {
		w := newWorld(t)
		api := w.optedIn("acme/api", nil)
		w.ownPR(api, staleKey)
		w.optedIn("acme/free", nil)
		d := w.deps()
		d.Providers[0].Reader = w.p.Reader(w.p.AddAccount("anonymous", platform.KindUnknown))
		d.Providers[0].Anonymous = true
		return w, d
	}
	t.Run("reachable", func(t *testing.T) {
		w, d := setup(t)
		w.p.FailNext("Lookup", errAuth) // the writer: refused without a credential
		rep := w.plan(d)
		p := rep.Providers[0]
		if p.Error != "" || p.Reader != "" || !p.ResolveComplete {
			t.Errorf("provider %+v", p)
		}
		want(t, rep, "gh:acme/free", report.OutcomeOpened, "", 0)
		// The writer is unknown: its pull request counts as someone else's.
		want(t, rep, "gh:acme/api", report.OutcomeBlocked, "branch-in-use", 1)
		for _, c := range w.p.Calls() {
			if c == "Self" {
				t.Error("an anonymous provider was asked who it is")
			}
		}
		if code := rep.ExitCode(); code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		w, d := setup(t)
		w.p.FailNext("Probe", errAuth)
		rep := w.plan(d)
		p := rep.Providers[0]
		if p.Error != "" || p.ResolveComplete || len(rep.Targets) != 0 {
			t.Errorf("provider %+v, %d targets", p, len(rep.Targets))
		}
		if !hasWarning(rep.Warnings, "provider gh: not checked without credentials: probe") {
			t.Errorf("warnings %q", rep.Warnings)
		}
		if code := rep.ExitCode(); code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
		rep.Strict = true
		if code := rep.ExitCode(); code != 3 {
			t.Errorf("strict exit code %d, want 3", code)
		}
	})
}

// A run cancelled before or during phase B defers what it did not resolve:
// no provider is reported unavailable, and no call follows the cancel.
func TestPlanCancelledResolve(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		w := newWorld(t)
		w.optedIn("acme/x", nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		rep, err := Plan(ctx, w.deps())
		if err != nil {
			t.Fatal(err)
		}
		checkReport(t, rep)
		if p := rep.Providers[0]; p.Error != "" || p.ResolveComplete || len(rep.Targets) != 0 {
			t.Errorf("provider %+v, %d targets", p, len(rep.Targets))
		}
		if !hasWarning(rep.Warnings, "deferred:interrupted") {
			t.Errorf("warnings %q", rep.Warnings)
		}
		if code := rep.ExitCode(); code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
		if calls := w.p.Calls(); len(calls) != 0 {
			t.Errorf("calls after the cancel: %q", calls)
		}
	})
	t.Run("between providers", func(t *testing.T) {
		w := newWorld(t)
		w.hubYML = "version: 1\nid: acme-eng\nproviders:\n  - id: gh\n    type: github\n    writer: acme-write[bot]\n  - id: gh2\n    type: github\n    writer: acme-old[bot]\n"
		w.targetsYML = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - repo: gh:acme/a\n  - repo: gh:acme/b\n  - repo: gh2:acme/c\n"
		for _, name := range []string{"acme/a", "acme/b", "acme/c"} {
			w.optedIn(name, nil)
		}
		hub, _, err := config.ParseHub([]byte(w.hubYML))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		d := w.deps()
		d.Providers = w.providers(hub, map[string]platform.Reader{"github.com": w.p.Reader(w.reader)})
		d.Providers[0].Reader = cancelOnRepo{Reader: d.Providers[0].Reader, cancel: cancel}
		rep, err := Plan(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		checkReport(t, rep)
		want(t, rep, "gh:acme/a", report.OutcomeDeferred, "interrupted", 0)
		if len(rep.Targets) != 1 {
			t.Errorf("%d targets, want 1", len(rep.Targets))
		}
		for _, p := range rep.Providers {
			if p.Error != "" || p.ResolveComplete {
				t.Errorf("provider %+v", p)
			}
		}
		if code := rep.ExitCode(); code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
		calls := w.p.Calls()
		if len(calls) == 0 || calls[len(calls)-1] != "Repo acme/a" {
			t.Errorf("calls %q, want none after the cancelled Repo acme/a", calls)
		}
	})
}

// cancelOnRepo cancels the run once a repository was looked up.
type cancelOnRepo struct {
	platform.Reader
	cancel context.CancelFunc
}

func (c cancelOnRepo) Repo(ctx context.Context, path string) (platform.Repo, error) {
	defer c.cancel()
	return c.Reader.Repo(ctx, path)
}

// Without the writer's id, pull requests by the writer count as someone
// else's.
func TestPlanWriterUnknown(t *testing.T) {
	cases := map[string]func(w *world){
		"no writer": func(w *world) {
			w.hubYML = strings.Replace(defaultHubYML, "    writer: acme-write[bot]\n", "", 1)
			w.hubYML = strings.Replace(w.hubYML, "    known_authors: [\"acme-old[bot]\"]\n", "", 1)
		},
		"unknown login": func(w *world) {
			w.hubYML = strings.Replace(defaultHubYML, "writer: acme-write[bot]", "writer: acme-gone[bot]", 1)
			w.hubYML = strings.Replace(w.hubYML, "    known_authors: [\"acme-old[bot]\"]\n", "", 1)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			edit(w)
			api := w.optedIn("acme/api", nil)
			w.ownPR(api, staleKey)
			rep := w.plan(w.deps())
			want(t, rep, "gh:acme/api", report.OutcomeBlocked, "branch-in-use", 1)
			if !hasWarning(rep.Warnings, "cannot recognize its own pull requests") {
				t.Errorf("warnings %q", rep.Warnings)
			}
		})
	}
	// A known author still counts when the writer does not resolve.
	w := newWorld(t)
	w.hubYML = strings.Replace(defaultHubYML, "writer: acme-write[bot]", "writer: acme-gone[bot]", 1)
	api := w.optedIn("acme/api", nil)
	w.pr(api, platform.PR{Head: branch, Author: w.known, Title: "chore: sync", Body: body(t, staleKey, hubFP)})
	rep := w.plan(w.deps())
	want(t, rep, "gh:acme/api", report.OutcomeUpdated, "content", 1)
	if !hasWarning(rep.Warnings, "only pull requests by known_authors") || !hasWarning(rep.Warnings, "look up the writer acme-gone[bot]") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	// A reader that is the writer is a warning locally and stops a plan in CI.
	w = newWorld(t)
	d := w.deps()
	d.Providers[0].Reader = w.p.Reader(w.writer)
	rep = w.plan(d)
	if !hasWarning(rep.Warnings, "the read credential acts as the writer") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	d.InCI = true
	if _, err := Plan(t.Context(), d); !errors.Is(err, ErrReaderIsWriter) {
		t.Errorf("plan in CI with the writer's credential: err %v, want ErrReaderIsWriter", err)
	}
}

// A Lookup that fails for another reason than an unknown login leaves the
// authors incomplete: a pull request that could be ours fails or defers its
// target by the error's class instead of blocking it on a guess; targets
// without such a pull request are planned as usual.
func TestPlanLookupFails(t *testing.T) {
	transient := &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	limited := &platform.Error{Op: "test", Class: platform.ClassRateLimited, Status: http.StatusTooManyRequests, Err: errors.New("slow down")}
	for _, tc := range []struct {
		err     error
		outcome report.Outcome
		reason  string
		times   int
	}{
		{transient, report.OutcomeFailed, "transient", readAttempts},
		{errAuth, report.OutcomeFailed, "auth", 1},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			w := newWorld(t)
			api := w.optedIn("acme/api", nil)
			w.ownPR(api, staleKey)
			w.optedIn("acme/free", nil)
			other := w.optedIn("acme/other-hub", nil)
			w.pr(other, platform.PR{Head: branch, Author: w.writer, Title: "chore: sync", Body: body(t, staleKey, otherFP)})
			transientTimes(w.p, "Lookup", tc.err, tc.times) // the writer; the known author resolves
			rep := w.plan(w.deps())
			tg := want(t, rep, "gh:acme/api", tc.outcome, tc.reason, 1)
			if !hasWarning(tg.Warnings, "tell whether #1 by acme-write[bot] is touchmark's pull request") {
				t.Errorf("warnings %q", tg.Warnings)
			}
			want(t, rep, "gh:acme/free", report.OutcomeOpened, "", 0)
			// Another hub's marker is someone else's, whoever the author.
			want(t, rep, "gh:acme/other-hub", report.OutcomeBlocked, "branch-in-use", 1)
			if !hasWarning(rep.Warnings, "look up the writer acme-write[bot]") {
				t.Errorf("warnings %q", rep.Warnings)
			}
		})
	}
	// Rate limits of the lookup that put the provider out of budget stop
	// it: its targets are not even listed, and wait for the next run.
	t.Run("rate-limit", func(t *testing.T) {
		w := newWorld(t)
		api := w.optedIn("acme/api", nil)
		w.ownPR(api, staleKey)
		w.optedIn("acme/free", nil)
		transientTimes(w.p, "Lookup", limited, throttle.Strikes)
		rep := w.plan(w.deps())
		if len(rep.Targets) != 0 || rep.Providers[0].ResolveComplete ||
			!hasWarning(rep.Warnings, "provider gh: deferred:rate-limit: resolve targets.yml targets[0]") {
			t.Errorf("targets %+v, provider %+v, warnings %q", rep.Targets, rep.Providers[0], rep.Warnings)
		}
		for _, c := range w.p.Calls() {
			if strings.HasPrefix(c, "Resolve") || strings.HasPrefix(c, "ReadFile") || strings.HasPrefix(c, "PRs") {
				t.Errorf("a provider out of budget was read: %q", c)
			}
		}
		rep.Strict = true
		if code := rep.ExitCode(); code != 3 {
			t.Errorf("strict exit code %d, want 3", code)
		}
	})
}

// Guard I8: a run whose hub commit is not the tip of the default branch is
// superseded and reads nothing.
func TestPlanSuperseded(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/x", nil)
	d := w.deps()
	d.Channel = channel{head: strings.Repeat("a", 40)}
	rep := w.plan(d)
	if rep.Outcome != report.Superseded || len(rep.Targets) != 0 || len(rep.Providers) != 0 {
		t.Errorf("outcome %s, %d targets, %d providers", rep.Outcome, len(rep.Targets), len(rep.Providers))
	}
	if !hasWarning(rep.Warnings, "this run is superseded") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	if calls := w.p.Calls(); len(calls) != 0 {
		t.Errorf("a superseded run called the platform: %q", calls)
	}
	rep.Strict = true
	if code := rep.ExitCode(); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}

	// The tip: the run goes on, without a warning.
	d.Channel = channel{head: strings.ToUpper(hubCommit)}
	rep = w.plan(d)
	if rep.Outcome != report.Completed || len(rep.Targets) != 1 || len(rep.Warnings) != 0 {
		t.Errorf("outcome %s, %d targets, warnings %q", rep.Outcome, len(rep.Targets), rep.Warnings)
	}

	// A channel that fails is a warning.
	d.Channel = channel{err: errors.New("hub channel: 503")}
	rep = w.plan(d)
	if rep.Outcome != report.Completed || !slices.Equal(rep.Warnings, []string{"hub head not checked: hub channel: 503"}) {
		t.Errorf("outcome %s, warnings %q", rep.Outcome, rep.Warnings)
	}

	// No channel: a warning where the guard applies.
	d.Channel = nil
	for _, tc := range []struct {
		ctx  hubch.Context
		want string
	}{
		{hubch.Context{CI: hubch.Local}, "hub head not checked: a local run"},
		{hubch.Context{CI: hubch.GitLabCI, DefaultBranch: "main", RefName: "main", RefIsBranch: true, Visibility: "private"}, "hub head not checked: no channel"},
		{hubch.Context{CI: hubch.GitLabCI, DefaultBranch: "main", RefName: "feature", RefIsBranch: true, Visibility: "private"}, ""},
		{hubch.Context{CI: hubch.GitHubActions, RefName: "41/merge", Visibility: "public"}, ""},
	} {
		d.HubContext = tc.ctx
		rep = w.plan(d)
		if got := strings.Join(rep.Warnings, "\n"); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: warnings %q, want %q", tc.ctx, got, tc.want)
		}
	}
}

func TestHeadGuard(t *testing.T) {
	for _, tc := range []struct {
		ctx  hubch.Context
		want bool
	}{
		{hubch.Context{}, true},
		{hubch.Context{CI: hubch.Local}, true},
		{hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "main", RefIsBranch: true}, true},
		{hubch.Context{CI: hubch.GitHubActions, RefName: "main", RefIsBranch: true}, true},
		{hubch.Context{CI: hubch.GitHubActions, DefaultBranch: "main", RefName: "41/merge"}, false},
		{hubch.Context{CI: hubch.GitLabCI, DefaultBranch: "main", RefName: "feature", RefIsBranch: true}, false},
		{hubch.Context{CI: hubch.ForgejoActions, DefaultBranch: "main", RefName: "v1.0"}, false},
	} {
		if got := HeadGuard(tc.ctx); got != tc.want {
			t.Errorf("HeadGuard(%+v) = %v, want %v", tc.ctx, got, tc.want)
		}
	}
}

// Two providers on one host reach the same repository: the provider of its
// first entry in targets.yml handles it once, with the packs of every entry,
// whatever the order of hub.yml.
func TestPlanDuplicateProvider(t *testing.T) {
	w := newWorld(t)
	w.hubYML = `version: 1
id: acme-eng
providers:
  - id: gh
    type: github
    writer: acme-write[bot]
  - id: gh2
    type: github
    writer: acme-old[bot]
`
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - repo: gh2:acme/shared
    packs: [python]
  - repo: gh:acme/shared
  - repo: gh2:acme/solo
`
	shared := w.optedIn("acme/shared", nil)
	// gh2's writer has an open pull request there: handled by gh2, it is
	// touchmark's own.
	w.pr(shared, platform.PR{Head: branch, Author: w.known, Title: "chore: sync", Body: body(t, keyOfMissing("base", "python"), hubFP)})
	w.optedIn("acme/solo", nil)
	hub, _, err := config.ParseHub([]byte(w.hubYML))
	if err != nil {
		t.Fatal(err)
	}
	d := w.deps()
	d.Providers = w.providers(hub, map[string]platform.Reader{"github.com": w.p.Reader(w.reader)})
	rep := w.plan(d)
	if len(rep.Targets) != 2 {
		t.Fatalf("%d targets, want 2", len(rep.Targets))
	}
	res := want(t, rep, "gh2:acme/shared", report.OutcomeUnchanged, "", 1)
	if !slices.Equal(res.Packs, []string{"base", "python"}) {
		t.Errorf("packs %q, want base and python", res.Packs)
	}
	if !hasWarning(res.Warnings, "duplicate-provider: also listed by provider gh on the same host; gh2 handles it") {
		t.Errorf("warnings %q", res.Warnings)
	}
	want(t, rep, "gh2:acme/solo", report.OutcomeOpened, "", 0)
	if rep.Cost["gh"] != 0 || rep.Cost["gh2"] != 4 {
		t.Errorf("cost %v", rep.Cost)
	}

	// The other order of targets.yml hands it to gh, whose writer's pull
	// requests are the only ones it knows.
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - repo: gh:acme/shared
  - repo: gh2:acme/shared
    packs: [python]
`
	d = w.deps()
	d.Providers = w.providers(hub, map[string]platform.Reader{"github.com": w.p.Reader(w.reader)})
	rep = w.plan(d)
	res = want(t, rep, "gh:acme/shared", report.OutcomeBlocked, "branch-in-use", 1)
	if !hasWarning(res.Warnings, "also listed by provider gh2 on the same host; gh handles it") {
		t.Errorf("warnings %q", res.Warnings)
	}
}

// --only plans the targets it names; the sweep is then off.
func TestPlanOnly(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/api", nil)
	w.optedIn("acme/sdk", nil)
	w.optedIn("acme/web", nil)
	d := w.deps()
	d.Only = []config.Ref{{Provider: "gh", Path: "acme/API"}, {Path: "acme/web"}, {Path: "acme/nothing"}}
	rep := w.plan(d)
	var got []string
	for _, tg := range rep.Targets {
		got = append(got, tg.Path)
	}
	if !slices.Equal(got, []string{"acme/api", "acme/web"}) {
		t.Errorf("targets %q", got)
	}
	if rep.Sweep.Reason == "" || rep.Sweep.Ran {
		t.Errorf("sweep %+v", rep.Sweep)
	}
	if !hasWarning(rep.Warnings, "--only acme/nothing matches no target") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	for _, c := range w.p.Calls() {
		if strings.HasPrefix(c, "ReadFile acme/sdk") {
			t.Errorf("a target outside --only was read: %q", c)
		}
	}
	rep.Strict = true
	if code := rep.ExitCode(); code != 3 {
		t.Errorf("strict exit code %d, want 3 (sweep off)", code)
	}
}

// Resolve: a missing repository, an incomplete listing, an excluded one
// never looked up, a renamed one.
func TestPlanResolve(t *testing.T) {
	w := newWorld(t)
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - repo: acme/gone
  - repo: acme/legacy
  - repo: acme/old-name
  - repo: ACME/Api
  - org: acme
    topics: [python]
exclude:
  - acme/legacy
`
	w.optedIn("acme/api", nil)
	w.optedIn("acme/new-name", nil)
	w.optedIn("acme/legacy", nil)
	w.optedIn("acme/py", topics("python"))
	w.p.SetIncompleteListings(true)
	d := w.deps()
	d.Providers[0].Reader = renaming{Reader: d.Providers[0].Reader, from: "acme/old-name", to: "acme/new-name"}
	rep := w.plan(d)
	p := rep.Providers[0]
	if p.ResolveComplete || !slices.Equal(p.Missing, []string{"gh:acme/gone"}) {
		t.Errorf("provider %+v", p)
	}
	if !hasWarning(rep.Warnings, "gh:acme/gone: target-missing") || !hasWarning(rep.Warnings, "is incomplete") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	renamed := want(t, rep, "gh:acme/new-name", report.OutcomeOpened, "", 0)
	if !slices.Equal(renamed.Warnings, []string{"renamed: targets.yml names it acme/old-name"}) {
		t.Errorf("warnings %q", renamed.Warnings)
	}
	// A spelling that differs only by case is not a rename.
	if api := want(t, rep, "gh:acme/api", report.OutcomeOpened, "", 0); len(api.Warnings) != 0 {
		t.Errorf("acme/api warnings %q", api.Warnings)
	}
	want(t, rep, "gh:acme/py", report.OutcomeOpened, "", 0)
	if len(rep.Targets) != 3 {
		t.Errorf("%d targets, want 3", len(rep.Targets))
	}
	for _, c := range w.p.Calls() {
		if strings.Contains(c, "acme/legacy") {
			t.Errorf("an excluded repository was looked up: %q", c)
		}
	}
	rep.Strict = true
	if code := rep.ExitCode(); code != 3 {
		t.Errorf("strict exit code %d, want 3", code)
	}
}

// renaming is a reader whose platform redirects a renamed repository.
type renaming struct {
	platform.Reader
	from, to string
}

func (r renaming) Repo(ctx context.Context, path string) (platform.Repo, error) {
	if strings.EqualFold(path, r.from) {
		path = r.to
	}
	return r.Reader.Repo(ctx, path)
}

// The opt-in file changed between the API read and the snapshot: it is
// read again at the snapshot's commit.
func TestPlanOptInRace(t *testing.T) {
	w := newWorld(t)
	x := w.repo("acme/x", nil, optInName, "version: 1\npacks: [python]\n")
	d := w.deps()
	stale := &staleOptIn{Reader: d.Providers[0].Reader}
	d.Providers[0].Reader = stale
	rep := w.plan(d)
	tg := want(t, rep, "gh:acme/x", report.OutcomeOpened, "", 0)
	if !slices.Equal(tg.Packs, []string{"base", "python"}) {
		t.Errorf("packs %q: the stale opt-in file was used", tg.Packs)
	}
	head := w.p.Head(x.ID)
	if !slices.Equal(stale.refs, []string{"", head}) {
		t.Errorf("opt-in reads at %q, want the default branch, then %s", stale.refs, head)
	}

	// A reader that keeps answering another blob fails the target.
	w2 := newWorld(t)
	w2.repo("acme/x", nil, optInName, "version: 1\n")
	d = w2.deps()
	d.Providers[0].Reader = &staleOptIn{Reader: d.Providers[0].Reader, always: true}
	rep = w2.plan(d)
	want(t, rep, "gh:acme/x", report.OutcomeFailed, "race", 0)
}

// staleOptIn answers the first read of the opt-in file (every read with
// always) with an older version.
type staleOptIn struct {
	platform.Reader
	always bool
	refs   []string
}

func (s *staleOptIn) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	s.refs = append(s.refs, ref)
	if s.always || len(s.refs) == 1 {
		content := []byte("version: 1\n")
		return platform.File{Path: path, Mode: "100644", OID: oid("version: 1\n# older\n"), Content: content}, nil
	}
	return s.Reader.ReadFile(ctx, r, ref, path, max)
}

// A GitLab provider: a new merge request costs 2 writes.
func TestPlanGitLab(t *testing.T) {
	p := fake.New("gitlab.example.com", fake.WithFlavor(fake.GitLab))
	reader := p.AddAccount("tm-reader", platform.KindServiceAccount)
	p.AddAccount("tm-writer", platform.KindServiceAccount)
	r := p.AddRepo(platform.Repo{Path: "platform/api"})
	p.SetFile(r.ID, optInName, []byte("version: 1\n"), "")
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.hubYML = `version: 1
id: acme-eng
providers:
  - id: corp
    type: gitlab
    url: https://gitlab.example.com
    writer: tm-writer
`
	w.targetsYML = "version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - group: platform\n"
	hub, _, err := config.ParseHub([]byte(w.hubYML))
	if err != nil {
		t.Fatal(err)
	}
	d := w.deps()
	d.Providers = w.providers(hub, map[string]platform.Reader{"gitlab.example.com": p.Reader(reader)})
	d.Snapshots = sources{"gitlab.example.com": p.Snapshots()}
	rep := w.plan(d)
	want(t, rep, "corp:platform/api", report.OutcomeOpened, "", 0)
	if rep.Cost["corp"] != 2 || rep.Providers[0].Host != "gitlab.example.com" {
		t.Errorf("cost %v, providers %+v", rep.Cost, rep.Providers)
	}
}

// The output does not depend on how many targets are inspected at once.
func TestPlanDeterministic(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		r := w.optedIn("acme/"+name, topics("python"))
		if name < "e" {
			w.ownPR(r, staleKey)
		}
	}
	w.repo("acme/k", nil)
	var outs []string
	for _, n := range []int{1, 3, 16} {
		d := w.deps()
		d.Concurrency = n
		data, err := json.Marshal(w.plan(d))
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(data))
	}
	if outs[0] != outs[1] || outs[0] != outs[2] {
		t.Errorf("reports differ:\n%s\n%s\n%s", outs[0], outs[1], outs[2])
	}
}

// A cancelled run defers the targets it could not finish.
func TestPlanInterrupted(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil)
	ctx, cancel := context.WithCancel(t.Context())
	d := w.deps()
	d.Concurrency = 1
	d.Providers[0].Reader = cancelling{Reader: d.Providers[0].Reader, cancel: cancel}
	rep, err := Plan(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	checkReport(t, rep)
	want(t, rep, "gh:acme/a", report.OutcomeDeferred, "interrupted", 0)
	want(t, rep, "gh:acme/b", report.OutcomeDeferred, "interrupted", 0)
}

// cancelling cancels the run on the first pull request listing.
type cancelling struct {
	platform.Reader
	cancel context.CancelFunc
}

func (c cancelling) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	c.cancel()
	return c.Reader.PRs(ctx, r, heads, authors)
}

// A run whose deadline passes defers the targets it could not finish with
// deferred:deadline.
func TestPlanDeadline(t *testing.T) {
	w := newWorld(t)
	w.optedIn("acme/a", nil)
	w.optedIn("acme/b", nil)
	ctx := newExpiring(t.Context())
	d := w.deps()
	d.Concurrency = 1
	d.Providers[0].Reader = expireOnPRs{Reader: d.Providers[0].Reader, ctx: ctx}
	rep, err := Plan(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	checkReport(t, rep)
	want(t, rep, "gh:acme/a", report.OutcomeDeferred, "deadline", 0)
	want(t, rep, "gh:acme/b", report.OutcomeDeferred, "deadline", 0)
	if code := rep.ExitCode(); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
}

// expiring is a context whose deadline passes when expire is called.
type expiring struct {
	context.Context
	mu      sync.Mutex
	done    chan struct{}
	expired bool
}

func newExpiring(parent context.Context) *expiring {
	return &expiring{Context: parent, done: make(chan struct{})}
}

func (c *expiring) Done() <-chan struct{} { return c.done }

func (c *expiring) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expired {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *expiring) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.expired {
		c.expired = true
		close(c.done)
	}
}

// expireOnPRs lets the deadline pass on the first pull request listing.
type expireOnPRs struct {
	platform.Reader
	ctx *expiring
}

func (e expireOnPRs) PRs(ctx context.Context, r platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	e.ctx.expire()
	return e.Reader.PRs(ctx, r, heads, authors)
}

func TestPlanErrors(t *testing.T) {
	w := newWorld(t)
	cases := map[string]func(d *Deps){
		"no hub":            func(d *Deps) { d.Hub = nil },
		"no fingerprint":    func(d *Deps) { d.Fingerprint = "" },
		"bad fingerprint":   func(d *Deps) { d.Fingerprint = "github.com" },
		"no branch":         func(d *Deps) { d.Hub.Branch = "" },
		"no providers":      func(d *Deps) { d.Providers = nil },
		"no reader":         func(d *Deps) { d.Providers[0].Reader = nil },
		"no snapshots":      func(d *Deps) { d.Snapshots = nil },
		"bad targets entry": func(d *Deps) { d.Targets = &config.Targets{Targets: []config.Entry{{}}} },
	}
	for name, edit := range cases {
		d := w.deps()
		edit(&d)
		if rep, err := Plan(t.Context(), d); err == nil || rep != nil || !strings.HasPrefix(err.Error(), "plan: ") {
			t.Errorf("%s: %v, %v", name, rep, err)
		}
	}
}
