package distribute

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// deepenMargin is how much older than the oldest commit to prove the
// default branch's history is fetched: its date less a day.
const deepenMargin = 24 * time.Hour

// branchReads is what step 6 learned about a target's sync branches.
type branchReads struct {
	// branches are the classified branches by name (the sync branch and the
	// aliases read); hubCommits the Touchmark-Hub-Commit trailer of each
	// one's Hc, which content_commit names while a branch holds C.
	branches   map[string]decide.Branch
	hubCommits map[string]string
	// differ tells whether tree(E) (tree(H) for a foreign or edited branch)
	// and tree(B) differ under .github/workflows for the branch a push would
	// move: GitHub asks for the Workflows permission to move a branch across
	// such changes. It is read only where the platform guards workflows
	// (Caps.WorkflowPerm).
	differ bool
}

// aliases returns the classified branches other than sync, by name.
func (b branchReads) aliases(sync string) map[string]decide.Branch {
	out := map[string]decide.Branch{}
	for name, br := range b.branches {
		if name != sync {
			out[name] = br
		}
	}
	return out
}

// branchRead is one sync branch as step 6 reads it.
type branchRead struct {
	hist decide.BranchHistory
	// at is the index of Hc in hist.Commits, -1 when there is none; trailers
	// are its trailers.
	at       int
	trailers decide.Trailers
	// legacy is set when the one-off migration rule holds for a branch
	// without Hc (adopt_unmarked).
	legacy bool
}

// readBranches runs step 6 for the sync branches names (the sync branch
// first): each is fetched (depth decide.MaxHistory+1, after the base, which
// the snapshot fetched) and its first-parent chain read at once, since a
// later fetch may cut the shallow history; then the ancestry of Hc's first
// parent and of the second parents of the merges after Hc is proven
// against B (DeepenSince, then IsAncestor; what cannot be proven is
// TriUnknown), the branches are classified, and with adopt the one-off
// migration rule is applied to foreign ones. moving is the branch a push
// would move, whose workflows difference is read.
//
// An error is one of the target: a fetch or a read of the branch failed, or
// the fetch of the default branch's history for the proofs failed in
// transport (the network, the credential, the rate: gitx.ClassifyFailure
// names a class, or the command timed out) after three attempts; the
// target then gets no decision and writes nothing. A proof that fails
// otherwise is not an error: a fetch that went through but cannot prove (a
// server without deepen-since, history that does not reach), an unknown
// commit date or an ancestry git cannot tell leave the branch edited, with a
// warning.
func (r *run) readBranches(ctx context.Context, w *Work, names []string, adopt bool, moving string) (branchReads, error) {
	reads := branchReads{branches: map[string]decide.Branch{}, hubCommits: map[string]string{}}
	if len(names) == 0 {
		return reads, nil
	}
	var hs []*branchRead
	for _, name := range names {
		h, err := r.readBranch(ctx, w, name)
		if err != nil {
			return reads, fmt.Errorf("branch %s: %w", name, err)
		}
		hs = append(hs, h)
	}
	if err := r.prove(ctx, w, hs, adopt); err != nil {
		return reads, fmt.Errorf("the history of %s: %w", w.DefaultBranch, err)
	}
	for _, h := range hs {
		b := decide.ClassifyBranch(h.hist, r.fps, decide.StreamSync)
		if adopt && b.State == decide.BranchForeign {
			b.LegacyRewritable = h.legacy
		}
		reads.branches[b.Name] = b
		if h.at >= 0 {
			reads.hubCommits[b.Name] = h.trailers.HubCommit
		}
	}
	if w.t.prov.caps.WorkflowPerm {
		reads.differ = r.workflowsDiffer(ctx, w, reads.branches[moving])
	}
	return reads, nil
}

