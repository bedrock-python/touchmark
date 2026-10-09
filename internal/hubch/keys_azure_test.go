package hubch

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

// azureKeyRoutes are the answers of Azure DevOps for the hub repository's
// key store: one pipeline with a secret write key (the mistake), the group
// touchmark-distribute with the key and a Branch control check for main
// (settings as Microsoft's Terraform provider writes them), open to every
// pipeline, a group with no key, and the project's pipeline settings.
func azureKeyRoutes() map[string]string {
	const org = "/acme"
	const project = org + "/" + azProject
	return map[string]string{
		org + "/_apis/git/repositories/" + azID: `{"id": "` + azID + `", "name": "engineering-assets", "defaultBranch": "refs/heads/main",
			"project": {"id": "` + azProject + `", "name": "Platform"}}`,
		org + "/_apis/projects/" + azProject: `{"id": "` + azProject + `", "visibility": "private"}`,
		project + "/_apis/build/definitions": `{"count": 1, "value": [{"id": 3, "name": "engineering-assets",
			"variables": {"TOUCHMARK_READ_TOKEN": {"value": null, "isSecret": true}, "TOUCHMARK_GH_WRITE_TOKEN": {"value": null, "isSecret": true}}}]}`,
		project + "/_apis/distributedtask/variablegroups": `{"count": 2, "value": [
			{"id": 5, "name": "touchmark-distribute", "type": "Vsts", "variables": {"TOUCHMARK_WRITE_TOKEN": {"value": null, "isSecret": true}}},
			{"id": 6, "name": "shared", "type": "Vsts", "variables": {"COLOR": {"value": "blue"}}}]}`,
		project + "/_apis/pipelines/checks/configurations": `{"count": 2, "value": [
			{"id": 1, "type": {"id": "8c6f20a7-a545-4486-9777-f762fafe0d4d", "name": "Approval"}, "settings": {"approvers": []}},
			{"id": 2, "type": {"id": "fe1de3ee-a436-41b4-bb20-f6eb4cb879a7", "name": "Task Check"},
			 "settings": {"definitionRef": {"id": "86b05a0c-73e6-4f7d-b3cf-e38f3b39a75b", "name": "evaluatebranchProtection", "version": "0.0.1"},
			   "displayName": "Branch control",
			   "inputs": {"allowedBranches": "refs/heads/main, refs/heads/release/*", "ensureProtectionOfBranch": "true", "allowUnknownStatusBranch": "false"}}}]}`,
		project + "/_apis/build/generalsettings": `{"enforceSettableVar": true, "enforceJobAuthScope": false, "enforceReferencedRepoScopedToken": true}`,
		project + "/_apis/pipelines/pipelinepermissions/variablegroup/5": `{"resource": {"type": "variablegroup", "id": "5"},
			"allPipelines": {"authorized": true}, "pipelines": []}`,
	}
}

func TestReadKeyStoreAzure(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+maintainerToken))
	s := newKeyServer(t, "Authorization", basic, azureKeyRoutes())
	ks, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "azure-devops", APIURL: s.URL + "/acme", RepoID: strings.ToUpper(azID), Token: maintainerToken})
	if err != nil {
		t.Fatal(err)
	}
	if ks.Platform != "azure-devops" || ks.RepoPath != "Platform/engineering-assets" || ks.DefaultBranch != "main" || ks.Visibility != "private" {
		t.Errorf("store %+v", ks)
	}
	var got []string
	for _, sec := range ks.Secrets {
		got = append(got, sec.Where+":"+sec.Group+":"+sec.Name)
		if !sec.Masked && sec.Name != "COLOR" {
			t.Errorf("%s is secret", sec.Name)
		}
	}
	slices.Sort(got)
	want := []string{"pipeline:engineering-assets:TOUCHMARK_GH_WRITE_TOKEN", "pipeline:engineering-assets:TOUCHMARK_READ_TOKEN",
		"variable group:shared:COLOR", "variable group:touchmark-distribute:TOUCHMARK_WRITE_TOKEN"}
	if !slices.Equal(got, want) {
		t.Errorf("secrets %q, want %q", got, want)
	}
	if len(ks.VariableGroups) != 2 {
		t.Fatalf("groups %+v", ks.VariableGroups)
	}
	g := ks.VariableGroups[0]
	if g.Name != "touchmark-distribute" || !g.AllPipelines || len(g.BranchChecks) != 1 || g.ChecksUnread != "" {
		t.Fatalf("group %+v", g)
	}
	if bc := g.BranchChecks[0]; !slices.Equal(bc.Allowed, []string{"refs/heads/main", "refs/heads/release/*"}) || !bc.Protection || bc.AllowUnknown {
		t.Errorf("branch check %+v", bc)
	}
	if st := ks.PipelineSettings; st == nil || !st.SettableVarsLimited || st.JobScopeLimited || !st.ReposProtected {
		t.Errorf("pipeline settings %+v (%s)", ks.PipelineSettings, ks.PipelineSettingsUnread)
	}
	// The second group's permissions are not in the stand-in: noted, not fatal.
	if ks.VariableGroups[1].PermissionsUnread == "" {
		t.Errorf("group shared: %+v", ks.VariableGroups[1])
	}
	for _, a := range s.auth {
		if a != basic {
			t.Errorf("a request with %q", a)
		}
	}

	// A token that may not read the checks: noted on the group, the rest
	// stands.
	routes := azureKeyRoutes()
	routes["/acme/"+azProject+"/_apis/pipelines/checks/configurations"] = "status 403"
	s = newKeyServer(t, "Authorization", basic, routes)
	ks, err = ReadKeyStore(t.Context(), KeyStoreInput{Platform: "azure-devops", APIURL: s.URL + "/acme", RepoID: azID, Token: maintainerToken})
	if err != nil || ks.VariableGroups[0].ChecksUnread == "" {
		t.Fatalf("checks refused: %+v, %v", ks.VariableGroups, err)
	}

	// A refused token ends the reading.
	s = newKeyServer(t, "Authorization", "Basic other", azureKeyRoutes())
	if _, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "azure-devops", APIURL: s.URL + "/acme", RepoID: azID, Token: maintainerToken}); err == nil {
		t.Error("a refused token")
	}
	if _, err := ReadKeyStore(t.Context(), KeyStoreInput{Platform: "azure-devops", APIURL: s.URL + "/acme", RepoID: "712345678", Token: maintainerToken}); err == nil {
		t.Error("a repository id that is not a GUID")
	}
}
