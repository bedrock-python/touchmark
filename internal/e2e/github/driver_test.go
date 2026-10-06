package githube2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/github"
	"github.com/bedrock-python/touchmark/internal/platform/github/ghfake"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// Driver behaviours that depend on what GitHub documents about accounts,
// installations and rulesets, against the fake: a user's private
// repositories, an owner's rename, a suspended installation and a ruleset
// the writer may bypass.

// TestUserNamespace: GET /users/{u}/repos lists public repositories only
// (docs), so a user's namespace is listed where the private ones show: the
// App's installation on the user, or GET /user/repos for a token of the
// user. Another user's token sees the public ones, and the listing says it
// is incomplete, so that no sweep runs over it.
func TestUserNamespace(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOptions{flavor: ghfake.DotCom})
	for _, spec := range []ghfake.RepoSpec{
		{Owner: person, Name: "dotfiles", Files: []ghfake.File{{Path: "README.md", Content: []byte("x\n")}}},
		{Owner: person, Name: "notes", Visibility: "private", Files: []ghfake.File{{Path: "README.md", Content: []byte("x\n")}}},
	} {
		try(w.srv.CreateRepo(spec)).of(t)
	}
	try(w.srv.Install(ghfake.InstallSpec{App: writeSlug, Account: person})).of(t)
	_, writer := w.drivers()
	sel := platform.Selector{Namespace: person}
	want := []string{person + "/dotfiles", person + "/notes"}

	res, err := writer.Resolve(t.Context(), sel)
	if err != nil || !res.Complete || !slices.Equal(repoPaths(res.Repos), want) {
		t.Errorf("the App on %s: %v, complete %v, %v", person, repoPaths(res.Repos), res.Complete, err)
	}

	own, err := github.NewReader(w.rp, tokenCredential(w.personToken), w.hc)
	check(t, err)
	res, err = own.Resolve(t.Context(), sel)
	if err != nil || !res.Complete || !slices.Equal(repoPaths(res.Repos), want) {
		t.Errorf("%s's own token: %v, complete %v, %v", person, repoPaths(res.Repos), res.Complete, err)
	}

	try(w.srv.AddUser("bob")).of(t)
	bobToken := try(w.srv.AddPAT("bob", ghfake.PATSpec{Scopes: []string{"repo"}})).of(t)
	w.reg.Add(bobToken, "x-access-token")
	check(t, w.srv.Grant(person+"/notes", "bob", "write"))
	other, err := github.NewReader(w.rp, tokenCredential(bobToken), w.hc)
	check(t, err)
	res, err = other.Resolve(t.Context(), sel)
	if err != nil || res.Complete || !slices.Equal(repoPaths(res.Repos), want[:1]) || !strings.Contains(res.Incomplete, "public repositories only") {
		t.Errorf("another user's token: %v, complete %v (%q), %v", repoPaths(res.Repos), res.Complete, res.Incomplete, err)
	}
}

// TestOwnerRenamed: acme is renamed to acme-eng while targets.yml still
// names repo: acme/svc. GitHub answers API requests under the old name
// with 404 (docs), so the target is missing, while the sweep lists its pull
// request under acme-eng/svc; decide.Sweep must leave it open.
func TestOwnerRenamed(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOptions{flavor: ghfake.DotCom})
	svc := try(w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: "svc", Files: []ghfake.File{{Path: "README.md", Content: []byte("x\n")}}})).of(t)
	const branch = "touchmark/hub"
	try(w.srv.Commit(svc.FullName, ghfake.CommitSpec{Branch: branch, Author: w.writeApp.Bot.Login,
		Files: []ghfake.File{{Path: "AGENTS.md", Content: []byte("sync\n")}}, Message: "sync"})).of(t)
	opened := try(w.srv.OpenPR(svc.FullName, ghfake.PRSpec{Head: branch, Title: "sync", Body: "sync", Author: w.writeApp.Bot.Login})).of(t)
	try(w.srv.RenameAccount(org, "acme-eng")).of(t)
	_, writer := w.drivers()
	bot, err := writer.Self(t.Context())
	check(t, err)

	_, err = writer.Repo(t.Context(), org+"/svc")
	if platform.ClassOf(err) != platform.ClassNotFound {
		t.Fatalf("Repo under the old owner: %v, want not found", err)
	}
	swept, err := writer.OpenPRsBy(t.Context(), []platform.Account{bot}, []string{branch})
	if err != nil || !swept.Complete || len(swept.PRs) != 1 || swept.PRs[0].Repo.Path != "acme-eng/svc" || swept.PRs[0].PR.Number != opened.Number {
		t.Fatalf("OpenPRsBy = %+v, %v", swept, err)
	}
	var candidates []decide.SweepCandidate
	for _, rp := range swept.PRs {
		candidates = append(candidates, decide.SweepCandidate{Provider: "gh", Repo: rp.Repo, PR: decide.OwnPR{PR: rp.PR}})
	}
	closes := decide.Sweep(decide.SweepInput{Candidates: candidates, Active: map[string]bool{},
		Unresolved: map[string]bool{org + "/svc": true}, Complete: true})
	if len(closes) != 0 {
		t.Errorf("the sweep closes %+v under the renamed owner", closes)
	}
}

