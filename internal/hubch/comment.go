package hubch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Commenter is a Channel that keeps one comment of touchmark in a hub pull
// request up to date (plan --comment): the REST channel of
// GitHub, Gitea and Forgejo, with the CI's own token (GitHub's GITHUB_TOKEN
// with pull-requests: write, the Actions token of Gitea and Forgejo).
// GitLab's channel is none: CI_JOB_TOKEN cannot write merge request notes.
type Commenter interface {
	// UpsertComment finds the comment on pull request pr that the CI's
	// token wrote and whose body holds marker, and replaces its body with
	// body (nothing is written when the body is the same); without one it
	// creates the comment. It reports what it did.
	UpsertComment(ctx context.Context, pr int64, marker, body string) (CommentResult, error)
}

// CommentResult is what UpsertComment did.
type CommentResult string

const (
	CommentCreated   CommentResult = "created"
	CommentUpdated   CommentResult = "updated"
	CommentUnchanged CommentResult = "unchanged"
)

// commentAuthors are the logins the CI's own token comments as: GitHub's
// GITHUB_TOKEN acts as github-actions[bot]; the Actions token of Gitea as
// its actions user gitea-actions, which Forgejo calls forgejo-actions.
var commentAuthors = map[CI][]string{
	GitHubActions:  {"github-actions[bot]"},
	GiteaActions:   {"gitea-actions"},
	ForgejoActions: {"forgejo-actions", "gitea-actions"},
}

// maxCommentPages bounds the pages of comments read looking for the one to
// update: 3 000 on GitHub, 1 500 on Gitea and Forgejo. A comment past them
// is not found, and a new one is made.
const maxCommentPages = 30

// commentPage is the page size of the comment listing: GitHub's largest,
// and Gitea's default largest (MAX_RESPONSE_ITEMS).
func (ch *restChannel) commentPage() (param string, size int) {
	if ch.github {
		return "per_page", 100
	}
	return "limit", 50
}

// UpsertComment keeps the comment (Commenter): GET
// /repos/{owner}/{repo}/issues/{pr}/comments page by page, the first
// comment by one of the CI token's logins (commentAuthors) whose body holds
// marker; PATCH /repos/{owner}/{repo}/issues/comments/{id} when its body
// differs, else POST /repos/{owner}/{repo}/issues/{pr}/comments. marker must
// be non-empty; a comment of anyone else that holds it is never touched.
func (ch *restChannel) UpsertComment(ctx context.Context, pr int64, marker, body string) (CommentResult, error) {
	if pr <= 0 {
		return "", fmt.Errorf("hub channel: no pull request to comment on (#%d)", pr)
	}
	if marker == "" || !strings.Contains(body, marker) {
		return "", errors.New("hub channel: the comment's body lacks its marker")
	}
	authors := commentAuthors[ch.ci]
	if len(authors) == 0 {
		return "", fmt.Errorf("hub channel: %s has no comment author touchmark knows", ch.ci)
	}
	issue := ch.repo + "/issues/" + strconv.FormatInt(pr, 10) + "/comments"
	param, size := ch.commentPage()
	for page := 1; page <= maxCommentPages; page++ {
		var list []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		u := fmt.Sprintf("%s?%s=%d&page=%d", issue, param, size, page)
		if _, err := ch.client.JSON(ctx, http.MethodGet, u, ch.auth, nil, &list); err != nil {
			return "", fmt.Errorf("hub channel: list the comments of #%d: %w", pr, err)
		}
		for _, c := range list {
			if c.User == nil || c.ID <= 0 || !strings.Contains(c.Body, marker) ||
				!slices.ContainsFunc(authors, func(a string) bool { return strings.EqualFold(a, c.User.Login) }) {
				continue
			}
			if c.Body == body {
				return CommentUnchanged, nil
			}
			edit := ch.repo + "/issues/comments/" + strconv.FormatInt(c.ID, 10)
			if _, err := ch.client.JSON(ctx, http.MethodPatch, edit, ch.auth, map[string]string{"body": body}, nil); err != nil {
				return "", fmt.Errorf("hub channel: update comment %d of #%d: %w", c.ID, pr, err)
			}
			return CommentUpdated, nil
		}
		if len(list) < size {
			break
		}
	}
	if _, err := ch.client.JSON(ctx, http.MethodPost, issue, ch.auth, map[string]string{"body": body}, nil); err != nil {
		return "", fmt.Errorf("hub channel: comment on #%d: %w", pr, err)
	}
	return CommentCreated, nil
}
