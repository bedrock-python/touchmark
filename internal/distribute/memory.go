package distribute

import (
	"fmt"
	"slices"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
)

// memory runs step 7: what the target's closed own pull requests say
// (decide.BuildMemory), with the opt-in file's hash, the paths the per-path
// plan finds local or ignored, the forget_declines of operations and the
// ticked repropose controls of closed bodies (none where descriptions carry
// no control, Caps.BodyControls).
//
// On a platform whose closed pull requests are immutable
// (Caps.ClosedImmutable), each decline in force whose marker records no
// opt-in state gets a target warning that says how to lift it: memory
// cannot tell when the team changed its choice (Memory.Unanchored).
func (r *run) memory(t *target, w *Work, ops decide.TargetOps) decide.Memory {
	repropose := map[int64]bool{}
	for _, o := range w.Own {
		if t.prov.caps.BodyControls() && o.PR.State == platform.Closed && prbody.Ticked(o.PR.Body, prbody.ControlRepropose) {
			repropose[o.PR.Number] = true
		}
	}
	local := map[string]bool{}
	for _, e := range w.Plan.Entries {
		switch e.State {
		case decide.Local, decide.RetiredLocal, decide.Ignored:
			local[e.Path] = true
		}
	}
	writers := make(map[string]bool, len(t.prov.ids))
	for _, id := range t.prov.ids {
		writers[id] = true
	}
	m := decide.BuildMemory(decide.MemoryInput{
		Own:            w.Own,
		OptIn:          w.OptInHash,
		LocalOrIgnored: func(path string) bool { return local[path] },
		Forget:         ops.Forget,
		Repropose:      repropose,
		Now:            r.now,
		Config: decide.MemoryConfig{
			Cooldown:        r.cooldown,
			Writers:         writers,
			Automation:      t.prov.automation,
			CloserKnown:     t.prov.caps.CloserKnown,
			ClosedImmutable: t.prov.caps.ClosedImmutable,
		},
	})
	for _, n := range m.Unanchored {
		r.targetWarning(t, unanchoredWarning(r.prName(t, n)))
	}
	return m
}

// unanchoredWarning is the target warning of a decline in force whose
// marker records no opt-in state, on a platform whose closed pull requests
// cannot be edited (decide.Memory.Unanchored): pr is "#7".
func unanchoredWarning(pr string) string {
	return fmt.Sprintf("the decline of %s holds until a forget_declines entry in .touchmark/operations.yml names it: "+
		"its marker records no state of the opt-in file, so a change of packs or ignore does not lift it, "+
		"and a declined pull request cannot be edited on this platform", pr)
}

// staleOptIn reports whether the marker m of an open pull request of w must
// be written again to record the current opt-in state: on a platform whose
// closed pull requests are immutable, the optin an open pull request's
// marker holds is what memory reads once the pull request is declined
// (decide.MemoryConfig.ClosedImmutable), so it follows the opt-in file
// while the pull request is open. Never on the other platforms, whose
// bodies an opt-in change alone leaves as they are.
func (w *Work) staleOptIn(m marker.Marker) bool {
	return w.t.prov.caps.ClosedImmutable && m.Key != "" && m.Data.OptIn != w.OptInHash
}

// refreshOptIn adds a decide.StepRefreshMarker for each open own pull
// request of w whose marker's optin is stale (Work.staleOptIn) and that no
// step of the final decision writes to, after every rule that rewrites the
// decision in phase C (blockProtected, checkSigning): one body-only edit
// each, which the estimates count. Without it, a decline of such a pull
// request would be read from the opt-in state it was opened under, and
// would lapse at once. Only where closed pull requests are immutable
// (staleOptIn is false elsewhere).
//
// Which pull requests it writes to, and why:
//   - Only w.Own: pull requests of touchmark's authors that carry a valid
//     marker of this hub. A foreign pull request (the one that blocks
//     branch-in-use, a fork's, another hub's) and a marker-invalid one are
//     never there; an adopted one has no marker key, so it is never stale.
//   - A pull request a step writes to already gets the current optin: an
//     edit (Work.staleOptIn makes it write), a close (a close of touchmark's
//     is no decline), a consumed recreate (an edit follows it).
//   - blocked:branch-in-use, blocked:rules:*, blocked:permission:workflows
//     and blocked:cannot-sign keep our open pull request without writing to
//     it: it gets the edit. The edit moves no branch, so the foreign pull
//     request or the rule that blocks the push plays no part.
//   - blocked:edited edits the kept pull request already (its Paused or
//     NothingMore block); a duplicate left open on an edited or foreign
//     branch gets the edit, which touches no commit of people's.
//   - blocked:branch-taken has no own open pull request (DecideTarget's
//     rule 3): there is nothing to refresh.
//   - blocked:marker-invalid writes memory upkeep only (DecideTarget's rule
//     1); the edit is upkeep of the same kind, on a pull request that is
//     ours by its marker, and leaves the marker-invalid one alone.
func (w *Work) refreshOptIn() {
	d := &w.Decision
	for _, o := range w.Own {
		if o.PR.State != platform.Open || !w.staleOptIn(o.Marker) || touches(*d, o.PR.Number) {
			continue
		}
		d.Steps = append(d.Steps, decide.Step{Kind: decide.StepRefreshMarker, PR: o.PR.Number, Branch: o.PR.Head})
	}
}

// operations are the operations of the run: operations.yml of the default
// branch, then those given as flags in a local run; both apply.
func (r *run) operations() []*config.Operations {
	var out []*config.Operations
	for _, ops := range []*config.Operations{r.d.Write.Operations, r.d.Write.LocalOps} {
		if ops != nil {
			out = append(out, ops)
		}
	}
	return out
}

// targetOps collects the operations for t (decide.OpsFor) from every source
// of the run, by every provider that found t and every path it goes by
// (the canonical one and those targets.yml wrote), merged: recreate heads
// and forgotten pull requests are united, adopt_unmarked holds when any
// source has it active.
func (r *run) targetOps(t *target) decide.TargetOps {
	var out decide.TargetOps
	for _, ops := range r.operations() {
		for _, id := range t.providers {
			for _, path := range t.paths() {
				o := decide.OpsFor(ops, r.hub, r.targets, id, path, r.now)
				for _, h := range o.RecreateHeads {
					if !slices.Contains(out.RecreateHeads, h) {
						out.RecreateHeads = append(out.RecreateHeads, h)
					}
				}
				for _, n := range o.Forget {
					if !slices.Contains(out.Forget, n) {
						out.Forget = append(out.Forget, n)
					}
				}
				out.AdoptUnmarked = out.AdoptUnmarked || o.AdoptUnmarked
			}
		}
	}
	return out
}

// massCloseOp is the allow_mass_close of the run that is active now: the
// larger of those operations.yml and the local flags give.
func (r *run) massCloseOp() *config.MassCloseOp {
	var out *config.MassCloseOp
	for _, ops := range r.operations() {
		if a := decide.ActiveMassClose(ops, r.now); a != nil && (out == nil || a.Max > out.Max) {
			out = a
		}
	}
	return out
}
