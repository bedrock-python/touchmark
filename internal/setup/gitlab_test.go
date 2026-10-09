package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/redact"
)

// glWorld is a GitLab with the person alice, an Owner of acme (whose
// subgroup acme/services holds the targets) and a Maintainer of the group
// platform, where the hub platform/hub lives.
type glWorld struct {
	f        *glFake
	alice    *fUser
	token    string
	acme     *fGroup
	services *fGroup
	platform *fGroup
	hub      *fProject
	reg      *redact.Registry
}

func newGLWorld(t *testing.T, version string) *glWorld {
	t.Helper()
	f := newGLFake(t, version)
	f.addUser("root", true)
	alice, token := f.addUser("alice", false)
	w := &glWorld{f: f, alice: alice, token: token, reg: redact.New()}
	w.acme = f.addGroup("acme", 0, map[int64]int{alice.id: 50})
	w.services = f.addGroup("acme/services", w.acme.id, nil)
	w.platform = f.addGroup("platform", 0, map[int64]int{alice.id: 40})
	w.hub = f.addProject("hub", w.platform.id, nil)
	return w
}

// input is the input of a run as the command line builds it.
func (w *glWorld) input(dry bool) GitLabInput {
	w.reg.Add(w.token, "oauth2")
	return GitLabInput{
		APIURL: w.f.api(), Host: "127.0.0.1", Project: "platform/hub", Group: "acme/services", Token: w.token,
		Client: httpx.New(httpx.Options{Redact: w.reg}), Isolation: "platform", Accounts: AccountsAuto,
		ReadVar: "TOUCHMARK_READ_TOKEN", WriteVar: "TOUCHMARK_WRITE_TOKEN", Schedules: true, DryRun: dry, Engine: "test",
		Now: func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }, Redact: w.reg, BasicUsers: []string{"oauth2"},
	}
}

// run runs setup and returns the report, failing on an error.
func (w *glWorld) run(t *testing.T, in GitLabInput) *Report {
	t.Helper()
	rep, err := GitLab(context.Background(), in)
	if err != nil {
		t.Fatalf("GitLab: %v", err)
	}
	w.noSecrets(t, rep)
	return rep
}

// noSecrets fails when a report shows a token of the fake.
func (w *glWorld) noSecrets(t *testing.T, rep *Report) {
	t.Helper()
	var text, js bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	for v, tok := range w.f.tokens {
		if strings.Contains(text.String(), v) || strings.Contains(js.String(), v) {
			t.Errorf("the report shows a token:\n%s", text.String())
		}
		// The people's own tokens ("personal") are not the run's.
		if tok.name != "personal" && !w.reg.Contains(v) {
			t.Errorf("a token the run minted is not registered with the redaction")
		}
	}
}

// steps returns "name status" for every step.
func steps(rep *Report) []string {
	var out []string
	for _, s := range rep.Steps {
		out = append(out, s.Name+" "+string(s.Status))
	}
	return out
}

// step returns the first step named name.
func step(t *testing.T, rep *Report, name string) Step {
	t.Helper()
	for _, s := range rep.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %s in %q", name, steps(rep))
	return Step{}
}

// wantSteps checks the statuses of named steps.
func wantSteps(t *testing.T, rep *Report, want map[string]Status) {
	t.Helper()
	for name, status := range want {
		if s := step(t, rep, name); s.Status != status {
			t.Errorf("step %s: %s %q, want %s", name, s.Status, s.Detail, status)
		}
	}
}

// onlyOK fails unless every step is ok.
func onlyOK(t *testing.T, rep *Report) {
	t.Helper()
	for _, s := range rep.Steps {
		if s.Status != StatusOK {
			t.Errorf("step %s: %s %q, want ok", s.Name, s.Status, s.Detail)
		}
	}
}

// activeToken returns the active token of a user.
func (w *glWorld) activeToken(t *testing.T, login string) *fToken {
	t.Helper()
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	var out *fToken
	for _, tok := range w.f.tokens {
		if tok.active && w.f.users[tok.user].username == login {
			if out != nil {
				t.Fatalf("%s has two active tokens", login)
			}
			out = tok
		}
	}
	if out == nil {
		t.Fatalf("%s has no active token", login)
	}
	return out
}

