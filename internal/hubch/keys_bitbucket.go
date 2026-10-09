package hubch

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// bitbucketVariable is a Pipelines variable of Bitbucket (repository,
// workspace or deployment); its value is not decoded.
type bitbucketVariable struct {
	Key     string `json:"key"`
	Secured bool   `json:"secured"`
}

// bitbucket reads the key store of a hub on Bitbucket Cloud:
//   - the repository by UUID, GET /repositories/{}/{uuid} (the empty
//     workspace field is documented: "Repository object and UUID" in
//     https://developer.atlassian.com/cloud/bitbucket/rest/intro/);
//   - its repository variables, GET …/pipelines_config/variables;
//   - its workspace's variables, GET /workspaces/{ws}/pipelines-config/variables
//     (workspace administrators only: otherwise unread);
//   - its deployment environments, GET …/environments, and each one's
//     variables, GET …/deployments_config/environments/{uuid}/variables.
//
// Bitbucket's API does not show which branches may deploy to an
// environment (a Premium setting), so the key store holds none; an
// environment's restrictions.admin_only, not in the API reference, is read
// when present.
func (k *keyReader) bitbucket(ctx context.Context) (KeyStore, error) {
	var repo struct {
		UUID       string `json:"uuid"`
		FullName   string `json:"full_name"`
		IsPrivate  bool   `json:"is_private"`
		MainBranch *struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	u := k.base + "/repositories/%7B%7D/%7B" + k.in.RepoID + "%7D"
	if err := k.get(ctx, u, &repo); err != nil {
		return KeyStore{}, fmt.Errorf("hub keys: read the hub repository {%s}: %w", k.in.RepoID, err)
	}
	if BitbucketRepoID(repo.UUID) != k.in.RepoID {
		return KeyStore{}, fmt.Errorf("hub keys: the answer names repository %q, not {%s}", repo.UUID, k.in.RepoID)
	}
	ws, slug, ok := strings.Cut(repo.FullName, "/")
	if !ok || !validName(ws) || !validName(slug) {
		return KeyStore{}, fmt.Errorf("hub keys: the hub repository's name %q is not workspace/slug", repo.FullName)
	}
	k.ks = KeyStore{Platform: "bitbucket", RepoPath: repo.FullName, Visibility: "public"}
	if repo.IsPrivate {
		k.ks.Visibility = "private"
	}
	if repo.MainBranch != nil {
		k.ks.DefaultBranch = repo.MainBranch.Name
	}
	for _, l := range []struct{ what, where, u string }{
		{"the repository's Pipelines variables", "repository", k.segs("repositories", ws, slug, "pipelines_config", "variables")},
		{"the workspace's Pipelines variables", "workspace", k.segs("workspaces", ws, "pipelines-config", "variables")},
	} {
		vars, err := bitbucketList[bitbucketVariable](ctx, k, l.u)
		if err != nil {
			if err := k.unread(l.what, err); err != nil {
				return KeyStore{}, err
			}
			continue
		}
		for _, v := range vars {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: v.Key, Where: l.where, Masked: v.Secured})
		}
	}
	type environment struct {
		UUID         string `json:"uuid"`
		Name         string `json:"name"`
		Restrictions *struct {
			AdminOnly bool `json:"admin_only"`
		} `json:"restrictions"`
	}
	envs, err := bitbucketList[environment](ctx, k, k.segs("repositories", ws, slug, "environments"))
	if err != nil {
		if err := k.unread("the repository's deployment environments", err); err != nil {
			return KeyStore{}, err
		}
		k.ks.EnvironmentsUnread = k.ks.Unread[len(k.ks.Unread)-1]
	}
	for _, e := range envs {
		ne := NamedEnvironment{Name: e.Name}
		if e.Restrictions != nil {
			ne.AdminOnly = e.Restrictions.AdminOnly
		}
		k.ks.Environments = append(k.ks.Environments, ne)
		if BitbucketRepoID(e.UUID) == "" {
			k.ks.Unread = append(k.ks.Unread, fmt.Sprintf("the variables of environment %s: no UUID in the answer", e.Name))
			continue
		}
		vars, err := bitbucketList[bitbucketVariable](ctx, k, k.segs("repositories", ws, slug, "deployments_config", "environments", e.UUID, "variables"))
		if err != nil {
			if err := k.unread("the variables of environment "+e.Name, err); err != nil {
				return KeyStore{}, err
			}
			continue
		}
		for _, v := range vars {
			k.ks.Secrets = append(k.ks.Secrets, Secret{Name: v.Key, Where: "environment", Environment: e.Name, Masked: v.Secured})
		}
	}
	return k.ks, nil
}

// bitbucketList reads a paginated Bitbucket listing of T (values), 100 a
// page, until a page without next.
func bitbucketList[T any](ctx context.Context, k *keyReader, u string) ([]T, error) {
	var out []T
	for page := 1; page <= keyPages; page++ {
		var list struct {
			Next   string `json:"next"`
			Values []T    `json:"values"`
		}
		if err := k.get(ctx, fmt.Sprintf("%s?pagelen=%d&page=%d", u, keyPageSize, page), &list); err != nil {
			return nil, err
		}
		out = append(out, list.Values...)
		if list.Next == "" || len(list.Values) == 0 {
			return out, nil
		}
	}
	return out, errors.New("more items than touchmark reads")
}
