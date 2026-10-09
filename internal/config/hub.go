package config

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Defaults of hub.yml.
const (
	defaultBranchPrefix      = "touchmark/"
	defaultCommitMessage     = "chore: sync engineering assets"
	defaultPRTitle           = "chore: sync engineering assets"
	defaultLabel             = "engineering-assets"
	defaultLinkHub           = "auto"
	defaultMaxNewPRsPerRun   = 100
	defaultMaxCloseFraction  = 0.1
	defaultAutoCloseCooldown = "30d"
	defaultWriteIsolation    = "platform"
	defaultPrivateTargets    = "skip"
	defaultSign              = "auto"
)

// Enumerations of hub.yml.
var (
	providerTypes   = []string{"github", "gitlab", "gitea", "forgejo", "bitbucket", "azure-devops"}
	linkHubModes    = []string{"auto", "always", "never"}
	isolationModes  = []string{"platform", "external", "none"}
	privateTargets  = []string{"skip", "deliver"}
	signModes       = []string{"auto", "always"}
	defaultURLs     = map[string]string{"github": "https://github.com", "gitlab": "https://gitlab.com", "bitbucket": bitbucketURL}
	shorthandFields = []string{"writer", "sign", "platform", "base_url"}
)

func parseHub(data []byte) (*Hub, []Warning, error) {
	h := &Hub{}
	if data == nil {
		h.Legacy = true
		h.Version = 1
		applyHubDefaults(h, document{})
		return h, []Warning{{File: HubFile, Message: "not found; legacy hub, every setting has its default"}}, nil
	}
	p := &problems{file: HubFile}
	if scanSecrets(p, data) {
		return nil, nil, p.err()
	}
	doc, err := decodeStrict(HubFile, data, h)
	if err != nil {
		return nil, nil, err
	}
	validateHub(h, doc, p)
	if err := p.err(); err != nil {
		return nil, p.warns, err
	}
	applyHubDefaults(h, doc)
	return h, p.warns, nil
}

func validateHub(h *Hub, doc document, p *problems) {
	checkVersion(&h.Version, doc, p)
	checkHubID(h, doc, p)
	checkBranches(h, doc, p)
	for i, fp := range h.PreviousFingerprints {
		if !fingerprintRe.MatchString(fp) {
			p.errorf(fmt.Sprintf("previous_fingerprints[%d]", i), "%q must be host/numeric-id, like github.com/712345678", fp)
		}
	}
	if doc.has("opt_in_file") {
		if err := pathx.Validate(h.OptInFile); err != nil {
			p.errorf("opt_in_file", "%v", err)
		}
	}
	checkCommit(h, doc, p)
	checkPR(&h.PR, doc, p)
	checkLimits(&h.Limits, doc, p)
	if doc.has("memory.auto_close_cooldown") && !cooldownRe.MatchString(h.Memory.AutoCloseCooldown) {
		p.errorf("memory.auto_close_cooldown", "%q must be a number of days or hours, like 30d or 12h", h.Memory.AutoCloseCooldown)
	}
	checkSecurity(&h.Security, doc, p)
	checkProviders(h, doc, p)
	for i, s := range h.SensitivePaths {
		if err := checkPattern(s); err != nil {
			p.errorf(fmt.Sprintf("sensitive_paths[%d]", i), "%v", err)
		}
	}
	checkPackMeta(h.Packs, p)
}

func checkHubID(h *Hub, doc document, p *problems) {
	n := utf8.RuneCountInString(h.ID)
	switch {
	case !doc.has("id"):
		p.errorf("id", "required: a short slug naming this hub, like acme-eng")
	case n < minHubIDLen || n > maxHubIDLen || !packNameRe.MatchString(h.ID):
		p.errorf("id", "%q must be %d to %d lowercase letters and digits in words joined by single hyphens", h.ID, minHubIDLen, maxHubIDLen)
	}
}

