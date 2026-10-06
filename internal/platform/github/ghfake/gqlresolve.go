package ghfake

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GraphQL objects of the fake. Every value of an object type implements
// gqlObject; resolve answers their fields. All of it runs with mu held.

type queryRoot struct{}

func (queryRoot) typename() string { return "Query" }

type mutationRoot struct{}

func (mutationRoot) typename() string { return "Mutation" }

type repoObj struct{ r *repo }

func (repoObj) typename() string { return "Repository" }

type acctObj struct{ a *account }

func (o acctObj) typename() string { return o.a.typ }

type refObj struct {
	r         *repo
	ref, sha  string
	shortName string
}

func (refObj) typename() string { return "Ref" }

type commitObj struct {
	r    *repo
	info commitInfo
}

func (commitObj) typename() string { return "Commit" }

type treeObj struct {
	r   *repo
	oid string
}

func (treeObj) typename() string { return "Tree" }

type blobObj struct {
	r   *repo
	oid string
}

func (blobObj) typename() string { return "Blob" }

type entryObj struct {
	r *repo
	e treeEntry
}

func (entryObj) typename() string { return "TreeEntry" }

type prObj struct{ p *pr }

func (prObj) typename() string { return "PullRequest" }

type labelObj struct{ l *label }

func (labelObj) typename() string { return "Label" }

type repoTopicObj struct {
	r    *repo
	name string
}

func (repoTopicObj) typename() string { return "RepositoryTopic" }

type topicObj struct{ name string }

func (topicObj) typename() string { return "Topic" }

type eventObj struct {
	e *event
	p *pr
}

func (o eventObj) typename() string { return o.e.typ }

type pageInfoObj struct {
	hasNext, hasPrev bool
	start, end       any
}

func (pageInfoObj) typename() string { return "PageInfo" }

type connObj struct {
	typ      string
	nodes    []any
	cursors  []string
	total    int
	filtered int
	info     pageInfoObj
}

func (o connObj) typename() string { return o.typ }

type edgeObj struct {
	typ    string
	cursor string
	node   any
}

func (o edgeObj) typename() string { return o.typ }

type rateLimitObj struct {
	limit, remaining, used int
	reset                  time.Time
}

func (rateLimitObj) typename() string { return "RateLimit" }

type payloadObj struct{ clientMutationID any }

func (payloadObj) typename() string { return "UpdateRefsPayload" }

// notFoundErr is GitHub's NOT_FOUND execution error.
func notFoundErr(msg string) *gqlError { return &gqlError{Type: "NOT_FOUND", Message: msg} }

// forbiddenErr is GitHub's FORBIDDEN execution error.
func forbiddenErr(id *identity) *gqlError {
	return &gqlError{Type: "FORBIDDEN", Message: forbiddenMessage(id)}
}

// unprocessableErr is an UNPROCESSABLE execution error.
func unprocessableErr(msg string) *gqlError { return &gqlError{Type: "UNPROCESSABLE", Message: msg} }

// str returns a string argument.
func argStr(args map[string]any, name string) (string, bool) {
	v, ok := args[name].(string)
	return v, ok
}

// argInt returns an Int argument.
func argInt(args map[string]any, name string) (int64, bool) {
	v, ok := args[name].(int64)
	return v, ok
}

