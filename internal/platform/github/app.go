package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// JWT and token lifetimes (https://docs.github.com/apps/creating-github-
// apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-
// for-a-github-app): iat 60 s in the past against clock drift, exp at most
// 10 minutes ahead. The JWT is signed for 9 minutes, so a clock up to a
// minute fast still passes, and signed again 2 minutes before it expires.
// Installation tokens live an hour and are minted again 10 minutes before
// expires_at.
const (
	jwtBackdate    = 60 * time.Second
	jwtLifetime    = 9 * time.Minute
	jwtRenewBefore = 2 * time.Minute
	tokenRenew     = 10 * time.Minute
)

// readPermissions are what a reading installation token asks for: the
// reader App's permissions (Metadata, Contents and Pull requests read);
// the writer reads with the same narrowed token.
var readPermissions = map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}

// maxInstallationPages bounds GET /app/installations: 10 000 installations.
const maxInstallationPages = 100

// app is a GitHub App credential: its id, its key, the JWT it signs, the
// installations of owners and their reading tokens. It is safe for
// concurrent use.
type app struct {
	c   *client
	id  string
	key *rsa.PrivateKey

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
	// owners caches the installation of each owner (lowercased login);
	// tokens the reading token of each installation.
	owners map[string]*ownerInstallation
	tokens map[int64]*instToken
	// perms caches the permissions of each installation, as the lookups of
	// installations report them.
	perms map[int64]map[string]string
	// any is an installation GET /app/installations named, for requests
	// about no repository; dead are installations whose token GitHub
	// refused as suspended.
	any  int64
	dead map[int64]bool
}

// ownerInstallation is the installation of one owner, or why there is
// none (a missing one is remembered: the App is not installed there).
// suspended is set when the lookup reported it suspended: it mints no
// token, so requests about no repository never go through it.
type ownerInstallation struct {
	id        int64
	err       error
	suspended bool
}

// instToken is the reading token of one installation; mu serializes its
// minting.
type instToken struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

// newApp parses the App key: PKCS #1 ("RSA PRIVATE KEY", what GitHub
// gives) or PKCS #8 ("PRIVATE KEY") RSA.
func newApp(c *client, id string, keyPEM []byte) (*app, error) {
	if id == "" {
		return nil, errors.New("a GitHub App without an id")
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("the GitHub App key is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("the GitHub App key does not parse as an RSA private key")
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		rk, ok := k.(*rsa.PrivateKey)
		if err != nil || !ok {
			return nil, errors.New("the GitHub App key is not an RSA private key")
		}
		key = rk
	default:
		return nil, fmt.Errorf("the GitHub App key is a %q block, not an RSA private key", block.Type)
	}
	return &app{c: c, id: id, key: key, owners: map[string]*ownerInstallation{}, tokens: map[int64]*instToken{},
		perms: map[int64]map[string]string{}, dead: map[int64]bool{}}, nil
}

// jwtClaims are the claims GitHub reads. iss is the App's id (or client
// id) as a string, as the documentation's examples send it.
type jwtClaims struct {
	IAT int64  `json:"iat"`
	EXP int64  `json:"exp"`
	ISS string `json:"iss"`
}

// jwtHeader is the fixed header of every JWT.
var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

// token returns the App's JWT, signed again when it expires within
// jwtRenewBefore. Each new JWT is registered as a secret.
func (a *app) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.c.now()
	if a.jwt != "" && now.Before(a.jwtExp.Add(-jwtRenewBefore)) {
		return a.jwt, nil
	}
	claims, err := json.Marshal(jwtClaims{IAT: now.Add(-jwtBackdate).Unix(), EXP: now.Add(jwtLifetime).Unix(), ISS: a.id})
	if err != nil {
		return "", err
	}
	signed := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign the GitHub App's JWT: %w", err)
	}
	jwt := signed + "." + base64.RawURLEncoding.EncodeToString(sig)
	a.c.register(jwt)
	a.jwt, a.jwtExp = jwt, now.Add(jwtLifetime)
	return jwt, nil
}

