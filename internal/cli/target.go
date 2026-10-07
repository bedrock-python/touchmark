package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/pathx"
	"github.com/bedrock-python/touchmark/internal/report"
)

// maxOptInRead is how much of the opt-in file is read: one byte over the
// parser's 64 KiB limit, so that it reports the file as too large.
const maxOptInRead = 64<<10 + 1

// target is the target working tree of status and apply.
type target struct {
	root string
	// git runs git at root; nil when root is not inside a git work tree.
	git *gitx.Git
	// ref names the target in targets.yml; refKnown is false when neither
	// --repo nor the origin remote gave one.
	ref      config.Ref
	refKnown bool
	// optInName is the opt-in file; optIn is nil when the target is not
	// opted in: the file is absent and targets.yml does not subscribe the
	// target, or it says enabled: false. optInState says which
	// (report.OptIn*).
	optInName  string
	optIn      *config.OptIn
	optInState string
	warnings   []string
}

// openTarget finds the target root: the top of the git work tree containing
// dir, or dir itself outside git. A directory inside a git directory (.git,
// a bare repository) is refused: it has no work tree, and files written
// there would configure the repository instead. So is a work tree with
// sha256 object ids.
func openTarget(ctx context.Context, e *env, dir string) (*target, error) {
	if dir == "" {
		wd, err := e.getwd()
		if err != nil {
			return nil, fmt.Errorf("current directory: %w", err)
		}
		dir = wd
	}
	abs, err := existingDir(dir)
	if err != nil {
		return nil, configErrorf("target: %w", err)
	}
	g := gitx.New(abs)
	top, ok, err := g.TopLevel(ctx)
	if err != nil {
		return nil, configErrorf("target %s: %w", abs, err)
	}
	if !ok {
		inside, err := g.InsideGitDir(ctx)
		if err != nil {
			return nil, configErrorf("target %s: %w", abs, err)
		}
		if inside {
			return nil, configErrorf("target %s is inside a git directory, not a work tree", abs)
		}
		return &target{root: abs}, nil
	}
	t := &target{root: top, git: gitx.New(top)}
	if err := requireSHA1(ctx, t.git, "target "+top); err != nil {
		return nil, err
	}
	return t, nil
}

// readOptIn reads and parses the opt-in file called name at the target
// root. An absent file, or one that says enabled: false, leaves optIn nil
// (optInState none or opted-out). The file comes from the target, so it is
// read without following symlinks and with a size limit.
func (t *target) readOptIn(name string) error {
	t.optInName, t.optInState = name, report.OptInNone
	data, found, err := readRegular(t.root, name, maxOptInRead)
	if err != nil {
		return configErrorf("opt-in file %s: %w", name, err)
	}
	if !found {
		return nil
	}
	o, warns, err := config.ParseOptIn(data)
	if err != nil {
		return configErrorf("%s: %w", name, joinedError(err))
	}
	for _, w := range warns {
		t.warnings = append(t.warnings, name+": "+w.Message)
	}
	if o.Disabled() {
		t.optInState = report.OptInDisabled
		return nil
	}
	t.optIn, t.optInState = o, report.OptInFile
	return nil
}

// assume opts in a target without an opt-in file that targets.yml
// subscribes (config.Assume): a repo: entry with opt_in: assumed names it.
// It then gets the packs of targets.yml, as if it had an empty opt-in
// file. An org or group entry with opt_in: assumed that may hold the
// target is a warning: only the platform tells whether it selects it, so
// the target stays not opted in here, and plan has the answer. flag
// (--assume-opt-in) settles that here instead: the target counts as
// opted in, as such an entry would make it. An opt-in file, enabled:
// false included, still wins over the flag.
func (t *target) assume(h *hub, flag bool) error {
	if t.optInState != report.OptInNone {
		return nil
	}
	var warning string
	if !t.refKnown {
		if h.targets != nil && (h.targets.Defaults.OptIn == config.OptInAssumed ||
			slices.ContainsFunc(h.targets.Targets, func(e config.Entry) bool { return e.OptIn == config.OptInAssumed })) {
			warning = "targets.yml subscribes some repositories without an opt-in file (opt_in: assumed), " +
				"and this one's name is unknown (no --repo and no origin remote): pass --repo to tell whether it is one of them, " +
				"or --assume-opt-in to count it as one"
		}
	} else {
		a, err := config.Assume(h.cfg, h.targets, t.ref)
		if err != nil {
			return configError(err)
		}
		if a.Assumed {
			t.optIn, t.optInState = &config.OptIn{Version: 1}, report.OptInAssumed
			return nil
		}
		if len(a.Unresolved) > 0 {
			warning = fmt.Sprintf("targets.yml may subscribe this repository without an opt-in file: %s with opt_in: assumed "+
				"cannot be resolved here, so it counts as not opted in; plan tells, and --assume-opt-in or an opt-in file decides it here",
				strings.Join(a.Unresolved, ", "))
		}
	}
	if flag {
		t.optIn, t.optInState = &config.OptIn{Version: 1}, report.OptInFlag
		return nil
	}
	if warning != "" {
		t.warnings = append(t.warnings, warning)
	}
	return nil
}