// resolve answers one field of obj.
func (e *executor) resolve(obj gqlObject, field string, args map[string]any) (any, *gqlError) {
	switch o := obj.(type) {
	case queryRoot:
		return e.resolveQuery(field, args)
	case mutationRoot:
		if field == "updateRefs" {
			return e.updateRefs(args)
		}
	case repoObj:
		return e.resolveRepo(o.r, field, args)
	case acctObj:
		return e.resolveAccount(o.a, field, args)
	case refObj:
		switch field {
		case "id":
			return base64.StdEncoding.EncodeToString([]byte("ghfake:Ref:" + strconv.FormatInt(o.r.id, 10) + ":" + o.ref)), nil
		case "name":
			return o.shortName, nil
		case "prefix":
			return strings.TrimSuffix(o.ref, o.shortName), nil
		case "target":
			return e.object(o.r, o.sha)
		}
	case commitObj:
		switch field {
		case "committedDate":
			return fmtTime(o.info.committer.when), nil
		case "message":
			return o.info.message, nil
		case "tree":
			return treeObj{r: o.r, oid: o.info.tree}, nil
		case "file":
			// The entry at a path of the commit's tree, of any type (a
			// submodule too), never through a symlinked directory; null
			// with a NOT_FOUND error on the field when there is none
			// (observed read-only 2026-09-29, also for a path through a
			// symlink).
			arg, _ := argStr(args, "path")
			missing := notFoundErr("Could not resolve file for path '" + arg + "'.")
			p := strings.Trim(arg, "/")
			if p == "" {
				return nil, missing
			}
			en, found, err := e.s.repoGit(o.r).entryAt(e.ctx, o.info.id, p)
			if err != nil || !found {
				return nil, missing
			}
			return entryObj{r: o.r, e: en}, nil
		}
		return e.gitObjectField(o.r, o.info.id, "Commit", field)
	case treeObj:
		if field == "entries" {
			list, err := e.s.repoGit(o.r).lsTree(e.ctx, o.oid, "", false)
			if err != nil {
				return nil, &gqlError{Type: "INTERNAL", Message: "Something went wrong while executing your query."}
			}
			out := make([]any, len(list))
			for i, en := range list {
				out[i] = entryObj{r: o.r, e: en}
			}
			return out, nil
		}
		return e.gitObjectField(o.r, o.oid, "Tree", field)
	case blobObj:
		switch field {
		case "byteSize", "isBinary", "isTruncated", "text":
			data, err := e.blob(o.r, o.oid)
			if err != nil {
				return nil, &gqlError{Type: "INTERNAL", Message: "Something went wrong while executing your query."}
			}
			binary := bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
			switch field {
			case "byteSize":
				return int64(len(data)), nil
			case "isBinary":
				return binary, nil
			case "isTruncated":
				return false, nil
			}
			if binary {
				return nil, nil
			}
			return string(data), nil
		}
		return e.gitObjectField(o.r, o.oid, "Blob", field)
	case entryObj:
		return e.resolveEntry(o, field)
	case prObj:
		return e.resolvePR(o.p, field, args)
	case labelObj:
		switch field {
		case "id":
			return nodeID("Label", o.l.id), nil
		case "name":
			return o.l.name, nil
		case "color":
			return o.l.color, nil
		case "description":
			if o.l.description == "" {
				return nil, nil
			}
			return o.l.description, nil
		}
	case repoTopicObj:
		switch field {
		case "id":
			return base64.StdEncoding.EncodeToString([]byte("ghfake:RepositoryTopic:" + strconv.FormatInt(o.r.id, 10) + ":" + o.name)), nil
		case "topic":
			return topicObj{name: o.name}, nil
		}
	case topicObj:
		switch field {
		case "id":
			return base64.StdEncoding.EncodeToString([]byte("ghfake:Topic:" + o.name)), nil
		case "name":
			return o.name, nil
		}
	case eventObj:
		return e.resolveEvent(o, field)
	case connObj:
		switch field {
		case "nodes":
			return o.nodes, nil
		case "edges":
			out := make([]any, len(o.nodes))
			for i, n := range o.nodes {
				out[i] = edgeObj{typ: strings.TrimSuffix(o.typ, "Connection") + "Edge", cursor: o.cursors[i], node: n}
			}
			return out, nil
		case "totalCount":
			return int64(o.total), nil
		case "filteredCount":
			return int64(o.filtered), nil
		case "pageCount":
			return int64(len(o.nodes)), nil
		case "pageInfo":
			return o.info, nil
		}
	case edgeObj:
		switch field {
		case "cursor":
			return o.cursor, nil
		case "node":
			return o.node, nil
		}
	case pageInfoObj:
		switch field {
		case "hasNextPage":
			return o.hasNext, nil
		case "hasPreviousPage":
			return o.hasPrev, nil
		case "startCursor":
			return o.start, nil
		case "endCursor":
			return o.end, nil
		}
	case rateLimitObj:
		switch field {
		case "cost":
			return int64(1), nil
		case "limit":
			return int64(o.limit), nil
		case "remaining":
			return int64(o.remaining), nil
		case "used":
			return int64(o.used), nil
		case "nodeCount":
			return int64(0), nil
		case "resetAt":
			return fmtTime(o.reset), nil
		}
	case payloadObj:
		if field == "clientMutationId" {
			return o.clientMutationID, nil
		}
	}
	return nil, nil
}

