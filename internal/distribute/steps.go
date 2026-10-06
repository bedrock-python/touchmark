package distribute

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
)

// actKind is what one write does.
type actKind uint8

const (
	// actPush moves a branch with a lease; an empty commit deletes it.
	actPush actKind = iota + 1
	actCreate
	actEdit
	actComment
)

// writeAct is one write of a target, rendered in full before the target's
// first write.
type writeAct struct {
	kind actKind
	// branch, commit and expect are a push's (commit "" deletes the
	// branch); ref, when set, is the hidden ref the push moves instead of
	// the branch (the stage ref of an API commit). api makes the push of a
	// commit to branch an API commit (Work.viaAPI).
	branch, commit, expect string
	ref                    string
	api                    bool
	// pr is the pull request an edit or a comment writes to.
	pr      int64
	newPR   platform.NewPR
	edit    platform.PREdit
	comment string
	// field is "title" or "body" for an edit of the open pull request's
	// fields (StepEditPR), what an unchanged decision reports it updated.
	field string
	// soft marks a secondary write: a comment, a branch deletion after a
	// close (see execute).
	soft bool
	// desc says what the write did, for warnings ("pushed touchmark/acme").
	desc string
}

// preparer renders the writes of one work.
type preparer struct {
	x *targetExec
	// pushed is set once a step pushed the branch: the marker then
	// describes D.
	pushed bool
	// closed holds the reason of each pull request a step closes, for the
	// comment after it.
	closed map[int64]string
	// body is the rendered human part of the pull request body, once
	// rendered.
	body *string
}

// prepare renders every write of the work (execute's step 4) and checks
// every text against the registered secrets before anything is written. ok
// is false when the target ended: a text it cannot write (failed:internal)
// or one that holds a secret (failed:secret-exposure).
func (x *targetExec) prepare(ctx context.Context) ([]writeAct, bool) {
	p := &preparer{x: x, closed: map[int64]string{}}
	var acts []writeAct
	for i, s := range workSteps(x.w) {
		if s.Kind == decide.StepEditPR && x.w.idle[i] {
			continue // phase C found it changes nothing
		}
		a, err := p.step(ctx, s)
		if err != nil {
			x.end(report.OutcomeFailed, "internal", fmt.Sprintf("prepare the %s step: %v", s.Kind, err))
			x.progress()
			return nil, false
		}
		acts = append(acts, a...)
	}
	what, err := x.leak(ctx, acts)
	switch {
	case err != nil:
		x.fail("read the commit to push", gitFailure(err))
		return nil, false
	case what != "":
		x.end(report.OutcomeFailed, "secret-exposure", what+" holds a secret touchmark holds; nothing is written")
		x.progress()
		return nil, false
	}
	if err := unsafeText(acts); err != nil {
		x.end(report.OutcomeFailed, "internal", err.Error()+"; nothing is written")
		x.progress()
		return nil, false
	}
	return acts, true
}

