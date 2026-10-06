package ghfake

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// push runs git push --porcelain in dir and classifies it like gitx.
func push(t *testing.T, env []string, dir string, args ...string) gitx.PushResult {
	t.Helper()
	out, errOut, _ := git(t, env, dir, append([]string{"push", "--porcelain"}, args...)...)
	return gitx.ParsePush(out, errOut)
}

func TestGitAccess(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	w.repo(RepoSpec{Owner: "acme", Name: "secret", Visibility: "private", Files: []File{{Path: "s", Content: []byte("s")}}})
	clone(t, w.s, "acme/api", "")
	env := gitEnvFor(t, w.s, "")
	if _, errOut, err := git(t, env, "", "ls-remote", w.s.CloneURL("acme/secret")); err == nil {
		t.Errorf("anonymous ls-remote of a private repository worked")
	} else if !strings.Contains(errOut, "Repository not found.") {
		t.Errorf("anonymous private: %s", errOut)
	}
	tok := w.token([]string{"secret"}, Permissions{"contents": Read})
	dir, tenv := clone(t, w.s, "acme/secret", tok)
	commitFile(t, tenv, dir, "x.txt", "x\n", "x")
	res := push(t, tenv, dir, "origin", "HEAD:refs/heads/x")
	if res.Status != gitx.PushPermission || !strings.Contains(res.Message, "Permission to acme/secret.git denied to hub-writer[bot].") {
		t.Errorf("read-only push: %+v", res)
	}
	// Renamed repositories redirect git too (assumed).
	must[Repo](t)(w.s.RenameRepo("acme/api", "service"))
	if _, errOut, err := git(t, env, "", "ls-remote", w.s.CloneURL("acme/api")); err != nil {
		t.Errorf("ls-remote through the redirect: %v %s", err, errOut)
	}
	w.noViolations()
}

func TestGitPushFollowsPRs(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	base := mustGit(t, env, dir, "rev-parse", "HEAD")
	c1 := commitFile(t, env, dir, "AGENTS.md", "v1\n", "sync")
	if res := push(t, env, dir, "--force-with-lease=refs/heads/touchmark/hub:", "origin", "HEAD:refs/heads/touchmark/hub"); res.Status != gitx.PushOK {
		t.Fatalf("push: %+v", res)
	}
	pr := w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "sync", "head": "touchmark/hub", "base": "main"}).obj(t)
	n := int64(pr["number"].(float64))
	if out := mustGit(t, env, dir, "ls-remote", "origin", "refs/pull/*"); !strings.Contains(out, c1+"\trefs/pull/"+itoa(n)+"/head") {
		t.Errorf("refs/pull: %q", out)
	}
	// A rebuild on the base moves the head; the pull request stays open.
	mustGit(t, env, dir, "reset", "-q", "--hard", base)
	c2 := commitFile(t, env, dir, "AGENTS.md", "v2\n", "sync again")
	if res := push(t, env, dir, "--force-with-lease=refs/heads/touchmark/hub:"+c1, "origin", "HEAD:refs/heads/touchmark/hub"); res.Status != gitx.PushOK {
		t.Fatalf("force push: %+v", res)
	}
	got, _ := w.s.GetPR("acme/api", n)
	if got.State != "open" || got.HeadSHA != c2 || got.Events[len(got.Events)-1].Type != EventHeadRefForced {
		t.Errorf("after the rebuild: %+v", got)
	}
	// A stale lease is refused by git itself.
	if res := push(t, env, dir, "--force-with-lease=refs/heads/touchmark/hub:"+c1, "origin", "HEAD:refs/heads/touchmark/hub"); res.Status != gitx.PushStale && res.Status != gitx.PushUpToDate {
		t.Errorf("stale lease: %+v", res)
	}
	w.noViolations()

	// Moving the head to its base closes the pull request (observed).
	if res := push(t, env, dir, "--force", "origin", base+":refs/heads/touchmark/hub"); res.Status != gitx.PushOK {
		t.Fatalf("head to base: %+v", res)
	}
	got, _ = w.s.GetPR("acme/api", n)
	if got.State != "closed" || got.Merged || got.ClosedBy == nil || got.ClosedBy.Login != "hub-writer[bot]" {
		t.Errorf("head at base: %+v", got)
	}
	v := w.s.Violations()
	if len(v) != 1 || !strings.HasPrefix(v[0], "head-to-base acme/api#"+itoa(n)) {
		t.Errorf("violations %q", v)
	}

	// A new pull request, then its branch deleted by a push.
	mustGit(t, env, dir, "reset", "-q", "--hard", c2)
	push(t, env, dir, "--force", "origin", "HEAD:refs/heads/touchmark/hub")
	if v := w.s.Violations(); len(v) != 1 || !strings.HasPrefix(v[0], "closed-branch-push") {
		t.Errorf("push to a closed pull request's branch: %q", v)
	}
	pr = w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "sync", "head": "touchmark/hub", "base": "main"}).obj(t)
	n2 := int64(pr["number"].(float64))
	w.noViolations()
	push(t, env, dir, "origin", ":refs/heads/touchmark/hub")
	got, _ = w.s.GetPR("acme/api", n2)
	if got.State != "closed" || got.Events[len(got.Events)-2].Type != EventHeadRefDeleted {
		t.Errorf("after deletion: %+v", got)
	}
	if v := w.s.Violations(); len(v) != 1 || !strings.HasPrefix(v[0], "deleted-open-branch") {
		t.Errorf("violations %q", v)
	}
}

