package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-python/touchmark/internal/auth"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/prbody"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/snapshot"
	"github.com/bedrock-python/touchmark/internal/sshsig"
)

// distributeDriver builds the write driver of one provider from its
// configuration, its write credential and an HTTP client that sends
// credentials only to the hosts they belong to. It is the only way to a
// platform.Writer: plan's registry (planDrivers) builds readers only.
type distributeDriver func(config.ResolvedProvider, auth.Credential, *httpx.Client) (platform.Writer, error)

// The write drivers of distribute by provider type: Gitea and Forgejo,
// GitLab and GitHub (drivers.go). Tests install the fake
// platform here; tests that do must not run in parallel.
var distributeDrivers = map[string]distributeDriver{}

// writeSecrets are what distribute reads from the environment for each
// provider (in the order of the providers): the write credential and the
// signing key (nil for none). missing are the providers without a write
// credential, when it was optional (readSecrets).
type writeSecrets struct {
	creds   []auth.Credential
	signers map[string]*sshsig.Signer
	missing []string
}

// readWriteSecrets reads every provider's write credential and signing key
// (see docs/reference/environment.md), registers their secrets with reg,
// and removes every credential variable of the providers from the process
// environment, so that no child process inherits them. A provider without
// a write credential, or with a credential or key that does not parse, is
// a configuration error.
func readWriteSecrets(rps []config.ResolvedProvider, getenv func(string) string, reg *redact.Registry) (writeSecrets, error) {
	return readSecrets(rps, getenv, reg, false)
}

// readSecrets is readWriteSecrets; with optional, a provider without a
// write credential is listed in missing instead (doctor --hub-token, which
// needs no write key), and its variables are removed all the same.
func readSecrets(rps []config.ResolvedProvider, getenv func(string) string, reg *redact.Registry, optional bool) (writeSecrets, error) {
	s := writeSecrets{creds: make([]auth.Credential, len(rps)), signers: map[string]*sshsig.Signer{}}
	var read []string
	defer func() {
		for _, name := range read {
			// Failing to unset leaves the value where it was: no reason to stop.
			_ = os.Unsetenv(name)
		}
	}()
	for i, rp := range rps {
		for _, name := range auth.Names(rp.EnvPrefix, rp.Short) {
			if getenv(name) != "" {
				read = append(read, name)
			}
		}
		c, ok, err := auth.FromEnv(rp.EnvPrefix, rp.Short, auth.Write, getenv)
		if err != nil {
			return writeSecrets{}, configErrorf("provider %s: %w", rp.ID, err)
		}
		if !ok && optional {
			s.missing = append(s.missing, rp.ID)
			continue
		}
		if !ok {
			p := rp.EnvPrefix
			return writeSecrets{}, configErrorf("provider %s: no write credential: set %sWRITE_TOKEN, or %sWRITE_APP_ID and %sWRITE_APP_KEY", rp.ID, p, p, p)
		}
		for _, secret := range c.Secrets() {
			reg.Add(secret, basicUsers...)
		}
		s.creds[i] = c
		key, ok, err := auth.SigningKey(rp.EnvPrefix, rp.Short, getenv)
		if err != nil {
			return writeSecrets{}, configErrorf("provider %s: %w", rp.ID, err)
		}
		if !ok {
			s.signers[rp.ID] = nil
			continue
		}
		// A signing key's forms are masked like an app key's.
		for _, secret := range (auth.Credential{Kind: auth.App, AppKey: key}).Secrets() {
			reg.Add(secret)
		}
		signer, err := sshsig.ParsePrivateKey(key)
		if err != nil {
			return writeSecrets{}, configErrorf("provider %s: %sSIGNING_KEY: %w", rp.ID, rp.EnvPrefix, err)
		}
		s.signers[rp.ID] = signer
	}
	return s, nil
}

// runInput is what distributeRun needs from runDistribute.
type runInput struct {
	hctx     hubch.Context
	fp       string
	rps      []config.ResolvedProvider
	drivers  []distributeDriver
	secrets  writeSecrets
	reg      *redact.Registry
	channel  hubch.Channel
	o        *options
	d        *distOptions
	localOps *config.Operations
	now      time.Time
}

// distRun is a prepared run of distribute: its dependencies and what must
// be released after it.
type distRun struct {
	deps     distribute.Deps
	mode     distribute.Mode
	warnings []string
	// shared are the dependencies plan has too; stream is the report
	// stream, closed on its own so that its failure reaches the report.
	shared *sharedDeps
	stream *streamWriter
	// drivers are the write drivers that hold credentials to release
	// (io.Closer: the GitHub driver revokes its reading tokens).
	drivers []io.Closer
}

