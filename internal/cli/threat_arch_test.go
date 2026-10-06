package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The security architecture, checked over the source of every package the
// touchmark binary is built from (docs/project/threat-model.md):
//   - touchmark runs no program but git, and nothing through a shell: only
//     package gitx starts processes, through one function, whose program is
//     git (or a test's replacement);
//   - every HTTP request goes through package httpx, whose client sends a
//     credential only to its own host and bounds every answer: no other
//     package makes an HTTP client, a transport or a request of its own, and
//     none replaces httpx's transport or body limit;
//   - touchmark never merges, and never asks a platform to merge for it: no
//     request names a merge endpoint or an auto-merge option, and the only
//     GraphQL mutation is updateRefs.
//
// The checks read the syntax of the non-test files, whatever their build
// tags, so that a Windows-only file is checked on Linux too.
// TestThreatArchChecksCatch shows that each check finds what it looks for.

// modulePath is the module's import path.
const modulePath = "github.com/bedrock-python/touchmark"

// testOnlyPackages are the packages under internal/ that only tests import:
// the fakes of the platforms, the conformance suite and the e2e tests. They
// start processes and serve HTTP, as fakes must; the binary never links
// them (TestTestOnlyPackagesStayOut).
var testOnlyPackages = []string{
	"internal/e2e",
	"internal/e2e/github",
	"internal/e2e/gitlab",
	"internal/platform/conformance",
	"internal/platform/fake",
	"internal/platform/github/ghfake",
}

// srcFile is one parsed non-test file of a binary package.
type srcFile struct {
	pkg  string // directory relative to the module root, slash-separated
	name string // file name
	f    *ast.File
	fset *token.FileSet
}

// pos describes a position for a message.
func (s srcFile) pos(p token.Pos) string {
	return s.pkg + "/" + s.name + ":" + strconv.Itoa(s.fset.Position(p).Line)
}

// imports returns the local name under which the file imports p.
func (s srcFile) imports(p string) (string, bool) {
	for _, imp := range s.f.Imports {
		v, err := strconv.Unquote(imp.Path.Value)
		if err != nil || v != p {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return path.Base(p), true
	}
	return "", false
}

// binarySources parses the non-test files of every package under cmd/ and
// internal/ but the test-only ones.
func binarySources(t *testing.T) []srcFile {
	t.Helper()
	root, err := filepath.Abs(filepath.Join(pkgDir, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var out []srcFile
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, filepath.Dir(p))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			if slices.Contains(testOnlyPackages, pkg) {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return err
			}
			out = append(out, srcFile{pkg: pkg, name: name, f: f, fset: fset})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 100 {
		t.Fatalf("only %d source files found under %s: update the test", len(out), root)
	}
	return out
}

// selectorOf returns Name for a selector local.Name, "" for anything else.
func selectorOf(e ast.Expr, local string) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != local {
		return ""
	}
	return sel.Sel.Name
}

// exprText renders a small expression for messages and comparisons.
func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.StarExpr:
		return "*" + exprText(x.X)
	case *ast.CallExpr:
		return exprText(x.Fun) + "(…)"
	}
	return "an expression"
}

// reportProblems fails t with every problem.
func reportProblems(t *testing.T, problems []string) {
	t.Helper()
	for _, p := range problems {
		t.Error(p)
	}
}

// TestTestOnlyPackagesStayOut: no package of the binary imports a fake, the
// conformance suite or an e2e package, so leaving them out of the checks
// below leaves nothing the binary runs unchecked.
func TestTestOnlyPackagesStayOut(t *testing.T) {
	reportProblems(t, checkTestOnly(binarySources(t)))
}

func checkTestOnly(files []srcFile) []string {
	var out []string
	for _, s := range files {
		for _, imp := range s.f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			rel, ok := strings.CutPrefix(p, modulePath+"/")
			if ok && slices.Contains(testOnlyPackages, rel) {
				out = append(out, fmt.Sprintf("%s imports %s, which only tests may use", s.pos(imp.Pos()), p))
			}
		}
	}
	return out
}

