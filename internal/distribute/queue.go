package distribute

import (
	"context"

	"github.com/bedrock-python/touchmark/internal/report"
)

// provQueue is the write queue of one provider: its works run one after the
// other, each target's writes as one block. What the provider's failures say
// (a rate limit, a streak of refused credentials) is the provider's circuit,
// shared with the inspections before it.
type provQueue struct {
	ex    *executor
	prov  *provider
	index int // the provider's place in hub.yml
	items []queuedWork
	// targets are the targets of the queue's works, once each; left counts
	// the works of each target still to run, est the writes phase E
	// estimated for it and writes those made.
	targets []*target
	left    map[*target]int
	est     map[*target]int
	writes  map[*target]int
	// ops are the queue's mutations, in the order they were made; byTarget
	// the same by target, for its stream line.
	ops      []report.Op
	byTarget map[*target][]report.Op
}

// run runs the queue's works in order. A target whose last work is done is
// streamed and its repository released at once: phase F of a large fleet
// lasts hours, and the repositories of targets done need not wait for it.
func (q *provQueue) run(ctx context.Context) {
	for _, it := range q.items {
		t := it.w.t
		x := newTargetExec(q, it.w)
		x.run(ctx)
		q.writes[t] += x.count()
		q.ops = append(q.ops, x.ops...)
		if q.byTarget == nil {
			q.byTarget = map[*target][]report.Op{}
		}
		q.byTarget[t] = append(q.byTarget[t], x.ops...)
		t.res.Writes = q.writes[t]
		if q.left[t]--; q.left[t] == 0 {
			q.ex.stream(t, q.byTarget[t])
			q.ex.r.release(t)
		}
	}
}

// deferral returns why the queue writes nothing more, or "": the provider
// is out of budget or down (circuit.go).
func (q *provQueue) deferral() (reason, why string) {
	return q.prov.deferral()
}

// settle records how a target of the queue ended in the provider's Gate
// (provider.settle).
func (q *provQueue) settle(res *report.DeliveryTarget) {
	q.prov.settle(res)
}

// refOf is how the report names t: "<provider>:<path>", or "<provider>:"
// for a target a public hub does not name.
func refOf(t *target) string {
	if t.hidden {
		return t.prov.cfg.ID + ":"
	}
	return t.prov.cfg.ID + ":" + t.repo.Path
}
