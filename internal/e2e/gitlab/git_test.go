//go:build e2e

package gitlabe2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform/conformance"
)

// gitCmd runs git in dir with the test process's environment (isolated by
// TestMain) plus env, and returns its standard output. stdin may be nil.
func gitCmd(t testing.TB, dir string, stdin []byte, env []string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, stdin, env, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// gitTry is gitCmd for a command that may fail: it returns the failure,
// with git's standard error masked, instead of failing the test.
func gitTry(dir string, env []string, args ...string) (string, error) {
	return gitRun(dir, nil, env, args...)
}

// gitRun runs git as gitCmd describes; its error holds the arguments and
// git's standard error, masked.
func gitRun(dir string, stdin []byte, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if live != nil {
			msg = live.Redact.Replace(msg)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitAuth is the environment that gives git the Authorization header of as
// for GitLab: Basic oauth2:<token>, through GIT_CONFIG_* variables, as
// touchmark itself does, never in a URL or an argument.
func (e *liveEnv) gitAuth(as account) []string {
	return e.basicAuth("oauth2", as.Token)
}

// basicAuth is gitAuth with Basic credentials of any user name and
// password.
func (e *liveEnv) basicAuth(user, password string) []string {
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + e.URL + "/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// remote is the git URL of the project at path, without credentials.
func (e *liveEnv) remote(path string) string { return e.URL + "/" + path + ".git" }

// identity is the environment that makes as the author and committer of a
// commit git makes, with the email GitLab gives the account.
func identity(as account, email string) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + as.Login, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + as.Login, "GIT_COMMITTER_EMAIL=" + email,
	}
}

// pushBranch points branch to of the project at path at the tip of branch
// from, with a push as as (forced: to may be anywhere).
func (e *liveEnv) pushBranch(t testing.TB, as account, path, from, to string) string {
	t.Helper()
	dir := t.TempDir()
	auth := e.gitAuth(as)
	gitCmd(t, dir, nil, nil, "init", "-q")
	gitCmd(t, dir, nil, auth, "fetch", "-q", e.remote(path), "refs/heads/"+from)
	tip := gitCmd(t, dir, nil, nil, "rev-parse", "FETCH_HEAD")
	gitCmd(t, dir, nil, auth, "push", "-q", "-f", e.remote(path), tip+":refs/heads/"+to)
	return tip
}

// pushFiles commits files with any mode (executables, symlinks and
// submodules, which the commits API cannot write) on top of the tip of
// branch, or as the branch's first commit when it does not exist, and
// pushes the commit as as. It returns the commit id.
func (e *liveEnv) pushFiles(t testing.TB, as account, path, branch string, files []conformance.File, message string) string {
	t.Helper()
	dir := t.TempDir()
	auth := e.gitAuth(as)
	gitCmd(t, dir, nil, nil, "init", "-q")
	ref := "refs/heads/" + branch
	parent := ""
	if out := gitCmd(t, dir, nil, auth, "ls-remote", e.remote(path), ref); out != "" {
		parent, _, _ = strings.Cut(out, "\t")
		gitCmd(t, dir, nil, auth, "fetch", "-q", "--depth=1", e.remote(path), ref)
		gitCmd(t, dir, nil, nil, "read-tree", parent)
	}
	for _, f := range files {
		mode, oid := f.Mode, ""
		switch mode {
		case "160000":
			oid = string(f.Content) // a submodule entry names a commit
		case "":
			mode = "100644"
			fallthrough
		default:
			oid = gitCmd(t, dir, f.Content, nil, "hash-object", "-w", "--stdin")
		}
		gitCmd(t, dir, nil, nil, "update-index", "--add", "--cacheinfo", mode+","+oid+","+f.Path)
	}
	tree := gitCmd(t, dir, nil, nil, "write-tree")
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit := gitCmd(t, dir, nil, identity(as, as.Login+"@example.com"), args...)
	gitCmd(t, dir, nil, auth, "push", "-q", e.remote(path), commit+":"+ref)
	return commit
}
