// Package distribute runs delivery: plan (read-only), distribute --dry-run
// and distribute.
//
// Run carries every target through the phases: A guards, B resolve, C
// inspect (classify, opt-in, snapshot, per-path decisions, pull requests,
// branch history, memory, the commit and decide.DecideTarget), D the
// stale-PR sweep, E the gate (mass-close guard, rollout limit, write
// estimates), F execute (distribute only, execute.go) and G the report.
// Plan is Run in ModePlan.
//
// The files split the phases by concern: resolve.go (B), inspect.go (C,
// steps 1–5 and the decision), history.go (C step 6), commit.go (C step 8),
// memory.go (C step 7), body.go (the PR body and marker a work carries),
// legacy.go (the snapshot-only plan, without target repositories),
// sweep.go (D), gate.go (E), run.go (Run, Work and G).
//
// Invariants. Comments and tests cite them by number; the property, crash
// and scale tests check them:
//   - I1: a commit changes, against its parent B, exactly the paths of D.
//   - I2: nothing someone else put on a sync branch is lost.
//   - I3: touchmark writes to no pull request or branch but its own, and
//     never moves or deletes a branch that someone else's open pull request
//     uses.
//   - I4: no pull request opens whose changes the declines in force cover.
//   - I5: the head of an open pull request never equals its base or passes
//     through it, and touchmark never deletes the branch of an open pull
//     request: platforms close such a pull request, or record it as merged.
//   - I6: the commit a platform makes holds the tree touchmark built.
//   - I7: a second run on the same inputs writes nothing, body, title and
//     labels included.
//   - I8: a run from a hub commit that is not the tip of the hub's default
//     branch writes nothing.
//   - I9: plan holds no write credential, and a token goes only to its own
//     host, only in a header.
//   - I10: a mass close happens only when every target resolved, and below
//     the threshold.
//   - I11: everything read from a target is data; only the recreate and
//     repropose tick boxes act.
//   - I12: the write key is out of reach of hub jobs that run unreviewed
//     code, or the hub accepted that risk explicitly.
//   - I13: operations that bypass the guards come only from operations.yml
//     on the default branch, or from a local run.
package distribute

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/snapshot"
	"github.com/bedrock-python/touchmark/internal/sshsig"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// Provider is one provider with its drivers.
type Provider struct {
	Config config.ResolvedProvider
	// Reader reads in ModePlan, where it is required.
	Reader platform.Reader
	// Writer is the write identity of ModeDryRun and ModeDistribute, where it
	// is required and does the reading too (distribute holds only the writer);
	// Reader is then not used. ModePlan never calls it.
	Writer platform.Writer
	// Anonymous is set when Reader has no credential: in a hub pull request a
	// provider that the pull request adds or changes is planned without one.
	// Self is not called, and a failure to reach the provider is a warning
	// that leaves its resolve incomplete instead of making it unavailable.
	// Only ModePlan reads it.
	Anonymous bool
}

// Deps is everything a run needs. The CLI builds it; tests build it with the
// fake platform.
type Deps struct {
	Hub      *config.Hub
	Targets  *config.Targets
	Manifest *provenance.Manifest
	Current  provenance.Current
	// Known are the packs the hub ships now; the packs of Current when nil.
	Known map[string]bool
	// HubCommit is the hub commit this run reads (resolved HEAD).
	HubCommit string
	// HubContext comes from hubch.Detect; its Fingerprint is the hub's.
	HubContext hubch.Context
	// Fingerprint overrides HubContext.Fingerprint() (--hub-fp, tests).
	Fingerprint string
	// Channel checks the hub's default branch tip (guard I8); nil skips the
	// guard, with a warning in the report when the guard applies to the run
	// (HeadGuard: a local run, or a CI run of the default branch).
	Channel   hubch.Channel
	Providers []Provider
	// Snapshots takes the snapshots of the targets. When it also implements
	// Repos (snapshot.GitSource does), phase C reads branch history and
	// builds commits in each target's repository; ModeDryRun and
	// ModeDistribute require it. A plan over a source that is only a
	// snapshot.Source is the snapshot-only plan (see Plan).
	Snapshots snapshot.Source
	// Only restricts the run to these targets (--only); the sweep is then
	// off.
	Only   []config.Ref
	Strict bool
	Engine string
	// InCI says the run is a CI job, whatever CI runs it (the CLI's CI=true):
	// HubContext names only the CIs touchmark recognizes. In any CI an
	// unknown visibility counts as public.
	InCI bool
	// Scope limits a plan in a hub pull request to the targets the pull
	// request touches (scope.go); nil plans every target.
	// AssumeOptIn plans every target without an opt-in file as opted in
	// (plan --assume-opt-in, optInAndTree). Only ModePlan reads them.
	Scope       *Scope
	AssumeOptIn bool
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Concurrency bounds the targets of one provider inspected in parallel
	// (each provider has a pool of its own); 8 when zero. 1 inspects every
	// target in order.
	Concurrency int
	// Write is what Run needs beyond the snapshot-only plan. With Repos a
	// plan uses it too: HubBlobs (required) to build the commits, the
	// operations, Intro, HubURL, CanWorkflows, HubIsAncestor, Deadline and
	// Stream; the signing keys and Redact serve every mode that has them.
	Write WriteDeps

	// sleep waits between the attempts of a call that failed transiently
	// (sleepCtx when nil); tests make it instant.
	sleep sleepFunc
	// clocks give providers, by id, a clock of their own instead of Now and
	// sleep: a test's fake clock per provider, so that each provider's
	// calls are timed on its own timeline while their write queues run at
	// once (scale_test.go). The provider's Gate waits on it, and its
	// inspections and writes read the time from it.
	clocks map[string]throttle.Clock
	// git stands in for the targets' repositories in phase C (steps 6 and
	// 8) where a test has no git (scale_test.go): nil uses Repos.
	git targetGit
}