func TestIndirectMerge(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	head := w.branch("acme/api", "feature")
	pr := w.openPR("acme/api", PRSpec{Head: "feature", Title: "f", Author: "alice"})
	check(t, w.s.SetBranch("acme/api", "main", head, "bob"))
	got, _ := w.s.GetPR("acme/api", pr.Number)
	if !got.Merged || got.MergedBy == nil || got.MergedBy.Login != "bob" {
		t.Errorf("indirect merge: %+v", got)
	}
	w.noViolations()
}

func TestGitWorkflows(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	tok := w.token([]string{"api"}, Permissions{"contents": Write, "pull_requests": Write})
	dir, env := clone(t, w.s, "acme/api", tok)
	commitFile(t, env, dir, ".github/workflows/ci.yml", "on: push\n", "ci")
	res := push(t, env, dir, "origin", "HEAD:refs/heads/touchmark/hub")
	want := "refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission"
	if res.Status != gitx.PushWorkflows || !strings.Contains(res.Message, want) {
		t.Errorf("push without workflows: %+v", res)
	}
	if w.s.Branch("acme/api", "touchmark/hub") != "" {
		t.Error("the refused push created the branch")
	}
	// Stage refs meet the same check (assumed).
	if res := push(t, env, dir, "origin", "HEAD:refs/touchmark/0123456789abcdef/stage"); res.Status != gitx.PushWorkflows {
		t.Errorf("stage without workflows: %+v", res)
	}
	full, fenv := clone(t, w.s, "acme/api", w.token([]string{"api"}, nil))
	commitFile(t, fenv, full, ".github/workflows/ci.yml", "on: push\n", "ci")
	if res := push(t, fenv, full, "origin", "HEAD:refs/heads/touchmark/hub"); res.Status != gitx.PushOK {
		t.Errorf("push with workflows: %+v", res)
	}
	// Moving a branch across others' workflow changes needs it too
	// (modeled after reports, unverified).
	w.commit("acme/api", CommitSpec{Author: "alice", Files: []File{{Path: ".github/workflows/lint.yml", Content: []byte("on: pull_request\n")}}})
	mustGit(t, env, dir, "fetch", "-q", "origin", "main")
	mustGit(t, env, dir, "reset", "-q", "--hard", "FETCH_HEAD")
	commitFile(t, env, dir, "AGENTS.md", "x\n", "sync")
	if res := push(t, env, dir, "--force", "origin", "HEAD:refs/heads/touchmark/hub"); res.Status != gitx.PushWorkflows {
		t.Errorf("rebuild across a workflow change: %+v", res)
	}
	w.noViolations()
}

