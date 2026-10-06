package marker

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseSample(t *testing.T) {
	m := sampleMarker()
	got, err := Parse(mustEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	equalMarkers(t, got, written(m))
	if got.Data.Changes[1] != (Change{Path: "prompts/old.md", From: "9a01bc23de45f678"}) {
		t.Errorf("deletion = %+v", got.Data.Changes[1])
	}
}

// TestParseAcceptsEquivalentForms covers what strictness still allows:
// JSON formatting, null arrays, a null or absent closed, an absent hub_repo
// (the only optional keys) and any gzip level.
func TestParseAcceptsEquivalentForms(t *testing.T) {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(sampleJSON(t)), "", "  "); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"pretty JSON": lineWithJSON(t, " \n"+pretty.String()+"\n "),
		"null arrays": lineWithJSON(t, jsonWith(t, func(o map[string]any) { o["packs"], o["changes"], o["labels_set"] = nil, nil, nil })),
		"null closed": lineWithJSON(t, jsonWith(t, func(o map[string]any) { o["closed"] = nil })),
		"no hub_repo": lineWithJSON(t, jsonWith(t, func(o map[string]any) { delete(o, "hub_repo") })),
		"no closed":   lineWithJSON(t, jsonWith(t, func(o map[string]any) { delete(o, "closed") })),
		"fast gzip":   lineWithData(gzipWith(t, []byte(sampleJSON(t)), gzip.BestSpeed, nil)),
		"escaped key": lineWithJSON(t, strings.Replace(sampleJSON(t), `"engine"`, `"`+uEsc("0065")+`ngine"`, 1)),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(line); err != nil {
				t.Fatalf("Parse: %v", err)
			}
		})
	}
}

// gzipWith compresses b at level; edit may set header fields.
func gzipWith(t testing.TB, b []byte, level int, edit func(*gzip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(zw)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseRejectsHeader(t *testing.T) {
	good := mustEncode(t, sampleMarker())
	data := good[strings.LastIndex(good, " data="):] // " data=… -->"
	hdr := func(hub, fp, stream, key string) string {
		return v1Prefix + "hub=" + hub + " fp=" + fp + " stream=" + stream + " key=" + key + data
	}
	fp := FP16(ourFP)
	cases := map[string]string{
		"empty":             "",
		"too long":          good[:len(good)-len(commentEnd)] + strings.Repeat("A", MaxLine) + commentEnd,
		"other version":     strings.Replace(good, "touchmark:v1 ", "touchmark:v2 ", 1),
		"version 10":        strings.Replace(good, "touchmark:v1 ", "touchmark:v10 ", 1),
		"no space in open":  strings.Replace(good, "<!-- ", "<!--", 1),
		"no close":          strings.TrimSuffix(good, commentEnd),
		"tight close":       strings.TrimSuffix(good, commentEnd) + "-->",
		"trailing CR":       good + "\r",
		"trailing space":    good + " ",
		"leading space":     " " + good,
		"prefix and suffix": "<!-- touchmark:v1 -->",
		"only prefix":       "<!-- touchmark:v1  -->",
		"double space":      strings.Replace(good, " fp=", "  fp=", 1),
		"tab separator":     strings.Replace(good, " fp=", "\tfp=", 1),
		"swapped":           v1Prefix + "fp=" + fp + " hub=acme-eng stream=sync key=" + key1 + data,
		"missing attribute": v1Prefix + "hub=acme-eng fp=" + fp + " key=" + key1 + data,
		"extra attribute":   strings.Replace(good, " data=", " extra=1 data=", 1),
		"attribute case":    strings.Replace(good, " fp=", " FP=", 1),
		"hub uppercase":     hdr("Acme-eng", fp, "sync", key1),
		"hub double hyphen": hdr("acme--eng", fp, "sync", key1),
		"hub edge hyphen":   hdr("-acme", fp, "sync", key1),
		"hub empty":         hdr("", fp, "sync", key1),
		"fp uppercase":      hdr("acme-eng", strings.ToUpper(fp), "sync", key1),
		"fp short":          hdr("acme-eng", fp[:15], "sync", key1),
		"fp long":           hdr("acme-eng", fp+"0", "sync", key1),
		"fp not hex":        hdr("acme-eng", "github.com/71234", "sync", key1),
		"stream unknown":    hdr("acme-eng", fp, "other", key1),
		"stream case":       hdr("acme-eng", fp, "Sync", key1),
		"key algorithm":     hdr("acme-eng", fp, "sync", "sha1:"+strings.Repeat("a", 40)),
		"key uppercase":     hdr("acme-eng", fp, "sync", strings.ToUpper(key1)),
		"key short":         hdr("acme-eng", fp, "sync", key1[:len(key1)-1]),
		"key bare":          hdr("acme-eng", fp, "sync", strings.TrimPrefix(key1, "sha256:")),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if m, err := Parse(line); err == nil {
				t.Fatalf("Parse accepted it: %s", describeMarker(m))
			}
		})
	}
}

