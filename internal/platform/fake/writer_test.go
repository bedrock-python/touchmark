package fake_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
)

func TestTargetPermissions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	all := platform.Perms{Contents: true, PRs: true, Workflows: true}
	for _, tc := range []struct {
		name   string
		flavor fake.Flavor
		grant  platform.Perms
		need   platform.Perms
		rule   string // "" when allowed
	}{
		{"no grant", fake.GitHub, platform.Perms{}, platform.Perms{Contents: true, PRs: true}, "contents"},
		{"no pull requests", fake.GitHub, platform.Perms{Contents: true}, platform.Perms{Contents: true, PRs: true}, "pull-requests"},
		{"no workflows", fake.GitHub, platform.Perms{Contents: true, PRs: true}, all, "workflows"},
		{"workflows granted", fake.GitHub, all, all, ""},
		{"workflows mean nothing on GitLab", fake.GitLab, platform.Perms{Contents: true, PRs: true}, all, ""},
		{"nothing needed", fake.GitHub, platform.Perms{}, platform.Perms{}, ""},
	} {
		e := newEnv(t, fake.WithFlavor(tc.flavor))
		r := e.p.AddRepo(platform.Repo{Path: "acme/api"})
		e.p.Grant(r.ID, e.writer, tc.grant)
		e.ok()
		tw, err := e.p.Writer(e.writer).Target(ctx, r, tc.need)
		if tc.rule == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if tw != nil {
			t.Errorf("%s: a writer with the error", tc.name)
		}
		wantRule(t, tc.name, err, platform.ClassPermission, http.StatusForbidden, tc.rule)
	}

	// The token can do only what it was minted for, and only while the grant
	// lasts.
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")
	contentsOnly, err := e.p.Writer(e.writer).Target(ctx, r, platform.Perms{Contents: true})
	must(t, err)
	_, err = contentsOnly.CreatePR(ctx, platform.NewPR{Head: "h", Base: "main", Title: "t"})
	wantRule(t, "PR with a contents token", err, platform.ClassPermission, http.StatusForbidden, "pull-requests")
	tw := e.target(r)
	e.p.Grant(r.ID, e.writer, platform.Perms{})
	e.ok()
	_, err = tw.EnsureLabels(ctx, []string{"x"})
	wantRule(t, "write after the revocation", err, platform.ClassPermission, http.StatusForbidden, "pull-requests")
	e.p.Grant(r.ID, platform.Account{ID: "nobody"}, platform.Perms{PRs: true})
	wantSetupErr(t, e.p, "unknown account")
}

func TestCreatePRByFlavor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flavor     fake.Flavor
		draftTitle string
		dupStatus  int
		url        string
	}{
		{fake.GitHub, "sync", http.StatusUnprocessableEntity, "https://github.com/acme/api/pull/1"},
		{fake.GitLab, "Draft: sync", http.StatusConflict, "https://github.com/acme/api/-/merge_requests/1"},
		{fake.Gitea, "WIP: sync", http.StatusUnprocessableEntity, "https://github.com/acme/api/pulls/1"},
		{fake.Forgejo, "WIP: sync", http.StatusUnprocessableEntity, "https://github.com/acme/api/pulls/1"},
	} {
		e := newEnv(t, fake.WithFlavor(tc.flavor))
		r := e.repo("acme/api", "README.md", "x")
		tw := e.target(r)
		ctx := t.Context()
		pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Draft: true})
		if err != nil || !pr.Draft || pr.Title != tc.draftTitle || pr.URL != tc.url {
			t.Errorf("%s: draft = %+v, %v", tc.flavor, pr, err)
		}
		// A title keeps the prefix of a draft and never doubles it.
		edited, err := tw.EditPR(ctx, pr.Number, platform.PREdit{Title: ptr("renamed")})
		if err != nil || !edited.Draft || edited.Title != strings.Replace(tc.draftTitle, "sync", "renamed", 1) {
			t.Errorf("%s: retitled draft = %q (draft %v), %v", tc.flavor, edited.Title, edited.Draft, err)
		}
		edited, _ = tw.EditPR(ctx, pr.Number, platform.PREdit{Title: ptr(tc.draftTitle)})
		if edited.Title != tc.draftTitle {
			t.Errorf("%s: a prefixed title became %q", tc.flavor, edited.Title)
		}
		dup, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "again"})
		wantRule(t, string(tc.flavor)+" duplicate", err, platform.ClassConflict, tc.dupStatus, "")
		wantIs(t, string(tc.flavor)+" duplicate", err, platform.ErrExists)
		if dup.Number != pr.Number || dup.Title != tc.draftTitle {
			t.Errorf("%s: duplicate returned %+v", tc.flavor, dup)
		}
	}
}

func TestCreatePRRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t, fake.WithFlavor(fake.GitLab))
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	ok := platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Body: "body\n  not a /quick action"}
	for name, tc := range map[string]struct {
		mod   func(*platform.NewPR)
		class platform.Class
	}{
		"no head":        {func(n *platform.NewPR) { n.Head = "" }, platform.ClassInvalid},
		"no base":        {func(n *platform.NewPR) { n.Base = "" }, platform.ClassInvalid},
		"head is base":   {func(n *platform.NewPR) { n.Head = "main" }, platform.ClassInvalid},
		"blank title":    {func(n *platform.NewPR) { n.Title = "  " }, platform.ClassInvalid},
		"quick action":   {func(n *platform.NewPR) { n.Body = "text\n  /close\nmore" }, platform.ClassInvalid},
		"quick action 1": {func(n *platform.NewPR) { n.Body = "/merge" }, platform.ClassInvalid},
		"body too long":  {func(n *platform.NewPR) { n.Body = strings.Repeat("x", 200001) }, platform.ClassInvalid},
		"blank label":    {func(n *platform.NewPR) { n.Labels = []string{""} }, platform.ClassInvalid},
	} {
		np := ok
		tc.mod(&np)
		_, err := tw.CreatePR(ctx, np)
		wantClass(t, name, err, tc.class)
	}
	if len(e.p.PRList(r.ID)) != 0 {
		t.Fatalf("refused PRs were created: %v", numbers(e.p.PRList(r.ID)))
	}
	if _, err := tw.CreatePR(ctx, ok); err != nil {
		t.Errorf("a valid PR: %v", err)
	}
	// A body of exactly MaxBody is fine.
	gl := fake.CapsFor(fake.GitLab).MaxBody
	np := ok
	np.Head, np.Body = "other", strings.Repeat("x", gl)
	if _, err := tw.CreatePR(ctx, np); err != nil {
		t.Errorf("a body of MaxBody bytes: %v", err)
	}

	// GitHub has no quick actions.
	gh := newEnv(t)
	ghr := gh.repo("acme/api", "README.md", "x")
	if _, err := gh.target(ghr).CreatePR(ctx, platform.NewPR{Head: "h", Base: "main", Title: "t", Body: "/close"}); err != nil {
		t.Errorf("a slash line on GitHub: %v", err)
	}

	// Archived repositories and disabled pull requests.
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.Archived = true })
	e.ok()
	_, err := tw.CreatePR(ctx, platform.NewPR{Head: "x", Base: "main", Title: "t"})
	wantRule(t, "archived", err, platform.ClassPermission, http.StatusForbidden, "archived")
	err = tw.Comment(ctx, 1, "x")
	wantRule(t, "comment in an archived repository", err, platform.ClassPermission, http.StatusForbidden, "archived")
	e.p.UpdateRepo(r.ID, func(repo *platform.Repo) { repo.Archived, repo.PRsDisabled = false, true })
	_, err = tw.CreatePR(ctx, platform.NewPR{Head: "x", Base: "main", Title: "t"})
	wantRule(t, "PRs disabled", err, platform.ClassPolicy, http.StatusForbidden, "prs-disabled")
}

func TestEditPRRules(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t, fake.WithFlavor(fake.GitLab))
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Body: "b", Labels: []string{"a"}})
	must(t, err)
	e.p.UpdatePR(r.ID, pr.Number, func(p *platform.PR) { p.Labels = append(p.Labels, "human") })
	for name, edit := range map[string]platform.PREdit{
		"blank title":       {Title: ptr(""), Body: ptr("changed")},
		"quick action body": {Body: ptr("/close"), Title: ptr("changed")},
		"merged state":      {State: ptr(platform.Merged), Body: ptr("changed")},
		"unknown state":     {State: ptr(platform.PRState("draft"))},
		"empty base":        {Base: ptr(""), Body: ptr("changed")},
		"base is head":      {Base: ptr("touchmark/hub")},
		"blank label":       {AddLabels: []string{"ok", " "}, Body: ptr("changed")},
		"body too long":     {Body: ptr(strings.Repeat("x", 200001)), Title: ptr("changed")},
	} {
		_, err := tw.EditPR(ctx, pr.Number, edit)
		wantClass(t, name, err, platform.ClassInvalid)
	}
	// Nothing of a refused edit was applied.
	got := e.p.PR(r.ID, pr.Number)
	if got.Title != "sync" || got.Body != "b" || got.State != platform.Open || got.Base != "main" {
		t.Errorf("a refused edit changed the PR: %+v", got)
	}
	sameList(t, "labels", got.Labels, []string{"a", "human"})
	if l := e.p.Labels(r.ID); len(l) != 2 {
		t.Errorf("a refused edit created labels: %v", l)
	}

	// Labels only add; closing records the writer.
	got, err = tw.EditPR(ctx, pr.Number, platform.PREdit{AddLabels: []string{"b", "a", "b"}, State: ptr(platform.Closed)})
	must(t, err)
	sameList(t, "labels after adding", got.Labels, []string{"a", "human", "b"})
	if got.ClosedBy == nil || got.ClosedBy.ID != e.writer.ID || got.ClosedAt.IsZero() {
		t.Errorf("closed: %+v", got)
	}
	// Closing a closed PR changes nothing.
	again, err := tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Closed)})
	if err != nil || !again.ClosedAt.Equal(got.ClosedAt) {
		t.Errorf("closing twice: %v, %v → %v", err, got.ClosedAt, again.ClosedAt)
	}

	e.p.SetPRState(r.ID, pr.Number, platform.Merged, &e.other, got.ClosedAt)
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{State: ptr(platform.Open)})
	wantClass(t, "reopen a merged PR", err, platform.ClassInvalid)
	if _, err := tw.EditPR(ctx, pr.Number, platform.PREdit{Body: ptr("a body edit is fine")}); err != nil {
		t.Errorf("editing the body of a merged PR: %v", err)
	}
}

