package distribute

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/marker"
	"github.com/bedrock-python/touchmark/internal/platform"
	"github.com/bedrock-python/touchmark/internal/report"
	"github.com/bedrock-python/touchmark/internal/throttle"
)

// modeDoctor is the run of Doctor: the write identity reads, and a
// credential that is not the writer hub.yml names is a check that fails,
// not the end of the run.
const modeDoctor = ModeDistribute + 1

// DoctorDeps is what Doctor needs.
type DoctorDeps struct {
	// Deps are the run's: Hub, Targets, HubContext, Fingerprint,
	// Providers (each with its Writer), Only, Engine, Now, Concurrency and
	// of Write the Signers, Deadline and Redact. Nothing else is used.
	Deps
	// HubHost and HubPath locate the hub repository: its host (the
	// fingerprint's) and its path on that host ("" when unknown). The check
	// hub-hidden asks each provider on that host whether its writer can write
	// to the hub.
	HubHost, HubPath string
}

// Doctor checks the write identity of every provider and every target it can
// write to, as `touchmark doctor` reports them: a matrix of target × check.
// It writes nothing.
//
// The providers are connected and the targets resolved as in phases A and B
// of Plan (throttle, retries, exclude, --only, deduplication), with the
// write identity; a public hub (or one whose visibility is unknown in CI)
// with security.private_targets_in_public_hub other than deliver names no
// non-public target: those are only counted, and the details of their checks
// are dropped.
//
// Per provider (DoctorProvider.Checks):
//   - writer: the credential acts as the writer hub.yml names (fail
//     otherwise; distribute exits 2 on it);
//   - the identity's own checks of platform.Checker (Check with no
//     repository: token-expiry, scopes, 2fa, …); unknown without one;
//   - signing: whether pushes that must be signed will be (providers[].sign,
//     the signing key, the platform's API commits, and whether rules that
//     require signatures are readable before a push);
//   - signing-key: with a key and a platform.KeyChecker, whether the key is
//     the writer's on the platform;
//   - hub-hidden: for the provider on the hub's host, that its writer may
//     not write to the hub: ok when the hub is not visible to it or it
//     cannot push there, fail when it can, warn when a private hub is
//     visible to it (it is a member). Under security.writer_on_hub guard a
//     writer that sees the hub gets hub-guard instead (hubGuardCheck).
//
// Per target: skipped with the reason of delivery (archived, disabled,
// empty, mirror, pending-deletion, prs-disabled, sha256), not-opted-in
// (no opt-in file, and targets.yml does not subscribe the target),
// opted-out (the opt-in file says enabled: false) or unsafe-opt-in (the
// opt-in file read through the API), or
// deferred:<reason> with an unknown check when the provider's circuit is
// open or the run ended (notChecked); else the checks
// of platform.Checker for the repository and the sync branches (access,
// permissions, workflows, rules, …), signing where rules decide it
// (platform.Preflighter, a provider without a key or API commits), and
// markers: the pull requests of the sync branches (Reader.PRs with the
// writer and known_authors) whose marker has the hub's id and another
// fingerprint (another hub with the same id, or a move without
// previous_fingerprints), and open ones whose marker is the hub's but whose
// author is neither the writer nor a known author. A failed read is that
// check's unknown.
func Doctor(ctx context.Context, dd DoctorDeps) (*report.Doctor, error) {
	r, err := newRun(dd.Deps, modeDoctor)
	if err != nil {
		return nil, fmt.Errorf("doctor: %w", err)
	}
	kept, err := r.resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("doctor: %w", err)
	}
	doc := report.NewDoctor(dd.Engine)
	doc.Strict = dd.Strict
	doc.Hub = r.rep.Hub
	doc.Providers = make([]report.DoctorProvider, len(r.provs))
	for i, p := range r.provs {
		dp := &doc.Providers[i]
		*dp = report.DoctorProvider{
			ID: p.cfg.ID, Type: p.cfg.Type, Host: p.cfg.Host, Writer: p.cfg.Writer, Self: p.self.Login,
			ResolveComplete: p.info.ResolveComplete, Missing: slices.Clone(p.info.Missing), Error: p.info.Error,
		}
		if dp.Error != "" {
			continue
		}
		dp.Checks = r.providerChecks(throttle.Default(ctx, p.throttle()), p, dd)
	}
	results := r.doctorTargets(ctx, kept)
	for i, t := range kept {
		dt := results[i]
		if t.hidden {
			dt.RepoID, dt.Path = "", ""
			for j := range dt.Checks {
				dt.Checks[j].Detail = ""
			}
		}
		doc.Targets = append(doc.Targets, dt)
	}
	doc.Warnings = append(doc.Warnings, r.rep.Warnings...)
	doc.Summarize()
	return doc, nil
}

