package ghfake

import (
	"crypto/rsa"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Account types, as REST "type" and GraphQL __typename name them.
const (
	TypeUser         = "User"
	TypeBot          = "Bot"
	TypeOrganization = "Organization"
)

// Plan is the billing plan of an owner. It decides what GitHub offers in
// private repositories: drafts and enforced rulesets need a paid plan
// (see the package documentation).
type Plan string

// Plans.
const (
	PlanFree       Plan = "free"
	PlanTeam       Plan = "team"
	PlanEnterprise Plan = "enterprise"
)

// Permission levels of an App installation or a token.
const (
	Read  = "read"
	Write = "write"
)

// Permissions maps a GitHub App permission name ("contents", "metadata",
// "pull_requests", "workflows", "issues", …) to a level (Read or Write).
type Permissions map[string]string

// clone returns a copy of p.
func (p Permissions) clone() Permissions {
	out := make(Permissions, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// has reports whether p grants name at level or above.
func (p Permissions) has(name, level string) bool {
	return levelRank(p[name]) >= levelRank(level)
}

// levelRank orders permission levels; unknown levels rank as none.
func levelRank(level string) int {
	switch level {
	case Read:
		return 1
	case Write:
		return 2
	case "admin":
		return 3
	}
	return 0
}

// account is a user, bot or organization.
type account struct {
	id    int64
	login string
	typ   string
	name  string
	plan  Plan
	// emails are the addresses GitHub attributes commits by: the noreply
	// address and those AddEmail adds.
	emails []string
	// signingKeys are SSH public key blobs registered as signing keys.
	signingKeys [][]byte
	app         *app // the App of a bot
	human       bool // writes through HTTP are not judged (Human)
	created     time.Time
	// noreplyDomain is the domain of the noreply address:
	// users.noreply.github.com, users.noreply.<host> on GHES.
	noreplyDomain string
}

// noreply returns the account's noreply commit address,
// <id>+<login>@<noreplyDomain>.
func (a *account) noreply() string {
	return strconv.FormatInt(a.id, 10) + "+" + a.login + "@" + a.noreplyDomain
}

// graphLogin is the login GraphQL reports: a bot's login without "[bot]"
// (observed read-only 2026-09-29 on a public bot account: GraphQL
// "<slug>", REST "<slug>[bot]").
func (a *account) graphLogin() string {
	if a.typ == TypeBot {
		return strings.TrimSuffix(a.login, "[bot]")
	}
	return a.login
}

// snapshot returns the exported view of a.
func (a *account) snapshot() Account {
	if a == nil {
		return Account{}
	}
	return Account{ID: a.id, NodeID: nodeID(a.typ, a.id), Login: a.login, Type: a.typ, Email: a.noreply()}
}

// Account is a user, bot or organization as the fake knows it.
type Account struct {
	ID     int64
	NodeID string
	Login  string
	Type   string // TypeUser, TypeBot or TypeOrganization
	// Email is the noreply commit address, <id>+<login>@users.noreply.github.com
	// (users.noreply.<host> on GHES).
	Email string
}

// app is a registered GitHub App.
type app struct {
	id       int64
	clientID string
	slug     string
	name     string
	owner    *account
	key      *rsa.PublicKey
	perms    Permissions // what the App asks for when installed
	bot      *account
}

// installation is an App installed on an owner.
type installation struct {
	id        int64
	app       *app
	account   *account
	perms     Permissions
	selection string // "all" or "selected"
	repos     map[int64]bool
	suspended bool
	created   time.Time
}

// covers reports whether the installation reaches r.
func (in *installation) covers(r *repo) bool {
	if r.owner != in.account {
		return false
	}
	return in.selection == "all" || in.repos[r.id]
}

// tokenKind tells installation tokens from personal access tokens.
type tokenKind uint8

const (
	tokenInstallation tokenKind = iota + 1
	tokenClassicPAT
	tokenFineGrainedPAT
)

// token is an installation access token or a personal access token.
type token struct {
	value string
	kind  tokenKind
	inst  *installation // installation tokens
	user  *account      // the bot of the installation, or the PAT's owner
	perms Permissions
	// repos restricts the token to these repositories; nil means every
	// repository the installation (or the PAT's user) reaches.
	repos   map[int64]bool
	scopes  []string // classic PAT scopes ("repo", "workflow")
	issued  time.Time
	expires time.Time // zero: never (PATs)
	revoked bool
}

// repo is one repository.
type repo struct {
	id            int64
	owner         *account
	name          string
	visibility    string // "public", "private", "internal"
	defaultBranch string
	archived      bool
	disabled      bool
	template      bool
	prsDisabled   bool
	prPolicy      string // "all" or "collaborators_only"
	noDrafts      bool
	objectFormat  string
	mirror        string
	topics        []string
	parent        *repo // the repository this one is a fork of
	dir           string
	prs           []*pr // by number - 1 would waste; kept in creation order
	nextNumber    int64
	labels        []*label
	rulesets      []*ruleset
	collaborators map[int64]string // account id → permission level
	settings      *repoSettings    // environments, secrets, variables (settings.go)
	deleted       bool
	created       time.Time
	pushed        time.Time
}

// path returns "owner/name".
func (r *repo) path() string { return r.owner.login + "/" + r.name }

// private reports whether the repository is not public.
func (r *repo) private() bool { return r.visibility != "public" }

// snapshot returns the exported view of r.
func (r *repo) snapshot() Repo {
	out := Repo{ID: r.id, NodeID: nodeID("Repository", r.id), Owner: r.owner.login, Name: r.name,
		FullName: r.path(), DefaultBranch: r.defaultBranch, Visibility: r.visibility, Archived: r.archived,
		Fork: r.parent != nil, ObjectFormat: r.objectFormat}
	if r.parent != nil {
		out.ParentID = r.parent.id
	}
	return out
}

// Repo is a repository as the fake knows it.
type Repo struct {
	ID            int64
	NodeID        string
	Owner, Name   string
	FullName      string // "owner/name"
	DefaultBranch string
	Visibility    string
	Archived      bool
	Fork          bool
	ParentID      int64
	ObjectFormat  string
}

// label is a repository label.
type label struct {
	id          int64
	name        string
	color       string
	description string
}

// comment is an issue comment on a pull request.
type comment struct {
	id      int64
	author  *account
	body    string
	created time.Time
}

// Event types of a pull request timeline (GraphQL __typename).
const (
	EventClosed         = "ClosedEvent"
	EventReopened       = "ReopenedEvent"
	EventMerged         = "MergedEvent"
	EventHeadRefDeleted = "HeadRefDeletedEvent"
	EventHeadRefForced  = "HeadRefForcePushedEvent"
	EventBaseRefChanged = "BaseRefChangedEvent"
)

// event is one timeline event of a pull request.
type event struct {
	id          int64
	typ         string
	actor       *account // nil when GitHub names nobody
	at          time.Time
	stateReason string // ClosedEvent: "COMPLETED" or "NOT_PLANNED"
	// from and to are the previous and current base of a
	// BaseRefChangedEvent.
	from, to string
}

// pr is a pull request.
type pr struct {
	id       int64
	number   int64
	repo     *repo
	headRepo *repo // nil once the head repository is deleted
	// headOwner is the login in the head label ("owner:branch").
	headOwner string
	headRef   string
	headSHA   string
	baseRef   string
	baseSHA   string
	title     string
	body      string
	draft     bool
	open      bool
	merged    bool
	mergedBy  *account
	mergeSHA  string
	author    *account
	labels    []*label
	created   time.Time
	updated   time.Time
	closedAt  time.Time
	mergedAt  time.Time
	events    []*event
	comments  []*comment
}

// label returns the head label "owner:branch".
func (p *pr) label() string { return p.headOwner + ":" + p.headRef }

// closer returns who closed or merged p: the actor of its last close.
func (p *pr) closer() *account {
	for _, e := range slices.Backward(p.events) {
		if e.typ == EventClosed {
			return e.actor
		}
	}
	return nil
}

// nodeID returns a legacy global node id: base64 of "0<len>:<Type><id>"
// ("MDEwOlJlcG9zaXRvcnkx" is "010:Repository1", "MDM6Qm90Mg==" is
// "03:Bot2"), the form GitHub still accepts and returns for older objects
// (observed read-only 2026-09-29 on a public repository and a public bot).
func nodeID(typ string, id int64) string {
	return base64.StdEncoding.EncodeToString([]byte("0" + strconv.Itoa(len(typ)) + ":" + typ + strconv.FormatInt(id, 10)))
}

// parseNodeID decodes a legacy node id into its type and database id.
func parseNodeID(s string) (typ string, id int64, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", 0, false
	}
	head, rest, found := strings.Cut(string(raw), ":")
	if !found || len(head) < 2 || head[0] != '0' {
		return "", 0, false
	}
	n, err := strconv.Atoi(head[1:])
	if err != nil || n <= 0 || n > len(rest) {
		return "", 0, false
	}
	id, err = strconv.ParseInt(rest[n:], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	return rest[:n], id, true
}

// fmtTime formats t as GitHub does ("2026-09-29T08:35:43Z"), "" for zero.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// timeOrNil returns fmtTime(t), or nil for a zero time (a JSON null).
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fmtTime(t)
}
