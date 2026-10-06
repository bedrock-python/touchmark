package distribute

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// sweepAll runs phase D: per provider, the own open pull requests of
// repositories that are no longer targets (target-dropped), or that are no
// longer opted in (opted-out: the target has no opt-in file and targets.yml
// does not subscribe it, or the file says enabled: false; target.optOut
// keeps which, for the comment), become sweep closes. It returns them as
// works, in decide.Sweep's order, provider by provider, and fills the
// report's Sweep.
//
// A plan limited to the targets of some packs (scope.go) keeps only the
// closes of the targets it processes: the targets it leaves out are active
// targets whatever their opt-in file says, and a repository dropped from
// targets.yml is none of its business; the open pull requests the listing
// finds still count for the mass-close guard (noteOpen).
//
// The sweep is off with Only, and for a provider that is unavailable,
// planned without credentials, whose resolve or pull request listing is
// incomplete, that recognizes no pull request as its own, or whose circuit
// is open (Sweep.Reason says why). Completeness is judged per host: a
// provider is swept only when every provider of the run on its host is
// available and resolved every target, since the targets of one may be the
// pull requests another's authors list (a target moved between providers,
// a writer of one listed in the known_authors of the other). A listing that
// fails (after three attempts of a transient failure) marks the sweep
// failed (exit 1); rate limiting and the end of ctx only turn it off. The
// sweep reads with the run's reading identity: the reader in a plan, the
// writer otherwise.
func (r *run) sweepAll(ctx context.Context, kept []*target) []*Work {
	info := &r.rep.Sweep
	if len(r.d.Only) > 0 {
		info.Reason = sweepOffOnly
		return nil
	}
	active, optedOut := map[string]bool{}, map[string]*target{}
	for _, t := range kept {
		key := t.host + "/" + t.repo.ID
		active[key] = true
		if t.outOfScope || t.res.Outcome != report.OutcomeSkipped {
			continue
		}
		// The skip reason says why, before the first close replaces it.
		switch t.res.Reason {
		case reasonNotOptedIn:
			optedOut[key], t.optOut = t, prbody.CauseNoOptIn
		case reasonOptedOut:
			optedOut[key], t.optOut = t, prbody.CauseDisabled
		}
	}
	var works []*Work
	var off []string
	dropped := map[string]*target{}
	complete := true
	for _, p := range r.provs {
		var closes []decide.SweepClose
		why := r.hostIncomplete(p)
		if why == "" {
			closes, why = r.sweepProvider(ctx, p, active, optedOut)
		}
		if why != "" {
			off = append(off, fmt.Sprintf("off for provider %s: %s", p.cfg.ID, why))
			complete = false
			continue
		}
		info.Ran = true
		if r.swept == nil {
			r.swept = map[*provider]bool{}
		}
		r.swept[p] = true
		for _, c := range closes {
			if r.keepSweep(c, optedOut) {
				works = append(works, r.sweepWork(p, c, optedOut, dropped))
			}
		}
	}
	info.Complete = info.Ran && complete
	info.Reason = strings.Join(off, "; ")
	return works
}

// hostIncomplete says why the sweep is off for p because of another
// provider of the run on p's host ("" when there is none): one that is
// unavailable, or whose resolve is incomplete. Its targets are missing from
// the active ones, and p's listing may hold their pull requests.
func (r *run) hostIncomplete(p *provider) string {
	for _, o := range r.provs {
		switch {
		case o == p || !strings.EqualFold(o.cfg.Host, p.cfg.Host):
		case o.info.Error != "":
			return fmt.Sprintf("provider %s on the same host is unavailable", o.cfg.ID)
		case !o.info.ResolveComplete:
			return fmt.Sprintf("the resolve of provider %s on the same host is incomplete", o.cfg.ID)
		}
	}
	return ""
}

