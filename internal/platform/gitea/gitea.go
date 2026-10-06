// Package gitea is the platform driver for Gitea and Forgejo.
//
// One driver serves both: Forgejo is a hard fork of Gitea with the same
// /api/v1 and a few delivery-critical differences, found by detection, not
// by version number (Forgejo answers /api/forgejo/v1/version, Gitea 404;
// Forgejo's versions carry a frozen "+gitea-1.22.0" suffix that must never
// gate Gitea features). Supported: Gitea ≥ 1.26, Forgejo ≥ 15 (LTS).
//
// Authentication is a personal access token of a bot user: the reader's
// with read:repository, read:issue, read:organization, read:user; the
// writer's with write:repository, write:issue, read:organization,
// read:user. The API gets "Authorization: token <token>"; git over HTTPS
// gets Basic "x-access-token:<token>": the server takes the password as the
// token and ignores the user name (services/auth/basic.go, checked on Gitea
// 1.26 and 1.27 and Forgejo 15 and 16), and the run's registry of secrets
// masks the Basic form of that user. An admin token is
// refused (it can act as anyone through Sudo).
//
// What the driver relies on, checked against the four versions:
//   - a pull request's head branch is head.label: head.ref turns into
//     refs/pull/<n>/head once the branch is deleted; AGit pull requests
//     have no head branch (an empty label, or flow 1 on Forgejo);
//   - there is no head filter before Forgejo 16, so pull requests are listed
//     by author (poster) and open ones in full, and filtered here; every
//     server-side filter is an optimization that the client-side filter
//     makes safe to ignore;
//   - who closed a pull request is the user of the last "close" event of its
//     timeline (merged_by for a merge), so CloserKnown holds;
//   - a draft is a title prefix (WORK_IN_PROGRESS_PREFIXES, "WIP:" and
//     "[WIP]" by default); labels are set by id, unknown ids and names are
//     dropped silently;
//   - deleting a base branch closes its pull requests on Forgejo, and on
//     Gitea retargets them to the default branch (RETARGET_CHILDREN_ON_MERGE,
//     on by default; closes them otherwise), after a moment; a missing base
//     shows as an empty base.sha, except in Gitea's pull request lists,
//     which keep the deleted branch's commit, so the branch API decides.
//
// The driver never retries or paces (internal/throttle does), never knows
// about packs or markers, and masks secrets in its errors.
package gitea

import (
	"errors"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// NewReader returns the read driver for provider p (type "gitea" or
// "forgejo") with credential c (an auth.Token; a zero credential reads
// anonymously) over client, which sends the token only to p's API host.
func NewReader(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &reader{c: cl}, nil
}

// NewWriter returns the write driver. Target checks the writer's
// permission on the repository (repository permissions push for
// contents, and the pull request write through issues) and returns a
// TargetWriter bound to it; Gitea and Forgejo cannot mint narrower tokens,
// so Close only retires the TargetWriter: later calls through it fail with
// ClassAuth.
func NewWriter(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
	if c.Kind == 0 {
		return nil, errors.New("gitea: the write driver needs a token")
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
