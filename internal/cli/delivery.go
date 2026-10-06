package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/gitx"
	"github.com/bedrock-python/touchmark/internal/httpx"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/redact"
	"github.com/bedrock-python/touchmark/internal/snapshot"
)

// The dependencies plan and distribute build alike, so that a plan and a
// dry run of the same state say the same (a test holds plan to distribute
// --dry-run): the private git repository of every target, the hub's blobs
// and history, operations.yml, the intro, the hub's web URL and the hub's
// channel. Only distribute adds its write credentials, signing keys, stream
// and deadline (distribute_deps.go): nothing here reaches a writer, so plan
// holds no write credential.

// sharedDeps are the dependencies of distribute.Run that plan and distribute
// share, and what must be released after the run.
type sharedDeps struct {
	repos *providerRepos
	write distribute.WriteDeps
	// closers run in reverse order by close.
	closers []func() error
}

// close releases what the dependencies held; failures are ignored
// (temporary files, a blob reader).
func (d *sharedDeps) close() {
	for i := len(d.closers) - 1; i >= 0; i-- {
		_ = d.closers[i]()
	}
}

// sharedDeps prepares the dependencies of a run over the providers pps (each
// ca_file read at its caRev), under a new temporary directory in $RUNNER_TEMP
// (or the system's): a private git repository per target (one
// snapshot.GitSource per provider host, trusting its provider's ca_file,
// plain http only for a provider whose URL is http, which gitx allows on
// loopback hosts only); the hub's blobs at HEAD, the date of HEAD,
// operations.yml of HEAD, pr.intro_file, the hub's web URL, the Workflows
// permission every provider is assumed to have (the template gives the writer
// it; a writer that tells its own through platform.Preflighter, a GitHub App,
// overrides it per target), and the ancestry of hub commits for the
// per-target head guard. The caller closes the result, which removes the
// directory; on an error nothing is left behind.
func (h *hub) sharedDeps(ctx context.Context, hctx hubch.Context, pps []planProvider, getenv func(string) string) (d *sharedDeps, err error) {
	s := &sharedDeps{repos: &providerRepos{byHost: map[string]*snapshot.GitSource{}}}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	tmp, err := os.MkdirTemp(runnerTemp(getenv), "touchmark-")
	if err != nil {
		return nil, err
	}
	s.closers = append(s.closers, func() error { return os.RemoveAll(tmp) })
	home := filepath.Join(tmp, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, err
	}
	canWorkflows := map[string]bool{}
	for i, pp := range pps {
		canWorkflows[pp.ID] = true
		host := strings.ToLower(pp.Host)
		if _, dup := s.repos.byHost[host]; dup {
			continue
		}
		iso := gitx.Isolation{Home: home, AllowHTTP: strings.HasPrefix(pp.URL, "http://")}
		if pp.CAFile != "" {
			if iso.CAFile, err = h.caFile(ctx, pp, tmp); err != nil {
				return nil, configErrorf("provider %s: ca_file %s: %w", pp.ID, pp.CAFile, err)
			}
		}
		s.repos.byHost[host] = &snapshot.GitSource{Dir: filepath.Join(tmp, "targets", strconv.Itoa(i)), Isolation: iso}
	}
	g := h.git
	if h.src == provenance.WorkTree {
		// plan --worktree ships the packs of the work tree: their blobs are
		// written to a scratch object directory first.
		wg, cleanup, err := h.workTreeBlobs(ctx)
		if err != nil {
			return nil, err
		}
		s.closers = append(s.closers, cleanup)
		g = wg
	}
	blobs, err := g.OpenBlobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("read hub blobs: %w", err)
	}
	s.closers = append(s.closers, blobs.Close)
	when, err := h.commitTime(ctx)
	if err != nil {
		return nil, err
	}
	intro, err := h.intro(ctx)
	if err != nil {
		return nil, err
	}
	s.write = distribute.WriteDeps{
		Operations:    h.ops,
		HubBlobs:      (&lockedBlobs{b: blobs}).open,
		HubCommitTime: when,
		Intro:         intro,
		HubURL:        h.webURL(ctx, hctx),
		CanWorkflows:  canWorkflows,
		HubIsAncestor: h.isAncestor,
	}
	return s, nil
}

// runnerTemp is where a run keeps its temporary files: $RUNNER_TEMP on a CI
// runner that names one (GitHub Actions cleans it after the job), else
// the system's temporary directory ("").
func runnerTemp(getenv func(string) string) string {
	if dir := strings.TrimSpace(getenv("RUNNER_TEMP")); dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return ""
}

