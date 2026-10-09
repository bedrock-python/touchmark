package gitlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// A writer on the hub (security.writer_on_hub: guard): the writer, a
// Developer of a group that holds both the targets and the hub, reaches the
// hub. What keeps a leaked write key from changing what distribute ships
// is then the hub's own protection, which GuardHub verifies:
//
//   - the writer is below Maintainer on the hub (a Maintainer changes the
//     protection, the settings and the variables);
//   - the default branch is protected, and none of the rules that match it
//     lets the writer push or merge (GitLab applies the most permissive);
//   - the hub's CI configuration is read from its default branch
//     (ci_config_path "<file>@<hub path>:<default branch>"): a merge
//     request pipeline otherwise runs the CI file of the source branch,
//     which the writer, pushing to it, could rewrite to drop the plan that
//     refuses its pushes;
//   - a merge waits for a pipeline that succeeded, and a skipped pipeline
//     does not count (only_allow_merge_if_pipeline_succeeds,
//     allow_merge_on_skipped_pipeline).
//
// Other protected branches the writer may push or merge to, and protected
// tags it may create, warn: their pipelines get the hub's protected
// variables. GitLab shows protected tags to Maintainers only, so the
// writer's view leaves them to doctor --hub-token (protected-tags).
// Developers and Reporters see the CI configuration path and the merge
// checks (checked on CE 18.11). Docs: https://docs.gitlab.com/api/protected_branches/,
// https://docs.gitlab.com/api/protected_tags/,
// https://docs.gitlab.com/api/projects/ (ci_config_path, merge settings),
// https://docs.gitlab.com/ci/pipelines/settings/#specify-a-custom-cicd-configuration-file.

// apiHubSettings are the hub's settings GuardHub reads; nil when the
// answer leaves them out (they are shown to some roles only).
type apiHubSettings struct {
	CIConfigPath          *string `json:"ci_config_path"`
	OnlyIfPipelineSucceed *bool   `json:"only_allow_merge_if_pipeline_succeeds"`
	MergeOnSkipped        *bool   `json:"allow_merge_on_skipped_pipeline"`
}

// apiProtectedTag is one of GET /projects/:id/protected_tags.
type apiProtectedTag struct {
	Name   string              `json:"name"`
	Create []apiProtectedLevel `json:"create_access_levels"`
}