func TestGitRules(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Name: "signed", Include: []string{"refs/heads/signed/**"},
		Rules: []Rule{{Type: RuleRequiredSignatures}}}))
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Name: "no force", Include: []string{"refs/heads/touchmark/**"},
		Rules: []Rule{{Type: RuleNonFastForward}}}))
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	base := mustGit(t, env, dir, "rev-parse", "HEAD")
	c1 := commitFile(t, env, dir, "a.txt", "a\n", "unsigned")

	res := push(t, env, dir, "origin", "HEAD:refs/heads/signed/x")
	if res.Status != gitx.PushUnsigned || !strings.Contains(res.Message, "GH013: Repository rule violations found for refs/heads/signed/x.") ||
		!strings.Contains(res.Message, "Found 1 violation:") || !strings.Contains(res.Message, c1) ||
		!strings.Contains(res.Message, "push declined due to repository rule violations") {
		t.Errorf("unsigned push: %+v", res)
	}
	// A new branch is judged by the commits no other branch has: main's
	// unsigned history does not count.
	if res := push(t, env, dir, "origin", base+":refs/heads/signed/y"); res.Status != gitx.PushOK {
		t.Errorf("existing commits to a signed branch: %+v", res)
	}

	if res := push(t, env, dir, "origin", "HEAD:refs/heads/touchmark/hub"); res.Status != gitx.PushOK {
		t.Fatalf("first push: %+v", res)
	}
	mustGit(t, env, dir, "reset", "-q", "--hard", base)
	commitFile(t, env, dir, "b.txt", "b\n", "rebuilt")
	res = push(t, env, dir, "--force-with-lease=refs/heads/touchmark/hub:"+c1, "origin", "HEAD:refs/heads/touchmark/hub")
	if res.Status != gitx.PushPolicy || !strings.Contains(res.Message, "Cannot force-push to this branch") {
		t.Errorf("force push: %+v", res)
	}
	if w.s.Branch("acme/api", "touchmark/hub") != c1 {
		t.Error("the refused force push moved the branch")
	}
	// An App on the bypass list may.
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Name: "bypassed", Include: []string{"refs/heads/free/**"},
		Rules: []Rule{{Type: RuleNonFastForward}}, BypassApps: []string{"hub-writer"}}))
	push(t, env, dir, "origin", c1+":refs/heads/free/x")
	if res := push(t, env, dir, "--force", "origin", "HEAD:refs/heads/free/x"); res.Status != gitx.PushOK {
		t.Errorf("bypass: %+v", res)
	}
	w.noViolations()
}

func TestGitHiddenRefs(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	w.branch("acme/api", "feature")
	pr := w.openPR("acme/api", PRSpec{Head: "feature", Title: "f", Author: "alice"})
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	c := commitFile(t, env, dir, "x", "x\n", "x")
	if res := push(t, env, dir, "origin", "HEAD:refs/touchmark/0123456789abcdef/stage"); res.Status != gitx.PushOK {
		t.Fatalf("stage push: %+v", res)
	}
	if out := mustGit(t, env, dir, "ls-remote", "origin", "refs/touchmark/*"); !strings.HasPrefix(out, c) {
		t.Errorf("ls-remote: %q", out)
	}
	res := push(t, env, dir, "--force", "origin", "HEAD:refs/pull/"+itoa(pr.Number)+"/head")
	if res.Status == gitx.PushOK || !strings.Contains(res.Message, "hidden ref") {
		t.Errorf("push to refs/pull: %+v", res)
	}
	if got, _ := w.s.GetPR("acme/api", pr.Number); got.State != "open" {
		t.Errorf("pull request: %+v", got)
	}
	w.noViolations()
}

