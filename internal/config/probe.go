package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/bedrock-python/touchmark/internal/docsurl"
)

// WorkflowsDir is where a hub on GitHub Actions keeps its workflows.
const WorkflowsDir = ".github/workflows"

// MaxWorkflowSize bounds a workflow file CheckProbe reads.
const MaxWorkflowSize = 512 << 10

// Patterns of CheckProbe: a variable or an action input that carries a
// write key (its name uppercased, '-' as '_'; the provider part in group
// 1, the key in group 2), a secret an expression names, a secret the
// probe's expression tests, and a step that runs distribute.
var (
	writeKeyVarRe   = regexp.MustCompile(`^TOUCHMARK_(?:([A-Z0-9_]+)_)?(WRITE_TOKEN|WRITE_APP_KEY|SIGNING_KEY)$`)
	writeKeyInputRe = regexp.MustCompile(`^(?:TOUCHMARK_)?(?:([A-Z0-9_]+)_)?(WRITE_TOKEN|WRITE_APP_KEY|SIGNING_KEY)$`)
	secretRefRe     = regexp.MustCompile(`secrets\.([A-Za-z_][A-Za-z0-9_]*)`)
	probedRe        = regexp.MustCompile(`secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*!=\s*(?:''|"")`)
	distributeRunRe = regexp.MustCompile(`\btouchmark\b[^\n]*\bdistribute\b`)
	// writeKeyNameRe is a secret named like a write key or signing key: an
	// expression that tests one is a probe expression.
	writeKeyNameRe = regexp.MustCompile(`(?i)(?:WRITE_TOKEN|WRITE_APP_KEY|SIGNING_KEY)$`)
	needsOutputRe  = regexp.MustCompile(`\bneeds\.([A-Za-z_][A-Za-z0-9_-]*)\.outputs\b`)
)

// CheckProbe checks the write isolation probe of the hub's GitHub Actions
// workflows: under security.write_isolation platform, the
// probe job computes TOUCHMARK_KEY_EXPOSED by testing every write key
// against the empty string (secrets.<name> != with an empty quoted
// string), so every secret a workflow hands a write key must be tested by
// such an expression, and the probe must cover the write key of every
// provider of hub.yml. workflows maps the paths of the files under
// WorkflowsDir to their content.
//
// It reads the files as data, across all of them (a reusable workflow may
// hold the key and its caller the probe):
//   - what carries a write key: the environment variables of any mapping
//     (workflow, job or step env) named as distribute reads them
//     (TOUCHMARK_[<ID>_]WRITE_TOKEN, …_WRITE_APP_KEY, …_SIGNING_KEY), and
//     the inputs of any action step (with:) named after them, in any case
//     and with '-' for '_' (gh-write-app-key, write-token, …), with the
//     secrets their values name;
//   - the tests of secrets against the empty string (in single or double
//     quotes) in any string.
//
// It returns an error for each secret that carries a write key and that no
// file tests. Tested somewhere is not enough: a probe job that misses a key
// lets distribute run while that key leaks, though another job tests it. So
// every probe expression (a string that tests a secret carrying a write key,
// or one named like a write key, against the empty string) must test every
// such secret, and so must the job whose output a TOUCHMARK_KEY_EXPOSED
// variable reads (needs.<job>.outputs.<name>: a job of the same file, else
// of any file): an error for each secret one of them misses, and for a
// TOUCHMARK_KEY_EXPOSED that is a constant or reads a job that tests no
// key. A value read from elsewhere (the inputs of a reusable workflow) is
// not traced. It also returns an error for each provider whose write key (…_WRITE_TOKEN or
// …_WRITE_APP_KEY from a secret) no workflow hands out where the check can
// see it, when the workflows run distribute at all (a step that runs
// "touchmark … distribute", a TOUCHMARK_KEY_EXPOSED variable, or any write
// key carried): a key it cannot see is a key the probe may miss; and one
// for a workflow file it cannot read as YAML. Short names (no provider
// part) count for a hub with one provider, and for one without providers
// (the implicit provider), any name does. Nothing is checked when the hub
// does not use the platform's isolation.
func CheckProbe(h *Hub, workflows map[string][]byte) []error {
	if h != nil && h.Security.WriteIsolation != "" && h.Security.WriteIsolation != "platform" {
		return nil
	}
	var errs []error
	used := map[string][]string{} // secret → "file: VARIABLE"
	probed := map[string]bool{}
	// carried holds the provider part of every write key (not a signing
	// key) handed out from a secret, uppercased; "" for the short names.
	carried := map[string]bool{}
	distributes := false
	var exposed []exposedVar
	jobs := map[string][]workflowJob{} // job id → the jobs of that id, by file
	for _, name := range sortedKeys(workflows) {
		data := workflows[name]
		if len(data) > MaxWorkflowSize {
			errs = append(errs, fmt.Errorf("%s: larger than %d bytes", name, MaxWorkflowSize))
			continue
		}
		docs, err := yamlDocuments(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %s", name, yamlError(err)))
			continue
		}
		budget := maxNodes
		for _, doc := range docs {
			if err := walkWorkflow(doc, "", &budget, func(parent, key string, value *yaml.Node) {
				if key == "TOUCHMARK_KEY_EXPOSED" {
					distributes = true
					if value.Kind == yaml.ScalarNode {
						exposed = append(exposed, exposedVar{file: name, value: value.Value})
					}
				}
				m := writeKeyVarRe.FindStringSubmatch(key)
				label := key
				if m == nil && parent == "with" {
					m = writeKeyInputRe.FindStringSubmatch(strings.ToUpper(strings.ReplaceAll(key, "-", "_")))
					label = "with " + key
				}
				if m == nil || value.Kind != yaml.ScalarNode {
					return
				}
				secrets := secretRefRe.FindAllStringSubmatch(value.Value, -1)
				for _, s := range secrets {
					used[s[1]] = append(used[s[1]], name+": "+label)
				}
				if len(secrets) > 0 && m[2] != "SIGNING_KEY" {
					carried[m[1]] = true
				}
			}, func(s string) {
				for _, m := range probedRe.FindAllStringSubmatch(s, -1) {
					probed[m[1]] = true
				}
				if distributeRunRe.MatchString(s) {
					distributes = true
				}
			}); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			for _, j := range workflowJobs(name, doc) {
				jobs[j.id] = append(jobs[j.id], j)
			}
		}
	}
	for _, secret := range sortedKeys(used) {
		if probed[secret] {
			continue
		}
		where := slices.Compact(slices.Sorted(slices.Values(used[secret])))
		errs = append(errs, fmt.Errorf("%s: secrets.%s carries a write key, and no probe tests it: add \"secrets.%s != ''\" to the expression "+
			"that sets TOUCHMARK_KEY_EXPOSED (security.write_isolation: platform; see "+docsurl.WriteIsolation+")", strings.Join(where, ", "), secret, secret))
	}
	errs = append(errs, incompleteProbes(used, probed, exposed, jobs)...)
	if distributes || len(carried) > 0 {
		errs = append(errs, uncarried(h, carried)...)
	}
	return errs
}

