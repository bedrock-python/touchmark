package decide

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// Memory defaults.
const (
	// memDefaultWindow is how many closed own PRs memory reads when
	// MemoryConfig.Window is not positive.
	memDefaultWindow = 50
	// memDefaultCooldown is memory.auto_close_cooldown when Cooldown gets
	// a duration that is not positive.
	memDefaultCooldown = 30 * 24 * time.Hour
	// memEscalateAt is the auto-close that counts as a decline: the third
	// in a row with the same key.
	memEscalateAt = 3
	// memClosedBy is marker closed.by when touchmark closed the PR.
	memClosedBy = "touchmark"
)

// OwnPR is one of touchmark's own pull requests (Identity.Own said Ours),
// with its parsed marker and the sync branch it lives on.
type OwnPR struct {
	PR     platform.PR
	Marker marker.Marker
	// Alias is set when the PR's head is one of the branch_aliases rather
	// than the sync branch itself.
	Alias bool
}

// CloseClass classifies a closed-without-merge own PR.
type CloseClass uint8

const (
	// CloseSelf: touchmark closed it (marker closed.by = touchmark), or
	// the writer or a known author closed it. Creates no memory.
	CloseSelf CloseClass = iota + 1
	// CloseAuto: a bot closed it (Account.Kind == KindBot or an
	// automation_accounts id), or its base branch no longer exists.
	CloseAuto
	// CloseDecline: anyone else, including an unknown closer; also every
	// close on a platform whose Caps.CloserKnown is false (except the two
	// cases above that do not need the closer: closed.by and a missing
	// base).
	CloseDecline
)

// String returns the class name for messages and tests.
func (c CloseClass) String() string {
	switch c {
	case CloseSelf:
		return "self"
	case CloseAuto:
		return "auto"
	case CloseDecline:
		return "decline"
	}
	return "unknown"
}

// MemoryConfig tunes memory for one provider.
type MemoryConfig struct {
	// Cooldown is memory.auto_close_cooldown (30 days by default).
	Cooldown time.Duration
	// Writers are the stable ids of the writer and known_authors.
	Writers map[string]bool
	// Automation are the stable ids of automation_accounts.
	Automation map[string]bool
	// CloserKnown is Caps.CloserKnown.
	CloserKnown bool
	// ClosedImmutable is Caps.ClosedImmutable: a closed pull request can
	// never be edited (Bitbucket Cloud's declined pull requests), so
	// memory asks for no write to one. Declines are then read as follows
	// (see BuildMemory):
	//   - a decline whose marker holds optin, the hash of the opt-in file
	//     touchmark wrote into the marker while the pull request was open
	//     (on every create and content edit, and on any edit that finds
	//     it stale), counts as acked with that optin: in force while the
	//     opt-in file hashes the same, lapsed once it changes, as after an
	//     ack written elsewhere;
	//   - a decline whose marker holds no optin (a marker touchmark did
	//     not write in full) is in force whatever the opt-in file says,
	//     and is listed in Memory.Unanchored so that the report says how to
	//     lift it: no state of the file was recorded to compare with, and a
	//     decline that lapsed silently would propose the content again;
	//   - a pull request a forget_declines entry names is no memory while
	//     the entry is present (Memory.Forgotten): its revocation cannot be
	//     written into the pull request, so the entry does not act once;
	//   - ticked repropose controls are ignored: such a platform shows no
	//     tick box (Caps.BodyControls), and no ack adds one.
	//
	// ToAck and ToRevoke are always empty then.
	ClosedImmutable bool
	// Window is how many closed own PRs are considered, newest first (50;
	// a value that is not positive means 50).
	Window int
}

