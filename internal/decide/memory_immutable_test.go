package decide

import (
	"slices"
	"testing"
)

// memImmutable is memInput on a platform whose closed pull requests are
// immutable (Bitbucket Cloud), with the current opt-in hash optIn.
func memImmutable(own []OwnPR, optIn string) MemoryInput {
	in := memInput(own)
	in.OptIn = optIn
	in.Config.ClosedImmutable = true
	return in
}

// TestMemoryClosedImmutable: where a declined pull request can never be
// edited, the optin its marker held while open stands for the ack: the
// decline holds while the opt-in file hashes the same and lapses when it
// changes; one without optin holds whatever the file says and is
// unanchored; a forget_declines entry lifts a decline while it is present;
// a ticked repropose control counts for nothing; and memory never asks to
// write to a closed pull request.
func TestMemoryClosedImmutable(t *testing.T) {
	a := []Pair{memCreate(memA, memV1)}
	declined := memPR(t, 7, a, &memPerson, 0) // optin memH1, written while open
	bare := memPR(t, 7, a, &memPerson, 0, memNoOptIn)
	cases := []struct {
		name string
		in   MemoryInput
		// want is memNumbers of the memory; verdict is memVerdict's for a.
		want, verdict string
		acked         bool
	}{
		{
			name: "in force while the opt-in file is the same",
			in:   memImmutable([]OwnPR{declined}, memH1),
			want: "Declines [7] Lapsed [] ToAck [] ToRevoke [] Auto []", verdict: "declined", acked: true,
		},
		{
			name: "lapsed once the opt-in file changes",
			in:   memImmutable([]OwnPR{declined}, memH2),
			want: "Declines [] Lapsed [7] ToAck [] ToRevoke [] Auto []", verdict: "new PR",
		},
		{
			name: "in force when the current state is unknown",
			in:   memImmutable([]OwnPR{declined}, ""),
			want: "Declines [7] Lapsed [] ToAck [] ToRevoke [] Auto []", verdict: "declined", acked: true,
		},
		{
			name: "without optin: in force whatever the opt-in file says",
			in:   memImmutable([]OwnPR{bare}, memH2),
			want: "Declines [7] Lapsed [] ToAck [] ToRevoke [] Auto [] Forgotten [] Unanchored [7]", verdict: "declined",
		},
		{
			name: "without optin: lapsed when a path becomes local",
			in: func() MemoryInput {
				in := memImmutable([]OwnPR{bare}, memH1)
				in.LocalOrIgnored = func(p string) bool { return p == memA }
				return in
			}(),
			want: "Declines [] Lapsed [7] ToAck [] ToRevoke [] Auto []", verdict: "new PR",
		},
		{
			name: "a forget_declines entry lifts it while present",
			in: func() MemoryInput {
				in := memImmutable([]OwnPR{bare}, memH1)
				in.Forget = []int64{7}
				return in
			}(),
			want: "Declines [] Lapsed [] ToAck [] ToRevoke [] Auto [] Forgotten [7] Unanchored []", verdict: "new PR",
		},
		{
			name: "a ticked repropose control counts for nothing",
			in: func() MemoryInput {
				in := memImmutable([]OwnPR{declined}, memH1)
				in.Repropose = map[int64]bool{7: true}
				return in
			}(),
			want: "Declines [7] Lapsed [] ToAck [] ToRevoke [] Auto []", verdict: "declined", acked: true,
		},
		{
			name: "the writer's own close is no memory",
			in:   memImmutable([]OwnPR{memPR(t, 7, a, &memWriter, 0)}, memH1),
			want: "Declines [] Lapsed [] ToAck [] ToRevoke [] Auto []", verdict: "new PR",
		},
		{
			name: "touchmark's close is no memory",
			in:   memImmutable([]OwnPR{memPR(t, 7, a, &memWriter, 0, memSelfClosed)}, memH1),
			want: "Declines [] Lapsed [] ToAck [] ToRevoke [] Auto []", verdict: "new PR",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := BuildMemory(c.in)
			if got := memNumbers(m); got != c.want {
				t.Errorf("memory %s\nwant   %s", got, c.want)
			}
			if got, _ := memVerdict(m, a, c.in.Now); got != c.verdict {
				t.Errorf("verdict %q, want %q", got, c.verdict)
			}
			if len(m.Declines) == 1 && m.Declines[0].Acked != c.acked {
				t.Errorf("decline %+v, want acked %v", m.Declines[0], c.acked)
			}
			d := DecideTarget(TargetInput{Stream: StreamSync, D: a, Key: Key(StreamSync, a), B: memX, DefaultBranch: "main",
				Branch: Branch{Name: "touchmark/acme-eng"}, Own: c.in.Own, Memory: m, Now: c.in.Now})
			for _, s := range d.Steps {
				if s.PR != 0 && s.Kind != StepCreatePR {
					t.Errorf("a write to closed #%d: %s", s.PR, s.Kind)
				}
			}
		})
	}
}