// ErrWriterMismatch is wrapped by the error of a distribute run whose write
// credential does not act as the writer hub.yml names for its provider, or
// whose provider names none (distribute compares providers[].writer with
// Self). The CLI exits 2 on it: nothing is written.
var ErrWriterMismatch = errors.New("the write credential is not the writer hub.yml names")

// ErrReaderIsWriter is wrapped by the error of a plan in CI whose read
// credential acts as the writer hub.yml names: plan runs for every branch of
// the hub, so a write credential there is as exposed as one the isolation
// probe finds. A local plan only warns.
var ErrReaderIsWriter = errors.New("the read credential acts as the writer")

// ErrHeadUnchecked is wrapped by the error of a dry run or distribute in CI
// that cannot tell whether the hub's HEAD is the tip of its default branch:
// the channel to the hub failed, or there is none (guard I8). A run that
// cannot rule out being stale writes nothing: the CLI exits 2 on it, and the
// next run tries again. A plan, which writes nothing, only warns.
var ErrHeadUnchecked = errors.New("the tip of the hub's default branch could not be read")

// Limits and defaults of a run.
const (
	// defaultConcurrency is how many targets of one provider are inspected
	// at once.
	defaultConcurrency = 8
	// maxOptIn bounds the opt-in file read through the API.
	maxOptIn = 64 << 10
	// shortHead is how many hex digits of a commit messages show.
	shortHead = 12
)

// sweepOffOnly is the sweep's reason with --only.
const sweepOffOnly = "off: --only restricts the run to some targets"

