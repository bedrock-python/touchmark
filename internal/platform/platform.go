// Package platform defines what touchmark needs from a code hosting platform
// (GitHub, GitLab, Gitea, Forgejo, Bitbucket Cloud, Azure DevOps, Bitbucket
// Data Center): the
// Reader and Writer interfaces drivers implement, their capabilities, and one
// error model.
//
// Drivers never retry or pace requests (internal/throttle does), never know
// about packs, markers or memory, and mask secrets in their errors. Git
// transport (snapshots, pushes) is not here: internal/gitx does it for every
// platform through Remote.
package platform

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// AccountKind tells people from automation.
type AccountKind uint8

const (
	KindUnknown AccountKind = iota
	KindUser
	KindBot
	KindServiceAccount
)

// Account is a user, bot or service account. ID is the platform's stable id
// (it survives renames and token rotation); Login is for people and logs.
type Account struct {
	ID    string
	Login string
	Email string
	Kind  AccountKind
}

// Repo is one repository as the platform reports it.
type Repo struct {
	// Host is the provider host ("github.com", "gitlab.example.com").
	Host string
	// ID is immutable: it survives renames and transfers. (Host, ID) is the
	// deduplication key of a run.
	ID string
	// Path is the canonical path: "owner/name" or "group/sub/project".
	Path          string
	DefaultBranch string
	WebURL        string
	// Visibility is "public", "internal" or "private".
	Visibility string
	// ObjectFormat is "sha1" or "sha256".
	ObjectFormat string
	Archived     bool
	Disabled     bool
	Empty        bool
	Mirror       bool
	Fork         bool
	// PendingDelete is set for repositories scheduled for deletion.
	PendingDelete bool
	// PRsDisabled is set when pull or merge requests are turned off.
	PRsDisabled bool
	Topics      []string
}

// Selector is one targets.yml entry for one provider.
type Selector struct {
	// Repo names one repository by path; when set, the other fields are
	// ignored and a missing repository is ErrNotFound.
	Repo string
	// Namespace is an organisation (GitHub, Gitea, Forgejo) or a group
	// (GitLab, with Subgroups for nested groups).
	Namespace string
	Subgroups bool
	// Topics must all be present on a repository.
	Topics []string
	// Forks includes forks; a Repo selector never skips a fork.
	Forks bool
}

// Resolved is the result of a Selector.
type Resolved struct {
	Repos []Repo
	// Complete is false when paging stopped early, a cap was hit or part of
	// the listing failed. Stale-PR sweeps never run on incomplete listings.
	Complete bool
	// Incomplete says why Complete is false when the driver knows more than
	// that (for the report): "" otherwise.
	Incomplete string
}

// File is a regular file read through the API.
type File struct {
	Path    string
	Mode    string // "100644" or "100755"
	OID     string
	Content []byte
}

// Remote is how gitx reaches a repository: a URL without credentials and a
// header source called once per git invocation.
type Remote struct {
	URL string
	// Header returns the value of the Authorization header, e.g.
	// "Basic <base64(user:token)>"; nil for anonymous access.
	Header func(context.Context) (string, error)
}

// PRState is the state of a pull or merge request.
type PRState string

const (
	Open   PRState = "open"
	Closed PRState = "closed" // closed without merge
	Merged PRState = "merged"
)

// PR is a pull request (a merge request on GitLab).
type PR struct {
	Number  int64
	URL     string
	State   PRState
	Draft   bool
	Head    string // source branch name
	HeadSHA string
	Base    string // target branch name
	// RepoID is the repository the PR targets; HeadRepoID the one its head
	// branch lives in. They differ for PRs from forks, which are never
	// touchmark's own.
	RepoID     string
	HeadRepoID string
	// BaseExists is false when the target branch was deleted.
	BaseExists bool
	Title      string
	// Body is the raw body, marker included: its last line on every
	// platform. Where the platform keeps the marker outside the description
	// (Caps.Marker MarkerInProperties) the driver appends it, as
	// marker.Attach does, so the core reads the same Body everywhere.
	Body   string
	Labels []string
	Author Account
	// ClosedBy is who closed or merged the PR; nil if open or not reported
	// (see Caps.CloserKnown).
	ClosedBy  *Account
	CreatedAt time.Time
	ClosedAt  time.Time // zero while open
}

