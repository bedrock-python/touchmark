package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Capabilities of GitLab.
const (
	// maxBody is the body budget: GitLab takes descriptions and notes of
	// up to description_and_note_max_size bytes (1 MiB by default,
	// app/models/concerns/issuable.rb); 200 000 leaves room for
	// instances that lower it and for proxies.
	maxBody = 200000
	// draftPrefix marks a draft: the REST API has no draft parameter
	// (https://docs.gitlab.com/api/merge_requests/#create-mr).
	draftPrefix = "Draft: "
	// minMajor is the oldest GitLab the driver supports.
	minMajor = 17
	// publicHost is gitlab.com, whose limits are fixed and known.
	publicHost = "gitlab.com"
)

// instance is what Probe finds out about the server.
type instance struct {
	version    string
	enterprise bool
}

// apiMetadata is GET /metadata (GitLab ≥ 15.2) and GET /version.
type apiMetadata struct {
	Version    string `json:"version"`
	Revision   string `json:"revision"`
	Enterprise *bool  `json:"enterprise"`
}

// apiUser is a user as the API reports it. /user adds is_admin for
// administrators only (API::Entities::UserWithAdmin); bot is in /user and
// /users/:id, not in the short users of lists and merge requests
// (UserBasic: id, username, name, state, locked, avatar_url, web_url).
type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Email    string `json:"email"`
	// PublicEmail is the email /users/:id shows.
	PublicEmail string `json:"public_email"`
	IsAdmin     bool   `json:"is_admin"`
	Bot         *bool  `json:"bot"`
}

// Probe reports the flavor, version and capabilities of the instance.
func (d *reader) Probe(ctx context.Context) (platform.Caps, error) {
	inst, err := d.c.instance(ctx)
	if err != nil {
		return platform.Caps{}, err
	}
	caps := platform.Caps{
		Flavor:       "gitlab",
		Version:      inst.version,
		MaxBody:      maxBody,
		Draft:        platform.DraftTitlePrefix,
		DraftPrefix:  draftPrefix,
		QuickActions: true,
		// closed_by is the user of the latest close
		// (merge_request.metrics.latest_closed_by, set by the close
		// service with the acting user, also when a deleted source branch
		// closes the MR: MergeRequests::RefreshService); merged_by who
		// merged. The live e2e confirms it on each version.
		CloserKnown: true,
		Marker:      platform.MarkerInBody,
		// A writer with Developer cannot read push rules (GET
		// /projects/:id/push_rule needs Maintainer): they show at push.
		RuntimeOnly: []string{"push_rules"},
		Limits:      limitsFor(d.c.host),
	}
	return caps, nil
}

// limitsFor returns the default pacing of host:
// gitlab.com has fixed limits (API 2000 per minute per user, notes 60 per
// minute), under which touchmark reads at most 600 times a minute; a
// self-managed instance has what its administrator set.
func limitsFor(host string) platform.Limits {
	h, _, _ := strings.Cut(strings.ToLower(host), ":")
	if h == publicHost {
		return platform.Limits{Reads: 8, GitReads: 4, ReadsPerMinute: 600, CommentsPerMinute: 50, MinInterval: 250 * time.Millisecond}
	}
	return platform.Limits{Reads: 8, GitReads: 4, MinInterval: 100 * time.Millisecond}
}

// instance finds the version once: GET /metadata, else GET /version (both
// need a signed-in user; an anonymous reader learns nothing and goes on).
// A GitLab older than minMajor is ClassUnsupported. A failure is not
// remembered.
func (c *client) instance(ctx context.Context) (instance, error) {
	c.mu.Lock()
	if c.inst != nil {
		inst := *c.inst
		c.mu.Unlock()
		return inst, nil
	}
	c.mu.Unlock()
	const op = "probe"
	var m apiMetadata
	resp, err := c.get(ctx, op, c.endpoint("metadata"), nil, &m)
	what := "GET /metadata"
	if err != nil && platform.ClassOf(err) == platform.ClassNotFound || err == nil && m.Version == "" {
		// No metadata endpoint, or an answer without the version: GET
		// /version has it too.
		m = apiMetadata{}
		resp, err = c.get(ctx, op, c.endpoint("version"), nil, &m)
		what = "GET /metadata and GET /version"
	}
	var inst instance
	switch {
	case err == nil && m.Version == "":
		body, status := "", 0
		if resp != nil {
			status = resp.Status
			body = oneLine(c.mask(string(resp.Body)))
			if len(body) > 200 {
				body = body[:200] + "..."
			}
		}
		return instance{}, shapeError(op, "%s: no version (HTTP %d: %s)", what, status, body)
	case err == nil:
		inst = instance{version: m.Version, enterprise: m.Enterprise != nil && *m.Enterprise}
		if major := majorOf(m.Version); major > 0 && major < minMajor {
			return instance{}, &platform.Error{Op: op, Class: platform.ClassUnsupported,
				Err: fmt.Errorf("GitLab %s: touchmark supports GitLab %d.0 and newer", m.Version, minMajor)}
		}
	case c.token == "" && (platform.ClassOf(err) == platform.ClassAuth || platform.ClassOf(err) == platform.ClassPermission ||
		platform.ClassOf(err) == platform.ClassNotFound):
		// The version is for signed-in users only.
		inst = instance{version: "unknown"}
	default:
		return instance{}, err
	}
	c.mu.Lock()
	c.inst = &inst
	c.mu.Unlock()
	return inst, nil
}

