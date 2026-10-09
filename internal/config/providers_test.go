package config

import (
	"reflect"
	"strings"
	"testing"
)

// envOf returns a getenv over name, value pairs.
func envOf(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(name string) string { return m[name] }
}

// resolvedURLs is what TestResolveProviders compares for each provider.
type resolvedURLs struct {
	url, host, api, graphql, prefix string
}

func urlsOf(p ResolvedProvider) resolvedURLs {
	return resolvedURLs{url: p.URL, host: p.Host, api: p.APIURL, graphql: p.GraphQLURL, prefix: p.EnvPrefix}
}

func TestResolveProviders(t *testing.T) {
	hub := &Hub{ID: "acme-eng", Providers: []Provider{
		{ID: "gh", Type: "github"},
		{ID: "ghe", Type: "github", URL: "https://Acme.GHE.com/"},
		{ID: "ghes", Type: "github", URL: "https://github.example.com:8443/sub"},
		{ID: "ghes-api", Type: "github", URL: "https://ghes.example.com", APIURL: "https://ghes-api.example.com/api/v3/"},
		{ID: "gh-proxy", Type: "github", URL: "https://github.com", APIURL: "https://proxy.example.com/github"},
		{ID: "gl", Type: "gitlab", Sign: "always"},
		{ID: "corp-gl", Type: "gitlab", URL: "https://example.com/gitlab"},
		{ID: "gl-api", Type: "gitlab", URL: "https://gitlab.example.com", APIURL: "https://gl-api.example.com/v4"},
		{ID: "tea", Type: "gitea", URL: "https://gitea.example.com"},
		{ID: "cb", Type: "forgejo", URL: "https://codeberg.org"},
		{ID: "local", Type: "gitea", URL: "http://localhost:3000"},
		{ID: "bb", Type: "bitbucket"},
		{ID: "bb-url", Type: "bitbucket", URL: "https://Bitbucket.org/"},
		{ID: "bb-test", Type: "bitbucket", URL: "http://localhost:8080", APIURL: "http://localhost:8080/2.0"},
		{ID: "ado", Type: "azure-devops", URL: "https://dev.azure.com/acme/"},
		{ID: "ado-test", Type: "azure-devops", URL: "http://localhost:8081/acme", APIURL: "http://localhost:8081/acme"},
	}}
	want := map[string]resolvedURLs{
		"gh":       {"https://github.com", "github.com", "https://api.github.com", "https://api.github.com/graphql", "TOUCHMARK_GH_"},
		"ghe":      {"https://Acme.GHE.com", "acme.ghe.com", "https://api.acme.ghe.com", "https://api.acme.ghe.com/graphql", "TOUCHMARK_GHE_"},
		"ghes":     {"https://github.example.com:8443/sub", "github.example.com:8443", "https://github.example.com:8443/sub/api/v3", "https://github.example.com:8443/sub/api/graphql", "TOUCHMARK_GHES_"},
		"ghes-api": {"https://ghes.example.com", "ghes.example.com", "https://ghes-api.example.com/api/v3", "https://ghes-api.example.com/api/graphql", "TOUCHMARK_GHES_API_"},
		"gh-proxy": {"https://github.com", "github.com", "https://proxy.example.com/github", "https://proxy.example.com/github/graphql", "TOUCHMARK_GH_PROXY_"},
		"gl":       {"https://gitlab.com", "gitlab.com", "https://gitlab.com/api/v4", "https://gitlab.com/api/graphql", "TOUCHMARK_GL_"},
		"corp-gl":  {"https://example.com/gitlab", "example.com", "https://example.com/gitlab/api/v4", "https://example.com/gitlab/api/graphql", "TOUCHMARK_CORP_GL_"},
		"gl-api":   {"https://gitlab.example.com", "gitlab.example.com", "https://gl-api.example.com/v4", "https://gitlab.example.com/api/graphql", "TOUCHMARK_GL_API_"},
		"tea":      {"https://gitea.example.com", "gitea.example.com", "https://gitea.example.com/api/v1", "", "TOUCHMARK_TEA_"},
		"cb":       {"https://codeberg.org", "codeberg.org", "https://codeberg.org/api/v1", "", "TOUCHMARK_CB_"},
		"local":    {"http://localhost:3000", "localhost:3000", "http://localhost:3000/api/v1", "", "TOUCHMARK_LOCAL_"},
		"bb":       {"https://bitbucket.org", "bitbucket.org", "https://api.bitbucket.org/2.0", "", "TOUCHMARK_BB_"},
		"bb-url":   {"https://Bitbucket.org", "bitbucket.org", "https://api.bitbucket.org/2.0", "", "TOUCHMARK_BB_URL_"},
		"bb-test":  {"http://localhost:8080", "localhost:8080", "http://localhost:8080/2.0", "", "TOUCHMARK_BB_TEST_"},
		"ado":      {"https://dev.azure.com/acme", "dev.azure.com", "https://dev.azure.com/acme", "", "TOUCHMARK_ADO_"},
		"ado-test": {"http://localhost:8081/acme", "localhost:8081", "http://localhost:8081/acme", "", "TOUCHMARK_ADO_TEST_"},
	}
	// The CI environment does not matter for declared providers, except
	// GITHUB_API_URL on the hub's own host (below).
	for _, getenv := range []func(string) string{
		envOf(),
		envOf("GITLAB_CI", "true", "CI_SERVER_URL", "https://gitlab.example.com"),
		envOf("GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://other.example.com", "GITHUB_API_URL", "https://other.example.com/api/v3"),
	} {
		got, err := hub.ResolveProviders(getenv)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(hub.Providers) {
			t.Fatalf("%d providers, want %d", len(got), len(hub.Providers))
		}
		for i, p := range got {
			if p.ID != hub.Providers[i].ID {
				t.Errorf("provider %d is %q, want hub.yml order", i, p.ID)
			}
			if u := urlsOf(p); u != want[p.ID] {
				t.Errorf("%s:\n got  %+v\n want %+v", p.ID, u, want[p.ID])
			}
			if p.Short || p.Implicit {
				t.Errorf("%s: Short %v, Implicit %v", p.ID, p.Short, p.Implicit)
			}
		}
		if got[0].Sign != "auto" || got[5].Sign != "always" {
			t.Errorf("sign = %q, %q; want the default auto and the explicit always", got[0].Sign, got[5].Sign)
		}
	}
	if hub.Providers[0].URL != "" || hub.Providers[3].APIURL != "https://ghes-api.example.com/api/v3/" {
		t.Error("ResolveProviders changed the hub")
	}
}