// resolveQuery answers the root query fields.
func (e *executor) resolveQuery(field string, args map[string]any) (any, *gqlError) {
	s := e.s
	switch field {
	case "repository":
		owner, _ := argStr(args, "owner")
		name, _ := argStr(args, "name")
		follow, _ := args["followRenames"].(bool)
		if r := e.findRepo(owner, name, follow); r != nil {
			return repoObj{r: r}, nil
		}
		return nil, notFoundErr("Could not resolve to a Repository with the name '" + owner + "/" + name + "'.")
	case "node":
		raw, _ := argStr(args, "id")
		if n := e.node(raw); n != nil {
			return n, nil
		}
		return nil, notFoundErr("Could not resolve to a node with the global id of '" + raw + "'")
	case "nodes":
		ids, _ := args["ids"].([]any)
		out := make([]any, len(ids))
		for i, v := range ids {
			raw, _ := v.(string)
			if n := e.node(raw); n != nil {
				out[i] = n
				continue
			}
			e.errors = append(e.errors, gqlError{Type: "NOT_FOUND", Path: []any{"nodes", i},
				Message: "Could not resolve to a node with the global id of '" + raw + "'"})
		}
		return out, nil
	case "user":
		login, _ := argStr(args, "login")
		if a := s.byLogin[strings.ToLower(login)]; a != nil && a.typ == TypeUser {
			return acctObj{a: a}, nil
		}
		return nil, notFoundErr("Could not resolve to a User with the login of '" + login + "'.")
	case "organization":
		login, _ := argStr(args, "login")
		if a := s.byLogin[strings.ToLower(login)]; a != nil && a.typ == TypeOrganization {
			return acctObj{a: a}, nil
		}
		return nil, notFoundErr("Could not resolve to an Organization with the login of '" + login + "'.")
	case "viewer":
		if e.id.installation() {
			return nil, &gqlError{Type: "FORBIDDEN", Message: "Resource not accessible by integration"}
		}
		return acctObj{a: e.id.who}, nil
	case "rateLimit":
		now := s.now()
		limit := s.limits.primaryLimit(e.id, "graphql")
		w := s.limits.window(now, "graphql:"+identityKey(e.id))
		return rateLimitObj{limit: limit, remaining: max(limit-w.used, 0), used: w.used, reset: w.start.Add(time.Hour)}, nil
	}
	return nil, nil
}

// findRepo resolves owner/name for the identity, following renames when
// asked; nil when it does not see it.
func (e *executor) findRepo(owner, name string, follow bool) *repo {
	s := e.s
	key := strings.ToLower(owner + "/" + name)
	r := s.byPath[key]
	if r == nil && follow {
		if id, moved := s.redirects[key]; moved && !s.apiGone[key] {
			r = s.repos[id]
		}
	}
	if r == nil || r.deleted {
		return nil
	}
	s.noteScope(e.id, r)
	if s.need(e.id, r, "metadata", Read) != nil {
		return nil
	}
	return r
}

// node resolves a global id the identity may see.
func (e *executor) node(raw string) gqlObject {
	s := e.s
	typ, id, ok := parseNodeID(raw)
	if !ok {
		return nil
	}
	switch typ {
	case "Repository":
		if r := s.repos[id]; r != nil && !r.deleted {
			s.noteScope(e.id, r)
			if s.need(e.id, r, "metadata", Read) == nil {
				return repoObj{r: r}
			}
		}
	case "User", "Bot", "Organization":
		if a := s.accounts[id]; a != nil && a.typ == typ {
			return acctObj{a: a}
		}
	case "PullRequest":
		for _, rid := range sortedIDs(s.repos) {
			r := s.repos[rid]
			for _, p := range r.prs {
				if p.id == id && !r.deleted && s.need(e.id, r, "pull_requests", Read) == nil {
					return prObj{p: p}
				}
			}
		}
	}
	return nil
}

