package ghfake

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// idKind is how a request authenticated.
type idKind uint8

const (
	idJWT   idKind = iota + 1 // a GitHub App's JWT
	idToken                   // an installation token or a personal access token
)

// identity is who sent a request; nil is anonymous.
type identity struct {
	kind idKind
	app  *app     // idJWT
	tok  *token   // idToken
	who  *account // the bot of an installation token, the user of a PAT
}

// key is the Request.Identity of id.
func (id *identity) key() string {
	switch {
	case id.kind == idJWT:
		return "app/" + id.app.slug
	case id.tok.kind == tokenInstallation:
		return "installation/" + strconv.FormatInt(id.tok.inst.id, 10)
	}
	return "user/" + id.who.login
}

// installation reports whether id is an installation token.
func (id *identity) installation() bool {
	return id != nil && id.kind == idToken && id.tok.kind == tokenInstallation
}

// jwtMaxAhead is how far in the future a JWT's exp may be.
const jwtMaxAhead = 10 * time.Minute

// tokenLifetime is how long an installation token lives.
const tokenLifetime = time.Hour

// JWT errors (public reports; the documentation does not quote them).
const (
	jwtUndecodable = "A JSON web token could not be decoded"
	jwtExpTooFar   = "'Expiration time' claim ('exp') is too far in the future"
	jwtExpired     = "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires"
	jwtIatFuture   = "'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued"
	badCredentials = "Bad credentials"
)

// credential reads the Authorization header: "Bearer <x>" or "token <x>".
func credential(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, value, ok := strings.Cut(h, " ")
	if !ok || value == "" {
		return "", true
	}
	switch strings.ToLower(scheme) {
	case "bearer", "token":
		return strings.TrimSpace(value), true
	}
	return "", true
}

