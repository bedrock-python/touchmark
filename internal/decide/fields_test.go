package decide

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The PR fields of the field-ownership tests.
const (
	fldTitle = "chore: sync engineering assets"
	fldLabel = "engineering-assets"
	fldBody  = "## Engineering assets\n\nThe hub proposes 2 files."
)

// fldSum is the expected form of BodyHash for a string already stripped.
func fldSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fldEncode returns the marker line for m.
func fldEncode(t *testing.T, m marker.Marker) string {
	t.Helper()
	line, err := marker.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// fldState returns an own open PR as touchmark last wrote it, its marker,
// and the PR touchmark wants: nothing to change.
func fldState(t *testing.T) (platform.PR, marker.Marker, DesiredPR) {
	t.Helper()
	d := []Pair{memCreate(memA, memV1), memCreate(memB, memV1)}
	recreateFor := memX // a recreate consumed before a push
	m := marker.Marker{
		Key: Key(StreamSync, d),
		Data: marker.Data{
			V: marker.Version, Stream: StreamSync, Hub: "acme-eng", FP: memFP,
			DecidedAt: memX, ContentCommit: memX, Base: memX, OptIn: memH1, Engine: "0.2.0",
			Packs: []string{"agents"}, Changes: ShortChanges(d), ChangesComplete: true,
			TitleSet: fldTitle, Body: BodyHash(fldBody), LabelsSet: []string{fldLabel},
			RecreateFor: &recreateFor,
		},
	}
	line := fldEncode(t, m)
	parsed, err := marker.Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	pr := platform.PR{
		Number: 7, State: platform.Open, Head: "touchmark/acme-eng", Base: "main",
		RepoID: "100", HeadRepoID: "100", BaseExists: true,
		Title: fldTitle, Body: fldBody + "\n\n" + line, Labels: []string{fldLabel},
		Author: memWriter,
	}
	return pr, parsed, DesiredPR{Title: fldTitle, Body: fldBody, Labels: []string{fldLabel}, Base: "main"}
}

// fldApply returns pr and its marker after the platform applied edit with
// the marker data next, as the caller of PlanPREdit writes them.
func fldApply(t *testing.T, pr platform.PR, m marker.Marker, edit platform.PREdit, next marker.Data) (platform.PR, marker.Marker) {
	t.Helper()
	if edit.Title != nil {
		pr.Title = *edit.Title
	}
	if edit.Base != nil {
		pr.Base = *edit.Base
	}
	for _, l := range edit.AddLabels {
		if !slices.Contains(pr.Labels, l) {
			pr.Labels = append(pr.Labels, l)
		}
	}
	if edit.Body != nil {
		line := fldEncode(t, marker.Marker{Key: m.Key, Data: next})
		pr.Body = *edit.Body + "\n\n" + line
		parsed, err := marker.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		m = parsed
	}
	return pr, m
}

func TestFieldsBodyHash(t *testing.T) {
	if got := BodyHash(""); got != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("BodyHash(\"\") = %s, want the sha256 of nothing", got)
	}
	_, m, _ := fldState(t)
	line := fldEncode(t, m)
	cases := []struct {
		name, body, want string
	}{
		{"text", fldBody, fldSum(fldBody)},
		{"trailing whitespace", fldBody + " \n\n\t\r\n", fldSum(fldBody)},
		{"the marker is left out", fldBody + "\n\n" + line, fldSum(fldBody)},
		{"a marker line with CRLF is left out", fldBody + "\r\n\r\n" + line + "\r\n", fldSum(fldBody)},
		{"a marker of another version is left out", fldBody + "\n<!-- touchmark:v2 anything -->", fldSum(fldBody)},
		{"a marker inside a line stays", "see <!-- touchmark:v1 x -->", fldSum("see <!-- touchmark:v1 x -->")},
		{"a control stays", "- [ ] <!-- touchmark:recreate --> Rebuild", fldSum("- [ ] <!-- touchmark:recreate --> Rebuild")},
		{"a carriage return inside stays", "a\r\nb", fldSum("a\r\nb")},
	}
	for _, c := range cases {
		if got := BodyHash(c.body); got != c.want {
			t.Errorf("%s: BodyHash = %s, want %s", c.name, got, c.want)
		}
	}
	if BodyHash(fldBody) == BodyHash(fldBody+"\nmore") {
		t.Error("BodyHash does not see a new line of text")
	}
}

