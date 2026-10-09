// Package bitbuckete2e checks the hub template's Bitbucket Pipelines file,
// bitbucket-pipelines.yml of the template's working tree in
// TOUCHMARK_E2E_TEMPLATE, without Bitbucket: TestTemplatePipelines reads
// its pipelines and plays what Bitbucket Pipelines gives each step (the
// default variables, the repository variables, and a deployment's
// variables only to a step that names it), then runs touchmark's own
// guards on those steps. Like the GitHub package it has no build tag, and
// skips without TOUCHMARK_E2E_TEMPLATE or when the template has no
// bitbucket-pipelines.yml.
//
// What only a live Bitbucket can confirm is listed in
// docs/getting-started/bitbucket.md ("Assumed, not yet seen live").
package bitbuckete2e
