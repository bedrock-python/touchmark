package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxLabelPages bounds the listing of a project's labels with its groups'.
const maxLabelPages = 50

// labelColor is the color of the labels touchmark creates: a neutral grey.
const labelColor = "#ededed"

// apiLabel is a label of a project or group.
type apiLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// createLabel is the body of POST /projects/:id/labels.
type createLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// labelList checks label names and joins them for the labels and
// add_labels parameters, which GitLab splits at commas
// (API::Validations::Types::CommaSeparatedToArray): a name with a comma
// would become two labels, so it is refused.
func labelList(op string, names []string) (string, error) {
	for _, n := range names {
		switch {
		case strings.TrimSpace(n) == "":
			return "", invalid(op, "blank label name")
		case strings.Contains(n, ","):
			return "", invalid(op, "label name %q has a comma, which GitLab takes for two labels", n)
		}
	}
	return strings.Join(names, ","), nil
}

// EnsureLabels creates the labels of names the project lacks (its own or
// its groups') and returns the names: GitLab labels merge requests by
// name, and creates a missing label on first use anyway. Names compare
// exactly, as GitLab stores them.
func (t *target) EnsureLabels(ctx context.Context, names []string) ([]string, error) {
	const op = "ensure labels"
	if err := t.live(op); err != nil {
		return nil, err
	}
	if _, err := labelList(op, names); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return []string{}, nil
	}
	have := map[string]bool{}
	complete, err := listAll(ctx, t.c, op, t.c.projectURL(t.id, "labels"), url.Values{"include_ancestor_groups": {"true"}}, maxLabelPages,
		func(l apiLabel) error {
			if l.Name == "" {
				return shapeError(op, "a label without a name")
			}
			have[l.Name] = true
			return nil
		})
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, shapeError(op, "more labels than %d pages", maxLabelPages)
	}
	for _, n := range names {
		if have[n] {
			continue
		}
		var l apiLabel
		_, err := t.c.call(ctx, op, http.MethodPost, t.c.projectURL(t.id, "labels"), nil, createLabel{Name: n, Color: labelColor}, &l)
		switch {
		case err == nil, isStatus(err, http.StatusConflict):
			// Created, or created meanwhile ("Label already exists").
		default:
			return nil, err
		}
		have[n] = true
	}
	out := make([]string, len(names))
	copy(out, names)
	return out, nil
}

var _ platform.TargetWriter = (*target)(nil)
