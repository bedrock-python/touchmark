package decide

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Blob ids, opt-in hashes and times of the memory tests.
const (
	memFP  = "github.com/712345678"
	memV1  = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	memV2  = "2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"
	memX   = "3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"
	memH1  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	memH2  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	memDay = 24 * time.Hour
)

// Paths of the memory tests: A and B are the two files of the decline table
// in docs/concepts/memory.md, R one the hub retires.
const (
	memA = "AGENTS.md"
	memB = "prompts/review.md"
	memR = "prompts/old.md"
)

// The accounts that close pull requests in the memory tests.
var (
	memT0     = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	memPerson = platform.Account{ID: "5150", Login: "jdoe", Kind: platform.KindUser}
	memStale  = platform.Account{ID: "29110", Login: "stale[bot]", Kind: platform.KindBot}
	// memWriter is touchmark's writer: a bot on GitHub.
	memWriter = platform.Account{ID: "55501017", Login: "acme-assets-write[bot]", Kind: platform.KindBot}
	// memKnown is in known_authors: multi-gitter's group access token bot.
	memKnown = platform.Account{ID: "12345", Login: "group_42_bot", Kind: platform.KindServiceAccount}
	// memRunner is in automation_accounts: a cleanup job's user.
	memRunner = platform.Account{ID: "2001", Login: "cleanup-runner", Kind: platform.KindUser}
)

func memConfig() MemoryConfig {
	return MemoryConfig{
		Cooldown:    30 * memDay,
		Writers:     map[string]bool{memWriter.ID: true, memKnown.ID: true},
		Automation:  map[string]bool{memRunner.ID: true},
		CloserKnown: true,
		Window:      50,
	}
}

func memCreate(path, to string) Pair { return Pair{Path: path, From: ZeroOID, Mode: "100644", To: to} }
func memUpdate(path, from, to string) Pair {
	return Pair{Path: path, From: from, Mode: "100644", To: to}
}
func memDelete(path, from string) Pair {
	return Pair{Path: path, From: from, Mode: ModeDelete, To: ZeroOID}
}

// memPR returns own PR n of the sync stream carrying d, closed without
// merge by `by` (nil: the platform did not say) on day `day` after memT0,
// with the marker data edited by edits. The marker goes through Encode and
// Parse, as it does through a PR body.
func memPR(t testing.TB, n int64, d []Pair, by *platform.Account, day int, edits ...func(*marker.Data)) OwnPR {
	t.Helper()
	data := marker.Data{
		V: marker.Version, Stream: StreamSync, Hub: "acme-eng", FP: memFP,
		DecidedAt: memX, ContentCommit: memX, Base: memX, OptIn: memH1, Engine: "0.2.0",
		Packs: []string{"agents"}, Changes: ShortChanges(d), ChangesComplete: true,
		TitleSet: "chore: sync engineering assets", Body: BodyHash("body"), LabelsSet: []string{"engineering-assets"},
	}
	for _, e := range edits {
		e(&data)
	}
	line, err := marker.Encode(marker.Marker{Key: Key(StreamSync, d), Data: data})
	if err != nil {
		t.Fatalf("encode the marker of #%d: %v", n, err)
	}
	m, err := marker.Parse(line)
	if err != nil {
		t.Fatalf("parse the marker of #%d: %v", n, err)
	}
	pr := platform.PR{
		Number: n, State: platform.Closed, Head: "touchmark/acme-eng", Base: "main",
		RepoID: "100", HeadRepoID: "100", BaseExists: true,
		Author: memWriter, Body: "body\n\n" + line,
		CreatedAt: memT0.Add(time.Duration(day)*memDay - time.Hour),
		ClosedAt:  memT0.Add(time.Duration(day) * memDay),
	}
	if by != nil {
		c := *by
		pr.ClosedBy = &c
	}
	return OwnPR{PR: pr, Marker: m}
}

// Marker edits.
func memAcked(optIn string) func(*marker.Data) {
	return func(d *marker.Data) { d.Ack, d.OptIn = true, optIn }
}
func memRevoked(d *marker.Data)    { d.Revoked = true }
func memIncomplete(d *marker.Data) { d.Changes, d.ChangesComplete = nil, false }
func memSelfClosed(d *marker.Data) { d.Closed = &marker.Closed{By: "touchmark", Reason: ReasonNoDiff} }
func memNoOptIn(d *marker.Data)    { d.OptIn = "" }

// memState returns o in another state: open or merged.
func memState(o OwnPR, s platform.PRState) OwnPR {
	o.PR.State, o.PR.ClosedBy = s, nil
	if s == platform.Open {
		o.PR.ClosedAt = time.Time{}
	}
	return o
}

// memInput returns the input of BuildMemory for own, with the current
// opt-in hash memH1, the paths in local local or ignored, and the default
// configuration.
func memInput(own []OwnPR, local ...string) MemoryInput {
	return MemoryInput{
		Own:            own,
		OptIn:          memH1,
		LocalOrIgnored: func(p string) bool { return slices.Contains(local, p) },
		Now:            memT0.Add(100 * memDay),
		Config:         memConfig(),
	}
}

// memVerdict is what DecideTarget's rules 3b and 3c make of memory for d
// when no own PR is open: "declined" with the covering PRs, "deferred", or
// "new PR" with the PRs of the "previously declined" block.
func memVerdict(m Memory, d []Pair, now time.Time) (string, []int64) {
	key := Key(StreamSync, d)
	if ok, prs := m.IsDeclined(d, key); ok {
		return "declined", prs
	}
	until, declined := m.Cooldown(key, now, 30*memDay)
	switch {
	case declined:
		return "declined", nil
	case !until.IsZero():
		return "deferred", nil
	}
	return "new PR", m.Overlap(d)
}

