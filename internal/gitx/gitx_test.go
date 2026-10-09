package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    [3]int
		wantErr bool
	}{
		{in: "git version 2.33.0.windows.2\n", want: [3]int{2, 33, 0}},
		{in: "git version 2.45.1 (Apple Git-154)", want: [3]int{2, 45, 1}},
		{in: "git version 2.39.5", want: [3]int{2, 39, 5}},
		{in: "git version 2.47.0.rc1", want: [3]int{2, 47, 0}},
		{in: "git version 2.40", want: [3]int{2, 40, 0}},
		{in: "git version 2.46.0-rc0", want: [3]int{2, 46, 0}},
		{in: "", wantErr: true},
		{in: "hello", wantErr: true},
		{in: "git version ", wantErr: true},
		{in: "git version 2", wantErr: true},
		{in: "git version x.y.z", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseVersion(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseVersion(%q) error = %v, want error %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseVersion(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	g := &Git{Env: testEnv(t)}
	v, err := g.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v[0] < 2 {
		t.Fatalf("Version() = %v", v)
	}
	if err := g.CheckVersion(t.Context(), MinVersion); err != nil {
		t.Errorf("CheckVersion(MinVersion): %v", err)
	}
	err = g.CheckVersion(t.Context(), [3]int{99, 1, 2})
	if err == nil || !strings.Contains(err.Error(), "99.1.2") {
		t.Errorf("CheckVersion(99.1.2) = %v, want an error naming 99.1.2", err)
	}
}

func TestRunError(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	_, err := r.g.Run(t.Context(), nil, "rev-parse", "--verify", "no-such-rev")
	var gitErr *Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if gitErr.Code != 128 {
		t.Errorf("Code = %d, want 128", gitErr.Code)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "git rev-parse --verify no-such-rev: exit 128: fatal:") {
		t.Errorf("Error() = %q", msg)
	}
	for _, kv := range r.g.Env {
		if strings.Contains(msg, kv) {
			t.Errorf("Error() leaks environment %q", kv)
		}
	}
}

func TestRunStdinAndEnv(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	out, err := r.g.Run(t.Context(), strings.NewReader("hello\n"), "hash-object", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("hash-object --stdin = %q", got)
	}
	// Env comes after the defaults, so it can override them.
	g := *r.g
	g.Env = append(g.Env[:len(g.Env):len(g.Env)], "GIT_AUTHOR_NAME=override")
	out, err = g.Run(t.Context(), nil, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "override <") {
		t.Errorf("GIT_AUTHOR_IDENT = %q", out)
	}
}

func TestRunIgnoresInheritedEnv(t *testing.T) {
	r := newTestRepo(t, false)
	r.write("packs/p/a.txt", "a\n")
	r.commit("one")
	want := r.treeBlobs("packs")
	other := newTestRepo(t, false)
	t.Setenv("GIT_DIR", filepath.Join(other.dir, ".git"))
	t.Setenv("GIT_WORK_TREE", other.dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other.dir, ".git", "index"))
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	top, ok, err := r.g.TopLevel(t.Context())
	if err != nil || !ok {
		t.Fatalf("TopLevel() = %q, %v, %v", top, ok, err)
	}
	assertSameDir(t, top, r.dir)
	diffSets(t, r.history("HEAD", "packs"), want)
}

// TestRunInherit: Inherit replaces the process environment git starts from;
// droppedEnv, the defaults and Env still apply on top.
func TestRunInherit(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	g := *r.g
	g.Inherit = func() []string {
		return []string{"GIT_AUTHOR_NAME=inherited", "GIT_DIR=/nowhere", "LC_ALL=xx"}
	}
	base := g.environ()
	for _, kv := range []string{"GIT_AUTHOR_NAME=inherited", "LC_ALL=C"} {
		if !slices.Contains(base, kv) {
			t.Errorf("environ %q lacks %q", base, kv)
		}
	}
	if slices.Contains(base, "GIT_DIR=/nowhere") || slices.ContainsFunc(base, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Errorf("environ %q holds a dropped or process variable", base)
	}
	if got := slices.Index(base, "LC_ALL=C"); got < slices.Index(base, "LC_ALL=xx") {
		t.Errorf("the defaults do not follow the inherited environment: %q", base)
	}
}

// TestRunDropsInheritedConfig: config the environment carries
// (GIT_CONFIG_COUNT with its KEY_n/VALUE_n, GIT_CONFIG_PARAMETERS,
// GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM) never reaches git, while the same
// variables set through Env do. Each variable is first shown to work on this
// git, so a pass is not a git that ignores it.
func TestRunDropsInheritedConfig(t *testing.T) {
	r := newTestRepo(t, false)
	home := t.TempDir()
	evil := filepath.Join(t.TempDir(), "evil.gitconfig")
	if err := os.WriteFile(evil, []byte("[core]\n\tfsmonitor = evil-global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"GIT_CONFIG_COUNT", []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=evil-count"}, "evil-count"},
		{"GIT_CONFIG_PARAMETERS", []string{"GIT_CONFIG_PARAMETERS='core.fsmonitor'='evil-parameters'"}, "evil-parameters"},
		{"GIT_CONFIG_GLOBAL", []string{"GIT_CONFIG_GLOBAL=" + evil}, "evil-global"},
		{"GIT_CONFIG_SYSTEM", []string{"GIT_CONFIG_SYSTEM=" + evil}, "evil-global"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The control: git itself reads the variable.
			cmd := exec.Command("git", "-C", r.dir, "config", "--get", "core.fsmonitor")
			cmd.Env = append(os.Environ(), append([]string{"HOME=" + home, "XDG_CONFIG_HOME=" + home}, tc.env...)...)
			if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("plain git reads core.fsmonitor = %q, %v; want %q", out, err, tc.want)
			}
			// Through Git, inherited: dropped.
			for _, kv := range tc.env {
				name, value, _ := strings.Cut(kv, "=")
				t.Setenv(name, value)
			}
			g := &Git{Dir: r.dir, Env: []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home}}
			out, err := g.Run(t.Context(), nil, "config", "--get", "core.fsmonitor")
			var gitErr *Error
			if !errors.As(err, &gitErr) || gitErr.Code != 1 {
				t.Errorf("Git inherits %s: core.fsmonitor = %q, %v", tc.name, out, err)
			}
			// Through Env: kept, as touchmark's own isolation needs.
			g.Env = append(g.Env, tc.env...)
			out, err = g.Run(t.Context(), nil, "config", "--get", "core.fsmonitor")
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Errorf("Env %s: core.fsmonitor = %q, %v; want %q", tc.name, out, err, tc.want)
			}
		})
	}
}

