package github

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// commitWorld is a repository with the git data and GraphQL ref updates
// of the stage ref (see appTarget.Commit), each step scriptable.
type commitWorld struct {
	f      *fixture
	tw     platform.TargetWriter
	mu     sync.Mutex
	posted []map[string]any
	// verified answers the n-th commit (from 0); tree the tree it reports
	// (the one asked for when "").
	verified func(n int) bool
	tree     string
	// updates are the refUpdates of every updateRefs; refuse decides one.
	updates [][]map[string]any
	refuse  func(refs []map[string]any) map[string]any
	// head is the branch's head for GET /git/ref.
	head string
}

// Object ids of the commit tests.
var (
	oidParent = strings.Repeat("1", 40)
	oidTree   = strings.Repeat("2", 40)
	oidHead   = strings.Repeat("3", 40)
	oidOther  = strings.Repeat("4", 40)
	oidZero   = strings.Repeat("0", 40)
)

const stageRef = "refs/touchmark/0123456789abcdef/stage"

func newCommitWorld(t *testing.T) *commitWorld {
	t.Helper()
	f := newFixture(t, fixtureOpts{kind: credApp, host: "github.com"})
	w := &commitWorld{f: f, verified: func(int) bool { return true }}
	w.tw = newTarget(t, f)
	f.json(http.MethodGet, "/repos/acme/api", http.StatusOK, repo(101, "acme/api"))
	f.handle(http.MethodPost, "/repos/acme/api/git/commits", func(rw http.ResponseWriter, r *http.Request) {
		if m, ok := f.tokenOf(r.Header.Get("Authorization")); !ok || !slices.Equal(m.repos, []int64{101}) || m.perms["contents"] != "write" {
			t.Errorf("a commit with %v", m)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.mu.Lock()
		n := len(w.posted)
		w.posted = append(w.posted, body)
		tree := w.tree
		w.mu.Unlock()
		if tree == "" {
			tree, _ = body["tree"].(string)
		}
		sha := strings.Repeat(string(rune('a'+n)), 40)
		writeJSON(rw, http.StatusCreated, map[string]any{"sha": sha, "node_id": "C_" + sha[:6], "tree": map[string]any{"sha": tree},
			"parents":      []any{map[string]any{"sha": oidParent}},
			"author":       map[string]any{"name": "touchmark-write[bot]", "email": "5001+touchmark-write[bot]@users.noreply.github.com"},
			"verification": map[string]any{"verified": w.verified(n), "reason": map[bool]string{true: "valid", false: "unsigned"}[w.verified(n)]}})
	})
	f.graphql("updateRefs(input: $input)", func(rw http.ResponseWriter, req gqlCall) {
		input := req.Variables["input"].(map[string]any)
		if input["repositoryId"] != "R_101" {
			t.Errorf("repositoryId %v", input["repositoryId"])
		}
		var refs []map[string]any
		for _, u := range input["refUpdates"].([]any) {
			refs = append(refs, u.(map[string]any))
		}
		w.mu.Lock()
		w.updates = append(w.updates, refs)
		refuse := w.refuse
		w.mu.Unlock()
		if refuse != nil {
			if e := refuse(refs); e != nil {
				gqlData(rw, map[string]any{"updateRefs": nil}, e)
				return
			}
		}
		gqlData(rw, map[string]any{"updateRefs": map[string]any{"clientMutationId": nil}})
	})
	f.handle(http.MethodGet, "/repos/acme/api/git/ref/heads/touchmark/acme", func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.Lock()
		head := w.head
		w.mu.Unlock()
		if head == "" {
			writeJSON(rw, http.StatusNotFound, notFoundBody)
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"ref": "refs/heads/touchmark/acme", "node_id": "REF_1",
			"object": map[string]any{"type": "commit", "sha": head}})
	})
	return w
}

func (w *commitWorld) commit(t *testing.T, req platform.CommitRequest) (platform.Commit, error) {
	t.Helper()
	return w.tw.(platform.Committer).Commit(t.Context(), apiRepoTarget, req)
}