func TestParseRejectsData(t *testing.T) {
	js := []byte(sampleJSON(t))
	raw := gz(t, js)
	// Padding needs a length that is not a multiple of 3: add JSON spaces.
	for len(raw)%3 == 0 {
		js = append(js, ' ')
		raw = gz(t, js)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	withData := func(s string) string { return header() + s + commentEnd }
	if _, err := Parse(withData(b64)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// The last base64 digit before "=" carries unused bits: set one.
	nonCanonical := func() string {
		i := strings.IndexByte(b64, '=') - 1
		const digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		d := strings.IndexByte(digits, b64[i])
		return b64[:i] + string(digits[d|1]) + b64[i+1:]
	}()
	corrupt := bytes.Clone(raw)
	corrupt[len(corrupt)-5] ^= 0xff // inside the CRC-32
	cases := map[string]string{
		"empty data":           withData(""),
		"URL alphabet":         withData("-" + b64[1:]),
		"unpadded":             withData(strings.TrimRight(b64, "=")),
		"non-canonical bits":   withData(nonCanonical),
		"CRLF inside":          withData(b64[:8] + "\r\n\r\n" + b64[8:]),
		"LF inside":            withData(b64[:8] + "\n\n\n\n" + b64[8:]),
		"not gzip":             withData(base64.StdEncoding.EncodeToString(js)),
		"truncated gzip":       lineWithData(raw[:len(raw)-9]),
		"bad checksum":         lineWithData(corrupt),
		"bytes after gzip":     lineWithData(append(bytes.Clone(raw), 0)),
		"two gzip members":     lineWithData(append(bytes.Clone(raw), gz(t, []byte(" "))...)),
		"gzip name":            lineWithData(gzipWith(t, js, gzip.BestCompression, func(z *gzip.Writer) { z.Name = "data.json" })),
		"gzip comment":         lineWithData(gzipWith(t, js, gzip.BestCompression, func(z *gzip.Writer) { z.Comment = "hi" })),
		"gzip extra":           lineWithData(gzipWith(t, js, gzip.BestCompression, func(z *gzip.Writer) { z.Extra = []byte{} })),
		"gzip time":            lineWithData(gzipWith(t, js, gzip.BestCompression, func(z *gzip.Writer) { z.ModTime = time.Unix(1767225600, 0) })),
		"over MaxData":         lineWithJSON(t, string(js)+strings.Repeat(" ", MaxData)),
		"invalid UTF-8":        lineWithJSON(t, strings.Replace(string(js), "chore:", "chore\xff:", 1)),
		"unknown key":          lineWithJSON(t, jsonWith(t, func(o map[string]any) { o["extra"] = 1 })),
		"key case":             lineWithJSON(t, strings.Replace(string(js), `"v":1`, `"V":1`, 1)),
		"key case only":        lineWithJSON(t, strings.Replace(string(js), `"engine"`, `"Engine"`, 1)),
		"repeated key":         lineWithJSON(t, strings.Replace(string(js), `"v":1,`, `"v":1,"v":1,`, 1)),
		"repeated key escaped": lineWithJSON(t, strings.Replace(string(js), `"v":1,`, `"v":1,"`+uEsc("0076")+`":1,`, 1)),
		"content after":        lineWithJSON(t, string(js)+" {}"),
		"garbage after":        lineWithJSON(t, string(js)+"x"),
		"array":                lineWithJSON(t, "["+string(js)+"]"),
		"null":                 lineWithJSON(t, "null"),
		"string":               lineWithJSON(t, `"x"`),
		"empty object":         lineWithJSON(t, "{}"),
		"empty JSON":           lineWithJSON(t, ""),
		"truncated JSON":       lineWithJSON(t, string(js[:len(js)-1])),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if len(line) > MaxLine {
				t.Fatalf("fixture: line of %d bytes", len(line))
			}
			if m, err := Parse(line); err == nil {
				t.Fatalf("Parse accepted it: %s", describeMarker(m))
			}
		})
	}
}