// TestOnlyGitRuns: touchmark starts no program but git, and none through a
// shell (threat T5, a harmful target). Only package gitx imports
// os/exec; nothing starts a process through os or syscall; in gitx, one
// method builds every command (Git.command), and the program it runs is
// its local bin: Git.Bin, or "git" when that is empty. Outside gitx, Bin
// is set only from a field that tests replace (the hub channel's bin),
// which no non-test code sets.
func TestOnlyGitRuns(t *testing.T) {
	problems, builders := checkOnlyGit(binarySources(t))
	reportProblems(t, problems)
	if builders != 1 {
		t.Errorf("found %d calls of exec.CommandContext in gitx, want the one in Git.command: update the test", builders)
	}
}

// checkOnlyGit returns the problems TestOnlyGitRuns reports and how many
// calls of exec.CommandContext it found.
func checkOnlyGit(files []srcFile) (out []string, builders int) {
	for _, s := range files {
		execName, usesExec := s.imports("os/exec")
		if usesExec && s.pkg != "internal/gitx" {
			out = append(out, fmt.Sprintf("%s/%s imports os/exec: only package gitx runs programs (git)", s.pkg, s.name))
		}
		osName, usesOS := s.imports("os")
		sysName, usesSys := s.imports("syscall")
		ast.Inspect(s.f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if usesOS && selectorOf(x, osName) == "StartProcess" {
					out = append(out, s.pos(x.Pos())+": os.StartProcess: only gitx.Git.command starts a process")
				}
				if usesSys && slices.Contains([]string{"Exec", "ForkExec", "StartProcess", "CreateProcess", "CreateProcessAsUser"}, selectorOf(x, sysName)) {
					out = append(out, fmt.Sprintf("%s: syscall.%s: only gitx.Git.command starts a process", s.pos(x.Pos()), x.Sel.Name))
				}
				if usesExec && selectorOf(x, execName) == "LookPath" {
					out = append(out, s.pos(x.Pos())+": exec.LookPath: the program is git, which exec finds itself")
				}
			case *ast.KeyValueExpr:
				k, ok := x.Key.(*ast.Ident)
				if !ok {
					break
				}
				if k.Name == "Bin" && s.pkg != "internal/gitx" {
					if sel, ok := x.Value.(*ast.SelectorExpr); !ok || sel.Sel.Name != "bin" {
						out = append(out, fmt.Sprintf("%s: Bin is set to %s: the program touchmark runs is git", s.pos(x.Pos()), exprText(x.Value)))
					}
				}
				if k.Name == "bin" {
					out = append(out, s.pos(x.Pos())+": the field bin is set outside tests")
				}
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && (sel.Sel.Name == "bin" || (sel.Sel.Name == "Bin" && s.pkg != "internal/gitx")) {
						out = append(out, fmt.Sprintf("%s: %s is assigned outside tests", s.pos(x.Pos()), exprText(l)))
					}
				}
			}
			return true
		})
		if !usesExec {
			continue
		}
		for _, d := range s.f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch selectorOf(call.Fun, execName) {
				case "Command":
					out = append(out, s.pos(call.Pos())+": exec.Command: commands are built by Git.command, with a context")
				case "CommandContext":
					builders++
					if fd.Name.Name != "command" || fd.Recv == nil || exprText(fd.Recv.List[0].Type) != "*Git" {
						out = append(out, fmt.Sprintf("%s: exec.CommandContext in %s: only Git.command builds commands", s.pos(call.Pos()), fd.Name.Name))
					}
					if len(call.Args) < 2 || exprText(call.Args[1]) != "bin" {
						out = append(out, s.pos(call.Pos())+": the program of exec.CommandContext is not Git.command's bin")
					}
					if values := binValues(fd); !slices.Equal(values, []string{`"git"`, "g.Bin"}) {
						out = append(out, fmt.Sprintf("%s: Git.command's bin takes %q, want g.Bin or \"git\"", s.pos(fd.Pos()), values))
					}
				}
				return true
			})
		}
	}
	return out, builders
}

// binValues returns, sorted, what the local bin of fd is set to.
func binValues(fd *ast.FuncDecl) []string {
	var values []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == "bin" && i < len(x.Rhs) {
					values = append(values, exprText(x.Rhs[i]))
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if id.Name == "bin" && i < len(x.Values) {
					values = append(values, exprText(x.Values[i]))
				}
			}
		}
		return true
	})
	slices.Sort(values)
	return values
}

