package distribute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// These tests need no git: their works write only through the API, on the
// fake in memory mode.

// A decline seen for the first time: the ack goes into the closed pull
// request's marker with the repropose control, then one comment; a second
// run writes nothing.
func TestExecuteAck(t *testing.T) {
	w := newExWorld(t, exConfig{})
	tg := w.target("acme/docs", exOptIn, "version: 1\n")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"ack", "comment"}) || wk.Decision.Outcome != decide.OutcomeDeclined {
		t.Fatalf("decision %s, steps %v", wk.Decision.Outcome, got)
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeDeclined, "", n)
	pr := w.pr(tg, n)
	m := w.marker(pr.Body)
	if !m.Data.Ack || m.Data.OptIn != wk.OptInHash || m.Key != decide.Key(decide.StreamSync, w.missing()) || m.Data.Closed != nil {
		t.Errorf("marker %+v", m.Data)
	}
	if !strings.Contains(pr.Body, prbody.ControlLine(prbody.ControlRepropose)) || !strings.HasPrefix(pr.Body, "Engineering assets from the hub.") {
		t.Errorf("body:\n%s", pr.Body)
	}
	cs := w.p.Comments(tg.repo.ID, n)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "closed without merging") || !strings.Contains(cs[0].Body, `"AGENTS.md"`) {
		t.Errorf("comments %+v", cs)
	}
	if got := w.opKinds(tg); !slices.Equal(got, []string{"edit-pr", "comment"}) || tg.res.Writes != 2 {
		t.Errorf("ops %v, writes %d", got, tg.res.Writes)
	}

	w.p.ResetCalls()
	wk = w.work(tg, nil)
	w.execute(wk)
	exWant(t, tg, report.OutcomeDeclined, "", n)
	if writes := w.p.Writes(); len(writes) > 0 || len(wk.Decision.Steps) > 0 {
		t.Errorf("a second run: steps %v, writes %q", exStepKinds(wk), writes)
	}
}

// The third auto-close in a row with one key counts as a decline: its ack
// and the comment that advises the bot's owners.
func TestExecuteAutoDeclined(t *testing.T) {
	w := newExWorld(t, exConfig{})
	stale := w.p.AddAccount("stale[bot]", platform.KindBot)
	tg := w.target("acme/docs", exOptIn, "version: 1\n")
	var last int64
	for range 3 {
		last = w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
		w.p.SetPRState(tg.repo.ID, last, platform.Closed, &stale, time.Time{})
	}
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"ack", "comment"}) || wk.Decision.Steps[1].Reason != decide.CommentAutoDeclined {
		t.Fatalf("steps %v %+v", got, wk.Decision.Steps)
	}
	w.execute(wk)
	exWant(t, tg, report.OutcomeDeclined, "", last)
	cs := w.p.Comments(tg.repo.ID, last)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "for the third time in a row") || !strings.Contains(cs[0].Body, "`engineering-assets`") {
		t.Errorf("comments %+v", cs)
	}
}

// A ticked repropose control revokes the decline: its marker says revoked.
func TestExecuteRevoke(t *testing.T) {
	w := newExWorld(t, exConfig{})
	tg := w.target("acme/docs", exOptIn, "version: 1\n")
	body := w.ownBody(w.missing(), func(d *marker.Data) { d.Ack = true })
	body = strings.Replace(prbody.AddControl(body, prbody.ControlRepropose), "- [ ]", "- [x]", 1)
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: body})
	w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})
	wk := w.work(tg, nil)
	if got := exStepKinds(wk); !slices.Equal(got, []string{"revoke", "push", "create-pr"}) {
		t.Fatalf("steps %v", got)
	}
	// Without git, the revocation alone.
	wk.Decision.Steps = wk.Decision.Steps[:1]
	wk.Decision.Outcome, wk.Decision.PR = decide.OutcomeUnchanged, 0
	wk.NeedPerms = needPerms(wk.Decision.Steps)
	tg.res.Outcome = report.OutcomeUnchanged
	w.execute(wk)
	exWant(t, tg, report.OutcomeUnchanged, "", 0)
	m := w.marker(w.pr(tg, n).Body)
	if !m.Data.Revoked || !m.Data.Ack {
		t.Errorf("marker %+v", m.Data)
	}
	if got := w.opKinds(tg); !slices.Equal(got, []string{"edit-pr"}) {
		t.Errorf("ops %v", got)
	}
}

