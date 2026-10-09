package gitlab

import (
	"net/http"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// pushEvent is a push event of GET /projects/:id/events.
func pushEvent(author int64, username, action, ref string) map[string]any {
	return map[string]any{
		"id": author*100 + int64(len(ref)), "project_id": 21, "action_name": "pushed to", "target_type": nil, "author_id": author,
		"author_username": username, "created_at": "2026-10-09T10:00:00.000Z",
		"push_data": map[string]any{"commit_count": 1, "action": action, "ref_type": "branch", "ref": ref, "commit_title": "x"},
	}
}

func TestMergeRequestPushes(t *testing.T) {
	audit := func(t *testing.T, events []any, mrOpts map[string]any) platform.MergeRequestPushes {
		t.Helper()
		fx := newFixture(t)
		mr := map[string]any{"iid": 5, "source_branch": "feature", "source_project_id": 21, "target_branch": "main",
			"author": basic(3, "jdoe")}
		for k, v := range mrOpts {
			mr[k] = v
		}
		fx.json(http.MethodGet, "/projects/21/merge_requests/5", http.StatusOK, mr)
		if events != nil {
			fx.pages("/projects/21/events", events)
		} else {
			fx.json(http.MethodGet, "/projects/99/events", http.StatusNotFound, msg("404 Project Not Found"))
		}
		var auditor platform.MergeRequestAuditor = fx.reader
		got, err := auditor.MergeRequestPushes(t.Context(), platform.Repo{ID: "21", Path: "acme/engineering-assets"}, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range fx.requests(http.MethodGet, "/projects/21/events") {
			if c.Query.Get("action") != "pushed" || c.Query.Get("sort") != "desc" {
				t.Errorf("events asked with %v", c.Query)
			}
		}
		return got
	}
	logins := func(as []platform.Account) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.ID+":"+a.Login)
		}
		return out
	}

	// Newest first: the writer pushed after the person created the branch;
	// pushes to other branches and before the creation do not count.
	got := audit(t, []any{
		pushEvent(9, "writer", "pushed", "other"),
		pushEvent(7, "group_9_bot_writer", "pushed", "feature"),
		pushEvent(3, "jdoe", "pushed", "feature"),
		pushEvent(3, "jdoe", "created", "feature"),
		pushEvent(8, "someone", "pushed", "feature"),
	}, nil)
	if !got.Complete || got.Author.ID != "3" || got.Branch != "feature" {
		t.Errorf("answer %+v", got)
	}
	if l := logins(got.Pushers); len(l) != 2 || l[0] != "7:group_9_bot_writer" || l[1] != "3:jdoe" {
		t.Errorf("pushers %q", l)
	}

	// The creation is not among the events: not complete.
	got = audit(t, []any{pushEvent(3, "jdoe", "pushed", "feature")}, nil)
	if got.Complete || got.Why == "" {
		t.Errorf("without the creation: %+v", got)
	}

	// A source project the reader cannot see: not complete.
	got = audit(t, nil, map[string]any{"source_project_id": 99})
	if got.Complete || got.Why == "" {
		t.Errorf("an unseen fork: %+v", got)
	}
}