// TestOnlyHTTPXSpeaksHTTP: every request goes through httpx.Client, which
// sends a credential only to its own host, follows no redirect with it,
// refuses plain http but to loopback and bounds every answer at 32 MiB. No
// other package makes an http.Client or a transport, uses net/http's
// default client or transport, or sends a request itself; and no
// httpx.Options outside httpx replaces its transport or its body limit.
func TestOnlyHTTPXSpeaksHTTP(t *testing.T) {
	reportProblems(t, checkOnlyHTTPX(binarySources(t)))
}

func checkOnlyHTTPX(files []srcFile) []string {
	var out []string
	for _, s := range files {
		if s.pkg == "internal/httpx" {
			continue
		}
		httpName, usesHTTP := s.imports("net/http")
		httpxName, usesHTTPX := s.imports(modulePath + "/internal/httpx")
		ast.Inspect(s.f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if name := selectorOf(x.Type, httpName); usesHTTP && (name == "Client" || name == "Transport") {
					out = append(out, fmt.Sprintf("%s: an http.%s of its own: requests go through httpx", s.pos(x.Pos()), name))
				}
				if !usesHTTPX || selectorOf(x.Type, httpxName) != "Options" {
					break
				}
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && (k.Name == "Transport" || k.Name == "MaxBody") {
							out = append(out, fmt.Sprintf("%s: httpx.Options sets %s: only tests replace httpx's transport or body limit", s.pos(kv.Pos()), k.Name))
						}
					}
				}
			case *ast.SelectorExpr:
				if !usesHTTP {
					break
				}
				switch name := selectorOf(x, httpName); name {
				case "DefaultClient", "DefaultTransport", "Get", "Head", "Post", "PostForm", "NewFileTransport":
					out = append(out, fmt.Sprintf("%s: http.%s: requests go through httpx", s.pos(x.Pos()), name))
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RoundTrip" {
					out = append(out, s.pos(x.Pos())+": a RoundTrip call: requests go through httpx.Client")
				}
			}
			return true
		})
	}
	return out
}

// mergeWords are words of the platforms' merge and auto-merge APIs
// (lowercase): REST bodies and paths, GraphQL fields. Struct tags of
// answers touchmark reads (merged_at, merged_by) are not requests and are
// not checked.
var mergeWords = []string{
	"auto_merge", "automerge", "merge_when_pipeline_succeeds", "merge_when_checks_succeed",
	"merge_commit_message", "merge_commit_sha", "merge_method", "mergepullrequest",
	"enablepullrequestautomerge", "enqueuepullrequest", "merge_queue", "mergequeue",
}

// TestNoMergeRequests: touchmark never merges a pull request, nor asks a
// platform to merge one later: no automerge, and no merge options. No
// string in the binary's code names a merge endpoint
// (a path segment "merge" or "merges": GitHub's PUT …/pulls/{n}/merge and
// POST …/merges, GitLab's PUT …/merge_requests/{iid}/merge, Gitea's POST
// …/pulls/{n}/merge) or a word of the merge and auto-merge APIs, and the
// only GraphQL mutation is updateRefs, which moves the sync branch.
// Messages that speak of merges, and git's merge-base, pass.
func TestNoMergeRequests(t *testing.T) {
	problems, mutations := checkNoMerge(binarySources(t))
	reportProblems(t, problems)
	if mutations == 0 {
		t.Error("no GraphQL mutation found: the GitHub driver sends updateRefs; update the test")
	}
}

// checkNoMerge returns the problems TestNoMergeRequests reports and how
// many GraphQL mutations it found.
func checkNoMerge(files []srcFile) (out []string, mutations int) {
	for _, s := range files {
		tags := map[*ast.BasicLit]bool{}
		ast.Inspect(s.f, func(n ast.Node) bool {
			if f, ok := n.(*ast.Field); ok && f.Tag != nil {
				tags[f.Tag] = true
			}
			return true
		})
		ast.Inspect(s.f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || tags[lit] {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '?' || r == '&' }) {
				if seg == "merge" || seg == "merges" {
					out = append(out, fmt.Sprintf("%s: %q names a merge endpoint", s.pos(lit.Pos()), v))
				}
			}
			lower := strings.ToLower(v)
			for _, w := range mergeWords {
				if strings.Contains(lower, w) {
					out = append(out, fmt.Sprintf("%s: %q names %s, of the merge APIs", s.pos(lit.Pos()), v, w))
				}
			}
			if gqlMutation(v) {
				mutations++
				if !strings.Contains(v, "updateRefs(input:") || strings.Count(v, "(input:") != 1 {
					out = append(out, fmt.Sprintf("%s: GraphQL mutation %q: the only one touchmark sends is updateRefs", s.pos(lit.Pos()), v))
				}
			}
			return true
		})
	}
	return out, mutations
}

