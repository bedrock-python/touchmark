// Package cli implements the touchmark command line.
//
// Commands:
//
//	touchmark check    [--hub DIR] [--worktree] [--format text|json]
//	touchmark plan     [--hub DIR] [--worktree] [--only REF]... [--hub-fp HOST/ID] [--strict] [--format text|json|markdown]
//	touchmark distribute [--hub DIR] [--dry-run] [--only REF]... [--hub-fp HOST/ID] [--deadline DURATION] [--strict]
//	                   [--format text|json|markdown] [--report FILE] [--stream FILE] [local operation flags]
//	touchmark doctor   [--hub DIR] [--only REF]... [--hub-fp HOST/ID] [--strict] [--format text|json|markdown] [--report FILE] [--hub-token]
//	touchmark probe
//	touchmark migrate  --from-multi-gitter FILE [--id ID] [--writer LOGIN] [--bot LOGIN]... [--ca-file FILE]
//	touchmark setup    github|gitlab [--hub DIR] [--provider ID] [--url URL] [--project PATH] [--dry-run] [--format text|json]
//	                   [gitlab: --group PATH ...] [github: --listen ADDR --key-dir DIR ...]
//	touchmark status   [--hub DIR] [--dir DIR] [--repo REF] [--packs a,b] [--worktree] [--format text|json]
//	touchmark apply    [same as status] [--adopt GLOB]... [--dry-run]
//	touchmark manifest [--hub DIR] [--format json]
//	touchmark schema   hub|targets|opt-in|operations|report|doctor|setup|status|check
//	touchmark version
//
// --hub defaults to $TOUCHMARK_HUB; --dir defaults to the current directory.
// --repo names the target in targets.yml ("path" or "provider:path"); when
// omitted it is derived from the target's `origin` remote.
//
// Exit codes: 0 success; 1 some action failed (for doctor: a check failed;
// for setup: a step failed), or an unexpected git or I/O error; 2 usage,
// configuration or guard error (nothing written); 3 plan or distribute
// --strict found a blocked or deferred target or an incomplete resolve,
// doctor --strict a check that warns or is unknown, setup a step left to
// the person.
//
// status, apply, plan, distribute and doctor print a report (text, or JSON
// with --format json) on stdout; errors go to stderr. In CI, distribute and
// doctor also leave their reports for the CI (distribute_out.go).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// Version, Commit and Date describe the build. A release sets them with
// -ldflags "-X .../internal/cli.Version=v0.1.0 -X .../internal/cli.Commit=<sha>
// -X .../internal/cli.Date=<RFC 3339 commit time>" (.goreleaser.yaml).
// Without them, `touchmark version` falls back to the module version and the
// VCS stamp the go command embeds (buildStamp).
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// env is what a command reads from and writes to besides its arguments.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	// environ lists the environment as "NAME=value" entries, for guards
	// that look for variables by pattern; nil lists nothing.
	environ func() []string
	getwd   func() (string, error)
}

// command is one touchmark subcommand.
type command struct {
	name     string
	synopsis string // arguments after the command name
	summary  string
	// flags returns the command's flag set bound to a fresh options value.
	flags func() (*flagSet, *options)
	run   func(ctx context.Context, e *env, o *options) error
}

// commands lists the subcommands in the order usage shows them.
func commands() []*command {
	return []*command{
		{name: "check", synopsis: "[--hub DIR] [--worktree] [--format text|json]",
			summary: "validate hub.yml, targets.yml and every pack",
			flags:   checkFlags, run: runCheck},
		planCommand(),
		distributeCommand(),
		doctorCommand(),
		{name: "probe", synopsis: "",
			summary: "exit 2 when this job sees a write credential or signing key (the isolation probe)",
			flags:   probeFlags, run: runProbe},
		migrateCommand(),
		setupCommand(),
		{name: "status", synopsis: "[--hub DIR] [--dir DIR] [--repo REF] [--packs a,b] [--assume-opt-in] [--worktree] [--format text|json]",
			summary: "list every managed path of the target and its state; changes nothing",
			flags:   statusFlags, run: runStatus},
		{name: "apply", synopsis: "[status flags] [--adopt GLOB]... [--dry-run]",
			summary: "apply the packs to the target's working tree",
			flags:   applyFlags, run: runApply},
		{name: "manifest", synopsis: "[--hub DIR] [--format json]",
			summary: "print the ownership manifest built from the hub's history",
			flags:   manifestFlags, run: runManifest},
		{name: "schema", synopsis: "hub|targets|opt-in|operations|report|doctor|setup|status|check",
			summary: "print the JSON Schema of a configuration file or of a report",
			flags:   schemaFlags, run: runSchema},
		{name: "version", synopsis: "",
			summary: "print the version",
			flags:   versionFlags, run: runVersion},
	}
}