// checkHub checks the hub after a run: the variables hold the accounts'
// active tokens, the environment, the protected default branch, the
// settings and the schedules.
func (w *glWorld) checkHub(t *testing.T, rep *Report) {
	t.Helper()
	rv := w.f.variable(w.hub, "TOUCHMARK_READ_TOKEN", "*")
	wv := w.f.variable(w.hub, "TOUCHMARK_WRITE_TOKEN", Environment)
	switch {
	case rv == nil || rv.protected || !rv.masked || !rv.hide:
		t.Errorf("reader variable %+v: want masked, hidden, not protected, scope *", rv)
	case wv == nil || !wv.protected || !wv.masked || !wv.hide:
		t.Errorf("writer variable %+v: want protected, masked, hidden, scope %s", wv, Environment)
	}
	if rv != nil {
		if tok := w.activeToken(t, rep.Reader); tok.value != rv.value || !sameSet(tok.scopes, []string{"read_api", "read_repository"}) {
			t.Errorf("the reader variable holds no active read token of %s: %+v", rep.Reader, tok)
		}
	}
	if wv != nil {
		if tok := w.activeToken(t, rep.Writer); tok.value != wv.value || !sameSet(tok.scopes, []string{"api", "write_repository"}) {
			t.Errorf("the writer variable holds no active write token of %s: %+v", rep.Writer, tok)
		}
	}
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	if !slices.Contains(w.hub.envs, Environment) {
		t.Errorf("environments %q", w.hub.envs)
	}
	b := w.hub.branches["main"]
	if b == nil || len(b.push) != 1 || b.push[0].level != 0 || b.force || len(b.merge) != 1 || b.merge[0].level != 40 {
		t.Errorf("main's protection %+v", b)
	}
	if got := w.hub.settings["ci_pipeline_variables_minimum_override_role"]; got != "no_one_allowed" {
		t.Errorf("pipeline variables role %v", got)
	}
	if got, ok := w.hub.settings["protect_merge_request_pipelines"]; ok && got != false {
		t.Errorf("protect_merge_request_pipelines %v", got)
	}
	if len(w.hub.schedules) != 2 {
		t.Errorf("schedules %v", w.hub.schedules)
	}
	for _, login := range []string{rep.Reader, rep.Writer} {
		for _, u := range w.f.users {
			if u.username == login && w.f.projectLevel(w.hub, u.id) != 0 {
				t.Errorf("%s reaches the hub", login)
			}
		}
	}
}

// TestGitLabGroupTokens: GitLab CE 18.11 has no service accounts API, so
// setup makes group access tokens; a second run and a dry run write
// nothing.
func TestGitLabGroupTokens(t *testing.T) {
	w := newGLWorld(t, "18.11")
	rep := w.run(t, w.input(false))
	if rep.Accounts != AccountsGroup || rep.ExitCode() != ExitOK {
		t.Fatalf("accounts %s, exit %d: %q", rep.Accounts, rep.ExitCode(), steps(rep))
	}
	wantSteps(t, rep, map[string]Status{
		"hub": StatusOK, "group": StatusOK, "reader-account": StatusDone, "reader-role": StatusOK, "writer-account": StatusDone,
		"writer-role": StatusOK, "writer-hidden": StatusOK, "environment": StatusDone, "reader-variable": StatusDone,
		"writer-variable": StatusDone, "default-branch": StatusDone, "pipeline-variables": StatusDone, "mr-pipelines": StatusDone,
		"schedule": StatusDone, "check key-location": StatusOK, "check protected-branches": StatusOK, "check pipeline-variables": StatusOK,
	})
	if !strings.HasPrefix(rep.Reader, fmt.Sprintf("group_%d_bot_", w.services.id)) || !strings.HasPrefix(rep.Writer, fmt.Sprintf("group_%d_bot_", w.services.id)) {
		t.Errorf("reader %s, writer %s", rep.Reader, rep.Writer)
	}
	if d := step(t, rep, "default-branch").Detail; !strings.Contains(d, "protected main again") {
		t.Errorf("default-branch on Free: %q", d)
	}
	if !slices.ContainsFunc(rep.Next, func(n string) bool { return strings.Contains(n, "set the writer to "+rep.Writer) }) {
		t.Errorf("next %q", rep.Next)
	}
	w.checkHub(t, rep)
	w.f.mu.Lock()
	reader, writer := w.f.tokens[w.f.variableValueLocked(w.hub, "TOUCHMARK_READ_TOKEN")], w.f.tokens[w.f.variableValueLocked(w.hub, "TOUCHMARK_WRITE_TOKEN")]
	w.f.mu.Unlock()
	if reader.level != 20 || writer.level != 30 || reader.name != fmt.Sprintf("touchmark-reader-hub-%d", w.hub.id) {
		t.Errorf("reader token %+v, writer token %+v", reader, writer)
	}

	before := w.f.writeCount()
	again := w.run(t, w.input(false))
	onlyOK(t, again)
	if n := w.f.writeCount() - before; n != 0 || again.ExitCode() != ExitOK {
		t.Errorf("the second run wrote %d times, exit %d", n, again.ExitCode())
	}
	dry := w.run(t, w.input(true))
	onlyOK(t, dry)
	if n := w.f.writeCount() - before; n != 0 {
		t.Errorf("the dry run wrote %d times", n)
	}
}

