package distribute

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

func TestParseCooldown(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30d": 30 * 24 * time.Hour, "12h": 12 * time.Hour, "1d": 24 * time.Hour,
		"": 0, "d": 0, "0d": 0, "-1d": 0, "30m": 0, "x1d": 0,
	} {
		if got := parseCooldown(in); got != want {
			t.Errorf("parseCooldown(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNoReplyEmail(t *testing.T) {
	for _, tc := range []struct{ typ, host, id, login, want string }{
		{"github", "github.com", "55501017", "acme-write[bot]", "55501017+acme-write[bot]@users.noreply.github.com"},
		{"github", "ghe.example.com:8443", "", "bot", "bot@users.noreply.ghe.example.com"},
		{"gitlab", "gitlab.example.com", "12", "tm-writer", "12-tm-writer@users.noreply.gitlab.example.com"},
		{"gitea", "git.example.org", "7", "tm-bot", "tm-bot@noreply.git.example.org"},
		{"forgejo", "codeberg.org", "", "tm-bot", "tm-bot@noreply.codeberg.org"},
		{"bitbucket", "bitbucket.org", "{583f7ec5-ed93-49a9-b449-cfc4556cd7f8}", "{583f7ec5-ed93-49a9-b449-cfc4556cd7f8}", "583f7ec5-ed93-49a9-b449-cfc4556cd7f8@touchmark.invalid"},
		{"bitbucket", "bitbucket.org", "", "{583f7ec5-ed93-49a9-b449-cfc4556cd7f8}", "583f7ec5-ed93-49a9-b449-cfc4556cd7f8@touchmark.invalid"},
		{"azure-devops", "dev.azure.com", "56b0f042-b05e-86d9-b0f7-c5ddb1e76385", "56b0f042-b05e-86d9-b0f7-c5ddb1e76385", "56b0f042-b05e-86d9-b0f7-c5ddb1e76385@touchmark.invalid"},
	} {
		if got := noReplyEmail(tc.typ, tc.host, tc.id, tc.login); got != tc.want {
			t.Errorf("noReplyEmail(%q, %q, %q, %q) = %q, want %q", tc.typ, tc.host, tc.id, tc.login, got, tc.want)
		}
	}
}

func TestPairsOf(t *testing.T) {
	a, b := oid("a"), oid("b")
	got := pairsOf([]gitx.DiffEntry{
		{Path: "added", OldMode: "000000", NewMode: "100644", OldOID: decide.ZeroOID, NewOID: a},
		{Path: "changed", OldMode: "100644", NewMode: "100644", OldOID: a, NewOID: b},
		{Path: "chmod", OldMode: "100644", NewMode: "100755", OldOID: a, NewOID: a},
		{Path: "deleted", OldMode: "100644", NewMode: "000000", OldOID: b, NewOID: decide.ZeroOID},
	})
	want := []decide.Pair{
		{Path: "added", From: decide.ZeroOID, Mode: "100644", To: a},
		{Path: "changed", From: a, Mode: "100644", To: b},
		{Path: "chmod", From: a, Mode: "100755", To: a},
		{Path: "deleted", From: b, Mode: decide.ModeDelete, To: decide.ZeroOID},
	}
	if !slices.Equal(got, want) {
		t.Errorf("pairsOf =\n%v\nwant\n%v", got, want)
	}
	// The same pairs as decide.Pairs writes, so the keys agree.
	if decide.Key(decide.StreamSync, got) != decide.Key(decide.StreamSync, want) {
		t.Error("keys differ")
	}
}

func TestTreeConflicts(t *testing.T) {
	tree := &snapshot.Tree{Entries: map[string]snapshot.Entry{
		"tools/x.sh": {Mode: "100644"}, "README": {Mode: "100644"}, "Docs/a.md": {Mode: "100644"}, "docs/b.md": {Mode: "100644"},
	}}
	create := func(path string) decide.Pair {
		return decide.Pair{Path: path, From: decide.ZeroOID, Mode: "100644", To: oid(path)}
	}
	got := treeConflicts(tree, []decide.Pair{
		create("Tools/run.md"),         // a new directory clashing with tools/
		create("bin"), create("bin/x"), // a file and a path beneath it
		create("new/One.md"), create("new/one.md"), // two new names of one fold
		create("docs/c.md"), // B's own clash (Docs, docs) is not D's
		create("fine/ok.md"),
		{Path: "README", From: oid("r"), Mode: decide.ModeDelete, To: decide.ZeroOID},
		create("README/inside.md"), // the file above it is deleted
	})
	want := map[string]string{
		"Tools/run.md": `"Tools" differs only by case from "tools" of the new tree`,
		"bin":          "it is a directory of the new tree",
		"bin/x":        `its parent "bin" is a file of the new tree`,
		"new/One.md":   `"new/One.md" differs only by case from "new/one.md" of the new tree`,
		"new/one.md":   `"new/one.md" differs only by case from "new/One.md" of the new tree`,
	}
	if !maps.Equal(got, want) {
		t.Errorf("treeConflicts =\n%v\nwant\n%v", got, want)
	}
}

func TestKeptPR(t *testing.T) {
	own := func(n int64, head string, state platform.PRState) decide.OwnPR {
		return decide.OwnPR{PR: platform.PR{Number: n, Head: head, State: state}, Marker: marker.Marker{Key: "k"}}
	}
	for _, tc := range []struct {
		name string
		s    prSet
		want int64
	}{
		{"none", prSet{}, 0},
		{"closed only", prSet{own: []decide.OwnPR{own(3, branch, platform.Closed)}}, 0},
		{"newest", prSet{own: []decide.OwnPR{own(5, alias, platform.Open), own(4, alias, platform.Open)}}, 5},
		{"sync branch first", prSet{own: []decide.OwnPR{own(5, alias, platform.Open), own(2, branch, platform.Open)}}, 2},
		{"marker-invalid counts", prSet{own: []decide.OwnPR{own(2, alias, platform.Open)},
			invalid: []platform.PR{{Number: 7, Head: branch, State: platform.Open}}}, 7},
	} {
		got, ok := keptPR(tc.s, branch)
		if (tc.want == 0) == ok || got.PR.Number != tc.want {
			t.Errorf("%s: keptPR = #%d, %v; want #%d", tc.name, got.PR.Number, ok, tc.want)
		}
	}
}

func TestNeedPerms(t *testing.T) {
	for _, tc := range []struct {
		steps []decide.Step
		want  platform.Perms
	}{
		{nil, platform.Perms{}},
		{[]decide.Step{{Kind: decide.StepAck}, {Kind: decide.StepComment}}, platform.Perms{PRs: true}},
		{[]decide.Step{{Kind: decide.StepPush}, {Kind: decide.StepCreatePR}}, platform.Perms{Contents: true, PRs: true}},
		{[]decide.Step{{Kind: decide.StepRecreateBranch, NeedWorkflows: true}}, platform.Perms{Contents: true, Workflows: true}},
		{[]decide.Step{{Kind: decide.StepDeleteBranch}}, platform.Perms{Contents: true}},
	} {
		if got := needPerms(tc.steps); got != tc.want {
			t.Errorf("needPerms(%v) = %+v, want %+v", tc.steps, got, tc.want)
		}
	}
}

func TestRowsAndGiteaWorkflows(t *testing.T) {
	a := oid("a")
	rows := rowsOf([]decide.Pair{
		{Path: ".gitea/workflows/ci.yml", From: decide.ZeroOID, Mode: "100644", To: a},
		{Path: "old.md", From: a, Mode: decide.ModeDelete, To: decide.ZeroOID},
		{Path: "run.sh", From: a, Mode: "100755", To: a},
		{Path: "AGENTS.md", From: oid("b"), Mode: "100644", To: a},
	}, map[string]string{"AGENTS.md": "base"})
	want := []prbody.Change{
		{Path: ".gitea/workflows/ci.yml", Action: prbody.ActionCreate, Mode: "100644"},
		{Path: "old.md", Action: prbody.ActionDelete},
		{Path: "run.sh", Action: prbody.ActionChmod, Mode: "100755"},
		{Path: "AGENTS.md", Pack: "base", Action: prbody.ActionUpdate, Mode: "100644"},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rowsOf =\n%v\nwant\n%v", rows, want)
	}
	github := &snapshot.Tree{Entries: map[string]snapshot.Entry{".github/workflows/ci.yml": {Mode: "100644"}}}
	both := &snapshot.Tree{Entries: map[string]snapshot.Entry{".github/workflows/ci.yml": {}, ".gitea/workflows/x.yml": {}}}
	for _, tc := range []struct {
		typ  string
		tree *snapshot.Tree
		want bool
	}{
		{"gitea", github, true},
		{"forgejo", github, true},
		{"github", github, false},
		{"gitea", both, false},
		{"gitea", &snapshot.Tree{}, false},
	} {
		if got := giteaWorkflows(tc.typ, tc.tree, rows); got != tc.want {
			t.Errorf("giteaWorkflows(%s, %v) = %v, want %v", tc.typ, tc.tree.Entries, got, tc.want)
		}
	}
}

func TestHubVisible(t *testing.T) {
	public := &target{repo: platform.Repo{Visibility: "public"}}
	private := &target{repo: platform.Repo{Visibility: "private"}}
	for _, tc := range []struct {
		link, hub string
		t         *target
		want      bool
	}{
		{"auto", "public", public, true},
		{"auto", "private", private, true},
		{"auto", "private", public, false},
		{"auto", "", public, false},
		{"always", "private", public, true},
		{"never", "public", private, false},
	} {
		w := newWorld(t)
		d := w.deps()
		d.Hub.PR.LinkHub = tc.link
		d.HubContext = hubch.Context{CI: hubch.Local, Visibility: tc.hub}
		d.Write.HubURL = hubURL
		r, err := newRun(d, ModePlan)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.hubVisible(tc.t); got != tc.want {
			t.Errorf("link_hub %s, hub %q, target %s: %v, want %v", tc.link, tc.hub, tc.t.repo.Visibility, got, tc.want)
		}
		wantURL, wantRepo := "", ""
		if tc.want {
			wantURL, wantRepo = hubURL, "github.com/acme/engineering-assets"
		}
		if r.hubURL(tc.t) != wantURL || r.hubRepo(tc.t) != wantRepo {
			t.Errorf("link_hub %s: URL %q, repo %q", tc.link, r.hubURL(tc.t), r.hubRepo(tc.t))
		}
	}
}

func TestMinusAndWrites(t *testing.T) {
	x := decide.Pair{Path: "x", From: decide.ZeroOID, Mode: "100644", To: oid("x")}
	y := decide.Pair{Path: "y", From: decide.ZeroOID, Mode: "100644", To: oid("y")}
	if got := minus([]decide.Pair{x, y}, []decide.Pair{y}); !slices.Equal(got, []decide.Pair{x}) {
		t.Errorf("minus = %v", got)
	}
	w := &Work{Decision: decide.TargetDecision{Steps: []decide.Step{{Kind: decide.StepEditPR}}}, idle: map[int]bool{0: true}}
	if w.writes() {
		t.Error("an idle edit writes")
	}
	w.idle = nil
	if !w.writes() {
		t.Error("an edit does not write")
	}
}

// Gitea 1.26 and 1.27 refuse an unsigned push to a branch whose protection
// requires signed commits with a failure of their own pre-receive hook (seen
// live by the e2e fact "signed-commits"): a permanent refusal,
// blocked:rules:pre-receive-hook, never a transient failure.
func TestPushRuleGiteaUnsignedRefusal(t *testing.T) {
	const oid = "3f786850e387550fdab836ed7e6dc881de23001b"
	res := gitx.ParsePush("To http://localhost:3000/acme/api.git\n!\t"+oid+":refs/heads/touchmark/sync\t[remote rejected] (pre-receive hook declined)\nDone\n",
		"remote: error: Internal Server Error (no message for end users)\n"+
			"To http://localhost:3000/acme/api.git\n ! [remote rejected] "+oid+" -> touchmark/sync (pre-receive hook declined)\n"+
			"error: failed to push some refs to 'http://localhost:3000/acme/api.git'\n")
	if res.Status != gitx.PushPolicy || res.Transient() {
		t.Errorf("ParsePush = %+v, want a policy refusal", res)
	}
	if got := pushRule(res.Message); got != "pre-receive-hook" {
		t.Errorf("pushRule(%q) = %q, want pre-receive-hook", res.Message, got)
	}
}

// TestPushRuleBitbucketBranchRestriction: Bitbucket Cloud refuses a push
// that a branch restriction forbids with "Permission denied to update
// branch …" and "pre-receive hook declined": the rule is
// the protected branch, not the hook, whatever the branch is called.
func TestPushRuleBitbucketBranchRestriction(t *testing.T) {
	const oid = "3f786850e387550fdab836ed7e6dc881de23001b"
	for _, branch := range []string{"touchmark/acme-eng", "release/delete-me"} {
		res := gitx.ParsePush("To https://bitbucket.org/acme/api.git\n!\t"+oid+":refs/heads/"+branch+"\t[remote rejected] (pre-receive hook declined)\nDone\n",
			"remote: Permission denied to update branch "+branch+".\n"+
				"To https://bitbucket.org/acme/api.git\n ! [remote rejected] "+oid+" -> "+branch+" (pre-receive hook declined)\n"+
				"error: failed to push some refs to 'https://bitbucket.org/acme/api.git'\n")
		if res.Status != gitx.PushPolicy || res.Transient() {
			t.Errorf("ParsePush = %+v, want a policy refusal", res)
		}
		if got := pushRule(res.Message); got != "protected-branch" {
			t.Errorf("pushRule(%q) = %q, want protected-branch", res.Message, got)
		}
	}
	for msg, want := range map[string]string{
		"remote: Permission denied to update branch main.":      "protected-branch",
		"remote: PERMISSION DENIED TO UPDATE BRANCH main.":      "protected-branch",
		"[remote rejected] (pre-receive hook declined)":         "pre-receive-hook",
		"remote: Permission denied to delete branch touchmark.": "deletion",
	} {
		if got := pushRule(msg); got != want {
			t.Errorf("pushRule(%q) = %q, want %q", msg, got, want)
		}
	}
}

// TestPushAzureRefusals: Azure Repos refuses a push to a branch a policy
// protects with TF402455 (blocked:rules:policy) and a push the identity
// lacks a Git permission for with TF401027 (blocked:permission:push).
func TestPushAzureRefusals(t *testing.T) {
	const oid = "3f786850e387550fdab836ed7e6dc881de23001b"
	policy := gitx.ParsePush("To https://dev.azure.com/acme/Billing/_git/api\n!\t"+oid+":refs/heads/main\t"+
		"[remote rejected] (TF402455: Pushes to this branch are not permitted; you must use a pull request to update this branch.)\nDone\n", "")
	if outcome, reason := pushOutcome(policy); outcome != report.OutcomeBlocked || reason != "rules:policy" {
		t.Errorf("TF402455: %s:%s, want blocked:rules:policy", outcome, reason)
	}
	perm := gitx.ParsePush("To https://dev.azure.com/acme/Billing/_git/api\n!\t"+oid+":refs/heads/touchmark/acme-eng\t"+
		"[remote rejected] (TF401027: You need the Git 'CreateBranch' permission to perform this action.)\nDone\n", "")
	if outcome, reason := pushOutcome(perm); outcome != report.OutcomeBlocked || reason != "permission:push" {
		t.Errorf("TF401027: %s:%s, want blocked:permission:push", outcome, reason)
	}
}