// NewPR is a pull request to create. Its Body, and PREdit's, end with the
// marker line; a driver of a MarkerInProperties platform stores that line
// apart (marker.Detach) and writes the rest as the description.
type NewPR struct {
	Head, Base, Title, Body string
	// Labels are created by the driver when missing.
	Labels []string
	// Draft falls back to a ready PR when the platform refuses drafts.
	Draft bool
}

// PREdit changes a pull request in one request. Nil fields are unchanged.
type PREdit struct {
	Title *string
	Body  *string
	State *PRState // Open or Closed
	Base  *string
	// AddLabels are added; labels are never removed.
	AddLabels []string
}

// Perms is what a per-target write token must allow.
type Perms struct {
	Contents  bool
	PRs       bool
	Workflows bool
}

// RepoPR is a pull request found by a sweep, with its repository.
type RepoPR struct {
	Repo Repo
	PR   PR
}

// Swept is the result of OpenPRsBy.
type Swept struct {
	PRs []RepoPR
	// Complete is false when the listing may have missed repositories.
	Complete bool
	// Notes are warnings about what the listing left out on purpose and
	// that does not make it incomplete (GitHub: the repositories of a
	// suspended installation, where the writer can close nothing).
	Notes []string
}

// FindingStatus grades one doctor or preflight check.
type FindingStatus string

const (
	FindingOK      FindingStatus = "ok"
	FindingWarn    FindingStatus = "warn"
	FindingFail    FindingStatus = "fail"
	FindingUnknown FindingStatus = "unknown" // the account cannot read it
)

// Finding is one result of Checker.Check.
type Finding struct {
	Repo   string // repository path, "" for provider-wide findings
	Check  string // e.g. "access", "token-expiry", "workflows", "rules"
	Status FindingStatus
	Detail string
}

// Change is one path of an API commit. Mode "" deletes the path.
type Change struct {
	Path string
	Mode string // "100644", "100755" or "" to delete
	OID  string // hub blob id
}

// CommitRequest asks the platform to create a commit (see Committer).
type CommitRequest struct {
	Branch string
	// Expect is the branch head the commit replaces; "" means the branch
	// must not exist.
	Expect string
	// Parent is the base commit (B).
	Parent string
	// Tree is the tree touchmark computed locally; the driver must produce
	// exactly this tree.
	Tree    string
	Changes []Change
	// Blob returns the content of a hub blob.
	Blob    func(oid string) ([]byte, error)
	Message string
	// Stage is the hidden ref, "refs/touchmark/<fp16>/stage", that the core
	// pushed its own commit of Tree to (through the TargetWriter's Remote)
	// before the call, so that the platform holds every object of Tree and
	// no workflow runs on it; "" when it pushed none. The driver builds
	// the commit from Tree and Parent, not from the staged commit, and
	// deletes Stage when it moves Branch (in the same atomic update where
	// the platform allows it). A Stage outside "refs/touchmark/" is refused.
	Stage string
}

// Commit is the result of an API commit.
type Commit struct {
	SHA      string
	Tree     string
	Verified bool // the platform signed it
	CAS      bool // the branch moved only if it still pointed at Expect
}

// DraftStyle is how a platform marks drafts.
type DraftStyle uint8

const (
	DraftNative      DraftStyle = iota // a flag on the PR
	DraftTitlePrefix                   // a title prefix ("Draft: ", "WIP:")
)

// MarkerStore is where the marker lives.
type MarkerStore uint8