// variableValueLocked returns the value of a variable by key in any
// scope. Called with mu held.
func (f *glFake) variableValueLocked(p *fProject, key string) string {
	for _, v := range p.vars {
		if v.key == key {
			return v.value
		}
	}
	return ""
}

// TestGitLabServiceAccounts: where the instance has service accounts
// (CE 19.x, or an Owner allowed to create them), the reader and the writer
// are service accounts of the top-level group, members of the group of
// targets; their tokens are named after the hub. A write key found
// unprotected is rotated: the old token is revoked.
func TestGitLabServiceAccounts(t *testing.T) {
	w := newGLWorld(t, "19.4")
	w.f.saAPI, w.f.saCreate = true, true
	rep := w.run(t, w.input(false))
	if rep.Accounts != AccountsService || rep.ExitCode() != ExitOK {
		t.Fatalf("accounts %s, exit %d: %q", rep.Accounts, rep.ExitCode(), steps(rep))
	}
	if rep.Reader != "acme-services-touchmark-reader" || rep.Writer != "acme-services-touchmark-writer" {
		t.Errorf("reader %s, writer %s", rep.Reader, rep.Writer)
	}
	wantSteps(t, rep, map[string]Status{"reader-role": StatusDone, "writer-role": StatusDone, "writer-variable": StatusDone})
	w.checkHub(t, rep)
	w.f.mu.Lock()
	for _, u := range w.f.users {
		if u.username == rep.Reader && (u.saOf != w.acme.id || w.f.services(u.id) != 20) {
			t.Errorf("reader %+v: member level %d", u, w.f.services(u.id))
		}
		if u.username == rep.Writer && (u.saOf != w.acme.id || w.f.services(u.id) != 30) {
			t.Errorf("writer %+v: member level %d", u, w.f.services(u.id))
		}
	}
	w.f.mu.Unlock()
	old := w.activeToken(t, rep.Writer)
	if old.name != fmt.Sprintf("touchmark-hub-%d", w.hub.id) {
		t.Errorf("token name %q", old.name)
	}

	before := w.f.writeCount()
	onlyOK(t, w.run(t, w.input(false)))
	if n := w.f.writeCount() - before; n != 0 {
		t.Errorf("the second run wrote %d times", n)
	}

	// Someone made the write key visible to every branch.
	w.f.variable(w.hub, "TOUCHMARK_WRITE_TOKEN", Environment).protected = false
	rep = w.run(t, w.input(false))
	s := step(t, rep, "writer-variable")
	if s.Status != StatusDone || !strings.Contains(s.Detail, "it was not protected") {
		t.Errorf("writer-variable %s %q", s.Status, s.Detail)
	}
	if w.f.tokenOf(old.value).active {
		t.Error("the exposed write token is still active")
	}
	w.checkHub(t, rep)
}

// services is a user's direct level in acme/services. Called with mu
// held.
func (f *glFake) services(uid int64) int {
	for _, g := range f.groups {
		if g.path == "acme/services" {
			return g.members[uid]
		}
	}
	return 0
}

