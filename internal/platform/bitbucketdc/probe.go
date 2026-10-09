package bitbucketdc

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

// Capabilities of Bitbucket Data Center.
const (
	// maxBody is the body budget: a description holds at most 32 768
	// characters (Atlassian's tracker, BSERV-14135), and touchmark's body
	// stays below with room to spare.
	maxBody = 30000
	// minVersion is the oldest version touchmark supports: 8.19, the
	// oldest long-term support release it was built against (drafts need
	// 8.18).
	minMajor, minMinor = 8, 19
)

// limits are the default pacing. Bitbucket meters each user with a token
// bucket (by default 60 requests, refilled at 5 a second, per node of a
// cluster): 120 reads a minute, four targets and two git fetches at once,
// and half a second between two writes stay well within it.
var limits = platform.Limits{Reads: 4, GitReads: 2, ReadsPerMinute: 120, MinInterval: 500 * time.Millisecond}

// branchPermissions is the check the writer cannot make upfront: branch
// permissions are readable with repository admin rights only, so a push
// meets them ("Branch … can only be modified through pull requests").
const branchPermissions = "branch-permissions"

// versionRe matches the version of application-properties: 8.19.3, 10.2.0.
var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// apiUser is a user as the API reports it (RestApplicationUser): slug is
// the login, name the user name (which X-AUSERNAME carries), type NORMAL
// for a person and SERVICE for a service user, such as the user of a
// project or repository access token.
type apiUser struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	Active       *bool  `json:"active"`
	Type         string `json:"type"`
}

// Probe reads the instance's version (GET /application-properties, which
// needs no credential) and reports the capabilities of Bitbucket Data
// Center. A version older than 8.19 is ClassUnsupported.
func (d *reader) Probe(ctx context.Context) (platform.Caps, error) {
	const op = "probe"
	v, raw, err := d.c.instanceVersion(ctx, op)
	if err != nil {
		return platform.Caps{}, err
	}
	if v[0] < minMajor || v[0] == minMajor && v[1] < minMinor {
		return platform.Caps{}, &platform.Error{Op: op, Class: platform.ClassUnsupported,
			Err: errors.New("Bitbucket Data Center " + raw + " is older than 8.19, the oldest version touchmark supports")}
	}
	return platform.Caps{
		Flavor:  "bitbucket-datacenter",
		Version: raw,
		MaxBody: maxBody,
		Draft:   platform.DraftNative,
		// Pull requests have no labels.
		NoLabels: true,
		// The activities of a declined pull request name who declined it.
		CloserKnown: true,
		// Whether a declined pull request can be edited is not documented:
		// touchmark writes to none.
		ClosedImmutable: true,
		// Declining a pull request needs read access only.
		ReaderCloses: true,
		// Markdown escapes HTML: an HTML comment would show.
		Marker:      platform.MarkerInRefDef,
		RuntimeOnly: []string{branchPermissions},
		Limits:      limits,
	}, nil
}

// instanceVersion reads and caches the instance's version.
func (c *client) instanceVersion(ctx context.Context, op string) ([3]int, string, error) {
	var props struct {
		Version string `json:"version"`
	}
	if _, err := c.get(ctx, op, c.endpoint("application-properties"), nil, &props); err != nil {
		return [3]int{}, "", err
	}
	m := versionRe.FindStringSubmatch(strings.TrimSpace(props.Version))
	if m == nil {
		return [3]int{}, "", shapeError(op, "application-properties: version %q", props.Version)
	}
	var v [3]int
	for i := range 3 {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	c.mu.Lock()
	c.version = &v
	c.mu.Unlock()
	return v, strings.TrimSpace(props.Version), nil
}

// Self is the user the credential acts as.
func (d *reader) Self(ctx context.Context) (platform.Account, error) {
	return d.c.selfAccount(ctx)
}

// selfAccount returns the credential's user, looked up once. Bitbucket has
// no "who am I" endpoint: every authenticated answer carries X-AUSERNAME,
// the user's name, so a cheap read (GET /application-properties) gives
// the name, and GET /users?filter=<name> the user whose name it is (an
// Atlassian engineer's advice, not in the reference). An answer without
// the header, or a name no user has, is ClassUnknown.
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
	resp, err := c.get(ctx, op, c.endpoint("application-properties"), nil, nil)
	if err != nil {
		return platform.Account{}, err
	}
	name := strings.TrimSpace(resp.Header.Get("X-AUSERNAME"))
	if name == "" {
		return platform.Account{}, &platform.Error{Op: op, Class: platform.ClassUnknown,
			Err: errors.New("the answer names no user (no X-AUSERNAME header): the token may be refused, or a proxy drops the header")}
	}
	// Jira percent-encodes the header's value; Bitbucket is not known to,
	// and a name with an escape is read both ways.
	if unescaped, uerr := url.PathUnescape(name); uerr == nil && unescaped != name {
		name = unescaped
	}
	var found *apiUser
	q := url.Values{"filter": {name}}
	_, err = listAll(ctx, c, op, c.endpoint("users"), q, pageLimit, 10, func(u apiUser) error {
		if found == nil && u.Name == name {
			found = &u
		}
		return nil
	})
	switch {
	case err != nil:
		return platform.Account{}, err
	case found == nil:
		return platform.Account{}, shapeError(op, "no user is named %q, the name X-AUSERNAME gives", name)
	}
	a := toAccount(found)
	if a.ID == "" || a.Login == "" {
		return platform.Account{}, shapeError(op, "user %q has no id or slug", name)
	}
	if validEmail(found.EmailAddress) {
		a.Email = found.EmailAddress
	}
	c.remember(found)
	c.mu.Lock()
	c.self = &a
	c.mu.Unlock()
	return a, nil
}

// validEmail reports whether s can be a commit author's address: one "@"
// between non-empty parts, and nothing git would refuse or misread.
func validEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		!strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == '<' || r == '>' || r == 0x7f })
}

// Lookup resolves a login, a user's slug, to its user: GET /users/{slug}.
// A slug the instance does not know is ErrNotFound.
func (d *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	const op = "look up account"
	if login == "" || strings.ContainsAny(login, "/\x00?#") || login == "." || login == ".." {
		return platform.Account{}, invalid(op, "%q is no user slug", login)
	}
	var u apiUser
	if _, err := d.c.get(ctx, op, d.c.endpoint("users", login), nil, &u); err != nil {
		return platform.Account{}, err
	}
	a := toAccount(&u)
	if a.ID == "" || a.Login == "" {
		return platform.Account{}, shapeError(op, "GET /users/%s: no id or slug", login)
	}
	d.c.remember(&u)
	return a, nil
}

// remember keeps the name of u by its id.
func (c *client) remember(u *apiUser) {
	if u == nil || u.ID <= 0 || u.Name == "" {
		return
	}
	c.mu.Lock()
	c.names[strconv.FormatInt(u.ID, 10)] = u.Name
	c.mu.Unlock()
}

// nameOf returns the name of the user with id, "" when Self or Lookup has
// not resolved it.
func (c *client) nameOf(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.names[id]
}

// toAccount converts a user: its numeric id is the account's id, its slug
// its login. A service user is a bot, a normal user a person; a user of
// another type is unknown.
func toAccount(u *apiUser) platform.Account {
	if u == nil || u.ID <= 0 || u.Slug == "" {
		return platform.Account{}
	}
	a := platform.Account{ID: strconv.FormatInt(u.ID, 10), Login: u.Slug, Kind: platform.KindUnknown}
	switch u.Type {
	case "NORMAL":
		a.Kind = platform.KindUser
	case "SERVICE":
		a.Kind = platform.KindBot
	}
	return a
}