// A sweep close: the pull request of a repository that is no target any
// more is closed with its marker's reason and one comment; no branch is
// touched.
func TestExecuteSweepClose(t *testing.T) {
	w := newExWorld(t, exConfig{})
	tg := w.target("acme/gone", exOptIn, "version: 1\n")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	pr := w.pr(tg, n)
	m := w.marker(pr.Body)
	tg.res.Outcome, tg.res.Reason, tg.res.PR = report.OutcomeClosed, decide.ReasonTargetDropped, prRef(pr)
	wk := &Work{t: tg, Sweep: true, SweepPR: &decide.OwnPR{PR: pr, Marker: m},
		Decision: decide.TargetDecision{Outcome: decide.OutcomeClosed, Reason: decide.ReasonTargetDropped, PR: n}}
	w.execute(wk)
	exWant(t, tg, report.OutcomeClosed, decide.ReasonTargetDropped, n)
	pr = w.pr(tg, n)
	if m := w.marker(pr.Body); pr.State != platform.Closed || m.Data.Closed == nil || m.Data.Closed.Reason != decide.ReasonTargetDropped || m.Key != decide.Key(decide.StreamSync, w.missing()) {
		t.Errorf("after the sweep: %s, marker %+v", pr.State, m.Data)
	}
	if cs := w.p.Comments(tg.repo.ID, n); len(cs) != 1 || !strings.Contains(cs[0].Body, "the hub no longer syncs this repository") {
		t.Errorf("comments %+v", cs)
	}
	if res := tg.res.PR; res == nil || res.State != string(platform.Closed) {
		t.Errorf("report PR %+v", res)
	}

	// Closed meanwhile: nothing to close, nothing written.
	n = w.addPR(tg, platform.PR{Head: exAlias, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	pr = w.pr(tg, n)
	wk = &Work{t: tg, Sweep: true, SweepPR: &decide.OwnPR{PR: pr, Marker: w.marker(pr.Body)},
		Decision: decide.TargetDecision{Outcome: decide.OutcomeClosed, Reason: decide.ReasonOptedOut, PR: n}}
	tg.res.Outcome, tg.res.Reason = report.OutcomeClosed, decide.ReasonOptedOut
	w.p.SetPRState(tg.repo.ID, n, platform.Merged, &w.person, time.Time{})
	w.p.ResetCalls()
	w.execute(wk)
	exWant(t, tg, report.OutcomeUnchanged, "", n)
	if writes := w.p.Writes(); len(writes) > 0 || !exHasWarn(tg, "was merged before touchmark closed it") {
		t.Errorf("writes %q, warnings %q", writes, tg.res.Warnings)
	}
}

// One queue per provider: closes first (the sweep's after the targets'),
// then updates, then new pull requests, each in targets.yml order.
func TestExecuteQueueOrder(t *testing.T) {
	w := newExWorld(t, exConfig{})
	// Opening needs a branch: here the crash case, whose branch exists and
	// needs only its pull request, made by hand for the fake in memory mode.
	open := w.target("acme/open", exOptIn, "version: 1\n")
	openWork := w.work(open, nil)
	openWork.Decision = decide.TargetDecision{Outcome: decide.OutcomeOpened, Branch: exBranch, Body: true,
		Steps: []decide.Step{{Kind: decide.StepCreatePR, Branch: exBranch}}}
	open.res.Outcome = report.OutcomeOpened

	update := w.target("acme/update", exOptIn, "version: 1\n")
	n := w.addPR(update, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	w.p.SetPRState(update.repo.ID, n, platform.Closed, &w.person, time.Time{})
	updateWork := w.work(update, nil)

	closing := w.target("acme/close", exOptIn, "version: 1\n", "AGENTS.md", exAgentsV1, "docs/guide.md", exGuide)
	closeN := w.addPR(closing, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	closeWork := w.work(closing, nil)

	swept := w.target("acme/swept")
	n = w.addPR(swept, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	pr := w.pr(swept, n)
	sweepWork := &Work{t: swept, Sweep: true, SweepPR: &decide.OwnPR{PR: pr, Marker: w.marker(pr.Body)},
		Decision: decide.TargetDecision{Outcome: decide.OutcomeClosed, Reason: decide.ReasonTargetDropped, PR: n}}
	swept.res.Outcome, swept.res.Reason = report.OutcomeClosed, decide.ReasonTargetDropped
	// The sweep's target comes first in targets.yml, and still closes after
	// the targets' closes.
	swept.first = [2]int{0, 0}

	w.p.ResetCalls()
	w.execute(openWork, updateWork, sweepWork, closeWork)
	var order []string
	for _, c := range w.p.Writes() {
		f := strings.Fields(c)
		if len(order) == 0 || order[len(order)-1] != f[1] {
			order = append(order, f[1])
		}
	}
	want := []string{"acme/close", "acme/swept", "acme/update", "acme/open"}
	if !slices.Equal(order, want) {
		t.Errorf("targets written in the order %q, want %q (writes %q)", order, want, w.p.Writes())
	}
	var targets []string
	for _, op := range w.r.rep.Ops {
		if len(targets) == 0 || targets[len(targets)-1] != op.Target {
			targets = append(targets, op.Target)
		}
	}
	if !slices.Equal(targets, []string{"gh:acme/close", "gh:acme/swept", "gh:acme/update", "gh:acme/open"}) {
		t.Errorf("ops by target %q", targets)
	}
	exWant(t, open, report.OutcomeOpened, "", open.res.PR.Number)
	exWant(t, closing, report.OutcomeClosed, decide.ReasonNoDiff, closeN)
	exWant(t, swept, report.OutcomeClosed, decide.ReasonTargetDropped, n)
	if lines := w.streamed(); len(lines) != 4 {
		t.Errorf("%d stream lines, want 4", len(lines))
	}
}

// The run's end: past the deadline nothing starts (deferred:deadline), and
// a cancelled run starts nothing either (deferred:interrupted).
func TestExecuteStops(t *testing.T) {
	w := newExWorld(t, exConfig{})
	tg := w.target("acme/docs", exOptIn, "version: 1\n")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})

	w.r.d.Write.Deadline = exEpoch
	wk := w.work(tg, nil)
	w.p.ResetCalls()
	w.execute(wk)
	exWant(t, tg, report.OutcomeDeferred, "deadline", n)
	if calls := w.p.Calls(); len(calls) > 0 {
		t.Errorf("calls past the deadline: %q", calls)
	}

	w.r.d.Write.Deadline = time.Time{}
	wk = w.work(tg, nil)
	ctx, cancel := context.WithCancel(w.ctx)
	cancel()
	w.p.ResetCalls()
	w.ex.run(ctx, []*Work{wk})
	exWant(t, tg, report.OutcomeDeferred, "interrupted", n)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("a cancelled run wrote: %q", writes)
	}
}

// Preflight: an identity that may not write pull requests is
// blocked:permission:<what>; one refused three times in a row by its
// credential puts the provider down for the rest of its queue.
func TestExecutePreflightAndAuth(t *testing.T) {
	w := newExWorld(t, exConfig{})
	var targets []*target
	var works []*Work
	for _, path := range []string{"acme/a", "acme/b", "acme/c", "acme/d", "acme/e"} {
		tg := w.target(path, exOptIn, "version: 1\n")
		n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
		w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})
		targets = append(targets, tg)
		works = append(works, w.work(tg, nil))
	}
	w.p.FailNext("Target", &platform.Error{Op: "target", Class: platform.ClassPermission, Status: http.StatusForbidden,
		Rule: "pull-requests", Err: errors.New("acme-write[bot] may not write pull-requests")})
	unauthorized := &platform.Error{Op: "target", Class: platform.ClassAuth, Status: http.StatusUnauthorized, Err: errors.New("bad credentials")}
	for range 3 {
		w.p.FailNext("Target", unauthorized)
	}
	w.execute(works...)
	exWant(t, targets[0], report.OutcomeBlocked, "permission:pull-requests", works[0].Decision.PR)
	for _, tg := range targets[1:4] {
		exWant(t, tg, report.OutcomeFailed, "auth", tg.res.PR.Number)
	}
	exWant(t, targets[4], report.OutcomeDeferred, "provider-down", targets[4].res.PR.Number)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("writes %q", writes)
	}
}