// ClassifyClose classifies a closed (not merged) own PR. It panics on an
// open or merged PR (a programming error).
//
// The rules apply in this order:
//  1. marker closed.by is "touchmark" → CloseSelf, whoever the platform
//     names as the closer;
//  2. the base branch no longer exists (PR.BaseExists false) → CloseAuto:
//     deleting a base closes its PRs on the platform's behalf, whoever
//     deleted it;
//  3. cfg.CloserKnown is false, or the platform names no closer (ClosedBy
//     nil or without an id) → CloseDecline: better to ask the team once
//     than to propose the content again silently;
//  4. the closer's id is in cfg.Writers → CloseSelf (on GitHub the writer is
//     itself a bot, so this comes before rule 5);
//  5. the closer is a bot (KindBot) or its id is in cfg.Automation →
//     CloseAuto (a service account counts only through automation_accounts);
//  6. anyone else → CloseDecline.
func ClassifyClose(o OwnPR, cfg MemoryConfig) CloseClass {
	if o.PR.State != platform.Closed {
		panic(fmt.Sprintf("decide.ClassifyClose: #%d is %q, not closed without merge", o.PR.Number, o.PR.State))
	}
	if c := o.Marker.Data.Closed; c != nil && c.By == memClosedBy {
		return CloseSelf
	}
	if !o.PR.BaseExists {
		return CloseAuto
	}
	by := o.PR.ClosedBy
	if !cfg.CloserKnown || by == nil || by.ID == "" {
		return CloseDecline
	}
	switch {
	case cfg.Writers[by.ID]:
		return CloseSelf
	case by.Kind == platform.KindBot || cfg.Automation[by.ID]:
		return CloseAuto
	}
	return CloseDecline
}

// Decline is an own PR a person closed without merging, while it is in
// force. An auto-close that counts as a decline (see Memory.Declines) is
// one too.
type Decline struct {
	PR       int64
	Key      string
	Changes  []marker.Change
	Complete bool // marker.Data.ChangesComplete
	// Acked is set when the opt-in state the decline holds under is known:
	// an ack was written into its marker, or, on a platform whose closed
	// pull requests are immutable (MemoryConfig.ClosedImmutable), the
	// marker recorded optin while the pull request was open.
	Acked    bool
	OptIn    string // marker.Data.OptIn of an acked decline ("" otherwise)
	ClosedAt time.Time
}

// AutoClose is an own PR a bot closed (class CloseAuto: a bot or an
// automation account, or its base branch is gone).
type AutoClose struct {
	PR       int64
	Key      string
	ClosedAt time.Time
}

// MemoryInput is what BuildMemory needs for one target and stream.
type MemoryInput struct {
	// Own are the target's own PRs of the stream, newest first. Open ones
	// are ignored here (an open PR is stronger than memory).
	Own []OwnPR
	// OptIn is the current hash of the opt-in file (config.OptIn.Hash).
	// When it is "" (unknown), acked declines stay in force.
	OptIn string
	// LocalOrIgnored reports whether a path is now local or ignored in the
	// target (from the per-path plan: states local, retired-local and
	// ignored). Nil means no path is.
	LocalOrIgnored func(path string) bool
	// Forget lists PR numbers operations.yml forget_declines names.
	Forget []int64
	// Repropose lists declined PRs whose "Propose this content again"
	// checkbox is ticked (ignored with Config.ClosedImmutable).
	Repropose map[int64]bool
	// Now is the run's clock. What memory holds does not depend on it:
	// Cooldown takes the time it compares with.
	Now    time.Time
	Config MemoryConfig
}