func checkBranches(h *Hub, doc document, p *problems) {
	if doc.has("branch") {
		if err := checkBranchName(h.Branch); err != nil {
			p.errorf("branch", "%v", err)
		}
	}
	for i, b := range h.BranchAliases {
		if err := checkBranchName(b); err != nil {
			p.errorf(fmt.Sprintf("branch_aliases[%d]", i), "%v", err)
		}
	}
}

func checkCommit(h *Hub, doc document, p *problems) {
	if doc.has("commit.message") {
		msg := h.Commit.Message
		switch {
		case isBlank(msg):
			p.errorf("commit.message", "must not be blank")
		case draftRe.MatchString(msg):
			p.errorf("commit.message", "must not start with Draft: or WIP:, which turns a GitLab merge request into a draft")
		case hasScissorsLine(msg):
			p.errorf("commit.message", "must not hold git's scissors line %q: git drops the message from it on, and touchmark's trailers with it", scissorsLine)
		}
	}
	if doc.has("commit.author") {
		p.warnf("commit.author", "ignored: commits are always authored by the provider's writer account")
		h.Commit.Author = nil
	}
}

func checkPR(pr *PR, doc document, p *problems) {
	switch {
	case doc.has("pr.title") && isBlank(pr.Title):
		p.errorf("pr.title", "must not be blank")
	case doc.has("pr.title") && draftRe.MatchString(pr.Title):
		// The GitLab and Gitea drivers refuse such a title for a ready
		// pull request, and a draft gets its prefix from pr.draft.
		p.errorf("pr.title", "must not start with Draft: or WIP:, which makes a GitLab or Gitea pull request a draft; set pr.draft: true instead")
	}
	for i, l := range pr.Labels {
		if !labelRe.MatchString(l) || utf8.RuneCountInString(l) > maxLabelLen {
			p.errorf(fmt.Sprintf("pr.labels[%d]", i), "%q must be non-blank, without commas, at most %d characters", l, maxLabelLen)
		}
	}
	if doc.has("pr.intro_file") {
		if err := pathx.Validate(pr.IntroFile); err != nil {
			p.errorf("pr.intro_file", "%v", err)
		}
	}
	checkEnum(p, doc, "pr.link_hub", pr.LinkHub, linkHubModes)
}

func checkLimits(l *Limits, doc document, p *problems) {
	if l.MaxNewPRsPerRun < 0 {
		p.errorf("limits.max_new_prs_per_run", "must not be negative")
	}
	if doc.has("limits.max_close_fraction") {
		if f := l.MaxCloseFraction; !(f > 0 && f <= 1) {
			p.errorf("limits.max_close_fraction", "must be greater than 0 and at most 1, got %v", f)
		}
	}
}

func checkSecurity(s *Security, doc document, p *problems) {
	checkEnum(p, doc, "security.write_isolation", s.WriteIsolation, isolationModes)
	checkEnum(p, doc, "security.private_targets_in_public_hub", s.PrivateTargetsInPublicHub, privateTargets)
	if s.WriteIsolation == "none" && isBlank(s.Reason) {
		p.errorf("security.reason", "required with write_isolation: none: say why the write key cannot be isolated")
	}
}

// checkEnum validates a present enumerated field.
func checkEnum(p *problems, doc document, field, value string, allowed []string) {
	if doc.has(field) && !slices.Contains(allowed, value) {
		p.errorf(field, "%q must be one of %s", value, strings.Join(allowed, ", "))
	}
}

// providerFields names the keys of one provider in the file: an entry of
// providers, or the top-level shorthand.
type providerFields struct {
	doc    document
	prefix string            // "providers[0]." or ""
	rename map[string]string // key in Provider → key in the file
}

func (f providerFields) name(key string) string {
	if k, ok := f.rename[key]; ok {
		key = k
	}
	return f.prefix + key
}

func (f providerFields) has(key string) bool { return f.doc.has(f.name(key)) }

