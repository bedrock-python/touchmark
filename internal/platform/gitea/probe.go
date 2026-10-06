package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// Capabilities shared by Gitea and Forgejo.
const (
	// maxBody is the body budget: the API sets none (the column is
	// LONGTEXT); 58 000 keeps parity with GitHub's 65 536-character limit
	// and under the 1 MiB proxies usually allow.
	maxBody = 58000
	// draftPrefix is the default WORK_IN_PROGRESS_PREFIXES entry; the
	// instance's list is not readable through the API, so CreatePR checks
	// the draft flag the server derives from the title.
	draftPrefix = "WIP: "
)

// actionsUserID is the id of the Actions bot (gitea-actions,
// forgejo-actions); a deleted user shows as the ghost, id -1
// (models/user/user_system.go of both platforms).
const actionsUserID = -2

// instance is what Probe finds out about the server.
type instance struct {
	flavor  string // "gitea" or "forgejo"
	version string
	major   int // Forgejo's major version, 0 for Gitea
	// signIn is set when the instance shows nothing to anyone who is not
	// signed in ([service] REQUIRE_SIGNIN_VIEW = true): a repository that is
	// not private is then visible to signed-in users only.
	signIn bool
}

// headFilter reports whether the pull request list filters by head branch
// (Forgejo 16). The client filters anyway; the server filter saves pages.
func (i instance) headFilter() bool { return i.flavor == "forgejo" && i.major >= 16 }

// apiVersion is GET /version and GET /api/forgejo/v1/version.
type apiVersion struct {
	Version string `json:"version"`
}