func TestResolveProvidersActionsURLs(t *testing.T) {
	hub := &Hub{Providers: []Provider{
		{ID: "ghe", Type: "github", URL: "https://acme.ghe.com"},
		{ID: "gh", Type: "github"},
		{ID: "ghe-explicit", Type: "github", URL: "https://ACME.ghe.com", APIURL: "https://api2.acme.ghe.com"},
		{ID: "same-host", Type: "gitea", URL: "https://acme.ghe.com"},
	}}
	actions := envOf(
		"GITHUB_ACTIONS", "true",
		"GITHUB_SERVER_URL", "https://acme.ghe.com/",
		"GITHUB_API_URL", "https://api.acme.ghe.com/",
		"GITHUB_GRAPHQL_URL", "https://api.acme.ghe.com/graphql2",
	)
	got, err := hub.ResolveProviders(actions)
	if err != nil {
		t.Fatal(err)
	}
	want := []resolvedURLs{
		{"https://acme.ghe.com", "acme.ghe.com", "https://api.acme.ghe.com", "https://api.acme.ghe.com/graphql2", "TOUCHMARK_GHE_"},
		{"https://github.com", "github.com", "https://api.github.com", "https://api.github.com/graphql", "TOUCHMARK_GH_"},
		{"https://ACME.ghe.com", "acme.ghe.com", "https://api2.acme.ghe.com", "https://api2.acme.ghe.com/graphql", "TOUCHMARK_GHE_EXPLICIT_"},
		{"https://acme.ghe.com", "acme.ghe.com", "https://acme.ghe.com/api/v1", "", "TOUCHMARK_SAME_HOST_"},
	}
	for i, p := range got {
		if u := urlsOf(p); u != want[i] {
			t.Errorf("%s:\n got  %+v\n want %+v", p.ID, u, want[i])
		}
	}

	// Without GITHUB_GRAPHQL_URL, GraphQL follows the API URL.
	ghes := &Hub{Providers: []Provider{{ID: "ghes", Type: "github", URL: "https://ghes.example.com"}}}
	got, err = ghes.ResolveProviders(envOf(
		"GITHUB_ACTIONS", "true",
		"GITHUB_SERVER_URL", "https://ghes.example.com",
		"GITHUB_API_URL", "https://ghes.example.com/api/v3",
	))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].APIURL != "https://ghes.example.com/api/v3" || got[0].GraphQLURL != "https://ghes.example.com/api/graphql" {
		t.Errorf("ghes: %+v", urlsOf(got[0]))
	}

	// Gitea and Forgejo runners set GITHUB_* too; their API URL is never
	// taken for a GitHub provider.
	gh := &Hub{Providers: []Provider{{ID: "gh", Type: "github"}}}
	for _, marker := range []string{"GITEA_ACTIONS", "FORGEJO_ACTIONS"} {
		got, err = gh.ResolveProviders(envOf(
			marker, "true", "GITHUB_ACTIONS", "true",
			"GITHUB_SERVER_URL", "https://github.com",
			"GITHUB_API_URL", "https://evil.example.com",
		))
		if err != nil || got[0].APIURL != "https://api.github.com" {
			t.Errorf("%s: %+v, %v", marker, got, err)
		}
	}

	// A malformed GITHUB_SERVER_URL makes no provider the hub's own.
	got, err = gh.ResolveProviders(envOf(
		"GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "github.com",
		"GITHUB_API_URL", "https://evil.example.com",
	))
	if err != nil || got[0].APIURL != "https://api.github.com" {
		t.Errorf("malformed server URL: %+v, %v", got, err)
	}

	// A malformed URL from the environment is an error that does not quote
	// it.
	for _, tc := range []struct{ name, value string }{
		{"GITHUB_API_URL", "http://api.github.com"},
		{"GITHUB_GRAPHQL_URL", "https://user:s3cret-value@api.github.com/graphql"},
	} {
		_, err := gh.ResolveProviders(envOf(
			"GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://github.com", tc.name, tc.value,
		))
		if err == nil || !strings.Contains(err.Error(), tc.name) || strings.Contains(err.Error(), tc.value) {
			t.Errorf("%s=%s: %v", tc.name, tc.value, err)
		}
	}
}

