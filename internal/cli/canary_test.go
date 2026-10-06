package cli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// The canaries: a distinctive write token must never appear in what
// distribute prints or writes (stdout, stderr, the JSON report, the report
// stream, golden files), in what it writes to the platform (pull request
// titles, bodies, labels and comments, branch names, commit messages), in the
// argv or the environment of any git process but as the extraHeader of
// GIT_CONFIG_VALUE_n, or in a target repository's config.

// canaryTracker watches every git process of the test binary and the texts
// a test hands it for the forms of its secrets.
type canaryTracker struct {
	t     *testing.T
	forms []string
	stop  func()
	// skip are directories whose git processes are the fake platform's,
	// not touchmark's.
	skip []string

	mu       sync.Mutex
	commands int
	leaks    []string
}

// newCanaryTracker starts watching git processes for secrets; stop ends
// it (the test's cleanup does too).
func newCanaryTracker(t *testing.T, secrets ...string) *canaryTracker {
	t.Helper()
	c := &canaryTracker{t: t}
	for _, s := range secrets {
		c.forms = append(c.forms, secretForms(s)...)
	}
	c.stop = gitx.TraceCommands(c.command)
	t.Cleanup(c.check)
	return c
}

// secretForms returns what must never be printed or passed for secret: the
// secret itself and, for each of the three alignments it can have in a
// longer string, the part of its standard and URL base64 that does not
// depend on the bytes around it (so any base64 of "user:secret", of
// "ro.secret", or of a whole header holding it, contains one of them).
func secretForms(secret string) []string {
	forms := []string{secret}
	for pad := range 3 {
		n := pad + len(secret)
		full := n / 3 * 4
		start := 0
		if pad > 0 {
			start = 4
		}
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
			s := enc.EncodeToString([]byte(strings.Repeat("\x00", pad) + secret))
			if full-start >= 8 {
				forms = append(forms, s[start:full])
			}
		}
	}
	return forms
}

// leak returns the first form of a secret that s holds, "" for none.
func (c *canaryTracker) leak(s string) string {
	for _, f := range c.forms {
		if strings.Contains(s, f) {
			return f
		}
	}
	return ""
}

// text records a leak when s, which what names, holds a secret.
func (c *canaryTracker) text(what, s string) {
	c.t.Helper()
	if f := c.leak(s); f != "" {
		c.t.Errorf("the token (as %q) is in %s", f, what)
	}
}

// command checks the argv and environment of a git process about to start,
// and the config of the repository it runs in.
func (c *canaryTracker) command(args, env []string) {
	if c.skipped(args, env) {
		return
	}
	c.mu.Lock()
	c.commands++
	c.mu.Unlock()
	var leaks []string
	for _, a := range args {
		if c.leak(a) != "" {
			leaks = append(leaks, fmt.Sprintf("argv of %v", args))
		}
	}
	dirs := []string{}
	for i, a := range args {
		if a == "-C" && i+1 < len(args) {
			dirs = append(dirs, args[i+1])
		}
	}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if name == "GIT_DIR" {
			dirs = append(dirs, value)
		}
		if c.leak(kv) != "" && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			leaks = append(leaks, fmt.Sprintf("the environment (%s) of %v", name, args))
		}
	}
	for _, dir := range dirs {
		for _, name := range []string{filepath.Join(dir, "config"), filepath.Join(dir, ".git", "config")} {
			if data, err := os.ReadFile(name); err == nil && c.leak(string(data)) != "" {
				leaks = append(leaks, "the config "+name)
			}
		}
	}
	if len(leaks) > 0 {
		c.mu.Lock()
		c.leaks = append(c.leaks, leaks...)
		c.mu.Unlock()
	}
}