// memOptInHash parses an opt-in file and returns its hash.
func memOptInHash(t *testing.T, text string) string {
	t.Helper()
	o, _, err := config.ParseOptIn([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return o.Hash()
}

func TestMemoryClassifyClose(t *testing.T) {
	cases := []struct {
		name  string
		by    *platform.Account
		edit  func(*OwnPR)
		known bool // Caps.CloserKnown
		want  CloseClass
	}{
		{"closed by touchmark", &memWriter, func(o *OwnPR) { memSelfClosed(&o.Marker.Data) }, true, CloseSelf},
		{"closed by touchmark, a person named", &memPerson, func(o *OwnPR) { memSelfClosed(&o.Marker.Data) }, true, CloseSelf},
		{"closed by touchmark, base gone", &memPerson, func(o *OwnPR) { memSelfClosed(&o.Marker.Data); o.PR.BaseExists = false }, true, CloseSelf},
		{"closed by touchmark, closer unknown", nil, func(o *OwnPR) { memSelfClosed(&o.Marker.Data) }, false, CloseSelf},
		{"closed.by is not touchmark", &memPerson, func(o *OwnPR) { o.Marker.Data.Closed = &marker.Closed{By: "jdoe", Reason: "no-diff"} }, true, CloseDecline},
		{"base gone, a person named", &memPerson, func(o *OwnPR) { o.PR.BaseExists = false }, true, CloseAuto},
		{"base gone, the writer named", &memWriter, func(o *OwnPR) { o.PR.BaseExists = false }, true, CloseAuto},
		{"base gone, closer unknown", nil, func(o *OwnPR) { o.PR.BaseExists = false }, false, CloseAuto},
		{"the writer (a bot)", &memWriter, nil, true, CloseSelf},
		{"a known author", &memKnown, nil, true, CloseSelf},
		{"a bot", &memStale, nil, true, CloseAuto},
		{"an automation account", &memRunner, nil, true, CloseAuto},
		{"a service account not listed", &platform.Account{ID: "808", Login: "svc", Kind: platform.KindServiceAccount}, nil, true, CloseDecline},
		{"an account of unknown kind", &platform.Account{ID: "809", Login: "who"}, nil, true, CloseDecline},
		{"a person", &memPerson, nil, true, CloseDecline},
		{"a person with a bot-like login", &platform.Account{ID: "810", Login: "renovate[bot]", Kind: platform.KindUser}, nil, true, CloseDecline},
		{"closer not reported", nil, nil, true, CloseDecline},
		{"closer without an id", &platform.Account{Login: "ghost"}, nil, true, CloseDecline},
		{"closer unreliable, the writer named", &memWriter, nil, false, CloseDecline},
		{"closer unreliable, a bot named", &memStale, nil, false, CloseDecline},
		{"closer unreliable, nobody named", nil, nil, false, CloseDecline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := memPR(t, 7, []Pair{memCreate(memA, memV1)}, c.by, 0)
			if c.edit != nil {
				c.edit(&o)
			}
			cfg := memConfig()
			cfg.CloserKnown = c.known
			if got := ClassifyClose(o, cfg); got != c.want {
				t.Errorf("ClassifyClose = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMemoryClassifyClosePanics(t *testing.T) {
	for _, state := range []platform.PRState{platform.Open, platform.Merged, ""} {
		t.Run(string(state), func(t *testing.T) {
			defer func() {
				r := recover()
				if msg, _ := r.(string); !strings.Contains(msg, "not closed") {
					t.Errorf("panic = %v, want one saying the PR is not closed", r)
				}
			}()
			o := memPR(t, 7, []Pair{memCreate(memA, memV1)}, &memPerson, 0)
			o.PR.State = state
			ClassifyClose(o, memConfig())
		})
	}
}

func TestMemoryCloseClassString(t *testing.T) {
	for c, want := range map[CloseClass]string{CloseSelf: "self", CloseAuto: "auto", CloseDecline: "decline", 0: "unknown", 9: "unknown"} {
		if got := c.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", c, got, want)
		}
	}
}

// TestMemoryDeclineTable runs the decline table of docs/concepts/memory.md,
// with rows for a deletion and a merge besides.
func TestMemoryDeclineTable(t *testing.T) {
	a1, b1 := memCreate(memA, memV1), memCreate(memB, memV1)
	comment := struct{ before, after string }{
		before: "version: 1\npacks: [agents]\nignore: [docs/**]\n",
		after:  "# we keep docs ourselves\nversion: 1\npacks:\n  - agents # the base\nignore: ['docs/**']\n",
	}
	cases := []struct {
		name  string
		own   func(t *testing.T) []OwnPR
		optIn string // current opt-in hash; memH1 when empty
		local []string
		d     []Pair
		now   time.Time // memT0 + 100 days when zero
		want  string
		prs   []int64
	}{
		{
			name: "#7 {A, B}, nothing since",
			own:  func(t *testing.T) []OwnPR { return []OwnPR{memPR(t, 7, []Pair{a1, b1}, &memPerson, 0)} },
			d:    []Pair{a1, b1}, want: "declined", prs: []int64{7},
		},
		{
			name:  "#7 {A, B}, B edited: local",
			own:   func(t *testing.T) []OwnPR { return []OwnPR{memPR(t, 7, []Pair{a1, b1}, &memPerson, 0)} },
			local: []string{memB},
			d:     []Pair{a1}, want: "new PR",
		},
		{
			name: "#7 {A, B}, B in ignore",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{memPR(t, 7, []Pair{a1, b1}, &memPerson, 0, memAcked(memH1))}
			},
			optIn: memH2, local: []string{memB},
			d: []Pair{a1}, want: "new PR",
		},
		{
			name: "#7 {A}, a comment in the opt-in file edited after the ack",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{memPR(t, 7, []Pair{a1}, &memPerson, 0, memAcked(memOptInHash(t, comment.before)))}
			},
			optIn: "after", // memOptInHash(comment.after), below
			d:     []Pair{a1}, want: "declined", prs: []int64{7},
		},
		{
			name: "#7 {A}, #9 {B}, nothing since",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{memPR(t, 9, []Pair{b1}, &memPerson, 5), memPR(t, 7, []Pair{a1}, &memPerson, 0)}
			},
			d: []Pair{a1, b1}, want: "declined", prs: []int64{9, 7},
		},
		{
			name: "#7 {A, B}, the pack changes B to 2",
			own:  func(t *testing.T) []OwnPR { return []OwnPR{memPR(t, 7, []Pair{a1, b1}, &memPerson, 0)} },
			d:    []Pair{a1, memCreate(memB, memV2)}, want: "new PR", prs: []int64{7},
		},
		{
			name: "#7 {A: none→1}, {A: none→2} merged, the hub rolls A back to 1",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{
					memState(memPR(t, 8, []Pair{memCreate(memA, memV2)}, nil, 10), platform.Merged),
					memPR(t, 7, []Pair{a1}, &memPerson, 0),
				}
			},
			d: []Pair{memUpdate(memA, memV2, memV1)}, want: "new PR",
		},
		{
			name: "#7 {R: 1→delete}, {R: 1→2} merged, the hub removes R again",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{
					memState(memPR(t, 8, []Pair{memUpdate(memR, memV1, memV2)}, nil, 10), platform.Merged),
					memPR(t, 7, []Pair{memDelete(memR, memV1)}, &memPerson, 0),
				}
			},
			d: []Pair{memDelete(memR, memV2)}, want: "new PR",
		},
		{
			name: "#7 {A}, the hub id changed, the old branch in branch_aliases",
			own: func(t *testing.T) []OwnPR {
				o := memPR(t, 7, []Pair{a1}, &memPerson, 0, func(d *marker.Data) { d.Hub = "old-id" })
				o.PR.Head, o.Alias = "touchmark/old-id", true
				return []OwnPR{o}
			},
			d: []Pair{a1}, want: "declined", prs: []int64{7},
		},
		{
			name: "#7 closed by the stale bot 10 days ago",
			own:  func(t *testing.T) []OwnPR { return []OwnPR{memPR(t, 7, []Pair{a1}, &memStale, 0)} },
			d:    []Pair{a1}, now: memT0.Add(10 * memDay), want: "deferred",
		},
		{
			name: "#7 closed by the stale bot, 30 days passed",
			own:  func(t *testing.T) []OwnPR { return []OwnPR{memPR(t, 7, []Pair{a1}, &memStale, 0)} },
			d:    []Pair{a1}, now: memT0.Add(31 * memDay), want: "new PR",
		},
		{
			name: "the third auto-close in a row",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{
					memPR(t, 11, []Pair{a1}, &memStale, 75),
					memPR(t, 9, []Pair{a1}, &memStale, 45),
					memPR(t, 7, []Pair{a1}, &memStale, 0),
				}
			},
			d: []Pair{a1}, want: "declined", prs: []int64{11},
		},
		{
			name: "merged, the team reverted the file",
			own: func(t *testing.T) []OwnPR {
				return []OwnPR{memState(memPR(t, 7, []Pair{a1}, nil, 0), platform.Merged)}
			},
			d: []Pair{memUpdate(memA, memX, memV1)}, want: "new PR",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := memInput(c.own(t), c.local...)
			switch c.optIn {
			case "":
			case "after":
				in.OptIn = memOptInHash(t, comment.after)
				if in.OptIn != memOptInHash(t, comment.before) {
					t.Fatal("editing a comment changed the opt-in hash")
				}
			default:
				in.OptIn = c.optIn
			}
			now := c.now
			if now.IsZero() {
				now = memT0.Add(100 * memDay)
			}
			got, prs := memVerdict(BuildMemory(in), c.d, now)
			if got != c.want || !slices.Equal(prs, c.prs) {
				t.Errorf("verdict = %s %v, want %s %v", got, prs, c.want, c.prs)
			}
		})
	}
}