// fromFinding converts a driver's finding.
func fromFinding(f platform.Finding) report.DoctorCheck {
	status := report.CheckStatus(f.Status)
	if !slices.Contains(report.Statuses, status) {
		status = report.StatusUnknown
	}
	return report.DoctorCheck{Name: f.Check, Status: status, Detail: f.Detail}
}

// unknownCheck is check name that could not be made because of err.
func unknownCheck(name, what string, err error) report.DoctorCheck {
	return report.DoctorCheck{Name: name, Status: report.StatusUnknown, Detail: what + ": " + err.Error()}
}

// providerChecks runs the checks of provider p's write identity.
func (r *run) providerChecks(ctx context.Context, p *provider, dd DoctorDeps) []report.DoctorCheck {
	var out []report.DoctorCheck
	out = append(out, r.writerCheck(p))
	checker, isChecker := p.writer.(platform.Checker)
	if isChecker {
		var fs []platform.Finding
		err := r.retry(ctx, p, func() error {
			var err error
			fs, err = checker.Check(ctx, nil, nil)
			return err
		})
		if err != nil {
			out = append(out, unknownCheck("token-expiry", "the identity's checks", err))
		}
		for _, f := range fs {
			out = append(out, fromFinding(f))
		}
	} else {
		out = append(out, report.DoctorCheck{Name: "token-expiry", Status: report.StatusUnknown,
			Detail: "the " + p.cfg.Type + " driver has no checks of its write identity"})
	}
	out = append(out, r.signingCheck(p))
	if c, ok := r.signingKeyCheck(ctx, p); ok {
		out = append(out, c)
	}
	out = append(out, r.hubHiddenCheck(ctx, p, dd))
	return out
}

// writerCheck compares the account the write credential acts as with the
// writer hub.yml names.
func (r *run) writerCheck(p *provider) report.DoctorCheck {
	c := report.DoctorCheck{Name: "writer"}
	w := p.cfg.Writer
	switch {
	case w == "":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("hub.yml names no writer for provider %s: distribute refuses to run", p.cfg.ID)
	case p.self.ID == "" && p.self.Login == "":
		c.Status, c.Detail = report.StatusUnknown, "the account the write credential acts as could not be read (see the warnings)"
	case sameAccount(p.self, w, p.writerAcct, p.writerAcct.ID != ""):
		c.Status, c.Detail = report.StatusOK, fmt.Sprintf("the write credential acts as %s, the writer hub.yml names", p.self.Login)
	default:
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("the write credential acts as %s, and hub.yml names %s: distribute refuses to run", p.self.Login, w)
	}
	return c
}

// signingCheck tells whether the pushes of provider p that must be signed
// will be.
func (r *run) signingCheck(p *provider) report.DoctorCheck {
	c := report.DoctorCheck{Name: "signing"}
	keyVar := p.cfg.EnvPrefix + "SIGNING_KEY"
	api := p.caps.Commit.API && p.caps.Commit.SignedByPlatform
	_, preflights := p.writer.(platform.Preflighter)
	switch {
	case p.signer != nil:
		c.Status, c.Detail = report.StatusOK, "touchmark signs its commits with the SSH key of "+keyVar
	case api:
		c.Status, c.Detail = report.StatusOK, "the platform signs touchmark's API commits where a signature is needed"
	case p.cfg.Sign == "always":
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("sign: always, but provider %s has no signing key (%s) and the platform makes no signed commits through its API: every push is blocked:cannot-sign", p.cfg.ID, keyVar)
	case preflights:
		c.Status, c.Detail = report.StatusOK, fmt.Sprintf("no signing key (%s): the targets whose rules require signed commits are checked one by one", keyVar)
	default:
		c.Status, c.Detail = report.StatusUnknown, fmt.Sprintf("no signing key (%s), and the writer cannot read upfront whether a target requires signed commits "+
			"(GitLab push rules, Gitea and Forgejo branch protections): such a target ends blocked:cannot-sign at its first push", keyVar)
	}
	return c
}

