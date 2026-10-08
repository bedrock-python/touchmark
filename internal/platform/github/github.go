// Package github is the platform driver for GitHub: github.com, GHE.com
// (data residency, *.ghe.com) and GitHub Enterprise Server ≥ 3.19.
//
// Authentication is a GitHub App: the driver signs an RS256
// JWT with the App key, finds the installation of each target's owner and
// mints installation tokens. The reader uses one token per installation,
// narrowed to contents, pull requests and metadata read and renewed ten
// minutes before expiry; the writer reads the same way and mints a token
// per target (repository_ids=[id], only the permissions the target needs)
// that Close revokes. A token credential (a fine-grained PAT of a machine
// user) is used as is: for reading, and for writing without API commits. A
// zero credential reads public repositories anonymously, without GraphQL.
// Every secret the driver mints is registered with the run's registry of
// secrets (httpx.Client.Registry) and masked in the driver's own errors.
//
// What the driver relies on, from docs.github.com, the REST descriptions
// of api.github.com, GHE.com (ghec) and GHES 3.19 (github/rest-api-
// description), the GraphQL schemas of github.com and GHES 3.19, and
// read-only requests to public github.com data (2026-09-29):
//   - REST requests carry Accept: application/vnd.github+json and
//     X-GitHub-Api-Version: 2022-11-28, the one version GHES 3.19 knows; a
//     renamed repository answers 301 to /repositories/<id>/…, which a GET
//     follows below the API base; listings page by the Link header;
//   - the head filter of GET /pulls needs "<owner>:<branch>": a bare branch
//     is ignored silently and every pull request comes back; pull requests
//     from forks (another head owner) are found by GraphQL
//     pullRequests(headRefName:), which returns them, cross-repository ones
//     included;
//   - who closed a pull request is the actor of its last ClosedEvent
//     (GraphQL), typed Bot or User, whose databaseId is the REST user id
//     and whose Bot login lacks the "[bot]" suffix REST shows; a merge also
//     leaves a ClosedEvent, mergedBy names who merged;
//   - GraphQL answers a primary rate limit, a missing object and a refusal
//     with HTTP 200 and "errors" (RATE_LIMITED, NOT_FOUND, FORBIDDEN), per
//     path; data holds null there;
//   - the git trees API reads a tree by commit id, ref or HEAD, names a
//     symlink 120000 and a submodule 160000, and never follows a link; a
//     repository without commits answers 409;
//   - Commit.file(path:) (GraphQL) gives an entry's mode as an integer
//     (33188 = 100644, 33261 = 100755, 40960 = 120000, 57344 = 160000) and
//     no entry through a symlinked directory;
//   - updateRefs updates several refs atomically, with beforeOid as a
//     compare-and-swap and afterOid 0…0 to delete;
//   - GET /rules/branches/{b} answers for a branch that does not exist and
//     lists every active rule whoever may bypass it; GET
//     /repos/{o}/{r}/rulesets/{id} names the caller's
//     current_user_can_bypass (always, pull_requests_only, never, exempt);
//   - GET /users/{u}/repos lists a user's public repositories only; API
//     requests with a renamed organization's old name answer 404, while
//     git on the old paths still works;
//   - repositories report has_pull_requests on github.com, GHE.com and
//     GHES 3.19; GET /hash-algorithm exists on github.com and GHE.com, not
//     in GHES 3.19.
//
// What only a live GitHub can confirm is listed in docs/project/e2e.md
// (what only a live GitHub confirms) and marked "unverified"
// where the code depends on it.
//
// The driver never retries or paces (internal/throttle does), never knows
// about packs or markers, and masks secrets in its errors.
package github

import (
	"context"
	"errors"
	"io"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// NewReader returns the read driver for provider p (type "github") with
// credential c (an App or a token; a zero credential reads anonymously,
// public repositories only) over client, which sends credentials only to
// p's API host.
func NewReader(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &reader{c: cl}, nil
}

// NewWriter returns the write driver. Target mints a token for one
// repository with the permissions it needs and fails with ClassPermission
// (Rule "workflows", "contents" or "pull-requests") when the installation
// lacks them; Close revokes that token.
func NewWriter(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
	if c.Kind == 0 {
		return nil, errors.New("github: the write driver needs a GitHub App or a token")
	}
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &writer{reader: reader{c: cl}}, nil
}

// reader is platform.Reader over one identity.
type reader struct{ c *client }

// writer is platform.Writer: a reader that can narrow itself to targets.
type writer struct{ reader }

// Close revokes every reading token the driver minted for the
// installations of a GitHub App. Revoking per-target tokens
// (TargetWriter.Close) is not enough: reading tokens cover whole
// installations, private repositories included, and would otherwise stay
// valid for up to an hour after the run. A token GitHub no longer takes
// counts as revoked; the errors are joined. Other credentials mint nothing.
// The CLI closes its drivers after the run; one used afterwards mints
// again.
func (d *reader) Close() error {
	if d.c.kind != credApp {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpx.DefaultTimeout)
	defer cancel()
	return d.c.app.revokeAll(ctx)
}

var (
	_ io.Closer            = (*reader)(nil)
	_ platform.Reader      = (*reader)(nil)
	_ platform.BatchReader = (*reader)(nil)
	_ platform.Preflighter = (*reader)(nil)
	_ platform.Writer      = (*writer)(nil)
	_ platform.Preflighter = (*writer)(nil)
	_ platform.Checker     = (*writer)(nil)
)
