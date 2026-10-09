package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// A value of targets[].repo, org or group, or of exclude, may be the web URL
// of a repository or a namespace, as a browser shows it:
// https://gitlab.example.com/platform/api, with or without a trailing
// slash or .git. ParseTargets checks its form; ResolveURLs finds its
// provider among the hub's by the provider's url and rewrites it as
// <provider>:<path>, the form every other part of touchmark reads.

// isURL reports whether a targets.yml value is a web URL rather than a
// [<provider>:]<path> reference.
func isURL(s string) bool { return strings.Contains(s, "://") }

// urlKind is what a URL in targets.yml names.
type urlKind int

const (
	urlRepo      urlKind = iota // targets[].repo: one repository
	urlNamespace                // targets[].org and group: a namespace
	urlExclude                  // exclude: a repository or a pattern
)

// checkTargetURL checks the form of a URL of kind k: https (http only for
// localhost), no credentials, query or fragment, path segments a repository
// path may hold (and, in exclude, the glob character *).
func checkTargetURL(s string, k urlKind) error {
	if len(s) > maxRefLen {
		return fmt.Errorf("%.40q…: longer than %d bytes", displayURL(s), maxRefLen)
	}
	re, like, chars := repoURLRe, "a repository URL like https://github.com/acme/billing", "'.', '-' and '_'"
	switch k {
	case urlNamespace:
		re, like = namespaceURLRe, "an organisation or group URL like https://gitlab.example.com/platform"
	case urlExclude:
		re, like = excludeURLRe, "a repository URL like https://github.com/acme/legacy or a pattern like https://gitlab.example.com/platform/legacy/**"
		chars = "'.', '-', '_' and the glob character *"
	}
	if re.MatchString(s) {
		return nil
	}
	return fmt.Errorf("%q is not %s: https:// (http:// only for localhost), without credentials, query or fragment, "+
		"and a path of letters, digits, %s", displayURL(s), like, chars)
}

// displayURL returns s with the user information of a URL in it (user:pass@)
// cut, so that a message never shows a credential written in targets.yml.
func displayURL(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return s
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return s
	}
	return scheme + "://…@" + rest[at+1:]
}

// site is where a URL points: its scheme and host (with a port other than
// the scheme's default) and the segments of its path.
type site struct {
	origin string
	segs   []string
}

// siteOf splits a URL that passed checkURL or checkTargetURL.
func siteOf(raw string) (site, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return site{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return site{}, false
		}
		defaultPort := (scheme == "https" && n == 443) || (scheme == "http" && n == 80)
		if !defaultPort {
			host += ":" + strconv.Itoa(n)
		}
	}
	var segs []string
	for _, seg := range strings.Split(u.Path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return site{origin: scheme + "://" + host, segs: segs}, true
}

// same reports whether s and base are the same place: the same origin and
// path segments, compared ignoring case.
func (s site) same(base site) bool {
	return s.origin == base.origin && slices.EqualFunc(s.segs, base.segs, strings.EqualFold)
}

// under reports whether s lies strictly beneath base: the same origin, and
// base's path segments, compared ignoring case, followed by at least one
// more.
func (s site) under(base site) bool {
	if s.origin != base.origin || len(s.segs) <= len(base.segs) {
		return false
	}
	for i, seg := range base.segs {
		if !strings.EqualFold(seg, s.segs[i]) {
			return false
		}
	}
	return true
}

// HasURLs reports whether a value of targets[].repo, org or group, or of
// exclude, is a web URL, which ResolveURLs resolves. A nil targets has
// none.
func (t *Targets) HasURLs() bool {
	if t == nil {
		return false
	}
	for i := range t.Targets {
		if isURL(selectorValue(&t.Targets[i])) {
			return true
		}
	}
	return slices.ContainsFunc(t.Exclude, isURL)
}