func TestParseRejectsPayload(t *testing.T) {
	tooLong := strings.Repeat("x", MaxString+1)
	change := func(c ...any) func(map[string]any) {
		return func(o map[string]any) { o["changes"] = []any{c} }
	}
	cases := map[string]func(map[string]any){
		"v two":                  func(o map[string]any) { o["v"] = 2 },
		"v missing":              func(o map[string]any) { delete(o, "v") },
		"v string":               func(o map[string]any) { o["v"] = "1" },
		"v fraction":             func(o map[string]any) { o["v"] = 1.5 },
		"stream differs":         func(o map[string]any) { o["stream"] = "adopt" },
		"stream missing":         func(o map[string]any) { delete(o, "stream") },
		"hub differs":            func(o map[string]any) { o["hub"] = "acme" },
		"fp not ours":            func(o map[string]any) { o["fp"] = otherFP },
		"fp missing":             func(o map[string]any) { delete(o, "fp") },
		"fp not host/id":         func(o map[string]any) { o["fp"] = "github.com" },
		"fp number":              func(o map[string]any) { o["fp"] = 712345678 },
		"hub_repo too long":      func(o map[string]any) { o["hub_repo"] = tooLong },
		"decided_at too long":    func(o map[string]any) { o["decided_at"] = tooLong },
		"content_commit long":    func(o map[string]any) { o["content_commit"] = tooLong },
		"base too long":          func(o map[string]any) { o["base"] = tooLong },
		"optin too long":         func(o map[string]any) { o["optin"] = tooLong },
		"engine too long":        func(o map[string]any) { o["engine"] = tooLong },
		"title too long":         func(o map[string]any) { o["title_set"] = tooLong },
		"body too long":          func(o map[string]any) { o["body"] = tooLong },
		"pack too long":          func(o map[string]any) { o["packs"] = []any{"a", tooLong} },
		"label too long":         func(o map[string]any) { o["labels_set"] = []any{tooLong} },
		"closed.by too long":     func(o map[string]any) { o["closed"] = map[string]any{"by": tooLong, "reason": "no-diff"} },
		"closed.reason too long": func(o map[string]any) { o["closed"] = map[string]any{"by": "touchmark", "reason": tooLong} },
		"recreate_for too long":  func(o map[string]any) { o["recreate_for"] = tooLong },
		"change path too long":   change(tooLong, "", "100644", "1"),
		"packs not array":        func(o map[string]any) { o["packs"] = "agents" },
		"pack not string":        func(o map[string]any) { o["packs"] = []any{1} },
		"changes not array":      func(o map[string]any) { o["changes"] = map[string]any{} },
		"change three strings":   change("a", "", "100644"),
		"change five strings":    change("a", "", "100644", "1", "x"),
		"change number":          change("a", "", "100644", 1),
		"change null element":    change("a", nil, "100644", "1"),
		"change null":            func(o map[string]any) { o["changes"] = []any{nil} },
		"change object":          func(o map[string]any) { o["changes"] = []any{map[string]any{"path": "a"}} },
		"change empty path":      change("", "", "100644", "1"),
		"change from uppercase":  change("a", "ABC", "100644", "1"),
		"change from too long":   change("a", "0123456789abcdef0", "100644", "1"),
		"change to not hex":      change("a", "", "100644", "g"),
		"change mode 000000":     change("a", "1", "000000", ""),
		"change mode gitlink":    change("a", "", "160000", "1"),
		"change mode, no to":     change("a", "1", "100644", ""),
		"change to, no mode":     change("a", "1", "", "2"),
		"change nothing":         change("a", "", "", ""),
		"closed unknown key": func(o map[string]any) {
			o["closed"] = map[string]any{"by": "touchmark", "reason": "no-diff", "at": "x"}
		},
		"closed key case":        func(o map[string]any) { o["closed"] = map[string]any{"By": "touchmark", "reason": "no-diff"} },
		"closed not object":      func(o map[string]any) { o["closed"] = "no-diff" },
		"closed without reason":  func(o map[string]any) { o["closed"] = map[string]any{"by": "touchmark"} },
		"closed empty":           func(o map[string]any) { o["closed"] = map[string]any{} },
		"recreate_for number":    func(o map[string]any) { o["recreate_for"] = 5 },
		"ack string":             func(o map[string]any) { o["ack"] = "true" },
		"changes_complete array": func(o map[string]any) { o["changes_complete"] = []any{} },
		// Encode writes every key but hub_repo and closed: a marker without
		// one was not written whole (a complete but empty set of changes
		// would otherwise silence memory).
		"engine missing":       func(o map[string]any) { delete(o, "engine") },
		"changes missing":      func(o map[string]any) { delete(o, "changes") },
		"optin missing":        func(o map[string]any) { delete(o, "optin") },
		"labels_set missing":   func(o map[string]any) { delete(o, "labels_set") },
		"recreate_for missing": func(o map[string]any) { delete(o, "recreate_for") },
		"only the header keys": func(o map[string]any) {
			for k := range o {
				if k != "v" && k != "stream" && k != "hub" && k != "fp" && k != "changes_complete" {
					delete(o, k)
				}
			}
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			line := lineWithJSON(t, jsonWith(t, edit))
			if m, err := Parse(line); err == nil {
				t.Fatalf("Parse accepted it: %s", describeMarker(m))
			}
		})
	}
}

