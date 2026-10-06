package ghfake

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// contentWorld adds acme/files: a regular file, an executable, a symlink
// to a file, a dangling symlink, a symlink out of the repository, a
// submodule with .gitmodules, a directory and a large file.
func contentWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, Options{MaxTreeEntries: 5})
	sub := strings.Repeat("ab", 20)
	w.repo(RepoSpec{Owner: "acme", Name: "files", Files: []File{
		{Path: "AGENTS.md", Content: []byte(strings.Repeat("agents line\n", 10))},
		{Path: "run.sh", Mode: ModeExecutable, Content: []byte("#!/bin/sh\n")},
		{Path: "CLAUDE.md", Mode: ModeSymlink, Content: []byte("AGENTS.md")},
		{Path: "docs/link.md", Mode: ModeSymlink, Content: []byte("../AGENTS.md")},
		{Path: "dangling", Mode: ModeSymlink, Content: []byte("nowhere")},
		{Path: "outside", Mode: ModeSymlink, Content: []byte("../../etc/passwd")},
		{Path: "vendor/lib", Mode: ModeGitlink, Content: []byte(sub)},
		{Path: ".gitmodules", Content: []byte("[submodule \"lib\"]\n\tpath = vendor/lib\n\turl = https://example.invalid/lib.git\n")},
		{Path: "docs/guide.md", Content: []byte("guide\n")},
		{Path: "big.bin", Content: bytes.Repeat([]byte{'x'}, 1<<20+1)},
	}})
	return w
}

func TestContents(t *testing.T) {
	w := contentWorld(t)
	get := func(path string, header ...string) reply {
		return w.call("GET", "/repos/acme/files/contents/"+path, "", nil, header...)
	}
	decode := func(t *testing.T, got map[string]any) string {
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(got["content"].(string), "\n", ""))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	t.Run("file", func(t *testing.T) {
		got := get("AGENTS.md").obj(t)
		if got["type"] != "file" || got["encoding"] != "base64" || got["size"] != float64(120) {
			t.Errorf("file: %v", got)
		}
		if c := got["content"].(string); !strings.Contains(c, "\n") || len(strings.Split(c, "\n")[0]) != 60 {
			t.Errorf("content is not wrapped at 60 characters: %q", c)
		}
		if decode(t, got) != strings.Repeat("agents line\n", 10) {
			t.Errorf("content %q", decode(t, got))
		}
	})
	t.Run("raw", func(t *testing.T) {
		r := get("run.sh", "Accept", "application/vnd.github.raw+json")
		if string(r.body) != "#!/bin/sh\n" || !strings.HasPrefix(r.header.Get("Content-Type"), "application/vnd.github.raw") {
			t.Errorf("raw: %q %v", r.body, r.header)
		}
	})
	t.Run("symlink to a file", func(t *testing.T) {
		link := get("CLAUDE.md").obj(t)
		target := get("AGENTS.md").obj(t)
		// Observed on github.com: the target's size and content under the
		// link's name, path and sha.
		if link["type"] != "file" || link["path"] != "CLAUDE.md" || link["sha"] == target["sha"] ||
			link["size"] != target["size"] || link["content"] != target["content"] {
			t.Errorf("symlink: %v", link)
		}
		if got := get("docs/link.md").obj(t); got["type"] != "file" || got["size"] != float64(120) {
			t.Errorf("relative symlink: %v", got)
		}
		if r := get("CLAUDE.md", "Accept", "application/vnd.github.raw"); !strings.HasPrefix(string(r.body), "agents line") {
			t.Errorf("raw symlink: %q", r.body)
		}
	})
	t.Run("other symlinks", func(t *testing.T) {
		for _, p := range []string{"dangling", "outside"} {
			got := get(p).obj(t)
			if got["type"] != "symlink" || got["target"] == nil || got["content"] != nil {
				t.Errorf("%s: %v", p, got)
			}
		}
	})
	t.Run("submodule", func(t *testing.T) {
		got := get("vendor/lib").obj(t)
		if got["type"] != "submodule" || got["sha"] != strings.Repeat("ab", 20) ||
			got["submodule_git_url"] != "https://example.invalid/lib.git" || got["size"] != float64(0) {
			t.Errorf("submodule: %v", got)
		}
	})
	t.Run("directory", func(t *testing.T) {
		list := get("docs").list(t)
		if len(list) != 2 || field(list, 0, "name") != "guide.md" || field(list, 1, "type") != "symlink" {
			t.Errorf("directory: %v", list)
		}
		obj := get("docs", "Accept", "application/vnd.github.object+json").obj(t)
		if obj["type"] != "dir" || len(obj["entries"].([]any)) != 2 {
			t.Errorf("object directory: %v", obj)
		}
	})
	t.Run("large", func(t *testing.T) {
		r := get("big.bin")
		wantStatus(t, "large JSON", r, 403)
		if field(r.obj(t), "errors", 0, "code") != "too_large" {
			t.Errorf("large: %s", r.body)
		}
		obj := get("big.bin", "Accept", "application/vnd.github.object+json").obj(t)
		if obj["encoding"] != "none" || obj["content"] != "" || obj["size"] != float64(1<<20+1) {
			t.Errorf("large object: encoding %v size %v", obj["encoding"], obj["size"])
		}
		if r := get("big.bin", "Accept", "application/vnd.github.raw+json"); len(r.body) != 1<<20+1 {
			t.Errorf("large raw: %d bytes", len(r.body))
		}
	})
	t.Run("refs", func(t *testing.T) {
		head := w.s.Branch("acme/files", "main")
		w.commit("acme/files", CommitSpec{Files: []File{{Path: "AGENTS.md", Content: []byte("v2\n")}}})
		if got := get("AGENTS.md?ref=" + head).obj(t); got["size"] != float64(120) {
			t.Errorf("at the old commit: %v", got["size"])
		}
		if got := get("AGENTS.md?ref=" + head[:7]).obj(t); got["size"] != float64(120) {
			t.Errorf("at an abbreviated id: %v", got["size"])
		}
		if got := get("AGENTS.md").obj(t); decode(t, got) != "v2\n" {
			t.Errorf("default branch: %v", got)
		}
		r := get("AGENTS.md?ref=nope")
		wantStatus(t, "unknown ref", r, 404)
		if r.obj(t)["message"] != "No commit found for the ref nope" {
			t.Errorf("unknown ref: %s", r.body)
		}
		wantStatus(t, "missing path", get("missing.md"), 404)
	})
	t.Run("empty repository", func(t *testing.T) {
		w.repo(RepoSpec{Owner: "acme", Name: "void"})
		r := w.call("GET", "/repos/acme/void/contents/README.md", "", nil)
		wantStatus(t, "empty", r, 404)
		if r.obj(t)["message"] != "This repository is empty." {
			t.Errorf("empty: %s", r.body)
		}
		r = w.call("GET", "/repos/acme/void/git/trees/main?recursive=1", "", nil)
		wantStatus(t, "empty tree", r, 409)
		if r.obj(t)["message"] != "Git Repository is empty." {
			t.Errorf("empty tree: %s", r.body)
		}
	})
}

