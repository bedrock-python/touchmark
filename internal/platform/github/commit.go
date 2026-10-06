package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// stagePrefix starts every stage ref the driver deletes.
const stagePrefix = "refs/touchmark/"

// errIntegrity is wrapped by a commit the platform made otherwise than
// asked: the published tree must be the one touchmark computed.
var errIntegrity = errors.New("integrity")

// createCommit is the body of POST /repos/{owner}/{repo}/git/commits:
// without author, committer and signature, so that GitHub signs the commit
// as the App (https://docs.github.com/authentication/managing-commit-
// signature-verification/about-commit-signature-verification, "signature
// verification for bots").
type createCommit struct {
	Message string   `json:"message"`
	Tree    string   `json:"tree"`
	Parents []string `json:"parents"`
}

// apiCommit is the answer of POST /git/commits.
type apiCommit struct {
	SHA  string `json:"sha"`
	Tree *struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Verification *struct {
		Verified bool   `json:"verified"`
		Reason   string `json:"reason"`
	} `json:"verification"`
}

// refUpdate is one RefUpdate of updateRefs; a nil BeforeOID checks nothing.
type refUpdate struct {
	Name      string  `json:"name"`
	BeforeOID *string `json:"beforeOid,omitempty"`
	AfterOID  string  `json:"afterOid"`
	Force     bool    `json:"force"`
}

// Commit makes req's commit through the API and moves req.Branch to it,
// through a stage ref:
//
//  1. the core has pushed its own commit of req.Tree to req.Stage, so every
//     object of the tree is on the server;
//  2. POST /git/commits {message, tree, parents: [Parent]} without author,
//     committer or signature: GitHub signs it as the App; the answer's tree
//     must be req.Tree, else nothing moves (ClassUnknown);
//  3. an answer without verification.verified is tried once more; still
//     unsigned is ErrUnsigned (ClassUnsupported, Rule "cannot-sign") with
//     the commit's SHA, and nothing moves: GHES signs only with web commit
//     signing enabled, and an unsigned commit cannot go where signatures
//     are required;
//  4. GraphQL updateRefs, atomically: refs/heads/<Branch> from req.Expect
//     (0…0 for a new branch: CAS) to the commit, forced, and req.Stage
//     deleted (afterOid 0…0).
//
// Whether updateRefs deletes a ref outside refs/heads and refs/tags in the
// same call is unverified: when the combined call is refused, the branch
// is updated alone, then the stage ref deleted alone (updateRefs, then
// DELETE /git/refs), each best effort, and later commits of the run skip
// the combined try. A refused branch update is read back: a branch that no
// longer points at req.Expect is ClassConflict (a stale lease). The stage
// ref is deleted too after a tree mismatch and an unsigned commit; after
// another failure it stays for a retry, and the core's next push to it
// replaces it (it must push the stage ref with force).
//
// The branch never goes through the base: it moves from its old head to
// the new commit in one update, so an open pull request stays open.
func (t *appTarget) Commit(ctx context.Context, r platform.Repo, req platform.CommitRequest) (platform.Commit, error) {
	const op = "commit"
	if err := t.checkCommit(op, r, req); err != nil {
		return platform.Commit{}, err
	}
	a, err := t.auth(ctx, op)
	if err != nil {
		return platform.Commit{}, err
	}
	var c *apiCommit
	for attempt := range 2 {
		c, err = t.createCommit(ctx, op, a, req)
		if err != nil {
			if errors.Is(err, errIntegrity) {
				t.dropStage(ctx, op, a, req)
			}
			// Otherwise the stage ref stays for a retry of the core.
			return platform.Commit{}, err
		}
		if c.Verification != nil && c.Verification.Verified {
			break
		}
		if attempt == 1 {
			t.dropStage(ctx, op, a, req)
			reason := "no verification"
			if c.Verification != nil && c.Verification.Reason != "" {
				reason = c.Verification.Reason
			}
			return platform.Commit{SHA: strings.ToLower(c.SHA), Tree: strings.ToLower(req.Tree), CAS: true},
				&platform.Error{Op: op, Class: platform.ClassUnsupported, Rule: "cannot-sign",
					Err: fmt.Errorf("%s/%s: GitHub made commit %s unsigned (%s) twice; the branch did not move: %w",
						t.owner, t.name, c.SHA, reason, platform.ErrUnsigned)}
		}
	}
	sha := strings.ToLower(c.SHA)
	if err := t.moveBranch(ctx, op, a, req, sha); err != nil {
		return platform.Commit{}, err
	}
	return platform.Commit{SHA: sha, Tree: strings.ToLower(req.Tree), Verified: true, CAS: true}, nil
}