// apiUser is a user as the API reports it; is_admin only in /user.
type apiUser struct {
	ID      int64  `json:"id"`
	Login   string `json:"login"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
}

// Probe reports the flavor, version and capabilities of the instance.
func (d *reader) Probe(ctx context.Context) (platform.Caps, error) {
	inst, err := d.c.instance(ctx)
	if err != nil {
		return platform.Caps{}, err
	}
	return platform.Caps{
		Flavor:      inst.flavor,
		Version:     inst.version,
		MaxBody:     maxBody,
		Draft:       platform.DraftTitlePrefix,
		DraftPrefix: draftPrefix,
		LabelsByID:  true,
		// The timeline names who closed a pull request (a "close" event),
		// merged_by who merged it: checked on Gitea 1.26 and 1.27 and
		// Forgejo 15 and 16.
		CloserKnown: true,
		Marker:      platform.MarkerInBody,
		Limits:      platform.Limits{Reads: 4, GitReads: 2, MinInterval: 250 * time.Millisecond},
	}, nil
}

// instance finds the flavor and version once: Forgejo
// answers its own version endpoint with 200, Gitea with 404. Forgejo's
// version drops the "+gitea-…" compatibility suffix it carries. It also
// finds whether the instance requires signing in to see anything
// (signInRequired). A failure is not remembered.
func (c *client) instance(ctx context.Context) (instance, error) {
	c.mu.Lock()
	if c.inst != nil {
		inst := *c.inst
		c.mu.Unlock()
		return inst, nil
	}
	c.mu.Unlock()
	const op = "probe"
	inst, found, err := c.forgejo(ctx)
	if err != nil {
		return instance{}, err
	}
	if !found {
		var v apiVersion
		if _, err := c.get(ctx, op, c.endpoint("version"), nil, &v); err != nil {
			return instance{}, err
		}
		if v.Version == "" {
			return instance{}, shapeError(op, "GET /version: no version")
		}
		inst = instance{flavor: "gitea", version: v.Version}
		// Where Forgejo's endpoint is unknown (a custom api_url), its frozen
		// suffix still tells it from Gitea.
		if main, suffix, ok := strings.Cut(v.Version, "+"); ok && strings.HasPrefix(suffix, "gitea-") && c.forgejoURL == "" {
			inst = instance{flavor: "forgejo", version: main, major: majorOf(main)}
		}
	}
	if inst.signIn, err = c.signInRequired(ctx); err != nil {
		return instance{}, err
	}
	c.mu.Lock()
	c.inst = &inst
	c.mu.Unlock()
	return inst, nil
}

// signInRequired asks the API for its version anonymously. An instance
// with [service] REQUIRE_SIGNIN_VIEW = true answers every API request of
// someone not signed in with 403 "Only signed in user is allowed to call
// APIs." (the middleware in front of all routes of routers/api/v1/api.go;
// checked on Gitea 1.26 and 1.27 and Forgejo 15 and 16), and shows its
// repositories that are not private to signed-in users only. Any answer
// but a version (a 401 or 403, a proxy's login page or redirect, a 404)
// counts as sign-in required, the safe side for a public hub, whose CI
// logs anyone reads; a transient failure or a rate limit is returned. An
// anonymous reader asks nothing: its own requests show what anyone sees.
func (c *client) signInRequired(ctx context.Context) (bool, error) {
	if c.token == "" {
		return false, nil
	}
	var v apiVersion
	_, err := c.http.JSON(ctx, http.MethodGet, c.endpoint("version"), nil, nil, &v)
	if err == nil {
		return v.Version == "", nil
	}
	perr := c.apiError("probe anonymously", err)
	switch platform.ClassOf(perr) {
	case platform.ClassTransient, platform.ClassRateLimited:
		return false, perr
	}
	if errors.Is(err, context.Canceled) {
		return false, perr
	}
	return true, nil
}

// forgejo asks Forgejo's version endpoint; found is false on 404 or an
// answer that is no Forgejo version (a proxy's page).
func (c *client) forgejo(ctx context.Context) (instance, bool, error) {
	if c.forgejoURL == "" {
		return instance{}, false, nil
	}
	var v apiVersion
	_, err := c.get(ctx, "probe", c.forgejoURL, nil, &v)
	switch {
	case err == nil && v.Version != "":
		version, _, _ := strings.Cut(v.Version, "+")
		return instance{flavor: "forgejo", version: version, major: majorOf(version)}, true, nil
	case err == nil, platform.ClassOf(err) == platform.ClassNotFound, platform.ClassOf(err) == platform.ClassUnknown && decodeError(err):
		return instance{}, false, nil
	}
	return instance{}, false, err
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
	if u.ID <= 0 || u.Login == "" {
		return platform.Account{}, shapeError(op, "GET /user: no id or login")
	}
	if u.IsAdmin {
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassInvalid,
			Err: fmt.Errorf("the token of %s belongs to a site administrator, who can act as any user through Sudo; "+
				"give touchmark a token of a bot user without administrator rights", u.Login)}
	}
	a := toAccount(&u)
	c.mu.Lock()
	c.self = &a
	c.mu.Unlock()
	return a, nil
}

// Lookup resolves a login to its account; ErrNotFound when there is none.
func (d *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	const op = "look up account"
	if login == "" || strings.ContainsAny(login, "/\x00?#") || login == "." || login == ".." {
		return platform.Account{}, notFound(op, "account %q", login)
	}
	var u apiUser
	if _, err := d.c.get(ctx, op, d.c.endpoint("users", login), nil, &u); err != nil {
		return platform.Account{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return platform.Account{}, shapeError(op, "GET /users/%s: no id or login", login)
	}
	return toAccount(&u), nil
}

// toAccount converts an API user. Neither platform tells bots from people
// in its API (Gitea's bot user type is not exported), so every account is a
// user but the Actions bot; a deleted user (the ghost) is unknown.
func toAccount(u *apiUser) platform.Account {
	if u == nil {
		return platform.Account{}
	}
	a := platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Email: u.Email, Kind: platform.KindUser}
	switch {
	case u.ID == actionsUserID:
		a.Kind = platform.KindBot
	case u.ID <= 0:
		a.Kind = platform.KindUnknown
	}
	return a
}
