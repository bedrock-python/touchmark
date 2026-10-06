package github

import (
	"context"
	"net/http"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// labelColor is the color of the labels touchmark creates: a neutral grey,
// without the leading '#', as the API takes it.
const labelColor = "ededed"

// apiLabel is a label of a repository or pull request.
type apiLabel struct {
	Name string `json:"name"`
}

// createLabel is the body of POST /repos/{owner}/{repo}/labels.
type createLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// ensureLabels returns each label name of names as the repository spells
// it, creating the missing ones: GET /labels/{name} (GitHub compares label
// names ignoring case), else POST /labels; a 422 "already_exists" from a
// label created meanwhile is success. Labels go on pull requests by name
// (POST /issues/{n}/labels), so the names are the "ids" of
// TargetWriter.EnsureLabels. Adding a label that does not exist would
// create it too, but that is not documented: labels are created first.
func (c *client) ensureLabels(ctx context.Context, op string, a *httpx.Auth, owner, name string, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return nil, invalid(op, "blank label name")
		}
	}
	for _, n := range names {
		var l apiLabel
		_, err := c.get(ctx, op, c.repoURL(owner, name, "labels", n), nil, a, &l)
		if platform.ClassOf(err) == platform.ClassNotFound {
			_, err = c.call(ctx, op, http.MethodPost, c.repoURL(owner, name, "labels"), nil, a, createLabel{Name: n, Color: labelColor}, &l)
			if statusOf(err) == http.StatusUnprocessableEntity && strings.Contains(messageOf(err), "already_exists") {
				l = apiLabel{}
				_, err = c.get(ctx, op, c.repoURL(owner, name, "labels", n), nil, a, &l)
			}
		}
		if err != nil {
			return nil, err
		}
		if l.Name == "" {
			return nil, shapeError(op, "the label %q has no name", n)
		}
		out = append(out, l.Name)
	}
	return out, nil
}

// addLabels puts labels on pull request number (POST
// /issues/{number}/labels, add-only) and returns its labels afterwards.
func (c *client) addLabels(ctx context.Context, op string, a *httpx.Auth, owner, name string, number int64, labels []string) ([]string, error) {
	var got []apiLabel
	_, err := c.call(ctx, op, http.MethodPost, c.issueURL(owner, name, number, "labels"), nil, a,
		map[string][]string{"labels": labels}, &got)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(got))
	for _, l := range got {
		out = append(out, l.Name)
	}
	return out, nil
}

// issueURL is the endpoint of issue (or pull request) number with more
// segments.
func (c *client) issueURL(owner, name string, number int64, more ...string) string {
	return c.repoURL(owner, name, append([]string{"issues", itoa(number)}, more...)...)
}
