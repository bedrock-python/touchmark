package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// envPrefixBase starts the name of every touchmark variable; alone it is
// the prefix of the short names a hub with one provider accepts.
const envPrefixBase = "TOUCHMARK_"

// Bitbucket Cloud: the web URL, which is the default url of a provider of
// type bitbucket, and its REST base. Bitbucket Data Center has another API
// and is not a bitbucket provider.
const (
	bitbucketURL    = "https://bitbucket.org"
	bitbucketAPIURL = "https://api.bitbucket.org/2.0"
)

// bitbucketURLError is the complaint about a bitbucket provider at another
// url without an api_url.
const bitbucketURLError = "a provider of type bitbucket is Bitbucket Cloud, at https://bitbucket.org (leave url out); " +
	"Bitbucket Data Center is not supported yet. Elsewhere (a test server) set api_url too"

// isBitbucketCloud reports whether raw, a checked URL, is Bitbucket Cloud's
// web URL: https://bitbucket.org, with no path.
func isBitbucketCloud(raw string) bool {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, "bitbucket.org") && u.Path == ""
}

// Azure DevOps Services: the host of every organization's web URL and REST
// base, https://dev.azure.com/<organization>. A provider of type
// azure-devops is one organization; its url names it.
const azureDevOpsHost = "dev.azure.com"

// azureDevOpsURLError is the complaint about an azure-devops provider whose
// url is not an organization of Azure DevOps Services.
const azureDevOpsURLError = "a provider of type azure-devops is one organization of Azure DevOps Services: " +
	"url is https://dev.azure.com/<organization> (one provider per organization; the old " +
	"https://<organization>.visualstudio.com form and Azure DevOps Server are not supported). " +
	"Elsewhere (a test server) set api_url too, and keep the organization as the url's only path segment"