// gqlMutation reports whether s is a GraphQL document (it has a selection
// set) whose first word, after blanks and comments, is "mutation".
func gqlMutation(s string) bool {
	if !strings.Contains(s, "{") {
		return false
	}
	for {
		s = strings.TrimLeft(s, " \t\r\n,")
		if !strings.HasPrefix(s, "#") {
			break
		}
		_, s, _ = strings.Cut(s, "\n")
	}
	rest, ok := strings.CutPrefix(s, "mutation")
	return ok && rest != "" && strings.ContainsAny(rest[:1], " ({\t\r\n")
}

// TestThreatArchChecksCatch feeds each check a package that breaks its
// rule, so that a check that silently finds nothing fails here.
func TestThreatArchChecksCatch(t *testing.T) {
	parse := func(pkg, src string) []srcFile {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return []srcFile{{pkg: pkg, name: "x.go", f: f, fset: fset}}
	}
	wantAll := func(name string, problems []string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, w) }) {
				t.Errorf("%s: problems %q lack %q", name, problems, w)
			}
		}
	}

	if got := checkTestOnly(parse("internal/distribute", `package distribute
import _ "github.com/bedrock-python/touchmark/internal/platform/fake"`)); len(got) != 1 {
		t.Errorf("a fake in the binary: %q", got)
	}

	shell, _ := checkOnlyGit(parse("internal/hubch", `package hubch
import (
	"os"
	"os/exec"
	sc "syscall"
	"github.com/bedrock-python/touchmark/internal/gitx"
)
func f() {
	_ = exec.Command("sh", "-c", "x")
	_, _ = os.StartProcess("/bin/sh", nil, nil)
	_ = sc.Exec("/bin/sh", nil, nil)
	_ = gitx.Git{Bin: "/bin/sh"}
}`))
	wantAll("a shell outside gitx", shell, "imports os/exec", "exec.Command", "os.StartProcess", "syscall.Exec", `Bin is set to "/bin/sh"`)
	other, builders := checkOnlyGit(parse("internal/gitx", `package gitx
import "os/exec"
type Git struct{ Bin string }
func (g *Git) command(args []string) *exec.Cmd {
	bin := "bash"
	return exec.CommandContext(nil, bin, args...)
}
func run() { _ = exec.CommandContext(nil, "git") }`))
	wantAll("gitx running another program", other, `bin takes ["\"bash\""]`, "exec.CommandContext in run", "not Git.command's bin")
	if builders != 2 {
		t.Errorf("builders %d", builders)
	}

	wantAll("HTTP outside httpx", checkOnlyHTTPX(parse("internal/platform/gitea", `package gitea
import (
	"net/http"
	"github.com/bedrock-python/touchmark/internal/httpx"
)
func f(rt http.RoundTripper, r *http.Request) {
	_ = &http.Client{}
	_, _ = http.Get("https://example.com")
	_ = http.DefaultClient
	_, _ = rt.RoundTrip(r)
	_ = httpx.New(httpx.Options{Transport: rt, MaxBody: 1 << 40})
}`)), "http.Client of its own", "http.Get", "http.DefaultClient", "RoundTrip call", "sets Transport", "sets MaxBody")

	merges, mutations := checkNoMerge(parse("internal/platform/github", `package github
type pr struct {
	MergedAt string `+"`json:\"merged_at\"`"+`
}
const q = `+"`mutation($i: MergePullRequestInput!) { mergePullRequest(input: $i) { clientMutationId } }`"+`
func f(c, owner, name, n string) {
	_ = []string{"repos", owner, name, "pulls", n, "merge"}
	_ = "/projects/1/merge_requests/2/merge"
	_ = map[string]bool{"merge_when_pipeline_succeeds": true}
	_ = "a merge of two parents"
}`))
	wantAll("merges", merges, `"merge" names a merge endpoint`, `"/projects/1/merge_requests/2/merge" names a merge endpoint`,
		"names merge_when_pipeline_succeeds", "names mergepullrequest", "the only one touchmark sends is updateRefs")
	if len(merges) != 5 || mutations != 1 {
		t.Errorf("merges %q, mutations %d", merges, mutations)
	}
}
