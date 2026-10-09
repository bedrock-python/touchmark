// Package azuree2e checks the hub template's Azure Pipelines files,
// azure-pipelines.yml and its step template .azure-pipelines/touchmark.yml
// of the template's working tree in TOUCHMARK_E2E_TEMPLATE, without Azure
// DevOps: TestTemplateAzurePipelines reads the stages and plays what Azure
// Pipelines gives each step (the predefined variables, the pipeline's
// secret variables a step maps, and the variable group's only to the jobs
// that link it), then runs touchmark's own guards on those steps. Like the
// GitHub package it has no build tag, and skips without
// TOUCHMARK_E2E_TEMPLATE or when the template has no azure-pipelines.yml.
//
// What only a live Azure DevOps can confirm is listed in
// docs/getting-started/azure-devops.md ("Assumed, not yet seen live").
package azuree2e
