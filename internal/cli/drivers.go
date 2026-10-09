package cli

import (
	"github.com/bedrock-python/touchmark/internal/platform/azuredevops"
	"github.com/bedrock-python/touchmark/internal/platform/bitbucket"
	"github.com/bedrock-python/touchmark/internal/platform/gitea"
	"github.com/bedrock-python/touchmark/internal/platform/github"
	"github.com/bedrock-python/touchmark/internal/platform/gitlab"
)

// init registers the platform drivers this build has: Gitea and Forgejo, one
// driver for both, GitLab, GitHub (github.com, GHE.com and GitHub
// Enterprise Server), Bitbucket Cloud and Azure DevOps Services. Tests that
// install their own drivers replace the maps whole and restore them
// afterwards.
func init() {
	for _, typ := range []string{"gitea", "forgejo"} {
		planDrivers[typ] = gitea.NewReader
		distributeDrivers[typ] = gitea.NewWriter
	}
	planDrivers["gitlab"] = gitlab.NewReader
	distributeDrivers["gitlab"] = gitlab.NewWriter
	planDrivers["github"] = github.NewReader
	distributeDrivers["github"] = github.NewWriter
	planDrivers["bitbucket"] = bitbucket.NewReader
	distributeDrivers["bitbucket"] = bitbucket.NewWriter
	planDrivers["azure-devops"] = azuredevops.NewReader
	distributeDrivers["azure-devops"] = azuredevops.NewWriter
}