func TestGitTokens(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	w.repo(RepoSpec{Owner: "acme", Name: "other", Files: []File{{Path: "o", Content: []byte("o")}}})
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	commitFile(t, env, dir, "x", "x\n", "x")
	mustGit(t, env, dir, "remote", "add", "other", w.s.CloneURL("acme/other"))
	res := push(t, env, dir, "other", "HEAD:refs/heads/x")
	if res.Status != gitx.PushPermission {
		t.Errorf("push outside the token's repository: %+v", res)
	}
	if v := w.s.Violations(); len(v) == 0 || !strings.HasPrefix(v[0], "token-scope") {
		t.Errorf("violations %q", v)
	}
	w.clock.Advance(time.Hour)
	res = push(t, env, dir, "origin", "HEAD:refs/heads/x")
	if res.Status != gitx.PushAuth && res.Status != gitx.PushError {
		t.Errorf("expired token: %+v", res)
	}
	if v := w.s.Violations(); len(v) == 0 || !strings.HasPrefix(v[0], "stale-token") {
		t.Errorf("violations %q", v)
	}
	if u := w.s.Usage("installation/" + itoa(w.inst.ID)); u.Pushes != 0 {
		t.Errorf("usage %+v", u)
	}
}

// sshKey returns a fresh OpenSSH ed25519 private key and its signer.
func sshKey(t *testing.T) *sshsig.Signer {
	t.Helper()
	str := func(b, s []byte) []byte { return append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...) }
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	check(t, err)
	pubBlob := str(str(nil, []byte("ssh-ed25519")), pub)
	sec := binary.BigEndian.AppendUint32(nil, 0x0badcafe)
	sec = binary.BigEndian.AppendUint32(sec, 0x0badcafe)
	sec = str(sec, []byte("ssh-ed25519"))
	sec = str(sec, pub)
	sec = str(sec, priv)
	sec = str(sec, []byte("ghfake test"))
	for i := byte(1); len(sec)%8 != 0; i++ {
		sec = append(sec, i)
	}
	b := []byte("openssh-key-v1\x00")
	b = str(b, []byte("none"))
	b = str(b, []byte("none"))
	b = str(b, nil)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = str(b, pubBlob)
	b = str(b, sec)
	signer, err := sshsig.ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b}))
	check(t, err)
	return signer
}

// signedCommit writes an SSH-signed commit on top of parent in a clone.
func signedCommit(t *testing.T, env []string, dir, parent, email string, key *sshsig.Signer) string {
	t.Helper()
	tree := mustGit(t, env, dir, "rev-parse", "HEAD^{tree}")
	who := "carol <" + email + "> " + itoa(time.Now().Unix()) + " +0000"
	payload := "tree " + tree + "\nparent " + parent + "\nauthor " + who + "\ncommitter " + who + "\n\nsigned\n"
	sig, err := key.Sign(sshsig.Namespace, []byte(payload))
	check(t, err)
	head, _, _ := strings.Cut(payload, "\n\n")
	text := head + "\ngpgsig " + strings.ReplaceAll(sig, "\n", "\n ") + "\n\nsigned\n"
	out, errOut, err := gitStdin(t, env, dir, text, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatalf("hash-object: %v %s", err, errOut)
	}
	return strings.TrimSpace(out)
}

