package ghfake

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// labelJSON is a label object.
func (c *call) labelJSON(r *repo, l *label) map[string]any {
	return map[string]any{"id": l.id, "node_id": nodeID("Label", l.id), "name": l.name, "color": l.color,
		"description": l.description, "default": false,
		"url": c.base + "/repos/" + escapePath(r.path()) + "/labels/" + escapePath(l.name)}
}

// prJSON is the pull request object; full adds merged, mergeable and
// merged_by as the single-pull endpoint does.
func (c *call) prJSON(ctx context.Context, p *pr, full bool) map[string]any {
	s := c.s
	r := p.repo
	api := c.base + "/repos/" + escapePath(r.path())
	labels := []any{}
	for _, l := range p.labels {
		labels = append(labels, c.labelJSON(r, l))
	}
	state := "open"
	if !p.open {
		state = "closed"
	}
	head := map[string]any{"label": p.label(), "ref": p.headRef, "sha": p.headSHA, "repo": nil,
		"user": c.accountJSON(s.byLogin[strings.ToLower(p.headOwner)])}
	if p.headRepo != nil && !p.headRepo.deleted {
		head["repo"] = c.repoJSON(ctx, p.headRepo, false)
		head["user"] = c.accountJSON(p.headRepo.owner)
	}
	base := map[string]any{"label": r.owner.login + ":" + p.baseRef, "ref": p.baseRef, "sha": p.baseSHA,
		"repo": c.repoJSON(ctx, r, false), "user": c.accountJSON(r.owner)}
	out := map[string]any{
		"url": api + "/pulls/" + strconv.FormatInt(p.number, 10), "id": p.id, "node_id": nodeID("PullRequest", p.id),
		"html_url": s.http.URL + "/" + r.path() + "/pull/" + strconv.FormatInt(p.number, 10),
		"number":   p.number, "state": state, "locked": false, "title": p.title, "user": c.accountJSON(p.author),
		"body": p.body, "labels": labels, "created_at": fmtTime(p.created), "updated_at": fmtTime(p.updated),
		"closed_at": timeOrNil(p.closedAt), "merged_at": timeOrNil(p.mergedAt), "draft": p.draft,
		"head": head, "base": base, "author_association": "CONTRIBUTOR", "auto_merge": nil,
		"merge_commit_sha": nil,
	}
	if p.mergeSHA != "" {
		out["merge_commit_sha"] = p.mergeSHA
	}
	if full {
		out["merged"] = p.merged
		out["merged_by"] = c.accountJSON(p.mergedBy)
		out["mergeable"] = nil
		out["mergeable_state"] = "unknown"
		out["comments"] = len(p.comments)
	}
	return out
}

// findPR finds a pull request of r by the {number} parameter.
func (c *call) findPR(r *repo) *pr {
	n, err := strconv.ParseInt(c.param("number"), 10, 64)
	if err != nil {
		return nil
	}
	for _, p := range r.prs {
		if p.number == n {
			return p
		}
	}
	return nil
}

