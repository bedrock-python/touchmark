package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// noSleep makes storeNewMarker's pauses instant for the test and records
// them.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = old })
	return &waits
}

// failProperties makes the first n PATCHes of #11's properties fail with
// status, header h and an error body; the others go to next.
func failProperties(s *apiServer, n, status int, h http.Header, next http.HandlerFunc) {
	calls := 0
	s.handle(http.MethodPatch, repoPath("pullRequests", "11", "properties"), func(w http.ResponseWriter, r *http.Request) {
		calls++
		if n < 0 || calls <= n {
			for k, v := range h {
				w.Header()[k] = v
			}
			writeJSON(w, status, errorBody("VssServiceException", "TF000000: failed"))
			return
		}
		next(w, r)
	})
}

// abandonRoute answers the PATCH of #11, recording its bodies; fail makes
// it answer 500.
func abandonRoute(t *testing.T, s *apiServer, pr *map[string]any, fail bool) *[]updatePR {
	t.Helper()
	var got []updatePR
	s.handle(http.MethodPatch, repoPath("pullrequests", "11"), func(w http.ResponseWriter, r *http.Request) {
		var in updatePR
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		got = append(got, in)
		if fail {
			writeJSON(w, http.StatusInternalServerError, errorBody("VssServiceException", "down"))
			return
		}
		if in.Status != nil {
			(*pr)["status"] = *in.Status
			(*pr)["closedDate"] = "2026-03-01T00:00:00Z"
		}
		writeJSON(w, http.StatusOK, *pr)
	})
	return &got
}

// propertyHandler is the properties PATCH of createWorld, taken back from
// the routes before a test replaces it.
func propertyHandler(s *apiServer) http.HandlerFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes[http.MethodPatch+" "+repoPath("pullRequests", "11", "properties")]
}

// TestCreatePRMarkerRetried: a property write that fails for now is tried
// again, after the driver's pauses; the pull request stays active.
func TestCreatePRMarkerRetried(t *testing.T) {
	waits := noSleep(t)
	s := newAPIServer(t)
	_, stored := createWorld(t, s)
	failProperties(s, 2, http.StatusServiceUnavailable, nil, propertyHandler(s))
	tw := newTarget(t, s)
	body := "text\n\n" + testLine
	pr, err := tw.CreatePR(context.Background(), platform.NewPR{Head: syncBranch, Base: "main", Title: "t", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.requests(http.MethodPatch, repoPath("pullRequests", "11", "properties"))); n != 3 {
		t.Errorf("%d property writes, want 3", n)
	}
	if len(*stored) != 1 || pr.Body != body || pr.State != platform.Open {
		t.Errorf("stored %+v, CreatePR = %+v", *stored, pr)
	}
	if fmt.Sprint(*waits) != "[1s 2s]" {
		t.Errorf("waits %v", *waits)
	}
	if n := len(s.requests(http.MethodPatch, repoPath("pullrequests", "11"))); n != 0 {
		t.Errorf("the pull request was patched %d times", n)
	}
}

// TestCreatePRMarkerAbandons: a property that cannot be stored gets the
// new pull request abandoned, and the call fails with the property's class;
// a rate limit asking for a long wait, or a refusal, is not tried again;
// a failed abandon says that the pull request stays active.
func TestCreatePRMarkerAbandons(t *testing.T) {
	body := "text\n\n" + testLine
	for _, tc := range []struct {
		name     string
		status   int
		h        http.Header
		attempts int
		class    platform.Class
		fail     bool
	}{
		{"transient", http.StatusServiceUnavailable, nil, 4, platform.ClassTransient, false},
		{"long rate limit", http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, 1, platform.ClassRateLimited, false},
		{"short rate limit", http.StatusTooManyRequests, http.Header{"Retry-After": {"5"}}, 4, platform.ClassRateLimited, false},
		{"refused", http.StatusForbidden, nil, 1, platform.ClassPermission, false},
		{"abandon fails", http.StatusServiceUnavailable, nil, 4, platform.ClassTransient, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := noSleep(t)
			s := newAPIServer(t)
			pr, _ := createWorld(t, s)
			failProperties(s, -1, tc.status, tc.h, nil)
			patches := abandonRoute(t, s, pr, tc.fail)
			tw := newTarget(t, s)
			_, err := tw.CreatePR(context.Background(), platform.NewPR{Head: syncBranch, Base: "main", Title: "t", Body: body})
			wantClass(t, "CreatePR", err, tc.class)
			if n := len(s.requests(http.MethodPatch, repoPath("pullRequests", "11", "properties"))); n != tc.attempts {
				t.Errorf("%d property writes, want %d", n, tc.attempts)
			}
			if len(*patches) != 1 || (*patches)[0].Status == nil || *(*patches)[0].Status != statusAbandoned ||
				(*patches)[0].Description != nil || (*patches)[0].Title != nil {
				t.Errorf("PATCHes %+v, want one abandon", *patches)
			}
			if tc.name == "short rate limit" && fmt.Sprint(*waits) != "[5s 5s 5s]" {
				t.Errorf("waits %v", *waits)
			}
			switch {
			case tc.fail && !strings.Contains(err.Error(), "stays active without its marker"):
				t.Errorf("error %v", err)
			case !tc.fail && !strings.Contains(err.Error(), "#11 was abandoned"):
				t.Errorf("error %v", err)
			}
			// No label is added to an abandoned pull request.
			if n := len(s.requests(http.MethodPost, repoPath("pullRequests", "11", "labels"))); n != 0 {
				t.Errorf("%d labels added", n)
			}
		})
	}
}