func TestSSHSignatures(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	must[Account](t)(w.s.AddUser("carol"))
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Include: []string{"refs/heads/signed/**"}, Rules: []Rule{{Type: RuleRequiredSignatures}}}))
	key := sshKey(t)
	check(t, w.s.AddEmail("carol", "carol@example.invalid"))
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	base := mustGit(t, env, dir, "rev-parse", "HEAD")
	unknown := signedCommit(t, env, dir, base, "carol@example.invalid", key)
	push(t, env, dir, "origin", unknown+":refs/heads/unknown")
	verification := func(sha string) map[string]any {
		return w.call("GET", "/repos/acme/api/git/commits/"+sha, tok, nil).obj(t)["verification"].(map[string]any)
	}
	if v := verification(unknown); v["verified"] != false || v["reason"] != "unknown_key" {
		t.Errorf("unregistered key: %v", v)
	}
	check(t, w.s.AddSigningKey("carol", key.PublicKey()))
	if v := verification(unknown); v["verified"] != true || v["reason"] != "valid" || v["signature"] == nil {
		t.Errorf("registered key: %v", v)
	}
	other := signedCommit(t, env, dir, base, "someone@example.invalid", key)
	push(t, env, dir, "origin", other+":refs/heads/other")
	if v := verification(other); v["reason"] != "bad_email" {
		t.Errorf("foreign email: %v", v)
	}
	if res := push(t, env, dir, "origin", unknown+":refs/heads/signed/ok"); res.Status != gitx.PushOK {
		t.Errorf("signed push: %+v", res)
	}
	if v := verification(base); v["reason"] != "unsigned" || v["signature"] != nil {
		t.Errorf("unsigned: %v", v)
	}
}

func TestStageRefFlow(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Include: []string{"refs/heads/touchmark/**"},
		Rules: []Rule{{Type: RuleRequiredSignatures}}}))
	tok := w.token([]string{"api"}, nil)
	dir, env := clone(t, w.s, "acme/api", tok)
	base := mustGit(t, env, dir, "rev-parse", "HEAD")
	// An earlier API commit on the branch with its pull request.
	first := apiCommit(t, w, tok, dir, env, base, "AGENTS.md", "v1\n")
	gql := `mutation M($input: UpdateRefsInput!) { updateRefs(input: $input) { clientMutationId } }`
	zero := strings.Repeat("0", 40)
	got := w.graphql(tok, gql, map[string]any{"input": map[string]any{"repositoryId": w.api.NodeID, "clientMutationId": "c1",
		"refUpdates": []any{map[string]any{"name": "refs/heads/touchmark/hub", "beforeOid": zero, "afterOid": first}}}})
	if field(got, "data", "updateRefs", "clientMutationId") != "c1" {
		t.Fatalf("create: %v", got)
	}
	pr := w.call("POST", "/repos/acme/api/pulls", tok, map[string]any{"title": "sync", "head": "touchmark/hub", "base": "main"}).obj(t)
	n := int64(pr["number"].(float64))

	// The rebuild: stage, API commit, one updateRefs moving the branch and
	// deleting the stage.
	mustGit(t, env, dir, "reset", "-q", "--hard", base)
	second := apiCommit(t, w, tok, dir, env, base, "AGENTS.md", "v2\n")
	stage := "refs/touchmark/0123456789abcdef/stage"
	got = w.graphql(tok, gql, map[string]any{"input": map[string]any{"repositoryId": w.api.NodeID, "refUpdates": []any{
		map[string]any{"name": "refs/heads/touchmark/hub", "beforeOid": first, "afterOid": second, "force": true},
		map[string]any{"name": stage, "afterOid": zero},
	}}})
	if got["errors"] != nil {
		t.Fatalf("updateRefs: %v", got)
	}
	p, _ := w.s.GetPR("acme/api", n)
	if p.State != "open" || p.HeadSHA != second {
		t.Errorf("pull request after the rebuild: %+v", p)
	}
	if out := mustGit(t, env, dir, "ls-remote", "origin", stage); out != "" {
		t.Errorf("stage ref left: %q", out)
	}
	w.noViolations()

	// A stale beforeOid moves nothing.
	got = w.graphql(tok, gql, map[string]any{"input": map[string]any{"repositoryId": w.api.NodeID, "refUpdates": []any{
		map[string]any{"name": "refs/heads/touchmark/hub", "beforeOid": first, "afterOid": first, "force": true},
	}}})
	if field(got, "errors", 0, "type") != "STALE_DATA" || field(got, "data", "updateRefs") != nil {
		t.Errorf("stale: %v", got)
	}
	// Without force, only fast-forwards.
	got = w.graphql(tok, gql, map[string]any{"input": map[string]any{"repositoryId": w.api.NodeID, "refUpdates": []any{
		map[string]any{"name": "refs/heads/touchmark/hub", "beforeOid": second, "afterOid": first},
	}}})
	if field(got, "errors", 0, "type") != "UNPROCESSABLE" {
		t.Errorf("non-fast-forward: %v", got)
	}
	// An unsigned commit meets the ruleset.
	unsigned := commitFile(t, env, dir, "x", "x\n", "unsigned")
	push(t, env, dir, "origin", "HEAD:"+stage)
	got = w.graphql(tok, gql, map[string]any{"input": map[string]any{"repositoryId": w.api.NodeID, "refUpdates": []any{
		map[string]any{"name": "refs/heads/touchmark/hub", "beforeOid": second, "afterOid": unsigned, "force": true},
	}}})
	if msg, _ := field(got, "errors", 0, "message").(string); !strings.Contains(msg, "Commits must have verified signatures.") {
		t.Errorf("unsigned: %v", got)
	}
	if w.s.Branch("acme/api", "touchmark/hub") != second {
		t.Error("a refused updateRefs moved the branch")
	}
	// Moving the head to the base is still a violation through the API.
	got = w.graphql(tok, gql, map[string]any{"input": map[string]any{"repositoryId": w.api.NodeID, "refUpdates": []any{
		map[string]any{"name": "refs/heads/other", "afterOid": base},
	}}})
	if got["errors"] != nil {
		t.Errorf("create other: %v", got)
	}
	w.noViolations()
	if u := w.s.Usage("installation/" + itoa(w.inst.ID)); u.ContentCreated < 5 || u.Pushes < 2 {
		t.Errorf("usage %+v", u)
	}
}