// signingKeyCheck asks the platform whether provider p's signing key is its
// writer's; ok is false when p has no key.
func (r *run) signingKeyCheck(ctx context.Context, p *provider) (report.DoctorCheck, bool) {
	if p.signer == nil {
		return report.DoctorCheck{}, false
	}
	kc, ok := p.writer.(platform.KeyChecker)
	if !ok {
		return report.DoctorCheck{Name: "signing-key", Status: report.StatusUnknown,
			Detail: "the " + p.cfg.Type + " driver cannot tell whether the signing key is the writer's on the platform"}, true
	}
	var f platform.Finding
	err := r.retry(ctx, p, func() error {
		var err error
		f, err = kc.CheckSigningKey(ctx, p.signer.PublicKey())
		return err
	})
	if err != nil {
		return unknownCheck("signing-key", "read the writer's keys", err), true
	}
	c := fromFinding(f)
	c.Name = "signing-key"
	return c, true
}

// hubHiddenCheck checks that provider p's writer cannot write to the hub
// (a leaked write key must not change the packs).
func (r *run) hubHiddenCheck(ctx context.Context, p *provider, dd DoctorDeps) report.DoctorCheck {
	c := report.DoctorCheck{Name: "hub-hidden"}
	host := strings.ToLower(dd.HubHost)
	switch {
	case host == "":
		c.Status, c.Detail = report.StatusUnknown, "the hub's host is unknown"
		return c
	case host != strings.ToLower(p.cfg.Host):
		c.Status, c.Detail = report.StatusOK, "the hub is on "+host+", not on this provider's host"
		return c
	case dd.HubPath == "":
		c.Status, c.Detail = report.StatusUnknown, "the hub's path is unknown (CI tells it; locally the origin remote)"
		return c
	}
	var hub platform.Repo
	err := r.retry(ctx, p, func() error {
		var err error
		hub, err = p.writer.Repo(ctx, dd.HubPath)
		return err
	})
	switch {
	case isNotFound(err):
		c.Status, c.Detail = report.StatusOK, "the writer does not see the hub"
		return c
	case err != nil:
		return unknownCheck("hub-hidden", "look up the hub as the writer", err)
	case r.hub != nil && r.hub.Security.WriterOnHub == "guard":
		return r.hubGuardCheck(ctx, p, hub)
	}
	checker, ok := p.writer.(platform.Checker)
	if !ok {
		c.Status, c.Detail = report.StatusUnknown, "the writer sees the hub, and the "+p.cfg.Type+" driver cannot tell whether it may push there"
		return c
	}
	var fs []platform.Finding
	err = r.retry(ctx, p, func() error {
		var err error
		fs, err = checker.Check(ctx, []platform.Repo{hub}, nil)
		return err
	})
	if err != nil {
		return unknownCheck("hub-hidden", "check the writer's access to the hub", err)
	}
	access := platform.FindingUnknown
	for _, f := range fs {
		if f.Check == "access" {
			access = f.Status
		}
	}
	switch {
	case access == platform.FindingOK:
		c.Status, c.Detail = report.StatusFail, "the writer may push to the hub: a leaked write key could change the packs; "+
			"take the writer off the hub"
	case access == platform.FindingUnknown:
		c.Status, c.Detail = report.StatusUnknown, "the writer sees the hub, and whether it may push there is not readable"
	case hub.Visibility == "private":
		c.Status, c.Detail = report.StatusWarn, "the writer cannot push to the hub, but sees the private hub: it is a member; "+
			"the writer should have no access to the hub at all"
	default:
		c.Status, c.Detail = report.StatusOK, "the writer sees the "+hub.Visibility+" hub but cannot push to it"
	}
	return c
}

