package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/distribute"
	"github.com/bedrock-python/touchmark/internal/hubch"
	"github.com/bedrock-python/touchmark/internal/platform"
)

// writerMergeGuard is plan's guard of a hub merge request under
// security.writer_on_hub guard (GitLab): the writer reaches the hub as a
// Developer, so it may open a merge request or push to the source branch
// of a maintainer's, and a leaked write key could slip a change into what
// a maintainer then merges. In a merge request pipeline of a hub whose
// default branch's hub.yml says guard, plan exits 2 when the merge request's
// author, or anyone who pushed to its source branch since the branch was
// created, is the writer of the provider on the hub's host or one of its
// known_authors; when that cannot be told (no such provider, a reader
// without the answer, events that do not reach the branch's creation), it
// exits 2 too: the guard fails closed. With "Pipelines must succeed" and
// no merge on a skipped pipeline, and the CI configuration read from the
// default branch (which doctor's hub-guard verifies), the merge waits.
//
// The setting, the writer and known_authors come from the default branch's
// hub.yml: the merge request's own could turn the guard off. When the
// default branch's cannot be read, the run's own decides, with a warning.
func (h *hub) writerMergeGuard(ctx context.Context, hctx hubch.Context, getenv func(string) string, pps []planProvider,
	providers []distribute.Provider) ([]string, error) {
	pr := hubPR(hctx, getenv)
	if hctx.CI != hubch.GitLabCI || hctx.Event != "merge_request_event" || pr <= 0 {
		return nil, nil
	}
	cfg, warnings := h.cfg, []string(nil)
	tip, err := h.defaultBranchTip(ctx, hctx)
	if err == nil {
		var base *config.Hub
		if base, err = h.hubAt(ctx, tip); err == nil {
			cfg = base
		}
	}
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("security.writer_on_hub is read from this merge request's %s: %v", config.HubFile, err))
	}
	if cfg == nil || cfg.Security.WriterOnHub != "guard" {
		return warnings, nil
	}
	refuse := func(format string, args ...any) error {
		return configErrorf("security.writer_on_hub guard: "+format, args...)
	}
	i := slices.IndexFunc(pps, func(pp planProvider) bool { return strings.EqualFold(pp.Host, hctx.Host) && pp.Type == "gitlab" })
	if i < 0 {
		return warnings, refuse("no GitLab provider of %s is on the hub's host %s, so plan cannot tell who pushed to !%d", config.HubFile, hctx.Host, pr)
	}
	auditor, ok := providers[i].Reader.(platform.MergeRequestAuditor)
	if !ok || pps[i].anonymous {
		return warnings, refuse("provider %s has no reader with a token here, so plan cannot tell who pushed to !%d", pps[i].ID, pr)
	}
	writer := pps[i].Writer
	if cfg.Writer != "" && len(cfg.Providers) == 0 {
		writer = cfg.Writer
	}
	if writer == "" {
		return warnings, refuse("provider %s names no writer, so plan cannot tell whether it pushed to !%d", pps[i].ID, pr)
	}
	logins := append([]string{writer}, pps[i].KnownAuthors...)
	var accounts []platform.Account
	for _, login := range logins {
		a, err := providers[i].Reader.Lookup(ctx, login)
		if err != nil {
			return warnings, refuse("look up %s on %s: %v", login, pps[i].ID, err)
		}
		accounts = append(accounts, a)
	}
	hub := platform.Repo{Host: hctx.Host, ID: hctx.RepoID, Path: hctx.RepoPath}
	got, err := auditor.MergeRequestPushes(ctx, hub, pr)
	if err != nil {
		return warnings, refuse("read !%d and the pushes to its source branch: %v", pr, err)
	}
	match := func(a platform.Account) (string, bool) {
		for _, w := range accounts {
			if a.ID != "" && a.ID == w.ID {
				return w.Login, true
			}
		}
		return "", false
	}
	if login, ok := match(got.Author); ok {
		return warnings, refuse("!%d was opened by %s, the writer or one of its known authors: a leaked write key must not reach the hub's default "+
			"branch; a maintainer opens it again from a branch of their own", pr, login)
	}
	for _, p := range got.Pushers {
		if login, ok := match(p); ok {
			return warnings, refuse("%s, the writer or one of its known authors, pushed to %s, the source branch of !%d: a leaked write key must not "+
				"reach the hub's default branch; a maintainer recreates the branch from commits they checked", login, got.Branch, pr)
		}
	}
	if !got.Complete {
		return warnings, refuse("plan cannot tell everyone who pushed to %s, the source branch of !%d (%s): the guard fails closed; "+
			"push the change to a new branch and open the merge request again", got.Branch, pr, got.Why)
	}
	return warnings, nil
}
