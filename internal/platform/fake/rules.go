package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// Branch rules and API commits: what GitHub's driver offers beyond the
// Reader and Writer contracts (platform.Preflighter and
// platform.Committer), modeled on the GitHub flavor.
//
// A repository holds rulesets (AddRuleset). In git mode the git server
// enforces them on every push through it, as GitHub's GH013 refusal: a
// ruleset with RequiredSignatures refuses a branch update that brings a
// commit without a signature header (gpgsig: the fake checks the header,
// not the signature), and one with NonFastForward refuses an update that
// is not a fast-forward; deletions and hidden refs (refs/touchmark/…) pass,
// and accounts on the ruleset's bypass list are not held to it.
//
// WithPreflight makes Reader and Writer platform.Preflighters: Preflight
// reports the rulesets of the branches asked (for a branch that does not
// exist too), and a writer the Workflows permission of its account on
// flavors with Caps.WorkflowPerm. HideRules and HideWorkflows make those
// unknown, as for an identity that cannot read them.
//
// A ruleset with NoPush is a protected branch the accounts it holds may
// not push to at all, as GitLab's and Gitea's branch protection: the git
// server refuses their pushes to it, creations and deletions included, with
// GitLab's message. WithPushGuard makes Writer a platform.PushGuard that
// reports those branches (HideRules hides them).
//
// WithAPICommits sets Caps.Commit and makes the per-target writers of
// Target platform.Committers (git mode only): Commit makes the commit on
// the platform, signed (a gpgsig header) unless SetAPISigning turned
// signing off, authored by the writer and committed by the platform, and
// moves the branch with compare-and-swap, deleting the stage ref, under the
// same rules as a push.

// Ruleset is a branch ruleset of a repository.
type Ruleset struct {
	// Branches are the branches it targets: "~ALL", "~DEFAULT_BRANCH", or
	// a name where '*' matches any run of characters ('/' included, unlike
	// GitHub's fnmatch) and '?' one.
	Branches []string
	// RequiredSignatures refuses commits without a signature.
	RequiredSignatures bool
	// NonFastForward refuses updates that are not fast-forwards.
	NonFastForward bool
	// Deletion refuses deleting the branch.
	Deletion bool
	// NoPush refuses every push to the branch, as a protected branch that
	// does not let the account push (GitLab, Gitea).
	NoPush bool
	// Bypass lists the accounts the ruleset does not hold (an App on the
	// bypass list).
	Bypass []platform.Account
}

// WithPreflight makes Reader and Writer implement platform.Preflighter
// (see Ruleset).
func WithPreflight() Option {
	return func(p *Platform) { p.preflight = true }
}

// WithPushGuard makes Writer implement platform.PushGuard (see Ruleset).
func WithPushGuard() Option {
	return func(p *Platform) { p.pushGuard = true }
}

// WithAPICommits makes the per-target writers implement
// platform.Committer and sets Caps.Commit (API, SignedByPlatform, CAS),
// after any WithFlavor.
func WithAPICommits() Option {
	return func(p *Platform) { p.apiCommits = true }
}

// AddRuleset adds a ruleset to the repository with id.
func (p *Platform) AddRuleset(repoID string, rs Ruleset) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.setupRepo("AddRuleset", repoID)
	if s == nil {
		return
	}
	for _, b := range rs.Branches {
		if b == "" {
			p.setupf("AddRuleset(%s): an empty branch pattern", repoID)
			return
		}
	}
	for _, a := range rs.Bypass {
		if p.accounts[a.ID] == nil {
			p.setupf("AddRuleset(%s): unknown bypass account %q", repoID, a.ID)
			return
		}
	}
	rs.Branches, rs.Bypass = slices.Clone(rs.Branches), slices.Clone(rs.Bypass)
	s.rulesets = append(s.rulesets, rs)
}

// SetRulesets replaces every ruleset of the repository with id by rs (none
// removes them all).
func (p *Platform) SetRulesets(repoID string, rs ...Ruleset) {
	p.mu.Lock()
	s := p.setupRepo("SetRulesets", repoID)
	if s != nil {
		s.rulesets = nil
	}
	p.mu.Unlock()
	for _, r := range rs {
		p.AddRuleset(repoID, r)
	}
}