// resolveRepo answers Repository fields.
func (e *executor) resolveRepo(r *repo, field string, args map[string]any) (any, *gqlError) {
	s := e.s
	switch field {
	case "id":
		return nodeID("Repository", r.id), nil
	case "databaseId":
		return r.id, nil
	case "name":
		return r.name, nil
	case "nameWithOwner":
		return r.path(), nil
	case "owner":
		return acctObj{a: r.owner}, nil
	case "url":
		return s.http.URL + "/" + r.path(), nil
	case "isArchived":
		return r.archived, nil
	case "isDisabled":
		return r.disabled, nil
	case "isFork":
		return r.parent != nil, nil
	case "isLocked":
		return false, nil
	case "isMirror":
		return r.mirror != "", nil
	case "isPrivate":
		return r.private(), nil
	case "isTemplate":
		return r.template, nil
	case "hasPullRequestsEnabled":
		return !r.prsDisabled, nil
	case "visibility":
		return strings.ToUpper(r.visibility), nil
	case "isEmpty":
		refs, err := s.repoGit(r).refs(e.ctx, "refs/heads/")
		return err == nil && len(refs) == 0, nil
	case "parent":
		if r.parent != nil && !r.parent.deleted && s.canSee(e.id, r.parent) {
			return repoObj{r: r.parent}, nil
		}
		return nil, nil
	case "viewerPermission":
		level, _ := s.perm(e.id, r, "contents")
		switch {
		case e.id.kind == idToken && e.id.tok.kind != tokenInstallation && repoLevel(e.id.who, r) == "admin":
			return "ADMIN", nil
		case levelRank(level) >= levelRank(Write):
			return "WRITE", nil
		case level != "":
			return "READ", nil
		}
		return nil, nil
	case "defaultBranchRef":
		tip := s.tip(e.ctx, r, r.defaultBranch)
		if tip == "" {
			return nil, nil
		}
		return refObj{r: r, ref: "refs/heads/" + r.defaultBranch, sha: tip, shortName: r.defaultBranch}, nil
	case "ref":
		name, _ := argStr(args, "qualifiedName")
		candidates := []string{name}
		if !strings.HasPrefix(name, "refs/") {
			candidates = []string{"refs/heads/" + name, "refs/tags/" + name}
		}
		refs, err := s.repoGit(r).refs(e.ctx, "refs/")
		if err != nil {
			return nil, nil
		}
		for _, c := range candidates {
			if sha, ok := refs[c]; ok {
				short := c
				for _, p := range []string{"refs/heads/", "refs/tags/", "refs/"} {
					if rest, found := strings.CutPrefix(c, p); found {
						short = rest
						break
					}
				}
				return refObj{r: r, ref: c, sha: sha, shortName: short}, nil
			}
		}
		return nil, nil
	case "object":
		if s.need(e.id, r, "contents", Read) != nil {
			return nil, forbiddenErr(e.id)
		}
		if oid, ok := argStr(args, "oid"); ok {
			return e.object(r, strings.ToLower(oid))
		}
		expr, _ := argStr(args, "expression")
		return e.expression(r, expr)
	case "pullRequests":
		if s.need(e.id, r, "pull_requests", Read) != nil {
			return nil, forbiddenErr(e.id)
		}
		return e.pullRequests(r, args)
	case "repositoryTopics":
		nodes := make([]any, len(r.topics))
		for i, t := range r.topics {
			nodes[i] = repoTopicObj{r: r, name: t}
		}
		return e.connection("RepositoryTopicConnection", "repositoryTopics", nodes, args, len(nodes))
	}
	return nil, nil
}

// expression resolves "<rev>:<path>", "<rev>:" or "<rev>" ("HEAD" is the
// default branch). A missing path and a submodule are null (observed
// read-only 2026-09-29); a symlink is the Blob of its target text.
func (e *executor) expression(r *repo, expr string) (any, *gqlError) {
	s := e.s
	rev, p, hasPath := strings.Cut(expr, ":")
	rg := s.repoGit(r)
	var commit string
	if rev == "HEAD" || rev == "" {
		commit = s.tip(e.ctx, r, r.defaultBranch)
	} else if id, ok, err := rg.resolve(e.ctx, rev); err == nil && ok {
		commit = id
	}
	if commit == "" {
		return nil, nil
	}
	if !hasPath {
		return e.object(r, commit)
	}
	p = strings.Trim(p, "/")
	if p == "" {
		tree, err := rg.treeOf(e.ctx, commit)
		if err != nil {
			return nil, nil
		}
		return treeObj{r: r, oid: tree}, nil
	}
	en, found, err := rg.entryAt(e.ctx, commit, p)
	if err != nil || !found {
		return nil, nil
	}
	switch en.typ {
	case "blob":
		return blobObj{r: r, oid: en.oid}, nil
	case "tree":
		return treeObj{r: r, oid: en.oid}, nil
	}
	return nil, nil
}

