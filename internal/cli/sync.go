package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bedrock-python/touchmark/internal/apply"
	"github.com/bedrock-python/touchmark/internal/config"
	"github.com/bedrock-python/touchmark/internal/decide"
	"github.com/bedrock-python/touchmark/internal/report"
)

// unknownTarget is the unresolved selector reported when targets.yml has
// entries but the target's name is unknown.
const unknownTarget = "entries for this target (unknown: no --repo and no origin remote)"

func runStatus(ctx context.Context, e *env, o *options) error {
	return runSync(ctx, e, o, false)
}

func runApply(ctx context.Context, e *env, o *options) error {
	return runSync(ctx, e, o, true)
}

// syncRun is one status or apply.
type syncRun struct {
	e     *env
	o     *options
	apply bool
	hub   *hub
	tgt   *target
	rep   *report.Sync
}

// runSync implements status and, with write set, apply. A hub that fails
// the checks of `touchmark check` (configs, formerly and requires, packs) is
// refused: its history and packs are not trusted to decide deletes.
func runSync(ctx context.Context, e *env, o *options, write bool) error {
	s := &syncRun{e: e, o: o, apply: write}
	if err := s.load(ctx); err != nil {
		return err
	}
	s.rep = &report.Sync{
		Command:  "status",
		Hub:      s.hub.report(),
		Target:   s.tgt.report(),
		Entries:  []report.Entry{},
		Warnings: nonNil(slices.Concat(s.hub.warnings, s.tgt.warnings)),
	}
	if write {
		s.rep.Command, s.rep.DryRun = "apply", o.dryRun
		if !o.dryRun {
			s.rep.Results = []report.Result{}
		}
	}
	if s.tgt.optIn == nil {
		return s.print()
	}
	if err := s.hub.build(ctx); err != nil {
		return err
	}
	if err := s.hub.readPacks(ctx); err != nil {
		return err
	}
	if msgs := s.hub.checkErrors(); len(msgs) > 0 {
		return configErrorf("the hub fails touchmark check:\n  %s\nrun touchmark check for details", strings.Join(msgs, "\n  "))
	}
	sel, err := s.selectPacks()
	if err != nil {
		return err
	}
	plan, err := s.plan(ctx, sel)
	if err != nil {
		return err
	}
	if write && !o.dryRun {
		if err := s.execute(ctx, plan); err != nil {
			return err
		}
	}
	s.rep.Summary = report.Summarize(s.rep.Entries, s.rep.Results)
	if err := s.print(); err != nil {
		return err
	}
	if s.rep.Summary.Failed > 0 {
		return exitWith(exitFailed)
	}
	return nil
}

// load opens the hub and the target and reads the configs, the opt-in file
// and the target's name.
func (s *syncRun) load(ctx context.Context) error {
	h, err := openHub(ctx, s.e, s.o.hub, s.o.worktree)
	if err != nil {
		return err
	}
	if errs := h.readConfigs(ctx); len(errs) > 0 {
		return configErrorf("%w\nrun touchmark check for details", joinedError(errors.Join(errs...)))
	}
	t, err := openTarget(ctx, s.e, s.o.dir)
	if err != nil {
		return err
	}
	if err := t.readOptIn(h.optInName()); err != nil {
		return err
	}
	if err := t.resolveRef(ctx, s.o.repo, h); err != nil {
		return err
	}
	s.hub, s.tgt = h, t
	return nil
}

// selectPacks resolves the packs of the target, records the selection in
// the report and refuses an incomplete selection for apply.
func (s *syncRun) selectPacks() (config.Selection, error) {
	h, t := s.hub, s.tgt
	var sel config.Selection
	if s.o.packs.set {
		var err error
		sel, err = config.Explicit(h.cfg, s.o.packs.names, h.known())
		if err != nil {
			return sel, configErrorf("--packs: %w", err)
		}
	} else {
		var warns []config.Warning
		var err error
		sel, warns, err = config.Select(h.cfg, h.targets, t.optIn, t.ref, h.known())
		if err != nil {
			return sel, configError(err)
		}
		for _, w := range warns {
			s.rep.Warnings = append(s.rep.Warnings, w.String())
		}
		if !t.refKnown && needsRef(h.targets) {
			sel.Complete = false
			sel.Unresolved = append(sel.Unresolved, unknownTarget)
		}
	}
	s.rep.Selection = &report.Selection{
		Packs:      nonNil(sel.Packs),
		Complete:   sel.Complete,
		Unresolved: nonNil(sel.Unresolved),
		Sources:    sel.Sources,
	}
	if s.rep.Selection.Sources == nil {
		s.rep.Selection.Sources = map[string][]string{}
	}
	if s.apply && !sel.Complete {
		return sel, configErrorf("selection incomplete: %s cannot be evaluated here; pass --packs",
			strings.Join(sel.Unresolved, ", "))
	}
	return sel, nil
}

// needsRef reports whether targets.yml could select packs for, or exclude,
// a target whose name is known.
func needsRef(t *config.Targets) bool {
	if t == nil {
		return false
	}
	if len(t.Exclude) > 0 {
		return true
	}
	return slices.ContainsFunc(t.Targets, func(e config.Entry) bool { return len(e.Packs) > 0 })
}

// plan observes the target and decides every path.
func (s *syncRun) plan(ctx context.Context, sel config.Selection) (decide.Plan, error) {
	in := decide.Input{
		Manifest: s.hub.manifest,
		Selected: sel.Packs,
		Aliases:  s.hub.aliases(),
		Desired:  decide.Layer(s.hub.current, sel.Packs),
		Ignore:   s.tgt.optIn.Ignore,
		// The opt-in file belongs to the target, whatever the history says.
		OptInFile: s.tgt.optInName,
	}
	if s.apply {
		in.Adopt = s.o.adopt
	}
	obs, err := apply.Observe(ctx, apply.Target{Root: s.tgt.root, Git: s.tgt.git}, decide.Paths(in))
	if err != nil {
		return decide.Plan{}, fmt.Errorf("observe the target: %w", err)
	}
	in.Observed = obs
	plan := decide.Decide(in)
	for _, e := range plan.Entries {
		s.rep.Entries = append(s.rep.Entries, report.NewEntry(e))
	}
	return plan, nil
}

// execute applies the plan and records one result per change.
func (s *syncRun) execute(ctx context.Context, plan decide.Plan) error {
	if len(plan.Changes()) == 0 {
		return nil
	}
	blobs, closeBlobs, err := s.hub.blobs(ctx)
	if err != nil {
		return err
	}
	results := apply.Execute(ctx, apply.Target{Root: s.tgt.root, Git: s.tgt.git}, plan, blobs)
	if err := closeBlobs(); err != nil {
		fmt.Fprintf(s.e.stderr, "touchmark apply: %v\n", err)
	}
	for _, r := range results {
		if r.Entry.Action != decide.Keep {
			s.rep.Results = append(s.rep.Results, report.NewResult(r))
		}
	}
	return nil
}

// print writes the report in the chosen format.
func (s *syncRun) print() error {
	if s.o.format == formatJSON {
		return report.WriteJSON(s.e.stdout, s.rep)
	}
	return s.rep.WriteText(s.e.stdout)
}

// nonNil returns s, or an empty slice for nil, so JSON prints [].
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
