//go:build e2e

package gitlabe2e

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
	"github.com/bedrock-python/touchmark/internal/platform/conformance"
	"github.com/bedrock-python/touchmark/internal/report"
)

// TestDoctor runs `touchmark doctor` against GitLab as a maintainer does,
// with the writer's token, over a fresh subgroup the
// writer is a Developer of:
//   - api: opted in: access ok, no protected branch covers the sync
//     branch (rules ok), no marker;
//   - prot: touchmark/* protected for Maintainers: rules fail;
//   - noforce: touchmark/* protected for Developers without force pushes:
//     rules warn;
//   - web: not opted in: skipped;
//   - twin: a merge request of the writer's on the sync branch carries a
//     marker of a hub with the same id and another fingerprint, and a
//     person's open one a copy of this hub's marker: markers warn.
//
// The writer's identity: its token's expiry and scopes from
// /personal_access_tokens/self, and it is a bot or service account (2fa
// ok). The hub is first a project of a top-level group the writer is no
// member of (hub-hidden ok), then one of the subgroup (fail). With
// --hub-token and root's token as the maintainer's, doctor reads the hub's
// variables, protected branches and tags: an unprotected write key fails,
// a protected one scoped to touchmark-distribute is ok, and a protected
// tag Developers may create fails. No token reaches any output.
func TestDoctor(t *testing.T) {
	e := needLive(t)
	t.Setenv("CI", "")
	fx := newGroup(t, e, "doctor")
	id := "e2e-" + randHex(t, 3)
	branch := "touchmark/" + id
	optIn := []conformance.File{{Path: config.DefaultOptIn, Content: []byte("version: 1\n")}}
	readme := []conformance.File{{Path: "README.md", Content: []byte("# doctor\n")}}
	projects := map[string]apiProject{}
	create := func(ns int64, name string, files []conformance.File) apiProject {
		p := fx.createProject(t, ns, name, files)
		e.waitAccess(t, p.ID, e.Writer)
		projects[name] = p
		return p
	}
	create(fx.nsID, "api", optIn)
	create(fx.nsID, "web", readme)
	prot := create(fx.nsID, "prot", optIn)
	noforce := create(fx.nsID, "noforce", optIn)
	root := e.api(e.Root)
	root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/protected_branches", prot.ID),
		map[string]any{"name": "touchmark/*", "push_access_level": levelMaintainer, "merge_access_level": levelMaintainer}, nil)
	root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/protected_branches", noforce.ID),
		map[string]any{"name": "touchmark/*", "push_access_level": levelDeveloper, "merge_access_level": levelDeveloper, "allow_force_push": false}, nil)
	twin := create(fx.nsID, "twin", optIn)

	// The hub: a project of a top-level group the writer is no member of.
	hubGroup := fx.sideGroup(t, "hub", nil)
	hub := fx.createProject(t, fx.groupID(t, hubGroup), "hub", readme)
	fp := fmt.Sprintf("%s/%d", e.Host, hub.ID)

	markerBody := func(fingerprint string) string {
		line, err := marker.Encode(marker.Marker{Key: "sha256:" + strings.Repeat("cd", 32), Data: marker.Data{
			V: marker.Version, Stream: decide.StreamSync, Hub: id, FP: fingerprint,
			DecidedAt: strings.Repeat("0", 40), ContentCommit: strings.Repeat("0", 40), Engine: "0.2.0",
			Packs: []string{"base"}, TitleSet: "chore: sync engineering assets", LabelsSet: []string{},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return "Engineering assets.\n\n" + line
	}
	other := fmt.Sprintf("%s/%d", e.Host, hub.ID+1000)
	twinRepo := fx.repo(t, twin.ID)
	foreign := fx.openMR(t, twinRepo, e.Writer, mrOptions{head: branch, base: "main", title: "sync", body: markerBody(other)})
	e.setState(t, e.Writer, twin.ID, foreign.IID, "close")
	copyMR := fx.openMR(t, twinRepo, e.Person, mrOptions{head: branch, base: "main", title: "copy", body: markerBody(fp)})

	dir := t.TempDir()
	gitCmd(t, dir, nil, nil, "init", "-q", "-b", "master")
	files := map[string]string{
		config.HubFile: fmt.Sprintf("version: 1\nid: %s\nproviders:\n  - id: gl\n    type: gitlab\n    url: %s\n    writer: %s\n",
			id, e.URL, e.Writer.Login),
		config.TargetsFile:     fmt.Sprintf("version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - group: %s\n", fx.ns),
		"packs/base/AGENTS.md": text("base AGENTS.md v1"),
	}
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, nil, nil, "add", "-A")
	gitCmd(t, dir, nil, nil, "commit", "-q", "-m", "the hub")
	gitCmd(t, dir, nil, nil, "remote", "add", "origin", e.remote(hub.PathWithNamespace))

	var outputs []string
	doctor := func(code int, fingerprint string, vars map[string]string, extra ...string) report.Doctor {
		t.Helper()
		for k, v := range vars {
			t.Setenv(k, v)
		}
		args := append([]string{"doctor", "--hub", dir, "--hub-fp", fingerprint, "--format", "json"}, extra...)
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
	writerVars := map[string]string{"TOUCHMARK_GL_WRITE_TOKEN": e.Writer.Token}
	doc := doctor(1, fp, writerVars)
	target := func(name string) report.DoctorTarget {
		t.Helper()
		for _, tg := range doc.Targets {
			if tg.Path == projects[name].PathWithNamespace {
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
	want("provider", pc, "2fa", report.StatusOK, "bot or service account")
	want("provider", pc, "scopes", report.StatusOK, "api")
	want("provider", pc, "hub-hidden", report.StatusOK, "does not see the hub")
	expiry := check("provider", pc, "token-expiry")
	if expiry.Status != report.StatusOK && expiry.Status != report.StatusWarn {
		t.Errorf("token-expiry: %s %q", expiry.Status, expiry.Detail)
	}
	finding(t, "doctor-identity", "token-expiry %s %q; scopes %q; 2fa %q", expiry.Status, expiry.Detail,
		check("provider", pc, "scopes").Detail, check("provider", pc, "2fa").Detail)
	want("provider", pc, "signing", report.StatusUnknown, "GitLab push rules")

	want("api", target("api").Checks, "access", report.StatusOK, "Developer")
	want("api", target("api").Checks, "rules", report.StatusOK, "no protected branch covers")
	want("api", target("api").Checks, "markers", report.StatusOK, "no marker")
	want("prot", target("prot").Checks, "rules", report.StatusFail, "blocked:rules:protected-branch")
	want("noforce", target("noforce").Checks, "rules", report.StatusWarn, "without force pushes")
	if tg := target("web"); tg.Skipped != "not-opted-in" {
		t.Errorf("web: %+v", tg)
	}
	markers := check("twin", target("twin").Checks, "markers")
	if markers.Status != report.StatusWarn || !strings.Contains(markers.Detail, other) ||
		!strings.Contains(markers.Detail, fmt.Sprintf("!%d by %s", copyMR.IID, e.Person.Login)) {
		t.Errorf("twin markers: %s %q", markers.Status, markers.Detail)
	}
	finding(t, "doctor-rules", "prot %q; noforce %q", check("prot", target("prot").Checks, "rules").Detail,
		check("noforce", target("noforce").Checks, "rules").Detail)

	// A hub in the writer's subgroup: the writer may push there.
	inside := fx.createProject(t, fx.nsID, "hub-inside", readme)
	e.waitAccess(t, inside.ID, e.Writer)
	gitCmd(t, dir, nil, nil, "remote", "set-url", "origin", e.remote(inside.PathWithNamespace))
	doc = doctor(1, fmt.Sprintf("%s/%d", e.Host, inside.ID), writerVars)
	want("provider", doc.Providers[0].Checks, "hub-hidden", report.StatusFail, "may push to the hub")
	gitCmd(t, dir, nil, nil, "remote", "set-url", "origin", e.remote(hub.PathWithNamespace))

	// --hub-token: where the hub keeps its write keys, as root reads it.
	secret := func() string { return "k" + randHex(t, 12) }
	for _, v := range []map[string]any{
		{"key": "TOUCHMARK_GL_WRITE_TOKEN", "value": secret(), "protected": true, "masked": true, "environment_scope": "touchmark-distribute"},
		{"key": "TOUCHMARK_X_WRITE_TOKEN", "value": secret(), "protected": false, "masked": true, "environment_scope": "touchmark-distribute"},
	} {
		root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/variables", hub.ID), v, nil)
	}
	root.ok(t, http.MethodPost, fmt.Sprintf("/projects/%d/protected_tags", hub.ID), map[string]any{"name": "v*", "create_access_level": levelDeveloper}, nil)
	doc = doctor(1, fp, map[string]string{"TOUCHMARK_GL_WRITE_TOKEN": e.Writer.Token, "TOUCHMARK_HUB_TOKEN": e.Root.Token}, "--hub-token")
	if !doc.HubToken {
		t.Error("hub_token is not set")
	}
	var lines []string
	for _, c := range doc.HubChecks {
		lines = append(lines, c.Name+" "+string(c.Status)+": "+c.Detail)
	}
	for _, w := range []string{
		"key-location ok: project variable TOUCHMARK_GL_WRITE_TOKEN is protected and scoped",
		"key-location fail: project variable TOUCHMARK_X_WRITE_TOKEN is not protected",
		"protected-tags fail: protected tag v* may be created by Developers",
	} {
		found := false
		for _, l := range lines {
			if strings.HasPrefix(l, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("no hub check %q in:\n%s", w, strings.Join(lines, "\n"))
		}
	}
	finding(t, "doctor-hub-token", "%s", strings.Join(lines, " | "))

	for i, out := range outputs {
		for _, a := range []account{e.Root, e.Reader, e.Writer, e.Person} {
			if strings.Contains(out, a.Token) {
				t.Errorf("output %d holds the token of %s", i, a.Login)
			}
		}
	}
}