// unsafeText checks every body and comment of acts with prbody.CheckText:
// none may run a GitLab quick action or mention anyone, whatever made it.
// Bodies touchmark renders pass by construction, and so do those it writes
// back (keptBody): a failure is a bug, and the target writes nothing.
func unsafeText(acts []writeAct) error {
	for _, a := range acts {
		var text, what string
		switch {
		case a.kind == actCreate:
			text, what = a.newPR.Body, "the new pull request's description"
		case a.kind == actEdit && a.edit.Body != nil:
			text, what = *a.edit.Body, fmt.Sprintf("the description of #%d", a.pr)
		case a.kind == actComment:
			text, what = a.comment, fmt.Sprintf("the comment on #%d", a.pr)
		default:
			continue
		}
		if err := prbody.CheckText(text); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return nil
}

// step renders the writes of one step.
func (p *preparer) step(ctx context.Context, s decide.Step) ([]writeAct, error) {
	x := p.x
	switch s.Kind {
	case decide.StepConsumeRecreate:
		return p.consume(s)
	case decide.StepPush:
		if x.w.Built.Commit == "" {
			return nil, errors.New("no commit was built to push")
		}
		p.pushed = true
		return []writeAct{{kind: actPush, branch: s.Branch, commit: x.w.Built.Commit, expect: s.Expect, api: x.w.viaAPI, desc: "pushed " + s.Branch}}, nil
	case decide.StepRecreateBranch:
		if x.w.Built.Commit == "" {
			return nil, errors.New("no commit was built to push")
		}
		p.pushed = true
		return []writeAct{
			{kind: actPush, branch: s.Branch, expect: s.Expect, desc: "deleted " + s.Branch},
			{kind: actPush, branch: s.Branch, commit: x.w.Built.Commit, api: x.w.viaAPI, desc: "pushed " + s.Branch},
		}, nil
	case decide.StepDeleteBranch:
		if x.w.Repo == nil {
			x.warn("branch " + s.Branch + " is left in place: touchmark deletes a branch only through the target's repository")
			return nil, nil
		}
		return []writeAct{{kind: actPush, branch: s.Branch, expect: s.Expect, soft: true, desc: "deleted " + s.Branch}}, nil
	case decide.StepCreatePR:
		return p.create(ctx, s)
	case decide.StepEditPR:
		return p.editOpen(ctx, s)
	case decide.StepClosePR:
		return p.close(ctx, s)
	case decide.StepComment:
		return p.comment(s)
	case decide.StepAck, decide.StepRevoke:
		return p.remember(s)
	}
	return nil, fmt.Errorf("unknown step %d", s.Kind)
}

// consume unticks the recreate control of the open pull request and writes
// recreate_for into its marker, before the push. The control must still be
// ticked in the body the recheck just read (it compares the controls with
// inspection's, and a change re-inspects the target): a rebuild drops
// people's commits only with their live consent (I2).
func (p *preparer) consume(s decide.Step) ([]writeAct, error) {
	pr, m, err := p.x.known(s.PR)
	if err != nil {
		return nil, err
	}
	if !prbody.Ticked(pr.Body, prbody.ControlRecreate) {
		return nil, fmt.Errorf("the rebuild control of #%d is no longer ticked", s.PR)
	}
	d := dataCopy(m.Data)
	head := s.Expect
	d.RecreateFor = &head
	p.x.stamp(&d, false)
	line, err := p.x.markerLine(m.Key, d)
	if err != nil {
		return nil, err
	}
	body := keptBody(prbody.Untick(pr.Body, prbody.ControlRecreate), line)
	return []writeAct{{kind: actEdit, pr: s.PR, edit: platform.PREdit{Body: &body}, desc: fmt.Sprintf("took the rebuild request of #%d", s.PR)}}, nil
}

// keptBody is a body touchmark writes back with a new marker line: people's
// part of it made inert first (prbody.Inert), so that touchmark never runs a
// GitLab quick action or mentions anyone with what people wrote.
func keptBody(body, markerLine string) string {
	return prbody.ReplaceMarker(prbody.Inert(marker.Strip(body)), markerLine)
}

// create opens the pull request of the sync branch: the rendered body with a
// marker of D (the branch holds D once pushed).
func (p *preparer) create(ctx context.Context, s decide.Step) ([]writeAct, error) {
	x := p.x
	human, err := p.render()
	if err != nil {
		return nil, err
	}
	d, key := x.withContent(ctx, marker.Data{}, true, decide.Branch{})
	hub := x.r.hub
	d.TitleSet = hub.PR.Title
	d.Body = decide.BodyHash(human)
	d.LabelsSet = slices.Clone(hub.PR.Labels)
	x.stamp(&d, true)
	line, err := x.markerLine(key, d)
	if err != nil {
		return nil, err
	}
	base := cmp.Or(x.w.DefaultBranch, x.t.repo.DefaultBranch)
	if base == "" {
		return nil, errors.New("the default branch is unknown")
	}
	np := platform.NewPR{
		Head:   s.Branch,
		Base:   base,
		Title:  hub.PR.Title,
		Body:   human + "\n\n" + line,
		Labels: slices.Clone(hub.PR.Labels),
		Draft:  hub.PR.Draft,
	}
	return []writeAct{{kind: actCreate, branch: s.Branch, newPR: np, desc: "opened a pull request from " + s.Branch}}, nil
}

// editOpen edits the open pull request's fields touchmark owns: nothing
// when PlanPREdit changes nothing and Content is not set.
func (p *preparer) editOpen(ctx context.Context, s decide.Step) ([]writeAct, error) {
	x := p.x
	pr, m, err := x.known(s.PR)
	if err != nil {
		return nil, err
	}
	human, err := p.render()
	if err != nil {
		return nil, err
	}
	hub := x.r.hub
	edit, next, changed := decide.PlanPREdit(pr, m, decide.DesiredPR{
		Title:       hub.PR.Title,
		Body:        human,
		Labels:      hub.PR.Labels,
		Base:        s.Base,
		DraftPrefix: draftPrefix(x.t.prov.caps),
	})
	empty := m.Key == ""
	if !changed && !s.Content && !empty {
		return nil, nil
	}
	key := m.Key
	if s.Content || empty {
		next, key = x.withContent(ctx, next, p.pushed, x.branchOf(s.Branch))
	}
	// A consumed recreate is done once the branch is rebuilt: by this run's
	// push, or by the push of a run that stopped before this edit. Any other
	// edit finds it stale (people moved the branch again, and the block asks
	// for a new tick). Kept, it would let the branch be rebuilt without a
	// tick the day its head is that commit again (someone restoring the
	// commits the rebuild dropped).
	next.RecreateFor = nil
	if next.TitleSet == "" && decide.PlainTitle(pr, draftPrefix(x.t.prov.caps)) == hub.PR.Title {
		// An adopted pull request whose marker was lost: its title is
		// still the one touchmark sets.
		next.TitleSet = pr.Title
	}
	x.stamp(&next, s.Content || empty)
	line, err := x.markerLine(key, next)
	if err != nil {
		return nil, err
	}
	body := human + "\n\n" + line
	field := "body"
	if titleOnly(edit, m, human) && !s.Content && !empty {
		field = "title"
	}
	edit.Body = &body
	return []writeAct{{kind: actEdit, pr: s.PR, edit: edit, field: field, desc: fmt.Sprintf("edited #%d", s.PR)}}, nil
}

// close closes a pull request of touchmark's in one edit: the marker's
// closed and the state.
func (p *preparer) close(ctx context.Context, s decide.Step) ([]writeAct, error) {
	x := p.x
	pr, m, err := x.known(s.PR)
	if err != nil {
		return nil, err
	}
	d, key := dataCopy(m.Data), m.Key
	content := s.Content || key == ""
	if content {
		d, key = x.withContent(ctx, d, false, x.branchOf(s.Branch))
	}
	d.Closed = &marker.Closed{By: "touchmark", Reason: s.Reason}
	x.stamp(&d, content)
	line, err := x.markerLine(key, d)
	if err != nil {
		return nil, err
	}
	body := keptBody(pr.Body, line)
	state := platform.Closed
	p.closed[s.PR] = s.Reason
	return []writeAct{{kind: actEdit, pr: s.PR, edit: platform.PREdit{Body: &body, State: &state}, desc: fmt.Sprintf("closed #%d", s.PR)}}, nil
}

// comment renders the comment after a close or a first-seen decline.
func (p *preparer) comment(s decide.Step) ([]writeAct, error) {
	x := p.x
	var text string
	switch s.Reason {
	case decide.OutcomeClosed:
		text = prbody.ClosedComment(cmp.Or(p.closed[s.PR], x.w.Decision.Reason, x.res.Reason), x.t.optOut, x.r.optIn)
	case decide.OutcomeDeclined:
		text = prbody.DeclinedComment(s.PR, x.declinedPaths(s.PR), x.r.optIn)
	case decide.CommentAutoDeclined:
		text = prbody.AutoDeclinedComment(s.PR, x.declinedPaths(s.PR), x.r.optIn, x.r.hub.PR.Labels)
	default:
		return nil, fmt.Errorf("unknown comment %q", s.Reason)
	}
	return []writeAct{{kind: actComment, pr: s.PR, comment: text, soft: true, desc: fmt.Sprintf("commented on #%d", s.PR)}}, nil
}

// remember writes a decline's ack (the marker's ack and optin, and the
// repropose control) or its revocation into the declined pull request.
func (p *preparer) remember(s decide.Step) ([]writeAct, error) {
	x := p.x
	pr, m, err := x.known(s.PR)
	if err != nil {
		return nil, err
	}
	d := dataCopy(m.Data)
	body := pr.Body
	desc := fmt.Sprintf("remembered the decline of #%d", s.PR)
	if s.Kind == decide.StepAck {
		d.Ack, d.OptIn = true, x.w.OptInHash
		body = prbody.AddControl(body, prbody.ControlRepropose)
	} else {
		d.Revoked = true
		desc = fmt.Sprintf("forgot the decline of #%d", s.PR)
	}
	x.stamp(&d, false)
	line, err := x.markerLine(m.Key, d)
	if err != nil {
		return nil, err
	}
	body = keptBody(body, line)
	return []writeAct{{kind: actEdit, pr: s.PR, edit: platform.PREdit{Body: &body}, desc: desc}}, nil
}

// render renders the human part of the pull request body once per work,
// exactly as phase C rendered it to decide the edits (Work.humanBody:
// Work.Body with the decision's blocks, and the recreate control while the
// branch is paused), so that a run writes what its plan showed.
func (p *preparer) render() (string, error) {
	if p.body == nil {
		body, err := p.x.w.humanBody()
		if err != nil {
			return "", err
		}
		p.body = &body
	}
	return *p.body, nil
}

// known returns a pull request of the work and its marker, as last read.
func (x *targetExec) known(n int64) (platform.PR, marker.Marker, error) {
	pr, ok := x.prs[n]
	if !ok || n <= 0 {
		return platform.PR{}, marker.Marker{}, fmt.Errorf("pull request #%d is not one the work knows", n)
	}
	return pr, x.marks[n], nil
}

// branchOf returns the classification of the sync branch name (absent when
// inspection did not classify it).
func (x *targetExec) branchOf(name string) decide.Branch {
	w := x.w
	if name == w.Branch.Name {
		return w.Branch
	}
	for key, b := range w.Aliases {
		if cmp.Or(b.Name, key) == name {
			return b
		}
	}
	return decide.Branch{Name: name}
}

// declinedPaths returns the paths a declined pull request carried (its
// marker's changes).
func (x *targetExec) declinedPaths(n int64) []string {
	var paths []string
	for _, c := range x.marks[n].Data.Changes {
		paths = append(paths, c.Path)
	}
	return paths
}

// withContent returns d with its content fields describing what the branch
// b holds after the decision (decide.Step.Content), and the content key:
// D (Work.Marker, Work.Key) when the work pushed it or b has no commit of
// touchmark's; otherwise b's C, with the hub commit of Hc's trailer.
func (x *targetExec) withContent(ctx context.Context, d marker.Data, pushed bool, b decide.Branch) (marker.Data, string) {
	base := x.markerBase()
	d.HubRepo, d.OptIn, d.Base, d.Packs = base.HubRepo, base.OptIn, base.Base, base.Packs
	if pushed || b.Hc == "" || b.CKey == "" {
		d.Changes, d.ChangesComplete, d.ContentCommit = base.Changes, base.ChangesComplete, base.ContentCommit
		key := x.w.Key
		if key == "" {
			key = decide.Key(decide.StreamSync, x.w.D)
		}
		return d, key
	}
	d.Changes, d.ChangesComplete = decide.ShortChanges(b.C), true
	d.ContentCommit = cmp.Or(x.hubCommitOf(ctx, b.Hc), base.ContentCommit)
	return d, b.CKey
}

// hubCommitOf returns the Touchmark-Hub-Commit trailer of commit hc, read
// from the target's repository; "" when it cannot be read.
func (x *targetExec) hubCommitOf(ctx context.Context, hc string) string {
	if x.w.Repo == nil {
		return ""
	}
	c, err := x.w.Repo.Commit(ctx, hc)
	if err != nil {
		return ""
	}
	tr, ok := decide.ParseTrailers(c.Message)
	if !ok {
		return ""
	}
	return tr.HubCommit
}

// markerBase returns Work.Marker with what inspection left out filled in
// from the run: the hub commit, B, the opt-in hash, the engine, the packs
// and the changes of D.
func (x *targetExec) markerBase() marker.Data {
	w := x.w
	d := dataCopy(w.Marker)
	d.DecidedAt = cmp.Or(d.DecidedAt, x.r.d.HubCommit)
	d.ContentCommit = cmp.Or(d.ContentCommit, x.r.d.HubCommit)
	d.Base = cmp.Or(d.Base, w.B)
	d.OptIn = cmp.Or(d.OptIn, w.OptInHash)
	d.Engine = cmp.Or(d.Engine, x.r.d.Engine)
	if d.Packs == nil {
		d.Packs = slices.Clone(x.res.Packs)
	}
	if d.Changes == nil && len(w.D) > 0 {
		d.Changes, d.ChangesComplete = decide.ShortChanges(w.D), true
	}
	return d
}

// stamp updates the marker fields that follow any write: the engine and the
// hub commit that decided it, and with content the base it was decided on.
func (x *targetExec) stamp(d *marker.Data, content bool) {
	d.Engine = cmp.Or(x.r.d.Engine, d.Engine)
	d.DecidedAt = cmp.Or(x.r.d.HubCommit, d.DecidedAt)
	if content {
		d.Base = cmp.Or(x.w.B, d.Base)
	}
}

// markerLine encodes the marker of d under key, as this hub (its id and
// current fingerprint) writes it.
func (x *targetExec) markerLine(key string, d marker.Data) (string, error) {
	if key == "" {
		return "", errors.New("the marker has no content key")
	}
	d.V = marker.Version
	d.Stream = cmp.Or(d.Stream, decide.StreamSync)
	d.Hub, d.FP = x.r.hub.ID, x.r.fps[0]
	return marker.Encode(marker.Marker{Key: key, Data: d})
}

// leak returns which text of the writes holds a registered secret ("the edit
// of #3", …), "" when none does. The commit message of a push is read from
// the target's repository; err is a failure to read it.
func (x *targetExec) leak(ctx context.Context, acts []writeAct) (string, error) {
	reg := x.r.d.Write.Redact
	has := func(texts ...string) bool {
		return slices.ContainsFunc(texts, reg.Contains)
	}
	for _, a := range acts {
		switch a.kind {
		case actPush:
			if has(a.branch) {
				return "the branch name " + a.branch, nil
			}
			if a.commit == "" {
				continue
			}
			if x.w.Repo == nil {
				return "", errors.New("no repository to read the commit from")
			}
			c, err := x.w.Repo.Commit(ctx, a.commit)
			if err != nil {
				return "", err
			}
			if has(c.Message) {
				return "the commit message", nil
			}
		case actCreate:
			if has(a.newPR.Head, a.newPR.Title, a.newPR.Body) || has(a.newPR.Labels...) {
				return "the new pull request", nil
			}
		case actEdit:
			if (a.edit.Body != nil && has(*a.edit.Body)) || (a.edit.Title != nil && has(*a.edit.Title)) ||
				(a.edit.Base != nil && has(*a.edit.Base)) || has(a.edit.AddLabels...) {
				return fmt.Sprintf("the edit of #%d", a.pr), nil
			}
		case actComment:
			if has(a.comment) {
				return fmt.Sprintf("the comment on #%d", a.pr), nil
			}
		}
	}
	return "", nil
}

// dataCopy returns a copy of d that shares no slice or pointer with it.
func dataCopy(d marker.Data) marker.Data {
	d.Packs = slices.Clone(d.Packs)
	d.Changes = slices.Clone(d.Changes)
	d.LabelsSet = slices.Clone(d.LabelsSet)
	if d.Closed != nil {
		c := *d.Closed
		d.Closed = &c
	}
	if d.RecreateFor != nil {
		r := *d.RecreateFor
		d.RecreateFor = &r
	}
	return d
}