func TestFieldsPlanPREdit(t *testing.T) {
	type change func(pr *platform.PR, d *marker.Data, w *DesiredPR)
	cases := []struct {
		name   string
		change change
		// The expected edit: title and base written ("" for none), labels
		// added, whether the body is written.
		title, base string
		add         []string
		body        bool
		// The expected marker fields.
		titleSet  string
		labelsSet []string
	}{
		{
			name:     "nothing changed",
			change:   func(*platform.PR, *marker.Data, *DesiredPR) {},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name: "people edited the title and body, marked it ready, removed the label",
			change: func(pr *platform.PR, _ *marker.Data, _ *DesiredPR) {
				pr.Title, pr.Draft, pr.Labels = "Sync, reviewed by the team", true, nil
				pr.Body = "- [x] <!-- touchmark:recreate --> Rebuild\n" + pr.Body
			},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name: "a new engine, another hub commit, another base commit",
			change: func(_ *platform.PR, d *marker.Data, _ *DesiredPR) {
				d.Engine, d.DecidedAt, d.Base = "0.1.0", memV1, memV2
			},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "the desired body changed",
			change:   func(_ *platform.PR, _ *marker.Data, w *DesiredPR) { w.Body = fldBody + "\n\nOne more file." },
			body:     true,
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "only trailing whitespace of the desired body changed",
			change:   func(_ *platform.PR, _ *marker.Data, w *DesiredPR) { w.Body = fldBody + "\n\n" },
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "a marker without a body hash",
			change:   func(_ *platform.PR, d *marker.Data, _ *DesiredPR) { d.Body = "" },
			body:     true,
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:   "pr.title changed in the hub, the title is still ours",
			change: func(_ *platform.PR, _ *marker.Data, w *DesiredPR) { w.Title = "chore: engineering assets" },
			title:  "chore: engineering assets", body: true,
			titleSet: "chore: engineering assets", labelsSet: []string{fldLabel},
		},
		{
			name: "pr.title changed in the hub, a person's title",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Title, w.Title = "Sync, reviewed by the team", "chore: engineering assets"
			},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			// A draft's prefixed title differs from the plain TitleSet: left
			// alone, so the draft state is too.
			name: "pr.title changed in the hub, a draft with a title prefix",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Title, pr.Draft, w.Title = "Draft: "+fldTitle, true, "chore: engineering assets"
			},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			// With the platform's draft prefix known, touchmark's own draft
			// is still its title: the new pr.title reaches it (the driver
			// keeps the prefix, so the draft state stays). Drafts once never
			// got a new pr.title.
			name: "pr.title changed in the hub, touchmark's own draft",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Title, pr.Draft, w.Title, w.DraftPrefix = "draft:  "+fldTitle, true, "chore: engineering assets", "Draft: "
			},
			title: "chore: engineering assets", body: true,
			titleSet: "chore: engineering assets", labelsSet: []string{fldLabel},
		},
		{
			name: "pr.title changed in the hub, a draft a person retitled",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Title, pr.Draft, w.Title, w.DraftPrefix = "Draft: Sync, reviewed", true, "chore: engineering assets", "Draft: "
			},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name: "pr.title changed in the hub, a ready title that starts like a prefix",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Title, pr.Draft, w.Title, w.DraftPrefix = "WIP: "+fldTitle, false, "chore: engineering assets", "WIP: "
			},
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "no desired title",
			change:   func(_ *platform.PR, _ *marker.Data, w *DesiredPR) { w.Title = "" },
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:   "a label added in the hub",
			change: func(_ *platform.PR, _ *marker.Data, w *DesiredPR) { w.Labels = []string{fldLabel, "sync"} },
			add:    []string{"sync"}, body: true,
			titleSet: fldTitle, labelsSet: []string{fldLabel, "sync"},
		},
		{
			name: "a label added in the hub that a person already added",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Labels, w.Labels = []string{fldLabel, "sync"}, []string{fldLabel, "sync"}
			},
			add: []string{"sync"}, body: true,
			titleSet: fldTitle, labelsSet: []string{fldLabel, "sync"},
		},
		{
			name:     "a label dropped in the hub is not removed",
			change:   func(_ *platform.PR, _ *marker.Data, w *DesiredPR) { w.Labels = nil },
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name: "repeated and empty labels",
			change: func(_ *platform.PR, _ *marker.Data, w *DesiredPR) {
				w.Labels = []string{"", "b", "b", fldLabel, "a"}
			},
			add: []string{"b", "a"}, body: true,
			titleSet: fldTitle, labelsSet: []string{fldLabel, "b", "a"},
		},
		{
			name:   "the default branch was renamed",
			change: func(pr *platform.PR, _ *marker.Data, _ *DesiredPR) { pr.Base = "master" },
			base:   "main", body: true,
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "no desired base",
			change:   func(pr *platform.PR, _ *marker.Data, w *DesiredPR) { pr.Base, w.Base = "master", "" },
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name: "reopened after touchmark closed it",
			change: func(_ *platform.PR, d *marker.Data, _ *DesiredPR) {
				d.Closed = &marker.Closed{By: "touchmark", Reason: ReasonNoDiff}
			},
			body:     true,
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "reopened after its decline was acked",
			change:   func(_ *platform.PR, d *marker.Data, _ *DesiredPR) { d.Ack, d.OptIn = true, memH2 },
			body:     true,
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name:     "reopened after its decline was revoked",
			change:   func(_ *platform.PR, d *marker.Data, _ *DesiredPR) { d.Ack, d.Revoked = true, true },
			body:     true,
			titleSet: fldTitle, labelsSet: []string{fldLabel},
		},
		{
			name: "everything at once",
			change: func(pr *platform.PR, _ *marker.Data, w *DesiredPR) {
				pr.Base = "master"
				w.Title, w.Body, w.Labels = "chore: engineering assets", "new body", []string{"sync", fldLabel}
			},
			title: "chore: engineering assets", base: "main", add: []string{"sync"}, body: true,
			titleSet: "chore: engineering assets", labelsSet: []string{fldLabel, "sync"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr, m, want := fldState(t)
			c.change(&pr, &m.Data, &want)
			before := fldClone(m.Data)
			edit, next, changed := PlanPREdit(pr, m, want)

			wantChanged := c.title != "" || c.base != "" || len(c.add) > 0 || c.body
			if changed != wantChanged {
				t.Errorf("changed = %v, want %v", changed, wantChanged)
			}
			if got := fldString(edit.Title); got != c.title {
				t.Errorf("title = %q, want %q", got, c.title)
			}
			if got := fldString(edit.Base); got != c.base {
				t.Errorf("base = %q, want %q", got, c.base)
			}
			if !slices.Equal(edit.AddLabels, c.add) {
				t.Errorf("labels added = %q, want %q", edit.AddLabels, c.add)
			}
			if (edit.Body != nil) != c.body {
				t.Errorf("body written = %v, want %v", edit.Body != nil, c.body)
			} else if c.body && *edit.Body != want.Body {
				t.Errorf("body = %q, want the desired human part %q", *edit.Body, want.Body)
			}
			if edit.State != nil {
				t.Error("PlanPREdit changes the state")
			}
			// The marker data: the three owned fields, no closed-PR fields,
			// the rest untouched.
			wantNext := fldClone(before)
			wantNext.TitleSet, wantNext.LabelsSet = c.titleSet, c.labelsSet
			wantNext.Closed, wantNext.Ack, wantNext.Revoked = nil, false, false
			if c.body {
				wantNext.Body = BodyHash(want.Body)
			}
			if !reflect.DeepEqual(next, wantNext) {
				t.Errorf("next =\n%+v\nwant\n%+v", next, wantNext)
			}
			if !reflect.DeepEqual(m.Data, before) {
				t.Error("PlanPREdit modified the marker it was given")
			}
			if !changed && !reflect.DeepEqual(edit, platform.PREdit{}) {
				t.Errorf("no change, yet an edit %+v", edit)
			}
			// Invariant I7 (see package distribute): once written, the same
			// inputs change nothing.
			pr2, m2 := fldApply(t, pr, m, edit, next)
			if edit2, _, changed2 := PlanPREdit(pr2, m2, want); changed2 {
				t.Errorf("after the write PlanPREdit plans %+v again", edit2)
			}
		})
	}
}