func TestResolveProvidersImplicit(t *testing.T) {
	base := ResolvedProvider{Short: true, Implicit: true}
	implicit := func(id, url, host, api, graphql string) ResolvedProvider {
		p := base
		p.Provider = Provider{ID: id, Type: id, URL: url, Sign: "auto"}
		p.Host, p.APIURL, p.GraphQLURL, p.EnvPrefix = host, api, graphql, EnvPrefix(id)
		return p
	}
	tests := []struct {
		name string
		env  func(string) string
		want ResolvedProvider
	}{
		{
			name: "GitHub Actions",
			env:  envOf("GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://github.com", "GITHUB_API_URL", "https://api.github.com"),
			want: implicit("github", "https://github.com", "github.com", "https://api.github.com", "https://api.github.com/graphql"),
		},
		{
			name: "GitHub Enterprise Server",
			env:  envOf("GITHUB_ACTIONS", "TRUE", "GITHUB_SERVER_URL", " https://ghes.example.com/ "),
			want: implicit("github", "https://ghes.example.com", "ghes.example.com", "https://ghes.example.com/api/v3", "https://ghes.example.com/api/graphql"),
		},
		{
			name: "GHE.com with the runner's URLs",
			env: envOf("GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://acme.ghe.com",
				"GITHUB_API_URL", "https://api.acme.ghe.com", "GITHUB_GRAPHQL_URL", "https://api.acme.ghe.com/graphql"),
			want: implicit("github", "https://acme.ghe.com", "acme.ghe.com", "https://api.acme.ghe.com", "https://api.acme.ghe.com/graphql"),
		},
		{
			name: "GitLab CI",
			env:  envOf("GITLAB_CI", "true", "CI_SERVER_URL", "https://gitlab.example.com:8443"),
			want: implicit("gitlab", "https://gitlab.example.com:8443", "gitlab.example.com:8443", "https://gitlab.example.com:8443/api/v4", "https://gitlab.example.com:8443/api/graphql"),
		},
		{
			name: "Gitea Actions",
			env:  envOf("GITEA_ACTIONS", "true", "GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://gitea.example.com", "GITHUB_API_URL", "https://gitea.example.com/api/v1"),
			want: implicit("gitea", "https://gitea.example.com", "gitea.example.com", "https://gitea.example.com/api/v1", ""),
		},
		{
			name: "Forgejo Actions",
			env:  envOf("FORGEJO_ACTIONS", "true", "GITEA_ACTIONS", "true", "GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://codeberg.org"),
			want: implicit("forgejo", "https://codeberg.org", "codeberg.org", "https://codeberg.org/api/v1", ""),
		},
	}
	legacy, _, err := ParseHub(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		for _, hub := range []*Hub{nil, legacy, {ID: "acme-eng"}} {
			got, err := hub.ResolveProviders(tt.env)
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
				continue
			}
			if !reflect.DeepEqual(got, []ResolvedProvider{tt.want}) {
				t.Errorf("%s:\n got  %+v\n want %+v", tt.name, got, tt.want)
			}
		}
	}

	// A hub built in code with the top-level writer and sign.
	hub := &Hub{ID: "acme-eng", Writer: "acme-bot", Sign: "always"}
	got, err := hub.ResolveProviders(envOf("GITLAB_CI", "true", "CI_SERVER_URL", "https://gitlab.com"))
	if err != nil || got[0].Writer != "acme-bot" || got[0].Sign != "always" {
		t.Errorf("writer and sign: %+v, %v", got, err)
	}

	// A hub built in code with the whole shorthand is not implicit, whatever
	// the CI says.
	hub = &Hub{ID: "acme-eng", Platform: "gitlab", BaseURL: "https://gitlab.example.com/", Writer: "acme-bot"}
	got, err = hub.ResolveProviders(envOf("GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://github.com"))
	want := ResolvedProvider{
		Provider:   Provider{ID: "gitlab", Type: "gitlab", URL: "https://gitlab.example.com", Writer: "acme-bot", Sign: "auto"},
		Host:       "gitlab.example.com",
		APIURL:     "https://gitlab.example.com/api/v4",
		GraphQLURL: "https://gitlab.example.com/api/graphql",
		EnvPrefix:  "TOUCHMARK_GITLAB_",
		Short:      true,
	}
	if err != nil || !reflect.DeepEqual(got, []ResolvedProvider{want}) {
		t.Errorf("shorthand built in code:\n got  %+v, %v\n want %+v", got, err, want)
	}
}