// TestGitLabAutoFallsBack: the instance lists service accounts but does
// not let the Owner create them: group access tokens instead.
func TestGitLabAutoFallsBack(t *testing.T) {
	w := newGLWorld(t, "19.4")
	w.f.saAPI = true
	rep := w.run(t, w.input(false))
	if rep.Accounts != AccountsGroup || rep.ExitCode() != ExitOK {
		t.Fatalf("accounts %s, exit %d: %q", rep.Accounts, rep.ExitCode(), steps(rep))
	}
	if d := step(t, rep, "reader-account").Detail; !strings.Contains(d, "group access tokens instead") {
		t.Errorf("reader-account %q", d)
	}
	w.checkHub(t, rep)
	// Explicitly service accounts: refused.
	in := w.input(false)
	in.Accounts = AccountsService
	rep, err := GitLab(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if s := step(t, rep, "reader-account"); s.Status != StatusFail {
		t.Errorf("reader-account %s %q", s.Status, s.Detail)
	}
}

// TestGitLabRepairsWriteKey: a write key in scope * and an unmasked one in
// the environment are replaced by one protected, masked and hidden
// variable with a new token.
func TestGitLabRepairsWriteKey(t *testing.T) {
	// leak puts two tokens of a writer made by hand where jobs could read
	// them: a copy for every environment, and the scoped variable without
	// masking. It returns both, registered as a run would register them.
	leak := func(t *testing.T, w *glWorld, hideCopy bool) (shown, unmasked string) {
		t.Helper()
		_, shown = w.f.addUser("hand-writer", false)
		w.f.mu.Lock()
		unmasked = w.f.mint(w.f.tokens[shown].user, "by hand", []string{"api"}).value
		w.hub.vars = append(w.hub.vars,
			&fVar{key: "TOUCHMARK_WRITE_TOKEN", value: shown, scope: "*", varType: "env_var", masked: hideCopy, hide: hideCopy},
			&fVar{key: "TOUCHMARK_WRITE_TOKEN", value: unmasked, scope: Environment, protected: true, varType: "env_var"})
		w.f.mu.Unlock()
		w.reg.Add(shown)
		w.reg.Add(unmasked)
		return shown, unmasked
	}
	writeVars := func(w *glWorld) int {
		w.f.mu.Lock()
		defer w.f.mu.Unlock()
		n := 0
		for _, v := range w.hub.vars {
			if v.key == "TOUCHMARK_WRITE_TOKEN" {
				n++
			}
		}
		return n
	}

	t.Run("revoked", func(t *testing.T) {
		w := newGLWorld(t, "18.11")
		shown, unmasked := leak(t, w, false)
		rep := w.run(t, w.input(false))
		s := step(t, rep, "writer-variable")
		for _, want := range []string{`deleted the copy with scope "*"`, `revoked the token TOUCHMARK_WRITE_TOKEN with environment scope "*" held`,
			`revoked the token TOUCHMARK_WRITE_TOKEN with environment scope "touchmark-distribute" held`} {
			if s.Status != StatusDone || !strings.Contains(s.Detail, want) {
				t.Errorf("writer-variable %s %q, want done with %q", s.Status, s.Detail, want)
			}
		}
		for _, tok := range []string{shown, unmasked} {
			if w.f.tokenOf(tok).active {
				t.Errorf("a token a job could read is still active")
			}
		}
		for _, st := range rep.Steps {
			if st.Name == "writer-exposed-token" {
				t.Errorf("step %s %s %q", st.Name, st.Status, st.Detail)
			}
		}
		w.checkHub(t, rep)
		if n := writeVars(w); n != 1 {
			t.Errorf("%d write variables", n)
		}
	})

	// A hidden copy's token cannot be read: the person revokes it.
	t.Run("hidden", func(t *testing.T) {
		w := newGLWorld(t, "18.11")
		shown, unmasked := leak(t, w, true)
		rep := w.run(t, w.input(false))
		s := step(t, rep, "writer-exposed-token")
		if s.Status != StatusManual || !strings.Contains(s.Detail, `TOUCHMARK_WRITE_TOKEN with environment scope "*"`) || !strings.Contains(s.Detail, "revoke that token by hand") {
			t.Errorf("writer-exposed-token %s %q", s.Status, s.Detail)
		}
		if !w.f.tokenOf(shown).active || w.f.tokenOf(unmasked).active {
			t.Errorf("want the hidden copy's token left to the person and the unmasked one revoked")
		}
		w.checkHub(t, rep)
	})

	// GitLab refuses the revocation: the step fails and says what to do.
	t.Run("refused", func(t *testing.T) {
		w := newGLWorld(t, "18.11")
		leak(t, w, false)
		w.f.failRoutes["DELETE personal_access_tokens/:personal_access_tokens"] = 500
		rep := w.run(t, w.input(false))
		s := step(t, rep, "writer-exposed-token")
		if s.Status != StatusFail || !strings.Contains(s.Detail, "could not revoke the token that was stored in TOUCHMARK_WRITE_TOKEN") {
			t.Errorf("writer-exposed-token %s %q", s.Status, s.Detail)
		}
		if rep.ExitCode() == ExitOK {
			t.Errorf("exit %d", rep.ExitCode())
		}
		if n := writeVars(w); n != 1 {
			t.Errorf("%d write variables", n)
		}
	})

	// setup's own token in an exposed copy: the rotation revoked it, and
	// the copy goes.
	t.Run("own token", func(t *testing.T) {
		w := newGLWorld(t, "18.11")
		w.run(t, w.input(false))
		own := w.f.variable(w.hub, "TOUCHMARK_WRITE_TOKEN", Environment).value
		w.f.mu.Lock()
		w.hub.vars = append(w.hub.vars, &fVar{key: "TOUCHMARK_WRITE_TOKEN", value: own, scope: "*", varType: "env_var"})
		w.f.mu.Unlock()
		rep := w.run(t, w.input(false))
		s := step(t, rep, "writer-variable")
		if s.Status != StatusDone || !strings.Contains(s.Detail, `deleted the copy with scope "*"`) {
			t.Errorf("writer-variable %s %q", s.Status, s.Detail)
		}
		if w.f.tokenOf(own).active {
			t.Error("the exposed token is still active")
		}
		w.checkHub(t, rep)
	})
}

// TestGitLabDryRunFresh: a dry run on a fresh hub writes nothing and says
// what it would do.
func TestGitLabDryRunFresh(t *testing.T) {
	w := newGLWorld(t, "18.11")
	rep := w.run(t, w.input(true))
	if n := w.f.writeCount(); n != 0 {
		t.Fatalf("the dry run wrote %d times", n)
	}
	wantSteps(t, rep, map[string]Status{
		"reader-account": StatusWould, "writer-account": StatusWould, "writer-hidden": StatusWould, "environment": StatusWould,
		"reader-variable": StatusWould, "writer-variable": StatusWould, "default-branch": StatusWould,
		"pipeline-variables": StatusWould, "mr-pipelines": StatusWould, "schedule": StatusWould,
	})
	if rep.ExitCode() != ExitOK || !rep.DryRun {
		t.Errorf("exit %d", rep.ExitCode())
	}
	var text bytes.Buffer
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "dry run") || !strings.Contains(text.String(), "would mint a token for the writer") {
		t.Errorf("text:\n%s", text.String())
	}
}