// readBranch fetches one sync branch and reads its first-parent chain:
// the commits, the pairs Hc brings relative to its first parent, and which
// merges after Hc are clean.
func (r *run) readBranch(ctx context.Context, w *Work, name string) (*branchRead, error) {
	h := &branchRead{hist: decide.BranchHistory{Name: name, Base: w.B}, at: -1}
	var head string
	var ok bool
	err := r.gitRetry(ctx, w.t.prov, func() error {
		var err error
		head, ok, err = w.Repo.FetchBranch(ctx, name, decide.MaxHistory+1)
		return gitFailure(err)
	})
	if err != nil || !ok {
		return h, err
	}
	h.hist.Exists, h.hist.Head = true, head
	log, shallow, err := w.Repo.FirstParentLog(ctx, head, decide.MaxHistory)
	if err != nil {
		return nil, err
	}
	commits := make([]decide.HistoryCommit, len(log))
	for i, c := range log {
		commits[i] = decide.HistoryCommit{SHA: c.SHA, Parents: c.Parents, Message: c.Message}
	}
	h.at, h.trailers = decide.FindHc(commits, r.fps, decide.StreamSync)
	h.hist.Truncated = h.at < 0 && shallow
	if h.at >= 0 {
		hc := &commits[h.at]
		parent := ""
		if len(hc.Parents) > 0 {
			parent = hc.Parents[0]
		}
		diff, err := w.Repo.DiffTree(ctx, parent, hc.SHA)
		if err != nil {
			return nil, err
		}
		hc.Pairs = pairsOf(diff)
		for i := range commits[:h.at] {
			if len(commits[i].Parents) == 2 {
				clean, err := w.Repo.IsCleanMerge(ctx, commits[i].SHA)
				commits[i].CleanMerge = err == nil && clean
			}
		}
	}
	h.hist.Commits = commits
	return h, nil
}

// pairsOf converts the entries of a tree diff to pairs: From is the old
// blob or ZeroOID, Mode the new mode or ModeDelete, To the new blob or
// ZeroOID.
func pairsOf(diff []gitx.DiffEntry) []decide.Pair {
	out := make([]decide.Pair, 0, len(diff))
	for _, e := range diff {
		p := decide.Pair{Path: e.Path, From: e.OldOID, Mode: e.NewMode, To: e.NewOID}
		if e.OldMode == decide.ModeDelete {
			p.From = decide.ZeroOID
		}
		if e.NewMode == decide.ModeDelete {
			p.Mode, p.To = decide.ModeDelete, decide.ZeroOID
		}
		out = append(out, p)
	}
	return out
}

// prove fills in the ancestry the branches of hs need against B: Hc's first
// parent (ParentBaseAncestor) and the second parent of each clean merge
// after Hc (BaseAncestor); a commit that is B needs no proof. The default
// branch is fetched back to a day before the oldest of them (with adopt,
// also before the oldest commit read of a branch without Hc, for its
// merge base), then each is checked with IsAncestor. Anything that cannot
// be proven stays TriUnknown, with a warning. With adopt, the one-off
// migration rule is applied to the branches without Hc. An error is a
// transport failure of the fetch (see readBranches): nothing is proven or
// refuted then.
func (r *run) prove(ctx context.Context, w *Work, hs []*branchRead, adopt bool) error {
	need := map[string]bool{}
	var legacy []*branchRead
	for _, h := range hs {
		c := h.hist.Commits
		switch {
		case h.at < 0 && adopt && h.hist.Exists && len(c) > 0:
			legacy = append(legacy, h)
		case h.at >= 0:
			if hc := c[h.at]; len(hc.Parents) == 1 && hc.Parents[0] != w.B {
				need[hc.Parents[0]] = true
			}
			for _, m := range c[:h.at] {
				if len(m.Parents) == 2 && m.CleanMerge && m.Parents[1] != w.B {
					need[m.Parents[1]] = true
				}
			}
		}
	}
	proven := map[string]decide.Tri{}
	if len(need) > 0 || len(legacy) > 0 {
		dates := slices.Sorted(maps.Keys(need))
		for _, h := range legacy {
			dates = append(dates, h.hist.Commits[len(h.hist.Commits)-1].SHA)
		}
		deepened, err := r.deepen(ctx, w, dates)
		if err != nil {
			return err
		}
		for _, sha := range slices.Sorted(maps.Keys(need)) {
			proven[sha] = r.ancestor(ctx, w, sha, deepened)
		}
		for _, h := range legacy {
			h.legacy = deepened && r.legacyRewritable(ctx, w, h.hist.Head)
		}
	}
	for _, h := range hs {
		if h.at < 0 {
			continue
		}
		c := h.hist.Commits
		if hc := &c[h.at]; len(hc.Parents) == 1 {
			hc.ParentBaseAncestor = triOf(hc.Parents[0], w.B, proven)
		}
		for i := range c[:h.at] {
			if len(c[i].Parents) == 2 {
				c[i].BaseAncestor = triOf(c[i].Parents[1], w.B, proven)
			}
		}
	}
	return nil
}

