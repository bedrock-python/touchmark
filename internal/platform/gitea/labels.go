package gitea

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxLabelPages bounds the listing of a repository's or organization's
// labels.
const maxLabelPages = 50

// errTooManyLabels says a label listing was capped.
var errTooManyLabels = errors.New("more labels than touchmark reads")

// labelColor is the color of the labels touchmark creates: a neutral grey.
const labelColor = "#ededed"

// apiLabel is a label of a repository or organization.
type apiLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// createLabel is the body of POST /repos/{owner}/{repo}/labels.
type createLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// ensureLabels returns the id of each label name, in order: the
// repository's label, else the organization's (usable in all of its
// repositories), else a label it creates in the repository. Gitea and
// Forgejo allow two labels of one name: the lowest id wins, so repeated
// calls agree. Names compare exactly, as the platforms store them.
func (c *client) ensureLabels(ctx context.Context, op, owner, name string, names []string) ([]int64, error) {
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return nil, invalid(op, "blank label name")
		}
	}
	if len(names) == 0 {
		return []int64{}, nil
	}
	have := map[string]int64{}
	collect := func(l apiLabel) error {
		if l.ID <= 0 || l.Name == "" {
			return shapeError(op, "a label without id or name")
		}
		if id, ok := have[l.Name]; !ok || l.ID < id {
			have[l.Name] = l.ID
		}
		return nil
	}
	if err := c.listLabels(ctx, op, c.endpoint("repos", owner, name, "labels"), collect); err != nil {
		return nil, err
	}
	if missing(names, have) {
		org := map[string]int64{}
		err := c.listLabels(ctx, op, c.endpoint("orgs", owner, "labels"), func(l apiLabel) error {
			if l.ID > 0 && l.Name != "" {
				if id, ok := org[l.Name]; !ok || l.ID < id {
					org[l.Name] = l.ID
				}
			}
			return nil
		})
		switch {
		case err == nil:
			for n, id := range org {
				if _, ok := have[n]; !ok {
					have[n] = id
				}
			}
		case platform.ClassOf(err) == platform.ClassNotFound, platform.ClassOf(err) == platform.ClassPermission:
			// A user's repository, or labels this token may not read.
		default:
			return nil, err
		}
	}
	ids := make([]int64, len(names))
	for i, n := range names {
		id, ok := have[n]
		if !ok {
			var l apiLabel
			if _, err := c.call(ctx, op, "POST", c.endpoint("repos", owner, name, "labels"), nil, createLabel{Name: n, Color: labelColor}, &l); err != nil {
				return nil, err
			}
			if l.ID <= 0 {
				return nil, shapeError(op, "the label %q was created without an id", n)
			}
			id = l.ID
			have[n] = id
		}
		ids[i] = id
	}
	return ids, nil
}

// listLabels reads every page of labels at u.
func (c *client) listLabels(ctx context.Context, op, u string, each func(apiLabel) error) error {
	complete, err := listAll(ctx, c, op, u, nil, listOpts{maxPages: maxLabelPages, trustTotal: true}, each)
	if err != nil {
		return err
	}
	if !complete {
		return &platform.Error{Op: op, Class: platform.ClassUnknown, Err: errTooManyLabels}
	}
	return nil
}

// missing reports whether a name has no id in have.
func missing(names []string, have map[string]int64) bool {
	for _, n := range names {
		if _, ok := have[n]; !ok {
			return true
		}
	}
	return false
}

// labelStrings returns ids as decimal strings.
func labelStrings(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}