// TestEditPROrder: an edit of an active pull request that is not a close
// sends its PATCH first, then the labels, then the property. A property
// that fails then leaves the marker of the previous body, so that the next
// run sees the edit still to make.
func TestEditPROrder(t *testing.T) {
	s := newAPIServer(t)
	patches := editWorld(t, s, statusActive)
	tw := newTarget(t, s)
	newLine := strings.Replace(testLine, "key=sha256:6b1f", "key=sha256:0000", 1)
	body := "new text\n\n" + newLine
	if _, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &body, AddLabels: []string{"engineering-assets"}}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range s.writes() {
		order = append(order, c.Method+" "+c.Path[strings.LastIndex(c.Path, "/")+1:])
	}
	if strings.Join(order, ", ") != "PATCH 11, POST labels, PATCH properties" {
		t.Errorf("writes %v", order)
	}
	if len(*patches) != 1 || (*patches)[0].Status != nil || *(*patches)[0].Description != "new text" {
		t.Errorf("PATCH %+v", *patches)
	}

	// The property fails after the description went through.
	failProperties(s, -1, http.StatusInternalServerError, nil, nil)
	next := "newer text\n\n" + testLine
	_, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &next})
	wantClass(t, "a failed property", err, platform.ClassTransient)
	r := newTestReader(t, s, testToken(t))
	prs, err := r.PRs(context.Background(), testRepo(), []string{syncBranch}, []platform.Account{{ID: botID}})
	if err != nil || len(prs) != 1 {
		t.Fatalf("PRs = %v, %v", prs, err)
	}
	if desc, line := marker.Detach(prs[0].Body); desc != "newer text" || line != newLine {
		t.Errorf("after the failure: description %q, marker %q: the marker must stay the previous one", desc, line)
	}
}

// TestEditPRKeepsDescription: a body whose description is the current one,
// but for line endings, trailing blanks and a marker line a person pasted,
// writes the property alone.
func TestEditPRKeepsDescription(t *testing.T) {
	s := newAPIServer(t)
	patches, pr := editWorldPR(t, s, statusActive)
	(*pr)["description"] = "old text\r\nsecond line  \r\n" + testLine + "\r\n\r\n"
	tw := newTarget(t, s)
	newLine := strings.Replace(testLine, "key=sha256:6b1f", "key=sha256:0000", 1)
	body := "old text\nsecond line\n\n" + newLine
	got, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &body})
	if err != nil {
		t.Fatal(err)
	}
	if len(*patches) != 0 {
		t.Errorf("PATCH %+v for an unchanged description", *patches)
	}
	if _, line := marker.Detach(got.Body); line != newLine {
		t.Errorf("marker %q", line)
	}
}