// hubGuardCheck is check hub-guard, under security.writer_on_hub guard for
// a writer that sees the hub: the conditions of platform.HubGuard as one
// check, its status the worst of theirs, its detail the conditions that
// are not ok (all of them when every one is). A driver without HubGuard
// gives unknown: only GitLab verifies the guard.
func (r *run) hubGuardCheck(ctx context.Context, p *provider, hub platform.Repo) report.DoctorCheck {
	c := report.DoctorCheck{Name: "hub-guard"}
	guard, ok := p.writer.(platform.HubGuard)
	if !ok {
		c.Status, c.Detail = report.StatusUnknown, "the writer sees the hub, and security.writer_on_hub guard is verified on GitLab only, not by the "+
			p.cfg.Type+" driver: keep the writer off the hub"
		return c
	}
	var fs []platform.Finding
	err := r.retry(ctx, p, func() error {
		var err error
		fs, err = guard.GuardHub(ctx, hub)
		return err
	})
	if err != nil {
		return unknownCheck("hub-guard", "check the hub's guard as the writer", err)
	}
	if len(fs) == 0 {
		c.Status, c.Detail = report.StatusUnknown, "the driver reported no condition of the guard"
		return c
	}
	rank := map[report.CheckStatus]int{report.StatusOK: 0, report.StatusUnknown: 1, report.StatusWarn: 2, report.StatusFail: 3}
	c.Status = report.StatusOK
	var bad, all []string
	for _, f := range fs {
		s := fromFinding(f).Status
		if rank[s] > rank[c.Status] {
			c.Status = s
		}
		all = append(all, f.Detail)
		if s != report.StatusOK {
			bad = append(bad, string(s)+": "+f.Detail)
		}
	}
	if len(bad) > 0 {
		c.Detail = strings.Join(bad, "; ")
	} else {
		c.Detail = "the writer reaches the hub but cannot get content onto its default branch: " + strings.Join(all, "; ")
	}
	return c
}

// doctorTargets checks every target of list, in parallel with the pools
// of inspectAll (eachByProvider), and returns their report entries in the
// order of list.
func (r *run) doctorTargets(ctx context.Context, list []*target) []report.DoctorTarget {
	out := make([]report.DoctorTarget, len(list))
	r.eachByProvider(list, func(i int) { out[i] = r.doctorTarget(ctx, list[i]) })
	return out
}

// doctorReads are the reads of one target's checks. Their failures feed the
// provider's circuit as the reads of phase C do: once the credential is
// refused for three targets in a row, or a ban answers everything, the
// provider's remaining targets are deferred without a call; a target whose
// reads all went through ends the streaks (doctorTarget).
type doctorReads struct {
	r      *run
	p      *provider
	failed bool
}

// read is retry for one read.
func (d *doctorReads) read(ctx context.Context, call func() error) error {
	err := d.r.retry(ctx, d.p, call)
	if err != nil && !isNotFound(err) {
		d.failed = true
		if _, refused := throttle.Refused(err); ctx.Err() == nil && !refused {
			d.p.failed(err)
		}
	}
	return err
}

