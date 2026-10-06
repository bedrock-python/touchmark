// Package githube2e runs the GitHub driver (internal/platform/github)
// end to end against ghfake, the HTTP fake of GitHub
// (internal/platform/github/ghfake), with no network.
// Unlike the live e2e packages of Gitea and GitLab it has no build tag:
// the tests are part of the ordinary go test ./..., and the heavier cases
// honor -short.
//
// The tests are:
//
//   - TestConformance: the platform contract suite (internal/platform/
//     conformance) against the driver over the fake, as github.com (the
//     fake's github.com flavor, the provider host github.com) and as GitHub
//     Enterprise Server (its /api/v3 layout): a reader App and a writer App
//     installed on the organization acme, a person with a personal access
//     token, repositories, forks and pull requests set up the way people
//     do on GitHub.
//   - TestDistribute: plan and distribute through cli.Main, as a
//     maintainer runs them, from a hub in a temporary directory whose
//     github provider points at the fake, with App credentials generated
//     at run time: pull requests by the writer App's bot with markers, a
//     second run that writes nothing, a person's close (a decline, the
//     closer read from GraphQL), a pack change (an updated pull request),
//     required signatures (an API commit, verified by GitHub, three
//     writes), an installation without the Workflows permission (blocked
//     before any write), per-target tokens (revoked after each target,
//     never used for another repository), a repository that refuses
//     drafts, a person's pull request from a fork on the sync branch
//     (never touched), and the sweep of a target dropped from targets.yml.
//     The fake's journal (Violations) must stay empty throughout.
//   - TestShapes: answers of the fake next to the shapes github.com gave
//     read-only requests on 2026-09-29 (testdata/shapes, trimmed to field
//     names and types, anonymized): the fields the driver reads exist in
//     both with the same JSON types.
//
// What only a live GitHub can confirm is listed in docs/project/e2e.md (GitHub).
package githube2e