func checkProviders(h *Hub, doc document, p *problems) {
	seen := map[string]bool{}
	for i := range h.Providers {
		pr := &h.Providers[i]
		f := providerFields{doc: doc, prefix: fmt.Sprintf("providers[%d].", i)}
		checkProvider(pr, f, p)
		if pr.ID != "" && seen[pr.ID] {
			p.errorf(f.name("id"), "duplicate provider id %q", pr.ID)
		}
		seen[pr.ID] = true
	}
	var used []string
	for _, k := range shorthandFields {
		if doc.has(k) {
			used = append(used, k)
		}
	}
	switch {
	case len(used) == 0:
	case len(h.Providers) > 0:
		p.errorf("", "%s: the single-provider shorthand cannot be combined with providers; set these per provider", strings.Join(used, ", "))
	case !doc.has("platform"):
		// writer and sign alone: the provider's type and URL
		// come from the CI environment, or locally from the hub's origin
		// remote (Hub.ResolveProviders). A base_url without a type names
		// no provider.
		if doc.has("base_url") {
			p.errorf("platform", "required with base_url")
		}
		f := providerFields{doc: doc}
		checkAccounts(p, f, &Provider{Writer: h.Writer})
		checkEnum(p, doc, "sign", h.Sign, signModes)
	default:
		sh := Provider{ID: h.Platform, Type: h.Platform, URL: h.BaseURL, Writer: h.Writer, Sign: h.Sign}
		f := providerFields{doc: doc, rename: map[string]string{"id": "platform", "type": "platform", "url": "base_url"}}
		checkProvider(&sh, f, p)
		h.Providers = []Provider{sh}
	}
}

func checkProvider(pr *Provider, f providerFields, p *problems) {
	if !f.has("id") {
		p.errorf(f.name("id"), "required")
	} else if err := checkProviderID(pr.ID); err != nil && f.name("id") != f.name("type") {
		p.errorf(f.name("id"), "%v", err)
	}
	if !f.has("type") {
		p.errorf(f.name("type"), "required: one of %s", strings.Join(providerTypes, ", "))
	} else {
		checkEnum(p, f.doc, f.name("type"), pr.Type, providerTypes)
	}
	if f.has("url") {
		if err := checkURL(pr.URL); err != nil {
			p.errorf(f.name("url"), "%v", err)
		}
	} else if pr.Type == "gitea" || pr.Type == "forgejo" {
		p.errorf(f.name("url"), "required for %s", pr.Type)
	} else if pr.Type == "azure-devops" {
		p.errorf(f.name("url"), "required for azure-devops: https://dev.azure.com/<organization>")
	}
	if pr.Type == "bitbucket" && f.has("url") && checkURL(pr.URL) == nil && !f.has("api_url") && !isBitbucketCloud(pr.URL) {
		p.errorf(f.name("url"), "%s", bitbucketURLError)
	}
	if pr.Type == "azure-devops" && f.has("url") && checkURL(pr.URL) == nil && !azureDevOpsURLOK(pr.URL, f.has("api_url")) {
		p.errorf(f.name("url"), "%s", azureDevOpsURLError)
	}
	if f.has("api_url") {
		if err := checkURL(pr.APIURL); err != nil {
			p.errorf(f.name("api_url"), "%v", err)
		}
	}
	if f.has("ca_file") {
		if err := pathx.Validate(pr.CAFile); err != nil {
			p.errorf(f.name("ca_file"), "%v", err)
		}
	}
	checkAccounts(p, f, pr)
	checkEnum(p, f.doc, f.name("sign"), pr.Sign, signModes)
	limits := []struct {
		key string
		v   int
	}{
		{"limits.writes_per_minute", pr.Limits.WritesPerMinute},
		{"limits.writes_per_hour", pr.Limits.WritesPerHour},
		{"limits.reads", pr.Limits.Reads},
		{"limits.git_reads", pr.Limits.GitReads},
		{"limits.reads_per_minute", pr.Limits.ReadsPerMinute},
		{"limits.comments_per_minute", pr.Limits.CommentsPerMinute},
	}
	for _, l := range limits {
		if l.v < 0 {
			p.errorf(f.name(l.key), "must not be negative")
		}
	}
	if f.has("limits.min_interval") {
		if _, err := parseInterval(pr.Limits.MinInterval); err != nil {
			p.errorf(f.name("limits.min_interval"), "%v", err)
		}
	}
}

