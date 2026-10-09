package gitlab

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// guardLevel is one access level of a protected branch or tag: a role, or
// with user or group set, a user or a group.
func guardLevel(role int, user, group any) map[string]any {
	return map[string]any{"id": role + 1, "access_level": role, "access_level_description": "x", "deploy_key_id": nil, "user_id": user, "group_id": group}
}

// guardRule is a protected branch with push and merge levels.
func guardRule(name string, push, merge []any) map[string]any {
	return map[string]any{"id": 1, "name": name, "allow_force_push": false, "push_access_levels": push, "merge_access_levels": merge}
}

// guardHub serves the hub acme/engineering-assets (id 21, default branch
// main) to the writer (user 7, a Developer), with the protected branches
// rules, the protected tags tags, and the settings opts.
func guardHub(t *testing.T, role int, rules, tags []any, opts ...projectOpt) []platform.Finding {
	t.Helper()
	fx := newFixture(t)
	fx.json(http.MethodGet, "/user", http.StatusOK, self(7, "group_9_bot_writer", true))
	opts = append([]projectOpt{with("permissions", map[string]any{"project_access": nil,
		"group_access": map[string]any{"access_level": role, "notification_level": 3}})}, opts...)
	fx.json(http.MethodGet, "/projects/21", http.StatusOK, project(21, "acme/engineering-assets", opts...))
	fx.pages("/projects/21/protected_branches", rules)
	if tags == nil {
		// What GitLab answers a Developer.
		fx.json(http.MethodGet, "/projects/21/protected_tags", http.StatusForbidden, msg("403 Forbidden"))
	} else {
		fx.pages("/projects/21/protected_tags", tags)
	}
	var guard platform.HubGuard = fx.writer
	fs, err := guard.GuardHub(t.Context(), platform.Repo{Host: fx.provider().Host, ID: "21", Path: "acme/engineering-assets", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.requests("", "") {
		if c.Method != http.MethodGet {
			t.Errorf("GuardHub wrote: %s %s", c.Method, c.Path)
		}
	}
	return fs
}

// guarded are the hub's settings of a well guarded hub.
var guarded = []projectOpt{
	with("ci_config_path", ".gitlab-ci.yml@acme/engineering-assets:main"),
	with("only_allow_merge_if_pipeline_succeeds", true),
	with("allow_merge_on_skipped_pipeline", false),
}

// hasGuard reports whether fs has a finding of status whose detail holds
// sub.
func hasGuard(fs []platform.Finding, status platform.FindingStatus, sub string) bool {
	for _, f := range fs {
		if f.Check == "hub-guard" && f.Status == status && strings.Contains(f.Detail, sub) {
			return true
		}
	}
	return false
}

func TestGuardHub(t *testing.T) {
	maintainers := []any{guardLevel(40, nil, nil)}
	noOne := []any{guardLevel(0, nil, nil)}
	mainRule := guardRule("main", noOne, maintainers)

	t.Run("guarded", func(t *testing.T) {
		fs := guardHub(t, 30, []any{mainRule}, []any{}, guarded...)
		for _, f := range fs {
			if f.Status != platform.FindingOK {
				t.Errorf("%s: %s", f.Status, f.Detail)
			}
		}
		if len(fs) != 5 {
			t.Errorf("%d findings: %+v", len(fs), fs)
		}
	})
	// GitLab shows protected tags to Maintainers only: the writer's view
	// leaves them out, doctor --hub-token grades them.
	t.Run("protected tags not shown", func(t *testing.T) {
		fs := guardHub(t, 30, []any{mainRule}, nil, guarded...)
		for _, f := range fs {
			if f.Status != platform.FindingOK {
				t.Errorf("%s: %s", f.Status, f.Detail)
			}
		}
	})
	for _, tc := range []struct {
		name   string
		role   int
		rules  []any
		tags   []any
		opts   []projectOpt
		status platform.FindingStatus
		want   string
	}{
		{"a Maintainer writer", 40, []any{mainRule}, []any{}, guarded, platform.FindingFail, "as a Maintainer it changes"},
		{"main unprotected", 30, []any{guardRule("release/*", noOne, maintainers)}, []any{}, guarded, platform.FindingFail, "main is not protected"},
		// GitLab applies the most permissive rule: a wildcard that lets
		// Developers push wins over main's own.
		{"a wildcard lets Developers push", 30, []any{mainRule, guardRule("ma*", []any{guardLevel(30, nil, nil)}, maintainers)}, []any{},
			guarded, platform.FindingFail, "lets the writer push to main"},
		{"the writer allowed by name", 30, []any{guardRule("main", []any{guardLevel(0, nil, nil), guardLevel(0, 7, nil)}, maintainers)}, []any{},
			guarded, platform.FindingFail, "lets the writer push to main"},
		{"Developers may merge", 30, []any{guardRule("main", noOne, []any{guardLevel(30, nil, nil)})}, []any{}, guarded,
			platform.FindingFail, "lets the writer merge into main"},
		{"a group may merge", 30, []any{guardRule("main", noOne, []any{guardLevel(0, nil, 55)})}, []any{}, guarded,
			platform.FindingUnknown, "lets a group merge into main"},
		{"a group may push", 30, []any{guardRule("main", []any{guardLevel(0, nil, 55)}, maintainers)}, []any{}, guarded,
			platform.FindingUnknown, "lets a group push to main"},
		{"another protected branch", 30, []any{mainRule, guardRule("release", []any{guardLevel(30, nil, nil)}, maintainers)}, []any{}, guarded,
			platform.FindingWarn, "protected branch release"},
		{"a protected tag the writer creates", 30, []any{mainRule},
			[]any{map[string]any{"name": "v*", "create_access_levels": []any{guardLevel(30, nil, nil)}}}, guarded, platform.FindingWarn, "protected tag v*"},
		{"the CI file of the source branch", 30, []any{mainRule}, []any{},
			[]projectOpt{with("ci_config_path", ""), with("only_allow_merge_if_pipeline_succeeds", true), with("allow_merge_on_skipped_pipeline", false)},
			platform.FindingFail, ".gitlab-ci.yml@acme/engineering-assets:main"},
		{"the CI file pinned to another branch", 30, []any{mainRule}, []any{},
			[]projectOpt{with("ci_config_path", ".gitlab-ci.yml@acme/engineering-assets:dev"), with("only_allow_merge_if_pipeline_succeeds", true),
				with("allow_merge_on_skipped_pipeline", false)}, platform.FindingFail, "read the CI file of their source branch"},
		{"a merge without the pipeline", 30, []any{mainRule}, []any{},
			[]projectOpt{with("ci_config_path", ".gitlab-ci.yml@acme/engineering-assets:main"), with("only_allow_merge_if_pipeline_succeeds", false),
				with("allow_merge_on_skipped_pipeline", false)}, platform.FindingFail, "Pipelines must succeed"},
		{"skipped pipelines count", 30, []any{mainRule}, []any{},
			[]projectOpt{with("ci_config_path", ".gitlab-ci.yml@acme/engineering-assets:main"), with("only_allow_merge_if_pipeline_succeeds", true),
				with("allow_merge_on_skipped_pipeline", true)}, platform.FindingFail, "Skipped pipelines"},
		{"settings not shown", 30, []any{mainRule}, []any{}, nil, platform.FindingUnknown, "not shown to the writer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := guardHub(t, tc.role, tc.rules, tc.tags, tc.opts...)
			if !hasGuard(fs, tc.status, tc.want) {
				t.Errorf("no %s finding with %q in %+v", tc.status, tc.want, fs)
			}
		})
	}
}