// GuardHub reports the conditions of check "hub-guard" on hub
// (platform.HubGuard), as the writer reads them.
func (w *writer) GuardHub(ctx context.Context, hub platform.Repo) ([]platform.Finding, error) {
	const op = "check the hub"
	find := func(status platform.FindingStatus, format string, args ...any) platform.Finding {
		return platform.Finding{Repo: hub.Path, Check: "hub-guard", Status: status, Detail: fmt.Sprintf(format, args...)}
	}
	unread := func(what string, err error) ([]platform.Finding, error) {
		if stops(err) {
			return nil, err
		}
		return []platform.Finding{find(platform.FindingUnknown, "cannot read %s: %v", what, err)}, nil
	}
	self, err := w.c.selfAccount(ctx)
	if err != nil {
		return nil, err
	}
	selfID, _ := strconv.ParseInt(self.ID, 10, 64)
	id := projectID(hub)
	p, err := w.c.getProject(ctx, op, id)
	if err != nil {
		return unread("the hub", err)
	}
	id = strconv.FormatInt(p.ID, 10)
	branch := hub.DefaultBranch
	if p.DefaultBranch != nil && *p.DefaultBranch != "" {
		branch = *p.DefaultBranch
	}
	var out []platform.Finding
	level, err := w.accessLevel(ctx, op, self, p)
	switch {
	case err != nil && stops(err):
		return nil, err
	case err != nil:
		out = append(out, find(platform.FindingUnknown, "the writer's role on the hub cannot be read: %v", err))
	case level >= accessMaintainer:
		out = append(out, find(platform.FindingFail, "the writer has access level %d on the hub: as a Maintainer it changes the hub's protection, "+
			"settings and variables; make it a Developer", level))
	default:
		out = append(out, find(platform.FindingOK, "the writer has access level %d on the hub, below Maintainer", level))
	}

	rules, complete, err := w.listProtected(ctx, op, id)
	if err != nil {
		if stops(err) {
			return nil, err
		}
		out = append(out, find(platform.FindingUnknown, "the hub's protected branches cannot be read: %v", err))
	} else {
		out = append(out, defaultBranchGuard(find, rules, complete, branch, selfID, level)...)
		out = append(out, otherBranchesGuard(find, rules, branch, selfID, level)...)
	}
	var tags []apiProtectedTag
	_, err = listAll(ctx, w.c, op, w.c.projectURL(id, "protected_tags"), nil, maxProtectedPages, func(t apiProtectedTag) error {
		tags = append(tags, t)
		return nil
	})
	switch c := platform.ClassOf(err); {
	case err != nil && stops(err):
		return nil, err
	case c == platform.ClassPermission || c == platform.ClassNotFound:
		// GitLab shows protected tags to Maintainers only (checked on CE
		// 18.11: 403 to a Developer and a Reporter); doctor --hub-token
		// grades them (protected-tags).
	case err != nil:
		out = append(out, find(platform.FindingUnknown, "the hub's protected tags cannot be read: %v", err))
	default:
		for _, t := range tags {
			switch canPush(t.Create, selfID, level) {
			case platform.FindingOK:
				out = append(out, find(platform.FindingWarn, "the writer may create the protected tag %s: its pipelines get the hub's protected variables", t.Name))
			case platform.FindingUnknown:
				out = append(out, find(platform.FindingUnknown, "a group may create the protected tag %s, whose members the writer cannot read", t.Name))
			}
		}
	}

	var s apiHubSettings
	if _, err := w.c.get(ctx, op, w.c.projectURL(id), nil, &s); err != nil {
		if stops(err) {
			return nil, err
		}
		return append(out, find(platform.FindingUnknown, "the hub's settings cannot be read: %v", err)), nil
	}
	return append(out, settingsGuard(find, s, p.PathWithNamespace, branch)...), nil
}

// findFunc builds a Finding of hub-guard.
type findFunc func(status platform.FindingStatus, format string, args ...any) platform.Finding

// defaultBranchGuard grades the protection of the default branch against
// the writer's pushes and merges.
func defaultBranchGuard(find findFunc, rules []apiProtectedBranch, complete bool, branch string, selfID int64, level int) []platform.Finding {
	if branch == "" {
		return []platform.Finding{find(platform.FindingUnknown, "the hub's default branch is unknown")}
	}
	v := verdictOf(rules, branch, selfID, level)
	m := mergeVerdictOf(rules, branch, selfID, level)
	names := strings.Join(v.rules, ", ")
	var out []platform.Finding
	switch {
	case len(v.rules) == 0 && !complete:
		return []platform.Finding{find(platform.FindingUnknown, "the hub has more protected branches than touchmark reads, and none read protects %s", branch)}
	case len(v.rules) == 0:
		return []platform.Finding{find(platform.FindingFail, "the default branch %s is not protected: the writer pushes to it; protect it with push No one and merge Maintainers", branch)}
	}
	switch v.push {
	case platform.FindingOK:
		out = append(out, find(platform.FindingFail, "protected branch %s lets the writer push to %s: allow No one to push", names, branch))
	case platform.FindingUnknown:
		out = append(out, find(platform.FindingUnknown, "protected branch %s lets a group push to %s, whose members the writer cannot read", names, branch))
	default:
		out = append(out, find(platform.FindingOK, "protected branch %s keeps the writer from pushing to %s", names, branch))
	}
	switch m {
	case platform.FindingOK:
		out = append(out, find(platform.FindingFail, "protected branch %s lets the writer merge into %s: allow Maintainers alone to merge", names, branch))
	case platform.FindingUnknown:
		out = append(out, find(platform.FindingUnknown, "protected branch %s lets a group merge into %s, whose members the writer cannot read", names, branch))
	default:
		out = append(out, find(platform.FindingOK, "protected branch %s keeps the writer from merging into %s", names, branch))
	}
	return out
}

