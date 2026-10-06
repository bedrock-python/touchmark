package config

import (
	"fmt"
	"strings"
	"time"
)

// CheckOperations performs the checks of `touchmark check` that need
// operations.yml together with hub.yml and targets.yml.
//
// Every entry's target must resolve to a provider of the hub: its own
// prefix, which must name a provider in hub.yml, else defaults.provider,
// else the hub's only provider. A hub without providers has one implicit
// provider (ResolveProviders): a bare path resolves to it and a prefix is
// unknown, as in Check. A bare path is an error when the hub has several
// providers and targets.yml sets no defaults.provider. A target that does
// not parse (operations built in code) is an error too.
//
// Entries whose until date has passed at now are warnings (Expired), so the
// file gets cleaned up. Heads, PR numbers and whether the targets exist
// need the platform: they are not checked here. A nil ops has nothing to
// check; nil hub and targets are a legacy hub and an empty targets.yml.
func CheckOperations(ops *Operations, targets *Targets, hub *Hub, now time.Time) ([]Warning, []error) {
	if ops == nil {
		return nil, nil
	}
	if hub == nil {
		hub = &Hub{Legacy: true}
	}
	if targets == nil {
		targets = &Targets{}
	}
	ids := map[string]bool{}
	var list []string
	for _, p := range hub.Providers {
		ids[p.ID] = true
		list = append(list, p.ID)
	}
	defined := "hub.yml defines none"
	if len(list) > 0 {
		defined = "hub.yml defines " + strings.Join(list, ", ")
	}
	res := resolver{hub: hub, targets: targets}
	var errs []error
	errorf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", OperationsFile, fmt.Sprintf(format, args...)))
	}
	target := func(field, s string) {
		ref, err := ParseRef(s)
		switch {
		case err != nil:
			errorf("%s: %v", field, err)
		case ref.Provider != "" && !ids[ref.Provider]:
			errorf("%s: unknown provider %q (%s)", field, ref.Provider, defined)
		case ref.Provider == "" && len(ids) > 1 && res.provider("") == "":
			errorf("%s: no provider, and %s; write the target as <provider>:%s", field, defined, ref.Path)
		}
	}
	for i, r := range ops.Recreate {
		target(fmt.Sprintf("recreate[%d].target", i), r.Target)
	}
	for i, f := range ops.ForgetDeclines {
		target(fmt.Sprintf("forget_declines[%d].target", i), f.Target)
	}
	return ops.Expired(now), errs
}
