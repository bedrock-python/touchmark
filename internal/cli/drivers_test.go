package cli

import "testing"

// TestDriversRegistered: this build plans and distributes on every
// platform of v1: Gitea and Forgejo, one driver for both,
// GitLab and GitHub.
func TestDriversRegistered(t *testing.T) {
	for _, typ := range []string{"gitea", "forgejo", "gitlab", "github"} {
		if planDrivers[typ] == nil || distributeDrivers[typ] == nil {
			t.Errorf("no %s driver: plan %v, distribute %v", typ, planDrivers[typ] != nil, distributeDrivers[typ] != nil)
		}
	}
}
