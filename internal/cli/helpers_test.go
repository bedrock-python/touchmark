package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/report"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// goldenDir is testdata/golden of the package, absolute once TestMain ran;
// pkgDir is the package's directory, where the tests start.
var (
	goldenDir = filepath.Join("testdata", "golden")
	pkgDir    string
)

// inJob runs the rest of a test that runs touchmark as a CI job (vars set
// CI) in a directory of its own, as a job runs: a CI run leaves its report
// files in the working directory (publish), which is the package's
// otherwise. A test that chose a directory keeps it. It must not run in a
// parallel test.
func inJob(t *testing.T, vars map[string]string) {
	t.Helper()
	if vars["CI"] == "" && vars["GITHUB_ACTIONS"] == "" && vars["GITLAB_CI"] == "" {
		return
	}
	if wd, err := os.Getwd(); err == nil && sameDir(wd, pkgDir) {
		t.Chdir(t.TempDir())
	}
}

// sameDir reports whether a and b are the same directory, however they are
// spelled: on Windows the working directory a t.Chdir restores may come
// back as the long form of a short (8.3) path.
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return a == b
	}
	bi, err := os.Stat(b)
	if err != nil {
		return a == b
	}
	return os.SameFile(ai, bi)
}

// TestMain isolates git from the machine's config (the system config may set
// core.autocrlf) and makes commits deterministic, for the test helpers and
// for the git that touchmark runs in process.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	// Tests may change the working directory (t.Chdir): the golden files
	// are found from where the package is.
	if dir, err := filepath.Abs(filepath.Join("testdata", "golden")); err == nil {
		goldenDir = dir
	}
	pkgDir, _ = os.Getwd()
	home, err := os.MkdirTemp("", "touchmark-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(home)
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	env := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   global,
		"HOME":                home,
		"XDG_CONFIG_HOME":     home,
		"GIT_AUTHOR_NAME":     "touchmark test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "touchmark test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_AUTHOR_DATE":     "1767225600 +0000",
		"GIT_COMMITTER_DATE":  "1767225600 +0000",
		// Plain directories under the temp dir must not resolve to a
		// repository around it (a dotfiles repository in the profile).
		"GIT_CEILING_DIRECTORIES": os.TempDir(),
	}
	for k, v := range env {
		os.Setenv(k, v)
	}
	os.Unsetenv(hubEnv)
	code := m.Run()
	// A test that runs a command as a CI job does must run it in a
	// directory of its own (t.Chdir): the job's report files go to the
	// working directory, which is the package's otherwise.
	for _, name := range []string{deliveryFiles + ".json", deliveryFiles + ".md", streamFile, doctorFiles + ".json", doctorFiles + ".md"} {
		if _, err := os.Stat(name); err == nil {
			fmt.Fprintf(os.Stderr, "a test left %s in the package's directory\n", name)
			os.Remove(name)
			code = 1
		}
	}
	return code
}

// repo is a throwaway git repository with a work tree, or a plain directory
// when g is nil.
type repo struct {
	t    *testing.T
	dir  string
	g    *gitx.Git
	tick int
}

// newRepo creates a repository on branch master with core.autocrlf set
// explicitly.
func newRepo(t *testing.T, autocrlf bool) *repo {
	t.Helper()
	dir := t.TempDir()
	r := &repo{t: t, dir: dir, g: gitx.New(dir)}
	r.git("init", "-q", "-b", "master")
	r.git("config", "core.autocrlf", fmt.Sprint(autocrlf))
	return r
}

// newHub creates a hub repository. Its files are committed byte for byte.
func newHub(t *testing.T) *repo { return newRepo(t, false) }

// newPlainDir creates a target directory that is not a git work tree.
func newPlainDir(t *testing.T) *repo {
	t.Helper()
	return &repo{t: t, dir: t.TempDir()}
}