// TestEditPRCloseLongDescription: a close whose description would not fit
// stores its marker and abandons the pull request, the description as it
// was; closing it again is nothing.
func TestEditPRCloseLongDescription(t *testing.T) {
	s := newAPIServer(t)
	patches := editWorld(t, s, statusActive)
	tw := newTarget(t, s)
	closedLine := strings.Replace(testLine, "key=sha256:6b1f", "key=sha256:c105", 1)
	body := strings.Repeat("x", 3990) + " cc @\u2060bob, @\u2060eve\n\n" + closedLine
	closed := platform.Closed
	pr, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &body, State: &closed})
	if err != nil {
		t.Fatal(err)
	}
	if len(*patches) != 1 || (*patches)[0].Description != nil || (*patches)[0].Status == nil {
		t.Errorf("PATCH %+v, want an abandon without the description", *patches)
	}
	if desc, line := marker.Detach(pr.Body); pr.State != platform.Closed || desc != "old text" || line != closedLine {
		t.Errorf("EditPR = %s, %q, %q", pr.State, desc, line)
	}
	s.reset()
	if _, err := tw.EditPR(context.Background(), 11, platform.PREdit{Body: &body, State: &closed}); err != nil {
		t.Errorf("closing again: %v", err)
	}
	if w := s.writes(); len(w) != 0 {
		t.Errorf("writes when closing again: %v", w)
	}
	// An active pull request's edit that does not fit still fails.
	s2 := newAPIServer(t)
	editWorld(t, s2, statusActive)
	_, err = newTarget(t, s2).EditPR(context.Background(), 11, platform.PREdit{Body: &body})
	wantClass(t, "a long description", err, platform.ClassInvalid)
	if w := s2.writes(); len(w) != 0 {
		t.Errorf("writes for a refused edit: %v", w)
	}
}

// TestReadFileDisabled: a disabled repository shows no default branch,
// as an empty one does, but its files are unknown, never missing.
func TestReadFileDisabled(t *testing.T) {
	s := newAPIServer(t)
	fileWorld(t, s)
	disabled := repoJSON(repoID, "Billing", "api", "")
	disabled["isDisabled"] = true
	s.json(repoPath(), http.StatusOK, disabled)
	r := newTestReader(t, s, testToken(t))
	repo := testRepo()
	repo.DefaultBranch = ""
	_, err := r.ReadFile(context.Background(), repo, "", ".engineering-assets.yml", 1<<20)
	if errors.Is(err, platform.ErrNotFound) {
		t.Errorf("a disabled repository's file is missing: %v", err)
	}
	wantClass(t, "a disabled repository", err, platform.ClassUnknown)
	repo = testRepo()
	repo.Disabled = true
	s.reset()
	_, err = r.ReadFile(context.Background(), repo, "", ".engineering-assets.yml", 1<<20)
	wantClass(t, "a repository known to be disabled", err, platform.ClassUnknown)
	if n := len(s.requests("", "")); n != 0 {
		t.Errorf("%d requests for a repository known to be disabled", n)
	}
}

// TestAnonymousSignIn: an anonymous read sent to the sign-in page says
// that Azure DevOps wants a token for it.
func TestAnonymousSignIn(t *testing.T) {
	s := newAPIServer(t)
	s.handle(http.MethodGet, repoPath("pullRequests", "7", "properties"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://spsprodweu5.vssps.visualstudio.com/_signin?realm=dev.azure.com")
		w.WriteHeader(http.StatusFound)
	})
	_, err := newTestReader(t, s, "").c.markerOf(context.Background(), "read", repoID, 7)
	wantClass(t, "anonymous properties", err, platform.ClassAuth)
	if err == nil || !strings.Contains(err.Error(), "only with a token") || strings.Contains(err.Error(), "not public") {
		t.Errorf("error %v", err)
	}
}

