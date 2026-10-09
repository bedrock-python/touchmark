package hubch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Azure DevOps keeps a hub's secrets in two places a pipeline reads:
//   - the pipeline's own variables (its definition): every run of the
//     pipeline gets them, of any branch, pull request builds included; a
//     secret one reaches a step whose YAML maps it, and any branch's YAML
//     may;
//   - variable groups of the project's Library: a stage gets a group it
//     links, when the pipeline may use the group and the group's checks
//     pass. A Branch control check that admits the default branch only is
//     what keeps every other branch's stage out.
//
// Sources (REST API 7.1; the checks and pipeline permissions are 7.1
// previews):
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/build/definitions/list
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/distributedtask/variablegroups/get-variable-groups
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/approvalsandchecks/check-configurations/list
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/approvalsandchecks/pipeline-permissions/get
//   - https://learn.microsoft.com/en-us/rest/api/azure/devops/build/general-settings/get
//
// The Branch control check's settings are not in the REST reference: their
// shape (definitionRef name evaluatebranchProtection, the string inputs
// allowedBranches, ensureProtectionOfBranch, allowUnknownStatusBranch) is
// what Microsoft's Terraform provider for Azure DevOps writes.

// VariableGroup is a variable group of the hub's project on Azure DevOps,
// as doctor --hub-token reads it.
type VariableGroup struct {
	ID   int64
	Name string
	// AllPipelines is set when every pipeline of the project may use the
	// group (open access); PermissionsUnread says why that could not be
	// read ("" when it was).
	AllPipelines      bool
	PermissionsUnread string
	// BranchChecks are its Branch control checks; ChecksUnread says why its
	// checks could not be read ("" when they were).
	BranchChecks []BranchCheck
	ChecksUnread string
}

// BranchCheck is a Branch control check of Azure Pipelines.
type BranchCheck struct {
	// Allowed are the branches it admits (refs/heads/main, refs/heads/*,
	// *), as the check lists them.
	Allowed []string
	// Protection is set when the branch must have a branch policy (Verify
	// branch protection); AllowUnknown when a branch whose protection is
	// not known passes.
	Protection   bool
	AllowUnknown bool
}

// PipelineSettings are the settings of an Azure DevOps project's pipelines
// (Build General Settings) that bear on the write key.
type PipelineSettings struct {
	// SettableVarsLimited is "Limit variables that can be set at queue
	// time" (enforceSettableVar): off, whoever may queue a run of the default
	// branch sets any variable, BASH_ENV or DOCKER_HOST among them, in the
	// steps that hold the write key.
	SettableVarsLimited bool
	// JobScopeLimited is "Limit job authorization scope to current project
	// for non-release pipelines" (enforceJobAuthScope); ReposProtected is
	// "Protect access to repositories in YAML pipelines"
	// (enforceReferencedRepoScopedToken).
	JobScopeLimited bool
	ReposProtected  bool
}

// azureBranchControl is the name of the Branch control check's task.
const azureBranchControl = "evaluatebranchProtection"

// azure reads the key store of a hub in Azure Repos: the repository by id
// (and its project), the variables of the pipelines that build it, the
// project's variable groups with each one's checks and pipeline
// permissions, and the project's pipeline settings.
func (k *keyReader) azure(ctx context.Context) (KeyStore, error) {
	var repo struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		DefaultBranch string `json:"defaultBranch"`
		Project       struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Visibility string `json:"visibility"`
		} `json:"project"`
	}
	if err := k.azureGet(ctx, azureAPIVersion, k.segs("_apis", "git", "repositories", k.in.RepoID), nil, &repo); err != nil {
		return KeyStore{}, fmt.Errorf("hub keys: read the hub repository %s: %w", k.in.RepoID, err)
	}
	project := AzureRepoID(repo.Project.ID)
	if AzureRepoID(repo.ID) != k.in.RepoID || project == "" || repo.Name == "" {
		return KeyStore{}, fmt.Errorf("hub keys: the answer names repository %q of project %q, not %s", repo.ID, repo.Project.ID, k.in.RepoID)
	}
	k.ks = KeyStore{Platform: "azure-devops", RepoPath: repo.Project.Name + "/" + repo.Name, DefaultBranch: strings.TrimPrefix(repo.DefaultBranch, "refs/heads/")}
	vis := repo.Project.Visibility
	if vis == "" {
		var p struct {
			Visibility string `json:"visibility"`
		}
		if err := k.azureGet(ctx, azureAPIVersion, k.segs("_apis", "projects", project), nil, &p); err != nil {
			if err := k.unread("the hub's project", err); err != nil {
				return KeyStore{}, err
			}
		}
		vis = p.Visibility
	}
	k.ks.Visibility = visibility(vis)

	type variable struct {
		IsSecret bool `json:"isSecret"`
	}
	var defs struct {
		Value []struct {
			ID        int64               `json:"id"`
			Name      string              `json:"name"`
			Variables map[string]variable `json:"variables"`
		} `json:"value"`
	}
	q := url.Values{"repositoryId": {k.in.RepoID}, "repositoryType": {"TfsGit"}, "includeAllProperties": {"true"}, "$top": {"1000"}}
	if err := k.azureGet(ctx, azureAPIVersion, k.segs(project, "_apis", "build", "definitions"), q, &defs); err != nil {
		if err := k.unread("the variables of the hub's pipelines", err); err != nil {
			return KeyStore{}, err
		}
	}
	for _, d := range defs.Value {
		for name, v := range d.Variables {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: name, Where: "pipeline", Group: d.Name, Masked: v.IsSecret})
		}
	}

	var groups struct {
		Value []struct {
			ID        int64               `json:"id"`
			Name      string              `json:"name"`
			Variables map[string]variable `json:"variables"`
		} `json:"value"`
	}
	if err := k.azureGet(ctx, azureAPIVersion, k.segs(project, "_apis", "distributedtask", "variablegroups"), nil, &groups); err != nil {
		if err := k.unread("the project's variable groups", err); err != nil {
			return KeyStore{}, err
		}
		k.ks.VariableGroupsUnread = k.ks.Unread[len(k.ks.Unread)-1]
	}
	for _, g := range groups.Value {
		if g.ID <= 0 || g.Name == "" {
			continue
		}
		vg := VariableGroup{ID: g.ID, Name: g.Name}
		for name, v := range g.Variables {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: name, Where: "variable group", Group: g.Name, Masked: v.IsSecret})
		}
		if err := k.azureGroupGuards(ctx, project, &vg); err != nil {
			return KeyStore{}, err
		}
		k.ks.VariableGroups = append(k.ks.VariableGroups, vg)
	}

	var settings struct {
		EnforceSettableVar               bool `json:"enforceSettableVar"`
		EnforceJobAuthScope              bool `json:"enforceJobAuthScope"`
		EnforceReferencedRepoScopedToken bool `json:"enforceReferencedRepoScopedToken"`
	}
	if err := k.azureGet(ctx, azureAPIVersion, k.segs(project, "_apis", "build", "generalsettings"), nil, &settings); err != nil {
		why, err := unreadable("the project's pipeline settings", err)
		if err != nil {
			return KeyStore{}, err
		}
		k.ks.PipelineSettingsUnread = why
	} else {
		k.ks.PipelineSettings = &PipelineSettings{SettableVarsLimited: settings.EnforceSettableVar,
			JobScopeLimited: settings.EnforceJobAuthScope, ReposProtected: settings.EnforceReferencedRepoScopedToken}
	}
	return k.ks, nil
}