// azureOrgRe matches an organization name of Azure DevOps Services:
// letters, digits and hyphens, starting and ending with a letter or digit.
var azureOrgRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,48}[A-Za-z0-9])?$`)

// AzureDevOpsOrg returns the organization an azure-devops provider's url
// names, its only path segment ("acme" of https://dev.azure.com/acme); ok is
// false when the url has another shape.
func AzureDevOpsOrg(raw string) (org string, ok bool) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return "", false
	}
	org = strings.TrimPrefix(u.Path, "/")
	if !azureOrgRe.MatchString(org) {
		return "", false
	}
	return org, true
}

// azureDevOpsURLOK reports whether raw, a checked URL, can be an
// azure-devops provider's url: one organization segment, and, without an
// api_url, on https://dev.azure.com.
func azureDevOpsURLOK(raw string, hasAPIURL bool) bool {
	if _, ok := AzureDevOpsOrg(raw); !ok {
		return false
	}
	if hasAPIURL {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, azureDevOpsHost)
}

// ResolvedProvider is a provider from hub.yml (or the implicit one) with
// everything a driver needs besides credentials.
type ResolvedProvider struct {
	Provider
	// Host is the lowercased host of URL, with a port when URL has one.
	Host string
	// APIURL is the REST base: https://api.github.com, https://api.<host>
	// for *.ghe.com, <url>/api/v3 for GitHub Enterprise Server,
	// <url>/api/v4 for GitLab, <url>/api/v1 for Gitea and Forgejo,
	// https://api.bitbucket.org/2.0 for Bitbucket Cloud, the url itself
	// (https://dev.azure.com/<organization>) for Azure DevOps;
	// Provider.APIURL when set. On GitHub Actions for the hub's own host,
	// GITHUB_API_URL wins over the derived URL, not over Provider.APIURL:
	// an explicit api_url means the same in CI and locally.
	APIURL string
	// GraphQLURL is set for GitHub (https://api.github.com/graphql,
	// <url>/api/graphql for GHES, GITHUB_GRAPHQL_URL on Actions) and GitLab
	// (<url>/api/graphql); "" elsewhere. Otherwise it follows APIURL:
	// .../api/v3 and .../api/v4 become .../api/graphql, another GitHub API
	// base gets /graphql appended (https://api.<host>/graphql for
	// *.ghe.com), and another GitLab one falls back to <url>/api/graphql.
	GraphQLURL string
	// EnvPrefix is the prefix of the provider's variables, EnvPrefix(ID):
	// "TOUCHMARK_<ID>_" with the id uppercased and '-' mapped to '_'. With
	// Short set, the short names under "TOUCHMARK_" are read too when the
	// prefixed ones are absent (auth.FromEnv(EnvPrefix, Short, …)).
	EnvPrefix string
	// Short is set when the hub has exactly one provider, so the short
	// variable names (TOUCHMARK_READ_TOKEN, …) are accepted too.
	Short bool
	// Implicit is set when the provider came from the CI environment or the
	// hub's origin remote, not from hub.yml.
	Implicit bool
}

// ResolveProviders returns the hub's providers in hub.yml order.
//
// With no providers declared (and no shorthand with platform), the single
// provider is implicit: GitHub Actions (GITHUB_SERVER_URL; type github, or
// ghes when the host is not github.com and not *.ghe.com — both "github"
// type here), GitLab CI (CI_SERVER_URL; gitlab), Gitea/Forgejo Actions
// (GITEA_ACTIONS or FORGEJO_ACTIONS with GITHUB_SERVER_URL; gitea or
// forgejo), else an error asking for providers in hub.yml
// (ResolveProvidersWithOrigin also reads the hub's origin remote outside
// CI). Provider ids of implicit providers are the type ("github",
// "gitlab", …), and they take the top-level writer and sign.
//
// The CI is told by GITHUB_ACTIONS, GITLAB_CI, GITEA_ACTIONS and
// FORGEJO_ACTIONS set to "true", checked in the order Forgejo, Gitea,
// GitHub, GitLab: Forgejo and Gitea runners set GITHUB_ACTIONS too. The
// single-provider shorthand with platform needs nothing here: ParseHub
// folds it into Providers. A hub built in code gets the same treatment:
// with platform set and no providers, the shorthand is its provider; with
// only the top-level writer and sign, the implicit provider takes them.
//
// Every provider is checked again, with ParseHub's defaults, so a hub built
// in code resolves like a parsed one: a valid id, no duplicate ids, a known
// type, the public URL of github, gitlab and bitbucket when URL is empty,
// an api_url for a bitbucket provider anywhere but Bitbucket Cloud, the url
// of an organization for azure-devops (an api_url anywhere but
// dev.azure.com), https URLs
// (http only for localhost), sign auto. A nil hub is a legacy hub. Error
// messages never quote environment values. A nil getenv reads the process
// environment.
func (h *Hub) ResolveProviders(getenv func(string) string) ([]ResolvedProvider, error) {
	return h.ResolveProvidersWithOrigin(getenv, "")
}

// ResolveProvidersWithOrigin is ResolveProviders for a hub whose origin
// remote is on originHost ("" when unknown). Outside CI, a hub without
// providers takes its implicit provider from that host when it is a public
// instance whose type the host tells: github.com and *.ghe.com (github at
// https://<host>), gitlab.com (gitlab). Any other host is a self-managed
// instance, whose type only hub.yml can tell: the error asks for providers
// there. In CI the environment decides, whatever the origin.
func (h *Hub) ResolveProvidersWithOrigin(getenv func(string) string, originHost string) ([]ResolvedProvider, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	env := readCIEnv(getenv)
	var providers []Provider
	implicit := false
	switch {
	case h != nil && len(h.Providers) > 0:
		providers = h.Providers
	case h != nil && h.Platform != "":
		// The shorthand of a hub built in code, folded as ParseHub does.
		providers = []Provider{{ID: h.Platform, Type: h.Platform, URL: h.BaseURL, Writer: h.Writer, Sign: h.Sign}}
	default:
		p, err := implicitProvider(h, env, originHost)
		if err != nil {
			return nil, err
		}
		providers, implicit = []Provider{p}, true
	}
	out := make([]ResolvedProvider, 0, len(providers))
	seen := map[string]bool{}
	for i, p := range providers {
		label := fmt.Sprintf("%s: providers[%d]", HubFile, i)
		if implicit {
			label = fmt.Sprintf("the implicit %s provider", p.Type)
		}
		rp, err := resolveProvider(p, env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, cleanErr(err))
		}
		if seen[rp.ID] {
			return nil, fmt.Errorf("%s: duplicate provider id %q", label, rp.ID)
		}
		seen[rp.ID] = true
		rp.Short = len(providers) == 1
		rp.Implicit = implicit
		out = append(out, rp)
	}
	return out, nil
}

// ciEnv is what the CI environment says about the hub's own platform.
type ciEnv struct {
	// kind is the provider type of the CI: "github", "gitlab", "gitea",
	// "forgejo", or "" outside CI.
	kind string
	// serverVar names the variable with the platform's URL, and serverURL
	// is its value, trimmed.
	serverVar, serverURL string
	// On GitHub Actions only: the lowercased host (with port) of
	// GITHUB_SERVER_URL, and GITHUB_API_URL and GITHUB_GRAPHQL_URL without
	// a trailing slash.
	actionsHost, apiURL, graphqlURL string
}

func readCIEnv(getenv func(string) string) ciEnv {
	isTrue := func(name string) bool { return strings.EqualFold(strings.TrimSpace(getenv(name)), "true") }
	var env ciEnv
	switch {
	case isTrue("FORGEJO_ACTIONS"):
		env.kind, env.serverVar = "forgejo", "GITHUB_SERVER_URL"
	case isTrue("GITEA_ACTIONS"):
		env.kind, env.serverVar = "gitea", "GITHUB_SERVER_URL"
	case isTrue("GITHUB_ACTIONS"):
		env.kind, env.serverVar = "github", "GITHUB_SERVER_URL"
	case isTrue("GITLAB_CI"):
		env.kind, env.serverVar = "gitlab", "CI_SERVER_URL"
	default:
		return env
	}
	env.serverURL = strings.TrimRight(strings.TrimSpace(getenv(env.serverVar)), "/")
	if env.kind == "github" {
		if u, err := url.Parse(env.serverURL); err == nil && checkURL(env.serverURL) == nil {
			env.actionsHost = strings.ToLower(u.Host)
		}
		env.apiURL = strings.TrimRight(strings.TrimSpace(getenv("GITHUB_API_URL")), "/")
		env.graphqlURL = strings.TrimRight(strings.TrimSpace(getenv("GITHUB_GRAPHQL_URL")), "/")
	}
	return env
}

// implicitProvider builds the provider of a hub without providers from the
// CI environment, or outside CI from the host of the hub's origin remote.
func implicitProvider(h *Hub, env ciEnv, originHost string) (Provider, error) {
	var p Provider
	switch {
	case env.kind != "":
		if env.serverURL == "" {
			return Provider{}, fmt.Errorf("%s declares no providers and %s is not set; add providers to %s", HubFile, env.serverVar, HubFile)
		}
		if checkURL(env.serverURL) != nil {
			return Provider{}, fmt.Errorf("%s declares no providers and %s is not an https:// URL without credentials, query or fragment; add providers to %s", HubFile, env.serverVar, HubFile)
		}
		p = Provider{ID: env.kind, Type: env.kind, URL: env.serverURL}
	default:
		var ok bool
		if p, ok = originProvider(originHost); !ok {
			return Provider{}, fmt.Errorf("%s declares no providers, no CI environment names one (GitHub Actions, GitLab CI, Gitea or Forgejo Actions), "+
				"and the hub's origin remote is not on github.com, a *.ghe.com host or gitlab.com; add providers to %s, or set platform and base_url there", HubFile, HubFile)
		}
	}
	if h != nil {
		p.Writer, p.Sign = h.Writer, h.Sign
	}
	return p, nil
}

// OriginTellsProvider reports whether a hub without providers can take its
// implicit provider outside CI from an origin remote on host: only a public
// instance (github.com, a *.ghe.com host, gitlab.com) tells its type.
func OriginTellsProvider(host string) bool {
	_, ok := originProvider(host)
	return ok
}

// originProvider returns the provider of a public instance at host, the
// host of a hub's origin remote: only there does the host tell the type.
func originProvider(host string) (Provider, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case host == "github.com":
		return Provider{ID: "github", Type: "github", URL: "https://github.com"}, true
	case strings.HasSuffix(host, ".ghe.com") && validHostname(host):
		return Provider{ID: "github", Type: "github", URL: "https://" + host}, true
	case host == "gitlab.com":
		return Provider{ID: "gitlab", Type: "gitlab", URL: "https://gitlab.com"}, true
	}
	return Provider{}, false
}

// validHostname reports whether host is a DNS name of letters, digits,
// hyphens and dots, as an https URL may carry it.
func validHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, c := range host {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// resolveProvider checks p, fills its defaults and derives its URLs.
func resolveProvider(p Provider, env ciEnv) (ResolvedProvider, error) {
	if err := checkProviderID(p.ID); err != nil {
		return ResolvedProvider{}, fmt.Errorf("id: %w", err)
	}
	if !slices.Contains(providerTypes, p.Type) {
		return ResolvedProvider{}, fmt.Errorf("type: %q must be one of %s", p.Type, strings.Join(providerTypes, ", "))
	}
	p.URL = strings.TrimRight(p.URL, "/")
	setDefault(&p.URL, defaultURLs[p.Type])
	if p.URL == "" {
		return ResolvedProvider{}, fmt.Errorf("url: required for %s", p.Type)
	}
	if err := checkURL(p.URL); err != nil {
		return ResolvedProvider{}, fmt.Errorf("url: %w", err)
	}
	p.APIURL = strings.TrimRight(p.APIURL, "/")
	if p.APIURL != "" {
		if err := checkURL(p.APIURL); err != nil {
			return ResolvedProvider{}, fmt.Errorf("api_url: %w", err)
		}
	}
	if p.Type == "bitbucket" && p.APIURL == "" && !isBitbucketCloud(p.URL) {
		return ResolvedProvider{}, fmt.Errorf("url: %s", bitbucketURLError)
	}
	if p.Type == "azure-devops" && !azureDevOpsURLOK(p.URL, p.APIURL != "") {
		return ResolvedProvider{}, fmt.Errorf("url: %s", azureDevOpsURLError)
	}
	setDefault(&p.Sign, defaultSign)
	u, err := url.Parse(p.URL)
	if err != nil {
		return ResolvedProvider{}, fmt.Errorf("url: %w", err)
	}
	rp := ResolvedProvider{Provider: p, Host: strings.ToLower(u.Host), EnvPrefix: EnvPrefix(p.ID)}
	// Actions sets GITHUB_API_URL for the server it runs on: it applies to
	// a GitHub provider on the hub's own host only.
	own := p.Type == "github" && env.actionsHost != "" && rp.Host == env.actionsHost
	rp.APIURL = p.APIURL
	if rp.APIURL == "" && own && env.apiURL != "" {
		if checkURL(env.apiURL) != nil {
			return ResolvedProvider{}, errors.New("GITHUB_API_URL is not an https:// URL without credentials, query or fragment")
		}
		rp.APIURL = env.apiURL
	}
	if rp.APIURL == "" {
		rp.APIURL = derivedAPIURL(p.Type, p.URL, strings.ToLower(u.Hostname()), rp.Host)
	}
	if p.APIURL == "" && own && env.graphqlURL != "" {
		if checkURL(env.graphqlURL) != nil {
			return ResolvedProvider{}, errors.New("GITHUB_GRAPHQL_URL is not an https:// URL without credentials, query or fragment")
		}
		rp.GraphQLURL = env.graphqlURL
	}
	if rp.GraphQLURL == "" {
		rp.GraphQLURL = derivedGraphQLURL(p.Type, p.URL, rp.APIURL)
	}
	return rp, nil
}

// derivedAPIURL returns the REST base of a provider of type typ at webURL,
// whose host is hostname without a port and host with one.
func derivedAPIURL(typ, webURL, hostname, host string) string {
	switch typ {
	case "github":
		switch {
		case hostname == "github.com":
			return "https://api.github.com"
		case strings.HasSuffix(hostname, ".ghe.com"):
			return "https://api." + host
		}
		return webURL + "/api/v3"
	case "gitlab":
		return webURL + "/api/v4"
	case "bitbucket":
		// resolveProvider asks for api_url anywhere else.
		return bitbucketAPIURL
	case "azure-devops":
		// The REST API of an organization is under its web URL,
		// https://dev.azure.com/<organization>/_apis.
		return webURL
	}
	return webURL + "/api/v1"
}

// derivedGraphQLURL returns the GraphQL endpoint that goes with the REST
// base api, or "" for platforms without one.
func derivedGraphQLURL(typ, webURL, api string) string {
	switch typ {
	case "github":
		if base, ok := strings.CutSuffix(api, "/api/v3"); ok {
			return base + "/api/graphql"
		}
		return api + "/graphql"
	case "gitlab":
		if base, ok := strings.CutSuffix(api, "/api/v4"); ok {
			return base + "/api/graphql"
		}
		return webURL + "/api/graphql"
	}
	return ""
}

// EnvPrefix returns "TOUCHMARK_<ID>_" for a provider id: uppercased, '-'
// mapped to '_'. Provider ids never contain '_', so the mapping is one to
// one.
func EnvPrefix(providerID string) string {
	return envPrefixBase + strings.ToUpper(strings.ReplaceAll(providerID, "-", "_")) + "_"
}

// SelectFor resolves the packs for a target that the platform resolved
// (delivery mode): defaults.packs, then the packs of the
// targets.yml entries at indexes matched (in file order), then optIn.Packs;
// duplicates keep their first position; requires expanded as in Select.
// The selection is always Complete. matched may be empty (a target found
// only through defaults is impossible, but an entry without packs is not).
//
// matched is read in file order whatever its order, and an index repeated
// counts once; an index outside targets.Targets is an error. Sources, former
// names (a warning, or unknown when the pack is gone), unknown packs and
// requires cycles are as in Select. Exclusion is not looked at: the caller
// drops excluded targets before selecting. Nil hub, targets and optIn are a
// legacy hub, an empty targets.yml and no opt-in packs.
func SelectFor(hub *Hub, targets *Targets, optIn *OptIn, matched []int, known map[string]bool) (Selection, []Warning, error) {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	if targets == nil {
		targets = &Targets{}
	}
	entries := slices.Clone(matched)
	slices.Sort(entries)
	entries = slices.Compact(entries)
	for _, i := range entries {
		if i < 0 || i >= len(targets.Targets) {
			return Selection{}, nil, fmt.Errorf("%s: targets[%d] does not exist: the file has %d entries", TargetsFile, i, len(targets.Targets))
		}
	}
	s := newSelection(hub, known)
	if err := s.add(targets.Defaults.Packs, sourceDefaults); err != nil {
		return Selection{}, s.warns, err
	}
	for _, i := range entries {
		if err := s.add(targets.Targets[i].Packs, sourceTargets); err != nil {
			return Selection{}, s.warns, err
		}
	}
	if optIn != nil {
		if err := s.add(optIn.Packs, s.optIn); err != nil {
			return Selection{}, s.warns, err
		}
	}
	packs, err := s.expand()
	if err != nil {
		return Selection{}, s.warns, err
	}
	return Selection{Packs: packs, Complete: true, Sources: s.sources}, s.warns, nil
}

// Hash returns the hash of the parsed opt-in file, "sha256:<64 hex>", over
// a canonical form: version (0 and absent count as 1), then packs and
// ignore sorted and deduplicated (ignore patterns after
// pathx.NormalizePattern). Comments, order and formatting do not change it;
// adding or removing a pack or an ignore pattern does, which lifts the
// target's declines (see docs/concepts/memory.md). A nil
// opt-in hashes like an empty file.
//
// The canonical form is these lines, each ending with "\n", where <n> is
// the length in bytes of the string after the colon, so a string can hold
// any byte without escaping:
//
//	touchmark-optin/v1
//	version <version, decimal>
//	pack <n>:<name>         one line per pack, sorted by bytes
//	ignore <n>:<pattern>    one line per normalized pattern, sorted by bytes
//
// Patterns that normalize to "" match nothing and are left out; pack names
// are taken as written. Legacy does not count: adding "version: 1" to a
// file without it changes nothing. Nor does Enabled: a file that says
// enabled: false is never compared, and one that says true chooses
// nothing new. For example, an empty file hashes
// "touchmark-optin/v1\nversion 1\n", and "packs: [claude]" appends
// "pack 6:claude\n".
func (o *OptIn) Hash() string {
	version := 1
	var packs, ignore []string
	if o != nil {
		if o.Version != 0 {
			version = o.Version
		}
		packs = slices.Clone(o.Packs)
		for _, pat := range o.Ignore {
			if n := pathx.NormalizePattern(pat); n != "" {
				ignore = append(ignore, n)
			}
		}
	}
	slices.Sort(packs)
	packs = slices.Compact(packs)
	slices.Sort(ignore)
	ignore = slices.Compact(ignore)
	h := sha256.New()
	fmt.Fprintf(h, "touchmark-optin/v1\nversion %d\n", version)
	for _, p := range packs {
		fmt.Fprintf(h, "pack %d:%s\n", len(p), p)
	}
	for _, p := range ignore {
		fmt.Fprintf(h, "ignore %d:%s\n", len(p), p)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
