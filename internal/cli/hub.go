package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bedrock-python/touchmark/internal/apply"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
)

// hubEnv is the environment variable that names the hub when --hub is not
// given.
const hubEnv = "TOUCHMARK_HUB"

// maxHubConfigSize bounds how much of a config file is read from the hub's
// work tree; the parsers reject anything over 64 KiB with a clear message.
const maxHubConfigSize = 1 << 20

// hub is an opened hub checkout and what the command read from it.
type hub struct {
	dir    string // root of the work tree
	git    *gitx.Git
	commit string // HEAD, resolved once
	src    provenance.Source

	// cfg and targets are nil until readConfigs parsed them.
	cfg     *config.Hub
	targets *config.Targets
	// ops is nil until readOperations parsed .touchmark/operations.yml;
	// only check reads it.
	ops      *config.Operations
	warnings []string
	// ciOnlyURLs is set when the web URLs of targets.yml resolved against
	// the implicit provider the CI's environment names, on an instance an
	// origin remote cannot name outside CI (config.OriginTellsProvider):
	// local runs cannot resolve them.
	ciOnlyURLs bool

	manifest *provenance.Manifest
	skipped  []string
	current  provenance.Current
	problems []provenance.Problem
}

// openHub locates the hub: --hub or $TOUCHMARK_HUB, which must be inside a
// git work tree with sha1 object ids and at least one commit, and a git new
// enough.
func openHub(ctx context.Context, e *env, flagDir string, worktree bool) (*hub, error) {
	dir := flagDir
	if dir == "" {
		dir = e.getenv(hubEnv)
	}
	if dir == "" {
		return nil, usageErrorf("no hub: pass --hub DIR or set %s", hubEnv)
	}
	abs, err := existingDir(dir)
	if err != nil {
		return nil, configErrorf("hub: %w", err)
	}
	g := gitx.New(abs)
	if err := g.CheckVersion(ctx, gitx.MinVersion); err != nil {
		return nil, configError(err)
	}
	top, ok, err := g.TopLevel(ctx)
	if err != nil {
		return nil, configErrorf("hub %s: %w", abs, err)
	}
	if !ok {
		return nil, configErrorf("hub %s is not a git work tree", abs)
	}
	h := &hub{dir: top, git: gitx.New(top), src: provenance.Committed}
	if worktree {
		h.src = provenance.WorkTree
	}
	if err := requireSHA1(ctx, h.git, "hub "+top); err != nil {
		return nil, err
	}
	h.commit, err = h.git.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, configErrorf("hub %s has no commit to read: %w", top, err)
	}
	return h, nil
}