func checkAccounts(p *problems, f providerFields, pr *Provider) {
	if f.has("writer") {
		if err := checkAccount(pr.Writer); err != nil {
			p.errorf(f.name("writer"), "%v", err)
		}
	}
	lists := []struct {
		key   string
		names []string
	}{
		{"known_authors", pr.KnownAuthors},
		{"automation_accounts", pr.AutomationAccounts},
	}
	for _, l := range lists {
		for i, a := range l.names {
			if err := checkAccount(a); err != nil {
				p.errorf(fmt.Sprintf("%s[%d]", f.name(l.key), i), "%v", err)
			}
		}
	}
}

func checkPackMeta(packs map[string]PackMeta, p *problems) {
	for _, name := range sortedKeys(packs) {
		if err := checkPackName(name); err != nil {
			p.errorf("packs", "%v", err)
		}
		meta := packs[name]
		checkPackNames(p, "packs."+name+".requires", meta.Requires)
		checkPackNames(p, "packs."+name+".formerly", meta.Formerly)
	}
}

// applyHubDefaults fills every setting the file leaves out.
func applyHubDefaults(h *Hub, doc document) {
	if h.Branch == "" && h.ID != "" {
		h.Branch = defaultBranchPrefix + h.ID
	}
	if h.Commit.Message == "" {
		h.Commit.Message = defaultCommitMessage
	}
	if h.PR.Title == "" {
		h.PR.Title = defaultPRTitle
	}
	if h.PR.Labels == nil {
		h.PR.Labels = []string{defaultLabel}
	}
	setDefault(&h.PR.LinkHub, defaultLinkHub)
	if !doc.has("limits.max_new_prs_per_run") {
		h.Limits.MaxNewPRsPerRun = defaultMaxNewPRsPerRun
	}
	if !doc.has("limits.max_close_fraction") {
		h.Limits.MaxCloseFraction = defaultMaxCloseFraction
	}
	setDefault(&h.Memory.AutoCloseCooldown, defaultAutoCloseCooldown)
	setDefault(&h.Security.WriteIsolation, defaultWriteIsolation)
	setDefault(&h.Security.PrivateTargetsInPublicHub, defaultPrivateTargets)
	for i, fp := range h.PreviousFingerprints {
		h.PreviousFingerprints[i] = CanonicalFingerprint(fp)
	}
	for i := range h.Providers {
		pr := &h.Providers[i]
		setDefault(&pr.URL, defaultURLs[pr.Type])
		pr.URL = strings.TrimSuffix(pr.URL, "/")
		pr.APIURL = strings.TrimSuffix(pr.APIURL, "/")
		setDefault(&pr.Sign, defaultSign)
	}
}

func setDefault(v *string, def string) {
	if *v == "" {
		*v = def
	}
}

// CanonicalFingerprint returns a hub fingerprint (host[:port]/id) in the
// form CI reports it, so that spellings of one hub compare
// equal and hash to the same marker fp: the host lowercased, the default
// ports 443 and 80 dropped, and the repository id without leading zeros.
// Anything that is not host[:port]/id is returned unchanged.
func CanonicalFingerprint(fp string) string {
	if !fingerprintRe.MatchString(fp) {
		return fp
	}
	host, id, _ := strings.Cut(fp, "/")
	host = strings.ToLower(host)
	if name, port, ok := strings.Cut(host, ":"); ok && (port == "443" || port == "80") {
		host = name
	}
	if id = strings.TrimLeft(id, "0"); id == "" {
		id = "0"
	}
	return host + "/" + id
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
