//go:build e2e

package gitlabe2e

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
)

// legacyBranch is the branch a hub that delivered with multi-gitter opened
// its merge requests on.
const legacyBranch = "chore/sync-engineering-assets"

// TestMigration is the move from a hub that delivered with multi-gitter,
// built synthetically. adopt_unmarked lets touchmark rewrite a branch
// without its commit when every path the branch changes holds a version
// the hub once delivered, or deletes the hub's content. The old hub is
// played by a group access token, whose bot opened merge requests without
// a marker or trailers on chore/sync-engineering-assets, with the content
// the hub delivers:
//
//   - legacy: the bot's open merge request, whose branch holds exactly the
//     hub's files on top of main;
//   - closed: the bot's merge request that a person closed without a
//     merge.
//
// Then the old hub's token is revoked: the bot must still be found, and
// its merge request adopted.
//
// migrate --from-multi-gitter reads the old hub's multi-gitter
// configuration with the reader's token and prints hub.yml (the gitlab
// provider, the branch an alias, the bot a known author, found through the
// merge requests), targets.yml and operations.yml (adopt_unmarked). The hub
// takes them as they are, adding only its packs. Then distribute:
//
//   - adopts legacy's merge request: the same one stays open on the alias,
//     its branch gets touchmark's commit, its body the marker;
//   - opens a new merge request in closed on touchmark/<id>: a closed merge
//     request without a marker is no decline: its content is unknown;
//   - writes nothing in a second run;
//   - and remembers: a person closes the adopted merge request, and the
//     next run takes it for a decline.
func TestMigration(t *testing.T) {
	e := needLive(t)
	suffix := randHex(t, 3)
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{
		t: t, e: e,
		id:       "mig-" + suffix,
		topic:    "touchmark-mig-" + suffix,
		fp:       fmt.Sprintf("%s/%d", e.Host, n.Int64()+1),
		provider: "gl", // until migrate names it
		repos:    map[string]string{}, ids: map[string]int64{},
		packs: map[string]string{
			"packs/base/AGENTS.md":     text("base AGENTS.md v1"),
			"packs/base/docs/guide.md": text("base guide v1"),
		},
		dir: t.TempDir(),
	}
	s.branch = "touchmark/" + s.id
	s.aliases = []string{legacyBranch}

	// The old hub's group: a subgroup of the seeded group, so that the
	// reader and the writer see it whatever they are (the bot of a group
	// access token is a member of its own group only), with a group access
	// token of its own, the old hub's.
	fx := newGroup(t, e, "mig")
	proto := e.groupAccessToken(t, fx.nsID, "multi-gitter", levelDeveloper, []string{"api", "write_repository"})
	finding(t, "multi-gitter-bot", "the old hub's group access token acts as %s (id %d, bot %v, email %s)",
		proto.Login, proto.ID, proto.Bot, proto.Email)

	delivered := map[string]string{
		"AGENTS.md":     s.packs["packs/base/AGENTS.md"],
		"docs/guide.md": s.packs["packs/base/docs/guide.md"],
	}
	for _, name := range []string{"legacy", "closed"} {
		p := fx.createProject(t, fx.nsID, s.id+"-"+name, fileList(map[string]string{
			"README.md": "# " + name + "\n", config.DefaultOptIn: "version: 1\n",
		}))
		e.waitAccess(t, p.ID, e.Reader, e.Writer, e.Person, proto.account)
		s.repos[name], s.ids[name] = p.PathWithNamespace, p.ID
		e.api(e.Root).ok(t, http.MethodPut, fmt.Sprintf("/projects/%d", p.ID), map[string]any{"topics": []string{s.topic}}, nil)
		// The old hub's commit: the hub's files, by the bot, without
		// touchmark's trailers.
		e.commitFiles(t, proto.account, p.ID, legacyBranch, "main", fileList(delivered), "chore: sync engineering assets")
		var mr apiMR
		e.api(proto.account).ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/merge_requests", p.ID), map[string]any{
			"source_branch": legacyBranch, "target_branch": "main", "title": "chore: sync engineering assets",
			"description": multiGitterBody,
		}, &mr)
		if mr.IID != 1 || mr.Author.ID != proto.ID {
			t.Fatalf("the old hub's merge request in %s: !%d by %s", name, mr.IID, mr.Author.Username)
		}
	}
	e.setState(t, e.Person, s.ids["closed"], 1, "close")
	// The old hub's token is revoked before the move, and its bot must still
	// be found: migrate finds the bot through its merge
	// requests and checks it, and distribute adopts its merge request.
	e.revokeGroupToken(t, fx.nsID, proto)

	// migrate prints the hub's configuration; the hub takes it as it is.
	files := s.migrate(fx.ns, proto)
	s.hub = t.TempDir()
	gitCmd(t, s.hub, nil, nil, "init", "-q", "-b", "master")
	files[config.TargetsFile] += "defaults:\n  packs: [base]\n"
	for p, content := range s.packs {
		files[p] = content
	}
	s.writeHub(files, "the hub after migrate")

	before := s.mr("legacy", 1)
	planned := outcomes(s.plan())
	rep := s.distribute(0)
	got := outcomes(rep)
	// plan cannot know the number of a merge request distribute opens.
	opened := map[string]string{}
	for k, v := range got {
		if strings.HasPrefix(v, "opened:") {
			v, _, _ = strings.Cut(v, " #")
		}
		opened[k] = v
	}
	if !sameMap(planned, opened) {
		t.Errorf("plan %v, distribute %v: plan must decide as distribute does", planned, got)
	}
	finding(t, "migration-outcomes", "legacy %q, closed %q", got[s.repos["legacy"]], got[s.repos["closed"]])
	if o := got[s.repos["legacy"]]; !strings.HasPrefix(o, "updated:") || !strings.HasSuffix(o, " #1") {
		t.Errorf("legacy: %q, want the old hub's !1 adopted and updated (%s)", o, render(rep))
	}
	if o := got[s.repos["closed"]]; o != "opened: #2" {
		t.Errorf("closed: %q, want opened: #2 (a closed merge request without a marker is no decline)", o)
	}
	s.wantOps("the migration", rep, map[string][]string{
		"legacy": {"push", "edit-pr #1"},
		"closed": {"push", "create-pr #2"},
	})

	// The adopted merge request: the same one, open, on the alias, with the
	// marker; its branch rebuilt as touchmark's commit on main.
	// GitLab moves the merge request's head from the push's background job
	// (MergeRequests::RefreshService), seconds after the push.
	after := e.waitMR(t, s.ids["legacy"], 1, time.Minute, func(m apiMR) bool { return m.SHA != before.SHA })
	m, status := marker.Find(after.Description, []string{s.fp})
	switch {
	case after.State != "opened" || after.SourceBranch != legacyBranch || after.Author.ID != proto.ID:
		t.Errorf("legacy !1 after the adoption: %s on %s by %s, want opened on %s by the old hub's bot", after.State, after.SourceBranch, after.Author.Username, legacyBranch)
	case status != marker.Found || m.Data.Hub != s.id:
		t.Errorf("legacy !1 after the adoption: marker %s (hub %q) in:\n%s", status, m.Data.Hub, after.Description)
	case after.SHA == before.SHA:
		t.Errorf("legacy !1: the branch still ends in the old hub's commit %s", before.SHA)
	}
	s.checkCommit("legacy", after)
	for f, content := range delivered {
		if got := s.file("legacy", legacyBranch, f); got != content {
			t.Errorf("legacy: %s on %s is %q, want %q", f, legacyBranch, got, content)
		}
	}
	fresh := s.mr("closed", 2)
	if fresh.SourceBranch != s.branch || fresh.Author.ID != e.Writer.ID || fresh.State != "opened" {
		t.Errorf("closed !2: %s from %s by %s, want opened from %s by the writer", fresh.State, fresh.SourceBranch, fresh.Author.Username, s.branch)
	}
	if old := s.mr("closed", 1); old.State != "closed" || old.Description != multiGitterBody || len(s.e.notes(t, s.ids["closed"], 1)) != 0 {
		t.Errorf("closed !1, the old hub's closed merge request, was touched: %s", old.State)
	}

	// A second run writes nothing.
	unchanged := s.stableSnapshot()
	if quiet := s.distribute(0); len(quiet.Ops) > 0 {
		t.Errorf("a second run after the migration wrote: %s", render(quiet))
	}
	if now := s.snapshot(); !sameMap(unchanged, now) {
		t.Errorf("a second run after the migration changed GitLab:\nbefore %v\nafter  %v", unchanged, now)
	}

	// Memory works for the adopted merge request: a person closes it, and
	// the next run takes it for a decline (one ack and one note).
	e.setState(t, e.Person, s.ids["legacy"], 1, "close")
	rep = s.settle("the adopted merge request declined", map[string]string{"legacy": "declined: #1", "closed": "unchanged: #2"})
	s.wantOps("the adopted merge request declined", rep, map[string][]string{"legacy": {"edit-pr #1", "comment #1"}})
	s.checkDeclined("legacy", 1)
	s.scanPlatform()
}

