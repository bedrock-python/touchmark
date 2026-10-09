package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxEventPages bounds the push events read looking for the creation of a
// merge request's source branch: 2 000.
const maxEventPages = 20

// apiMergeRequest is the part of GET /projects/:id/merge_requests/:iid the
// guard reads.
type apiMergeRequest struct {
	IID             int64    `json:"iid"`
	SourceBranch    string   `json:"source_branch"`
	SourceProjectID int64    `json:"source_project_id"`
	Author          *apiUser `json:"author"`
}

// apiPushEvent is one of GET /projects/:id/events?action=pushed.
type apiPushEvent struct {
	AuthorID       int64  `json:"author_id"`
	AuthorUsername string `json:"author_username"`
	PushData       *struct {
		Action  string `json:"action"` // pushed, created, removed
		RefType string `json:"ref_type"`
		Ref     string `json:"ref"`
	} `json:"push_data"`
}

// errFoundCreation ends the listing of events once the source branch's
// creation is found.
var errFoundCreation = errors.New("found the branch's creation")

// MergeRequestPushes reads merge request number of hub and who pushed to
// its source branch (platform.MergeRequestAuditor): GET
// /projects/:id/merge_requests/:iid, then the push events of the source
// project, newest first (GET /projects/:source/events?action=pushed), back
// to the event that created the branch. The events name who pushed, which a
// commit's author does not; a pipeline re-run by someone else does not hide
// them. Docs: https://docs.gitlab.com/api/merge_requests/,
// https://docs.gitlab.com/api/events/. Without the branch's creation among
// the events touchmark reads (more than it reads, events GitLab no longer
// keeps, a source project the reader cannot see), the answer is not
// Complete.
func (d *reader) MergeRequestPushes(ctx context.Context, hub platform.Repo, number int64) (platform.MergeRequestPushes, error) {
	const op = "read the hub merge request"
	var out platform.MergeRequestPushes
	var mr apiMergeRequest
	u := d.c.projectURL(projectID(hub), "merge_requests", strconv.FormatInt(number, 10))
	if _, err := d.c.get(ctx, op, u, nil, &mr); err != nil {
		return out, err
	}
	if mr.IID != number || mr.SourceBranch == "" || mr.SourceProjectID <= 0 || mr.Author == nil || mr.Author.ID <= 0 {
		return out, fmt.Errorf("gitlab: %s: the answer for !%d names no author, source branch or source project", op, number)
	}
	out.Author = platform.Account{ID: strconv.FormatInt(mr.Author.ID, 10), Login: mr.Author.Username}
	out.Branch = mr.SourceBranch
	seen := map[int64]bool{}
	q := url.Values{"action": {"pushed"}, "sort": {"desc"}}
	_, err := listAll(ctx, d.c, op, d.c.projectURL(strconv.FormatInt(mr.SourceProjectID, 10), "events"), q, maxEventPages, func(e apiPushEvent) error {
		if e.PushData == nil || e.PushData.RefType != "branch" || e.PushData.Ref != mr.SourceBranch {
			return nil
		}
		if e.AuthorID > 0 && !seen[e.AuthorID] {
			seen[e.AuthorID] = true
			out.Pushers = append(out.Pushers, platform.Account{ID: strconv.FormatInt(e.AuthorID, 10), Login: e.AuthorUsername})
		}
		if e.PushData.Action == "created" {
			return errFoundCreation
		}
		return nil
	})
	switch {
	case errors.Is(err, errFoundCreation):
		out.Complete = true
	case err != nil && stops(err):
		return out, err
	case err != nil:
		out.Why = fmt.Sprintf("the push events of the source project cannot be read: %v", err)
	default:
		out.Why = fmt.Sprintf("the push events touchmark reads do not reach the creation of %s", mr.SourceBranch)
	}
	return out, nil
}
