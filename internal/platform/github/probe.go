package github

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// maxBody is the body budget: GitHub refuses a body over 65 536 characters
// (422 "Body is too long"); 58 000 leaves room for what proxies in front of
// it add. The core measures bodies in bytes, and a character is at least
// one byte in UTF-8 whether GitHub counts code points or UTF-16 units, so
// a budget of 58 000 bytes keeps a body under 58 000 characters.
const maxBody = 58000

// Default limits: github.com and GHE.com create content at most 80 times a
// minute and 500 an hour (secondary limits); GHES has no limits by
// default, but the same one second between writes keeps it calm.
var (
	cloudLimits = platform.Limits{Reads: 8, GitReads: 4, WritesPerMinute: 60, WritesPerHour: 450, MinInterval: time.Second}
	ghesLimits  = platform.Limits{Reads: 8, GitReads: 4, MinInterval: time.Second}
)

// instance is what Probe finds out about the server.
type instance struct {
	flavor  string
	version string // GHES only
}

// apiMeta is GET /meta of GitHub Enterprise Server.
type apiMeta struct {
	InstalledVersion string `json:"installed_version"`
}

// apiUser is a user, bot or organization as the API reports it.
type apiUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // User, Bot, Organization
	Email string `json:"email"`
}

// apiApp is GET /app.
type apiApp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Probe reports the flavor, version and capabilities.
//
// The flavor comes from the host: github.com, *.ghe.com (GHE.com), else
// GitHub Enterprise Server, whose version GET /meta names
// (installed_version), or the X-GitHub-Enterprise-Version header every
// answer of it carries.
//
// An App commits through the API (Commit.API, through a stage ref) with a
// compare-and-swap (updateRefs beforeOid); a token does not: GitHub does
// not sign REST commits of tokens. github.com and GHE.com sign
// the App's API commits; GHES only when its administrator enabled web
// commit signing, which the API does not show: SignedByPlatform is false
// there, and the first Commit tells (Verified, or ErrUnsigned).
//
// Who closed a pull request needs GraphQL, which an anonymous reader
// cannot use: CloserKnown is false then.
func (d *reader) Probe(ctx context.Context) (platform.Caps, error) {
	inst, err := d.c.instance(ctx)
	if err != nil {
		return platform.Caps{}, err
	}
	caps := platform.Caps{
		Flavor:       inst.flavor,
		Version:      inst.version,
		MaxBody:      maxBody,
		Draft:        platform.DraftNative,
		WorkflowPerm: true,
		CloserKnown:  d.c.kind != credAnonymous,
		Marker:       platform.MarkerInBody,
		// Classic branch protection is read with Administration only: the
		// writer meets it when it pushes.
		RuntimeOnly: []string{"branch_protection"},
		Limits:      cloudLimits,
	}
	caps.Commit.API = d.c.kind == credApp
	caps.Commit.SignedByPlatform = caps.Commit.API && inst.flavor != flavorGHES
	caps.Commit.CAS = true
	if inst.flavor == flavorGHES {
		caps.Limits = ghesLimits
	}
	return caps, nil
}

// instance finds the flavor and, on GHES, the version once. A failure is
// not remembered.
func (c *client) instance(ctx context.Context) (instance, error) {
	c.mu.Lock()
	if c.inst != nil {
		inst := *c.inst
		c.mu.Unlock()
		return inst, nil
	}
	c.mu.Unlock()
	inst := instance{flavor: c.flavor}
	if c.flavor == flavorGHES {
		v, err := c.ghesVersion(ctx)
		if err != nil {
			return instance{}, err
		}
		inst.version = v
	}
	c.mu.Lock()
	c.inst = &inst
	c.mu.Unlock()
	return inst, nil
}