const (
	MarkerInBody MarkerStore = iota // last line of the description
	// MarkerInProperties keeps the marker line out of the description, in
	// a property of the pull request (Azure DevOps: the properties API,
	// key touchmark.marker), for a platform whose descriptions are too
	// short to hold one (4 000 characters there). The core still writes
	// and reads one Body with the marker as its last line, in the comment
	// frame: a driver of such a platform splits what the core writes with
	// marker.Detach (the description, without marker lines; the line, to
	// the property) and joins what it reads with marker.Attach (the
	// description without marker lines, then the property's line), so the
	// description a person edits can never forge or hide the marker. The
	// description alone must fit MaxBody: prbody reserves no room for the
	// marker there. Other platforms see no change: their Body is the
	// description itself.
	MarkerInProperties
	// MarkerInRefDef is the last line of the description too, with the
	// payload of MarkerInBody wrapped in a Markdown link reference
	// definition, `[touchmark]: # "touchmark:v1 …"`, which renderers do not
	// show: Bitbucket Cloud escapes HTML in descriptions, so an HTML comment
	// would show as text (marker.FrameRefDef; the core reads both frames on
	// every platform).
	MarkerInRefDef
)

// Limits are a provider's default pacing (internal/throttle applies them,
// providers[].limits in hub.yml override them). Zero is no limit.
type Limits struct {
	Reads             int // targets inspected at once (their API reads)
	GitReads          int // concurrent git fetches
	ReadsPerMinute    int // the read budget: API reads in any minute
	WritesPerMinute   int
	WritesPerHour     int
	CommentsPerMinute int
	MinInterval       time.Duration // between two writes
}

// Caps describes a platform instance, found once per run by Reader.Probe.
type Caps struct {
	// Flavor is "github", "ghe.com", "ghes", "gitlab", "gitea", "forgejo",
	// "bitbucket" (Bitbucket Cloud), "azure-devops" (Azure DevOps
	// Services) or "bitbucket-datacenter".
	Flavor  string
	Version string
	// MaxBody is the body budget in bytes, marker included, but where the
	// marker lives apart (MarkerInProperties): there it bounds the
	// description alone.
	MaxBody     int
	Draft       DraftStyle
	DraftPrefix string
	// WorkflowPerm is set when CI files need an extra permission (GitHub).
	WorkflowPerm bool
	// QuickActions is set when body lines starting with "/" execute (GitLab).
	QuickActions bool
	// LabelsByID is set when pull requests take labels by id only: names
	// are refused or dropped silently (Gitea and Forgejo answer 422 to
	// names when a pull request is created, and drop unknown ids).
	LabelsByID bool
	// NoLabels is set when the platform has no pull request labels
	// (Bitbucket Cloud): the core asks for none (NewPR.Labels,
	// PREdit.AddLabels and EnsureLabels stay empty, the marker records
	// none set) and reports none, and the driver ignores any it is given.
	NoLabels bool
	// CloserKnown is set when PR.ClosedBy is reliable.
	CloserKnown bool
	// ClosedImmutable is set when a pull request closed without merging can
	// never be edited again, by anyone (Bitbucket Cloud's declined pull
	// requests): the core never plans a write to the body of a closed pull
	// request there. Memory then reads a decline from what the marker held
	// while the pull request was open (its optin), and a forget_declines
	// entry acts while it is present instead of once (see
	// decide.MemoryConfig.ClosedImmutable).
	ClosedImmutable bool
	// ReaderCloses is set when an identity that may only read a repository
	// can still close its pull requests (Bitbucket Data Center, where
	// declining needs read access): a plan then warns unless the read
	// credential's account is among automation_accounts, so that a pull
	// request declined with the read key, which every branch of the hub
	// can use, is not taken for the team's decision.
	ReaderCloses bool
	Marker       MarkerStore
	Commit       struct{ API, SignedByPlatform, CAS bool }
	// RuntimeOnly lists checks the writer cannot read upfront, e.g.
	// "push_rules" on GitLab.
	RuntimeOnly []string
	Limits      Limits
}

