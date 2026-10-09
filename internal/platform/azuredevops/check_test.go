package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

func TestCheck(t *testing.T) {
	s := newAPIServer(t)
	targetWorld(t, s, map[int]bool{permContribute: true, permCreateBranch: true})
	w := newTestWriter(t, s, testToken(t))
	ids, err := w.Check(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("identity checks %+v", ids)
	}
	for _, f := range ids {
		if f.Status != platform.FindingUnknown || f.Repo != "" {
			t.Errorf("identity check %+v", f)
		}
	}
	repo := testRepo()
	repo.Host = w.c.host
	fs, err := w.Check(context.Background(), []platform.Repo{repo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Check != "access" || fs[0].Status != platform.FindingFail || fs[1].Check != "rules" || fs[1].Status != platform.FindingUnknown {
		t.Errorf("repository checks %+v", fs)
	}

	// All allowed: ok; the permissions API closed to the token: unknown.
	targetWorld(t, s, allPerms)
	if fs, err := w.Check(context.Background(), []platform.Repo{repo}, nil); err != nil || fs[0].Status != platform.FindingOK {
		t.Errorf("all allowed: %+v, %v", fs, err)
	}
	for _, bits := range []int{permContribute, permCreateBranch, permPullRequestContib} {
		s.json(apisPath("permissions", gitNamespace, fmt.Sprint(bits)), http.StatusForbidden, errorBody("X", "no"))
	}
	if fs, err := w.Check(context.Background(), []platform.Repo{repo}, nil); err != nil || fs[0].Status != platform.FindingUnknown {
		t.Errorf("no permissions API: %+v, %v", fs, err)
	}
	// A repository the writer does not see fails.
	s.json(repoPath(), http.StatusNotFound, errorBody(keyRepoNotFound, "TF401019"))
	if fs, err := w.Check(context.Background(), []platform.Repo{repo}, nil); err != nil || len(fs) != 1 || fs[0].Status != platform.FindingFail {
		t.Errorf("an unseen repository: %+v, %v", fs, err)
	}
}
