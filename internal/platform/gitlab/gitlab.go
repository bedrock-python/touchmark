// Package gitlab is the platform driver for GitLab: gitlab.com, Dedicated
// and self-managed, CE and EE.
//
// Supported: GitLab ≥ 17.0, tested on 17.11, 18.11 and the latest 19.x.
// Capabilities come from GET /api/v4/metadata and the version, never from
// assumptions about gitlab.com.
//
// Authentication is a token of a non-human account: a service account (its
// user id survives token rotation; the gitlab-ce images tested have the
// instance service accounts API from 19.x on: CE 17.11.7 and 18.11.12
// answer 404 to GET /service_accounts, which is EE code there, and 19.4.1
// has it), or a group or project access token (a new bot user per token:
// the previous one goes to known_authors). The reader has role Reporter
// with read_api and read_repository; the writer role Developer with api
// and write_repository. The scope api alone already reads and writes the
// repository over git HTTP (GitLab's scope table; a group access token
// with api alone could ls-remote and push on CE 17.11.7, 18.11.12 and
// 19.4.1): write_repository is redundant for the writer and kept for
// clarity, and no token but the writer's may carry api. The API gets
// "PRIVATE-TOKEN: <token>"; git over HTTPS gets Basic "oauth2:<token>"
// (GitLab's git HTTP endpoint takes Basic or Kerberos only). An
// administrator's token is refused: it can act as anyone through Sudo.
//
// Merge requests: at most one opened MR per source and target branch (a
// duplicate is 409 "Another open merge request already exists for this
// source branch: !N"); drafts are the "Draft: " title prefix (the REST API
// has no draft parameter, and a commit message starting with "Draft:" turns
// an MR into a draft); labels are names, created on first use; quick actions
// at the start of a line in a description or note run as the bot, through
// the API too (Caps.QuickActions), so the driver refuses to send such a
// line; an MR from a fork has source_project_id other than
// target_project_id; closed_by names who closed it, merged_by who merged
// it; deleting the source branch closes the MR; a force push keeps it open.
//
// What the driver relies on, from the GitLab source (gitlab-org/gitlab)
// and the REST documentation of 17.x to 19.x:
//   - the API finds a project by an old path of a renamed project
//     (Gitlab::ResourceLookup#lookup_project: find_by_full_path with
//     follow_redirects), with 200 and the canonical path_with_namespace;
//     groups and users are found by their current path only;
//   - a PUT of a merge request runs its state_event (the close or reopen
//     service) before it assigns title and description
//     (IssuableBaseService#update: change_additional_attributes, then
//     assign_attributes), and a reopen of a merged MR is ignored without
//     an error; a reopen of an MR whose source branch is gone is done on
//     CE 17.11.7 and 18.11.12 (200, the MR opens without a branch) and
//     refused with 422 on 19.4.1 (live, TestFacts reopen-without-branch).
//     So a reopen goes in a request of its own, after a check that both
//     branches exist, and everything that cannot apply is refused before
//     anything is sent;
//   - target_branch is checked for existence on create only
//     (MergeRequest#validate_branch_existence, on: :create): EditPR checks
//     a new base itself;
//   - the files API answers a missing ref "404 Commit Not Found", a
//     missing path, a directory "404 File Not Found", and a submodule or a
//     symlink as a blob (Gitlab::Git::Blob.tree_entry): the tree says what
//     the path is;
//   - the topic filter of project listings compares names case-sensitively
//     (Project.contains_all_topic_names): topics are filtered here only.
//
// The driver does not retry or pace (internal/throttle does). When the
// merge request API has not registered the push of a source branch yet
// (it sees it only after the push's background job; measured live),
// CreatePR says so with platform.ErrNotYet, and the core sends it again
// after a pause (see unregistered). It never knows about packs or markers,
// and masks secrets in its errors.
package gitlab

import (
	"errors"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// NewReader returns the read driver for provider p (type "gitlab") with
// credential c (an auth.Token; a zero credential reads anonymously, public
// projects only) over client, which sends the token only to p's API host.
func NewReader(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &reader{c: cl}, nil
}

// NewWriter returns the write driver. Target checks the writer's access
// level on the project (Developer or higher, from the project's permissions
// or members/all) and returns a TargetWriter bound to it; GitLab cannot mint
// narrower tokens from a token, so Close only retires the TargetWriter:
// later calls through it fail with ClassAuth.
func NewWriter(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
	if c.Kind == 0 {
		return nil, errors.New("gitlab: the write driver needs a token")
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
	_ platform.Reader = (*reader)(nil)
	_ platform.Writer = (*writer)(nil)
)
