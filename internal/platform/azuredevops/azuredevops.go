// Package azuredevops is the platform driver for Azure DevOps Services
// (dev.azure.com, REST API 7.1). One provider is one organization:
// https://dev.azure.com/<organization>. Azure DevOps Server (on premises)
// and the old https://<organization>.visualstudio.com URLs are not served.
//
// NewReader serves plan; NewWriter serves distribute and doctor: it opens,
// edits, abandons and comments on pull requests, labels them, keeps the
// marker in a pull request property, pushes over git, and reports the
// writer's access for doctor (platform.Checker).
//
// Authentication is a personal access token (PAT) of a user: the reader's
// with the scope Code (read), the writer's with Code (read & write), of two
// different users. The REST API gets HTTP Basic with an empty user name and
// the token as the password, git over HTTPS Basic with any non-empty user
// name (https://learn.microsoft.com/en-us/azure/devops/organizations/accounts/use-personal-access-tokens-to-authenticate,
// "Use a PAT in your code";
// https://learn.microsoft.com/en-us/azure/devops/repos/git/auth-overview,
// "Git interactions require a username, which can be anything except an
// empty string"). Every API request sends X-TFS-FedAuthRedirect: Suppress,
// so that a refused credential answers 401 instead of a redirect to the
// sign-in page; a redirect to sign-in, or the 203 with an HTML page that
// Azure DevOps answers some refused credentials with, is ClassAuth all the
// same. Service principals of Microsoft Entra ID (Bearer tokens) come later.
//
// What the driver relies on, from the REST reference
// (https://learn.microsoft.com/en-us/rest/api/azure/devops/?view=azure-devops-rest-7.1),
// the concept pages it links, and anonymous answers of public projects
// (2026-10-09):
//   - accounts are identities known by their id, a GUID, which never
//     changes; displayName is neither unique nor stable and uniqueName is
//     empty for service identities, so a login in hub.yml is a GUID, and
//     Account.ID and Account.Login are that GUID. GET
//     {org}/_apis/connectionData names the credential's identity
//     (authenticatedUser; anonymous gets the all-a GUID of public access),
//     GET https://vssps.dev.azure.com/{org}/_apis/identities?identityIds=
//     another;
//   - an organization holds projects, a project holds repositories; there
//     are no topics and no nested groups. A repository is
//     <project>/<repository>; its id is a GUID, and repository-scoped calls
//     go through {org}/_apis/git/repositories/{id} without the project. An
//     empty repository has no defaultBranch; a disabled one says isDisabled
//     (and shows no default branch either);
//   - files: the Items API describes a path (objectId, gitObjectType,
//     isSymLink, commitId), the Trees API a folder's entries with their git
//     mode and size, the Blobs API a blob's bytes ($format=octetstream). A
//     404 tells what is missing by its typeKey: GitItemNotFoundException
//     (the path), GitUnresolvableToCommitException (the branch or commit),
//     GitRepositoryNotFoundException (the repository);
//   - pull requests: status active, completed (merged) or abandoned;
//     isDraft; closedBy only when one pull request is read alone (listings
//     leave it out and cut descriptions to 400 characters); a description
//     holds at most 4 000 characters (Pull Requests - Update); labels are
//     added by name (Pull Request Labels - Create); an abandoned pull
//     request could be reactivated, which touchmark never does; properties
//     (Pull Request Properties) carry the marker under touchmark.marker;
//   - limits are in TSTUs over a sliding five minutes, announced in
//     X-RateLimit-Remaining and X-RateLimit-Reset (which internal/throttle
//     reads from every answer) and in Retry-After; a blocked request is 429
//     with TF400733 (https://learn.microsoft.com/en-us/azure/devops/integrate/concepts/rate-limits);
//   - branch policies refuse direct pushes to a branch they protect
//     (TF402455) and a missing permission refuses a push with TF401027; the
//     writer reads neither upfront.
//
// What only a live organization can confirm is marked "assumed" where the
// driver relies on it, and listed in docs/guide/providers.md.
//
// The driver never retries or paces (internal/throttle does), knows the
// marker only as an opaque line it stores apart (marker.Detach,
// marker.Attach: platform.MarkerInProperties), and masks secrets in its
// errors.
package azuredevops

import (
	"errors"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// NewReader returns the read driver for provider p (type "azure-devops")
// with credential c (an auth.Token, a PAT with Code (read); a zero
// credential reads anonymously, public projects only) over client, which
// sends the token only to the organization's API hosts.
func NewReader(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &reader{c: cl}, nil
}

// NewWriter returns the write driver for provider p with credential c, a
// PAT with Code (read & write) of the user that writes (a write identity is
// never anonymous). Target checks the user's permissions on the repository
// and returns a TargetWriter bound to it; Azure DevOps cannot mint narrower
// tokens, so Close only retires the TargetWriter: later calls through it
// fail with ClassAuth.
func NewWriter(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
	if c.Kind == 0 {
		return nil, errors.New("azure-devops: the write driver needs a token")
	}
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &writer{reader: reader{c: cl}}, nil
}

// reader is platform.Reader over one identity.
type reader struct{ c *client }

// writer is platform.Writer: a reader that can mint TargetWriters.
type writer struct{ reader }

var (
	_ platform.Reader  = (*reader)(nil)
	_ platform.Writer  = (*writer)(nil)
	_ platform.Checker = (*writer)(nil)
)