// object returns the git object oid of r, nil when it does not exist.
func (e *executor) object(r *repo, oid string) (any, *gqlError) {
	rg := e.s.repoGit(r)
	typ, err := rg.objectType(e.ctx, oid)
	if err != nil {
		return nil, nil
	}
	switch typ {
	case "commit":
		info, err := rg.readCommit(e.ctx, oid)
		if err != nil {
			return nil, nil
		}
		return commitObj{r: r, info: info}, nil
	case "tree":
		return treeObj{r: r, oid: oid}, nil
	case "blob":
		return blobObj{r: r, oid: oid}, nil
	}
	return nil, nil
}

// gitObjectField answers the GitObject interface fields.
func (e *executor) gitObjectField(r *repo, oid, typ, field string) (any, *gqlError) {
	switch field {
	case "oid":
		return oid, nil
	case "abbreviatedOid":
		return oid[:7], nil
	case "id":
		return base64.StdEncoding.EncodeToString([]byte("ghfake:" + typ + ":" + strconv.FormatInt(r.id, 10) + ":" + oid)), nil
	case "repository":
		return repoObj{r: r}, nil
	case "commitUrl":
		return e.s.http.URL + "/" + r.path() + "/commit/" + oid, nil
	case "commitResourcePath":
		return "/" + r.path() + "/commit/" + oid, nil
	}
	return nil, nil
}

// resolveEntry answers TreeEntry fields.
func (e *executor) resolveEntry(o entryObj, field string) (any, *gqlError) {
	switch field {
	case "name":
		return o.e.path[strings.LastIndexByte(o.e.path, '/')+1:], nil
	case "path":
		return o.e.path, nil
	case "mode":
		n, _ := strconv.ParseInt(o.e.mode, 8, 64)
		return n, nil
	case "type":
		return o.e.typ, nil
	case "oid":
		return o.e.oid, nil
	case "size":
		return max(o.e.size, 0), nil
	case "repository":
		return repoObj{r: o.r}, nil
	case "object":
		switch o.e.typ {
		case "blob":
			return blobObj{r: o.r, oid: o.e.oid}, nil
		case "tree":
			return treeObj{r: o.r, oid: o.e.oid}, nil
		}
	}
	return nil, nil
}

// resolveAccount answers User, Bot and Organization fields.
func (e *executor) resolveAccount(a *account, field string, args map[string]any) (any, *gqlError) {
	s := e.s
	switch field {
	case "id":
		return nodeID(a.typ, a.id), nil
	case "databaseId":
		return a.id, nil
	case "login":
		return a.graphLogin(), nil
	case "name":
		if a.typ == TypeBot {
			return nil, nil
		}
		return a.name, nil
	case "email":
		return "", nil
	case "url":
		if a.app != nil {
			return s.http.URL + "/apps/" + a.app.slug, nil
		}
		return s.http.URL + "/" + a.login, nil
	case "resourcePath":
		if a.app != nil {
			return "/apps/" + a.app.slug, nil
		}
		return "/" + a.login, nil
	case "avatarUrl":
		return s.http.URL + "/avatars/u/" + strconv.FormatInt(a.id, 10), nil
	case "repository":
		name, _ := argStr(args, "name")
		follow, _ := args["followRenames"].(bool)
		if r := e.findRepo(a.login, name, follow); r != nil {
			return repoObj{r: r}, nil
		}
		return nil, nil
	}
	return nil, nil
}

