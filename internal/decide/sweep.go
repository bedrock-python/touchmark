package decide

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Close reasons.
const (
	ReasonNoDiff        = "no-diff"
	ReasonOptedOut      = "opted-out"
	ReasonTargetDropped = "target-dropped"
	ReasonDuplicate     = "duplicate"
)

// sweepMinClose is the smallest mass-close threshold: a run may always
// close this many PRs.
const sweepMinClose = 5

// SweepCandidate is an own open PR found by Reader.OpenPRsBy.
type SweepCandidate struct {
	Provider string
	Repo     platform.Repo
	PR       OwnPR
}

// SweepInput is one provider's sweep.
type SweepInput struct {
	Candidates []SweepCandidate
	// Active holds "host/id" of every target this run resolved on the
	// provider (whatever their outcome). Keys carry the host, so the caller
	// may, and should, list the targets of every provider of the run: a
	// repository another provider on the same host handles is still a
	// target. Hosts compare ignoring case.
	Active map[string]bool
	// Unresolved holds lowercased paths of explicit repo: targets that
	// could not be resolved (404): their PRs are never swept, nor those of
	// any repository with the same name (the last segment of the path),
	// wherever it lives: an owner renamed on GitHub leaves no API redirect,
	// so the target is missing under its old path while the sweep finds
	// its pull requests under the new one.
	Unresolved map[string]bool
	// OptedOut holds "host/id" of the Active targets that are not opted in
	// (skipped:not-opted-in: no opt-in file, and targets.yml does not
	// subscribe them; skipped:opted-out: the file says enabled: false):
	// their own open PRs are closed with ReasonOptedOut. DecideTarget never
	// runs for them.
	OptedOut map[string]bool
	// Complete is set when the provider's resolve and the OpenPRsBy
	// listing were both complete; otherwise nothing is swept.
	Complete bool
	// Only is set for --only runs: nothing is swept.
	Only bool
}

// SweepClose is one PR to close.
type SweepClose struct {
	Candidate SweepCandidate
	Reason    string // ReasonTargetDropped or ReasonOptedOut
}

// Sweep returns the own open PRs of repositories that are no longer
// targets: repositories not in Active and not Unresolved (by path, or by
// name: see SweepInput.Unresolved), with ReasonTargetDropped; and those of
// targets in OptedOut, with ReasonOptedOut. It returns nil when !Complete or
// Only; the caller reports why.
//
// It fails closed on what it cannot place: a candidate is never swept when
// its repository has no host, id or path, its PR is not open or has no
// number, or the PR's repository id or head repository id differs from the
// repository's (a fork's PR, or a listing that mixed them up: every
// candidate must pass the own-PR rule in full). Candidates listed twice
// (same host, repository id and PR number) give one close. Archived
// repositories are returned like any other: the caller reports
// blocked:archived for them.
//
// The result is sorted by provider, host, path (ignoring case, then
// exactly), repository id and PR number, whatever the order of Candidates.
func Sweep(in SweepInput) []SweepClose {
	if !in.Complete || in.Only {
		return nil
	}
	fold := func(keys map[string]bool) map[string]bool {
		out := make(map[string]bool, len(keys))
		for k, v := range keys {
			if v {
				out[sweepFoldHost(k)] = true
			}
		}
		return out
	}
	active, optedOut := fold(in.Active), fold(in.OptedOut)
	unresolved := make(map[string]bool, len(in.Unresolved))
	names := make(map[string]bool, len(in.Unresolved))
	for k, v := range in.Unresolved {
		if v {
			unresolved[strings.ToLower(k)] = true
			names[sweepName(k)] = true
		}
	}
	seen := map[string]bool{}
	var out []SweepClose
	for _, c := range in.Candidates {
		r, pr := c.Repo, c.PR.PR
		key := sweepFoldHost(r.Host + "/" + r.ID)
		reason := ReasonTargetDropped
		switch {
		case r.Host == "" || r.ID == "" || r.Path == "":
			continue
		case pr.State != platform.Open || pr.Number <= 0 || pr.RepoID != r.ID || pr.HeadRepoID != r.ID:
			continue
		case optedOut[key]:
			reason = ReasonOptedOut
		case active[key] || unresolved[strings.ToLower(r.Path)] || names[sweepName(r.Path)]:
			continue
		}
		id := key + "#" + strconv.FormatInt(pr.Number, 10)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, SweepClose{Candidate: c, Reason: reason})
	}
	slices.SortFunc(out, func(a, b SweepClose) int {
		x, y := a.Candidate, b.Candidate
		return cmp.Or(
			strings.Compare(x.Provider, y.Provider),
			strings.Compare(strings.ToLower(x.Repo.Host), strings.ToLower(y.Repo.Host)),
			strings.Compare(strings.ToLower(x.Repo.Path), strings.ToLower(y.Repo.Path)),
			strings.Compare(x.Repo.Path, y.Repo.Path),
			strings.Compare(x.Repo.ID, y.Repo.ID),
			cmp.Compare(x.PR.PR.Number, y.PR.PR.Number),
		)
	})
	return out
}

// sweepName is the lowercased name of a repository path: its last
// segment.
func sweepName(path string) string {
	path = strings.ToLower(strings.TrimRight(path, "/"))
	return path[strings.LastIndexByte(path, '/')+1:]
}

// sweepFoldHost lowercases the host part of a "host/id" key: everything
// before its last '/'. Ids keep their case.
func sweepFoldHost(key string) string {
	i := strings.LastIndexByte(key, '/')
	if i < 0 {
		return key
	}
	return strings.ToLower(key[:i]) + key[i:]
}

// MassCloseInput is the mass-close guard's input.
type MassCloseInput struct {
	// Closes counts every close this run would make, for all reasons.
	Closes int
	// OwnOpen counts own open PRs seen this run (targets and sweep).
	OwnOpen int
	// MaxFraction is limits.max_close_fraction.
	MaxFraction float64
	// Allow is operations.yml allow_mass_close, nil when absent.
	Allow *config.MassCloseOp
	Now   time.Time
}

// MassCloseAllowed reports whether Closes may run: Closes ≤ max(5,
// floor(MaxFraction × OwnOpen)), or an allow_mass_close entry whose until
// date (inclusive, UTC) is not past and whose max ≥ Closes. limit is the
// threshold that applied.
//
// limit is max(5, floor(MaxFraction × OwnOpen)), raised to Allow.Max while
// Allow is active (config.MassCloseOp.Active), and ok is Closes ≤ limit.
// The product is rounded to six decimals before the floor, so float noise
// (0.29 × 100 = 28.999999999999996) does not cost a close. A fraction
// outside what hub.yml allows counts as the nearest bound: one that is not
// a positive number (NaN included) as 0, one above 1 as 1.
func MassCloseAllowed(in MassCloseInput) (ok bool, limit int) {
	limit = max(sweepMinClose, sweepFraction(in.MaxFraction, in.OwnOpen))
	if in.Allow.Active(in.Now) && in.Allow.Max > limit {
		limit = in.Allow.Max
	}
	return in.Closes <= limit, limit
}

// sweepFraction returns floor(f × n) for the mass-close threshold, with f
// clamped to [0, 1] and the result to [0, MaxInt32].
func sweepFraction(f float64, n int) int {
	switch {
	case !(f > 0) || n <= 0: // also NaN
		return 0
	case f > 1:
		f = 1
	}
	x := f * float64(n)
	if x >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(math.Floor(math.Round(x*1e6) / 1e6))
}