// The description of the old hub's merge requests: no touchmark comment at
// all.
const multiGitterBody = "Synced by multi-gitter from engineering-assets."

// migrate runs migrate --from-multi-gitter with the old hub's multi-gitter
// configuration for group, as a maintainer does before the move, with the
// reader's token, and returns the files it prints by name. It checks that
// hub.yml has the old hub's branch among branch_aliases and its bot, found
// through the merge requests, among known_authors, and that operations.yml
// turns adopt_unmarked on.
func (s *scenario) migrate(group string, proto bot) map[string]string {
	s.t.Helper()
	cfg := filepath.Join(s.dir, "multi-gitter.yml")
	content := fmt.Sprintf("platform: gitlab\nbase-url: %s\ngroup:\n  - %s\ninclude-subgroups: true\nskip-forks: true\n"+
		"topic:\n  - %s\nbranch: %s\npr-title: \"chore: sync engineering assets\"\ncommit-message: \"chore: sync engineering assets\"\n",
		s.e.URL, group, s.topic, legacyBranch)
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
	// The short form: migrate names the provider after the host, and a hub
	// with one provider takes TOUCHMARK_READ_TOKEN too.
	out := s.run(0, map[string]string{"TOUCHMARK_READ_TOKEN": s.e.Reader.Token},
		"migrate", "--from-multi-gitter", cfg, "--id", s.id, "--writer", s.e.Writer.Login)
	finding(s.t, "migrate", "migrate --from-multi-gitter printed:\n%s", out)
	files := map[string]string{}
	name := ""
	for _, line := range strings.SplitAfter(out, "\n") {
		if strings.HasPrefix(line, "# ==> ") && strings.HasSuffix(strings.TrimSpace(line), " <==") {
			name = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "# ==> "), " <==")
			continue
		}
		if name != "" {
			files[name] += line
		}
	}
	hub, ops := files[config.HubFile], files[config.OperationsFile]
	if m := providerID.FindStringSubmatch(hub); m != nil {
		s.provider = m[1]
	}
	switch {
	case hub == "" || files[config.TargetsFile] == "" || ops == "":
		s.t.Fatalf("migrate printed no hub.yml, targets.yml and operations.yml:\n%s", out)
	case !strings.Contains(hub, legacyBranch):
		s.t.Errorf("migrate's hub.yml has no alias %s:\n%s", legacyBranch, hub)
	case !strings.Contains(hub, proto.Login) || strings.Contains(hub, "not checked"):
		s.t.Errorf("migrate's hub.yml does not name the old hub's bot %s as a checked known author:\n%s", proto.Login, hub)
	case !strings.Contains(ops, "adopt_unmarked"):
		s.t.Errorf("migrate's operations.yml does not adopt:\n%s", ops)
	}
	return files
}