// request is a CommitRequest of the tests.
func request(expect string) platform.CommitRequest {
	return platform.CommitRequest{Branch: syncBranch, Expect: expect, Parent: oidParent, Tree: oidTree,
		Message: "chore: sync engineering assets\n\nTouchmark-Hub: acme-eng@github.com/712345678\n", Stage: stageRef}
}

// branchUpdates returns the updates of refs/heads/<sync>.
func (w *commitWorld) branchUpdates() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, refs := range w.updates {
		for _, r := range refs {
			if r["name"] == "refs/heads/"+syncBranch {
				out = append(out, r)
			}
		}
	}
	return out
}

// TestCommit: a commit of the tree on the parent, without author,
// committer or signature; the branch moved from its head and the stage
// ref deleted in one updateRefs.
func TestCommit(t *testing.T) {
	w := newCommitWorld(t)
	got, err := w.commit(t, request(oidHead))
	if err != nil {
		t.Fatal(err)
	}
	want := platform.Commit{SHA: strings.Repeat("a", 40), Tree: oidTree, Verified: true, CAS: true}
	if got != want {
		t.Errorf("Commit = %+v, want %+v", got, want)
	}
	if len(w.posted) != 1 {
		t.Fatalf("%d commits", len(w.posted))
	}
	keys := make([]string, 0)
	for k := range w.posted[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"message", "parents", "tree"}) || w.posted[0]["tree"] != oidTree {
		t.Errorf("posted %v", w.posted[0])
	}
	if len(w.updates) != 1 || len(w.updates[0]) != 2 {
		t.Fatalf("updates %v", w.updates)
	}
	b, s := w.updates[0][0], w.updates[0][1]
	if b["name"] != "refs/heads/"+syncBranch || b["beforeOid"] != oidHead || b["afterOid"] != got.SHA || b["force"] != true {
		t.Errorf("branch update %v", b)
	}
	if s["name"] != stageRef || s["afterOid"] != oidZero || s["beforeOid"] != nil {
		t.Errorf("stage update %v", s)
	}

	// A new branch: beforeOid is zero.
	if _, err := w.commit(t, request("")); err != nil {
		t.Fatal(err)
	}
	if u := w.branchUpdates(); u[len(u)-1]["beforeOid"] != oidZero {
		t.Errorf("new branch: %v", u[len(u)-1])
	}
}

// TestCommitTreeMismatch: a commit of another tree never moves the branch.
func TestCommitTreeMismatch(t *testing.T) {
	w := newCommitWorld(t)
	w.tree = oidOther
	_, err := w.commit(t, request(oidHead))
	wantClass(t, "tree mismatch", err, platform.ClassUnknown, nil)
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("error %v", err)
	}
	if u := w.branchUpdates(); len(u) != 0 {
		t.Errorf("the branch moved: %v", u)
	}
	if len(w.updates) != 1 || w.updates[0][0]["name"] != stageRef {
		t.Errorf("the stage ref was not dropped: %v", w.updates)
	}
}

// TestCommitUnverified: an unsigned commit is made once more; a second
// unsigned one is ErrUnsigned and the branch stays.
func TestCommitUnverified(t *testing.T) {
	w := newCommitWorld(t)
	w.verified = func(n int) bool { return n >= 1 }
	got, err := w.commit(t, request(oidHead))
	if err != nil || got.SHA != strings.Repeat("b", 40) || !got.Verified || len(w.posted) != 2 {
		t.Errorf("retry: %+v, %v, %d commits", got, err, len(w.posted))
	}

	w2 := newCommitWorld(t)
	w2.verified = func(int) bool { return false }
	got, err = w2.commit(t, request(oidHead))
	wantClass(t, "unsigned", err, platform.ClassUnsupported, platform.ErrUnsigned)
	if ruleOfErr(err) != "cannot-sign" || got.Verified || got.SHA == "" || len(w2.posted) != 2 {
		t.Errorf("unsigned: %+v, rule %q, %d commits", got, ruleOfErr(err), len(w2.posted))
	}
	if u := w2.branchUpdates(); len(u) != 0 {
		t.Errorf("the branch moved to an unsigned commit: %v", u)
	}
}