// sweepProvider sweeps provider p: it lists the open pull requests of p's
// authors on the sync branches (Reader.OpenPRsBy), keeps touchmark's own
// (decide.Identity) and returns those decide.Sweep closes. why says why the
// sweep is off for p ("" when it ran).
func (r *run) sweepProvider(ctx context.Context, p *provider, active map[string]bool, optedOut map[string]*target) ([]decide.SweepClose, string) {
	switch reason, _ := p.deferral(); {
	case ctx.Err() != nil:
		return nil, "the run stopped before the sweep"
	case p.info.Error != "":
		return nil, "the provider is unavailable"
	case p.anonymous:
		return nil, "it is planned without credentials"
	case !p.info.ResolveComplete:
		return nil, "its resolve is incomplete"
	case len(p.ids) == 0:
		return nil, "touchmark cannot recognize its own pull requests"
	case reason != "":
		return nil, "deferred:" + reason + ": the provider's circuit is open"
	}
	var swept platform.Swept
	ctx = throttle.Default(ctx, p.throttle())
	err := r.retry(ctx, p, func() error {
		var err error
		swept, err = p.reader.OpenPRsBy(ctx, p.authors, r.branches)
		return err
	})
	refusal, refused := throttle.Refused(err)
	if err != nil && ctx.Err() == nil && !refused {
		p.failed(err)
	}
	switch {
	case err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled)):
		return nil, "the run stopped during the sweep"
	case refused:
		return nil, "deferred:" + refusal.Reason + ": " + refusal.Error()
	case err != nil && platform.ClassOf(err) == platform.ClassRateLimited:
		return nil, "deferred:rate-limit: " + err.Error()
	case err != nil:
		r.rep.Sweep.Failed = true
		r.warnf("provider %s: the sweep failed: list open pull requests: %v", p.cfg.ID, err)
		return nil, "listing the open pull requests failed"
	case !swept.Complete:
		return nil, "the listing of its open pull requests is incomplete"
	}
	p.succeeded()
	for _, note := range swept.Notes {
		r.warnf("provider %s: the sweep: %s", p.cfg.ID, note)
	}
	unresolved := map[string]bool{}
	for _, ref := range p.info.Missing {
		_, path, _ := strings.Cut(ref, ":")
		unresolved[strings.ToLower(path)] = true
	}
	opted := map[string]bool{}
	for key := range optedOut {
		opted[key] = true
	}
	cands := r.candidates(p, swept)
	r.noteOpen(cands)
	return decide.Sweep(decide.SweepInput{
		Candidates: cands,
		Active:     active,
		Unresolved: unresolved,
		OptedOut:   opted,
		Complete:   true,
	}), ""
}

// candidates returns the pull requests of swept that are touchmark's own
// (decide.Identity), as sweep candidates of provider p.
func (r *run) candidates(p *provider, swept platform.Swept) []decide.SweepCandidate {
	id := decide.Identity{Branches: r.branches, Authors: p.ids, Fingerprints: r.fps}
	var out []decide.SweepCandidate
	for _, rp := range swept.PRs {
		m, status := id.Own(rp.PR)
		if status != decide.Ours {
			continue
		}
		repo := rp.Repo
		if repo.Host == "" {
			repo.Host = p.cfg.Host
		}
		out = append(out, decide.SweepCandidate{Provider: p.cfg.ID, Repo: repo,
			PR: decide.OwnPR{PR: rp.PR, Marker: m, Alias: rp.PR.Head != r.branches[0]}})
	}
	return out
}

// sweepWork makes the work of one sweep close of provider p. Its target is
// the opted-out target of the run, or a target the sweep adds for the
// dropped repository (one per repository, in dropped): a report line of
// its own, which a public hub does not name when the repository is not
// public. The work closes the pull request (StepClosePR, StepComment); the
// branch stays, since the sweep never reads its history (touchmark
// deletes only a branch it proved rewritable). A pull request in an archived
// repository cannot be closed: blocked:archived.
func (r *run) sweepWork(p *provider, c decide.SweepClose, optedOut, dropped map[string]*target) *Work {
	repo := c.Candidate.Repo
	key := strings.ToLower(repo.Host) + "/" + repo.ID
	t := optedOut[key]
	if t == nil {
		if t = dropped[key]; t == nil {
			t = &target{prov: p, repo: repo, host: strings.ToLower(repo.Host), dropped: true, first: [2]int{len(r.targets.Targets), 0}}
			t.hidden = r.hidePrivate && repo.Visibility != "public"
			t.res = report.DeliveryTarget{Provider: p.cfg.ID, Host: t.host}
			if !t.hidden {
				t.res.RepoID, t.res.Path = repo.ID, repo.Path
			}
			dropped[key] = t
		}
	}
	pr := c.Candidate.PR
	w := &Work{t: t, Sweep: true, SweepPR: &pr, DefaultBranch: repo.DefaultBranch, Marker: pr.Marker.Data}
	w.Own = []decide.OwnPR{pr}
	w.Decision = decide.TargetDecision{Outcome: decide.OutcomeClosed, Reason: c.Reason, PR: pr.PR.Number, Branch: pr.PR.Head,
		Steps: []decide.Step{
			{Kind: decide.StepClosePR, PR: pr.PR.Number, Branch: pr.PR.Head, Reason: c.Reason},
			{Kind: decide.StepComment, PR: pr.PR.Number, Branch: pr.PR.Head, Reason: decide.OutcomeClosed},
		}}
	if repo.Archived {
		w.Decision = decide.TargetDecision{Outcome: decide.OutcomeBlocked, Reason: "archived", PR: pr.PR.Number, Branch: pr.PR.Head}
	}
	w.NeedPerms = needPerms(w.Decision.Steps)
	t.sweeps = append(t.sweeps, w)
	if len(t.sweeps) == 1 {
		r.reportSweep(t, w)
	}
	return w
}

// reportSweep writes the outcome of the sweep work w into its target's
// report line (the first sweep close of a target names its pull request);
// a target a public hub must not name gets no pull request.
func (r *run) reportSweep(t *target, w *Work) {
	res := &t.res
	res.Outcome, res.Reason = report.Outcome(w.Decision.Outcome), w.Decision.Reason
	if !t.hidden {
		res.PR = prRef(w.SweepPR.PR)
	}
}