func TestCommentsAndLabels(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tc := range []struct {
		flavor fake.Flavor
		ids    []string
	}{
		{fake.GitHub, []string{"engineering-assets", "x", "engineering-assets"}},
		{fake.Gitea, []string{"1", "2", "1"}},
	} {
		e := newEnv(t, fake.WithFlavor(tc.flavor))
		r := e.repo("acme/api", "README.md", "x")
		tw := e.target(r)
		e.p.ResetCalls()
		ids, err := tw.EnsureLabels(ctx, []string{"engineering-assets", "x", "engineering-assets"})
		if err != nil {
			t.Fatal(err)
		}
		sameList(t, string(tc.flavor)+" ids", ids, tc.ids)
		again, _ := tw.EnsureLabels(ctx, []string{"x"})
		sameList(t, string(tc.flavor)+" ids again", again, tc.ids[1:2])
		sameList(t, string(tc.flavor)+" calls", e.p.Calls(), []string{
			"EnsureLabels acme/api", "CreateLabel acme/api engineering-assets", "CreateLabel acme/api x", "EnsureLabels acme/api"})
		_, err = tw.EnsureLabels(ctx, []string{"ok", ""})
		wantClass(t, "blank label", err, platform.ClassInvalid)
	}

	e := newEnv(t, fake.WithFlavor(fake.GitLab))
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync"})
	must(t, err)
	must(t, tw.Comment(ctx, pr.Number, "first"))
	must(t, tw.Comment(ctx, pr.Number, "second"))
	for name, body := range map[string]string{"blank": " \n", "quick action": "ok\n/approve", "too long": strings.Repeat("x", 200001)} {
		wantClass(t, name, tw.Comment(ctx, pr.Number, body), platform.ClassInvalid)
	}
	wantIs(t, "comment on a missing PR", tw.Comment(ctx, 99, "x"), platform.ErrNotFound)
	c := e.p.Comments(r.ID, pr.Number)
	if len(c) != 2 || c[0].Body != "first" || c[1].Body != "second" || c[0].Author.ID != e.writer.ID || !c[1].CreatedAt.After(c[0].CreatedAt) {
		t.Errorf("comments = %+v", c)
	}
	if e.p.Comments(r.ID, 99) != nil || e.p.Comments("nope", 1) != nil || e.p.Labels("nope") != nil {
		t.Error("comments or labels of something missing")
	}
}