// TestRetryAfterOnSuccess: an answer that went through with Retry-After
// pauses the provider's throttle, without a strike; one without it does
// not.
func TestRetryAfterOnSuccess(t *testing.T) {
	s := newAPIServer(t)
	calls := 0
	s.handle(http.MethodGet, apisPath("connectionData"), func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
		}
		writeJSON(w, http.StatusOK, connJSON(botID, "Microsoft.IdentityModel.Claims.ClaimsIdentity;x\\bot@acme.example", "bot@acme.example"))
	})
	at := time.Unix(1_800_000_000, 0)
	g := throttle.New(throttle.Options{Name: "ado", Clock: throttle.Clock{
		Now:   func() time.Time { return at },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}})
	ctx := throttle.With(context.Background(), g)
	r := newTestReader(t, s, testToken(t))
	if _, err := r.c.selfAccount(ctx); err != nil {
		t.Fatal(err)
	}
	st := g.Stats()
	if st.Pauses != 1 || st.Paused != 7*time.Second || st.Limits != 0 {
		t.Errorf("stats %+v, want one pause of 7s and no rate limit", st)
	}
	r2 := newTestReader(t, s, testToken(t))
	if _, err := r2.c.selfAccount(ctx); err != nil {
		t.Fatal(err)
	}
	if st := g.Stats(); st.Pauses != 1 {
		t.Errorf("stats %+v after an answer without Retry-After", st)
	}
}

// TestPRsClosedMarkerBound: the markers of pull requests that are not
// active are read for the newest maxClosedReads of each status only.
func TestPRsClosedMarkerBound(t *testing.T) {
	s := newAPIServer(t)
	bot := identityRef(botID, "touchmark bot", "aad.Ym90")
	var all []any
	const n = maxClosedReads + 5
	for i := 1; i <= n; i++ {
		p := prJSON(int64(i), statusAbandoned, syncBranch, "main", bot, "short")
		all = append(all, p)
		full := prJSON(int64(i), statusAbandoned, syncBranch, "main", bot, "short")
		full["closedBy"] = bot
		s.json(repoPath("pullrequests", fmt.Sprint(i)), http.StatusOK, full)
		s.json(repoPath("pullRequests", fmt.Sprint(i), "properties"), http.StatusOK, propsJSON(testLine))
	}
	all = append(all, prJSON(n+1, statusActive, syncBranch, "main", bot, "open"))
	s.json(repoPath("pullRequests", fmt.Sprint(n+1), "properties"), http.StatusOK, propsJSON(testLine))
	s.handle(http.MethodGet, repoPath("pullrequests"), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, collection(all...))
	})
	s.json(repoPath("refs"), http.StatusOK, collection(map[string]any{"name": "refs/heads/" + syncBranch, "objectId": headSHA}))
	prs, err := newTestReader(t, s, testToken(t)).PRs(context.Background(), testRepo(), []string{syncBranch}, []platform.Account{{ID: botID}})
	if err != nil {
		t.Fatal(err)
	}
	reads, withMarker := 0, 0
	for _, c := range s.requests(http.MethodGet, "") {
		if strings.HasSuffix(c.Path, "/properties") {
			reads++
		}
	}
	for _, pr := range prs {
		if _, line := marker.Detach(pr.Body); line != "" {
			withMarker++
			if pr.State == platform.Closed && pr.Number <= n-maxClosedReads {
				t.Errorf("#%d, older than the bound, has its marker", pr.Number)
			}
		}
	}
	if reads != maxClosedReads+1 || withMarker != maxClosedReads+1 || len(prs) != n+1 {
		t.Errorf("%d property reads, %d markers of %d pull requests; want %d", reads, withMarker, len(prs), maxClosedReads+1)
	}
}