// requireSHA1 is a guard error unless the repository g runs in uses sha1
// object ids: target identities are sha1 blob ids, which never equal the
// ids of a sha256 repository, so every file would look local and every
// write would fail its check.
func requireSHA1(ctx context.Context, g *gitx.Git, what string) error {
	format, err := g.ObjectFormat(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if format != "sha1" {
		return configErrorf("%s uses %s object ids; touchmark supports sha1 repositories only", what, format)
	}
	return nil
}

// existingDir returns the absolute form of dir and fails unless it is a
// directory.
func existingDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

// readConfigs reads and parses hub.yml and targets.yml, and rewrites the
// web URLs of targets.yml as <provider>:<path> (resolveURLs). It returns
// every problem found; cfg and targets stay nil for a file that failed,
// targets too when a URL did not resolve.
func (h *hub) readConfigs(ctx context.Context, e *env) []error {
	var errs []error
	data, err := h.readConfig(ctx, config.HubFile)
	if err == nil {
		var warns []config.Warning
		h.cfg, warns, err = config.ParseHub(data)
		h.addWarnings(warns)
	}
	if err != nil {
		errs = append(errs, err)
	}
	data, err = h.readConfig(ctx, config.TargetsFile)
	if err == nil {
		var warns []config.Warning
		h.targets, warns, err = config.ParseTargets(data)
		h.addWarnings(warns)
	}
	if err != nil {
		errs = append(errs, err)
	}
	if h.cfg != nil && h.targets.HasURLs() {
		if err := h.resolveURLs(ctx, e.getenv); err != nil {
			errs = append(errs, err)
			h.targets = nil
		}
	}
	return errs
}

// resolveURLs rewrites the web URLs of targets.yml as <provider>:<path>
// (config.ResolveURLs) against the providers plan and distribute resolve:
// those of hub.yml, or the implicit one, which the CI's environment or,
// outside CI, the hub's origin remote names (on github.com, a *.ghe.com
// host or gitlab.com only).
func (h *hub) resolveURLs(ctx context.Context, getenv func(string) string) error {
	rps, err := h.resolveProviders(ctx, hubch.Detect(getenv, os.ReadFile), getenv)
	if err != nil {
		return fmt.Errorf("%s: its web URLs are matched with the providers' urls, which are unknown: %w", config.TargetsFile, err)
	}
	targets, err := config.ResolveURLs(h.targets, rps)
	if err != nil {
		return err
	}
	h.targets = targets
	h.ciOnlyURLs = len(rps) == 1 && rps[0].Implicit && !config.OriginTellsProvider(rps[0].Host)
	return nil
}

// readOperations reads and parses .touchmark/operations.yml like
// readConfigs reads hub.yml: from HEAD or the work tree, absent meaning no
// operations. ops stays nil when the file failed.
func (h *hub) readOperations(ctx context.Context) error {
	data, err := h.readConfig(ctx, config.OperationsFile)
	if err != nil {
		return err
	}
	ops, warns, err := config.ParseOperations(data)
	h.addWarnings(warns)
	if err != nil {
		return err
	}
	h.ops = ops
	return nil
}

func (h *hub) addWarnings(warns []config.Warning) {
	for _, w := range warns {
		h.warnings = append(h.warnings, w.String())
	}
}

// readConfig returns the content of a config file at the hub's root, from
// HEAD or the work tree; nil when the file does not exist.
func (h *hub) readConfig(ctx context.Context, name string) ([]byte, error) {
	if h.src == provenance.WorkTree {
		data, err := readLimited(filepath.Join(h.dir, name), maxHubConfigSize)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return data, nil
	}
	data, err := h.git.ShowFile(ctx, h.commit, name)
	if errors.Is(err, gitx.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return data, nil
}

// readLimited reads at most limit+1 bytes of a file.
func readLimited(name string, limit int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// source returns the hub as provenance reads it, pinned to the commit
// resolved when the hub was opened.
func (h *hub) source() *provenance.Hub {
	return &provenance.Hub{Dir: h.dir, Git: h.git, Rev: h.commit}
}

// build builds the manifest from the hub's committed history. A shallow or
// partial hub is a guard error.
func (h *hub) build(ctx context.Context) error {
	m, skipped, err := provenance.Build(ctx, h.source())
	if isGuard(err) {
		return configError(err)
	}
	if err != nil {
		return err
	}
	h.manifest, h.skipped = m, skipped
	return nil
}

// isGuard reports whether a manifest error means the checkout cannot be
// used as a hub as it is.
func isGuard(err error) bool {
	return errors.Is(err, provenance.ErrShallow) || errors.Is(err, provenance.ErrPartialClone)
}

// readPacks reads what the packs ship now.
func (h *hub) readPacks(ctx context.Context) error {
	cur, problems, err := provenance.ReadCurrent(ctx, h.source(), h.src)
	if err != nil {
		return err
	}
	h.current, h.problems = cur, problems
	return nil
}

// known returns the packs the hub ships now.
func (h *hub) known() map[string]bool {
	out := make(map[string]bool, len(h.current))
	for pack := range h.current {
		out[pack] = true
	}
	return out
}

// packNames returns the packs the hub ships now, sorted.
func (h *hub) packNames() []string { return slices.Sorted(maps.Keys(h.current)) }

// optInName returns the opt-in file name, the default while hub.yml is not
// parsed.
func (h *hub) optInName() string { return h.cfg.OptInName() }

// aliases returns the former names of every pack that selection honours:
// a name check rejects (a current pack, one claimed twice, a rename to a
// pack that is gone) never pulls another pack's history into a decision.
func (h *hub) aliases() provenance.Aliases {
	if h.cfg == nil {
		return nil
	}
	return provenance.Aliases(h.cfg.KnownAliases(h.known()))
}

// checkErrors returns what `touchmark check` would report as errors about the
// configs and the packs, given the configs parsed: status and apply refuse
// to act on such a hub.
func (h *hub) checkErrors() []string {
	var msgs []string
	if h.cfg != nil && h.targets != nil {
		_, errs := config.Check(h.cfg, h.targets, h.known())
		for _, err := range errs {
			msgs = append(msgs, flatten(err)...)
		}
	}
	for _, p := range h.packProblems() {
		msgs = append(msgs, p.String())
	}
	return msgs
}

// packProblems returns the defects of the packs, found while reading them
// and by provenance.CheckPacks, sorted by pack, path and message.
func (h *hub) packProblems() []provenance.Problem {
	problems := slices.Concat(h.problems, provenance.CheckPacks(h.current, h.optInName()))
	slices.SortFunc(problems, func(a, b provenance.Problem) int {
		return cmp.Or(cmp.Compare(a.Pack, b.Pack), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Message, b.Message))
	})
	return problems
}

// report returns the hub part of a report.
func (h *hub) report() report.Hub {
	r := report.Hub{Dir: h.dir, Commit: h.commit}
	if h.cfg != nil {
		r.ID = h.cfg.ID
	}
	return r
}

// blobs returns the source of pack contents for apply: the hub's object
// store, or with --worktree the blobs git would commit for the files of its
// work tree (its clean filters applied, as provenance.ReadCurrent computed
// their ids), written into a scratch object directory. close releases it
// and removes the scratch directory.
func (h *hub) blobs(ctx context.Context) (src apply.BlobSource, closeFn func() error, err error) {
	g, cleanup := h.git, func() error { return nil }
	if h.src == provenance.WorkTree {
		g, cleanup, err = h.workTreeBlobs(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	b, err := g.OpenBlobs(ctx)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("read hub blobs: %w", err), cleanup())
	}
	return apply.GitBlobs{B: b}, func() error { return errors.Join(b.Close(), cleanup()) }, nil
}

// workTreeBlobs writes the blobs of every pack file in the hub's work tree
// into a scratch object directory and returns a Git that reads from it. A
// file edited since ReadCurrent gets another id, so its write fails the
// check against the planned one instead of shipping other content.
func (h *hub) workTreeBlobs(ctx context.Context) (*gitx.Git, func() error, error) {
	scratch, err := os.MkdirTemp("", "touchmark-objects-")
	if err != nil {
		return nil, nil, fmt.Errorf("read hub blobs: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(scratch) }
	var paths []string
	for _, pack := range h.packNames() {
		for _, p := range slices.Sorted(maps.Keys(h.current[pack])) {
			paths = append(paths, provenance.PacksDir+"/"+pack+"/"+p)
		}
	}
	g := h.git.WithObjectDir(scratch)
	if _, err := g.WritePaths(ctx, paths); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("read hub blobs from the work tree: %w", err), cleanup())
	}
	return g, cleanup, nil
}

// flatten splits joined errors (errors.Join, as the config parsers return
// them) into one message each. A wrapped join is kept whole, with its
// context.
func flatten(err error) []string {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range j.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []string{err.Error()}
}

// joinedError returns the messages of err on separate lines, indented, for
// one stderr message.
func joinedError(err error) error {
	msgs := flatten(err)
	if len(msgs) <= 1 {
		return err
	}
	return errors.New(strings.Join(msgs, "\n  "))
}

// display returns s as is when it prints cleanly, and Go-quoted when it holds
// invalid UTF-8 or characters that are not printable.
func display(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return strconv.Quote(s)
}

// workflows returns the GitHub Actions workflows of the hub (the regular
// *.yml and *.yaml files directly under config.WorkflowsDir) by path, from
// HEAD, or the work tree with --worktree; none when the directory is
// absent. A file is read up to one byte past config.MaxWorkflowSize, so
// that config.CheckProbe sees one too large.
func (h *hub) workflows(ctx context.Context) (map[string][]byte, error) {
	out := map[string][]byte{}
	isWorkflow := func(name string) bool {
		return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
	}
	if h.src == provenance.WorkTree {
		dir := filepath.Join(h.dir, filepath.FromSlash(config.WorkflowsDir))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", config.WorkflowsDir, err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !isWorkflow(e.Name()) {
				continue
			}
			data, err := readLimited(filepath.Join(dir, e.Name()), config.MaxWorkflowSize)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", config.WorkflowsDir, e.Name(), err)
			}
			out[config.WorkflowsDir+"/"+e.Name()] = data
		}
		return out, nil
	}
	list, err := h.git.Run(ctx, nil, "ls-tree", "-z", h.commit, "--", config.WorkflowsDir+"/")
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", config.WorkflowsDir, err)
	}
	for _, rec := range strings.Split(string(list), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || !isWorkflow(path) || strings.Contains(strings.TrimPrefix(path, config.WorkflowsDir+"/"), "/") {
			continue
		}
		if f := strings.Fields(meta); len(f) != 3 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") {
			continue
		}
		data, err := h.git.ShowFile(ctx, h.commit, path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(data) > config.MaxWorkflowSize {
			data = data[:config.MaxWorkflowSize+1]
		}
		out[path] = data
	}
	return out, nil
}