// readRegular reads at most limit bytes of the repository path p under
// root. found is false when p or one of its parents does not exist. A
// symlink or anything but a directory on the way, or anything but a regular
// file at p, is an error.
func readRegular(root, p string, limit int64) (data []byte, found bool, err error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	for _, dir := range pathx.Parents(p) {
		typ, exists, err := lstatType(r, dir)
		if err != nil || !exists {
			return nil, false, err
		}
		if typ != fs.ModeDir {
			return nil, false, fmt.Errorf("parent %s is not a directory", dir)
		}
	}
	typ, exists, err := lstatType(r, p)
	if err != nil || !exists {
		return nil, false, err
	}
	if typ != 0 {
		return nil, false, errors.New("not a regular file")
	}
	f, err := r.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s changed while being read", p)
	}
	data, err = io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// lstatType returns the type bits of the repository path p under r, without
// following a symlink at p; exists is false when p does not exist.
func lstatType(r *os.Root, p string) (typ fs.FileMode, exists bool, err error) {
	fi, err := r.Lstat(filepath.FromSlash(p))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return fi.Mode().Type(), true, nil
}

// resolveRef sets the target's ref: --repo when given, else the origin
// remote with the provider whose URL host matches it.
func (t *target) resolveRef(ctx context.Context, flagRepo string, h *hub) error {
	if flagRepo != "" {
		ref, err := config.ParseRef(flagRepo)
		if err != nil {
			return usageErrorf("--repo: %w", err)
		}
		t.ref, t.refKnown = ref, true
		return nil
	}
	if t.git == nil {
		return nil
	}
	out, err := t.git.Run(ctx, nil, "remote", "get-url", "origin")
	if err != nil {
		var gerr *gitx.Error
		if errors.As(err, &gerr) {
			return nil // no origin remote
		}
		return fmt.Errorf("read the origin remote: %w", err)
	}
	// The URL may hold credentials: it is never printed.
	host, path, ok := parseRemote(strings.TrimSpace(string(out)))
	if !ok {
		t.warnings = append(t.warnings, "cannot tell the target from the origin remote; pass --repo")
		return nil
	}
	ref, err := config.ParseRef(path)
	if err != nil {
		t.warnings = append(t.warnings, "the origin remote does not name a repository path touchmark understands; pass --repo")
		return nil
	}
	ref.Provider = remoteProvider(host, h.cfg, h.targets)
	t.ref, t.refKnown = ref, true
	return nil
}

// parseRemote returns the host and repository path of a remote URL:
// https://host/path, ssh://[user@]host[:port]/path, git://host/path and the
// scp-like [user@]host:path, with a trailing ".git" or slash removed. Local
// paths and other schemes are not recognized.
func parseRemote(remote string) (host, path string, ok bool) {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", "", false
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	} else {
		before, after, found := strings.Cut(remote, ":")
		if !found || strings.ContainsAny(before, `/\`) {
			return "", "", false
		}
		if i := strings.LastIndex(before, "@"); i >= 0 {
			before = before[i+1:]
		}
		// "C:" is a Windows drive, not a host.
		if len(before) <= 1 {
			return "", "", false
		}
		host, path = before, after
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimRight(path, "/")
	if host == "" || path == "" {
		return "", "", false
	}
	return strings.ToLower(host), path, true
}

// remoteProvider picks the provider of a target found through its remote:
// the hub provider whose URL has host, else defaults.provider, else the
// hub's only provider, else "" (unknown).
func remoteProvider(host string, cfg *config.Hub, targets *config.Targets) string {
	if cfg == nil {
		return ""
	}
	for _, p := range cfg.Providers {
		if u, err := url.Parse(p.URL); err == nil && strings.EqualFold(u.Hostname(), host) {
			return p.ID
		}
	}
	if targets != nil && targets.Defaults.Provider != "" {
		return targets.Defaults.Provider
	}
	if len(cfg.Providers) == 1 {
		return cfg.Providers[0].ID
	}
	return ""
}

// report returns the target part of a report.
func (t *target) report() report.Target {
	r := report.Target{Root: t.root, OptInFile: t.optInName, OptedIn: t.optIn != nil, OptIn: t.optInState}
	if t.refKnown {
		r.Ref = t.ref.String()
	}
	return r
}
