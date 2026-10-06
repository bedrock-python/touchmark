package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInitTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	s.chain("main", "", 1)
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "a", "target")
	tr, err := InitTarget(t.Context(), dir, s.dir, Auth{}, Isolation{Home: home, AllowFile: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Dir != dir || tr.Remote != s.dir || tr.Git.Dir != dir {
		t.Errorf("TargetRepo = %+v", tr)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"repositoryformatversion = 1", "bare = true", "partialClone = origin",
		"promisor = true", "partialclonefilter = blob:none", "[remote \"origin\"]",
	} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config lacks %q:\n%s", want, config)
		}
	}
	if strings.Contains(string(config), "fetch =") {
		t.Errorf("config has a fetch refspec:\n%s", config)
	}
	// No template files: no sample hooks, no info/exclude, no description.
	for _, name := range []string{"hooks", "info", "description"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists: %v", name, err)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"gitconfig", "hooks"}) {
		t.Errorf("home holds %v", names)
	}
	partial, err := tr.Git.IsPartialClone(t.Context())
	if err != nil || !partial {
		t.Errorf("IsPartialClone() = %v, %v", partial, err)
	}

	// A second target shares the home.
	if _, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "b"), s.dir, Auth{}, Isolation{Home: home, AllowFile: true}); err != nil {
		t.Errorf("second target with the same home: %v", err)
	}
}

func TestInitTargetDirAndHome(t *testing.T) {
	t.Parallel()
	s := newServed(t, t.TempDir(), "repo.git")
	iso := func() Isolation { return Isolation{Home: t.TempDir(), AllowFile: true} }

	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InitTarget(t.Context(), full, s.dir, Auth{}, iso()); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("InitTarget(non-empty dir) = %v", err)
	}

	for name, prepare := range map[string]func(home string) error{
		"stray file": func(home string) error {
			return os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n"), 0o644)
		},
		"config": func(home string) error {
			return os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[core]\n"), 0o644)
		},
		"hook": func(home string) error {
			if err := os.Mkdir(filepath.Join(home, "hooks"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(home, "hooks", "pre-push"), nil, 0o755)
		},
		"attributes": func(home string) error {
			if err := os.MkdirAll(filepath.Join(home, "git"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(home, "git", "attributes"), []byte("* text\n"), 0o644)
		},
	} {
		is := iso()
		if err := prepare(is.Home); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "t")
		if _, err := InitTarget(t.Context(), dir, s.dir, Auth{}, is); err == nil {
			t.Errorf("%s: InitTarget accepted a home that is not empty", name)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the target directory was created: %v", name, err)
		}
	}

	if _, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "t"), s.dir, Auth{}, Isolation{AllowFile: true}); err == nil {
		t.Error("InitTarget without Home succeeded")
	}
	if _, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "t"), s.dir, Auth{}, Isolation{Home: t.TempDir(), AllowFile: true, CAFile: filepath.Join(t.TempDir(), "none.pem")}); err == nil {
		t.Error("InitTarget with a missing CA file succeeded")
	}

	// A failure after the directory is made removes what InitTarget made.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, pre := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "t")
		if pre {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := InitTarget(ctx, dir, s.dir, Auth{}, iso()); err == nil {
			t.Fatal("InitTarget with a cancelled context succeeded")
		}
		entries, err := os.ReadDir(dir)
		switch {
		case pre && (err != nil || len(entries) > 0):
			t.Errorf("existing dir after a failure: %v, %v", entries, err)
		case !pre && !errors.Is(err, os.ErrNotExist):
			t.Errorf("created dir left after a failure: %v, %v", entries, err)
		}
	}
}