// TestDroppedEnv: which inherited variables git gets. Names compare
// case-insensitively; prefixes drop whole families.
func TestDroppedEnv(t *testing.T) {
	t.Parallel()
	dropped := []string{
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=x",
		"git_config_key_12=x", "GIT_CONFIG_PARAMETERS='a.b'='c'", "GIT_CONFIG_GLOBAL=/x",
		"GIT_CONFIG_SYSTEM=/x", "GIT_CONFIG=/x", "GIT_EXEC_PATH=/x", "GIT_SSH=/x",
		"GIT_SSH_COMMAND=x", "GIT_ASKPASS=x", "SSH_ASKPASS=x", "GIT_PROXY_COMMAND=x",
		"GIT_EXTERNAL_DIFF=x", "GIT_PAGER=x", "PAGER=x", "EDITOR=x", "GIT_EDITOR=x",
		"VISUAL=x", "GIT_TRACE=/x", "GIT_TRACE2_EVENT=/x", "GIT_TRACE_PACKET=1",
		"GIT_CURL_VERBOSE=1", "GIT_TEMPLATE_DIR=/x", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/x",
		"GIT_OBJECT_DIRECTORY=/x", "GIT_DIR=/x", "GIT_WORK_TREE=/x", "GIT_INDEX_FILE=/x",
		"GIT_NAMESPACE=x", "GIT_ATTR_SOURCE=HEAD", "LD_PRELOAD=/x.so", "LD_LIBRARY_PATH=/x",
		"LD_AUDIT=/x.so", "DYLD_INSERT_LIBRARIES=/x.dylib", "DYLD_LIBRARY_PATH=/x",
		"TOUCHMARK_GITHUB_TOKEN=x",
	}
	kept := []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES=/tmp",
		"GIT_AUTHOR_NAME=a", "HOME=/home/a", "PATH=/bin", "HTTPS_PROXY=http://proxy",
		"LDFLAGS=-s", "GIT_SSL_CAINFO=/ca.pem",
	}
	for _, kv := range dropped {
		if !isDroppedEnv(kv) {
			t.Errorf("%q is inherited", kv)
		}
	}
	for _, kv := range kept {
		if isDroppedEnv(kv) {
			t.Errorf("%q is dropped", kv)
		}
	}
	g := &Git{
		Inherit: func() []string { return slices.Concat(dropped, kept) },
		Env:     []string{"GIT_CONFIG_GLOBAL=/own", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"},
	}
	env := g.environ()
	want := slices.Concat(kept, defaultEnv, g.Env)
	if !slices.Equal(env, want) {
		t.Errorf("environ() = %q\nwant %q", env, want)
	}
}

