// Package marker encodes and parses the marker touchmark writes as the last
// line of every pull request it opens:
//
//	<!-- touchmark:v1 hub=<id> fp=<16 hex> stream=<stream> key=sha256:<64 hex> data=<base64(gzip(json))> -->
//
// or, where a platform escapes HTML in descriptions (Bitbucket Cloud), the
// same payload in a Markdown link reference definition (FrameRefDef):
//
//	[touchmark]: # "touchmark:v1 hub=<id> fp=<16 hex> stream=<stream> key=sha256:<64 hex> data=<base64(gzip(json))>"
//
// The marker is the only memory touchmark has: what the PR carried, who
// the hub is, and what the engine wrote last. Everything in a PR body is
// untrusted input, so parsing is strict and bounded.
package marker

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Format limits.
const (
	// Version is the marker format version ("touchmark:v1").
	Version = 1
	// MaxLine is the largest marker line written, in either frame, and the
	// largest accepted in the comment frame. A reference definition is
	// accepted up to twice as long before its backslash escapes are undone
	// (an escape per byte at most), and up to MaxLine after.
	MaxLine = 16 << 10
	// MaxData is the largest decompressed JSON accepted.
	MaxData = 64 << 10
	// MaxJSON is the largest JSON the encoder writes before it drops
	// Changes and sets ChangesComplete to false.
	MaxJSON = 48 << 10
	// MaxChanges and MaxString bound the parsed JSON.
	MaxChanges = 5000
	MaxString  = 4096
	// ShortOID is how many hex digits of each blob id Changes keeps.
	ShortOID = 16
)

// Change is one pair in short form: path, first ShortOID hex digits of the
// blob in the base ("" when absent), mode to write ("" for a deletion) and
// first ShortOID hex digits of the blob to write ("" for a deletion). It is
// encoded as a JSON array of four strings.
type Change struct {
	Path string
	From string
	Mode string
	To   string
}

// Closed records that touchmark closed the PR itself, and why.
type Closed struct {
	By     string `json:"by"`     // "touchmark"
	Reason string `json:"reason"` // no-diff, opted-out, target-dropped, duplicate
}

// Data is the JSON payload.
type Data struct {
	V      int    `json:"v"`
	Stream string `json:"stream"`
	Hub    string `json:"hub"`
	// FP is the full hub fingerprint, e.g. "github.com/712345678". The
	// header carries only FP16(FP).
	FP string `json:"fp"`
	// HubRepo is omitted when the hub is private and the target public.
	HubRepo       string   `json:"hub_repo,omitempty"`
	DecidedAt     string   `json:"decided_at"`
	ContentCommit string   `json:"content_commit"`
	Base          string   `json:"base"`
	OptIn         string   `json:"optin"`
	Engine        string   `json:"engine"`
	Packs         []string `json:"packs"`
	Changes       []Change `json:"changes"`
	// ChangesComplete is false when Changes was dropped to fit the limits;
	// memory then matches only by the exact key.
	ChangesComplete bool     `json:"changes_complete"`
	TitleSet        string   `json:"title_set"`
	Body            string   `json:"body"` // sha256 of the last body touchmark wrote, marker excluded
	LabelsSet       []string `json:"labels_set"`
	Closed          *Closed  `json:"closed,omitempty"`
	Ack             bool     `json:"ack"`
	Revoked         bool     `json:"revoked"`
	RecreateFor     *string  `json:"recreate_for"`
}

// Marker is a parsed or to-be-written marker.
type Marker struct {
	// Header attributes. Hub and Stream repeat Data's; FP16 is FP16(Data.FP);
	// Key is "sha256:<64 hex>".
	Hub    string
	FP16   string
	Stream string
	Key    string
	Data   Data
}