// TestParseRejectsRepeatedClosedKey needs raw JSON: a map cannot repeat a key.
func TestParseRejectsRepeatedClosedKey(t *testing.T) {
	js := strings.Replace(sampleJSON(t), `"ack":`, `"closed":{"by":"touchmark","by":"x","reason":"no-diff"},"ack":`, 1)
	if _, err := Parse(lineWithJSON(t, js)); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("Parse = %v, want a repeated-key error", err)
	}
}

// TestParseMaxChanges calls the payload decoder directly: no payload within
// MaxData can hold more than MaxChanges changes, so the bound is a second
// line of defence.
func TestParseMaxChanges(t *testing.T) {
	build := func(n int) []byte {
		var b strings.Builder
		b.WriteString(strings.TrimSuffix(jsonWith(t, func(o map[string]any) { delete(o, "changes") }), "}"))
		b.WriteString(`,"changes":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `["%d","1","",""]`, i)
		}
		b.WriteString("]}")
		return []byte(b.String())
	}
	if d, err := unmarshalData(build(MaxChanges)); err != nil || len(d.Changes) != MaxChanges {
		t.Fatalf("MaxChanges changes: %d, %v", len(d.Changes), err)
	}
	if _, err := unmarshalData(build(MaxChanges + 1)); err == nil {
		t.Fatal("MaxChanges+1 changes accepted")
	}
}

// TestParseGzipBomb feeds data that inflates to 10 MiB from a line under
// MaxLine: Parse must stop at MaxData without allocating the rest.
func TestParseGzipBomb(t *testing.T) {
	bomb := gz(t, bytes.Repeat([]byte{' '}, 10<<20))
	line := lineWithData(bomb)
	if len(line) > MaxLine {
		t.Fatalf("fixture: line of %d bytes", len(line))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	_, err := Parse(line)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d bytes", MaxData)) {
		t.Fatalf("Parse = %v, want a decompressed-size error", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 2<<20 {
		t.Errorf("Parse allocated %d bytes for a %d-byte line, want it bounded by MaxData", alloc, len(line))
	}
	if elapsed > 5*time.Second {
		t.Errorf("Parse took %v", elapsed)
	}
}

func TestParseErrorsAreShort(t *testing.T) {
	long := strings.Repeat("h", 10000) + "X"
	line := v1Prefix + "hub=" + long + " fp=" + FP16(ourFP) + " stream=sync key=" + key1 + " data=AAAA" + commentEnd
	_, err := Parse(line)
	if err == nil || len(err.Error()) > 300 {
		t.Fatalf("Parse error of %d bytes: %.300v", len(fmt.Sprint(err)), err)
	}
}
