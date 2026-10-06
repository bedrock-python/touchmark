package distribute

import (
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/platform/fake"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// doctorDeps returns the dependencies of Doctor over the world, with the
// writer's identity as the provider's write credential (as when the
// credential acts as another account).
func (w *world) doctorDeps(as platform.Account) DoctorDeps {
	w.t.Helper()
	d := w.deps()
	for i := range d.Providers {
		d.Providers[i].Writer = w.p.Writer(as)
		d.Providers[i].Reader = nil
	}
	d.Snapshots = nil
	return DoctorDeps{Deps: d, HubHost: "github.com", HubPath: "acme/engineering-assets"}
}

// doctor runs Doctor and fails the test on an error or a setup error.
func (w *world) doctor(dd DoctorDeps) *report.Doctor {
	w.t.Helper()
	doc, err := Doctor(w.t.Context(), dd)
	if err != nil {
		w.t.Fatalf("Doctor: %v", err)
	}
	w.ok()
	checkDoctorReport(w.t, doc)
	return doc
}

// doctorTarget returns the entry of ref ("<provider>:<path>").
func doctorTarget(t *testing.T, doc *report.Doctor, ref string) report.DoctorTarget {
	t.Helper()
	for _, tg := range doc.Targets {
		if tg.Provider+":"+tg.Path == ref {
			return tg
		}
	}
	t.Fatalf("no target %s in %+v", ref, doc.Targets)
	return report.DoctorTarget{}
}

// checkOf returns check name of cs; ok false when there is none.
func checkOf(cs []report.DoctorCheck, name string) (report.DoctorCheck, bool) {
	for _, c := range cs {
		if c.Name == name {
			return c, true
		}
	}
	return report.DoctorCheck{}, false
}

// wantCheck checks the status of check name in cs, and that its detail
// holds detail.
func wantCheck(t *testing.T, where string, cs []report.DoctorCheck, name string, status report.CheckStatus, detail string) {
	t.Helper()
	c, ok := checkOf(cs, name)
	switch {
	case !ok:
		t.Errorf("%s: no check %s in %+v", where, name, cs)
	case c.Status != status || !strings.Contains(c.Detail, detail):
		t.Errorf("%s: %s is %s %q, want %s with %q", where, name, c.Status, c.Detail, status, detail)
	}
}

// TestDoctor: the matrix of a hub's targets, from what the write identity
// can read.
func TestDoctor(t *testing.T) {
	w := newWorld(t)
	api := w.optedIn("acme/api", nil)
	w.p.GrantWrite(api.ID, w.writer)
	w.repo("acme/web", nil, "README.md", "not opted in\n")
	w.optedIn("acme/old", func(r *platform.Repo) { r.Archived = true })
	w.optedIn("acme/ro", nil)
	twin := w.optedIn("acme/twin", nil)
	w.p.GrantWrite(twin.ID, w.writer)
	// Another hub with the same id wrote a marker on the sync branch; a
	// person copied this hub's marker into a pull request of their own.
	w.pr(twin, platform.PR{Head: branch, Author: w.writer, Title: "sync", Body: body(t, staleKey, otherFP), State: platform.Closed})
	w.pr(twin, platform.PR{Head: branch, Author: w.person, Title: "copy", Body: body(t, staleKey, hubFP)})
	rules := w.optedIn("acme/rules", nil)
	w.p.GrantWrite(rules.ID, w.writer)
	w.p.AddRuleset(rules.ID, fake.Ruleset{Branches: []string{"touchmark/*"}, NonFastForward: true})
	hub := w.repo("acme/engineering-assets", func(r *platform.Repo) { r.Visibility = "private" })
	w.p.Grant(hub.ID, w.writer, platform.Perms{})
	w.ok()

	doc := w.doctor(w.doctorDeps(w.writer))
	if len(doc.Providers) != 1 {
		t.Fatalf("providers %+v", doc.Providers)
	}
	pc := doc.Providers[0].Checks
	wantCheck(t, "gh", pc, "writer", report.StatusOK, "acts as acme-write[bot]")
	wantCheck(t, "gh", pc, "token-expiry", report.StatusOK, "do not expire")
	wantCheck(t, "gh", pc, "signing", report.StatusUnknown, "TOUCHMARK_GH_SIGNING_KEY")
	wantCheck(t, "gh", pc, "hub-hidden", report.StatusWarn, "sees the private hub")
	if _, ok := checkOf(pc, "signing-key"); ok {
		t.Errorf("a signing-key check without a key: %+v", pc)
	}

	wantCheck(t, "api", doctorTarget(t, doc, "gh:acme/api").Checks, "access", report.StatusOK, "may push")
	wantCheck(t, "api", doctorTarget(t, doc, "gh:acme/api").Checks, "markers", report.StatusOK, "no marker")
	if got := doctorTarget(t, doc, "gh:acme/web"); got.Skipped != "not-opted-in" || len(got.Checks) != 0 {
		t.Errorf("web: %+v", got)
	}
	if got := doctorTarget(t, doc, "gh:acme/old"); got.Skipped != "archived" {
		t.Errorf("old: %+v", got)
	}
	wantCheck(t, "ro", doctorTarget(t, doc, "gh:acme/ro").Checks, "access", report.StatusFail, "may not push")
	twinChecks := doctorTarget(t, doc, "gh:acme/twin").Checks
	wantCheck(t, "twin", twinChecks, "markers", report.StatusWarn, "#1 ("+otherFP+")")
	wantCheck(t, "twin", twinChecks, "markers", report.StatusWarn, "#2 by jdoe")
	wantCheck(t, "rules", doctorTarget(t, doc, "gh:acme/rules").Checks, "rules", report.StatusWarn, "force pushes blocked on touchmark/acme-eng")
	for _, tg := range doc.Targets {
		if tg.Path == "acme/legacy" {
			t.Errorf("an excluded target was checked: %+v", tg)
		}
	}
	if doc.ExitCode() != 1 {
		t.Errorf("exit %d with a failed check, want 1", doc.ExitCode())
	}
	if doc.Summary[report.StatusFail] != 1 {
		t.Errorf("summary %v", doc.Summary)
	}
	for _, c := range w.p.Writes() {
		t.Errorf("doctor wrote %s", c)
	}
}

// TestDoctorOptIn: doctor checks a repository targets.yml subscribes
// without an opt-in file, as delivery writes to it, and skips one whose
// opt-in file says enabled: false, whatever targets.yml says.
func TestDoctorOptIn(t *testing.T) {
	w := newWorld(t)
	w.targetsYML = assumedTargetsYML
	svc := w.repo("acme/svc-a", nil, "README.md", "subscribed by the hub\n")
	w.p.GrantWrite(svc.ID, w.writer)
	w.repo("acme/svc-off", nil, optInName, "version: 1\nenabled: false\n")
	w.repo("acme/web", topics("python"), "README.md", "wants an opt-in file\n")
	broken := w.repo("acme/svc-broken", nil, optInName, "version: 2\n")
	w.p.GrantWrite(broken.ID, w.writer)
	doc := w.doctor(w.doctorDeps(w.writer))
	wantCheck(t, "svc-a", doctorTarget(t, doc, "gh:acme/svc-a").Checks, "access", report.StatusOK, "may push")
	if got := doctorTarget(t, doc, "gh:acme/svc-off"); got.Skipped != "opted-out" || len(got.Checks) != 0 {
		t.Errorf("svc-off: %+v", got)
	}
	if got := doctorTarget(t, doc, "gh:acme/web"); got.Skipped != "not-opted-in" {
		t.Errorf("web: %+v", got)
	}
	// An opt-in file that does not parse is delivery's business: the
	// writer's access is checked anyway.
	wantCheck(t, "svc-broken", doctorTarget(t, doc, "gh:acme/svc-broken").Checks, "access", report.StatusOK, "may push")
}

// TestDoctorHub: whether the writer may write to the hub, by what it sees.
func TestDoctorHub(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(w *world)
		host   string
		path   string
		status report.CheckStatus
		detail string
	}{
		{"not visible", func(w *world) {}, "github.com", "acme/engineering-assets", report.StatusOK, "does not see the hub"},
		{"writable", func(w *world) {
			hub := w.repo("acme/engineering-assets", nil)
			w.p.GrantWrite(hub.ID, w.writer)
		}, "github.com", "acme/engineering-assets", report.StatusFail, "may push to the hub"},
		{"public, read only", func(w *world) {
			w.repo("acme/engineering-assets", nil)
		}, "github.com", "acme/engineering-assets", report.StatusOK, "sees the public hub but cannot push"},
		{"another host", func(w *world) {}, "gitlab.example.com", "acme/engineering-assets", report.StatusOK, "the hub is on gitlab.example.com"},
		{"path unknown", func(w *world) {}, "github.com", "", report.StatusUnknown, "path is unknown"},
		{"host unknown", func(w *world) {}, "", "", report.StatusUnknown, "host is unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(w)
			w.ok()
			dd := w.doctorDeps(w.writer)
			dd.HubHost, dd.HubPath = tc.host, tc.path
			doc := w.doctor(dd)
			wantCheck(t, "gh", doc.Providers[0].Checks, "hub-hidden", tc.status, tc.detail)
		})
	}
}

