package distribute

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// openOwn adds our open pull request on the sync branch of r (no branch
// behind it: a sweep close never reads one).
func (g *gitWorld) openOwn(r platform.Repo) int64 {
	g.t.Helper()
	return g.ownOn(r, branch, keyOf(baseFiles...), baseFiles...)
}

// TestRunSweep: the own open pull requests of repositories that are no
// longer targets are closed: dropped from targets.yml or excluded
// (target-dropped, a report line of their own), or without the opt-in file
// any more (opted-out, the target's line); one in an archived repository
// cannot be (blocked:archived). A fork's pull request with our marker is not
// ours, and a public hub does not name a private repository.
func TestRunSweep(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.ctx = hubch.Context{CI: hubch.GitHubActions, Host: "github.com", RepoID: "712345678", Visibility: "public",
		DefaultBranch: "main", RefName: "main", RefIsBranch: true, Event: "schedule"}
	outside := w.repo("other/outside", nil, "README.md", "x\n")
	nOutside := w.openOwn(outside)
	legacy := w.optedIn("acme/legacy", nil)
	nLegacy := w.openOwn(legacy)
	gone := w.repo("acme/gone", nil, "README.md", "the opt-in file is gone\n")
	nGone := w.openOwn(gone)
	archived := w.repo("other/archived", func(r *platform.Repo) { r.Archived = true }, "README.md", "x\n")
	nArchived := w.openOwn(archived)
	secret := w.repo("other/secret", func(r *platform.Repo) { r.Visibility = "private" }, "README.md", "x\n")
	w.openOwn(secret)
	// A fork of a dropped repository with a copy of our marker.
	fork := w.repo("jdoe/outside", func(r *platform.Repo) { r.Fork = true }, "README.md", "x\n")
	forked := w.repo("other/forked", nil, "README.md", "x\n")
	w.pr(forked, platform.PR{Head: branch, HeadRepoID: fork.ID, Author: w.writer, Title: "chore: sync",
		Body: markerBody(t, keyOf(baseFiles...), hubFP, nil, nil)})
	// A target that stays: its pull request is not swept.
	api := w.optedIn("acme/api", nil)
	w.syncCommit(api, branch, "", baseFiles...)
	w.written(api, w.openOwn(api))

	// The runs of a CI job read the tip of the hub's default branch.
	w.edit = func(d *Deps) { d.Channel = channel{head: hubCommit} }
	rep := w.both(nil)
	want(t, rep, "gh:other/outside", report.OutcomeClosed, "target-dropped", nOutside)
	want(t, rep, "gh:acme/legacy", report.OutcomeClosed, "target-dropped", nLegacy)
	want(t, rep, "gh:acme/gone", report.OutcomeClosed, "opted-out", nGone)
	want(t, rep, "gh:other/archived", report.OutcomeBlocked, "archived", nArchived)
	want(t, rep, "gh:acme/api", report.OutcomeUnchanged, "", 1)
	hidden := 0
	for _, tg := range rep.Targets {
		switch {
		case tg.Path == "other/forked" || tg.Path == "jdoe/outside":
			t.Errorf("a fork's pull request was swept: %+v", tg)
		case tg.Path == "" && tg.Outcome == report.OutcomeClosed && tg.Reason == "target-dropped":
			hidden++
			if tg.PR != nil || tg.RepoID != "" {
				t.Errorf("a private repository is named: %+v", tg)
			}
		case strings.Contains(fmt.Sprint(tg), "secret"):
			t.Errorf("a private repository is named: %+v", tg)
		}
	}
	if hidden != 1 {
		t.Errorf("%d hidden closes, want 1", hidden)
	}
	for _, ref := range []string{"gh:other/outside", "gh:acme/legacy", "gh:acme/gone"} {
		if n := targetOf(t, rep, ref).Writes; n != 2 {
			t.Errorf("%s: %d writes, want 2 (close, comment)", ref, n)
		}
	}
	if rep.Sweep != (report.SweepInfo{Ran: true, Complete: true}) {
		t.Errorf("sweep %+v", rep.Sweep)
	}

	// --only turns the sweep off.
	rep = w.both(func(d *Deps) { d.Only = []config.Ref{{Path: "acme/api"}} })
	if rep.Sweep.Ran || rep.Sweep.Reason != sweepOffOnly || len(rep.Targets) != 1 {
		t.Errorf("--only: sweep %+v, %d targets", rep.Sweep, len(rep.Targets))
	}

	// An incomplete listing turns it off for the provider (strict: exit 3).
	w.p.SetIncompleteListings(true)
	rep = w.both(nil)
	if rep.Sweep.Ran || !strings.Contains(rep.Sweep.Reason, "off for provider gh") {
		t.Errorf("incomplete: sweep %+v", rep.Sweep)
	}
	for _, tg := range rep.Targets {
		if tg.Outcome == report.OutcomeClosed {
			t.Errorf("closed with an incomplete listing: %+v", tg)
		}
	}
	rep.Strict = true
	if code := rep.ExitCode(); code != 3 {
		t.Errorf("strict exit code %d, want 3", code)
	}
	w.p.SetIncompleteListings(false)

	// A listing that fails fails the sweep: exit 1.
	fail := &platform.Error{Op: "test", Class: platform.ClassTransient, Status: http.StatusBadGateway, Err: errors.New("bad gateway")}
	transientTimes(w.p, "OpenPRsBy", fail, readAttempts)
	rep = w.run(w.deps(ModePlan), ModePlan)
	if !rep.Sweep.Failed || rep.Sweep.Ran {
		t.Errorf("failed listing: sweep %+v", rep.Sweep)
	}
	if code := rep.ExitCode(); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
}