// jwtAuth is the credential of the App itself: its JWT as a Bearer token
// (https://docs.github.com/rest/authentication/authenticating-to-the-rest-api:
// a JWT must go as Bearer).
func (a *app) jwtAuth() (*httpx.Auth, error) {
	jwt, err := a.token()
	if err != nil {
		return nil, &platform.Error{Op: "sign the App's JWT", Class: platform.ClassAuth, Err: err}
	}
	return a.c.staticAuth("Bearer " + jwt), nil
}

// apiInstallation is an installation as the API reports it.
type apiInstallation struct {
	ID          int64             `json:"id"`
	Account     *apiUser          `json:"account"`
	AppSlug     string            `json:"app_slug"`
	Permissions map[string]string `json:"permissions"`
	SuspendedAt *time.Time        `json:"suspended_at"`
}

// installation returns the installation of owner: GET
// /orgs/{owner}/installation, else GET /users/{owner}/installation, as the
// App. An owner without one is ClassNotFound (remembered for the run);
// other failures are returned and not remembered.
func (a *app) installation(ctx context.Context, owner string) (int64, error) {
	const op = "find the App's installation"
	key := strings.ToLower(owner)
	a.mu.Lock()
	if oi, ok := a.owners[key]; ok {
		a.mu.Unlock()
		return oi.id, oi.err
	}
	a.mu.Unlock()
	auth, err := a.jwtAuth()
	if err != nil {
		return 0, err
	}
	var inst apiInstallation
	_, err = a.c.get(ctx, op, a.c.endpoint("orgs", owner, "installation"), nil, auth, &inst)
	if platform.ClassOf(err) == platform.ClassNotFound {
		inst = apiInstallation{}
		_, err = a.c.get(ctx, op, a.c.endpoint("users", owner, "installation"), nil, auth, &inst)
	}
	switch {
	case platform.ClassOf(err) == platform.ClassNotFound:
		err = &platform.Error{Op: op, Class: platform.ClassNotFound, Status: http.StatusNotFound,
			Err: fmt.Errorf("the GitHub App is not installed on %s: %w", owner, platform.ErrNotFound)}
	case err != nil:
		return 0, err
	case inst.ID <= 0:
		return 0, shapeError(op, "an installation of %s without an id", owner)
	}
	a.mu.Lock()
	a.owners[key] = &ownerInstallation{id: inst.ID, err: err, suspended: inst.SuspendedAt != nil}
	if err == nil && inst.Permissions != nil {
		a.perms[inst.ID] = inst.Permissions
	}
	a.mu.Unlock()
	return inst.ID, err
}

// installationPerms returns the permissions of the installation that
// covers owner/name: those a lookup of the owner's installation cached,
// else GET /repos/{owner}/{repo}/installation (cached in turn). The
// permissions are the installation's, whichever of its repositories asks;
// whether r is among them is Target's to tell. An App not installed there
// is ClassNotFound.
func (a *app) installationPerms(ctx context.Context, op, owner, name string) (map[string]string, error) {
	a.mu.Lock()
	var cached map[string]string
	if oi, ok := a.owners[strings.ToLower(owner)]; ok && oi.err == nil {
		cached = a.perms[oi.id]
	}
	a.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	inst, err := a.repoInstallation(ctx, op, owner, name)
	if err != nil {
		return nil, err
	}
	return inst.Permissions, nil
}

// repoInstallation returns the installation of repository owner/name with
// its permissions: GET /repos/{owner}/{repo}/installation, as the App. A
// repository the App is not installed on, or that does not exist, is
// ClassNotFound. The owner's installation is remembered.
func (a *app) repoInstallation(ctx context.Context, op, owner, name string) (*apiInstallation, error) {
	inst, err := a.findRepoInstallation(ctx, op, owner, name)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.owners[strings.ToLower(owner)] = &ownerInstallation{id: inst.ID, suspended: inst.SuspendedAt != nil}
	a.mu.Unlock()
	return inst, nil
}