// TestDoctorWriterMismatch: a credential of another account fails the
// writer check, and the targets are still checked with it.
func TestDoctorWriterMismatch(t *testing.T) {
	w := newWorld(t)
	api := w.optedIn("acme/api", nil)
	w.p.GrantWrite(api.ID, w.person)
	doc := w.doctor(w.doctorDeps(w.person))
	wantCheck(t, "gh", doc.Providers[0].Checks, "writer", report.StatusFail, "acts as jdoe, and hub.yml names acme-write[bot]")
	wantCheck(t, "api", doctorTarget(t, doc, "gh:acme/api").Checks, "access", report.StatusOK, "jdoe may push")
	if doc.Providers[0].Self != "jdoe" || doc.Providers[0].Writer != "acme-write[bot]" {
		t.Errorf("provider %+v", doc.Providers[0])
	}
}

// TestDoctorSigning: the signing checks follow the key, the platform's API
// commits and the rules the writer can read.
func TestDoctorSigning(t *testing.T) {
	signer, err := sshsig.ParsePrivateKey(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("key registered", func(t *testing.T) {
		w := newWorld(t)
		w.p.SetSigningKeys(w.writer, signer.PublicKey())
		dd := w.doctorDeps(w.writer)
		dd.Write.Signers = map[string]*sshsig.Signer{"gh": signer}
		doc := w.doctor(dd)
		wantCheck(t, "gh", doc.Providers[0].Checks, "signing", report.StatusOK, "SSH key of TOUCHMARK_GH_SIGNING_KEY")
		wantCheck(t, "gh", doc.Providers[0].Checks, "signing-key", report.StatusOK, "acme-write[bot]'s signing key")
	})
	t.Run("key not registered", func(t *testing.T) {
		w := newWorld(t)
		dd := w.doctorDeps(w.writer)
		dd.Write.Signers = map[string]*sshsig.Signer{"gh": signer}
		doc := w.doctor(dd)
		wantCheck(t, "gh", doc.Providers[0].Checks, "signing-key", report.StatusFail, "not among")
	})
	t.Run("api commits", func(t *testing.T) {
		w := newWorld(t, fake.WithAPICommits())
		doc := w.doctor(w.doctorDeps(w.writer))
		wantCheck(t, "gh", doc.Providers[0].Checks, "signing", report.StatusOK, "platform signs")
	})
	t.Run("sign always", func(t *testing.T) {
		w := newWorld(t)
		w.hubYML = strings.Replace(defaultHubYML, "    writer: acme-write[bot]\n", "    writer: acme-write[bot]\n    sign: always\n", 1)
		doc := w.doctor(w.doctorDeps(w.writer))
		wantCheck(t, "gh", doc.Providers[0].Checks, "signing", report.StatusFail, "sign: always")
	})
	t.Run("rules per target", func(t *testing.T) {
		w := newWorld(t, fake.WithPreflight())
		signed := w.optedIn("acme/signed", nil)
		w.p.GrantWrite(signed.ID, w.writer)
		w.p.AddRuleset(signed.ID, fake.Ruleset{Branches: []string{"~DEFAULT_BRANCH"}, RequiredSignatures: true})
		plain := w.optedIn("acme/plain", nil)
		w.p.GrantWrite(plain.ID, w.writer)
		w.ok()
		doc := w.doctor(w.doctorDeps(w.writer))
		wantCheck(t, "gh", doc.Providers[0].Checks, "signing", report.StatusOK, "checked one by one")
		wantCheck(t, "signed", doctorTarget(t, doc, "gh:acme/signed").Checks, "signing", report.StatusFail, "blocked:cannot-sign")
		wantCheck(t, "plain", doctorTarget(t, doc, "gh:acme/plain").Checks, "signing", report.StatusOK, "no rule")
	})
}

// TestDoctorPublicHub: a public hub names no non-public target in its
// report, not in a check's detail either; the target is still checked
// and counted.
func TestDoctorPublicHub(t *testing.T) {
	w := newWorld(t)
	w.ctx = hubch.Context{CI: hubch.GitHubActions, Visibility: "public"}
	secret := w.optedIn("acme/secret-plans", func(r *platform.Repo) { r.Visibility = "private" })
	_ = secret
	api := w.optedIn("acme/api", nil)
	w.p.GrantWrite(api.ID, w.writer)
	w.ok()
	doc := w.doctor(w.doctorDeps(w.writer))
	hidden := 0
	for _, tg := range doc.Targets {
		if tg.Path == "" {
			hidden++
			if tg.RepoID != "" || len(tg.Checks) == 0 {
				t.Errorf("hidden target %+v", tg)
			}
			for _, c := range tg.Checks {
				if c.Detail != "" {
					t.Errorf("hidden target's check %+v has a detail", c)
				}
			}
		}
	}
	if hidden != 1 {
		t.Errorf("%d hidden targets, want 1: %+v", hidden, doc.Targets)
	}
	var b strings.Builder
	for _, write := range []func() error{
		func() error { return doc.WriteText(&b) },
		func() error { return doc.WriteMarkdown(&b) },
		func() error { return report.WriteJSON(&b, doc) },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(b.String(), "secret-plans") {
		t.Errorf("the output names the private target:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "1 target private in a public hub, not named: fail 1, ok 1") &&
		!strings.Contains(b.String(), "1 target private in a public hub, not named") {
		t.Errorf("the output does not count the private target:\n%s", b.String())
	}
}

// TestDoctorOutOfBudget: once the provider is out of budget, the rest of
// its targets are skipped as deferred, and nothing fails for it.
func TestDoctorRateLimited(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{"acme/a", "acme/b"} {
		r := w.optedIn(path, nil)
		w.p.GrantWrite(r.ID, w.writer)
	}
	w.ok()
	limited := &platform.Error{Op: "check", Class: platform.ClassRateLimited, Status: 429}
	w.p.Fail("Check", limited)
	dd := w.doctorDeps(w.writer)
	dd.Concurrency = 1
	doc := w.doctor(dd)
	for _, tg := range doc.Targets {
		c, ok := checkOf(tg.Checks, "access")
		if tg.Skipped == "" && (!ok || c.Status != report.StatusUnknown) {
			t.Errorf("%s: %+v", tg.Path, tg)
		}
	}
	if doc.ExitCode() != 0 {
		t.Errorf("exit %d: nothing failed", doc.ExitCode())
	}
	doc.Strict = true
	if doc.ExitCode() != 3 {
		t.Errorf("strict exit %d, want 3", doc.ExitCode())
	}
}

// TestDoctorAuthCircuit: a credential the platform refuses for three
// targets in a row stops the calls to the provider; its remaining targets
// are deferred without a call.
func TestDoctorAuthCircuit(t *testing.T) {
	w := newWorld(t)
	for i := range 6 {
		r := w.optedIn("acme/r"+string(rune('a'+i)), nil)
		w.p.GrantWrite(r.ID, w.writer)
	}
	w.ok()
	w.p.Fail("Check", errAuth)
	dd := w.doctorDeps(w.writer)
	dd.Concurrency = 1
	doc := w.doctor(dd)
	deferred := 0
	for _, tg := range doc.Targets {
		if tg.Skipped == "deferred:provider-down" {
			deferred++
		}
	}
	if deferred != 3 {
		t.Errorf("%d targets deferred, want 3: %+v", deferred, doc.Targets)
	}
}

// TestDoctorDeferredUnknown: targets left unchecked because the provider is
// out of budget carry an unknown check, so that doctor --strict does not
// pass a fleet it could not check (exit 3). Such targets once
// had no check at all, and --strict exited 0.
func TestDoctorDeferredUnknown(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{"acme/a", "acme/b", "acme/c"} {
		r := w.optedIn(path, nil)
		w.p.GrantWrite(r.ID, w.writer)
	}
	w.ok()
	w.p.Fail("ReadFile", &platform.Error{Op: "read file", Class: platform.ClassRateLimited, Status: 429})
	dd := w.doctorDeps(w.writer)
	dd.Concurrency = 1
	doc := w.doctor(dd)
	for _, tg := range doc.Targets {
		c, ok := checkOf(tg.Checks, "access")
		if tg.Skipped != "deferred:rate-limit" || !ok || c.Status != report.StatusUnknown || !strings.Contains(c.Detail, "not checked") {
			t.Errorf("%s: skipped %q, checks %+v", tg.Path, tg.Skipped, tg.Checks)
		}
	}
	if n := doc.Summary[report.StatusUnknown]; n < len(doc.Targets) {
		t.Errorf("%d unknown in the summary, %d targets unchecked", n, len(doc.Targets))
	}
	doc.Strict = true
	if code := doc.ExitCode(); code != 3 {
		t.Errorf("strict exit %d, want 3", code)
	}
	// A report whose deferred targets carry no check exits 3 too.
	doc.Targets = []report.DoctorTarget{{Provider: "gh", Path: "acme/a", Skipped: "deferred:rate-limit"}}
	doc.HubChecks, doc.Providers = nil, nil
	doc.Summarize()
	if code := doc.ExitCode(); code != 3 {
		t.Errorf("a deferred target without checks: strict exit %d, want 3", code)
	}
}
