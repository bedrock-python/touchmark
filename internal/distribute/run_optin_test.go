package distribute

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// assumedTargetsYML subscribes the services of acme (opt_in: assumed,
// narrowed by match) and other/tool, and lists every repository of acme
// with the python topic as an entry that wants an opt-in file; the legacy
// services are excluded by a pattern.
const assumedTargetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - org: acme
    match: [acme/svc-*]
    opt_in: assumed
  - org: acme
    topics: [python]
    packs: [python]
  - repo: other/tool
    opt_in: assumed
exclude:
  - acme/svc-legacy*
`

// TestPlanAssumedOptIn: a repository an entry with opt_in: assumed
// selects counts as opted in without an opt-in file, with the packs of
// defaults and of its entries, and the report says targets.yml assumed
// it; one selected only by an entry that wants the file is not opted in;
// enabled: false opts any repository out; an opt-in file of its own wins
// over the assumption. plan --assume-opt-in marks what it assumes apart.
func TestPlanAssumedOptIn(t *testing.T) {
	w := newWorld(t)
	w.targetsYML = assumedTargetsYML
	w.repo("acme/svc-a", nil, "README.md", "service a\n")
	w.repo("acme/svc-b", topics("python"), "README.md", "service b\n")
	w.repo("acme/web", topics("python"), "README.md", "the web site\n")
	w.repo("acme/svc-off", nil, optInName, "version: 1\nenabled: false\n")
	w.repo("acme/svc-legacy-1", nil, "README.md", "excluded\n")
	w.repo("acme/lib", topics("python"), optInName, "version: 1\n")
	w.repo("other/tool", nil, "README.md", "a tool\n")
	w.repo("acme/svc-own", nil, optInName, "version: 1\npacks: [python]\n")
	w.repo("acme/batch", nil, "README.md", "selected by no entry\n")

	rep := w.plan(w.deps())
	check := func(rep *report.Delivery, ref string, outcome report.Outcome, reason, by string, packs ...string) {
		t.Helper()
		tg := want(t, rep, ref, outcome, reason, 0)
		if tg.Assumed != (by != "") || tg.AssumedBy != by {
			t.Errorf("%s: assumed %v by %q, want %q", ref, tg.Assumed, tg.AssumedBy, by)
		}
		if packs != nil && !slices.Equal(tg.Packs, packs) {
			t.Errorf("%s: packs %v, want %v", ref, tg.Packs, packs)
		}
	}
	check(rep, "gh:acme/svc-a", report.OutcomeOpened, "", report.AssumedByHub, "base")
	check(rep, "gh:acme/svc-b", report.OutcomeOpened, "", report.AssumedByHub, "base", "python")
	check(rep, "gh:acme/web", report.OutcomeSkipped, "not-opted-in", "")
	check(rep, "gh:acme/svc-off", report.OutcomeSkipped, "opted-out", "")
	check(rep, "gh:acme/lib", report.OutcomeOpened, "", "", "base", "python")
	check(rep, "gh:other/tool", report.OutcomeOpened, "", report.AssumedByHub, "base")
	check(rep, "gh:acme/svc-own", report.OutcomeOpened, "", "", "base", "python")
	for _, tg := range rep.Targets {
		if tg.Path == "acme/svc-legacy-1" || tg.Path == "acme/batch" {
			t.Errorf("%s is a target: %+v", tg.Path, tg)
		}
	}
	if len(rep.Targets) != 7 {
		t.Errorf("%d targets, want 7", len(rep.Targets))
	}
	var text, md bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	line := "3 targets without an opt-in file are opted in by targets.yml"
	if !strings.Contains(text.String(), "opt_in: assumed: "+line) ||
		!strings.Contains(md.String(), `**opt\_in\: assumed:** 3 targets without an opt\-in file are opted in by targets\.yml`) {
		t.Errorf("no line %q in\n%s\n%s", line, text.String(), md.String())
	}

	// --assume-opt-in takes the rest for opted in, apart; enabled: false
	// still opts out, and the hub's own assumption stays the hub's.
	rep = w.plan(func() Deps { d := w.deps(); d.AssumeOptIn = true; return d }())
	check(rep, "gh:acme/web", report.OutcomeOpened, "", report.AssumedByFlag, "base", "python")
	check(rep, "gh:acme/svc-a", report.OutcomeOpened, "", report.AssumedByHub, "base")
	check(rep, "gh:acme/svc-off", report.OutcomeSkipped, "opted-out", "")
	text.Reset()
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "--assume-opt-in: for this report only, a target without an opt-in file counts as opted in (one whose file says enabled: false stays opted out); 1 target is planned") {
		t.Errorf("--assume-opt-in line:\n%s", text.String())
	}
}

// TestPlanTargetsByURL: entries and exclude entries written as web URLs
// select the same targets as their <provider>:<path> forms, and the report
// names the targets <provider>:<path>.
func TestPlanTargetsByURL(t *testing.T) {
	w := newWorld(t)
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - org: https://github.com/acme/
    match: [acme/svc-*]
  - repo: https://github.com/other/tool.git
exclude:
  - https://github.com/acme/svc-legacy-*
`
	w.optedIn("acme/svc-a", nil)
	w.optedIn("acme/svc-legacy-1", nil)
	w.optedIn("acme/web", nil)
	w.optedIn("other/tool", nil)
	rep := w.plan(w.deps())
	var refs []string
	for _, tg := range rep.Targets {
		refs = append(refs, tg.Provider+":"+tg.Path)
	}
	if !slices.Equal(refs, []string{"gh:acme/svc-a", "gh:other/tool"}) {
		t.Errorf("targets %q", refs)
	}
	// A URL no provider serves stops the run before any target is read.
	w.targetsYML = "version: 1\ntargets:\n  - repo: https://gitlab.com/acme/x\n"
	if _, err := Plan(t.Context(), w.deps()); err == nil || !strings.Contains(err.Error(), "is not under the url of any provider") {
		t.Errorf("Plan with a URL of no provider: %v", err)
	}
}

