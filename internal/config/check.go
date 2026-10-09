package config

import (
	"fmt"
	"slices"
	"strings"
)

// checker runs the cross-file checks of `touchmark check`.
type checker struct {
	hub     *Hub
	targets *Targets
	known   map[string]bool
	renamed map[string]string
	warns   []Warning
	errs    []error
}

func (c *checker) errorf(file, format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("%s: %s", file, fmt.Sprintf(format, args...)))
}

func (c *checker) warnf(file, format string, args ...any) {
	c.warns = append(c.warns, Warning{File: file, Message: fmt.Sprintf(format, args...)})
}

func check(hub *Hub, targets *Targets, known map[string]bool) ([]Warning, []error) {
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	if targets == nil {
		targets = &Targets{}
	}
	c := &checker{hub: hub, targets: targets, known: known, renamed: renames(hub, known)}
	if !hub.Legacy && hub.ID == PlaceholderID {
		c.errorf(HubFile, "id: still the template placeholder %q; set this hub's own id", PlaceholderID)
	}
	c.packMeta()
	c.formerly()
	c.cycles()
	if targets.HasURLs() {
		c.errorf(TargetsFile, "holds web URLs that were not resolved to providers (ResolveURLs)")
		return c.warns, c.errs
	}
	c.targetPacks()
	c.providers()
	c.exclude()
	c.match()
	c.topics()
	c.optIn()
	return c.warns, c.errs
}

// packRef checks one reference to a pack from file at field.
func (c *checker) packRef(file, field, name string) {
	if c.known[name] {
		return
	}
	if cur, ok := c.renamed[name]; ok {
		c.warnf(file, "%s: pack %q was renamed to %q", field, name, cur)
		return
	}
	c.errorf(file, "%s: unknown pack %q", field, name)
}

func (c *checker) packMeta() {
	for _, name := range sortedKeys(c.hub.Packs) {
		if !c.known[name] {
			c.warnf(HubFile, "packs.%s: no such pack under packs/", name)
		}
		for i, dep := range c.hub.Packs[name].Requires {
			c.packRef(HubFile, fmt.Sprintf("packs.%s.requires[%d]", name, i), dep)
		}
	}
}

// formerly checks that former names collide neither with current packs nor
// with each other.
func (c *checker) formerly() {
	owner := map[string]string{}
	for _, name := range sortedKeys(c.hub.Packs) {
		for _, old := range c.hub.Packs[name].Formerly {
			field := fmt.Sprintf("packs.%s.formerly", name)
			if _, current := c.hub.Packs[old]; current || c.known[old] {
				c.errorf(HubFile, "%s: %q is a current pack", field, old)
				continue
			}
			if prev, ok := owner[old]; ok && prev != name {
				c.errorf(HubFile, "%s: %q is also a former name of %q", field, old, prev)
				continue
			}
			owner[old] = name
		}
	}
}

