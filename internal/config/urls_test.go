package config

import (
	"reflect"
	"strings"
	"testing"
)

// urlHub declares providers at the root of a host, under a path, on a
// port, and on the loopback host over http.
const urlHub = `version: 1
id: acme-eng
providers:
  - id: gh
    type: github
  - id: corp
    type: gitlab
    url: https://gitlab.example.com
  - id: sub
    type: gitlab
    url: https://example.com/GitLab/
  - id: tea
    type: gitea
    url: http://localhost:3000
  - id: ghe
    type: github
    url: https://ghe.example.com:8443
`

// resolvedProviders parses hub.yml and resolves its providers without an
// environment.
func resolvedProviders(t *testing.T, yml string) []ResolvedProvider {
	t.Helper()
	hub := mustParseHub(t, []byte(yml))
	rps, err := hub.ResolveProviders(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return rps
}

func TestResolveURLs(t *testing.T) {
	provs := resolvedProviders(t, urlHub)
	for _, tc := range []struct {
		entry Entry
		want  string // the resolved value, or a substring of the error after "!"
	}{
		{Entry{Repo: "https://github.com/acme/billing"}, "gh:acme/billing"},
		{Entry{Repo: "https://GitHub.com:443/Acme/Billing.git/"}, "gh:Acme/Billing"},
		{Entry{Repo: "https://github.com/acme/billing.git.git"}, "gh:acme/billing.git"},
		{Entry{Org: "https://github.com/acme/"}, "gh:acme"},
		{Entry{Repo: "https://gitlab.example.com/platform/sub/api"}, "corp:platform/sub/api"},
		{Entry{Group: "https://gitlab.example.com/platform/sub/"}, "corp:platform/sub"},
		{Entry{Group: "https://gitlab.example.com/x.git"}, "corp:x.git"},
		{Entry{Repo: "https://example.com/gitlab/team/app"}, "sub:team/app"},
		{Entry{Repo: "http://localhost:3000/acme/tools"}, "tea:acme/tools"},
		{Entry{Repo: "https://ghe.example.com:8443/acme/x"}, "ghe:acme/x"},
		{Entry{Repo: "https://github.com/acme/x", Provider: "gh"}, "gh:acme/x"},
		// No provider there: another host, port, scheme or path.
		{Entry{Repo: "https://bitbucket.org/acme/x"}, "!is not under the url of any provider of hub.yml (gh at https://github.com, corp at https://gitlab.example.com"},
		{Entry{Repo: "https://ghe.example.com/acme/x"}, "!is not under the url of any provider"},
		{Entry{Repo: "https://localhost:3000/acme/x"}, "!is not under the url of any provider"},
		{Entry{Repo: "https://example.com/gitlabx/team/app"}, "!is not under the url of any provider"},
		// The provider's own URL names no repository.
		{Entry{Org: "https://example.com/gitlab/"}, "!is not under the url of any provider"},
		// The path after the provider's url is too short, or has the wrong shape.
		{Entry{Repo: "https://example.com/gitlab/team"}, "!a repository path needs an owner and a name"},
		{Entry{Repo: "https://github.com/acme/billing/tree/main"}, "!a github repository URL names owner/name"},
		{Entry{Org: "https://github.com/acme/billing"}, "!a github organisation URL names one owner"},
		{Entry{Org: "http://localhost:3000/acme/x"}, "!a gitea organisation URL names one owner"},
		{Entry{Repo: "https://gitlab.example.com/platform/api/-/merge_requests"}, "!the /-/ segment points inside a GitLab project"},
		// The entry's provider must be where the URL points.
		{Entry{Repo: "https://github.com/acme/x", Provider: "corp"}, "!is not under the url of provider corp (https://gitlab.example.com)"},
		// A URL in the wrong form.
		{Entry{Repo: "http://github.com/acme/x"}, `!"http://github.com/acme/x" is not a repository URL`},
	} {
		in := &Targets{Targets: []Entry{tc.entry}}
		got, err := ResolveURLs(in, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%+v: error %v, want one with %q", tc.entry, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", tc.entry, err)
			continue
		}
		if v := selectorValue(&got.Targets[0]); v != tc.want {
			t.Errorf("%+v: %q, want %q", tc.entry, v, tc.want)
		}
		if !reflect.DeepEqual(in.Targets[0], tc.entry) {
			t.Errorf("%+v: the input changed to %+v", tc.entry, in.Targets[0])
		}
	}
}