// A text that holds a secret is never written: the whole target writes
// nothing (failed:secret-exposure), and the report names the text, not the
// secret.
func TestExecuteSecretExposure(t *testing.T) {
	w := newExWorld(t, exConfig{})
	const secret = "tok-5ecr3t-0123456789"
	w.reg.Add(secret)
	tg := w.target("acme/docs", exOptIn, "version: 1\n")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})
	w.r.optIn = "config/" + secret + ".yml" // the comment names the opt-in file
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeFailed, "secret-exposure", n)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("writes %q", writes)
	}
	if !exHasWarn(tg, "the comment on #1 holds a secret") || slices.ContainsFunc(tg.res.Warnings, func(s string) bool { return strings.Contains(s, secret) }) {
		t.Errorf("warnings %q", tg.res.Warnings)
	}
}

// A sweep close in a repository a public hub does not name: closed all the
// same, and neither the journal nor the report line names it.
func TestExecuteSweepHidden(t *testing.T) {
	w := newExWorld(t, exConfig{})
	tg := w.target("acme/secret")
	n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
	pr := w.pr(tg, n)
	tg.hidden = true
	tg.res = report.DeliveryTarget{Provider: "gh", Host: tg.host, Outcome: report.OutcomeClosed, Reason: decide.ReasonTargetDropped}
	w.p.FailNext("Comment", exHTTPErr(platform.ClassTransient, http.StatusBadGateway))
	w.execute(&Work{t: tg, Sweep: true, SweepPR: &decide.OwnPR{PR: pr, Marker: w.marker(pr.Body)},
		Decision: decide.TargetDecision{Outcome: decide.OutcomeClosed, Reason: decide.ReasonTargetDropped, PR: n}})
	if got := w.pr(tg, n); got.State != platform.Closed {
		t.Errorf("the pull request is %s", got.State)
	}
	for _, op := range w.r.rep.Ops {
		if op.Target != "gh:" {
			t.Errorf("op %+v names the target", op)
		}
	}
	if tg.res.Path != "" || len(tg.res.Warnings) > 0 || strings.Contains(w.stream.String(), "acme/secret") {
		t.Errorf("report line %+v, stream %s", tg.res, w.stream.String())
	}
}

