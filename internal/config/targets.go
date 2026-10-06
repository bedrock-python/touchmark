package config

import (
	"fmt"
	"strings"
)

// optInLabel names the opt-in file in messages: its name is set by the hub,
// which ParseOptIn does not see.
const optInLabel = "opt-in file"

// v1TargetsKeys are the top-level keys of targets.yml v1.
var v1TargetsKeys = []string{"version", "defaults", "targets", "exclude"}

func parseTargets(data []byte) (*Targets, []Warning, error) {
	t := &Targets{Version: 1}
	if data == nil {
		return t, nil, nil
	}
	t.Version = 0
	p := &problems{file: TargetsFile}
	if scanSecrets(p, data) {
		return nil, nil, p.err()
	}
	doc, err := decodeStrict(TargetsFile, data, t)
	if err != nil {
		return nil, nil, err
	}
	if doc.has("repos") {
		convertLegacyTargets(t, doc, p)
	} else {
		validateTargets(t, doc, p)
	}
	if err := p.err(); err != nil {
		return nil, p.warns, err
	}
	return t, p.warns, nil
}

// convertLegacyTargets reads the pre-v1 format, a bare repos: list, as
// targets: entries.
func convertLegacyTargets(t *Targets, doc document, p *problems) {
	var mixed []string
	for _, k := range v1TargetsKeys {
		if doc.has(k) {
			mixed = append(mixed, k)
		}
	}
	if len(mixed) > 0 {
		p.errorf("repos", "the legacy list cannot be combined with %s; move the repositories under targets:", strings.Join(mixed, ", "))
		return
	}
	t.Version = 1
	t.Legacy = true
	for i, repo := range t.Repos {
		if _, err := ParseRef(repo); err != nil {
			p.errorf(fmt.Sprintf("repos[%d]", i), "%v", err)
		}
		t.Targets = append(t.Targets, Entry{Repo: repo})
	}
	p.warnf("repos", "legacy format, read as targets: entries; convert it to version: 1 with targets:")
}

func validateTargets(t *Targets, doc document, p *problems) {
	checkVersion(&t.Version, doc, p)
	if doc.has("defaults.provider") {
		if err := checkProviderID(t.Defaults.Provider); err != nil {
			p.errorf("defaults.provider", "%v", err)
		}
	}
	checkPackNames(p, "defaults.packs", t.Defaults.Packs)
	checkEnum(p, doc, "defaults.opt_in", t.Defaults.OptIn, optInModes)
	for i := range t.Targets {
		checkEntry(&t.Targets[i], fmt.Sprintf("targets[%d]", i), doc, p)
	}
	for i, ex := range t.Exclude {
		if err := checkExclude(ex); err != nil {
			p.errorf(fmt.Sprintf("exclude[%d]", i), "%v", err)
		}
	}
}

// optInModes are the values of opt_in.
var optInModes = []string{OptInRequired, OptInAssumed}

// checkExclude validates an exclude entry: a reference or a glob pattern,
// [<provider>:]<path>, or a web URL of either.
func checkExclude(ex string) error {
	if isURL(ex) {
		return checkTargetURL(ex, urlExclude)
	}
	_, err := parsePattern(ex)
	return err
}