// TestResolveURLsExclude: exclude entries, patterns included; the values
// that are no URLs stay as they are, and every URL that fails is reported
// with its field.
func TestResolveURLsExclude(t *testing.T) {
	provs := resolvedProviders(t, urlHub)
	in := &Targets{
		Defaults: Defaults{Provider: "gh", Packs: []string{"agents"}},
		Targets:  []Entry{{Repo: "acme/billing"}, {Org: "https://github.com/acme", Match: []string{"acme/svc-*"}}},
		Exclude: []string{
			"acme/legacy",
			"https://gitlab.example.com/platform/legacy/**",
			"https://github.com/acme/legacy-*",
			"https://github.com/acme/old/",
		},
	}
	got, err := ResolveURLs(in, provs)
	if err != nil {
		t.Fatal(err)
	}
	want := &Targets{
		Defaults: Defaults{Provider: "gh", Packs: []string{"agents"}},
		Targets:  []Entry{{Repo: "acme/billing"}, {Org: "gh:acme", Match: []string{"acme/svc-*"}}},
		Exclude:  []string{"acme/legacy", "corp:platform/legacy/**", "gh:acme/legacy-*", "gh:acme/old"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveURLs =\n%+v\nwant\n%+v", got, want)
	}
	if got.HasURLs() || !in.HasURLs() {
		t.Error("HasURLs is wrong for the result or the input")
	}
	if _, errs := Check(&Hub{ID: "acme-eng", Providers: []Provider{{ID: "gh"}, {ID: "corp"}}}, got, knownPacks()); len(errs) > 0 {
		t.Errorf("Check of the result: %v", errs)
	}

	bad := &Targets{
		Targets: []Entry{{Repo: "https://bitbucket.org/a/b"}, {Repo: "acme/x"}, {Group: "https://gitlab.example.com/a/-/b"}},
		Exclude: []string{"https://github.com/a/*/-", "https://jdoe:hunter2@github.com/acme/x", "https://gitlab.example.com/a/-/*"},
	}
	_, err = ResolveURLs(bad, provs)
	msgs := flattenErr(err)
	wantFields := []string{"targets.yml: targets[0].repo:", "targets.yml: targets[2].group:", "targets.yml: exclude[0]:", "targets.yml: exclude[1]:", "targets.yml: exclude[2]:"}
	if len(msgs) != len(wantFields) {
		t.Fatalf("errors %q, want %d", msgs, len(wantFields))
	}
	for i, f := range wantFields {
		if !strings.HasPrefix(msgs[i], f) {
			t.Errorf("error %q, want it to start with %q", msgs[i], f)
		}
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "jdoe") {
		t.Errorf("the error shows the credential of a URL: %v", err)
	}

	// Nothing to resolve: the same targets come back.
	plain := &Targets{Targets: []Entry{{Repo: "acme/x"}}}
	if got, err := ResolveURLs(plain, nil); got != plain || err != nil {
		t.Errorf("ResolveURLs without URLs = %p, %v; want the input", got, err)
	}
	if got, err := ResolveURLs(nil, provs); got != nil || err != nil {
		t.Errorf("ResolveURLs(nil) = %v, %v", got, err)
	}
}

// TestResolveURLsExcludeShape: an exclude URL copied from a page inside a
// repository of a platform without nested namespaces (GitHub's /tree/main,
// Gitea's /src/branch/main) names no repository and would exclude nothing,
// so it is refused, as for repo:; a "**" may span nothing, so it may stand
// in a longer path. A pattern ending in .git is refused: dropping .git, as
// a repository URL may end in it, would widen the pattern.
func TestResolveURLsExcludeShape(t *testing.T) {
	provs := resolvedProviders(t, urlHub)
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/acme/legacy/tree/main", "!a github repository URL names owner/name"},
		{"https://github.com/acme/legacy/tree/main/**", "gh:acme/legacy/tree/main/**"},
		{"https://github.com/acme/**/legacy", "gh:acme/**/legacy"},
		{"http://localhost:3000/acme/legacy/src/branch/main", "!so it points inside a repository and would exclude nothing"},
		{"https://gitlab.example.com/platform/legacy/api", "corp:platform/legacy/api"},
		{"https://github.com/acme/legacy.git", "gh:acme/legacy"},
		{"https://github.com/acme/*.git", "!a pattern whose last segment ends in .git; write it without .git"},
		{"https://gitlab.example.com/platform/**/*.GIT", "!ends in .git"},
	} {
		got, err := ResolveURLs(&Targets{Exclude: []string{tc.url}}, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %v, want one with %q", tc.url, err, want)
			}
			continue
		}
		if err != nil || got.Exclude[0] != tc.want {
			t.Errorf("%s: %v, %v; want %q", tc.url, got, err, tc.want)
		}
	}
}