// triOf is what the proofs say about sha: yes when it is b.
func triOf(sha, b string, proven map[string]decide.Tri) decide.Tri {
	if sha == b {
		return decide.TriYes
	}
	return proven[sha]
}

// deepen fetches the default branch's history back to a day before the
// oldest of the commits shas (DeepenSince) and reports whether it did. A
// fetch that failed in transport (transportFailure), after three attempts,
// is an error; one that failed otherwise, and an unknown date, prove
// nothing (false, with a warning).
func (r *run) deepen(ctx context.Context, w *Work, shas []string) (bool, error) {
	var oldest time.Time
	for _, sha := range shas {
		c, err := w.Repo.Commit(ctx, sha)
		if err != nil && ctx.Err() != nil {
			return false, err
		}
		if err != nil || c.Time.IsZero() {
			r.proofWarning(w, fmt.Sprintf("the date of %s is unknown (%v)", short(sha), err))
			return false, nil
		}
		if oldest.IsZero() || c.Time.Before(oldest) {
			oldest = c.Time
		}
	}
	if oldest.IsZero() {
		return false, nil
	}
	since := oldest.Add(-deepenMargin)
	err := r.gitRetry(ctx, w.t.prov, func() error { return gitFailure(w.Repo.DeepenSince(ctx, w.DefaultBranch, since)) })
	switch {
	case err == nil:
		return true, nil
	case transportFailure(ctx, err):
		return false, fmt.Errorf("fetch it back to %s: %w", since.UTC().Format(time.DateOnly), err)
	}
	r.proofWarning(w, fmt.Sprintf("the history of %s could not be fetched back to %s: %v", w.DefaultBranch, since.UTC().Format(time.DateOnly), err))
	return false, nil
}

// transportFailure reports whether err, a failed fetch, says nothing about
// the history it asked for: the run ended (ctx), or the transport failed (a
// network failure or the command's own timeout, a refused credential or
// identity, a rate limit: the classes gitFailure gives).
func transportFailure(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		platform.ClassOf(err) != platform.ClassUnknown
}

// ancestor proves whether sha is an ancestor of B, after deepened history.
func (r *run) ancestor(ctx context.Context, w *Work, sha string, deepened bool) decide.Tri {
	if !deepened {
		return decide.TriUnknown
	}
	ok, err := w.Repo.IsAncestor(ctx, sha, w.B)
	switch {
	case err != nil:
		r.proofWarning(w, fmt.Sprintf("whether %s is on %s is unknown: %v", short(sha), w.DefaultBranch, err))
		return decide.TriUnknown
	case ok:
		return decide.TriYes
	}
	return decide.TriNo
}

// ancestryAnswer is one answer of Write.HubIsAncestor about a hub commit.
type ancestryAnswer struct {
	descends bool
	err      error
}