func TestTrees(t *testing.T) {
	w := contentWorld(t)
	w.s.SetMaxTreeEntries(100)
	got := w.call("GET", "/repos/acme/files/git/trees/main?recursive=1", "", nil).obj(t)
	entries := map[string]map[string]any{}
	for _, e := range got["tree"].([]any) {
		m := e.(map[string]any)
		entries[m["path"].(string)] = m
	}
	for path, want := range map[string][2]string{
		"run.sh": {ModeExecutable, "blob"}, "CLAUDE.md": {ModeSymlink, "blob"}, "vendor/lib": {ModeGitlink, "commit"},
		"docs": {"040000", "tree"}, "docs/guide.md": {ModeFile, "blob"},
	} {
		e := entries[path]
		if e == nil || e["mode"] != want[0] || e["type"] != want[1] {
			t.Errorf("%s: %v, want %v", path, e, want)
		}
	}
	if entries["CLAUDE.md"]["size"] != float64(len("AGENTS.md")) || entries["vendor/lib"]["url"] != nil || entries["vendor/lib"]["size"] != nil {
		t.Errorf("entries: %v %v", entries["CLAUDE.md"], entries["vendor/lib"])
	}
	if got["truncated"] != false {
		t.Errorf("truncated %v", got["truncated"])
	}
	w.s.SetMaxTreeEntries(3)
	got = w.call("GET", "/repos/acme/files/git/trees/"+w.s.Branch("acme/files", "main")+"?recursive=1", "", nil).obj(t)
	if got["truncated"] != true || len(got["tree"].([]any)) != 3 {
		t.Errorf("truncated: %v", got["truncated"])
	}
	top := w.call("GET", "/repos/acme/files/git/trees/main", "", nil).obj(t)
	for _, e := range top["tree"].([]any) {
		if strings.Contains(field(e, "path").(string), "/") {
			t.Errorf("non-recursive listing has %v", field(e, "path"))
		}
	}
	blob := entries["docs/guide.md"]["sha"].(string)
	b := w.call("GET", "/repos/acme/files/git/blobs/"+blob, "", nil).obj(t)
	if raw, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(b["content"].(string), "\n", "")); string(raw) != "guide\n" {
		t.Errorf("blob: %v", b)
	}
}

