package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/pathx"
)

// Output formats.
const (
	formatText     = "text"
	formatJSON     = "json"
	formatMarkdown = "markdown"
)

// errHelp is returned by flagSet.parse for -h and --help.
var errHelp = flag.ErrHelp

// options holds every flag value; each command binds the ones it takes.
type options struct {
	hub      string
	dir      string
	repo     string
	packs    packList
	adopt    globList
	worktree bool
	dryRun   bool
	format   string
	// assumeOptIn is status's and apply's --assume-opt-in (target.assume).
	assumeOptIn bool
	// only, hubFP and strict are plan's (plan.go).
	only   refList
	hubFP  fingerprintFlag
	strict bool
	// args holds the positional arguments.
	args []string
}

// flagSet is a flag.FlagSet with the positional arguments a command takes.
// The standard flag package accepts both -flag and --flag.
type flagSet struct {
	fs *flag.FlagSet
	o  *options
	// nargs is the exact number of positional arguments, and argName their
	// name in messages.
	nargs   int
	argName string
	// formats are the values --format accepts; text and json when nil.
	formats []string
	// leading is set when flags may follow the positional arguments
	// (setup github --hub DIR): the standard flag package stops at the
	// first positional argument.
	leading bool
}

func newFlagSet(name string) (*flagSet, *options) {
	o := &options{format: formatText}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{fs: fs, o: o}, o
}

// parse parses args and checks the positional arguments and the format.
func (f *flagSet) parse(args []string) error {
	var positional []string
	for {
		if err := f.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return errHelp
			}
			return usageErrorf("%v", err)
		}
		rest := f.fs.Args()
		// The standard flag package stops at the first positional argument;
		// a command with leading set takes flags after it too.
		if !f.leading || len(rest) == 0 {
			positional = append(positional, rest...)
			break
		}
		positional, args = append(positional, rest[0]), rest[1:]
		if len(args) == 0 {
			break
		}
	}
	f.o.args = positional
	switch {
	case len(f.o.args) > f.nargs:
		return usageErrorf("unexpected argument %q", f.o.args[f.nargs])
	case len(f.o.args) < f.nargs:
		return usageErrorf("missing %s", f.argName)
	}
	formats := f.formats
	if formats == nil {
		formats = []string{formatText, formatJSON}
	}
	if !slices.Contains(formats, f.o.format) {
		return usageErrorf("--format: %q is not %s", f.o.format, orList(formats))
	}
	return nil
}

// orList joins items as "a, b or c".
func orList(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// printDefaults prints the flags as "--name VALUE  usage" lines, with the
// default of a flag that has one, unless its usage already says it: a
// boolean that is on unless turned off shows (default true).
func (f *flagSet) printDefaults(w io.Writer) {
	type line struct{ left, usage string }
	var lines []line
	width := 0
	f.fs.VisitAll(func(fl *flag.Flag) {
		name, usage := flag.UnquoteUsage(fl)
		if def := shortDefault(fl.DefValue); def != "" && !strings.Contains(usage, "(default") {
			usage += " (default " + def + ")"
		}
		left := "--" + fl.Name
		if name != "" {
			left += " " + name
		}
		width = max(width, len(left))
		lines = append(lines, line{left, usage})
	})
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "\nFlags:")
	for _, l := range lines {
		fmt.Fprintf(w, "  %-*s  %s\n", width, l.left, l.usage)
	}
}

// shortDefault returns a flag's default as help shows it: "" for a zero
// value, which needs no mention, and durations without trailing zero units
// (30m, not 30m0s).
func shortDefault(def string) string {
	switch def {
	case "", "false", "0", "0s", "[]":
		return ""
	}
	if strings.HasSuffix(def, "m0s") {
		def = strings.TrimSuffix(def, "0s")
		if strings.HasSuffix(def, "h0m") {
			def = strings.TrimSuffix(def, "0m")
		}
	}
	return def
}

func (f *flagSet) hubFlags() {
	f.fs.StringVar(&f.o.hub, "hub", "", "the hub checkout `DIR` (default $TOUCHMARK_HUB)")
	f.fs.BoolVar(&f.o.worktree, "worktree", false, "read configs and packs from the hub's working tree instead of its HEAD commit")
}

func (f *flagSet) formatFlag() {
	f.fs.StringVar(&f.o.format, "format", formatText, "output `FORMAT`: text or json")
}

func (f *flagSet) targetFlags() {
	f.fs.StringVar(&f.o.dir, "dir", "", "the target working tree `DIR` (default: the current directory)")
	f.fs.StringVar(&f.o.repo, "repo", "", "the target in targets.yml, as `OWNER/NAME` or PROVIDER:PATH (default: from the origin remote)")
	f.fs.Var(&f.o.packs, "packs", "comma-separated `PACKS` to apply instead of the selection from targets.yml and the opt-in file")
	f.fs.BoolVar(&f.o.assumeOptIn, "assume-opt-in", false,
		"count the target as opted in when it has no opt-in file, as an org or group entry with opt_in: assumed that a local run cannot resolve would")
}

func checkFlags() (*flagSet, *options) {
	f, o := newFlagSet("check")
	f.hubFlags()
	f.formatFlag()
	return f, o
}

func statusFlags() (*flagSet, *options) {
	f, o := newFlagSet("status")
	f.hubFlags()
	f.targetFlags()
	f.formatFlag()
	return f, o
}

func applyFlags() (*flagSet, *options) {
	f, o := newFlagSet("apply")
	f.hubFlags()
	f.targetFlags()
	f.formatFlag()
	f.fs.Var(&o.adopt, "adopt", "overwrite local files matching `GLOB` with the pack version (repeatable)")
	f.fs.BoolVar(&o.dryRun, "dry-run", false, "print what apply would change without writing")
	return f, o
}

func manifestFlags() (*flagSet, *options) {
	f, o := newFlagSet("manifest")
	f.fs.StringVar(&o.hub, "hub", "", "the hub checkout `DIR` (default $TOUCHMARK_HUB)")
	f.fs.StringVar(&o.format, "format", formatJSON, "output `FORMAT`: json, or text, which prints the same JSON")
	return f, o
}

func schemaFlags() (*flagSet, *options) {
	f, o := newFlagSet("schema")
	f.nargs, f.argName = 1, "schema name: hub, targets, opt-in, operations, report, doctor or setup"
	return f, o
}

func versionFlags() (*flagSet, *options) {
	return newFlagSet("version")
}

// packList is the value of --packs: comma-separated names; the flag may
// repeat. set records that the flag was given at all.
type packList struct {
	names []string
	set   bool
}

func (l *packList) String() string { return strings.Join(l.names, ",") }

func (l *packList) Set(s string) error {
	l.set = true
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("empty pack name")
		}
		l.names = append(l.names, name)
	}
	return nil
}

// globList is the value of a repeatable glob flag (--adopt).
type globList []string

func (l *globList) String() string { return strings.Join(*l, " ") }

func (l *globList) Set(s string) error {
	if pathx.NormalizePattern(s) == "" {
		return fmt.Errorf("pattern %q is empty", s)
	}
	*l = append(*l, s)
	return nil
}
