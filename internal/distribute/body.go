package distribute

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// Directories of CI workflows (on Gitea and Forgejo a new .gitea/workflows
// or .forgejo/workflows turns off .github/workflows).
var giteaWorkflowDirs = []string{".gitea/workflows", ".forgejo/workflows"}

// content describes what the branch of a decision holds once the decision
// is carried out: D when the decision pushes it, when the branch it writes
// to or blocks on has no commit of touchmark's, or when that commit brings
// D already; else that branch's C. It returns the pairs and the hub commit
// that decided them: the run's for D it pushes or that no commit carries
// yet, the Touchmark-Hub-Commit of Hc for what Hc brings (C, or D when C is
// D). So a hub commit that changes nothing for the target changes nothing
// in its description either.
func (r *run) content(w *Work, reads branchReads) (pairs []decide.Pair, hubCommit string) {
	b, ok := reads.branches[w.Decision.Branch]
	switch {
	case pushes(w.Decision) || !ok || b.Hc == "" || b.CKey == "":
		return w.D, r.d.HubCommit
	case decide.PairsEqual(b.C, w.D):
		return w.D, cmp.Or(reads.hubCommits[b.Name], r.d.HubCommit)
	}
	return b.C, reads.hubCommits[b.Name]
}

// fillBody fills the body input (Work.Body: the rows of what the branch
// holds, and while paused what a rebuild would bring, D − C; the local
// files; the hub's name, link and sensitive paths; the target's packs and
// platform) and the marker data of D (Work.Marker). A paused branch without
// a commit of touchmark's (its Hc is gone: foreign) has no C: the body
// claims nothing about what it holds, shows no table, and lists all of D as
// what a rebuild would bring.
func (r *run) fillBody(w *Work, sel config.Selection, reads branchReads) {
	t := w.t
	pairs, hubCommit := r.content(w, reads)
	packs := map[string]string{}
	for _, e := range w.Plan.Entries {
		packs[e.Path] = e.Pack
	}
	var local []string
	for _, e := range w.Plan.Entries {
		if e.State == decide.Local {
			local = append(local, e.Path)
		}
	}
	rows := rowsOf(pairs, packs)
	var pending []prbody.Change
	unknown := false
	if w.Decision.Blocks.Paused {
		b, ok := reads.branches[w.Decision.Branch]
		if unknown = !ok || b.Hc == "" || b.CKey == ""; unknown {
			rows, pending = nil, rowsOf(w.D, packs)
		} else {
			pending = rowsOf(minus(w.D, pairs), packs)
		}
	}
	w.Body = prbody.Input{
		Intro:          r.d.Write.Intro,
		HubName:        r.hub.ID,
		HubURL:         r.hubURL(t),
		ContentCommit:  hubCommit,
		Packs:          slices.Clone(sel.Packs),
		Changes:        rows,
		Pending:        pending,
		BranchUnknown:  unknown,
		Local:          local,
		Sensitive:      slices.Clone(r.hub.SensitivePaths),
		OptInFile:      r.optIn,
		GiteaWorkflows: giteaWorkflows(t.prov.cfg.Type, w.Tree, append(slices.Clone(rows), pending...)),
		Caps:           t.prov.caps,
	}
	if w.Body.GiteaWorkflows && pushes(w.Decision) {
		// plan says so too, not only the pull request's body.
		r.targetWarning(t, giteaWorkflowsWarning)
	}
	w.Marker = marker.Data{
		V:               marker.Version,
		Stream:          decide.StreamSync,
		Hub:             r.hub.ID,
		FP:              r.fps[0],
		HubRepo:         r.hubRepo(t),
		DecidedAt:       r.d.HubCommit,
		ContentCommit:   r.d.HubCommit,
		Base:            w.B,
		OptIn:           w.OptInHash,
		Engine:          r.d.Engine,
		Packs:           slices.Clone(sel.Packs),
		Changes:         decide.ShortChanges(w.D),
		ChangesComplete: true,
	}
}

