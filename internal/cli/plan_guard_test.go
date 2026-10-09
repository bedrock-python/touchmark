package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// auditReader is a GitLab reader that answers Lookup by login and
// MergeRequestPushes with pushes.
type auditReader struct {
	platform.Reader
	accounts map[string]platform.Account
	pushes   platform.MergeRequestPushes
	asked    int
}

func (r *auditReader) Lookup(_ context.Context, login string) (platform.Account, error) {
	a, ok := r.accounts[login]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

func (r *auditReader) MergeRequestPushes(_ context.Context, hub platform.Repo, number int64) (platform.MergeRequestPushes, error) {
	r.asked++
	if hub.ID != "21" || number != 5 {
		return platform.MergeRequestPushes{}, errors.New("another merge request")
	}
	return r.pushes, nil
}

// guardHubRepo is a hub whose default branch master says writer_on_hub
// mainMode, checked out on a branch that says branchMode.
func guardHubRepo(t *testing.T, mainMode, branchMode string) *hub {
	t.Helper()
	hubYML := func(mode string) string {
		return "version: 1\nid: acme-eng\nproviders:\n  - id: corp\n    type: gitlab\n    url: https://gitlab.example.com\n" +
			"    writer: writer-bot\n    known_authors: [old-bot]\nsecurity:\n  writer_on_hub: " + mode + "\n"
	}
	r := newHub(t)
	r.write(config.HubFile, hubYML(mainMode))
	r.commit("hub")
	r.git("checkout", "-q", "-b", "feature")
	r.write(config.HubFile, hubYML(branchMode))
	r.commit("feature")
	e := &env{getenv: func(string) string { return "" }, stdout: os.Stdout, stderr: os.Stderr, getwd: os.Getwd}
	h, err := openHub(t.Context(), e, r.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if errs := h.readConfigs(t.Context(), e); len(errs) > 0 {
		t.Fatal(errs)
	}
	return h
}

// TestWriterMergeGuard: plan in a GitLab merge request pipeline of a hub
// whose default branch says writer_on_hub guard refuses (exit 2) a merge
// request the writer or a known author opened or pushed to, and one whose
// pushes it cannot tell; the merge request's own hub.yml cannot turn the
// guard off.
func TestWriterMergeGuard(t *testing.T) {
	mr := hubch.Context{CI: hubch.GitLabCI, Event: "merge_request_event", Host: "gitlab.example.com", RepoID: "21",
		RepoPath: "acme/engineering-assets", DefaultBranch: "master"}
	getenv := func(k string) string {
		if k == "CI_MERGE_REQUEST_IID" {
			return "5"
		}
		return ""
	}
	accounts := map[string]platform.Account{"writer-bot": {ID: "7", Login: "writer-bot"}, "old-bot": {ID: "8", Login: "old-bot"}}
	person := platform.Account{ID: "3", Login: "jdoe"}
	pps := []planProvider{{ResolvedProvider: config.ResolvedProvider{Provider: config.Provider{ID: "corp", Type: "gitlab",
		Writer: "writer-bot", KnownAuthors: []string{"old-bot"}}, Host: "gitlab.example.com"}}}
	run := func(h *hub, c hubch.Context, pushes platform.MergeRequestPushes) (*auditReader, error) {
		r := &auditReader{accounts: accounts, pushes: pushes}
		_, err := h.writerMergeGuard(t.Context(), c, getenv, pps, []distribute.Provider{{Reader: r}})
		return r, err
	}
	clean := platform.MergeRequestPushes{Author: person, Branch: "feature", Pushers: []platform.Account{person}, Complete: true}
	for _, tc := range []struct {
		name   string
		main   string
		branch string
		pushes platform.MergeRequestPushes
		want   string
	}{
		{"clean", "guard", "guard", clean, ""},
		{"opened by the writer", "guard", "guard", platform.MergeRequestPushes{Author: platform.Account{ID: "7"}, Branch: "feature", Complete: true},
			"was opened by writer-bot"},
		{"a known author pushed", "guard", "guard", platform.MergeRequestPushes{Author: person, Branch: "feature",
			Pushers: []platform.Account{person, {ID: "8"}}, Complete: true}, "old-bot, the writer or one of its known authors, pushed to feature"},
		{"pushes unknown", "guard", "guard", platform.MergeRequestPushes{Author: person, Branch: "feature", Why: "no creation"}, "fails closed"},
		// The merge request's own hub.yml cannot turn the guard off.
		{"turned off in the merge request", "guard", "refuse", platform.MergeRequestPushes{Author: platform.Account{ID: "7"}, Complete: true},
			"was opened by"},
		{"off on the default branch", "refuse", "guard", platform.MergeRequestPushes{Author: platform.Account{ID: "7"}, Complete: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := guardHubRepo(t, tc.main, tc.branch)
			_, err := run(h, mr, tc.pushes)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("error %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error %v, want %q", err, tc.want)
			case tc.want != "":
				var ce *cliError
				if !errors.As(err, &ce) || ce.code != exitUsage {
					t.Errorf("error %v: want exit %d", err, exitUsage)
				}
			}
		})
	}
	h := guardHubRepo(t, "guard", "guard")
	// Not a merge request pipeline: nothing is asked.
	for _, c := range []hubch.Context{{CI: hubch.GitLabCI, Event: "push"}, {CI: hubch.GitHubActions, Event: "pull_request"}} {
		if r, err := run(h, c, clean); err != nil || r.asked != 0 {
			t.Errorf("%+v: %v, asked %d", c, err, r.asked)
		}
	}
	// A reader without the answer, or without a token, fails closed.
	if _, err := h.writerMergeGuard(t.Context(), mr, getenv, pps, []distribute.Provider{{Reader: offlineReader{}}}); err == nil ||
		!strings.Contains(err.Error(), "no reader with a token") {
		t.Errorf("no auditor: %v", err)
	}
	anon := []planProvider{pps[0]}
	anon[0].anonymous = true
	if _, err := h.writerMergeGuard(t.Context(), mr, getenv, anon, []distribute.Provider{{Reader: &auditReader{accounts: accounts, pushes: clean}}}); err == nil {
		t.Error("an anonymous provider passed")
	}
}
