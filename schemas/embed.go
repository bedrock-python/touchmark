// Package schemas embeds the JSON Schemas (draft 2020-12) of touchmark's
// configuration files and of its JSON reports, for `touchmark schema` and
// for tests.
package schemas

import "embed"

// BaseURL is where the schemas are published; every $id is BaseURL plus
// "<name>.schema.json".
const BaseURL = "https://raw.githubusercontent.com/bedrock-python/touchmark/master/schemas/"

//go:embed *.json
var files embed.FS

var names = []string{"hub", "targets", "opt-in", "operations", "report", "doctor", "setup", "status", "check"}

// Names returns the schema names Get accepts: hub, targets, opt-in,
// operations, report, doctor, setup, status and check.
func Names() []string { return append([]string(nil), names...) }

// Get returns the JSON Schema called name: "hub" (hub.yml), "targets"
// (targets.yml), "opt-in" (the target's opt-in file), "operations" (the
// hub's .touchmark/operations.yml), "report" (the JSON report of plan and
// distribute, report/v1), "doctor" (the JSON report of doctor, doctor/v1),
// "setup" (the JSON report of setup, setup/v1), "status" (the JSON report
// of status and apply, status/v1) or "check" (the JSON report of check,
// check/v1).
func Get(name string) ([]byte, bool) {
	for _, n := range names {
		if n == name {
			data, err := files.ReadFile(name + ".schema.json")
			return data, err == nil
		}
	}
	return nil, false
}
