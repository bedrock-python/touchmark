package decide

import (
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// swpCandidate is own open PR number of the repository id at path on host.
func swpCandidate(provider, host, id, path string, number int64) SweepCandidate {
	return SweepCandidate{
		Provider: provider,
		Repo:     platform.Repo{Host: host, ID: id, Path: path, DefaultBranch: "main", Visibility: "public"},
		PR: OwnPR{PR: platform.PR{
			Number: number, State: platform.Open, Head: "touchmark/acme-eng", Base: "main",
			RepoID: id, HeadRepoID: id, BaseExists: true, Author: memWriter,
		}},
	}
}

// swpNumbers returns "path#number" of every close, in order.
func swpNumbers(closes []SweepClose) []string {
	var out []string
	for _, c := range closes {
		out = append(out, c.Candidate.Repo.Path+"#"+strconv.FormatInt(c.Candidate.PR.PR.Number, 10))
	}
	return out
}

func TestSweep(t *testing.T) {
	dropped := swpCandidate("gh", "github.com", "2", "acme/old", 9)
	archived := swpCandidate("gh", "github.com", "4", "acme/zeta", 2)
	archived.Repo.Archived = true
	closed := swpCandidate("gh", "github.com", "6", "acme/closed", 4)
	closed.PR.PR.State = platform.Closed
	merged := swpCandidate("gh", "github.com", "6", "acme/closed", 5)
	merged.PR.PR.State = platform.Merged
	mixed := swpCandidate("gh", "github.com", "7", "acme/mixed", 1)
	mixed.PR.PR.RepoID = "8"
	// A fork's PR that reached the listing: its head lives in another
	// repository (threat T9 of docs/project/threat-model.md).
	fork := swpCandidate("gh", "github.com", "13", "acme/forked", 2)
	fork.PR.PR.HeadRepoID = "999"
	noHead := swpCandidate("gh", "github.com", "14", "acme/nohead", 2)
	noHead.PR.PR.HeadRepoID = ""
	optedOut := swpCandidate("gh", "github.com", "15", "acme/opted-out", 8)
	optedOutFork := swpCandidate("gh", "github.com", "15", "acme/opted-out", 9)
	optedOutFork.PR.PR.HeadRepoID = "999"
	candidates := []SweepCandidate{
		swpCandidate("gh", "github.com", "1", "acme/api", 5), // a target
		dropped, // swept
		swpCandidate("gh", "GITHUB.COM", "3", "acme/Missing", 3), // an unresolved explicit target
		swpCandidate("gh", "", "9", "acme/nohost", 1),            // no host
		swpCandidate("gh", "github.com", "", "acme/noid", 1),     // no id
		swpCandidate("gh", "github.com", "10", "", 1),            // no path
		closed, merged, // not open
		mixed,        // the PR's repository differs
		fork, noHead, // the head's repository differs
		dropped,  // listed twice
		archived, // swept: the caller reports blocked:archived
		swpCandidate("gh", "github.com", "5", "acme/active-by-case", 1), // Active spells the host in capitals
		swpCandidate("gh", "github.com", "11", "acme/zero", 0),          // no number
		swpCandidate("gh", "github.com", "12", "acme/inactive", 6),      // Active maps it to false
		optedOut,     // an active target whose opt-in file is gone
		optedOutFork, // a fork's PR there
	}
	in := SweepInput{
		Candidates: candidates,
		Active:     map[string]bool{"github.com/1": true, "GitHub.COM/5": true, "github.com/12": false, "github.com/15": true},
		Unresolved: map[string]bool{"acme/missing": true},
		OptedOut:   map[string]bool{"GitHub.com/15": true, "github.com/1": false},
		Complete:   true,
	}
	want := []SweepClose{
		{Candidate: swpCandidate("gh", "github.com", "12", "acme/inactive", 6), Reason: ReasonTargetDropped},
		{Candidate: dropped, Reason: ReasonTargetDropped},
		{Candidate: optedOut, Reason: ReasonOptedOut},
		{Candidate: archived, Reason: ReasonTargetDropped},
	}
	if got := Sweep(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("Sweep = %v, want %v", swpNumbers(got), swpNumbers(want))
	}

	// The order does not depend on the listing's.
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		shuffled := slices.Clone(candidates)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		in.Candidates = shuffled
		if got := Sweep(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("Sweep of a shuffled listing = %v, want %v", swpNumbers(got), swpNumbers(want))
		}
	}
	in.Candidates = candidates

	for name, edit := range map[string]func(*SweepInput){
		"an incomplete resolve or listing": func(in *SweepInput) { in.Complete = false },
		"--only":                           func(in *SweepInput) { in.Only = true },
	} {
		c := in
		edit(&c)
		if got := Sweep(c); got != nil {
			t.Errorf("%s: Sweep = %v, want nothing", name, swpNumbers(got))
		}
	}
	if got := Sweep(SweepInput{Complete: true}); got != nil {
		t.Errorf("Sweep of no candidates = %v", got)
	}
}