// resolvePR answers PullRequest fields.
func (e *executor) resolvePR(p *pr, field string, args map[string]any) (any, *gqlError) {
	s := e.s
	switch field {
	case "id":
		return nodeID("PullRequest", p.id), nil
	case "databaseId":
		return p.id, nil
	case "number":
		return p.number, nil
	case "title":
		return p.title, nil
	case "body":
		return p.body, nil
	case "url":
		return s.http.URL + "/" + p.repo.path() + "/pull/" + strconv.FormatInt(p.number, 10), nil
	case "state":
		switch {
		case p.merged:
			return "MERGED", nil
		case p.open:
			return "OPEN", nil
		}
		return "CLOSED", nil
	case "closed":
		return !p.open, nil
	case "merged":
		return p.merged, nil
	case "isDraft":
		return p.draft, nil
	case "headRefName":
		return p.headRef, nil
	case "headRefOid":
		return p.headSHA, nil
	case "baseRefName":
		return p.baseRef, nil
	case "baseRef":
		// null once the base branch is gone.
		tip := s.tip(e.ctx, p.repo, p.baseRef)
		if tip == "" {
			return nil, nil
		}
		return refObj{r: p.repo, ref: "refs/heads/" + p.baseRef, sha: tip, shortName: p.baseRef}, nil
	case "baseRefOid":
		return p.baseSHA, nil
	case "isCrossRepository":
		return p.headRepo != p.repo, nil
	case "repository", "baseRepository":
		return repoObj{r: p.repo}, nil
	case "headRepository":
		if p.headRepo != nil && !p.headRepo.deleted && s.canSee(e.id, p.headRepo) {
			return repoObj{r: p.headRepo}, nil
		}
		return nil, nil
	case "headRepositoryOwner":
		if a := s.byLogin[strings.ToLower(p.headOwner)]; a != nil {
			return acctObj{a: a}, nil
		}
		return nil, nil
	case "author":
		return acctOrNil(p.author), nil
	case "mergedBy":
		return acctOrNil(p.mergedBy), nil
	case "createdAt":
		return fmtTime(p.created), nil
	case "updatedAt":
		return fmtTime(p.updated), nil
	case "closedAt":
		return timeOrNil(p.closedAt), nil
	case "mergedAt":
		return timeOrNil(p.mergedAt), nil
	case "labels":
		nodes := make([]any, len(p.labels))
		for i, l := range p.labels {
			nodes[i] = labelObj{l: l}
		}
		return e.connection("LabelConnection", "labels", nodes, args, len(nodes))
	case "timelineItems":
		return e.timeline(p, args)
	}
	return nil, nil
}

// acctOrNil wraps an account, nil for none.
func acctOrNil(a *account) any {
	if a == nil {
		return nil
	}
	return acctObj{a: a}
}

// resolveEvent answers the fields of timeline events.
func (e *executor) resolveEvent(o eventObj, field string) (any, *gqlError) {
	switch field {
	case "id":
		return nodeID(o.e.typ, o.e.id), nil
	case "actor":
		return acctOrNil(o.e.actor), nil
	case "createdAt":
		return fmtTime(o.e.at), nil
	case "stateReason":
		if o.e.typ == EventReopened {
			return "REOPENED", nil
		}
		if o.e.stateReason == "" {
			return nil, nil
		}
		return o.e.stateReason, nil
	case "headRefName":
		return o.p.headRef, nil
	case "previousRefName":
		return o.e.from, nil
	case "currentRefName":
		return o.e.to, nil
	}
	return nil, nil
}

// itemTypes maps PullRequestTimelineItemsItemType values to the event
// types the fake records.
var itemTypes = map[string]string{
	"CLOSED_EVENT":                EventClosed,
	"REOPENED_EVENT":              EventReopened,
	"MERGED_EVENT":                EventMerged,
	"HEAD_REF_DELETED_EVENT":      EventHeadRefDeleted,
	"HEAD_REF_FORCE_PUSHED_EVENT": EventHeadRefForced,
	"BASE_REF_CHANGED_EVENT":      EventBaseRefChanged,
}

// timeline answers PullRequest.timelineItems: events filtered by
// itemTypes and since, then skip and the pagination arguments.
func (e *executor) timeline(p *pr, args map[string]any) (any, *gqlError) {
	var want map[string]bool
	if list, ok := args["itemTypes"].([]any); ok {
		want = map[string]bool{}
		for _, v := range list {
			if t, ok := itemTypes[v.(string)]; ok {
				want[t] = true
			}
		}
	}
	var since time.Time
	if v, ok := argStr(args, "since"); ok {
		since, _ = time.Parse(time.RFC3339, v)
	}
	var nodes []any
	for _, ev := range p.events {
		if (want == nil || want[ev.typ]) && !ev.at.Before(since) {
			nodes = append(nodes, eventObj{e: ev, p: p})
		}
	}
	if skip, ok := argInt(args, "skip"); ok && skip > 0 {
		nodes = nodes[min(int(skip), len(nodes)):]
	}
	c, err := e.connection("PullRequestTimelineItemsConnection", "timelineItems", nodes, args, len(p.events))
	if err != nil {
		return nil, err
	}
	conn := c.(connObj)
	conn.filtered = len(nodes)
	return conn, nil
}