// TestFieldsPlanPREditOwnsNext: next shares no slice or pointer with the
// marker data it came from.
func TestFieldsPlanPREditOwnsNext(t *testing.T) {
	sample := func() marker.Data {
		r := memX
		return marker.Data{Packs: []string{"a"}, Closed: &marker.Closed{By: "touchmark", Reason: ReasonNoDiff}, RecreateFor: &r}
	}
	for _, w := range []func(*DesiredPR){func(*DesiredPR) {}, func(w *DesiredPR) { w.Body = "new" }} {
		pr, m, want := fldState(t)
		w(&want)
		before := fldClone(m.Data)
		_, next, _ := PlanPREdit(pr, m, want)
		next.Packs[0], next.Changes[0].Path, next.LabelsSet[0] = "x", "x", "x"
		next.LabelsSet = append(next.LabelsSet, "y")
		*next.RecreateFor = "x"
		if !reflect.DeepEqual(m.Data, before) {
			t.Fatal("changing next changed the marker data")
		}
	}
	// A marker without the optional parts.
	pr, m, want := fldState(t)
	m.Data.RecreateFor, m.Data.Packs, m.Data.LabelsSet, want.Labels = nil, nil, nil, nil
	_, next, _ := PlanPREdit(pr, m, want)
	if next.Closed != nil || next.RecreateFor != nil || next.Packs != nil || next.LabelsSet != nil {
		t.Errorf("next = %+v, want the absent parts absent", next)
	}
	// The deep copy itself.
	d := sample()
	c := fieldsCloneData(d)
	c.Closed.Reason, *c.RecreateFor, c.Packs[0] = "x", "x", "x"
	if !reflect.DeepEqual(d, sample()) {
		t.Error("fieldsCloneData shares the pointers")
	}
}

func fldString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fldClone is a deep copy of d for comparisons.
func fldClone(d marker.Data) marker.Data {
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
