//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/cli"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestDoctor runs `touchmark doctor` against the forge as a maintainer
// does, with the writer's token, in a fresh organisation:
//   - api: opted in, the writer writes it: access ok, no sync branch yet
//     (rules unknown: the writer reads the protection of existing branches
//     only), no marker;
//   - ro: opted in, the writer only reads it: access fails;
//   - web: not opted in: skipped;
//   - prot: opted in, writable, with the sync branch protected against
//     pushes (touchmark/*): rules fail;
//   - twin: opted in, writable; a pull request of the writer's on the sync
//     branch carries a marker of a hub with the same id and another
//     fingerprint, and a person's open one a copy of this hub's marker:
//     markers warn with both.
//
// The hub is a repository of the organisation that the writer reads
// (hub-hidden warns: it is a member), then writes (fails). The token's
// scopes come from GET /api/v1/token where the forge has it (Gitea 1.27).
// With --hub-token and a maintainer's token (the admin's here), doctor
// reads the hub's Actions secrets: a write key there fails. No token
// reaches any output.
func TestDoctor(t *testing.T) {
	e := needLive(t)
	t.Setenv("CI", "")
	fx := newOrg(t, e, "doctor")
	id := "e2e-" + randHex(t, 3)
	branch := "touchmark/" + id
	optIn := []map[string]any{{"operation": "create", "path": config.DefaultOptIn, "content": b64([]byte("version: 1\n"))}}
	readme := []map[string]any{{"operation": "create", "path": "README.md", "content": b64([]byte("# doctor\n"))}}
	create := func(name string, files []map[string]any, writable bool) string {
		p := fx.org + "/" + name
		fx.admin().ok(t, http.MethodPost, "/orgs/"+fx.org+"/repos", map[string]any{
			"name": name, "private": true, "auto_init": false, "default_branch": "main",
		}, nil)
		fx.admin().ok(t, http.MethodPost, "/repos/"+p+"/contents", map[string]any{"message": "init", "files": files}, nil)
		if writable {
			fx.admin().ok(t, http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s", fx.writeTeam, p), nil, nil)
		}
		return p
	}
	create("api", optIn, true)
	create("ro", optIn, false)
	create("web", readme, true)
	prot := create("prot", optIn, true)
	fx.commit(t, e.Person, prot, branch)
	fx.admin().ok(t, http.MethodPost, "/repos/"+prot+"/branch_protections", map[string]any{
		"rule_name": "touchmark/*", "enable_push": false,
	}, nil)
	twin := create("twin", optIn, true)
	hubPath := create("hub", readme, false)
	var hubRepo apiRepo
	fx.admin().get(t, "/repos/"+hubPath, &hubRepo)
	fp := fmt.Sprintf("%s/%d", e.Host, hubRepo.ID)

	// The markers: another hub's, with this hub's id, on a pull request of
	// the writer's; this hub's, copied by a person.
	twinRepo := fx.repo(t, twin)
	markerBody := func(fingerprint string) string {
		line, err := marker.Encode(marker.Marker{Key: "sha256:" + strings.Repeat("ab", 32), Data: marker.Data{
			V: marker.Version, Stream: decide.StreamSync, Hub: id, FP: fingerprint,
			DecidedAt: strings.Repeat("0", 40), ContentCommit: strings.Repeat("0", 40), Engine: "0.2.0",
			Packs: []string{"base"}, TitleSet: "chore: sync engineering assets", LabelsSet: []string{},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return "Engineering assets.\n\n" + line
	}
	other := fmt.Sprintf("%s/%d", e.Host, hubRepo.ID+1000)
	foreign := fx.openPR(t, twinRepo, e.Writer, prOptions{head: branch, base: "main", title: "sync", body: markerBody(other)})
	fx.env.api(e.Writer).ok(t, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", twin, foreign.Number), map[string]any{"state": "closed"}, nil)
	copyPR := fx.openPR(t, twinRepo, e.Person, prOptions{head: branch, base: "main", title: "copy", body: markerBody(fp)})

	// The hub checkout, with origin on the forge: doctor finds the hub's
	// path there.
	hub := t.TempDir()
	gitCmd(t, hub, nil, nil, "init", "-q", "-b", "master")
	files := map[string]string{
		config.HubFile: fmt.Sprintf("version: 1\nid: %s\nproviders:\n  - id: forge\n    type: %s\n    url: %s\n    writer: %s\n",
			id, e.Flavor, e.URL, e.Writer.Login),
		config.TargetsFile:     fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - org: %s\n", fx.org),
		"packs/base/AGENTS.md": text("base AGENTS.md v1"),
	}
	for p, content := range files {
		full := filepath.Join(hub, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, hub, nil, nil, "add", "-A")
	gitCmd(t, hub, nil, nil, "commit", "-q", "-m", "the hub")
	gitCmd(t, hub, nil, nil, "remote", "add", "origin", e.remote(hubPath))

	var outputs []string
	doctor := func(code int, vars map[string]string, extra ...string) report.Doctor {
		t.Helper()
		for k, v := range vars {
			t.Setenv(k, v)
		}
		args := append([]string{"doctor", "--hub", hub, "--hub-fp", fp, "--format", "json"}, extra...)
		var stdout, stderr bytes.Buffer
		got := cli.Main(context.Background(), args, &stdout, &stderr)
		outputs = append(outputs, stdout.String(), stderr.String())
		if got != code {
			t.Fatalf("touchmark %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, code,
				e.Redact.Replace(stdout.String()), e.Redact.Replace(stderr.String()))
		}
		validate(t, "doctor", stdout.Bytes(), e.Redact.Replace)
		var doc report.Doctor
		dec := json.NewDecoder(strings.NewReader(stdout.String()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("decode the doctor report: %v\n%s", err, e.Redact.Replace(stdout.String()))
		}
		return doc
	}
	writerVars := map[string]string{"TOUCHMARK_FORGE_WRITE_TOKEN": e.Writer.Token}
	doc := doctor(1, writerVars)

	check := func(where string, cs []report.DoctorCheck, name string) report.DoctorCheck {
		t.Helper()
		for _, c := range cs {
			if c.Name == name {
				return c
			}
		}
		t.Errorf("%s: no check %s in %+v", where, name, cs)
		return report.DoctorCheck{}
	}
	want := func(where string, cs []report.DoctorCheck, name string, status report.CheckStatus, detail string) {
		t.Helper()
		if c := check(where, cs, name); c.Status != status || !strings.Contains(c.Detail, detail) {
			t.Errorf("%s %s: %s %q, want %s with %q", where, name, c.Status, c.Detail, status, detail)
		}
	}
	target := func(name string) report.DoctorTarget {
		t.Helper()
		for _, tg := range doc.Targets {
			if tg.Path == fx.org+"/"+name {
				return tg
			}
		}
		t.Fatalf("no target %s in %+v", name, doc.Targets)
		return report.DoctorTarget{}
	}
	if len(doc.Providers) != 1 {
		t.Fatalf("providers %+v", doc.Providers)
	}
	pc := doc.Providers[0].Checks
	want("provider", pc, "writer", report.StatusOK, e.Writer.Login)
	want("provider", pc, "token-expiry", report.StatusOK, "no expiry date")
	want("provider", pc, "2fa", report.StatusUnknown, "do not show")
	want("provider", pc, "hub-hidden", report.StatusWarn, "sees the private hub")
	scopes := check("provider", pc, "scopes")
	if e.Flavor == "gitea" && scopes.Status != report.StatusOK {
		t.Errorf("scopes on Gitea: %s %q", scopes.Status, scopes.Detail)
	}
	finding(t, "doctor-scopes", "the writer's scopes: %s %q", scopes.Status, scopes.Detail)

	want("api", target("api").Checks, "access", report.StatusOK, "may push")
	want("api", target("api").Checks, "rules", report.StatusUnknown, "no sync branch exists yet")
	want("api", target("api").Checks, "markers", report.StatusOK, "no marker")
	want("ro", target("ro").Checks, "access", report.StatusFail, "may not push")
	if tg := target("web"); tg.Skipped != "not-opted-in" {
		t.Errorf("web: %+v", tg)
	}
	protRules := check("prot", target("prot").Checks, "rules")
	if protRules.Status != report.StatusFail || !strings.Contains(protRules.Detail, "blocked:rules:protected-branch") {
		t.Errorf("prot rules: %s %q", protRules.Status, protRules.Detail)
	}
	finding(t, "doctor-protected-branch", "a sync branch under a rule touchmark/* without pushes: %s %q", protRules.Status, protRules.Detail)
	markers := check("twin", target("twin").Checks, "markers")
	if markers.Status != report.StatusWarn || !strings.Contains(markers.Detail, other) ||
		!strings.Contains(markers.Detail, fmt.Sprintf("#%d by %s", copyPR.Number, e.Person.Login)) {
		t.Errorf("twin markers: %s %q", markers.Status, markers.Detail)
	}

	// The writer writes the hub: hub-hidden fails.
	fx.admin().ok(t, http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s", fx.writeTeam, hubPath), nil, nil)
	doc = doctor(1, writerVars)
	want("provider", doc.Providers[0].Checks, "hub-hidden", report.StatusFail, "may push to the hub")

	// --hub-token: a write key among the hub's Actions secrets fails.
	resp := fx.admin().do(t, http.MethodPut, "/repos/"+hubPath+"/actions/secrets/TOUCHMARK_FORGE_WRITE_TOKEN", map[string]any{"data": "a-secret-value-" + randHex(t, 4)})
	doc = doctor(1, map[string]string{"TOUCHMARK_FORGE_WRITE_TOKEN": e.Writer.Token, "TOUCHMARK_HUB_TOKEN": e.Admin.Token}, "--hub-token")
	if !doc.HubToken {
		t.Error("hub_token is not set")
	}
	var keys []string
	for _, c := range doc.HubChecks {
		if c.Name == "key-location" {
			keys = append(keys, string(c.Status)+": "+c.Detail)
		}
	}
	const listed = "fail: Actions secret TOUCHMARK_FORGE_WRITE_TOKEN (repository)"
	switch {
	case e.Flavor == "gitea":
		// Gitea stores and lists Actions secrets (1.27.3, the pinned image):
		// the write key must be found. An unreadable listing once passed as
		// well.
		if resp.Status/100 != 2 {
			t.Errorf("PUT actions/secrets: HTTP %d", resp.Status)
		} else if !containsLine(keys, listed) {
			t.Errorf("key-location with the secret in the hub: %q", keys)
		}
	case resp.Status/100 == 2 && containsLine(keys, "unknown: not readable with this token: the repository's Actions secrets"):
		// Forgejo: whether it lists Actions secrets is not verified; an
		// unreadable listing is an unknown check.
	case resp.Status/100 == 2:
		if !containsLine(keys, listed) {
			t.Errorf("key-location with the secret in the hub: %q", keys)
		}
	default:
		t.Logf("PUT actions/secrets: HTTP %d", resp.Status)
	}
	finding(t, "doctor-hub-token", "PUT /repos/{hub}/actions/secrets/{name}: HTTP %d; key-location: %s", resp.Status, strings.Join(keys, " | "))

	for i, out := range outputs {
		for _, a := range []account{e.Admin, e.Reader, e.Writer, e.Person} {
			if strings.Contains(out, a.Token) {
				t.Errorf("output %d holds the token of %s", i, a.Login)
			}
		}
	}
}

// containsLine reports whether some line of lines starts with prefix.
func containsLine(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
