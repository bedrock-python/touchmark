package distribute

import (
	"context"
	"testing"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
)

// guardWriter is a writer whose GuardHub answers findings.
type guardWriter struct {
	platform.Writer
	findings []platform.Finding
}

func (g guardWriter) GuardHub(context.Context, platform.Repo) ([]platform.Finding, error) {
	return g.findings, nil
}

// TestDoctorHubGuard: under security.writer_on_hub guard, a writer that
// reaches the hub gets hub-guard instead of hub-hidden: the worst of the
// driver's conditions, unknown without HubGuard; a writer that does not
// see the hub keeps hub-hidden.
func TestDoctorHubGuard(t *testing.T) {
	ok := platform.Finding{Check: "hub-guard", Status: platform.FindingOK, Detail: "keeps the writer from pushing to main"}
	for _, tc := range []struct {
		name     string
		visible  bool
		findings []platform.Finding
		guard    bool
		check    string
		status   report.CheckStatus
		detail   string
	}{
		{"not visible", false, nil, true, "hub-hidden", report.StatusOK, "does not see the hub"},
		{"no HubGuard", true, nil, false, "hub-guard", report.StatusUnknown, "verified on GitLab only"},
		{"guarded", true, []platform.Finding{ok, ok}, true, "hub-guard", report.StatusOK, "cannot get content onto its default branch"},
		{"a failing condition", true, []platform.Finding{ok, {Check: "hub-guard", Status: platform.FindingFail, Detail: "main is not protected"},
			{Check: "hub-guard", Status: platform.FindingWarn, Detail: "protected branch release"}}, true, "hub-guard", report.StatusFail,
			"fail: main is not protected; warn: protected branch release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.hubYML += "security:\n  writer_on_hub: guard\n"
			if tc.visible {
				hub := w.repo("acme/engineering-assets", nil)
				w.p.GrantWrite(hub.ID, w.writer)
			}
			w.ok()
			dd := w.doctorDeps(w.writer)
			if tc.guard {
				for i := range dd.Providers {
					dd.Providers[i].Writer = guardWriter{Writer: dd.Providers[i].Writer, findings: tc.findings}
				}
			}
			doc := w.doctor(dd)
			wantCheck(t, "gh", doc.Providers[0].Checks, tc.check, tc.status, tc.detail)
		})
	}
}

// TestKeyLocationGuardSettings: doctor --hub-token grades, under
// security.writer_on_hub guard, the GitLab hub's CI configuration path
// and merge checks.
func TestKeyLocationGuardSettings(t *testing.T) {
	str := func(s string) *string { return &s }
	yes, no := true, false
	guard := hubWithIsolation("platform", "")
	guard.Security.WriterOnHub = "guard"
	for _, tc := range []struct {
		name           string
		path           *string
		after, skipped *bool
		status         report.CheckStatus
		detail         string
	}{
		{"guarded", str(".gitlab-ci.yml@Acme/Engineering-Assets:main"), &yes, &no, report.StatusOK, "read their CI file from main"},
		{"the source branch's CI file", str(""), &yes, &no, report.StatusFail, ".gitlab-ci.yml@acme/engineering-assets:main"},
		{"another project", str(".gitlab-ci.yml@acme/other:main"), &yes, &no, report.StatusFail, "source branch"},
		{"no pipeline needed", str(".gitlab-ci.yml@acme/engineering-assets:main"), &no, &no, report.StatusFail, "Pipelines must succeed"},
		{"skipped counts", str(".gitlab-ci.yml@acme/engineering-assets:main"), &yes, &yes, report.StatusFail, "skipped pipeline"},
		{"not shown", nil, nil, nil, report.StatusUnknown, "not shown"},
	} {
		ks := hubch.KeyStore{Platform: "gitlab", RepoPath: "acme/engineering-assets", DefaultBranch: "main",
			CIConfigPath: tc.path, MergeAfterPipeline: tc.after, MergeOnSkipped: tc.skipped}
		lines := keyChecks(KeyLocationChecks(ks, nil, guard), "hub-guard")
		if !hasCheck(lines, tc.status, tc.detail) {
			t.Errorf("%s: no %s with %q in %q", tc.name, tc.status, tc.detail, lines)
		}
		if lines := keyChecks(KeyLocationChecks(ks, nil, hubWithIsolation("platform", "")), "hub-guard"); len(lines) != 0 {
			t.Errorf("%s: hub-guard without guard: %q", tc.name, lines)
		}
	}
}