// cycles reports every requires cycle once, in a deterministic order.
func (c *checker) cycles() {
	const visiting, done = 1, 2
	state := map[string]int{}
	var stack []string
	var visit func(name string)
	visit = func(name string) {
		state[name] = visiting
		stack = append(stack, name)
		for _, dep := range c.hub.Packs[name].Requires {
			if cur, ok := c.renamed[dep]; ok {
				dep = cur
			}
			switch state[dep] {
			case visiting:
				i := slices.Index(stack, dep)
				c.errorf(HubFile, "requires cycle: %s", strings.Join(append(slices.Clone(stack[i:]), dep), " -> "))
			case 0:
				visit(dep)
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
	}
	for _, name := range sortedKeys(c.hub.Packs) {
		if state[name] == 0 {
			visit(name)
		}
	}
}

func (c *checker) targetPacks() {
	for i, name := range c.targets.Defaults.Packs {
		c.packRef(TargetsFile, fmt.Sprintf("defaults.packs[%d]", i), name)
	}
	for i, e := range c.targets.Targets {
		for j, name := range e.Packs {
			c.packRef(TargetsFile, fmt.Sprintf("targets[%d].packs[%d]", i, j), name)
		}
	}
}

// providers checks that every provider targets.yml names exists in hub.yml,
// and that every entry resolves to a provider when the hub has several.
func (c *checker) providers() {
	ids := map[string]bool{}
	var list []string
	for _, p := range c.hub.Providers {
		ids[p.ID] = true
		list = append(list, p.ID)
	}
	defined := "hub.yml defines none"
	if len(list) > 0 {
		defined = "hub.yml defines " + strings.Join(list, ", ")
	}
	ref := func(field, id string) {
		if id != "" && !ids[id] {
			c.errorf(TargetsFile, "%s: unknown provider %q (%s)", field, id, defined)
		}
	}
	ref("defaults.provider", c.targets.Defaults.Provider)
	for i := range c.targets.Targets {
		e := &c.targets.Targets[i]
		field := fmt.Sprintf("targets[%d]", i)
		r, err := entryRef(e)
		if err != nil {
			c.errorf(TargetsFile, "%s: %v", field, err)
			continue
		}
		ref(field+".provider", e.Provider)
		ref(field+"."+entryKind(e), r.Provider)
		if len(ids) > 1 && c.targets.Defaults.Provider == "" && e.Provider == "" && r.Provider == "" {
			c.errorf(TargetsFile, "%s: no provider, and %s; set provider or defaults.provider", field, defined)
		}
	}
	for i, ex := range c.targets.Exclude {
		if r, err := parsePattern(ex); err == nil {
			ref(fmt.Sprintf("exclude[%d]", i), r.Provider)
		}
	}
}

// exclude checks that exclude entries parse and warns about targets that are
// both listed and excluded. A pattern that covers no repository listed
// explicitly is fine: it is there for the repositories of org and group
// entries. On a platform without nested namespaces, an entry of more than
// owner/name without a "**" (which may span nothing) can cover no
// repository: a warning.
func (c *checker) exclude() {
	res := resolver{hub: c.hub, targets: c.targets}
	for i, ex := range c.targets.Exclude {
		pat, err := parsePattern(ex)
		if err != nil {
			c.errorf(TargetsFile, "exclude[%d]: %v", i, err)
			continue
		}
		segs := strings.Split(pat.Path, "/")
		if typ := c.flatType(res.provider(pat.Provider)); typ != "" && len(segs) > 2 && !slices.Contains(segs, "**") {
			shape := "a " + typ + " repository path is owner/name"
			switch typ {
			case "azure-devops":
				shape = "an Azure DevOps repository path is project/repository"
			case "bitbucket-datacenter":
				shape = "a Bitbucket Data Center repository path is PROJECT/repository"
			}
			c.warnf(TargetsFile, "exclude[%d]: %s has %d path segments, and %s, so it excludes nothing", i, ex, len(segs), shape)
		}
		for j := range c.targets.Targets {
			e := &c.targets.Targets[j]
			if entryKind(e) != "repo" {
				continue
			}
			r, err := ParseRef(e.Repo)
			if err != nil {
				continue
			}
			r.Provider = firstNonEmpty(e.Provider, r.Provider)
			if !res.matchPattern(pat, r) {
				continue
			}
			if isGlob(pat.Path) {
				c.warnf(TargetsFile, "exclude[%d]: %s also covers targets[%d] (%s); exclude wins", i, ex, j, e.Repo)
			} else {
				c.warnf(TargetsFile, "exclude[%d]: %s is also listed as targets[%d]; exclude wins", i, ex, j)
			}
		}
	}
}

// match warns about a pattern of an org or group entry's match that no
// repository of its namespace can match: it selects nothing. A namespace
// on a platform without nested namespaces (flatType) holds no subgroups,
// whatever subgroups says. On Azure DevOps the namespace is the
// organization, which no repository path starts with (they are
// project/repository): a pattern there selects nothing only when it is
// deeper than that.
func (c *checker) match() {
	res := resolver{hub: c.hub, targets: c.targets}
	for i := range c.targets.Targets {
		e := &c.targets.Targets[i]
		if entryKind(e) == "repo" {
			continue
		}
		ns, err := entryRef(e)
		if err != nil {
			continue
		}
		id := res.provider(firstNonEmpty(e.Provider, ns.Provider))
		typ := c.flatType(id)
		if c.providerType(id) == "azure-devops" {
			for j, pat := range e.Match {
				segs := strings.Split(pat, "/")
				if checkMatchPattern(pat) == nil && len(segs) != 2 && !slices.Contains(segs, "**") {
					c.warnf(TargetsFile, "targets[%d].match[%d]: %s has %d path segments, and an Azure DevOps repository path is project/repository, "+
						"so it selects nothing", i, j, pat, len(segs))
				}
			}
			continue
		}
		for j, pat := range e.Match {
			if checkMatchPattern(pat) == nil && !globUnder(pat, ns.Path, includeSubgroups(e) && typ == "") {
				where := "under " + ns.Path
				switch {
				case !includeSubgroups(e):
					where = "directly under " + ns.Path + " (subgroups: false)"
				case typ != "" && globUnder(pat, ns.Path, true):
					where = "directly under " + ns.Path + " (a " + typ + " namespace has no subgroups)"
				}
				c.warnf(TargetsFile, "targets[%d].match[%d]: %s matches no repository %s, so it selects nothing", i, j, pat, where)
			}
		}
	}
}

// flatType returns the type of the provider with id when its platform has no
// nested namespaces (github, gitea, forgejo, bitbucket, azure-devops,
// bitbucket-datacenter): a repository path there is owner/name
// (project/repository on Azure DevOps, the project's key and the
// repository's slug on Bitbucket Data Center).
// It returns "" for any other type, and for a provider hub.yml does not
// list (the implicit one, whose type the CI tells).
func (c *checker) flatType(id string) string {
	switch typ := c.providerType(id); typ {
	case "github", "gitea", "forgejo", "bitbucket", "azure-devops", "bitbucket-datacenter":
		return typ
	}
	return ""
}

// providerType returns the type of the provider with id, "" for a provider
// hub.yml does not list.
func (c *checker) providerType(id string) string {
	for _, p := range c.hub.Providers {
		if p.ID == id {
			return p.Type
		}
	}
	return ""
}

// topics refuses topics on an entry whose provider is Bitbucket Cloud or
// Azure DevOps, whose repositories have none: the entry would select
// nothing, and the driver refuses the selector.
func (c *checker) topics() {
	res := resolver{hub: c.hub, targets: c.targets}
	for i := range c.targets.Targets {
		e := &c.targets.Targets[i]
		if len(e.Topics) == 0 || entryKind(e) == "repo" {
			continue
		}
		ns, err := entryRef(e)
		if err != nil {
			continue
		}
		id := res.provider(firstNonEmpty(e.Provider, ns.Provider))
		switch c.providerType(id) {
		case "bitbucket":
			c.errorf(TargetsFile, "targets[%d].topics: provider %s is Bitbucket, whose repositories have no topics; "+
				"select them with match: (paths like %s/svc-*) or list them with repo:", i, id, ns.Path)
		case "azure-devops":
			c.errorf(TargetsFile, "targets[%d].topics: provider %s is Azure DevOps, whose repositories have no topics; "+
				"select them with match: (paths like <project>/svc-*) or list them with repo: (<project>/<repository>)", i, id)
		}
	}
}

// optIn warns about a repo entry whose opt_in: required says nothing: an
// org or group entry with opt_in: assumed may select the same repository,
// and one entry with assumed is enough to subscribe it (Targets.Assumed).
func (c *checker) optIn() {
	res := resolver{hub: c.hub, targets: c.targets}
	for i := range c.targets.Targets {
		e := &c.targets.Targets[i]
		if entryKind(e) != "repo" || e.OptIn != OptInRequired {
			continue
		}
		r, err := ParseRef(e.Repo)
		if err != nil {
			continue
		}
		r.Provider = firstNonEmpty(e.Provider, r.Provider)
		for j := range c.targets.Targets {
			o := &c.targets.Targets[j]
			if entryKind(o) == "repo" || c.targets.effectiveOptIn(o) != OptInAssumed {
				continue
			}
			ns, err := entryRef(o)
			if err == nil && res.mayContain(o, ns, r) {
				c.warnf(TargetsFile, "targets[%d]: opt_in: required does not hold %s back: targets[%d] (%s: %s) has opt_in: assumed and may select it, "+
					"and one entry with assumed subscribes a repository; to have it opt in itself, leave it out of targets[%d] with match", i, e.Repo, j, entryKind(o), selectorValue(o), j)
				break
			}
		}
	}
}