// azureGroupGuards reads the checks of variable group vg and which
// pipelines may use it. A listing the token may not read is noted in the
// group.
func (k *keyReader) azureGroupGuards(ctx context.Context, project string, vg *VariableGroup) error {
	id := strconv.FormatInt(vg.ID, 10)
	var checks struct {
		Value []struct {
			Type struct {
				Name string `json:"name"`
			} `json:"type"`
			Settings json.RawMessage `json:"settings"`
		} `json:"value"`
	}
	q := url.Values{"resourceType": {"variablegroup"}, "resourceId": {id}, "$expand": {"settings"}}
	if err := k.azureGet(ctx, azurePreviewVersion, k.segs(project, "_apis", "pipelines", "checks", "configurations"), q, &checks); err != nil {
		why, err := unreadable("the checks of variable group "+vg.Name, err)
		if err != nil {
			return err
		}
		vg.ChecksUnread = why
	}
	for _, c := range checks.Value {
		var s struct {
			DefinitionRef struct {
				Name string `json:"name"`
			} `json:"definitionRef"`
			Inputs map[string]string `json:"inputs"`
		}
		if json.Unmarshal(c.Settings, &s) != nil || !strings.EqualFold(s.DefinitionRef.Name, azureBranchControl) {
			continue
		}
		bc := BranchCheck{Protection: strings.EqualFold(s.Inputs["ensureProtectionOfBranch"], "true")}
		bc.AllowUnknown = strings.EqualFold(s.Inputs["allowUnknownStatusBranch"], "true") || strings.EqualFold(s.Inputs["allowUnknownStatusBranches"], "true")
		for b := range strings.SplitSeq(s.Inputs["allowedBranches"], ",") {
			if b = strings.TrimSpace(b); b != "" {
				bc.Allowed = append(bc.Allowed, b)
			}
		}
		vg.BranchChecks = append(vg.BranchChecks, bc)
	}
	var perms struct {
		AllPipelines *struct {
			Authorized bool `json:"authorized"`
		} `json:"allPipelines"`
	}
	if err := k.azureGet(ctx, azurePreviewVersion, k.segs(project, "_apis", "pipelines", "pipelinepermissions", "variablegroup", id), nil, &perms); err != nil {
		why, err := unreadable("the pipeline permissions of variable group "+vg.Name, err)
		if err != nil {
			return err
		}
		vg.PermissionsUnread = why
	}
	vg.AllPipelines = perms.AllPipelines != nil && perms.AllPipelines.Authorized
	return nil
}

// azureGet reads u with api-version and query into out, through the
// key reader's credential, refusing a redirect to the sign-in page.
func (k *keyReader) azureGet(ctx context.Context, version, u string, query url.Values, out any) error {
	q := url.Values{}
	for key, v := range query {
		q[key] = v
	}
	q.Set("api-version", version)
	resp, err := k.in.Client.JSONWith(ctx, http.MethodGet, u+"?"+q.Encode(), k.auth, azureHeaders(), nil, out)
	if resp != nil && (resp.Status == http.StatusNonAuthoritativeInfo || resp.Status/100 == 3) {
		return errors.New("sent to the sign-in page of Azure DevOps: the token is missing, expired or refused")
	}
	return err
}