// TestCommitStale: a refused update of a branch that moved is
// ClassConflict; one refused by a rule with the branch where expected
// keeps its class.
func TestCommitStale(t *testing.T) {
	w := newCommitWorld(t)
	w.head = oidOther
	w.refuse = func(refs []map[string]any) map[string]any {
		return gqlErr("UNPROCESSABLE", "Reference update failed: expected "+oidHead, "updateRefs")
	}
	_, err := w.commit(t, request(oidHead))
	wantClass(t, "stale lease", err, platform.ClassConflict, nil)

	w2 := newCommitWorld(t)
	w2.head = oidHead
	w2.refuse = func(refs []map[string]any) map[string]any {
		return gqlErr("UNPROCESSABLE", "Repository rule violations found\n\nGH013: Commits must have verified signatures.", "updateRefs")
	}
	_, err = w2.commit(t, request(oidHead))
	wantClass(t, "rule", err, platform.ClassPolicy, nil)
	if ruleOfErr(err) != "GH013" {
		t.Errorf("rule %q", ruleOfErr(err))
	}
	// A new branch that exists meanwhile.
	w3 := newCommitWorld(t)
	w3.head = oidOther
	w3.refuse = func([]map[string]any) map[string]any {
		return gqlErr("UNPROCESSABLE", "Reference already exists", "updateRefs")
	}
	_, err = w3.commit(t, request(""))
	wantClass(t, "created meanwhile", err, platform.ClassConflict, nil)
}

// TestCommitStageSplit: where updateRefs refuses the stage ref with the
// branch, the branch moves alone and the stage ref is deleted after it;
// later commits of the run go that way at once.
func TestCommitStageSplit(t *testing.T) {
	w := newCommitWorld(t)
	w.head = oidHead
	w.refuse = func(refs []map[string]any) map[string]any {
		for _, r := range refs {
			if r["name"] == stageRef {
				return gqlErr("UNPROCESSABLE", "refs/touchmark/0123456789abcdef/stage is not a valid ref to update", "updateRefs")
			}
		}
		return nil
	}
	w.f.json(http.MethodDelete, "/repos/acme/api/git/refs/touchmark/0123456789abcdef/stage", http.StatusNoContent, nil)
	got, err := w.commit(t, request(oidHead))
	if err != nil || !got.Verified {
		t.Fatalf("Commit = %+v, %v", got, err)
	}
	if n := len(w.f.requests(http.MethodDelete, "/repos/acme/api/git/refs/touchmark/0123456789abcdef/stage")); n != 1 {
		t.Errorf("stage deleted through REST %d times", n)
	}
	// The combined call (refused), then the branch alone; the stage ref goes
	// through REST once the split is known.
	if len(w.updates) != 2 {
		t.Errorf("updates %v", w.updates)
	}
	w.mu.Lock()
	w.updates = nil
	w.mu.Unlock()
	if _, err := w.commit(t, request(oidHead)); err != nil {
		t.Fatal(err)
	}
	if len(w.updates) != 1 || len(w.updates[0]) != 1 {
		t.Errorf("the second commit tried the combined call again: %v", w.updates)
	}
}

// TestCommitInvalid: requests the driver refuses before any write.
func TestCommitInvalid(t *testing.T) {
	w := newCommitWorld(t)
	for name, mod := range map[string]func(*platform.CommitRequest){
		"ref as branch":   func(r *platform.CommitRequest) { r.Branch = "refs/heads/x" },
		"empty branch":    func(r *platform.CommitRequest) { r.Branch = "" },
		"short tree":      func(r *platform.CommitRequest) { r.Tree = "abc" },
		"bad expect":      func(r *platform.CommitRequest) { r.Expect = "HEAD" },
		"stage elsewhere": func(r *platform.CommitRequest) { r.Stage = "refs/heads/main" },
		"stage dots":      func(r *platform.CommitRequest) { r.Stage = "refs/touchmark/../heads/main" },
		"no message":      func(r *platform.CommitRequest) { r.Message = " " },
	} {
		req := request(oidHead)
		mod(&req)
		_, err := w.commit(t, req)
		wantClass(t, name, err, platform.ClassInvalid, nil)
	}
	_, err := w.tw.(platform.Committer).Commit(t.Context(), platform.Repo{Host: "github.com", ID: "102", Path: "acme/other"}, request(oidHead))
	wantClass(t, "another repository", err, platform.ClassInvalid, nil)
	if len(w.posted) != 0 || len(w.updates) != 0 {
		t.Errorf("wrote: %d commits, %d updates", len(w.posted), len(w.updates))
	}
	if err := w.tw.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = w.commit(t, request(oidHead))
	wantClass(t, "after Close", err, platform.ClassAuth, nil)
}