// checkCommit refuses a request the driver cannot carry out safely.
func (t *appTarget) checkCommit(op string, r platform.Repo, req platform.CommitRequest) error {
	if id, err := repoID(op, r); err != nil || id != t.repoID {
		return invalid(op, "the target writer is for repository %d, not %s (%s)", t.repoID, r.Path, r.ID)
	}
	switch {
	case req.Branch == "" || strings.HasPrefix(req.Branch, "refs/") || strings.ContainsAny(req.Branch, " ~^:?*[\\\x00") || strings.Contains(req.Branch, ".."):
		return invalid(op, "branch %q is not a branch name", req.Branch)
	case !isHexOID(req.Parent) || !isHexOID(req.Tree):
		return invalid(op, "parent %q and tree %q must be full object ids", req.Parent, req.Tree)
	case req.Expect != "" && !isHexOID(req.Expect):
		return invalid(op, "expected head %q is not a full object id", req.Expect)
	case strings.TrimSpace(req.Message) == "":
		return invalid(op, "the commit message is empty")
	case req.Stage != "" && (!strings.HasPrefix(req.Stage, stagePrefix) || strings.Contains(req.Stage, "..") ||
		strings.ContainsAny(req.Stage, " ~^:?*[\\\x00")):
		return invalid(op, "stage ref %q is not under %s", req.Stage, stagePrefix)
	}
	return nil
}

// createCommit posts the commit and checks the answer against the request.
func (t *appTarget) createCommit(ctx context.Context, op string, a *httpx.Auth, req platform.CommitRequest) (*apiCommit, error) {
	var c apiCommit
	_, err := t.c.call(ctx, op, http.MethodPost, t.c.repoURL(t.owner, t.name, "git", "commits"), nil, a,
		createCommit{Message: req.Message, Tree: req.Tree, Parents: []string{req.Parent}}, &c)
	if err != nil {
		return nil, err
	}
	switch {
	case !isHexOID(c.SHA) || c.Tree == nil:
		return nil, shapeError(op, "a commit without sha or tree")
	case !strings.EqualFold(c.Tree.SHA, req.Tree):
		return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%w: GitHub made commit %s of tree %s, not of the tree %s touchmark built; the branch did not move", errIntegrity, c.SHA, c.Tree.SHA, req.Tree)}
	case len(c.Parents) != 1 || !strings.EqualFold(c.Parents[0].SHA, req.Parent):
		return nil, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: fmt.Errorf("%w: GitHub made commit %s on other parents than %s; the branch did not move", errIntegrity, c.SHA, req.Parent)}
	}
	return &c, nil
}

// moveBranch runs updateRefs: the branch from req.Expect to sha and the
// stage ref deleted, in one call; the fallbacks of Commit when it is
// refused.
func (t *appTarget) moveBranch(ctx context.Context, op string, a *httpx.Auth, req platform.CommitRequest, sha string) error {
	zero := strings.Repeat("0", len(req.Parent))
	before := strings.ToLower(req.Expect)
	if before == "" {
		before = zero
	}
	branch := refUpdate{Name: "refs/heads/" + req.Branch, BeforeOID: &before, AfterOID: sha, Force: true}
	split := t.c.stageSplit()
	if req.Stage != "" && !split {
		err := t.updateRefs(ctx, op, a, []refUpdate{branch, {Name: req.Stage, AfterOID: zero, Force: true}})
		if err == nil || fatal(err) {
			return err
		}
	}
	// The combined call was refused (or is known to be): the branch alone.
	err := t.updateRefs(ctx, op, a, []refUpdate{branch})
	if err == nil {
		if req.Stage != "" {
			if !split {
				// The branch alone passes where both did not: the stage
				// ref was refused. Later commits of the run skip the try.
				t.c.setStageSplit()
			}
			t.dropStage(ctx, op, a, req)
		}
		return nil
	}
	if fatal(err) {
		return err
	}
	// Refused: a stale lease, or a rule. The branch tells which.
	head, rerr := t.branchHead(ctx, op, a, req.Branch)
	if rerr != nil {
		return errors.Join(err, rerr)
	}
	if head != strings.ToLower(req.Expect) {
		return &platform.Error{Op: op, Class: platform.ClassConflict,
			Err: fmt.Errorf("%s/%s: branch %s is at %q, not at %q: %w", t.owner, t.name, req.Branch, head, req.Expect, err)}
	}
	return err
}