func TestRules(t *testing.T) {
	w := newWorld(t, Options{})
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Name: "default", Include: []string{"~DEFAULT_BRANCH"},
		Rules: []Rule{{Type: RuleRequiredSignatures}, {Type: RuleDeletion}}}))
	sync := must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Name: "sync", Include: []string{"refs/heads/touchmark/**"},
		Exclude: []string{"refs/heads/touchmark/free"}, Rules: []Rule{{Type: RuleNonFastForward}}, BypassApps: []string{"hub-writer"}}))
	must[int64](t)(w.s.AddRuleset("acme/api", Ruleset{Name: "trial", Enforcement: "evaluate",
		Rules: []Rule{{Type: RuleCreation}}}))
	must[int64](t)(w.s.AddOrgRuleset("acme", Ruleset{Name: "org", Include: []string{"refs/heads/touchmark/*"},
		Rules: []Rule{{Type: RulePullRequest, Parameters: map[string]any{"required_approving_review_count": 1}}}}))
	tok := w.token(nil, Permissions{"metadata": Read})
	types := func(branch string) []string {
		var out []string
		for _, item := range w.call("GET", "/repos/acme/api/rules/branches/"+branch, tok, nil).list(t) {
			out = append(out, field(item, "type").(string)+"@"+field(item, "ruleset_source_type").(string))
		}
		return out
	}
	if got := strings.Join(types("main"), ","); got != "required_signatures@Repository,deletion@Repository" {
		t.Errorf("main: %s", got)
	}
	// The branch need not exist; "*" stays within a segment, "**" does not.
	if got := strings.Join(types("touchmark/hub"), ","); got != "non_fast_forward@Repository,pull_request@Organization" {
		t.Errorf("touchmark/hub: %s", got)
	}
	if got := strings.Join(types("touchmark/a/b"), ","); got != "non_fast_forward@Repository" {
		t.Errorf("touchmark/a/b: %s", got)
	}
	if got := types("touchmark/free"); strings.Join(got, ",") != "pull_request@Organization" {
		t.Errorf("excluded: %v", got)
	}
	item := w.call("GET", "/repos/acme/api/rules/branches/main", "", nil).list(t)[0]
	if field(item, "ruleset_source") != "acme/api" || field(item, "ruleset_id") == nil || field(item, "parameters") != nil {
		t.Errorf("rule item: %v", item)
	}
	org := w.call("GET", "/repos/acme/api/rules/branches/touchmark/hub", "", nil).list(t)[1]
	if field(org, "ruleset_source") != "acme" || field(org, "parameters", "required_approving_review_count") != float64(1) {
		t.Errorf("org rule: %v", org)
	}
	list := w.call("GET", "/repos/acme/api/rulesets", tok, nil).list(t)
	if len(list) != 4 {
		t.Errorf("rulesets: %d", len(list))
	}
	full := w.call("GET", "/repos/acme/api/rulesets/"+itoa(sync), tok, nil).obj(t)
	if full["current_user_can_bypass"] != "always" || field(full, "conditions", "ref_name", "include", 0) != "refs/heads/touchmark/**" {
		t.Errorf("ruleset: %v", full)
	}
	if got := w.call("GET", "/repos/acme/api/rulesets/"+itoa(sync), "", nil).obj(t); got["current_user_can_bypass"] != "never" {
		t.Errorf("anonymous bypass: %v", got["current_user_can_bypass"])
	}
	// Private repositories of free owners get no rulesets.
	must[Account](t)(w.s.AddOrg("small", PlanFree))
	w.repo(RepoSpec{Owner: "small", Name: "p", Visibility: "private"})
	must[int64](t)(w.s.AddRuleset("small/p", Ruleset{Rules: []Rule{{Type: RuleRequiredSignatures}}}))
	pat := must[string](t)(w.s.AddPAT("alice", PATSpec{Scopes: []string{"repo"}}))
	check(t, w.s.Grant("small/p", "alice", "write"))
	if got := w.call("GET", "/repos/small/p/rules/branches/main", pat, nil).list(t); len(got) != 0 {
		t.Errorf("free private: %v", got)
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/*", "refs/heads/main", true},
		{"refs/heads/*", "refs/heads/a/b", false},
		{"refs/heads/**", "refs/heads/a/b", true},
		{"refs/heads/**/*", "refs/heads/a/b", true},
		{"refs/heads/touchmark/**", "refs/heads/touchmark/hub", true},
		{"refs/heads/touchmark/**", "refs/heads/touch", false},
		{"refs/heads/release-?", "refs/heads/release-1", true},
		{"refs/heads/release-?", "refs/heads/release-10", false},
	} {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v", tc.pattern, tc.name, got)
		}
	}
}
