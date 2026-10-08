package bitbucket

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Capabilities of Bitbucket Cloud.
const (
	// maxBody is the body budget. The API documents no limit on a pull
	// request's description (Renovate assumes 250 000 characters); 60 000
	// bytes keeps parity with the other platforms until a live check
	// finds the real bound.
	maxBody = 60000
)

// limits are the default pacing. The repository API allows 1 000 requests
// an hour per account (about 16 a minute), and a run reads several times
// per target: 15 reads a minute, two targets and two git fetches at once,
// and at least a second between two writes, keep a run under the limit.
var limits = platform.Limits{Reads: 2, GitReads: 2, ReadsPerMinute: 15, MinInterval: time.Second}

// branchRestrictions is the check the writer cannot make upfront: branch
// restrictions are readable with repository:admin only, so a push meets
// them ("Permission denied to update branch …").
const branchRestrictions = "branch-restrictions"

// Account ids: a uuid in braces, and an Atlassian account id (24 hex
// digits, or "<number>:<uuid>").
var (
	uuidRe      = regexp.MustCompile(`^\{[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\}$`)
	accountIDRe = regexp.MustCompile(`^(?:[0-9a-f]{24}|[0-9]{1,10}:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
)

// apiAccount is an account as the API reports it: type "user" for a
// person or a bot account with API tokens, "app_user" for an app or an
// access token's bot, "team" for a workspace.
type apiAccount struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	AccountID   string `json:"account_id"`
	Nickname    string `json:"nickname"`
	DisplayName string `json:"display_name"`
	// Kind is the kind of an app_user (the OpenAPI description names it
	// without listing values).
	Kind string `json:"kind"`
}

// Probe reports the capabilities of Bitbucket Cloud, which has one version:
// it asks the API nothing.
func (d *reader) Probe(context.Context) (platform.Caps, error) {
	return platform.Caps{
		Flavor:  "bitbucket",
		MaxBody: maxBody,
		Draft:   platform.DraftNative,
		// Pull requests have no labels.
		NoLabels: true,
		// closed_by names who declined or merged a pull request.
		CloserKnown: true,
		// The API escapes HTML in descriptions: an HTML comment would show.
		Marker:      platform.MarkerInRefDef,
		RuntimeOnly: []string{branchRestrictions},
		Limits:      limits,
	}, nil
}

// Self is the account the credential acts as: GET /2.0/user, which takes
// the API token of an account.
func (d *reader) Self(ctx context.Context) (platform.Account, error) {
	return d.c.selfAccount(ctx)
}

// selfAccount returns the credential's account, looked up once.
func (c *client) selfAccount(ctx context.Context) (platform.Account, error) {
	const op = "get account"
	c.mu.Lock()
	if c.self != nil {
		a := *c.self
		c.mu.Unlock()
		return a, nil
	}
	c.mu.Unlock()
	if c.token == "" {
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassAuth, Err: errors.New("no credential: the reader is anonymous")}
	}
	var u apiAccount
	if _, err := c.get(ctx, op, c.endpoint("user"), nil, &u); err != nil {
		return platform.Account{}, err
	}
	if !uuidRe.MatchString(u.UUID) {
		return platform.Account{}, shapeError(op, "GET /user: no uuid")
	}
	a := toAccount(&u)
	if c.wantEmail {
		email, err := c.primaryEmail(ctx, op)
		if err != nil {
			return platform.Account{}, err
		}
		a.Email = email
	}
	c.mu.Lock()
	c.self = &a
	c.mu.Unlock()
	return a, nil
}

// maxEmailPages bounds the listing of the account's addresses.
const maxEmailPages = 5

// apiEmail is an address of GET /2.0/user/emails (the OpenAPI description
// does not describe its answer; these are the fields the API sends).
type apiEmail struct {
	Email       string `json:"email"`
	IsPrimary   bool   `json:"is_primary"`
	IsConfirmed bool   `json:"is_confirmed"`
}

// primaryEmail returns the primary address of the credential's account
// when it is confirmed (GET /2.0/user/emails, scope read:user:bitbucket, the
// scope GET /2.0/user needs too): the commit author touchmark gives the
// writer's commits, which Bitbucket links to the account by that address.
// "" when the account has none or the listing fails for another reason
// than a rate limit, a refused credential or the end of ctx, which fail the
// call: the core then authors commits with a placeholder address.
func (c *client) primaryEmail(ctx context.Context, op string) (string, error) {
	found := ""
	q := url.Values{"pagelen": {strconv.Itoa(repoPageLen)}}
	_, err := listAll(ctx, c, op, c.endpoint("user", "emails"), q, maxEmailPages, func(e apiEmail) error {
		if found == "" && e.IsPrimary && e.IsConfirmed && validEmail(e.Email) {
			found = e.Email
		}
		return nil
	})
	switch {
	case err != nil && stops(err):
		return "", err
	case err != nil:
		return "", nil
	}
	return found, nil
}

// validEmail reports whether s can be a commit author's address: one "@"
// between non-empty parts, and nothing git would refuse or misread (no
// angle brackets, blanks or control characters).
func validEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		!strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == '<' || r == '>' || r == 0x7f })
}

// Lookup resolves a login to its account: a uuid in braces or an Atlassian
// account id (GET /2.0/users/{selected_user} takes both). Bitbucket finds no
// account by its nickname, so anything else is ClassInvalid; an id the API
// does not know is ErrNotFound.
func (d *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	const op = "look up account"
	if !uuidRe.MatchString(login) && !accountIDRe.MatchString(login) {
		return platform.Account{}, invalid(op, "%q: Bitbucket logins are account UUIDs in braces, such as {3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}, "+
			"as GET /2.0/user shows them (a nickname names no account)", login)
	}
	var u apiAccount
	if _, err := d.c.get(ctx, op, d.c.endpoint("users", login), nil, &u); err != nil {
		return platform.Account{}, err
	}
	if !uuidRe.MatchString(u.UUID) {
		return platform.Account{}, shapeError(op, "GET /users/%s: no uuid", login)
	}
	return toAccount(&u), nil
}

// toAccount converts an API account: its uuid is both its id and its login.
// An app user is a bot; a user is a person or a bot account with API tokens,
// which the API does not tell apart; a workspace (team) or an account of
// another type is unknown.
func toAccount(u *apiAccount) platform.Account {
	if u == nil || u.UUID == "" {
		return platform.Account{}
	}
	a := platform.Account{ID: u.UUID, Login: u.UUID, Kind: platform.KindUnknown}
	switch u.Type {
	case "user":
		a.Kind = platform.KindUser
	case "app_user":
		a.Kind = platform.KindBot
	}
	return a
}