// exposedVar is a TOUCHMARK_KEY_EXPOSED variable of a workflow file.
type exposedVar struct {
	file, value string
}

// workflowJob is a job of a workflow file, with the probe expressions of
// its strings: the secrets each one tests against the empty string.
type workflowJob struct {
	file, id string
	probes   []map[string]bool
}

// workflowJobs returns the jobs of a workflow document, in file order.
func workflowJobs(file string, doc *yaml.Node) []workflowJob {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var out []workflowJob
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "jobs" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		list := root.Content[i+1]
		for k := 0; k+1 < len(list.Content); k += 2 {
			j := workflowJob{file: file, id: list.Content[k].Value}
			budget := maxNodes
			_ = walkWorkflow(list.Content[k+1], "", &budget, func(string, string, *yaml.Node) {}, func(s string) {
				tested := map[string]bool{}
				for _, m := range probedRe.FindAllStringSubmatch(s, -1) {
					tested[m[1]] = true
				}
				if len(tested) > 0 {
					j.probes = append(j.probes, tested)
				}
			})
			out = append(out, j)
		}
	}
	return out
}

// incompleteProbes returns an error for each secret of used (a secret that
// carries a write key) that some file tests but that a probe expression,
// or the job a TOUCHMARK_KEY_EXPOSED variable reads, does not test, once
// per job; and one for each TOUCHMARK_KEY_EXPOSED that is a constant or
// reads a job that tests no write key. Secrets no file tests are left to
// CheckProbe's own error.
func incompleteProbes(used map[string][]string, probed map[string]bool, exposed []exposedVar, jobs map[string][]workflowJob) []error {
	if len(used) == 0 {
		return nil
	}
	var want []string // the secrets every probe must test
	for _, s := range sortedKeys(used) {
		if probed[s] {
			want = append(want, s)
		}
	}
	isProbe := func(tested map[string]bool) bool {
		for s := range tested {
			if used[s] != nil || writeKeyNameRe.MatchString(s) {
				return true
			}
		}
		return false
	}
	var errs []error
	reported := map[string]bool{}
	missing := func(j workflowJob, tested map[string]bool, what string) {
		for _, s := range want {
			key := j.file + "\x00" + j.id + "\x00" + s
			if tested[s] || reported[key] {
				continue
			}
			reported[key] = true
			errs = append(errs, fmt.Errorf("%s: job %s: %s does not test secrets.%s, which carries a write key: add \"secrets.%s != ''\" to it; "+
				"every probe must test every write key (security.write_isolation: platform; see "+docsurl.WriteIsolation+")", j.file, j.id, what, s, s))
		}
	}
	for _, id := range sortedKeys(jobs) {
		for _, j := range jobs[id] {
			for _, tested := range j.probes {
				if isProbe(tested) {
					missing(j, tested, "its probe expression")
				}
			}
		}
	}
	for _, e := range exposed {
		refs := needsOutputRe.FindAllStringSubmatch(e.value, -1)
		if len(refs) == 0 {
			if !strings.Contains(e.value, "${{") {
				errs = append(errs, fmt.Errorf("%s: TOUCHMARK_KEY_EXPOSED is the constant %q: it must be the output of the probe job "+
					"(security.write_isolation: platform; see "+docsurl.WriteIsolation+")", e.file, e.value))
			}
			// Else tested in place, a probe expression of its job, or read
			// from where check cannot follow.
			continue
		}
		for _, ref := range refs {
			cands := jobs[ref[1]]
			var job *workflowJob
			for i := range cands {
				if cands[i].file == e.file {
					job = &cands[i]
					break
				}
			}
			if job == nil && len(cands) > 0 {
				job = &cands[0]
			}
			if job == nil {
				errs = append(errs, fmt.Errorf("%s: TOUCHMARK_KEY_EXPOSED reads needs.%s, and no workflow has a job %s "+
					"(security.write_isolation: platform; see "+docsurl.WriteIsolation+")", e.file, ref[1], ref[1]))
				continue
			}
			union := map[string]bool{}
			for _, tested := range job.probes {
				for s := range tested {
					union[s] = true
				}
			}
			if !isProbe(union) {
				key := job.file + "\x00" + job.id
				if !reported[key] {
					reported[key] = true
					errs = append(errs, fmt.Errorf("%s: job %s, whose output TOUCHMARK_KEY_EXPOSED reads, tests no write key against the empty string "+
						"(security.write_isolation: platform; see "+docsurl.WriteIsolation+")", job.file, job.id))
				}
				continue
			}
			missing(*job, union, "the probe whose output TOUCHMARK_KEY_EXPOSED reads")
		}
	}
	return errs
}