// TestGitLabPreconditions: setup refuses, writing nothing, a hub inside the
// group of targets, a token that is no Maintainer of the hub or no Owner
// of the group, and isolation none.
func TestGitLabPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *glWorld, in *GitLabInput)
		want  string
	}{
		{"hub-inside", func(w *glWorld, in *GitLabInput) {
			w.f.addProject("hub2", w.services.id, nil)
			in.Project = "acme/services/hub2"
		}, "inside the group of targets"},
		{"not-maintainer", func(w *glWorld, in *GitLabInput) { w.platform.members[w.alice.id] = 30 }, "is not a Maintainer"},
		{"not-owner", func(w *glWorld, in *GitLabInput) {
			w.acme.members[w.alice.id] = 40
		}, "is not an Owner"},
		{"isolation-none", func(w *glWorld, in *GitLabInput) { in.Isolation = "none" }, "isolated write key only"},
		{"unknown-group", func(w *glWorld, in *GitLabInput) { in.Group = "nope" }, "not visible"},
		{"bad-token", func(w *glWorld, in *GitLabInput) { in.Token = "glpat-unknown-token" }, "refuses the token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newGLWorld(t, "18.11")
			in := w.input(false)
			tc.setup(w, &in)
			_, err := GitLab(context.Background(), in)
			if !IsPrecondition(err) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want a precondition with %q", err, tc.want)
			}
			if n := w.f.writeCount(); n != 0 {
				t.Errorf("wrote %d times", n)
			}
		})
	}
}