// pullRequests answers Repository.pullRequests: filtered by headRefName
// (pull requests from forks included, as GitHub does), baseRefName,
// states and labels, ordered by orderBy (by number when absent), then
// paginated.
func (e *executor) pullRequests(r *repo, args map[string]any) (any, *gqlError) {
	head, hasHead := argStr(args, "headRefName")
	base, hasBase := argStr(args, "baseRefName")
	var states map[string]bool
	if list, ok := args["states"].([]any); ok {
		states = map[string]bool{}
		for _, v := range list {
			states[v.(string)] = true
		}
	}
	var labels []string
	if list, ok := args["labels"].([]any); ok {
		for _, v := range list {
			labels = append(labels, v.(string))
		}
	}
	var list []*pr
	for _, p := range r.prs {
		state := "CLOSED"
		switch {
		case p.merged:
			state = "MERGED"
		case p.open:
			state = "OPEN"
		}
		switch {
		case hasHead && p.headRef != head, hasBase && p.baseRef != base, states != nil && !states[state]:
			continue
		}
		if len(labels) > 0 && !slices.ContainsFunc(p.labels, func(l *label) bool {
			return slices.ContainsFunc(labels, func(n string) bool { return strings.EqualFold(n, l.name) })
		}) {
			continue
		}
		list = append(list, p)
	}
	if order, ok := args["orderBy"].(map[string]any); ok {
		desc := order["direction"] == "DESC"
		field, _ := order["field"].(string)
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			less := a.number < b.number
			switch field {
			case "UPDATED_AT":
				if !a.updated.Equal(b.updated) {
					less = a.updated.Before(b.updated)
				}
			case "COMMENTS":
				if len(a.comments) != len(b.comments) {
					less = len(a.comments) < len(b.comments)
				}
			}
			if desc {
				return !less
			}
			return less
		})
	}
	nodes := make([]any, len(list))
	for i, p := range list {
		nodes[i] = prObj{p: p}
	}
	return e.connection("PullRequestConnection", "pullRequests", nodes, args, len(nodes))
}

// connection paginates nodes with first/after and last/before (one of
// first and last is required, at most 100; GitHub's
// MISSING_PAGINATION_BOUNDARIES and EXCESSIVE_PAGINATION errors, messages
// assumed). Cursors are opaque base64 strings.
func (e *executor) connection(typ, name string, nodes []any, args map[string]any, total int) (any, *gqlError) {
	first, hasFirst := argInt(args, "first")
	last, hasLast := argInt(args, "last")
	switch {
	case !hasFirst && !hasLast:
		return nil, &gqlError{Type: "MISSING_PAGINATION_BOUNDARIES",
			Message: "You must provide a `first` or `last` value to properly paginate the `" + name + "` connection."}
	case hasFirst && (first < 0 || first > 100):
		return nil, &gqlError{Type: "EXCESSIVE_PAGINATION", Message: "Requesting " + strconv.FormatInt(first, 10) +
			" records on the `" + name + "` connection exceeds the `first` limit of 100 records."}
	case hasLast && (last < 0 || last > 100):
		return nil, &gqlError{Type: "EXCESSIVE_PAGINATION", Message: "Requesting " + strconv.FormatInt(last, 10) +
			" records on the `" + name + "` connection exceeds the `last` limit of 100 records."}
	}
	lo, hi := 0, len(nodes)
	if after, ok := argStr(args, "after"); ok {
		if i, ok := decodeCursor(after); ok {
			lo = min(i+1, hi)
		}
	}
	if before, ok := argStr(args, "before"); ok {
		if i, ok := decodeCursor(before); ok {
			hi = max(min(i, hi), lo)
		}
	}
	boundLo, boundHi := lo, hi
	if hasFirst {
		hi = min(hi, lo+int(first))
	}
	if hasLast {
		lo = max(lo, hi-int(last))
	}
	c := connObj{typ: typ, nodes: nodes[lo:hi], total: total, filtered: len(nodes)}
	for i := lo; i < hi; i++ {
		c.cursors = append(c.cursors, encodeCursor(i))
	}
	c.info = pageInfoObj{hasNext: hi < boundHi, hasPrev: lo > boundLo}
	if len(c.cursors) > 0 {
		c.info.start, c.info.end = c.cursors[0], c.cursors[len(c.cursors)-1]
	}
	if c.nodes == nil {
		c.nodes = []any{}
	}
	return c, nil
}

// encodeCursor and decodeCursor map list positions to opaque cursors.
func encodeCursor(i int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:v2:" + strconv.Itoa(i)))
}

func decodeCursor(c string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:v2:"))
	return n, err == nil && n >= 0
}

