package gitx

// Delivery-side git: a private, isolated bare repository per target, blobless
// fetches, commits built without a work tree, and pushes with a lease.
//
// The implementation is split by concern: target.go (isolation, InitTarget,
// running commands), fetch.go (RemoteRefs, FetchBranch, DeepenSince,
// FetchBlobs, ReadBlob), log.go (FirstParentLog, Commit, DiffTree,
// IsCleanMerge, IsAncestor, Tree), attrs.go (Attrs, Renormalized),
// build.go (BuildCommit), push.go (Push, ParsePush) and failure.go
// (ClassifyFailure, PushResult.Transient).
//
// A target's steps, in the order the pipeline of internal/distribute runs
// them:
//
//	t, _ := InitTarget(ctx, dir, remote, auth, iso) // or snapshot.GitSource.Repo
//	b, _, _ := t.FetchBranch(ctx, base, 1)          // snapshot: B, trees only
//	t.Tree(ctx, b)                                   // entries of B
//	t.FetchBlobs(ctx, gitattributesOf(B))            // before Attrs/Renormalized
//	h, ok, _ := t.FetchBranch(ctx, sync, MaxHistory+1)
//	log, shallow, _ := t.FirstParentLog(ctx, h, MaxHistory)
//	t.DiffTree(ctx, hcParent, hc)                    // C
//	t.IsCleanMerge(ctx, m); t.DeepenSince(ctx, base, p2Date-24h); t.IsAncestor(ctx, p2, b)
//	t.IsAncestor(ctx, hcParent, b)                   // unless hcParent == b (decide.HistoryCommit.ParentBaseAncestor)
//	built, _ := t.BuildCommit(ctx, spec)            // then Attrs/Renormalized on built.Tree
//	t.WithAuth(write).Push(ctx, PushSpec{…})
//
// The proofs of ancestry (a merge's second parent, Hc's first parent) run
// after DeepenSince back to a day before the older of the commits to prove,
// and anything but a yes counts as no proof: decide.ClassifyBranch calls
// the branch edited then.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// DeliveryMinVersion is the oldest git distribute supports: --no-lazy-fetch
// (GIT_NO_LAZY_FETCH) arrived in 2.45, check-attr --source in 2.40, git
// --attr-source in 2.41. InitTarget does not check it: phase A of a run
// does, once. With an older git, a command that needs a
// missing object of the blobless repository fetches it instead of failing.
var DeliveryMinVersion = [3]int{2, 45, 0}

// ErrIntegrity is wrapped by BuildCommit when the commit it wrote does not
// change exactly the requested paths or a blob does not hash to its id;
// the target's outcome is failed:integrity.
var ErrIntegrity = errors.New("integrity check failed")

// Auth is a remote's credential: a header source bound to the remote URL's
// scheme://host[:port]/. It is passed to git only through
// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n as
// http.<base>/.extraHeader, never in a URL, argv or config file.
//
// Header is called once per network command (ls-remote, fetch, push), so a
// credential that refreshes itself stays current; it is never called for
// local commands, nor for a remote that is not http(s). A nil Header is
// anonymous access.
type Auth struct {
	// Header returns the Authorization header value, e.g. "Basic …".
	Header func(context.Context) (string, error)
}

// Isolation configures an isolated repository.
type Isolation struct {
	// Home is an empty directory used as HOME (and XDG_CONFIG_HOME) so no
	// user file is read. Required. InitTarget keeps two entries in it, an
	// empty global config file ("gitconfig") and an empty hooks directory
	// ("hooks"), and refuses a Home that holds anything else. Several
	// target repositories may share one Home.
	Home string
	// CAFile is a PEM bundle git trusts for https (providers[].ca_file).
	CAFile string
	// AllowHTTP permits http:// remotes (tests, and loopback only);
	// AllowFile permits file:// and local paths (tests only).
	AllowHTTP bool
	AllowFile bool
}