// TestMemoryClosedImmutableSame: on a platform whose closed pull requests
// can be edited, the same marker of an unacked decline asks for the ack,
// whatever optin it holds, and a forget_declines entry revokes it: the
// platform decides, not the marker.
func TestMemoryClosedImmutableSame(t *testing.T) {
	a := []Pair{memCreate(memA, memV1)}
	in := memInput([]OwnPR{memPR(t, 7, a, &memPerson, 0)})
	in.OptIn = memH2
	if got, want := memNumbers(BuildMemory(in)), "Declines [7] Lapsed [] ToAck [7] ToRevoke [] Auto []"; got != want {
		t.Errorf("memory %s\nwant   %s", got, want)
	}
	in.Forget = []int64{7}
	if got, want := memNumbers(BuildMemory(in)), "Declines [] Lapsed [] ToAck [] ToRevoke [7] Auto []"; got != want {
		t.Errorf("forgotten: memory %s\nwant   %s", got, want)
	}
}

// TestMemoryClosedImmutableEscalation: auto-closes count as before from
// the closer and the marker: the first two defer the content, the third
// counts as a decline, which holds with the optin of its marker and
// lapses when the opt-in file changes; nothing is written to any of them.
func TestMemoryClosedImmutableEscalation(t *testing.T) {
	a := []Pair{memCreate(memA, memV1)}
	key := Key(StreamSync, a)
	var own []OwnPR
	for i, n := range []int64{9, 8, 7} {
		own = append(own, memPR(t, n, a, &memStale, 20-i))
	}
	m := BuildMemory(memImmutable(own[2:], memH1))
	if until, declined := m.Cooldown(key, memT0.Add(19*memDay), 0); until.IsZero() || declined {
		t.Errorf("one auto-close: until %v, declined %v", until, declined)
	}
	m = BuildMemory(memImmutable(own, memH1))
	if want := "Declines [9] Lapsed [] ToAck [] ToRevoke [] Auto [9 8 7]"; memNumbers(m) != want {
		t.Errorf("three auto-closes: %s\nwant %s", memNumbers(m), want)
	}
	if _, declined := m.Cooldown(key, memT0.Add(100*memDay), 0); !declined {
		t.Error("the third auto-close does not count as a decline")
	}
	d := DecideTarget(TargetInput{Stream: StreamSync, D: a, Key: key, B: memX, DefaultBranch: "main",
		Branch: Branch{Name: "touchmark/acme-eng"}, Own: own, Memory: m, CooldownDeclined: true, Now: memT0.Add(100 * memDay)})
	if d.Outcome != OutcomeDeclined || d.PR != 9 || len(d.Steps) != 0 {
		t.Errorf("decision %s #%d, steps %v", d.Outcome, d.PR, d.Steps)
	}
	m = BuildMemory(memImmutable(own, memH2))
	if want := "Declines [] Lapsed [9] ToAck [] ToRevoke [] Auto [9 8 7]"; memNumbers(m) != want {
		t.Errorf("after an opt-in change: %s\nwant %s", memNumbers(m), want)
	}
	if until, declined := m.Cooldown(key, memT0.Add(100*memDay), 0); !until.IsZero() || declined {
		t.Errorf("after an opt-in change: until %v, declined %v", until, declined)
	}
	// A forget_declines entry ends the cooldown of an auto-close it names,
	// while present.
	in := memImmutable(own[2:], memH1)
	in.Forget = []int64{7}
	m = BuildMemory(in)
	if len(m.Auto) != 0 || !slices.Equal(m.Forgotten, []int64{7}) {
		t.Errorf("a forgotten auto-close: %s", memNumbers(m))
	}
}