// authenticate finds who sent r. Without credentials it returns a nil
// identity and true. A bad credential is a 401; a stale (expired or
// revoked) token is also a 401 and, for a write, a violation.
func (s *Server) authenticate(r *http.Request) (*identity, response, bool) {
	value, present := credential(r)
	if !present {
		return nil, response{}, true
	}
	if value == "" {
		return nil, apiError(http.StatusUnauthorized, badCredentials), false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if looksLikeJWT(value) {
		a, msg := s.verifyJWT(value)
		if a == nil {
			return nil, apiError(http.StatusUnauthorized, msg), false
		}
		return &identity{kind: idJWT, app: a, who: a.bot}, response{}, true
	}
	t := s.lookupToken(value)
	if t == nil {
		return nil, apiError(http.StatusUnauthorized, badCredentials), false
	}
	if why := s.stale(t); why != "" {
		if restPoints(r.Method) > 1 {
			s.violate("stale-token %s: %s %s: the token is %s", tokenOwner(t), r.Method, r.URL.Path, why)
		}
		return nil, apiError(http.StatusUnauthorized, badCredentials), false
	}
	return &identity{kind: idToken, tok: t, who: t.user}, response{}, true
}

// lookupToken finds a token by value in constant time per candidate.
// Called with mu held.
func (s *Server) lookupToken(value string) *token {
	if t := s.tokens[value]; t != nil && subtle.ConstantTimeCompare([]byte(t.value), []byte(value)) == 1 {
		return t
	}
	return nil
}

// stale returns why t no longer works ("expired", "revoked"), "" when it
// does. Called with mu held.
func (s *Server) stale(t *token) string {
	switch {
	case t.revoked:
		return "revoked"
	case !t.expires.IsZero() && !s.now().Before(t.expires):
		return "expired"
	case t.inst != nil && t.inst.suspended:
		return "suspended"
	}
	return ""
}

// tokenOwner names a token's owner for violations, never its value.
func tokenOwner(t *token) string {
	if t.kind == tokenInstallation {
		return "installation/" + strconv.FormatInt(t.inst.id, 10)
	}
	return "user/" + t.user.login
}

// looksLikeJWT reports whether a credential is a JWT rather than a token:
// three base64url parts and no token prefix.
func looksLikeJWT(v string) bool {
	if strings.HasPrefix(v, "gh") {
		return false
	}
	return strings.Count(v, ".") == 2
}

// verifyJWT checks an App JWT (RS256; iat not in the future; exp in the
// future and at most 10 minutes ahead; iss the App's client id or id) and
// returns its App, or the error message. Called with mu held.
func (s *Server) verifyJWT(v string) (*app, string) {
	parts := strings.Split(v, ".")
	var head struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iat json.Number `json:"iat"`
		Exp json.Number `json:"exp"`
		Iss any         `json:"iss"`
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(hb, &head) != nil || head.Alg != "RS256" {
		return nil, jwtUndecodable
	}
	d := json.NewDecoder(strings.NewReader(string(cb)))
	d.UseNumber()
	if d.Decode(&claims) != nil {
		return nil, jwtUndecodable
	}
	a := s.appByIssuer(claims.Iss)
	if a == nil || a.key == nil {
		return nil, jwtUndecodable
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(a.key, crypto.SHA256, sum[:], sig) != nil {
		return nil, jwtUndecodable
	}
	iat, err := claims.Iat.Int64()
	if err != nil {
		return nil, jwtIatFuture
	}
	exp, err := claims.Exp.Int64()
	if err != nil {
		return nil, jwtExpired
	}
	now := s.now().Unix()
	switch {
	case iat > now:
		return nil, jwtIatFuture
	case exp <= now:
		return nil, jwtExpired
	case exp > now+int64(jwtMaxAhead/time.Second):
		return nil, jwtExpTooFar
	}
	return a, ""
}

// appByIssuer finds the App an iss claim names: its client id, or its id
// as a number or a string. Called with mu held.
func (s *Server) appByIssuer(iss any) *app {
	var id int64
	switch v := iss.(type) {
	case string:
		for _, a := range s.apps {
			if a.clientID == v {
				return a
			}
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil
		}
		id = n
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return nil
		}
		id = n
	default:
		return nil
	}
	return s.apps[id]
}

// newTokenValue returns a fresh token with prefix. Installation tokens on
// DotCom take the stateless format (about 520 characters with two dots)
// unless Options.ShortTokens.
func (s *Server) newTokenValue(prefix string, long bool) string {
	if !long {
		return prefix + randomAlnum(36)
	}
	part := func(n int) string {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return prefix + part(27) + "." + part(270) + "." + part(86)
}

// randomAlnum returns n random letters and digits.
func randomAlnum(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			out[i] = 'x'
			continue
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out)
}

// mintToken creates an installation token for in, narrowed to repos (nil:
// every repository the installation reaches) and perms (nil: all of the
// installation's). The caller has validated both. Called with mu held.
func (s *Server) mintToken(in *installation, repos map[int64]bool, perms Permissions) *token {
	if perms == nil {
		perms = in.perms.clone()
	}
	if !perms.has("metadata", Read) {
		perms["metadata"] = Read
	}
	now := s.now()
	t := &token{
		value:   s.newTokenValue("ghs_", s.flavor == DotCom && !s.opts.ShortTokens),
		kind:    tokenInstallation,
		inst:    in,
		user:    in.app.bot,
		perms:   perms,
		repos:   repos,
		issued:  now,
		expires: now.Add(tokenLifetime),
	}
	s.tokens[t.value] = t
	return t
}

// GenerateAppKey returns a new 2048-bit RSA key for an App and its PEM
// (PKCS#1 "RSA PRIVATE KEY", the form GitHub hands out).
func GenerateAppKey() (*rsa.PrivateKey, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("ghfake: generate an App key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, pemBytes, nil
}

// SignJWT returns an App JWT signed with key: RS256 with the claims iat,
// exp and iss (a string; pass the client id or the decimal App id).
func SignJWT(key *rsa.PrivateKey, iss string, iat, exp time.Time) (string, error) {
	if key == nil {
		return "", errors.New("ghfake: SignJWT: no key")
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": iat.Unix(), "exp": exp.Unix(), "iss": iss})
	if err != nil {
		return "", fmt.Errorf("ghfake: SignJWT: %w", err)
	}
	signing := head + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("ghfake: SignJWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// repoLevel is the level of a user on r through ownership and
// collaboration. Called with mu held.
func repoLevel(u *account, r *repo) string {
	switch {
	case u == nil:
		return ""
	case r.owner == u:
		return "admin"
	}
	return r.collaborators[u.id]
}

// perm returns the level id holds on r for a permission name ("contents",
// "metadata", "pull_requests", "issues", "workflows"), and whether r is in
// the token's scope. Called with mu held.
//
// Anyone reads public repositories (metadata, contents, pull requests,
// issues). An installation token holds its permissions on the
// repositories of its installation it was minted for; a classic personal
// access token its user's level with the "repo" scope ("public_repo" for
// public repositories; workflows need "workflow"); a fine-grained one its
// permissions on its repositories, capped by its user's level. A JWT
// reaches no repository.
func (s *Server) perm(id *identity, r *repo, name string) (level string, inScope bool) {
	publicRead := ""
	if !r.private() && name != "workflows" && name != "administration" {
		publicRead = Read
	}
	if id == nil || id.kind == idJWT {
		return publicRead, false
	}
	t := id.tok
	switch t.kind {
	case tokenInstallation:
		if !t.inst.covers(r) || (t.repos != nil && !t.repos[r.id]) {
			return publicRead, false
		}
		level := t.perms[name]
		if name == "metadata" && level == "" {
			level = Read
		}
		return maxLevel(level, publicRead), true
	case tokenClassicPAT:
		user := repoLevel(t.user, r)
		if user == "" {
			return publicRead, false
		}
		scoped := hasScope(t.scopes, "repo") || (!r.private() && hasScope(t.scopes, "public_repo"))
		switch {
		case !scoped:
			return publicRead, true
		case name == "workflows":
			if hasScope(t.scopes, "workflow") && levelRank(user) >= levelRank(Write) {
				return Write, true
			}
			return "", true
		}
		return capLevel(user), true
	default: // fine-grained
		user := repoLevel(t.user, r)
		if user == "" || (t.repos != nil && !t.repos[r.id]) {
			return publicRead, false
		}
		level := t.perms[name]
		if name == "metadata" && level == "" {
			level = Read
		}
		if levelRank(level) > levelRank(user) {
			level = capLevel(user)
		}
		return maxLevel(level, publicRead), true
	}
}

// capLevel maps a repository role to a permission level.
func capLevel(role string) string {
	if levelRank(role) >= levelRank(Write) {
		return Write
	}
	return role
}

// maxLevel returns the higher of two levels.
func maxLevel(a, b string) string {
	if levelRank(a) >= levelRank(b) {
		return a
	}
	return b
}

// hasScope reports whether a classic token has a scope.
func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// forbiddenMessage is the 403 message for id lacking a permission.
func forbiddenMessage(id *identity) string {
	if id != nil && id.kind == idToken && id.tok.kind != tokenInstallation {
		return "Resource not accessible by personal access token"
	}
	return "Resource not accessible by integration"
}

// canSee reports whether id sees r at all. Called with mu held.
func (s *Server) canSee(id *identity, r *repo) bool {
	if r.deleted {
		return false
	}
	level, _ := s.perm(id, r, "metadata")
	return level != ""
}

// noteScope records a violation when an installation token narrowed to
// some repositories is used on another one. Called with mu held.
func (s *Server) noteScope(id *identity, r *repo) {
	if id.installation() && id.tok.repos != nil && !id.tok.repos[r.id] {
		s.violate("token-scope %s: used on %s outside the repositories it was minted for", tokenOwner(id.tok), r.path())
	}
}

// need checks that id holds name at level on r and returns the refusal
// otherwise: 404 when id does not see r, 403 with
// X-Accepted-GitHub-Permissions when it lacks the permission. Called with
// mu held.
func (s *Server) need(id *identity, r *repo, name, level string) *response {
	have, _ := s.perm(id, r, name)
	if !s.canSee(id, r) {
		resp := notFound()
		return &resp
	}
	if levelRank(have) >= levelRank(level) {
		return nil
	}
	resp := apiError(http.StatusForbidden, forbiddenMessage(id))
	resp.header = http.Header{"X-Accepted-Github-Permissions": {name + "=" + level}}
	return &resp
}