// TestSweepRenamedOwner: acme is renamed to acme-eng while targets.yml
// still names repo: acme/svc. GitHub answers the old owner's paths with
// 404 (docs), so the target is unresolved, while the sweep lists its pull
// request under acme-eng/svc: it must not be closed as target-dropped. A
// repository of another name is swept as before.
func TestSweepRenamedOwner(t *testing.T) {
	moved := swpCandidate("gh", "github.com", "21", "acme-eng/svc", 4)
	other := swpCandidate("gh", "github.com", "22", "acme-eng/old", 5)
	nested := swpCandidate("gl", "gitlab.example.com", "23", "group/sub/SVC", 6)
	in := SweepInput{
		Candidates: []SweepCandidate{moved, other, nested},
		Active:     map[string]bool{},
		Unresolved: map[string]bool{"acme/svc": true},
		Complete:   true,
	}
	if got := swpNumbers(Sweep(in)); !slices.Equal(got, []string{"acme-eng/old#5"}) {
		t.Errorf("Sweep = %q, want only acme-eng/old#5", got)
	}
	if got := sweepName("Group/Sub/SVC"); got != "svc" {
		t.Errorf("sweepName = %q", got)
	}
}

func TestSweepOrder(t *testing.T) {
	in := SweepInput{Complete: true, Candidates: []SweepCandidate{
		swpCandidate("gl", "gitlab.example.com:8443", "3", "group/b", 1),
		swpCandidate("gh", "github.com", "4", "acme/B", 2),
		swpCandidate("gh", "github.com", "1", "acme/a", 7),
		swpCandidate("gh", "github.com", "1", "acme/a", 3),
		swpCandidate("gh", "GitHub.com", "2", "acme/b", 1),
	}}
	var got []string
	for _, c := range Sweep(in) {
		got = append(got, c.Candidate.Provider+" "+c.Candidate.Repo.Path+" "+c.Candidate.Repo.ID)
	}
	// Paths compare ignoring case first, then exactly ('B' before 'b'),
	// then by repository id and PR number.
	want := []string{"gh acme/a 1", "gh acme/a 1", "gh acme/B 4", "gh acme/b 2", "gl group/b 3"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
	if got := sweepFoldHost("GitLab.Example.com:8443/AbC"); got != "gitlab.example.com:8443/AbC" {
		t.Errorf("sweepFoldHost = %q", got)
	}
}

func TestSweepMassCloseAllowed(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	allow := func(max int, until string) *config.MassCloseOp { return &config.MassCloseOp{Max: max, Until: until} }
	moscow := time.FixedZone("MSK", 3*60*60)
	cases := []struct {
		name  string
		in    MassCloseInput
		ok    bool
		limit int
	}{
		{"nothing to close", MassCloseInput{MaxFraction: 0.1}, true, 5},
		{"five of ten", MassCloseInput{Closes: 5, OwnOpen: 10, MaxFraction: 0.1}, true, 5},
		{"six of ten", MassCloseInput{Closes: 6, OwnOpen: 10, MaxFraction: 0.1}, false, 5},
		{"ten of a hundred", MassCloseInput{Closes: 10, OwnOpen: 100, MaxFraction: 0.1}, true, 10},
		{"eleven of a hundred", MassCloseInput{Closes: 11, OwnOpen: 100, MaxFraction: 0.1}, false, 10},
		{"the floor", MassCloseInput{Closes: 6, OwnOpen: 69, MaxFraction: 0.1}, true, 6},
		{"the floor, one more", MassCloseInput{Closes: 7, OwnOpen: 69, MaxFraction: 0.1}, false, 6},
		{"float noise below", MassCloseInput{Closes: 29, OwnOpen: 100, MaxFraction: 0.29}, true, 29},
		{"float noise below, again", MassCloseInput{Closes: 57, OwnOpen: 100, MaxFraction: 0.57}, true, 57},
		{"float noise above", MassCloseInput{Closes: 7, OwnOpen: 70, MaxFraction: 0.1}, true, 7},
		{"a fraction of all", MassCloseInput{Closes: 70, OwnOpen: 70, MaxFraction: 1}, true, 70},
		{"a fraction above 1 counts as 1", MassCloseInput{Closes: 71, OwnOpen: 70, MaxFraction: 2}, false, 70},
		{"an infinite fraction counts as 1", MassCloseInput{Closes: 71, OwnOpen: 70, MaxFraction: math.Inf(1)}, false, 70},
		{"no fraction", MassCloseInput{Closes: 6, OwnOpen: 100}, false, 5},
		{"a negative fraction", MassCloseInput{Closes: 6, OwnOpen: 100, MaxFraction: -0.5}, false, 5},
		{"a fraction that is not a number", MassCloseInput{Closes: 6, OwnOpen: 100, MaxFraction: math.NaN()}, false, 5},
		{"negative counts", MassCloseInput{Closes: -1, OwnOpen: -100, MaxFraction: 0.1}, true, 5},
		{"allowed", MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-10-01"), Now: now}, true, 400},
		{"allowed, one too many", MassCloseInput{Closes: 401, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-10-01"), Now: now}, false, 400},
		{"allowed until yesterday", MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-09-24"), Now: now}, false, 100},
		{"allowed until today", MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-09-25"), Now: now}, true, 400},
		{
			"allowed until today, the last second in UTC",
			MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-09-25"), Now: time.Date(2026, 9, 25, 23, 59, 59, 0, time.UTC)},
			true, 400,
		},
		{
			"allowed until yesterday in UTC, today in Moscow",
			MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-09-25"), Now: time.Date(2026, 9, 26, 1, 0, 0, 0, moscow)},
			true, 400,
		},
		{
			"allowed until yesterday in UTC, midnight",
			MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "2026-09-25"), Now: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)},
			false, 100,
		},
		{"an allowance below the threshold", MassCloseInput{Closes: 8, OwnOpen: 100, MaxFraction: 0.1, Allow: allow(3, "2026-10-01"), Now: now}, true, 10},
		{"a malformed until", MassCloseInput{Closes: 400, OwnOpen: 1000, MaxFraction: 0.1, Allow: allow(400, "soon"), Now: now}, false, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, limit := MassCloseAllowed(c.in)
			if ok != c.ok || limit != c.limit {
				t.Errorf("MassCloseAllowed = %v %d, want %v %d", ok, limit, c.ok, c.limit)
			}
		})
	}
	if got := sweepFraction(1, math.MaxInt); got != math.MaxInt32 {
		t.Errorf("sweepFraction(1, MaxInt) = %d, want MaxInt32", got)
	}
}
