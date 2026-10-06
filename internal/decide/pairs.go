package decide

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// ZeroOID stands for "no blob" in pairs and keys.
const ZeroOID = "0000000000000000000000000000000000000000"

// ModeDelete is the mode of a deletion in pairs and keys.
const ModeDelete = "000000"

// Streams. A stream is a separate line of pull requests with its own branch
// and key domain. The adopt stream is not implemented yet.
const (
	StreamSync  = "sync"
	StreamAdopt = "adopt"
)

// keyDomain starts the hashed form of every content key.
const keyDomain = "touchmark-content/v1\n"

// Pair is one change a target should receive: the path, the blob the base
// holds there, the mode to write and the blob to write.
type Pair struct {
	Path string
	From string // blob in the base (B), or ZeroOID
	Mode string // "100644", "100755", or ModeDelete
	To   string // blob to write, or ZeroOID for a deletion
}

// Pairs returns D: one pair per plan entry whose action changes the target
// (Create, Update, Adopt, Chmod, Delete), sorted by path bytes. Keep and
// unknown actions give no pair; a Create with AfterDeletes is a create like
// any other.
//
//   - From is Entry.From, or ZeroOID when empty.
//   - To is Entry.To; for Chmod with an empty To it is Entry.From; for
//     Delete it is ZeroOID.
//   - Mode is Entry.Mode ("100644" when empty), or ModeDelete for Delete.
//
// Decide leaves From empty for Adopt (a local file matches no version), so
// an adopt pair has From = ZeroOID; the adopt stream, once implemented,
// needs Decide to record the observed blob first.
func Pairs(plan Plan) []Pair {
	var out []Pair
	for _, e := range plan.Entries {
		p := Pair{Path: e.Path, From: orZero(e.From), Mode: e.Mode, To: e.To}
		switch e.Action {
		case Create, Update, Adopt:
		case Chmod:
			if p.To == "" {
				p.To = e.From
			}
		case Delete:
			p.Mode, p.To = ModeDelete, ""
		default:
			continue
		}
		if p.Mode == "" {
			p.Mode = modeFile
		}
		p.To = orZero(p.To)
		out = append(out, p)
	}
	sortPairs(out)
	return out
}

// Key returns the content key of pairs in stream:
//
//	"sha256:" + hex(sha256("touchmark-content/v1\n" + stream + "\n" +
//	    for each pair in path byte order: path "\x00" from "\x00" mode "\x00" to "\n"))
//
// The base commit, hub commit, engine version, packs, PR text and hub id do
// not enter the key: memory survives unrelated commits, pack renames and a
// hub id change. The input order does not matter and the input is not
// modified; the fields are hashed as given (Pairs writes ZeroOID and
// ModeDelete, never ""). Duplicate paths are a programming error and panic.
func Key(stream string, pairs []Pair) string {
	sorted := slices.Clone(pairs)
	sortPairs(sorted)
	buf := make([]byte, 0, len(keyDomain)+len(stream)+1+len(sorted)*128)
	buf = append(buf, keyDomain...)
	buf = append(buf, stream...)
	buf = append(buf, '\n')
	for i, p := range sorted {
		if i > 0 && sorted[i-1].Path == p.Path {
			panic(fmt.Sprintf("decide.Key: duplicate path %q", p.Path))
		}
		buf = append(buf, p.Path...)
		buf = append(buf, 0)
		buf = append(buf, p.From...)
		buf = append(buf, 0)
		buf = append(buf, p.Mode...)
		buf = append(buf, 0)
		buf = append(buf, p.To...)
		buf = append(buf, '\n')
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Short returns the first n hex digits of oid, or "" for "", for an id of
// zeros only (ZeroOID or its SHA-256 form) and for n <= 0. An oid shorter
// than n is returned whole.
func Short(oid string, n int) string {
	if n <= 0 || strings.Trim(oid, "0") == "" {
		return ""
	}
	if n >= len(oid) {
		return oid
	}
	return oid[:n]
}

// orZero returns oid, or ZeroOID when it is empty.
func orZero(oid string) string {
	if oid == "" {
		return ZeroOID
	}
	return oid
}

// sortPairs orders pairs by path bytes.
func sortPairs(pairs []Pair) {
	slices.SortFunc(pairs, func(a, b Pair) int { return strings.Compare(a.Path, b.Path) })
}