// listPulls answers GET /repos/{owner}/{repo}/pulls: state (open by
// default, closed, all), base, and head as "owner:branch" — a head without
// "owner:" is ignored and every pull request listed (observed read-only
// 2026-09-29) — sorted by created (newest first) or updated.
func listPulls(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "pull_requests", Read); refused != nil {
		return *refused
	}
	state := c.query.Get("state")
	if state == "" {
		state = "open"
	}
	owner, branch, headFilter := strings.Cut(c.query.Get("head"), ":")
	var list []*pr
	for _, p := range r.prs {
		switch {
		case state == "open" && !p.open, state == "closed" && p.open:
			continue
		case headFilter && (!strings.EqualFold(p.headOwner, owner) || p.headRef != branch):
			continue
		case c.query.Get("base") != "" && p.baseRef != c.query.Get("base"):
			continue
		}
		list = append(list, p)
	}
	asc := c.query.Get("direction") == "asc"
	byUpdated := c.query.Get("sort") == "updated"
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		less := a.number < b.number
		if byUpdated && !a.updated.Equal(b.updated) {
			less = a.updated.Before(b.updated)
		}
		if asc {
			return less
		}
		return !less
	})
	lo, hi, h, _ := c.page(len(list), c.base+"/repositories/"+strconv.FormatInt(r.id, 10)+"/pulls")
	ctx, cancel := c.ctx()
	defer cancel()
	out := []any{}
	for _, p := range list[lo:hi] {
		out = append(out, c.prJSON(ctx, p, false))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// getPull answers GET /repos/{owner}/{repo}/pulls/{pull_number}.
func getPull(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "pull_requests", Read); refused != nil {
		return *refused
	}
	p := c.findPR(r)
	if p == nil {
		return notFound()
	}
	ctx, cancel := c.ctx()
	defer cancel()
	return ok(c.prJSON(ctx, p, true))
}