// rowsOf returns the table rows of pairs, with the pack of each path
// (packs; "" when unknown).
func rowsOf(pairs []decide.Pair, packs map[string]string) []prbody.Change {
	out := make([]prbody.Change, 0, len(pairs))
	for _, p := range pairs {
		c := prbody.Change{Path: p.Path, Pack: packs[p.Path], Mode: p.Mode}
		switch {
		case p.Mode == decide.ModeDelete:
			c.Action, c.Mode = prbody.ActionDelete, ""
		case p.From == decide.ZeroOID:
			c.Action = prbody.ActionCreate
		case p.From == p.To:
			c.Action = prbody.ActionChmod
		default:
			c.Action = prbody.ActionUpdate
		}
		out = append(out, c)
	}
	return out
}

// minus returns the pairs of a that b does not hold.
func minus(a, b []decide.Pair) []decide.Pair {
	var out []decide.Pair
	for _, p := range a {
		if !slices.Contains(b, p) {
			out = append(out, p)
		}
	}
	return out
}

// giteaWorkflowsWarning is the target warning of a push that brings a
// Gitea or Forgejo target its first .gitea/workflows or .forgejo/workflows
// (giteaWorkflows).
const giteaWorkflowsWarning = "the change adds .gitea/workflows or .forgejo/workflows, which turns off the target's .github/workflows on Gitea and Forgejo"

// giteaWorkflows reports whether rows, on a Gitea or Forgejo target, create
// a .gitea/workflows or .forgejo/workflows directory the target does not
// have while it has .github/workflows, which the new directory turns off.
func giteaWorkflows(providerType string, tree *snapshot.Tree, rows []prbody.Change) bool {
	if providerType != "gitea" && providerType != "forgejo" {
		return false
	}
	under := func(p string, dirs ...string) bool {
		return slices.ContainsFunc(dirs, func(d string) bool { return pathx.Under(pathx.Fold(p), d) })
	}
	creates := slices.ContainsFunc(rows, func(c prbody.Change) bool {
		return c.Action == prbody.ActionCreate && under(c.Path, giteaWorkflowDirs...)
	})
	if !creates {
		return false
	}
	github := false
	for p := range tree.Entries {
		if under(p, giteaWorkflowDirs...) {
			return false
		}
		github = github || under(p, decide.WorkflowsDir)
	}
	return github
}

// hubVisible reports whether the body and marker of a pull request in t may
// name and link the hub (pr.link_hub): always with "always", never with
// "never", and with "auto" unless the hub is not public (or its visibility
// is unknown) while t is public.
func (r *run) hubVisible(t *target) bool {
	switch r.hub.PR.LinkHub {
	case "always":
		return true
	case "never":
		return false
	}
	return r.d.HubContext.Visibility == "public" || t.repo.Visibility != "public"
}

// hubURL is the hub's web URL for t's pull request body, "" when it must
// not be shown.
func (r *run) hubURL(t *target) string {
	if !r.hubVisible(t) {
		return ""
	}
	return r.d.Write.HubURL
}

// hubRepo is the marker's hub_repo for t ("github.com/acme/engineering-
// assets"), "" when the hub must not be named: from Write.HubURL, else from
// the CI context.
func (r *run) hubRepo(t *target) string {
	if !r.hubVisible(t) {
		return ""
	}
	if u, err := url.Parse(r.d.Write.HubURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Host) + strings.TrimSuffix(u.Path, "/")
	}
	if c := r.d.HubContext; c.Host != "" && c.RepoPath != "" {
		return c.Host + "/" + c.RepoPath
	}
	return ""
}