// BodyControls reports whether pull request descriptions on the platform
// can carry touchmark's tick boxes ("Rebuild this branch", "Propose this
// content again"). Each is a task list item tagged with an HTML comment,
// which renderers hide; a platform that escapes HTML in descriptions would
// show the tag as text, and it is the same platform whose marker is a
// Markdown reference definition (MarkerInRefDef). There the description
// offers no tick box, says to ask for a rebuild or a new proposal through
// .touchmark/operations.yml instead, and the core reads no tick box from
// it.
func (c Caps) BodyControls() bool { return c.Marker != MarkerInRefDef }

// Reader is one provider from hub.yml under one identity. Implementations
// are safe for concurrent use: the core inspects targets in parallel.
type Reader interface {
	// Probe reports the flavor, version and capabilities; once per run.
	Probe(ctx context.Context) (Caps, error)
	// Self is the account the credential acts as.
	Self(ctx context.Context) (Account, error)
	// Lookup resolves a login (writer, known_authors, automation_accounts)
	// to an account with its stable id.
	Lookup(ctx context.Context, login string) (Account, error)
	// Resolve lists the repositories sel selects, sorted by path ignoring
	// case. Archived, empty and otherwise skippable repositories are
	// included (the core classifies them); forks only with sel.Forks or a
	// Repo selector.
	Resolve(ctx context.Context, sel Selector) (Resolved, error)
	// Repo returns one repository by path, or ErrNotFound. Paths compare
	// case-insensitively; the result carries the canonical path.
	Repo(ctx context.Context, path string) (Repo, error)
	// ReadFile returns a regular file at ref ("" = default branch head), at
	// most max bytes: ErrNotFound, ErrNotRegular (symlink, submodule,
	// directory) or ErrTooLarge. It never follows links.
	ReadFile(ctx context.Context, r Repo, ref, path string, max int64) (File, error)
	// Remote is how gitx fetches from and pushes to r.
	Remote(ctx context.Context, r Repo) (Remote, error)
	// PRs returns, newest first (by number, descending), the PRs by
	// authors from heads in every state, plus open PRs by anyone from
	// heads. Authors match by Account.ID. PRs from forks are included; the
	// core decides what is its own.
	PRs(ctx context.Context, r Repo, heads []string, authors []Account) ([]PR, error)
	// OpenPRsBy lists open PRs by authors from heads in every repository
	// this identity sees (the stale-PR sweep).
	OpenPRsBy(ctx context.Context, authors []Account, heads []string) (Swept, error)
}

// Writer is a Reader with write access.
type Writer interface {
	Reader
	// Target narrows the write identity to one repository and perms. On
	// GitHub it mints a token for that repository only and revokes it on
	// Close. It is the permission preflight before a target's writes: an
	// identity without need on r gets ClassPermission (Rule names what is
	// missing), or ClassNotFound where the platform hides r.
	Target(ctx context.Context, r Repo, need Perms) (TargetWriter, error)
}

// TargetWriter writes to one repository.
type TargetWriter interface {
	Remote() Remote
	// CreatePR opens a PR. If an open PR from the same head exists, it
	// returns that PR with ErrExists.
	CreatePR(ctx context.Context, pr NewPR) (PR, error)
	// EditPR changes the body, state, title, base and labels in one
	// request. It never changes draft state and never removes labels.
	EditPR(ctx context.Context, number int64, e PREdit) (PR, error)
	Comment(ctx context.Context, number int64, body string) error
	// EnsureLabels creates missing labels and returns their ids (names on
	// platforms that label by name).
	EnsureLabels(ctx context.Context, names []string) ([]string, error)
	// Close releases the identity (revokes a per-target token).
	Close() error
}