// apiCommit pushes a commit changing path to the stage ref and recreates
// it through POST /git/commits; it returns the API commit, which must be
// signed.
func apiCommit(t *testing.T, w *world, tok, dir string, env []string, parent, path, content string) string {
	t.Helper()
	local := commitFile(t, env, dir, path, content, "sync")
	if res := push(t, env, dir, "--force", "origin", "HEAD:refs/touchmark/0123456789abcdef/stage"); res.Status != gitx.PushOK {
		t.Fatalf("stage: %+v", res)
	}
	tree := mustGit(t, env, dir, "rev-parse", local+"^{tree}")
	r := w.call("POST", "/repos/acme/api/git/commits", tok, map[string]any{"message": "chore: sync\n", "tree": tree, "parents": []string{parent}})
	wantStatus(t, "API commit", r, 201)
	c := r.obj(t)
	if field(c, "verification", "verified") != true || field(c, "verification", "reason") != "valid" ||
		field(c, "author", "name") != "hub-writer[bot]" || field(c, "author", "email") != w.app.Bot.Email ||
		field(c, "committer", "email") != "noreply@github.com" || field(c, "tree", "sha") != tree {
		t.Fatalf("API commit: %v", c)
	}
	return c["sha"].(string)
}

func TestAPICommitSigning(t *testing.T) {
	needGit(t)
	for _, tc := range []struct {
		name   string
		opts   Options
		signed bool
	}{
		{"github.com", Options{}, true},
		{"GHES", Options{Flavor: GHES}, false},
		{"GHES with web commit signing", Options{Flavor: GHES, WebCommitSigning: ptr(true)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, tc.opts)
			tree := strings.TrimSpace(must[string](t)(gitOut(w.s.GitDir("acme/api"), "rev-parse", "main^{tree}")))
			parent := w.s.Branch("acme/api", "main")
			prefix := ""
			if tc.opts.Flavor == GHES {
				prefix = "/api/v3"
			}
			body := map[string]any{"message": "m", "tree": tree, "parents": []string{parent}}
			c := w.call("POST", prefix+"/repos/acme/api/git/commits", w.token(nil, nil), body).obj(t)
			if field(c, "verification", "verified") != tc.signed {
				t.Errorf("App commit: %v", c["verification"])
			}
			// Custom author information disables the signature (documented).
			body["author"] = map[string]any{"name": "x", "email": "x@example.invalid"}
			c = w.call("POST", prefix+"/repos/acme/api/git/commits", w.token(nil, nil), body).obj(t)
			if field(c, "verification", "verified") != false || field(c, "author", "name") != "x" {
				t.Errorf("custom author: %v", c)
			}
			delete(body, "author")
			check(t, w.s.Grant("acme/api", "alice", "write"))
			pat := must[string](t)(w.s.AddPAT("alice", PATSpec{Scopes: []string{"repo"}}))
			c = w.call("POST", prefix+"/repos/acme/api/git/commits", pat, body).obj(t)
			if field(c, "verification", "reason") != "unsigned" || field(c, "author", "name") != "alice" {
				t.Errorf("PAT commit: %v", c)
			}
			r := w.call("POST", prefix+"/repos/acme/api/git/commits", w.token(nil, nil), map[string]any{"message": "m", "tree": strings.Repeat("1", 40)})
			wantStatus(t, "missing tree", r, 422)
		})
	}
}

