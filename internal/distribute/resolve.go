package distribute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// resolve runs phase B and returns the targets to inspect, in the order
// they were found. Its error is one of the run itself: a targets.yml entry
// that does not parse, or a write credential that is not the writer
// (ErrWriterMismatch).
func (r *run) resolve(ctx context.Context) ([]*target, error) {
	sels, err := config.Selectors(r.hub, r.targets)
	if err != nil {
		return nil, err
	}
	found := map[string]*target{}
	var list []*target
	add := func(p *provider, s config.Selector, repo platform.Repo, pos int) {
		if repo.ID == "" {
			r.warnf("provider %s: targets.yml targets[%d]: the platform reported a repository without an id; it is left out", p.cfg.ID, s.Entry)
			return
		}
		host := strings.ToLower(repo.Host)
		if host == "" {
			host = p.cfg.Host
		}
		key := host + "\x00" + repo.ID
		t := found[key]
		if t == nil {
			t = &target{prov: p, repo: repo, host: host, first: [2]int{s.Entry, pos}}
			found[key] = t
			list = append(list, t)
		}
		if !slices.Contains(t.entries, s.Entry) {
			t.entries = append(t.entries, s.Entry)
		}
		t.assumed = t.assumed || s.Assumed
		if !slices.Contains(t.providers, p.cfg.ID) {
			t.providers = append(t.providers, p.cfg.ID)
		}
		if s.Repo != "" && !slices.Contains(t.written, s.Repo) {
			t.written = append(t.written, s.Repo)
		}
		// The first entry in targets.yml decides the order and, when two
		// providers reach the repository, which one handles it.
		if s.Entry < t.first[0] || (s.Entry == t.first[0] && pos < t.first[1]) {
			t.first = [2]int{s.Entry, pos}
			t.prov, t.repo = p, repo
		}
	}
	for _, p := range r.provs {
		if r.stop(ctx) {
			p.info.ResolveComplete = false
			continue
		}
		// Every read of the provider is paced by its Gate.
		pctx := throttle.With(ctx, p.throttle())
		if !r.connect(pctx, p) {
			if r.err != nil {
				return nil, r.err
			}
			continue
		}
		for _, s := range sels {
			if !r.owns(p, s) {
				continue
			}
			if r.stop(ctx) {
				p.info.ResolveComplete = false
				break
			}
			var more bool
			if s.Repo != "" {
				more = r.resolveRepo(pctx, p, s, add)
			} else {
				more = r.resolveNamespace(pctx, p, s, add)
			}
			if !more {
				break
			}
		}
	}
	for _, t := range list {
		// The handling provider comes first, as the report names it.
		if i := slices.Index(t.providers, t.prov.cfg.ID); i > 0 {
			t.providers = slices.Insert(slices.Delete(t.providers, i, i+1), 0, t.prov.cfg.ID)
		}
	}
	r.orphanSelectors(sels)
	return r.filter(list), nil
}

// stop reports whether the resolve must stop because ctx is done; the first
// time, it records why with one run warning.
func (r *run) stop(ctx context.Context) bool {
	err := ctx.Err()
	if err == nil {
		return false
	}
	if !r.stopped {
		r.stopped = true
		reason := "interrupted"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "deadline"
		}
		r.warnf("deferred:%s: the run stopped before every provider resolved its targets; the rest waits for the next run", reason)
	}
	return true
}