// TargetRepo is a private bare repository for one target. Every command
// runs with: GIT_CONFIG_NOSYSTEM=1, GIT_CONFIG_GLOBAL=<empty file in
// Home>, HOME=Isolation.Home, GIT_TERMINAL_PROMPT=0, GIT_NO_LAZY_FETCH=1,
// of the process environment only PATH, the system and temporary
// directories, proxies and TLS trust (no credential, no GIT_* variable:
// see inheritedEnv), and -c core.hooksPath=<empty dir>,
// -c core.fsmonitor=false, -c credential.helper=, -c protocol.allow=never,
// -c protocol.https.allow=always (plus http/file when allowed),
// -c http.followRedirects=false, -c transfer.fsckObjects=true,
// -c submodule.recurse=false, -c gc.auto=0, -c maintenance.auto=false.
//
// Besides: XDG_CONFIG_HOME=Home, GIT_DIR=Dir (git never looks for another
// repository), GIT_ATTR_NOSYSTEM=1 (attributes come from the trees only),
// GIT_ASKPASS= and -c core.askPass= (no askpass program runs, SSH_ASKPASS
// included), -c http.sslCAInfo=<CAFile> when Isolation.CAFile is set, and
// the pinned -c core.ignorecase=true, -c core.precomposeunicode=false and
// -c core.autocrlf=false, whatever `git init` found on the runner's file
// system. Git carries the same settings in its environment
// (GIT_CONFIG_COUNT), so a command a caller runs through Git directly is
// isolated too; such a command gets no credential.
//
// Network commands (ls-remote, fetch, push) are bounded by three minutes
// (ErrNetworkTimeout) and get the credential of Auth as an
// extra header; git errors never carry it. Fetches into one repository are
// serialized, and each first removes the lock files a killed fetch may
// have left. The methods are otherwise safe for concurrent use, but
// fetching shortens or deepens the shared history (see FetchBranch), so a
// target's steps are best run in order: the base, then the sync branches,
// then DeepenSince.
type TargetRepo struct {
	Git    *Git
	Dir    string
	Remote string // URL without credentials
	auth   Auth
	// iso is shared by the copies WithAuth makes.
	iso *isolated
}

// isolated is the fixed part of a target repository's isolation.
type isolated struct {
	home, global, hooks string
	// scope is the extraHeader base of an http(s) remote,
	// "scheme://host[:port]/"; "" for a local remote, which gets no header.
	scope string
	// args are the -c options of every command; config the same settings
	// for GIT_CONFIG_COUNT; env the environment without them.
	args   []string
	config [][2]string
	env    []string
	// fetchMu serializes fetches (they update the shallow file and refs).
	fetchMu sync.Mutex
	// trace, when set (tests), sees the argv and environment of every
	// command before it starts.
	trace func(args, env []string)
	// timeout, when positive (tests), replaces networkTimeout.
	timeout time.Duration
	// attrFiles caches attributeFiles by tree id.
	attrMu    sync.Mutex
	attrFiles map[string][]TreeEntry
}

// netTimeout is the bound of one network command.
func (x *isolated) netTimeout() time.Duration {
	if x.timeout > 0 {
		return x.timeout
	}
	return networkTimeout
}

// CommitInfo is one commit of a log.
type CommitInfo struct {
	SHA string
	// Parents are the parents the commit object names, even those beyond a
	// shallow boundary (which git's history walk hides).
	Parents []string
	// Message is the raw message as stored, after the header's blank line
	// (no re-encoding, trailing newline kept).
	Message string
	// Time is the committer date, in the committer's time zone; zero when
	// the committer line is malformed.
	Time time.Time
}

// DiffEntry is one path of `git diff-tree -r -z --no-renames`.
type DiffEntry struct {
	Path    string
	OldMode string // "000000" when added
	NewMode string // "000000" when deleted
	OldOID  string // zero id when added
	NewOID  string // zero id when deleted
}

// Person is a commit author or committer. Both fields are required and may
// hold neither '<', '>' nor control characters; Name has no leading or
// trailing spaces.
type Person struct {
	Name  string
	Email string
}

// Blob is one blob to write.
type Blob struct {
	OID  string
	Open func() (io.ReadCloser, error)
}

// Change is one path of a commit: Mode "" deletes it.
type Change struct {
	Path string
	Mode string // "100644", "100755" or "" to delete
	OID  string
}

