// Package bitbucket is the platform driver for Bitbucket Cloud
// (bitbucket.org, REST API 2.0). Bitbucket Data Center has another API and
// is not served here.
//
// NewReader serves plan; NewWriter serves distribute and doctor: it opens,
// edits, declines and comments on pull requests and pushes over git, and
// reports the writer's access for doctor (platform.Checker).
//
// Authentication is an API token of a bot account (app passwords are gone:
// their creation closed on 2025-09-09). The API gets
// "Authorization: Bearer <token>"; git over HTTPS gets Basic
// "x-bitbucket-api-token-auth:<token>" (REST intro, "API tokens";
// support.atlassian.com/bitbucket-cloud, "Using API tokens"). A token's
// scopes do not narrow the repositories it reaches: the account's
// permissions do. Access tokens of a repository, project or workspace are
// left for later: GET /2.0/user does not accept them (BCLOUD-23528), so the
// identity cannot learn who it is.
//
// What the driver relies on, from the OpenAPI description
// (https://api.bitbucket.org/swagger.json), the REST intro
// (https://developer.atlassian.com/cloud/bitbucket/rest/intro/) and
// anonymous answers of public repositories:
//   - accounts are known by their uuid, in braces, which never changes;
//     nickname is neither unique nor free of spaces, and there is no
//     lookup by name, so a login in hub.yml is a uuid (or an Atlassian
//     account id), and Account.ID and Account.Login are the uuid;
//   - a workspace is the namespace: no nested groups, no topics, no
//     archiving; a repository is workspace/slug, its uuid is immutable;
//   - collections are pages of values with an opaque next link, absent on
//     the last page; q= filters (BBQL) and sort= orders them, and on pull
//     request listings a q= makes the state parameter ignored, so the state
//     goes into q;
//   - GET …/src/{commit}/{path}?format=meta describes a path (commit_file
//     or commit_directory, attributes link, executable, subrepository,
//     binary, lfs, and size), and without format returns the raw bytes;
//     Git LFS content is a redirect to another host; there is no blob id;
//   - a pull request's states are OPEN, MERGED, DECLINED and SUPERSEDED,
//     draft is a flag, closed_by names who merged or declined it, and its
//     commit hashes are short (12 digits); there are no labels;
//   - limits are per account and hour (1 000 requests to
//     /2.0/repositories/*), answered with 429 and x-ratelimit-* headers
//     whose reset is in seconds;
//   - only open pull requests can be changed (PUT …/pullrequests/{id}), and a
//     declined one can never be reopened; a pull request is closed by
//     declining it (POST …/decline);
//   - an account's permission on a repository (admin, write, read) is
//     GET /2.0/user/workspaces/{workspace}/permissions/repositories, filtered
//     by the repository's uuid; branch restrictions are readable with admin
//     rights only, so a push meets them ("Permission denied to update
//     branch …").
//
// The driver never retries or paces (internal/throttle does), never knows
// about packs or markers, and masks secrets in its errors.
package bitbucket

import (
	"errors"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// NewReader returns the read driver for provider p (type "bitbucket") with
// credential c (an auth.Token, an API token; a zero credential reads
// anonymously, public repositories only) over client, which sends the token
// only to p's API host.
func NewReader(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &reader{c: cl}, nil
}

// NewWriter returns the write driver for provider p with credential c, an
// API token of the bot account that writes (a write identity is never
// anonymous). Target checks the account's permission on the repository
// (write or admin) and returns a TargetWriter bound to it; Bitbucket cannot
// mint narrower tokens, so Close only retires the TargetWriter: later calls
// through it fail with ClassAuth. Self also reads the account's primary
// confirmed address (GET /2.0/user/emails), the author of its commits.
func NewWriter(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
	if c.Kind == 0 {
		return nil, errors.New("bitbucket: the write driver needs a token")
	}
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	cl.wantEmail = true
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
