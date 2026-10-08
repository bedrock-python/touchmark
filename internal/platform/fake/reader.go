package fake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// ops name what each method attempts, for platform.Error.Op.
var ops = map[string]string{
	"Probe":        "probe",
	"Self":         "get account",
	"Lookup":       "look up account",
	"Resolve":      "resolve",
	"Repo":         "get repository",
	"ReadFile":     "read file",
	"Remote":       "get remote",
	"PRs":          "list pull requests",
	"OpenPRsBy":    "list open pull requests",
	"Target":       "target",
	"CreatePR":     "create pull request",
	"EditPR":       "edit pull request",
	"Comment":      "comment",
	"EnsureLabels": "ensure labels",
	"Close":        "close",
	"Snapshot":     "snapshot",
	"Preflight":    "preflight",
	"NoPush":       "read branch protection",
	"Commit":       "commit",
	// The checks of doctor (checker.go).
	"Check":           "check",
	"CheckSigningKey": "check the signing key",
}

// Reader returns the platform.Reader of account as. Every call fails with
// ClassAuth when as is not an account of the platform.
func (p *Platform) Reader(as platform.Account) platform.Reader {
	if p.preflight {
		return &preflightReader{reader{p: p, as: as}}
	}
	return &reader{p: p, as: as}
}

type reader struct {
	p  *Platform
	as platform.Account
}

// start opens a call of method: it checks ctx, logs the call and returns
// the error the call fails with (the setup error, an injected fault, a bad
// credential), or a fault to return once the call took effect. as is nil
// for calls without an identity. Called with p.mu held.
func (p *Platform) start(ctx context.Context, as *platform.Account, method string, args []string) (lost, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", ops[method], err)
	}
	p.logCall(method, args...)
	if p.err != nil {
		return nil, fmt.Errorf("%s: broken fixture: %w", ops[method], p.err)
	}
	f, ok := p.takeFault(method)
	if !ok && p.inject != nil {
		ictx := context.WithValue(ctx, loggedKey{}, len(p.requests))
		if err, applied := p.inject(ictx, method, args); err != nil {
			f, ok = fault{err: err, applied: applied}, true
		}
	}
	if ok {
		if !f.applied {
			return nil, f.err
		}
		lost = f.err
	}
	if as != nil && (as.ID == "" || p.accounts[as.ID] == nil) {
		return nil, &platform.Error{Op: ops[method], Class: platform.ClassAuth, Status: http.StatusUnauthorized,
			Err: errors.New("bad credentials")}
	}
	return lost, nil
}

