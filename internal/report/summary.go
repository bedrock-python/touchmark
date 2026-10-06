package report

import (
	"fmt"
	"io"
)

// MaxSummary is the size limit of a GitHub Actions step summary: GitHub
// refuses a step's summary over 1 MiB (it shows an error instead), and so
// does Gitea's runner, which follows it.
const MaxSummary = maxMarkdown

// minSummary is the least room WriteSummary writes into: below it, a cut
// report would be little more than its note.
const minSummary = 4 << 10

// summaryNote ends a report cut to fit a CI summary.
const summaryNote = "\n*This summary is cut to fit its limit; the JSON report lists everything.*\n"

// WriteSummary renders the Markdown report of WriteMarkdown for a CI step
// summary into at most limit bytes: lines that do not fit are left out
// whole, an open <details> section is closed, and a note says the report is
// cut. With less room than 4 KiB it writes nothing.
func (d *Delivery) WriteSummary(w io.Writer, limit int) error {
	if limit < minSummary {
		return nil
	}
	m := &mdWriter{limit: limit, note: summaryNote}
	d.markdown(m)
	if _, err := io.WriteString(w, m.finish()); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// WriteSummary renders the Markdown report of WriteMarkdown for a CI step
// summary into at most limit bytes, as Delivery.WriteSummary does.
func (d *Doctor) WriteSummary(w io.Writer, limit int) error {
	if limit < minSummary {
		return nil
	}
	return d.writeMarkdown(w, &mdWriter{limit: limit, note: summaryNote})
}