// Committer creates commits through the API, for platform signatures.
// Optional; GitHub only in v1, where the TargetWriter of
// a GitHub App implements it, with the per-target token.
//
// Commit makes a commit of req.Tree on req.Parent and moves req.Branch to
// it only if the branch still points at req.Expect (CAS: ClassConflict
// otherwise). It never moves the branch to a commit whose tree is not
// req.Tree, nor to one the platform did not sign: that is ErrUnsigned
// (ClassUnsupported, Rule "cannot-sign"), with the unsigned commit's SHA
// in the returned Commit, and the branch where it was.
type Committer interface {
	Commit(ctx context.Context, r Repo, req CommitRequest) (Commit, error)
}

// Checker runs write-identity checks for doctor. Optional.
//
// With repositories, Check reports the checks of each of them (Finding.Repo
// its path): "access" (the identity may push and write pull requests),
// and what else the platform shows the identity ("permissions",
// "workflows", "rules" of branches, the sync branches among branches). With
// none, it reports the checks of the identity itself (Finding.Repo ""):
// "token-expiry", "scopes", "2fa". A check the identity cannot read is
// FindingUnknown, never FindingOK. A rate limit, a refused credential or
// the end of ctx fails the call; other failures of one check are that
// check's FindingUnknown.
type Checker interface {
	Check(ctx context.Context, repos []Repo, branches []string) ([]Finding, error)
}

// KeyChecker tells whether an SSH signing key is the write identity's on
// the platform, which shows commits it signs as verified only then.
// Optional; doctor uses it.
type KeyChecker interface {
	// CheckSigningKey reports, as a Finding of check "signing-key",
	// whether publicKey (the authorized_keys form, "ssh-ed25519 AAAA…") is
	// among the identity's keys and may sign.
	CheckSigningKey(ctx context.Context, publicKey string) (Finding, error)
}

// Preflighter tells, before any write, which platform rules delivery to r
// will meet (signatures, force pushes, deletions, the permission to change
// CI files). Optional: without it the core knows none of these upfront, as
// on GitLab, Gitea and Forgejo, where pushes find them at run time (a
// PushGuard still tells the branches the writer may not push to at all).
type Preflighter interface {
	// Preflight reads the rules on branches of r (the default branch and
	// the sync branch, which may not exist yet).
	Preflight(ctx context.Context, r Repo, branches []string) (Rules, error)
}

// Rules is what Preflighter found for one repository.
type Rules struct {
	// Known is false when the identity cannot read the rules; the other
	// rule fields are then zero.
	Known bool
	// SignedCommits is set when a rule requires signed commits on one of
	// the branches (GitHub: required_signatures).
	SignedCommits bool
	// NoForcePush lists the branches on which a rule forbids force pushes
	// (GitHub: non_fast_forward).
	NoForcePush []string
	// NoDelete lists the branches on which a rule forbids deleting them
	// (GitHub: deletion, which its ruleset form selects together with
	// non_fast_forward by default).
	NoDelete []string
	// WorkflowsKnown and Workflows tell whether the identity may change CI
	// files in r (GitHub: the App installation's Workflows permission).
	// Only a write identity knows; a reader leaves both false.
	WorkflowsKnown, Workflows bool
}

// PushGuard tells, before any write, which branches a protection rule
// keeps the write identity from pushing to at all (GitLab's protected
// branches, Gitea's and Forgejo's branch protection). Optional.
type PushGuard interface {
	// NoPush returns the branches among branches, the default branch
	// aside, that a rule the identity can read keeps it from pushing to,
	// each with the rule. A rule it cannot read never lists a branch:
	// the push finds it. Only a rate limit, a refused credential or the
	// end of ctx fail the call; any other failure leaves branches out.
	NoPush(ctx context.Context, r Repo, branches []string) ([]Protected, error)
}

