package fake

import (
	"context"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/platform"
)

// The write identity's checks for doctor (platform.Checker and
// platform.KeyChecker): every Writer of the fake implements them.
//
// Check with no repository reports SetIdentityFindings' findings (by
// default a token that does not expire). With repositories it reports per
// repository "access" (ok when the account may push and write pull
// requests: Grant, GrantWrite), on flavors with Caps.WorkflowPerm
// "workflows", and "rules" from the rulesets of the branches asked:
// required signatures, blocked force pushes on a branch other than the
// default one (accounts on a ruleset's bypass list are not held to it), or
// unknown when HideRules hid them. CheckSigningKey reports the keys
// SetSigningKeys registered for the account.

// SetIdentityFindings sets what Check reports about the identity itself
// (Finding.Repo ""), for every account.
func (p *Platform) SetIdentityFindings(fs ...platform.Finding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = slices.Clone(fs)
	if p.identity == nil {
		p.identity = []platform.Finding{}
	}
}

// SetSigningKeys registers public signing keys ("ssh-ed25519 AAAA…") for
// account, replacing the ones it had.
func (p *Platform) SetSigningKeys(account platform.Account, keys ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accounts[account.ID] == nil {
		p.setupf("SetSigningKeys: unknown account %q", account.ID)
		return
	}
	if p.signingKeys == nil {
		p.signingKeys = map[string][]string{}
	}
	p.signingKeys[account.ID] = slices.Clone(keys)
}

// Check implements platform.Checker.
func (w *writer) Check(ctx context.Context, repos []platform.Repo, branches []string) ([]platform.Finding, error) {
	var args []string
	for _, r := range repos {
		args = append(args, r.Path)
	}
	return do(ctx, w.p, &w.as, "Check", args, func() ([]platform.Finding, error) {
		p := w.p
		if len(repos) == 0 {
			if p.identity != nil {
				return slices.Clone(p.identity), nil
			}
			return []platform.Finding{{Check: "token-expiry", Status: platform.FindingOK, Detail: "the fake's tokens do not expire"}}, nil
		}
		var out []platform.Finding
		for _, r := range repos {
			s, err := p.repoOf(ops["Check"], r)
			if err != nil {
				if platform.ClassOf(err) == platform.ClassNotFound {
					out = append(out, platform.Finding{Repo: r.Path, Check: "access", Status: platform.FindingFail, Detail: "the writer does not see this repository"})
					continue
				}
				return nil, err
			}
			out = append(out, p.checkRepo(s, w.as, branches)...)
		}
		return out, nil
	})
}

// checkRepo reports the checks of one repository for account.
func (p *Platform) checkRepo(s *repoState, account platform.Account, branches []string) []platform.Finding {
	path := s.repo.Path
	have := s.grants[account.ID]
	out := []platform.Finding{}
	if have.Contents && have.PRs {
		out = append(out, platform.Finding{Repo: path, Check: "access", Status: platform.FindingOK, Detail: account.Login + " may push and write pull requests"})
	} else {
		out = append(out, platform.Finding{Repo: path, Check: "access", Status: platform.FindingFail, Detail: account.Login + " may not push and write pull requests"})
	}
	if p.caps.WorkflowPerm {
		switch {
		case p.workflowsHidden:
			out = append(out, platform.Finding{Repo: path, Check: "workflows", Status: platform.FindingUnknown, Detail: "the Workflows permission is not readable"})
		case have.Workflows:
			out = append(out, platform.Finding{Repo: path, Check: "workflows", Status: platform.FindingOK, Detail: "workflows:write"})
		default:
			out = append(out, platform.Finding{Repo: path, Check: "workflows", Status: platform.FindingWarn, Detail: "no Workflows permission"})
		}
	}
	if s.rulesHidden {
		return append(out, platform.Finding{Repo: path, Check: "rules", Status: platform.FindingUnknown, Detail: "the branch rules are not readable"})
	}
	var notes []string
	status := platform.FindingOK
	for _, b := range append([]string{s.repo.DefaultBranch}, branches...) {
		for _, rs := range s.rulesets {
			if !rs.matches(b, s.repo.DefaultBranch) || slices.ContainsFunc(rs.Bypass, func(a platform.Account) bool { return a.ID == account.ID }) {
				continue
			}
			if rs.RequiredSignatures && !slices.Contains(notes, "signed commits required on "+b) {
				notes = append(notes, "signed commits required on "+b)
			}
			if rs.NonFastForward && b != s.repo.DefaultBranch && !slices.Contains(notes, "force pushes blocked on "+b) {
				notes = append(notes, "force pushes blocked on "+b)
				status = platform.FindingWarn
			}
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "no rule stands in the way")
	}
	return append(out, platform.Finding{Repo: path, Check: "rules", Status: status, Detail: strings.Join(notes, "; ")})
}

// CheckSigningKey implements platform.KeyChecker.
func (w *writer) CheckSigningKey(ctx context.Context, publicKey string) (platform.Finding, error) {
	return do(ctx, w.p, &w.as, "CheckSigningKey", nil, func() (platform.Finding, error) {
		if slices.Contains(w.p.signingKeys[w.as.ID], strings.TrimSpace(publicKey)) {
			return platform.Finding{Check: "signing-key", Status: platform.FindingOK, Detail: "the key is " + w.as.Login + "'s signing key"}, nil
		}
		return platform.Finding{Check: "signing-key", Status: platform.FindingFail, Detail: "the key is not among " + w.as.Login + "'s keys"}, nil
	})
}

var (
	_ platform.Checker    = (*writer)(nil)
	_ platform.KeyChecker = (*writer)(nil)
)
