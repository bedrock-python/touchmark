package distribute

import (
	"context"
	"fmt"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// verify reads back the pull request a push or CreatePR left (execute's
// step 5): it must be open with the commit at its head. The write's own
// answer counts as the first reading; then up to 3 reads, 1, 2 and 4 s
// apart. A pull request that never shows it gets the warning "eventual":
// platforms update a pull request's head after a push asynchronously, and
// the next run sees the outcome.
func (x *targetExec) verify(ctx context.Context) {
	n := x.created
	if n == 0 && x.pushed != "" {
		n = x.w.Decision.PR
	}
	pr, ok := x.prs[n]
	if n <= 0 || !ok {
		return
	}
	head := x.pushed
	if head == "" {
		head, _ = x.classified(pr.Head)
	}
	if head == "" {
		return
	}
	shows := func(pr platform.PR) bool { return pr.State == platform.Open && pr.HeadSHA == head }
	if shows(pr) {
		return
	}
	g := x.t.prov.throttle()
	for _, d := range verifyDelays {
		if x.ex.sleep(ctx, d) != nil {
			break
		}
		tk := g.TicketFor(ctx, throttle.ReadCall)
		prs, err := x.t.prov.reader.PRs(ctx, x.t.repo, []string{pr.Head}, x.t.prov.authors)
		if platform.ClassOf(err) == platform.ClassRateLimited {
			// The provider pauses; the next run sees the outcome.
			g.Limited(tk, retryAfterOf(err))
			break
		}
		if _, refused := throttle.Refused(err); refused {
			break
		}
		if err != nil {
			continue
		}
		g.Passed(tk)
		for _, got := range prs {
			if got.Number != n {
				continue
			}
			x.remember(got)
			if shows(got) {
				return
			}
		}
	}
	x.warn(fmt.Sprintf("eventual: #%d does not show %s at its head yet; the next run sees the outcome", n, short(head)))
}