// ResolveURLs returns targets with every web URL among the values of
// targets[].repo, org and group and of exclude replaced by
// <provider>:<path>; the rest of touchmark reads that form only, so reports,
// --only and operations.yml keep naming targets <provider>:<path>.
//
// A URL's provider is the one of providers whose url (scheme, host, port and
// path) the URL lies beneath; the rest of the URL's path, without a
// trailing slash and, for a repository or an exclude entry, without a
// trailing .git, is the target's path. Exactly one provider must match:
// none, or several (providers that share a url), is an error, unless the
// entry names its provider with provider:, which then must be one of them.
// An implicit provider (the CI's own platform, for a hub.yml without
// providers) is the hub's only one: its targets are written as bare paths.
// On GitHub, Gitea, Forgejo and Bitbucket, which have no nested
// namespaces, a repository URL must name owner/name (workspace/repository on
// Bitbucket, as in https://bitbucket.org/acme/billing) and an organisation
// URL one namespace; on GitLab a URL with a /-/ segment points inside a project and
// is refused. On Azure DevOps, whose provider url is an organization
// (https://dev.azure.com/acme), a repository URL is
// https://dev.azure.com/acme/<project>/_git/<repository> and names
// <project>/<repository>, and the organization URL itself is the namespace
// of an org entry (the whole organization: a project is selected with
// match).
//
// The result is a copy: targets is left as it is, and returned as it is
// when it holds no URL. Errors name the field of each URL that did not
// resolve, joined, safe to print. A nil targets is returned as it is.
func ResolveURLs(targets *Targets, providers []ResolvedProvider) (*Targets, error) {
	if !targets.HasURLs() {
		return targets, nil
	}
	out := *targets
	out.Targets = slices.Clone(targets.Targets)
	out.Exclude = slices.Clone(targets.Exclude)
	sites := make([]site, len(providers))
	for i, p := range providers {
		sites[i], _ = siteOf(p.URL)
	}
	r := urlResolver{providers: providers, sites: sites}
	var errs []error
	for i := range out.Targets {
		e := &out.Targets[i]
		kind := entryKind(e)
		v := selectorValue(e)
		if !isURL(v) {
			continue
		}
		k := urlNamespace
		if kind == "repo" {
			k = urlRepo
		}
		ref, err := r.resolve(v, k, e.Provider, out.Defaults.Provider)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: targets[%d].%s: %w", TargetsFile, i, kind, err))
			continue
		}
		switch kind {
		case "repo":
			e.Repo = ref
		case "org":
			e.Org = ref
		default:
			e.Group = ref
		}
	}
	for i, ex := range out.Exclude {
		if !isURL(ex) {
			continue
		}
		ref, err := r.resolve(ex, urlExclude, "", out.Defaults.Provider)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: exclude[%d]: %w", TargetsFile, i, err))
			continue
		}
		out.Exclude[i] = ref
	}
	if len(errs) > 0 {
		return nil, cleanErr(errors.Join(errs...))
	}
	return &out, nil
}

// urlResolver finds the provider of a URL.
type urlResolver struct {
	providers []ResolvedProvider
	sites     []site
}