// uncarried returns an error for each provider of h whose write key no
// workflow hands out from a secret (carried holds the provider parts of
// those that do, "" for the short names).
func uncarried(h *Hub, carried map[string]bool) []error {
	var providers []Provider
	if h != nil {
		providers = h.Providers
	}
	if len(providers) == 0 {
		// The implicit provider, whose id is the CI's type: any name counts.
		if len(carried) == 0 {
			return []error{fmt.Errorf("no workflow hands touchmark a write key from a secret where check can see it " +
				"(TOUCHMARK_WRITE_TOKEN or TOUCHMARK_WRITE_APP_KEY in an env mapping, or an action input of that name), " +
				"so the probe cannot be checked (security.write_isolation: platform; see " + docsurl.WriteIsolation + ")")}
		}
		return nil
	}
	var errs []error
	for _, p := range providers {
		id := strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_"))
		if carried[id] || (len(providers) == 1 && carried[""]) {
			continue
		}
		prefix := EnvPrefix(p.ID)
		errs = append(errs, fmt.Errorf("provider %s: no workflow hands touchmark its write key from a secret where check can see it "+
			"(%sWRITE_TOKEN or %sWRITE_APP_KEY in an env mapping, or an action input %s-write-token or %s-write-app-key), "+
			"so the probe cannot be checked for it (security.write_isolation: platform; see "+docsurl.WriteIsolation+")",
			p.ID, prefix, prefix, strings.ToLower(p.ID), strings.ToLower(p.ID)))
	}
	return errs
}

// yamlDocuments decodes every YAML document of data.
func yamlDocuments(data []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, &n)
	}
}

// walkWorkflow calls pair for every key and value of every mapping of n,
// with the key the mapping is the value of (parent; "" at the top and in
// sequences), and scalar for every scalar, at most *budget nodes; aliases
// are not followed.
func walkWorkflow(n *yaml.Node, parent string, budget *int, pair func(parent, key string, value *yaml.Node), scalar func(string)) error {
	*budget--
	if *budget < 0 {
		return fmt.Errorf("more than %d YAML nodes", maxNodes)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		scalar(n.Value)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			pair(parent, n.Content[i].Value, n.Content[i+1])
			if err := walkWorkflow(n.Content[i], "", budget, pair, scalar); err != nil {
				return err
			}
			if err := walkWorkflow(n.Content[i+1], n.Content[i].Value, budget, pair, scalar); err != nil {
				return err
			}
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := walkWorkflow(c, "", budget, pair, scalar); err != nil {
				return err
			}
		}
	}
	return nil
}