// Main runs the command line and returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr, getenv: os.Getenv, environ: os.Environ, getwd: os.Getwd}
	return e.main(ctx, args)
}

func (e *env) main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		e.usage(e.stderr)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		e.usage(e.stdout)
		return exitOK
	}
	var cmd *command
	for _, c := range commands() {
		if c.name == args[0] {
			cmd = c
		}
	}
	if cmd == nil {
		fmt.Fprintf(e.stderr, "touchmark: unknown command %q\n\n", args[0])
		e.usage(e.stderr)
		return exitUsage
	}
	fs, opts := cmd.flags()
	if err := fs.parse(args[1:]); err != nil {
		if errors.Is(err, errHelp) {
			cmd.usage(e.stdout, fs)
			return exitOK
		}
		return e.fail(cmd, fs, err)
	}
	if err := cmd.run(ctx, e, opts); err != nil {
		return e.fail(cmd, fs, err)
	}
	return exitOK
}

// fail prints err, and the usage for a usage error, and returns its exit
// code.
func (e *env) fail(cmd *command, fs *flagSet, err error) int {
	var ce *cliError
	if !errors.As(err, &ce) {
		fmt.Fprintf(e.stderr, "touchmark %s: %v\n", cmd.name, err)
		return exitFailed
	}
	if ce.err != nil {
		fmt.Fprintf(e.stderr, "touchmark %s: %v\n", cmd.name, ce.err)
	}
	if ce.usage {
		fmt.Fprintln(e.stderr)
		cmd.usage(e.stderr, fs)
	}
	return ce.code
}

// usage prints the list of commands.
func (e *env) usage(w io.Writer) {
	fmt.Fprintln(w, "touchmark keeps shared files in sync across many repositories.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: touchmark <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run touchmark <command> --help for the flags of a command.")
	fmt.Fprintln(w, "Exit codes: 0 success, 1 some action failed, 2 usage, configuration or guard error,")
	fmt.Fprintln(w, "3 plan or distribute --strict found a blocked or deferred target or an incomplete resolve,")
	fmt.Fprintln(w, "doctor --strict a check that warns or is unknown, or setup a step left to you.")
}

// usage prints the synopsis and flags of one command.
func (c *command) usage(w io.Writer, fs *flagSet) {
	fmt.Fprintf(w, "Usage: touchmark %s %s\n\n%s.\n", c.name, c.synopsis, upperFirst(c.summary))
	fs.printDefaults(w)
}

func upperFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// cliError is an error with the exit code it maps to. Errors that are not a
// cliError exit with exitFailed.
type cliError struct {
	code  int
	usage bool // print the command's usage after the message
	err   error
}

func (e *cliError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *cliError) Unwrap() error { return e.err }

// usageErrorf is a usage error: exit 2, with the command's usage.
func usageErrorf(format string, args ...any) error {
	return &cliError{code: exitUsage, usage: true, err: fmt.Errorf(format, args...)}
}

// configError is a configuration or guard error: exit 2, nothing written.
func configError(err error) error {
	return &cliError{code: exitUsage, err: err}
}

// configErrorf is configError with a formatted message.
func configErrorf(format string, args ...any) error {
	return configError(fmt.Errorf(format, args...))
}

// exitWith ends the command with code after its output was printed.
func exitWith(code int) error { return &cliError{code: code} }