// ghesVersion asks GET /meta for installed_version, with the token (or
// anonymously for an App: /meta takes no JWT); an answer without it, or a
// refusal, still names the version in X-GitHub-Enterprise-Version.
func (c *client) ghesVersion(ctx context.Context) (string, error) {
	const op = "probe"
	var a *httpx.Auth
	if c.kind == credToken {
		a = c.staticAuth("Bearer " + c.token)
	}
	var m apiMeta
	resp, err := c.get(ctx, op, c.endpoint("meta"), nil, a, &m)
	if m.InstalledVersion != "" {
		return m.InstalledVersion, nil
	}
	if resp != nil {
		if v := strings.TrimSpace(resp.Header.Get("X-GitHub-Enterprise-Version")); v != "" {
			return v, nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", shapeError(op, "GET /meta names no installed_version, and the answer no X-GitHub-Enterprise-Version: is %s a GitHub Enterprise Server?", c.host)
}

// Self is the account the credential acts as. An App is
// its bot, "<slug>[bot]": GET /app with the JWT names the slug (an
// installation token cannot call GET /user), GET /users/<slug>[bot] its
// id, and its commit email is "<id>+<slug>[bot]@users.noreply.github.com".
// A token is GET /user.
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
	var a platform.Account
	switch c.kind {
	case credAnonymous:
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassAuth, Err: errors.New("no credential: the reader is anonymous")}
	case credToken:
		var u apiUser
		_, err := c.get(ctx, op, c.endpoint("user"), nil, c.staticAuth("Bearer "+c.token), &u)
		if platform.ClassOf(err) == platform.ClassPermission && strings.Contains(messageOf(err), "not accessible by integration") {
			return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassInvalid, Status: statusOf(err),
				Err: errors.New("the token is an installation token, which cannot name its account; give touchmark the GitHub App's id and key instead")}
		}
		if err != nil {
			return platform.Account{}, err
		}
		if u.ID <= 0 || u.Login == "" {
			return platform.Account{}, shapeError(op, "GET /user: no id or login")
		}
		a = c.toAccount(&u)
	case credApp:
		auth, err := c.app.jwtAuth()
		if err != nil {
			return platform.Account{}, err
		}
		var ap apiApp
		if _, err := c.get(ctx, op, c.endpoint("app"), nil, auth, &ap); err != nil {
			return platform.Account{}, err
		}
		if ap.Slug == "" {
			return platform.Account{}, shapeError(op, "GET /app: no slug")
		}
		a, err = c.lookup(ctx, ap.Slug+"[bot]")
		if err != nil {
			return platform.Account{}, err
		}
		if a.Kind != platform.KindBot {
			return platform.Account{}, shapeError(op, "the App's bot %s[bot] is not a bot", ap.Slug)
		}
	}
	c.mu.Lock()
	c.self = &a
	c.mu.Unlock()
	return a, nil
}

// Lookup resolves a login to its account (GET /users/{login}); ErrNotFound
// when there is none. A bot's login ends in "[bot]".
func (d *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	return d.c.lookup(ctx, login)
}

func (c *client) lookup(ctx context.Context, login string) (platform.Account, error) {
	const op = "look up account"
	if login == "" || strings.ContainsAny(login, "/\x00?#\\") || login == "." || login == ".." {
		return platform.Account{}, notFound(op, "account %q", login)
	}
	a, err := c.anyAuth(ctx)
	if err != nil {
		return platform.Account{}, err
	}
	var u apiUser
	if _, err := c.get(ctx, op, c.endpoint("users", login), nil, a, &u); err != nil {
		return platform.Account{}, err
	}
	if u.ID <= 0 || u.Login == "" {
		return platform.Account{}, shapeError(op, "GET /users/%s: no id or login", login)
	}
	return c.toAccount(&u), nil
}

// toAccount converts an API user. Bots are type "Bot"; an organization is
// no author, and a type the driver does not know is unknown. The email is
// the public one, else the id-based noreply address, which GitHub ties to
// the account (for a bot always: its commit email).
func (c *client) toAccount(u *apiUser) platform.Account {
	if u == nil {
		return platform.Account{}
	}
	a := platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Login, Email: u.Email}
	switch u.Type {
	case "Bot":
		a.Kind, a.Email = platform.KindBot, ""
	case "User":
		a.Kind = platform.KindUser
	default:
		a.Kind = platform.KindUnknown
	}
	if a.Email == "" && u.ID > 0 && u.Login != "" {
		a.Email = c.noreply(u.ID, u.Login)
	}
	return a
}

// noreply is the id-based noreply address of an account: users.noreply.
// github.com on github.com
// (https://docs.github.com/account-and-profile/reference/email-addresses-reference),
// users.noreply.<host> elsewhere (unverified on GHE.com and GHES).
func (c *client) noreply(id int64, login string) string {
	host := c.host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return fmt.Sprintf("%d+%s@users.noreply.%s", id, login, host)
}
