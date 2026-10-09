// Package bitbucketdc is the platform driver for Bitbucket Data Center
// (self-managed Bitbucket, formerly Bitbucket Server; REST API 1.0, served
// as /rest/api/latest), 8.19 and later. Bitbucket Cloud has another API
// (package bitbucket).
//
// NewReader serves plan; NewWriter serves distribute and doctor: it opens,
// edits, declines and comments on pull requests and pushes over git, and
// reports the writer's access for doctor (platform.Checker).
//
// Authentication is an HTTP access token, personal (of a user) or of a
// project or repository (of a service user Bitbucket makes for it). The
// API and git over HTTPS both get "Authorization: Bearer <token>", which
// every kind of token takes on 8.19 to 10.x (git through
// http.extraHeader); Basic with x-token-auth works for project and
// repository tokens from 9.4 only.
//
// What the driver relies on, from the OpenAPI descriptions of Bitbucket
// Data Center 8.0 to 10.5 (developer.atlassian.com/server/bitbucket/rest/)
// and Atlassian's documentation (confluence.atlassian.com/bitbucketserver),
// none of it seen on a live instance:
//   - a repository is <project key>/<slug>, its numeric id stays across
//     renames (its slug follows its name); a personal repository's project
//     key is ~<user slug>; a project is the namespace, with no nesting;
//     repository labels are the topics' counterpart
//     (GET /labels/{name}/labeled);
//   - accounts are users, known by their slug (the login in hub.yml) and
//     numeric id; type NORMAL is a person, SERVICE a service user (the
//     user of a project or repository token); there is no "who am I"
//     endpoint: every answer carries X-AUSERNAME, the user's name, which
//     GET /users?filter= turns into the user (an Atlassian engineer's
//     answer, not in the reference);
//   - collections are pages of values with start, limit, isLastPage and
//     nextPageStart, which a client must use as the next start; pages hold
//     at most 1 000 (500 for a pull request's activities);
//   - a pull request has states OPEN, DECLINED and MERGED, a version that
//     every change must name (409 when it is stale), one open pull request
//     per pair of branches, a native draft flag (8.18 and later), no
//     labels and no writable properties; a declined one can be reopened,
//     and whether it can be edited is not documented, so the driver edits
//     none (Caps.ClosedImmutable); who declined it is in its activities;
//     inactive pull requests are declined by Bitbucket's system user after
//     four weeks by default;
//   - HTML in Markdown is escaped (an HTML comment would show), and
//     link reference definitions render as nothing (CommonMark): the
//     marker is one (MarkerInRefDef); a description holds at most 32 768
//     characters (Atlassian's tracker, BSERV-14135);
//   - creating, declining and reopening pull requests and commenting need
//     read access only; pushes need write access; branch permissions are
//     readable with repository admin rights only, so a push meets them
//     ("Branch … can only be modified through pull requests");
//   - limits are a token bucket per user and node (60, refilled at 5 a
//     second, by default), answered with 429.
//
// The driver never retries or paces (internal/throttle does), never knows
// about packs or markers, and masks secrets in its errors.
package bitbucketdc

import (
	"errors"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// NewReader returns the read driver for provider p (type
// "bitbucket-datacenter") with credential c (an auth.Token, an HTTP access
// token; a zero credential reads anonymously, which an instance answers
// only where it allows anonymous access) over client, which sends the
// token only to p's API host.
func NewReader(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Reader, error) {
	cl, err := newClient(p, c, client)
	if err != nil {
		return nil, err
	}
	return &reader{c: cl}, nil
}

// NewWriter returns the write driver for provider p with credential c, an
// HTTP access token of the user that writes (a write identity is never
// anonymous). Target checks the user's write access to the repository and
// returns a TargetWriter bound to it; Bitbucket cannot mint narrower
// tokens, so Close only retires the TargetWriter: later calls through it
// fail with ClassAuth.
func NewWriter(p config.ResolvedProvider, c auth.Credential, client *httpx.Client) (platform.Writer, error) {
	if c.Kind == 0 {
		return nil, errors.New("bitbucket-datacenter: the write driver needs a token")
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