// flattenErr splits a joined error into its messages.
func flattenErr(err error) []string {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range j.Unwrap() {
			out = append(out, flattenErr(e)...)
		}
		return out
	}
	return []string{err.Error()}
}

// TestResolveURLsAmbiguous: providers whose urls a URL lies beneath alike
// are an error, unless the entry names one, or defaults.provider is one of
// them (for exclude entries too).
func TestResolveURLsAmbiguous(t *testing.T) {
	provs := resolvedProviders(t, `version: 1
id: acme-eng
providers:
  - {id: gh, type: github}
  - {id: gh-app, type: github, url: https://github.com/}
  - {id: tea, type: gitea, url: https://example.com}
  - {id: lab, type: gitlab, url: https://example.com/gitlab}
`)
	for _, tc := range []struct {
		targets *Targets
		want    string
	}{
		{&Targets{Targets: []Entry{{Repo: "https://github.com/acme/x"}}}, "is under the url of providers gh at https://github.com, gh-app at https://github.com"},
		{&Targets{Exclude: []string{"https://github.com/acme/x"}}, "name one with provider: or defaults.provider, or write the target as <provider>:<path>"},
		// defaults.provider picks only among the providers the URL lies under.
		{&Targets{Defaults: Defaults{Provider: "tea"}, Targets: []Entry{{Repo: "https://github.com/acme/x"}}}, "is under the url of providers gh at"},
		{&Targets{Targets: []Entry{{Repo: "https://example.com/gitlab/team/app"}}}, "providers tea at https://example.com, lab at https://example.com/gitlab"},
	} {
		if _, err := ResolveURLs(tc.targets, provs); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: error %v, want one with %q", tc.targets, err, tc.want)
		}
	}
	got, err := ResolveURLs(&Targets{Targets: []Entry{
		{Repo: "https://github.com/acme/x", Provider: "gh-app"},
		{Repo: "https://example.com/gitlab/team/app", Provider: "lab"},
		{Repo: "https://example.com/acme/tools", Provider: "tea"},
	}}, provs)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"gh-app:acme/x", "lab:team/app", "tea:acme/tools"} {
		if got.Targets[i].Repo != want {
			t.Errorf("targets[%d] = %q, want %q", i, got.Targets[i].Repo, want)
		}
	}
	// defaults.provider picks one for entries that name none, and for
	// exclude entries; an entry's provider still wins.
	got, err = ResolveURLs(&Targets{
		Defaults: Defaults{Provider: "gh-app"},
		Targets:  []Entry{{Repo: "https://github.com/acme/x"}, {Repo: "https://github.com/acme/y", Provider: "gh"}, {Repo: "https://example.com/acme/tools"}},
		Exclude:  []string{"https://github.com/acme/legacy-*"},
	}, provs)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"gh-app:acme/x", "gh:acme/y", "tea:acme/tools"} {
		if got.Targets[i].Repo != want {
			t.Errorf("with defaults.provider: targets[%d] = %q, want %q", i, got.Targets[i].Repo, want)
		}
	}
	if got.Exclude[0] != "gh-app:acme/legacy-*" {
		t.Errorf("with defaults.provider: exclude[0] = %q", got.Exclude[0])
	}
	// gitea has no nested namespaces: tea cannot hold gitlab/team/app.
	if _, err := ResolveURLs(&Targets{Targets: []Entry{{Repo: "https://example.com/gitlab/team/app", Provider: "tea"}}}, provs); err == nil ||
		!strings.Contains(err.Error(), "a gitea repository URL names owner/name") {
		t.Errorf("tea with three segments: %v", err)
	}
}