// createPull answers POST /repos/{owner}/{repo}/pulls: head is a branch of
// the repository, or "owner:branch" of a fork in its network; see openPR
// for the refusals. It creates content.
func createPull(c *call) response {
	var in struct {
		Title    string `json:"title"`
		Head     string `json:"head"`
		HeadRepo string `json:"head_repo"`
		Base     string `json:"base"`
		Body     string `json:"body"`
		Draft    bool   `json:"draft"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	s := c.s
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "pull_requests", Write); refused != nil && !s.mayPropose(c.id, r) {
		return *refused
	}
	if r.archived {
		return apiError(http.StatusForbidden, "Repository was archived so is read-only.")
	}
	if in.Head == "" || in.Base == "" {
		field := "head"
		if in.Head != "" {
			field = "base"
		}
		return validation(map[string]any{"resource": "PullRequest", "code": "missing_field", "field": field})
	}
	np := newPR{head: in.Head, base: in.Base, title: in.Title, body: in.Body, draft: in.Draft, author: c.id.who}
	if owner, branch, cross := strings.Cut(in.Head, ":"); cross {
		np.head = branch
		np.headRepo = s.networkRepo(r, owner, in.HeadRepo)
		if np.headRepo == nil {
			return validation(map[string]any{"resource": "PullRequest", "field": "head", "code": "invalid"})
		}
	}
	if refused := s.limits.contentCreated(s.now(), c.id); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	p, refused := s.openPR(ctx, r, np)
	if refused != nil {
		return *refused
	}
	return created(c.prJSON(ctx, p, true))
}

// mayPropose reports whether a person's token may open a pull request in
// r without write access: anyone who reads a repository whose
// pull_request_creation_policy is "all" may (from a fork). Called with mu
// held.
func (s *Server) mayPropose(id *identity, r *repo) bool {
	if id == nil || id.kind != idToken || id.tok.kind == tokenInstallation || r.prPolicy != "all" {
		return false
	}
	level, _ := s.perm(id, r, "pull_requests")
	return level != ""
}

// networkRepo finds the repository of owner in r's fork network (name
// when head_repo is given). Called with mu held.
func (s *Server) networkRepo(r *repo, owner, name string) *repo {
	root := r
	for root.parent != nil {
		root = root.parent
	}
	for _, id := range sortedIDs(s.repos) {
		cand := s.repos[id]
		if cand.deleted || !strings.EqualFold(cand.owner.login, owner) || (name != "" && !strings.EqualFold(cand.name, name)) {
			continue
		}
		top := cand
		for top.parent != nil {
			top = top.parent
		}
		if top == root {
			return cand
		}
	}
	return nil
}

// updatePull answers PATCH /repos/{owner}/{repo}/pulls/{pull_number}:
// title, body, state and base in one request, applied all or nothing
// (assumed). Reopening meets GitHub's checks (see checkReopen).
func updatePull(c *call) response {
	var raw map[string]json.RawMessage
	if resp, ok := c.decode(&raw); !ok {
		return resp
	}
	var e prEdit
	for key, field := range map[string]**string{"title": &e.title, "body": &e.body, "state": &e.state, "base": &e.base} {
		if v, ok := raw[key]; ok {
			var str string
			if json.Unmarshal(v, &str) != nil {
				return validation(map[string]any{"resource": "PullRequest", "code": "invalid", "field": key})
			}
			*field = &str
		}
	}
	s := c.s
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "pull_requests", Write); refused != nil {
		return *refused
	}
	p := c.findPR(r)
	if p == nil {
		return notFound()
	}
	if r.archived {
		return apiError(http.StatusForbidden, "Repository was archived so is read-only.")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	if refused := s.editPR(ctx, p, e, c.id.who); refused != nil {
		return *refused
	}
	return ok(c.prJSON(ctx, p, true))
}

// mergePull answers PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge.
func mergePull(c *call) response {
	var in struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	s := c.s
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Write); refused != nil {
		return *refused
	}
	p := c.findPR(r)
	if p == nil {
		return notFound()
	}
	how := MergeMethod(in.MergeMethod)
	if how == "" {
		how = MergeCommit
	}
	ctx, cancel := c.ctx()
	defer cancel()
	sha, refused := s.mergePR(ctx, p, how, c.id.who, in.SHA, true)
	if refused != nil {
		return *refused
	}
	return ok(map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

// mergePR merges an open pull request as actor: a merge commit with
// parents [base, head], a squash commit on the base, or a rebase of a head
// based on its base, each committed by GitHub and signed as web merges are
// (observed read-only 2026-09-29: committer "GitHub"). 405 when it cannot
// be merged, 409 when sha is not the head. Called with gitMu and mu held.
func (s *Server) mergePR(ctx context.Context, p *pr, how MergeMethod, actor *account, sha string, judged bool) (string, *response) {
	fail := func(resp response) (string, *response) { return "", &resp }
	r := p.repo
	rg := s.repoGit(r)
	if !p.open {
		return fail(apiError(http.StatusMethodNotAllowed, "Pull Request is not mergeable"))
	}
	if sha != "" && !strings.EqualFold(sha, p.headSHA) {
		return fail(apiError(http.StatusConflict, "Head branch was modified. Review and try the merge again."))
	}
	base := s.tip(ctx, r, p.baseRef)
	if base == "" {
		return fail(apiError(http.StatusMethodNotAllowed, "Base branch was deleted"))
	}
	now := s.now()
	author := ident{name: actor.login, email: actor.noreply(), when: now}
	committer := ident{name: webFlowName, email: webFlowEmail, when: now}
	sign := func(tree string, parents []string, a ident, msg string) (string, error) {
		text := commitText(tree, parents, a, committer, "", msg)
		text = commitText(tree, parents, a, committer, s.webFlowSign(text), msg)
		return rg.writeObject(ctx, "commit", []byte(text))
	}
	var tip string
	switch how {
	case MergeCommit, MergeSquash:
		tree, err := rg.mergeTree(ctx, base, p.headSHA)
		if err != nil {
			return fail(apiError(http.StatusMethodNotAllowed, "Pull Request is not mergeable"))
		}
		if how == MergeCommit {
			msg := "Merge pull request #" + strconv.FormatInt(p.number, 10) + " from " + p.headOwner + "/" + p.headRef + "\n\n" + p.title + "\n"
			tip, err = sign(tree, []string{base, p.headSHA}, author, msg)
		} else {
			tip, err = sign(tree, []string{base}, author, p.title+" (#"+strconv.FormatInt(p.number, 10)+")\n")
		}
		if err != nil {
			return fail(apiError(http.StatusInternalServerError, "Server Error"))
		}
	case MergeRebase:
		based, err := rg.isAncestor(ctx, base, p.headSHA)
		if err != nil || !based {
			return fail(apiError(http.StatusMethodNotAllowed, "This branch can't be rebased"))
		}
		commits, err := rg.revList(ctx, p.headSHA, []string{base})
		if err != nil {
			return fail(apiError(http.StatusInternalServerError, "Server Error"))
		}
		slices.Reverse(commits)
		tip = base
		for _, id := range commits {
			info, err := rg.readCommit(ctx, id)
			if err != nil || len(info.parents) != 1 {
				return fail(apiError(http.StatusMethodNotAllowed, "This branch can't be rebased"))
			}
			if tip, err = sign(info.tree, []string{tip}, info.author, info.message); err != nil {
				return fail(apiError(http.StatusInternalServerError, "Server Error"))
			}
		}
	default:
		return fail(validation(map[string]any{"resource": "PullRequest", "code": "invalid", "field": "merge_method"}))
	}
	ref := "refs/heads/" + p.baseRef
	if err := rg.updateRefs(ctx, []refTx{{ref: ref, old: base, new: tip}}); err != nil {
		return fail(apiError(http.StatusConflict, "Base branch was modified. Review and try the merge again."))
	}
	p.mergeSHA = tip
	s.finish(p, true, actor)
	if err := s.refsMoved(ctx, r, []refChange{{ref: ref, old: base, new: tip}}, actor, judged); err != nil {
		return fail(apiError(http.StatusInternalServerError, "Server Error"))
	}
	return tip, nil
}

// commentJSON is an issue comment.
func (c *call) commentJSON(r *repo, p *pr, cm *comment) map[string]any {
	return map[string]any{"id": cm.id, "node_id": nodeID("IssueComment", cm.id), "body": cm.body,
		"user": c.accountJSON(cm.author), "created_at": fmtTime(cm.created), "updated_at": fmtTime(cm.created),
		"author_association": "CONTRIBUTOR",
		"url":                c.base + "/repos/" + escapePath(r.path()) + "/issues/comments/" + strconv.FormatInt(cm.id, 10),
		"html_url": c.s.http.URL + "/" + r.path() + "/pull/" + strconv.FormatInt(p.number, 10) +
			"#issuecomment-" + strconv.FormatInt(cm.id, 10)}
}

// listComments answers GET /repos/{owner}/{repo}/issues/{number}/comments.
func listComments(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "pull_requests", Read); refused != nil {
		return *refused
	}
	p := c.findPR(r)
	if p == nil {
		return notFound()
	}
	lo, hi, h, _ := c.page(len(p.comments), c.base+"/repositories/"+strconv.FormatInt(r.id, 10)+"/issues/"+
		strconv.FormatInt(p.number, 10)+"/comments")
	out := []any{}
	for _, cm := range p.comments[lo:hi] {
		out = append(out, c.commentJSON(r, p, cm))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// createComment answers POST /repos/{owner}/{repo}/issues/{number}/comments.
// It creates content.
func createComment(c *call) response {
	var in struct {
		Body *string `json:"body"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needIssueWrite(c.id, r); refused != nil {
		return *refused
	}
	p := c.findPR(r)
	if p == nil {
		return notFound()
	}
	if in.Body == nil || *in.Body == "" {
		return validation(map[string]any{"resource": "IssueComment", "code": "missing_field", "field": "body"})
	}
	if resp := bodyTooLong(*in.Body); resp != nil {
		return *resp
	}
	if r.archived {
		return apiError(http.StatusForbidden, "Repository was archived so is read-only.")
	}
	if refused := s.limits.contentCreated(s.now(), c.id); refused != nil {
		return *refused
	}
	cm := &comment{id: s.id(), author: c.id.who, body: *in.Body, created: s.now()}
	p.comments = append(p.comments, cm)
	p.updated = s.now()
	return created(c.commentJSON(r, p, cm))
}

// needIssueWrite admits writes to issue endpoints on pull requests: Pull
// requests or Issues write (docs: both permissions list them). Called with
// mu held.
func (s *Server) needIssueWrite(id *identity, r *repo) *response {
	if s.need(id, r, "issues", Write) == nil {
		return nil
	}
	return s.need(id, r, "pull_requests", Write)
}

// labelPR adds a label to a pull request, creating it in the repository
// when missing (with color "ededed"). Called with mu held.
func (s *Server) labelPR(p *pr, name string) {
	l := findLabel(p.repo, name)
	if l == nil {
		l = &label{id: s.id(), name: name, color: "ededed"}
		p.repo.labels = append(p.repo.labels, l)
	}
	if !slices.Contains(p.labels, l) {
		p.labels = append(p.labels, l)
	}
}

// findLabel finds a label by name, case-insensitively.
func findLabel(r *repo, name string) *label {
	for _, l := range r.labels {
		if strings.EqualFold(l.name, name) {
			return l
		}
	}
	return nil
}

// addLabels answers POST /repos/{owner}/{repo}/issues/{number}/labels with
// {"labels": [...]} or a bare list: the labels are added (missing ones
// created, a long-standing undocumented behavior from public reports) and
// every label of the pull request returned. It creates content.
func addLabels(c *call) response {
	var names []string
	if trimmed := strings.TrimSpace(string(c.body)); strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal(c.body, &names) != nil {
			return apiError(http.StatusBadRequest, "Problems parsing JSON")
		}
	} else {
		var in struct {
			Labels []string `json:"labels"`
		}
		if resp, ok := c.decode(&in); !ok {
			return resp
		}
		names = in.Labels
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needIssueWrite(c.id, r); refused != nil {
		return *refused
	}
	p := c.findPR(r)
	if p == nil {
		return notFound()
	}
	if refused := s.limits.contentCreated(s.now(), c.id); refused != nil {
		return *refused
	}
	for _, name := range names {
		s.labelPR(p, name)
	}
	p.updated = s.now()
	out := []any{}
	for _, l := range p.labels {
		out = append(out, c.labelJSON(r, l))
	}
	return ok(out)
}

// listLabels answers GET /repos/{owner}/{repo}/labels.
func listLabels(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "metadata", Read); refused != nil {
		return *refused
	}
	lo, hi, h, _ := c.page(len(r.labels), c.base+"/repositories/"+strconv.FormatInt(r.id, 10)+"/labels")
	out := []any{}
	for _, l := range r.labels[lo:hi] {
		out = append(out, c.labelJSON(r, l))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// getLabel answers GET /repos/{owner}/{repo}/labels/{name}.
func getLabel(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "metadata", Read); refused != nil {
		return *refused
	}
	l := findLabel(r, c.param("name"))
	if l == nil {
		return notFound()
	}
	return ok(c.labelJSON(r, l))
}

// createLabel answers POST /repos/{owner}/{repo}/labels: 422
// already_exists for a name taken in any case.
func createLabel(c *call) response {
	var in struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.needIssueWrite(c.id, r); refused != nil {
		return *refused
	}
	if strings.TrimSpace(in.Name) == "" {
		return validation(map[string]any{"resource": "Label", "code": "missing_field", "field": "name"})
	}
	if findLabel(r, in.Name) != nil {
		return validation(map[string]any{"resource": "Label", "code": "already_exists", "field": "name"})
	}
	if len([]rune(in.Description)) > 100 {
		return validation(map[string]any{"resource": "Label", "code": "invalid", "field": "description"})
	}
	if refused := s.limits.contentCreated(s.now(), c.id); refused != nil {
		return *refused
	}
	color := strings.TrimPrefix(in.Color, "#")
	if color == "" {
		color = "ededed"
	}
	l := &label{id: s.id(), name: in.Name, color: color, description: in.Description}
	r.labels = append(r.labels, l)
	return created(c.labelJSON(r, l))
}