// TestRunMassClose: when the run would close more of touchmark's pull
// requests than max(5, max_close_fraction × own open ones), nothing is
// closed, whatever the reason of each close; operations.yml allow_mass_close
// lifts the guard until its date.
func TestRunMassClose(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	for i := range 5 {
		w.openOwn(w.repo(fmt.Sprintf("other/dropped-%d", i), nil, "README.md", "x\n"))
	}
	// A target whose default branch took the hub's files: a no-diff close.
	old := w.optedIn("acme/old", nil)
	w.syncCommit(old, branch, "", baseFiles...)
	w.openOwn(old)
	w.push(old, "main", w.person, baseFiles...)

	rep := w.both(nil)
	blocked := 0
	for _, tg := range rep.Targets {
		switch {
		case tg.Outcome == report.OutcomeBlocked && tg.Reason == "mass-close":
			blocked++
			if tg.Writes != 0 {
				t.Errorf("%s: %d writes", tg.Path, tg.Writes)
			}
		case tg.Outcome == report.OutcomeClosed:
			t.Errorf("%s closed past the guard", tg.Path)
		}
	}
	if blocked != 6 {
		t.Errorf("%d blocked:mass-close, want 6", blocked)
	}
	if !hasWarning(rep.Warnings, "blocked:mass-close: this run would close 6 of touchmark's 6 open pull requests, more than 5") {
		t.Errorf("warnings %q", rep.Warnings)
	}
	if code := rep.ExitCode(); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}

	// allow_mass_close lifts it; an expired one does not.
	allow := func(until string) func(*Deps) {
		return func(d *Deps) {
			d.Write.Operations = &config.Operations{Version: 1, AllowMassClose: &config.MassCloseOp{Max: 6, Until: until}}
		}
	}
	rep = w.both(allow("2099-12-31"))
	want(t, rep, "gh:acme/old", report.OutcomeClosed, "no-diff", 1)
	want(t, rep, "gh:other/dropped-0", report.OutcomeClosed, "target-dropped", 1)
	if rep.Summary[report.OutcomeClosed] != 6 {
		t.Errorf("summary %v", rep.Summary)
	}
	rep = w.both(allow("2020-01-01"))
	if rep.Summary[report.OutcomeBlocked] != 6 {
		t.Errorf("an expired allow_mass_close: summary %v", rep.Summary)
	}
}

// A provider's sweep is off, with its reason, whenever its listing could not
// be trusted to name every pull request of a dropped target; none of these
// marks the sweep failed.
func TestSweepOffReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(w *world, p *provider)
		why  string
	}{
		{"unavailable", func(_ *world, p *provider) { p.info.Error = "probe: bad gateway" }, "the provider is unavailable"},
		{"anonymous", func(_ *world, p *provider) { p.anonymous = true }, "it is planned without credentials"},
		{"incomplete resolve", func(_ *world, p *provider) { p.info.ResolveComplete = false }, "its resolve is incomplete"},
		{"no own ids", func(_ *world, p *provider) { p.ids = nil }, "touchmark cannot recognize its own pull requests"},
		{"circuit open", func(_ *world, p *provider) { exhaust(p) }, "deferred:rate-limit: the provider's circuit is open"},
		{"rate-limited listing", func(w *world, _ *provider) { transientTimes(w.p, "OpenPRsBy", errLimited, 3) },
			"deferred:rate-limit: provider gh limited the rate 3 times in a row"},
		{"incomplete listing", func(w *world, _ *provider) { w.p.SetIncompleteListings(true) }, "the listing of its open pull requests is incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.optedIn("acme/x", nil)
			r, err := newRun(runDeps(w, ModePlan), ModePlan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.resolve(t.Context()); err != nil {
				t.Fatal(err)
			}
			p := r.provs[0]
			if len(p.ids) == 0 || p.info.Error != "" || !p.info.ResolveComplete {
				t.Fatalf("the provider did not resolve: %+v", p.info)
			}
			tc.edit(w, p)
			closes, why := r.sweepProvider(t.Context(), p, map[string]bool{}, nil)
			if len(closes) > 0 || !strings.HasPrefix(why, tc.why) {
				t.Errorf("sweep = %d closes, %q; want off: %q", len(closes), why, tc.why)
			}
			if r.rep.Sweep.Failed {
				t.Error("an off sweep is marked failed")
			}
		})
	}
}