// Two providers write in parallel, one queue each; the journal orders
// their ops by time, then provider.
func TestExecuteProviders(t *testing.T) {
	w := newExWorld(t, exConfig{})
	gl := fake.New("gitlab.example.com", fake.WithFlavor(fake.GitLab))
	glWriter := gl.AddAccount("tm-writer", platform.KindServiceAccount)
	glPerson := gl.AddAccount("jdoe", platform.KindUser)
	glProv := &provider{
		cfg:     config.ResolvedProvider{Provider: config.Provider{ID: "corp", Type: "gitlab", Writer: "tm-writer"}, Host: "gitlab.example.com"},
		reader:  gl.Writer(glWriter),
		writer:  gl.Writer(glWriter),
		self:    glWriter,
		caps:    gl.Caps(),
		authors: []platform.Account{glWriter},
		ids:     []string{glWriter.ID},
		info:    &report.ProviderInfo{ID: "corp"},
		gate:    w.gate("corp"),
	}
	w.r.provs = append(w.r.provs, glProv)
	var works []*Work
	for i := range 3 {
		tg := w.target(fmt.Sprintf("acme/gh-%d", i), exOptIn, "version: 1\n")
		n := w.addPR(tg, platform.PR{Head: exBranch, Author: w.writer, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
		w.p.SetPRState(tg.repo.ID, n, platform.Closed, &w.person, time.Time{})
		works = append(works, w.work(tg, nil))

		r := gl.AddRepo(platform.Repo{Path: fmt.Sprintf("platform/gl-%d", i)})
		gl.SetFile(r.ID, exOptIn, []byte("version: 1\n"), "")
		gl.GrantWrite(r.ID, glWriter)
		n = gl.AddPR(r.ID, platform.PR{Head: exBranch, Author: glWriter, Title: exTitle, Body: w.ownBody(w.missing(), nil)})
		gl.SetPRState(r.ID, n, platform.Closed, &glPerson, time.Time{})
		if err := gl.Err(); err != nil {
			t.Fatal(err)
		}
		repo, _ := gl.RepoByID(r.ID)
		gt := &target{prov: glProv, repo: repo, host: repo.Host, first: [2]int{1, i}}
		gt.res = report.DeliveryTarget{Provider: "corp", Host: repo.Host, RepoID: repo.ID, Path: repo.Path, Outcome: report.OutcomeDeclined}
		wk := &Work{t: gt, DefaultBranch: "main", OptInHash: works[0].OptInHash}
		pr := gl.PR(r.ID, n)
		m := w.marker(pr.Body)
		wk.Own = []decide.OwnPR{{PR: pr, Marker: m}}
		wk.Decision = decide.TargetDecision{Outcome: decide.OutcomeDeclined, PR: n, Steps: []decide.Step{
			{Kind: decide.StepAck, PR: n}, {Kind: decide.StepComment, PR: n, Reason: decide.OutcomeDeclined},
		}}
		wk.NeedPerms = needPerms(wk.Decision.Steps)
		works = append(works, wk)
	}
	w.execute(works...)
	byProv := map[string]int{}
	for i, op := range w.r.rep.Ops {
		byProv[strings.SplitN(op.Target, ":", 2)[0]]++
		if i > 0 && w.r.rep.Ops[i-1].Time.After(op.Time) {
			t.Errorf("ops out of time order: %+v before %+v", w.r.rep.Ops[i-1], op)
		}
	}
	if byProv["gh"] != 6 || byProv["corp"] != 6 {
		t.Errorf("ops by provider %v", byProv)
	}
	for _, wk := range works {
		exWant(t, wk.t, report.OutcomeDeclined, "", wk.Decision.PR)
	}
	if v := gl.Violations(); len(v) > 0 {
		t.Errorf("violations on GitLab: %q", v)
	}
}