// providerFailed records that call what of provider p failed with err in
// phase B and reports whether the provider may go on resolving. A done ctx
// stops the run (stop); a call the throttle refused (the provider out of
// budget, or a pause past the deadline) and a rate limit the retries gave
// up on defer the provider's remaining resolve; an error that makes the
// provider unusable (unavailable true: a failed Probe or Self, an auth
// error) sets ProviderInfo.Error, or is a warning for an anonymous provider;
// any other error is a warning. Each clears ResolveComplete.
func (r *run) providerFailed(ctx context.Context, p *provider, what string, err error, unavailable bool) bool {
	p.info.ResolveComplete = false
	refusal, refused := throttle.Refused(err)
	switch {
	case r.stop(ctx):
		return false
	case refused:
		r.warnf("provider %s: deferred:%s: %s: %v; its remaining targets wait for the next run", p.cfg.ID, refusal.Reason, what, err)
		return false
	case platform.ClassOf(err) == platform.ClassRateLimited:
		r.warnf("provider %s: deferred:rate-limit: %s: %v; its remaining targets wait for the next run", p.cfg.ID, what, err)
		return false
	case unavailable && p.anonymous:
		r.warnf("provider %s: not checked without credentials: %s: %v", p.cfg.ID, what, err)
		return false
	case unavailable:
		if p.info.Error == "" {
			p.info.Error = err.Error()
		}
		return false
	}
	r.warnf("provider %s: %s: %v", p.cfg.ID, what, err)
	return true
}

// connect probes the provider, learns who its credential acts as and looks
// up its authors and automation accounts. It reports whether the provider
// can be used. In ModeDryRun and ModeDistribute the credential is the
// writer's: Self is provider.self, and it must be the writer hub.yml names,
// else r.err wraps ErrWriterMismatch and the run stops.
func (r *run) connect(ctx context.Context, p *provider) bool {
	var caps platform.Caps
	err := r.retry(ctx, p, func() error {
		var err error
		caps, err = p.reader.Probe(ctx)
		return err
	})
	if err != nil {
		return r.providerFailed(ctx, p, "probe", err, true)
	}
	p.caps = caps
	// The platform's pacing, with hub.yml's overrides.
	p.throttle().SetLimits(limitsOf(caps.Limits, p.cfg.Limits, r.d.Concurrency))
	var self platform.Account
	if !p.anonymous {
		err = r.retry(ctx, p, func() error {
			var err error
			self, err = p.reader.Self(ctx)
			return err
		})
		if err != nil {
			return r.providerFailed(ctx, p, "who the read credential is", err, true)
		}
		p.info.Reader = self.Login
		if r.mode != ModePlan {
			p.self = self
			p.info.Writer = self.Login
		}
	}
	writerKnown, ok := r.lookupWriter(ctx, p, self)
	if !ok {
		return false
	}
	for i, login := range p.cfg.KnownAuthors {
		a, found, ok := r.lookup(ctx, p, fmt.Sprintf("known_authors[%d]", i), login)
		if !ok {
			return false
		}
		if found {
			p.addAuthor(a)
		}
	}
	if !r.lookupAutomation(ctx, p) {
		return false
	}
	if r.mode == ModePlan && caps.ReaderCloses && self.ID != "" && !p.automation[self.ID] {
		r.warnf("provider %s: the read credential's account %s can decline pull requests on %s; list it in automation_accounts, "+
			"so that a pull request declined with the read key is not taken for the team's decision", p.cfg.ID, self.Login, caps.Flavor)
	}
	switch {
	case len(p.ids) == 0:
		r.warnf("provider %s: touchmark cannot recognize its own pull requests, so an open pull request on a sync branch counts as someone else's", p.cfg.ID)
	case !writerKnown:
		r.warnf("provider %s: only pull requests by known_authors are recognized as touchmark's", p.cfg.ID)
	}
	if r.mode != ModePlan {
		p.info.WriteCheck = "ok"
	}
	return true
}

// addAuthor records a as one of p's authors (its pull requests are
// touchmark's), once by stable id.
func (p *provider) addAuthor(a platform.Account) {
	if a.ID != "" && !slices.Contains(p.ids, a.ID) {
		p.authors = append(p.authors, a)
		p.ids = append(p.ids, a.ID)
	}
}