func TestResolveProvidersErrors(t *testing.T) {
	tests := []struct {
		name    string
		hub     *Hub
		env     func(string) string
		want    string
		secrets []string // must not appear in the message
	}{
		{
			name: "no providers outside CI",
			hub:  &Hub{ID: "acme-eng"},
			env:  envOf("CI", "true", "GITHUB_ACTIONS", "1"),
			want: "hub.yml declares no providers, no CI environment names one (GitHub Actions, GitLab CI, Gitea or Forgejo Actions), and the hub's origin remote is not on github.com",
		},
		{
			name: "no server URL",
			hub:  &Hub{ID: "acme-eng"},
			env:  envOf("GITHUB_ACTIONS", "true"),
			want: "GITHUB_SERVER_URL is not set; add providers to hub.yml",
		},
		{
			name:    "a server URL with credentials",
			hub:     &Hub{ID: "acme-eng"},
			env:     envOf("GITLAB_CI", "true", "CI_SERVER_URL", "https://gitlab-ci-token:t0ps3cret@gitlab.example.com"),
			want:    "CI_SERVER_URL is not an https:// URL",
			secrets: []string{"t0ps3cret"},
		},
		{
			name: "an http server URL",
			hub:  &Hub{ID: "acme-eng"},
			env:  envOf("GITEA_ACTIONS", "true", "GITHUB_SERVER_URL", "http://gitea.internal"),
			want: "GITHUB_SERVER_URL is not an https:// URL",
		},
		{
			name: "bad id",
			hub:  &Hub{Providers: []Provider{{ID: "GH", Type: "github"}}},
			want: `hub.yml: providers[0]: id: provider id "GH"`,
		},
		{
			name: "bad type",
			hub:  &Hub{Providers: []Provider{{ID: "srht", Type: "sourcehut"}}},
			want: `hub.yml: providers[0]: type: "sourcehut" must be one of github, gitlab, gitea, forgejo, bitbucket, azure-devops`,
		},
		{
			name: "azure-devops without url",
			hub:  &Hub{Providers: []Provider{{ID: "ado", Type: "azure-devops"}}},
			want: "hub.yml: providers[0]: url: required for azure-devops",
		},
		{
			name: "azure-devops at a project",
			hub:  &Hub{Providers: []Provider{{ID: "ado", Type: "azure-devops", URL: "https://dev.azure.com/acme/Billing"}}},
			want: "hub.yml: providers[0]: url: a provider of type azure-devops is one organization of Azure DevOps Services",
		},
		{
			name: "azure-devops elsewhere without api_url",
			hub:  &Hub{Providers: []Provider{{ID: "ado", Type: "azure-devops", URL: "https://ado.example.com/acme"}}},
			want: "hub.yml: providers[0]: url: a provider of type azure-devops is one organization",
		},
		{
			name: "bitbucket elsewhere without api_url",
			hub:  &Hub{Providers: []Provider{{ID: "bb", Type: "bitbucket", URL: "https://bitbucket.example.com"}}},
			want: "hub.yml: providers[0]: url: a provider of type bitbucket is Bitbucket Cloud, at https://bitbucket.org (leave url out); Bitbucket Data Center is not supported yet",
		},
		{
			name: "bitbucket under a path",
			hub:  &Hub{Providers: []Provider{{ID: "bb", Type: "bitbucket", URL: "https://bitbucket.org/acme"}}},
			want: "hub.yml: providers[0]: url: a provider of type bitbucket is Bitbucket Cloud",
		},
		{
			name: "gitea without url",
			hub:  &Hub{Providers: []Provider{{ID: "gh", Type: "github"}, {ID: "tea", Type: "gitea"}}},
			want: "hub.yml: providers[1]: url: required for gitea",
		},
		{
			name: "http url",
			hub:  &Hub{Providers: []Provider{{ID: "gl", Type: "gitlab", URL: "http://gitlab.example.com"}}},
			want: `hub.yml: providers[0]: url: "http://gitlab.example.com" must be an https:// URL`,
		},
		{
			name: "bad api_url",
			hub:  &Hub{Providers: []Provider{{ID: "gl", Type: "gitlab", APIURL: "https://gitlab.com/api/v4?x=1"}}},
			want: "hub.yml: providers[0]: api_url:",
		},
		{
			name: "duplicate ids",
			hub:  &Hub{Providers: []Provider{{ID: "gh", Type: "github"}, {ID: "gh", Type: "gitlab"}}},
			want: `hub.yml: providers[1]: duplicate provider id "gh"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.env
			if env == nil {
				env = envOf()
			}
			got, err := tt.hub.ResolveProviders(env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveProviders = %+v, %v; want an error with %q", got, err, tt.want)
			}
			for _, s := range tt.secrets {
				if strings.Contains(err.Error(), s) {
					t.Errorf("the error quotes %q: %v", s, err)
				}
			}
		})
	}
}

// TestResolveProvidersParsed: a parsed hub resolves its providers,
// including the single-provider shorthand, whatever the CI.
func TestResolveProvidersParsed(t *testing.T) {
	actions := envOf("GITHUB_ACTIONS", "true", "GITHUB_SERVER_URL", "https://github.com")
	h := mustParseHub(t, readFixture(t, "hub/valid/shorthand-gitea.yml"))
	got, err := h.ResolveProviders(actions)
	if err != nil {
		t.Fatal(err)
	}
	want := []ResolvedProvider{{
		Provider:  Provider{ID: "gitea", Type: "gitea", URL: "https://git.example.com", Writer: "touchmark-bot", Sign: "always"},
		Host:      "git.example.com",
		APIURL:    "https://git.example.com/api/v1",
		EnvPrefix: "TOUCHMARK_GITEA_",
		Short:     true,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shorthand:\n got  %+v\n want %+v", got, want)
	}

	h = mustParseHub(t, readFixture(t, "hub/valid/full.yml"))
	got, err = h.ResolveProviders(actions)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "gh" || got[1].ID != "corp" || got[0].Short || got[1].Short {
		t.Fatalf("providers = %+v", got)
	}
	if got[1].APIURL != "https://gitlab.example.com:8443/gitlab/api/v4" || got[1].GraphQLURL != "https://gitlab.example.com:8443/gitlab/api/graphql" ||
		got[1].Host != "gitlab.example.com:8443" || got[1].CAFile != "certs/corp-ca.pem" || got[1].EnvPrefix != "TOUCHMARK_CORP_" {
		t.Errorf("corp = %+v", got[1])
	}
	if !reflect.DeepEqual(got[0].Provider, h.Providers[0]) {
		t.Errorf("gh = %+v, want the parsed provider %+v", got[0].Provider, h.Providers[0])
	}
}

// TestResolveProvidersOrigin: outside CI, a hub without providers (the
// README's hub.yml: id and writer) takes its provider from the origin
// remote of a public instance; a self-managed one needs providers. In CI
// the environment decides.
func TestResolveProvidersOrigin(t *testing.T) {
	h := mustParseHub(t, readFixture(t, "hub/valid/shorthand-writer.yml"))
	if len(h.Providers) != 0 || h.Writer != "acme-assets-write[bot]" || h.Sign != "always" {
		t.Fatalf("parsed %+v", h)
	}
	implicit := func(id, url, host, api, graphql string) ResolvedProvider {
		return ResolvedProvider{
			Provider: Provider{ID: id, Type: id, URL: url, Writer: "acme-assets-write[bot]", Sign: "always"},
			Host:     host, APIURL: api, GraphQLURL: graphql, EnvPrefix: EnvPrefix(id), Short: true, Implicit: true,
		}
	}
	for _, tc := range []struct {
		origin string
		want   ResolvedProvider
	}{
		{"github.com", implicit("github", "https://github.com", "github.com", "https://api.github.com", "https://api.github.com/graphql")},
		{"GitHub.com", implicit("github", "https://github.com", "github.com", "https://api.github.com", "https://api.github.com/graphql")},
		{"acme.ghe.com", implicit("github", "https://acme.ghe.com", "acme.ghe.com", "https://api.acme.ghe.com", "https://api.acme.ghe.com/graphql")},
		{"gitlab.com", implicit("gitlab", "https://gitlab.com", "gitlab.com", "https://gitlab.com/api/v4", "https://gitlab.com/api/graphql")},
	} {
		got, err := h.ResolveProvidersWithOrigin(envOf(), tc.origin)
		if err != nil || !reflect.DeepEqual(got, []ResolvedProvider{tc.want}) {
			t.Errorf("origin %s:\n got  %+v, %v\n want %+v", tc.origin, got, err, tc.want)
		}
	}
	for _, origin := range []string{"", "gitlab.example.com", "github.example.com", "ghe.com", "a..ghe.com", "github.com-work"} {
		if got, err := h.ResolveProvidersWithOrigin(envOf(), origin); err == nil || !strings.Contains(err.Error(), "add providers to hub.yml") {
			t.Errorf("origin %q: %+v, %v; want an error asking for providers", origin, got, err)
		}
	}
	// In CI the environment decides.
	got, err := h.ResolveProvidersWithOrigin(envOf("GITLAB_CI", "true", "CI_SERVER_URL", "https://gitlab.example.com"), "github.com")
	if err != nil || len(got) != 1 || got[0].Type != "gitlab" || got[0].URL != "https://gitlab.example.com" || got[0].Writer != "acme-assets-write[bot]" {
		t.Errorf("CI over origin: %+v, %v", got, err)
	}
	// Declared providers ignore the origin.
	decl := mustParseHub(t, readFixture(t, "hub/valid/full.yml"))
	if got, err := decl.ResolveProvidersWithOrigin(envOf(), "gitlab.com"); err != nil || len(got) != 2 || got[0].ID != "gh" {
		t.Errorf("declared providers: %+v, %v", got, err)
	}
}

// TestCanonicalFingerprint: every spelling of one hub gives one form.
func TestCanonicalFingerprint(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/712345678":         "github.com/712345678",
		"GitHub.com/712345678":         "github.com/712345678",
		"gitlab.example.com:443/1234":  "gitlab.example.com/1234",
		"gitlab.example.com:80/1234":   "gitlab.example.com/1234",
		"gitlab.example.com:8443/1234": "gitlab.example.com:8443/1234",
		"gitlab.example.com/01234":     "gitlab.example.com/1234",
		"GitLab.Example.com:443/007":   "gitlab.example.com/7",
		"host/0":                       "host/0",
		"github.com":                   "github.com",
		"":                             "",
	} {
		if got := CanonicalFingerprint(in); got != want {
			t.Errorf("CanonicalFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
	h := mustParseHub(t, []byte("version: 1\nid: acme-eng\nprevious_fingerprints: [GitLab.example.com:443/01234, github.com/1]\n"))
	if want := []string{"gitlab.example.com/1234", "github.com/1"}; !reflect.DeepEqual(h.PreviousFingerprints, want) {
		t.Errorf("previous_fingerprints = %q, want %q", h.PreviousFingerprints, want)
	}
}

// TestResolveProvidersProcessEnv: a nil getenv reads the process
// environment.
func TestResolveProvidersProcessEnv(t *testing.T) {
	for _, name := range []string{"GITHUB_ACTIONS", "GITEA_ACTIONS", "FORGEJO_ACTIONS"} {
		t.Setenv(name, "")
	}
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_SERVER_URL", "https://gitlab.example.com")
	got, err := (&Hub{ID: "acme-eng"}).ResolveProviders(nil)
	if err != nil || len(got) != 1 || got[0].ID != "gitlab" || got[0].URL != "https://gitlab.example.com" {
		t.Errorf("ResolveProviders(nil) = %+v, %v", got, err)
	}
}

func TestEnvPrefix(t *testing.T) {
	for id, want := range map[string]string{
		"gh":       "TOUCHMARK_GH_",
		"github":   "TOUCHMARK_GITHUB_",
		"corp-gl":  "TOUCHMARK_CORP_GL_",
		"a1-b2-c3": "TOUCHMARK_A1_B2_C3_",
		"x":        "TOUCHMARK_X_",
	} {
		if got := EnvPrefix(id); got != want {
			t.Errorf("EnvPrefix(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSelectFor(t *testing.T) {
	targets := &Targets{
		Defaults: Defaults{Packs: []string{"agents"}},
		Targets: []Entry{
			{Repo: "acme/billing", Packs: []string{"python-service"}},
			{Org: "acme", Packs: []string{"claude"}},
			{Group: "acme/platform", Packs: []string{"gitlab", "python"}},
			{Org: "acme", Topics: []string{"docs"}},
		},
	}
	optIn := &OptIn{Packs: []string{"claude"}}
	wantSel := Selection{
		Packs:    []string{"agents", "python-service", "gitlab", "claude"},
		Complete: true,
		Sources: map[string][]string{
			"agents":         {"defaults", "requires python-service", "requires claude"},
			"python-service": {"targets.yml"},
			"gitlab":         {"targets.yml"},
			"claude":         {".engineering-assets.yml"},
		},
	}
	for _, matched := range [][]int{{0, 2}, {2, 0}, {2, 0, 2, 0}} {
		sel, warns, err := SelectFor(selectHub("gh"), targets, optIn, matched, knownPacks())
		if err != nil {
			t.Fatalf("%v: %v", matched, err)
		}
		if !reflect.DeepEqual(sel, wantSel) {
			t.Errorf("%v:\n got  %+v\n want %+v", matched, sel, wantSel)
		}
		want := []Warning{{File: TargetsFile, Message: `pack "python" was renamed to "python-service"; using "python-service"`}}
		if !reflect.DeepEqual(warns, want) {
			t.Errorf("%v: warnings %v, want %v", matched, warns, want)
		}
	}

	for _, tc := range []struct {
		name    string
		matched []int
		optIn   *OptIn
		want    []string
	}{
		{"no entries", nil, optIn, []string{"agents", "claude"}},
		{"an entry without packs", []int{3}, nil, []string{"agents"}},
		{"an org entry", []int{1, 3}, &OptIn{Packs: []string{"gitlab"}}, []string{"agents", "claude", "gitlab"}},
	} {
		sel, warns, err := SelectFor(selectHub(), targets, tc.optIn, tc.matched, knownPacks())
		if err != nil || !reflect.DeepEqual(sel.Packs, tc.want) || !sel.Complete || sel.Unresolved != nil || len(warns) > 0 {
			t.Errorf("%s: %+v, %v, %v; want packs %q", tc.name, sel, warns, err, tc.want)
		}
	}

	for _, tc := range []struct {
		name    string
		hub     *Hub
		targets *Targets
		optIn   *OptIn
		matched []int
		want    string
	}{
		{"index past the end", nil, targets, nil, []int{0, 4}, "targets.yml: targets[4] does not exist: the file has 4 entries"},
		{"negative index", nil, targets, nil, []int{-1}, "targets[-1] does not exist"},
		{"index without targets", nil, nil, nil, []int{0}, "the file has 0 entries"},
		{"unknown pack in an entry", nil, &Targets{Targets: []Entry{{Org: "acme", Packs: []string{"rust"}}}}, nil, []int{0}, `unknown pack "rust" (from targets.yml)`},
		{"unknown pack in defaults", nil, &Targets{Defaults: Defaults{Packs: []string{"go"}}}, nil, nil, `unknown pack "go" (from defaults)`},
		{"unknown pack in the opt-in file", &Hub{OptInFile: ".github/assets.yml"}, nil, &OptIn{Packs: []string{"rust"}}, nil, `unknown pack "rust" (from .github/assets.yml)`},
		{
			"requires cycle",
			&Hub{Packs: map[string]PackMeta{"agents": {Requires: []string{"claude"}}, "claude": {Requires: []string{"agents"}}}},
			nil, &OptIn{Packs: []string{"claude"}}, nil,
			"requires cycle: claude -> agents -> claude",
		},
	} {
		hub := tc.hub
		if hub == nil {
			hub = selectHub()
		}
		if _, _, err := SelectFor(hub, tc.targets, tc.optIn, tc.matched, knownPacks()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}

	sel, warns, err := SelectFor(nil, nil, nil, nil, nil)
	if err != nil || !reflect.DeepEqual(sel, Selection{Packs: []string{}, Complete: true, Sources: map[string][]string{}}) || len(warns) > 0 {
		t.Errorf("all nil: %+v, %v, %v", sel, warns, err)
	}
}

// TestSelectForMatchesSelect: for repo entries, which Select evaluates
// locally, SelectFor with the matching entries gives the same selection.
func TestSelectForMatchesSelect(t *testing.T) {
	hub := selectHub("gh")
	targets := &Targets{
		Defaults: Defaults{Packs: []string{"gitlab"}},
		Targets: []Entry{
			{Repo: "acme/billing", Packs: []string{"python"}},
			{Repo: "acme/other", Packs: []string{"python-library"}},
			{Repo: "gh:Acme/Billing", Packs: []string{"claude", "gitlab"}},
		},
	}
	optIn := &OptIn{Packs: []string{"agents", "python-library"}}
	want, wantWarns, err := Select(hub, targets, optIn, ref(t, "acme/billing"), knownPacks())
	if err != nil {
		t.Fatal(err)
	}
	got, warns, err := SelectFor(hub, targets, optIn, []int{0, 2}, knownPacks())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(warns, wantWarns) {
		t.Errorf("SelectFor = %+v, %v\nSelect    = %+v, %v", got, warns, want, wantWarns)
	}
}

func TestOptInHash(t *testing.T) {
	// The expected values are sha256 of the canonical form written out by
	// hand (printf ... | sha256sum), so they pin the form documented on Hash.
	const (
		empty      = "sha256:30e55501db0d69e2eacd8c683b38f517a65230d7627732f2b4aaedaf2f63604c" // "touchmark-optin/v1\nversion 1\n"
		claude     = "sha256:baad60f2cace5239242fc31de906495bdfd4e21a8629223087b177a9ec6a1a69" // + "pack 6:claude\n"
		documented = "sha256:6d10fe0a880c6cb1c134abc2f3f465e2ad755d81b1a9db34eca9c5e86e80c626" // + "ignore 21:.agents/guidelines/**\nignore 21:.claude/settings.json\n"
		two        = "sha256:82593c8b5735ffd2640a700d424e7ee4852a70064723380f407f8e6d7955bf63" // "pack 6:agents\npack 6:claude\n"
		nl         = "sha256:de2595845a1b2ac3af43a40dc997f83113d6432c7c776923866bc2f0804f216b" // "ignore 3:a\nb\n"
		ab         = "sha256:6440ee73aaed31d625fb7536d288db75a82ff5a18d1ae2fc074889cb9c24d623" // "ignore 1:a\nignore 1:b\n"
		v2         = "sha256:c250f6b90489371a80b1aa155f011a44ded6519b9c711ca71f7c17b35227d0f8" // "version 2\n"
	)
	var none *OptIn
	for _, tc := range []struct {
		name string
		o    *OptIn
		want string
	}{
		{"nil", none, empty},
		{"zero", &OptIn{}, empty},
		{"legacy", &OptIn{Version: 1, Legacy: true}, empty},
		{"empty lists", &OptIn{Version: 1, Packs: []string{}, Ignore: []string{}}, empty},
		{"a pack", &OptIn{Packs: []string{"claude"}}, claude},
		{"documented example", &OptIn{Version: 1, Packs: []string{"claude"}, Ignore: []string{".agents/guidelines/**", ".claude/settings.json"}}, documented},
		{"sorted and deduplicated", &OptIn{Packs: []string{"claude", "agents", "claude"}}, two},
		{"a pattern with a newline", &OptIn{Ignore: []string{"a\nb"}}, nl},
		{"two patterns", &OptIn{Ignore: []string{"b", "a", "./a/", " b "}}, ab},
		{"empty patterns are dropped", &OptIn{Ignore: []string{"a", "b", "./", " / "}}, ab},
		{"version 2", &OptIn{Version: 2}, v2},
	} {
		if got := tc.o.Hash(); got != tc.want {
			t.Errorf("%s: Hash() = %s, want %s", tc.name, got, tc.want)
		}
	}

	// Comments, order, formatting, duplicates and the version key do not
	// change the hash of a parsed file.
	same := []string{
		"version: 1\npacks: [claude]\nignore:\n  - .claude/settings.json\n  - .agents/guidelines/**\n",
		"# The team's choice.\nignore: ['./.agents/guidelines/**', '.claude/settings.json/', .claude/settings.json]\npacks:\n  - claude   # reviewed\n  - claude\n",
		"version: 1\r\nignore: [\".agents\\\\guidelines\\\\**\", \" .claude/settings.json \"]\r\npacks: [claude]\r\n",
	}
	for _, text := range same {
		o, _, err := ParseOptIn([]byte(text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if got := o.Hash(); got != documented {
			t.Errorf("%q: Hash() = %s, want %s", text, got, documented)
		}
	}
	o, _, err := ParseOptIn(readFixture(t, "opt-in/valid/example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Hash(); got != documented {
		t.Errorf("example.yml: %s, want %s", got, documented)
	}

	// Adding or removing a pack or a pattern, and case, change it.
	seen := map[string]string{}
	for _, text := range []string{
		"",
		"packs: [claude]\n",
		"packs: [claude, agents]\n",
		"packs: [claude]\nignore: [docs/**]\n",
		"packs: [claude]\nignore: [docs/**, AGENTS.md]\n",
		"packs: [claude]\nignore: [Docs/**]\n",
		"ignore: [docs/**]\n",
		"packs: [docs]\n",
	} {
		o, _, err := ParseOptIn([]byte(text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		h := o.Hash()
		if prev, dup := seen[h]; dup {
			t.Errorf("%q and %q hash the same", text, prev)
		}
		seen[h] = text
		if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
			t.Errorf("%q: malformed hash %q", text, h)
		}
	}

	// Hash does not change the opt-in it reads.
	in := &OptIn{Packs: []string{"b", "a"}, Ignore: []string{"./z", "y"}}
	in.Hash()
	if !reflect.DeepEqual(in, &OptIn{Packs: []string{"b", "a"}, Ignore: []string{"./z", "y"}}) {
		t.Errorf("Hash changed its receiver: %+v", in)
	}
}