// resolve returns the URL s of kind k as [<provider>:]<path>; want is the
// entry's provider field ("" for none), and fallback defaults.provider,
// which picks among several matching providers when want is "" and it is
// one of them.
func (r urlResolver) resolve(s string, k urlKind, want, fallback string) (string, error) {
	if err := checkTargetURL(s, k); err != nil {
		return "", err
	}
	u, ok := siteOf(s)
	if !ok {
		return "", fmt.Errorf("%q is not a URL touchmark can read", displayURL(s))
	}
	var match []int
	for i, ps := range r.sites {
		// An Azure DevOps organization is the provider's url itself.
		whole := k == urlNamespace && r.providers[i].Type == "azure-devops" && u.same(ps)
		if ps.origin != "" && (u.under(ps) || whole) {
			match = append(match, i)
		}
	}
	if want != "" {
		i := slices.IndexFunc(r.providers, func(p ResolvedProvider) bool { return p.ID == want })
		if i >= 0 && !slices.Contains(match, i) {
			return "", fmt.Errorf("%s is not under the url of provider %s (%s)", s, want, r.providers[i].URL)
		}
		if i >= 0 {
			match = []int{i}
		}
	}
	if want == "" && fallback != "" && len(match) > 1 {
		i := slices.IndexFunc(r.providers, func(p ResolvedProvider) bool { return p.ID == fallback })
		if slices.Contains(match, i) {
			match = []int{i}
		}
	}
	switch {
	case len(match) == 0:
		return "", fmt.Errorf("%s is not under the url of any provider of %s (%s); add the provider there, or write the target as <provider>:<path>",
			s, HubFile, r.list(nil))
	case len(match) > 1:
		return "", fmt.Errorf("%s is under the url of providers %s; name one with provider: or defaults.provider, or write the target as <provider>:<path>",
			s, r.list(match))
	}
	p := r.providers[match[0]]
	base := r.sites[match[0]].segs
	segs := slices.Clone(u.segs[len(base):])
	switch p.Type {
	case "azure-devops":
		var err error
		if segs, err = azureSegments(segs, base, k); err != nil {
			return "", fmt.Errorf("%s: %w", s, err)
		}
	case "bitbucket-datacenter":
		var err error
		if segs, err = dataCenterSegments(segs, k); err != nil {
			return "", fmt.Errorf("%s: %w", s, err)
		}
	}
	if last := len(segs) - 1; k != urlNamespace && len(segs[last]) > len(".git") && strings.HasSuffix(strings.ToLower(segs[last]), ".git") {
		// A repository URL may end in .git; dropping it from a pattern
		// would widen it (acme/*.git to acme/*).
		if isGlob(segs[last]) {
			return "", fmt.Errorf("%s: a pattern whose last segment ends in .git; write it without .git", s)
		}
		segs[last] = segs[last][:len(segs[last])-len(".git")]
	}
	path := strings.Join(segs, "/")
	min := 2
	if k == urlNamespace {
		min = 1
	}
	if err := checkRefPath(path, min, k == urlExclude); err != nil {
		return "", fmt.Errorf("%s: %w", s, err)
	}
	if err := checkURLShape(p.Type, segs, k); err != nil {
		return "", fmt.Errorf("%s: %w", s, err)
	}
	if p.Implicit {
		return path, nil
	}
	return p.ID + ":" + path, nil
}

// list names the providers at indexes (every provider when nil) with their
// urls.
func (r urlResolver) list(indexes []int) string {
	if indexes == nil {
		for i := range r.providers {
			indexes = append(indexes, i)
		}
	}
	if len(indexes) == 0 {
		return "it has none"
	}
	parts := make([]string, 0, len(indexes))
	for _, i := range indexes {
		parts = append(parts, r.providers[i].ID+" at "+r.providers[i].URL)
	}
	return strings.Join(parts, ", ")
}

// azureSegments returns the path an Azure DevOps URL of kind k names, from
// segs, its path segments after the provider's url base (the
// organization): the organization itself for an org entry, which takes the
// organization URL only; <project>/<repository> for a repository URL,
// <project>/_git/<repository>; and for an exclude entry the same, or a
// pattern of the project's repositories (Legacy/*, Legacy/**).
func azureSegments(segs, base []string, k urlKind) ([]string, error) {
	const repoForm = "an Azure DevOps repository URL is https://dev.azure.com/<organization>/<project>/_git/<repository>"
	switch {
	case k == urlNamespace && len(segs) == 0 && len(base) > 0:
		return []string{base[len(base)-1]}, nil
	case k == urlNamespace:
		return nil, errors.New("an Azure DevOps org entry names the whole organization, the provider's url https://dev.azure.com/<organization>; " +
			"select a project's repositories with match: (<project>/*)")
	case len(segs) == 3 && segs[1] == "_git":
		return []string{segs[0], segs[2]}, nil
	case k == urlExclude && !slices.Contains(segs, "_git") && (len(segs) == 2 && isGlob(segs[1]) || slices.Contains(segs, "**")):
		return segs, nil
	case k == urlExclude:
		return nil, errors.New(repoForm + ", or a pattern of a project's repositories like https://dev.azure.com/<organization>/<project>/*")
	}
	return nil, errors.New(repoForm)
}