// lookupWriter looks up the writer hub.yml names for p and records it as
// an author; known reports that it resolved to an id, ok false that the
// run must stop. A plan warns when the writer is not named, or is the
// reader itself (self); in CI the latter stops it (ErrReaderIsWriter). A dry run or distribute stops instead (r.err wraps
// ErrWriterMismatch) when no writer is named or self, the account the
// write credential acts as, is not it.
func (r *run) lookupWriter(ctx context.Context, p *provider, self platform.Account) (known, ok bool) {
	w := p.cfg.Writer
	if w == "" {
		if r.mode != ModePlan && r.mode != modeDoctor {
			r.err = fmt.Errorf("provider %s: %w: hub.yml names no writer, so the pull requests this run would open could not be told from others'", p.cfg.ID, ErrWriterMismatch)
			return false, false
		}
		r.warnf("provider %s: hub.yml names no writer", p.cfg.ID)
		return false, true
	}
	a, found, ok := r.lookup(ctx, p, "the writer", w)
	if !ok {
		return false, false
	}
	if found {
		p.addAuthor(a)
		p.writerAcct = a
		known = a.ID != ""
		if r.mode == ModePlan && a.ID != "" && a.ID == self.ID {
			if r.inCI {
				r.err = fmt.Errorf("provider %s: %w %s: plan runs for every branch of the hub, so it needs an account of its own that can only read", p.cfg.ID, ErrReaderIsWriter, w)
				return false, false
			}
			r.warnf("provider %s: the read credential acts as the writer %s; plan needs an account of its own that can only read", p.cfg.ID, w)
		}
	}
	// Doctor reports a credential of another account as a failed check
	// (writerCheck) and checks on with it.
	if r.mode != ModePlan && r.mode != modeDoctor && !sameAccount(self, w, a, found) {
		r.err = fmt.Errorf("provider %s: %w: the write credential acts as %s, and hub.yml names %s", p.cfg.ID, ErrWriterMismatch, self.Login, w)
		return false, false
	}
	return known, true
}

// lookup resolves login (what names it in warnings) through p's reader.
// found is false when the login did not resolve; ok is false when the run
// must stop. A login that does not exist (or that an anonymous reader may
// not look up) is a warning; another failure is remembered in
// p.lookupErr: a pull request of an author touchmark cannot place is then
// not planned.
func (r *run) lookup(ctx context.Context, p *provider, what, login string) (a platform.Account, found, ok bool) {
	err := r.retry(ctx, p, func() error {
		var err error
		a, err = p.reader.Lookup(ctx, login)
		return err
	})
	switch {
	case err == nil:
		return a, true, true
	case r.stop(ctx):
		p.info.ResolveComplete = false
		return a, false, false
	case isNotFound(err) || (p.anonymous && refusedAnonymous(err)):
		// An unknown login, or one an anonymous reader may not look up:
		// its pull requests count as someone else's.
		r.warnf("provider %s: look up %s %s: %v", p.cfg.ID, what, login, err)
	default:
		r.warnf("provider %s: look up %s %s: %v; a pull request of an author touchmark cannot place is not planned", p.cfg.ID, what, login, err)
		if p.lookupErr == nil {
			p.lookupErr = err
		}
	}
	return a, false, true
}

// lookupAutomation resolves the provider's automation_accounts, whose closes
// are auto-closes. One that does not resolve is a warning: its closes count
// as declines, the safe side. It reports false when the run must stop.
func (r *run) lookupAutomation(ctx context.Context, p *provider) bool {
	for i, login := range p.cfg.AutomationAccounts {
		var a platform.Account
		err := r.retry(ctx, p, func() error {
			var err error
			a, err = p.reader.Lookup(ctx, login)
			return err
		})
		switch {
		case err == nil && a.ID != "":
			if p.automation == nil {
				p.automation = map[string]bool{}
			}
			p.automation[a.ID] = true
		case err != nil && r.stop(ctx):
			p.info.ResolveComplete = false
			return false
		case err != nil:
			r.warnf("provider %s: look up automation_accounts[%d] %s: %v; its closes count as declines", p.cfg.ID, i, login, err)
		}
	}
	return true
}