func TestRunStartFailure(t *testing.T) {
	t.Parallel()
	g := &Git{Bin: "touchmark-no-such-git-binary"}
	_, err := g.Run(t.Context(), nil, "version")
	var gitErr *Error
	if err == nil || errors.As(err, &gitErr) {
		t.Fatalf("err = %v, want a start failure", err)
	}
}

func TestRunCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&Git{}).Run(ctx, nil, "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTailBuffer(t *testing.T) {
	t.Parallel()
	b := newTailBuffer(8)
	_, _ = b.Write([]byte("0123"))
	if got := b.String(); got != "0123" {
		t.Errorf("String() = %q", got)
	}
	_, _ = b.Write([]byte("456789"))
	_, _ = b.Write([]byte("ab"))
	if got := b.String(); got != "...456789ab" {
		t.Errorf("String() = %q", got)
	}
	_, _ = b.Write([]byte(strings.Repeat("x", 20) + "tail1234"))
	if got := b.String(); got != "...tail1234" {
		t.Errorf("String() = %q", got)
	}
}

func TestEnvName(t *testing.T) {
	t.Parallel()
	for kv, want := range map[string]string{
		"GIT_DIR=/x":    "GIT_DIR",
		"git_dir=/x":    "git_dir",
		"=C:=C:\\x":     "=C:",
		"EMPTY=":        "EMPTY",
		"NOEQUALS":      "NOEQUALS",
		"A=B=C":         "A",
		"GIT_DIRX=keep": "GIT_DIRX",
	} {
		if got := envName(kv); got != want {
			t.Errorf("envName(%q) = %q, want %q", kv, got, want)
		}
	}
	if !isDroppedEnv("git_dir=/x") || !isDroppedEnv("GIT_LITERAL_PATHSPECS=1") || isDroppedEnv("GIT_DIRX=/x") || !isDroppedEnv("GIT_CONFIG_COUNT=1") || isDroppedEnv("GIT_CONFIG_NOSYSTEM=1") {
		t.Error("isDroppedEnv misclassifies")
	}
	// touchmark's own variables, credentials first, reach no git process.
	for _, kv := range []string{"TOUCHMARK_GH_WRITE_TOKEN=x", "touchmark_signing_key=x", "TOUCHMARK_HUB=/hub"} {
		if !isDroppedEnv(kv) {
			t.Errorf("isDroppedEnv(%q) = false", kv)
		}
	}
	if isDroppedEnv("TOUCHMARKX=1") || isDroppedEnv("MY_TOUCHMARK_TOKEN=1") {
		t.Error("isDroppedEnv drops variables that are not touchmark's")
	}
}