func TestInitTargetRemotes(t *testing.T) {
	t.Parallel()
	local := t.TempDir()
	abs, _ := filepath.Abs(local)
	cases := []struct {
		remote  string
		iso     Isolation
		ok      bool
		private string // must not appear in the error
	}{
		{remote: "https://example.com/acme/api.git", ok: true},
		{remote: "https://example.com:8443/g/p.git", ok: true},
		{remote: "http://127.0.0.1:9/r.git", iso: Isolation{AllowHTTP: true}, ok: true},
		{remote: "http://localhost:9/r.git", iso: Isolation{AllowHTTP: true}, ok: true},
		{remote: "http://[::1]:9/r.git", iso: Isolation{AllowHTTP: true}, ok: true},
		{remote: abs, iso: Isolation{AllowFile: true}, ok: true},
		{remote: fileURL(abs), iso: Isolation{AllowFile: true}, ok: true},

		{remote: ""},
		{remote: "http://127.0.0.1:9/r.git"},
		{remote: "http://example.com/r.git", iso: Isolation{AllowHTTP: true}},
		{remote: "https://user:s3cr3t-t0ken@example.com/r.git", private: "s3cr3t-t0ken"},
		{remote: "https://x-access-token@example.com/r.git", private: "x-access-token"},
		{remote: "https://example.com/r.git?token=s3cr3t-t0ken", private: "s3cr3t-t0ken"},
		{remote: "https://example.com/r.git#frag"},
		{remote: "HTTPS://example.com/r.git"},
		{remote: "https:///r.git"},
		{remote: "ssh://git@example.com/r.git"},
		{remote: "git://example.com/r.git"},
		{remote: "ftp://example.com/r.git"},
		{remote: "git@example.com:acme/api.git", iso: Isolation{AllowFile: true}},
		{remote: "ext::sh -c touch% /tmp/pwned", iso: Isolation{AllowFile: true}},
		{remote: "fd::17", iso: Isolation{AllowFile: true}},
		{remote: "relative/repo.git", iso: Isolation{AllowFile: true}},
		{remote: abs},
		{remote: fileURL(abs)},
		{remote: "file://example.com/r.git", iso: Isolation{AllowFile: true}},
		{remote: "file://localhost/r.git", iso: Isolation{AllowFile: true}},
		{remote: "https://example.com/r.git\n", iso: Isolation{AllowFile: true}},
	}
	for _, c := range cases {
		iso := c.iso
		iso.Home = t.TempDir()
		if c.ok {
			// Accepted remotes: the check alone (a repository costs six git
			// runs; TestInitTarget makes one).
			if _, _, err := checkRemote(c.remote, iso); err != nil {
				t.Errorf("checkRemote(%q, %+v) = %v", c.remote, c.iso, err)
			}
			continue
		}
		dir := filepath.Join(t.TempDir(), "t")
		_, err := InitTarget(t.Context(), dir, c.remote, Auth{}, iso)
		if err == nil {
			t.Errorf("InitTarget(%q, %+v) succeeded", c.remote, c.iso)
			continue
		}
		if c.private != "" && strings.Contains(err.Error(), c.private) {
			t.Errorf("InitTarget(%q) error leaks the secret: %v", c.remote, err)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("InitTarget(%q) left %s", c.remote, dir)
		}
	}
	// One accepted http remote end to end.
	if _, err := InitTarget(t.Context(), filepath.Join(t.TempDir(), "t"), "http://127.0.0.1:9/r.git", Auth{},
		Isolation{Home: t.TempDir(), AllowHTTP: true}); err != nil {
		t.Errorf("InitTarget(loopback http) = %v", err)
	}
}

// TestTargetRepoZero: a TargetRepo InitTarget did not make fails, never
// panics.
func TestTargetRepoZero(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, tr := range []*TargetRepo{nil, {}, {Dir: t.TempDir()}} {
		checks := map[string]error{}
		_, checks["RemoteRefs"] = tr.RemoteRefs(ctx)
		_, _, checks["FetchBranch"] = tr.FetchBranch(ctx, "main", 1)
		checks["DeepenSince"] = tr.DeepenSince(ctx, "main", when)
		checks["FetchBlobs"] = tr.FetchBlobs(ctx, []string{oidA})
		_, checks["ReadBlob"] = tr.ReadBlob(ctx, oidA)
		_, _, checks["FirstParentLog"] = tr.FirstParentLog(ctx, oidA, 1)
		_, checks["Commit"] = tr.Commit(ctx, oidA)
		_, checks["DiffTree"] = tr.DiffTree(ctx, "", oidA)
		_, checks["IsCleanMerge"] = tr.IsCleanMerge(ctx, oidA)
		_, checks["IsAncestor"] = tr.IsAncestor(ctx, oidA, oidB)
		_, checks["Tree"] = tr.Tree(ctx, oidA)
		_, checks["Attrs"] = tr.Attrs(ctx, oidA, []string{"a"}, "text")
		_, checks["Renormalized"] = tr.Renormalized(ctx, oidA, "a", nil)
		_, checks["BuildCommit"] = tr.BuildCommit(ctx, CommitSpec{Parent: oidA, Changes: []Change{{Path: "a"}},
			Author: bot, Committer: bot, When: when, Message: "m"})
		_, checks["Push"] = tr.Push(ctx, PushSpec{Branch: "b", Commit: oidA})
		for name, err := range checks {
			if !errors.Is(err, errNotInitialized) {
				t.Errorf("%+v: %s() = %v, want errNotInitialized", tr, name, err)
			}
		}
	}
	if (*TargetRepo)(nil).WithAuth(Auth{}) != nil {
		t.Error("WithAuth(nil) is not nil")
	}
}

func TestCheckRemoteScope(t *testing.T) {
	t.Parallel()
	for remote, want := range map[string]string{
		"https://GitHub.com/acme/api.git":   "https://github.com/",
		"https://gitlab.example.com:8443/x": "https://gitlab.example.com:8443/",
		"http://127.0.0.1:4242/r.git":       "http://127.0.0.1:4242/",
	} {
		got, _, err := checkRemote(remote, Isolation{AllowHTTP: true})
		if err != nil || got != want {
			t.Errorf("checkRemote(%q) = %q, %v; want %q", remote, got, err, want)
		}
	}
	_, protocols, _ := checkRemote("https://example.com/r", Isolation{})
	if !slices.Equal(protocols, []string{"https"}) {
		t.Errorf("protocols = %v", protocols)
	}
}