// Memory is what the target's closed PRs say (see docs/concepts/memory.md).
//
// Every list keeps the order of the window: newest first, as MemoryInput.Own
// lists the PRs.
type Memory struct {
	// Declines are in force: class CloseDecline, not revoked, not
	// forgotten or reproposed, OptIn unchanged since the ack (an unacked
	// decline is in force with the current hash), and no path of its
	// changes local or ignored now.
	//
	// An auto-close counts as a decline too, under the same rules, when it
	// is the third or later of a run of auto-closes with its key in Auto,
	// or when its marker carries an ack: touchmark remembered it as a
	// decline once (an escalated auto-close, or a person's decline whose
	// base branch was deleted since, or one seen before Caps.CloserKnown
	// turned true), and memory does not flip back.
	// Such a PR is in Auto as well.
	Declines []Decline
	// Lapsed are declines no longer in force because the team changed
	// packs or ignore, or made a path its own.
	Lapsed []int64
	// ToAck are declines in force without an ack: the caller writes the
	// ack (marker ack + optin, the repropose checkbox) and one comment.
	ToAck []int64
	// ToRevoke are declines to mark revoked now (forget_declines,
	// repropose ticked). Auto-closes named there are revoked too, which
	// ends their cooldown. A PR in ToRevoke is already out of every other
	// list, so a run that writes the revocation decides as the next run
	// will. Always empty with Config.ClosedImmutable.
	ToRevoke []int64
	// Forgotten are, with Config.ClosedImmutable, the closes a
	// forget_declines entry names: no memory while the entry is present,
	// and out of every other list (an auto-close among them ends its
	// cooldown). Nothing is written to them.
	Forgotten []int64
	// Unanchored are, with Config.ClosedImmutable, the declines in force
	// whose marker records no opt-in state (see MemoryConfig): they hold
	// until a forget_declines entry names them or a path of theirs becomes
	// local or ignored, and the report says so.
	Unanchored []int64
	// Auto are auto-closes among the window, newest first. Revoked ones
	// (and those in ToRevoke) are left out: a revoked PR is not memory.
	Auto []AutoClose
}

// BuildMemory reads the last Config.Window closed own PRs.
//
// The window is the first Config.Window PRs of Own whose state is Closed
// (closed without merge); open and merged PRs are skipped and do not count
// towards it: a merge is not a decline. Each PR of the window is
// classified (ClassifyClose), then:
//   - CloseSelf: no memory, whatever the marker says;
//   - marker revoked: no memory;
//   - named by Forget, or its repropose checkbox ticked: ToRevoke, and no
//     memory from this run on (revoking a lapsed decline makes the revocation
//     permanent, even if the opt-in file later returns to the acked state);
//     with Config.ClosedImmutable, named by Forget: Forgotten, and no memory
//     while the entry is present (a ticked checkbox counts for nothing
//     there);
//   - CloseAuto: an entry of Auto; it is a decline as well when its marker
//     is acked or it is the third or later of a run (see Memory.Declines);
//   - declines are then in force (Declines, and ToAck while unacked) or
//     Lapsed: acked with an optin other than OptIn, or a path of their
//     Changes LocalOrIgnored. With Config.ClosedImmutable a decline whose
//     marker holds optin counts as acked with it, and one in force without
//     it is Unanchored instead of ToAck.
//
// A run of an auto-close is itself and the consecutive older entries of
// Auto with the same key: an auto-close with another key ends it, while a
// decline, a self-close or a revoked PR between two auto-closes does not.
// So a repropose buys the content one more proposal (it bypasses memory
// once): if a bot closes that one too, the run goes on.
//
// BuildMemory does not modify its input and never panics.
func BuildMemory(in MemoryInput) Memory {
	window := in.Config.Window
	if window <= 0 {
		window = memDefaultWindow
	}
	forget := make(map[int64]bool, len(in.Forget))
	for _, n := range in.Forget {
		forget[n] = true
	}
	immutable := in.Config.ClosedImmutable
	var m Memory
	// kept are the closes that carry memory, in window order, with whether
	// the class was CloseAuto.
	type memo struct {
		o    OwnPR
		auto bool
	}
	var kept []memo
	seen := map[int64]bool{}
	for _, o := range in.Own {
		if len(seen) == window {
			break
		}
		n := o.PR.Number
		if o.PR.State != platform.Closed || seen[n] {
			continue
		}
		seen[n] = true
		class := ClassifyClose(o, in.Config)
		switch {
		case class == CloseSelf, o.Marker.Data.Revoked:
			continue
		case immutable && forget[n]:
			m.Forgotten = append(m.Forgotten, n)
			continue
		case !immutable && (forget[n] || in.Repropose[n]):
			m.ToRevoke = append(m.ToRevoke, n)
			continue
		}
		auto := class == CloseAuto
		if auto {
			m.Auto = append(m.Auto, AutoClose{PR: n, Key: o.Marker.Key, ClosedAt: o.PR.ClosedAt})
		}
		kept = append(kept, memo{o: o, auto: auto})
	}
	escalated := map[int64]bool{}
	for i, a := range m.Auto {
		if memRun(m.Auto, i) >= memEscalateAt {
			escalated[a.PR] = true
		}
	}
	for _, c := range kept {
		data := c.o.Marker.Data
		n := c.o.PR.Number
		if c.auto && !data.Ack && !escalated[n] {
			continue
		}
		d := Decline{
			PR:       n,
			Key:      c.o.Marker.Key,
			Changes:  slices.Clone(data.Changes),
			Complete: data.ChangesComplete,
			// On a platform whose closed pull requests are immutable, the
			// optin the open pull request's marker held stands for the ack
			// no one can write.
			Acked:    data.Ack || (immutable && data.OptIn != ""),
			ClosedAt: c.o.PR.ClosedAt,
		}
		if d.Acked {
			d.OptIn = data.OptIn
		}
		if memLapsed(d, in) {
			m.Lapsed = append(m.Lapsed, n)
			continue
		}
		m.Declines = append(m.Declines, d)
		switch {
		case d.Acked:
		case immutable:
			m.Unanchored = append(m.Unanchored, n)
		default:
			m.ToAck = append(m.ToAck, n)
		}
	}
	return m
}