// supersededOn applies guard I8 to one target: when Hc of a sync branch read
// names, in its Touchmark-Hub-Commit trailer, a hub commit that descends
// from this run's HubCommit (and is not it), a newer run of the hub wrote
// the branch, and this one must not undo it: it returns why ("" when no
// branch says so). The branches are asked in the order of their names. A hub
// commit the hub's clone does not know does not block (Write.HubIsAncestor
// answers false); an error of HubIsAncestor is returned, and the target
// fails rather than risk writing old content.
func (r *run) supersededOn(ctx context.Context, reads branchReads) (string, error) {
	if r.d.Write.HubIsAncestor == nil {
		return "", nil
	}
	for _, name := range keysInOrder(reads.hubCommits) {
		hc := reads.hubCommits[name]
		if !isCommitID(hc) || strings.EqualFold(hc, r.d.HubCommit) {
			continue
		}
		descends, err := r.hubDescends(ctx, strings.ToLower(hc))
		if err != nil {
			return "", fmt.Errorf("is hub commit %s newer than %s: %w", short(hc), short(r.d.HubCommit), err)
		}
		if descends {
			return fmt.Sprintf("superseded: branch %s carries touchmark's commit for hub commit %s, which is newer than this run's %s; "+
				"the run of the newer commit keeps the target", name, short(hc), short(r.d.HubCommit)), nil
		}
	}
	return "", nil
}

// hubDescends reports whether hub commit c descends from this run's
// HubCommit (Write.HubIsAncestor), once per commit and run.
func (r *run) hubDescends(ctx context.Context, c string) (bool, error) {
	r.mu.Lock()
	a, ok := r.ancestry[c]
	r.mu.Unlock()
	if ok {
		return a.descends, a.err
	}
	descends, err := r.d.Write.HubIsAncestor(ctx, r.d.HubCommit, c)
	if ctx.Err() == nil {
		r.mu.Lock()
		r.ancestry[c] = ancestryAnswer{descends: descends, err: err}
		r.mu.Unlock()
	}
	return descends, err
}

// isCommitID reports whether s is a full hex commit id (SHA-1 or SHA-256),
// in either case: a trailer is text from a target.
func isCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// proofWarning adds a warning about a proof of ancestry that failed: the
// branch then counts as edited.
func (r *run) proofWarning(w *Work, what string) {
	w.t.res.Warnings = append(w.t.res.Warnings, "the sync branch's history: "+what+"; touchmark leaves the branch as it is")
}

// legacyRewritable applies the one-off migration rule
// (decide.LegacyRewritable) to the branch whose head is head: every path of
// diff(merge-base(B, head), head) holds a version the hub ever shipped, or
// deletes one. A branch whose merge base with B is not in the fetched
// history fails it.
func (r *run) legacyRewritable(ctx context.Context, w *Work, head string) bool {
	out, err := w.Repo.Git.Run(ctx, nil, "merge-base", w.B, head)
	if err != nil {
		return false
	}
	base := strings.TrimSpace(string(out))
	diff, err := w.Repo.DiffTree(ctx, base, head)
	if err != nil {
		return false
	}
	return decide.LegacyRewritable(pairsOf(diff), r.d.Manifest)
}

// workflowsDiffer reports whether moving b to a commit on B carries changes
// under .github/workflows that are not the push's own: tree(E) (tree(H) for
// a foreign or edited branch) and tree(B) differ there. An absent branch
// moves nothing. When the trees cannot be compared, it says yes: the
// permission is asked for when in doubt.
func (r *run) workflowsDiffer(ctx context.Context, w *Work, b decide.Branch) bool {
	from := ""
	switch b.State {
	case decide.BranchRewritable:
		from = b.E
	case decide.BranchForeign, decide.BranchEdited:
		from = b.Head
	}
	if from == "" || from == w.B {
		return false
	}
	diff, err := w.Repo.DiffTree(ctx, from, w.B)
	if err != nil {
		return !errors.Is(err, context.Canceled)
	}
	return slices.ContainsFunc(diff, func(e gitx.DiffEntry) bool {
		return pathx.Under(pathx.Fold(e.Path), decide.WorkflowsDir)
	})
}