func TestCloseAndRemote(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnv(t)
	r := e.repo("acme/api", "README.md", "x")

	rem, err := e.p.Reader(e.reader).Remote(ctx, r)
	if err != nil || rem.URL != "fake://github.com/acme/api" || rem.Header != nil {
		t.Errorf("reader Remote = %+v, %v", rem, err)
	}
	tw := e.target(r)
	trem := tw.Remote()
	if trem.URL != rem.URL || trem.Header == nil {
		t.Fatalf("target Remote = %+v", trem)
	}
	h, err := trem.Header(ctx)
	if err != nil || !strings.HasPrefix(h, "Basic ") {
		t.Fatalf("header = %q, %v", h, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	if err != nil || !strings.HasPrefix(string(raw), "x-access-token:fake-token-") {
		t.Errorf("header decodes to %q, %v", raw, err)
	}
	if other := e.target(r); other.Remote().Header == nil {
		t.Error("second target without a header")
	} else if h2, _ := other.Remote().Header(ctx); h2 == h {
		t.Error("two targets share a token")
	}

	must(t, tw.Close())
	must(t, tw.Close())
	_, err = tw.CreatePR(ctx, platform.NewPR{Head: "h", Base: "main", Title: "t"})
	wantRule(t, "write after Close", err, platform.ClassAuth, http.StatusUnauthorized, "")
	_, err = trem.Header(ctx)
	wantClass(t, "header after Close", err, platform.ClassAuth)

	// Close takes faults: a plain one leaves the token alive.
	tw2 := e.target(r)
	boom := &platform.Error{Class: platform.ClassTransient, Status: 502}
	e.p.FailNext("Close", boom)
	wantIs(t, "failing Close", tw2.Close(), boom)
	if _, err := tw2.EnsureLabels(ctx, []string{"x"}); err != nil {
		t.Errorf("the token died with a failed Close: %v", err)
	}
	e.p.FailNextApplied("Close", boom)
	wantIs(t, "applied Close fault", tw2.Close(), boom)
	_, err = tw2.EnsureLabels(ctx, []string{"x"})
	wantClass(t, "after an applied Close fault", err, platform.ClassAuth)
}

// TestBitbucketFlavor: the Bitbucket flavor reports the capabilities of
// Bitbucket's driver, refuses labels, and treats a pull request closed
// without merging as final: an edit that closes writes the body first,
// and afterwards no title, body or base changes and no one reopens it.
func TestBitbucketFlavor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := fake.CapsFor(fake.Bitbucket)
	if c.Flavor != "bitbucket" || !c.NoLabels || !c.ClosedImmutable || c.Marker != platform.MarkerInRefDef || c.BodyControls() ||
		c.Draft != platform.DraftNative || !c.CloserKnown || c.WorkflowPerm || c.LabelsByID || c.QuickActions || c.MaxBody != 60000 {
		t.Errorf("CapsFor(Bitbucket) = %+v", c)
	}
	for _, f := range []fake.Flavor{fake.GitHub, fake.GitLab, fake.Gitea, fake.Forgejo} {
		if c := fake.CapsFor(f); c.ClosedImmutable || c.NoLabels || !c.BodyControls() {
			t.Errorf("CapsFor(%s) = %+v", f, c)
		}
	}
	e := newEnv(t, fake.WithFlavor(fake.Bitbucket))
	r := e.repo("acme/api", "README.md", "x")
	tw := e.target(r)
	_, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Labels: []string{"engineering-assets"}})
	wantClass(t, "a pull request with labels", err, platform.ClassInvalid)
	pr, err := tw.CreatePR(ctx, platform.NewPR{Head: "touchmark/hub", Base: "main", Title: "sync", Body: "first", Draft: true})
	if err != nil || pr.URL != "https://github.com/acme/api/pull-requests/1" || pr.Title != "sync" || !pr.Draft {
		t.Fatalf("CreatePR = %+v, %v", pr, err)
	}
	_, err = tw.EditPR(ctx, pr.Number, platform.PREdit{AddLabels: []string{"x"}})
	wantClass(t, "a label added", err, platform.ClassInvalid)
	_, err = tw.EnsureLabels(ctx, []string{"x"})
	wantClass(t, "EnsureLabels", err, platform.ClassInvalid)

	// The close writes its body first, and the writer is the closer.
	closed := platform.Closed
	got, err := tw.EditPR(ctx, pr.Number, platform.PREdit{Body: ptr("closed by touchmark"), State: &closed})
	if err != nil || got.State != platform.Closed || got.Body != "closed by touchmark" || got.ClosedBy == nil || got.ClosedBy.ID != e.writer.ID {
		t.Fatalf("close = %+v, %v", got, err)
	}
	open := platform.Open
	for name, edit := range map[string]platform.PREdit{
		"reopen":        {State: &open},
		"body":          {Body: ptr("again")},
		"title":         {Title: ptr("renamed")},
		"base":          {Base: ptr("develop")},
		"body and open": {Body: ptr("again"), State: &open},
	} {
		_, err := tw.EditPR(ctx, pr.Number, edit)
		wantRule(t, name, err, platform.ClassUnsupported, http.StatusBadRequest, "")
	}
	// What the declined pull request already holds, and closing it again,
	// change nothing and succeed.
	for name, edit := range map[string]platform.PREdit{
		"close again":   {State: &closed},
		"the same body": {Body: ptr("closed by touchmark"), Title: ptr("sync"), Base: ptr("main")},
	} {
		if _, err := tw.EditPR(ctx, pr.Number, edit); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after := e.p.PR(r.ID, pr.Number); after.State != platform.Closed || after.Body != "closed by touchmark" || after.Title != "sync" ||
		after.Base != "main" || len(after.Labels) != 0 {
		t.Errorf("after the refused edits: %+v", after)
	}
	// A person cannot reopen it either.
	e.p.SetPRState(r.ID, pr.Number, platform.Open, nil, time.Time{})
	wantSetupErr(t, e.p, "cannot be reopened")
}
