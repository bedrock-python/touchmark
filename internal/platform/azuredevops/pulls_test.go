package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// testLine is a marker line, as the core stores it.
const testLine = "<!-- touchmark:v1 hub=acme-eng fp=b7461799636b11ec stream=sync key=sha256:6b1f data=H4sI -->"

// syncBranch is touchmark's branch of the tests.
const syncBranch = "touchmark/acme-eng"

const headSHA = "9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f7081920"

// prWorld declares the pull requests of the sync branch: #7 active by the
// bot (a long description, cut in the listing), #5 abandoned by a person,
// #4 completed, #3 active by someone else, #2 abandoned by someone else
// (left out: not by our authors), #6 active from a fork; and their
// properties, single reads, refs.
func prWorld(t *testing.T, s *apiServer) {
	t.Helper()
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	user := identityRef(userID, "Dev", "aad.ZGV2")
	long := strings.Repeat("Long text. ", 50)
	p7 := prJSON(7, statusActive, syncBranch, "main", bot, long)
	p7["labels"] = []any{map[string]any{"id": "l1", "name": "engineering-assets", "active": true}}
	p7cut := prJSON(7, statusActive, syncBranch, "main", bot, long[:400])
	p5 := prJSON(5, statusAbandoned, syncBranch, "main", bot, "short")
	p5full := prJSON(5, statusAbandoned, syncBranch, "main", bot, "short")
	p5full["closedBy"] = user
	p4 := prJSON(4, statusCompleted, syncBranch, "release", bot, "merged")
	p3 := prJSON(3, statusActive, syncBranch, "main", user, "theirs")
	p2 := prJSON(2, statusAbandoned, syncBranch, "main", user, "theirs")
	p6 := prJSON(6, statusActive, syncBranch, "main", user, "from a fork")
	p6["forkSource"] = map[string]any{"name": "refs/heads/" + syncBranch, "repository": repoJSON(forkRepo, "Billing", "api-fork", "main")}
	s.handle(http.MethodGet, repoPath("pullrequests"), func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("searchCriteria.sourceRefName") != "refs/heads/"+syncBranch || q.Get("searchCriteria.status") != "all" {
			writeJSON(w, http.StatusOK, collection())
			return
		}
		writeJSON(w, http.StatusOK, collection(p2, p3, p4, p5, p6, p7cut))
	})
	s.json(repoPath("pullrequests", "7"), http.StatusOK, p7)
	s.json(repoPath("pullrequests", "5"), http.StatusOK, p5full)
	for _, n := range []int{2, 3, 4, 5, 6, 7} {
		line := ""
		if n == 7 || n == 5 || n == 4 {
			line = testLine
		}
		s.json(repoPath("pullRequests", fmt.Sprint(n), "properties"), http.StatusOK, propsJSON(line))
	}
	s.handle(http.MethodGet, repoPath("refs"), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("filter") {
		case "heads/" + syncBranch:
			writeJSON(w, http.StatusOK, collection(
				map[string]any{"name": "refs/heads/" + syncBranch + "-old", "objectId": "1111111111111111111111111111111111111111"},
				map[string]any{"name": "refs/heads/" + syncBranch, "objectId": headSHA}))
		default:
			writeJSON(w, http.StatusOK, collection())
		}
	})
}

