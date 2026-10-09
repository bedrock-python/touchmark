package gitx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/throttle"
)

// pushStderrLimit is how much of git push's stderr Push keeps: the remote's
// messages come before git's own summary.
const pushStderrLimit = 64 << 10

// Push runs `git push --porcelain --atomic --no-verify
// --force-with-lease=refs/heads/<b>:<Expect> origin <Commit>:refs/heads/<b>`
// (":refs/heads/<b>" to delete; spec.Ref in place of refs/heads/<b> for a
// hidden ref) and parses the porcelain output. A refused
// push is not an error: it is a PushResult with its status; err is for
// failures to run git at all.
//
// The push also keeps pack-objects from deltifying: a thin pack deltifies
// new blobs against the base's blobs at the same paths (its preferred
// bases), which a blobless repository does not have, and with lazy
// fetching off pack-objects dies ("could not fetch … from promisor
// remote"). --no-thin is not enough: git-remote-http, the transport of
// every https remote, runs send-pack --thin whatever push says. So the
// push also sets pack.window=0 (in GIT_CONFIG_COUNT, which the send-pack
// and pack-objects it starts inherit): with no delta window, pack-objects
// never reads a preferred base. The commit's new blobs are all local
// (BuildCommit wrote them), so a pack without deltas is small.
//
// Commit and Expect are full object ids or ""; a deletion needs a lease on
// the current head (Expect). Git checks "up to date" before the lease: a
// branch already at Commit gives PushUpToDate whatever Expect is, which is
// what a run that lost the answer of its own push needs. A failure to reach
// the remote is PushError (PushAuth, PushPermission or PushRateLimited when
// the server said why), and so is a push that ran into its own bound of
// three minutes: that PushError is Transient, its outcome unknown until the
// caller reads the branch again. A cancelled or expired ctx (the caller's
// own deadline), a credential that cannot be read, or the refusal of the
// internal/throttle Meter ctx carries (asked before git runs) is err.
func (t *TargetRepo) Push(ctx context.Context, spec PushSpec) (PushResult, error) {
	ref, err := spec.ref()
	if err != nil {
		return PushResult{}, fmt.Errorf("git push: %w", err)
	}
	for _, id := range []string{spec.Commit, spec.Expect} {
		if id != "" && !isOID(id) {
			return PushResult{}, fmt.Errorf("git push: %q is not a full object id", abbrev(id))
		}
	}
	if spec.Commit == "" && spec.Expect == "" {
		return PushResult{}, errors.New("git push: a deletion needs the head it replaces (Expect)")
	}
	// A push is a write of the provider's budget: the throttle ctx carries
	// paces it, and a push it refuses is not made.
	if err := throttle.Write(ctx, throttle.Push); err != nil {
		return PushResult{}, fmt.Errorf("git push: %w", err)
	}
	opts := cmdOpts{network: true, stderrLimit: pushStderrLimit, config: [][2]string{{"pack.window", "0"}}}
	stdout, stderr, err := t.exec(ctx, opts,
		"push", "--porcelain", "--atomic", "--no-verify", "--no-thin",
		"--force-with-lease="+ref+":"+spec.Expect,
		remoteName, spec.Commit+":"+ref)
	if errors.Is(err, ErrNetworkTimeout) {
		return PushResult{Status: PushError, Message: cleanMessage(fmt.Sprintf(
			"git push timed out after %v: whether %s moved is unknown", t.iso.netTimeout(), ref))}, nil
	}
	var gitErr *Error
	if err != nil && !errors.As(err, &gitErr) {
		return PushResult{}, err
	}
	return ParsePush(string(stdout), stderr), nil
}

// ParsePush classifies git push output (porcelain stdout and stderr);
// exported for fuzzing and tests.
//
// Every porcelain status line ("<flag> TAB <from>:<to> TAB <summary>
// [(<reason>)]") counts; the worst one decides, in this order: workflows,
// unsigned, policy, stale, error, up to date, ok (' ', '+', '-' and '*'
// lines). A rejected ('!') ref is
//   - PushStale for "[rejected] (stale info)" (the lease failed) and
//     "(remote ref updated since checkout)";
//   - PushWorkflows when the reason or a remote: line says the credential
//     may not create or update a workflow (GitHub: "refusing to allow a
//     GitHub App to create or update workflow `…` without `workflows`
//     permission"; OAuth apps and tokens say "without `workflow` scope");
//   - PushUnsigned when a remote: line or the reason requires signatures:
//     GitHub "Commits must have verified signatures" (rulesets under GH013,
//     classic protection under GH006); GitLab's push rule "Commit must be
//     signed" (ee/lib/ee/gitlab/checks/push_rules/commit_check.rb; before
//     GitLab 16 "Commit must be signed with a GPG key"); Gitea and Forgejo
//     "branch … is protected from unverified commit …"
//     (routers/private/hook_pre_receive.go; Forgejo 15 and 16 send it,
//     Gitea 1.26 and 1.27 fail their own check and send "Internal Server
//     Error (no message for end users)", a PushPolicy: the case "gitea
//     unsigned 1.26 and 1.27" of TestParsePush, from the live e2e fact
//     "signed-commits");
//   - PushPermission for a "[remote rejected]" reason or remote: line with
//     Azure Repos' TF401027 (a missing Git permission);
//   - PushPolicy for other "[remote rejected]" reasons that name a rule or
//     a refusal: "protected branch hook declined", "pre-receive hook
//     declined", "push declined due to repository rule violations",
//     GH006/GH013, Azure Repos' TF402455, "deletion prohibited", "denied",
//     "not allowed", …;
//   - PushError otherwise.
//
// Without status lines, stderr decides, with the rules of ClassifyFailure:
// HTTP 401 and git's authentication failures ("Authentication failed",
// "could not read Username", GitLab's "HTTP Basic: Access denied") are
// PushAuth; HTTP 403 and GitHub's "Permission to … denied" PushPermission;
// HTTP 429 PushRateLimited; anything else PushError.
//
// Message is the status line's summary and reason followed by the remote:
// lines, or the last lines of stderr without status lines; control
// characters (terminal escapes included) are dropped and it is at most 2
// KiB. ParsePush never fails.
func ParsePush(stdout, stderr string) PushResult {
	remote := remoteLines(stderr)
	var worst PushStatus
	var worstLine pushRef
	for line := range strings.SplitSeq(stdout, "\n") {
		r, ok := parsePushRef(strings.TrimSuffix(line, "\r"))
		if !ok {
			continue
		}
		if s := r.status(remote); worst == 0 || pushRank(s) > pushRank(worst) {
			worst, worstLine = s, r
		}
	}
	if worst == 0 {
		status := PushError
		switch classifyStderr(stderr) {
		case FailureAuth:
			status = PushAuth
		case FailurePermission:
			status = PushPermission
		case FailureRateLimited:
			status = PushRateLimited
		}
		msg := lastLines(stderr, 8)
		if msg == "" {
			msg = "git push printed no status"
		}
		return PushResult{Status: status, Message: cleanMessage(msg)}
	}
	var msg strings.Builder
	msg.WriteString(worstLine.summary)
	if worstLine.reason != "" {
		msg.WriteString(" (" + worstLine.reason + ")")
	}
	if worst != PushOK && worst != PushUpToDate {
		for _, l := range remote {
			msg.WriteString("\nremote: " + l)
		}
	}
	return PushResult{Status: worst, Message: cleanMessage(msg.String())}
}