// doctorTarget checks one target.
func (r *run) doctorTarget(ctx context.Context, t *target) (dt report.DoctorTarget) {
	p := t.prov
	ctx = throttle.Default(ctx, p.throttle())
	dt = report.DoctorTarget{Provider: p.cfg.ID, Host: t.host, RepoID: t.repo.ID, Path: t.repo.Path, Checks: []report.DoctorCheck{}}
	defer func() {
		if v := recover(); v != nil {
			dt.Checks = append(dt.Checks, report.DoctorCheck{Name: "access", Status: report.StatusUnknown, Detail: fmt.Sprintf("internal error: %v", v)})
		}
	}()
	if reason := skipReason(t.repo); reason != "" {
		dt.Skipped = reason
		return dt
	}
	if reason, why := p.deferral(); reason != "" {
		return notChecked(dt, reason, why)
	}
	if err := ctx.Err(); err != nil {
		return notChecked(dt, "interrupted", "the run was interrupted")
	}
	rd := &doctorReads{r: r, p: p}
	var file platform.File
	err := rd.read(ctx, func() error {
		var err error
		file, err = p.reader.ReadFile(ctx, t.repo, "", r.optIn, maxOptIn)
		return err
	})
	switch {
	case err == nil:
		// A file that does not parse is delivery's business
		// (blocked:opt-in-invalid); the writer's access is checked anyway.
		if o, _, perr := config.ParseOptIn(file.Content); perr == nil && o.Disabled() {
			dt.Skipped = reasonOptedOut
			return dt
		}
	case isNotFound(err) && ctx.Err() == nil && t.assumed:
		// targets.yml subscribes it: delivery writes to it without the file.
	case isNotFound(err) && ctx.Err() == nil:
		dt.Skipped = reasonNotOptedIn
		return dt
	case errors.Is(err, platform.ErrNotRegular) || errors.Is(err, platform.ErrTooLarge):
		dt.Skipped = "unsafe-opt-in"
		return dt
	default:
		if refusal, refused := throttle.Refused(err); refused {
			reason, why := p.deferral()
			return notChecked(dt, cmp.Or(reason, refusal.Reason), cmp.Or(why, refusal.Error()))
		}
		dt.Checks = append(dt.Checks, unknownCheck("opt-in", "read "+r.optIn, err))
	}
	dt.Checks = append(dt.Checks, r.targetChecks(ctx, t, rd)...)
	dt.Checks = append(dt.Checks, r.markerCheck(ctx, t, rd))
	if !rd.failed {
		p.succeeded()
	}
	return dt
}

// notChecked ends dt, a target the run could not check (the provider took
// no more calls, or the run ended), as skipped deferred:<reason>, with an
// unknown check that says why: doctor --strict does not pass a fleet it
// could not check (unreadable checks are unknown).
func notChecked(dt report.DoctorTarget, reason, why string) report.DoctorTarget {
	dt.Skipped = "deferred:" + reason
	dt.Checks = append(dt.Checks, report.DoctorCheck{Name: "access", Status: report.StatusUnknown,
		Detail: "not checked (" + dt.Skipped + "): " + why})
	return dt
}

// targetChecks runs the driver's checks of t's repository and the sync
// branches, and the signing check where rules decide it.
func (r *run) targetChecks(ctx context.Context, t *target, rd *doctorReads) []report.DoctorCheck {
	p := t.prov
	var out []report.DoctorCheck
	checker, ok := p.writer.(platform.Checker)
	if !ok {
		out = append(out, report.DoctorCheck{Name: "access", Status: report.StatusUnknown, Detail: "the " + p.cfg.Type + " driver has no checks of its write identity"})
	} else {
		var fs []platform.Finding
		err := rd.read(ctx, func() error {
			var err error
			fs, err = checker.Check(ctx, []platform.Repo{t.repo}, r.branches)
			return err
		})
		if err != nil {
			out = append(out, unknownCheck("access", "check the writer's access", err))
		}
		for _, f := range fs {
			if f.Repo == "" || strings.EqualFold(f.Repo, t.repo.Path) {
				out = append(out, fromFinding(f))
			}
		}
	}
	if c, ok := r.targetSigning(ctx, t, rd); ok {
		out = append(out, c)
	}
	return out
}