func TestForkPR(t *testing.T) {
	needGit(t)
	w := newWorld(t, Options{})
	must[Repo](t)(w.s.Fork("acme/api", "bob"))
	pat := must[string](t)(w.s.AddPAT("bob", PATSpec{Scopes: []string{"repo"}}))
	dir, env := clone(t, w.s, "bob/api", pat)
	c1 := commitFile(t, env, dir, "f", "1\n", "one")
	push(t, env, dir, "origin", "HEAD:refs/heads/touchmark/hub")
	r := w.call("POST", "/repos/acme/api/pulls", pat, map[string]any{"title": "from a fork", "head": "bob:touchmark/hub", "base": "main"})
	wantStatus(t, "fork PR", r, 201)
	pr := r.obj(t)
	if field(pr, "head", "label") != "bob:touchmark/hub" || field(pr, "head", "repo", "full_name") != "bob/api" || field(pr, "head", "sha") != c1 {
		t.Errorf("fork PR: %v", pr)
	}
	n := int64(pr["number"].(float64))
	c2 := commitFile(t, env, dir, "f", "2\n", "two")
	push(t, env, dir, "origin", "HEAD:refs/heads/touchmark/hub")
	if got, _ := w.s.GetPR("acme/api", n); got.HeadSHA != c2 {
		t.Errorf("fork head %s, want %s", got.HeadSHA, c2)
	}
	base := gitEnvFor(t, w.s, "")
	if out := mustGit(t, base, "", "ls-remote", w.s.CloneURL("acme/api"), "refs/pull/"+itoa(n)+"/head"); !strings.HasPrefix(out, c2) {
		t.Errorf("refs/pull in the base: %q", out)
	}
	if got := w.call("GET", "/repos/acme/api/pulls?state=all&head=bob:touchmark/hub", "", nil).list(t); len(got) != 1 {
		t.Errorf("head filter: %d", len(got))
	}
	if got := w.call("GET", "/repos/acme/api/pulls?state=all&head=acme:touchmark/hub", "", nil).list(t); len(got) != 0 {
		t.Errorf("head filter of the base owner: %d", len(got))
	}
}

// gitStdin runs git with stdin.
func gitStdin(t *testing.T, env []string, dir, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := gitCmd(dir, env, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// gitOut runs git in a bare repository of the fake.
func gitOut(dir string, args ...string) (string, error) {
	cmd := gitCmd(dir, nil, args...)
	out, err := cmd.Output()
	return string(out), err
}

func ptr[T any](v T) *T { return &v }