// close releases what the run held; failures are ignored (temporary files,
// tokens that expire within the hour anyway). It is safe on a nil run.
func (r *distRun) close() {
	if r == nil {
		return
	}
	closeDrivers(r.drivers)
	r.drivers = nil
	if r.shared != nil {
		r.shared.close()
	}
}

// closeDrivers closes drivers, ignoring failures.
func closeDrivers(drivers []io.Closer) {
	for _, d := range drivers {
		_ = d.Close()
	}
}

// closeStream flushes and closes the report stream, if any. It is safe on
// a nil run.
func (r *distRun) closeStream() error {
	if r == nil || r.stream == nil {
		return nil
	}
	s := r.stream
	r.stream = nil
	return s.Close()
}

// distributeRun prepares the dependencies of distribute.Run: the
// dependencies plan has too (sharedDeps), the write drivers, the operation
// flags, the signing keys, the registry of secrets, the report stream and
// the deadline. On an error nothing is left behind.
func (h *hub) distributeRun(ctx context.Context, e *env, in runInput) (*distRun, error) {
	run := &distRun{mode: distribute.ModeDistribute}
	if in.o.dryRun {
		run.mode = distribute.ModeDryRun
	}
	ok := false
	defer func() {
		if !ok {
			_ = run.closeStream()
			run.close()
		}
	}()
	providers := make([]distribute.Provider, len(in.rps))
	pps := make([]planProvider, len(in.rps))
	for i, rp := range in.rps {
		pps[i] = planProvider{ResolvedProvider: rp}
		client, err := h.providerClient(ctx, pps[i], in.reg)
		if err != nil {
			return nil, configErrorf("provider %s: %w", rp.ID, err)
		}
		writer, err := in.drivers[i](rp, in.secrets.creds[i], client)
		if err != nil {
			return nil, configErrorf("provider %s: %w", rp.ID, err)
		}
		if c, ok := writer.(io.Closer); ok {
			run.drivers = append(run.drivers, c)
		}
		providers[i] = distribute.Provider{Config: rp, Reader: writer, Writer: writer}
	}
	shared, err := h.sharedDeps(ctx, in.hctx, pps, e.getenv)
	if err != nil {
		return nil, err
	}
	run.shared = shared
	stream, err := openStream(in, e.getenv)
	if err != nil {
		return nil, err
	}
	run.stream = stream
	var streamOut io.Writer
	if stream != nil {
		streamOut = stream
	}
	w := shared.write
	w.LocalOps = in.localOps
	w.Signers = in.secrets.signers
	w.Redact = in.reg
	w.Stream = streamOut
	w.Deadline = deadline(in.d, in.hctx, e.getenv, in.now)
	run.deps = distribute.Deps{
		Hub:         h.cfg,
		Targets:     h.targets,
		Manifest:    h.manifest,
		Current:     h.current,
		Known:       h.known(),
		HubCommit:   h.commit,
		HubContext:  in.hctx,
		InCI:        inCI(in.hctx, e.getenv),
		Fingerprint: in.fp,
		Channel:     in.channel,
		Providers:   providers,
		Snapshots:   shared.repos,
		Only:        in.o.only,
		Strict:      in.o.strict,
		Engine:      version(),
		Write:       w,
	}
	ok = true
	return run, nil
}

// deadline returns when distribute stops starting targets: --deadline, else
// on GitLab CI_JOB_TIMEOUT less 5 minutes, counted from the job's start
// (CI_JOB_STARTED_AT, when it is set and not in the future) and at least a
// minute from now, on GitHub Actions 5h30m, else none. A
// --deadline of 0 is none.
func deadline(d *distOptions, hctx hubch.Context, getenv func(string) string, now time.Time) time.Time {
	switch {
	case d.deadline.set:
		if d.deadline.d <= 0 {
			return time.Time{}
		}
		return now.Add(d.deadline.d)
	case hctx.CI == hubch.GitLabCI:
		secs, err := strconv.Atoi(strings.TrimSpace(getenv("CI_JOB_TIMEOUT")))
		if err != nil || secs <= 0 {
			return time.Time{}
		}
		start := now
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(getenv("CI_JOB_STARTED_AT"))); err == nil && !t.After(now) {
			start = t
		}
		return latest(start.Add(time.Duration(secs)*time.Second-deadlineMargin), now.Add(minDeadline))
	case hctx.CI == hubch.GitHubActions:
		return now.Add(actionsDeadline)
	}
	return time.Time{}
}