// memLapsed reports whether decline d no longer holds: the team changed the
// opt-in file since the ack, or made a path of d its own or ignored it. An
// unacked decline counts from the current hash; an unknown current hash
// ("") keeps acked declines in force.
func memLapsed(d Decline, in MemoryInput) bool {
	if d.Acked && in.OptIn != "" && d.OptIn != in.OptIn {
		return true
	}
	if in.LocalOrIgnored == nil {
		return false
	}
	return slices.ContainsFunc(d.Changes, func(c marker.Change) bool { return in.LocalOrIgnored(c.Path) })
}

// memRun returns how many consecutive entries of auto, from index i on
// (older ones), have the key of auto[i].
func memRun(auto []AutoClose, i int) int {
	n := 0
	for _, a := range auto[i:] {
		if a.Key != auto[i].Key {
			break
		}
		n++
	}
	return n
}

// IsDeclined reports whether d is covered by declines in force: d is not
// empty, and every pair of d appears in the union of their Changes
// (compared in short form: path, Short(from), mode, Short(to), with
// ModeDelete written as "" in Changes), or key equals the Key of a decline
// whose changes are incomplete. It returns the PR numbers that cover d.
//
// The covering PRs are, in the order of Declines (newest first, so the
// first one is the newest): when the union covers d, every decline that
// holds at least one pair of d; and every incomplete decline whose key is
// key. The Changes an incomplete decline still lists count towards the
// union: each of them was part of what was declined.
func (m Memory) IsDeclined(d []Pair, key string) (bool, []int64) {
	if len(d) == 0 {
		return false, nil
	}
	want := memShortSet(d)
	covered := map[marker.Change]bool{}
	holds := map[int64]bool{}
	byKey := map[int64]bool{}
	for _, dec := range m.Declines {
		if !dec.Complete && key != "" && dec.Key == key {
			byKey[dec.PR] = true
		}
		for _, c := range dec.Changes {
			if want[c] {
				covered[c] = true
				holds[dec.PR] = true
			}
		}
	}
	union := len(covered) == len(want)
	if !union && len(byKey) == 0 {
		return false, nil
	}
	var prs []int64
	for _, dec := range m.Declines {
		if ((union && holds[dec.PR]) || byKey[dec.PR]) && !slices.Contains(prs, dec.PR) {
			prs = append(prs, dec.PR)
		}
	}
	return true, prs
}