// TestGitLabExternal: under external isolation the hub keeps no write key:
// the writer service account is made, its token is the person's to mint
// into the store; a write key in the hub fails.
func TestGitLabExternal(t *testing.T) {
	w := newGLWorld(t, "19.4")
	w.f.saAPI, w.f.saCreate = true, true
	in := w.input(false)
	in.Isolation = "external"
	rep := w.run(t, in)
	wantSteps(t, rep, map[string]Status{"writer-account": StatusDone, "writer-variable": StatusOK, "writer-token": StatusManual})
	if rep.ExitCode() != ExitPending || w.f.variable(w.hub, "TOUCHMARK_WRITE_TOKEN", Environment) != nil {
		t.Errorf("exit %d, steps %q", rep.ExitCode(), steps(rep))
	}
	if !slices.ContainsFunc(rep.Next, func(n string) bool { return strings.Contains(n, "ref_protected is true") }) {
		t.Errorf("next %q", rep.Next)
	}
	w.f.mu.Lock()
	w.hub.vars = append(w.hub.vars, &fVar{key: "TOUCHMARK_WRITE_TOKEN", value: "in-the-hub", scope: Environment, protected: true, masked: true})
	w.f.mu.Unlock()
	rep = w.run(t, in)
	if s := step(t, rep, "writer-variable"); s.Status != StatusFail {
		t.Errorf("writer-variable %s %q", s.Status, s.Detail)
	}

	// Group access tokens under external: the writer is the person's.
	g := newGLWorld(t, "18.11")
	in = g.input(false)
	in.Isolation = "external"
	rep = g.run(t, in)
	wantSteps(t, rep, map[string]Status{"writer-account": StatusManual, "reader-variable": StatusDone})
}

// TestGitLabPremiumUpdatesInPlace: on Premium the default branch's levels
// change in place; the branch is never unprotected.
func TestGitLabPremiumUpdatesInPlace(t *testing.T) {
	w := newGLWorld(t, "18.11")
	w.f.premium = true
	rep := w.run(t, w.input(false))
	if d := step(t, rep, "default-branch").Detail; !strings.Contains(d, "(updated)") {
		t.Errorf("default-branch %q", d)
	}
	w.checkHub(t, rep)
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	for _, wr := range w.f.writes {
		if strings.HasPrefix(wr, "DELETE projects/:projects/protected_branches") {
			t.Errorf("unprotected the branch: %v", w.f.writes)
		}
	}
}

// TestGitLabWriterReachesHub: a writer that reaches the hub fails the run.
func TestGitLabWriterReachesHub(t *testing.T) {
	w := newGLWorld(t, "19.4")
	w.f.saAPI, w.f.saCreate = true, true
	rep := w.run(t, w.input(false))
	for _, u := range w.f.users {
		if u.username == rep.Writer {
			w.platform.members[u.id] = 30
		}
	}
	rep = w.run(t, w.input(false))
	if s := step(t, rep, "writer-hidden"); s.Status != StatusFail || rep.ExitCode() != ExitFailed {
		t.Errorf("writer-hidden %s %q, exit %d", s.Status, s.Detail, rep.ExitCode())
	}
}

// TestGitLabVersions: protect_merge_request_pipelines is in the API from
// 18.10; before 18.1 merge request pipelines get no protected variables;
// in between setup cannot tell and asks the person.
func TestGitLabVersions(t *testing.T) {
	for _, tc := range []struct {
		version string
		status  Status
		exit    int
	}{{"17.11", StatusOK, ExitOK}, {"18.5", StatusManual, ExitPending}, {"18.11", StatusDone, ExitOK}} {
		w := newGLWorld(t, tc.version)
		rep := w.run(t, w.input(false))
		if s := step(t, rep, "mr-pipelines"); s.Status != tc.status || rep.ExitCode() != tc.exit {
			t.Errorf("%s: mr-pipelines %s %q, exit %d", tc.version, s.Status, s.Detail, rep.ExitCode())
		}
	}
}

// TestGitLabStoreFails: a token minted and not stored is never printed;
// the next run rotates it, which revokes it.
func TestGitLabStoreFails(t *testing.T) {
	w := newGLWorld(t, "19.4")
	w.f.saAPI, w.f.saCreate = true, true
	w.f.failRoutes["POST projects/:projects/variables"] = 500
	rep := w.run(t, w.input(false))
	s := step(t, rep, "writer-variable")
	if s.Status != StatusFail || !strings.Contains(s.Detail, "the token is not printed") || rep.ExitCode() != ExitFailed {
		t.Errorf("writer-variable %s %q, exit %d", s.Status, s.Detail, rep.ExitCode())
	}
	orphan := w.activeToken(t, rep.Writer)
	delete(w.f.failRoutes, "POST projects/:projects/variables")
	rep = w.run(t, w.input(false))
	if rep.ExitCode() != ExitOK {
		t.Errorf("exit %d: %q", rep.ExitCode(), steps(rep))
	}
	if w.f.tokenOf(orphan.value).active {
		t.Error("the token that was never stored is still active")
	}
	w.checkHub(t, rep)
}