// CommitSpec describes a commit built without a work tree.
type CommitSpec struct {
	// Parent is B; the tree starts as Parent's tree.
	Parent  string
	Changes []Change
	// Blobs supplies the content of every OID written; each is stored with
	// hash-object -w --no-filters and must hash to its OID.
	Blobs     map[string]Blob
	Author    Person
	Committer Person
	// When is the author and committer date (max of the hub commit's date
	// and B's date), written in UTC.
	When    time.Time
	Message string
	// Sign, when set, returns the armored signature of the commit payload
	// (for SSHSIG: sshsig.Signer.Sign with namespace "git"); it is added as
	// the gpgsig header.
	Sign func(payload []byte) (string, error)
}

// Built is a commit written to the local object store.
type Built struct {
	Commit string
	Tree   string
}

// PushSpec moves one branch, or one hidden ref of touchmark's.
type PushSpec struct {
	Branch string
	// Ref, when set, is the full name of a hidden ref under
	// HiddenRefPrefix to move instead of refs/heads/<Branch> (the stage ref
	// of an API commit); Branch is then not used.
	Ref string
	// Commit is the new head; "" deletes the branch.
	Commit string
	// Expect is the lease: the branch head the push replaces, "" when the
	// branch must not exist.
	Expect string
}

// HiddenRefPrefix starts every ref touchmark pushes outside refs/heads:
// such a ref is no branch, so it triggers no workflow and no pull request
// follows it.
const HiddenRefPrefix = "refs/touchmark/"

// ref returns the full name of the ref s moves, checked.
func (s PushSpec) ref() (string, error) {
	if s.Ref == "" {
		if err := checkBranch(s.Branch); err != nil {
			return "", err
		}
		return "refs/heads/" + s.Branch, nil
	}
	if !strings.HasPrefix(s.Ref, HiddenRefPrefix) || s.Ref == HiddenRefPrefix {
		return "", fmt.Errorf("invalid ref %q: a hidden ref lies under %s", abbrev(s.Ref), HiddenRefPrefix)
	}
	if err := checkRefName(s.Ref); err != nil {
		return "", err
	}
	return s.Ref, nil
}

// PushStatus classifies a push outcome.
type PushStatus uint8

const (
	PushOK PushStatus = iota + 1
	PushUpToDate
	// PushStale: the lease failed (someone moved the branch).
	PushStale
	// PushPolicy: a server rule refused it (protected branch, pre-receive
	// hook, GitHub GH013 rulesets).
	PushPolicy
	// PushWorkflows: GitHub refused a change under .github/workflows
	// without the Workflows permission.
	PushWorkflows
	// PushUnsigned: the server requires signed commits.
	PushUnsigned
	// PushAuth: the remote refused the credential (HTTP 401 and git's
	// authentication failures, FailureAuth): failed:auth, which counts
	// towards provider-down. A refusal by permission is
	// PushPermission.
	PushAuth
	// PushError: anything else; Transient tells the failures whose outcome
	// is unknown.
	PushError
	// PushPermission: the credential was accepted, but the identity may not
	// push to this repository (HTTP 403, GitHub's "Permission to … denied";
	// FailurePermission): blocked:permission for this target only.
	PushPermission
	// PushRateLimited: the server answered HTTP 429; nothing was applied.
	// The caller defers the provider's queue (deferred:rate-limit), it does
	// not retry at once.
	PushRateLimited
)

// pushStatusNames are the names of the statuses, indexed by PushStatus.
var pushStatusNames = [...]string{
	PushOK:          "ok",
	PushUpToDate:    "up-to-date",
	PushStale:       "stale",
	PushPolicy:      "policy",
	PushWorkflows:   "workflows",
	PushUnsigned:    "unsigned",
	PushAuth:        "auth",
	PushError:       "error",
	PushPermission:  "permission",
	PushRateLimited: "rate-limited",
}

// String returns the status name ("ok", "stale", …), "unknown" for a value
// outside the defined statuses.
func (s PushStatus) String() string {
	if int(s) < len(pushStatusNames) && pushStatusNames[s] != "" {
		return pushStatusNames[s]
	}
	return "unknown"
}

// PushResult is the outcome of one push.
type PushResult struct {
	Status PushStatus
	// Message is the server's reason (remote: lines and the porcelain
	// summary), secrets never included.
	Message string
}