// findRepoInstallation is repoInstallation without remembering the
// installation as owner's; the permissions are cached per installation.
// GitHub answers the old path of a transferred repository with a redirect,
// which the GET follows: the installation is then the new owner's.
func (a *app) findRepoInstallation(ctx context.Context, op, owner, name string) (*apiInstallation, error) {
	auth, err := a.jwtAuth()
	if err != nil {
		return nil, err
	}
	var inst apiInstallation
	if _, err := a.c.get(ctx, op, a.c.repoURL(owner, name, "installation"), nil, auth, &inst); err != nil {
		if platform.ClassOf(err) == platform.ClassNotFound {
			return nil, &platform.Error{Op: op, Class: platform.ClassNotFound, Status: http.StatusNotFound,
				Err: fmt.Errorf("the GitHub App is not installed on %s/%s, or it does not exist: %w", owner, name, platform.ErrNotFound)}
		}
		return nil, err
	}
	if inst.ID <= 0 {
		return nil, shapeError(op, "an installation of %s/%s without an id", owner, name)
	}
	if inst.Permissions != nil {
		a.mu.Lock()
		a.perms[inst.ID] = inst.Permissions
		a.mu.Unlock()
	}
	return &inst, nil
}

// repoToken returns the reading token of the installation that covers
// repository owner/name, for a repository whose owner has no installation
// of its own (Repo): one transferred to an owner the App is installed on
// keeps a redirect from its old path. The owner is not remembered as the
// installation's.
func (a *app) repoToken(ctx context.Context, op, owner, name string) (string, error) {
	inst, err := a.findRepoInstallation(ctx, op, owner, name)
	if err != nil {
		return "", err
	}
	return a.readToken(ctx, inst.ID)
}

// ownerToken returns the reading token of owner's installation.
func (a *app) ownerToken(ctx context.Context, owner string) (string, error) {
	id, err := a.installation(ctx, owner)
	if err != nil {
		return "", err
	}
	return a.readToken(ctx, id)
}

// anyToken returns a reading token for requests about no repository
// (users, the instance): that of an installation of a target's owner
// already looked up and not suspended (the lowest id, for a stable
// choice), else of the first installation GET /app/installations lists
// that is not suspended (paged until one is found). A suspended
// installation mints no token ("This installation has been suspended"),
// so one suspended installation never stops the provider. An App without
// an installation that is not suspended is ClassNotFound (anyAuth then
// goes anonymous).
func (a *app) anyToken(ctx context.Context) (string, error) {
	const op = "find an installation of the App"
	for attempt := 0; ; attempt++ {
		a.mu.Lock()
		id := a.any
		if id == 0 {
			for _, oi := range a.owners {
				if oi.err == nil && oi.id > 0 && !oi.suspended && !a.dead[oi.id] && (id == 0 || oi.id < id) {
					id = oi.id
				}
			}
		}
		a.mu.Unlock()
		if id == 0 {
			found, err := a.liveInstallation(ctx, op)
			if err != nil {
				return "", err
			}
			id = found
			a.mu.Lock()
			a.any = id
			a.mu.Unlock()
		}
		tok, err := a.readToken(ctx, id)
		if err == nil || !suspendedError(err) || attempt > 0 {
			return tok, err
		}
		// Suspended since it was listed or looked up: try another.
		a.mu.Lock()
		a.dead[id] = true
		if a.any == id {
			a.any = 0
		}
		a.mu.Unlock()
	}
}

// suspendedError reports whether err is GitHub's refusal to mint a token
// of a suspended installation (403 "This installation has been
// suspended").
func suspendedError(err error) bool {
	return platform.ClassOf(err) == platform.ClassPermission && strings.Contains(messageOf(err), "suspended")
}

// errFound stops a listing once it found what it looked for.
var errFound = errors.New("found")

// liveInstallation returns the id of the first installation GET
// /app/installations lists that is not suspended (nor refused a token
// earlier in the run), reading at most maxInstallationPages pages.
func (a *app) liveInstallation(ctx context.Context, op string) (int64, error) {
	auth, err := a.jwtAuth()
	if err != nil {
		return 0, err
	}
	var id int64
	suspended := 0
	_, err = listAll(ctx, a.c, op, a.c.endpoint("app", "installations"), nil, auth, maxInstallationPages, func(i apiInstallation) error {
		a.mu.Lock()
		dead := a.dead[i.ID]
		a.mu.Unlock()
		switch {
		case i.ID <= 0:
		case i.SuspendedAt != nil || dead:
			suspended++
		default:
			id = i.ID
			return errFound
		}
		return nil
	})
	switch {
	case id > 0:
		return id, nil
	case err != nil && !errors.Is(err, errFound):
		return 0, err
	case suspended > 0:
		return 0, &platform.Error{Op: op, Class: platform.ClassNotFound,
			Err: fmt.Errorf("every installation of the GitHub App that touchmark read (%d) is suspended: %w", suspended, platform.ErrNotFound)}
	}
	return 0, &platform.Error{Op: op, Class: platform.ClassNotFound,
		Err: fmt.Errorf("the GitHub App has no installation: %w", platform.ErrNotFound)}
}