func assertSameDir(t *testing.T, got, want string) {
	t.Helper()
	gi, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	wi, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gi, wi) {
		t.Errorf("dir = %q, want %q", got, want)
	}
	if got != filepath.Clean(got) || strings.Contains(got, "/") && filepath.Separator != '/' {
		t.Errorf("dir %q is not a clean OS path", got)
	}
}

func TestTopLevel(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	sub := filepath.Join(r.dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{r.dir, sub} {
		g := &Git{Dir: dir, Env: r.g.Env}
		top, ok, err := g.TopLevel(t.Context())
		if err != nil || !ok {
			t.Fatalf("TopLevel(%s) = %q, %v, %v", dir, top, ok, err)
		}
		assertSameDir(t, top, r.dir)
	}

	// Inside .git and in a bare repository there is no work tree.
	bare := t.TempDir()
	(&testRepo{t: t, g: &Git{Dir: bare, Env: r.g.Env}}).git("init", "-q", "--bare")
	for _, dir := range []string{filepath.Join(r.dir, ".git"), bare} {
		top, ok, err := (&Git{Dir: dir, Env: r.g.Env}).TopLevel(t.Context())
		if err != nil || ok {
			t.Errorf("TopLevel(%s) = %q, %v, %v; want not a work tree", dir, top, ok, err)
		}
	}

	// Outside any repository.
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(r.g.Env[:len(r.g.Env):len(r.g.Env)], "GIT_CEILING_DIRECTORIES="+filepath.Dir(plain))
	top, ok, err := (&Git{Dir: plain, Env: env}).TopLevel(t.Context())
	if err != nil || ok {
		t.Errorf("TopLevel(plain) = %q, %v, %v; want not a work tree", top, ok, err)
	}

	// A missing directory is an error, not "no repository".
	_, _, err = (&Git{Dir: filepath.Join(plain, "missing"), Env: env}).TopLevel(t.Context())
	if err == nil {
		t.Error("TopLevel(missing dir): want error")
	}
}

func TestIsShallow(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("a.txt", "one\n")
	r.commit("one")
	r.write("a.txt", "two\n")
	r.commit("two")
	if shallow, err := r.g.IsShallow(t.Context()); err != nil || shallow {
		t.Fatalf("IsShallow(full) = %v, %v", shallow, err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	clone := &Git{Env: r.g.Env}
	if _, err := clone.Run(t.Context(), nil, "clone", "-q", "--depth", "1", fileURL(r.dir), dst); err != nil {
		t.Fatal(err)
	}
	clone.Dir = dst
	if shallow, err := clone.IsShallow(t.Context()); err != nil || !shallow {
		t.Fatalf("IsShallow(clone --depth 1) = %v, %v", shallow, err)
	}
}

// fileURL returns a file:// URL for a local directory, which makes clone
// honour --depth.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:///C:/...
	}
	return "file://" + p
}

func TestObjectFormat(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	if f, err := r.g.ObjectFormat(t.Context()); err != nil || f != "sha1" {
		t.Errorf("ObjectFormat() = %q, %v; want sha1", f, err)
	}
	r256 := newTestRepo(t, false, "--object-format=sha256")
	if f, err := r256.g.ObjectFormat(t.Context()); err != nil || f != "sha256" {
		t.Errorf("ObjectFormat(sha256 repo) = %q, %v; want sha256", f, err)
	}
	// Reading works on sha256 repositories too.
	r256.write("packs/p/a.txt", "a\n")
	head := r256.commit("one")
	entries, err := r256.g.LsTree(t.Context(), head, "packs")
	if err != nil || len(entries) != 1 || len(entries[0].OID) != 64 {
		t.Errorf("LsTree(sha256) = %+v, %v", entries, err)
	}
	got := r256.history("HEAD", "packs")
	diffSets(t, got, r256.treeBlobs("packs"))
}

func TestRevParse(t *testing.T) {
	t.Parallel()
	r := newTestRepo(t, false)
	r.write("a.txt", "a\n")
	head := r.commit("one")
	for _, rev := range []string{"HEAD", "master", head, head[:12]} {
		if got, err := r.g.RevParse(t.Context(), rev); err != nil || got != head {
			t.Errorf("RevParse(%q) = %q, %v; want %s", rev, got, err, head)
		}
	}
	tree, err := r.g.RevParse(t.Context(), "HEAD^{tree}")
	if err != nil || tree == head || !isOID(tree) {
		t.Errorf("RevParse(HEAD^{tree}) = %q, %v", tree, err)
	}
	for _, rev := range []string{"", "-h", "--all", "no-such-branch"} {
		if got, err := r.g.RevParse(t.Context(), rev); err == nil {
			t.Errorf("RevParse(%q) = %q, want error", rev, got)
		}
	}
}

// lsTreeRepo builds a commit through the index only, so that symlinks and
// gitlinks work on any OS.
func lsTreeRepo(t *testing.T) (r *testRepo, oids map[string]string) {
	r = newTestRepo(t, false)
	base := r.commit("base")
	oids = map[string]string{
		"a.txt":                    r.setEntry("100644", "alpha\n", "a.txt"),
		"bin/run.sh":               r.setEntry("100755", "#!/bin/sh\n", "bin/run.sh"),
		"link":                     r.setEntry("120000", "a.txt", "link"),
		"sub":                      r.setEntry("160000", base, "sub"),
		"dir/файл с пробелом.txt":  r.setEntry("100644", "unicode\n", "dir/файл с пробелом.txt"),
		"a[b]/x.txt":               r.setEntry("100644", "bracket\n", "a[b]/x.txt"),
		"ab/y.txt":                 r.setEntry("100644", "plain\n", "ab/y.txt"),
		"bin/nested/deeper/z.bin":  r.setEntry("100644", "z\x00z", "bin/nested/deeper/z.bin"),
		"binary-sibling/other.txt": r.setEntry("100644", "sibling\n", "binary-sibling/other.txt"),
	}
	r.commit("tree")
	return r, oids
}

func TestLsTree(t *testing.T) {
	t.Parallel()
	r, oids := lsTreeRepo(t)
	all, err := r.g.LsTree(t.Context(), "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(oids) {
		t.Fatalf("LsTree(HEAD, \"\") returned %d entries, want %d: %+v", len(all), len(oids), all)
	}
	want := map[string]TreeEntry{
		"a.txt":      {Mode: "100644", Type: "blob", Size: 6},
		"bin/run.sh": {Mode: "100755", Type: "blob", Size: 10},
		"link":       {Mode: "120000", Type: "blob", Size: 5},
		"sub":        {Mode: "160000", Type: "commit", Size: -1},
	}
	for _, e := range all {
		if e.OID != oids[e.Path] {
			t.Errorf("%s: OID %s, want %s", e.Path, e.OID, oids[e.Path])
		}
		w, ok := want[e.Path]
		if !ok {
			if e.Mode != "100644" || e.Type != "blob" || e.Size <= 0 {
				t.Errorf("%s: %+v", e.Path, e)
			}
			continue
		}
		if e.Mode != w.Mode || e.Type != w.Type || e.Size != w.Size {
			t.Errorf("%s: got %s %s %d, want %s %s %d", e.Path, e.Mode, e.Type, e.Size, w.Mode, w.Type, w.Size)
		}
	}

	tests := []struct {
		prefix string
		want   []string
	}{
		{"bin", []string{"bin/nested/deeper/z.bin", "bin/run.sh"}},
		{"bin/", []string{"bin/nested/deeper/z.bin", "bin/run.sh"}},
		{"bin/nested", []string{"bin/nested/deeper/z.bin"}},
		{"bi", nil},
		{"a[b]", []string{"a[b]/x.txt"}},
		{"a?", nil},
		{"dir", []string{"dir/файл с пробелом.txt"}},
		{"missing", nil},
		{"a.txt", []string{"a.txt"}},
	}
	for _, tt := range tests {
		entries, err := r.g.LsTree(t.Context(), "HEAD", tt.prefix)
		if err != nil {
			t.Errorf("LsTree(%q): %v", tt.prefix, err)
			continue
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Path)
		}
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("LsTree(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}

	// Prefixes are paths from the repository root even when Dir is not.
	if err := os.MkdirAll(filepath.Join(r.dir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := &Git{Dir: filepath.Join(r.dir, "dir"), Env: r.g.Env}
	if entries, err := sub.LsTree(t.Context(), "HEAD", "bin/nested"); err != nil || len(entries) != 1 ||
		entries[0].Path != "bin/nested/deeper/z.bin" {
		t.Errorf("LsTree from a subdirectory = %+v, %v", entries, err)
	}
	if got, err := sub.ShowFile(t.Context(), "HEAD", "a.txt"); err != nil || string(got) != "alpha\n" {
		t.Errorf("ShowFile from a subdirectory = %q, %v", got, err)
	}

	if _, err := r.g.LsTree(t.Context(), "no-such-rev", ""); err == nil {
		t.Error("LsTree(no-such-rev): want error")
	}
	if _, err := r.g.LsTree(t.Context(), "--all", ""); err == nil {
		t.Error("LsTree(--all): want error")
	}
}

func TestParseLsTree(t *testing.T) {
	t.Parallel()
	out := "100644 blob e69de29bb2d1d6434b8b29ae775ad8c2e48c5391       0\ttab\there\x00" +
		"160000 commit 1111111111111111111111111111111111111111       -\tsub\x00"
	entries, err := parseLsTree([]byte(out), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "tab\there" || entries[0].Size != 0 || entries[1].Size != -1 {
		t.Errorf("parseLsTree = %+v", entries)
	}
	for _, bad := range []string{
		"100644 blob e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 0 no-tab\x00",
		"100644 blob nothex 0\tp\x00",
		"100644 blob e69de29bb2d1d6434b8b29ae775ad8c2e48c5391 x\tp\x00",
		"100644 blob e69de29bb2d1d6434b8b29ae775ad8c2e48c5391\tp\x00",
	} {
		if _, err := parseLsTree([]byte(bad), ""); err == nil {
			t.Errorf("parseLsTree(%q): want error", bad)
		}
	}
}

func TestShowFile(t *testing.T) {
	t.Parallel()
	r, _ := lsTreeRepo(t)
	ctx := t.Context()
	for path, want := range map[string]string{
		"a.txt":                   "alpha\n",
		"dir/файл с пробелом.txt": "unicode\n",
		"bin/nested/deeper/z.bin": "z\x00z",
		"a[b]/x.txt":              "bracket\n",
		"link":                    "a.txt",
	} {
		got, err := r.g.ShowFile(ctx, "HEAD", path)
		if err != nil || string(got) != want {
			t.Errorf("ShowFile(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"missing.txt", "a.txt/below", "bin/missing", "ab/y.txt/x"} {
		if _, err := r.g.ShowFile(ctx, "HEAD", path); !errors.Is(err, ErrNotFound) {
			t.Errorf("ShowFile(%q) = %v, want ErrNotFound", path, err)
		}
	}
	// A directory, a submodule or a bad revision is an error, but not "not found".
	for _, c := range []struct{ rev, path string }{
		{"HEAD", "bin"},
		{"HEAD", "sub"},
		{"no-such-rev", "a.txt"},
		{"-x", "a.txt"},
		{"HEAD", ""},
		{"HEAD", "../a.txt"},
	} {
		_, err := r.g.ShowFile(ctx, c.rev, c.path)
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("ShowFile(%q, %q) = %v, want a non-ErrNotFound error", c.rev, c.path, err)
		}
	}
	// The file existed in an older commit only.
	r.git("rm", "-q", "--cached", "a.txt")
	r.commit("drop a")
	if _, err := r.g.ShowFile(ctx, "HEAD", "a.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ShowFile(HEAD, a.txt) after delete = %v, want ErrNotFound", err)
	}
	if got, err := r.g.ShowFile(ctx, "HEAD~1", "a.txt"); err != nil || string(got) != "alpha\n" {
		t.Errorf("ShowFile(HEAD~1, a.txt) = %q, %v", got, err)
	}
}
