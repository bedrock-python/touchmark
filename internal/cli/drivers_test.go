package cli

import "testing"

// TestDriversRegistered: this build plans and distributes on every
// platform it supports: Gitea and Forgejo, one driver for both, GitLab,
// GitHub, Bitbucket Cloud and Azure DevOps Services.
func TestDriversRegistered(t *testing.T) {
	for _, typ := range []string{"gitea", "forgejo", "gitlab", "github", "bitbucket", "azure-devops"} {
		if planDrivers[typ] == nil || distributeDrivers[typ] == nil {
			t.Errorf("no %s driver: plan %v, distribute %v", typ, planDrivers[typ] != nil, distributeDrivers[typ] != nil)
		}
	}
}