// TestReportJSON: the JSON report has the summary of every status.
func TestReportJSON(t *testing.T) {
	w := newGLWorld(t, "18.11")
	rep := w.run(t, w.input(true))
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	summary, _ := got["summary"].(map[string]any)
	if got["schema"] != Schema || got["command"] != "setup" || len(summary) != len(Statuses) {
		t.Errorf("report %v", got)
	}
}

// TestDefaultNames: usernames from the group's path, token names from the
// hub's id.
func TestDefaultNames(t *testing.T) {
	r, wr := DefaultNames(AccountsService, "Acme/Platform.Team", 42)
	if r != "acme-platform-team-touchmark-reader" || wr != "acme-platform-team-touchmark-writer" {
		t.Errorf("%s %s", r, wr)
	}
	r, wr = DefaultNames(AccountsGroup, "acme", 42)
	if r != "touchmark-reader-hub-42" || wr != "touchmark-writer-hub-42" {
		t.Errorf("%s %s", r, wr)
	}
	if r, _ := DefaultNames(AccountsService, strings.Repeat("a/", 200), 1); len(r) > 255 {
		t.Errorf("%d characters", len(r))
	}
}

// TestGitLabWriterOnHub: under security.writer_on_hub guard a hub inside
// the group of targets is set up: the writer reaches it as a Developer,
// merges wait for a pipeline that succeeded, and the CI file is read from
// the default branch; a writer that is a Maintainer fails.
func TestGitLabWriterOnHub(t *testing.T) {
	w := newGLWorld(t, "18.11")
	inner := w.f.addProject("hub2", w.services.id, nil)
	in := w.input(false)
	in.Project, in.WriterOnHub = "acme/services/hub2", "guard"
	rep := w.run(t, in)
	wantSteps(t, rep, map[string]Status{"group": StatusOK, "writer-guard": StatusOK, "merge-checks": StatusDone, "ci-config": StatusDone})
	if d := step(t, rep, "group").Detail; !strings.Contains(d, "holds the targets and the hub") {
		t.Errorf("group %q", d)
	}
	if d := step(t, rep, "writer-guard").Detail; !strings.Contains(d, "Developer") {
		t.Errorf("writer-guard %q", d)
	}
	w.f.mu.Lock()
	settings := inner.settings
	w.f.mu.Unlock()
	if settings["ci_config_path"] != ".gitlab-ci.yml@acme/services/hub2:main" || settings["only_allow_merge_if_pipeline_succeeds"] != true ||
		settings["allow_merge_on_skipped_pipeline"] != false {
		t.Errorf("settings %v", settings)
	}
	if s := step(t, rep, "check hub-guard"); s.Status != StatusOK {
		t.Errorf("check hub-guard %s %q", s.Status, s.Detail)
	}
	// A second run changes nothing.
	rep = w.run(t, in)
	wantSteps(t, rep, map[string]Status{"merge-checks": StatusOK, "ci-config": StatusOK})

	// A custom CI file keeps its name.
	w.f.mu.Lock()
	inner.settings["ci_config_path"] = "ci/hub.yml"
	w.f.mu.Unlock()
	w.run(t, in)
	w.f.mu.Lock()
	got := inner.settings["ci_config_path"]
	w.f.mu.Unlock()
	if got != "ci/hub.yml@acme/services/hub2:main" {
		t.Errorf("ci_config_path %v", got)
	}

	// A writer that is a Maintainer of the hub fails.
	for _, u := range w.f.users {
		if u.username == rep.Writer {
			inner.members[u.id] = 40
		}
	}
	rep = w.run(t, in)
	if s := step(t, rep, "writer-guard"); s.Status != StatusFail || rep.ExitCode() != ExitFailed {
		t.Errorf("writer-guard %s %q, exit %d", s.Status, s.Detail, rep.ExitCode())
	}
}