// Syntax of the comment line.
const (
	// commentPrefix starts a touchmark comment of any version.
	commentPrefix = "<!-- touchmark:"
	// v1Prefix starts a version 1 marker, up to its first attribute.
	v1Prefix = "<!-- touchmark:v1 "
	// commentEnd closes the comment.
	commentEnd = " -->"
	// keyPrefix starts a content key.
	keyPrefix = "sha256:"
)

// attrNames are the header attributes, in the only order accepted.
var attrNames = [...]string{"hub", "fp", "stream", "key", "data"}

// streams are the stream names a marker may carry (decide.StreamSync and
// decide.StreamAdopt).
var streams = map[string]bool{"sync": true, "adopt": true}

var (
	// hubRe is the hub id syntax of hub.yml: lowercase words joined by single
	// hyphens, so an id never holds "--", which HTML comments forbid.
	hubRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// fingerprintRe is the fingerprint syntax of hub.yml
	// (previous_fingerprints): host, optional port, numeric repository id.
	fingerprintRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?/[0-9]+$`)
)

// errTooLarge marks an Encode failure caused by size alone: the marker does
// not fit the limits even without Changes.
var errTooLarge = errors.New("marker too large")

// FP16 returns the first 16 hex digits of sha256(fingerprint). The header
// carries this form because a host may contain "--", which HTML comments
// forbid.
func FP16(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:8])
}

// Encode returns the marker line in the comment frame (no trailing
// newline): EncodeFrame(m, FrameComment).
func Encode(m Marker) (string, error) { return EncodeFrame(m, FrameComment) }

// EncodeFrame returns the marker line in frame f (no trailing newline).
//
// It fills Hub, FP16 and Stream from Data, validates every field with the
// rules of Parse (and every string as valid UTF-8, which JSON needs to
// round-trip), and enforces the limits: when there are more than MaxChanges
// changes, the JSON exceeds MaxJSON or the line exceeds MaxLine, it drops
// Changes and sets ChangesComplete to false; if the JSON still exceeds
// MaxJSON or the line MaxLine it fails; the line is measured in frame f.
// Output is deterministic: JSON with empty arrays for nil slices, gzip at
// the best compression level without a timestamp or name, standard padded
// base64. Parse accepts every line EncodeFrame returns and yields the
// marker it encoded.
func EncodeFrame(m Marker, f Frame) (string, error) {
	m.Hub, m.Stream, m.FP16 = m.Data.Hub, m.Data.Stream, FP16(m.Data.FP)
	if err := checkHeader(m); err != nil {
		return "", fmt.Errorf("encode marker: %w", err)
	}
	if err := checkData(m); err != nil {
		return "", fmt.Errorf("encode marker: %w", err)
	}
	d := m.Data
	if len(d.Changes) > MaxChanges {
		d.Changes, d.ChangesComplete = nil, false
	}
	line, size, err := encodeLine(m, d)
	if err != nil {
		return "", fmt.Errorf("encode marker: %w", err)
	}
	line = frame(line, f)
	if (size > MaxJSON || len(line) > MaxLine) && len(d.Changes) > 0 {
		d.Changes, d.ChangesComplete = nil, false
		if line, size, err = encodeLine(m, d); err != nil {
			return "", fmt.Errorf("encode marker: %w", err)
		}
		line = frame(line, f)
	}
	switch {
	case size > MaxJSON:
		return "", fmt.Errorf("encode marker: %w: %d bytes of JSON without changes, more than %d", errTooLarge, size, MaxJSON)
	case len(line) > MaxLine:
		return "", fmt.Errorf("encode marker: %w: a line of %d bytes without changes, more than %d", errTooLarge, len(line), MaxLine)
	}
	return line, nil
}

// encodeLine renders the comment for the header of m and the payload d, and
// returns it with the size of the JSON.
func encodeLine(m Marker, d Data) (line string, size int, err error) {
	js, err := json.Marshal(wire(d))
	if err != nil {
		return "", 0, fmt.Errorf("marshal data: %w", err)
	}
	var z bytes.Buffer
	zw, err := newGzipWriter(&z)
	if err != nil {
		return "", 0, fmt.Errorf("compress data: %w", err)
	}
	defer gzipWriters.Put(zw)
	if _, err := zw.Write(js); err != nil {
		return "", 0, fmt.Errorf("compress data: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", 0, fmt.Errorf("compress data: %w", err)
	}
	var b strings.Builder
	b.Grow(len(v1Prefix) + len(m.Hub) + len(m.Key) + base64.StdEncoding.EncodedLen(z.Len()) + 64)
	b.WriteString(v1Prefix)
	values := [...]string{m.Hub, m.FP16, m.Stream, m.Key}
	for i, v := range values {
		b.WriteString(attrNames[i])
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte(' ')
	}
	b.WriteString(attrNames[len(values)])
	b.WriteByte('=')
	b.WriteString(base64.StdEncoding.EncodeToString(z.Bytes()))
	b.WriteString(commentEnd)
	return b.String(), len(js), nil
}

// wire returns d as it is written: nil slices become empty arrays.
func wire(d Data) Data {
	if d.Packs == nil {
		d.Packs = []string{}
	}
	if d.Changes == nil {
		d.Changes = []Change{}
	}
	if d.LabelsSet == nil {
		d.LabelsSet = []string{}
	}
	return d
}

// Parse parses one marker line strictly, in either frame: a reference
// definition is read as the comment it frames (see IsLine), its backslash
// escapes undone, and the comment is parsed. Strictly means: exact prefix
// and attribute order separated by single spaces, base64 that decodes
// (standard alphabet, padded, canonical), one gzip member without a name, comment, extra field
// or time and with nothing after it, that decompresses to at most MaxData
// bytes (read through a limited reader), UTF-8 JSON with exactly the keys of
// Data (compared case-sensitively, none repeated, no unknown fields, none
// missing but the optional hub_repo and closed; closed, when present, with
// both of its keys) and nothing after it, at most MaxChanges changes of
// exactly four strings,
// every string at most MaxString bytes, V == Version, Data.Stream == Stream
// ("sync" or "adopt"), Data.Hub == Hub (a hub id), Data.FP of the form
// host/id with FP16(Data.FP) == FP16, and Key of the form
// "sha256:<64 lowercase hex>". A change has a non-empty path, short blob
// ids that are "" or at most ShortOID lowercase hex digits, a mode that is
// "", "100644" or "100755", an empty mode exactly when To is empty, and
// From or To set. Empty arrays parse as nil slices. Error messages never
// quote more than a short prefix of the input.
func Parse(line string) (Marker, error) {
	m, err := parse(line)
	if err != nil {
		return Marker{}, fmt.Errorf("parse marker: %w", err)
	}
	return m, nil
}

func parse(line string) (Marker, error) {
	if !strings.HasPrefix(line, commentPrefix) {
		if len(line) > 2*MaxLine {
			return Marker{}, fmt.Errorf("the line is %d bytes, more than %d", len(line), 2*MaxLine)
		}
		if c, ok := asComment(line); ok {
			line = c
		}
	}
	if len(line) > MaxLine {
		return Marker{}, fmt.Errorf("the line is %d bytes, more than %d", len(line), MaxLine)
	}
	if len(line) < len(v1Prefix)+len(commentEnd) || !strings.HasPrefix(line, v1Prefix) || !strings.HasSuffix(line, commentEnd) {
		return Marker{}, fmt.Errorf("not a %q ... %q comment", v1Prefix, commentEnd)
	}
	fields := strings.Split(line[len(v1Prefix):len(line)-len(commentEnd)], " ")
	if len(fields) != len(attrNames) {
		return Marker{}, fmt.Errorf("want the attributes %s separated by single spaces", strings.Join(attrNames[:], ", "))
	}
	var values [len(attrNames)]string
	for i, f := range fields {
		v, ok := strings.CutPrefix(f, attrNames[i]+"=")
		if !ok {
			return Marker{}, fmt.Errorf("attribute %d must be %s=", i+1, attrNames[i])
		}
		values[i] = v
	}
	m := Marker{Hub: values[0], FP16: values[1], Stream: values[2], Key: values[3]}
	if err := checkHeader(m); err != nil {
		return Marker{}, err
	}
	js, err := decodeData(values[4])
	if err != nil {
		return Marker{}, err
	}
	d, err := unmarshalData(js)
	if err != nil {
		return Marker{}, err
	}
	m.Data = d
	if err := checkData(m); err != nil {
		return Marker{}, err
	}
	return m, nil
}

// decodeData returns the JSON inside the data attribute: base64, then one
// gzip member of at most MaxData decompressed bytes.
func decodeData(s string) ([]byte, error) {
	// The decoder skips CR and LF; the alphabet check refuses them.
	if s == "" || len(s)%4 != 0 || strings.IndexFunc(s, notBase64) >= 0 {
		return nil, errors.New("data is not padded standard base64")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("data is not padded standard base64: %w", err)
	}
	br := bytes.NewReader(raw)
	zr, err := newGzipReader(br)
	if err != nil {
		return nil, fmt.Errorf("data is not gzip: %w", err)
	}
	defer gzipReaders.Put(zr)
	zr.Multistream(false)
	if zr.Name != "" || zr.Comment != "" || zr.Extra != nil || !zr.ModTime.IsZero() {
		return nil, errors.New("data has a gzip name, comment, extra field or time")
	}
	js, err := io.ReadAll(io.LimitReader(zr, MaxData+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("data does not decompress: %w", err)
	case len(js) > MaxData:
		return nil, fmt.Errorf("data decompresses to more than %d bytes", MaxData)
	case br.Len() != 0:
		return nil, errors.New("data has bytes after the gzip stream")
	}
	return js, nil
}

// Compressors and decompressors are reused: a compressor at the best level
// allocates about a megabyte of tables and a decompressor its 32 KiB
// window.
var gzipWriters, gzipReaders sync.Pool

// newGzipWriter returns a writer to w at the best compression level, with
// the zero header: no name, comment or time.
func newGzipWriter(w io.Writer) (*gzip.Writer, error) {
	if zw, ok := gzipWriters.Get().(*gzip.Writer); ok {
		zw.Reset(w)
		return zw, nil
	}
	return gzip.NewWriterLevel(w, gzip.BestCompression)
}

// newGzipReader returns a reader of the gzip stream in r, its header read.
func newGzipReader(r io.Reader) (*gzip.Reader, error) {
	if zr, ok := gzipReaders.Get().(*gzip.Reader); ok {
		if err := zr.Reset(r); err != nil {
			gzipReaders.Put(zr)
			return nil, err
		}
		return zr, nil
	}
	return gzip.NewReader(r)
}

// notBase64 reports whether r is outside the padded standard base64
// alphabet.
func notBase64(r rune) bool {
	switch {
	case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z', '0' <= r && r <= '9', r == '+', r == '/', r == '=':
		return false
	}
	return true
}

// checkHeader validates the header attributes of m.
func checkHeader(m Marker) error {
	switch {
	case !hubRe.MatchString(m.Hub):
		return fmt.Errorf("hub %.40q is not a hub id (lowercase words joined by single hyphens)", m.Hub)
	case !isHex(m.FP16) || len(m.FP16) != 16:
		return fmt.Errorf("fp %.40q is not 16 lowercase hex digits", m.FP16)
	case !streams[m.Stream]:
		return fmt.Errorf("stream %.40q is neither sync nor adopt", m.Stream)
	}
	key, ok := strings.CutPrefix(m.Key, keyPrefix)
	if !ok || len(key) != 2*sha256.Size || !isHex(key) {
		return fmt.Errorf("key %.40q is not sha256:<64 lowercase hex digits>", m.Key)
	}
	return nil
}

// checkData validates m.Data against the header of m, which checkHeader
// accepted. It does not count changes: Encode drops too many, Parse refuses
// them.
func checkData(m Marker) error {
	d := m.Data
	switch {
	case d.V != Version:
		return fmt.Errorf("data v is %d, want %d", d.V, Version)
	case d.Stream != m.Stream:
		return fmt.Errorf("data stream %.40q differs from the header's %q", d.Stream, m.Stream)
	case d.Hub != m.Hub:
		return fmt.Errorf("data hub %.40q differs from the header's %.40q", d.Hub, m.Hub)
	case !fingerprintRe.MatchString(d.FP):
		return fmt.Errorf("data fp %.40q is not host/numeric-id", d.FP)
	case FP16(d.FP) != m.FP16:
		return fmt.Errorf("data fp %.40q does not hash to the header's fp %s", d.FP, m.FP16)
	}
	for _, s := range []struct{ name, value string }{
		{"stream", d.Stream},
		{"hub", d.Hub},
		{"fp", d.FP},
		{"hub_repo", d.HubRepo},
		{"decided_at", d.DecidedAt},
		{"content_commit", d.ContentCommit},
		{"base", d.Base},
		{"optin", d.OptIn},
		{"engine", d.Engine},
		{"title_set", d.TitleSet},
		{"body", d.Body},
	} {
		if err := checkString(s.name, s.value); err != nil {
			return err
		}
	}
	if err := checkStrings("packs", d.Packs); err != nil {
		return err
	}
	if err := checkStrings("labels_set", d.LabelsSet); err != nil {
		return err
	}
	for i, c := range d.Changes {
		if err := checkChange(c); err != nil {
			return fmt.Errorf("changes[%d]: %w", i, err)
		}
	}
	if d.Closed != nil {
		if err := checkString("closed.by", d.Closed.By); err != nil {
			return err
		}
		if err := checkString("closed.reason", d.Closed.Reason); err != nil {
			return err
		}
	}
	if d.RecreateFor != nil {
		if err := checkString("recreate_for", *d.RecreateFor); err != nil {
			return err
		}
	}
	return nil
}

// checkChange validates one change.
func checkChange(c Change) error {
	for _, s := range []struct{ name, value string }{
		{"path", c.Path}, {"from", c.From}, {"mode", c.Mode}, {"to", c.To},
	} {
		if err := checkString(s.name, s.value); err != nil {
			return err
		}
	}
	switch {
	case c.Path == "":
		return errors.New("empty path")
	case !isShortOID(c.From):
		return fmt.Errorf("from %.40q is not \"\" or up to %d lowercase hex digits", c.From, ShortOID)
	case !isShortOID(c.To):
		return fmt.Errorf("to %.40q is not \"\" or up to %d lowercase hex digits", c.To, ShortOID)
	case c.Mode != "" && c.Mode != "100644" && c.Mode != "100755":
		return fmt.Errorf("mode %.40q is not \"\", 100644 or 100755", c.Mode)
	case (c.Mode == "") != (c.To == ""):
		return errors.New("mode and to must both be empty (a deletion) or both be set")
	case c.From == "" && c.To == "":
		return errors.New("from and to are both empty")
	}
	return nil
}

// checkStrings bounds every string of the array name.
func checkStrings(name string, ss []string) error {
	for i, s := range ss {
		if len(s) > MaxString || !utf8.ValidString(s) {
			return checkString(fmt.Sprintf("%s[%d]", name, i), s)
		}
	}
	return nil
}

// checkString bounds one string of the payload.
func checkString(name, s string) error {
	if len(s) > MaxString {
		return fmt.Errorf("%s is %d bytes, more than %d", name, len(s), MaxString)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	return nil
}

// isShortOID reports whether s is "" or at most ShortOID lowercase hex
// digits.
func isShortOID(s string) bool { return s == "" || (len(s) <= ShortOID && isHex(s)) }

// isHex reports whether s is non-empty lowercase hex.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