// mergeVerdictOf is verdictOf for merges: ok when a rule that matches
// branch lets the writer merge, unknown when none does but one lets a group
// merge, fail otherwise (and for a branch no rule matches).
func mergeVerdictOf(rules []apiProtectedBranch, branch string, selfID int64, level int) platform.FindingStatus {
	status := platform.FindingFail
	for _, rule := range rules {
		if !protectedMatch(rule.Name, branch) {
			continue
		}
		switch canPush(rule.Merge, selfID, level) {
		case platform.FindingOK:
			return platform.FindingOK
		case platform.FindingUnknown:
			status = platform.FindingUnknown
		}
	}
	return status
}

// otherBranchesGuard warns of the protected branches, besides those that
// match the default branch, the writer may push or merge to.
func otherBranchesGuard(find findFunc, rules []apiProtectedBranch, branch string, selfID int64, level int) []platform.Finding {
	var out []platform.Finding
	for _, rule := range rules {
		if branch != "" && protectedMatch(rule.Name, branch) {
			continue
		}
		push, merge := canPush(rule.Push, selfID, level), canPush(rule.Merge, selfID, level)
		switch {
		case push == platform.FindingOK || merge == platform.FindingOK:
			out = append(out, find(platform.FindingWarn, "the writer may push or merge to the protected branch %s: its pipelines get the hub's protected variables", rule.Name))
		case push == platform.FindingUnknown || merge == platform.FindingUnknown:
			out = append(out, find(platform.FindingUnknown, "a group may push or merge to the protected branch %s, whose members the writer cannot read", rule.Name))
		}
	}
	return out
}

// settingsGuard grades the hub's settings that make a merge wait for the
// plan of the default branch's CI configuration.
func settingsGuard(find findFunc, s apiHubSettings, path, branch string) []platform.Finding {
	var out []platform.Finding
	switch {
	case s.CIConfigPath == nil:
		out = append(out, find(platform.FindingUnknown, "the hub's CI configuration path is not shown to the writer: doctor --hub-token reads it"))
	case config.PinnedCIConfig(*s.CIConfigPath, path, branch):
		out = append(out, find(platform.FindingOK, "the hub's pipelines read their CI file from the default branch %s (%s)", branch, *s.CIConfigPath))
	default:
		out = append(out, find(platform.FindingFail, "the hub's merge request pipelines read the CI file of their source branch (CI configuration file %q), "+
			"which the writer may rewrite to drop the plan that refuses its pushes: set Settings > CI/CD > General pipelines > CI/CD "+
			"configuration file to .gitlab-ci.yml@%s:%s", strings.TrimSpace(deref(s.CIConfigPath)), path, branch))
	}
	switch {
	case s.OnlyIfPipelineSucceed == nil || s.MergeOnSkipped == nil:
		out = append(out, find(platform.FindingUnknown, "the hub's merge checks are not shown to the writer: doctor --hub-token reads them"))
	case !*s.OnlyIfPipelineSucceed:
		out = append(out, find(platform.FindingFail, "a merge into the hub does not wait for its pipeline: turn on Settings > Merge requests > Pipelines must succeed"))
	case *s.MergeOnSkipped:
		out = append(out, find(platform.FindingFail, "a skipped pipeline counts as a success for a merge into the hub: turn off Skipped pipelines are considered successful"))
	default:
		out = append(out, find(platform.FindingOK, "a merge into the hub waits for a pipeline that succeeded, and a skipped one does not count"))
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
