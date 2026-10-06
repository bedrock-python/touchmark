package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/bedrock-python/touchmark/schemas"
)

// runManifest prints the manifest built from the hub's committed history.
// Pack names are raw directory names: formerly is not applied.
func runManifest(ctx context.Context, e *env, o *options) error {
	h, err := openHub(ctx, e, o.hub, false)
	if err != nil {
		return err
	}
	if err := h.build(ctx); err != nil {
		return err
	}
	return h.manifest.WriteJSON(e.stdout)
}

// runSchema prints an embedded JSON Schema.
func runSchema(_ context.Context, e *env, o *options) error {
	name := o.args[0]
	data, ok := schemas.Get(name)
	if !ok {
		return usageErrorf("unknown schema %q: want %s", name, strings.Join(schemas.Names(), ", "))
	}
	if _, err := e.stdout.Write(data); err != nil {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		_, err := fmt.Fprintln(e.stdout)
		return err
	}
	return nil
}

// runVersion prints touchmark's version, the Go toolchain and platform it
// was built for, and, when known, the commit it was built from and that
// commit's time: "touchmark v0.1.0 (go1.26.8 linux/amd64, commit 3f2a…,
// 2026-10-01T12:00:00Z)".
func runVersion(_ context.Context, e *env, _ *options) error {
	var b strings.Builder
	fmt.Fprintf(&b, "touchmark %s (%s %s/%s", version(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	commit, date := buildStamp()
	if commit != "" {
		b.WriteString(", commit " + commit)
	}
	if date != "" {
		b.WriteString(", " + date)
	}
	b.WriteString(")\n")
	_, err := io.WriteString(e.stdout, b.String())
	return err
}

// buildStamp returns Commit and Date, or, for a build that did not set
// them, the VCS stamp the go command embeds when it builds in a git checkout
// (vcs.revision with "-dirty" for uncommitted changes, and vcs.time).
func buildStamp() (commit, date string) {
	commit, date = Commit, Date
	if commit != "" && date != "" {
		return commit, date
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return commit, date
	}
	var revision, modified, vcsTime string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		case "vcs.time":
			vcsTime = s.Value
		}
	}
	if commit == "" && revision != "" {
		commit = revision
		if modified == "true" {
			commit += "-dirty"
		}
	}
	if date == "" {
		date = vcsTime
	}
	return commit, date
}

// version returns Version, or the module version when touchmark was built
// with `go install ...@version` and Version was not set.
func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}