// git runs git in the repository and fails the test on error.
func (r *repo) git(args ...string) string {
	r.t.Helper()
	out, err := r.g.Run(r.t.Context(), nil, args...)
	if err != nil {
		r.t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// abs returns the OS path of the repository path p.
func (r *repo) abs(p string) string { return filepath.Join(r.dir, filepath.FromSlash(p)) }

// put writes content at p without staging it.
func (r *repo) put(p, content string) {
	r.t.Helper()
	full := r.abs(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// write writes content at p and stages it in a git repository.
func (r *repo) write(p, content string) {
	r.t.Helper()
	r.put(p, content)
	if r.g != nil {
		r.git("add", "--", p)
	}
}

// rm removes p (a file or directory) from the work tree and the index.
func (r *repo) rm(p string) {
	r.t.Helper()
	r.git("rm", "-q", "-r", "-f", "--", p)
}

// setEntry stages an index entry with any mode without touching the work
// tree; content is written as a blob.
func (r *repo) setEntry(mode, content, p string) {
	r.t.Helper()
	oid, err := r.g.Run(r.t.Context(), strings.NewReader(content), "hash-object", "-w", "--stdin")
	if err != nil {
		r.t.Fatal(err)
	}
	r.git("update-index", "--add", "--cacheinfo", mode+","+strings.TrimSpace(string(oid))+","+p)
}

// commit commits the index with a distinct, increasing date and returns the
// new HEAD.
func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.tick++
	date := fmt.Sprintf("%d +0000", 1767225600+60*r.tick)
	g := *r.g
	g.Env = []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	if _, err := g.Run(r.t.Context(), nil, "commit", "-q", "--allow-empty", "--no-verify", "-m", msg); err != nil {
		r.t.Fatalf("commit %q: %v", msg, err)
	}
	return r.git("rev-parse", "HEAD")
}

// read returns the content at p, failing the test when it is absent.
func (r *repo) read(p string) string {
	r.t.Helper()
	b, err := os.ReadFile(r.abs(p))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// exists reports whether something is at p.
func (r *repo) exists(p string) bool {
	_, err := os.Lstat(r.abs(p))
	return err == nil
}

// root returns the directory touchmark reports for the repository: the git
// top level, or the directory itself.
func (r *repo) root() string {
	r.t.Helper()
	if r.g == nil {
		return r.dir
	}
	top, ok, err := r.g.TopLevel(r.t.Context())
	if err != nil || !ok {
		r.t.Fatalf("top level of %s: ok=%v err=%v", r.dir, ok, err)
	}
	return top
}

// tree renders the work tree without .git: one line per file with its path
// and first line (marked [crlf] when it holds CRLF line endings), and one
// line per empty directory.
func (r *repo) tree() string {
	r.t.Helper()
	var lines []string
	err := filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && d.Name() == ".git":
			return fs.SkipDir
		case d.IsDir():
			entries, err := os.ReadDir(p)
			if err == nil && len(entries) == 0 {
				lines = append(lines, rel+"/")
			}
			return err
		case d.Type()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(p)
			lines = append(lines, rel+" -> "+filepath.ToSlash(dest))
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(string(data), "\n")
		label := strings.TrimSuffix(first, "\r")
		switch {
		case len(data) == 0:
			label = "(empty)"
		case strings.Contains(string(data), "\r\n"):
			label += " [crlf]"
		}
		lines = append(lines, rel+"  "+label)
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

// text returns distinct content of at least 64 bytes whose first line is
// label.
func text(label string) string {
	return label + "\n" + "shared engineering asset, kept in sync by touchmark\n" + "more shared content\n"
}

// result is the outcome of one in-process run.
type result struct {
	code           int
	stdout, stderr string
}

// run runs the command line in process.
func run(t *testing.T, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(t.Context(), args, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// scenario is one end-to-end test: a hub, a target and a golden directory.
type scenario struct {
	t      *testing.T
	name   string
	hub    *repo
	target *repo
	// flags are added to every status and apply run (--repo, …).
	flags []string
}

func newScenario(t *testing.T, name string, hub, target *repo, flags ...string) *scenario {
	return &scenario{t: t, name: name, hub: hub, target: target, flags: flags}
}

// args returns the arguments of command against the scenario's hub and
// target.
func (s *scenario) args(command string, extra ...string) []string {
	args := []string{command, "--hub", s.hub.dir}
	if command == "status" || command == "apply" {
		args = append(args, "--dir", s.target.dir)
		args = append(args, s.flags...)
	}
	return append(args, extra...)
}

// run runs command and fails the test unless it exits with code.
func (s *scenario) run(code int, command string, extra ...string) result {
	s.t.Helper()
	res := run(s.t, s.args(command, extra...)...)
	if res.code != code {
		s.t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(s.args(command, extra...), " "), res.code, code, res.stdout, res.stderr)
	}
	return res
}

// json runs command with --format json, expects exit 0 and returns the
// report with machine-specific values replaced.
func (s *scenario) json(command string, extra ...string) string {
	s.t.Helper()
	res := s.run(0, command, append([]string{"--format", "json"}, extra...)...)
	return s.normalizeSync(res.stdout)
}

// text runs command, expects exit 0 and returns its normalized text output.
func (s *scenario) text(command string, extra ...string) string {
	s.t.Helper()
	return s.normalize(s.run(0, command, extra...).stdout)
}

// report runs command with --format json, expects exit 0 and decodes the
// report.
func (s *scenario) report(command string, extra ...string) report.Sync {
	s.t.Helper()
	res := s.run(0, command, append([]string{"--format", "json"}, extra...)...)
	return decodeSync(s.t, res.stdout)
}

func decodeSync(t *testing.T, out string) report.Sync {
	t.Helper()
	var rep report.Sync
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	return rep
}

// normalizeSync replaces the hub directory, the target root and the hub
// commit of a JSON report with placeholders.
func (s *scenario) normalizeSync(out string) string {
	s.t.Helper()
	rep := decodeSync(s.t, out)
	rep.Hub.Dir, rep.Hub.Commit = "$HUB", "$COMMIT"
	rep.Target.Root = "$TARGET"
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, &rep); err != nil {
		s.t.Fatal(err)
	}
	return s.normalize(buf.String())
}

// normalize replaces the hub and target paths and the hub commit in output.
func (s *scenario) normalize(out string) string {
	s.t.Helper()
	repl := map[string]string{}
	add := func(path, placeholder string) {
		for _, p := range []string{path, filepath.ToSlash(path)} {
			repl[p] = placeholder
			if q, err := json.Marshal(p); err == nil {
				repl[strings.Trim(string(q), `"`)] = placeholder
			}
		}
	}
	add(s.hub.root(), "$HUB")
	add(s.hub.dir, "$HUB")
	if s.target != nil {
		add(s.target.root(), "$TARGET")
		add(s.target.dir, "$TARGET")
	}
	if commit := s.hub.git("rev-parse", "HEAD"); len(commit) >= 7 {
		repl[commit] = "$COMMIT"
		repl[commit[:7]] = "$COMMIT"
	}
	// Longest first, so that a path is not replaced by its prefix.
	keys := make([]string, 0, len(repl))
	for k := range repl {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	for _, k := range keys {
		out = strings.ReplaceAll(out, k, repl[k])
	}
	return out
}

// golden compares got with testdata/golden/<scenario>/<file>, or rewrites
// it with -update.
func (s *scenario) golden(file, got string) {
	s.t.Helper()
	path := filepath.Join(goldenDir, s.name, file)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			s.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatalf("%v (run go test -update to create it)", err)
	}
	if string(want) != got {
		s.t.Errorf("%s differs from the golden file (go test -update rewrites it)\n--- got\n%s--- want\n%s", path, got, want)
	}
}

// wantContent fails the test unless the target holds content at p.
func (s *scenario) wantContent(p, content string) {
	s.t.Helper()
	if got := s.target.read(p); got != content {
		s.t.Errorf("%s = %q, want %q", p, got, content)
	}
}

// entry returns the report entry for path, failing the test when absent.
func entry(t *testing.T, rep report.Sync, path string) report.Entry {
	t.Helper()
	for _, e := range rep.Entries {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", path, rep.Entries)
	return report.Entry{}
}
