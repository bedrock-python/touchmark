package ghfake

import (
	"context"
	"encoding/base64"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxContentsBlob is the largest blob the contents API returns as JSON.
const maxContentsBlob = 1 << 20

// wrap64 encodes data in base64 with a newline every 60 characters and at
// the end, as GitHub's contents and blobs APIs do.
func wrap64(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(enc) > 60 {
		b.WriteString(enc[:60] + "\n")
		enc = enc[60:]
	}
	if enc != "" {
		b.WriteString(enc + "\n")
	}
	return b.String()
}

// media is the custom media type an Accept header asks for: "raw",
// "object", or "" for JSON.
func media(r *http.Request) string {
	accept := r.Header.Get("Accept")
	switch {
	case strings.Contains(accept, "application/vnd.github.raw"), strings.Contains(accept, "application/vnd.github.v3.raw"):
		return "raw"
	case strings.Contains(accept, "application/vnd.github.object"):
		return "object"
	}
	return ""
}

// rawResponse answers raw content.
func rawResponse(data []byte, sha string) response {
	return response{status: http.StatusOK, raw: data, ctype: "application/vnd.github.raw+json; charset=utf-8",
		header: http.Header{"Etag": {`"` + sha + `"`}}}
}

// commitAt resolves the ref query parameter ("" for the default branch)
// of a read. Called with mu held.
func (c *call) commitAt(ctx context.Context, r *repo, ref string) (string, *response) {
	s := c.s
	rg := s.repoGit(r)
	if ref == "" {
		tip := s.tip(ctx, r, r.defaultBranch)
		if tip == "" {
			resp := notFound()
			resp.body.(map[string]any)["message"] = "This repository is empty."
			return "", &resp
		}
		return tip, nil
	}
	id, ok, err := rg.resolve(ctx, ref)
	if err != nil {
		resp := apiError(http.StatusInternalServerError, "Server Error")
		return "", &resp
	}
	if !ok {
		resp := notFound()
		resp.body.(map[string]any)["message"] = "No commit found for the ref " + ref
		return "", &resp
	}
	return id, nil
}

// getContents answers GET /repos/{owner}/{repo}/contents/{path}: a file
// (base64 JSON up to 1 MB; the object media type gives an empty content
// with encoding "none" above that, the raw one the bytes), a directory
// listing, a submodule (type "submodule"), or a symlink. A symlink whose
// target is a regular file of the repository answers that file's size and
// content under the link's name, path and sha (observed read-only
// 2026-09-29); any other symlink answers type "symlink" with its target
// (documented).
func getContents(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	commit, refused := c.commitAt(ctx, r, c.query.Get("ref"))
	if refused != nil {
		return *refused
	}
	rg := s.repoGit(r)
	p := strings.Trim(c.param("path"), "/")
	kind := media(c.r)
	if p == "" {
		return c.dirListing(ctx, rg, r, commit, "", kind)
	}
	e, found, err := rg.entryAt(ctx, commit, p)
	switch {
	case err != nil:
		return apiError(http.StatusInternalServerError, "Server Error")
	case !found:
		return notFound()
	}
	switch e.mode {
	case modeTree:
		return c.dirListing(ctx, rg, r, commit, p, kind)
	case ModeGitlink:
		return ok(c.submoduleJSON(ctx, rg, r, commit, e))
	case ModeSymlink:
		link, err := rg.catFile(ctx, "blob", e.oid)
		if err != nil {
			return apiError(http.StatusInternalServerError, "Server Error")
		}
		target := path.Clean(path.Join(path.Dir(p), string(link)))
		if !path.IsAbs(string(link)) && target != ".." && !strings.HasPrefix(target, "../") {
			if te, isFile, err := rg.entryAt(ctx, commit, target); err == nil && isFile && (te.mode == ModeFile || te.mode == ModeExecutable) {
				data, err := rg.catFile(ctx, "blob", te.oid)
				if err != nil {
					return apiError(http.StatusInternalServerError, "Server Error")
				}
				if kind == "raw" {
					return rawResponse(data, te.oid)
				}
				out := c.fileJSON(r, commit, e, data)
				out["size"] = len(data)
				return ok(out)
			}
		}
		if kind == "raw" {
			return rawResponse(link, e.oid)
		}
		out := c.entryJSON(r, commit, e, "symlink")
		out["target"] = string(link)
		return ok(out)
	}
	data, err := rg.catFile(ctx, "blob", e.oid)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	switch {
	case kind == "raw":
		return rawResponse(data, e.oid)
	case len(data) > maxContentsBlob && kind != "object":
		return response{status: http.StatusForbidden, body: map[string]any{
			"message": "This API returns blobs up to 1 MB in size. The requested blob is too large to fetch via the API, " +
				"but you can use the Git Data API to request blobs up to 100 MB in size.",
			"errors":            []any{map[string]any{"resource": "Blob", "field": "data", "code": "too_large"}},
			"documentation_url": docURL, "status": "403"}}
	}
	return ok(c.fileJSON(r, commit, e, data))
}

// entryJSON is the common part of a contents entry.
func (c *call) entryJSON(r *repo, commit string, e treeEntry, typ string) map[string]any {
	api := c.base + "/repos/" + escapePath(r.path())
	size := e.size
	if size < 0 {
		size = 0
	}
	out := map[string]any{"type": typ, "name": path.Base(e.path), "path": e.path, "sha": e.oid, "size": size,
		"url":      api + "/contents/" + escapePath(e.path) + "?ref=" + commit,
		"html_url": c.s.http.URL + "/" + r.path() + "/blob/" + commit + "/" + e.path,
		"git_url":  api + "/git/blobs/" + e.oid, "download_url": nil}
	if typ == "file" {
		out["download_url"] = c.s.http.URL + "/" + r.path() + "/raw/" + commit + "/" + e.path
	}
	if typ == "dir" {
		out["git_url"] = api + "/git/trees/" + e.oid
	}
	return out
}

// fileJSON is a file with its content.
func (c *call) fileJSON(r *repo, commit string, e treeEntry, data []byte) map[string]any {
	out := c.entryJSON(r, commit, e, "file")
	out["size"] = len(data)
	if len(data) > maxContentsBlob {
		out["content"], out["encoding"] = "", "none"
		return out
	}
	out["content"], out["encoding"] = wrap64(data), "base64"
	return out
}

// submoduleJSON is a submodule entry, with its URL from .gitmodules when
// the commit has one.
func (c *call) submoduleJSON(ctx context.Context, rg *rgit, r *repo, commit string, e treeEntry) map[string]any {
	out := c.entryJSON(r, commit, e, "submodule")
	out["git_url"] = nil
	out["submodule_git_url"] = nil
	if url := gitmoduleURL(ctx, rg, commit, e.path); url != "" {
		out["submodule_git_url"] = url
	}
	return out
}

// gitmoduleURL reads the url of the submodule at path from .gitmodules.
func gitmoduleURL(ctx context.Context, rg *rgit, commit, p string) string {
	out, err := rg.g.Run(ctx, nil, "config", "--blob", commit+":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`)
	if err != nil {
		return ""
	}
	paths, urls := map[string]string{}, map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		name, field := key[:strings.LastIndexByte(key, '.')], key[strings.LastIndexByte(key, '.')+1:]
		if field == "path" {
			paths[name] = value
		} else {
			urls[name] = value
		}
	}
	for name, sp := range paths {
		if sp == p {
			return urls[name]
		}
	}
	return ""
}

// dirListing answers a directory: its entries (at most 1000), or with the
// object media type an object with them as "entries".
func (c *call) dirListing(ctx context.Context, rg *rgit, r *repo, commit, dir, kind string) response {
	list, err := rg.lsTree(ctx, commit, dir, false)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	entries := []any{}
	for i, e := range list {
		if i == 1000 {
			break
		}
		if dir != "" {
			e.path = dir + "/" + e.path
		}
		typ := "file"
		switch e.mode {
		case modeTree:
			typ = "dir"
		case ModeSymlink:
			typ = "symlink"
		case ModeGitlink:
			typ = "submodule"
		}
		entries = append(entries, c.entryJSON(r, commit, e, typ))
	}
	if kind == "object" {
		out := map[string]any{"type": "dir", "name": path.Base(dir), "path": dir, "entries": entries}
		return ok(out)
	}
	return ok(entries)
}

// getTree answers GET /repos/{owner}/{repo}/git/trees/{tree_sha}: a tree,
// commit or branch, recursively with recursive set (trees included, blobs
// with size, submodules without url), truncated above
// Options.MaxTreeEntries. An empty repository answers 409 "Git Repository
// is empty." (the status from GitHub's documentation).
func getTree(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	if refs, err := rg.refs(ctx, "refs/heads/"); err == nil && len(refs) == 0 {
		return apiError(http.StatusConflict, "Git Repository is empty.")
	}
	want := c.param("sha")
	var tree string
	if isHexID(want) {
		if typ, _ := rg.objectType(ctx, want); typ == "tree" || typ == "commit" {
			tree, _ = rg.treeOf(ctx, want)
		}
	} else if id, ok, err := rg.resolve(ctx, want); err == nil && ok {
		tree, _ = rg.treeOf(ctx, id)
	}
	if tree == "" {
		return notFound()
	}
	recursive := c.query.Get("recursive") != ""
	list, err := rg.lsTree(ctx, tree, "", recursive)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	limit := s.opts.MaxTreeEntries
	if limit <= 0 {
		limit = 100000
	}
	truncated := len(list) > limit
	if truncated {
		list = list[:limit]
	}
	api := c.base + "/repos/" + escapePath(r.path())
	entries := []any{}
	for _, e := range list {
		item := map[string]any{"path": e.path, "mode": e.mode, "type": e.typ, "sha": e.oid}
		switch e.typ {
		case "blob":
			item["size"], item["url"] = e.size, api+"/git/blobs/"+e.oid
		case "tree":
			item["url"] = api + "/git/trees/" + e.oid
		}
		entries = append(entries, item)
	}
	return ok(map[string]any{"sha": tree, "url": api + "/git/trees/" + tree, "tree": entries, "truncated": truncated})
}

// getBlob answers GET /repos/{owner}/{repo}/git/blobs/{file_sha}.
func getBlob(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	sha := strings.ToLower(c.param("sha"))
	if typ, _ := rg.objectType(ctx, sha); typ != "blob" {
		return notFound()
	}
	data, err := rg.catFile(ctx, "blob", sha)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	if media(c.r) == "raw" {
		return rawResponse(data, sha)
	}
	return ok(map[string]any{"sha": sha, "node_id": "", "size": len(data),
		"url": c.base + "/repos/" + escapePath(r.path()) + "/git/blobs/" + sha, "content": wrap64(data), "encoding": "base64"})
}

// identJSON is a git author or committer.
func identJSON(i ident) map[string]any {
	return map[string]any{"name": i.name, "email": i.email, "date": fmtTime(i.when)}
}

// gitCommitJSON is the Git Data commit object with its verification.
func (c *call) gitCommitJSON(r *repo, info commitInfo) map[string]any {
	api := c.base + "/repos/" + escapePath(r.path())
	parents := []any{}
	for _, p := range info.parents {
		parents = append(parents, map[string]any{"sha": p, "url": api + "/git/commits/" + p,
			"html_url": c.s.http.URL + "/" + r.path() + "/commit/" + p})
	}
	return map[string]any{"sha": info.id, "node_id": "", "url": api + "/git/commits/" + info.id,
		"html_url": c.s.http.URL + "/" + r.path() + "/commit/" + info.id,
		"author":   identJSON(info.author), "committer": identJSON(info.committer), "message": info.message,
		"tree":    map[string]any{"sha": info.tree, "url": api + "/git/trees/" + info.tree},
		"parents": parents, "verification": c.s.verify(info).json()}
}

// getGitCommit answers GET /repos/{owner}/{repo}/git/commits/{commit_sha}.
func getGitCommit(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	sha := strings.ToLower(c.param("sha"))
	if typ, _ := rg.objectType(ctx, sha); typ != "commit" {
		return notFound()
	}
	info, err := rg.readCommit(ctx, sha)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	return ok(c.gitCommitJSON(r, info))
}

// createGitCommit answers POST /repos/{owner}/{repo}/git/commits. Without
// author, committer and signature, a commit by an installation token is
// the App's: author "<slug>[bot]" with its noreply address, committer
// "GitHub <noreply@github.com>", signed by GitHub (verification valid;
// documented for bots, observed read-only 2026-09-29) unless the flavor
// does not sign API commits (GHES without web commit signing). A personal
// access token's commit is its user's and unsigned (public reports;
// assumed). Given author or committer are used as is; a given signature
// becomes the gpgsig header.
func createGitCommit(c *call) response {
	var in struct {
		Message   *string  `json:"message"`
		Tree      string   `json:"tree"`
		Parents   []string `json:"parents"`
		Author    *gitUser `json:"author"`
		Committer *gitUser `json:"committer"`
		Signature string   `json:"signature"`
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
	if refused := s.need(c.id, r, "contents", Write); refused != nil {
		return *refused
	}
	if r.archived {
		return apiError(http.StatusForbidden, "Repository was archived so is read-only.")
	}
	if in.Message == nil {
		return validation(map[string]any{"resource": "Commit", "code": "missing_field", "field": "message"})
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	tree := strings.ToLower(in.Tree)
	if typ, _ := rg.objectType(ctx, tree); typ != "tree" {
		return unprocessable("Tree SHA does not exist")
	}
	for i, p := range in.Parents {
		in.Parents[i] = strings.ToLower(p)
		if typ, _ := rg.objectType(ctx, in.Parents[i]); typ != "commit" {
			return unprocessable("Parent SHA does not exist or is not a commit object")
		}
	}
	if refused := s.limits.contentCreated(s.now(), c.id); refused != nil {
		return *refused
	}
	now := s.now().Truncate(time.Second)
	who := c.id.who
	self := ident{name: who.login, email: who.noreply(), when: now}
	custom := in.Author != nil || in.Committer != nil || in.Signature != ""
	author := self
	if in.Author != nil {
		author = in.Author.ident(now)
	}
	committer := author
	signs := c.id.installation() && !custom && s.signsAPICommits()
	if c.id.installation() && !custom {
		committer = ident{name: webFlowName, email: webFlowEmail, when: now}
	}
	if in.Committer != nil {
		committer = in.Committer.ident(now)
	}
	sig := in.Signature
	text := commitText(tree, in.Parents, author, committer, sig, *in.Message)
	if signs {
		text = commitText(tree, in.Parents, author, committer, s.webFlowSign(text), *in.Message)
	}
	id, err := rg.writeObject(ctx, "commit", []byte(text))
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	info, err := rg.readCommit(ctx, id)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	return created(c.gitCommitJSON(r, info))
}

// signsAPICommits reports whether the App's API commits are signed.
func (s *Server) signsAPICommits() bool {
	if s.opts.WebCommitSigning != nil {
		return *s.opts.WebCommitSigning
	}
	return s.flavor == DotCom
}

// gitUser is an author or committer in a request.
type gitUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// ident converts u, dated now unless it has a date.
func (u *gitUser) ident(now time.Time) ident {
	when := now
	if t, err := time.Parse(time.RFC3339, u.Date); err == nil {
		when = t
	}
	return ident{name: u.Name, email: u.Email, when: when}
}

// getCommit answers GET /repos/{owner}/{repo}/commits/{ref} with the
// fields a driver reads.
func getCommit(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	id, found, err := rg.resolve(ctx, c.param("ref"))
	if err != nil || !found {
		return unprocessable("No commit found for SHA: " + c.param("ref"))
	}
	info, err := rg.readCommit(ctx, id)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	git := c.gitCommitJSON(r, info)
	delete(git, "sha")
	delete(git, "parents")
	delete(git, "html_url")
	return ok(map[string]any{"sha": id, "node_id": "", "commit": git,
		"url": c.base + "/repos/" + escapePath(r.path()) + "/commits/" + id, "parents": c.gitCommitJSON(r, info)["parents"],
		"author": c.accountJSON(s.byEmail(info.author.email)), "committer": c.accountJSON(s.byEmail(info.committer.email))})
}

// byEmail finds the account an email belongs to. Called with mu held.
func (s *Server) byEmail(email string) *account {
	for _, id := range sortedIDs(s.accounts) {
		if a := s.accounts[id]; owns(a, email) {
			return a
		}
	}
	return nil
}

// compareCommits answers GET /repos/{owner}/{repo}/compare/{base}...{head}
// with status, ahead_by, behind_by and the merge base.
func compareCommits(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	base, head, found := strings.Cut(c.param("basehead"), "...")
	if !found {
		return notFound()
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	b, ok1, _ := rg.resolve(ctx, base)
	h, ok2, _ := rg.resolve(ctx, head)
	if !ok1 || !ok2 {
		return notFound()
	}
	count := func(from, to string) int {
		out, err := rg.g.Run(ctx, nil, "rev-list", "--count", to, "^"+from)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return n
	}
	mb := ""
	if out, err := rg.g.Run(ctx, nil, "merge-base", b, h); err == nil {
		mb, _ = hexOut("git merge-base", out)
	}
	ahead, behind := count(b, h), count(h, b)
	status := "diverged"
	switch {
	case ahead == 0 && behind == 0:
		status = "identical"
	case behind == 0:
		status = "ahead"
	case ahead == 0:
		status = "behind"
	}
	var mbc any
	if mb != "" {
		mbc = map[string]any{"sha": mb}
	}
	return ok(map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": ahead,
		"base_commit": map[string]any{"sha": b}, "merge_base_commit": mbc})
}

// branchJSON is a branch.
func (c *call) branchJSON(r *repo, name, sha string, full bool) map[string]any {
	api := c.base + "/repos/" + escapePath(r.path())
	commit := map[string]any{"sha": sha, "url": api + "/commits/" + sha}
	out := map[string]any{"name": name, "commit": commit, "protected": len(c.s.activeRules(r, name)) > 0}
	if full {
		out["_links"] = map[string]any{"self": api + "/branches/" + name, "html": c.s.http.URL + "/" + r.path() + "/tree/" + name}
	}
	return out
}

// listBranches answers GET /repos/{owner}/{repo}/branches, by name.
func listBranches(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	branches, err := s.repoGit(r).branches(ctx)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	names := make([]string, 0, len(branches))
	for name := range branches {
		names = append(names, name)
	}
	slices.Sort(names)
	lo, hi, h, _ := c.page(len(names), c.base+"/repositories/"+strconv.FormatInt(r.id, 10)+"/branches")
	out := []any{}
	for _, name := range names[lo:hi] {
		out = append(out, c.branchJSON(r, name, branches[name], false))
	}
	return response{status: http.StatusOK, body: out, header: h}
}

// getBranch answers GET /repos/{owner}/{repo}/branches/{branch}.
func getBranch(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	tip := s.tip(ctx, r, c.param("branch"))
	if tip == "" {
		resp := notFound()
		resp.body.(map[string]any)["message"] = "Branch not found"
		return resp
	}
	return ok(c.branchJSON(r, c.param("branch"), tip, true))
}

// refJSON is a git reference.
func (c *call) refJSON(ctx context.Context, r *repo, ref, sha string) map[string]any {
	api := c.base + "/repos/" + escapePath(r.path())
	typ, _ := c.s.repoGit(r).objectType(ctx, sha)
	return map[string]any{"ref": ref, "node_id": nodeID("Ref", r.id), "url": api + "/git/" + ref,
		"object": map[string]any{"sha": sha, "type": typ, "url": api + "/git/" + typ + "s/" + sha}}
}

// getRef answers GET /repos/{owner}/{repo}/git/ref/{ref} ("heads/main").
func getRef(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	ref := "refs/" + c.param("ref")
	refs, err := s.repoGit(r).refs(ctx, ref)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	sha, found := refs[ref]
	if !found {
		return notFound()
	}
	return ok(c.refJSON(ctx, r, ref, sha))
}

// matchingRefs answers GET /repos/{owner}/{repo}/git/matching-refs/{ref}:
// the refs starting with refs/<ref>, sorted.
func matchingRefs(c *call) response {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	r, resp := c.repo()
	if resp != nil {
		return *resp
	}
	if refused := s.need(c.id, r, "contents", Read); refused != nil {
		return *refused
	}
	ctx, cancel := c.ctx()
	defer cancel()
	prefix := "refs/" + c.param("ref")
	all, err := s.repoGit(r).refs(ctx, "refs/")
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	var names []string
	for ref := range all {
		if strings.HasPrefix(ref, prefix) {
			names = append(names, ref)
		}
	}
	slices.Sort(names)
	out := []any{}
	for _, ref := range names {
		out = append(out, c.refJSON(ctx, r, ref, all[ref]))
	}
	return ok(out)
}

// createRef answers POST /repos/{owner}/{repo}/git/refs.
func createRef(c *call) response {
	var in struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	if !strings.HasPrefix(in.Ref, "refs/") || strings.Count(in.Ref, "/") < 2 {
		return unprocessable("Reference name must start with 'refs/' and have at least two slashes.")
	}
	return c.refWrite(in.Ref, func(ctx context.Context, rg *rgit, old string) (refChange, *response) {
		if old != "" {
			resp := unprocessable("Reference already exists")
			return refChange{}, &resp
		}
		sha := strings.ToLower(in.SHA)
		if typ, _ := rg.objectType(ctx, sha); typ == "" {
			resp := unprocessable("Object does not exist")
			return refChange{}, &resp
		}
		return refChange{ref: in.Ref, new: sha}, nil
	}, http.StatusCreated)
}

// updateRef answers PATCH /repos/{owner}/{repo}/git/refs/{ref}: a
// fast-forward unless force (422 "Update is not a fast forward").
func updateRef(c *call) response {
	var in struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	if resp, ok := c.decode(&in); !ok {
		return resp
	}
	ref := "refs/" + c.param("ref")
	return c.refWrite(ref, func(ctx context.Context, rg *rgit, old string) (refChange, *response) {
		if old == "" {
			resp := unprocessable("Reference does not exist")
			return refChange{}, &resp
		}
		sha := strings.ToLower(in.SHA)
		if typ, _ := rg.objectType(ctx, sha); typ != "commit" {
			resp := unprocessable("Object does not exist")
			return refChange{}, &resp
		}
		if !in.Force {
			if ff, err := rg.isAncestor(ctx, old, sha); err != nil || !ff {
				resp := unprocessable("Update is not a fast forward")
				return refChange{}, &resp
			}
		}
		return refChange{ref: ref, old: old, new: sha}, nil
	}, http.StatusOK)
}

// deleteRef answers DELETE /repos/{owner}/{repo}/git/refs/{ref}; the
// default branch cannot be deleted (422).
func deleteRef(c *call) response {
	ref := "refs/" + c.param("ref")
	return c.refWrite(ref, func(ctx context.Context, rg *rgit, old string) (refChange, *response) {
		if old == "" {
			resp := unprocessable("Reference does not exist")
			return refChange{}, &resp
		}
		return refChange{ref: ref, old: old}, nil
	}, http.StatusNoContent)
}

// refWrite runs one REST ref change: plan returns the change against the
// ref's current value; the change then meets the Workflows permission and
// the rulesets, is applied atomically, and pull requests follow.
func (c *call) refWrite(ref string, plan func(context.Context, *rgit, string) (refChange, *response), status int) response {
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
	if r.archived {
		return apiError(http.StatusForbidden, "Repository was archived so is read-only.")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	rg := s.repoGit(r)
	if refs, err := rg.refs(ctx, "refs/heads/"); err == nil && len(refs) == 0 {
		return apiError(http.StatusConflict, "Git Repository is empty.")
	}
	if strings.HasPrefix(ref, "refs/pull/") {
		return unprocessable("Reference cannot be updated")
	}
	refs, err := rg.refs(ctx, ref)
	if err != nil {
		return apiError(http.StatusInternalServerError, "Server Error")
	}
	change, refused := plan(ctx, rg, refs[ref])
	if refused != nil {
		return *refused
	}
	if br, isBranch := branchOf(ref); isBranch && change.new == "" && br == r.defaultBranch {
		return unprocessable("Cannot delete the default branch")
	}
	if _, refused := s.applyRefChanges(ctx, r, c.id, []refChange{change}); refused != nil {
		return *refused
	}
	if change.new == "" {
		return noContent()
	}
	return response{status: status, body: c.refJSON(ctx, r, ref, change.new)}
}

// applyRefChanges checks and applies API ref changes of r by id in one
// transaction: the Workflows permission and rulesets first (the refusal
// of the first failing change is returned with its reason), then
// content-creation limits for created or updated refs, then pull requests
// follow. Called with gitMu and mu held.
func (s *Server) applyRefChanges(ctx context.Context, r *repo, id *identity, changes []refChange) (*refusal, *response) {
	rg := s.repoGit(r)
	for _, ch := range changes {
		ref, err := s.checkUpdate(ctx, rg, r, id, ch)
		if err != nil {
			resp := apiError(http.StatusInternalServerError, "Server Error")
			return nil, &resp
		}
		if ref != nil {
			resp := apiRefusal(id, ref)
			return ref, &resp
		}
	}
	for _, ch := range changes {
		if ch.new != "" {
			if refused := s.limits.contentCreated(s.now(), id); refused != nil {
				return nil, refused
			}
			break
		}
	}
	txs := make([]refTx, len(changes))
	for i, ch := range changes {
		txs[i] = refTx(ch)
	}
	if err := rg.updateRefs(ctx, txs); err != nil {
		resp := unprocessable("Reference update failed")
		return nil, &resp
	}
	if err := s.refsMoved(ctx, r, changes, id.who, true); err != nil {
		resp := apiError(http.StatusInternalServerError, "Server Error")
		return nil, &resp
	}
	return nil, nil
}