func TestPRs(t *testing.T) {
	s := newAPIServer(t)
	prWorld(t, s)
	r := newTestReader(t, s, testToken(t))
	prs, err := r.PRs(context.Background(), testRepo(), []string{syncBranch, ""}, []platform.Account{{ID: botID}})
	if err != nil {
		t.Fatal(err)
	}
	var nums []int64
	for _, pr := range prs {
		nums = append(nums, pr.Number)
	}
	if fmt.Sprint(nums) != "[7 6 5 4 3]" {
		t.Fatalf("PRs = %v, want [7 6 5 4 3]", nums)
	}
	p7, p6, p5, p4, p3 := prs[0], prs[1], prs[2], prs[3], prs[4]
	if p7.State != platform.Open || p7.Head != syncBranch || p7.Base != "main" || !p7.BaseExists || p7.HeadSHA != headSHA ||
		p7.Author.ID != botID || p7.RepoID != repoID || p7.HeadRepoID != repoID ||
		p7.URL != "https://dev.azure.com/acme/Billing/_git/api/pullrequest/7" || len(p7.Labels) != 1 {
		t.Errorf("#7 = %+v", p7)
	}
	// The description read alone, then the stored marker.
	if !strings.HasPrefix(p7.Body, strings.Repeat("Long text. ", 49)) || !strings.HasSuffix(p7.Body, "\n\n"+testLine) {
		t.Errorf("#7 body %q", p7.Body)
	}
	if m, status := marker.Find(p7.Body, nil); status != marker.Foreign || m.Key != "" {
		t.Errorf("the marker of #7 is not in its body: %v", status)
	}
	if p6.HeadRepoID != forkRepo || p6.HeadSHA != "" {
		t.Errorf("#6 from a fork: %+v", p6)
	}
	if p5.State != platform.Closed || p5.ClosedBy == nil || p5.ClosedBy.ID != userID || p5.ClosedAt.IsZero() || p5.Body != "short\n\n"+testLine {
		t.Errorf("#5 = %+v", p5)
	}
	if p4.State != platform.Merged || p4.BaseExists || p4.ClosedBy != nil {
		t.Errorf("#4 = %+v", p4)
	}
	if p3.Author.ID != userID || p3.Body != "theirs" {
		t.Errorf("#3 = %+v", p3)
	}
	// One listing, #7 and #5 read alone, five property reads.
	if n := len(s.requests(http.MethodGet, repoPath("pullrequests"))); n != 1 {
		t.Errorf("%d listings", n)
	}
	if len(s.requests(http.MethodGet, repoPath("pullrequests", "7"))) != 1 || len(s.requests(http.MethodGet, repoPath("pullrequests", "5"))) != 1 ||
		len(s.requests(http.MethodGet, repoPath("pullrequests", "3"))) != 0 {
		t.Error("single reads: want #7 (a cut description) and #5 (abandoned) only")
	}
	if none, err := r.PRs(context.Background(), testRepo(), nil, nil); err != nil || len(none) != 0 {
		t.Errorf("no heads: %v, %v", none, err)
	}
}

// TestPRsMarkerProperty: only a marker line counts as the stored marker;
// a marker pasted into the description never does.
func TestPRsMarkerProperty(t *testing.T) {
	s := newAPIServer(t)
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	s.json(repoPath("pullrequests"), http.StatusOK, collection(
		prJSON(9, statusActive, syncBranch, "main", bot, "text\n\n"+testLine),
		prJSON(8, statusActive, syncBranch, "main", bot, "text")))
	s.json(repoPath("pullRequests", "9", "properties"), http.StatusOK, propsJSON(""))
	s.json(repoPath("pullRequests", "8", "properties"), http.StatusOK, propsJSON("not a marker"))
	s.json(repoPath("refs"), http.StatusOK, collection())
	prs, err := newTestReader(t, s, testToken(t)).PRs(context.Background(), testRepo(), []string{syncBranch}, []platform.Account{{ID: botID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range prs {
		if pr.Body != "text" {
			t.Errorf("#%d body %q, want the description alone", pr.Number, pr.Body)
		}
	}
}

func TestOpenPRsBy(t *testing.T) {
	s := newAPIServer(t)
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	other := repoJSON(otherRepo, "Web", "site", "main")
	other["project"].(map[string]any)["id"] = "aaaaaaaa-0000-0000-0000-000000000002"
	s.json(apisPath("git", "repositories"), http.StatusOK, collection(repoJSON(repoID, "Billing", "api", "main"), other))
	s.handle(http.MethodGet, "/"+org+"/"+projectID+"/_apis/git/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("searchCriteria.creatorId") != botID || q.Get("searchCriteria.status") != "active" {
			t.Errorf("sweep query %v", q)
		}
		writeJSON(w, http.StatusOK, collection(
			prJSON(7, statusActive, syncBranch, "main", bot, "ours"),
			prJSON(8, statusActive, "feature", "main", bot, "another branch")))
	})
	s.json("/"+org+"/aaaaaaaa-0000-0000-0000-000000000002/_apis/git/pullrequests", http.StatusForbidden, errorBody("X", "TF401019"))
	s.json(repoPath("pullRequests", "7", "properties"), http.StatusOK, propsJSON(testLine))
	swept, err := newTestReader(t, s, testToken(t)).OpenPRsBy(context.Background(), []platform.Account{{ID: botID}}, []string{syncBranch})
	if err != nil {
		t.Fatal(err)
	}
	if swept.Complete || len(swept.PRs) != 1 || swept.PRs[0].PR.Number != 7 || swept.PRs[0].Repo.Path != "Billing/api" ||
		!strings.HasSuffix(swept.PRs[0].PR.Body, testLine) || !swept.PRs[0].PR.BaseExists {
		t.Errorf("OpenPRsBy = %+v", swept)
	}
	_, err = newTestReader(t, s, "").OpenPRsBy(context.Background(), []platform.Account{{ID: botID}}, []string{syncBranch})
	wantClass(t, "anonymous sweep", err, platform.ClassAuth)
}