// majorOf returns the leading number of a version, 0 when there is none.
func majorOf(version string) int {
	head, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Self is the account the credential acts as. An administrator is refused:
// its token acts as any user through Sudo.
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
	var u apiUser
	if _, err := c.get(ctx, op, c.endpoint("user"), nil, &u); err != nil {
		return platform.Account{}, err
	}
	if u.ID <= 0 || u.Username == "" {
		return platform.Account{}, shapeError(op, "GET /user: no id or username")
	}
	if u.IsAdmin {
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassInvalid,
			Err: fmt.Errorf("the token of %s belongs to an administrator, who can act as any user through Sudo; "+
				"give touchmark a token of a service account or a group access token without administrator rights", u.Username)}
	}
	a := toAccount(&u)
	if u.Email != "" {
		a.Email = u.Email
	}
	c.mu.Lock()
	c.self = &a
	c.mu.Unlock()
	return a, nil
}

// Lookup resolves a login to its account; ErrNotFound when there is none.
// GET /users?username= matches the current username exactly, ignoring case
// (https://docs.gitlab.com/api/users/); an old username of a renamed user
// is not found. Its short user lacks the bot flag, so the account is read
// once more by id (GET /users/:id).
func (d *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	const op = "look up account"
	if login == "" || strings.ContainsAny(login, "/\x00?#,") || login == "." || login == ".." {
		return platform.Account{}, notFound(op, "account %q", login)
	}
	var list []apiUser
	if _, err := d.c.get(ctx, op, d.c.endpoint("users"), url.Values{"username": {login}}, &list); err != nil {
		return platform.Account{}, err
	}
	var match *apiUser
	for i := range list {
		if strings.EqualFold(list[i].Username, login) {
			match = &list[i]
			break
		}
	}
	switch {
	case match == nil:
		return platform.Account{}, notFound(op, "account %q", login)
	case match.ID <= 0:
		return platform.Account{}, shapeError(op, "GET /users?username=%s: a user without id", login)
	case kindOf(match.Username, nil) == platform.KindBot:
		// An access token's bot: the username tells.
		return toAccount(match), nil
	}
	full, err := d.c.user(ctx, op, match.ID)
	switch {
	case err == nil:
		return toAccount(full), nil
	case platform.ClassOf(err) == platform.ClassNotFound || platform.ClassOf(err) == platform.ClassPermission:
		// Hidden since (a private profile shows the user anyway): the
		// short user is what there is.
		return toAccount(match), nil
	}
	return platform.Account{}, err
}

// user returns user id as GET /users/:id reports it, looked up once per
// client.
func (c *client) user(ctx context.Context, op string, id int64) (*apiUser, error) {
	c.mu.Lock()
	u, ok := c.users[id]
	c.mu.Unlock()
	if ok {
		return u, nil
	}
	var got apiUser
	if _, err := c.get(ctx, op, c.endpoint("users", strconv.FormatInt(id, 10)), nil, &got); err != nil {
		return nil, err
	}
	if got.ID != id || got.Username == "" {
		return nil, shapeError(op, "GET /users/%d: another id or no username", id)
	}
	c.mu.Lock()
	c.users[id] = &got
	c.mu.Unlock()
	return &got, nil
}

// withKind returns a with its kind read from GET /users/:id when the short
// user it came from could not tell; a lookup that fails for good (the user
// is gone or hidden) leaves the kind the username tells. A rate limit, an
// auth failure, a transient failure or the end of ctx is returned.
func (c *client) withKind(ctx context.Context, op string, a *platform.Account) error {
	if a == nil || a.Kind == platform.KindBot {
		return nil
	}
	c.mu.Lock()
	me := c.self
	c.mu.Unlock()
	if me != nil && me.ID == a.ID {
		// The credential's own account, read by Self already.
		a.Kind = me.Kind
		return nil
	}
	id, err := strconv.ParseInt(a.ID, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	u, err := c.user(ctx, op, id)
	if err != nil {
		if fatal(err) {
			return err
		}
		return nil
	}
	a.Kind = kindOf(u.Username, u.Bot)
	return nil
}

// botName matches the users GitLab creates for project and group access
// tokens: project_<id>_bot_<hex> and group_<id>_bot_<hex>, older ones
// project_<id>_bot and project_<id>_bot<n>
// (app/services/resource_access_tokens/create_service.rb).
var botName = regexp.MustCompile(`(?i)^(project|group)_\d+_bot(\d+|_[0-9a-f]+)?$`)

// kindOf tells people from automation by the username and the bot flag
// (nil when the API did not show it). The REST API has no user type:
// every non-human user has bot true (User#bot?, which includes service
// accounts), so:
//   - a username of an access token's bot is KindBot, flag or not;
//   - bot true with a username starting "service_account" (GitLab's
//     default for service accounts) is KindServiceAccount; with "bot" in
//     it (GitLab's own bots: support-bot, alert-bot, GitLab-Admin-Bot, …,
//     or an account named so) KindBot; else a service account with a
//     username of its own;
//   - bot false is KindUser; no flag KindUnknown.
func kindOf(username string, bot *bool) platform.AccountKind {
	lower := strings.ToLower(username)
	switch {
	case botName.MatchString(username):
		return platform.KindBot
	case bot == nil:
		return platform.KindUnknown
	case !*bot:
		return platform.KindUser
	case strings.HasPrefix(lower, "service_account"):
		return platform.KindServiceAccount
	case strings.Contains(lower, "bot"):
		return platform.KindBot
	}
	return platform.KindServiceAccount
}

// toAccount converts an API user.
func toAccount(u *apiUser) platform.Account {
	if u == nil {
		return platform.Account{}
	}
	return platform.Account{
		ID:    strconv.FormatInt(u.ID, 10),
		Login: u.Username,
		Email: u.PublicEmail,
		Kind:  kindOf(u.Username, u.Bot),
	}
}