// Overlap returns the declines whose changes share at least one pair with d,
// for the "previously declined in #N" block of a new PR.
//
// The pairs compare in short form, as in IsDeclined; the PRs come in the
// order of Declines (newest first), each once. A decline without Changes
// shares nothing.
func (m Memory) Overlap(d []Pair) []int64 {
	if len(d) == 0 {
		return nil
	}
	want := memShortSet(d)
	var out []int64
	for _, dec := range m.Declines {
		if slices.Contains(out, dec.PR) {
			continue
		}
		if slices.ContainsFunc(dec.Changes, func(c marker.Change) bool { return want[c] }) {
			out = append(out, dec.PR)
		}
	}
	return out
}

// Cooldown applies auto-close escalation to key: among
// consecutive newest auto-closes with this key, the first defers the key
// until ClosedAt + Cooldown, the second until ClosedAt + 2×Cooldown, and
// the third counts as a decline. It returns the time until which the key is
// deferred (zero when not deferred at now) and whether it now counts as
// declined.
//
// The run is the one that starts at the newest auto-close with key in Auto
// (see BuildMemory), and ClosedAt is that newest one's. When that auto-close
// counts as a decline (the run holds three or more, or it is among
// Declines), declined is true, unless the decline lapsed (it is in Lapsed):
// then the team's change lifts it and the key is neither declined nor
// deferred. A cooldown that is not positive means the default, 30 days; a
// zero ClosedAt (the platform did not say) defers nothing. An empty key is
// never deferred.
func (m Memory) Cooldown(key string, now time.Time, cooldown time.Duration) (until time.Time, declined bool) {
	if key == "" {
		return time.Time{}, false
	}
	i := slices.IndexFunc(m.Auto, func(a AutoClose) bool { return a.Key == key })
	if i < 0 {
		return time.Time{}, false
	}
	newest := m.Auto[i]
	if slices.Contains(m.Lapsed, newest.PR) {
		return time.Time{}, false
	}
	n := memRun(m.Auto, i)
	if n >= memEscalateAt || slices.ContainsFunc(m.Declines, func(d Decline) bool { return d.PR == newest.PR }) {
		return time.Time{}, true
	}
	if newest.ClosedAt.IsZero() {
		return time.Time{}, false
	}
	if cooldown <= 0 {
		cooldown = memDefaultCooldown
	}
	wait := cooldown
	if n > 1 {
		wait = memScale(cooldown, int64(n))
	}
	until = newest.ClosedAt.Add(wait)
	if !until.After(now) {
		return time.Time{}, false
	}
	return until, false
}

// memScale returns d×k, saturating at the largest duration instead of
// overflowing.
func memScale(d time.Duration, k int64) time.Duration {
	if d > time.Duration(math.MaxInt64/k) {
		return time.Duration(math.MaxInt64)
	}
	return d * time.Duration(k)
}

// ShortChanges converts pairs to marker changes (short oids; a deletion has
// Mode "" and To "").
//
// The oids keep their first marker.ShortOID hex digits, lowercased, and the
// zero id becomes "" (Short). The result is sorted by path bytes, like D and
// the key, so the marker does not depend on the order of the input; it is
// nil for no pairs. The input is not modified.
func ShortChanges(d []Pair) []marker.Change {
	if len(d) == 0 {
		return nil
	}
	out := make([]marker.Change, 0, len(d))
	for _, p := range d {
		out = append(out, memShort(p))
	}
	slices.SortStableFunc(out, func(a, b marker.Change) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// memShort returns p in the short form a marker keeps.
func memShort(p Pair) marker.Change {
	c := marker.Change{Path: p.Path, From: memShortOID(p.From), Mode: p.Mode, To: memShortOID(p.To)}
	if p.Mode == ModeDelete {
		c.Mode, c.To = "", ""
	}
	return c
}

// memShortOID returns the short, lowercase form of a blob id in a marker.
func memShortOID(oid string) string { return strings.ToLower(Short(oid, marker.ShortOID)) }

// memShortSet returns the short forms of pairs as a set.
func memShortSet(pairs []Pair) map[marker.Change]bool {
	set := make(map[marker.Change]bool, len(pairs))
	for _, p := range pairs {
		set[memShort(p)] = true
	}
	return set
}
