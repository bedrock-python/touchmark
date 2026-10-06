package distribute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestRunAdoptMultiGitter: the move from multi-gitter (see
// docs/guide/migrate.md, and the one-off rule of decide.LegacyRewritable).
// multi-gitter opened merge requests on the alias, without a marker or
// trailers, as a bot that hub.yml lists in known_authors. While
// operations.yml holds adopt_unmarked:
//   - an open one whose branch holds only versions the hub shipped, or
//     deletes the hub's content, is taken over: the first write rebuilds
//     the branch as touchmark's commit with trailers on the default branch
//     and adds the marker, and the next run writes nothing;
//   - a closed one is no decline, whatever it carried: its content is
//     unknown, and a new pull request opens on the sync branch;
//   - an open one by someone else is not taken over, and its branch is not
//     touched;
//   - an open one whose branch holds something the hub never shipped is
//     not taken over: blocked:marker-invalid, nothing written.
//
// Without the entry every open one of the bot is marker-invalid.
func TestRunAdoptMultiGitter(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t)
	// The fake's git server lets the writer move the branch of an open pull
	// request by a known author only.
	w.p.SetKnownAuthors(w.writer, w.known)
	protoBody := "Synced by multi-gitter."
	unmarked := func(r platform.Repo, by platform.Account) int64 {
		return w.pr(r, platform.PR{Head: alias, Author: by, Title: "chore: sync engineering assets", Body: protoBody})
	}

	// An older version of the hub's.
	proto := w.optedIn("acme/proto", nil)
	protoHead := w.push(proto, alias, w.known, "AGENTS.md", agentsV1)
	nProto := unmarked(proto, w.known)
	// The branch deletes a file the hub ships, which the target holds.
	deletes := w.optedIn("acme/proto-deletes", nil, "docs/guide.md", guideV1)
	w.push(deletes, alias, w.known, "docs/guide.md", "")
	nDeletes := unmarked(deletes, w.known)
	// Closed by a person, with exactly what the hub proposes now.
	closed := w.optedIn("acme/proto-closed", nil)
	closedHead := w.push(closed, alias, w.known, baseFiles...)
	nClosed := unmarked(closed, w.known)
	w.p.SetPRState(closed.ID, nClosed, platform.Closed, &w.person, time.Time{})
	// Someone else's, on the alias, with a version of the hub's.
	person := w.optedIn("acme/proto-person", nil)
	personHead := w.push(person, alias, w.person, "AGENTS.md", agentsV1)
	nPerson := unmarked(person, w.person)
	// The bot's, but the branch holds a file the hub never shipped.
	edited := w.optedIn("acme/proto-edited", nil)
	editedHead := w.push(edited, alias, w.known, "AGENTS.md", agentsV1, "notes.md", "a person's notes\n")
	nEdited := unmarked(edited, w.known)
	w.ok()

	rep := w.both(nil)
	want(t, rep, "gh:acme/proto", report.OutcomeBlocked, "marker-invalid", nProto)
	want(t, rep, "gh:acme/proto-deletes", report.OutcomeBlocked, "marker-invalid", nDeletes)
	want(t, rep, "gh:acme/proto-edited", report.OutcomeBlocked, "marker-invalid", nEdited)
	for _, path := range []string{"acme/proto-closed", "acme/proto-person"} {
		if tg := targetOf(t, rep, "gh:"+path); tg.Outcome != report.OutcomeOpened {
			t.Errorf("%s without adopt_unmarked: %s:%s, want opened", path, tg.Outcome, tg.Reason)
		}
	}

	ops := &config.Operations{Version: 1, AdoptUnmarked: &config.UntilOp{Until: "2099-12-31"}}
	plan := w.both(func(d *Deps) { d.Write.Operations = ops })
	rep = w.distribute(func(d *Deps) { d.Write.Operations = ops })
	for i := range rep.Targets {
		a, b := plan.Targets[i], rep.Targets[i]
		if a.Path != b.Path || a.Outcome != b.Outcome || a.Reason != b.Reason {
			t.Errorf("plan said %s %s:%s, distribute did %s %s:%s", a.Path, a.Outcome, a.Reason, b.Path, b.Outcome, b.Reason)
		}
	}

	// Step 3: taken over on the alias, rebuilt with our trailers, marked.
	want(t, rep, "gh:acme/proto", report.OutcomeUpdated, "content", nProto)
	want(t, rep, "gh:acme/proto-deletes", report.OutcomeUpdated, "content", nDeletes)
	for _, c := range []struct {
		repo platform.Repo
		n    int64
		old  string
	}{{proto, nProto, protoHead}, {deletes, nDeletes, ""}} {
		pr := w.p.PR(c.repo.ID, c.n)
		if pr.State != platform.Open || pr.Head != alias || pr.Author.ID != w.known.ID {
			t.Errorf("%s #%d: %s on %s by %s", c.repo.Path, c.n, pr.State, pr.Head, pr.Author.Login)
		}
		if _, st := marker.Find(pr.Body, []string{hubFP}); st != marker.Found {
			t.Errorf("%s #%d: no marker of the hub after the first write:\n%s", c.repo.Path, c.n, pr.Body)
		}
		head := w.p.Branch(c.repo.ID, alias)
		if head == "" || head == c.old {
			t.Errorf("%s: the alias is at %q, not rebuilt", c.repo.Path, head)
		}
		msg, parent := commitOf(t, w, c.repo, head)
		tr, ok := decide.ParseTrailers(msg)
		if !ok || tr.Fingerprint != hubFP || tr.HubID != "acme-eng" || tr.Stream != decide.StreamSync || tr.Content == "" {
			t.Errorf("%s: the alias's head %s has trailers %+v (%v):\n%s", c.repo.Path, short(head), tr, ok, msg)
		}
		if m, _ := marker.Find(pr.Body, []string{hubFP}); m.Key != tr.Content {
			t.Errorf("%s: marker key %s, commit content %s", c.repo.Path, m.Key, tr.Content)
		}
		if b := w.p.Head(c.repo.ID); parent != b {
			t.Errorf("%s: our commit's parent %s is not the default branch's head %s", c.repo.Path, short(parent), short(b))
		}
		if w.p.Branch(c.repo.ID, branch) != "" {
			t.Errorf("%s: the sync branch was created; the adopted pull request lives on the alias", c.repo.Path)
		}
	}

	// Step 4: a closed one is no decline; the new pull request opens on
	// the sync branch and the old branch stays.
	tg := targetOf(t, rep, "gh:acme/proto-closed")
	if tg.Outcome != report.OutcomeOpened || tg.PR == nil || tg.PR.Number == nClosed {
		t.Errorf("proto-closed: %s:%s %+v, want a new pull request", tg.Outcome, tg.Reason, tg.PR)
	} else if pr := w.p.PR(closed.ID, tg.PR.Number); pr.Head != branch || pr.Author.ID != w.writer.ID {
		t.Errorf("proto-closed: the new pull request is on %s by %s", pr.Head, pr.Author.Login)
	}
	if head := w.p.Branch(closed.ID, alias); head != closedHead {
		t.Errorf("proto-closed: the alias moved from %s to %s", short(closedHead), short(head))
	}
	if pr := w.p.PR(closed.ID, nClosed); pr.State != platform.Closed || pr.Body != protoBody {
		t.Errorf("proto-closed: #%d is %s with body %q", nClosed, pr.State, pr.Body)
	}

	// Someone else's is left alone; touchmark opens its own.
	tg = targetOf(t, rep, "gh:acme/proto-person")
	if tg.Outcome != report.OutcomeOpened || tg.PR == nil || tg.PR.Number == nPerson {
		t.Errorf("proto-person: %s:%s %+v, want a new pull request", tg.Outcome, tg.Reason, tg.PR)
	}
	if pr := w.p.PR(person.ID, nPerson); pr.State != platform.Open || pr.Body != protoBody || w.p.Branch(person.ID, alias) != personHead {
		t.Errorf("proto-person: #%d is %s, body %q, alias %s (was %s)", nPerson, pr.State, pr.Body, short(w.p.Branch(person.ID, alias)), short(personHead))
	}

	// A branch with something the hub never shipped is not rewritable.
	want(t, rep, "gh:acme/proto-edited", report.OutcomeBlocked, "marker-invalid", nEdited)
	if pr := w.p.PR(edited.ID, nEdited); pr.Body != protoBody || w.p.Branch(edited.ID, alias) != editedHead {
		t.Errorf("proto-edited: #%d body %q, alias %s (was %s)", nEdited, pr.Body, short(w.p.Branch(edited.ID, alias)), short(editedHead))
	}
}

// commitOf returns the message and first parent of commit in r's
// repository on the fake's side.
func commitOf(t *testing.T, w *gitWorld, r platform.Repo, commit string) (msg, parent string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := gitx.New(w.p.GitDir(r.ID))
	g.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + cfg}
	out, err := g.Run(t.Context(), nil, "log", "-1", "--format=%P%x00%B", commit)
	if err != nil {
		t.Fatalf("git log %s: %v", commit, err)
	}
	parents, msg, _ := strings.Cut(string(out), "\x00")
	parent, _, _ = strings.Cut(strings.TrimSpace(parents), " ")
	return msg, parent
}