// TestMemoryInForce checks when a decline holds and what BuildMemory records
// about it.
func TestMemoryInForce(t *testing.T) {
	a1, b1 := memCreate(memA, memV1), memCreate(memB, memV1)
	cases := []struct {
		name     string
		edits    []func(*marker.Data)
		optIn    string
		local    []string
		nilLocal bool
		want     Memory
	}{
		{
			// The marker's optin was written with the content; before the
			// ack the reference is the current hash, whatever that is.
			name:  "unacked, the opt-in file changed since the content",
			optIn: memH2,
			want:  Memory{Declines: []Decline{{PR: 7, Acked: false}}, ToAck: []int64{7}},
		},
		{
			name:  "acked, same opt-in",
			edits: []func(*marker.Data){memAcked(memH1)},
			optIn: memH1,
			want:  Memory{Declines: []Decline{{PR: 7, Acked: true, OptIn: memH1}}},
		},
		{
			name:  "acked, opt-in changed since",
			edits: []func(*marker.Data){memAcked(memH1)},
			optIn: memH2,
			want:  Memory{Lapsed: []int64{7}},
		},
		{
			name:  "acked, current hash unknown",
			edits: []func(*marker.Data){memAcked(memH1)},
			optIn: "",
			want:  Memory{Declines: []Decline{{PR: 7, Acked: true, OptIn: memH1}}},
		},
		{
			name:  "a path of it local",
			optIn: memH1, local: []string{memB},
			want: Memory{Lapsed: []int64{7}},
		},
		{
			name:  "acked, a path of it ignored",
			edits: []func(*marker.Data){memAcked(memH1)},
			optIn: memH1, local: []string{memA},
			want: Memory{Lapsed: []int64{7}},
		},
		{
			name:  "another path local",
			optIn: memH1, local: []string{"README.md"},
			want: Memory{Declines: []Decline{{PR: 7}}, ToAck: []int64{7}},
		},
		{
			name:  "no LocalOrIgnored",
			optIn: memH1, nilLocal: true,
			want: Memory{Declines: []Decline{{PR: 7}}, ToAck: []int64{7}},
		},
		{
			// Without changes the paths are unknown: only the exact key
			// matches, and a local path would have changed the key.
			name:  "incomplete changes, a path local",
			edits: []func(*marker.Data){memIncomplete},
			optIn: memH1, local: []string{memB},
			want: Memory{Declines: []Decline{{PR: 7}}, ToAck: []int64{7}},
		},
		{
			name:  "revoked",
			edits: []func(*marker.Data){memAcked(memH1), memRevoked},
			optIn: memH1,
			want:  Memory{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := memPR(t, 7, []Pair{a1, b1}, &memPerson, 3, c.edits...)
			in := memInput([]OwnPR{o}, c.local...)
			in.OptIn = c.optIn
			if c.nilLocal {
				in.LocalOrIgnored = nil
			}
			// Fill what every expected decline shares.
			for i := range c.want.Declines {
				d := &c.want.Declines[i]
				d.Key, d.Changes, d.Complete, d.ClosedAt = o.Marker.Key, o.Marker.Data.Changes, o.Marker.Data.ChangesComplete, o.PR.ClosedAt
			}
			if got := BuildMemory(in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("BuildMemory =\n%+v\nwant\n%+v", got, c.want)
			}
		})
	}
}

func TestMemoryDeclineFields(t *testing.T) {
	d := []Pair{memCreate(memB, memV1), memDelete(memR, memV2), memUpdate(memA, memX, memV1)}
	o := memPR(t, 7, d, &memPerson, 4, memAcked(memH1))
	m := BuildMemory(memInput([]OwnPR{o}))
	want := []Decline{{
		PR:  7,
		Key: Key(StreamSync, d),
		Changes: []marker.Change{
			{Path: memA, From: memX[:16], Mode: "100644", To: memV1[:16]},
			{Path: memR, From: memV2[:16], Mode: "", To: ""},
			{Path: memB, From: "", Mode: "100644", To: memV1[:16]},
		},
		Complete: true,
		Acked:    true,
		OptIn:    memH1,
		ClosedAt: memT0.Add(4 * memDay),
	}}
	if !reflect.DeepEqual(m.Declines, want) {
		t.Errorf("Declines =\n%+v\nwant\n%+v", m.Declines, want)
	}
	// The decline owns its changes: editing them leaves the marker alone.
	m.Declines[0].Changes[0].Path = "changed"
	if o.Marker.Data.Changes[0].Path != memA {
		t.Error("the decline shares its changes with the marker")
	}
}

// TestMemoryWindow: only the 50 newest closed own PRs count; open and
// merged ones are not part of the window.
func TestMemoryWindow(t *testing.T) {
	a1 := []Pair{memCreate(memA, memV1)}
	// own returns closed self-closes numbered from 500 down, the open and
	// merged PRs between them, and a person's decline #1 as the oldest.
	own := func(t *testing.T, selfCloses int) []OwnPR {
		var out []OwnPR
		for i := range selfCloses {
			n := int64(500 - 3*i)
			out = append(out,
				memState(memPR(t, n, a1, nil, 90), platform.Open),
				memPR(t, n-1, a1, &memWriter, 80),
				memState(memPR(t, n-2, a1, nil, 70), platform.Merged))
		}
		return append(out, memPR(t, 1, a1, &memPerson, 0))
	}
	for _, c := range []struct {
		name       string
		selfCloses int
		window     int
		declined   bool
	}{
		{"the decline is the 50th closed PR", 49, 50, true},
		{"the decline is the 51st closed PR", 50, 50, false},
		{"a window of 0 means 50", 49, 0, true},
		{"a window of 0 means 50, the 51st", 50, 0, false},
		{"a window of 3", 2, 3, true},
		{"a window of 3, the 4th", 3, 3, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := memInput(own(t, c.selfCloses))
			in.Config.Window = c.window
			got, _ := BuildMemory(in).IsDeclined(a1, Key(StreamSync, a1))
			if got != c.declined {
				t.Errorf("declined = %v, want %v", got, c.declined)
			}
		})
	}
	// A PR listed twice counts once.
	in := memInput([]OwnPR{memPR(t, 9, a1, &memWriter, 5), memPR(t, 9, a1, &memWriter, 5), memPR(t, 1, a1, &memPerson, 0)})
	in.Config.Window = 2
	if got, _ := BuildMemory(in).IsDeclined(a1, Key(StreamSync, a1)); !got {
		t.Error("a PR listed twice took two places in the window")
	}
}