// installations lists every installation of the App (the sweep's
// scope); complete is false when the listing was capped or a later
// page failed.
func (a *app) installations(ctx context.Context, op string) ([]apiInstallation, bool, error) {
	auth, err := a.jwtAuth()
	if err != nil {
		return nil, false, err
	}
	var out []apiInstallation
	complete, err := listAll(ctx, a.c, op, a.c.endpoint("app", "installations"), nil, auth, maxInstallationPages,
		func(i apiInstallation) error {
			if i.ID > 0 {
				out = append(out, i)
			}
			return nil
		})
	if err != nil {
		if !laterPage(err) || fatal(err) {
			return nil, false, err
		}
		complete = false
	}
	return out, complete, nil
}

// mintRequest is the body of POST /app/installations/{id}/access_tokens.
type mintRequest struct {
	RepositoryIDs []int64           `json:"repository_ids,omitempty"`
	Permissions   map[string]string `json:"permissions"`
}

// mintResponse is its answer.
type mintResponse struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		ID int64 `json:"id"`
	} `json:"repositories"`
}

// readToken returns the reading token of installation id, minted when
// there is none or it expires within tokenRenew.
func (a *app) readToken(ctx context.Context, id int64) (string, error) {
	a.mu.Lock()
	it := a.tokens[id]
	if it == nil {
		it = &instToken{}
		a.tokens[id] = it
	}
	a.mu.Unlock()
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.token != "" && a.c.now().Before(it.expires.Add(-tokenRenew)) {
		return it.token, nil
	}
	m, err := a.mint(ctx, "mint an installation token", id, mintRequest{Permissions: readPermissions})
	if err != nil {
		return "", err
	}
	it.token, it.expires = m.Token, m.ExpiresAt
	return it.token, nil
}

// revokeAll revokes every reading token the App holds and forgets them
// (a later request mints again); the errors are joined.
func (a *app) revokeAll(ctx context.Context) error {
	a.mu.Lock()
	var held []*instToken
	for _, id := range sortedInt64Keys(a.tokens) {
		held = append(held, a.tokens[id])
	}
	a.mu.Unlock()
	var errs []error
	for _, it := range held {
		it.mu.Lock()
		tok := it.token
		it.token, it.expires = "", time.Time{}
		it.mu.Unlock()
		if tok != "" {
			errs = append(errs, a.c.revoke(ctx, tok))
		}
	}
	return errors.Join(errs...)
}