// humanBody renders the part of w's pull request description touchmark owns,
// without the marker: Work.Body with the blocks of the decision, and the
// recreate control while the branch is paused. Its hash is the marker's body
// (decide.BodyHash).
func (w *Work) humanBody() (string, error) {
	in := w.Body
	b := w.Decision.Blocks
	in.PreviouslyDeclined = slices.Clone(b.PreviouslyDeclined)
	in.Paused = b.Paused
	in.UpdateBranchNeeded = b.UpdateBranchNeeded
	in.NothingMore = b.NothingMore
	in.ShowRecreate = b.Paused
	in.ShowRepropose = false
	in.Marker = ""
	return prbody.Render(in)
}

// ownByNumber returns the open pull request n the decision edits: an own
// one, or a marker-invalid one operations adopted (with an empty marker).
func ownByNumber(prs prSet, n int64) (decide.OwnPR, bool) {
	for _, o := range prs.own {
		if o.PR.Number == n {
			return o, true
		}
	}
	for _, pr := range prs.invalid {
		if pr.Number == n {
			return decide.OwnPR{PR: pr}, true
		}
	}
	return decide.OwnPR{}, false
}

// draftPrefix is the title prefix of drafts on a title-prefix platform, ""
// on others.
func draftPrefix(c platform.Caps) string {
	if c.Draft != platform.DraftTitlePrefix {
		return ""
	}
	return c.DraftPrefix
}

// reportDecision writes w's decision into the target's report line: the
// outcome, reason and pull request, and for a branch touchmark leaves alone
// (blocked:edited, blocked:branch-taken) why (decide.Branch.Detail). It
// renders the description when the decision writes one, and applies field
// ownership (decide.PlanPREdit) to every StepEditPR: an edit that changes
// nothing and rewrites no content is skipped (Work.idle), and an unchanged
// target whose edit writes is reported as updated:title when only the title
// changes, else updated:body.
func (r *run) reportDecision(w *Work, prs prSet, reads branchReads) error {
	res := &w.t.res
	d := w.Decision
	res.Outcome, res.Reason, res.PR = report.Outcome(d.Outcome), d.Reason, nil
	if d.PR != 0 {
		if i := slices.IndexFunc(prs.all, func(pr platform.PR) bool { return pr.Number == d.PR }); i >= 0 {
			res.PR = prRef(prs.all[i])
		}
	}
	if b, ok := reads.branches[d.Branch]; ok && b.Detail != "" && (d.Reason == decide.ReasonEdited || d.Reason == decide.ReasonBranchTaken) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("branch %s: %s", b.Name, b.Detail))
	}
	if !d.Body {
		return nil
	}
	human, err := w.humanBody()
	if err != nil {
		return err
	}
	w.idle = map[int]bool{}
	changed := ""
	for i, s := range d.Steps {
		if s.Kind != decide.StepEditPR {
			continue
		}
		o, ok := ownByNumber(prs, s.PR)
		if !ok {
			return fmt.Errorf("the decision edits #%d, which is not an open pull request of touchmark", s.PR)
		}
		desired := decide.DesiredPR{Title: r.hub.PR.Title, Body: human, Labels: r.hub.PR.Labels, Base: s.Base,
			DraftPrefix: draftPrefix(w.t.prov.caps)}
		edit, _, writes := decide.PlanPREdit(o.PR, o.Marker, desired)
		if !writes && !s.Content {
			w.idle[i] = true
			continue
		}
		if changed != "body" {
			changed = "body"
			if titleOnly(edit, o.Marker, human) && !s.Content {
				changed = "title"
			}
		}
	}
	if d.Outcome == decide.OutcomeUnchanged && changed != "" {
		res.Outcome, res.Reason = report.OutcomeUpdated, changed
	}
	return nil
}

// titleOnly reports whether edit changes the title of a pull request whose
// marker is m and nothing else: its body, labels, base and marker state
// stay.
func titleOnly(edit platform.PREdit, m marker.Marker, human string) bool {
	d := m.Data
	return edit.Title != nil && edit.Base == nil && len(edit.AddLabels) == 0 &&
		decide.BodyHash(human) == d.Body && d.Closed == nil && !d.Ack && !d.Revoked
}