// TestMemoryUpkeep covers acks and revocations.
func TestMemoryUpkeep(t *testing.T) {
	a1, b1 := memCreate(memA, memV1), memCreate(memB, memV1)
	own := func(t *testing.T) []OwnPR {
		return []OwnPR{
			memState(memPR(t, 15, []Pair{a1}, nil, 60), platform.Open),
			memPR(t, 13, []Pair{a1}, &memStale, 50),                             // an auto-close
			memPR(t, 12, []Pair{a1}, &memWriter, 40),                            // a self-close
			memPR(t, 11, []Pair{b1}, &memPerson, 30, memAcked(memH1)),           // acked
			memPR(t, 10, []Pair{a1, b1}, &memPerson, 20),                        // unacked
			memPR(t, 9, []Pair{b1}, &memPerson, 10, memAcked(memH2)),            // lapsed
			memPR(t, 8, []Pair{a1}, &memPerson, 5, memAcked(memH1), memRevoked), // revoked
			memState(memPR(t, 7, []Pair{a1}, nil, 1), platform.Merged),
		}
	}
	base := Memory{
		Declines: []Decline{{PR: 11}, {PR: 10}},
		Lapsed:   []int64{9},
		ToAck:    []int64{10},
		Auto:     []AutoClose{{PR: 13}},
	}
	cases := []struct {
		name      string
		forget    []int64
		repropose map[int64]bool
		want      func(Memory) Memory
	}{
		{"nothing to do", nil, nil, func(m Memory) Memory { return m }},
		{"forget an acked decline", []int64{11}, nil, func(m Memory) Memory {
			m.Declines, m.ToRevoke = m.Declines[1:], []int64{11}
			return m
		}},
		{"repropose ticked on an unacked decline", nil, map[int64]bool{10: true}, func(m Memory) Memory {
			m.Declines, m.ToAck, m.ToRevoke = m.Declines[:1], nil, []int64{10}
			return m
		}},
		{"forget a lapsed decline: the revocation makes it permanent", []int64{9}, nil, func(m Memory) Memory {
			m.Lapsed, m.ToRevoke = nil, []int64{9}
			return m
		}},
		{"forget an auto-close: its cooldown ends", []int64{13}, nil, func(m Memory) Memory {
			m.Auto, m.ToRevoke = nil, []int64{13}
			return m
		}},
		{"forget what holds no memory", []int64{15, 12, 8, 7, 99}, map[int64]bool{12: true, 8: true}, func(m Memory) Memory { return m }},
		{"forget and repropose together, in window order", []int64{9, 13}, map[int64]bool{11: true, 10: false}, func(m Memory) Memory {
			m.Declines, m.Lapsed, m.Auto, m.ToRevoke = m.Declines[1:], nil, nil, []int64{13, 11, 9}
			return m
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prs := own(t)
			in := memInput(prs)
			in.Forget, in.Repropose = c.forget, c.repropose
			got := BuildMemory(in)
			want := c.want(memClone(base))
			if !memSameNumbers(got, want) {
				t.Errorf("BuildMemory =\n%s\nwant\n%s", memNumbers(got), memNumbers(want))
			}
		})
	}
}

// TestMemoryReproposeBypassesOnce: a ticked repropose revokes that decline
// alone. Content another decline in force still covers stays declined; the
// rest opens a new PR that names the other decline.
func TestMemoryReproposeBypassesOnce(t *testing.T) {
	a1, b1 := memCreate(memA, memV1), memCreate(memB, memV1)
	own := []OwnPR{memPR(t, 9, []Pair{b1}, &memPerson, 5, memAcked(memH1)), memPR(t, 7, []Pair{a1}, &memPerson, 0, memAcked(memH1))}
	in := memInput(own)
	in.Repropose = map[int64]bool{7: true}
	m := BuildMemory(in)
	if !slices.Equal(m.ToRevoke, []int64{7}) {
		t.Fatalf("ToRevoke = %v, want [7]", m.ToRevoke)
	}
	if got, prs := memVerdict(m, []Pair{a1, b1}, in.Now); got != "new PR" || !slices.Equal(prs, []int64{9}) {
		t.Errorf("D {A, B}: %s %v, want a new PR naming #9", got, prs)
	}
	if got, prs := memVerdict(m, []Pair{b1}, in.Now); got != "declined" || !slices.Equal(prs, []int64{9}) {
		t.Errorf("D {B}: %s %v, want declined by #9", got, prs)
	}
	// The next run reads the revocation from the marker and decides alike.
	own[1] = memPR(t, 7, []Pair{a1}, &memPerson, 0, memAcked(memH1), memRevoked)
	next := BuildMemory(memInput(own))
	if len(next.ToRevoke) != 0 || !reflect.DeepEqual(next.Declines, m.Declines) {
		t.Errorf("after the revocation: %s, want the declines of the run that wrote it", memNumbers(next))
	}
}