// sameAccount reports whether self, the account the write credential acts
// as, is the writer named login, which Lookup found as a (found): the same
// stable id, or the same login ignoring case.
func sameAccount(self platform.Account, login string, a platform.Account, found bool) bool {
	if found && a.ID != "" && a.ID == self.ID {
		return true
	}
	return self.Login != "" && strings.EqualFold(self.Login, login)
}

// refusedAnonymous reports whether err refuses a call for lack of
// credentials: an auth or permission error.
func refusedAnonymous(err error) bool {
	c := platform.ClassOf(err)
	return c == platform.ClassAuth || c == platform.ClassPermission
}

// owns reports whether selector s belongs to provider p: it names p, or it
// names none and p is the run's only provider.
func (r *run) owns(p *provider, s config.Selector) bool {
	if s.Provider == "" {
		return len(r.provs) == 1
	}
	return s.Provider == p.cfg.ID
}

// orphanSelectors warns about entries no provider of the run owns (check
// rejects such files; a hub built in code may have them).
func (r *run) orphanSelectors(sels []config.Selector) {
	for _, s := range sels {
		owned := slices.ContainsFunc(r.provs, func(p *provider) bool { return r.owns(p, s) })
		switch {
		case owned:
		case s.Provider == "":
			r.warnf("%s: targets[%d] names no provider, and the hub has several; it is left out", config.TargetsFile, s.Entry)
		default:
			r.warnf("%s: targets[%d] names provider %q, which hub.yml does not define; it is left out", config.TargetsFile, s.Entry, s.Provider)
		}
	}
}

// addFunc records a repository selector s found at position pos of its
// listing.
type addFunc func(p *provider, s config.Selector, repo platform.Repo, pos int)

// resolveRepo resolves a repo: entry and reports whether the provider may go
// on resolving.
func (r *run) resolveRepo(ctx context.Context, p *provider, s config.Selector, add addFunc) bool {
	if _, excluded := config.Excluded(r.hub, r.targets, p.cfg.ID, s.Repo); excluded {
		return true
	}
	var repo platform.Repo
	err := r.retry(ctx, p, func() error {
		var err error
		repo, err = p.reader.Repo(ctx, s.Repo)
		return err
	})
	switch {
	case err == nil:
		add(p, s, repo, 0)
		return true
	case ctx.Err() == nil && isNotFound(err):
		ref := p.cfg.ID + ":" + s.Repo
		p.info.Missing = append(p.info.Missing, ref)
		r.warnf("%s: target-missing: %s does not know targets.yml targets[%d]", ref, p.cfg.Host, s.Entry)
		return true
	}
	return r.providerFailed(ctx, p, fmt.Sprintf("look up %s (targets.yml targets[%d])", s.Repo, s.Entry), err,
		platform.ClassOf(err) == platform.ClassAuth)
}

// resolveNamespace resolves an org: or group: entry, keeping the
// repositories its match patterns select, and reports whether the provider
// may go on resolving.
func (r *run) resolveNamespace(ctx context.Context, p *provider, s config.Selector, add addFunc) bool {
	var res platform.Resolved
	err := r.retry(ctx, p, func() error {
		var err error
		res, err = p.reader.Resolve(ctx, platform.Selector{
			Namespace: s.Namespace,
			Subgroups: s.Subgroups,
			Topics:    slices.Clone(s.Topics),
			Forks:     s.Forks,
		})
		return err
	})
	if err != nil {
		return r.providerFailed(ctx, p, fmt.Sprintf("resolve targets.yml targets[%d] (%s)", s.Entry, s.Namespace), err,
			platform.ClassOf(err) == platform.ClassAuth)
	}
	if !res.Complete {
		p.info.ResolveComplete = false
		why := ""
		if res.Incomplete != "" {
			why = ": " + res.Incomplete
		}
		r.warnf("provider %s: the listing of targets.yml targets[%d] (%s) is incomplete%s", p.cfg.ID, s.Entry, s.Namespace, why)
	}
	for i, repo := range res.Repos {
		if s.Selects(repo.Path) {
			add(p, s, repo, i)
		}
	}
	return true
}

