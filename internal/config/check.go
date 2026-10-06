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
	c.targetPacks()
	c.providers()
	c.exclude()
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
		if r, err := ParseRef(ex); err == nil {
			ref(fmt.Sprintf("exclude[%d]", i), r.Provider)
		}
	}
}

// exclude checks that exclude entries parse and warns about targets that are
// both listed and excluded.
func (c *checker) exclude() {
	res := resolver{hub: c.hub, targets: c.targets}
	for i, ex := range c.targets.Exclude {
		exRef, err := ParseRef(ex)
		if err != nil {
			c.errorf(TargetsFile, "exclude[%d]: %v", i, err)
			continue
		}
		for j := range c.targets.Targets {
			e := &c.targets.Targets[j]
			if entryKind(e) != "repo" {
				continue
			}
			r, err := ParseRef(e.Repo)
			if err == nil && res.matchRepo(r, e.Provider, exRef) {
				c.warnf(TargetsFile, "exclude[%d]: %s is also listed as targets[%d]; exclude wins", i, ex, j)
			}
		}
	}
}
