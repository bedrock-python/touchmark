package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/report"
)

// check reads .touchmark/operations.yml like hub.yml: from HEAD unless
// --worktree. A file that fails to parse, or whose targets name no provider
// of the hub, fails check with exit 2; an entry past its date is a warning.
func TestCheckOperations(t *testing.T) {
	t.Parallel()
	const opsFile = ".touchmark/operations.yml"
	h := baseHub(t)
	h.write(opsFile, "version: 1\nrecreate:\n  - target: acme/api\n    head: 4b1d9e0\n")
	h.commit("operations")
	s := newScenario(t, "", h, nil)

	headErr := opsFile + `: recreate[0].head: "4b1d9e0" must be a full commit id: 40 or 64 lowercase hex digits`
	res := s.run(exitUsage, "check")
	if !strings.Contains(res.stdout, headErr) {
		t.Errorf("check: stdout %q, want %q", res.stdout, headErr)
	}
	res = s.run(exitUsage, "check", "--format", "json")
	var rep report.Check
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatalf("check --format json: %v\n%s", err, res.stdout)
	}
	if !slices.Contains(rep.Errors, headErr) {
		t.Errorf("check --format json: errors %q, want %q", rep.Errors, headErr)
	}

	// The work tree: the file parses, but its target names a provider the
	// hub does not declare; the expired entry is a warning.
	h.put(opsFile, "version: 1\nforget_declines:\n  - target: gh:acme/docs\n    pr: 44\nadopt_unmarked: {until: 2000-01-01}\n")
	res = s.run(exitUsage, "check", "--worktree")
	provErr := opsFile + `: forget_declines[0].target: unknown provider "gh" (hub.yml defines none)`
	expired := opsFile + ": adopt_unmarked: until 2000-01-01 has passed, the entry does nothing; remove it"
	if !strings.Contains(res.stdout, provErr) || !strings.Contains(res.stdout, expired) || strings.Contains(res.stdout, headErr) {
		t.Errorf("check --worktree: stdout %q, want %q and %q", res.stdout, provErr, expired)
	}

	h.put(opsFile, "version: 1\nforget_declines:\n  - target: acme/docs\n    pr: 44\nadopt_unmarked: {until: 2000-01-01}\n")
	res = s.run(0, "check", "--worktree")
	if !strings.Contains(res.stdout, expired) {
		t.Errorf("check --worktree: stdout %q, want the warning %q", res.stdout, expired)
	}
	// HEAD still holds the broken file.
	s.run(exitUsage, "check")
}
