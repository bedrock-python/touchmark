package cli

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/schemas"
)

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: nil, code: exitUsage, stderr: "Usage: touchmark <command>"},
		{args: []string{"--help"}, code: exitOK, stdout: "Commands:"},
		{args: []string{"help"}, code: exitOK, stdout: "Commands:"},
		{args: []string{"frobnicate"}, code: exitUsage, stderr: `unknown command "frobnicate"`},
		{args: []string{"status", "--help"}, code: exitOK, stdout: "--packs PACKS"},
		{args: []string{"apply", "-h"}, code: exitOK, stdout: "--adopt GLOB"},
		{args: []string{"status", "--bogus"}, code: exitUsage, stderr: "flag provided but not defined: -bogus"},
		{args: []string{"status", "extra"}, code: exitUsage, stderr: `unexpected argument "extra"`},
		{args: []string{"status", "--format", "yaml"}, code: exitUsage, stderr: `"yaml" is not text or json`},
		{args: []string{"status"}, code: exitUsage, stderr: "no hub: pass --hub DIR or set TOUCHMARK_HUB"},
		{args: []string{"check", "--hub", t.TempDir()}, code: exitUsage, stderr: "is not a git work tree"},
		{args: []string{"schema"}, code: exitUsage, stderr: "missing schema name"},
		{args: []string{"schema", "nope"}, code: exitUsage, stderr: `unknown schema "nope": want hub, targets, opt-in`},
		{args: []string{"version", "x"}, code: exitUsage, stderr: `unexpected argument "x"`},
	} {
		res := run(t, tc.args...)
		if res.code != tc.code || !strings.Contains(res.stdout, tc.stdout) || !strings.Contains(res.stderr, tc.stderr) {
			t.Errorf("%v: exit %d\nstdout: %s\nstderr: %s\nwant exit %d, stdout with %q, stderr with %q",
				tc.args, res.code, res.stdout, res.stderr, tc.code, tc.stdout, tc.stderr)
		}
	}
}

func TestVersion(t *testing.T) {
	res := run(t, "version")
	want := "touchmark " + Version + " (" + runtime.Version()
	if res.code != exitOK || !strings.HasPrefix(res.stdout, want) {
		t.Errorf("version: exit %d, %q; want %q...", res.code, res.stdout, want)
	}
}

// TestVersionStamp: the commit and date a release build sets with -ldflags
// (.goreleaser.yaml) appear in the version line.
func TestVersionStamp(t *testing.T) {
	saved := [3]string{Version, Commit, Date}
	t.Cleanup(func() { Version, Commit, Date = saved[0], saved[1], saved[2] })
	Version, Commit, Date = "v0.1.0", "0123456789abcdef0123456789abcdef01234567", "2026-10-01T12:00:00Z"
	res := run(t, "version")
	want := "touchmark v0.1.0 (" + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH +
		", commit 0123456789abcdef0123456789abcdef01234567, 2026-10-01T12:00:00Z)\n"
	if res.code != exitOK || res.stdout != want {
		t.Errorf("version: exit %d, %q; want %q", res.code, res.stdout, want)
	}
}

func TestSchema(t *testing.T) {
	for _, name := range schemas.Names() {
		res := run(t, "schema", name)
		want, _ := schemas.Get(name)
		if res.code != exitOK || strings.TrimSpace(res.stdout) != strings.TrimSpace(string(want)) {
			t.Errorf("schema %s: exit %d, output differs from the embedded schema", name, res.code)
		}
		if !json.Valid([]byte(res.stdout)) {
			t.Errorf("schema %s: not valid JSON", name)
		}
	}
}

// $TOUCHMARK_HUB names the hub, and --dir defaults to the current directory.
func TestDefaultsFromEnvironment(t *testing.T) {
	h := baseHub(t)
	tg := optedIn(t, optInBase)
	t.Setenv(hubEnv, h.dir)
	tg.put("sub/dir/README.md", "the target's own\n")
	t.Chdir(tg.abs("sub/dir"))
	res := run(t, "status", "--format", "json", "--repo", "acme/svc")
	if res.code != exitOK {
		t.Fatalf("status: exit %d: %s", res.code, res.stderr)
	}
	rep := decodeSync(t, res.stdout)
	if rep.Summary.Missing != 3 {
		t.Errorf("summary %+v, want 3 missing", rep.Summary)
	}
}

func TestParseRemote(t *testing.T) {
	for _, tc := range []struct {
		remote, host, path string
	}{
		{"https://github.com/acme/svc.git", "github.com", "acme/svc"},
		{"https://github.com/acme/svc", "github.com", "acme/svc"},
		{"https://x-access-token:secret@GitHub.com/acme/svc/", "github.com", "acme/svc"},
		{"http://localhost:3000/acme/svc.git", "localhost", "acme/svc"},
		{"ssh://git@gitlab.example.com:2222/group/sub/project.git", "gitlab.example.com", "group/sub/project"},
		{"git@github.com:acme/svc.git", "github.com", "acme/svc"},
		{"gitlab.example.com:group/project", "gitlab.example.com", "group/project"},
		{"git://example.com/acme/svc.git", "example.com", "acme/svc"},
		{"file:///srv/git/acme/svc.git", "", ""},
		{"/srv/git/acme/svc.git", "", ""},
		{"../svc", "", ""},
		{`C:\src\svc`, "", ""},
		{"C:/src/svc", "", ""},
		{"https://github.com/", "", ""},
	} {
		host, path, ok := parseRemote(tc.remote)
		if ok != (tc.host != "") || host != tc.host || path != tc.path {
			t.Errorf("parseRemote(%q) = %q, %q, %v; want %q, %q", tc.remote, host, path, ok, tc.host, tc.path)
		}
	}
}

func TestRemoteProvider(t *testing.T) {
	hub := &config.Hub{Providers: []config.Provider{
		{ID: "gh", URL: "https://github.com"},
		{ID: "corp", URL: "https://gitlab.example.com:8443"},
	}}
	for _, tc := range []struct {
		host     string
		defaults string
		want     string
	}{
		{"github.com", "", "gh"},
		{"GITLAB.example.com", "gh", "corp"},
		{"other.example.com", "gh", "gh"},
		{"other.example.com", "", ""},
	} {
		targets := &config.Targets{Defaults: config.Defaults{Provider: tc.defaults}}
		if got := remoteProvider(tc.host, hub, targets); got != tc.want {
			t.Errorf("remoteProvider(%q, defaults %q) = %q, want %q", tc.host, tc.defaults, got, tc.want)
		}
	}
	single := &config.Hub{Providers: []config.Provider{{ID: "only", URL: "https://gitea.example.com"}}}
	if got := remoteProvider("github.com", single, nil); got != "only" {
		t.Errorf("single provider: got %q, want only", got)
	}
}
