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
	for i := range t.Targets {
		checkEntry(&t.Targets[i], fmt.Sprintf("targets[%d]", i), doc, p)
	}
	for i, ex := range t.Exclude {
		if _, err := ParseRef(ex); err != nil {
			p.errorf(fmt.Sprintf("exclude[%d]", i), "%v", err)
		}
	}
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
	ref, err := selectorRef(e, kind)
	if err != nil {
		p.errorf(field+"."+kind, "%v", err)
	} else if ref.Provider != "" && e.Provider != "" && ref.Provider != e.Provider {
		p.errorf(field, "provider %q contradicts the %q prefix of %s", e.Provider, ref.Provider, kind)
	}
	if kind == "repo" {
		for _, k := range []string{"topics", "subgroups", "forks"} {
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
}

// entryRef parses the selector of e: a repository for repo, a namespace for
// org and group.
func entryRef(e *Entry) (Ref, error) { return selectorRef(e, entryKind(e)) }

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