// updateRefs runs the updateRefs mutation on the target repository.
func (t *appTarget) updateRefs(ctx context.Context, op string, a *httpx.Auth, updates []refUpdate) error {
	node, err := t.repoNode(ctx, op, a)
	if err != nil {
		return err
	}
	input := map[string]any{"repositoryId": node, "refUpdates": updates}
	errs, err := t.c.graphql(ctx, op, a, mutationUpdateRefs, map[string]any{"input": input}, nil)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		return t.c.fieldError(op, &errs[0])
	}
	return nil
}

// dropStage deletes the stage ref, best effort: updateRefs, else DELETE
// /git/refs/<ref without "refs/"> (whether the REST API takes a ref
// outside heads and tags is unverified).
func (t *appTarget) dropStage(ctx context.Context, op string, a *httpx.Auth, req platform.CommitRequest) {
	if req.Stage == "" {
		return
	}
	zero := strings.Repeat("0", len(req.Parent))
	if !t.c.stageSplit() && t.updateRefs(ctx, op, a, []refUpdate{{Name: req.Stage, AfterOID: zero, Force: true}}) == nil {
		return
	}
	segments := append([]string{"git", "refs"}, strings.Split(strings.TrimPrefix(req.Stage, "refs/"), "/")...)
	_, _ = t.c.call(ctx, op, http.MethodDelete, t.c.repoURL(t.owner, t.name, segments...), nil, a, nil, nil)
}

// stageSplit reports whether updateRefs refused to delete a stage ref
// with the branch update in this run.
func (c *client) stageSplit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.splitStage
}

// setStageSplit remembers that it did.
func (c *client) setStageSplit() {
	c.mu.Lock()
	c.splitStage = true
	c.mu.Unlock()
}

// repoNode returns the repository's GraphQL node id (GET /repos: node_id),
// read once.
func (t *target) repoNode(ctx context.Context, op string, a *httpx.Auth) (string, error) {
	t.mu.Lock()
	node := t.nodeID
	t.mu.Unlock()
	if node != "" {
		return node, nil
	}
	var r apiRepo
	if _, err := t.c.get(ctx, op, t.c.repoURL(t.owner, t.name), nil, a, &r); err != nil {
		return "", err
	}
	if r.NodeID == "" || r.ID != t.repoID {
		return "", shapeError(op, "%s/%s: no node id for repository %d", t.owner, t.name, t.repoID)
	}
	t.mu.Lock()
	t.nodeID = r.NodeID
	t.mu.Unlock()
	return r.NodeID, nil
}

// apiGitRef is GET /repos/{owner}/{repo}/git/ref/heads/{branch}.
type apiGitRef struct {
	Ref    string `json:"ref"`
	Object *struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// branchHead returns the commit branch points at (GET
// /git/ref/heads/<branch>: one ref, 404 when there is none), "" when it
// does not exist.
func (t *appTarget) branchHead(ctx context.Context, op string, a *httpx.Auth, branch string) (string, error) {
	segments := append([]string{"git", "ref", "heads"}, strings.Split(branch, "/")...)
	var ref apiGitRef
	_, err := t.c.get(ctx, op, t.c.repoURL(t.owner, t.name, segments...), nil, a, &ref)
	switch {
	case platform.ClassOf(err) == platform.ClassNotFound:
		return "", nil
	case err != nil:
		return "", err
	case ref.Object == nil || !isHexOID(ref.Object.SHA) || ref.Ref != "refs/heads/"+branch:
		return "", shapeError(op, "GET /git/ref/heads/%s: not the branch's ref", branch)
	}
	return strings.ToLower(ref.Object.SHA), nil
}