// TestRunAssumedOptIn: the life of a repository the hub subscribes
// (opt_in: assumed) through distribute. Without an opt-in file it gets a
// pull request whose description says how to choose packs and how to opt
// out; closing it is a decline, remembered, which an empty opt-in file
// keeps and a change of ignore lifts. An open pull request of a subscribed
// repository is never swept for its missing file, while one of a
// repository selected only by an entry that wants the file is. enabled:
// false closes the open pull request as opted-out; deleting the file again
// returns the repository to the hub's subscription.
func TestRunAssumedOptIn(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.targetsYML = assumedTargetsYML
	billing := w.repo("acme/svc-billing", nil, "README.md", "the billing service\n")
	api := w.repo("acme/svc-api", nil, "README.md", "the api\n")
	kept := w.repo("acme/svc-kept", nil, "README.md", "a service with a sync pull request\n")
	w.syncCommit(kept, branch, "", baseFiles...)
	nKept := w.openOwn(kept)
	w.written(kept, nKept)
	web := w.repo("acme/web", topics("python"), "README.md", "the web site\n")
	nWeb := w.openOwn(web)

	rep := w.distribute(nil)
	tg := targetOf(t, rep, "gh:acme/svc-billing")
	if tg.Outcome != report.OutcomeOpened || tg.PR == nil || tg.AssumedBy != report.AssumedByHub {
		t.Fatalf("svc-billing: %+v", tg)
	}
	nBilling := tg.PR.Number
	pr := w.p.PR(billing.ID, nBilling)
	if !strings.Contains(pr.Body, "The hub subscribed this repository, which has no `.engineering-assets.yml`.") ||
		!strings.Contains(pr.Body, "add it with `enabled: false`") {
		t.Errorf("the description does not explain the subscription:\n%s", pr.Body)
	}
	nAPI := want(t, rep, "gh:acme/svc-api", report.OutcomeOpened, "", 1).PR.Number
	want(t, rep, "gh:acme/svc-kept", report.OutcomeUnchanged, "", nKept)
	want(t, rep, "gh:acme/web", report.OutcomeClosed, "opted-out", nWeb)
	if c := w.p.Comments(web.ID, nWeb); len(c) == 0 || !strings.Contains(c[len(c)-1].Body, "has no opt-in file, and the hub does not subscribe it") {
		t.Errorf("close comment of a repository without the file: %+v", c)
	}
	if got := w.p.PR(kept.ID, nKept).State; got != platform.Open {
		t.Errorf("the subscribed repository's pull request is %s", got)
	}

	// A person closes the pull request: a decline, acked once, then quiet.
	w.p.SetPRState(billing.ID, nBilling, platform.Closed, &w.person, time.Now())
	w.ok()
	rep = w.distribute(nil)
	if tg := want(t, rep, "gh:acme/svc-billing", report.OutcomeDeclined, "", nBilling); tg.Writes != 2 {
		t.Errorf("first sight of the decline: %d writes, want 2 (ack, comment)", tg.Writes)
	}
	// An empty opt-in file chooses nothing new: the decline holds.
	w.push(billing, "main", w.person, optInName, "version: 1\n")
	rep = w.run(w.deps(ModeDistribute), ModeDistribute)
	if tg := want(t, rep, "gh:acme/svc-billing", report.OutcomeDeclined, "", nBilling); tg.Writes != 0 || tg.Assumed {
		t.Errorf("with an empty opt-in file: %+v", tg)
	}
	// A change of ignore is a new choice: the content comes again, without
	// the paragraph, as the repository has its file now.
	w.push(billing, "main", w.person, optInName, "version: 1\nignore: [notes/**]\n")
	rep = w.distribute(nil)
	tg = targetOf(t, rep, "gh:acme/svc-billing")
	if tg.Outcome != report.OutcomeOpened || tg.PR == nil || tg.PR.Number == nBilling || tg.Assumed {
		t.Fatalf("after the change of ignore: %+v", tg)
	}
	if body := w.p.PR(billing.ID, tg.PR.Number).Body; strings.Contains(body, "subscribed") {
		t.Errorf("a repository with its opt-in file is told it was subscribed:\n%s", body)
	}

	// enabled: false opts the repository out: its pull request is closed.
	w.push(api, "main", w.person, optInName, "version: 1\nenabled: false\n")
	rep = w.distribute(nil)
	if tg := want(t, rep, "gh:acme/svc-api", report.OutcomeClosed, "opted-out", nAPI); tg.Writes != 2 {
		t.Errorf("opt-out: %d writes, want 2 (close, comment)", tg.Writes)
	}
	if got := w.p.PR(api.ID, nAPI).State; got != platform.Closed {
		t.Errorf("the pull request of the opted-out repository is %s", got)
	}
	comments := w.p.Comments(api.ID, nAPI)
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Body, "says `enabled: false`") {
		t.Errorf("close comment %+v", comments)
	}
	rep = w.run(w.deps(ModeDistribute), ModeDistribute)
	want(t, rep, "gh:acme/svc-api", report.OutcomeSkipped, "opted-out", 0)

	// Deleting the file returns it to the hub's subscription: the content
	// comes again, as touchmark's own close was no decline.
	w.push(api, "main", w.person, optInName, "")
	rep = w.distribute(nil)
	tg = targetOf(t, rep, "gh:acme/svc-api")
	if tg.Outcome != report.OutcomeOpened || tg.PR == nil || tg.PR.Number == nAPI || tg.AssumedBy != report.AssumedByHub {
		t.Errorf("after the file is gone again: %+v", tg)
	}
}

