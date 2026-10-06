package distribute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The report stream (Write.Stream, touchmark-report.jsonl) receives one line
// per finished target as the run goes, so that it survives a run killed half
// way. Each line is one JSON object (streamLine), written with one Write
// call and flushed when the stream can be flushed.
//
// Who writes which line: execute streams the target of every work it gets,
// once the last work of that target is done (whatever its outcome: written,
// deferred, failed), with the mutations made to it; the run streams every
// other target itself (run.stream), once its outcome is final (after phase
// E). No target is streamed twice. Both share run.mu and run.streamErr: after
// the first failed write no line is written.

// streamLine is one line of the report stream: a target's report entry and
// the mutations touchmark made to it (none for plan and dry runs).
type streamLine struct {
	Target report.DeliveryTarget `json:"target"`
	Ops    []report.Op           `json:"ops"`
}

// writeStreamLine writes t and its ops to w as one line of JSON, with every
// secret of reg masked in the target's warnings (platform messages may quote
// one). A target without a path, which a public hub does not name, is
// written without warnings: they may name it. The caller serializes writes
// to w.
func writeStreamLine(w io.Writer, reg *redact.Registry, t report.DeliveryTarget, ops []report.Op) error {
	t.Warnings = slices.Clone(t.Warnings)
	if t.Path == "" {
		t.Warnings = nil
	}
	for i, s := range t.Warnings {
		t.Warnings[i] = reg.Replace(s)
	}
	if ops == nil {
		ops = []report.Op{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(streamLine{Target: t, Ops: ops}); err != nil {
		return fmt.Errorf("report stream: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("report stream: %w", err)
	}
	return nil
}

// stream writes the line of target t, with the ops made to it, to
// Write.Stream, if any, and flushes it. A failure is one run warning; the
// run goes on without the stream.
func (ex *executor) stream(t *target, ops []report.Op) {
	r := ex.r
	w := r.d.Write.Stream
	if w == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streamErr != nil {
		return
	}
	r.streamErr = writeStreamLine(w, r.d.Write.Redact, t.res, ops)
	if f, ok := w.(interface{ Flush() error }); ok && r.streamErr == nil {
		r.streamErr = f.Flush()
	}
	if r.streamErr != nil {
		r.rep.Warnings = append(r.rep.Warnings, fmt.Sprintf("%v; the stream misses the targets after it", r.streamErr))
	}
}