// TestResolveURLsImplicit: the implicit provider, from the CI or the hub's
// origin, is the hub's only one: its targets are bare paths, which Check
// accepts with no providers in hub.yml.
func TestResolveURLsImplicit(t *testing.T) {
	hub := mustParseHub(t, []byte("version: 1\nid: acme-eng\nwriter: acme-write[bot]\n"))
	env := map[string]string{"GITLAB_CI": "true", "CI_SERVER_URL": "https://gitlab.example.com/"}
	rps, err := hub.ResolveProviders(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	targets, _, err := ParseTargets([]byte("version: 1\ntargets:\n  - group: https://gitlab.example.com/platform\nexclude:\n  - https://gitlab.example.com/platform/legacy/**\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveURLs(targets, rps)
	if err != nil {
		t.Fatal(err)
	}
	if got.Targets[0].Group != "platform" || got.Exclude[0] != "platform/legacy/**" {
		t.Errorf("resolved %+v", got)
	}
	if warns, errs := Check(hub, got, knownPacks()); len(errs) > 0 || len(warns) > 0 {
		t.Errorf("Check: %v, %v", errs, warns)
	}
	if _, errs := Check(hub, targets, knownPacks()); len(errs) != 1 || !strings.Contains(errs[0].Error(), "not resolved to providers") {
		t.Errorf("Check of unresolved URLs: %v", errs)
	}
}

func TestDisplayURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/x":               "https://github.com/acme/x",
		"https://jdoe:hunter2@github.com/acme/x":  "https://…@github.com/acme/x",
		"https://a@b@github.com":                  "https://…@github.com",
		"https://github.com/acme/x?mail=me@x.com": "https://github.com/acme/x?mail=me@x.com",
		"acme/x":                          "acme/x",
		"gh:acme/x":                       "gh:acme/x",
		"ssh://git@github.com:acme/x.git": "ssh://…@github.com:acme/x.git",
		"https://token@github.com#frag@not-userinfo": "https://…@github.com#frag@not-userinfo",
	} {
		if got := displayURL(in); got != want {
			t.Errorf("displayURL(%q) = %q, want %q", in, got, want)
		}
	}
	// Every message of the parsers cuts it too, wherever the URL is.
	if got := cleanMessage(`hub.yml: url: "https://jdoe:hunter2@gitlab.example.com" must be`); strings.Contains(got, "hunter2") {
		t.Errorf("cleanMessage kept the password: %q", got)
	}
	if _, _, err := ParseHub([]byte("version: 1\nid: acme-eng\nproviders:\n  - {id: corp, type: gitlab, url: \"https://jdoe:hunter2@gitlab.example.com\"}\n")); err == nil ||
		strings.Contains(err.Error(), "hunter2") {
		t.Errorf("ParseHub error shows the password: %v", err)
	}
	if _, _, err := ParseTargets([]byte("version: 1\ntargets:\n  - repo: https://jdoe:hunter2@github.com/acme/x\n")); err == nil ||
		strings.Contains(err.Error(), "hunter2") {
		t.Errorf("ParseTargets error shows the password: %v", err)
	}
}

// TestRefRefusesURL: where a URL is not accepted (operations.yml, --only,
// a URL left unresolved), the message says so.
func TestRefRefusesURL(t *testing.T) {
	if _, err := ParseRef("https://github.com/acme/x"); err == nil || !strings.Contains(err.Error(), "a URL is not accepted here") {
		t.Errorf("ParseRef of a URL: %v", err)
	}
	if _, err := Selectors(nil, &Targets{Targets: []Entry{{Repo: "https://github.com/acme/x"}}}); err == nil ||
		!strings.Contains(err.Error(), "a URL is not accepted here") {
		t.Errorf("Selectors of a URL: %v", err)
	}
}

// TestResolveURLsBitbucket: a Bitbucket Cloud provider takes the web URLs of
// bitbucket.org, workspace/repository and workspace only.
func TestResolveURLsBitbucket(t *testing.T) {
	provs := resolvedProviders(t, `version: 1
id: acme-eng
providers:
  - id: bb
    type: bitbucket
  - id: gh
    type: github
`)
	for _, tc := range []struct {
		entry Entry
		want  string // the resolved value, or a substring of the error after "!"
	}{
		{Entry{Repo: "https://bitbucket.org/acme/billing"}, "bb:acme/billing"},
		{Entry{Repo: "https://bitbucket.org/acme/billing.git"}, "bb:acme/billing"},
		{Entry{Org: "https://bitbucket.org/acme/"}, "bb:acme"},
		{Entry{Repo: "https://bitbucket.org/acme/billing/src/main/README.md"}, "!a bitbucket repository URL names owner/name"},
		{Entry{Org: "https://bitbucket.org/acme/billing"}, "!a bitbucket organisation URL names one owner"},
		{Entry{Repo: "https://api.bitbucket.org/acme/billing"}, "!is not under the url of any provider"},
	} {
		got, err := ResolveURLs(&Targets{Targets: []Entry{tc.entry}}, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%+v: error %v, want one with %q", tc.entry, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", tc.entry, err)
			continue
		}
		if v := selectorValue(&got.Targets[0]); v != tc.want {
			t.Errorf("%+v: %q, want %q", tc.entry, v, tc.want)
		}
	}
}