// HubGuard tells, for a writer that reaches the hub
// (security.writer_on_hub: guard), whether it can get content onto the
// hub's default branch. Optional (GitLab); doctor uses it.
type HubGuard interface {
	// GuardHub returns the findings of check "hub-guard" on hub, one per
	// condition: the writer's role, the protection of the default branch
	// against its pushes and merges, the other protected branches and tags
	// it may push or create, and the hub's settings that make a merge wait
	// for the plan of the default branch's CI configuration. A condition it
	// cannot read is FindingUnknown. Only a rate limit, a refused credential
	// or the end of ctx fail the call.
	GuardHub(ctx context.Context, hub Repo) ([]Finding, error)
}

// MergeRequestAuditor tells who opened a merge request of the hub and who
// pushed to its source branch, for plan's guard under
// security.writer_on_hub guard. Optional (GitLab); the reader answers.
type MergeRequestAuditor interface {
	// MergeRequestPushes reads merge request number of hub and the pushes
	// to its source branch since the branch was created.
	MergeRequestPushes(ctx context.Context, hub Repo, number int64) (MergeRequestPushes, error)
}

// MergeRequestPushes is what MergeRequestAuditor found.
type MergeRequestPushes struct {
	Author Account
	// Branch is the source branch; Pushers the accounts that pushed to it,
	// each once, since it was created.
	Branch  string
	Pushers []Account
	// Complete is false when the pushes could not be read back to the
	// branch's creation; Why then says why.
	Complete bool
	Why      string
}

// Protected is a branch a protection rule keeps the identity from pushing
// to, and the rule (its name or pattern; empty when the platform does not
// show it).
type Protected struct {
	Branch, Rule string
}

// BatchReader reads one file from many repositories per request (GitHub
// GraphQL aliases, GitLab GraphQL). Optional.
type BatchReader interface {
	// ReadFiles reads path at the default branch head of each repository,
	// as ReadFile(ctx, r, "", path, max) would. It returns one File per
	// repository, in the order of repos. When the file of some repositories
	// cannot be returned, the error is a FileErrors with ReadFile's error
	// for each of them (ErrNotFound, ErrNotRegular, ErrTooLarge or another
	// classified error) and their Files are zero. A failure of the whole
	// call (a rate limit, a refused credential, the end of ctx) is an
	// ordinary error with no Files.
	ReadFiles(ctx context.Context, repos []Repo, path string, max int64) ([]File, error)
}

// FileErrors is the error of BatchReader.ReadFiles when some files could
// not be read: one entry per repository of the call, in order, nil where
// the file was read.
type FileErrors []error

// Error lists the failures, "; "-separated.
func (e FileErrors) Error() string {
	var parts []string
	for _, err := range e {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	if len(parts) == 0 {
		return "no file errors"
	}
	return strings.Join(parts, "; ")
}

// Unwrap returns the failures, for errors.Is, errors.As and ClassOf.
func (e FileErrors) Unwrap() []error {
	var out []error
	for _, err := range e {
		if err != nil {
			out = append(out, err)
		}
	}
	return out
}

// Sentinel errors. Drivers wrap them in *Error when they carry a status.
var (
	ErrNotFound   = errors.New("not found")
	ErrNotRegular = errors.New("not a regular file")
	ErrTooLarge   = errors.New("too large")
	// ErrExists comes with the existing PR from CreatePR.
	ErrExists = errors.New("already exists")
	// ErrUnsigned comes from Committer.Commit when the platform did not sign
	// the commit it made (GitHub Enterprise Server without web commit
	// signing): the branch did not move.
	ErrUnsigned = errors.New("the platform did not sign the commit")
	// ErrNotYet comes, in an *Error of ClassTransient with a RetryAfter,
	// from a write the platform refused because it is not ready for it yet,
	// and that applied nothing: GitLab refuses a merge request from a branch
	// whose push its background job has not registered.
	// Drivers never wait for it themselves: the core sends the same write
	// again after RetryAfter, for a bounded time, without reading the
	// platform back first.
	ErrNotYet = errors.New("the platform is not ready for the request yet")
)

// Class groups errors by how the core reacts.
type Class uint8

const (
	ClassUnknown     Class = iota
	ClassTransient         // 5xx, timeout, EOF: retried after a reconciling read
	ClassRateLimited       // pause the provider queue
	ClassAuth              // bad or expired credential
	ClassPermission        // the identity may not do this (Rule names what)
	ClassNotFound
	ClassConflict    // lease, CAS or duplicate: decide again
	ClassPolicy      // a platform rule refused it (Rule names it)
	ClassInvalid     // the request was wrong
	ClassUnsupported // the platform cannot do this
)

// classNames are the report names of the classes, indexed by Class.
var classNames = [...]string{
	ClassUnknown:     "unknown",
	ClassTransient:   "transient",
	ClassRateLimited: "rate-limited",
	ClassAuth:        "auth",
	ClassPermission:  "permission",
	ClassNotFound:    "not-found",
	ClassConflict:    "conflict",
	ClassPolicy:      "policy",
	ClassInvalid:     "invalid",
	ClassUnsupported: "unsupported",
}

// String returns the class name used in reports ("rate-limited", …). A value
// outside the defined classes is "unknown", as ClassUnknown.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return classNames[ClassUnknown]
}

