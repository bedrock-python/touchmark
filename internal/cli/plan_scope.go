package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/provenance"
	"github.com/bedrock-python/touchmark/internal/report"
)

// The scope of plan in a hub pull request: the hub commit
// is compared with its merge base with the default branch, and the paths
// the pull request changes (changedSince: the net diff, and the pack paths
// of every commit in between) decide which targets are processed in full
// (distribute.ScopeOf).

// planOptions are the flags of plan beyond those it shares with the other
// commands.
type planOptions struct {
	// all processes every target of a hub pull request (--all).
	all bool
	// comment keeps plan's comment in the hub pull request (--comment).
	comment bool
	// assumeOptIn plans every target as opted in (--assume-opt-in).
	assumeOptIn bool
}

// planCommand is the plan command. Its flag values beyond the shared
// options live with the command itself.
func planCommand() *command {
	p := &planOptions{}
	return &command{
		name: "plan",
		synopsis: "[--hub DIR] [--worktree] [--only REF]... [--hub-fp HOST/ID] [--strict] [--all] [--comment] [--assume-opt-in] " +
			"[--format text|json|markdown]",
		summary: "report what distribute would do in every target; changes nothing",
		flags: func() (*flagSet, *options) {
			f, o := planFlags()
			*p = planOptions{}
			f.fs.BoolVar(&p.all, "all", false, "in a hub pull request, process every target, not only those of the packs it changes")
			f.fs.BoolVar(&p.comment, "comment", false, "keep one comment with the report in the hub pull request, through the CI's own token")
			f.fs.BoolVar(&p.assumeOptIn, "assume-opt-in", false, "plan every target as if it had an empty opt-in file when it has none (for the report only)")
			return f, o
		},
		run: func(ctx context.Context, e *env, o *options) error { return runPlan(ctx, e, o, p) },
	}
}

// pullRequestRun reports whether a CI run builds a hub pull or merge
// request.
func pullRequestRun(hctx hubch.Context) bool {
	ev := hctx.Event
	return hctx.CI != hubch.Local && hctx.CI != "" &&
		(strings.HasPrefix(ev, "pull_request") || ev == "merge_request_event" || ev == "external_pull_request_event")
}

// planScope returns the scope of a plan (distribute.Deps.Scope): every
// target with --all; none (nil: every target, no scope in the report)
// outside a hub pull request; else the scope of the paths the pull request
// changes. tip reads the tip of the default branch through the hub channel
// (nil without one). A scope that cannot be computed plans every target,
// with a warning.
func (h *hub) planScope(ctx context.Context, hctx hubch.Context, getenv func(string) string, p *planOptions, tip func(context.Context) (string, error)) (*distribute.Scope, []string) {
	switch {
	case p.all:
		return &distribute.Scope{Mode: report.ScopeAll, Reason: "--all"}, nil
	case !pullRequestRun(hctx):
		return nil, nil
	case h.src == provenance.WorkTree:
		return &distribute.Scope{Mode: report.ScopeAll, Reason: "--worktree plans the work tree, not a pull request"}, nil
	}
	base, err := h.mergeBase(ctx, hctx, getenv, tip)
	if err == nil {
		var changed []string
		if changed, err = h.changedSince(ctx, base); err == nil {
			s := distribute.ScopeOf(changed, h.cfg)
			s.Base = base
			return &s, nil
		}
	}
	why := fmt.Sprintf("the pull request's scope is unknown: %v", err)
	return &distribute.Scope{Mode: report.ScopeAll, Reason: why}, []string{why + "; every target is planned"}
}

// mergeBase returns the merge base of the hub commit with the tip of the
// hub's default branch: the tip the hub channel reads when this clone has
// that commit, else the branch in the clone (refs/remotes/origin/<branch>,
// then refs/heads/<branch>), else GitLab's CI_MERGE_REQUEST_DIFF_BASE_SHA.
func (h *hub) mergeBase(ctx context.Context, hctx hubch.Context, getenv func(string) string, tip func(context.Context) (string, error)) (string, error) {
	var candidates []string
	if tip != nil {
		if oid, err := tip(ctx); err == nil && isFullOID(oid) {
			candidates = append(candidates, oid)
		}
	}
	branch := hctx.DefaultBranch
	if branch != "" && safeBranch(branch) {
		candidates = append(candidates, "refs/remotes/origin/"+branch, "refs/heads/"+branch)
	}
	if hctx.CI == hubch.GitLabCI {
		if oid := strings.TrimSpace(getenv("CI_MERGE_REQUEST_DIFF_BASE_SHA")); isFullOID(oid) {
			candidates = append(candidates, oid)
		}
	}
	for _, c := range candidates {
		oid, err := h.git.RevParse(ctx, c+"^{commit}")
		if err != nil {
			continue
		}
		out, err := h.git.Run(ctx, nil, "merge-base", h.commit, oid)
		if err != nil {
			return "", fmt.Errorf("git merge-base %s %s: %w", short(h.commit), short(oid), err)
		}
		if base := strings.TrimSpace(string(out)); isFullOID(base) {
			return strings.ToLower(base), nil
		}
	}
	switch {
	case branch == "":
		return "", errors.New("the CI does not name the hub's default branch")
	case !safeBranch(branch):
		return "", fmt.Errorf("the default branch %q is not a branch name touchmark reads", branch)
	}
	return "", fmt.Errorf("the default branch %s is not in this clone (neither refs/remotes/origin/%s nor refs/heads/%s); fetch it, e.g. git fetch origin %s:refs/remotes/origin/%s",
		branch, branch, branch, branch, branch)
}

// changedSince lists the paths the pull request changes: those that differ
// between commit base and the hub commit (git diff --name-only
// --no-renames: a rename is its two paths), and the paths under packs/ that
// any commit of base..commit changes, merges against each parent and the
// commits of merged side branches included. A pack file's every version in
// the hub's history decides what targets get (provenance: a target's file
// that matches a version its packs once shipped is theirs to update or
// delete), so a version a commit adds and a later one takes back changes
// targets once the pull request is merged, though the net diff is empty.
func (h *hub) changedSince(ctx context.Context, base string) ([]string, error) {
	out, err := h.git.Run(ctx, nil, "-c", "core.quotePath=false", "diff", "--name-only", "--no-renames", "-z", base, h.commit, "--")
	if err != nil {
		return nil, fmt.Errorf("git diff %s %s: %w", short(base), short(h.commit), err)
	}
	// The flags of provenance's walk (gitx.History): every commit, merges
	// against each parent, no renames.
	hist, err := h.git.Run(ctx, nil, "-c", "core.quotePath=false", "-c", "log.diffMerges=separate", "-c", "diff.relative=false",
		"--noglob-pathspecs", "log", "--full-history", "-m", "--name-only", "--no-renames", "-z", "--format=", "--no-color",
		base+".."+h.commit, "--", ":(top)"+provenance.PacksDir)
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", short(base), short(h.commit), err)
	}
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, p := range bytes.Split(out, []byte{0}) {
		add(string(p))
	}
	// Commits are separated by empty tokens, or newlines before a path
	// (every path here starts with packs/).
	for _, p := range bytes.Split(hist, []byte{0}) {
		add(string(bytes.TrimLeft(p, "\n")))
	}
	return paths, nil
}
