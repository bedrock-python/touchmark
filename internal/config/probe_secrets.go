package config

import (
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// WriteKeySecrets returns the names of the secrets the hub's GitHub Actions
// workflows hand touchmark as a write key or signing key, read as
// CheckProbe reads them: environment variables named as distribute reads
// them (TOUCHMARK_[<ID>_]WRITE_TOKEN, …_WRITE_APP_KEY, …_SIGNING_KEY) and
// action inputs named after them, whose values name secrets.<name>. The
// names are sorted, each once; files that do not parse add none.
// `doctor --hub-token` looks for these secrets wherever the hub keeps
// secrets.
func WriteKeySecrets(workflows map[string][]byte) []string {
	var out []string
	for _, name := range sortedKeys(workflows) {
		data := workflows[name]
		if len(data) > MaxWorkflowSize {
			continue
		}
		docs, err := yamlDocuments(data)
		if err != nil {
			continue
		}
		budget := maxNodes
		for _, doc := range docs {
			_ = walkWorkflow(doc, "", &budget, func(parent, key string, value *yaml.Node) {
				m := writeKeyVarRe.FindStringSubmatch(key)
				if m == nil && parent == "with" {
					m = writeKeyInputRe.FindStringSubmatch(strings.ToUpper(strings.ReplaceAll(key, "-", "_")))
				}
				if m == nil || value.Kind != yaml.ScalarNode {
					return
				}
				for _, s := range secretRefRe.FindAllStringSubmatch(value.Value, -1) {
					out = append(out, s[1])
				}
			}, func(string) {})
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