// TestMemoryEscalation: auto-closes in a row with one key escalate: the
// first defers, the second defers twice as long, the third is a decline,
// with an ack and a comment like any other.
func TestMemoryEscalation(t *testing.T) {
	a1, b1 := memCreate(memA, memV1), memCreate(memB, memV1)
	keyA := Key(StreamSync, []Pair{a1})
	t.Run("the first and the second defer", func(t *testing.T) {
		one := BuildMemory(memInput([]OwnPR{memPR(t, 7, []Pair{a1}, &memStale, 0)}))
		if until, declined := one.Cooldown(keyA, memT0.Add(29*memDay), 0); declined || !until.Equal(memT0.Add(30*memDay)) {
			t.Errorf("one auto-close: %v %v, want deferred until day 30", until, declined)
		}
		two := BuildMemory(memInput([]OwnPR{memPR(t, 9, []Pair{a1}, &memStale, 40), memPR(t, 7, []Pair{a1}, &memStale, 0)}))
		if until, declined := two.Cooldown(keyA, memT0.Add(41*memDay), 0); declined || !until.Equal(memT0.Add(100*memDay)) {
			t.Errorf("two auto-closes: %v %v, want deferred until day 40 + 60", until, declined)
		}
		if len(one.Declines)+len(two.Declines)+len(one.ToAck)+len(two.ToAck) != 0 {
			t.Error("an auto-close counted as a decline before the third")
		}
	})
	t.Run("the third is a decline", func(t *testing.T) {
		m := BuildMemory(memInput([]OwnPR{
			memPR(t, 11, []Pair{a1}, &memStale, 80),
			memPR(t, 9, []Pair{a1}, &memRunner, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		}))
		if want := "Declines [11] Lapsed [] ToAck [11] ToRevoke [] Auto [11 9 7]"; memNumbers(m) != want {
			t.Errorf("memory %s, want %s", memNumbers(m), want)
		}
		if until, declined := m.Cooldown(keyA, memT0.Add(81*memDay), 0); !declined || !until.IsZero() {
			t.Errorf("Cooldown = %v %v, want declined", until, declined)
		}
	})
	t.Run("another key breaks the run", func(t *testing.T) {
		m := BuildMemory(memInput([]OwnPR{
			memPR(t, 11, []Pair{a1}, &memStale, 80),
			memPR(t, 10, []Pair{b1}, &memStale, 60),
			memPR(t, 9, []Pair{a1}, &memStale, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		}))
		if len(m.Declines) != 0 {
			t.Errorf("Declines = %+v, want none", m.Declines)
		}
		if until, declined := m.Cooldown(keyA, memT0.Add(81*memDay), 0); declined || !until.Equal(memT0.Add(110*memDay)) {
			t.Errorf("Cooldown = %v %v, want deferred until day 80 + 30", until, declined)
		}
	})
	t.Run("a decline or a self-close between does not break it", func(t *testing.T) {
		m := BuildMemory(memInput([]OwnPR{
			memPR(t, 12, []Pair{a1}, &memStale, 80),
			memPR(t, 11, []Pair{b1}, &memPerson, 70, memAcked(memH1)),
			memPR(t, 10, []Pair{a1}, &memWriter, 60),
			memPR(t, 9, []Pair{a1}, &memStale, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		}))
		if want := "Declines [12 11] Lapsed [] ToAck [12] ToRevoke [] Auto [12 9 7]"; memNumbers(m) != want {
			t.Errorf("memory %s, want %s", memNumbers(m), want)
		}
	})
	t.Run("a repropose buys one more proposal", func(t *testing.T) {
		// #11 was the third and acked; the team ticked repropose, touchmark
		// revoked it and opened #13, which the bot closed as well.
		m := BuildMemory(memInput([]OwnPR{
			memPR(t, 13, []Pair{a1}, &memStale, 120),
			memPR(t, 11, []Pair{a1}, &memStale, 80, memAcked(memH1), memRevoked),
			memPR(t, 9, []Pair{a1}, &memStale, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		}))
		if want := "Declines [13] Lapsed [] ToAck [13] ToRevoke [] Auto [13 9 7]"; memNumbers(m) != want {
			t.Errorf("memory %s, want %s", memNumbers(m), want)
		}
	})
	t.Run("repropose ticked on the third: proposed again now", func(t *testing.T) {
		in := memInput([]OwnPR{
			memPR(t, 11, []Pair{a1}, &memStale, 80, memAcked(memH1)),
			memPR(t, 9, []Pair{a1}, &memStale, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		})
		in.Repropose = map[int64]bool{11: true}
		m := BuildMemory(in)
		if got, _ := memVerdict(m, []Pair{a1}, memT0.Add(101*memDay)); got != "new PR" {
			t.Errorf("verdict %s, want a new PR", got)
		}
		if !slices.Equal(m.ToRevoke, []int64{11}) {
			t.Errorf("ToRevoke = %v, want [11]", m.ToRevoke)
		}
	})
	t.Run("the escalated decline lapses with the opt-in file", func(t *testing.T) {
		in := memInput([]OwnPR{
			memPR(t, 11, []Pair{a1}, &memStale, 80, memAcked(memH1)),
			memPR(t, 9, []Pair{a1}, &memStale, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		})
		in.OptIn = memH2
		m := BuildMemory(in)
		if want := "Declines [] Lapsed [11] ToAck [] ToRevoke [] Auto [11 9 7]"; memNumbers(m) != want {
			t.Errorf("memory %s, want %s", memNumbers(m), want)
		}
		if got, _ := memVerdict(m, []Pair{a1}, memT0.Add(81*memDay)); got != "new PR" {
			t.Errorf("verdict %s, want a new PR", got)
		}
	})
	t.Run("an acked auto-close stays a decline", func(t *testing.T) {
		// Its older siblings left the window; the ack remembers.
		in := memInput([]OwnPR{
			memPR(t, 11, []Pair{a1}, &memStale, 80, memAcked(memH1)),
			memPR(t, 9, []Pair{a1}, &memStale, 40),
			memPR(t, 7, []Pair{a1}, &memStale, 0),
		})
		in.Config.Window = 1
		m := BuildMemory(in)
		if want := "Declines [11] Lapsed [] ToAck [] ToRevoke [] Auto [11]"; memNumbers(m) != want {
			t.Errorf("memory %s, want %s", memNumbers(m), want)
		}
		if _, declined := m.Cooldown(keyA, memT0.Add(81*memDay), 0); !declined {
			t.Error("Cooldown does not count it as declined")
		}
	})
	t.Run("a person's acked decline whose base was deleted since", func(t *testing.T) {
		o := memPR(t, 7, []Pair{a1}, &memPerson, 0, memAcked(memH1))
		o.PR.BaseExists = false
		m := BuildMemory(memInput([]OwnPR{o}))
		if got, prs := memVerdict(m, []Pair{a1}, memT0.Add(100*memDay)); got != "declined" || !slices.Equal(prs, []int64{7}) {
			t.Errorf("verdict %s %v, want declined by #7", got, prs)
		}
	})
	t.Run("acked while the closer was unknown, then the platform names a bot", func(t *testing.T) {
		o := memPR(t, 7, []Pair{a1}, &memStale, 0, memAcked(memH1))
		cfg := memConfig()
		cfg.CloserKnown = false
		if ClassifyClose(o, cfg) != CloseDecline {
			t.Fatal("not a decline while the closer is unknown")
		}
		m := BuildMemory(memInput([]OwnPR{o}))
		if got, _ := memVerdict(m, []Pair{a1}, memT0.Add(100*memDay)); got != "declined" {
			t.Errorf("verdict %s, want declined", got)
		}
	})
	t.Run("an auto-close with incomplete changes escalates by key", func(t *testing.T) {
		m := BuildMemory(memInput([]OwnPR{
			memPR(t, 11, []Pair{a1}, &memStale, 80, memIncomplete),
			memPR(t, 9, []Pair{a1}, &memStale, 40, memIncomplete),
			memPR(t, 7, []Pair{a1}, &memStale, 0, memIncomplete),
		}))
		if ok, prs := m.IsDeclined([]Pair{a1}, keyA); !ok || !slices.Equal(prs, []int64{11}) {
			t.Errorf("IsDeclined = %v %v, want declined by #11", ok, prs)
		}
	})
}

func TestMemoryIsDeclined(t *testing.T) {
	a1, b1, r1 := memCreate(memA, memV1), memCreate(memB, memV1), memDelete(memR, memV1)
	x := Pair{Path: "bin/tool", From: memX, Mode: "100755", To: memX}
	dec := func(pr int64, complete bool, pairs ...Pair) Decline {
		d := Decline{PR: pr, Key: Key(StreamSync, pairs), Complete: complete}
		if complete {
			d.Changes = ShortChanges(pairs)
		}
		return d
	}
	cases := []struct {
		name     string
		declines []Decline
		d        []Pair
		key      string // Key(StreamSync, d) when empty
		want     bool
		prs      []int64
	}{
		{"no declines", nil, []Pair{a1}, "", false, nil},
		{"empty D", []Decline{dec(7, true, a1)}, nil, "", false, nil},
		{"equal", []Decline{dec(7, true, a1, b1)}, []Pair{a1, b1}, "", true, []int64{7}},
		{"a subset", []Decline{dec(7, true, a1, b1)}, []Pair{b1}, "", true, []int64{7}},
		{"a superset", []Decline{dec(7, true, a1)}, []Pair{a1, b1}, "", false, nil},
		{"the union of two", []Decline{dec(9, true, b1), dec(7, true, a1)}, []Pair{a1, b1}, "", true, []int64{9, 7}},
		{"every decline holding a pair is listed", []Decline{dec(9, true, a1, b1), dec(8, true, r1), dec(7, true, a1)}, []Pair{a1}, "", true, []int64{9, 7}},
		{"another from", []Decline{dec(7, true, a1)}, []Pair{memUpdate(memA, memV2, memV1)}, "", false, nil},
		{"another to", []Decline{dec(7, true, a1)}, []Pair{memCreate(memA, memV2)}, "", false, nil},
		{"another mode", []Decline{dec(7, true, a1)}, []Pair{{Path: memA, From: ZeroOID, Mode: "100755", To: memV1}}, "", false, nil},
		{"a deletion", []Decline{dec(7, true, r1, a1)}, []Pair{r1}, "", true, []int64{7}},
		{"a chmod", []Decline{dec(7, true, x)}, []Pair{x}, "", true, []int64{7}},
		{"incomplete, the same key", []Decline{dec(7, false, a1, b1)}, []Pair{a1, b1}, "", true, []int64{7}},
		{"incomplete, a subset", []Decline{dec(7, false, a1, b1)}, []Pair{a1}, "", false, nil},
		{"incomplete, key given as empty", []Decline{{PR: 7}}, []Pair{a1}, "-", false, nil},
		{"incomplete and complete", []Decline{dec(9, true, a1), dec(7, false, a1, b1)}, []Pair{a1, b1}, "", true, []int64{7}},
		{"covered both ways", []Decline{dec(9, true, a1, b1), dec(7, false, a1, b1)}, []Pair{a1, b1}, "", true, []int64{9, 7}},
		{
			"the listed changes of an incomplete decline count",
			[]Decline{{PR: 9, Key: "sha256:other", Changes: ShortChanges([]Pair{b1})}, dec(7, true, a1)},
			[]Pair{a1, b1}, "", true, []int64{9, 7},
		},
		{
			// A complete decline counts by its changes; its key alone does not.
			"complete, the same key, changes edited",
			[]Decline{{PR: 7, Key: Key(StreamSync, []Pair{a1, b1}), Complete: true, Changes: ShortChanges([]Pair{a1})}},
			[]Pair{a1, b1}, "", false, nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := c.key
			switch key {
			case "":
				if len(c.d) > 0 {
					key = Key(StreamSync, c.d)
				}
			case "-":
				key = ""
			}
			got, prs := Memory{Declines: c.declines}.IsDeclined(c.d, key)
			if got != c.want || !slices.Equal(prs, c.prs) {
				t.Errorf("IsDeclined = %v %v, want %v %v", got, prs, c.want, c.prs)
			}
		})
	}
}

// TestMemoryShortForm: pairs and changes compare by the first 16 hex digits
// of their blob ids, whatever the object format.
func TestMemoryShortForm(t *testing.T) {
	sha256a := strings.Repeat("ab", 32)
	sha256b := strings.Repeat("cd", 32)
	d := []Pair{memUpdate(memA, sha256a, sha256b), memDelete(memR, sha256b)}
	m := Memory{Declines: []Decline{{
		PR: 7, Complete: true,
		Changes: []marker.Change{
			{Path: memA, From: sha256a[:16], Mode: "100644", To: sha256b[:16]},
			{Path: memR, From: sha256b[:16]},
		},
	}}}
	if ok, prs := m.IsDeclined(d, Key(StreamSync, d)); !ok || !slices.Equal(prs, []int64{7}) {
		t.Errorf("IsDeclined = %v %v, want declined by #7", ok, prs)
	}
	if got := m.Overlap(d[:1]); !slices.Equal(got, []int64{7}) {
		t.Errorf("Overlap = %v, want [7]", got)
	}
}

func TestMemoryOverlap(t *testing.T) {
	a1, b1, b2 := memCreate(memA, memV1), memCreate(memB, memV1), memCreate(memB, memV2)
	m := Memory{Declines: []Decline{
		{PR: 12, Complete: true, Changes: ShortChanges([]Pair{b2})},
		{PR: 11, Key: Key(StreamSync, []Pair{a1})}, // incomplete: no changes
		{PR: 9, Complete: true, Changes: ShortChanges([]Pair{a1, b1})},
		{PR: 7, Complete: true, Changes: ShortChanges([]Pair{a1})},
		{PR: 7, Complete: true, Changes: ShortChanges([]Pair{a1})}, // listed twice
	}}
	for _, c := range []struct {
		name string
		d    []Pair
		want []int64
	}{
		{"B at version 2 in #12, A in #9 and #7", []Pair{a1, b2}, []int64{12, 9, 7}},
		{"the same path, another pair", []Pair{memCreate(memA, memV2)}, nil},
		{"B at version 1", []Pair{b1}, []int64{9}},
		{"nothing", nil, nil},
	} {
		if got := m.Overlap(c.d); !slices.Equal(got, c.want) {
			t.Errorf("%s: Overlap = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMemoryCooldown(t *testing.T) {
	const cd = 30 * memDay
	day := func(n int) time.Time { return memT0.Add(time.Duration(n) * memDay) }
	auto := func(pr int64, key string, closed int) AutoClose {
		return AutoClose{PR: pr, Key: key, ClosedAt: day(closed)}
	}
	cases := []struct {
		name     string
		m        Memory
		key      string
		now      int
		cooldown time.Duration
		until    time.Time
		declined bool
	}{
		{"no auto-close", Memory{}, "k", 5, cd, time.Time{}, false},
		{"another key", Memory{Auto: []AutoClose{auto(7, "j", 0)}}, "k", 5, cd, time.Time{}, false},
		{"an empty key", Memory{Auto: []AutoClose{auto(7, "", 0)}}, "", 5, cd, time.Time{}, false},
		{"the first defers", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 5, cd, day(30), false},
		{"the first, a second before the end", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 30, cd - time.Second, time.Time{}, false},
		{"the first, at the end", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 30, cd, time.Time{}, false},
		{"the first, after it", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 31, cd, time.Time{}, false},
		{"the second defers twice as long", Memory{Auto: []AutoClose{auto(9, "k", 40), auto(7, "k", 0)}}, "k", 50, cd, day(100), false},
		{"the third declines", Memory{Auto: []AutoClose{auto(11, "k", 80), auto(9, "k", 40), auto(7, "k", 0)}}, "k", 500, cd, time.Time{}, true},
		{"the fourth declines", Memory{Auto: []AutoClose{auto(13, "k", 90), auto(11, "k", 80), auto(9, "k", 40), auto(7, "k", 0)}}, "k", 500, cd, time.Time{}, true},
		{"the newest of the key is not the newest auto-close", Memory{Auto: []AutoClose{auto(10, "j", 60), auto(9, "k", 40), auto(7, "k", 0)}}, "k", 50, cd, day(100), false},
		{"a run broken by another key", Memory{Auto: []AutoClose{auto(11, "k", 80), auto(10, "j", 60), auto(9, "k", 40), auto(7, "k", 0)}}, "k", 90, cd, day(110), false},
		{
			"the third, lapsed",
			Memory{Lapsed: []int64{11}, Auto: []AutoClose{auto(11, "k", 80), auto(9, "k", 40), auto(7, "k", 0)}},
			"k", 81, cd, time.Time{}, false,
		},
		{
			"the first, remembered as a decline",
			Memory{Declines: []Decline{{PR: 7, Key: "k", Acked: true}}, Auto: []AutoClose{auto(7, "k", 0)}},
			"k", 5, cd, time.Time{}, true,
		},
		{"no cooldown means 30 days", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 5, 0, day(30), false},
		{"a negative cooldown means 30 days", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 5, -time.Hour, day(30), false},
		{"hours", Memory{Auto: []AutoClose{auto(7, "k", 0)}}, "k", 0, 12 * time.Hour, memT0.Add(12 * time.Hour), false},
		{"closed at an unknown time", Memory{Auto: []AutoClose{{PR: 7, Key: "k"}}}, "k", 5, cd, time.Time{}, false},
		{"closed at an unknown time, now unknown too", Memory{Auto: []AutoClose{{PR: 7, Key: "k"}}}, "k", -1, cd, time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := day(c.now)
			if c.now < 0 {
				now = time.Time{}
			}
			until, declined := c.m.Cooldown(c.key, now, c.cooldown)
			if !until.Equal(c.until) || declined != c.declined {
				t.Errorf("Cooldown = %v %v, want %v %v", until, declined, c.until, c.declined)
			}
		})
	}
	// Twice the largest cooldown hub.yml allows (99999 days) overflows a
	// duration: it saturates instead of turning negative.
	m := Memory{Auto: []AutoClose{auto(9, "k", 40), auto(7, "k", 0)}}
	if until, _ := m.Cooldown("k", day(50), 99999*memDay); !until.After(day(50 + 99999)) {
		t.Errorf("Cooldown with a huge cooldown = %v, want far in the future", until)
	}
}

func TestMemoryShortChanges(t *testing.T) {
	sha256 := strings.Repeat("9f", 32)
	d := []Pair{
		memCreate(memB, memV1),
		{Path: "bin/tool", From: memX, Mode: "100755", To: memX},
		memDelete(memR, memV2),
		memUpdate(memA, strings.ToUpper(memX), sha256),
	}
	before := slices.Clone(d)
	want := []marker.Change{
		{Path: memA, From: memX[:16], Mode: "100644", To: sha256[:16]},
		{Path: "bin/tool", From: memX[:16], Mode: "100755", To: memX[:16]},
		{Path: memR, From: memV2[:16], Mode: "", To: ""},
		{Path: memB, From: "", Mode: "100644", To: memV1[:16]},
	}
	got := ShortChanges(d)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ShortChanges =\n%+v\nwant\n%+v", got, want)
	}
	if !slices.Equal(d, before) {
		t.Error("ShortChanges modified its input")
	}
	if ShortChanges(nil) != nil || ShortChanges([]Pair{}) != nil {
		t.Error("ShortChanges of no pairs is not nil")
	}
	// A deletion always has an empty mode and to, whatever the pair says.
	stray := []Pair{{Path: memR, From: memV2, Mode: ModeDelete, To: memV1}}
	if got := ShortChanges(stray); !reflect.DeepEqual(got, []marker.Change{{Path: memR, From: memV2[:16]}}) {
		t.Errorf("ShortChanges of a deletion with a blob to write = %+v", got)
	}
	// A marker takes them as they are.
	line, err := marker.Encode(marker.Marker{
		Key:  Key(StreamSync, d),
		Data: marker.Data{V: marker.Version, Stream: StreamSync, Hub: "acme-eng", FP: memFP, Changes: got, ChangesComplete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := marker.Parse(line); err != nil || !reflect.DeepEqual(m.Data.Changes, want) {
		t.Errorf("round trip: %+v, %v", m.Data.Changes, err)
	}
}

func TestMemoryDoesNotModifyInput(t *testing.T) {
	a1 := []Pair{memCreate(memA, memV1)}
	own := []OwnPR{
		memPR(t, 11, a1, &memStale, 80),
		memPR(t, 10, a1, &memPerson, 60, memAcked(memH1)),
		memPR(t, 9, a1, &memStale, 40),
		memPR(t, 7, a1, &memStale, 0),
	}
	in := memInput(own)
	in.Forget, in.Repropose = []int64{10}, map[int64]bool{9: true}
	// JSON follows pointers (ClosedBy, Closed, RecreateFor), which %#v
	// prints as addresses. LocalOrIgnored is a function: left out.
	encode := func() string {
		js, err := json.Marshal([]any{in.Own, in.OptIn, in.Forget, in.Repropose, in.Now, in.Config})
		if err != nil {
			t.Fatal(err)
		}
		return string(js)
	}
	want := encode()
	first := BuildMemory(in)
	if got := encode(); got != want {
		t.Errorf("BuildMemory modified its input:\n%s\n%s", want, got)
	}
	if second := BuildMemory(in); !reflect.DeepEqual(first, second) {
		t.Error("BuildMemory is not deterministic")
	}
}

// TestMemoryProperties checks invariants of BuildMemory, IsDeclined and
// Overlap on random histories.
func TestMemoryProperties(t *testing.T) {
	runs := 400
	if testing.Short() {
		runs = 50
	}
	paths := []string{"a", "b", "c"}
	blobs := []string{memV1, memV2, memX}
	closers := []*platform.Account{nil, &memPerson, &memStale, &memWriter, &memKnown, &memRunner}
	r := rand.New(rand.NewPCG(3, 5))
	// r2 draws what only the platforms with immutable closed pull requests
	// read (markers without optin), so that r draws the inputs it drew
	// before they existed.
	r2 := rand.New(rand.NewPCG(7, 11))
	for run := range runs {
		var own []OwnPR
		pairs := map[int64][]Pair{}
		for i := range r.IntN(10) {
			n := int64(100 - i)
			var d []Pair
			for _, p := range paths {
				if r.IntN(2) == 0 {
					d = append(d, memUpdate(p, blobs[r.IntN(3)], blobs[r.IntN(3)]))
				}
			}
			if len(d) == 0 {
				d = []Pair{memCreate(paths[r.IntN(3)], blobs[r.IntN(3)])}
			}
			var edits []func(*marker.Data)
			switch r.IntN(6) {
			case 0:
				edits = append(edits, memAcked(memH1))
			case 1:
				edits = append(edits, memAcked(memH2))
			case 2:
				edits = append(edits, memSelfClosed)
			}
			if r.IntN(8) == 0 {
				edits = append(edits, memRevoked)
			}
			if r.IntN(8) == 0 {
				edits = append(edits, memIncomplete)
			}
			if r2.IntN(6) == 0 {
				edits = append(edits, memNoOptIn)
			}
			o := memPR(t, n, d, closers[r.IntN(len(closers))], 100-i, edits...)
			o.PR.BaseExists = r.IntN(10) > 0
			switch r.IntN(8) {
			case 0:
				o = memState(o, platform.Open)
			case 1:
				o = memState(o, platform.Merged)
			}
			own = append(own, o)
			pairs[n] = d
		}
		local := map[string]bool{}
		for _, p := range paths {
			local[p] = r.IntN(5) == 0
		}
		in := MemoryInput{
			Own:            own,
			OptIn:          []string{memH1, memH2, ""}[r.IntN(3)],
			LocalOrIgnored: func(p string) bool { return local[p] },
			Repropose:      map[int64]bool{},
			Config:         memConfig(),
		}
		in.Config.CloserKnown = r.IntN(4) > 0
		in.Config.Window = r.IntN(12)
		for n := range pairs {
			switch r.IntN(10) {
			case 0:
				in.Forget = append(in.Forget, n)
			case 1:
				in.Repropose[n] = true
			}
		}
		slices.Sort(in.Forget)
		// The same input on a platform whose closed pull requests are
		// immutable, and on the others.
		for _, immutable := range []bool{false, true} {
			in.Config.ClosedImmutable = immutable
			m := BuildMemory(in)
			if err := memCheck(in, m, pairs); err != nil {
				t.Fatalf("run %d, closed immutable %v: %v\ninput: %+v\nmemory: %s", run, immutable, err, in, memNumbers(m))
			}
		}
	}
}

// memCheck verifies the invariants of m = BuildMemory(in); pairs holds
// the pairs of every PR of in.Own.
func memCheck(in MemoryInput, m Memory, pairs map[int64][]Pair) error {
	window := in.Config.Window
	if window <= 0 {
		window = memDefaultWindow
	}
	// The window, and what each PR in it must become.
	var closed []OwnPR
	for _, o := range in.Own {
		if o.PR.State == platform.Closed && len(closed) < window {
			closed = append(closed, o)
		}
	}
	type where struct{ decline, lapsed, revoke, auto, ack, forgotten, unanchored bool }
	got := map[int64]*where{}
	at := func(n int64) *where {
		if got[n] == nil {
			got[n] = &where{}
		}
		return got[n]
	}
	for _, d := range m.Declines {
		at(d.PR).decline = true
	}
	for _, n := range m.Lapsed {
		at(n).lapsed = true
	}
	for _, n := range m.ToRevoke {
		at(n).revoke = true
	}
	for _, a := range m.Auto {
		at(a.PR).auto = true
	}
	for _, n := range m.ToAck {
		at(n).ack = true
	}
	for _, n := range m.Forgotten {
		at(n).forgotten = true
	}
	for _, n := range m.Unanchored {
		at(n).unanchored = true
	}
	immutable := in.Config.ClosedImmutable
	if immutable && len(m.ToAck)+len(m.ToRevoke) > 0 {
		return fmt.Errorf("closed pull requests are immutable, yet memory asks to write ack %v, revoke %v", m.ToAck, m.ToRevoke)
	}
	if !immutable && len(m.Forgotten)+len(m.Unanchored) > 0 {
		return fmt.Errorf("closed pull requests can be edited, yet memory lists forgotten %v, unanchored %v", m.Forgotten, m.Unanchored)
	}
	inWindow := map[int64]bool{}
	var order []int64
	for _, o := range closed {
		n, data := o.PR.Number, o.Marker.Data
		inWindow[n] = true
		order = append(order, n)
		w := at(n)
		class := ClassifyClose(o, in.Config)
		switch {
		case class == CloseSelf || data.Revoked:
			if *w != (where{}) {
				return fmt.Errorf("#%d holds no memory, yet it is in %+v", n, *w)
			}
			continue
		case immutable && slices.Contains(in.Forget, n):
			if *w != (where{forgotten: true}) {
				return fmt.Errorf("#%d is forgotten, yet it is in %+v", n, *w)
			}
			continue
		case !immutable && (slices.Contains(in.Forget, n) || in.Repropose[n]):
			if *w != (where{revoke: true}) {
				return fmt.Errorf("#%d is to revoke, yet it is in %+v", n, *w)
			}
			continue
		}
		if w.revoke || w.forgotten {
			return fmt.Errorf("#%d is in ToRevoke or Forgotten unasked", n)
		}
		if (class == CloseAuto) != w.auto {
			return fmt.Errorf("#%d: class %v, in Auto %v", n, class, w.auto)
		}
		// A decline, or an auto-close that counts as one: acked, or the
		// third or later of its run.
		candidate := class == CloseDecline || data.Ack
		if i := slices.IndexFunc(m.Auto, func(a AutoClose) bool { return a.PR == n }); i >= 0 && memRun(m.Auto, i) >= memEscalateAt {
			candidate = true
		}
		if (candidate && w.decline == w.lapsed) || (!candidate && (w.decline || w.lapsed)) {
			return fmt.Errorf("#%d: a decline %v, in Declines %v, in Lapsed %v", n, candidate, w.decline, w.lapsed)
		}
		// Where no one can write an ack, the optin the marker held while
		// open stands for one.
		acked := data.Ack || (immutable && data.OptIn != "")
		if w.ack != (w.decline && !acked && !immutable) || w.unanchored != (w.decline && !acked && immutable) {
			return fmt.Errorf("#%d: in ToAck %v, unanchored %v, in force %v, acked %v", n, w.ack, w.unanchored, w.decline, acked)
		}
		if i := slices.IndexFunc(m.Declines, func(d Decline) bool { return d.PR == n }); i >= 0 {
			want := ""
			if acked {
				want = data.OptIn
			}
			if d := m.Declines[i]; d.Acked != acked || d.OptIn != want {
				return fmt.Errorf("#%d: decline %+v, want acked %v with %q", n, d, acked, want)
			}
		}
		lapse := acked && in.OptIn != "" && data.OptIn != in.OptIn
		for _, c := range data.Changes {
			lapse = lapse || in.LocalOrIgnored(c.Path)
		}
		if w.lapsed != (lapse && (w.decline || w.lapsed)) {
			return fmt.Errorf("#%d: lapsed %v, the rules say %v", n, w.lapsed, lapse)
		}
	}
	for n := range got {
		if !inWindow[n] {
			return fmt.Errorf("#%d is remembered from outside the window", n)
		}
	}
	// Lists follow the window order.
	inOrder := func(list []int64) bool {
		last := -1
		for _, n := range list {
			i := slices.Index(order, n)
			if i <= last {
				return false
			}
			last = i
		}
		return true
	}
	var declines, auto []int64
	for _, d := range m.Declines {
		declines = append(declines, d.PR)
	}
	for _, a := range m.Auto {
		auto = append(auto, a.PR)
	}
	for _, list := range [][]int64{declines, m.Lapsed, m.ToAck, m.ToRevoke, auto, m.Forgotten, m.Unanchored} {
		if !inOrder(list) {
			return fmt.Errorf("a list is out of window order: %v", list)
		}
	}
	// A decline in force covers its own content, and whatever it covers
	// names it.
	for _, d := range m.Declines {
		p := pairs[d.PR]
		ok, prs := m.IsDeclined(p, Key(StreamSync, p))
		if !ok || !slices.Contains(prs, d.PR) {
			return fmt.Errorf("#%d does not cover its own pairs: %v %v", d.PR, ok, prs)
		}
		for _, n := range prs {
			if !slices.Contains(declines, n) {
				return fmt.Errorf("IsDeclined names #%d, which is not in force", n)
			}
		}
		if d.Complete && !slices.Contains(m.Overlap(p), d.PR) {
			return fmt.Errorf("#%d does not overlap its own pairs", d.PR)
		}
	}
	return nil
}

// memNumbers prints the PR numbers of every list of m.
func memNumbers(m Memory) string {
	var declines, auto []int64
	for _, d := range m.Declines {
		declines = append(declines, d.PR)
	}
	for _, a := range m.Auto {
		auto = append(auto, a.PR)
	}
	s := fmt.Sprintf("Declines %v Lapsed %v ToAck %v ToRevoke %v Auto %v", declines, m.Lapsed, m.ToAck, m.ToRevoke, auto)
	if len(m.Forgotten)+len(m.Unanchored) > 0 {
		s += fmt.Sprintf(" Forgotten %v Unanchored %v", m.Forgotten, m.Unanchored)
	}
	return s
}

// memSameNumbers compares the PR numbers of every list of two memories.
func memSameNumbers(a, b Memory) bool { return memNumbers(a) == memNumbers(b) }

// memClone returns a copy of m whose lists can be cut without touching m.
func memClone(m Memory) Memory {
	m.Declines = slices.Clone(m.Declines)
	m.Lapsed = slices.Clone(m.Lapsed)
	m.ToAck = slices.Clone(m.ToAck)
	m.ToRevoke = slices.Clone(m.ToRevoke)
	m.Auto = slices.Clone(m.Auto)
	m.Forgotten = slices.Clone(m.Forgotten)
	m.Unanchored = slices.Clone(m.Unanchored)
	return m
}