// TestResolveURLsAzureDevOps: an Azure DevOps provider is one organization;
// its repository URLs are <project>/_git/<repository>, and the organization
// URL itself is the namespace of an org entry.
func TestResolveURLsAzureDevOps(t *testing.T) {
	provs := resolvedProviders(t, `version: 1
id: acme-eng
providers:
  - id: ado
    type: azure-devops
    url: https://dev.azure.com/acme
  - id: other
    type: azure-devops
    url: https://dev.azure.com/fabrikam
`)
	for _, tc := range []struct {
		entry Entry
		want  string // the resolved value, or a substring of the error after "!"
	}{
		{Entry{Repo: "https://dev.azure.com/acme/Billing/_git/api"}, "ado:Billing/api"},
		{Entry{Repo: "https://dev.azure.com/ACME/Billing/_git/api/"}, "ado:Billing/api"},
		{Entry{Repo: "https://dev.azure.com/fabrikam/Web/_git/site"}, "other:Web/site"},
		{Entry{Org: "https://dev.azure.com/acme"}, "ado:acme"},
		{Entry{Org: "https://dev.azure.com/acme/"}, "ado:acme"},
		{Entry{Org: "https://dev.azure.com/acme/Billing"}, "!an Azure DevOps org entry names the whole organization"},
		{Entry{Repo: "https://dev.azure.com/acme/Billing/api"}, "!an Azure DevOps repository URL is https://dev.azure.com/<organization>/<project>/_git/<repository>"},
		{Entry{Repo: "https://dev.azure.com/acme/Billing/_git/api/pullrequest/3"}, "!an Azure DevOps repository URL is"},
		{Entry{Repo: "https://dev.azure.com/contoso/Billing/_git/api"}, "!is not under the url of any provider"},
	} {
		got, err := ResolveURLs(&Targets{Targets: []Entry{tc.entry}}, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%+v: error %v, want one with %q", tc.entry, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", tc.entry, err)
			continue
		}
		if v := selectorValue(&got.Targets[0]); v != tc.want {
			t.Errorf("%+v: %q, want %q", tc.entry, v, tc.want)
		}
	}
	for _, tc := range []struct{ ex, want string }{
		{"https://dev.azure.com/acme/Legacy/_git/old", "ado:Legacy/old"},
		{"https://dev.azure.com/acme/Legacy/_git/old-*", "ado:Legacy/old-*"},
		{"https://dev.azure.com/acme/Legacy/*", "ado:Legacy/*"},
		{"https://dev.azure.com/acme/Legacy/**", "ado:Legacy/**"},
		{"https://dev.azure.com/acme/Legacy/old", "!or a pattern of a project's repositories"},
	} {
		got, err := ResolveURLs(&Targets{Exclude: []string{tc.ex}}, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("exclude %s: error %v, want one with %q", tc.ex, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("exclude %s: %v", tc.ex, err)
			continue
		}
		if got.Exclude[0] != tc.want {
			t.Errorf("exclude %s: %q, want %q", tc.ex, got.Exclude[0], tc.want)
		}
	}
}