// do runs one call of method with p.mu held: start, then body, whose
// result an applied fault replaces.
func do[T any](ctx context.Context, p *Platform, as *platform.Account, method string, args []string, body func() (T, error)) (T, error) {
	var zero T
	if err := p.meterRead(ctx, method); err != nil {
		return zero, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lost, err := p.start(ctx, as, method, args)
	if err != nil {
		return zero, err
	}
	v, err := body()
	if lost != nil {
		return zero, lost
	}
	return v, err
}

func (r *reader) Probe(ctx context.Context) (platform.Caps, error) {
	return do(ctx, r.p, &r.as, "Probe", nil, func() (platform.Caps, error) {
		return cloneCaps(r.p.caps), nil
	})
}

func (r *reader) Self(ctx context.Context) (platform.Account, error) {
	return do(ctx, r.p, &r.as, "Self", nil, func() (platform.Account, error) {
		return *r.p.accounts[r.as.ID], nil
	})
}

func (r *reader) Lookup(ctx context.Context, login string) (platform.Account, error) {
	return do(ctx, r.p, &r.as, "Lookup", []string{login}, func() (platform.Account, error) {
		if a := r.p.accountByLogin(login); a != nil && login != "" {
			return *a, nil
		}
		return platform.Account{}, notFound(ops["Lookup"], "account %q", login)
	})
}

// Resolve lists the repositories sel selects, sorted by path (case
// folded). A namespace selects the repositories directly in it, and with
// Subgroups those in nested namespaces too; Topics compare
// case-insensitively and must all be present; forks only with Forks.
// Archived, disabled, empty and other skippable repositories are included.
func (r *reader) Resolve(ctx context.Context, sel platform.Selector) (platform.Resolved, error) {
	arg := sel.Repo
	if arg == "" {
		arg = sel.Namespace
	}
	return do(ctx, r.p, &r.as, "Resolve", []string{arg}, func() (platform.Resolved, error) {
		p := r.p
		switch {
		case sel.Repo != "":
			s := p.repoByPathLocked(sel.Repo)
			if s == nil {
				return platform.Resolved{}, notFound(ops["Resolve"], "repository %q", sel.Repo)
			}
			return platform.Resolved{Repos: []platform.Repo{p.repoView(s)}, Complete: true}, nil
		case sel.Namespace == "":
			return platform.Resolved{}, invalid(ops["Resolve"], "the selector names no repository and no namespace")
		}
		prefix := strings.ToLower(sel.Namespace) + "/"
		var found []*repoState
		for _, s := range p.repos {
			path := strings.ToLower(s.repo.Path)
			rest, ok := strings.CutPrefix(path, prefix)
			switch {
			case !ok:
			case !sel.Subgroups && strings.Contains(rest, "/"):
			case s.repo.Fork && !sel.Forks:
			case !hasTopics(s.repo.Topics, sel.Topics):
			default:
				found = append(found, s)
			}
		}
		sortRepos(found)
		out := platform.Resolved{Repos: make([]platform.Repo, 0, len(found)), Complete: !p.incomplete}
		for _, s := range found {
			out.Repos = append(out.Repos, p.repoView(s))
		}
		return out, nil
	})
}

func (r *reader) Repo(ctx context.Context, path string) (platform.Repo, error) {
	return do(ctx, r.p, &r.as, "Repo", []string{path}, func() (platform.Repo, error) {
		s := r.p.repoByPathLocked(path)
		if s == nil || path == "" {
			return platform.Repo{}, notFound(ops["Repo"], "repository %q", path)
		}
		return r.p.repoView(s), nil
	})
}

// ReadFile serves ref "" and, for the same tree, the default branch name
// and its head commit; other refs are ClassUnsupported. In git mode it
// reads the bare repository at any branch or commit (gitReadFile).
func (r *reader) ReadFile(ctx context.Context, repo platform.Repo, ref, path string, max int64) (platform.File, error) {
	op := ops["ReadFile"]
	return do(ctx, r.p, &r.as, "ReadFile", []string{repo.Path, path}, func() (platform.File, error) {
		p := r.p
		s, err := p.repoOf(op, repo)
		if err != nil {
			return platform.File{}, err
		}
		if p.git == nil {
			if err := checkRef(op, s, ref); err != nil {
				return platform.File{}, err
			}
		}
		if max < 0 {
			return platform.File{}, invalid(op, "negative size limit %d", max)
		}
		if err := checkTreePath(path); err != nil {
			return platform.File{}, invalid(op, "%v", err)
		}
		if p.git != nil {
			return p.gitReadFile(ctx, op, s, ref, path, max)
		}
		e, ok := s.entries[path]
		if !ok {
			if _, isDir := s.child(path); isDir {
				return platform.File{}, fmt.Errorf("%s: %s: %s is a directory: %w", op, s.repo.Path, path, platform.ErrNotRegular)
			}
			return platform.File{}, notFound(op, "%s: %s", s.repo.Path, path)
		}
		if e.Mode != ModeFile && e.Mode != ModeExecutable {
			return platform.File{}, fmt.Errorf("%s: %s: %s has mode %s: %w", op, s.repo.Path, path, e.Mode, platform.ErrNotRegular)
		}
		content := p.blobs[e.OID]
		if int64(len(content)) > max {
			return platform.File{}, fmt.Errorf("%s: %s: %s has %d bytes, more than %d: %w",
				op, s.repo.Path, path, len(content), max, platform.ErrTooLarge)
		}
		return platform.File{Path: path, Mode: e.Mode, OID: e.OID, Content: slices.Clone(content)}, nil
	})
}

// Remote returns the URL "fake://<host>/<path>" without a header; in git
// mode, the repository's URL on the git server with a header that sends
// the account's token (none without a token), in its read-only form on
// GitHub-like flavors (Writer.Remote too: writes go through Target).
func (r *reader) Remote(ctx context.Context, repo platform.Repo) (platform.Remote, error) {
	return do(ctx, r.p, &r.as, "Remote", []string{repo.Path}, func() (platform.Remote, error) {
		s, err := r.p.repoOf(ops["Remote"], repo)
		if err != nil {
			return platform.Remote{}, err
		}
		rem := platform.Remote{URL: r.p.remoteURL(s)}
		if r.p.git != nil {
			rem.Header = r.p.gitHeader(r.as.ID)
		}
		return rem, nil
	})
}

func (r *reader) PRs(ctx context.Context, repo platform.Repo, heads []string, authors []platform.Account) ([]platform.PR, error) {
	return do(ctx, r.p, &r.as, "PRs", []string{repo.Path}, func() ([]platform.PR, error) {
		p := r.p
		s, err := p.repoOf(ops["PRs"], repo)
		if err != nil {
			return nil, err
		}
		ids := accountIDs(authors)
		out := []platform.PR{}
		for _, ps := range s.prs {
			pr := ps.pr
			if slices.Contains(heads, pr.Head) && (pr.State == platform.Open || ids[pr.Author.ID]) {
				out = append(out, p.prView(pr))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
		return out, nil
	})
}

// OpenPRsBy sees every repository of the platform. The result is sorted by
// repository path (case folded), then newest first.
func (r *reader) OpenPRsBy(ctx context.Context, authors []platform.Account, heads []string) (platform.Swept, error) {
	return do(ctx, r.p, &r.as, "OpenPRsBy", nil, func() (platform.Swept, error) {
		p := r.p
		ids := accountIDs(authors)
		var repos []*repoState
		for _, s := range p.repos {
			repos = append(repos, s)
		}
		sortRepos(repos)
		out := platform.Swept{PRs: []platform.RepoPR{}, Complete: !p.incomplete}
		for _, s := range repos {
			var prs []platform.PR
			for _, ps := range s.prs {
				pr := ps.pr
				if pr.State == platform.Open && ids[pr.Author.ID] && slices.Contains(heads, pr.Head) {
					prs = append(prs, p.prView(pr))
				}
			}
			sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })
			for _, pr := range prs {
				out.PRs = append(out.PRs, platform.RepoPR{Repo: p.repoView(s), PR: pr})
			}
		}
		return out, nil
	})
}

// repoOf finds the repository a platform.Repo names, by ID. A repository
// of another host or without an id is a caller's bug: ClassInvalid.
func (p *Platform) repoOf(op string, r platform.Repo) (*repoState, error) {
	if r.ID == "" {
		return nil, invalid(op, "repository %q has no id", r.Path)
	}
	if r.Host != "" && !strings.EqualFold(r.Host, p.host) {
		return nil, invalid(op, "repository %q is on %s, not %s", r.Path, r.Host, p.host)
	}
	s := p.repos[r.ID]
	if s == nil {
		return nil, notFound(op, "repository %q (id %s)", r.Path, r.ID)
	}
	return s, nil
}

// checkRef accepts the refs that name the default branch head.
func checkRef(op string, s *repoState, ref string) error {
	switch ref {
	case "", s.repo.DefaultBranch, "refs/heads/" + s.repo.DefaultBranch:
		return nil
	}
	if head := s.head(); head != "" && strings.EqualFold(ref, head) {
		return nil
	}
	return &platform.Error{Op: op, Class: platform.ClassUnsupported,
		Err: fmt.Errorf("the fake serves only the default branch head, not %q", ref)}
}

// hasTopics reports whether have holds every topic of want, ignoring case.
func hasTopics(have, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}

func accountIDs(accounts []platform.Account) map[string]bool {
	ids := map[string]bool{}
	for _, a := range accounts {
		if a.ID != "" {
			ids[a.ID] = true
		}
	}
	return ids
}

// sortRepos orders repositories by case-folded path, then path, then id.
func sortRepos(repos []*repoState) {
	sort.Slice(repos, func(i, j int) bool {
		a, b := repos[i].repo, repos[j].repo
		if la, lb := strings.ToLower(a.Path), strings.ToLower(b.Path); la != lb {
			return la < lb
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.ID < b.ID
	})
}