// dataCenterSegments returns the path a Bitbucket Data Center URL of kind k
// names, from segs, its path segments after the provider's url (which
// keeps any context path): <KEY>/<slug> for a repository's page,
// /projects/<KEY>/repos/<slug>[/browse…], or its clone URL,
// /scm/<key>/<slug>.git; <KEY> for a project's page, /projects/<KEY>; and
// for an exclude entry the same, or a pattern of a project's repositories
// (/projects/<KEY>/repos/*). Personal repositories (/users/<slug>/repos/…,
// project key ~<slug>) are not targets: their paths would not pass
// targets.yml's path rules.
func dataCenterSegments(segs []string, k urlKind) ([]string, error) {
	const repoForm = "a Bitbucket Data Center repository URL is <url>/projects/<KEY>/repos/<repository> (or its clone URL, <url>/scm/<key>/<repository>.git)"
	switch {
	case k == urlNamespace && len(segs) == 2 && segs[0] == "projects":
		return []string{segs[1]}, nil
	case k == urlNamespace:
		return nil, errors.New("a Bitbucket Data Center org entry names a project, <url>/projects/<KEY>")
	case len(segs) >= 4 && segs[0] == "projects" && segs[2] == "repos" && (len(segs) == 4 || segs[4] == "browse" && k != urlExclude):
		return []string{segs[1], segs[3]}, nil
	case len(segs) == 3 && segs[0] == "scm":
		return []string{segs[1], segs[2]}, nil
	case k == urlExclude && len(segs) >= 4 && segs[0] == "projects" && segs[2] == "repos":
		return append([]string{segs[1]}, segs[3:]...), nil
	}
	return nil, errors.New(repoForm)
}

// checkURLShape checks the path segments a URL of kind k names on a
// platform of type typ: GitHub, Gitea, Forgejo and Bitbucket have no nested
// namespaces, so an exclude URL of more than owner/name, without a "**"
// that could span nothing, points inside a repository (/tree/main) and
// would exclude nothing; a GitLab URL with a /-/ segment points inside a
// project (a file, a merge request), not at it.
func checkURLShape(typ string, segs []string, k urlKind) error {
	switch typ {
	case "bitbucket-datacenter":
		// dataCenterSegments has reduced the URL to <KEY>/<slug> or <KEY>.
		if k == urlExclude && len(segs) > 2 && !slices.Contains(segs, "**") {
			return errors.New("a Bitbucket Data Center repository URL is <url>/projects/<KEY>/repos/<repository>; " +
				"this one points inside a repository and would exclude nothing")
		}
	case "github", "gitea", "forgejo", "bitbucket":
		switch {
		case k == urlRepo && len(segs) != 2:
			return fmt.Errorf("a %s repository URL names owner/name, like https://example.com/acme/billing; this one has %d path segments after the provider's url", typ, len(segs))
		case k == urlExclude && len(segs) > 2 && !slices.Contains(segs, "**"):
			return fmt.Errorf("a %s repository URL names owner/name, like https://example.com/acme/legacy; this one has %d path segments after the provider's url, "+
				"so it points inside a repository and would exclude nothing", typ, len(segs))
		case k == urlNamespace && len(segs) != 1:
			return fmt.Errorf("a %s organisation URL names one owner, like https://example.com/acme; this one has %d path segments after the provider's url", typ, len(segs))
		}
	case "gitlab":
		if slices.Contains(segs, "-") {
			return errors.New("the /-/ segment points inside a GitLab project; use the URL of the project or group itself")
		}
	}
	return nil
}
