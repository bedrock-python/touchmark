//go:build e2e

// Package gitlabe2e holds the live end-to-end tests of touchmark against
// GitLab CE. The build tag e2e keeps them out of the
// ordinary go test ./...: scripts/e2e/gitlab.sh starts GitLab and a runner
// in Docker, seeds the accounts of a hub (a reader and a writer) and runs
//
//	go test -tags e2e -race ./internal/e2e/gitlab/...
//
// with GitLab's URL, the logins and the tokens in TOUCHMARK_E2E_GITLAB_*
// variables (docs/project/e2e.md). Without TOUCHMARK_E2E_GITLAB_URL every test
// skips, so the Gitea harness, which runs ./internal/e2e/..., skips them.
//
// The tests are:
//
//   - TestConformance: the platform contract suite (internal/platform/
//     conformance) against the gitlab driver, with a live fixture that sets
//     GitLab up through the API as root: a fresh subgroup of the seeded
//     group per subtest, projects with files, nested subgroups, forks,
//     merge requests by the writer and by a person (also from the person's
//     fork), their states.
//   - TestAdminTokenRefused: the driver refuses an administrator's token.
//   - TestDistribute: plan and distribute through cli.Main against projects
//     of the seeded group and its subgroup: merge requests with their
//     bodies, markers and commits by the writer, a second run that writes
//     nothing, a person's decline, a pack change that force-pushes the sync
//     branch and keeps the merge request open, a deleted source branch that
//     closes it, and a merge request from a fork on the sync branch's name
//     that is never touched.
//   - TestMigration: the move from a hub that delivered with multi-gitter,
//     built synthetically: a group access token bot opens a merge request
//     without a marker on chore/sync-engineering-assets; with
//     branch_aliases, known_authors and adopt_unmarked, distribute adopts
//     it, also after that bot's token was revoked.
//   - TestProtectedBranch: a wildcard protected branch over touchmark/*,
//     which the writer meets at push: blocked:rules:protected-branch.
//   - TestFacts: GitLab behaviour touchmark relies on, through the API and
//     git alone, independent of the driver.
//   - TestCI: the CI experiments behind the isolation probe and threat T5
//     (a harmful target) on the seeded runner: what the isolation probe
//     sees in a merge request pipeline with an environment of action
//     prepare, and what CI_JOB_TOKEN of one target's pipeline reaches of
//     another target.
//
// Every test prints what it learned about GitLab as FINDING lines; TestMain
// repeats them in a summary at the end of the run.
//
// The package repeats a few helpers of internal/e2e (the findings, the
// isolated git, the watchdog) rather than sharing them: that package tests
// Gitea and Forgejo and stays as it is.
package gitlabe2e
