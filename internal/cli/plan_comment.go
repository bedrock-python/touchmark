package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// plan --comment: one comment in the hub pull request
// holds plan's Markdown report and is edited by every later run, through
// the hub channel with the CI's own token (GITHUB_TOKEN with
// pull-requests: write in the plan job; the Actions token of Gitea and
// Forgejo). GitLab has no comment: CI_JOB_TOKEN cannot write merge request
// notes.

// planCommentMarker is the hidden line that tells plan's comment from the
// others: one per hub, so that two hubs planning in one repository (a
// template's own tests) keep a comment each.
func planCommentMarker(hubID string) string {
	return "<!-- touchmark plan: " + hubID + " -->"
}

// postPlanComment keeps plan's comment in the hub pull request pr through
// ch, with rep rendered as its body (masked with reg), and returns a warning
// when it could not: the run builds no hub pull request, the CI has no
// comment through its token (GitLab), or the platform refused (a pull
// request from a fork gets a read-only token). A comment never changes the
// exit code.
func postPlanComment(ctx context.Context, hctx hubch.Context, ch hubch.Channel, pr int64, rep *report.Delivery, reg *redact.Registry) string {
	const what = "plan --comment: "
	switch {
	case hctx.CI == hubch.GitLabCI:
		return what + "not posted: GitLab's CI_JOB_TOKEN cannot write merge request notes; the report is in the job's log and artifacts"
	case !pullRequestRun(hctx) || pr <= 0:
		return what + "not posted: this run builds no hub pull request"
	}
	if fc, failed := ch.(failedChannel); failed {
		return what + fmt.Sprintf("not posted: %v", fc.err)
	}
	c, ok := ch.(hubch.Commenter)
	if !ok {
		return what + "not posted: no channel to the hub for its CI token"
	}
	marker := planCommentMarker(rep.Hub.ID)
	var b strings.Builder
	if err := rep.WriteComment(&b, report.MaxComment, "\n"+marker+"\n"); err != nil {
		return what + err.Error()
	}
	body := reg.Replace(b.String())
	if _, err := c.UpsertComment(ctx, pr, marker, body); err != nil {
		return what + fmt.Sprintf("not posted: %v", err)
	}
	return ""
}
