package distribute

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// leakSecret is a secret the run's registry holds in the leak tests.
const leakSecret = "leak-5ecr3t-0123456789"

// Every text touchmark would send is checked for the run's secrets before
// the first write (a secret in a body, a comment, a commit or a branch name
// stops the target, failed:secret-exposure): each field of each kind of
// write, with the secret in every form the registry keeps. Texts without it
// pass.
func TestLeakEveryText(t *testing.T) {
	reg := redact.New()
	reg.Add(leakSecret, "x-access-token")
	x := &targetExec{r: &run{d: Deps{Write: WriteDeps{Redact: reg}}}, w: &Work{}}
	str := func(s string) *string { return &s }
	// Every form the registry keeps (raw, base64 of user:secret), inside
	// other text.
	forms := map[string]string{}
	for i, f := range reg.Forms() {
		forms["form "+strconv.Itoa(i)] = "x " + f + " y"
	}
	if len(forms) < 2 {
		t.Fatalf("the registry keeps forms %q, want the raw secret and its Basic form", reg.Forms())
	}
	for form, s := range forms {
		cases := []struct {
			name string
			act  writeAct
			what string
		}{
			{"push branch", writeAct{kind: actPush, branch: "touchmark/" + s}, "the branch name"},
			{"new pull request head", writeAct{kind: actCreate, newPR: platform.NewPR{Head: "touchmark/" + s}}, "the new pull request"},
			{"new pull request title", writeAct{kind: actCreate, newPR: platform.NewPR{Head: "touchmark/a", Title: "sync " + s}}, "the new pull request"},
			{"new pull request body", writeAct{kind: actCreate, newPR: platform.NewPR{Head: "touchmark/a", Body: "| `" + s + "` |"}}, "the new pull request"},
			{"new pull request label", writeAct{kind: actCreate, newPR: platform.NewPR{Head: "touchmark/a", Labels: []string{"ok", s}}}, "the new pull request"},
			{"edit body", writeAct{kind: actEdit, pr: 7, edit: platform.PREdit{Body: str(s)}}, "the edit of #7"},
			{"edit title", writeAct{kind: actEdit, pr: 7, edit: platform.PREdit{Title: str(s)}}, "the edit of #7"},
			{"edit base", writeAct{kind: actEdit, pr: 7, edit: platform.PREdit{Base: str(s)}}, "the edit of #7"},
			{"edit label", writeAct{kind: actEdit, pr: 7, edit: platform.PREdit{AddLabels: []string{s}}}, "the edit of #7"},
			{"comment", writeAct{kind: actComment, pr: 9, comment: "closed: " + s}, "the comment on #9"},
		}
		for _, tc := range cases {
			// A harmless write before it does not hide it.
			acts := []writeAct{{kind: actComment, pr: 1, comment: "fine"}, tc.act}
			what, err := x.leak(t.Context(), acts)
			if err != nil || !strings.HasPrefix(what, tc.what) {
				t.Errorf("%s, %s: leak = %q, %v; want %q", form, tc.name, what, err, tc.what)
			}
			// Only the branch finding names its text, the branch: the report
			// is masked as a whole where it is printed, and nothing is sent.
			if strings.Contains(what, leakSecret) && tc.what != "the branch name" {
				t.Errorf("%s, %s: the finding %q quotes the secret", form, tc.name, what)
			}
		}
	}
	clean := []writeAct{
		{kind: actPush, branch: "touchmark/acme-eng"},
		{kind: actCreate, newPR: platform.NewPR{Head: "touchmark/acme-eng", Title: "chore: sync", Body: "no secret here", Labels: []string{"engineering-assets"}}},
		{kind: actEdit, pr: 3, edit: platform.PREdit{Body: str("body"), Title: str("title"), Base: str("main"), AddLabels: []string{"x"}}},
		{kind: actComment, pr: 3, comment: "leak-5ecr3t-012345678"}, // one character short of the secret
	}
	if what, err := x.leak(t.Context(), clean); what != "" || err != nil {
		t.Errorf("clean writes: leak = %q, %v", what, err)
	}
}

// A secret in a path the hub ships reaches the body's changes table and
// the commit: the target writes nothing at all, not even its push, and the
// report says what held it without quoting it.
func TestExecuteSecretInPath(t *testing.T) {
	w := newExWorld(t, exConfig{git: true})
	w.reg.Add(leakSecret)
	w.files["docs/"+leakSecret+".md"] = "a page whose name is a token\n"
	tg := w.target("acme/leak", exOptIn, "version: 1\n")
	w.p.ResetCalls()
	w.execute(w.work(tg, nil))
	exWant(t, tg, report.OutcomeFailed, "secret-exposure", 0)
	if writes := w.p.Writes(); len(writes) > 0 {
		t.Errorf("writes %q", writes)
	}
	if got := w.p.Branch(tg.repo.ID, exBranch); got != "" {
		t.Errorf("the sync branch was pushed: %s", got)
	}
	if !slices.ContainsFunc(tg.res.Warnings, func(s string) bool { return strings.Contains(s, "holds a secret touchmark holds") }) {
		t.Errorf("warnings %q", tg.res.Warnings)
	}
	for _, s := range tg.res.Warnings {
		if strings.Contains(s, leakSecret) {
			t.Errorf("warning %q quotes the secret", s)
		}
	}
}