// fingerprintRe is the form of a hub fingerprint: host, optional port and
// the repository id: numeric, or a Bitbucket repository UUID (with or
// without braces, any case; config.CanonicalFingerprint makes it lowercase
// without braces).
var fingerprintRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?/([0-9]+|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}|\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\})$`)

// HeadGuard reports whether guard I8 (a run from an old hub commit does
// nothing) applies to a run in context c: a local run, or a CI run that
// builds the hub's default branch (or a branch, when the default branch is
// unknown). A hub pull request's commit is never the tip of the default
// branch, so the guard does not apply to it, nor to runs of other branches
// and tags.
func HeadGuard(c hubch.Context) bool {
	if c.CI == hubch.Local || c.CI == "" {
		return true
	}
	return c.RefIsBranch && (c.DefaultBranch == "" || c.RefName == c.DefaultBranch)
}

// Plan runs a read-only plan: Run(ctx, d, ModePlan), phases A, B, C, D, E
// and G of the package doc. Phases A, B and the first steps of C are
// described here; Run describes what follows.
//
// A. guards: a fingerprint is required (error without one); it and
// hub.PreviousFingerprints are compared in canonical form
// (config.CanonicalFingerprint). When Channel is set and its Head differs
// from HubCommit, the run outcome is Superseded and no target is inspected.
// A Channel that fails, or a nil Channel where HeadGuard applies, adds the
// warning "hub head not checked" to a plan and to a local run; a dry run or
// distribute in CI stops instead, with an error wrapping ErrHeadUnchecked
// (fail closed: a run that cannot rule out being stale writes nothing). A
// CI run (HubContext.CI other than Local) whose hub visibility is unknown
// treats the hub as public, with a warning: an unknown visibility never
// names non-public targets.
//
// B. resolve, per provider in hub.yml order: Probe and Self of the reader
// (Self is skipped for an Anonymous provider); Lookup of the writer,
// known_authors and automation_accounts (their stable ids feed
// decide.Identity and the classes of closes); every targets.yml entry whose
// provider is this one becomes a platform.Selector (repo: → Reader.Repo;
// org/group → Resolve with topics, subgroups, forks, keeping the
// repositories whose path matches one of the entry's match patterns);
// exclude removes targets (same matching as config, patterns included, by
// the canonical path and the path a repo: entry wrote; a repo: entry
// excluded as written is never looked up); web URLs in targets.yml are
// resolved against the run's providers first (config.ResolveURLs);
// targets are deduplicated by (Host, ID), and a repository two providers
// reach is handled by the provider of its first entry in targets.yml, with
// the target warning "duplicate-provider"; every target remembers the
// indexes of the entries that matched it, whichever provider matched. A Repo
// selector that returns ErrNotFound adds a warning "target-missing" and the
// path to ProviderInfo.Missing; a repo: entry whose path differs from the
// canonical one (other than by case) adds the target warning "renamed".
// Every call of a provider is paced by its throttle.Gate (the platform's
// limits from Probe, with hub.yml's providers[].limits over them). Errors
// are classified (platform.ClassOf), after three attempts of a call that
// failed transiently (1 and 2 s apart, with jitter), or after the pauses of
// a call that was rate limited (so for every read of the run):
//   - a cancelled or expired ctx stops the resolve: one run warning
//     (deferred:interrupted or deferred:deadline), no ProviderInfo.Error,
//     and no further platform calls for any provider;
//   - rate limits that put the provider out of budget (three in a row, or a
//     pause longer than the Gate allows), and a pause past the deadline,
//     defer the provider's remaining resolve (a run warning
//     "deferred:rate-limit" or "deferred:deadline"): its targets are
//     deferred without a call;
//   - a failed Probe or Self, and an auth error of Resolve or Repo, make the
//     provider unavailable (ProviderInfo.Error: exit 1) and end its resolve;
//     for an Anonymous provider they are warnings instead;
//   - other errors of Resolve and Repo, or an incomplete listing, add a
//     warning and mark the provider's resolve incomplete;
//   - a Lookup that finds no such login (or, for an Anonymous provider, is
//     refused with an auth or permission error) is a warning, and pull
//     requests of that login count as someone else's; a Lookup that fails
//     otherwise is remembered: a target with an open pull request on a sync
//     branch that could be touchmark's is then failed or deferred by that
//     error's class (step C5), never blocked on a guess.
//
// Every provider starts with ResolveComplete set; any of the above but a
// missing target clears it.
//
// C. inspect each target (bounded parallel):
//  1. classify: a public hub (visibility "public", or unknown in CI) with a
//     non-public target and security.private_targets_in_public_hub other
//     than "deliver" → skipped:private-in-public-hub, checked first and
//     reported without path or id; then archived, disabled, empty, mirror,
//     pending-deletion, prs-disabled, sha256 → skipped with that reason.
//     Past three quarters of the time Write.Deadline left at the start (the
//     rest is phase F's) a target is not started: deferred:deadline.
//  2. opt-in: Reader.ReadFile(default branch, hub.OptInName(), 64 KiB), or
//     a chunk of 50 targets at once where the reader is a
//     platform.BatchReader (optin.go; a file the batch cannot settle is read
//     on its own): not found (platform.ClassOf gives ClassNotFound) →
//     skipped:not-opted-in, unless an entry that selects the target has
//     opt_in: assumed, which takes an empty file for it (DeliveryTarget
//     Assumed, AssumedBy targets.yml); ErrNotRegular or ErrTooLarge →
//     skipped:unsafe-opt-in; parse error → blocked:opt-in-invalid;
//     enabled: false → skipped:opted-out, whose pull requests the sweep
//     closes as a missing file's. With AssumeOptIn a missing or irregular
//     file is taken for an empty one (AssumedBy --assume-opt-in).
//  3. packs: config.SelectFor with the matched entries; unknown packs →
//     blocked:opt-in-invalid with the message as a warning. A plan limited
//     by Scope stops here for a target whose final pack list holds no pack
//     of the scope, and for a target decided before its packs were known
//     whose hub packs hold none (scope.go): it is counted in Scope, not
//     listed, and the sweep and the mass-close guard treat it as a target
//     of the run.
//  4. snapshot: Snapshots.Snapshot(default branch, through Reader.Remote) →
//     tree (not found: skipped:empty); the opt-in file's blob in the tree
//     must be the one read through the API, else it is read again at the
//     tree's commit; decide.Layer, decide.Paths, tree.Observe, decide.Decide
//     (no Adopt; OptInFile set); D = decide.Pairs(plan); key =
//     decide.Key(StreamSync, D) when D is not empty; orphaned paths go to
//     the target's Orphaned list.
//
// With Repos, steps 5–9 of Run follow (pull requests in every state,
// branch history, memory, the commit, DecideTarget), then the sweep and
// the gate of Run.
//
// Without Repos (a Snapshots that is only a snapshot.Source), plan is the
// snapshot-only plan instead, which knows neither branch history nor memory
// and has no sweep:
//  5. PRs: Reader.PRs(repo, [branch]+aliases, authors) and decide.Identity
//     (fingerprints: current then hub.PreviousFingerprints); only open PRs
//     count:
//     - an own PR with an invalid marker (OursMarkerInvalid) →
//     blocked:marker-invalid; one that carries another hub's or another
//     version's marker instead is someone else's (decide.Own);
//     - when a Lookup of the provider failed (step B), an open PR from the
//     target repository on a sync branch whose author is not known to be
//     ours and that carries no marker of another hub → failed or deferred
//     by the class of the Lookup error;
//     - an open PR that is not ours, from the target repository itself, on
//     the sync branch this run would push to (the own PR's branch, else
//     hub.yml's branch when D is not empty) → blocked:branch-in-use, even
//     with an own open PR (I3). A PR from a fork is never ours and never
//     blocks: its branch lives in the fork; one carrying our marker adds a
//     warning;
//     - an own open PR (the one on hub.yml's branch when there are several):
//     D empty → closed:no-diff; marker key ≠ key → updated:content; base
//     other than the default branch → updated:base-renamed; else unchanged;
//     - no own open PR: D empty → unchanged; else opened.
//
// E. gate (without Repos): opened targets beyond limits.max_new_prs_per_run
// (0 opens none), in targets.yml order (first matching entry, then the
// platform's order within it), become deferred:rollout-limit; writes are
// estimated per target (opened 3 on GitHub, 2 elsewhere, plus 1 on GitHub,
// Gitea and Forgejo for the label when the target has no pull request of our
// authors yet and hub.yml sets labels; updated 2; closed 3) and summed per
// provider into Cost. With Repos the gate of Run applies.
//
// G. the report: report.Delivery with Command "plan", Summarize()d. With
// Only, the sweep's reason says it is off.
//
// Errors of one target never stop the others: a platform error becomes
// failed:<class> for that target (auth → failed:auth, permission and
// not-found → failed:access, transient → failed:transient after three
// attempts, a snapshot error without a class → failed:git, others →
// failed:internal), a rate limit pauses the provider and the call is made
// again, a call the throttle refused becomes deferred by its reason, and a
// cancelled or expired ctx deferred:interrupted or deferred:deadline. A
// provider out of budget (rate limited three times in a row), or that
// refused the credential for three targets in a row, is called no more: its
// remaining targets are deferred:rate-limit or deferred:provider-down (the
// provider's Gate). Each provider's targets are inspected at most as many at
// once as its limits allow. A run warning says how often and how long each
// provider paused. Plan returns an error only for problems of the run
// itself: no or a malformed fingerprint, no hub.yml id (no sync branch), no
// providers, readers or snapshot source, a targets.yml entry that does not
// parse, a Repos source without Write.HubBlobs, an unusable Write.Intro.
func Plan(ctx context.Context, d Deps) (*report.Delivery, error) {
	return Run(ctx, d, ModePlan)
}

// run is one run.
type run struct {
	d    Deps
	mode Mode
	rep  *report.Delivery
	hub  *config.Hub
	// targets is targets.yml (an empty one when absent).
	targets *config.Targets
	known   map[string]bool
	aliases provenance.Aliases
	// optIn is the opt-in file name.
	optIn string
	// branches are the sync branch, then the aliases; fps the hub's
	// fingerprint, then the previous ones.
	branches []string
	fps      []string
	provs    []*provider
	// hidePrivate is set when the hub is public (or its visibility is
	// unknown in CI) and does not deliver to non-public targets: they are
	// skipped and never named.
	hidePrivate bool
	// inCI is set when the run is a CI job (Deps.InCI or a known CI).
	inCI bool
	// stopped is set once the resolve saw the run cancelled or past its
	// deadline; resolve runs on one goroutine.
	stopped bool
	// repos is Snapshots as Repos; nil for the snapshot-only plan (legacy.go).
	repos Repos
	// clock reads the time; now is its reading at the start of the run,
	// which every decision of the run uses (operations, memory).
	clock func() time.Time
	now   time.Time
	// inspectBy is when inspections stop starting: before Write.Deadline,
	// so that phase F keeps part of the run's time (zero for none).
	inspectBy time.Time
	// sleep and jitter pace the retries of reads (retry) and of phase F.
	sleep  sleepFunc
	jitter func(time.Duration) time.Duration
	// cooldown is memory.auto_close_cooldown.
	cooldown time.Duration
	// ancestry caches Write.HubIsAncestor by the hub commit it was asked
	// about (per-target guard I8).
	ancestry map[string]ancestryAnswer
	// gated is what phase E let the run do (regate), under mu.
	gated gateCounts
	// err is an error of the run itself found during the resolve
	// (ErrWriterMismatch); resolve returns it.
	err error
	// mu guards the report's warnings, the completion order and the stream
	// while targets are inspected and executed in parallel.
	mu sync.Mutex
	// finishedAt counts the targets whose inspection finished.
	finishedAt int
	// streamErr is the first error writing Write.Stream; nothing more is
	// written to it after one.
	streamErr error
	// affected are the packs a limited plan processes the targets of
	// (scope.go); sweptOpen the open pull requests of touchmark its sweep
	// listed, for the mass-close guard, and swept the providers it listed.
	affected  map[string]bool
	sweptOpen map[string]bool
	swept     map[*provider]bool
	// batches read the opt-in files of each provider's targets in batches
	// (optin.go); set before phase C starts, read-only after.
	batches map[*provider]*optInBatch
}

// provider is one provider during a run.
type provider struct {
	cfg config.ResolvedProvider
	// reader reads: Provider.Reader in ModePlan, Provider.Writer otherwise.
	reader platform.Reader
	// anonymous is Provider.Anonymous (ModePlan only).
	anonymous bool
	// info is the provider's line in the report.
	info *report.ProviderInfo
	// authors are the writer and known_authors that resolved; ids their
	// stable ids.
	authors []platform.Account
	ids     []string
	// lookupErr is the first Lookup error other than an unknown login:
	// the authors may be incomplete, so a pull request of an unknown author
	// may still be ours.
	lookupErr error
	// writer is the provider's write identity (nil in ModePlan),
	// self the account it acts as (the commit author and committer), caps
	// what Probe reported, signer the SSH signing key (nil for none).
	writer platform.Writer
	self   platform.Account
	caps   platform.Caps
	signer *sshsig.Signer
	// writerAcct is the writer hub.yml names, as Lookup found it (zero when
	// it did not): the commit author of a plan, which has no Self of the
	// writer.
	writerAcct platform.Account
	// automation are the stable ids of automation_accounts that resolved.
	automation map[string]bool
	// gate is the provider's throttle: its pacing, its pauses, and what its
	// failures told the run so far (circuit.go). gateOnce guards the Gate of a
	// provider built without one.
	gate     *throttle.Gate
	gateOnce sync.Once
	// clock is the provider's own clock (Deps.clocks); nil reads the run's.
	clock func() time.Time
	// apiUnsigned, once set, says why the provider's API commits do not sign
	// (the first unsigned answer of phase F): for the rest of the run a push
	// that needs a signature and has no key is blocked:cannot-sign. Guarded by
	// apiMu: phase F's queue and the re-inspections it runs read it.
	apiMu       sync.Mutex
	apiUnsigned string
}

// unsignedAPI returns why the provider's API commits do not sign, "" while
// none came back unsigned in this run.
func (p *provider) unsignedAPI() string {
	p.apiMu.Lock()
	defer p.apiMu.Unlock()
	return p.apiUnsigned
}

// markUnsignedAPI records the first unsigned API commit of the provider.
func (p *provider) markUnsignedAPI(why string) {
	p.apiMu.Lock()
	defer p.apiMu.Unlock()
	if p.apiUnsigned == "" {
		p.apiUnsigned = why
	}
}

// target is one resolved target.
type target struct {
	// prov is the provider that handles the target: the one of its first
	// entry in targets.yml; repo is the target as that provider reported
	// it.
	prov *provider
	repo platform.Repo
	host string
	// written are the paths repo: entries wrote for it.
	written []string
	// entries are the targets.yml entries that matched it, providers the
	// ids of every provider that found it (prov first). assumed is set when
	// one of the entries has opt_in: assumed: the target counts as opted in
	// without an opt-in file.
	entries   []int
	providers []string
	assumed   bool
	// first is the first entry that found it and its position in that
	// entry's listing: its place in targets.yml order.
	first  [2]int
	hidden bool
	// warnings are what the resolve found about the target (duplicate
	// provider, renamed); every inspection starts its report line with them.
	warnings []string
	// noOwnPRs is set when the platform lists no pull request of our
	// authors on the sync branches, in any state: opening one creates the
	// label first.
	noOwnPRs bool
	res      report.DeliveryTarget
	// remote is how the snapshot reached the target (the reader's remote).
	remote platform.Remote
	// work is the target's work after phase C; nil when its outcome needs
	// none. sweeps are the sweep closes of its pull requests (opted-out, or
	// target-dropped for a target the sweep added).
	work   *Work
	sweeps []*Work
	// dropped marks a repository the sweep added: it is no target of the
	// run any more.
	dropped bool
	// optOut is why the sweep closes the pull requests of a target that is
	// not opted in (prbody.CauseDisabled, prbody.CauseNoOptIn), for the
	// comment after each close; "" for any other target.
	optOut string
	// outOfScope marks a target a limited plan leaves out (scope.go): it
	// stays a target of the run, without a report line. optIn is its opt-in
	// file as a batch read it, until phase C takes it (optin.go).
	outOfScope bool
	optIn      *optInEntry
	// later are the writes of a target deferred:rollout-limit: a later run
	// makes them (estimate.go).
	later pathWrites
	// order is the rank of the target's inspection among those that
	// finished (the completion order of the stream); done is set once its
	// report line was streamed and its repository released.
	order int
	done  bool
}

// commandOf is the report's command for mode.
func commandOf(mode Mode) string {
	switch mode {
	case ModePlan:
		return "plan"
	case modeDoctor:
		return "doctor"
	}
	return "distribute"
}

// checkDeps rejects dependencies a run in mode cannot work with, and
// returns the hub's fingerprint (as given, not canonical yet) and the
// snapshot source as Repos (nil when it is not one).
func checkDeps(d Deps, mode Mode) (fp string, repos Repos, err error) {
	if d.Hub == nil {
		return "", nil, errors.New("no hub.yml")
	}
	fp = d.Fingerprint
	if fp == "" {
		fp = d.HubContext.Fingerprint()
	}
	switch {
	case fp == "":
		return "", nil, errors.New("the hub's fingerprint (host/repository-id) is unknown")
	case !fingerprintRe.MatchString(fp):
		return "", nil, fmt.Errorf("the hub's fingerprint %q is not host/repository-id, like github.com/712345678", fp)
	case d.Hub.Branch == "":
		return "", nil, errors.New("hub.yml has no id, so the sync branch has no name")
	case len(d.Providers) == 0:
		return "", nil, errors.New("no providers")
	case d.Snapshots == nil && mode != modeDoctor:
		return "", nil, errors.New("no snapshot source")
	}
	repos, _ = d.Snapshots.(Repos)
	switch {
	case mode != ModePlan && mode != modeDoctor && repos == nil:
		return "", nil, errors.New("the snapshot source hands out no target repositories (Repos), which distribute needs for branch history, commits and pushes")
	case repos != nil && d.Write.HubBlobs == nil:
		return "", nil, errors.New("no source of hub blobs (WriteDeps.HubBlobs) to build the commits with")
	}
	if d.Write.Intro != "" {
		if err := prbody.CheckIntro(d.Write.Intro); err != nil {
			return "", nil, fmt.Errorf("pr.intro_file: %w", err)
		}
	}
	return fp, repos, nil
}

func newRun(d Deps, mode Mode) (*run, error) {
	fp, repos, err := checkDeps(d, mode)
	if err != nil {
		return nil, err
	}
	fp = config.CanonicalFingerprint(fp)
	fps := []string{fp}
	for _, prev := range d.Hub.PreviousFingerprints {
		fps = append(fps, config.CanonicalFingerprint(prev))
	}
	known := d.Known
	if known == nil {
		known = map[string]bool{}
		for pack := range d.Current {
			known[pack] = true
		}
	}
	clock := d.Now
	if clock == nil {
		clock = time.Now
	}
	inCI := d.InCI || (d.HubContext.CI != hubch.Local && d.HubContext.CI != "")
	visibility := d.HubContext.Visibility
	unknownInCI := inCI && visibility == ""
	deliver := d.Hub.Security.PrivateTargetsInPublicHub == "deliver"
	r := &run{
		d:           d,
		mode:        mode,
		rep:         report.NewDelivery(commandOf(mode), d.Engine),
		hub:         d.Hub,
		targets:     d.Targets,
		known:       known,
		aliases:     provenance.Aliases(d.Hub.KnownAliases(known)),
		optIn:       d.Hub.OptInName(),
		branches:    uniqueNonEmpty(append([]string{d.Hub.Branch}, d.Hub.BranchAliases...)),
		fps:         uniqueNonEmpty(fps),
		hidePrivate: (visibility == "public" || unknownInCI) && !deliver,
		inCI:        inCI,
		repos:       repos,
		clock:       clock,
		now:         clock(),
		sleep:       d.sleep,
		jitter:      jitter,
		cooldown:    parseCooldown(d.Hub.Memory.AutoCloseCooldown),
		ancestry:    map[string]ancestryAnswer{},
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	r.inspectBy = inspectDeadline(r.now, d.Write.Deadline)
	if r.targets == nil {
		r.targets = &config.Targets{}
	}
	if r.targets.HasURLs() {
		// The CLI resolves them as it reads targets.yml; a caller that did
		// not gets them resolved against the run's providers.
		rps := make([]config.ResolvedProvider, len(d.Providers))
		for i, p := range d.Providers {
			rps[i] = p.Config
		}
		targets, err := config.ResolveURLs(r.targets, rps)
		if err != nil {
			return nil, err
		}
		r.targets = targets
	}
	r.affected = r.scopePacks()
	r.rep.Strict = d.Strict
	r.rep.Assumed = mode == ModePlan && d.AssumeOptIn
	r.rep.DryRun = mode == ModeDryRun
	r.rep.Hub = report.DeliveryHub{ID: d.Hub.ID, Fingerprint: fp, Commit: d.HubCommit}
	r.rep.Providers = make([]report.ProviderInfo, len(d.Providers))
	for i, p := range d.Providers {
		prov, err := r.newProvider(p, &r.rep.Providers[i])
		if err != nil {
			return nil, err
		}
		r.rep.Cost[p.Config.ID] = 0
		r.provs = append(r.provs, prov)
	}
	if unknownInCI && !deliver {
		r.warnf("the hub's visibility is unknown (the CI event says nothing about it): the hub is treated as public, " +
			"so non-public targets are skipped and not named")
	}
	return r, nil
}

// newProvider makes the run's provider of p, whose report line is info.
func (r *run) newProvider(p Provider, info *report.ProviderInfo) (*provider, error) {
	prov := &provider{cfg: p.Config, reader: p.Reader, anonymous: p.Anonymous, info: info,
		signer: r.d.Write.Signers[p.Config.ID], gate: r.newGate(p.Config)}
	if c, ok := r.d.clocks[p.Config.ID]; ok {
		prov.clock = c.Now
	}
	if r.mode != ModePlan {
		if p.Writer == nil {
			return nil, fmt.Errorf("provider %s has no writer", p.Config.ID)
		}
		prov.writer, prov.reader, prov.anonymous = p.Writer, p.Writer, false
	}
	if prov.reader == nil {
		return nil, fmt.Errorf("provider %s has no reader", p.Config.ID)
	}
	*info = report.ProviderInfo{
		ID:              p.Config.ID,
		Type:            p.Config.Type,
		Host:            p.Config.Host,
		Writer:          p.Config.Writer,
		WriteCheck:      "not checked",
		ResolveComplete: true,
	}
	return prov, nil
}

// parseCooldown reads memory.auto_close_cooldown ("30d", "12h"); zero, which
// decide reads as its default of 30 days, for anything else.
func parseCooldown(s string) time.Duration {
	if len(s) < 2 {
		return 0
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0
	}
	switch s[len(s)-1] {
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	case 'h':
		return time.Duration(n) * time.Hour
	}
	return 0
}

func (r *run) warnf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf(format, args...))
}

// superseded applies guard I8 and reports whether the run stops there. A
// dry run or distribute in CI that cannot read the tip of the hub's default
// branch returns an error wrapping ErrHeadUnchecked (see Plan's doc); a
// plan and a local run warn instead.
func (r *run) superseded(ctx context.Context) (bool, error) {
	local := r.d.HubContext.CI == hubch.Local || r.d.HubContext.CI == ""
	failClosed := r.mode != ModePlan && !local
	if r.d.Channel == nil {
		switch {
		case !HeadGuard(r.d.HubContext):
		case local:
			r.warnf("hub head not checked: a local run cannot read the tip of the hub's default branch")
		case failClosed:
			return false, fmt.Errorf("%w: no channel to the hub; nothing is written", ErrHeadUnchecked)
		default:
			r.warnf("hub head not checked: no channel to the hub")
		}
		return false, nil
	}
	head, err := r.d.Channel.Head(ctx)
	if err != nil {
		if failClosed {
			return false, fmt.Errorf("%w: %w; nothing is written until a run can read it", ErrHeadUnchecked, err)
		}
		r.warnf("hub head not checked: %v", err)
		return false, nil
	}
	if strings.EqualFold(head, r.d.HubCommit) {
		return false, nil
	}
	r.rep.Outcome = report.Superseded
	r.rep.Providers = []report.ProviderInfo{}
	r.rep.Cost = map[string]int{}
	r.warnf("the tip of the hub's default branch is %s, not %s: this run is superseded and inspects no target",
		short(head), short(r.d.HubCommit))
	return true, nil
}

// fail records err of step what as the target's outcome: deferred when the
// run was cancelled, its deadline passed, the throttle refused the call or
// the platform limits the rate; else failed with the reason of the error's
// class, fallback for an error without one. What the error says about the
// provider (a refused credential, a plain 403) goes to its Gate. A target a
// public hub does not name gets no warning: platform messages may name it.
func (r *run) fail(ctx context.Context, t *target, what string, err error, fallback string) {
	r.record(ctx, t, what, err, fallback)
	if _, refused := throttle.Refused(err); ctx.Err() == nil && !refused {
		t.prov.failed(err)
	}
}

// record is fail without the circuit: for an error told again (a Lookup of
// phase B that failed), which the circuit heard once already.
func (r *run) record(ctx context.Context, t *target, what string, err error, fallback string) {
	res := &t.res
	refusal, refused := throttle.Refused(err)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Outcome, res.Reason = report.OutcomeDeferred, "deadline"
	case ctx.Err() != nil:
		res.Outcome, res.Reason = report.OutcomeDeferred, "interrupted"
	case refused:
		res.Outcome, res.Reason = report.OutcomeDeferred, refusal.Reason
	case platform.ClassOf(err) == platform.ClassRateLimited:
		res.Outcome, res.Reason = report.OutcomeDeferred, "rate-limit"
	default:
		res.Outcome, res.Reason = report.OutcomeFailed, failedReason(err, fallback)
	}
	r.targetWarning(t, what+": "+err.Error())
}

// targetWarning adds a warning to t's report line, unless t is a target a
// public hub does not name.
func (r *run) targetWarning(t *target, why string) {
	if why != "" && !t.hidden {
		t.res.Warnings = append(t.res.Warnings, why)
	}
}

// inspectShare is the part of the time to Write.Deadline in which
// inspections start: the rest is phase F's, so that a phase C as long as
// the run still leaves time to write (deferred:deadline).
const inspectShare = 3

// inspectShareOf is the denominator of inspectShare: inspections start in
// the first three quarters of the time to the deadline.
const inspectShareOf = 4

// inspectDeadline returns when inspections stop starting for a run started
// at start with deadline (zero for none): three quarters of the way.
func inspectDeadline(start, deadline time.Time) time.Time {
	if deadline.IsZero() || !start.Before(deadline) {
		return deadline
	}
	return start.Add(deadline.Sub(start) * inspectShare / inspectShareOf)
}

// failedReason maps an error to a failed reason.
func failedReason(err error, fallback string) string {
	switch platform.ClassOf(err) {
	case platform.ClassAuth:
		return "auth"
	case platform.ClassPermission, platform.ClassNotFound:
		return "access"
	case platform.ClassTransient:
		return "transient"
	case platform.ClassUnknown:
		return fallback
	}
	return "internal"
}

// skipReason returns why a repository is skipped, or "".
func skipReason(r platform.Repo) string {
	switch {
	case r.Archived:
		return "archived"
	case r.Disabled:
		return "disabled"
	case r.Empty:
		return "empty"
	case r.Mirror:
		return "mirror"
	case r.PendingDelete:
		return "pending-deletion"
	case r.PRsDisabled:
		return "prs-disabled"
	case strings.EqualFold(r.ObjectFormat, "sha256"):
		return "sha256"
	}
	return ""
}

// isNotFound reports whether err says the thing does not exist: its class
// is ClassNotFound. A classified *platform.Error keeps its own class over a
// sentinel it wraps, so a 401 that wraps ErrNotFound is an auth error, not
// a missing file (platform.ClassOf).
func isNotFound(err error) bool {
	return platform.ClassOf(err) == platform.ClassNotFound
}

func prRef(pr platform.PR) *report.PRRef {
	return &report.PRRef{Number: pr.Number, URL: pr.URL, State: string(pr.State)}
}

// short returns the first hex digits of a commit id for messages.
func short(oid string) string {
	if len(oid) > shortHead {
		return oid[:shortHead]
	}
	return oid
}

// uniqueNonEmpty returns list without empty strings and repeats, in order.
func uniqueNonEmpty(list []string) []string {
	var out []string
	for _, s := range list {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// flatten splits joined errors (errors.Join, as the config parsers return
// them) into one message each.
func flatten(err error) []string {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range j.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []string{err.Error()}
}