// HideRules makes Preflight report the rules of the repository with id as
// unknown (Rules.Known false); they still apply.
func (p *Platform) HideRules(repoID string, hidden bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.setupRepo("HideRules", repoID); s != nil {
		s.rulesHidden = hidden
	}
}

// HideWorkflows makes a writer's Preflight leave the Workflows permission
// unknown (Rules.WorkflowsKnown false), as for a token.
func (p *Platform) HideWorkflows(hidden bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workflowsHidden = hidden
}

// SetAPISigning turns the signature of API commits on (the default) or
// off, as GitHub Enterprise Server without web commit signing.
func (p *Platform) SetAPISigning(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apiUnsigned = !on
}

// HiddenRefs returns the refs of the repository's bare repository outside
// refs/heads, as "<ref> <commit>", sorted: the stage refs an API commit
// leaves behind, say. Nil in memory mode.
func (p *Platform) HiddenRefs(repoID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.repos[repoID]
	if s == nil || p.git == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := p.repoGit(s).g.Run(ctx, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		p.setupf("HiddenRefs(%s): %v", repoID, err)
		return nil
	}
	var refs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "refs/heads/") {
			refs = append(refs, line)
		}
	}
	slices.Sort(refs)
	return refs
}

// matches reports whether the ruleset targets branch of a repository whose
// default branch is def.
func (rs Ruleset) matches(branch, def string) bool {
	for _, pat := range rs.Branches {
		switch pat {
		case "~ALL":
			return true
		case "~DEFAULT_BRANCH":
			if branch == def {
				return true
			}
			continue
		}
		if patternRe(pat).MatchString(branch) {
			return true
		}
	}
	return false
}