// pushRef is one porcelain status line of git push.
type pushRef struct {
	flag    byte
	refs    string // "<from>:<to>"
	summary string
	reason  string
}

// parsePushRef parses "<flag> TAB <from>:<to> TAB <summary> [(<reason>)]".
func parsePushRef(line string) (pushRef, bool) {
	if len(line) < 3 || line[1] != '\t' || !strings.ContainsRune(" +-*!=", rune(line[0])) {
		return pushRef{}, false
	}
	refs, status, ok := strings.Cut(line[2:], "\t")
	if !ok || !strings.Contains(refs, ":") || status == "" {
		return pushRef{}, false
	}
	r := pushRef{flag: line[0], refs: refs, summary: status}
	if i := strings.Index(status, " ("); i >= 0 && strings.HasSuffix(status, ")") {
		r.summary, r.reason = status[:i], status[i+2:len(status)-1]
	}
	return r, true
}

// status classifies one status line, with the remote: lines of the push.
func (r pushRef) status(remote []string) PushStatus {
	switch r.flag {
	case ' ', '+', '-', '*':
		return PushOK
	case '=':
		return PushUpToDate
	}
	reason := strings.ToLower(r.reason)
	switch r.summary {
	case "[rejected]":
		if reason == "stale info" || reason == "remote ref updated since checkout" {
			return PushStale
		}
		return PushError
	case "[remote rejected]":
		text := reason + "\n" + strings.ToLower(strings.Join(remote, "\n"))
		switch {
		case containsAny(text, workflowMarkers):
			return PushWorkflows
		case containsAny(text, unsignedMarkers):
			return PushUnsigned
		case containsAny(text, permissionMarkers):
			return PushPermission
		case containsAny(text, policyMarkers):
			return PushPolicy
		}
	}
	return PushError
}

// Markers of rejections, lowercase (see ParsePush).
var (
	workflowMarkers = []string{
		"to create or update workflow",
		"without `workflows` permission",
		"without `workflow` scope",
	}
	unsignedMarkers = []string{
		"must have verified signatures",
		"commit must be signed",
		"protected from unverified commit",
	}
	// permissionMarkers: Azure Repos refuses a push the identity lacks a
	// Git permission for with TF401027 ("You need the Git 'GenericContribute'
	// permission to perform this action"; also 'CreateBranch' and
	// 'ForcePush'), in the status line's reason (assumed: to confirm live).
	permissionMarkers = []string{"tf401027"}
	// policyMarkers include Azure Repos' TF402455 ("Pushes to this branch
	// are not permitted; you must use a pull request to update this
	// branch"), which a branch policy answers.
	policyMarkers = []string{
		"protected", "declined", "rule violation", "gh006", "gh013",
		"prohibited", "denied", "deny ", "not allowed", "forbidden", "tf402455",
	}
)

// pushRank orders statuses from the best (ok) to the worst.
func pushRank(s PushStatus) int {
	switch s {
	case PushOK:
		return 0
	case PushUpToDate:
		return 1
	case PushError:
		return 2
	case PushAuth, PushPermission, PushRateLimited:
		return 3
	case PushStale:
		return 4
	case PushPolicy:
		return 5
	case PushUnsigned:
		return 6
	case PushWorkflows:
		return 7
	}
	return 2
}

// remoteLines returns the messages the remote sent ("remote: …" lines),
// trimmed, without empty ones.
func remoteLines(stderr string) []string {
	var out []string
	for line := range strings.SplitSeq(stderr, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "remote:")
		if !ok {
			continue
		}
		if rest = strings.TrimSpace(rest); rest != "" {
			out = append(out, rest)
		}
	}
	return out
}

// lastLines returns the last n non-empty lines of s.
func lastLines(s string, n int) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// maxPushMessage bounds PushResult.Message.
const maxPushMessage = 2 << 10

// cleanMessage drops control characters other than newlines and tabs
// (terminal escapes from a remote included) and invalid UTF-8, and cuts s
// to maxPushMessage bytes on a character boundary.
func cleanMessage(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError || (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxPushMessage {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