// TestCommitWrongParent: a commit GitHub made on other parents than asked
// never moves the branch (ClassUnknown, integrity), and the stage ref is
// dropped.
func TestCommitWrongParent(t *testing.T) {
	w := newCommitWorld(t)
	req := request(oidHead)
	req.Parent = oidOther // the world answers every commit with parent oidParent
	_, err := w.commit(t, req)
	wantClass(t, "wrong parent", err, platform.ClassUnknown, nil)
	if err == nil || !strings.Contains(err.Error(), "other parents") {
		t.Errorf("error %v", err)
	}
	if u := w.branchUpdates(); len(u) != 0 {
		t.Errorf("the branch moved: %v", u)
	}
	if len(w.updates) != 1 || w.updates[0][0]["name"] != stageRef {
		t.Errorf("the stage ref was not dropped: %v", w.updates)
	}
}

// TestCommitTransientUpdate: an updateRefs whose outcome is unknown (a
// 502) is transient, and the driver never sends a second update after it
// (the core reads the branch back before it tries again); the stage ref is
// not dropped through REST either.
func TestCommitTransientUpdate(t *testing.T) {
	w := newCommitWorld(t)
	w.f.graphql("updateRefs(input: $input)", func(rw http.ResponseWriter, req gqlCall) {
		w.mu.Lock()
		w.updates = append(w.updates, nil)
		w.mu.Unlock()
		writeJSON(rw, http.StatusBadGateway, ghError("Server Error"))
	})
	_, err := w.commit(t, request(oidHead))
	wantClass(t, "502", err, platform.ClassTransient, nil)
	if len(w.updates) != 1 {
		t.Errorf("%d updateRefs after an unknown outcome, want 1", len(w.updates))
	}
	if n := len(w.f.requests(http.MethodDelete, "")); n != 0 {
		t.Errorf("%d deletions", n)
	}
}

// TestCommitDropStageREST: when updateRefs refuses to delete the stage ref
// on its own (after a tree mismatch), the driver deletes it through REST.
func TestCommitDropStageREST(t *testing.T) {
	w := newCommitWorld(t)
	w.tree = oidOther
	w.refuse = func([]map[string]any) map[string]any {
		return gqlErr("UNPROCESSABLE", "refs/touchmark/0123456789abcdef/stage is not a valid ref to update", "updateRefs")
	}
	w.f.json(http.MethodDelete, "/repos/acme/api/git/refs/touchmark/0123456789abcdef/stage", http.StatusNoContent, nil)
	_, err := w.commit(t, request(oidHead))
	wantClass(t, "tree mismatch", err, platform.ClassUnknown, nil)
	if n := len(w.f.requests(http.MethodDelete, "/repos/acme/api/git/refs/touchmark/0123456789abcdef/stage")); n != 1 {
		t.Errorf("stage deleted through REST %d times", n)
	}
}

// TestCommitFailedPost: a commit that could not be made leaves the stage
// ref for the core's retry and moves nothing.
func TestCommitFailedPost(t *testing.T) {
	w := newCommitWorld(t)
	w.f.json(http.MethodPost, "/repos/acme/api/git/commits", http.StatusBadGateway, ghError("Server Error"))
	_, err := w.commit(t, request(oidHead))
	wantClass(t, "5xx", err, platform.ClassTransient, nil)
	if len(w.updates) != 0 {
		t.Errorf("updates %v", w.updates)
	}
	w.f.json(http.MethodPost, "/repos/acme/api/git/commits", http.StatusUnprocessableEntity, ghError("Tree SHA does not exist"))
	_, err = w.commit(t, request(oidHead))
	wantClass(t, "422", err, platform.ClassInvalid, nil)
}