// updateRefs runs the updateRefs mutation (documented in the schema:
// atomic; beforeOid must match, all zeros meaning the ref must not exist;
// an all-zero afterOid deletes; force allows a non-fast-forward). A
// beforeOid that does not match is STALE_DATA (modeled on
// createCommitOnBranch's expectedHeadOid, assumed); other refusals are
// UNPROCESSABLE, the Workflows permission FORBIDDEN with GitHub's
// "refusing to allow …" message, rule violations UNPROCESSABLE
// "Repository rule violations found" (all assumed). Nothing moves unless
// every update is accepted.
func (e *executor) updateRefs(args map[string]any) (any, *gqlError) {
	s := e.s
	in, _ := args["input"].(map[string]any)
	rid, _ := in["repositoryId"].(string)
	typ, id, ok := parseNodeID(rid)
	r := s.repos[id]
	if r != nil && ok && typ == "Repository" {
		s.noteScope(e.id, r)
	}
	if !ok || typ != "Repository" || r == nil || r.deleted || !s.canSee(e.id, r) {
		return nil, notFoundErr("Could not resolve to a node with the global id of '" + rid + "'")
	}
	if s.need(e.id, r, "contents", Write) != nil {
		return nil, forbiddenErr(e.id)
	}
	if r.archived {
		return nil, unprocessableErr("Repository was archived so is read-only.")
	}
	rg := s.repoGit(r)
	current, err := rg.refs(e.ctx, "refs/")
	if err != nil {
		return nil, &gqlError{Type: "INTERNAL", Message: "Something went wrong while executing your query."}
	}
	empty := true
	for ref := range current {
		empty = empty && !strings.HasPrefix(ref, "refs/heads/")
	}
	if empty {
		return nil, unprocessableErr("Git Repository is empty.")
	}
	updates, _ := in["refUpdates"].([]any)
	var changes []refChange
	seen := map[string]bool{}
	for _, raw := range updates {
		u, _ := raw.(map[string]any)
		name, _ := u["name"].(string)
		after, _ := u["afterOid"].(string)
		force, _ := u["force"].(bool)
		after = strings.ToLower(after)
		switch {
		case !strings.HasPrefix(name, "refs/") || strings.Count(name, "/") < 2:
			return nil, unprocessableErr("Invalid reference name: " + name)
		case strings.HasPrefix(name, "refs/pull/"):
			return nil, unprocessableErr("Reference cannot be updated: " + name)
		case seen[name]:
			return nil, unprocessableErr("Reference " + name + " is updated twice")
		case !isHexID(after):
			return nil, unprocessableErr("Invalid object id: " + after)
		}
		seen[name] = true
		cur := current[name]
		if before, ok := u["beforeOid"].(string); ok {
			before = strings.ToLower(before)
			if (isZeroID(before) && cur != "") || (!isZeroID(before) && cur != before) {
				return nil, &gqlError{Type: "STALE_DATA",
					Message: "Expected " + name + " to point to \"" + before + "\" but it did not. Pull and try again."}
			}
		}
		if isZeroID(after) {
			if cur == "" {
				return nil, unprocessableErr("Reference does not exist: " + name)
			}
			if br, isBranch := branchOf(name); isBranch && br == r.defaultBranch {
				return nil, unprocessableErr("Cannot delete the default branch")
			}
			changes = append(changes, refChange{ref: name, old: cur})
			continue
		}
		if t, _ := rg.objectType(e.ctx, after); t != "commit" {
			return nil, unprocessableErr("Object does not exist: " + after)
		}
		if cur != "" && !force {
			if ff, err := rg.isAncestor(e.ctx, cur, after); err != nil || !ff {
				return nil, unprocessableErr("Update is not a fast forward: " + name)
			}
		}
		changes = append(changes, refChange{ref: name, old: cur, new: after})
	}
	ref, refused := s.applyRefChanges(e.ctx, r, e.id, changes)
	switch {
	case ref != nil && ref.workflow != "":
		return nil, &gqlError{Type: "FORBIDDEN", Message: workflowMessage(e.id, ref.workflow)}
	case ref != nil:
		return nil, unprocessableErr("Repository rule violations found\n\n" + strings.Join(ref.rules, "\n\n") + "\n\n")
	case refused != nil && refused.status == http.StatusForbidden:
		return nil, &gqlError{Type: "RATE_LIMITED", Message: contentMessage}
	case refused != nil:
		return nil, unprocessableErr(describe(*refused))
	}
	return payloadObj{clientMutationID: in["clientMutationId"]}, nil
}