// isAncestor reports whether hub commit a is an ancestor of commit c in the
// hub's clone (distribute.WriteDeps.HubIsAncestor). c comes from a trailer
// of a target's branch: anything but a full commit id, and a commit the
// clone does not have (a shallow CI checkout, a local clone behind the
// hub), is no descendant: an unknown hub commit never blocks a write.
func (h *hub) isAncestor(ctx context.Context, a, c string) (bool, error) {
	if !isFullOID(a) || !isFullOID(c) {
		return false, nil
	}
	if _, err := h.git.RevParse(ctx, c+"^{commit}"); err != nil {
		return false, nil
	}
	_, err := h.git.Run(ctx, nil, "merge-base", "--is-ancestor", a, c)
	var ge *gitx.Error
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ge) && ge.Code == 1:
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

// isFullOID reports whether s is a full hex object id, sha1 or sha256.
func isFullOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(strings.ToLower(s), "0123456789abcdef") == ""
}

// caFile writes the provider's ca_file, read from the hub at pp.caRev (from
// HEAD, or the work tree with --worktree, when it is ""), into dir for git,
// and returns its path.
func (h *hub) caFile(ctx context.Context, pp planProvider, dir string) (string, error) {
	var data []byte
	var err error
	if pp.caRev == "" {
		data, err = h.readConfig(ctx, pp.CAFile)
	} else if data, err = h.git.ShowFile(ctx, pp.caRev, pp.CAFile); errors.Is(err, gitx.ErrNotFound) {
		data, err = nil, nil
	}
	if err != nil {
		return "", err
	}
	if data == nil {
		return "", errors.New("no such file in the hub")
	}
	name := filepath.Join(dir, "ca-"+pp.ID+".pem")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// hubChannel returns the channel to the hub for the head guard: only in CI,
// and only where the guard applies (distribute.HeadGuard). It trusts, besides
// the system roots, the ca_file of the first provider on the hub's own host
// (a GitHub Enterprise Server or Gitea behind a private CA). A channel that
// cannot be built fails when asked, so the report says why the head was not
// checked (and distribute stops: distribute.ErrHeadUnchecked).
func (h *hub) hubChannel(ctx context.Context, hctx hubch.Context, pps []planProvider, token string, reg *redact.Registry) hubch.Channel {
	if hctx.CI == hubch.Local || hctx.CI == "" || !distribute.HeadGuard(hctx) {
		return nil
	}
	return h.newChannel(ctx, hctx, pps, token, reg)
}

// newChannel is the channel to the hub of a CI run, whatever it builds (a
// hub pull request's plan reads the default branch's tip and comments
// through it), trusting what hubChannel trusts.
func (h *hub) newChannel(ctx context.Context, hctx hubch.Context, pps []planProvider, token string, reg *redact.Registry) hubch.Channel {
	var roots *x509.CertPool
	for _, pp := range pps {
		if pp.CAFile == "" || !strings.EqualFold(pp.Host, hctx.Host) {
			continue
		}
		pool, err := h.caPool(ctx, pp.CAFile, pp.caRev)
		if err != nil {
			return failedChannel{fmt.Errorf("hub channel: ca_file %s of provider %s: %w", pp.CAFile, pp.ID, err)}
		}
		roots = pool
		break
	}
	ch, err := hubch.New(hctx, httpx.New(httpx.Options{Redact: reg, RootCAs: roots}), token)
	if err != nil {
		return failedChannel{err}
	}
	return ch
}

// failedChannel is a hub channel that could not be built.
type failedChannel struct{ err error }

func (c failedChannel) Head(context.Context) (string, error) { return "", c.err }

// channelVisibility fills in the hub's visibility from the channel when the
// CI's event payload names none (GitHub's and Gitea's schedule event). It
// returns hctx as it was, with a warning, when the channel cannot tell: the
// run then treats the hub as public.
func channelVisibility(ctx context.Context, hctx hubch.Context, ch hubch.Channel) (hubch.Context, []string) {
	if hctx.Visibility != "" || hctx.CI == hubch.Local || hctx.CI == "" {
		return hctx, nil
	}
	v, ok := ch.(hubch.VisibilityReader)
	if !ok {
		return hctx, nil
	}
	vis, err := v.Visibility(ctx)
	if err != nil {
		return hctx, []string{fmt.Sprintf("the hub's visibility is not in the CI event, and the hub channel cannot read it: %v", err)}
	}
	hctx.Visibility = vis
	return hctx, nil
}
