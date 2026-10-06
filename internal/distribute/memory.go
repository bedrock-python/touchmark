package distribute

import (
	"slices"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
)

// memory runs step 7: what the target's closed own pull requests say
// (decide.BuildMemory), with the opt-in file's hash, the paths the per-path
// plan finds local or ignored, the forget_declines of operations and the
// ticked repropose controls of closed bodies.
func (r *run) memory(t *target, w *Work, ops decide.TargetOps) decide.Memory {
	repropose := map[int64]bool{}
	for _, o := range w.Own {
		if o.PR.State == platform.Closed && prbody.Ticked(o.PR.Body, prbody.ControlRepropose) {
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
	return decide.BuildMemory(decide.MemoryInput{
		Own:            w.Own,
		OptIn:          w.OptInHash,
		LocalOrIgnored: func(path string) bool { return local[path] },
		Forget:         ops.Forget,
		Repropose:      repropose,
		Now:            r.now,
		Config: decide.MemoryConfig{
			Cooldown:    r.cooldown,
			Writers:     writers,
			Automation:  t.prov.automation,
			CloserKnown: t.prov.caps.CloserKnown,
		},
	})
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