// sortedInt64Keys returns the keys of m in order.
func sortedInt64Keys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// mint creates an installation token (POST
// /app/installations/{id}/access_tokens) and registers it as a secret. The
// token's length and form are never assumed (github.com mints stateless
// "ghs_" tokens of about 520 characters since 2026-04-27). A token that
// lacks a write permission asked for, holds a permission not asked for or
// above the level asked for (metadata read aside, which every token has),
// or, when repository_ids were sent, is not narrowed to exactly those
// repositories (repository_selection "selected", and the repositories
// listed, when listed, those asked for) is revoked and refused: GitHub
// gives a token every permission of the installation, or every
// repository, when it ignores the request's (docs).
func (a *app) mint(ctx context.Context, op string, id int64, req mintRequest) (*mintResponse, error) {
	auth, err := a.jwtAuth()
	if err != nil {
		return nil, err
	}
	var m mintResponse
	// A minted token spends the App's limit of tokens, not the write budget.
	_, err = a.c.call(throttle.Uncounted(ctx), op, http.MethodPost, a.c.endpoint("app", "installations", strconv.FormatInt(id, 10), "access_tokens"),
		nil, auth, req, &m)
	if err != nil {
		return nil, mintError(err, req)
	}
	if m.Token == "" || m.ExpiresAt.IsZero() {
		return nil, shapeError(op, "an installation token without token or expires_at")
	}
	a.c.register(m.Token)
	for _, perm := range sortedKeys(req.Permissions) {
		if level := req.Permissions[perm]; level == "write" && m.Permissions[perm] != "write" {
			got := m.Permissions[perm]
			_ = a.c.revoke(ctx, m.Token)
			return nil, &platform.Error{Op: op, Class: platform.ClassPermission, Rule: ruleOfPermission(perm),
				Err: fmt.Errorf("the installation token was granted %s %q, not %q", perm, got, level)}
		}
	}
	for _, perm := range sortedKeys(m.Permissions) {
		got, want := m.Permissions[perm], req.Permissions[perm]
		if perm == "metadata" && got == "read" {
			continue
		}
		if want == "" || permissionRank(got) > permissionRank(want) {
			_ = a.c.revoke(ctx, m.Token)
			return nil, shapeError(op, "the installation token was granted %s %q, which was not asked for (asked %q)", perm, got, want)
		}
	}
	if len(req.RepositoryIDs) > 0 {
		if m.RepositorySelection != "selected" {
			_ = a.c.revoke(ctx, m.Token)
			return nil, shapeError(op, "the installation token covers repositories %q, not the ones asked for", m.RepositorySelection)
		}
		var got []int64
		for _, r := range m.Repositories {
			got = append(got, r.ID)
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(req.RepositoryIDs))
		if len(got) > 0 && !slices.Equal(slices.Compact(got), slices.Compact(want)) {
			_ = a.c.revoke(ctx, m.Token)
			return nil, shapeError(op, "the installation token covers repositories %v, not %v, which were asked for", got, want)
		}
	}
	return &m, nil
}

// permissionRank orders the levels of an installation token's permissions.
func permissionRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	if level == "" {
		return 0
	}
	return 4 // a level the driver does not know counts as the widest
}

// mintError classifies a refused mint. GitHub answers 422 when the
// installation lacks a permission asked for ("The permissions requested
// are not granted to this installation.") and when a repository is not
// part of it ("There is at least one repository that does not exist or is
// not accessible to the parent installation."): the first is
// ClassPermission, Rule the most specific permission asked for
// (workflows, then contents, then pull-requests: the installation's
// permissions were checked before, so the one that fails is the one a
// change in the meantime took away); the second ClassNotFound.
func mintError(err error, req mintRequest) error {
	var pe *platform.Error
	if !errors.As(err, &pe) || pe.Status != http.StatusUnprocessableEntity {
		return err
	}
	lower := messageOf(pe.Err)
	switch {
	case strings.Contains(lower, "not accessible to the parent installation"), strings.Contains(lower, "does not exist or is not accessible"):
		return &platform.Error{Op: pe.Op, Class: platform.ClassNotFound, Status: pe.Status, Err: fmt.Errorf("%w: %w", pe.Err, platform.ErrNotFound)}
	case strings.Contains(lower, "permission"):
		rule := "contents"
		switch {
		case req.Permissions["workflows"] == "write":
			rule = "workflows"
		case req.Permissions["contents"] == "write":
			rule = "contents"
		case req.Permissions["pull_requests"] == "write":
			rule = "pull-requests"
		}
		return &platform.Error{Op: pe.Op, Class: platform.ClassPermission, Status: pe.Status, Rule: rule, Err: pe.Err}
	}
	return err
}

// ruleOfPermission names a permission of an installation token the way
// Rule does.
func ruleOfPermission(perm string) string {
	if perm == "pull_requests" {
		return "pull-requests"
	}
	return perm
}

// revoke revokes an installation token (DELETE /installation/token with
// the token itself). A token GitHub no longer takes (401) is revoked
// already.
func (c *client) revoke(ctx context.Context, token string) error {
	_, err := c.call(throttle.Uncounted(ctx), "revoke the installation token", http.MethodDelete, c.endpoint("installation", "token"), nil,
		c.staticAuth("Bearer "+token), nil, nil)
	if platform.ClassOf(err) == platform.ClassAuth {
		return nil
	}
	return err
}