// latest returns the later of a and b.
func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// openStream opens the report stream: --stream, else touchmark-report.jsonl
// in CI; nil for none.
func openStream(in runInput, getenv func(string) string) (*streamWriter, error) {
	name := in.d.streamFile
	if name == "" && inCI(in.hctx, getenv) {
		name = streamFile
	}
	if name == "" {
		return nil, nil
	}
	f, err := os.Create(name)
	if err != nil {
		return nil, configErrorf("--stream: %w", err)
	}
	return &streamWriter{f: f, w: in.reg.Writer(f)}, nil
}

// streamWriter writes the report stream to its file through the masking
// writer, each line written through at once: the stream must survive a run
// killed half way.
type streamWriter struct {
	f *os.File
	w *redact.Writer
}

func (s *streamWriter) Write(p []byte) (int, error) { return s.w.Write(p) }

// Flush writes what the masking writer holds back.
func (s *streamWriter) Flush() error { return s.w.Flush() }

// Close flushes the stream and closes its file.
func (s *streamWriter) Close() error { return errors.Join(s.w.Flush(), s.f.Close()) }

// lockedBlobs serializes the reads of the hub's blobs: targets build their
// commits in parallel, and gitx.Blobs is not safe for concurrent use.
type lockedBlobs struct {
	mu sync.Mutex
	b  *gitx.Blobs
}

// open returns the content of blob oid.
func (l *lockedBlobs) open(oid string) (io.ReadCloser, error) {
	l.mu.Lock()
	data, err := l.b.Read(oid)
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// providerRepos is the snapshot source of distribute: a private git
// repository per target (snapshot.GitSource), one source per provider host
// so that each trusts its provider's ca_file.
type providerRepos struct {
	byHost map[string]*snapshot.GitSource
}

func (p *providerRepos) source(repo platform.Repo) (*snapshot.GitSource, error) {
	s := p.byHost[strings.ToLower(repo.Host)]
	if s == nil {
		return nil, fmt.Errorf("no provider on host %s", repo.Host)
	}
	return s, nil
}

func (p *providerRepos) Snapshot(ctx context.Context, repo platform.Repo, remote platform.Remote, ref string) (*snapshot.Tree, error) {
	s, err := p.source(repo)
	if err != nil {
		return nil, err
	}
	return s.Snapshot(ctx, repo, remote, ref)
}

func (p *providerRepos) Repo(ctx context.Context, repo platform.Repo, remote platform.Remote) (*gitx.TargetRepo, error) {
	s, err := p.source(repo)
	if err != nil {
		return nil, err
	}
	return s.Repo(ctx, repo, remote)
}

func (p *providerRepos) Release(repo platform.Repo) error {
	s, err := p.source(repo)
	if err != nil {
		return nil
	}
	return s.Release(repo)
}

var _ distribute.Repos = (*providerRepos)(nil)

// commitTime returns the committer date of the hub's HEAD, which dates the
// commits distribute builds.
func (h *hub) commitTime(ctx context.Context) (time.Time, error) {
	out, err := h.git.Run(ctx, nil, "show", "-s", "--format=%ct", h.commit)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the date of the hub's HEAD: %w", err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the date of the hub's HEAD: %q", strings.TrimSpace(string(out)))
	}
	return time.Unix(secs, 0).UTC(), nil
}

// intro reads pr.intro_file from the hub's HEAD ("" without one); a file
// that is missing or breaks prbody.CheckIntro is a configuration error.
func (h *hub) intro(ctx context.Context) (string, error) {
	name := h.cfg.PR.IntroFile
	if name == "" {
		return "", nil
	}
	data, err := h.readConfig(ctx, name)
	if err != nil {
		return "", configErrorf("pr.intro_file: %w", err)
	}
	if data == nil {
		return "", configErrorf("pr.intro_file: %s is not in the hub", name)
	}
	if err := prbody.CheckIntro(string(data)); err != nil {
		return "", configErrorf("pr.intro_file %s: %w", name, err)
	}
	return string(data), nil
}

// webURL returns the hub's web URL for pull request bodies: the CI's server
// and repository, else the origin remote's; "" with pr.link_hub never or
// when neither is known. The run hides it per target (pr.link_hub auto).
func (h *hub) webURL(ctx context.Context, hctx hubch.Context) string {
	if h.cfg.PR.LinkHub == "never" {
		return ""
	}
	if hctx.ServerURL != "" && hctx.RepoPath != "" {
		return hctx.ServerURL + "/" + hctx.RepoPath
	}
	out, err := h.git.Run(ctx, nil, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	host, path, ok := parseRemote(strings.TrimSpace(string(out)))
	if !ok {
		return ""
	}
	return "https://" + host + "/" + path
}
