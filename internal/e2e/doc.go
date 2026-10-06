//go:build e2e

// Package e2e holds the live end-to-end tests of touchmark against real
// Gitea and Forgejo servers. The build tag e2e keeps
// them out of the ordinary go test ./...: scripts/e2e/gitea.sh starts a
// forge in Docker, seeds its accounts and runs
//
//	go test -tags e2e -race ./internal/e2e/...
//
// with the forge's URL, flavor, logins and tokens in TOUCHMARK_E2E_*
// variables (docs/project/e2e.md). Without TOUCHMARK_E2E_URL every test skips.
//
// The tests are:
//
//   - TestConformance: the platform contract suite (internal/platform/
//     conformance) against the gitea driver, with a live fixture that sets
//     platforms up through the API as an admin: organisations, teams,
//     repositories with files, forks, pull requests by a person and by the
//     writer, their states.
//   - TestSHA256Repo: the driver reports a repository of the sha256 object
//     format and reads its files.
//   - TestAGitPR: a pull request pushed through AGit, with a topic named
//     like a sync branch, is never listed as one from a branch.
//   - TestAdminTokenRefused: the driver's Self and Target refuse a site
//     admin's token, which Probe still reads the instance with.
//   - TestDistribute: plan and distribute through cli.Main against an
//     organisation of the forge, from a hub in a temporary directory:
//     pull requests with their bodies, markers and commits, a sha256
//     target skipped, a second run that writes nothing, a person's title
//     and labels and a new engine version that write nothing, a decline, a
//     close by the writer's own account, a pack change, a pull request from
//     a fork on the sync branch's name that is never touched, the driver's
//     view of the sync branch against the platform's, a change of the
//     hub's id (the old branch an alias), a person's pull request on a sync
//     branch (blocked:branch-in-use), a person's merge of another branch
//     into one (blocked:edited), an opt-in comment that keeps a decline and
//     an ignore that lifts it, a no-diff close with the branch deleted on a
//     target with autodetect_manual_merge (no merge recorded), a sweep
//     close of a target dropped from targets.yml, and a manual merge the
//     forge detects.
//   - TestFacts: platform facts touchmark assumes
//     (the closer in the timeline, the head filter of the pull request
//     list, drafts, labels by id, 409 on a duplicate, branch deletion,
//     manual merges, git's Basic credentials, signed-commit rules,
//     rate-limit headers, the pagination headers of each listing the
//     driver reads, ...), checked through the API and git alone,
//     independent of the driver.
//
// Every test prints what it learned about the platform as FINDING lines;
// TestMain repeats them in a summary at the end of the run.
package e2e
