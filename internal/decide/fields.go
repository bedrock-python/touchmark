package decide

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// DesiredPR is what touchmark wants an own open PR to show.
type DesiredPR struct {
	Title string
	// Body is the human part, without the marker.
	Body   string
	Labels []string
	// Base is the base to set: Step.Base of the StepEditPR, the target's
	// default branch when the decision rebuilt the branch on it, "" to keep
	// the PR's base. A base moved without a rebuild would show the commits
	// between the two branches.
	Base string
	// DraftPrefix is Caps.DraftPrefix on platforms that mark drafts with a
	// title prefix ("Draft: " on GitLab, "WIP: " on Gitea and Forgejo), ""
	// elsewhere: the title of a draft is compared without it (PlainTitle).
	DraftPrefix string
}

// PlainTitle returns pr's title without the draft prefix the driver puts
// before the title of a draft on a title-prefix platform (prefix, compared
// ignoring case and the blanks around it); the title as it is otherwise.
// touchmark's own drafts carry exactly that prefix, so their plain title
// is the title touchmark set.
func PlainTitle(pr platform.PR, prefix string) string {
	p := strings.TrimSpace(prefix)
	if !pr.Draft || p == "" || len(pr.Title) < len(p) || !strings.EqualFold(pr.Title[:len(p)], p) {
		return pr.Title
	}
	return strings.TrimLeft(pr.Title[len(p):], " \t")
}

// BodyHash returns "sha256:<64 hex>" of marker.Strip(body): the value kept
// in marker.Data.Body.
//
// Strip drops every marker line and the trailing whitespace, so the hash of
// a body with its marker equals the hash of its human part alone. The hash
// is always taken of a body touchmark renders, never of one read back from
// the platform (which may store CRLF): a person's edit never makes the
// desired body differ, so a rerun on the same inputs writes nothing.
func BodyHash(body string) string {
	sum := sha256.Sum256([]byte(marker.Strip(body)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PlanPREdit applies field ownership (see docs/concepts/delivery.md) to an
// own open PR:
//   - body: written when BodyHash(want.Body) differs from m.Data.Body;
//   - title: written only when want.Title differs from m.Data.TitleSet AND
//     the current title still equals m.Data.TitleSet (a person's title is
//     theirs);
//   - labels: only labels of want.Labels not in m.Data.LabelsSet are added;
//     labels are never removed and people's removals are not undone;
//   - base: written when want.Base is set and differs from pr.Base
//     (base-renamed; want.Base is Step.Base, set only after a rebuild);
//   - draft: never.
//
// It returns the edit (nil fields for no change), the marker data to write
// with it (Body, TitleSet and LabelsSet updated), and whether anything
// changes. Engine, DecidedAt and Base in the marker are not compared: a new
// engine version alone never rewrites bodies.
//
// Details:
//   - Whenever anything changes, edit.Body is set to want.Body, the human
//     part, even when only the title, labels or base change: the marker is
//     the body's last line and carries TitleSet and LabelsSet, so it is
//     rewritten with them. The caller appends the marker line encoded from
//     next (updating Engine, DecidedAt and Base there if it likes).
//   - A title is never blanked: an empty want.Title writes nothing. Titles
//     compare exactly, after the draft prefix of want.DraftPrefix is taken
//     off a draft's title (PlainTitle): TitleSet and want.Title are plain
//     titles, so a new pr.title reaches touchmark's own drafts too, and the
//     driver keeps the prefix, so the draft state stays. A draft a person
//     retitled is left alone.
//   - Labels are added in want.Labels order, without repeats or empty
//     names, and next.LabelsSet grows by them: once set, a label is never
//     added again, so a person's removal stays.
//   - An empty want.Base writes no base.
//   - The marker of an open PR carries no closed, ack or revoked: those
//     describe a closed PR, and are left over when a person reopened one
//     touchmark closed or remembered as declined. next clears them, which
//     is a change: a later close must be classified afresh (a stale
//     closed.by would hide a person's decline, a stale ack would date it
//     from an old opt-in file). recreate_for is the caller's.
//   - The marker's key and content fields are not PlanPREdit's: a
//     StepEditPR with Content set (after a push, or when the marker's key
//     is stale) rewrites them whatever this reports, and the caller then
//     writes the body even when changed is false.
//
// next shares nothing with m.Data: the caller may change it freely. With no
// change, edit is zero and next equals m.Data.
func PlanPREdit(pr platform.PR, m marker.Marker, want DesiredPR) (edit platform.PREdit, next marker.Data, changed bool) {
	next = fieldsCloneData(m.Data)
	if next.Closed != nil || next.Ack || next.Revoked {
		next.Closed, next.Ack, next.Revoked = nil, false, false
		changed = true
	}
	if want.Title != "" && want.Title != m.Data.TitleSet && PlainTitle(pr, want.DraftPrefix) == m.Data.TitleSet {
		title := want.Title
		edit.Title = &title
		next.TitleSet = title
		changed = true
	}
	set := make(map[string]bool, len(m.Data.LabelsSet))
	for _, l := range m.Data.LabelsSet {
		set[l] = true
	}
	for _, l := range want.Labels {
		if l == "" || set[l] {
			continue
		}
		set[l] = true
		edit.AddLabels = append(edit.AddLabels, l)
		next.LabelsSet = append(next.LabelsSet, l)
		changed = true
	}
	if want.Base != "" && pr.Base != want.Base {
		base := want.Base
		edit.Base = &base
		changed = true
	}
	if hash := BodyHash(want.Body); changed || hash != m.Data.Body {
		body := want.Body
		edit.Body = &body
		next.Body = hash
		changed = true
	}
	return edit, next, changed
}

// fieldsCloneData returns a copy of d that shares no slice or pointer
// with it.
func fieldsCloneData(d marker.Data) marker.Data {
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