// TestTargetIsolation: inherited git variables are dropped, every command
// carries the -c options and the isolation environment, and only the
// repository's own config is read.
func TestTargetIsolation(t *testing.T) {
	// Setenv: not parallel.
	root := t.TempDir()
	s := newServed(t, root, "repo.git")
	head := s.chain("main", "", 3)[2]
	other := newServed(t, root, "other.git")

	evilHooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\necho ran > '" + filepath.ToSlash(marker) + "'\n"
	for _, name := range []string{"reference-transaction", "post-checkout", "pre-push"} {
		if err := os.WriteFile(filepath.Join(evilHooks, name), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	systemConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(systemConfig, []byte("[core]\n\thooksPath = "+filepath.ToSlash(evilHooks)+"\n[url \""+filepath.ToSlash(other.dir)+"\"]\n\tinsteadOf = "+filepath.ToSlash(s.dir)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", other.dir)
	t.Setenv("GIT_CONFIG_GLOBAL", systemConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", systemConfig)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hookspath'='"+filepath.ToSlash(evilHooks)+"'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.ToSlash(evilHooks))
	t.Setenv("GIT_ASKPASS", "false")
	t.Setenv("SSH_ASKPASS", "false")
	t.Setenv("GIT_TRACE", filepath.Join(t.TempDir(), "trace"))
	// Credentials of touchmark and of the CI never reach a target's git;
	// what git needs to reach the network does.
	secrets := map[string]string{
		"GITHUB_TOKEN":                   "ghs_isolation-canary-1",
		"CI_JOB_TOKEN":                   "isolation-canary-2",
		"TOUCHMARK_ACME_WRITE_TOKEN":     "isolation-canary-3",
		"TOUCHMARK_WRITE_APP_KEY":        "isolation-canary-4",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "isolation-canary-5",
		"GITEA_TOKEN":                    "isolation-canary-6",
		"SOME_OTHER_SECRET":              "isolation-canary-7",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("no_proxy", "localhost")

	tr := newTarget(t, s.dir, Auth{})
	var argvs, envs [][]string
	tr.iso.trace = func(args, env []string) {
		argvs = append(argvs, args)
		envs = append(envs, env)
	}
	sha, ok, err := tr.FetchBranch(t.Context(), "main", 5)
	if err != nil || !ok || sha != head {
		t.Fatalf("FetchBranch() = %s, %v, %v; want %s", sha, ok, err, head)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a client-side hook ran: %v", err)
	}
	if len(argvs) == 0 {
		t.Fatal("no command traced")
	}
	for i, argv := range argvs {
		for _, want := range []string{"core.hooksPath=" + filepath.ToSlash(tr.iso.hooks), "protocol.allow=never",
			"credential.helper=", "transfer.fsckObjects=true", "http.followRedirects=false", "gc.auto=0",
			"maintenance.auto=false", "submodule.recurse=false", "core.fsmonitor=false", "protocol.file.allow=always"} {
			if !slices.Contains(argv, want) {
				t.Errorf("command %v lacks -c %s", argv, want)
			}
		}
		env := envs[i]
		for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_ATTR_NOSYSTEM=1", "GIT_ASKPASS=", "HOME=" + tr.iso.home, "GIT_CONFIG_GLOBAL=" + tr.iso.global,
			"GIT_DIR=" + tr.Dir} {
			if !slices.Contains(env, want) {
				t.Errorf("command %v: environment lacks %s", argv, want)
			}
		}
		for _, kv := range env {
			switch {
			case strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS="), strings.HasPrefix(kv, "GIT_TRACE="),
				strings.HasPrefix(kv, "GIT_CONFIG_SYSTEM="), kv == "GIT_DIR="+other.dir,
				kv == "GIT_CONFIG_GLOBAL="+systemConfig, kv == "GIT_ASKPASS=false", strings.HasPrefix(kv, "SSH_ASKPASS="):
				t.Errorf("command %v inherits %s", argv, kv)
			}
			for name, value := range secrets {
				if strings.Contains(kv, value) {
					t.Errorf("command %v inherits %s", argv, name)
				}
			}
		}
		for _, want := range []string{"HTTPS_PROXY=http://proxy.invalid:3128", "no_proxy=localhost"} {
			if !slices.Contains(env, want) {
				t.Errorf("command %v: environment lacks %s", argv, want)
			}
		}
		if !slices.ContainsFunc(env, func(kv string) bool { return strings.EqualFold(envName(kv), "PATH") }) {
			t.Errorf("command %v: environment lacks PATH", argv)
		}
	}
	// Only the command line and the repository's own config are read.
	out, err := tr.Git.Run(t.Context(), nil, "config", "--list", "--show-scope")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		scope, _, _ := strings.Cut(line, "\t")
		if scope != "command" && scope != "local" {
			t.Errorf("config from scope %q: %s", scope, line)
		}
		if strings.Contains(line, filepath.ToSlash(evilHooks)) {
			t.Errorf("config holds the inherited hooks path: %s", line)
		}
	}
}