// filter drops excluded targets and, with Only, the targets it does not
// name; marks the targets a public hub must not name; and adds the
// warnings about duplicates and renames, and about exclude entries that
// name one repository while targets lie beneath them (beneathExcludes).
func (r *run) filter(list []*target) []*target {
	matched := make([]bool, len(r.d.Only))
	var kept []*target
	beneath := map[int]int{}
	for _, t := range list {
		if r.excluded(t) {
			continue
		}
		for _, i := range config.ExcludedBeneath(r.hub, r.targets, t.prov.cfg.ID, t.repo.Path) {
			beneath[i]++
		}
		if len(r.d.Only) > 0 {
			hit := false
			for i, ref := range r.d.Only {
				if r.names(t, ref) {
					matched[i], hit = true, true
				}
			}
			if !hit {
				continue
			}
		}
		t.hidden = r.hidePrivate && t.repo.Visibility != "public"
		kept = append(kept, t)
	}
	for i, ref := range r.d.Only {
		if !matched[i] {
			r.warnf("--only %s matches no target", ref)
		}
	}
	r.beneathExcludes(beneath)
	for _, t := range kept {
		if len(t.providers) > 1 {
			others := strings.Join(t.providers[1:], ", ")
			if t.hidden {
				r.warnf("duplicate-provider: a non-public target on %s is also listed by provider %s; %s handles it", t.host, others, t.prov.cfg.ID)
			} else {
				t.warnings = append(t.warnings, fmt.Sprintf("duplicate-provider: also listed by provider %s on the same host; %s handles it (its entry comes first in targets.yml)", others, t.prov.cfg.ID))
			}
		}
		if t.hidden {
			continue
		}
		for _, w := range t.written {
			if !strings.EqualFold(w, t.repo.Path) {
				t.warnings = append(t.warnings, fmt.Sprintf("renamed: targets.yml names it %s", w))
			}
		}
	}
	return kept
}

// beneathExcludes warns about each exclude entry that names one repository
// while n targets lie beneath it (beneath: entry index → n): it excludes
// none of them, and a namespace takes "/**". The warning names the entry,
// which targets.yml shows anyway, and counts the targets without naming
// them.
func (r *run) beneathExcludes(beneath map[int]int) {
	for i := range r.targets.Exclude {
		n := beneath[i]
		if n == 0 {
			continue
		}
		what := "1 target lies"
		if n > 1 {
			what = fmt.Sprintf("%d targets lie", n)
		}
		ex := r.targets.Exclude[i]
		r.warnf("%s: exclude[%d] (%s) names one repository and excludes nothing beneath it, where %s; to exclude a namespace, write %s/**",
			config.TargetsFile, i, ex, what, ex)
	}
}

// excluded reports whether exclude names t on any provider that found it,
// by its canonical path or a path a repo: entry wrote.
func (r *run) excluded(t *target) bool {
	for _, id := range t.providers {
		for _, path := range t.paths() {
			if _, ok := config.Excluded(r.hub, r.targets, id, path); ok {
				return true
			}
		}
	}
	return false
}

// names reports whether ref (from --only) names t.
func (r *run) names(t *target, ref config.Ref) bool {
	for _, id := range t.providers {
		for _, path := range t.paths() {
			if config.Names(r.hub, r.targets, ref, id, path) {
				return true
			}
		}
	}
	return false
}

// paths returns the canonical path of t and the paths repo: entries wrote.
func (t *target) paths() []string { return append([]string{t.repo.Path}, t.written...) }
