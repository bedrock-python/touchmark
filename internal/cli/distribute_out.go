package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The outputs of a run in CI:
//   - GitHub Actions: the step summary ($GITHUB_STEP_SUMMARY, at most
//     1 MiB for the step), workflow commands (::error, ::warning) for the
//     first failed and blocked targets, and the report files for the
//     artifact;
//   - GitLab CI: the report files, which the hub template's job exposes
//     (artifacts:expose_as);
//   - Gitea Actions: the step summary (Gitea 1.27 with runner 2.0 shows it)
//     and the report files;
//   - Forgejo Actions: the log and the report files (Forgejo shows no step
//     summary).
//
// The report files are <name>.json and <name>.md in the working directory:
// touchmark-report.{json,md} for distribute (next to the report stream
// touchmark-report.jsonl), touchmark-doctor.{json,md} for doctor. Every
// output goes through the run's masking writer, and a target a public hub
// does not name is named in none of them: the report's renderers leave it
// out, and the annotations skip it.

// Names of the report files a CI run leaves for the artifact.
const (
	deliveryFiles = "touchmark-report"
	doctorFiles   = "touchmark-doctor"
)

// maxAnnotations is how many failed and how many blocked targets a GitHub
// Actions run annotates.
const maxAnnotations = 10

// ciOutputs says which outputs a run leaves in its CI.
type ciOutputs struct {
	// summary is the step summary file ("" for none).
	summary string
	// files is set in CI: the report files for the artifact.
	files bool
	// annotate is set on GitHub Actions: workflow commands for the first
	// problems.
	annotate bool
}

// ciOutputsOf returns the outputs of a run in context hctx.
func ciOutputsOf(hctx hubch.Context, getenv func(string) string) ciOutputs {
	var o ciOutputs
	if !inCI(hctx, getenv) {
		return o
	}
	o.files = true
	switch hctx.CI {
	case hubch.GitHubActions:
		o.annotate = true
		o.summary = strings.TrimSpace(getenv("GITHUB_STEP_SUMMARY"))
	case hubch.GiteaActions:
		o.summary = strings.TrimSpace(getenv("GITHUB_STEP_SUMMARY"))
	}
	return o
}

// renderers are the renderings of one report.
type renderers struct {
	print    func(io.Writer) error      // stdout, in the chosen format
	json     func(io.Writer) error      // the JSON report
	markdown func(io.Writer) error      // the Markdown report
	summary  func(io.Writer, int) error // the step summary, within a limit
	// annotations are the workflow commands of GitHub Actions ("" for none).
	annotations func() string
}

// publish prints the report through the masking writer, writes the JSON
// report to reportFile, and in CI the report files named base, the step
// summary and the annotations (ciOutputsOf). The annotations go to stderr:
// stdout holds the report, which --format json keeps parseable.
func publish(e *env, hctx hubch.Context, reg *redact.Registry, r renderers, base, reportFile string) error {
	out := reg.Writer(e.stdout)
	if err := errors.Join(r.print(out), out.Flush()); err != nil {
		return err
	}
	ci := ciOutputsOf(hctx, e.getenv)
	if ci.annotate && r.annotations != nil {
		if s := r.annotations(); s != "" {
			w := reg.Writer(e.stderr)
			_, err := io.WriteString(w, s)
			if err := errors.Join(err, w.Flush()); err != nil {
				return err
			}
		}
	}
	if reportFile != "" {
		if err := writeReportFile(reg, reportFile, r.json, false); err != nil {
			return fmt.Errorf("--report: %w", err)
		}
	}
	var errs []error
	if ci.files {
		if err := writeReportFile(reg, base+".json", r.json, false); err != nil {
			errs = append(errs, fmt.Errorf("the report file %s.json: %w", base, err))
		}
		if err := writeReportFile(reg, base+".md", r.markdown, false); err != nil {
			errs = append(errs, fmt.Errorf("the report file %s.md: %w", base, err))
		}
	}
	if ci.summary != "" {
		if err := writeSummary(reg, ci.summary, r.summary); err != nil {
			errs = append(errs, fmt.Errorf("the step summary: %w", err))
		}
	}
	return errors.Join(errs...)
}

// publishDelivery publishes the report of distribute (publish) and returns
// its exit code as an error (nil for 0).
func publishDelivery(e *env, hctx hubch.Context, reg *redact.Registry, rep *report.Delivery, format, reportFile string) error {
	err := publish(e, hctx, reg, renderers{
		print:       func(w io.Writer) error { return printDelivery(w, rep, format) },
		json:        func(w io.Writer) error { return report.WriteJSON(w, rep) },
		markdown:    rep.WriteMarkdown,
		summary:     rep.WriteSummary,
		annotations: func() string { return annotations(rep) },
	}, deliveryFiles, reportFile)
	if err != nil {
		return err
	}
	if code := rep.ExitCode(); code != exitOK {
		return exitWith(code)
	}
	return nil
}

// writeReportFile writes a rendering of the report to the file name through
// the masking writer: truncated, or appended to with add.
func writeReportFile(reg *redact.Registry, name string, render func(io.Writer) error, add bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if add {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(name, flags, 0o644)
	if err != nil {
		return err
	}
	w := reg.Writer(f)
	err = render(w)
	return errors.Join(err, w.Flush(), f.Close())
}

// writeSummary appends the step summary to the file name, within what is
// left of the step's 1 MiB: a job step may have written to it already, and
// a summary over the limit is refused whole. Masking replaces secrets with
// "***", never longer than a secret's shortest form, so the masked text
// fits too.
func writeSummary(reg *redact.Registry, name string, render func(io.Writer, int) error) error {
	used := int64(0)
	if fi, err := os.Stat(name); err == nil {
		used = fi.Size()
	}
	left := report.MaxSummary - int(min(used, int64(report.MaxSummary)))
	return writeReportFile(reg, name, func(w io.Writer) error { return render(w, left) }, true)
}

// annotations returns the GitHub Actions workflow commands for the first
// failed targets (::error) and the first blocked ones (::warning), with
// their reason and first warning. Targets a public hub does not name are
// never named.
func annotations(rep *report.Delivery) string {
	var b strings.Builder
	counts := map[report.Outcome]int{}
	for _, t := range rep.Targets {
		level := ""
		switch t.Outcome {
		case report.OutcomeFailed:
			level = "error"
		case report.OutcomeBlocked:
			level = "warning"
		default:
			continue
		}
		if t.Path == "" || counts[t.Outcome] == maxAnnotations {
			continue
		}
		counts[t.Outcome]++
		msg := t.Provider + ":" + t.Path + " " + string(t.Outcome)
		if t.Reason != "" {
			msg += ":" + t.Reason
		}
		if len(t.Warnings) > 0 {
			msg += ": " + t.Warnings[0]
		}
		b.WriteString(annotation(level, msg))
	}
	return b.String()
}

// annotation is one workflow command of level with msg as its message.
func annotation(level, msg string) string {
	return "::" + level + " title=touchmark::" + annotationEscaper.Replace(msg) + "\n"
}

// annotationEscaper escapes the message of a workflow command.
var annotationEscaper = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