// patternRe compiles a branch pattern: '*' any run, '?' one character.
func patternRe(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pat {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// holds reports whether the ruleset holds account (it is not on the bypass
// list).
func (rs Ruleset) holds(account platform.Account) bool {
	return !slices.ContainsFunc(rs.Bypass, func(a platform.Account) bool { return a.ID == account.ID })
}

// hookRules returns the patterns of the branches on which the rulesets of s
// that hold account require signatures, forbid force pushes, forbid
// deletions and forbid any push, in the shell's case syntax of the
// pre-receive hook. Called with mu held.
func (p *Platform) hookRules(s *repoState, account platform.Account) (signed, noForce, noDelete, noPush []string) {
	for _, rs := range s.rulesets {
		if !rs.holds(account) {
			continue
		}
		var pats []string
		for _, pat := range rs.Branches {
			switch pat {
			case "~ALL":
				pats = append(pats, "*")
			case "~DEFAULT_BRANCH":
				pats = append(pats, shellQuotePattern(s.repo.DefaultBranch))
			default:
				pats = append(pats, pat)
			}
		}
		if rs.RequiredSignatures {
			signed = append(signed, pats...)
		}
		if rs.NonFastForward {
			noForce = append(noForce, pats...)
		}
		if rs.Deletion {
			noDelete = append(noDelete, pats...)
		}
		if rs.NoPush {
			noPush = append(noPush, pats...)
		}
	}
	return signed, noForce, noDelete, noPush
}

// shellQuotePattern escapes the characters a branch name may hold that a
// shell case pattern reads as special.
func shellQuotePattern(name string) string {
	return strings.NewReplacer("*", `\*`, "?", `\?`, "[", `\[`).Replace(name)
}

// rulesOf is Preflight of the Preflighter forms of Reader and Writer: the
// rules of branches of r, and for a writer its Workflows permission.
func (p *Platform) rulesOf(ctx context.Context, as platform.Account, r platform.Repo, branches []string, writer bool) (platform.Rules, error) {
	op := ops["Preflight"]
	return do(ctx, p, &as, "Preflight", append([]string{r.Path}, branches...), func() (platform.Rules, error) {
		s, err := p.repoOf(op, r)
		if err != nil {
			return platform.Rules{}, err
		}
		var out platform.Rules
		if !s.rulesHidden {
			out.Known = true
			for _, b := range dedupe(branches) {
				for _, rs := range s.rulesets {
					if !rs.matches(b, s.repo.DefaultBranch) {
						continue
					}
					out.SignedCommits = out.SignedCommits || rs.RequiredSignatures
					if rs.NonFastForward && !slices.Contains(out.NoForcePush, b) {
						out.NoForcePush = append(out.NoForcePush, b)
					}
					if rs.Deletion && !slices.Contains(out.NoDelete, b) {
						out.NoDelete = append(out.NoDelete, b)
					}
				}
			}
		}
		if writer && p.caps.WorkflowPerm && !p.workflowsHidden {
			out.WorkflowsKnown, out.Workflows = true, s.grants[as.ID].Workflows
		}
		return out, nil
	})
}

// noPushOf is NoPush of the PushGuard forms of Writer: the branches of r,
// the default branch aside, that a NoPush ruleset holding as covers, each
// with the ruleset's first pattern that matches it; none while the rules
// are hidden.
func (p *Platform) noPushOf(ctx context.Context, as platform.Account, r platform.Repo, branches []string) ([]platform.Protected, error) {
	op := ops["NoPush"]
	return do(ctx, p, &as, "NoPush", append([]string{r.Path}, branches...), func() ([]platform.Protected, error) {
		s, err := p.repoOf(op, r)
		if err != nil {
			return nil, err
		}
		if s.rulesHidden {
			return nil, nil
		}
		var out []platform.Protected
		for _, b := range dedupe(branches) {
			if b == "" || b == s.repo.DefaultBranch {
				continue
			}
			for _, rs := range s.rulesets {
				if !rs.NoPush || !rs.holds(as) || !rs.matches(b, s.repo.DefaultBranch) {
					continue
				}
				rule := rs.Branches[0]
				for _, pat := range rs.Branches {
					if (Ruleset{Branches: []string{pat}}).matches(b, s.repo.DefaultBranch) {
						rule = pat
						break
					}
				}
				out = append(out, platform.Protected{Branch: b, Rule: rule})
				break
			}
		}
		return out, nil
	})
}

// guardWriter is a writer that tells the branches it may not push to
// (WithPushGuard); preflightGuardWriter also reads the rules
// (WithPreflight).
type (
	guardWriter          struct{ writer }
	preflightGuardWriter struct{ preflightWriter }
)

func (w *guardWriter) NoPush(ctx context.Context, repo platform.Repo, branches []string) ([]platform.Protected, error) {
	return w.p.noPushOf(ctx, w.as, repo, branches)
}

func (w *preflightGuardWriter) NoPush(ctx context.Context, repo platform.Repo, branches []string) ([]platform.Protected, error) {
	return w.p.noPushOf(ctx, w.as, repo, branches)
}

// preflightReader is a reader that reads the rules (WithPreflight).
type preflightReader struct{ reader }

func (r *preflightReader) Preflight(ctx context.Context, repo platform.Repo, branches []string) (platform.Rules, error) {
	return r.p.rulesOf(ctx, r.as, repo, branches, false)
}

// preflightWriter is a writer that reads the rules and its Workflows
// permission (WithPreflight).
type preflightWriter struct{ writer }

func (w *preflightWriter) Preflight(ctx context.Context, repo platform.Repo, branches []string) (platform.Rules, error) {
	return w.p.rulesOf(ctx, w.as, repo, branches, true)
}

// apiTarget is a per-target writer that makes API commits
// (WithAPICommits).
type apiTarget struct{ *target }

// Commit makes req's commit on the platform and moves req.Branch to it
// (platform.Committer). Git mode only, with a writer minted
// with Contents. The tree and the parent must be in the repository (the
// core pushed them to req.Stage); the commit is authored by the writer,
// committed by the platform, dated as the staged commit (so that a request
// made again makes the same commit), and signed unless SetAPISigning
// turned it off: an unsigned commit deletes req.Stage and fails with
// platform.ErrUnsigned (ClassUnsupported, Rule "cannot-sign") and the
// branch where it was. Then, as GitHub's updateRefs: the branch must be at
// req.Expect ("" for none: ClassConflict otherwise, the stage ref kept);
// the Workflows permission guards changes under .github/workflows
// (ClassPermission, Rule "workflows"); a ruleset against force pushes that
// holds the writer refuses a move that is no fast-forward (ClassPolicy,
// Rule "GH013"); then the branch moves, req.Stage is deleted, pull
// requests follow, and the move is judged as a push through the server
// (Violations). The call log has "Commit <repo> <branch>" and, once the
// branch moved, "UpdateRefs <repo> <branch>": two writes.
func (t apiTarget) Commit(ctx context.Context, r platform.Repo, req platform.CommitRequest) (platform.Commit, error) {
	op := ops["Commit"]
	path := t.repoArg()
	// GitHub's driver writes the commit, then moves the refs when the
	// commit is what it must be: two writes, one when the
	// commit comes back unsigned or the call fails.
	if err := t.p.meterWrites(ctx, "Commit", throttle.API, 1); err != nil {
		return platform.Commit{}, err
	}
	c, err := t.commit(ctx, op, path, r, req)
	if err == nil {
		// The refs moved already: a refusal of the meter only leaves the
		// write uncounted.
		_ = t.p.meterWrites(ctx, "Commit", throttle.API, 1)
	}
	return c, err
}

// commit makes the API commit of Commit.
func (t apiTarget) commit(ctx context.Context, op, path string, r platform.Repo, req platform.CommitRequest) (platform.Commit, error) {
	p := t.p
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	return do(ctx, p, &t.as, "Commit", []string{path, req.Branch}, func() (platform.Commit, error) {
		s := p.repos[t.repoID]
		switch {
		case t.closed:
			return platform.Commit{}, revoked(op)
		case s == nil:
			return platform.Commit{}, notFound(op, "repository id %s", t.repoID)
		case r.ID != t.repoID:
			return platform.Commit{}, invalid(op, "the writer is for repository %s, not %s", t.repoID, r.ID)
		case !t.perms.Contents:
			return platform.Commit{}, denied(op, "contents", "the token of %s was minted without contents", s.repo.Path)
		case p.git == nil:
			return platform.Commit{}, &platform.Error{Op: op, Class: platform.ClassUnsupported, Err: errors.New("API commits need git mode")}
		case s.repo.Archived:
			return platform.Commit{}, denied(op, "archived", "%s is archived", s.repo.Path)
		}
		if err := p.allowed(op, s, t.as, platform.Perms{Contents: true}); err != nil {
			return platform.Commit{}, err
		}
		if err := checkCommitRequest(req); err != nil {
			return platform.Commit{}, invalid(op, "%v", err)
		}
		return p.apiCommit(ctx, op, t, s, req)
	})
}

// checkCommitRequest refuses a request GitHub's API would refuse.
func checkCommitRequest(req platform.CommitRequest) error {
	switch {
	case checkBranchName(req.Branch) != nil:
		return fmt.Errorf("%q is not a branch name", req.Branch)
	case !isHexID(req.Parent) || !isHexID(req.Tree):
		return errors.New("the parent and the tree must be full object ids")
	case req.Expect != "" && !isHexID(req.Expect):
		return fmt.Errorf("%q is not a full object id", req.Expect)
	case strings.TrimSpace(req.Message) == "":
		return errors.New("the message is empty")
	case req.Stage != "" && (!strings.HasPrefix(req.Stage, "refs/touchmark/") || strings.Contains(req.Stage, "..")):
		return fmt.Errorf("the stage ref %q is not under refs/touchmark/", req.Stage)
	}
	return nil
}

// apiCommit carries out Commit. Called with gitMu and mu held.
func (p *Platform) apiCommit(ctx context.Context, op string, t apiTarget, s *repoState, req platform.CommitRequest) (platform.Commit, error) {
	rg := p.repoGit(s)
	tree, parent := strings.ToLower(req.Tree), strings.ToLower(req.Parent)
	if _, err := rg.g.Run(ctx, nil, "cat-file", "-e", tree+"^{tree}"); err != nil {
		return platform.Commit{}, invalid(op, "tree %s does not exist in %s", tree, s.repo.Path)
	}
	if ok, err := rg.isCommit(ctx, parent); err != nil || !ok {
		return platform.Commit{}, invalid(op, "parent %s does not exist in %s", parent, s.repo.Path)
	}
	// Dated as the staged commit (else as the parent): the same request
	// makes the same commit, so that runs converge on one head. GitHub
	// dates it now.
	dated := parent
	if req.Stage != "" {
		if _, err := rg.g.Run(ctx, nil, "rev-parse", "-q", "--verify", req.Stage+"^{commit}"); err == nil {
			dated = req.Stage
		}
	}
	out, err := rg.g.Run(ctx, nil, "show", "-s", "--format=%ct", dated)
	if err != nil {
		return platform.Commit{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return platform.Commit{}, fmt.Errorf("%s: %s: the date of %s: %w", op, s.repo.Path, dated, err)
	}
	when := time.Unix(secs, 0).UTC()
	author := p.person(t.as, when)
	signed := !p.apiUnsigned
	sha, err := rg.signedCommit(ctx, tree, parent, author, ident{name: "GitHub", email: "noreply@" + p.host, when: when}, req.Message, signed)
	if err != nil {
		return platform.Commit{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	dropStage := func() {
		if req.Stage != "" {
			_, _ = rg.g.Run(ctx, nil, "update-ref", "-d", req.Stage)
		}
	}
	if !signed {
		dropStage()
		return platform.Commit{SHA: sha, Tree: tree, CAS: true}, &platform.Error{Op: op, Class: platform.ClassUnsupported, Rule: "cannot-sign",
			Err: fmt.Errorf("%s: the platform did not sign commit %s; the branch did not move: %w", s.repo.Path, sha, platform.ErrUnsigned)}
	}
	cur := s.refs[req.Branch]
	if cur != strings.ToLower(req.Expect) {
		return platform.Commit{}, &platform.Error{Op: op, Class: platform.ClassConflict,
			Err: fmt.Errorf("%s: branch %s is at %q, not at %q", s.repo.Path, req.Branch, cur, req.Expect)}
	}
	from := cur
	if from == "" {
		from = s.refs[s.repo.DefaultBranch]
	}
	if p.caps.WorkflowPerm && (!t.perms.Workflows || !s.grants[t.as.ID].Workflows) && from != "" {
		out, err := rg.g.Run(ctx, nil, "diff-tree", "-r", "--name-only", "--no-renames", from, sha, "--", ".github/workflows")
		if err != nil {
			return platform.Commit{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
		}
		if first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); first != "" {
			return platform.Commit{}, denied(op, "workflows", "refusing to allow a GitHub App to create or update workflow `%s` without `workflows` permission", first)
		}
	}
	if cur != "" {
		ff, err := rg.isAncestor(ctx, cur, sha)
		if err != nil {
			return platform.Commit{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
		}
		for _, rs := range s.rulesets {
			if !ff && rs.NonFastForward && rs.holds(t.as) && rs.matches(req.Branch, s.repo.DefaultBranch) {
				return platform.Commit{}, &platform.Error{Op: op, Class: platform.ClassPolicy, Rule: "GH013",
					Err: fmt.Errorf("repository rule violations found for refs/heads/%s: Cannot force-push to this branch", req.Branch)}
			}
		}
	}
	if err := rg.updateRef(ctx, req.Branch, sha, cur); err != nil {
		return platform.Commit{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	dropStage()
	p.logCall("UpdateRefs", s.repo.Path, req.Branch)
	if err := p.refsMoved(ctx, s, []refChange{{branch: req.Branch, old: cur, new: sha}}, &t.as, true, false); err != nil {
		return platform.Commit{}, fmt.Errorf("%s: %s: %w", op, s.repo.Path, err)
	}
	return platform.Commit{SHA: sha, Tree: tree, Verified: true, CAS: true}, nil
}

// signedCommit writes a commit of tree on parent, with a signature header
// when signed (the fake's stand-in for the platform's signature), and
// returns its id.
func (r *repoGit) signedCommit(ctx context.Context, tree, parent string, author, committer ident, msg string, signed bool) (string, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\nparent %s\nauthor %s\ncommitter %s\n", tree, parent, author, committer)
	if signed {
		b.WriteString("gpgsig -----BEGIN PGP SIGNATURE-----\n \n fake platform signature\n -----END PGP SIGNATURE-----\n")
	}
	b.WriteString("\n")
	b.WriteString(msg)
	out, err := r.g.Run(ctx, &b, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return r.id("git hash-object", out)
}