// skipped reports whether a git process is the fake platform's: it runs in
// a directory of skip, or with a global config there.
func (c *canaryTracker) skipped(args, env []string) bool {
	under := func(p string) bool {
		p = filepath.ToSlash(p)
		return slices.ContainsFunc(c.skip, func(d string) bool { return strings.HasPrefix(p, filepath.ToSlash(d)) })
	}
	for i, a := range args {
		if a == "-C" && i+1 < len(args) && under(args[i+1]) {
			return true
		}
	}
	return slices.ContainsFunc(env, func(kv string) bool {
		v, ok := strings.CutPrefix(kv, "GIT_CONFIG_GLOBAL=")
		return ok && under(v)
	})
}

// watched returns how many git processes of touchmark's the tracker saw.
func (c *canaryTracker) watched() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commands
}

// check reports the leaks the git processes showed.
func (c *canaryTracker) check() {
	c.stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	slices.Sort(c.leaks)
	for _, l := range slices.Compact(c.leaks) {
		c.t.Errorf("the token reached %s", l)
	}
}

// The canaries of a maintainer's local distribute: the write token is in
// the process environment, as CI jobs have it; the platform's messages
// quote it (an API error of a pull request, a push refused by a hook that
// prints it); distribute runs with a report, a stream, in text and in JSON.
func TestCanaries(t *testing.T) {
	needDistributeGit(t)
	h := distHub(t)
	w := newDistWorld(t)
	const canary = distWriteToken
	w.install()
	c := newCanaryTracker(t, canary)
	c.skip = []string{filepath.Dir(w.p.GitDir(w.repos["acme/api"].ID))}
	t.Setenv("TOUCHMARK_GH_WRITE_TOKEN", canary)
	vars := map[string]string{"TOUCHMARK_GH_WRITE_TOKEN": canary}
	// The platform quotes the token: in an API error, and in the message of
	// a hook that refuses a push.
	w.p.FailNext("CreatePR", &platform.Error{Op: "create pull request", Class: platform.ClassInvalid, Status: http.StatusUnprocessableEntity,
		Err: errors.New("validation failed for token " + canary)})
	hook := "#!/bin/sh\necho \"the credential " + canary + " may not push here\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(w.p.GitDir(w.repos["acme/api"].ID), "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	reportFile, stream := filepath.Join(dir, "report.json"), filepath.Join(dir, "stream.jsonl")
	var outputs []string
	for _, format := range []string{"json", "text", "markdown"} {
		// distribute takes the token out of the process environment once it
		// read it: every run starts as a CI job does, with it there.
		if err := os.Setenv("TOUCHMARK_GH_WRITE_TOKEN", canary); err != nil {
			t.Fatal(err)
		}
		res := runWith(t, vars, "distribute", "--hub", h.dir, "--hub-fp", distFP, "--format", format, "--report", reportFile, "--stream", stream)
		if res.code != exitFailed && res.code != exitOK {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
		}
		outputs = append(outputs, res.stdout)
		c.text(format+" stdout", res.stdout)
		c.text(format+" stderr", res.stderr)
		for _, name := range []string{reportFile, stream} {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			c.text(filepath.Base(name)+" of the "+format+" run", string(data))
		}
	}
	// The faults did reach the output, masked.
	if !strings.Contains(outputs[0], "validation failed for token ***") || !strings.Contains(outputs[0], "may not push here") {
		t.Errorf("the platform's messages are not in the report:\n%s", outputs[0])
	}
	if c.watched() == 0 {
		t.Error("no git process was watched")
	}
	for _, r := range w.repos {
		for _, pr := range w.p.PRList(r.ID) {
			c.text(fmt.Sprintf("%s #%d", r.Path, pr.Number), pr.Title+"\n"+pr.Body+"\n"+pr.Head+"\n"+strings.Join(pr.Labels, "\n"))
			for _, cm := range w.p.Comments(r.ID, pr.Number) {
				c.text("a comment", cm.Body)
			}
		}
		out, err := gitx.New(w.p.GitDir(r.ID)).Run(t.Context(), nil, "log", "--all", "--format=%H%n%B")
		if err != nil {
			t.Fatal(err)
		}
		c.text("the commits of "+r.Path, string(out))
	}
	// Golden files hold no token, whatever -update wrote.
	err := filepath.WalkDir(goldenDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err == nil {
			c.text(p, string(data))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