func checkEntry(e *Entry, field string, doc document, p *problems) {
	var kinds []string
	for _, k := range []string{"repo", "org", "group"} {
		if doc.has(field + "." + k) {
			kinds = append(kinds, k)
		}
	}
	if doc.has(field + ".provider") {
		if err := checkProviderID(e.Provider); err != nil {
			p.errorf(field+".provider", "%v", err)
		}
	}
	checkPackNames(p, field+".packs", e.Packs)
	checkEnum(p, doc, field+".opt_in", e.OptIn, optInModes)
	switch len(kinds) {
	case 0:
		p.errorf(field, "needs one of repo, org or group")
		return
	case 1:
	default:
		p.errorf(field, "%s are mutually exclusive", strings.Join(kinds, " and "))
		return
	}
	kind := kinds[0]
	if v := selectorValue(e); isURL(v) {
		k := urlNamespace
		if kind == "repo" {
			k = urlRepo
		}
		if err := checkTargetURL(v, k); err != nil {
			p.errorf(field+"."+kind, "%v", err)
		}
	} else if ref, err := selectorRef(e, kind); err != nil {
		p.errorf(field+"."+kind, "%v", err)
	} else if ref.Provider != "" && e.Provider != "" && ref.Provider != e.Provider {
		p.errorf(field, "provider %q contradicts the %q prefix of %s", e.Provider, ref.Provider, kind)
	}
	if kind == "repo" {
		for _, k := range []string{"topics", "subgroups", "forks", "match"} {
			if doc.has(field + "." + k) {
				p.errorf(field+"."+k, "only with org or group")
			}
		}
		return
	}
	for i, topic := range e.Topics {
		if isBlank(topic) {
			p.errorf(fmt.Sprintf("%s.topics[%d]", field, i), "must not be blank")
		}
	}
	if e.Match != nil && len(e.Match) == 0 {
		p.errorf(field+".match", "must not be empty; leave match out to select every repository of the %s", kind)
	}
	for i, pat := range e.Match {
		if err := checkMatchPattern(pat); err != nil {
			p.errorf(fmt.Sprintf("%s.match[%d]", field, i), "%v", err)
		}
	}
}

// entryRef parses the selector of e: a repository for repo, a namespace for
// org and group. A URL is an error: ResolveURLs rewrites it first.
func entryRef(e *Entry) (Ref, error) { return selectorRef(e, entryKind(e)) }

// effectiveOptIn returns the opt_in of entry e: its own, else
// defaults.opt_in, else OptInRequired.
func (t *Targets) effectiveOptIn(e *Entry) string {
	switch {
	case e.OptIn != "":
		return e.OptIn
	case t.Defaults.OptIn != "":
		return t.Defaults.OptIn
	}
	return OptInRequired
}

// Assumed reports whether targets.yml subscribes a target that the entries
// at indexes select (Selector.Entry; indexes outside Targets are left
// out): one of them has opt_in: assumed, its own or through
// defaults.opt_in. Any one is enough, whatever the others say and whatever
// their kind: entries add up, as their packs do, and a repository a hub
// subscribes through its organisation stays subscribed when another entry
// names it to give it more packs. A nil targets assumes nothing.
func (t *Targets) Assumed(indexes []int) bool {
	if t == nil {
		return false
	}
	for _, i := range indexes {
		if i >= 0 && i < len(t.Targets) && t.effectiveOptIn(&t.Targets[i]) == OptInAssumed {
			return true
		}
	}
	return false
}

func selectorRef(e *Entry, kind string) (Ref, error) {
	switch kind {
	case "repo":
		return ParseRef(e.Repo)
	case "org":
		return parseRef(e.Org, 1)
	default:
		return parseRef(e.Group, 1)
	}
}

// entryKind returns "repo", "org" or "group" for a well-formed entry.
func entryKind(e *Entry) string {
	switch {
	case e.Repo != "":
		return "repo"
	case e.Org != "":
		return "org"
	default:
		return "group"
	}
}

// includeSubgroups reports whether an org or group entry reaches into
// nested namespaces; it does unless subgroups is false.
func includeSubgroups(e *Entry) bool { return e.Subgroups == nil || *e.Subgroups }

func parseOptIn(data []byte) (*OptIn, []Warning, error) {
	o := &OptIn{}
	doc, err := decodeStrict(optInLabel, data, o)
	if err != nil {
		return nil, nil, err
	}
	p := &problems{file: optInLabel}
	if doc.has("version") {
		if o.Version != 1 {
			p.errorf("version", "must be 1, got %d", o.Version)
		}
	} else {
		o.Legacy = true
		o.Version = 1
	}
	checkPackNames(p, "packs", o.Packs)
	if len(o.Ignore) > maxIgnorePatterns {
		p.errorf("ignore", "%d patterns, more than the %d allowed", len(o.Ignore), maxIgnorePatterns)
	}
	for i, pat := range o.Ignore {
		if err := checkPattern(pat); err != nil {
			p.errorf(fmt.Sprintf("ignore[%d]", i), "%v", err)
		}
	}
	if err := p.err(); err != nil {
		return nil, nil, err
	}
	return o, nil, nil
}