// TestResolveURLsBitbucketDataCenter: a Bitbucket Data Center provider is
// one instance, often under a context path; its repository URLs are
// /projects/<KEY>/repos/<slug> (with /browse… after them in a browser) or
// clone URLs, /scm/<key>/<slug>.git, and a project's URL is the namespace
// of an org entry.
func TestResolveURLsBitbucketDataCenter(t *testing.T) {
	provs := resolvedProviders(t, `version: 1
id: acme-eng
providers:
  - id: bbdc
    type: bitbucket-datacenter
    url: https://git.example.com/bitbucket
`)
	for _, tc := range []struct {
		entry Entry
		want  string // the resolved value, or a substring of the error after "!"
	}{
		{Entry{Repo: "https://git.example.com/bitbucket/projects/ACME/repos/api"}, "bbdc:ACME/api"},
		{Entry{Repo: "https://git.example.com/bitbucket/projects/ACME/repos/api/browse"}, "bbdc:ACME/api"},
		{Entry{Repo: "https://git.example.com/bitbucket/projects/ACME/repos/api/browse/src/main.go"}, "bbdc:ACME/api"},
		{Entry{Repo: "https://git.example.com/bitbucket/scm/acme/api.git"}, "bbdc:acme/api"},
		{Entry{Org: "https://git.example.com/bitbucket/projects/ACME"}, "bbdc:ACME"},
		{Entry{Org: "https://git.example.com/bitbucket/projects/ACME/"}, "bbdc:ACME"},
		{Entry{Org: "https://git.example.com/bitbucket/ACME"}, "!a Bitbucket Data Center org entry names a project, <url>/projects/<KEY>"},
		{Entry{Repo: "https://git.example.com/bitbucket/ACME/api"}, "!a Bitbucket Data Center repository URL is <url>/projects/<KEY>/repos/<repository>"},
		{Entry{Repo: "https://git.example.com/bitbucket/projects/ACME/repos/api/pull-requests/3"}, "!a Bitbucket Data Center repository URL is"},
		{Entry{Repo: "https://git.example.com/bitbucket/users/jane/repos/notes"}, "!a Bitbucket Data Center repository URL is"},
		{Entry{Repo: "https://git.example.com/projects/ACME/repos/api"}, "!is not under the url of any provider"},
	} {
		got, err := ResolveURLs(&Targets{Targets: []Entry{tc.entry}}, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%+v: error %v, want one with %q", tc.entry, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", tc.entry, err)
			continue
		}
		if v := selectorValue(&got.Targets[0]); v != tc.want {
			t.Errorf("%+v: %q, want %q", tc.entry, v, tc.want)
		}
	}
	for _, tc := range []struct{ ex, want string }{
		{"https://git.example.com/bitbucket/projects/LEGACY/repos/old", "bbdc:LEGACY/old"},
		{"https://git.example.com/bitbucket/projects/LEGACY/repos/old-*", "bbdc:LEGACY/old-*"},
		{"https://git.example.com/bitbucket/projects/LEGACY/repos/*", "bbdc:LEGACY/*"},
		{"https://git.example.com/bitbucket/scm/legacy/old.git", "bbdc:legacy/old"},
		{"https://git.example.com/bitbucket/projects/LEGACY/repos/old/browse", "!points inside a repository and would exclude nothing"},
		{"https://git.example.com/bitbucket/projects/LEGACY", "!a Bitbucket Data Center repository URL is"},
	} {
		got, err := ResolveURLs(&Targets{Exclude: []string{tc.ex}}, provs)
		if want, ok := strings.CutPrefix(tc.want, "!"); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("exclude %s: error %v, want one with %q", tc.ex, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("exclude %s: %v", tc.ex, err)
			continue
		}
		if got.Exclude[0] != tc.want {
			t.Errorf("exclude %s: %q, want %q", tc.ex, got.Exclude[0], tc.want)
		}
	}
}