// providerID finds the id of the first provider of a hub.yml.
var providerID = regexp.MustCompile(`(?m)^providers:\s*\n\s*- id: "?([a-z][a-z0-9-]*)"?\s*$`)

// writeHub writes files (path → content) into the hub, replacing all it
// had but .git, and commits them.
func (s *scenario) writeHub(files map[string]string, msg string) {
	s.t.Helper()
	for p, content := range files {
		full := filepath.Join(s.hub, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	gitCmd(s.t, s.hub, nil, nil, "add", "-A")
	gitCmd(s.t, s.hub, nil, nil, "commit", "-q", "-m", msg)
}

// bot is the bot user of a group access token, with the token.
type bot struct {
	account
	Bot   bool
	Email string
}

// revokeGroupToken revokes the group access token of the group with the id
// whose bot is b, as root.
func (e *liveEnv) revokeGroupToken(t testing.TB, group int64, b bot) {
	t.Helper()
	var toks []struct {
		ID     int64 `json:"id"`
		UserID int64 `json:"user_id"`
	}
	e.api(e.Root).get(t, fmt.Sprintf("/groups/%d/access_tokens", group), &toks)
	for _, x := range toks {
		if x.UserID == b.ID {
			e.api(e.Root).ok(t, http.MethodDelete, fmt.Sprintf("/groups/%d/access_tokens/%d", group, x.ID), nil, nil)
			return
		}
	}
	t.Fatalf("the group access token of %s is not listed in group %d", b.Login, group)
}

// groupAccessToken creates a group access token of the group with the id
// as root and returns its bot user with the token.
func (e *liveEnv) groupAccessToken(t testing.TB, group int64, name string, level int, scopes []string) bot {
	t.Helper()
	var tok struct {
		UserID int64  `json:"user_id"`
		Token  string `json:"token"`
	}
	e.api(e.Root).ok(t, http.MethodPost, fmt.Sprintf("/groups/%d/access_tokens", group), map[string]any{
		"name": name, "scopes": scopes, "access_level": level, "expires_at": time.Now().AddDate(0, 0, 7).Format("2006-01-02"),
	}, &tok)
	var u apiUser
	e.api(e.Root).get(t, fmt.Sprintf("/users/%d", tok.UserID), &u)
	e.Redact.Add(tok.Token, "oauth2", "gitlab-ci-token", u.Username)
	return bot{account: account{Login: u.Username, Token: tok.Token, ID: u.ID}, Bot: u.Bot, Email: u.Email}
}