// TestRunAssumedOptInWithdrawn: the hub withdraws its subscription (the
// entry drops opt_in: assumed). The open pull request of a subscribed
// repository that never had an opt-in file is closed as opted-out, with a
// comment that blames no deletion and no enabled: false.
func TestRunAssumedOptInWithdrawn(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - org: acme
    opt_in: assumed
`
	svc := w.repo("acme/svc", nil, "README.md", "a service\n")
	rep := w.distribute(nil)
	tg := targetOf(t, rep, "gh:acme/svc")
	if tg.Outcome != report.OutcomeOpened || tg.PR == nil {
		t.Fatalf("subscribed: %+v", tg)
	}
	n := tg.PR.Number

	w.targetsYML = `version: 1
defaults:
  packs: [base]
targets:
  - org: acme
`
	rep = w.distribute(nil)
	want(t, rep, "gh:acme/svc", report.OutcomeClosed, "opted-out", n)
	c := w.p.Comments(svc.ID, n)
	if len(c) == 0 {
		t.Fatal("no close comment")
	}
	body := c[len(c)-1].Body
	if !strings.Contains(body, "has no opt-in file, and the hub does not subscribe it") ||
		!strings.Contains(body, "add the opt-in file `"+optInName+"`") ||
		strings.Contains(body, "enabled") || strings.Contains(body, " back") {
		t.Errorf("close comment:\n%s", body)
	}
}

// TestPlanAssumedOptInDeleted: the opt-in file of a subscribed repository
// is deleted between the API read and the snapshot. It is read again at the
// snapshot's commit, where the subscription holds, so the repository is
// planned as subscribed, not as not opted in (which the sweep would take
// for an opt-out); a reader that keeps answering the file fails the
// target. A repository targets.yml does not subscribe takes the snapshot's
// word: not opted in.
func TestPlanAssumedOptInDeleted(t *testing.T) {
	w := newWorld(t)
	w.targetsYML = assumedTargetsYML
	svc := w.repo("acme/svc-a", nil, "README.md", "service a\n")
	web := w.repo("acme/web", topics("python"), "README.md", "the web site\n")
	d := w.deps()
	stale := &staleOptInOf{Reader: d.Providers[0].Reader, repos: []string{svc.ID, web.ID}}
	d.Providers[0].Reader = stale
	rep := w.plan(d)
	if tg := want(t, rep, "gh:acme/svc-a", report.OutcomeOpened, "", 0); tg.AssumedBy != report.AssumedByHub {
		t.Errorf("svc-a: assumed by %q, want %q", tg.AssumedBy, report.AssumedByHub)
	}
	if got := stale.reads(svc.ID); !slices.Equal(got, []string{"", w.p.Head(svc.ID)}) {
		t.Errorf("svc-a: opt-in reads at %q, want the default branch, then %s", got, w.p.Head(svc.ID))
	}
	want(t, rep, "gh:acme/web", report.OutcomeSkipped, "not-opted-in", 0)
	if got := stale.reads(web.ID); !slices.Equal(got, []string{""}) {
		t.Errorf("web: opt-in reads at %q, want the default branch only", got)
	}

	d.Providers[0].Reader = &staleOptInOf{Reader: w.deps().Providers[0].Reader, repos: []string{svc.ID}, always: true}
	rep = w.plan(d)
	tg := want(t, rep, "gh:acme/svc-a", report.OutcomeFailed, "race", 0)
	if !slices.ContainsFunc(tg.Warnings, func(s string) bool { return strings.Contains(s, "is not in the snapshot") }) {
		t.Errorf("svc-a: warnings %q", tg.Warnings)
	}
}

// TestRunAssumedOptInDeleted: distribute over the race of
// TestPlanAssumedOptInDeleted keeps the open pull request of the subscribed
// repository: it is no opt-out.
func TestRunAssumedOptInDeleted(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	w.targetsYML = assumedTargetsYML
	kept := w.repo("acme/svc-kept", nil, "README.md", "a service with a sync pull request\n")
	w.syncCommit(kept, branch, "", baseFiles...)
	n := w.openOwn(kept)
	w.written(kept, n)
	var stale *staleOptInOf
	rep := w.distribute(func(d *Deps) {
		stale = &staleOptInOf{Reader: d.Providers[0].Writer, repos: []string{kept.ID}}
		d.Providers[0].Writer = staleOptInWriter{Writer: d.Providers[0].Writer, stale: stale}
	})
	if tg := want(t, rep, "gh:acme/svc-kept", report.OutcomeUnchanged, "", n); tg.AssumedBy != report.AssumedByHub {
		t.Errorf("svc-kept: assumed by %q", tg.AssumedBy)
	}
	if got := w.p.PR(kept.ID, n).State; got != platform.Open {
		t.Errorf("the pull request of the subscribed repository is %s", got)
	}
	if got := stale.reads(kept.ID); len(got) < 2 || got[0] != "" || got[1] != w.p.Head(kept.ID) {
		t.Errorf("opt-in reads at %q, want the default branch, then %s", got, w.p.Head(kept.ID))
	}
}

// staleOptInOf answers the first read of the opt-in file of each of repos
// (every read with always) with a file the default branch no longer holds.
type staleOptInOf struct {
	platform.Reader
	repos  []string
	always bool

	mu   sync.Mutex
	refs map[string][]string
}

// staleOptInWriter is a writer, which reads in distribute, whose opt-in
// reads go through stale (whose Reader is the writer).
type staleOptInWriter struct {
	platform.Writer
	stale *staleOptInOf
}

func (s staleOptInWriter) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	return s.stale.ReadFile(ctx, r, ref, path, max)
}

func (s *staleOptInOf) ReadFile(ctx context.Context, r platform.Repo, ref, path string, max int64) (platform.File, error) {
	if path != optInName || !slices.Contains(s.repos, r.ID) {
		return s.Reader.ReadFile(ctx, r, ref, path, max)
	}
	s.mu.Lock()
	if s.refs == nil {
		s.refs = map[string][]string{}
	}
	s.refs[r.ID] = append(s.refs[r.ID], ref)
	first := len(s.refs[r.ID]) == 1
	s.mu.Unlock()
	if s.always || first {
		content := "version: 1\n"
		return platform.File{Path: path, Mode: "100644", OID: oid(content), Content: []byte(content)}, nil
	}
	return s.Reader.ReadFile(ctx, r, ref, path, max)
}

// reads returns the refs the opt-in file of repo was read at.
func (s *staleOptInOf) reads(repo string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.refs[repo])
}