// TestSuspendedInstallation: the writer is installed on old-org first (the
// lowest installation id), then suspended there, and on acme. Requests
// about no repository (Self, lookups) go through acme's installation, and
// the sweep leaves old-org out with a note, complete: the writer could
// close nothing there.
func TestSuspendedInstallation(t *testing.T) {
	t.Parallel()
	keys, err := appKeys()
	check(t, err)
	srv, err := ghfake.New(ghfake.Options{Flavor: ghfake.DotCom})
	check(t, err)
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the fake: %v", err)
		}
	})
	w := &world{t: t, srv: srv, opts: worldOptions{flavor: ghfake.DotCom}, readKey: keys[0].pem, writeKey: keys[1].pem, reg: redact.New()}
	w.hc = httpx.New(httpx.Options{Redact: w.reg, Timeout: requestTimeout})
	try(srv.AddOrg("old-org", ghfake.PlanTeam)).of(t)
	try(srv.AddOrg(org, ghfake.PlanTeam)).of(t)
	w.writeApp = try(srv.RegisterApp(ghfake.AppSpec{Slug: writeSlug, Owner: org, PublicKey: &keys[1].key.PublicKey,
		Permissions: ghfake.Permissions{"contents": ghfake.Write, "pull_requests": ghfake.Write, "metadata": ghfake.Read}})).of(t)
	old := try(srv.Install(ghfake.InstallSpec{App: writeSlug, Account: "old-org"})).of(t)
	acme := try(srv.Install(ghfake.InstallSpec{App: writeSlug, Account: org})).of(t)
	if old.ID >= acme.ID {
		t.Fatalf("installation ids %d, %d: the suspended one must be listed first", old.ID, acme.ID)
	}
	check(t, srv.SuspendInstallation(old.ID, true))
	w.rp = w.provider("gh")
	w.host = w.rp.Host
	writer, err := github.NewWriter(w.rp, w.credential(w.writeApp, w.writeKey), w.hc)
	check(t, err)

	bot, err := writer.Self(t.Context())
	if err != nil || bot.ID != strconv.FormatInt(w.writeApp.Bot.ID, 10) {
		t.Fatalf("Self = %+v, %v", bot, err)
	}
	swept, err := writer.OpenPRsBy(t.Context(), []platform.Account{bot}, []string{"touchmark/hub"})
	if err != nil || !swept.Complete || len(swept.Notes) != 1 || !strings.Contains(swept.Notes[0], "old-org is suspended") {
		t.Errorf("OpenPRsBy = %+v, %v", swept, err)
	}
	if n := len(srv.Tokens(old.ID)); n != 0 {
		t.Errorf("%d tokens of the suspended installation", n)
	}
}

// TestRulesetBypass: a ruleset against force pushes on touchmark/** with
// the writer's App on its bypass list. rules/branches lists the rule
// whoever may bypass it (docs); the writer's Preflight reads the ruleset's
// current_user_can_bypass and drops it, so an open pull request's branch
// can be rebuilt. Without the bypass the rule stays, and the reader, which
// cannot tell the writer's bypass, always reports it. A rule of the
// default branch stays for the writer too: the person who merges meets it.
func TestRulesetBypass(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOptions{flavor: ghfake.DotCom})
	reader, writer := w.drivers()
	const branch = "touchmark/hub"
	branches := []string{"main", branch}
	for _, tc := range []struct {
		name   string
		bypass []string
	}{{"bypass", []string{writeSlug}}, {"no-bypass", nil}} {
		created := try(w.srv.CreateRepo(ghfake.RepoSpec{Owner: org, Name: tc.name, Files: []ghfake.File{{Path: "README.md", Content: []byte("x\n")}}})).of(t)
		try(w.srv.AddRuleset(created.FullName, ghfake.Ruleset{Name: "no force", Include: []string{"refs/heads/touchmark/**", "~DEFAULT_BRANCH"},
			Rules: []ghfake.Rule{{Type: ghfake.RuleNonFastForward}, {Type: ghfake.RuleDeletion}}, BypassApps: tc.bypass})).of(t)
		repo := w.repo(created, false, nil)
		got, err := writer.(platform.Preflighter).Preflight(t.Context(), repo, branches)
		wantForce := []string{"main", branch}
		if tc.bypass != nil {
			wantForce = []string{"main"}
		}
		if err != nil || !got.Known || !slices.Equal(got.NoForcePush, wantForce) {
			t.Errorf("%s: writer Preflight = %+v, %v; want NoForcePush %q", tc.name, got, err, wantForce)
		}
		got, err = reader.(platform.Preflighter).Preflight(t.Context(), repo, branches)
		if err != nil || !slices.Equal(got.NoForcePush, []string{"main", branch}) {
			t.Errorf("%s: reader Preflight = %+v, %v", tc.name, got, err)
		}
	}
}

// tokenCredential is a token credential.
func tokenCredential(token string) auth.Credential {
	return auth.Credential{Kind: auth.Token, Token: token}
}

// repoPaths returns the paths of repos.
func repoPaths(repos []platform.Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Path
	}
	return out
}