// Error is a classified platform error. Its message must never contain a
// secret: drivers mask before wrapping.
type Error struct {
	Op         string // what was attempted, e.g. "list pull requests"
	Class      Class
	Status     int           // HTTP status, 0 if none
	RetryAfter time.Duration // from Retry-After or reset headers
	Rule       string        // policy or permission detail, e.g. "workflows", "GH013"
	Err        error
}

// Error formats e as "<op>: <class> (HTTP <status>): rule <rule>: <err>",
// leaving out the parts that are empty. Only the fields shown are printed:
// no headers, no RetryAfter.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(e.Class.String())
	if e.Status != 0 {
		b.WriteString(" (HTTP ")
		b.WriteString(strconv.Itoa(e.Status))
		b.WriteByte(')')
	}
	if e.Rule != "" {
		b.WriteString(": rule ")
		b.WriteString(e.Rule)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ClassOf returns the class of err: the Class of the first *Error in its
// tree, depth first as errors.As walks it (both Unwrap() error and
// Unwrap() []error), that is not ClassUnknown (an *Error of ClassUnknown
// defers to what it wraps and to what follows it); ClassNotFound for
// ErrNotFound; ClassConflict for ErrExists; ClassTransient for
// context.DeadlineExceeded; ClassUnknown otherwise (including nil and
// context.Canceled). So a classified *Error decides over a sentinel it
// wraps, and a joined error takes the class of its first classified part,
// wherever it sits.
func ClassOf(err error) Class {
	if c := errorClass(err); c != ClassUnknown {
		return c
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return ClassNotFound
	case errors.Is(err, ErrExists):
		return ClassConflict
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTransient
	}
	return ClassUnknown
}

// maxErrorDepth bounds the walk of errorClass: an error tree deeper than
// this is a bug, not something to follow.
const maxErrorDepth = 100

// errorClass returns the Class of the first *Error in err's tree, in
// depth-first pre-order, that is not ClassUnknown.
func errorClass(err error) Class {
	return classIn(err, 0)
}

func classIn(err error, depth int) Class {
	if err == nil || depth > maxErrorDepth {
		return ClassUnknown
	}
	switch e := err.(type) { //nolint:errorlint // it walks the error tree itself, as errors.As does, to reach every sibling
	case *Error:
		switch {
		case e == nil:
			return ClassUnknown
		case e.Class != ClassUnknown:
			return e.Class
		}
		return classIn(e.Err, depth+1)
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if c := classIn(inner, depth+1); c != ClassUnknown {
				return c
			}
		}
	case interface{ Unwrap() error }:
		return classIn(e.Unwrap(), depth+1)
	}
	return ClassUnknown
}