// targetSigning checks, for a provider that cannot sign otherwise (no key,
// no signed API commits) and whose writer reads rules upfront, whether a
// rule of t requires signed commits; ok is false where the provider's
// signing check says it all.
func (r *run) targetSigning(ctx context.Context, t *target, rd *doctorReads) (report.DoctorCheck, bool) {
	p := t.prov
	pf, preflights := p.writer.(platform.Preflighter)
	if p.signer != nil || (p.caps.Commit.API && p.caps.Commit.SignedByPlatform) || p.cfg.Sign == "always" || !preflights {
		return report.DoctorCheck{}, false
	}
	var rules platform.Rules
	err := rd.read(ctx, func() error {
		var err error
		rules, err = pf.Preflight(ctx, t.repo, uniqueNonEmpty(append([]string{t.repo.DefaultBranch}, r.branches...)))
		return err
	})
	c := report.DoctorCheck{Name: "signing"}
	switch {
	case err != nil:
		return unknownCheck("signing", "read the branch rules", err), true
	case !rules.Known:
		c.Status, c.Detail = report.StatusUnknown, "the branch rules are not readable"
	case rules.SignedCommits:
		c.Status, c.Detail = report.StatusFail, fmt.Sprintf("a rule requires signed commits, and provider %s cannot sign: pushes end blocked:cannot-sign; set %sSIGNING_KEY", p.cfg.ID, p.cfg.EnvPrefix)
	default:
		c.Status, c.Detail = report.StatusOK, "no rule requires signed commits"
	}
	return c, true
}

// maxMarkerNotes bounds the pull requests one markers check names.
const maxMarkerNotes = 3

// markerCheck looks at the pull requests of t's sync branches for markers
// with the hub's id and a foreign fingerprint, and for open ones with the
// hub's marker by an author who is neither the writer nor a known author.
func (r *run) markerCheck(ctx context.Context, t *target, rd *doctorReads) report.DoctorCheck {
	p := t.prov
	var prs []platform.PR
	err := rd.read(ctx, func() error {
		var err error
		prs, err = p.reader.PRs(ctx, t.repo, r.branches, p.authors)
		return err
	})
	if err != nil {
		return unknownCheck("markers", "list the pull requests of the sync branches", err)
	}
	sign := "#"
	if p.cfg.Type == "gitlab" {
		sign = "!"
	}
	var foreign, unknown []string
	for _, pr := range prs {
		if !slices.Contains(r.branches, pr.Head) {
			continue
		}
		for _, fp := range markerFingerprints(pr.Body, r.hub.ID) {
			if !slices.Contains(r.fps, fp) {
				foreign = append(foreign, fmt.Sprintf("%s%d (%s)", sign, pr.Number, fp))
				break
			}
		}
		if pr.State != platform.Open || slices.Contains(p.ids, pr.Author.ID) {
			continue
		}
		if _, status := marker.Find(pr.Body, r.fps); status == marker.Found {
			from := ""
			if pr.HeadRepoID != "" && pr.HeadRepoID != pr.RepoID {
				from = ", from a fork"
			}
			unknown = append(unknown, fmt.Sprintf("%s%d by %s%s", sign, pr.Number, cmp.Or(pr.Author.Login, "an unknown account"), from))
		}
	}
	c := report.DoctorCheck{Name: "markers", Status: report.StatusOK, Detail: "no marker of another hub with this id, and none of this hub by an unknown author"}
	var notes []string
	if len(foreign) > 0 {
		notes = append(notes, fmt.Sprintf("markers with the hub's id %s and another fingerprint: %s (another hub with the same id, "+
			"or this hub moved: list its old fingerprint in previous_fingerprints)", r.hub.ID, listMarkers(foreign)))
	}
	if len(unknown) > 0 {
		notes = append(notes, fmt.Sprintf("open pull requests with this hub's marker by an author who is neither the writer nor in known_authors: %s "+
			"(a former writer belongs in known_authors; a copy is never touchmark's)", listMarkers(unknown)))
	}
	if len(notes) > 0 {
		c.Status, c.Detail = report.StatusWarn, strings.Join(notes, "; ")
	}
	return c
}

// listMarkers joins up to maxMarkerNotes items and says how many more there
// are.
func listMarkers(items []string) string {
	if len(items) <= maxMarkerNotes {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:maxMarkerNotes], ", ") + fmt.Sprintf(" and %d more", len(items)-maxMarkerNotes)
}

// markerFingerprints returns the fingerprints of the valid v1 markers in
// body, in either frame, whose hub id is id, in the order they appear.
func markerFingerprints(body, id string) []string {
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if !marker.IsLine(line) {
			continue
		}
		m, err := marker.Parse(line)
		if err != nil || m.Hub != id || m.Data.FP == "" {
			continue
		}
		if fp := config.CanonicalFingerprint(m.Data.FP); !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	return out
}
